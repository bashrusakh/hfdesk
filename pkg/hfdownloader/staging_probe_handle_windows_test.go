// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

// Use production-equivalent Windows handles: shared read/write access for a
// status probe and deny-all sharing for the lifetime writer.

import (
	"syscall"
)

func openStagingProbeHandle(path string, mode stagingProbeHoldMode) (stagingProbeHandle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return stagingProbeHandle{}, err
	}
	shareMode := uint32(syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE)
	access := uint32(syscall.GENERIC_READ)
	if mode == probeHoldExclusive {
		// dwShareMode=0: the deny-all handle the owner holds.
		shareMode = 0
		access = syscall.GENERIC_READ | syscall.GENERIC_WRITE
	}
	h, err := syscall.CreateFile(
		p,
		access,
		shareMode,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return stagingProbeHandle{}, err
	}
	return stagingProbeHandle{
		releaseFn: func() { _ = syscall.CloseHandle(h) },
		heldFn: func() bool {
			probe, perr := syscall.CreateFile(
				p,
				syscall.GENERIC_READ|syscall.GENERIC_WRITE,
				0,
				nil,
				syscall.OPEN_EXISTING,
				syscall.FILE_ATTRIBUTE_NORMAL,
				0,
			)
			if perr != nil {
				return true
			}
			_ = syscall.CloseHandle(probe)
			return false
		},
	}, nil
}
