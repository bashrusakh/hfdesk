// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// Contract pin: the job snapshot carries appendFilterSubdir on the API —
// in the POST /api/download response and via GET /api/jobs — the flag
// round-trips through jobs_state.json, and an older state file written
// before the field existed loads it as false.
func TestJobSnapshotAndStateCarryAppendFilterSubdir(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unexpected hub request", http.StatusInternalServerError)
	}))
	defer hub.Close()

	s := newTestServerWithConfig(t, Config{
		CacheDir: t.TempDir(),
		Endpoint: hub.URL,
		Proxy:    &hfdownloader.ProxyConfig{NoEnvProxy: true},
	})
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, err := json.Marshal(DownloadRequest{
		Repo: "owner/model", Filters: []string{"q4_k_m"},
		ExactMatch: true, AppendFilterSubdir: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/download", bytes.NewReader(body)))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d, want 202; body=%s", w.Code, w.Body.String())
	}
	var admitted Job
	if err := json.Unmarshal(w.Body.Bytes(), &admitted); err != nil {
		t.Fatal(err)
	}
	if !admitted.AppendFilterSubdir {
		t.Fatalf("POST /api/download snapshot dropped appendFilterSubdir: %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
	var list struct {
		Jobs []*Job `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	listed := findJobByID(list.Jobs, admitted.ID)
	if listed == nil || !listed.AppendFilterSubdir {
		t.Fatalf("GET /api/jobs snapshot=%+v, want appendFilterSubdir on job %s", listed, admitted.ID)
	}

	statePath := filepath.Join(t.TempDir(), "jobs_state.json")
	if err := saveJobsState(statePath, s.jobs.ListJobs()); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadJobsState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	restored := findJobByID(loaded, admitted.ID)
	if restored == nil || !restored.AppendFilterSubdir {
		t.Fatalf("jobs_state.json round trip=%+v, want appendFilterSubdir preserved", restored)
	}

	// A state file written before the field existed loads as false.
	legacy := []byte(`{"jobs":[{"id":"legacy","repo":"owner/model","revision":"main","outputDir":"/tmp/legacy","status":"queued","createdAt":"2026-01-01T00:00:00Z"}]}`)
	if err := os.WriteFile(statePath, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadJobsState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	legacyJob := findJobByID(loaded, "legacy")
	if legacyJob == nil {
		t.Fatalf("legacy state did not load: %+v", loaded)
	}
	if legacyJob.AppendFilterSubdir {
		t.Fatal("legacy state without appendFilterSubdir must load the field as false")
	}
}

// Contract pin: appendFilterSubdir joins the write-decision identity. The
// descriptor key splits on the flag alone, decisionJob carries it,
// jobFromDecision reconstructs it for every consumer of a descriptor, and
// the frozen captureWriteDecisionLocked snapshot keeps it — so two decisions
// differing only in the flag never compare equal.
func TestWriteDecisionIdentitySplitsOnAppendFilterSubdir(t *testing.T) {
	base := Job{
		ID: "same-id", Repo: "owner/model", Revision: "main",
		OutputDir: "/tmp/out", Status: JobStatusQueued, CreatedAt: time.Unix(0, 0),
	}
	plain, flagged := base, base
	flagged.AppendFilterSubdir = true

	if decisionDescriptorKey(decisionJob(&plain)) == decisionDescriptorKey(decisionJob(&flagged)) {
		t.Fatal("write-decision descriptor identity ignores appendFilterSubdir; two layouts would share one decision")
	}
	if !jobFromDecision(decisionJob(&flagged)).AppendFilterSubdir {
		t.Fatal("jobFromDecision drops appendFilterSubdir; descriptor consumers would model the wrong write layout")
	}

	// The frozen snapshot keeps the flag: the captured descriptor of a live
	// flagged job differs from the same descriptor with the flag cleared.
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{CacheDir: root}, nil, filepath.Join(root, "state"))
	t.Cleanup(func() {
		m.mu.Lock()
		m.fenced = true
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	live := flagged
	live.ID = "live"
	live.OutputDir = root
	live.partialFilesMu = &sync.Mutex{}
	m.mu.Lock()
	m.fenced = true
	m.jobs[live.ID] = &live
	snapshot := m.captureWriteDecisionLocked()
	m.mu.Unlock()
	var captured *writeDecisionJob
	for i := range snapshot.jobs {
		if snapshot.jobs[i].id == live.ID {
			captured = &snapshot.jobs[i]
		}
	}
	if captured == nil {
		t.Fatalf("captured snapshot lacks job %s: %+v", live.ID, snapshot.jobs)
	}
	cleared := *captured
	cleared.appendFilterSubdir = false
	if decisionDescriptorKey(*captured) == decisionDescriptorKey(cleared) {
		t.Fatal("captureWriteDecisionLocked freezes a descriptor whose key ignores appendFilterSubdir")
	}
}
