// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

const (
	// unitName is the supervisor visible name of the HFDesk unit.
	unitName = ServiceName + ".service"

	// unitFileMode is the permission mode of a written unit file. The
	// supervisor reads the unit as an unprivileged user, so the file must
	// be world readable.
	unitFileMode os.FileMode = 0o644

	// unitDirMode is the permission mode of the directory that holds the
	// unit file.
	unitDirMode os.FileMode = 0o755

	// systemUnitPath is the machine wide location of the unit file.
	systemUnitPath = "/etc/systemd/system/" + unitName

	// systemServiceHome is the HOME of a machine wide instance. It is a
	// machine level directory: a system scope instance never inherits the
	// home of the user who happens to run the install command.
	systemServiceHome = "/var/lib/hfdesk"
)

// systemdManager drives the systemd supervisor of HFDesk.
//
// A user scope installation is a systemd user unit: it belongs to the
// session of the user and it is the default, because a desktop or a
// background installation belongs to the user who uses it.
//
// A system scope installation is a machine wide system unit. It is an
// explicit choice and it needs root: the manager never escalates
// privileges, it fails with a clear error when the process is not root.
// The lifecycle commands of the machine wide scope need root as well: the
// unit of the machine belongs to the machine.
//
// The supervisor owns the lifecycle of the instance. HFDesk stays a normal
// foreground process: the unit is a simple service, the process is never
// forked, detached or put into the background by HFDesk itself.
//
// Booting without a login session is an explicit choice of the user or of
// the administrator. The manager therefore never enables linger on its own:
// `loginctl enable-linger` (or anything like it) is never called, so a
// user scope instance starts with the session of the user and stops with it.
//
// The manager drives the supervisor with systemctl. The supervisor is never
// called through a shell, and the manager never launches an interactive
// process: no browser and no tray icon are ever started from the service
// layer.
type systemdManager struct {
	scope Scope
	// restartWait overrides the bounded wait for a restarted instance to
	// run the new executable. It is zero in production, where the wait is
	// the shutdown budget of the service layer; a test drives the failure
	// of a restart without waiting for the whole budget.
	restartWait time.Duration
}

// newPlatformManager returns the systemd manager of the given scope.
func newPlatformManager(scope Scope) (Manager, error) {
	switch scope {
	case ScopeUser, ScopeSystem:
		return &systemdManager{scope: scope}, nil
	default:
		return nil, fmt.Errorf("service: unknown scope %q (want %q or %q)", scope, ScopeUser, ScopeSystem)
	}
}

// unitPath returns the path of the unit file of the manager scope.
//
// A user scope unit lives in the "systemd/user" directory of the per user
// configuration directory: $XDG_CONFIG_HOME/systemd/user/hfdesk.service, or
// ~/.config/systemd/user/hfdesk.service when XDG_CONFIG_HOME is not set. A
// system scope unit lives in /etc/systemd/system.
//
// The path is resolved on every call, so the manager always describes the
// registration of the current environment.
func (m *systemdManager) unitPath() (string, error) {
	if m.scope == ScopeSystem {
		return systemUnitPath, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil || home == "" {
			return "", errors.New("service: the configuration directory of the user is unknown, the unit file path cannot be resolved")
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", unitName), nil
}

// systemctlArgs prefixes the supervisor arguments with the scope of the
// manager: a user scope installation is driven by "systemctl --user", a
// machine wide one by plain "systemctl".
func (m *systemdManager) systemctlArgs(args ...string) []string {
	if m.scope == ScopeUser {
		return append([]string{"--user"}, args...)
	}
	return args
}

// supervisorHint returns the command a user can run to inspect the unit of
// the manager scope.
func (m *systemdManager) supervisorHint() string {
	if m.scope == ScopeUser {
		return "systemctl --user"
	}
	return "systemctl"
}

// systemctlError describes a failed supervisor call. The message of the
// supervisor is kept, so the reason of a failure is never lost.
type systemctlError struct {
	args   []string
	err    error
	stderr string
}

func (e *systemctlError) Error() string {
	msg := fmt.Sprintf("service: systemctl %s failed: %v", strings.Join(e.args, " "), e.err)
	if e.stderr != "" {
		msg += ": " + e.stderr
	}
	return msg
}

func (e *systemctlError) Unwrap() error { return e.err }

// runSystemctl runs the supervisor with the given arguments and returns its
// standard output. The supervisor is never called through a shell.
func (m *systemdManager) runSystemctl(ctx context.Context, args ...string) (string, error) {
	full := m.systemctlArgs(args...)
	cmd := exec.CommandContext(ctx, "systemctl", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &systemctlError{
			args:   full,
			err:    err,
			stderr: strings.TrimSpace(stderr.String()),
		}
	}
	return stdout.String(), nil
}

// requireRoot rejects a machine wide operation that needs root when the
// process does not have it. The manager never escalates privileges itself:
// it never calls sudo, su or pkexec.
func (m *systemdManager) requireRoot(action string) error {
	return m.requireRootFor(action, os.Geteuid())
}

// requireRootFor rejects a machine wide operation when the given user id is
// not the one of root.
//
// The user id is a parameter, so the decision can be driven from both sides
// without an elevated shell: the real check passes the effective user id of
// the process, and the test passes the ids of the two cases.
//
// A user scope instance belongs to the user of the session: it is written
// into the configuration directory of the user and it is driven by the
// supervisor of that session, so it never needs root.
func (m *systemdManager) requireRootFor(action string, uid int) error {
	if m.scope != ScopeSystem {
		return nil
	}
	if uid == 0 {
		return nil
	}
	return fmt.Errorf("service: %s in the system scope requires root: run the command as root, HFDesk never escalates privileges itself", action)
}

// serviceAccountName returns the name of the account a machine-wide
// instance runs as: the account the user named, or the default of the
// platform.
//
// The name is resolved without touching the machine: the existence of the
// account is a separate question, so a unit can be rendered for an account
// that is not on the machine yet.
func serviceAccountName(opts InstallOptions) string {
	if opts.ServiceUser != "" {
		return opts.ServiceUser
	}
	return DefaultServiceUser()
}

// serviceAccountCreateCommand returns the command that creates the service
// account of a machine-wide instance.
//
// The command is a hint for the administrator: the installation never runs
// it, it never calls useradd and it never escalates privileges.
func serviceAccountCreateCommand(name string) string {
	return fmt.Sprintf("useradd --system --no-create-home --home-dir %s --shell /usr/sbin/nologin %s", systemServiceHome, name)
}

// resolveServiceAccount resolves the account a machine-wide instance runs
// as.
//
// The account is never created by an installation: the installation refuses
// an instance whose account does not exist and names the command that
// creates it. The account has to be a stable one, so the files it owns stay
// writable across reboots.
func resolveServiceAccount(opts InstallOptions) (*user.User, error) {
	name := serviceAccountName(opts)
	if name == "" {
		return nil, errors.New("service: the machine-wide instance needs a service account")
	}
	account, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("service: the machine-wide instance needs the service account %q: create it with %q and install again (HFDesk never creates accounts itself)", name, serviceAccountCreateCommand(name))
	}
	return account, nil
}

// serviceAccount resolves the account of the manager scope.
//
// A user scope instance runs as the user of the session: it needs no
// service account, so no account is resolved for it.
func (m *systemdManager) serviceAccount(opts InstallOptions) (*user.User, error) {
	if m.scope != ScopeSystem {
		return nil, nil
	}
	return resolveServiceAccount(opts)
}

// errNotWritable reports that the service account cannot write into a
// directory. The caller turns it into the guidance of the installation.
var errNotWritable = errors.New("the service account cannot write into the directory")

// checkServiceAccountAccess verifies that the account of a machine-wide
// instance can write into the directories the user named explicitly.
//
// The scope defaults are not checked: the cache of a machine-wide instance
// lives inside its own state directory, which the installation owns.
//
// A directory inside the state directory of the instance is not checked
// either, as long as it does not exist yet: the installation creates the
// state directory and grants it to the account, and the service creates the
// directory itself below it. A directory that exists already inside the
// state directory belongs to whoever owns it: the installation never walks
// the tree of the state directory and never grants such a directory to the
// account, so it is checked like every other named directory.
//
// The decision is made on the real location of the two paths, so a link
// inside the state directory that points out of it is checked like any other
// path.
//
// The ownership and the permissions of a named tree are never changed: the
// installation refuses the installation and names the way to grant the
// access.
func (m *systemdManager) checkServiceAccountAccess(opts InstallOptions, account *user.User) error {
	if m.scope != ScopeSystem || account == nil {
		return nil
	}
	defaultCache, defaultHub := DefaultCacheDirs(m.scope, opts.Paths)
	for _, dir := range []struct {
		kind string
		path string
		def  string
	}{
		{"cache", opts.CacheDir, defaultCache},
		{"hub", opts.HubDir, defaultHub},
		{"local", opts.LocalDir, ""},
	} {
		if dir.path == "" || dir.path == dir.def {
			continue
		}
		// A directory inside the state directory of the instance that
		// does not exist yet belongs to the account: the installation
		// grants the state directory to the account, and the service
		// creates the directory itself below it.
		//
		// A directory that exists already belongs to whoever owns it, so
		// it is checked against the account below.
		//
		// The decision is made on the real location of the two paths: a
		// link inside the state directory that points out of it leaves
		// the state directory, and the account has to reach the target of
		// the link on its own.
		if pathMissingInsideRealDir(opts.Paths.StateDir, dir.path) {
			continue
		}
		if err := checkDirWritableByAccount(dir.path, account); err != nil {
			if !errors.Is(err, errNotWritable) {
				return fmt.Errorf("service: the %s directory %q of the machine-wide instance is not usable: %w", dir.kind, dir.path, err)
			}
			// The guidance never names a recursive ownership change:
			// the tree of a model or a cache directory belongs to the
			// user who named it, and the installation never walks it.
			// The account is either made a member of the group that
			// owns the directory, or the directory itself is given to
			// the account.
			return fmt.Errorf("service: the service account %q cannot write to the %s directory %q: grant it access and install again (for example add the account to the group that owns the directory, or give the directory itself to the account with %q; for a directory that holds a lot of data, granting group access is preferable to changing the owner)",
				account.Username, dir.kind, dir.path, "chown "+account.Username+" "+dir.path)
		}
	}
	return nil
}

// checkDirWritableByAccount reports whether the account can write into the
// given directory.
//
// The check is the effective one: the account has to create entries in the
// directory and to reach it. A directory the account owns with the mode 0500
// is not writable at all, because the owner has no write permission in it,
// and a directory below a parent without the execute permission cannot be
// reached at all.
//
// A directory that is missing is checked through its nearest existing
// ancestor: the instance creates the directory it needs inside a directory
// that has to be writable for it.
func checkDirWritableByAccount(path string, account *user.User) error {
	identity, err := linuxAccessAccount(account)
	if err != nil {
		return err
	}
	ok, err := accountCanUseDir(path, identity)
	if err != nil {
		return err
	}
	if !ok {
		return errNotWritable
	}
	return nil
}

// linuxAccessAccount returns the identity of the account an access check
// runs for.
//
// The groups of the account are its primary group and the supplementary
// groups the group database of the machine knows: a directory the account
// reaches through a supplementary group is as usable as one it reaches
// through its primary group.
func linuxAccessAccount(account *user.User) (accessAccount, error) {
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return accessAccount{}, fmt.Errorf("the user id %q of the account %q cannot be read: %w", account.Uid, account.Username, err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return accessAccount{}, fmt.Errorf("the group id %q of the account %q cannot be read: %w", account.Gid, account.Username, err)
	}
	identity := accessAccount{UID: uid, GID: gid}
	for _, id := range accountGroupIds(account) {
		group, err := strconv.Atoi(id)
		if err != nil {
			continue
		}
		identity.Groups = append(identity.Groups, group)
	}
	return identity, nil
}

// accountGroupIds returns the ids of the groups the account belongs to.
//
// The primary group of the account is always one of them. The other groups
// are read from the group database of the machine: the standard library
// knows no membership of an account.
func accountGroupIds(account *user.User) []string {
	ids := []string{account.Gid}
	content, err := os.ReadFile("/etc/group")
	if err != nil {
		return ids
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) < 4 {
			continue
		}
		for _, member := range strings.Split(fields[3], ",") {
			if member == account.Username {
				ids = append(ids, fields[2])
				break
			}
		}
	}
	return ids
}

