// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// secretSentinel is an obviously secret value. It is put into every field
// of the installation options that could carry a credential, so the tests
// can prove that no credential reaches the registration of the service.
const secretSentinel = "hfdesk-secret-sentinel"

// scmTestOptions returns the options of a registration whose executable
// does not exist. The pure helpers of the manager never touch the file
// system, so the options do not need one.
func scmTestOptions() InstallOptions {
	root := filepath.Join("C:\\ProgramData", "HFDesk")
	return InstallOptions{
		Scope:      ScopeSystem,
		Executable: filepath.Join(root, "hfdesk.exe"),
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "hfdesk.json"),
			StateDir:   root,
			LogDir:     filepath.Join(root, "logs"),
		},
		CacheDir: filepath.Join(root, "cache"),
		HubDir:   filepath.Join(root, "cache", "hub"),
		LocalDir: filepath.Join(root, "models"),
	}
}

func TestBuildSCMConfigUsesTheLeastPrivilegedAccount(t *testing.T) {
	opts := scmTestOptions()
	config := buildSCMConfig(opts)

	if config.ServiceStartName != scmLocalServiceAccount {
		t.Errorf("ServiceStartName = %q, want %q", config.ServiceStartName, scmLocalServiceAccount)
	}
	if config.ServiceStartName != `NT AUTHORITY\LocalService` {
		t.Errorf("ServiceStartName = %q, want the LocalService account", config.ServiceStartName)
	}
	if strings.Contains(strings.ToLower(config.ServiceStartName), "localsystem") {
		t.Errorf("ServiceStartName = %q, want the LocalService account and not LocalSystem", config.ServiceStartName)
	}
	if config.Password != "" {
		t.Errorf("Password = %q, want no password at all", config.Password)
	}
	if config.StartType != mgr.StartAutomatic {
		t.Errorf("StartType = %d, want %d", config.StartType, mgr.StartAutomatic)
	}
	if config.ErrorControl != mgr.ErrorNormal {
		t.Errorf("ErrorControl = %d, want %d", config.ErrorControl, mgr.ErrorNormal)
	}
	if config.ServiceType != windows.SERVICE_WIN32_OWN_PROCESS {
		t.Errorf("ServiceType = %d, want %d", config.ServiceType, windows.SERVICE_WIN32_OWN_PROCESS)
	}
	if config.DisplayName == "" || config.Description == "" {
		t.Errorf("DisplayName = %q and Description = %q, want both of them to be set", config.DisplayName, config.Description)
	}
}

func TestSCMCommandLineCarriesNoCredential(t *testing.T) {
	opts := scmTestOptions()
	// Every field of the options that could carry a credential carries
	// the sentinel.
	setCredentialFields(&opts, secretSentinel)

	commandLine := scmCommandLine(opts)
	if strings.Contains(commandLine, secretSentinel) {
		t.Errorf("the command line %q carries a credential", commandLine)
	}

	// The command line holds the executable and the canonical argument
	// vector of the shared contract, nothing else.
	parts, err := windows.DecomposeCommandLine(commandLine)
	if err != nil {
		t.Fatalf("DecomposeCommandLine(%q) = %v, want nil", commandLine, err)
	}
	want := append([]string{opts.Executable}, RunArgs(opts)...)
	if !reflect.DeepEqual(parts, want) {
		t.Errorf("the command line = %q, want %q", parts, want)
	}
	if !filepath.IsAbs(parts[0]) {
		t.Errorf("the registered executable %q is not an absolute path", parts[0])
	}
	if buildSCMConfig(opts).BinaryPathName != commandLine {
		t.Errorf("BinaryPathName = %q, want the command line %q", buildSCMConfig(opts).BinaryPathName, commandLine)
	}
}

