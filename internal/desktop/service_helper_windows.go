// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
	"golang.org/x/sys/windows"
)

// serviceWaitBudget bounds the helper-ready handshake and the wait for the
// successful-completion evidence. It is a variable so tests can shorten it.
var serviceWaitBudget = 60 * time.Second

// serviceParentExitBudget bounds the subsequent wait for the original desktop
// exit. It is separate on purpose: consuming the completion budget must never
// zero the exit budget, or a legitimate install would fail at the transition.
var serviceParentExitBudget = 60 * time.Second

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

// newServiceManager returns the manager of the Windows service control manager
// for the given scope.
//
// It is a package level variable so the tests can drive the helper without a
// registration: a test cannot register a machine-wide service without an
// elevated shell.
var newServiceManager = service.New

// RunServiceAction performs one action of the elevated helper.
//
// The helper is the desktop executable started with administrator rights and
// the hidden --service-action flag. It never starts a server, a tray icon or a
// WebView2 window: it drives the service control manager and reports the
// outcome in a native message box, because a GUI process has no console.
//
// An install is accepted only with a complete tray handover. The helper opens
// every required handle before it signals ready, then requires both the
// successful-completion event and the original desktop process exit.
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

	if err := validateServiceHandoverOptions(action, o); err != nil {
		(&serviceActionReporter{action: action}).handoverFailure(err)
		return err
	}
	var handover *helperHandover
	if o.WaitPID > 0 {
		handover, err = prepareHelperHandover(o)
		if err != nil {
			(&serviceActionReporter{action: action}).handoverFailure(err)
			return err
		}
		defer handover.close()
		// Ready authorizes the desktop shutdown, so the image the action
		// would put in service is checked before it: a program the host
		// cannot execute must fail while the desktop is still open.
		if err := preflightServiceProgram(); err != nil {
			(&serviceActionReporter{action: action, handover: handover}).handoverFailure(err)
			return err
		}
		if err := handover.signalReady(); err != nil {
			(&serviceActionReporter{action: action, handover: handover}).handoverFailure(err)
			return err
		}
		if err := handover.waitForAuthorization(serviceWaitBudget, serviceParentExitBudget); err != nil {
			(&serviceActionReporter{action: action, handover: handover}).handoverFailure(err)
			return err
		}
		// Authorization is settled: only the singleton outlives it, through
		// the service mutation, and is released before the final notice.
		handover.releaseAuthorizationHandles()
	} else if action == serviceActionUpdate || action == serviceActionStart {
		// Every mutation that starts or restarts the service holds the
		// per-session desktop singleton, whether the gate handed this
		// launch's object over or a command line was typed by hand: the
		// mutation takes the service out of a positively confirmed
		// stopped state, and a relaunch in that window could bind the
		// address the restarted instance needs. An empty name computes
		// the object of this session itself. The hold starts before any
		// mutation and ends with the action: the reporter paths release
		// it before the final notice, and the deferred close covers every
		// other return. A hold that cannot be established refuses the
		// action before anything is touched.
		release, holdErr := holdServiceSingleton(o.InstanceMutex)
		if holdErr != nil {
			(&serviceActionReporter{action: action}).singletonHoldRefused(holdErr)
			return holdErr
		}
		handover = &helperHandover{hold: release}
		defer handover.close()
	}
	return runServiceAction(action, o.Version, &serviceActionReporter{action: action, handover: handover})
}

// holdServiceSingleton holds the per-session desktop singleton for a service
// action that has no desktop handover wait.
//
// It is a package level variable so the tests can drive the refusal of an
// action that cannot hold the window: a hold that fails must refuse before
// the mutation, not run it unprotected.
var holdServiceSingleton = service.HoldDesktopSingleton

// preflightServiceProgram checks the console program of this folder with the
// image preflight the installation runs, before the ready signal; the
// installation keeps its own copy for a file that changes after this point.
func preflightServiceProgram() error {
	executable, err := desktopExecutable()
	if err != nil {
		return err
	}
	console, err := findSiblingExecutable(filepath.Dir(executable), executable)
	if err != nil {
		return fmt.Errorf("the console program of the service is missing: %w", err)
	}
	return service.PreflightExecutableImage(console)
}

