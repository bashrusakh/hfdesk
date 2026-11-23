// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
	"github.com/energye/systray"
)

// trayServiceRefreshInterval is the cadence of the service status line of
// the tray menu.
//
// The line follows a machine that can change under it — a start or an
// update from either surface lands minutes later through the elevated
// helper — so it is re-read on a modest cadence instead of being polled:
// one read is a single local query of the service control manager, and the
// service actions also ask for a refresh right after they ran, so the
// cadence only has to catch what happens outside this host.
const trayServiceRefreshInterval = 30 * time.Second

// trayActions are the actions of the tray menu.
type trayActions struct {
	// open opens the web UI in the default browser: the desktop host
	// opens the address of its own instance, the companion the address
	// of the REGISTRATION.
	open func()
	// runtime shows the Runtime / Logs window.
	runtime func()
	// install registers the desktop host as a Windows service.
	install func()
	// start starts the registered service.
	start func()
	// stop stops the registered service.
	stop func()
	// update re-registers the registered service from this folder.
	update func()
	// delete removes the registration of the registered service.
	delete func()
	// service opens the registered-service dialog of an installed
	// service: the open/start/update choices the service gate asks.
	service func()
	// quit stops the host: the desktop host stops its server, the
	// companion closes the tray and its window — the Windows service is
	// not touched by either.
	quit func()
}

// trayServiceView is the feed of the service section of the tray menu: the
// read the status line is built from, and the channel an action posts to
// when it changed the registration, so the line and the two items catch up
// right away instead of waiting for the next tick. Both fields are set by
// the host.
type trayServiceView struct {
	// read reads the registration for a status line.
	read func() (service.Status, error)
	// refresh wakes the menu. A send is dropped when one is already
	// pending, so an action can request a refresh without blocking.
	refresh <-chan struct{}
}

// trayMenuItem is one item of the tray menu as the service section uses it.
//
// The menu holds the items of the tray library behind this interface, so the
// refresh logic — applying a read, dropping an identical update, recording a
// changed line — is testable without a notification area. The tray library's
// own menu items satisfy it as they are.
type trayMenuItem interface {
	// SetTitle replaces the label of the item.
	SetTitle(string)
	// Enable lets the item act.
	Enable()
	// Disable greys the item.
	Disable()
	// Show makes the item visible in the menu.
	Show()
	// Hide removes the item from the menu.
	Hide()
	// Click runs the handler when the item is clicked.
	Click(func())
}

// trayServiceMenu is the service section of the tray menu: the status line
// and the items that act on the state the line reports — the installation
// and the registered-service dialog, with the lifecycle items (start, stop,
// update, delete) between them that belong to the menu only while a
// registration was proven.
type trayServiceMenu struct {
	line    trayMenuItem
	open    trayMenuItem
	install trayMenuItem
	start   trayMenuItem
	stop    trayMenuItem
	update  trayMenuItem
	delete  trayMenuItem
	service trayMenuItem
	// shown is the state the items currently display, and shownSet
	// reports whether a state was ever applied. Together they let an
	// update identical to the visible one be dropped: the result of the
	// read is already on the items, and the refresh that follows every
	// display read must not rewrite the same values on every status
	// poll of the Runtime window.
	shown    serviceMenuState
	shownSet bool
	// installOnAbsence is the absence rule of the tray companion: its
	// install item exists in the menu ONLY beside a proven absence of
	// the registration — registered and unsettled reads hide it, so an
	// item that cannot act never clutters the strict companion menu.
	// The desktop menu keeps its own documented rule instead (the item
	// always sits beside the status line, greyed when it cannot act),
	// so this flag is false there and the install write stays byte for
	// byte what it was.
	installOnAbsence bool
	// openFollowsService is the companion rule of the Open HFDesk item:
	// the companion opens the address of the REGISTRATION, so its item
	// shows beside a proven registration (grey while the service does
	// not serve the address) and hides beside a proven absence and an
	// unsettled read. The desktop menu opens the address of its OWN
	// server — the item is always there and acts whatever the
	// registration says — so this flag is false there and apply leaves
	// the desktop open item alone, byte for byte as before.
	openFollowsService bool
}

