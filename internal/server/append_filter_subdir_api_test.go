// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

func TestPlanAppendFilterSubdirPathFilterMapsToBadRequest(t *testing.T) {
	var requests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected hub request", http.StatusInternalServerError)
	}))
	defer hub.Close()

	s := newTestServerWithConfig(t, Config{
		Endpoint:  hub.URL,
		CacheDir:  t.TempDir(),
		ModelsDir: t.TempDir(),
		Proxy:     &hfdownloader.ProxyConfig{NoEnvProxy: true},
	})
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	for _, route := range []struct {
		name string
		path string
		dry  bool
	}{
		{name: "plan", path: "/api/plan"},
		{name: "download dry-run", path: "/api/download", dry: true},
	} {
		t.Run(route.name, func(t *testing.T) {
			body, err := json.Marshal(DownloadRequest{
				Repo: "owner/model", Filters: []string{"weights/model.gguf"},
				ExactMatch: true, AppendFilterSubdir: true, DryRun: route.dry,
			})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, route.path, bytes.NewReader(body)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode error response: %v; body=%s", err, w.Body.String())
			}
			if resp.ErrorDetail == nil || resp.ErrorDetail.Code != "invalid_append_filter_subdir" {
				t.Fatalf("error_detail=%+v, want code invalid_append_filter_subdir; body=%s", resp.ErrorDetail, w.Body.String())
			}
			if resp.Error != "AppendFilterSubdir cannot be combined with path-shaped filters" {
				t.Errorf("error=%q, want the documented append-filter message", resp.Error)
			}
			if resp.ErrorDetail.Details == "" || !strings.Contains(resp.ErrorDetail.Details, "weights/model.gguf") {
				t.Errorf("details=%q, want the offending filter included", resp.ErrorDetail.Details)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("rejected requests reached the hub %d time(s), want none", got)
	}
}

func TestAppendFilterSubdirBoundaryConsumesLibraryRule(t *testing.T) {
	req := DownloadRequest{AppendFilterSubdir: true, Filters: []string{"weights/model.gguf"}}
	err := validateAppendFilterSubdirRequest(req)
	want := hfdownloader.ValidateAppendFilterSubdirFilters(req.Filters)
	if err == nil || err.Error() != want.Error() || !errors.Is(err, hfdownloader.ErrAppendFilterSubdirPathFilter) {
		t.Fatalf("boundary error=%v, want the library owner's answer %v", err, want)
	}
}

func TestPlanAppendFilterSubdirSingleComponentStillWorks(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"deadbeef"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte(`[{"type":"file","path":"model-Q4_K_M.gguf","size":10}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	s := newTestServerWithConfig(t, Config{
		Endpoint:  hub.URL,
		CacheDir:  t.TempDir(),
		ModelsDir: t.TempDir(),
		Proxy:     &hfdownloader.ProxyConfig{NoEnvProxy: true},
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
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/plan", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	var plan PlanResponse
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 1 || plan.Files[0].Path != "model-Q4_K_M.gguf" {
		t.Fatalf("plan files=%+v, want repository-relative model-Q4_K_M.gguf", plan.Files)
	}
}

// Contract pin: with appendFilterSubdir applied, the write/deletion
// interlock guards the paths the job really writes — the PREFIXED
// flat/friendly destination and the UNPREFIXED HF snapshot path — and does
// not guard the phantom unprefixed flat/friendly path. Before the flag was
// wired through runJob, the plan enumerated unprefixed destinations: DELETE
// of the real prefixed target would be admitted (the 409/DELETE mismatch)
// while the phantom was refused instead.
func TestAppendFilterSubdirInterlockGuardsPrefixedWrites(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{CacheDir: root}, nil, filepath.Join(root, "state"))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	m.mu.Lock()
	m.fenced = true // Keep scheduler execution outside this interlock test.
	m.mu.Unlock()

	created, existing, err := m.CreateJob(DownloadRequest{
		Repo: "owner/model", Filters: []string{"q4_k_m"},
		ExactMatch: true, AppendFilterSubdir: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first request should create a new job")
	}
	if len(m.runActivities) != 0 {
		t.Fatal("fenced scheduler registered a run activity")
	}
	live := m.jobs[created.ID]

	// The planner's own attribution: the winning filter is each item's
	// destination prefix, exactly as hfdownloader composes finalRel.
	plan := hfdownloader.Plan{Commit: "deadbeef", Items: []hfdownloader.PlanItem{
		{RelativePath: "model-Q4_K_M.gguf", Size: 4, DestinationSubdir: "q4_k_m"},
	}}
	entries, complete := jobPlannedWriteEntries(live, plan)
	if !complete {
		t.Fatalf("planned write entries were reported incomplete: %v", entries)
	}
	planned, frozenComplete := plannedDestinationSet(entries)
	if !frozenComplete {
		t.Fatalf("planned destinations did not fully freeze: %v", entries)
	}
	m.runActivities[7] = &jobWriteActivity{
		job: live, descriptor: decisionJob(live),
		planKnown: true, planComplete: true, planned: planned,
	}

	base := jobDestinationBase(live)
	prefixed := filepath.Join(base, "q4_k_m", "model-Q4_K_M.gguf")
	phantom := filepath.Join(base, "model-Q4_K_M.gguf")
	snapshot := filepath.Join(live.HubDir, "models--owner--model", "snapshots", "deadbeef", "model-Q4_K_M.gguf")

	if _, ok := m.reserveSelectedGGUF([]string{prefixed}); ok {
		t.Fatalf("DELETE of the real prefixed target %s was admitted: the interlock must guard the destination appendFilterSubdir really writes", prefixed)
	}
	if _, ok := m.reserveSelectedGGUF([]string{snapshot}); ok {
		t.Fatalf("DELETE of the snapshot target %s was admitted: the unprefixed snapshot path stays planned", snapshot)
	}
	release, ok := m.reserveSelectedGGUF([]string{phantom})
	if !ok {
		t.Fatalf("DELETE of the phantom unprefixed path %s was blocked: the interlock must not guard a destination the job no longer writes", phantom)
	}
	release()

	// The entries themselves: prefixed flat/friendly + unprefixed snapshot,
	// and no phantom unprefixed flat/friendly entry.
	got := make(map[string]bool, len(entries))
	for _, entry := range entries {
		got[filepath.Clean(entry)] = true
	}
	if !got[filepath.Clean(prefixed)] {
		t.Errorf("planned entries lack the prefixed flat/friendly destination: %v", entries)
	}
	if !got[filepath.Clean(snapshot)] {
		t.Errorf("planned entries lack the unprefixed snapshot destination: %v", entries)
	}
	if got[filepath.Clean(phantom)] {
		t.Errorf("planned entries guard the phantom unprefixed destination: %v", entries)
	}
}

// Contract pin (FLIPPED): a real download combining a path-shaped filter
// with appendFilterSubdir is rejected at the request boundary with the same
// named 400 the plan and dry-run surfaces answer with — 400
// invalid_append_filter_subdir, the documented message, details naming the
// offending filter — before any Hub contact and before any job is admitted.
// hfdownloader's validate rejects the same combination inside runJob, so a
// stored path-filter+flag job could only die at run time; the boundary now
// fails it closed instead.
func TestRealDownloadPathFilterWithAppendFilterSubdirRejectedBeforeHub(t *testing.T) {
	var requests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected hub request", http.StatusInternalServerError)
	}))
	defer hub.Close()

	s := newTestServerWithConfig(t, Config{
		CacheDir:  t.TempDir(),
		LocalDir:  t.TempDir(),
		Endpoint:  hub.URL,
		MaxActive: 1,
		Proxy:     &hfdownloader.ProxyConfig{NoEnvProxy: true},
	})
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, err := json.Marshal(DownloadRequest{
		Repo: "owner/model", Filters: []string{"weights/model.gguf"},
		ExactMatch: true, AppendFilterSubdir: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/download", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response: %v; body=%s", err, w.Body.String())
	}
	if resp.ErrorDetail == nil || resp.ErrorDetail.Code != "invalid_append_filter_subdir" {
		t.Fatalf("error_detail=%+v, want code invalid_append_filter_subdir; body=%s", resp.ErrorDetail, w.Body.String())
	}
	if resp.Error != "AppendFilterSubdir cannot be combined with path-shaped filters" {
		t.Errorf("error=%q, want the documented append-filter message", resp.Error)
	}
	if resp.ErrorDetail.Details == "" || !strings.Contains(resp.ErrorDetail.Details, "weights/model.gguf") {
		t.Errorf("details=%q, want the offending filter included", resp.ErrorDetail.Details)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("rejected request reached the hub %d time(s), want none", got)
	}
	if jobs := s.jobs.ListJobs(); len(jobs) != 0 {
		t.Errorf("rejected request admitted %d job(s), want none", len(jobs))
	}
}

// Contract pin (FLIPPED): a REAL download with a single-component filter and
// appendFilterSubdir applies the library's filter-subdirectory layout over
// HTTP. Planning gives such an item a non-empty DestinationSubdir (the
// matched filter), so the file must land under <filter>/ and the flat
// repository-relative path must NOT be written — the mirror image of the
// old flat-landing lock, now that runJob passes the flag through.
func TestRealDownloadSingleComponentFilterWithAppendFilterSubdirLandsUnderFilterSubdir(t *testing.T) {
	const payload = "data"
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"deadbeef"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte(`[{"type":"file","path":"model-Q4_K_M.gguf","size":4}]`))
		case r.URL.Path == "/owner/model/raw/deadbeef/model-Q4_K_M.gguf":
			_, _ = w.Write([]byte(payload))
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	root := t.TempDir()
	s := newTestServerWithConfig(t, Config{
		CacheDir:  t.TempDir(),
		LocalDir:  root,
		Endpoint:  hub.URL,
		MaxActive: 1,
		Proxy:     &hfdownloader.ProxyConfig{NoEnvProxy: true},
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
		t.Fatalf("status=%d, want 202 admitted job; body=%s", w.Code, w.Body.String())
	}
	var admitted Job
	if err := json.Unmarshal(w.Body.Bytes(), &admitted); err != nil {
		t.Fatal(err)
	}
	if admitted.ID == "" {
		t.Fatalf("admitted without job id: %s", w.Body.String())
	}
	finished := waitJobStatus(t, s.jobs, admitted.ID, JobStatusCompleted, 10*time.Second)
	if finished.Status != JobStatusCompleted {
		t.Fatalf("job status=%s err=%q; want completed", finished.Status, finished.Error)
	}

	base, err := hfdownloader.DestinationBase(root, "owner/model")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "q4_k_m", "model-Q4_K_M.gguf")
	got, err := os.ReadFile(target)
	if err != nil || string(got) != payload {
		t.Fatalf("filter-subdirectory destination %s = %q, err=%v; want %q (the flag must move the file under <filter>/)", target, got, err, payload)
	}
	flatTarget := filepath.Join(base, "model-Q4_K_M.gguf")
	if _, err := os.Stat(flatTarget); err == nil {
		t.Fatalf("flat destination %s exists: appendFilterSubdir must not also write the unprefixed path", flatTarget)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
