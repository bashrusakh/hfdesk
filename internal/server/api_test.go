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
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

var testCacheDir string

func newTestServer() *Server {
	if testCacheDir == "" {
		testCacheDir = "/tmp/hfdesk_test_cache"
	}
	cfg := Config{
		Addr:        "127.0.0.1",
		Port:        0, // Random port
		CacheDir:    testCacheDir,
		Concurrency: 2,
		MaxActive:   1,
	}
	return New(cfg)
}

func TestAPI_Health(t *testing.T) {
	srv := newTestServer()

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

	srv := New(Config{
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
	srv := newTestServer()

	req := httptest.NewRequest("GET", "/api/settings", nil)
	w := httptest.NewRecorder()

	srv.handleGetSettings(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	var resp SettingsResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.CacheDir != testCacheDir {
		t.Errorf("Expected cacheDir %s, got %s", testCacheDir, resp.CacheDir)
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

	srv := New(Config{CacheDir: cacheDir, LocalDir: localDir})
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
	srv := New(cfg)

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
	srv := newTestServer()

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
	srv := newTestServer()

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
			srv := newTestServer()
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
			srv := newTestServer()
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
		srv := newTestServer()
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
		srv := newTestServer()
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
	srv := newTestServer()

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
	srv := newTestServer()

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
	if resp.OutputDir != testCacheDir {
		t.Errorf("Expected server-controlled HF cache output, got %s", resp.OutputDir)
	}
}

func TestAPI_StartDownload_DatasetUsesSameCacheDir(t *testing.T) {
	srv := newTestServer()

	body := `{"repo": "test/dataset", "dataset": true}`
	req := httptest.NewRequest("POST", "/api/download", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleStartDownload(w, req)

	var resp Job
	json.Unmarshal(w.Body.Bytes(), &resp)

	// In v3, both models and datasets use the same HF cache directory
	if resp.OutputDir != testCacheDir {
		t.Errorf("Dataset should use HF cache dir, got %s", resp.OutputDir)
	}
}

func TestAPI_StartDownload_DuplicateReturnsExisting(t *testing.T) {
	srv := newTestServer()

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
	srv := newTestServer()

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
	srv := newTestServer()

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
	srv := newTestServer()

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
	srv := newTestServer()

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
	srv := New(cfg)

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

func TestAPI_CacheDelete_LocalDir(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")
	writeRepoFixture(t, filepath.Join(localDir, "owner", "name"), "model.gguf")
	writeRepoFixture(t, filepath.Join(localDir, "owner", "sibling"), "sibling.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?type=model&source=Local", nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(localDir, "owner", "name")); !os.IsNotExist(err) {
		t.Errorf("Expected repo folder to be deleted, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(localDir, "owner", "sibling")); err != nil {
		t.Errorf("Expected sibling folder to survive, stat err = %v", err)
	}
}

func TestAPI_CacheDelete_LocalScanDirs(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	scanDir := filepath.Join(root, "scan")
	writeRepoFixture(t, filepath.Join(scanDir, "owner", "name"), "model.safetensors")
	writeRepoFixture(t, filepath.Join(scanDir, "owner", "sibling"), "sibling.safetensors")

	srv := New(Config{
		Addr:          "127.0.0.1",
		Port:          0,
		CacheDir:      cacheDir,
		LocalScanDirs: []string{scanDir},
		Concurrency:   2,
		MaxActive:     1,
	})

	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?type=model&source=Local", nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(scanDir, "owner", "name")); !os.IsNotExist(err) {
		t.Errorf("Expected repo folder to be deleted, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(scanDir, "owner", "sibling")); err != nil {
		t.Errorf("Expected sibling folder to survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_LocalSymlinkedOwner verifies that when the intermediate
// owner directory under a local root is a symlink pointing at a sibling owner
// directory, the delete is rejected and the real sibling target is left intact.
// Without component-level symlink validation, os.RemoveAll would follow the
// symlinked owner and destroy the sibling repo.
func TestAPI_CacheDelete_LocalSymlinkedOwner(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")

	// Real repo that must survive, reachable as localDir/other/name.
	writeRepoFixture(t, filepath.Join(localDir, "other", "name"), "victim.gguf")

	// localDir/owner -> localDir/other (symlinked owner component).
	if err := os.Symlink(filepath.Join(localDir, "other"), filepath.Join(localDir, "owner")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?type=model&source=Local", nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("Expected delete to fail through symlinked owner, got 200. Body: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(localDir, "other", "name", "victim.gguf")); err != nil {
		t.Errorf("Expected real sibling target to survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_LocalTargetSymlink verifies that a repo directory which is
// itself a symlink is rejected rather than followed and deleted.
func TestAPI_CacheDelete_LocalTargetSymlink(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")

	// Real repo that the symlinked target points at; it must survive.
	writeRepoFixture(t, filepath.Join(localDir, "real", "name"), "model.gguf")
	if err := os.MkdirAll(filepath.Join(localDir, "owner"), 0o755); err != nil {
		t.Fatalf("mkdir owner: %v", err)
	}
	// localDir/owner/name -> localDir/real/name (symlinked leaf).
	if err := os.Symlink(filepath.Join(localDir, "real", "name"), filepath.Join(localDir, "owner", "name")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?type=model&source=Local", nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("Expected symlinked target to be rejected, got 200. Body: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(localDir, "real", "name", "model.gguf")); err != nil {
		t.Errorf("Expected symlink destination to survive, stat err = %v", err)
	}
}

// writeFriendlyProjection creates a genuine friendly-view projection: a real
// <cache>/models/<owner>/<name> directory holding a relative symlink whose
// lexical target is inside the repo's hub directory. The target may be dangling
// (an orphan friendly view). Tests that need a deletable friendly copy must use
// this instead of regular files, because a folder of real files is not a proven
// projection and is deliberately preserved by the delete path.
func writeFriendlyProjection(t *testing.T, cacheDir, owner, name, linkName string) string {
	t.Helper()
	dir := filepath.Join(cacheDir, "models", owner, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir friendly projection: %v", err)
	}
	target := filepath.Join("..", "..", "..", "hub", "models--"+owner+"--"+name, "blobs", "sha256")
	if err := os.Symlink(target, filepath.Join(dir, linkName)); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	return dir
}

// TestAPI_CacheDelete_FriendlyViewRoundTrip verifies that deleting with
// source=Friendly view removes both the friendly <cache>/models/<owner>/<name>
// projection and the HF hub directory in one request, returning 200.
func TestAPI_CacheDelete_FriendlyViewRoundTrip(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")

	friendlyRepo := writeFriendlyProjection(t, cacheDir, "owner", "name", "model.gguf")
	hubRepo := filepath.Join(cacheDir, "hub", "models--owner--name")
	writeRepoFixture(t, hubRepo, "model.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?type=model&source=Friendly+view", nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(friendlyRepo); !os.IsNotExist(err) {
		t.Errorf("Expected friendly dir to be deleted, stat err = %v", err)
	}
	if _, err := os.Stat(hubRepo); !os.IsNotExist(err) {
		t.Errorf("Expected hub dir to be deleted, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_LocalRootPrefixCollision verifies that deleting a repo
// under a root like /tmp/.../Models never touches the sibling
// /tmp/.../Models-evil even though one root path is a string prefix of the
// other.
func TestAPI_CacheDelete_LocalRootPrefixCollision(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "Models")
	evilDir := filepath.Join(root, "Models-evil")
	writeRepoFixture(t, filepath.Join(localDir, "owner", "name"), "model.gguf")
	writeRepoFixture(t, filepath.Join(evilDir, "owner", "name"), "model.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?type=model&source=Local", nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(localDir, "owner", "name")); !os.IsNotExist(err) {
		t.Errorf("Expected repo folder under root to be deleted, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(evilDir, "owner", "name")); err != nil {
		t.Errorf("Expected sibling root Models-evil to be untouched, stat err = %v", err)
	}

	// A repo that exists ONLY under the sibling (unconfigured) root must not be
	// deleted as a fall-through: the handler must report 404 and leave it intact.
	onlyEvil := filepath.Join(evilDir, "owner", "only-evil")
	writeRepoFixture(t, onlyEvil, "model.gguf")
	req = httptest.NewRequest("DELETE", "/api/cache/owner/only-evil?type=model&source=Local", nil)
	req.SetPathValue("repo", "owner/only-evil")
	w = httptest.NewRecorder()
	srv.handleCacheDelete(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("Expected 404 for repo only in sibling root, got %d. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(onlyEvil); err != nil {
		t.Errorf("Expected sibling-only repo to be untouched, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_LocalTraversalRejected verifies path traversal is still
// rejected with 400 before the local-source branch is reached.
func TestAPI_CacheDelete_LocalTraversalRejected(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")
	writeRepoFixture(t, filepath.Join(localDir, "owner", "name"), "model.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	for _, repo := range []string{"../name", "owner/..", "owner//name", "..\\name"} {
		req := httptest.NewRequest("DELETE", "/api/cache/x?type=model&source=Local", nil)
		req.SetPathValue("repo", repo)
		w := httptest.NewRecorder()
		srv.handleCacheDelete(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("Expected 400 for %q, got %d. Body: %s", repo, w.Code, w.Body.String())
		}
	}
}

// TestAPI_CacheDelete_HFCacheNotFoundUnchanged verifies the explicit HF-cache
// path still returns 404 when neither the hub nor the friendly path exists.
func TestAPI_CacheDelete_HFCacheNotFoundUnchanged(t *testing.T) {
	root := t.TempDir()
	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    filepath.Join(root, "cache"),
		LocalDir:    filepath.Join(root, "local"),
		Concurrency: 2,
		MaxActive:   1,
	})

	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?type=model&source=HF+cache", nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("Expected 404, got %d. Body: %s", w.Code, w.Body.String())
	}
}

// TestAPI_CacheDelete_OrphanFriendlyView verifies a genuine friendly-view
// projection whose hub directory is gone is deleted instead of reported as not
// found.
func TestAPI_CacheDelete_OrphanFriendlyView(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	friendlyRepo := writeFriendlyProjection(t, cacheDir, "owner", "name", "model.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?type=model&source=HF+cache", nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(friendlyRepo); !os.IsNotExist(err) {
		t.Errorf("Expected orphan friendly path to be deleted, stat err = %v", err)
	}
}

// writeRepoFixture creates dir plus a weight file so hasLocalWeightFile treats
// it as a real repo.
func writeRepoFixture(t *testing.T, dir, filename string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
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
	srv := newTestServer()

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
	srv := newTestServer()

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

	srv := newTestServer()
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

// cacheInfoForTest fetches the details response for repo from the handler.
func cacheInfoForTest(t *testing.T, srv *Server, repo string) CachedRepoInfo {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/cache/"+repo, nil)
	req.SetPathValue("repo", repo)
	w := httptest.NewRecorder()
	srv.handleCacheInfo(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handleCacheInfo(%s) = %d, want 200. Body: %s", repo, w.Code, w.Body.String())
	}
	var info CachedRepoInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode cache info: %v", err)
	}
	return info
}

// cacheCopyBySource returns the copy with the given source, or nil.
func cacheCopyBySource(copies []CacheCopy, source string) *CacheCopy {
	for i := range copies {
		if copies[i].Source == source {
			return &copies[i]
		}
	}
	return nil
}

// countCacheCopiesBySource counts copies with the given source.
func countCacheCopiesBySource(copies []CacheCopy, source string) int {
	n := 0
	for _, c := range copies {
		if c.Source == source {
			n++
		}
	}
	return n
}

// TestAPI_CacheInfo_ListsHFCacheAndLocalCopies verifies that a repo present in
// both the HF cache and a local root reports both deletable copies, with the HF
// cache entry first.
func TestAPI_CacheInfo_ListsHFCacheAndLocalCopies(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")

	hubRepo := filepath.Join(cacheDir, "hub", "models--owner--name")
	writeRepoFixture(t, hubRepo, "blob")
	writeRepoFixture(t, filepath.Join(localDir, "owner", "name"), "model.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	info := cacheInfoForTest(t, srv, "owner/name")
	if len(info.Copies) != 2 {
		t.Fatalf("copies = %#v, want 2 entries", info.Copies)
	}
	if info.Copies[0].Source != cacheSourceHFCache {
		t.Errorf("first copy source = %q, want %q", info.Copies[0].Source, cacheSourceHFCache)
	}
	localCopy := cacheCopyBySource(info.Copies, cacheSourceLocal)
	if localCopy == nil {
		t.Fatalf("no Local copy in %#v", info.Copies)
	}
	wantLocal := filepath.Join(localDir, "owner", "name")
	if localCopy.Path != wantLocal {
		t.Errorf("local copy path = %q, want %q", localCopy.Path, wantLocal)
	}
	// The Friendly view copy shares storage with the hub and must not be listed.
	if cacheCopyBySource(info.Copies, cacheSourceFriendlyView) != nil {
		t.Errorf("did not expect a Friendly view copy when the hub exists: %#v", info.Copies)
	}
	// Top-level fields stay backward compatible (primary copy = HF cache).
	if info.Source != cacheSourceHFCache {
		t.Errorf("top-level source = %q, want %q", info.Source, cacheSourceHFCache)
	}
}

// TestAPI_CacheInfo_AlwaysEmitsCopiesKey is M1: a current server must always
// include the "copies" key in a cache-info response, even when there are zero
// deletable copies. Otherwise the UI's legacy fallback (which keys off the
// absence of the array) renders a Delete button that always 400s. Two zero-copy
// cases are covered: a real folder under the friendly namespace that is not a
// projection, and an HF hub outside the configured cache dir.
func TestAPI_CacheInfo_AlwaysEmitsCopiesKey(t *testing.T) {
	assertCopiesKey := func(t *testing.T, body []byte) {
		t.Helper()
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("decode raw cache info: %v", err)
		}
		value, ok := raw["copies"]
		if !ok {
			t.Fatalf("cache info omitted the copies key: %s", body)
		}
		if string(value) == "null" {
			t.Fatalf("copies key is JSON null, want []: %s", body)
		}
		var decoded []CacheCopy
		if err := json.Unmarshal(value, &decoded); err != nil {
			t.Fatalf("decode copies %s: %v", value, err)
		}
		if len(decoded) != 0 {
			t.Fatalf("copies = %#v, want empty for this fixture", decoded)
		}
	}

	t.Run("friendly-view-only-real-folder", func(t *testing.T) {
		root := t.TempDir()
		cacheDir := filepath.Join(root, "cache")
		// A real folder under <cache>/models with no hub and no friendly
		// projection: handleCacheInfo resolves it but there is nothing
		// deletable, so copies must still be emitted as [].
		writeRepoFixture(t, filepath.Join(cacheDir, "models", "owner", "name"), "model.gguf")

		srv := New(Config{
			Addr: "127.0.0.1", Port: 0, CacheDir: cacheDir,
			Concurrency: 2, MaxActive: 1,
		})
		req := httptest.NewRequest("GET", "/api/cache/owner/name", nil)
		req.SetPathValue("repo", "owner/name")
		w := httptest.NewRecorder()
		srv.handleCacheInfo(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("handleCacheInfo = %d, want 200. Body: %s", w.Code, w.Body.String())
		}
		assertCopiesKey(t, w.Body.Bytes())
	})

	t.Run("hf-hub-outside-configured-cache", func(t *testing.T) {
		root := t.TempDir()
		cacheDir := filepath.Join(root, "cache")
		externalHub := filepath.Join(root, "external-hub")
		// Point HF_HUB_CACHE at a hub outside the configured cache dir so the
		// HF-cache copy is not deletable and the response has zero copies.
		t.Setenv("HF_HUB_CACHE", externalHub)
		writeRepoFixture(t, filepath.Join(externalHub, "models--owner--name"), "blob")

		srv := New(Config{
			Addr: "127.0.0.1", Port: 0, CacheDir: cacheDir,
			Concurrency: 2, MaxActive: 1,
		})
		req := httptest.NewRequest("GET", "/api/cache/owner/name", nil)
		req.SetPathValue("repo", "owner/name")
		w := httptest.NewRecorder()
		srv.handleCacheInfo(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("handleCacheInfo = %d, want 200. Body: %s", w.Code, w.Body.String())
		}
		assertCopiesKey(t, w.Body.Bytes())
	})
}

// TestAPI_CacheDelete_ByPathTargetsOneCopy verifies that delete with an exact
// path removes only the targeted local copy and leaves the HF-cache copy.
func TestAPI_CacheDelete_ByPathTargetsOneCopy(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")

	hubRepo := filepath.Join(cacheDir, "hub", "models--owner--name")
	writeRepoFixture(t, hubRepo, "blob")
	localRepo := filepath.Join(localDir, "owner", "name")
	writeRepoFixture(t, localRepo, "model.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	info := cacheInfoForTest(t, srv, "owner/name")
	localCopy := cacheCopyBySource(info.Copies, cacheSourceLocal)
	if localCopy == nil {
		t.Fatalf("no Local copy in %#v", info.Copies)
	}

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	q.Set("path", localCopy.Path)
	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?"+q.Encode(), nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("delete by path = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(localRepo); !os.IsNotExist(err) {
		t.Errorf("expected local copy deleted, stat err = %v", err)
	}
	if _, err := os.Stat(hubRepo); err != nil {
		t.Errorf("expected HF-cache copy to survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_ByPathRejectsUnknownPath verifies that a path that is not
// a server-computed copy returns 400 and deletes nothing, even when it names a
// real directory.
func TestAPI_CacheDelete_ByPathRejectsUnknownPath(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")

	hubRepo := filepath.Join(cacheDir, "hub", "models--owner--name")
	writeRepoFixture(t, hubRepo, "blob")
	localRepo := filepath.Join(localDir, "owner", "name")
	writeRepoFixture(t, localRepo, "model.gguf")
	// A real sibling that must never be deleted through a forged path.
	evilRepo := filepath.Join(localDir, "owner", "evil")
	writeRepoFixture(t, evilRepo, "evil.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	q.Set("path", evilRepo)
	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?"+q.Encode(), nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("delete with unknown path = %d, want 400. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(evilRepo); err != nil {
		t.Errorf("expected forged path to survive, stat err = %v", err)
	}
	if _, err := os.Stat(localRepo); err != nil {
		t.Errorf("expected real local copy to survive, stat err = %v", err)
	}
	if _, err := os.Stat(hubRepo); err != nil {
		t.Errorf("expected HF-cache copy to survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_ByPathTwoLocalRoots verifies that two local roots holding
// the same repo produce two Local copies, and deleting one by path leaves the
// other.
func TestAPI_CacheDelete_ByPathTwoLocalRoots(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")
	scanDir := filepath.Join(root, "scan")

	repoA := filepath.Join(localDir, "owner", "name")
	repoB := filepath.Join(scanDir, "owner", "name")
	writeRepoFixture(t, repoA, "model.gguf")
	writeRepoFixture(t, repoB, "model.safetensors")

	srv := New(Config{
		Addr:          "127.0.0.1",
		Port:          0,
		CacheDir:      cacheDir,
		LocalDir:      localDir,
		LocalScanDirs: []string{scanDir},
		Concurrency:   2,
		MaxActive:     1,
	})

	info := cacheInfoForTest(t, srv, "owner/name")
	if got := countCacheCopiesBySource(info.Copies, cacheSourceLocal); got != 2 {
		t.Fatalf("Local copies = %d, want 2. copies=%#v", got, info.Copies)
	}

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	q.Set("path", repoA)
	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?"+q.Encode(), nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("delete by path = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(repoA); !os.IsNotExist(err) {
		t.Errorf("expected first local copy deleted, stat err = %v", err)
	}
	if _, err := os.Stat(repoB); err != nil {
		t.Errorf("expected second local copy to survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_ByPathHFCacheCopy verifies that delete by path can also
// target the HF-cache copy, removing the hub directory.
func TestAPI_CacheDelete_ByPathHFCacheCopy(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	hubRepo := filepath.Join(cacheDir, "hub", "models--owner--name")
	writeRepoFixture(t, hubRepo, "blob")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	info := cacheInfoForTest(t, srv, "owner/name")
	if len(info.Copies) != 1 || info.Copies[0].Source != cacheSourceHFCache {
		t.Fatalf("copies = %#v, want a single HF cache entry", info.Copies)
	}

	q := url.Values{}
	q.Set("type", "model")
	q.Set("path", info.Copies[0].Path)
	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?"+q.Encode(), nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("delete HF copy by path = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(hubRepo); !os.IsNotExist(err) {
		t.Errorf("expected hub copy deleted, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_DatasetLocalNotFound verifies that a Local delete with
// type=dataset returns 404: local entries are always models.
func TestAPI_CacheDelete_DatasetLocalNotFound(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")
	localRepo := filepath.Join(localDir, "owner", "name")
	writeRepoFixture(t, localRepo, "model.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	q := url.Values{}
	q.Set("type", "dataset")
	q.Set("source", cacheSourceLocal)
	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?"+q.Encode(), nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("dataset local delete = %d, want 404. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(localRepo); err != nil {
		t.Errorf("expected local folder to survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_NoPathLocalScanDirsOnly reproduces the review scenario
// where a repo exists ONLY under a localScanDirs root (no localDir, no hub).
// The details modal lists a Local copy, so the no-path source=Local delete must
// find the same candidate set and return 200 rather than 404.
func TestAPI_CacheDelete_NoPathLocalScanDirsOnly(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	scanDir := filepath.Join(root, "scan")
	repoDir := filepath.Join(scanDir, "owner", "name")
	writeRepoFixture(t, repoDir, "model.gguf")

	srv := New(Config{
		Addr:          "127.0.0.1",
		Port:          0,
		CacheDir:      cacheDir,
		LocalScanDirs: []string{scanDir},
		Concurrency:   2,
		MaxActive:     1,
	})

	info := cacheInfoForTest(t, srv, "owner/name")
	localCopy := cacheCopyBySource(info.Copies, cacheSourceLocal)
	if localCopy == nil {
		t.Fatalf("handleCacheInfo did not list a Local copy: %#v", info.Copies)
	}
	if localCopy.Path != repoDir {
		t.Fatalf("listed Local path = %q, want %q", localCopy.Path, repoDir)
	}

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?"+q.Encode(), nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("no-path Local delete = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(repoDir); !os.IsNotExist(err) {
		t.Errorf("expected scan-dir repo deleted, stat err = %v", err)
	}
}

// TestAPI_CacheInfo_DatasetOmitsLocalCopy verifies that a dataset details
// response does not advertise a Local copy even when a same-named local model
// folder exists: local roots only hold models and deleting one as a dataset
// returns 404, so listing it would show an undeletable copy.
func TestAPI_CacheInfo_DatasetOmitsLocalCopy(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")

	datasetHub := filepath.Join(cacheDir, "hub", "datasets--owner--name")
	writeRepoFixture(t, datasetHub, "data.parquet")
	// Same owner/name exists under a local root as a model.
	writeRepoFixture(t, filepath.Join(localDir, "owner", "name"), "model.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	info := cacheInfoForTest(t, srv, "owner/name")
	if info.Type != "dataset" {
		t.Fatalf("info.Type = %q, want dataset", info.Type)
	}
	hfCopy := cacheCopyBySource(info.Copies, cacheSourceHFCache)
	if hfCopy == nil {
		t.Fatalf("no HF cache copy in %#v", info.Copies)
	}
	if localCopy := cacheCopyBySource(info.Copies, cacheSourceLocal); localCopy != nil {
		t.Errorf("dataset must not list a Local copy, got %#v", localCopy)
	}
	if len(info.Copies) != 1 {
		t.Errorf("copies = %#v, want exactly the HF cache copy", info.Copies)
	}
}

// TestFindLocalCachedRepo_SkipsSpecialOwner verifies that the single-repo
// lookup applies the same SkipSpecial owner exclusion as scanLocalCachedRepos
// and localCopyCandidates: a raw-cache child under an HF-internal owner
// (hub/models/datasets/blobs/snapshots/refs) is not an independent Local repo,
// so the details lookup agrees with the list and the delete routes instead of
// resolving to a repo no delete route accepts.
func TestFindLocalCachedRepo_SkipsSpecialOwner(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	// A raw-cache root child under a special owner that directly owns a weight
	// file. Before the parity fix this resolved as a Local repo, so
	// GET /api/cache/hub/foo returned 200 while every delete route 404'd.
	writeRepoFixture(t, filepath.Join(cacheDir, "hub", "foo"), "model.gguf")
	// A genuine Local repo under a real owner must still resolve.
	realLocal := filepath.Join(cacheDir, "alice", "one")
	writeRepoFixture(t, realLocal, "model.gguf")

	if got, err := findLocalCachedRepo(cacheDir, "", nil, nil, "hub/foo", false); err == nil {
		t.Errorf("findLocalCachedRepo(hub/foo) resolved to %#v, want not found", got)
	}
	got, err := findLocalCachedRepo(cacheDir, "", nil, nil, "alice/one", false)
	if err != nil {
		t.Fatalf("findLocalCachedRepo(alice/one) err = %v", err)
	}
	if filepath.Clean(got.Path) != filepath.Clean(realLocal) {
		t.Errorf("real Local path = %q, want %q", got.Path, realLocal)
	}

	// The details handler must agree with the delete route: no Local copy is
	// advertised (and the handler 404s) for the special-owner child.
	srv := New(Config{
		Addr: "127.0.0.1", Port: 0, CacheDir: cacheDir,
		Concurrency: 2, MaxActive: 1,
	})
	req := httptest.NewRequest("GET", "/api/cache/hub/foo", nil)
	req.SetPathValue("repo", "hub/foo")
	w := httptest.NewRecorder()
	srv.handleCacheInfo(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("handleCacheInfo(hub/foo) = %d, want 404. Body: %s", w.Code, w.Body.String())
	}
}

// TestAPI_CacheInfo_SymlinkedHubNotListed verifies that a hub path which is a
// top-level symlink is not advertised as a deletable HF-cache copy, matching
// the delete path which rejects such a link with 400.
func TestAPI_CacheInfo_SymlinkedHubNotListed(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")

	// Real directory the symlinked hub points at, inside the cache.
	realDir := filepath.Join(cacheDir, "real-target")
	writeRepoFixture(t, realDir, "blob")

	hubPath := filepath.Join(cacheDir, "hub", "models--owner--name")
	if err := os.MkdirAll(filepath.Dir(hubPath), 0o755); err != nil {
		t.Fatalf("mkdir hub: %v", err)
	}
	if err := os.Symlink(realDir, hubPath); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	// handleCacheInfo treats the symlink as present (os.Stat follows it) and
	// returns details; the copies list must not include the symlinked hub.
	info := cacheInfoForTest(t, srv, "owner/name")
	if copy := cacheCopyBySource(info.Copies, cacheSourceHFCache); copy != nil {
		t.Errorf("symlinked hub must not be listed as an HF cache copy: %#v", copy)
	}
	if copy := cacheCopyBySource(info.Copies, cacheSourceFriendlyView); copy != nil {
		t.Errorf("symlinked hub must not expose a friendly view copy: %#v", copy)
	}
	for _, c := range info.Copies {
		if filepath.Clean(c.Path) == filepath.Clean(hubPath) {
			t.Errorf("symlinked hub path %q must not be listed: %#v", hubPath, info.Copies)
		}
	}

	// Delete-by-path on the symlinked hub path must be rejected (not listed),
	// so the link survives.
	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?type=model&path="+url.QueryEscape(hubPath), nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("delete-by-path on symlinked hub = %d, want 400. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(hubPath); err != nil {
		t.Errorf("symlinked hub must survive rejected delete, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_ByPathSymlinkedLocalOwner verifies that deleting a Local
// copy by its exact path through a symlinked owner directory is rejected and
// leaves the real sibling target intact, mirroring the no-path symlink test.
func TestAPI_CacheDelete_ByPathSymlinkedLocalOwner(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")

	// Real repo that must survive, reachable as localDir/other/name.
	writeRepoFixture(t, filepath.Join(localDir, "other", "name"), "victim.gguf")

	// localDir/owner -> localDir/other (symlinked owner component).
	if err := os.Symlink(filepath.Join(localDir, "other"), filepath.Join(localDir, "owner")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	byPath := filepath.Join(localDir, "owner", "name")
	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	q.Set("path", byPath)
	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?"+q.Encode(), nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("expected by-path delete through symlinked owner to fail, got 200. Body: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(localDir, "other", "name", "victim.gguf")); err != nil {
		t.Errorf("expected real sibling target to survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_UnknownSourceRejected verifies that an unrecognized
// non-empty source label (e.g. a typo) is rejected with 400 without deleting
// anything. Before the fix such labels fell through to the legacy HF-cache
// branch, deleting the hub and friendly copies of a different copy than the
// caller named while still reporting success.
func TestAPI_CacheDelete_UnknownSourceRejected(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")
	hubRepo := filepath.Join(cacheDir, "hub", "models--owner--name")
	friendlyRepo := filepath.Join(cacheDir, "models", "owner", "name")
	localRepo := filepath.Join(localDir, "owner", "name")
	writeRepoFixture(t, hubRepo, "model.gguf")
	writeRepoFixture(t, friendlyRepo, "model.gguf")
	writeRepoFixture(t, localRepo, "model.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	// A typo for "local" must not be interpreted as any known copy.
	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?type=model&source=locle", nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown source = %d, want 400. Body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if resp.Error != "Invalid source" {
		t.Errorf("error = %q, want %q", resp.Error, "Invalid source")
	}
	for _, path := range []string{hubRepo, friendlyRepo, localRepo} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s to survive rejected delete, stat err = %v", path, err)
		}
	}
}

// TestAPI_CacheDelete_EmptySourceKeepsLocalFallback verifies that the empty
// source label keeps its legacy behavior after the unknown-label rejection
// was added: HF-cache-first delete, with the local-roots fallback still
// reachable when nothing exists in the HF cache. "hf cache" must hit the
// legacy branch too, but without the local fallback (it returns 404).
func TestAPI_CacheDelete_EmptySourceKeepsLocalFallback(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")
	localRepo := filepath.Join(localDir, "owner", "name")
	// Nothing in the HF cache; only a local copy exists.
	writeRepoFixture(t, localRepo, "model.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	// Empty source: falls back to the local roots and deletes the local copy.
	req := httptest.NewRequest("DELETE", "/api/cache/owner/name?type=model", nil)
	req.SetPathValue("repo", "owner/name")
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("empty source delete = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(localRepo); !os.IsNotExist(err) {
		t.Errorf("expected local copy deleted by empty-source fallback, stat err = %v", err)
	}

	// "hf cache" label: same legacy branch, but no local fallback.
	writeRepoFixture(t, localRepo, "model.gguf")
	req = httptest.NewRequest("DELETE", "/api/cache/owner/name?type=model&source=HF+cache", nil)
	req.SetPathValue("repo", "owner/name")
	w = httptest.NewRecorder()
	srv.handleCacheDelete(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("hf cache source with only a local copy = %d, want 404. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(localRepo); err != nil {
		t.Errorf("expected local copy to survive hf-cache delete, stat err = %v", err)
	}
}

// TestAPI_CacheInfo_SymlinkedOwnerLocalCopyNotListed verifies that a Local
// copy reached through a symlinked intermediate owner directory is not
// listed in the details copies: safeDeleteLocalRepo/rejectSymlinkedComponents
// would reject its delete, so listing it would advertise an undeletable copy.
// A non-symlinked candidate in another root must still be listed.
func TestAPI_CacheInfo_SymlinkedOwnerLocalCopyNotListed(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")
	scanDir := filepath.Join(root, "scan")

	// Real repo under the symlink target, reachable as localDir/owner/name.
	writeRepoFixture(t, filepath.Join(localDir, "other", "name"), "victim.gguf")
	if err := os.Symlink(filepath.Join(localDir, "other"), filepath.Join(localDir, "owner")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	// A legit candidate in a different root must keep listing.
	writeRepoFixture(t, filepath.Join(scanDir, "owner", "name"), "model.gguf")

	srv := New(Config{
		Addr:          "127.0.0.1",
		Port:          0,
		CacheDir:      cacheDir,
		LocalDir:      localDir,
		LocalScanDirs: []string{scanDir},
		Concurrency:   2,
		MaxActive:     1,
	})

	info := cacheInfoForTest(t, srv, "owner/name")
	for _, c := range info.Copies {
		if c.Source == cacheSourceLocal && filepath.Clean(c.Path) == filepath.Clean(filepath.Join(localDir, "owner", "name")) {
			t.Errorf("symlinked-owner local copy %q must not be listed: %#v", c.Path, info.Copies)
		}
	}
	localCopy := cacheCopyBySource(info.Copies, cacheSourceLocal)
	if localCopy == nil {
		t.Fatalf("no Local copy listed; want the non-symlinked scanDir candidate: %#v", info.Copies)
	}
	wantPath := filepath.Join(scanDir, "owner", "name")
	if filepath.Clean(localCopy.Path) != filepath.Clean(wantPath) {
		t.Errorf("listed Local path = %q, want %q", localCopy.Path, wantPath)
	}
}

// TestAPI_CacheInfo_HFCacheOutsideConfiguredCacheDirNotListed verifies that
// an HF-cache copy resolved through HF_HUB_CACHE pointing outside the
// configured cacheDir is not listed: the delete endpoint validates against the
// configured cacheDir, so both delete routes would reject it. A copy that
// would fail the same validation must not be advertised as deletable.
func TestAPI_CacheInfo_HFCacheOutsideConfiguredCacheDirNotListed(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	// Separate hub location, outside the configured cacheDir.
	hubHome := filepath.Join(root, "hfhome")
	t.Setenv("HF_HUB_CACHE", filepath.Join(hubHome, "hub"))

	hubRepo := filepath.Join(hubHome, "hub", "models--owner--name")
	writeRepoFixture(t, hubRepo, "blob")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	// The cache list and handleCacheInfo must not list the hub copy living
	// outside the configured cache dir.
	info := cacheInfoForTest(t, srv, "owner/name")
	if copy := cacheCopyBySource(info.Copies, cacheSourceHFCache); copy != nil {
		t.Errorf("HF cache copy outside configured cacheDir must not be listed: %#v", copy)
	}
	for _, c := range info.Copies {
		if filepath.Clean(c.Path) == filepath.Clean(hubRepo) {
			t.Errorf("hub path %q must not be listed: %#v", hubRepo, info.Copies)
		}
	}
	if _, err := os.Stat(hubRepo); err != nil {
		t.Errorf("expected hub copy outside cacheDir to survive, stat err = %v", err)
	}
}

// ---------------------------------------------------------------------
// Batch: per-copy cache deletion selectors, physical identity, and
// partial-delete retry. Each test below fails on the pre-batch candidate
// and passes after the fix.
// ---------------------------------------------------------------------

// deleteCacheReq issues a DELETE /api/cache/{repo} request against the
// handler and returns the recorder.
func deleteCacheReq(t *testing.T, srv *Server, repo string, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("DELETE", "/api/cache/"+repo+"?"+q.Encode(), nil)
	req.SetPathValue("repo", repo)
	w := httptest.NewRecorder()
	srv.handleCacheDelete(w, req)
	return w
}

// decodeDeleteSuccess decodes a delete success body and reports whether the
// machine-readable incomplete-cleanup signal was set.
func decodeDeleteSuccess(t *testing.T, w *httptest.ResponseRecorder) (bool, map[string]any) {
	t.Helper()
	var resp struct {
		Success           bool     `json:"success"`
		CleanupIncomplete bool     `json:"cleanupIncomplete"`
		CleanupWarnings   []string `json:"cleanupWarnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode delete response: %v (body=%s)", err, w.Body.String())
	}
	if !resp.Success {
		t.Errorf("success = false in a 200 response: %s", w.Body.String())
	}
	return resp.CleanupIncomplete, map[string]any{"warnings": resp.CleanupWarnings}
}

// TestAPI_CacheDelete_UnknownSourceWithValidPathRejected is defect A: an
// unknown non-empty source must be rejected 400 before the by-path branch even
// when a valid copy path is supplied, so a typo cannot delete the wrong copy.
func TestAPI_CacheDelete_UnknownSourceWithValidPathRejected(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")
	hubRepo := filepath.Join(cacheDir, "hub", "models--owner--name")
	friendlyRepo := writeFriendlyProjection(t, cacheDir, "owner", "name", "model.gguf")
	localRepo := filepath.Join(localDir, "owner", "name")
	writeRepoFixture(t, hubRepo, "model.gguf")
	writeRepoFixture(t, localRepo, "model.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	info := cacheInfoForTest(t, srv, "owner/name")
	localCopy := cacheCopyBySource(info.Copies, cacheSourceLocal)
	if localCopy == nil {
		t.Fatalf("no Local copy in %#v", info.Copies)
	}

	// Unknown label plus a perfectly valid path: must still be 400, nothing
	// deleted.
	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", "locle")
	q.Set("path", localCopy.Path)
	w := deleteCacheReq(t, srv, "owner/name", q)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown source with valid path = %d, want 400. Body: %s", w.Code, w.Body.String())
	}
	for _, path := range []string{hubRepo, friendlyRepo, localRepo} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s to survive rejected delete, stat err = %v", path, err)
		}
	}

	// A known-but-conflicting label is not a new invalid-input requirement: the
	// supplied path still selects the actual copy (valid-path precedence).
	q = url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceHFCache)
	q.Set("path", localCopy.Path)
	w = deleteCacheReq(t, srv, "owner/name", q)
	if w.Code != http.StatusOK {
		t.Fatalf("known source with valid path = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(localRepo); !os.IsNotExist(err) {
		t.Errorf("expected pathed Local copy deleted, stat err = %v", err)
	}
	if _, err := os.Stat(hubRepo); err != nil {
		t.Errorf("expected hub copy to survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_RouteDirsBehaveLikeLocalRoots verifies the semantic
// reconciliation with upstream #66: a configured download-route destination is a
// Local root like any other. It is listed and deletable as its own
// <routeDir>/<owner>/<name> repo, and it is protected as an ancestor boundary so
// a route dir can never be deleted as a synthetic two-level repo under another
// scan root.
func TestAPI_CacheDelete_RouteDirsBehaveLikeLocalRoots(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	routeDir := filepath.Join(root, "LLM", "GGUF")
	repoDir := filepath.Join(routeDir, "owner", "name")
	writeRepoFixture(t, repoDir, "model.gguf")

	srv := New(Config{
		Addr:           "127.0.0.1",
		Port:           0,
		CacheDir:       cacheDir,
		DownloadRoutes: map[string]string{"llm/gguf": routeDir},
		Concurrency:    2,
		MaxActive:      1,
	})

	// Listed as a Local copy.
	info := cacheInfoForTest(t, srv, "owner/name")
	localCopy := cacheCopyBySource(info.Copies, cacheSourceLocal)
	if localCopy == nil {
		t.Fatalf("route-dir repo not listed as Local copy: %#v", info.Copies)
	}
	if filepath.Clean(localCopy.Path) != filepath.Clean(repoDir) {
		t.Fatalf("listed Local path = %q, want %q", localCopy.Path, repoDir)
	}

	// Deletable.
	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	if w := deleteCacheReq(t, srv, "owner/name", q); w.Code != http.StatusOK {
		t.Fatalf("deleting route-dir repo = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(repoDir); !os.IsNotExist(err) {
		t.Errorf("expected route-dir repo removed, stat err = %v", err)
	}

	// A route dir nested under a parent scan root is a hard ancestor boundary:
	// the parent scan root must not advertise it as a two-level repo and must
	// not delete it.
	parent := filepath.Join(root, "parent")
	nestedRoute := filepath.Join(parent, "org", "family")
	writeRepoFixture(t, filepath.Join(nestedRoute, "model"), "deep.gguf")

	srv2 := New(Config{
		Addr:           "127.0.0.1",
		Port:           0,
		CacheDir:       cacheDir,
		LocalScanDirs:  []string{parent},
		DownloadRoutes: map[string]string{"audio": nestedRoute},
		Concurrency:    2,
		MaxActive:      1,
	})
	if ids := cacheListRepoIDs(t, srv2, nil); containsRepo(ids, "org/family") {
		t.Errorf("nested route dir advertised as a repo: %v", ids)
	}
	q2 := url.Values{}
	q2.Set("type", "model")
	q2.Set("source", cacheSourceLocal)
	if w := deleteCacheReq(t, srv2, "org/family", q2); w.Code == http.StatusOK {
		t.Fatalf("deleting a nested route dir as a repo succeeded: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(nestedRoute, "model")); err != nil {
		t.Errorf("nested route dir must survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_ScanDirEqualToCacheDirNoAncestorDelete is defect B1:
// localScanDirs=[cacheDir] must keep SkipSpecial, so /cache/models/victim with
// two model folders is NOT advertised as Local repo models/victim and deleting
// that repo does not remove both folders (synthetic ancestor deletion).
func TestAPI_CacheDelete_ScanDirEqualToCacheDirNoAncestorDelete(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	// Two independent repos under the friendly namespace.
	one := filepath.Join(cacheDir, "models", "alice", "one")
	two := filepath.Join(cacheDir, "models", "bob", "two")
	writeRepoFixture(t, one, "one.gguf")
	writeRepoFixture(t, two, "two.gguf")

	srv := New(Config{
		Addr:          "127.0.0.1",
		Port:          0,
		CacheDir:      cacheDir,
		LocalScanDirs: []string{cacheDir},
		Concurrency:   2,
		MaxActive:     1,
	})

	// The friendly namespace's direct child "models" must stay special-cased.
	req := httptest.NewRequest("GET", "/api/cache", nil)
	w := httptest.NewRecorder()
	srv.handleCacheList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cache list = %d: %s", w.Code, w.Body.String())
	}
	var list struct {
		Repos []CachedRepoInfo `json:"repos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, r := range list.Repos {
		if r.Repo == "models/victim" || r.Repo == "models/alice" || r.Repo == "models/bob" {
			t.Errorf("synthetic ancestor repo %q advertised: %#v", r.Repo, r)
		}
	}

	// Deleting the synthetic repo must not remove the two real repos.
	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	w = deleteCacheReq(t, srv, "models/victim", q)
	if w.Code == http.StatusOK {
		t.Errorf("delete of synthetic ancestor repo unexpectedly succeeded: %s", w.Body.String())
	}
	if _, err := os.Stat(one); err != nil {
		t.Errorf("expected alice/one to survive, stat err = %v", err)
	}
	if _, err := os.Stat(two); err != nil {
		t.Errorf("expected bob/two to survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_ExplicitCacheModelsRootKeepsIndependentLocal is defect B2:
// an explicit Local root at <cache>/models may hold independent real files; a
// same-named hub entry must not hide it, and deleting the HF copy must not
// remove it.
func TestAPI_CacheDelete_ExplicitCacheModelsRootKeepsIndependentLocal(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	modelsRoot := filepath.Join(cacheDir, "models")

	hubRepo := filepath.Join(cacheDir, "hub", "models--owner--name")
	writeRepoFixture(t, hubRepo, "blob")
	// Real independent Local storage under an explicit Local root at
	// <cache>/models.
	realLocal := filepath.Join(modelsRoot, "owner", "name")
	writeRepoFixture(t, realLocal, "real.gguf")

	srv := New(Config{
		Addr:          "127.0.0.1",
		Port:          0,
		CacheDir:      cacheDir,
		LocalScanDirs: []string{modelsRoot},
		Concurrency:   2,
		MaxActive:     1,
	})

	info := cacheInfoForTest(t, srv, "owner/name")
	if copy := cacheCopyBySource(info.Copies, cacheSourceHFCache); copy == nil {
		t.Errorf("expected HF cache copy in %#v", info.Copies)
	}
	localCopy := cacheCopyBySource(info.Copies, cacheSourceLocal)
	if localCopy == nil {
		t.Fatalf("independent real Local copy hidden: %#v", info.Copies)
	}
	if filepath.Clean(localCopy.Path) != filepath.Clean(realLocal) {
		t.Errorf("Local copy path = %q, want %q", localCopy.Path, realLocal)
	}

	// Deleting the HF copy must not remove the independent real Local folder.
	q := url.Values{}
	q.Set("type", "model")
	q.Set("path", filepath.Join(hubRepo))
	w := deleteCacheReq(t, srv, "owner/name", q)
	if w.Code != http.StatusOK {
		t.Fatalf("HF-cache by-path delete = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(hubRepo); !os.IsNotExist(err) {
		t.Errorf("expected hub copy deleted, stat err = %v", err)
	}
	if _, err := os.Stat(realLocal); err != nil {
		t.Errorf("independent real Local folder must survive HF delete, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_OrphanFriendlyOwnerAliasPreserved is defect B3: an owner
// alias /cache/models/orphan-alias -> /cache/models/other-orphan must not be
// advertised as repo orphan-alias/name, and deleting that repo must not remove
// the other repo's projection.
func TestAPI_CacheDelete_OrphanFriendlyOwnerAliasPreserved(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")

	// Real orphan friendly projection for other-orphan/name.
	realProjection := writeFriendlyProjection(t, cacheDir, "other-orphan", "name", "model.gguf")
	// Owner alias: models/orphan-alias -> models/other-orphan.
	aliasOwner := filepath.Join(cacheDir, "models", "orphan-alias")
	if err := os.Symlink(filepath.Join(cacheDir, "models", "other-orphan"), aliasOwner); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	// The alias must not resolve to a listed repo.
	req := httptest.NewRequest("GET", "/api/cache", nil)
	w := httptest.NewRecorder()
	srv.handleCacheList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cache list = %d: %s", w.Code, w.Body.String())
	}
	var list struct {
		Repos []CachedRepoInfo `json:"repos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, r := range list.Repos {
		if r.Repo == "orphan-alias/name" {
			t.Errorf("owner alias advertised as repo: %#v", r)
		}
	}

	// Deleting the alias repo must not remove the other repo's projection.
	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceHFCache)
	w = deleteCacheReq(t, srv, "orphan-alias/name", q)
	if w.Code == http.StatusOK {
		t.Errorf("delete via orphan alias unexpectedly succeeded: %s", w.Body.String())
	}
	if _, err := os.Stat(realProjection); err != nil {
		t.Errorf("other orphan's projection must survive alias delete, stat err = %v", err)
	}
}

// TestAPI_CacheInfo_SymlinkedRootAliasVsTwoCaseRoots is defect B4: a configured
// symlinked root plus its real path must collapse to one physical copy, while
// two truly distinct case-sensitive roots whose lowercased spellings collide
// must both stay visible.
func TestAPI_CacheInfo_SymlinkedRootAliasVsTwoCaseRoots(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")

	realRoot := filepath.Join(root, "real-local")
	aliasRoot := filepath.Join(root, "alias-local")
	writeRepoFixture(t, filepath.Join(realRoot, "owner", "name"), "model.gguf")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	// Two roots whose lowercased spellings collide. On a case-sensitive
	// filesystem they are distinct directories; skip on a case-insensitive
	// filesystem where the second creation resolves to the same directory.
	upper := filepath.Join(root, "ModelsRoot")
	lower := filepath.Join(root, "modelsroot")
	writeRepoFixture(t, filepath.Join(upper, "owner", "name"), "upper.gguf")
	writeRepoFixture(t, filepath.Join(lower, "owner", "name"), "lower.gguf")
	upperInfo, errU := os.Stat(upper)
	lowerInfo, errL := os.Stat(lower)
	if errU == nil && errL == nil && os.SameFile(upperInfo, lowerInfo) {
		t.Skip("case-insensitive filesystem: cannot test case-distinct roots")
	}

	srv := New(Config{
		Addr:          "127.0.0.1",
		Port:          0,
		CacheDir:      cacheDir,
		LocalDir:      aliasRoot,
		LocalScanDirs: []string{realRoot},
		Concurrency:   2,
		MaxActive:     1,
	})

	// Symlinked root + real path collapse to one physical Local copy.
	info := cacheInfoForTest(t, srv, "owner/name")
	if got := countCacheCopiesBySource(info.Copies, cacheSourceLocal); got != 1 {
		t.Errorf("Local copies for symlinked root + real path = %d, want 1. copies=%#v", got, info.Copies)
	}

	// Two case-distinct roots whose lowercased forms collide must both stay
	// visible (the old lowercased dedup merged them).
	srv2 := New(Config{
		Addr:          "127.0.0.1",
		Port:          0,
		CacheDir:      cacheDir,
		LocalScanDirs: []string{upper, lower},
		Concurrency:   2,
		MaxActive:     1,
	})
	info2 := cacheInfoForTest(t, srv2, "owner/name")
	if got := countCacheCopiesBySource(info2.Copies, cacheSourceLocal); got != 2 {
		t.Errorf("Local copies for two case-colliding roots = %d, want 2. copies=%#v", got, info2.Copies)
	}
}

// TestAPI_CacheDelete_LocalPartialThenRetry is defect C2: after a partial local
// delete (weights removed, a protected remainder left) the identical retry
// must succeed and remove only the remainder.
func TestAPI_CacheDelete_LocalPartialThenRetry(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")
	localRepo := filepath.Join(localDir, "owner", "name")
	writeRepoFixture(t, localRepo, "model.gguf")
	// A remainder that is not a weight file.
	remainder := filepath.Join(localRepo, "config.json")
	if err := os.WriteFile(remainder, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	siblingRepo := filepath.Join(localDir, "owner", "sibling")
	writeRepoFixture(t, siblingRepo, "sibling.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	// Force a real partial state: the delete validates and records the target,
	// then fails before removal; the test removes the weight file to simulate
	// the remainder a protected config.json would leave.
	srv.deleteStepHook = func(step string) error {
		if step == "local:before-remove" {
			_ = os.Remove(filepath.Join(localRepo, "model.gguf"))
			return fmt.Errorf("injected partial failure")
		}
		return nil
	}

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	q.Set("path", localRepo)
	w := deleteCacheReq(t, srv, "owner/name", q)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("partial local delete = %d, want 500. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(localRepo); err != nil {
		t.Fatalf("expected remainder to survive partial delete, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(localRepo, "config.json")); err != nil {
		t.Fatalf("expected config.json remainder, stat err = %v", err)
	}

	// The identical retry must succeed and remove the remainder.
	srv.deleteStepHook = nil
	w = deleteCacheReq(t, srv, "owner/name", q)
	if w.Code != http.StatusOK {
		t.Fatalf("identical retry after partial = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(localRepo); !os.IsNotExist(err) {
		t.Errorf("expected remainder removed on retry, stat err = %v", err)
	}
	if _, err := os.Stat(siblingRepo); err != nil {
		t.Errorf("expected sibling to survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_UnrelatedEmptyTwoLevelFolderStaysIneligible is defect C2:
// retry eligibility must not degrade into accepting any empty/two-level folder.
// A different, unrelated empty owner/name folder must remain non-deletable.
func TestAPI_CacheDelete_UnrelatedEmptyTwoLevelFolderStaysIneligible(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")

	// A partial delete for owner/name leaves evidence.
	localRepo := filepath.Join(localDir, "owner", "name")
	writeRepoFixture(t, localRepo, "model.gguf")

	// An unrelated empty two-level folder must never be eligible.
	unrelated := filepath.Join(localDir, "owner", "unrelated-empty")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatal(err)
	}

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	srv.deleteStepHook = func(step string) error {
		if step == "local:before-remove" {
			_ = os.Remove(filepath.Join(localRepo, "model.gguf"))
			return fmt.Errorf("injected partial failure")
		}
		return nil
	}
	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	q.Set("path", localRepo)
	if w := deleteCacheReq(t, srv, "owner/name", q); w.Code != http.StatusInternalServerError {
		t.Fatalf("expected partial 500, got %d: %s", w.Code, w.Body.String())
	}
	srv.deleteStepHook = nil

	// A no-path delete for the unrelated empty repo must still be 404.
	q2 := url.Values{}
	q2.Set("type", "model")
	q2.Set("source", cacheSourceLocal)
	w := deleteCacheReq(t, srv, "owner/unrelated-empty", q2)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unrelated empty folder delete = %d, want 404. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated empty folder must survive, stat err = %v", err)
	}

	// The retained owner/name evidence must not authorize the unrelated path.
	q3 := url.Values{}
	q3.Set("type", "model")
	q3.Set("source", cacheSourceLocal)
	q3.Set("path", unrelated)
	w = deleteCacheReq(t, srv, "owner/name", q3)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unrelated path with owner/name repo = %d, want 400. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated path must survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_ChangedTargetBeforeRetryInvalidates is defect C2: retry
// eligibility is bound to the configured root, so a changed root invalidates
// the continuation rather than redirecting it to another copy.
func TestAPI_CacheDelete_ChangedTargetBeforeRetryInvalidates(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")
	otherDir := filepath.Join(root, "other")
	localRepo := filepath.Join(localDir, "owner", "name")
	otherRepo := filepath.Join(otherDir, "owner", "name")
	writeRepoFixture(t, localRepo, "model.gguf")
	writeRepoFixture(t, otherRepo, "model.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	srv.deleteStepHook = func(step string) error {
		if step == "local:before-remove" {
			_ = os.Remove(filepath.Join(localRepo, "model.gguf"))
			return fmt.Errorf("injected partial failure")
		}
		return nil
	}
	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	q.Set("path", localRepo)
	if w := deleteCacheReq(t, srv, "owner/name", q); w.Code != http.StatusInternalServerError {
		t.Fatalf("expected partial 500, got %d: %s", w.Code, w.Body.String())
	}
	srv.deleteStepHook = nil

	// Change the configured Local root: the recorded target no longer belongs
	// to an authorized root, so the identical retry must NOT succeed and must
	// not delete the other copy instead.
	srv.configMu.Lock()
	srv.config.LocalDir = otherDir
	srv.configMu.Unlock()

	w := deleteCacheReq(t, srv, "owner/name", q)
	if w.Code == http.StatusOK {
		t.Errorf("retry after root change unexpectedly succeeded: %s", w.Body.String())
	}
	if _, err := os.Stat(otherRepo); err != nil {
		t.Errorf("other copy must survive invalidated retry, stat err = %v", err)
	}
	if _, err := os.Stat(localRepo); err != nil {
		t.Errorf("original remainder must survive invalidated retry, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_HFRemovedProtectedProjectionIncomplete is defect C1: when
// the HF hub directory is removed but the friendly projection cleanup is
// protected, the response must still report primary success while exposing a
// machine-readable incomplete-cleanup signal.
func TestAPI_CacheDelete_HFRemovedProtectedProjectionIncomplete(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	hubRepo := filepath.Join(cacheDir, "hub", "models--owner--name")
	writeRepoFixture(t, hubRepo, "blob")
	friendlyRepo := writeFriendlyProjection(t, cacheDir, "owner", "name", "model.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		Concurrency: 2,
		MaxActive:   1,
	})
	srv.deleteStepHook = func(step string) error {
		if step == "hf:friendly-cleanup" {
			return fmt.Errorf("injected protected projection failure")
		}
		return nil
	}

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceHFCache)
	w := deleteCacheReq(t, srv, "owner/name", q)
	if w.Code != http.StatusOK {
		t.Fatalf("HF delete with protected projection = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	incomplete, _ := decodeDeleteSuccess(t, w)
	if !incomplete {
		t.Errorf("expected cleanupIncomplete=true, got body %s", w.Body.String())
	}
	if _, err := os.Stat(hubRepo); !os.IsNotExist(err) {
		t.Errorf("expected hub removed, stat err = %v", err)
	}
	if _, err := os.Stat(friendlyRepo); err != nil {
		t.Errorf("protected projection must survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_HFFriendlyCleanupOfRealFolderPreservedAndSignaled
// verifies that a friendly directory containing real files is never removed by
// an HF-cache delete (safe default) and that the response exposes the
// incomplete-cleanup signal rather than silently reporting full success.
func TestAPI_CacheDelete_HFFriendlyCleanupOfRealFolderPreservedAndSignaled(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	hubRepo := filepath.Join(cacheDir, "hub", "models--owner--name")
	writeRepoFixture(t, hubRepo, "blob")
	// A real friendly folder (regular files, not a symlink projection).
	friendlyRepo := filepath.Join(cacheDir, "models", "owner", "name")
	writeRepoFixture(t, friendlyRepo, "real.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceHFCache)
	w := deleteCacheReq(t, srv, "owner/name", q)
	if w.Code != http.StatusOK {
		t.Fatalf("HF delete with real friendly folder = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	incomplete, _ := decodeDeleteSuccess(t, w)
	if !incomplete {
		t.Errorf("expected cleanupIncomplete=true for preserved real folder, got %s", w.Body.String())
	}
	if _, err := os.Stat(hubRepo); !os.IsNotExist(err) {
		t.Errorf("expected hub removed, stat err = %v", err)
	}
	if _, err := os.Stat(friendlyRepo); err != nil {
		t.Errorf("real friendly folder must be preserved, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_SymlinkedConfiguredRootWorks preserves the behavior that
// a configured root may itself be a symlink: only components below the root are
// restricted, so a repo directly under a symlinked root is still deletable.
func TestAPI_CacheDelete_SymlinkedConfiguredRootWorks(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	realRoot := filepath.Join(root, "real-local")
	linkRoot := filepath.Join(root, "link-local")
	repoDir := filepath.Join(realRoot, "owner", "name")
	writeRepoFixture(t, repoDir, "model.gguf")
	sibling := filepath.Join(realRoot, "owner", "sibling")
	writeRepoFixture(t, sibling, "sibling.gguf")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    linkRoot,
		Concurrency: 2,
		MaxActive:   1,
	})

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	w := deleteCacheReq(t, srv, "owner/name", q)
	if w.Code != http.StatusOK {
		t.Fatalf("delete under symlinked configured root = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(repoDir); !os.IsNotExist(err) {
		t.Errorf("expected repo under symlinked root deleted, stat err = %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("expected sibling to survive, stat err = %v", err)
	}
}

// ---------------------------------------------------------------------
// Batch: leaf-repo ownership predicate for Local deletion (H1) and
// platform-aware physical-key case folding (M2). Each H1 test fails on
// the pre-batch candidate by advertising and deleting a container, and
// passes once a Local unit must be a leaf model directory.
// ---------------------------------------------------------------------

// cacheListRepoIDs issues GET /api/cache and returns the advertised repo IDs.
func cacheListRepoIDs(t *testing.T, srv *Server, q url.Values) []string {
	t.Helper()
	path := "/api/cache"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	srv.handleCacheList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cache list = %d: %s", w.Code, w.Body.String())
	}
	var list struct {
		Repos []CachedRepoInfo `json:"repos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode cache list: %v", err)
	}
	ids := make([]string, 0, len(list.Repos))
	for _, r := range list.Repos {
		ids = append(ids, r.Repo)
	}
	return ids
}

func containsRepo(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestAPI_CacheDelete_ScanRootDoesNotAdvertiseCacheDir is H1 case 1: a
// home-like scan root whose two-level descendant is the HF cache dir must not
// advertise or delete that directory. Before the leaf predicate, `.cache` /
// `huggingface` collapsed onto the whole cache dir and deleting it wiped the
// cache.
func TestAPI_CacheDelete_ScanRootDoesNotAdvertiseCacheDir(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cacheDir := filepath.Join(home, ".cache", "huggingface")
	hubRepo := filepath.Join(cacheDir, "hub", "models--alice--one")
	writeRepoFixture(t, hubRepo, "sha256")
	// A real friendly model too, so the cache dir is clearly non-empty.
	writeRepoFixture(t, filepath.Join(cacheDir, "models", "alice", "one"), "model.gguf")

	srv := New(Config{
		Addr:          "127.0.0.1",
		Port:          0,
		CacheDir:      cacheDir,
		LocalScanDirs: []string{home},
		Concurrency:   2,
		MaxActive:     1,
	})

	if ids := cacheListRepoIDs(t, srv, nil); containsRepo(ids, ".cache/huggingface") {
		t.Errorf("cache dir advertised as Local repo: %v", ids)
	}

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	w := deleteCacheReq(t, srv, ".cache/huggingface", q)
	if w.Code == http.StatusOK {
		t.Fatalf("deleting the cache dir via scan root succeeded: %s", w.Body.String())
	}
	if _, err := os.Stat(cacheDir); err != nil {
		t.Fatalf("cache dir must survive, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "models", "alice", "one", "model.gguf")); err != nil {
		t.Errorf("friendly model must survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_ParentScanRootDoesNotExposeFriendlyNamespace is H1 case 2:
// a scan root that is the parent of the cache dir makes `cache` / `models`
// resolve to the friendly namespace. That namespace must not be advertised or
// deleted, because deleting it removes every repo under it.
func TestAPI_CacheDelete_ParentScanRootDoesNotExposeFriendlyNamespace(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	alice := filepath.Join(cacheDir, "models", "alice", "one")
	bob := filepath.Join(cacheDir, "models", "bob", "two")
	writeRepoFixture(t, alice, "one.gguf")
	writeRepoFixture(t, bob, "two.gguf")

	srv := New(Config{
		Addr:          "127.0.0.1",
		Port:          0,
		CacheDir:      cacheDir,
		LocalScanDirs: []string{root},
		Concurrency:   2,
		MaxActive:     1,
	})

	if ids := cacheListRepoIDs(t, srv, nil); containsRepo(ids, "cache/models") {
		t.Errorf("friendly namespace advertised as Local repo: %v", ids)
	}

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	w := deleteCacheReq(t, srv, "cache/models", q)
	if w.Code == http.StatusOK {
		t.Fatalf("deleting the friendly namespace via parent scan root succeeded: %s", w.Body.String())
	}
	if _, err := os.Stat(alice); err != nil {
		t.Errorf("alice/one must survive, stat err = %v", err)
	}
	if _, err := os.Stat(bob); err != nil {
		t.Errorf("bob/two must survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_OverlappingRootsDoNotExposeContainer is H1 case 3:
// overlapping parent/child scan roots expose `models/alice` from the parent
// root, a container holding `alice/one`. The container must not be advertised
// or deleted, while the real leaf `alice/one` from the child root stays
// deletable.
func TestAPI_CacheDelete_OverlappingRootsDoNotExposeContainer(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	scanRoot := filepath.Join(root, "scan")
	modelsRoot := filepath.Join(scanRoot, "models")
	container := filepath.Join(modelsRoot, "alice")
	leaf := filepath.Join(container, "one")
	writeRepoFixture(t, leaf, "model.gguf")
	// The container itself owns no weight file directly; only the nested leaf
	// does, which makes it a container rather than a leaf model.

	srv := New(Config{
		Addr:          "127.0.0.1",
		Port:          0,
		CacheDir:      cacheDir,
		LocalScanDirs: []string{scanRoot, modelsRoot},
		Concurrency:   2,
		MaxActive:     1,
	})

	ids := cacheListRepoIDs(t, srv, nil)
	if containsRepo(ids, "models/alice") {
		t.Errorf("container advertised as a repo: %v", ids)
	}
	if !containsRepo(ids, "alice/one") {
		t.Errorf("real leaf alice/one not advertised: %v", ids)
	}

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	// By-path on the container must also be rejected and delete nothing.
	q.Set("path", container)
	w := deleteCacheReq(t, srv, "models/alice", q)
	if w.Code == http.StatusOK {
		t.Fatalf("deleting the container succeeded: %s", w.Body.String())
	}
	if _, err := os.Stat(leaf); err != nil {
		t.Errorf("nested leaf must survive, stat err = %v", err)
	}

	// The real leaf from the child root is deletable.
	q2 := url.Values{}
	q2.Set("type", "model")
	q2.Set("source", cacheSourceLocal)
	if w := deleteCacheReq(t, srv, "alice/one", q2); w.Code != http.StatusOK {
		t.Fatalf("deleting real leaf alice/one = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(leaf); !os.IsNotExist(err) {
		t.Errorf("expected real leaf removed, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_DeepLibraryNestedWeightsIsOneDeletableModel documents the
// user-approved Local-model semantics: a locally stored model is exactly
// <root>/<owner>/<name> when it contains at least one weight file ANYWHERE
// inside it, including nested subdirectories. There is deliberately no notion of
// a nested repo deeper than <root>/<owner>/<name> under a single root; only the
// hard boundaries (the cache dir, the friendly namespaces, and configured
// roots) are excluded. So with LocalDir=lib and the layout
// lib/org/family/model/model.gguf, `org` / `family` is ONE legitimate model: it
// is listed and deleting it removes exactly lib/org/family, including the
// nested model folder.
func TestAPI_CacheDelete_DeepLibraryNestedWeightsIsOneDeletableModel(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	libDir := filepath.Join(root, "lib")
	nested := filepath.Join(libDir, "org", "family", "model")
	writeRepoFixture(t, nested, "model.gguf")
	// A sibling model under the same owner stays untouched.
	sibling := filepath.Join(libDir, "org", "other", "model")
	writeRepoFixture(t, sibling, "other.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    libDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	ids := cacheListRepoIDs(t, srv, nil)
	if !containsRepo(ids, "org/family") {
		t.Fatalf("deep nested-weight model org/family not advertised: %v", ids)
	}
	if !containsRepo(ids, "org/other") {
		t.Errorf("sibling org/other not advertised: %v", ids)
	}

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	if w := deleteCacheReq(t, srv, "org/family", q); w.Code != http.StatusOK {
		t.Fatalf("deleting deep nested-weight model = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(libDir, "org", "family")); !os.IsNotExist(err) {
		t.Errorf("expected lib/org/family removed as one model, stat err = %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("expected sibling org/other to survive, stat err = %v", err)
	}

	// Negative: a configured root that is reachable as a two-level repo under
	// another configured scan root is a hard boundary and stays non-deletable,
	// even though it contains nested weights.
	parent := filepath.Join(root, "parent")
	configuredRoot := filepath.Join(parent, "org", "family")
	writeRepoFixture(t, filepath.Join(configuredRoot, "model"), "deep.gguf")

	srv2 := New(Config{
		Addr:          "127.0.0.1",
		Port:          0,
		CacheDir:      cacheDir,
		LocalDir:      configuredRoot,
		LocalScanDirs: []string{parent},
		Concurrency:   2,
		MaxActive:     1,
	})

	if ids := cacheListRepoIDs(t, srv2, nil); containsRepo(ids, "org/family") {
		t.Errorf("configured root advertised as Local repo: %v", ids)
	}
	q2 := url.Values{}
	q2.Set("type", "model")
	q2.Set("source", cacheSourceLocal)
	if w := deleteCacheReq(t, srv2, "org/family", q2); w.Code == http.StatusOK {
		t.Fatalf("deleting the configured root as a repo succeeded: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(configuredRoot, "model")); err != nil {
		t.Errorf("configured root must survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_DiffusersLeafWithComponentSubdirs covers the user-approved
// semantics for diffusers-style models: a single Local model whose weights live
// only under component subdirectories (<repo>/unet, <repo>/vae, ...) is one
// model. It is listed as a Local copy and a by-path delete removes exactly the
// <localDir>/alice/diff folder, component subdirs included, leaving siblings.
func TestAPI_CacheDelete_DiffusersLeafWithComponentSubdirs(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")
	repoDir := filepath.Join(localDir, "alice", "diff")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "model_index.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRepoFixture(t, filepath.Join(repoDir, "unet"), "weights.safetensors")
	writeRepoFixture(t, filepath.Join(repoDir, "vae"), "weights.safetensors")
	sibling := filepath.Join(localDir, "alice", "sibling")
	writeRepoFixture(t, sibling, "sibling.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	info := cacheInfoForTest(t, srv, "alice/diff")
	localCopy := cacheCopyBySource(info.Copies, cacheSourceLocal)
	if localCopy == nil {
		t.Fatalf("diffusers model not listed as Local copy: %#v", info.Copies)
	}
	if filepath.Clean(localCopy.Path) != filepath.Clean(repoDir) {
		t.Errorf("listed Local path = %q, want %q", localCopy.Path, repoDir)
	}

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	q.Set("path", localCopy.Path)
	if w := deleteCacheReq(t, srv, "alice/diff", q); w.Code != http.StatusOK {
		t.Fatalf("by-path delete of diffusers model = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(repoDir); !os.IsNotExist(err) {
		t.Errorf("expected diffusers model folder removed, stat err = %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("expected sibling to survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_FilteredLeafSubdir covers filtered layouts: a model whose
// weights live under a filter subdirectory (<repo>/q4_k_m/model.gguf) is still
// one model. It is listed and a no-path Local delete removes exactly that
// folder, leaving siblings.
func TestAPI_CacheDelete_FilteredLeafSubdir(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")
	repoDir := filepath.Join(localDir, "alice", "qwen")
	writeRepoFixture(t, filepath.Join(repoDir, "q4_k_m"), "model.gguf")
	sibling := filepath.Join(localDir, "alice", "sibling")
	writeRepoFixture(t, sibling, "sibling.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	info := cacheInfoForTest(t, srv, "alice/qwen")
	localCopy := cacheCopyBySource(info.Copies, cacheSourceLocal)
	if localCopy == nil {
		t.Fatalf("filtered model not listed as Local copy: %#v", info.Copies)
	}
	if filepath.Clean(localCopy.Path) != filepath.Clean(repoDir) {
		t.Errorf("listed Local path = %q, want %q", localCopy.Path, repoDir)
	}

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	if w := deleteCacheReq(t, srv, "alice/qwen", q); w.Code != http.StatusOK {
		t.Fatalf("no-path delete of filtered model = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(repoDir); !os.IsNotExist(err) {
		t.Errorf("expected filtered model folder removed, stat err = %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("expected sibling to survive, stat err = %v", err)
	}
}

// TestAPI_CacheDelete_LeafModelWithNestedNonWeightDirsDeletes is the H1
// positive control: a legitimate single-model folder containing shards, an
// mmproj companion, and nested non-weight subdirectories (including a nested
// real hfd.yaml) is still a leaf and must delete, removing the whole folder
// while leaving siblings intact.
func TestAPI_CacheDelete_LeafModelWithNestedNonWeightDirsDeletes(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "local")
	repoDir := filepath.Join(localDir, "owner", "name")
	writeRepoFixture(t, repoDir, "model-00001-of-00002.safetensors")
	writeRepoFixture(t, repoDir, "model-00002-of-00002.safetensors")
	writeRepoFixture(t, repoDir, "mmproj-F16.gguf")
	if err := os.MkdirAll(filepath.Join(repoDir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "configs", "generation.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A nested real hfd.yaml is a non-weight file and must not disqualify a
	// folder that directly owns its weights.
	if err := os.WriteFile(filepath.Join(repoDir, "configs", "hfd.yaml"), []byte("nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(localDir, "owner", "sibling")
	writeRepoFixture(t, sibling, "sibling.gguf")

	srv := New(Config{
		Addr:        "127.0.0.1",
		Port:        0,
		CacheDir:    cacheDir,
		LocalDir:    localDir,
		Concurrency: 2,
		MaxActive:   1,
	})

	// Advertised and deletable.
	info := cacheInfoForTest(t, srv, "owner/name")
	localCopy := cacheCopyBySource(info.Copies, cacheSourceLocal)
	if localCopy == nil {
		t.Fatalf("legit leaf model not listed as Local copy: %#v", info.Copies)
	}
	if filepath.Clean(localCopy.Path) != filepath.Clean(repoDir) {
		t.Errorf("listed Local path = %q, want %q", localCopy.Path, repoDir)
	}

	q := url.Values{}
	q.Set("type", "model")
	q.Set("source", cacheSourceLocal)
	if w := deleteCacheReq(t, srv, "owner/name", q); w.Code != http.StatusOK {
		t.Fatalf("deleting legit leaf model = %d, want 200. Body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(repoDir); !os.IsNotExist(err) {
		t.Errorf("expected leaf model folder removed, stat err = %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("expected sibling to survive, stat err = %v", err)
	}
}

// TestPhysPathKey_CaseFoldByPlatform is M2: on platforms whose default
// filesystem is case-insensitive (Windows, macOS) the physical key folds case,
// while Linux preserves it so truly distinct case-sensitive roots stay
// distinct. Symlink aliases collapse on every platform.
func TestPathIdentityKey_CaseFoldByPlatform(t *testing.T) {
	// The platform decision itself.
	for _, tc := range []struct {
		goos string
		want bool
	}{
		{"linux", false},
		{"windows", true},
		{"darwin", true},
		{"freebsd", false},
	} {
		if got := pathIdentityKeyCaseInsensitive(tc.goos); got != tc.want {
			t.Errorf("pathIdentityKeyCaseInsensitive(%q) = %v, want %v", tc.goos, got, tc.want)
		}
	}

	root := t.TempDir()
	mixed := filepath.Join(root, "ModelsRoot")
	if err := os.MkdirAll(mixed, 0o755); err != nil {
		t.Fatal(err)
	}

	// Host-independent: both keys resolve the same path, and darwin/windows
	// differ from linux only by case folding.
	linuxKey := pathIdentityKeyForGOOS(mixed, "linux")
	for _, goos := range []string{"windows", "darwin"} {
		got := pathIdentityKeyForGOOS(mixed, goos)
		if got != strings.ToLower(linuxKey) {
			t.Errorf("%s key = %q, want folded %q", goos, got, strings.ToLower(linuxKey))
		}
	}
	if linuxKey == strings.ToLower(linuxKey) {
		// The path's case did not survive on-disk resolution (case-insensitive
		// host); the fold-vs-preserve distinction cannot be exercised here.
		t.Logf("host resolved %q to %q; skipping case-preservation assertions", mixed, linuxKey)
		return
	}

	// Two case-distinct roots whose lowercased spellings collide must stay
	// distinct on Linux.
	other := filepath.Join(root, "modelsroot")
	if pathIdentityKeyForGOOS(mixed, "linux") == pathIdentityKeyForGOOS(other, "linux") {
		t.Errorf("linux keys collapsed distinct case roots: %q", pathIdentityKeyForGOOS(mixed, "linux"))
	}
	if pathIdentityKeyForGOOS(mixed, "darwin") != pathIdentityKeyForGOOS(other, "darwin") {
		t.Errorf("darwin keys did not collapse case-differing aliases")
	}

	// Symlink alias and real path collapse on every platform.
	realDir := filepath.Join(root, "real-local")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(root, "alias-local")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	for _, goos := range []string{"linux", "windows", "darwin"} {
		if pathIdentityKeyForGOOS(aliasDir, goos) != pathIdentityKeyForGOOS(realDir, goos) {
			t.Errorf("%s: symlink alias did not collapse to real path", goos)
		}
	}
}
