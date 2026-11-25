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

// submenuFakes is the fake Service section of a test: the ROOT entry, every
// item the plan can show inside it, plus the menu the refresh logic writes
// them through.
type submenuFakes struct {
	service *fakeTrayItem
	line    *fakeTrayItem
	open    *fakeTrayItem
	install *fakeTrayItem
	start   *fakeTrayItem
	stop    *fakeTrayItem
	update  *fakeTrayItem
	delete  *fakeTrayItem
	menu    *trayServiceMenu
}

// newSubmenuFakes builds a service section whose items start in the state the
// runner creates them: visible, and grey until the menu enables them.
func newSubmenuFakes(openFollowsService bool) *submenuFakes {
	f := &submenuFakes{
		service: newFakeTrayItem(),
		line:    newFakeTrayItem(),
		open:    newFakeTrayItem(),
		install: newFakeTrayItem(),
		start:   newFakeTrayItem(),
		stop:    newFakeTrayItem(),
		update:  newFakeTrayItem(),
		delete:  newFakeTrayItem(),
	}
	f.menu = &trayServiceMenu{
		service: f.service, line: f.line, open: f.open, install: f.install,
		start: f.start, stop: f.stop, update: f.update, delete: f.delete,
		openFollowsService: openFollowsService,
	}
	return f
}

// menuView is what the fake submenu shows at one moment.
type menuView struct {
	serviceVisible bool
	line           string
	lineVisible    bool
	installVisible bool
	actionsVisible bool
	startVisible   bool
	startEnabled   bool
	stopEnabled    bool
	updateEnabled  bool
	deleteEnabled  bool
	openVisible    bool
	openEnabled    bool
}

// view reads the fake items.
func (f *submenuFakes) view() menuView {
	var v menuView
	_, _, v.serviceVisible, _ = f.service.snapshot()
	v.line, _, v.lineVisible, _ = f.line.snapshot()
	_, _, v.installVisible, _ = f.install.snapshot()
	_, v.startEnabled, v.startVisible, _ = f.start.snapshot()
	_, v.stopEnabled, v.actionsVisible, _ = f.stop.snapshot()
	_, v.updateEnabled, _, _ = f.update.snapshot()
	_, v.deleteEnabled, _, _ = f.delete.snapshot()
	_, v.openEnabled, v.openVisible, _ = f.open.snapshot()
	return v
}

// waitFor polls until the submenu satisfies the expectation and fails the
// test when it never does, reporting the values it last read.
func (f *submenuFakes) waitFor(t *testing.T, what string, want func(menuView) bool) menuView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got menuView
	for time.Now().Before(deadline) {
		got = f.view()
		if want(got) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the menu did not show %s: %+v", what, got)
	return got
}

