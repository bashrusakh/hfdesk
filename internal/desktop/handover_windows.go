// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const handoverEventAccess = windows.SYNCHRONIZE | windows.EVENT_MODIFY_STATE

const handoverEventPrefix = `Local\HFDesk.ServiceHandover.`

type handoverEvents struct {
	readyName   string
	successName string
	ready       windows.Handle
	success     windows.Handle
	closeOnce   sync.Once
}

type desktopHandover struct {
	events *handoverEvents
	helper *elevatedProcess
}

type desktopShutdownCoordinator struct {
	mu        sync.Mutex
	started   bool
	committed bool
	handover  *desktopHandover
}

func (c *desktopShutdownCoordinator) commit(handover *desktopHandover) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.committed || handover == nil {
		return false
	}
	c.committed = true
	c.handover = handover
	return true
}

func (c *desktopShutdownCoordinator) beginShutdown() (*desktopHandover, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = true
	return c.handover, c.committed
}

func newHandoverEvents() (*handoverEvents, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("desktop: create a unique service handover name: %w", err)
	}
	base := handoverEventPrefix + hex.EncodeToString(nonce[:])
	readyName, successName := base+".Ready", base+".Success"
	readyAttrs, readyDescriptor, err := restrictedObjectSecurityFor(
		windows.SYNCHRONIZE, windows.EVENT_MODIFY_STATE, handoverEventAccess)
	if err != nil {
		return nil, fmt.Errorf("desktop: secure the service handover ready event: %w", err)
	}
	ready, err := createHandoverEvent(readyName, readyAttrs)
	runtime.KeepAlive(readyDescriptor)
	if err != nil {
		return nil, err
	}
	successAttrs, successDescriptor, err := restrictedObjectSecurityFor(
		windows.EVENT_MODIFY_STATE, windows.SYNCHRONIZE, handoverEventAccess)
	if err != nil {
		_ = windows.CloseHandle(ready)
		return nil, fmt.Errorf("desktop: secure the service handover success event: %w", err)
	}
	success, err := createHandoverEvent(successName, successAttrs)
	runtime.KeepAlive(successDescriptor)
	if err != nil {
		_ = windows.CloseHandle(ready)
		return nil, err
	}
	return &handoverEvents{
		readyName: readyName, successName: successName,
		ready: ready, success: success,
	}, nil
}

func createHandoverEvent(name string, attrs *windows.SecurityAttributes) (windows.Handle, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, fmt.Errorf("desktop: encode the service handover event name: %w", err)
	}
	handle, err := windows.CreateEvent(attrs, 1, 0, namePtr)
	runtime.KeepAlive(attrs)
	if handle == 0 {
		if err == nil {
			err = errors.New("the operating system returned no handle")
		}
		return 0, fmt.Errorf("desktop: create service handover event %q: %w", name, err)
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("desktop: service handover event %q already exists", name)
	}
	if err != nil {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("desktop: create service handover event %q: %w", name, err)
	}
	return handle, nil
}

func restrictedObjectSecurity(userAccess, adminAccess uint32) (*windows.SecurityAttributes, *windows.SECURITY_DESCRIPTOR, error) {
	return restrictedObjectSecurityFor(userAccess, adminAccess, adminAccess)
}

func restrictedObjectSecurityFor(userAccess, adminAccess, systemAccess uint32) (*windows.SecurityAttributes, *windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, nil, err
	}
	sddl := fmt.Sprintf("D:P(A;;0x%08X;;;%s)(A;;0x%08X;;;BA)(A;;0x%08X;;;SY)",
		userAccess, sid, adminAccess, systemAccess)
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, nil, fmt.Errorf("build the restricted security descriptor: %w", err)
	}
	attrs := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	return attrs, descriptor, nil
}

func (h *handoverEvents) close() {
	if h == nil {
		return
	}
	h.closeOnce.Do(func() {
		if h.ready != 0 {
			_ = windows.CloseHandle(h.ready)
			h.ready = 0
		}
		if h.success != 0 {
			_ = windows.CloseHandle(h.success)
			h.success = 0
		}
	})
}

func (h *desktopHandover) close() {
	if h == nil {
		return
	}
	h.events.close()
	h.helper.Close()
}