// instanceDir is one directory an installation owns.
type instanceDir struct {
	// name names the directory in an error message.
	name string
	// path is the absolute path of the directory.
	path string
}

// instanceDirs returns the directories an installation owns: the directory
// of the config file, the state directory and the log directory.
//
// These are the directories the installation creates anyway. A model, a
// cache or a hub tree is never one of them: such a tree belongs to the user
// who named it.
func instanceDirs(opts InstallOptions) []instanceDir {
	// The directory of the config file is derived from the path of a
	// file. An empty path has no directory: the parent of an empty path
	// would be the working directory of the process, which belongs to
	// nobody.
	configDir := ""
	if opts.Paths.ConfigFile != "" {
		configDir = filepath.Dir(opts.Paths.ConfigFile)
	}
	dirs := []instanceDir{
		{"config dir", configDir},
		{"state dir", opts.Paths.StateDir},
		{"log dir", opts.Paths.LogDir},
	}
	seen := make(map[string]bool, len(dirs))
	unique := make([]instanceDir, 0, len(dirs))
	for _, dir := range dirs {
		if dir.path == "" || dir.path == "." || seen[dir.path] {
			continue
		}
		seen[dir.path] = true
		unique = append(unique, dir)
	}
	return unique
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
		} else if !errors.Is(err, fs.ErrNotExist) {
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
func machineDir(path string) bool {
	clean := filepath.Clean(path)
	return filepath.Dir(clean) == string(filepath.Separator)
}

// grantInstanceDirs grants the three directories the installation owns to
// the account of the instance.
//
// A directory the installation created belongs to the account: the instance
// writes its config file, its state and its logs there, and it runs as the
// account. The ownership is granted with a single chown per directory: the
// tree of a directory is never walked, and the model and the cache trees are
// never touched.
//
// A directory that existed before the installation belongs to whoever owns
// it, so it is not chowned. The account still has to be able to write there,
// and the installation refuses to go on when it cannot.
//
// A directory of the machine is refused: the directory of the config file is
// derived from the path of a file, so a config file in /etc would hand /etc
// itself to the account of the instance.
//
// Every refusal happens before the first ownership is changed, so a refused
// installation leaves every ownership as it was.
//
// The created map holds the directories the installation creates for every
// directory of the instance. It is read before the directories are created,
// so the installation knows which of them it owns.
func (m *systemdManager) grantInstanceDirs(opts InstallOptions, account *user.User, created map[string][]string) error {
	if m.scope != ScopeSystem || account == nil {
		return nil
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return fmt.Errorf("service: the id of the service account %q is not a number: %w", account.Username, err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return fmt.Errorf("service: the group id of the service account %q is not a number: %w", account.Username, err)
	}
	dirs := instanceDirs(opts)
	// The directories are checked before the first of them is granted, so
	// a refused installation leaves every ownership as it was.
	for _, dir := range dirs {
		if machineDir(dir.path) {
			return fmt.Errorf("service: the %s %q is a directory of the machine: the instance needs a directory of its own, name one below a directory of the machine", dir.name, dir.path)
		}
	}
	// A directory that existed before the installation belongs to its
	// owner: it is left alone, so the account has to be able to write
	// there already.
	for _, dir := range dirs {
		if len(created[dir.path]) > 0 {
			// The installation creates the directory: it owns it and
			// grants it to the account below.
			continue
		}
		if err := checkDirWritableByAccount(dir.path, account); err != nil {
			if !errors.Is(err, errNotWritable) {
				return fmt.Errorf("service: the %s %q of the machine-wide instance is not usable: %w", dir.name, dir.path, err)
			}
			return fmt.Errorf("service: the %s %q of the machine-wide instance is not writable by the service account %q: grant it to the account (for example %q) and install again",
				dir.name, dir.path, account.Username, "chown "+account.Username+" "+dir.path)
		}
	}
	for _, dir := range dirs {
		for _, path := range created[dir.path] {
			if err := os.Chown(path, uid, gid); err != nil {
				return fmt.Errorf("service: grant the %s %q to the service account %q: %w", dir.name, path, account.Username, err)
			}
		}
	}
	return nil
}

// Install registers the service with the supervisor.
//
// The installation validates first, merges its options with an existing
// registration, writes the unit file atomically, reloads the supervisor and
// enables the unit for the boot of the scope when the installation has to.
//
// A first installation enables the registration for the boot of the scope; a
// repeat installation keeps the autostart state it finds, so a unit an
// administrator disabled is never enabled again, and a masked unit refuses the
// installation instead of being replaced (see reloadAndEnable).
//
// An installation that replaces a registration keeps every setting the request
// did not name, so a reinstall with a new binary resets no bind address, paths,
// cache layout or account; the executable always comes from the request.
//
// A reinstalled instance that was running before is restarted and verified,
// because a registration alone does not switch the running process. A stopped
// instance and a first installation are never started.
//
// A machine-wide instance runs as the dedicated service account of the
// platform; the account is validated before anything is created, and the
// installation never creates an account and never changes the ownership of a
// model or a cache tree.
//
// A failure after the unit file was written rolls the unit file back, and a
// failure after the restart of a running instance restores and restarts the
// previous registration. A failure removes the directories the installation
// created as well, so a failed attempt leaves the machine as it was.
//
// The returned options are the registration as written, so a caller reports a
// preserved setting instead of the default it was not reset to.
func (m *systemdManager) Install(ctx context.Context, opts InstallOptions) (InstallOptions, error) {
	// The registration of the scope is read before anything is decided:
	// an installation over an existing registration must merge with what
	// the instance really runs with, and it must know whether the instance
	// has to be brought up again.
	path, err := m.unitPath()
	if err != nil {
		return InstallOptions{}, err
	}
	content, existed, err := readUnitFile(path)
	if err != nil {
		return InstallOptions{}, err
	}
	if existed {
		opts = mergeInstallOptions(opts, registrationOptions(parseUnit(string(content)), opts.Scope))
	}

	if err := ValidateInstallOptions(opts); err != nil {
		return opts, err
	}
	// A path with a control character would end up in the unit file
	// literally, and a newline in it would split the unit into another
	// directive the installation never wrote. The check runs before the
	// root requirement, before any directory is created and before the
	// unit file is written: a refused installation leaves nothing behind.
	if err := validateUnitPaths(opts); err != nil {
		return opts, err
	}
	// A machine-wide instance must not adopt the state of the user who
	// runs the install command. The check runs before the root
	// requirement, before any directory is created and before the unit
	// file is written: a refused installation leaves nothing behind, and
	// every platform reports the same reason for the same command line.
	if err := m.checkSystemPaths(opts); err != nil {
		return opts, err
	}
	// The account of a machine-wide instance is a rule of the scope and
	// not a privilege of the process, so it is validated the same way:
	// before the root requirement, before any directory is created and
	// before the unit file is written. A refused installation leaves
	// nothing behind.
	account, err := m.serviceAccount(opts)
	if err != nil {
		return opts, err
	}
	if err := m.checkServiceAccountAccess(opts, account); err != nil {
		return opts, err
	}
	// The root requirement is checked before any directory is created: a
	// permission error of the machine wide directories is a much less
	// readable answer than the real reason.
	if err := m.requireRoot("an installation"); err != nil {
		return opts, err
	}
	// The state of the replaced registration is read before the unit file
	// is written: only a running instance is restarted, a stopped one has
	// to stay stopped.
	previous := previousRegistration{path: path, content: content, existed: existed}
	if existed {
		running, err := m.registrationRunning(ctx)
		if err != nil {
			return opts, err
		}
		previous.running = running
		// The enablement is read before the unit file is written: a repeat
		// installation keeps the autostart state it finds, and a masked unit
		// is refused instead of silently unmasked.
		enablement := m.registrationEnablement(ctx)
		if enablement.masked {
			return opts, m.maskedUnitRefusal()
		}
		previous.enabled = enablement.enabled
	}
	// The directories the installation is about to create are remembered
	// before they are created, so the account is given the ownership of
	// exactly those and of nothing else. A directory that exists already
	// belongs to its owner and is left alone.
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
		return opts, err
	}
	// The installation owns the directories it created from here on: a
	// failure removes them again, so a failed installation never leaves
	// the directories of the instance behind.
	if err := m.register(ctx, opts, account, created, previous); err != nil {
		removeCreatedDirs(createdDirOrder(opts, created))
		return opts, err
	}
	return opts, nil
}

// previousRegistration is the registration an installation replaces.
type previousRegistration struct {
	// path is the path of the unit file of the scope.
	path string
	// content is the unit file that was there, empty when there was none.
	content []byte
	// existed reports whether the scope had a registration at all.
	existed bool
	// running reports whether the instance ran before the installation.
	// Only such an instance is restarted.
	running bool
	// enabled reports whether the supervisor had the unit enabled for boot.
	// Only a registration that existed carries the flag.
	enabled bool
}

// registrationOptions returns the installation options a unit registers.
//
// The values come from the argument vector and the identity directives of the
// unit, so an installation over an existing registration keeps what the
// instance really runs with instead of what a command line would default to.
func registrationOptions(unit parsedUnit, scope Scope) InstallOptions {
	opts := InstallOptions{
		Scope:      scope,
		Executable: unit.ExecStart,
		Listen:     unit.listen(),
		Paths:      unit.paths(),
		CacheDir:   unit.flagValue("--cache-dir"),
		HubDir:     unit.flagValue("--hub-dir"),
		LocalDir:   unit.flagValue("--local-dir"),
	}
	// A user scope unit names no account: it runs as the user of the
	// session, and an account of the request never applies to it.
	if scope == ScopeSystem {
		opts.ServiceUser = unit.User
	}
	return opts
}

// registrationRunning reports whether the supervisor runs the registered unit.
//
// A unit the supervisor does not know does not run either: the unit file is
// there, but a daemon-reload is what makes the supervisor pick it up. Any other
// failure is reported, because an installation that cannot tell whether the
// instance runs cannot promise to bring it back.
func (m *systemdManager) registrationRunning(ctx context.Context) (bool, error) {
	out, err := m.runSystemctl(ctx, "show", unitName,
		"--property=LoadState", "--property=ActiveState", "--property=SubState")
	if err != nil {
		if isUnitAbsent(err) {
			return false, nil
		}
		return false, fmt.Errorf("service: the state of the installed unit %s could not be determined, the registration was not changed: %w", unitName, err)
	}
	state := parseSystemdShow(out).State
	// A starting instance already runs: it is about to reach the running
	// state, and a reinstall has to switch it to the new program as well.
	return state == StateRunning || state == StateStarting, nil
}

// registrationEnablement is what the supervisor reports about the enablement of
// the unit.
type registrationEnablement struct {
	// enabled reports whether the supervisor had the unit enabled for the
	// boot of the scope.
	enabled bool
	// masked reports whether the unit is masked. A masked unit is a link to
	// /dev/null, so writing the unit file would replace the mask and silently
	// undo the decision of an administrator.
	masked bool
}

// registrationEnablement reads whether the supervisor had the unit enabled for
// the boot of the scope.
//
// The answer comes from the supervisor, not from the file system: an
// enablement is the links the supervisor keeps for the [Install] section, and
// only the supervisor knows them. "systemctl is-enabled" exits zero only for an
// enabled unit and prints the state either way, so a non-zero status alone is
// not an unreadable supervisor: the printed state decides whether the unit is
// masked, and a failed read is otherwise reported as not enabled; a repeat
// installation then leaves the autostart state alone, and a supervisor that
// cannot be reached still fails the installation at the reload.
func (m *systemdManager) registrationEnablement(ctx context.Context) registrationEnablement {
	out, err := m.runSystemctl(ctx, "is-enabled", unitName)
	state := strings.TrimSpace(out)
	facts := registrationEnablement{
		masked: state == "masked" || state == "masked-runtime",
	}
	if err != nil {
		return facts
	}
	facts.enabled = true
	return facts
}

// maskedUnitRefusal returns the error of an installation over a masked unit.
//
// The installation never replaces the mask: writing the unit file would unmask
// the unit silently, so the administrator who masked it would lose that
// decision without noticing. The error names the unit, the refusal and the
// command that unmask it, with the scope prefix the manager uses for systemctl.
func (m *systemdManager) maskedUnitRefusal() error {
	return fmt.Errorf("service: the unit %s is masked: an installation does not replace the mask. Unmask it with %q and run the installation again", unitName, m.supervisorHint()+" unmask "+unitName)
}

// register writes the registration of the instance and hands it to the
// supervisor.
//
// The directories of the instance are already created when the function is
// called. A failure of the registration is rolled back: the unit file is
// restored and the directories the installation created are removed by the
// caller, so a failed installation leaves no half configured unit and no
// empty directories behind.
//
// The unit is enabled for boot only when the installation has to enable it
// (see reloadAndEnable).
//
// A replaced registration that was running is restarted and verified; a failure
// of that restart or of the verification restores the previous registration and
// brings the previous instance back into service. The restore reports a started
// instance only when the supervisor was observed running it.
func (m *systemdManager) register(ctx context.Context, opts InstallOptions, account *user.User, created map[string][]string, previous previousRegistration) error {
	// The instance runs as the service account, so the directories it owns
	// belong to it.
	if err := m.grantInstanceDirs(opts, account, created); err != nil {
		return err
	}
	if err := m.preflight(opts); err != nil {
		return err
	}

	unit, err := renderUnit(opts, account)
	if err != nil {
		return err
	}

	if err := writeUnitFile(previous.path, unit); err != nil {
		return err
	}
	// A first installation enables the unit for boot; a repeat installation
	// keeps the autostart state it finds.
	enable := !previous.existed || previous.enabled
	if err := m.reloadAndEnable(ctx, enable); err != nil {
		m.rollbackUnit(ctx, previous.path, previous.content, previous.existed)
		return err
	}
	if !previous.running {
		// A first installation and a stopped instance stay stopped:
		// starting is an explicit, separate command.
		return nil
	}
	if err := m.restartAndVerify(ctx, opts.Executable, parseUnit(unit).listen()); err != nil {
		if restoreErr := m.restorePreviousRegistration(ctx, previous); restoreErr != nil {
			return fmt.Errorf("service: the installation did not put the new executable into service: %w (the previous registration could not be put back into service: %v)", err, restoreErr)
		}
		return fmt.Errorf("service: the installation did not put the new executable into service: %w (the previous registration was restored and the previous instance was started again)", err)
	}
	return nil
}

// restorePreviousRegistration restores the registration an installation
// replaced and the running state it had.
//
// The unit file is restored and the supervisor is reloaded first, so the
// restart below starts the previous program, and the restored file is read back
// before the restart: a caller is only told that the registration was restored
// when the file really holds it again.
//
// A restart that the supervisor accepted is not a running instance: a
// Type=simple unit is reported restarted as soon as the process was forked, so
// the running state and the readiness are read back, and an unconfirmed restart
// is an error that says exactly that. The restore never replaces the original
// failure.
func (m *systemdManager) restorePreviousRegistration(ctx context.Context, previous previousRegistration) error {
	// The caller may have cancelled the context, the previous registration
	// still has to be put back into service.
	cleanup := context.WithoutCancel(ctx)
	m.rollbackUnit(cleanup, previous.path, previous.content, previous.existed)
	if err := verifyRestoredUnit(previous); err != nil {
		return err
	}
	if !previous.running {
		return nil
	}
	if _, err := m.runSystemctl(cleanup, "restart", unitName); err != nil {
		return fmt.Errorf("the registration was restored, but the restart of the previous instance was refused: %w", err)
	}
	// The restored unit names the program the previous instance ran; a unit
	// whose command line cannot be read names no program, so only the running
	// state and the probe of its bind address are verified then.
	restored := parseUnit(string(previous.content))
	if err := m.verifyRunningExecutable(cleanup, restored.ExecStart, restored.listen()); err != nil {
		return fmt.Errorf("the registration was restored and the restart of the previous instance was issued, but the restart was not confirmed: %w", err)
	}
	return nil
}

// verifyRestoredUnit checks that the unit file holds the previous registration
// again: the rollback writes and reloads best effort, so the file is read back
// instead of assumed.
func verifyRestoredUnit(previous previousRegistration) error {
	content, existed, err := readUnitFile(previous.path)
	if err != nil {
		return fmt.Errorf("the restored unit file could not be read back: %w", err)
	}
	switch {
	case !previous.existed && existed:
		return fmt.Errorf("the unit file of the failed installation is still there: %s", previous.path)
	case previous.existed && !existed:
		return fmt.Errorf("the unit file %s is gone", previous.path)
	case previous.existed && !bytes.Equal(content, previous.content):
		return fmt.Errorf("the unit file %s does not hold the previous registration", previous.path)
	}
	return nil
}

// restartAndVerify restarts the unit and verifies that the supervisor runs the
// new executable and that the instance serves on the listen address of the
// unit. The restart is what switches the running process, which keeps the image
// it was started from; the listen address comes from the unit that was written.
func (m *systemdManager) restartAndVerify(ctx context.Context, executable, listen string) error {
	if _, err := m.runSystemctl(ctx, "restart", unitName); err != nil {
		return fmt.Errorf("service: restart %s: %w", unitName, err)
	}
	return m.verifyRunningExecutable(ctx, executable, listen)
}

// restartPollInterval is the interval between two checks whether a restarted
// instance runs the new executable.
const restartPollInterval = 100 * time.Millisecond

// restartVerifyWait returns the bounded wait for a restarted instance: the
// shutdown budget, because the new program cannot start before the old instance
// finished its orderly shutdown.
func (m *systemdManager) restartVerifyWait() time.Duration {
	if m.restartWait > 0 {
		return m.restartWait
	}
	return ShutdownBudget
}

// verifyRunningExecutable waits until the supervisor reports the unit as
// running with the given program and the instance answers on the listen address
// of the unit.
//
// systemctl show reports the configuration of the loaded unit, not the image of
// the process that runs it, so a registration that points at the new executable
// proves nothing while the old process still runs. The unit is of type simple,
// so the supervisor reports it active as soon as the process was forked, and a
// process that dies right after is active for a moment: the readiness probe
// therefore has to answer as well. A probe that does not answer within the wait
// fails the verification like a state that never becomes running, so the caller
// rolls the installation back instead of reporting an update that left no
// server behind.
//
// An empty executable checks the running state and the probe alone, and the
// wait is bounded by the shutdown budget and the context of the installation.
func (m *systemdManager) verifyRunningExecutable(ctx context.Context, executable, listen string) error {
	budget := m.restartVerifyWait()
	deadline := time.Now().Add(budget)
	address := probeServiceAddress(listen)
	detail := "the supervisor did not report a state"
	for {
		probeFailed := false
		out, err := m.runSystemctl(ctx, "show", unitName,
			"--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=ExecStart")
		if err != nil {
			detail = err.Error()
		} else {
			show := parseSystemdShow(out)
			path := execStartPath(out)
			programMatches := show.State == StateRunning && (executable == "" || path == executable)
			switch {
			case !programMatches && executable == "":
				detail = fmt.Sprintf("the supervisor reports the state %q", show.State)
			case !programMatches:
				detail = fmt.Sprintf("the supervisor reports the state %q with the program %q", show.State, path)
			default:
				// The supervisor reports the instance the registration
				// names; the instance itself has to answer before the
				// start counts.
				if probeErr := serviceReadinessProbe(listen); probeErr != nil {
					probeFailed = true
					if executable == "" {
						detail = fmt.Sprintf("the supervisor reports the state %q, but the instance does not answer on %s: %v", show.State, address, probeErr)
					} else {
						detail = fmt.Sprintf("the supervisor reports the state %q with the program %q, but the instance does not answer on %s: %v", show.State, path, address, probeErr)
					}
				} else {
					return nil
				}
			}
		}
		if !time.Now().Before(deadline) {
			switch {
			case probeFailed:
				return fmt.Errorf("the restarted %s does not answer on %s within %s: %s", unitName, address, budget, detail)
			case executable == "":
				return fmt.Errorf("the %s is not active within %s: %s", unitName, budget, detail)
			default:
				return fmt.Errorf("the restarted %s does not run %q within %s: %s", unitName, executable, budget, detail)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(restartPollInterval):
		}
	}
}

// execStartPath returns the program path of the ExecStart property, which the
// supervisor reports as a structure:
//
//	ExecStart={ path=/usr/local/bin/hfdesk ; argv[]=/usr/local/bin/hfdesk ... }
//
// The "path=" field is the program that really executes; an absent or empty
// property yields an empty string.
func execStartPath(output string) string {
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(key) != "ExecStart" {
			continue
		}
		_, rest, ok := strings.Cut(strings.TrimSpace(value), "path=")
		if !ok {
			return ""
		}
		for _, end := range []string{" ; argv[]=", "; argv[]=", " ;", " }"} {
			if i := strings.Index(rest, end); i >= 0 {
				return trimExecStartPath(rest[:i])
			}
		}
		return trimExecStartPath(strings.TrimSuffix(rest, "}"))
	}
	return ""
}

// trimExecStartPath returns the program path of a property value without the
// quoting a path with spaces carries.
func trimExecStartPath(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
}

// preflight rejects an installation the supervisor could not run: a missing
// supervisor, an unusable bind address or an executable that cannot be run.
func (m *systemdManager) preflight(opts InstallOptions) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("service: systemctl is not available: %w", err)
	}
	if _, _, err := ParseListen(opts.Listen); err != nil {
		return err
	}
	info, err := os.Stat(opts.Executable)
	if err != nil {
		return fmt.Errorf("service: executable %q is not usable: %w", opts.Executable, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("service: executable %q is not a regular file", opts.Executable)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("service: executable %q is not executable", opts.Executable)
	}
	return nil
}

