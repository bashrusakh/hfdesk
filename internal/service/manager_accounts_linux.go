// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

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

// accountValidationOptions returns the options whose account an installation
// validates.
//
// An update of an existing registration that named no account validates the
// account the supervisor really loads: the User= of a unit may come from a
// drop-in the unit source does not contain, and that loaded identity — not
// the default account of the platform, which the machine may never have
// created — is what the instance runs as. An account the request named
// explicitly is validated exactly as requested, and a first installation
// validates what its request carries, so neither path changes here.
//
// The loaded configuration is read best-effort: when the supervisor cannot
// answer, the account stays what the request and the unit source say, which
// is the behavior of the registration without a readable loaded identity.
func (m *systemdManager) accountValidationOptions(ctx context.Context, requested, opts InstallOptions, registered bool) InstallOptions {
	if m.scope != ScopeSystem || !registered || requested.Explicit.ServiceUser {
		return opts
	}
	loaded, err := m.registrationUnitConfig(ctx)
	if err != nil || !loaded.loaded || loaded.user == "" {
		return opts
	}
	validation := opts
	validation.ServiceUser = loaded.user
	return validation
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
	if !previous.existed || !previous.loaded.dropInPathsKnown || len(previous.loaded.dropInPaths) > 0 || systemdUnitHasEnvironmentFiles(previous.content) || systemdUnitHasUnmodeledLaunchDirective(previous.content) || len(previous.loaded.environmentFiles) > 0 {
		return false
	}

	unit := parseUnit(string(previous.content))
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
