// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// TestSCMRollbackSettlesTheStopItInherits checks the rollback of a failed stop: a
// control that was never delivered restores the registration and starts the
// previous instance again only when the state proved it stopped, while a stop
// that may still put the instance down keeps the registration and starts
// nothing.
func TestSCMRollbackSettlesTheStopItInherits(t *testing.T) {
	shortenSCMStopBudget(t, 10*time.Millisecond)
	readErr := errors.New("the service control manager did not answer")
	cases := []struct {
		name         string
		mode         scmRollbackMode
		controlErr   error
		queryErr     error
		queryErrs    []error
		state        svc.State
		settle       bool
		wantOutcome  scmStopOutcome
		wantWrites   int
		wantStarts   int
		wantDirsSafe bool
		wantReports  []string
	}{
		{
			name:         "a control that was never delivered",
			controlErr:   errors.New("the service control manager refused the stop"),
			wantOutcome:  scmStopNotDelivered,
			wantWrites:   1,
			wantDirsSafe: true,
			wantReports:  []string{"the configuration of the previous registration was restored", "the instance still runs", "no instance was started"},
		},
		{
			name:         "the previous instance then stops",
			settle:       true,
			wantOutcome:  scmStopUnsettled,
			wantWrites:   1,
			wantStarts:   1,
			wantDirsSafe: true,
			wantReports:  []string{"the previous instance was proven stopped", scmRollbackSuccessClaim},
		},
		{
			name:        "a stop that stays pending",
			wantOutcome: scmStopUnsettled,
			wantReports: []string{"was not proven to be stopped", "no instance was started", "hfdesk service status", "did not stop within"},
		},
		{
			name:         "a state that cannot be read",
			queryErr:     readErr,
			wantOutcome:  scmStopNotDelivered,
			wantWrites:   1,
			wantDirsSafe: true,
			wantReports:  []string{"the configuration of the previous registration was restored", "the running state of the instance could not be confirmed", "no instance was started", "hfdesk service status", "did not answer"},
		},
		{
			name:         "a state that cannot be read first and then reads as running",
			queryErrs:    []error{readErr},
			wantOutcome:  scmStopNotDelivered,
			wantWrites:   1,
			wantDirsSafe: true,
			wantReports:  []string{"the configuration of the previous registration was restored", "the instance still runs", "no instance was started"},
		},
		{
			name:         "a state that cannot be read first and then reads as stopped",
			queryErrs:    []error{readErr},
			state:        svc.Stopped,
			wantOutcome:  scmStopNotDelivered,
			wantWrites:   1,
			wantStarts:   1,
			wantDirsSafe: true,
			wantReports:  []string{"the previous instance was proven stopped", "the configuration of the previous registration was restored", scmRollbackSuccessClaim},
		},
		{
			name:         "a state that cannot be read first and then stays stopping",
			queryErrs:    []error{readErr},
			state:        svc.StopPending,
			wantOutcome:  scmStopNotDelivered,
			wantWrites:   1,
			wantDirsSafe: true,
			wantReports:  []string{"the configuration of the previous registration was restored", "the running state of the instance could not be confirmed", "no instance was started", "hfdesk service status"},
		},
		{
			name:        "the instance of a failed start",
			mode:        scmRollbackRestart,
			wantOutcome: scmStopUnsettled,
			wantReports: []string{"the instance it started was not proven to be stopped"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := &scmTestRecoveryTarget{}
			target.state = svc.Running
			target.pendingAfterControl = true
			target.controlErr = tc.controlErr
			target.queryErr = tc.queryErr
			target.queryErrs = tc.queryErrs
			if tc.state != 0 {
				target.state = tc.state
			}
			manager := &scmManager{scope: ScopeSystem}
			previous := scmTestRunningPrevious()
			failure := errors.New("the stop did not settle")

			outcome, err := manager.stopService(context.Background(), target)
			if err == nil {
				t.Fatal("stopService() = nil, want a failed stop")
			}
			if outcome != tc.wantOutcome {
				t.Errorf("stopService() outcome = %d, want %d", outcome, tc.wantOutcome)
			}
			mode := tc.mode
			if mode == scmRollbackRegistration {
				mode = scmStopRollbackMode(outcome)
			}
			if tc.settle {
				// The accepted control reaches the stopped state after the
				// failed call returned, and the supervisor answers again.
				target.state = svc.Stopped
				target.queryAfterControlErr = nil
			}
			controlsBeforeRollback := target.controls
			dirsSafe, rollbackErr := manager.rollbackInstallation(context.Background(), target, previous, mode, failure)
			if rollbackErr == nil || !errors.Is(rollbackErr, failure) {
				t.Fatalf("rollbackInstallation() = %v, want the original failure wrapped", rollbackErr)
			}
			if dirsSafe != tc.wantDirsSafe {
				t.Errorf("rollbackInstallation() directory cleanup safe = %v, want %v", dirsSafe, tc.wantDirsSafe)
			}
			if outcome == scmStopNotDelivered && target.controls != controlsBeforeRollback {
				t.Errorf("the rollback delivered %d stop controls after a stop that was never delivered, want none", target.controls-controlsBeforeRollback)
			}
			if len(target.writes) != tc.wantWrites {
				t.Errorf("the rollback wrote %d configurations, want %d", len(target.writes), tc.wantWrites)
			}
			for _, write := range target.writes {
				if !scmSameRegistration(write.BinaryPathName, previous.config.BinaryPathName) {
					t.Errorf("the rollback wrote %q, want the previous command line %q", write.BinaryPathName, previous.config.BinaryPathName)
				}
			}
			if target.starts != tc.wantStarts || target.deletes != 0 {
				t.Errorf("the rollback started %d instances and removed %d registrations, want %d starts and no removal",
					target.starts, target.deletes, tc.wantStarts)
			}
			if tc.wantStarts == 1 && (!slices.Contains(target.excluded, previous.processID) ||
				target.started != scmTestOptions().Executable) {
				t.Errorf("the rollback started %q excluding %v, want the previous program without the process %d",
					target.started, target.excluded, previous.processID)
			}
			for _, want := range tc.wantReports {
				if !strings.Contains(rollbackErr.Error(), want) {
					t.Errorf("the report %q does not mention %q", rollbackErr, want)
				}
			}
			// Every rollback report carries the one action: reading the
			// state of the service decides what the failure left behind.
			if !strings.Contains(rollbackErr.Error(), "hfdesk service status") {
				t.Errorf("the report %q names no state command, want the one action of the report", rollbackErr)
			}
		})
	}
}

