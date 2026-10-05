// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sync"
	"testing"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := Config{
		Addr:        "127.0.0.1",
		Port:        0, // Random port
		CacheDir:    t.TempDir(),
		Concurrency: 2,
		MaxActive:   1,
	}
	return newTestServerWithConfig(t, cfg)
}

func newTestServerWithConfig(t *testing.T, cfg Config) *Server {
	t.Helper()
	hub := NewWSHub()
	return &Server{config: cfg, jobs: newTestJobManager(t, cfg, hub), wsHub: hub}
}

func TestAPI_Health(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()

	srv.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["status"] != "ok" {
		t.Errorf("Expected status ok, got %v", resp["status"])
	}
	if v, _ := resp["version"].(string); v == "" {
		t.Errorf("Expected non-empty version string, got %v", resp["version"])
	}
}

func TestScanLocalCachedRepos(t *testing.T) {
	root := t.TempDir()
	localDir := filepath.Join(root, "local")
	cacheDir := filepath.Join(root, "hf")

	lmRepo := filepath.Join(localDir, "bartowski", "Qwen3-Coder-Next-GGUF")
	if err := os.MkdirAll(lmRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lmRepo, "model-Q4_K_M.gguf"), []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}

	friendlyRepo := filepath.Join(cacheDir, "models", "owner", "model")
	if err := os.MkdirAll(friendlyRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(friendlyRepo, "model.safetensors"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}

	repos, err := scanLocalCachedRepos(cacheDir, localDir, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]string{}
	for _, repo := range repos {
		found[repo.Repo] = repo.Source
	}
	if found["bartowski/Qwen3-Coder-Next-GGUF"] != "Local" {
		t.Fatalf("expected LM-style local repo, got %#v", found)
	}
	if found["owner/model"] != "Friendly view" {
		t.Fatalf("expected friendly-view repo, got %#v", found)
	}
}