func TestParseSCMCommandLineReadsTheRegisteredExecutableAndAddress(t *testing.T) {
	opts := scmTestOptions()
	// A path with a space in it exercises the escaping of the command
	// line.
	opts.Executable = filepath.Join("C:\\Program Files", "HFDesk", "hfdesk.exe")

	registration, err := parseSCMCommandLine(scmCommandLine(opts))
	if err != nil {
		t.Fatalf("parseSCMCommandLine() = %v, want nil", err)
	}
	if registration.executable != opts.Executable {
		t.Errorf("the registered executable = %q, want %q", registration.executable, opts.Executable)
	}
	if got, want := registration.listen(), "127.0.0.1:8080"; got != want {
		t.Errorf("the registered listen address = %q, want %q", got, want)
	}
	if got := registration.paths(); got != opts.Paths {
		t.Errorf("the registered paths = %+v, want %+v", got, opts.Paths)
	}
}

func TestSCMStateMapsEverySupervisorState(t *testing.T) {
	cases := []struct {
		name  string
		state svc.State
		want  State
	}{
		{"running", svc.Running, StateRunning},
		{"stopped", svc.Stopped, StateStopped},
		{"start pending", svc.StartPending, StateStarting},
		{"stop pending", svc.StopPending, StateStopping},
		{"paused", svc.Paused, StateUnknown},
		{"pause pending", svc.PausePending, StateUnknown},
		{"continue pending", svc.ContinuePending, StateUnknown},
		{"unknown", svc.State(4711), StateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmState(tc.state); got != tc.want {
				t.Errorf("scmState(%v) = %q, want %q", tc.state, got, tc.want)
			}
		})
	}
}

func TestSCMStateSDDLGrantsOnlyTheMachineAccounts(t *testing.T) {
	sddl := scmStateSDDL()

	// SYSTEM, the administrators of the machine and the account the
	// instance runs as.
	for _, want := range []string{";;;SY)", ";;;BA)", ";;;LS)"} {
		if !strings.Contains(sddl, want) {
			t.Errorf("the descriptor %q does not grant %q", sddl, want)
		}
	}
	// Nobody else.
	for _, forbidden := range []string{"WD", "Everyone", "Users", "AU;", ";;;AU)", "IU)", "BU)", "Authenticated"} {
		if strings.Contains(sddl, forbidden) {
			t.Errorf("the descriptor %q grants access to %q", sddl, forbidden)
		}
	}
	// The inheritance of the parent directory is blocked.
	if !strings.HasPrefix(sddl, "D:P") {
		t.Errorf("the descriptor %q does not block the inheritance", sddl)
	}
}

func TestNewPlatformManagerRejectsTheUserScope(t *testing.T) {
	manager, err := newPlatformManager(ScopeUser)
	if err == nil {
		t.Fatalf("newPlatformManager(ScopeUser) = %v, want an error", manager)
	}
	if !strings.Contains(err.Error(), "machine-wide") {
		t.Errorf("newPlatformManager(ScopeUser) = %v, want an error that names the reason", err)
	}
	if manager != nil {
		t.Errorf("newPlatformManager(ScopeUser) = %v, want no manager", manager)
	}

	manager, err = newPlatformManager(ScopeSystem)
	if err != nil {
		t.Fatalf("newPlatformManager(ScopeSystem) = %v, want nil", err)
	}
	if _, ok := manager.(*scmManager); !ok {
		t.Errorf("newPlatformManager(ScopeSystem) = %T, want a *scmManager", manager)
	}
}

func TestRequireElevatedNamesTheMissingPrivilege(t *testing.T) {
	err := requireElevated("an installation")
	if isElevatedProcess() {
		if err != nil {
			t.Errorf("requireElevated() = %v, want nil for an elevated process", err)
		}
		return
	}
	if err == nil {
		t.Fatal("requireElevated() = nil, want an error for a process that is not elevated")
	}
	if !strings.Contains(err.Error(), "elevated") {
		t.Errorf("requireElevated() = %v, want an error that names the elevated shell", err)
	}
}

