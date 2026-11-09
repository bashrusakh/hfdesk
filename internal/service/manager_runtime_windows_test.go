// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/bashrusakh/hfdesk/internal/server"
)

// TestSCMStopWaitCoversTheShutdownOfTheServer checks that the wait of the
// manager for the stopped instance covers the graceful shutdown of the
// server.
//
// The server publishes the budget of its own orderly shutdown: the drain of
// the HTTP server and the close of the job manager, each with its own budget.
// The manager has to wait at least the total of those budgets, or it reports
// a failed stop while the instance is still saving its state.
//
// The wait is the shared budget of the service layer and not a number of its
// own: a second copy of the same decision would drift below the budget of
// the server.
func TestSCMStopWaitCoversTheShutdownOfTheServer(t *testing.T) {
	if ShutdownBudget < server.GracefulShutdownTimeout {
		t.Errorf("the shared budget of the service layer = %s, want at least %s", ShutdownBudget, server.GracefulShutdownTimeout)
	}
	if scmStopTimeout < server.GracefulShutdownTimeout {
		t.Errorf("the stop wait of the manager = %s, want at least %s: the wait of the manager has to cover the graceful shutdown of the server", scmStopTimeout, server.GracefulShutdownTimeout)
	}
	if scmStopTimeout != ShutdownBudget {
		t.Errorf("the stop wait of the manager = %s, want the shared budget %s", scmStopTimeout, ShutdownBudget)
	}
}

// TestSCMStopRetriesTheControlOfAnInstanceThatIsStillStarting checks that a
// stop the supervisor refuses because the instance is still starting is
// delivered again, and not given up on.
//
// The service control manager refuses the control of an instance that did not
// report the running state yet (ERROR_SERVICE_CANNOT_ACCEPT_CTRL). The
// refusal is not the end of the stop: the control is delivered again inside
// the wait, until the instance stops or the budget of the wait is over.
func TestSCMStopRetriesTheControlOfAnInstanceThatIsStillStarting(t *testing.T) {
	handle := &scmTestServiceControl{state: svc.StartPending, refusals: 2}

	manager := &scmManager{scope: ScopeSystem}
	if _, err := manager.stopService(context.Background(), handle); err != nil {
		t.Fatalf("stopService() = %v, want nil: the refusal of a starting instance is not the end of the stop", err)
	}
	if handle.controls < 3 {
		t.Errorf("the stop control was delivered %d times, want it delivered again after every refusal", handle.controls)
	}
	if handle.state != svc.Stopped {
		t.Errorf("the state of the instance = %v, want %v", handle.state, svc.Stopped)
	}
}

// TestSCMStopThatDoesNotSettleIsNotReportedAsRunning checks that an accepted stop
// is never reported as a running instance: the delivered control decides the
// outcome, not a later read of the state.
func TestSCMStopThatDoesNotSettleIsNotReportedAsRunning(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, handle *scmTestServiceControl) context.Context
		wantErr string
	}{
		{"cancellation", func(t *testing.T, handle *scmTestServiceControl) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			handle.onControl = cancel
			return ctx
		}, ""},
		{"query error", func(t *testing.T, handle *scmTestServiceControl) context.Context {
			handle.queryAfterControlErr = errors.New("the service control manager did not answer")
			return context.Background()
		}, "did not answer"},
		{"timeout", func(t *testing.T, handle *scmTestServiceControl) context.Context {
			shortenSCMStopBudget(t, 10*time.Millisecond)
			return context.Background()
		}, "did not stop within"},
		// The accepted control decides the classification, even when a later
		// read still reports the running state.
		{"running read after the control", func(t *testing.T, handle *scmTestServiceControl) context.Context {
			shortenSCMStopBudget(t, 10*time.Millisecond)
			handle.stateAfterControl = svc.Running
			return context.Background()
		}, "did not stop within"},
		{"already stopping", func(t *testing.T, handle *scmTestServiceControl) context.Context {
			shortenSCMStopBudget(t, 10*time.Millisecond)
			handle.state = svc.StopPending
			handle.refusals = 100
			return context.Background()
		}, "did not stop within"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handle := &scmTestServiceControl{state: svc.Running, pendingAfterControl: true}
			outcome, err := (&scmManager{scope: ScopeSystem}).stopService(tc.prepare(t, handle), handle)
			if outcome != scmStopUnsettled {
				t.Errorf("the outcome = %d, want the unsettled stop", outcome)
			}
			switch {
			case err == nil:
				t.Fatal("stopService() = nil, want a failed stop")
			case tc.wantErr == "" && !errors.Is(err, context.Canceled):
				t.Errorf("stopService() = %v, want the cancellation of the wait", err)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("stopService() = %v, want %q", err, tc.wantErr)
			}
			// The stop timeout carries the one action: reading the
			// state of the service shows what the instance did.
			if tc.wantErr == "did not stop within" && !strings.Contains(err.Error(), `run "hfdesk service status"`) {
				t.Errorf("the stop timeout %q carries no state command, want the one action of the report", err)
			}
		})
	}
}

