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

func TestNormalDownloadPathFilterWithAppendSubdirStillRuns(t *testing.T) {
	const payload = "data"
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"deadbeef"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte(`[{"type":"file","path":"weights/model.gguf","size":4}]`))
		case r.URL.Path == "/owner/model/raw/deadbeef/weights/model.gguf":
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
		Repo: "owner/model", Filters: []string{"weights/model.gguf"},
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
	target := filepath.Join(base, "weights", "model.gguf")
	got, err := os.ReadFile(target)
	if err != nil || string(got) != payload {
		t.Fatalf("repo-relative destination %s = %q, err=%v; want %q", target, got, err, payload)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		t.Fatalf("path filter was applied as a destination directory: %s is a directory", target)
	}
}
