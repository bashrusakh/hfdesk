// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// TestServiceMenuForOneRead proves that one read of the registration maps
// onto the status line and the visibility claims of the Service submenu
// exactly as the surface promises:
//
//   - a proven absence shows the install item and hides the lifecycle
//     group, so a machine without a service shows no dead actions,
//   - a proven registration shows the lifecycle items and hides the
//     install item, so exactly one action group is ever on screen,
//   - a transition names itself in the line ("Service: starting",
//     "Service: stopping") while the registration is still proven,
//   - a broken registration reads with the failed status — the one user
//     status of a service that does not run and whose program is gone,
//   - and an unsettled read claims nothing and shows nothing: no line, no
//     item of the submenu at all, because "no service is installed" was
//     not proven by a read that failed.
func TestServiceMenuForOneRead(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         service.Status
		readErr        error
		line           string
		lineVisible    bool
		installVisible bool
		actionsVisible bool
	}{
		{
			name:           "no registration",
			status:         service.Status{State: service.StateNotInstalled},
			line:           "Service: not installed",
			lineVisible:    true,
			installVisible: true,
			actionsVisible: false,
		},
		{
			name:           "running service",
			status:         service.Status{State: service.StateRunning, Running: true},
			line:           "Service: running",
			lineVisible:    true,
			installVisible: false,
			actionsVisible: true,
		},
		{
			name:           "stopped service",
			status:         service.Status{State: service.StateStopped},
			line:           "Service: stopped",
			lineVisible:    true,
			installVisible: false,
			actionsVisible: true,
		},
		{
			name:           "service on its way up",
			status:         service.Status{State: service.StateStarting},
			line:           "Service: starting",
			lineVisible:    true,
			installVisible: false,
			actionsVisible: true,
		},
		{
			name:           "service on its way down",
			status:         service.Status{State: service.StateStopping},
			line:           "Service: stopping",
			lineVisible:    true,
			installVisible: false,
			actionsVisible: true,
		},
		{
			// A crashed instance and a registration whose program is
			// gone are one user status: neither runs, and from the
			// user's side both are repaired by the update.
			name:           "failed service",
			status:         service.Status{State: service.StateFailed},
			line:           "Service: failed",
			lineVisible:    true,
			installVisible: false,
			actionsVisible: true,
		},
		{
			name:           "broken registration",
			status:         service.Status{State: service.StateBroken},
			line:           "Service: failed",
			lineVisible:    true,
			installVisible: false,
			actionsVisible: true,
		},
		{
			// The running fact is a fact: a broken registration whose
			// process still serves IS running for the user, whatever
			// the registration's diagnosis says.
			name:           "broken registration with a running instance",
			status:         service.Status{State: service.StateBroken, Running: true},
			line:           "Service: running",
			lineVisible:    true,
			installVisible: false,
			actionsVisible: true,
		},
		{
			// The read failed: the menu must not claim a machine
			// without a service, must not invent a state word, and
			// must not offer an action on a state that was never
			// settled — the submenu is empty.
			name:    "unreadable registration",
			readErr: errors.New("the service control manager did not answer"),
		},
		{
			name:   "state the supervisor could not report",
			status: service.Status{State: service.StateUnknown},
		},
		{
			name:   "empty state",
			status: service.Status{},
		},
		{
			// The error decides alone: a status beside it is not a
			// reading the menu may act on.
			name:    "failed read beside a state",
			status:  service.Status{State: service.StateNotInstalled},
			readErr: errors.New("the service control manager did not answer"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := serviceMenuFor(tc.status, tc.readErr)
			if state.Line != tc.line {
				t.Errorf("line = %q, want %q", state.Line, tc.line)
			}
			if state.LineVisible != tc.lineVisible {
				t.Errorf("line visible = %t, want %t", state.LineVisible, tc.lineVisible)
			}
			if state.InstallVisible != tc.installVisible {
				t.Errorf("install visible = %t, want %t", state.InstallVisible, tc.installVisible)
			}
			if state.ActionsVisible != tc.actionsVisible {
				t.Errorf("lifecycle items visible = %t, want %t", state.ActionsVisible, tc.actionsVisible)
			}

			// Of the two action groups — install and the lifecycle
			// group — at most one is on screen, and for a read that
			// proved a class exactly one: a visible install item beside
			// a registration would be the dead second item this menu
			// removed, and a lifecycle item beside a proven absence
			// would act on nothing.
			groups := 0
			for _, visible := range []bool{state.InstallVisible, state.ActionsVisible} {
				if visible {
					groups++
				}
			}
			if tc.lineVisible && groups != 1 {
				t.Errorf("%d action groups are visible beside a stated line, want exactly one: %+v", groups, state)
			}
			if !tc.lineVisible && groups != 0 {
				t.Errorf("%d action groups are visible beside no line, want none: the read proved nothing to act on: %+v",
					groups, state)
			}

			// The line and the action groups are one claim: a state that
			// was read shows the line and the group of that state, a read
			// that proved no state shows nothing at all — never an
			// "unknown" row to leave the user in.
			if state.LineVisible && state.Line == "" {
				t.Error("the line is visible but carries no word")
			}
			if !state.LineVisible && state.Line != "" {
				t.Errorf("the line is hidden but still carries %q", state.Line)
			}

			// The line of the menu and the line a standalone read
			// shows are the same words.
			if line := serviceStatusLine(tc.status, tc.readErr); line != state.Line {
				t.Errorf("serviceStatusLine = %q, want the menu line %q", line, state.Line)
			}
		})
	}
}

