// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/service"
)

// capturedLogRecord is one log record of a test: its message and the values
// of its attributes.
type capturedLogRecord struct {
	message string
	attrs   map[string]string
}

// capturedLogRecords records the log records of the host through the default
// logger the component loggers resolve on every call.
type capturedLogRecords struct {
	mu      sync.Mutex
	records []capturedLogRecord
}

// Enabled accepts every level.
func (h *capturedLogRecords) Enabled(context.Context, slog.Level) bool { return true }

// Handle records the message and the attributes of the record.
func (h *capturedLogRecords) Handle(_ context.Context, record slog.Record) error {
	attrs := map[string]string{}
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, capturedLogRecord{message: record.Message, attrs: attrs})
	return nil
}

// WithAttrs keeps the same recorder.
func (h *capturedLogRecords) WithAttrs([]slog.Attr) slog.Handler { return h }

// WithGroup keeps the same recorder.
func (h *capturedLogRecords) WithGroup(string) slog.Handler { return h }

// all returns a copy of the recorded records.
func (h *capturedLogRecords) all() []capturedLogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]capturedLogRecord(nil), h.records...)
}

// messages returns the recorded messages, for a failure report.
func (h *capturedLogRecords) messages() []string {
	records := h.all()
	messages := make([]string, 0, len(records))
	for _, record := range records {
		messages = append(messages, record.message)
	}
	return messages
}

// captureLogRecords installs a recorder as the default logger for the
// duration of the test.
func captureLogRecords(t *testing.T) *capturedLogRecords {
	t.Helper()
	records := &capturedLogRecords{}
	previous := slog.Default()
	slog.SetDefault(slog.New(records))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return records
}

// TestDisplayReadPolicyReportsHonestUnknownWhenTheReadRunsOut proves the
// failure class of the display policy: a read whose budget runs out reports
// the deadline instead of inventing a result, and the menu classifies that
// as the honest unknown state — a timeout is never proof of a machine
// without a service, so the install item is not enabled beside it.
//
// The stub blocks on the context of the read itself: a display policy that
// lost its budget would hang this test until the package timeout fails it.
func TestDisplayReadPolicyReportsHonestUnknownWhenTheReadRunsOut(t *testing.T) {
	replaceSeam(t, &serviceDisplayStatusTimeout, 50*time.Millisecond)
	replaceSeam(t, &serviceStatus, func(ctx context.Context) (service.Status, error) {
		<-ctx.Done()
		return service.Status{}, ctx.Err()
	})

	start := time.Now()
	status, err := serviceState.readForDisplay()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the display read returned (%q, %v), want the deadline of its budget", status.State, err)
	}
	if elapsed := time.Since(start); elapsed > serviceDisplayStatusTimeout+2*time.Second {
		t.Errorf("the display read took %s, want it bounded by the display budget", elapsed)
	}

	state := serviceMenuFor(status, err)
	if state.Line != "" || state.LineVisible {
		t.Errorf("the menu shows the line %q (visible=%t) for the timed-out read, want no line: "+
			"an unsettled read has no state to name", state.Line, state.LineVisible)
	}
	if state.InstallVisible || state.ActionsVisible || state.StartVisible || state.OpenVisible {
		t.Errorf("the timed-out read produced installVisible=%t actionsVisible=%t startVisible=%t openVisible=%t: "+
			"no claim was proven by it", state.InstallVisible, state.ActionsVisible, state.StartVisible, state.OpenVisible)
	}
}

