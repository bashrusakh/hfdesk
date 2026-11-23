// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"github.com/bashrusakh/hfdesk/internal/service"
)

// serviceRegistrationClass is how one read of the registration classifies
// the machine for the service section of the tray menu.
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

// serviceMenuState is what the service section of the tray menu shows for
// one read of the registration.
type serviceMenuState struct {
	// Line is the compact status line of the menu.
	Line string
	// InstallEnabled reports whether the install item may act.
	InstallEnabled bool
	// ServiceVisible reports whether the registered-service section — the
	// registered-service dialog item and the lifecycle items beside the
	// greyed install item — belongs into the menu. It is the single
	// visibility claim of that section: the dialog item and the lifecycle
	// items appear and disappear together, because all of them act on a
	// registration and a read that did not prove one offers none of them.
	ServiceVisible bool
	// StartEnabled reports whether the start item may act: only a
	// registration whose state is settled and does not run — stopped,
	// failed, broken without an instance — is a state a start can act on.
	// A transition is not a state to decide on (the rule of the service
	// gate), so a pending state leaves the item grey.
	StartEnabled bool
	// StopEnabled reports whether the stop item may act: it follows the
	// supervisor's running fact — the same classification every other
	// surface decides "the service runs" with, where a pending start
	// counts as running and a pending stop does not.
	StopEnabled bool
	// UpdateEnabled reports whether the update item may act. Every
	// registration is a state an update may re-point, so it follows the
	// visibility claim alone.
	UpdateEnabled bool
	// DeleteEnabled reports whether the delete item may act. The removal
	// stops a running instance inside Manager.Uninstall as part of the
	// action, so every registration is a state it may act on as well.
	DeleteEnabled bool
}

// serviceStatusLine returns the compact status line of a service read.
//
// The wording never claims "not installed" for a state that could not be
// read: an unsettled read is "Service: unknown", the one honest word for
// that machine. A proven registration is named by the one word the
// decision rests on — the line reads "Service: installed" and nothing
// else, because the line answers only whether a registration exists, not
// how it is doing.
func serviceStatusLine(status service.Status, readErr error) string {
	switch classifyRegistration(status, readErr) {
	case registrationAbsent:
		return "Service: not installed"
	case registrationPresent:
		return "Service: installed"
	default:
		return "Service: unknown"
	}
}

// serviceMenuFor maps one read of the registration onto the menu it shows.
//
// The two items follow the same decision as the line: a proven absence
// enables the install item and hides the service section — a menu with no
// registration shows no dead second item beside the install action — while
// a proven registration greys the install item and shows the
// registered-service section beside it, so the grey item always sits next
// to the state that greys it. An unsettled read takes neither claim: the
// install item is greyed rather than left active, because "enabled only
// while no service is installed" is not proven by a read that failed, and
// the lifecycle items stay hidden — a destructive action is never offered
// without a state that was read.
//
// Inside a proven registration the lifecycle items enable per state, on the
// classifications every other surface already decides with: the stop item
// follows the running fact (a pending start counts as running, a pending
// stop does not), the start item follows its negation restricted to settled
// states (a transition is not a state to decide on), and update and delete
// follow the registration alone — the update re-points it and the removal
// stops a running instance inside Manager.Uninstall as part of the action.
func serviceMenuFor(status service.Status, readErr error) serviceMenuState {
	state := serviceMenuState{Line: serviceStatusLine(status, readErr)}
	switch classifyRegistration(status, readErr) {
	case registrationAbsent:
		state.InstallEnabled = true
	case registrationPresent:
		running := serviceStatusRunning(status)
		state.ServiceVisible = true
		state.StartEnabled = !running && serviceStateSettled(status.State)
		state.StopEnabled = running
		state.UpdateEnabled = true
		state.DeleteEnabled = true
	}
	return state
}
