// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/godbus/dbus/v5"
)

// systemdSecondAccount returns an account of the machine that is not the
// account of the test process.
//
// The tests of the service account need an account that is not the owner of
// the directories of the test: only then a directory the account cannot
// write into is a directory it really cannot write into.
func systemdSecondAccount(t *testing.T) *user.User {
	t.Helper()
	current := systemdCurrentAccount(t)
	for _, name := range []string{"nobody", "daemon", "bin", "sys", "games"} {
		account, err := user.Lookup(name)
		if err != nil {
			continue
		}
		if account.Uid == current.Uid {
			continue
		}
		return account
	}
	t.Skip("skipping: the machine has no second account the test could use")
	return nil
}

// systemdCurrentAccount returns the account of the test process.
func systemdCurrentAccount(t *testing.T) *user.User {
	t.Helper()
	account, err := user.Current()
	if err != nil {
		t.Skipf("skipping: the account of the test process is unknown: %v", err)
	}
	return account
}

// systemdAccountsOfTheMachine returns the accounts of the machine.
//
// The accounts are read from the password database of the machine, so the
// test works on every machine and needs no privilege.
func systemdAccountsOfTheMachine(t *testing.T) []*user.User {
	t.Helper()
	content, err := os.ReadFile("/etc/passwd")
	if err != nil {
		t.Skipf("skipping: the password database of the machine cannot be read: %v", err)
	}
	var accounts []*user.User
	for _, line := range strings.Split(string(content), "\n") {
		name, _, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || name == "" {
			continue
		}
		account, err := user.Lookup(name)
		if err != nil {
			continue
		}
		accounts = append(accounts, account)
	}
	if len(accounts) == 0 {
		t.Skip("skipping: the machine knows no account the test could use")
	}
	return accounts
}

// systemdAccountWithAnotherPrimaryGroup returns an account of the machine
// whose primary group carries another name than the account itself.
//
// Such an account is the case that shows the difference between the primary
// group of an account and the name of the account. The second result is
// false when the machine knows no such account.
func systemdAccountWithAnotherPrimaryGroup(t *testing.T) (*user.User, bool) {
	t.Helper()
	for _, account := range systemdAccountsOfTheMachine(t) {
		group, err := user.LookupGroupId(account.Gid)
		if err != nil || group.Name == "" {
			continue
		}
		if group.Name != account.Username {
			return account, true
		}
	}
	return nil, false
}

// systemdUnresolvableAccount returns an account whose primary group the
// machine does not know.
//
// The group of a unit is never guessed: an account whose group cannot be
// resolved leaves the group out of the unit, so the supervisor uses the
// default group of the account.
func systemdUnresolvableAccount(t *testing.T) *user.User {
	t.Helper()
	for _, gid := range []string{"4294967294", "999999999", "999999998"} {
		if _, err := user.LookupGroupId(gid); err == nil {
			continue
		}
		return &user.User{
			Username: "hfdesk-svc",
			Uid:      "999999999",
			Gid:      gid,
			Name:     "HFDesk service account",
			HomeDir:  systemServiceHome,
		}
	}
	t.Skip("skipping: the machine knows a group for every id the test could use")
	return nil
}

// systemdOwner returns the owner and the group of a path.
func systemdOwner(t *testing.T, path string) (string, string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skipf("skipping: the file system of the test does not report an owner: %v", info.Sys())
	}
	return strconv.Itoa(int(stat.Uid)), strconv.Itoa(int(stat.Gid))
}

// systemdTraversableTempDir returns a temporary directory the account of the
// test can pass through.
//
// The temporary directory of a test is owner-only, so an account that is not
// the owner of the test never reaches a directory below it. The access check
// of the service layer asks for the same: a directory below a parent
// without the execute permission is out of reach of the account.
func systemdTraversableTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// The mode of a create call is filtered by the umask of the process,
	// so the mode is set explicitly.
	for _, path := range []string{root, filepath.Dir(root)} {
		if err := os.Chmod(path, 0o711); err != nil {
			t.Fatalf("make %q traversable: %v", path, err)
		}
	}
	return root
}

// systemdSupplementaryGroup returns the id of a supplementary group of the
// account of the test process.
//
// The groups of an account are read from the group database of the machine,
// so the test needs a group whose member list holds the account of the
// test. The second result is false when the machine knows no such group.
func systemdSupplementaryGroup(t *testing.T) (string, bool) {
	t.Helper()
	current := systemdCurrentAccount(t)
	content, err := os.ReadFile("/etc/group")
	if err != nil {
		t.Skipf("skipping: the group database of the machine cannot be read: %v", err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) < 4 {
			continue
		}
		if fields[2] == current.Gid {
			// The primary group of the account is no supplementary
			// group of it.
			continue
		}
		for _, member := range strings.Split(fields[3], ",") {
			if member == current.Username {
				return fields[2], true
			}
		}
	}
	return "", false
}

// TestSystemdServiceAccountName checks how the account of a machine-wide
// instance is resolved: the account the user named wins, and an empty value
// falls back to the default of the platform.
func TestSystemdServiceAccountName(t *testing.T) {
	t.Run("the account of the user wins", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)
		opts.ServiceUser = "hfdesk-svc"

		if got := serviceAccountName(opts); got != "hfdesk-svc" {
			t.Errorf("serviceAccountName() = %q, want %q", got, "hfdesk-svc")
		}
	})

	t.Run("an empty account falls back to the default of the platform", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)
		opts.ServiceUser = ""

		got := serviceAccountName(opts)
		if want := DefaultServiceUser(); got != want {
			t.Errorf("serviceAccountName() = %q, want %q", got, want)
		}
	})
}

// TestSystemdInstallRefusesAMissingServiceAccount checks that a
// machine-wide installation whose account does not exist is refused with the
// guidance that names the account and the command that creates it, and that
// the refused installation leaves nothing behind.
func TestSystemdInstallRefusesAMissingServiceAccount(t *testing.T) {
	const missing = "hfdesk-missing-service-account"
	if _, err := user.Lookup(missing); err == nil {
		t.Skipf("skipping: the account %q exists on this machine", missing)
	}

	// The home of the user is a temporary directory, so the guard of the
	// machine-wide scope does not refuse the paths of the test.
	home := t.TempDir()
	t.Setenv("HOME", home)

	m := &systemdManager{scope: ScopeSystem}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	// The unit file of the machine scope lives in a fixed place of the
	// machine. The test never writes it: the refusal happens before the
	// unit file is written. The state of the file is compared before and
	// after the installation, so a unit that is installed already is never
	// touched by the test.
	before, existedBefore, err := readUnitFile(path)
	if err != nil {
		t.Logf("the unit file %q cannot be read: %v", path, err)
	}

	root := t.TempDir()
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
		ServiceUser: missing,
	}

	_, err = m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the missing service account")
	}
	want := fmt.Sprintf("service: the machine-wide instance needs the service account %q: create it with %q and install again (HFDesk never creates accounts itself)",
		missing, serviceAccountCreateCommand(missing))
	if err.Error() != want {
		t.Errorf("Install() = %q, want %q", err, want)
	}
	if !strings.Contains(err.Error(), "useradd --system --no-create-home") {
		t.Errorf("Install() = %q, want the command that creates the account", err)
	}

	// The refused installation writes no unit file.
	after, existedAfter, err := readUnitFile(path)
	if err != nil {
		t.Logf("the unit file %q cannot be read: %v", path, err)
	}
	if existedBefore != existedAfter || string(before) != string(after) {
		t.Errorf("the refused installation changed the unit file %q", path)
	}
	// The refused installation creates no directory of the instance
	// either.
	for _, dir := range instanceDirs(opts) {
		if _, err := os.Stat(dir.path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused installation created the %s %q: %v", dir.name, dir.path, err)
		}
	}
}