// desktopVerifyBudget is the independent outer wait of the desktop service
// helper (internal/desktop/service_helper_windows.go, serviceVerifyBudget).
//
// The desktop package imports this one, so this test cannot read the value back
// without an import cycle; the number is repeated here to prove that the
// verification of a single start still fits inside the wait that wraps an
// update.
const desktopVerifyBudget = 60 * time.Second

// TestSCMVerifyWaitCoversTheReadinessWait checks that the wait for a restarted
// instance to run the registered program is sized above the readiness wait of
// the instance.
//
// The service control manager reports a started instance as running only after
// the instance itself waited for its HTTP server, so a verification that gave up
// at the readiness budget would fail a start that is still inside its own
// budget. The wait is derived from that budget, and it stays inside the
// independent outer wait of the desktop helper that wraps an update.
func TestSCMVerifyWaitCoversTheReadinessWait(t *testing.T) {
	if scmVerifyTimeout <= scmReadinessTimeout {
		t.Errorf("the verify wait of the manager = %s, want more than the readiness budget %s: the instance reports the running state only after it waited for its HTTP server", scmVerifyTimeout, scmReadinessTimeout)
	}
	if scmVerifyTimeout > desktopVerifyBudget {
		t.Errorf("the verify wait of the manager = %s, want at most the outer wait %s of the desktop service helper", scmVerifyTimeout, desktopVerifyBudget)
	}
	if scmVerifyPollInterval <= 0 || scmVerifyPollInterval > scmVerifyTimeout {
		t.Errorf("the verify poll interval = %s, want a positive pause inside the wait %s", scmVerifyPollInterval, scmVerifyTimeout)
	}
}

// TestSCMVerifyRunningExecutableKeepsIndependentImageAndProbeEvidence checks
// that readiness failure reaches the timeout while a verified process image
// remains a separate fact.
//
// The service control manager reports the instance as running with the image of
// the test process, so the probe is the only step that fails. The error has to
// name the address and the probe failure: a verification that keeps only the
// empty reason of a settled image read hides why the instance never came up.
func TestSCMVerifyRunningExecutableKeepsTheProbeCause(t *testing.T) {
	processID := uint32(os.Getpid())
	executable, err := queryProcessImageName(processID)
	if err != nil {
		t.Fatalf("the image of the test process: %v", err)
	}

	previousQuery := scmQueryStatus
	t.Cleanup(func() { scmQueryStatus = previousQuery })
	scmQueryStatus = func(*mgr.Service) (svc.Status, error) {
		return svc.Status{State: svc.Running, ProcessId: processID}, nil
	}

	previousProbe := serviceInstallProbe
	t.Cleanup(func() { serviceInstallProbe = previousProbe })
	probeErr := errors.New("the health endpoint did not answer")
	probed := false
	serviceInstallProbe = func(string, string) (bool, error) {
		probed = true
		return false, probeErr
	}

	shortenSCMVerifyBudget(t, 10*time.Millisecond)

	const listen = "127.0.0.1:8080"
	check, verified, err := (&scmManager{scope: ScopeSystem}).verifyRunningExecutableVersion(
		context.Background(), &mgr.Service{}, executable, listen, "1.2.3",
	)
	if err == nil {
		t.Fatal("verifyRunningExecutableVersion() = nil, want the timeout of the instance that does not answer")
	}
	if !probed {
		t.Fatalf("verifyRunningExecutableVersion() = %q before the readiness probe was asked", err)
	}
	if check != scmImageVerified || verified {
		t.Errorf("verifyRunningExecutableVersion() = %d, %t, want verified image evidence and no version claim", check, verified)
	}
	for _, want := range []string{listen, probeErr.Error()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("verifyRunningExecutableVersion() = %q, want an error that mentions %q", err, want)
		}
	}
}

