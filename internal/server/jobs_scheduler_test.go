// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"testing"
	"time"
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

	// First sample only establishes a baseline; no speed yet.
	updateJobSpeed(job, 1000)
	if job.Progress.BytesPerSecond != 0 {
		t.Fatalf("first sample should not set speed, got %d", job.Progress.BytesPerSecond)
	}

	// Simulate ~1s elapsed and 2 MB transferred since the baseline.
	job.speedPrevTime = time.Now().Add(-1 * time.Second)
	job.speedPrevBytes = 1000
	updateJobSpeed(job, 1000+2_000_000)
	if job.Progress.BytesPerSecond < 1_500_000 || job.Progress.BytesPerSecond > 2_500_000 {
		t.Fatalf("expected ~2 MB/s, got %d", job.Progress.BytesPerSecond)
	}

	// A sample taken too soon (< 0.5s) is ignored, leaving the speed unchanged.
	prev := job.Progress.BytesPerSecond
	updateJobSpeed(job, 1000+2_000_000+9_000_000)
	if job.Progress.BytesPerSecond != prev {
		t.Fatalf("sub-window sample changed speed: %d -> %d", prev, job.Progress.BytesPerSecond)
	}
}
