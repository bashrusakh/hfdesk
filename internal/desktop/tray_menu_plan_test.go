// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import "testing"

// TestTrayMenuPlanRootsPinBothCompositions pins the ROOT of both tray
// menus: Open HFDesk, Logs, the Service entry, the separator and the
// style's own end item — in that order and nothing else. The whole
// service section (the status line and every service action) lives in
// the submenu serviceSubmenuPlan describes, so no root can ever show the
// install item beside the lifecycle actions: the state and exactly the
// actions of that state are one short list behind the Service entry.
// The shared titles are the very titles both roots use, so one
// vocabulary serves both hosts; Exit is the companion's own last item,
// the way Quit closes the desktop host.
func TestTrayMenuPlanRootsPinBothCompositions(t *testing.T) {
	for _, tc := range []struct {
		style     trayMenuStyle
		styleName string
		endTitle  string
	}{
		{style: trayMenuCompanion, styleName: "companion", endTitle: "Exit"},
		{style: trayMenuFull, styleName: "full", endTitle: "Quit HFDesk"},
	} {
		t.Run(tc.styleName, func(t *testing.T) {
			plan := trayMenuPlan(tc.style)

			wantKinds := []trayMenuKind{
				trayItemOpen, trayItemRuntime, trayItemService, trayItemSeparator, trayItemQuit,
			}
			if len(plan) != len(wantKinds) {
				t.Fatalf("the %s root has %d items (%v), want exactly %d: %v",
					tc.styleName, len(plan), planKinds(plan), len(wantKinds), wantKinds)
			}
			for i, want := range wantKinds {
				if plan[i].Kind != want {
					t.Errorf("the %s item %d is %d, want %d: the plan is %v",
						tc.styleName, i, plan[i].Kind, want, planKinds(plan))
				}
			}

			// No service action and no status line may appear in a
			// ROOT: those items belong to the Service submenu only —
			// an install item or a lifecycle action in the root is
			// exactly the duplicate layout the submenu removes.
			for _, forbidden := range []struct {
				kind trayMenuKind
				name string
			}{
				{trayItemLine, "the status line"},
				{trayItemInstall, "Install service..."},
				{trayItemStart, "Start service"},
				{trayItemStop, "Stop service"},
				{trayItemUpdate, "Update service"},
				{trayItemDelete, "Delete service"},
			} {
				for i, spec := range plan {
					if spec.Kind == forbidden.kind {
						t.Errorf("the %s item %d carries %s: it lives in the Service submenu, never in the root",
							tc.styleName, i, forbidden.name)
					}
				}
			}

			// The separator sits directly in front of the end item:
			// the exit group stays the group a user reaches for last.
			if plan[len(plan)-2].Kind != trayItemSeparator || plan[len(plan)-1].Kind != trayItemQuit {
				t.Errorf("the plan ends with %v, want the separator directly in front of the end item",
					planKinds(plan[len(plan)-2:]))
			}

			wantTitles := map[trayMenuKind]string{
				trayItemOpen:    "Open HFDesk",
				trayItemRuntime: "Logs",
				trayItemService: "Service",
				trayItemQuit:    tc.endTitle,
			}
			for _, spec := range plan {
				if spec.Kind == trayItemSeparator {
					if spec.Title != "" || spec.Tooltip != "" {
						t.Errorf("the separator carries text %q/%q, want none", spec.Title, spec.Tooltip)
					}
					continue
				}
				if want := wantTitles[spec.Kind]; spec.Title != want {
					t.Errorf("the item of kind %d is titled %q, want %q", spec.Kind, spec.Title, want)
				}
				if spec.Tooltip == "" {
					t.Errorf("the item %q carries no tooltip", spec.Title)
				}
			}
		})
	}
}

// TestServiceSubmenuPlanPinsTheSharedServiceSection pins the ONE submenu
// both styles build behind their Service entry: the status line first
// (created from the first read, disabled: it reports, it does not act),
// then Install service... and the four lifecycle actions — that order and
// nothing else: no dialog item, no separator, no end item inside the
// submenu. WHICH of these items are on screen for one read is apply's
// mode-times-state decision (pinned by the apply tests); the plan lists
// every item the submenu can show at all.
func TestServiceSubmenuPlanPinsTheSharedServiceSection(t *testing.T) {
	plan := serviceSubmenuPlan()

	want := []struct {
		kind  trayMenuKind
		title string
	}{
		{trayItemLine, ""},
		{trayItemInstall, "Install service..."},
		{trayItemStart, "Start service"},
		{trayItemStop, "Stop service"},
		{trayItemUpdate, "Update service"},
		{trayItemDelete, "Delete service"},
	}
	if len(plan) != len(want) {
		t.Fatalf("the Service submenu has %d items (%v), want %d", len(plan), planKinds(plan), len(want))
	}
	for i, expected := range want {
		if plan[i].Kind != expected.kind {
			t.Errorf("the submenu item %d is %d, want %d: the plan is %v",
				i, plan[i].Kind, expected.kind, planKinds(plan))
		}
		if plan[i].Title != expected.title {
			t.Errorf("the submenu item %d is titled %q, want %q", i, plan[i].Title, expected.title)
		}
	}
	for _, forbidden := range []struct {
		kind trayMenuKind
		name string
	}{
		{trayItemOpen, "Open HFDesk"},
		{trayItemRuntime, "Logs"},
		{trayItemSeparator, "the separator"},
		{trayItemQuit, "the end item"},
	} {
		for i, spec := range plan {
			if spec.Kind == forbidden.kind {
				t.Errorf("the submenu item %d is %s: the Service submenu carries only the status line and the service actions",
					i, forbidden.name)
			}
		}
	}
	// The status line carries no title (the runner creates it from the
	// first read) but does carry its tooltip; every action carries both.
	for _, spec := range plan {
		if spec.Kind == trayItemLine {
			if spec.Title != "" {
				t.Errorf("the status line carries the title %q, want it created from the first read", spec.Title)
			}
		} else if spec.Title == "" {
			t.Errorf("the submenu item of kind %d carries no title", spec.Kind)
		}
		if spec.Tooltip == "" {
			t.Errorf("the submenu item %q carries no tooltip", spec.Title)
		}
	}
}

// planKinds returns the kinds of the given items, for a failure message.
func planKinds(plan []trayMenuItemSpec) []trayMenuKind {
	kinds := make([]trayMenuKind, 0, len(plan))
	for _, spec := range plan {
		kinds = append(kinds, spec.Kind)
	}
	return kinds
}
