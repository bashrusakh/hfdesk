// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// TestServiceMenuForOneRead proves that one read of the registration maps
// onto the status line and the two visibility claims of the Service
// submenu exactly as the surface promises:
//
//   - a proven absence shows the install item and hides the lifecycle
//     group, so a machine without a service shows no dead actions,
//   - a proven registration shows the lifecycle group and hides the
//     install item, so exactly one action group is ever on screen,
//   - a transition names itself in the line ("Service: starting",
//     "Service: stopping") while the registration is still proven,
//   - and an unsettled read claims neither: the line says unknown and no
//     action shows, because "no service is installed" was not proven by
//     a read that failed.
func TestServiceMenuForOneRead(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         service.Status
		readErr        error
		line           string
		installVisible bool
		actionsVisible bool
	}{
		{
			name:           "no registration",
			status:         service.Status{State: service.StateNotInstalled},
			line:           "Service: not installed",
			installVisible: true,
			actionsVisible: false,
		},
		{
			name:           "running service",
			status:         service.Status{State: service.StateRunning, Running: true},
			line:           "Service: running",
			installVisible: false,
			actionsVisible: true,
		},
		{
			name:           "stopped service",
			status:         service.Status{State: service.StateStopped},
			line:           "Service: installed",
			installVisible: false,
			actionsVisible: true,
		},
		{
			name:           "service on its way up",
			status:         service.Status{State: service.StateStarting},
			line:           "Service: starting",
			installVisible: false,
			actionsVisible: true,
		},
		{
			name:           "service on its way down",
			status:         service.Status{State: service.StateStopping},
			line:           "Service: stopping",
			installVisible: false,
			actionsVisible: true,
		},
		{
			name:           "failed service",
			status:         service.Status{State: service.StateFailed},
			line:           "Service: installed",
			installVisible: false,
			actionsVisible: true,
		},
		{
			// A registration whose program is gone is still a
			// registration: the line still says installed — it never
			// carried a state word — and the items stay the ones of an
			// installed service.
			name:           "broken registration",
			status:         service.Status{State: service.StateBroken},
			line:           "Service: installed",
			installVisible: false,
			actionsVisible: true,
		},
		{
			// The read failed: the line must not claim a machine
			// without a service, and no item may act on a state that
			// was never settled.
			name:           "unreadable registration",
			readErr:        errors.New("the service control manager did not answer"),
			line:           "Service: unknown",
			installVisible: false,
			actionsVisible: false,
		},
		{
			name:           "state the supervisor could not report",
			status:         service.Status{State: service.StateUnknown},
			line:           "Service: unknown",
			installVisible: false,
			actionsVisible: false,
		},
		{
			name:           "empty state",
			status:         service.Status{},
			line:           "Service: unknown",
			installVisible: false,
			actionsVisible: false,
		},
		{
			// The error decides alone: a status beside it is not a
			// reading the menu may act on.
			name:           "failed read beside a state",
			status:         service.Status{State: service.StateNotInstalled},
			readErr:        errors.New("the service control manager did not answer"),
			line:           "Service: unknown",
			installVisible: false,
			actionsVisible: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := serviceMenuFor(tc.status, tc.readErr)
			if state.Line != tc.line {
				t.Errorf("line = %q, want %q", state.Line, tc.line)
			}
			if state.InstallVisible != tc.installVisible {
				t.Errorf("install visible = %t, want %t", state.InstallVisible, tc.installVisible)
			}
			if state.ActionsVisible != tc.actionsVisible {
				t.Errorf("lifecycle items visible = %t, want %t", state.ActionsVisible, tc.actionsVisible)
			}

			// Exactly one action group is ever on screen: the install
			// item and the lifecycle group are each other's alternate.
			if state.InstallVisible && state.ActionsVisible {
				t.Errorf("both action groups are visible at once: %+v", state)
			}

			// The line of the menu and the line a standalone read
			// shows are the same words.
			if line := serviceStatusLine(tc.status, tc.readErr); line != state.Line {
				t.Errorf("serviceStatusLine = %q, want the menu line %q", line, state.Line)
			}
		})
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

// TestServiceMenuLifecycleItemsPerState pins the enablement map of the four
// lifecycle items of the Service submenu (start, stop, update, delete):
// which of them are visible and enabled for every class of read, on the
// classifications every other surface already decides with.
//
// The chosen mapping, pinned here on purpose:
//
//   - visibility follows the registration claim alone: a proven
//     registration shows the whole group, a proven absence and an
//     unsettled read hide it — a destructive action is never offered
//     without a state that was read;
//   - INSIDE a proven registration every action needs a SETTLED state:
//     start is enabled on a settled state that does not run (stopped,
//     failed, broken without an instance), stop on a settled state whose
//     running fact stands (a pending start counts as running only once
//     it settled into one), update and delete on the settled
//     registration alone;
//   - a TRANSITION (a pending start or stop) greys the whole group until
//     the reading settles again — the rule of the service gate, "a
//     pending state is not a state to decide on", applied to every
//     action instead of only to start;
//   - unknown claims no action at all (the honesty pin of the line
//     above), and auto re-reading continues outside this mapping.
func TestServiceMenuLifecycleItemsPerState(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         service.Status
		readErr        error
		visible        bool
		start, stop    bool
		update, delete bool
	}{
		{
			name: "registered and running", visible: true,
			status: service.Status{State: service.StateRunning, Running: true},
			stop:   true, update: true, delete: true,
		},
		{
			name: "registered and stopped", visible: true,
			status: service.Status{State: service.StateStopped},
			start:  true, update: true, delete: true,
		},
		{
			// The supervisor's running fact wins over the diagnosed
			// state: such a service serves, so stop is the action and
			// start is not offered beside it.
			name: "stopped with the running fact", visible: true,
			status: service.Status{State: service.StateStopped, Running: true},
			stop:   true, update: true, delete: true,
		},
		{
			name: "failed", visible: true,
			status: service.Status{State: service.StateFailed},
			start:  true, update: true, delete: true,
		},
		{
			name: "broken without an instance", visible: true,
			status: service.Status{State: service.StateBroken},
			start:  true, update: true, delete: true,
		},
		{
			name: "broken but running", visible: true,
			status: service.Status{State: service.StateBroken, Running: true},
			stop:   true, update: true, delete: true,
		},
		{
			// On its way up nothing is decided: the group stays on
			// screen — the registration is proven — but every action
			// is grey until the transition ends.
			name: "on its way up", visible: true,
			status: service.Status{State: service.StateStarting},
		},
		{
			// On its way down the same rule: no action decides on a
			// state that is no result yet.
			name: "on its way down", visible: true,
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
		})
	}
}