// checkSystemPaths rejects a machine-wide installation that would adopt the
// state of the user who runs the install command.
//
// A system scope unit runs as root. A path inside the home directory of the
// invoking user would either expose the state of the user to the machine or
// take the state of the user away from it, so the installation is refused
// instead of silently taking the state over. The model and the cache trees
// are never touched by an installation: only the three directories of the
// instance (the config directory, the state directory and the log directory)
// are created, and nothing is ever chowned.
//
// A user scope instance is supposed to live inside the directories of its own
// user, so the guard does not touch it.
//
// The comparison is lexical: the paths are compared as they are written and
// the symbolic links of the file system are not resolved. A path that only
// reaches the home directory of the user through a symbolic link is
// therefore compared by its own path and not caught. The lexical test is the
// conservative choice: it never resolves a link of an unprivileged user into
// a machine-wide instance, and it reports the paths of the command line as
// they were typed.
func (m *systemdManager) checkSystemPaths(opts InstallOptions) error {
	if m.scope != ScopeSystem {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
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
		// The containment helper of the downloader package is a lexical
		// test: it counts the home directory itself as inside it and it
		// keeps a sibling that shares the prefix of the home ("/home/user2"
		// against "/home/user") outside it.
		if hfdownloader.PathInside(home, p.path) {
			return fmt.Errorf("service: the %s %q of a machine-wide instance must not live inside the home directory %q of the invoking user", p.name, p.path, home)
		}
	}
	return nil
}

