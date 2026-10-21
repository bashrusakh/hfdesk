// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
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
	opts.User = secretSentinel
	opts.UID = secretSentinel

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
