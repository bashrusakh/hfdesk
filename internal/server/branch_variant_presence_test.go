package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

func branchPresenceRequest(t *testing.T, s *Server, query string) (int, BranchVariantPresenceResponse) {
	t.Helper()
	w := cacheRequest(t, s, http.MethodGet, "/api/cache/owner/model?type=model&branchVariant=true&"+query, "")
	var response BranchVariantPresenceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode branch presence response (%d): %s: %v", w.Code, w.Body.String(), err)
	}
	return w.Code, response
}

func TestBranchVariantPresenceIsExactAndDurable(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	endpoint, _, _, _ := cacheDownloadFixture(t, "")
	cacheRoot := filepath.Join(base, "cache")
	cfg := Config{CacheDir: cacheRoot, Endpoint: endpoint.URL, MaxActive: 1, Concurrency: 1, Verify: "sha256"}
	s := newTestServerWithConfig(t, cfg)
	body := `{"repo":"owner/model","revision":"4.00bpw","branchVariant":true}`
	started := cacheRequest(t, s, http.MethodPost, "/api/download", body)
	if started.Code != http.StatusAccepted {
		t.Fatalf("branch download status=%d body=%s", started.Code, started.Body.String())
	}
	var job Job
	if err := json.Unmarshal(started.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Status != JobStatusQueued && job.Status != JobStatusRunning {
		t.Fatalf("branch download job=%+v", job)
	}
	if !s.jobs.WaitAll(10 * time.Second) {
		t.Fatal("branch download did not finish")
	}
	completed, ok := s.jobs.GetJob(job.ID)
	if !ok || completed.Status != JobStatusCompleted || !completed.BranchVariant {
		t.Fatalf("completed branch job=%+v, found=%t", completed, ok)
	}

	query := "revision=4.00bpw"
	status, got := branchPresenceRequest(t, s, query)
	if status != http.StatusOK || !got.Downloaded || got.DownloadStatus != "complete" || got.StorageMode != "hf-cache" || got.Commit != "commit123" || got.ExpectedFiles != 1 || got.ArtifactPath == "" {
		t.Fatalf("exact HF presence status=%d response=%+v", status, got)
	}
	if len(got.Files) != 1 || got.Files[0].Name != "weights.safetensors" {
		t.Fatalf("expected file set = %+v", got.Files)
	}

	if status, wrong := branchPresenceRequest(t, s, "revision=wrong-branch"); status != http.StatusOK || wrong.Downloaded || wrong.DownloadStatus != "unknown" {
		t.Fatalf("wrong revision presence status=%d response=%+v", status, wrong)
	}
	wrongRoot := filepath.Join(base, "other-local-root")
	if status, wrong := branchPresenceRequest(t, s, "revision=4.00bpw&localDir="+url.QueryEscape(wrongRoot)); status != http.StatusOK || wrong.Downloaded || wrong.StorageMode != "local" || wrong.DestinationRoot != wrongRoot {
		t.Fatalf("wrong destination presence status=%d response=%+v", status, wrong)
	}
	if status, explicit := branchPresenceRequest(t, s, "revision=4.00bpw&localDir="+url.QueryEscape(filepath.Join(cacheRoot, "models"))); status != http.StatusOK || explicit.Downloaded {
		t.Fatalf("explicit empty destination was treated as the HF root: status=%d response=%+v", status, explicit)
	}

	cache := cfg.cache()
	repo, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	metadataDir, err := repo.BranchVariantMetadataPath("4.00bpw", "commit123")
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(metadataDir, hfdownloader.ManifestFilename)
	manifest, err := hfdownloader.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Commit = "stale-commit"
	if _, err := manifest.WriteBranchVariant(metadataDir); err != nil {
		t.Fatal(err)
	}
	if status, stale := branchPresenceRequest(t, s, query); status != http.StatusOK || stale.Downloaded {
		t.Fatalf("stale manifest presence status=%d response=%+v", status, stale)
	}

	manifest.Commit = "commit123"
	if _, err := manifest.WriteBranchVariant(metadataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(got.ArtifactPath, "weights.safetensors")); err != nil {
		t.Fatal(err)
	}
	if status, missing := branchPresenceRequest(t, s, query); status != http.StatusOK || missing.Downloaded {
		t.Fatalf("missing expected member presence status=%d response=%+v", status, missing)
	}

	legacy := filepath.Join(cacheRoot, "models", "owner", "model", "weights.safetensors")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status, old := branchPresenceRequest(t, s, "revision=legacy"); status != http.StatusOK || old.Downloaded {
		t.Fatalf("legacy common-path data was attributed to a branch: status=%d response=%+v", status, old)
	}

	// Restarting the server does not depend on in-memory job state for proof.
	restarted := newTestServerWithConfig(t, cfg)
	if err := os.Remove(legacy); err != nil {
		t.Fatal(err)
	}
	// Restore the branch file after the deliberately missing-member check.
	if err := os.WriteFile(filepath.Join(got.ArtifactPath, "weights.safetensors"), []byte("controlled complete cache weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status, durable := branchPresenceRequest(t, restarted, query); status != http.StatusOK || !durable.Downloaded {
		t.Fatalf("restarted server lost durable branch proof: status=%d response=%+v", status, durable)
	}
}

func TestBranchVariantPresenceValidatesSelectorAndPreservesLegacyCacheResponse(t *testing.T) {
	s := newTestServer(t)
	for _, path := range []string{
		"/api/cache/owner/model?branchVariant=true",
		"/api/cache/owner/model?branchVariant=true&revision=main&revision=other",
		"/api/cache/owner/model?branchVariant=false&revision=main",
		"/api/cache/owner/model?type=dataset&branchVariant=true&revision=main",
		"/api/cache/owner/model?branchVariant=true&revision=main&routeKey=unknown",
		"/api/cache/owner/model?branchVariant=true&revision=main&routeKey=llm/safetensors",
	} {
		if w := cacheRequest(t, s, http.MethodGet, path, ""); w.Code != http.StatusBadRequest {
			t.Errorf("invalid selector %s status=%d body=%s", path, w.Code, w.Body.String())
		}
	}

	w := cacheRequest(t, s, http.MethodGet, "/api/cache/owner/model", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("legacy no-selector cache lookup changed: status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "branchVariant") {
		t.Fatalf("legacy cache response unexpectedly contains branch selector fields: %s", w.Body.String())
	}
}

func TestCopiedHFAndLocalRootsRetainBranchPresence(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	endpoint, body, _, _ := cacheDownloadFixture(t, "")
	defer endpoint.Close()
	sourceRoot := filepath.Join(base, "source-cache")
	copyRoot := filepath.Join(base, "copied-cache")
	cfg := Config{CacheDir: sourceRoot, Endpoint: endpoint.URL, MaxActive: 1, Concurrency: 1, Verify: "sha256"}
	s := newTestServerWithConfig(t, cfg)
	started := cacheRequest(t, s, http.MethodPost, "/api/download", `{"repo":"owner/model","revision":"4.00bpw","branchVariant":true}`)
	if started.Code != http.StatusAccepted {
		t.Fatalf("branch download status=%d body=%s", started.Code, started.Body.String())
	}
	var job Job
	if err := json.Unmarshal(started.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if !s.jobs.WaitAll(10 * time.Second) {
		t.Fatal("branch download did not finish before root copy")
	}
	if completed, ok := s.jobs.GetJob(job.ID); !ok || completed.Status != JobStatusCompleted {
		t.Fatalf("branch download before root copy=%+v found=%t", completed, ok)
	}
	if err := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: "main"}, hfdownloader.Settings{
		CacheDir: sourceRoot, Endpoint: endpoint.URL, Concurrency: 1, Verify: "sha256",
	}, nil); err != nil {
		t.Fatalf("ordinary download before root copy: %v", err)
	}
	if _, err := hfdownloader.NewHFCache(sourceRoot, 0).WriteRebuildScript(); err != nil {
		t.Fatalf("write source wrapper: %v", err)
	}
	sourceService, err := hfdownloader.BranchVariantServiceRoot(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	copyHFRootBytes(t, sourceRoot, copyRoot)
	copiedService, err := hfdownloader.BranchVariantServiceRoot(copyRoot)
	if err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(sourceService)
	if err != nil {
		t.Fatal(err)
	}
	copiedInfo, err := os.Stat(copiedService)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(sourceInfo, copiedInfo) {
		t.Fatal("copy fixture retained the source service-root object")
	}
	if err := os.RemoveAll(sourceRoot); err != nil {
		t.Fatalf("remove source root before presence query: %v", err)
	}
	copiedServer := newTestServerWithConfig(t, Config{CacheDir: copyRoot, Endpoint: endpoint.URL, MaxActive: 1, Concurrency: 1, Verify: "sha256"})
	status, before := branchPresenceRequest(t, copiedServer, "revision=4.00bpw")
	if status != http.StatusOK || !before.Downloaded || before.Commit != "commit123" {
		t.Fatalf("copied-root presence before rebuild status=%d response=%+v", status, before)
	}
	if _, err := hfdownloader.NewHFCache(copyRoot, 0).Sync(hfdownloader.SyncOptions{}); err != nil {
		t.Fatalf("Sync copied HF root after source removal: %v", err)
	}
	status, after := branchPresenceRequest(t, copiedServer, "revision=4.00bpw")
	if status != http.StatusOK || !after.Downloaded || after.Commit != before.Commit {
		t.Fatalf("copied-root presence after rebuild status=%d response=%+v", status, after)
	}
	ordinary, err := os.ReadFile(filepath.Join(copyRoot, "models", "owner", "model", "weights.safetensors"))
	if err != nil || !bytes.Equal(ordinary, body) {
		t.Fatalf("ordinary Hub view after copied-root rebuild=%q err=%v", ordinary, err)
	}

	localSource := filepath.Join(base, "local-source")
	localCopy := filepath.Join(base, "local-copy")
	if err := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, hfdownloader.Settings{
		OutputDir: localSource, Endpoint: endpoint.URL, Concurrency: 1, Verify: "sha256",
	}, nil); err != nil {
		t.Fatalf("download local branch artifact before root copy: %v", err)
	}
	localService, err := hfdownloader.BranchVariantServiceRoot(localSource)
	if err != nil {
		t.Fatal(err)
	}
	copyHFRootBytes(t, localSource, localCopy)
	copiedLocalService, err := hfdownloader.BranchVariantServiceRoot(localCopy)
	if err != nil {
		t.Fatal(err)
	}
	localInfo, err := os.Stat(localService)
	if err != nil {
		t.Fatal(err)
	}
	copiedLocalInfo, err := os.Stat(copiedLocalService)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(localInfo, copiedLocalInfo) {
		t.Fatal("local copy fixture retained the source service-root object")
	}
	if err := os.RemoveAll(localSource); err != nil {
		t.Fatalf("remove local source before presence query: %v", err)
	}
	localServer := newTestServerWithConfig(t, Config{CacheDir: copyRoot, Endpoint: endpoint.URL})
	status, localPresence := branchPresenceRequest(t, localServer, "revision=4.00bpw&localDir="+url.QueryEscape(localCopy))
	if status != http.StatusOK || !localPresence.Downloaded || localPresence.StorageMode != "local" || localPresence.DestinationRoot != localCopy {
		t.Fatalf("copied local-root presence status=%d response=%+v", status, localPresence)
	}
}

// TestCopiedLocalRootPresenceSurvivesIncompleteSiblingArtifact covers copy
// recognition per artifact at the server boundary. A copied store whose
// sibling artifact is left incomplete by a failed trusted-hash retry keeps its
// complete artifact: the presence query for that exact destination still
// reports it as downloaded, while the incomplete sibling stays unknown.
func TestCopiedLocalRootPresenceSurvivesIncompleteSiblingArtifact(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	sourceRoot := filepath.Join(base, "source-local")
	copiedRoot := filepath.Join(base, "copied-local")
	file := "weights.safetensors"
	revisions := map[string]coordinationFixtureRevision{
		"4_25":    {commit: "commit-a", expected: []byte("AAAAAAAA"), response: []byte("AAAAAAAA"), path: file},
		"5_00bpw": {commit: "commit-b", expected: []byte("BBBBBBBB"), response: []byte("CCCCCCCC"), path: file},
	}
	hub := newCoordinationFixtureHub(t, "owner/model", revisions, nil)
	defer hub.Close()

	if err := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: "4_25", BranchVariant: true}, hfdownloader.Settings{
		OutputDir: sourceRoot, Endpoint: hub.URL, Concurrency: 1, Verify: "sha256",
	}, nil); err != nil {
		t.Fatalf("download complete artifact A: %v", err)
	}
	failure := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: "5_00bpw", BranchVariant: true}, hfdownloader.Settings{
		OutputDir: sourceRoot, Endpoint: hub.URL, Concurrency: 1, Verify: "sha256",
	}, nil)
	if failure == nil || !strings.Contains(failure.Error(), "sha256 verify failed") {
		t.Fatalf("artifact B failure=%v; want trusted-hash failure", failure)
	}

	sourceService, err := hfdownloader.BranchVariantServiceRoot(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(sourceService)
	if err != nil {
		t.Fatal(err)
	}
	ownerPath := filepath.Join(sourceService, ".hfdesk-owner.json")
	ownerBefore, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	copyHFRootBytes(t, sourceRoot, copiedRoot)
	copiedService, err := hfdownloader.BranchVariantServiceRoot(copiedRoot)
	if err != nil {
		t.Fatal(err)
	}
	copiedInfo, err := os.Stat(copiedService)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(sourceInfo, copiedInfo) {
		t.Fatal("copy fixture retained the source service-root object")
	}
	if err := os.RemoveAll(sourceRoot); err != nil {
		t.Fatalf("remove source root before copied-root presence query: %v", err)
	}

	presenceServer := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(base, "unused-cache"), Endpoint: hub.URL})
	queryA := "revision=4_25&localDir=" + url.QueryEscape(copiedRoot)
	status, presenceA := branchPresenceRequest(t, presenceServer, queryA)
	if status != http.StatusOK || !presenceA.Downloaded || presenceA.DownloadStatus != "complete" {
		t.Fatalf("complete copied artifact A presence status=%d response=%+v", status, presenceA)
	}
	ownerReadOnly, err := os.ReadFile(filepath.Join(copiedService, ".hfdesk-owner.json"))
	if err != nil || !bytes.Equal(ownerReadOnly, ownerBefore) {
		t.Fatalf("read-only copied-root presence rewrote its owner record: %q err=%v", ownerReadOnly, err)
	}
	queryB := "revision=5_00bpw&localDir=" + url.QueryEscape(copiedRoot)
	status, presenceB := branchPresenceRequest(t, presenceServer, queryB)
	if status != http.StatusOK || presenceB.Downloaded {
		t.Fatalf("incomplete copied artifact B presence status=%d response=%+v", status, presenceB)
	}
}

