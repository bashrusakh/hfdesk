// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"sync"
	"time"
)

// jobCoalescer throttles WebSocket broadcasts for job updates so the frontend
// doesn't have to re-render its job list at the raw 5Hz-per-file progress
// rate (github issue #62). For each job ID we enforce a minimum gap between
// sends; events arriving inside the gap are collapsed — only the latest queued
// state is flushed when the gap elapses. Terminal status changes (completed,
// failed, cancelled, paused) bypass the gap and are sent immediately so the
// UI sees the final state and its pause/resume buttons without delay.
type jobCoalescer struct {
	mu       sync.Mutex
	lastSent map[string]time.Time
	pending  map[string]*Job
	timers   map[string]*time.Timer
	minGap   time.Duration
	send     func(*Job)
	stopped  bool
}

// newJobCoalescer builds a coalescer that forwards at most one event per job
// per minGap via the supplied send function. send is invoked without the
// coalescer's lock held.
func newJobCoalescer(minGap time.Duration, send func(*Job)) *jobCoalescer {
	return &jobCoalescer{
		lastSent: make(map[string]time.Time),
		pending:  make(map[string]*Job),
		timers:   make(map[string]*time.Timer),
		minGap:   minGap,
		send:     send,
	}
}

// schedule forwards a job update through the throttling gate. The first event
// for a given job, any event arriving after minGap has elapsed since the
// previous send, and any event whose status is terminal are dispatched
// immediately. Anything else is queued (superseding any earlier queued state)
// and flushed when the gap expires.
func (c *jobCoalescer) schedule(job *Job) {
	terminal := isTerminalJobStatus(job.Status)

	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}

	// Terminal updates always flush immediately and clear any pending state.
	if terminal {
		if t, ok := c.timers[job.ID]; ok {
			t.Stop()
			delete(c.timers, job.ID)
		}
		delete(c.pending, job.ID)
		c.lastSent[job.ID] = time.Now()
		c.mu.Unlock()
		c.send(job)
		return
	}

	now := time.Now()
	last, hasLast := c.lastSent[job.ID]
	if !hasLast || now.Sub(last) >= c.minGap {
		// Outside the throttle window: dispatch immediately. Any queued state
		// is superseded by this fresher update.
		if t, ok := c.timers[job.ID]; ok {
			t.Stop()
			delete(c.timers, job.ID)
		}
		delete(c.pending, job.ID)
		c.lastSent[job.ID] = now
		c.mu.Unlock()
		c.send(job)
		return
	}

	// Inside the window: queue the latest state and arm a flush timer if
	// there isn't one already.
	c.pending[job.ID] = job
	if _, armed := c.timers[job.ID]; armed {
		c.mu.Unlock()
		return
	}
	delay := c.minGap - now.Sub(last)
	id := job.ID // capture for the closure
	c.timers[id] = time.AfterFunc(delay, func() {
		c.mu.Lock()
		if c.stopped {
			c.mu.Unlock()
			return
		}
		latest, ok := c.pending[id]
		delete(c.pending, id)
		delete(c.timers, id)
		if ok {
			c.lastSent[id] = time.Now()
		}
		c.mu.Unlock()
		if ok {
			c.send(latest)
		}
	})
	c.mu.Unlock()
}

// stop halts pending timers and rejects further schedule calls. Safe to call
// multiple times.
func (c *jobCoalescer) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	for id, t := range c.timers {
		t.Stop()
		delete(c.timers, id)
	}
	c.pending = map[string]*Job{}
}

// isTerminalJobStatus reports whether a status represents a state the user
// cares about seeing right away (so it shouldn't be delayed by the throttle).
func isTerminalJobStatus(s JobStatus) bool {
	switch s {
	case JobStatusCompleted, JobStatusFailed, JobStatusCancelled, JobStatusPaused:
		return true
	default:
		return false
	}
}