// TestSystemdAccountValidationUsesTheLoadedIdentityOfAnExistingUnit checks
// which account an update of an existing registration validates: the account
// the supervisor really loads — a drop-in may provide User= although the
// unit source names none, and the default account of the platform may not
// exist on the machine — while an explicitly named account, a first
// installation, a user scope and an unreadable loaded identity all keep the
// account of the request.
func TestSystemdAccountValidationUsesTheLoadedIdentityOfAnExistingUnit(t *testing.T) {
	systemdRecordingSupervisor(t)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", systemdTestExecutable(t))
	exe := os.Getenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE")
	current, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current() = %v", err)
	}

	for _, tc := range []struct {
		name        string
		scope       Scope
		registered  bool
		serviceUser string
		explicit    bool
		loadedUser  string
		loadedErr   error
		want        string
		wantReads   int
	}{
		{
			name:       "an existing unit validates the loaded account",
			scope:      ScopeSystem,
			registered: true,
			loadedUser: current.Username,
			want:       current.Username,
			wantReads:  1,
		},
		{
			name:        "an explicitly named account is validated as requested",
			scope:       ScopeSystem,
			registered:  true,
			serviceUser: "hfdesk-named-account",
			explicit:    true,
			loadedUser:  current.Username,
			want:        "hfdesk-named-account",
			wantReads:   0,
		},
		{
			name:       "a first installation keeps the request",
			scope:      ScopeSystem,
			registered: false,
			loadedUser: current.Username,
			want:       "",
			wantReads:  0,
		},
		{
			name:       "an unreadable loaded identity keeps the request",
			scope:      ScopeSystem,
			registered: true,
			loadedErr:  errors.New("the supervisor does not answer"),
			want:       "",
			wantReads:  1,
		},
		{
			name:       "a loaded identity without an account keeps the request",
			scope:      ScopeSystem,
			registered: true,
			loadedUser: "",
			want:       "",
			wantReads:  1,
		},
		{
			name:       "a user scope validates no service account",
			scope:      ScopeUser,
			registered: true,
			loadedUser: current.Username,
			want:       "",
			wantReads:  0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			m := &systemdManager{
				scope: tc.scope,
				readServiceProperties: func(context.Context, string) (map[string]dbus.Variant, error) {
					reads++
					if tc.loadedErr != nil {
						return nil, tc.loadedErr
					}
					return systemdTypedProperties(exe, []string{exe}, nil, "", tc.loadedUser, "", nil), nil
				},
			}
			requested := InstallOptions{Scope: tc.scope, Executable: exe}
			requested.ServiceUser = tc.serviceUser
			requested.Explicit.ServiceUser = tc.explicit
			// The merged options carry the named account when the request
			// named one, and the unit source's account (none here) when it
			// did not — the drop-in case of the defect.
			opts := requested
			if !tc.explicit {
				opts.ServiceUser = ""
			}

			got := m.accountValidationOptions(context.Background(), requested, opts, tc.registered)
			if got.ServiceUser != tc.want {
				t.Errorf("accountValidationOptions().ServiceUser = %q, want %q", got.ServiceUser, tc.want)
			}
			if reads != tc.wantReads {
				t.Errorf("the loaded configuration was read %d times, want %d", reads, tc.wantReads)
			}
		})
	}
}

// TestSystemdGrantInstanceDirs checks that the directories an installation
// creates belong to the service account of the instance, and that the
// installation never changes the ownership of a tree it does not own.
//
// Only root can change the ownership of a directory, so the test skips
// itself when it does not run as root.
func TestSystemdGrantInstanceDirs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("skipping: only root can change the ownership of a directory")
	}
	account := systemdSecondAccount(t)

	root := t.TempDir()
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
	}

	// The directories of the instance do not exist yet: the installation
	// creates them and grants them to the account.
	created := make(map[string][]string, 3)
	for _, dir := range instanceDirs(opts) {
		created[dir.path] = createdDirs(dir.path)
	}
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}

	// A cache tree of the instance is not owned by the installation: its
	// ownership has to stay the one it had.
	cache := filepath.Join(root, "cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatalf("create the cache directory: %v", err)
	}
	cacheOwner, cacheGroup := systemdOwner(t, cache)

	m := &systemdManager{scope: ScopeSystem}
	if err := m.grantInstanceDirs(opts, account, created); err != nil {
		t.Fatalf("grantInstanceDirs() = %v, want nil", err)
	}

	// Every directory the installation created belongs to the account: the
	// parents it created for the directories of the instance belong to it
	// as well, because the instance has to reach its own directories
	// through them.
	for _, dir := range instanceDirs(opts) {
		for _, path := range created[dir.path] {
			uid, gid := systemdOwner(t, path)
			if uid != account.Uid {
				t.Errorf("the owner of the %s %q = %q, want the service account %q", dir.name, path, uid, account.Uid)
			}
			if gid != account.Gid {
				t.Errorf("the group of the %s %q = %q, want the group %q of the service account", dir.name, path, gid, account.Gid)
			}
		}
	}
	if uid, gid := systemdOwner(t, cache); uid != cacheOwner || gid != cacheGroup {
		t.Errorf("the ownership of the cache directory %q = %q:%q, want %q:%q", cache, uid, gid, cacheOwner, cacheGroup)
	}
}

// TestSystemdGrantInstanceDirsRefusesAPreExistingDirectory checks that a
// directory that existed before the installation is not chowned.
//
// The directory belongs to whoever owns it, so the installation leaves it
// alone. The service account still has to be able to write there, so an
// installation that cannot do that is refused with the guidance that names
// the directory and the account.
func TestSystemdGrantInstanceDirsRefusesAPreExistingDirectory(t *testing.T) {
	account := systemdSecondAccount(t)

	root := t.TempDir()
	// The state directory exists before the installation and belongs to
	// the user of the test: the service account cannot write into it.
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	stateOwner, stateGroup := systemdOwner(t, state)

	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   state,
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
	}

	// The state directory existed before the installation, the other two
	// directories are created by it.
	created := make(map[string][]string, 3)
	for _, dir := range instanceDirs(opts) {
		created[dir.path] = createdDirs(dir.path)
	}
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	config := filepath.Dir(opts.Paths.ConfigFile)
	configOwner, configGroup := systemdOwner(t, config)

	m := &systemdManager{scope: ScopeSystem}
	err := m.grantInstanceDirs(opts, account, created)
	if err == nil {
		t.Fatal("grantInstanceDirs() = nil, want the refusal of a pre-existing directory")
	}
	want := fmt.Sprintf("service: the state dir %q of the machine-wide instance is not writable by the service account %q: grant it to the account (for example %q) and install again",
		state, account.Username, "chown "+account.Username+" "+state)
	if err.Error() != want {
		t.Errorf("grantInstanceDirs() = %q, want %q", err, want)
	}

	// The directory that existed before the installation still belongs to
	// its owner: it was not chowned.
	if uid, gid := systemdOwner(t, state); uid != stateOwner || gid != stateGroup {
		t.Errorf("the ownership of the state directory %q = %q:%q, want %q:%q", state, uid, gid, stateOwner, stateGroup)
	}
	// The refusal happened before the first ownership was changed, so the
	// directory the installation would have granted is untouched as well.
	if uid, gid := systemdOwner(t, config); uid != configOwner || gid != configGroup {
		t.Errorf("the ownership of the config directory %q = %q:%q, want %q:%q", config, uid, gid, configOwner, configGroup)
	}
}

