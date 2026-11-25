// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// waitForShownCaption waits until the message box has shown the report with
// the given caption and returns every report recorded so far, in order.
//
// The caption of the report ends the wait, never the count of everything
// shown: a window host also re-shows its window around a service action, and
// whether that re-show (of the refused press, or of the action that was
// still running) has landed yet must not decide whether the refusal was
// reported at all.
func waitForShownCaption(t *testing.T, shown chan shownMessage, caption, what string) []shownMessage {
	t.Helper()
	var messages []shownMessage
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for drained := false; !drained; {
			select {
			case message := <-shown:
				messages = append(messages, message)
				if message.caption == caption {
					return messages
				}
			default:
				drained = true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
	return messages
}

// TestDesktopWindowActionsCarryTheFixedActionNames pins the bridge of the
// desktop host's Logs window: the window binds exactly the eight fixed
// action names its page can call — open the log folder, open the log file,
// open the web UI and the five service actions (start, stop, update, delete
// and install) — so the window of a desktop offers what its tray offers,
// and no foreign handler rides along into the window.
func TestDesktopWindowActionsCarryTheFixedActionNames(t *testing.T) {
	opened := make(chan struct{}, 1)
	actions := desktopWindowActions(panicGuard{}, func() { opened <- struct{}{} }, testGateSingleton)

	if len(actions) != 8 {
		keys := make([]string, 0, len(actions))
		for name := range actions {
			keys = append(keys, name)
		}
		t.Fatalf("the desktop window actions = %v, want exactly the eight fixed names", keys)
	}
	for _, name := range []string{
		openLogFolderAction, openLogFileAction, openUiAction,
		logsStartServiceAction, logsStopServiceAction, logsUpdateServiceAction,
		logsDeleteServiceAction, logsInstallServiceAction,
	} {
		if actions[name] == nil {
			t.Fatalf("the desktop window action %q is not bound: its button would find no function", name)
		}
	}

	// The open-the-web-UI handler is the host's own opener — the very
	// function the tray's Open HFDesk item runs — dispatched on a
	// goroutine under the guard like every other action.
	actions[openUiAction]()
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("the window's open-the-web-UI action never reached the host's opener")
	}
}

// windowHostActionsOf builds the window action map of one host the way its
// run wires it: the companion with a window getter and an unavailable
// runtime (so its re-read reaches the established report), the desktop host
// with its plain opener. Both maps are the production constructors, so the
// tests below exercise the very composition each host ships.
func windowHostActionsOf(windowHost string, guard panicGuard) map[string]func() {
	if windowHost == "companion" {
		return companionWindowActions(guard,
			func() *runtimeWindow { return &runtimeWindow{} },
			func() bool { return false }, testGateSingleton)
	}
	return desktopWindowActions(guard, func() {}, testGateSingleton)
}

// TestWindowServiceActionsRunTheMenuStopAndDeleteFlow proves the two new
// buttons of BOTH window hosts end to end: a press runs the VERY flow of the
// matching tray item — performServiceActionFromTray with the stop or the
// removal action, through the one service-action guard both surfaces share —
// exactly ONCE, asks the confirmation of that item before anything else,
// launches nothing on a declined answer, and ends with the established
// re-read: the menu catch-up of the action path (one request) and, for the
// tray companion, the window re-show its Try again button runs, so a
// document whose state moved under its own action catches up instead of
// offering a stale step.
func TestWindowServiceActionsRunTheMenuStopAndDeleteFlow(t *testing.T) {
	for _, tc := range []struct {
		windowHost  string
		action      string
		wantCaption string
		// windowReread reports the host whose window re-shows itself
		// after an action: the companion renders its own document and
		// must catch it up, the desktop host shows the live page of its
		// own server, which re-reads itself.
		windowReread bool
	}{
		{
			windowHost:   "companion",
			action:       logsStopServiceAction,
			wantCaption:  stopServiceConfirmCaption,
			windowReread: true,
		},
		{
			windowHost:   "companion",
			action:       logsDeleteServiceAction,
			wantCaption:  removeServiceConfirmCaption,
			windowReread: true,
		},
		{
			windowHost:  "desktop",
			action:      logsStopServiceAction,
			wantCaption: stopServiceConfirmCaption,
		},
		{
			windowHost:  "desktop",
			action:      logsDeleteServiceAction,
			wantCaption: removeServiceConfirmCaption,
		},
	} {
		t.Run(tc.windowHost+" "+tc.action, func(t *testing.T) {
			useTemporaryStateDir(t)
			freshServiceInstallGuard(t)
			stubNoServiceRegistration(t)

			var confirmations atomic.Int32
			replaceSeam(t, &confirmServiceAction, func(caption, text string) bool {
				confirmations.Add(1)
				if caption != tc.wantCaption {
					t.Errorf("the confirmation is captioned %q, want the caption of the menu item %q",
						caption, tc.wantCaption)
				}
				return false
			})
			replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
				t.Error("a declined confirmation launched an elevated helper")
				return 0, nil
			})
			var refreshes atomic.Int32
			replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })
			shown := recordMessagesChannel(t)

			actions := windowHostActionsOf(tc.windowHost, panicGuard{})
			actions[tc.action]()

			wantShown := 0
			if tc.windowReread {
				wantShown = 1
			}
			// The declined action releases the guard and asks the menu
			// to catch up on its own end path; the companion re-shows
			// its window after it. Both are the established re-reads,
			// so the document never keeps a step its state moved on
			// from.
			waitForTrayCondition(t, "the declined action to end, release the guard and re-read",
				func() bool {
					return confirmations.Load() == 1 && !serviceActionRunning.Load() &&
						refreshes.Load() == 1 && len(shown) == wantShown
				})

			for _, message := range drainMessages(shown) {
				if message.caption != companionWindowUnavailableCaption {
					t.Errorf("the declined action showed %q (%q), want only the companion's window re-read report",
						message.caption, message.text)
				}
			}
		})
	}
}