// TestSCMRollbackOfAFailureOutsideTheInstanceRestoresTheRegistration checks that
// a failure that never touched the running instance puts the previous
// configuration back and leaves the instance alone.
func TestSCMRollbackOfAFailureOutsideTheInstanceRestoresTheRegistration(t *testing.T) {
	target := &scmTestRecoveryTarget{}
	target.state = svc.Running
	previous := scmTestRunningPrevious()
	failure := errors.New("the failure recovery could not be configured")

	dirsSafe, err := (&scmManager{scope: ScopeSystem}).rollbackInstallation(context.Background(), target, previous, scmRollbackRegistration, failure)
	if err == nil || !errors.Is(err, failure) {
		t.Fatalf("rollbackInstallation() = %v, want the original failure wrapped", err)
	}
	if !dirsSafe {
		t.Error("rollbackInstallation() says directory cleanup is unsafe after a verified configuration restore")
	}
	if len(target.writes) != 1 || target.starts != 0 || target.controls != 0 || target.deletes != 0 {
		t.Errorf("the rollback wrote %d configurations, started %d instances, delivered %d controls and removed %d registrations, want one write and no other change",
			len(target.writes), target.starts, target.controls, target.deletes)
	}
	for _, want := range []string{"the configuration of the previous registration was restored", "the running instance was not restarted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the report %q does not mention %q", err, want)
		}
	}
}

