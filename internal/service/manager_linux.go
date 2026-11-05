// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
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
	// readServiceProperties supplies typed manager data in tests. Production
	// reads the same properties through a private D-Bus connection.
	readServiceProperties func(context.Context, string) (map[string]dbus.Variant, error)
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

// systemdInstallIdentity separates complete access credentials from the
// UID/GID known to own directories the installation creates. Supplementary
// groups affect access, but not ownership.
type systemdInstallIdentity struct {
	accountName string
	access      *accessAccount
	owner       *accessAccount
}

// systemdAccessIdentityForInstall uses the generated identity for a new unit
// and loaded native properties for an existing unit. An absent or unloaded
// manager projection is not replaced with a guess from the unit source.
func systemdAccessIdentityForInstall(scope Scope, requested InstallOptions, existing bool, loaded systemdLoadedConfig, loadedKnown bool, account *user.User) systemdInstallIdentity {
	effective := systemdInstallIdentity{accountName: "the native service identity"}
	if scope == ScopeUser {
		current, err := user.Current()
		if err != nil {
			effective.accountName = "the current user"
			return effective
		}
		identity := currentExecutableAccessIdentity()
		effective.accountName = current.Username
		effective.access = &identity
		effective.owner = &identity
		return effective
	}

	var userName, groupName string
	var supplementary []string
	if !existing {
		if account == nil {
			effective.accountName = "the service account"
			return effective
		}
		userName = account.Username
		groupName = account.Gid
	} else {
		if !loadedKnown || !loaded.loaded {
			return effective
		}
		supplementary = loaded.supplementaryGroups
		if requested.Explicit.ServiceUser {
			if account == nil {
				effective.accountName = "the requested service account"
				return effective
			}
			userName = account.Username
			groupName = account.Gid
		} else {
			userName, groupName = loaded.user, loaded.group
		}
	}
	if userName == "" {
		return effective
	}
	identityAccount, err := systemdAccountByName(userName)
	if err != nil {
		effective.accountName = userName
		return effective
	}
	effective.accountName = identityAccount.Username
	owner, err := linuxSystemdOwnershipIdentity(identityAccount, groupName)
	if err != nil {
		return effective
	}
	effective.owner = &owner
	if existing && !loaded.supplementaryGroupsKnown {
		return effective
	}
	access, err := linuxSystemdAccessIdentity(identityAccount, groupName, supplementary)
	if err == nil {
		effective.access = &access
	}
	return effective
}

func systemdAccountByName(name string) (*user.User, error) {
	if _, err := strconv.Atoi(name); err == nil {
		return user.LookupId(name)
	}
	return user.Lookup(name)
}

func linuxSystemdOwnershipIdentity(account *user.User, groupName string) (accessAccount, error) {
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return accessAccount{}, err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return accessAccount{}, err
	}
	if groupName != "" {
		gid, err = systemdGroupID(groupName)
		if err != nil {
			return accessAccount{}, err
		}
	}
	return accessAccount{UID: uid, GID: gid}, nil
}

func linuxSystemdAccessIdentity(account *user.User, groupName string, supplementary []string) (accessAccount, error) {
	identity, err := linuxSystemdOwnershipIdentity(account, groupName)
	if err != nil {
		return accessAccount{}, err
	}
	groupAccount := *account
	groupAccount.Gid = strconv.Itoa(identity.GID)
	groupIDs, err := groupAccount.GroupIds()
	if err != nil {
		return accessAccount{}, err
	}
	seen := map[int]struct{}{identity.GID: {}}
	addGroup := func(id string) error {
		group, err := systemdGroupID(id)
		if err != nil {
			return err
		}
		if _, ok := seen[group]; !ok {
			seen[group] = struct{}{}
			identity.Groups = append(identity.Groups, group)
		}
		return nil
	}
	identity.Groups = append(identity.Groups, identity.GID)
	for _, id := range groupIDs {
		if err := addGroup(id); err != nil {
			return accessAccount{}, err
		}
	}
	for _, name := range supplementary {
		if err := addGroup(name); err != nil {
			return accessAccount{}, err
		}
	}
	return identity, nil
}

func systemdGroupID(name string) (int, error) {
	if id, err := strconv.Atoi(name); err == nil {
		if id < 0 {
			return 0, fmt.Errorf("negative group id %q", name)
		}
		return id, nil
	}
	group, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(group.Gid)
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
	identity, err := linuxAccessAccount(account)
	if err != nil {
		return err
	}
	return m.checkServiceAccountAccessWithIdentity(opts, account.Username, &identity)
}

