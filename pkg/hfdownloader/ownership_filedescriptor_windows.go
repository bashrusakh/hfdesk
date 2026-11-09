// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"errors"

	"golang.org/x/sys/windows"
)

// currentOwnershipFileLimit reports no soft/hard model: Windows has no
// NOFILE-style per-process soft limit to raise (the handle quota grows with
// demand and is far above the tree sizes in question), so the tree-admission
// limit hold is a no-op on this backend.
func currentOwnershipFileLimit() (soft, hard uint64, ok bool) { return 0, 0, false }

// setOwnershipSoftFileLimit has nothing to set on the Windows backend.
func setOwnershipSoftFileLimit(uint64) error { return nil }

// ownershipFileDescriptorPressure identifies handle exhaustion on Windows so
// a raw quota error never surfaces from tree admission either.
func ownershipFileDescriptorPressure(err error) bool {
	return errors.Is(err, windows.ERROR_TOO_MANY_OPEN_FILES) ||
		errors.Is(err, windows.ERROR_NO_SYSTEM_RESOURCES)
}
