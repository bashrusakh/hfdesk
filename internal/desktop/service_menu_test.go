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
			line:           "Service: installed (running)",
			installEnabled: false,
			serviceVisible: true,
		},
		{
			name:           "stopped service",
			status:         service.Status{State: service.StateStopped},
			line:           "Service: installed (stopped)",
			installEnabled: false,
			serviceVisible: true,
		},
		{
			name:           "service on its way up",
			status:         service.Status{State: service.StateStarting},
			line:           "Service: installed (starting)",
			installEnabled: false,
			serviceVisible: true,
		},
		{
			name:           "failed service",
			status:         service.Status{State: service.StateFailed},
			line:           "Service: installed (failed)",
			installEnabled: false,
			serviceVisible: true,
		},
		{
			// A registration whose program is gone is still a
			// registration: the diagnosis is the state word of the
			// line, and the items stay the ones of an installed
			// service.
			name:           "broken registration",
			status:         service.Status{State: service.StateBroken},
			line:           "Service: installed (broken)",
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
