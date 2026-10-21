// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bashrusakh/hfdesk/internal/app"
	"github.com/bashrusakh/hfdesk/internal/logging"
	"github.com/energye/systray"
	"golang.org/x/sys/windows"
)

// desktopLog is the logger of the desktop host.
var desktopLog = logging.NewComponentLogger("desktop")

// trayStartupTimeout bounds the wait for the tray.
//
// The tray reports that it is ready through its ready callback. Without a
// tray the host would be invisible, so the wait ends in a message box and a
// non-zero exit instead of a silent process.
const trayStartupTimeout = 20 * time.Second

// gracefulShutdownTimeout bounds the wait for the graceful shutdown of the
// server.
//
// The shutdown itself is bounded: the HTTP drain and the close of the job
// manager each get their own budget. This budget only covers the case where
// the server does not stop at all, so a stuck server cannot keep the host
// from ending.
const gracefulShutdownTimeout = 30 * time.Second

// Run starts the desktop host and blocks until the user quits it.
//
// The sequence is the one of the shared application layer: the logging
// pipeline is installed, the single instance lock is taken, the server is
// started in-process and the system tray takes over the main thread.
//
// Run returns nil after a normal quit and after a launch that found a
// running instance. It returns an error after a startup failure, and every
// startup failure is shown to the user in a native message box.
func Run(o Options) error {
	logPath := o.App.LogFile
	if logPath == "" {
		logPath = app.DefaultLogPath()
	}
	return run(o, logPath)
}

