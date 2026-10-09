// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/hubtree"
)

// A successful opposite namespace must not hide a selected tree's structural
// failure. Exercise both classifier uses and the intentionally optional probe.
func TestAnalyze_StructuralPaginationClassification(t *testing.T) {
	for _, word := range []string{"401", "unauthorized", "not found"} {
		for _, kind := range []string{"invalid", "malformed", "cycle", "empty", "malformed relation", "origin"} {
			for _, stage := range []string{"first", "later", "directory"} {
				for _, mode := range []string{"model", "dataset", "fallback401", "fallback404", "optional"} {
					t.Run(strings.Join([]string{word, kind, stage, mode}, "/"), func(t *testing.T) {
						var selectedReqs, oppositeReqs, foreignReqs atomic.Int64
						dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							foreignReqs.Add(1)
							_, _ = w.Write([]byte(`[]`))
						}))
						defer dst.Close()
						api := "models"
						if mode != "model" {
							api = "datasets"
						}
						treePath := "/api/" + api + "/owner/repo/tree/main"
						srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if strings.Contains(r.URL.Path, "/tree/") && r.URL.Path != treePath && r.URL.Path != treePath+"/sub" {
								oppositeReqs.Add(1)
								if mode == "fallback401" {
									w.WriteHeader(http.StatusUnauthorized)
								} else if mode == "fallback404" {
									w.WriteHeader(http.StatusNotFound)
								} else {
									_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: "opposite.txt", Size: 7}})
								}
								return
							}
							if strings.HasSuffix(r.URL.Path, "/refs") {
								_, _ = w.Write([]byte(`{"branches":[],"tags":[]}`))
								return
							}
							page := selectedReqs.Add(1)
							if page > 3 || (r.URL.Path != treePath && r.URL.Path != treePath+"/sub") {
								t.Errorf("unexpected request: %s", r.URL)
								w.WriteHeader(http.StatusBadRequest)
								return
							}
							if stage == "later" && page == 1 {
								w.Header().Set("Link", `<?cursor=2>; rel=next`)
							} else if stage == "directory" && r.URL.Path == treePath {
								_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: "root.txt"}, {Type: "directory", Path: "sub"}})
								return
							} else {
								link := ""
								switch kind {
								case "invalid":
									link = "</" + word + "/%zz>; rel=next"
								case "malformed":
									link = "</" + word + "; rel=next"
								case "cycle":
									link = "<?recursive=true&cursor=" + url.QueryEscape(word) + ">; rel=next"
								case "empty":
									link = `<>; rel=next; title="` + word + `"`
								case "malformed relation":
									link = `</` + word + `>; rel="next`
								case "origin":
									link = "<" + dst.URL + "/" + strings.ReplaceAll(word, " ", "%20") + ">; rel=next"
								}
								w.Header().Set("Link", link)
							}
							_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: "partial.txt", Size: 1}})
						}))
						defer srv.Close()
						a := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client(), Token: "fake-token"})
						info, err := a.Analyze(context.Background(), "owner/repo", mode == "dataset")
						if mode == "optional" {
							if err != nil || info == nil || info.IsDataset || info.FileCount != 1 || info.Files[0].Path != "opposite.txt" {
								t.Errorf("optional probe: info = %v, error = %v; want successful model", info, err)
							}
						} else if info != nil || !errors.Is(err, hubtree.ErrPagination) {
							t.Errorf("info = %v, error = %v; want structural failure, no analysis", info, err)
						}
						wantSelected, wantOpposite := int64(1), int64(0)
						if stage != "first" {
							wantSelected++
						}
						if kind == "cycle" {
							wantSelected++
						}
						if mode == "fallback401" || mode == "fallback404" || mode == "optional" {
							wantOpposite = 1
						}
						if selectedReqs.Load() != wantSelected || oppositeReqs.Load() != wantOpposite || foreignReqs.Load() != 0 {
							t.Errorf("selected/opposite/foreign = %d/%d/%d, want %d/%d/0", selectedReqs.Load(), oppositeReqs.Load(), foreignReqs.Load(), wantSelected, wantOpposite)
						}
					})
				}
			}
		}
	}
}

