// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func ownershipContractSettings(cacheDir, endpoint, threshold string) Settings {
	return Settings{
		CacheDir: cacheDir, Endpoint: endpoint, Concurrency: 1, MaxActiveDownloads: 1,
		MultipartThreshold: threshold, Verify: "size", Retries: 0, StallTimeout: "0",
	}
}

type ownershipContractOutcome struct {
	err   error
	skips []string
	done  []string
}

func runOwnershipContractDownload(ctx context.Context, cfg Settings) ownershipContractOutcome {
	out := ownershipContractOutcome{}
	var mu sync.Mutex
	out.err = Download(ctx, Job{Repo: "owner/model", Revision: "main"}, cfg, func(ev ProgressEvent) {
		switch {
		case ev.Event == "file_done" && strings.Contains(ev.Message, "skip"):
			mu.Lock()
			out.skips = append(out.skips, ev.Message)
			mu.Unlock()
		case ev.Event == "file_done":
			mu.Lock()
			out.done = append(out.done, ev.Path)
			mu.Unlock()
		}
	})
	return out
}

func ownershipContractRepo(t *testing.T, cacheDir string) *RepoDir {
	t.Helper()
	repo, err := NewHFCache(cacheDir).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return repo
}

func assertOwnershipContractLinks(t *testing.T, cacheDir, rel string) {
	t.Helper()
	repoDir := filepath.Join(cacheDir, "hub", "models--owner--model")
	snap := filepath.Join(repoDir, "snapshots", "commit123", filepath.FromSlash(rel))
	if _, err := os.Lstat(snap); err != nil {
		t.Errorf("the snapshot entry %s is missing: %v", snap, err)
	}
	friendly := filepath.Join(cacheDir, "models", "owner", "model", filepath.FromSlash(rel))
	if _, err := os.Lstat(friendly); err != nil {
		t.Errorf("the friendly entry %s is missing: %v", friendly, err)
	}
}

// Install before launching writers; this package-level hook is not concurrent-safe.
func installDomainContentionProbe(t *testing.T) <-chan struct{} {
	t.Helper()
	contended := make(chan struct{})
	var once sync.Once
	restore := stagingOwnershipContentionHook
	stagingOwnershipContentionHook = func(string) {
		once.Do(func() { close(contended) })
	}
	t.Cleanup(func() { stagingOwnershipContentionHook = restore })
	return contended
}

func installBlobDomainContentionProbe(t *testing.T) <-chan struct{} {
	t.Helper()
	contended := make(chan struct{})
	var once sync.Once
	restore := blobDomainContentionHook
	blobDomainContentionHook = func(string) {
		once.Do(func() { close(contended) })
	}
	t.Cleanup(func() { blobDomainContentionHook = restore })
	return contended
}

func awaitContention(t *testing.T, contended <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-contended:
	case <-time.After(60 * time.Second):
		t.Fatalf("no writer waited for %s", what)
	}
}

func shortBlobOwnershipWait(t *testing.T, d time.Duration) {
	t.Helper()
	restore := blobOwnershipWaitBudget
	blobOwnershipWaitBudget = d
	t.Cleanup(func() { blobOwnershipWaitBudget = restore })
}

// A separate OS-lock handle publishes while Download waits. Both routes must
// reuse the blob and create links without transferring it again.
func TestDownloadWaitsForLiveForeignDomainOwnerAndReusesPublishedBlob(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	for _, tc := range []struct {
		name      string
		threshold string
	}{
		{name: "multipart route", threshold: "0"},
		{name: "single-request route", threshold: "1MB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testDownloadWaitsForLiveForeignDomainOwner(t, tc.threshold)
		})
	}
}

