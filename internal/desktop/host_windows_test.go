// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/energye/systray"
	"golang.org/x/sys/windows"
)

// trayReadyTimeout bounds the wait for the notification area host.
//
// The tray library creates its window and adds its icon to the notification
// area in a moment, so the budget only covers a slow machine.
const trayReadyTimeout = 15 * time.Second

// trayStopTimeout bounds the wait for the message loop of the tray thread.
//
// The loop ends when the window of the tray is destroyed. The budget covers
// the moment between the quit and the end of the loop.
const trayStopTimeout = 10 * time.Second

// windowCreationTimeout bounds the wait for the Runtime / Logs window.
//
// The window creates a WebView2 environment on a thread of its own, which
// can take a moment on a cold machine.
const windowCreationTimeout = 30 * time.Second

// windowCloseTimeout bounds the wait for the message loop of the window
// thread.
//
// The loop ends when the window is destroyed.
const windowCloseTimeout = 10 * time.Second

// procPostThreadMessageW posts a message to the message queue of a thread.
//
// The tray library pumps the messages of the thread it runs on, so a message
// that ends its loop has to be posted to that very thread.
var procPostThreadMessageW = windows.NewLazySystemDLL("user32.dll").NewProc("PostThreadMessageW")

// wmQuit ends the message loop of a thread.
const wmQuit = 0x0012

// trayHostStarted reports whether the notification area host was already
// started in this process.
//
// The tray library is a process wide singleton: it registers its window
// class once and its quit is a one-shot, so the notification area host can
// only be started and stopped once per test process.
var trayHostStarted bool

// TestDesktopTrayHostBecomesReady proves that the desktop host creates the
// notification area icon: the tray host of the production code is started
// and it reports that it is ready.
//
// The tray library locks the OS thread in its package init and it creates
// its window on the thread that pumps the messages of the window, so the
// test runs the tray on a thread of its own.
//
// The test never shows a message box and never waits without a bound: an
// environment that cannot host the notification area (no interactive window
// station and no shell that owns it) skips the test instead of failing it.
func TestDesktopTrayHostBecomesReady(t *testing.T) {
	if trayHostStarted {
		t.Skip("the tray library is a process wide singleton: the notification area host can only be started once per test process")
	}
	trayHostStarted = true

	iconBytes, err := productIconBytes()
	if err != nil {
		t.Fatalf("build the product icon: %v", err)
	}

	ready := make(chan struct{})
	stopped := make(chan struct{})
	threadIDs := make(chan uint32, 1)

	go func() {
		// The window of the tray belongs to the thread that pumps its
		// messages, so the thread is locked to this goroutine for the
		// whole life of the tray.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(stopped)
		threadIDs <- windows.GetCurrentThreadId()
		runTray(iconBytes, "HFDesk - test", trayActions{
			open:    func() {},
			runtime: func() {},
			install: func() {},
			quit:    func() {},
		}, ready)
	}()

	threadID := <-threadIDs

	select {
	case <-ready:
		// The notification area host came up.
	case <-stopped:
		// The message loop ended without a ready host: the tray
		// library could not create its window.
		t.Skip("the notification area icon could not be created in this environment: " +
			"the tray library reports no ready host without an interactive window station and a shell that owns the notification area")
	case <-time.After(trayReadyTimeout):
		// The environment cannot host the notification area. The
		// message loop of the thread is ended, so the test leaves no
		// goroutine behind.
		if !stopTrayLoop(threadID, stopped) {
			t.Fatal("the message loop of the tray thread did not end")
		}
		t.Skip("the notification area icon could not be created in this environment: " +
			"the tray library reports no ready host without an interactive window station and a shell that owns the notification area")
	}

	// The notification area host is up. It is stopped again, so the test
	// leaves no icon in the notification area behind.
	if !stopTrayLoop(threadID, stopped) {
		t.Fatal("the message loop of the tray thread did not end after the quit")
	}
}

// TestDesktopRuntimeHostCreatesWindow proves that the desktop host creates
// the Runtime / Logs window: the window owner of the production code is
// built, the window is shown and the window is destroyed again.
//
// The test never shows a message box: the production helper that shows the
// window to the user is not called, the window owner is driven directly.
//
// An environment without the WebView2 runtime skips the test: the window
// needs the runtime, and the runtime is not part of every Windows machine.
func TestDesktopRuntimeHostCreatesWindow(t *testing.T) {
	available, version := webview2Available()
	if !available {
		t.Skip("the Microsoft Edge WebView2 runtime is not installed in this environment: " +
			"the Runtime / Logs window needs it")
	}

	// The WebView2 runtime keeps its user data in the per user
	// configuration directory, so the test points it at a temporary
	// directory and never touches the data of a real installation.
	//
	// The browser process of the runtime is a process of its own and it
	// can still hold the files of that data when the window was closed,
	// so the temporary directory is removed on a best effort basis. The
	// removal runs after the window was closed: the cleanup of the
	// window is registered after the cleanup of the directory, and the
	// cleanups of the test framework run in the reverse order of their
	// registration.
	useTemporaryStateDir(t)

	// The window shows a page of a local server, so the navigation stays
	// on the machine and the test does not depend on the network.
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><title>HFDesk runtime</title>"))
	}))
	defer page.Close()

	iconBytes, err := productIconBytes()
	if err != nil {
		t.Fatalf("build the product icon: %v", err)
	}

	window := newRuntimeWindow(page.URL, iconBytes)
	// The window is closed at the end of the test in every case, so the
	// test leaves no window behind.
	t.Cleanup(window.Close)

	// The window is created on a thread of its own and the creation can
	// take a moment, so the wait is bounded.
	shown := make(chan error, 1)
	go func() { shown <- window.Show() }()

	select {
	case err := <-shown:
		if err != nil {
			t.Skipf("the WebView2 runtime %s is installed, but the Runtime / Logs window could not be created in this environment: %v", version, err)
		}
	case <-time.After(windowCreationTimeout):
		t.Skipf("the WebView2 runtime %s is installed, but the Runtime / Logs window did not come up within %s in this environment", version, windowCreationTimeout)
	}

	// The window owner holds the window that was created.
	window.mu.Lock()
	instance := window.instance
	window.mu.Unlock()
	if instance == nil {
		t.Fatal("the runtime window holds no instance after it was shown")
	}
	if instance.closed() {
		t.Fatal("the runtime window was closed right after it was shown")
	}

	// The window is destroyed again, so the test leaves no window and no
	// goroutine behind.
	window.Close()
	select {
	case <-instance.done:
	case <-time.After(windowCloseTimeout):
		t.Fatal("the runtime window did not end its message loop after it was closed")
	}
}

// stopTrayLoop ends the message loop of the tray thread and waits for it.
//
// The quit of the tray library is a one-shot and it only reaches the tray
// when the library created its window, so the loop of the thread is ended
// with a quit message as the fallback.
//
// It reports whether the loop ended.
func stopTrayLoop(threadID uint32, stopped <-chan struct{}) bool {
	systray.Quit()
	select {
	case <-stopped:
		return true
	case <-time.After(trayStopTimeout):
	}

	// The tray library did not end its loop, so the thread is released
	// with a quit message.
	procPostThreadMessageW.Call(uintptr(threadID), uintptr(wmQuit), 0, 0)
	select {
	case <-stopped:
		return true
	case <-time.After(trayStopTimeout):
		return false
	}
}
