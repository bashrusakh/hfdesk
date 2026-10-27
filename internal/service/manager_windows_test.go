// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"encoding/binary"
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

	"github.com/bashrusakh/hfdesk/internal/pe"
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

// scmTestServiceControl is a registration of the service control manager
// that a test drives.
//
// The registration refuses the stop control while the instance is still
// starting, the way the platform does: the control of an instance that did
// not report the running state yet is refused with
// ERROR_SERVICE_CANNOT_ACCEPT_CTRL.
type scmTestServiceControl struct {
	// state is the state the registration reports.
	state svc.State
	// refusals is the number of times the registration refuses the stop
	// control.
	refusals int
	// controls counts the delivered stop controls.
	controls int
}

// Query returns the state of the registration.
func (c *scmTestServiceControl) Query() (svc.Status, error) {
	return svc.Status{State: c.state}, nil
}

// Control sends the stop control to the registration.
//
// The control is refused while the registration has refusals left: the
// instance is still starting then. The control that is accepted stops the
// instance.
func (c *scmTestServiceControl) Control(cmd svc.Cmd) (svc.Status, error) {
	c.controls++
	if c.refusals > 0 {
		c.refusals--
		return svc.Status{State: c.state}, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL
	}
	c.state = svc.Stopped
	return svc.Status{State: svc.Stopped}, nil
}

