// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/app"
	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/service"
	"golang.org/x/sys/windows"
)

// capturedTrayRun is what a host wired into the tray loop that its launch
// called — the composition, the tooltip and the actions of the menu, plus
// the view feed of the status line.
type capturedTrayRun struct {
	style   trayMenuStyle
	tooltip string
	actions trayActions
	view    trayServiceView
}

// driveHostToTheTray runs the REAL startup of the desktop host up to its
// tray loop, with the loop replaced by a recorder that answers the ready
// channel immediately — so the launch completes without a notification
// area.
//
// It installs the two seams that carry THE invariant of a tray companion:
// the application — the first step that can create a server and its
// listener — and the publish of the claim must both stay untouched, or
// the path under test is not a companion. The claim assertion runs on
// disk: a companion launch leaves no published state behind.
func driveHostToTheTray(t *testing.T, opts Options) *capturedTrayRun {
	t.Helper()
	useTemporaryStateDir(t)
	// The message boxes are recorded instead shown: a message box blocks
	// a process without an interactive window station (the subsystem
	// warning and the WebView2 report of this launch reach it).
	recordMessages(t)

	applicationCreations := 0
	replaceSeam(t, &newApplication, func(app.Options, string) (*app.App, error) {
		applicationCreations++
		t.Error("the launch constructed an application: an application is the first step that can create a server and its listener")
		return nil, errors.New("a companion launch must never reach the application")
	})
	replaceSeam(t, &publishState, func(path, url string) error {
		t.Error("the launch published a claim: a claim says this session serves an address, and a companion serves nothing")
		return errors.New("a companion launch must never publish a claim")
	})

	captured := &capturedTrayRun{}
	replaceSeam(t, &runTrayLoop, func(icon []byte, tooltip string, style trayMenuStyle,
		actions trayActions, view trayServiceView, ready chan struct{},
	) {
		captured.style = style
		captured.tooltip = tooltip
		captured.actions = actions
		captured.view = view
		// The ready channel ends the tray-timeout watcher of the host:
		// without this close the launch would end the test process
		// through os.Exit after its budget.
		close(ready)
	})

	logFile := filepath.Join(t.TempDir(), "hfdesk.log")
	if err := run(opts, logFile); err != nil {
		t.Fatalf("the companion launch failed: %v", err)
	}

	if applicationCreations != 0 {
		t.Errorf("the application was created %d time(s), want none", applicationCreations)
	}
	if state, ok := service.ReadPublishedState(service.PublishedStatePath()); ok {
		t.Errorf("a claim %q exists after the companion launch", state.URL)
	}
	return captured
}

// assertCompanionWiring pins what every companion host wires into its tray:
// the decided composition — including the install action of the absence
// rule: without it a relaunch on a machine without a registration, and a
// menu after Delete, would be a dead end — plus the two items of the user's
// decision: Open HFDesk (the address of the registration) and Exit (the
// end path of this companion) — and NO handler for the one item the
// composition never carries (an action without its item would be a hidden
// second menu): the registered-service dialog belongs to the desktop host.
func assertCompanionWiring(t *testing.T, captured *capturedTrayRun) {
	t.Helper()
	if captured.style != trayMenuCompanion {
		t.Errorf("the tray composition = %d, want the decided companion composition %d",
			captured.style, trayMenuCompanion)
	}
	if captured.view.read == nil || captured.view.refresh == nil {
		t.Error("the companion menu carries no status feed: the line must read through the display policy")
	}
	for name, action := range map[string]func(){
		"open":    captured.actions.open,
		"runtime": captured.actions.runtime,
		"install": captured.actions.install,
		"start":   captured.actions.start,
		"stop":    captured.actions.stop,
		"update":  captured.actions.update,
		"delete":  captured.actions.delete,
		"quit":    captured.actions.quit,
	} {
		if action == nil {
			t.Errorf("the companion action %q is not wired", name)
		}
	}
	// The decided composition carries no registered-service dialog item
	// — its handler must not exist either.
	if captured.actions.service != nil {
		t.Error("the companion wires the service-dialog action although its item belongs to the desktop host only")
	}
}

// TestCompanionFlagLaunchCreatesNoServerNoListenerAndNoClaim drives the
// REAL startup of a launch that carries the private companion flag and
// pins the hard invariant of the companion: the application — the only
// path to a server and any listener — is never constructed, no claim is
// published, and the service gate is not even consulted: the flag names
// the mode, so this launch reads the registration NOWHERE (its menu and
// window read through the established reader only when they are really
// asked — and the tray of this test is a recorder).
func TestCompanionFlagLaunchCreatesNoServerNoListenerAndNoClaim(t *testing.T) {
	// The singleton must be free: a companion launch takes it like every
	// desktop launch, and a holder from another test would turn this
	// launch into an "already running" refusal that proves nothing.
	probe, err := acquireInstanceLock()
	assertNoError(t, err, "probe the single instance lock: %v", err)
	if probe.Exists() {
		probe.Release()
		t.Skip("another desktop test or app owns the per-session singleton")
	}
	probe.Release()

	gateReads := 0
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		gateReads++
		t.Error("the flag-driven companion launch read the service registration: it skips the gate, and its menu and window were not asked")
		return service.Status{}, nil
	})

	captured := driveHostToTheTray(t, Options{
		Companion:     true,
		CompanionWait: time.Minute,
	})

	if gateReads != 0 {
		t.Errorf("the service registration was read %d time(s), want no read at all", gateReads)
	}
	assertCompanionWiring(t, captured)
}

