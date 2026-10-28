// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
	"golang.org/x/sys/windows"
)

// serviceWaitBudget bounds the wait for the desktop host that launched the
// helper.
//
// The desktop stops its own server through the graceful path before it ends,
// and the host bounds that shutdown itself (gracefulShutdownTimeout). This
// budget covers the shutdown plus the exit of the process; a desktop that does
// not end within it is reported as a failure of the action instead of starting
// the service on an address that is still held.
const serviceWaitBudget = 60 * time.Second

// serviceActionBudget bounds one service action of the helper.
//
// The installation writes directories and a registration, and an update can
// restart the instance: the budget covers the shutdown of the running instance
// (ShutdownBudget), the registration and the verification of the resulting
// state.
const serviceActionBudget = 2 * time.Minute

// serviceVerifyBudget bounds the wait for the state an action expects.
//
// The service control manager reports a started instance as pending first, so
// the state is read until it settles. The wait wraps a wait of the instance
// itself, which probes its HTTP server for up to its readiness budget after the
// manager started the process: a budget of the inner size alone would declare a
// legitimate start failed just before it becomes ready. It is therefore twice
// the 30 s readiness budget of the service layer (scmReadinessTimeout).
//
// It is a variable so a test can shorten the wait.
var serviceVerifyBudget = 60 * time.Second

// serviceVerifyInterval is the pause between two reads of the state of the
// service.
const serviceVerifyInterval = 200 * time.Millisecond

// serviceRemovalBudget bounds the removal of a registration that a failed
// installation wrote.
//
// The removal runs on a context that the end of the action cannot cancel: the
// action budget may already be spent by the start that failed, and the removal
// may also have to stop an instance the failed start left behind. It is still
// bounded, so a service control manager that does not answer cannot keep the
// failure report from the user.
const serviceRemovalBudget = service.ShutdownBudget + 5*time.Second

// newServiceManager returns the manager of the Windows service control manager
// for the given scope.
//
// It is a package level variable so the tests can drive the helper without a
// registration: a test cannot register a machine-wide service without an
// elevated shell.
var newServiceManager = service.New

// waitForProcess waits for a process to exit.
//
// It is a package level variable so the tests can prove that the helper waits
// before it touches the service, without a second process.
var waitForProcess = waitForProcessExit

// RunServiceAction performs one action of the elevated helper.
//
// The helper is the desktop executable started with administrator rights and
// the hidden --service-action flag. It never starts a server, a tray icon or a
// WebView2 window: it drives the service control manager and reports the
// outcome in a native message box, because a GUI process has no console.
//
// The action waits for the desktop host that launched it before it starts or
// restarts the service: the desktop stops its own server through the graceful
// path and ends, and only then may the service bind the address. A wait that
// times out is a failure of the action.
//
// The helper installs no logging pipeline: the rotating log file belongs to the
// launch that runs the desktop host, and a second writer breaks its rotation.
func RunServiceAction(o ServiceActionOptions) error {
	action, err := parseServiceAction(o.Action)
	if err != nil {
		desktopLog.Error("the service helper was started with an unknown action", "action", o.Action)
		showMessage(serviceFailureCaption(""), unknownServiceActionText(err), windows.MB_ICONERROR)
		return err
	}

	if o.WaitPID > 0 {
		if err := waitForProcess(o.WaitPID, serviceWaitBudget); err != nil {
			reportServiceFailure(action, err, "")
			return err
		}
	}

	switch action {
	case serviceActionInstall:
		return installWindowsService(action, o.Version)
	case serviceActionUpdate:
		return updateWindowsService(action, o.Version)
	case serviceActionStart:
		return startWindowsService(action)
	}
	return fmt.Errorf("desktop: the service action %q cannot be performed", action)
}