// TestServiceStatusLineVocabulary pins the ONE axis of the tray line: the
// words are exactly the user statuses of the machine, a broken registration
// reads with the failed status, and nothing else — no "installed" (which
// mixed "a registration exists" with "it runs") and no "unknown" row (a read
// that failed has no state to name, so it shows no line).
func TestServiceStatusLineVocabulary(t *testing.T) {
	for _, tc := range []struct {
		status service.Status
		want   string
	}{
		{service.Status{State: service.StateNotInstalled}, "Service: not installed"},
		{service.Status{State: service.StateStopped}, "Service: stopped"},
		{service.Status{State: service.StateStarting}, "Service: starting"},
		{service.Status{State: service.StateRunning}, "Service: running"},
		{service.Status{State: service.StateStopping}, "Service: stopping"},
		{service.Status{State: service.StateFailed}, "Service: failed"},
		{service.Status{State: service.StateBroken}, "Service: failed"},
	} {
		if got := serviceStatusLine(tc.status, nil); got != tc.want {
			t.Errorf("the line of %q (running=%t) = %q, want %q",
				tc.status.State, tc.status.Running, got, tc.want)
		}
		if got := serviceStatusLine(tc.status, nil); got == "Service: installed" ||
			got == "Service: unknown" || got == "Service: broken" {
			t.Errorf("the line of %q carries the retired word %q", tc.status.State, got)
		}
	}

	// No read that proved nothing produces a word: the submenu shows
	// nothing at all until a read settles a class of machine.
	for _, tc := range []struct {
		status  service.Status
		readErr error
	}{
		{service.Status{}, nil},
		{service.Status{State: service.StateUnknown}, nil},
		{service.Status{}, errors.New("the service control manager did not answer")},
	} {
		if got := serviceStatusLine(tc.status, tc.readErr); got != "" {
			t.Errorf("the line of an unsettled read = %q, want none", got)
		}
	}
}

// TestServiceMenuUnsettledReadShowsNothing pins the surface of a failed read:
// NO line and NO item of the submenu — install, start, stop, update, delete
// and Open HFDesk all hidden — because no action may be offered without a
// registration that was read, and "not installed" is a claim that read did
// not prove. Nothing in the menu asks the user to retry: the read policy
// retries by itself and the ordinary cadence brings the state back.
func TestServiceMenuUnsettledReadShowsNothing(t *testing.T) {
	state := serviceMenuFor(service.Status{}, errors.New("the service control manager did not answer"))

	if state != (serviceMenuState{}) {
		t.Errorf("the unsettled read produced %+v, want the empty state: no line and no item", state)
	}
	if state.Line == "Service: not installed" {
		t.Error("the unsettled read claims a machine without a registration")
	}
	if state.InstallVisible || state.ActionsVisible || state.StartVisible || state.OpenVisible {
		t.Errorf("the unsettled read offers install=%t actions=%t start=%t open=%t, want none",
			state.InstallVisible, state.ActionsVisible, state.StartVisible, state.OpenVisible)
	}
	// Nothing to put inside the submenu means no ROOT entry either: an
	// empty submenu is the same dead surface as an "unknown" row.
	if state.showsSomething() {
		t.Errorf("the unsettled read reports something to show (%+v), want nothing at all", state)
	}
}