func validateServiceHandoverOptions(action serviceAction, o ServiceActionOptions) error {
	if o.WaitPID < 0 || uint64(o.WaitPID) > uint64(^uint32(0)) {
		return errors.New("desktop: the original desktop process id is outside the Windows process-id range")
	}
	hasNames := strings.TrimSpace(o.ReadyEvent) != "" || strings.TrimSpace(o.SuccessEvent) != "" ||
		strings.TrimSpace(o.InstanceMutex) != ""
	if o.WaitPID == 0 && !hasNames {
		if action == serviceActionInstall {
			return errors.New("desktop: service installation requires a graceful desktop handover")
		}
		// An update or a start without any handover argument is accepted:
		// the helper computes the desktop singleton of its own session
		// and holds it through the mutation itself (see RunServiceAction),
		// so a hand-typed command line holds the same window the gate
		// hands over, and it still waits for no desktop exit.
		return nil
	}
	// An update or a start may carry the singleton identity alone: it holds
	// that desktop singleton through its mutation without waiting for a
	// desktop exit, because the gate that asked for it already ended. Every
	// other combination still needs the complete install handover below.
	if (action == serviceActionUpdate || action == serviceActionStart) && o.WaitPID == 0 &&
		strings.TrimSpace(o.ReadyEvent) == "" && strings.TrimSpace(o.SuccessEvent) == "" &&
		strings.TrimSpace(o.InstanceMutex) != "" {
		return nil
	}
	if action != serviceActionInstall || o.WaitPID <= 0 ||
		strings.TrimSpace(o.ReadyEvent) == "" || strings.TrimSpace(o.SuccessEvent) == "" ||
		strings.TrimSpace(o.InstanceMutex) == "" {
		return errors.New("desktop: incomplete service handover arguments")
	}
	return nil
}

func runServiceAction(action serviceAction, version string, reporter *serviceActionReporter) error {
	switch action {
	case serviceActionInstall:
		return installWindowsService(reporter, version)
	case serviceActionUpdate:
		return updateWindowsService(reporter, version)
	case serviceActionStart:
		return startWindowsService(reporter)
	}
	return fmt.Errorf("desktop: the service action %q cannot be performed", action)
}

// installWindowsService registers the service, starts it and verifies that it
// runs.
//
// The registration is the default installation of the service layer, made for
// the console program next to the helper: no field is named. A registration
// that already exists is refused by this first-install action instead of being
// merged with: it is left untouched and reported as a conflict.
// The installation starts its first instance itself, in the same action; a
// service that cannot be started is reported as a failure of the
// installation instead of leaving a registered instance that serves nothing.
//
// A failed first installation must not trap the user: the service gate ends
// every later launch, so a registration that serves nothing keeps the desktop
// app from starting its own server. Only a registration this action created is
// removed again when the start fails; one that existed before, or that appeared
// between the manager's lookup and its write, is never touched.
func installWindowsService(reporter *serviceActionReporter, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), serviceActionBudget)
	defer cancel()

	opts, manager, err := serviceInstallation(version)
	if err != nil {
		reporter.failure(err, "")
		return err
	}

	result, err := service.InstallAndStart(ctx, manager, opts)
	if err != nil {
		var startErr *service.InstallStartError
		if errors.As(err, &startErr) {
			reporter.firstInstallStartFailure(err, startErr, opts.Executable)
		} else {
			reporter.failure(err, opts.Executable)
		}
		return err
	}
	installed := result.Installation
	status := result.Status

	// The installation reports the registration as written. A manager that
	// reports none is not a reason to report an empty program: the request is
	// what the registration was made from.
	report := serviceProgram(installed, opts)
	desktopLog.Info("the service is installed", "executable", report, "listen", status.Listen,
		"versionVerified", installed.VersionVerified)
	reporter.message(serviceInstalledCaption,
		serviceInstalledText(verifiedVersion(installed), report, status.Paths.StateDir, serviceWebAddress(status)),
		windows.MB_ICONINFORMATION)
	return nil
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
func updateWindowsService(reporter *serviceActionReporter, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), serviceActionBudget)
	defer cancel()

	opts, manager, err := serviceInstallation(version)
	if err != nil {
		reporter.failure(err, "")
		return err
	}

	before, err := manager.Status(ctx)
	if err != nil {
		reporter.failure(fmt.Errorf("the state of the service before the update cannot be read: %w", err), "")
		return err
	}
	if before.State == service.StateNotInstalled {
		return reporter.notInstalled()
	}
	wasRunning := serviceStatusRunning(before)

	installed, err := manager.Install(ctx, opts)
	if err != nil {
		reporter.failure(err, opts.Executable)
		return err
	}
	status, err := waitForServiceState(ctx, manager, wasRunning)
	if err != nil {
		if errors.Is(err, errServiceRegistrationGone) {
			// The registration disappeared while the update ran: the report
			// names that fact instead of a state the machine no longer has,
			// and never claims an update that cannot be verified.
			reporter.failure(err, opts.Executable)
			return err
		}
		expected := "stopped"
		if wasRunning {
			expected = "running"
		}
		beforeText := fmt.Sprintf("was %q before the update", before.State)
		if wasRunning {
			beforeText = "was running before the update"
		}
		reporter.failure(fmt.Errorf(
			"the update must leave the service %s, because it %s: %w",
			expected, beforeText, err), serviceProgram(installed, opts))
		return err
	}

	report := serviceProgram(installed, opts)
	desktopLog.Info("the service is updated",
		"executable", report,
		"state", string(status.State),
		"wasRunning", wasRunning,
		"versionVerified", installed.VersionVerified)
	reporter.message(serviceUpdatedCaption,
		serviceUpdatedText(verifiedVersion(installed), report, status.Paths.StateDir,
			serviceWebAddress(status), serviceStatusRunning(status)),
		windows.MB_ICONINFORMATION)
	return nil
}

