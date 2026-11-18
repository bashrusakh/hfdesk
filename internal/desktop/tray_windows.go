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

// trayServiceStatusTooltip is the tooltip of the status line.
const trayServiceStatusTooltip = "The Windows service registration of this machine"

// trayActions are the actions of the tray menu.
type trayActions struct {
	// open opens the web UI of the running instance in the default
	// browser.
	open func()
	// runtime shows the Runtime / Logs window.
	runtime func()
	// install registers the desktop host as a Windows service.
	install func()
	// service opens the registered-service dialog of an installed
	// service: the open/start/update choices the service gate asks.
	service func()
	// quit stops the host.
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

// trayServiceMenu is the service section of the tray menu: the status line
// and the two items that act on the state the line reports.
type trayServiceMenu struct {
	line    *systray.MenuItem
	install *systray.MenuItem
	service *systray.MenuItem
}

// apply writes one read of the registration into the menu.
//
// The install item stays visible and greyed when a service is installed —
// the grey item beside the status line is the reason it is grey — while the
// registered-service item is hidden on a machine without one, so no
// dead-looking second item sits beside the active install action.
func (m *trayServiceMenu) apply(state serviceMenuState) {
	m.line.SetTitle(state.Line)
	if state.InstallEnabled {
		m.install.Enable()
	} else {
		m.install.Disable()
	}
	if state.ServiceVisible {
		m.service.Show()
	} else {
		m.service.Hide()
	}
}

// keepFresh keeps the menu truthful while the tray runs: it re-reads on the
// cadence and whenever an action asks for a refresh. The goroutine owns the
// items after the menu was built and ends when the tray loop closes
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

// runTray runs the tray message loop.
//
// The loop runs on the main thread: the tray library locks the main thread
// in its package init and the window it creates belongs to the thread that
// pumps its messages.
//
// The ready callback runs on a goroutine of its own, the click callbacks run
// in the message loop of the main thread. The ready channel is closed as
// soon as the tray is up, so the caller can watch for a tray that never
// comes up.
func runTray(iconBytes []byte, tooltip string, actions trayActions, serviceView trayServiceView, ready chan struct{}) {
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

		// The order of the menu is the order of the actions: open the
		// web UI, show the runtime window, the service group, then
		// quit.
		//
		// The service group reads top down: the status line says what
		// the machine has, and the items under it act on that state —
		// install when no registration was read, and the
		// registered-service dialog beside the greyed install item when
		// one was, so the grey item always sits next to the state that
		// greys it. The installation is still the last action before
		// the separator: it is the only action that changes the
		// machine, and the quit group stays the group a user reaches
		// for last.
		openItem := systray.AddMenuItem("Open HFDesk", "Open the HFDesk web UI in the default browser")
		runtimeItem := systray.AddMenuItem("Runtime / Logs", "Show the runtime status and the log window")
		menu := &trayServiceMenu{
			line:    systray.AddMenuItem(menuState.Line, trayServiceStatusTooltip),
			install: systray.AddMenuItem("Install service...", "Register HFDesk as a Windows service that starts at boot"),
			service: systray.AddMenuItem("Service...", "Open, start or update the installed Windows service"),
		}
		systray.AddSeparator()
		quitItem := systray.AddMenuItem("Quit HFDesk", "Stop HFDesk and close the local server")

		openItem.Click(func() { actions.open() })
		runtimeItem.Click(func() { actions.runtime() })
		menu.install.Click(func() { actions.install() })
		menu.service.Click(func() { actions.service() })
		quitItem.Click(func() { actions.quit() })

		menu.apply(menuState)
		go menu.keepFresh(serviceView, stopped)
	}, func() {
		// The exit callback runs in the message loop of the main
		// thread. It must not block, so it only stops the refresh of
		// the status line; the menu itself is gone with the loop.
		close(stopped)
	})
}
