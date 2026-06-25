// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"io"
	"sync"
	"time"
)

// RateLimiter is a thread-safe token-bucket limiter that caps the aggregate
// download throughput in bytes per second. A single RateLimiter is shared by
// every concurrent connection and file in a download (and, in the server, by
// every running job), so the *total* bandwidth never exceeds the configured
// rate regardless of how many transfers run in parallel.
//
// A rate of 0 (or negative) means unlimited: WaitN returns immediately. The
// limit can be changed at runtime with SetLimit, so the server can apply a new
// setting without restarting in-flight downloads.
type RateLimiter struct {
	mu     sync.Mutex
	rate   float64   // bytes per second; <= 0 means unlimited
	burst  float64   // bucket capacity in bytes
	tokens float64   // currently available tokens (bytes)
	last   time.Time // last time tokens were refilled
}

// minBurst keeps the bucket large enough to admit a full read chunk even when
// the configured rate is very small, so WaitN never deadlocks waiting for more
// tokens than the bucket can ever hold.
const minBurst = 64 * 1024

// NewRateLimiter returns a limiter capping throughput at bytesPerSec. Pass 0
// for unlimited (the limiter can be switched on later with SetLimit).
func NewRateLimiter(bytesPerSec int64) *RateLimiter {
	l := &RateLimiter{last: time.Now()}
	l.SetLimit(bytesPerSec)
	l.mu.Lock()
	l.tokens = l.burst
	l.mu.Unlock()
	return l
}

// SetLimit changes the throughput cap to bytesPerSec (0 = unlimited). It is
// safe to call concurrently with active transfers.
func (l *RateLimiter) SetLimit(bytesPerSec int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rate = float64(bytesPerSec)
	if bytesPerSec > 0 {
		l.burst = float64(bytesPerSec)
		if l.burst < minBurst {
			l.burst = minBurst
		}
	} else {
		l.burst = 0
	}
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
}

// Limit reports the current cap in bytes per second (0 = unlimited).
func (l *RateLimiter) Limit() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(l.rate)
}

// WaitN blocks until n bytes' worth of tokens are available and consumes them,
// or until ctx is cancelled. When the limiter is unlimited it returns at once.
func (l *RateLimiter) WaitN(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	for {
		l.mu.Lock()
		if l.rate <= 0 { // unlimited
			l.mu.Unlock()
			return nil
		}
		now := time.Now()
		if elapsed := now.Sub(l.last).Seconds(); elapsed > 0 {
			l.tokens += elapsed * l.rate
			if l.tokens > l.burst {
				l.tokens = l.burst
			}
			l.last = now
		}
		need := float64(n)
		if need > l.burst {
			need = l.burst // n is capped to burst by the reader; guard anyway
		}
		if l.tokens >= need {
			l.tokens -= need
			l.mu.Unlock()
			return nil
		}
		deficit := need - l.tokens
		wait := time.Duration(deficit / l.rate * float64(time.Second))
		l.mu.Unlock()

		if wait <= 0 {
			wait = time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Reader wraps r so that reading from it is paced by the limiter. If l is nil
// the original reader is returned unchanged.
func (l *RateLimiter) Reader(ctx context.Context, r io.Reader) io.Reader {
	if l == nil {
		return r
	}
	return &rateLimitedReader{ctx: ctx, r: r, l: l}
}

// rateLimitedReader paces an underlying reader through a RateLimiter. Each Read
// is capped to a modest chunk so the limiter can pace transfers smoothly even
// at low rates, instead of reading a large buffer and then sleeping for a long
// stretch.
type rateLimitedReader struct {
	ctx context.Context
	r   io.Reader
	l   *RateLimiter
}

// readChunk bounds a single underlying Read so pacing stays fine-grained and
// always fits within the limiter's bucket (readChunk <= minBurst <= burst).
const readChunk = 16 * 1024

func (rr *rateLimitedReader) Read(p []byte) (int, error) {
	// Unlimited: pass through without chunking or pacing. The limit is checked
	// per Read, so a runtime SetLimit still takes effect on the next read.
	if rr.l.Limit() <= 0 {
		return rr.r.Read(p)
	}
	if len(p) > readChunk {
		p = p[:readChunk]
	}
	n, err := rr.r.Read(p)
	if n > 0 {
		if werr := rr.l.WaitN(rr.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// ParseSpeed parses a human-readable bytes-per-second string ("2MB", "500KB",
// "1.5MiB", "0") into a byte count. Empty, "0", or malformed input yields 0,
// meaning unlimited.
func ParseSpeed(s string) int64 {
	n, err := parseSizeString(s, 0)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