// run performs the desktop startup.
//
// A panic of the host is caught here: the desktop host has no console, so a
// panic that is not caught ends the process without an explanation and
// without a log record. The panic is recorded in the log file, shown to the
// user in a native message box and ends the host through the returned error,
// so the process still ends.
func run(o Options, logPath string) (err error) {
	var pipeline *logging.Pipeline
	var shutdown func()
	var statePath string

	// The guard ends the host after a panic: it removes the published
	// state, stops the server through the normal path and closes the log
	// pipeline.
	guard := panicGuard{
		logPath: logPath,
		end: func() {
			removePublishedState(statePath)
			if shutdown != nil {
				stopServer(shutdown)
			}
			closePipeline(pipeline)
		},
	}

	// The panic handler is registered before the startup runs, so a panic
	// of the startup is caught as well. The other deferred cleanups run
	// before it (the defers run in reverse order of their registration),
	// so the handler closes the log pipeline itself and the panic still
	// reaches the log file.
	defer func() {
		recovered := recover()
		if recovered == nil {
			closePipeline(pipeline)
			return
		}
		detail := guard.report(recovered)
		guard.end()
		err = fmt.Errorf("desktop: the desktop host stopped after a panic: %s", detail)
	}()

	pipeline, err = setupLogging(o.App.LogLevel, logPath)
	if err != nil {
		return err
	}

	// The GUI subsystem is a build time property of the linker. A binary
	// that was built without it opens a console window next to the
	// notification area icon, so the user is told how to rebuild it. The
	// check only reports the problem: the host starts either way.
	checkSubsystem()

	// The third party libraries report their problems through the
	// standard library logger, and one of them ends the process on its
	// own. A GUI process has no console, so such a message would vanish.
	// The standard logger is routed to a writer that shows the message in
	// a native message box and records it in the log file.
	restoreLogger := installStandardLogger()
	defer restoreLogger()

	lock, err := acquireInstanceLock()
	if err != nil {
		reportStartupFailure("HFDesk could not start",
			fmt.Sprintf("HFDesk could not create its single instance lock.\n\n%v", err), logPath)
		return err
	}
	defer lock.Release()

	if lock.Exists() {
		// Another instance holds the lock: the lock is the authority on
		// whether HFDesk is already running. This instance starts no
		// server.
		reportAlreadyRunning()
		return nil
	}

	application, err := app.New(o.App, o.Version)
	if err != nil {
		reportStartupFailure("HFDesk could not start",
			fmt.Sprintf("HFDesk could not read its configuration.\n\n%v", err), logPath)
		return err
	}

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()

	if err := application.Start(ctx); err != nil {
		reportStartupFailure("HFDesk could not start",
			fmt.Sprintf("HFDesk could not start its local server.\n\n%v", err), logPath)
		return err
	}

	// The runtime window is created further down; the shutdown closes it
	// when it exists.
	var window *runtimeWindow

	// The shutdown stops the server and waits for the graceful shutdown
	// to finish. It runs once, whoever asks for it.
	//
	// It exists from the moment the server runs, so every path that ends
	// the host while the server is running can stop it through the normal
	// path.
	var shutdownOnce sync.Once
	shutdown = func() {
		shutdownOnce.Do(func() {
			cancel()
			if err := application.Wait(); err != nil {
				desktopLog.Error("the server did not stop cleanly", "error", err)
			}
			// The runtime window is closed after the server stopped, so
			// it never shows a page of a server that is gone.
			if window != nil {
				window.Close()
			}
		})
	}

	url := application.URL()
	desktopLog.Info("the desktop host started", "url", url, "logFile", logPath)

	// The effective URL is published, so a second launch can open the web
	// UI of this instance.
	statePath = publishedStatePath()
	if err := publishState(statePath, url); err != nil {
		desktopLog.Warn("could not publish the effective URL", "path", statePath, "error", err)
	}
	defer removePublishedState(statePath)

	if o.App.ShouldOpenUI() {
		if err := application.OpenBrowser(); err != nil {
			desktopLog.Warn("could not open the web UI", "error", err)
		}
	}

	iconBytes, err := productIconBytes()
	if err != nil {
		desktopLog.Warn("could not build the product icon", "error", err)
	}

	window = newRuntimeWindow(runtimePageURL(url), iconBytes)
	webview2Ready, webview2Version := webview2Available()
	if !webview2Ready {
		desktopLog.Warn("the WebView2 runtime is not installed", "url", url)
		reportWebView2Missing(url)
	} else {
		desktopLog.Info("the WebView2 runtime is available", "version", webview2Version)
	}

	// The quit action hands the shutdown off to a goroutine and returns:
	// the message loop of the tray keeps running until the shutdown has
	// finished, so the process never exits in the middle of it.
	requestQuit := func() {
		go guard.runBackground(func() {
			shutdown()
			systray.Quit()
		})
	}

	ready := make(chan struct{})
	go guard.runBackground(func() {
		select {
		case <-ready:
		case <-time.After(trayStartupTimeout):
			// The tray did not come up: without it the host would be
			// invisible, so the user is told and the host stops.
			showMessage("HFDesk could not start",
				"HFDesk could not create its icon in the notification area.\n\n"+
					"The desktop host needs the icon in the notification area to be reachable, so it stops.",
				windows.MB_ICONERROR)
			removePublishedState(statePath)
			// The server is already serving: it is stopped through the
			// normal path, so the jobs are quiesced, the HTTP drain
			// runs and the job state is flushed. The wait is bounded,
			// so a server that does not stop cannot keep the host from
			// ending.
			stopServer(shutdown)
			_ = pipeline.Close()
			os.Exit(1)
		}
	})

	// The tray loop runs on the main thread until the user quits the host.
	runTray(iconBytes, "HFDesk - running", trayActions{
		open: func() {
			// The URL comes from the application, so it is the very URL
			// the startup opened and the two cannot drift apart.
			if err := application.OpenBrowser(); err != nil {
				desktopLog.Warn("could not open the web UI", "error", err)
			}
		},
		runtime: func() {
			// The window is created on a thread of its own and the
			// creation takes a moment. It runs in a goroutine, so the
			// message loop of the tray keeps running while the window
			// comes up.
			go guard.runBackground(func() {
				showRuntimeWindow(window, url, webview2Ready)
			})
		},
		quit: requestQuit,
	}, ready)

	// The tray loop ended. The server is stopped before the process
	// exits, also when the loop ended for another reason than the quit
	// action.
	shutdown()
	return nil
}

// stopServer runs the graceful shutdown and waits for it with a bound.
//
// The shutdown cancels the context of the application, waits for the server
// to stop and closes the runtime window. The wait is bounded, so a server
// that does not stop cannot keep the host from ending.
//
// The shutdown runs in a goroutine of its own, so the caller waits with the
// bound instead of blocking on the shutdown.
func stopServer(shutdown func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if recovered := recover(); recovered != nil {
				// A shutdown that panics must not take the process
				// with it: the host still has to end.
				desktopLog.Error("the graceful shutdown panicked",
					"panic", fmt.Sprintf("%v", recovered))
			}
		}()
		shutdown()
	}()

	select {
	case <-done:
	case <-time.After(gracefulShutdownTimeout):
		desktopLog.Error("the server did not stop within the shutdown budget",
			"timeout", gracefulShutdownTimeout)
	}
}

