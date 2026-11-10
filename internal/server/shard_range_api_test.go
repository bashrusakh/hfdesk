// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
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

// shardRangeContent is the shared fixture: three plain shards plus a config,
// all raw-served under one pinned revision.
func shardRangeContent() map[string]string {
	return map[string]string{
		"model-00001-of-00003.safetensors": "shard-one",
		"model-00002-of-00003.safetensors": "shard-two",
		"model-00003-of-00003.safetensors": "shard-three",
		"config.json":                      `{"ok":true}`,
	}
}

// shardRangeHub serves the fixture tree pinned to deadbeef with raw content
// per path, for plan and download tests.
func shardRangeHub(content map[string]string) *httptest.Server {
	paths := []string{
		"model-00001-of-00003.safetensors",
		"model-00002-of-00003.safetensors",
		"model-00003-of-00003.safetensors",
		"config.json",
	}
	files := make([]map[string]any, 0, len(paths))
	for _, path := range paths {
		files = append(files, map[string]any{"type": "file", "path": path, "size": len(content[path])})
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"deadbeef"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode(files)
		case strings.HasPrefix(r.URL.Path, "/owner/model/raw/deadbeef/"):
			if body, ok := content[strings.TrimPrefix(r.URL.Path, "/owner/model/raw/deadbeef/")]; ok {
				_, _ = w.Write([]byte(body))
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
}

// newShardRangeServer wires the fixture hub into a test server + mux.
func newShardRangeServer(t *testing.T, hub *httptest.Server, extra Config) (*Server, *http.ServeMux) {
	t.Helper()
	cfg := Config{
		Endpoint:  hub.URL,
		CacheDir:  t.TempDir(),
		ModelsDir: t.TempDir(),
		Proxy:     &hfdownloader.ProxyConfig{NoEnvProxy: true},
	}
	if extra.LocalDir != "" {
		cfg.LocalDir = extra.LocalDir
	}
	if extra.MaxActive != 0 {
		cfg.MaxActive = extra.MaxActive
	}
	s := newTestServerWithConfig(t, cfg)
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)
	return s, mux
}

func postPlanResponse(t *testing.T, mux *http.ServeMux, path string, req DownloadRequest) (int, PlanResponse, string) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
	var resp PlanResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode plan response: %v; body=%s", err, w.Body.String())
		}
	}
	return w.Code, resp, w.Body.String()
}

func planPathsOf(resp PlanResponse) []string {
	paths := make([]string, 0, len(resp.Files))
	for _, f := range resp.Files {
		paths = append(paths, f.Path)
	}
	return paths
}

// TestShardRangeValidationFailsClosedBeforeHubContact exercises the named
// 400 on every entry surface — plan, dry-run download, real download — for
// the rejected shapes, and proves no Hub request and no job result from any
// of them.
func TestShardRangeValidationFailsClosedBeforeHubContact(t *testing.T) {
	var requests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected hub request", http.StatusInternalServerError)
	}))
	defer hub.Close()
	s, mux := newShardRangeServer(t, hub, Config{})

	for _, route := range []struct {
		name string
		path string
		dry  bool
	}{
		{name: "plan", path: "/api/plan"},
		{name: "download dry-run", path: "/api/download", dry: true},
		{name: "download", path: "/api/download"},
	} {
		for name, req := range map[string]DownloadRequest{
			"reversed span":     {Repo: "owner/model", Filters: []string{".safetensors"}, ExactMatch: true, ShardRanges: "10-5"},
			"empty expression":  {Repo: "owner/model", Filters: []string{".safetensors"}, ExactMatch: true, ShardRanges: "  "},
			"zero index":        {Repo: "owner/model", Filters: []string{".safetensors"}, ExactMatch: true, ShardRanges: "0-5"},
			"without selection": {Repo: "owner/model", ShardRanges: "1-2"},
		} {
			t.Run(route.name+"/"+name, func(t *testing.T) {
				req.DryRun = route.dry
				body, err := json.Marshal(req)
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
				if resp.ErrorDetail == nil || resp.ErrorDetail.Code != "invalid_shard_ranges" || resp.ErrorDetail.Details == "" {
					t.Fatalf("error_detail=%+v, want code invalid_shard_ranges with details; body=%s", resp.ErrorDetail, w.Body.String())
				}
			})
		}
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("rejected requests reached the hub %d time(s), want none", got)
	}
	if jobs := s.jobs.ListJobs(); len(jobs) != 0 {
		t.Errorf("rejected requests created %d job(s), want none", len(jobs))
	}
}

