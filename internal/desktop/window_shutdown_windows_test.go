// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"sync"
	"testing"
	"time"
)

// shutdownView is the window seam of the shutdown tests.
//
// The real window ends its message loop when it is destroyed, and the loop is
// what the shutdown waits for. The fake models that end through onClose, so a
// test can prove WHEN the server is stopped relative to the window.
type shutdownView struct {
	mu      sync.Mutex
	closes  int
	onClose func()
}

// The fake stands in for the WebView2 driver the window is built on.
var _ webView = (*shutdownView)(nil)

func (v *shutdownView) Show() error               { return nil }
func (v *shutdownView) Hide()                     {}
func (v *shutdownView) Navigate(string)           {}
func (v *shutdownView) NavigateDocument(string)   {}
func (v *shutdownView) BindAction(string, func()) {}

func (v *shutdownView) Close() {
	v.mu.Lock()
	v.closes++
	hook := v.onClose
	v.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (v *shutdownView) closeCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.closes
}

// shutdownTestWindow builds a window owner around the fake view, with the
// fake's done channel as the end of the message loop — the very channel the
// production creation closes when the loop of the window returned.
func shutdownTestWindow(view *shutdownView, done chan struct{}) *runtimeWindow {
	window := newRuntimeWindow("http://127.0.0.1:8080/runtime.html", nil, nil)
	window.instance = &windowInstance{view: view, done: done}
	return window
}

// TestCloseWindowBeforeServerStopDestroysTheWindowFirst pins the order the red
// flash of the Logs window is fixed by: the window is destroyed — and its
// message loop has ended — BEFORE the server that serves its page is stopped.
//
// A window that outlives the stop keeps a page whose server is gone, and that
// page paints its own error state (the error banner, the red status
// indicators) within the first failed poll. Ordering the close first removes
// the page that could paint it, for the committed service handover and for
// every ordinary quit alike.
func TestCloseWindowBeforeServerStopDestroysTheWindowFirst(t *testing.T) {
	view := &shutdownView{}
	done := make(chan struct{})

	// The window thread ends its loop a moment after the destroy. The
	// shutdown has to wait for that end and not race it.
	view.onClose = func() {
		go func() {
			time.Sleep(20 * time.Millisecond)
			close(done)
		}()
	}

	window := shutdownTestWindow(view, done)

	stopped := false
	cancel := func() {
		stopped = true
		select {
		case <-done:
		default:
			t.Error("the server was stopped before the message loop of the Logs window ended")
		}
	}

	closeWindowBeforeServerStop(window, time.Second, cancel)

	if !stopped {
		t.Fatal("the server was not stopped at all")
	}
	if got := view.closeCount(); got != 1 {
		t.Errorf("the window was destroyed %d time(s), want exactly once", got)
	}
}

// TestCloseWindowBeforeServerStopStopsTheServerWhenTheWindowDoesNotAnswer pins
// the bound of the wait: a window whose loop never ends must not hold the
// shutdown. The server is stopped anyway, so the host still ends.
func TestCloseWindowBeforeServerStopStopsTheServerWhenTheWindowDoesNotAnswer(t *testing.T) {
	const budget = 50 * time.Millisecond

	view := &shutdownView{}
	done := make(chan struct{}) // never closed: the loop does not end
	window := shutdownTestWindow(view, done)

	stopped := 0
	started := time.Now()
	closeWindowBeforeServerStop(window, budget, func() { stopped++ })
	elapsed := time.Since(started)

	if stopped != 1 {
		t.Fatalf("the server was stopped %d time(s), want exactly once", stopped)
	}
	if elapsed < budget {
		t.Errorf("the shutdown stopped the server after %s, want the window budget of %s to be waited out", elapsed, budget)
	}
	if elapsed > 2*time.Second {
		t.Errorf("the shutdown waited %s for a window that does not answer, want a bounded wait", elapsed)
	}
	if got := view.closeCount(); got != 1 {
		t.Errorf("the window was destroyed %d time(s), want exactly once", got)
	}
}

// TestCloseWindowBeforeServerStopWithoutAWindowStopsTheServer pins the states
// of a host that has no window to close: a host that never created the window
// (the window is assigned after the shutdown exists), and a window owner whose
// instance is already gone. Both stop the server, and neither panics.
func TestCloseWindowBeforeServerStopWithoutAWindowStopsTheServer(t *testing.T) {
	t.Run("the host never created the window", func(t *testing.T) {
		stopped := 0
		closeWindowBeforeServerStop(nil, time.Second, func() { stopped++ })
		if stopped != 1 {
			t.Errorf("the server was stopped %d time(s), want exactly once", stopped)
		}
	})

	t.Run("the window owner has no instance", func(t *testing.T) {
		stopped := 0
		closeWindowBeforeServerStop(newRuntimeWindow("http://127.0.0.1:8080/runtime.html", nil, nil),
			time.Second, func() { stopped++ })
		if stopped != 1 {
			t.Errorf("the server was stopped %d time(s), want exactly once", stopped)
		}
	})
}

// TestRuntimeWindowCloseDoesNotWaitForTheLoop pins the difference between the
// two closes: the plain Close only asks the window to destroy itself, so every
// existing caller keeps its behavior, while the shutdown's CloseAndWait also
// waits for the end of the loop.
func TestRuntimeWindowCloseDoesNotWaitForTheLoop(t *testing.T) {
	view := &shutdownView{}
	done := make(chan struct{}) // never closed
	window := shutdownTestWindow(view, done)

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		window.Close()
	}()

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Close() waited for a message loop that does not end")
	}
	if got := view.closeCount(); got != 1 {
		t.Errorf("Close() destroyed the window %d time(s), want exactly once", got)
	}
}
