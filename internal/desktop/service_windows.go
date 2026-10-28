// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
	"golang.org/x/sys/windows"
)

// serviceStatusTimeout bounds the read of the service registration at the
// startup of the desktop host.
//
// The lookup is a local read of the service control manager, so the budget only
// covers a busy machine or one that does not answer, and a service that is
// starting or stopping is read again until it settled. It is a variable so the
// tests can shorten the wait.
var serviceStatusTimeout = 10 * time.Second

// serviceGatePollInterval is the pause between two reads of the service state
// while the service is starting or stopping.
//
// The state is read again shortly, so a launch that found the service in the
// middle of a transition decides on the state it reached and not on the
// transition.
const serviceGatePollInterval = 200 * time.Millisecond

// errServiceGatePending reports a service that is still starting or stopping
// when the budget of the service gate ends.
//
// A pending state is not a state to decide on: it is neither a running nor a
// stopped service, so the gate offers no action for it.
var errServiceGatePending = errors.New("the service is still starting or stopping")

// serviceStatus reads the registration of the Windows service.
//
// It is a package level variable so the tests can drive the startup of the
// host without a service: a test cannot register a machine-wide service
// without an elevated shell.
var serviceStatus = lookupServiceStatus

// lookupServiceStatus reads the registration of the Windows service with the
// factory of the service layer. The desktop host drives no supervisor of its
// own: it only asks the shared layer for the state of the registration.
func lookupServiceStatus(ctx context.Context) (service.Status, error) {
	manager, err := service.New(service.ScopeSystem)
	if err != nil {
		return service.Status{}, err
	}
	return manager.Status(ctx)
}

// serviceGate decides whether this launch starts its own server.
//
// The Windows service is a second HFDesk on the same machine and binds the same
// address, so a launch that finds a registration never starts a server of its
// own: it asks the user what to do and ends.
//
// The gate runs before the application is created and reports true only when the
// machine has no registration, which is the launch the desktop host had before
// the service existed. A registration that cannot be read is not a machine
// without one: the gate starts no server and reports the failure with a notice
// that names the way to read the state and to try again.
//
// A service that is starting or stopping is not a state to decide on, so the
// gate waits for it to settle first; one that did not settle within the budget
// is reported with a notice and ends the launch as well.
func serviceGate() bool {
	ctx, cancel := context.WithTimeout(context.Background(), serviceStatusTimeout)
	defer cancel()

	status, err := serviceGateStatus(ctx)
	if err != nil {
		if errors.Is(err, errServiceGatePending) {
			desktopLog.Info("the Windows service is still starting or stopping: this launch changes nothing",
				"state", string(status.State))
			showMessage(serviceGatePendingCaption,
				serviceGatePendingText(status.State), windows.MB_ICONINFORMATION)
			return false
		}
		// The launch cannot tell a machine without a service from a machine
		// with one, so it decides on neither: it starts no server and leaves
		// the state of the machine as it is.
		desktopLog.Error("could not read the Windows service registration: this launch starts no server", "error", err)
		showMessage(serviceGateUnreadableCaption,
			serviceGateUnreadableText(err), windows.MB_ICONERROR)
		return false
	}
	if status.State == service.StateNotInstalled {
		return true
	}

	prompt := serviceChoicePrompt{
		State:   status.State,
		Listen:  status.Listen,
		DataDir: status.Paths.StateDir,
		Detail:  status.Detail,
	}
	if url, err := serviceURL(status.Listen); err == nil {
		prompt.URL = url
	}
	desktopLog.Info("the Windows service is installed",
		"state", string(status.State),
		"url", prompt.URL,
		"dataDir", prompt.DataDir)

	switch showServiceChoice(prompt) {
	case serviceChoiceOpen:
		// The URL is derived again from the registration: the dialog only
		// carried it, so the address the browser opens is the address of
		// the registration and nothing else.
		url, err := serviceURL(status.Listen)
		if err != nil {
			reportBrowserFailure(status.Listen, err)
			return false
		}
		if err := openBrowser(url); err != nil {
			reportBrowserFailure(url, err)
		}
	case serviceChoiceStart:
		if _, err := launchServiceHelper(serviceActionStart, 0); err != nil {
			reportServiceLaunchFailure(serviceActionStart, err)
		}
	case serviceChoiceUpdate:
		// The gate started no server, so nothing holds the listen address
		// and the helper has nothing to wait for.
		if _, err := launchServiceHelper(serviceActionUpdate, 0); err != nil {
			reportServiceLaunchFailure(serviceActionUpdate, err)
		}
	default:
		desktopLog.Info("the service gate changed nothing")
	}
	return false
}

// serviceGateStatus reads the registration of the service and waits for a
// pending state to settle.
//
// The service control manager reports a started or a stopped instance as pending
// first, and deciding on a stopping instance would offer the start of a service
// that is still on its way down. A state that did not settle within the budget
// is returned with errServiceGatePending.
func serviceGateStatus(ctx context.Context) (service.Status, error) {
	for {
		status, err := serviceStatus(ctx)
		if err != nil {
			return status, err
		}
		if serviceStateSettled(status.State) {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return status, errServiceGatePending
		case <-time.After(serviceGatePollInterval):
		}
	}
}

