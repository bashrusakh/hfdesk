// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// TestJobManager_DispatchRespectsMaxActive verifies that no more than
// max-active jobs are started at once and the rest wait as queued, in
// creation order (oldest first).
func TestJobManager_DispatchRespectsMaxActive(t *testing.T) {
	cfg := Config{CacheDir: t.TempDir(), MaxActive: 2}
	hub := NewWSHub()
	go hub.Run()
	mgr := NewJobManager(cfg, hub)
	t.Cleanup(func() {
		for _, j := range mgr.ListJobs() {
			mgr.CancelJob(j.ID)
		}
		mgr.WaitAll(5 * time.Second)
	})

	now := time.Now()
	mgr.mu.Lock()
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("q%d", i)
		ct := now.Add(time.Duration(i) * time.Second)
		mgr.jobs[id] = &Job{ID: id, Repo: "x/y", Status: JobStatusQueued, CreatedAt: ct}
	}
	mgr.dispatchLocked()

	// Hold the lock so the launched runJob goroutines can't mutate state
	// while we assert: exactly MaxActive jobs are starting, rest queued,
	// and the two starting ones are the oldest.
	starting, queued := 0, 0
	for _, j := range mgr.jobs {
		if j.starting {
			starting++
		} else if j.Status == JobStatusQueued {
			queued++
		}
	}
	q0Starting := mgr.jobs["q0"].starting && mgr.jobs["q1"].starting
	mgr.mu.Unlock()

	if starting != 2 || queued != 3 {
		t.Fatalf("expected 2 starting / 3 queued, got %d starting / %d queued", starting, queued)
	}
	if !q0Starting {
		t.Error("expected the two oldest jobs (q0, q1) to be the ones started")
	}
}

// TestJobManager_LoweringMaxActiveRequeuesExcess verifies that reducing the
// max-active setting re-queues the most-recently-started running jobs so the
// active count drops to the new limit, keeping the oldest downloads running and
// letting the dispatcher restart the rest later.
func TestJobManager_LoweringMaxActiveRequeuesExcess(t *testing.T) {
	cfg := Config{CacheDir: t.TempDir(), MaxActive: 4}
	hub := NewWSHub()
	go hub.Run()
	mgr := NewJobManager(cfg, hub)

	now := time.Now()
	for i := 0; i < 4; i++ {
		st := now.Add(time.Duration(i) * time.Second)
		id := fmt.Sprintf("run%d", i)
		mgr.jobs[id] = &Job{ID: id, Repo: "x/y", Status: JobStatusRunning, StartedAt: &st, cancel: func() {}}
	}

	// Lower the limit to 2 via the same path the settings API uses.
	mgr.UpdateConfig(Config{CacheDir: cfg.CacheDir, MaxActive: 2})

	running, queued := 0, 0
	for _, j := range mgr.ListJobs() {
		switch j.Status {
		case JobStatusRunning:
			running++
		case JobStatusQueued:
			queued++
		}
	}
	if running != 2 || queued != 2 {
		t.Fatalf("expected 2 running / 2 queued after lowering limit, got %d running / %d queued", running, queued)
	}

	// The two oldest-started jobs should keep running.
	for _, id := range []string{"run0", "run1"} {
		j, _ := mgr.GetJob(id)
		if j == nil || j.Status != JobStatusRunning {
			t.Errorf("expected oldest job %s to stay running", id)
		}
	}
}

