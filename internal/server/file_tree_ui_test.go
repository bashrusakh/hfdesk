// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os/exec"
	"testing"
)

// Node's built-in VM drives the shipped app.js Files-tab file tree with DOM,
// localStorage and fetch stubs. The harness exercises the real build/reset,
// directory tri-state selection, strict "Download selected" request body,
// existing-job message handling and per-file status mapping rather than
// reimplementing them here.
func TestFileTreeUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: run testdata/file-tree-ui.cjs with Node to verify the Files tab tree")
	}

	cmd := exec.Command(node, "testdata/file-tree-ui.cjs")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("file tree UI regression: %v\n%s", err, out)
	}
	t.Log(string(out))
}
