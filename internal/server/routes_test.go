// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// Node's built-in VM runs the shipped UI against the real HTTP handlers and
// CreateJob. Only DOM plumbing and disk capacity (for threshold cases) are
// simulated; selector mapping and destination resolution are production code.
func TestDownloadUIDestination(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: run testdata/download-ui.cjs with Node to verify the UI")
	}
	for _, mode := range []string{"parent", "fine", "local", "cache"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			cfg := Config{CacheDir: filepath.Join(root, "cache"), MaxActive: 1}
			if mode != "cache" {
				cfg.LocalDir = filepath.Join(root, "local")
			}
			if mode == "parent" || mode == "fine" {
				cfg.DownloadRoutes = map[string]string{"llm": filepath.Join(root, "llm"), "audio": filepath.Join(root, "audio")}
			}
			if mode == "fine" {
				cfg.DownloadRoutes["llm/gguf"] = filepath.Join(root, "gguf")
				cfg.DownloadRoutes["llm/safetensors"] = filepath.Join(root, "safetensors")
			}
			mgr := newTestJobManager(t, cfg, nil)
			mgr.jobs["occupied"] = &Job{Status: JobStatusRunning}
			srv := &Server{config: cfg, jobs: mgr}
			mux := http.NewServeMux()
			srv.registerAPIRoutes(mux)
			httpSrv := httptest.NewServer(mux)
			defer httpSrv.Close()
			fixture, err := json.Marshal(map[string]any{"url": httpSrv.URL, "routes": cfg.DownloadRoutes, "local": cfg.LocalDir, "cache": cfg.CacheDir, "manual": filepath.Join(root, "manual")})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(node, "testdata/download-ui.cjs")
			cmd.Env = append(os.Environ(), "HFDESK_UI_FIXTURE="+string(fixture))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("UI regression: %v\n%s", err, out)
			}
			t.Log(string(out))
		})
	}
}

// Check the public disk preview against real job creation, not a second resolver
// in the test. A full scheduler slot keeps these jobs queued and off the network.
func TestDownloadDiskFreeDestination(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	local := filepath.Join(root, "local")
	parent := filepath.Join(root, "llm")
	fine := filepath.Join(root, "gguf")
	audio := filepath.Join(root, "audio")
	explicit := filepath.Join(root, "manual")
	for _, dir := range []string{cache, local, parent, fine, audio, explicit} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name string
		cfg  Config
		req  DownloadRequest
		want string
	}{
		{"explicit override", Config{CacheDir: cache, LocalDir: local, DownloadRoutes: map[string]string{"llm/gguf": fine}}, DownloadRequest{RouteKey: "llm/gguf", LocalDir: explicit}, explicit},
		{"fine wins", Config{CacheDir: cache, LocalDir: local, DownloadRoutes: map[string]string{"llm": parent, "llm/gguf": fine}}, DownloadRequest{RouteKey: "llm/gguf"}, fine},
		{"parent gguf", Config{CacheDir: cache, LocalDir: local, DownloadRoutes: map[string]string{"llm": parent}}, DownloadRequest{RouteKey: "llm/gguf"}, parent},
		{"parent safetensors", Config{CacheDir: cache, LocalDir: local, DownloadRoutes: map[string]string{"llm": parent}}, DownloadRequest{RouteKey: "llm/safetensors"}, parent},
		{"audio", Config{CacheDir: cache, DownloadRoutes: map[string]string{"audio": audio}}, DownloadRequest{RouteKey: "audio"}, audio},
		{"global local", Config{CacheDir: cache, LocalDir: local}, DownloadRequest{RouteKey: "llm/gguf"}, local},
		{"cache", Config{CacheDir: cache}, DownloadRequest{RouteKey: "llm/gguf"}, cache},
		{"default cache", Config{}, DownloadRequest{}, hfdownloader.DefaultCacheDir()},
		{"dataset bypass", Config{CacheDir: cache, LocalDir: local, DownloadRoutes: map[string]string{"llm/gguf": fine}}, DownloadRequest{Dataset: true, RouteKey: "unknown"}, local},
		{"dataset explicit", Config{CacheDir: cache, LocalDir: local}, DownloadRequest{Dataset: true, LocalDir: explicit}, explicit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cfg.MaxActive = 1
			mgr := newTestJobManager(t, tt.cfg, nil)
			mgr.jobs["occupied"] = &Job{Status: JobStatusRunning}
			srv := &Server{config: tt.cfg, jobs: mgr}
			tt.req.Repo = "owner/model"
			body, err := json.Marshal(tt.req)
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			srv.registerAPIRoutes(mux)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/diskfree", bytes.NewReader(body)))
			if w.Code != http.StatusOK {
				t.Fatalf("disk preview: %d %s", w.Code, w.Body.String())
			}
			var preview struct {
				Path        string
				Free, Total uint64
			}
			if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
				t.Fatal(err)
			}
			job, _, err := mgr.CreateJob(tt.req)
			if err != nil {
				t.Fatal(err)
			}
			if preview.Path != tt.want || job.OutputDir != preview.Path {
				t.Fatalf("preview=%q job=%q want=%q", preview.Path, job.OutputDir, tt.want)
			}
			free, total, err := diskFreeBytes(tt.want)
			if err != nil {
				t.Fatal(err)
			}
			if preview.Total != total || preview.Free > total || free > total {
				t.Fatalf("invalid disk statistics: %+v", preview)
			}
			// A settings replacement cannot move the destination of an existing job.
			mgr.UpdateConfig(Config{LocalDir: explicit, MaxActive: 1})
			frozen, _ := mgr.GetJob(job.ID)
			if frozen.OutputDir != tt.want {
				t.Fatalf("job moved after settings change: %+v", frozen)
			}
		})
	}
}

