// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !solaris && !aix

package hfdownloader

// Unix flock ownership belongs to an open file description, so separate opens
// conflict even in one process. Close/process exit releases the lock.
// Only writers take LOCK_EX; observations use LOCK_SH so probes coexist.
// A refused shared probe proves an exclusive holder; a refused exclusive
// acquisition alone may mean either a writer or a temporary shared probe.

import (
	"errors"
	"os"
	"syscall"
)

// stagingOSLockAvailable means this build implements an OS ownership primitive;
// individual filesystems may still refuse it.
const stagingOSLockAvailable = true

// stagingOwnershipProbeFlock is the status probe's flock operation. It is a
// narrow seam for exercising unsupported-kernel/filesystem errors without
// depending on a particular mounted filesystem in tests.
var stagingOwnershipProbeFlock = syscall.Flock

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

// probeStagingOSLock opens an existing file read-only and briefly takes
// LOCK_SH|LOCK_NB. Probes coexist; refusal identifies an exclusive writer.
// A missing file is not busy, and the probe creates nothing.
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