// TestSCMStopRetriesTheControlOfAnInstanceThatIsStillStarting checks that a
// stop the supervisor refuses because the instance is still starting is
// delivered again, and not given up on.
//
// The service control manager refuses the control of an instance that did not
// report the running state yet (ERROR_SERVICE_CANNOT_ACCEPT_CTRL). The
// refusal is not the end of the stop: the control is delivered again inside
// the wait, until the instance stops or the budget of the wait is over.
func TestSCMStopRetriesTheControlOfAnInstanceThatIsStillStarting(t *testing.T) {
	handle := &scmTestServiceControl{state: svc.StartPending, refusals: 2}

	manager := &scmManager{scope: ScopeSystem}
	if err := manager.stopService(context.Background(), handle); err != nil {
		t.Fatalf("stopService() = %v, want nil: the refusal of a starting instance is not the end of the stop", err)
	}
	if handle.controls < 3 {
		t.Errorf("the stop control was delivered %d times, want it delivered again after every refusal", handle.controls)
	}
	if handle.state != svc.Stopped {
		t.Errorf("the state of the instance = %v, want %v", handle.state, svc.Stopped)
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
	if _, err := service.Install(ctx, opts); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}
	// The registration carries the failure recovery of the instance.
	scmTestCheckRecoveryActions(t)

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
	if _, err := service.Install(ctx, opts); err != nil {
		t.Fatalf("the second Install() = %v, want nil", err)
	}
	// The failure recovery is configured on the update path as well: an
	// updated registration whose recovery was lost would not be restarted
	// after a crash either.
	scmTestCheckRecoveryActions(t)
	status, err = service.Status(ctx)
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if status.Executable != opts.Executable {
		t.Errorf("the registered executable = %q, want %q", status.Executable, opts.Executable)
	}

	// An installation that names one setting keeps the others: only the
	// named bind address moves, the runtime paths of the instance stay what
	// the previous installation wrote. The request names no path at all, so
	// the paths of the registration are what the installation uses.
	named := InstallOptions{
		Scope:      ScopeSystem,
		Executable: opts.Executable,
		Explicit:   InstallFieldSet{Listen: true},
		Listen:     "127.0.0.1:18080",
	}
	effective, err := service.Install(ctx, named)
	if err != nil {
		t.Fatalf("the Install() of a named listen address = %v, want nil", err)
	}
	if effective.Listen != named.Listen {
		t.Errorf("the effective listen address = %q, want the named one %q", effective.Listen, named.Listen)
	}
	if effective.Paths != opts.Paths {
		t.Errorf("the effective paths = %+v, want the kept ones %+v", effective.Paths, opts.Paths)
	}
	status, err = service.Status(ctx)
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if status.Listen != named.Listen {
		t.Errorf("the registered listen address = %q, want the named one %q", status.Listen, named.Listen)
	}
	if status.Paths != opts.Paths {
		t.Errorf("the registered paths = %+v, want the kept ones %+v", status.Paths, opts.Paths)
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

// scmTestCheckRecoveryActions checks that the registration of the instance
// carries the failure recovery of the platform.
//
// The check reads the recovery from the service control manager, so it proves
// what the registration really holds and not what the installation meant to
// write. The switch for the non-crash failures is part of the policy as well:
// the handler of the instance reports a failed start, a failed host and a
// failed stop with an error code, and only the switch makes the platform
// recover those failures.
func scmTestCheckRecoveryActions(t *testing.T) {
	t.Helper()
	manager, err := mgr.Connect()
	if err != nil {
		t.Fatalf("mgr.Connect() = %v, want nil", err)
	}
	defer manager.Disconnect()
	handle, err := manager.OpenService(ServiceName)
	if err != nil {
		t.Fatalf("OpenService(%q) = %v, want nil", ServiceName, err)
	}
	defer handle.Close()

	actions, err := handle.RecoveryActions()
	if err != nil {
		t.Fatalf("RecoveryActions() = %v, want nil", err)
	}
	if !reflect.DeepEqual(actions, scmRecoveryActions()) {
		t.Errorf("the recovery actions of the registration = %v, want %v", actions, scmRecoveryActions())
	}
	period, err := handle.ResetPeriod()
	if err != nil {
		t.Fatalf("ResetPeriod() = %v, want nil", err)
	}
	if period != scmRecoveryResetPeriod {
		t.Errorf("the reset period of the registration = %d seconds, want %d", period, scmRecoveryResetPeriod)
	}
	nonCrash, err := handle.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		t.Fatalf("RecoveryActionsOnNonCrashFailures() = %v, want nil", err)
	}
	if !nonCrash {
		t.Error("the registration recovers a crash only, want the recovery of a non-crash failure as well: an instance that reports its failure has to be restarted")
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
// The set of the trustees that cover the account is the documented
// membership: the account itself, everybody and the authenticated users.
// The membership of the account is checked by
// TestSDDLGrantsIdentityWithTheMembershipOfTheAccount.
func TestSDDLGrantsIdentity(t *testing.T) {
	identity := scmDocumentedIdentitySIDs()
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
		{"the authenticated users group", "D:P(A;;FA;;;S-1-5-11)", false},
		{"the alias of the authenticated users group", "D:P(A;;FA;;;AU)", false},
		{"an unrelated account", "D:P(A;;FA;;;S-1-5-18)", true},
		{"an unrelated group", "D:P(A;;FA;;;S-1-5-32-544)", true},
		{"a denied entry next to an allowed one", "D:P(D;;GA;;;S-1-5-19)(A;;FA;;;S-1-1-0)", true},
		{"a denied entry for everybody", "D:P(D;;GA;;;S-1-1-0)(A;;FA;;;S-1-5-19)", true},
		{"a denied entry that denies the write access", "D:P(D;;FW;;;S-1-5-19)(A;;FR;;;S-1-1-0)", true},
		{"a denied entry that denies the read access", "D:P(D;;FR;;;S-1-5-19)(A;;FW;;;S-1-1-0)", true},
		{"a denied entry that denies the change of the access control list", "D:P(D;;WD;;;S-1-5-19)(A;;FA;;;S-1-1-0)", false},
		{"a denied entry that denies the change of the owner", "D:P(D;;WO;;;S-1-5-19)(A;;FA;;;S-1-1-0)", false},
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

// TestSDDLGrantsIdentityIgnoresADenialOfARightTheInstanceDoesNotNeed checks
// that a denied entry refuses the path only when it denies a right the
// instance needs.
//
// The instance reads the directory, it creates entries in it and it reads and
// writes the files in it. The right to change the access control list of the
// directory, the right to change its owner and the rights of the
// configuration of the platform are none of them, so a denied entry that
// carries one of them denies the instance nothing it needs.
func TestSDDLGrantsIdentityIgnoresADenialOfARightTheInstanceDoesNotNeed(t *testing.T) {
	identity := scmDocumentedIdentitySIDs()

	// A denied entry that denies the change of the access control list of
	// the directory: the directory is still usable by the instance.
	if err := sddlGrantsIdentity("D:P(D;;WD;;;S-1-5-19)(A;;FA;;;S-1-1-0)", identity); err != nil {
		t.Errorf("sddlGrantsIdentity() = %v, want nil: a denial of the change of the access control list takes nothing the instance needs away", err)
	}

	// The same for the change of the owner of the directory.
	if err := sddlGrantsIdentity("D:P(D;;WO;;;S-1-5-19)(A;;FA;;;S-1-1-0)", identity); err != nil {
		t.Errorf("sddlGrantsIdentity() = %v, want nil: a denial of the change of the owner takes nothing the instance needs away", err)
	}

	// A denial of the write access is a denial the instance needs: it has
	// to create entries in the directory.
	err := sddlGrantsIdentity("D:P(D;;FW;;;S-1-5-19)(A;;FA;;;S-1-1-0)", identity)
	if err == nil {
		t.Fatal("sddlGrantsIdentity() = nil, want the refusal of a denied write access")
	}
	if !strings.Contains(err.Error(), "write access") {
		t.Errorf("sddlGrantsIdentity() = %v, want an error that names the denied right", err)
	}

	// A denial of the read access is a denial the instance needs: it has
	// to read the directory and the files in it.
	err = sddlGrantsIdentity("D:P(D;;FR;;;S-1-5-19)(A;;FA;;;S-1-1-0)", identity)
	if err == nil {
		t.Fatal("sddlGrantsIdentity() = nil, want the refusal of a denied read access")
	}
	if !strings.Contains(err.Error(), "read access") {
		t.Errorf("sddlGrantsIdentity() = %v, want an error that names the denied right", err)
	}
}

// TestSDDLGrantsIdentityRefusesADenialOfAnIndividualFileRight checks that a
// denied entry that carries a single file right the instance needs is
// refused.
//
// The instance creates the files of the cache in the directory it works
// with, so the right to create a file in it (FILE_ADD_FILE) is a right the
// instance needs. A descriptor that denies that right to the account of the
// instance and grants the full control to everybody else denies the write
// access the instance needs, and the installation has to refuse it.
//
// The denial is measured against the individual rights of the entry: the
// aggregate masks alone carry nothing here, and the denial would be ignored.
func TestSDDLGrantsIdentityRefusesADenialOfAnIndividualFileRight(t *testing.T) {
	identity := scmDocumentedIdentitySIDs()

	err := sddlGrantsIdentity("D:P(D;;0x00000002;;;LS)(A;;FA;;;WD)", identity)
	if err == nil {
		t.Fatal("sddlGrantsIdentity() = nil, want the refusal of a denied FILE_ADD_FILE")
	}
	if !strings.Contains(err.Error(), "write access") {
		t.Errorf("sddlGrantsIdentity() = %v, want an error that names the denied right", err)
	}
}

// TestSDDLGrantsIdentityGrantsAnIndividualFileRight checks that an allowed
// entry that carries a single file right grants that right and not the
// other one.
//
// The right to create a file in a directory (FILE_ADD_FILE) is the write
// access of the check, and it is not the read access: a directory that
// grants it alone is a directory the account of the instance can write in
// and cannot read.
func TestSDDLGrantsIdentityGrantsAnIndividualFileRight(t *testing.T) {
	identity := scmDocumentedIdentitySIDs()

	// The mapping itself: the right to create a file is the write
	// access and not the read access.
	if got := scmMaskRights(windows.FILE_WRITE_DATA); got != scmRightWrite {
		t.Errorf("scmMaskRights(FILE_ADD_FILE) = %#08x, want the write access and not the read access", got)
	}

	// The entry alone grants the write access and not the read access,
	// so the directory is refused for the missing read access.
	err := sddlGrantsIdentity("D:P(A;;0x00000002;;;LS)", identity)
	if err == nil {
		t.Fatal("sddlGrantsIdentity() = nil, want the refusal of a directory without the read access")
	}
	if !strings.Contains(err.Error(), "read access") {
		t.Errorf("sddlGrantsIdentity() = %v, want an error that names the missing read access", err)
	}
	if strings.Contains(err.Error(), "write access") {
		t.Errorf("sddlGrantsIdentity() = %v, want the write access to be granted", err)
	}

	// The same entry next to an entry that grants the read access: the
	// directory is usable by the account of the instance.
	if err := sddlGrantsIdentity("D:P(A;;0x00000002;;;LS)(A;;0x00000001;;;LS)", identity); err != nil {
		t.Errorf("sddlGrantsIdentity() = %v, want nil for a directory the account of the instance can read and write", err)
	}
}

// TestSCMMaskRightsMapsTheIndividualFileRights checks that the rights of an
// access control entry are read by their individual bits.
//
// The instance needs the rights of a directory: it reads the entries of it
// and the files in it, and it creates entries in it and writes the files in
// it. An entry that carries a single one of them carries that right, so an
// entry that denies a single one of them denies it.
//
// The rights the instance does not need are not rights of the check: the
// right to delete the object, the right to read its descriptor, the right to
// change its access control list, the right to change its owner, the right
// to synchronize with it and the right to execute it are none of them.
//
// The generic rights and the aggregate masks carry the same rights as the
// individual bits they stand for.
func TestSCMMaskRightsMapsTheIndividualFileRights(t *testing.T) {
	cases := []struct {
		name string
		mask uint32
		want uint32
	}{
		{"the right to list the directory", windows.FILE_LIST_DIRECTORY, scmRightRead},
		{"the right to read the extended attributes", windows.FILE_READ_EA, scmRightRead},
		{"the right to walk through the directory", windows.FILE_TRAVERSE, scmRightRead},
		{"the right to read the attributes", windows.FILE_READ_ATTRIBUTES, scmRightRead},
		{"the right to create a file", windows.FILE_WRITE_DATA, scmRightWrite},
		{"the right to create a directory", windows.FILE_APPEND_DATA, scmRightWrite},
		{"the right to write the extended attributes", windows.FILE_WRITE_EA, scmRightWrite},
		{"the right to delete a child", scmFileDeleteChild, scmRightWrite},
		{"the right to write the attributes", windows.FILE_WRITE_ATTRIBUTES, scmRightWrite},
		{"the right to delete the object", windows.DELETE, 0},
		{"the right to read the descriptor", windows.READ_CONTROL, 0},
		{"the right to change the access control list", windows.WRITE_DAC, 0},
		{"the right to change the owner", windows.WRITE_OWNER, 0},
		{"the right to synchronize", windows.SYNCHRONIZE, 0},
		{"the right to execute", windows.GENERIC_EXECUTE, 0},
		{"the generic read", windows.GENERIC_READ, scmRightRead},
		{"the generic write", windows.GENERIC_WRITE, scmRightWrite},
		{"the generic all", windows.GENERIC_ALL, scmRightRead | scmRightWrite},
		{"the aggregate read", windows.FILE_GENERIC_READ, scmRightRead},
		{"the aggregate write", windows.FILE_GENERIC_WRITE, scmRightWrite},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmMaskRights(tc.mask); got != tc.want {
				t.Errorf("scmMaskRights(%#08x) = %#08x, want %#08x", tc.mask, got, tc.want)
			}
		})
	}
}

// TestSDDLGrantsIdentityWithTheMembershipOfTheAccount checks the access
// control list of a path against the membership of the account the instance
// runs as.
//
// The membership is the documented one: the account itself, everybody and
// the authenticated users. A group the membership carries covers the account
// and a group it does not carry does not.
//
// The test never skips: the membership of the account is the documented one
// on every machine.
func TestSDDLGrantsIdentityWithTheMembershipOfTheAccount(t *testing.T) {
	identity := scmServiceIdentity()

	// The membership the check is made against: it carries the account
	// itself and everybody, and it never carries the interactive users,
	// because the instance runs a service logon.
	if !identity.covers(scmServiceIdentitySID) {
		t.Fatalf("the membership %v does not carry the account %s", identity.sids, scmServiceIdentitySID)
	}
	if !identity.covers(scmEveryoneSID) {
		t.Fatalf("the membership %v does not carry everybody", identity.sids)
	}
	if identity.covers(scmInteractiveUsersSID) {
		t.Fatalf("the membership %v carries the interactive users, want a membership without them", identity.sids)
	}

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

// TestSDDLInheritOnlyDoesNotGrantTheIdentityAccess checks that an access
// control entry that applies to the children of the object grants the
// account of the instance nothing on the object itself.
//
// An inherit only entry ("IO") is about the children of the directory: the
// directory itself is not covered by it, so a directory whose only entry for
// the account of the instance is an inherit only one is a directory the
// instance can never use.
func TestSDDLInheritOnlyDoesNotGrantTheIdentityAccess(t *testing.T) {
	identity := scmDocumentedIdentitySIDs()

	// The entry is about the children of the directory: it grants the
	// account nothing on the directory itself.
	err := sddlGrantsIdentity("D:P(A;OICIIO;FA;;;LS)", identity)
	if err == nil {
		t.Fatal("sddlGrantsIdentity() = nil, want the refusal of an inherit only entry")
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("sddlGrantsIdentity() = %v, want an error that names the account of the instance", err)
	}

	// The same entry without the inherit only flag applies to the
	// directory itself and grants the account the access.
	if err := sddlGrantsIdentity("D:P(A;OICI;FA;;;LS)", identity); err != nil {
		t.Errorf("sddlGrantsIdentity() = %v, want nil for an entry that applies to the directory itself", err)
	}

	// An inherit only entry does not deny the account either: the
	// directory is refused because nothing grants it, and not because
	// something denies it.
	err = sddlGrantsIdentity("D:P(D;OICIIO;GA;;;LS)", identity)
	if err == nil {
		t.Fatal("sddlGrantsIdentity() = nil, want the refusal of a directory that grants the account nothing")
	}
	if !strings.Contains(err.Error(), "no access") {
		t.Errorf("sddlGrantsIdentity() = %v, want the refusal of the missing access and not a denial", err)
	}
	if strings.Contains(err.Error(), "denies") {
		t.Errorf("sddlGrantsIdentity() = %v, want no denial: an inherit only entry denies the account nothing", err)
	}
}

// TestSCMIdentitySIDsOfTheAccount checks the membership of the account the
// instance runs as.
//
// The membership is the documented one: the account itself is in it,
// everybody is in it, and the interactive users are never in it, because the
// instance runs a service logon.
//
// The test never skips: the membership of the account is the documented one
// on every machine.
func TestSCMIdentitySIDsOfTheAccount(t *testing.T) {
	identity := scmServiceIdentity()

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

// TestSddlGrantsIdentityNamesTheDocumentedMembership checks that a check
// that was made against the documented membership says so.
//
// The membership of the account is the documented one and not the one of a
// token of it, so the user has to know that the groups of the account were
// not read from a token of the account.
func TestSddlGrantsIdentityNamesTheDocumentedMembership(t *testing.T) {
	identity := scmDocumentedIdentitySIDs()

	err := sddlGrantsIdentity("D:P(A;;FA;;;S-1-5-32-544)", identity)
	if err == nil {
		t.Fatal("sddlGrantsIdentity() = nil, want the refusal of a group outside the documented membership")
	}
	if !strings.Contains(err.Error(), "documentation of the platform") {
		t.Errorf("sddlGrantsIdentity() = %v, want an error that names the documented membership", err)
	}
	if !strings.Contains(err.Error(), "not against the groups of a token") {
		t.Errorf("sddlGrantsIdentity() = %v, want an error that says the check was not made against a token of the account", err)
	}
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
	t.Fatalf("the membership %v carries every candidate group", identity.sids)
	return ""
}

// scmTestNamesPath reports whether a message of the manager names a path.
//
// The messages of the manager quote the path they talk about, so the
// backslashes of a Windows path are escaped in them: the path of a message
// is the quoted form of the path, not the path itself. The check looks for
// both forms, so it does not depend on the way a message renders the path.
//
// The manager judges a path by its real location: the platform follows the
// links of a path before it uses it, and it reports the name a directory has
// on the disk, so the name of a directory that is reached through a short
// name is the long name of the same directory. A message can therefore name
// the real location of a path instead of the path as it was written, and the
// check looks for that form as well.
//
// The real location of a path that does not exist cannot be read: the path as
// it was written is then the only form a message can name.
func scmTestNamesPath(message, path string) bool {
	if strings.Contains(message, path) || strings.Contains(message, strconv.Quote(path)) {
		return true
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return strings.Contains(message, resolved) || strings.Contains(message, strconv.Quote(resolved))
}

// TestSCMTestNamesPathAcceptsTheRealLocationOfAPath checks that the
// comparison of a path with a message accepts the real location of the path.
//
// The manager judges a path by its real location, and it names that location
// in its messages: the platform reports the name a directory has on the disk,
// so a directory that is reached through a short name is named by its long
// name. A comparison that accepts only the path as it was written fails on a
// machine that names the directory differently.
//
// The comparison keeps its meaning: the path as it was written is still
// accepted, and a path the message does not name is not a path the message
// names.
func TestSCMTestNamesPathAcceptsTheRealLocationOfAPath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "cache"), 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

	// The path as it was written names the directory with another case: the
	// platform reports the name the directory has on the disk.
	spelled := filepath.Join(root, "CACHE")
	resolved, err := filepath.EvalSymlinks(spelled)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q) = %v, want nil", spelled, err)
	}
	if resolved == spelled {
		t.Skipf("the platform reports the path %q as it was written", spelled)
	}

	// The message names the real location of the path.
	message := fmt.Sprintf("service: the cache dir %q does not exist", resolved)
	if !scmTestNamesPath(message, spelled) {
		t.Errorf("scmTestNamesPath(%q, %q) = false, want true: the message names the real location %q of the path", message, spelled, resolved)
	}
	// The path as it was written is still accepted.
	if !scmTestNamesPath(fmt.Sprintf("service: the cache dir %q does not exist", spelled), spelled) {
		t.Errorf("scmTestNamesPath() = false, want true: the message names the path %q", spelled)
	}
	// A path the message does not name is not a path the message names.
	other := filepath.Join(root, "other")
	if scmTestNamesPath(message, other) {
		t.Errorf("scmTestNamesPath(%q, %q) = true, want false: the message does not name the path", message, other)
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
	if !scmTestNamesPath(err.Error(), path) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the path", path, err)
	}
}

// scmTestNamedDir is one directory an installation can name.
type scmTestNamedDir struct {
	// name names the directory in the name of the subtest.
	name string
	// path returns the named directory of the installation.
	path func(opts *InstallOptions, root string) string
}

// scmTestNamedDirs returns the directories an installation can name: the
// cache root, the hub directory and the local output directory.
func scmTestNamedDirs() []scmTestNamedDir {
	return []scmTestNamedDir{
		{"the cache dir", func(opts *InstallOptions, root string) string {
			opts.CacheDir = filepath.Join(root, "cache")
			return opts.CacheDir
		}},
		{"the hub dir", func(opts *InstallOptions, root string) string {
			opts.HubDir = filepath.Join(root, "hub")
			return opts.HubDir
		}},
		{"the local dir", func(opts *InstallOptions, root string) string {
			opts.LocalDir = filepath.Join(root, "models")
			return opts.LocalDir
		}},
	}
}

// scmTestOptionsWithNamedDir returns the options of an installation that
// names one directory only, and the directory it names.
//
// The other directories are not part of the installation: the preflight
// checks the named directories only.
func scmTestOptionsWithNamedDir(root string, named scmTestNamedDir) (InstallOptions, string) {
	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	opts.CacheDir = ""
	opts.HubDir = ""
	opts.LocalDir = ""
	return opts, named.path(&opts, root)
}

// TestPreflightDataPathsAcceptAMissingNamedDirectory checks that a named
// directory that does not exist yet is accepted when the account of the
// instance can create it in its nearest existing ancestor.
//
// The instance creates the directory when it runs, as the account the
// instance runs as, so the installation judges the directory through the
// ancestor it would be created in: the account has to be able to create
// entries there.
//
// The installation creates nothing: the directory belongs to whoever named
// it, and the instance creates it when it runs.
func TestPreflightDataPathsAcceptAMissingNamedDirectory(t *testing.T) {
	for _, named := range scmTestNamedDirs() {
		t.Run(named.name, func(t *testing.T) {
			root := t.TempDir()
			// The nearest existing ancestor of the directory grants the
			// account of the instance the access it needs to create the
			// directory there.
			scmTestSetDACL(t, root, scmTestAllowInstanceIdentityDACL(t))

			opts, path := scmTestOptionsWithNamedDir(root, named)

			if err := preflightDataPaths(opts); err != nil {
				t.Errorf("preflightDataPaths() = %v, want nil for a directory the account of the instance can create", err)
			}
			// The installation created nothing: the instance creates the
			// directory when it runs.
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the directory %q exists (stat error: %v), want none: the instance creates it when it runs", path, err)
			}
		})
	}
}

