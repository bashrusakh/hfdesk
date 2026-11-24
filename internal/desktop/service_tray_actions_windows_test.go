// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
	"golang.org/x/sys/windows"
)

// waitForTrayCondition polls until the condition holds and fails the test
// when it never does. The end paths of a launched helper run on a goroutine
// of their own — the guard release and the menu catch-up follow the exit of
// the helper — so a test that proves them waits for them instead of
// assuming an order.
func waitForTrayCondition(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestTrayServiceActionsRefuseASecondActionOfAnyType proves that the ONE
// service-action guard covers every action of every surface: while an action
// runs, a request of any other surface — the install item, the Service...
// dialog, and each of the four lifecycle items — is refused with its own
// honest report and performs nothing: no state read, no confirmation, no
// dialog, no helper.
func TestTrayServiceActionsRefuseASecondActionOfAnyType(t *testing.T) {
	freshServiceInstallGuard(t)
	serviceActionRunning.Store(true)
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		t.Fatal("a refused concurrent request read the service state")
		return service.Status{}, nil
	})
	replaceSeam(t, &confirmServiceInstall, func(string) bool {
		t.Fatal("a refused concurrent request asked the installation confirmation")
		return true
	})
	replaceSeam(t, &confirmServiceAction, func(string, string) bool {
		t.Fatal("a refused concurrent request asked for a confirmation")
		return true
	})
	replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
		t.Fatal("a refused concurrent request opened the service dialog")
		return serviceChoiceCancel
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		t.Fatal("a refused concurrent request launched an elevated helper")
		return 0, nil
	})
	replaceSeam(t, &launchElevatedProcess, func(elevatedLaunch) (*elevatedProcess, error) {
		t.Fatal("a refused concurrent request started a handover")
		return nil, nil
	})
	messages := recordMessages(t)

	if handover := installServiceFromTray(testGateSingleton); handover != nil {
		handover.close()
		t.Error("the install item handed over although another action runs")
	}
	for _, action := range []serviceAction{
		serviceActionStart, serviceActionStop, serviceActionUpdate, serviceActionUninstall,
	} {
		performServiceActionFromTray(action, testGateSingleton)
	}

	// Every refusal showed exactly the report of a busy guard, and the
	// guard of the running action is untouched by all five refusals.
	if len(*messages) != 5 {
		t.Fatalf("the refused requests showed %d message boxes, want one per request: %v", len(*messages), *messages)
	}
	for _, message := range *messages {
		if message.caption != serviceActionBusyCaption {
			t.Errorf("the refusal is captioned %q, want %q", message.caption, serviceActionBusyCaption)
		}
		assertNames(t, message.text, "the refusal", []string{
			"Another HFDesk service action is already running",
			"This action was not started",
		})
	}
	if !serviceActionRunning.Load() {
		t.Error("a refused concurrent request released the guard of the running action")
	}
}

