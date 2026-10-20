// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !solaris && !aix

package hfdownloader

// Unix ownership lock: flock(2) on an open file description. LOCK_EX|LOCK_NB
// is non-blocking; EWOULDBLOCK means a live owner. The lock is owned by the
// open file description, so two separate opens of the same path in one process
// also conflict (the in-process registry makes that a defensive property, not
// the primary exclusion). Closing the descriptor releases the lock, including
// when the process dies.

import (
	"errors"
	"os"
	"syscall"
)

// stagingOSLockAvailable reports that this platform implements the OS
// ownership lock (used by tests and documentation).
const stagingOSLockAvailable = true

// stagingOwnershipProbeFlock is the status probe's flock operation. It is a
// narrow seam for exercising unsupported-kernel/filesystem errors without
// depending on a particular mounted filesystem in tests.
var stagingOwnershipProbeFlock = syscall.Flock

// stagingOSLockHandle is one held flock descriptor.
type stagingOSLockHandle struct {
	f *os.File
}

// openStagingOSLock creates/opens the persistent lock file and takes the
// exclusive lock without waiting.
func openStagingOSLock(path string) (stagingOSLockHandle, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return stagingOSLockHandle{}, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		switch {
		case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
			return stagingOSLockHandle{}, errStagingOwnershipBusy
		case errors.Is(err, syscall.ENOLCK), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOSYS):
			return stagingOSLockHandle{}, errStagingOSLockUnsupported
		}
		return stagingOSLockHandle{}, err
	}
	return stagingOSLockHandle{f: f}, nil
}

// release drops the lock and closes the descriptor. The lock file itself is
// deliberately left in place (see staging_oslock.go).
func (h stagingOSLockHandle) release() {
	if h.f == nil {
		return
	}
	_ = syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN)
	_ = h.f.Close()
}

// probeStagingOSLock reports whether the lock file exists and is held by a
// writer. It takes a NON-OWNING shared lock (LOCK_SH|LOCK_NB): shared locks
// coexist with each other, so concurrent probes never see each other as an
// owner, while an exclusive writer's LOCK_EX makes the shared acquisition fail.
// The lock is released immediately. It opens read-only without creating
// anything and never acts as a writer itself.
func probeStagingOSLock(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	if err := stagingOwnershipProbeFlock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		switch {
		case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
			return true, nil
		}
		return false, err
	}
	_ = stagingOwnershipProbeFlock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}
