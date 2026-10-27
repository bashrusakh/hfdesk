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
// the state of an action is read until it settles. It is a variable so a test
// can shorten the wait: an action whose expectation never holds has to fail,
// and a test must not wait for the whole budget to prove that.
var serviceVerifyBudget = 30 * time.Second

// serviceVerifyInterval is the pause between two reads of the state of the
// service.
const serviceVerifyInterval = 200 * time.Millisecond

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
// outcome of the action in a native message box, because a GUI process has no
// console to report it to.
//
// The action waits for the desktop host that launched it before it starts or
// restarts the service: the desktop stops its own server through the graceful
// path and ends, and only then may the service bind the address. A wait that
// times out is a failure of the action, so the service never starts next to a
// desktop that still serves.
//
// The helper installs no logging pipeline: the rotating log file belongs to the
// launch that runs the desktop host, and a second writer breaks its rotation. The
// message box is the surface of the helper, and it reports every result,
// including a success.
func RunServiceAction(o ServiceActionOptions) error {
	action, err := parseServiceAction(o.Action)
	if err != nil {
		desktopLog.Error("the service helper was started with an unknown action", "action", o.Action)
		showMessage(serviceFailureCaption(""), unknownServiceActionText(err), windows.MB_ICONERROR)
		return err
	}

	if o.WaitPID > 0 {
		if err := waitForProcess(o.WaitPID, serviceWaitBudget); err != nil {
			reportServiceFailure(action, err)
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
// the console program next to the helper: the desktop host downloads and copies
// nothing, so the service runs the program of the folder the helper was started
// from. No field of the installation is named, so an installation over an
// existing registration keeps every setting the instance already runs with.
//
// The installation never starts an instance itself, so the start is a separate
// step of the same action. A service that was installed but cannot be started is
// reported as a failure of the installation instead of leaving the user with a
// registered instance that serves nothing.
func installWindowsService(action serviceAction, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), serviceActionBudget)
	defer cancel()

	opts, manager, err := serviceInstallation()
	if err != nil {
		reportServiceFailure(action, err)
		return err
	}

	installed, err := manager.Install(ctx, opts)
	if err != nil {
		reportServiceFailure(action, err)
		return err
	}
	if err := manager.Start(ctx); err != nil {
		reportServiceFailure(action, err)
		return err
	}
	status, err := waitForServiceState(ctx, manager, true)
	if err != nil {
		reportServiceFailure(action, err)
		return err
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

// updateWindowsService re-registers the service from the folder of the helper
// and keeps the running state it had.
//
// The state before the update decides the expectation: a service that was
// running has to run afterwards, and a service that was not running has to be
// stopped afterwards. The update never starts a stopped service, so a service
// that is running after an update of a stopped one is a failure of the action
// and is reported as such.
//
// The update only re-points an existing registration. A machine without one is
// a failure of the action: an update that registered a stopped service would
// leave the user with an instance that serves nothing, and the installation is
// the action that starts what it registers.
func updateWindowsService(action serviceAction, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), serviceActionBudget)
	defer cancel()

	opts, manager, err := serviceInstallation()
	if err != nil {
		reportServiceFailure(action, err)
		return err
	}

	before, err := manager.Status(ctx)
	if err != nil {
		reportServiceFailure(action, fmt.Errorf("the state of the service before the update cannot be read: %w", err))
		return err
	}
	if before.State == service.StateNotInstalled {
		return reportServiceNotInstalled(action)
	}
	wasRunning := serviceStateRunning(before.State)

	installed, err := manager.Install(ctx, opts)
	if err != nil {
		reportServiceFailure(action, err)
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
			expected, before.State, err))
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
		reportServiceFailure(action, err)
		return err
	}
	before, err := manager.Status(ctx)
	if err != nil {
		reportServiceFailure(action, fmt.Errorf("the state of the service cannot be read: %w", err))
		return err
	}
	if before.State == service.StateNotInstalled {
		return reportServiceNotInstalled(action)
	}
	if err := manager.Start(ctx); err != nil {
		reportServiceFailure(action, err)
		return err
	}
	status, err := waitForServiceState(ctx, manager, true)
	if err != nil {
		reportServiceFailure(action, err)
		return err
	}

	desktopLog.Info("the service is started", "executable", status.Executable, "listen", status.Listen)
	showMessage(serviceStartedCaption,
		serviceStartedText(status.Executable, serviceWebAddress(status)),
		windows.MB_ICONINFORMATION)
	return nil
}

// serviceInstallation returns the installation options and the manager of an
// installation or an update.
//
// The options are the defaults of the service layer for the console program in
// the folder of the helper: the desktop host names no value of its own, so an
// installation over an existing registration keeps the settings the instance
// already runs with.
func serviceInstallation() (service.InstallOptions, service.Manager, error) {
	executable, err := desktopExecutable()
	if err != nil {
		return service.InstallOptions{}, nil, err
	}
	console, err := findSiblingExecutable(filepath.Dir(executable))
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

// serviceProgram returns the program the registration of the service runs.
//
// The installation reports the registration as written, which is what an
// installation over an existing registration reports: the merged registration.
// A manager that reports no program is not a reason to report an empty one, so
// the request is the fallback.
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
// the user.
//
// The helper is a GUI process: a failure that is only recorded leaves the user
// with a helper that vanished. The box carries the underlying error, so the user
// can see what the service control manager refused.
func reportServiceFailure(action serviceAction, err error) {
	if err == nil {
		return
	}
	desktopLog.Error("the service action failed", "action", string(action), "error", err)
	showMessage(serviceFailureCaption(action), serviceActionFailureText(action, err), windows.MB_ICONERROR)
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
