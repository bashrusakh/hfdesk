// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPlanRepoRejectsPathShapedFilterWithAppendFilterSubdir(t *testing.T) {
	for _, filter := range []string{
		"weights/model.gguf",
		"unet/",
		"./model.gguf",
		`weights\model.gguf`,
		".",
		"..",
	} {
		t.Run(filter, func(t *testing.T) {
			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Error(w, "unexpected network request", http.StatusInternalServerError)
			}))
			defer srv.Close()

			plan, err := PlanRepo(t.Context(), Job{
				Repo: "owner/model", Filters: []string{filter}, ExactMatch: true,
				AppendFilterSubdir: true,
			}, Settings{Endpoint: srv.URL})
			if plan != nil {
				t.Fatalf("PlanRepo plan=%+v, want nil for rejected filter %q", plan.Items, filter)
			}
			if !errors.Is(err, ErrAppendFilterSubdirPathFilter) {
				t.Fatalf("PlanRepo filter %q error=%v, want ErrAppendFilterSubdirPathFilter", filter, err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", filter)) {
				t.Errorf("PlanRepo filter %q error=%q, want offending filter included", filter, err)
			}
			if got := requests.Load(); got != 0 {
				t.Errorf("PlanRepo filter %q made %d network request(s), want none", filter, got)
			}
		})
	}
}

func TestPlanRepoAppendFilterSubdirErrorNamesOnlyPathFilters(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected network request", http.StatusInternalServerError)
	}))
	defer srv.Close()

	plan, err := PlanRepo(t.Context(), Job{
		Repo: "owner/model", Filters: []string{"q4_k_m", "weights/model.gguf"},
		ExactMatch: true, AppendFilterSubdir: true,
	}, Settings{Endpoint: srv.URL})
	if plan != nil || !errors.Is(err, ErrAppendFilterSubdirPathFilter) {
		t.Fatalf("PlanRepo = (%v, %v), want nil plan and ErrAppendFilterSubdirPathFilter", plan, err)
	}
	if !strings.Contains(err.Error(), `"weights/model.gguf"`) {
		t.Errorf("error=%q, want offending path filter included", err)
	}
	if strings.Contains(err.Error(), `"q4_k_m"`) {
		t.Errorf("error=%q, want only offending filters", err)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("PlanRepo made %d network request(s), want none", got)
	}
}

func TestDownloadRejectsPathShapedFilterWithAppendFilterSubdir(t *testing.T) {
	for _, filter := range []string{
		"weights/model.gguf",
		"unet/",
		"./model.gguf",
		`weights\model.gguf`,
		".",
		"..",
	} {
		t.Run(filter, func(t *testing.T) {
			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Error(w, "unexpected network request", http.StatusInternalServerError)
			}))
			defer srv.Close()
			out := t.TempDir()

			err := Download(t.Context(), Job{
				Repo: "owner/model", Filters: []string{filter}, ExactMatch: true,
				AppendFilterSubdir: true,
			}, Settings{Endpoint: srv.URL, OutputDir: out, Concurrency: 1, MaxActiveDownloads: 1}, nil)
			if !errors.Is(err, ErrAppendFilterSubdirPathFilter) {
				t.Fatalf("Download filter %q error=%v, want ErrAppendFilterSubdirPathFilter", filter, err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", filter)) {
				t.Errorf("Download filter %q error=%q, want offending filter included", filter, err)
			}
			if got := requests.Load(); got != 0 {
				t.Errorf("Download filter %q made %d network request(s), want none", filter, got)
			}
			entries, readErr := os.ReadDir(out)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Errorf("Download filter %q wrote %d output entr(ies) before rejection", filter, len(entries))
			}
		})
	}
}

func TestAppendFilterSubdirAcceptsSingleComponentFilters(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "model-q4_k_m.gguf", Size: 4},
		{Type: "file", Path: "config.json", Size: 4},
		{Type: "file", Path: "model.safetensors", Size: 4},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	for _, tc := range []struct {
		filter string
		want   string
	}{
		{filter: "q4_k_m", want: "model-q4_k_m.gguf"},
		{filter: "config.json", want: "config.json"},
		{filter: ".safetensors", want: "model.safetensors"},
	} {
		t.Run(tc.filter, func(t *testing.T) {
			plan, err := PlanRepo(t.Context(), Job{
				Repo: "owner/model", Filters: []string{tc.filter}, AppendFilterSubdir: true,
			}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("PlanRepo filter %q: %v", tc.filter, err)
			}
			if plan == nil || !containsString(planPaths(plan), tc.want) {
				t.Fatalf("PlanRepo filter %q paths=%v, want %q", tc.filter, planPaths(plan), tc.want)
			}
		})
	}
}

func TestPathFilterSucceedsWithoutAppendFilterSubdir(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "deadbeef"})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]hfNode{{Type: "file", Path: "weights/model.gguf", Size: 4}})
		case r.URL.Path == "/owner/model/raw/main/weights/model.gguf":
			_, _ = w.Write([]byte("data"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	job := Job{
		Repo: "owner/model", Filters: []string{"weights/model.gguf"}, ExactMatch: true,
		AppendFilterSubdir: false,
	}
	plan, err := PlanRepo(t.Context(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo path filter with append disabled: %v", err)
	}
	if plan == nil || !containsString(planPaths(plan), "weights/model.gguf") {
		t.Fatalf("PlanRepo paths=%v, want weights/model.gguf", planPaths(plan))
	}

	out := t.TempDir()
	if err := Download(context.Background(), job, Settings{
		Endpoint: srv.URL, OutputDir: out, Concurrency: 1, MaxActiveDownloads: 1,
	}, nil); err != nil {
		t.Fatalf("Download path filter with append disabled: %v", err)
	}
	base, err := DestinationBase(out, job.Repo)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(base, "weights", "model.gguf"))
	if err != nil || string(got) != "data" {
		t.Fatalf("downloaded path filter destination = %q, err=%v; want data", got, err)
	}
	if got := requests.Load(); got == 0 {
		t.Error("expected network requests for the append-disabled control")
	}
}