func testDownloadWaitsForLiveForeignDomainOwner(t *testing.T, threshold string) {
	t.Helper()
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	// Hold the foreign-owner lock in a separate open file description.
	owner, err := acquireStagingOwnershipLock(mustDomainDst(t, repo, sha))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.release()

	srv, getRanges := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, threshold)

	contended := installDomainContentionProbe(t)
	result := make(chan ownershipContractOutcome, 1)
	go func() { result <- runOwnershipContractDownload(context.Background(), cfg) }()

	awaitContention(t, contended, "the blob key's ownership domain")
	select {
	case out := <-result:
		t.Fatalf("the download finished while the live owner still held the domain (err=%v, skips=%v)",
			out.err, out.skips)
	default:
	}

	if err := os.WriteFile(repo.BlobPath(sha), full, 0o644); err != nil {
		t.Fatal(err)
	}
	owner.release()

	var out ownershipContractOutcome
	select {
	case out = <-result:
	case <-time.After(120 * time.Second):
		t.Fatal("the download did not finish after the owner released the domain")
	}
	if out.err != nil {
		t.Fatalf("Download: %v", out.err)
	}
	if len(out.skips) != 1 || !strings.Contains(out.skips[0], "blob exists") {
		t.Fatalf("skips = %v, want exactly one 'blob exists' skip", out.skips)
	}
	if got := getRanges(); len(got) != 0 {
		t.Errorf("the waiting item issued %d range GETs (%v), want 0: the published blob must be reused", len(got), got)
	}
	assertExactFile(t, repo.BlobPath(sha), full)
	assertOwnershipContractLinks(t, cacheDir, "model.bin")
}

func TestDownloadWaitsForLiveForeignDomainOwnerThatFails(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	owner, err := acquireStagingOwnershipLock(mustDomainDst(t, repo, sha))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.release()

	srv, getRanges := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")

	contended := installDomainContentionProbe(t)
	result := make(chan ownershipContractOutcome, 1)
	go func() { result <- runOwnershipContractDownload(context.Background(), cfg) }()

	awaitContention(t, contended, "the blob key's ownership domain")
	select {
	case out := <-result:
		t.Fatalf("the download finished while the live owner still held the domain (err=%v, skips=%v)",
			out.err, out.skips)
	default:
	}

	owner.release()

	var out ownershipContractOutcome
	select {
	case out = <-result:
	case <-time.After(120 * time.Second):
		t.Fatal("the download did not finish after the owner released the domain")
	}
	if out.err != nil {
		t.Fatalf("Download: %v", out.err)
	}
	if len(out.skips) != 0 {
		t.Errorf("skips = %v, want none: the owner published nothing, so the item had to download the file", out.skips)
	}
	if got := getRanges(); len(got) == 0 {
		t.Error("the item issued no range GET: the owner published nothing, so the item had to download the file")
	}
	assertExactFile(t, repo.BlobPath(sha), full)
	assertOwnershipContractLinks(t, cacheDir, "model.bin")
}

func TestDownloadFailsWhenLiveForeignOwnerHoldsDomainPastBudget(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	shortBlobOwnershipWait(t, 300*time.Millisecond)

	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	owner, err := acquireStagingOwnershipLock(mustDomainDst(t, repo, sha))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.release()

	srv, getRanges := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")

	out := runOwnershipContractDownload(context.Background(), cfg)

	if out.err == nil {
		t.Fatalf("the download reported success although the live owner still holds the domain (skips=%v)", out.skips)
	}
	if !errors.Is(out.err, errBlobOwnedByLiveWriter) {
		t.Errorf("Download error = %v, want the actionable live-owner error", out.err)
	}
	if !strings.Contains(out.err.Error(), sha) || !strings.Contains(out.err.Error(), "try again later") {
		t.Errorf("Download error = %v, want the blob key and the retry action in the message", out.err)
	}
	if len(out.skips) != 0 {
		t.Errorf("skips = %v, want none: the file is not present, so a skip would be a false success", out.skips)
	}
	if len(out.done) != 0 {
		t.Errorf("file_done events = %v, want none: the file is not present", out.done)
	}
	if _, serr := os.Stat(repo.BlobPath(sha)); !os.IsNotExist(serr) {
		t.Errorf("a blob was published although the live owner holds the domain, stat err=%v", serr)
	}
	if got := getRanges(); len(got) != 0 {
		t.Errorf("the item issued %d range GETs (%v), want 0: it never owned the key", len(got), got)
	}
}

