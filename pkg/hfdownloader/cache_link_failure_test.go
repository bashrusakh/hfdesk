// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers link failures in transfer, skip, and reconciliation paths. A missing
// friendly entry is valid when NoFriendlyView is enabled.

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

// failSnapshotEntryLink injects an actual link-creation error after ownership
// admission, keeping the failure regression focused on the link operation.
func failSnapshotEntryLink(t *testing.T) {
	t.Helper()
	oldCreate := cacheEntryLinkCreate
	cacheEntryLinkCreate = func(_ context.Context, _ *RepoDir, _, relativePath, _, _ string, _ bool) error {
		return fmt.Errorf("%w: the snapshot entry of %s could not be created: injected link failure", ErrCacheLinkUnreachable, relativePath)
	}
	t.Cleanup(func() { cacheEntryLinkCreate = oldCreate })
}

func publishBlob(t *testing.T, repo *RepoDir, sha string, content []byte) {
	t.Helper()
	if err := os.WriteFile(repo.BlobPath(sha), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// An existing blob is not a successful skip if its snapshot entry cannot be created.
func TestPreDownloadSkipLinkFailureFailsTheItem(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	publishBlob(t, repo, sha, full)
	failSnapshotEntryLink(t)

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

	// Hold the blob domain so the item must wait before rechecking publication.
	owner := acquireTestDomainFresh(t, mustDomainDst(t, repo, sha))

	srv, _ := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")

	// The probe and link failure are installed before the writer starts: both
	// hooks are package-level variables.
	contended := installBlobDomainContentionProbe(t)
	failSnapshotEntryLink(t)
	result := make(chan ownershipContractOutcome, 1)
	go func() { result <- runOwnershipContractDownload(context.Background(), cfg) }()
	awaitContention(t, contended, "the blob key's ownership domain")

	// Publish before releasing the domain to exercise the post-domain skip.
	publishBlob(t, repo, sha, full)
	owner.release()

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

// A reachable cached item still skips successfully without another request.
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
	assertOwnershipContractLinks(t, cacheDir, "model.bin")
}

// NoFriendlyView deliberately omits the friendly entry; the snapshot remains required.
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

// A reconciliation link failure must fail the job, not merely warn.
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

// Same-SHA items share one blob but need separate snapshot and friendly entries.
// The second item must reuse the blob without another transfer.
func TestSameSHADifferentPathItemGetsItsOwnLinks(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

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
			if r.Header.Get("X-HFDesk-Addressing-Probe") == "1" {
				http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
				return
			}
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

	assertExactFile(t, repo.BlobPath(sha), full)
	for _, rel := range []string{"a.bin", "b.bin"} {
		assertOwnershipContractLinks(t, cacheDir, rel)
	}
	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	if len(got) != 1 {
		t.Errorf("the two items issued %d payload GETs (%v), want exactly 1: the shared blob must be reused", len(got), got)
	}
}

// Both the direct ranged-blob route and the plain-file cache-store route must
// return ErrCacheLinkUnreachable when publication succeeds but link creation fails.
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
			failSnapshotEntryLink(t)

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

func containsAll(s string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(s, part) {
			return false
		}
	}
	return true
}
