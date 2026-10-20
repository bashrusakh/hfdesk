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
	errorLockViolation = syscall.Errno(33)
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
		// Access denied, a read-only lock file or an unsupported network
		// share: keep the download working with the lease fallback rather
		// than failing the file.
		return stagingOSLockHandle{}, errStagingOSLockUnsupported
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
		return false, nil
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
		return false, nil
	}
	_ = syscall.CloseHandle(h)
	return false, nil
}
