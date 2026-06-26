// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ptrOf is a small helper for tests to build a pointer to a literal
// map. Saves a few lines of `m := map{...}; &m` in test setup.
func ptrOf[K comparable, V any](m map[K]V) *map[K]V {
	return &m
}

// pausedJobWithPartialFiles seeds the cache with a realistic set of
// in-flight partial files for a paused job and registers the
// corresponding dsts in the job's per-job in-flight tracker. The job
// is inserted into the manager directly so the test does not have to
// drive a real download and then cancel it.
//
// dsts is the set the downloader would have called OnPartialFile
// with: for HF cache mode the dst IS the in-flight tmp-<sha> file
// (the downloader writes directly to that path), so the dst matches
// the file we create.
func pausedJobWithPartialFiles(t *testing.T, mgr *JobManager, repo string) (*Job, string) {
	t.Helper()
	blobsDir := filepath.Join(mgr.config.CacheDir, "hub", "models--owner--"+repo, "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// In-flight HF cache files: the dst IS the tmp-<sha> file the
	// downloader writes to directly. Two dsts in the tracker.
	dst1 := filepath.Join(blobsDir, "tmp-deadbeef00000000")
	dst2 := filepath.Join(blobsDir, "tmp-cafebabe00000000")
	for _, dst := range []string{dst1, dst2} {
		if err := os.WriteFile(dst, []byte("partial"), 0o644); err != nil {
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
	job := &Job{
		ID:        id,
		Repo:      "owner/" + repo,
		Status:    JobStatusPaused,
		CreatedAt: time.Now(),
		OutputDir: mgr.config.CacheDir,
	}
	// Populate the in-flight tracker as the downloader would have.
	tracker := map[string]struct{}{
		dst1: {},
		dst2: {},
	}
	job.partialFilesPtr = &tracker
	job.partialFilesMu = &sync.Mutex{}
	mgr.jobs[id] = job
	return job, blobsDir
}

// TestCancelJob_PausedCleansUpPartialFiles is the regression test for
// the bug: a paused job whose download goroutine has already exited
// should have its in-flight dsts removed when the user clicks Cancel.
// The cleanup is precise: only the dsts the per-job in-flight tracker
// recorded are touched, so a concurrent job for the same repo is not
// disturbed.
func TestCancelJob_PausedCleansUpPartialFiles(t *testing.T) {
	cacheDir := t.TempDir()
	hub := NewWSHub()
	go hub.Run()
	mgr := NewJobManager(Config{CacheDir: cacheDir}, hub)

	job, blobsDir := pausedJobWithPartialFiles(t, mgr, "pausecancel")
	dst1 := filepath.Join(blobsDir, "tmp-deadbeef00000000")
	dst2 := filepath.Join(blobsDir, "tmp-cafebabe00000000")
	for _, dst := range []string{dst1, dst2} {
		if _, err := os.Stat(dst); err != nil {
			t.Fatalf("precondition: %s should exist: %v", dst, err)
		}
	}

	if !mgr.CancelJob(job.ID) {
		t.Fatal("CancelJob should succeed for a paused job")
	}
	if j, _ := mgr.GetJob(job.ID); j.Status != JobStatusCancelled {
		t.Errorf("status = %s, want cancelled", j.Status)
	}

	for _, dst := range []string{dst1, dst2} {
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("in-flight dst %s was not cleaned up (err=%v)", dst, err)
		}
	}
	// Completed blob survives.
	if _, err := os.Stat(filepath.Join(blobsDir, "completed-blob")); err != nil {
		t.Errorf("completed blob should survive cancel: %v", err)
	}
}

// TestDismissJob_PausedCleansUpPartialFiles covers the same scenario
// as the cancel test, but via the dismiss path. A dismissed paused
// job cannot be resumed, so leaving partial files behind is pure
// garbage.
func TestDismissJob_PausedCleansUpPartialFiles(t *testing.T) {
	cacheDir := t.TempDir()
	hub := NewWSHub()
	go hub.Run()
	mgr := NewJobManager(Config{CacheDir: cacheDir}, hub)

	job, blobsDir := pausedJobWithPartialFiles(t, mgr, "pausedismiss")
	dst1 := filepath.Join(blobsDir, "tmp-deadbeef00000000")
	dst2 := filepath.Join(blobsDir, "tmp-cafebabe00000000")

	res, snapshot := mgr.DismissJobResult(job.ID)
	if res != DismissJobOK {
		t.Fatalf("DismissJobResult = %v, want DismissJobOK", res)
	}
	if snapshot == nil || snapshot.Status != JobStatusPaused {
		t.Fatalf("snapshot = %+v, want paused", snapshot)
	}

	for _, dst := range []string{dst1, dst2} {
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("in-flight dst %s survived dismiss (err=%v)", dst, err)
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

	// Seed a partial file for a running job. The job has no entries
	// in its in-flight tracker (we never call runJob), so the
	// server-side cleanup is a no-op.
	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--running", "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	partPath := filepath.Join(blobsDir, "tmp-running")
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
		// No partialFiles — running job is still in its goroutine.
	}
	mgr.mu.Unlock()

	if !mgr.CancelJob("running-1") {
		t.Fatal("CancelJob should succeed for running job")
	}
	// The running-job path does not call cleanupPausedJobPartFiles
	// because cleanupPausedJobPartFiles is only invoked when
	// wasPaused. The live downloader goroutine owns the cleanup,
	// and the in-flight tracker is empty anyway, so the helper
	// would have been a no-op.
	if _, err := os.Stat(partPath); err != nil {
		t.Errorf("running-job partial file was removed by CancelJob: %v", err)
	}
}

// TestCancelJob_PausedUnaffectedByMissingBlobs ensures we don't
// accidentally invoke cleanup for a paused job that has no in-flight
// dsts (which happens when the pause landed before any per-file
// goroutine registered, or when every dst had already been
// finalized). Cleanup is a no-op in that case, not an error.
func TestCancelJob_PausedUnaffectedByMissingBlobs(t *testing.T) {
	cacheDir := t.TempDir()
	hub := NewWSHub()
	go hub.Run()
	mgr := NewJobManager(Config{CacheDir: cacheDir}, hub)

	mgr.mu.Lock()
	mgr.jobs["paused-empty"] = &Job{
		ID:        "paused-empty",
		Repo:      "owner/empty",
		Status:    JobStatusPaused,
		CreatedAt: time.Now(),
		OutputDir: cacheDir,
		// No partialFiles — empty in-flight set.
	}
	mgr.mu.Unlock()

	if !mgr.CancelJob("paused-empty") {
		t.Error("CancelJob should succeed even with no in-flight dsts")
	}
	if j, _ := mgr.GetJob("paused-empty"); j == nil || j.Status != JobStatusCancelled {
		t.Errorf("status = %+v, want cancelled", j)
	}
}

// TestCancelJob_ConcurrentJobSameRepoNotDisturbed is the regression
// test for the high-priority finding from the second review pass:
// the JobManager explicitly supports concurrent jobs on the same repo
// (model + mmproj is the documented case). The per-dst cleanup must
// only touch the dsts that the cancelled job's downloader allocated,
// not any other file in the shared blobs directory. The previous
// scan-based implementation would have deleted a concurrent job's
// in-flight partials and corrupted the live download.
func TestCancelJob_ConcurrentJobSameRepoNotDisturbed(t *testing.T) {
	cacheDir := t.TempDir()
	hub := NewWSHub()
	go hub.Run()
	mgr := NewJobManager(Config{CacheDir: cacheDir}, hub)

	// Both jobs share the same blobs directory (same repo). Job A
	// is paused and about to be cancelled. Job B is still actively
	// downloading — its in-flight dsts MUST NOT be removed when Job
	// A is cancelled.
	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--shared", "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Job A's in-flight files (will be cancelled).
	jobADst1 := filepath.Join(blobsDir, "tmp-jobA-file1")
	jobADst2 := filepath.Join(blobsDir, "tmp-jobA-file2")
	// Job B's in-flight files (must survive A's cancel).
	jobBDst1 := filepath.Join(blobsDir, "tmp-jobB-file1")
	jobBDst2 := filepath.Join(blobsDir, "tmp-jobB-file2")
	for _, dst := range []string{jobADst1, jobADst2, jobBDst1, jobBDst2} {
		if err := os.WriteFile(dst, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mgr.mu.Lock()
	trackerA := map[string]struct{}{
		jobADst1: {},
		jobADst2: {},
	}
	mgr.jobs["jobA"] = &Job{
		ID:              "jobA",
		Repo:            "owner/shared",
		Status:          JobStatusPaused,
		CreatedAt:       time.Now(),
		OutputDir:       cacheDir,
		partialFilesPtr: &trackerA,
		partialFilesMu:  &sync.Mutex{},
	}
	trackerB := map[string]struct{}{
		jobBDst1: {},
		jobBDst2: {},
	}
	mgr.jobs["jobB"] = &Job{
		ID:              "jobB",
		Repo:            "owner/shared",
		Status:          JobStatusRunning, // still actively downloading
		CreatedAt:       time.Now(),
		OutputDir:       cacheDir,
		partialFilesPtr: &trackerB,
		partialFilesMu:  &sync.Mutex{},
	}
	mgr.mu.Unlock()

	if !mgr.CancelJob("jobA") {
		t.Fatal("CancelJob should succeed for paused Job A")
	}
	if j, _ := mgr.GetJob("jobA"); j.Status != JobStatusCancelled {
		t.Errorf("Job A status = %s, want cancelled", j.Status)
	}

	// Job A's in-flight files removed.
	for _, dst := range []string{jobADst1, jobADst2} {
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("Job A dst %s should be cleaned up (err=%v)", dst, err)
		}
	}
	// Job B's in-flight files UNTOUCHED. This is the regression
	// guard for the high-priority finding.
	for _, dst := range []string{jobBDst1, jobBDst2} {
		if _, err := os.Stat(dst); err != nil {
			t.Errorf("Job B dst %s was removed by Job A's cancel — concurrent-job data loss: %v", dst, err)
		}
	}
	// Job B still running.
	if j, _ := mgr.GetJob("jobB"); j == nil || j.Status != JobStatusRunning {
		t.Errorf("Job B status = %+v, want still running", j)
	}
}

// TestDismissJob_ConcurrentJobSameRepoNotDisturbed is the same
// regression for the dismiss path: dismissing a paused job for one
// file must not touch a concurrent job's in-flight files for a
// different file in the same repo.
func TestDismissJob_ConcurrentJobSameRepoNotDisturbed(t *testing.T) {
	cacheDir := t.TempDir()
	hub := NewWSHub()
	go hub.Run()
	mgr := NewJobManager(Config{CacheDir: cacheDir}, hub)

	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--shared", "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jobADst := filepath.Join(blobsDir, "tmp-jobA-only")
	jobBDst := filepath.Join(blobsDir, "tmp-jobB-only")
	for _, dst := range []string{jobADst, jobBDst} {
		if err := os.WriteFile(dst, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mgr.mu.Lock()
	mgr.jobs["jobA"] = &Job{
		ID:              "jobA",
		Repo:            "owner/shared",
		Status:          JobStatusPaused,
		CreatedAt:       time.Now(),
		OutputDir:       cacheDir,
		partialFilesPtr: ptrOf(map[string]struct{}{jobADst: {}}),
		partialFilesMu:  &sync.Mutex{},
	}
	mgr.jobs["jobB"] = &Job{
		ID:              "jobB",
		Repo:            "owner/shared",
		Status:          JobStatusRunning,
		CreatedAt:       time.Now(),
		OutputDir:       cacheDir,
		partialFilesPtr: ptrOf(map[string]struct{}{jobBDst: {}}),
		partialFilesMu:  &sync.Mutex{},
	}
	mgr.mu.Unlock()

	if res, _ := mgr.DismissJobResult("jobA"); res != DismissJobOK {
		t.Fatalf("DismissJobResult = %v, want DismissJobOK", res)
	}

	if _, err := os.Stat(jobADst); !os.IsNotExist(err) {
		t.Errorf("Job A dst should be cleaned up: %v", err)
	}
	if _, err := os.Stat(jobBDst); err != nil {
		t.Errorf("Job B dst was removed by Job A's dismiss: %v", err)
	}
}