// TestPreflightDataPathsRefuseAMissingNamedDirectory checks that a named
// directory that does not exist yet is refused when the account of the
// instance cannot create it in its nearest existing ancestor.
//
// The instance creates the directory when it runs, as the account the
// instance runs as, so the account has to be able to create entries in the
// ancestor. A named data directory belongs to whoever named it: the
// installation only reads the access control list of the ancestor and never
// creates the directory.
//
// The refusal names the directory, the account and the access that is
// missing, so the user knows what to grant.
func TestPreflightDataPathsRefuseAMissingNamedDirectory(t *testing.T) {
	for _, named := range scmTestNamedDirs() {
		t.Run(named.name, func(t *testing.T) {
			root := t.TempDir()
			// The nearest existing ancestor of the directory does not
			// grant the account of the instance the access it needs to
			// create the directory there.
			scmTestSetDACL(t, root, scmTestDenyInstanceIdentityDACL(t))

			opts, path := scmTestOptionsWithNamedDir(root, named)

			err := preflightDataPaths(opts)
			if err == nil {
				t.Fatalf("preflightDataPaths() = nil, want the refusal of %s", named.name)
			}
			if !scmTestNamesPath(err.Error(), path) {
				t.Errorf("preflightDataPaths() = %v, want an error that names the path %q", err, path)
			}
			if !strings.Contains(err.Error(), scmServiceIdentityName) {
				t.Errorf("preflightDataPaths() = %v, want an error that names the account of the instance", err)
			}
			if !strings.Contains(err.Error(), "read and write access") {
				t.Errorf("preflightDataPaths() = %v, want an error that names the access that is missing", err)
			}
			if !strings.Contains(err.Error(), "never creates a directory the user named") {
				t.Errorf("preflightDataPaths() = %v, want an error that tells the user that the installation creates no directory of its own", err)
			}
			if !strings.Contains(err.Error(), "grant the account read and write access to the ancestor directory") {
				t.Errorf("preflightDataPaths() = %v, want an error that tells the user what to do", err)
			}
			// The refused directory was not created behind the back of
			// the user.
			if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("the refused directory %q exists: %v", path, statErr)
			}
		})
	}
}