// TestShardRangePlanDryRunParityAndWarnings: POST /api/plan and dry-run
// POST /api/download apply ranges identically, an ineffective segment
// surfaces as the typed warning on both surfaces, and a range selection that
// narrows to zero files takes the established no_filter_match 400 path.
func TestShardRangePlanDryRunParityAndWarnings(t *testing.T) {
	hub := shardRangeHub(shardRangeContent())
	defer hub.Close()
	_, mux := newShardRangeServer(t, hub, Config{})

	base := DownloadRequest{
		Repo: "owner/model", Revision: "main",
		Filters: []string{".safetensors"}, ExactMatch: true,
	}
	want := []string{"model-00001-of-00003.safetensors", "model-00003-of-00003.safetensors", "config.json"}

	narrowed := base
	narrowed.ShardRanges = "1,3"
	for _, path := range []string{"/api/plan", "/api/download"} {
		req := narrowed
		req.DryRun = path == "/api/download"
		code, plan, raw := postPlanResponse(t, mux, path, req)
		if code != http.StatusOK {
			t.Fatalf("%s status=%d, want 200; body=%s", path, code, raw)
		}
		if got := strings.Join(planPathsOf(plan), ","); got != strings.Join(want, ",") {
			t.Errorf("%s paths = %v, want %v", path, planPathsOf(plan), want)
		}
		if len(plan.Warnings) != 0 {
			t.Errorf("%s warnings = %+v, want none for effective ranges", path, plan.Warnings)
		}
	}

	warned := base
	warned.ShardRanges = "1,9-10"
	for _, path := range []string{"/api/plan", "/api/download"} {
		req := warned
		req.DryRun = path == "/api/download"
		_, resp, _ := postPlanResponse(t, mux, path, req)
		if len(resp.Warnings) != 1 || resp.Warnings[0].Code != hfdownloader.PlanWarningShardRangesIneffectiveSegments ||
			strings.Join(resp.Warnings[0].Segments, ",") != "9-10" {
			t.Errorf("%s warnings = %+v, want one shard_ranges_ineffective_segments [9-10]", path, resp.Warnings)
		}
	}

	// Companion-present zero case: the retained config.json stays planned,
	// but the ranges exclude every matched shard — retention alone must
	// never satisfy a filtered request, so the plan still takes the
	// established no_filter_match 400 path.
	zero := base
	zero.ShardRanges = "5-9"
	code, _, raw := postPlanResponse(t, mux, "/api/plan", zero)
	if code != http.StatusBadRequest {
		t.Fatalf("zero-narrowed plan status=%d, want 400; body=%s", code, raw)
	}
	var errResp ErrorResponse
	if err := json.Unmarshal([]byte(raw), &errResp); err != nil {
		t.Fatal(err)
	}
	if errResp.ErrorDetail == nil || errResp.ErrorDetail.Code != "no_filter_match" {
		t.Fatalf("error_detail=%+v, want code no_filter_match; body=%s", errResp.ErrorDetail, raw)
	}
}

// TestShardRangeDownloadCompletesWithRangedShards is the e2e parity case:
// one real job ends completed with exactly the ranged shards (plus the
// unaffected config) on disk, and the ranged-out shard absent.
func TestShardRangeDownloadCompletesWithRangedShards(t *testing.T) {
	hub := shardRangeHub(shardRangeContent())
	defer hub.Close()
	root := t.TempDir()
	s, mux := newShardRangeServer(t, hub, Config{LocalDir: root, MaxActive: 1})

	body, err := json.Marshal(DownloadRequest{
		Repo: "owner/model", Revision: "main",
		Filters: []string{".safetensors"}, ExactMatch: true, ShardRanges: "1,3",
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
	if admitted.ShardRanges != "1,3" {
		t.Errorf("admitted job shardRanges = %q, want %q", admitted.ShardRanges, "1,3")
	}
	finished := waitJobStatus(t, s.jobs, admitted.ID, JobStatusCompleted, 10*time.Second)
	if finished.Status != JobStatusCompleted {
		t.Fatalf("job status=%s err=%q; want completed", finished.Status, finished.Error)
	}

	base, err := hfdownloader.DestinationBase(root, "owner/model")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"model-00001-of-00003.safetensors": "shard-one",
		"model-00003-of-00003.safetensors": "shard-three",
		"config.json":                      `{"ok":true}`,
	} {
		got, err := os.ReadFile(filepath.Join(base, path))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, err=%v; want %q", path, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "model-00002-of-00003.safetensors")); !os.IsNotExist(err) {
		t.Errorf("ranged-out shard exists on disk (stat err=%v), want absent", err)
	}
}

// TestCreateJobShardRangeIdentity: shard ranges participate in job
// identity like filters — identical requests dedup, different ranges (or no
// ranges) create independent jobs — and the snapshot exposes the field for
// /api/jobs and jobs_state.json.
func TestCreateJobShardRangeIdentity(t *testing.T) {
	mgr, cleanup := newRouteTestManager(t, Config{CacheDir: t.TempDir()})
	defer cleanup()

	base := DownloadRequest{
		Repo: "dedup/shard", Filters: []string{".safetensors"},
		ExactMatch: true, ShardRanges: "1-2",
	}
	first, existing, err := mgr.CreateJob(base)
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first job should not be existing")
	}
	encoded, _ := json.Marshal(first)
	if first.ShardRanges != "1-2" || !strings.Contains(string(encoded), `"shardRanges":"1-2"`) {
		t.Errorf("job snapshot/JSON shardRanges = %q / %s, want the request's ranges", first.ShardRanges, encoded)
	}

	same, existing, err := mgr.CreateJob(base)
	if err != nil {
		t.Fatal(err)
	}
	if !existing || same.ID != first.ID {
		t.Errorf("identical shardRanges should dedup; existing=%v ids %s/%s", existing, same.ID, first.ID)
	}

	wider := base
	wider.ShardRanges = "1-3"
	other, existing, err := mgr.CreateJob(wider)
	if err != nil {
		t.Fatal(err)
	}
	if existing || other.ID == first.ID {
		t.Errorf("different shardRanges must not dedup; existing=%v ids %s/%s", existing, other.ID, first.ID)
	}

	plain := base
	plain.ShardRanges = ""
	none, existing, err := mgr.CreateJob(plain)
	if err != nil {
		t.Fatal(err)
	}
	if existing || none.ID == first.ID {
		t.Errorf("missing shardRanges must not dedup against a ranged job; existing=%v ids %s/%s", existing, none.ID, first.ID)
	}
}