// apply writes one read of the registration into the menu.
//
// The install item stays visible and greyed when a service is installed —
// the grey item beside the status line is the reason it is grey — while the
// registered-service section is hidden on a machine without one, so no
// dead-looking second item sits beside the active install action. The
// section carries the dialog item and the four lifecycle items: they appear
// and disappear together (a read that proved no registration offers none of
// them), and inside a proven registration each lifecycle item enables per
// state — start and stop on the running fact, update and delete on the
// registration alone.
//
// The Open HFDesk item is written only where its style says so
// (openFollowsService): the companion's item follows the registration and
// the running fact exactly like the mapping above, and the desktop item —
// the address of this host's own server — is left alone.
//
// The install item and the dialog item belong to the FULL menu only: the
// companion menu is the strict composition without them, so its fields stay
// nil and every write here skips a nil item instead of inventing one —
// the companion gets the identical status line and the identical
// enable-per-state mapping of the four lifecycle items, and nothing else.
//
// An update that equals the state already shown is dropped: the result of
// that read is visible by definition, so nothing has to be rewritten. Every
// update that does change what the menu shows records the state it applied —
// the status line first — so the log of the host carries the transitions of
// the line the user reads, beside the records of the reads themselves.
func (m *trayServiceMenu) apply(state serviceMenuState) {
	if m.shownSet && state == m.shown {
		return
	}
	desktopLog.Info("the tray service menu updated",
		"line", state.Line,
		"installEnabled", state.InstallEnabled,
		"serviceVisible", state.ServiceVisible,
		"startEnabled", state.StartEnabled,
		"stopEnabled", state.StopEnabled)
	m.line.SetTitle(state.Line)
	if m.openFollowsService {
		// The companion rule of Open HFDesk: the item acts on the
		// address of the registration, so it shows beside a proven
		// registration — grey while nothing serves the address —
		// and hides beside a proven absence and an unsettled read.
		if state.OpenVisible {
			showItem(m.open)
		} else {
			hideItem(m.open)
		}
		enableItem(m.open, state.OpenEnabled)
	}
	if m.installOnAbsence {
		// The absence rule of the companion: the install item shows
		// only beside a PROVEN absence (the only state InstallEnabled
		// claims) and hides beside a registration or an unsettled
		// read — the four lifecycle items below follow their own
		// visibility claim, so exactly one group of the service
		// actions is ever on screen.
		if state.InstallEnabled {
			showItem(m.install)
		} else {
			hideItem(m.install)
		}
		enableItem(m.install, state.InstallEnabled)
	} else {
		// The desktop rule: the install item always sits beside the
		// status line, greyed when it cannot act.
		enableItem(m.install, state.InstallEnabled)
	}
	if state.ServiceVisible {
		showItem(m.service)
		showItem(m.start)
		showItem(m.stop)
		showItem(m.update)
		showItem(m.delete)
		enableItem(m.start, state.StartEnabled)
		enableItem(m.stop, state.StopEnabled)
		enableItem(m.update, state.UpdateEnabled)
		enableItem(m.delete, state.DeleteEnabled)
	} else {
		hideItem(m.service)
		hideItem(m.start)
		hideItem(m.stop)
		hideItem(m.update)
		hideItem(m.delete)
	}
	m.shown = state
	m.shownSet = true
}

// enableItem lets the item act when enabled and greys it otherwise, so every
// lifecycle item is written through one rule. A menu without the item (the
// companion composition) skips it: the item does not exist to act.
func enableItem(item trayMenuItem, enabled bool) {
	if item == nil {
		return
	}
	if enabled {
		item.Enable()
	} else {
		item.Disable()
	}
}

// showItem makes an existing item visible; a menu without it (the
// companion composition) skips the write.
func showItem(item trayMenuItem) {
	if item != nil {
		item.Show()
	}
}

// hideItem removes an existing item from the menu; a menu without it (the
// companion composition) skips the write.
func hideItem(item trayMenuItem) {
	if item != nil {
		item.Hide()
	}
}

// keepFresh keeps the menu truthful while the tray runs: it re-reads on the
// cadence, whenever an action asks for a refresh, and after every display
// read made elsewhere — the state reads of the Runtime / Logs window ask for
// the same catch-up, so no read result can stay invisible in the menu until
// the next tick. The goroutine owns the items after the menu was built and
// ends when the tray loop closes
// stopped — yet an update already in flight when that happens still applies
// its result, and each pass through the select may pick a queued tick or
// refresh before the closed stopped channel. What the loop guarantees is
// therefore the stopping of the recurring refreshes on shutdown, not that no
// update ever reaches a menu that is already gone.
func (m *trayServiceMenu) keepFresh(view trayServiceView, stopped <-chan struct{}) {
	ticker := time.NewTicker(trayServiceRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stopped:
			return
		case <-ticker.C:
		case <-view.refresh:
		}
		m.apply(readServiceMenuState(view))
	}
}

