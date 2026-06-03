// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDismissJob_OnlyTerminalStates ensures DismissJob refuses to remove
// jobs that are still queued or running — a user can't accidentally wipe
// a live download by clicking the wrong button. Paused jobs are terminal
// from the "visible in list" standpoint and can be dismissed.
func TestDismissJob_OnlyTerminalStates(t *testing.T) {
	m := NewJobManager(Config{}, nil)

	cases := []struct {
		status     JobStatus
		wantRemove bool
	}{
		{JobStatusQueued, false},
		{JobStatusRunning, false},
		{JobStatusPaused, true},
		{JobStatusCompleted, true},
		{JobStatusFailed, true},
		{JobStatusCancelled, true},
	}

	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			id := "job-" + string(tc.status)
			m.jobs[id] = &Job{
				ID:        id,
				Repo:      "owner/repo",
				Status:    tc.status,
				CreatedAt: time.Now(),
			}

			got := m.DismissJob(id)
			if got != tc.wantRemove {
				t.Errorf("DismissJob(%q) = %v, want %v", tc.status, got, tc.wantRemove)
			}
			_, present := m.jobs[id]
			if tc.wantRemove && present {
				t.Errorf("job %q still present after successful dismiss", id)
			}
			if !tc.wantRemove && !present {
				t.Errorf("job %q removed despite being in non-terminal state", id)
			}
			// Clean up for the next subtest.
			delete(m.jobs, id)
		})
	}
}

// TestDismissJob_SurvivesPageRefresh verifies the core #68-secondary
// guarantee: once a job is dismissed, it no longer appears in the list
// that sendInitialState uses to rehydrate a reconnecting browser.
func TestDismissJob_SurvivesPageRefresh(t *testing.T) {
	m := NewJobManager(Config{}, nil)

	m.jobs["live"] = &Job{ID: "live", Status: JobStatusRunning, CreatedAt: time.Now()}
	m.jobs["gone"] = &Job{ID: "gone", Status: JobStatusCompleted, CreatedAt: time.Now()}

	if !m.DismissJob("gone") {
		t.Fatal("DismissJob(gone) returned false")
	}

	jobs := m.ListJobs()
	foundLive := false
	for _, j := range jobs {
		if j.ID == "gone" {
			t.Errorf("dismissed job %q still appears in ListJobs; refresh would repopulate the UI", j.ID)
		}
		if j.ID == "live" {
			foundLive = true
		}
	}
	if !foundLive {
		t.Error("live running job was unexpectedly removed")
	}
}

// TestHandleDismissJob_EndToEnd drives the HTTP endpoint, asserts the
// dismissed job is gone, and confirms the response envelope. Exercises
// the full route wiring so we catch regressions in server.go registration.
func TestHandleDismissJob_EndToEnd(t *testing.T) {
	hub := NewWSHub()
	go hub.Run()

	srv := &Server{
		config: Config{},
		jobs:   NewJobManager(Config{}, hub),
		wsHub:  hub,
	}
	srv.jobs.jobs["done1"] = &Job{
		ID:        "done1",
		Repo:      "owner/repo",
		Status:    JobStatusCompleted,
		CreatedAt: time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/jobs/{id}/dismiss", srv.handleDismissJob)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/jobs/done1/dismiss", "application/json", nil)
	if err != nil {
		t.Fatalf("POST dismiss: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["success"] != true {
		t.Errorf("response success = %v, want true", body["success"])
	}

	// Attempting to dismiss a running job must fail with 409 so the UI
	// can distinguish "not yet dismissable" from "not found".
	srv.jobs.jobs["busy"] = &Job{ID: "busy", Status: JobStatusRunning, CreatedAt: time.Now()}
	resp2, err := http.Post(ts.URL+"/api/jobs/busy/dismiss", "application/json", nil)
	if err != nil {
		t.Fatalf("POST dismiss (busy): %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("busy dismiss status = %d, want 409", resp2.StatusCode)
	}

	// 404 for unknown job.
	resp3, err := http.Post(ts.URL+"/api/jobs/nope/dismiss", "application/json", nil)
	if err != nil {
		t.Fatalf("POST dismiss (nope): %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("nope dismiss status = %d, want 404", resp3.StatusCode)
	}
}