// reloadAndEnable makes the supervisor aware of the unit file and enables the
// unit for the boot of the scope when the installation has to. The reload always
// happens; the enable is only asked for a first installation or a registration
// that was enabled before, so a reinstall never re-enables a unit an
// administrator disabled. A masked unit is refused before this point, so it
// never reaches the reload.
func (m *systemdManager) reloadAndEnable(ctx context.Context, enable bool) error {
	if _, err := m.runSystemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if !enable {
		return nil
	}
	if _, err := m.runSystemctl(ctx, "enable", unitName); err != nil {
		return err
	}
	return nil
}

// rollbackUnit restores the registration of a failed installation: the
// previous unit file is written back, or the unit file of the failed
// installation is removed when there was none. The supervisor is reloaded
// afterwards, so it never keeps a stale unit.
//
// The rollback is best effort: the original failure of the installation is
// the error the caller gets.
func (m *systemdManager) rollbackUnit(ctx context.Context, path string, previous []byte, existed bool) {
	// The caller may have cancelled the context, the rollback still has to
	// clean the registration up.
	cleanup := context.WithoutCancel(ctx)
	if existed {
		_ = writeUnitFile(path, string(previous))
	} else {
		_ = os.Remove(path)
	}
	_, _ = m.runSystemctl(cleanup, "daemon-reload")
}

