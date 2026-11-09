// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/bashrusakh/hfdesk/internal/service"
	"golang.org/x/sys/windows"
)

// instanceLock is the named mutex that keeps a single desktop instance.
//
// The mutex is a named kernel object: the operating system removes it when
// the last handle is closed, so the lock of a process that died cannot
// survive the process. A state file cannot offer that, because a state file
// of a crashed process stays behind and would keep a new instance from
// starting.
type instanceLock struct {
	// handle is the handle of the named mutex.
	handle windows.Handle
	// name lets the elevated helper hold this exact per-session object.
	name string
	// existing reports that the mutex was already there, which means that
	// another instance of the desktop host is running.
	existing bool
}

// acquireInstanceLock takes the single instance lock.
//
// The name of the mutex carries the security identifier of the user and the
// id of the session, so every interactive user session gets its own
// instance.
func acquireInstanceLock() (*instanceLock, error) {
	name, err := instanceMutexName()
	if err != nil {
		return nil, err
	}
	ptr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("desktop: build the name of the single instance lock: %w", err)
	}
	attrs, descriptor, err := restrictedObjectSecurity(windows.MUTEX_ALL_ACCESS, windows.READ_CONTROL)
	if err != nil {
		return nil, fmt.Errorf("desktop: secure the single instance lock: %w", err)
	}

	// The mutex is created without an owner: the existence of the named
	// object is the lock. The operating system destroys the object when
	// the last handle is closed, which happens when the process ends, also
	// when it ends unexpectedly.
	handle, err := windows.CreateMutex(attrs, false, ptr)
	runtime.KeepAlive(descriptor)
	if handle == 0 {
		if err == nil {
			err = errors.New("the operating system returned no handle")
		}
		return nil, fmt.Errorf("desktop: create the single instance lock %q: %w", name, err)
	}
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("desktop: create the single instance lock %q: %w", name, err)
	}
	return &instanceLock{
		handle:   handle,
		name:     name,
		existing: errors.Is(err, windows.ERROR_ALREADY_EXISTS),
	}, nil
}

// instanceMutexName returns the name of the single instance mutex.
//
// The name is computed by the service layer: the elevated helper and the
// service command hold this very object across their mutations, and one
// computation of the user, the session and the shape keeps the desktop host
// and those holders from naming different objects.
func instanceMutexName() (string, error) {
	return service.DesktopSingletonName()
}

// instanceLockStartupText returns the report of a launch that could not take
// its single instance lock.
//
// The ordinary failure names the lock that could not be created. A refusal
// because the identity of this launch has no access to the object is a
// different fact: the object exists and another Windows identity created it,
// so the launch starts no server — the safe direction — and the notice says
// which process holds the object instead of reading like an internal failure.
// It never claims a running instance the way the notice of a second launch
// over the same object does: this launch could not even reach that object's
// owner check.
func instanceLockStartupText(err error) string {
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return "HFDesk found its single instance object, but this Windows identity cannot access it.\n\n" +
			"Another HFDesk process running under a different Windows account created it — for example an elevated helper that another administrator approved — so this launch starts no server: two instances never compete for the same address.\n\n" +
			"End that other HFDesk process (Task Manager lists it) and start HFDesk again."
	}
	return fmt.Sprintf("HFDesk could not create its single instance lock.\n\n%v", err)
}

// currentUserSID returns the security identifier of the user of the process.
func currentUserSID() (string, error) {
	// The pseudo token of the current process needs no closing.
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("desktop: read the user of the process: %w", err)
	}
	return user.User.Sid.String(), nil
}

// Exists reports that another instance of the desktop host holds the lock.
func (l *instanceLock) Exists() bool {
	return l != nil && l.existing
}

// Release closes the handle of the mutex, which releases the lock.
func (l *instanceLock) Release() {
	if l == nil || l.handle == 0 {
		return
	}
	_ = windows.CloseHandle(l.handle)
	l.handle = 0
}
