// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// The macOS service mode of HFDesk is a launchd registration.
//
// launchd owns the lifecycle of the instance: the registered program stays
// in the foreground, launchd starts it at load time and restarts it after a
// failure. HFDesk never forks, never daemonizes itself, never writes a PID
// file and never runs the program through a shell: the property list holds
// the absolute path of the executable and the canonical argument vector of
// the service host, nothing else.
//
// A user scope instance is a LaunchAgent: it belongs to the user who
// installed it and it lives in the gui domain of that user. A system scope
// instance is a LaunchDaemon: it belongs to the machine and it lives in the
// system domain. The machine-wide instance never reuses the state of a user:
// its home is the machine-level state root of the platform and its paths are
// the machine-level paths.
//
// A machine-wide instance runs as a dedicated service account, not as root:
// the account is a stable, unprivileged identity that owns the three
// directories of the instance (the config directory, the state directory and
// the log directory) and nothing else. The installation never creates the
// account: it validates the account and refuses the installation with the
// commands that create it when the account does not exist.
//
// The machine-wide scope needs an elevated shell: the property list of a
// LaunchDaemon is written into /Library/LaunchDaemons and the job is loaded
// into the system domain, and both belong to the machine. The manager never
// escalates privileges itself: it fails with a clear error when the process
// is not root. The user scope needs no elevation: a LaunchAgent belongs to
// the user who installs it.
//
// The label of the registration is com.hfdesk.hfdesk. It is derived from the
// service name ("hfdesk") and the reverse DNS prefix of the project, and it
// is also the name of the property list file: launchd refuses to load a
// property list whose file name does not match its label.
const (
	// launchdLabelPrefix is the reverse DNS prefix of the launchd label.
	launchdLabelPrefix = "com.hfdesk."

	// launchAgentsDir is the directory of the per-user LaunchAgents,
	// relative to the home directory of the user.
	launchAgentsDir = "Library/LaunchAgents"

	// launchDaemonsDir is the directory of the machine-wide LaunchDaemons.
	launchDaemonsDir = "/Library/LaunchDaemons"

	// systemHome is the home directory of a machine-wide instance. It is
	// the machine-level state root of the platform: it is never the home
	// directory of the user who runs the install command and never the
	// personal home of root (/var/root).
	systemHome = "/var/lib/hfdesk"

	// plistMode is the permission mode of the property list. launchd
	// reads the file as the user of the domain, so it has to be readable
	// by that user.
	plistMode os.FileMode = 0o644

	// plistDirMode is the permission mode of the directory that holds the
	// property list.
	plistDirMode os.FileMode = 0o755
)

// stopWaitTimeout is the time the manager waits for the graceful shutdown of
// the instance before it unloads the job.
//
// The wait covers the whole orderly shutdown of the server: the drain of its
// HTTP server and the close of its job manager. It is the shared budget of
// the service layer, so the manager never kills an instance that is still
// saving its state.
const stopWaitTimeout = ShutdownBudget

// stopPollInterval is the interval between two checks of the state of the
// job.
const stopPollInterval = 100 * time.Millisecond

// launchdManager manages a HFDesk instance with launchd.
type launchdManager struct {
	// scope is the installation scope of the instance.
	scope Scope
	// uid is the numeric user id of the invoking user. It is the target
	// of the gui domain of a user scope instance.
	uid string
	// home is the home directory of the invoking user. It holds the
	// property list of a user scope instance and it is the reference that
	// keeps a machine-wide instance away from the state of a user.
	home string
	// run runs the commands of the supervisor. It is nil in production,
	// where launchctl is called directly, and a test sets it to a
	// recorder: a lifecycle can then be driven without a supervisor and
	// the commands the manager issues can be asserted.
	run func(ctx context.Context, args ...string) (string, error)
	// stopBudget is the time the manager waits for the graceful shutdown
	// of the instance. It is zero in production, where the wait uses the
	// shared budget of the service layer, and a test shortens it to drive
	// the timeout of the wait.
	stopBudget time.Duration
}

// newPlatformManager returns the launchd manager of the given scope.
func newPlatformManager(scope Scope) (Manager, error) {
	switch scope {
	case ScopeUser, ScopeSystem:
	default:
		return nil, fmt.Errorf("service: unknown scope %q (want %q or %q)", scope, ScopeUser, ScopeSystem)
	}

	manager := &launchdManager{
		scope: scope,
		uid:   strconv.Itoa(os.Getuid()),
	}
	// The home directory of the invoking user is the base of the property
	// list of a user scope instance and the reference that keeps a
	// machine-wide instance away from the state of a user. A machine-wide
	// installation does not need it, so a missing home directory is not an
	// error there.
	home, err := os.UserHomeDir()
	if err == nil {
		manager.home = home
	} else if scope == ScopeUser {
		return nil, fmt.Errorf("service: resolve the home directory of the user scope instance: %w", err)
	}
	return manager, nil
}

// launchdLabel returns the launchd label of the instance.
func launchdLabel() string {
	return launchdLabelPrefix + ServiceName
}

// plistPath returns the path of the property list of the instance.
func (m *launchdManager) plistPath() string {
	if m.scope == ScopeSystem {
		return filepath.Join(launchDaemonsDir, launchdLabel()+".plist")
	}
	return filepath.Join(m.home, launchAgentsDir, launchdLabel()+".plist")
}

// domain returns the launchd domain the instance is registered in.
//
// A LaunchAgent belongs to the gui domain of its user, a LaunchDaemon to the
// system domain. The gui domain of the invoking user is used even when the
// installation options carry another user id: registering a job into the
// domain of another user needs root and is not a per-user installation.
func (m *launchdManager) domain() string {
	if m.scope == ScopeSystem {
		return "system"
	}
	return "gui/" + m.uid
}

// target returns the launchd target of the instance.
func (m *launchdManager) target() string {
	return m.domain() + "/" + launchdLabel()
}

// Install registers the instance with launchd.
//
// The property list is rendered, written atomically and loaded into the
// domain of the instance. An installation that fails after the property list
// was written restores the previous registration, so a failed installation
// never leaves a half configured registration behind. A failure removes the
// directories the installation created as well, so a failed attempt leaves
// the machine as it was.
//
// A machine-wide instance runs as a service account. The account is
// resolved and validated before any directory is created and before the
// property list is written, so a refused installation leaves nothing behind
// and every shell reports the same reason.
func (m *launchdManager) Install(ctx context.Context, opts InstallOptions) error {
	if err := ValidateInstallOptions(opts); err != nil {
		return err
	}
	// A machine-wide instance must not adopt the state of the user who
	// runs the install command. The check runs before the elevation
	// requirement, before any directory is created and before the property
	// list is written: a refused installation leaves nothing behind, and
	// every platform reports the same reason for the same command line.
	if err := m.checkSystemPaths(opts); err != nil {
		return err
	}
	// The service account of a machine-wide instance is resolved before
	// anything is created: an account that does not exist is a refusal with
	// the commands that create it, not a property list that points at an
	// identity the machine does not know.
	account, err := m.resolveAccount(opts)
	if err != nil {
		return err
	}
	// A directory the user named outside of the state directory of the
	// instance has to be writable by the account. The check runs before the
	// directories of the instance are created, so a refused installation
	// leaves nothing behind.
	if err := m.checkNamedDirs(opts, account); err != nil {
		return err
	}
	// The elevation requirement is checked before any directory is created:
	// a permission error of the machine-wide directories is a much less
	// readable answer than the real reason.
	if err := m.requireElevated("an installation"); err != nil {
		return err
	}
	// The directories the installation is about to create are remembered
	// before they are created, so the account is given the ownership of
	// exactly those and of nothing else.
	//
	// The list is also what a failed installation removes again, so a
	// failed installation leaves the machine as it was.
	created := make(map[string][]string, 3)
	for _, dir := range instanceDirs(opts) {
		created[dir.path] = createdDirs(dir.path)
	}
	if err := PrepareDirs(opts); err != nil {
		// The preparation may have created some of the directories
		// before it failed: they are removed again, so a failed attempt
		// leaves the machine as it was.
		removeCreatedDirs(createdDirOrder(opts, created))
		return err
	}
	// The installation owns the directories it created from here on: a
	// failure removes them again, so a failed installation never leaves
	// the directories of the instance behind.
	if err := m.register(ctx, opts, account, created); err != nil {
		removeCreatedDirs(createdDirOrder(opts, created))
		return err
	}
	return nil
}

