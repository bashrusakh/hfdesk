// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPrepareStagingPayloadSizesStagingPayload covers the portable part of the
// staging preallocation: the helper leaves a payload of exactly the requested
// length, and a write inside that length does not grow it. Whether the file is
// sparse is asserted where the allocation size is observable.
func TestPrepareStagingPayloadSizesStagingPayload(t *testing.T) {
	const size = 8 << 20

	path := filepath.Join(t.TempDir(), "payload.incomplete")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("create staging payload: %v", err)
	}
	defer f.Close()

	if err := prepareStagingPayload(f, size); err != nil {
		t.Fatalf("prepare staging payload: %v", err)
	}
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat staging payload: %v", err)
	}
	if fi.Size() != size {
		t.Fatalf("staging payload size = %d, want %d", fi.Size(), size)
	}

	if _, err := f.WriteAt([]byte{0x5a}, size-1); err != nil {
		t.Fatalf("write the last payload byte: %v", err)
	}
	fi, err = f.Stat()
	if err != nil {
		t.Fatalf("stat staging payload after write: %v", err)
	}
	if fi.Size() != size {
		t.Fatalf("staging payload size after write = %d, want %d", fi.Size(), size)
	}
}