// TestDesktopWindowActionsBindTheFiveServiceActions proves the rest of the
// desktop host's service section: the start, the update and the installation
// of its window run the very flows of the matching tray items, with this
// launch's singleton, exactly once — the same construction the companion
// window uses (windowServiceActions), so the two windows can never disagree
// about what a service action from a window is.
func TestDesktopWindowActionsBindTheFiveServiceActions(t *testing.T) {
	t.Run("start launches the helper of the menu's start item", func(t *testing.T) {
		freshServiceInstallGuard(t)
		launched := make(chan elevatedLaunch, 1)
		replaceSeam(t, &launchElevated, func(launch elevatedLaunch) (uint32, error) {
			launched <- launch
			return 7781, nil
		})
		waiterCalled := make(chan uint32, 1)
		releaseWaiter := make(chan struct{})
		replaceSeam(t, &waitForServiceActionHelper, func(pid uint32) {
			waiterCalled <- pid
			<-releaseWaiter
		})
		var refreshes atomic.Int32
		replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })

		actions := desktopWindowActions(panicGuard{}, func() {}, testGateSingleton)
		actions[logsStartServiceAction]()

		var launch elevatedLaunch
		select {
		case launch = <-launched:
		case <-time.After(5 * time.Second):
			t.Fatal("the start button of the desktop window never reached the menu's start flow")
		}
		if launch.Action != serviceActionStart {
			t.Errorf("the helper performs %q, want the action of the menu's start item %q",
				launch.Action, serviceActionStart)
		}
		if launch.InstanceMutex != testGateSingleton {
			t.Errorf("the helper carries the singleton %q, want the one of this launch %q",
				launch.InstanceMutex, testGateSingleton)
		}
		if !serviceActionRunning.Load() {
			t.Error("the guard was released although the helper of the start is still running")
		}
		if pid := <-waiterCalled; pid != 7781 {
			t.Errorf("the waiter waits for the process %d, want the launched helper 7781", pid)
		}
		close(releaseWaiter)
		waitForTrayCondition(t, "the guard release and the catch-up after the start helper ended",
			func() bool {
				return !serviceActionRunning.Load() && refreshes.Load() == 1
			})
	})

	t.Run("update launches the helper of the menu's update item", func(t *testing.T) {
		freshServiceInstallGuard(t)
		// The update re-reads the registration under the action policy;
		// a positively stopped registration is the pure rewrite that
		// launches the helper directly.
		replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
			return registeredStoppedService(), nil
		})
		replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
			return `C:\portable\hfdesk.exe`, nil
		})
		launched := make(chan elevatedLaunch, 1)
		replaceSeam(t, &launchElevated, func(launch elevatedLaunch) (uint32, error) {
			launched <- launch
			return 7783, nil
		})
		waiterCalled := make(chan uint32, 1)
		releaseWaiter := make(chan struct{})
		replaceSeam(t, &waitForServiceActionHelper, func(pid uint32) {
			waiterCalled <- pid
			<-releaseWaiter
		})
		var refreshes atomic.Int32
		replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })

		actions := desktopWindowActions(panicGuard{}, func() {}, testGateSingleton)
		actions[logsUpdateServiceAction]()

		var launch elevatedLaunch
		select {
		case launch = <-launched:
		case <-time.After(5 * time.Second):
			t.Fatal("the update button of the desktop window never reached the menu's update flow")
		}
		if launch.Action != serviceActionUpdate {
			t.Errorf("the helper performs %q, want the action of the menu's update item %q",
				launch.Action, serviceActionUpdate)
		}
		if !serviceActionRunning.Load() {
			t.Error("the guard was released although the helper of the update is still running")
		}
		if pid := <-waiterCalled; pid != 7783 {
			t.Errorf("the waiter waits for the process %d, want the launched helper 7783", pid)
		}
		close(releaseWaiter)
		waitForTrayCondition(t, "the guard release and the catch-up after the update helper ended",
			func() bool {
				return !serviceActionRunning.Load() && refreshes.Load() == 1
			})
	})

	t.Run("install runs the menu's installation flow once", func(t *testing.T) {
		freshServiceInstallGuard(t)
		stubNoServiceRegistration(t)
		replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
			return `C:\portable\hfdesk.exe`, nil
		})
		var confirmations atomic.Int32
		replaceSeam(t, &confirmServiceInstall, func(string) bool {
			confirmations.Add(1)
			return false
		})
		replaceSeam(t, &launchElevatedProcess, func(elevatedLaunch) (*elevatedProcess, error) {
			t.Error("a declined installation confirmation started an elevated helper")
			return nil, nil
		})
		var refreshes atomic.Int32
		replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })
		shown := recordMessagesChannel(t)

		actions := desktopWindowActions(panicGuard{}, func() {}, testGateSingleton)
		actions[logsInstallServiceAction]()

		waitForTrayCondition(t, "the declined install to run once, release the guard and catch the menu up",
			func() bool {
				return confirmations.Load() == 1 && !serviceActionRunning.Load() && refreshes.Load() == 1
			})
		if messages := drainMessages(shown); len(messages) != 0 {
			t.Errorf("the declined installation showed %v, want no report: nothing was attempted", messages)
		}
	})
}

