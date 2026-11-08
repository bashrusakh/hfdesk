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

// A separately held ownership domain publishes while Download waits. Both
// routes must reuse the blob and create links without transferring it again.
func TestDownloadWaitsForLiveForeignDomainOwnerAndReusesPublishedBlob(t *testing.T) {
	if !ownershipOSLockAvailable {
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

	// Hold the foreign owner's domain before Download starts.
	owner := acquireTestDomainFresh(t, mustDomainDst(t, repo, sha))

	srv, getRanges := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, threshold)

	contended := installBlobDomainContentionProbe(t)
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
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	owner := acquireTestDomainFresh(t, mustDomainDst(t, repo, sha))

	srv, getRanges := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")

	contended := installBlobDomainContentionProbe(t)
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
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	shortBlobOwnershipWait(t, 300*time.Millisecond)

	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	realm, err := NewHFCache(cacheDir).ownershipRealmForData()
	if err != nil {
		t.Fatal(err)
	}
	dst := mustDomainDst(t, repo, sha)
	owner, err := realm.acquireWriter(context.Background(), []string{
		repo.BlobPath(sha), dst, dst + ".part", repo.IncompletePath(sha), repo.IncompleteMetaPath(sha),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()

	srv, getRanges := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")

	out := runOwnershipContractDownload(context.Background(), cfg)

	if out.err == nil {
		t.Fatalf("the download reported success although the live owner still holds the domain (skips=%v)", out.skips)
	}
	if !errors.Is(out.err, errStagingOwnershipBusy) {
		t.Errorf("Download error = %v, want the exclusive-scope-held error", out.err)
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
	if !ownershipOSLockAvailable {
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
			if r.Header.Get("X-HFDesk-Addressing-Probe") == "1" {
				http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
				return
			}
			if r.Method == http.MethodGet {
				if r.Header.Get("Range") != "bytes=0-0" {
					mu.Lock()
					ranges = append(ranges, r.Header.Get("Range"))
					mu.Unlock()
					getOnce.Do(func() { close(firstGet) })
					<-release
				}
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
	if !ownershipOSLockAvailable {
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
	if !ownershipOSLockAvailable {
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

func TestSeparateProcessDownloadsShareCacheAnchorAndReleaseWaiterSlot(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no external ownership lock primitive")
	}
	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	fullA, fullB := rangedTestPayload(4_096), bytes.Repeat([]byte("b"), 4_096)
	shaA, shaB := singleTestSHA(fullA), singleTestSHA(fullB)
	tree := "[" + lfsTreeNode("a.bin", len(fullA), shaA) + "," + lfsTreeNode("b.bin", len(fullB), shaB) + "]"
	var (
		mu       sync.Mutex
		payloads = map[string]int{}
		aEntered = make(chan struct{})
		bEntered = make(chan struct{})
		releaseA = make(chan struct{})
		release  sync.Once
		aOnce    sync.Once
		bOnce    sync.Once
	)
	defer func() { release.Do(func() { close(releaseA) }) }()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte(tree))
		case strings.Contains(r.URL.Path, "/resolve/"):
			rel := filepath.Base(r.URL.Path)
			body := fullA
			if rel == "b.bin" {
				body = fullB
			}
			if r.Method == http.MethodGet && r.Header.Get("X-HFDesk-Addressing-Probe") != "1" {
				mu.Lock()
				payloads[rel]++
				mu.Unlock()
				if rel == "a.bin" {
					aOnce.Do(func() { close(aEntered) })
					select {
					case <-releaseA:
					case <-r.Context().Done():
						return
					}
				} else if rel == "b.bin" {
					bOnce.Do(func() { close(bEntered) })
				}
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	child := startBlobOwnershipChild(t, blobOwnershipChildConfig{
		DownloadEndpoint: hub.URL, CacheDir: cacheDir,
		ReadyPath: filepath.Join(base, "child-ready"), GoPath: filepath.Join(base, "child-go"),
	})
	select {
	case <-aEntered:
	case <-time.After(60 * time.Second):
		t.Fatal("the child Download did not enter the gated a.bin payload request")
	}
	waitForFile(t, filepath.Join(base, "child-ready"), 10*time.Second)

	contended := installBlobDomainContentionProbe(t)
	parentEvents := make(chan ProgressEvent, 64)
	parentResult := make(chan error, 1)
	var eventMu sync.Mutex
	var skippedA bool
	settings := Settings{
		CacheDir: cacheDir, Endpoint: hub.URL,
		Concurrency: 1, MaxActiveDownloads: 1, MultipartThreshold: "0",
		Verify: "size", Retries: 0, StallTimeout: "0",
	}
	go func() {
		parentResult <- Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, settings, func(ev ProgressEvent) {
			if ev.Event == "file_done" && ev.Path == "a.bin" && strings.Contains(ev.Message, "skip") {
				eventMu.Lock()
				skippedA = true
				eventMu.Unlock()
			}
			select {
			case parentEvents <- ev:
			default:
			}
		})
	}()
	awaitContention(t, contended, "the child's external item scope")

	bDone := false
	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()
	for !bDone {
		select {
		case ev := <-parentEvents:
			if ev.Event == "file_done" && ev.Path == "b.bin" {
				if strings.Contains(ev.Message, "skip") {
					t.Fatalf("b.bin unexpectedly skipped before the child completed: %q", ev.Message)
				}
				bDone = true
			}
		case err := <-parentResult:
			t.Fatalf("parent Download ended while the child held a.bin: %v", err)
		case <-timer.C:
			t.Fatal("the parent retained its only transfer slot while waiting; b.bin did not complete")
		}
	}
	select {
	case <-bEntered:
	default:
		t.Fatal("b.bin completed without a payload request")
	}
	mu.Lock()
	seenA, seenB := payloads["a.bin"], payloads["b.bin"]
	mu.Unlock()
	if seenA != 1 || seenB != 1 {
		t.Fatalf("payload GET counts while a.bin was held = a:%d b:%d, want 1 each", seenA, seenB)
	}

	release.Do(func() { close(releaseA) })
	releaseBlobOwnershipChild(t, child, filepath.Join(base, "child-go"))
	select {
	case err := <-parentResult:
		if err != nil {
			t.Fatalf("parent Download: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("parent did not reuse the blob after the child released its item")
	}
	eventMu.Lock()
	gotSkippedA := skippedA
	eventMu.Unlock()
	if !gotSkippedA {
		t.Fatal("the parent did not report the published a.bin as a skip after waiting")
	}

	repo, err := NewHFCacheResolved(cacheDir, "").Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	assertExactFile(t, repo.BlobPath(shaA), fullA)
	assertExactFile(t, repo.BlobPath(shaB), fullB)
	assertOwnershipContractLinks(t, cacheDir, "a.bin")
	assertOwnershipContractLinks(t, cacheDir, "b.bin")
}

const (
	blobOwnershipChildEnv  = "HFDESK_BLOB_OWNERSHIP_CHILD"
	blobOwnershipConfigEnv = "HFDESK_BLOB_OWNERSHIP_CONFIG"
)

type blobOwnershipChildConfig struct {
	DomainDst        string `json:"domain_dst"`
	OwnershipDir     string `json:"ownership_dir"`
	DownloadEndpoint string `json:"download_endpoint"`
	CacheDir         string `json:"cache_dir"`
	PartialPath      string `json:"partial_path"`
	Payload          string `json:"payload"`
	ReadyPath        string `json:"ready_path"`
	GoPath           string `json:"go_path"`
	// PublishPath is written only after PublishGoPath appears, letting the parent
	// establish contention before publication.
	PublishPath string `json:"publish_path"`
	// Base64 preserves binary payloads in the JSON configuration.
	PublishPayloadBase64 string `json:"publish_payload_base64"`
	// PublishGoPath is the file whose appearance asks the helper to publish.
	PublishGoPath string `json:"publish_go_path"`
	// HoldCacheRoot with HoldEntryPath or HoldTreePath makes the helper hold
	// one cache admission scope (entry replacement or tree removal) instead of
	// a blob domain: the parent then drives a mutating operation against a
	// live holder in another process.
	HoldCacheRoot string `json:"hold_cache_root"`
	HoldEntryPath string `json:"hold_entry_path"`
	HoldTreePath  string `json:"hold_tree_path"`
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
	if cfg.HoldEntryPath != "" || cfg.HoldTreePath != "" {
		admission, aerr := OpenCacheAdmission(context.Background(), cfg.HoldCacheRoot)
		if aerr != nil {
			t.Fatalf("child cache admission: %v", aerr)
		}
		var release func()
		if cfg.HoldTreePath != "" {
			release, err = admission.AdmitTreeRemoval(context.Background(), cfg.HoldTreePath)
		} else {
			release, err = admission.AdmitEntryReplacement(context.Background(), cfg.HoldEntryPath)
		}
		if err != nil {
			t.Fatalf("child scope hold: %v", err)
		}
		defer release()
		if err := os.WriteFile(cfg.ReadyPath, []byte("ready"), 0o644); err != nil {
			t.Fatalf("child ready marker: %v", err)
		}
		waitForFile(t, cfg.GoPath, 120*time.Second)
		return
	}
	if cfg.DownloadEndpoint != "" {
		settings := Settings{
			CacheDir: cfg.CacheDir, OwnershipDir: cfg.OwnershipDir, Endpoint: cfg.DownloadEndpoint,
			Concurrency: 1, MaxActiveDownloads: 1, MultipartThreshold: "0",
			Verify: "size", Retries: 0, StallTimeout: "0",
		}
		err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, settings, func(ev ProgressEvent) {
			if ev.Event == "file_start" && ev.Path == "a.bin" {
				if err := os.WriteFile(cfg.ReadyPath, []byte("ready"), 0o644); err != nil {
					t.Errorf("child ready marker: %v", err)
				}
			}
		})
		if err != nil {
			t.Fatalf("child Download: %v", err)
		}
		waitForFile(t, cfg.GoPath, 120*time.Second)
		return
	}
	var realm *ownershipRealm
	if _, isCachePath := cacheDstDomainKey(cfg.DomainDst); isCachePath {
		hubDir := filepath.Dir(filepath.Dir(filepath.Dir(cfg.DomainDst)))
		cache := NewHFCacheResolved(hubDir, hubDir)
		realm, err = cache.ownershipRealmForData()
	} else {
		ownerDir, resolveErr := resolveOwnershipDir(cfg.OwnershipDir)
		if resolveErr != nil {
			t.Fatalf("child ownership path: %v", resolveErr)
		}
		realm, err = newOwnershipRealm(ownerDir)
	}
	if err != nil {
		t.Fatalf("child ownership realm: %v", err)
	}
	key, domainDst := testDomainIdentity(cfg.DomainDst)
	proofPath := cfg.DomainDst
	paths := []string{cfg.DomainDst, domainDst, cfg.PartialPath}
	if !strings.HasPrefix(key, flatDomainKeyPrefix) {
		if canonicalSHA256(filepath.Base(key)) {
			proofPath = key
			paths = append(paths, key, key+".incomplete", key+".incomplete.meta")
		}
	} else {
		proofPath = strings.TrimPrefix(key, flatDomainKeyPrefix)
	}
	if cfg.PublishPath != "" {
		paths = append(paths, cfg.PublishPath)
	}
	owner, err := acquireBlobDomainScoped(context.Background(), key, domainDst, realm, paths, proofPath)
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
	if !ownershipOSLockAvailable {
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