// TestServiceMenuReturnsWithTheFirstSuccessfulRead pins the other half of
// the decision: nothing of the menu is latched. The very next read that
// settles a class of machine shows its line and its items again — the state
// the automatic retry and the ordinary tick deliver without any user step.
func TestServiceMenuReturnsWithTheFirstSuccessfulRead(t *testing.T) {
	failed := serviceMenuFor(service.Status{}, errors.New("the service control manager did not answer"))
	if failed.LineVisible || failed.ActionsVisible || failed.InstallVisible {
		t.Fatalf("the unsettled read produced a visible menu: %+v", failed)
	}

	recovered := serviceMenuFor(service.Status{State: service.StateStopped}, nil)
	if !recovered.LineVisible || recovered.Line != "Service: stopped" {
		t.Errorf("the read after the failure shows the line %q (visible=%t), want %q",
			recovered.Line, recovered.LineVisible, "Service: stopped")
	}
	if !recovered.ActionsVisible || !recovered.StartVisible || !recovered.StartEnabled ||
		recovered.StopEnabled || !recovered.UpdateEnabled || !recovered.DeleteEnabled {
		t.Errorf("the read after the failure does not restore the lifecycle items: %+v", recovered)
	}
	if recovered.InstallVisible {
		t.Error("the read after the failure shows the install item beside a proven registration")
	}

	absent := serviceMenuFor(service.Status{State: service.StateNotInstalled}, nil)
	if !absent.LineVisible || !absent.InstallVisible || absent.ActionsVisible {
		t.Errorf("an absence read after the failure produced %+v, want the absence line and the install item", absent)
	}
}

// TestServiceMenuEntryHidesOnlyForAnUnsettledRead pins the claim behind the
// ROOT Service entry: every read that settled a class of machine has
// something to show inside the submenu (a line, the install item or the
// lifecycle items), and only a read that proved nothing has none — that is
// the one case in which the entry itself disappears from the menu, so an
// empty submenu is never left behind.
func TestServiceMenuEntryHidesOnlyForAnUnsettledRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  service.Status
		readErr error
	}{
		{name: "no registration", status: service.Status{State: service.StateNotInstalled}},
		{name: "stopped", status: service.Status{State: service.StateStopped}},
		{name: "failed", status: service.Status{State: service.StateFailed}},
		{name: "broken", status: service.Status{State: service.StateBroken}},
		{name: "running", status: service.Status{State: service.StateRunning, Running: true}},
		{name: "starting", status: service.Status{State: service.StateStarting}},
		{name: "stopping", status: service.Status{State: service.StateStopping}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if state := serviceMenuFor(tc.status, tc.readErr); !state.showsSomething() {
				t.Errorf("the %s read has nothing to show (%+v): the root entry would hide beside a state that was read",
					tc.name, state)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		status  service.Status
		readErr error
	}{
		{name: "empty state", status: service.Status{}},
		{name: "state the supervisor could not report", status: service.Status{State: service.StateUnknown}},
		{name: "failed read", readErr: errors.New("the service control manager did not answer")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if state := serviceMenuFor(tc.status, tc.readErr); state.showsSomething() {
				t.Errorf("the unsettled read (%s) reports something to show (%+v), want nothing: the entry hides",
					tc.name, state)
			}
		})
	}
}

// TestServiceMenuBrokenHidesStartAndKeepsUpdate pins the repair of a broken
// registration: its program is gone, so a start would fail on exactly that
// fact and is not offered at all (hidden, not greyed); the update re-registers
// the service from this folder and is the one action that repairs it. The
// desktop Logs window offers the same step for the same state.
func TestServiceMenuBrokenHidesStartAndKeepsUpdate(t *testing.T) {
	for _, status := range []service.Status{
		{State: service.StateBroken},
		{State: service.StateBroken, Running: true},
	} {
		state := serviceMenuFor(status, nil)
		if state.Line != "Service: failed" && state.Line != "Service: running" {
			t.Errorf("the line of a broken registration (running=%t) = %q, want the failed or the running fact",
				status.Running, state.Line)
		}
		if state.StartVisible {
			t.Errorf("a broken registration (running=%t) shows the start item, whose start would fail on the missing program",
				status.Running)
		}
		if state.StartEnabled {
			t.Errorf("a broken registration (running=%t) enables the start item", status.Running)
		}
		if !state.ActionsVisible || !state.UpdateEnabled || !state.DeleteEnabled {
			t.Errorf("a broken registration (running=%t) offers update=%t delete=%t (group visible=%t), want both: "+
				"the update is the repair",
				status.Running, state.UpdateEnabled, state.DeleteEnabled, state.ActionsVisible)
		}
		if status.Running && !state.StopEnabled {
			t.Error("a broken registration whose instance runs does not offer the stop of that instance")
		}
	}
}

