// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const (
	lockFileExclusive     = 0x00000002
	lockFileFailImmediate = 0x00000001
	winErrorLockViolation = syscall.Errno(33)
	// LockFileEx permits a byte range beyond EOF. Private lock files stay empty;
	// the offset therefore has no conflict with application metadata.
	lockRegionOffsetHigh = 1
)

var (
	lockFileExProc   = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	unlockFileExProc = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
)

func acquireBranchVariantOSFileLock(ctx context.Context, file *os.File, exclusive bool, onWait func()) (func(), error) {
	flags := uint32(lockFileFailImmediate)
	if exclusive {
		flags |= lockFileExclusive
	}
	contentionReported := false
	for {
		var overlapped syscall.Overlapped
		overlapped.OffsetHigh = lockRegionOffsetHigh
		ok, _, callErr := lockFileExProc.Call(
			file.Fd(), uintptr(flags), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)),
		)
		if ok != 0 {
			return func() {
				var unlock syscall.Overlapped
				unlock.OffsetHigh = lockRegionOffsetHigh
				_, _, _ = unlockFileExProc.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&unlock)))
				_ = file.Close()
			}, nil
		}
		if !errors.Is(callErr, winErrorLockViolation) {
			return nil, callErr
		}
		if !contentionReported && onWait != nil {
			contentionReported = true
			onWait()
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
