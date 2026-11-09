// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

// Package service provides the platform-neutral service layer of HFDesk.
//
// The package owns everything about "run HFDesk as a background service" that
// is identical on every operating system: the installation scope and the
// lifecycle vocabulary, the validation rules of an installation, the canonical
// argument vector of the registered process and the headless host that runs
// the server.
//
// The supervisor specific parts (a systemd unit, a launchd property list, a
// Windows SCM registration) live in separate files with a build tag and
// implement the Manager interface. The shared layer never branches on the
// operating system: it owns the command semantics and the validation, the
// platform files own the supervisor details and the facts of the platform
// (the default scope, the scopes an installation may have and the account of
// a machine-wide instance).
package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/server"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

const (
	// ServiceName is the supervisor-visible service name/label of a HFDesk
	// instance.
	ServiceName = "hfdesk"

	// DefaultListen is the bind address of a service instance. Service mode
	// binds loopback by default, even though the interactive server defaults
	// to 0.0.0.0: a supervised instance has no user sitting in front of it
	// and must not expose the dashboard to the network by accident.
	DefaultListen = "127.0.0.1:8080"

	// dirMode is the permission mode of a directory owned by a service
	// installation.
	dirMode os.FileMode = 0o700

	// shutdownMargin is the room a supervisor keeps on top of the budget
	// the server publishes for its own shutdown.
	//
	// The margin covers the work that happens around the shutdown of the
	// server: the process still has to leave its event loop, close its log
	// sink and return from the supervisor call.
	shutdownMargin = 5 * time.Second

	// ShutdownBudget is the time a supervisor waits for the graceful
	// shutdown of an instance.
	//
	// The server publishes the budget of its own orderly shutdown
	// (server.GracefulShutdownTimeout): it drains its HTTP server and then
	// closes its job manager, and every phase of it has its own budget. A
	// supervisor waits for that total plus a margin, so it never kills an
	// instance that is still saving its state.
	//
	// Every supervisor side wait and timeout of the service layer is sized
	// after this budget: the wait of the launchd manager, the kill deadline
	// of the launchd job, the stop timeout of the systemd unit and the wait
	// of the Windows service handler.
	ShutdownBudget = server.GracefulShutdownTimeout + shutdownMargin
)

// ErrNotImplemented reports that service mode is not available on the
// current platform yet.
var ErrNotImplemented = errors.New("service mode is not implemented for this platform")

// Scope selects the installation scope of a service instance.
type Scope string

const (
	// ScopeUser is a per-user installation.
	ScopeUser Scope = "user"
	// ScopeSystem is a machine-wide installation.
	ScopeSystem Scope = "system"
)

// State is the lifecycle state of a registered service.
type State string

const (
	// StateNotInstalled means the service is not registered.
	StateNotInstalled State = "not-installed"
	// StateStopped means the service is registered but not running.
	StateStopped State = "stopped"
	// StateStarting means the service is starting.
	StateStarting State = "starting"
	// StateRunning means the service is running.
	StateRunning State = "running"
	// StateStopping means the service is shutting down.
	StateStopping State = "stopping"
	// StateFailed means the service failed.
	StateFailed State = "failed"
	// StateBroken means the service is registered, but its executable is
	// gone. The instance itself can still be running: a supervisor keeps
	// the process it started, and only the next start fails. The running
	// fact is therefore carried by Status.Running and not by this state,
	// which stays the diagnosis of the registration.
	StateBroken State = "broken"
	// StateUnknown means the supervisor did not report a usable state.
	StateUnknown State = "unknown"
)

// Status describes the current state of a registered service.
type Status struct {
	// State is the lifecycle state of the instance.
	State State
	// Running is the supervisor's own fact: the instance runs now. It is
	// read from the same supervisor answer as State, and it is separate
	// from it because the two answer different questions: State classifies
	// the registration, and a registration whose executable is gone is
	// broken (`StateBroken`) even while its process still serves. A
	// consumer that has to know whether anything serves reads this; a
	// manager that does not report the fact leaves State as the only
	// evidence.
	Running bool
	// Scope is the installation scope of the instance.
	Scope Scope
	// Name is the supervisor-visible service name/label.
	Name string
	// UnitPath is the path of the supervisor registration file.
	UnitPath string
	// Executable is the absolute path the registration points at.
	Executable string
	// Listen is the host:port the service binds.
	Listen string
	// Paths are the absolute runtime paths of the instance.
	Paths runtimepaths.Paths
	// CacheDir is the Hugging Face cache root the instance uses: the cache
	// directory of the instance's own config file when it defines one,
	// otherwise the cache directory recorded in the registration.
	CacheDir string
	// Detail is a human-readable detail, e.g. the last error.
	Detail string
}