// TestServiceMenuFailedKeepsStartAndUpdate pins the other settled state that
// does not run: a failed instance is a start candidate — its program is
// there — and the update stays a sensible second answer.
func TestServiceMenuFailedKeepsStartAndUpdate(t *testing.T) {
	state := serviceMenuFor(service.Status{State: service.StateFailed}, nil)
	if state.Line != "Service: failed" {
		t.Errorf("the line = %q, want %q", state.Line, "Service: failed")
	}
	if !state.StartVisible || !state.StartEnabled {
		t.Errorf("a failed registration shows start=%t enabled=%t, want both", state.StartVisible, state.StartEnabled)
	}
	if !state.UpdateEnabled {
		t.Error("a failed registration offers no update")
	}
	if state.StopEnabled {
		t.Error("a failed registration that does not run offers the stop")
	}
}

// TestServiceMenuOpenItemFollowsRegistrationAndRunningFact pins the map of
// the Open HFDesk item: it belongs into the menu only beside a PROVEN
// registration (a proven absence and an unsettled read hide it — an item
// that would open nothing never sits there), and inside a registration it
// acts only while the service serves the address — the supervisor's running
// fact, the same classification the stop item follows — and stays grey for a
// registered service that does not run. The desktop host opens the
// address of its OWN server, so apply leaves its open item out of this
// mapping (openFollowsService is the companion's flag, pinned by the apply
// test of the companion menu).
func TestServiceMenuOpenItemFollowsRegistrationAndRunningFact(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  service.Status
		readErr error
		visible bool
		enabled bool
	}{
		{
			name:    "registered and running: visible and enabled",
			status:  service.Status{State: service.StateRunning, Running: true},
			visible: true, enabled: true,
		},
		{
			name:    "registered and stopped: visible but grey — nothing serves the address yet",
			status:  service.Status{State: service.StateStopped},
			visible: true, enabled: false,
		},
		{
			// The supervisor's running fact decides: a pending
			// start counts as running, the very classification
			// every other surface (the stop item included) uses
			// for "the service runs".
			name:    "a pending start counts as running: the established running fact enables it",
			status:  service.Status{State: service.StateStarting},
			visible: true, enabled: true,
		},
		{
			// A broken registration whose instance still serves has
			// the running fact too, so the address answers and the
			// item opens it.
			name:    "a broken registration with a running instance: visible and enabled",
			status:  service.Status{State: service.StateBroken, Running: true},
			visible: true, enabled: true,
		},
		{
			name:    "proven absence: hidden",
			status:  service.Status{State: service.StateNotInstalled},
			visible: false, enabled: false,
		},
		{
			name:    "unsettled read: hidden — no address was proven",
			readErr: errors.New("the service control manager did not answer"),
			visible: false, enabled: false,
		},
		{
			name:    "state the supervisor could not report: hidden",
			status:  service.Status{State: service.StateUnknown},
			visible: false, enabled: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := serviceMenuFor(tc.status, tc.readErr)
			if state.OpenVisible != tc.visible {
				t.Errorf("Open HFDesk visible = %t, want %t", state.OpenVisible, tc.visible)
			}
			if state.OpenEnabled != tc.enabled {
				t.Errorf("Open HFDesk enabled = %t, want %t", state.OpenEnabled, tc.enabled)
			}
			// An enabled open item always sits behind a proven
			// registration: the two claims can never disagree.
			if state.OpenEnabled && !state.OpenVisible {
				t.Errorf("Open HFDesk is enabled although it is hidden: %+v", state)
			}
		})
	}
}