// installWindowsService registers the service, starts it and verifies that it
// runs.
//
// The registration is the default installation of the service layer, made for
// the console program next to the helper: no field is named, so an installation
// over an existing registration keeps every setting the instance runs with.
// The installation never starts an instance itself, so the start is a separate
// step; a service that cannot be started is reported as a failure of the
// installation instead of leaving a registered instance that serves nothing.
//
// A failed first installation must not trap the user: the service gate ends
// every later launch, so a registration that serves nothing keeps the desktop
// app from starting its own server. The state of the machine is therefore read
// before the installation, and only a registration this action wrote is removed
// again when the start or its verification fails. A registration that existed
// before and a state that could not be read are never touched.
func installWindowsService(action serviceAction, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), serviceActionBudget)
	defer cancel()

	opts, manager, err := serviceInstallation()
	if err != nil {
		reportServiceFailure(action, err, "")
		return err
	}

	// The state before the installation decides what a failure may remove: a
	// registration that existed before the action is not this action's to
	// delete, and a state that cannot be read is not evidence of a machine
	// without a service. The installation proceeds either way; only the
	// removal after a failure depends on this read.
	before, beforeErr := manager.Status(ctx)
	freshInstall := beforeErr == nil && before.State == service.StateNotInstalled
	if beforeErr != nil {
		desktopLog.Warn("the state of the service before the installation cannot be read",
			"error", beforeErr)
	} else {
		desktopLog.Info("the state of the service before the installation",
			"state", string(before.State), "freshInstall", freshInstall)
	}

	installed, err := manager.Install(ctx, opts)
	if err != nil {
		reportServiceFailure(action, err, opts.Executable)
		return err
	}
	program := serviceProgram(installed, opts)

	// The failure of the start and the failure of its verification are the
	// same outcome for the user: the service does not run, and what happens
	// to the registration depends on whether this action wrote it.
	fail := func(err error) error {
		aftermath, removalErr := removeFailedInstallation(ctx, manager, freshInstall)
		desktopLog.Error("the service action failed",
			"action", string(action), "error", err, "program", program,
			"aftermath", aftermath.String(), "removalError", removalErr)
		showMessage(serviceFailureCaption(action),
			serviceInstallFailureText(err, program, aftermath, removalErr), windows.MB_ICONERROR)
		return err
	}

	if err := manager.Start(ctx); err != nil {
		return fail(err)
	}
	status, err := waitForServiceState(ctx, manager, true)
	if err != nil {
		return fail(err)
	}

	// The installation reports the registration as written. A manager that
	// reports none is not a reason to report an empty program: the request is
	// what the registration was made from.
	report := serviceProgram(installed, opts)
	desktopLog.Info("the service is installed", "executable", report, "listen", status.Listen)
	showMessage(serviceInstalledCaption,
		serviceInstalledText(version, report, status.Paths.StateDir, serviceWebAddress(status)),
		windows.MB_ICONINFORMATION)
	return nil
}

// removeFailedInstallation removes the registration of a failed installation and
// reports what is left on the machine.
//
// freshInstall reports whether the registration was written by this action, and
// only that registration is removed, and only the registration: the data
// directory stays where it is.
//
// A destructive step is taken only on a state that was observed: the state is
// read again, and the registration is removed only when it reports a settled
// state that does not serve. A running or still starting instance is never torn
// down, and a state that cannot be read is not evidence of a stopped service.
//
// The removal runs on a context that is never cancelled with the action, so a
// spent action budget cannot keep the registration behind.
func removeFailedInstallation(ctx context.Context, manager service.Manager, freshInstall bool) (serviceInstallAftermath, error) {
	if !freshInstall {
		desktopLog.Info("the registration of the service is left in place: it was not written by this installation")
		return serviceInstallKept, nil
	}

	removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serviceRemovalBudget)
	defer cancel()

	status, err := manager.Status(removeCtx)
	if err != nil {
		desktopLog.Warn("the state of the failed installation cannot be read: the registration is left in place",
			"error", err)
		return serviceInstallKept, errServiceRemovalSkipped
	}
	if serviceStateRunning(status.State) || !serviceStateSettled(status.State) {
		desktopLog.Info("the instance of the failed installation is running or still in transition: the registration is left in place",
			"state", string(status.State))
		return serviceInstallKept, errServiceRemovalSkipped
	}

	if err := manager.Uninstall(removeCtx); err != nil {
		desktopLog.Error("the registration of the failed installation could not be removed", "error", err)
		return serviceInstallRemovalFailed, err
	}
	desktopLog.Info("the registration of the failed installation was removed again")
	return serviceInstallRemoved, nil
}

