// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// TestPlanRepo_PaginatedTreeReturnsEveryFile drives the full plan scan
// against a fake Hub whose tree listing spans three pages behind Link
// rel="next" (issue #96): every file must reach the plan, for models and
// datasets alike, and PlanItem LFS semantics (size from LFS.Size, canonical
// sha256) must be preserved.
func TestPlanRepo_PaginatedTreeReturnsEveryFile(t *testing.T) {
	const (
		hexA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		hexB = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	)
	files := []hfNode{
		{Type: "file", Path: "config.json", Size: 100},
		{Type: "file", Path: "README.md", Size: 200},
		{Type: "file", Path: ".gitattributes", Size: 50},
		{Type: "file", Path: "weights.bin", Size: 16, LFS: &hfLfsInfo{Size: 12345, Sha256: hexA}},
		{Type: "file", Path: "model-00001-of-00002.safetensors", Size: 15, LFS: &hfLfsInfo{Size: 777, Oid: hexB}},
		{Type: "file", Path: "model-00002-of-00002.safetensors", Size: 15, LFS: &hfLfsInfo{Size: 888, Sha256: hexA, Oid: hexB}},
		{Type: "file", Path: "tokenizer.json", Size: 300},
		{Type: "file", Path: "special_tokens_map.json", Size: 40},
		{Type: "file", Path: "vocab.json", Size: 500},
		{Type: "file", Path: "merges.txt", Size: 600},
		{Type: "file", Path: "generation_config.json", Size: 90},
		{Type: "file", Path: "training_args.json", Size: 70},
	}
	const pageSize = 5 // 12 files -> 3 pages

	for _, tc := range []struct {
		name      string
		isDataset bool
	}{
		{"model", false},
		{"dataset", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				treeReqs   atomic.Int64
				recursive  atomic.Int64
				cursorReqs atomic.Int64
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/revision/"):
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "cafebabe"})
				case strings.Contains(r.URL.Path, "/tree/"):
					treeReqs.Add(1)
					if r.URL.Query().Get("recursive") == "true" {
						recursive.Add(1)
					}
					cursor := 0
					if c := r.URL.Query().Get("cursor"); c != "" {
						n, err := strconv.Atoi(c)
						if err != nil {
							t.Errorf("bad cursor %q: %v", c, err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						cursor = n
						cursorReqs.Add(1)
					}
					end := cursor + pageSize
					if end > len(files) {
						end = len(files)
					}
					if end < len(files) {
						w.Header().Set("Link", fmt.Sprintf("<%s?recursive=true&cursor=%d>; rel=\"next\"", r.URL.Path, end))
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(files[cursor:end])
				default:
					t.Logf("unexpected HF request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			plan, err := PlanRepo(context.Background(),
				Job{Repo: "owner/repo", Revision: "main", IsDataset: tc.isDataset},
				Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("PlanRepo: %v", err)
			}
			if plan.Commit != "cafebabe" {
				t.Errorf("plan.Commit = %q, want %q", plan.Commit, "cafebabe")
			}
			if len(plan.Items) != len(files) {
				t.Fatalf("plan has %d items, want %d (every file past the first page)", len(plan.Items), len(files))
			}
			if n := treeReqs.Load(); n != 3 {
				t.Errorf("tree requests = %d, want 3 paginated pages", n)
			}
			if n := recursive.Load(); n != 3 {
				t.Errorf("recursive=true on %d requests, want 3", n)
			}
			if n := cursorReqs.Load(); n != 2 {
				t.Errorf("Link-followed requests = %d, want 2", n)
			}

			byPath := make(map[string]PlanItem, len(plan.Items))
			for _, it := range plan.Items {
				byPath[it.RelativePath] = it
			}
			// PlanItem semantics must be unchanged: LFS size comes from
			// LFS.Size (never the pointer size) and the sha256 is canonical.
			checks := []struct {
				path string
				size int64
				lfs  bool
				sha  string
			}{
				{"config.json", 100, false, ""},
				{"README.md", 200, false, ""},
				{"weights.bin", 12345, true, hexA},
				{"model-00001-of-00002.safetensors", 777, true, hexB},
				{"model-00002-of-00002.safetensors", 888, true, hexA},
				{"training_args.json", 70, false, ""},
			}
			for _, c := range checks {
				it, ok := byPath[c.path]
				if !ok {
					t.Errorf("missing plan item %q", c.path)
					continue
				}
				if it.Size != c.size || it.LFS != c.lfs || it.SHA256 != c.sha {
					t.Errorf("item %q = {size:%d lfs:%v sha:%q}, want {size:%d lfs:%v sha:%q}",
						c.path, it.Size, it.LFS, it.SHA256, c.size, c.lfs, c.sha)
				}
			}
		})
	}
}

// TestPlanRepo_TerminalTreeFailsFast pins the fail-fast contract on the
// plan path: terminal tree responses surface the historical messages
// after exactly one request, with no retry loop.
func TestPlanRepo_TerminalTreeFailsFast(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var treeReqs atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/revision/"):
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "cafebabe"})
				case strings.Contains(r.URL.Path, "/tree/"):
					treeReqs.Add(1)
					w.WriteHeader(status)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			_, err := PlanRepo(context.Background(),
				Job{Repo: "owner/repo", Revision: "main"},
				Settings{Endpoint: srv.URL})
			want := map[int]string{
				http.StatusUnauthorized: fmt.Sprintf("401 unauthorized: repo requires token or you do not have access (visit %s/owner/repo)", srv.URL),
				http.StatusForbidden:    fmt.Sprintf("403 forbidden: please accept the repository terms: %s/owner/repo", srv.URL),
				http.StatusNotFound:     "tree API failed: 404 Not Found",
			}[status]
			if err == nil || err.Error() != want {
				t.Errorf("error = %v, want %q", err, want)
			}
			if n := treeReqs.Load(); n != 1 {
				t.Errorf("tree requests = %d, want exactly 1 (no retry on %d)", n, status)
			}
		})
	}
}
