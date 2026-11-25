// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

// trayMenuStyle selects which items the tray menu carries.
//
// The desktop host and the tray companion share one menu machinery (the
// status line inside the Service submenu, the enable-per-state mapping,
// the catch-up refresh), and they differ in the declarative facts a test
// can pin: which item ends the menu (Quit closes the desktop host, Exit
// closes the companion alone) and how the Open HFDesk item follows the
// registration. The plan is a pure function of the style, so a test can
// pin both compositions without a notification area.
type trayMenuStyle int

// trayServiceStatusTooltip is the tooltip of the status line.
const trayServiceStatusTooltip = "The Windows service registration of this machine"

// trayServiceTooltip is the tooltip of the Service submenu parent: the
// one entry into the state of the Windows service both menus share.
const trayServiceTooltip = "Show the state of the Windows service and act on it"

const (
	// trayMenuFull is the root menu of the desktop host: the open item,
	// the Logs item, the Service submenu, the separator and Quit.
	trayMenuFull trayMenuStyle = iota
	// trayMenuCompanion is the root menu of the tray companion — the
	// decided composition: Open HFDesk, Logs, the Service submenu, the
	// separator and Exit — in that order (pinned by test). The whole
	// service section lives INSIDE the submenu: the root carries no
	// status line and no service action, so Install and the lifecycle
	// group can never sit beside each other in the root. Open HFDesk
	// opens the address of the REGISTRATION and follows the visibility
	// rule (visible beside a proven registration, enabled while the
	// service serves the address); Exit closes this companion alone and
	// is the last item of every companion menu, the way Quit closes the
	// desktop host. There is NO registered-service dialog item: that
	// dialog belongs to the install recheck of the desktop host.
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
// inside the Service submenu with the status line of the first read, so
// the menu is already right when it appears.
type trayMenuItemSpec struct {
	Kind    trayMenuKind
	Title   string
	Tooltip string
}

// trayMenuPlan returns the ROOT items of the tray menu of the given
// style, in display order.
//
// Both roots are the same three entries — Open HFDesk, Logs and the
// Service submenu — followed by the separator and the style's own end
// item. The service section (the status line and every service action)
// lives in the submenu serviceSubmenuPlan describes, so the root can
// never show the install item beside the lifecycle actions: the state
// and exactly the actions of that state are one short list behind the
// Service entry.
func trayMenuPlan(style trayMenuStyle) []trayMenuItemSpec {
	if style == trayMenuCompanion {
		return []trayMenuItemSpec{
			{Kind: trayItemOpen, Title: "Open HFDesk", Tooltip: "Open the web UI of the Windows service in the default browser"},
			{Kind: trayItemRuntime, Title: "Logs", Tooltip: "Show the runtime status and the log window"},
			{Kind: trayItemService, Title: "Service", Tooltip: trayServiceTooltip},
			{Kind: trayItemSeparator},
			{Kind: trayItemQuit, Title: "Exit", Tooltip: "Close the tray companion; the Windows service is not affected"},
		}
	}
	return []trayMenuItemSpec{
		{Kind: trayItemOpen, Title: "Open HFDesk", Tooltip: "Open the HFDesk web UI in the default browser"},
		{Kind: trayItemRuntime, Title: "Logs", Tooltip: "Show the runtime status and the log window"},
		{Kind: trayItemService, Title: "Service", Tooltip: trayServiceTooltip},
		{Kind: trayItemSeparator},
		{Kind: trayItemQuit, Title: "Quit HFDesk", Tooltip: "Stop HFDesk and close the local server"},
	}
}

// serviceSubmenuPlan returns the items of the Service submenu, in display
// order — one shared plan for both styles: the status line first
// (disabled: it reports, it does not act), then the install action and
// the four lifecycle actions. Which of them are on screen for one read is
// apply's mode-times-state decision; the plan lists every item the
// submenu can show at all.
//
// A read that proves nothing shows none of these items: the submenu is
// empty until a read settles a class of machine, so no action is ever
// offered on a state that was not read — and the user is not asked to
// care about a failed read, which the policy retries by itself.
func serviceSubmenuPlan() []trayMenuItemSpec {
	return []trayMenuItemSpec{
		{Kind: trayItemLine, Tooltip: trayServiceStatusTooltip},
		{Kind: trayItemInstall, Title: "Install service...", Tooltip: "Register HFDesk as a Windows service that starts at boot"},
		{Kind: trayItemStart, Title: "Start service", Tooltip: "Start the registered Windows service"},
		{Kind: trayItemStop, Title: "Stop service", Tooltip: "Stop the running Windows service"},
		{Kind: trayItemUpdate, Title: "Update service", Tooltip: "Re-register the Windows service from the folder of this app"},
		{Kind: trayItemDelete, Title: "Delete service", Tooltip: "Remove the Windows service registration and keep all data"},
	}
}
