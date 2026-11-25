// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/service"
	"github.com/bashrusakh/hfdesk/internal/serviceview"
)

// TestServiceMenuMatchesTheSharedView pins the tray menu to the one table of
// the application: for every read, the menu fields are exactly the shared
// serviceview snapshot translated into the menu shape. The tray is an
// adapter, not a second table, so the menu and the Runtime window cannot
// describe the same machine differently.
func TestServiceMenuMatchesTheSharedView(t *testing.T) {
	readFailure := errors.New("the service control manager did not answer")

	for _, tc := range []struct {
		name    string
		status  service.Status
		readErr error
	}{
		{name: "no registration", status: service.Status{State: service.StateNotInstalled}},
		{name: "running", status: service.Status{State: service.StateRunning}},
		{name: "running beside its fact", status: service.Status{State: service.StateRunning, Running: true}},
		{name: "stopped", status: service.Status{State: service.StateStopped}},
		{name: "stopped beside the running fact", status: service.Status{State: service.StateStopped, Running: true}},
		{name: "starting", status: service.Status{State: service.StateStarting}},
		{name: "stopping", status: service.Status{State: service.StateStopping}},
		{name: "failed", status: service.Status{State: service.StateFailed}},
		{name: "broken", status: service.Status{State: service.StateBroken}},
		{name: "broken beside its running instance", status: service.Status{State: service.StateBroken, Running: true}},
		{name: "masked", status: service.Status{State: service.StateMasked}},
		{name: "masked runtime", status: service.Status{State: service.StateMaskedRuntime}},
		{name: "unknown word", status: service.Status{State: service.StateUnknown}},
		{name: "empty state", status: service.Status{}},
		{name: "failed read", readErr: readFailure},
		{name: "failed read beside a state", status: service.Status{State: service.StateNotInstalled}, readErr: readFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := serviceview.From(string(tc.status.State), tc.status.Running, tc.readErr != nil)
			menu := serviceMenuFor(tc.status, tc.readErr)

			if menu.Line != view.Label {
				t.Errorf("line = %q, want the shared label %q", menu.Line, view.Label)
			}
			if menu.LineVisible != (view.Label != "") {
				t.Errorf("line visible = %t, want %t (label %q)",
					menu.LineVisible, view.Label != "", view.Label)
			}
			if menu.InstallVisible != view.Actions.Install.Visible {
				t.Errorf("install visible = %t, want %t",
					menu.InstallVisible, view.Actions.Install.Visible)
			}
			if menu.ActionsVisible != view.Actions.Stop.Visible {
				t.Errorf("lifecycle group visible = %t, want %t",
					menu.ActionsVisible, view.Actions.Stop.Visible)
			}
			if menu.OpenVisible != view.Actions.Open.Visible {
				t.Errorf("open visible = %t, want %t",
					menu.OpenVisible, view.Actions.Open.Visible)
			}
			if menu.OpenEnabled != view.Actions.Open.Enabled {
				t.Errorf("open enabled = %t, want %t",
					menu.OpenEnabled, view.Actions.Open.Enabled)
			}
			if menu.StartVisible != view.Actions.Start.Visible {
				t.Errorf("start visible = %t, want %t",
					menu.StartVisible, view.Actions.Start.Visible)
			}
			if menu.StartEnabled != view.Actions.Start.Enabled {
				t.Errorf("start enabled = %t, want %t",
					menu.StartEnabled, view.Actions.Start.Enabled)
			}
			if menu.StopEnabled != view.Actions.Stop.Enabled {
				t.Errorf("stop enabled = %t, want %t",
					menu.StopEnabled, view.Actions.Stop.Enabled)
			}
			if menu.UpdateEnabled != view.Actions.Update.Enabled {
				t.Errorf("update enabled = %t, want %t",
					menu.UpdateEnabled, view.Actions.Update.Enabled)
			}
			if menu.DeleteEnabled != view.Actions.Delete.Enabled {
				t.Errorf("delete enabled = %t, want %t",
					menu.DeleteEnabled, view.Actions.Delete.Enabled)
			}
		})
	}
}