func TestUpdateJobSpeed(t *testing.T) {
	job := &Job{}
	start := time.Now()

	// First sample only establishes a baseline; no speed yet.
	updateJobSpeed(job, 0, start)
	if job.Progress.BytesPerSecond != 0 {
		t.Fatalf("first sample should not set speed, got %d", job.Progress.BytesPerSecond)
	}

	// A sample arriving sooner than the 400ms throttle is ignored entirely.
	updateJobSpeed(job, 50_000_000, start.Add(200*time.Millisecond))
	if got := len(job.speedSamples); got != 1 {
		t.Fatalf("throttled sample was recorded: %d samples", got)
	}
	if job.Progress.BytesPerSecond != 0 {
		t.Fatalf("throttled sample changed speed: %d", job.Progress.BytesPerSecond)
	}

	// Steady 2 MB/s for a few seconds settles the reading at ~2 MB/s.
	for i := 1; i <= 6; i++ {
		now := start.Add(time.Duration(i) * 500 * time.Millisecond)
		updateJobSpeed(job, int64(i)*1_000_000, now)
	}
	if job.Progress.BytesPerSecond < 1_500_000 || job.Progress.BytesPerSecond > 2_500_000 {
		t.Fatalf("expected ~2 MB/s, got %d", job.Progress.BytesPerSecond)
	}

	// The rate suddenly doubles to 4 MB/s. The EMA blend keeps the very next
	// reading between the old rate and the raw windowed rate instead of
	// snapping to it.
	prev := job.Progress.BytesPerSecond
	updateJobSpeed(job, 6_000_000+2_000_000, start.Add(3500*time.Millisecond))
	got := job.Progress.BytesPerSecond
	if got <= prev {
		t.Fatalf("speed did not rise after a faster sample: %d -> %d", prev, got)
	}
	if got >= 4_000_000 {
		t.Fatalf("speed snapped to the raw rate instead of smoothing: %d -> %d", prev, got)
	}

	// Samples older than speedWindow fall out of the window.
	later := start.Add(speedWindow + 4*time.Second)
	updateJobSpeed(job, 9_000_000, later)
	for _, s := range job.speedSamples {
		if age := later.Sub(s.t); age > speedWindow {
			t.Fatalf("stale sample kept: age %v", age)
		}
	}
}

// TestApplyJobProgress_FileFinalizingPhase verifies that the job only enters
// the finalizing phase once nothing is left downloading: a file finishing its
// transfer while another is still active must not flip the phase, and once
// the last transfer ends the stale speed/ETA readings are cleared.
func TestApplyJobProgress_FileFinalizingPhase(t *testing.T) {
	job := &Job{}
	now := time.Now()

	apply := func(evt hfdownloader.ProgressEvent) {
		applyJobProgress(job, evt, now)
	}

	apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: "a.gguf", Total: 100})
	apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: "b.gguf", Total: 100})
	apply(hfdownloader.ProgressEvent{Event: "file_start", Path: "a.gguf"})
	apply(hfdownloader.ProgressEvent{Event: "file_start", Path: "b.gguf"})

	// a finishes its transfer while b is still active: no job-level phase.
	apply(hfdownloader.ProgressEvent{Event: "file_finalizing", Path: "a.gguf"})
	if job.Files[0].Status != "finalizing" {
		t.Fatalf("file a status = %q, want finalizing", job.Files[0].Status)
	}
	if job.Phase != "" {
		t.Fatalf("phase flipped to %q while b is still downloading", job.Phase)
	}

	// b finishes its transfer too: the whole job is now local post-processing.
	job.Progress.BytesPerSecond = 1_000_000
	job.Progress.EtaSeconds = 42
	apply(hfdownloader.ProgressEvent{Event: "file_finalizing", Path: "b.gguf"})
	if job.Phase != "finalizing" {
		t.Fatalf("phase = %q, want finalizing", job.Phase)
	}
	if job.Progress.BytesPerSecond != 0 || job.Progress.EtaSeconds != 0 {
		t.Fatalf("stale speed/ETA not cleared: %d B/s, %d s",
			job.Progress.BytesPerSecond, job.Progress.EtaSeconds)
	}

	apply(hfdownloader.ProgressEvent{Event: "file_done", Path: "a.gguf"})
	apply(hfdownloader.ProgressEvent{Event: "file_done", Path: "b.gguf"})
	if job.Files[0].Status != "complete" || job.Files[1].Status != "complete" {
		t.Fatalf("files not complete after file_done: %q, %q",
			job.Files[0].Status, job.Files[1].Status)
	}
}