// TestSCMLifecycle drives the registration of a service against the real
// service control manager.
//
// The test needs an elevated shell: the service control manager only accepts
// a machine-wide registration from an administrator. It is skipped without
// one.
func TestSCMLifecycle(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("the service control manager needs an elevated shell, run the test as an administrator")
	}

	// A registration that exists already belongs to the machine and not
	// to this test: it is never removed here.
	manager, err := mgr.Connect()
	if err != nil {
		t.Skipf("the service control manager is not available: %v", err)
	}
	existing, err := manager.OpenService(ServiceName)
	if err == nil {
		existing.Close()
		manager.Disconnect()
		t.Skipf("%s is registered already, the test would remove a real installation", ServiceName)
	}
	manager.Disconnect()

	root := t.TempDir()
	executable := filepath.Join("C:\\Windows", "System32", "cmd.exe")
	if _, err := os.Stat(executable); err != nil {
		t.Skipf("the test program %s is not available: %v", executable, err)
	}
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: executable,
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var"),
			LogDir:     filepath.Join(root, "var", "logs"),
		},
	}
	service := &scmManager{scope: ScopeSystem}

	// The registration never survives the test.
	t.Cleanup(func() {
		cleanupManager, err := mgr.Connect()
		if err != nil {
			return
		}
		defer cleanupManager.Disconnect()
		handle, err := cleanupManager.OpenService(ServiceName)
		if err != nil {
			return
		}
		defer handle.Close()
		_ = handle.Delete()
	})

	ctx := context.Background()
	if err := service.Install(ctx, opts); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}

	status, err := service.Status(ctx)
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if status.State != StateStopped {
		t.Errorf("the state = %q, want %q", status.State, StateStopped)
	}
	if status.Name != ServiceName {
		t.Errorf("the name = %q, want %q", status.Name, ServiceName)
	}
	if status.Executable != opts.Executable {
		t.Errorf("the registered executable = %q, want %q", status.Executable, opts.Executable)
	}
	if status.Listen != opts.Listen {
		t.Errorf("the registered listen address = %q, want %q", status.Listen, opts.Listen)
	}
	if status.Paths != opts.Paths {
		t.Errorf("the registered paths = %+v, want %+v", status.Paths, opts.Paths)
	}

	// A second installation updates the registration instead of creating
	// a second one.
	if err := service.Install(ctx, opts); err != nil {
		t.Fatalf("the second Install() = %v, want nil", err)
	}
	status, err = service.Status(ctx)
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if status.Executable != opts.Executable {
		t.Errorf("the registered executable = %q, want %q", status.Executable, opts.Executable)
	}

	// Stopping an instance that does not run is a stable success.
	if err := service.Stop(ctx); err != nil {
		t.Errorf("Stop() = %v, want nil for an instance that does not run", err)
	}

	// The removal leaves the state of the instance alone.
	if err := service.Uninstall(ctx); err != nil {
		t.Fatalf("Uninstall() = %v, want nil", err)
	}
	status, err = service.Status(ctx)
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if status.State != StateNotInstalled {
		t.Errorf("the state = %q, want %q", status.State, StateNotInstalled)
	}
	if _, err := os.Stat(opts.Paths.StateDir); err != nil {
		t.Errorf("the state directory of the instance is gone: %v", err)
	}

	// The removal of an absent registration is a clean success.
	if err := service.Uninstall(ctx); err != nil {
		t.Errorf("the second Uninstall() = %v, want nil", err)
	}
}

