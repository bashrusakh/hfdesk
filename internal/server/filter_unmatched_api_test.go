// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// filterUnmatchedRequest is the shared fixture request: one matched
// extension selector plus one pattern the fixture tree does not contain.
func filterUnmatchedRequest() DownloadRequest {
	return DownloadRequest{
		Repo: "owner/model", Revision: "main",
		Filters: []string{".safetensors", "q4"}, ExactMatch: true,
	}
}

// TestFilterUnmatchedPlanDryRunWarnings: POST /api/plan and dry-run
// POST /api/download carry the typed filters_unmatched warning naming only
// the unmatched pattern through the established Plan.Warnings plumbing, a
// fully matched filter set stays warning-free, and a total-zero selection
// keeps the no_filter_match 400 instead of ever becoming a warning.
func TestFilterUnmatchedPlanDryRunWarnings(t *testing.T) {
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
		if len(resp.Warnings) != 1 ||
			resp.Warnings[0].Code != hfdownloader.PlanWarningFiltersUnmatched ||
			strings.Join(resp.Warnings[0].Segments, ",") != "q4" {
			t.Fatalf("%s warnings = %+v, want one filters_unmatched [q4]", path, resp.Warnings)
		}
		if resp.Warnings[0].Message != `filter "q4" matched nothing` {
			t.Errorf("%s message = %q, want %q", path, resp.Warnings[0].Message, `filter "q4" matched nothing`)
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
		if len(resp.Warnings) != 0 {
			t.Errorf("%s warnings = %+v, want none for a fully matched filter set", path, resp.Warnings)
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
		if strings.Contains(raw, "warnings") {
			t.Errorf("%s total-zero response carries warnings: %s", path, raw)
		}
	}
}

// TestFilterUnmatchedWarningOnJobSnapshot: a real download whose plan
// reports filters_unmatched exposes the warning on the job snapshot served
// by GET /api/jobs (and in the jobs_state.json JSON shape), the warning is
// an output rather than identity — a fresh identical request starts an
// independent job with no warnings carried over — and the job still
// completes: the warning is non-blocking.
func TestFilterUnmatchedWarningOnJobSnapshot(t *testing.T) {
	hub := shardRangeHub(shardRangeContent())
	defer hub.Close()
	root := t.TempDir()
	s, mux := newShardRangeServer(t, hub, Config{LocalDir: root, MaxActive: 1})

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
	if len(finished.Warnings) != 1 ||
		finished.Warnings[0].Code != hfdownloader.PlanWarningFiltersUnmatched ||
		strings.Join(finished.Warnings[0].Segments, ",") != "q4" {
		t.Fatalf("job snapshot warnings = %+v, want one filters_unmatched [q4]", finished.Warnings)
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
		if len(job.Warnings) != 1 || job.Warnings[0].Code != hfdownloader.PlanWarningFiltersUnmatched {
			t.Errorf("/api/jobs warnings = %+v, want one filters_unmatched", job.Warnings)
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
		!strings.Contains(string(encoded), `filters_unmatched`) {
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