// TestDisplayReadPolicyRetriesAMomentaryFailure proves the retry of one
// display pass: a read that fails once is tried again inside the same pass,
// and the pass reports the read that succeeded — so a momentary failure of
// the service control manager never surfaces as an empty tray submenu.
//
// It also pins the budget of the pass: the attempts and their pause stay
// inside serviceDisplayStatusTimeout, so the menu refresh and the window
// handler are never held longer than the budget they name.
func TestDisplayReadPolicyRetriesAMomentaryFailure(t *testing.T) {
	replaceSeam(t, &serviceDisplayStatusTimeout, 200*time.Millisecond)
	replaceSeam(t, &serviceReadRetryDelay, 5*time.Millisecond)
	attempts := 0
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		attempts++
		if attempts < 2 {
			return service.Status{}, errors.New("the service control manager did not answer")
		}
		return service.Status{State: service.StateRunning, Running: true}, nil
	})

	started := time.Now()
	status, err := serviceState.readForDisplay()
	elapsed := time.Since(started)

	assertNoError(t, err, "the display read of a momentary failure failed: %v", err)
	if status.State != service.StateRunning {
		t.Errorf("the display read reported %q, want the running service the second attempt read", status.State)
	}
	if attempts != 2 {
		t.Errorf("the display pass made %d attempts, want exactly two: the second one succeeded", attempts)
	}
	if elapsed > serviceDisplayStatusTimeout+2*time.Second {
		t.Errorf("the display pass took %s, want it bounded by its budget %s", elapsed, serviceDisplayStatusTimeout)
	}
	if state := serviceMenuFor(status, nil); !state.LineVisible || state.Line != "Service: running" {
		t.Errorf("the menu shows %+v after the recovered read, want the running line and its items", state)
	}
}

// TestDisplayReadPolicyBoundsTheAttemptsOfAFailingPass proves the other end
// of the retry: a read that keeps failing is tried at most
// serviceReadAttempts times in one pass, the pass reports the failure, and
// the menu it feeds shows nothing — no line, no item, and never a claim of
// "not installed". The pass stays inside the display budget, so the retries
// can never hold the menu or the window longer than the policy allows.
func TestDisplayReadPolicyBoundsTheAttemptsOfAFailingPass(t *testing.T) {
	replaceSeam(t, &serviceDisplayStatusTimeout, 200*time.Millisecond)
	replaceSeam(t, &serviceReadRetryDelay, 5*time.Millisecond)
	attempts := 0
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		attempts++
		return service.Status{}, errors.New("the service control manager did not answer")
	})

	started := time.Now()
	status, err := serviceState.readForDisplay()
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("the display read of a permanent failure reported no error")
	}
	if attempts != serviceReadAttempts {
		t.Errorf("the display pass made %d attempts, want the %d attempts of one pass", attempts, serviceReadAttempts)
	}
	if elapsed > serviceDisplayStatusTimeout+2*time.Second {
		t.Errorf("the display pass took %s, want it bounded by its budget %s", elapsed, serviceDisplayStatusTimeout)
	}
	state := serviceMenuFor(status, err)
	if state != (serviceMenuState{}) {
		t.Errorf("the menu shows %+v for a failing read, want nothing at all", state)
	}
	if state.Line == "Service: not installed" {
		t.Error("a failing read produced the absence line: an unread machine is not a machine without a service")
	}
}

// TestDisplayReadPolicyNeverWaitsForASettledState proves the second rule of
// the display policy beside the action policy: a pending state is a
// truthful result for a status surface — one read, one answer — so the
// policy reads exactly once and reports the transition the machine has
// right now instead of waiting for it to settle or reporting a pending
// error.
func TestDisplayReadPolicyNeverWaitsForASettledState(t *testing.T) {
	for _, pending := range []service.State{service.StateStarting, service.StateStopping} {
		t.Run(string(pending), func(t *testing.T) {
			reads := 0
			replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
				reads++
				return service.Status{State: pending}, nil
			})

			status, err := serviceState.readForDisplay()
			assertNoError(t, err, "the display read of the %s service failed: %v", pending, err)
			if status.State != pending {
				t.Errorf("the display read reported %q, want the state the machine has right now, %q",
					status.State, pending)
			}
			if reads != 1 {
				t.Errorf("the display policy read %d times, want the single read of a status surface: it must not wait for a settled state",
					reads)
			}
		})
	}
}