func TestLocalCachedRepos_RootRestrictions(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		for _, friendlyOverlap := range []bool{false, true} {
			t.Run(fmt.Sprintf("overlap-%d/friendly-%v", mask, friendlyOverlap), func(t *testing.T) {
				base := t.TempDir()
				cacheDir := filepath.Join(base, "cache")
				friendlyDir := filepath.Join(cacheDir, "models")
				otherDir := filepath.Join(base, "local")
				writeWeight := func(path string) {
					t.Helper()
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("weights"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				// Nested weights ensure internals would qualify as false repos.
				blocked := []string{"hub/models--fake--repo", "models/owner", "datasets/fake", "blobs/fake", "snapshots/fake", "refs/fake", "HuB/fake"}
				for _, id := range blocked {
					if id == "models/owner" {
						continue // The friendly owner/real fixture supplies nested weights.
					}
					writeWeight(filepath.Join(cacheDir, filepath.FromSlash(id), "nested", "model.safetensors"))
				}
				writeWeight(filepath.Join(friendlyDir, "owner", "real", "nested", "model.safetensors"))
				writeWeight(filepath.Join(friendlyDir, "models", "friendly", "nested", "model.safetensors"))
				writeWeight(filepath.Join(cacheDir, "ordinary", "raw", "nested", "model.safetensors"))
				writeWeight(filepath.Join(otherDir, "ordinary", "local", "nested", "model.safetensors"))
				writeWeight(filepath.Join(otherDir, "models", "owner", "nested", "model.safetensors"))

				localDir := ""
				var scanDirs []string
				var routes map[string]string
				if mask&1 != 0 {
					routes = map[string]string{"audio": cacheDir, "embedding": cacheDir + string(filepath.Separator)}
				}
				if mask&2 != 0 {
					localDir = cacheDir
				}
				if mask&4 != 0 {
					scanDirs = append(scanDirs, cacheDir+string(filepath.Separator))
				}
				if friendlyOverlap {
					scanDirs = append(scanDirs, friendlyDir)
					if routes == nil {
						routes = make(map[string]string)
					}
					routes["llm"] = friendlyDir
				}
				// When LocalDir is the cache, lookup must skip that root and
				// continue here for the otherwise excluded models/owner ID.
				scanDirs = append(scanDirs, otherDir)
				want := map[string]localCacheRoot{
					"owner/real":      {Path: filepath.Join(friendlyDir, "owner", "real"), Source: "Friendly view"},
					"models/friendly": {Path: filepath.Join(friendlyDir, "models", "friendly"), Source: "Friendly view"},
					"ordinary/raw":    {Path: filepath.Join(cacheDir, "ordinary", "raw"), Source: "Local"},
					"ordinary/local":  {Path: filepath.Join(otherDir, "ordinary", "local"), Source: "Local"},
					"models/owner":    {Path: filepath.Join(otherDir, "models", "owner"), Source: "Local"},
				}
				for _, includeFiles := range []bool{false, true} {
					t.Run(fmt.Sprintf("files-%v", includeFiles), func(t *testing.T) {
						checkRepo := func(repo CachedRepoInfo) {
							t.Helper()
							expected, ok := want[repo.Repo]
							if !ok || repo.Path != expected.Path || repo.Source != expected.Source {
								t.Errorf("unexpected repo %s at %s (%s)", repo.Repo, repo.Path, repo.Source)
							}
							if repo.FileCount != 1 || repo.Size != int64(len("weights")) {
								t.Errorf("repo %s: fileCount=%d size=%d", repo.Repo, repo.FileCount, repo.Size)
							}
							if includeFiles {
								if len(repo.Files) != 1 || repo.Files[0].Name != filepath.Join("nested", "model.safetensors") {
									t.Errorf("repo %s: files = %#v", repo.Repo, repo.Files)
								}
							} else if len(repo.Files) != 0 {
								t.Errorf("repo %s: unwanted file list", repo.Repo)
							}
						}
						repos, err := scanLocalCachedRepos(cacheDir, localDir, scanDirs, routes, includeFiles)
						if err != nil {
							t.Fatal(err)
						}
						if len(repos) != len(want) {
							t.Errorf("scan returned %d repos, want %d", len(repos), len(want))
						}
						seen := make(map[string]bool)
						for _, repo := range repos {
							if seen[repo.Repo] {
								t.Errorf("duplicate repo %s", repo.Repo)
							}
							seen[repo.Repo] = true
							checkRepo(repo)
						}
						for id := range want {
							if !seen[id] {
								t.Errorf("scan omitted %s", id)
							}
							info, err := findLocalCachedRepo(cacheDir, localDir, scanDirs, routes, id, includeFiles)
							if err != nil {
								t.Errorf("lookup %s: %v", id, err)
								continue
							}
							checkRepo(*info)
						}
						for _, id := range blocked {
							if _, ok := want[id]; ok {
								continue // models/owner exists in the independent local root.
							}
							if _, err := findLocalCachedRepo(cacheDir, localDir, scanDirs, routes, id, includeFiles); !os.IsNotExist(err) {
								t.Errorf("lookup exposed internal %s: %v", id, err)
							}
						}
					})
				}
			})
		}
	}
}

func TestLocalCachedRepos_NestedRoots(t *testing.T) {
	type expectedRepo struct {
		path, source string
		files        []string
		quants       []string
	}
	for _, tc := range []struct {
		name, cache, local string
		scanDirs           []string
		routes             map[string]string
		files              []string
		want               map[string]expectedRepo
		absent             []string
		linuxOnly          bool
		relativeRoutes     bool
	}{
		{
			name: "local-and-fine", local: "models",
			routes: map[string]string{"llm/gguf": "models/LLM/GGUF"},
			files:  []string{"models/LLM/GGUF/owner/model/foo.gguf"},
			want:   map[string]expectedRepo{"owner/model": {path: "models/LLM/GGUF/owner/model", files: []string{"foo.gguf"}}},
			absent: []string{"LLM/GGUF"},
		},
		{
			name:   "coarse-and-fine-without-local",
			routes: map[string]string{"llm": "models/LLM", "llm/gguf": "models/LLM/GGUF"},
			files:  []string{"models/LLM/GGUF/owner/model/foo.gguf", "models/LLM/coarse/real/shards/model.safetensors"},
			want: map[string]expectedRepo{
				"owner/model": {path: "models/LLM/GGUF/owner/model", files: []string{"foo.gguf"}},
				"coarse/real": {path: "models/LLM/coarse/real", files: []string{"shards/model.safetensors"}},
			},
			absent: []string{"GGUF/owner"},
		},
		{
			name: "three-root-levels", local: "models",
			routes: map[string]string{"llm": "models/LLM", "llm/gguf": "models/LLM/GGUF"},
			files: []string{
				"models/parent/real/shards/model.safetensors", "models/LLM/coarse/real/model.safetensors",
				"models/LLM/GGUF/owner/model/foo.gguf",
			},
			want: map[string]expectedRepo{
				"parent/real": {path: "models/parent/real", files: []string{"shards/model.safetensors"}},
				"coarse/real": {path: "models/LLM/coarse/real", files: []string{"model.safetensors"}},
				"owner/model": {path: "models/LLM/GGUF/owner/model", files: []string{"foo.gguf"}},
			},
			absent: []string{"LLM/GGUF", "LLM/coarse", "GGUF/owner"},
		},
		{
			name: "absolute-local-relative-route", local: "models", relativeRoutes: true,
			routes: map[string]string{"llm/gguf": "models/LLM/GGUF"},
			files:  []string{"models/LLM/GGUF/owner/model/foo.gguf"},
			want:   map[string]expectedRepo{"owner/model": {path: "models/LLM/GGUF/owner/model", files: []string{"foo.gguf"}}},
			absent: []string{"LLM/GGUF"},
		},
		{
			name: "siblings-and-real-ancestor", local: "models",
			routes: map[string]string{"llm/gguf": "models/LLM/GGUF", "audio": "models/Audio"},
			files:  []string{"models/LLM/GGUF/owner/model/foo.gguf", "models/Audio/sound/voice/model.safetensors", "models/parent/real/shards/deep/model.safetensors"},
			want: map[string]expectedRepo{
				"owner/model": {path: "models/LLM/GGUF/owner/model", files: []string{"foo.gguf"}},
				"sound/voice": {path: "models/Audio/sound/voice", files: []string{"model.safetensors"}},
				"parent/real": {path: "models/parent/real", files: []string{"shards/deep/model.safetensors"}},
			},
			absent: []string{"LLM/GGUF", "Audio/sound"},
		},
		{
			name: "scan-dir-overlap", local: "models", scanDirs: []string{"models", "models/LLM/GGUF"},
			routes: map[string]string{"llm/gguf": "models/LLM/GGUF/"},
			files:  []string{"models/LLM/GGUF/owner/model/foo.gguf", "models/parent/real/shards/model.safetensors"},
			want: map[string]expectedRepo{
				"owner/model": {path: "models/LLM/GGUF/owner/model", files: []string{"foo.gguf"}},
				"parent/real": {path: "models/parent/real", files: []string{"shards/model.safetensors"}},
			},
			absent: []string{"LLM/GGUF"},
		},
		{
			name: "friendly-and-raw-overlap", cache: "cache", local: "cache", scanDirs: []string{"cache", "cache/models"},
			routes: map[string]string{"llm": "cache", "llm/gguf": "cache/models/LLM/GGUF"},
			files: []string{
				"cache/models/LLM/GGUF/owner/model/foo.gguf", "cache/models/friendly/real/model.safetensors",
				"cache/raw/real/model.safetensors", "cache/hub/fake/nested/model.safetensors",
			},
			want: map[string]expectedRepo{
				"owner/model":   {path: "cache/models/LLM/GGUF/owner/model", files: []string{"foo.gguf"}},
				"friendly/real": {path: "cache/models/friendly/real", source: "Friendly view", files: []string{"model.safetensors"}},
				"raw/real":      {path: "cache/raw/real", files: []string{"model.safetensors"}},
			},
			absent: []string{"LLM/GGUF", "models/LLM", "hub/fake"},
		},
		{
			name: "descendant-inside-real-repo", local: "models",
			routes: map[string]string{"llm/gguf": "models/parent/real/routed"},
			files: []string{
				"models/parent/real/shards/own-Q4_K_M.gguf", "models/parent/real/routed/owner/model/child-Q8_0.gguf",
				"models/parent/real/routed/owner/model/mmproj-F16.gguf",
			},
			want: map[string]expectedRepo{
				"parent/real": {path: "models/parent/real", files: []string{"shards/own-Q4_K_M.gguf"}, quants: []string{"Q4_K_M"}},
				"owner/model": {path: "models/parent/real/routed/owner/model", files: []string{"child-Q8_0.gguf", "mmproj-F16.gguf"}, quants: []string{"Q8_0"}},
			},
		},
		{
			name: "descendant-only-weights-do-not-qualify-parent", local: "models",
			routes: map[string]string{"llm/gguf": "models/parent/empty/routed"},
			files:  []string{"models/parent/empty/README.md", "models/parent/empty/routed/owner/model/foo.gguf"},
			want:   map[string]expectedRepo{"owner/model": {path: "models/parent/empty/routed/owner/model", files: []string{"foo.gguf"}}},
			absent: []string{"parent/empty"},
		},
		{
			name: "lookup-continues-after-excluded-candidate", local: "models", scanDirs: []string{"other"},
			routes: map[string]string{"llm/gguf": "models/LLM/GGUF"},
			files:  []string{"models/LLM/GGUF/owner/model/foo.gguf", "other/LLM/GGUF/shards/model.safetensors"},
			want: map[string]expectedRepo{
				"owner/model": {path: "models/LLM/GGUF/owner/model", files: []string{"foo.gguf"}},
				"LLM/GGUF":    {path: "other/LLM/GGUF", files: []string{"shards/model.safetensors"}},
			},
		},
		{
			name: "prefix-boundary", local: "models",
			routes: map[string]string{"llm/gguf": "models/LLM/GGUF"},
			files:  []string{"models/LLM/GGUF/owner/model/foo.gguf", "models/LLM/GGUF-other/shards/model.safetensors"},
			want: map[string]expectedRepo{
				"owner/model":    {path: "models/LLM/GGUF/owner/model", files: []string{"foo.gguf"}},
				"LLM/GGUF-other": {path: "models/LLM/GGUF-other", files: []string{"shards/model.safetensors"}},
			},
			absent: []string{"LLM/GGUF"},
		},
		{
			name: "case-distinct", local: "models", linuxOnly: true,
			routes: map[string]string{"llm/gguf": "models/LLM/GGUF"},
			files:  []string{"models/LLM/GGUF/owner/model/foo.gguf", "models/LLM/gguf/shards/model.safetensors"},
			want: map[string]expectedRepo{
				"owner/model": {path: "models/LLM/GGUF/owner/model", files: []string{"foo.gguf"}},
				"LLM/gguf":    {path: "models/LLM/gguf", files: []string{"shards/model.safetensors"}},
			},
			absent: []string{"LLM/GGUF"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.linuxOnly && runtime.GOOS != "linux" {
				t.Skip("requires Linux case-sensitive directories")
			}
			base := t.TempDir()
			path := func(relative string) string {
				if relative == "" {
					return ""
				}
				return filepath.Join(base, filepath.FromSlash(relative))
			}
			cacheDir := path(tc.cache)
			if cacheDir == "" {
				cacheDir = path("cache")
			}
			var scanDirs []string
			for _, dir := range tc.scanDirs {
				scanDirs = append(scanDirs, path(dir))
			}
			routes := make(map[string]string)
			for key, dir := range tc.routes {
				routes[key] = path(dir)
				if tc.relativeRoutes {
					cwd, err := os.Getwd()
					if err != nil {
						t.Fatal(err)
					}
					routes[key], err = filepath.Rel(cwd, routes[key])
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, file := range tc.files {
				if err := os.MkdirAll(filepath.Dir(path(file)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path(file), []byte("weights"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, includeFiles := range []bool{false, true} {
				t.Run(fmt.Sprintf("files-%v", includeFiles), func(t *testing.T) {
					check := func(repo CachedRepoInfo) {
						t.Helper()
						want, ok := tc.want[repo.Repo]
						if !ok {
							t.Errorf("synthetic/unexpected repo %s at %s", repo.Repo, repo.Path)
							return
						}
						source := want.source
						if source == "" {
							source = "Local"
						}
						absolute, err := filepath.Abs(repo.Path)
						if err != nil {
							t.Fatal(err)
						}
						if absolute != path(want.path) || repo.Source != source {
							t.Errorf("%s: path/source = %s/%s, want %s/%s", repo.Repo, repo.Path, repo.Source, path(want.path), source)
						}
						if repo.FileCount != len(want.files) || repo.Size != int64(len(want.files)*len("weights")) {
							t.Errorf("%s: fileCount/size = %d/%d, want %d/%d", repo.Repo, repo.FileCount, repo.Size, len(want.files), len(want.files)*len("weights"))
						}
						var names, mmproj []string
						for _, file := range repo.Files {
							names = append(names, file.Name)
						}
						var wantNames []string
						wantMMProj := false
						for _, file := range want.files {
							if includeFiles {
								wantNames = append(wantNames, filepath.FromSlash(file))
							}
							if filepath.Base(file) == "mmproj-F16.gguf" {
								wantMMProj = true
								if includeFiles {
									mmproj = append(mmproj, filepath.FromSlash(file))
								}
							}
						}
						if !reflect.DeepEqual(names, wantNames) || !reflect.DeepEqual(repo.Quantizations, want.quants) || repo.HasMMProj != wantMMProj || !reflect.DeepEqual(repo.MMProjFiles, mmproj) {
							t.Errorf("%s: files/metadata = %v/%v/%v/%v, want %v/%v/%v/%v", repo.Repo, names, repo.Quantizations, repo.HasMMProj, repo.MMProjFiles, wantNames, want.quants, wantMMProj, mmproj)
						}
					}
					repos, err := scanLocalCachedRepos(cacheDir, path(tc.local), scanDirs, routes, includeFiles)
					if err != nil {
						t.Fatal(err)
					}
					if len(repos) != len(tc.want) {
						t.Errorf("scan returned %d repos, want %d", len(repos), len(tc.want))
					}
					seen := make(map[string]bool)
					for _, repo := range repos {
						if seen[repo.Repo] {
							t.Errorf("duplicate repo %s", repo.Repo)
						}
						seen[repo.Repo] = true
						check(repo)
					}
					for id := range tc.want {
						if !seen[id] {
							t.Errorf("scan omitted %s", id)
						}
						repo, err := findLocalCachedRepo(cacheDir, path(tc.local), scanDirs, routes, id, includeFiles)
						if err != nil {
							t.Errorf("lookup %s: %v", id, err)
							continue
						}
						check(*repo)
					}
					for _, id := range tc.absent {
						if _, err := findLocalCachedRepo(cacheDir, path(tc.local), scanDirs, routes, id, includeFiles); !os.IsNotExist(err) {
							t.Errorf("lookup exposed synthetic repo %s: %v", id, err)
						}
					}
				})
			}
		})
	}
}

func TestLocalCacheRoot_SubrootCaseSemantics(t *testing.T) {
	base := t.TempDir()
	root := localCacheRoot{Path: filepath.Join(base, "Models")}
	child := localCacheRoot{Path: filepath.Join(base, "models", "GGUF")}
	roots := []localCacheRoot{root, child}
	got := root.excludedSubroots(roots)
	var want []string
	if runtime.GOOS == "windows" {
		want = []string{pathIdentityKey(child.Path)}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("case-different parent: excluded = %v, want %v", got, want)
	}
}

func TestAPI_CacheList_IncludesLocalRepos(t *testing.T) {
	cacheDir := t.TempDir()
	localDir := t.TempDir()
	repoDir := filepath.Join(localDir, "Abiray", "Qwen3-Coder-Next-GGUF")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "qwen-Q3_K_XL.gguf"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "mmproj-F16.gguf"), []byte("vision"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := newTestServerWithConfig(t, Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	req := httptest.NewRequest("GET", "/api/cache", nil)
	w := httptest.NewRecorder()
	srv.handleCacheList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Repos []CachedRepoInfo `json:"repos"`
		Stats CacheStats       `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Stats.TotalModels != 1 {
		t.Fatalf("TotalModels = %d, want 1", resp.Stats.TotalModels)
	}
	if len(resp.Repos) != 1 {
		t.Fatalf("Repos length = %d, want 1", len(resp.Repos))
	}
	if resp.Repos[0].Repo != "Abiray/Qwen3-Coder-Next-GGUF" || resp.Repos[0].Source != "Local" {
		t.Fatalf("unexpected repo: %#v", resp.Repos[0])
	}
	if len(resp.Repos[0].Quantizations) != 1 || resp.Repos[0].Quantizations[0] != "Q3_K_XL" {
		t.Fatalf("Quantizations = %#v, want [Q3_K_XL]", resp.Repos[0].Quantizations)
	}
	if !resp.Repos[0].HasMMProj {
		t.Fatalf("HasMMProj = false, want true")
	}
	if len(resp.Repos[0].Capabilities) != 1 || resp.Repos[0].Capabilities[0] != "vision" {
		t.Fatalf("Capabilities = %#v, want [vision]", resp.Repos[0].Capabilities)
	}
}

// addCacheTestGGUF uses the real blob/snapshot/friendly projection helpers, so
// metadata tests exercise symlink names without substituting snapshot metadata.
func addCacheTestGGUF(t *testing.T, repo *hfdownloader.RepoDir, commit, name, filter string, friendly bool) {
	t.Helper()
	name = filepath.FromSlash(name)
	data := []byte(name)
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	if err := os.MkdirAll(repo.BlobsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repo.BlobPath(hash), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateSnapshot(commit, []hfdownloader.SnapshotFile{{RelativePath: name, SHA256: hash}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.WriteRef("main", commit); err != nil {
		t.Fatal(err)
	}
	if friendly {
		if err := repo.CreateFriendlySymlink(commit, name, filepath.FromSlash(filter)); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(repo.FriendlyPath(), filepath.FromSlash(filter), name)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("friendly file is not a symlink: %s (%v)", path, err)
		}
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("friendly link does not resolve to blob: %s (%v)", path, err)
		}
	}
}

func getCacheTestJSON(t *testing.T, mux *http.ServeMux, path string, result any) {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d: %s", path, w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
		t.Fatal(err)
	}
}

func checkCacheTestMetadata(t *testing.T, repo CachedRepoInfo, quants, mmproj []string, detail bool) {
	t.Helper()
	var capabilities, paths []string
	if len(mmproj) > 0 {
		capabilities = []string{"vision"}
		if detail {
			paths = mmproj
		}
	}
	if !reflect.DeepEqual(repo.Quantizations, quants) || repo.HasMMProj != (len(mmproj) > 0) ||
		!reflect.DeepEqual(repo.Capabilities, capabilities) || !reflect.DeepEqual(repo.MMProjFiles, paths) {
		t.Errorf("%s metadata = %v/%v/%v/%v, want %v/%v/%v/%v", repo.Repo,
			repo.Quantizations, repo.HasMMProj, repo.Capabilities, repo.MMProjFiles,
			quants, len(mmproj) > 0, capabilities, paths)
	}
}

func TestAPI_CacheHFFriendlyMetadata_Ownership(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("production HF projection helpers do not create symlinks on Windows")
	}
	t.Setenv("HF_HUB_CACHE", "")
	for _, repoType := range []hfdownloader.RepoType{hfdownloader.RepoTypeModel, hfdownloader.RepoTypeDataset} {
		for _, tc := range []struct {
			name, registration, subroot string
			relative, retained          bool
			prunesRepo                  bool
		}{
			{name: "route-nested", registration: "route", subroot: "parent/real/routed"},
			{name: "local-nested", registration: "local", subroot: "parent/real/routed"},
			{name: "scan-nested", registration: "scan", subroot: "parent/real/routed"},
			{name: "relative-route", registration: "route", subroot: "parent/real/routed", relative: true},
			{name: "equal-repo", registration: "route", subroot: "parent/real", prunesRepo: true},
			{name: "enclosing-repo", registration: "scan", subroot: "parent", prunesRepo: true},
			{name: "equal-library", registration: "route", subroot: "."},
			{name: "enclosing-library", registration: "local", subroot: ".."},
			{name: "retained-filtered-projection", registration: "route", subroot: "parent/real/routed", retained: true},
		} {
			// Registering the dataset library itself as a model scan root has
			// existing dual-type listing semantics outside this metadata fix.
			if repoType == hfdownloader.RepoTypeDataset && tc.name == "equal-library" {
				continue
			}
			t.Run(string(repoType)+"/"+tc.name, func(t *testing.T) {
				base := t.TempDir()
				cache := hfdownloader.NewHFCache(filepath.Join(base, "cache"), 0)
				parent, err := cache.Repo("parent/real", repoType)
				if err != nil {
					t.Fatal(err)
				}
				addCacheTestGGUF(t, parent, "aaaaaaaa", "own-Q4_K_M.gguf", "", true)
				library := cache.ModelsDir()
				if repoType == hfdownloader.RepoTypeDataset {
					library = cache.DatasetsDir()
				}
				root := filepath.Join(library, filepath.FromSlash(tc.subroot))
				writeFile := func(path string) {
					t.Helper()
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("weights"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				childPath := filepath.Join(root, "child", "model")
				writeFile(filepath.Join(childPath, "child-Q8_0.gguf"))
				writeFile(filepath.Join(childPath, "mmproj-F16.gguf"))
				// Unrelated local, friendly-only and raw-cache repos must survive.
				independent := filepath.Join(base, "independent")
				writeFile(filepath.Join(independent, "sibling", "real", "model.safetensors"))
				writeFile(filepath.Join(cache.ModelsDir(), "friendly", "only", "model.safetensors"))
				writeFile(filepath.Join(cache.Root, "ordinary", "raw", "model.safetensors"))
				quants := []string{"Q4_K_M"}
				var mmproj []string
				if tc.retained {
					// Both revisions remain in the projection. Deep/filter-prefixed
					// own files and a prefix sibling of the excluded root stay owned.
					addCacheTestGGUF(t, parent, "bbbbbbbb", "deep/own-Q5_K_M.gguf", "selected", true)
					addCacheTestGGUF(t, parent, "bbbbbbbb", "deep/mmproj-F16.gguf", "routed-other", true)
					quants = []string{"Q4_K_M", "Q5_K_M"}
					mmproj = []string{filepath.FromSlash("routed-other/deep/mmproj-F16.gguf")}
				}
				if tc.prunesRepo {
					quants = nil
				}
				configuredRoot := root
				if tc.relative {
					cwd, err := os.Getwd()
					if err != nil {
						t.Fatal(err)
					}
					configuredRoot, err = filepath.Rel(cwd, root)
					if err != nil {
						t.Fatal(err)
					}
				}
				cfg := Config{CacheDir: cache.Root, LocalScanDirs: []string{independent}}
				switch tc.registration {
				case "route":
					cfg.DownloadRoutes = map[string]string{"llm/gguf": configuredRoot}
				case "local":
					cfg.LocalDir = configuredRoot
				case "scan":
					cfg.LocalScanDirs = append(cfg.LocalScanDirs, configuredRoot)
				}
				srv := &Server{config: cfg}
				mux := http.NewServeMux()
				srv.registerAPIRoutes(mux)
				var list struct {
					Repos []CachedRepoInfo `json:"repos"`
					Stats CacheStats       `json:"stats"`
				}
				getCacheTestJSON(t, mux, "/api/cache", &list)
				if len(list.Repos) != 5 || list.Stats.TotalModels+list.Stats.TotalDatasets != 5 {
					t.Errorf("list/stats count = %d/%d, want 5 without synthetic repos", len(list.Repos), list.Stats.TotalModels+list.Stats.TotalDatasets)
				}
				want := map[string]bool{"parent/real": true, "child/model": true, "sibling/real": true, "friendly/only": true, "ordinary/raw": true}
				for _, repo := range list.Repos {
					if !want[repo.Repo] {
						t.Errorf("synthetic or duplicate repo: %s (%s)", repo.Repo, repo.Path)
					}
					delete(want, repo.Repo)
					var detail CachedRepoInfo
					getCacheTestJSON(t, mux, "/api/cache/"+repo.Repo, &detail)
					if detail.Path != repo.Path || detail.Source != repo.Source || detail.Type != repo.Type {
						t.Errorf("%s list/detail ownership mismatch: %#v / %#v", repo.Repo, repo, detail)
					}
					switch repo.Repo {
					case "parent/real":
						if repo.Source != "HF cache" || repo.Path != parent.Path() || repo.FriendlyPath != parent.FriendlyPath() || repo.Type != string(repoType) {
							t.Errorf("parent lost HF precedence: %#v", repo)
						}
						checkCacheTestMetadata(t, repo, quants, mmproj, false)
						checkCacheTestMetadata(t, detail, quants, mmproj, true)
						// Ownership filtering must not change snapshot file accounting.
						if len(detail.Files) != 1 || detail.Files[0].Name != "own-Q4_K_M.gguf" || detail.Size != int64(len("own-Q4_K_M.gguf")) {
							t.Errorf("parent snapshot accounting changed: %#v", detail)
						}
					case "child/model":
						absPath, err := filepath.Abs(repo.Path)
						if err != nil || absPath != childPath || repo.FileCount != 2 || detail.FileCount != 2 || len(detail.Files) != 2 {
							t.Errorf("child ownership/accounting changed: %#v / %#v (%v)", repo, detail, err)
						}
						checkCacheTestMetadata(t, repo, []string{"Q8_0"}, []string{"mmproj-F16.gguf"}, false)
						checkCacheTestMetadata(t, detail, []string{"Q8_0"}, []string{"mmproj-F16.gguf"}, true)
					default:
						if repo.FileCount != 1 || detail.FileCount != 1 {
							t.Errorf("preserved repo accounting changed: %#v / %#v", repo, detail)
						}
						checkCacheTestMetadata(t, repo, nil, nil, false)
						checkCacheTestMetadata(t, detail, nil, nil, true)
					}
				}
				if len(want) != 0 {
					t.Errorf("omitted repos: %v", want)
				}
			})
		}
	}
}

func TestAPI_CacheHFFriendlyMetadata_NoProjection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("real snapshot symlink fixture requires non-Windows HF helpers")
	}
	t.Setenv("HF_HUB_CACHE", "")
	for _, repoType := range []hfdownloader.RepoType{hfdownloader.RepoTypeModel, hfdownloader.RepoTypeDataset} {
		t.Run(string(repoType), func(t *testing.T) {
			cache := hfdownloader.NewHFCache(t.TempDir(), 0)
			parent, err := cache.Repo("parent/real", repoType)
			if err != nil {
				t.Fatal(err)
			}
			addCacheTestGGUF(t, parent, "aaaaaaaa", "own-Q4_K_M.gguf", "", false)
			addCacheTestGGUF(t, parent, "aaaaaaaa", "mmproj-F16.gguf", "", false)
			srv := &Server{config: Config{CacheDir: cache.Root}}
			mux := http.NewServeMux()
			srv.registerAPIRoutes(mux)
			var list struct {
				Repos []CachedRepoInfo `json:"repos"`
			}
			getCacheTestJSON(t, mux, "/api/cache", &list)
			if len(list.Repos) != 1 || list.Repos[0].Source != "HF cache" {
				t.Fatalf("missing HF record: %#v", list.Repos)
			}
			var detail CachedRepoInfo
			getCacheTestJSON(t, mux, "/api/cache/parent/real", &detail)
			if len(detail.Files) != 2 || len(detail.Snapshots) != 1 || detail.FileCount != 2 {
				t.Errorf("snapshot fixture not visible: %#v", detail)
			}
			checkCacheTestMetadata(t, list.Repos[0], nil, nil, false)
			checkCacheTestMetadata(t, detail, nil, nil, true)
		})
	}
}

func TestAPI_CacheHFFriendlyMetadata_ConfigSnapshot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("production HF projection helpers do not create symlinks on Windows")
	}
	t.Setenv("HF_HUB_CACHE", "")
	cache := hfdownloader.NewHFCache(t.TempDir(), 0)
	parent, err := cache.Repo("parent/real", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	addCacheTestGGUF(t, parent, "aaaaaaaa", "own-Q4_K_M.gguf", "", true)
	// Existing parent projection content changes ownership when this directory
	// is registered/unregistered; no sticky lifecycle reservation is intended.
	addCacheTestGGUF(t, parent, "aaaaaaaa", "child/model/child-Q8_0.gguf", "routed", true)
	addCacheTestGGUF(t, parent, "aaaaaaaa", "child/model/mmproj-F16.gguf", "routed", true)
	root := filepath.Join(parent.FriendlyPath(), "routed")
	srv := &Server{config: Config{CacheDir: cache.Root}}
	mux := http.NewServeMux()
	srv.registerAPIRoutes(mux)
	for _, registered := range []bool{false, true, false} {
		srv.withConfig(func(cfg *Config) {
			cfg.DownloadRoutes = nil
			if registered {
				cfg.DownloadRoutes = map[string]string{"llm/gguf": root}
			}
		})
		quants := []string{"Q4_K_M", "Q8_0"}
		mmproj := []string{filepath.FromSlash("routed/child/model/mmproj-F16.gguf")}
		count := 1
		if registered {
			quants, mmproj, count = []string{"Q4_K_M"}, nil, 2
		}
		var list struct {
			Repos []CachedRepoInfo `json:"repos"`
		}
		getCacheTestJSON(t, mux, "/api/cache", &list)
		if len(list.Repos) != count {
			t.Errorf("registered=%v: list count = %d, want %d", registered, len(list.Repos), count)
		}
		for _, repo := range list.Repos {
			if repo.Repo == "parent/real" {
				checkCacheTestMetadata(t, repo, quants, mmproj, false)
			}
		}
		var detail CachedRepoInfo
		getCacheTestJSON(t, mux, "/api/cache/parent/real", &detail)
		checkCacheTestMetadata(t, detail, quants, mmproj, true)
		if registered {
			getCacheTestJSON(t, mux, "/api/cache/child/model", &detail)
			checkCacheTestMetadata(t, detail, []string{"Q8_0"}, []string{"mmproj-F16.gguf"}, true)
		} else {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/cache/child/model", nil))
			if w.Code != http.StatusNotFound {
				t.Errorf("unregistered child lookup = %d, want 404", w.Code)
			}
		}
	}
}

func TestAPI_GetSettings(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/api/settings", nil)
	w := httptest.NewRecorder()

	srv.handleGetSettings(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	var resp SettingsResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.CacheDir != srv.config.CacheDir {
		t.Errorf("Expected cacheDir %s, got %s", srv.config.CacheDir, resp.CacheDir)
	}
}

func TestAPI_DiskFreeDefaultsToLocalDir(t *testing.T) {
	root := t.TempDir()
	localDir := filepath.Join(root, "local")
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}

	srv := newTestServerWithConfig(t, Config{CacheDir: cacheDir, LocalDir: localDir})
	req := httptest.NewRequest("GET", "/api/diskfree", nil)
	w := httptest.NewRecorder()

	srv.handleDiskFree(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if got := resp["path"]; got != localDir {
		t.Fatalf("path = %v, want localDir %s", got, localDir)
	}
}

func TestAPI_GetSettings_TokenMasked(t *testing.T) {
	cfg := Config{
		CacheDir: "/tmp/test_cache",
		Token:    "hf_abcdefghijklmnop",
	}
	srv := newTestServerWithConfig(t, cfg)

	req := httptest.NewRequest("GET", "/api/settings", nil)
	w := httptest.NewRecorder()

	srv.handleGetSettings(w, req)

	var resp SettingsResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	// Token should be masked, not exposed
	if resp.Token == "hf_abcdefghijklmnop" {
		t.Error("Token should be masked, not exposed in full")
	}
	if resp.Token != "********mnop" {
		t.Errorf("Expected masked token ********mnop, got %s", resp.Token)
	}
}

func TestAPI_UpdateSettings(t *testing.T) {
	srv := newTestServer(t)

	// Update concurrency
	body := `{"connections": 16, "maxActive": 8, "retries": 0, "verify": "sha256"}`
	req := httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleUpdateSettings(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	// Verify changes applied
	if srv.config.Concurrency != 16 {
		t.Errorf("Expected concurrency 16, got %d", srv.config.Concurrency)
	}
	if srv.config.MaxActive != 8 {
		t.Errorf("Expected maxActive 8, got %d", srv.config.MaxActive)
	}
	if srv.config.Retries != 0 {
		t.Errorf("Expected retries 0, got %d", srv.config.Retries)
	}
	if srv.config.Verify != "sha256" {
		t.Errorf("Expected verify sha256, got %s", srv.config.Verify)
	}
}

func TestAPI_UpdateSettings_UpdatesCacheDir(t *testing.T) {
	srv := newTestServer(t)

	// Storage settings are editable via the API (see 2e75528); the value is
	// trimmed before being applied.
	body := `{"cacheDir": "  /data/hf-cache  "}`
	req := httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleUpdateSettings(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}
	if srv.config.CacheDir != "/data/hf-cache" {
		t.Errorf("Expected trimmed cacheDir to be applied, got %q", srv.config.CacheDir)
	}

	// Omitting the field leaves the configured path untouched.
	req = httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(`{"connections": 4}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()

	srv.handleUpdateSettings(w, req)

	if srv.config.CacheDir != "/data/hf-cache" {
		t.Errorf("CacheDir changed by unrelated update: %q", srv.config.CacheDir)
	}
}

func TestAPI_UpdateSettings_ValidatesMaxSpeed(t *testing.T) {
	// Speed cap is a trust-boundary input: invalid values must surface as
	// 400 and must not be persisted into srv.config (otherwise a follow-up
	// download would silently run uncapped).
	tests := []struct {
		name      string
		body      string
		wantCode  int
		wantSpeed string // expected srv.config.MaxSpeed after the request
	}{
		{"valid", `{"maxSpeed":"2MB"}`, http.StatusOK, "2MB"},
		{"empty means unlimited", `{"maxSpeed":""}`, http.StatusOK, ""},
		{"zero means unlimited", `{"maxSpeed":"0"}`, http.StatusOK, "0"},
		{"trimmed and stored", `{"maxSpeed":"  4MB  "}`, http.StatusOK, "4MB"},
		{"invalid word", `{"maxSpeed":"abc"}`, http.StatusBadRequest, ""},
		{"invalid number+unit", `{"maxSpeed":"5xyz"}`, http.StatusBadRequest, ""},
		{"unit only", `{"maxSpeed":"KB"}`, http.StatusBadRequest, ""},
		{"negative", `{"maxSpeed":"-1MB"}`, http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t)
			req := httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.handleUpdateSettings(w, req)
			if w.Code != tt.wantCode {
				t.Errorf("%s: status = %d, want %d. body=%s", tt.name, w.Code, tt.wantCode, w.Body.String())
			}
			if srv.config.MaxSpeed != tt.wantSpeed {
				t.Errorf("%s: srv.config.MaxSpeed = %q, want %q", tt.name, srv.config.MaxSpeed, tt.wantSpeed)
			}
		})
	}
}

func TestAPI_UpdateSettings_ValidatesMultipartThreshold(t *testing.T) {
	// MultipartThreshold shares the same silent-fallthrough pattern as
	// MaxSpeed did before: an invalid size would only fail at the next
	// download attempt, far from the source. Validation must surface as
	// 400 and the field must not be persisted.
	tests := []struct {
		name      string
		body      string
		wantCode  int
		wantStore string // expected srv.config.MultipartThreshold after the request
	}{
		{"valid", `{"multipartThreshold":"16MiB"}`, http.StatusOK, "16MiB"},
		{"empty is skipped (default applies)", `{"multipartThreshold":""}`, http.StatusOK, ""},
		{"trimmed and stored", `{"multipartThreshold":"  32MiB  "}`, http.StatusOK, "32MiB"},
		{"invalid word", `{"multipartThreshold":"xyz"}`, http.StatusBadRequest, ""},
		{"invalid number+unit", `{"multipartThreshold":"5xyz"}`, http.StatusBadRequest, ""},
		{"unit only", `{"multipartThreshold":"MB"}`, http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t)
			req := httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.handleUpdateSettings(w, req)
			if w.Code != tt.wantCode {
				t.Errorf("%s: status = %d, want %d. body=%s", tt.name, w.Code, tt.wantCode, w.Body.String())
			}
			if srv.config.MultipartThreshold != tt.wantStore {
				t.Errorf("%s: srv.config.MultipartThreshold = %q, want %q", tt.name, srv.config.MultipartThreshold, tt.wantStore)
			}
		})
	}
}

// TestAPI_UpdateSettings_AtomicValidation guards the trust-boundary
// invariant: a request that combines valid fields with an invalid
// size-string field must NOT partially apply the valid fields. Otherwise
// s.config diverges from the persisted file (in-memory updated, file
// unchanged) and a subsequent GET /api/settings returns a value the server
// can't survive a restart with.
func TestAPI_UpdateSettings_AtomicValidation(t *testing.T) {
	t.Run("invalid maxSpeed does not apply concurrency", func(t *testing.T) {
		srv := newTestServer(t)
		origConcurrency := srv.config.Concurrency
		origCacheDir := srv.config.CacheDir

		body := `{"connections": 16, "cacheDir": "/data/hf", "maxSpeed": "abc"}`
		req := httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.handleUpdateSettings(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d. body=%s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		if srv.config.Concurrency != origConcurrency {
			t.Errorf("Concurrency = %d, want %d (must not be partially applied)", srv.config.Concurrency, origConcurrency)
		}
		if srv.config.CacheDir != origCacheDir {
			t.Errorf("CacheDir = %q, want %q (must not be partially applied)", srv.config.CacheDir, origCacheDir)
		}
	})

	t.Run("invalid multipartThreshold does not apply maxActive", func(t *testing.T) {
		srv := newTestServer(t)
		origMaxActive := srv.config.MaxActive

		body := `{"maxActive": 8, "multipartThreshold": "xyz"}`
		req := httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.handleUpdateSettings(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d. body=%s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		if srv.config.MaxActive != origMaxActive {
			t.Errorf("MaxActive = %d, want %d (must not be partially applied)", srv.config.MaxActive, origMaxActive)
		}
	})
}

func TestAPI_StartDownload_ValidatesRepo(t *testing.T) {
	srv := newTestServer(t)

	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{
			name:     "missing repo",
			body:     `{}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "invalid repo format",
			body:     `{"repo": "invalid"}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "valid repo",
			body:     `{"repo": "owner/name"}`,
			wantCode: http.StatusAccepted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/download", bytes.NewBufferString(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			srv.handleStartDownload(w, req)

			if w.Code != tt.wantCode {
				t.Errorf("Expected %d, got %d. Body: %s", tt.wantCode, w.Code, w.Body.String())
			}
		})
	}
}

func TestAPI_StartDownload_OutputIgnored(t *testing.T) {
	srv := newTestServer(t)

	// Try to specify custom output path
	body := `{"repo": "test/model", "output": "/etc/evil"}`
	req := httptest.NewRequest("POST", "/api/download", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleStartDownload(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("Expected 202, got %d", w.Code)
	}

	var resp Job
	json.Unmarshal(w.Body.Bytes(), &resp)

	// Output should be server-controlled (HF cache), not from request
	if resp.OutputDir == "/etc/evil" {
		t.Error("Output path from request should be ignored!")
	}
	if resp.OutputDir != srv.config.CacheDir {
		t.Errorf("Expected server-controlled HF cache output, got %s", resp.OutputDir)
	}
}

func TestAPI_StartDownload_DatasetUsesSameCacheDir(t *testing.T) {
	srv := newTestServer(t)

	body := `{"repo": "test/dataset", "dataset": true}`
	req := httptest.NewRequest("POST", "/api/download", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleStartDownload(w, req)

	var resp Job
	json.Unmarshal(w.Body.Bytes(), &resp)

	// In v3, both models and datasets use the same HF cache directory
	if resp.OutputDir != srv.config.CacheDir {
		t.Errorf("Dataset should use HF cache dir, got %s", resp.OutputDir)
	}
}

func TestAPI_StartDownload_DuplicateReturnsExisting(t *testing.T) {
	srv := newTestServer(t)

	body := `{"repo": "dup/test"}`

	// First request
	req1 := httptest.NewRequest("POST", "/api/download", bytes.NewBufferString(body))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	srv.handleStartDownload(w1, req1)

	if w1.Code != http.StatusAccepted {
		t.Fatalf("First request should return 202, got %d", w1.Code)
	}

	var job1 Job
	json.Unmarshal(w1.Body.Bytes(), &job1)

	// Second request (duplicate)
	req2 := httptest.NewRequest("POST", "/api/download", bytes.NewBufferString(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.handleStartDownload(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("Duplicate request should return 200, got %d", w2.Code)
	}

	var resp map[string]any
	json.Unmarshal(w2.Body.Bytes(), &resp)

	if resp["message"] != "Download already in progress" {
		t.Errorf("Expected duplicate message, got %v", resp["message"])
	}

	jobMap := resp["job"].(map[string]any)
	if jobMap["id"] != job1.ID {
		t.Error("Duplicate should return same job ID")
	}
}

func TestAPI_ListJobs(t *testing.T) {
	srv := newTestServer(t)

	// Create a job first
	body := `{"repo": "list/test"}`
	req := httptest.NewRequest("POST", "/api/download", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleStartDownload(w, req)

	// List jobs
	listReq := httptest.NewRequest("GET", "/api/jobs", nil)
	listW := httptest.NewRecorder()
	srv.handleListJobs(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", listW.Code)
	}

	var resp map[string]any
	json.Unmarshal(listW.Body.Bytes(), &resp)

	count := int(resp["count"].(float64))
	if count < 1 {
		t.Error("Expected at least 1 job")
	}
}

func TestAPI_ParseFiltersFromRepo(t *testing.T) {
	srv := newTestServer(t)

	body := `{"repo": "owner/model:q4_0,q5_0"}`
	req := httptest.NewRequest("POST", "/api/download", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleStartDownload(w, req)

	var resp Job
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Repo != "owner/model" {
		t.Errorf("Repo should be parsed without filters, got %s", resp.Repo)
	}
	if len(resp.Filters) != 2 {
		t.Errorf("Expected 2 filters, got %d", len(resp.Filters))
	}
}

// --- Delete Cache Security Tests ---

func TestAPI_CacheDelete_PathTraversal(t *testing.T) {
	srv := newTestServer(t)

	// Test various path traversal attempts
	tests := []struct {
		name     string
		repo     string
		wantCode int
	}{
		{
			name:     "direct path traversal",
			repo:     "../../../etc/passwd",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "double dot in owner",
			repo:     "../passwd/file",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "double slash",
			repo:     "owner//name",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "backslash traversal",
			repo:     "owner\\..\\etc",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "just dots owner",
			repo:     "../name",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "just dots name",
			repo:     "owner/..",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "single dot owner",
			repo:     "./name",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "single dot name",
			repo:     "owner/.",
			wantCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("DELETE", "/api/cache/"+tt.repo, nil)
			req.SetPathValue("repo", tt.repo)
			w := httptest.NewRecorder()

			srv.handleCacheDelete(w, req)

			if w.Code != tt.wantCode {
				t.Errorf("Expected %d for %q, got %d. Body: %s",
					tt.wantCode, tt.repo, w.Code, w.Body.String())
			}
		})
	}
}

func TestAPI_CacheDelete_InvalidCharacters(t *testing.T) {
	srv := newTestServer(t)

	// Test invalid characters that could be used in attacks
	// Note: Some characters (null byte, control chars) are rejected by the HTTP layer itself
	// and cannot reach our handler, so we only test what can actually arrive.
	tests := []struct {
		name     string
		repo     string
		wantCode int
	}{
		{
			name:     "shell metacharacter semicolon",
			repo:     "owner/name;rm",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "shell metacharacter pipe",
			repo:     "owner/name|cat",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "shell metacharacter backtick",
			repo:     "owner/`whoami`",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "dollar sign",
			repo:     "owner/$HOME",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "colon",
			repo:     "owner/name:evil",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "asterisk",
			repo:     "owner/name*",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "ampersand",
			repo:     "owner/name&cmd",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "space",
			repo:     "owner/name evil",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "at sign",
			repo:     "owner/@evil",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "hash",
			repo:     "owner/#evil",
			wantCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("DELETE", "/api/cache/test/repo", nil)
			req.SetPathValue("repo", tt.repo) // Set path value directly to bypass URL parsing
			w := httptest.NewRecorder()

			srv.handleCacheDelete(w, req)

			if w.Code != tt.wantCode {
				t.Errorf("Expected %d for %q, got %d. Body: %s",
					tt.wantCode, tt.repo, w.Code, w.Body.String())
			}
		})
	}
}

func TestAPI_CacheDelete_ValidRepoFormat(t *testing.T) {
	// Use a real temp directory (avoids /tmp -> /private/tmp symlink issues on macOS)
	tempDir := t.TempDir()
	cfg := Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    tempDir,
		Concurrency: 2,
		MaxActive:   1,
	}
	srv := newTestServerWithConfig(t, cfg)

	// Valid format repos should pass validation (may return 404 if not found)
	tests := []struct {
		name     string
		repo     string
		wantCode int // 404 is OK - it means validation passed
	}{
		{
			name:     "simple valid repo",
			repo:     "owner/name",
			wantCode: http.StatusNotFound, // Passes validation, not found in cache
		},
		{
			name:     "repo with dash",
			repo:     "the-owner/model-name",
			wantCode: http.StatusNotFound,
		},
		{
			name:     "repo with underscore",
			repo:     "my_owner/my_model",
			wantCode: http.StatusNotFound,
		},
		{
			name:     "repo with numbers",
			repo:     "owner123/model456",
			wantCode: http.StatusNotFound,
		},
		{
			name:     "repo with period",
			repo:     "owner.org/model.v1",
			wantCode: http.StatusNotFound,
		},
		{
			name:     "mixed case",
			repo:     "TheBloke/Mistral-7B-Instruct-v0.2-GGUF",
			wantCode: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("DELETE", "/api/cache/"+tt.repo, nil)
			req.SetPathValue("repo", tt.repo)
			w := httptest.NewRecorder()

			srv.handleCacheDelete(w, req)

			if w.Code != tt.wantCode {
				t.Errorf("Expected %d for %q, got %d. Body: %s",
					tt.wantCode, tt.repo, w.Code, w.Body.String())
			}
		})
	}
}

func TestIsValidRepoComponent(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		// Valid
		{"owner", true},
		{"my-org", true},
		{"my_org", true},
		{"MyOrg123", true},
		{"model.v1", true},
		{"a", true},
		{"1", true},
		{"a-b_c.d", true},

		// Invalid - special components
		{"", false},
		{".", false},
		{"..", false},

		// Invalid - dangerous characters
		{"/", false},
		{"\\", false},
		{";", false},
		{"|", false},
		{"$", false},
		{"`", false},
		{"'", false},
		{"\"", false},
		{" ", false},
		{"\n", false},
		{"\t", false},
		{"\x00", false},
		{"*", false},
		{"?", false},
		{"<", false},
		{">", false},
		{":", false},
		{"&", false},
		{"!", false},
		{"(", false},
		{")", false},
		{"[", false},
		{"]", false},
		{"{", false},
		{"}", false},
		{"@", false},
		{"#", false},
		{"%", false},
		{"^", false},
		{"=", false},
		{"+", false},
		{"~", false},

		// Invalid - mixed valid/invalid
		{"owner;evil", false},
		{"owner|evil", false},
		{"name$var", false},
		{"../passwd", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isValidRepoComponent(tt.input)
			if got != tt.want {
				t.Errorf("isValidRepoComponent(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestAPI_ConcurrentSettingsAccess stresses the new s.configMu by racing
// concurrent UpdateSettings writers against handleGetSettings, handleCacheList
// and other read paths. Run with `go test -race` to catch any lock the
// refactor missed: the test is meaningless without the race detector.
func TestAPI_ConcurrentSettingsAccess(t *testing.T) {
	srv := newTestServer(t)

	const writers = 4
	const readers = 8
	const iterations = 50

	var wg sync.WaitGroup

	// Writers: cycle through POST /api/settings with varying fields.
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				body := fmt.Sprintf(`{"connections": %d, "maxActive": %d, "maxSpeed": "%dKB"}`,
					(id+1)*4, (id+1)*2, 256+(id*64)+(j%4)*32)
				req := httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				srv.handleUpdateSettings(w, req)
				if w.Code != http.StatusOK {
					t.Errorf("writer %d iter %d: status = %d", id, j, w.Code)
					return
				}
			}
		}(i)
	}

	// Readers: hit every read handler that touches s.config.
	readHandlers := []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, r *http.Request) { srv.handleGetSettings(w, r) },
		func(w http.ResponseWriter, r *http.Request) { srv.handleCacheList(w, r) },
		func(w http.ResponseWriter, r *http.Request) { srv.handleDiskFree(w, r) },
		func(w http.ResponseWriter, r *http.Request) {
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
			srv.corsMiddleware(inner).ServeHTTP(w, r)
		},
		func(w http.ResponseWriter, r *http.Request) {
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
			srv.basicAuthMiddleware(inner).ServeHTTP(w, r)
		},
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			h := readHandlers[id%len(readHandlers)]
			for j := 0; j < iterations; j++ {
				w := httptest.NewRecorder()
				h(w, httptest.NewRequest("GET", "/", nil))
			}
		}(i)
	}

	wg.Wait()
}

// TestAPI_UpdateSettings_DropsStalePostLockSideEffects guards the
// generation-counter fix: when two handleUpdateSettings calls commit
// back-to-back, the loser's post-lock side effects (UpdateConfig and
// SaveConfigFile) must be dropped so the job manager and the persisted
// file are not rolled back to the loser's older snapshot.
func TestAPI_UpdateSettings_DropsStalePostLockSideEffects(t *testing.T) {
	srv := newTestServer(t)

	srv.config.MaxSpeed = "0"

	var wg sync.WaitGroup
	const writers = 4
	results := make([]int, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"maxSpeed": "%dMB"}`, (id+1)*10)
			req := httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.handleUpdateSettings(w, req)
			results[id] = w.Code
		}(i)
	}
	wg.Wait()

	wantPattern := regexp.MustCompile(`^(10|20|30|40)MB$`)
	cfg := srv.snapshotConfig()
	if !wantPattern.MatchString(cfg.MaxSpeed) {
		t.Errorf("srv.config.MaxSpeed = %q; want one of the writers' values", cfg.MaxSpeed)
	}
	// Job manager must converge on the same committed value — it must
	// NOT have been rolled back by a stale post-lock side effect.
	if srv.jobs.snapshotConfig().MaxSpeed != cfg.MaxSpeed {
		t.Errorf("srv.jobs.config.MaxSpeed = %q; want committed %q", srv.jobs.snapshotConfig().MaxSpeed, cfg.MaxSpeed)
	}
	for i, code := range results {
		if code != http.StatusOK {
			t.Errorf("writer %d: status = %d, want 200", i, code)
		}
	}
}

// TestAPI_UpdateSettings_PersistedFileMatchesLatest guards the persistMu fix:
// concurrent POST /api/settings writers must leave the persisted config file
// in a state consistent with the authoritative in-memory config. Before the
// fix, two concurrent writes could interleave so that the older writer's
// SaveConfigFile executed after the newer writer's, rolling the file back.
func TestAPI_UpdateSettings_PersistedFileMatchesLatest(t *testing.T) {
	// Keep this test isolated from the caller's real config file.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfgPath := ConfigPath()
	_ = os.Remove(cfgPath)

	srv := newTestServer(t)
	const writers = 4
	var wg sync.WaitGroup

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"maxSpeed": "%dMB"}`, (id+1)*10)
			req := httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.handleUpdateSettings(w, req)
		}(i)
	}
	wg.Wait()

	// Read the config file through the same path the server uses.
	fileCfg, err := LoadConfigFile()
	if err != nil {
		t.Fatalf("failed to load config file: %v", err)
	}

	// The file's MaxSpeed must match the authoritative in-memory config.
	if fileCfg.MaxSpeed != srv.config.MaxSpeed {
		t.Errorf("config file MaxSpeed = %q, want %q (in-memory)", fileCfg.MaxSpeed, srv.config.MaxSpeed)
	}

	// Both must be one of the writers' values.
	wantPattern := regexp.MustCompile(`^(10|20|30|40)MB$`)
	if !wantPattern.MatchString(srv.config.MaxSpeed) {
		t.Errorf("srv.config.MaxSpeed = %q; want one of the writers' values", srv.config.MaxSpeed)
	}
	if !wantPattern.MatchString(fileCfg.MaxSpeed) {
		t.Errorf("config file MaxSpeed = %q; want one of the writers' values", fileCfg.MaxSpeed)
	}
}