// TestGateCompanionChoiceContinuesAsCompanionWithoutAServer drives the
// REAL startup of a launch that finds a registration and answers the gate
// dialog with the tray-companion choice: the gate is consulted (its dialog
// offers the choice — the prompt carries OfferCompanion), and the launch
// continues exactly like the flag launch — no application, no claim, the
// strict companion wiring.
func TestGateCompanionChoiceContinuesAsCompanionWithoutAServer(t *testing.T) {
	probe, err := acquireInstanceLock()
	assertNoError(t, err, "probe the single instance lock: %v", err)
	if probe.Exists() {
		probe.Release()
		t.Skip("another desktop test or app owns the per-session singleton")
	}
	probe.Release()

	gateReads := 0
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		gateReads++
		return registeredStoppedService(), nil
	})
	replaceSeam(t, &showServiceChoice, func(prompt serviceChoicePrompt) serviceChoice {
		// The gate offers the tray companion beside the answers of the
		// state: the label is the label of the new choice.
		if !prompt.OfferCompanion {
			t.Error("the gate dialog does not offer the tray-companion choice")
		}
		return serviceChoiceCompanion
	})

	captured := driveHostToTheTray(t, Options{})

	if gateReads == 0 {
		t.Fatal("the gate never read the registration: the test did not cover the gate choice")
	}
	assertCompanionWiring(t, captured)
}

// TestRegisteredServiceDialogKeepsTheCompanionChoiceForTheGateOnly pins
// the SURFACE split of the new choice: the registered-service dialog of a
// running desktop (the Service... item and the install recheck reach it
// through offerRegisteredServiceFromTray) never offers the companion —
// none of its answers switches the mode of this host — while the launch
// gate always does.
func TestRegisteredServiceDialogKeepsTheCompanionChoiceForTheGateOnly(t *testing.T) {
	freshServiceInstallGuard(t)
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return registeredStoppedService(), nil
	})
	prompts := make(chan serviceChoicePrompt, 4)
	replaceSeam(t, &showServiceChoice, func(prompt serviceChoicePrompt) serviceChoice {
		prompts <- prompt
		return serviceChoiceCancel
	})

	// The registered-service dialog beside the running desktop.
	offerRegisteredServiceFromTray(registeredStoppedService(), testGateSingleton)
	dialogPrompt := <-prompts
	if dialogPrompt.OfferCompanion {
		t.Error("the registered-service dialog of a running desktop offers the tray companion: none of its answers switches this host's mode")
	}

	// The launch gate offers it.
	serviceGate(testGateSingleton)
	gatePrompt := <-prompts
	if !gatePrompt.OfferCompanion {
		t.Error("the launch gate does not offer the tray-companion choice")
	}
}

// TestCompanionActionsRunThroughTheCommonServiceFlow proves that the
// companion's menu items act through the very flow the desktop host runs:
// the confirmation with No as default comes BEFORE anything else, a
// declined answer launches no helper, reports nothing, releases the single
// service-action guard and asks for the menu catch-up — the established
// end path of every action (performServiceActionFromTray), reached from
// the companion's own wiring.
func TestCompanionActionsRunThroughTheCommonServiceFlow(t *testing.T) {
	probe, err := acquireInstanceLock()
	assertNoError(t, err, "probe the single instance lock: %v", err)
	if probe.Exists() {
		probe.Release()
		t.Skip("another desktop test or app owns the per-session singleton")
	}
	probe.Release()

	freshServiceInstallGuard(t)
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		t.Error("the flag-driven companion launch read the service registration at its gate")
		return service.Status{}, nil
	})
	captured := driveHostToTheTray(t, Options{Companion: true})

	// From here the action runs on the seams of the common flow: one
	// confirmation (declined), no helper, no report, guard released,
	// exactly one menu catch-up.
	confirmations := 0
	replaceSeam(t, &confirmServiceAction, func(caption, text string) bool {
		confirmations++
		if caption != stopServiceConfirmCaption {
			t.Errorf("the confirmation is captioned %q, want %q", caption, stopServiceConfirmCaption)
		}
		return false
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		t.Error("a declined confirmation launched an elevated helper")
		return 0, nil
	})
	var refreshes atomic.Int32
	replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })
	messages := recordMessages(t)

	captured.actions.stop()

	waitForTrayCondition(t, "the declined stop to release the guard and ask for the menu catch-up",
		func() bool {
			return !serviceActionRunning.Load() && refreshes.Load() == 1
		})
	if confirmations != 1 {
		t.Errorf("the companion stop asked %d confirmations, want exactly one question before anything else", confirmations)
	}
	if len(*messages) != 0 {
		t.Errorf("the declined stop showed %v, want no report: nothing was attempted", *messages)
	}
}

