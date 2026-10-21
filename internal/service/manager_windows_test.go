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
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/server"
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

// TestSCMClassifyRegistrationError checks that the outcome of the
// existence check tells the three cases apart: the registration is not
// there, the registration is there, and the caller may not look at it.
func TestSCMClassifyRegistrationError(t *testing.T) {
	// A registration that is there.
	state, err := scmClassifyRegistrationError(nil)
	if state != scmRegistrationPresent {
		t.Errorf("scmClassifyRegistrationError(nil) = %v, want %v", state, scmRegistrationPresent)
	}
	if err != nil {
		t.Errorf("scmClassifyRegistrationError(nil) = %v, want no error", err)
	}

	// A registration that is not there is not an error: it is the state
	// the caller asked about.
	state, err = scmClassifyRegistrationError(windows.ERROR_SERVICE_DOES_NOT_EXIST)
	if state != scmRegistrationAbsent {
		t.Errorf("scmClassifyRegistrationError(ERROR_SERVICE_DOES_NOT_EXIST) = %v, want %v", state, scmRegistrationAbsent)
	}
	if err != nil {
		t.Errorf("scmClassifyRegistrationError(ERROR_SERVICE_DOES_NOT_EXIST) = %v, want no error", err)
	}

	// A registration the caller may not look at is not an absent
	// registration, and the raw access denied error is not reported: the
	// elevation requirement is the truthful reason.
	state, err = scmClassifyRegistrationError(windows.ERROR_ACCESS_DENIED)
	if state != scmRegistrationUnreadable {
		t.Errorf("scmClassifyRegistrationError(ERROR_ACCESS_DENIED) = %v, want %v", state, scmRegistrationUnreadable)
	}
	if err != nil {
		t.Errorf("scmClassifyRegistrationError(ERROR_ACCESS_DENIED) = %v, want no error", err)
	}

	// A failure that is not about the registration is reported as it is.
	other := errors.New("the service control manager is gone")
	state, err = scmClassifyRegistrationError(other)
	if state != scmRegistrationUnreadable {
		t.Errorf("scmClassifyRegistrationError(%v) = %v, want %v", other, state, scmRegistrationUnreadable)
	}
	if !errors.Is(err, other) {
		t.Errorf("scmClassifyRegistrationError(%v) = %v, want the error itself", other, err)
	}
}