// startWindowsService starts the registered service and verifies that it runs.
//
// A machine without a registration is reported with the way to install it: the
// start of a service that does not exist is not an error of the service control
// manager the user could act on.
func startWindowsService(reporter *serviceActionReporter) error {
	ctx, cancel := context.WithTimeout(context.Background(), serviceActionBudget)
	defer cancel()

	manager, err := newServiceManager(service.ScopeSystem)
	if err != nil {
		reporter.failure(err, "")
		return err
	}
	before, err := manager.Status(ctx)
	if err != nil {
		reporter.failure(fmt.Errorf("the state of the service cannot be read: %w", err), "")
		return err
	}
	if before.State == service.StateNotInstalled {
		return reporter.notInstalled()
	}
	if err := manager.Start(ctx); err != nil {
		reporter.failure(err, before.Executable)
		return err
	}
	status, err := waitForServiceState(ctx, manager, true)
	if err != nil {
		reporter.failure(err, before.Executable)
		return err
	}

	desktopLog.Info("the service is started", "executable", status.Executable, "listen", status.Listen)
	reporter.message(serviceStartedCaption,
		serviceStartedText(status.Executable, serviceWebAddress(status)),
		windows.MB_ICONINFORMATION)
	return nil
}

// serviceInstallation returns the installation options and the manager of an
// installation or an update: the defaults of the service layer for the console
// program in the folder of the helper. The desktop host names no value of its
// own, so an installation over an existing registration keeps the settings the
// instance already runs with.
//
// A release build knows the version of the console program it selected; a local
// build ("dev") names no release and passes no expectation.
func serviceInstallation(version string) (service.InstallOptions, service.Manager, error) {
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
	opts := service.DefaultInstallOptions(service.ScopeSystem, console)
	if version = strings.TrimSpace(version); version != "" && version != "dev" {
		opts.ExpectedVersion = version
	}
	return opts, manager, nil
}

// verifiedVersion returns the version the installation really compared with the
// health endpoint, so a report never names a version the instance did not prove.
func verifiedVersion(installed service.InstallOptions) string {
	if !installed.VersionVerified {
		return ""
	}
	return installed.ExpectedVersion
}

// errServiceRegistrationGone reports a service whose registration disappeared
// while an action was verifying the state it expected: a machine without a
// registration has no state the action could confirm, so the action fails
// honestly instead of accepting "not installed" as the settled stopped state
// a stopped-service update waits for.
var errServiceRegistrationGone = errors.New(
	"the registration of the service is gone: HFDesk is no longer registered as a Windows service")

