// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import "testing"

// TestTrayMenuPlanCompanionIsTheDecidedComposition pins the composition of
// the tray companion menu: the status line, the service group (the install
// item of the absence rule beside the four lifecycle actions) and the
// Runtime / Logs item — in that order — and NOTHING else: no open item, no
// registered-service dialog item, no separator and no quit item (the first
// and the last await the user's decision). WHICH of the service-group items
// is on screen for one read is apply's absence-rule decision (pinned by the
// apply test of the companion menu); the plan lists every item this style
// can show at all. The titles are the very titles the desktop menu uses,
// so one vocabulary serves both hosts.
func TestTrayMenuPlanCompanionIsTheDecidedComposition(t *testing.T) {
	plan := trayMenuPlan(trayMenuCompanion)

	wantKinds := []trayMenuKind{
		trayItemLine, trayItemInstall, trayItemStart, trayItemStop, trayItemUpdate, trayItemDelete, trayItemRuntime,
	}
	if len(plan) != len(wantKinds) {
		t.Fatalf("the companion plan has %d items (%v), want exactly %d: %v",
			len(plan), planKinds(plan), len(wantKinds), wantKinds)
	}
	for i, want := range wantKinds {
		if plan[i].Kind != want {
			t.Errorf("the companion item %d is %d, want %d: the plan is %v",
				i, plan[i].Kind, want, planKinds(plan))
		}
	}

	// The forbidden items must not appear anywhere in the plan: each one
	// pinned by its own diagnostic, so a silently added item fails the
	// item it resembles. Install is NOT forbidden — it is the absence
	// rule's item, shown and hidden by apply.
	for _, forbidden := range []struct {
		kind trayMenuKind
		name string
	}{
		{trayItemOpen, "Open HFDesk"},
		{trayItemService, "Service..."},
		{trayItemQuit, "Quit HFDesk"},
		{trayItemSeparator, "the separator"},
	} {
		for i, spec := range plan {
			if spec.Kind == forbidden.kind {
				t.Errorf("the companion item %d is the %s item: the decided composition does not carry it", i, forbidden.name)
			}
		}
	}

	// The titles of the six fixed items; the status line carries none —
	// the runner creates it with the result of the first read, so the
	// line is right from the first moment the menu exists.
	wantTitles := map[trayMenuKind]string{
		trayItemInstall: "Install service...",
		trayItemStart:   "Start service",
		trayItemStop:    "Stop service",
		trayItemUpdate:  "Update service",
		trayItemDelete:  "Delete service",
		trayItemRuntime: "Runtime / Logs",
	}
	for _, spec := range plan {
		if spec.Kind == trayItemLine {
			if spec.Title != "" {
				t.Errorf("the status line carries the title %q, want it created from the first read", spec.Title)
			}
			if spec.Tooltip == "" {
				t.Error("the status line carries no tooltip")
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
}

// TestTrayMenuPlanFullKeepsTheDesktopComposition pins the OTHER plan byte
// for byte: the desktop host menu keeps every item it has always had, in
// its order — so the plan-driven runner cannot silently add, drop or move
// an item of the established menu while it learned to build two of them.
func TestTrayMenuPlanFullKeepsTheDesktopComposition(t *testing.T) {
	plan := trayMenuPlan(trayMenuFull)

	want := []struct {
		kind  trayMenuKind
		title string
	}{
		{trayItemOpen, "Open HFDesk"},
		{trayItemRuntime, "Runtime / Logs"},
		{trayItemLine, ""},
		{trayItemInstall, "Install service..."},
		{trayItemStart, "Start service"},
		{trayItemStop, "Stop service"},
		{trayItemUpdate, "Update service"},
		{trayItemDelete, "Delete service"},
		{trayItemService, "Service..."},
		{trayItemSeparator, ""},
		{trayItemQuit, "Quit HFDesk"},
	}
	if len(plan) != len(want) {
		t.Fatalf("the full plan has %d items (%v), want %d", len(plan), planKinds(plan), len(want))
	}
	for i, expected := range want {
		if plan[i].Kind != expected.kind {
			t.Errorf("the full item %d is %d, want %d: the plan is %v",
				i, plan[i].Kind, expected.kind, planKinds(plan))
		}
		if plan[i].Title != expected.title {
			t.Errorf("the full item %d is titled %q, want %q", i, plan[i].Title, expected.title)
		}
	}
	// The separator sits directly in front of quit: the quit group stays
	// the group a user reaches for last.
	if plan[len(plan)-2].Kind != trayItemSeparator || plan[len(plan)-1].Kind != trayItemQuit {
		t.Errorf("the plan ends with %v, want the separator directly in front of quit", planKinds(plan[len(plan)-2:]))
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