// TestServiceMenuLifecycleItemsPerState pins the enablement map of the
// lifecycle items of the Service submenu (start, stop, update, delete):
// which of them are visible and enabled for every class of read, on the
// classifications every other surface already decides with.
//
// The chosen mapping, pinned here on purpose:
//
//   - visibility follows the registration claim alone: a proven
//     registration shows the group, a proven absence and an unsettled read
//     hide it — a destructive action is never offered without a state that
//     was read;
//   - the start item has its own visibility, because a BROKEN registration
//     cannot be started: its program is gone, so the start hides and the
//     update is the one repair. Every other proven registration shows it;
//   - INSIDE a proven registration every action needs a SETTLED state:
//     start is enabled on a settled state that does not run (stopped,
//     failed), stop on a settled state whose running fact stands (a
//     pending start counts as running only once it settled into one),
//     update and delete on the settled registration alone;
//   - a TRANSITION (a pending start or stop) greys the whole group until
//     the reading settles again — the rule of the service gate, "a
//     pending state is not a state to decide on", applied to every
//     action instead of only to start;
//   - unknown claims no action at all (the honesty pin of the line
//     above), and the re-read is its only item.
func TestServiceMenuLifecycleItemsPerState(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         service.Status
		readErr        error
		visible        bool
		startVisible   bool
		start, stop    bool
		update, delete bool
	}{
		{
			name: "registered and running", visible: true, startVisible: true,
			status: service.Status{State: service.StateRunning, Running: true},
			stop:   true, update: true, delete: true,
		},
		{
			name: "registered and stopped", visible: true, startVisible: true,
			status: service.Status{State: service.StateStopped},
			start:  true, update: true, delete: true,
		},
		{
			// The supervisor's running fact wins over the diagnosed
			// state: such a service serves, so stop is the action and
			// start is not offered beside it.
			name: "stopped with the running fact", visible: true, startVisible: true,
			status: service.Status{State: service.StateStopped, Running: true},
			stop:   true, update: true, delete: true,
		},
		{
			name: "failed", visible: true, startVisible: true,
			status: service.Status{State: service.StateFailed},
			start:  true, update: true, delete: true,
		},
		{
			// The start of a broken registration would fail on the
			// missing program: the item is not on screen at all, and
			// the update is the repair.
			name: "broken without an instance", visible: true, startVisible: false,
			status: service.Status{State: service.StateBroken},
			update: true, delete: true,
		},
		{
			name: "broken but running", visible: true, startVisible: false,
			status: service.Status{State: service.StateBroken, Running: true},
			stop:   true, update: true, delete: true,
		},
		{
			// On its way up nothing is decided: the group stays on
			// screen — the registration is proven — but every action
			// is grey until the transition ends.
			name: "on its way up", visible: true, startVisible: true,
			status: service.Status{State: service.StateStarting},
		},
		{
			// On its way down the same rule: no action decides on a
			// state that is no result yet.
			name: "on its way down", visible: true, startVisible: true,
			status: service.Status{State: service.StateStopping},
		},
		{
			name:   "not registered",
			status: service.Status{State: service.StateNotInstalled},
		},
		{
			name:    "unreadable registration",
			readErr: errors.New("the service control manager did not answer"),
		},
		{
			name:   "state the supervisor could not report",
			status: service.Status{State: service.StateUnknown},
		},
		{
			name:   "empty state",
			status: service.Status{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := serviceMenuFor(tc.status, tc.readErr)
			if state.ActionsVisible != tc.visible {
				t.Errorf("lifecycle items visible = %t, want %t", state.ActionsVisible, tc.visible)
			}
			if state.StartVisible != tc.startVisible {
				t.Errorf("start visible = %t, want %t", state.StartVisible, tc.startVisible)
			}
			if state.StartEnabled != tc.start {
				t.Errorf("start enabled = %t, want %t", state.StartEnabled, tc.start)
			}
			if state.StopEnabled != tc.stop {
				t.Errorf("stop enabled = %t, want %t", state.StopEnabled, tc.stop)
			}
			if state.UpdateEnabled != tc.update {
				t.Errorf("update enabled = %t, want %t", state.UpdateEnabled, tc.update)
			}
			if state.DeleteEnabled != tc.delete {
				t.Errorf("delete enabled = %t, want %t", state.DeleteEnabled, tc.delete)
			}

			// A destructive action never leaves the visibility claim:
			// whatever a state enables, an unsettled read shows none of
			// the items — the enablement flags may only be set where the
			// group is visible at all.
			if !state.ActionsVisible && (state.StartEnabled || state.StopEnabled ||
				state.UpdateEnabled || state.DeleteEnabled) {
				t.Errorf("an item is enabled although the group is hidden: %+v", state)
			}
			// A hidden start item never stays enabled beside a visible
			// group: the two visibility claims are separate, the
			// enablement cannot contradict either.
			if !state.StartVisible && state.StartEnabled {
				t.Errorf("the start item is enabled although it is hidden: %+v", state)
			}
		})
	}
}
