// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os/exec"
	"testing"
)

// TestCacheDetailResizeUI drives the shipped app.js Repository Details
// renderer inside Node's VM. It covers the resize grip availability, stored
// size restore/ignore, double-click reset, clamp bounds, keyboard steps, and
// pointer capture cleanup.
func TestCacheDetailResizeUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: run testdata/cache-detail-resize-ui.cjs with Node to verify dialog resizing")
	}

	cmd := exec.Command(node, "testdata/cache-detail-resize-ui.cjs")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cache detail resize regression: %v\n%s", err, out)
	}
	t.Log(string(out))
}
