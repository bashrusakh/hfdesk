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

// TestRateLimiterSetLimitWakesWaiters verifies that a goroutine blocked in
// WaitN is woken by a concurrent SetLimit, instead of having to wait out the
// originally-computed deficit/rate timer.
func TestRateLimiterSetLimitWakesWaiters(t *testing.T) {
	const initialRate = 1024 // 1 KiB/s
	l := NewRateLimiter(initialRate)
	// Drain the bucket so the next WaitN has to block.
	if err := l.WaitN(context.Background(), minBurst); err != nil {
		t.Fatalf("drain WaitN: %v", err)
	}

	// Try to wait for 10 MiB at 1 KiB/s. At the old rate that would take
	// ~10000 seconds; we expect SetLimit(unlimited) to wake us in well
	// under a second.
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_ = l.WaitN(context.Background(), 10<<20)
		done <- time.Since(start)
	}()

	// Give the goroutine a moment to register as a waiter and start
	// blocking on the timer.
	time.Sleep(50 * time.Millisecond)
	l.SetLimit(0) // unlimited — must wake the blocked WaitN.

	select {
	case elapsed := <-done:
		if elapsed > 500*time.Millisecond {
			t.Errorf("WaitN took %v after SetLimit(unlimited); should be woken near-instantly", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitN did not return within 2s after SetLimit — wakeup is not working")
	}

	// And a follow-up WaitN should now return immediately, since the rate
	// is unlimited.
	if err := l.WaitN(context.Background(), 10<<20); err != nil {
		t.Errorf("post-wakeup WaitN: %v", err)
	}
}

// TestRateLimiterWaitersDoNotLeak guards against an earlier bug where the
// normal-timer wakeup branch did not remove its entry from the waiters
// slice: SetLimit only clears the slice wholesale, so without the per-wakeup
// removal a long-lived rate-limited transfer would accumulate one stale
// channel per timer wakeup per goroutine.
//
// Each call below exercises the timer-fire path (no SetLimit, no ctx cancel):
// 1 KiB/s rate with an empty bucket means the 1 KiB request waits ~1 s for
// the timer to fire and refill. With the bug, the waiter entry would be
// left behind on every iteration.
func TestRateLimiterWaitersDoNotLeak(t *testing.T) {
	l := NewRateLimiter(1024) // 1 KiB/s
	// Drain the bucket so the next WaitN has to wait for the timer.
	if err := l.WaitN(context.Background(), minBurst); err != nil {
		t.Fatalf("drain: %v", err)
	}

	const iters = 2
	for i := 0; i < iters; i++ {
		if err := l.WaitN(context.Background(), 1024); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}

	l.mu.Lock()
	waiters := len(l.waiters)
	l.mu.Unlock()
	if waiters != 0 {
		t.Errorf("after %d successful WaitN calls, waiters slice has %d entries; want 0", iters, waiters)
	}
}

// TestRateLimiterContextCancellationAfterWakeup guards against a select
// race: when SetLimit closes the wakeup channel at the same instant the
// caller's context is cancelled, Go's select picks one ready case
// non-deterministically. If the wakeup case wins, the goroutine must still
// honor the cancellation before returning nil.
func TestRateLimiterContextCancellationAfterWakeup(t *testing.T) {
	const initialRate = 1024
	l := NewRateLimiter(initialRate)
	// Drain the bucket so the next WaitN has to block.
	if err := l.WaitN(context.Background(), minBurst); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// Run the race many times to make the non-deterministic path
	// likely. The previous bug silently returned nil on every iteration
	// where the select happened to pick <-wakeup while ctx was already
	// cancelled.
	const iters = 50
	for i := 0; i < iters; i++ {
		// Reset to the slow rate so WaitN actually blocks this iteration
		// (the previous iteration ended with rate=0 from the
		// SetLimit(unlimited) call below).
		l.SetLimit(initialRate)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- l.WaitN(ctx, 10<<20) }()

		// Wait until the goroutine has actually registered a waiter,
		// so SetLimit(0) below can't win the race and let WaitN return
		// a legitimate nil via the unlimited fast-path. A fixed
		// time.Sleep is flaky under CI scheduling pressure.
		for {
			l.mu.Lock()
			registered := len(l.waiters) > 0
			l.mu.Unlock()
			if registered {
				break
			}
			time.Sleep(50 * time.Microsecond)
		}

		// Cancel the context FIRST, then raise the rate. The previous
		// bug: when SetLimit closes the wakeup channel and ctx is
		// already cancelled, the select picks one of <wakeup> or
		// <ctx.Done> at random; if <wakeup> wins, WaitN must still
		// honor the cancellation. (If SetLimit runs first, the wakeup
		// fires before ctx is cancelled and the goroutine returns nil
		// legitimately — there is no race to catch in that order.)
		cancel()
		l.SetLimit(0)

		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("iter %d: WaitN returned nil despite ctx cancellation racing SetLimit wakeup", i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: WaitN did not return within 2s", i)
		}
	}
}

func TestParseSizeStrict(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		// Unlimited / unset.
		{"", 0, false},
		{"   ", 0, false},
		{"0", 0, false},
		{"0KB", 0, false},
		{"0  ", 0, false},
		// Valid sizes.
		{"500", 500, false},
		{"500B", 500, false},
		{"2KB", 2000, false},
		{"2MB", 2_000_000, false},
		{"1.5MiB", 1_572_864, false},
		{"  1GB  ", 1_000_000_000, false},
		// Invalid.
		{"abc", 0, true},
		{"5xyz", 0, true},
		{"MB", 0, true},
		{"-1", 0, true},
	}
	for _, c := range cases {
		got, err := ParseSizeStrict(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseSizeStrict(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
		if !c.wantErr && got != c.want {
			t.Errorf("ParseSizeStrict(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
