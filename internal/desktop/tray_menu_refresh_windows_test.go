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

// fakeTrayItem is a menu item of a test: it records the writes it
// receives IN ORDER and keeps the values the menu currently shows, so a
// test can read what the refresh logic applied without a notification
// area. The order is the pin of the hide rule: the tray library
// re-inserts a hidden item on ANY update after Hide, so no enable,
// disable or set-title may follow a hide until the next show.
//
// A fake starts VISIBLE — the state the runner creates every real item
// in — so the first apply writes it the way the real menu does.
type fakeTrayItem struct {
	mu      sync.Mutex
	title   string
	enabled bool
	visible bool
	writes  int
	ops     []string
}

// newFakeTrayItem returns an item in the state a created menu item has:
// visible, and grey until the menu enables it.
func newFakeTrayItem() *fakeTrayItem {
	return &fakeTrayItem{visible: true}
}

func (f *fakeTrayItem) SetTitle(value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.title = value
	f.writes++
	f.ops = append(f.ops, "settitle")
}

func (f *fakeTrayItem) Enable() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = true
	f.writes++
	f.ops = append(f.ops, "enable")
}

func (f *fakeTrayItem) Disable() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = false
	f.writes++
	f.ops = append(f.ops, "disable")
}

func (f *fakeTrayItem) Show() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.visible = true
	f.writes++
	f.ops = append(f.ops, "show")
}

func (f *fakeTrayItem) Hide() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.visible = false
	f.writes++
	f.ops = append(f.ops, "hide")
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

