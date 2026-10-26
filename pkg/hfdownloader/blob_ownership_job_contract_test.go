// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// The JOB CONTRACT of the blob key's ownership domain (issue #140 / PR #151).
//
// "Job completed successfully" MUST mean the requested files are present. A
// file owned by another LIVE writer is not present, so it is not a successful
// skip:
//
//   - the item WAITS for the blob key's ownership domain (bounded and
//     context-aware) instead of skipping;
//   - after it holds the domain it RE-CHECKS the published content-addressed
//     target: a blob the owner published while the item waited is reused, the
//     item finishes as an ordinary "blob exists" skip with its own cache links
//     created, and NO bytes are transferred again;
//   - when the wait budget expires with the owner still there, the item fails
//     with an explicit actionable error and the job does not report success.
//
// The CLEANUP of a blob key's artifacts participates in the same protocol: the
// artifacts are removed only while the key's ownership domain is held, so a
// cancelled job never deletes another live writer's partial.
//
// The cases below are driven through the real Download and cleanup paths, and
// the synchronization is deterministic: a writer proves that it waits through
// the production contention hooks, never through a sleep.

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

// ownershipContractSettings is the Download configuration of the ownership
// contract cases: one file, one connection, and no retries, so every observed
// request belongs to the item under test.
//
// threshold selects the route: the multipart route stages in the blob key's
// staging file, the single-request route stages in blobs/tmp-<sha>.part. Both
// routes publish the same content-addressed blob and both must reuse it.
func ownershipContractSettings(cacheDir, endpoint, threshold string) Settings {
	return Settings{
		CacheDir: cacheDir, Endpoint: endpoint, Concurrency: 1, MaxActiveDownloads: 1,
		MultipartThreshold: threshold, Verify: "size", Retries: 0, StallTimeout: "0",
	}
}

// ownershipContractOutcome is what one Download reported.
type ownershipContractOutcome struct {
	err   error
	skips []string
	// done reports the paths of the items that reported "file_done".
	done []string
}

// runOwnershipContractDownload runs one Download and collects its outcome.
func runOwnershipContractDownload(ctx context.Context, cfg Settings) ownershipContractOutcome {
	out := ownershipContractOutcome{}
	out.err = Download(ctx, Job{Repo: "owner/model", Revision: "main"}, cfg, func(ev ProgressEvent) {
		switch {
		case ev.Event == "file_done" && strings.Contains(ev.Message, "skip"):
			out.skips = append(out.skips, ev.Message)
		case ev.Event == "file_done":
			out.done = append(out.done, ev.Path)
		}
	})
	return out
}

// ownershipContractRepo builds the cache repository of the contract cases.
func ownershipContractRepo(t *testing.T, cacheDir string) *RepoDir {
	t.Helper()
	repo, err := NewHFCache(cacheDir, time.Minute).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return repo
}

// assertOwnershipContractLinks checks that the item's own cache entries exist:
// the snapshot entry and the friendly-view entry of its path.
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

// installDomainContentionProbe installs the production contention hook and
// returns a channel that is closed when an acquisition finds the ownership
// lock held.
//
// The hook is the production seam that proves a writer really waits for the
// domain instead of inferring it from a sleep. It MUST be installed before the
// writer starts: the hook is a package-level variable, so installing it while
// a writer is running is a data race.
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

// installBlobDomainContentionProbe installs the in-process domain contention
// hook and returns a channel that is closed when the slot is found busy.
//
// It proves that a second writer of this process waits for the key instead of
// proceeding. Like installDomainContentionProbe it must be installed before
// the writer starts.
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

// awaitContention blocks until the contention channel is closed.
func awaitContention(t *testing.T, contended <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-contended:
	case <-time.After(60 * time.Second):
		t.Fatalf("no writer waited for %s", what)
	}
}

// shortBlobOwnershipWait shrinks the blob key ownership wait budget for the
// duration of a test, so the exhaustion case does not wait the production
// budget. The behavior under test is the classification, not the duration.
func shortBlobOwnershipWait(t *testing.T, d time.Duration) {
	t.Helper()
	restore := blobOwnershipWaitBudget
	blobOwnershipWaitBudget = d
	t.Cleanup(func() { blobOwnershipWaitBudget = restore })
}

