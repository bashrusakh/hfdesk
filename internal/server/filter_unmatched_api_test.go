// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// filterUnmatchedRequest is the shared fixture request: one matched
// extension selector plus one pattern the fixture tree does not contain.
func filterUnmatchedRequest() DownloadRequest {
	return DownloadRequest{
		Repo: "owner/model", Revision: "main",
		Filters: []string{".safetensors", "q4"}, ExactMatch: true,
	}
}

// filterNotices extracts the filters_unmatched sentences from a plan's
// notices (the `filter "…" matched nothing` / `filters "… matched nothing`
// shapes; split-mention notices start with "excluded" and shard notices
// with "shard").
func filterNotices(notices []string) []string {
	var found []string
	for _, n := range notices {
		if strings.HasPrefix(n, `filter "`) || strings.HasPrefix(n, `filters "`) {
			found = append(found, n)
		}
	}
	return found
}

// TestFilterUnmatchedPlanDryRunNotices: POST /api/plan and dry-run
// POST /api/download carry the filters_unmatched notice naming only the
// unmatched pattern through the established Plan.Notices plumbing, a fully
// matched filter set stays free of such notices, and a total-zero
// selection keeps the no_filter_match 400 instead of ever becoming a
// notice.
func TestFilterUnmatchedPlanDryRunNotices(t *testing.T) {
	hub := shardRangeHub(shardRangeContent())
	defer hub.Close()
	_, mux := newShardRangeServer(t, hub, Config{})

	warned := filterUnmatchedRequest()
	for _, path := range []string{"/api/plan", "/api/download"} {
		req := warned
		req.DryRun = path == "/api/download"
		code, resp, raw := postPlanResponse(t, mux, path, req)
		if code != http.StatusOK {
			t.Fatalf("%s status=%d, want 200; body=%s", path, code, raw)
		}
		found := filterNotices(resp.Notices)
		if len(found) != 1 {
			t.Fatalf("%s notices = %q, want exactly one filters_unmatched notice", path, resp.Notices)
		}
		if found[0] != `filter "q4" matched nothing` {
			t.Errorf("%s notice = %q, want %q", path, found[0], `filter "q4" matched nothing`)
		}
	}

	matched := DownloadRequest{
		Repo: "owner/model", Revision: "main",
		Filters: []string{".safetensors"}, ExactMatch: true,
	}
	for _, path := range []string{"/api/plan", "/api/download"} {
		req := matched
		req.DryRun = path == "/api/download"
		code, resp, raw := postPlanResponse(t, mux, path, req)
		if code != http.StatusOK {
			t.Fatalf("%s status=%d, want 200; body=%s", path, code, raw)
		}
		if len(resp.Notices) != 0 {
			t.Errorf("%s notices = %q, want none for a fully matched filter set", path, resp.Notices)
		}
	}

	zero := DownloadRequest{
		Repo: "owner/model", Revision: "main",
		Filters: []string{"absent"}, ExactMatch: true,
	}
	for _, path := range []string{"/api/plan", "/api/download"} {
		req := zero
		req.DryRun = path == "/api/download"
		code, _, raw := postPlanResponse(t, mux, path, req)
		if code != http.StatusBadRequest {
			t.Fatalf("%s total-zero status=%d, want 400; body=%s", path, code, raw)
		}
		var errResp ErrorResponse
		if err := json.Unmarshal([]byte(raw), &errResp); err != nil {
			t.Fatal(err)
		}
		if errResp.ErrorDetail == nil || errResp.ErrorDetail.Code != "no_filter_match" {
			t.Errorf("%s error_detail=%+v, want code no_filter_match; body=%s", path, errResp.ErrorDetail, raw)
		}
		if strings.Contains(raw, "notices") {
			t.Errorf("%s total-zero response carries notices: %s", path, raw)
		}
	}
}

// TestFilterUnmatchedNoticeOnJobSnapshot: a real download whose plan
// reports filters_unmatched exposes the SAME sentence on the job snapshot
// served by GET /api/jobs (and in the jobs_state.json JSON shape) through
// main's notices -> warning events -> Job.Warnings carriage — the exact
// message the plan notices carried — the warning is an output rather than
// identity (a fresh identical request starts an independent job with no
// warnings carried over), and the job still completes: the notice is
// non-blocking. This is the end-to-end both-surfaces proof: the plan
// response above and this job snapshot carry the identical sentence.
func TestFilterUnmatchedNoticeOnJobSnapshot(t *testing.T) {
	hub := shardRangeHub(shardRangeContent())
	defer hub.Close()
	root := t.TempDir()
	s, mux := newShardRangeServer(t, hub, Config{LocalDir: root, MaxActive: 1})

	const want = `filter "q4" matched nothing`

	body, err := json.Marshal(filterUnmatchedRequest())
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/download", bytes.NewReader(body)))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d, want 202 admitted job; body=%s", w.Code, w.Body.String())
	}
	var admitted Job
	if err := json.Unmarshal(w.Body.Bytes(), &admitted); err != nil {
		t.Fatal(err)
	}
	if len(admitted.Warnings) != 0 {
		t.Errorf("admitted snapshot already carries warnings %+v; the plan has not run yet", admitted.Warnings)
	}
	finished := waitJobStatus(t, s.jobs, admitted.ID, JobStatusCompleted, 10*time.Second)
	if finished.Status != JobStatusCompleted {
		t.Fatalf("job status=%s err=%q; want completed (warnings must not block)", finished.Status, finished.Error)
	}
	if !slices.Contains(finished.Warnings, want) {
		t.Fatalf("job snapshot warnings = %q, want the exact plan notice %q", finished.Warnings, want)
	}
	if n := len(filterNotices(finished.Warnings)); n != 1 {
		t.Errorf("job snapshot carries %d filters_unmatched notices %q, want exactly one (no double-reporting)",
			n, finished.Warnings)
	}

	// /api/jobs serves the same field to clients.
	jw := httptest.NewRecorder()
	mux.ServeHTTP(jw, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
	if jw.Code != http.StatusOK {
		t.Fatalf("GET /api/jobs status=%d; body=%s", jw.Code, jw.Body.String())
	}
	var listed struct {
		Jobs []Job `json:"jobs"`
	}
	if err := json.Unmarshal(jw.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, job := range listed.Jobs {
		if job.ID != admitted.ID {
			continue
		}
		seen = true
		if !slices.Contains(job.Warnings, want) {
			t.Errorf("/api/jobs warnings = %q, want the exact plan notice %q", job.Warnings, want)
		}
	}
	if !seen {
		t.Errorf("job %s missing from /api/jobs", admitted.ID)
	}

	// The jobs_state.json shape is the job's own JSON encoding.
	encoded, err := json.Marshal(finished)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"warnings"`) ||
		!strings.Contains(string(encoded), `filter \"q4\" matched nothing`) {
		t.Errorf("persisted job JSON lacks the warnings surface: %s", encoded)
	}

	// Warnings are outputs, not identity: an identical request after
	// completion starts a fresh job whose snapshot carries none.
	fresh, existing, err := s.jobs.CreateJob(filterUnmatchedRequest())
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Errorf("warnings must not change identity: identical request to a finished job deduped")
	}
	if len(fresh.Warnings) != 0 {
		t.Errorf("fresh job snapshot carries warnings %+v, want none", fresh.Warnings)
	}
}