// TestPreflightDataPathJudgesTheNearestExistingAncestor checks that a named
// directory that does not exist yet is judged through the nearest existing
// ancestor of it and not through the directory above it.
//
// The instance creates the directory when it runs, as the account the
// instance runs as: the account has to be able to create it in the
// directory that holds it, and the directory that holds it is the nearest
// existing ancestor.
func TestPreflightDataPathJudgesTheNearestExistingAncestor(t *testing.T) {
	root := t.TempDir()
	// The directory that holds the named directory does not exist yet
	// either: the nearest existing ancestor is the one below the root.
	ancestor := filepath.Join(root, "a")
	if err := os.Mkdir(ancestor, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	scmTestSetDACL(t, ancestor, scmTestAllowInstanceIdentityDACL(t))
	// The root of the test does not grant the account of the instance the
	// access: the check has to judge the ancestor and not the root.
	scmTestSetDACL(t, root, scmTestDenyInstanceIdentityDACL(t))

	path := filepath.Join(ancestor, "b", "cache")
	if err := preflightDataPath("cache dir", path); err != nil {
		t.Errorf("preflightDataPath(%q) = %v, want nil for a directory the account of the instance can create in its nearest existing ancestor", path, err)
	}
	// The installation created nothing.
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the directory %q exists (stat error: %v), want none: the instance creates it when it runs", path, err)
	}
}

// TestSCMPreflightDataPathsAcceptADirectoryInsideTheStateRoot checks that a
// named directory that does not exist yet and that really lives inside the
// state root of the instance is not checked against the account of the
// instance.
//
// The installation grants the state root to that account, and the instance
// creates the cache tree it needs inside it. A directory of the state root
// that does not exist yet is therefore neither checked nor created.
//
// A directory that exists already is checked, whatever it lives in: the
// access control list of the child decides what the account of the instance
// can do there, and owning the state root grants no right on it.
func TestSCMPreflightDataPathsAcceptADirectoryInsideTheStateRoot(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "var", "lib", "hfdesk")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	// The state root belongs to the account of the instance.
	scmTestSetDACL(t, stateDir, scmTestAllowInstanceIdentityDACL(t))

	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   stateDir,
		LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
	}
	opts.CacheDir = filepath.Join(stateDir, "cache")
	opts.HubDir = filepath.Join(stateDir, "cache", "hub")
	opts.LocalDir = ""

	if err := preflightDataPaths(opts); err != nil {
		t.Errorf("preflightDataPaths() = %v, want nil for a directory inside the state root", err)
	}
	// The directory was not created: the instance creates it when it runs.
	if _, err := os.Stat(opts.CacheDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the cache directory %q exists (stat error: %v), want none: the instance creates it when it runs", opts.CacheDir, err)
	}
}

// TestSCMPreflightDataPathsRefuseAnExistingDirectoryInsideTheStateRoot
// checks that a named directory that exists inside the state root of the
// instance is checked against the account of the instance.
//
// Owning a directory grants no right on a directory inside it: the access
// control list of the child decides what the account of the instance can do
// there. A cache that was moved by hand, a cache of an earlier installation
// and a directory that was named with --cache-dir and exists already
// therefore have to be checked, even when they live inside the state root
// the installation owns.
func TestSCMPreflightDataPathsRefuseAnExistingDirectoryInsideTheStateRoot(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "var", "lib", "hfdesk")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	// The state root belongs to the account of the instance.
	scmTestSetDACL(t, stateDir, scmTestAllowInstanceIdentityDACL(t))

	// The cache exists already and the account of the instance cannot
	// work with it.
	cacheDir := filepath.Join(stateDir, "cache")
	if err := os.Mkdir(cacheDir, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	scmTestSetDACL(t, cacheDir, scmTestDenyInstanceIdentityDACL(t))

	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   stateDir,
		LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
	}
	opts.CacheDir = cacheDir
	opts.HubDir = ""
	opts.LocalDir = ""

	err := preflightDataPaths(opts)
	if err == nil {
		t.Fatalf("preflightDataPaths() = nil, want the refusal of an existing directory inside the state root")
	}
	if !scmTestNamesPath(err.Error(), cacheDir) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the path", err)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the account of the instance", err)
	}
}

// TestSCMPreflightDataPathsCheckAnExistingDirectoryOutsideTheStateRoot
// checks that a named directory that exists outside the state root of the
// instance is checked against the account of the instance, the way it has
// always been.
func TestSCMPreflightDataPathsCheckAnExistingDirectoryOutsideTheStateRoot(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "var", "lib", "hfdesk")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	scmTestSetDACL(t, stateDir, scmTestAllowInstanceIdentityDACL(t))

	// A directory outside of the state root the account of the instance
	// can work with.
	usable := filepath.Join(root, "models")
	if err := os.Mkdir(usable, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	scmTestSetDACL(t, usable, scmTestAllowInstanceIdentityDACL(t))

	// A directory outside of the state root the account of the instance
	// cannot work with.
	unusable := filepath.Join(root, "other-models")
	if err := os.Mkdir(unusable, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	scmTestSetDACL(t, unusable, scmTestDenyInstanceIdentityDACL(t))

	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   stateDir,
		LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
	}
	opts.HubDir = ""
	opts.LocalDir = ""

	// The directory the account of the instance can work with is
	// accepted.
	opts.CacheDir = usable
	if err := preflightDataPaths(opts); err != nil {
		t.Errorf("preflightDataPaths() = %v, want nil for a directory the account of the instance can work with", err)
	}

	// The directory it cannot work with is refused.
	opts.CacheDir = unusable
	err := preflightDataPaths(opts)
	if err == nil {
		t.Fatalf("preflightDataPaths() = nil, want the refusal of a directory the account of the instance cannot work with")
	}
	if !scmTestNamesPath(err.Error(), unusable) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the path", err)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the account of the instance", err)
	}
}

// TestSCMPreflightDataPathsRefuseALinkThatLeavesTheStateRoot checks that a
// named directory that is reached through a link inside the state root is
// checked like every other named directory.
//
// The exemption of the state root is decided on the real location of the two
// paths: a link that leaves the state root points at a directory outside of
// it, and the account of the instance has to be able to work with it. A
// comparison of the paths as they were written would grant the exemption to
// the link, and the installation would be accepted and fail later with an
// access error.
func TestSCMPreflightDataPathsRefuseALinkThatLeavesTheStateRoot(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "var", "lib", "hfdesk")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	scmTestSetDACL(t, stateDir, scmTestAllowInstanceIdentityDACL(t))

	// A directory outside of the state root that the account of the
	// instance cannot use.
	outside := filepath.Join(root, "models")
	if err := os.Mkdir(outside, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	scmTestSetDACL(t, outside, scmTestDenyInstanceIdentityDACL(t))

	// A link inside the state root that points at it.
	link := filepath.Join(stateDir, "cache")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("the platform does not allow a symbolic link here: %v", err)
	}

	// The link is not inside the state root: the directory it points at is
	// somewhere else.
	inside, err := scmPathInsideStateRoot(stateDir, link)
	if err != nil {
		t.Fatalf("scmPathInsideStateRoot() = %v, want nil", err)
	}
	if inside {
		t.Errorf("scmPathInsideStateRoot(%q, %q) = true, want false: the link points outside of the state root", stateDir, link)
	}

	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   stateDir,
		LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
	}
	opts.CacheDir = link
	opts.HubDir = ""
	opts.LocalDir = ""

	err = preflightDataPaths(opts)
	if err == nil {
		t.Fatalf("preflightDataPaths() = nil, want the refusal of a link that leaves the state root")
	}
	if !scmTestNamesPath(err.Error(), link) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the path", err)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the account of the instance", err)
	}
}

