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
	// runtime shows the Logs window.
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
	// follow carries ONE read of the registration to the open Logs window
	// of a tray companion, so the window follows the state beside it on
	// the very cadence of the line and the two surfaces never state
	// different moments: the pass reads once, the menu applies that read
	// and the window decides from it. Nil for the desktop host, whose
	// window shows the page of its own server and has no other kind of
	// content to switch to.
	follow func(service.Status, error)
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

// trayServiceMenu is the service section of the tray menu: the ROOT entry
// that opens the submenu, and inside it the status line and the items that
// act on the state the line reports — with the lifecycle items (start,
// stop, update, delete) beside the install item; each group belongs to the
// submenu only for the state that proved it (serviceMenuFor), and a read
// that proves nothing hides the submenu items and the root entry itself.
type trayServiceMenu struct {
	service trayMenuItem
	line    trayMenuItem
	open    trayMenuItem
	install trayMenuItem
	start   trayMenuItem
	stop    trayMenuItem
	update  trayMenuItem
	delete  trayMenuItem
	// shown is the state the items currently display, and shownSet
	// reports whether a state was ever applied. Together they let an
	// update identical to the visible one be dropped: the result of the
	// read is already on the items, and the refresh that follows every
	// display read must not rewrite the same values on every status
	// poll of the Logs window. shown is also where each managed item's
	// current VISIBILITY comes from: apply writes an item differently
	// depending on whether the previously applied state showed it
	// (writeItem), and before the first apply every item counts as
	// visible — the items are created visible.
	shown    serviceMenuState
	shownSet bool
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
// Every item follows the mode-times-state mapping of serviceMenuFor: the
// line names the state of a read that settled one and hides on a read that
// did not, the install item belongs to a proven absence only, the
// lifecycle items belong to a proven registration only (inside it they
// enable per state — start and stop on the settled running fact, start
// hidden beside a broken registration, update and delete on the settled
// registration), and an unsettled read hides the line, every item AND the
// root entry that opens the submenu, so exactly one group is ever on screen
// beside a state that was read — and nothing at all beside a state that was
// not.
//
// The Open HFDesk item is written only where its style says so
// (openFollowsService): the companion's item follows the registration and
// the running fact exactly like the mapping above, and the desktop item —
// the address of this host's own server — is left alone.
//
// Every visibility write goes through writeItem, which owns the rule that
// keeps a hidden item hidden: the tray library re-inserts a hidden item
// on ANY update of it, so an already-hidden item is never written again,
// and Hide is the last write of the pass that hides an item. The
// visibility of the PREVIOUSLY applied state (shown) tells writeItem what
// the item looks like right now; before the first apply every item counts
// as visible, because the runner creates them visible.
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
		"lineVisible", state.LineVisible,
		"installVisible", state.InstallVisible,
		"actionsVisible", state.ActionsVisible,
		"startVisible", state.StartVisible,
		"startEnabled", state.StartEnabled,
		"stopEnabled", state.StopEnabled)
	wasShown := m.shownSet
	// The ROOT entry that opens the submenu follows the read as a whole:
	// a read that proved nothing leaves the menu without a Service entry
	// at all — an EMPTY submenu is the same dead surface as the retired
	// "unknown" row — and the first read that proves a state brings the
	// entry back with its line and its items. It is written before its
	// children, so a returning entry is on screen again before the items
	// inside it are shown, and it is never greyed: a disabled parent would
	// block the submenu it opens.
	serviceVisible := state.showsSomething()
	writeItem(m.service, serviceVisible, serviceVisible, !wasShown || m.shown.showsSomething())
	// The status line reports the state of a read that settled one, and
	// disappears on a read that did not: the title is written AFTER the
	// visibility (never beside a hidden item — an update would
	// re-insert it), so the line returns with the state that proves it.
	writeItem(m.line, state.LineVisible, false, !wasShown || m.shown.LineVisible)
	if state.LineVisible {
		m.line.SetTitle(state.Line)
	}
	if m.openFollowsService {
		// The companion rule of Open HFDesk: the item acts on the
		// address of the registration, so it shows beside a proven
		// registration — grey while nothing serves the address —
		// and hides beside a proven absence and an unsettled read.
		writeItem(m.open, state.OpenVisible, state.OpenEnabled,
			!wasShown || m.shown.OpenVisible)
	}
	// The absence rule of BOTH styles: the install item exists only
	// beside a PROVEN absence and acts whenever it shows — exactly one
	// group of the service actions is ever on screen.
	writeItem(m.install, state.InstallVisible, state.InstallVisible,
		!wasShown || m.shown.InstallVisible)
	// The lifecycle items follow their own visibility claims: the start
	// item hides beside a broken registration, the other three appear
	// and disappear together beside a proven registration, and inside it
	// each item enables per state.
	writeItem(m.start, state.StartVisible, state.StartEnabled,
		!wasShown || m.shown.StartVisible)
	wasActions := !wasShown || m.shown.ActionsVisible
	writeItem(m.stop, state.ActionsVisible, state.StopEnabled, wasActions)
	writeItem(m.update, state.ActionsVisible, state.UpdateEnabled, wasActions)
	writeItem(m.delete, state.ActionsVisible, state.DeleteEnabled, wasActions)
	m.shown = state
	m.shownSet = true
}

