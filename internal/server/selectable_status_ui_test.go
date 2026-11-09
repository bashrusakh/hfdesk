// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os/exec"
	"testing"
)

// Node's built-in VM drives the shipped app.js artifact status reconciliation
// with DOM, localStorage and cache-fetch stubs. The harness exercises the real
// refreshDlStatusFromJobs / syncLocalAnalysisStatus / renderSelectableItems
// sequence rather than reimplementing its completion rules.
func TestSelectableArtifactStatusUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: run testdata/selectable-status-ui.cjs with Node to verify artifact status reconciliation")
	}

	cmd := exec.Command(node, "testdata/selectable-status-ui.cjs")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("selectable artifact status regression: %v\n%s", err, out)
	}
	t.Log(string(out))
}
