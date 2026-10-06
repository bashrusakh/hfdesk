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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

func isolateCacheState(t *testing.T) string {
	t.Helper()
	r := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "AppData"} {
		t.Setenv(key, r)
	}
	t.Setenv("HF_HUB_CACHE", "")
	t.Setenv("HF_HOME", "")
	return r
}

func settingsFor(t *testing.T, s *Server) SettingsResponse {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleGetSettings(w, httptest.NewRequest("GET", "/api/settings", nil))
	var out SettingsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func updateCacheSettings(t *testing.T, s *Server, body string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleUpdateSettings(w, httptest.NewRequest("POST", "/api/settings", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", w.Code, w.Body.String())
	}
}

func TestCacheEnvironmentMatrix(t *testing.T) {
	for _, tc := range []struct {
		name                string
		home, hub, explicit bool
		namedHub            bool
		source              string
	}{
		{name: "default", source: "default"},
		{name: "home", home: true, source: "HF_HOME"},
		{name: "hub only", hub: true, source: "HF_HUB_CACHE"},
		{name: "both", home: true, hub: true, source: "HF_HUB_CACHE"},
		{name: "explicit", explicit: true, source: "cacheDir"},
		{name: "explicit home", home: true, explicit: true, source: "cacheDir"},
		{name: "explicit hub", explicit: true, hub: true, source: "HF_HUB_CACHE"},
		{name: "root named hub", explicit: true, namedHub: true, source: "cacheDir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := isolateCacheState(t)
			home, hub := filepath.Join(base, "hf-home"), filepath.Join(base, "arbitrary ' shared")
			if tc.home {
				t.Setenv("HF_HOME", home)
			}
			if tc.hub {
				t.Setenv("HF_HUB_CACHE", hub)
			}
			cfg := DefaultConfig()
			wantR := filepath.Join(base, ".cache", "huggingface")
			if tc.home {
				wantR = home
			}
			if tc.explicit {
				cfg.CacheDir = filepath.Join(base, "explicit")
				if tc.namedHub {
					cfg.CacheDir = filepath.Join(base, "hub")
				}
				wantR = cfg.CacheDir
			}
			wantH := filepath.Join(wantR, "hub")
			if tc.hub {
				wantH = hub
			}
			s := New(cfg)
			t.Setenv("HF_HOME", filepath.Join(base, "later-home"))
			t.Setenv("HF_HUB_CACHE", filepath.Join(base, "later-hub"))
			out := settingsFor(t, s)
			if out.CacheDir != wantR || out.ConfiguredCacheDir != cfg.CacheDir || out.EffectiveHubDir != wantH || out.HubDirSource != tc.source {
				t.Fatalf("settings=%+v want R=%s H=%s source=%s", out, wantR, wantH, tc.source)
			}
		})
	}
}

func TestCacheSettingsRawPreferenceAndRestart(t *testing.T) {
	base := isolateCacheState(t)
	r, h := filepath.Join(base, "home"), filepath.Join(base, "external")
	t.Setenv("HF_HOME", r)
	t.Setenv("HF_HUB_CACHE", h)
	s := New(DefaultConfig())
	out := settingsFor(t, s)
	if out.ConfiguredCacheDir != "" || out.CacheDir != r {
		t.Fatalf("initial=%+v", out)
	}
	// Supported form submits its raw preference. Effective fields are output-only.
	updateCacheSettings(t, s, `{"cacheDir":"","connections":2,"effectiveHubDir":"/ignored","configuredCacheDir":"/ignored","hubDirSource":"ignored"}`)
	file, err := LoadConfigFile()
	if err != nil || file.CacheDir != "" {
		t.Fatalf("persisted=%+v %v", file, err)
	}
	newR, newH := filepath.Join(base, "restart-home"), filepath.Join(base, "restart-hub")
	t.Setenv("HF_HOME", newR)
	t.Setenv("HF_HUB_CACHE", newH)
	cfg := DefaultConfig()
	ApplyConfigToServer(&cfg)
	restarted := New(cfg)
	out = settingsFor(t, restarted)
	if out.CacheDir != newR || out.EffectiveHubDir != newH || out.ConfiguredCacheDir != "" {
		t.Fatalf("restart=%+v", out)
	}
	// Same-path old clients explicitly write R; no equality heuristic discards it.
	data, _ := json.Marshal(map[string]string{"cacheDir": newR})
	updateCacheSettings(t, restarted, string(data))
	updateCacheSettings(t, restarted, `{"connections":3}`)
	file, err = LoadConfigFile()
	if err != nil || file.CacheDir != newR {
		t.Fatalf("explicit same-path write lost: %+v %v", file, err)
	}
	updateCacheSettings(t, restarted, `{"cacheDir":""}`)
	if out = settingsFor(t, restarted); out.ConfiguredCacheDir != "" || out.EffectiveHubDir != newH {
		t.Fatalf("clear=%+v", out)
	}
	// CLI/nonempty input still beats the selected config preference.
	if err := SaveConfigFile(&ConfigFile{CacheDir: filepath.Join(base, "file")}); err != nil {
		t.Fatal(err)
	}
	cfg = DefaultConfig()
	cfg.CacheDir = filepath.Join(base, "cli")
	ApplyConfigToServer(&cfg)
	if got := settingsFor(t, New(cfg)); got.CacheDir != cfg.CacheDir || got.EffectiveHubDir != newH {
		t.Fatalf("CLI=%+v", got)
	}
}

// This endpoint transfers real controlled bytes through the production downloader.
// first="pause" blocks the first transfer until cancellation; first="fail"
// fails just that run. Neither path mocks finalization or physical storage.
func cacheDownloadFixture(t *testing.T, first string) (*httptest.Server, []byte, string, <-chan struct{}) {
	t.Helper()
	body := []byte("controlled complete cache weights")
	hash := fmt.Sprintf("%x", sha256.Sum256(body))
	reached := make(chan struct{})
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "commit123"})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"type": "file", "path": "weights.safetensors", "size": len(body), "lfs": map[string]any{"oid": hash, "size": len(body)}}})
		case strings.Contains(r.URL.Path, "/resolve/"):
			if r.Method != "HEAD" && gets.Add(1) == 1 {
				close(reached)
				if first == "pause" {
					<-r.Context().Done()
					return
				}
				if first == "fail" {
					w.WriteHeader(500)
					return
				}
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Header().Set("ETag", hash)
			if r.Method != "HEAD" {
				_, _ = w.Write(body)
			}
		case r.URL.Path == "/api/models":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "owner/model"}, {"id": "owner/empty"}, {"id": "owner/partial"}, {"id": "owner/local"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, body, hash, reached
}

