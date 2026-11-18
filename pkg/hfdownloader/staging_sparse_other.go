// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package hfdownloader

import "os"

// prepareStagingPayload sizes the staging payload. Unix ftruncate creates a
// sparse file: extending the file allocates no blocks, and writes into the
// resulting holes do not zero-fill them. The downloader uses only this
// truncate-to-size form, never an allocation-style preallocation.
func prepareStagingPayload(f *os.File, size int64) error {
	return f.Truncate(size)
}