// register writes the registration of the instance and hands it to the
// supervisor.
//
// The directories of the instance are already created when the function is
// called. A failure of the registration is rolled back: the property list is
// restored and the directories the installation created are removed by the
// caller, so a failed installation leaves no half configured registration and
// no empty directories behind.
func (m *launchdManager) register(ctx context.Context, opts InstallOptions, account *serviceAccount, created map[string][]string) error {
	// The instance runs as the account, so the account owns the directories
	// the instance writes.
	if err := m.grantInstanceDirs(opts, account, created); err != nil {
		return err
	}
	if err := m.preflight(opts); err != nil {
		return err
	}

	content, err := renderPlist(opts, account)
	if err != nil {
		return err
	}

	path := m.plistPath()
	backup, err := backupPlist(path)
	if err != nil {
		return err
	}
	changed := !backup.existed || !bytes.Equal(backup.content, []byte(content))
	if err := writePlist(path, []byte(content), plistMode); err != nil {
		return err
	}

	state, err := m.load(ctx, path, changed)
	if err != nil {
		return m.rollback(ctx, backup, state, err)
	}
	return nil
}

// Uninstall removes the registration of the instance.
//
// Only the registration is removed. The config file, the jobs, the history,
// the logs, the models and the cache of the instance are left alone: an
// uninstall removes the registration of the service, not its data.
//
// A machine-wide uninstallation needs an elevated shell: the LaunchDaemon is
// registered in /Library/LaunchDaemons and it is unloaded from the system
// domain, and both belong to the machine. The check runs after the existence
// of the registration, so a registration that is not there stays a clean
// success in every shell. A user scope instance belongs to the user who
// installed it and needs no elevation.
func (m *launchdManager) Uninstall(ctx context.Context) error {
	path := m.plistPath()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			// There is no registration: the uninstall is already done.
			return nil
		}
		return fmt.Errorf("service: read the registration %q: %w", path, err)
	}

	// The elevation requirement is checked before the job is unloaded and
	// before the property list is removed: a permission error of the
	// machine is a much less readable answer than the real reason.
	if err := m.requireElevated("an uninstallation"); err != nil {
		return err
	}

	// The job is unloaded before the property list is removed, so launchd
	// never keeps a registration whose file is gone. A job that is not
	// loaded any more is not an error: the registration is removed either
	// way.
	if _, err := m.launchctl(ctx, "bootout", m.target()); err != nil && !isServiceMissing(err) {
		return fmt.Errorf("service: bootout %s: %w", m.target(), err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("service: remove the registration %q: %w", path, err)
	}
	return nil
}

// Start starts the registered instance.
//
// The job is started with "launchctl kickstart", which starts a job that is
// not running and leaves a running job alone: starting a running instance
// is a stable no-op. A job that was unloaded by a stop is registered again
// from the property list that is on disk, so a start after a stop works
// without a reinstall.
//
// A machine-wide instance needs an elevated shell: the LaunchDaemon is
// registered in /Library/LaunchDaemons and it is started by the system
// domain, and both belong to the machine. The check runs after the
// existence of the registration, so a registration that is not there is
// reported as such in every shell. A user scope instance belongs to the
// user who installs it and needs no elevation.
func (m *launchdManager) Start(ctx context.Context) error {
	path := m.plistPath()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("service: %s is not installed", m.target())
		}
		return fmt.Errorf("service: read the registration %q: %w", path, err)
	}
	// The elevation requirement is checked before the job is started: a
	// permission error of the machine is a much less readable answer than
	// the real reason.
	if err := m.requireElevated("a start"); err != nil {
		return err
	}
	if err := m.ensureLoaded(ctx, path); err != nil {
		return err
	}
	if _, err := m.launchctl(ctx, "kickstart", m.target()); err != nil {
		return fmt.Errorf("service: kickstart %s: %w", m.target(), err)
	}
	return nil
}

// Stop stops the registered instance.
//
// The graceful shutdown is driven by HFDesk itself: SIGTERM lets the server
// quiesce, drain its connections and close the job manager before the
// process goes away. The job is unloaded afterwards, because a process that
// was killed by a signal exits with a non-zero status and launchd would
// restart it under the KeepAlive policy of the registration. The unload
// makes the stop stable and leaves the property list in place, so a later
// start brings the instance back.
//
// Stopping an instance that does not run is a stable no-op: the state the
// caller asked for already holds.
//
// An instance that did not finish its shutdown within the budget of the
// service layer is reported: the stop unloads the job anyway, but a caller
// never sees a plain success for a shutdown that did not complete.
//
// A machine-wide instance needs an elevated shell: the LaunchDaemon is
// unloaded from the system domain, and the system domain belongs to the
// machine. The check runs after the existence of the registration, so a
// registration that is not there stays a clean success in every shell. A
// user scope instance belongs to the user who installs it and needs no
// elevation.
func (m *launchdManager) Stop(ctx context.Context) error {
	path := m.plistPath()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			// The instance is not registered, so it does not run either.
			return nil
		}
		return fmt.Errorf("service: read the registration %q: %w", path, err)
	}

	// The elevation requirement is checked before the job is unloaded: a
	// permission error of the machine is a much less readable answer than
	// the real reason.
	if err := m.requireElevated("a stop"); err != nil {
		return err
	}

	_, output, err := m.printState(ctx)
	if err != nil {
		return err
	}
	_, _, pid := launchdPrintFields(output)

	if _, err := m.launchctl(ctx, "kill", "SIGTERM", m.target()); err != nil && !isServiceMissing(err) {
		return fmt.Errorf("service: kill SIGTERM %s: %w", m.target(), err)
	}
	// The wait reports the overrun of the budget: an instance that is
	// still there when the budget is over did not shut down in an orderly
	// way.
	waitErr := m.waitForExit(ctx, pid)
	// The job is unloaded even when the instance did not finish its
	// shutdown: a process that is still there would be restarted by
	// launchd under the KeepAlive policy of the registration.
	if _, err := m.launchctl(ctx, "bootout", m.target()); err != nil && !isServiceMissing(err) {
		if waitErr != nil {
			return fmt.Errorf("%w (the registration could not be unloaded: %v)", waitErr, err)
		}
		return fmt.Errorf("service: bootout %s: %w", m.target(), err)
	}
	if waitErr != nil {
		return fmt.Errorf("%w (the instance was unloaded anyway)", waitErr)
	}
	return nil
}

// Restart restarts the registered instance.
//
// The restart is the graceful stop followed by the start: SIGTERM lets the
// server quiesce, drain its connections and close the job manager, and the
// job is unloaded before the replacement is registered and started. The
// replacement is never started while the old instance is still draining, so
// the two never compete for the bind address.
//
// The replacement is started only when the stop succeeded: an instance that
// did not shut down in an orderly way is not covered by a new one, and the
// failure is reported instead.
//
// A machine-wide instance needs an elevated shell: the LaunchDaemon is
// registered in /Library/LaunchDaemons and it is started by the system
// domain, and both belong to the machine. The check runs after the
// existence of the registration, so a registration that is not there is
// reported as such in every shell. A user scope instance belongs to the
// user who installs it and needs no elevation.
func (m *launchdManager) Restart(ctx context.Context) error {
	path := m.plistPath()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("service: %s is not installed", m.target())
		}
		return fmt.Errorf("service: read the registration %q: %w", path, err)
	}
	// The elevation requirement is checked before the job is stopped and
	// before the job is started: a permission error of the machine is a
	// much less readable answer than the real reason.
	if err := m.requireElevated("a restart"); err != nil {
		return err
	}
	// The instance is stopped the way a stop stops it: the graceful
	// shutdown of the server, the bounded wait for it and the unload of
	// the job. The replacement is started only when that succeeded.
	if err := m.Stop(ctx); err != nil {
		return fmt.Errorf("service: restart %s: %w", m.target(), err)
	}
	if err := m.Start(ctx); err != nil {
		return fmt.Errorf("service: restart %s: %w", m.target(), err)
	}
	return nil
}

