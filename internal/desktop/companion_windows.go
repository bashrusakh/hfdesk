// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
	"github.com/energye/systray"
	"golang.org/x/sys/windows"
)

// companionTrayTooltip is the tooltip of the tray icon of a tray companion.
const companionTrayTooltip = "HFDesk - tray companion"

// companionWindowUnavailableCaption is the caption of the report that the
// Runtime / Logs window of the tray companion cannot be shown because the
// WebView2 runtime is missing — the same caption the desktop host reports
// it under, so the user reads one title for one problem.
const companionWindowUnavailableCaption = "HFDesk - the Runtime / Logs window is not available"

// companionRelaunchBudget bounds the wait of the relaunched companion for
// the single instance lock of the session.
//
// The budget has to outlast the elevated helper's own action budget
// (serviceActionBudget, two minutes): the helper holds the lock from its
// ready signal through the whole installation and releases it right before
// its final report, so the child acquires shortly after the installation
// settled. The margin covers the handover's own tail. The wait is the
// WINDOW of the relaunch only — an ordinary launch never waits
// (CompanionWait is zero), and a holder that outlives this budget ends the
// relaunch with the honest report of a session that is still held.
var companionRelaunchBudget = 150 * time.Second

// startCompanionRelaunch starts the tray-companion relaunch after a
// successful installation handover.
//
// It is a package level variable so the tests can prove WHEN — and
// whether — the relaunch runs, without spawning a real process.
var startCompanionRelaunch = launchCompanionRelaunch

// finishInstallHandover ends the committed installation handover: the
// failure of the authorization is reported exactly like every other
// handover failure, and a settled authorization relaunches this host as a
// tray companion.
//
// The relaunch runs HERE — right after the success event was sent and the
// helper acknowledged it — for three reasons the protocol fixes:
//
//   - from this point on the helper waits ONLY for the exit of this
//     process before it installs (waitForHandoverAuthorization, second
//     phase), so this process must exit promptly: a blocking report in
//     this path could outlive that exit budget and abort the
//     installation itself;
//   - the helper still holds the single instance lock, so a child started
//     now waits for the exact moment the installation released it — the
//     companion appears with the registration the installation created;
//   - an authorization that FAILED never spawns: the installation does
//     not run, and a companion beside a machine without the registration
//     the user asked for would be a claim the outcome does not support.
func finishInstallHandover(err error) {
	if err != nil {
		reportServiceHandoverFailure(err)
		return
	}
	startCompanionRelaunch()
}

// launchCompanionRelaunch starts a detached copy of this very executable
// as a tray companion.
//
// The child is spawned by THIS departing host, never by the elevated
// helper: it must run under the SAME Windows identity as this session —
// the same user token, the same configuration directory, the same
// per-session singleton name. A child of the elevated helper would inherit
// the elevated token, and an elevation approved by ANOTHER administrator
// would even run the companion under that approver's profile and session
// identity — the wrong user entirely (the same fact the helper's own
// --desktop-state flag exists for). The departing host holds the right
// identity, and the child it starts survives its exit — the handover
// protocol requires this process to exit, so the restart IS this exit plus
// the child.
//
// A failure of the spawn is recorded in the log of this launch — the very
// log the helper's reports name — and never shown through a blocking box
// here: this path runs after the authorization was sent, the helper waits
// for THIS process to exit, and a box the user reads for a minute could
// exceed that exit budget and abort the installation. The installation
// outcome stays truthful either way; the user starts the desktop app
// manually when the relaunch record says so.
func launchCompanionRelaunch() {
	executable, err := desktopExecutable()
	if err != nil {
		desktopLog.Error("the tray companion relaunch could not be built: start the desktop app manually",
			"error", err)
		return
	}
	args := companionRelaunchArgs(os.Args[1:], companionRelaunchBudget)
	command := exec.Command(executable, args...)
	if err := command.Start(); err != nil {
		desktopLog.Error("the tray companion relaunch could not be started: start the desktop app manually",
			"error", err)
		return
	}
	desktopLog.Info("the tray companion relaunch was started",
		"pid", command.Process.Pid,
		"waitBudget", companionRelaunchBudget.String())
	// The child owns its process from here; nothing of this exiting
	// launch stays open in it. The handle is released — on Windows a
	// released process handle leaves the child running, and the child
	// waits for the locks of this process and of the helper on its own.
	_ = command.Process.Release()
}