// runningState reports whether a lifecycle state means the instance runs: it
// serves, or it is on its way up and about to serve. It is the state side of
// Status.Running, which the platform managers fill from the same supervisor
// answer as State, so the two never describe different reads.
//
// The stopping state is not a running one: an instance on its way down is
// treated like a stopped one, so a caller never reports that it serves.
func runningState(state State) bool {
	return state == StateRunning || state == StateStarting
}

// brokenRunningDetail is appended to the detail of a registration that is
// broken while its instance still runs.
//
// The supervisor keeps the process it started, so the instance serves although
// the registration can never start it again. The sentence is written once and
// used by every platform manager, so the three cannot describe the same state
// differently.
const brokenRunningDetail = " (the instance still runs, but the registration cannot start it again)"

// statusReadHint is the one action of a failure report whose outcome the
// report itself cannot settle: reading the state of the service decides
// what the failure left behind. The composite rollback outcomes of the
// service layer append it, so the command line — which prints the manager
// text as it is — and the desktop surface both carry result, cause and one
// action.
const statusReadHint = `run "hfdesk service status" to read the current state of the service`

// namedActionMarkers are the command and instruction shapes the refusal and
// reason texts of the service layer print as the one action of a report:
// quoted commands of the hfdesk/supervisor families, the Windows repair
// commands, the account-creation and directory-grant instructions, and the
// elevation reasons of the three platforms. The list is the single rule
// every surface consults before it appends an action of its own, so a
// composite report states exactly one route whichever layer reads it — a
// marker also matches the diagnostic hint itself, which keeps a cause that
// carries it from being served a second one.
//
// The list recognizes the shapes the texts print TODAY: every command a
// message names is printed QUOTED (the refusal family renders commands with
// %q), and the prose instructions are listed by their fixed wording. A
// future message that prints an action unquoted must add its shape here —
// the owner test pins the list.
var namedActionMarkers = []string{
	`"hfdesk service `,
	`"systemctl `,
	"sc config ",
	`"chown `,
	"create it with ",
	"run the command as ",
	"run the command from ",
	"install again",
}