// updateWindowsService re-registers the service from the folder of the helper
// and keeps the running state it had.
//
// A service that was running has to run afterwards, and one that was not
// running has to be stopped afterwards. The update never starts a stopped
// service, so a running service after an update of a stopped one is a failure
// of the action.
//
// The update only re-points an existing registration: a machine without one is
// a failure, because an update that registered a stopped service would leave an
// instance that serves nothing.
func updateWindowsService(action serviceAction, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), serviceActionBudget)
	defer cancel()

	opts, manager, err := serviceInstallation()
	if err != nil {
		reportServiceFailure(action, err, "")
		return err
	}

	before, err := manager.Status(ctx)
	if err != nil {
		reportServiceFailure(action, fmt.Errorf("the state of the service before the update cannot be read: %w", err), "")
		return err
	}
	if before.State == service.StateNotInstalled {
		return reportServiceNotInstalled(action)
	}
	wasRunning := serviceStateRunning(before.State)

	installed, err := manager.Install(ctx, opts)
	if err != nil {
		reportServiceFailure(action, err, opts.Executable)
		return err
	}
	status, err := waitForServiceState(ctx, manager, wasRunning)
	if err != nil {
		expected := "stopped"
		if wasRunning {
			expected = "running"
		}
		reportServiceFailure(action, fmt.Errorf(
			"the update must leave the service %s, because it was %q before the update: %w",
			expected, before.State, err), serviceProgram(installed, opts))
		return err
	}

	report := serviceProgram(installed, opts)
	desktopLog.Info("the service is updated",
		"executable", report,
		"state", string(status.State),
		"wasRunning", wasRunning)
	showMessage(serviceUpdatedCaption,
		serviceUpdatedText(version, report, status.Paths.StateDir,
			serviceWebAddress(status), serviceStateRunning(status.State)),
		windows.MB_ICONINFORMATION)
	return nil
}

// startWindowsService starts the registered service and verifies that it runs.
//
// A machine without a registration is reported with the way to install it: the
// start of a service that does not exist is not an error of the service control
// manager the user could act on.
func startWindowsService(action serviceAction) error {
	ctx, cancel := context.WithTimeout(context.Background(), serviceActionBudget)
	defer cancel()

	manager, err := newServiceManager(service.ScopeSystem)
	if err != nil {
		reportServiceFailure(action, err, "")
		return err
	}
	before, err := manager.Status(ctx)
	if err != nil {
		reportServiceFailure(action, fmt.Errorf("the state of the service cannot be read: %w", err), "")
		return err
	}
	if before.State == service.StateNotInstalled {
		return reportServiceNotInstalled(action)
	}
	if err := manager.Start(ctx); err != nil {
		reportServiceFailure(action, err, before.Executable)
		return err
	}
	status, err := waitForServiceState(ctx, manager, true)
	if err != nil {
		reportServiceFailure(action, err, before.Executable)
		return err
	}

	desktopLog.Info("the service is started", "executable", status.Executable, "listen", status.Listen)
	showMessage(serviceStartedCaption,
		serviceStartedText(status.Executable, serviceWebAddress(status)),
		windows.MB_ICONINFORMATION)
	return nil
}

// serviceInstallation returns the installation options and the manager of an
// installation or an update: the defaults of the service layer for the console
// program in the folder of the helper. The desktop host names no value of its
// own, so an installation over an existing registration keeps the settings the
// instance already runs with.
func serviceInstallation() (service.InstallOptions, service.Manager, error) {
	executable, err := desktopExecutable()
	if err != nil {
		return service.InstallOptions{}, nil, err
	}
	console, err := findSiblingExecutable(filepath.Dir(executable), executable)
	if err != nil {
		return service.InstallOptions{}, nil, fmt.Errorf("the console program of the service is missing: %w", err)
	}
	manager, err := newServiceManager(service.ScopeSystem)
	if err != nil {
		return service.InstallOptions{}, nil, fmt.Errorf("the service control manager of Windows is not available: %w", err)
	}
	return service.DefaultInstallOptions(service.ScopeSystem, console), manager, nil
}