func waitForHelperReady(helper, ready windows.Handle, budget time.Duration) error {
	result, err := windows.WaitForMultipleObjects(
		[]windows.Handle{helper, ready}, false, waitMilliseconds(budget))
	if err != nil {
		return fmt.Errorf("desktop: wait for the service helper to become ready: %w", err)
	}
	switch result {
	case windows.WAIT_OBJECT_0:
		return errors.New("desktop: the service helper exited before it became ready")
	case windows.WAIT_OBJECT_0 + 1:
		result, err := windows.WaitForSingleObject(helper, 0)
		if err != nil {
			return fmt.Errorf("desktop: verify the service helper is ready: %w", err)
		}
		if result != uint32(windows.WAIT_TIMEOUT) {
			return fmt.Errorf("desktop: the service helper was not running after its ready signal (wait result %#x)", result)
		}
		return nil
	case uint32(windows.WAIT_TIMEOUT):
		return fmt.Errorf("desktop: the service helper did not become ready within %s", budget)
	default:
		return fmt.Errorf("desktop: waiting for the service helper ended with result %#x", result)
	}
}

// waitForHandoverAuthorization requires both evidences inside two independent
// bounded machine windows, so waiting for completion can never zero the exit
// window and no user reading time sits on this path.
func waitForHandoverAuthorization(parent, success windows.Handle, successBudget, parentExitBudget time.Duration) error {
	result, err := windows.WaitForMultipleObjects(
		[]windows.Handle{parent, success}, false, waitMilliseconds(successBudget))
	if err != nil {
		return fmt.Errorf("desktop: wait for successful shutdown: %w", err)
	}
	switch result {
	case windows.WAIT_OBJECT_0:
		state, err := windows.WaitForSingleObject(success, 0)
		if err != nil {
			return fmt.Errorf("desktop: read the service handover result: %w", err)
		}
		if state != windows.WAIT_OBJECT_0 {
			return errors.New("desktop: the desktop exited without confirming successful graceful completion")
		}
		return nil
	case windows.WAIT_OBJECT_0 + 1:
		// Completion is confirmed; the exit evidence gets its own window.
	case uint32(windows.WAIT_TIMEOUT):
		return fmt.Errorf("desktop: successful shutdown was not confirmed within %s", successBudget)
	default:
		return fmt.Errorf("desktop: waiting for successful shutdown ended with result %#x", result)
	}

	result, err = windows.WaitForSingleObject(parent, waitMilliseconds(parentExitBudget))
	if err != nil {
		return fmt.Errorf("desktop: wait for the original desktop process to exit: %w", err)
	}
	switch result {
	case windows.WAIT_OBJECT_0:
		return nil
	case uint32(windows.WAIT_TIMEOUT):
		return fmt.Errorf("desktop: the original desktop process did not exit within %s after successful shutdown", parentExitBudget)
	default:
		return fmt.Errorf("desktop: waiting for the original desktop process ended with result %#x", result)
	}
}

func waitMilliseconds(budget time.Duration) uint32 {
	if budget <= 0 {
		return 0
	}
	milliseconds := uint64((budget + time.Millisecond - 1) / time.Millisecond)
	if milliseconds >= uint64(^uint32(0)) {
		return ^uint32(0) - 1
	}
	return uint32(milliseconds)
}

func authorizeHandoverCompletion(waitForCompletion func() error, helper windows.Handle, success windows.Handle) error {
	if err := waitForCompletion(); err != nil {
		return err
	}
	result, err := windows.WaitForSingleObject(helper, 0)
	if err != nil {
		return fmt.Errorf("desktop: verify the service helper is still waiting: %w", err)
	}
	if result != uint32(windows.WAIT_TIMEOUT) {
		return fmt.Errorf("desktop: the service helper exited before graceful completion was confirmed (wait result %#x)", result)
	}
	if err := windows.SetEvent(success); err != nil {
		return fmt.Errorf("desktop: signal successful graceful completion to the service helper: %w", err)
	}
	return nil
}

// completeHandoverWithNotice shows the native waiting state and then joins the
// completion evidence. The notice runs on its own goroutine and is never
// joined: a modal box must not hold persistence, the success signal, or the
// desktop exit it describes.
func completeHandoverWithNotice(handover *desktopHandover, waitForCompletion func() error) error {
	go showMessage("HFDesk - finishing service installation",
		"HFDesk is shutting down and finishing its graceful save before the service can start.\n\n"+
			"The service starts only if the server and its saved state both complete successfully.",
		windows.MB_ICONINFORMATION)
	return authorizeHandoverCompletion(waitForCompletion, handover.helper.handle, handover.events.success)
}

