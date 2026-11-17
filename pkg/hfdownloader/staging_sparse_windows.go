// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"os"

	"golang.org/x/sys/windows"
)

// prepareStagingPayload sizes the staging payload without allocating its
// unwritten ranges. NTFS allocates the full range on SetEndOfFile and leaves
// the valid-data length at zero, so the first ranged write beyond that length
// zero-fills the gap at disk speed. FSCTL_SET_SPARSE makes the extension a
// hole instead, so the ranged workers reach their offsets immediately.
//
// A filesystem without sparse-file support refuses the control code; the
// payload is still sized and the download continues with the prior
// size-only behaviour.
func prepareStagingPayload(f *os.File, size int64) error {
	var returned uint32
	if err := windows.DeviceIoControl(
		windows.Handle(f.Fd()),
		windows.FSCTL_SET_SPARSE,
		nil, 0,
		nil, 0,
		&returned,
		nil,
	); err != nil {
		downloadLog.Warn("staging payload cannot be made sparse; preallocation may zero-fill unwritten ranges",
			"path", f.Name(),
			"error", err,
		)
	}
	return f.Truncate(size)
}