// Uninstall removes the registration of the service.
//
// It removes the registration only: the config file, the job history, the
// logs, the models and the cache of the instance are never touched. An
// absent registration is a clean success and the supervisor is left alone.
func (m *systemdManager) Uninstall(ctx context.Context) error {
	path, err := m.unitPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := m.requireRoot("an uninstallation"); err != nil {
		return err
	}

	// Stop the unit and drop it from the boot of the scope. A unit the
	// supervisor does not know is not an error: the registration is
	// already gone.
	if _, err := m.runSystemctl(ctx, "disable", "--now", unitName); err != nil {
		if !isUnitAbsent(err) {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("service: remove the unit file %q: %w", path, err)
	}
	if _, err := m.runSystemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	return nil
}

// lifecycleKind tells the guard of a lifecycle command what the command
// needs from the registration of the service.
type lifecycleKind int

const (
	// lifecycleStart is a command that brings the service up, a start or a
	// restart: it needs a registration, so an absent one is a refusal.
	lifecycleStart lifecycleKind = iota
	// lifecycleStop is a command that stops the service: a service that is
	// not registered does not run either, so an absent registration is a
	// stable success.
	lifecycleStop
)

// lifecycleOutcome is the answer of the guard of a lifecycle command.
type lifecycleOutcome int

const (
	// lifecycleRun means the command may touch the supervisor.
	lifecycleRun lifecycleOutcome = iota
	// lifecycleAlreadyReached means the wanted state of the command holds
	// already: the service is not registered, so it does not run either,
	// and the command is a stable success that must not touch the
	// supervisor.
	lifecycleAlreadyReached
)

// Start starts the registered unit. Starting a unit that already runs is a
// stable success, not an error.
//
// A unit that is not registered cannot be started: the command reports that
// the service is not installed.
func (m *systemdManager) Start(ctx context.Context) error {
	installed, err := m.installed()
	if err != nil {
		return err
	}
	if _, err := m.requireLifecycle(lifecycleStart, installed, os.Geteuid(), "a start"); err != nil {
		return err
	}
	if _, err := m.runSystemctl(ctx, "start", unitName); err != nil {
		// The supervisor may refuse a start of a unit that is already
		// active. That is the state the caller asked for.
		if state, ok := m.currentState(ctx); ok && (state == StateRunning || state == StateStarting) {
			return nil
		}
		return err
	}
	return nil
}

// Stop stops the registered unit. Stopping a unit that is already stopped is
// a stable success, not an error, and so is stopping a unit that is not
// registered at all: it does not run either.
func (m *systemdManager) Stop(ctx context.Context) error {
	installed, err := m.installed()
	if err != nil {
		return err
	}
	outcome, err := m.requireLifecycle(lifecycleStop, installed, os.Geteuid(), "a stop")
	if err != nil {
		return err
	}
	if outcome == lifecycleAlreadyReached {
		// The unit is not registered, so it does not run either: the
		// state the caller asked for already holds.
		return nil
	}
	if _, err := m.runSystemctl(ctx, "stop", unitName); err != nil {
		// A unit the supervisor does not know is not running, so the
		// wanted state is already reached.
		if isUnitAbsent(err) {
			return nil
		}
		if state, ok := m.currentState(ctx); ok && state == StateStopped {
			return nil
		}
		return err
	}
	return nil
}

// Restart stops and starts the registered unit.
//
// A unit that is not registered cannot be restarted: the command reports
// that the service is not installed.
func (m *systemdManager) Restart(ctx context.Context) error {
	installed, err := m.installed()
	if err != nil {
		return err
	}
	if _, err := m.requireLifecycle(lifecycleStart, installed, os.Geteuid(), "a restart"); err != nil {
		return err
	}
	if _, err := m.runSystemctl(ctx, "restart", unitName); err != nil {
		return err
	}
	return nil
}

// requireLifecycle reports what a lifecycle command of the manager may do.
//
// The registration of the service comes first: it needs no privilege and it
// gives the more useful answer. A stop of a service that is not registered
// reaches its wanted state already, because such a service does not run
// either.
//
// The elevation requirement comes second: a machine wide unit belongs to
// the machine, so a lifecycle command of that scope needs root. A user scope
// instance belongs to the user of the session and needs no elevation.
//
// The state of the registration and the effective user id are parameters,
// so the decision can be driven from both sides without an elevated shell:
// the command passes the state of the file system and the id of the process,
// and the test passes the states of the cases.
func (m *systemdManager) requireLifecycle(kind lifecycleKind, installed bool, uid int, action string) (lifecycleOutcome, error) {
	if !installed {
		if kind == lifecycleStop {
			return lifecycleAlreadyReached, nil
		}
		return lifecycleRun, fmt.Errorf("service: %s is not installed", unitName)
	}
	if err := m.requireRootFor(action, uid); err != nil {
		return lifecycleRun, err
	}
	return lifecycleRun, nil
}

// installed reports whether the unit file of the manager scope is there.
//
// The registration is read from the file system, so the answer needs no
// privilege: a caller that is not elevated still learns whether the
// service is registered.
func (m *systemdManager) installed() (bool, error) {
	path, err := m.unitPath()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// currentState queries the supervisor for the state of the unit. The second
// result is false when the supervisor could not be reached.
func (m *systemdManager) currentState(ctx context.Context) (State, bool) {
	out, err := m.runSystemctl(ctx, "show", unitName,
		"--property=LoadState", "--property=ActiveState", "--property=SubState")
	if err != nil {
		return StateUnknown, false
	}
	return parseSystemdShow(out).State, true
}

// Status reports the state of the registered service.
//
// The registration is described by the unit file: the executable, the bind
// address and the runtime paths come from the registered unit, not from the
// current intent of the caller. A registered executable that is gone is
// reported as broken, so a moved or deleted binary is never reported as a
// running service.
//
// The name of the status is the label of the service ("hfdesk"), not the
// name of the unit file ("hfdesk.service"): the label is the identity the
// user knows the service by.
func (m *systemdManager) Status(ctx context.Context) (Status, error) {
	path, err := m.unitPath()
	if err != nil {
		return Status{}, err
	}
	status := Status{
		Scope:    m.scope,
		Name:     ServiceName,
		UnitPath: path,
	}

	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			status.State = StateNotInstalled
			status.Detail = fmt.Sprintf("the unit file does not exist: %s", path)
			return status, nil
		}
		return Status{}, err
	}

	unit := parseUnit(string(content))
	status.Executable = unit.ExecStart
	status.Listen = unit.listen()
	status.Paths = unit.paths()
	// The cache root of the instance is the cache dir of the config file
	// of the instance when that file defines one, and the registered cache
	// dir otherwise: the status describes the instance that runs, not the
	// registration alone. An instance that defines no cache root at all
	// reports none.
	status.CacheDir = EffectiveCacheDir(unit.flagValue("--config"), unit.flagValue("--cache-dir"))

	if broken, detail := brokenExecutable(status.Executable); broken {
		status.State = StateBroken
		status.Detail = detail
		return status, nil
	}

	out, err := m.runSystemctl(ctx, "show", unitName,
		"--property=LoadState", "--property=ActiveState", "--property=SubState")
	if err != nil {
		status.State = StateUnknown
		status.Detail = err.Error()
		return status, err
	}
	show := parseSystemdShow(out)
	status.State = show.State
	switch status.State {
	case StateNotInstalled:
		status.Detail = "the supervisor does not know the unit, reload the supervisor to pick the unit file up"
	case StateFailed:
		status.Detail = fmt.Sprintf("the unit failed, see `%s status %s`", m.supervisorHint(), unitName)
	}
	return status, nil
}

