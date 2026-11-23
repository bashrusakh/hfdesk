// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bashrusakh/hfdesk/internal/opener"
	"github.com/bashrusakh/hfdesk/internal/service"
	"github.com/energye/systray"
	"golang.org/x/sys/windows"
)

// companionTrayTooltip is the tooltip of the tray icon of a tray companion.
const companionTrayTooltip = "HFDesk - tray companion"

// companionWindowUnavailableCaption is the caption of the report that the
// Logs window of the tray companion cannot be shown because the WebView2
// runtime is missing — the same caption the desktop host reports it
// under, so the user reads one title for one problem.
const companionWindowUnavailableCaption = "HFDesk - the Logs window is not available"

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

// endCompanionTray ends the notification-area loop of the companion — the
// last step of the established end path (endHost) that both the Exit item
// and a signal take, so the two can never drift apart.
//
// It is a package level variable so the tests can prove an Exit really
// reaches that end path without ending a notification-area host this
// process never started (the tray library is a process wide singleton
// whose quit is a one-shot).
var endCompanionTray = systray.Quit

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
//   - the tray with the decided composition (trayMenuCompanion): the
//     status line, Open HFDesk — the address of the REGISTRATION in the
//     default browser, visible beside a proven registration and enabled
//     while the service serves the address — the service group — the
//     install item beside a proven absence of the registration, the four
//     lifecycle actions beside a proven one (the absence rule of apply)
//     — the Logs window item, and the separator with Exit as
//     the last item (Exit ends THIS companion through the normal path
//     and touches no service, the way Quit closes the desktop host);
//     every action runs through the SAME flow as the desktop host (the
//     single service-action guard, the confirmations with No as
//     default, the administrator prompt, the menu catch-up), and the
//     installation commits its handover to this host's shutdown like
//     the desktop host's installation does;
//   - the ONE Logs window the desktop host also opens: on every open it
//     resolves the address of the REGISTRATION and — when that address
//     answers — loads the live runtime page of the SERVICE through a
//     plain outgoing navigation of the WebView2 (the same log, filters
//     and search the desktop window shows). When the address does not
//     answer it shows the local fallback document instead: the honest
//     state of the service, the address that did not answer, the path of
//     the service log with the one open-the-file action, and the retry —
//     a document that fetches and listens to nothing, rendered from
//     reads under the display policy. The window host binds the open
//     actions, so they run in THIS process — the user's session — and
//     not in the service, which runs in session 0 and could not open a
//     window or a program on the user's desktop.
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
	// Every helper this host launches is launched by a tray companion, so
	// its reports name this companion's own Start service item as the way
	// back. The surface is assigned before the first service action can be
	// wired (the tray below), and it travels to the helper beside the log
	// path (launchServiceHelper).
	hostLaunchSurface = launchSurfaceCompanion

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
	// of its own — and then closes the Logs window. The quit
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
	// desktop host uses: the shutdown first (an installation handover it
	// must still complete, and the window), then the end of the
	// notification-area loop. The Windows service is not part of this
	// path — the companion owns no server and stops no service.
	ready := make(chan struct{})
	requestQuit := func() {
		go guard.runBackground(func() {
			endHost(shutdown, ready, endCompanionTray)
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

	// The WebView2 readiness decides before the window exists, so the
	// window actions can report through the established report of this
	// launch when the window cannot be shown at all.
	webview2Ready, webview2Version := webview2Available()

	// The window binds every host action before any document loads: the
	// live runtime page of the service and the local fallback document
	// both call the same fixed names, and the retry action re-runs the
	// very show the tray item runs — the getters read the window (and
	// its readiness) at click time, the way the tray wiring beside it
	// captures them.
	windowActions := companionWindowActions(guard,
		func() *runtimeWindow { return window },
		func() bool { return webview2Ready })
	window = newCompanionWindow(companionRuntimePageURL, companionWindowPage, iconBytes, windowActions)
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

	// The tray loop runs on the main thread until the signal or the Exit
	// item ends this companion. The menu is the decided composition: the
	// status line, Open HFDesk (the address of the registration), the
	// service group — the install item beside a proven absence of the
	// registration, the four lifecycle actions beside a proven
	// registration (the absence rule of apply) — the Logs
	// window item, the separator and Exit, every action wired to the
	// very same flow the desktop host runs (the single guard, the
	// confirmations, the elevation, the catch-up). Exit runs the end
	// path above: it closes this companion and touches no service. The
	// registered-service-dialog item does not exist — that dialog
	// belongs to the desktop host.
	runTrayLoop(iconBytes, companionTrayTooltip, trayMenuCompanion, trayActions{
		open: func() {
			// The address comes from the REGISTRATION (the very
			// source the window link uses), read again on this
			// click, so the browser never opens an address this
			// host invented. The read is bounded by the display
			// budget, so it runs on a goroutine like every other
			// action that can hold the message loop.
			go guard.runBackground(openCompanionServiceURL)
		},
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
		quit: requestQuit,
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

// showCompanionRuntimeWindow shows the Logs window of the tray companion.
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
		desktopLog.Error("could not show the logs window", "error", err)
		showMessage("HFDesk - the Logs window could not be shown",
			runtimeWindowShowFailedText(err),
			windows.MB_ICONERROR)
	}
}

// companionRuntimePageURL resolves the runtime page of the REGISTERED
// service address — the very address Open HFDesk reads — for one open of
// the Logs window.
//
// The read runs under the display policy of the one owner of every state
// read (serviceState.readForDisplay — the very reader the menu line and
// the fallback document read through), and it ends with the menu
// catch-up request exactly like every other display read: a result read
// for the window must never stay invisible in the menu until the next
// tick, and a failed read — the honest unknown state — asks for the
// catch-up as well.
//
// It returns "" — no address to navigate to — beside a failed read, a
// machine without a registration, and an address that cannot be read as a
// URL: the window then shows its fallback document instead of inventing
// an address. A registered address that simply does not answer is NOT
// decided here: the window probes it and decides from the answer.
func companionRuntimePageURL() string {
	status, err := serviceState.readForDisplay()
	requestServiceMenuCatchUp()
	if err != nil {
		return ""
	}
	url, urlErr := serviceURL(status.Listen)
	if urlErr != nil {
		return ""
	}
	return runtimePageURL(url)
}

// companionWindowPage builds the fallback document of the Logs window:
// the state the window shows when the registered address does not answer
// (or names none), in the visual language of the runtime page the live
// window would have loaded.
//
// The read runs under the display policy of the one owner of every state
// read (serviceState.readForDisplay), and it ends with the menu
// catch-up request exactly like every other display read: a result read
// for the window must never stay invisible in the menu until the next
// tick, and a failed read — the honest unknown state — asks for the
// catch-up as well.
//
// The document states the honest state of the service, the address that
// did not answer as a FACT — never a link: the address just proved it
// does not answer, so a link would be a dead path — and the honest reason
// when there is no address to name. The path of the service log file
// completes it: the very file the open action opens, resolved fresh on
// every render (a second display read with its own catch-up), so the path
// the document shows and the file the action hands to the default handler
// are always one and the same file.
func companionWindowPage() string {
	status, err := serviceState.readForDisplay()
	requestServiceMenuCatchUp()

	state := logsFallbackState{LogPath: companionServiceLogPath()}
	switch {
	case err != nil:
		state.State = "The state of the Windows service could not be read."
		state.Note = "The address of the web UI could not be read."
	case status.State == service.StateNotInstalled:
		state.State = "HFDesk is not registered as a Windows service."
		state.Note = "The registration names no address, so there is nothing to open."
	case status.State == "":
		state.State = "The state of the Windows service is unknown."
		state.Note = "The address of the web UI is not known."
	default:
		prompt := serviceChoicePrompt{State: status.State, Running: status.Running}
		state.State = "The HFDesk service is " + serviceGateStatePhrase(prompt) + "."
		if url, urlErr := serviceURL(status.Listen); urlErr == nil {
			state.Address = url
		} else if raw := strings.TrimSpace(status.Listen); raw != "" {
			// The registration has an address that cannot be read as
			// a URL: naming the address itself is still the honest
			// fact beside a window that did not reach it.
			state.Address = raw
		} else {
			state.Note = "The registration names no address, so there is nothing to open."
		}
	}
	return logsFallbackPage(state)
}

// companionServiceLogPath resolves the path of the service log file for
// this click or render: the log directory the REGISTRATION decides — the
// same source the page section was built from, so the open-full-file action
// and the tail the page shows speak about the same file — and a machine
// without a readable registration falls back to the default of the
// machine-wide scope.
//
// The read is a display read like every other one of this host, so it asks
// the menu to catch up right after it ran — a failed read included — and a
// result read can never stay invisible in the menu until the next tick.
func companionServiceLogPath() string {
	status, err := serviceState.readForDisplay()
	requestServiceMenuCatchUp()
	registered := ""
	if err == nil && status.Paths.LogDir != "" {
		registered = status.Paths.LogDir
	}
	return serviceLogPath(registered)
}

// companionWindowActions returns the host handlers the document of the
// companion window can call by name — open the full service log file
// (openLogFileAction), open the folder of that log (openLogFolderAction),
// open the web UI in the default browser (openUiAction — the very
// address the link of the old static window carried, re-read from the
// registration on the click) and retry the live page (logsTryAgainAction).
//
// The handlers run on a goroutine of its own under the panic guard, like
// every other action of this host: each one resolves its target on the
// host side — in THIS process, the user's session, never the server of
// the service, which runs in session 0 and cannot open a file, a folder,
// a browser or a window on the user's desktop — and the page carries no
// path and no logic beyond naming these fixed actions. The retry re-runs
// the show the tray item runs, through the getters the caller passes: the
// window itself exists only after this function returned.
func companionWindowActions(guard panicGuard, current func() *runtimeWindow,
	webview2Ready func() bool) map[string]func() {
	return map[string]func(){
		openLogFileAction: func() {
			go guard.runBackground(openCompanionServiceLog)
		},
		openLogFolderAction: func() {
			go guard.runBackground(openCompanionServiceLogFolder)
		},
		openUiAction: func() {
			go guard.runBackground(openCompanionServiceURL)
		},
		logsTryAgainAction: func() {
			go guard.runBackground(func() {
				if window := current(); window != nil {
					showCompanionRuntimeWindow(window, webview2Ready())
				}
			})
		},
	}
}

// openCompanionServiceLog opens the full service log file with the default
// handler of Windows — Notepad for a .log file with the standard
// association — so the whole file is readable beside the path the fallback
// document states. It runs in the process of the window, in the user's
// session.
//
// The honest states end with their own report: no resolvable path, a file
// the service has not created yet, and a file the default handler refused
// to open. Every report carries the standard log-path line of this launch
// (the failure window rule), and the refusal names the service log path
// itself.
func openCompanionServiceLog() {
	path := companionServiceLogPath()
	if path == "" {
		desktopLog.Info("the companion did not open the service log: no log path could be resolved")
		showMessage(serviceLogOpenFailedCaption, noLogFileText(), windows.MB_ICONWARNING)
		return
	}
	if _, err := os.Stat(path); err != nil && os.IsNotExist(err) {
		desktopLog.Info("the companion did not open the service log: the file does not exist yet",
			"logFile", path)
		showMessage(serviceLogOpenFailedCaption, serviceLogMissingText(path), windows.MB_ICONWARNING)
		return
	}
	if err := openExternalFile(path); err != nil {
		desktopLog.Error("the companion could not open the service log",
			"logFile", path, "error", err)
		showMessage(serviceLogOpenFailedCaption, serviceLogOpenFailedText(path, err), windows.MB_ICONERROR)
	}
}

// openCompanionServiceLogFolder opens the folder of the service log file
// with the default file manager — in the process of the window, in the
// user's session: the server of the service runs in session 0 and could
// not open a folder on the user's desktop from there (the very reason the
// window host owns these actions).
//
// The honest states end with their own report: a log path that could not
// be resolved, a folder the service has not created yet, and an opener
// that refused the folder. Every report carries the standard log-path
// line of this launch, and the folder reports name the folder itself.
func openCompanionServiceLogFolder() {
	path := companionServiceLogPath()
	if path == "" {
		cause := errors.New("the log folder of the service could not be resolved")
		desktopLog.Error("the companion could not resolve the service log folder", "error", cause)
		showMessage(logFolderOpenFailedCaption, logFolderOpenFailedText("", cause), windows.MB_ICONERROR)
		return
	}
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); err != nil && os.IsNotExist(err) {
		desktopLog.Info("the companion did not open the service log folder: the folder does not exist yet",
			"dir", dir)
		showMessage(logFolderOpenFailedCaption, logFolderMissingText(dir), windows.MB_ICONWARNING)
		return
	}
	if err := openExternalFolder(dir); err != nil {
		desktopLog.Error("the companion could not open the service log folder",
			"dir", dir, "error", err)
		showMessage(logFolderOpenFailedCaption, logFolderOpenFailedText(dir, err), windows.MB_ICONERROR)
	}
}

// openExternalFile opens a file with the default handler of the operating
// system.
//
// It is a package level variable so the tests can prove WHICH path the
// open-the-log-file action hands over, without starting a real handler on
// the machine.
var openExternalFile = openFileWithDefaultHandler

// openExternalFolder opens a folder with the default file manager of the
// operating system — the same opener the runtime endpoint of the server
// uses, so every caller reaches the file manager the same way.
//
// It is a package level variable so the tests can prove WHICH folder the
// open-the-log-folder action hands over, without starting a real file
// manager on the machine.
var openExternalFolder = opener.Open

// openFileWithDefaultHandler hands the file to the shell association of
// Windows: the verb "open" runs the program registered for the file's
// extension — Notepad for a .log file with the standard association, or
// whatever the user chose instead. The process is started, not waited for,
// so the thread that runs this action is never held by it.
//
// The opener of internal/opener is deliberately not reused here: on
// Windows it opens the CONTAINING FOLDER and selects the file (its job is
// "show me where this lives"), while this action must open the FILE ITSELF
// in its application.
func openFileWithDefaultHandler(path string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return fmt.Errorf("the default handler verb for %s cannot be built: %w", path, err)
	}
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("the path %s cannot be handed to the default handler: %w", path, err)
	}
	if err := windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL); err != nil {
		return fmt.Errorf("the default handler did not open %s: %w", path, err)
	}
	return nil
}

