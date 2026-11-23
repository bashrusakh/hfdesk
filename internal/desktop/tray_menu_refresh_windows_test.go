// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// fakeTrayItem is a menu item of a test: it records the writes it receives
// and keeps the value the menu currently shows, so a test can read what the
// refresh logic applied without a notification area.
type fakeTrayItem struct {
	mu      sync.Mutex
	title   string
	enabled bool
	visible bool
	writes  int
}

func (f *fakeTrayItem) SetTitle(value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.title = value
	f.writes++
}

func (f *fakeTrayItem) Enable() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = true
	f.writes++
}

func (f *fakeTrayItem) Disable() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = false
	f.writes++
}

func (f *fakeTrayItem) Show() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.visible = true
	f.writes++
}

func (f *fakeTrayItem) Hide() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.visible = false
	f.writes++
}

// Click records no click: the tests of the refresh logic never click an
// item, and the handler the menu wires belongs to the tray loop.
func (f *fakeTrayItem) Click(func()) {}

// snapshot returns the current values of the item.
func (f *fakeTrayItem) snapshot() (title string, enabled, visible bool, writes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.title, f.enabled, f.visible, f.writes
}

// waitForServiceMenuItem polls until the three items of the menu show the
// expected state, and fails the test when they never do.
func waitForServiceMenuItem(t *testing.T, what string, line *fakeTrayItem, install, serviceItem *fakeTrayItem,
	wantLine string, wantInstallEnabled, wantServiceVisible bool,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var gotLine string
	var gotInstallEnabled, gotServiceVisible bool
	for time.Now().Before(deadline) {
		gotLine, _, _, _ = line.snapshot()
		_, gotInstallEnabled, _, _ = install.snapshot()
		_, _, gotServiceVisible, _ = serviceItem.snapshot()
		if gotLine == wantLine && gotInstallEnabled == wantInstallEnabled && gotServiceVisible == wantServiceVisible {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the menu did not show %s: line=%q installEnabled=%t serviceVisible=%t",
		what, gotLine, gotInstallEnabled, gotServiceVisible)
}

// TestMenuRedrawsAfterEveryWindowRead proves the end of the redraw seam: a
// display read made for the Runtime / Logs window ends with the refresh
// request, keepFresh re-reads through the same display policy and applies
// the result — including the honest unknown state of a read that failed, so
// no read made for the window can stay invisible in the menu until the next
// tick.
//
// The cadence itself is not part of this test: the tray keeps its own 30
// second tick, and the request travels the existing non-blocking refresh
// signal.
func TestMenuRedrawsAfterEveryWindowRead(t *testing.T) {
	var mu sync.Mutex
	current := service.Status{State: service.StateRunning, Running: true}
	var readErr error
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, readErr
	})
	setCurrent := func(status service.Status, err error) {
		mu.Lock()
		defer mu.Unlock()
		current = status
		readErr = err
	}

	line := &fakeTrayItem{}
	install := &fakeTrayItem{}
	serviceItem := &fakeTrayItem{}
	startItem := &fakeTrayItem{}
	stopItem := &fakeTrayItem{}
	updateItem := &fakeTrayItem{}
	deleteItem := &fakeTrayItem{}
	menu := &trayServiceMenu{
		line: line, install: install, service: serviceItem,
		start: startItem, stop: stopItem, update: updateItem, delete: deleteItem,
	}

	refresh := make(chan struct{}, 1)
	view := trayServiceView{read: serviceState.readForDisplay, refresh: refresh}
	stopped := make(chan struct{})
	go menu.keepFresh(view, stopped)
	t.Cleanup(func() { close(stopped) })

	// The window's own read posts exactly the request the host posts.
	requestRefresh := func() {
		select {
		case refresh <- struct{}{}:
		default:
		}
	}

	// A window read of a running service: the menu catches up with a
	// registration it must show as installed.
	if state, err := readRuntimeServiceState(requestRefresh); err != nil || state != "running" {
		t.Fatalf("the window read answered (%q, %v), want the running state", state, err)
	}
	waitForServiceMenuItem(t, "the running service", line, install, serviceItem,
		"Service: installed", false, true)

	// The registration is gone between the two window polls: the second
	// read must reach the menu as well, not wait for the next tick.
	setCurrent(service.Status{State: service.StateNotInstalled}, nil)
	if state, err := readRuntimeServiceState(requestRefresh); err != nil || state != "not-installed" {
		t.Fatalf("the window read answered (%q, %v), want the not-installed state", state, err)
	}
	waitForServiceMenuItem(t, "the machine without a registration", line, install, serviceItem,
		"Service: not installed", true, false)

	// A read that fails must reach the menu as the honest unknown state —
	// it asks for the catch-up as any other read does.
	setCurrent(service.Status{}, errors.New("the service control manager did not answer"))
	if state, err := readRuntimeServiceState(requestRefresh); err == nil {
		t.Fatal("the window read answered no error although the read failed")
	} else if state != "" {
		t.Errorf("the failed window read answered the state %q, want no state beside the error", state)
	}
	waitForServiceMenuItem(t, "the state that could not be read", line, install, serviceItem,
		"Service: unknown", false, false)
}