// TestCompleteInstallHandoverRelaunchesOnlyAfterSettledAuthorization
// drives the REAL end sequence of a committed installation handover at the
// very event handles the protocol uses, and pins both branches:
//
//   - a settled authorization (completion ran, the helper is alive, it
//     acknowledges the success signal) ends with the companion relaunch —
//     and at the moment of the spawn BOTH signals stand, so the spawn
//     provably runs strictly after the helper entered its parent-exit
//     wait: the departing host may exit at once, and the child waits for
//     the locks that exit and the installation still hold;
//   - a failed authorization is reported like every handover failure and
//     relaunches nothing: the installation did not run, so no companion
//     claims it did.
func TestCompleteInstallHandoverRelaunchesOnlyAfterSettledAuthorization(t *testing.T) {
	t.Run("a settled authorization relaunches the companion", func(t *testing.T) {
		helper := testEvent(t) // nonsignaled: the helper process is alive
		success := testEvent(t)
		confirmed := testEvent(t)
		// The helper acknowledges the authorization; the preset is the
		// settled acknowledgment the real helper leaves behind.
		if err := windows.SetEvent(confirmed); err != nil {
			t.Fatalf("preset the helper acknowledgment: %v", err)
		}
		handover := &desktopHandover{
			events: &handoverEvents{success: success, confirmed: confirmed},
			helper: &elevatedProcess{handle: helper},
		}
		spawns := 0
		replaceSeam(t, &startCompanionRelaunch, func() {
			spawns++
			// The spawn runs after BOTH evidences: the success signal
			// stands and the helper's acknowledgment arrived — the
			// exact moment the helper waits only for this process's
			// exit.
			if result, err := windows.WaitForSingleObject(success, 0); err != nil || result != windows.WAIT_OBJECT_0 {
				t.Errorf("the relaunch ran before the success signal stood (result %#x, err %v)", result, err)
			}
			if result, err := windows.WaitForSingleObject(confirmed, 0); err != nil || result != windows.WAIT_OBJECT_0 {
				t.Errorf("the relaunch ran before the helper acknowledged the authorization (result %#x, err %v)", result, err)
			}
		})
		// The native waiting notice of the real sequence shows a box on
		// its own goroutine — a channel recorder receives it without
		// racing the assertions of this test.
		shown := recordMessagesChannel(t)

		completeInstallHandover(handover, func() error { return nil })

		if spawns != 1 {
			t.Errorf("the settled handover spawned the companion %d time(s), want exactly one", spawns)
		}
		// The waiting notice is not joined by the protocol (it must
		// never hold the authorization), so it may still be in flight;
		// drain what arrived without blocking and pin that no FAILURE
		// report was shown beside a settled handover.
		for _, message := range drainMessages(shown) {
			if message.caption == serviceFailureCaption(serviceActionInstall) {
				t.Errorf("the settled handover showed the failure report %q", message.caption)
			}
		}
	})

	t.Run("a failed authorization reports instead of relaunching", func(t *testing.T) {
		helper := testEvent(t)
		success := testEvent(t)
		confirmed := testEvent(t)
		handover := &desktopHandover{
			events: &handoverEvents{success: success, confirmed: confirmed},
			helper: &elevatedProcess{handle: helper},
		}
		replaceSeam(t, &startCompanionRelaunch, func() {
			t.Error("a handover that was not authorized relaunched the companion: the installation did not run")
		})
		shown := recordMessagesChannel(t)

		failure := errors.New("the persistence of the server did not complete")
		completeInstallHandover(handover, func() error { return failure })

		// The failure report of this very sequence is shown
		// synchronously, so it has arrived by now: the established
		// report of the failed installation.
		found := false
		for _, message := range drainMessages(shown) {
			if message.caption == serviceFailureCaption(serviceActionInstall) {
				found = true
			}
		}
		if !found {
			t.Error("the failed handover showed no report of the failed installation")
		}
		if result, err := windows.WaitForSingleObject(success, 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
			t.Errorf("the failed handover left the success signal standing (result %#x, err %v): a helper could install on it", result, err)
		}
	})
}

// recordMessagesChannel replaces the message box with a channel recorder:
// the handover sequence shows one of its boxes from its own goroutine (the
// native waiting notice is never joined — it must not hold the
// authorization), and a channel receives it without racing the
// assertions.
func recordMessagesChannel(t *testing.T) chan shownMessage {
	t.Helper()
	shown := make(chan shownMessage, 8)
	replaceSeam(t, &showMessage, func(caption, text string, icon uint32) {
		shown <- shownMessage{caption: caption, text: text, icon: icon}
	})
	return shown
}

// drainMessages returns what arrived so far, without blocking: a box that
// is still in flight (the unjoined waiting notice) is simply not part of
// the assertions that only pin synchronous reports.
func drainMessages(shown chan shownMessage) []shownMessage {
	var messages []shownMessage
	for {
		select {
		case message := <-shown:
			messages = append(messages, message)
		default:
			return messages
		}
	}
}

