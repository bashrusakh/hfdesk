// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

func TestRateLimiterUnlimited(t *testing.T) {
	l := NewRateLimiter(0)
	start := time.Now()
	if err := l.WaitN(context.Background(), 10<<20); err != nil {
		t.Fatalf("WaitN: %v", err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("unlimited WaitN blocked for %v", d)
	}
	if l.Limit() != 0 {
		t.Fatalf("Limit() = %d, want 0", l.Limit())
	}
}

// TestRateLimiterPaces checks that reading more than one bucket's worth of data
// through the limiter takes at least the expected time. With rate R and N bytes
// the transfer must take at least (N-burst)/R seconds.
func TestRateLimiterPaces(t *testing.T) {
	const rate = 512 * 1024 // 512 KiB/s (== initial burst)
	l := NewRateLimiter(rate)

	// Transfer one burst plus another half-second's worth. The bucket starts
	// full, so only the extra 256 KiB is gated: expected wait ~= 256/512 = 0.5s.
	const total = 768 * 1024
	src := bytes.NewReader(make([]byte, total))
	r := l.Reader(context.Background(), src)

	start := time.Now()
	n, err := io.Copy(io.Discard, r)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if n != total {
		t.Fatalf("copied %d bytes, want %d", n, total)
	}
	if elapsed < 400*time.Millisecond {
		t.Fatalf("transfer of %d B at %d B/s took only %v; limiter not pacing", total, rate, elapsed)
	}
}

func TestRateLimiterContextCancel(t *testing.T) {
	l := NewRateLimiter(1024) // very slow
	// Drain the initial bucket.
	_ = l.WaitN(context.Background(), minBurst)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.WaitN(ctx, 1<<20); err == nil {
		t.Fatal("WaitN should return ctx error when cancelled")
	}
}

func TestRateLimiterReaderNilPassthrough(t *testing.T) {
	var l *RateLimiter
	src := bytes.NewReader([]byte("hello"))
	if got := l.Reader(context.Background(), src); got != io.Reader(src) {
		t.Fatal("nil limiter Reader should return the original reader unchanged")
	}
}