// TestPreflightDataPathRefusesAPathTheIdentityCannotReach checks that a
// directory the account of the instance cannot reach is refused before the
// registration is written.
//
// The directory does not exist: the instance creates it when it runs, as the
// account the instance runs as, so the refusal names the missing directory,
// the nearest existing ancestor it would be created in and the access the
// account is missing there.
//
// The guidance names the account of the instance, because the access rights
// of the ancestor have to be granted to that account.
func TestPreflightDataPathRefusesAPathTheIdentityCannotReach(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	// The directory grants the machine and its administrators the access,
	// not the account the instance runs as: the account of the instance
	// could not create the directory there either.
	scmTestSetDACL(t, parent, fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)", scmTestCurrentUserSID(t)))

	path := filepath.Join(parent, "cache")
	err := preflightDataPath("cache dir", path)
	if err == nil {
		t.Fatalf("preflightDataPath(%q) = nil, want an error", path)
	}
	if !scmTestNamesPath(err.Error(), path) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the path", path, err)
	}
	// The manager names the real location of the ancestor: the platform
	// reports the name the directory has on the disk, so a directory that is
	// reached through a short name is named by its long name. The name of the
	// ancestor is what makes the guidance actionable, and the check accepts
	// the path as it was written and the real location of it.
	if !scmTestNamesPath(err.Error(), parent) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the nearest existing ancestor %q", path, err, parent)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the account of the instance", path, err)
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the missing directory", path, err)
	}
	if !strings.Contains(err.Error(), "read and write access") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the access that is missing", path, err)
	}
	if !strings.Contains(err.Error(), "never creates a directory the user named") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that tells the user that the installation creates no directory of its own", path, err)
	}
	if !strings.Contains(err.Error(), "grant the account read and write access to the ancestor directory") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that tells the user what to do", path, err)
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

// TestPreflightDataPathRefusesADirectoryThatGrantsOnlyTheChildren checks
// that a directory whose only entry for the account of the instance applies
// to its children is refused.
//
// An inherit only entry is about the children of the directory: the
// directory itself is not covered by it. A directory the account of the
// instance can only reach through its children is a directory the instance
// can never use, and the preflight has to refuse it.
func TestPreflightDataPathRefusesADirectoryThatGrantsOnlyTheChildren(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cache")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	// The directory grants the account of the instance the access to its
	// children only, and the account of the test the access to the
	// directory itself: the test has to keep the access to the directory
	// it restricted.
	scmTestSetDACL(t, path, fmt.Sprintf(
		"D:P(A;OICIIO;FA;;;%s)(A;;FA;;;%s)",
		scmServiceIdentitySID, scmTestCurrentUserSID(t)))

	err := preflightDataPath("cache dir", path)
	if err == nil {
		t.Fatalf("preflightDataPath(%q) = nil, want the refusal of a directory that grants the account the access to its children only", path)
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

// TestPreflightExecutableImageRefusesAFileThatIsNotAProgram proves that an
// installation refuses a file that is not a Windows program: a text file
// renamed to hfdesk.exe is not a program the service control manager could
// start.
func TestPreflightExecutableImageRefusesAFileThatIsNotAProgram(t *testing.T) {
	path := scmTestExecutable(t, []byte(strings.Repeat("this is not a Windows program\n", 8)))

	err := preflightExecutableImage(path)
	if err == nil {
		t.Fatal("preflightExecutableImage accepted a file that is not a Windows program")
	}
	if !scmTestNamesPath(err.Error(), path) {
		t.Errorf("preflightExecutableImage(%q) = %v, want an error that names the file", path, err)
	}
	if !strings.Contains(err.Error(), "not a Windows executable") {
		t.Errorf("preflightExecutableImage(%q) = %v, want an error that says the file is not a Windows executable", path, err)
	}
}

// TestPreflightExecutableImageRefusesATruncatedProgram proves that a program
// whose header was cut off is refused: a download that ended in the middle of
// the header is not a program the service control manager could start.
func TestPreflightExecutableImageRefusesATruncatedProgram(t *testing.T) {
	image := scmTestPEImage(pe.MachineAMD64)
	// The image ends before the PE header its DOS header points at.
	path := scmTestExecutable(t, image[:scmTestDosHeaderSize])

	err := preflightExecutableImage(path)
	if err == nil {
		t.Fatal("preflightExecutableImage accepted a truncated program")
	}
	if !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("preflightExecutableImage(%q) = %v, want the truncated header to be reported", path, err)
	}
}

// TestPreflightExecutableImageRefusesAProgramOfAnotherArchitecture proves
// that an installation refuses a program that was built for another
// architecture than the process that installs it, and that a process
// architecture without a comparison only requires a PE image.
func TestPreflightExecutableImageComparesTheMachineOfTheProcess(t *testing.T) {
	cases := []struct {
		name    string
		arch    string
		machine uint16
		wantErr bool
	}{
		{"AMD64 image for an AMD64 process", "amd64", pe.MachineAMD64, false},
		{"ARM64 image for an AMD64 process", "amd64", pe.MachineARM64, true},
		{"ARM64 image for an ARM64 process", "arm64", pe.MachineARM64, false},
		{"AMD64 image for an ARM64 process", "arm64", pe.MachineAMD64, true},
		// The installation has no comparison for this architecture of
		// the process: the file is still required to be a PE image.
		{"ARM64 image for a 386 process", "386", pe.MachineARM64, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := scmTestExecutable(t, scmTestPEImage(tc.machine))

			err := preflightExecutableImageFor(path, tc.arch)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("preflightExecutableImageFor(%q, %q) = %v, want nil", path, tc.arch, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("preflightExecutableImageFor(%q, %q) = nil, want the refusal of the architecture 0x%04X", path, tc.arch, tc.machine)
			}
			if !scmTestNamesPath(err.Error(), path) {
				t.Errorf("preflightExecutableImageFor(%q, %q) = %v, want an error that names the file", path, tc.arch, err)
			}
			if !strings.Contains(err.Error(), "another architecture") {
				t.Errorf("preflightExecutableImageFor(%q, %q) = %v, want an error that says the program is built for another architecture", path, tc.arch, err)
			}
		})
	}
}

// TestPreflightExecutableImageRequiresAPEImageForAnArchitectureWithoutAComparison
// proves that the architecture without a comparison is no way around the check
// of the file: a file that is not a program is refused as well.
func TestPreflightExecutableImageRequiresAPEImageForAnArchitectureWithoutAComparison(t *testing.T) {
	path := scmTestExecutable(t, []byte(strings.Repeat("this is not a Windows program\n", 8)))

	if err := preflightExecutableImageFor(path, "386"); err == nil {
		t.Fatal("preflightExecutableImageFor accepted a file that is not a Windows program")
	}
}