// TestWindowServiceActionsRefuseASecondClick proves the double-click rule of
// both window hosts: while one service action runs, a second press from the
// same window is refused by the ONE service-action guard with the very busy
// report the tray shows, and it performs nothing — no confirmation, no state
// read, no second elevated helper — while the guard of the running action
// survives it.
func TestWindowServiceActionsRefuseASecondClick(t *testing.T) {
	// A report still in flight when a subtest ends must never reach the
	// native box of this machine: each subtest records the reports of its
	// own presses, and this sink only catches what a press goroutine still
	// holds after that subtest's recorder was restored. The native box
	// returns when the whole test ends.
	replaceSeam(t, &showMessage, func(string, string, uint32) {})

	for _, windowHost := range []string{"companion", "desktop"} {
		t.Run(windowHost, func(t *testing.T) {
			freshServiceInstallGuard(t)
			launched := make(chan elevatedLaunch, 1)
			replaceSeam(t, &launchElevated, func(launch elevatedLaunch) (uint32, error) {
				launched <- launch
				return 7791, nil
			})
			waiterCalled := make(chan uint32, 1)
			releaseWaiter := make(chan struct{})
			var releaseOnce sync.Once
			releaseHelperWait := func() { releaseOnce.Do(func() { close(releaseWaiter) }) }
			// The first action waits for its helper on this channel. A
			// check below that ends the subtest early must still let
			// that action end, or its goroutine would stay blocked in
			// the helper wait into the next subtest.
			t.Cleanup(func() {
				releaseHelperWait()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) && serviceActionRunning.Load() {
					time.Sleep(10 * time.Millisecond)
				}
			})
			replaceSeam(t, &waitForServiceActionHelper, func(pid uint32) {
				waiterCalled <- pid
				<-releaseWaiter
			})
			replaceSeam(t, &confirmServiceAction, func(string, string) bool {
				t.Error("a refused press asked for a confirmation")
				return false
			})
			replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
				t.Error("a refused press read the service registration")
				return service.Status{}, nil
			})
			shown := recordMessagesChannel(t)

			actions := windowHostActionsOf(windowHost, panicGuard{})

			// The first press takes the guard and keeps it while its
			// helper runs.
			actions[logsStartServiceAction]()
			select {
			case <-launched:
			case <-time.After(5 * time.Second):
				t.Fatal("the start button never reached the menu's start flow")
			}
			if !serviceActionRunning.Load() {
				t.Fatal("the start did not take the service-action guard")
			}

			// The second press beside that running action is refused
			// by the ONE guard: its busy report, no confirmation, no
			// helper — and the guard of the running action survives.
			actions[logsStopServiceAction]()

			// The refusal is proven by its own report, by caption: the
			// companion re-shows its window around every service
			// action — the refused one and the one still running
			// included — so the busy report may arrive before or after
			// either re-show report, and no count of everything shown
			// can stand for "the refusal was reported".
			messages := waitForShownCaption(t, shown, serviceActionBusyCaption,
				"the busy report of the refused press")
			select {
			case second := <-launched:
				t.Errorf("the refused press launched a second helper %q", second.Action)
			default:
			}
			if !serviceActionRunning.Load() {
				t.Error("the refused press released the guard of the action that is still running")
			}

			if pid := <-waiterCalled; pid != 7791 {
				t.Errorf("the waiter waits for the process %d, want the launched helper 7791", pid)
			}
			// Release the first action and let it end, so nothing of
			// this subtest outlives it (the cleanup above takes the
			// same path when a check failed earlier).
			releaseHelperWait()
			waitForTrayCondition(t, "the guard release after the helper ended", func() bool {
				return !serviceActionRunning.Load()
			})

			// Every report of both presses has been shown by now: the
			// busy report exactly once, and, beside it, only the
			// companion window's own re-show report — the re-show of
			// either press, whichever the companion ran last, and
			// whether it ran before or after the busy report. The
			// desktop host's window re-reads its live page itself, so
			// its presses have no such report, but a press goroutine
			// still in flight must not turn into a false failure.
			messages = append(messages, drainMessages(shown)...)
			busy := 0
			for _, message := range messages {
				switch message.caption {
				case serviceActionBusyCaption:
					busy++
				case companionWindowUnavailableCaption:
				default:
					t.Errorf("the refused press showed %q (%q), want only the busy report of the guard",
						message.caption, message.text)
				}
			}
			if busy != 1 {
				t.Errorf("the refused press showed %d busy report(s), want exactly one", busy)
			}
		})
	}
}