// TestAcquireInstanceLockForLaunchWaitsOnlyForTheRelaunch pins the three
// rules of the bounded lock wait at the real named mutex:
//
//   - an ordinary launch never waits — it finds the held object and
//     reports the running instance exactly as before;
//   - the relaunch (CompanionWait > 0) waits for the moment both holders
//     — the exiting host and the elevated helper — released the object, and
//     acquires it then;
//   - the wait is bounded, and no attempt leaves a handle behind: a
//     leaked handle of the wait loop would keep the object alive BY ITS
//     OWN and the object would survive the release of every real holder.
func TestAcquireInstanceLockForLaunchWaitsOnlyForTheRelaunch(t *testing.T) {
	replaceSeam(t, &companionAcquirePollInterval, 20*time.Millisecond)

	holder, err := acquireInstanceLock()
	assertNoError(t, err, "acquire the single instance lock: %v", err)
	if holder.Exists() {
		holder.Release()
		t.Skip("another desktop test or app owns the per-session singleton")
	}
	released := false
	releaseHolder := func() {
		if !released {
			released = true
			holder.Release()
		}
	}
	t.Cleanup(releaseHolder)

	t.Run("an ordinary launch never waits", func(t *testing.T) {
		start := time.Now()
		lock, err := acquireInstanceLockForLaunch(Options{})
		assertNoError(t, err, "acquire the lock like an ordinary launch: %v", err)
		defer lock.Release()
		if !lock.Exists() {
			t.Fatal("the ordinary launch does not find the running instance")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("the ordinary launch waited %s: a launch without a companion wait decides immediately", elapsed)
		}
	})

	t.Run("the relaunch waits for the release and acquires", func(t *testing.T) {
		type outcome struct {
			lock *instanceLock
			err  error
		}
		acquired := make(chan outcome, 1)
		go func() {
			lock, err := acquireInstanceLockForLaunch(Options{CompanionWait: 5 * time.Second})
			acquired <- outcome{lock: lock, err: err}
		}()

		// The holders release only after the child started waiting.
		time.Sleep(100 * time.Millisecond)
		releaseHolder()

		select {
		case result := <-acquired:
			assertNoError(t, result.err, "the waiting acquisition failed: %v", result.err)
			defer result.lock.Release()
			if result.lock.Exists() {
				t.Fatal("the relaunch still finds the object held although every holder released it")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the relaunch never acquired the lock after the release")
		}
	})

	// The bounded end: the holder is re-created for this subtest, so the
	// wait really runs out.
	holder2, err := acquireInstanceLock()
	assertNoError(t, err, "acquire the single instance lock for the bounded wait: %v", err)
	if holder2.Exists() {
		holder2.Release()
		t.Fatal("the singleton reappeared although every holder released it")
	}
	t.Cleanup(func() { holder2.Release() })

	t.Run("the wait is bounded and leaks no handle", func(t *testing.T) {
		start := time.Now()
		lock, err := acquireInstanceLockForLaunch(Options{CompanionWait: 300 * time.Millisecond})
		assertNoError(t, err, "the bounded wait failed instead of ending: %v", err)
		if !lock.Exists() {
			lock.Release()
			t.Fatal("the bounded wait reports a free lock although the holder runs")
		}
		if elapsed := time.Since(start); elapsed < 250*time.Millisecond || elapsed > 5*time.Second {
			lock.Release()
			t.Errorf("the bounded wait ended after %s, want a wait close to its 300ms budget", elapsed)
		}
		// No attempt left a handle behind: after the returned lock and
		// the real holder both close, the object must DIE — a leaked
		// handle of the wait loop would keep it alive, and a later
		// launch would be refused as already running forever.
		lock.Release()
		holder2.Release()
		probe, err := acquireInstanceLock()
		assertNoError(t, err, "probe the singleton after the bounded wait: %v", err)
		if probe.Exists() {
			probe.Release()
			t.Fatal("the singleton survived the release of every holder: the wait loop leaked a handle and holds the session hostage")
		}
		probe.Release()
	})
}

// TestCompanionWindowPageReportsStateAndRegistrationAddress proves the
// fallback document of the Logs window: the honest state of the service —
// including the STOPPED state, where the window works exactly the same —
// the honest line that the registered address did not answer (or the
// honest reason there is no address to name), with the menu catch-up
// after every read like every other display read (failed reads
// included). The address is stated as a fact and never linked: it just
// proved it does not answer.
func TestCompanionWindowPageReportsStateAndRegistrationAddress(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    service.Status
		readErr   error
		wantState string
		wantNote  string
		wantAddr  string
		noAddr    bool
	}{
		{
			name:      "a stopped service names the address that did not answer",
			status:    registeredStoppedService(),
			wantState: "The HFDesk service is stopped.",
			wantAddr:  "http://127.0.0.1:18080",
		},
		{
			name:      "a running service whose address did not answer",
			status:    runningServiceStatus(service.StateRunning, "127.0.0.1:18080"),
			wantState: "The HFDesk service is running.",
			wantAddr:  "http://127.0.0.1:18080",
		},
		{
			name:      "a machine without a registration names no address",
			status:    service.Status{State: service.StateNotInstalled},
			wantState: "HFDesk is not registered as a Windows service.",
			wantNote:  "The registration names no address, so there is nothing to open.",
			noAddr:    true,
		},
		{
			name:      "a read that fails reports the honest unknown",
			readErr:   errors.New("the service control manager did not answer"),
			wantState: "The state of the Windows service could not be read.",
			wantNote:  "The address of the web UI could not be read.",
			noAddr:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
				return tc.status, tc.readErr
			})
			var refreshes atomic.Int32
			replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })

			page := companionWindowPage()

			assertNames(t, page, "the fallback document", []string{"<h1>" + tc.wantState + "</h1>"})
			if tc.noAddr {
				assertNamesWithout(t, page, "the honest reason", []string{tc.wantNote},
					[]string{"does not answer.", "<a ", "href="})
			} else {
				assertNamesWithout(t, page, "the address line", []string{
					"The web UI at <code>" + tc.wantAddr + "</code> does not answer.",
				}, []string{"<a ", "href="})
			}
			// The render reads the registration twice — the state
			// line and the service-log path of the same document —
			// and every read is reflected exactly once.
			if got := refreshes.Load(); got != 2 {
				t.Errorf("the window read asked for the menu catch-up %d time(s), want exactly once per read (2)", got)
			}
		})
	}
}

// TestCompanionWindowReadsEachAskTheMenuToCatchUp pins the catch-up rule of
// EVERY display read of the companion surfaces: the state read of a render,
// the read the service-log resolver runs for that same render, and the read
// it runs for the open-full-file click — each asks the tray menu to catch
// up right after it ran, a failed read included, so a result read can never
// stay invisible in the menu until the next tick.
//
// The counts are the pin: removing the catch-up of either read leaves reads
// ahead of catch-ups, and an extra read — with or without its own catch-up —
// breaks the exact count of a render.
func TestCompanionWindowReadsEachAskTheMenuToCatchUp(t *testing.T) {
	status := runningServiceStatus(service.StateRunning, "127.0.0.1:18080")

	for _, tc := range []struct {
		name    string
		readErr error
	}{
		{name: "a readable registration"},
		{name: "a failed read is reflected as well", readErr: errors.New("the service control manager did not answer")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int32
			replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
				reads.Add(1)
				return status, tc.readErr
			})
			var refreshes atomic.Int32
			replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })

			companionWindowPage()

			// The render reads the registration twice — the state
			// line and the service-log path of the same page — and
			// every read is reflected exactly once.
			if got := reads.Load(); got != 2 {
				t.Errorf("the render read the registration %d time(s), want its two reads", got)
			}
			if got := refreshes.Load(); got != reads.Load() {
				t.Errorf("%d display read(s) asked for the menu catch-up %d time(s), want one request per read",
					reads.Load(), got)
			}
		})
	}

	t.Run("the open-full-file click reflects its read", func(t *testing.T) {
		var reads atomic.Int32
		replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
			reads.Add(1)
			return status, nil
		})
		var refreshes atomic.Int32
		replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })
		replaceSeam(t, &showMessage, func(caption, text string, icon uint32) {})
		replaceSeam(t, &openExternalFile, func(path string) error { return nil })

		openCompanionServiceLog()

		if got := reads.Load(); got != 1 {
			t.Errorf("the click read the registration %d time(s), want exactly one", got)
		}
		if got := refreshes.Load(); got != reads.Load() {
			t.Errorf("%d display read(s) asked for the menu catch-up %d time(s), want one request per read",
				reads.Load(), got)
		}
	})

	t.Run("the live-URL read reflects its read", func(t *testing.T) {
		var reads atomic.Int32
		replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
			reads.Add(1)
			return status, nil
		})
		var refreshes atomic.Int32
		replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })

		if url := companionRuntimePageURL(); url != "http://127.0.0.1:18080/runtime.html" {
			t.Errorf("companionRuntimePageURL() = %q, want the runtime page of the registered address", url)
		}

		if got := reads.Load(); got != 1 {
			t.Errorf("the live-URL resolve read the registration %d time(s), want exactly one", got)
		}
		if got := refreshes.Load(); got != reads.Load() {
			t.Errorf("%d display read(s) asked for the menu catch-up %d time(s), want one request per read",
				reads.Load(), got)
		}
	})
}