// operations returns the writes the item received, in order.
func (f *fakeTrayItem) operations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// waitForServiceMenuItem polls until the items of the submenu show the
// expected state, and fails the test when they never do. The install item
// is probed by VISIBILITY (a hidden item keeps the enabled value it had
// when it was hidden), the lifecycle group by the visibility of its start
// item.
func waitForServiceMenuItem(t *testing.T, what string, line, install, group *fakeTrayItem,
	wantLine string, wantInstallVisible, wantActionsVisible bool,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var gotLine string
	var gotInstallVisible, gotActionsVisible bool
	for time.Now().Before(deadline) {
		gotLine, _, _, _ = line.snapshot()
		_, _, gotInstallVisible, _ = install.snapshot()
		_, _, gotActionsVisible, _ = group.snapshot()
		if gotLine == wantLine && gotInstallVisible == wantInstallVisible && gotActionsVisible == wantActionsVisible {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the menu did not show %s: line=%q installVisible=%t actionsVisible=%t",
		what, gotLine, gotInstallVisible, gotActionsVisible)
}

// TestMenuRedrawsAfterEveryWindowRead proves the end of the redraw seam: a
// display read made for the Logs window ends with the refresh request,
// keepFresh re-reads through the same display policy and applies the
// result — including the honest unknown state of a read that failed, so
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

	line := newFakeTrayItem()
	install := newFakeTrayItem()
	startItem := newFakeTrayItem()
	stopItem := newFakeTrayItem()
	updateItem := newFakeTrayItem()
	deleteItem := newFakeTrayItem()
	menu := &trayServiceMenu{
		line: line, install: install,
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
	// registration it must show as running, install hidden.
	if state, err := readRuntimeServiceState(requestRefresh); err != nil || state != "running" {
		t.Fatalf("the window read answered (%q, %v), want the running state", state, err)
	}
	waitForServiceMenuItem(t, "the running service", line, install, startItem,
		"Service: running", false, true)

	// The registration is gone between the two window polls: the second
	// read must reach the menu as well, not wait for the next tick.
	setCurrent(service.Status{State: service.StateNotInstalled}, nil)
	if state, err := readRuntimeServiceState(requestRefresh); err != nil || state != "not-installed" {
		t.Fatalf("the window read answered (%q, %v), want the not-installed state", state, err)
	}
	waitForServiceMenuItem(t, "the machine without a registration", line, install, startItem,
		"Service: not installed", true, false)

	// A read that fails must reach the menu as the honest unknown state —
	// it asks for the catch-up as any other read does.
	setCurrent(service.Status{}, errors.New("the service control manager did not answer"))
	if state, err := readRuntimeServiceState(requestRefresh); err == nil {
		t.Fatal("the window read answered no error although the read failed")
	} else if state != "" {
		t.Errorf("the failed window read answered the state %q, want no state beside the error", state)
	}
	waitForServiceMenuItem(t, "the state that could not be read", line, install, startItem,
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

	line := newFakeTrayItem()
	install := newFakeTrayItem()
	start := newFakeTrayItem()
	menu := &trayServiceMenu{
		line: line, install: install, start: start,
		stop:   newFakeTrayItem(),
		update: newFakeTrayItem(),
		delete: newFakeTrayItem(),
	}

	installed := serviceMenuState{Line: "Service: installed", ActionsVisible: true, StartEnabled: true}
	notInstalled := serviceMenuState{Line: "Service: not installed", InstallVisible: true}

	menu.apply(installed)
	if title, _, _, _ := line.snapshot(); title != installed.Line {
		t.Errorf("the line shows %q, want %q", title, installed.Line)
	}
	if _, _, visible, _ := install.snapshot(); visible {
		t.Error("the install item is visible beside an installed service")
	}
	if _, _, visible, _ := start.snapshot(); !visible {
		t.Error("the lifecycle group is hidden beside an installed service")
	}
	writesAfterFirst := totalItemWrites(line, install, start)
	if got := len(records.all()); got != 1 {
		t.Fatalf("the first apply recorded %d updates, want the one that built the menu: %v",
			got, records.messages())
	}

	// The identical state is dropped: no item is touched and no update is
	// recorded for a change the menu did not perform.
	menu.apply(installed)
	if got := totalItemWrites(line, install, start); got != writesAfterFirst {
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
	if _, _, visible, _ := install.snapshot(); !visible {
		t.Error("the install item stayed hidden although the read proved no registration")
	}
	if _, _, visible, _ := start.snapshot(); visible {
		t.Error("the lifecycle group stayed visible although the read proved no registration")
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

// TestMenuNeverUpdatesAnItemAfterHidingIt pins the write ORDER over every
// hide/show edge of the submenu — the rule the tray library forces: Hide
// removes the item from the menu, and ANY later update of that item
// (Enable, Disable, SetTitle) re-inserts it (the library's update path
// inserts an item it does not find visible), which resurrected the grey
// install item beside a registration — the duplicate this menu removes.
//
// The walk applies one state per edge: an absence, a running
// registration (both groups swap), a transition (the line changes while
// the visibility does not), an unsettled read (everything that acts
// hides), a settled stop (everything shows again), a running-fact state
// (only the line changes — the already hidden install item must receive
// nothing at all) and the absence again (install returns through Show).
// The assertion is on the recorded operation ORDER of each item: between
// a hide and the next show only nothing may happen. On the former write
// order (hide, then disable, in one pass) this test fails; on the
// current order it passes.
func TestMenuNeverUpdatesAnItemAfterHidingIt(t *testing.T) {
	line := newFakeTrayItem()
	open := newFakeTrayItem()
	install := newFakeTrayItem()
	start := newFakeTrayItem()
	stop := newFakeTrayItem()
	update := newFakeTrayItem()
	del := newFakeTrayItem()
	menu := &trayServiceMenu{
		line: line, open: open, install: install,
		start: start, stop: stop, update: update, delete: del,
		openFollowsService: true,
	}

	states := []serviceMenuState{
		serviceMenuFor(service.Status{State: service.StateNotInstalled}, nil),
		serviceMenuFor(service.Status{State: service.StateRunning, Running: true}, nil),
		serviceMenuFor(service.Status{State: service.StateStarting}, nil),
		serviceMenuFor(service.Status{}, errors.New("the service control manager did not answer")),
		serviceMenuFor(service.Status{State: service.StateStopped}, nil),
		serviceMenuFor(service.Status{State: service.StateStopped, Running: true}, nil),
		// The absence again: the install item — hidden since the
		// running registration — must come back through Show, and the
		// group hides once more.
		serviceMenuFor(service.Status{State: service.StateNotInstalled}, nil),
	}
	for _, state := range states {
		menu.apply(state)
	}

	managed := map[string]*fakeTrayItem{
		"open": open, "install": install, "start": start,
		"stop": stop, "update": update, "delete": del,
	}
	for name, item := range managed {
		ops := item.operations()
		hidden := false
		for _, op := range ops {
			switch op {
			case "hide":
				hidden = true
			case "show":
				hidden = false
			default:
				if hidden {
					t.Errorf("the %s item received %q after Hide: the tray library re-inserts a hidden item, "+
						"which resurrects it grey — the duplicate the submenu rework removes; ops=%v",
						name, op, ops)
				}
			}
		}
	}

	// The walk must really have exercised the edges, or the scan above
	// would pass on an empty record: the install item has to travel
	// visible → hidden → visible, and the group the other way round.
	requireOps := func(name string, item *fakeTrayItem, want ...string) {
		t.Helper()
		ops := item.operations()
		for _, w := range want {
			found := false
			for _, op := range ops {
				if op == w {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("the %s item never saw %q, the walk did not exercise that edge: ops=%v", name, w, ops)
			}
		}
	}
	requireOps("install", install, "hide", "show")
	requireOps("start", start, "hide", "show")
	requireOps("open", open, "hide", "show")
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