func TestSCMRollbackStartVerificationRemainsImageOnly(t *testing.T) {
	opts := scmTestOptions()
	probed := false
	replaceSCMTestSeam(t, &scmStartService, func(*mgr.Service) error { return nil })
	replaceSCMTestSeam(t, &scmQueryStatus, func(*mgr.Service) (svc.Status, error) {
		return svc.Status{State: svc.Running, ProcessId: 8142}, nil
	})
	replaceSCMTestSeam(t, &scmQueryProcessImageName, func(uint32) (string, error) {
		return opts.Executable, nil
	})
	replaceSCMTestSeam(t, &serviceInstallProbe, func(string, string) (bool, error) {
		probed = true
		return false, errors.New("readiness is not part of rollback image proof")
	})

	image, err := (&scmManager{scope: ScopeSystem}).startAndVerify(
		context.Background(), &mgr.Service{}, opts.Executable)
	if err != nil || image != scmImageVerified {
		t.Fatalf("startAndVerify() = %v, %v, want only the verified restored image", image, err)
	}
	if probed {
		t.Error("rollback image verification unexpectedly required HTTP readiness")
	}
}

// scmRollbackSuccessClaim is the phrase that reports the previous instance as in
// service again. It is the claim a restart rollback may only make for a verified
// restart, so the test of the outcome sentences looks for exactly this phrase.
const scmRollbackSuccessClaim = "the previous instance was started again"