// runCompanion runs the desktop host as a tray companion.
//
// THE INVARIANT: a companion never listens. It creates no application —
// and therefore no server, no listener and no runtime-API endpoint of its
// own — and it publishes no claim, because a claim says that this session
// SERVES an address and the companion serves nothing. The seam test of
// this function pins both facts (the application seam and the publish seam
// fail the test if the path reaches them). Two instances therefore never
// serve one address: the service serves its registered address alone, and
// the companion binds none.
//
// What the companion owns instead:
//
//   - the single instance lock of the session (lock): it IS this
//     session's desktop instance, so no second desktop host can start
//     beside it, and the service mutations of its menu hold that very
//     object through their window (serviceActionHoldsSingleton). Because
//     no claim is published, the mutation guard classifies it correctly:
//     the singleton probe finds the holder, the claim read finds none, so
//     Start/Update are NOT refused as "beside a serving desktop" — the
//     companion serves nothing and may start the service. The helper
//     decides the same way from the same two facts.
//   - the tray with the strictly decided composition (trayMenuCompanion):
//     the status line, the service group — the install item beside a
//     proven absence of the registration, the four lifecycle actions
//     beside a proven one (the absence rule of apply) — and the Runtime
//     / Logs window item; every action runs through the SAME flow as
//     the desktop host (the single service-action guard, the
//     confirmations with No as default, the administrator prompt, the
//     menu catch-up), and the installation commits its handover to this
//     host's shutdown like the desktop host's installation does;
//   - the window in document mode: honest service state plus the address
//     the registration names, rendered from a read under the display
//     policy — no listener anywhere.
//
// A companion never writes the claim file of this session; the panic
// guard of run() removes it on a panic like every launch's guard does.
// guard is the panic guard of this launch, and endWithoutTray ends a host
// whose icon in the notification area could not come up; shutdownSink
// receives the companion's shutdown — close the window — so the panic
// guard of run() stops this host through the normal path too.
func runCompanion(lock *instanceLock, guard panicGuard,
	endWithoutTray func(), shutdownSink *func(),
) error {
	// The signal ends the whole companion, exactly like the desktop
	// host: the window is closed and the tray loop ends through the same
	// quit path the menu would use.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// The window exists before any goroutine that can reach it starts —
	// the same ordering contract as the desktop host (see run()).
	var window *runtimeWindow

	// The shutdown of a companion stops no server: there is none. It
	// completes a committed installation handover first — the companion
	// installs the service through the very protocol of the desktop host
	// (the helper waits for THIS process to exit, and a settled
	// authorization relaunches the companion through the child), with
	// nothing to drain or persist: there is no server and no job state
	// of its own — and then closes the Runtime / Logs window. The quit
	// path ends the tray loop after the shutdown — the invariant "the
	// tray never outlives what it serves" holds for the same reason,
	// trivially: a companion serves nothing.
	var coordinator desktopShutdownCoordinator
	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			if handover, committed := coordinator.beginShutdown(); committed {
				completeInstallHandover(handover, func() error {
					// The companion has no server, so there is
					// nothing to stop and nothing to persist:
					// the completion evidence is complete the
					// moment this function runs.
					return nil
				})
			}
			if window != nil {
				window.Close()
			}
		})
	}
	*shutdownSink = shutdown

	// The quit ends the companion as one unit, on the same endHost the
	// desktop host uses.
	ready := make(chan struct{})
	requestQuit := func() {
		go guard.runBackground(func() {
			endHost(shutdown, ready, systray.Quit)
		})
	}

	// The menu catch-up of THIS host — the same single signal the
	// desktop host assigns before any menu item exists. The service
	// actions and the window's display reads post to it, so a result
	// read for either surface can never stay invisible in the menu until
	// the next tick.
	serviceRefresh := make(chan struct{}, 1)
	requestServiceMenuRefresh := func() {
		select {
		case serviceRefresh <- struct{}{}:
		default:
		}
	}
	serviceMenuRefresh = requestServiceMenuRefresh

	// The install item of the companion runs the very flow the desktop
	// host's install item runs — runInstallServiceAction: the missing-
	// console settlement, the confirmation, the elevated helper, the
	// handover, the commit to the shutdown and the reports — through the
	// companion's own coordinator and quit above. The commit hands the
	// handover to THIS host's shutdown, and the settled authorization
	// relaunches the companion through the child of the established
	// relaunch mechanism: after the installation settles, the fresh
	// companion reads the registration it just got (or, on a failed
	// installation, the proven absence — and offers this very item
	// again, so a relaunch on a machine without a registration is no
	// dead end).
	dispatchInstallService := func() {
		go guard.runBackground(func() {
			runInstallServiceAction(lock.name, requestServiceMenuRefresh,
				coordinator.commit, requestQuit)
		})
	}

	iconBytes, err := productIconBytes()
	if err != nil {
		desktopLog.Warn("could not build the product icon", "error", err)
	}

	window = newCompanionWindow(companionWindowPage, iconBytes)
	webview2Ready, webview2Version := webview2Available()
	if !webview2Ready {
		desktopLog.Warn("the WebView2 runtime is not installed", "mode", "tray companion")
		showMessage(companionWindowUnavailableCaption, companionWebView2MissingText(), windows.MB_ICONWARNING)
	} else {
		desktopLog.Info("the WebView2 runtime is available", "version", webview2Version)
	}
	desktopLog.Info("the tray companion started",
		"logFile", runLogPath,
		"listen", "none: the companion never binds an address")

	// A signal ends the companion as one unit — the same end path as the
	// desktop host uses for its signal.
	go guard.runBackground(func() {
		endHostOnSignal(signalCtx, requestQuit)
	})

	go guard.runBackground(func() {
		select {
		case <-ready:
		case <-time.After(trayStartupTimeout):
			// Without the icon the companion would be invisible, so
			// it stops: the refusal is recorded first — this is the
			// only surface it reaches — so the log the window names
			// really carries it, and the report carries the
			// standard line that makes that log reachable.
			desktopLog.Error("the tray companion could not create its icon in the notification area and stops")
			showMessage("HFDesk could not start",
				trayStartupFailureText(),
				windows.MB_ICONERROR)
			endWithoutTray()
		}
	})

	// The tray loop runs on the main thread until the signal ends this
	// companion. The menu is the strictly decided composition: the status
	// line, the service group — the install item beside a proven absence
	// of the registration, the four lifecycle actions beside a proven
	// registration (the absence rule of apply) — and the Runtime / Logs
	// window item, every action wired to the very same flow the desktop
	// host runs (the single guard, the confirmations, the elevation, the
	// catch-up). The open, registered-service-dialog and quit ITEMS do
	// not exist; quit exists only as the internal path above, until the
	// user decides on an exit item.
	runTrayLoop(iconBytes, companionTrayTooltip, trayMenuCompanion, trayActions{
		install: dispatchInstallService,
		runtime: func() {
			go guard.runBackground(func() {
				showCompanionRuntimeWindow(window, webview2Ready)
			})
		},
		start: func() {
			go guard.runBackground(func() {
				performServiceActionFromTray(serviceActionStart, lock.name)
			})
		},
		stop: func() {
			go guard.runBackground(func() {
				performServiceActionFromTray(serviceActionStop, lock.name)
			})
		},
		update: func() {
			go guard.runBackground(func() {
				performServiceActionFromTray(serviceActionUpdate, lock.name)
			})
		},
		delete: func() {
			go guard.runBackground(func() {
				performServiceActionFromTray(serviceActionUninstall, lock.name)
			})
		},
	}, trayServiceView{
		read:    serviceState.readForDisplay,
		refresh: serviceRefresh,
	}, ready)

	// The tray loop ended (a signal, or a loop that died): the shutdown
	// completes a committed installation handover if one is running and
	// closes the window — the same path the quit and the panic guard
	// take. There is no server to stop — there never was one.
	shutdown()
	return nil
}

