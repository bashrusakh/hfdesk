// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Focused regression coverage for the ONE-destination-domain ownership model
// (issue #140 / PR #151, P1 simplification):
//
//   - the multipart claim refuses to run without a held domain that covers its
//     staging path (no production-accessible fallback);
//   - a cache transfer holds ONLY the destination domain lock
//     (blobs/tmp-<sha>.lock): the obsolete <staging>.lock fence is never taken;
//   - CheckBlob interprets the destination domain lock, not the old staging
//     lock, as the live-writer signal.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestClaimStagingRequiresCoveringDomain pins the claim's fail-closed
// precondition: the claim does not take a lock of its own any more, so it must
// be handed the destination domain that fences its staging path. A nil,
// released or non-covering domain mutates nothing.
func TestClaimStagingRequiresCoveringDomain(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "guard.bin.incomplete")
	metaPath := staging + ".meta"
	pub := filepath.Join(dir, "guard.bin")
	payload := []byte("a resumable prefix")
	if err := os.WriteFile(staging, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{
		PID: os.Getpid(), Instance: processInstanceToken, Host: processHostname,
		StartedAt: time.Now().UTC(), Size: int64(len(payload)), Version: stagingMetaVersion,
		Repo: "o/r", Path: "guard.bin", LockFenced: true,
		Ranges: buildRangeLayout(int64(len(payload)), 1),
	}
	if err := writeStagingMeta(metaPath, &meta); err != nil {
		t.Fatal(err)
	}
	beforeMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	it := PlanItem{RelativePath: "guard.bin", Size: int64(len(payload))}
	job := Job{Repo: "o/r"}
	cfg := Settings{Verify: "none"}

	claim := func(domain *blobDomain) error {
		t.Helper()
		_, err := claimStaging(context.Background(), staging, pub, metaPath, it, job, "commit", true, cfg, false, domain)
		return err
	}

	if err := claim(nil); err == nil {
		t.Fatal("claim without a destination ownership domain succeeded")
	}
	// A held domain of ANOTHER destination does not cover this staging path.
	if err := claim(acquireTestDomain(t, filepath.Join(dir, "other.bin"))); err == nil {
		t.Fatal("claim with a non-covering destination ownership domain succeeded")
	}
	// A released domain fences nothing.
	released := acquireTestStagingDomain(t, staging)
	released.release()
	if err := claim(released); err == nil {
		t.Fatal("claim with a released destination ownership domain succeeded")
	}

	// None of the refusals may have mutated the pair.
	got, rerr := os.ReadFile(staging)
	if rerr != nil || !bytes.Equal(got, payload) {
		t.Errorf("the refused claims changed the staging payload: %q, %v", got, rerr)
	}
	afterMeta, rerr := os.ReadFile(metaPath)
	if rerr != nil || !bytes.Equal(afterMeta, beforeMeta) {
		t.Errorf("the refused claims changed the staging record: err=%v", rerr)
	}
	if _, serr := os.Stat(pub); !os.IsNotExist(serr) {
		t.Errorf("a refused claim published the destination, stat err=%v", serr)
	}
}

