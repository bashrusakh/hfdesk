// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly || illumos || windows

package hfdownloader

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBranchVariantDirectoryLeaseContextBoundsACompetingWait proves the
// cancellable lease form gives up instead of waiting for the whole duration
// of a competing exclusive lease. A reader that competes with a long-running
// artifact mutation must be able to answer "not proven" promptly.
func TestBranchVariantDirectoryLeaseContextBoundsACompetingWait(t *testing.T) {
	target := filepath.Join(t.TempDir(), "artifact")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	exclusive, err := AcquireBranchVariantDirectoryLeaseContext(context.Background(), target, true)
	if err != nil {
		t.Fatalf("acquire exclusive directory lease: %v", err)
	}
	defer exclusive()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	release, err := AcquireBranchVariantDirectoryLeaseContext(ctx, target, false)
	if err == nil {
		release()
		t.Fatal("shared directory lease was granted while an exclusive lease was held")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("cancellable directory lease wait took %s", elapsed)
	}
}

// TestBranchVariantDirectoryLeaseAcceptsANilContext proves the lease
// primitives tolerate a nil context. A nil context means "no deadline", not a
// panic, on every platform that implements directory leases.
func TestBranchVariantDirectoryLeaseAcceptsANilContext(t *testing.T) {
	target := filepath.Join(t.TempDir(), "artifact")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := AcquireBranchVariantDirectoryLeaseContext(nil, target, false)
	if err != nil {
		t.Fatalf("acquire shared directory lease with a nil context: %v", err)
	}
	release()
}
