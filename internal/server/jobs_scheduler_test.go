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

// TestJobManager_LoweringMaxActivePausesExcess verifies that reducing the
// max-active setting pauses the most-recently-started running jobs so the
// active count drops to the new limit, keeping the oldest downloads running.
func TestJobManager_LoweringMaxActivePausesExcess(t *testing.T) {
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

	running, paused := 0, 0
	for _, j := range mgr.ListJobs() {
		switch j.Status {
		case JobStatusRunning:
			running++
		case JobStatusPaused:
			paused++
		}
	}
	if running != 2 || paused != 2 {
		t.Fatalf("expected 2 running / 2 paused after lowering limit, got %d running / %d paused", running, paused)
	}

	// The two oldest-started jobs should keep running.
	for _, id := range []string{"run0", "run1"} {
		j, _ := mgr.GetJob(id)
		if j == nil || j.Status != JobStatusRunning {
			t.Errorf("expected oldest job %s to stay running", id)
		}
	}
}