// TestNewTrayLifecycleActionsConfirmBeforeTheElevation proves the flow of the
// two new menu actions end to end on the tray side: the confirmation with
// its caption and its text comes BEFORE the administrator prompt, a declined
// answer launches nothing and releases the guard with a menu catch-up, and
// an accepted answer launches exactly that helper — without a desktop
// singleton, because stop and removal hold none — and keeps the guard until
// that helper ends, with the catch-up following its exit.
func TestNewTrayLifecycleActionsConfirmBeforeTheElevation(t *testing.T) {
	for _, tc := range []struct {
		name            string
		action          serviceAction
		confirmCaption  string
		confirmContains string
	}{
		{
			name:            "stop",
			action:          serviceActionStop,
			confirmCaption:  stopServiceConfirmCaption,
			confirmContains: "Do you want to stop the service?",
		},
		{
			name:            "delete",
			action:          serviceActionUninstall,
			confirmCaption:  removeServiceConfirmCaption,
			confirmContains: "Do you want to remove the registration?",
		},
	} {
		t.Run(tc.name+" declined", func(t *testing.T) {
			freshServiceInstallGuard(t)
			var refreshes atomic.Int32
			replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })
			var askedCaption, askedText string
			confirmations := 0
			replaceSeam(t, &confirmServiceAction, func(caption, text string) bool {
				confirmations++
				askedCaption, askedText = caption, text
				return false
			})
			replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
				t.Error("a declined confirmation launched an elevated helper")
				return 0, nil
			})
			messages := recordMessages(t)

			performServiceActionFromTray(tc.action, testGateSingleton)

			if confirmations != 1 {
				t.Fatalf("the action asked its confirmation %d times, want one question", confirmations)
			}
			if askedCaption != tc.confirmCaption {
				t.Errorf("the confirmation is captioned %q, want %q", askedCaption, tc.confirmCaption)
			}
			if !strings.Contains(askedText, tc.confirmContains) {
				t.Errorf("the confirmation %q does not carry its question", askedText)
			}
			// No rights sentence in any action confirmation
			// (decision of 2026-11-24): the Windows prompt that
			// follows states that itself.
			if strings.Contains(strings.ToLower(askedText), "administrator") ||
				strings.Contains(askedText, "Windows prompt") {
				t.Errorf("the confirmation %q carries the rights sentence, want none", askedText)
			}
			if len(*messages) != 0 {
				t.Errorf("a declined confirmation showed %v, want no report: nothing was attempted", *messages)
			}
			if serviceActionRunning.Load() {
				t.Error("a declined confirmation kept the service-action guard")
			}
			if got := refreshes.Load(); got != 1 {
				t.Errorf("a declined confirmation asked for the menu catch-up %d times, want exactly once", got)
			}
		})

		t.Run(tc.name+" accepted", func(t *testing.T) {
			freshServiceInstallGuard(t)
			var refreshes atomic.Int32
			replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })
			replaceSeam(t, &confirmServiceAction, func(caption, text string) bool {
				if caption != tc.confirmCaption {
					t.Errorf("the confirmation is captioned %q, want %q", caption, tc.confirmCaption)
				}
				return true
			})
			var launches []elevatedLaunch
			replaceSeam(t, &launchElevated, func(l elevatedLaunch) (uint32, error) {
				launches = append(launches, l)
				return 7771, nil
			})
			waiterCalled := make(chan uint32, 1)
			releaseWaiter := make(chan struct{})
			replaceSeam(t, &waitForServiceActionHelper, func(pid uint32) {
				waiterCalled <- pid
				<-releaseWaiter
			})
			messages := recordMessages(t)

			performServiceActionFromTray(tc.action, testGateSingleton)

			if len(launches) != 1 {
				t.Fatalf("the action launched %d helpers, want exactly one", len(launches))
			}
			launch := launches[0]
			if launch.Action != tc.action {
				t.Errorf("the helper performs %q, want %q", launch.Action, tc.action)
			}
			if launch.WaitPID != 0 {
				t.Errorf("the helper waits for the process %d, want no handover wait", launch.WaitPID)
			}
			if launch.InstanceMutex != "" {
				t.Errorf("the helper carries the singleton %q, want none: %s holds no singleton",
					launch.InstanceMutex, tc.action)
			}
			if launch.DesktopState != service.PublishedStatePath() {
				t.Errorf("the helper state path = %q, want the claim path of this desktop %q",
					launch.DesktopState, service.PublishedStatePath())
			}
			if len(*messages) != 0 {
				t.Errorf("the launched action showed %v, want the helper to report the outcome", *messages)
			}
			// The guard spans the helper, not only the click: it is
			// still held while the helper runs, and no catch-up has
			// been asked yet — the helper's exit asks for it.
			if !serviceActionRunning.Load() {
				t.Error("the guard was released although the helper is still running")
			}
			if got := refreshes.Load(); got != 0 {
				t.Errorf("the launch itself asked for the menu catch-up %d times, want the helper's exit to ask instead", got)
			}
			pid := <-waiterCalled
			if pid != 7771 {
				t.Errorf("the waiter waits for the process %d, want the launched helper 7771", pid)
			}

			close(releaseWaiter)
			waitForTrayCondition(t, "the guard release and the menu catch-up after the helper ended",
				func() bool {
					return !serviceActionRunning.Load() && refreshes.Load() == 1
				})
		})
	}
}