// TestCompanionMenuAppliesTheAbsenceRuleAndTheSharedMapping proves the
// whole composition of the companion menu per read — the absence rule of
// the service group beside the shared mapping everything else follows:
//
//   - beside a proven REGISTRATION the four lifecycle actions show and
//     enable per state, the install item HIDES (it cannot act — the
//     registered menu is status + Open HFDesk + four actions +
//     Runtime / Logs + Exit), and Open HFDesk shows — enabled while the
//     service serves the address, grey beside a registered service that
//     does not run;
//   - beside a PROVEN ABSENCE the install item shows and acts while the
//     lifecycle items AND Open HFDesk hide (an item that would open
//     nothing never sits there) — the
//     shape a catch-up reaches after Delete from the companion and the
//     shape the relaunched child finds on a machine whose installation
//     failed: no dead end;
//   - an UNSETTLED read offers neither group and no Open HFDesk — the
//     honest unknown line
//     alone — because neither claim was proven;
//   - the registered-service dialog item stays NIL throughout: the
//     desktop host owns that dialog.
func TestCompanionMenuAppliesTheAbsenceRuleAndTheSharedMapping(t *testing.T) {
	line := &fakeTrayItem{}
	open := &fakeTrayItem{}
	install := &fakeTrayItem{}
	start := &fakeTrayItem{}
	stop := &fakeTrayItem{}
	update := &fakeTrayItem{}
	del := &fakeTrayItem{}
	menu := &trayServiceMenu{
		line: line, open: open, install: install, start: start, stop: stop, update: update, delete: del,
		installOnAbsence:   true,
		openFollowsService: true,
		// service stays NIL: the decided companion composition carries
		// no registered-service dialog item.
	}
	lifecycleItems := map[string]*fakeTrayItem{
		"start": start, "stop": stop, "update": update, "delete": del,
	}
	requireLifecycleHidden := func(t *testing.T, stage string) {
		t.Helper()
		for name, item := range lifecycleItems {
			if _, _, visible, _ := item.snapshot(); visible {
				t.Errorf("the %s item is visible at %q although that read offers no lifecycle action", name, stage)
			}
		}
	}
	requireInstallHidden := func(t *testing.T, stage string) {
		t.Helper()
		if _, _, visible, _ := install.snapshot(); visible {
			t.Errorf("the install item is visible at %q although that read does not prove an absence", stage)
		}
	}
	requireOpenHidden := func(t *testing.T, stage string) {
		t.Helper()
		if _, _, visible, _ := open.snapshot(); visible {
			t.Errorf("Open HFDesk is visible at %q although that read proves no registration to open", stage)
		}
	}

	// Registered and running: the line, the visible lifecycle section,
	// stop/update/delete enabled, start grey, Open HFDesk visible and
	// enabled — and NO install item: the registered shape of the
	// composition.
	menu.apply(serviceMenuFor(service.Status{State: service.StateRunning, Running: true}, nil))
	if title, _, _, _ := line.snapshot(); title != "Service: installed" {
		t.Errorf("the status line = %q, want the shared presence-only line", title)
	}
	requireInstallHidden(t, "a running registration")
	if _, enabled, visible, _ := open.snapshot(); !visible || !enabled {
		t.Errorf("Open HFDesk visible=%t enabled=%t, want it visible and enabled beside the running service", visible, enabled)
	}
	if _, enabled, visible, _ := start.snapshot(); !visible || enabled {
		t.Errorf("the start item visible=%t enabled=%t, want it visible and grey beside a running service", visible, enabled)
	}
	if _, enabled, visible, _ := stop.snapshot(); !visible || !enabled {
		t.Errorf("the stop item visible=%t enabled=%t, want it visible and enabled beside a running service", visible, enabled)
	}
	if _, enabled, visible, _ := update.snapshot(); !visible || !enabled {
		t.Errorf("the update item visible=%t enabled=%t, want it visible and enabled", visible, enabled)
	}
	if _, enabled, visible, _ := del.snapshot(); !visible || !enabled {
		t.Errorf("the delete item visible=%t enabled=%t, want it visible and enabled", visible, enabled)
	}

	// Registered and stopped: start becomes the action, stop goes grey,
	// and Open HFDesk stays VISIBLE but goes grey — the address is
	// proven, but nothing answers it yet; the install item stays hidden
	// — the registration is proven.
	menu.apply(serviceMenuFor(service.Status{State: service.StateStopped}, nil))
	requireInstallHidden(t, "a stopped registration")
	if _, enabled, visible, _ := open.snapshot(); !visible || enabled {
		t.Errorf("Open HFDesk visible=%t enabled=%t, want it visible and grey beside a stopped service", visible, enabled)
	}
	if _, enabled, visible, _ := start.snapshot(); !visible || !enabled {
		t.Errorf("the start item visible=%t enabled=%t, want it the visible action beside a stopped service", visible, enabled)
	}
	if _, enabled, _, _ := stop.snapshot(); enabled {
		t.Error("the stop item acts beside a stopped service")
	}

	// An UNSETTLED read offers neither group: the honest unknown line
	// alone, no lifecycle item, and no install item — an item that
	// cannot act never clutters this menu.
	menu.apply(serviceMenuFor(service.Status{}, errors.New("the service control manager did not answer")))
	if title, _, _, _ := line.snapshot(); title != "Service: unknown" {
		t.Errorf("the status line = %q, want the honest unknown state", title)
	}
	requireInstallHidden(t, "an unsettled read")
	requireLifecycleHidden(t, "an unsettled read")
	requireOpenHidden(t, "an unsettled read")

	// Proven absence — the shape after Delete from the companion and the
	// shape of a relaunched child whose installation failed: the install
	// item appears and acts, the lifecycle section and Open HFDesk are
	// gone. This
	// transition (registered → absent) is exactly what the menu catch-up
	// applies after the helper of a delete ended.
	menu.apply(serviceMenuFor(service.Status{State: service.StateNotInstalled}, nil))
	if title, _, _, _ := line.snapshot(); title != "Service: not installed" {
		t.Errorf("the status line = %q, want the shared presence-only line", title)
	}
	if _, enabled, visible, _ := install.snapshot(); !visible || !enabled {
		t.Errorf("the install item visible=%t enabled=%t beside a proven absence, want it visible and enabled: the menu must not be a dead end", visible, enabled)
	}
	requireLifecycleHidden(t, "a proven absence")
	requireOpenHidden(t, "a proven absence")

	// And back: a registration the next read proves hides the install
	// item again and brings Open HFDesk back — the relaunch child after
	// a SUCCESSFUL installation
	// lands on the registered shape.
	menu.apply(serviceMenuFor(service.Status{State: service.StateRunning, Running: true}, nil))
	requireInstallHidden(t, "the registration proved after the absence")
	if _, _, visible, _ := stop.snapshot(); !visible {
		t.Error("the lifecycle section did not return beside the proven registration")
	}
	if _, enabled, visible, _ := open.snapshot(); !visible || !enabled {
		t.Errorf("Open HFDesk visible=%t enabled=%t after the registration was proved again", visible, enabled)
	}
}