// TestSCMProcessExcluded checks the decision that keeps a process of the failed
// attempt from being taken for the running instance.
//
// The installation excludes the process it replaced; the restart rollback
// excludes the process of the failed attempt next to it. A process identifier of
// zero names no process, so it never excludes one: the caller passes the process
// the service control manager reported even when it reported none.
func TestSCMProcessExcluded(t *testing.T) {
	cases := []struct {
		name      string
		exclude   []uint32
		processID uint32
		want      bool
	}{
		{"no excluded process", nil, 4711, false},
		{"the process of the attempt", []uint32{4711}, 4711, true},
		{"another process", []uint32{4711}, 4712, false},
		{"the process of the failed attempt next to the replaced one", []uint32{4710, 4711}, 4711, true},
		{"a replaced process the manager reported as none", []uint32{0, 4711}, 4711, true},
		{"no process at all", []uint32{0}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmProcessExcluded(tc.exclude, tc.processID); got != tc.want {
				t.Errorf("scmProcessExcluded(%v, %d) = %v, want %v", tc.exclude, tc.processID, got, tc.want)
			}
		})
	}
}

// TestSCMStopOfAnInstanceThatIsStoppedIsACleanNoOp checks the first step of the
// restart rollback on an instance that does not run.
//
// The rollback stops the instance of the failed installation before it restores
// the registration. An instance the failed installation never started, or one
// whose process already ended, is stopped already: the stop is a clean no-op with
// no control delivered, so the restore proceeds instead of failing over a service
// that has nothing to stop.
func TestSCMStopOfAnInstanceThatIsStoppedIsACleanNoOp(t *testing.T) {
	handle := &scmTestServiceControl{state: svc.Stopped}

	manager := &scmManager{scope: ScopeSystem}
	if _, err := manager.stopService(context.Background(), handle); err != nil {
		t.Fatalf("stopService() = %v, want nil for an instance that does not run", err)
	}
	if handle.controls != 0 {
		t.Errorf("the stop control was delivered %d times, want none for an instance that does not run", handle.controls)
	}
	if handle.state != svc.Stopped {
		t.Errorf("the state of the instance = %v, want %v", handle.state, svc.Stopped)
	}
}

// TestSCMStartAndVerifyExcludesEveryProcessOfTheAttempt checks the seam the
// restart rollback needs at compile time.
//
// The verification of a start takes the processes it must not accept as the
// running instance, so the installation excludes the process it replaced and the
// rollback excludes the process of the failed attempt next to it. A verification
// that could only exclude one process would let the rollback accept the process
// it did not start.
//
// The sequence itself (stop the instance of the failed installation, restore the
// registration, start the previous program, verify it) needs a registration of
// the service control manager: a test cannot create one without an elevated
// shell, so the sequence is only proven on a machine that runs the elevated
// tests.
func TestSCMStartAndVerifyExcludesEveryProcessOfTheAttempt(t *testing.T) {
	var start func(*scmManager, context.Context, *mgr.Service, string, ...uint32) (scmImageCheck, error) = (*scmManager).startAndVerify
	var verify func(*scmManager, context.Context, *mgr.Service, string, ...uint32) (scmImageCheck, error) = (*scmManager).verifyRunningExecutable
	if start == nil || verify == nil {
		t.Fatal("the verification of a start is gone, the restart rollback cannot prove the instance it started")
	}

	// The rollback excludes the process the installation replaced and the
	// process of the failed attempt: the exclusion list it passes keeps the
	// process it did not start out of the answer.
	exclude := []uint32{4710, 4711}
	if !scmProcessExcluded(exclude, 4711) {
		t.Errorf("scmProcessExcluded(%v, 4711) = false, want the process of the failed attempt excluded", exclude)
	}
	if !scmProcessExcluded(exclude, 4710) {
		t.Errorf("scmProcessExcluded(%v, 4710) = false, want the replaced process excluded", exclude)
	}
	if scmProcessExcluded(exclude, 4712) {
		t.Errorf("scmProcessExcluded(%v, 4712) = true, want the process the rollback started accepted", exclude)
	}
}
