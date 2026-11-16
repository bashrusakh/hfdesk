// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os/exec"
	"testing"
)

// TestDownloadPhaseUI drives the shipped app.js job-card render/update path
// with Node's built-in VM. The harness exercises the real createJobCard and
// updateJobCard for the preparing/finalizing local phases, the unchanged
// transferring row, terminal jobs, unknown phase values, and node stability.
func TestDownloadPhaseUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: run testdata/download-phase-ui.cjs with Node to verify download phase rendering")
	}

	cmd := exec.Command(node, "testdata/download-phase-ui.cjs")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("download phase row regression: %v\n%s", err, out)
	}
	t.Log(string(out))
}
