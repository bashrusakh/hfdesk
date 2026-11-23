// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// TestServiceMenuForOneRead proves that one read of the registration maps
// onto the status line and the two items of the tray menu exactly as the
// surface promises:
//
//   - a proven absence enables the install item and hides the service item,
//     so a machine without a service shows no dead second item,
//   - a proven registration greys the install item and shows the
//     registered-service item beside it,
//   - and an unsettled read claims neither: the line says unknown and the
//     install item is greyed, because "no service is installed" was not
//     proven by a read that failed.
func TestServiceMenuForOneRead(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         service.Status
		readErr        error
		line           string
		installEnabled bool
		serviceVisible bool
	}{
		{
			name:           "no registration",
			status:         service.Status{State: service.StateNotInstalled},
			line:           "Service: not installed",
			installEnabled: true,
			serviceVisible: false,
		},
		{
			name:           "running service",
			status:         service.Status{State: service.StateRunning, Running: true},
			line:           "Service: installed",
			installEnabled: false,
			serviceVisible: true,
		},
		{
			name:           "stopped service",
			status:         service.Status{State: service.StateStopped},
			line:           "Service: installed",
			installEnabled: false,
			serviceVisible: true,
		},
		{
			name:           "service on its way up",
			status:         service.Status{State: service.StateStarting},
			line:           "Service: installed",
			installEnabled: false,
			serviceVisible: true,
		},
		{
			name:           "failed service",
			status:         service.Status{State: service.StateFailed},
			line:           "Service: installed",
			installEnabled: false,
			serviceVisible: true,
		},
		{
			// A registration whose program is gone is still a
			// registration: the line still says installed — it never
			// carried a state word — and the items stay the ones of an
			// installed service.
			name:           "broken registration",
			status:         service.Status{State: service.StateBroken},
			line:           "Service: installed",
			installEnabled: false,
			serviceVisible: true,
		},
		{
			// The read failed: the line must not claim a machine
			// without a service, and no item may act on a state that
			// was never settled.
			name:           "unreadable registration",
			readErr:        errors.New("the service control manager did not answer"),
			line:           "Service: unknown",
			installEnabled: false,
			serviceVisible: false,
		},
		{
			name:           "state the supervisor could not report",
			status:         service.Status{State: service.StateUnknown},
			line:           "Service: unknown",
			installEnabled: false,
			serviceVisible: false,
		},
		{
			name:           "empty state",
			status:         service.Status{},
			line:           "Service: unknown",
			installEnabled: false,
			serviceVisible: false,
		},
		{
			// The error decides alone: a status beside it is not a
			// reading the menu may act on.
			name:           "failed read beside a state",
			status:         service.Status{State: service.StateNotInstalled},
			readErr:        errors.New("the service control manager did not answer"),
			line:           "Service: unknown",
			installEnabled: false,
			serviceVisible: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := serviceMenuFor(tc.status, tc.readErr)
			if state.Line != tc.line {
				t.Errorf("line = %q, want %q", state.Line, tc.line)
			}
			if state.InstallEnabled != tc.installEnabled {
				t.Errorf("install enabled = %t, want %t", state.InstallEnabled, tc.installEnabled)
			}
			if state.ServiceVisible != tc.serviceVisible {
				t.Errorf("service item visible = %t, want %t", state.ServiceVisible, tc.serviceVisible)
			}

			// The line of the menu and the line a standalone read
			// shows are the same words.
			if line := serviceStatusLine(tc.status, tc.readErr); line != state.Line {
				t.Errorf("serviceStatusLine = %q, want the menu line %q", line, state.Line)
			}
		})
	}
}

// TestServiceMenuLifecycleItemsPerState pins the enablement map of the four
// lifecycle items of the tray menu (start, stop, update, delete): which of
// them are visible and enabled for every class of read, on the
// classifications every other surface already decides with.
//
// The chosen mapping, pinned here on purpose:
//
//   - visibility follows the registration claim alone: a proven
//     registration shows the whole section, a proven absence and an
//     unsettled read hide it — a destructive action is never offered
//     without a state that was read;
//   - start is enabled on a settled state that does not run (stopped,
//     failed, broken without an instance) and stays grey during a
//     transition — a pending state is not a state to decide on, the rule
//     of the service gate;
//   - stop follows the running fact, where a pending start counts as
//     running and a pending stop does not — the same classification the
//     gate's first button and the reports use;
//   - update and delete follow the registration alone: the update
//     re-points it and the removal stops a running instance inside
//     Manager.Uninstall as part of the action.
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
			// On its way up the instance counts as running (stop is
			// offered) but the state is no result yet, so start stays
			// grey.
			name: "on its way up", visible: true,
			status: service.Status{State: service.StateStarting},
			stop:   true, update: true, delete: true,
		},
		{
			// On its way down it neither runs nor settled: both
			// lifecycle actions stay grey until the transition ends.
			name: "on its way down", visible: true,
			status: service.Status{State: service.StateStopping},
			update: true, delete: true,
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
			if state.ServiceVisible != tc.visible {
				t.Errorf("lifecycle items visible = %t, want %t", state.ServiceVisible, tc.visible)
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
			// section is visible at all.
			if !state.ServiceVisible && (state.StartEnabled || state.StopEnabled ||
				state.UpdateEnabled || state.DeleteEnabled) {
				t.Errorf("an item is enabled although the section is hidden: %+v", state)
			}
		})
	}
}
