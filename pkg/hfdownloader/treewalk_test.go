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

// Both production namespaces must fail closed while retaining same-origin
// pagination, including authorization on every source request.
func TestPlanRepo_PaginationOriginBoundary(t *testing.T) {
	for _, dataset := range []bool{false, true} {
		for _, token := range []string{"", "fake-token"} {
			for _, form := range []string{"foreign absolute", "foreign network path", "foreign hostname", "absolute", "network path", "query", "root relative", "path relative"} {
				t.Run(fmt.Sprintf("dataset=%v/token=%s/%s", dataset, token, form), func(t *testing.T) {
					var foreign, treeReqs atomic.Int64
					dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						foreign.Add(1)
						_ = json.NewEncoder(w).Encode([]hfNode{{Type: "file", Path: "foreign.txt", Size: 1}})
					}))
					defer dst.Close()
					api := "models"
					if dataset {
						api = "datasets"
					}
					treePath := "/api/" + api + "/owner/repo/tree/main"
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						wantAuth := ""
						if token != "" {
							wantAuth = "Bearer " + token
						}
						if r.Header.Get("Authorization") != wantAuth {
							t.Error("source authorization changed")
						}
						if strings.Contains(r.URL.Path, "/revision/") {
							_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "cafebabe"})
							return
						}
						page := treeReqs.Add(1)
						if r.URL.Path != treePath || page > 2 || (page == 2 && r.URL.Query().Get("cursor") != "2") {
							t.Errorf("unexpected tree request: %s", r.URL)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						if page == 1 {
							next := "?cursor=2"
							switch form {
							case "foreign absolute":
								next = dst.URL + "/401/unauthorized/not%20found"
							case "foreign network path":
								next = strings.TrimPrefix(dst.URL, "http:") + "/page"
							case "foreign hostname":
								next = "http://" + strings.Replace(r.Host, "127.0.0.1", "localhost", 1) + treePath + next
							case "absolute":
								next = "http://" + r.Host + treePath + next
							case "network path":
								next = "//" + r.Host + treePath + next
							case "root relative":
								next = treePath + next
							case "path relative":
								next = "main" + next
							}
							w.Header().Set("Link", "<"+next+">; rel=next")
						}
						_ = json.NewEncoder(w).Encode([]hfNode{{Type: "file", Path: fmt.Sprintf("f%d.txt", page), Size: page}})
					}))
					defer srv.Close()
					plan, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "main", IsDataset: dataset}, Settings{Endpoint: srv.URL, Token: token})
					wantReqs := int64(2)
					if strings.HasPrefix(form, "foreign") {
						wantReqs = 1
						if err == nil || err.Error() != "hubtree: cross-origin next pagination target" || plan != nil {
							t.Errorf("plan = %v, error = %v; want cross-origin failure, no partial plan", plan, err)
						}
					} else if err != nil || plan == nil || len(plan.Items) != 2 || plan.Items[0].RelativePath != "f1.txt" || plan.Items[1].RelativePath != "f2.txt" || plan.Commit != "cafebabe" {
						t.Errorf("same-origin plan = %v, error = %v; want both files and commit", plan, err)
					}
					if foreign.Load() != 0 || treeReqs.Load() != wantReqs {
						t.Errorf("foreign/tree requests = %d/%d, want 0/%d", foreign.Load(), treeReqs.Load(), wantReqs)
					}
				})
			}
		}
	}
}