func assertCacheArtifacts(t *testing.T, r, h string, body []byte, hash string) {
	t.Helper()
	repo := filepath.Join(h, "models--owner--model")
	for _, p := range []string{filepath.Join(repo, "blobs", hash), filepath.Join(repo, "snapshots", "commit123", "weights.safetensors")} {
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("artifact %s=%q %v", p, got, err)
		}
	}
	ref, err := os.ReadFile(filepath.Join(repo, "refs", "main"))
	if err != nil || strings.TrimSpace(string(ref)) != "commit123" {
		t.Fatalf("ref=%q %v", ref, err)
	}
	if runtime.GOOS != "windows" {
		got, err := os.ReadFile(filepath.Join(r, "models", "owner", "model", "weights.safetensors"))
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("friendly content=%q %v", got, err)
		}
	}
	m, err := hfdownloader.ReadManifest(filepath.Join(r, "models", "owner", "model", hfdownloader.ManifestFilename))
	if err != nil || m.TotalFiles != 1 || m.TotalSize != int64(len(body)) || m.Commit != "commit123" {
		t.Fatalf("manifest=%+v %v", m, err)
	}
}

func TestFrozenHubJobCompletedTransitions(t *testing.T) {
	for _, transition := range []string{"queue", "pause", "requeue", "retry", "restart"} {
		t.Run(transition, func(t *testing.T) {
			base := isolateCacheState(t)
			a, b := filepath.Join(base, "friendly-a"), filepath.Join(base, "friendly-b")
			h1, h2 := filepath.Join(base, "hub-one-exact"), filepath.Join(base, "hub-two-exact")
			t.Setenv("HF_HUB_CACHE", h1)
			first := ""
			if transition == "pause" || transition == "requeue" {
				first = "pause"
			}
			if transition == "retry" {
				first = "fail"
			}
			ep, body, hash, reached := cacheDownloadFixture(t, first)
			cfg := Config{CacheDir: a, MaxActive: 1, Concurrency: 1, Verify: "sha256", Endpoint: ep.URL}
			if transition == "requeue" {
				cfg.MaxActive = 2
			}
			mgr := NewJobManager(cfg, nil)
			t.Cleanup(func() {
				for _, pending := range mgr.ListJobs() {
					mgr.CancelJob(pending.ID)
				}
				mgr.WaitAll(5 * time.Second)
			})
			if transition == "queue" || transition == "restart" || transition == "requeue" {
				older := time.Now().Add(-time.Hour)
				mgr.jobs["occupied"] = &Job{ID: "occupied", Status: JobStatusRunning, StartedAt: &older}
			}
			job, _, err := mgr.CreateJob(DownloadRequest{Repo: "owner/model"})
			if err != nil {
				t.Fatal(err)
			}
			if transition == "pause" || transition == "requeue" {
				select {
				case <-reached:
				case <-time.After(5 * time.Second):
					t.Fatal("transfer never reached fixture")
				}
				if transition == "pause" {
					if !mgr.PauseJob(job.ID) {
						t.Fatal("pause failed")
					}
				} else {
					cfg.MaxActive = 1
					mgr.UpdateConfig(cfg)
					if got, _ := mgr.GetJob(job.ID); got.Status != JobStatusQueued {
						t.Fatalf("limit did not requeue: %+v", got)
					}
				}
				mgr.WaitAll(5 * time.Second)
			}
			if transition == "retry" {
				waitJobStatus(t, mgr, job.ID, JobStatusFailed, 5*time.Second)
				mgr.WaitAll(5 * time.Second)
			}
			t.Setenv("HF_HUB_CACHE", h2)
			cfg.CacheDir = b
			if transition == "requeue" {
				cfg.MaxActive = 2
			}
			if transition == "restart" {
				if err := SaveJobsState([]*Job{job}); err != nil {
					t.Fatal(err)
				}
				mgr = NewJobManager(cfg, nil)
				mgr.LoadState()
				if got, _ := mgr.GetJob(job.ID); got.HubDir != h1 || got.OutputDir != a || got.DestinationWarning != "" {
					t.Fatalf("new-format restore=%+v", got)
				}
			} else {
				mgr.UpdateConfig(cfg)
			}
			switch transition {
			case "queue":
				mgr.mu.Lock()
				delete(mgr.jobs, "occupied")
				mgr.dispatchLocked()
				mgr.mu.Unlock()
			case "pause", "restart":
				if !mgr.ResumeJob(job.ID) {
					t.Fatal("resume failed")
				}
			case "retry":
				if !mgr.RetryJob(job.ID) {
					t.Fatal("retry failed")
				}
			}
			completed := waitJobStatus(t, mgr, job.ID, JobStatusCompleted, 5*time.Second)
			mgr.WaitAll(5 * time.Second)
			if completed.OutputDir != a || completed.HubDir != h1 {
				t.Fatalf("job moved=%+v", completed)
			}
			assertCacheArtifacts(t, a, h1, body, hash)
			for _, p := range []string{filepath.Join(a, "hub"), h2, b} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Fatalf("unintended artifacts at %s: %v", p, err)
				}
			}
		})
	}
}