// waitForServiceState waits for the service to reach the expected state.
//
// The wait ends as soon as the service reached a settled state that matches the
// expectation, so an action that already holds does not wait. A pending state is
// not a result: a started instance is reported as starting first, and the
// caller reports what it read after the instance settled.
//
// A registration that is gone is not a result either: StateNotInstalled never
// matches the expectation, whatever the action waits for, and is rejected as
// soon as it is read instead of being treated as a settled non-running state.
//
// The expectation is checked against the supervisor's running fact: a
// registration whose program is gone is reported as broken while its process
// still serves, and such a service is running for the action that is waiting.
func waitForServiceState(ctx context.Context, manager service.Manager, wantRunning bool) (service.Status, error) {
	deadline := time.Now().Add(serviceVerifyBudget)
	for {
		status, err := manager.Status(ctx)
		if err != nil {
			return status, fmt.Errorf("the state of the service cannot be read: %w", err)
		}
		if status.State == service.StateNotInstalled {
			return status, errServiceRegistrationGone
		}
		if serviceStatusRunning(status) == wantRunning && serviceStateSettled(status.State) {
			return status, nil
		}
		if !time.Now().Before(deadline) {
			expected := "stopped"
			if wantRunning {
				expected = "running"
			}
			return status, fmt.Errorf("the service reports the state %q (running: %t) after %s, want a settled %s state",
				status.State, status.Running, serviceVerifyBudget, expected)
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-time.After(serviceVerifyInterval):
		}
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

type serviceActionReporter struct {
	action   serviceAction
	handover *helperHandover
}

func (r *serviceActionReporter) message(caption, text string, icon uint32) {
	if r.handover != nil {
		r.handover.close()
	}
	showMessage(caption, text, icon)
}

// failure reports a service-action failure after releasing the singleton that
// the desktop helper held across the handover.
func (r *serviceActionReporter) failure(err error, program string) {
	if err == nil {
		return
	}
	desktopLog.Error("the service action failed", "action", string(r.action), "error", err, "program", program)
	r.message(serviceFailureCaption(r.action), serviceActionFailureText(r.action, err, program), windows.MB_ICONERROR)
}

func (r *serviceActionReporter) handoverFailure(err error) {
	if err == nil {
		return
	}
	if r.handover != nil {
		r.handover.close()
	}
	desktopLog.Error("the service helper could not authorize the desktop handover", "action", string(r.action), "error", err)
	showMessage(serviceFailureCaption(r.action),
		"HFDesk could not confirm the desktop handover. No service action was performed.\n\n"+err.Error(),
		windows.MB_ICONERROR)
}

// singletonHoldRefused reports a mutation that refused before it changed
// anything because the desktop singleton could not be held.
//
// The refusal happens before the first change: no hold means no mutation, so
// the report states that no service action was performed and names the cause
// of the failed hold.
func (r *serviceActionReporter) singletonHoldRefused(err error) {
	if err == nil {
		return
	}
	desktopLog.Error("the service action refused without holding the desktop singleton",
		"action", string(r.action), "error", err)
	r.message(serviceFailureCaption(r.action),
		serviceSingletonHoldRefusedText(r.action, err), windows.MB_ICONERROR)
}

func (r *serviceActionReporter) firstInstallStartFailure(err error, cleanup *service.InstallStartError, program string) {
	if err == nil {
		return
	}
	var cleanupErr error
	removed := false
	if cleanup != nil {
		cleanupErr = cleanup.CleanupError
		removed = cleanup.RegistrationRemoved
	}
	desktopLog.Error("the first service installation could not be started",
		"error", err,
		"program", program,
		"registrationRemoved", removed,
		"cleanupError", cleanupErr)
	r.message(serviceFailureCaption(serviceActionInstall),
		serviceInstallStartFailureText(err, program, cleanup), windows.MB_ICONERROR)
}

func (r *serviceActionReporter) notInstalled() error {
	err := errors.New("HFDesk is not installed as a Windows service")
	desktopLog.Error("the service action needs a registration", "action", string(r.action))
	r.message(serviceFailureCaption(r.action), serviceNotInstalledText(r.action), windows.MB_ICONERROR)
	return err
}

// reportServiceLaunchFailure records the failure of a launch of the elevated
// helper and reports it to the user.
//
// A helper that never ran gets the only report its failure has: the helper is
// the surface of the actions it performs, and this one did not start. A
// helper that was started but never became ready already showed — or is
// showing — its own message with its own reason, so its report is the
// distinct one: it references that message instead of contradicting it with
// a caption that claims the helper could not start.
func reportServiceLaunchFailure(action serviceAction, err error) {
	if err == nil {
		return
	}
	var notReady *helperNotReadyError
	if errors.As(err, &notReady) {
		desktopLog.Error("the elevated service helper started but did not become ready", "action", string(action), "error", err)
		showMessage(serviceHelperNotReadyCaption, serviceHelperNotReadyText(action, err), windows.MB_ICONERROR)
		return
	}
	desktopLog.Error("the elevated service helper could not be started", "action", string(action), "error", err)
	showMessage(serviceFailureCaption(action), serviceLaunchFailedText(action, err), windows.MB_ICONERROR)
}