func (m *systemdManager) checkServiceAccountAccessWithIdentity(opts InstallOptions, accountName string, identity *accessAccount) error {
	if m.scope != ScopeSystem {
		return nil
	}
	if identity == nil {
		log.Printf("service: custom-directory write access is inconclusive because the effective systemd identity is not established; no denial is inferred")
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
		if err := checkDirWritableByIdentity(dir.path, *identity); err != nil {
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
				accountName, dir.kind, dir.path, "chown "+accountName+" "+dir.path)
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
	return checkDirWritableByIdentity(path, identity)
}

func checkDirWritableByIdentity(path string, identity accessAccount) error {
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

func systemdInstanceDirs(opts InstallOptions, dynamic map[string]bool) []instanceDir {
	dirs := instanceDirs(opts)
	filtered := make([]instanceDir, 0, len(dirs))
	for _, dir := range dirs {
		if dir.name == "config dir" && dynamic["config"] ||
			dir.name == "state dir" && dynamic["state-dir"] ||
			dir.name == "log dir" && dynamic["log-dir"] {
			continue
		}
		filtered = append(filtered, dir)
	}
	return filtered
}

func prepareSystemdInstanceDirs(opts InstallOptions, dirs []instanceDir, dynamic map[string]bool) error {
	if !dynamic["config"] && !dynamic["state-dir"] && !dynamic["log-dir"] {
		return PrepareDirs(opts)
	}
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
		if err := ensureDir(dir.path, dirMode); err != nil {
			return fmt.Errorf("service: %s %q: %w", dir.name, dir.path, err)
		}
	}
	return nil
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
	return createdDirOrderForDirs(instanceDirs(opts), created)
}

func createdDirOrderForDirs(dirs []instanceDir, created map[string][]string) []string {
	var order []string
	seen := make(map[string]bool, len(created))
	for _, dir := range dirs {
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

// systemdCreatedDirsSafeToRemove reports whether a verified rollback leaves no
// prior native paths that can refer to a directory created by this update.
// Dynamic path operands, drop-ins, or environment files keep the answer unknown.
func systemdCreatedDirsSafeToRemove(previous previousRegistration, created map[string][]string) bool {
	var createdPaths []string
	for _, paths := range created {
		createdPaths = append(createdPaths, paths...)
	}
	if len(createdPaths) == 0 || !previous.registrationPresent() {
		return true
	}
	if !previous.loadedKnown || !previous.loaded.loaded {
		return false
	}
	sourceContent, sourceKnown := previous.sourceContent, previous.sourceKnown
	if !sourceKnown && previous.existed {
		sourceContent, sourceKnown = previous.content, true
	}
	if !sourceKnown || !previous.loaded.dropInPathsKnown || len(previous.loaded.dropInPaths) > 0 || systemdUnitHasEnvironmentFiles(sourceContent) || systemdUnitHasUnmodeledLaunchDirective(sourceContent) || len(previous.loaded.environmentFiles) > 0 {
		return false
	}

	unit := parseUnit(string(sourceContent))
	var referenced []string
	for _, flag := range []string{"config", "state-dir", "log-dir", "cache-dir", "hub-dir", "local-dir"} {
		if unit.DynamicRunArgs[flag] {
			return false
		}
		path := unit.flagValue("--" + flag)
		if path != "" {
			if !filepath.IsAbs(path) {
				return false
			}
			referenced = append(referenced, path)
		}
	}
	if unit.WorkingDirectoryDynamic {
		if !filepath.IsAbs(previous.loaded.workingDirectory) {
			return false
		}
		referenced = append(referenced, previous.loaded.workingDirectory)
	} else if unit.WorkingDirectory != "" {
		if !filepath.IsAbs(unit.WorkingDirectory) {
			return false
		}
		referenced = append(referenced, unit.WorkingDirectory)
	}
	if previous.loaded.loaded && previous.loaded.workingDirectory != "" {
		if !filepath.IsAbs(previous.loaded.workingDirectory) {
			return false
		}
		referenced = append(referenced, previous.loaded.workingDirectory)
	}
	for _, value := range unit.Environment {
		if hasSystemdExpansion(value) {
			return false
		}
		if filepath.IsAbs(value) {
			referenced = append(referenced, value)
		}
	}
	for _, entry := range previous.loaded.environment {
		_, value, ok := strings.Cut(entry, "=")
		if !ok || hasSystemdExpansion(value) {
			return false
		}
		if filepath.IsAbs(value) {
			referenced = append(referenced, value)
		}
	}
	for _, createdPath := range createdPaths {
		resolvedCreated, err := resolvePath(createdPath)
		if err != nil {
			return false
		}
		for _, referencedPath := range referenced {
			resolvedReference, err := resolvePath(referencedPath)
			if err != nil {
				return false
			}
			if pathContains(resolvedCreated, resolvedReference) {
				return false
			}
		}
	}
	return true
}

func systemdUnitHasEnvironmentFiles(content []byte) bool {
	section := ""
	for _, logical := range systemdLogicalLines(string(content)) {
		line := strings.TrimSpace(logical.text)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		if section != "service" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "EnvironmentFile" && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func systemdUnitHasUnmodeledLaunchDirective(content []byte) bool {
	section := ""
	for _, logical := range systemdLogicalLines(string(content)) {
		line := strings.TrimSpace(logical.text)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		switch section {
		case "unit":
			switch key {
			case "Description", "Documentation", "After", "Wants":
			default:
				return true
			}
		case "service":
			switch key {
			case "Type", "ExecStart", "WorkingDirectory", "User", "Group", "SupplementaryGroups", "Environment", "EnvironmentFile", "Restart", "RestartSec", "TimeoutStopSec", "NoNewPrivileges", "UMask":
			default:
				// Unmodeled launch directives such as ExecStartPre or
				// filesystem namespaces may still use these directories.
				return true
			}
		}
	}
	return false
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
	return m.grantInstanceDirsForDirs(opts, account, created, instanceDirs(opts))
}

func (m *systemdManager) grantInstanceDirsForDirs(opts InstallOptions, account *user.User, created map[string][]string, dirs []instanceDir) error {
	if m.scope != ScopeSystem {
		return nil
	}
	if account == nil {
		return m.grantInstanceDirsForDirsWithIdentity(opts, systemdInstallIdentity{accountName: "the native service identity"}, created, dirs)
	}
	owner, err := linuxSystemdOwnershipIdentity(account, account.Gid)
	if err != nil {
		return err
	}
	access, accessErr := linuxAccessAccount(account)
	identity := systemdInstallIdentity{accountName: account.Username, owner: &owner}
	if accessErr == nil {
		identity.access = &access
	}
	return m.grantInstanceDirsForDirsWithIdentity(opts, identity, created, dirs)
}

func (m *systemdManager) grantInstanceDirsForDirsWithIdentity(opts InstallOptions, identity systemdInstallIdentity, created map[string][]string, dirs []instanceDir) error {
	if m.scope != ScopeSystem {
		return nil
	}
	needsOwnership := false
	var firstNewDir instanceDir
	for _, dir := range dirs {
		if len(created[dir.path]) > 0 {
			needsOwnership = true
			firstNewDir = dir
			break
		}
	}
	if needsOwnership && identity.owner == nil {
		return fmt.Errorf("service: cannot safely assign ownership of the new %s %q because the effective systemd UID/GID is not established", firstNewDir.name, firstNewDir.path)
	}
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
		if identity.access == nil {
			log.Printf("service: write access for the pre-existing %s %q is inconclusive because the effective systemd groups are not established; no denial is inferred", dir.name, dir.path)
			continue
		}
		if err := checkDirWritableByIdentity(dir.path, *identity.access); err != nil {
			if !errors.Is(err, errNotWritable) {
				return fmt.Errorf("service: the %s %q of the machine-wide instance is not usable: %w", dir.name, dir.path, err)
			}
			return fmt.Errorf("service: the %s %q of the machine-wide instance is not writable by the service account %q: grant it to the account (for example %q) and install again",
				dir.name, dir.path, identity.accountName, "chown "+identity.accountName+" "+dir.path)
		}
	}
	for _, dir := range dirs {
		for _, path := range created[dir.path] {
			if err := os.Chown(path, identity.owner.UID, identity.owner.GID); err != nil {
				return fmt.Errorf("service: grant the %s %q to the effective systemd identity %q (%d:%d): %w", dir.name, path, identity.accountName, identity.owner.UID, identity.owner.GID, err)
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
// repeat installation keeps the autostart state it finds, including a
// runtime-only enablement, so a unit an administrator disabled is never enabled
// again, a unit enabled with `systemctl enable --runtime` stays enabled at
// runtime, and a masked unit refuses the installation instead of being replaced
// (see reloadAndEnable). The mask is asked of the supervisor and not of the file
// system, so a runtime mask, which lives in /run and leaves no file at the unit
// path, refuses the installation as well.
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
// A failure after the unit file was written restores the previous unit and
// verifies the loaded configuration; a failed running restart only restarts
// the previous instance after that verification. Created directories are
// removed only after no-write or rollback evidence shows they are unreferenced.
//
// The returned options are the registration as written, so a caller reports a
// preserved setting instead of the default it was not reset to.
func (m *systemdManager) Install(ctx context.Context, opts InstallOptions) (InstallOptions, error) {
	requested := opts
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
	sourceContent, sourceKnown := content, existed
	sourceUnit := parsedUnit{}
	dynamicFields := map[string]bool{}
	registered := existed
	var initialLoaded systemdLoadedConfig
	initialLoadedKnown := false
	if existed {
		sourceUnit = parseUnit(string(content))
	} else if m.scope != ScopeSystem || os.Geteuid() == 0 {
		// A missing writable override is still an existing registration when
		// systemd loaded a vendor or generated fragment for this name.
		initialLoaded, err = m.registrationUnitConfig(ctx)
		if err != nil {
			return opts, fmt.Errorf("service: determine whether %s already has a loaded registration: %w", unitName, err)
		}
		initialLoadedKnown = true
		if initialLoaded.loaded {
			if !initialLoaded.fragmentPathKnown || !filepath.IsAbs(initialLoaded.fragmentPath) {
				return opts, fmt.Errorf("service: the loaded registration of %s has no readable source fragment; refusing to replace unknown native settings", unitName)
			}
			sourceContent, err = os.ReadFile(initialLoaded.fragmentPath)
			if err != nil {
				return opts, fmt.Errorf("service: read the loaded source fragment %q: %w", initialLoaded.fragmentPath, err)
			}
			sourceKnown = true
			sourceUnit = parseUnit(string(sourceContent))
			registered = true
		}
	}
	if registered {
		opts = mergeInstallOptions(opts, registrationOptions(sourceUnit, opts.Scope), true)
		dynamicFields = systemdDynamicInstallFields(sourceUnit, requested)
	}

	validationOpts := systemdValidationOptions(opts, dynamicFields)
	if err := ValidateInstallOptions(validationOpts); err != nil {
		return opts, err
	}
	// A path with a control character would end up in the unit file
	// literally, and a newline in it would split the unit into another
	// directive the installation never wrote. The check runs before the
	// root requirement, before any directory is created and before the
	// unit file is written: a refused installation leaves nothing behind.
	if err := validateUnitPaths(validationOpts, registered); err != nil {
		return opts, err
	}
	// A machine-wide instance must not adopt the state of the user who
	// runs the install command. The check runs before the root
	// requirement, before any directory is created and before the unit
	// file is written: a refused installation leaves nothing behind, and
	// every platform reports the same reason for the same command line.
	filesystemOpts := systemdFilesystemOptions(opts, dynamicFields)
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
	preflightLoaded := initialLoaded
	preflightLoadedKnown := initialLoadedKnown
	preflightLoadedAttempted := initialLoadedKnown
	if existed && !preflightLoadedAttempted && m.scope == ScopeSystem && os.Geteuid() == 0 {
		preflightLoadedAttempted = true
		if loaded, readErr := m.registrationUnitConfig(ctx); readErr == nil {
			preflightLoaded = loaded
			preflightLoadedKnown = true
		}
	}
	identity := systemdAccessIdentityForInstall(opts.Scope, requested, registered, preflightLoaded, preflightLoadedKnown, account)
	if err := m.checkServiceAccountAccessWithIdentity(filesystemOpts, identity.accountName, identity.access); err != nil {
		return opts, err
	}
	// The root requirement is checked before any directory is created: a
	// permission error of the machine wide directories is a much less
	// readable answer than the real reason.
	if err := m.requireRoot("an installation"); err != nil {
		return opts, err
	}
	if registered && !preflightLoadedAttempted {
		if loaded, readErr := m.registrationUnitConfig(ctx); readErr == nil {
			preflightLoaded = loaded
			preflightLoadedKnown = true
		}
	}
	// The program is checked before anything is read or written, so a refused
	// program creates no directory, rewrites no unit and stops no instance.
	preflightOpts := validationOpts
	if registered && dynamicFields["state-dir"] {
		preflightOpts.Paths.StateDir = ""
	}
	workingDirectory := systemdWorkingDirectoryForPreflight(preflightOpts, registered, preflightLoaded, preflightLoadedKnown)
	if err := m.preflightWithContext(ctx, preflightOpts, identity.accountName, identity.access, workingDirectory); err != nil {
		return opts, err
	}
	// The state of the replaced registration is read before the unit file
	// is written: only a running instance is restarted, a stopped one has
	// to stay stopped.
	previous := previousRegistration{
		path: path, content: content, sourceContent: sourceContent, sourceKnown: sourceKnown,
		existed: existed, loaded: preflightLoaded, loadedKnown: preflightLoadedKnown,
		dynamicFields: dynamicFields,
	}
	if registered {
		running, err := m.registrationRunning(ctx)
		if err != nil {
			return opts, err
		}
		previous.running = running
	}
	// The enablement is read before the unit file is written, whether or not
	// a unit file exists: the mask of a unit is a question for the
	// supervisor, and a runtime mask (`systemctl mask --runtime`) lives in
	// /run and leaves no file at the unit path, so a unit that is only
	// masked at runtime would otherwise look like a first installation and
	// be registered while it stays masked and cannot start.
	//
	// A masked unit is refused instead of silently unmasked, and a repeat
	// installation keeps the autostart state it finds, in its mode: a unit
	// enabled for this boot only stays enabled for this boot only. A fresh
	// machine is unaffected: "is-enabled" answers "not-found", which is not a
	// mask, and a first installation enables the registration it writes
	// anyway.
	enablement := m.registrationEnablement(ctx)
	if enablement.masked {
		return opts, m.maskedUnitRefusal()
	}
	if registered {
		previous.enablement = enablement.mode
		loaded, readErr := m.registrationUnitConfig(ctx)
		if readErr != nil {
			return opts, fmt.Errorf("service: the loaded configuration of %s could not be read, the registration was not changed: %w", unitName, readErr)
		}
		if !existed && loaded.loaded {
			if !loaded.fragmentPathKnown || loaded.fragmentPath != initialLoaded.fragmentPath {
				return opts, fmt.Errorf("service: the source fragment of the loaded %s changed during installation preflight", unitName)
			}
			currentSource, sourceErr := os.ReadFile(loaded.fragmentPath)
			if sourceErr != nil || !bytes.Equal(currentSource, sourceContent) {
				return opts, fmt.Errorf("service: the loaded source fragment %q changed or became unreadable during installation preflight", loaded.fragmentPath)
			}
		}
		if !loaded.loaded && previous.running {
			return opts, errors.New("service: systemd reported a running unit without a loaded configuration; the registration was not changed")
		}
		if loaded.loaded {
			if err := loadedMatchesUnit(loaded, parseUnit(string(sourceContent))); err != nil {
				return opts, fmt.Errorf("service: the loaded command of %s differs from its unit file; an override may own the effective command, refusing to update it: %w", unitName, err)
			}
		}
		previous.loaded = loaded
		previous.loadedKnown = true
		previous.sourceContent = sourceContent
		previous.sourceKnown = sourceKnown
		contextChanged := !preflightLoadedKnown || !sameSystemdLoadedConfig(preflightLoaded, loaded)
		preflightLoaded = loaded
		preflightLoadedKnown = true
		if contextChanged && loaded.loaded {
			identity = systemdAccessIdentityForInstall(opts.Scope, requested, true, loaded, true, account)
			if err := m.checkServiceAccountAccessWithIdentity(filesystemOpts, identity.accountName, identity.access); err != nil {
				return opts, err
			}
			workingDirectory = systemdWorkingDirectoryForPreflight(preflightOpts, true, loaded, true)
			if err := m.preflightWithContext(ctx, preflightOpts, identity.accountName, identity.access, workingDirectory); err != nil {
				return opts, err
			}
		}
	}
	// The directories the installation is about to create are remembered
	// before they are created, so the effective systemd identity is given
	// ownership of exactly those and of nothing else. Existing directories
	// belong to their current owner and are left alone.
	//
	// The list is also what a failed installation removes again, so a
	// failed installation leaves the machine as it was.
	instanceDirectories := systemdInstanceDirs(opts, dynamicFields)
	created := make(map[string][]string, len(instanceDirectories))
	for _, dir := range instanceDirectories {
		created[dir.path] = createdDirs(dir.path)
	}
	if m.scope == ScopeSystem && identity.owner == nil {
		for _, dir := range instanceDirectories {
			if len(created[dir.path]) > 0 {
				return opts, fmt.Errorf("service: cannot assign ownership of the new %s %q because the effective systemd User/Group is not established", dir.name, dir.path)
			}
		}
	}
	if err := prepareSystemdInstanceDirs(opts, instanceDirectories, dynamicFields); err != nil {
		// The preparation may have created some of the directories
		// before it failed. A previous unit may still reference those
		// paths even though this attempt did not write a registration.
		if systemdCreatedDirsSafeToRemove(previous, created) {
			removeCreatedDirs(createdDirOrderForDirs(instanceDirectories, created))
		} else {
			err = fmt.Errorf("%w (installation-created directories were retained because the prior unit may still reference them)", err)
		}
		return opts, err
	}
	// The result of register decides whether a failed attempt may remove
	// these paths; an uncertain or retained registration keeps them.
	registrationResult, err := m.register(ctx, opts, account, identity, created, previous)
	if err != nil {
		if registrationResult.createdDirsSafeToRemove {
			removeCreatedDirs(createdDirOrderForDirs(instanceDirectories, created))
		} else {
			err = fmt.Errorf("%w (installation-created directories were retained because the supervisor could not prove the restored or retained unit no longer references them)", err)
		}
		return opts, err
	}
	opts.VersionVerified = registrationResult.versionVerified
	return opts, nil
}

// previousRegistration is the registration an installation replaces.
type previousRegistration struct {
	// path is the path of the unit file of the scope.
	path string
	// content is the unit file that was there, empty when there was none.
	content []byte
	// sourceContent is the effective fragment on the systemd load path. It may
	// be different from content when no writable override file exists.
	sourceContent []byte
	sourceKnown   bool
	// existed reports whether the scope had a writable unit file at path.
	existed bool
	// running reports whether the instance ran before the installation.
	// Only such an instance is restarted.
	running bool
	// enablement is the autostart state the supervisor reported for the
	// registration, in its mode. Only a registration that existed carries
	// it.
	enablement enablementMode
	// loaded is the effective command and launch configuration reported by
	// systemd before the update. The file on disk alone cannot prove what the
	// supervisor has loaded.
	loaded systemdLoadedConfig
	// loadedKnown reports that loaded was read successfully for an existing
	// registration. A first installation expects no loaded registration.
	loadedKnown bool
	// dynamicFields are untouched ExecStart options whose source uses systemd
	// specifiers or environment references. Their source text is preserved
	// while available manager-reported values are compared with the snapshot.
	dynamicFields map[string]bool
}

func (p previousRegistration) registrationPresent() bool {
	return p.existed || (p.loadedKnown && p.loaded.loaded)
}

func (p previousRegistration) sourceUnitContent() ([]byte, bool) {
	if p.sourceKnown {
		return p.sourceContent, true
	}
	if p.existed {
		return p.content, true
	}
	return nil, false
}

// systemdLoadedConfig is the effective command and the launch properties
// systemd reports for a unit. Dynamic process state is intentionally omitted:
// it is verified separately from the loaded registration.
type systemdLoadedConfig struct {
	loaded                   bool
	execPath                 string
	execArgs                 []string
	execFlags                []string
	ignoreErrors             bool
	workingDirectory         string
	user                     string
	group                    string
	supplementaryGroups      []string
	supplementaryGroupsKnown bool
	environment              []string
	environmentFiles         []systemdEnvironmentFile
	fragmentPath             string
	fragmentPathKnown        bool
	dropInPaths              []string
	dropInPathsKnown         bool
}

type systemdEnvironmentFile struct {
	Path         string
	IgnoreErrors bool
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
	return runningState(state), nil
}

// registrationUnitConfig reads systemd's typed D-Bus properties. systemctl's
// ExecStart display joins argv elements with spaces, so it cannot prove their
// boundaries when a path or argument itself contains whitespace. These are the
// manager's parsed settings before runtime environment expansion, not the
// process argv or an atomic snapshot with service state.
func (m *systemdManager) registrationUnitConfig(ctx context.Context) (systemdLoadedConfig, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := m.runSystemctl(ctx, "show", unitName,
		"--property=LoadState")
	if err != nil {
		return systemdLoadedConfig{}, err
	}
	show := parseSystemdShow(out)
	if show.LoadState == "not-found" {
		return systemdLoadedConfig{}, nil
	}
	if show.LoadState != "loaded" {
		return systemdLoadedConfig{}, fmt.Errorf("systemd reported LoadState=%q", show.LoadState)
	}
	properties, err := m.loadedServiceProperties(ctx, unitName)
	if err != nil {
		return systemdLoadedConfig{}, err
	}
	return loadedConfigFromProperties(properties)
}

func (m *systemdManager) loadedServiceProperties(ctx context.Context, name string) (map[string]dbus.Variant, error) {
	if m.readServiceProperties != nil {
		return m.readServiceProperties(ctx, name)
	}
	conn, err := m.systemdDBusConnection(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: connect to the %s systemd manager: %w", m.scope, err)
	}
	defer conn.Close()

	manager := conn.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1")
	var objectPath dbus.ObjectPath
	if err := manager.CallWithContext(ctx, "org.freedesktop.systemd1.Manager.GetUnit", dbus.FlagNoAutoStart, name).Store(&objectPath); err != nil {
		return nil, fmt.Errorf("service: read the loaded object path of %s: %w", name, err)
	}
	if !objectPath.IsValid() {
		return nil, fmt.Errorf("service: systemd returned an invalid object path for %s", name)
	}
	unit := conn.Object("org.freedesktop.systemd1", objectPath)
	call := unit.CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", dbus.FlagNoAutoStart, "org.freedesktop.systemd1.Service")
	properties, err := dbusPropertiesFromCall(call)
	if err != nil {
		return nil, fmt.Errorf("service: read the loaded properties of %s: %w", name, err)
	}
	// FragmentPath and DropInPaths belong to the base Unit interface, not
	// Service. Missing values keep source/cleanup proof conservative without
	// blocking an otherwise usable registration.
	call = unit.CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", dbus.FlagNoAutoStart, "org.freedesktop.systemd1.Unit")
	if unitProperties, unitErr := dbusPropertiesFromCall(call); unitErr == nil {
		for _, name := range []string{"FragmentPath", "DropInPaths"} {
			if property, ok := unitProperties[name]; ok {
				properties[name] = property
			}
		}
	}
	return properties, nil
}

func dbusPropertiesFromCall(call *dbus.Call) (map[string]dbus.Variant, error) {
	if call == nil {
		return nil, errors.New("systemd returned no D-Bus property reply")
	}
	if call.Err != nil {
		return nil, call.Err
	}
	if len(call.Body) != 1 {
		return nil, fmt.Errorf("systemd returned %d D-Bus property values, want one dictionary", len(call.Body))
	}
	properties, ok := call.Body[0].(map[string]dbus.Variant)
	if !ok {
		return nil, fmt.Errorf("systemd returned D-Bus property body %T, want map[string]dbus.Variant", call.Body[0])
	}
	return properties, nil
}

func (m *systemdManager) systemdDBusConnection(ctx context.Context) (*dbus.Conn, error) {
	switch m.scope {
	case ScopeSystem:
		if os.Geteuid() == 0 {
			conn, privateErr := connectSystemdPrivate(ctx, "/run/systemd/private")
			if privateErr == nil {
				return conn, nil
			}
			conn, busErr := dbus.ConnectSystemBus(dbus.WithContext(ctx))
			if busErr == nil {
				return conn, nil
			}
			return nil, fmt.Errorf("manager-private socket: %v; system bus: %w", privateErr, busErr)
		}
		return dbus.ConnectSystemBus(dbus.WithContext(ctx))

	case ScopeUser:
		if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
			conn, privateErr := connectSystemdPrivate(ctx, filepath.Join(runtimeDir, "systemd", "private"))
			if privateErr == nil {
				return conn, nil
			}
			if address := sessionBusAddress(); address != "" {
				conn, busErr := dbus.Connect(address, dbus.WithContext(ctx))
				if busErr == nil {
					return conn, nil
				}
				return nil, fmt.Errorf("manager-private socket: %v; session bus: %w", privateErr, busErr)
			}
			return nil, privateErr
		}
		if address := sessionBusAddress(); address != "" {
			return dbus.Connect(address, dbus.WithContext(ctx))
		}
		return nil, errors.New("no user manager transport is available from XDG_RUNTIME_DIR or the session bus environment")

	default:
		return nil, fmt.Errorf("unknown systemd scope %q", m.scope)
	}
}

func connectSystemdPrivate(ctx context.Context, socketPath string) (*dbus.Conn, error) {
	stream, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	closeStream := func() { _ = stream.Close() }
	if err := checkSystemdPrivatePeer(stream); err != nil {
		closeStream()
		return nil, err
	}
	conn, err := dbus.NewConn(stream, dbus.WithContext(ctx))
	if err != nil {
		closeStream()
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := stream.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	if err := conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Geteuid()))}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := stream.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func checkSystemdPrivatePeer(stream net.Conn) error {
	unixConn, ok := stream.(*net.UnixConn)
	if !ok {
		return errors.New("systemd manager-private transport is not a Unix socket")
	}
	rawConn, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var peer *unix.Ucred
	var peerErr error
	if err := rawConn.Control(func(fd uintptr) {
		peer, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if peerErr != nil {
		return peerErr
	}
	if peer.Uid != 0 && peer.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("systemd manager-private peer uid %d does not match the current user", peer.Uid)
	}
	return nil
}

func sessionBusAddress() string {
	if address := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); address != "" && address != "autolaunch:" {
		return address
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		currentUser, err := user.Current()
		if err != nil {
			return ""
		}
		runtimeDir = filepath.Join("/run/user", currentUser.Uid)
	}
	busPath := filepath.Join(runtimeDir, "bus")
	if _, err := os.Stat(busPath); err == nil {
		return "unix:path=" + dbus.EscapeBusAddressValue(busPath)
	}
	content, err := os.ReadFile(filepath.Join(runtimeDir, "dbus-session"))
	if err != nil {
		return ""
	}
	const prefix = "DBUS_SESSION_BUS_ADDRESS="
	address := strings.TrimRight(strings.TrimPrefix(string(content), prefix), "\r\n")
	if !strings.HasPrefix(string(content), prefix) {
		return ""
	}
	return address
}

// loadedMatchesUnit rejects an update when a drop-in or another loaded source
// already makes the effective command differ from the unit file being edited.
func loadedMatchesUnit(loaded systemdLoadedConfig, unit parsedUnit) error {
	var expected []string
	if unit.ExecStartRaw != "" {
		expected = append([]string{unit.ExecStartRaw}, unit.ExecArgsRaw...)
	}
	if !loaded.loaded || len(loaded.execArgs) != len(expected) || loaded.ignoreErrors || len(loaded.execFlags) != 0 {
		return errors.New("the effective ExecStart is not the command in the unit file")
	}
	if !unit.ExecStartDynamic && loaded.execPath != unit.ExecStartRaw {
		return errors.New("the effective executable is not the command in the unit file")
	}
	for i, arg := range expected {
		if i == 0 && unit.ExecStartDynamic || i > 0 && i-1 < len(unit.ExecArgsDynamic) && unit.ExecArgsDynamic[i-1] {
			continue
		}
		if loaded.execArgs[i] != arg {
			return errors.New("the effective ExecStart is not the command in the unit file")
		}
	}
	return nil
}

func systemdInstallFlagExplicit(opts InstallOptions, flag string) bool {
	switch flag {
	case "config":
		return opts.Explicit.Config
	case "state-dir":
		return opts.Explicit.StateDir
	case "log-dir":
		return opts.Explicit.LogDir
	case "listen":
		return opts.Explicit.Listen
	case "cache-dir":
		return opts.Explicit.CacheDir
	case "hub-dir":
		return opts.Explicit.HubDir
	case "local-dir":
		return opts.Explicit.LocalDir
	default:
		return false
	}
}

func systemdDynamicInstallFields(unit parsedUnit, opts InstallOptions) map[string]bool {
	dynamic := make(map[string]bool)
	for flag, unresolved := range unit.DynamicRunArgs {
		if unresolved && !systemdInstallFlagExplicit(opts, flag) {
			dynamic[flag] = true
		}
	}
	if unit.WorkingDirectoryDynamic && !opts.Explicit.StateDir {
		if _, hasStateArg := unit.DynamicRunArgs["state-dir"]; !hasStateArg {
			dynamic["state-dir"] = true
		}
	}
	return dynamic
}

func systemdValidationOptions(opts InstallOptions, dynamic map[string]bool) InstallOptions {
	defaults := DefaultInstallOptions(opts.Scope, opts.Executable)
	if dynamic["config"] {
		opts.Paths.ConfigFile = defaults.Paths.ConfigFile
	}
	if dynamic["state-dir"] {
		opts.Paths.StateDir = defaults.Paths.StateDir
	}
	if dynamic["log-dir"] {
		opts.Paths.LogDir = defaults.Paths.LogDir
	}
	if dynamic["listen"] {
		opts.Listen = defaults.Listen
	}
	if dynamic["cache-dir"] {
		opts.CacheDir = ""
	}
	if dynamic["hub-dir"] {
		opts.HubDir = ""
	}
	if dynamic["local-dir"] {
		opts.LocalDir = ""
	}
	return opts
}

func systemdFilesystemOptions(opts InstallOptions, dynamic map[string]bool) InstallOptions {
	if dynamic["config"] {
		opts.Paths.ConfigFile = ""
	}
	if dynamic["state-dir"] {
		opts.Paths.StateDir = ""
	}
	if dynamic["log-dir"] {
		opts.Paths.LogDir = ""
	}
	if dynamic["cache-dir"] {
		opts.CacheDir = ""
	}
	if dynamic["hub-dir"] {
		opts.HubDir = ""
	}
	if dynamic["local-dir"] {
		opts.LocalDir = ""
	}
	return opts
}

// verifyUpdatedUnit checks the command and preserved launch settings after
// daemon-reload and before a running instance can be restarted.
func (m *systemdManager) verifyUpdatedUnit(ctx context.Context, opts InstallOptions, account *user.User, previous previousRegistration, unitContent string) error {
	loaded, err := m.registrationUnitConfig(ctx)
	if err != nil {
		return fmt.Errorf("service: read the loaded configuration after daemon-reload: %w", err)
	}
	updatedUnit := parseUnit(unitContent)
	if err := loadedMatchesUnit(loaded, updatedUnit); err != nil {
		return errors.New("service: systemd did not load the requested executable and arguments; an override may own the effective command")
	}
	if !previous.loadedKnown || !previous.loaded.loaded {
		return nil
	}
	for flag, dynamic := range updatedUnit.DynamicRunArgs {
		if !dynamic || systemdInstallFlagExplicit(opts, flag) {
			continue
		}
		before := runArgValue(previous.loaded.execArgs[1:], "--"+flag)
		after := runArgValue(loaded.execArgs[1:], "--"+flag)
		if before != after {
			return fmt.Errorf("service: systemd's typed --%s argument changed across the update", flag)
		}
	}
	if !slices.Equal(loaded.environment, previous.loaded.environment) || !slices.Equal(loaded.environmentFiles, previous.loaded.environmentFiles) {
		return errors.New("service: systemd did not preserve the loaded environment and environment files")
	}
	if previous.loaded.supplementaryGroupsKnown && loaded.supplementaryGroupsKnown && !slices.Equal(loaded.supplementaryGroups, previous.loaded.supplementaryGroups) {
		return errors.New("service: systemd did not preserve the loaded supplementary groups")
	}
	if !opts.Explicit.StateDir && loaded.workingDirectory != previous.loaded.workingDirectory {
		return errors.New("service: systemd did not preserve the loaded working directory")
	}
	if !opts.Explicit.ServiceUser && (loaded.user != previous.loaded.user || loaded.group != previous.loaded.group) {
		return errors.New("service: systemd did not preserve the loaded service identity")
	}
	if opts.Explicit.StateDir && loaded.workingDirectory != opts.Paths.StateDir {
		return errors.New("service: systemd did not load the requested working directory")
	}
	if opts.Explicit.ServiceUser {
		identity, err := unitIdentity(opts, account)
		if err != nil {
			return err
		}
		identityUnit := parseUnit("[Service]\n" + identity)
		if loaded.user != identityUnit.User || loaded.group != identityUnit.Group {
			return errors.New("service: systemd did not load the requested service identity")
		}
	}
	return nil
}

func sameSystemdLoadedConfig(a, b systemdLoadedConfig) bool {
	return a.loaded == b.loaded && a.execPath == b.execPath && slices.Equal(a.execArgs, b.execArgs) &&
		slices.Equal(a.execFlags, b.execFlags) && a.ignoreErrors == b.ignoreErrors &&
		a.workingDirectory == b.workingDirectory && a.user == b.user && a.group == b.group &&
		a.supplementaryGroupsKnown == b.supplementaryGroupsKnown && slices.Equal(a.supplementaryGroups, b.supplementaryGroups) &&
		slices.Equal(a.environment, b.environment) && slices.Equal(a.environmentFiles, b.environmentFiles) &&
		a.fragmentPathKnown == b.fragmentPathKnown && a.fragmentPath == b.fragmentPath &&
		a.dropInPathsKnown == b.dropInPathsKnown && slices.Equal(a.dropInPaths, b.dropInPaths)
}

type systemdExecStartEntry struct {
	Path                    string
	Argv                    []string
	IgnoreErrors            bool
	StartTimestamp          uint64
	ExitTimestamp           uint64
	StartTimestampMonotonic uint64
	ExitTimestampMonotonic  uint64
	PID                     uint32
	Code                    int32
	Status                  int32
}

type systemdExecStartExEntry struct {
	Path                    string
	Argv                    []string
	Flags                   []string
	StartTimestamp          uint64
	ExitTimestamp           uint64
	StartTimestampMonotonic uint64
	ExitTimestampMonotonic  uint64
	PID                     uint32
	Code                    int32
	Status                  int32
}

func loadedConfigFromProperties(properties map[string]dbus.Variant) (systemdLoadedConfig, error) {
	loaded := systemdLoadedConfig{loaded: true}
	if err := storeDBusProperty(properties, "WorkingDirectory", "s", &loaded.workingDirectory); err != nil {
		return systemdLoadedConfig{}, err
	}
	if err := storeDBusProperty(properties, "User", "s", &loaded.user); err != nil {
		return systemdLoadedConfig{}, err
	}
	if err := storeDBusProperty(properties, "Group", "s", &loaded.group); err != nil {
		return systemdLoadedConfig{}, err
	}
	if property, ok := properties["SupplementaryGroups"]; ok {
		if signature := property.Signature().String(); signature != "as" {
			return systemdLoadedConfig{}, fmt.Errorf("systemd property SupplementaryGroups has signature %q, want %q", signature, "as")
		}
		if err := dbus.Store([]interface{}{property.Value()}, &loaded.supplementaryGroups); err != nil {
			return systemdLoadedConfig{}, fmt.Errorf("decode systemd property SupplementaryGroups: %w", err)
		}
		loaded.supplementaryGroupsKnown = true
	}
	if err := storeDBusProperty(properties, "Environment", "as", &loaded.environment); err != nil {
		return systemdLoadedConfig{}, err
	}
	if err := storeDBusProperty(properties, "EnvironmentFiles", "a(sb)", &loaded.environmentFiles); err != nil {
		return systemdLoadedConfig{}, err
	}
	if property, ok := properties["FragmentPath"]; ok {
		if signature := property.Signature().String(); signature != "s" {
			return systemdLoadedConfig{}, fmt.Errorf("systemd property FragmentPath has signature %q, want %q", signature, "s")
		}
		if err := dbus.Store([]interface{}{property.Value()}, &loaded.fragmentPath); err != nil {
			return systemdLoadedConfig{}, fmt.Errorf("decode systemd property FragmentPath: %w", err)
		}
		loaded.fragmentPathKnown = true
	}
	if property, ok := properties["DropInPaths"]; ok {
		if signature := property.Signature().String(); signature != "as" {
			return systemdLoadedConfig{}, fmt.Errorf("systemd property DropInPaths has signature %q, want %q", signature, "as")
		}
		if err := dbus.Store([]interface{}{property.Value()}, &loaded.dropInPaths); err != nil {
			return systemdLoadedConfig{}, fmt.Errorf("decode systemd property DropInPaths: %w", err)
		}
		loaded.dropInPathsKnown = true
	}

	if command, ok := properties["ExecStartEx"]; ok {
		if signature := command.Signature().String(); signature != "a(sasasttttuii)" {
			return systemdLoadedConfig{}, fmt.Errorf("systemd returned ExecStartEx with signature %q", signature)
		}
		var commands []systemdExecStartExEntry
		if err := dbus.Store([]interface{}{command.Value()}, &commands); err != nil {
			return systemdLoadedConfig{}, fmt.Errorf("decode ExecStartEx: %w", err)
		}
		if len(commands) > 1 {
			return systemdLoadedConfig{}, errors.New("systemd reports multiple ExecStart commands")
		}
		if len(commands) == 1 {
			loaded.execPath = commands[0].Path
			loaded.execArgs = commands[0].Argv
			loaded.execFlags = commands[0].Flags
		}
	} else if command, ok := properties["ExecStart"]; ok {
		if signature := command.Signature().String(); signature != "a(sasbttttuii)" {
			return systemdLoadedConfig{}, fmt.Errorf("systemd returned ExecStart with signature %q", signature)
		}
		var commands []systemdExecStartEntry
		if err := dbus.Store([]interface{}{command.Value()}, &commands); err != nil {
			return systemdLoadedConfig{}, fmt.Errorf("decode ExecStart: %w", err)
		}
		if len(commands) > 1 {
			return systemdLoadedConfig{}, errors.New("systemd reports multiple ExecStart commands")
		}
		if len(commands) == 1 {
			loaded.execPath = commands[0].Path
			loaded.execArgs = commands[0].Argv
			loaded.ignoreErrors = commands[0].IgnoreErrors
		}
	} else {
		return systemdLoadedConfig{}, errors.New("systemd did not return ExecStart or ExecStartEx")
	}
	return loaded, nil
}

func storeDBusProperty(properties map[string]dbus.Variant, name, signature string, destination interface{}) error {
	property, ok := properties[name]
	if !ok {
		return fmt.Errorf("systemd did not return property %s", name)
	}
	if got := property.Signature().String(); got != signature {
		return fmt.Errorf("systemd property %s has signature %q, want %q", name, got, signature)
	}
	if err := dbus.Store([]interface{}{property.Value()}, destination); err != nil {
		return fmt.Errorf("decode systemd property %s: %w", name, err)
	}
	return nil
}

// registrationEnablement is what the supervisor reports about the enablement of
// the unit.
type registrationEnablement struct {
	// mode is the autostart state the supervisor reported for the unit.
	// Only an enabled unit is enabled again by a repeat installation, and
	// the mode is kept: a runtime-only enablement stays runtime-only.
	mode enablementMode
	// masked reports whether the unit is masked, persistently or at
	// runtime. A persistent mask is a link to /dev/null, so writing the
	// unit file would replace the mask and silently undo the decision of
	// an administrator. A runtime mask lives in /run and leaves no file at
	// the unit path, so the installation would register a unit that stays
	// masked and cannot start.
	masked bool
}

// enablementMode is the autostart state of a unit, as far as an installation is
// concerned: whether the unit is enabled for the boot of the scope, and whether
// that enablement survives a reboot.
type enablementMode int

const (
	// enablementNone means the unit is not enabled for boot, or the
	// supervisor reported a state no installation can keep. The
	// installation then enables nothing.
	enablementNone enablementMode = iota
	// enablementPersistent means the unit is enabled for boot and stays
	// enabled across reboots.
	enablementPersistent
	// enablementRuntime means the unit is enabled for this boot only: an
	// administrator ran `systemctl enable --runtime`.
	enablementRuntime
)

// enablementOf returns the enablement the printed state of "systemctl
// is-enabled" describes.
//
// The printed state decides, not the exit status: "is-enabled" exits zero for
// "static", "indirect", "alias" and "linked" as well, and none of those is an
// enablement an installation could keep. Only "enabled" and "enabled-runtime"
// are enablements, and they differ in whether they survive a reboot, so the
// installation preserves the mode instead of running a plain "enable".
func enablementOf(state string) registrationEnablement {
	switch state {
	case "enabled":
		return registrationEnablement{mode: enablementPersistent}
	case "enabled-runtime":
		return registrationEnablement{mode: enablementRuntime}
	case "masked", "masked-runtime":
		return registrationEnablement{masked: true}
	default:
		// "disabled", "not-found", "static", "indirect", "alias",
		// "linked", an empty answer and a state a later release adds all
		// name no enablement to keep.
		return registrationEnablement{}
	}
}

// registrationEnablement reads whether the supervisor had the unit enabled for
// the boot of the scope, and in which mode.
//
// The answer comes from the supervisor, not from the file system: an
// enablement is the links the supervisor keeps for the [Install] section, and
// only the supervisor knows them. The printed state decides, so a state that
// exits zero without being an enablement ("static", "indirect", "alias",
// "linked") is not taken for one. A supervisor that cannot be reached prints
// nothing and describes no enablement either, which leaves the autostart state
// alone on a repeat installation, and the unreachable supervisor still fails the
// installation at the reload.
//
// The mask is part of the answer for the same reason: a persistent mask is a
// link to /dev/null at the unit path, but a runtime mask ("systemctl mask
// --runtime") lives in /run and leaves no file there, so the masked state is
// only visible through the supervisor. A fresh machine reports "not-found" and
// is not masked, which is what a first installation needs.
func (m *systemdManager) registrationEnablement(ctx context.Context) registrationEnablement {
	out, _ := m.runSystemctl(ctx, "is-enabled", unitName)
	return enablementOf(strings.TrimSpace(out))
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

// systemdRegisterResult distinguishes a successful version check from proof
// that created directories are safe to remove after a failure.
type systemdRegisterResult struct {
	versionVerified         bool
	createdDirsSafeToRemove bool
}

// register writes the registration of the instance and hands it to the
// supervisor.
//
// The directories of the instance are already created when the function is
// called. A failure of the registration is rolled back: the unit file is
// restored, and the result tells the caller whether the remaining registration
// is proven not to reference directories this attempt created.
//
// The unit is enabled for boot only when the installation has to enable it
// (see reloadAndEnable).
//
// A replaced registration that was running is restarted and verified; a failure
// of that restart or of the verification restores the previous registration and
// brings the previous instance back into service. The restore reports a started
// instance only when the supervisor was observed running it. The result reports
// whether the restarted instance reported the expected version.

func (m *systemdManager) register(ctx context.Context, opts InstallOptions, account *user.User, identity systemdInstallIdentity, created map[string][]string, previous previousRegistration) (systemdRegisterResult, error) {
	result := systemdRegisterResult{createdDirsSafeToRemove: systemdCreatedDirsSafeToRemove(previous, created)}
	// The instance runs as the service account, so the directories it owns
	// belong to it.
	if err := m.grantInstanceDirsForDirsWithIdentity(opts, identity, created, systemdInstanceDirs(opts, previous.dynamicFields)); err != nil {
		return result, err
	}

	var unit string
	var err error
	if previous.registrationPresent() {
		sourceContent, ok := previous.sourceUnitContent()
		if !ok {
			return result, errors.New("service: the prior registration source fragment was not captured")
		}
		unit, err = updateUnit(string(sourceContent), opts, account)
	} else {
		unit, err = renderUnit(opts, account)
	}
	if err != nil {
		return result, err
	}

	if err := writeUnitFile(previous.path, unit); err != nil {
		return result, err
	}
	// The file is now part of the registration path. Until rollback proves
	// the previous unit restored or the new unit removed, its directories stay.
	result.createdDirsSafeToRemove = false
	// A first installation enables the unit for boot; a repeat installation
	// keeps the autostart state it finds, in the mode it found it.
	enable := enablementPersistent
	if previous.registrationPresent() {
		enable = previous.enablement
	}
	if err := m.reloadAndEnable(ctx, enable); err != nil {
		result.createdDirsSafeToRemove, err = m.reportUnitRollback(ctx, previous, created, err)
		return result, err
	}
	if previous.registrationPresent() {
		if err := m.verifyUpdatedUnit(ctx, opts, account, previous, unit); err != nil {
			result.createdDirsSafeToRemove, err = m.reportUnitRollback(ctx, previous, created, err)
			return result, err
		}
	}
	if !previous.running {
		// A first installation and a stopped instance stay stopped:
		// starting is an explicit, separate command.
		return result, nil
	}
	updatedUnit := parseUnit(unit)
	verified, err := m.restartAndVerify(ctx, opts.Executable, updatedUnit.listen(), opts.ExpectedVersion, updatedUnit.DynamicRunArgs["listen"])
	if err != nil {
		if restoreErr := m.restorePreviousRegistration(ctx, previous); restoreErr != nil {
			return result, fmt.Errorf("service: the installation did not put the new executable into service: %w (the previous registration could not be put back into service: %v)", err, restoreErr)
		}
		result.createdDirsSafeToRemove = systemdCreatedDirsSafeToRemove(previous, created)
		return result, fmt.Errorf("service: the installation did not put the new executable into service: %w (the previous registration was restored and the previous instance was started again)", err)
	}
	result.versionVerified = verified
	return result, nil
}

// restorePreviousRegistration restores the registration an installation
// replaced and the running state it had.
//
// The unit file is restored and read back, then the supervisor is reloaded and
// its loaded command and launch properties are compared with the pre-update
// snapshot. The restart below is issued only after both states are confirmed.
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
	if err := m.rollbackUnit(cleanup, previous); err != nil {
		return err
	}
	if !previous.running {
		return nil
	}
	if _, err := m.runSystemctl(cleanup, "restart", unitName); err != nil {
		return fmt.Errorf("the registration was restored, but the restart of the previous instance was refused: %w", err)
	}
	// The restored unit supplies its static Listen source, while the restored
	// MainPID must still expose a live HFDesk service-run argv before readiness
	// can be claimed, even when the old ExecStart path was unavailable.
	sourceContent, ok := previous.sourceUnitContent()
	if !ok {
		return errors.New("the source fragment of the previous registration was not captured")
	}
	restored := parseUnit(string(sourceContent))
	if _, err := m.verifyRunningExecutableWithListenSource(cleanup, restored.ExecStart, restored.listen(), "", restored.DynamicRunArgs["listen"]); err != nil {
		return fmt.Errorf("the registration was restored and the restart of the previous instance was issued, but the restart was not confirmed: %w", err)
	}
	return nil
}

// reportUnitRollback preserves the failure that triggered a rollback and names
// only the outcome the storage and loaded-configuration checks proved.
func (m *systemdManager) reportUnitRollback(ctx context.Context, previous previousRegistration, created map[string][]string, failure error) (bool, error) {
	if err := m.rollbackUnit(context.WithoutCancel(ctx), previous); err != nil {
		return false, fmt.Errorf("%w (the previous unit file or captured loaded settings could not be restored or verified: %v)", failure, err)
	}
	if previous.registrationPresent() {
		if !previous.loaded.loaded {
			return systemdCreatedDirsSafeToRemove(previous, created), fmt.Errorf("%w (the previous registration and command were restored and loaded; no instance was restarted)", failure)
		}
		return systemdCreatedDirsSafeToRemove(previous, created), fmt.Errorf("%w (the previous registration and captured loaded command/launch settings were restored)", failure)
	}
	return true, fmt.Errorf("%w (the failed registration was removed from disk and the supervisor)", failure)
}

// verifyRestoredUnit checks that the unit file holds the previous registration
// again before systemd is asked to reload it.
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

// restartAndVerify restarts the unit and verifies the supervisor state, the
// actual HFDesk MainPID/argv, and the readiness response of that same stable
// process. A dynamic Listen value comes from the runtime argv, never the source.
func (m *systemdManager) restartAndVerify(ctx context.Context, executable, listen, expectedVersion string, dynamicListen bool) (bool, error) {
	if _, err := m.runSystemctl(ctx, "restart", unitName); err != nil {
		return false, fmt.Errorf("service: restart %s: %w", unitName, err)
	}
	return m.verifyRunningExecutableWithListenSource(ctx, executable, listen, expectedVersion, dynamicListen)
}

func (m *systemdManager) runtimeRunOptions(ctx context.Context) (RunOptions, uint32, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	properties, err := m.loadedServiceProperties(ctx, unitName)
	if err != nil {
		return RunOptions{}, 0, fmt.Errorf("read the active service process from systemd: %w", err)
	}
	mainPID, err := systemdMainPIDFromProperties(properties)
	if err != nil {
		return RunOptions{}, 0, err
	}
	commandLine, err := os.ReadFile(filepath.Join("/proc", strconv.FormatUint(uint64(mainPID), 10), "cmdline"))
	if err != nil {
		return RunOptions{}, 0, fmt.Errorf("read the actual service process argv: %w", err)
	}
	if len(commandLine) == 0 {
		return RunOptions{}, 0, errors.New("the running service process exposed an empty argv")
	}
	argv := strings.Split(strings.TrimSuffix(string(commandLine), "\x00"), "\x00")
	if len(argv) < 2 || argv[0] == "" {
		return RunOptions{}, 0, errors.New("the running service process did not expose usable argv")
	}
	if options, err := ParseRunArgs(argv[1:]); err == nil {
		return options, mainPID, nil
	}
	// A shebang executable may put its script path before the service command.
	// Reuse the host parser rather than interpreting an environment expression.
	for i := 1; i+1 < len(argv); i++ {
		if argv[i] == "service" && argv[i+1] == "run" {
			options, err := ParseRunArgs(argv[i:])
			return options, mainPID, err
		}
	}
	return RunOptions{}, 0, errors.New("the running process argv did not expose HFDesk service run options")
}

func systemdMainPIDFromProperties(properties map[string]dbus.Variant) (uint32, error) {
	property, ok := properties["MainPID"]
	if !ok {
		return 0, errors.New("systemd did not report the running service MainPID")
	}
	if signature := property.Signature().String(); signature != "u" {
		return 0, fmt.Errorf("systemd property MainPID has signature %q, want %q", signature, "u")
	}
	var pid uint32
	if err := dbus.Store([]interface{}{property.Value()}, &pid); err != nil {
		return 0, fmt.Errorf("decode systemd property MainPID: %w", err)
	}
	if pid == 0 {
		return 0, errors.New("systemd reported no running service MainPID")
	}
	return pid, nil
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

// verifyRunningExecutable waits until the supervisor reports the expected
// program, a stable MainPID exposes HFDesk service-run arguments, and that
// process answers on its effective Listen address.
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
// An empty executable skips only the path comparison. It does not waive the
// requirement for a live HFDesk process. The wait is bounded by the shutdown
// budget and the context of the installation.
func (m *systemdManager) verifyRunningExecutable(ctx context.Context, executable, listen, expectedVersion string) (bool, error) {
	return m.verifyRunningExecutableWithListenSource(ctx, executable, listen, expectedVersion, false)
}

func (m *systemdManager) verifyRunningExecutableWithListenSource(ctx context.Context, executable, listen, expectedVersion string, dynamicListen bool) (bool, error) {
	budget := m.restartVerifyWait()
	deadline := time.Now().Add(budget)
	address := ""
	if !dynamicListen {
		address = probeServiceAddress(listen)
	}
	detail := "the supervisor did not report a state"
	for {
		probeFailed := false
		var runtimeProcessErr error
		out, err := m.runSystemctl(ctx, "show", unitName,
			"--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=ExecStart")
		if err != nil {
			detail = err.Error()
		} else {
			show := parseSystemdShow(out)
			path := execStartPath(out)
			programMatches := show.State == StateRunning && (executable == "" || path == systemdLoadedExecutablePath(executable))
			switch {
			case !programMatches && executable == "":
				detail = fmt.Sprintf("the supervisor reports the state %q", show.State)
			case !programMatches:
				detail = fmt.Sprintf("the supervisor reports the state %q with the program %q", show.State, path)
			default:
				// Active unit metadata is not a running process: RemainAfterExit
				// can report active with MainPID=0. Use systemd's main process and
				// HFDesk's parser for static and expanded arguments alike.
				runOptions, mainPID, processErr := m.runtimeRunOptions(ctx)
				if processErr != nil {
					runtimeProcessErr = processErr
					probeFailed = true
					detail = fmt.Sprintf("the active unit's HFDesk process is not observable: %v", processErr)
					break
				}
				effectiveListen := runOptions.Listen
				address = probeServiceAddress(effectiveListen)
				if !dynamicListen && listenArg(effectiveListen) != listenArg(listen) {
					runtimeProcessErr = fmt.Errorf("the running process Listen %q does not match the static unit Listen %q", effectiveListen, listen)
					probeFailed = true
					detail = runtimeProcessErr.Error()
					break
				}
				verified, probeErr := serviceInstallProbe(effectiveListen, expectedVersion)
				if probeErr != nil {
					if errors.Is(probeErr, errServiceVersionMismatch) {
						return false, fmt.Errorf("the restarted %s does not run the expected version: %w", unitName, probeErr)
					}
					probeFailed = true
					if executable == "" {
						detail = fmt.Sprintf("the supervisor reports the state %q, but the instance does not answer on %s: %v", show.State, address, probeErr)
					} else {
						detail = fmt.Sprintf("the supervisor reports the state %q with the program %q, but the instance does not answer on %s: %v", show.State, path, address, probeErr)
					}
				} else {
					confirmedOptions, confirmedPID, confirmErr := m.runtimeRunOptions(ctx)
					switch {
					case confirmErr != nil:
						runtimeProcessErr = fmt.Errorf("confirm the service process after readiness: %w", confirmErr)
						probeFailed = true
						detail = runtimeProcessErr.Error()
					case confirmedPID != mainPID || confirmedOptions != runOptions:
						runtimeProcessErr = errors.New("the service MainPID or runtime argv changed during readiness verification")
						probeFailed = true
						detail = runtimeProcessErr.Error()
					default:
						return verified, nil
					}
				}
			}
		}
		if !time.Now().Before(deadline) {
			if runtimeProcessErr != nil {
				if dynamicListen {
					return false, fmt.Errorf("the restarted %s runtime process/Listen was not observable and stable within %s: %w", unitName, budget, runtimeProcessErr)
				}
				return false, fmt.Errorf("the restarted %s service process could not be verified within %s: %w", unitName, budget, runtimeProcessErr)
			}
			switch {
			case probeFailed:
				return false, fmt.Errorf("the restarted %s does not answer on %s within %s: %s", unitName, address, budget, detail)
			case executable == "":
				return false, fmt.Errorf("the %s is not active within %s: %s", unitName, budget, detail)
			default:
				return false, fmt.Errorf("the restarted %s does not run %q within %s: %s", unitName, executable, budget, detail)
			}
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
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

// preflight rejects an installation the supervisor demonstrably cannot run:
// a missing supervisor, an unusable bind address, a definite access denial, or
// a format failure that the host's visible binary-format handlers cannot
// resolve. Unknown executable formats are left to systemd rather than rejected
// by this partial inspection.
func (m *systemdManager) preflight(ctx context.Context, opts InstallOptions, account *user.User) error {
	var (
		accountName string
		identity    *accessAccount
	)
	if m.scope == ScopeUser {
		if current, err := user.Current(); err == nil {
			accountName = current.Username
		}
		current := currentExecutableAccessIdentity()
		identity = &current
	} else if account != nil {
		accountName = account.Username
		if current, err := linuxExecutableAccessIdentity(account); err == nil {
			identity = &current
		}
	}
	return m.preflightWithContext(ctx, opts, accountName, identity, opts.Paths.StateDir)
}

func systemdWorkingDirectoryForPreflight(opts InstallOptions, existing bool, loaded systemdLoadedConfig, loadedKnown bool) string {
	if !existing {
		return opts.Paths.StateDir
	}
	// An update that changes WorkingDirectory can be shadowed by native
	// drop-ins. Until the resulting manager value is loaded, its effective
	// cwd is not established for this preflight.
	if opts.Explicit.StateDir || !loadedKnown || !loaded.loaded {
		return ""
	}
	if filepath.IsAbs(loaded.workingDirectory) {
		return loaded.workingDirectory
	}
	return ""
}

func (m *systemdManager) preflightWithContext(ctx context.Context, opts InstallOptions, accountName string, identity *accessAccount, workingDirectory string) error {
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
	inspection, err := inspectExecutable(opts.Executable, workingDirectory, linuxBinfmtMiscDirectory)
	if err != nil {
		return err
	}
	if inspection.format == executableFormatUnknown {
		log.Printf("service: executable %q has an unclassified format; the format is not being treated as verified or refused by the preflight", opts.Executable)
	}

	accessMode := uint32(unix.X_OK)
	if inspection.format == executableFormatScript {
		// Linux's script handler starts the interpreter, which must be able
		// to read the script itself. Native executables do not need this bit.
		accessMode |= unix.R_OK
	}
	access := executableAccessUnknown
	if identity != nil {
		access, err = checkExecutableAccess(ctx, opts.Executable, *identity, accessMode, workingDirectory)
		if err != nil {
			return err
		}
	}
	if accountName == "" {
		accountName = "the native service identity"
	}
	switch access {
	case executableAccessDenied:
		reason := errExecutableNotUsable
		if inspection.format == executableFormatScript && accessMode&unix.R_OK != 0 {
			reason = errExecutableNotReadable
		}
		return fmt.Errorf("service: executable %q is not usable by the account %q: %w", opts.Executable, accountName, reason)
	case executableAccessUnknown:
		log.Printf("service: execute access for %q by %s is inconclusive; it is not treated as a denial or as verified", opts.Executable, accountName)
	}

	if inspection.interpreter != "" {
		interpreterWorkingDirectory := ""
		if !filepath.IsAbs(inspection.interpreter) {
			interpreterWorkingDirectory = workingDirectory
		}
		interpreterAccess := executableAccessUnknown
		if identity != nil && (filepath.IsAbs(inspection.interpreter) || interpreterWorkingDirectory != "") {
			interpreterAccess, err = checkExecutableAccess(ctx, inspection.interpreter, *identity, uint32(unix.X_OK), interpreterWorkingDirectory)
			if err != nil {
				return err
			}
		}
		switch interpreterAccess {
		case executableAccessDenied:
			return fmt.Errorf("service: executable %q names an interpreter the account %q cannot use: %q", opts.Executable, accountName, inspection.interpreter)
		case executableAccessUnknown:
			log.Printf("service: interpreter access for %q by %s is inconclusive; it is not treated as a denial or as verified", inspection.interpreter, accountName)
		}
	}
	return nil
}

type executableAccess int

const (
	executableAccessUnknown executableAccess = iota
	executableAccessAllowed
	executableAccessDenied
)

var (
	errExecutableNotUsable   = errors.New("the account cannot execute it")
	errExecutableNotReadable = errors.New("the account cannot read the script")
)

func executableAccessFromError(err error) executableAccess {
	switch {
	case err == nil:
		return executableAccessAllowed
	case errors.Is(err, errAccessWorkingDirectoryNotReady):
		return executableAccessUnknown
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ELOOP):
		return executableAccessDenied
	default:
		return executableAccessUnknown
	}
}

type executableFormat int

const (
	executableFormatUnknown executableFormat = iota
	executableFormatELF
	executableFormatScript
)

type executableInspection struct {
	format      executableFormat
	interpreter string
}

type binfmtMiscAvailability int

const (
	binfmtMiscUnknown binfmtMiscAvailability = iota
	binfmtMiscEmpty
	binfmtMiscActive
)

const (
	linuxBinfmtMiscDirectory = "/proc/sys/fs/binfmt_misc"
	maxExecutablePrefix      = 4096
	maxShebangLine           = 127
)

// inspectExecutable rejects only formats the available host evidence shows the
// ordinary Linux loaders cannot handle. It deliberately does not compare ELF
// machine identities or reject other files merely because this inspection does
// not recognize them.
func inspectExecutable(path, workingDirectory, binfmtDirectory string) (executableInspection, error) {
	handlers := binfmtMiscHandlers(binfmtDirectory)
	file, err := os.Open(path)
	if err != nil {
		// In particular, an execute-only ELF is a valid input even though the
		// installer cannot read its header.
		return executableInspection{format: executableFormatUnknown}, nil
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return executableInspection{format: executableFormatUnknown}, nil
	}
	readSize := info.Size()
	if readSize > maxExecutablePrefix {
		readSize = maxExecutablePrefix
	}
	prefix := make([]byte, int(readSize))
	n, readErr := io.ReadFull(file, prefix)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return executableInspection{format: executableFormatUnknown}, nil
	}
	prefix = prefix[:n]

	if bytes.HasPrefix(prefix, []byte{0x7f, 'E', 'L', 'F'}) {
		if handlers != binfmtMiscEmpty {
			return executableInspection{format: executableFormatUnknown}, nil
		}
		if err := checkTruncatedELFHeader(prefix, info.Size()); err != nil {
			return executableInspection{}, fmt.Errorf("service: executable %q: %w", path, err)
		}
		interpreter, known, err := elfProgramInterpreter(file, prefix, info.Size())
		if err != nil {
			return executableInspection{}, fmt.Errorf("service: executable %q: %w", path, err)
		}
		if !known {
			return executableInspection{format: executableFormatUnknown}, nil
		}
		if interpreter != "" && !filepath.IsAbs(interpreter) && workingDirectory == "" {
			return executableInspection{format: executableFormatUnknown}, nil
		}
		return executableInspection{format: executableFormatELF, interpreter: interpreter}, nil
	}

	if bytes.HasPrefix(prefix, []byte("#!")) {
		if handlers != binfmtMiscEmpty {
			return executableInspection{format: executableFormatUnknown}, nil
		}
		interpreter, ok := linuxShebangInterpreter(prefix, info.Size())
		if !ok {
			return executableInspection{format: executableFormatUnknown}, nil
		}
		if interpreter == "" {
			return executableInspection{}, fmt.Errorf("service: executable %q has a shebang with no interpreter", path)
		}
		if !filepath.IsAbs(interpreter) && workingDirectory == "" {
			return executableInspection{format: executableFormatUnknown}, nil
		}
		return executableInspection{format: executableFormatScript, interpreter: interpreter}, nil
	}

	if handlers == binfmtMiscEmpty && plainTextFile(prefix, info.Size()) {
		return executableInspection{}, fmt.Errorf("service: executable %q is plain text without a shebang and no active binfmt_misc handler is visible", path)
	}
	return executableInspection{format: executableFormatUnknown}, nil
}

func checkTruncatedELFHeader(prefix []byte, fileSize int64) error {
	if len(prefix) < 16 || fileSize < 16 {
		return errors.New("truncated ELF identification")
	}
	var headerSize int64
	switch prefix[4] {
	case 1: // ELFCLASS32
		headerSize = 52
	case 2: // ELFCLASS64
		headerSize = 64
	default:
		return errors.New("invalid ELF class")
	}
	if prefix[5] != 1 && prefix[5] != 2 {
		return errors.New("invalid ELF byte order")
	}
	if prefix[6] != 1 {
		return errors.New("invalid ELF identification version")
	}
	if fileSize < headerSize || int64(len(prefix)) < headerSize {
		return errors.New("truncated ELF header")
	}
	return nil
}

// elfProgramInterpreter reads only the ELF program-header table that the Linux
// executable loader consumes. It does not classify the machine or emulate ELF
// load behavior; a table it cannot interpret stays unclassified.
func elfProgramInterpreter(file *os.File, header []byte, fileSize int64) (string, bool, error) {
	var order binary.ByteOrder
	var phoff uint64
	var phentsize, phnum uint16
	var programHeaderSize uint64
	switch header[4] {
	case 1: // ELFCLASS32
		order = binary.LittleEndian
		if header[5] == 2 {
			order = binary.BigEndian
		}
		phoff = uint64(order.Uint32(header[28:32]))
		phentsize = order.Uint16(header[42:44])
		phnum = order.Uint16(header[44:46])
		programHeaderSize = 32
	case 2: // ELFCLASS64
		order = binary.LittleEndian
		if header[5] == 2 {
			order = binary.BigEndian
		}
		phoff = order.Uint64(header[32:40])
		phentsize = order.Uint16(header[54:56])
		phnum = order.Uint16(header[56:58])
		programHeaderSize = 56
	default:
		return "", false, nil
	}
	if phnum == 0xffff || phnum == 0 || uint64(phentsize) != programHeaderSize {
		return "", false, nil
	}
	tableSize := uint64(phnum) * programHeaderSize
	if fileSize < 0 || phoff > uint64(fileSize) || tableSize > uint64(fileSize)-phoff {
		return "", true, errors.New("truncated ELF program header table")
	}
	table := make([]byte, int(tableSize))
	n, err := file.ReadAt(table, int64(phoff))
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", false, nil
	}
	if n != len(table) {
		return "", false, nil
	}

	var interpreter string
	for i := 0; i < int(phnum); i++ {
		entry := table[i*int(programHeaderSize) : (i+1)*int(programHeaderSize)]
		if order.Uint32(entry[:4]) != 3 { // PT_INTERP
			continue
		}
		if interpreter != "" {
			return "", true, errors.New("ELF has multiple interpreter segments")
		}
		var offset, size uint64
		if header[4] == 1 {
			offset = uint64(order.Uint32(entry[4:8]))
			size = uint64(order.Uint32(entry[16:20]))
		} else {
			offset = order.Uint64(entry[8:16])
			size = order.Uint64(entry[32:40])
		}
		if size == 0 || offset > uint64(fileSize) || size > uint64(fileSize)-offset {
			return "", true, errors.New("truncated ELF interpreter segment")
		}
		if size > maxExecutablePrefix {
			return "", false, nil
		}
		segment := make([]byte, int(size))
		n, err := file.ReadAt(segment, int64(offset))
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return "", false, nil
		}
		if n != len(segment) {
			return "", false, nil
		}
		terminator := bytes.IndexByte(segment, 0)
		if terminator <= 0 {
			return "", false, nil
		}
		interpreter = string(segment[:terminator])
	}
	return interpreter, true, nil
}

func linuxShebangInterpreter(prefix []byte, fileSize int64) (string, bool) {
	line := prefix[2:]
	if newline := bytes.IndexByte(line, '\n'); newline >= 0 {
		if newline > maxShebangLine {
			return "", false
		}
		line = line[:newline]
	} else if fileSize-int64(2) > maxShebangLine {
		// The kernel truncates long shebang lines. Without knowing which
		// kernel rule applies, do not guess at the interpreter path.
		return "", false
	}
	line = bytes.TrimLeft(line, " \t")
	if nul := bytes.IndexByte(line, 0); nul >= 0 {
		return "", false
	}
	if end := bytes.IndexAny(line, " \t"); end >= 0 {
		line = line[:end]
	}
	return string(line), true
}

func plainTextFile(prefix []byte, fileSize int64) bool {
	if fileSize < 0 || fileSize > maxExecutablePrefix || int64(len(prefix)) != fileSize || !utf8.Valid(prefix) {
		return false
	}
	for _, r := range string(prefix) {
		if r != '\n' && r != '\r' && r != '\t' && !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func binfmtMiscHandlers(directory string) binfmtMiscAvailability {
	status, err := os.ReadFile(filepath.Join(directory, "status"))
	if err != nil {
		return binfmtMiscUnknown
	}
	switch strings.TrimSpace(string(status)) {
	case "disabled":
		return binfmtMiscEmpty
	case "enabled":
	default:
		return binfmtMiscUnknown
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return binfmtMiscUnknown
	}
	for _, entry := range entries {
		if entry.Name() == "register" || entry.Name() == "status" {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return binfmtMiscUnknown
		}
		firstLine, _, _ := bytes.Cut(contents, []byte{'\n'})
		if string(firstLine) == "enabled" {
			return binfmtMiscActive
		}
		if string(firstLine) != "disabled" {
			return binfmtMiscUnknown
		}
	}
	return binfmtMiscEmpty
}

func linuxExecutableAccessIdentity(account *user.User) (accessAccount, error) {
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return accessAccount{}, err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return accessAccount{}, err
	}
	groupIDs, err := account.GroupIds()
	if err != nil {
		return accessAccount{}, err
	}
	identity := accessAccount{UID: uid, GID: gid, Groups: []int{gid}}
	seen := map[int]struct{}{gid: {}}
	for _, groupID := range groupIDs {
		group, err := strconv.Atoi(groupID)
		if err != nil {
			return accessAccount{}, err
		}
		if _, ok := seen[group]; ok {
			continue
		}
		seen[group] = struct{}{}
		identity.Groups = append(identity.Groups, group)
	}
	return identity, nil
}

func currentExecutableAccessIdentity() accessAccount {
	groups, _ := unix.Getgroups()
	identity := accessAccount{UID: unix.Geteuid(), GID: unix.Getegid(), Groups: []int{unix.Getegid()}}
	seen := map[int]struct{}{identity.GID: {}}
	for _, group := range groups {
		if _, ok := seen[group]; ok {
			continue
		}
		seen[group] = struct{}{}
		identity.Groups = append(identity.Groups, group)
	}
	return identity
}

func sameAccessIdentity(left, right accessAccount) bool {
	if left.UID != right.UID || left.GID != right.GID || len(left.Groups) != len(right.Groups) {
		return false
	}
	groups := make(map[int]struct{}, len(left.Groups))
	for _, group := range left.Groups {
		groups[group] = struct{}{}
	}
	for _, group := range right.Groups {
		if _, ok := groups[group]; !ok {
			return false
		}
	}
	return true
}

func accessCredential(identity accessAccount) (*syscall.Credential, error) {
	uid, uidErr := linuxIDToUint32(int64(identity.UID))
	gid, gidErr := linuxIDToUint32(int64(identity.GID))
	if uidErr != nil || gidErr != nil {
		return nil, errors.New("the service identity is outside the Linux UID/GID range")
	}
	groups := make([]uint32, 0, len(identity.Groups))
	seen := make(map[uint32]struct{}, len(identity.Groups))
	for _, group := range append([]int{identity.GID}, identity.Groups...) {
		id, err := linuxIDToUint32(int64(group))
		if err != nil {
			return nil, errors.New("the service identity has a group outside the Linux GID range")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		groups = append(groups, id)
	}
	return &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}, nil
}

func linuxIDToUint32(id int64) (uint32, error) {
	if id < 0 || id > math.MaxUint32 {
		return 0, errors.New("the Linux UID/GID is outside the uint32 range")
	}
	return uint32(id), nil
}

func systemdUnicode32EscapeRune(value uint64) (rune, bool) {
	if value == 0 || value > utf8.MaxRune {
		return 0, false
	}
	r := rune(value)
	if !utf8.ValidRune(r) || (r >= 0xfdd0 && r <= 0xfdef) || r&0xfffe == 0xfffe {
		return 0, false
	}
	return r, true
}

func (m *systemdManager) executableAccess(ctx context.Context, path string, mode uint32, account *user.User, workingDirectory string) (executableAccess, error) {
	if m.scope == ScopeUser {
		return checkExecutableAccess(ctx, path, currentExecutableAccessIdentity(), mode, workingDirectory)
	}
	if account == nil {
		return executableAccessUnknown, nil
	}
	target, err := linuxExecutableAccessIdentity(account)
	if err != nil {
		return executableAccessUnknown, nil
	}
	return checkExecutableAccess(ctx, path, target, mode, workingDirectory)
}

func checkExecutableAccess(ctx context.Context, path string, identity accessAccount, mode uint32, workingDirectory string) (executableAccess, error) {
	if sameAccessIdentity(identity, currentExecutableAccessIdentity()) {
		if identity.UID != 0 && accessProbeHasCapabilities() {
			return executableAccessUnknown, nil
		}
		if unix.Getuid() != unix.Geteuid() || unix.Getgid() != unix.Getegid() {
			return executableAccessUnknown, nil
		}
		return executableAccessFromError(faccessatExecutablePath(path, mode, workingDirectory)), nil
	}
	credential, err := accessCredential(identity)
	if err != nil {
		return executableAccessUnknown, nil
	}
	running, err := os.Executable()
	if err != nil {
		return executableAccessUnknown, nil
	}
	if !safeAccessProbeExecutable(running) {
		return executableAccessUnknown, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, running, internalAccessProbeCommand, path, strconv.FormatUint(uint64(mode), 10), workingDirectory)
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	err = cmd.Run()
	if ctx.Err() != nil {
		return executableAccessUnknown, ctx.Err()
	}
	if err == nil {
		return executableAccessAllowed, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case accessProbeDeniedCode:
			return executableAccessDenied, nil
		case accessProbeUnknownCode:
			return executableAccessUnknown, nil
		}
	}
	return executableAccessUnknown, nil
}

func safeAccessProbeExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return false
	}
	_, err = unix.Getxattr(path, "security.capability", nil)
	return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
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
// unit for the boot of the scope in the mode the installation keeps. The reload
// always happens; the enable is only asked for a first installation or a
// registration that was enabled before, so a reinstall never re-enables a unit
// an administrator disabled.
//
// The mode is part of what is kept: a unit an administrator enabled at runtime
// ("systemctl enable --runtime", which survives no reboot) is enabled at runtime
// again, so a repeat installation neither makes that enablement permanent nor
// drops it. A masked unit is refused before this point, so it never reaches the
// reload.
func (m *systemdManager) reloadAndEnable(ctx context.Context, mode enablementMode) error {
	if _, err := m.runSystemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	switch mode {
	case enablementPersistent:
		if _, err := m.runSystemctl(ctx, "enable", unitName); err != nil {
			return err
		}
	case enablementRuntime:
		if _, err := m.runSystemctl(ctx, "enable", "--runtime", unitName); err != nil {
			return err
		}
	}
	return nil
}

// rollbackUnit restores the registration of a failed installation and checks
// both the file on disk and the captured command/launch properties systemd
// reported before the update. A successful file write or daemon-reload command
// alone does not prove those loaded properties returned to their prior values.
func (m *systemdManager) rollbackUnit(ctx context.Context, previous previousRegistration) error {
	// The caller may have cancelled the context, the rollback still has to
	// clean the registration up.
	cleanup := context.WithoutCancel(ctx)
	if previous.existed {
		if err := writeUnitFile(previous.path, string(previous.content)); err != nil {
			return fmt.Errorf("write the previous unit file back: %w", err)
		}
	} else {
		if err := os.Remove(previous.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove the failed unit file: %w", err)
		}
	}
	if err := verifyRestoredUnit(previous); err != nil {
		return err
	}
	if _, err := m.runSystemctl(cleanup, "daemon-reload"); err != nil {
		return fmt.Errorf("reload the restored unit: %w", err)
	}
	loaded, err := m.registrationUnitConfig(cleanup)
	if err != nil {
		return fmt.Errorf("read the restored loaded configuration: %w", err)
	}
	if previous.existed {
		if !previous.loadedKnown {
			return errors.New("the previous loaded configuration was not captured before the update")
		}
		if previous.loaded.loaded && !sameSystemdLoadedConfig(loaded, previous.loaded) {
			return errors.New("the supervisor did not load the previous command and captured launch settings")
		}
		if !previous.loaded.loaded {
			if !loaded.loaded {
				return errors.New("the restored unit remains unknown to systemd")
			}
			sourceContent, ok := previous.sourceUnitContent()
			if !ok {
				return errors.New("the source fragment of the previous unit was not captured")
			}
			if err := loadedMatchesUnit(loaded, parseUnit(string(sourceContent))); err != nil {
				return fmt.Errorf("the restored unit file is present but its command did not load: %w", err)
			}
		}
	} else {
		if !previous.loadedKnown {
			return errors.New("the prior loaded state was not captured before the update")
		}
		if previous.loaded.loaded {
			if !loaded.loaded || !sameSystemdLoadedConfig(loaded, previous.loaded) {
				return errors.New("systemd did not restore the loaded fragment after the override was removed")
			}
		} else if loaded.loaded {
			return errors.New("the failed unit is still loaded after its file was removed")
		}
	}
	return nil
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
// healthy registration. The state stays the diagnosis of the registration: the
// instance the supervisor started can still run, and Status.Running carries that
// fact, so a caller that has to know whether anything serves does not read the
// broken state as "nothing runs".
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

	// The state of the supervisor is read before the registration is judged:
	// the running fact answers a question of its own, and a registration
	// whose executable is gone can still have a live process. A supervisor
	// that cannot be reached is not a failure for a broken registration: the
	// registration is described by the unit file alone then.
	out, showErr := m.runSystemctl(ctx, "show", unitName,
		"--property=LoadState", "--property=ActiveState", "--property=SubState")
	show := systemdShow{State: StateUnknown}
	if showErr == nil {
		show = parseSystemdShow(out)
		status.Running = runningState(show.State)
	}

	if broken, detail := brokenExecutable(status.Executable); broken {
		status.State = StateBroken
		if status.Running {
			detail += brokenRunningDetail
		}
		status.Detail = detail
		return status, nil
	}

	if showErr != nil {
		status.State = StateUnknown
		status.Detail = showErr.Error()
		return status, showErr
	}
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
# updates its command while preserving other unit settings.
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
	workingDirectory, err := systemdWorkingDirectoryValue(opts.Paths.StateDir)
	if err != nil {
		return "", err
	}
	identity, err := unitIdentity(opts, account)
	if err != nil {
		return "", err
	}

	target := "default.target"
	if opts.Scope == ScopeSystem {
		target = "multi-user.target"
	}

	return fmt.Sprintf(unitTemplate,
		identity,
		systemdExecStart(opts),
		workingDirectory,
		systemdStopTimeout(),
		systemdQuote("HOME="+home),
		target,
	), nil
}

func systemdExecStart(opts InstallOptions) string {
	words := make([]string, 0, len(RunArgs(opts))+1)
	words = append(words, systemdQuoteCommandAlways(opts.Executable))
	for _, arg := range RunArgs(opts) {
		words = append(words, systemdQuoteCommand(arg))
	}
	return strings.Join(words, " ")
}

func systemdLoadedExecutablePath(executable string) string {
	words := splitSystemdWords(systemdQuoteCommandAlways(executable))
	if len(words) == 0 {
		return executable
	}
	return words[0]
}

type systemdDirectiveChange struct {
	value  string
	remove bool
}

// updateUnit changes only native directives that carry options explicitly
// named by an update. Every other line, including directives HFDesk does not
// interpret, stays intact.
func updateUnit(content string, opts InstallOptions, account *user.User) (string, error) {
	execStart, err := updateSystemdExecStartSource(content, opts)
	if err != nil {
		return "", err
	}
	changes := map[string]systemdDirectiveChange{
		"ExecStart": {value: execStart},
	}
	if opts.Explicit.StateDir {
		workingDirectory, err := systemdWorkingDirectoryValue(opts.Paths.StateDir)
		if err != nil {
			return "", err
		}
		changes["WorkingDirectory"] = systemdDirectiveChange{value: workingDirectory}
	}
	if opts.Explicit.ServiceUser {
		identity, err := unitIdentity(opts, account)
		if err != nil {
			return "", err
		}
		changes["User"] = systemdDirectiveChange{value: systemdQuote(opts.ServiceUser)}
		changes["Group"] = systemdDirectiveChange{remove: true}
		for _, line := range strings.Split(identity, "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				changes[key] = systemdDirectiveChange{value: value}
			}
		}
	}
	return updateSystemdServiceDirectives(content, changes)
}

func updateSystemdExecStartSource(content string, opts InstallOptions) (string, error) {
	section := ""
	for _, line := range systemdLogicalLines(content) {
		trimmed := strings.TrimSpace(line.text)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.ToLower(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
			continue
		}
		if section != "service" {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(key) != "ExecStart" {
			continue
		}
		tokens := splitSystemdWordTokens(strings.TrimSpace(value))
		if len(tokens) == 0 {
			return systemdExecStart(opts), nil
		}
		tokens[0] = systemdSourceWord(systemdCommandPrefix(tokens[0].source) + systemdQuoteCommandAlways(opts.Executable))
		for _, option := range []struct {
			name     string
			value    string
			explicit bool
			present  bool
		}{
			{"config", opts.Paths.ConfigFile, opts.Explicit.Config, opts.Paths.ConfigFile != ""},
			{"state-dir", opts.Paths.StateDir, opts.Explicit.StateDir, opts.Paths.StateDir != ""},
			{"log-dir", opts.Paths.LogDir, opts.Explicit.LogDir, opts.Paths.LogDir != ""},
			{"listen", listenArg(opts.Listen), opts.Explicit.Listen, listenArg(opts.Listen) != ""},
			{"cache-dir", opts.CacheDir, opts.Explicit.CacheDir, opts.CacheDir != ""},
			{"hub-dir", opts.HubDir, opts.Explicit.HubDir, opts.HubDir != ""},
			{"local-dir", opts.LocalDir, opts.Explicit.LocalDir, opts.LocalDir != ""},
		} {
			if !option.explicit {
				continue
			}
			tokens = replaceSystemdRunFlag(tokens, option.name, option.value, option.present)
		}
		words := make([]string, len(tokens))
		for i, token := range tokens {
			words[i] = token.source
		}
		return strings.Join(words, " "), nil
	}
	return systemdExecStart(opts), nil
}

func systemdSourceWord(source string) systemdWord {
	if words := splitSystemdWordTokens(source); len(words) == 1 {
		return words[0]
	}
	return systemdWord{source: source, value: source}
}

func systemdCommandPrefix(source string) string {
	for i := 0; i < len(source); i++ {
		if strings.ContainsRune("-@:+!", rune(source[i])) {
			continue
		}
		return source[:i]
	}
	return source
}

func replaceSystemdRunFlag(words []systemdWord, name, value string, present bool) []systemdWord {
	layout := scanSystemdRunArgs(words, true)
	occurrences := make(map[int]int)
	insertAt := layout.insertionPoint
	for _, option := range layout.options {
		if option.name == name {
			occurrences[option.start] = option.end
			insertAt = option.start
		}
	}
	var updated []systemdWord
	for i := 0; i < len(words); {
		if i == insertAt && present {
			updated = append(updated, systemdSourceWord("--"+name), systemdSourceWord(systemdQuoteCommand(value)))
		}
		if occurrence, ok := occurrences[i]; ok {
			i = occurrence
			continue
		}
		updated = append(updated, words[i])
		i++
	}
	if insertAt == len(words) && present {
		updated = append(updated, systemdSourceWord("--"+name), systemdSourceWord(systemdQuoteCommand(value)))
	}
	return updated
}

type systemdRunOption struct {
	name       string
	start, end int
	dynamic    bool
}

type systemdRunArgLayout struct {
	options        []systemdRunOption
	terminator     int
	insertionPoint int
}

// scanSystemdRunArgs follows the option/value consumption used by Go's
// flag.FlagSet: a separate option consumes its next word even when it is "--".
func scanSystemdRunArgs(words []systemdWord, executableIncluded bool) systemdRunArgLayout {
	start := 0
	if executableIncluded && len(words) > 0 {
		start = 1
	}
	if len(words) >= start+2 && words[start].value == "service" && words[start+1].value == "run" {
		start += 2
	} else if len(words) > start && words[start].value == "run" {
		start++
	}

	layout := systemdRunArgLayout{terminator: -1, insertionPoint: len(words)}
	for i := start; i < len(words); {
		if words[i].value == "--" {
			layout.terminator = i
			layout.insertionPoint = i
			return layout
		}
		name, hasInlineValue, ok := parseSystemdRunOption(words[i].value)
		if !ok {
			layout.insertionPoint = i
			return layout
		}
		end := i + 1
		dynamic := words[i].dynamic
		if !hasInlineValue {
			if end == len(words) {
				layout.options = append(layout.options, systemdRunOption{name: name, start: i, end: end, dynamic: dynamic})
				return layout
			}
			dynamic = words[end].dynamic
			end++
		}
		layout.options = append(layout.options, systemdRunOption{name: name, start: i, end: end, dynamic: dynamic})
		i = end
	}
	return layout
}

func parseSystemdRunOption(word string) (name string, hasInlineValue, ok bool) {
	if len(word) < 2 || word[0] != '-' {
		return "", false, false
	}
	nameStart := 1
	if word[1] == '-' {
		if len(word) == 2 {
			return "", false, false
		}
		nameStart = 2
	}
	if word[nameStart] == '-' || word[nameStart] == '=' {
		return "", false, false
	}
	name, _, hasInlineValue = strings.Cut(word[nameStart:], "=")
	if !systemdRunFlag(name) {
		return "", false, false
	}
	return name, hasInlineValue, true
}

func updateSystemdServiceDirectives(content string, changes map[string]systemdDirectiveChange) (string, error) {
	lines := strings.Split(content, "\n")
	logicalLines := systemdLogicalLines(content)
	section := ""
	serviceHeader := -1
	counts := make(map[string]int, len(changes))
	for _, line := range logicalLines {
		trimmed := strings.TrimSpace(line.text)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.ToLower(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
			if section == "service" && serviceHeader < 0 {
				serviceHeader = line.start
			}
			continue
		}
		if section != "service" || trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		key, _, ok := strings.Cut(trimmed, "=")
		key = strings.TrimSpace(key)
		if _, changing := changes[key]; ok && changing {
			counts[key]++
		}
	}
	if serviceHeader < 0 {
		return "", errors.New("service: the existing unit has no [Service] section to update")
	}
	if counts["ExecStart"] > 1 {
		return "", errors.New("service: the existing unit has multiple ExecStart directives; refusing to replace an ambiguous command")
	}

	ordered := []string{"ExecStart", "WorkingDirectory", "User", "Group"}
	var updated []string
	section = ""
	appendOriginal := func(line systemdLogicalLine) {
		updated = append(updated, lines[line.start:line.end+1]...)
	}
	for _, line := range logicalLines {
		trimmed := strings.TrimSpace(line.text)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.ToLower(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
			appendOriginal(line)
			if line.start == serviceHeader {
				for _, key := range ordered {
					change, changing := changes[key]
					if changing && !change.remove && counts[key] == 0 {
						updated = append(updated, key+"="+change.value)
					}
				}
			}
			continue
		}
		if section == "service" {
			key, _, ok := strings.Cut(trimmed, "=")
			key = strings.TrimSpace(key)
			if change, changing := changes[key]; ok && changing {
				if change.remove {
					continue
				}
				updated = append(updated, replaceUnitDirective(lines[line.start], key, change.value))
				continue
			}
		}
		appendOriginal(line)
	}
	return strings.Join(updated, "\n"), nil
}

func replaceUnitDirective(line, key, value string) string {
	ending := ""
	if strings.HasSuffix(line, "\r") {
		ending = "\r"
		line = strings.TrimSuffix(line, "\r")
	}
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	return indent + key + "=" + value + ending
}

// validateUnitPaths checks paths whose source bytes the operation will emit
// into the unit. A first install writes every option; an update writes the
// executable and only the explicitly named path operands.
//
// Unit-file controls can split newly written directives. An emitted
// WorkingDirectory is also checked using its native value rules; an inherited
// app state path is not that directive.
func validateUnitPaths(opts InstallOptions, existing bool) error {
	for _, p := range []struct {
		name     string
		path     string
		explicit bool
	}{
		{"executable", opts.Executable, true},
		{"config file", opts.Paths.ConfigFile, opts.Explicit.Config},
		{"state dir", opts.Paths.StateDir, opts.Explicit.StateDir},
		{"log dir", opts.Paths.LogDir, opts.Explicit.LogDir},
		{"cache dir", opts.CacheDir, opts.Explicit.CacheDir},
		{"hub dir", opts.HubDir, opts.Explicit.HubDir},
		{"local dir", opts.LocalDir, opts.Explicit.LocalDir},
	} {
		if p.path == "" || existing && !p.explicit {
			continue
		}
		if r, ok := controlCharacter(p.path); ok {
			return fmt.Errorf("service: the %s %q contains the control character %q: a path of an installation must not contain a control character", p.name, p.path, r)
		}
	}
	if !existing || opts.Explicit.StateDir {
		if _, err := systemdWorkingDirectoryValue(opts.Paths.StateDir); err != nil {
			return err
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

// systemdWorkingDirectoryValue leaves path bytes intact; unlike ExecStart,
// WorkingDirectory does not perform C-style unescaping. Only literal percent
// signs need escaping from native systemd specifier expansion. A final odd
// backslash gets a separator space so it cannot continue the unit-file line.
func systemdWorkingDirectoryValue(value string) (string, error) {
	if strings.HasSuffix(value, " ") {
		return "", fmt.Errorf("service: the state dir %q cannot be represented by systemd WorkingDirectory because trailing spaces are stripped", value)
	}
	value = strings.ReplaceAll(value, "%", "%%")
	backslashes := 0
	for i := len(value) - 1; i >= 0 && value[i] == '\\'; i-- {
		backslashes++
	}
	if backslashes%2 != 0 {
		// Keep an odd final backslash from continuing the unit-file line.
		value += " "
	}
	return value, nil
}

func parseSystemdWorkingDirectory(value string) (string, bool) {
	var b strings.Builder
	dynamic := false
	for i := 0; i < len(value); {
		if value[i] == '%' && i+1 < len(value) {
			if value[i+1] == '%' {
				b.WriteByte('%')
				i += 2
				continue
			}
			dynamic = true
		}
		b.WriteByte(value[i])
		i++
	}
	return b.String(), dynamic
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
	// ExecStartRaw and ExecArgsRaw retain the systemd-parsed, pre-environment-
	// expansion values reported by D-Bus. The resolved fields below are for
	// interpreting the options the running process receives.
	ExecStartRaw string
	ExecArgsRaw  []string
	// Dynamic marks source words that systemd expands as specifiers or
	// environment references. Those source expressions are kept separate from
	// the manager's typed loaded values.
	ExecStartDynamic bool
	ExecArgsDynamic  []bool
	DynamicRunArgs   map[string]bool
	// ExecArgs are the arguments of the executable.
	ExecArgs []string
	// WorkingDirectory is the working directory of the unit.
	WorkingDirectory        string
	WorkingDirectoryDynamic bool
	// User is the account a machine-wide unit runs as. A user scope unit
	// names no account, so the value is empty there.
	User  string
	Group string
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
	return runArgValue(u.ExecArgs, flag)
}

// parseUnit reads the registration facts of a unit file.
//
// The parser is tolerant: a unit file that was edited by hand is described
// as far as it can be read, so the status of the registration stays
// meaningful.
func parseUnit(content string) parsedUnit {
	unit := parsedUnit{Environment: map[string]string{}}
	section := ""
	for _, logical := range systemdLogicalLines(content) {
		line := logical.text
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
			tokens := splitSystemdWordTokens(value)
			rawWords := make([]string, len(tokens))
			for i, token := range tokens {
				rawWords[i] = token.value
			}
			if len(rawWords) == 0 {
				continue
			}
			words := resolveSystemdDollars(slices.Clone(rawWords))
			unit.ExecStartRaw = rawWords[0]
			unit.ExecArgsRaw = append([]string(nil), rawWords[1:]...)
			unit.ExecStartDynamic = tokens[0].dynamic
			unit.ExecArgsDynamic = make([]bool, len(tokens)-1)
			for i := 1; i < len(tokens); i++ {
				unit.ExecArgsDynamic[i-1] = tokens[i].dynamic
			}
			unit.DynamicRunArgs = dynamicRunArgValues(tokens[1:])
			unit.ExecStart = words[0]
			unit.ExecArgs = words[1:]
		case "WorkingDirectory":
			unit.WorkingDirectory, unit.WorkingDirectoryDynamic = parseSystemdWorkingDirectory(value)
		case "User":
			if words := splitSystemdWordTokens(value); len(words) > 0 {
				unit.User = words[0].value
			}
		case "Group":
			if words := splitSystemdWordTokens(value); len(words) > 0 {
				unit.Group = words[0].value
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
	if unit.WorkingDirectoryDynamic {
		if _, hasStateArg := unit.DynamicRunArgs["state-dir"]; !hasStateArg {
			if unit.DynamicRunArgs == nil {
				unit.DynamicRunArgs = make(map[string]bool)
			}
			unit.DynamicRunArgs["state-dir"] = true
		}
	}
	return unit
}

type systemdLogicalLine struct {
	text       string
	start, end int
}

// systemdLogicalLines joins the physical lines systemd continues before it
// parses assignments. start/end retain the original span so an updated
// directive can replace its whole continued value without leaving orphaned
// physical lines behind.
func systemdLogicalLines(content string) []systemdLogicalLine {
	physical := strings.Split(content, "\n")
	logical := make([]systemdLogicalLine, 0, len(physical))
	for start := 0; start < len(physical); {
		line := strings.TrimSuffix(physical[start], "\r")
		end := start
		for hasSystemdLineContinuation(line) {
			line = line[:len(line)-1] + " "
			next := end + 1
			for next < len(physical) && isSystemdCommentLine(physical[next]) {
				next++
			}
			if next >= len(physical) {
				end = len(physical) - 1
				break
			}
			line += strings.TrimSuffix(physical[next], "\r")
			end = next
		}
		logical = append(logical, systemdLogicalLine{text: line, start: start, end: end})
		start = end + 1
	}
	return logical
}

func isSystemdCommentLine(line string) bool {
	trimmed := strings.TrimLeft(line, " \t\r\n")
	return strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";")
}

func hasSystemdLineContinuation(line string) bool {
	backslashes := 0
	for i := len(line) - 1; i >= 0 && line[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
}

type systemdWord struct {
	source  string
	value   string
	dynamic bool
}

// splitSystemdWords implements systemd's ExecStart word boundary: whitespace
// separates unquoted words, quotes group a word, and C escapes are decoded
// without re-splitting the escaped result. Unknown escapes remain literal, as
// systemd's relaxed syntax path does after warning.
func splitSystemdWords(line string) []string {
	tokens := splitSystemdWordTokens(line)
	words := make([]string, len(tokens))
	for i, token := range tokens {
		words[i] = token.value
	}
	return words
}

func splitSystemdWordTokens(line string) []systemdWord {
	var (
		words     []systemdWord
		word      strings.Builder
		expansion strings.Builder
		start     = -1
		quote     byte
	)
	flush := func(end int) {
		if start < 0 {
			return
		}
		words = append(words, systemdWord{
			source:  line[start:end],
			value:   word.String(),
			dynamic: hasSystemdExpansion(expansion.String()),
		})
		word.Reset()
		expansion.Reset()
		start = -1
	}
	for i := 0; i < len(line); {
		c := line[i]
		if c == '\\' {
			if start < 0 {
				start = i
			}
			value, next := unescapeSystemdSequence(line, i)
			word.WriteString(value)
			expansion.WriteString(value)
			i = next
			continue
		}
		if c == '%' && i+1 < len(line) && line[i+1] == '%' {
			if start < 0 {
				start = i
			}
			word.WriteByte('%')
			expansion.WriteString("%%")
			i += 2
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				word.WriteByte(c)
				expansion.WriteByte(c)
			}
			i++
			continue
		}
		switch c {
		case '\'', '"':
			if start < 0 {
				start = i
			}
			quote = c
		case ' ', '\t', '\n', '\r':
			flush(i)
		default:
			if start < 0 {
				start = i
			}
			word.WriteByte(c)
			expansion.WriteByte(c)
		}
		i++
	}
	flush(len(line))
	return words
}

func hasSystemdExpansion(value string) bool {
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '$':
			if i+1 < len(value) && value[i+1] == '$' {
				i++
				continue
			}
			if i+1 < len(value) && (value[i+1] == '{' || isSystemdVariableNameStart(value[i+1])) {
				return true
			}
		case '%':
			if i+1 < len(value) && value[i+1] == '%' {
				i++
				continue
			}
			if i+1 < len(value) {
				return true
			}
		}
	}
	return false
}

func isSystemdVariableNameStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func dynamicRunArgValues(args []systemdWord) map[string]bool {
	values := make(map[string]bool)
	for _, option := range scanSystemdRunArgs(args, false).options {
		values[option.name] = option.dynamic
	}
	return values
}

func systemdRunFlag(name string) bool {
	switch name {
	case "config", "state-dir", "log-dir", "listen", "cache-dir", "hub-dir", "local-dir":
		return true
	default:
		return false
	}
}

func unescapeSystemdSequence(line string, slash int) (string, int) {
	if slash+1 >= len(line) {
		return "\\", slash + 1
	}
	code := line[slash+1]
	switch code {
	case 'a':
		return "\a", slash + 2
	case 'b':
		return "\b", slash + 2
	case 'f':
		return "\f", slash + 2
	case 'n':
		return "\n", slash + 2
	case 'r':
		return "\r", slash + 2
	case 't':
		return "\t", slash + 2
	case 'v':
		return "\v", slash + 2
	case '\\':
		return "\\", slash + 2
	case '"':
		return `"`, slash + 2
	case '\'':
		return "'", slash + 2
	case 's':
		return " ", slash + 2
	case 'x':
		if slash+4 <= len(line) {
			value, err := strconv.ParseUint(line[slash+2:slash+4], 16, 8)
			if err == nil && value != 0 {
				return string([]byte{byte(value)}), slash + 4
			}
		}
	case 'u':
		if slash+6 <= len(line) {
			value, err := strconv.ParseUint(line[slash+2:slash+6], 16, 16)
			if err == nil && value != 0 && value <= utf8.MaxRune && utf8.ValidRune(rune(value)) {
				return string(rune(value)), slash + 6
			}
		}
	case 'U':
		if slash+10 <= len(line) {
			value, err := strconv.ParseUint(line[slash+2:slash+10], 16, 32)
			if err == nil {
				if r, valid := systemdUnicode32EscapeRune(value); valid {
					return string(r), slash + 10
				}
			}
		}
	default:
		if code >= '0' && code <= '7' && slash+4 <= len(line) {
			value, err := strconv.ParseUint(line[slash+1:slash+4], 8, 8)
			if err == nil && value != 0 {
				return string([]byte{byte(value)}), slash + 4
			}
		}
	}
	return line[slash : slash+2], slash + 2
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