// TestTrayServiceActionsRefreshTheMenuAfterEveryUnsuccessfulOutcome proves
// the second half of the catch-up rule: an outcome that ended the action
// without a running helper — a declined confirmation, a refused elevation, a
// mutation the serving desktop refuses — releases the guard and re-reads the
// status right away, so no outcome waits for the next tick of the menu.
func TestTrayServiceActionsRefreshTheMenuAfterEveryUnsuccessfulOutcome(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action serviceAction
		// setup configures the outcome of the attempt.
		setup func(t *testing.T)
		// wantReport is the caption the outcome reports, empty when the
		// outcome shows no box at all.
		wantReport string
	}{
		{
			name:   "a declined confirmation",
			action: serviceActionStop,
			setup: func(t *testing.T) {
				replaceSeam(t, &confirmServiceAction, func(string, string) bool { return false })
			},
		},
		{
			name:   "a refused elevation",
			action: serviceActionUninstall,
			setup: func(t *testing.T) {
				replaceSeam(t, &confirmServiceAction, func(string, string) bool { return true })
				replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
					return 0, errors.New(elevationDeclined)
				})
			},
			wantReport: serviceFailureCaption(serviceActionUninstall),
		},
		{
			// The honest refusal of the serving desktop is the LAST
			// resort of the handover, not its main path: this surface
			// has no host endpoints to hand over to, so the start
			// confirms nothing it could perform and ends with the
			// refusal instead of an elevation or a handover beside the
			// desktop that serves the address.
			name:   "a start beside a serving desktop whose handover cannot be established",
			action: serviceActionStart,
			setup: func(t *testing.T) {
				// The tray holds its own singleton and its published
				// claim is live: the two facts the guard of the
				// helper decides a start from.
				release, err := service.HoldDesktopSingleton(testGateSingleton)
				if err != nil {
					t.Fatalf("hold the desktop singleton %q: %v", testGateSingleton, err)
				}
				t.Cleanup(release)
				replaceSeam(t, &service.PublishedStateLive, func() (bool, bool) { return true, false })
				// No host endpoints: this surface cannot hand over,
				// which is the last resort the refusal below reports.
				replaceSeam(t, &serviceHandoverCommit,
					(func(*desktopHandover, serviceAction, handoverRelaunch) bool)(nil))
				replaceSeam(t, &serviceHandoverQuit, (func())(nil))
				replaceSeam(t, &confirmServiceAction, func(string, string) bool {
					t.Error("the doomed start asked a confirmation it could not act on")
					return true
				})
				replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
					t.Error("the refused start launched an elevated helper")
					return 0, nil
				})
				replaceSeam(t, &launchElevatedProcess, func(elevatedLaunch) (*elevatedProcess, error) {
					t.Error("the refused start started a handover it has no host for")
					return nil, nil
				})
			},
			wantReport: serviceFailureCaption(serviceActionStart),
		},
		{
			// The update decides beside the state, so a transition
			// that outlives its budget reaches the established notice
			// of the gate instead of an elevation — and still ends
			// with the catch-up like every other outcome.
			name:   "an update beside a state that cannot be decided on",
			action: serviceActionUpdate,
			setup: func(t *testing.T) {
				replaceSeam(t, &serviceStatusTimeout, 50*time.Millisecond)
				replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
					return service.Status{State: service.StateStopping}, nil
				})
				replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
					t.Error("the update launched although the state is still a transition")
					return 0, nil
				})
			},
			wantReport: serviceGatePendingCaption,
		},
		{
			// A registration that vanished between the menu read and
			// the click changes nothing: no launch, no report — the
			// menu line catches up through the same catch-up.
			name:   "an update on a machine whose registration is gone",
			action: serviceActionUpdate,
			setup: func(t *testing.T) {
				replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
					return service.Status{State: service.StateNotInstalled}, nil
				})
				replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
					t.Error("the update launched although no registration exists")
					return 0, nil
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			freshServiceInstallGuard(t)
			var refreshes atomic.Int32
			replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })
			tc.setup(t)
			messages := recordMessages(t)

			performServiceActionFromTray(tc.action, testGateSingleton)

			if serviceActionRunning.Load() {
				t.Error("an unsuccessful outcome kept the service-action guard")
			}
			if got := refreshes.Load(); got != 1 {
				t.Errorf("the menu catch-up was asked %d times, want exactly once after the outcome", got)
			}
			if tc.wantReport == "" {
				if len(*messages) != 0 {
					t.Errorf("the outcome showed %v, want no report", *messages)
				}
				return
			}
			if len(*messages) != 1 {
				t.Fatalf("the outcome showed %d message boxes, want its one report: %v", len(*messages), *messages)
			}
			if (*messages)[0].caption != tc.wantReport {
				t.Errorf("the report is captioned %q, want %q", (*messages)[0].caption, tc.wantReport)
			}
		})
	}
}

// TestUntrackedHelperHoldCoversTheActionBudgetAndItsWarmUpMargin pins the
// relationship the untracked hold is built on: the hold measured from the
// launch must outlast the action budget measured from the start of the
// action, or the mutation window of a helper nobody can wait for could
// outlive the guard. Shrinking the hold to the bare action budget, deleting
// the warm-up margin or making that margin non-positive all fail here; the
// busy-refusal behavior of the hold itself is pinned by
// TestTrayServiceActionHoldsTheGuardForAHelperWithoutAProcessRecord.
func TestUntrackedHelperHoldCoversTheActionBudgetAndItsWarmUpMargin(t *testing.T) {
	if untrackedHelperHoldBudget <= serviceActionBudget {
		t.Errorf("the untracked hold %s does not outlast the action budget %s: the mutation window of an untracked helper could outlive the guard",
			untrackedHelperHoldBudget, serviceActionBudget)
	}
	if untrackedHelperHoldBudget != serviceActionBudget+untrackedHelperWarmUpMargin {
		t.Errorf("the untracked hold %s is not the action budget %s plus the warm-up margin %s",
			untrackedHelperHoldBudget, serviceActionBudget, untrackedHelperWarmUpMargin)
	}
	if untrackedHelperWarmUpMargin <= 0 {
		t.Errorf("the warm-up margin %s does not cover the window between the launch of the helper and the start of its action",
			untrackedHelperWarmUpMargin)
	}
}

