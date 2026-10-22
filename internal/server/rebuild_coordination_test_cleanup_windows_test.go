// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package server

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	testLockFileExProc   = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	testUnlockFileExProc = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
)

// winErrorSharingViolation is the Windows error for a file that another
// handle still holds open without delete sharing.
const winErrorSharingViolation = syscall.Errno(32)

func tryLockServerCoordinationFile(file *os.File) (func(), bool, error) {
	var overlapped syscall.Overlapped
	overlapped.OffsetHigh = 1
	ok, _, callErr := testLockFileExProc.Call(file.Fd(), 0x00000003, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if ok == 0 {
		if callErr == syscall.Errno(33) {
			return nil, false, nil
		}
		return nil, false, callErr
	}
	return func() {
		var unlock syscall.Overlapped
		unlock.OffsetHigh = 1
		_, _, _ = testUnlockFileExProc.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&unlock)))
	}, true, nil
}

// removeServerCoordinationLeaseEntry removes one tracked lease entry.
//
// Windows cannot delete a file that an open handle holds without delete
// sharing, and the standard file API does not request delete sharing, so the
// helper's own handle is released before the removal. When another actor
// still holds the entry open, the removal fails with a sharing violation:
// that entry is inert while the actor runs, and it is left in place instead
// of failing the test.
func removeServerCoordinationLeaseEntry(path string, held *os.File) error {
	_ = held.Close()
	err := os.Remove(path)
	if err == nil {
		return nil
	}
	if isServerCoordinationSharingViolation(err) {
		return fmt.Errorf("%w: %v", errServerCoordinationLeaseStillHeld, err)
	}
	return err
}

// isServerCoordinationSharingViolation reports whether a removal failed
// because another handle still holds the file open.
func isServerCoordinationSharingViolation(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == winErrorSharingViolation || errno == syscall.Errno(33)
}