// waitForServiceState waits for the service to reach the expected state.
//
// The wait ends as soon as the service reached a settled state that matches the
// expectation, so an action that already holds does not wait. A pending state is
// not a result: a started instance is reported as starting first, and the
// caller reports what it read after the instance settled.
func waitForServiceState(ctx context.Context, manager service.Manager, wantRunning bool) (service.Status, error) {
	deadline := time.Now().Add(serviceVerifyBudget)
	for {
		status, err := manager.Status(ctx)
		if err != nil {
			return status, fmt.Errorf("the state of the service cannot be read: %w", err)
		}
		if serviceStateRunning(status.State) == wantRunning && serviceStateSettled(status.State) {
			return status, nil
		}
		if !time.Now().Before(deadline) {
			expected := "stopped"
			if wantRunning {
				expected = "running"
			}
			return status, fmt.Errorf("the service reports the state %q after %s, want a settled %s state",
				status.State, serviceVerifyBudget, expected)
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-time.After(serviceVerifyInterval):
		}
	}
}

// waitForProcessExit waits until the process exited.
//
// The process is observed through a handle of the process, so the wait ends on
// the end of the process and not on a poll that could miss it. A process that
// does not exist anymore is a process that exited: the helper then has nothing
// to wait for and starts the service.
func waitForProcessExit(pid int, budget time.Duration) error {
	if pid <= 0 {
		return nil
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return fmt.Errorf("the HFDesk desktop app (process %d) cannot be observed: %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	event, err := windows.WaitForSingleObject(handle, uint32(budget/time.Millisecond))
	if err != nil {
		return fmt.Errorf("waiting for the HFDesk desktop app (process %d) failed: %w", pid, err)
	}
	switch event {
	case windows.WAIT_OBJECT_0:
		return nil
	case uint32(windows.WAIT_TIMEOUT):
		return fmt.Errorf("the HFDesk desktop app (process %d) did not exit within %s", pid, budget)
	default:
		return fmt.Errorf("waiting for the HFDesk desktop app (process %d) ended with the event %d", pid, event)
	}
}

// serviceProgram returns the program the registration of the service runs: the
// effective registration, or the request when the manager reports no program.
func serviceProgram(installed, requested service.InstallOptions) string {
	if installed.Executable != "" {
		return installed.Executable
	}
	return requested.Executable
}

// serviceWebAddress returns the address of the service a user can open.
//
// The registered address is the fallback: a registration whose address cannot be
// read as a URL still names the address the service binds, and naming it is
// better than reporting no address at all.
func serviceWebAddress(status service.Status) string {
	if url, err := serviceURL(status.Listen); err == nil {
		return url
	}
	return status.Listen
}

// reportServiceFailure records the failure of a service action and reports it to
// the user: a GUI helper that only records a failure vanishes without telling
// the user. The box carries the underlying error and the program of the action.
func reportServiceFailure(action serviceAction, err error, program string) {
	if err == nil {
		return
	}
	desktopLog.Error("the service action failed", "action", string(action), "error", err, "program", program)
	showMessage(serviceFailureCaption(action), serviceActionFailureText(action, err, program), windows.MB_ICONERROR)
}

// reportServiceLaunchFailure records the failure of a launch of the elevated
// helper and reports it to the user.
//
// The helper never ran, so the report is the only surface the failure has: the
// helper is the surface of the actions it performs, and this one did not start.
func reportServiceLaunchFailure(action serviceAction, err error) {
	if err == nil {
		return
	}
	desktopLog.Error("the elevated service helper could not be started", "action", string(action), "error", err)
	showMessage(serviceFailureCaption(action), serviceLaunchFailedText(action, err), windows.MB_ICONERROR)
}

// reportServiceNotInstalled reports an action that needs a registration on a
// machine without one.
func reportServiceNotInstalled(action serviceAction) error {
	err := errors.New("HFDesk is not installed as a Windows service")
	desktopLog.Error("the service action needs a registration", "action", string(action))
	showMessage(serviceFailureCaption(action), serviceNotInstalledText(action), windows.MB_ICONERROR)
	return err
}