// TestTrayServiceActionHoldsTheGuardForAHelperWithoutAProcessRecord proves
// the guard policy for the launch outcome the process id used to hide: the
// shell started the helper but named no process for it, so nothing can wait
// for its exit while it may still move the registration. The single
// service-action guard is held for the bounded hold instead of being
// released silently at the launch, every second request of ANY action in
// that window is refused with the honest busy report and launches nothing,
// and the hold ends like the run of a waited-for helper: the guard is
// released and the menu catches up.
//
// Deleting the bounded hold fails this test at the guard check right after
// the launch; releasing the guard at the launch fails it at the same check;
// a second action that slips through fails the launch count or the caption
// check of the refusals.
func TestTrayServiceActionHoldsTheGuardForAHelperWithoutAProcessRecord(t *testing.T) {
	freshServiceInstallGuard(t)
	replaceSeam(t, &untrackedHelperHoldBudget, 2*time.Second)
	var refreshes atomic.Int32
	replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })
	replaceSeam(t, &waitForServiceActionHelper, func(pid uint32) {
		t.Errorf("a helper without a process record was waited for by the process id %d", pid)
	})
	replaceSeam(t, &confirmServiceAction, func(string, string) bool { return true })
	launches := 0
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		launches++
		// The branch under test: the shell started the helper and named
		// no process for it.
		return 0, nil
	})
	messages := recordMessages(t)

	performServiceActionFromTray(serviceActionUninstall, testGateSingleton)

	if launches != 1 {
		t.Fatalf("the action launched %d helpers, want exactly one", launches)
	}
	if !serviceActionRunning.Load() {
		t.Fatal("the guard was released at a launch whose helper may still perform its mutation")
	}
	if got := refreshes.Load(); got != 0 {
		t.Fatalf("the launch itself asked for the menu catch-up %d times, want the hold to ask when it ends", got)
	}

	// The second request of every surface is refused with the busy report
	// in the hold window — no state read, no confirmation, no dialog and
	// above all no second helper beside the possibly live one.
	if handover := installServiceFromTray(testGateSingleton); handover != nil {
		handover.close()
		t.Error("the install item handed over during the bounded hold of another action")
	}
	for _, action := range []serviceAction{
		serviceActionStart, serviceActionStop, serviceActionUpdate, serviceActionUninstall,
	} {
		performServiceActionFromTray(action, testGateSingleton)
	}
	if launches != 1 {
		t.Fatalf("the refused requests launched %d new helpers, want none: %d launches in total",
			launches-1, launches)
	}
	if len(*messages) != 5 {
		t.Fatalf("the refused requests showed %d message boxes, want one per request: %v",
			len(*messages), *messages)
	}
	for _, message := range *messages {
		if message.caption != serviceActionBusyCaption {
			t.Errorf("the refusal is captioned %q, want %q", message.caption, serviceActionBusyCaption)
		}
	}

	// The bounded hold ends like a finished action: the guard is released
	// and the menu is re-read exactly once.
	waitForTrayCondition(t, "the bounded hold to end with the guard release and the menu catch-up",
		func() bool {
			return !serviceActionRunning.Load() && refreshes.Load() == 1
		})
}

// TestTrayServiceActionReleasesTheGuardWhenTheLaunchStartedNothing proves
// the other launch outcome keeps the end path it always had: a launch that
// started NO helper — here the declined console-program notice of an update,
// whose decline stays quiet — releases the guard immediately and asks for the
// menu catch-up at once, and no busy state survives: the next request runs
// its ordinary flow instead of being refused.
//
// Together with the bounded-hold case above this pins that the two launch
// outcomes stay apart: conflating them either releases the guard beside a
// possibly live helper or holds it for a launch that never happened.
func TestTrayServiceActionReleasesTheGuardWhenTheLaunchStartedNothing(t *testing.T) {
	freshServiceInstallGuard(t)
	var refreshes atomic.Int32
	replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return registeredStoppedService(), nil
	})
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return "", errors.New("the pair of this release is missing")
	})
	replaceSeam(t, &consoleDownloadOfferFor, func(string) (*consoleDownloadOffer, error) {
		return nil, nil
	})
	replaceSeam(t, &showSiblingMissingNotice, func(siblingMissingNotice) siblingMissingAnswer {
		return siblingMissingDismiss
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		t.Error("the declined console-program notice launched an elevated helper")
		return 0, nil
	})
	messages := recordMessages(t)

	performServiceActionFromTray(serviceActionUpdate, testGateSingleton)

	if serviceActionRunning.Load() {
		t.Error("a launch that started nothing kept the service-action guard")
	}
	if got := refreshes.Load(); got != 1 {
		t.Errorf("the menu catch-up was asked %d times, want exactly once after the declined notice", got)
	}
	if len(*messages) != 0 {
		t.Errorf("the declined notice showed %v, want the decline to stay quiet", *messages)
	}

	// No busy state survives the decline: the next action enters its
	// ordinary flow — it asks its confirmation instead of being refused.
	confirmations := 0
	replaceSeam(t, &confirmServiceAction, func(string, string) bool {
		confirmations++
		return false
	})
	performServiceActionFromTray(serviceActionStop, testGateSingleton)

	if confirmations != 1 {
		t.Errorf("the next action reached its confirmation %d times, want its ordinary flow beside a free guard",
			confirmations)
	}
	for _, message := range *messages {
		if message.caption == serviceActionBusyCaption {
			t.Errorf("the next action was refused as busy after a launch that started nothing: %v", *messages)
		}
	}
}

