// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"path/filepath"
	"testing"
	"time"
)

// TestFullySkippedJobEndsCompleted pins the job contract for a run whose
// requested files are ALL already present in the cache: every item takes a
// real completed skip (the blob is complete and its links reachable), so the
// job must end presented exactly like a job that transferred everything —
// terminal completed state with a consistent finished progress shape.
func TestFullySkippedJobEndsCompleted(t *testing.T) {
	base := isolateCacheState(t)
	r, h := filepath.Join(base, "friendly"), filepath.Join(base, "hub")
	t.Setenv("HF_HUB_CACHE", h)
	ep, _, _, _ := cacheDownloadFixture(t, "")
	mgr := newLifecycleTestManager(t, Config{CacheDir: r, MaxActive: 1, Concurrency: 1, Verify: "sha256", Endpoint: ep.URL})

	first, _, err := mgr.CreateJob(DownloadRequest{Repo: "owner/model"})
	assertNoError(t, err, "")
	waitJobStatus(t, mgr, first.ID, JobStatusCompleted, 10*time.Second)
	mgr.WaitAll(10 * time.Second)

	// The cache now holds every requested file; this run skips all of them.
	second, _, err := mgr.CreateJob(DownloadRequest{Repo: "owner/model"})
	assertNoError(t, err, "")
	terminal := waitJobStatus(t, mgr, second.ID, JobStatusCompleted, 10*time.Second)
	mgr.WaitAll(10 * time.Second)
	t.Logf("terminal status=%q phase=%q progress=%+v files=%+v",
		terminal.Status, terminal.Phase, terminal.Progress, terminal.Files)

	if terminal.Status != JobStatusCompleted {
		t.Fatalf("fully-skipped job status = %q (err %q), want completed",
			terminal.Status, terminal.Error)
	}
	if terminal.Progress.TotalFiles == 0 || terminal.Progress.CompletedFiles != terminal.Progress.TotalFiles {
		t.Fatalf("fully-skipped job completed files = %d/%d, want all files complete",
			terminal.Progress.CompletedFiles, terminal.Progress.TotalFiles)
	}
	if terminal.Progress.DownloadedBytes != terminal.Progress.TotalBytes {
		t.Fatalf("fully-skipped job bytes = %d/%d, want the completed shape DownloadedBytes == TotalBytes",
			terminal.Progress.DownloadedBytes, terminal.Progress.TotalBytes)
	}
	for _, f := range terminal.Files {
		if f.Status != "complete" {
			t.Fatalf("fully-skipped job file %s status = %q, want complete", f.Path, f.Status)
		}
	}
}
