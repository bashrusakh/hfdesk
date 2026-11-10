// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for same-process cache-mode ownership contention:
// many-item downloads must complete at the product's default concurrency,
// and contention between in-process siblings must never be classified or
// surfaced as the foreign/live-owner error class.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const cacheConcurrencyFileCount = 100

// cacheConcurrencyPayload yields unique deterministic bytes per item so every
// item is a distinct content-addressed blob.
func cacheConcurrencyPayload(i int) []byte {
	unit := fmt.Sprintf("hfdesk-cache-concurrency-%04d-", i)
	body := strings.Repeat(unit, 4096/len(unit)+1)
	return []byte(body[:4096])
}

func cacheConcurrencySHA(i int) string {
	sum := sha256.Sum256(cacheConcurrencyPayload(i))
	return hex.EncodeToString(sum[:])
}

// newCacheConcurrencyHub serves count non-LFS files under data/ with known
// sha256 tree entries, one revision and one tree page.
func newCacheConcurrencyHub(t *testing.T, count int) *httptest.Server {
	t.Helper()
	nodes := make([]string, 0, count)
	for i := 0; i < count; i++ {
		rel := fmt.Sprintf("data/file-%04d.bin", i)
		body := cacheConcurrencyPayload(i)
		nodes = append(nodes, fmt.Sprintf(`{"type":"file","path":%q,"size":%d,"sha256":%q}`,
			rel, len(body), cacheConcurrencySHA(i)))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte("[" + strings.Join(nodes, ",") + "]"))
		case strings.Contains(r.URL.Path, "/resolve/") || strings.Contains(r.URL.Path, "/raw/"):
			name := filepath.Base(r.URL.Path)
			var idx int
			if _, err := fmt.Sscanf(name, "file-%04d.bin", &idx); err != nil || idx < 0 || idx >= count {
				http.NotFound(w, r)
				return
			}
			body := cacheConcurrencyPayload(idx)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cacheConcurrencySettings(cacheDir, endpoint string, maxActive int) Settings {
	return Settings{
		CacheDir: cacheDir, Endpoint: endpoint, Concurrency: 8,
		MaxActiveDownloads: maxActive, MultipartThreshold: "1MB",
		Verify: "size", Retries: 0, StallTimeout: "0",
	}
}

// TestCacheManyItemConcurrentDownloadCompletes is the discriminating test for
// the same-process ownership livelock: 100 small files in one repo must all
// complete within a bounded deadline at the product's default concurrency
// levels. Any ownership failure fails the job, so a same-process contention
// error surfacing at all fails this test.
func TestCacheManyItemConcurrentDownloadCompletes(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	for _, maxActive := range []int{3, 8} {
		t.Run(fmt.Sprintf("max-active-%d", maxActive), func(t *testing.T) {
			srv := newCacheConcurrencyHub(t, cacheConcurrencyFileCount)
			cacheDir := t.TempDir()

			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			start := time.Now()
			var doneCount atomic.Int64
			err := Download(ctx, Job{Repo: "owner/model", Revision: "main"},
				cacheConcurrencySettings(cacheDir, srv.URL, maxActive), func(ev ProgressEvent) {
					if ev.Event == "file_done" {
						doneCount.Add(1)
					}
				})
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("%d files at MaxActiveDownloads=%d did not complete within the deadline (after %v, %d/%d items finished): %v",
					cacheConcurrencyFileCount, maxActive, elapsed, doneCount.Load(), cacheConcurrencyFileCount, err)
			}
			t.Logf("completed %d files at MaxActiveDownloads=%d in %v",
				cacheConcurrencyFileCount, maxActive, elapsed)

			// JOB CONTRACT: every requested file is present AND reachable at
			// its snapshot and friendly paths, with the expected bytes.
			repoDir := filepath.Join(cacheDir, "hub", "models--owner--model")
			for i := 0; i < cacheConcurrencyFileCount; i++ {
				rel := fmt.Sprintf("data/file-%04d.bin", i)
				sha := cacheConcurrencySHA(i)
				if got, rerr := os.ReadFile(filepath.Join(repoDir, "blobs", sha)); rerr != nil || string(got) != string(cacheConcurrencyPayload(i)) {
					t.Fatalf("blob for %s: %v", rel, rerr)
				}
				for _, entry := range []string{
					filepath.Join(repoDir, "snapshots", "commit123", filepath.FromSlash(rel)),
					filepath.Join(cacheDir, "models", "owner", "model", filepath.FromSlash(rel)),
				} {
					if _, lerr := os.Lstat(entry); lerr != nil {
						t.Fatalf("reachable entry missing for %s: %v", entry, lerr)
					}
				}
			}
		})
	}
}

