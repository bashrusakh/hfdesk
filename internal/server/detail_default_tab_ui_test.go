// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os/exec"
	"testing"
)

// TestDetailDefaultTabUI drives the shipped app.js analysis renderer inside
// Node's VM. The default Model Details tab is decided by whether the analysis
// is recognized, not by whether it produced selectable items: a plain
// Transformers base model with no selectable items opens on Overview, and only
// an unrecognized/generic repository opens on Files.
func TestDetailDefaultTabUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: run testdata/detail-default-tab-ui.cjs with Node to verify the default detail tab")
	}

	cmd := exec.Command(node, "testdata/detail-default-tab-ui.cjs")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("detail default tab regression: %v\n%s", err, out)
	}
	t.Log(string(out))
}