// Both routes must reuse the sibling's blob after the domain wait.
func TestDownloadSameProcessSiblingWriterReusesPublishedBlob(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	for _, tc := range []struct {
		name      string
		threshold string
	}{
		{name: "multipart route", threshold: "0"},
		{name: "single-request route", threshold: "1MB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testDownloadSameProcessSiblingWriter(t, tc.threshold)
		})
	}
}

func testDownloadSameProcessSiblingWriter(t *testing.T, threshold string) {
	t.Helper()
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	// Hold the first transfer while the second writer waits.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHub := func() { releaseOnce.Do(func() { close(release) }) }
	firstGet := make(chan struct{})
	var getOnce sync.Once
	var (
		mu     sync.Mutex
		ranges []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte("[" + lfsTreeNode("model.bin", total, sha) + "]"))
		case strings.Contains(r.URL.Path, "/resolve/"):
			if r.Method == http.MethodGet {
				mu.Lock()
				ranges = append(ranges, r.Header.Get("Range"))
				mu.Unlock()
				getOnce.Do(func() { close(firstGet) })
				<-release
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
		default:
			http.NotFound(w, r)
		}
	}))
	// The deferred release runs BEFORE the server close (defers are LIFO):
	// a failing test must not leave the held handler blocking the close.
	defer srv.Close()
	defer releaseHub()

	cfg := ownershipContractSettings(cacheDir, srv.URL, threshold)

	first := make(chan ownershipContractOutcome, 1)
	go func() { first <- runOwnershipContractDownload(context.Background(), cfg) }()

	select {
	case <-firstGet:
	case <-time.After(60 * time.Second):
		t.Fatal("the first writer never started its transfer")
	}

	// Install the package hook before launching the competing writer.
	contended := installBlobDomainContentionProbe(t)
	second := make(chan ownershipContractOutcome, 1)
	go func() { second <- runOwnershipContractDownload(context.Background(), cfg) }()
	awaitContention(t, contended, "the in-process ownership domain")

	releaseHub()

	var firstOut ownershipContractOutcome
	select {
	case firstOut = <-first:
	case <-time.After(120 * time.Second):
		t.Fatal("the first writer did not finish")
	}
	if firstOut.err != nil {
		t.Fatalf("the first writer failed: %v", firstOut.err)
	}

	var secondOut ownershipContractOutcome
	select {
	case secondOut = <-second:
	case <-time.After(120 * time.Second):
		t.Fatal("the second writer did not finish")
	}
	if secondOut.err != nil {
		t.Fatalf("the second writer failed although the blob was published: %v", secondOut.err)
	}
	if len(secondOut.skips) != 1 || !strings.Contains(secondOut.skips[0], "blob exists") {
		t.Fatalf("the second writer's skips = %v, want exactly one 'blob exists' skip", secondOut.skips)
	}
	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	if len(got) != 1 {
		t.Errorf("the two writers issued %d range GETs (%v), want exactly 1: the second writer must reuse the published blob",
			len(got), got)
	}
	assertExactFile(t, repo.BlobPath(sha), full)
	assertOwnershipContractLinks(t, cacheDir, "model.bin")
}