// TestApplyJobProgress_DownloadedBytesMonotonic guards against the
// regression where, on a paused/resumed job, the first file_done or
// file_progress event after resume would recalculate DownloadedBytes
// from a partially-populated job.Files list and overwrite the value
// ResumeJob preserved from the pre-pause state. The preserved value is
// the floor: it stays on screen until the sum of all file progress
// catches up to it.
func TestApplyJobProgress_DownloadedBytesMonotonic(t *testing.T) {
	now := time.Now()

	t.Run("file_done after resume does not drop the preserved total", func(t *testing.T) {
		// 6000 of 10000 bytes were downloaded before pause; ResumeJob
		// preserved DownloadedBytes=6000 and nilled job.Files. The
		// downloader will now replay the plan and process files.
		job := &Job{Progress: JobProgress{DownloadedBytes: 6000}}
		apply := func(evt hfdownloader.ProgressEvent) {
			applyJobProgress(job, evt, now)
		}

		apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: "small.bin", Total: 100})
		apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: "big.bin", Total: 9900})
		// First skipped file emits file_done: the recalculated sum is
		// only 100 (other file still has Downloaded=0). Without the
		// monotonic guard this would drop 6000 -> 100.
		apply(hfdownloader.ProgressEvent{Event: "file_done", Path: "small.bin"})
		if got := job.Progress.DownloadedBytes; got != 6000 {
			t.Fatalf("DownloadedBytes dropped after first file_done: got %d, want 6000", got)
		}

		// Once all files report done the sum catches up and growth
		// resumes normally.
		apply(hfdownloader.ProgressEvent{Event: "file_done", Path: "big.bin"})
		if got := job.Progress.DownloadedBytes; got != 10000 {
			t.Fatalf("DownloadedBytes after both file_done: got %d, want 10000", got)
		}
	})

	t.Run("file_progress after resume does not drop the preserved total", func(t *testing.T) {
		job := &Job{Progress: JobProgress{DownloadedBytes: 6000}}
		apply := func(evt hfdownloader.ProgressEvent) {
			applyJobProgress(job, evt, now)
		}

		apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: "small.bin", Total: 100})
		apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: "partial.bin", Total: 5000})
		// First progress for the partial file: 3000 on disk, the
		// recalculated sum is just 3000 (small.bin still Downloaded=0).
		apply(hfdownloader.ProgressEvent{Event: "file_progress", Path: "partial.bin", Downloaded: 3000})
		if got := job.Progress.DownloadedBytes; got != 6000 {
			t.Fatalf("DownloadedBytes dropped after first file_progress: got %d, want 6000", got)
		}

		// Once the partial file's progress exceeds the floor, total
		// growth resumes from the new (larger) value.
		apply(hfdownloader.ProgressEvent{Event: "file_progress", Path: "partial.bin", Downloaded: 7000})
		if got := job.Progress.DownloadedBytes; got != 7000 {
			t.Fatalf("DownloadedBytes after surpassing floor: got %d, want 7000", got)
		}
	})
}

func TestUpdateJobETA(t *testing.T) {
	job := &Job{}
	job.Progress.TotalBytes = 100_000_000
	start := time.Now()

	// Nothing transferred yet — no estimate.
	updateJobSpeed(job, 0, start)
	if job.Progress.EtaSeconds != 0 {
		t.Fatalf("ETA before any transfer should be 0, got %d", job.Progress.EtaSeconds)
	}

	// Steady 2 MB/s fresh download: DownloadedBytes mirrors transferred.
	for i := 1; i <= 10; i++ {
		transferred := int64(i) * 1_000_000
		job.Progress.DownloadedBytes = transferred
		updateJobSpeed(job, transferred, start.Add(time.Duration(i)*500*time.Millisecond))
	}
	// 10 MB moved in 5s, 90 MB remaining → ≈45s.
	if eta := job.Progress.EtaSeconds; eta < 40 || eta > 50 {
		t.Fatalf("expected ETA ≈45s, got %d", eta)
	}

	// Everything downloaded — the estimate clears.
	job.Progress.DownloadedBytes = job.Progress.TotalBytes
	updateJobSpeed(job, job.Progress.TotalBytes, start.Add(60*time.Second))
	if job.Progress.EtaSeconds != 0 {
		t.Fatalf("ETA after completion should be 0, got %d", job.Progress.EtaSeconds)
	}
}
