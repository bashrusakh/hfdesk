// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileStandardInfo mirrors FILE_STANDARD_INFO. BOOLEAN fields stay one byte
// wide; the struct's trailing padding is irrelevant because the API writes
// only the declared fields.
type fileStandardInfo struct {
	AllocationSize int64
	EndOfFile      int64
	NumberOfLinks  uint32
	DeletePending  uint8
	Directory      uint8
}

// allocationSize reports the clusters NTFS reserved for f. A sparse file keeps
// this far below its end-of-file length until ranges are written.
func allocationSize(t *testing.T, f *os.File) (allocated, eof int64) {
	t.Helper()
	var info fileStandardInfo
	if err := windows.GetFileInformationByHandleEx(
		windows.Handle(f.Fd()),
		uint32(windows.FileStandardInfo),
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		t.Fatalf("GetFileInformationByHandleEx: %v", err)
	}
	return info.AllocationSize, info.EndOfFile
}

// TestPrepareStagingPayloadWindowsAllocation proves the sparse flag is what
// removes the allocation: SetEndOfFile alone reserves the payload, while the
// helper leaves it unallocated and a ranged write at a high offset touches
// only that range.
func TestPrepareStagingPayloadWindowsAllocation(t *testing.T) {
	const size = 256 << 20

	openStaging := func(t *testing.T) *os.File {
		t.Helper()
		path := filepath.Join(t.TempDir(), "payload.incomplete")
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			t.Fatalf("create staging payload: %v", err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}

	t.Run("size-only truncate reserves the payload", func(t *testing.T) {
		f := openStaging(t)
		if err := f.Truncate(size); err != nil {
			t.Fatalf("truncate staging payload: %v", err)
		}
		allocated, eof := allocationSize(t, f)
		if eof != size {
			t.Fatalf("end of file = %d, want %d", eof, size)
		}
		if allocated < size*9/10 {
			t.Fatalf("allocation = %d, want approximately the %d-byte end of file without the sparse flag", allocated, size)
		}
	})

	t.Run("sparse payload stays unallocated", func(t *testing.T) {
		f := openStaging(t)
		if err := prepareStagingPayload(f, size); err != nil {
			t.Fatalf("prepare staging payload: %v", err)
		}
		allocated, eof := allocationSize(t, f)
		if eof != size {
			t.Fatalf("end of file = %d, want %d", eof, size)
		}
		if allocated > size/8 {
			t.Fatalf("allocation = %d, want far below the %d-byte end of file", allocated, size)
		}

		const rangeSize = 1 << 20
		if _, err := f.WriteAt(bytes.Repeat([]byte{0x5a}, rangeSize), size-rangeSize); err != nil {
			t.Fatalf("write one range at a high offset: %v", err)
		}
		allocated, eof = allocationSize(t, f)
		if eof != size {
			t.Fatalf("end of file after write = %d, want %d", eof, size)
		}
		if allocated > size/8 {
			t.Fatalf("allocation after one %d-byte high-offset range = %d, want only that range, not the %d-byte file", rangeSize, allocated, size)
		}
	})
}