// TestSystemdGrantInstanceDirsKeepsAWritablePreExistingDirectory checks that
// a directory that existed before the installation and that the service
// account can write into is accepted and left alone.
//
// The directory belongs to whoever owns it, so the installation grants it to
// nobody: it only makes sure that the account can work there.
func TestSystemdGrantInstanceDirsKeepsAWritablePreExistingDirectory(t *testing.T) {
	account := systemdSecondAccount(t)

	root := systemdTraversableTempDir(t)
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "state"),
			LogDir:     filepath.Join(root, "logs"),
		},
	}
	// Every directory of the instance exists before the installation and
	// is writable by the account.
	owners := map[string][2]string{}
	for _, dir := range instanceDirs(opts) {
		if err := os.MkdirAll(dir.path, 0o700); err != nil {
			t.Fatalf("create the %s %q: %v", dir.name, dir.path, err)
		}
		// The mode of a create call is filtered by the umask of the
		// process, so the mode is set explicitly.
		if err := os.Chmod(dir.path, 0o777); err != nil {
			t.Fatalf("make the %s %q writable: %v", dir.name, dir.path, err)
		}
		uid, gid := systemdOwner(t, dir.path)
		owners[dir.path] = [2]string{uid, gid}
	}

	created := make(map[string][]string, 3)
	for _, dir := range instanceDirs(opts) {
		created[dir.path] = createdDirs(dir.path)
	}

	m := &systemdManager{scope: ScopeSystem}
	if err := m.grantInstanceDirs(opts, account, created); err != nil {
		t.Fatalf("grantInstanceDirs() = %v, want nil for a pre-existing directory the account can write into", err)
	}

	// The directories still belong to their owner: the installation grants
	// only the directories it created itself.
	for _, dir := range instanceDirs(opts) {
		uid, gid := systemdOwner(t, dir.path)
		want := owners[dir.path]
		if uid != want[0] || gid != want[1] {
			t.Errorf("the ownership of the %s %q = %q:%q, want %q:%q", dir.name, dir.path, uid, gid, want[0], want[1])
		}
	}
}

// TestSystemdInstallRefusesAPreExistingDirectory checks that a machine-wide
// installation whose state directory existed before the installation is
// refused with the guidance that names the directory and the account, and
// that the refused installation never changes the ownership of that
// directory.
//
// The installation needs an elevated shell, so the test skips itself when it
// does not run as root.
func TestSystemdInstallRefusesAPreExistingDirectory(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("skipping: a machine-wide installation needs root")
	}
	account := systemdSecondAccount(t)

	// The home of the user is a temporary directory, so the guard of the
	// machine-wide scope does not refuse the paths of the test.
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := t.TempDir()
	// The state directory exists before the installation and belongs to
	// the user of the test: the service account cannot write into it.
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	stateOwner, stateGroup := systemdOwner(t, state)

	m := &systemdManager{scope: ScopeSystem}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	// The unit file of the machine scope lives in a fixed place of the
	// machine. The test never writes it: the refusal happens before the
	// unit file is written. The state of the file is compared before and
	// after the installation, so a unit that is installed already is never
	// touched by the test.
	before, existedBefore, err := readUnitFile(path)
	if err != nil {
		t.Logf("the unit file %q cannot be read: %v", path, err)
	}

	opts := InstallOptions{
		Scope:       ScopeSystem,
		Executable:  systemdTestExecutable(t),
		Listen:      DefaultListen,
		ServiceUser: account.Username,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   state,
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
	}

	_, err = m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the pre-existing state directory")
	}
	want := fmt.Sprintf("service: the state dir %q of the machine-wide instance is not writable by the service account %q: grant it to the account (for example %q) and install again",
		state, account.Username, "chown "+account.Username+" "+state)
	if err.Error() != want {
		t.Errorf("Install() = %q, want %q", err, want)
	}

	// The refused installation never changes the ownership of the
	// directory that existed before it.
	if uid, gid := systemdOwner(t, state); uid != stateOwner || gid != stateGroup {
		t.Errorf("the ownership of the state directory %q = %q:%q, want %q:%q", state, uid, gid, stateOwner, stateGroup)
	}
	// The refused installation writes no unit file.
	after, existedAfter, err := readUnitFile(path)
	if err != nil {
		t.Logf("the unit file %q cannot be read: %v", path, err)
	}
	if existedBefore != existedAfter || string(before) != string(after) {
		t.Errorf("the refused installation changed the unit file %q", path)
	}
}

// TestSystemdGrantInstanceDirsRefusesADirectoryOfTheMachine checks that the
// installation never hands a directory of the machine to the account of the
// instance.
//
// The directory of the config file is derived from the path of a file, so a
// config file in /etc would hand /etc itself to the account. The refusal
// happens before the first directory is granted, so no ownership changes at
// all.
func TestSystemdGrantInstanceDirsRefusesADirectoryOfTheMachine(t *testing.T) {
	account := systemdSecondAccount(t)

	root := t.TempDir()
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			// The config file lies directly in a top level directory
			// of the machine: its directory is /etc.
			ConfigFile: "/etc/hfdesk.json",
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
	}
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	stateOwner, stateGroup := systemdOwner(t, opts.Paths.StateDir)

	m := &systemdManager{scope: ScopeSystem}
	err := m.grantInstanceDirs(opts, account, nil)
	if err == nil {
		t.Fatal("grantInstanceDirs() = nil, want the refusal of a directory of the machine")
	}
	want := `service: the config dir "/etc" is a directory of the machine: the instance needs a directory of its own, name one below a directory of the machine`
	if err.Error() != want {
		t.Errorf("grantInstanceDirs() = %q, want %q", err, want)
	}

	// The refusal granted nothing: the state directory still belongs to
	// the account it belonged to.
	if uid, gid := systemdOwner(t, opts.Paths.StateDir); uid != stateOwner || gid != stateGroup {
		t.Errorf("the ownership of the state directory %q = %q:%q, want %q:%q", opts.Paths.StateDir, uid, gid, stateOwner, stateGroup)
	}
}

// TestSystemdCheckServiceAccountAccess checks the rules that decide whether
// the service account can write into a directory the user named: the
// ownership of the account, the group of the account, the world write
// permission and the nearest existing ancestor of a directory that is not
// there yet.
//
// The account of the test is the account of the test process, so the
// directories of the test belong to it.
func TestSystemdCheckServiceAccountAccess(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatalf("Current() = %v, want nil", err)
	}
	other := systemdSecondAccount(t)

	root := t.TempDir()
	owned := filepath.Join(root, "owned")
	if err := os.Mkdir(owned, 0o700); err != nil {
		t.Fatal(err)
	}
	group := filepath.Join(root, "group")
	if err := os.Mkdir(group, 0o770); err != nil {
		t.Fatal(err)
	}
	world := filepath.Join(root, "world")
	if err := os.Mkdir(world, 0o777); err != nil {
		t.Fatal(err)
	}
	closed := filepath.Join(root, "closed")
	if err := os.Mkdir(closed, 0o500); err != nil {
		t.Fatal(err)
	}

	// A directory the account cannot write into: it belongs to the
	// account of the test, and the account of the check is another one.
	foreign := filepath.Join(root, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}

	m := &systemdManager{scope: ScopeSystem}
	cases := []struct {
		name    string
		account *user.User
		path    string
		wantErr bool
	}{
		{name: "a directory the account owns", account: account, path: owned},
		{name: "a directory of the group of the account", account: account, path: group},
		{name: "a world writable directory", account: account, path: world},
		{name: "a directory the account cannot write into", account: other, path: foreign, wantErr: true},
		{name: "a directory without a write permission", account: other, path: closed, wantErr: true},
		{name: "a missing directory below a writable ancestor", account: account, path: filepath.Join(owned, "cache", "hub")},
		{name: "a missing directory below a closed ancestor", account: other, path: filepath.Join(closed, "cache"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDirWritableByAccount(tc.path, tc.account)
			if tc.wantErr && err == nil {
				t.Errorf("checkDirWritableByAccount(%q) = nil, want a refusal", tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("checkDirWritableByAccount(%q) = %v, want nil", tc.path, err)
			}
		})
	}

	t.Run("the scope default of the cache is not checked", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)
		opts.Paths.StateDir = filepath.Join(root, "state")
		opts.CacheDir = filepath.Join(opts.Paths.StateDir, "cache")
		opts.HubDir = filepath.Join(opts.CacheDir, "hub")
		// The machine-wide scope has no local dir of its own, so no
		// directory is named and nothing is checked.
		opts.LocalDir = ""
		// The account cannot write into the cache of the scope, and the
		// installation does not check it: the cache lives inside the
		// state directory of the instance.
		if err := m.checkServiceAccountAccess(opts, other); err != nil {
			t.Errorf("checkServiceAccountAccess() = %v, want nil for the cache of the scope", err)
		}
	})

	t.Run("a named cache of the scope is checked", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)
		opts.Paths.StateDir = filepath.Join(root, "state")
		opts.CacheDir = foreign
		opts.HubDir = filepath.Join(foreign, "hub")
		if err := m.checkServiceAccountAccess(opts, other); err == nil {
			t.Error("checkServiceAccountAccess() = nil, want the refusal of the named cache")
		}
	})

	t.Run("the user scope is not checked", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeUser)
		opts.CacheDir = foreign

		user := &systemdManager{scope: ScopeUser}
		if err := user.checkServiceAccountAccess(opts, other); err != nil {
			t.Errorf("checkServiceAccountAccess() = %v, want nil for a user scope instance", err)
		}
	})
}