// Cleanup must preserve a live writer's partial while removing unowned artifacts.
func TestCleanupLeavesAnotherLiveWritersPartialAlone(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	restoreWait := stagingLockCleanupWait
	stagingLockCleanupWait = 50 * time.Millisecond
	defer func() { stagingLockCleanupWait = restoreWait }()

	const (
		ownedSHA   = "1111111111111111111111111111111111111111111111111111111111111111"
		foreignSHA = "2222222222222222222222222222222222222222222222222222222222222222"
		otherSHA   = "3333333333333333333333333333333333333333333333333333333333333333"
	)

	newCache := func(t *testing.T) (string, string) {
		t.Helper()
		cacheDir := t.TempDir()
		blobsDir := filepath.Join(cacheDir, "hub", "models--owner--model", "blobs")
		if err := os.MkdirAll(blobsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		return cacheDir, blobsDir
	}

	t.Run("owner in another process", func(t *testing.T) {
		cacheDir, blobsDir := newCache(t)
		dst := filepath.Join(blobsDir, "tmp-"+foreignSHA)
		partial := dst + ".part"
		live := []byte("the live partial of the other process")
		if err := os.WriteFile(partial, live, 0o644); err != nil {
			t.Fatal(err)
		}
		ownDst := filepath.Join(blobsDir, "tmp-"+otherSHA)
		ownPartial := ownDst + ".part"
		if err := os.WriteFile(ownPartial, []byte("this job's own partial"), 0o644); err != nil {
			t.Fatal(err)
		}

		child := startBlobOwnershipChild(t, blobOwnershipChildConfig{
			DomainDst:   dst,
			PartialPath: partial,
			Payload:     string(live),
			ReadyPath:   filepath.Join(blobsDir, "child-ready"),
			GoPath:      filepath.Join(blobsDir, "child-go"),
		})
		waitForFile(t, filepath.Join(blobsDir, "child-ready"), 60*time.Second)

		if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst, ownDst}); err != nil {
			t.Fatalf("CleanupJobPartFiles: %v", err)
		}

		got, err := os.ReadFile(partial)
		if err != nil {
			t.Fatalf("the live partial of the other process was removed: %v", err)
		}
		if string(got) != string(live) {
			t.Errorf("the live partial holds %q, want %q", got, live)
		}
		if _, err := os.Stat(ownPartial); !os.IsNotExist(err) {
			t.Errorf("the job's own partial survived the cleanup, stat err=%v", err)
		}
		releaseBlobOwnershipChild(t, child, filepath.Join(blobsDir, "child-go"))
	})

	t.Run("owner in this process", func(t *testing.T) {
		cacheDir, blobsDir := newCache(t)
		dst := filepath.Join(blobsDir, "tmp-"+ownedSHA)
		partial := dst + ".part"
		live := []byte("the live partial of the other writer")
		if err := os.WriteFile(partial, live, 0o644); err != nil {
			t.Fatal(err)
		}
		domain, err := blobDomainAcquire(t.Context(), filepath.Join(blobsDir, ownedSHA), dst)
		if err != nil {
			t.Fatal(err)
		}
		defer domain.release()

		if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
			t.Fatalf("CleanupJobPartFiles: %v", err)
		}
		got, err := os.ReadFile(partial)
		if err != nil {
			t.Fatalf("the live partial of the other writer was removed: %v", err)
		}
		if string(got) != string(live) {
			t.Errorf("the live partial holds %q, want %q", got, live)
		}
	})
}

func TestCleanupRemovesOwnPartialAfterCancel(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const sha = "4444444444444444444444444444444444444444444444444444444444444444"
	cacheDir := t.TempDir()
	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--model", "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(blobsDir, "tmp-"+sha)
	partial := dst + ".part"
	if err := os.WriteFile(partial, []byte("this job's own partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".part-00", []byte("legacy part"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("completed temp"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	for _, p := range []string{partial, dst + ".part-00", dst} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("the job's own artifact %s survived the cleanup, stat err=%v", filepath.Base(p), err)
		}
	}
}

const (
	blobOwnershipChildEnv  = "HFDESK_BLOB_OWNERSHIP_CHILD"
	blobOwnershipConfigEnv = "HFDESK_BLOB_OWNERSHIP_CONFIG"
)

type blobOwnershipChildConfig struct {
	DomainDst   string `json:"domain_dst"`
	PartialPath string `json:"partial_path"`
	Payload     string `json:"payload"`
	ReadyPath   string `json:"ready_path"`
	GoPath      string `json:"go_path"`
	// PublishPath is written only after PublishGoPath appears, letting the parent
	// establish contention before publication.
	PublishPath string `json:"publish_path"`
	// Base64 preserves binary payloads in the JSON configuration.
	PublishPayloadBase64 string `json:"publish_payload_base64"`
	// PublishGoPath is the file whose appearance asks the helper to publish.
	PublishGoPath string `json:"publish_go_path"`
}