// HasNamedAction reports whether text already carries a named next action
// from that list.
func HasNamedAction(text string) bool {
	for _, marker := range namedActionMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// AppendActionIfUnnamed returns the report with the diagnostic action
// appended — unless the report already names one (the one-route rule of
// namedActionMarkers).
func AppendActionIfUnnamed(report, action string) string {
	if HasNamedAction(report) {
		return report
	}
	return report + "; " + action
}

// AppendActionErrorIfUnnamed is the error form of AppendActionIfUnnamed:
// the wrapping stays %w-based, so classification of the cause survives.
func AppendActionErrorIfUnnamed(report error, action string) error {
	if HasNamedAction(report.Error()) {
		return report
	}
	return fmt.Errorf("%w; %s", report, action)
}

// InstallFieldSet marks the fields of an InstallOptions that a request set
// explicitly.
//
// A reinstall keeps every setting the request did not name, so an unmarked
// field only applies when the existing registration has no value of its own.
// The executable is never part of the set: it always comes from the request.
type InstallFieldSet struct {
	// Listen marks a bind address the request named.
	Listen bool
	// Config marks a config file the request named.
	Config bool
	// StateDir marks a state directory the request named.
	StateDir bool
	// LogDir marks a log directory the request named.
	LogDir bool
	// CacheDir marks a cache root the request named.
	CacheDir bool
	// HubDir marks a hub directory the request named, or the hub directory
	// a named cache root resolved to (<cache>/hub): that directory is the
	// value the request asked for, not a default of the scope.
	HubDir bool
	// LocalDir marks an output directory the request named.
	LocalDir bool
	// ServiceUser marks a service account the request named.
	ServiceUser bool
}

// InstallOptions describes one service installation.
type InstallOptions struct {
	// Scope is the installation scope.
	Scope Scope
	// Executable is the absolute path of the hfdesk binary.
	Executable string
	// Explicit marks the fields the request named itself. See
	// InstallFieldSet: an unmarked field never overrides the value of an
	// existing registration.
	Explicit InstallFieldSet
	// Listen is the host:port to bind.
	Listen string
	// Paths are the absolute config/state/log paths of the instance.
	Paths runtimepaths.Paths
	// CacheDir is the absolute HF cache root. It may be empty.
	CacheDir string
	// HubDir is the absolute HF hub dir. It may be empty.
	HubDir string
	// LocalDir is the absolute flat output dir. It may be empty.
	LocalDir string
	// ServiceUser is the account a machine-wide instance runs as.
	//
	// It applies to the machine-wide scope only: a user scope instance
	// runs as the user itself and never names a service account. An
	// empty value means the default account of the platform
	// (DefaultServiceUser), and a platform without a named account
	// (Windows uses the built-in LocalService) keeps its own identity.
	//
	// The account is never created by an installation: it is the identity
	// the supervisor starts the instance with, and the installation
	// refuses to register an instance whose account does not exist.
	ServiceUser string
	// ExpectedVersion is the version the started instance must report; empty
	// and "dev" leave readiness as it was.
	ExpectedVersion string
	// Created and VersionVerified are results of Manager.Install, not inputs:
	// Created reports that THIS installation made the registration (its first
	// one) — a caller may remove only a registration the installation created
	// itself — and an update over an existing registration reports false on
	// every platform.
	Created         bool
	VersionVerified bool
}

// DefaultInstallOptions returns the options of an installation that names
// nothing: the default listen address, paths and cache layout of the scope.
//
// No field is marked explicit, so a caller without a command line (the desktop
// host) asks for "the defaults, nothing named" and never resets an existing
// registration. A scope the platform lacks yields empty paths, which
// ValidateInstallOptions rejects before anything is written.
func DefaultInstallOptions(scope Scope, executable string) InstallOptions {
	opts := InstallOptions{
		Scope:      scope,
		Executable: executable,
		Listen:     DefaultListen,
	}
	paths, err := DefaultPaths(scope)
	if err != nil {
		return opts
	}
	opts.Paths = paths
	opts.CacheDir, opts.HubDir = DefaultCacheDirs(scope, paths)
	return opts
}

// mergeInstallOptions returns the effective options of an installation over
// an existing registration. existingPresent distinguishes the absence of an
// optional value in a real registration from the zero value of a first install.
//
// A named field of the request wins. An unmarked required field keeps the
// registration value when it has one and the request default otherwise;
// optional destinations keep the registration's absence as well as its value.
// A first installation has no registration to merge, so it uses the request.
// The executable and scope always come from the request: another scope is a
// different registration and is never merged.
func mergeInstallOptions(requested, existing InstallOptions, existingPresent bool) InstallOptions {
	merged := requested
	if !requested.Explicit.Listen && existing.Listen != "" {
		merged.Listen = existing.Listen
	}
	if !requested.Explicit.Config && existing.Paths.ConfigFile != "" {
		merged.Paths.ConfigFile = existing.Paths.ConfigFile
	}
	if !requested.Explicit.StateDir && existing.Paths.StateDir != "" {
		merged.Paths.StateDir = existing.Paths.StateDir
	}
	if !requested.Explicit.LogDir && existing.Paths.LogDir != "" {
		merged.Paths.LogDir = existing.Paths.LogDir
	}
	if existingPresent && !requested.Explicit.CacheDir {
		// An absent optional destination is meaningful: the host then
		// follows its config/environment/default precedence. Do not replace
		// that absence with a default carried by the reinstall request.
		merged.CacheDir = existing.CacheDir
	}
	if existingPresent && !requested.Explicit.HubDir {
		merged.HubDir = existing.HubDir
	}
	if existingPresent && !requested.Explicit.LocalDir {
		merged.LocalDir = existing.LocalDir
	}
	if !requested.Explicit.ServiceUser && existing.ServiceUser != "" {
		merged.ServiceUser = existing.ServiceUser
	}
	return merged
}

// DefaultServiceUser returns the default account a machine-wide instance
// runs as on this platform.
//
// The account is a dedicated, stable and unprivileged service account: the
// files it owns have to stay writable across reboots, and a machine-wide
// instance needs no privilege of root. The name is only a default, the
// caller may name another account.
//
// A platform without a named service account returns an empty string:
// Windows runs the instance as the built-in LocalService account, which has
// no name of its own.
func DefaultServiceUser() string {
	return defaultServiceUser()
}

// DefaultScope returns the scope of an installation that does not ask for
// one.
//
// The user scope is the default on Linux and macOS; on Windows the system
// scope is (the platform rule: SupportsUserScope).
//
// The default scope is a fact of the platform: the shared layer never
// compares the operating system itself.
func DefaultScope() Scope {
	return defaultScope()
}

// SupportsUserScope reports whether a per-user installation exists on this
// platform.
//
// Every platform but Windows offers the per-user scope: the service control
// manager of Windows registers machine-wide services only, so a per-user
// instance does not exist there.
//
// A platform without a supervisor does not restrict the scope either: it
// reports that service mode is unavailable, which is a more useful answer
// than a refusal of the scope.
func SupportsUserScope() bool {
	return supportsUserScope()
}

// SupportsNamedServiceAccount reports whether the account of a machine-wide
// instance can be named on this platform.
//
// The account can be named on Linux and macOS; Windows has no account to
// name — its platform default is stated with DefaultServiceUser.
func SupportsNamedServiceAccount() bool {
	return supportsNamedServiceAccount()
}

// Manager drives the platform supervisor. The common layer owns the command
// semantics and the validation; the platform implementations own the
// supervisor details.
type Manager interface {
	// Install registers the service with the platform supervisor and
	// returns the installation as it was written: a reinstall reports what
	// the instance runs with, a wholesale replacement the request, and Created
	// whether this call wrote the registration. On error the returned options
	// describe no effective installation and must not be reported as one.
	Install(ctx context.Context, opts InstallOptions) (InstallOptions, error)
	// Uninstall removes the registration of the service.
	Uninstall(ctx context.Context) error
	// Start starts the registered service.
	Start(ctx context.Context) error
	// Stop stops the registered service.
	Stop(ctx context.Context) error
	// Restart stops and starts the registered service.
	Restart(ctx context.Context) error
	// Status reports the state of the registered service.
	Status(ctx context.Context) (Status, error)
}

// InstallStartResult is the registration and settled state of a new
// installation that the desktop helper starts as part of its install action.
type InstallStartResult struct {
	Installation InstallOptions
	Status       Status
}

// InstallStartError reports a first-install start failure and what its
// ownership-scoped cleanup established.
type InstallStartError struct {
	Cause               error
	RegistrationRemoved bool
	RegistrationOwned   bool
	ProcessStopped      bool
	CleanupError        error
}

func (e *InstallStartError) Error() string {
	if e == nil {
		return "service: first-install start failed"
	}
	message := "service: first-install start failed"
	if e.Cause != nil {
		message = e.Cause.Error()
	}
	switch {
	case e.RegistrationRemoved:
		return message + " (the failed instance was settled and its registration was removed)"
	case e.CleanupError != nil:
		if e.ProcessStopped && !e.RegistrationOwned {
			return fmt.Sprintf("%s (the failed process was stopped, but the registration changed and was kept: %v)", message, e.CleanupError)
		}
		if e.RegistrationOwned && e.ProcessStopped {
			return fmt.Sprintf("%s (the failed process was proven stopped, but its registration could not be removed: %v)", message, e.CleanupError)
		}
		if e.RegistrationOwned {
			return fmt.Sprintf("%s (the registration was kept because its process was not proven stopped: %v)", message, e.CleanupError)
		}
		return fmt.Sprintf("%s (the registration was kept because ownership could not be proven: %v)", message, e.CleanupError)
	default:
		return message + " (the registration was kept because ownership and process state were not proven)"
	}
}

func (e *InstallStartError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ErrAlreadyRegistered classifies a refusal of the first-install path: the
// action found a registration it did not create, at its read or between the
// read and its write, so it changes nothing. It carries no text of its own so
// that %w adds nothing to the refusal message shown to the user verbatim;
// callers classify with errors.Is, never by wording.
var ErrAlreadyRegistered = errors.New("")

// ErrSingletonAccessDenied classifies a refusal to hold the per-session
// desktop singleton: the object exists, but the Windows identity of this
// process has no access to it, because another identity created it. It
// carries no text of its own; classify with errors.Is (the verbatim-message
// rule is stated with ErrAlreadyRegistered).
var ErrSingletonAccessDenied = errors.New("")

// ErrServiceAccountMismatch classifies the refusal of a Windows update over
// a registration whose account is not the supported identity: an
// administrator changed it away from the built-in LocalService account, and
// the update refuses instead of silently cancelling that decision. It carries
// no text of its own; classify with errors.Is (see ErrAlreadyRegistered).
var ErrServiceAccountMismatch = errors.New("")

// HoldDesktopSingleton holds the per-session desktop singleton object across
// one service mutation.
//
// The object is the single-instance lock of the Windows desktop host: its
// existence tells a launch of the desktop app of the same user session that
// an HFDesk instance of that session already runs or is being changed. A
// mutation that starts or restarts the service therefore holds it from before
// the first change through the whole mutation, including a rollback and the
// restart of a running instance, and releases it before the final report. A
// launch that finds the object reports the running instance instead of
// binding the address the mutation needs.
//
// name names the object when the caller received it (the service gate passes
// the lock of its own launch). An empty name computes the object of the
// calling session itself, so a caller without a name — the helper of a
// hand-typed command line, or the service command — still holds the very
// object a relaunch in this session would find; one computation owns the name
// shape, so the desktop host, the helper and the command cannot drift apart.
//
// A hold that cannot be established returns an error and no release, and the
// caller refuses the mutation before it changes anything: the hold fails
// closed. The platform owns the object: Windows has it, and a platform
// without the desktop host has no such object to hold, so the release of a
// hold there ends nothing.
func HoldDesktopSingleton(name string) (release func(), err error) {
	return holdDesktopSingleton(name)
}

// ProbeDesktopSingleton reports whether the per-session desktop singleton
// object already existed when this process first touched it, through the
// probe of the platform, and returns a release that keeps the object alive
// until the caller ends it.
//
// A service mutation probes before it holds: together with the published
// state (LiveServingDesktop), the result tells whether a desktop instance
// of this session is alive. A platform without the desktop host has no
// object (the ownership rule: HoldDesktopSingleton), so the probe reports
// that the object did not pre-exist.
func ProbeDesktopSingleton(name string) (release func(), preExisting bool, err error) {
	return probeDesktopSingleton(name)
}

type installStarter interface {
	InstallAndStart(context.Context, InstallOptions) (InstallStartResult, error)
}

// InstallAndStart asks a platform manager that supports an ownership-preserving
// first install to create, start and verify the new registration as one action.
// The ordinary Install method keeps its existing stopped-first-install
// semantics for command-line callers.
func InstallAndStart(ctx context.Context, manager Manager, opts InstallOptions) (InstallStartResult, error) {
	starter, ok := manager.(installStarter)
	if !ok {
		return InstallStartResult{}, errors.New("service: this platform manager does not support an ownership-preserving first install")
	}
	return starter.InstallAndStart(ctx, opts)
}

// New returns the manager of the platform supervisor for the given scope.
// The scope is validated here so an unknown scope never reaches a platform
// implementation.
func New(scope Scope) (Manager, error) {
	switch scope {
	case ScopeUser, ScopeSystem:
	default:
		return nil, fmt.Errorf("service: unknown scope %q (want one of %q, %q)", scope, ScopeUser, ScopeSystem)
	}
	return newPlatformManager(scope)
}

// ParseScope converts a user supplied scope name into a Scope. The comparison
// is case-insensitive; every other value is rejected.
func ParseScope(s string) (Scope, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(ScopeUser):
		return ScopeUser, nil
	case string(ScopeSystem):
		return ScopeSystem, nil
	default:
		return "", fmt.Errorf("service: unknown scope %q (want one of %q, %q)", s, ScopeUser, ScopeSystem)
	}
}

// ParseListen splits a listen address into its host and port. A bracketed
// IPv6 literal ("[::1]:8080") is accepted. The host must be non-empty: an
// empty host ("":8080") binds every interface and is rejected here, so a
// service instance never listens on the whole network by accident. The port
// must be in the range 1-65535.
func ParseListen(addr string) (string, int, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", 0, errors.New("service: listen address is required")
	}

	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("service: invalid listen address %q: %w", addr, err)
	}
	if host == "" {
		return "", 0, fmt.Errorf("service: invalid listen address %q: the host is empty, an empty host binds every interface", addr)
	}

	port, err := strconv.Atoi(portText)
	if err != nil {
		return "", 0, fmt.Errorf("service: invalid listen address %q: %q is not a port number", addr, portText)
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("service: invalid listen address %q: port %d is out of range (1-65535)", addr, port)
	}
	return host, port, nil
}

