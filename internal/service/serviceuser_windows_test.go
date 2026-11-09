// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestDesktopSingletonNameGuardsTheNameShape pins the name of the
// per-session desktop singleton: the namespace prefix, the user and the
// session, in that order.
//
// The name is the shared identity of the desktop host, the elevated helper
// and the service command: a change of its shape would make one of them hold
// an object the others never see, so the shape is pinned where it is
// computed.
func TestDesktopSingletonNameGuardsTheNameShape(t *testing.T) {
	name, err := DesktopSingletonName()
	if err != nil {
		t.Fatalf("DesktopSingletonName() failed: %v", err)
	}
	sid, err := processUserSID()
	if err != nil {
		t.Fatalf("read the user of the process: %v", err)
	}
	var session uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &session); err != nil {
		t.Fatalf("read the session of the process: %v", err)
	}
	want := desktopSingletonPrefix + sid + "." + strconv.FormatUint(uint64(session), 10)
	if name != want {
		t.Errorf("DesktopSingletonName() = %q, want %q", name, want)
	}
	if !strings.HasPrefix(name, `Local\HFDesk.Desktop.`) {
		t.Errorf("the singleton name %q does not carry the local-namespace product prefix", name)
	}
	if !strings.HasPrefix(sid, "S-1-") {
		t.Errorf("the user part %q is not a security identifier", sid)
	}
}

// TestHoldDesktopSingletonHoldsAndReleasesANamedObject proves both sides of
// one hold: while it is held the object exists and its creator can open it,
// and after the release the object is gone again, so a later launch is never
// refused by a hold that ended.
//
// The test names a test object of its own instead of the real per-session
// name: the desktop package's tests own that object on the same machine, and
// two test packages creating it in parallel would make both results depend
// on timing.
func TestHoldDesktopSingletonHoldsAndReleasesANamedObject(t *testing.T) {
	const name = `Local\HFDesk.Test.ServiceHold.1`
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatalf("encode the test singleton name: %v", err)
	}

	release, err := HoldDesktopSingleton(name)
	if err != nil {
		t.Fatalf("HoldDesktopSingleton(%q) failed: %v", name, err)
	}
	if release == nil {
		t.Fatal("a successful hold returned no release")
	}

	handle, err := windows.OpenMutex(windows.MUTEX_ALL_ACCESS, false, namePtr)
	if err != nil {
		t.Fatalf("the held object cannot be opened by the identity that created it: %v", err)
	}
	_ = windows.CloseHandle(handle)

	release()
	// A second release ends nothing instead of closing a handle twice.
	release()

	if handle, err := windows.OpenMutex(windows.READ_CONTROL, false, namePtr); err == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("the singleton object survived the release: a later launch would be refused as already running")
	} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("read the singleton after the release: %v", err)
	}
}

// TestHoldDesktopSingletonFailsWhenTheSessionCannotBeNamed proves the hold
// fails closed when the object cannot be named: a guessed or empty name
// could hold an object of another session, and a hold that cannot be
// established must return no release at all.
func TestHoldDesktopSingletonFailsWhenTheSessionCannotBeNamed(t *testing.T) {
	previous := desktopSessionIdentity
	desktopSessionIdentity = func() (string, uint32, error) {
		return "", 0, errors.New("the session of the process cannot be read")
	}
	t.Cleanup(func() { desktopSessionIdentity = previous })

	release, err := HoldDesktopSingleton("")
	if err == nil {
		release()
		t.Fatal("a hold without a name succeeded although the session cannot be read")
	}
	if release != nil {
		t.Error("a failed hold returned a release")
	}
	if !strings.Contains(err.Error(), "cannot be named") {
		t.Errorf("the refusal %q does not name the cause", err)
	}
}

// TestHoldDesktopSingletonReportsAnIdentityThatCannotAccessTheObject proves
// the refusal of an object another Windows identity owns: the hold refuses
// before any mutation, classifies the access denial, and names the step that
// ends it instead of reporting a generic failure.
func TestHoldDesktopSingletonReportsAnIdentityThatCannotAccessTheObject(t *testing.T) {
	const name = `Local\HFDesk.Test.ServiceDeny.1`
	createDeniedMutex(t, name)

	release, err := HoldDesktopSingleton(name)
	if err == nil {
		release()
		t.Fatal("the hold took an object its identity cannot access")
	}
	if !errors.Is(err, ErrSingletonAccessDenied) {
		t.Errorf("the refusal %v is not classified as ErrSingletonAccessDenied", err)
	}
	for _, want := range []string{
		"this Windows identity cannot access it",
		"different Windows account",
		"end that process and try again",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q", err, want)
		}
	}
}