// TestCompanionInstallRunsThroughTheCommonServiceFlow proves that the
// install item of the absence rule acts through the very flow the desktop
// host's install item runs (runInstallServiceAction): the confirmation
// comes BEFORE the administrator prompt, a declined answer launches
// nothing and reports nothing but still asks for the menu catch-up — so
// the line catches up with whatever the attempt re-read — and an accepted
// one reaches the elevation, whose failure is reported by the established
// launch report of the flow with the same catch-up. The single
// service-action guard spans every step and is released on both end
// paths.
func TestCompanionInstallRunsThroughTheCommonServiceFlow(t *testing.T) {
	probe, err := acquireInstanceLock()
	assertNoError(t, err, "probe the single instance lock: %v", err)
	if probe.Exists() {
		probe.Release()
		t.Skip("another desktop test or app owns the per-session singleton")
	}
	probe.Release()

	freshServiceInstallGuard(t)
	// This machine reads as without a registration — the state the
	// install item's own visibility rule offers it in — for both reads
	// of the flow: the data directory of the confirmation text and the
	// recheck beside the elevated launch.
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return service.Status{State: service.StateNotInstalled}, nil
	})
	// The console program of the folder is settled, so the flow reaches
	// the confirmation instead of the missing-program notice.
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	captured := driveHostToTheTray(t, Options{Companion: true})
	if captured.actions.install == nil {
		t.Fatal("the companion does not wire its install action: a relaunch on a machine without a registration, and the menu after Delete, would be dead ends")
	}

	t.Run("a declined confirmation launches nothing and catches the menu up", func(t *testing.T) {
		freshServiceInstallGuard(t)
		confirmations := 0
		replaceSeam(t, &confirmServiceInstall, func(text string) bool {
			confirmations++
			if !strings.Contains(text, "administrator rights") {
				t.Errorf("the confirmation %q lacks the administrator-rights sentence of the common flow", text)
			}
			return false
		})
		replaceSeam(t, &launchElevatedProcess, func(elevatedLaunch) (*elevatedProcess, error) {
			t.Error("a declined confirmation started an elevated helper")
			return nil, nil
		})
		var refreshes atomic.Int32
		replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })
		messages := recordMessages(t)

		captured.actions.install()

		waitForTrayCondition(t, "the declined install to release the guard and catch the menu up",
			func() bool {
				return !serviceActionRunning.Load() && refreshes.Load() == 1
			})
		if confirmations != 1 {
			t.Errorf("the install asked %d confirmations, want exactly one question", confirmations)
		}
		if len(*messages) != 0 {
			t.Errorf("the declined install showed %v, want no report: nothing was attempted", *messages)
		}
	})

	t.Run("an accepted confirmation reaches the administrator prompt", func(t *testing.T) {
		freshServiceInstallGuard(t)
		// The steps are written by the action goroutine and read after
		// the atomic catch-up below, which orders them before this
		// read.
		var steps []string
		replaceSeam(t, &confirmServiceInstall, func(string) bool {
			steps = append(steps, "confirm")
			return true
		})
		replaceSeam(t, &launchElevatedProcess, func(elevatedLaunch) (*elevatedProcess, error) {
			steps = append(steps, "launch")
			return nil, errors.New(elevationDeclined)
		})
		var refreshes atomic.Int32
		replaceSeam(t, &serviceMenuRefresh, func() { refreshes.Add(1) })
		messages := recordMessages(t)

		captured.actions.install()

		waitForTrayCondition(t, "the failed elevation to release the guard and catch the menu up",
			func() bool {
				return !serviceActionRunning.Load() && refreshes.Load() == 1
			})
		if len(steps) != 2 || steps[0] != "confirm" || steps[1] != "launch" {
			t.Errorf("the steps = %v, want [confirm launch]: the confirmation must come before the administrator prompt", steps)
		}
		if len(*messages) != 1 {
			t.Fatalf("the failed elevation showed %d reports, want exactly the established launch report: %v", len(*messages), *messages)
		}
		if (*messages)[0].caption != serviceFailureCaption(serviceActionInstall) {
			t.Errorf("the report is captioned %q, want %q",
				(*messages)[0].caption, serviceFailureCaption(serviceActionInstall))
		}
	})
}