// Status reports the state of the registered instance.
//
// The description of the instance comes from the property list that is on
// disk, so the status describes the registration and not the intent of the
// caller. The state comes from launchd itself.
func (m *launchdManager) Status(ctx context.Context) (Status, error) {
	path := m.plistPath()
	status := Status{
		State:    StateNotInstalled,
		Scope:    m.scope,
		Name:     launchdLabel(),
		UnitPath: path,
	}

	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// There is no registration at all.
			status.Detail = fmt.Sprintf("no launchd registration at %s", path)
			return status, nil
		}
		return status, fmt.Errorf("service: read the registration %q: %w", path, err)
	}
	registration, err := parsePlist(content)
	if err != nil {
		return status, fmt.Errorf("service: read the registration %q: %w", path, err)
	}
	status.Executable = registration.executable()
	status.Listen = registration.listen()
	status.Paths = registration.paths()
	// The cache root of the instance is the cache directory of the
	// instance's own config file when it defines one, and the cache
	// directory of the registration otherwise. Both come from the
	// registration on disk, so the status describes the instance and not
	// the intent of the caller.
	status.CacheDir = EffectiveCacheDir(status.Paths.ConfigFile, registration.cacheDir())

	loaded, output, err := m.printState(ctx)
	if err != nil {
		return status, err
	}
	if !loaded {
		// The registration is on disk but the job is not loaded into its
		// domain: the instance is registered and does not run.
		status.State = StateStopped
		status.Detail = fmt.Sprintf("%s is not loaded into %s", launchdLabel(), m.domain())
	} else {
		status.State, status.Detail = launchdStatusState(m.target(), output)
	}

	// A registration that points at a program that is gone is broken: the
	// instance can never run again until it is installed again.
	if status.Executable != "" {
		if _, err := os.Stat(status.Executable); err != nil {
			status.State = StateBroken
			status.Detail = fmt.Sprintf("the registered executable %s does not exist", status.Executable)
		}
	}
	return status, nil
}

