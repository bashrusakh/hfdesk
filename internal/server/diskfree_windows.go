//go:build windows

// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// diskFreeBytes returns available bytes for the filesystem containing path.
// If path does not exist, it walks up to the nearest existing ancestor.
func diskFreeBytes(path string) (free, total uint64, err error) {
	// Walk up until we find an existing path (handles non-existent dirs)
	p := path
	for {
		if _, err := os.Stat(p); err == nil {
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			break // at root
		}
		p = parent
	}

	pathPtr, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return 0, 0, err
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	r, _, e := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFreeBytes)),
	)
	if r == 0 {
		return 0, 0, e
	}
	return freeBytesAvailable, totalBytes, nil
}