// A later directory failure must invalidate the selected tree even after a
// successful root file callback, including mirrors rejecting recursive=true.
func TestPlanRepo_CrossOriginDirectoryFailsWholePlan(t *testing.T) {
	for _, dataset := range []bool{false, true} {
		for _, token := range []string{"", "fake-token"} {
			for _, fallback := range []int{0, http.StatusBadRequest, http.StatusUnprocessableEntity} {
				t.Run(fmt.Sprintf("dataset=%v/token=%s/fallback=%d", dataset, token, fallback), func(t *testing.T) {
					var foreign, treeReqs atomic.Int64
					dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						foreign.Add(1)
						_ = json.NewEncoder(w).Encode([]hfNode{{Type: "file", Path: "sub/foreign.txt"}})
					}))
					defer dst.Close()
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.Contains(r.URL.Path, "/revision/") {
							_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "cafebabe"})
							return
						}
						treeReqs.Add(1)
						if fallback != 0 && r.URL.Query().Has("recursive") {
							w.WriteHeader(fallback)
							return
						}
						nodes := []hfNode{{Type: "file", Path: "root.txt"}, {Type: "directory", Path: "sub"}}
						if strings.HasSuffix(r.URL.Path, "/sub") {
							w.Header().Set("Link", "<"+dst.URL+"/page>; rel=next")
							nodes = []hfNode{{Type: "file", Path: "sub/local.txt"}}
						}
						_ = json.NewEncoder(w).Encode(nodes)
					}))
					defer srv.Close()
					plan, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", IsDataset: dataset}, Settings{Endpoint: srv.URL, Token: token})
					if err == nil || err.Error() != "hubtree: cross-origin next pagination target" || plan != nil {
						t.Errorf("plan = %v, error = %v; want whole-plan failure", plan, err)
					}
					want := int64(2)
					if fallback != 0 {
						want++
					}
					if foreign.Load() != 0 || treeReqs.Load() != want {
						t.Errorf("foreign/tree requests = %d/%d, want 0/%d", foreign.Load(), treeReqs.Load(), want)
					}
				})
			}
		}
	}
}

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

// A successful plan requires exhaustion of advertised pagination, not just
// consumption of the first page. Exercise the production adapter for both APIs.
func TestPlanRepo_PaginationCompletion(t *testing.T) {
	for _, dataset := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			links    []string
			cycle    bool
			wantErr  bool
			requests int64
		}{
			{name: "no next", requests: 1},
			{name: "unrelated malformed prev", links: []string{`<broken; rel=prev`}, requests: 1},
			{name: "invalid escape", links: []string{`</%zz>; rel=next`}, wantErr: true, requests: 1},
			{name: "invalid host", links: []string{`<http://[::1>; rel=next`}, wantErr: true, requests: 1},
			{name: "empty next", links: []string{`<>; rel=next`}, wantErr: true, requests: 1},
			{name: "unclosed target", links: []string{`<?cursor=2; rel=next`}, wantErr: true, requests: 1},
			{name: "self cycle", links: []string{`<?recursive=true>; rel=next`}, wantErr: true, requests: 1},
			{name: "multiple page cycle", links: []string{`<?cursor=2>; rel=next`}, cycle: true, wantErr: true, requests: 2},
			{name: "repeated fields", links: []string{`<?cursor=0>; rel=prev`, `<?cursor=2>; rel=next`}, requests: 2},
			{name: "next first", links: []string{`<?cursor=2>; rel=next, <?cursor=0>; rel=prev`}, requests: 2},
			{name: "next last", links: []string{`<?cursor=0>; rel=prev, <?cursor=2>; rel=next`}, requests: 2},
			{name: "quoted delimiters", links: []string{`<?cursor=0>; title="ignore;rel=next,<not a target>"; rel=prev, <?cursor=2>; title="page, two"; rel="next alternate"`}, requests: 2},
			{name: "reported apostrophe prev first", links: []string{`<?cursor=0>; title=owner's; rel=prev, <?cursor=2>; rel=next`}, requests: 2},
			{name: "reported apostrophe next only", links: []string{`<?cursor=2>; title=owner's; rel=next`}, requests: 2},
			{name: "leading apostrophe metadata", links: []string{`<?cursor=2>; title='owners; rel=next`}, requests: 2},
			{name: "apostrophe parameter key", links: []string{`<?cursor=2>; owner's=metadata; rel=next`}, requests: 2},
			{name: "single quoted relation with token metadata", links: []string{`<?cursor=2>; title=owner's; rel='alternate next'`}, requests: 2},
			{name: "field state resets", links: []string{`<?cursor=0>; title="unfinished`, `<?cursor=2>; rel=next`}, requests: 2},
			{name: "malformed next after token metadata", links: []string{`<broken; title=owner's; rel=next`}, wantErr: true, requests: 1},
		} {
			t.Run(fmt.Sprintf("dataset=%v/%s", dataset, tc.name), func(t *testing.T) {
				var reqs atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "/revision/") {
						_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "cafebabe"})
						return
					}
					if !strings.Contains(r.URL.Path, "/tree/") {
						t.Errorf("unexpected request: %s", r.URL)
						w.WriteHeader(http.StatusNotFound)
						return
					}
					api := "models"
					if dataset {
						api = "datasets"
					}
					if r.URL.Path != "/api/"+api+"/owner/repo/tree/main" {
						t.Errorf("unexpected tree API path: %s", r.URL.Path)
					}
					page := reqs.Add(1)
					if page > 2 {
						t.Error("pagination did not terminate within two requests")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if page == 2 && r.URL.Query().Get("cursor") != "2" {
						t.Errorf("followed wrong link: %s", r.URL)
					}
					if r.URL.Query().Get("cursor") == "" {
						for _, link := range tc.links {
							w.Header().Add("Link", link)
						}
					} else if tc.cycle {
						w.Header().Set("Link", `<?recursive=true>; rel=next`)
					}
					_ = json.NewEncoder(w).Encode([]hfNode{{Type: "file", Path: fmt.Sprintf("f%d.bin", page), Size: page}})
				}))
				defer srv.Close()
				plan, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "main", IsDataset: dataset}, Settings{Endpoint: srv.URL})
				if (err != nil) != tc.wantErr {
					t.Fatalf("PlanRepo error = %v, want error %v", err, tc.wantErr)
				}
				if tc.wantErr && plan != nil {
					t.Error("failed pagination returned a partial plan")
				}
				if got := reqs.Load(); got != tc.requests {
					t.Errorf("requests = %d, want %d", got, tc.requests)
				}
				if !tc.wantErr && len(plan.Items) != int(tc.requests) {
					t.Errorf("files = %d, want %d", len(plan.Items), tc.requests)
				}
			})
		}
	}
}