// TestActionReadPolicySettlesAndReportsPending proves that the action
// policy kept its semantics beside the display policy: it re-reads until
// the state settled, and a transition that outlives the budget is reported
// as the pending state of the gate instead of a decided one. The two
// policies are read through the same owner and must never be averaged into
// one behavior.
func TestActionReadPolicySettlesAndReportsPending(t *testing.T) {
	t.Run("waits for the transition to settle", func(t *testing.T) {
		reads := 0
		replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
			reads++
			if reads == 1 {
				return service.Status{State: service.StateStarting}, nil
			}
			return service.Status{State: service.StateRunning, Running: true}, nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), serviceStatusTimeout)
		defer cancel()

		status, err := serviceState.readForAction(ctx)
		assertNoError(t, err, "the action read failed on its way to a settled state: %v", err)
		if status.State != service.StateRunning {
			t.Errorf("the action read reported %q, want the settled running service", status.State)
		}
		if reads < 2 {
			t.Errorf("the action policy read %d times, want it to re-read until the state settled", reads)
		}
	})

	t.Run("reports a transition that outlives the budget as pending", func(t *testing.T) {
		replaceSeam(t, &serviceStatusTimeout, 50*time.Millisecond)
		replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
			return service.Status{State: service.StateStopping}, nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), serviceStatusTimeout)
		defer cancel()

		status, err := serviceState.readForAction(ctx)
		if !errors.Is(err, errServiceGatePending) {
			t.Fatalf("the action read returned the error %v, want the pending report of the gate", err)
		}
		if status.State != service.StateStopping {
			t.Errorf("the pending report carries the state %q, want the observed transition", status.State)
		}
	})
}

// TestRuntimeWindowReadAsksTheMenuToCatchUp proves the redraw seam of the
// Logs window: every state read made for the window ends with the
// request that makes the tray menu catch up — after the read, and for a
// failed read as well, so the honest unknown state a failure classifies as
// reaches the menu too. The cadence of the tray's own polling is not part of
// this request: it is the existing non-blocking refresh signal of the host.
func TestRuntimeWindowReadAsksTheMenuToCatchUp(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    service.Status
		readErr   error
		wantState string
		wantErr   bool
	}{
		{
			name:      "a settled read",
			status:    service.Status{State: service.StateStopped},
			wantState: "stopped",
		},
		{
			name:      "a failed read still asks for the catch-up",
			readErr:   errors.New("the service control manager did not answer"),
			wantState: "",
			wantErr:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readFinished := false
			replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
				readFinished = true
				return tc.status, tc.readErr
			})
			refreshes := 0

			state, err := readRuntimeServiceState(func() {
				if !readFinished {
					t.Error("the menu was asked to catch up before the read finished")
				}
				refreshes++
			})

			if tc.wantErr {
				if err == nil {
					t.Error("the window read returned no error although the read failed")
				}
			} else {
				assertNoError(t, err, "the window read failed: %v", err)
			}
			if state != tc.wantState {
				t.Errorf("the window read answered the state %q, want %q", state, tc.wantState)
			}
			if refreshes != 1 {
				t.Errorf("the window read asked the menu to catch up %d times, want exactly once after every read",
					refreshes)
			}
		})
	}
}

