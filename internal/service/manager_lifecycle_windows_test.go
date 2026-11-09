// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

func TestSCMStatusUsesAcceptedFlagSyntaxAndLastValue(t *testing.T) {
	executable, err := os.Executable()
	assertNoError(t, err, "Executable() = %v", err)
	root := t.TempDir()
	configFile := filepath.Join(root, "new", "hfdesk.json")
	stateDir := filepath.Join(root, "new", "state")
	logDir := filepath.Join(root, "new", "logs")
	cacheDir := filepath.Join(root, "new", "cache")
	hubDir := filepath.Join(root, "new", "hub")
	localDir := filepath.Join(root, "new", "models")
	args := []string{
		"-config=C:\\old\\hfdesk.json", "--config", configFile,
		"-state-dir", `C:\old\state`, "--state-dir=" + stateDir,
		"-log-dir=" + logDir,
		"-listen=127.0.0.1:8181", "--listen", "127.0.0.1:9191",
		"-cache-dir=C:\\old\\cache", "--cache-dir=" + cacheDir,
		"-hub-dir=C:\\old\\hub", "--hub-dir=" + hubDir,
		"-local-dir=C:\\old\\models", "--local-dir=" + localDir,
	}
	commandLine := syscall.EscapeArg(executable)
	for _, arg := range args {
		commandLine += " " + syscall.EscapeArg(arg)
	}
	facts := scmRegistrationFacts{
		present:     true,
		state:       svc.Running,
		commandLine: commandLine,
	}
	status, err := scmStatusFrom(ScopeSystem, facts)
	assertNoError(t, err, "scmStatusFrom() = %v", err)
	if status.Executable != executable || status.Listen != "127.0.0.1:9191" || status.Paths != (runtimepaths.Paths{ConfigFile: configFile, StateDir: stateDir, LogDir: logDir}) || status.CacheDir != cacheDir {
		t.Errorf("status = %+v, want the last values of the registered command", status)
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
	assertNoError(t, err, "Status() = %v, want nil", err)
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
	assertNoError(t, err, "Status() = %v, want nil", err)
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
	assertNoError(t, err, "the Install() of a named listen address = %v, want nil", err)
	if effective.Listen != named.Listen {
		t.Errorf("the effective listen address = %q, want the named one %q", effective.Listen, named.Listen)
	}
	if effective.Paths != opts.Paths {
		t.Errorf("the effective paths = %+v, want the kept ones %+v", effective.Paths, opts.Paths)
	}
	status, err = service.Status(ctx)
	assertNoError(t, err, "Status() = %v, want nil", err)
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
	assertNoError(t, err, "Status() = %v, want nil", err)
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
	assertNoError(t, err, "mgr.Connect() = %v, want nil", err)
	defer manager.Disconnect()
	handle, err := manager.OpenService(ServiceName)
	assertNoError(t, err, "OpenService(%q) = %v, want nil", ServiceName, err)
	defer handle.Close()

	actions, err := handle.RecoveryActions()
	assertNoError(t, err, "RecoveryActions() = %v, want nil", err)
	if !reflect.DeepEqual(actions, scmRecoveryActions()) {
		t.Errorf("the recovery actions of the registration = %v, want %v", actions, scmRecoveryActions())
	}
	period, err := handle.ResetPeriod()
	assertNoError(t, err, "ResetPeriod() = %v, want nil", err)
	if period != scmRecoveryResetPeriod {
		t.Errorf("the reset period of the registration = %d seconds, want %d", period, scmRecoveryResetPeriod)
	}
	nonCrash, err := handle.RecoveryActionsOnNonCrashFailures()
	assertNoError(t, err, "RecoveryActionsOnNonCrashFailures() = %v, want nil", err)
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
	assertNoError(t, err, "scmStatusFrom() = %v, want nil", err)
	if status.State != StateRunning {
		t.Errorf("the state = %q, want %q", status.State, StateRunning)
	}
	if !status.Running {
		t.Error("a running instance is reported as not running")
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
	assertNoError(t, err, "scmStatusFrom() = %v, want nil for an absent registration", err)
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
	assertNoError(t, err, "scmStatusFrom() = %v, want nil", err)
	if status.State != StateBroken {
		t.Errorf("the state = %q, want %q", status.State, StateBroken)
	}
	if !strings.Contains(status.Detail, missing) {
		t.Errorf("the detail = %q, want the path of the missing executable %q", status.Detail, missing)
	}
	if status.Running {
		t.Error("a stopped instance is reported as running")
	}
}

// TestSCMStatusFromABrokenRegistrationThatRunsKeepsTheRunningFact checks that a
// registration whose executable is gone is reported as broken while the instance
// it started still runs.
//
// The service control manager keeps the process it started: deleting the program
// stops the next start, not the instance. The state stays the diagnosis of the
// registration, and the running fact is carried next to it, so a caller that has
// to know whether anything serves does not read "broken" as "nothing runs".
func TestSCMStatusFromABrokenRegistrationThatRunsKeepsTheRunningFact(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "gone", "hfdesk.exe")
	opts := scmTestOptions()
	opts.Executable = missing

	facts := scmRegistrationFacts{
		present:     true,
		state:       svc.Running,
		processID:   4242,
		commandLine: scmCommandLine(opts),
	}

	status, err := scmStatusFrom(ScopeSystem, facts)
	assertNoError(t, err, "scmStatusFrom() = %v, want nil", err)
	if status.State != StateBroken {
		t.Errorf("the state = %q, want the diagnosis %q of the registration", status.State, StateBroken)
	}
	if !status.Running {
		t.Error("a registration the supervisor reports as running is reported as not running")
	}
	for _, want := range []string{missing, "the instance still runs"} {
		if !strings.Contains(status.Detail, want) {
			t.Errorf("the detail = %q, want it to mention %q", status.Detail, want)
		}
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
	assertNoError(t, err, "scmStatusFrom() = %v, want nil", err)
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
	assertNoError(t, err, "scmStatusFrom() = %v, want nil", err)
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
	assertNoError(t, err, "scmStatusFrom() = %v, want nil", err)
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
		// wantRunning is the running fact of the same supervisor answer.
		wantRunning bool
	}{
		{"running", svc.Running, StateRunning, true},
		{"start pending", svc.StartPending, StateStarting, true},
		{"stop pending", svc.StopPending, StateStopping, false},
		{"paused", svc.Paused, StateUnknown, false},
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
			assertNoError(t, err, "scmStatusFrom() = %v, want nil", err)
			if status.State != tc.want {
				t.Errorf("the state = %q, want %q", status.State, tc.want)
			}
			if status.Running != tc.wantRunning {
				t.Errorf("the running fact = %v, want %v", status.Running, tc.wantRunning)
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
	assertNoError(t, err, "scmStatusFrom() = %v, want nil for an absent registration", err)
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
	assertNoError(t, err, "scmLookupRegistration(%q) = %v, want nil", name, err)
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
	assertNoError(t, err, "scmLookupRegistration(%q) = %v, want nil", name, err)
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
	assertNoError(t, err, "Status() = %v, want nil for an absent registration", err)
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
	err := os.WriteFile(existing, []byte("not a program"), 0o600)
	assertNoError(t, err, "WriteFile() = %v, want nil", err)
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
	assertNoError(t, err, "parseSCMCommandLine() = %v, want nil", err)

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
	assertNoError(t, err, "parseSCMCommandLine() = %v, want nil", err)
	status = Status{}
	applyRegistration(&status, registration)
	if status.CacheDir != "" {
		t.Errorf("the cache dir = %q, want none", status.CacheDir)
	}
}