// TestSCMBrokenRegistration checks that a registration that points at a
// program that is gone is reported as broken.
//
// The decision is checked on the registered path alone: reading the command
// line of a registration needs a real service in the service control
// manager, which a test cannot create without an elevated shell.
func TestSCMBrokenRegistration(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "hfdesk.exe")
	if err := os.WriteFile(existing, []byte("not a program"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	missing := filepath.Join(root, "gone", "hfdesk.exe")

	// A registered program that is gone is a broken registration, and the
	// detail names the missing path.
	broken, detail := scmBrokenRegistration(missing)
	if !broken {
		t.Errorf("scmBrokenRegistration(%q) = %v, want a broken registration", missing, broken)
	}
	if !strings.Contains(detail, missing) {
		t.Errorf("the detail = %q, want the path of the missing program %q", detail, missing)
	}

	// A registered program that exists is not a broken registration.
	broken, detail = scmBrokenRegistration(existing)
	if broken {
		t.Errorf("scmBrokenRegistration(%q) = %v, want no broken registration", existing, broken)
	}
	if detail != "" {
		t.Errorf("the detail = %q, want none", detail)
	}

	// A registration that carries no program has nothing that could be
	// missing.
	broken, detail = scmBrokenRegistration("")
	if broken {
		t.Errorf("scmBrokenRegistration(\"\") = %v, want no broken registration", broken)
	}
	if detail != "" {
		t.Errorf("the detail = %q, want none", detail)
	}
}

// TestSDDLGrantsIdentity checks the access control list of a path against
// the account the instance runs as.
//
// The access control lists are the SDDL strings the platform writes: the
// trustees appear as raw SIDs and not as aliases.
func TestSDDLGrantsIdentity(t *testing.T) {
	cases := []struct {
		name    string
		sddl    string
		wantErr bool
	}{
		{"the account itself", "D:P(A;;FA;;;S-1-5-19)", false},
		{"everyone", "D:P(A;;FA;;;S-1-1-0)", false},
		{"the users group", "D:P(A;;FA;;;S-1-5-32-545)", false},
		{"the authenticated users group", "D:P(A;;FA;;;S-1-5-11)", false},
		{"the interactive users group", "D:P(A;;FA;;;S-1-5-4)", false},
		{"an unrelated account", "D:P(A;;FA;;;S-1-5-18)", true},
		{"an unrelated group", "D:P(A;;FA;;;S-1-5-32-544)", true},
		{"the alias of the account", "D:P(A;;FA;;;LS)", false},
		{"the alias of everyone", "D:P(A;;FA;;;WD)", false},
		{"the alias of the users group", "D:P(A;;FA;;;BU)", false},
		{"a denied entry next to an allowed one", "D:P(D;;GA;;;S-1-5-19)(A;;FA;;;S-1-1-0)", true},
		{"a denied entry for a group of the account", "D:P(D;;GA;;;S-1-1-0)(A;;FA;;;S-1-5-19)", true},
		{"read only rights", "D:P(A;;FR;;;S-1-5-19)", true},
		{"write only rights", "D:P(A;;FW;;;S-1-5-19)", true},
		{"the generic rights", "D:P(A;;0xC0000000;;;S-1-5-19)", false},
		{"the file rights", "D:P(A;;0x0013019F;;;S-1-5-19)", false},
		{"an empty list", "D:P", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := sddlGrantsIdentity(tc.sddl)
			if tc.wantErr && err == nil {
				t.Errorf("sddlGrantsIdentity(%q) = nil, want an error", tc.sddl)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("sddlGrantsIdentity(%q) = %v, want nil", tc.sddl, err)
			}
		})
	}
}

// TestPreflightDataPathRefusesAFile checks that a file where the instance
// needs a directory is refused.
func TestPreflightDataPathRefusesAFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cache")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	err := preflightDataPath("cache dir", path)
	if err == nil {
		t.Fatalf("preflightDataPath(%q) = nil, want an error", path)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the reason", path, err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the path", path, err)
	}
}

// TestPreflightDataPathRefusesAPathTheIdentityCannotReach checks that a
// directory the account of the instance cannot reach is refused before the
// registration is written.
//
// The directory does not exist yet: the nearest existing ancestor has to
// grant the account the access, otherwise the installation would leave a
// directory behind that the instance cannot use.
func TestPreflightDataPathRefusesAPathTheIdentityCannotReach(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	// The directory grants the machine and its administrators the access,
	// not the account the instance runs as.
	scmTestSetDACL(t, parent, fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)", scmTestCurrentUserSID(t)))

	path := filepath.Join(parent, "cache")
	err := preflightDataPath("cache dir", path)
	if err == nil {
		t.Fatalf("preflightDataPath(%q) = nil, want an error", path)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the path", path, err)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the account of the instance", path, err)
	}
	// The refused directory is not created behind the back of the user.
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the refused directory %q exists: %v", path, statErr)
	}
}

