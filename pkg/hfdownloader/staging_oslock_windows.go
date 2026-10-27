// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

// Windows ownership uses a deny-all CreateFile handle (dwShareMode=0).
// Sharing violation signals a conflicting open. Close/process exit releases it;
// non-owning shared probes distinguish a writer from probe contention.

import (
	"errors"
	"fmt"
	"syscall"
)

const (
	// stagingOSLockAvailable means this build implements the OS ownership primitive.
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

// CreateFile checks access/share compatibility in both directions. Our shared
// read/write probes coexist, but a writer's deny-all handle refuses them.
// Never observe with dwShareMode=0: that would impersonate a writer and make
// peers wait or fail on ownership that does not exist.