// TestSCMProcessMachineNamesTheArchitecturesOfTheImages proves that only the
// architectures HFDesk ships have a machine type the installation compares.
func TestSCMProcessMachineNamesTheArchitecturesOfTheImages(t *testing.T) {
	cases := map[string]struct {
		machine uint16
		known   bool
	}{
		"amd64": {pe.MachineAMD64, true},
		"arm64": {pe.MachineARM64, true},
		"386":   {0, false},
		"wasm":  {0, false},
		"":      {0, false},
	}
	for arch, want := range cases {
		machine, known := scmProcessMachine(arch)
		if known != want.known {
			t.Errorf("scmProcessMachine(%q) known = %v, want %v", arch, known, want.known)
		}
		if machine != want.machine {
			t.Errorf("scmProcessMachine(%q) machine = 0x%04X, want 0x%04X", arch, machine, want.machine)
		}
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

// The layout of the PE image the tests synthesize.
const (
	// scmTestDosHeaderSize is the size of the DOS header, the part of an
	// image the offset of the PE header is read from.
	scmTestDosHeaderSize = 64
	// scmTestPeHeaderOffset is the offset of the PE header in the
	// synthesized image.
	scmTestPeHeaderOffset = 0x80
	// scmTestPeHeaderSize is the size of the PE signature and of the COFF
	// header that follows it.
	scmTestPeHeaderSize = 24
)

// scmTestPEImage builds a PE image of the given machine type.
//
// Only the fields the installation reads are filled in: the DOS header, the PE
// signature and the machine type of the COFF header. The rest of the image is
// zero, which is enough for the machine type to be read.
func scmTestPEImage(machine uint16) []byte {
	image := make([]byte, scmTestPeHeaderOffset+scmTestPeHeaderSize)
	copy(image[0:], []byte("MZ"))
	binary.LittleEndian.PutUint32(image[0x3C:], scmTestPeHeaderOffset)
	copy(image[scmTestPeHeaderOffset:], []byte("PE\x00\x00"))
	binary.LittleEndian.PutUint16(image[scmTestPeHeaderOffset+4:], machine)
	return image
}

// scmTestExecutable writes data as hfdesk.exe into a temporary directory and
// returns its path.
func scmTestExecutable(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hfdesk.exe")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	return path
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
// The installation creates the directories of the instance only: a named data
// directory belongs to whoever named it, and the instance creates the cache
// tree it needs inside its own directories when it runs.
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
	if _, err := service.Install(context.Background(), opts); err == nil {
		t.Fatal("Install() = nil, want the refusal of the local directory")
	}

	// Every directory the installation created is gone again: the
	// directories of the instance are removed before the parents that
	// hold them. The cache root and the hub directory are not created by
	// the installation at all, so they are gone as well.
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
	if _, err := service.Install(context.Background(), opts); err == nil {
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
	_, err := service.Install(context.Background(), opts)
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
// The installation creates the directories of the instance only: the cache
// root and the hub directory live in the state directory of the instance and
// are created by the instance when it runs, and the local directory belongs
// to whoever named it.
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
	if _, err := service.Install(context.Background(), opts); err != nil {
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
		// The local directory belongs to whoever named it: the
		// installation reuses it and never removes it.
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

	// The cache root and the hub directory are not created by the
	// installation: the instance creates the cache tree it needs inside
	// its own state directory when it runs.
	for _, dir := range []string{opts.CacheDir, opts.HubDir} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the directory %q exists after the successful installation (stat error: %v), want none: the instance creates it when it runs", dir, err)
		}
	}
}

// TestSCMRegistrationOptionsRecoversTheRegistration checks that the options of
// an existing registration are read back from the command line the service
// control manager holds: the program, the bind address, the runtime paths and
// the cache layout.
//
// The recovery is what makes an installation over an existing registration keep
// the settings the instance really runs with.
func TestSCMRegistrationOptionsRecoversTheRegistration(t *testing.T) {
	cases := []struct {
		name string
		opts InstallOptions
	}{
		{"the full layout", scmTestOptions()},
		{
			name: "without a cache layout",
			opts: func() InstallOptions {
				opts := scmTestOptions()
				opts.CacheDir = ""
				opts.HubDir = ""
				opts.LocalDir = ""
				return opts
			}(),
		},
		{
			name: "with a path that carries a space",
			opts: func() InstallOptions {
				opts := scmTestOptions()
				opts.Executable = filepath.Join("C:\\Program Files", "HFDesk", "hfdesk.exe")
				opts.Paths.StateDir = filepath.Join("C:\\Program Files", "HFDesk", "state")
				return opts
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scmRegistrationOptions(scmCommandLine(tc.opts), ScopeSystem)
			if !reflect.DeepEqual(got, tc.opts) {
				t.Errorf("scmRegistrationOptions() =\n%+v\nwant\n%+v", got, tc.opts)
			}
			if got.Explicit != (InstallFieldSet{}) {
				t.Errorf("the recovered options mark fields explicit: %+v", got.Explicit)
			}
			// The account of a Windows instance is the built-in
			// LocalService account and never a setting of the
			// registration.
			if got.ServiceUser != "" {
				t.Errorf("the recovered account = %q, want none", got.ServiceUser)
			}
		})
	}
}

// TestSCMRegistrationOptionsOfADamagedRegistrationNameNothing checks that a
// registration that carries no readable command line records no setting.
//
// An installation is the way a damaged registration is repaired, so the
// recovery never fails: the options are empty, and the merge falls back to the
// defaults of the request.
func TestSCMRegistrationOptionsOfADamagedRegistrationNameNothing(t *testing.T) {
	for _, commandLine := range []string{"", "   "} {
		got := scmRegistrationOptions(commandLine, ScopeSystem)
		want := InstallOptions{Scope: ScopeSystem}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("scmRegistrationOptions(%q) =\n%+v\nwant\n%+v", commandLine, got, want)
		}
	}
}

// TestSCMReinstallKeepsTheRegistrationOfTheInstance checks the merge of a
// request with the registration of the instance: a setting the request did not
// name keeps the value the instance runs with, and only the program comes from
// the request.
func TestSCMReinstallKeepsTheRegistrationOfTheInstance(t *testing.T) {
	registered := scmTestOptions()
	registered.Executable = filepath.Join("C:\\Old\\HFDesk", "hfdesk.exe")
	registered.Listen = "127.0.0.1:9090"

	existing := scmRegistrationOptions(scmCommandLine(registered), ScopeSystem)

	// A request that names nothing but the new program: the defaults of the
	// scope apply only where the registration has no value of its own.
	requested := InstallOptions{
		Scope:      ScopeSystem,
		Executable: filepath.Join("C:\\New\\HFDesk", "hfdesk.exe"),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join("C:\\ProgramData", "HFDesk", "hfdesk.json"),
			StateDir:   filepath.Join("C:\\ProgramData", "HFDesk"),
			LogDir:     filepath.Join("C:\\ProgramData", "HFDesk", "logs"),
		},
		CacheDir: filepath.Join("C:\\ProgramData", "HFDesk", "cache"),
		HubDir:   filepath.Join("C:\\ProgramData", "HFDesk", "cache", "hub"),
	}

	merged := mergeInstallOptions(requested, existing)

	if merged.Executable != requested.Executable {
		t.Errorf("the executable = %q, want the one of the request %q", merged.Executable, requested.Executable)
	}
	if merged.Listen != registered.Listen {
		t.Errorf("the listen address = %q, want the registered one %q", merged.Listen, registered.Listen)
	}
	if merged.Paths != registered.Paths {
		t.Errorf("the paths = %+v, want the registered ones %+v", merged.Paths, registered.Paths)
	}
	if merged.CacheDir != registered.CacheDir || merged.HubDir != registered.HubDir || merged.LocalDir != registered.LocalDir {
		t.Errorf("the cache layout = %q/%q/%q, want the registered one %q/%q/%q",
			merged.CacheDir, merged.HubDir, merged.LocalDir, registered.CacheDir, registered.HubDir, registered.LocalDir)
	}
}

// TestSCMReinstallHonoursTheExplicitFields checks that a setting the request
// named itself wins over the registration of the instance.
func TestSCMReinstallHonoursTheExplicitFields(t *testing.T) {
	registered := scmTestOptions()
	existing := scmRegistrationOptions(scmCommandLine(registered), ScopeSystem)

	requested := InstallOptions{
		Scope:      ScopeSystem,
		Executable: filepath.Join("C:\\New\\HFDesk", "hfdesk.exe"),
		Explicit: InstallFieldSet{
			Listen:   true,
			Config:   true,
			StateDir: true,
			LogDir:   true,
			CacheDir: true,
			HubDir:   true,
			LocalDir: true,
		},
		Listen: "127.0.0.1:7070",
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join("C:\\Named", "hfdesk.json"),
			StateDir:   filepath.Join("C:\\Named", "state"),
			LogDir:     filepath.Join("C:\\Named", "log"),
		},
		CacheDir: filepath.Join("C:\\Named", "cache"),
		HubDir:   filepath.Join("C:\\Named", "cache", "hub"),
		LocalDir: filepath.Join("C:\\Named", "models"),
	}

	merged := mergeInstallOptions(requested, existing)
	if !reflect.DeepEqual(merged, requested) {
		t.Errorf("mergeInstallOptions() =\n%+v\nwant the request\n%+v", merged, requested)
	}
}

// TestSCMRecoveryActionsRestartTheFailedInstance checks the failure recovery
// of the registration: the platform restarts an instance that ended without
// reporting its stop, after five, thirty and sixty seconds, and it forgets the
// failures of the instance after one day.
//
// The platform never reboots the machine and never runs a command for a failed
// instance: only a restart of the instance is one of the actions.
func TestSCMRecoveryActionsRestartTheFailedInstance(t *testing.T) {
	actions := scmRecoveryActions()
	want := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}
	if !reflect.DeepEqual(actions, want) {
		t.Errorf("scmRecoveryActions() = %v, want %v", actions, want)
	}
	for _, action := range actions {
		switch action.Type {
		case mgr.ServiceRestart:
		case mgr.NoAction, mgr.ComputerReboot, mgr.RunCommand:
			t.Errorf("the recovery action %v is not a restart of the instance", action)
		}
		if action.Delay < 0 {
			t.Errorf("the recovery action %v has a negative delay", action)
		}
	}

	if scmRecoveryResetPeriod != 24*60*60 {
		t.Errorf("the reset period = %d seconds, want one day (86400)", scmRecoveryResetPeriod)
	}
}