// TestServiceHelperStopStopsTheService proves the helper action itself: the
// state is read before the stop, the stop runs, the state is read until it
// settled, and the report is the state plus the one way back — with no
// installation, start, restart or removal anywhere in the steps.
func TestServiceHelperStopStopsTheService(t *testing.T) {
	replaceSeam(t, &serviceVerifyBudget, 100*time.Millisecond)
	running := runningServiceStatus(service.StateRunning, "127.0.0.1:18080")
	stopped := runningServiceStatus(service.StateStopped, "127.0.0.1:18080")
	manager := &fakeServiceManager{statuses: []service.Status{running, stopped}}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	err := RunServiceAction(ServiceActionOptions{Action: "stop"})
	assertNoError(t, err, "the stop of the service failed: %v", err)

	if stepIndex(manager.steps, "stop") != 1 {
		t.Errorf("the steps are %v, want the stop after the first state read", manager.steps)
	}
	if manager.starts != 0 {
		t.Errorf("the stop started something %d times, want no start", manager.starts)
	}
	for _, forbidden := range []string{"install", "start", "restart", "uninstall"} {
		if stepIndex(manager.steps, forbidden) != -1 {
			t.Errorf("the steps are %v, want no %q in a stop", manager.steps, forbidden)
		}
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the report of the stop: %v", len(*messages), *messages)
	}
	if (*messages)[0].caption != serviceStoppedCaption {
		t.Errorf("the report is captioned %q, want %q", (*messages)[0].caption, serviceStoppedCaption)
	}
	assertNames(t, (*messages)[0].text, "the report", []string{
		"The HFDesk service is stopped.",
		"Nothing serves the address of the service right now.",
		"Quit HFDesk from the notification-area icon",
	})
}

// TestServiceHelperStopOnAMachineWithoutARegistration reports the settled
// absence instead of the stable success the manager answers a stop of an
// absent instance with, and it never sends the user to an installation.
func TestServiceHelperStopOnAMachineWithoutARegistration(t *testing.T) {
	manager := &fakeServiceManager{
		statuses: []service.Status{{State: service.StateNotInstalled}},
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	err := RunServiceAction(ServiceActionOptions{Action: "stop"})
	if err == nil {
		t.Fatal("the stop of a machine without a registration reported no outcome")
	}
	if stepIndex(manager.steps, "stop") != -1 {
		t.Errorf("the steps are %v, want the action to stop at the state read", manager.steps)
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the one report: %v", len(*messages), *messages)
	}
	if (*messages)[0].caption != serviceFailureCaption(serviceActionStop) {
		t.Errorf("the report is captioned %q, want %q", (*messages)[0].caption, serviceFailureCaption(serviceActionStop))
	}
	assertNames(t, (*messages)[0].text, "the report", []string{
		"There is no service to stop.",
	})
	assertNamesWithout(t, (*messages)[0].text, "the report", nil,
		[]string{"Install it from the tray", "The service was stopped"})
}

// TestServiceHelperUninstallRemovesOnlyTheRegistration pins the invariant of
// the new delete action: the helper calls Manager.Uninstall — the very call
// the command line performs — and no other mutator of the manager beside the
// state read that reports what it found. Nothing installs, starts, stops or
// restarts, and the report states that the data stays untouched instead of
// claiming any of it was deleted.
func TestServiceHelperUninstallRemovesOnlyTheRegistration(t *testing.T) {
	for _, tc := range []struct {
		name        string
		registered  bool
		wantCaption string
		wantText    []string
	}{
		{
			name:        "a registration exists",
			registered:  true,
			wantCaption: serviceRegistrationRemovedCaption,
			wantText: []string{
				"The Windows service registration of HFDesk is removed.",
				"settings, jobs, models, cache, history and logs stay untouched",
				"Install the service again from the tray of the desktop app",
			},
		},
		{
			name:        "no registration",
			registered:  false,
			wantCaption: serviceNoRegistrationCaption,
			wantText: []string{
				"HFDesk is not registered as a Windows service.",
				"nothing was changed",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := runningServiceStatus(service.StateStopped, "127.0.0.1:18080")
			if !tc.registered {
				state = service.Status{State: service.StateNotInstalled}
			}
			manager := &fakeServiceManager{statuses: []service.Status{state}}
			replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
				return manager, nil
			})
			messages := recordMessages(t)

			err := RunServiceAction(ServiceActionOptions{Action: "uninstall"})
			assertNoError(t, err, "the removal of the registration failed: %v", err)

			// The invariant: one read, then EXACTLY Uninstall — and
			// no other mutator of the manager anywhere in the flow.
			if len(manager.steps) != 2 || manager.steps[0] != "status" || manager.steps[1] != "uninstall" {
				t.Fatalf("the steps are %v, want exactly [status uninstall]: the removal owns no second mutator",
					manager.steps)
			}
			if manager.starts != 0 || len(manager.installs) != 0 {
				t.Errorf("the removal started %d times and installed %d times, want neither",
					manager.starts, len(manager.installs))
			}

			if len(*messages) != 1 {
				t.Fatalf("the helper showed %d message boxes, want the one report: %v", len(*messages), *messages)
			}
			if (*messages)[0].caption != tc.wantCaption {
				t.Errorf("the report is captioned %q, want %q", (*messages)[0].caption, tc.wantCaption)
			}
			assertNames(t, (*messages)[0].text, "the report", tc.wantText)
			// The report never claims that data was deleted — in any
			// of the singular or plural, past or perfect shapes of
			// such a claim (the shared stop list of the text layer).
			assertNamesWithout(t, (*messages)[0].text, "the report", nil, deletionClaimForms)
		})
	}
}

