// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// mockHFServerForPlan sets up a minimal HuggingFace API server that responds
// to the endpoints PlanRepo touches: /api/models/{repo}/revision/{revision}
// (commit SHA) and /api/models/{repo}/tree/{revision}[/...] (file list).
//
// The tree is a flat list (no subdirectories) so the test only needs to
// register one tree response. The mock rejects any other path so a missing
// mock setup surfaces as a 404 rather than silently passing.
func mockHFServerForPlan(t *testing.T, files []hfNode) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/revision/main"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "deadbeef"})
		case strings.Contains(path, "/tree/"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(files)
		default:
			t.Logf("mockHFServerForPlan: unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestPlanRepo_CaseDuplicateMMProj is the regression test for the bug where
// selecting the recommended mmproj downloads both case-only-different copies
// (e.g. mmproj-X-F16.gguf and mmproj-X-f16.gguf). The HF API lists both files,
// the picker filter is case-insensitive and matches both, and the plan must
// not contain both.
func TestPlanRepo_CaseDuplicateMMProj(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "model-Q4_K_M.gguf", Size: 100},
		{Type: "file", Path: "mmproj-Qwythos-9B-Claude-Mythos-5-1M-F16.gguf", Size: 200},
		{Type: "file", Path: "mmproj-Qwythos-9B-Claude-Mythos-5-1M-f16.gguf", Size: 200},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	job := Job{
		Repo:       "owner/repo",
		Revision:   "main",
		Filters:    []string{"mmproj-qwythos-9b-claude-mythos-5-1m-f16"},
		ExactMatch: true,
	}
	cfg := Settings{Endpoint: srv.URL}

	plan, err := PlanRepo(context.Background(), job, cfg)
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}

	if got := len(plan.Items); got != 1 {
		paths := []string{}
		for _, it := range plan.Items {
			paths = append(paths, it.RelativePath)
		}
		t.Fatalf("got %d plan items, want 1; paths=%v", got, paths)
	}
	got := plan.Items[0].RelativePath
	// The first occurrence (what the HF API returns first) is the F16 variant;
	// pin the "first wins" contract so a regression to "last wins" fails.
	const want = "mmproj-Qwythos-9B-Claude-Mythos-5-1M-F16.gguf"
	if got != want {
		t.Errorf("plan kept %q, want first case-variant %q", got, want)
	}
}

// TestPlanRepo_DistinctMMProjUnaffected verifies that the case-insensitive
// dedup does NOT collapse genuinely different files. Two mmproj variants with
// different precision tokens (F16 vs BF16) must both end up in the plan.
func TestPlanRepo_DistinctMMProjUnaffected(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "model-Q4_K_M.gguf", Size: 100},
		{Type: "file", Path: "mmproj-F16.gguf", Size: 200},
		{Type: "file", Path: "mmproj-BF16.gguf", Size: 200},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	job := Job{
		Repo:       "owner/repo",
		Revision:   "main",
		Filters:    []string{"mmproj"},
		ExactMatch: true,
	}
	cfg := Settings{Endpoint: srv.URL}

	plan, err := PlanRepo(context.Background(), job, cfg)
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}

	if got := len(plan.Items); got != 2 {
		paths := []string{}
		for _, it := range plan.Items {
			paths = append(paths, it.RelativePath)
		}
		t.Fatalf("got %d plan items, want 2; paths=%v", got, paths)
	}
}

// TestPlanRepo_CaseDuplicateWithoutFilter verifies that the defensive
// case-insensitive dedup also applies when the user downloads the whole repo
// (no filter). Two case-variant files collapse to one entry.
func TestPlanRepo_CaseDuplicateWithoutFilter(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "model-Q4_K_M.gguf", Size: 100},
		{Type: "file", Path: "mmproj-X-F16.gguf", Size: 200},
		{Type: "file", Path: "mmproj-X-f16.gguf", Size: 200},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	job := Job{
		Repo:     "owner/repo",
		Revision: "main",
		// no Filters
	}
	cfg := Settings{Endpoint: srv.URL}

	plan, err := PlanRepo(context.Background(), job, cfg)
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}

	if got := len(plan.Items); got != 2 {
		paths := []string{}
		for _, it := range plan.Items {
			paths = append(paths, it.RelativePath)
		}
		t.Fatalf("got %d plan items, want 2 (one LLM + one mmproj); paths=%v", got, paths)
	}
}