// showCompanionRuntimeWindow shows the Runtime / Logs window of the tray
// companion.
//
// When the WebView2 runtime is missing, the companion's own actionable
// report is shown again instead of failing silently; a window that cannot
// be shown reports through the established text, which carries the log-path
// line like every failure window of a launch that has a log.
func showCompanionRuntimeWindow(window *runtimeWindow, webview2Ready bool) {
	if !webview2Ready {
		showMessage(companionWindowUnavailableCaption, companionWebView2MissingText(), windows.MB_ICONWARNING)
		return
	}
	if err := window.Show(); err != nil {
		desktopLog.Error("could not show the runtime window", "error", err)
		showMessage("HFDesk - the Runtime / Logs window could not be shown",
			runtimeWindowShowFailedText(err),
			windows.MB_ICONERROR)
	}
}

// companionWindowPage reads the registration for the Runtime / Logs window
// of the tray companion and builds its page.
//
// The read runs under the display policy of the one owner of every state
// read (serviceState.readForDisplay — the very reader the menu line and
// the runtime surface read through), and it ends with the menu catch-up
// request exactly like every other display read: a result read for the
// window must never stay invisible in the menu until the next tick, and a
// failed read — the honest unknown state — asks for the catch-up as well.
//
// The page reports the honest state of the service and the address the
// REGISTRATION names: the link is offered by the fact of the registration,
// so it is there while the service is stopped too (it answers once the
// service runs), and a machine without a registration gets no invented
// address. The log path of this launch completes the page — the standard
// line of every surface of a launch that has a log — and the service log
// section stays out of it until its source is decided.
func companionWindowPage() string {
	status, err := serviceState.readForDisplay()
	requestServiceMenuCatchUp()

	state := companionPageState{LogPath: runLogPath}
	switch {
	case err != nil:
		state.State = "The state of the Windows service could not be read."
	case status.State == service.StateNotInstalled:
		state.State = "HFDesk is not registered as a Windows service."
	case status.State == "":
		state.State = "The state of the Windows service is unknown."
	default:
		prompt := serviceChoicePrompt{State: status.State, Running: status.Running}
		state.State = "The HFDesk service is " + serviceGateStatePhrase(prompt) + "."
		if url, urlErr := serviceURL(status.Listen); urlErr == nil {
			state.WebURL = url
		} else {
			state.Address = strings.TrimSpace(serviceAddressText(status.Listen))
		}
	}
	return companionPage(state)
}
