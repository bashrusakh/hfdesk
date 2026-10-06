// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
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

	mu    sync.Mutex
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
	s.timer = time.AfterFunc(s.timeout, s.cancel)
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