// writeItem writes one managed menu item for one read, owning the rule
// that keeps a hidden item hidden — and inert:
//
//   - an item that shows is shown first when it was hidden, then enabled
//     or greyed — both writes land on a VISIBLE item, which the tray
//     library updates in place;
//   - an item that hides is greyed FIRST and then hidden, so Hide stays
//     the last write of the pass: the tray library keeps the enabled
//     state of an item it removes, and an item that comes back from the
//     menu must never carry a promise it no longer shows — a hidden
//     Start beside a broken registration was exactly such a leftover;
//   - an item that is already hidden receives NO write at all: any
//     update (Enable, Disable, SetTitle) re-inserts a hidden item into
//     the menu in the tray library, which resurrected the grey install
//     item beside a registration — the duplicate this menu no longer
//     shows.
//
// wasVisible is the visibility of the previously applied state (true
// before the first apply: the runner creates every item visible). A menu
// without the item (a nil field) skips the write: the item does not
// exist to act.
func writeItem(item trayMenuItem, visible, enabled, wasVisible bool) {
	if item == nil {
		return
	}
	if visible {
		if !wasVisible {
			item.Show()
		}
		if enabled {
			item.Enable()
		} else {
			item.Disable()
		}
		return
	}
	if wasVisible {
		item.Disable()
		item.Hide()
	}
}

// keepFresh keeps the menu truthful while the tray runs: it re-reads on the
// cadence, whenever an action asks for a refresh, and after every display
// read made elsewhere — the state reads of the Logs window ask for
// the same catch-up, so no read result can stay invisible in the menu until
// the next tick. The read of every pass also reaches the open Logs window
// through view.follow, so the window follows the same state on the same
// cadence. The goroutine owns the items after the menu was built and
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
		// The pass reads the registration once: the line is built from
		// that read, and the Logs window decides from the very same one
		// — one state, read once, carried to both surfaces. The menu is
		// applied first, so the window neither delays nor orders the
		// line the user reads.
		status, readErr := view.read()
		m.apply(serviceMenuFor(status, readErr))
		if view.follow != nil {
			view.follow(status, readErr)
		}
	}
}

// readServiceMenuState reads the registration for the menu. A read that
// fails classifies as unknown, so the menu shows nothing at all — no line,
// no item — instead of claiming a machine without a service; the read
// policy retries, and the tick brings the state back by itself.
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
// The items come from the plan of the given style: the root of the
// desktop host or the root of the tray companion (see trayMenuPlan), plus
// the Service submenu both styles share (see serviceSubmenuPlan). Both
// share the status line, the enable-per-state mapping and the catch-up
// refresh, so an action of either host runs through the same machinery.
//
// The ready callback runs on a goroutine of its own, the click callbacks run
// in the message loop of the main thread. The ready channel is closed as
// soon as the tray is up, so the caller can watch for a tray that never
// comes up.
func runTray(iconBytes []byte, tooltip string, style trayMenuStyle, actions trayActions, serviceView trayServiceView, ready chan struct{}) {
	// The left click opens the Logs window.
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

		// The root items are built from the plan of the style, in its
		// order: Open HFDesk, Logs, the Service entry, the separator
		// and the style's end item. The Service entry carries the
		// submenu both styles share — the status line first, created
		// from the first read and disabled (it reports, it does not
		// act), then the install item and the lifecycle actions with
		// the same click wiring everywhere; the shell draws the submenu
		// marker itself. Which of the submenu items are on screen for
		// one read is apply's mode-times-state decision.
		menu := &trayServiceMenu{
			openFollowsService: style == trayMenuCompanion,
		}
		for _, spec := range trayMenuPlan(style) {
			if spec.Kind == trayItemSeparator {
				systray.AddSeparator()
				continue
			}
			item := systray.AddMenuItem(spec.Title, spec.Tooltip)
			switch spec.Kind {
			case trayItemService:
				// The ROOT entry of the service section: apply hides it
				// when the read proves nothing, so the menu carries no
				// empty submenu.
				menu.service = item
				for _, child := range serviceSubmenuPlan() {
					title := child.Title
					if child.Kind == trayItemLine {
						// The status line is created with the
						// result of the first read, so the line
						// is right from the first moment the
						// menu exists.
						title = menuState.Line
					}
					sub := item.AddSubMenuItem(title, child.Tooltip)
					switch child.Kind {
					case trayItemLine:
						menu.line = sub
						sub.Disable()
					case trayItemInstall:
						menu.install = sub
						sub.Click(func() { actions.install() })
					case trayItemStart:
						menu.start = sub
						sub.Click(func() { actions.start() })
					case trayItemStop:
						menu.stop = sub
						sub.Click(func() { actions.stop() })
					case trayItemUpdate:
						menu.update = sub
						sub.Click(func() { actions.update() })
					case trayItemDelete:
						menu.delete = sub
						sub.Click(func() { actions.delete() })
					}
				}
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