// TestApplyRegistrationReportsTheEffectiveCacheDir checks that the status
// reports the cache root the instance uses: the cache directory of the
// config file of the instance wins over the registered one.
func TestApplyRegistrationReportsTheEffectiveCacheDir(t *testing.T) {
	root := t.TempDir()
	configFile := filepath.Join(root, "hfdesk.json")
	registered := filepath.ToSlash(filepath.Join(root, "registered-cache"))
	fromConfig := filepath.ToSlash(filepath.Join(root, "cache-of-the-config-file"))

	opts := scmTestOptions()
	opts.Paths.ConfigFile = configFile
	opts.CacheDir = registered

	registration, err := parseSCMCommandLine(scmCommandLine(opts))
	if err != nil {
		t.Fatalf("parseSCMCommandLine() = %v, want nil", err)
	}

	status := Status{}
	applyRegistration(&status, registration)
	if status.CacheDir != registered {
		t.Errorf("the cache dir = %q, want the registered %q", status.CacheDir, registered)
	}
	if status.Paths.ConfigFile != configFile {
		t.Errorf("the config file = %q, want %q", status.Paths.ConfigFile, configFile)
	}

	// The config file of the instance wins over the registration.
	if err := os.WriteFile(configFile, []byte(`{"cache-dir":"`+fromConfig+`"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	status = Status{}
	applyRegistration(&status, registration)
	if status.CacheDir != fromConfig {
		t.Errorf("the cache dir = %q, want the cache dir of the config file %q", status.CacheDir, fromConfig)
	}

	// A registration without a cache root and a config file without one
	// report no cache root at all.
	opts.CacheDir = ""
	opts.Paths.ConfigFile = filepath.Join(root, "without-cache.json")
	registration, err = parseSCMCommandLine(scmCommandLine(opts))
	if err != nil {
		t.Fatalf("parseSCMCommandLine() = %v, want nil", err)
	}
	status = Status{}
	applyRegistration(&status, registration)
	if status.CacheDir != "" {
		t.Errorf("the cache dir = %q, want none", status.CacheDir)
	}
}

// TestInstanceStatePathsProtectEveryOwnedPath checks that every path the
// instance owns carries the restrictive descriptor: the directory of the
// config file, the state directory, the log directory and the config file
// itself.
func TestInstanceStatePathsProtectEveryOwnedPath(t *testing.T) {
	root := t.TempDir()
	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	if err := os.MkdirAll(filepath.Dir(opts.Paths.ConfigFile), 0o700); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	if err := os.WriteFile(opts.Paths.ConfigFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	want := []string{
		filepath.Dir(opts.Paths.ConfigFile),
		opts.Paths.StateDir,
		opts.Paths.LogDir,
		opts.Paths.ConfigFile,
	}
	got := instanceStatePaths(opts)
	if len(got) != len(want) {
		t.Fatalf("instanceStatePaths() = %v, want %v", got, want)
	}
	protected := make(map[string]bool, len(got))
	for _, path := range got {
		protected[path] = true
	}
	for _, path := range want {
		if !protected[path] {
			t.Errorf("instanceStatePaths() = %v, want %v", got, want)
		}
	}

	// A config file that does not exist yet inherits the descriptor of its
	// directory, so it is not part of the paths.
	if err := os.Remove(opts.Paths.ConfigFile); err != nil {
		t.Fatalf("Remove() = %v, want nil", err)
	}
	got = instanceStatePaths(opts)
	if len(got) != len(want)-1 {
		t.Fatalf("instanceStatePaths() = %v, want the three directories of the instance", got)
	}
	for _, path := range got {
		if path == opts.Paths.ConfigFile {
			t.Errorf("instanceStatePaths() = %v, want no config file", got)
		}
	}
}

// scmTestCurrentUserSID returns the SID of the account the test runs as.
//
// The account is needed by the tests that build an access control list: the
// list has to keep the access of the test itself, so the test can still
// read the directory it restricted.
func scmTestCurrentUserSID(t *testing.T) string {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Skipf("the account of the process is unknown: %v", err)
	}
	if user.User.Sid == nil {
		t.Skip("the account of the process carries no SID")
	}
	return user.User.Sid.String()
}

// scmTestSetDACL applies an access control list to a path.
//
// The descriptor is applied to the path itself and never recursively.
func scmTestSetDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("SecurityDescriptorFromString(%q) = %v, want nil", sddl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("the access control list of %q cannot be read: %v", sddl, err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatalf("the access control list %q cannot be applied to %q: %v", sddl, path, err)
	}
}