// readServiceMenuState reads the registration for the menu. A read that
// fails classifies as unknown, so the menu greys the install item and
// shows the line that is honest about the failure instead of claiming a
// machine without a service.
func readServiceMenuState(view trayServiceView) serviceMenuState {
	status, err := view.read()
	return serviceMenuFor(status, err)
}

// runTrayLoop is the entry of the tray message loop the hosts call.
//
// It is a package level variable so the startup tests can drive a host
// without a notification area: a CI agent has no shell that owns one, and
// the loop would either block or end the host through its tray-timeout
// report. The seam receives everything the real loop needs, so a test can
// prove WHICH composition and which actions a host wired while the loop
// itself is replaced.
var runTrayLoop = runTray

// runTray runs the tray message loop.
//
// The loop runs on the main thread: the tray library locks the main thread
// in its package init and the window it creates belongs to the thread that
// pumps its messages.
//
// The items come from the plan of the given style: the full composition of
// the desktop host or the strict composition of the tray companion (see
// trayMenuPlan). Both share the status line, the enable-per-state mapping
// and the catch-up refresh, so an action of either host runs through the
// same machinery.
//
// The ready callback runs on a goroutine of its own, the click callbacks run
// in the message loop of the main thread. The ready channel is closed as
// soon as the tray is up, so the caller can watch for a tray that never
// comes up.
func runTray(iconBytes []byte, tooltip string, style trayMenuStyle, actions trayActions, serviceView trayServiceView, ready chan struct{}) {
	// The left click opens the Runtime / Logs window.
	systray.SetOnClick(func(systray.IMenu) {
		actions.runtime()
	})

	// The exit callback ends the refresh of the status line. It must not
	// block, and closing the channel is the whole of that cleanup: the
	// menu is gone with the loop it belonged to.
	stopped := make(chan struct{})

	systray.Run(func() {
		close(ready)

		if len(iconBytes) > 0 {
			systray.SetIcon(iconBytes)
		}
		systray.SetTooltip(tooltip)

		// The first read runs here, so the menu is already right when
		// it appears; the ready channel above is already closed, and
		// the read is bounded by the display budget of the host.
		menuState := readServiceMenuState(serviceView)

		// The items are built from the plan, in its order. The service
		// group reads top down in both styles: the status line says
		// what the machine has, and the items under it act on that
		// state — install when no registration was read, and the
		// registered-service section beside the greyed install item
		// when one was (the full menu only), and the four lifecycle
		// items with the same enable-per-state mapping everywhere.
		menu := &trayServiceMenu{
			installOnAbsence:   style == trayMenuCompanion,
			openFollowsService: style == trayMenuCompanion,
		}
		for _, spec := range trayMenuPlan(style) {
			if spec.Kind == trayItemSeparator {
				systray.AddSeparator()
				continue
			}
			// The status line is created with the result of the
			// first read, so the line is right from the first
			// moment the menu exists; every other item carries its
			// fixed title from the plan.
			title := spec.Title
			if spec.Kind == trayItemLine {
				title = menuState.Line
			}
			item := systray.AddMenuItem(title, spec.Tooltip)
			switch spec.Kind {
			case trayItemLine:
				menu.line = item
			case trayItemInstall:
				menu.install = item
				item.Click(func() { actions.install() })
			case trayItemStart:
				menu.start = item
				item.Click(func() { actions.start() })
			case trayItemStop:
				menu.stop = item
				item.Click(func() { actions.stop() })
			case trayItemUpdate:
				menu.update = item
				item.Click(func() { actions.update() })
			case trayItemDelete:
				menu.delete = item
				item.Click(func() { actions.delete() })
			case trayItemService:
				menu.service = item
				item.Click(func() { actions.service() })
			case trayItemOpen:
				menu.open = item
				item.Click(func() { actions.open() })
			case trayItemRuntime:
				item.Click(func() { actions.runtime() })
			case trayItemQuit:
				item.Click(func() { actions.quit() })
			}
		}

		menu.apply(menuState)
		go menu.keepFresh(serviceView, stopped)
	}, func() {
		// The exit callback runs in the message loop of the main
		// thread. It must not block, so it only stops the refresh of
		// the status line; the menu itself is gone with the loop.
		close(stopped)
	})
}