// LocalAddress returns the address a client of the same machine reaches the
// listener of listen with.
//
// A wildcard bind has no address a client can connect to: 0.0.0.0 and :: fail
// with WSAEADDRNOTAVAIL on Windows and EADDRNOTAVAIL elsewhere. Such a bind is
// replaced with the loopback address of the same family, which the listener
// accepts like any other client; an empty host becomes the IPv4 loopback.
// Every other host is kept, including "localhost".
//
// The port is always kept, and the rule has one owner: the readiness probe
// dials the address this function returns and the desktop host opens the URL of
// it, so the address a start is verified on and the address a browser is sent
// to cannot differ. An address without a host and port is an error.
func LocalAddress(listen string) (string, error) {
	addr := strings.TrimSpace(listen)
	if addr == "" {
		addr = DefaultListen
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("service: invalid listen address %q: %w", addr, err)
	}
	if port == "" {
		return "", fmt.Errorf("service: invalid listen address %q: the port is empty", addr)
	}
	if host == "" {
		host = "127.0.0.1"
	} else if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}
	return net.JoinHostPort(host, port), nil
}

// IsLoopbackListen reports whether the address binds a loopback interface.
// It is true for a loopback IP literal (127.0.0.0/8 or ::1) and for the
// literal host name "localhost". Every other address, including the wildcard
// addresses 0.0.0.0 and ::, is not loopback.
func IsLoopbackListen(addr string) bool {
	host, _, err := ParseListen(addr)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}