func stopDesktopServer(shutdown func(), coordinator *desktopShutdownCoordinator) {
	_, committed := coordinator.beginShutdown()
	if committed {
		shutdown()
		return
	}
	stopServer(shutdown)
}

type helperHandover struct {
	parent  windows.Handle
	ready   windows.Handle
	success windows.Handle
	mutex   windows.Handle
	// hold releases the singleton of the service layer for an action that
	// has no desktop handover wait: the gate's update and start, and a
	// hand-typed helper command line. It outlives the authorization
	// handles, through the service mutation, and ends with releaseSingleton.
	hold    func()
	release sync.Once
}

var prepareHelperHandover = openHelperHandover

func openHelperHandover(o ServiceActionOptions) (*helperHandover, error) {
	if o.WaitPID <= 0 || strings.TrimSpace(o.ReadyEvent) == "" ||
		strings.TrimSpace(o.SuccessEvent) == "" || strings.TrimSpace(o.InstanceMutex) == "" {
		return nil, errors.New("desktop: incomplete service handover arguments")
	}
	h := &helperHandover{}
	var err error
	h.parent, err = windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(o.WaitPID))
	if err != nil || h.parent == 0 {
		if err == nil {
			err = errors.New("the operating system returned no process handle")
		}
		return nil, fmt.Errorf("desktop: open the original desktop process %d: %w", o.WaitPID, err)
	}
	if h.ready, err = openNamedEvent(o.ReadyEvent, windows.EVENT_MODIFY_STATE); err != nil {
		h.close()
		return nil, err
	}
	if h.success, err = openNamedEvent(o.SuccessEvent, windows.SYNCHRONIZE); err != nil {
		h.close()
		return nil, err
	}
	h.mutex, err = openDesktopSingleton(o.InstanceMutex)
	if err != nil {
		h.close()
		return nil, err
	}
	return h, nil
}

func openDesktopSingleton(name string) (windows.Handle, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, fmt.Errorf("desktop: encode the desktop singleton name: %w", err)
	}
	handle, err := windows.OpenMutex(windows.READ_CONTROL, false, namePtr)
	if err != nil || handle == 0 {
		if err == nil {
			err = errors.New("the operating system returned no mutex handle")
		}
		return 0, fmt.Errorf("desktop: hold the existing desktop singleton during service installation: %w", err)
	}
	return handle, nil
}

func openNamedEvent(name string, access uint32) (windows.Handle, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, fmt.Errorf("desktop: encode the service handover event name: %w", err)
	}
	handle, err := windows.OpenEvent(access, false, namePtr)
	if err != nil || handle == 0 {
		if err == nil {
			err = errors.New("the operating system returned no event handle")
		}
		return 0, fmt.Errorf("desktop: open service handover event %q: %w", name, err)
	}
	return handle, nil
}

func (h *helperHandover) signalReady() error {
	if h == nil || h.ready == 0 {
		return errors.New("desktop: the service handover ready event is unavailable")
	}
	if err := windows.SetEvent(h.ready); err != nil {
		return fmt.Errorf("desktop: signal that the service helper is ready: %w", err)
	}
	return nil
}

func (h *helperHandover) waitForAuthorization(successBudget, parentExitBudget time.Duration) error {
	if h == nil || h.parent == 0 || h.success == 0 {
		return errors.New("desktop: the service handover authorization handles are unavailable")
	}
	return waitForHandoverAuthorization(h.parent, h.success, successBudget, parentExitBudget)
}

func (h *helperHandover) releaseSingleton() {
	if h == nil {
		return
	}
	h.release.Do(func() {
		if h.mutex != 0 {
			_ = windows.CloseHandle(h.mutex)
			h.mutex = 0
		}
		if h.hold != nil {
			h.hold()
			h.hold = nil
		}
	})
}

// releaseAuthorizationHandles drops the evidence handles as soon as the
// authorization outcome is known; only the singleton outlives them, through
// the service mutation, and it is released before the final notice.
func (h *helperHandover) releaseAuthorizationHandles() {
	if h == nil {
		return
	}
	for _, handle := range []*windows.Handle{&h.parent, &h.ready, &h.success} {
		if *handle != 0 {
			_ = windows.CloseHandle(*handle)
			*handle = 0
		}
	}
}

func (h *helperHandover) close() {
	if h == nil {
		return
	}
	h.releaseSingleton()
	h.releaseAuthorizationHandles()
}
