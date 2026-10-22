// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// winErrorSharingViolation is the Windows error for a file that another
// handle still holds open without delete sharing.
const winErrorSharingViolation = syscall.Errno(32)

func tryLockCoordinationFileForCleanup(file *os.File) (func(), bool, error) {
	var overlapped syscall.Overlapped
	overlapped.OffsetHigh = lockRegionOffsetHigh
	flags := uint32(lockFileFailImmediate | lockFileExclusive)
	ok, _, callErr := lockFileExProc.Call(file.Fd(), uintptr(flags), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if ok == 0 {
		if callErr == winErrorLockViolation {
			return nil, false, nil
		}
		return nil, false, callErr
	}
	return func() {
		var unlock syscall.Overlapped
		unlock.OffsetHigh = lockRegionOffsetHigh
		_, _, _ = unlockFileExProc.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&unlock)))
	}, true, nil
}

// removeTestCoordinationLeaseEntry removes one tracked lease entry.
//
// Windows cannot delete a file that an open handle holds without delete
// sharing, and the standard file API does not request delete sharing, so the
// helper's own handle is released before the removal. When another actor
// still holds the entry open, the removal fails with a sharing violation:
// that entry is inert while the actor runs, and it is left in place instead
// of failing the test.
func removeTestCoordinationLeaseEntry(path string, held *os.File) error {
	_ = held.Close()
	err := os.Remove(path)
	if err == nil {
		return nil
	}
	if isTestCoordinationSharingViolation(err) {
		return fmt.Errorf("%w: %v", errTestCoordinationLeaseStillHeld, err)
	}
	return err
}

// isTestCoordinationSharingViolation reports whether a removal failed
// because another handle still holds the file open.
func isTestCoordinationSharingViolation(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == winErrorSharingViolation || errno == winErrorLockViolation
}