// isLoopbackHost reports whether the host part of a listen address is
// loopback.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// RunArgs returns the canonical argument vector of the registered service
// process. The vector carries paths and the bind address only: a token, a
// proxy password or a basic auth password is never part of it, because a
// supervisor registration is readable by every user of the machine. The
// credentials of an instance come from its own config file.
//
// An empty Listen yields DefaultListen so the vector is always complete.
func RunArgs(opts InstallOptions) []string {
	args := []string{
		"service",
		"run",
		"--config", opts.Paths.ConfigFile,
		"--state-dir", opts.Paths.StateDir,
		"--log-dir", opts.Paths.LogDir,
		"--listen", listenArg(opts.Listen),
	}
	if opts.CacheDir != "" {
		args = append(args, "--cache-dir", opts.CacheDir)
	}
	if opts.HubDir != "" {
		args = append(args, "--hub-dir", opts.HubDir)
	}
	if opts.LocalDir != "" {
		args = append(args, "--local-dir", opts.LocalDir)
	}
	return args
}

// listenArg normalizes a listen address for the argument vector. An invalid
// address is passed through unchanged: the caller validates it before the
// registration is written.
func listenArg(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return DefaultListen
	}
	if host, port, err := ParseListen(listen); err == nil {
		return net.JoinHostPort(host, strconv.Itoa(port))
	}
	return listen
}

