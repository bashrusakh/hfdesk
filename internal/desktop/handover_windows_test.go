// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func readyTestProcess(t *testing.T, launch elevatedLaunch) *elevatedProcess {
	t.Helper()
	ready, err := openNamedEvent(launch.ReadyEvent, windows.EVENT_MODIFY_STATE)
	assertNoError(t, err, "open the parent-created ready event: %v", err)
	if err := windows.SetEvent(ready); err != nil {
		_ = windows.CloseHandle(ready)
		t.Fatalf("signal the helper-ready event: %v", err)
	}
	_ = windows.CloseHandle(ready)
	handle, err := windows.CreateEvent(nil, 1, 0, nil)
	assertNoError(t, err, "create the test helper-process wait handle: %v", err)
	return &elevatedProcess{handle: handle, pid: 5151}
}

func testHelperHandover(t *testing.T) *helperHandover {
	t.Helper()
	handles := make([]windows.Handle, 4)
	for i := range handles {
		handle, err := windows.CreateEvent(nil, 1, 0, nil)
		if err != nil {
			for _, opened := range handles[:i] {
				_ = windows.CloseHandle(opened)
			}
			t.Fatalf("create test handover event %d: %v", i, err)
		}
		handles[i] = handle
	}
	handover := &helperHandover{parent: handles[0], ready: handles[1], success: handles[2], mutex: handles[3]}
	t.Cleanup(handover.close)
	return handover
}

func testEvent(t *testing.T) windows.Handle {
	t.Helper()
	handle, err := windows.CreateEvent(nil, 1, 0, nil)
	assertNoError(t, err, "create test event: %v", err)
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
	return handle
}

func duplicateTestHandle(t *testing.T, handle windows.Handle) windows.Handle {
	t.Helper()
	process, err := windows.GetCurrentProcess()
	assertNoError(t, err, "get the current process handle: %v", err)
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(process, handle, process, &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		t.Fatalf("duplicate test handle: %v", err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(duplicate) })
	return duplicate
}

func securityEntries(t *testing.T, acl *windows.ACL) map[string]windows.ACCESS_MASK {
	t.Helper()
	entries := make(map[string]windows.ACCESS_MASK, acl.AceCount)
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		err := windows.GetAce(acl, i, &ace)
		assertNoError(t, err, "read DACL entry %d: %v", i, err)
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("DACL entry %d has ACE type %d, want an allow ACE", i, ace.Header.AceType)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		entries[sid.String()] |= ace.Mask
	}
	return entries
}

func TestHandoverEventsAreUniqueNonsignaledAndRestricted(t *testing.T) {
	events, err := newHandoverEvents()
	assertNoError(t, err, "create the handover event pair: %v", err)
	defer events.close()
	if events.readyName == events.successName || !strings.HasPrefix(events.readyName, handoverEventPrefix) {
		t.Fatalf("the event names are not a unique pair: %q, %q", events.readyName, events.successName)
	}
	sid, err := currentUserSID()
	assertNoError(t, err, "read the initiating user's SID: %v", err)
	admin, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	assertNoError(t, err, "build the Administrators SID: %v", err)
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	assertNoError(t, err, "build the SYSTEM SID: %v", err)
	for i, handle := range []windows.Handle{events.ready, events.success} {
		result, err := windows.WaitForSingleObject(handle, 0)
		if err != nil || result != uint32(windows.WAIT_TIMEOUT) {
			t.Fatalf("new event state = %#x, err=%v; want nonsignaled", result, err)
		}
		descriptor, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
		assertNoError(t, err, "read the event security descriptor: %v", err)
		acl, _, err := descriptor.DACL()
		if err != nil || acl == nil || acl.AceCount != 3 {
			t.Fatalf("event DACL = %v, err=%v; want exactly three explicit principals", acl, err)
		}
		entries := securityEntries(t, acl)
		userRights, adminRights := windows.SYNCHRONIZE, windows.EVENT_MODIFY_STATE
		if i == 1 {
			userRights, adminRights = windows.EVENT_MODIFY_STATE, windows.SYNCHRONIZE
		}
		want := make(map[string]windows.ACCESS_MASK, 3)
		want[sid] |= windows.ACCESS_MASK(userRights)
		want[admin.String()] |= windows.ACCESS_MASK(adminRights)
		want[system.String()] |= windows.ACCESS_MASK(handoverEventAccess)
		if len(entries) != len(want) {
			t.Errorf("event DACL principals = %v, want exactly %v", entries, want)
		}
		for principal, rights := range want {
			if entries[principal] != rights {
				t.Errorf("event DACL grants %#x to %s, want %#x", entries[principal], principal, rights)
			}
		}
	}
	attrs, descriptor, err := restrictedObjectSecurityFor(windows.SYNCHRONIZE, windows.EVENT_MODIFY_STATE, handoverEventAccess)
	assertNoError(t, err, "build the event security descriptor for a duplicate-name probe: %v", err)
	if _, err := createHandoverEvent(events.readyName, attrs); err == nil {
		t.Error("a reused named event was accepted")
	}
	_ = descriptor
}