// brokenExecutable reports whether a registration that points at the given
// executable is broken, and the detail that names the missing program.
//
// A registration without an executable is not broken: there is no program
// that could be missing. A registered program that cannot be reached is
// broken: the instance can never run again until the program is installed
// again.
//
// The decision is pure and separate from the supervisor: a broken
// registration is described by the unit file alone, so the status of a
// broken registration never depends on a running supervisor.
func brokenExecutable(executable string) (bool, string) {
	if executable == "" {
		return false, ""
	}
	if _, err := os.Stat(executable); err != nil {
		return true, fmt.Sprintf("registered executable not found: %s", executable)
	}
	return false, ""
}

// isUnitAbsent reports whether a supervisor error means that the unit is
// not known to the supervisor. Such an error is not a failure of the
// operation: the wanted state (no unit) is already reached.
func isUnitAbsent(err error) bool {
	var svcErr *systemctlError
	if !errors.As(err, &svcErr) {
		return false
	}
	message := strings.ToLower(svcErr.stderr)
	for _, marker := range []string{
		"not loaded",
		"does not exist",
		"no such file",
		"not found",
		"unknown unit",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// readUnitFile reads the current content of a unit file. The second result
// reports whether the file existed at all.
func readUnitFile(path string) ([]byte, bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return content, true, nil
}

// writeUnitFile writes a unit file atomically: the content goes into a
// temporary file of the same directory and is renamed over the target. A
// reader of the unit file therefore never sees a half written unit.
func writeUnitFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, unitDirMode); err != nil {
		return fmt.Errorf("service: create the unit directory %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".hfdesk-unit-*")
	if err != nil {
		return fmt.Errorf("service: create a temporary unit file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// The temporary file only survives when it was not renamed.
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("service: write the unit file %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("service: close the unit file %q: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, unitFileMode); err != nil {
		return fmt.Errorf("service: set the mode of the unit file %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("service: install the unit file %q: %w", path, err)
	}
	return nil
}

// unitTemplate is the shape of a HFDesk unit.
//
// The unit keeps the supervisor in charge: HFDesk stays a normal foreground
// process, the unit is a simple service and the process is never forked,
// detached or put into the background by HFDesk itself.
//
// The hardening set is deliberately small. HFDesk reads and writes the
// models, the cache and the state of the instance in directories that depend
// on the installation, so a sandbox that hides the file system or the home
// directory of the user would break the instance instead of protecting it.
// Every further restriction would need the explicit list of the paths the
// instance may write, and that list depends on the installation.
//
// The unit does not redirect the output: HFDesk writes its own bounded
// rotating log file into the log directory of the instance and the supervisor
// keeps the standard output and the standard error of the process in its
// journal.
//
// The identity of a machine-wide unit is the dedicated service account of
// the platform. A user scope unit names no account: it runs as the user of
// the session.
const unitTemplate = `# HFDesk service unit.
#
# Generated by the hfdesk service install command; a later installation
# overwrites this file.
#
# The unit holds no access data: an instance reads its own settings from its
# config file, which only the owner of the instance can read. The unit
# therefore carries the executable and the canonical arguments of the
# process only.

[Unit]
Description=HFDesk
Documentation=https://github.com/bashrusakh/hfdesk
After=network-online.target
Wants=network-online.target

[Service]
# The supervisor owns the lifecycle of the instance: HFDesk stays a normal
# foreground process and the supervisor restarts it when it fails.
Type=simple
%sExecStart=%s
WorkingDirectory=%s
Restart=on-failure
RestartSec=5s
# The supervisor waits for the graceful shutdown of the instance before it
# kills it. The wait covers the whole orderly shutdown of the server, so a
# stop never interrupts an instance that is still saving its state.
TimeoutStopSec=%s
NoNewPrivileges=true
UMask=0077
Environment=%s

[Install]
WantedBy=%s
`

// renderUnit renders the systemd unit of an installation.
//
// The function renders the text of the unit and never changes anything on
// the machine. The command of the unit is the canonical argument vector of
// the shared contract, so every platform registers the same command.
//
// The account is the service account the installation resolved for a
// machine-wide instance. It is nil for a user scope instance, which runs as
// the user of the session.
func renderUnit(opts InstallOptions, account *user.User) (string, error) {
	home, err := unitHomeDir(opts)
	if err != nil {
		return "", err
	}
	identity, err := unitIdentity(opts, account)
	if err != nil {
		return "", err
	}

	words := make([]string, 0, len(RunArgs(opts))+1)
	words = append(words, systemdQuoteCommandAlways(opts.Executable))
	for _, arg := range RunArgs(opts) {
		words = append(words, systemdQuoteCommand(arg))
	}

	target := "default.target"
	if opts.Scope == ScopeSystem {
		target = "multi-user.target"
	}

	return fmt.Sprintf(unitTemplate,
		identity,
		strings.Join(words, " "),
		systemdQuote(opts.Paths.StateDir),
		systemdStopTimeout(),
		systemdQuote("HOME="+home),
		target,
	), nil
}

// validateUnitPaths rejects an installation whose paths carry a control
// character.
//
// A unit file is a line based file: a control character in a path would end
// up in it literally, and a newline would end the directive it is written in
// and start another one the installation never wrote.
//
// The check covers every path that ends up in the unit: the executable, the
// paths of the instance and the directories the user named.
func validateUnitPaths(opts InstallOptions) error {
	for _, p := range []struct {
		name string
		path string
	}{
		{"executable", opts.Executable},
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
		if r, ok := controlCharacter(p.path); ok {
			return fmt.Errorf("service: the %s %q contains the control character %q: a path of an installation must not contain a control character", p.name, p.path, r)
		}
	}
	return nil
}

// controlCharacter returns the first control character of the value.
//
// A control character is a character of the C0 control set: the unit file is
// a line based file, so a newline in a path would end the directive it is
// written in.
func controlCharacter(value string) (rune, bool) {
	for _, r := range value {
		if r < 0x20 {
			return r, true
		}
	}
	return 0, false
}

// systemdStopTimeout returns the value of the TimeoutStopSec directive of a
// unit.
//
// The directive is the deadline of the supervisor: systemd asks the instance
// to stop and kills it when the deadline is over. The deadline is the shared
// budget of the service layer, so it covers the whole orderly shutdown of
// the server and can never be shorter than it.
//
// The value is written as a duration, the form systemd reads in every unit.
func systemdStopTimeout() string {
	return ShutdownBudget.String()
}

// unitIdentity renders the identity directives of a unit.
//
// A machine-wide instance runs as the dedicated service account of the
// platform: the unit names the account and its group, so the supervisor
// starts the instance with the least privilege it needs and not with the
// identity of the administrator who installed it.
//
// The group of the unit is the primary group of the account, never the name
// of the account: an account whose primary group is named after it only by
// accident (a wheel group, a shared group, a group of a directory server)
// would otherwise name a group that does not exist, and the unit would fail
// to start with a group error the installation never sees.
//
// A group that cannot be resolved is left out of the unit: the supervisor
// then uses the default group of the account, which is the primary group of
// the account itself. A name is never guessed.
//
// A user scope instance runs as the user of the session, so it names no
// account at all.
func unitIdentity(opts InstallOptions, account *user.User) (string, error) {
	if opts.Scope != ScopeSystem {
		return "", nil
	}
	name := serviceAccountName(opts)
	if name == "" {
		return "", errors.New("service: the machine-wide instance needs a service account")
	}
	identity := "User=" + systemdQuote(name) + "\n"
	if account == nil {
		// The account of the instance is unknown: the unit names the
		// account of the command line and no group at all.
		return identity, nil
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil || group.Name == "" {
		// The primary group of the account cannot be resolved: the
		// unit names no group, and the supervisor uses the default
		// group of the account.
		return identity, nil
	}
	return identity + "Group=" + systemdQuote(group.Name) + "\n", nil
}

// unitHomeDir returns the HOME of the registered process.
//
// A user scope instance runs as the user of the session, so it keeps the
// home of that user. A machine wide instance never inherits the home of the
// user who runs the install command: it uses the machine level state
// directory as its home.
func unitHomeDir(opts InstallOptions) (string, error) {
	if opts.Scope == ScopeSystem {
		return systemServiceHome, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("service: the home directory of the user is unknown: %w", err)
	}
	if home == "" {
		return "", errors.New("service: the home directory of the user is unknown")
	}
	return home, nil
}

// systemdQuote renders one value of a unit file. The value is quoted only
// when it needs it, so a plain path stays readable.
//
// The value is a setting of the unit and not a part of its command line:
// the supervisor expands the environment of the unit in a command line only,
// so a dollar sign of such a value is a literal one.
func systemdQuote(value string) string {
	if value == "" {
		return `""`
	}
	if !strings.ContainsAny(value, " \t\"'\\%") {
		return value
	}
	return systemdQuoteAlways(value)
}

// systemdQuoteAlways renders one value of a unit file in the C style
// quoting of systemd, no matter what the value contains.
func systemdQuoteAlways(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '%':
			// A percent sign starts a specifier of the supervisor, a
			// doubled one is a literal percent sign.
			b.WriteString("%%")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// systemdQuoteCommand renders one word of the command line of a unit file.
//
// The command line is the one place where the supervisor expands the
// environment of the unit: a dollar sign starts a reference to a variable of
// the unit, and a doubled dollar sign is the documented escape of a literal
// dollar sign. Every dollar sign of the word is therefore doubled, so the
// process sees the path as it was written.
//
// The word is quoted only when it needs it, so a plain path stays readable.
func systemdQuoteCommand(value string) string {
	if value == "" {
		return `""`
	}
	if !strings.ContainsAny(value, " \t\"'\\%$") {
		return value
	}
	return systemdQuoteCommandAlways(value)
}

// systemdQuoteCommandAlways renders one word of the command line of a unit
// file in the C style quoting of systemd, no matter what the word contains.
//
// The dollar sign of the word is doubled: the supervisor expands the
// environment of the unit in the command line, and a doubled dollar sign is
// the documented escape of a literal dollar sign.
func systemdQuoteCommandAlways(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '%':
			// A percent sign starts a specifier of the supervisor, a
			// doubled one is a literal percent sign.
			b.WriteString("%%")
		case '$':
			// A dollar sign starts a variable of the unit, a doubled
			// one is a literal dollar sign.
			b.WriteString("$$")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// parsedUnit holds the registration facts of a unit file.
type parsedUnit struct {
	// ExecStart is the executable of the unit.
	ExecStart string
	// ExecArgs are the arguments of the executable.
	ExecArgs []string
	// WorkingDirectory is the working directory of the unit.
	WorkingDirectory string
	// User is the account a machine-wide unit runs as. A user scope unit
	// names no account, so the value is empty there.
	User string
	// Environment holds the environment variables of the unit.
	Environment map[string]string
}

// listen returns the bind address of the registered instance.
func (u parsedUnit) listen() string {
	return u.flagValue("--listen")
}

// paths returns the runtime paths of the registered instance.
//
// The values come from the registered unit file, so the status describes
// the registration and not the current intent of the caller.
func (u parsedUnit) paths() runtimepaths.Paths {
	paths := runtimepaths.Paths{
		ConfigFile: u.flagValue("--config"),
		StateDir:   u.flagValue("--state-dir"),
		LogDir:     u.flagValue("--log-dir"),
	}
	if paths.StateDir == "" {
		paths.StateDir = u.WorkingDirectory
	}
	return paths
}

// flagValue returns the value of a flag of the argument vector of the unit.
func (u parsedUnit) flagValue(flag string) string {
	for i := 0; i+1 < len(u.ExecArgs); i++ {
		if u.ExecArgs[i] == flag {
			return u.ExecArgs[i+1]
		}
	}
	return ""
}

// parseUnit reads the registration facts of a unit file.
//
// The parser is tolerant: a unit file that was edited by hand is described
// as far as it can be read, so the status of the registration stays
// meaningful.
func parseUnit(content string) parsedUnit {
	unit := parsedUnit{Environment: map[string]string{}}
	section := ""
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.ToLower(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		if section != "service" {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ExecStart":
			words := resolveSystemdDollars(splitSystemdWords(value))
			if len(words) == 0 {
				continue
			}
			unit.ExecStart = words[0]
			unit.ExecArgs = words[1:]
		case "WorkingDirectory":
			if words := splitSystemdWords(value); len(words) > 0 {
				unit.WorkingDirectory = words[0]
			}
		case "User":
			if words := splitSystemdWords(value); len(words) > 0 {
				unit.User = words[0]
			}
		case "Environment":
			for _, word := range splitSystemdWords(value) {
				name, envValue, ok := strings.Cut(word, "=")
				if !ok {
					continue
				}
				unit.Environment[name] = envValue
			}
		}
	}
	return unit
}

// splitSystemdWords splits a command line of a unit file into its words.
// It understands the quoting of the supervisor: single quotes, double quotes
// and backslash escapes.
func splitSystemdWords(line string) []string {
	var (
		words []string
		word  strings.Builder
		has   bool
	)
	flush := func() {
		if !has {
			return
		}
		words = append(words, word.String())
		word.Reset()
		has = false
	}
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		case c == '\'':
			has = true
			end := strings.IndexByte(line[i+1:], '\'')
			if end < 0 {
				word.WriteString(line[i+1:])
				i = len(line)
				continue
			}
			word.WriteString(line[i+1 : i+1+end])
			i = i + 1 + end
		case c == '"':
			has = true
			for i++; i < len(line); i++ {
				if line[i] == '\\' && i+1 < len(line) {
					word.WriteString(unescapeSystemdChar(line[i+1]))
					i++
					continue
				}
				if line[i] == '%' && i+1 < len(line) && line[i+1] == '%' {
					word.WriteByte('%')
					i++
					continue
				}
				if line[i] == '"' {
					break
				}
				word.WriteByte(line[i])
			}
		case c == '\\':
			has = true
			if i+1 < len(line) {
				word.WriteString(unescapeSystemdChar(line[i+1]))
				i++
			}
		case c == '%':
			has = true
			if i+1 < len(line) && line[i+1] == '%' {
				word.WriteByte('%')
				i++
				continue
			}
			word.WriteByte('%')
		default:
			has = true
			word.WriteByte(c)
		}
	}
	flush()
	return words
}

// resolveSystemdDollars resolves the escaped dollar signs of the words of a
// command line.
//
// The supervisor expands the environment of the unit in a command line, and a
// doubled dollar sign is the documented escape of a literal dollar sign. The
// parser resolves it the way the supervisor does, so the registration of a
// path that holds a dollar sign describes the path the process really sees.
//
// The words of a setting of the unit are not touched: the supervisor does not
// expand the environment of the unit there.
func resolveSystemdDollars(words []string) []string {
	for i, word := range words {
		if !strings.Contains(word, "$$") {
			continue
		}
		words[i] = strings.ReplaceAll(word, "$$", "$")
	}
	return words
}

// unescapeSystemdChar returns the character an escape sequence of a unit
// file stands for.
func unescapeSystemdChar(c byte) string {
	switch c {
	case 'n':
		return "\n"
	case 't':
		return "\t"
	case 'r':
		return "\r"
	case 's':
		return " "
	default:
		return string(c)
	}
}

// systemdShow holds the properties of the unit the supervisor reported.
type systemdShow struct {
	// LoadState tells whether the supervisor knows the unit.
	LoadState string
	// ActiveState is the high level state of the unit.
	ActiveState string
	// SubState is the low level state of the unit.
	SubState string
	// State is the lifecycle state of the unit in the vocabulary of the
	// shared contract.
	State State
}

// parseSystemdShow parses the output of the supervisor and maps the
// reported properties onto the lifecycle states of the shared contract.
func parseSystemdShow(output string) systemdShow {
	show := systemdShow{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "LoadState":
			show.LoadState = strings.TrimSpace(value)
		case "ActiveState":
			show.ActiveState = strings.TrimSpace(value)
		case "SubState":
			show.SubState = strings.TrimSpace(value)
		}
	}
	show.State = show.state()
	return show
}

// state maps the reported properties of the unit onto the lifecycle states
// of the shared contract.
func (s systemdShow) state() State {
	if s.LoadState == "not-found" {
		return StateNotInstalled
	}
	switch s.ActiveState {
	case "active":
		return StateRunning
	case "inactive":
		return StateStopped
	case "activating":
		return StateStarting
	case "deactivating":
		return StateStopping
	case "failed":
		return StateFailed
	}
	return StateUnknown
}