// TestSystemdCheckServiceAccountAccessSkipsAMissingDirectoryInsideTheStateRoot
// checks that a directory inside the state directory of the instance that does
// not exist yet is not checked against the service account.
//
// The installation creates the state directory of the instance and grants it
// to the account, so the service creates the directory itself below the state
// directory, as the account. The check of the named directories must not
// refuse such a directory. A directory outside of the state directory is
// still checked, because the installation never grants it to the account.
func TestSystemdCheckServiceAccountAccessSkipsAMissingDirectoryInsideTheStateRoot(t *testing.T) {
	other := systemdSecondAccount(t)
	root := systemdTraversableTempDir(t)

	// The state directory belongs to the account of the test, so the
	// account of the check cannot write into it.
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatalf("close the state directory: %v", err)
	}

	m := &systemdManager{scope: ScopeSystem}
	opts := systemdTestOptions(t, ScopeSystem)
	opts.Paths.StateDir = state
	// The named directories lie inside the state directory of the instance
	// and do not exist yet: the service creates them below the state
	// directory, and the account owns the state directory after the
	// installation.
	opts.CacheDir = filepath.Join(state, "models")
	opts.HubDir = filepath.Join(opts.CacheDir, "hub")
	// The local directory lies inside the state directory of the instance
	// as well: the installation grants the state directory to the account,
	// so the account can use the local directory as well.
	opts.LocalDir = filepath.Join(state, "output")

	if err := m.checkServiceAccountAccess(opts, other); err != nil {
		t.Errorf("checkServiceAccountAccess() = %v, want nil for a directory inside the state directory of the instance", err)
	}

	// A directory outside of the state directory is still checked: the
	// installation never grants it to the account.
	foreign := filepath.Join(root, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatalf("create the foreign directory: %v", err)
	}
	if err := os.Chmod(foreign, 0o700); err != nil {
		t.Fatalf("close the foreign directory: %v", err)
	}
	opts.LocalDir = foreign
	if err := m.checkServiceAccountAccess(opts, other); err == nil {
		t.Error("checkServiceAccountAccess() = nil, want the refusal of a directory outside of the state directory of the instance")
	}
}

// TestSystemdCheckServiceAccountAccessChecksAnExistingDirectoryInsideTheStateRoot
// checks that a named directory inside the state directory of the instance is
// only exempt from the check while it does not exist yet.
//
// The installation grants the state directory to the account and creates
// nothing below it: a directory that exists already inside the state
// directory belongs to whoever owns it, and the account has to be able to
// write there. The installation refuses such a directory when the account
// cannot, so the failure happens at the installation and not at the first
// write of the service.
func TestSystemdCheckServiceAccountAccessChecksAnExistingDirectoryInsideTheStateRoot(t *testing.T) {
	other := systemdSecondAccount(t)
	root := systemdTraversableTempDir(t)

	// The state directory of the instance. The installation grants it to
	// the account, so the account passes through it: the mode leaves the
	// traversal to every account.
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o711); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	if err := os.Chmod(state, 0o711); err != nil {
		t.Fatalf("open the state directory to the traversal: %v", err)
	}

	// A named directory inside the state directory that does not exist
	// yet: the service creates it below the state directory, and the
	// account owns the state directory after the installation.
	missing := filepath.Join(state, "models")

	// A named directory inside the state directory that exists and that
	// the account cannot write into: it belongs to the user of the test
	// and to nobody else.
	closed := filepath.Join(state, "closed")
	if err := os.Mkdir(closed, 0o700); err != nil {
		t.Fatalf("create the closed directory: %v", err)
	}
	if err := os.Chmod(closed, 0o700); err != nil {
		t.Fatalf("close the closed directory: %v", err)
	}

	// A named directory inside the state directory that exists and that
	// the account can write into.
	open := filepath.Join(state, "open")
	if err := os.Mkdir(open, 0o777); err != nil {
		t.Fatalf("create the open directory: %v", err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatalf("open the open directory: %v", err)
	}

	// A named directory outside of the state directory that does not
	// exist yet: the account has to create it below the scratch
	// directory, and it cannot write there.
	outsideMissing := filepath.Join(root, "outside-missing")

	// A directory outside of the state directory that the account of the
	// check cannot write into.
	foreign := filepath.Join(root, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatalf("create the foreign directory: %v", err)
	}
	if err := os.Chmod(foreign, 0o700); err != nil {
		t.Fatalf("close the foreign directory: %v", err)
	}

	// A link inside the state directory that points out of it, to a
	// directory that exists.
	outside := filepath.Join(state, "outside")
	if err := os.Symlink(foreign, outside); err != nil {
		t.Fatalf("create the link out of the state directory: %v", err)
	}

	// A link inside the state directory that points out of it, to a
	// directory that does not exist yet.
	outsideMissingLink := filepath.Join(state, "outside-missing")
	if err := os.Symlink(outsideMissing, outsideMissingLink); err != nil {
		t.Fatalf("create the link to the missing directory: %v", err)
	}

	m := &systemdManager{scope: ScopeSystem}
	opts := systemdTestOptions(t, ScopeSystem)
	opts.Paths.StateDir = state
	opts.HubDir = ""
	opts.LocalDir = ""

	cases := []struct {
		name    string
		cache   string
		wantErr bool
	}{
		{name: "a missing directory inside the state directory", cache: missing},
		{name: "an existing directory inside the state directory that the account cannot write", cache: closed, wantErr: true},
		{name: "an existing directory inside the state directory that the account can write", cache: open},
		{name: "a missing directory outside of the state directory", cache: outsideMissing, wantErr: true},
		{name: "a link inside the state directory that points to an existing directory out of it", cache: outside, wantErr: true},
		{name: "a link inside the state directory that points to a missing directory out of it", cache: outsideMissingLink, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts.CacheDir = tc.cache
			err := m.checkServiceAccountAccess(opts, other)
			if !tc.wantErr {
				if err != nil {
					t.Errorf("checkServiceAccountAccess() = %v, want nil for the directory %q", err, tc.cache)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkServiceAccountAccess() = nil, want the refusal of the directory %q", tc.cache)
			}
			// The refusal names the directory, the account and the
			// access that is missing.
			message := err.Error()
			for _, want := range []string{
				"the service account " + strconv.Quote(other.Username),
				strconv.Quote(tc.cache),
				"cannot write",
			} {
				if !strings.Contains(message, want) {
					t.Errorf("checkServiceAccountAccess() = %q, want a message that mentions %q", message, want)
				}
			}
		})
	}
}