// assertNotForeignOwnershipClass is the shared pin: no same-process
// contention error may match the foreign/live-owner error class or blame
// another process.
func assertNotForeignOwnershipClass(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a same-process contention error, got success")
	}
	if errors.Is(err, errStagingOwnershipBusy) {
		t.Errorf("same-process contention surfaced as the external-scope class: %v", err)
	}
	if errors.Is(err, errBlobOwnedByLiveWriter) {
		t.Errorf("same-process contention surfaced as the live-owner class: %v", err)
	}
	if errors.Is(err, errBlobIncompleteOwned) {
		t.Errorf("same-process contention surfaced as the live-writer-claim class: %v", err)
	}
	if strings.Contains(err.Error(), "another process") {
		t.Errorf("same-process contention blamed another process: %v", err)
	}
	if strings.Contains(err.Error(), "external ownership") {
		t.Errorf("same-process contention reported an external ownership scope: %v", err)
	}
}

// TestSameProcessBootstrapContentionIsNotAForeignOwnerError pins the F1b
// classification: when this process holds the shared node-child-bootstrap
// gate, an acquisition that exhausts its bounded wait must surface a
// same-process contention error, never the external/live-owner class that
// would tell the user another process holds the item.
func TestSameProcessBootstrapContentionIsNotAForeignOwnerError(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	cacheDir := t.TempDir()
	srv := newCacheConcurrencyHub(t, 4)

	cache := NewHFCache(cacheDir)
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	// Create the repository directories first so directory admission passes
	// and the download reaches destination-scope acquisition.
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	realm, err := cache.ownershipRealmForData()
	if err != nil {
		t.Fatal(err)
	}

	// Hold the shared child-bootstrap gate of the blobs node — the gate every
	// item's staging leaf node must pass on first creation — the way a
	// same-process sibling creating nodes would. The path must be the
	// realm's canonical spelling: production registers held locks under the
	// EvalSymlinks-resolved node root, so a hand-built spelling would miss
	// the process-local registry and misclassify the contention.
	bootstrapPath := filepath.Join(realm.nodeRoot, "children", "models--owner--model",
		"children", "blobs", "metadata", "child-bootstrap.lock")
	gate, err := ownershipLockOpen(bootstrapPath, ownershipExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.release()

	shortBlobOwnershipWait(t, 300*time.Millisecond)
	dlErr := Download(t.Context(), Job{Repo: "owner/model", Revision: "main"},
		cacheConcurrencySettings(cacheDir, srv.URL, 2), func(ProgressEvent) {})
	if dlErr == nil {
		t.Fatal("the download succeeded although the shared bootstrap gate was held")
	}
	assertNotForeignOwnershipClass(t, dlErr)
	if !errors.Is(dlErr, errStagingOwnershipSameProcess) {
		t.Fatalf("same-process bootstrap contention error = %v, want errStagingOwnershipSameProcess", dlErr)
	}
}

// TestKeyedDomainWaiterTimeoutIsSameProcessContention pins the in-process
// keyed waiter: a second acquisition of a domain this process already holds
// must time out as same-process contention, never as the foreign class (the
// waiter map is process-local by construction).
func TestKeyedDomainWaiterTimeoutIsSameProcessContention(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	sha := cacheConcurrencySHA(0)
	dst := mustDomainDst(t, repo, sha)
	key, ok := cacheDstDomainKey(dst)
	if !ok {
		t.Fatalf("destination %q is not a cache staging name", dst)
	}
	realm, err := repo.cache.ownershipRealmForData()
	if err != nil {
		t.Fatal(err)
	}
	paths := appendPartialOwnershipPaths(nil, dst)
	paths = appendUniqueOwnershipPaths(paths, repo.BlobPath(sha), repo.IncompletePath(sha), repo.IncompleteMetaPath(sha))
	proofPath := repo.BlobPath(sha)

	holder, err := acquireBlobDomainScoped(t.Context(), key, dst, realm, paths, proofPath)
	if err != nil {
		t.Fatalf("hold the domain: %v", err)
	}
	defer holder.release()

	waiter, err := acquireBlobDomainScopedForCleanup(t.Context(), key, dst, realm, paths, proofPath)
	if waiter != nil {
		waiter.release()
	}
	assertNotForeignOwnershipClass(t, err)
	if !errors.Is(err, errStagingOwnershipSameProcess) {
		t.Fatalf("keyed in-process waiter error = %v, want errStagingOwnershipSameProcess", err)
	}
}
