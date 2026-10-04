// Copyright 2025
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
	"testing"
	"time"
)

func TestResolveRoute(t *testing.T) {
	routes := map[string]string{
		"llm":             "/models/LLM",
		"llm/gguf":        "/models/LLM/GGUF",
		"llm/safetensors": "  /models/LLM/Safetensors/  ",
		"audio":           "/models/Audio",
		"diffusion":       "   ", // whitespace-only is unconfigured
	}

	tests := []struct {
		name string
		key  string
		want string
	}{
		{"fine key wins over coarse", "llm/gguf", "/models/LLM/GGUF"},
		{"fine key falls back to coarse", "llm/safetensors", "/models/LLM/Safetensors"},
		{"coarse key configured", "llm", "/models/LLM"},
		{"top-level key", "audio", "/models/Audio"},
		{"whitespace value treated as unconfigured", "diffusion", ""},
		{"unconfigured known key", "embedding", ""},
		{"unknown key", "not-a-key", ""},
		{"empty key", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveRoute(routes, tt.key); got != tt.want {
				t.Errorf("resolveRoute(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}

	t.Run("nil map", func(t *testing.T) {
		if got := resolveRoute(nil, "llm"); got != "" {
			t.Errorf("resolveRoute(nil) = %q, want empty", got)
		}
	})

	t.Run("safetensors falls back to llm when fine unset", func(t *testing.T) {
		got := resolveRoute(map[string]string{"llm": "/models/LLM"}, "llm/safetensors")
		if got != "/models/LLM" {
			t.Errorf("got %q, want coarse fallback /models/LLM", got)
		}
	})
}

func TestNormalizeDownloadRoutes(t *testing.T) {
	got := normalizeDownloadRoutes(map[string]string{
		"audio":     "  /models/Audio/  ",
		"diffusion": "   ", // dropped
		"embedding": "/models/Embedding",
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 routes after dropping empty, got %d: %#v", len(got), got)
	}
	if got["audio"] != "/models/Audio" {
		t.Errorf("audio = %q, want /models/Audio", got["audio"])
	}
	if _, ok := got["diffusion"]; ok {
		t.Errorf("whitespace-only route should be dropped, got %q", got["diffusion"])
	}

	if v := normalizeDownloadRoutes(map[string]string{"audio": "  "}); v != nil {
		t.Errorf("all-empty map should normalize to nil, got %#v", v)
	}
}

func TestSanitizeDownloadRoutes(t *testing.T) {
	got := sanitizeDownloadRoutes(map[string]string{
		"llm":       "  /models/LLM/  ",
		"llm/gguf":  "/models/LLM/GGUF",
		"llm/gptq":  "/models/GPTQ", // unknown key: dropped, value not a path
		"not-a-key": "/etc/evil",    // unknown key: dropped
		"diffusion": "   ",          // known but empty: dropped
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 valid routes, got %d: %#v", len(got), got)
	}
	if got["llm"] != "/models/LLM" {
		t.Errorf("llm = %q, want trimmed/cleaned /models/LLM", got["llm"])
	}
	if got["llm/gguf"] != "/models/LLM/GGUF" {
		t.Errorf("llm/gguf = %q, want /models/LLM/GGUF", got["llm/gguf"])
	}
	for _, k := range []string{"llm/gptq", "not-a-key", "diffusion"} {
		if _, ok := got[k]; ok {
			t.Errorf("key %q should have been dropped: %#v", k, got)
		}
	}

	if v := sanitizeDownloadRoutes(map[string]string{"not-a-key": "/x"}); v != nil {
		t.Errorf("all-unknown map should sanitize to nil, got %#v", v)
	}
	if v := sanitizeDownloadRoutes(nil); v != nil {
		t.Errorf("nil map should sanitize to nil, got %#v", v)
	}
}

func TestRouteDirs(t *testing.T) {
	dirs := routeDirs(map[string]string{
		"llm/gguf":        "/models/LLM/GGUF",
		"llm/safetensors": "/models/LLM/Safetensors",
		"audio":           "/models/Audio/", // same path after Clean as another key below
		"embedding":       "/models/Audio",
		"diffusion":       "   ",             // dropped (empty)
		"not-a-key":       "/models/Unknown", // ignored (outside closed set)
	})
	// Dedup is case-insensitive by cleaned path; /models/Audio appears twice.
	if len(dirs) != 3 {
		t.Fatalf("expected 3 distinct route dirs, got %d: %#v", len(dirs), dirs)
	}
	for _, d := range dirs {
		if strings.Contains(d, "Unknown") {
			t.Errorf("unknown-key path leaked into route dirs: %#v", dirs)
		}
	}
	for i := 1; i < len(dirs); i++ {
		if dirs[i-1] >= dirs[i] {
			t.Errorf("route dirs not sorted: %#v", dirs)
		}
	}
}

// newRouteTestManager builds a manager whose jobs stall on a local endpoint so
// tests can inspect job fields without hitting the real Hub. Callers must call
// cleanup, which drains the jobs and shuts the endpoint down.
//
// Jobs run with a high max-active so every created job reaches Running
// immediately; this matters because CancelJob can only cancel a job whose
// cancel hook is already installed (runJob allocates it at start). A job still
// in the dispatch "starting" gate has a nil cancel hook, so cancelling it
// would let its runJob outlive the stall endpoint and hang stall.Close().
// Cleanup therefore waits until every non-terminal job has reached Running
// before cancelling.
func newRouteTestManager(t *testing.T, cfg Config) (*JobManager, func()) {
	t.Helper()
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	cfg.Endpoint = stall.URL
	if cfg.MaxActive == 0 {
		cfg.MaxActive = 1000
	}
	hub := NewWSHub()
	go hub.Run()
	mgr := NewJobManager(cfg, hub)
	cleanup := func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			allRunning := true
			for _, j := range mgr.ListJobs() {
				if j.Status == JobStatusQueued {
					allRunning = false
					break
				}
			}
			if allRunning || time.Now().After(deadline) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		for _, j := range mgr.ListJobs() {
			mgr.CancelJob(j.ID)
		}
		mgr.WaitAll(10 * time.Second)
		stall.Close()
	}
	return mgr, cleanup
}

func TestJobManager_CreateJob_RouteKey(t *testing.T) {
	cacheDir := t.TempDir()
	routeDir := filepath.Join(t.TempDir(), "LLM", "GGUF")
	coarseDir := filepath.Join(t.TempDir(), "LLM")
	mgr, cleanup := newRouteTestManager(t, Config{
		CacheDir: cacheDir,
		DownloadRoutes: map[string]string{
			"llm":      coarseDir,
			"llm/gguf": routeDir,
		},
	})
	defer cleanup()

	t.Run("route key sets flat destination", func(t *testing.T) {
		job, _, err := mgr.CreateJob(DownloadRequest{Repo: "route/gguf", RouteKey: "llm/gguf"})
		if err != nil {
			t.Fatalf("CreateJob failed: %v", err)
		}
		if job.LocalDir != routeDir {
			t.Errorf("LocalDir = %q, want route dir %q", job.LocalDir, routeDir)
		}
		if !job.Flat {
			t.Error("Flat should be true for a routed download")
		}
		if job.OutputDir != routeDir {
			t.Errorf("OutputDir = %q, want %q", job.OutputDir, routeDir)
		}
		if job.RouteKey != "llm/gguf" {
			t.Errorf("RouteKey = %q, want llm/gguf", job.RouteKey)
		}
	})

	t.Run("coarse fallback used when fine key unset", func(t *testing.T) {
		job, _, err := mgr.CreateJob(DownloadRequest{Repo: "route/safe", RouteKey: "llm/safetensors"})
		if err != nil {
			t.Fatalf("CreateJob failed: %v", err)
		}
		if job.LocalDir != coarseDir {
			t.Errorf("LocalDir = %q, want coarse llm route %q", job.LocalDir, coarseDir)
		}
	})

	t.Run("explicit localDir wins over route key", func(t *testing.T) {
		explicit := filepath.Join(t.TempDir(), "Explicit")
		job, _, err := mgr.CreateJob(DownloadRequest{Repo: "route/override", RouteKey: "llm/gguf", LocalDir: explicit})
		if err != nil {
			t.Fatalf("CreateJob failed: %v", err)
		}
		if job.LocalDir != explicit {
			t.Errorf("LocalDir = %q, want explicit override %q", job.LocalDir, explicit)
		}
	})

	t.Run("no key and no localDir uses HF cache", func(t *testing.T) {
		job, _, err := mgr.CreateJob(DownloadRequest{Repo: "route/plain"})
		if err != nil {
			t.Fatalf("CreateJob failed: %v", err)
		}
		if job.LocalDir != "" || job.Flat {
			t.Errorf("expected HF cache mode, got LocalDir=%q Flat=%v", job.LocalDir, job.Flat)
		}
		if job.OutputDir != cacheDir {
			t.Errorf("OutputDir = %q, want cache dir %q", job.OutputDir, cacheDir)
		}
	})

	t.Run("unknown route key is rejected", func(t *testing.T) {
		_, _, err := mgr.CreateJob(DownloadRequest{Repo: "route/bad", RouteKey: "not-a-key"})
		if err != errInvalidRouteKey {
			t.Fatalf("err = %v, want errInvalidRouteKey", err)
		}
	})

	t.Run("unconfigured known key falls back to LocalDir", func(t *testing.T) {
		job, _, err := mgr.CreateJob(DownloadRequest{Repo: "route/unconf", RouteKey: "embedding"})
		if err != nil {
			t.Fatalf("CreateJob failed: %v", err)
		}
		if job.LocalDir != "" || job.Flat {
			t.Errorf("unconfigured route should keep HF cache behavior, got LocalDir=%q Flat=%v", job.LocalDir, job.Flat)
		}
	})
}

// TestJobManager_CreateJob_DatasetIgnoresRouteKey is the regression guard for
// the plan invariant (plan.md §2.5/§6): a dataset is never routed to a model
// key. A raw API request with dataset:true plus routeKey must fall through to
// the existing LocalDir/HF-cache behavior regardless of the key.
func TestJobManager_CreateJob_DatasetIgnoresRouteKey(t *testing.T) {
	cacheDir := t.TempDir()
	routeDir := filepath.Join(t.TempDir(), "LLM", "GGUF")

	t.Run("dataset ignores route key and keeps LocalDir", func(t *testing.T) {
		localDir := filepath.Join(t.TempDir(), "models")
		mgr, cleanup := newRouteTestManager(t, Config{
			CacheDir:       cacheDir,
			LocalDir:       localDir,
			DownloadRoutes: map[string]string{"llm/gguf": routeDir},
		})
		defer cleanup()

		job, _, err := mgr.CreateJob(DownloadRequest{Repo: "owner/data", Dataset: true, RouteKey: "llm/gguf"})
		if err != nil {
			t.Fatalf("CreateJob failed: %v", err)
		}
		if job.LocalDir != localDir {
			t.Errorf("LocalDir = %q, want server LocalDir %q (dataset must not be routed)", job.LocalDir, localDir)
		}
		if job.LocalDir == routeDir {
			t.Errorf("dataset landed in route folder %q", routeDir)
		}
		if job.OutputDir != localDir || !job.Flat {
			t.Errorf("OutputDir=%q Flat=%v, want %q/true", job.OutputDir, job.Flat, localDir)
		}
		if job.RouteKey != "" {
			t.Errorf("RouteKey = %q, want empty for an unrouted dataset", job.RouteKey)
		}
	})

	t.Run("dataset ignores route key and uses HF cache", func(t *testing.T) {
		mgr, cleanup := newRouteTestManager(t, Config{
			CacheDir:       cacheDir,
			DownloadRoutes: map[string]string{"llm/gguf": routeDir},
		})
		defer cleanup()

		job, _, err := mgr.CreateJob(DownloadRequest{Repo: "owner/data2", Dataset: true, RouteKey: "llm/gguf"})
		if err != nil {
			t.Fatalf("CreateJob failed: %v", err)
		}
		if job.LocalDir != "" || job.Flat {
			t.Errorf("dataset with route key should keep HF cache behavior, got LocalDir=%q Flat=%v", job.LocalDir, job.Flat)
		}
		if job.OutputDir != cacheDir {
			t.Errorf("OutputDir = %q, want cache dir %q", job.OutputDir, cacheDir)
		}
		if job.RouteKey != "" {
			t.Errorf("RouteKey = %q, want empty for an unrouted dataset", job.RouteKey)
		}
	})

	t.Run("dataset ignores even an unknown route key", func(t *testing.T) {
		mgr, cleanup := newRouteTestManager(t, Config{CacheDir: cacheDir})
		defer cleanup()

		// Ignoring routeKey for datasets means no new 400 for a request that
		// previously succeeded; the dataset simply follows default behavior.
		if _, _, err := mgr.CreateJob(DownloadRequest{Repo: "owner/data3", Dataset: true, RouteKey: "not-a-key"}); err != nil {
			t.Fatalf("dataset route key must be ignored, got error: %v", err)
		}
	})

	t.Run("model with same route key is still routed", func(t *testing.T) {
		mgr, cleanup := newRouteTestManager(t, Config{
			CacheDir:       cacheDir,
			DownloadRoutes: map[string]string{"llm/gguf": routeDir},
		})
		defer cleanup()

		job, _, err := mgr.CreateJob(DownloadRequest{Repo: "owner/model", RouteKey: "llm/gguf"})
		if err != nil {
			t.Fatalf("CreateJob failed: %v", err)
		}
		if job.LocalDir != routeDir {
			t.Errorf("LocalDir = %q, want route dir %q", job.LocalDir, routeDir)
		}
	})
}

func TestJobManager_RouteKey_Dedup(t *testing.T) {
	mgr, cleanup := newRouteTestManager(t, Config{
		CacheDir: t.TempDir(),
		DownloadRoutes: map[string]string{
			"llm/gguf": filepath.Join(t.TempDir(), "GGUF"),
			"audio":    filepath.Join(t.TempDir(), "Audio"),
		},
	})
	defer cleanup()

	job1, existing1, err := mgr.CreateJob(DownloadRequest{Repo: "dedup/routed", RouteKey: "llm/gguf"})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}
	if existing1 {
		t.Fatal("first routed job should not be existing")
	}

	// Same repo to a different destination must NOT be collapsed.
	job2, existing2, err := mgr.CreateJob(DownloadRequest{Repo: "dedup/routed", RouteKey: "audio"})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}
	if existing2 {
		t.Error("same repo routed to a different destination should create a new job")
	}
	if job1.ID == job2.ID {
		t.Error("routed-to-different-folder jobs should have different IDs")
	}

	// Same repo to the same destination is still deduped.
	job3, existing3, err := mgr.CreateJob(DownloadRequest{Repo: "dedup/routed", RouteKey: "llm/gguf"})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}
	if !existing3 || job3.ID != job1.ID {
		t.Errorf("same repo + same route should dedup; existing=%v ids %s/%s", existing3, job1.ID, job3.ID)
	}
}

func TestAPI_StartDownload_InvalidRouteKey(t *testing.T) {
	srv := newTestServer()

	// Both a traversal-looking value and a legacy/reserved key (which
	// /api/settings now drops) must be strict 400s on /api/download: a route
	// selector must not silently become a no-op.
	for _, key := range []string{"/etc/evil", "llm/gptq"} {
		body := `{"repo": "owner/model", "routeKey": "` + key + `"}`
		req := httptest.NewRequest("POST", "/api/download", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.handleStartDownload(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("routeKey %q: status = %d, want 400. body=%s", key, w.Code, w.Body.String())
		}
	}
}

func TestLocalCacheRoots_IncludesRoutes(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "hf")
	routeDir := filepath.Join(root, "LLM", "GGUF")

	repoDir := filepath.Join(routeDir, "owner", "model")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "model-Q4_K_M.gguf"), []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}

	routes := map[string]string{
		"llm/gguf": routeDir,
		"audio":    routeDir, // duplicate path, must dedup
	}
	roots := localCacheRoots(cacheDir, "", nil, routes)
	count := 0
	foundRoute := false
	for _, r := range roots {
		if strings.EqualFold(filepath.Clean(r.Path), filepath.Clean(routeDir)) {
			count++
			foundRoute = true
		}
	}
	if !foundRoute {
		t.Fatalf("route dir not present in roots: %#v", roots)
	}
	if count != 1 {
		t.Errorf("route dir appeared %d times, want 1 (deduped)", count)
	}

	repos, err := scanLocalCachedRepos(cacheDir, "", nil, routes, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := foundRepo(repos, "owner/model", "Local"); err != nil {
		t.Errorf("scanLocalCachedRepos did not see routed repo: %v", err)
	}

	info, err := findLocalCachedRepo(cacheDir, "", nil, routes, "owner/model", false)
	if err != nil {
		t.Fatalf("findLocalCachedRepo failed: %v", err)
	}
	if info.Source != "Local" {
		t.Errorf("Source = %q, want Local", info.Source)
	}
}

func foundRepo(repos []CachedRepoInfo, id, source string) error {
	for _, r := range repos {
		if r.Repo == id && r.Source == source {
			return nil
		}
	}
	return os.ErrNotExist
}

func TestAPI_DiskFree_AcceptsRoutePath(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	routeDir := filepath.Join(root, "LLM", "GGUF")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(routeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	srv := New(Config{
		CacheDir: cacheDir,
		DownloadRoutes: map[string]string{
			"llm/gguf": routeDir,
		},
	})

	req := httptest.NewRequest("GET", "/api/diskfree?path="+routeDir, nil)
	w := httptest.NewRecorder()
	srv.handleDiskFree(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("configured route path: status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if got := resp["path"]; got != routeDir {
		t.Errorf("path = %v, want %s", got, routeDir)
	}

	// Unconfigured path is still rejected.
	req = httptest.NewRequest("GET", "/api/diskfree?path="+filepath.Join(root, "nope"), nil)
	w = httptest.NewRecorder()
	srv.handleDiskFree(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unconfigured path: status = %d, want 400", w.Code)
	}
}

func TestAPI_Settings_DownloadRoutes_RoundTripJSON(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_ = os.Remove(ConfigPath())

	srv := newTestServer()

	body := `{"downloadRoutes": {"llm/gguf": "  /mnt/models/LLM/GGUF  ", "audio": "/mnt/models/Audio"}}`
	req := httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleUpdateSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}

	got := srv.config.DownloadRoutes
	if got["llm/gguf"] != "/mnt/models/LLM/GGUF" {
		t.Errorf("in-memory llm/gguf = %q, want trimmed+cleaned path", got["llm/gguf"])
	}

	fileCfg, err := LoadConfigFile()
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if fileCfg.DownloadRoutes["llm/gguf"] != "/mnt/models/LLM/GGUF" || fileCfg.DownloadRoutes["audio"] != "/mnt/models/Audio" {
		t.Errorf("persisted routes = %#v, want normalized values", fileCfg.DownloadRoutes)
	}

	// GET exposes the map back.
	req = httptest.NewRequest("GET", "/api/settings", nil)
	w = httptest.NewRecorder()
	srv.handleGetSettings(w, req)
	var resp SettingsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.DownloadRoutes["audio"] != "/mnt/models/Audio" {
		t.Errorf("GET downloadRoutes = %#v, want audio path", resp.DownloadRoutes)
	}
}

func TestAPI_Settings_DownloadRoutes_RoundTripYAML(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)

	// Force a YAML config file so ConfigPath() selects it and SaveConfigFile
	// writes YAML.
	cfgDir := AppConfigDir()
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(cfgDir, "hfdesk.yaml")
	if err := os.WriteFile(yamlPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if ConfigPath() != yamlPath {
		t.Skipf("ConfigPath resolved to %s, not the YAML file", ConfigPath())
	}

	srv := newTestServer()
	body := `{"downloadRoutes": {"embedding": "/mnt/models/Embedding"}}`
	req := httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleUpdateSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}

	fileCfg, err := LoadConfigFile()
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if fileCfg.DownloadRoutes["embedding"] != "/mnt/models/Embedding" {
		t.Errorf("YAML persisted routes = %#v, want embedding path", fileCfg.DownloadRoutes)
	}
}

func TestAPI_Settings_DropsUnknownRouteKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_ = os.Remove(ConfigPath())

	srv := newTestServer()

	// Mixed map: one valid key and one unknown (legacy/reserved) key. The save
	// must succeed and keep only the valid key; the unknown key must never be
	// stored or echoed back. This is the N1 regression: the UI echoes the
	// loaded map on save, so an unknown key must not fail the whole request.
	body := `{"connections": 16, "downloadRoutes": {"llm/gguf": "/mnt/models/GGUF", "llm/gptq": "/mnt/models/GPTQ"}}`
	req := httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleUpdateSettings(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := srv.config.DownloadRoutes["llm/gguf"]; got != "/mnt/models/GGUF" {
		t.Errorf("valid route = %q, want /mnt/models/GGUF (in-memory %#v)", got, srv.config.DownloadRoutes)
	}
	if _, ok := srv.config.DownloadRoutes["llm/gptq"]; ok {
		t.Errorf("unknown route key stored in memory: %#v", srv.config.DownloadRoutes)
	}
	// The valid fields in the same request are still applied (no partial-apply
	// failure, but also no silent drop of everything).
	if srv.config.Concurrency != 16 {
		t.Errorf("Concurrency = %d, want 16", srv.config.Concurrency)
	}

	// The persisted file must not contain the unknown key either.
	fileCfg, err := LoadConfigFile()
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if _, ok := fileCfg.DownloadRoutes["llm/gptq"]; ok {
		t.Errorf("unknown route key persisted: %#v", fileCfg.DownloadRoutes)
	}
	if fileCfg.DownloadRoutes["llm/gguf"] != "/mnt/models/GGUF" {
		t.Errorf("persisted routes = %#v, want llm/gguf kept", fileCfg.DownloadRoutes)
	}

	// GET must not advertise the unknown key.
	req = httptest.NewRequest("GET", "/api/settings", nil)
	w = httptest.NewRecorder()
	srv.handleGetSettings(w, req)
	var resp SettingsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.DownloadRoutes["llm/gptq"]; ok {
		t.Errorf("GET advertised unknown route key: %#v", resp.DownloadRoutes)
	}
	if resp.DownloadRoutes["llm/gguf"] != "/mnt/models/GGUF" {
		t.Errorf("GET downloadRoutes = %#v, want llm/gguf kept", resp.DownloadRoutes)
	}

	// A map containing only unknown keys sanitizes to no routes and still 200.
	body = `{"downloadRoutes": {"not-a-key": "/tmp/x"}}`
	req = httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	srv.handleUpdateSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("all-unknown map: status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(srv.config.DownloadRoutes) != 0 {
		t.Errorf("all-unknown map should clear routes, got %#v", srv.config.DownloadRoutes)
	}
}

// TestApplyConfigToServer_FiltersUnknownRouteKeys guards the config boundary:
// a hand-edited/legacy key in the file must be dropped on load so GET cannot
// advertise it and a later echo-save cannot fail.
func TestApplyConfigToServer_FiltersUnknownRouteKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfgDir := AppConfigDir()
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfgDir, "hfdesk.json")
	body := `{"download-routes": {"llm/gptq": "/mnt/GPTQ", " audio ": "/mnt/Audio", "llm/gguf": "  /mnt/GGUF  "}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if ConfigPath() != path {
		t.Skipf("ConfigPath resolved to %s, not the test config %s", ConfigPath(), path)
	}

	serverCfg := Config{}
	if err := ApplyConfigToServer(&serverCfg); err != nil {
		t.Fatalf("ApplyConfigToServer: %v", err)
	}
	if _, ok := serverCfg.DownloadRoutes["llm/gptq"]; ok {
		t.Errorf("reserved key survived config load: %#v", serverCfg.DownloadRoutes)
	}
	if _, ok := serverCfg.DownloadRoutes[" audio "]; ok {
		t.Errorf("unknown space-padded key survived config load: %#v", serverCfg.DownloadRoutes)
	}
	if got := serverCfg.DownloadRoutes["llm/gguf"]; got != "/mnt/GGUF" {
		t.Errorf("valid key = %q, want trimmed/cleaned /mnt/GGUF (%#v)", got, serverCfg.DownloadRoutes)
	}
}

// TestDownloadRoutes_EmptyPreservesBehavior is the regression guard: with no
// download-routes configured, CreateJob must follow LocalDir/HF-cache exactly
// as before.
func TestDownloadRoutes_EmptyPreservesBehavior(t *testing.T) {
	t.Run("LocalDir-only mode", func(t *testing.T) {
		localDir := filepath.Join(t.TempDir(), "models")
		mgr, cleanup := newRouteTestManager(t, Config{CacheDir: t.TempDir(), LocalDir: localDir})
		defer cleanup()

		job, _, err := mgr.CreateJob(DownloadRequest{Repo: "plain/model"})
		if err != nil {
			t.Fatalf("CreateJob failed: %v", err)
		}
		if job.LocalDir != localDir || !job.Flat {
			t.Errorf("LocalDir=%q Flat=%v, want %q/true", job.LocalDir, job.Flat, localDir)
		}
		if job.OutputDir != localDir {
			t.Errorf("OutputDir = %q, want %q", job.OutputDir, localDir)
		}
	})

	t.Run("HF-cache-only mode", func(t *testing.T) {
		cacheDir := t.TempDir()
		mgr, cleanup := newRouteTestManager(t, Config{CacheDir: cacheDir})
		defer cleanup()

		job, _, err := mgr.CreateJob(DownloadRequest{Repo: "plain/cache"})
		if err != nil {
			t.Fatalf("CreateJob failed: %v", err)
		}
		if job.LocalDir != "" || job.Flat {
			t.Errorf("LocalDir=%q Flat=%v, want empty/false", job.LocalDir, job.Flat)
		}
		if job.OutputDir != cacheDir {
			t.Errorf("OutputDir = %q, want %q", job.OutputDir, cacheDir)
		}
	})
}
