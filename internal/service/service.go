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
	// gone.
	StateBroken State = "broken"
	// StateUnknown means the supervisor did not report a usable state.
	StateUnknown State = "unknown"
)

// Status describes the current state of a registered service.
type Status struct {
	// State is the lifecycle state of the instance.
	State State
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

// InstallOptions describes one service installation.
type InstallOptions struct {
	// Scope is the installation scope.
	Scope Scope
	// Executable is the absolute path of the hfdesk binary.
	Executable string
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
// The user scope is the default on Linux and macOS. The Windows service
// control manager registers machine-wide services only, so the system scope
// is the default there.
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
// The account can be named on Linux and macOS. Windows has no account to
// name: the instance runs as the built-in LocalService account, which has
// no name of its own.
func SupportsNamedServiceAccount() bool {
	return supportsNamedServiceAccount()
}

// Manager drives the platform supervisor. The common layer owns the command
// semantics and the validation; the platform implementations own the
// supervisor details.
type Manager interface {
	// Install registers the service with the platform supervisor.
	Install(ctx context.Context, opts InstallOptions) error
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

// New returns the manager of the platform supervisor for the given scope.
// The scope is validated here so an unknown scope never reaches a platform
// implementation.
func New(scope Scope) (Manager, error) {
	switch scope {
	case ScopeUser, ScopeSystem:
	default:
		return nil, fmt.Errorf("service: unknown scope %q (want %q or %q)", scope, ScopeUser, ScopeSystem)
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
		return "", fmt.Errorf("service: unknown scope %q (want %q or %q)", s, ScopeUser, ScopeSystem)
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
		return fmt.Errorf("service: invalid scope %q (want %q or %q)", opts.Scope, ScopeUser, ScopeSystem)
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
		if dir.path == "" || dir.path == "." {
			return fmt.Errorf("service: %s path is required", dir.name)
		}
		if !filepath.IsAbs(dir.path) {
			return fmt.Errorf("service: %s path %q must be an absolute path", dir.name, dir.path)
		}
		if isRootDir(dir.path) {
			return fmt.Errorf("service: %s path %q is the filesystem root and must not be tightened", dir.name, dir.path)
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