func TestWaitForHelperReadyRequiresLiveHelper(t *testing.T) {
	helper := testEvent(t)
	ready := testEvent(t)
	if err := windows.SetEvent(ready); err != nil {
		t.Fatalf("signal helper readiness: %v", err)
	}
	if err := waitForHelperReady(helper, ready, time.Second); err != nil {
		t.Fatalf("a live ready helper was rejected: %v", err)
	}
	if err := windows.SetEvent(helper); err != nil {
		t.Fatalf("signal helper exit: %v", err)
	}
	if err := waitForHelperReady(helper, ready, time.Second); err == nil {
		t.Fatal("simultaneous ready and helper exit authorized shutdown")
	}
	if err := waitForHelperReady(testEvent(t), testEvent(t), 10*time.Millisecond); err == nil {
		t.Fatal("a helper that never became ready was accepted")
	}
}

func TestWaitForHandoverAuthorizationRequiresBothSignals(t *testing.T) {
	parentOnly, noSuccess := testEvent(t), testEvent(t)
	if err := windows.SetEvent(parentOnly); err != nil {
		t.Fatalf("signal parent exit: %v", err)
	}
	if err := waitForHandoverAuthorization(parentOnly, noSuccess, time.Second, time.Second); err == nil {
		t.Fatal("parent exit without a success signal authorized service start")
	}
	parentLater, successOnly := testEvent(t), testEvent(t)
	if err := windows.SetEvent(successOnly); err != nil {
		t.Fatalf("signal success: %v", err)
	}
	if err := waitForHandoverAuthorization(parentLater, successOnly, time.Second, 20*time.Millisecond); err == nil {
		t.Fatal("success without parent exit authorized service start")
	}
	parentAndSuccess, bothSignals := testEvent(t), testEvent(t)
	if err := windows.SetEvent(bothSignals); err != nil {
		t.Fatalf("signal successful completion: %v", err)
	}
	if err := windows.SetEvent(parentAndSuccess); err != nil {
		t.Fatalf("signal parent exit: %v", err)
	}
	err := waitForHandoverAuthorization(parentAndSuccess, bothSignals, time.Second, time.Second)
	assertNoError(t, err, "simultaneous success and parent exit were rejected: %v", err)
}

func TestOnlySuccessfulCompletionSignalsTheHelper(t *testing.T) {
	helper, success := testEvent(t), testEvent(t)
	want := errors.New("persistence did not complete successfully")
	if err := authorizeHandoverCompletion(func() error { return want }, helper, success); !errors.Is(err, want) {
		t.Fatalf("completion error = %v, want %v", err, want)
	}
	if result, err := windows.WaitForSingleObject(success, 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("failed completion signaled success: result=%#x err=%v", result, err)
	}
	if err := windows.SetEvent(helper); err != nil {
		t.Fatalf("signal early helper exit: %v", err)
	}
	if err := authorizeHandoverCompletion(func() error { return nil }, helper, success); err == nil {
		t.Fatal("an exited helper was treated as waiting for success")
	}
	if result, err := windows.WaitForSingleObject(success, 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("early helper exit signaled success: result=%#x err=%v", result, err)
	}
	liveHelper := testEvent(t)
	if err := authorizeHandoverCompletion(func() error { return nil }, liveHelper, success); err != nil {
		t.Fatalf("successful completion did not authorize the live helper: %v", err)
	}
	if result, err := windows.WaitForSingleObject(success, 0); err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("successful completion did not signal the helper: result=%#x err=%v", result, err)
	}
}