// TestStateReadRecordsReadAsReads pins the wording of the records of the
// state reads: both the display read and the gate's read say that the state
// was READ and carry its facts (state, running), and no record of a read may
// read as the outcome of an installation or another service action — a user
// has read "the Windows service is installed {running=false state=stopped}"
// as the report of an installation, and a record of a read must not offer
// that reading.
func TestStateReadRecordsReadAsReads(t *testing.T) {
	records := captureLogRecords(t)
	// The record is written for a CHANGE beside the last read, so this
	// test starts from an empty stamp: reads of earlier tests in this
	// binary must not make the first read here look unchanged.
	replaceSeam(t, &serviceReadLogLast, serviceReadStamp{})
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return service.Status{State: service.StateStopped}, nil
	})

	// The display read behind the status surfaces.
	if _, err := serviceState.readForDisplay(); err != nil {
		t.Fatalf("the display read failed: %v", err)
	}
	display, ok := findLogRecord(records.all(), "read the Windows service state")
	if !ok {
		t.Fatalf("the records are %v, want the record of the display read", records.messages())
	}
	if display.attrs["state"] != string(service.StateStopped) || display.attrs["running"] != "false" {
		t.Errorf("the record of the display read carries %v, want the facts state=stopped running=false",
			display.attrs)
	}

	// The read of the registration beside the question of the gate.
	replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
		return serviceChoiceCancel
	})
	serviceGate(testGateSingleton)
	gate, ok := findLogRecord(records.all(), "read the Windows service state at the launch gate")
	if !ok {
		t.Fatalf("the records are %v, want the record of the gate's read", records.messages())
	}
	if gate.attrs["state"] != string(service.StateStopped) || gate.attrs["running"] != "false" {
		t.Errorf("the record of the gate's read carries %v, want the facts state=stopped running=false",
			gate.attrs)
	}
	if _, exists := gate.attrs["url"]; !exists {
		t.Errorf("the record of the gate's read carries %v, want it to keep the address of the read", gate.attrs)
	}
	if _, exists := gate.attrs["dataDir"]; !exists {
		t.Errorf("the record of the gate's read carries %v, want it to keep the data directory of the read", gate.attrs)
	}

	for _, record := range records.all() {
		if strings.Contains(strings.ToLower(record.message), "installed") {
			t.Errorf("the record %q reads as the outcome of an installation: a record of a read must say it was a read",
				record.message)
		}
	}
}

// TestDataDirAndDisplayReadsKeepTheirOwnBudgets proves that the one owner
// names a budget per caller instead of forcing one budget on every single
// read: a registration that answers slower than the display budget but
// within the data-directory budget still yields the registered directory
// for the confirmation text, while the status surface classifies the very
// same slow read as the honest unknown at its own, shorter budget.
//
// The test fails whichever way the two paths are collapsed onto one budget:
// a data-directory read under the display budget times out and falls back
// to the default of the scope, and a display read under the data-directory
// budget succeeds and can no longer report the unknown state.
func TestDataDirAndDisplayReadsKeepTheirOwnBudgets(t *testing.T) {
	const readDuration = 300 * time.Millisecond
	replaceSeam(t, &serviceDisplayStatusTimeout, 50*time.Millisecond)
	replaceSeam(t, &serviceStatusTimeout, 2*time.Second)
	replaceSeam(t, &serviceStatus, func(ctx context.Context) (service.Status, error) {
		// The machine answers, but slower than the display budget: the
		// read ends with the registered directory or with the deadline
		// of whichever budget it was started under.
		select {
		case <-time.After(readDuration):
			return service.Status{
				State: service.StateStopped,
				Paths: runtimepaths.Paths{StateDir: `D:\hfdesk`},
			}, nil
		case <-ctx.Done():
			return service.Status{}, ctx.Err()
		}
	})

	// The data-directory read passes its own budget and names the
	// registered directory — not the default of the scope.
	if got := serviceDataDir(); got != `D:\hfdesk` {
		t.Errorf("serviceDataDir() = %q, want the registered directory: its own named budget must outlast the slower read",
			got)
	}

	// The display policy runs out at its own, shorter budget: the surface
	// reports the honest unknown instead of the state the slower read
	// would have delivered.
	status, err := serviceState.readForDisplay()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the display read returned (%q, %v), want the deadline of the display budget", status.State, err)
	}
	if state := serviceMenuFor(status, err); state != (serviceMenuState{}) {
		t.Errorf("the menu shows %+v for the display read that ran out of budget, want the empty state: "+
			"no line and no item", state)
	}

	// A read that fails keeps the fallback to the default of the scope,
	// exactly as before the reads joined the owner.
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return service.Status{}, errors.New("the service control manager did not answer")
	})
	paths, err := service.DefaultPaths(service.ScopeSystem)
	assertNoError(t, err, "resolve the default paths of the scope: %v", err)
	if got := serviceDataDir(); got != paths.StateDir {
		t.Errorf("serviceDataDir() = %q beside a failed read, want the default of the scope %q",
			got, paths.StateDir)
	}
}