// TestBlobOwnershipChildProcess is a re-executed lock holder. It writes a live
// partial, optionally publishes after the parent signal, and waits for release.
func TestBlobOwnershipChildProcess(t *testing.T) {
	if os.Getenv(blobOwnershipChildEnv) != "1" {
		t.Skip("helper process for the blob ownership cleanup tests")
	}
	raw, err := os.ReadFile(os.Getenv(blobOwnershipConfigEnv))
	if err != nil {
		t.Fatalf("child config: %v", err)
	}
	var cfg blobOwnershipChildConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("child config: %v", err)
	}
	owner, err := acquireStagingOwnershipLock(cfg.DomainDst)
	if err != nil {
		t.Fatalf("child domain: %v", err)
	}
	defer owner.release()
	if err := os.WriteFile(cfg.PartialPath, []byte(cfg.Payload), 0o644); err != nil {
		t.Fatalf("child partial: %v", err)
	}
	if err := os.WriteFile(cfg.ReadyPath, []byte("ready"), 0o644); err != nil {
		t.Fatalf("child ready: %v", err)
	}
	if cfg.PublishPath != "" {
		waitForFile(t, cfg.PublishGoPath, 120*time.Second)
		payload, derr := base64.StdEncoding.DecodeString(cfg.PublishPayloadBase64)
		if derr != nil {
			t.Fatalf("child publish payload: %v", derr)
		}
		if err := os.WriteFile(cfg.PublishPath, payload, 0o644); err != nil {
			t.Fatalf("child publish: %v", err)
		}
	}
	waitForFile(t, cfg.GoPath, 120*time.Second)
}

func startBlobOwnershipChild(t *testing.T, cfg blobOwnershipChildConfig) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := cfg.ReadyPath + ".cfg"
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestBlobOwnershipChildProcess$", "-test.timeout=180s")
	cmd.Env = append(os.Environ(),
		blobOwnershipChildEnv+"=1",
		blobOwnershipConfigEnv+"="+cfgPath)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

func releaseBlobOwnershipChild(t *testing.T, cmd *exec.Cmd, goPath string) {
	t.Helper()
	if err := os.WriteFile(goPath, []byte("go"), 0o644); err != nil {
		t.Fatalf("release helper: %v", err)
	}
	_ = cmd.Wait()
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", filepath.Base(path))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestCleanupLeavesOldMultipartFamilyUnderLiveDomain(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	restoreWait := stagingLockCleanupWait
	stagingLockCleanupWait = 50 * time.Millisecond
	defer func() { stagingLockCleanupWait = restoreWait }()

	const sha = "6666666666666666666666666666666666666666666666666666666666666666"
	cacheDir := t.TempDir()
	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--model", "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(blobsDir, "tmp-"+sha)
	oldPart := dst + ".part-00"
	oldLayout := dst + ".parts.json"
	if err := os.WriteFile(oldPart, []byte("obsolete part bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldLayout, []byte(`{"size":19}`), 0o644); err != nil {
		t.Fatal(err)
	}

	key, ok := cacheDstDomainKey(dst)
	if !ok {
		t.Fatalf("cache destination %s has no domain key", dst)
	}
	live, err := blobDomainAcquire(context.Background(), key, dst)
	if err != nil {
		t.Fatalf("acquire live domain: %v", err)
	}
	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	for _, p := range []string{oldPart, oldLayout} {
		if _, serr := os.Stat(p); serr != nil {
			t.Errorf("cleanup removed a live owner's old-format artifact %s: %v", filepath.Base(p), serr)
		}
	}
	live.release()

	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles after release: %v", err)
	}
	for _, p := range []string{oldPart, oldLayout} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("old-format artifact %s survived a free-domain cleanup, stat err=%v", filepath.Base(p), serr)
		}
	}
}