// TestTrayMenuAppliesLifecycleItems proves the write side of the menu map:
// one read is applied to all the submenu items — the four lifecycle items
// appear beside the registration and enable per state, and a machine
// without one hides the whole group while the install item becomes the
// only active action.
func TestTrayMenuAppliesLifecycleItems(t *testing.T) {
	line := newFakeTrayItem()
	install := newFakeTrayItem()
	start := newFakeTrayItem()
	stop := newFakeTrayItem()
	update := newFakeTrayItem()
	del := newFakeTrayItem()
	menu := &trayServiceMenu{
		line: line, install: install, start: start, stop: stop,
		update: update, delete: del,
	}
	snapshot := func() (startVisible, startEnabled, stopEnabled, updateEnabled, delEnabled, installVisible bool) {
		_, startEnabled, startVisible, _ = start.snapshot()
		_, stopEnabled, _, _ = stop.snapshot()
		_, updateEnabled, _, _ = update.snapshot()
		_, delEnabled, _, _ = del.snapshot()
		_, _, installVisible, _ = install.snapshot()
		return
	}

	// Registered and running: the group appears, stop is the action,
	// start stays grey, update and delete act, install hides.
	menu.apply(serviceMenuFor(service.Status{State: service.StateRunning, Running: true}, nil))
	startVisible, startEnabled, stopEnabled, updateEnabled, delEnabled, installVisible := snapshot()
	if !startVisible {
		t.Error("the start item is hidden although the registration is shown")
	}
	// Every lifecycle item is SHOWN in the shown state — not just enabled:
	// the enablement flags are written in the same branch and would still
	// be set if one of the Show calls were dropped, so each of the four
	// items pins its own visibility line.
	if _, _, v, _ := stop.snapshot(); !v {
		t.Error("the stop item is hidden although the registration is shown")
	}
	if _, _, v, _ := update.snapshot(); !v {
		t.Error("the update item is hidden although the registration is shown")
	}
	if _, _, v, _ := del.snapshot(); !v {
		t.Error("the delete item is hidden although the registration is shown")
	}
	if startEnabled || !stopEnabled || !updateEnabled || !delEnabled {
		t.Errorf("the running registration enables start=%t stop=%t update=%t delete=%t, want stop/update/delete only",
			startEnabled, stopEnabled, updateEnabled, delEnabled)
	}
	if installVisible {
		t.Error("the install item is visible although a registration is proven")
	}

	// Registered and stopped: start becomes the action, stop goes grey.
	menu.apply(serviceMenuFor(service.Status{State: service.StateStopped}, nil))
	_, startEnabled, stopEnabled, updateEnabled, delEnabled, _ = snapshot()
	if !startEnabled || stopEnabled || !updateEnabled || !delEnabled {
		t.Errorf("the stopped registration enables start=%t stop=%t update=%t delete=%t, want start/update/delete only",
			startEnabled, stopEnabled, updateEnabled, delEnabled)
	}

	// A transition keeps the group on screen but greys every action: a
	// pending state is not a state to decide on.
	menu.apply(serviceMenuFor(service.Status{State: service.StateStarting}, nil))
	startVisible, startEnabled, stopEnabled, updateEnabled, delEnabled, installVisible = snapshot()
	if !startVisible || startEnabled || stopEnabled || updateEnabled || delEnabled {
		t.Errorf("the transition produced visible=%t start=%t stop=%t update=%t delete=%t, want the visible group with every action grey",
			startVisible, startEnabled, stopEnabled, updateEnabled, delEnabled)
	}
	if installVisible {
		t.Error("the install item is visible during a transition: the registration is proven")
	}

	// No registration: the whole group disappears, install acts.
	menu.apply(serviceMenuFor(service.Status{State: service.StateNotInstalled}, nil))
	startVisible, _, _, _, _, installVisible = snapshot()
	if startVisible {
		t.Error("the start item is visible beside a proven absence, want the group hidden")
	}
	for name, item := range map[string]*fakeTrayItem{"start": start, "stop": stop, "update": update, "delete": del} {
		if _, _, itemVisible, _ := item.snapshot(); itemVisible {
			t.Errorf("the %s item stayed visible beside a proven absence", name)
		}
	}
	if !installVisible {
		t.Error("the install item is hidden although the read proved no registration")
	}
	if _, enabled, visible, _ := install.snapshot(); !visible || !enabled {
		t.Errorf("the install item visible=%t enabled=%t beside a proven absence, want it visible and enabled", visible, enabled)
	}
}

// registeredStoppedService is the registration every route of the tests
// below reads: present, settled and stopped.
func registeredStoppedService() service.Status {
	return runningServiceStatus(service.StateStopped, "127.0.0.1:18080")
}