// TestSystemdCheckServiceAccountAccessUsesTheRealLocationOfTheDirectory
// checks that the exemption for a directory inside the state directory of the
// instance is decided on the real location of the paths.
//
// The kernel follows the symbolic links of a path before it uses it, so a
// link inside the state directory that points out of it leaves the state
// directory: the account has to reach the target of the link on its own, and
// the installation refuses the directory when it cannot.
func TestSystemdCheckServiceAccountAccessUsesTheRealLocationOfTheDirectory(t *testing.T) {
	other := systemdSecondAccount(t)
	root := systemdTraversableTempDir(t)

	// The state directory belongs to the account of the test: the
	// installation grants it to the service account, so a directory inside
	// it is not checked against the account. The account of the check may
	// pass through it, so a refusal of a case is about the directory of the
	// case and not about the state directory.
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o711); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	if err := os.Chmod(state, 0o711); err != nil {
		t.Fatalf("open the state directory to the traversal: %v", err)
	}

	// A plain directory inside the state directory. It exists already, so
	// it belongs to whoever owns it: the account of the check cannot write
	// into it, and the installation refuses it.
	plain := filepath.Join(state, "models")
	if err := os.Mkdir(plain, 0o700); err != nil {
		t.Fatalf("create the plain directory: %v", err)
	}
	if err := os.Chmod(plain, 0o700); err != nil {
		t.Fatalf("close the plain directory: %v", err)
	}

	// A directory outside of the state directory that the account of the
	// check cannot write into.
	foreign := filepath.Join(root, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatalf("create the foreign directory: %v", err)
	}
	if err := os.Chmod(foreign, 0o700); err != nil {
		t.Fatalf("close the foreign directory: %v", err)
	}

	// A directory outside of the state directory that every account can
	// write into.
	reachable := filepath.Join(root, "reachable")
	if err := os.Mkdir(reachable, 0o777); err != nil {
		t.Fatalf("create the reachable directory: %v", err)
	}
	if err := os.Chmod(reachable, 0o777); err != nil {
		t.Fatalf("open the reachable directory: %v", err)
	}

	// A link inside the state directory that points out of it.
	outside := filepath.Join(state, "outside")
	if err := os.Symlink(foreign, outside); err != nil {
		t.Fatalf("create the link out of the state directory: %v", err)
	}

	// A link inside the state directory that points to a place the account
	// of the check can reach.
	inside := filepath.Join(state, "inside")
	if err := os.Symlink(reachable, inside); err != nil {
		t.Fatalf("create the link to the reachable directory: %v", err)
	}

	// A directory inside the state directory that does not exist yet: the
	// installation creates it below the state directory, and the account
	// owns the state directory after the installation.
	missing := filepath.Join(state, "missing")

	m := &systemdManager{scope: ScopeSystem}
	opts := systemdTestOptions(t, ScopeSystem)
	opts.Paths.StateDir = state
	opts.HubDir = ""
	opts.LocalDir = ""

	cases := []struct {
		name    string
		cache   string
		wantErr bool
	}{
		{name: "a plain directory inside the state directory", cache: plain, wantErr: true},
		{name: "a missing directory inside the state directory", cache: missing},
		{name: "a link inside the state directory that points out of it", cache: outside, wantErr: true},
		{name: "a link inside the state directory that points to a reachable place", cache: inside},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts.CacheDir = tc.cache
			err := m.checkServiceAccountAccess(opts, other)
			if !tc.wantErr {
				if err != nil {
					t.Errorf("checkServiceAccountAccess() = %v, want nil for the directory %q", err, tc.cache)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkServiceAccountAccess() = nil, want the refusal of the directory %q", tc.cache)
			}
			// The refusal names the directory, the account and the
			// access that is missing.
			message := err.Error()
			for _, want := range []string{
				"the service account " + strconv.Quote(other.Username),
				strconv.Quote(tc.cache),
				"cannot write",
			} {
				if !strings.Contains(message, want) {
					t.Errorf("checkServiceAccountAccess() = %q, want a message that mentions %q", message, want)
				}
			}
		})
	}

	t.Run("the state directory is a link to another place", func(t *testing.T) {
		// The state directory of the instance is a link: the installation
		// grants the directory the link points at to the account, so a
		// directory inside it that does not exist yet is not checked
		// against the account.
		real := filepath.Join(root, "real-state")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatalf("create the real state directory: %v", err)
		}
		if err := os.Chmod(real, 0o700); err != nil {
			t.Fatalf("close the real state directory: %v", err)
		}
		models := filepath.Join(real, "models")
		if err := os.Mkdir(models, 0o700); err != nil {
			t.Fatalf("create the models directory: %v", err)
		}
		if err := os.Chmod(models, 0o700); err != nil {
			t.Fatalf("close the models directory: %v", err)
		}
		link := filepath.Join(root, "state-link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatalf("create the link of the state directory: %v", err)
		}

		opts.Paths.StateDir = link

		// A directory that does not exist yet below the linked state
		// directory: the service creates it, and the account owns the
		// real state directory after the installation.
		opts.CacheDir = filepath.Join(link, "output")
		if err := m.checkServiceAccountAccess(opts, other); err != nil {
			t.Errorf("checkServiceAccountAccess() = %v, want nil for a directory inside the state directory of the instance", err)
		}

		// The same directory named by its real location is inside the
		// state directory as well.
		opts.CacheDir = filepath.Join(real, "output")
		if err := m.checkServiceAccountAccess(opts, other); err != nil {
			t.Errorf("checkServiceAccountAccess() = %v, want nil for a directory inside the state directory of the instance", err)
		}

		// The directory that exists already inside the linked state
		// directory belongs to whoever owns it: the account cannot write
		// into it, and the installation refuses it.
		opts.CacheDir = models
		if err := m.checkServiceAccountAccess(opts, other); err == nil {
			t.Error("checkServiceAccountAccess() = nil, want the refusal of an existing directory inside the state directory of the instance")
		}
	})
}

// TestSystemdAccessCheckUsesTheEffectivePermission checks that the access
// check of a directory the service account has to use is the effective one.
//
// The ownership of a directory is not enough: a directory the account owns
// with the mode 0500 is not writable, because the owner has no write
// permission in it, and a directory below a parent without the execute
// permission cannot be reached at all.
//
// The account of the checks owns nothing of the test and belongs to the
// group of its directories, so the group permissions are the ones that
// apply to it.
func TestSystemdAccessCheckUsesTheEffectivePermission(t *testing.T) {
	current := systemdCurrentAccount(t)
	root := systemdTraversableTempDir(t)

	// The group of the directories of the test is the group of the test
	// process, so the other identity reaches them through the group.
	probe := filepath.Join(root, "probe")
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatalf("create the probe directory: %v", err)
	}
	_, group := systemdOwner(t, probe)

	// The other identity owns nothing of the test and belongs to the
	// group of its directories.
	other := &user.User{
		Username: "hfdesk-access-test",
		Uid:      "65534",
		Gid:      group,
		Name:     "HFDesk access test account",
		HomeDir:  "/nonexistent",
	}

	cases := []struct {
		name    string
		dir     string
		account *user.User
		mode    os.FileMode
		wantErr bool
	}{
		{
			name:    "a directory the account owns",
			dir:     "owned",
			account: current,
			mode:    0o700,
		},
		{
			name:    "a directory the account owns without the write permission",
			dir:     "readonly",
			account: current,
			mode:    0o500,
			wantErr: true,
		},
		{
			name:    "a directory the account owns without the write and the execute permission",
			dir:     "closed",
			account: current,
			mode:    0o400,
			wantErr: true,
		},
		{
			name:    "a directory of the group of the account",
			dir:     "group",
			account: other,
			mode:    0o770,
		},
		{
			name:    "a directory of the group of the account without the execute permission",
			dir:     "group-without-execute",
			account: other,
			mode:    0o620,
			wantErr: true,
		},
		{
			name:    "a directory the account does not own and cannot write into",
			dir:     "foreign",
			account: other,
			mode:    0o700,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(root, tc.dir)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatalf("create the directory: %v", err)
			}
			// The mode of a create call is filtered by the umask of
			// the process, so the mode is set explicitly.
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatalf("set the mode of the directory: %v", err)
			}
			err := checkDirWritableByAccount(dir, tc.account)
			if tc.wantErr && err == nil {
				t.Errorf("checkDirWritableByAccount(%q) = nil, want a refusal", dir)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("checkDirWritableByAccount(%q) = %v, want nil", dir, err)
			}
		})
	}

	t.Run("a directory below a parent without the execute permission", func(t *testing.T) {
		parent := filepath.Join(root, "closed-parent")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatalf("create the parent directory: %v", err)
		}
		child := filepath.Join(parent, "child")
		if err := os.Mkdir(child, 0o700); err != nil {
			t.Fatalf("create the child directory: %v", err)
		}
		// The group of the child directory is the group of the
		// account, so the account could use it if it could reach it.
		if err := os.Chmod(child, 0o770); err != nil {
			t.Fatalf("make the child directory usable: %v", err)
		}
		// The parent is closed for everybody but its owner.
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Fatalf("close the parent directory: %v", err)
		}
		if err := checkDirWritableByAccount(child, other); err == nil {
			t.Error("checkDirWritableByAccount() = nil, want the refusal of a directory the account cannot reach")
		}
	})
}