func TestAnalyze_NamespaceFallbackPreserved(t *testing.T) {
	// Keep ephemeral ports out of status diagnostics: the unchanged historical
	// classifier can match "401" even in a forbidden repo URL. Dial only our Hub.
	const endpoint = "http://hub.test"
	for _, modelStatus := range []int{200, 401, 403, 404} {
		for _, datasetStatus := range []int{200, 401, 403, 404} {
			t.Run(fmt.Sprintf("model=%d/dataset=%d", modelStatus, datasetStatus), func(t *testing.T) {
				var modelReqs, datasetReqs atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					status := 200
					if r.URL.Path == "/api/models/owner/repo/tree/main" {
						modelReqs.Add(1)
						status = modelStatus
					} else if r.URL.Path == "/api/datasets/owner/repo/tree/main" {
						datasetReqs.Add(1)
						status = datasetStatus
					} else if strings.HasSuffix(r.URL.Path, "/refs") {
						_, _ = w.Write([]byte(`{"branches":[],"tags":[]}`))
						return
					} else {
						t.Errorf("unexpected request: %s", r.URL)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					w.WriteHeader(status)
					if status == 200 {
						_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: "data.txt", Size: 7}})
					}
				}))
				defer srv.Close()
				transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					var dialer net.Dialer
					return dialer.DialContext(ctx, network, srv.Listener.Addr().String())
				}}
				defer transport.CloseIdleConnections()
				a := NewAnalyzer(AnalyzerOptions{Endpoint: endpoint, HTTPClient: &http.Client{Transport: transport}})
				info, err := a.Analyze(context.Background(), "owner/repo", false)
				switch {
				case modelStatus == 403:
					if info != nil || err == nil || !strings.Contains(err.Error(), "forbidden") {
						t.Errorf("info = %v, error = %v; want forbidden", info, err)
					}
				case modelStatus == 200 && datasetStatus == 200:
					if info != nil || !errors.Is(err, ErrBothExist) {
						t.Errorf("info = %v, error = %v; want ErrBothExist", info, err)
					}
				case modelStatus == 200 || datasetStatus == 200:
					if err != nil || info == nil || info.IsDataset != (modelStatus != 200) || info.FileCount != 1 || info.Files[0].Path != "data.txt" {
						t.Errorf("info = %v, error = %v; want successful selected namespace", info, err)
					}
				default:
					want := "repository not found as model or dataset: owner/repo"
					if datasetStatus == 403 {
						want = "forbidden: please accept the repository terms at " + endpoint + "/datasets/owner/repo"
					}
					if info != nil || err == nil || err.Error() != "fetch file tree: "+want {
						t.Errorf("info = %v, error = %v; want %q", info, err, want)
					}
				}
				wantDataset := int64(1)
				if modelStatus == 403 {
					wantDataset = 0
				}
				if modelReqs.Load() != 1 || datasetReqs.Load() != wantDataset {
					t.Errorf("model/dataset = %d/%d, want 1/%d", modelReqs.Load(), datasetReqs.Load(), wantDataset)
				}
			})
		}
	}
}

// Exercise public Analyze so attacker text cannot turn a pagination failure
// into namespace autodetection and successful partial analysis.
func TestAnalyze_PaginationOriginBoundary(t *testing.T) {
	for _, dataset := range []bool{false, true} {
		for _, token := range []string{"", "fake-token"} {
			for _, form := range []string{"foreign absolute", "foreign network path", "foreign hostname", "absolute", "network path", "query", "root relative", "path relative"} {
				t.Run(fmt.Sprintf("dataset=%v/token=%s/%s", dataset, token, form), func(t *testing.T) {
					var foreign, treeReqs, oppositeReqs atomic.Int64
					dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						foreign.Add(1)
						_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: "foreign.txt", Size: 1}})
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
						if r.URL.Path == "/api/datasets/owner/repo/tree/main" && !dataset {
							oppositeReqs.Add(1)
							// Successful opposite namespace must not hide a selected
							// tree failure via classification of attacker URL text.
							if strings.HasPrefix(form, "foreign") {
								_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: "dataset.txt"}})
							} else {
								w.WriteHeader(http.StatusNotFound)
							}
							return
						}
						if r.URL.Path == "/api/"+api+"/owner/repo/refs" {
							_, _ = w.Write([]byte(`{"branches":[],"tags":[]}`))
							return
						}
						page := treeReqs.Add(1)
						if r.URL.Path != treePath || page > 2 || (page == 2 && r.URL.Query().Get("cursor") != "2") {
							t.Errorf("unexpected tree request: %s", r.URL)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						if page == 1 {
							w.Header().Set("X-Repo-Commit", "cafebabe")
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
						_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: fmt.Sprintf("f%d.txt", page), Size: page}})
					}))
					defer srv.Close()
					a := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client(), Token: token})
					info, err := a.Analyze(context.Background(), "owner/repo", dataset)
					wantReqs, wantOpposite := int64(2), int64(0)
					if strings.HasPrefix(form, "foreign") {
						wantReqs = 1
						if err == nil || err.Error() != "fetch file tree: hubtree: cross-origin next pagination target" || info != nil {
							t.Errorf("info = %v, error = %v; want pagination failure, no partial analysis", info, err)
						}
					} else {
						if !dataset {
							wantOpposite = 1
						}
						if err != nil || info == nil || info.FileCount != 2 || info.Files[0].Path != "f1.txt" || info.Files[1].Path != "f2.txt" || info.Commit != "cafebabe" || info.IsDataset != dataset {
							t.Errorf("same-origin info = %v, error = %v; want both files and commit", info, err)
						}
					}
					if foreign.Load() != 0 || treeReqs.Load() != wantReqs || oppositeReqs.Load() != wantOpposite {
						t.Errorf("foreign/tree/opposite requests = %d/%d/%d, want 0/%d/%d", foreign.Load(), treeReqs.Load(), oppositeReqs.Load(), wantReqs, wantOpposite)
					}
				})
			}
		}
	}
}

