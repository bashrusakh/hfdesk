// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"os"
	"syscall"
	"unsafe"
)

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
