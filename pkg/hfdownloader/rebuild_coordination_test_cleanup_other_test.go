// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !illumos

package hfdownloader

import (
	"fmt"
	"os"
)

func tryLockCoordinationFileForCleanup(file *os.File) (func(), bool, error) {
	return nil, false, fmt.Errorf("cross-process HF rebuild coordination leases are unsupported on this platform")
}

// removeTestCoordinationLeaseEntry removes one tracked lease entry.
//
// An open handle does not prevent unlinking on this platform, so the entry is
// removed directly.
func removeTestCoordinationLeaseEntry(path string, held *os.File) error {
	return os.Remove(path)
}