// preflight checks the conditions an installation needs before it touches
// the system.
func (m *launchdManager) preflight(opts InstallOptions) error {
	if _, err := exec.LookPath("launchctl"); err != nil {
		return fmt.Errorf("service: launchctl is not available: %w", err)
	}
	if _, _, err := ParseListen(opts.Listen); err != nil {
		return err
	}
	info, err := os.Stat(opts.Executable)
	if err != nil {
		return fmt.Errorf("service: executable %q is not usable: %w", opts.Executable, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("service: executable %q is not executable", opts.Executable)
	}
	return nil
}

// requireElevated rejects a machine-wide operation that needs an elevated
// shell when the process does not have one.
//
// A LaunchDaemon is registered in /Library/LaunchDaemons and started by the
// system domain: both belong to the machine and need root. The manager never
// escalates privileges itself: it never calls sudo, su or osascript and it
// never asks the user for a password, so it refuses the operation with a
// readable reason instead of letting it fail with a permission error of a
// directory of the machine.
func (m *launchdManager) requireElevated(action string) error {
	return m.requireElevatedFor(action, os.Geteuid())
}

// requireElevatedFor rejects a machine-wide operation when the given user id
// is not the one of an elevated shell.
//
// The user id is a parameter, so the decision can be driven from both sides
// without an elevated shell: the real check passes the effective user id of
// the process, and the test passes the ids of the two cases.
//
// A user scope instance belongs to the user who installs it: it is written
// into the LaunchAgents directory of that user and it is loaded into the gui
// domain of that user, so it never needs an elevated shell.
func (m *launchdManager) requireElevatedFor(action string, uid int) error {
	if m.scope != ScopeSystem {
		return nil
	}
	if uid == 0 {
		return nil
	}
	return fmt.Errorf("service: %s in the system scope requires an elevated shell: the LaunchDaemon is registered in %s and started by the system domain, HFDesk never escalates privileges itself", action, launchDaemonsDir)
}

// checkSystemPaths rejects a machine-wide installation that would reuse the
// state of a user.
//
// A LaunchDaemon runs as the service account of the machine. A state
// directory inside the home directory of the invoking user would either
// expose the state of the user to the machine or take the state of the user
// away from it, so the installation is refused instead of silently taking
// the state over. The model and the cache trees are never touched by an
// installation: only the three directories of the instance (the config
// directory, the state directory and the log directory) are created, and
// only those are granted to the service account.
//
// A user scope instance is supposed to live inside the directories of its own
// user, so the guard does not touch it.
func (m *launchdManager) checkSystemPaths(opts InstallOptions) error {
	if m.scope != ScopeSystem {
		return nil
	}
	home := m.home
	if home == "" {
		// The home directory of the invoking user is unknown: there is
		// nothing to compare the paths of the installation with, and an
		// unknown home is not a failed installation.
		return nil
	}
	for _, p := range []struct {
		name string
		path string
	}{
		{"config file", opts.Paths.ConfigFile},
		{"state dir", opts.Paths.StateDir},
		{"log dir", opts.Paths.LogDir},
		{"cache dir", opts.CacheDir},
		{"hub dir", opts.HubDir},
		{"local dir", opts.LocalDir},
	} {
		if p.path == "" {
			continue
		}
		if pathWithin(p.path, home) {
			return fmt.Errorf("service: the %s %q of a machine-wide instance must not live inside the home directory %q of the invoking user", p.name, p.path, home)
		}
	}
	return nil
}

// serviceAccount is the identity a machine-wide instance runs as.
//
// The account is a dedicated, stable and unprivileged identity: it owns the
// three directories of the instance and nothing else, so the files it writes
// stay writable across reboots and the instance needs no privilege of root.
type serviceAccount struct {
	// Name is the name of the account.
	Name string
	// UID is the numeric id of the account.
	UID int
	// GID is the numeric id of the primary group of the account.
	GID int
	// Group is the name of the primary group of the account.
	Group string
	// Groups holds the numeric ids of the groups the account belongs to.
	Groups []int
}

// serviceAccountName returns the name of the account a machine-wide instance
// runs as.
//
// The name of the options wins: an installation may name another account. An
// empty name means the default account of the platform.
func serviceAccountName(opts InstallOptions) string {
	if name := strings.TrimSpace(opts.ServiceUser); name != "" {
		return name
	}
	return DefaultServiceUser()
}

// resolveAccount returns the account a machine-wide instance runs as.
//
// A user scope instance runs as the user who installs it: it needs no
// service account, and the returned account is nil.
func (m *launchdManager) resolveAccount(opts InstallOptions) (*serviceAccount, error) {
	if m.scope != ScopeSystem {
		return nil, nil
	}
	return resolveServiceAccount(opts)
}

// resolveServiceAccount resolves the account of a machine-wide instance.
//
// The account is looked up in the user database of the machine: on macOS the
// lookup goes through the directory service, so an account that was created
// with "dscl" is found.
//
// The lookup does not depend on cgo: the darwin build of os/user calls the
// getpwnam_r of libSystem, which asks the directory service, and the reader
// of /etc/passwd is only compiled when the osusergo build tag is set
// explicitly.
//
// The installation never creates an account: an account that does not exist
// is refused with the commands that create it.
func resolveServiceAccount(opts InstallOptions) (*serviceAccount, error) {
	name := serviceAccountName(opts)
	entry, err := user.Lookup(name)
	if err != nil {
		return nil, missingServiceAccountError(name)
	}
	uid, err := unixID(entry.Uid)
	if err != nil {
		return nil, fmt.Errorf("service: the service account %q has the unreadable user id %q", name, entry.Uid)
	}
	gid, err := unixID(entry.Gid)
	if err != nil {
		return nil, fmt.Errorf("service: the service account %q has the unreadable group id %q", name, entry.Gid)
	}
	group, err := user.LookupGroupId(entry.Gid)
	if err != nil {
		return nil, fmt.Errorf("service: the primary group %q of the service account %q cannot be resolved: %w (give the account a primary group and install again)", entry.Gid, name, err)
	}

	account := &serviceAccount{
		Name:  entry.Username,
		UID:   uid,
		GID:   gid,
		Group: group.Name,
		// The primary group belongs to the account as well, so it is
		// part of the groups the account may own a directory through.
		Groups: []int{gid},
	}
	if ids, err := entry.GroupIds(); err == nil {
		for _, id := range ids {
			if gid, err := unixID(id); err == nil {
				account.Groups = append(account.Groups, gid)
			}
		}
	}
	return account, nil
}

// missingServiceAccountError reports the refusal of an installation whose
// service account does not exist.
//
// The installation never creates an account: it names the account and the
// commands that create it, so the administrator can run them and install
// again.
func missingServiceAccountError(name string) error {
	return fmt.Errorf("service: the machine-wide instance needs the service account %q: create it with %q, %q and %q, then install again (HFDesk never creates accounts itself)",
		name,
		fmt.Sprintf("dscl . -create /Users/%s UserShell /usr/bin/false", name),
		fmt.Sprintf("dscl . -create /Users/%s UserHome %s", name, systemHome),
		fmt.Sprintf("dscl . -create /Users/%s UniqueID <a free numeric id>", name))
}

// unixID converts the numeric id of the user database into the unsigned form
// the file system uses.
//
// The user database of macOS knows the account "nobody" with the id -2, and
// the file system holds the very same id as the unsigned 4294967294.
func unixID(value string) (int, error) {
	id, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	if id < 0 {
		id += 1 << 32
	}
	return id, nil
}

// instanceDirs returns the directories an instance owns: the directory of
// the config file, the state directory and the log directory.
//
// They are the only directories the installation creates, so they are the
// only directories the service account is given the ownership of. The model
// and the cache trees are never part of them.
func instanceDirs(opts InstallOptions) []instanceDir {
	return []instanceDir{
		{name: "config directory", path: filepath.Dir(opts.Paths.ConfigFile)},
		{name: "state directory", path: opts.Paths.StateDir},
		{name: "log directory", path: opts.Paths.LogDir},
	}
}

// instanceDir is one directory an instance owns.
type instanceDir struct {
	// name is the name of the directory in an error message.
	name string
	// path is the absolute path of the directory.
	path string
}

// createdDirs returns the directories that do not exist yet on the way to
// the path, from the top down.
//
// The list holds the path itself when it is missing: it is the set of
// directories an installation creates for the path. The list is read before
// the directories are created, so the installation knows which of them it
// owns.
func createdDirs(path string) []string {
	var missing []string
	current := filepath.Clean(path)
	for {
		if _, err := os.Stat(current); err == nil {
			break
		} else if !os.IsNotExist(err) {
			break
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	// The list was collected from the bottom up, the directories are
	// created from the top down.
	for i, j := 0, len(missing)-1; i < j; i, j = i+1, j-1 {
		missing[i], missing[j] = missing[j], missing[i]
	}
	return missing
}

// createdDirOrder returns the directories an installation creates, in the
// order it creates them.
//
// The map holds the directories of every directory of the instance. The
// directories of the instance are created in the order of instanceDirs (the
// config directory, the state directory and the log directory) and from the
// top down inside each of them, so the list is the order of the creation.
//
// A directory that several directories of the instance need is listed once:
// the first directory of the instance that needed it is the one that created
// it.
func createdDirOrder(opts InstallOptions, created map[string][]string) []string {
	var order []string
	seen := make(map[string]bool, len(created))
	for _, dir := range instanceDirs(opts) {
		for _, path := range created[dir.path] {
			if seen[path] {
				continue
			}
			seen[path] = true
			order = append(order, path)
		}
	}
	return order
}

// removeCreatedDirs removes the directories an installation created.
//
// The directories are removed in the reverse order of their creation, so a
// parent is only removed after its children. A directory that is not empty is
// kept: the removal never deletes data and it never walks into a directory.
//
// The removal is best effort: a failure of the removal is ignored, so the
// original failure of the installation stays the error the caller gets.
func removeCreatedDirs(dirs []string) {
	for i := len(dirs) - 1; i >= 0; i-- {
		path := dirs[i]
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			// The directory is gone already, or it is not a directory
			// any more: nothing is removed, so no file is ever deleted.
			continue
		}
		// A directory that holds something is kept: the removal of such
		// a directory would delete what it holds.
		_ = os.Remove(path)
	}
}

// machineDir reports whether the directory is a directory of the machine
// itself: a top level directory of the file system, like /etc or /usr.
//
// Such a directory belongs to the machine and to the software it holds, so
// it is never handed to the account of an instance. The instance needs a
// directory of its own below it.
//
// The rule is the rule of the Linux installation, so the same command line
// is answered the same way on both platforms.
func machineDir(path string) bool {
	clean := filepath.Clean(path)
	return filepath.Dir(clean) == string(filepath.Separator)
}

// grantInstanceDirs grants the directories of the instance to the account.
//
// The directories the installation creates belong to the account: the
// instance writes its config file, its state and its logs there, and it runs
// as the account. The ownership is granted with a single chown per
// directory: the tree of a directory is never walked, and the model and the
// cache trees are never touched.
//
// A directory that existed before the installation belongs to whoever owns
// it, so it is not chowned. The account still has to be able to write there,
// and the installation refuses to go on when it cannot.
//
// A directory of the machine is refused: the directory of the config file is
// derived from the path of a file, so a config file in /etc would hand /etc
// itself to the account of the instance.
//
// Every directory is checked before the first of them is granted, so a
// refused installation leaves every ownership exactly as it was.
//
// The created map holds the directories the installation creates for every
// directory of the instance. It is read before the directories are created,
// so the installation knows which of them it owns.
func (m *launchdManager) grantInstanceDirs(opts InstallOptions, account *serviceAccount, created map[string][]string) error {
	if account == nil {
		// A user scope instance runs as the user who installs it: it owns
		// its directories already.
		return nil
	}
	dirs := instanceDirs(opts)
	// A directory of the machine is refused before the first ownership is
	// changed, so a refused installation leaves every ownership exactly as
	// it was.
	for _, dir := range dirs {
		if machineDir(dir.path) {
			return fmt.Errorf("service: the %s %q is a directory of the machine: the instance needs a directory of its own, name one below a directory of the machine", dir.name, dir.path)
		}
	}
	// The directories are checked before the first of them is granted, so a
	// refused installation leaves every ownership as it was.
	for _, dir := range dirs {
		if len(created[dir.path]) > 0 {
			// The installation creates the directory: it owns it and
			// grants it to the account below.
			continue
		}
		// The directory existed before the installation: it belongs to its
		// owner, so it is left alone. The account still has to be able to
		// write there.
		ok, err := accountCanWrite(dir.path, account)
		if err != nil {
			return fmt.Errorf("service: the %s %q of the machine-wide instance cannot be used: %w", dir.name, dir.path, err)
		}
		if !ok {
			return fmt.Errorf("service: the %s %q of the machine-wide instance is not writable by the service account %q: grant it to the account (for example %q) and install again",
				dir.name, dir.path, account.Name, "chown "+account.Name+" "+dir.path)
		}
	}
	// Every directory passed the check: the directories the installation
	// created belong to the account now.
	for _, dir := range dirs {
		for _, path := range created[dir.path] {
			if err := os.Chown(path, account.UID, account.GID); err != nil {
				return fmt.Errorf("service: grant the %s %q to the service account %q: %w", dir.name, path, account.Name, err)
			}
		}
	}
	return nil
}

// checkNamedDirs verifies that the service account can write to the
// directories the user named explicitly.
//
// The defaults of the scope live inside the state directory of the instance,
// and the state directory belongs to the account, so they need no check. A
// directory the user named is outside of the instance: the installation
// verifies that the account can write there and refuses the installation
// with the commands that grant the access when it cannot.
//
// The exemption for a directory inside the state directory is decided on the
// real location of the two paths: a link inside the state directory that
// points out of it leaves the state directory, and the account has to reach
// the target of the link on its own.
//
// The installation never changes the ownership or the permissions of such a
// tree: a model or a cache tree belongs to whoever put it there.
//
// The refusal names the directory, the account and the access that is
// missing. It never recommends a recursive ownership change of such a tree:
// the access is granted to the directory itself or through the group that
// owns it, and for a tree that holds a lot of data the group is preferable
// to a new owner.
func (m *launchdManager) checkNamedDirs(opts InstallOptions, account *serviceAccount) error {
	if account == nil {
		// A user scope instance runs as the user who installs it: it can
		// write into its own directories.
		return nil
	}
	for _, dir := range []struct {
		name string
		path string
	}{
		{"cache directory", opts.CacheDir},
		{"hub directory", opts.HubDir},
		{"local directory", opts.LocalDir},
	} {
		if dir.path == "" {
			continue
		}
		if pathInsideRealDir(opts.Paths.StateDir, dir.path) {
			// The default of the scope lives inside the state directory
			// of the instance, and the state directory belongs to the
			// account.
			//
			// The decision is made on the real location of the two
			// paths: a link inside the state directory that points out
			// of it leaves the state directory, and the account has to
			// reach the target of the link on its own.
			continue
		}
		ok, err := accountCanWrite(dir.path, account)
		if err != nil {
			return fmt.Errorf("service: the %s %q of the machine-wide instance cannot be used: %w", dir.name, dir.path, err)
		}
		if !ok {
			return fmt.Errorf("service: the service account %q has no write access to the %s %q: grant it access to that directory (for example %q) or make it a member of the group that owns the directory; for a tree that holds a lot of data, granting group access is preferable to changing the owner, and install again",
				account.Name, dir.name, dir.path, "chown "+account.Name+" "+dir.path)
		}
	}
	return nil
}

// accountCanWrite reports whether the account can write to the directory.
//
// The check is the effective one: the account has to create entries in the
// directory and to reach it. A directory the account owns with the mode 0500
// is not writable at all, because the owner has no write permission in it,
// and a directory below a parent without the execute permission cannot be
// reached at all.
//
// A directory that does not exist yet is checked through its nearest
// existing ancestor: the account has to be able to create it there.
func accountCanWrite(path string, account *serviceAccount) (bool, error) {
	return accountCanUseDir(path, accessAccount{
		UID:    account.UID,
		GID:    account.GID,
		Groups: account.Groups,
	})
}

// load registers the property list with launchd and reports what it did to
// the registration of the instance.
//
// A job that is not loaded yet is bootstrapped. A job that is already loaded
// keeps its registration: an installation that does not change the property
// list must not restart the running instance. An installation that does
// change the property list loads the job again, so the new configuration
// takes effect instead of waiting for the next boot of the machine.
//
// The report tells a rollback what the attempt did to the supervisor: the
// state the registration had before it, whether the attempt loaded the job
// and whether it took over the registration of a job that was loaded
// already.
func (m *launchdManager) load(ctx context.Context, path string, changed bool) (launchdLoadState, error) {
	previous, err := m.supervisorState(ctx)
	if err != nil {
		return launchdLoadState{}, err
	}
	state := launchdLoadState{previous: previous}

	switch {
	case !previous.loaded:
		if _, err := m.launchctl(ctx, "bootstrap", m.domain(), path); err != nil {
			return state, fmt.Errorf("service: bootstrap %s: %w", m.target(), err)
		}
		state.bootstrapped = true
	case changed:
		if _, err := m.launchctl(ctx, "bootout", m.target()); err != nil && !isServiceMissing(err) {
			return state, fmt.Errorf("service: bootout %s: %w", m.target(), err)
		}
		// The job is unloaded now: it holds no registration of its
		// own, so the restored file has to be loaded again by a
		// rollback.
		state.replaced = true
		if _, err := m.launchctl(ctx, "bootstrap", m.domain(), path); err != nil {
			return state, fmt.Errorf("service: bootstrap %s: %w", m.target(), err)
		}
		state.bootstrapped = true
	}

	// A job that was disabled in the override database of launchd would
	// never start, so the disabled flag is cleared explicitly. The job is
	// loaded at this point, so the command can address it.
	if _, err := m.launchctl(ctx, "enable", m.target()); err != nil {
		return state, fmt.Errorf("service: enable %s: %w", m.target(), err)
	}
	return state, nil
}

// launchdLoadState is what an installation did to the registration of the
// instance in the supervisor.
type launchdLoadState struct {
	// previous is the state the registration had before the attempt.
	previous launchdSupervisorState
	// bootstrapped reports whether the attempt loaded the job into its
	// domain.
	bootstrapped bool
	// replaced reports whether the attempt took over the registration of
	// a job that was loaded already: the job holds the registration of
	// the attempt now, or it holds none at all.
	replaced bool
}

// launchdSupervisorState is the state of the registration of the instance in
// the supervisor.
type launchdSupervisorState struct {
	// loaded reports whether the job is loaded into its domain.
	loaded bool
	// disabled reports whether the job is disabled in the override
	// database of launchd.
	disabled bool
}

// supervisorState reports the state of the registration of the instance in
// the supervisor: whether the job is loaded into its domain and whether it
// is disabled in the override database of launchd.
func (m *launchdManager) supervisorState(ctx context.Context) (launchdSupervisorState, error) {
	loaded, _, err := m.printState(ctx)
	if err != nil {
		return launchdSupervisorState{}, err
	}
	return launchdSupervisorState{
		loaded:   loaded,
		disabled: m.jobDisabled(ctx),
	}, nil
}

// jobDisabled reports whether the job of the instance is disabled in the
// override database of launchd.
//
// The database of the domain is asked, because that is where the disabled
// flag of a job lives. The question is best effort: a supervisor that cannot
// answer it is treated as one that knows no disabled job, so an installation
// never fails because of it.
func (m *launchdManager) jobDisabled(ctx context.Context) bool {
	output, err := m.launchctl(ctx, "print-disabled", m.domain())
	if err != nil {
		return false
	}
	return launchdDisabledLabel(output, launchdLabel())
}

// launchdDisabledLabel reports whether the output of "launchctl
// print-disabled" says that the job of the given label is disabled.
//
// The output of launchd is not an API: a disabled job is listed as
// "<label> => disabled", and a job that carries further overrides is listed
// as a block that names the disabled flag. Both forms are read. A job that
// is not listed at all is not disabled.
func launchdDisabledLabel(output, label string) bool {
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		sep := strings.Index(line, "=>")
		if sep < 0 {
			continue
		}
		if strings.TrimSpace(line[:sep]) != label {
			continue
		}
		value := strings.TrimSpace(line[sep+2:])
		if value == "" || value == "{" {
			// The job carries a block of overrides: the flags of the
			// block are read.
			return launchdDisabledBlock(lines[i+1:])
		}
		return strings.Contains(strings.ToLower(value), "disabled")
	}
	return false
}

// launchdDisabledBlock reports whether a block of overrides of a job names
// the disabled flag.
func launchdDisabledBlock(lines []string) bool {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "}" {
			return false
		}
		sep := strings.Index(line, "=>")
		if sep < 0 {
			continue
		}
		key := strings.Trim(strings.TrimSpace(line[:sep]), `"`)
		if launchdPrintKey(key) != "disabled" {
			continue
		}
		value := strings.Trim(strings.TrimSpace(line[sep+2:]), `"`)
		return strings.EqualFold(value, "true")
	}
	return false
}