// closePipeline closes the log pipeline.
//
// The pipeline is closed by the panic handler of the host, because the
// handler runs after the other deferred cleanups and the panic still has to
// reach the log file. Closing it twice is harmless.
func closePipeline(pipeline *logging.Pipeline) {
	if pipeline == nil {
		return
	}
	_ = pipeline.Close()
}

// setupLogging installs the logging pipeline of the desktop host.
//
// The file sink is always enabled: the desktop host has no console, so the
// log file is the only place the records are written to. The console sink
// is disabled, because a GUI subsystem process has no console at all.
//
// A failure to create the log directory or to open the log file is reported
// to the user: the desktop host depends on the log file for its support
// material.
func setupLogging(level, logPath string) (*logging.Pipeline, error) {
	dir := filepath.Dir(logPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		reportStartupFailure("HFDesk could not start",
			fmt.Sprintf("HFDesk could not create its log directory.\n\n%s\n\n%v", dir, err), "")
		return nil, fmt.Errorf("desktop: create the log directory %s: %w", dir, err)
	}

	pipeline, err := app.SetupLogging(app.LogOptions{
		Level:    level,
		Console:  false,
		FilePath: logPath,
	})
	if err != nil {
		reportStartupFailure("HFDesk could not start",
			fmt.Sprintf("HFDesk could not open its log file.\n\n%s\n\n%v", logPath, err), "")
		return nil, fmt.Errorf("desktop: install the logging pipeline: %w", err)
	}
	return pipeline, nil
}

// reportAlreadyRunning tells the user that HFDesk is already running.
//
// The published URL is only opened when the running instance answers it, so
// a stale state file never sends the browser to a dead URL.
func reportAlreadyRunning() {
	url := publishedURL()
	if url == "" || !serverReachable(url) {
		showMessage("HFDesk is already running",
			"HFDesk is already running.\n\nUse the icon in the notification area to open the Runtime / Logs window.",
			windows.MB_ICONINFORMATION)
		return
	}

	showMessage("HFDesk is already running",
		fmt.Sprintf("HFDesk is already running.\n\nThe running instance serves %s.\n\nThe web UI is opened in your default browser.", url),
		windows.MB_ICONINFORMATION)

	if err := app.OpenBrowser(url); err != nil {
		desktopLog.Warn("could not open the web UI of the running instance", "url", url, "error", err)
	}
}

// showRuntimeWindow shows the Runtime / Logs window.
//
// When the WebView2 runtime is missing, the same actionable message is
// shown again instead of failing silently.
func showRuntimeWindow(window *runtimeWindow, url string, webview2Ready bool) {
	if !webview2Ready {
		reportWebView2Missing(url)
		return
	}
	if err := window.Show(); err != nil {
		desktopLog.Error("could not show the runtime window", "error", err)
		showMessage("HFDesk - the Runtime / Logs window could not be shown",
			fmt.Sprintf("The Runtime / Logs window could not be shown.\n\n%v", err),
			windows.MB_ICONERROR)
	}
}

// reportWebView2Missing tells the user that the Runtime / Logs window is
// not available.
//
// The host keeps running: the server and the tray keep working, and the
// action shows the same message again.
func reportWebView2Missing(url string) {
	showMessage("HFDesk - the Runtime / Logs window is not available",
		"The Microsoft Edge WebView2 runtime is not installed, so the Runtime / Logs window cannot be shown.\n\n"+
			"Install the Evergreen runtime from\nhttps://developer.microsoft.com/microsoft-edge/webview2/\n\n"+
			"HFDesk keeps running in the notification area. The web UI is still available at "+url+".",
		windows.MB_ICONWARNING)
}

// runtimePageURL returns the URL of the runtime page of the running server.
//
// The page is served by the server itself from the embedded assets, so the
// window shows the live state of the very instance that owns it.
func runtimePageURL(url string) string {
	return strings.TrimRight(url, "/") + "/runtime.html"
}