// TestMenuRedrawsAfterEveryWindowRead proves the end of the redraw seam: a
// display read made for the Logs window ends with the refresh request,
// keepFresh re-reads through the same display policy and applies the
// result — including the unsettled read of a failed one, which leaves the
// menu without a Service entry at all (no line, no item, no empty submenu),
// so no read made for the window can
// stay invisible in the menu until the next tick.
//
// The cadence itself is not part of this test: the tray keeps its own 30
// second tick, and the request travels the existing non-blocking refresh
// signal.
func TestMenuRedrawsAfterEveryWindowRead(t *testing.T) {
	replaceSeam(t, &serviceReadRetryDelay, time.Millisecond)
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

	fakes := newSubmenuFakes(false)

	refresh := make(chan struct{}, 1)
	view := trayServiceView{read: serviceState.readForDisplay, refresh: refresh}
	stopped := make(chan struct{})
	go fakes.menu.keepFresh(view, stopped)
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
	fakes.waitFor(t, "the running service", func(v menuView) bool {
		return v.serviceVisible && v.line == "Service: running" && v.lineVisible &&
			!v.installVisible && v.actionsVisible
	})

	// The registration is gone between the two window polls: the second
	// read must reach the menu as well, not wait for the next tick.
	setCurrent(service.Status{State: service.StateNotInstalled}, nil)
	if state, err := readRuntimeServiceState(requestRefresh); err != nil || state != "not-installed" {
		t.Fatalf("the window read answered (%q, %v), want the not-installed state", state, err)
	}
	fakes.waitFor(t, "the machine without a registration", func(v menuView) bool {
		return v.line == "Service: not installed" && v.lineVisible &&
			v.installVisible && !v.actionsVisible
	})

	// A read that fails must reach the menu as the empty submenu — no
	// line and no item, never a claimed absence — because it asks for the
	// catch-up as any other read does.
	setCurrent(service.Status{}, errors.New("the service control manager did not answer"))
	if state, err := readRuntimeServiceState(requestRefresh); err == nil {
		t.Fatal("the window read answered no error although the read failed")
	} else if state != "" {
		t.Errorf("the failed window read answered the state %q, want no state beside the error", state)
	}
	fakes.waitFor(t, "the state that could not be read", func(v menuView) bool {
		return !v.serviceVisible && !v.lineVisible && !v.installVisible &&
			!v.actionsVisible && !v.startVisible
	})

	// The next successful read brings the line and the items back by
	// itself: nothing of the menu was latched by the failure.
	setCurrent(service.Status{State: service.StateStopped}, nil)
	if state, err := readRuntimeServiceState(requestRefresh); err != nil || state != "stopped" {
		t.Fatalf("the window read after the failure answered (%q, %v), want the stopped state", state, err)
	}
	fakes.waitFor(t, "the registration the read after the failure proved", func(v menuView) bool {
		return v.serviceVisible && v.line == "Service: stopped" && v.lineVisible &&
			!v.installVisible && v.actionsVisible && v.startVisible && v.startEnabled
	})
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

	registered := serviceMenuState{
		Line: "Service: stopped", LineVisible: true,
		ActionsVisible: true, StartVisible: true, StartEnabled: true,
	}
	notInstalled := serviceMenuState{Line: "Service: not installed", LineVisible: true, InstallVisible: true}

	menu.apply(registered)
	if title, _, visible, _ := line.snapshot(); title != registered.Line || !visible {
		t.Errorf("the line shows %q (visible=%t), want %q and visible", title, visible, registered.Line)
	}
	if _, _, visible, _ := install.snapshot(); visible {
		t.Error("the install item is visible beside a registration")
	}
	if _, _, visible, _ := start.snapshot(); !visible {
		t.Error("the start item is hidden beside a registration")
	}
	writesAfterFirst := totalItemWrites(line, install, start)
	if got := len(records.all()); got != 1 {
		t.Fatalf("the first apply recorded %d updates, want the one that built the menu: %v",
			got, records.messages())
	}

	// The identical state is dropped: no item is touched and no update is
	// recorded for a change the menu did not perform.
	menu.apply(registered)
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
		t.Error("the start item stayed visible although the read proved no registration")
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
// the visibility does not), an unsettled read (the line and every item
// hide), a settled stop (the line and the items show again), a broken
// registration (the start hides, the update and the delete stay), a
// running-fact state (only the line changes — the already hidden install
// item must receive nothing at all) and the absence again (install returns
// through Show).
// The assertion is on the recorded operation ORDER of each item: between
// a hide and the next show only nothing may happen. On the former write
// order (hide, then disable, in one pass) this test fails; on the
// current order it passes.
func TestMenuNeverUpdatesAnItemAfterHidingIt(t *testing.T) {
	entry := newFakeTrayItem()
	line := newFakeTrayItem()
	open := newFakeTrayItem()
	install := newFakeTrayItem()
	start := newFakeTrayItem()
	stop := newFakeTrayItem()
	update := newFakeTrayItem()
	del := newFakeTrayItem()
	menu := &trayServiceMenu{
		service: entry, line: line, open: open, install: install,
		start: start, stop: stop, update: update, delete: del,
		openFollowsService: true,
	}

	states := []serviceMenuState{
		serviceMenuFor(service.Status{State: service.StateNotInstalled}, nil),
		serviceMenuFor(service.Status{State: service.StateRunning, Running: true}, nil),
		serviceMenuFor(service.Status{State: service.StateStarting}, nil),
		serviceMenuFor(service.Status{}, errors.New("the service control manager did not answer")),
		serviceMenuFor(service.Status{State: service.StateStopped}, nil),
		serviceMenuFor(service.Status{State: service.StateBroken}, nil),
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
		"service": entry, "line": line, "open": open, "install": install, "start": start,
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
	requireOps("line", line, "hide", "show")
	requireOps("service", entry, "hide", "show")
}

// TestRootServiceEntryFollowsTheRead pins the root consequence of an
// unsettled read: the entry that opens the Service submenu is GONE from the
// root menu — an empty submenu is the same dead surface as the retired
// "unknown" row — and the first read that proves a state brings the entry
// back with its line and its items.
//
// The rest of the root is the plan's business and stays what it is (Open
// HFDesk, Logs, the separator and the end item, pinned by
// TestTrayMenuPlanRootsPinBothCompositions): apply owns the service section
// alone, so hiding its entry can never take another root item with it.
func TestRootServiceEntryFollowsTheRead(t *testing.T) {
	fakes := newSubmenuFakes(true)
	fakes.menu.apply(serviceMenuFor(service.Status{}, errors.New("the service control manager did not answer")))

	visible := func(item *fakeTrayItem) bool {
		_, _, shown, _ := item.snapshot()
		return shown
	}
	if visible(fakes.service) {
		t.Error("the root Service entry is on screen at an unsettled read: it would open an empty submenu")
	}
	if visible(fakes.line) || visible(fakes.install) || visible(fakes.start) ||
		visible(fakes.stop) || visible(fakes.update) || visible(fakes.delete) || visible(fakes.open) {
		t.Error("an item of the service section is on screen at an unsettled read")
	}

	// The successful read restores the entry beside its line and items.
	fakes.menu.apply(serviceMenuFor(service.Status{State: service.StateRunning, Running: true}, nil))
	if !visible(fakes.service) {
		t.Error("the root Service entry did not return after a read proved a state")
	}
	if !visible(fakes.line) || !visible(fakes.stop) || !visible(fakes.open) {
		t.Error("the service section did not return with the entry that opens it")
	}

	// The entry is never greyed: a disabled parent would block the submenu
	// it opens, so it acts whenever it is on screen.
	if _, enabled, _, _ := fakes.service.snapshot(); !enabled {
		t.Error("the root Service entry is grey, want it acting beside the state that proved it")
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