// TestSystemdAccessCheckResolvesTheSymlinksOfThePath checks that the access
// check judges the real location of a path.
//
// The kernel follows the symbolic links of a path before it uses it, so a
// directory that is reached through a link is used by its real location. A
// check that asked about the path as it was written would accept a directory
// whose real location the account never reaches, and the instance would fail
// with a permission error of the file system.
//
// The account of the check owns nothing of the test and is not a member of
// the group of its directories, so the permissions of the other accounts
// apply to it.
func TestSystemdAccessCheckResolvesTheSymlinksOfThePath(t *testing.T) {
	other := systemdSecondAccount(t)
	root := systemdTraversableTempDir(t)

	// The public directory is world writable, so the account could create
	// a directory in it. The private directory is closed for everybody but
	// its owner, so the account never reaches what is below it.
	public := filepath.Join(root, "public")
	if err := os.Mkdir(public, 0o777); err != nil {
		t.Fatalf("create the public directory: %v", err)
	}
	if err := os.Chmod(public, 0o777); err != nil {
		t.Fatalf("make the public directory world writable: %v", err)
	}
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatalf("create the private directory: %v", err)
	}
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatalf("close the private directory: %v", err)
	}
	data := filepath.Join(private, "data")
	if err := os.Mkdir(data, 0o777); err != nil {
		t.Fatalf("create the data directory: %v", err)
	}
	if err := os.Chmod(data, 0o777); err != nil {
		t.Fatalf("make the data directory world writable: %v", err)
	}

	// The open directory is traversable by the account, so the real
	// location of the directory below it is usable.
	open := filepath.Join(root, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatalf("create the open directory: %v", err)
	}
	openData := filepath.Join(open, "data")
	if err := os.Mkdir(openData, 0o777); err != nil {
		t.Fatalf("create the data directory of the open directory: %v", err)
	}
	if err := os.Chmod(openData, 0o777); err != nil {
		t.Fatalf("make the data directory world writable: %v", err)
	}

	// The locked directory is closed for everybody but its owner, so the
	// account never reaches the link in it.
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatalf("create the locked directory: %v", err)
	}
	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatalf("close the locked directory: %v", err)
	}
	lockedLink := filepath.Join(locked, "cache")
	if err := os.Symlink(filepath.Join("..", "open", "data"), lockedLink); err != nil {
		t.Fatalf("create the link in the locked directory: %v", err)
	}

	// The links are the paths the user names: the cache points into the
	// private directory, the models into the open one.
	linked := filepath.Join(public, "cache")
	if err := os.Symlink(filepath.Join("..", "private", "data"), linked); err != nil {
		t.Fatalf("create the link into the private directory: %v", err)
	}
	linkedMissing := filepath.Join(public, "missing")
	if err := os.Symlink(filepath.Join("..", "private", "new"), linkedMissing); err != nil {
		t.Fatalf("create the link into the missing directory: %v", err)
	}
	openLink := filepath.Join(public, "models")
	if err := os.Symlink(filepath.Join("..", "open", "data"), openLink); err != nil {
		t.Fatalf("create the link into the open directory: %v", err)
	}

	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name:    "a link into a directory the account cannot reach",
			path:    linked,
			wantErr: true,
		},
		{
			name:    "a link into a directory that does not exist yet below a directory the account cannot reach",
			path:    linkedMissing,
			wantErr: true,
		},
		{
			name:    "a link the account cannot reach itself",
			path:    lockedLink,
			wantErr: true,
		},
		{
			name: "a link into a directory the account can reach",
			path: openLink,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDirWritableByAccount(tc.path, other)
			if tc.wantErr && err == nil {
				t.Errorf("checkDirWritableByAccount(%q) = nil, want the refusal of the real location of the path", tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("checkDirWritableByAccount(%q) = %v, want nil for the real location of the path", tc.path, err)
			}
		})
	}
}

// TestSystemdAccessCheckUsesTheSupplementaryGroups checks that the access
// check of a directory the service account has to use knows the
// supplementary groups of the account.
//
// The groups of an account are read from the group database of the machine,
// so the test needs a group the account of the test belongs to. The test
// skips itself when the machine knows no such group.
func TestSystemdAccessCheckUsesTheSupplementaryGroups(t *testing.T) {
	current := systemdCurrentAccount(t)
	group, ok := systemdSupplementaryGroup(t)
	if !ok {
		t.Skip("skipping: the machine knows no supplementary group of the account of the test")
	}
	root := systemdTraversableTempDir(t)

	dir := filepath.Join(root, "supplementary")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}
	gid, err := strconv.Atoi(group)
	if err != nil {
		t.Fatalf("the group id %q is not a number: %v", group, err)
	}
	// The directory belongs to the group of the account, not to the
	// account itself.
	if err := os.Chown(dir, os.Getuid(), gid); err != nil {
		t.Fatalf("give the directory to the group %q: %v", group, err)
	}

	// The account of the check carries the name of the account of the
	// test, so the group database of the machine gives it the
	// supplementary group of the account of the test. It owns nothing of
	// the test.
	account := &user.User{
		Username: current.Username,
		Uid:      "65534",
		Gid:      current.Gid,
		Name:     current.Name,
		HomeDir:  current.HomeDir,
	}

	// The group permissions apply to the account: it can create entries
	// in the directory and it can reach it.
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatalf("make the directory usable: %v", err)
	}
	if err := checkDirWritableByAccount(dir, account); err != nil {
		t.Errorf("checkDirWritableByAccount() = %v, want nil for a directory of the supplementary group of the account", err)
	}

	// The group has no execute permission: the account cannot use the
	// directory.
	if err := os.Chmod(dir, 0o620); err != nil {
		t.Fatalf("close the directory: %v", err)
	}
	if err := checkDirWritableByAccount(dir, account); err == nil {
		t.Error("checkDirWritableByAccount() = nil, want the refusal of a directory without the execute permission of the group")
	}
}

