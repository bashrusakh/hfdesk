// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"sync"
	"testing"
	"time"
)

// recordingSink collects every *Job it receives.
type recordingSink struct {
	mu   sync.Mutex
	jobs []*Job
}

func (r *recordingSink) send(j *Job) {
	r.mu.Lock()
	r.jobs = append(r.jobs, j)
	r.mu.Unlock()
}

func (r *recordingSink) snapshot() []*Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Job(nil), r.jobs...)
}

// TestJobCoalescer_FirstEventSentImmediately — an event for a job with no prior
// activity must be forwarded without delay.
func TestJobCoalescer_FirstEventSentImmediately(t *testing.T) {
	sink := &recordingSink{}
	c := newJobCoalescer(100*time.Millisecond, sink.send)
	defer c.stop()

	job := &Job{ID: "j1", Status: JobStatusRunning}
	c.schedule(job)

	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("got %d sent jobs, want 1", len(got))
	}
	if got[0].ID != "j1" {
		t.Errorf("got job %q, want j1", got[0].ID)
	}
}

// TestJobCoalescer_CoalescesBurst — a burst of progress events within the
// min-gap window collapses to at most two sends: the first one (immediate)
// and a flush with the latest state after min-gap.
func TestJobCoalescer_CoalescesBurst(t *testing.T) {
	sink := &recordingSink{}
	c := newJobCoalescer(100*time.Millisecond, sink.send)
	defer c.stop()

	for i := 0; i < 10; i++ {
		j := &Job{
			ID:     "j1",
			Status: JobStatusRunning,
			Progress: JobProgress{
				DownloadedBytes: int64(i * 1000),
			},
		}
		c.schedule(j)
	}
	// Wait for the coalescer to flush.
	time.Sleep(200 * time.Millisecond)

	got := sink.snapshot()
	if len(got) < 1 || len(got) > 2 {
		t.Fatalf("got %d sent jobs, want 1 or 2", len(got))
	}
	// The last sent job must reflect the LATEST state (i=9 → 9000 bytes).
	last := got[len(got)-1]
	if last.Progress.DownloadedBytes != 9000 {
		t.Errorf("last sent downloadedBytes = %d, want 9000", last.Progress.DownloadedBytes)
	}
}

// TestJobCoalescer_DifferentJobsAreIndependent — coalescing is per-job, so a
// different job ID doesn't share the min-gap window with another.
func TestJobCoalescer_DifferentJobsAreIndependent(t *testing.T) {
	sink := &recordingSink{}
	c := newJobCoalescer(100*time.Millisecond, sink.send)
	defer c.stop()

	c.schedule(&Job{ID: "j1", Status: JobStatusRunning})
	c.schedule(&Job{ID: "j2", Status: JobStatusRunning})
	c.schedule(&Job{ID: "j3", Status: JobStatusRunning})

	got := sink.snapshot()
	if len(got) != 3 {
		t.Fatalf("got %d sent jobs, want 3 (one per distinct job ID)", len(got))
	}
}

// TestJobCoalescer_TerminalBypasses — completed / failed / cancelled / paused
// events must flush immediately regardless of throttling, so the UI sees the
// final state without waiting for the min-gap window to elapse.
func TestJobCoalescer_TerminalBypasses(t *testing.T) {
	sink := &recordingSink{}
	c := newJobCoalescer(500*time.Millisecond, sink.send)
	defer c.stop()

	// First event (immediate send).
	c.schedule(&Job{ID: "j1", Status: JobStatusRunning, Progress: JobProgress{DownloadedBytes: 100}})
	// Next progress update within the gap (will be queued).
	c.schedule(&Job{ID: "j1", Status: JobStatusRunning, Progress: JobProgress{DownloadedBytes: 200}})
	// Terminal event arrives — must flush immediately.
	c.schedule(&Job{ID: "j1", Status: JobStatusCompleted, Progress: JobProgress{DownloadedBytes: 300}})

	// Give a tiny sliver of time for the terminal-path send to land.
	time.Sleep(20 * time.Millisecond)

	got := sink.snapshot()
	if len(got) < 2 {
		t.Fatalf("expected at least 2 sends (first + terminal), got %d", len(got))
	}
	last := got[len(got)-1]
	if last.Status != JobStatusCompleted {
		t.Errorf("last sent job status = %q, want %q", last.Status, JobStatusCompleted)
	}
	if last.Progress.DownloadedBytes != 300 {
		t.Errorf("last sent downloaded = %d, want 300", last.Progress.DownloadedBytes)
	}
}

// TestJobCoalescer_LateEventsAfterGap — once the min-gap window has elapsed
// after a send, the next event is sent immediately (not queued).
func TestJobCoalescer_LateEventsAfterGap(t *testing.T) {
	sink := &recordingSink{}
	c := newJobCoalescer(50*time.Millisecond, sink.send)
	defer c.stop()

	c.schedule(&Job{ID: "j1", Status: JobStatusRunning})
	time.Sleep(100 * time.Millisecond)
	c.schedule(&Job{ID: "j1", Status: JobStatusRunning, Progress: JobProgress{DownloadedBytes: 42}})

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d sent jobs, want 2", len(got))
	}
	if got[1].Progress.DownloadedBytes != 42 {
		t.Errorf("second send downloaded = %d, want 42", got[1].Progress.DownloadedBytes)
	}
}
