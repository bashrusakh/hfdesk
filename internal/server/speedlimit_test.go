// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import "testing"

// TestJobManagerSpeedLimiterInit verifies the shared limiter is created from
// the initial config's MaxSpeed.
func TestJobManagerSpeedLimiterInit(t *testing.T) {
	m := NewJobManager(Config{MaxSpeed: "250KB"}, nil)
	if m.speedLimiter == nil {
		t.Fatal("speedLimiter should be non-nil")
	}
	if got := m.speedLimiter.Limit(); got != 250000 {
		t.Fatalf("initial Limit() = %d, want 250000", got)
	}
}

// TestJobManagerSpeedLimiterLiveUpdate verifies that UpdateConfig applies a new
// cap to the shared limiter on the fly (the path POST /api/settings drives).
func TestJobManagerSpeedLimiterLiveUpdate(t *testing.T) {
	m := NewJobManager(Config{}, nil)
	limiter := m.speedLimiter
	if got := m.speedLimiter.Limit(); got != 0 {
		t.Fatalf("default Limit() = %d, want 0 (unlimited)", got)
	}

	// 2 Mbit/s == 250 KB/s, exactly what the UI sends for the "2" preset.
	m.UpdateConfig(Config{MaxSpeed: "250KB"})
	if m.speedLimiter != limiter {
		t.Fatal("speedLimiter should be updated in place, not replaced")
	}
	if got := m.speedLimiter.Limit(); got != 250000 {
		t.Fatalf("after raise Limit() = %d, want 250000", got)
	}

	// Back to unlimited.
	m.UpdateConfig(Config{MaxSpeed: ""})
	if got := m.speedLimiter.Limit(); got != 0 {
		t.Fatalf("after clear Limit() = %d, want 0", got)
	}
}