func TestPlanRepo_FilteredGGUFDoesNotIncludeUnmatchedNonLFSQuant(t *testing.T) {
	files := []hfNode{{Type: "file", Path: "model-Q4_K_M.gguf", Size: 100}}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/repo", Revision: "main", Filters: []string{"Q5_K_M"},
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	if len(plan.Items) != 0 {
		t.Fatalf("PlanRepo included unmatched GGUF: %+v", plan.Items)
	}
}

func TestPlanRepo_GGUFOnlyFilterPreservesNonGGUFCompanionExclusion(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "model-Q4_K_M.gguf", Size: 100},
		{Type: "file", Path: "config.json", Size: 20},
		{Type: "file", Path: "tokenizer.json", Size: 30},
		{Type: "file", Path: "README.md", Size: 40},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "main", Filters: []string{"Q4_K_M"}}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	if len(plan.Items) != 1 || plan.Items[0].RelativePath != "model-Q4_K_M.gguf" {
		t.Fatalf("GGUF-only selection included companions: %+v", plan.Items)
	}
}

func TestPlanRepo_GGUFModePreservesExplicitNonGGUFFilterMatches(t *testing.T) {
	cases := []struct {
		name    string
		files   []hfNode
		filters []string
		want    []string
	}{
		{
			name:    "explicit config match",
			files:   []hfNode{{Type: "file", Path: "model-Q4_K_M.gguf", Size: 100}, {Type: "file", Path: "config.json", Size: 20}},
			filters: []string{"Q4_K_M", "config.json"},
			want:    []string{"model-Q4_K_M.gguf", "config.json"},
		},
		{
			name:    "README basename matches requested filter",
			files:   []hfNode{{Type: "file", Path: "model-Q4_K_M.gguf", Size: 100}, {Type: "file", Path: "README-Q4_K_M.md", Size: 20}},
			filters: []string{"Q4_K_M"},
			want:    []string{"model-Q4_K_M.gguf", "README-Q4_K_M.md"},
		},
		{
			name:    "unmatched companions remain excluded",
			files:   []hfNode{{Type: "file", Path: "model-Q4_K_M.gguf", Size: 100}, {Type: "file", Path: "config.json", Size: 20}, {Type: "file", Path: "README.md", Size: 30}},
			filters: []string{"Q4_K_M"},
			want:    []string{"model-Q4_K_M.gguf"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := mockHFServerForPlan(t, tc.files)
			defer srv.Close()
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "main", Filters: tc.filters}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("PlanRepo: %v", err)
			}
			got := make([]string, len(plan.Items))
			for i := range plan.Items {
				got[i] = plan.Items[i].RelativePath
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("plan paths=%v want %v", got, tc.want)
			}
		})
	}
}

func TestGGUFPathSelectedMatchesFilteredGGUFPlanDecision(t *testing.T) {
	filters, excludes := []string{"Q4_K_M"}, []string{"draft"}
	for _, path := range []string{"model-Q4_K_M.gguf", "model-Q5_K_M.gguf", "draft-Q4_K_M.gguf", "config.json"} {
		want := strings.Contains(strings.ToLower(path), "q4_k_m") && !strings.Contains(strings.ToLower(path), "draft") && strings.EqualFold(filepath.Ext(path), ".gguf")
		if got := GGUFPathSelected(path, filters, excludes, false); got != want {
			t.Errorf("GGUFPathSelected(%q)=%v want %v", path, got, want)
		}
	}
}

func TestPlanRepo_FilteredGGUFKeepsOnlyMatchingQuantAcrossStorageTypes(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "model-Q4_K_M.gguf", Size: 100},
		{Type: "file", Path: "model-Q5_K_M.gguf", Size: 200, LFS: &hfLfsInfo{Size: 200}},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/repo", Revision: "main", Filters: []string{"Q5_K_M"},
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	if len(plan.Items) != 1 || plan.Items[0].RelativePath != "model-Q5_K_M.gguf" || !plan.Items[0].LFS {
		t.Fatalf("PlanRepo items = %+v, want only matching LFS Q5 GGUF", plan.Items)
	}
}
