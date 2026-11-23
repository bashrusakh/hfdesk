// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

// trayMenuStyle selects which items the tray menu carries.
//
// The desktop host and the tray companion share one menu machinery (the
// status line, the enable-per-state mapping, the catch-up refresh), and
// they differ in exactly one declarative fact: which items exist. The
// plan is a pure function of the style, so a test can pin both
// compositions without a notification area.
type trayMenuStyle int

// trayServiceStatusTooltip is the tooltip of the status line.
const trayServiceStatusTooltip = "The Windows service registration of this machine"

const (
	// trayMenuFull is the menu of the desktop host: the open item, the
	// window item, the service section with the install item, and quit.
	trayMenuFull trayMenuStyle = iota
	// trayMenuCompanion is the menu of the tray companion — the strictly
	// decided composition: the status line, the service group and the
	// item that opens the Runtime / Logs window. The service group
	// follows the absence rule: beside a PROVEN registration the four
	// lifecycle actions (Start/Stop/Update/Delete) show, beside a proven
	// ABSENCE the Install item shows, and an unsettled read offers
	// neither — an item that cannot act never clutters the menu (apply
	// owns that visibility decision; the plan lists every item the style
	// can show at all). There is NO permanent open, install, dialog or
	// quit item: Open HFDesk and an exit item await the user's decision,
	// the registered-service dialog belongs to the desktop host, and
	// Install exists in the menu only through the absence rule.
	trayMenuCompanion
)

// trayMenuKind names one kind of tray menu item, independent of its text.
type trayMenuKind int

const (
	trayItemOpen trayMenuKind = iota
	trayItemRuntime
	trayItemLine
	trayItemInstall
	trayItemStart
	trayItemStop
	trayItemUpdate
	trayItemDelete
	trayItemService
	trayItemSeparator
	trayItemQuit
)

// trayMenuItemSpec is one item of the tray menu before it exists: the
// kind the runner wires it by, the title it is created with and its
// tooltip. The status line carries no title here — the runner creates it
// with the status line of the first read, so the menu is already right
// when it appears.
type trayMenuItemSpec struct {
	Kind    trayMenuKind
	Title   string
	Tooltip string
}

// trayMenuPlan returns the items of the tray menu of the given style, in
// display order.
//
// The full plan is the composition the desktop host has always had, kept
// byte for byte: the order of the actions is the order of the items. The
// companion plan is the decided strict composition — status line,
// lifecycle actions, Runtime / Logs — and nothing else.
func trayMenuPlan(style trayMenuStyle) []trayMenuItemSpec {
	if style == trayMenuCompanion {
		return []trayMenuItemSpec{
			{Kind: trayItemLine, Tooltip: trayServiceStatusTooltip},
			{Kind: trayItemInstall, Title: "Install service...", Tooltip: "Register HFDesk as a Windows service that starts at boot"},
			{Kind: trayItemStart, Title: "Start service", Tooltip: "Start the registered Windows service"},
			{Kind: trayItemStop, Title: "Stop service", Tooltip: "Stop the running Windows service"},
			{Kind: trayItemUpdate, Title: "Update service", Tooltip: "Re-register the Windows service from the folder of this app"},
			{Kind: trayItemDelete, Title: "Delete service", Tooltip: "Remove the Windows service registration and keep all data"},
			{Kind: trayItemRuntime, Title: "Runtime / Logs", Tooltip: "Show the runtime status and the log window"},
		}
	}
	return []trayMenuItemSpec{
		{Kind: trayItemOpen, Title: "Open HFDesk", Tooltip: "Open the HFDesk web UI in the default browser"},
		{Kind: trayItemRuntime, Title: "Runtime / Logs", Tooltip: "Show the runtime status and the log window"},
		{Kind: trayItemLine, Tooltip: trayServiceStatusTooltip},
		{Kind: trayItemInstall, Title: "Install service...", Tooltip: "Register HFDesk as a Windows service that starts at boot"},
		{Kind: trayItemStart, Title: "Start service", Tooltip: "Start the registered Windows service"},
		{Kind: trayItemStop, Title: "Stop service", Tooltip: "Stop the running Windows service"},
		{Kind: trayItemUpdate, Title: "Update service", Tooltip: "Re-register the Windows service from the folder of this app"},
		{Kind: trayItemDelete, Title: "Delete service", Tooltip: "Remove the Windows service registration and keep all data"},
		{Kind: trayItemService, Title: "Service...", Tooltip: "Open, start or update the installed Windows service"},
		{Kind: trayItemSeparator},
		{Kind: trayItemQuit, Title: "Quit HFDesk", Tooltip: "Stop HFDesk and close the local server"},
	}
}