// TestHoldDesktopSingletonHoldsAnObjectOfAnotherIdentityThroughReadControl
// proves the cross-identity side of the hold: an object whose descriptor
// grants this identity read control only denies the full-access create
// request, and the hold still succeeds through the limited open — any handle
// keeps the object alive, which is all a hold needs — while the descriptor
// itself stays untouched: the administrators keep read control and nothing
// more. Without the fallback the hold would refuse although the rights model
// allows it.
func TestHoldDesktopSingletonHoldsAnObjectOfAnotherIdentityThroughReadControl(t *testing.T) {
	const name = `Local\HFDesk.Test.ServiceLimited.1`
	sid, err := processUserSID()
	if err != nil {
		t.Fatalf("read the user of the process: %v", err)
	}
	fixture := createReadControlMutex(t, name, sid)

	release, err := HoldDesktopSingleton(name)
	if err != nil {
		t.Fatalf("the hold refused although its descriptor grants this identity read control: %v", err)
	}
	if release == nil {
		t.Fatal("a successful hold returned no release")
	}

	// The creator of the object is gone: only the hold keeps it alive, so
	// the probe below proves that the limited handle really is a hold.
	_ = windows.CloseHandle(fixture)

	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		release()
		t.Fatalf("encode the test singleton name: %v", err)
	}
	if handle, err := windows.OpenMutex(windows.READ_CONTROL, false, namePtr); err == nil {
		_ = windows.CloseHandle(handle)
	} else {
		release()
		t.Fatalf("the object did not survive its creator during the hold: %v", err)
	}

	release()
	// A second release ends nothing instead of closing a handle twice.
	release()

	if handle, err := windows.OpenMutex(windows.READ_CONTROL, false, namePtr); err == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("the singleton object survived the release: a later launch would be refused as already running")
	} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("read the singleton after the release: %v", err)
	}
}

// createReadControlMutex creates a named mutex whose descriptor grants this
// identity read control only: the shape a singleton of this session has for
// a hold that requested full mutex access — the request is denied, while the
// limited access of the descriptor still opens the object. The fixture
// handle is closed when the test ends unless the test closes it earlier, and
// the object dies with its last handle.
func createReadControlMutex(t *testing.T, name, sid string) windows.Handle {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(
		fmt.Sprintf("D:P(A;;0x%08X;;;%s)", uint32(windows.READ_CONTROL), sid))
	if err != nil {
		t.Fatalf("build the read-control descriptor: %v", err)
	}
	attrs := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatalf("encode the test singleton name: %v", err)
	}
	handle, err := windows.CreateMutex(attrs, false, namePtr)
	if handle == 0 {
		if err == nil {
			err = errors.New("the operating system returned no handle")
		}
		t.Fatalf("create the read-control fixture object: %v", err)
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		t.Fatal("the fixture object already exists: the test would deny a real object")
	}
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
	return handle
}

// createDeniedMutex creates a named mutex whose descriptor grants the object
// to SYSTEM only: the identity of the test has no access to it, which is the
// shape a singleton another Windows identity created has for this one — the
// object cannot be opened even with the limited read-control access, so the
// hold refuses with ErrSingletonAccessDenied. The fixture handle is closed
// when the test ends, and the object dies with its last handle.
func createDeniedMutex(t *testing.T, name string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(
		fmt.Sprintf("D:P(A;;0x%08X;;;SY)", uint32(windows.MUTEX_ALL_ACCESS)))
	if err != nil {
		t.Fatalf("build the denying descriptor: %v", err)
	}
	attrs := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatalf("encode the test singleton name: %v", err)
	}
	handle, err := windows.CreateMutex(attrs, false, namePtr)
	if handle == 0 {
		if err == nil {
			err = errors.New("the operating system returned no handle")
		}
		t.Fatalf("create the denying fixture object: %v", err)
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		t.Fatal("the fixture object already exists: the test would deny a real object")
	}
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
}