// Directory traversal must let the existing FILE guard reject unsafe children.
// An empty backslash directory itself was not an error in the legacy walker.
func TestPlanRepo_BackslashDirectoryPreservesFileValidation(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		direct, childFile, wantErr bool
		requests                   int64
	}{
		{name: "empty directory", requests: 2},
		{name: "unsafe child file", childFile: true, wantErr: true, requests: 2},
		{name: "direct unsafe file", direct: true, wantErr: true, requests: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reqs atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/revision/") {
					_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "cafebabe"})
					return
				}
				reqs.Add(1)
				nodes := []hfNode{{Type: "file", Path: "ok.bin", Size: 1}}
				switch {
				case strings.HasSuffix(r.URL.Path, "/tree/main"):
					if tc.direct {
						nodes = append(nodes, hfNode{Type: "file", Path: `win\dir/evil/file.bin`})
					} else {
						nodes = append(nodes, hfNode{Type: "directory", Path: `win\dir`})
					}
				case strings.HasSuffix(r.URL.Path, `/tree/main/win\dir`):
					if !strings.Contains(r.URL.EscapedPath(), "win%5Cdir") {
						t.Errorf("backslash not escaped: %s", r.URL.EscapedPath())
					}
					nodes = []hfNode{}
					if tc.childFile {
						nodes = append(nodes, hfNode{Type: "file", Path: `win\dir/evil/file.bin`})
					}
				default:
					t.Errorf("unexpected request: %s", r.URL)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(nodes)
			}))
			defer srv.Close()
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "main"}, Settings{Endpoint: srv.URL})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "refusing unsafe path from repo tree") {
					t.Fatalf("error = %v, want unsafe FILE error", err)
				}
				if plan != nil {
					t.Error("unsafe file returned a partial plan")
				}
			} else if err != nil || len(plan.Items) != 1 {
				t.Fatalf("empty directory plan = %v, error = %v", plan, err)
			}
			if got := reqs.Load(); got != tc.requests {
				t.Errorf("requests = %d, want %d", got, tc.requests)
			}
		})
	}
}
