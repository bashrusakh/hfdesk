// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

// Windows ownership lock: an exclusive-share CreateFile handle
// (dwShareMode=0). While the handle is open, every other open of the same path
// fails with ERROR_SHARING_VIOLATION, which is the ownership-busy signal. The
// handle is closed to release the lock; the kernel does so at process exit, so
// a crashed owner is recoverable. syscall (stdlib) is sufficient — no
// dependency is added.

import (
	"errors"
	"fmt"
	"syscall"
)

const (
	// stagingOSLockAvailable reports that this platform implements the OS
	// ownership lock.
	stagingOSLockAvailable = true

	// errorSharingViolation is Windows ERROR_SHARING_VIOLATION. syscall does
	// not export the symbolic constant, so it is declared here.
	errorSharingViolation = syscall.Errno(32)
	// errorLockViolation is Windows ERROR_LOCK_VIOLATION, returned by some
	// filesystems instead of a sharing violation.
	errorLockViolation   = syscall.Errno(33)
	errorNotSupported    = syscall.Errno(50)
	errorInvalidFunction = syscall.Errno(1)
)

// stagingOSLockHandle is one held exclusive-share file handle.
type stagingOSLockHandle struct {
	h syscall.Handle
}

// openStagingOSLock creates/opens the persistent lock file with no sharing and
// holds the handle as the ownership lock.
func openStagingOSLock(path string) (stagingOSLockHandle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return stagingOSLockHandle{}, errStagingOSLockUnsupported
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // dwShareMode=0: exclusive open, the ownership fence
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		switch {
		case errors.Is(err, errorSharingViolation), errors.Is(err, errorLockViolation):
			return stagingOSLockHandle{}, errStagingOwnershipBusy
		}
		switch {
		case errors.Is(err, errorNotSupported), errors.Is(err, errorInvalidFunction):
			return stagingOSLockHandle{}, fmt.Errorf("%w: %v", errStagingOSLockUnsupported, err)
		default:
			return stagingOSLockHandle{}, err
		}
	}
	return stagingOSLockHandle{h: h}, nil
}

// release closes the handle and drops the exclusive share.
func (h stagingOSLockHandle) release() {
	if h.h != 0 {
		_ = syscall.CloseHandle(h.h)
	}
}

// probeStagingOSLock reports whether the lock file exists and is held by a
// writer. It opens an existing file read-only with shared read/write access:
// the owner's exclusive-share (deny-all) handle makes that fail with a sharing
// violation, while a concurrent probe with the same shared access can proceed,
// so a probe is never mistaken for a writer. A missing file is not busy.
func probeStagingOSLock(path string) (bool, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errStagingOSLockUnsupported, err)
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		switch {
		case errors.Is(err, syscall.ERROR_FILE_NOT_FOUND), errors.Is(err, syscall.ERROR_PATH_NOT_FOUND):
			return false, nil
		case errors.Is(err, errorSharingViolation), errors.Is(err, errorLockViolation):
			return true, nil
		}
		return false, err
	}
	_ = syscall.CloseHandle(h)
	return false, nil
}

// observeStagingOSLockHolder is implemented once, in staging_oslock.go, on
// top of the shared non-owning status probe.
//
// On Windows the status probe is exactly the right primitive. A Windows open
// succeeds only when it is compatible with EVERY existing handle in both
// directions. The owner holds a deny-all handle (dwShareMode=0), so a
// shared-access open (FILE_SHARE_READ|FILE_SHARE_WRITE, GENERIC_READ) is:
//
//   - REFUSED by the deny-all owner, because the owner's share mode does not
//     permit the new access => an exclusive holder exists; and
//   - GRANTED when only a reader holds the file, because the reader's share
//     mode permits the new access.
//
// The observation must never open the file with dwShareMode=0. A deny-all
// observer is itself an exclusive holder, so every peer — a concurrent
// claimant or a concurrent status probe — would see a live writer that does
// not exist and would skip files nobody is downloading.