// TestInstallRouteCatchesTheMenuUpAfterEveryOutcome pins the catch-up of the
// install-service route itself: every path that ends without a committed
// handover asks the host for the menu refresh right there, and the committed
// handover quits the host instead — deleting either refresh() call in
// runInstallServiceAction fails the matching subtest.
func TestInstallRouteCatchesTheMenuUpAfterEveryOutcome(t *testing.T) {
	t.Run("nothing was handed over: the menu catches up", func(t *testing.T) {
		freshServiceInstallGuard(t)
		replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
			return registeredStoppedService(), nil
		})
		replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
			return `C:\portable\hfdesk.exe`, nil
		})
		replaceSeam(t, &confirmServiceInstall, func(string) bool { return true })
		replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
			return serviceChoiceCancel
		})
		refreshes, commits, quits := 0, 0, 0

		runInstallServiceAction(testGateSingleton, func() { refreshes++ },
			func(*desktopHandover, serviceAction, handoverRelaunch) bool { commits++; return true },
			func() { quits++ })

		if refreshes != 1 {
			t.Errorf("the route asked for the menu catch-up %d times, want exactly once", refreshes)
		}
		if commits != 0 || quits != 0 {
			t.Errorf("the route committed %d times and quit %d times although nothing was handed over", commits, quits)
		}
	})

	t.Run("the confirmation is declined: the menu catches up", func(t *testing.T) {
		freshServiceInstallGuard(t)
		replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
			return `C:\portable\hfdesk.exe`, nil
		})
		replaceSeam(t, &confirmServiceInstall, func(string) bool { return false })
		replaceSeam(t, &launchElevatedProcess, func(elevatedLaunch) (*elevatedProcess, error) {
			t.Error("the route started a handover although the confirmation was declined")
			return nil, nil
		})
		refreshes, commits, quits := 0, 0, 0

		runInstallServiceAction(testGateSingleton, func() { refreshes++ },
			func(*desktopHandover, serviceAction, handoverRelaunch) bool { commits++; return true },
			func() { quits++ })

		if refreshes != 1 {
			t.Errorf("the route asked for the menu catch-up %d times after the declined confirmation, want exactly once",
				refreshes)
		}
		if commits != 0 || quits != 0 {
			t.Errorf("the route committed %d times and quit %d times although nothing was handed over", commits, quits)
		}
	})

	t.Run("the handover is committed: the quit owns the end", func(t *testing.T) {
		freshServiceInstallGuard(t)
		stubNoServiceRegistration(t)
		replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
			return `C:\portable\hfdesk.exe`, nil
		})
		replaceSeam(t, &confirmServiceInstall, func(string) bool { return true })
		replaceSeam(t, &launchElevatedProcess, func(l elevatedLaunch) (*elevatedProcess, error) {
			return readyTestProcess(t, l), nil
		})
		refreshes, commits, quits := 0, 0, 0

		runInstallServiceAction(testGateSingleton, func() { refreshes++ },
			func(handover *desktopHandover, _ serviceAction, _ handoverRelaunch) bool {
				commits++
				handover.close()
				return true
			},
			func() { quits++ })

		if commits != 1 || quits != 1 {
			t.Errorf("the route committed %d times and quit %d times, want one commit that ends in the quit: %d refreshes",
				commits, quits, refreshes)
		}
		if refreshes != 0 {
			t.Errorf("the route asked for the menu catch-up %d times beside a committed handover: the quit owns the end", refreshes)
		}
		if !serviceActionRunning.Load() {
			t.Error("the route released the guard although the handover was committed and the host quits on it")
		}
	})

	t.Run("the host cannot commit: the guard is released and the menu catches up", func(t *testing.T) {
		freshServiceInstallGuard(t)
		stubNoServiceRegistration(t)
		replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
			return `C:\portable\hfdesk.exe`, nil
		})
		replaceSeam(t, &confirmServiceInstall, func(string) bool { return true })
		replaceSeam(t, &launchElevatedProcess, func(l elevatedLaunch) (*elevatedProcess, error) {
			return readyTestProcess(t, l), nil
		})
		refreshes, commits, quits := 0, 0, 0

		runInstallServiceAction(testGateSingleton, func() { refreshes++ },
			func(*desktopHandover, serviceAction, handoverRelaunch) bool { commits++; return false },
			func() { quits++ })

		if refreshes != 1 {
			t.Errorf("the route asked for the menu catch-up %d times after the refused commit, want exactly once", refreshes)
		}
		if commits != 1 || quits != 0 {
			t.Errorf("the route committed %d times and quit %d times, want one refused commit and no quit", commits, quits)
		}
		if serviceActionRunning.Load() {
			t.Error("the route kept the guard although the handover was not committed")
		}
	})
}