// TestSCMRestartRollbackSentence checks that the outcome of a restart rollback is
// only described as far as the rollback proved it.
//
// The sentence is the whole report a caller reads next to the failure of the
// installation, so it is the place the truthfulness rule lives: the previous
// instance is only reported as started again for a verified restart, a
// registration that could not name its program is reported as such, and a
// registration that could not be put back is never reported as restored. The
// stop of the instance of the failed installation comes before the restore, and
// the sentence of a failed stop says so: the registration of the failed
// installation is kept, because its instance may still run.
func TestSCMRestartRollbackSentence(t *testing.T) {
	const stopReason = "the instance did not stop within 2m0s"
	const restoreReason = "the service control manager refused the write of the registration"
	const startReason = `the instance does not run "C:\\ProgramData\\HFDesk\\hfdesk.exe" within 2m0s`
	const readReason = "the service control manager did not answer"

	stopped := scmRestartRollback{stopped: true}
	restored := scmRestartRollback{stopped: true, restored: true}
	programKnown := scmRestartRollback{stopped: true, restored: true, programKnown: true}
	started := scmRestartRollback{stopped: true, restored: true, programKnown: true, started: true}
	undeliveredRestored := scmRestartRollback{attempt: scmRollbackUndeliveredInstance, restored: true}
	undeliveredRunning := scmRestartRollback{attempt: scmRollbackUndeliveredInstance, restored: true, running: true}
	undeliveredStopped := scmRestartRollback{attempt: scmRollbackUndeliveredInstance, stopped: true, restored: true, programKnown: true, started: true, verified: true}

	cases := []struct {
		name string
		// facts are the observations of the rollback up to the step that did
		// not reach its goal.
		facts scmRestartRollback
		// reason is the detail the rollback embeds for a step that failed.
		reason string
		// want contains the phrases the sentence has to carry.
		want []string
		// verified reports whether the sentence may claim the previous
		// instance as started again.
		verified bool
	}{
		{
			name:     "the instance of the failed installation did not stop",
			facts:    scmRestartRollback{},
			reason:   stopReason,
			want:     []string{"was kept", "was not proven to be stopped", stopReason, "the previous registration was not restored"},
			verified: false,
		},
		{
			name:     "the instance the failed installation replaced did not stop",
			facts:    scmRestartRollback{attempt: scmRollbackPreviousInstance},
			reason:   stopReason,
			want:     []string{"instance it replaced was not proven to be stopped", stopReason, "no instance was started", "hfdesk service status"},
			verified: false,
		},
		{
			name:     "the previous registration could not be put back",
			facts:    stopped,
			reason:   restoreReason,
			want:     []string{"was stopped", "could not be put back", restoreReason},
			verified: false,
		},
		{
			name:     "the restored registration names no program",
			facts:    restored,
			reason:   "",
			want:     []string{"was stopped", "was restored", "names no program", "no instance was started", "is stopped"},
			verified: false,
		},
		{
			name:     "the previous instance was not proven to be in service again",
			facts:    programKnown,
			reason:   startReason,
			want:     []string{"was stopped", "was restored", "was not proven to be in service again", startReason},
			verified: false,
		},
		{
			name:     "the started instance was not proven to be the previous program",
			facts:    started,
			reason:   "",
			want:     []string{"was stopped", "was restored", "was not proven to be the previous program", "the image of the process could not be read"},
			verified: false,
		},
		{
			name:     "the previous instance was started again",
			facts:    scmRestartRollback{stopped: true, restored: true, programKnown: true, started: true, verified: true},
			reason:   "",
			want:     []string{"was stopped", "was restored", scmRollbackSuccessClaim, "the failure recovery of the registration was not part of the restore"},
			verified: true,
		},
		{
			name:     "the previous instance was started again after its stop",
			facts:    scmRestartRollback{attempt: scmRollbackPreviousInstance, stopped: true, restored: true, programKnown: true, started: true, verified: true},
			reason:   "",
			want:     []string{"the previous instance was proven stopped", "was restored", scmRollbackSuccessClaim},
			verified: true,
		},
		{
			name:     "a stop that was never delivered and the instance still runs",
			facts:    undeliveredRunning,
			reason:   "",
			want:     []string{"no stop control was delivered", "was restored", "still runs", "no instance was started"},
			verified: false,
		},
		{
			name:     "a stop that was never delivered and the state cannot be read",
			facts:    undeliveredRestored,
			reason:   readReason,
			want:     []string{"no stop control was delivered", "was restored", "could not be confirmed", "no instance was started", "hfdesk service status", readReason},
			verified: false,
		},
		{
			name:     "a stop that was never delivered and the previous registration could not be put back",
			facts:    scmRestartRollback{attempt: scmRollbackUndeliveredInstance},
			reason:   restoreReason,
			want:     []string{"no stop control was delivered", "could not be put back", restoreReason},
			verified: false,
		},
		{
			name:     "the previous instance was stopped after its stop was never delivered",
			facts:    undeliveredStopped,
			reason:   "",
			want:     []string{"the previous instance was proven stopped", "was restored", scmRollbackSuccessClaim},
			verified: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sentence := scmRestartRollbackSentence(tc.facts, tc.reason)
			if sentence == "" {
				t.Fatal("scmRestartRollbackSentence() is empty, want the outcome of the rollback")
			}
			for _, want := range tc.want {
				if !strings.Contains(sentence, want) {
					t.Errorf("the sentence %q does not carry %q", sentence, want)
				}
			}
			// Every branch of the rollback sentence carries the one
			// action: reading the state of the service decides what the
			// failure left behind.
			if !strings.Contains(sentence, "hfdesk service status") {
				t.Errorf("the sentence %q names no state command, want the one action of the report", sentence)
			}
			// The success claim is only made for a verified restart: every
			// other outcome reports what it proved instead.
			if claimed := strings.Contains(sentence, scmRollbackSuccessClaim); claimed != tc.verified {
				t.Errorf("the sentence %q claims the previous instance was started again = %v, want %v", sentence, claimed, tc.verified)
			}
			// A rollback that did not prove the stop never claims the
			// restore, unless the stop was never delivered: there was no
			// control that could have put the instance down.
			undelivered := tc.facts.attempt == scmRollbackUndeliveredInstance
			if !tc.facts.stopped && !undelivered && strings.Contains(sentence, "the configuration of the previous registration was restored") {
				t.Errorf("the sentence %q claims the previous configuration was restored while the instance of the failed installation was not proven to be stopped", sentence)
			}
		})
	}
}