func copyHFRootBytes(t *testing.T, source, destination string) {
	t.Helper()
	rootInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destination, rootInfo.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(destination, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected non-regular cache entry %q", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatalf("copy complete HF root: %v", err)
	}
}

func TestBranchVariantPresenceFailsClosedOnCacheReadError(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	cfg := Config{CacheDir: filepath.Join(base, "cache")}
	s := newTestServerWithConfig(t, cfg)
	repo, err := cfg.cache().Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	revisionBase, err := repo.BranchVariantMetadataRevisionPath("4.00bpw")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(revisionBase), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(revisionBase, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, response := branchPresenceRequest(t, s, "revision=4.00bpw")
	if status != http.StatusOK || response.Downloaded || response.DownloadStatus != "unknown" || response.Warning == "" {
		t.Fatalf("cache read error was not fail-closed: status=%d response=%+v", status, response)
	}
}

func TestBranchVariantPresenceUsesExactLocalDestinationAcrossRestart(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	endpoint, _, _, _ := cacheDownloadFixture(t, "")
	cacheRoot := filepath.Join(base, "cache")
	localRoot := filepath.Join(base, "models")
	cfg := Config{CacheDir: cacheRoot, LocalDir: localRoot, Endpoint: endpoint.URL}
	settings := hfdownloader.Settings{OutputDir: localRoot, Endpoint: endpoint.URL, Concurrency: 1, Verify: "sha256"}
	if err := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, settings, nil); err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithConfig(t, cfg)
	query := "revision=4.00bpw"
	status, got := branchPresenceRequest(t, s, query)
	if status != http.StatusOK || !got.Downloaded || got.StorageMode != "local" || got.DestinationRoot != localRoot || got.ArtifactPath == "" {
		t.Fatalf("exact local presence status=%d response=%+v", status, got)
	}
	if content, err := os.ReadFile(filepath.Join(got.ArtifactPath, "weights.safetensors")); err != nil || string(content) != "controlled complete cache weights" {
		t.Fatalf("local branch content=%q, %v", content, err)
	}
	wrongRoot := filepath.Join(base, "different-model-root")
	if status, wrong := branchPresenceRequest(t, s, query+"&localDir="+url.QueryEscape(wrongRoot)); status != http.StatusOK || wrong.Downloaded || wrong.DestinationRoot != wrongRoot {
		t.Fatalf("wrong local destination status=%d response=%+v", status, wrong)
	}
	restarted := newTestServerWithConfig(t, cfg)
	if status, durable := branchPresenceRequest(t, restarted, query); status != http.StatusOK || !durable.Downloaded || durable.ArtifactPath != got.ArtifactPath {
		t.Fatalf("local branch proof did not survive restart: status=%d response=%+v", status, durable)
	}
}