func TestDownloadDiskFreeValidation(t *testing.T) {
	srv := &Server{config: Config{CacheDir: t.TempDir()}}
	for _, body := range []string{`{`, `{"routeKey":"unknown"}`, `{"routeKey":"unknown","localDir":"/manual"}`} {
		w := httptest.NewRecorder()
		srv.handleDownloadDiskFree(w, httptest.NewRequest("POST", "/api/diskfree", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	// GET's arbitrary-path restriction must not be widened by the new preview.
	w := httptest.NewRecorder()
	srv.handleDiskFree(w, httptest.NewRequest("GET", "/api/diskfree?path=/not-configured", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET restriction: %d %s", w.Code, w.Body.String())
	}
}

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
	// /models/Audio appears twice after cleaning.
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
	mgr := newTestJobManager(t, cfg, hub)
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
	srv := newTestServer(t)

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

func TestConfiguredPathIdentity(t *testing.T) {
	upper := filepath.Join(t.TempDir(), "Audio")
	lower := filepath.Join(filepath.Dir(upper), "audio")
	want := []string{upper, lower}
	if runtime.GOOS == "windows" {
		want = []string{upper}
	}
	paths := []string{"  " + upper + "  ", upper + string(filepath.Separator), lower, " "}
	if got := cleanPathList(paths); !reflect.DeepEqual(got, want) {
		t.Errorf("cleanPathList = %v, want %v", got, want)
	}
	routes := map[string]string{"audio": upper, "embedding": lower, "llm": upper + string(filepath.Separator)}
	if got := routeDirs(routes); !reflect.DeepEqual(got, want) {
		t.Errorf("routeDirs = %v, want %v", got, want)
	}
	roots := localCacheRoots(filepath.Join(filepath.Dir(upper), "cache"), upper, cleanPathList(paths), routes)
	var got []string
	for _, root := range roots {
		if root.Path == upper || root.Path == lower {
			got = append(got, root.Path)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("localCacheRoots = %v, want %v", got, want)
	}
	// Both configured spellings must be allowed, even when Windows dedups them.
	srv := &Server{config: Config{DownloadRoutes: routes}}
	for _, dir := range []string{upper, lower} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		srv.handleDiskFree(w, httptest.NewRequest("GET", "/api/diskfree?path="+dir, nil))
		if w.Code != http.StatusOK {
			t.Errorf("diskfree %s: %d %s", dir, w.Code, w.Body.String())
		}
	}
	// Cache special directories are skipped only when the local root is the
	// cache itself, not a distinct Linux directory with case-only differences.
	for _, localDir := range []string{lower, upper} {
		wantSkip := localDir == lower || runtime.GOOS == "windows"
		for _, root := range localCacheRoots(lower, localDir, nil, nil) {
			if root.Path == localDir && root.SkipSpecial != wantSkip {
				t.Errorf("local %s SkipSpecial = %v, want %v", localDir, root.SkipSpecial, wantSkip)
			}
		}
	}
}

func TestCaseDistinctRouteDestinations(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux case-sensitive directories")
	}
	root := t.TempDir()
	routes := map[string]string{"audio": filepath.Join(root, "Audio"), "embedding": filepath.Join(root, "audio")}
	cacheDir := filepath.Join(root, "cache")
	srv := &Server{config: Config{CacheDir: cacheDir, DownloadRoutes: routes}}
	for key, dir := range routes {
		repoDir := filepath.Join(dir, "owner", key)
		if err := os.MkdirAll(repoDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repoDir, "model.safetensors"), []byte("weights"), 0o644); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		srv.handleDiskFree(w, httptest.NewRequest("GET", "/api/diskfree?path="+dir, nil))
		if w.Code != http.StatusOK {
			t.Errorf("diskfree %s: %d %s", dir, w.Code, w.Body.String())
		}
	}
	repos, err := scanLocalCachedRepos(cacheDir, "", nil, routes, false)
	if err != nil {
		t.Fatal(err)
	}
	for key := range routes {
		id := "owner/" + key
		if err := foundRepo(repos, id, "Local"); err != nil {
			t.Errorf("scanner omitted %s", id)
		}
		if _, err := findLocalCachedRepo(cacheDir, "", nil, routes, id, false); err != nil {
			t.Errorf("lookup omitted %s: %v", id, err)
		}
	}
}

func TestAPI_RouteKeyValidationIngress(t *testing.T) {
	var calls atomic.Int64
	var datasetCalls atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasPrefix(r.URL.Path, "/api/datasets/") {
			datasetCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			fmt.Fprint(w, `{"sha":"deadbeef"}`)
		case strings.Contains(r.URL.Path, "/tree/"):
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected HF request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()
	cfg := Config{CacheDir: t.TempDir(), Endpoint: endpoint.URL, DownloadRoutes: map[string]string{"audio": t.TempDir()}}
	srv := &Server{config: cfg, jobs: newTestJobManager(t, cfg, NewWSHub())}
	mux := http.NewServeMux()
	srv.registerAPIRoutes(mux)
	for _, ingress := range []struct {
		name, path string
		dryRun     bool
	}{
		{"download", "/api/download", false},
		{"dry-run", "/api/download", true},
		{"plan", "/api/plan", false},
	} {
		for _, tc := range []struct {
			key     string
			dataset bool
			want    int
		}{
			{"/etc/evil", false, http.StatusBadRequest},
			{"llm/gptq", false, http.StatusBadRequest},
			{"audio", false, http.StatusOK},
			{"embedding", false, http.StatusOK},
			{"", false, http.StatusOK},
			{"llm/gptq", true, http.StatusOK},
			{"audio", true, http.StatusOK},
			{"", true, http.StatusOK},
		} {
			// Successful real job creation is covered by the existing manager tests.
			if ingress.name == "download" && tc.want != http.StatusBadRequest {
				continue
			}
			t.Run(ingress.name+"/"+tc.key+fmt.Sprint(tc.dataset), func(t *testing.T) {
				body, err := json.Marshal(DownloadRequest{Repo: "owner/model", RouteKey: tc.key, Dataset: tc.dataset, DryRun: ingress.dryRun})
				if err != nil {
					t.Fatal(err)
				}
				before, beforeDataset := calls.Load(), datasetCalls.Load()
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, httptest.NewRequest("POST", ingress.path, bytes.NewReader(body)))
				if w.Code != tc.want {
					t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
				}
				if tc.want == http.StatusBadRequest {
					if calls.Load() != before {
						t.Error("invalid selector attempted network")
					}
				} else {
					if calls.Load() == before {
						t.Error("preview did not reach HF scanner")
					}
					if tc.dataset && datasetCalls.Load() == beforeDataset {
						t.Error("dataset preview did not use dataset API")
					}
				}
				if len(srv.jobs.ListJobs()) != 0 {
					t.Error("preview or invalid request created a job")
				}
			})
		}
	}
}

func TestLocalCacheRoots_MergedRestrictions(t *testing.T) {
	// Each bit registers the raw cache through another configuration path.
	for mask := 0; mask < 8; mask++ {
		for _, friendlyOverlap := range []bool{false, true} {
			t.Run(fmt.Sprintf("overlap-%d/friendly-%v", mask, friendlyOverlap), func(t *testing.T) {
				cacheDir := filepath.Join(t.TempDir(), "cache")
				friendlyDir := filepath.Join(cacheDir, "models")
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
				want := []localCacheRoot{
					{Path: friendlyDir, Source: "Friendly view"},
					{Path: cacheDir, Source: "Local", SkipSpecial: true},
				}
				if got := localCacheRoots(cacheDir, localDir, scanDirs, routes); !reflect.DeepEqual(got, want) {
					t.Errorf("roots = %#v, want %#v", got, want)
				}
			})
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

	srv := newTestServerWithConfig(t, Config{
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

	srv := newTestServer(t)

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

	srv := newTestServer(t)
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

	srv := newTestServer(t)

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

// The downloader-side destination resolution used by runJob: for cache mode
// hfdownloader.Run derives <CacheDir>/hub/models--<repo>/blobs from
// settings.CacheDir, and for flat mode from
// settings.OutputDir/<LocalRepo-or-repo>/... Each destinationFreeze test
// below asserts the actual settings.CacheDir/OutputDir handed to
// hfdownloader.Run — observed by inspecting which cache tree gained the
// repo's EnsureDirs layout — not merely the Job.OutputDir field.

// --- Destination freeze (PR66 follow-up) ---

// stallGuard backs stalledEndpointManager: a local HTTP endpoint that
// hangs on every request until released or the test ends, plus the
// wait/capture helpers the freeze tests need.
type stallGuard struct {
	endpoint  *httptest.Server
	blocked   chan struct{}
	closeOnce sync.Once
}

func newStallGuard() *stallGuard {
	g := &stallGuard{blocked: make(chan struct{})}
	g.endpoint = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.closeOnce.Do(func() { close(g.blocked) })
		<-r.Context().Done()
	}))
	return g
}

func (g *stallGuard) close() {
	g.endpoint.Close()
}

// awaitBlocked waits until at least one downloader request has reached the
// stall endpoint, proving the run actually entered hfdownloader.Run with
// the settings under test (rather than failing before network setup).
func (g *stallGuard) awaitBlocked() bool {
	select {
	case <-g.blocked:
		return true
	case <-time.After(10 * time.Second):
		return false
	}
}

// release unblocks the stalled request so the run can advance to
// termination (the stalling endpoint then serves 404-style failures
// from the closed listener).
func (g *stallGuard) release() {
	g.endpoint.Close()
}

// jobSettingsCacheDir re-derives the settings.CacheDir runJob would hand
// to hfdownloader.Run for the given manager/job, following the
// production freeze contract: frozen job.OutputDir for cache mode, frozen
// job.LocalDir for flat mode, and the configured/default cache only for
// legacy jobs whose OutputDir was empty at restore time. The freeze tests
// call this on a live job and assert against the actual destination tree
// that gains the repo's blobs/snapshots layout, so a field-vs-settings
// divergence can't pass unnoticed.
func jobSettingsCacheDir(mgr *JobManager, job *Job) string {
	cfg := mgr.config
	if job.LocalDir != "" {
		return job.LocalDir // flat mode; settings.OutputDir
	}
	if job.OutputDir != "" {
		return job.OutputDir
	}
	if cfg.CacheDir != "" {
		return cfg.CacheDir
	}
	return hfdownloader.DefaultCacheDir()
}

// stalledEndpointManager builds a JobManager whose downloader contacts a
// locally stalling HTTP endpoint, plus the guard used to observe and
// release the in-flight run.
func stalledEndpointManager(t *testing.T, cfg Config) (*JobManager, *stallGuard) {
	t.Helper()
	guard := newStallGuard()
	cfg.Endpoint = guard.endpoint.URL
	if cfg.MaxActive == 0 {
		cfg.MaxActive = 1
	}
	hub := NewWSHub()
	go hub.Run()
	mgr := newTestJobManager(t, cfg, hub)
	t.Cleanup(func() {
		for _, j := range mgr.ListJobs() {
			mgr.CancelJob(j.ID)
		}
		if !mgr.WaitAll(10 * time.Second) {
			t.Error("runJob goroutines still running after WaitAll timeout")
		}
		guard.close()
	})
	return mgr, guard
}

func runAndWaitQueued(t *testing.T, mgr *JobManager, req DownloadRequest) *Job {
	t.Helper()
	job, existing, err := mgr.CreateJob(req)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if existing {
		t.Fatal("test precondition: job unexpectedly deduped")
	}
	if job.Status != JobStatusQueued {
		t.Fatalf("job status = %s, want queued", job.Status)
	}
	return job
}

// waitJobStatus polls until the job reaches want or the timeout elapses.
func waitJobStatus(t *testing.T, mgr *JobManager, id string, want JobStatus, timeout time.Duration) *Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if job, ok := mgr.GetJob(id); ok {
			if job.Status == want {
				return job
			}
			if job.Status == JobStatusCompleted || job.Status == JobStatusFailed || job.Status == JobStatusCancelled {
				return job
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, _ := mgr.GetJob(id)
	if job == nil {
		t.Fatalf("job %s disappeared", id)
	}
	t.Fatalf("job %s did not reach %s in time; status=%s err=%q", id, want, job.Status, job.Error)
	return nil
}

// cacheHubLayoutDir returns the per-repo hub layout path the downloader
// creates via EnsureDirs when given the settings.CacheDir root: assert
// on this tree actually appearing under a specific root to prove which
// root the run used.
func cacheHubLayoutDir(cacheDir, repo string) string {
	return filepath.Join(cacheDir, "hub", "models--"+strings.ReplaceAll(repo, "/", "--"))
}

// TestJobDestFreeze_CacheDirChange creates a queued cache-mode job, changes
// the server cache root in settings, then resumes it: the run must write
// into the frozen destination A (asserted via the hub layout created under
// A and the settings CacheDir derivation), not the new B.
func TestJobDestFreeze_CacheDirChange(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "cacheA")
	dirB := filepath.Join(root, "cacheB")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr, guard := stalledEndpointManager(t, Config{CacheDir: dirA, MaxActive: 1})

	// Occupy the only slot so the created job stays queued across the
	// settings change (the queued-execution case).
	fill := stalledSlotJob(t, mgr)
	job := runAndWaitQueued(t, mgr, DownloadRequest{Repo: "freeze/cache"})

	// Settings change while the job is queued: CacheDir A->B. UpdateConfig
	// replaces the whole manager Config, so every field the downloader needs
	// (Endpoint) must be carried over — as the settings handler does by
	// building the new config inside withConfig on the current values. The
	// queued job then moves through the paused state exactly as a restored
	// job would (LoadState clamps queued/running to paused) and resumes.
	mgr.UpdateConfig(Config{CacheDir: dirB, MaxActive: 1, Endpoint: guard.endpoint.URL})
	mgr.mu.Lock()
	if svc := mgr.jobs[job.ID]; svc != nil && svc.Status == JobStatusQueued {
		svc.Status = JobStatusPaused
	}
	mgr.mu.Unlock()
	if !mgr.ResumeJob(job.ID) {
		t.Fatal("ResumeJob failed")
	}
	// Free the slot and dispatch the resumed job.
	mgr.CancelJob(fill.ID)
	mgr.UpdateConfig(Config{CacheDir: dirB, MaxActive: 1, Endpoint: guard.endpoint.URL})

	// The resumed run must contact the stall endpoint using settings
	// derived from the frozen OutputDir (dirA), not the new dirB.
	if !guard.awaitBlocked() {
		t.Fatal("resumed job never reached hfdownloader.Run")
	}
	// Give the downloader a moment to perform EnsureDirs under the
	// destination, then stop the run by cancelling (the stall endpoint
	// never returns data).
	time.Sleep(50 * time.Millisecond)

	used, err := observedLayoutRoot(cacheHubLayoutDir(dirA, "freeze/cache"), cacheHubLayoutDir(dirB, "freeze/cache"))
	if err != nil {
		t.Fatal(err)
	}
	if used != cacheHubLayoutDir(dirA, "freeze/cache") {
		t.Errorf("run wrote into %s; frozen destination root %s was expected", used, dirA)
	}
	if got := jobSettingsCacheDir(mgr, mustGetJob(t, mgr, job.ID)); got != dirA {
		t.Errorf("settings.CacheDir derivation = %q, want frozen %q", got, dirA)
	}
	mgr.CancelJob(job.ID)
}

// observedLayoutRoot asserts that exactly one of the candidate repo hub
// layout dirs exists on disk and returns the winning root.
func observedLayoutRoot(candidates ...string) (string, error) {
	var found []string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("expected exactly one destination layout, found %v of %v", found, candidates)
	}
	return found[0], nil
}

func mustGetJob(t *testing.T, mgr *JobManager, id string) *Job {
	t.Helper()
	job, ok := mgr.GetJob(id)
	if !ok {
		t.Fatalf("job %s not found", id)
	}
	return job
}

// TestJobDestFreeze_QueuedStartsInA is the direct "queued created on A,
// settings change to B, executes" case: the queued job starts after the
// settings change and must download into the frozen A.
func TestJobDestFreeze_QueuedStartsInA(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "cacheA")
	dirB := filepath.Join(root, "cacheB")
	mgr, guard := stalledEndpointManager(t, Config{CacheDir: dirA, MaxActive: 1})

	// Fill the only active slot with a placeholder running job so the
	// real job stays queued while settings change.
	fill := stalledSlotJob(t, mgr)

	job := runAndWaitQueued(t, mgr, DownloadRequest{Repo: "freeze/queued"})
	mgr.UpdateConfig(Config{CacheDir: dirB, MaxActive: 1, Endpoint: guard.endpoint.URL})

	// Free the slot and let the scheduler dispatch: cancel removes the
	// filler, and the noop-cancel UpdateConfig re-runs dispatchLocked to
	// start the queued job, which must use the frozen dirA despite
	// cfg.CacheDir being dirB at start time.
	mgr.CancelJob(fill.ID)
	mgr.UpdateConfig(Config{CacheDir: dirB, MaxActive: 1, Endpoint: guard.endpoint.URL})
	waitJobStatus(t, mgr, job.ID, JobStatusRunning, 10*time.Second)
	if dbg, ok := mgr.GetJob(job.ID); ok {
		t.Logf("debug job status=%s phase=%s err=%q progress=%+v startedAt=%v", dbg.Status, dbg.Phase, dbg.Error, dbg.Progress, dbg.StartedAt != nil)
	}
	if !guard.awaitBlocked() {
		t.Fatal("started job never reached hfdownloader.Run")
	}
	time.Sleep(50 * time.Millisecond)

	used, err := observedLayoutRoot(cacheHubLayoutDir(dirA, "freeze/queued"), cacheHubLayoutDir(dirB, "freeze/queued"))
	if err != nil {
		t.Fatal(err)
	}
	if used != cacheHubLayoutDir(dirA, "freeze/queued") {
		t.Errorf("started job wrote into %s; frozen destination root %s was expected", used, dirA)
	}
	if got := jobSettingsCacheDir(mgr, mustGetJob(t, mgr, job.ID)); got != dirA {
		t.Errorf("settings.CacheDir derivation = %q, want frozen %q", got, dirA)
	}
	mgr.CancelJob(job.ID)
}

// stalledSlotJob registers a running placeholder job that keeps a
// scheduler slot busy without touching the network. Its cancel func is a
// no-op because there is no runJob goroutine behind it.
func stalledSlotJob(t *testing.T, mgr *JobManager) *Job {
	t.Helper()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	job := &Job{
		ID:             "slot-filler-" + generateID(),
		Repo:           "filler/slot",
		Status:         JobStatusRunning,
		CreatedAt:      time.Now(),
		cancel:         func() {},
		partialFilesMu: &sync.Mutex{},
	}
	mgr.jobs[job.ID] = job
	return job
}

// TestJobDestFreeze_ResumeStaysA covers paused -> resume after the cache
// root changed: a job paused mid-run must resume into its frozen root.
func TestJobDestFreeze_ResumeStaysA(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "cacheA")
	dirB := filepath.Join(root, "cacheB")
	mgr, guard := stalledEndpointManager(t, Config{CacheDir: dirA, MaxActive: 1})

	job, _, err := mgr.CreateJob(DownloadRequest{Repo: "freeze/resume"})
	if err != nil {
		t.Fatal(err)
	}
	// Let it reach the stalled request.
	if !guard.awaitBlocked() {
		t.Fatal("job never reached hfdownloader.Run")
	}
	if !mgr.PauseJob(job.ID) {
		t.Fatal("PauseJob failed")
	}
	waitJobStatus(t, mgr, job.ID, JobStatusPaused, 10*time.Second)

	// Settings change while paused.
	mgr.UpdateConfig(Config{CacheDir: dirB, MaxActive: 1, Endpoint: guard.endpoint.URL})

	if !mgr.ResumeJob(job.ID) {
		t.Fatal("ResumeJob failed")
	}
	waitJobStatus(t, mgr, job.ID, JobStatusRunning, 10*time.Second)
	time.Sleep(50 * time.Millisecond)

	used, err := observedLayoutRoot(cacheHubLayoutDir(dirA, "freeze/resume"), cacheHubLayoutDir(dirB, "freeze/resume"))
	if err != nil {
		t.Fatal(err)
	}
	if used != cacheHubLayoutDir(dirA, "freeze/resume") {
		t.Errorf("resumed run wrote into %s; frozen destination root %s was expected", used, dirA)
	}
	if got := jobSettingsCacheDir(mgr, mustGetJob(t, mgr, job.ID)); got != dirA {
		t.Errorf("settings.CacheDir derivation = %q, want frozen %q", got, dirA)
	}
	mgr.CancelJob(job.ID)
}

// TestJobDestFreeze_RetryStaysA covers failed -> retry after the cache root
// changed: the retried run must keep the frozen destination.
func TestJobDestFreeze_RetryStaysA(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "cacheA")
	dirB := filepath.Join(root, "cacheB")
	mgr, guard := stalledEndpointManager(t, Config{CacheDir: dirA, MaxActive: 1})

	job, _, err := mgr.CreateJob(DownloadRequest{Repo: "freeze/retry"})
	if err != nil {
		t.Fatal(err)
	}
	if !guard.awaitBlocked() {
		t.Fatal("job never reached hfdownloader.Run")
	}
	// Cancel while stalled: the run unwinds through the context path and
	// the job ends cancelled — a retryable state.
	if !mgr.CancelJob(job.ID) {
		t.Fatal("CancelJob failed")
	}
	waitJobStatus(t, mgr, job.ID, JobStatusCancelled, 30*time.Second)

	// Settings change while failed/cancelled, then retry.
	mgr.UpdateConfig(Config{CacheDir: dirB, MaxActive: 1, Endpoint: guard.endpoint.URL})

	if !mgr.RetryJob(job.ID) {
		t.Fatal("RetryJob failed")
	}
	waitJobStatus(t, mgr, job.ID, JobStatusRunning, 10*time.Second)
	time.Sleep(50 * time.Millisecond)

	used, err := observedLayoutRoot(cacheHubLayoutDir(dirA, "freeze/retry"), cacheHubLayoutDir(dirB, "freeze/retry"))
	if err != nil {
		t.Fatal(err)
	}
	if used != cacheHubLayoutDir(dirA, "freeze/retry") {
		t.Errorf("retried run wrote into %s; frozen destination root %s was expected", used, dirA)
	}
	if got := jobSettingsCacheDir(mgr, mustGetJob(t, mgr, job.ID)); got != dirA {
		t.Errorf("settings.CacheDir derivation = %q, want frozen %q", got, dirA)
	}
	mgr.CancelJob(job.ID)
}

