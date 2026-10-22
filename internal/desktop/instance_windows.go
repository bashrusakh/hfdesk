// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// instanceMutexPrefix is the prefix of the name of the single instance
// mutex.
//
// The Local prefix keeps the object inside the namespace of the session, so
// a second session of the same user does not see it.
const instanceMutexPrefix = `Local\HFDesk.Desktop.`

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

	// The mutex is created without an owner: the existence of the named
	// object is the lock. The operating system destroys the object when
	// the last handle is closed, which happens when the process ends, also
	// when it ends unexpectedly.
	handle, err := windows.CreateMutex(nil, false, ptr)
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
		existing: errors.Is(err, windows.ERROR_ALREADY_EXISTS),
	}, nil
}

// instanceMutexName returns the name of the single instance mutex.
//
// The name is scoped to the interactive user and to the session: the
// security identifier of the user and the id of the session of the process.
func instanceMutexName() (string, error) {
	sid, err := currentUserSID()
	if err != nil {
		return "", err
	}
	session, err := currentSessionID()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%s.%d", instanceMutexPrefix, sid, session), nil
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

// currentSessionID returns the id of the session of the process.
func currentSessionID() (uint32, error) {
	var session uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &session); err != nil {
		return 0, fmt.Errorf("desktop: read the session of the process: %w", err)
	}
	return session, nil
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