// TestStartAndUpdateRoutesAskForNoConfirmation pins the confirmation-free
// behavior of start and update DIRECTLY, on every surface that can launch
// them without a handover: the lifecycle menu items, the registered-service
// dialog and the launch gate, each beside a desktop that does not serve the
// address (the claim of this test is not live), which is the only state in
// which these two actions reach the elevated helper directly. Each step also
// proves it really reached the helper — the zero-confirmation assert only
// discriminates when the action ran — and any confirmation added to those
// direct routes (the install box or a new one) fails the step that grew it.
// The handover route of a desktop host that DOES serve the address asks its
// own confirmation of the closing window first, and is pinned by
// TestTrayServiceAnswersHandOverOnlyWhenThisDesktopServes.
func TestStartAndUpdateRoutesAskForNoConfirmation(t *testing.T) {
	freshServiceInstallGuard(t)
	confirmations := &atomic.Int32{}
	replaceSeam(t, &confirmServiceInstall, func(string) bool {
		confirmations.Add(1)
		return true
	})
	replaceSeam(t, &confirmServiceAction, func(string, string) bool {
		confirmations.Add(1)
		return true
	})
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return registeredStoppedService(), nil
	})
	// No live claim: the start beside this desktop passes its
	// pre-elevation guard and really reaches the launch.
	replaceSeam(t, &service.PublishedStateLive, func() (bool, bool) { return false, false })
	replaceSeam(t, &waitForServiceActionHelper, func(uint32) {})
	replaceSeam(t, &openBrowser, func(string) error {
		t.Error("a start or an update answer opened a browser")
		return nil
	})
	choice := serviceChoiceStart
	replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice { return choice })
	var launches []elevatedLaunch
	replaceSeam(t, &launchElevated, func(l elevatedLaunch) (uint32, error) {
		launches = append(launches, l)
		return 4242, nil
	})

	stopped := registeredStoppedService()
	steps := []struct {
		name string
		want serviceAction
		run  func()
	}{
		{"the menu start item", serviceActionStart, func() {
			performServiceActionFromTray(serviceActionStart, testGateSingleton)
		}},
		{"the menu update item", serviceActionUpdate, func() {
			performServiceActionFromTray(serviceActionUpdate, testGateSingleton)
		}},
		{"the dialog start answer", serviceActionStart, func() {
			choice = serviceChoiceStart
			offerRegisteredServiceFromTray(stopped, testGateSingleton)
		}},
		{"the dialog update answer", serviceActionUpdate, func() {
			choice = serviceChoiceUpdate
			offerRegisteredServiceFromTray(stopped, testGateSingleton)
		}},
		{"the gate start answer", serviceActionStart, func() {
			choice = serviceChoiceStart
			serviceGate(testGateSingleton)
		}},
		{"the gate update answer", serviceActionUpdate, func() {
			choice = serviceChoiceUpdate
			serviceGate(testGateSingleton)
		}},
	}
	for _, step := range steps {
		before := len(launches)
		step.run()
		waitForTrayCondition(t, "the guard of "+step.name+" to be released",
			func() bool { return !serviceActionRunning.Load() })
		if got := confirmations.Load(); got != 0 {
			t.Fatalf("%s asked for a confirmation %d times: start and update keep their confirmation-free behavior",
				step.name, got)
		}
		if len(launches) != before+1 {
			t.Fatalf("%s launched %d helpers, want exactly one — the zero-confirmation assert only discriminates when the action really ran",
				step.name, len(launches)-before)
		}
		if launches[before].Action != step.want {
			t.Errorf("%s launched the helper of %q, want %q", step.name, launches[before].Action, step.want)
		}
	}
}

// TestServiceConfirmationsDefaultToNo pins the message-box composition of
// every Yes/No confirmation of a service action: the exact flag set, and
// especially MB_DEFBUTTON2 as THE default button — a stray Enter selects
// the answer that changes nothing, so losing that flag (or trading it for
// another default) fails here instead of only in a manual walkthrough.
func TestServiceConfirmationsDefaultToNo(t *testing.T) {
	want := windows.MB_YESNO | windows.MB_ICONQUESTION | windows.MB_DEFBUTTON2 |
		windows.MB_SETFOREGROUND | windows.MB_TOPMOST
	if serviceConfirmMessageBoxFlags != want {
		t.Fatalf("the confirmation flags are %#x, want exactly the composition %#x",
			serviceConfirmMessageBoxFlags, want)
	}

	// The default-button slot (bits 8..11) holds MB_DEFBUTTON2 and nothing
	// else: No is the button a stray Enter selects.
	const defaultButtonMask = 0x0f00
	if serviceConfirmMessageBoxFlags&defaultButtonMask != windows.MB_DEFBUTTON2 {
		t.Errorf("the default button of the confirmations is %#x, want MB_DEFBUTTON2 (%#x): a stray Enter must select the answer that changes nothing",
			serviceConfirmMessageBoxFlags&defaultButtonMask, windows.MB_DEFBUTTON2)
	}

	// No other button set or icon may join the composition unnoticed.
	extra := windows.MB_OKCANCEL | windows.MB_YESNOCANCEL |
		windows.MB_ICONERROR | windows.MB_ICONWARNING | windows.MB_ICONINFORMATION |
		windows.MB_DEFBUTTON3 | windows.MB_DEFBUTTON4
	if serviceConfirmMessageBoxFlags&extra != 0 {
		t.Errorf("the confirmation flags carry %#x beside the intended composition",
			serviceConfirmMessageBoxFlags&extra)
	}
}