func TestAnalyze_CrossOriginDirectoryFailsWholeAnalysis(t *testing.T) {
	for _, dataset := range []bool{false, true} {
		for _, token := range []string{"", "fake-token"} {
			for _, fallback := range []int{0, http.StatusBadRequest, http.StatusUnprocessableEntity} {
				t.Run(fmt.Sprintf("dataset=%v/token=%s/fallback=%d", dataset, token, fallback), func(t *testing.T) {
					var foreign, treeReqs atomic.Int64
					dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						foreign.Add(1)
						_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: "sub/foreign.txt"}})
					}))
					defer dst.Close()
					api := "models"
					if dataset {
						api = "datasets"
					}
					treePath := "/api/" + api + "/owner/repo/tree/main"
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						treeReqs.Add(1)
						if r.URL.Path != treePath && r.URL.Path != treePath+"/sub" {
							t.Errorf("unexpected request after selected-tree failure: %s", r.URL)
							w.WriteHeader(http.StatusNotFound)
							return
						}
						if fallback != 0 && r.URL.Query().Has("recursive") {
							w.WriteHeader(fallback)
							return
						}
						nodes := []hfTreeNode{{Type: "file", Path: "root.txt"}, {Type: "directory", Path: "sub"}}
						if strings.HasSuffix(r.URL.Path, "/sub") {
							w.Header().Set("Link", "<"+dst.URL+"/401/unauthorized>; rel=next")
							nodes = []hfTreeNode{{Type: "file", Path: "sub/local.txt"}}
						}
						_ = json.NewEncoder(w).Encode(nodes)
					}))
					defer srv.Close()
					a := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client(), Token: token})
					info, err := a.Analyze(context.Background(), "owner/repo", dataset)
					if err == nil || err.Error() != "fetch file tree: hubtree: cross-origin next pagination target" || info != nil {
						t.Errorf("info = %v, error = %v; want whole-analysis failure", info, err)
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

// TestFetchFileTree_PaginatedTree drives the analyzer's fetch against a fake
// Hub whose tree listing spans three pages behind Link rel="next" (issue
// #96): every file must reach the FileInfo list, for models and datasets
// alike, while X-Repo-Commit capture and FileInfo semantics stay unchanged.
func TestFetchFileTree_PaginatedTree(t *testing.T) {
	const (
		hexA     = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		hexB     = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
		commit   = "c0ffee1234"
		pageSize = 4
	)
	// One recursive listing: a directory node, a nested file, and LFS files
	// whose LFS.Size differs from the git-pointer size.
	nodes := []hfTreeNode{
		{Type: "directory", Path: "onnx"},
		{Type: "file", Path: "config.json", Size: 100},
		{Type: "file", Path: "README.md", Size: 200},
		{Type: "file", Path: "onnx/model.onnx", Size: 300},
		{Type: "file", Path: "model.safetensors", Size: 16, LFS: &struct {
			Size   int64  `json:"size,omitempty"`
			SHA256 string `json:"sha256,omitempty"`
			OID    string `json:"oid,omitempty"`
		}{Size: 9999, SHA256: hexA}},
		{Type: "file", Path: "shard.bin", Size: 15, LFS: &struct {
			Size   int64  `json:"size,omitempty"`
			SHA256 string `json:"sha256,omitempty"`
			OID    string `json:"oid,omitempty"`
		}{Size: 555, OID: hexB}},
		{Type: "file", Path: "tokenizer.json", Size: 400},
		{Type: "file", Path: "vocab.json", Size: 500},
		{Type: "file", Path: "merges.txt", Size: 600},
		{Type: "file", Path: "generation_config.json", Size: 90},
		{Type: "file", Path: "training_args.json", Size: 70},
	} // 11 nodes -> 3 pages at pageSize 4

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
				if !strings.Contains(r.URL.Path, "/tree/") {
					t.Errorf("unexpected HF request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				treeReqs.Add(1)
				// Only the FIRST response carries the header: capture must
				// happen there, not on some later page.
				if r.URL.Query().Get("cursor") == "" {
					w.Header().Set("X-Repo-Commit", commit)
				}
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
				if end > len(nodes) {
					end = len(nodes)
				}
				if end < len(nodes) {
					w.Header().Set("Link", fmt.Sprintf("<%s?recursive=true&cursor=%d>; rel=\"next\"", r.URL.Path, end))
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(nodes[cursor:end])
			}))
			defer srv.Close()

			a := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()})
			files, gotCommit, err := a.fetchFileTree(context.Background(), "owner/repo", tc.isDataset, "main")
			if err != nil {
				t.Fatalf("fetchFileTree: %v", err)
			}

			// X-Repo-Commit is captured from the first response (unchanged).
			if gotCommit != commit {
				t.Errorf("commit = %q, want %q (X-Repo-Commit from first response)", gotCommit, commit)
			}
			if len(files) != 10 {
				t.Fatalf("got %d files, want 10 (the directory node must not become a file)", len(files))
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

			byPath := make(map[string]FileInfo, len(files))
			for _, f := range files {
				byPath[f.Path] = f
			}
			// FileInfo semantics must be unchanged: Size comes from LFS.Size
			// for LFS files, SHA256 from lfs.sha256 (lfs.oid as fallback),
			// and Name/Directory derive from the path.
			checks := []struct {
				path      string
				size      int64
				isLFS     bool
				sha256    string
				name      string
				directory string
			}{
				{"config.json", 100, false, "", "config.json", "."},
				{"onnx/model.onnx", 300, false, "", "model.onnx", "onnx"},
				{"model.safetensors", 9999, true, hexA, "model.safetensors", "."},
				{"shard.bin", 555, true, hexB, "shard.bin", "."},
			}
			for _, c := range checks {
				f, ok := byPath[c.path]
				if !ok {
					t.Errorf("missing file %q", c.path)
					continue
				}
				if f.Size != c.size || f.IsLFS != c.isLFS || f.SHA256 != c.sha256 ||
					f.Name != c.name || f.Directory != c.directory {
					t.Errorf("file %q = {size:%d lfs:%v sha:%q name:%q dir:%q}, want {size:%d lfs:%v sha:%q name:%q dir:%q}",
						c.path, f.Size, f.IsLFS, f.SHA256, f.Name, f.Directory,
						c.size, c.isLFS, c.sha256, c.name, c.directory)
				}
			}
		})
	}
}