// TestTrayMenuApplyDeduplicatesAndRecordsTransitions proves the two rules of
// the menu update itself: a state equal to the one already shown is dropped
// without touching the items — the result of that read is visible by
// definition, so the refresh after every window read cannot rewrite the same
// values on every status poll — and every state that does change the menu
// records the line it applied, so the log carries the transitions of the
// line the user reads beside the records of the reads themselves.
func TestTrayMenuApplyDeduplicatesAndRecordsTransitions(t *testing.T) {
	records := captureLogRecords(t)

	line := &fakeTrayItem{}
	install := &fakeTrayItem{}
	serviceItem := &fakeTrayItem{}
	menu := &trayServiceMenu{
		line: line, install: install, service: serviceItem,
		start: &fakeTrayItem{}, stop: &fakeTrayItem{},
		update: &fakeTrayItem{}, delete: &fakeTrayItem{},
	}

	installed := serviceMenuState{Line: "Service: installed", InstallEnabled: false, ServiceVisible: true}
	notInstalled := serviceMenuState{Line: "Service: not installed", InstallEnabled: true, ServiceVisible: false}

	menu.apply(installed)
	if title, _, _, _ := line.snapshot(); title != installed.Line {
		t.Errorf("the line shows %q, want %q", title, installed.Line)
	}
	if _, enabled, _, _ := install.snapshot(); enabled {
		t.Error("the install item is enabled beside an installed service")
	}
	if _, _, visible, _ := serviceItem.snapshot(); !visible {
		t.Error("the registered-service item is hidden beside an installed service")
	}
	writesAfterFirst := totalItemWrites(line, install, serviceItem)
	if got := len(records.all()); got != 1 {
		t.Fatalf("the first apply recorded %d updates, want the one that built the menu: %v",
			got, records.messages())
	}

	// The identical state is dropped: no item is touched and no update is
	// recorded for a change the menu did not perform.
	menu.apply(installed)
	if got := totalItemWrites(line, install, serviceItem); got != writesAfterFirst {
		t.Errorf("the identical state wrote %d item updates, want none: the result is already visible",
			got-writesAfterFirst)
	}
	if got := len(records.all()); got != 1 {
		t.Errorf("the identical state recorded %d updates in total, want the one of the first apply: %v",
			got, records.messages())
	}

	// The changed state is applied and its line is recorded as the
	// transition it is.
	menu.apply(notInstalled)
	if title, _, _, _ := line.snapshot(); title != notInstalled.Line {
		t.Errorf("the line shows %q, want %q", title, notInstalled.Line)
	}
	if _, enabled, _, _ := install.snapshot(); !enabled {
		t.Error("the install item stayed grey although the read proved no registration")
	}
	if _, _, visible, _ := serviceItem.snapshot(); visible {
		t.Error("the registered-service item stayed visible although the read proved no registration")
	}
	transitioned := 0
	for _, record := range records.all() {
		if record.attrs["line"] == notInstalled.Line {
			transitioned++
		}
	}
	if transitioned != 1 {
		t.Errorf("the transition to %q was recorded %d times, want exactly once: %v",
			notInstalled.Line, transitioned, records.messages())
	}
}

// totalItemWrites returns how many item updates the given items received.
func totalItemWrites(items ...*fakeTrayItem) int {
	total := 0
	for _, item := range items {
		_, _, _, count := item.snapshot()
		total += count
	}
	return total
}