// ensureLoaded registers the job with launchd when it is not loaded into its
// domain any more.
func (m *launchdManager) ensureLoaded(ctx context.Context, path string) error {
	loaded, _, err := m.printState(ctx)
	if err != nil {
		return err
	}
	if loaded {
		return nil
	}
	if _, err := m.launchctl(ctx, "bootstrap", m.domain(), path); err != nil {
		return fmt.Errorf("service: bootstrap %s: %w", m.target(), err)
	}
	return nil
}

// printState reports whether the job is loaded into its domain and returns
// the output of "launchctl print".
func (m *launchdManager) printState(ctx context.Context) (bool, string, error) {
	output, err := m.launchctl(ctx, "print", m.target())
	if err != nil {
		if isServiceMissing(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("service: print %s: %w", m.target(), err)
	}
	return true, output, nil
}

// waitForExit waits until the registered process is gone.
//
// The wait is bounded: a process that ignores the signal must not block the
// caller forever. The bound is the shared budget of the service layer, and
// an instance that is still there when the budget is over is reported: the
// caller has to know that the orderly shutdown did not happen.
func (m *launchdManager) waitForExit(ctx context.Context, pid int) error {
	budget := m.shutdownBudget()
	deadline := time.Now().Add(budget)
	for {
		loaded, output, err := m.printState(ctx)
		if err != nil {
			return err
		}
		if !loaded {
			// The job is not loaded any more.
			return nil
		}
		state, _, current := launchdPrintFields(output)
		if !launchdJobRunning(state, current) {
			return nil
		}
		if pid > 0 && current != pid {
			// The process was replaced: the old one is gone.
			return nil
		}
		if time.Now().After(deadline) {
			// The instance is still there: the shutdown did not
			// happen in an orderly way.
			return &stopTimeoutError{budget: budget}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(stopPollInterval):
		}
	}
}

// shutdownBudget returns the time the manager waits for the graceful
// shutdown of the instance.
//
// The budget is the shared budget of the service layer, so the wait never
// ends before the shutdown it waits for.
func (m *launchdManager) shutdownBudget() time.Duration {
	if m.stopBudget > 0 {
		return m.stopBudget
	}
	return stopWaitTimeout
}

// stopTimeoutError reports that the instance did not finish its graceful
// shutdown within the budget of the service layer.
type stopTimeoutError struct {
	// budget is the time the manager waited for the shutdown.
	budget time.Duration
}

// Error names the budget the graceful shutdown of the instance exceeded.
func (e *stopTimeoutError) Error() string {
	return fmt.Sprintf("service: the instance did not finish its graceful shutdown within the %s budget of the service layer", e.budget)
}

// rollback undoes a failed installation: the job is unloaded again, the
// property list is restored to the state it had before the installation and
// the supervisor is given the state of the registration back.
//
// The state of the supervisor is restored as well: the instance may have
// been running before the installation, and a rollback that only restores the
// file would leave a registration that claims to be running while nothing
// runs. A state that cannot be restored is reported, so the failure of the
// installation never hides the failure of the rollback.
func (m *launchdManager) rollback(ctx context.Context, backup *plistBackup, state launchdLoadState, cause error) error {
	// The registration of the failed attempt is unloaded when the attempt
	// loaded the job or took over the registration of a job that was
	// loaded already: launchd must not keep the registration of an
	// installation that failed, and the restored file has to be loaded
	// again.
	unload := state.replaced || (state.bootstrapped && !state.previous.loaded)
	if unload {
		if _, err := m.launchctl(ctx, "bootout", m.target()); err != nil && !isServiceMissing(err) {
			return fmt.Errorf("%w (the registration could not be unloaded: %v)", cause, err)
		}
	}
	if err := backup.restore(); err != nil {
		return fmt.Errorf("%w (the property list could not be restored: %v)", cause, err)
	}
	if state.replaced {
		// The attempt took over the registration of a job that was
		// loaded before it: the restored file is loaded again, so the
		// instance runs like it did before.
		if _, err := m.launchctl(ctx, "bootstrap", m.domain(), backup.path); err != nil {
			return fmt.Errorf("%w (the restored registration could not be loaded: %v)", cause, err)
		}
	}
	// The enable state of the registration is restored: the failed
	// attempt may have enabled a job that was disabled before it.
	if state.previous.disabled {
		if _, err := m.launchctl(ctx, "disable", m.target()); err != nil {
			return fmt.Errorf("%w (the restored registration could not be disabled: %v)", cause, err)
		}
	} else if state.previous.loaded {
		if _, err := m.launchctl(ctx, "enable", m.target()); err != nil {
			return fmt.Errorf("%w (the restored registration could not be enabled: %v)", cause, err)
		}
	}
	return cause
}

// plistBackup is the state of a property list before it was overwritten.
type plistBackup struct {
	// path is the path of the property list.
	path string
	// content is the content of the property list before the change.
	content []byte
	// mode is the permission mode of the property list before the change.
	mode os.FileMode
	// existed reports whether there was a property list at all.
	existed bool
}

// backupPlist reads the property list at path, if it exists.
func backupPlist(path string) (*plistBackup, error) {
	backup := &plistBackup{path: path}
	content, err := os.ReadFile(path)
	switch {
	case err == nil:
		backup.existed = true
		backup.content = content
		if info, err := os.Stat(path); err == nil {
			backup.mode = info.Mode().Perm()
		}
	case os.IsNotExist(err):
		// There was no registration before.
	default:
		return nil, fmt.Errorf("service: read the registration %q: %w", path, err)
	}
	return backup, nil
}

// restore puts the property list back into the state it had before the
// installation.
func (b *plistBackup) restore() error {
	if !b.existed {
		if err := os.Remove(b.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("service: remove the registration %q: %w", b.path, err)
		}
		return nil
	}
	return writePlist(b.path, b.content, b.mode)
}

// writePlist writes a property list to path.
//
// The file is written to a temporary file in the same directory and renamed
// afterwards, so launchd never reads a half written property list.
func writePlist(path string, content []byte, mode os.FileMode) error {
	if mode == 0 {
		mode = plistMode
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, plistDirMode); err != nil {
		return fmt.Errorf("service: create the registration directory %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".hfdesk-plist-*")
	if err != nil {
		return fmt.Errorf("service: create a temporary property list in %q: %w", dir, err)
	}
	name := tmp.Name()
	// The temporary file is removed when it was not renamed onto the
	// registration.
	defer os.Remove(name)

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("service: write the temporary property list %q: %w", name, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("service: set the mode of %q: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("service: close the temporary property list %q: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("service: install the property list %q: %w", path, err)
	}
	return nil
}

// launchctl runs a command of the supervisor and returns its standard
// output.
//
// The runner of the manager is used when there is one: a test drives the
// manager through it and records the commands the manager issues, so a
// lifecycle can be checked without a supervisor.
func (m *launchdManager) launchctl(ctx context.Context, args ...string) (string, error) {
	if m.run != nil {
		return m.run(ctx, args...)
	}
	return runLaunchctl(ctx, args...)
}

// runLaunchctl runs launchctl with the given arguments and returns its
// standard output.
//
// The command is never run through a shell: the arguments are passed to
// launchctl as they are, so a path with a space or a quote in it cannot
// change the command.
func runLaunchctl(ctx context.Context, args ...string) (string, error) {
	path, err := exec.LookPath("launchctl")
	if err != nil {
		return "", fmt.Errorf("service: launchctl is not available: %w", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail != "" {
			return stdout.String(), fmt.Errorf("service: launchctl %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return stdout.String(), fmt.Errorf("service: launchctl %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

// serviceMissingMarkers are the messages launchctl prints when a job is not
// loaded into its domain, or when it is not running.
var serviceMissingMarkers = []string{
	"could not find service",
	"service not found",
	"no such service",
	"not found",
	"no such process",
	"not running",
}

// isServiceMissing reports whether the error of a launchctl call says that
// the job is not loaded into its domain, or that it is not running.
func isServiceMissing(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range serviceMissingMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// pathWithin reports whether path is inside dir.
func pathWithin(path, dir string) bool {
	if dir == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// plistTemplate is the launchd property list of a HFDesk instance.
//
// The property list is rendered from a template, so the same options always
// produce the same bytes and the bytes can be inspected without touching
// the system.
//
// The property list carries no StandardOutPath and no StandardErrorPath:
// launchd does not rotate those files, so they would grow without bound.
// HFDesk writes its own bounded rotating log file into the log directory of
// the instance instead (see the service host), and the standard output and
// the standard error of the process stay connected to the supervisor.
//
// The property list carries the ExitTimeOut of the job: the number of
// seconds launchd waits after it asked the instance to stop before it kills
// it. The default of launchd is 20 seconds, which is exactly the total
// budget of the graceful shutdown of the server, so the default would kill
// an instance that is still saving its state. The registration carries the
// shared budget of the service layer instead, so the deadline of the
// supervisor is never shorter than the shutdown it waits for.
//
// The environment of the process carries the home directory and nothing
// else: a token, a proxy password or a basic auth password is never part of
// a registration, because a property list is readable by every user of the
// machine. The credentials of an instance come from its own config file.
//
// A machine-wide instance names the account it runs as and the primary group
// of that account. A user scope instance names neither: a LaunchAgent runs
// as the user of its gui domain.
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label | xml}}</string>
	<key>ProgramArguments</key>
	<array>
{{- range .ProgramArguments}}
		<string>{{. | xml}}</string>
{{- end}}
	</array>
	<key>WorkingDirectory</key>
	<string>{{.WorkingDirectory | xml}}</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Background</string>
	<key>ExitTimeOut</key>
	<integer>{{.ExitTimeOut}}</integer>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>{{.Home | xml}}</string>
	</dict>
{{- if .UserName}}
	<key>UserName</key>
	<string>{{.UserName | xml}}</string>
	<key>GroupName</key>
	<string>{{.GroupName | xml}}</string>
{{- end}}
</dict>
</plist>
`

// plistFuncs are the template functions of the property list. Every value
// is escaped, so a path with a markup character in it cannot break the
// property list.
var plistFuncs = template.FuncMap{
	"xml": func(value string) (string, error) {
		var buf bytes.Buffer
		if err := xml.EscapeText(&buf, []byte(value)); err != nil {
			return "", err
		}
		return buf.String(), nil
	},
}

// plistData is the data of the property list template.
type plistData struct {
	// Label is the launchd label of the instance.
	Label string
	// ProgramArguments is the argument vector of the registered process.
	ProgramArguments []string
	// WorkingDirectory is the state directory of the instance.
	WorkingDirectory string
	// Home is the home directory of the registered process.
	Home string
	// UserName is the account the registered process runs as. It is empty
	// for a user scope instance: a LaunchAgent runs as the user of its
	// gui domain.
	UserName string
	// GroupName is the primary group of the account.
	GroupName string
	// ExitTimeOut is the number of seconds launchd waits after it asked
	// the instance to stop before it kills it. It is the shared budget of
	// the service layer, so the deadline of the supervisor is never
	// shorter than the graceful shutdown of the instance.
	ExitTimeOut int
}

// renderPlist renders the launchd property list of an installation.
//
// The function is pure: it reads the environment of the caller and it
// writes nothing, so the property list of an installation can be inspected
// and tested without touching the machine.
//
// A machine-wide instance runs as the given service account: the property
// list names the account and its primary group, so launchd starts the
// instance with the identity the installation granted its directories to.
// A user scope instance names neither: it runs as the user of its gui
// domain.
func renderPlist(opts InstallOptions, account *serviceAccount) (string, error) {
	if err := validatePlistOptions(opts); err != nil {
		return "", err
	}
	home, err := plistHome(opts)
	if err != nil {
		return "", err
	}

	data := plistData{
		Label:            launchdLabel(),
		ProgramArguments: append([]string{opts.Executable}, RunArgs(opts)...),
		WorkingDirectory: opts.Paths.StateDir,
		Home:             home,
		ExitTimeOut:      int(ShutdownBudget / time.Second),
	}
	if opts.Scope == ScopeSystem {
		if account == nil {
			return "", fmt.Errorf("service: the machine-wide instance needs a service account")
		}
		data.UserName = account.Name
		data.GroupName = account.Group
	}
	tmpl, err := template.New("plist").Funcs(plistFuncs).Parse(plistTemplate)
	if err != nil {
		return "", fmt.Errorf("service: parse the property list template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("service: render the property list: %w", err)
	}
	return buf.String(), nil
}

// validatePlistOptions rejects the options a property list cannot be
// rendered from. The check does not touch the file system: the existence of
// the executable is verified by the installation itself.
func validatePlistOptions(opts InstallOptions) error {
	switch opts.Scope {
	case ScopeUser, ScopeSystem:
	default:
		return fmt.Errorf("service: invalid scope %q (want %q or %q)", opts.Scope, ScopeUser, ScopeSystem)
	}
	if opts.Executable == "" {
		return fmt.Errorf("service: executable is required")
	}
	if !filepath.IsAbs(opts.Executable) {
		return fmt.Errorf("service: executable %q must be an absolute path", opts.Executable)
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
	return nil
}

// plistHome returns the home directory the registered process sees.
//
// A user scope instance runs as the user of its gui domain, so it keeps the
// home directory of that user. A machine-wide instance never inherits the
// home directory of the user who happens to run the install command and
// never inherits the personal home of root: it uses the machine-level
// state root of the platform.
func plistHome(opts InstallOptions) (string, error) {
	if opts.Scope == ScopeSystem {
		return systemHome, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("service: resolve the home directory of the user scope instance: %w", err)
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", fmt.Errorf("service: the home directory of the user scope instance is not an absolute path")
	}
	return home, nil
}

// plistFile is the document element of a property list.
type plistFile struct {
	XMLName xml.Name  `xml:"plist"`
	Dict    plistDict `xml:"dict"`
}

// plistDict is a dictionary of a property list: an ordered list of keys with
// their values.
type plistDict struct {
	keys   []string
	values map[string]plistValue
}

// UnmarshalXML reads the key/value pairs of a dictionary element.
func (d *plistDict) UnmarshalXML(dec *xml.Decoder, start xml.StartElement) error {
	d.keys = nil
	d.values = map[string]plistValue{}
	for {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		switch element := token.(type) {
		case xml.StartElement:
			if element.Name.Local != "key" {
				// A dictionary holds keys and values only: anything else
				// is skipped instead of shifting the pairs.
				if err := dec.Skip(); err != nil {
					return err
				}
				continue
			}
			var key string
			if err := dec.DecodeElement(&key, &element); err != nil {
				return err
			}
			value, err := decodePlistValue(dec)
			if err != nil {
				return err
			}
			if _, seen := d.values[key]; !seen {
				d.keys = append(d.keys, key)
			}
			d.values[key] = value
		case xml.EndElement:
			return nil
		}
	}
}

// get returns the value of a key.
func (d *plistDict) get(key string) (plistValue, bool) {
	value, ok := d.values[key]
	return value, ok
}

// keyList returns the keys of the dictionary in the order they appear.
func (d *plistDict) keyList() []string {
	return append([]string(nil), d.keys...)
}

// plistValue is one value of a property list.
type plistValue struct {
	// Kind is the name of the element the value was written as.
	Kind string
	// Text is the content of a scalar value.
	Text string
	// Dict is the content of a dictionary value.
	Dict *plistDict
	// Array is the content of an array value.
	Array []plistValue
}

// UnmarshalXML reads one value element of a property list.
func (v *plistValue) UnmarshalXML(dec *xml.Decoder, start xml.StartElement) error {
	v.Kind = start.Name.Local
	switch v.Kind {
	case "dict":
		dict := &plistDict{}
		if err := dec.DecodeElement(dict, &start); err != nil {
			return err
		}
		v.Dict = dict
	case "array":
		items, err := decodePlistChildren(dec)
		if err != nil {
			return err
		}
		v.Array = items
	default:
		var text string
		if err := dec.DecodeElement(&text, &start); err != nil {
			return err
		}
		v.Text = text
	}
	return nil
}

// stringValue returns the value as a string.
func (v plistValue) stringValue() (string, bool) {
	if v.Kind != "string" {
		return "", false
	}
	return v.Text, true
}

// boolValue returns the value as a boolean.
func (v plistValue) boolValue() (bool, bool) {
	switch v.Kind {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// intValue returns the value as an integer.
func (v plistValue) intValue() (int, bool) {
	if v.Kind != "integer" {
		return 0, false
	}
	number, err := strconv.Atoi(strings.TrimSpace(v.Text))
	if err != nil {
		return 0, false
	}
	return number, true
}

// decodePlistChildren reads the child elements of the current element.
func decodePlistChildren(dec *xml.Decoder) ([]plistValue, error) {
	var items []plistValue
	for {
		token, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch element := token.(type) {
		case xml.StartElement:
			var item plistValue
			if err := dec.DecodeElement(&item, &element); err != nil {
				return nil, err
			}
			items = append(items, item)
		case xml.EndElement:
			return items, nil
		}
	}
}

// decodePlistValue reads the value element that follows a key.
func decodePlistValue(dec *xml.Decoder) (plistValue, error) {
	for {
		token, err := dec.Token()
		if err != nil {
			return plistValue{}, err
		}
		switch element := token.(type) {
		case xml.StartElement:
			var value plistValue
			if err := dec.DecodeElement(&value, &element); err != nil {
				return plistValue{}, err
			}
			return value, nil
		case xml.EndElement:
			return plistValue{}, fmt.Errorf("the property list holds a key without a value")
		}
	}
}

// plistRegistration is the registration a launchd property list describes.
type plistRegistration struct {
	// Label is the launchd label of the registration.
	Label string
	// ProgramArguments is the argument vector of the registered process.
	ProgramArguments []string
	// WorkingDirectory is the working directory of the process.
	WorkingDirectory string
	// Environment is the environment of the process.
	Environment map[string]string
	// UserName is the user the process runs as.
	UserName string
	// GroupName is the group the process runs as.
	GroupName string
	// RunAtLoad reports whether launchd starts the job when it is loaded.
	RunAtLoad bool
	// KeepAliveSuccessfulExit is the "SuccessfulExit" key of the KeepAlive
	// dictionary. It is false in a HFDesk registration: the job is
	// restarted after a failure, not after a clean exit.
	KeepAliveSuccessfulExit bool
	// ExitTimeOut is the number of seconds launchd waits after it asked
	// the instance to stop before it kills it.
	ExitTimeOut int
	// Keys are the keys of the property list, in the order they appear.
	Keys []string
}

// parsePlist reads a launchd property list.
func parsePlist(content []byte) (plistRegistration, error) {
	registration := plistRegistration{Environment: map[string]string{}}
	var file plistFile
	if err := xml.Unmarshal(content, &file); err != nil {
		return registration, fmt.Errorf("parse the property list: %w", err)
	}

	registration.Keys = file.Dict.keyList()
	for _, key := range registration.Keys {
		value, _ := file.Dict.get(key)
		switch key {
		case "Label":
			registration.Label, _ = value.stringValue()
		case "ProgramArguments":
			for _, item := range value.Array {
				text, _ := item.stringValue()
				registration.ProgramArguments = append(registration.ProgramArguments, text)
			}
		case "WorkingDirectory":
			registration.WorkingDirectory, _ = value.stringValue()
		case "EnvironmentVariables":
			if value.Dict == nil {
				continue
			}
			for _, name := range value.Dict.keyList() {
				envValue, _ := value.Dict.get(name)
				text, _ := envValue.stringValue()
				registration.Environment[name] = text
			}
		case "UserName":
			registration.UserName, _ = value.stringValue()
		case "GroupName":
			registration.GroupName, _ = value.stringValue()
		case "RunAtLoad":
			registration.RunAtLoad, _ = value.boolValue()
		case "ExitTimeOut":
			registration.ExitTimeOut, _ = value.intValue()
		case "KeepAlive":
			if value.Dict == nil {
				continue
			}
			keepAlive, _ := value.Dict.get("SuccessfulExit")
			registration.KeepAliveSuccessfulExit, _ = keepAlive.boolValue()
		}
	}
	return registration, nil
}

// executable returns the program the registration points at.
func (r plistRegistration) executable() string {
	if len(r.ProgramArguments) == 0 {
		return ""
	}
	return r.ProgramArguments[0]
}

// argumentValue returns the value of a flag of the argument vector.
func (r plistRegistration) argumentValue(flag string) string {
	for i, arg := range r.ProgramArguments {
		if arg == flag && i+1 < len(r.ProgramArguments) {
			return r.ProgramArguments[i+1]
		}
	}
	return ""
}

// listen returns the bind address of the registration.
func (r plistRegistration) listen() string {
	return r.argumentValue("--listen")
}

// cacheDir returns the Hugging Face cache root recorded in the
// registration.
//
// The value is empty when the registration carries no cache directory: the
// instance then uses the cache root of its own config file, or the default
// cache root of the platform.
func (r plistRegistration) cacheDir() string {
	return r.argumentValue("--cache-dir")
}

// paths returns the runtime path model of the registration.
func (r plistRegistration) paths() runtimepaths.Paths {
	return runtimepaths.Paths{
		ConfigFile: r.argumentValue("--config"),
		StateDir:   r.argumentValue("--state-dir"),
		LogDir:     r.argumentValue("--log-dir"),
	}
}

// launchdStatusState maps the output of "launchctl print" to the state of the
// instance and to the detail of the status.
//
// A job that runs is running, a job that exited with a non-zero status
// failed, and a job that is not running and that exited cleanly is stopped.
//
// The output of launchd is not an API, so a state line of a release that
// this build does not know is read through the process of the job: launchd
// reports the pid of the live process of a job that runs. A job that does
// not run, that exited cleanly and whose state this build does not know is
// reported as unknown, and the detail carries what the supervisor reported,
// so an administrator and a failing test see the same thing.
func launchdStatusState(target, output string) (State, string) {
	state := parseLaunchdPrint(output)
	switch state {
	case StateFailed:
		_, exitCode, _ := launchdPrintFields(output)
		return state, fmt.Sprintf("the instance exited with the status %d", exitCode)
	case StateUnknown:
		_, exitCode, pid := launchdPrintFields(output)
		switch {
		case pid > 0 && launchdProcessAlive(pid):
			return StateRunning, fmt.Sprintf("launchctl print %s reported no state, but the process %d of the instance is alive", target, pid)
		case exitCode != 0:
			return StateFailed, fmt.Sprintf("the instance exited with the status %d", exitCode)
		default:
			return state, launchdUnknownStateDetail(target, output)
		}
	default:
		return state, ""
	}
}

// parseLaunchdPrint maps the output of "launchctl print" to the state of the
// instance.
//
// A job that is not running is stopped, unless it exited with a non-zero
// status: such a job failed.
func parseLaunchdPrint(output string) State {
	state, exitCode, _ := launchdPrintFields(output)
	switch strings.ToLower(state) {
	case "running":
		return StateRunning
	case "exited", "not running", "stopped":
		if exitCode != 0 {
			return StateFailed
		}
		return StateStopped
	case "waiting":
		return StateStarting
	default:
		return StateUnknown
	}
}

// launchdPrintFields extracts the state, the last exit code and the process
// id from the output of "launchctl print".
//
// The output of "launchctl print" is not an API: Apple documents that it may
// change from release to release without warning. The fields are therefore
// read defensively. A line is trimmed, the key and the value are separated
// at the first "=", the key may be quoted, the value may be quoted and it
// may end with a semicolon, and a field is looked for in the whole output:
// a state of a nested block of the output is a state of the job as well.
//
// The first occurrence of a field wins: the properties of the job itself are
// printed before the nested dictionaries of the output, so the state of the
// job is the first state of the output.
func launchdPrintFields(output string) (state string, exitCode int, pid int) {
	var haveState, haveExit, havePID bool
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := launchdPrintPair(line)
		if !ok {
			continue
		}
		switch launchdPrintKey(key) {
		case "state":
			if !haveState {
				state, haveState = value, true
			}
		case "pid":
			if !havePID {
				if number, err := strconv.Atoi(value); err == nil {
					pid, havePID = number, true
				}
			}
		case "lastexitcode", "lastexitstatus":
			if !haveExit {
				if code, err := strconv.Atoi(value); err == nil {
					exitCode, haveExit = code, true
				}
			}
		}
	}
	return state, exitCode, pid
}

// launchdPrintPair splits a line of the output of "launchctl print" into its
// key and its value.
//
// The output is not a property list, but it borrows the shape of one: the
// key and the value are separated by "=", the key may be quoted, the value
// may be quoted and it may end with a semicolon. A line without a key or
// without a value is not a field.
func launchdPrintPair(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	sep := strings.Index(line, "=")
	if sep < 0 {
		return "", "", false
	}
	key = strings.Trim(strings.TrimSpace(line[:sep]), `"`)
	value = strings.TrimSpace(line[sep+1:])
	value = strings.TrimSpace(strings.TrimSuffix(value, ";"))
	value = strings.Trim(value, `"`)
	if key == "" || value == "" {
		return "", "", false
	}
	return key, value, true
}

// launchdPrintKey normalizes the key of a field of the output of "launchctl
// print".
//
// The keys of the output are lower case and separated by spaces ("last exit
// code"), the keys of a property list are not ("LastExitStatus"). The spaces
// and the underscores are removed and the case is ignored, so a key of the
// output and a key of a property list name the same field.
func launchdPrintKey(key string) string {
	key = strings.ToLower(key)
	key = strings.ReplaceAll(key, " ", "")
	key = strings.ReplaceAll(key, "_", "")
	return key
}

// launchdJobRunning reports whether the job of the output of "launchctl
// print" runs.
//
// A job runs when launchd reports the state "running" or when launchd
// reports the pid of a live process: a state line of a release that this
// build does not know must not hide a running instance.
func launchdJobRunning(state string, pid int) bool {
	if strings.EqualFold(strings.TrimSpace(state), "running") {
		return true
	}
	return pid > 0 && launchdProcessAlive(pid)
}

// launchdProcessAlive reports whether the process with the given id exists.
//
// A process of another user cannot be signalled: the check of such a process
// ends in a permission error, and the process exists.
func launchdProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// launchdUnknownStateDetail describes the output of "launchctl print" for an
// instance whose state could not be read.
//
// The detail carries what the supervisor reported, so an administrator and a
// failing test see the same thing. The excerpt is short: the output of
// launchd is not an API, and a status is a line of a terminal.
func launchdUnknownStateDetail(target, output string) string {
	return fmt.Sprintf("the state of %s is unknown: launchctl print %s reported: %s",
		launchdLabel(), target, launchdPrintExcerpt(output, launchdStateDetailLimit))
}

// launchdStateDetailLimit is the maximum number of characters of the output
// of "launchctl print" that the detail of a status carries.
const launchdStateDetailLimit = 400

// launchdPrintExcerpt returns the output of "launchctl print" as a single
// line of at most limit characters.
//
// The empty lines are dropped and the lines are joined with a semicolon, so
// the excerpt stays one line. The value of an environment variable whose
// name looks like a secret is removed: the registration of HFDesk carries no
// credential, but the output of launchd is not under the control of HFDesk.
func launchdPrintExcerpt(output string, limit int) string {
	lines := make([]string, 0, 8)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines = append(lines, launchdRedactSecret(line))
	}
	excerpt := strings.Join(lines, "; ")
	if runes := []rune(excerpt); len(runes) > limit {
		excerpt = string(runes[:limit]) + "..."
	}
	return excerpt
}

// launchdSecretMarkers are the parts of the name of an environment variable
// whose value must not reach a status detail.
var launchdSecretMarkers = []string{
	"token",
	"secret",
	"password",
	"passwd",
	"credential",
	"apikey",
	"privatekey",
}

// launchdRedactSecret removes the value of an environment variable whose name
// looks like a secret.
//
// The name of the variable is the part of the line before the separator, so
// only the value of a variable with a secret looking name is removed.
func launchdRedactSecret(line string) string {
	arrow := strings.Index(line, "=>")
	sep := arrow
	if sep < 0 {
		sep = strings.Index(line, "=")
	}
	if sep < 0 {
		return line
	}
	name := strings.ToLower(line[:sep])
	for _, marker := range launchdSecretMarkers {
		if strings.Contains(name, marker) {
			if arrow >= 0 {
				return line[:arrow+2] + " <redacted>"
			}
			return line[:sep+1] + " <redacted>"
		}
	}
	return line
}
