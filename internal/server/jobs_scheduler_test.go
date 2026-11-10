// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
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
	mgr := newTestJobManager(t, cfg, hub)
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
		mgr.jobs[id] = &Job{
			ID:             id,
			Repo:           "x/y",
			Status:         JobStatusQueued,
			CreatedAt:      ct,
			partialFilesMu: &sync.Mutex{}, // mirror CreateJob so runJob can use it
		}
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
	mgr := newTestJobManager(t, cfg, hub)

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
// the floor for the recovery window only: once the running sum catches
// up to it, the floor clears and a real drop (e.g. a 200 fallback
// truncating a .part file) is reflected immediately.
func TestApplyJobProgress_DownloadedBytesMonotonic(t *testing.T) {
	now := time.Now()

	// resumedJob mirrors what ResumeJob leaves behind: DownloadedBytes
	// preserved from the pre-pause state and resumeFloor armed at the
	// same value so the monotonic guard in applyJobProgress fires.
	resumedJob := func(downloadedBytes int64) *Job {
		job := &Job{Progress: JobProgress{DownloadedBytes: downloadedBytes}}
		job.resumeFloor = downloadedBytes
		return job
	}

	t.Run("file_done after resume does not drop the preserved total", func(t *testing.T) {
		// 6000 of 10000 bytes were downloaded before pause; ResumeJob
		// preserved DownloadedBytes=6000 and nilled job.Files. The
		// downloader will now replay the plan and process files.
		job := resumedJob(6000)
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
		if job.resumeFloor != 6000 {
			t.Fatalf("resumeFloor cleared too early: got %d, want 6000", job.resumeFloor)
		}

		// Once all files report done the sum catches up and growth
		// resumes normally — the floor is also released here.
		apply(hfdownloader.ProgressEvent{Event: "file_done", Path: "big.bin"})
		if got := job.Progress.DownloadedBytes; got != 10000 {
			t.Fatalf("DownloadedBytes after both file_done: got %d, want 10000", got)
		}
		if job.resumeFloor != 0 {
			t.Fatalf("resumeFloor not cleared after catch-up: got %d, want 0", job.resumeFloor)
		}
	})

	t.Run("file_progress after resume does not drop the preserved total", func(t *testing.T) {
		job := resumedJob(6000)
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
		if job.resumeFloor != 6000 {
			t.Fatalf("resumeFloor cleared too early: got %d, want 6000", job.resumeFloor)
		}

		// Once the partial file's progress exceeds the floor, total
		// growth resumes from the new (larger) value and the floor
		// is released.
		apply(hfdownloader.ProgressEvent{Event: "file_progress", Path: "partial.bin", Downloaded: 7000})
		if got := job.Progress.DownloadedBytes; got != 7000 {
			t.Fatalf("DownloadedBytes after surpassing floor: got %d, want 7000", got)
		}
		if job.resumeFloor != 0 {
			t.Fatalf("resumeFloor not cleared after catch-up: got %d, want 0", job.resumeFloor)
		}
	})

	t.Run("drop is reflected after the resume floor clears", func(t *testing.T) {
		// A job that has fully recovered from a previous resume: the
		// floor is 0 (no longer active), and DownloadedBytes is at
		// the genuine 10000. A file rolling back — e.g.
		// downloadSingle's 200 fallback truncating the .part file
		// and emitting a file_progress with Downloaded=0 — must
		// propagate the drop to the displayed total. The previous
		// (permanent) guard would have kept the UI at 10000 and lied
		// about the actual on-disk state.
		job := &Job{Progress: JobProgress{DownloadedBytes: 10000}}
		apply := func(evt hfdownloader.ProgressEvent) {
			applyJobProgress(job, evt, now)
		}

		apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: "big.bin", Total: 10000})
		apply(hfdownloader.ProgressEvent{Event: "file_progress", Path: "big.bin", Downloaded: 0})
		if got := job.Progress.DownloadedBytes; got != 0 {
			t.Fatalf("post-recovery rollback not reflected: got %d, want 0", got)
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

// TestApplyJobProgressKeepsWarningEvents pins the job's warning list: a
// warning event is kept in the job state and rides the Job JSON the API
// serves (handleGetJob, handleListJobs and BroadcastJob marshal the *Job
// itself), a job that receives no warnings carries none (the field is
// omitted), an empty message is not stored, a non-warning event never
// becomes one, and the list is bounded — once maxJobWarnings entries are
// kept it stops growing while keeping the first ones.
func TestApplyJobProgressKeepsWarningEvents(t *testing.T) {
	now := time.Now()
	warn := func(msg string) hfdownloader.ProgressEvent {
		return hfdownloader.ProgressEvent{Level: "warn", Event: "warning", Message: msg}
	}

	t.Run("kept in state and in the API response shape", func(t *testing.T) {
		job := &Job{ID: "warn-job"}
		applyJobProgress(job, hfdownloader.ProgressEvent{Event: "plan_item", Path: "a.gguf", Total: 10}, now)
		applyJobProgress(job, warn("revision is not pinned to an immutable commit"), now)

		const want = "revision is not pinned to an immutable commit"
		if len(job.Warnings) != 1 || job.Warnings[0] != want {
			t.Fatalf("job.Warnings = %q, want [%q]", job.Warnings, want)
		}
		// The warning case folds nothing else into the job.
		if job.Progress.TotalFiles != 1 || job.Progress.TotalBytes != 10 || len(job.Files) != 1 {
			t.Errorf("warning disturbed progress: totalFiles=%d totalBytes=%d fileList=%d",
				job.Progress.TotalFiles, job.Progress.TotalBytes, len(job.Files))
		}

		data, err := json.Marshal(job)
		if err != nil {
			t.Fatalf("marshal job: %v", err)
		}
		var decoded struct {
			Warnings []string `json:"warnings"`
		}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal job JSON: %v", err)
		}
		if len(decoded.Warnings) != 1 || decoded.Warnings[0] != want {
			t.Errorf("response warnings = %q, want [%q]", decoded.Warnings, want)
		}
	})

	t.Run("job without warnings carries none", func(t *testing.T) {
		job := &Job{ID: "quiet-job"}
		applyJobProgress(job, hfdownloader.ProgressEvent{Event: "plan_item", Path: "a.gguf", Total: 10}, now)
		// A non-warning event carrying a message must not become a warning.
		applyJobProgress(job, hfdownloader.ProgressEvent{Level: "error", Event: "error", Message: "boom"}, now)
		if len(job.Warnings) != 0 {
			t.Fatalf("job.Warnings = %q, want none", job.Warnings)
		}
		data, err := json.Marshal(job)
		if err != nil {
			t.Fatalf("marshal job: %v", err)
		}
		if strings.Contains(string(data), `"warnings"`) {
			t.Errorf("job JSON must omit the warnings field when empty, got %s", data)
		}
	})

	t.Run("empty message is not stored", func(t *testing.T) {
		job := &Job{}
		applyJobProgress(job, warn(""), now)
		if len(job.Warnings) != 0 {
			t.Fatalf("job.Warnings = %q, want none for an empty message", job.Warnings)
		}
	})

	t.Run("list is bounded and keeps the first entries", func(t *testing.T) {
		job := &Job{}
		for i := 0; i < maxJobWarnings+5; i++ {
			applyJobProgress(job, warn(fmt.Sprintf("warning %d", i)), now)
		}
		if len(job.Warnings) != maxJobWarnings {
			t.Fatalf("len(job.Warnings) = %d, want the cap %d", len(job.Warnings), maxJobWarnings)
		}
		if job.Warnings[0] != "warning 0" {
			t.Errorf("first kept warning = %q, want %q (the cap keeps the first entries)",
				job.Warnings[0], "warning 0")
		}
		if last, wantLast := job.Warnings[maxJobWarnings-1], fmt.Sprintf("warning %d", maxJobWarnings-1); last != wantLast {
			t.Errorf("last kept warning = %q, want %q", last, wantLast)
		}
	})

	t.Run("repeated diagnostic keeps one entry", func(t *testing.T) {
		job := &Job{}
		applyJobProgress(job, warn("revision is not pinned"), now)
		applyJobProgress(job, warn("revision is not pinned"), now) // re-emitted by a later attempt
		if len(job.Warnings) != 1 || job.Warnings[0] != "revision is not pinned" {
			t.Fatalf("job.Warnings = %q, want one entry for the repeated diagnostic", job.Warnings)
		}
		// A distinct diagnostic is kept beside it, first arrival first.
		applyJobProgress(job, warn("failed to write manifest: disk full"), now)
		if len(job.Warnings) != 2 ||
			job.Warnings[0] != "revision is not pinned" ||
			job.Warnings[1] != "failed to write manifest: disk full" {
			t.Fatalf("job.Warnings = %q, want both diagnostics in first-arrival order", job.Warnings)
		}
		// A further repeat of the first diagnostic still adds nothing.
		applyJobProgress(job, warn("revision is not pinned"), now)
		if len(job.Warnings) != 2 {
			t.Fatalf("job.Warnings = %q, want the repeat to add no entry", job.Warnings)
		}
	})

	t.Run("repeats do not consume the cap", func(t *testing.T) {
		job := &Job{}
		// Every message below is emitted twice, as a re-run of the plan
		// would emit it: the repeats must not fill the bounded list.
		for i := 0; i < maxJobWarnings-1; i++ {
			applyJobProgress(job, warn(fmt.Sprintf("warning %d", i)), now)
			applyJobProgress(job, warn(fmt.Sprintf("warning %d", i)), now)
		}
		if len(job.Warnings) != maxJobWarnings-1 {
			t.Fatalf("len(job.Warnings) = %d, want %d after doubled emissions", len(job.Warnings), maxJobWarnings-1)
		}
		// The cap still has room for the next distinct diagnostic even
		// though every earlier message was already emitted twice.
		applyJobProgress(job, warn("failed to write manifest: disk full"), now)
		if len(job.Warnings) != maxJobWarnings {
			t.Fatalf("len(job.Warnings) = %d, want the cap %d", len(job.Warnings), maxJobWarnings)
		}
		if job.Warnings[maxJobWarnings-1] != "failed to write manifest: disk full" {
			t.Errorf("last entry = %q, want the distinct diagnostic to take the final slot", job.Warnings[maxJobWarnings-1])
		}
		// Full: a further distinct diagnostic is still dropped.
		applyJobProgress(job, warn("failed to write ref: permission denied"), now)
		if len(job.Warnings) != maxJobWarnings {
			t.Errorf("len(job.Warnings) = %d, want the cap %d once full", len(job.Warnings), maxJobWarnings)
		}
	})
}

// TestResumeKeepsOneWarningEntryPerDiagnostic pins the warning lifecycle
// across a resume-style restart: resetAndQueueRestartLocked leaves the
// job's warnings alone, and when the downloader re-emits the identical
// messages on the next attempt — the unpinned-revision and plan-notice
// warnings come from the per-attempt plan — they stay one entry each, so a
// later distinct diagnostic of the new attempt still fits the bound.
func TestResumeKeepsOneWarningEntryPerDiagnostic(t *testing.T) {
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "state"))
	m.fenced = true // no attempt is dispatched; this pins the reset and re-emission only.
	const unpinned = "revision is not pinned to an immutable commit"
	job := &Job{
		ID:        "resume-warnings",
		Repo:      "owner/repo",
		Status:    JobStatusPaused,
		CreatedAt: time.Now(),
		Warnings:  []string{unpinned},
	}
	m.jobs[job.ID] = job

	now := time.Now()
	warn := func(msg string) hfdownloader.ProgressEvent {
		return hfdownloader.ProgressEvent{Level: "warn", Event: "warning", Message: msg}
	}

	// Resume path (retry=false): the reset must neither clear the kept
	// diagnostics nor duplicate them.
	m.mu.Lock()
	resetAndQueueRestartLocked(m, job, false)
	m.mu.Unlock()
	if len(job.Warnings) != 1 || job.Warnings[0] != unpinned {
		t.Fatalf("job.Warnings after resume reset = %q, want the single kept diagnostic", job.Warnings)
	}

	// The next attempt re-emits the same warning: same fact, same entry.
	applyJobProgress(job, warn(unpinned), now)
	if len(job.Warnings) != 1 {
		t.Fatalf("job.Warnings after re-emission = %q, want one entry for the repeated diagnostic", job.Warnings)
	}
	// A distinct diagnostic of the new attempt is kept beside it.
	applyJobProgress(job, warn("failed to write manifest: disk full"), now)
	if len(job.Warnings) != 2 || job.Warnings[1] != "failed to write manifest: disk full" {
		t.Fatalf("job.Warnings = %q, want the re-emitted and the distinct diagnostic once each", job.Warnings)
	}
}
