// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"github.com/bashrusakh/hfdesk/internal/service"
)

// serviceRegistrationClass is how one read of the registration classifies
// the machine for the Service submenu of the tray menu.
type serviceRegistrationClass int

const (
	// registrationUnknown is a read that could not settle whether a
	// registration exists: the read failed, or the supervisor reported no
	// usable state.
	registrationUnknown serviceRegistrationClass = iota
	// registrationAbsent is a read that proved there is no registration.
	registrationAbsent
	// registrationPresent is a read that found a registration.
	registrationPresent
)

// classifyRegistration decides what one read of the registration proves.
//
// A failed read and a state the supervisor could not report both stay
// unknown: neither proves a machine without a service, and a menu that
// guessed "absent" from them would offer an installation beside a
// registration it simply could not see.
func classifyRegistration(status service.Status, readErr error) serviceRegistrationClass {
	switch {
	case readErr != nil:
		return registrationUnknown
	case status.State == "" || status.State == service.StateUnknown:
		return registrationUnknown
	case status.State == service.StateNotInstalled:
		return registrationAbsent
	default:
		return registrationPresent
	}
}

// serviceMenuState is what the Service submenu of the tray menu shows
// for one read of the registration.
type serviceMenuState struct {
	// Line is the compact status line of the submenu, empty when the
	// read proved no state to name.
	Line string
	// LineVisible reports whether the status line belongs into the
	// submenu: a read that settled a class of machine shows its word,
	// an unsettled read shows no line at all — the menu states what it
	// read and nothing else.
	LineVisible bool
	// InstallVisible reports whether the install item belongs into the
	// submenu: it acts ONLY on a proven absence of a registration, so
	// exactly one group of service actions — install or the lifecycle
	// group — is ever on screen beside the line. A visible install item
	// always acts; there is no grey install state anymore.
	InstallVisible bool
	// ActionsVisible reports whether the lifecycle items the
	// registration owns as a group — stop, update and delete — belong
	// into the submenu: they act on a registration, so a read that
	// proved one shows them and a proven absence and an unsettled read
	// hide them — a destructive action is never offered without a state
	// that was read. The start item has its own visibility
	// (StartVisible), because a broken registration cannot be started.
	ActionsVisible bool
	// OpenVisible reports whether the Open HFDesk item belongs into the
	// menu: it acts on the address of the REGISTRATION, so a proven
	// registration shows it and a proven absence and an unsettled read
	// hide it — an item that would open nothing never sits in the menu
	// (the rule of the companion menu; the desktop host opens its own
	// server's address and keeps its open item out of this mapping).
	OpenVisible bool
	// OpenEnabled reports whether the Open HFDesk item may act: only a
	// service that serves the address — the supervisor's running fact,
	// the same classification the stop item follows — is a service whose
	// address a browser can reach. A registered service that does not
	// run leaves the item grey: the registration proved the address, but
	// nothing answers it yet.
	OpenEnabled bool
	// StartVisible reports whether the start item belongs into the
	// submenu: a proven registration shows it, EXCEPT a broken one —
	// whose program is gone, so a start would fail on exactly that fact
	// and the update is the one action that repairs it. A transition
	// keeps the item on screen, grey.
	StartVisible bool
	// StartEnabled reports whether the start item may act: only a
	// registration whose state is settled and does not run — stopped or
	// failed — is a state a start can act on.
	StartEnabled bool
	// StopEnabled reports whether the stop item may act: a settled
	// registration whose running fact stands — where a pending start
	// counts as running only after it settled into one.
	StopEnabled bool
	// UpdateEnabled reports whether the update item may act: a settled
	// registration is a state an update may re-point — and a broken or
	// failed one is exactly the registration the update repairs.
	UpdateEnabled bool
	// DeleteEnabled reports whether the delete item may act: a settled
	// registration is a state the removal may take — it stops a running
	// instance inside Manager.Uninstall as part of the action.
	DeleteEnabled bool
}

// serviceStatusLine returns the compact status line of a service read.
//
// The wording is one honest word per user status of the machine: a proven
// absence is "Service: not installed", a registration in a transition names
// the transition it is in ("Service: starting", "Service: stopping"), a
// settled registration that serves reads "Service: running", a settled
// registration that is stopped reads "Service: stopped", and every other
// settled registration reads "Service: failed" — a crashed instance and a
// registration whose program is gone ("broken") are one user status, because
// from the user's side neither runs and both are repaired by the update.
//
// A read that proved nothing has NO line: the empty string, and the submenu
// shows no item at all (every visibility claim stays false). The menu never
// states a state it did not read, and "unknown" is not a state of the
// machine the user could act on — it is the absence of an answer. The read
// retries itself before it reports (see serviceStateReader.readForDisplay),
// so a momentary failure of the service control manager never surfaces.
func serviceStatusLine(status service.Status, readErr error) string {
	switch classifyRegistration(status, readErr) {
	case registrationAbsent:
		return "Service: not installed"
	case registrationPresent:
		if !serviceStateSettled(status.State) {
			return "Service: " + string(status.State)
		}
		if serviceStatusRunning(status) {
			return "Service: running"
		}
		if status.State == service.StateStopped {
			return "Service: stopped"
		}
		return "Service: failed"
	default:
		return ""
	}
}

// serviceMenuFor maps one read of the registration onto the submenu it
// shows — the mode-times-state model both styles share:
//
//   - a proven ABSENCE shows the install item and nothing else that
//     acts: a menu with no registration shows no dead lifecycle items,
//     and the install item is the one action of that state;
//   - a proven REGISTRATION shows the lifecycle items and hides the
//     install item, so exactly one action group is ever on screen —
//     beside a registration whose state is SETTLED: start is shown and
//     enabled on a settled state that does not run, except beside a
//     BROKEN registration, whose start would fail and whose one repair is
//     the update (start hidden, update and delete enabled); stop on a
//     settled state whose running fact stands, update and delete on the
//     registration alone — so a failed registration offers both the start
//     and the update;
//   - a TRANSITION (a pending start or stop) is not a state to decide
//     on — the rule of the service gate applied to the group — so every
//     lifecycle item goes grey until the reading settles again;
//   - an UNSETTLED read claims nothing and shows nothing: no line and no
//     item of the submenu at all, because "no service is installed" was
//     not proven by a read that failed and no action may be offered on a
//     state that was never read. The read retries itself before it
//     reports (readForDisplay), and the ordinary tick and the catch-up
//     after every action keep reading, so the line and the items come
//     back by themselves as soon as a read proves a state.
//
// The Open HFDesk item keeps its own two claims (visible beside a proven
// registration, enabled on the running fact) — the companion rule apply
// consumes; the desktop host opens its own address and leaves its item
// out of this mapping.
func serviceMenuFor(status service.Status, readErr error) serviceMenuState {
	state := serviceMenuState{Line: serviceStatusLine(status, readErr)}
	switch classifyRegistration(status, readErr) {
	case registrationAbsent:
		state.LineVisible = true
		state.InstallVisible = true
	case registrationPresent:
		running := serviceStatusRunning(status)
		settled := serviceStateSettled(status.State)
		broken := status.State == service.StateBroken
		state.LineVisible = true
		state.ActionsVisible = true
		state.OpenVisible = true
		state.OpenEnabled = running
		state.StartVisible = !broken
		state.StartEnabled = settled && !running && !broken
		state.StopEnabled = settled && running
		state.UpdateEnabled = settled
		state.DeleteEnabled = settled
	}
	return state
}