// TestDownloadWaitsForLiveForeignDomainOwnerAndReusesPublishedBlob covers the
// core of the new job contract: a live owner in ANOTHER PROCESS holds the blob
// key's ownership domain.
//
// The item must WAIT for the domain, and when the owner publishes the blob
// the item must finish as an ordinary "blob exists" skip: the file is present,
// the item's own cache links exist, NO additional range GET is issued, and the
// job reports success.
//
// The historical behavior skipped the item as "downloading by another process":
// the job reported success while the file was still missing, and the item's
// cache links were never created.
//
// Both routes are covered: the multipart route (which stages in the blob key's
// staging file) and the single-request route (which stages in
// blobs/tmp-<sha>.part). The single-request route is the one that would
// transfer the file a second time without the re-check after the domain.
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

// testDownloadWaitsForLiveForeignDomainOwner is the body of the cross-process
// wait case, once per route.
func testDownloadWaitsForLiveForeignDomainOwner(t *testing.T, threshold string) {
	t.Helper()
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	// The foreign owner holds the blob key's ownership domain. The lock
	// belongs to the OPEN FILE DESCRIPTION, not to the process, so a second
	// open in this process conflicts exactly like another process's writer
	// does.
	owner, err := acquireStagingOwnershipLock(mustDomainDst(t, repo, sha))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.release()

	srv, getRanges := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, threshold)

	// The probe is installed before the writer starts: the hook is a
	// package-level variable.
	contended := installDomainContentionProbe(t)
	result := make(chan ownershipContractOutcome, 1)
	go func() { result <- runOwnershipContractDownload(context.Background(), cfg) }()

	// The item must wait for the domain instead of skipping the file.
	awaitContention(t, contended, "the blob key's ownership domain")
	select {
	case out := <-result:
		t.Fatalf("the download finished while the live owner still held the domain (err=%v, skips=%v)",
			out.err, out.skips)
	default:
	}

	// The owner publishes the blob and releases the domain.
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
	// The file is present and the item's own links exist.
	assertExactFile(t, repo.BlobPath(sha), full)
	assertOwnershipContractLinks(t, cacheDir, "model.bin")
}

// TestDownloadWaitsForLiveForeignDomainOwnerThatFails covers the other half
// of the wait: the owner releases the domain WITHOUT publishing. The waiting
// item must then download the file itself and publish it correctly - the
// domain wait is not a skip, it is a wait for the result.
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

	// The probe is installed before the writer starts: the hook is a
	// package-level variable.
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

	// The owner fails: it releases the domain without publishing anything.
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

// TestDownloadFailsWhenLiveForeignOwnerHoldsDomainPastBudget covers the one
// case where the file cannot be produced: a live foreign owner still holds the
// domain when the wait budget expires.
//
// The item must fail with an explicit, actionable error; the file must not be
// reported as complete; and the job must not report success.
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

	// The owner never releases the domain.
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

// TestDownloadSameProcessSiblingWriterReusesPublishedBlob covers the
// in-process half of the domain: a sibling writer of this process holds the
// key, the second writer waits for the domain, and the published blob is
// reused instead of transferred a second time.
//
// The wait proves itself through the in-process contention hook, and the
// reused blob proves the re-check after the domain was acquired.
//
// Both routes are covered. The single-request route is the discriminating
// one: without the re-check after the domain it has no claim-level re-check
// to fall back on, so it would transfer the file a second time.
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

