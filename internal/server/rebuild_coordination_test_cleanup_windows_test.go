// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package server

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	testLockFileExProc   = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	testUnlockFileExProc = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
)

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