func TestFailedTrustedSHAStableRetryRevokesOldPresenceProof(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	root := filepath.Join(base, "local")
	expected := []byte("AAAAAAAA")
	digest := sha256.Sum256(expected)
	hash := hex.EncodeToString(digest[:])
	var bad atomic.Bool
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "commit-a"})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"type": "file", "path": "weights.safetensors", "size": len(expected), "lfs": map[string]any{"oid": hash, "size": len(expected)}}})
		default:
			w.Header().Set("Content-Length", fmt.Sprint(len(expected)))
			if r.Method != http.MethodHead {
				if bad.Load() {
					_, _ = w.Write([]byte("BBBBBBBB"))
				} else {
					_, _ = w.Write(expected)
				}
			}
		}
	}))
	defer hub.Close()

	job := hfdownloader.Job{Repo: "owner/model", Revision: "4_25", BranchVariant: true}
	settings := hfdownloader.Settings{OutputDir: root, Endpoint: hub.URL, Concurrency: 1, Verify: "sha256"}
	if err := hfdownloader.Download(context.Background(), job, settings, nil); err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(base, "cache"), LocalDir: root})
	_, before := branchPresenceRequest(t, s, "revision=4_25")
	if !before.Downloaded {
		t.Fatalf("fixture did not begin complete: %+v", before)
	}
	if err := os.Remove(filepath.Join(before.ArtifactPath, "weights.safetensors")); err != nil {
		t.Fatal(err)
	}
	_, missing := branchPresenceRequest(t, s, "revision=4_25")
	if missing.Downloaded {
		t.Fatalf("missing member remained Downloaded: %+v", missing)
	}

	bad.Store(true)
	err := hfdownloader.Download(context.Background(), job, settings, nil)
	if err == nil || !strings.Contains(err.Error(), "sha256 verify failed") {
		t.Fatalf("retry error=%v; want trusted SHA verification failure", err)
	}
	_, after := branchPresenceRequest(t, s, "revision=4_25")
	if after.Downloaded || after.DownloadStatus == "complete" {
		t.Fatalf("failed SHA retry left old proof authoritative: %+v", after)
	}
}

