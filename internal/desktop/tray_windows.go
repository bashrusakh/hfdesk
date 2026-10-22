// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import "github.com/energye/systray"

// trayActions are the actions of the tray menu.
type trayActions struct {
	// open opens the web UI of the running instance in the default
	// browser.
	open func()
	// runtime shows the Runtime / Logs window.
	runtime func()
	// quit stops the host.
	quit func()
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
func runTray(iconBytes []byte, tooltip string, actions trayActions, ready chan struct{}) {
	// The left click opens the Runtime / Logs window.
	systray.SetOnClick(func(systray.IMenu) {
		actions.runtime()
	})

	systray.Run(func() {
		close(ready)

		if len(iconBytes) > 0 {
			systray.SetIcon(iconBytes)
		}
		systray.SetTooltip(tooltip)

		// The order of the menu is the order of the actions: open the
		// web UI, show the runtime window, then quit.
		openItem := systray.AddMenuItem("Open HFDesk", "Open the HFDesk web UI in the default browser")
		runtimeItem := systray.AddMenuItem("Runtime / Logs", "Show the runtime status and the log window")
		systray.AddSeparator()
		quitItem := systray.AddMenuItem("Quit HFDesk", "Stop HFDesk and close the local server")

		openItem.Click(func() { actions.open() })
		runtimeItem.Click(func() { actions.runtime() })
		quitItem.Click(func() { actions.quit() })
	}, func() {
		// The exit callback runs in the message loop of the main thread.
		// It must not block, so the cleanup happens after the loop ends.
	})
}