// openCompanionServiceURL opens the web UI address of the REGISTRATION in
// the default browser — the very address the link of the window carries,
// read again on this click, so the two cannot drift apart and a browser
// never opens an address this host invented.
//
// Every way it can end is reported through the established reports: a
// registration that cannot be read gets the gate's unreadable notice (the
// state the user has to know before anything else), a registration the
// machine no longer has and an address that cannot be read as a URL reach
// the one browser-failure report every opener of a browser uses, and a
// browser that did not start reports through that same report.
func openCompanionServiceURL() {
	status, err := serviceState.readForDisplay()
	// Like every display read beside the menu itself (readRuntimeServiceState
	// and the window page), it asks the menu to catch up right after it
	// ran: a result read for this surface must never stay invisible in
	// the menu until the next tick — a failed read included.
	requestServiceMenuCatchUp()
	if err != nil {
		desktopLog.Error("the tray companion could not read the Windows service registration for Open HFDesk",
			"error", err)
		showMessage(serviceGateUnreadableCaption,
			serviceGateUnreadableText(err), windows.MB_ICONERROR)
		return
	}
	switch status.State {
	case "", service.StateUnknown, service.StateNotInstalled:
		// The menu showed the item for a registration the machine no
		// longer has (its line catches up with the next refresh): the
		// report names that fact instead of inventing an address.
		reportBrowserFailure(status.Listen,
			errors.New("the Windows service is not registered, so there is no address to open"))
		return
	}
	url, err := serviceURL(status.Listen)
	if err != nil {
		reportBrowserFailure(status.Listen, err)
		return
	}
	if err := openBrowser(url); err != nil {
		reportBrowserFailure(url, err)
	}
}