// ValidateInstallOptions rejects an installation that cannot be registered or
// started safely: an unusable executable, a relative runtime path, a relative
// output directory, an invalid listen address or an unknown scope.
func ValidateInstallOptions(opts InstallOptions) error {
	switch opts.Scope {
	case ScopeUser, ScopeSystem:
	default:
		return fmt.Errorf("service: invalid scope %q (want one of %q, %q)", opts.Scope, ScopeUser, ScopeSystem)
	}

	if opts.Executable == "" {
		return errors.New("service: executable is required")
	}
	if !filepath.IsAbs(opts.Executable) {
		return fmt.Errorf("service: executable %q must be an absolute path", opts.Executable)
	}
	info, err := os.Stat(opts.Executable)
	if err != nil {
		return fmt.Errorf("service: executable %q is not usable: %w", opts.Executable, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("service: executable %q is not a regular file", opts.Executable)
	}

	for _, p := range []struct {
		name string
		path string
	}{
		{"config file", opts.Paths.ConfigFile},
		{"state dir", opts.Paths.StateDir},
		{"log dir", opts.Paths.LogDir},
	} {
		if p.path == "" {
			return fmt.Errorf("service: %s path is required", p.name)
		}
		if !filepath.IsAbs(p.path) {
			return fmt.Errorf("service: %s path %q must be an absolute path", p.name, p.path)
		}
	}

	for _, p := range []struct {
		name string
		path string
	}{
		{"cache dir", opts.CacheDir},
		{"hub dir", opts.HubDir},
		{"local dir", opts.LocalDir},
	} {
		if p.path == "" {
			continue
		}
		if !filepath.IsAbs(p.path) {
			return fmt.Errorf("service: %s %q must be an absolute path", p.name, p.path)
		}
	}

	if _, _, err := ParseListen(opts.Listen); err != nil {
		return err
	}
	return nil
}

// PrepareDirs creates the directories an installation owns: the parent
// directory of the config file, the state directory and the log directory.
//
// A directory that is missing is created with the mode the instance owns
// (0700). The mode of a directory that already exists is never changed: a
// user who names an existing directory with --state-dir or --config keeps
// the permissions of that directory, and the installation only makes sure
// that it exists.
//
// Nothing outside those three directories is touched.
// instanceDirRule is the shape rule of a directory an installation owns:
// required, absolute and never the filesystem root — the installation
// would tighten the root otherwise. PrepareDirs applies it where it
// creates; validateInstanceDirs applies the same rule where a machine
// state is about to be read, so a violation carries the identical message
// at either moment.
func instanceDirRule(name, path string) error {
	if path == "" || path == "." {
		return fmt.Errorf("service: %s path is required", name)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("service: %s path %q must be an absolute path", name, path)
	}
	if isRootDir(path) {
		return fmt.Errorf("service: %s path %q is the filesystem root and must not be tightened", name, path)
	}
	return nil
}

// validateInstanceDirs applies the directory rules of an installation
// without touching the filesystem. The darwin installation calls it after
// the request is validated and before it reads the state of the
// supervisor: request validation is local, cheap and gives the clearest
// cause, so it must neither pay for a machine-state read nor be masked by
// one.
func validateInstanceDirs(opts InstallOptions) error {
	for _, dir := range []struct {
		name string
		path string
	}{
		{"config dir", filepath.Dir(opts.Paths.ConfigFile)},
		{"state dir", opts.Paths.StateDir},
		{"log dir", opts.Paths.LogDir},
	} {
		if err := instanceDirRule(dir.name, dir.path); err != nil {
			return err
		}
	}
	return nil
}

func PrepareDirs(opts InstallOptions) error {
	dirs := []struct {
		name string
		path string
	}{
		{"config dir", filepath.Dir(opts.Paths.ConfigFile)},
		{"state dir", opts.Paths.StateDir},
		{"log dir", opts.Paths.LogDir},
	}

	seen := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		if err := instanceDirRule(dir.name, dir.path); err != nil {
			return err
		}
		if seen[dir.path] {
			continue
		}
		seen[dir.path] = true

		if err := ensureDir(dir.path, dirMode); err != nil {
			return fmt.Errorf("service: %s %q: %w", dir.name, dir.path, err)
		}
	}
	return nil
}

