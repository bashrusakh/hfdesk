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
	// Line is the compact status line of the submenu.
	Line string
	// InstallVisible reports whether the install item belongs into the
	// submenu: it acts ONLY on a proven absence of a registration, so
	// exactly one group of service actions — install or the lifecycle
	// group — is ever on screen beside the line. A visible install item
	// always acts; there is no grey install state anymore.
	InstallVisible bool
	// ActionsVisible reports whether the four lifecycle items (start,
	// stop, update, delete) belong into the submenu: they act on a
	// registration, so a read that proved one shows them and a proven
	// absence and an unsettled read hide them — a destructive action is
	// never offered without a state that was read.
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
	// StartEnabled reports whether the start item may act: only a
	// registration whose state is settled and does not run — stopped,
	// failed, broken without an instance — is a state a start can act on.
	StartEnabled bool
	// StopEnabled reports whether the stop item may act: a settled
	// registration whose running fact stands — where a pending start
	// counts as running only after it settled into one.
	StopEnabled bool
	// UpdateEnabled reports whether the update item may act: a settled
	// registration is a state an update may re-point.
	UpdateEnabled bool
	// DeleteEnabled reports whether the delete item may act: a settled
	// registration is a state the removal may take — it stops a running
	// instance inside Manager.Uninstall as part of the action.
	DeleteEnabled bool
}

// serviceStatusLine returns the compact status line of a service read.
//
// The wording is one honest word per class of machine: an unsettled read
// is "Service: unknown", a proven absence is "Service: not installed",
// a registration in a transition names the transition it is in
// ("Service: starting", "Service: stopping"), a settled registration
// that serves reads "Service: running", and any other settled
// registration reads "Service: installed". The line answers what the
// machine has right now — never a guess: a state that could not be read
// never claims a machine without a service.
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
		return "Service: installed"
	default:
		return "Service: unknown"
	}
}

// serviceMenuFor maps one read of the registration onto the submenu it
// shows — the mode-times-state model both styles share:
//
//   - a proven ABSENCE shows the install item and nothing else that
//     acts: a menu with no registration shows no dead lifecycle items,
//     and the install item is the one action of that state;
//   - a proven REGISTRATION shows the four lifecycle items and hides the
//     install item, so exactly one action group is ever on screen —
//     beside a registration whose state is SETTLED: start is enabled on
//     a settled state that does not run, stop on a settled state whose
//     running fact stands, update and delete on the registration alone;
//   - a TRANSITION (a pending start or stop) is not a state to decide
//     on — the rule of the service gate applied to the whole group — so
//     every lifecycle item goes grey until the reading settles again;
//   - an UNSETTLED read claims neither group: the line says unknown and
//     no action is offered, because "no service is installed" was not
//     proven by a read that failed. Auto re-reading continues, so the
//     submenu catches up as soon as a read proves a state.
//
// The Open HFDesk item keeps its own two claims (visible beside a proven
// registration, enabled on the running fact) — the companion rule apply
// consumes; the desktop host opens its own address and leaves its item
// out of this mapping.
func serviceMenuFor(status service.Status, readErr error) serviceMenuState {
	state := serviceMenuState{Line: serviceStatusLine(status, readErr)}
	switch classifyRegistration(status, readErr) {
	case registrationAbsent:
		state.InstallVisible = true
	case registrationPresent:
		running := serviceStatusRunning(status)
		settled := serviceStateSettled(status.State)
		state.ActionsVisible = true
		state.OpenVisible = true
		state.OpenEnabled = running
		state.StartEnabled = settled && !running
		state.StopEnabled = settled && running
		state.UpdateEnabled = settled
		state.DeleteEnabled = settled
	}
	return state
}
