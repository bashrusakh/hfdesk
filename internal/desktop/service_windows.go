// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
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
// The lookup is a local read of the service control manager, so the budget
// only covers a machine that is busy or a service control manager that does
// not answer. The launch waits for it before it decides whether it starts a
// server, so the wait has to be bounded.
const serviceStatusTimeout = 10 * time.Second

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
// The Windows service is a second HFDesk on the same machine, and it binds the
// same address. A launch that finds a registration therefore never starts a
// server of its own: it asks the user what to do and ends, so the two
// instances can never compete for the address.
//
// The gate runs before the application is created. It reports false when the
// launch has to end, and the caller returns before it starts a server, opens a
// tray icon or publishes a state file. The gate reports true only when the
// machine has no registration, which is the launch the desktop host had before
// the service existed.
//
// A registration that cannot be read is logged and treated like a machine
// without one: a service control manager that does not answer has no running
// instance to compete with, and the desktop host must not refuse to start
// because it could not look.
func serviceGate() bool {
	ctx, cancel := context.WithTimeout(context.Background(), serviceStatusTimeout)
	defer cancel()

	status, err := serviceStatus(ctx)
	if err != nil {
		desktopLog.Warn("could not read the Windows service registration", "error", err)
		return true
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
// desktop app: the service starts at boot and keeps running after the window
// closes, and its data is not the data of the desktop.
//
// The console program of the service is verified before the user is asked: the
// helper re-registers the service from its own folder, and a folder without the
// program would be reported only after the user answered the elevation prompt
// and the desktop had already begun to quit. The missing program is therefore
// reported instead of a confirmation that cannot lead to a working
// installation.
//
// The helper is launched elevated; it waits for this process before it starts
// the service, so the desktop can stop its own server through the graceful
// path first. The action reports true when the host has to quit, which is the
// case after a successful launch and never after a failure: a refused request
// for administrator rights leaves the running desktop untouched.
//
// A click that arrives while an installation action is still running reports
// false without a second confirmation and without a second prompt for
// administrator rights: only one elevated helper is ever launched, and the tray
// item works again as soon as an action ended without a hand-over.
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
// An existing registration decides: an installation merges with it, so the
// service keeps the state directory it already runs with. A machine without a
// registration gets the default directory of the scope, which is the directory
// the installation will create.
//
// The directory is a fact of the service layer: the machine-wide instance keeps
// its state under the machine-wide directories of the platform, never in the
// profile of the user who installs it. The confirmation names the directory, so
// the user learns where the data of the service lives instead of reading a path
// from the documentation that the installation does not use.
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
