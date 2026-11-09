//go:build windows

// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"syscall"
	"unsafe"
)

var moveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

const moveFileReplaceExisting = 0x1

func renameStateFile(oldpath, newpath string) error {
	oldPtr, err := syscall.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	newPtr, err := syscall.UTF16PtrFromString(newpath)
	if err != nil {
		return err
	}
	// The temp file is created beside the target, so replacement stays on one
	// volume. WRITE_THROUGH is omitted: this commit path makes no power-loss
	// durability claim, and that flag is not needed for replacement semantics.
	result, _, callErr := moveFileEx.Call(
		uintptr(unsafe.Pointer(oldPtr)),
		uintptr(unsafe.Pointer(newPtr)),
		moveFileReplaceExisting,
	)
	if result == 0 {
		return callErr
	}
	return nil
}
