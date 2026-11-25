// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

// windowServiceActions returns the host handlers of the five service actions
// the Logs window can call by name — start, stop, update, delete and install
// — for BOTH hosts of that window, so the window offers what the tray offers
// through ONE code path.
//
// One press runs the very flow of the matching tray item, on a goroutine of
// its own under the panic guard, like every other action of the host: the
// four lifecycle actions run performServiceActionFromTray (the single
// service-action guard, the confirmations with No as default, the
// administrator prompt, the handover, the reports and the menu catch-up are
// the established ones — this constructor adds none of them), and the
// installation runs runInstallServiceAction with this host's published
// handover endpoints and catch-up signal (serviceHandoverCommit,
// serviceHandoverQuit and requestServiceMenuCatchUp). instanceMutex is the
// singleton of THIS launch — the very name the menu items pass — so a start
// or an update from the window acts exactly like the tray item does.
//
// A second press during any of these actions is refused by that one guard
// with the busy report of the tray: no second confirmation, no second
// elevated helper, and no surface has a guard of its own.
//
// reread is the host's own way to re-read the state after the action
// returned — for an action that only launched its helper, that is the moment
// it was dispatched, while the settled state arrives through the menu
// catch-up and the next tick: the tray companion passes the window re-show
// its Try again button runs — the same display reads the tray tick's
// fallback reload makes — so a document whose state moved under its own
// action catches up instead of offering a stale step. The desktop host
// passes nil: its window shows the live page of its own server, which
// re-reads itself, while the action path above already asks the menu to
// catch up on every end path, and reloading that live page would only reset
// the scroll position of the log the user is reading.
func windowServiceActions(guard panicGuard, instanceMutex string, reread func()) map[string]func() {
	lifecycle := func(action serviceAction) func() {
		return func() {
			go guard.runBackground(func() {
				performServiceActionFromTray(action, instanceMutex)
				if reread != nil {
					reread()
				}
			})
		}
	}
	return map[string]func(){
		logsStartServiceAction:  lifecycle(serviceActionStart),
		logsStopServiceAction:   lifecycle(serviceActionStop),
		logsUpdateServiceAction: lifecycle(serviceActionUpdate),
		logsDeleteServiceAction: lifecycle(serviceActionUninstall),
		logsInstallServiceAction: func() {
			go guard.runBackground(func() {
				// The published catch-up of the host, not a copy
				// captured at wiring: every surface reads the one
				// owner (the lifecycle items and the windows
				// already do).
				runInstallServiceAction(instanceMutex, requestServiceMenuCatchUp,
					serviceHandoverCommit, serviceHandoverQuit)
				if reread != nil {
					reread()
				}
			})
		},
	}
}