// testDownloadSameProcessSiblingWriter is the body of the in-process wait
// case, once per route.
func testDownloadSameProcessSiblingWriter(t *testing.T, threshold string) {
	t.Helper()
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	// The hub holds the first writer's payload response, so the first writer
	// keeps the blob key's ownership domain while the second writer waits.
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

	// The first writer owns the domain as soon as it starts its transfer.
	select {
	case <-firstGet:
	case <-time.After(60 * time.Second):
		t.Fatal("the first writer never started its transfer")
	}

	// The second writer must wait for the domain the first one holds. The
	// probe is installed before that writer starts: the hook is a
	// package-level variable.
	contended := installBlobDomainContentionProbe(t)
	second := make(chan ownershipContractOutcome, 1)
	go func() { second <- runOwnershipContractDownload(context.Background(), cfg) }()
	awaitContention(t, contended, "the in-process ownership domain")

	// Let the first writer finish and publish.
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

// TestCleanupLeavesAnotherLiveWritersPartialAlone covers the cleanup half of
// the ownership protocol.
//
// A cancelled job must not delete the LIVE partial of another writer that
// owns the same blob key: that writer would keep writing to an unlinked inode
// and its later publication would fail with ENOENT. The artifacts of a key the
// other writer does NOT own are still cleaned up, and a job that owns its own
// key still removes its own partial.
func TestCleanupLeavesAnotherLiveWritersPartialAlone(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	// The cleanup is best-effort: it must not delay an explicit cancel.
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

	// A writer in ANOTHER PROCESS owns the key and writes a live partial.
	t.Run("owner in another process", func(t *testing.T) {
		cacheDir, blobsDir := newCache(t)
		dst := filepath.Join(blobsDir, "tmp-"+foreignSHA)
		partial := dst + ".part"
		live := []byte("the live partial of the other process")
		if err := os.WriteFile(partial, live, 0o644); err != nil {
			t.Fatal(err)
		}
		// A key this job owns alone: its artifacts must still be cleaned.
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

		// The live partial of the other process survived, byte for byte.
		got, err := os.ReadFile(partial)
		if err != nil {
			t.Fatalf("the live partial of the other process was removed: %v", err)
		}
		if string(got) != string(live) {
			t.Errorf("the live partial holds %q, want %q", got, live)
		}
		// This job's own artifacts are still cleaned up.
		if _, err := os.Stat(ownPartial); !os.IsNotExist(err) {
			t.Errorf("the job's own partial survived the cleanup, stat err=%v", err)
		}
		releaseBlobOwnershipChild(t, child, filepath.Join(blobsDir, "child-go"))
	})

	// A DIFFERENT WRITER of this process owns the key and writes a live
	// partial.
	t.Run("owner in this process", func(t *testing.T) {
		cacheDir, blobsDir := newCache(t)
		dst := filepath.Join(blobsDir, "tmp-"+ownedSHA)
		partial := dst + ".part"
		live := []byte("the live partial of the other writer")
		if err := os.WriteFile(partial, live, 0o644); err != nil {
			t.Fatal(err)
		}
		// The other writer takes the blob key's ownership domain.
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

// TestCleanupRemovesOwnPartialAfterCancel pins the guarantee that a job CAN
// clean up its OWN artifacts: the key's ownership domain is free, so the
// cleanup takes it and removes the partial.
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

// TestNegativeControlCleanupWithoutDomainDeletesTheLivePartial is the
// negative control for the cleanup half of the ownership protocol.
//
// It installs the historical model through the production seam - a cleanup
// that removes a blob key's artifacts WITHOUT the key's ownership domain -
// and shows that the live partial of the other writer is deleted. That is
// the defect the domain fence removes, and it proves that the cleanup
// regression above really detects it.
func TestNegativeControlCleanupWithoutDomainDeletesTheLivePartial(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	restoreWait := stagingLockCleanupWait
	stagingLockCleanupWait = 50 * time.Millisecond
	defer func() { stagingLockCleanupWait = restoreWait }()

	const sha = "5555555555555555555555555555555555555555555555555555555555555555"
	cacheDir := t.TempDir()
	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--model", "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(blobsDir, "tmp-"+sha)
	partial := dst + ".part"
	live := []byte("the live partial of the other writer")
	if err := os.WriteFile(partial, live, 0o644); err != nil {
		t.Fatal(err)
	}

	// The historical model: the cleanup does not take the blob key's
	// ownership domain at all.
	oldAcquire := blobDomainCleanupAcquire
	blobDomainCleanupAcquire = func(context.Context, string, string) (*blobDomain, error) {
		return &blobDomain{}, nil
	}
	defer func() { blobDomainCleanupAcquire = oldAcquire }()

	// The other writer owns the key.
	domain, err := blobDomainAcquire(t.Context(), filepath.Join(blobsDir, sha), dst)
	if err != nil {
		t.Fatal(err)
	}
	defer domain.release()

	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("the negative control did not reproduce the defect: the live partial survived (stat err=%v)", err)
	}
	t.Logf("negative control reproduced the defect: the cleanup deleted the live partial of the other writer")
}

// TestNegativeControlNoDomainWaitReTransfersTheFile is the negative control
// for the wait-and-re-check half of the contract.
//
// It is a real fails-before/passes-after control: the SAME property - "the
// item reuses the published blob instead of transferring the file again" - is
// evaluated under the shipped model and under the historical model that the
// production seam installs.
//
//   - under the SHIPPED model the property HOLDS: the item waits for the
//     blob key's ownership domain, and the blob the owner published while it
//     waited is reused with ZERO additional range GETs;
//   - under the HISTORICAL model (no shared ownership domain for the blob
//     key) the property is VIOLATED: the item never waits for the key and
//     transfers the file again.
//
// A regression of the shipped model fails the first half; a seam that no
// longer reproduces the historical model fails the second half. Neither half
// can pass by accident, and neither half can skip.
//
// The synchronization is deterministic: the shipped half proves that the item
// waits through the production contention hook, and the historical half uses
// the hub's first payload request to know that the item is transferring
// before the owner publishes the blob.
//
// Both halves use the SINGLE-REQUEST route on purpose: it is the route that
// has no claim-level re-check to fall back on, so the post-domain re-check is
// the only thing that can stop the redundant transfer. The multipart route
// checks the publish target before its transfer and would hide a missing
// re-check.
func TestNegativeControlNoDomainWaitReTransfersTheFile(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	t.Run("shipped model reuses the published blob", func(t *testing.T) {
		testNegativeControlShippedModelReusesPublishedBlob(t)
	})
	t.Run("historical model re-transfers the file", func(t *testing.T) {
		testNegativeControlHistoricalModelReTransfersTheFile(t)
	})
}

// testNegativeControlShippedModelReusesPublishedBlob is the "passes-after"
// half of the control: with the shipped model the item waits for the domain
// and reuses the blob the owner published while it waited.
func testNegativeControlShippedModelReusesPublishedBlob(t *testing.T) {
	t.Helper()
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	// The foreign owner holds the blob key's ownership domain.
	owner, err := acquireStagingOwnershipLock(mustDomainDst(t, repo, sha))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.release()

	srv, getRanges := recordingHub(t, full, "model.bin", true)
	// The single-request route: it is the route that has no claim-level
	// re-check to fall back on, so the post-domain re-check is the only
	// thing that can stop the redundant transfer.
	cfg := ownershipContractSettings(cacheDir, srv.URL, "1MB")

	// The probe is installed before the writer starts: the hook is a
	// package-level variable.
	contended := installDomainContentionProbe(t)
	result := make(chan ownershipContractOutcome, 1)
	go func() { result <- runOwnershipContractDownload(context.Background(), cfg) }()

	// The item waits for the domain the owner holds.
	awaitContention(t, contended, "the blob key's ownership domain")

	// The owner publishes the blob and releases the domain.
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
		t.Fatalf("the shipped model issued %d range GETs (%v), want 0: the published blob must be reused",
			len(got), got)
	}
}

// testNegativeControlHistoricalModelReTransfersTheFile is the "fails-before"
// half of the control: with the historical model installed through the
// production seam, the item never waits for the key and transfers the file
// again instead of reusing the published blob.
func testNegativeControlHistoricalModelReTransfersTheFile(t *testing.T) {
	t.Helper()
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	// The foreign owner holds the blob key's ownership domain.
	owner, err := acquireStagingOwnershipLock(mustDomainDst(t, repo, sha))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.release()

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
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// The single-request route, like the shipped half: it is the route that
	// has no claim-level re-check to fall back on.
	cfg := ownershipContractSettings(cacheDir, srv.URL, "1MB")

	// The historical model: every writer keeps its own private ownership, so
	// the second writer never waits for the key.
	oldAcquire := blobDomainAcquire
	blobDomainAcquire = func(context.Context, string, string) (*blobDomain, error) {
		return &blobDomain{}, nil
	}
	defer func() { blobDomainAcquire = oldAcquire }()

	result := make(chan ownershipContractOutcome, 1)
	go func() { result <- runOwnershipContractDownload(context.Background(), cfg) }()

	// The item transfers the file even though the owner holds the key: the
	// historical model has no shared domain to wait for.
	select {
	case <-firstGet:
	case <-time.After(60 * time.Second):
		t.Fatal("the historical model did not reproduce the defect: the item never started its transfer")
	}
	// The owner publishes the blob while the item is transferring.
	if err := os.WriteFile(repo.BlobPath(sha), full, 0o644); err != nil {
		t.Fatal(err)
	}
	owner.release()

	var out ownershipContractOutcome
	select {
	case out = <-result:
	case <-time.After(120 * time.Second):
		t.Fatal("the download did not finish")
	}
	_ = out
	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("the negative control did not reproduce the defect: the historical model issued no range GET, so the scenario proved nothing")
	}
	t.Logf("negative control reproduced the defect: the historical model transferred the file again (%d range GETs) instead of reusing the published blob",
		len(got))
}

// --- the helper process that owns a blob key in ANOTHER process ---

const (
	blobOwnershipChildEnv  = "HFDESK_BLOB_OWNERSHIP_CHILD"
	blobOwnershipConfigEnv = "HFDESK_BLOB_OWNERSHIP_CONFIG"
)

// blobOwnershipChildConfig describes the helper process's work.
type blobOwnershipChildConfig struct {
	DomainDst   string `json:"domain_dst"`
	PartialPath string `json:"partial_path"`
	Payload     string `json:"payload"`
	ReadyPath   string `json:"ready_path"`
	GoPath      string `json:"go_path"`
	// PublishPath is the blob the helper publishes into the key it owns.
	// It is published only after PublishGoPath appears, so the parent can
	// first prove that its own item is already waiting for the domain.
	PublishPath string `json:"publish_path"`
	// PublishPayloadBase64 is the published content. It is base64 encoded
	// because the payload is binary and the configuration is JSON.
	PublishPayloadBase64 string `json:"publish_payload_base64"`
	// PublishGoPath is the file whose appearance asks the helper to publish.
	PublishGoPath string `json:"publish_go_path"`
}

// TestBlobOwnershipChildProcess is the re-executed helper of the ownership
// contract cases: it takes the blob key's ownership domain (the lifetime OS
// lock of the key), writes a live partial and holds both until the parent
// releases it.
//
// When the configuration names a publish target, the helper publishes that
// blob into the key it owns after the parent asks for it (PublishGoPath), and
// it keeps holding the domain until the parent releases the run. The parent
// therefore controls the order: the item under test is already waiting for
// the domain when the blob appears.
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
		// The parent asks for the publication only after its own item is
		// waiting for the domain, so the blob can never predate the wait.
		waitForFile(t, cfg.PublishGoPath, 120*time.Second)
		payload, derr := base64.StdEncoding.DecodeString(cfg.PublishPayloadBase64)
		if derr != nil {
			t.Fatalf("child publish payload: %v", derr)
		}
		if err := os.WriteFile(cfg.PublishPath, payload, 0o644); err != nil {
			t.Fatalf("child publish: %v", err)
		}
	}
	// Hold the domain until the parent releases the run.
	waitForFile(t, cfg.GoPath, 120*time.Second)
}

// startBlobOwnershipChild starts the helper process that owns a blob key.
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

// releaseBlobOwnershipChild releases the helper process.
func releaseBlobOwnershipChild(t *testing.T, cmd *exec.Cmd, goPath string) {
	t.Helper()
	if err := os.WriteFile(goPath, []byte("go"), 0o644); err != nil {
		t.Fatalf("release helper: %v", err)
	}
	_ = cmd.Wait()
}

// waitForFile blocks until path exists (or the timeout expires).
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