// TestJobDestFreeze_LegacyRestoredJobRuns verifies the legacy persistence
// path: a jobs_state.json entry with an empty OutputDir (pre-OutputDir
// format) restores and resumes with the historical current-cache-root
// semantics — the run uses the configured cache root at resume time.
func TestJobDestFreeze_LegacyRestoredJobRuns(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "cacheA")
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	if AppConfigDir() != filepath.Join(cfgDir, "HFDesk") {
		t.Skipf("AppConfigDir resolved elsewhere: %s", AppConfigDir())
	}
	if err := os.MkdirAll(AppConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	state := `{"jobs":[{"id":"legacy1","repo":"freeze/legacy","revision":"main","outputDir":"","localDir":"","status":"paused"}]}`
	if err := os.WriteFile(filepath.Join(AppConfigDir(), "jobs_state.json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := newJobManagerWithStatePath(Config{CacheDir: dirA, MaxActive: 1, Endpoint: newStallGuardURL(t)}, NewWSHub(), filepath.Join(AppConfigDir(), "jobs_state.json"))
	registerTestJobManagerCleanup(t, mgr)
	mgr.LoadState()
	restored, ok := mgr.GetJob("legacy1")
	if !ok {
		t.Fatal("legacy job not restored")
	}
	if restored.Status != JobStatusPaused {
		t.Fatalf("restored status = %s, want paused", restored.Status)
	}
	if restored.OutputDir != "" {
		t.Fatalf("legacy OutputDir should reload empty, got %q", restored.OutputDir)
	}
	if !mgr.ResumeJob("legacy1") {
		t.Fatal("ResumeJob of legacy job failed")
	}
	job := waitJobStatus(t, mgr, "legacy1", JobStatusRunning, 10*time.Second)
	// Legacy job falls back to the configured current cache dir: the
	// settings derivation must be dirA, proving an empty frozen OutputDir
	// does not stall the run with an empty cache root.
	if got := jobSettingsCacheDir(mgr, job); got != dirA {
		t.Errorf("legacy settings.CacheDir derivation = %q, want configured %q", got, dirA)
	}
	mgr.CancelJob(job.ID)
}

// newStallGuardURL returns only the URL of a stall endpoint for callers
// that need it inline in a Config literal; the guard lives for the test.
func newStallGuardURL(t *testing.T) string {
	t.Helper()
	g := newStallGuard()
	t.Cleanup(g.close)
	return g.endpoint.URL
}

// --- CreateJob dedup destination identity (PR66 follow-up) ---

// TestCreateJob_DedupDestinationIdentity is the dedup regression suite for
// the frozen-destination identity: identical requests dedup, requests whose
// frozen destination differs do not, and LocalRepo/ExactMatch are part of
// the identity. All cases run through the real CreateJob dedup path.
func TestCreateJob_DedupDestinationIdentity(t *testing.T) {
	t.Run("same repo same CacheDir dedups", func(t *testing.T) {
		mgr, cleanup := newRouteTestManager(t, Config{CacheDir: t.TempDir()})
		defer cleanup()

		job1, existing1, err := mgr.CreateJob(DownloadRequest{Repo: "dedup/same"})
		if err != nil {
			t.Fatal(err)
		}
		if existing1 {
			t.Fatal("first job should not be existing")
		}
		job2, existing2, err := mgr.CreateJob(DownloadRequest{Repo: "dedup/same"})
		if err != nil {
			t.Fatal(err)
		}
		if !existing2 || job2.ID != job1.ID {
			t.Errorf("same repo + same cache root should dedup; existing=%v ids %s/%s", existing2, job1.ID, job2.ID)
		}
	})

	t.Run("same repo CacheDir A then B creates new job", func(t *testing.T) {
		root := t.TempDir()
		dirA := filepath.Join(root, "cacheA")
		dirB := filepath.Join(root, "cacheB")
		mgr, cleanup := newRouteTestManager(t, Config{CacheDir: dirA})
		defer cleanup()

		job1, _, err := mgr.CreateJob(DownloadRequest{Repo: "dedup/cacheab"})
		if err != nil {
			t.Fatal(err)
		}
		if job1.OutputDir != dirA {
			t.Fatalf("job1.OutputDir = %q, want %q", job1.OutputDir, dirA)
		}
		// Settings move the cache root to B, then re-request the repo.
		mgr.UpdateConfig(Config{CacheDir: dirB})
		job2, existing2, err := mgr.CreateJob(DownloadRequest{Repo: "dedup/cacheab"})
		if err != nil {
			t.Fatal(err)
		}
		if existing2 {
			t.Error("cache-mode request after CacheDir change must NOT dedup across roots")
		}
		if job2.ID == job1.ID {
			t.Error("different frozen destinations must have different job IDs")
		}
		if job2.OutputDir != dirB {
			t.Errorf("job2.OutputDir = %q, want new root %q", job2.OutputDir, dirB)
		}
	})

	t.Run("mmproj same filters different LocalRepo not deduped", func(t *testing.T) {
		mgr, cleanup := newRouteTestManager(t, Config{CacheDir: t.TempDir()})
		defer cleanup()

		// The documented upstream-mmproj flow (app.js downloadQuant):
		// repo = base model repo, localRepo = analyzed model's repo. Two
		// requests with equal filters/exactMatch but different LocalRepo
		// store into different cache folders and must not collapse.
		job1, _, err := mgr.CreateJob(DownloadRequest{
			Repo:       "base/model",
			LocalRepo:  "vendor/model-a",
			Filters:    []string{"mmproj-f16"},
			ExactMatch: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		job2, existing2, err := mgr.CreateJob(DownloadRequest{
			Repo:       "base/model",
			LocalRepo:  "vendor/model-b",
			Filters:    []string{"mmproj-f16"},
			ExactMatch: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if existing2 {
			t.Error("different LocalRepo (different destination folder) must not dedup")
		}
		if job2.ID == job1.ID {
			t.Error("different LocalRepo jobs should have different IDs")
		}
		if job2.LocalRepo != "vendor/model-b" {
			t.Errorf("job2.LocalRepo = %q, want vendor/model-b", job2.LocalRepo)
		}
	})

	t.Run("flat LocalDir equal LocalRepo different not deduped", func(t *testing.T) {
		localDir := filepath.Join(t.TempDir(), "models")
		mgr, cleanup := newRouteTestManager(t, Config{LocalDir: localDir})
		defer cleanup()

		// Flat mode: same effective LocalDir (resolved from the same global
		// LocalDir setting), different LocalRepo => different subfolder
		// destination => no dedup.
		job1, _, err := mgr.CreateJob(DownloadRequest{Repo: "flat/repo", LocalRepo: "one/model"})
		if err != nil {
			t.Fatal(err)
		}
		job2, existing2, err := mgr.CreateJob(DownloadRequest{Repo: "flat/repo", LocalRepo: "two/model"})
		if err != nil {
			t.Fatal(err)
		}
		if existing2 {
			t.Error("flat jobs with equal LocalDir but different LocalRepo must not dedup")
		}
		if job2.ID == job1.ID || job2.LocalRepo != "two/model" {
			t.Errorf("expected independent flat jobs, got ids %s/%s localRepo %q", job1.ID, job2.ID, job2.LocalRepo)
		}
	})

	t.Run("ExactMatch difference not deduped", func(t *testing.T) {
		mgr, cleanup := newRouteTestManager(t, Config{CacheDir: t.TempDir()})
		defer cleanup()

		// app.js downloadQuant sends exactMatch:true with the label-derived
		// filter; a manual/API request for the same repo without exact
		// matching selects a different file set (substring vs whole
		// segment) and is a different download.
		job1, _, err := mgr.CreateJob(DownloadRequest{Repo: "exact/test", Filters: []string{"q4_k_m"}, ExactMatch: true})
		if err != nil {
			t.Fatal(err)
		}
		job2, existing2, err := mgr.CreateJob(DownloadRequest{Repo: "exact/test", Filters: []string{"q4_k_m"}})
		if err != nil {
			t.Fatal(err)
		}
		if existing2 {
			t.Error("same filters with different ExactMatch must not dedup")
		}
		if job2.ID == job1.ID {
			t.Error("ExactMatch-differing jobs should have different IDs")
		}
	})

	t.Run("flat LocalDir equal everything still dedups", func(t *testing.T) {
		localDir := filepath.Join(t.TempDir(), "models")
		mgr, cleanup := newRouteTestManager(t, Config{LocalDir: localDir})
		defer cleanup()

		job1, _, err := mgr.CreateJob(DownloadRequest{Repo: "flat/same"})
		if err != nil {
			t.Fatal(err)
		}
		job2, existing2, err := mgr.CreateJob(DownloadRequest{Repo: "flat/same"})
		if err != nil {
			t.Fatal(err)
		}
		if !existing2 || job2.ID != job1.ID {
			t.Errorf("identical flat requests must dedup; existing=%v ids %s/%s", existing2, job1.ID, job2.ID)
		}
		if job2.LocalDir != localDir {
			t.Errorf("dedup returned LocalDir %q, want %q", job2.LocalDir, localDir)
		}
	})
}

// TestCreateJob_DedupDestinationIdentity_LocalDirOverride covers the flat
// route/local case the old LocalDir-only comparison missed: two requests
// with different explicit localDirs are handled, and when the second
// request's effective flat destination equals the first job's frozen
// OutputDir while its LocalRepo differs, no dedup happens.
func TestCreateJob_DedupDestinationIdentity_LocalDirOverride(t *testing.T) {
	root := t.TempDir()
	dirX := filepath.Join(root, "x")
	dirY := filepath.Join(root, "y")
	mgr, cleanup := newRouteTestManager(t, Config{CacheDir: t.TempDir()})
	defer cleanup()

	job1, _, err := mgr.CreateJob(DownloadRequest{Repo: "ovr/model", LocalDir: dirX})
	if err != nil {
		t.Fatal(err)
	}
	// Different explicit dir: new job (existing behavior preserved).
	job2, existing2, err := mgr.CreateJob(DownloadRequest{Repo: "ovr/model", LocalDir: dirY})
	if err != nil {
		t.Fatal(err)
	}
	if existing2 || job2.ID == job1.ID {
		t.Errorf("different explicit localDir must not dedup (existing=%v)", existing2)
	}
	// Same explicit dir again: dedup (existing behavior preserved).
	job3, existing3, err := mgr.CreateJob(DownloadRequest{Repo: "ovr/model", LocalDir: dirX})
	if err != nil {
		t.Fatal(err)
	}
	if !existing3 || job3.ID != job1.ID {
		t.Errorf("same explicit localDir should dedup; existing=%v ids %s/%s", existing3, job1.ID, job3.ID)
	}
	// Equal explicit dir but different LocalRepo: different destination.
	job4, existing4, err := mgr.CreateJob(DownloadRequest{Repo: "ovr/model", LocalDir: dirX, LocalRepo: "renamed/model"})
	if err != nil {
		t.Fatal(err)
	}
	if existing4 {
		t.Error("equal LocalDir with different LocalRepo must not dedup")
	}
	if job4.ID == job1.ID {
		t.Error("LocalRepo-split flat jobs should have different IDs")
	}
}

// --- PausedJob partial-cleanup destination freeze (e21c9c0 missed case) ---

// seedPausedFrozenCleanupJob inserts a paused cache-mode job whose frozen
// OutputDir points at rootDir and seeds real downloader partial artifacts
// plus the per-job in-flight dst tracker under that same root, mirroring the
// state cleanupPausedJobPartFiles sees for a paused job after its runJob
// goroutine has already exited. Each dst contributes the artifact family
// CleanupJobPartFiles removes (dst+".part", dst+".part-NN", dst+".parts.json").
func seedPausedFrozenCleanupJob(t *testing.T, mgr *JobManager, id, repo, rootDir string) (*Job, string, []string) {
	t.Helper()
	blobsDir := filepath.Join(cacheHubLayoutDir(rootDir, repo), "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst1 := filepath.Join(blobsDir, "tmp-deadbeef00000000")
	dst2 := filepath.Join(blobsDir, "tmp-cafebabe00000000")
	artifacts := []string{dst1 + ".part", dst2 + ".part-00", dst2 + ".parts.json"}
	for _, path := range artifacts {
		if err := os.WriteFile(path, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A "completed" blob that must survive any cleanup.
	if err := os.WriteFile(filepath.Join(blobsDir, "completed-blob"), []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := map[string]struct{}{dst1: {}, dst2: {}}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	job := &Job{
		ID:              id,
		Repo:            repo,
		Status:          JobStatusPaused,
		CreatedAt:       time.Now(),
		OutputDir:       rootDir,
		partialFilesPtr: &tracker,
		partialFilesMu:  &sync.Mutex{},
	}
	mgr.jobs[id] = job
	return job, blobsDir, artifacts
}

// TestJobDestFreeze_PausedCleanupTracksFrozenCacheRoot is the paused-cleanup
// companion of the runJob destination freeze: pause → cancel/dismiss must
// validate and remove the in-flight dsts under the cache root the job was
// frozen with (job.OutputDir), not the cache root currently configured in
// settings.
func TestJobDestFreeze_PausedCleanupTracksFrozenCacheRoot(t *testing.T) {
	checkFrozenCleanup := func(t *testing.T, mgr *JobManager, cacheDirB, blobsA string, artifacts []string, end func(id string) bool) {
		t.Helper()
		// Partials must exist under the frozen root before the settings move.
		for _, path := range artifacts {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("precondition: %s should exist: %v", path, err)
			}
		}
		if err := os.MkdirAll(filepath.Join(cacheHubLayoutDir(cacheDirB, "owner/pausedfreeze"), "blobs"), 0o755); err != nil {
			t.Fatal(err)
		}
		untouchedB := filepath.Join(cacheHubLayoutDir(cacheDirB, "owner/pausedfreeze"), "blobs", "tmp-underb.part")
		if err := os.WriteFile(untouchedB, []byte("untouched"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !end("") {
			t.Fatal("pausing end action should succeed")
		}
		for _, path := range artifacts {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("frozen-root partial %s was not cleaned up after settings moved the cache root (err=%v)", path, err)
			}
		}
		if _, err := os.Stat(filepath.Join(blobsA, "completed-blob")); err != nil {
			t.Errorf("completed blob should survive cleanup (err=%v)", err)
		}
		if _, err := os.Stat(untouchedB); err != nil {
			t.Errorf("file under settings cache root B must be untouched (err=%v)", err)
		}
	}

	t.Run("cancel targets frozen root after settings move", func(t *testing.T) {
		root := t.TempDir()
		dirA := filepath.Join(root, "cacheA")
		dirB := filepath.Join(root, "cacheB")
		mgr := newTestJobManager(t, Config{CacheDir: dirA, MaxActive: 1}, NewWSHub())

		job, blobsA, artifacts := seedPausedFrozenCleanupJob(t, mgr, "frozen-cancel", "owner/pausedfreeze", dirA)

		// Settings move the cache root to B while the job is paused.
		mgr.UpdateConfig(Config{CacheDir: dirB, MaxActive: 1})

		checkFrozenCleanup(t, mgr, dirB, blobsA, artifacts, func(id string) bool {
			if !mgr.CancelJob(job.ID) {
				return false
			}
			if j, _ := mgr.GetJob(job.ID); j.Status != JobStatusCancelled {
				t.Errorf("status = %s, want cancelled", j.Status)
			}
			return true
		})
	})

	t.Run("dismiss targets frozen root after settings move", func(t *testing.T) {
		root := t.TempDir()
		dirA := filepath.Join(root, "cacheA")
		dirB := filepath.Join(root, "cacheB")
		mgr := newTestJobManager(t, Config{CacheDir: dirA, MaxActive: 1}, NewWSHub())

		job, blobsA, artifacts := seedPausedFrozenCleanupJob(t, mgr, "frozen-dismiss", "owner/pausedfreeze", dirA)

		mgr.UpdateConfig(Config{CacheDir: dirB, MaxActive: 1})

		checkFrozenCleanup(t, mgr, dirB, blobsA, artifacts, func(id string) bool {
			res, _ := mgr.DismissJobResult(job.ID)
			return res == DismissJobOK
		})
	})
}

// TestJobDestFreeze_PausedCleanupLegacyAndFlatPreserved pins the preserved
// semantics around the paused-cleanup freeze: legacy restored jobs (empty
// OutputDir) keep cleaning under the current configured cache root — exactly
// runJob's legacy fallback — and flat jobs keep cleaning under the frozen
// job.LocalDir regardless of the settings CacheDir.
func TestJobDestFreeze_PausedCleanupLegacyAndFlatPreserved(t *testing.T) {
	t.Run("legacy empty OutputDir cleans current cache root", func(t *testing.T) {
		dirA := t.TempDir()
		mgr := newTestJobManager(t, Config{CacheDir: dirA, MaxActive: 1}, NewWSHub())

		blobsDir := filepath.Join(cacheHubLayoutDir(dirA, "owner/legacyclean"), "blobs")
		if err := os.MkdirAll(blobsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(blobsDir, "tmp-legacycurrent")
		if err := os.WriteFile(dst+".part", []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
		tracker := map[string]struct{}{dst: {}}
		mgr.mu.Lock()
		mgr.jobs["legacy-clean"] = &Job{
			ID:              "legacy-clean",
			Repo:            "owner/legacyclean",
			Status:          JobStatusPaused,
			CreatedAt:       time.Now(),
			OutputDir:       "", // legacy state file, pre-OutputDir format
			LocalDir:        "",
			partialFilesPtr: &tracker,
			partialFilesMu:  &sync.Mutex{},
		}
		mgr.mu.Unlock()

		if !mgr.CancelJob("legacy-clean") {
			t.Fatal("CancelJob should succeed for a legacy paused job")
		}
		if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
			t.Errorf("legacy paused job partial under current cache root should be cleaned up (err=%v)", err)
		}
	})

	t.Run("legacy cleanup follows settings-changed current root", func(t *testing.T) {
		root := t.TempDir()
		dirA := filepath.Join(root, "cacheA")
		dirB := filepath.Join(root, "cacheB")
		mgr := newTestJobManager(t, Config{CacheDir: dirA, MaxActive: 1}, NewWSHub())

		// A resumed legacy job runs under the current root: after the settings
		// move to B its dsts live under B, so cleanup must target B too.
		mgr.UpdateConfig(Config{CacheDir: dirB, MaxActive: 1})
		blobsDir := filepath.Join(cacheHubLayoutDir(dirB, "owner/legacymoved"), "blobs")
		if err := os.MkdirAll(blobsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(blobsDir, "tmp-legacymoved")
		if err := os.WriteFile(dst+".part", []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
		tracker := map[string]struct{}{dst: {}}
		mgr.mu.Lock()
		mgr.jobs["legacy-moved"] = &Job{
			ID:              "legacy-moved",
			Repo:            "owner/legacymoved",
			Status:          JobStatusPaused,
			CreatedAt:       time.Now(),
			OutputDir:       "", // legacy state file, pre-OutputDir format
			LocalDir:        "",
			partialFilesPtr: &tracker,
			partialFilesMu:  &sync.Mutex{},
		}
		mgr.mu.Unlock()

		if !mgr.CancelJob("legacy-moved") {
			t.Fatal("CancelJob should succeed for a legacy paused job")
		}
		if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
			t.Errorf("legacy paused job partial under settings-changed current root should be cleaned up (err=%v)", err)
		}
	})

	t.Run("flat cleanup keeps frozen LocalDir", func(t *testing.T) {
		root := t.TempDir()
		dirA := filepath.Join(root, "cacheA")
		dirB := filepath.Join(root, "cacheB")
		localDir := filepath.Join(root, "models")
		mgr := newTestJobManager(t, Config{CacheDir: dirA, MaxActive: 1}, NewWSHub())

		flatSub := filepath.Join(localDir, "owner", "flatclean")
		if err := os.MkdirAll(flatSub, 0o755); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(flatSub, "model.gguf")
		if err := os.WriteFile(dst+".part", []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
		tracker := map[string]struct{}{dst: {}}
		mgr.mu.Lock()
		mgr.jobs["flat-clean"] = &Job{
			ID:              "flat-clean",
			Repo:            "owner/flatclean",
			Status:          JobStatusPaused,
			CreatedAt:       time.Now(),
			OutputDir:       localDir, // flat jobs freeze OutputDir = LocalDir
			LocalDir:        localDir,
			Flat:            true,
			partialFilesPtr: &tracker,
			partialFilesMu:  &sync.Mutex{},
		}
		mgr.mu.Unlock()

		// Settings change must not reroute flat cleanup into the cache roots.
		mgr.UpdateConfig(Config{CacheDir: dirB, MaxActive: 1})

		if !mgr.CancelJob("flat-clean") {
			t.Fatal("CancelJob should succeed for a flat paused job")
		}
		if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
			t.Errorf("flat paused job partial under frozen LocalDir should be cleaned up (err=%v)", err)
		}
		for _, cacheDir := range []string{dirA, dirB} {
			if _, err := os.Stat(cacheHubLayoutDir(cacheDir, "owner/flatclean")); !os.IsNotExist(err) {
				t.Errorf("flat cleanup must not touch cache root %s (err=%v)", cacheDir, err)
			}
		}
	})
}
