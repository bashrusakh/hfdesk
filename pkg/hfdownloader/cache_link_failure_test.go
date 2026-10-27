// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// The cache entry link contract (issue #140 / PR #151).
//
// A cached file is reachable through its snapshot entry (and its friendly
// entry). The transfer path always treated a failed link creation as fatal
// for the item; the two SKIP paths swallowed the error and reported the item
// as a successful skip, so a job could report success while the requested
// path was not reachable at all.
//
// The skip paths and the end-of-job reconciliation now agree with the
// transfer path: a link-creation failure fails the item (or the job) with an
// actionable error, and a job that reports success means the requested files
// are present AND reachable at their snapshot/friendly paths.
//
// The friendly view is deliberately absent when Settings.NoFriendlyView is
// set, so that is not an error.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// breakSnapshotEntry makes the snapshot entry of a commit impossible to
// create: the commit's snapshot directory is a regular file, so the parent
// directory of the entry cannot be created.
//
// The failure is deterministic and portable: it does not depend on
// permissions, on a read-only filesystem or on symlink support.
func breakSnapshotEntry(t *testing.T, repo *RepoDir, commit string) {
	t.Helper()
	dir := filepath.Join(repo.SnapshotsDir(), commit)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// publishBlob writes a published content-addressed blob, so the pre-download
// skip check finds it.
func publishBlob(t *testing.T, repo *RepoDir, sha string, content []byte) {
	t.Helper()
	if err := os.WriteFile(repo.BlobPath(sha), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPreDownloadSkipLinkFailureFailsTheItem covers the pre-download "blob
// exists" skip: the payload is present, but the requested path cannot be
// linked, so the item must FAIL instead of reporting a successful skip.
func TestPreDownloadSkipLinkFailureFailsTheItem(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	publishBlob(t, repo, sha, full)
	// The snapshot entry of this commit cannot be created.
	breakSnapshotEntry(t, repo, "commit123")

	srv, _ := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")
	out := runOwnershipContractDownload(context.Background(), cfg)

	if out.err == nil {
		t.Fatalf("the job reported success although the requested path is not reachable (skips=%v)", out.skips)
	}
	if !errors.Is(out.err, ErrCacheLinkUnreachable) {
		t.Errorf("Download error = %v, want the unreachable-cache-path error", out.err)
	}
	if !containsAll(out.err.Error(), "model.bin", "snapshot entry") {
		t.Errorf("Download error = %v, want the item path and the failing entry in the message", out.err)
	}
	if len(out.skips) != 0 {
		t.Errorf("skips = %v, want none: the requested path is not reachable", out.skips)
	}
	if len(out.done) != 0 {
		t.Errorf("file_done events = %v, want none: the requested path is not reachable", out.done)
	}
}

// TestPostDomainSkipLinkFailureFailsTheItem covers the post-domain "blob
// exists" skip: the blob was published while the item waited for the
// ownership domain, and the item's own link creation fails.
func TestPostDomainSkipLinkFailureFailsTheItem(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	// A live owner holds the blob key's ownership domain, so the item has to
	// wait for it before it can re-check the published target.
	owner, err := acquireStagingOwnershipLock(mustDomainDst(t, repo, sha))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.release()

	srv, _ := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")

	// The probe is installed before the writer starts: the hook is a
	// package-level variable.
	contended := installDomainContentionProbe(t)
	result := make(chan ownershipContractOutcome, 1)
	go func() { result <- runOwnershipContractDownload(context.Background(), cfg) }()
	awaitContention(t, contended, "the blob key's ownership domain")

	// The owner publishes the blob and releases the domain. The item then
	// takes the domain, finds the published blob and takes the post-domain
	// skip path.
	publishBlob(t, repo, sha, full)
	owner.release()

	// The snapshot entry of this commit cannot be created.
	breakSnapshotEntry(t, repo, "commit123")

	var out ownershipContractOutcome
	select {
	case out = <-result:
	case <-time.After(120 * time.Second):
		t.Fatal("the download did not finish after the owner released the domain")
	}
	if out.err == nil {
		t.Fatalf("the job reported success although the requested path is not reachable (skips=%v)", out.skips)
	}
	if !errors.Is(out.err, ErrCacheLinkUnreachable) {
		t.Errorf("Download error = %v, want the unreachable-cache-path error", out.err)
	}
	if len(out.skips) != 0 {
		t.Errorf("skips = %v, want none: the requested path is not reachable", out.skips)
	}
	if len(out.done) != 0 {
		t.Errorf("file_done events = %v, want none: the requested path is not reachable", out.done)
	}
}

// TestSkipWithWorkingLinksKeepsTheSkipBehavior is the control case: when the
// links can be created, the pre-download skip keeps its historical behavior -
// the item is skipped, the job succeeds, and the requested path is reachable.
func TestSkipWithWorkingLinksKeepsTheSkipBehavior(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	publishBlob(t, repo, sha, full)

	srv, gets := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")
	out := runOwnershipContractDownload(context.Background(), cfg)

	if out.err != nil {
		t.Fatalf("Download: %v", out.err)
	}
	if len(out.skips) != 1 || !containsAll(out.skips[0], "blob exists") {
		t.Fatalf("skips = %v, want exactly one 'blob exists' skip", out.skips)
	}
	if got := gets(); len(got) != 0 {
		t.Errorf("the skipped item issued %d range GETs (%v), want 0", len(got), got)
	}
	// The requested path is reachable through its entries.
	assertOwnershipContractLinks(t, cacheDir, "model.bin")
}

// TestSkipWithDisabledFriendlyViewIsNotALinkFailure pins the
// NoFriendlyView exception: the friendly entry is deliberately absent in that
// mode, so a missing friendly view is not an error.
func TestSkipWithDisabledFriendlyViewIsNotALinkFailure(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	publishBlob(t, repo, sha, full)

	srv, _ := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")
	cfg.NoFriendlyView = true
	out := runOwnershipContractDownload(context.Background(), cfg)

	if out.err != nil {
		t.Fatalf("Download: %v", out.err)
	}
	if len(out.skips) != 1 {
		t.Fatalf("skips = %v, want exactly one skip", out.skips)
	}
	// The snapshot entry exists; the friendly entry is deliberately absent.
	snap := filepath.Join(repo.SnapshotsDir(), "commit123", "model.bin")
	if _, err := os.Lstat(snap); err != nil {
		t.Errorf("the snapshot entry %s is missing: %v", snap, err)
	}
	friendly := filepath.Join(cacheDir, "models", "owner", "model", "model.bin")
	if _, err := os.Lstat(friendly); !os.IsNotExist(err) {
		t.Errorf("the friendly entry exists although the friendly view is disabled, stat err=%v", err)
	}
}

// TestReconciliationLinkFailureFailsTheJob covers the end-of-job
// reconciliation: a link it cannot create leaves the requested path
// unreachable, so the job must fail instead of only warning about it.
func TestReconciliationLinkFailureFailsTheJob(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	publishBlob(t, repo, sha, full)

	// The item's own skip creates the links; the reconciliation - the second
	// link creation of this run - fails.
	var calls atomic.Int32
	oldCreate := cacheEntryLinkCreate
	cacheEntryLinkCreate = func(ctx context.Context, r *RepoDir, commit, rel, filterSubdir, blobSHA string, noFriendly bool) error {
		if calls.Add(1) > 1 {
			return fmt.Errorf("%w: the snapshot entry of %s could not be created: %w", ErrCacheLinkUnreachable, rel, errors.New("the snapshot directory vanished"))
		}
		return oldCreate(ctx, r, commit, rel, filterSubdir, blobSHA, noFriendly)
	}
	defer func() { cacheEntryLinkCreate = oldCreate }()

	srv, _ := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")
	out := runOwnershipContractDownload(context.Background(), cfg)

	if out.err == nil {
		t.Fatalf("the job reported success although the reconciliation could not create the link (skips=%v)", out.skips)
	}
	if !errors.Is(out.err, ErrCacheLinkUnreachable) {
		t.Errorf("Download error = %v, want the unreachable-cache-path error", out.err)
	}
}

// TestSameSHADifferentPathItemGetsItsOwnLinks covers the shared-blob
// reconciliation: two plan items with the same digest but different paths
// share ONE blob, and each of them ends with its OWN snapshot and friendly
// entries, so a shared blob never leaves a plan path without cache entries.
//
// The second item must reuse the published blob (no second transfer) and its
// own entries must exist.
func TestSameSHADifferentPathItemGetsItsOwnLinks(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	// Two files with the SAME digest: the blob is shared, the paths differ.
	node := func(path string) string {
		return fmt.Sprintf(`{"type":"file","path":%q,"size":%d,"sha256":%q}`, path, len(full), sha)
	}
	var (
		mu     sync.Mutex
		ranges []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte("[" + node("a.bin") + "," + node("b.bin") + "]"))
		case strings.Contains(r.URL.Path, "/resolve/") || strings.Contains(r.URL.Path, "/raw/"):
			if r.Method == http.MethodGet {
				mu.Lock()
				ranges = append(ranges, r.Header.Get("Range"))
				mu.Unlock()
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := ownershipContractSettings(cacheDir, srv.URL, "1MB")
	out := runOwnershipContractDownload(context.Background(), cfg)
	if out.err != nil {
		t.Fatalf("Download: %v", out.err)
	}

	// The blob is shared and holds the exact bytes.
	assertExactFile(t, repo.BlobPath(sha), full)
	// Both plan paths have their own entries.
	for _, rel := range []string{"a.bin", "b.bin"} {
		assertOwnershipContractLinks(t, cacheDir, rel)
	}
	// The second item reused the published blob: only one transfer happened.
	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	if len(got) != 1 {
		t.Errorf("the two items issued %d payload GETs (%v), want exactly 1: the shared blob must be reused", len(got), got)
	}
}

// TestNegativeControlSwallowedLinkFailureReportsSuccess is the negative
// control for the link contract.
//
// It installs the historical behavior through the production seam - a
// swallowed link failure - and shows that the job reports success although
// the requested path is not reachable. That is the defect the link contract
// removes, and it proves that the link regressions really detect it.
func TestNegativeControlSwallowedLinkFailureReportsSuccess(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	publishBlob(t, repo, sha, full)
	breakSnapshotEntry(t, repo, "commit123")

	// The historical model: the link failure is swallowed.
	oldCreate := cacheEntryLinkCreate
	cacheEntryLinkCreate = func(ctx context.Context, r *RepoDir, commit, rel, filterSubdir, blobSHA string, noFriendly bool) error {
		return nil
	}
	defer func() { cacheEntryLinkCreate = oldCreate }()

	srv, _ := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")
	out := runOwnershipContractDownload(context.Background(), cfg)

	if out.err != nil {
		t.Fatalf("the negative control did not reproduce the defect: the job failed instead of reporting success: %v", out.err)
	}
	if len(out.skips) != 1 {
		t.Fatalf("the negative control did not reproduce the defect: the job did not report a successful skip: %v", out.skips)
	}
	t.Logf("negative control reproduced the defect: the job reported success although the requested path is not reachable")
}

// TestTransferPathLinkFailureCarriesTheUnreachableSentinel covers the
// transfer paths: a link-creation failure there must carry the SAME exported
// ErrCacheLinkUnreachable sentinel as the skip paths, so the failure is
// uniform on every path that creates the cache entries.
//
// The two transfer paths are covered:
//   - the direct blob path (an LFS blob with a canonical SHA-256: the ranged
//     route publishes blobs/<sha> and the downloader creates the entries);
//   - the store path (a plain file: the downloader stores the payload through
//     the cache store, which creates the entries).
//
// In both cases the payload IS in blobs/ while the requested path is not
// reachable, so the item must fail instead of reporting a success.
func TestTransferPathLinkFailureCarriesTheUnreachableSentinel(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	for _, tc := range []struct {
		name string
		lfs  bool
	}{
		{name: "direct blob path", lfs: true},
		{name: "store path", lfs: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cacheDir := t.TempDir()
			repo := ownershipContractRepo(t, cacheDir)
			// The snapshot entry of this commit cannot be created, so the
			// transfer publishes the payload and then fails on the link.
			breakSnapshotEntry(t, repo, "commit123")

			srv, _ := recordingHub(t, full, "model.bin", tc.lfs)
			cfg := ownershipContractSettings(cacheDir, srv.URL, "0")
			out := runOwnershipContractDownload(context.Background(), cfg)

			if out.err == nil {
				t.Fatalf("the job reported success although the requested path is not reachable (done=%v)", out.done)
			}
			if !errors.Is(out.err, ErrCacheLinkUnreachable) {
				t.Errorf("Download error = %v, want the unreachable-cache-path error", out.err)
			}
			if !containsAll(out.err.Error(), "model.bin", "snapshot entry") {
				t.Errorf("Download error = %v, want the item path and the failing entry in the message", out.err)
			}
			if len(out.done) != 0 {
				t.Errorf("file_done events = %v, want none: the requested path is not reachable", out.done)
			}
			// The payload is in the cache; only the entry is missing.
			if _, err := os.Stat(repo.BlobPath(sha)); err != nil {
				t.Errorf("the published blob is missing: %v", err)
			}
		})
	}
}

// containsAll reports whether s contains every one of the parts.
func containsAll(s string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(s, part) {
			return false
		}
	}
	return true
}