// TestDisplayReadsRecordOnlyChanges pins the log policy of the periodic
// state reads: a read that reports what the last record already carries
// writes NOTHING — the open Logs window polls every few seconds beside
// the menu refresh, and at rest that must add no lines — while every
// CHANGE is recorded with the state it saw and, when known, the state it
// moved from. A read that starts failing records once, and the recovery
// records again: the silence belongs to the unchanged repeat, never to a
// transition.
func TestDisplayReadsRecordOnlyChanges(t *testing.T) {
	records := captureLogRecords(t)
	// This test owns the stamp: reads of earlier tests in this binary
	// must not make its first read look unchanged.
	replaceSeam(t, &serviceReadLogLast, serviceReadStamp{})
	current := service.Status{State: service.StateStopped}
	var readErr error
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return current, readErr
	})
	reads := func() []capturedLogRecord {
		var found []capturedLogRecord
		for _, record := range records.all() {
			if record.message == "read the Windows service state" {
				found = append(found, record)
			}
		}
		return found
	}
	readOnce := func() {
		// The display policy: one read, one answer. One stage below
		// the read fails on purpose, so its error is not checked here.
		_, _ = serviceState.readForDisplay()
	}

	// The first read records what it saw.
	readOnce()
	if got := len(reads()); got != 1 {
		t.Fatalf("the first read recorded %d lines, want the one record of the first observation: %v",
			got, records.messages())
	}

	// The machine rests: repeated reads of the same state record
	// nothing — this is the open window at its status poll.
	readOnce()
	readOnce()
	if got := len(reads()); got != 1 {
		t.Errorf("the resting reads recorded %d lines in total, want the single first record: %v",
			got, records.messages())
	}

	// A state change records exactly once, and the record names the
	// state it saw beside the one it moved from.
	current = service.Status{State: service.StateRunning, Running: true}
	readOnce()
	changed := reads()
	if len(changed) != 2 {
		t.Fatalf("the state change recorded %d lines in total, want the first record plus the change: %v",
			len(changed), records.messages())
	}
	last := changed[1]
	if last.attrs["state"] != string(service.StateRunning) || last.attrs["running"] != "true" {
		t.Errorf("the record of the change carries %v, want the facts of the new state", last.attrs)
	}
	if last.attrs["previous"] != string(service.StateStopped) {
		t.Errorf("the record of the change carries previous=%q, want the state it moved from", last.attrs["previous"])
	}

	// The new state rests: still no lines.
	readOnce()
	if got := len(reads()); got != 2 {
		t.Errorf("the resting reads after the change recorded %d lines in total, want the two records so far: %v",
			got, records.messages())
	}

	// A read that starts failing records once with its error, and the
	// identical failing read after it stays silent.
	readErr = errors.New("the service control manager did not answer")
	current = service.Status{}
	readOnce()
	readOnce()
	failing := reads()
	if len(failing) != 3 {
		t.Fatalf("the failing reads recorded %d lines in total, want one record of the failure: %v",
			len(failing), records.messages())
	}
	if failing[2].attrs["error"] != readErr.Error() {
		t.Errorf("the record of the failure carries %v, want the error of the read", failing[2].attrs)
	}

	// The recovery is a change again: it records with the state it
	// reads now.
	readErr = nil
	current = service.Status{State: service.StateStopped}
	readOnce()
	recovered := reads()
	if len(recovered) != 4 {
		t.Fatalf("the recovery recorded %d lines in total, want the fourth record: %v",
			len(recovered), records.messages())
	}
	if recovered[3].attrs["state"] != string(service.StateStopped) {
		t.Errorf("the record of the recovery carries %v, want the state it settled into", recovered[3].attrs)
	}
}

// findLogRecord returns the record with the exact message.
func findLogRecord(records []capturedLogRecord, message string) (capturedLogRecord, bool) {
	for _, record := range records {
		if record.message == message {
			return record, true
		}
	}
	return capturedLogRecord{}, false
}