// TestSCMUninstallActionFor checks the decision of the uninstallation: an
// absent registration needs nothing, a registered one has to be removed,
// and a registration the caller may not look at needs an elevated shell.
func TestSCMUninstallActionFor(t *testing.T) {
	cases := []struct {
		name  string
		state scmRegistrationState
		want  scmUninstallAction
	}{
		{"absent", scmRegistrationAbsent, scmUninstallNothing},
		{"present", scmRegistrationPresent, scmUninstallRemove},
		{"unreadable", scmRegistrationUnreadable, scmUninstallElevate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmUninstallActionFor(tc.state); got != tc.want {
				t.Errorf("scmUninstallActionFor(%v) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

// TestSCMUninstallOfAnAbsentRegistrationIsACleanSuccess checks that the
// uninstallation of an instance that is not registered is a clean success
// in every shell.
//
// The decision is driven from both sides: the state of the registration
// and the state of the token of the process are parameters, so the test
// needs neither a registration nor an elevated shell of its own. The real
// command is checked as well, against the service control manager of the
// machine.
func TestSCMUninstallOfAnAbsentRegistrationIsACleanSuccess(t *testing.T) {
	manager := &scmManager{scope: ScopeSystem}
	ctx := context.Background()

	// The decision does not ask for an elevated shell: a registration
	// that is not there is uninstalled already.
	for _, elevated := range []bool{false, true} {
		if err := manager.uninstall(ctx, scmRegistrationAbsent, elevated); err != nil {
			t.Errorf("uninstall(absent, elevated=%v) = %v, want nil", elevated, err)
		}
	}

	// The real command against the service control manager of the
	// machine.
	state, err := scmRegistrationStateOf()
	if err != nil {
		t.Skipf("the service control manager is not available: %v", err)
	}
	if state != scmRegistrationAbsent {
		t.Skipf("%s is registered, the test would remove a real installation", ServiceName)
	}
	if err := manager.Uninstall(ctx); err != nil {
		t.Errorf("Uninstall() = %v, want nil for an absent registration", err)
	}
}

// TestSCMUninstallOfARegisteredInstanceNeedsElevation checks that the
// removal of a registration asks for an elevated shell.
//
// The state of the registration is a parameter of the decision, so the
// test never opens the registration of the instance: the elevation
// requirement is answered before the service control manager is asked for
// anything.
func TestSCMUninstallOfARegisteredInstanceNeedsElevation(t *testing.T) {
	manager := &scmManager{scope: ScopeSystem}
	err := manager.uninstall(context.Background(), scmRegistrationPresent, false)
	if err == nil {
		t.Fatal("uninstall(present) = nil, want an error that names the elevated shell")
	}
	if !strings.Contains(err.Error(), "elevated") {
		t.Errorf("uninstall(present) = %v, want an error that names the elevated shell", err)
	}
	if !strings.Contains(err.Error(), "administrator") {
		t.Errorf("uninstall(present) = %v, want an error that tells the user what to do", err)
	}
}

// TestSCMUninstallOfAnUnreadableRegistrationNeedsElevation checks that a
// registration the caller may not look at is reported as the elevation
// requirement it is, and not as the raw access denied error of the
// platform.
func TestSCMUninstallOfAnUnreadableRegistrationNeedsElevation(t *testing.T) {
	manager := &scmManager{scope: ScopeSystem}
	for _, elevated := range []bool{false, true} {
		err := manager.uninstall(context.Background(), scmRegistrationUnreadable, elevated)
		if err == nil {
			t.Fatalf("uninstall(unreadable, elevated=%v) = nil, want an error", elevated)
		}
		if !strings.Contains(err.Error(), "elevated") {
			t.Errorf("uninstall(unreadable, elevated=%v) = %v, want an error that names the elevated shell", elevated, err)
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Errorf("uninstall(unreadable, elevated=%v) = %v, want the elevation requirement and not the raw access denied error", elevated, err)
		}
	}
}

// TestSCMRefusedReportsTheElevationRequirement checks that a refusal of the
// service control manager because the shell has no rights is reported as
// the elevation requirement it is, and not as the raw access denied error
// of the platform.
func TestSCMRefusedReportsTheElevationRequirement(t *testing.T) {
	for _, action := range []string{"a start", "a stop"} {
		err := scmRefused(action, fmt.Errorf("service: start %s: %w", ServiceName, windows.ERROR_ACCESS_DENIED))
		if err == nil {
			t.Fatalf("scmRefused(%q, access denied) = nil, want an error", action)
		}
		if !strings.Contains(err.Error(), "elevated") {
			t.Errorf("scmRefused(%q, access denied) = %v, want an error that names the elevated shell", action, err)
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Errorf("scmRefused(%q, access denied) = %v, want the elevation requirement and not the raw access denied error", action, err)
		}
	}

	// A failure that is not about the rights of the shell is reported as
	// it is.
	other := errors.New("the service control manager is gone")
	if got := scmRefused("a start", other); !errors.Is(got, other) {
		t.Errorf("scmRefused(%q, %v) = %v, want the error itself", "a start", other, got)
	}
	if got := scmRefused("a start", nil); got != nil {
		t.Errorf("scmRefused(%q, nil) = %v, want nil", "a start", got)
	}
}

// TestSCMRegistrationStateOfFindsAnAbsentRegistration checks the existence
// check against the service control manager of the machine: a name that
// cannot be registered is reported as absent.
//
// The check reads the registration with the right to query its state only,
// so it works in a normal shell as well.
func TestSCMRegistrationStateOfFindsAnAbsentRegistration(t *testing.T) {
	name := fmt.Sprintf("HfdeskTestAbsentRegistration%d", time.Now().UnixNano())
	state, err := scmRegistrationStateOfName(name)
	if err != nil {
		t.Skipf("the service control manager is not available: %v", err)
	}
	if state == scmRegistrationUnreadable {
		t.Skip("the service control manager cannot be read from this shell")
	}
	if state != scmRegistrationAbsent {
		t.Errorf("scmRegistrationStateOfName(%q) = %v, want %v", name, state, scmRegistrationAbsent)
	}
}

// TestSCMRegistrationStateOfFindsARegisteredService checks the existence
// check against the service control manager of the machine: a service of
// the operating system is reported as registered.
//
// The test needs a service that is registered on the machine. The
// candidates are the services every installation of the operating system
// carries, so the test is skipped when the machine carries none of them.
func TestSCMRegistrationStateOfFindsARegisteredService(t *testing.T) {
	for _, candidate := range []string{"EventLog", "RpcSs", "Dhcp", "W32Time", "NlaSvc", "Schedule", "Power"} {
		state, err := scmRegistrationStateOfName(candidate)
		if err != nil {
			t.Skipf("the service control manager is not available: %v", err)
		}
		switch state {
		case scmRegistrationPresent:
			return
		case scmRegistrationAbsent:
			continue
		default:
			t.Skipf("the registration of %s cannot be read from this shell", candidate)
		}
	}
	t.Skip("the machine carries none of the services of the operating system")
}

// TestSCMStartAndStopOfAnAbsentRegistration checks the lifecycle commands
// against an instance that is not registered: the stop is a clean success
// and the start reports that the instance is not installed.
//
// The test is skipped when the instance is registered, so it never touches
// a real installation.
func TestSCMStartAndStopOfAnAbsentRegistration(t *testing.T) {
	state, err := scmRegistrationStateOf()
	if err != nil {
		t.Skipf("the service control manager is not available: %v", err)
	}
	if state != scmRegistrationAbsent {
		t.Skipf("%s is registered, the test would touch a real installation", ServiceName)
	}

	manager := &scmManager{scope: ScopeSystem}
	ctx := context.Background()

	// An instance that does not run is stopped already.
	if err := manager.Stop(ctx); err != nil {
		t.Errorf("Stop() = %v, want nil for an instance that is not registered", err)
	}
	// An instance that is not registered cannot be started, and the
	// reason is reported in every shell.
	err = manager.Start(ctx)
	if err == nil {
		t.Fatal("Start() = nil, want an error for an instance that is not registered")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("Start() = %v, want an error that names the missing registration", err)
	}
}

// TestSCMStopWaitCoversTheShutdownOfTheServer checks that the wait of the
// manager for the stopped instance covers the graceful shutdown of the
// server.
//
// The server publishes the budget of its own orderly shutdown: the drain of
// the HTTP server and the close of the job manager, each with its own budget.
// The manager has to wait at least the total of those budgets, or it reports
// a failed stop while the instance is still saving its state.
//
// The wait is the shared budget of the service layer and not a number of its
// own: a second copy of the same decision would drift below the budget of
// the server.
func TestSCMStopWaitCoversTheShutdownOfTheServer(t *testing.T) {
	if ShutdownBudget < server.GracefulShutdownTimeout {
		t.Errorf("the shared budget of the service layer = %s, want at least %s", ShutdownBudget, server.GracefulShutdownTimeout)
	}
	if scmStopTimeout < server.GracefulShutdownTimeout {
		t.Errorf("the stop wait of the manager = %s, want at least %s: the wait of the manager has to cover the graceful shutdown of the server", scmStopTimeout, server.GracefulShutdownTimeout)
	}
	if scmStopTimeout != ShutdownBudget {
		t.Errorf("the stop wait of the manager = %s, want the shared budget %s", scmStopTimeout, ShutdownBudget)
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
	// The instance never ran: the service control manager reports the
	// code of a service that was never started for it, and that code is
	// no failure of the instance.
	if status.Detail != "" {
		t.Errorf("the detail = %q, want none for an instance that never ran", status.Detail)
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

// TestSCMStatusFromRegistrationReportsTheStateAndTheCommandLine checks
// that the status of a registered instance is built out of the facts of a
// read-only lookup.
//
// The mapping is checked without a registration of its own: reading the
// command line of a registration needs a real service in the service
// control manager, which a test cannot create without an elevated shell.
func TestSCMStatusFromRegistrationReportsTheStateAndTheCommandLine(t *testing.T) {
	root := t.TempDir()
	opts := scmTestOptions()
	opts.Executable = filepath.Join(root, "hfdesk.exe")
	if err := os.WriteFile(opts.Executable, []byte("not a program"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	facts := scmRegistrationFacts{
		present:     true,
		state:       svc.Running,
		commandLine: scmCommandLine(opts),
	}

	status, err := scmStatusFrom(ScopeSystem, facts)
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil", err)
	}
	if status.State != StateRunning {
		t.Errorf("the state = %q, want %q", status.State, StateRunning)
	}
	if status.Scope != ScopeSystem {
		t.Errorf("the scope = %q, want %q", status.Scope, ScopeSystem)
	}
	if status.Name != ServiceName {
		t.Errorf("the name = %q, want %q", status.Name, ServiceName)
	}
	if status.UnitPath != scmRegistrationPath {
		t.Errorf("the registration path = %q, want %q", status.UnitPath, scmRegistrationPath)
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
	if status.CacheDir != opts.CacheDir {
		t.Errorf("the cache dir = %q, want %q", status.CacheDir, opts.CacheDir)
	}
}

// TestSCMStatusFromAnAbsentRegistrationIsNotInstalled checks that an
// instance that is not registered is reported as not installed, without an
// error.
func TestSCMStatusFromAnAbsentRegistrationIsNotInstalled(t *testing.T) {
	status, err := scmStatusFrom(ScopeSystem, scmRegistrationFacts{})
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil for an absent registration", err)
	}
	if status.State != StateNotInstalled {
		t.Errorf("the state = %q, want %q", status.State, StateNotInstalled)
	}
	if status.Scope != ScopeSystem {
		t.Errorf("the scope = %q, want %q", status.Scope, ScopeSystem)
	}
	if status.Name != ServiceName {
		t.Errorf("the name = %q, want %q", status.Name, ServiceName)
	}
	if !strings.Contains(status.Detail, ServiceName) {
		t.Errorf("the detail = %q, want the name of the missing registration %q", status.Detail, ServiceName)
	}
}

// TestSCMStatusFromABrokenRegistrationNamesTheMissingExecutable checks
// that a registration that points at a program that is gone is reported as
// broken, and that the detail names the missing program.
//
// The broken registration is the case a normal shell has to be able to
// see: the registration is readable, the program behind it is not there
// anymore.
func TestSCMStatusFromABrokenRegistrationNamesTheMissingExecutable(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "gone", "hfdesk.exe")
	opts := scmTestOptions()
	opts.Executable = missing

	facts := scmRegistrationFacts{
		present:     true,
		state:       svc.Stopped,
		commandLine: scmCommandLine(opts),
	}

	status, err := scmStatusFrom(ScopeSystem, facts)
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil", err)
	}
	if status.State != StateBroken {
		t.Errorf("the state = %q, want %q", status.State, StateBroken)
	}
	if !strings.Contains(status.Detail, missing) {
		t.Errorf("the detail = %q, want the path of the missing executable %q", status.Detail, missing)
	}
}

// TestSCMStatusFromAFailedInstanceIsFailedAndNamesTheExitCode checks that
// an instance that crashed is reported as failed, and that the detail names
// the exit code it ended with.
//
// The service control manager has no state of its own for a failure: it
// reports a crashed instance as stopped, like a cleanly stopped one. The
// exit code is what tells the two apart, and the managers of the other
// platforms report the same crash as failed.
func TestSCMStatusFromAFailedInstanceIsFailedAndNamesTheExitCode(t *testing.T) {
	root := t.TempDir()
	opts := scmTestOptions()
	opts.Executable = filepath.Join(root, "hfdesk.exe")
	if err := os.WriteFile(opts.Executable, []byte("not a program"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	facts := scmRegistrationFacts{
		present:     true,
		state:       svc.Stopped,
		exitCode:    3,
		commandLine: scmCommandLine(opts),
	}

	status, err := scmStatusFrom(ScopeSystem, facts)
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil", err)
	}
	if status.State != StateFailed {
		t.Errorf("the state = %q, want %q", status.State, StateFailed)
	}
	if !strings.Contains(status.Detail, "3") {
		t.Errorf("the detail = %q, want the exit code of the instance", status.Detail)
	}
}

// TestSCMStatusFromACleanlyStoppedInstanceIsStopped checks that an
// instance that ended cleanly is reported as stopped, and not as failed.
//
// The exit code of a clean run is zero, so the instance is not a failed one:
// the two cases are told apart by the exit code alone.
func TestSCMStatusFromACleanlyStoppedInstanceIsStopped(t *testing.T) {
	root := t.TempDir()
	opts := scmTestOptions()
	opts.Executable = filepath.Join(root, "hfdesk.exe")
	if err := os.WriteFile(opts.Executable, []byte("not a program"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	facts := scmRegistrationFacts{
		present:     true,
		state:       svc.Stopped,
		commandLine: scmCommandLine(opts),
	}

	status, err := scmStatusFrom(ScopeSystem, facts)
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil", err)
	}
	if status.State != StateStopped {
		t.Errorf("the state = %q, want %q", status.State, StateStopped)
	}
	if status.Detail != "" {
		t.Errorf("the detail = %q, want none", status.Detail)
	}
}

// TestSCMStatusFromANeverStartedInstanceIsStopped checks that an instance
// that never ran is reported as stopped and not as failed.
//
// The service control manager reports the code of a service that was never
// started (ERROR_SERVICE_NEVER_STARTED) as the exit code of an instance
// that never ran. That code is no failure of the instance: it never ran, the
// way a unit that never ran is reported as stopped by the managers of the
// other platforms.
func TestSCMStatusFromANeverStartedInstanceIsStopped(t *testing.T) {
	root := t.TempDir()
	opts := scmTestOptions()
	opts.Executable = filepath.Join(root, "hfdesk.exe")
	if err := os.WriteFile(opts.Executable, []byte("not a program"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	facts := scmRegistrationFacts{
		present:     true,
		state:       svc.Stopped,
		exitCode:    uint32(windows.ERROR_SERVICE_NEVER_STARTED),
		commandLine: scmCommandLine(opts),
	}

	status, err := scmStatusFrom(ScopeSystem, facts)
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil", err)
	}
	if status.State != StateStopped {
		t.Errorf("the state = %q, want %q", status.State, StateStopped)
	}
	if status.Detail != "" {
		t.Errorf("the detail = %q, want none", status.Detail)
	}
}

// TestSCMStatusFromKeepsTheOtherSupervisorStates checks that the exit code
// of an earlier run does not turn an instance that runs, that starts or that
// stops into a failed one.
//
// The exit code is only read for an instance that is stopped: every other
// state of the supervisor is reported as it is, the way the managers of the
// other platforms report it.
func TestSCMStatusFromKeepsTheOtherSupervisorStates(t *testing.T) {
	cases := []struct {
		name  string
		state svc.State
		want  State
	}{
		{"running", svc.Running, StateRunning},
		{"start pending", svc.StartPending, StateStarting},
		{"stop pending", svc.StopPending, StateStopping},
		{"paused", svc.Paused, StateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			opts := scmTestOptions()
			opts.Executable = filepath.Join(root, "hfdesk.exe")
			if err := os.WriteFile(opts.Executable, []byte("not a program"), 0o600); err != nil {
				t.Fatalf("WriteFile() = %v, want nil", err)
			}

			facts := scmRegistrationFacts{
				present:     true,
				state:       tc.state,
				exitCode:    3,
				commandLine: scmCommandLine(opts),
			}

			status, err := scmStatusFrom(ScopeSystem, facts)
			if err != nil {
				t.Fatalf("scmStatusFrom() = %v, want nil", err)
			}
			if status.State != tc.want {
				t.Errorf("the state = %q, want %q", status.State, tc.want)
			}
			if status.Detail != "" {
				t.Errorf("the detail = %q, want none", status.Detail)
			}
		})
	}
}

// TestSCMLookupErrorReportsTheElevationRequirement checks that a caller
// that may not read the registration is told about the elevation
// requirement, and not about the raw access denied error of the platform.
func TestSCMLookupErrorReportsTheElevationRequirement(t *testing.T) {
	err := scmLookupError("open the registration of "+ServiceName, windows.ERROR_ACCESS_DENIED)
	if err == nil {
		t.Fatal("scmLookupError(access denied) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "elevated") {
		t.Errorf("scmLookupError(access denied) = %v, want an error that names the elevated shell", err)
	}
	if !strings.Contains(err.Error(), "administrator") {
		t.Errorf("scmLookupError(access denied) = %v, want an error that tells the user what to do", err)
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Errorf("scmLookupError(access denied) = %v, want the elevation requirement and not the raw access denied error", err)
	}

	// A failure that is not about the rights of the shell is reported as
	// it is.
	other := errors.New("the service control manager is gone")
	if got := scmLookupError("connect to the service control manager", other); !errors.Is(got, other) {
		t.Errorf("scmLookupError(%v) = %v, want the error itself", other, got)
	}
	if got := scmLookupError("connect to the service control manager", nil); got != nil {
		t.Errorf("scmLookupError(nil) = %v, want nil", got)
	}
}

// TestSCMLookupRegistrationReportsAnAbsentRegistration checks the
// read-only lookup against the service control manager of the machine: a
// name that cannot be registered is reported as absent, without an error.
func TestSCMLookupRegistrationReportsAnAbsentRegistration(t *testing.T) {
	name := fmt.Sprintf("HfdeskTestAbsentRegistration%d", time.Now().UnixNano())
	facts, err := scmLookupRegistration(name)
	if err != nil {
		t.Skipf("the service control manager is not available: %v", err)
	}
	if facts.present {
		t.Errorf("scmLookupRegistration(%q) = a registered instance, want an absent registration", name)
	}

	status, err := scmStatusFrom(ScopeSystem, facts)
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil for an absent registration", err)
	}
	if status.State != StateNotInstalled {
		t.Errorf("the state = %q, want %q", status.State, StateNotInstalled)
	}
}

// TestSCMLookupRegistrationReadsARegisteredService checks the read-only
// lookup against the service control manager of the machine: a service of
// the operating system is reported with its state and its registered
// command line.
//
// The lookup needs no elevated shell: it opens the service control manager
// with the right to connect and the registration with the right to query
// its state and its configuration.
func TestSCMLookupRegistrationReadsARegisteredService(t *testing.T) {
	name := scmTestFindReadableService(t)
	facts, err := scmLookupRegistration(name)
	if err != nil {
		t.Fatalf("scmLookupRegistration(%q) = %v, want nil", name, err)
	}
	if !facts.present {
		t.Errorf("scmLookupRegistration(%q) = an absent registration, want a registered one", name)
	}
	if facts.commandLine == "" {
		t.Errorf("the command line of %q is empty, want the registered command line", name)
	}
	if facts.state == 0 {
		t.Errorf("the state of %q = %d, want a state of the supervisor", name, facts.state)
	}
}

// TestSCMLookupRegistrationNeedsNoElevatedShell checks that the read-only
// lookup reads a registration where the privileged connection of the
// platform is refused.
//
// The privileged connection asks for every right of the service control
// manager, so a normal shell does not have it. The read-only lookup opens
// the manager with the right to connect and the registration with the
// rights a status needs, so it reads the state of a registration in a
// normal shell.
func TestSCMLookupRegistrationNeedsNoElevatedShell(t *testing.T) {
	manager, err := mgr.Connect()
	if err == nil {
		manager.Disconnect()
		t.Skip("the shell of the test is elevated, the read-only path cannot be told apart from the privileged one")
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Skipf("the service control manager is not available: %v", err)
	}

	name := scmTestFindReadableService(t)
	facts, err := scmLookupRegistration(name)
	if err != nil {
		t.Fatalf("scmLookupRegistration(%q) = %v, want nil", name, err)
	}
	if !facts.present {
		t.Errorf("scmLookupRegistration(%q) = an absent registration, want a registered one", name)
	}
}

// scmTestFindReadableService returns the name of a service of the
// operating system that this shell may read.
//
// The candidates are the services every installation of the operating
// system carries. The test is skipped when the machine carries none of
// them that this shell may read.
func scmTestFindReadableService(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"EventLog", "RpcSs", "Dhcp", "W32Time", "NlaSvc", "Schedule", "Power"} {
		facts, err := scmLookupRegistration(candidate)
		if err != nil || !facts.present || facts.commandLine == "" {
			// The service is not there or not readable from this
			// shell: the next candidate is tried.
			continue
		}
		return candidate
	}
	t.Skip("the machine carries no service that this shell may read")
	return ""
}

// TestSCMStatusOfAnAbsentRegistrationIsNotInstalled checks the status of
// an instance that is not registered: it reports that the instance is not
// installed, and it does so without an error and without an elevated
// shell.
//
// The test is skipped when the instance is registered, so it never reads a
// real installation.
func TestSCMStatusOfAnAbsentRegistrationIsNotInstalled(t *testing.T) {
	state, err := scmRegistrationStateOf()
	if err != nil {
		t.Skipf("the service control manager is not available: %v", err)
	}
	if state != scmRegistrationAbsent {
		t.Skipf("%s is registered, the test would read a real installation", ServiceName)
	}

	manager := &scmManager{scope: ScopeSystem}
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() = %v, want nil for an absent registration", err)
	}
	if status.State != StateNotInstalled {
		t.Errorf("the state = %q, want %q", status.State, StateNotInstalled)
	}
	if status.Name != ServiceName {
		t.Errorf("the name = %q, want %q", status.Name, ServiceName)
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
//
// The set of the trustees that cover the account is the certain subset: the
// account itself and everybody. It is the set the check falls back to when
// the platform refuses the logon of the account, and the effective
// membership of the account is checked by TestSDDLGrantsIdentityWithTheEffectiveGroups.
func TestSDDLGrantsIdentity(t *testing.T) {
	identity := scmCertainIdentitySIDs(errors.New("the test checks the certain subset"))
	cases := []struct {
		name    string
		sddl    string
		wantErr bool
	}{
		{"the account itself", "D:P(A;;FA;;;S-1-5-19)", false},
		{"the alias of the account", "D:P(A;;FA;;;LS)", false},
		{"everyone", "D:P(A;;FA;;;S-1-1-0)", false},
		{"the alias of everyone", "D:P(A;;FA;;;WD)", false},
		{"the interactive users group", "D:P(A;;FA;;;S-1-5-4)", true},
		{"the alias of the interactive users group", "D:P(A;;FA;;;IU)", true},
		{"the users group", "D:P(A;;FA;;;S-1-5-32-545)", true},
		{"the authenticated users group", "D:P(A;;FA;;;S-1-5-11)", true},
		{"an unrelated account", "D:P(A;;FA;;;S-1-5-18)", true},
		{"an unrelated group", "D:P(A;;FA;;;S-1-5-32-544)", true},
		{"a denied entry next to an allowed one", "D:P(D;;GA;;;S-1-5-19)(A;;FA;;;S-1-1-0)", true},
		{"a denied entry for everybody", "D:P(D;;GA;;;S-1-1-0)(A;;FA;;;S-1-5-19)", true},
		{"read only rights", "D:P(A;;FR;;;S-1-5-19)", true},
		{"write only rights", "D:P(A;;FW;;;S-1-5-19)", true},
		{"the generic rights", "D:P(A;;0xC0000000;;;S-1-5-19)", false},
		{"the file rights", "D:P(A;;0x0013019F;;;S-1-5-19)", false},
		{"an empty list", "D:P", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := sddlGrantsIdentity(tc.sddl, identity)
			if tc.wantErr && err == nil {
				t.Errorf("sddlGrantsIdentity(%q) = nil, want an error", tc.sddl)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("sddlGrantsIdentity(%q) = %v, want nil", tc.sddl, err)
			}
		})
	}
}

// TestSDDLGrantsIdentityWithTheEffectiveGroups checks the access control
// list of a path against the effective membership of the account the
// instance runs as.
//
// The membership comes from a token of the account, so a group the account
// really carries covers it and a group it does not carry does not.
func TestSDDLGrantsIdentityWithTheEffectiveGroups(t *testing.T) {
	identity := scmTestEffectiveIdentitySIDs(t)

	// The account itself.
	if err := sddlGrantsIdentity("D:P(A;;FA;;;S-1-5-19)", identity); err != nil {
		t.Errorf("sddlGrantsIdentity() = %v, want nil for the account itself", err)
	}
	// Everybody.
	if err := sddlGrantsIdentity("D:P(A;;FA;;;S-1-1-0)", identity); err != nil {
		t.Errorf("sddlGrantsIdentity() = %v, want nil for everybody", err)
	}
	// The interactive users: the instance runs a service logon and is
	// never an interactive user.
	if err := sddlGrantsIdentity("D:P(A;;FA;;;S-1-5-4)", identity); err == nil {
		t.Error("sddlGrantsIdentity() = nil, want the refusal of the interactive users")
	}
	// A group the account does not carry.
	uncarried := scmTestUncarriedSID(t, identity)
	if err := sddlGrantsIdentity("D:P(A;;FA;;;"+uncarried+")", identity); err == nil {
		t.Errorf("sddlGrantsIdentity() = nil, want the refusal of the group %s", uncarried)
	}
	// Every group of the membership covers the account.
	for group := range identity.sids {
		if err := sddlGrantsIdentity("D:P(A;;FA;;;"+group+")", identity); err != nil {
			t.Errorf("sddlGrantsIdentity() = %v, want nil for the group %s the account carries", err, group)
		}
	}
}

// TestSCMEffectiveIdentitySIDs checks the membership the implementation
// derives out of a token of the account the instance runs as.
//
// The membership is the effective one: the account itself is in it, everybody
// is in it, and the interactive users are never in it, because the instance
// runs a service logon.
func TestSCMEffectiveIdentitySIDs(t *testing.T) {
	identity := scmTestEffectiveIdentitySIDs(t)

	if !identity.exact {
		t.Error("the membership of the account is not the effective one, want the membership of a token")
	}
	if !identity.covers(scmServiceIdentitySID) {
		t.Errorf("the membership %v does not carry the account %s", identity.sids, scmServiceIdentitySID)
	}
	if !identity.covers(scmEveryoneSID) {
		t.Errorf("the membership %v does not carry everybody", identity.sids)
	}
	if identity.covers(scmInteractiveUsersSID) {
		t.Errorf("the membership %v carries the interactive users, want a membership without them", identity.sids)
	}
}

// TestSddlGrantsIdentityNamesTheCertainSubset checks that a check that was
// made against the certain subset says so.
//
// The platform can refuse the logon of the account. The check then falls
// back to the subset that is certain, and the user has to know that the
// groups of the account were not known.
func TestSddlGrantsIdentityNamesTheCertainSubset(t *testing.T) {
	identity := scmCertainIdentitySIDs(errors.New("the logon of the account was refused"))

	err := sddlGrantsIdentity("D:P(A;;FA;;;S-1-5-32-544)", identity)
	if err == nil {
		t.Fatal("sddlGrantsIdentity() = nil, want the refusal of a group outside the certain subset")
	}
	if !strings.Contains(err.Error(), "everybody only") {
		t.Errorf("sddlGrantsIdentity() = %v, want an error that names the certain subset", err)
	}
	if !strings.Contains(err.Error(), "the logon of the account was refused") {
		t.Errorf("sddlGrantsIdentity() = %v, want an error that names the reason", err)
	}
}

// scmTestEffectiveIdentitySIDs returns the effective membership of the
// account the instance runs as.
//
// The membership is a property of the machine: the test skips when the
// platform refuses the logon of the account, because the machine cannot
// answer the question then.
func scmTestEffectiveIdentitySIDs(t *testing.T) scmIdentitySIDs {
	t.Helper()
	if !isElevatedProcess() {
		t.Skip("the logon of a service account needs an elevated shell, run the test as an administrator")
	}
	identity, err := scmReadServiceIdentitySIDs()
	if err != nil {
		t.Skipf("the account %s cannot be logged on as a service: %v", scmServiceIdentityName, err)
	}
	return identity
}

// scmTestUncarriedSID returns a SID the account of the instance does not
// carry.
//
// The candidates are the groups of the machine that the account of the
// instance is not a member of.
func scmTestUncarriedSID(t *testing.T, identity scmIdentitySIDs) string {
	t.Helper()
	for _, candidate := range []string{
		"S-1-5-32-544", // Administrators
		"S-1-5-18",     // LocalSystem
		"S-1-5-20",     // NetworkService
		"S-1-5-32-546", // Guests
		"S-1-5-4",      // Interactive Users
	} {
		if !identity.covers(candidate) {
			return candidate
		}
	}
	t.Skip("the account of the instance carries every candidate group")
	return ""
}

// scmTestNamesPath reports whether a message of the manager names a path.
//
// The messages of the manager quote the path they talk about, so the
// backslashes of a Windows path are escaped in them: the path of a message
// is the quoted form of the path, not the path itself. The check looks for
// both forms, so it does not depend on the way a message renders the path.
func scmTestNamesPath(message, path string) bool {
	return strings.Contains(message, path) || strings.Contains(message, strconv.Quote(path))
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
	if !scmTestNamesPath(err.Error(), path) {
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
	if !scmTestNamesPath(err.Error(), path) {
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

// TestPreflightDataPathRefusesADirectoryThatGrantsOnlyTheInteractiveUsers
// checks that a directory the instance can never use is refused.
//
// The instance runs a service logon and is never an interactive user, so a
// descriptor that grants the access to the interactive users of the machine
// only grants the access to the users of an interactive session. The
// preflight has to refuse the directory instead of accepting it.
func TestPreflightDataPathRefusesADirectoryThatGrantsOnlyTheInteractiveUsers(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cache")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	// The directory grants the access to the interactive users of the
	// machine, to the machine itself and to the account of the test: the
	// test has to keep the access to the directory it restricted.
	scmTestSetDACL(t, path, fmt.Sprintf(
		"D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;%s)",
		scmInteractiveUsersSID, scmTestCurrentUserSID(t)))

	err := preflightDataPath("cache dir", path)
	if err == nil {
		t.Fatalf("preflightDataPath(%q) = nil, want the refusal of a directory that grants the access to the interactive users only", path)
	}
	if !scmTestNamesPath(err.Error(), path) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the path", path, err)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the account of the instance", path, err)
	}
	if !strings.Contains(err.Error(), "grant the account") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that tells the user what to do", path, err)
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

// TestInstanceStatePathsTellTheCreatedPathsFromTheExistingOnes checks that
// the installation knows which paths of the instance it owns: the
// directories it creates carry the restrictive descriptor, the paths that
// existed before the installation do not.
//
// A directory that existed before the installation belongs to whoever owns
// it, so the installation reads its descriptor and changes nothing.
func TestInstanceStatePathsTellTheCreatedPathsFromTheExistingOnes(t *testing.T) {
	root := t.TempDir()
	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	opts.CacheDir = filepath.Join(root, "cache")
	opts.HubDir = filepath.Join(root, "cache", "hub")
	opts.LocalDir = filepath.Join(root, "models")

	// The config directory and the config file existed before the
	// installation.
	if err := os.MkdirAll(filepath.Dir(opts.Paths.ConfigFile), 0o700); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	if err := os.WriteFile(opts.Paths.ConfigFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	snapshot := newSCMInstallSnapshot(opts)
	paths := instanceStatePaths(opts, snapshot)

	created := make(map[string]bool, len(paths))
	existing := make(map[string]bool, len(paths))
	for _, path := range paths {
		if path.created {
			created[path.path] = true
			continue
		}
		existing[path.path] = true
	}

	// The directories the installation creates are the state directory
	// and the log directory: the config directory existed already.
	for _, want := range []string{opts.Paths.StateDir, opts.Paths.LogDir} {
		if !created[want] {
			t.Errorf("the state directory %q is not marked as created: %v", want, paths)
		}
	}
	for _, want := range []string{filepath.Dir(opts.Paths.ConfigFile), opts.Paths.ConfigFile} {
		if !existing[want] {
			t.Errorf("the path %q is not marked as existing: %v", want, paths)
		}
	}
	for path := range created {
		if existing[path] {
			t.Errorf("the path %q is marked as created and as existing: %v", path, paths)
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

// scmTestAliases maps the aliases of the trustees of an access control
// entry onto the SID they stand for.
//
// The platform writes a trustee as an alias or as a raw SID, depending on
// the entry. The tests compare the trustees as SIDs, so the form the
// platform uses does not matter.
var scmTestAliases = map[string]string{
	"SY": "S-1-5-18",     // LocalSystem
	"BA": "S-1-5-32-544", // Administrators
	"LS": "S-1-5-19",     // LocalService
	"WD": "S-1-1-0",      // Everyone
}

// scmTestDACL returns the access control list of a path in its SDDL form.
//
// The descriptor of a path carries the owner and the group next to the
// access control list: only the list is compared, so a change of the access
// rights is told apart from a change of the owner.
func scmTestDACL(t *testing.T, path string) string {
	t.Helper()
	_, permissions := scmTestSecurity(t, path)
	index := strings.Index(permissions, "D:")
	if index < 0 {
		t.Fatalf("the security descriptor of %q = %q, want an access control list", path, permissions)
	}
	return permissions[index:]
}

// scmTestTrustees returns the trustees of the access control list of a path.
//
// The trustees are returned as SIDs: the aliases of the platform are
// resolved, so the assertions do not depend on the form the platform writes
// them in.
func scmTestTrustees(t *testing.T, path string) []string {
	t.Helper()
	var trustees []string
	for _, ace := range parseSDDLAces(scmTestDACL(t, path)) {
		trustee := strings.ToUpper(strings.TrimSpace(ace.trustee))
		if sid, ok := scmTestAliases[trustee]; ok {
			trustee = sid
		}
		trustees = append(trustees, trustee)
	}
	return trustees
}

// scmTestDACLProtected reports whether the access control list of a path
// blocks the inheritance of its parent directory.
//
// The flag is read from the descriptor of the path itself and not from its
// SDDL form, so the assertion does not depend on the way the platform writes
// the flag.
func scmTestDACLProtected(t *testing.T, path string) bool {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("the security descriptor of %q cannot be read: %v", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("the control bits of %q cannot be read: %v", path, err)
	}
	return control&windows.SE_DACL_PROTECTED != 0
}

// TestSCMInstanceStateRestrictsTheDirectoriesItCreated checks that the
// directories the installation creates carry the restrictive descriptor of
// the instance.
//
// The descriptor is the one of the instance: SYSTEM, the administrators of
// the machine and the account the instance runs as, nobody else, and the
// inheritance of the parent directory is cut off.
func TestSCMInstanceStateRestrictsTheDirectoriesItCreated(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	if err := restrictInstanceState(opts, snapshot); err != nil {
		t.Fatalf("restrictInstanceState() = %v, want nil", err)
	}

	want := []string{"S-1-5-18", "S-1-5-19", "S-1-5-32-544"}
	for _, dir := range []string{
		filepath.Dir(opts.Paths.ConfigFile),
		opts.Paths.StateDir,
		opts.Paths.LogDir,
	} {
		if !scmTestDACLProtected(t, dir) {
			t.Errorf("the access control list of %q is not protected, want a protected one", dir)
		}
		got := scmTestTrustees(t, dir)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("the trustees of %q = %v, want %v", dir, got, want)
		}
	}
}

// TestSCMInstanceStateKeepsTheDescriptorOfADirectoryThatExistedBefore
// checks that a directory the user named and that existed before the
// installation keeps its descriptor.
//
// The descriptor of the directory grants the access to everybody, so a
// rewrite of it by the restrictive descriptor of the instance would be
// visible in the comparison.
func TestSCMInstanceStateKeepsTheDescriptorOfADirectoryThatExistedBefore(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	// The state directory existed before the installation.
	stateDir := opts.Paths.StateDir
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", stateDir, err)
	}
	scmTestSetDACL(t, stateDir, fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;WD)"))
	before := scmTestDACL(t, stateDir)

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	if err := restrictInstanceState(opts, snapshot); err != nil {
		t.Fatalf("restrictInstanceState() = %v, want nil", err)
	}

	if after := scmTestDACL(t, stateDir); after != before {
		t.Errorf("the access control list of the state directory %q = %q, want %q", stateDir, after, before)
	}
	// The restrictive descriptor of the instance was not applied to it:
	// everybody still has the access the directory had.
	trustees := scmTestTrustees(t, stateDir)
	if !slices.Contains(trustees, "S-1-1-0") {
		t.Errorf("the trustees of the state directory %q = %v, want everybody", stateDir, trustees)
	}
}

// TestSCMInstanceStateRefusesADirectoryTheIdentityCannotUse checks that a
// directory that existed before the installation and that the account of the
// instance cannot use is refused, and that no descriptor was changed.
//
// The refusal happens before the first descriptor is changed, so the
// directories the installation created keep the descriptor they inherited.
func TestSCMInstanceStateRefusesADirectoryTheIdentityCannotUse(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	// The state directory existed before the installation and grants the
	// access to the machine and to its administrators only.
	stateDir := opts.Paths.StateDir
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", stateDir, err)
	}
	scmTestSetDACL(t, stateDir, scmTestDenyInstanceIdentityDACL(t))

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	before := make(map[string]string, 3)
	for _, path := range []string{
		stateDir,
		filepath.Dir(opts.Paths.ConfigFile),
		opts.Paths.LogDir,
	} {
		before[path] = scmTestDACL(t, path)
	}

	err := restrictInstanceState(opts, snapshot)
	if err == nil {
		t.Fatal("restrictInstanceState() = nil, want the refusal of the state directory")
	}
	if !scmTestNamesPath(err.Error(), stateDir) {
		t.Errorf("restrictInstanceState() = %v, want an error that names the path", err)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("restrictInstanceState() = %v, want an error that names the account of the instance", err)
	}
	if !strings.Contains(err.Error(), "grant the account") {
		t.Errorf("restrictInstanceState() = %v, want an error that tells the user what to do", err)
	}

	// No descriptor was changed anywhere: not on the directory that
	// existed before and not on the directories the installation
	// created.
	for path, want := range before {
		if got := scmTestDACL(t, path); got != want {
			t.Errorf("the access control list of %q = %q, want %q", path, got, want)
		}
	}
}

// TestSCMInstanceStateKeepsTheDescriptorOfAConfigFileThatExistedBefore
// checks that the config file of the instance keeps its descriptor when it
// existed before the installation.
//
// The config file holds the credentials of the instance, but it belongs to
// whoever wrote it: the installation reads its descriptor and changes
// nothing.
func TestSCMInstanceStateKeepsTheDescriptorOfAConfigFileThatExistedBefore(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	// The config directory and the config file existed before the
	// installation.
	configDir := filepath.Dir(opts.Paths.ConfigFile)
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", configDir, err)
	}
	// The directory has to be usable by the account of the instance,
	// otherwise the installation refuses it.
	scmTestSetDACL(t, configDir, scmTestAllowInstanceIdentityDACL(t))
	if err := os.WriteFile(opts.Paths.ConfigFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	scmTestSetDACL(t, opts.Paths.ConfigFile, fmt.Sprintf("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;WD)"))
	before := scmTestDACL(t, opts.Paths.ConfigFile)

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	if err := restrictInstanceState(opts, snapshot); err != nil {
		t.Fatalf("restrictInstanceState() = %v, want nil", err)
	}

	if after := scmTestDACL(t, opts.Paths.ConfigFile); after != before {
		t.Errorf("the access control list of the config file %q = %q, want %q", opts.Paths.ConfigFile, after, before)
	}
}

// TestSCMInstanceStateKeepsTheDescriptorOfTheReusedDirectories checks that
// the directories the installation reuses keep their descriptor.
//
// The cache root, the hub directory and the local output directory can hold
// the models of the user: the installation reads their descriptor and never
// changes it.
func TestSCMInstanceStateKeepsTheDescriptorOfTheReusedDirectories(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	// The reused directories existed before the installation. The
	// descriptor of every one of them is protected, so the inheritance
	// of the platform cannot change it behind the back of the test.
	reused := []string{opts.CacheDir, opts.HubDir, opts.LocalDir}
	before := make(map[string]string, len(reused))
	for _, dir := range reused {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("MkdirAll(%q) = %v, want nil", dir, err)
		}
		scmTestSetDACL(t, dir, scmTestAllowInstanceIdentityDACL(t))
		before[dir] = scmTestDACL(t, dir)
	}
	// The cache root lives in the state directory of the instance, so the
	// state directory existed before the installation as well: it has to
	// be usable by the account of the instance.
	scmTestSetDACL(t, opts.Paths.StateDir, scmTestAllowInstanceIdentityDACL(t))

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	if err := restrictInstanceState(opts, snapshot); err != nil {
		t.Fatalf("restrictInstanceState() = %v, want nil", err)
	}
	// The preflight reads the reused directories as well.
	if err := preflightDataPaths(opts); err != nil {
		t.Fatalf("preflightDataPaths() = %v, want nil", err)
	}

	for _, dir := range reused {
		if after := scmTestDACL(t, dir); after != before[dir] {
			t.Errorf("the access control list of the reused directory %q = %q, want %q", dir, after, before[dir])
		}
	}
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

// scmTestInstallOptions returns the options of an installation whose
// directories live below the given root.
//
// The executable is a program of the operating system: the preflight needs a
// regular file it can stat. The cache root lives in the state directory of
// the instance, the way the default paths of a machine-wide instance build
// it.
func scmTestInstallOptions(t *testing.T, root string) InstallOptions {
	t.Helper()
	executable := filepath.Join("C:\\Windows", "System32", "cmd.exe")
	if _, err := os.Stat(executable); err != nil {
		t.Skipf("the test program %s is not available: %v", executable, err)
	}
	return InstallOptions{
		Scope:      ScopeSystem,
		Executable: executable,
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
		CacheDir: filepath.Join(root, "var", "lib", "hfdesk", "cache"),
		HubDir:   filepath.Join(root, "var", "lib", "hfdesk", "cache", "hub"),
		LocalDir: filepath.Join(root, "models"),
	}
}

// scmTestDenyInstanceIdentityDACL returns a descriptor that grants the
// machine, its administrators and the account of the test the access, and
// the account of the instance none.
//
// A directory with this descriptor is refused by the preflight: the account
// the instance runs as cannot work with it.
func scmTestDenyInstanceIdentityDACL(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)", scmTestCurrentUserSID(t))
}

// scmTestAllowInstanceIdentityDACL returns a descriptor that grants the
// machine, its administrators, the account of the instance and the account
// of the test the access.
//
// A directory with this descriptor passes the preflight: the account the
// instance runs as can work with it.
func scmTestAllowInstanceIdentityDACL(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s(A;OICI;FA;;;%s)", scmStateSDDL(), scmTestCurrentUserSID(t))
}

// scmTestSecurity returns the owner and the permissions of a path.
//
// The owner is the account that owns the path, the permissions are the
// security descriptor of the path in its SDDL form.
func scmTestSecurity(t *testing.T, path string) (string, string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("the security descriptor of %q cannot be read: %v", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatalf("the owner of %q cannot be read: %v", path, err)
	}
	if owner == nil {
		t.Fatalf("the owner of %q is unknown", path)
	}
	return owner.String(), sd.String()
}

// scmTestOwnerAndMode returns the owner and the mode of a path.
//
// The owner is the account that owns the path, the mode are the permission
// bits of the path.
func scmTestOwnerAndMode(t *testing.T, path string) (string, os.FileMode) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("the owner of %q cannot be read: %v", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatalf("the owner of %q cannot be read: %v", path, err)
	}
	if owner == nil {
		t.Fatalf("the owner of %q is unknown", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the mode of %q cannot be read: %v", path, err)
	}
	return owner.String(), info.Mode().Perm()
}

// scmTestRefuseLocalDir turns the local directory of an installation into a
// directory the account of the instance cannot reach.
//
// The preflight refuses the installation when it reaches the local
// directory, so the installation fails after the directories of the instance
// were created: the point a failed installation has to clean up.
func scmTestRefuseLocalDir(t *testing.T, opts InstallOptions) {
	t.Helper()
	if err := os.MkdirAll(opts.LocalDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", opts.LocalDir, err)
	}
	scmTestSetDACL(t, opts.LocalDir, scmTestDenyInstanceIdentityDACL(t))
}

// TestSCMCreatedDirOrder checks the order of the directories a failed
// installation removes: the directories are listed in the order they are
// created, so a parent is always removed after its children.
//
// The directories of the instance come first, the directories the
// installation reuses come afterwards: the preflight creates them once the
// directories of the instance exist.
func TestSCMCreatedDirOrder(t *testing.T) {
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	dirs := scmInstallDirs(opts)
	created := make(map[string][]string, len(dirs))
	for _, dir := range dirs {
		created[dir] = createdDirs(dir)
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
		filepath.Join(root, "var", "lib", "hfdesk", "cache"),
		filepath.Join(root, "var", "lib", "hfdesk", "cache", "hub"),
		filepath.Join(root, "models"),
	}
	if strings.Join(order, "\n") != strings.Join(want, "\n") {
		t.Errorf("createdDirOrder() =\n%s\nwant\n%s", strings.Join(order, "\n"), strings.Join(want, "\n"))
	}
}

// TestSCMFailedInstallRemovesTheDirectoriesItCreated checks that a failed
// installation removes the directories it created, so a failed attempt
// leaves the machine as it was.
//
// The installation is refused by the preflight: the local directory cannot
// be reached by the account of the instance. The refusal happens after the
// directories of the instance were created, so the cleanup has to remove
// them again.
func TestSCMFailedInstallRemovesTheDirectoriesItCreated(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)
	scmTestRefuseLocalDir(t, opts)

	service := &scmManager{scope: ScopeSystem}
	if err := service.Install(context.Background(), opts); err == nil {
		t.Fatal("Install() = nil, want the refusal of the local directory")
	}

	// Every directory the installation created is gone again: the cache
	// root of the instance is removed before the state directory that
	// holds it.
	for _, dir := range []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		filepath.Join(root, "var", "lib", "hfdesk"),
		filepath.Join(root, "var", "lib", "hfdesk", "cache"),
		filepath.Join(root, "var", "lib", "hfdesk", "cache", "hub"),
		filepath.Join(root, "var", "log"),
		filepath.Join(root, "var", "log", "hfdesk"),
	} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the directory %q is still there after the failed installation (stat error: %v)", dir, err)
		}
	}
}

// TestSCMFailedInstallKeepsADirectoryThatExistedBefore checks that a failed
// installation never removes a directory that existed before it: such a
// directory belongs to whoever owns it.
//
// The local directory is a directory the installation reuses: the
// installation never describes it, so its owner and its permissions are the
// ones it had. The config directory of the instance is a directory the
// installation owns as well: it is still there as well, with the owner it
// had and with the descriptor it had, because the installation never
// rewrites the descriptor of a path that existed before it.
func TestSCMFailedInstallKeepsADirectoryThatExistedBefore(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)
	scmTestRefuseLocalDir(t, opts)

	// The config directory of the instance existed before the
	// installation as well.
	configDir := filepath.Dir(opts.Paths.ConfigFile)
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", configDir, err)
	}
	// The directory has to be usable by the account of the instance:
	// the installation refuses it otherwise, before it reaches the local
	// directory.
	scmTestSetDACL(t, configDir, scmTestAllowInstanceIdentityDACL(t))
	configOwner, configMode := scmTestOwnerAndMode(t, configDir)
	configPermissions := scmTestDACL(t, configDir)

	owner, permissions := scmTestSecurity(t, opts.LocalDir)

	service := &scmManager{scope: ScopeSystem}
	if err := service.Install(context.Background(), opts); err == nil {
		t.Fatal("Install() = nil, want the refusal of the local directory")
	}

	// The directory the installation reuses is still there, with the
	// owner and the permissions it had.
	info, err := os.Stat(opts.LocalDir)
	if err != nil {
		t.Fatalf("the local directory %q is gone after the failed installation: %v", opts.LocalDir, err)
	}
	if !info.IsDir() {
		t.Errorf("the path %q is not a directory after the failed installation", opts.LocalDir)
	}
	gotOwner, gotPermissions := scmTestSecurity(t, opts.LocalDir)
	if gotOwner != owner {
		t.Errorf("the owner of the local directory %q = %q, want %q", opts.LocalDir, gotOwner, owner)
	}
	if gotPermissions != permissions {
		t.Errorf("the permissions of the local directory %q = %q, want %q", opts.LocalDir, gotPermissions, permissions)
	}

	// The directory of the instance that existed before the installation
	// is still there as well, with the owner, the mode and the
	// descriptor it had.
	if _, err := os.Stat(configDir); err != nil {
		t.Errorf("the config directory %q is gone after the failed installation: %v", configDir, err)
	}
	gotConfigOwner, gotConfigMode := scmTestOwnerAndMode(t, configDir)
	if gotConfigOwner != configOwner {
		t.Errorf("the owner of the config directory %q = %q, want %q", configDir, gotConfigOwner, configOwner)
	}
	if gotConfigMode != configMode {
		t.Errorf("the mode of the config directory %q = %v, want %v", configDir, gotConfigMode, configMode)
	}
	if gotConfigDACL := scmTestDACL(t, configDir); gotConfigDACL != configPermissions {
		t.Errorf("the access control list of the config directory %q = %q, want %q", configDir, gotConfigDACL, configPermissions)
	}
}

// TestSCMRemoveCreatedDirsKeepsADirectoryThatHoldsData checks that the
// cleanup of a failed installation never deletes data: a directory that
// holds something is kept, and the empty directories around it are removed.
//
// The directory that holds data is checked on the cleanup itself: an
// installation writes no file into the directories it creates, so a
// directory that holds something is a directory that holds the data of
// somebody else.
func TestSCMRemoveCreatedDirsKeepsADirectoryThatHoldsData(t *testing.T) {
	root := t.TempDir()
	holding := filepath.Join(root, "holding")
	if err := os.Mkdir(holding, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	marker := filepath.Join(holding, "marker")
	if err := os.WriteFile(marker, []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

	removeCreatedDirs([]string{holding, empty})

	// The directory that holds data and the file in it are still there.
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the file of the directory that holds data is gone: %v", err)
	}
	if _, err := os.Stat(holding); err != nil {
		t.Errorf("the directory %q that holds data is gone: %v", holding, err)
	}
	// The empty directory is gone.
	if _, err := os.Stat(empty); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the empty directory %q is still there (stat error: %v)", empty, err)
	}
}

// TestSCMFailedInstallReportsTheOriginalError checks that the cleanup of a
// failed installation never replaces the error of the installation: the
// caller gets the refusal of the preflight itself.
func TestSCMFailedInstallReportsTheOriginalError(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)
	scmTestRefuseLocalDir(t, opts)

	// The refusal of the preflight is the error the caller gets.
	want := preflightDataPath("local dir", opts.LocalDir)
	if want == nil {
		t.Fatal("the local directory is usable by the account of the instance, the test cannot fail the installation")
	}

	service := &scmManager{scope: ScopeSystem}
	err := service.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the local directory")
	}
	if err.Error() != want.Error() {
		t.Errorf("Install() = %q, want %q", err, want)
	}
}

// TestSCMSuccessfulInstallKeepsItsDirectories checks that a successful
// installation keeps the directories it created: the cleanup of a failed
// installation never touches the directories of an instance that is
// installed.
//
// The test registers the instance in the service control manager of the
// machine, so it needs an elevated shell. The registration never survives
// the test.
func TestSCMSuccessfulInstallKeepsItsDirectories(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	// A registration that exists already belongs to the machine and not
	// to this test: it is never touched here.
	state, err := scmRegistrationStateOf()
	if err != nil {
		t.Skipf("the service control manager is not available: %v", err)
	}
	if state != scmRegistrationAbsent {
		t.Skipf("%s is registered, the test would touch a real installation", ServiceName)
	}

	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)
	// The local directory has to be usable by the account of the
	// instance: the preflight refuses the installation otherwise.
	if err := os.MkdirAll(opts.LocalDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", opts.LocalDir, err)
	}
	scmTestSetDACL(t, opts.LocalDir, scmTestAllowInstanceIdentityDACL(t))

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

	service := &scmManager{scope: ScopeSystem}
	if err := service.Install(context.Background(), opts); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}

	for _, dir := range []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		filepath.Join(root, "var", "lib", "hfdesk"),
		filepath.Join(root, "var", "lib", "hfdesk", "cache"),
		filepath.Join(root, "var", "lib", "hfdesk", "cache", "hub"),
		filepath.Join(root, "var", "log"),
		filepath.Join(root, "var", "log", "hfdesk"),
		filepath.Join(root, "models"),
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