// TestFetchFileTree_TerminalFailsFast pins the fail-fast contract on the
// analyze path: terminal tree responses surface the historical messages
// after exactly one request, with no retry loop.
func TestFetchFileTree_TerminalFailsFast(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var treeReqs atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "/tree/") {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				treeReqs.Add(1)
				w.WriteHeader(status)
			}))
			defer srv.Close()

			a := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()})
			_, _, err := a.fetchFileTree(context.Background(), "owner/repo", true, "main")
			want := map[int]string{
				http.StatusUnauthorized: "unauthorized: repo requires token or you do not have access",
				http.StatusForbidden:    fmt.Sprintf("forbidden: please accept the repository terms at %s/datasets/owner/repo", srv.URL),
				http.StatusNotFound:     "repository not found: owner/repo",
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

func TestFetchFileTree_PaginationCompletion(t *testing.T) {
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
					_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: fmt.Sprintf("f%d.bin", page), Size: page}})
				}))
				defer srv.Close()
				a := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()})
				files, _, err := a.fetchFileTree(context.Background(), "owner/repo", dataset, "main")
				if (err != nil) != tc.wantErr {
					t.Fatalf("fetchFileTree error = %v, want error %v", err, tc.wantErr)
				}
				if got := reqs.Load(); got != tc.requests {
					t.Errorf("requests = %d, want %d", got, tc.requests)
				}
				if !tc.wantErr && len(files) != int(tc.requests) {
					t.Errorf("files = %d, want %d", len(files), tc.requests)
				}
			})
		}
	}
}

