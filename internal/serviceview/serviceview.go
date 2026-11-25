// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

// Package serviceview is the ONE table of the service state a surface shows.
//
// The tray menu, the Runtime window of the desktop host and the runtime page
// of the service host all describe the same platform service registration.
// Each of them used to carry its own mapping from the read to the words and
// the actions it offers, so the surfaces could disagree about the same
// machine. This package is that mapping, once: a surface reads the
// registration, passes the state word and the supervisor's running fact to
// From, and renders the Snapshot it gets back.
//
// The package deliberately imports nothing but the standard library. It is
// imported by internal/server, internal/service and internal/desktop, and the
// server must not import internal/service (which imports the server), so the
// table cannot live in either of them.
package serviceview

import "strings"

// Class is what one read proved about the machine.
type Class string

const (
	// ClassAbsent means the read proved that no registration exists.
	ClassAbsent Class = "absent"
	// ClassPresent means the read proved a registration.
	ClassPresent Class = "present"
	// ClassUnknown means the read proved nothing: it failed, or the
	// supervisor reported no usable state. Nothing about the service may be
	// claimed from it.
	ClassUnknown Class = "unknown"
)

// The tones of the status line. They are the color classes of the surfaces:
// a running service is success, a transition is a warning, a registration
// that does not run is an error, and the absence of a registration is muted.
const (
	ToneSuccess = "success"
	ToneWarning = "warning"
	ToneError   = "error"
	ToneMuted   = "muted"
)

// Action is the availability of one action of a surface.
//
// Visible=false means the action does not belong on screen at all, and a
// hidden action is always Enabled=false: a hidden item must never be
// clickable behind the scenes.
type Action struct {
	// Visible reports whether the action belongs on screen.
	Visible bool `json:"visible"`
	// Enabled reports whether the action may be activated.
	Enabled bool `json:"enabled"`
}

// Actions is the availability of every action a service surface can offer.
type Actions struct {
	// Install is the installation of a service.
	Install Action `json:"install"`
	// Start starts a registered service.
	Start Action `json:"start"`
	// Stop stops a running service.
	Stop Action `json:"stop"`
	// Update re-registers the service from the folder of the application.
	Update Action `json:"update"`
	// Delete removes the registration and keeps the data.
	Delete Action `json:"delete"`
	// Open opens the web UI the registration serves.
	Open Action `json:"open"`
}

// Snapshot is what one read of the registration means for a surface.
type Snapshot struct {
	// State is the raw state word the read reported, kept for the
	// compatible serviceState field of the runtime status. It is never
	// serialized inside this object.
	State string `json:"-"`
	// Class is what the read proved about the machine.
	Class Class `json:"class"`
	// Label is the status line of the surface ("Service: running"). It is
	// empty for an unknown read: a read that proved nothing has no state
	// to name.
	Label string `json:"label"`
	// Tone is the color class of the status line, empty for an unknown
	// read.
	Tone string `json:"tone"`
	// Actions is the availability of every action of the surface.
	Actions Actions `json:"actions"`
}

// The state words this table distinguishes. They are the vocabulary of the
// service layer; the table spells them here so the package imports no other
// package of the application.
const (
	stateNotInstalled = "not-installed"
	stateStopped      = "stopped"
	stateStarting     = "starting"
	stateRunning      = "running"
	stateStopping     = "stopping"
	stateBroken       = "broken"
	stateUnknown      = "unknown"
)

// From maps one read of the service registration onto what a surface shows.
//
// The rules of the whole application live here:
//
//   - a failed read, an empty state and the "unknown" word prove nothing:
//     the Snapshot is ClassUnknown, it carries no label, no tone and no
//     action, because "no service is installed" is a claim such a read did
//     not make;
//   - "not-installed" is a proven absence: the status line names it and the
//     install item is the one action of that state;
//   - a registration in a transition ("starting", "stopping") names the
//     transition in its line and greys every lifecycle item: a pending
//     state is not a result to decide on;
//   - a registration whose running fact stands — the supervisor's fact, or
//     the "running" state as the fallback of a manager that does not report
//     the fact — reads as running, and start is not offered beside it; a
//     broken registration keeps its start hidden even while it runs, because
//     its program is gone and the update is the one action that repairs it;
//   - a settled registration that does not run reads as stopped when the
//     supervisor says stopped and as failed for every other diagnosis
//     (failed, broken, masked, masked-runtime): neither runs, and both are
//     repaired by the update.
//
// running is the supervisor's own running fact of the same read. readErr
// reports that the read failed; then nothing beside the unknown class is
// claimed, whatever the state beside the failure says.
func From(state string, running bool, readErr bool) Snapshot {
	if readErr {
		return Snapshot{Class: ClassUnknown}
	}
	state = strings.TrimSpace(state)
	if state == "" || state == stateUnknown {
		return Snapshot{State: state, Class: ClassUnknown}
	}
	if state == stateNotInstalled {
		return Snapshot{
			State: state,
			Class: ClassAbsent,
			Label: "Service: not installed",
			Tone:  ToneMuted,
			Actions: Actions{
				Install: Action{Visible: true, Enabled: true},
			},
		}
	}

	settled := state != stateStarting && state != stateStopping
	broken := state == stateBroken
	// The supervisor's fact comes first: a broken registration whose
	// process still serves IS running for the user. The state is the
	// fallback of a manager that does not report the fact, and a pending
	// start counts as serving because the instance is on its way up.
	serving := running || state == stateRunning || state == stateStarting

	snapshot := Snapshot{
		State: state,
		Class: ClassPresent,
		Actions: Actions{
			// A broken registration cannot be started: its program is
			// gone, so the start would fail on exactly that fact. The
			// update is the repair.
			Start:  Action{Visible: !broken, Enabled: settled && !serving && !broken},
			Stop:   Action{Visible: true, Enabled: settled && serving},
			Update: Action{Visible: true, Enabled: settled},
			Delete: Action{Visible: true, Enabled: settled},
			Open:   Action{Visible: true, Enabled: serving},
		},
	}

	switch {
	case !settled:
		// The transition names itself, and every lifecycle item stays
		// grey until the reading settles again.
		snapshot.Label = "Service: " + state
		snapshot.Tone = ToneWarning
	case serving:
		snapshot.Label = "Service: running"
		snapshot.Tone = ToneSuccess
	case state == stateStopped:
		snapshot.Label = "Service: stopped"
		snapshot.Tone = ToneMuted
	default:
		snapshot.Label = "Service: failed"
		snapshot.Tone = ToneError
	}
	return snapshot
}