// serviceInstallRunning is the guard of the tray installation action.
//
// The tray keeps delivering clicks while the confirmation of a click is open:
// the action runs on a goroutine of its own, so the message loop of the tray is
// never blocked by the message box. Only one installation action may run at a
// time: a second one would ask the same confirmation again, raise a second
// prompt of the user account control and launch a second elevated helper, and
// the two helpers would write the same registration and start the service next
// to each other.
//
// The guard is held for the whole action and released by every path that ends
// without a launched helper, so a declined confirmation, a missing console
// program or a refused elevation leaves the tray item working. The successful
// path keeps it: the host quits after the hand-over, and nothing may start a
// second installation while the process ends.
var serviceInstallRunning atomic.Bool

// installServiceFromTray registers the Windows service from the tray.
//
// The action asks the user to confirm the installation before anything is
// changed, because the registration is a machine-wide change that survives the
// desktop app. The console program is verified before the user is asked, so a
// folder without it is reported instead of after the elevation prompt.
//
// The helper is launched elevated; it waits for this process before it starts
// the service, so the desktop can stop its own server first. The action reports
// true when the host has to quit, which is the case after a successful launch
// and never after a failure.
func installServiceFromTray() bool {
	if !serviceInstallRunning.CompareAndSwap(false, true) {
		desktopLog.Info("an installation action is already running: this click on the tray item is ignored")
		return false
	}

	// The guard is released when this call ends without a launched helper, so
	// the tray item works again after every failure. The hand-over keeps it.
	handedOver := false
	defer func() {
		if !handedOver {
			serviceInstallRunning.Store(false)
		}
	}()

	executable, err := desktopExecutable()
	if err != nil {
		reportServiceLaunchFailure(serviceActionInstall, err)
		return false
	}

	console, err := findSiblingExecutable(filepath.Dir(executable), executable)
	if err != nil {
		desktopLog.Warn("the console program of the service is missing",
			"folder", filepath.Dir(executable), "error", err)
		showMessage(serviceSiblingMissingCaption,
			serviceSiblingMissingText(filepath.Dir(executable), err),
			windows.MB_ICONERROR)
		return false
	}

	if !confirmServiceInstall(installServiceText(serviceDataDir())) {
		desktopLog.Info("the service installation was not confirmed")
		return false
	}

	pid, err := launchServiceHelper(serviceActionInstall, os.Getpid())
	if err != nil {
		reportServiceLaunchFailure(serviceActionInstall, err)
		return false
	}
	handedOver = true
	desktopLog.Info("the elevated service installation is running",
		"helperPID", pid, "console", console)
	return true
}

// serviceDataDir returns the data directory the service owns.
//
// An existing registration decides, because an installation merges with it; a
// machine without a registration gets the default state directory of the scope.
// It is a machine-wide directory, never the profile of the user who installs
// it, so the confirmation names the directory the service will really own.
func serviceDataDir() string {
	ctx, cancel := context.WithTimeout(context.Background(), serviceStatusTimeout)
	defer cancel()
	if status, err := serviceStatus(ctx); err == nil && status.Paths.StateDir != "" {
		return status.Paths.StateDir
	}

	paths, err := service.DefaultPaths(service.ScopeSystem)
	if err != nil {
		desktopLog.Warn("could not resolve the data directory of the service", "error", err)
		return ""
	}
	return paths.StateDir
}

// desktopExecutable returns the absolute path of the running desktop program.
func desktopExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("the path of the desktop program cannot be read: %w", err)
	}
	return path, nil
}

// launchServiceHelper starts the elevated helper of the desktop host.
//
// The helper is this very program, started with administrator rights: the
// desktop never implements a second service client, and the helper runs the
// same actions the user would run from a console. The process id of the helper
// is returned, so the caller can record which helper performs the action.
func launchServiceHelper(action serviceAction, waitPID int) (uint32, error) {
	executable, err := desktopExecutable()
	if err != nil {
		return 0, err
	}
	pid, err := launchElevated(elevatedLaunch{
		Executable: executable,
		Action:     action,
		WaitPID:    waitPID,
	})
	if err != nil {
		return 0, err
	}
	desktopLog.Info("the elevated service helper started",
		"action", string(action), "pid", pid)
	return pid, nil
}

// confirmServiceInstall asks the user whether the Windows service is
// installed.
//
// It is a package level variable so the tests can answer the confirmation: the
// real message box blocks a process without an interactive window station.
var confirmServiceInstall = confirmNativeServiceInstall

// The answers a message box reports.
const (
	// messageBoxYes is the id of the Yes button.
	messageBoxYes = 6
	// messageBoxNo is the id of the No button.
	messageBoxNo = 7
	// messageBoxCancel is the id of the Cancel button, and the id a task
	// dialog reports when it was closed with Esc or with the close button.
	messageBoxCancel = 2
)

// confirmNativeServiceInstall shows the confirmation of the service
// installation in a native message box.
//
// No is the default button: the registration is a machine-wide change that
// survives the desktop app, so the answer that changes nothing is the one a
// stray Enter selects.
func confirmNativeServiceInstall(text string) bool {
	textPtr, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return false
	}
	captionPtr, err := windows.UTF16PtrFromString(installServiceCaption)
	if err != nil {
		return false
	}
	result, _ := windows.MessageBox(0, textPtr, captionPtr,
		windows.MB_YESNO|windows.MB_ICONQUESTION|windows.MB_DEFBUTTON2|
			windows.MB_SETFOREGROUND|windows.MB_TOPMOST)
	return result == messageBoxYes
}