func TestLegacyHubRestoreOnce(t *testing.T) {
	base := isolateCacheState(t)
	r, h := filepath.Join(base, "recorded"), filepath.Join(base, "startup-hub")
	t.Setenv("HF_HUB_CACHE", h)
	if err := SaveJobsState([]*Job{{ID: "legacy", Repo: "owner/model", OutputDir: r, Status: JobStatusPaused}, {ID: "empty", Repo: "owner/empty", Status: JobStatusPaused}}); err != nil {
		t.Fatal(err)
	}
	mgr := NewJobManager(Config{CacheDir: filepath.Join(base, "current")}, nil)
	mgr.LoadState()
	job, _ := mgr.GetJob("legacy")
	if job.OutputDir != r || job.HubDir != h || job.DestinationWarning == "" {
		t.Fatalf("legacy=%+v", job)
	}
	empty, _ := mgr.GetJob("empty")
	if empty.OutputDir != filepath.Join(base, "current") || empty.HubDir != h {
		t.Fatalf("legacy empty=%+v", empty)
	}
	t.Setenv("HF_HUB_CACHE", filepath.Join(base, "later"))
	mgr = NewJobManager(Config{CacheDir: filepath.Join(base, "later-root")}, nil)
	mgr.LoadState()
	job, _ = mgr.GetJob("legacy")
	if job.OutputDir != r || job.HubDir != h {
		t.Fatalf("legacy reinterpreted on second restore=%+v", job)
	}
}