// TestCacheTransferHoldsOnlyTheDestinationDomainLock pins the single fence of
// a cache item: while the multipart transfer runs, the destination domain lock
// blobs/tmp-<sha>.lock is held and the obsolete blobs/<sha>.incomplete.lock is
// never created. After the publish only the persistent domain lock remains.
func TestCacheTransferHoldsOnlyTheDestinationDomainLock(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo, err := NewHFCache(cacheDir).Repo("o/r", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	domainDst, err := repo.blobDomainDst(sha)
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	gate := make(chan struct{})
	var enterOnce, gateOnce sync.Once
	node := lfsTreeNode("model.bin", total, sha)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte("[" + node + "]"))
		case strings.Contains(r.URL.Path, "/resolve/"):
			if r.Method == http.MethodGet {
				enterOnce.Do(func() { close(entered) })
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		gateOnce.Do(func() { close(gate) })
		srv.Close()
	})

	cfg := Settings{
		CacheDir: cacheDir, Endpoint: srv.URL, Concurrency: 1, MaxActiveDownloads: 1,
		MultipartThreshold: "0", Verify: "size", Retries: 0, StallTimeout: "0",
	}
	done := make(chan error, 1)
	go func() {
		done <- Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {})
	}()

	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("the transfer finished before the gate opened: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the transfer never started")
	}

	// The destination domain lock is held for the whole transfer.
	if busy, berr := stagingOwnershipBusy(domainDst); berr != nil || !busy {
		t.Errorf("destination domain lock while transferring = (%v, %v), want a held lock", busy, berr)
	}
	// The obsolete second fence is never created.
	obsoleteLock := stagingOwnershipLockPath(repo.IncompletePath(sha))
	if _, serr := os.Stat(obsoleteLock); !os.IsNotExist(serr) {
		t.Errorf("the obsolete staging lock %s exists; the claim must not take a second fence (stat err=%v)",
			filepath.Base(obsoleteLock), serr)
	}

	gateOnce.Do(func() { close(gate) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Download: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Download did not finish after the gate opened")
	}
	assertExactFile(t, repo.BlobPath(sha), full)

	// A successful publish leaves the destination and the persistent domain
	// lock; the obsolete staging lock still does not exist.
	blobsDir := repo.BlobsDir()
	names := dirEntryNames(t, blobsDir)
	want := map[string]bool{
		sha:                    true,
		"tmp-" + sha + ".lock": true,
	}
	for _, name := range names {
		if !want[name] {
			t.Errorf("the blobs directory holds the unexpected artifact %q; want only the published blob and the destination domain lock", name)
		}
	}
	if _, serr := os.Stat(obsoleteLock); !os.IsNotExist(serr) {
		t.Errorf("the obsolete staging lock %s was created, stat err=%v", filepath.Base(obsoleteLock), serr)
	}
}

// TestCheckBlobProbesTheDestinationDomain is the discriminating control for
// the status query: the live-writer signal is the destination domain lock
// blobs/tmp-<sha>.lock. Holding the OLD <staging>.lock proves nothing and must
// not report a live writer; holding the domain does.
func TestCheckBlobProbesTheDestinationDomain(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	cache := NewHFCache(t.TempDir())
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 64)
	staging := repo.IncompletePath(sha)
	if err := os.WriteFile(staging, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{
		PID: os.Getpid(), Instance: processInstanceToken, Host: processHostname,
		StartedAt: time.Now().UTC(), Size: 7, Version: stagingMetaVersion,
		Repo: "owner/model", Path: "model.bin", Ranges: buildRangeLayout(7, 1), LockFenced: true,
	}
	if err := writeStagingMeta(repo.IncompleteMetaPath(sha), &meta); err != nil {
		t.Fatal(err)
	}

	// Negative control: the obsolete <staging>.lock is not ownership.
	obsolete, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	status, _, cerr := repo.CheckBlob(sha)
	obsolete.release()
	if cerr != nil || status == BlobDownloading {
		t.Fatalf("CheckBlob while only the obsolete staging lock is held = %v (err=%v), want not-downloading", status, cerr)
	}
	// A lock-fenced record plus a free DOMAIN is stale and resumable.
	if status != BlobStale {
		t.Errorf("status with a free destination domain = %v, want BlobStale", status)
	}

	// The destination domain is the signal: holding it reports a live writer.
	domain, err := acquireStagingOwnershipLock(mustDomainDst(t, repo, sha))
	if err != nil {
		t.Fatal(err)
	}
	status, _, cerr = repo.CheckBlob(sha)
	domain.release()
	if cerr != nil || status != BlobDownloading {
		t.Fatalf("CheckBlob while the destination domain is held = %v (err=%v), want BlobDownloading", status, cerr)
	}
}