func TestSystemdGrantInstanceDirsUsesLoadedNativeUserAndGroup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to chown temporary instance directories and verify the actual effective UID/GID; ownership is not simulated")
	}
	base, loadedUser := systemdCurrentAccount(t), systemdSecondAccount(t)
	loadedUID, err := strconv.Atoi(loadedUser.Uid)
	if err != nil {
		t.Fatal(err)
	}
	loadedGID, err := strconv.Atoi(loadedUser.Gid)
	if err != nil {
		t.Fatal(err)
	}
	root := systemdTraversableTempDir(t)
	configDir := filepath.Join(root, "config")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(configDir, loadedUID, loadedGID); err != nil {
		t.Fatal(err)
	}
	beforeUID, beforeGID := systemdOwner(t, configDir)
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(configDir, "hfdesk.json"),
			StateDir:   filepath.Join(root, "state"),
			LogDir:     filepath.Join(root, "logs"),
		},
	}
	properties := systemdTypedProperties(opts.Executable, []string{opts.Executable}, nil, root, loadedUser.Username, loadedUser.Gid, nil)
	loaded, err := loadedConfigFromProperties(properties)
	if err != nil {
		t.Fatal(err)
	}
	effective := systemdAccessIdentityForInstall(ScopeSystem, InstallOptions{Scope: ScopeSystem}, true, loaded, true, base)
	if effective.access == nil || effective.accountName != loadedUser.Username || effective.owner == nil {
		t.Fatalf("projected identity = %+v, want the loaded user rather than the base account", effective)
	}
	baseUID, _ := strconv.Atoi(base.Uid)
	if effective.owner.UID == baseUID || effective.owner.UID != loadedUID || effective.owner.GID != loadedGID {
		t.Fatalf("projected owner = %+v, want loaded User/Group %d:%d rather than request account %q", effective.owner, loadedUID, loadedGID, base.Username)
	}
	dirs := instanceDirs(opts)
	created := make(map[string][]string, len(dirs))
	for _, dir := range dirs {
		created[dir.path] = createdDirs(dir.path)
	}
	if err := PrepareDirs(opts); err != nil {
		t.Fatal(err)
	}
	manager := &systemdManager{scope: ScopeSystem}
	if err := manager.grantInstanceDirsForDirsWithIdentity(opts, effective, created, dirs); err != nil {
		t.Fatalf("grantInstanceDirsForDirsWithIdentity() = %v", err)
	}
	for _, path := range []string{opts.Paths.StateDir, opts.Paths.LogDir} {
		uid, gid := systemdOwner(t, path)
		if uid != loadedUser.Uid || gid != loadedUser.Gid {
			t.Errorf("new instance directory %q owner = %s:%s, want loaded User/Group %s:%s", path, uid, gid, loadedUser.Uid, loadedUser.Gid)
		}
	}
	if uid, gid := systemdOwner(t, configDir); uid != beforeUID || gid != beforeGID {
		t.Errorf("pre-existing config directory owner changed to %s:%s; want unchanged %s:%s", uid, gid, beforeUID, beforeGID)
	}
}

// TestSystemdInstallRefusesACacheDirTheAccountCannotWrite checks that a
// machine-wide installation whose cache directory the service account cannot
// write into is refused with the guidance that names the account and the
// directory, and that the refused installation never changes the ownership
// of that directory.
func TestSystemdInstallRefusesACacheDirTheAccountCannotWrite(t *testing.T) {
	account := systemdSecondAccount(t)

	// The home of the user is a temporary directory, so the guard of the
	// machine-wide scope does not refuse the paths of the test.
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatalf("create the cache directory: %v", err)
	}
	owner, group := systemdOwner(t, cache)

	m := &systemdManager{scope: ScopeSystem}
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
		CacheDir:    cache,
		ServiceUser: account.Username,
	}

	_, err := m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the cache directory")
	}
	want := fmt.Sprintf("service: the service account %q cannot write to the cache directory %q: grant it access and install again (for example add the account to the group that owns the directory, or give the directory itself to the account with %q; for a directory that holds a lot of data, granting group access is preferable to changing the owner)",
		account.Username, cache, "chown "+account.Username+" "+cache)
	if err.Error() != want {
		t.Errorf("Install() = %q, want %q", err, want)
	}
	// The refusal names the directory, the account and the missing write
	// access.
	for _, part := range []string{account.Username, cache, "cannot write to"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("Install() = %q, want the %q in the refusal", err, part)
		}
	}
	// The refusal never names a recursive ownership change: the tree of a
	// model or a cache directory belongs to the user who named it, and the
	// installation never walks it.
	if strings.Contains(err.Error(), "-R") {
		t.Errorf("Install() = %q, want no recursive ownership change in the refusal", err)
	}

	// The refused installation never changes the ownership of the cache
	// tree.
	if uid, gid := systemdOwner(t, cache); uid != owner || gid != group {
		t.Errorf("the ownership of the cache directory %q = %q:%q, want %q:%q", cache, uid, gid, owner, group)
	}
	// The refused installation creates no directory of the instance.
	for _, dir := range instanceDirs(opts) {
		if _, err := os.Stat(dir.path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused installation created the %s %q: %v", dir.name, dir.path, err)
		}
	}
}

// TestSystemdUserScopeNeedsNoServiceAccount checks that a user scope
// instance is not affected by the service account of the machine-wide scope:
// it needs no account, no account is written into its unit and the access of
// a directory it does not own is not checked.
func TestSystemdUserScopeNeedsNoServiceAccount(t *testing.T) {
	const missing = "hfdesk-missing-service-account"

	opts := systemdTestOptions(t, ScopeUser)
	opts.ServiceUser = missing
	// A cache directory the account could not write into: the user scope
	// instance writes it as the user of the session.
	opts.CacheDir = filepath.Join(t.TempDir(), "cache")

	m := &systemdManager{scope: ScopeUser}

	account, err := m.serviceAccount(opts)
	if err != nil {
		t.Fatalf("serviceAccount() = %v, want nil for a user scope instance", err)
	}
	if account != nil {
		t.Errorf("a user scope instance resolved the account %q, want none", account.Username)
	}
	if err := m.checkServiceAccountAccess(opts, account); err != nil {
		t.Errorf("checkServiceAccountAccess() = %v, want nil for a user scope instance", err)
	}
	if err := m.grantInstanceDirs(opts, account, nil); err != nil {
		t.Errorf("grantInstanceDirs() = %v, want nil for a user scope instance", err)
	}

	// The unit of a user scope instance names no account, even when the
	// command line named one.
	unit, err := renderUnit(opts, nil)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}
	for _, directive := range unitDirectives(unit) {
		if strings.HasPrefix(directive, "User=") || strings.HasPrefix(directive, "Group=") {
			t.Errorf("a user scope unit names an account: %q", directive)
		}
	}
}

// TestSystemdCreatedDirOrder checks the order of the directories a failed
// installation removes: the directories are listed in the order they are
// created, so a parent is always removed after its children.
func TestSystemdCreatedDirOrder(t *testing.T) {
	root := t.TempDir()
	opts := InstallOptions{
		Scope:      ScopeUser,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
	}

	created := make(map[string][]string, 3)
	for _, dir := range instanceDirs(opts) {
		created[dir.path] = createdDirs(dir.path)
	}
	order := createdDirOrder(opts, created)

	want := []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		filepath.Join(root, "var", "lib", "hfdesk"),
		filepath.Join(root, "var", "log"),
		filepath.Join(root, "var", "log", "hfdesk"),
	}
	if strings.Join(order, "\n") != strings.Join(want, "\n") {
		t.Errorf("createdDirOrder() =\n%s\nwant\n%s", strings.Join(order, "\n"), strings.Join(want, "\n"))
	}
}

// TestSystemdFailedInstallRemovesTheDirectoriesItCreated checks that created
// paths are removed after rollback verifies the first registration was removed.
func TestSystemdFailedInstallRemovesTheDirectoriesItCreated(t *testing.T) {
	systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	// The first reload fails, but the rollback reload succeeds and proves the
	// failed first registration was removed from the supervisor.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_DAEMON_RELOAD_FAIL_AT", "1")

	opts := systemdTempInstallOptions(t, root)
	m := &systemdManager{scope: ScopeUser}
	if _, err := m.Install(context.Background(), opts); err == nil {
		t.Fatal("Install() = nil, want the failure of the supervisor")
	}

	for _, dir := range []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		filepath.Join(root, "var", "lib", "hfdesk"),
		filepath.Join(root, "var", "log"),
		filepath.Join(root, "var", "log", "hfdesk"),
	} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the directory %q is still there after the failed installation (stat error: %v)", dir, err)
		}
	}
}