// TestSCMRecoveryDecisionKeepsARegisteredPolicy checks what an installation does
// with the failure recovery of a registration: the policy of HFDesk is configured
// only when the registration has none, and a policy that is registered already is
// kept, because a repeat installation keeps the settings of the instance and an
// administrator may have set a policy of their own.
//
// A read that fails does not skip the required policy: a service without recovery
// is the state the installation exists to prevent, so the policy of HFDesk is
// configured then.
func TestSCMRecoveryDecisionKeepsARegisteredPolicy(t *testing.T) {
	administrator := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
	}
	readFailure := errors.New("the failure actions of the registration cannot be read")

	tests := []struct {
		name     string
		existing []mgr.RecoveryAction
		readErr  error
		want     scmRecoveryDecision
	}{
		{"a registration without actions", nil, nil, scmRecoveryConfigure},
		{"an empty list of actions", []mgr.RecoveryAction{}, nil, scmRecoveryConfigure},
		{"the policy of HFDesk is registered already", scmRecoveryActions(), nil, scmRecoveryKeep},
		{"a policy of an administrator", administrator, nil, scmRecoveryKeep},
		{"an explicitly registered policy of no action", []mgr.RecoveryAction{{Type: mgr.NoAction}}, nil, scmRecoveryKeep},
		{"a read that fails", nil, readFailure, scmRecoveryConfigure},
		{"a read that fails although actions were returned", administrator, readFailure, scmRecoveryConfigure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := scmRecoveryDecisionFor(test.existing, test.readErr); got != test.want {
				t.Errorf("scmRecoveryDecisionFor(%v, %v) = %v, want %v",
					test.existing, test.readErr, got, test.want)
			}
		})
	}
}

// TestSCMSameExecutablePath checks the comparison of two paths that name a
// program: the platform stores the path of a program in the case and the
// spelling it was given, so the comparison is case-insensitive and made on the
// cleaned path.
func TestSCMSameExecutablePath(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"the same path", `C:\ProgramData\HFDesk\hfdesk.exe`, `C:\ProgramData\HFDesk\hfdesk.exe`, true},
		{"another case", `C:\ProgramData\HFDesk\hfdesk.exe`, `c:\programdata\hfdesk\HFDESK.EXE`, true},
		{"a redundant part of the path", `C:\ProgramData\HFDesk\hfdesk.exe`, `C:\ProgramData\HFDesk\.\hfdesk.exe`, true},
		{"another program in the same directory", `C:\ProgramData\HFDesk\hfdesk.exe`, `C:\ProgramData\HFDesk\hfdesk-old.exe`, false},
		{"another directory", `C:\ProgramData\HFDesk\hfdesk.exe`, `C:\ProgramData\HFDesk2\hfdesk.exe`, false},
		{"a program below the directory", `C:\ProgramData\HFDesk\hfdesk.exe`, `C:\ProgramData\HFDesk\bin\hfdesk.exe`, false},
		{"an empty path against a program", "", `C:\ProgramData\HFDesk\hfdesk.exe`, false},
		{"a program against an empty path", `C:\ProgramData\HFDesk\hfdesk.exe`, "", false},
		{"two empty paths", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmSameExecutablePath(tc.a, tc.b); got != tc.want {
				t.Errorf("scmSameExecutablePath(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			if got := scmSameExecutablePath(tc.b, tc.a); got != tc.want {
				t.Errorf("scmSameExecutablePath(%q, %q) = %v, want %v", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

// TestSCMInstanceRan checks the classification of the state of a registration
// before an installation.
//
// Only an instance that runs, or that is still starting, is restarted by the
// installation. A stopped, broken, absent or stopping instance is left alone:
// an installation never starts a service that was not running, and it never
// brings back a service the user stopped.
func TestSCMInstanceRan(t *testing.T) {
	cases := []struct {
		name    string
		present bool
		state   svc.State
		want    bool
	}{
		{"running", true, svc.Running, true},
		{"starting", true, svc.StartPending, true},
		{"stopped", true, svc.Stopped, false},
		{"stopping", true, svc.StopPending, false},
		{"paused", true, svc.Paused, false},
		{"pause pending", true, svc.PausePending, false},
		{"continue pending", true, svc.ContinuePending, false},
		{"an unknown state", true, svc.State(4711), false},
		{"a running state without a registration", false, svc.Running, false},
		{"a stopped state without a registration", false, svc.Stopped, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmInstanceRan(tc.present, tc.state); got != tc.want {
				t.Errorf("scmInstanceRan(%v, %v) = %v, want %v", tc.present, tc.state, got, tc.want)
			}
		})
	}
}

// TestSCMSameRegistration checks the comparison of two command lines of a
// registration: the program is compared like every other path, and the
// arguments are compared as they are.
func TestSCMSameRegistration(t *testing.T) {
	opts := scmTestOptions()
	commandLine := scmCommandLine(opts)

	otherArgs := opts
	otherArgs.Listen = "127.0.0.1:9090"

	otherProgram := opts
	otherProgram.Executable = filepath.Join("C:\\Program Files", "HFDesk", "hfdesk.exe")

	// The platform stores the path of a program in the case it was given:
	// the same registration in another case is the same registration.
	otherCase := opts
	otherCase.Executable = strings.ToUpper(opts.Executable)

	cases := []struct {
		name string
		got  string
		want string
		same bool
	}{
		{"the same command line", commandLine, commandLine, true},
		{"the same registration in another case", scmCommandLine(otherCase), commandLine, true},
		{"another bind address", scmCommandLine(otherArgs), commandLine, false},
		{"another program", scmCommandLine(otherProgram), commandLine, false},
		{"a command line that cannot be read", "", commandLine, false},
		{"two command lines that cannot be read", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmSameRegistration(tc.got, tc.want); got != tc.same {
				t.Errorf("scmSameRegistration(%q, %q) = %v, want %v", tc.got, tc.want, got, tc.same)
			}
		})
	}
}

// TestSCMVerifyWaitIsTheShutdownBudget checks that the wait for a restarted
// instance to run the registered program covers the graceful shutdown of the
// instance and is the shared budget of the service layer.
//
// The wait is not a number of its own: a second copy of the same decision would
// drift below the budget the server publishes for its own shutdown.
func TestSCMVerifyWaitIsTheShutdownBudget(t *testing.T) {
	if scmVerifyTimeout < server.GracefulShutdownTimeout {
		t.Errorf("the verify wait of the manager = %s, want at least %s: the wait has to cover the graceful shutdown of the previous instance", scmVerifyTimeout, server.GracefulShutdownTimeout)
	}
	if scmVerifyTimeout != ShutdownBudget {
		t.Errorf("the verify wait of the manager = %s, want the shared budget %s", scmVerifyTimeout, ShutdownBudget)
	}
	if scmVerifyPollInterval <= 0 || scmVerifyPollInterval > scmVerifyTimeout {
		t.Errorf("the verify poll interval = %s, want a positive pause inside the wait %s", scmVerifyPollInterval, scmVerifyTimeout)
	}
}

// scmRollbackSuccessClaim is the phrase that reports the previous instance as in
// service again. It is the claim a restart rollback may only make for a verified
// restart, so the test of the outcome sentences looks for exactly this phrase.
const scmRollbackSuccessClaim = "the previous instance was started again"

// TestSCMRestartRollbackSentence checks that the outcome of a restart rollback is
// only described as far as the rollback proved it.
//
// The sentence is the whole report a caller reads next to the failure of the
// installation, so it is the place the truthfulness rule lives: the previous
// instance is only reported as started again for a verified restart, a
// registration that could not name its program is reported as such, and a
// registration that could not be put back is never reported as restored. The
// stop of the instance of the failed installation comes before the restore, and
// the sentence of a failed stop says so: the registration of the failed
// installation is kept, because its instance may still run.
func TestSCMRestartRollbackSentence(t *testing.T) {
	const stopReason = "the instance did not stop within 2m0s"
	const restoreReason = "the service control manager refused the write of the registration"
	const startReason = `the instance does not run "C:\\ProgramData\\HFDesk\\hfdesk.exe" within 2m0s`

	stopped := scmRestartRollback{stopped: true}
	restored := scmRestartRollback{stopped: true, restored: true}
	programKnown := scmRestartRollback{stopped: true, restored: true, programKnown: true}
	started := scmRestartRollback{stopped: true, restored: true, programKnown: true, started: true}

	cases := []struct {
		name string
		// facts are the observations of the rollback up to the step that did
		// not reach its goal.
		facts scmRestartRollback
		// reason is the detail the rollback embeds for a step that failed.
		reason string
		// want contains the phrases the sentence has to carry.
		want []string
		// verified reports whether the sentence may claim the previous
		// instance as started again.
		verified bool
	}{
		{
			name:     "the instance of the failed installation did not stop",
			facts:    scmRestartRollback{},
			reason:   stopReason,
			want:     []string{"was kept", "was not proven to be stopped", stopReason, "the previous registration was not restored"},
			verified: false,
		},
		{
			name:     "the previous registration could not be put back",
			facts:    stopped,
			reason:   restoreReason,
			want:     []string{"was stopped", "could not be put back", restoreReason},
			verified: false,
		},
		{
			name:     "the restored registration names no program",
			facts:    restored,
			reason:   "",
			want:     []string{"was stopped", "was restored", "names no program", "no instance was started", "is stopped"},
			verified: false,
		},
		{
			name:     "the previous instance was not proven to be in service again",
			facts:    programKnown,
			reason:   startReason,
			want:     []string{"was stopped", "was restored", "was not proven to be in service again", startReason},
			verified: false,
		},
		{
			name:     "the started instance was not proven to be the previous program",
			facts:    started,
			reason:   "",
			want:     []string{"was stopped", "was restored", "was not proven to be the previous program", "the image of the process could not be read"},
			verified: false,
		},
		{
			name:     "the previous instance was started again",
			facts:    scmRestartRollback{stopped: true, restored: true, programKnown: true, started: true, verified: true},
			reason:   "",
			want:     []string{"was stopped", "was restored", scmRollbackSuccessClaim, "the failure recovery of the registration was not part of the restore"},
			verified: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sentence := scmRestartRollbackSentence(tc.facts, tc.reason)
			if sentence == "" {
				t.Fatal("scmRestartRollbackSentence() is empty, want the outcome of the rollback")
			}
			for _, want := range tc.want {
				if !strings.Contains(sentence, want) {
					t.Errorf("the sentence %q does not carry %q", sentence, want)
				}
			}
			// The success claim is only made for a verified restart: every
			// other outcome reports what it proved instead.
			if claimed := strings.Contains(sentence, scmRollbackSuccessClaim); claimed != tc.verified {
				t.Errorf("the sentence %q claims the previous instance was started again = %v, want %v", sentence, claimed, tc.verified)
			}
			// A rollback that did not prove the stop never claims the
			// restore: the registration of the failed installation is the
			// one that describes the process that may still run.
			if !tc.facts.stopped && strings.Contains(sentence, "the configuration of the previous registration was restored") {
				t.Errorf("the sentence %q claims the previous configuration was restored while the instance of the failed installation was not proven to be stopped", sentence)
			}
		})
	}
}

// TestSCMConfigExecutableNamesTheProgramOfTheRegistration checks the decision
// that tells a restored registration with a program from one without.
//
// The restart rollback starts the previous program again only when the restored
// registration names it. A command line that cannot be read names no program, so
// the rollback reports the restored registration and the state of the instance
// instead of claiming a start it never made.
func TestSCMConfigExecutableNamesTheProgramOfTheRegistration(t *testing.T) {
	opts := scmTestOptions()
	readable := mgr.Config{BinaryPathName: scmCommandLine(opts)}

	if got := scmConfigExecutable(readable); got != opts.Executable {
		t.Errorf("scmConfigExecutable(a readable command line) = %q, want %q", got, opts.Executable)
	}

	// The same program in another case is still the program of the registration:
	// the comparison of the image of a process is made without regard to the
	// case of the path.
	otherCase := opts
	otherCase.Executable = strings.ToUpper(opts.Executable)
	got := scmConfigExecutable(mgr.Config{BinaryPathName: scmCommandLine(otherCase)})
	if got == "" {
		t.Fatal("scmConfigExecutable(a command line in another case) = \"\", want the program of the registration")
	}
	if !scmSameExecutablePath(got, opts.Executable) {
		t.Errorf("scmConfigExecutable(a command line in another case) = %q, want the same program as %q", got, opts.Executable)
	}

	// A registration whose command line is empty names no program: that is the
	// registration a failed installation can leave behind, and the rollback
	// reports it instead of starting an instance it cannot name.
	if got := scmConfigExecutable(mgr.Config{BinaryPathName: ""}); got != "" {
		t.Errorf("scmConfigExecutable(an empty command line) = %q, want no program", got)
	}
}

// TestSCMProcessExcluded checks the decision that keeps a process of the failed
// attempt from being taken for the running instance.
//
// The installation excludes the process it replaced; the restart rollback
// excludes the process of the failed attempt next to it. A process identifier of
// zero names no process, so it never excludes one: the caller passes the process
// the service control manager reported even when it reported none.
func TestSCMProcessExcluded(t *testing.T) {
	cases := []struct {
		name      string
		exclude   []uint32
		processID uint32
		want      bool
	}{
		{"no excluded process", nil, 4711, false},
		{"the process of the attempt", []uint32{4711}, 4711, true},
		{"another process", []uint32{4711}, 4712, false},
		{"the process of the failed attempt next to the replaced one", []uint32{4710, 4711}, 4711, true},
		{"a replaced process the manager reported as none", []uint32{0, 4711}, 4711, true},
		{"no process at all", []uint32{0}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmProcessExcluded(tc.exclude, tc.processID); got != tc.want {
				t.Errorf("scmProcessExcluded(%v, %d) = %v, want %v", tc.exclude, tc.processID, got, tc.want)
			}
		})
	}
}