// TestCompanionOpenHFDeskOpensTheRegisteredAddress proves the Open HFDesk
// item of the companion: the wired action reads the REGISTRATION on the
// click and opens the address that read names in the default browser — not
// an address this host invented — and a successful open reports nothing.
func TestCompanionOpenHFDeskOpensTheRegisteredAddress(t *testing.T) {
	probe, err := acquireInstanceLock()
	assertNoError(t, err, "probe the single instance lock: %v", err)
	if probe.Exists() {
		probe.Release()
		t.Skip("another desktop test or app owns the per-session singleton")
	}
	probe.Release()

	captured := driveHostToTheTray(t, Options{Companion: true})
	if captured.actions.open == nil {
		t.Fatal("the companion does not wire its Open HFDesk action")
	}

	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return runningServiceStatus(service.StateRunning, "127.0.0.1:18080"), nil
	})
	opened := make(chan string, 1)
	replaceSeam(t, &openBrowser, func(url string) error {
		opened <- url
		return nil
	})
	shown := recordMessagesChannel(t)

	captured.actions.open()

	select {
	case url := <-opened:
		if want := "http://127.0.0.1:18080"; url != want {
			t.Errorf("the browser opened %q, want the address of the registration %q", url, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the Open HFDesk action never opened the address of the registration")
	}
	if messages := drainMessages(shown); len(messages) != 0 {
		t.Errorf("a successful open showed %v, want no report", messages)
	}
}

// TestCompanionExitEndsTheHostWithoutTouchingTheService proves the Exit
// item: the wired action runs the ESTABLISHED end path of this companion
// (shutdown first, then the end of the notification-area loop — the very
// path a signal takes), and nothing of it touches the Windows service: no
// registration is read, no confirmation is asked, no helper is launched,
// no report is shown. The companion closes; the service keeps its state.
func TestCompanionExitEndsTheHostWithoutTouchingTheService(t *testing.T) {
	probe, err := acquireInstanceLock()
	assertNoError(t, err, "probe the single instance lock: %v", err)
	if probe.Exists() {
		probe.Release()
		t.Skip("another desktop test or app owns the per-session singleton")
	}
	probe.Release()
	freshServiceInstallGuard(t)

	// Every service surface is armed: Exit reaching any of them fails
	// the test — the item closes THIS companion and nothing else.
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		t.Error("the Exit read the Windows service registration")
		return service.Status{}, nil
	})
	replaceSeam(t, &confirmServiceInstall, func(string) bool {
		t.Error("the Exit asked for an installation confirmation")
		return false
	})
	replaceSeam(t, &confirmServiceAction, func(string, string) bool {
		t.Error("the Exit asked for a service-action confirmation")
		return false
	})
	replaceSeam(t, &launchElevatedProcess, func(elevatedLaunch) (*elevatedProcess, error) {
		t.Error("the Exit launched an elevated helper")
		return nil, nil
	})
	ended := make(chan struct{}, 1)
	replaceSeam(t, &endCompanionTray, func() {
		ended <- struct{}{}
	})

	captured := driveHostToTheTray(t, Options{Companion: true})
	if captured.actions.quit == nil {
		t.Fatal("the companion does not wire its Exit action")
	}
	// The recorder joins after the launch: the WebView2 warning of a
	// machine without the runtime belongs to the launch, not to the
	// Exit this test pins.
	shown := recordMessagesChannel(t)

	captured.actions.quit()

	select {
	case <-ended:
		// endHost ran to its last step: the shutdown before it ran
		// first, so the host ended through the established path.
	case <-time.After(5 * time.Second):
		t.Fatal("the Exit never ended the notification-area loop through the established end path")
	}
	if serviceActionRunning.Load() {
		t.Error("the Exit left the single service-action guard taken: it acted on the service")
	}
	if messages := drainMessages(shown); len(messages) != 0 {
		t.Errorf("the Exit showed %v, want no report: it only closes the companion", messages)
	}
}