// TestSystemdFailedInstallKeepsADirectoryThatExistedBefore checks that a
// failed installation never removes a directory that existed before it: such
// a directory belongs to whoever owns it, with the ownership and the
// permissions it had.
func TestSystemdFailedInstallKeepsADirectoryThatExistedBefore(t *testing.T) {
	systemdFakeSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()

	// The config directory existed before the installation.
	config := filepath.Join(root, "etc")
	if err := os.Mkdir(config, 0o750); err != nil {
		t.Fatalf("create the config directory: %v", err)
	}
	owner, group := systemdOwner(t, config)

	opts := systemdTempInstallOptions(t, root)
	m := &systemdManager{scope: ScopeUser}
	if _, err := m.Install(context.Background(), opts); err == nil {
		t.Fatal("Install() = nil, want the failure of the supervisor")
	}

	// The directory that existed before the installation is still there,
	// with the owner and the permissions it had.
	info, err := os.Stat(config)
	if err != nil {
		t.Fatalf("the config directory %q is gone after the failed installation: %v", config, err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Errorf("the mode of the config directory %q = %v, want %v", config, info.Mode().Perm(), os.FileMode(0o750))
	}
	if uid, gid := systemdOwner(t, config); uid != owner || gid != group {
		t.Errorf("the ownership of the config directory %q = %q:%q, want %q:%q", config, uid, gid, owner, group)
	}
	// The rollback did not prove whether the still-loaded unit references this
	// directory, so the newly created path is retained.
	created := filepath.Join(config, "hfdesk")
	if info, err := os.Stat(created); err != nil || !info.IsDir() {
		t.Errorf("the directory %q was removed despite unconfirmed rollback: %v", created, err)
	}
}

// TestSystemdFailedInstallKeepsACreatedDirectoryThatHoldsData checks that an
// unverified rollback retains every created path, not only the nonempty one.
func TestSystemdFailedInstallKeepsACreatedDirectoryThatHoldsData(t *testing.T) {
	systemdFakeSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()

	opts := systemdTempInstallOptions(t, root)
	state := opts.Paths.StateDir
	m := &systemdManager{scope: ScopeUser}

	// The fake supervisor writes a marker into the state directory, so the
	// directory holds data when the installation fails.
	t.Setenv("HFDESK_TEST_MARKER_DIR", state)
	_, err := m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the failure of the supervisor")
	}
	if !strings.Contains(err.Error(), "the fake supervisor refuses the command") {
		t.Errorf("Install() = %q, want the original fake-supervisor failure", err)
	}
	if !strings.Contains(err.Error(), "the previous unit file or captured loaded settings could not be restored or verified") {
		t.Errorf("Install() = %q, want the unconfirmed rollback outcome", err)
	}

	// The state directory holds the marker, so it is kept, and so are the
	// directories that hold it.
	for _, dir := range []string{
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		state,
	} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("the directory %q is gone after the failed installation: %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "marker")); err != nil {
		t.Errorf("the marker of the state directory is gone: %v", err)
	}
	// Rollback could not establish whether the retained registration refers to
	// these directories. Empty paths are kept on that uncertainty too.
	for _, dir := range []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var", "log"),
		filepath.Join(root, "var", "log", "hfdesk"),
	} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("the directory %q was removed despite unconfirmed rollback: %v", dir, err)
		}
	}
}

// TestSystemdFailedInstallRemovesTheDirectoriesOfAFailedPreparation checks
// that an installation that fails while it prepares the directories of the
// instance leaves the machine as it was as well: the directories it created
// before the failure are removed too.
func TestSystemdFailedInstallRemovesTheDirectoriesOfAFailedPreparation(t *testing.T) {
	systemdFakeSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()

	opts := systemdTempInstallOptions(t, root)
	// The log directory is the root of the file system: the installation
	// refuses to tighten it, and it fails after the other two
	// directories were created.
	opts.Paths.LogDir = string(filepath.Separator)

	m := &systemdManager{scope: ScopeUser}
	_, err := m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the log directory")
	}
	want := fmt.Sprintf("service: log dir path %q is the filesystem root and must not be tightened", string(filepath.Separator))
	if err.Error() != want {
		t.Errorf("Install() = %q, want %q", err, want)
	}

	// The directories the installation created before it failed are gone.
	for _, dir := range []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		filepath.Join(root, "var", "lib", "hfdesk"),
	} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the directory %q is still there after the failed installation (stat error: %v)", dir, err)
		}
	}
}

func TestSystemdPreparationFailureRetainsPathsReferencedByPriorUnit(t *testing.T) {
	systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	manager, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	blockingFile := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockingFile, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	requested := DefaultInstallOptions(ScopeUser, newExecutable)
	requested.Paths.LogDir = filepath.Join(blockingFile, "logs")
	requested.Explicit.LogDir = true
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

	_, err := manager.Install(context.Background(), requested)
	if err == nil || !strings.Contains(err.Error(), "log dir") {
		t.Fatalf("Install() = %v, want directory-preparation failure", err)
	}
	stored, readErr := os.ReadFile(unitPath)
	if readErr != nil || string(stored) != previousUnit {
		t.Fatalf("the no-write failure changed the previous registration (read error %v)", readErr)
	}
	for _, dir := range []string{filepath.Join(root, "etc", "hfdesk"), previous.Paths.StateDir} {
		if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
			t.Errorf("created path %q was removed despite the retained unit referring to it: %v", dir, statErr)
		}
	}
}

func TestSystemdDirectoryCleanupRequiresKnownEmptyDropIns(t *testing.T) {
	candidate := filepath.Join(t.TempDir(), "candidate-state")
	previous := previousRegistration{
		existed:     true,
		loadedKnown: true,
		content: []byte("[Service]\nExecStart=/bin/true service run --config /tmp/config --state-dir /tmp/state --log-dir /tmp/log\n" +
			"WorkingDirectory=/tmp/state\n"),
	}
	created := map[string][]string{candidate: {candidate}}

	for _, tc := range []struct {
		name    string
		dropIns []string
		known   bool
	}{
		{name: "unavailable property"},
		{name: "loaded drop-in", dropIns: []string{"/etc/systemd/system/hfdesk.service.d/override.conf"}, known: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			properties := systemdTypedProperties("/bin/true", []string{"/bin/true", "service", "run"}, nil, "/tmp/state", "", "", nil)
			if tc.known {
				properties["DropInPaths"] = dbus.MakeVariant(tc.dropIns)
			} else {
				delete(properties, "DropInPaths")
			}
			loaded, err := loadedConfigFromProperties(properties)
			if err != nil {
				t.Fatal(err)
			}
			previous.loaded = loaded
			if systemdCreatedDirsSafeToRemove(previous, created) {
				t.Fatal("cleanup was allowed without proof that the previous loaded unit has no drop-ins")
			}
		})
	}
}

// TestSystemdSuccessfulInstallKeepsItsDirectories checks that a successful
// installation keeps the directories it created: the cleanup of a failed
// installation never touches the directories of an instance that runs.
func TestSystemdSuccessfulInstallKeepsItsDirectories(t *testing.T) {
	systemdFakeSupervisor(t)
	t.Setenv("HFDESK_TEST_SUPERVISOR", "ok")
	systemdTempInstallEnvironment(t)
	root := t.TempDir()

	opts := systemdTempInstallOptions(t, root)
	m := &systemdManager{scope: ScopeUser}
	if _, err := m.Install(context.Background(), opts); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}

	for _, dir := range []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		filepath.Join(root, "var", "lib", "hfdesk"),
		filepath.Join(root, "var", "log"),
		filepath.Join(root, "var", "log", "hfdesk"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("the directory %q is gone after the successful installation: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("the path %q is not a directory after the successful installation", dir)
		}
	}
}
