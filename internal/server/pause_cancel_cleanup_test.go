// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pausedJobWithPartFiles creates a paused job (the state the runJob
// goroutine leaves after a Pause) and seeds the cache with a realistic
// set of partial files in the repo's blobs directory. The job is
// inserted into the manager directly so the test does not have to drive
// a real download and then cancel it.
func pausedJobWithPartFiles(t *testing.T, mgr *JobManager, repo string) (*Job, string) {
	t.Helper()
	blobsDir := filepath.Join(mgr.config.CacheDir, "hub", "models--owner--"+repo, "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	partFiles := []string{
		"tmp-deadbeef00000000.part",
		"tmp-deadbeef00000000.part-00",
		"tmp-deadbeef00000000.part-01",
		"tmp-deadbeef00000000.parts.json",
	}
	for _, name := range partFiles {
		if err := os.WriteFile(filepath.Join(blobsDir, name), []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A "completed" blob that must survive any cleanup.
	if err := os.WriteFile(filepath.Join(blobsDir, "completed-blob"), []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	id := "paused-" + repo
	mgr.jobs[id] = &Job{
		ID:        id,
		Repo:      "owner/" + repo,
		Status:    JobStatusPaused,
		CreatedAt: time.Now(),
		OutputDir: mgr.config.CacheDir,
	}
	return mgr.jobs[id], blobsDir
}

// TestCancelJob_PausedCleansUpPartFiles is the regression test for the
// bug: a paused job whose download goroutine has already exited should
// have its partial .part / .part-NN / .parts.json files removed when
// the user clicks Cancel.
func TestCancelJob_PausedCleansUpPartFiles(t *testing.T) {
	cacheDir := t.TempDir()
	hub := NewWSHub()
	go hub.Run()
	mgr := NewJobManager(Config{CacheDir: cacheDir}, hub)

	job, blobsDir := pausedJobWithPartFiles(t, mgr, "pausecancel")
	wantGone := []string{
		"tmp-deadbeef00000000.part",
		"tmp-deadbeef00000000.part-00",
		"tmp-deadbeef00000000.part-01",
		"tmp-deadbeef00000000.parts.json",
	}
	for _, name := range wantGone {
		if _, err := os.Stat(filepath.Join(blobsDir, name)); err != nil {
			t.Fatalf("precondition: %s should exist: %v", name, err)
		}
	}

	if !mgr.CancelJob(job.ID) {
		t.Fatal("CancelJob should succeed for a paused job")
	}
	if j, _ := mgr.GetJob(job.ID); j.Status != JobStatusCancelled {
		t.Errorf("status = %s, want cancelled", j.Status)
	}

	for _, name := range wantGone {
		if _, err := os.Stat(filepath.Join(blobsDir, name)); !os.IsNotExist(err) {
			t.Errorf("partial file %s was not cleaned up (err=%v)", name, err)
		}
	}
	// Completed blob survives.
	if _, err := os.Stat(filepath.Join(blobsDir, "completed-blob")); err != nil {
		t.Errorf("completed blob should survive cancel: %v", err)
	}
}

// TestDismissJob_PausedCleansUpPartFiles covers the same scenario as
// the cancel test, but via the dismiss path. A dismissed paused job
// cannot be resumed, so leaving partial files behind is pure garbage.
func TestDismissJob_PausedCleansUpPartFiles(t *testing.T) {
	cacheDir := t.TempDir()
	hub := NewWSHub()
	go hub.Run()
	mgr := NewJobManager(Config{CacheDir: cacheDir}, hub)

	job, blobsDir := pausedJobWithPartFiles(t, mgr, "pausedismiss")

	res, snapshot := mgr.DismissJobResult(job.ID)
	if res != DismissJobOK {
		t.Fatalf("DismissJobResult = %v, want DismissJobOK", res)
	}
	if snapshot == nil || snapshot.Status != JobStatusPaused {
		t.Fatalf("snapshot = %+v, want paused", snapshot)
	}

	for _, name := range []string{
		"tmp-deadbeef00000000.part",
		"tmp-deadbeef00000000.part-00",
		"tmp-deadbeef00000000.part-01",
		"tmp-deadbeef00000000.parts.json",
	} {
		if _, err := os.Stat(filepath.Join(blobsDir, name)); !os.IsNotExist(err) {
			t.Errorf("partial file %s survived dismiss (err=%v)", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(blobsDir, "completed-blob")); err != nil {
		t.Errorf("completed blob should survive dismiss: %v", err)
	}
}

// TestCancelJob_RunningDoesNotTouchFiles makes sure the new path for
// paused jobs does not regress the running-job case: the live
// downloader callback is still responsible for cleanup, and the
// JobManager should not preemptively delete anything while a job is
// still running.
func TestCancelJob_RunningDoesNotTouchFiles(t *testing.T) {
	cacheDir := t.TempDir()
	hub := NewWSHub()
	go hub.Run()
	mgr := NewJobManager(Config{CacheDir: cacheDir}, hub)
	t.Cleanup(func() { mgr.WaitAll(5 * time.Second) })

	// Seed a partial file for a running job. We never call runJob; we
	// just check that CancelJob on a Running job returns without
	// deleting the file.
	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--running", "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	partPath := filepath.Join(blobsDir, "tmp-running.part")
	if err := os.WriteFile(partPath, []byte("in-flight"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr.mu.Lock()
	mgr.jobs["running-1"] = &Job{
		ID:        "running-1",
		Repo:      "owner/running",
		Status:    JobStatusRunning,
		CreatedAt: time.Now(),
		OutputDir: cacheDir,
	}
	mgr.mu.Unlock()

	if !mgr.CancelJob("running-1") {
		t.Fatal("CancelJob should succeed for running job")
	}
	// The running-job path does not call cleanupPausedJobPartFiles
	// because the downloader goroutine owns cleanup while the job is
	// live. Our seeded part file must still be on disk after the call.
	if _, err := os.Stat(partPath); err != nil {
		t.Errorf("running-job partial file was removed by CancelJob: %v", err)
	}
}

// TestCancelJob_NonPausedNoOp ensures we don't accidentally invoke
// cleanup for a non-paused cancel where the live downloader goroutine
// is responsible for cleanup (regression guard for the Pause → Cancel
// fix).
func TestCancelJob_PausedUnaffectedByMissingBlobs(t *testing.T) {
	cacheDir := t.TempDir()
	hub := NewWSHub()
	go hub.Run()
	mgr := NewJobManager(Config{CacheDir: cacheDir}, hub)

	// Paused job for a repo whose blobs dir has never been created.
	mgr.mu.Lock()
	mgr.jobs["paused-empty"] = &Job{
		ID:        "paused-empty",
		Repo:      "owner/empty",
		Status:    JobStatusPaused,
		CreatedAt: time.Now(),
		OutputDir: cacheDir,
	}
	mgr.mu.Unlock()

	// Should still succeed; missing dir is not an error.
	if !mgr.CancelJob("paused-empty") {
		t.Error("CancelJob should succeed even when blobs dir is missing")
	}
	if j, _ := mgr.GetJob("paused-empty"); j == nil || j.Status != JobStatusCancelled {
		t.Errorf("status = %+v, want cancelled", j)
	}
}
