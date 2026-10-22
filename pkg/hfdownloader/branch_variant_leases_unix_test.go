// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly || illumos

package hfdownloader

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestBranchVariantDirectoryLeaseNilContextSurvivesContention proves a
// nil-context lease acquisition that has to wait for a competing exclusive
// lease keeps polling instead of dereferencing a nil context. The nil context
// means "no deadline", so the waiter must still succeed once the exclusive
// lease is released.
func TestBranchVariantDirectoryLeaseNilContextSurvivesContention(t *testing.T) {
	target := filepath.Join(t.TempDir(), "artifact")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	exclusive, err := AcquireBranchVariantDirectoryLeaseContext(context.Background(), target, true)
	if err != nil {
		t.Fatalf("acquire exclusive directory lease: %v", err)
	}
	var releaseOnce sync.Once
	releaseExclusive := func() { releaseOnce.Do(func() { exclusive() }) }
	defer releaseExclusive()

	contended := make(chan struct{}, 1)
	previousHook := branchVariantLeaseContentionHook
	branchVariantLeaseContentionHook = func(string) {
		select {
		case contended <- struct{}{}:
		default:
		}
	}
	defer func() { branchVariantLeaseContentionHook = previousHook }()

	acquired := make(chan error, 1)
	go func() {
		release, err := AcquireBranchVariantDirectoryLeaseContext(nil, target, false)
		if err == nil {
			release()
		}
		acquired <- err
	}()
	select {
	case <-contended:
	case err := <-acquired:
		t.Fatalf("shared lease finished before it contended: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("shared lease never reached the contended retry loop")
	}
	releaseExclusive()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("shared lease with a nil context failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shared lease with a nil context did not complete after the exclusive lease was released")
	}
}