func TestShutdownCoordinatorDoesNotLetQuitRaceCommit(t *testing.T) {
	var coordinator desktopShutdownCoordinator
	if _, committed := coordinator.beginShutdown(); committed {
		t.Fatal("ordinary shutdown unexpectedly found a handover")
	}
	if coordinator.commit(&desktopHandover{}) {
		t.Fatal("an install committed after ordinary shutdown had already begun")
	}

	var handoverCoordinator desktopShutdownCoordinator
	handover := &desktopHandover{}
	if !handoverCoordinator.commit(handover) {
		t.Fatal("a ready helper could not commit before shutdown")
	}
	got, committed := handoverCoordinator.beginShutdown()
	if !committed || got != handover {
		t.Fatalf("shutdown snapshot = (%p, %t), want (%p, true)", got, committed, handover)
	}
}

func TestInstanceMutexSurvivesParentHandleUntilHelperReleases(t *testing.T) {
	parent, err := acquireInstanceLock()
	assertNoError(t, err, "acquire the desktop singleton: %v", err)
	if parent.Exists() {
		parent.Release()
		t.Skip("another desktop test or app owns the per-session singleton")
	}
	helper, err := openDesktopSingleton(parent.name)
	if err != nil {
		parent.Release()
		t.Fatalf("open the existing singleton as the elevated helper: %v", err)
	}
	security, err := windows.GetSecurityInfo(parent.handle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		_ = windows.CloseHandle(helper)
		parent.Release()
		t.Fatalf("read singleton DACL: %v", err)
	}
	acl, _, err := security.DACL()
	if err != nil || acl == nil || acl.AceCount != 3 {
		t.Errorf("singleton ACL = %v, err=%v; want user, Administrators, and SYSTEM", acl, err)
	} else {
		entries := securityEntries(t, acl)
		user, err := currentUserSID()
		assertNoError(t, err, "read the initiating user's SID: %v", err)
		admin, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
		assertNoError(t, err, "build the Administrators SID: %v", err)
		system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
		assertNoError(t, err, "build the SYSTEM SID: %v", err)
		want := make(map[string]windows.ACCESS_MASK, 3)
		want[user] |= windows.ACCESS_MASK(windows.MUTEX_ALL_ACCESS)
		want[admin.String()] |= windows.ACCESS_MASK(windows.READ_CONTROL)
		want[system.String()] |= windows.ACCESS_MASK(windows.READ_CONTROL)
		if len(entries) != len(want) || entries[user] != want[user] ||
			entries[admin.String()] != want[admin.String()] || entries[system.String()] != want[system.String()] {
			t.Errorf("singleton DACL entries = %v, want %v", entries, want)
		}
	}
	parent.Release()
	probe, err := acquireInstanceLock()
	if err != nil {
		_ = windows.CloseHandle(helper)
		t.Fatalf("probe the retained singleton: %v", err)
	}
	if !probe.Exists() {
		t.Error("the singleton disappeared while the helper still held it")
	}
	probe.Release()
	_ = windows.CloseHandle(helper)
	fresh, err := acquireInstanceLock()
	assertNoError(t, err, "acquire singleton after helper release: %v", err)
	if fresh.Exists() {
		t.Error("the singleton remained after its last helper handle closed")
	}
	fresh.Release()
}

func TestProcessWaitUsesCurrentWindowsProcess(t *testing.T) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(os.Getpid()))
	assertNoError(t, err, "open the current process for synchronization: %v", err)
	defer windows.CloseHandle(handle)
	if result, err := windows.WaitForSingleObject(handle, 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("the current process is not reported live: result=%#x err=%v", result, err)
	}
}

