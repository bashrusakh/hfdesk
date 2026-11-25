// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package serviceview

import "testing"

// act builds one expected Action.
func act(visible, enabled bool) Action { return Action{Visible: visible, Enabled: enabled} }

// wantPresent builds the expected Snapshot of a proven registration.
func wantPresent(state, label, tone string, start, stop, update, delete, open Action) Snapshot {
	return Snapshot{
		State: state,
		Class: ClassPresent,
		Label: label,
		Tone:  tone,
		Actions: Actions{
			Start:  start,
			Stop:   stop,
			Update: update,
			Delete: delete,
			Open:   open,
		},
	}
}

// TestFrom is the table of the whole application: every state one read can
// report maps onto exactly one class, label, tone and set of actions.
func TestFrom(t *testing.T) {
	runningActions := Actions{
		Start:  act(true, false),
		Stop:   act(true, true),
		Update: act(true, true),
		Delete: act(true, true),
		Open:   act(true, true),
	}
	brokenRunningActions := Actions{
		Start:  act(false, false),
		Stop:   act(true, true),
		Update: act(true, true),
		Delete: act(true, true),
		Open:   act(true, true),
	}
	stoppedActions := Actions{
		Start:  act(true, true),
		Stop:   act(true, false),
		Update: act(true, true),
		Delete: act(true, true),
		Open:   act(true, false),
	}
	brokenActions := Actions{
		Start:  act(false, false),
		Stop:   act(true, false),
		Update: act(true, true),
		Delete: act(true, true),
		Open:   act(true, false),
	}
	transitionActions := Actions{
		Start:  act(true, false),
		Stop:   act(true, false),
		Update: act(true, false),
		Delete: act(true, false),
		Open:   act(true, false),
	}
	startingActions := transitionActions
	startingActions.Open = act(true, true)

	for _, tc := range []struct {
		name    string
		state   string
		running bool
		readErr bool
		want    Snapshot
	}{
		{
			name:    "a failed read proves nothing",
			state:   "running",
			running: true,
			readErr: true,
			want:    Snapshot{Class: ClassUnknown},
		},
		{
			name: "an empty state proves nothing",
			want: Snapshot{Class: ClassUnknown},
		},
		{
			name:  "a state of whitespace proves nothing",
			state: "   ",
			want:  Snapshot{Class: ClassUnknown},
		},
		{
			name:  "the unknown word proves nothing",
			state: "unknown",
			want:  Snapshot{State: "unknown", Class: ClassUnknown},
		},
		{
			name:  "no registration",
			state: "not-installed",
			want: Snapshot{
				State:   "not-installed",
				Class:   ClassAbsent,
				Label:   "Service: not installed",
				Tone:    ToneMuted,
				Actions: Actions{Install: act(true, true)},
			},
		},
		{
			name:    "a proven absence stays absent beside a running fact",
			state:   "not-installed",
			running: true,
			want: Snapshot{
				State:   "not-installed",
				Class:   ClassAbsent,
				Label:   "Service: not installed",
				Tone:    ToneMuted,
				Actions: Actions{Install: act(true, true)},
			},
		},
		{
			name:  "a running service",
			state: "running",
			want: wantPresent("running", "Service: running", ToneSuccess,
				runningActions.Start, runningActions.Stop, runningActions.Update,
				runningActions.Delete, runningActions.Open),
		},
		{
			name:    "a running service beside its running fact",
			state:   "running",
			running: true,
			want: wantPresent("running", "Service: running", ToneSuccess,
				runningActions.Start, runningActions.Stop, runningActions.Update,
				runningActions.Delete, runningActions.Open),
		},
		{
			name:  "a stopped service",
			state: "stopped",
			want: wantPresent("stopped", "Service: stopped", ToneMuted,
				stoppedActions.Start, stoppedActions.Stop, stoppedActions.Update,
				stoppedActions.Delete, stoppedActions.Open),
		},
		{
			name:    "a stopped state beside the running fact serves",
			state:   "stopped",
			running: true,
			want: wantPresent("stopped", "Service: running", ToneSuccess,
				runningActions.Start, runningActions.Stop, runningActions.Update,
				runningActions.Delete, runningActions.Open),
		},
		{
			name:  "a service on its way up",
			state: "starting",
			want: wantPresent("starting", "Service: starting", ToneWarning,
				startingActions.Start, startingActions.Stop, startingActions.Update,
				startingActions.Delete, startingActions.Open),
		},
		{
			name:    "a service on its way up beside its running fact",
			state:   "starting",
			running: true,
			want: wantPresent("starting", "Service: starting", ToneWarning,
				startingActions.Start, startingActions.Stop, startingActions.Update,
				startingActions.Delete, startingActions.Open),
		},
		{
			name:  "a service on its way down",
			state: "stopping",
			want: wantPresent("stopping", "Service: stopping", ToneWarning,
				transitionActions.Start, transitionActions.Stop, transitionActions.Update,
				transitionActions.Delete, transitionActions.Open),
		},
		{
			name:    "a stopping state beside the running fact keeps the transition word",
			state:   "stopping",
			running: true,
			want: wantPresent("stopping", "Service: stopping", ToneWarning,
				transitionActions.Start, transitionActions.Stop, transitionActions.Update,
				transitionActions.Delete, act(true, true)),
		},
		{
			name:  "a failed service",
			state: "failed",
			want: wantPresent("failed", "Service: failed", ToneError,
				stoppedActions.Start, stoppedActions.Stop, stoppedActions.Update,
				stoppedActions.Delete, stoppedActions.Open),
		},
		{
			name:    "a failed state beside the running fact serves",
			state:   "failed",
			running: true,
			want: wantPresent("failed", "Service: running", ToneSuccess,
				runningActions.Start, runningActions.Stop, runningActions.Update,
				runningActions.Delete, runningActions.Open),
		},
		{
			name:  "a broken registration",
			state: "broken",
			want: wantPresent("broken", "Service: failed", ToneError,
				brokenActions.Start, brokenActions.Stop, brokenActions.Update,
				brokenActions.Delete, brokenActions.Open),
		},
		{
			name:    "a broken registration with a running instance",
			state:   "broken",
			running: true,
			want: wantPresent("broken", "Service: running", ToneSuccess,
				brokenRunningActions.Start, brokenRunningActions.Stop, brokenRunningActions.Update,
				brokenRunningActions.Delete, brokenRunningActions.Open),
		},
		{
			name:  "a masked registration",
			state: "masked",
			want: wantPresent("masked", "Service: failed", ToneError,
				stoppedActions.Start, stoppedActions.Stop, stoppedActions.Update,
				stoppedActions.Delete, stoppedActions.Open),
		},
		{
			name:  "a runtime-masked registration",
			state: "masked-runtime",
			want: wantPresent("masked-runtime", "Service: failed", ToneError,
				stoppedActions.Start, stoppedActions.Stop, stoppedActions.Update,
				stoppedActions.Delete, stoppedActions.Open),
		},
		{
			name:    "a masked registration with a running instance",
			state:   "masked",
			running: true,
			want: wantPresent("masked", "Service: running", ToneSuccess,
				runningActions.Start, runningActions.Stop, runningActions.Update,
				runningActions.Delete, runningActions.Open),
		},
		{
			name:  "any other settled diagnosis is failed",
			state: "some-diagnosis",
			want: wantPresent("some-diagnosis", "Service: failed", ToneError,
				stoppedActions.Start, stoppedActions.Stop, stoppedActions.Update,
				stoppedActions.Delete, stoppedActions.Open),
		},
		{
			name:  "the state word is trimmed",
			state: "  running  ",
			want: wantPresent("running", "Service: running", ToneSuccess,
				runningActions.Start, runningActions.Stop, runningActions.Update,
				runningActions.Delete, runningActions.Open),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := From(tc.state, tc.running, tc.readErr)
			if got != tc.want {
				t.Errorf("From(%q, running=%t, readErr=%t) = %+v, want %+v",
					tc.state, tc.running, tc.readErr, got, tc.want)
			}

			// A hidden action is never enabled: the invariant every
			// surface relies on, whatever the state.
			for name, action := range map[string]Action{
				"install": got.Actions.Install,
				"start":   got.Actions.Start,
				"stop":    got.Actions.Stop,
				"update":  got.Actions.Update,
				"delete":  got.Actions.Delete,
				"open":    got.Actions.Open,
			} {
				if !action.Visible && action.Enabled {
					t.Errorf("the %s action is hidden but enabled: %+v", name, action)
				}
			}

			if got.Class == ClassUnknown {
				if got.Label != "" || got.Tone != "" {
					t.Errorf("an unknown read claims label %q or tone %q: it proved nothing", got.Label, got.Tone)
				}
				if got.Actions != (Actions{}) {
					t.Errorf("an unknown read offers the actions %+v: it proved nothing", got.Actions)
				}
			}
		})
	}
}