// TestCompanionWindowActionsCarryTheOpenLogActions pins the bridge of
// the fallback document and the live page: the window binds exactly the
// four fixed action names the documents call (open the file, open the
// folder, open the web UI, retry), the open-file action opens EXACTLY the service log
// file the document's path speaks about (the same resolver, so the path
// the document shows and the file the default handler receives can never
// be different files), the open-folder action opens that file's folder —
// both in the process of the window, on the host side — and a file or
// folder the service has not written yet reports honestly instead of
// starting a handler without a target. The retry beside a window that is
// gone (current() returns nil) is a no-op instead of a panic.
func TestCompanionWindowActionsCarryTheOpenLogActions(t *testing.T) {
	actions := companionWindowActions(panicGuard{},
		func() *runtimeWindow { return nil },
		func() bool { return false })
	if len(actions) != 4 {
		keys := make([]string, 0, len(actions))
		for name := range actions {
			keys = append(keys, name)
		}
		t.Fatalf("the window actions = %v, want exactly the four fixed names", keys)
	}
	for _, name := range []string{openLogFileAction, openLogFolderAction, openUiAction, logsTryAgainAction} {
		if actions[name] == nil {
			t.Fatalf("the window action %q is not bound: its button would find no function", name)
		}
	}

	t.Run("the retry beside a gone window is a no-op", func(t *testing.T) {
		// current() returns nil: the handler must not panic and must
		// not show anything — a retry can only fail when the window
		// that asked for it is already gone.
		replaceSeam(t, &showMessage, func(caption, text string, icon uint32) {
			t.Errorf("the retry showed %q although there is no window to retry", caption)
		})
		actions[logsTryAgainAction]()
		// The handler runs on its own goroutine: a panic there would
		// fail the test process, so a moment without one is the proof.
		time.Sleep(50 * time.Millisecond)
	})

	t.Run("the file action opens exactly the file the document shows", func(t *testing.T) {
		logDir := t.TempDir()
		logPath := filepath.Join(logDir, serviceLogFileName)
		if err := os.WriteFile(logPath, []byte("record one\nrecord two\n"), 0o600); err != nil {
			t.Fatalf("write the service log: %v", err)
		}
		replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
			return service.Status{
				State: service.StateRunning,
				Paths: runtimepaths.Paths{LogDir: logDir},
			}, nil
		})
		opened := make(chan string, 1)
		replaceSeam(t, &openExternalFile, func(path string) error {
			opened <- path
			return nil
		})
		shown := recordMessagesChannel(t)

		actions[openLogFileAction]()

		select {
		case path := <-opened:
			if path != logPath {
				t.Errorf("the default handler received %q, want the service log file %q", path, logPath)
			}
			// The document's path reads through the very same
			// resolver, so the path it shows and the file the
			// default handler receives are one and the same path.
			if resolved := companionServiceLogPath(); resolved != path {
				t.Errorf("the document speaks about %q, the action opened %q: two different files", resolved, path)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the window action never handed the service log file to the default handler")
		}
		if messages := drainMessages(shown); len(messages) != 0 {
			t.Errorf("a successful open showed %v, want no report", messages)
		}
	})

	t.Run("the folder action opens the folder of that log", func(t *testing.T) {
		logDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(logDir, serviceLogFileName), []byte("record\n"), 0o600); err != nil {
			t.Fatalf("write the service log: %v", err)
		}
		replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
			return service.Status{
				State: service.StateRunning,
				Paths: runtimepaths.Paths{LogDir: logDir},
			}, nil
		})
		opened := make(chan string, 1)
		replaceSeam(t, &openExternalFolder, func(path string) error {
			opened <- path
			return nil
		})
		shown := recordMessagesChannel(t)

		actions[openLogFolderAction]()

		select {
		case dir := <-opened:
			if dir != logDir {
				t.Errorf("the file manager received %q, want the log folder %q", dir, logDir)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the window action never handed the log folder to the file manager")
		}
		if messages := drainMessages(shown); len(messages) != 0 {
			t.Errorf("a successful open showed %v, want no report", messages)
		}
	})

	t.Run("a log that does not exist yet reports honestly and opens nothing", func(t *testing.T) {
		logDir := t.TempDir() // no log file in it
		replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
			return service.Status{
				State: service.StateRunning,
				Paths: runtimepaths.Paths{LogDir: logDir},
			}, nil
		})
		replaceSeam(t, &openExternalFile, func(path string) error {
			t.Errorf("the default handler was started for the missing file %q", path)
			return nil
		})
		shown := recordMessagesChannel(t)

		actions[openLogFileAction]()

		waitForTrayCondition(t, "the honest report of a service log that does not exist yet",
			func() bool { return len(shown) == 1 })
		messages := drainMessages(shown)
		if len(messages) != 1 {
			t.Fatalf("the missing log showed %d reports, want exactly one: %v", len(messages), messages)
		}
		if messages[0].caption != serviceLogOpenFailedCaption {
			t.Errorf("the report is captioned %q, want %q", messages[0].caption, serviceLogOpenFailedCaption)
		}
		assertNames(t, messages[0].text, "the missing-log report", []string{
			"The service log file has not been created yet",
		})
	})

	t.Run("a folder that does not exist yet reports honestly and opens nothing", func(t *testing.T) {
		// The registration names a log directory that was never
		// created: the service has never written a log on this
		// machine.
		missing := filepath.Join(t.TempDir(), "never-created")
		logPath := filepath.Join(missing, serviceLogFileName)
		replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
			return service.Status{
				State: service.StateRunning,
				Paths: runtimepaths.Paths{LogDir: missing},
			}, nil
		})
		replaceSeam(t, &openExternalFolder, func(path string) error {
			t.Errorf("the file manager was started for the missing folder %q", path)
			return nil
		})
		shown := recordMessagesChannel(t)

		actions[openLogFolderAction]()

		waitForTrayCondition(t, "the honest report of a log folder that does not exist yet",
			func() bool { return len(shown) == 1 })
		messages := drainMessages(shown)
		if len(messages) != 1 {
			t.Fatalf("the missing folder showed %d reports, want exactly one: %v", len(messages), messages)
		}
		if messages[0].caption != logFolderOpenFailedCaption {
			t.Errorf("the report is captioned %q, want %q", messages[0].caption, logFolderOpenFailedCaption)
		}
		assertNames(t, messages[0].text, "the missing-folder report", []string{
			"The log folder has not been created yet",
			"The log folder is at " + filepath.Dir(logPath),
		})
	})
}