// ensureDir creates a directory that is missing and leaves the mode of a
// directory that already exists alone.
//
// The mode of a directory the installation creates itself is the owner-only
// mode of the instance: the state directory holds the job list, the history
// and the mirror targets, and the config directory holds the credentials.
// The mode of a directory that already exists belongs to whoever named it,
// so the installation never changes it.
func ensureDir(path string, mode os.FileMode) error {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("it exists and is not a directory")
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	created, err := createMissingDirs(path, mode)
	if err != nil {
		return err
	}
	// The mode of a create call is filtered by the umask of the process,
	// so the mode of every directory the installation created is set
	// explicitly.
	for _, dir := range created {
		if err := os.Chmod(dir, mode); err != nil {
			return err
		}
	}
	return nil
}

// createMissingDirs creates the directory and the missing parents of it and
// returns the directories it created.
//
// The parents are collected first, so the directories are created from the
// top down and every missing one is known afterwards. A directory that
// appears in the meantime is not created and not touched.
func createMissingDirs(path string, mode os.FileMode) ([]string, error) {
	var missing []string
	current := filepath.Clean(path)
	for {
		if _, err := os.Stat(current); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}

	created := make([]string, 0, len(missing))
	for i := len(missing) - 1; i >= 0; i-- {
		err := os.Mkdir(missing[i], mode)
		if errors.Is(err, fs.ErrExist) {
			// The directory appeared in the meantime: its permissions
			// belong to whoever created it.
			continue
		}
		if err != nil {
			return nil, err
		}
		created = append(created, missing[i])
	}
	return created, nil
}