// TestSCMStopOfAnInstanceThatIsStoppedIsACleanNoOp checks the first step of the
// restart rollback on an instance that does not run.
//
// The rollback stops the instance of the failed installation before it restores
// the registration. An instance the failed installation never started, or one
// whose process already ended, is stopped already: the stop is a clean no-op with
// no control delivered, so the restore proceeds instead of failing over a service
// that has nothing to stop.
func TestSCMStopOfAnInstanceThatIsStoppedIsACleanNoOp(t *testing.T) {
	handle := &scmTestServiceControl{state: svc.Stopped}

	manager := &scmManager{scope: ScopeSystem}
	if err := manager.stopService(context.Background(), handle); err != nil {
		t.Fatalf("stopService() = %v, want nil for an instance that does not run", err)
	}
	if handle.controls != 0 {
		t.Errorf("the stop control was delivered %d times, want none for an instance that does not run", handle.controls)
	}
	if handle.state != svc.Stopped {
		t.Errorf("the state of the instance = %v, want %v", handle.state, svc.Stopped)
	}
}

// TestSCMStartAndVerifyExcludesEveryProcessOfTheAttempt checks the seam the
// restart rollback needs at compile time.
//
// The verification of a start takes the processes it must not accept as the
// running instance, so the installation excludes the process it replaced and the
// rollback excludes the process of the failed attempt next to it. A verification
// that could only exclude one process would let the rollback accept the process
// it did not start.
//
// The sequence itself (stop the instance of the failed installation, restore the
// registration, start the previous program, verify it) needs a registration of
// the service control manager: a test cannot create one without an elevated
// shell, so the sequence is only proven on a machine that runs the elevated
// tests.
func TestSCMStartAndVerifyExcludesEveryProcessOfTheAttempt(t *testing.T) {
	var start func(*scmManager, context.Context, *mgr.Service, string, ...uint32) (scmImageCheck, error) = (*scmManager).startAndVerify
	var verify func(*scmManager, context.Context, *mgr.Service, string, ...uint32) (scmImageCheck, error) = (*scmManager).verifyRunningExecutable
	if start == nil || verify == nil {
		t.Fatal("the verification of a start is gone, the restart rollback cannot prove the instance it started")
	}

	// The rollback excludes the process the installation replaced and the
	// process of the failed attempt: the exclusion list it passes keeps the
	// process it did not start out of the answer.
	exclude := []uint32{4710, 4711}
	if !scmProcessExcluded(exclude, 4711) {
		t.Errorf("scmProcessExcluded(%v, 4711) = false, want the process of the failed attempt excluded", exclude)
	}
	if !scmProcessExcluded(exclude, 4710) {
		t.Errorf("scmProcessExcluded(%v, 4710) = false, want the replaced process excluded", exclude)
	}
	if scmProcessExcluded(exclude, 4712) {
		t.Errorf("scmProcessExcluded(%v, 4712) = true, want the process the rollback started accepted", exclude)
	}
}
