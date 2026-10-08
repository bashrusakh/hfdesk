// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// DefaultStallTimeout is how long a body read may deliver no bytes before the
// attempt is aborted and retried. 0 (an explicit "0s"/"0" setting) disables
// the watchdog.
const DefaultStallTimeout = 60 * time.Second

// stallTimeout resolves the effective stall timeout from settings. An empty
// value yields DefaultStallTimeout, so library callers that leave the field
// unset get the protective default. A parsed value of 0 disables the watchdog;
// an unparseable OR NEGATIVE value falls back to the default rather than
// disabling it, so a bad setting can never silently remove the protection.
func stallTimeout(cfg Settings) time.Duration {
	if cfg.StallTimeout == "" {
		return DefaultStallTimeout
	}
	d, err := time.ParseDuration(cfg.StallTimeout)
	if err != nil {
		return DefaultStallTimeout
	}
	if d < 0 {
		return DefaultStallTimeout
	}
	return d
}

// StallError reports that a transfer attempt was aborted by the stall
// watchdog: the body read delivered no bytes for the configured Timeout. It
// deliberately does not wrap context.Canceled. The attempt context was
// cancelled internally by the watchdog, never by the caller, and callers (job
// status classification, users reading job.Error) must be able to tell an
// aborted stall apart from a genuine cancellation: errors.Is(err,
// context.Canceled) stays false for a StallError.
type StallError struct {
	Timeout time.Duration
}

func (e *StallError) Error() string {
	return fmt.Sprintf("transfer stalled: no data received for %s", e.Timeout)
}

// stallReader aborts a body read that stops delivering bytes. Each Read arms a
// timer; if the underlying read does not return within the timeout, the timer
// cancels the attempt context, which closes the connection (or HTTP/2 stream)
// and unblocks the read with an error. The outer job context is untouched, so
// the caller can retry from the current on-disk offset.
//
// The watchdog observes only the reader it wraps. In particular it should wrap
// the network body before any local pacing wrapper (SpeedLimiter), so that
// deliberate rate limiting is never mistaken for a stalled peer.
type stallReader struct {
	r       io.Reader
	timeout time.Duration
	cancel  context.CancelFunc

	mu sync.Mutex
	// fired records that the watchdog timer fired during THIS attempt. It is
	// set by the timer callback before the callback cancels the attempt, so
	// every error the cancellation subsequently produces is observable as
	// stall-caused. One fired watchdog marks the whole attempt: the timer
	// firing as data arrives still leaves the next read failing from this
	// attempt's own cancellation, which is stall-caused too.
	fired bool
	timer *time.Timer
}

// newStallReader wraps r so that a Read blocked longer than timeout cancels
// the attempt. timeout <= 0 disables the watchdog (Read passes straight
// through). cancel must be non-nil when the watchdog is active.
func newStallReader(r io.Reader, timeout time.Duration, cancel context.CancelFunc) *stallReader {
	return &stallReader{r: r, timeout: timeout, cancel: cancel}
}

func (s *stallReader) Read(p []byte) (int, error) {
	if s.timeout <= 0 || s.cancel == nil {
		return s.r.Read(p)
	}

	s.mu.Lock()
	s.timer = time.AfterFunc(s.timeout, s.fire)
	s.mu.Unlock()

	n, err := s.r.Read(p)

	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()
	return n, err
}

// fire is the watchdog timer callback. It records the firing BEFORE cancelling
// the attempt, so by the time the cancellation surfaces as a read error the
// flag is already observable. Setting the flag first also covers the race
// where the timer fires just as data arrives: the cancel still runs, and the
// next read fails from this attempt's own cancellation.
func (s *stallReader) fire() {
	s.mu.Lock()
	s.fired = true
	s.mu.Unlock()
	s.cancel()
}

// watchdogFired reports whether the watchdog fired during this attempt.
func (s *stallReader) watchdogFired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fired
}

// classifyStall translates an attempt-copy failure into an explicit
// StallError when the stall watchdog fired for that attempt. Precedence rule:
// if the parent (job/file/part) context is done, the failure is a genuine
// cancellation (user cancel, pause, shutdown, requeue) and the original error
// keeps its existing cancellation semantics and message; only a fired
// watchdog with a still-live parent is reported as a stall. err is returned
// unchanged whenever the watchdog never fired for this attempt, so unrelated
// failures (network, disk, HTTP status) are never relabelled as stalls.
func classifyStall(parent context.Context, r *stallReader, err error) error {
	if err == nil || r == nil || !r.watchdogFired() {
		return err
	}
	if parent != nil && parent.Err() != nil {
		return err
	}
	return &StallError{Timeout: r.timeout}
}