// TestHandoverNoticeDoesNotHoldCompletionAuthorization proves the native
// waiting notice is shown but never joined: completion evidence and the
// success signal proceed while the modal box is still undismissed.
func TestHandoverNoticeDoesNotHoldCompletionAuthorization(t *testing.T) {
	handover := &desktopHandover{
		events: &handoverEvents{ready: testEvent(t), success: testEvent(t)},
		helper: &elevatedProcess{handle: testEvent(t)},
	}

	noticeOpen := make(chan struct{})
	noticeDone := make(chan struct{})
	noticeDismissed := make(chan struct{})
	var openOnce, doneOnce sync.Once
	replaceSeam(t, &showMessage, func(caption, text string, icon uint32) {
		openOnce.Do(func() { close(noticeOpen) })
		<-noticeDismissed
		doneOnce.Do(func() { close(noticeDone) })
	})
	t.Cleanup(func() {
		select {
		case <-noticeDismissed:
		default:
			close(noticeDismissed)
		}
		select {
		case <-noticeDone:
		case <-time.After(2 * time.Second):
		}
	})

	completionGate := make(chan struct{})
	waitForCompletion := func() error { <-completionGate; return nil }
	done := make(chan error, 1)
	go func() { done <- completeHandoverWithNotice(handover, waitForCompletion) }()

	select {
	case <-noticeOpen:
	case <-time.After(2 * time.Second):
		t.Fatal("the native waiting notice never appeared")
	}
	select {
	case err := <-done:
		t.Fatalf("authorization finished before the completion evidence: %v", err)
	default:
	}

	close(completionGate)
	select {
	case err := <-done:
		assertNoError(t, err, "the authorized handover failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("completion authorization was held by the open notice")
	}
	if result, err := windows.WaitForSingleObject(handover.events.success, 0); err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("success was not signaled while the notice was open: result=%#x err=%v", result, err)
	}
	close(noticeDismissed)
	select {
	case <-noticeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the notice goroutine never finished")
	}
}

// TestHandoverExitWindowSurvivesAFullCompletionWait proves the exit evidence
// gets its own full bounded window: a completion signal arriving late in its
// budget must not leave a remainder clamped to zero for the desktop exit.
func TestHandoverExitWindowSurvivesAFullCompletionWait(t *testing.T) {
	parent, success := testEvent(t), testEvent(t)
	const budget = time.Second
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		time.Sleep(500 * time.Millisecond)
		_ = windows.SetEvent(success)
		// The desktop exits at ~1.1 s: past the shared deadline the old
		// countdown would have zeroed, but inside the fresh exit window.
		time.Sleep(600 * time.Millisecond)
		_ = windows.SetEvent(parent)
	}()
	defer func() {
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
		}
	}()

	start := time.Now()
	err := waitForHandoverAuthorization(parent, success, budget, budget)
	assertNoError(t, err, "a late completion starved the exit window: %v", err)
	if elapsed := time.Since(start); elapsed < budget {
		t.Fatalf("authorization returned after %s, before the parent exit at ~1.1s", elapsed)
	}
}

// TestHelperHandoverKeepsOnlyTheSingletonAfterAuthorization proves the
// evidence handles die with the authorization outcome while the singleton
// survives to cover the service mutation, and close releases it too.
func TestHelperHandoverKeepsOnlyTheSingletonAfterAuthorization(t *testing.T) {
	handover := testHelperHandover(t)
	handover.releaseAuthorizationHandles()
	if handover.parent != 0 || handover.ready != 0 || handover.success != 0 {
		t.Errorf("authorization handles survived: parent=%d ready=%d success=%d",
			handover.parent, handover.ready, handover.success)
	}
	if handover.mutex == 0 {
		t.Fatal("the singleton was released before the service mutation ended")
	}
	handover.close()
	if handover.mutex != 0 {
		t.Error("close left the singleton handle behind")
	}
}
