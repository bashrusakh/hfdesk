// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCloseQuiescesAndClosesTheJobManager proves that the close seam
// releases the job manager the way the graceful shutdown does: the manager is
// quiesced first, so nothing is dispatched anymore, and it is closed
// afterwards, so it commits its final state.
func TestCloseQuiescesAndClosesTheJobManager(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "jobs_state.json")
	manager := newJobManagerWithStatePath(Config{CacheDir: t.TempDir()}, nil, statePath)
	registerTestJobManagerCleanup(t, manager)
	s := &Server{jobs: manager}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	manager.mu.RLock()
	fenced := manager.fenced
	stopping := manager.stopping
	manager.mu.RUnlock()
	if !fenced {
		t.Error("Close did not quiesce the job manager")
	}
	if !stopping {
		t.Error("Close did not stop the job manager")
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("the closed job manager did not commit its final state: %v", err)
	}
}

// TestCloseIsSafeToCallTwice proves that the seam can be called again: the
// job manager runs its close once, so the second call waits for the same
// result instead of closing it a second time.
func TestCloseIsSafeToCallTwice(t *testing.T) {
	manager := newTestJobManager(t, Config{CacheDir: t.TempDir()}, nil)
	s := &Server{jobs: manager}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestCloseWithoutAJobManager proves that the seam is harmless for a server
// that never built a job manager.
func TestCloseWithoutAJobManager(t *testing.T) {
	s := &Server{}
	if err := s.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
}
