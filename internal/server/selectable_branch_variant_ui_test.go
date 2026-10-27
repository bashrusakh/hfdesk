// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// The branch download control builds its request in the browser. The body it
// posts must pass the server's own branch-variant admission, so the harness
// reports the captured request and the server's real rule decides. A restated
// copy of the rule in the harness would only prove the restatement.
func TestSelectableBranchVariantDownloadAdmission(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: run testdata/selectable-status-ui.cjs with Node to verify the branch-variant download request")
	}

	cmd := exec.Command(node, "testdata/selectable-status-ui.cjs")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("selectable branch variant regression: %v\n%s", err, out)
	}

	const marker = "BRANCH_DOWNLOAD_REQUEST "
	var body string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, marker) {
			body = strings.TrimPrefix(line, marker)
			break
		}
	}
	if body == "" {
		t.Fatalf("harness did not report a branch download request:\n%s", out)
	}

	var req DownloadRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("branch download request is not a download body: %v\n%s", err, body)
	}
	// The admission only applies to a branch-variant request, so the flag and
	// the unfiltered, model-only shape are checked before it.
	if !req.BranchVariant {
		t.Fatalf("branch download request must select a whole branch:\n%s", body)
	}
	if req.Dataset {
		t.Fatalf("branch download request must not be a dataset download:\n%s", body)
	}
	if req.Revision == "" {
		t.Fatalf("branch download request must name the branch revision:\n%s", body)
	}
	if len(req.Filters) != 0 || len(req.Excludes) != 0 {
		t.Fatalf("branch download request must not select files:\n%s", body)
	}
	if req.LocalRepo != "" || req.AppendFilterSubdir {
		t.Fatalf("branch download request must not redirect its destination:\n%s", body)
	}
	if err := validateBranchVariantRequest(req); err != nil {
		t.Fatalf("branch download request must pass the server admission: %v\n%s", err, body)
	}
	t.Log(string(out))
}