func TestCacheDeleteRefusesNestedForeignBranchServiceBeforeAnyRemoval(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	cacheRoot := filepath.Join(base, "cache")
	cache := hfdownloader.NewHFCache(cacheRoot, 0)
	ordinary, err := cache.Repo("ordinary/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := ordinary.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := ordinary.WriteRef("main", "ordinary-commit"); err != nil {
		t.Fatal(err)
	}
	ordinaryMarker := filepath.Join(ordinary.FriendlyPath(), "keep.txt")
	if err := os.MkdirAll(ordinary.FriendlyPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ordinaryMarker, []byte("ordinary"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifact, err := hfdownloader.ClaimBranchVariantArtifact(ordinary.FriendlyPath(), hfdownloader.RepoTypeModel, "other/model", "4_25", "commit-other")
	if err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(artifact.PayloadRoot, "weights.safetensors")
	if err := os.WriteFile(protected, []byte("AAAAAAAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	controlPath := filepath.Join(artifact.ControlRoot, ".hfdesk-owner.json")
	controlBefore, err := os.ReadFile(controlPath)
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServerWithConfig(t, Config{CacheDir: cacheRoot})
	response := cacheRequest(t, s, http.MethodDelete, "/api/cache/ordinary/model?type=model", "")
	if response.Code != http.StatusConflict {
		t.Fatalf("DELETE status=%d body=%s; want preflight conflict", response.Code, response.Body.String())
	}
	if _, err := os.Stat(ordinary.Path()); err != nil {
		t.Fatalf("Hub repo was partially deleted: %v", err)
	}
	if got, err := os.ReadFile(ordinaryMarker); err != nil || string(got) != "ordinary" {
		t.Fatalf("friendly ordinary data changed: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(protected); err != nil || string(got) != "AAAAAAAA" {
		t.Fatalf("foreign nested branch payload changed: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(controlPath); err != nil || !bytes.Equal(got, controlBefore) {
		t.Fatalf("foreign nested branch control changed: %q err=%v", got, err)
	}
}

func TestSyncCleanPreflightsNestedBranchServiceAndPreservesOrdinaryControl(t *testing.T) {
	root := t.TempDir()
	cache := hfdownloader.NewHFCache(root, 0)
	ordinary, err := cache.Repo("ordinary/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := ordinary.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	artifact, err := hfdownloader.ClaimBranchVariantArtifact(ordinary.FriendlyPath(), hfdownloader.RepoTypeModel, "other/model", "4_25", "commit-other")
	if err != nil {
		t.Fatal(err)
	}
	protectedLink := filepath.Join(artifact.PayloadRoot, "dangling.bin")
	if err := os.Symlink(filepath.Join(root, "missing-branch-target"), protectedLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	ordinaryLink := filepath.Join(cache.ModelsDir(), "unrelated", "model", "dangling.bin")
	if err := os.MkdirAll(filepath.Dir(ordinaryLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing-ordinary-target"), ordinaryLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := cache.Sync(hfdownloader.SyncOptions{Clean: true}); err == nil || !strings.Contains(err.Error(), "recursive mutation scope") {
		t.Fatalf("Sync(Clean:true) error=%v; want nested service-tree preflight conflict", err)
	}
	if _, err := os.Lstat(protectedLink); err != nil {
		t.Fatalf("protected dangling payload entry removed: %v", err)
	}
	if _, err := os.Lstat(ordinaryLink); err != nil {
		t.Fatalf("ordinary link changed before clean refusal: %v", err)
	}

	cleanRoot := t.TempDir()
	cleanCache := hfdownloader.NewHFCache(cleanRoot, 0)
	if err := os.MkdirAll(cleanCache.HubDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	ordinaryLink = filepath.Join(cleanCache.ModelsDir(), "ordinary", "model", "dangling.bin")
	if err := os.MkdirAll(filepath.Dir(ordinaryLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cleanRoot, "missing-ordinary-target"), ordinaryLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := cleanCache.Sync(hfdownloader.SyncOptions{Clean: true}); err != nil {
		t.Fatalf("ordinary clean control failed: %v", err)
	}
	if _, err := os.Lstat(ordinaryLink); !os.IsNotExist(err) {
		t.Fatalf("ordinary dangling link was not cleaned: %v", err)
	}
}

func TestBranchVariantPresenceUsesOnlyTheSelectedConfiguredRoute(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	endpoint, _, _, _ := cacheDownloadFixture(t, "")
	cacheRoot := filepath.Join(base, "cache")
	routeRoot := filepath.Join(base, "routes", "safetensors")
	cfg := Config{
		CacheDir: cacheRoot, Endpoint: endpoint.URL,
		DownloadRoutes: map[string]string{"llm/safetensors": routeRoot},
	}
	settings := hfdownloader.Settings{OutputDir: routeRoot, Endpoint: endpoint.URL, Concurrency: 1, Verify: "sha256"}
	if err := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, settings, nil); err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithConfig(t, cfg)
	if status, selected := branchPresenceRequest(t, s, "revision=4.00bpw&routeKey=llm%2Fsafetensors"); status != http.StatusOK || !selected.Downloaded || selected.StorageMode != "local" || selected.DestinationRoot != routeRoot {
		t.Fatalf("selected route presence status=%d response=%+v", status, selected)
	}
	if status, defaultRoot := branchPresenceRequest(t, s, "revision=4.00bpw"); status != http.StatusOK || defaultRoot.Downloaded || defaultRoot.StorageMode != "hf-cache" || defaultRoot.DestinationRoot != cacheRoot {
		t.Fatalf("omitted route silently used the routed destination: status=%d response=%+v", status, defaultRoot)
	}
}

func TestOrdinaryRoutedCollisionLeavesQualifiedBranchProofIntact(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	cacheRoot := filepath.Join(base, "cache")
	branchRoot := filepath.Join(base, "owner", "model")
	branchRevision := "4.00bpw"
	branchCommit := "commit-branch"
	branchBytes := []byte("AAAAAAAA")
	ordinaryBytes := []byte("BBBBBBBB")
	branchPayload, err := hfdownloader.BranchVariantPath(branchRoot, hfdownloader.RepoTypeModel, "owner/model", branchRevision, branchCommit)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryBase, err := hfdownloader.DestinationBase(base, "owner/model")
	if err != nil {
		t.Fatal(err)
	}
	ordinaryRelative, err := filepath.Rel(ordinaryBase, filepath.Join(branchPayload, "weights.safetensors"))
	if err != nil || filepath.IsAbs(ordinaryRelative) || ordinaryRelative == ".." || strings.HasPrefix(ordinaryRelative, ".."+string(filepath.Separator)) {
		t.Fatalf("invalid ordinary collision fixture path %q: %v", ordinaryRelative, err)
	}
	ordinaryRelative = filepath.ToSlash(ordinaryRelative)

	commits := map[string]map[string][]byte{
		branchCommit:  {"weights.safetensors": branchBytes},
		"commit-main": {ordinaryRelative: ordinaryBytes},
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const repoPrefix = "/api/models/owner/model/"
		if strings.HasPrefix(r.URL.Path, repoPrefix+"revision/") {
			revision := strings.TrimPrefix(r.URL.Path, repoPrefix+"revision/")
			commit := branchCommit
			if revision == "main" {
				commit = "commit-main"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": commit})
			return
		}
		if strings.HasPrefix(r.URL.Path, repoPrefix+"tree/") {
			revision := strings.TrimPrefix(r.URL.Path, repoPrefix+"tree/")
			commit := revision
			if revision == "main" {
				commit = "commit-main"
			}
			files, ok := commits[commit]
			if !ok {
				http.NotFound(w, r)
				return
			}
			nodes := make([]map[string]any, 0, len(files))
			for name, data := range files {
				nodes = append(nodes, map[string]any{"type": "file", "path": name, "size": len(data)})
			}
			_ = json.NewEncoder(w).Encode(nodes)
			return
		}
		for _, prefix := range []string{"/owner/model/resolve/", "/owner/model/raw/"} {
			if !strings.HasPrefix(r.URL.Path, prefix) {
				continue
			}
			rest := strings.TrimPrefix(r.URL.Path, prefix)
			commit, name, ok := strings.Cut(rest, "/")
			data, exists := commits[commit][name]
			if !ok || !exists {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(data)
			}
			return
		}
		http.NotFound(w, r)
	}))
	defer endpoint.Close()

	cfg := Config{
		CacheDir: cacheRoot, Endpoint: endpoint.URL, MaxActive: 1, Concurrency: 1, Verify: "size",
		DownloadRoutes: map[string]string{"llm/safetensors": branchRoot, "llm/gguf": base},
	}
	s := newTestServerWithConfig(t, cfg)
	started := cacheRequest(t, s, http.MethodPost, "/api/download", `{"repo":"owner/model","revision":"4.00bpw","branchVariant":true,"routeKey":"llm/safetensors"}`)
	if started.Code != http.StatusAccepted {
		t.Fatalf("branch route download status=%d body=%s", started.Code, started.Body.String())
	}
	var branchJob Job
	if err := json.Unmarshal(started.Body.Bytes(), &branchJob); err != nil {
		t.Fatal(err)
	}
	if !s.jobs.WaitAll(10 * time.Second) {
		t.Fatal("branch route download did not finish")
	}
	if job, ok := s.jobs.GetJob(branchJob.ID); !ok || job.Status != JobStatusCompleted {
		t.Fatalf("branch route job=%+v found=%t", job, ok)
	}
	controlDir, err := hfdownloader.BranchVariantMetadataPath(branchRoot, hfdownloader.RepoTypeModel, "owner/model", branchRevision, branchCommit)
	if err != nil {
		t.Fatal(err)
	}
	controlPath := filepath.Join(controlDir, hfdownloader.ManifestFilename)
	controlBefore, err := os.ReadFile(controlPath)
	if err != nil {
		t.Fatal(err)
	}

	ordinary := cacheRequest(t, s, http.MethodPost, "/api/download", `{"repo":"owner/model","revision":"main","routeKey":"llm/gguf"}`)
	if ordinary.Code != http.StatusAccepted {
		t.Fatalf("ordinary nested-route request status=%d body=%s", ordinary.Code, ordinary.Body.String())
	}
	var ordinaryJob Job
	if err := json.Unmarshal(ordinary.Body.Bytes(), &ordinaryJob); err != nil {
		t.Fatal(err)
	}
	if !s.jobs.WaitAll(10 * time.Second) {
		t.Fatal("ordinary conflict job did not finish")
	}
	failed, ok := s.jobs.GetJob(ordinaryJob.ID)
	if !ok || failed.Status != JobStatusFailed || !strings.Contains(failed.Error, "app-owned EXL branch variant service tree") {
		t.Fatalf("ordinary conflict job=%+v found=%t; want explicit failure", failed, ok)
	}
	if got, err := os.ReadFile(filepath.Join(branchPayload, "weights.safetensors")); err != nil || !bytes.Equal(got, branchBytes) {
		t.Fatalf("ordinary conflict changed routed branch payload: got=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(controlPath); err != nil || !bytes.Equal(got, controlBefore) {
		t.Fatalf("ordinary conflict changed routed control record: got=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(ordinaryBase, filepath.FromSlash(ordinaryRelative))); err != nil || !bytes.Equal(got, branchBytes) {
		t.Fatalf("ordinary conflicting projection changed service payload: got=%q err=%v", got, err)
	}
	if status, presence := branchPresenceRequest(t, s, "revision=4.00bpw&routeKey=llm%2Fsafetensors"); status != http.StatusOK || !presence.Downloaded || presence.ArtifactPath != branchPayload || presence.ControlPath != controlDir {
		t.Fatalf("qualified proof after ordinary conflict status=%d response=%+v", status, presence)
	}
}

func TestVariantPlanAdmissionRejectsOrdinaryServiceOverlapButKeepsVariantsConcurrent(t *testing.T) {
	root := t.TempDir()
	branchRoot := filepath.Join(root, "owner", "model")
	branchPayload, err := hfdownloader.BranchVariantPath(branchRoot, hfdownloader.RepoTypeModel, "owner/model", "4.00bpw", "commit-a")
	if err != nil {
		t.Fatal(err)
	}
	branchControl, err := hfdownloader.BranchVariantMetadataPath(branchRoot, hfdownloader.RepoTypeModel, "owner/model", "4.00bpw", "commit-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Dir(branchPayload), filepath.Dir(branchControl), filepath.Join(root, "owner", "model")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ordinaryRelative, err := filepath.Rel(filepath.Join(root, "owner", "model"), filepath.Join(branchPayload, "weights.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	branchJob := &Job{ID: "branch", Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true, Flat: true, OutputDir: branchRoot, LocalDir: branchRoot}
	ordinaryJob := &Job{ID: "ordinary", Repo: "owner/model", Revision: "main", Flat: true, OutputDir: root, LocalDir: root}
	branchPlan := hfdownloader.Plan{Commit: "commit-a", CommitResolved: true, Items: []hfdownloader.PlanItem{{RelativePath: "weights.safetensors"}}}
	ordinaryPlan := hfdownloader.Plan{Commit: "main-commit", Items: []hfdownloader.PlanItem{{RelativePath: filepath.ToSlash(ordinaryRelative)}}}

	{
		m := &JobManager{runActivities: map[uint64]*jobWriteActivity{
			1: {job: branchJob, descriptor: decisionJob(branchJob), planned: make(map[string]frozenMutationPath)},
		}}
		if err := m.admitPlanWrite(branchJob, 1, branchPlan); err != nil {
			t.Fatalf("branch-first admission: %v", err)
		}
		m.runActivities[2] = &jobWriteActivity{job: ordinaryJob, descriptor: decisionJob(ordinaryJob), planned: make(map[string]frozenMutationPath)}
		if err := m.admitPlanWrite(ordinaryJob, 2, ordinaryPlan); err == nil || !strings.Contains(err.Error(), "ordinary download plan conflicts") {
			t.Fatalf("ordinary concurrent plan error=%v; want service conflict", err)
		}
	}
	{
		m := &JobManager{runActivities: map[uint64]*jobWriteActivity{
			2: {job: ordinaryJob, descriptor: decisionJob(ordinaryJob), planned: make(map[string]frozenMutationPath)},
		}}
		if err := m.admitPlanWrite(ordinaryJob, 2, ordinaryPlan); err != nil {
			t.Fatalf("ordinary-first admission: %v", err)
		}
		m.runActivities[1] = &jobWriteActivity{job: branchJob, descriptor: decisionJob(branchJob), planned: make(map[string]frozenMutationPath)}
		if err := m.admitPlanWrite(branchJob, 1, branchPlan); err == nil || !strings.Contains(err.Error(), "ordinary download plan conflicts") {
			t.Fatalf("branch concurrent plan error=%v; want ordinary conflict", err)
		}
	}

	variantB, err := hfdownloader.BranchVariantPath(branchRoot, hfdownloader.RepoTypeModel, "owner/model", "4_25", "commit-b")
	if err != nil {
		t.Fatal(err)
	}
	variantBControl, err := hfdownloader.BranchVariantMetadataPath(branchRoot, hfdownloader.RepoTypeModel, "owner/model", "4_25", "commit-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(variantB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(variantBControl), 0o755); err != nil {
		t.Fatal(err)
	}
	secondBranch := &Job{ID: "branch-b", Repo: "owner/model", Revision: "4_25", BranchVariant: true, Flat: true, OutputDir: branchRoot, LocalDir: branchRoot}
	secondPlan := hfdownloader.Plan{Commit: "commit-b", CommitResolved: true, Items: []hfdownloader.PlanItem{{RelativePath: "weights.safetensors"}}}
	m := &JobManager{runActivities: map[uint64]*jobWriteActivity{
		1: {job: branchJob, descriptor: decisionJob(branchJob), planned: make(map[string]frozenMutationPath)},
		2: {job: secondBranch, descriptor: decisionJob(secondBranch), planned: make(map[string]frozenMutationPath)},
	}}
	if err := m.admitPlanWrite(branchJob, 1, branchPlan); err != nil {
		t.Fatalf("first variant admission: %v", err)
	}
	if err := m.admitPlanWrite(secondBranch, 2, secondPlan); err != nil {
		t.Fatalf("distinct branch variant should be admitted concurrently: %v", err)
	}
}

func TestWholeRepoDeleteRemovesOnlyOwnedBranchServiceArtifacts(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	endpoint, _, _, _ := cacheDownloadFixture(t, "")
	cacheRoot := filepath.Join(base, "cache")
	settings := hfdownloader.Settings{CacheDir: cacheRoot, Endpoint: endpoint.URL, Concurrency: 1, Verify: "sha256"}
	for _, revision := range []string{"4.00bpw", "4_25"} {
		if err := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: revision, BranchVariant: true}, settings, nil); err != nil {
			t.Fatalf("download owner/model %s: %v", revision, err)
		}
	}
	coordinationCarrier := hfdownloader.NewHFCache(cacheRoot, 0).RebuildCoordinationPath()
	carrierBefore, err := os.Stat(coordinationCarrier)
	if err != nil {
		t.Fatalf("stat service-root coordination owner before delete: %v", err)
	}
	carrierBytesBefore, err := os.ReadFile(coordinationCarrier)
	if err != nil {
		t.Fatal(err)
	}
	if err := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "other/model", Revision: "4.00bpw", BranchVariant: true}, settings, nil); err != nil {
		t.Fatalf("download other/model: %v", err)
	}
	ownerPayloadRepo, err := hfdownloader.BranchVariantPayloadRepoPath(cacheRoot, hfdownloader.RepoTypeModel, "owner/model")
	if err != nil {
		t.Fatal(err)
	}
	ownerControlRepo, err := hfdownloader.BranchVariantControlRepoPath(cacheRoot, hfdownloader.RepoTypeModel, "owner/model")
	if err != nil {
		t.Fatal(err)
	}
	activeBranch := &Job{ID: "active-branch", Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true, Flat: true, OutputDir: cacheRoot, LocalDir: cacheRoot, Status: JobStatusRunning}
	reservationManager := &JobManager{runActivities: map[uint64]*jobWriteActivity{
		1: {job: activeBranch, descriptor: decisionJob(activeBranch), planned: make(map[string]frozenMutationPath)},
	}}
	if release, ok := reservationManager.reserveSelectedHF(filepath.Join(cacheRoot, "hub", "models--owner--model"), filepath.Join(cacheRoot, "models", "owner", "model"), ownerControlRepo, ownerPayloadRepo); ok {
		release()
		t.Fatal("whole-repository reservation ignored an active branch service writer")
	}
	foreignPayload := filepath.Join(ownerPayloadRepo, "foreign-unowned-entry")
	foreignControl := filepath.Join(ownerControlRepo, "foreign-unowned-entry")
	for _, path := range []string{foreignPayload, foreignControl} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("preserve"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	otherArtifact, err := hfdownloader.BranchVariantPath(cacheRoot, hfdownloader.RepoTypeModel, "other/model", "4.00bpw", "commit123")
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServerWithConfig(t, Config{CacheDir: cacheRoot})
	deleted := cacheRequest(t, s, http.MethodDelete, "/api/cache/owner/model?type=model", "")
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"success":true`) {
		t.Fatalf("whole-repo delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	for _, revision := range []string{"4.00bpw", "4_25"} {
		artifact, err := hfdownloader.BranchVariantPath(cacheRoot, hfdownloader.RepoTypeModel, "owner/model", revision, "commit123")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(artifact); !os.IsNotExist(err) {
			t.Errorf("owned owner/model %s payload remains after whole-repo delete: %v", revision, err)
		}
	}
	for _, path := range []string{foreignPayload, foreignControl} {
		if got, err := os.ReadFile(path); err != nil || string(got) != "preserve" {
			t.Errorf("whole-repo delete changed unowned service entry %q: got=%q err=%v", path, got, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(otherArtifact, "weights.safetensors")); err != nil || !bytes.Equal(got, []byte("controlled complete cache weights")) {
		t.Errorf("whole-repo delete changed other/model service artifact: got=%q err=%v", got, err)
	}
	carrierAfter, err := os.Stat(coordinationCarrier)
	if err != nil || !os.SameFile(carrierBefore, carrierAfter) {
		t.Errorf("whole-repo delete removed/replaced the service-root coordination owner: before=%v after=%v err=%v", carrierBefore, carrierAfter, err)
	}
	if got, err := os.ReadFile(coordinationCarrier); err != nil || !bytes.Equal(got, carrierBytesBefore) {
		t.Errorf("whole-repo delete changed the service-root coordination owner: got=%q err=%v", got, err)
	}
}

func TestUnownedSameSpelledNestedDirectoryRemainsOrdinaryForSyncAndDelete(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	endpoint, body, hash, _ := cacheDownloadFixture(t, "")
	cacheRoot := filepath.Join(base, "cache")
	settings := hfdownloader.Settings{CacheDir: cacheRoot, Endpoint: endpoint.URL, Concurrency: 1, Verify: "sha256"}
	if err := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: "main"}, settings, nil); err != nil {
		t.Fatalf("download ordinary owner/model: %v", err)
	}
	cache := hfdownloader.NewHFCache(cacheRoot, 0)
	repo, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(repo.BlobPath(hash)); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("ordinary Hub payload fixture=%q err=%v", got, err)
	}
	unowned := filepath.Join(repo.FriendlyPath(), hfdownloader.BranchVariantServiceDirName)
	foreign := filepath.Join(unowned, "ordinary.txt")
	if err := os.MkdirAll(unowned, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("ordinary same-spelled directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(unowned, "dangling-link")
	orphanCreated := false
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing-target"), orphan); err == nil {
		orphanCreated = true
	} else {
		t.Logf("dangling symlink control unavailable: %v", err)
	}
	result, err := cache.Sync(hfdownloader.SyncOptions{Clean: true})
	if err != nil || result == nil || len(result.Errors) != 0 {
		t.Fatalf("Sync(Clean:true) rejected an unowned same-spelled directory: result=%+v err=%v", result, err)
	}
	if got, err := os.ReadFile(foreign); err != nil || string(got) != "ordinary same-spelled directory" {
		t.Fatalf("Sync(Clean:true) changed ordinary same-spelled data: %q err=%v", got, err)
	}
	if orphanCreated {
		if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
			t.Fatalf("clean did not retain ordinary dangling-link cleanup semantics: %v", err)
		}
	}

	s := newTestServerWithConfig(t, Config{CacheDir: cacheRoot, Endpoint: endpoint.URL})
	deleted := cacheRequest(t, s, http.MethodDelete, "/api/cache/owner/model?type=model", "")
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"success":true`) {
		t.Fatalf("whole-repo delete rejected an unowned same-spelled directory: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	for _, path := range []string{filepath.Join(repo.Path(), "refs", "main"), repo.FriendlyPath()} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("normal whole-repo delete retained %q: %v", path, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(repo.Path(), "blobs", hash)); !os.IsNotExist(err) {
		t.Errorf("ordinary Hub blob remained after selected repo DELETE: %v", err)
	}
}

func TestLocalCacheScanHidesOwnedServiceRootWithoutHidingSameSpelledOrdinaryFiles(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	localRoot := filepath.Join(base, "local")
	type localRevision struct {
		commit string
		files  map[string][]byte
	}
	revisions := map[string]localRevision{}
	// Reuse a compact fake Hub with a branch file plus an ordinary GGUF below a
	// same-spelled directory that is not the physical service root.
	branchRevision := localRevision{commit: "commit-branch", files: map[string][]byte{"weights.safetensors": []byte("AAAAAAAA")}}
	ordinaryPath := ".hfdesk-variant-store/model-Q4_K_M.gguf"
	ordinaryRevision := localRevision{commit: "commit-main", files: map[string][]byte{ordinaryPath: []byte("ordinary-gguf")}}
	revisions["4.00bpw"] = branchRevision
	revisions["main"] = ordinaryRevision
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/api/models/owner/model/"
		if strings.HasPrefix(r.URL.Path, prefix+"revision/") {
			revision := strings.TrimPrefix(r.URL.Path, prefix+"revision/")
			info, ok := revisions[revision]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": info.commit})
			return
		}
		if strings.HasPrefix(r.URL.Path, prefix+"tree/") {
			revision := strings.TrimPrefix(r.URL.Path, prefix+"tree/")
			info, ok := revisions[revision]
			if !ok {
				for _, candidate := range revisions {
					if candidate.commit == revision {
						info, ok = candidate, true
						break
					}
				}
			}
			if !ok {
				http.NotFound(w, r)
				return
			}
			nodes := make([]map[string]any, 0, len(info.files))
			for name, data := range info.files {
				nodes = append(nodes, map[string]any{"type": "file", "path": name, "size": len(data)})
			}
			_ = json.NewEncoder(w).Encode(nodes)
			return
		}
		for _, prefix := range []string{"/owner/model/resolve/", "/owner/model/raw/"} {
			if !strings.HasPrefix(r.URL.Path, prefix) {
				continue
			}
			rest := strings.TrimPrefix(r.URL.Path, prefix)
			commit, name, ok := strings.Cut(rest, "/")
			var data []byte
			if commit == "main" {
				data, ok = revisions["main"].files[name]
			} else {
				for _, info := range revisions {
					if info.commit == commit {
						data, ok = info.files[name]
						break
					}
				}
			}
			if !ok || data == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(data)
			}
			return
		}
		http.NotFound(w, r)
	}))
	defer endpoint.Close()
	if err := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, hfdownloader.Settings{OutputDir: localRoot, Endpoint: endpoint.URL, Concurrency: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: "main"}, hfdownloader.Settings{OutputDir: localRoot, Endpoint: endpoint.URL, Concurrency: 1}, nil); err != nil {
		t.Fatal(err)
	}

	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(base, "cache"), LocalDir: localRoot, Endpoint: endpoint.URL})
	w := cacheRequest(t, s, http.MethodGet, "/api/cache", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/cache status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Repos []struct {
			Repo string `json:"repo"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	foundOrdinary := false
	for _, repo := range response.Repos {
		if repo.Repo == "owner/model" {
			foundOrdinary = true
		}
		if strings.HasPrefix(repo.Repo, hfdownloader.BranchVariantServiceDirName+"/") {
			t.Errorf("owned branch service tree was exposed as an ordinary local repository: %q", repo.Repo)
		}
	}
	if !foundOrdinary {
		t.Fatalf("ordinary same-spelled GGUF repository was hidden: %s", w.Body.String())
	}
}

func TestLocalBranchVariantCancelRetryRestoresExactPresenceProof(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	shared := []byte("same-sha-shared-lfs-file")
	revisions := map[string]struct {
		commit string
		files  map[string][]byte
	}{
		"4.00bpw": {
			commit: "commit-a",
			files: map[string][]byte{
				"00-config.json":         []byte(`{"model":"aaaa"}`),
				"01-weights.safetensors": []byte("weights-A"),
				"02-shared.bin":          shared,
				"03-support.json":        []byte(`{"branch":"AAAA"}`),
				"04-tokenizer.json":      []byte(`{"tokens":"aaaa"}`),
			},
		},
		"4_25": {
			commit: "commit-b",
			files: map[string][]byte{
				"00-config.json":         []byte(`{"model":"bbbb"}`),
				"01-weights.safetensors": []byte("weights-B"),
				"02-shared.bin":          shared,
				"03-support.json":        []byte(`{"branch":"BBBB"}`),
				"04-tokenizer.json":      []byte(`{"tokens":"bbbb"}`),
			},
		},
	}
	lookup := func(revision string) (struct {
		commit string
		files  map[string][]byte
	}, bool) {
		if info, ok := revisions[revision]; ok {
			return info, true
		}
		for _, info := range revisions {
			if info.commit == revision {
				return info, true
			}
		}
		return struct {
			commit string
			files  map[string][]byte
		}{}, false
	}
	aAtShared := make(chan struct{})
	aCancelled := make(chan struct{})
	bAtSharedPartial := make(chan struct{})
	releaseB := make(chan struct{})
	var releaseBOnce sync.Once
	releaseBTransfer := func() { releaseBOnce.Do(func() { close(releaseB) }) }
	var aGateOnce, bGateOnce sync.Once
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const repoPrefix = "/api/models/owner/model/"
		if strings.HasPrefix(r.URL.Path, repoPrefix+"revision/") {
			revision := strings.TrimPrefix(r.URL.Path, repoPrefix+"revision/")
			info, ok := lookup(revision)
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": info.commit})
			return
		}
		if strings.HasPrefix(r.URL.Path, repoPrefix+"tree/") {
			revision := strings.TrimPrefix(r.URL.Path, repoPrefix+"tree/")
			info, ok := lookup(revision)
			if !ok {
				http.NotFound(w, r)
				return
			}
			names := make([]string, 0, len(info.files))
			for name := range info.files {
				names = append(names, name)
			}
			sort.Strings(names)
			nodes := make([]map[string]any, 0, len(names))
			for _, name := range names {
				body := info.files[name]
				node := map[string]any{"type": "file", "path": name, "size": len(body)}
				if name == "01-weights.safetensors" || name == "02-shared.bin" {
					digest := sha256.Sum256(body)
					node["lfs"] = map[string]any{"oid": hex.EncodeToString(digest[:]), "size": len(body)}
				}
				nodes = append(nodes, node)
			}
			_ = json.NewEncoder(w).Encode(nodes)
			return
		}
		for _, prefix := range []string{"/owner/model/raw/", "/owner/model/resolve/"} {
			if !strings.HasPrefix(r.URL.Path, prefix) {
				continue
			}
			revision, name, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, prefix), "/")
			if !ok {
				http.NotFound(w, r)
				return
			}
			info, found := lookup(revision)
			body, hasFile := info.files[name]
			if !found || !hasFile {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			if r.Method == http.MethodHead {
				return
			}
			if name == "02-shared.bin" && info.commit == "commit-a" {
				first := false
				aGateOnce.Do(func() {
					first = true
					close(aAtShared)
					<-r.Context().Done()
					close(aCancelled)
				})
				if first {
					return
				}
			}
			if name == "02-shared.bin" && info.commit == "commit-b" {
				bGateOnce.Do(func() {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(body[:4])
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
					close(bAtSharedPartial)
					select {
					case <-releaseB:
					case <-r.Context().Done():
						return
					}
					_, _ = w.Write(body[4:])
				})
				return
			}
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(hub.Close)
	t.Cleanup(releaseBTransfer)

	localRoot := filepath.Join(base, "local-models")
	cfg := Config{
		CacheDir: filepath.Join(base, "cache"), LocalDir: localRoot,
		Endpoint: hub.URL, MaxActive: 2, Concurrency: 1, Verify: "size",
	}
	s := newTestServerWithConfig(t, cfg)
	updates := s.jobs.Subscribe()
	defer s.jobs.Unsubscribe(updates)
	waitFilesComplete := func(jobID string, names ...string) {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			job, ok := s.jobs.GetJob(jobID)
			if ok {
				complete := make(map[string]bool, len(names))
				for _, name := range names {
					complete[name] = false
				}
				for _, file := range job.Files {
					if _, wanted := complete[file.Path]; wanted && file.Status == "complete" {
						complete[file.Path] = true
					}
				}
				allComplete := true
				for _, done := range complete {
					allComplete = allComplete && done
				}
				if allComplete {
					return
				}
			}
			select {
			case <-updates:
			case <-timer.C:
				job, _ := s.jobs.GetJob(jobID)
				t.Fatalf("job %s did not finish files %v: %+v", jobID, names, job)
			}
		}
	}
	start := func(revision string) *Job {
		t.Helper()
		body := fmt.Sprintf(`{"repo":"owner/model","revision":%q,"branchVariant":true}`, revision)
		w := cacheRequest(t, s, http.MethodPost, "/api/download", body)
		if w.Code != http.StatusAccepted {
			t.Fatalf("start %s status=%d body=%s", revision, w.Code, w.Body.String())
		}
		var job Job
		if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		return &job
	}

	jobA := start("4.00bpw")
	waitJobStatus(t, s.jobs, jobA.ID, JobStatusRunning, 5*time.Second)
	select {
	case <-aAtShared:
	case <-time.After(5 * time.Second):
		job, _ := s.jobs.GetJob(jobA.ID)
		t.Fatalf("branch A did not finish its earlier files and reach the shared-file gate: %+v", job)
	}
	branchA, err := hfdownloader.BranchVariantPath(localRoot, hfdownloader.RepoTypeModel, "owner/model", "4.00bpw", "commit-a")
	if err != nil {
		t.Fatal(err)
	}
	waitFilesComplete(jobA.ID, "00-config.json", "01-weights.safetensors")
	for _, name := range []string{"00-config.json", "01-weights.safetensors"} {
		if _, err := os.Stat(filepath.Join(branchA, name)); err != nil {
			t.Fatalf("branch A did not materialize %s before its gate: %v", name, err)
		}
	}

	jobB := start("4_25")
	waitJobStatus(t, s.jobs, jobB.ID, JobStatusRunning, 5*time.Second)
	select {
	case <-bAtSharedPartial:
	case <-time.After(5 * time.Second):
		t.Fatal("branch B did not reach the shared-file partial gate")
	}
	cancel := cacheRequest(t, s, http.MethodDelete, "/api/jobs/"+jobA.ID, "")
	if cancel.Code != http.StatusOK {
		t.Fatalf("cancel branch A status=%d body=%s", cancel.Code, cancel.Body.String())
	}
	select {
	case <-aCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("branch A transfer did not observe cancellation")
	}
	if got, _ := s.jobs.GetJob(jobB.ID); got == nil || got.Status != JobStatusRunning {
		t.Fatalf("cancelling branch A disturbed branch B: %+v", got)
	}
	releaseBTransfer()
	if !s.jobs.WaitAll(10 * time.Second) {
		t.Fatal("cancelled A / active B run goroutines did not settle")
	}
	if got, _ := s.jobs.GetJob(jobA.ID); got == nil || got.Status != JobStatusCancelled {
		t.Fatalf("branch A status after cancel=%+v", got)
	}
	if got, _ := s.jobs.GetJob(jobB.ID); got == nil || got.Status != JobStatusCompleted {
		t.Fatalf("branch B status after release=%+v", got)
	}
	if status, present := branchPresenceRequest(t, s, "revision=4_25"); status != http.StatusOK || !present.Downloaded {
		t.Fatalf("completed branch B presence status=%d response=%+v", status, present)
	}
	if status, absent := branchPresenceRequest(t, s, "revision=4.00bpw"); status != http.StatusOK || absent.Downloaded {
		t.Fatalf("cancelled branch A acquired completion proof: status=%d response=%+v", status, absent)
	}

	// Model same-size external corruption of an untrusted non-LFS file before
	// retry. A size-only skip must not bless it merely to create a manifest hash.
	configPath := filepath.Join(branchA, "00-config.json")
	corrupt := []byte(strings.Repeat("x", len(revisions["4.00bpw"].files["00-config.json"])))
	if err := os.WriteFile(configPath, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if len(corrupt) != len(revisions["4.00bpw"].files["00-config.json"]) || string(corrupt) == string(revisions["4.00bpw"].files["00-config.json"]) {
		t.Fatal("test setup did not create same-size wrong content")
	}
	retry := cacheRequest(t, s, http.MethodPost, "/api/jobs/"+jobA.ID+"/retry", "")
	if retry.Code != http.StatusOK {
		t.Fatalf("retry branch A status=%d body=%s", retry.Code, retry.Body.String())
	}
	waitJobStatus(t, s.jobs, jobA.ID, JobStatusCompleted, 10*time.Second)
	if !s.jobs.WaitAll(10 * time.Second) {
		t.Fatal("retried branch A did not settle")
	}

	for revision, info := range revisions {
		artifact, err := hfdownloader.BranchVariantPath(localRoot, hfdownloader.RepoTypeModel, "owner/model", revision, info.commit)
		if err != nil {
			t.Fatal(err)
		}
		for name, want := range info.files {
			got, err := os.ReadFile(filepath.Join(artifact, name))
			if err != nil || string(got) != string(want) {
				t.Errorf("retry artifact %s/%s = %q, %v; want %q", revision, name, got, err, want)
			}
		}
		controlDir, err := hfdownloader.BranchVariantMetadataPath(localRoot, hfdownloader.RepoTypeModel, "owner/model", revision, info.commit)
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := hfdownloader.ReadManifest(filepath.Join(controlDir, hfdownloader.ManifestFilename))
		if err != nil {
			t.Errorf("read %s retry manifest: %v", revision, err)
			continue
		}
		if !manifest.IsCompleteBranchVariant("owner/model", hfdownloader.RepoTypeModel, revision) {
			t.Errorf("%s retry manifest does not prove the exact artifact: %+v", revision, manifest)
		}
		for _, file := range manifest.Files {
			want := info.files[file.Name]
			digest := sha256.Sum256(want)
			if file.Blob != "blobs/"+hex.EncodeToString(digest[:]) {
				t.Errorf("%s manifest hash for %s = %q; want downloaded-content hash %x", revision, file.Name, file.Blob, digest)
			}
		}
		if status, presence := branchPresenceRequest(t, s, "revision="+url.QueryEscape(revision)); status != http.StatusOK || !presence.Downloaded || presence.ExpectedFiles != len(info.files) {
			t.Errorf("retry presence %s status=%d response=%+v", revision, status, presence)
		}
	}
}

func TestBranchVariantDownloadRequestRejectsFilteredAndImplicitRevision(t *testing.T) {
	s := newTestServer(t)
	for _, body := range []string{
		`{"repo":"owner/model","branchVariant":true}`,
		`{"repo":"owner/model","revision":"4.00bpw","branchVariant":true,"filters":["weights"]}`,
		`{"repo":"owner/model","revision":"4.00bpw","branchVariant":true,"dataset":true}`,
	} {
		if w := cacheRequest(t, s, http.MethodPost, "/api/download", body); w.Code != http.StatusBadRequest {
			t.Errorf("invalid branch request status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

func TestBranchVariantJobPauseResumeKeepsArtifactIdentity(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	endpoint, _, _, reached := cacheDownloadFixture(t, "pause")
	cfg := Config{CacheDir: filepath.Join(base, "cache"), Endpoint: endpoint.URL, MaxActive: 1, Concurrency: 1, Verify: "sha256"}
	s := newTestServerWithConfig(t, cfg)
	job, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("branch transfer did not reach the pause gate")
	}
	if !s.jobs.PauseJob(job.ID) {
		t.Fatal("pause branch job failed")
	}
	if !s.jobs.WaitAll(5 * time.Second) {
		t.Fatal("paused branch run did not exit")
	}
	paused, ok := s.jobs.GetJob(job.ID)
	if !ok || paused.Status != JobStatusPaused || !paused.BranchVariant {
		t.Fatalf("paused branch job=%+v, found=%t", paused, ok)
	}
	if !s.jobs.ResumeJob(job.ID) {
		t.Fatal("resume branch job failed")
	}
	if !s.jobs.WaitAll(5 * time.Second) {
		t.Fatal("resumed branch run did not exit")
	}
	completed, ok := s.jobs.GetJob(job.ID)
	if !ok || completed.Status != JobStatusCompleted {
		t.Fatalf("resumed branch job=%+v, found=%t", completed, ok)
	}
	if status, presence := branchPresenceRequest(t, s, "revision=4.00bpw"); status != http.StatusOK || !presence.Downloaded || presence.Commit != "commit123" {
		t.Fatalf("resumed branch presence status=%d response=%+v", status, presence)
	}
}