// isRootDir reports whether the path is the root of its filesystem. Such a
// directory is never owned by an installation: tightening it to 0700 would
// take the whole filesystem away from its users.
func isRootDir(path string) bool {
	clean := filepath.Clean(path)
	return clean == filepath.VolumeName(clean)+string(filepath.Separator)
}

// DefaultPaths returns the default runtime paths of a service instance in the
// given scope: the config file the instance owns, the state directory it
// writes and the directory its log files live in.
//
// The paths are the single source of truth of an installation. The CLI, the
// platform managers and the documentation all derive their paths from them,
// so a user scope instance and a system scope instance can never end up on
// the same state root: a user scope instance keeps its state under the
// per-user configuration directory, a system scope instance under the
// machine-wide directories of the platform.
//
// The scope is rejected with an error when the platform does not support it.
func DefaultPaths(scope Scope) (runtimepaths.Paths, error) {
	return defaultPathsForScope(scope)
}

// DefaultCacheDirs returns the absolute Hugging Face cache root and the
// absolute hub directory of a service instance in the given scope.
//
// A system scope instance uses the cache of its own state root
// (<state dir>/cache): a machine-wide instance must not read or write the
// cache of the user who happens to run the install command. A user scope
// instance uses the Hugging Face cache of the user itself (HF_HOME or
// ~/.cache/huggingface), so it keeps access to the models the user already
// downloaded.
//
// The hub directory is always reported, even when HF_HUB_CACHE is unset: the
// registered process needs an explicit hub path. An unknown scope is treated
// like ScopeSystem, so an instance that is not known to be a user instance
// never reaches the personal cache of the invoking user.
func DefaultCacheDirs(scope Scope, paths runtimepaths.Paths) (string, string) {
	if scope == ScopeUser {
		root := absolutePath(hfdownloader.DefaultCacheDir())
		hub := filepath.Join(root, "hub")
		if env := os.Getenv("HF_HUB_CACHE"); env != "" {
			hub = absolutePath(env)
		}
		return root, hub
	}

	root := absolutePath(filepath.Join(paths.StateDir, "cache"))
	return root, filepath.Join(root, "hub")
}

// EffectiveCacheDir returns the Hugging Face cache root of an instance: the
// cache-dir of the instance's own config file when it defines one,
// otherwise the registered cache directory. A config file that cannot be
// read falls back to the registered value.
//
// The config file of an instance wins over its registration: the user
// changes the cache directory in the settings of the instance, and the
// instance has to use the changed value after the next start.
func EffectiveCacheDir(configFile, registered string) string {
	fileCfg, err := server.LoadConfigFileAt(configFile)
	if err != nil || fileCfg == nil || fileCfg.CacheDir == "" {
		return registered
	}
	return fileCfg.CacheDir
}

// absolutePath returns the absolute form of path. A path that cannot be
// resolved (the working directory is unavailable) is returned unchanged, so
// the caller never receives an empty path.
func absolutePath(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