func TestHubAssociationDedupAndCleanup(t *testing.T) {
	base := isolateCacheState(t)
	h := filepath.Join(base, "shared")
	t.Setenv("HF_HUB_CACHE", h)
	cfg := Config{CacheDir: filepath.Join(base, "a"), MaxActive: 1}
	mgr := NewJobManager(cfg, nil)
	mgr.jobs["occupied"] = &Job{Status: JobStatusRunning}
	one, _, err := mgr.CreateJob(DownloadRequest{Repo: "owner/model"})
	if err != nil {
		t.Fatal(err)
	}
	if same, dup, _ := mgr.CreateJob(DownloadRequest{Repo: "owner/model"}); !dup || same.ID != one.ID {
		t.Fatal("same association not deduplicated")
	}
	cfg.CacheDir = filepath.Join(base, "b")
	mgr.UpdateConfig(cfg)
	two, dup, err := mgr.CreateJob(DownloadRequest{Repo: "owner/model"})
	if err != nil || dup || two.ID == one.ID || two.HubDir != h {
		t.Fatalf("different R collapsed: %+v %v", two, err)
	}
	dst := filepath.Join(h, "models--owner--model", "blobs", "tmp-test")
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".part", []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.WriteFile(outside+".part", []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	j := mgr.jobs[one.ID]
	j.Status = JobStatusPaused
	j.partialFilesMu = &sync.Mutex{}
	tracker := map[string]struct{}{dst: {}, outside: {}}
	j.partialFilesPtr = &tracker
	mgr.mu.Unlock()
	t.Setenv("HF_HUB_CACHE", filepath.Join(base, "changed"))
	if !mgr.CancelJob(one.ID) {
		t.Fatal("cancel failed")
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Fatalf("frozen cleanup failed: %v", err)
	}
	if got, err := os.ReadFile(outside + ".part"); err != nil || string(got) != "keep" {
		t.Fatalf("cleanup escaped H: %q %v", got, err)
	}
}

func TestCacheDiskUsesExactHub(t *testing.T) {
	base := isolateCacheState(t)
	r, h := filepath.Join(base, "friendly"), filepath.Join(base, "external exact")
	t.Setenv("HF_HUB_CACHE", h)
	s := New(Config{CacheDir: r})
	for _, req := range []*http.Request{httptest.NewRequest("GET", "/api/diskfree", nil), httptest.NewRequest("GET", "/api/diskfree?path="+url.QueryEscape(r), nil), httptest.NewRequest("POST", "/api/diskfree", strings.NewReader(`{}`))} {
		w := httptest.NewRecorder()
		if req.Method == "GET" {
			s.handleDiskFree(w, req)
		} else {
			s.handleDownloadDiskFree(w, req)
		}
		var got struct{ Path, EffectiveHubDir, HubDirSource string }
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != 200 || got.Path != h || got.EffectiveHubDir != h || got.HubDirSource != "HF_HUB_CACHE" {
			t.Fatalf("disk=%d %s", w.Code, w.Body.String())
		}
	}
	if _, err := os.Stat(h); !os.IsNotExist(err) {
		t.Fatal("disk preview created Hub")
	}
	// An explicit configured local route can intentionally equal R. Preserve
	// that local-path browsing meaning; cache previews still select H.
	s.withConfig(func(c *Config) { c.DownloadRoutes = map[string]string{"llm": r} })
	for _, req := range []*http.Request{httptest.NewRequest("GET", "/api/diskfree?path="+url.QueryEscape(r), nil), httptest.NewRequest("POST", "/api/diskfree", strings.NewReader(`{"routeKey":"llm/gguf"}`))} {
		w := httptest.NewRecorder()
		if req.Method == "GET" {
			s.handleDiskFree(w, req)
		} else {
			s.handleDownloadDiskFree(w, req)
		}
		var got struct{ Path string }
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != 200 || got.Path != r {
			t.Fatalf("R-equal local route redirected to H: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestManifestAssociationUsesPhysicalEvidenceOnly(t *testing.T) {
	base := isolateCacheState(t)
	store := filepath.Join(base, "store")
	link := filepath.Join(base, "hub-link")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store, link); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	cache := hfdownloader.NewHFCacheResolved(filepath.Join(base, "app"), link, 0)
	repo, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(repo.SnapshotsDir(), "revision", "weights.bin")
	if err := os.MkdirAll(filepath.Dir(snapshot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	physicalRepo := filepath.Join(store, filepath.Base(repo.Path()))
	manifest := &hfdownloader.DownloadManifest{RepoPath: physicalRepo}
	if !manifestBelongsToRepo(cache, repo, manifest) {
		t.Fatal("manifest path naming the same existing repository object was not associated")
	}
	otherRepo := filepath.Join(base, "other", filepath.Base(repo.Path()))
	if err := os.MkdirAll(otherRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest.RepoPath = otherRepo
	if manifestBelongsToRepo(cache, repo, manifest) {
		t.Fatal("manifest path naming a different repository object was accepted")
	}
}

func cacheRequest(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func TestExactHubConsumersAndConfinement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlink/rebuild assertions")
	}
	base := isolateCacheState(t)
	r, h := filepath.Join(base, "friendly ' root"), filepath.Join(base, "separate ' storage")
	t.Setenv("HF_HUB_CACHE", h)
	ep, body, hash, _ := cacheDownloadFixture(t, "")
	s := New(Config{CacheDir: r, MaxActive: 1, Concurrency: 1, Verify: "sha256", Endpoint: ep.URL})
	j, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "owner/model"})
	if err != nil {
		t.Fatal(err)
	}
	waitJobStatus(t, s.jobs, j.ID, JobStatusCompleted, 5*time.Second)
	s.jobs.WaitAll(5 * time.Second)
	assertCacheArtifacts(t, r, h, body, hash)
	t.Setenv("HF_HUB_CACHE", filepath.Join(base, "changed"))
	// Current cache reads remain at startup H; raw R is not the storage claim.
	w := cacheRequest(t, s, "GET", "/api/cache", "")
	var list struct {
		Repos                                   []CachedRepoInfo
		CacheDir, EffectiveHubDir, HubDirSource string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || list.CacheDir != r || list.EffectiveHubDir != h || len(list.Repos) != 1 || list.Repos[0].Path != filepath.Join(h, "models--owner--model") || list.Repos[0].DownloadStatus != "complete" {
		t.Fatalf("cache list: %d %s", w.Code, w.Body.String())
	}
	w = cacheRequest(t, s, "GET", "/api/cache/owner/model", "")
	var info CachedRepoInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || info.Path != filepath.Join(h, "models--owner--model") || info.FileCount != 1 || info.DownloadStatus != "complete" {
		t.Fatalf("cache info: %d %s", w.Code, w.Body.String())
	}
	otherH := filepath.Join(base, "other-selected-hub")
	if err := copyRepoCache(filepath.Join(h, "models--owner--model"), h, otherH); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HF_HUB_CACHE", otherH)
	// A genuinely new startup config captures new ENV, rather than retaining
	// the old server's private snapshot as a caller-supplied startup value.
	restarted := New(Config{CacheDir: r, Endpoint: ep.URL})
	w = cacheRequest(t, restarted, "GET", "/api/cache/owner/model", "")
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || info.Path != filepath.Join(otherH, "models--owner--model") || info.DownloadStatus != "unknown" {
		t.Fatalf("stale friendly manifest mislabeled new H: %s", w.Body.String())
	}
	// Python-style Hub content remains visible without an HFDesk friendly view.
	if err := os.RemoveAll(filepath.Join(r, "models")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"owner/empty", "owner/partial"} {
		rd, err := s.snapshotConfig().cache().Repo(id, hfdownloader.RepoTypeModel)
		if err != nil {
			t.Fatal(err)
		}
		if err := rd.EnsureDirs(); err != nil {
			t.Fatal(err)
		}
		if id == "owner/partial" {
			if err := os.WriteFile(filepath.Join(rd.BlobsDir(), "tmp-file.part"), []byte("partial"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	local := filepath.Join(base, "local-scan")
	if err := os.MkdirAll(filepath.Join(local, "owner", "local"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "owner", "local", "weights.gguf"), []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	s.withConfig(func(c *Config) { c.LocalScanDirs = []string{local} })
	w = cacheRequest(t, s, "GET", "/api/search?q=owner", "")
	var search SearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &search); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(search.Results) != 4 {
		t.Fatalf("search: %d %s", w.Code, w.Body.String())
	}
	for _, hit := range search.Results {
		want := hit.ID == "owner/model" || hit.ID == "owner/local"
		if hit.Cached != want {
			t.Errorf("cached(%s)=%v want %v", hit.ID, hit.Cached, want)
		}
		if hit.ID == "owner/model" && (hit.CacheSource != "HF cache" || hit.CacheStatus != "unknown") {
			t.Errorf("Hub-only badge=%+v", hit)
		}
		if hit.ID == "owner/local" && hit.CacheSource != "Local" {
			t.Errorf("local source changed=%+v", hit)
		}
	}
	// Remote mirror is explicitly target/hub, independent of local ENV H.
	target := filepath.Join(base, "target")
	payload, _ := json.Marshal(map[string]any{"target": target, "dryRun": true, "verify": true})
	w = cacheRequest(t, s, "POST", "/api/mirror/push", string(payload))
	var mirror MirrorSyncResult
	if err := json.Unmarshal(w.Body.Bytes(), &mirror); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || !mirror.Success || mirror.Copied != 3 {
		t.Fatalf("mirror dry run: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("mirror dry run wrote target")
	}
	payload, _ = json.Marshal(map[string]any{"target": target, "verify": true})
	w = cacheRequest(t, s, "POST", "/api/mirror/push", string(payload))
	if err := json.Unmarshal(w.Body.Bytes(), &mirror); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || !mirror.Success || len(mirror.Errors) != 0 {
		t.Fatalf("mirror push: %d %s", w.Code, w.Body.String())
	}
	for _, p := range []string{filepath.Join(target, "hub", "models--owner--model", "blobs", hash), filepath.Join(target, "hub", "models--owner--model", "snapshots", "commit123", "weights.safetensors")} {
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("mirror artifact %s=%q %v", p, got, err)
		}
	}
	extra := filepath.Join(target, "hub", "models--owner--extra")
	if err := os.MkdirAll(extra, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(target, "keep-parent")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	payload, _ = json.Marshal(map[string]any{"target": target, "deleteExtra": true})
	w = cacheRequest(t, s, "POST", "/api/mirror/push", string(payload))
	if w.Code != 200 {
		t.Fatalf("mirror deleteExtra=%s", w.Body.String())
	}
	if _, err := os.Stat(extra); !os.IsNotExist(err) {
		t.Fatal("mirror did not delete selected extra repo")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal("mirror deleted target parent content")
	}
	// Pull maps target/hub back into selected H, not R/hub.
	if err := os.RemoveAll(filepath.Join(h, "models--owner--model")); err != nil {
		t.Fatal(err)
	}
	w = cacheRequest(t, s, "POST", "/api/mirror/pull", string(payload))
	if w.Code != 200 {
		t.Fatalf("mirror pull=%s", w.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(h, "models--owner--model", "blobs", hash)); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("pull content=%q %v", got, err)
	}
	w = cacheRequest(t, s, "POST", "/api/cache/rebuild", `{}`)
	if w.Code != 200 {
		t.Fatalf("rebuild: %d %s", w.Code, w.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(r, "models", "owner", "model", "weights.safetensors")); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("API rebuild content=%q %v", got, err)
	}
	script, err := os.ReadFile(filepath.Join(r, "rebuild.sh"))
	if err != nil || !strings.Contains(string(script), "HUB_DIR='") {
		t.Fatalf("selected-H rebuild script missing: %v", err)
	}
	// Refuse a repository link even though H itself is explicitly trusted.
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(h, "models--owner--evil")); err != nil {
		t.Fatal(err)
	}
	w = cacheRequest(t, s, "DELETE", "/api/cache/owner/evil", "")
	if w.Code != 400 {
		t.Fatalf("symlink delete allowed: %d %s", w.Code, w.Body.String())
	}
	w = cacheRequest(t, s, "DELETE", "/api/cache/owner/model", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatalf("external H delete rejected: %d %s", w.Code, w.Body.String())
	}
	for _, p := range []string{filepath.Join(h, "models--owner--model"), filepath.Join(r, "models", "owner", "model")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("delete left %s: %v", p, err)
		}
	}
	for _, p := range []string{outside, filepath.Join(h, "models--owner--partial"), target, r, h} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("delete escaped selected repo: %s %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(r, "hub")); !os.IsNotExist(err) {
		t.Fatal("consumer created unintended R/hub")
	}
}

func TestExactHubMirrorRejectsDestinationSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlinks")
	}
	base := isolateCacheState(t)
	src, dst, outside := filepath.Join(base, "source"), filepath.Join(base, "destination"), filepath.Join(base, "outside")
	repo := filepath.Join(src, "models--owner--model")
	if err := os.MkdirAll(filepath.Join(repo, "blobs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "blobs", "hash"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "models--owner--model"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "hash"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dst, "models--owner--model", "blobs")); err != nil {
		t.Fatal(err)
	}
	if err := copyRepoCache(repo, src, dst); err == nil {
		t.Fatal("copy followed destination symlink")
	}
	if err := verifyRepoCache(repo, src, dst); err == nil {
		t.Fatal("verification followed destination symlink")
	}
	if got, err := os.ReadFile(filepath.Join(outside, "hash")); err != nil || string(got) != "keep" {
		t.Fatalf("mirror escaped root: %q %v", got, err)
	}
}

func TestExactHubDeleteRetainsRootSymlinkGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlinks")
	}
	base := isolateCacheState(t)
	outside := filepath.Join(base, "outside")
	repo := filepath.Join(outside, "models--owner--model")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "selected-hub-link")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HF_HUB_CACHE", alias)
	s := New(Config{CacheDir: filepath.Join(base, "friendly")})
	w := cacheRequest(t, s, "DELETE", "/api/cache/owner/model", "")
	if w.Code != 400 {
		t.Fatalf("Hub link redefined trusted delete root: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(repo); err != nil {
		t.Fatalf("delete followed root link: %v", err)
	}
}

func TestWholeHubDeleteKeepsExternalModelAndDatasetSupport(t *testing.T) {
	for _, tc := range []struct {
		typeName string
		prefix   string
	}{
		{typeName: "model", prefix: "models--"},
		{typeName: "dataset", prefix: "datasets--"},
	} {
		t.Run(tc.typeName, func(t *testing.T) {
			base := isolateCacheState(t)
			friendly := filepath.Join(base, "friendly")
			externalHub := filepath.Join(base, "separate-hub")
			t.Setenv("HF_HUB_CACHE", externalHub)
			repo := filepath.Join(externalHub, tc.prefix+"owner--name")
			if err := os.MkdirAll(filepath.Join(repo, "snapshots", "revision"), 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(repo, "snapshots", "revision", "weights.bin")
			if err := os.WriteFile(marker, []byte("weights"), 0o644); err != nil {
				t.Fatal(err)
			}
			s := New(Config{CacheDir: friendly})
			requestPath := "/api/cache/owner/name"
			if tc.typeName == "dataset" {
				requestPath += "?type=dataset"
			}
			response := cacheRequest(t, s, "DELETE", requestPath, "")
			if response.Code != http.StatusOK {
				t.Fatalf("external %s delete status=%d body=%s", tc.typeName, response.Code, response.Body.String())
			}
			if _, err := os.Stat(repo); !os.IsNotExist(err) {
				t.Fatalf("external %s repository was not removed: %v", tc.typeName, err)
			}
		})
	}
}

func TestHubEnvironmentDoesNotRedirectLocalOrDataset(t *testing.T) {
	for _, mode := range []string{"explicit", "route", "global", "dataset"} {
		t.Run(mode, func(t *testing.T) {
			base := isolateCacheState(t)
			r, h, l := filepath.Join(base, "friendly"), filepath.Join(base, "exact-hub"), filepath.Join(base, "local")
			t.Setenv("HF_HUB_CACHE", h)
			ep, body, hash, _ := cacheDownloadFixture(t, "")
			cfg := Config{CacheDir: r, MaxActive: 1, Concurrency: 1, Verify: "sha256", Endpoint: ep.URL, DownloadRoutes: map[string]string{"llm": l}}
			req := DownloadRequest{Repo: "owner/model"}
			switch mode {
			case "explicit":
				req.LocalDir = l
			case "route":
				req.RouteKey = "llm/gguf"
			case "global":
				cfg.LocalDir = l
			case "dataset":
				req.Dataset = true
				req.RouteKey = "unknown"
			}
			mgr := NewJobManager(cfg, nil)
			j, _, err := mgr.CreateJob(req)
			if err != nil {
				t.Fatal(err)
			}
			waitJobStatus(t, mgr, j.ID, JobStatusCompleted, 5*time.Second)
			mgr.WaitAll(5 * time.Second)
			physical := filepath.Join(l, "owner", "model", "weights.safetensors")
			if mode == "dataset" {
				physical = filepath.Join(h, "datasets--owner--model", "snapshots", "commit123", "weights.safetensors")
			}
			if got, err := os.ReadFile(physical); err != nil || !bytes.Equal(got, body) {
				t.Fatalf("%s artifact=%q %v", mode, got, err)
			}
			if mode == "dataset" {
				if got, err := os.ReadFile(filepath.Join(h, "datasets--owner--model", "blobs", hash)); err != nil || !bytes.Equal(got, body) {
					t.Fatal("dataset blob incorrect")
				}
				if j.HubDir != h || j.LocalDir != "" {
					t.Fatalf("dataset routed: %+v", j)
				}
			} else {
				if j.HubDir != "" || !j.Flat {
					t.Fatalf("local job became cache-mode: %+v", j)
				}
				if _, err := os.Stat(h); !os.IsNotExist(err) {
					t.Fatal("local download touched H")
				}
			}
		})
	}
}

// TestSymlinkedHubContentDetection covers the accepted truthfulness invariant
// when the selected H root is itself a symlink to an external shared store
// (/mnt/hf-link -> /data/store): a genuinely complete repository reached through
// the link must be reported complete rather than hidden as unknown by a lexical
// containment check, while a repository that is itself a symlink out of the Hub
// must still not count as Hub content. Detection only; deletion/mirror
// confinement keeps its pre-existing fail-closed behavior.
func TestSymlinkedHubContentDetection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlinks")
	}
	base := isolateCacheState(t)
	r, store := filepath.Join(base, "friendly"), filepath.Join(base, "store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "hf-link")
	if err := os.Symlink(store, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ep, body, hash, _ := cacheDownloadFixture(t, "")
	t.Setenv("HF_HUB_CACHE", link)
	s := New(Config{CacheDir: r, MaxActive: 1, Concurrency: 1, Verify: "sha256", Endpoint: ep.URL})
	j, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "owner/model"})
	if err != nil {
		t.Fatal(err)
	}
	waitJobStatus(t, s.jobs, j.ID, JobStatusCompleted, 5*time.Second)
	s.jobs.WaitAll(5 * time.Second)
	// Real bytes land in the resolved store and are reachable through the link.
	assertCacheArtifacts(t, r, store, body, hash)
	hLinkRepo := filepath.Join(link, "models--owner--model")
	if got, err := os.ReadFile(filepath.Join(hLinkRepo, "blobs", hash)); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("link artifact=%q %v", got, err)
	}

	// A complete repository under the symlinked H must be detected as complete.
	rd, err := s.snapshotConfig().cache().Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if !hubRepoHasContent(rd) {
		t.Fatal("complete repo under symlinked H not detected")
	}
	w := cacheRequest(t, s, "GET", "/api/cache/owner/model", "")
	var info CachedRepoInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || info.Path != hLinkRepo || info.DownloadStatus != "complete" {
		t.Fatalf("symlinked-H info: %d %s", w.Code, w.Body.String())
	}
	w = cacheRequest(t, s, "GET", "/api/cache", "")
	var list struct {
		Repos           []CachedRepoInfo
		EffectiveHubDir string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(list.Repos) != 1 || list.Repos[0].DownloadStatus != "complete" || list.EffectiveHubDir != link {
		t.Fatalf("symlinked-H list: %d %s", w.Code, w.Body.String())
	}

	// Adversarial: a repo that is itself a symlink out of the Hub, even with a
	// resolvable snapshot file, is not inside the selected H.
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "snapshots", "commit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "snapshots", "commit", "weights.bin"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(store, "models--owner--evil")); err != nil {
		t.Fatal(err)
	}
	evil, err := s.snapshotConfig().cache().Repo("owner/evil", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if hubRepoHasContent(evil) {
		t.Fatal("repo symlinked out of Hub treated as inside")
	}
}

func TestFrozenRelativeDestinationsAcrossLaunchDirectories(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprint(local), func(t *testing.T) {
			base := isolateCacheState(t)
			oldCwd, newCwd := filepath.Join(base, "old-launch"), filepath.Join(base, "new-launch")
			for _, dir := range []string{oldCwd, newCwd} {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(oldCwd)
			t.Setenv("HF_HUB_CACHE", "exact-store")
			cfg := Config{CacheDir: "friendly", MaxActive: 1}
			if local {
				cfg.LocalDir = "local"
			}
			mgr := NewJobManager(cfg, nil)
			mgr.jobs["occupied"] = &Job{Status: JobStatusRunning}
			j, _, err := mgr.CreateJob(DownloadRequest{Repo: "owner/model"})
			if err != nil {
				t.Fatal(err)
			}
			wantR := filepath.Join(oldCwd, "friendly")
			if local {
				wantR = filepath.Join(oldCwd, "local")
			}
			if j.OutputDir != wantR || !filepath.IsAbs(j.OutputDir) {
				t.Fatalf("relative destination not frozen: %+v", j)
			}
			if !local && j.HubDir != filepath.Join(oldCwd, "exact-store") {
				t.Fatalf("relative H not frozen: %+v", j)
			}
			if err := SaveJobsState([]*Job{j}); err != nil {
				t.Fatal(err)
			}
			t.Chdir(newCwd)
			restored := NewJobManager(cfg, nil)
			restored.LoadState()
			got, _ := restored.GetJob(j.ID)
			if got.OutputDir != j.OutputDir || got.HubDir != j.HubDir || got.LocalDir != j.LocalDir {
				t.Fatalf("restart reinterpreted relative identity: %+v", got)
			}
		})
	}
}