// The two reported headers must also survive the public analysis workflow.
// Model autodetection probes the dataset route separately; that route is 404
// here so it cannot hide pagination results behind ErrBothExist.
func TestAnalyze_TokenMetadataPagination(t *testing.T) {
	for _, dataset := range []bool{false, true} {
		for _, header := range []string{
			`<?cursor=0>; title=owner's; rel=prev, <?cursor=2>; rel=next`,
			`<?cursor=2>; title=owner's; rel=next`,
		} {
			t.Run(fmt.Sprintf("dataset=%v/%s", dataset, header), func(t *testing.T) {
				api := "models"
				if dataset {
					api = "datasets"
				}
				treePath := "/api/" + api + "/owner/repo/tree/main"
				var reqs, missingDatasetReqs atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != treePath {
						if !dataset && r.URL.Path == "/api/datasets/owner/repo/tree/main" {
							missingDatasetReqs.Add(1)
							w.WriteHeader(http.StatusNotFound)
							return
						}
						if r.URL.Path == "/api/"+api+"/owner/repo/refs" {
							_, _ = w.Write([]byte(`{"branches":[],"tags":[]}`))
							return
						}
						t.Errorf("unexpected analysis request: %s", r.URL)
						w.WriteHeader(http.StatusNotFound)
						return
					}
					page := reqs.Add(1)
					if page == 1 {
						w.Header().Set("Link", header)
						w.Header().Set("X-Repo-Commit", "cafebabe")
					} else if page != 2 || r.URL.Query().Get("cursor") != "2" {
						t.Errorf("unexpected pagination request: %s", r.URL)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: fmt.Sprintf("f%d.txt", page), Size: page}})
				}))
				defer srv.Close()
				a := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()})
				info, err := a.Analyze(context.Background(), "owner/repo", dataset)
				if err != nil {
					t.Fatalf("Analyze: %v", err)
				}
				if info.FileCount != 2 || len(info.Files) != 2 || info.Files[0].Path != "f1.txt" || info.Files[1].Path != "f2.txt" || info.IsDataset != dataset || info.Commit != "cafebabe" {
					t.Fatalf("incomplete analysis: %+v", info)
				}
				if got := reqs.Load(); got != 2 {
					t.Errorf("selected API tree requests = %d, want 2", got)
				}
				wantMissing := int64(1)
				if dataset {
					wantMissing = 0
				}
				if got := missingDatasetReqs.Load(); got != wantMissing {
					t.Errorf("opposite API probes = %d, want %d", got, wantMissing)
				}
			})
		}
	}
}

// Analysis retains literal repository paths; file-safety rejection belongs to
// the downloader, not a new shared directory/callback policy.
func TestFetchFileTree_BackslashDirectoryTraversal(t *testing.T) {
	var reqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		nodes := []hfTreeNode{{Type: "file", Path: "ok.bin"}, {Type: "directory", Path: `win\dir`}}
		if strings.HasSuffix(r.URL.Path, `/win\dir`) {
			if !strings.Contains(r.URL.EscapedPath(), "win%5Cdir") {
				t.Errorf("backslash not escaped: %s", r.URL.EscapedPath())
			}
			nodes = []hfTreeNode{{Type: "file", Path: `win\dir/evil/file.bin`}}
		} else if !strings.HasSuffix(r.URL.Path, "/tree/main") {
			t.Errorf("unexpected request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(nodes)
	}))
	defer srv.Close()
	a := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()})
	files, _, err := a.fetchFileTree(context.Background(), "owner/repo", false, "main")
	if err != nil || len(files) != 2 || files[1].Path != `win\dir/evil/file.bin` {
		t.Fatalf("files = %v, error = %v; want both literal file paths", files, err)
	}
	if got := reqs.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}
