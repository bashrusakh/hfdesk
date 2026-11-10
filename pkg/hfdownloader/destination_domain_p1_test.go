// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers fail-closed claims and external realm ownership.

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

// A nil, released, or non-covering destination domain must refuse the claim
// without changing its record or payload; the claim acquires no domain itself.
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
	if err := claim(acquireTestDomain(t, filepath.Join(dir, "other.bin"))); err == nil {
		t.Fatal("claim with a non-covering destination ownership domain succeeded")
	}
	released := acquireTestStagingDomain(t, staging)
	released.release()
	if err := claim(released); err == nil {
		t.Fatal("claim with a released destination ownership domain succeeded")
	}

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

// The transfer holds its external item scope and creates no payload-side lock.
func TestCacheTransferHoldsExternalDestinationScope(t *testing.T) {
	if !ownershipOSLockAvailable {
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
	realm, err := repo.cache.ownershipRealmForData()
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
			if r.Header.Get("X-HFDesk-Addressing-Probe") == "1" {
				http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
				return
			}
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

	busy, berr := realm.probeWriter(context.Background(), []string{repo.BlobPath(sha), repo.IncompletePath(sha), domainDst})
	if berr != nil || !busy {
		t.Errorf("external item scope while transferring = (%v, %v), want a held scope", busy, berr)
	}
	obsoleteLock := repo.IncompletePath(sha) + stagingLockSuffix
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

	blobsDir := repo.BlobsDir()
	names := dirEntryNames(t, blobsDir)
	want := map[string]bool{
		sha: true,
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

// A legacy lock-fenced record is current only when its realm/key proof matches.
func TestCheckBlobProbesTheDestinationDomain(t *testing.T) {
	if !ownershipOSLockAvailable {
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
	realm, err := repo.cache.ownershipRealmForData()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 64)
	staging := repo.IncompletePath(sha)
	if err := os.WriteFile(staging, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{
		PID: os.Getpid(), Instance: "foreign-instance", Host: processHostname,
		StartedAt: time.Now().UTC(), Size: 7, Version: stagingMetaVersion,
		Repo: "owner/model", Path: "model.bin", Ranges: buildRangeLayout(7, 1), LockFenced: true,
	}
	meta.OwnershipFence, err = realm.fenceProof(repo.BlobPath(sha))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeStagingMeta(repo.IncompleteMetaPath(sha), &meta); err != nil {
		t.Fatal(err)
	}

	// Negative control: a legacy payload-side .lock residue file (left in the
	// wild by old versions) is inert and is not current ownership.
	obsolete := staging + stagingLockSuffix
	if err := os.WriteFile(obsolete, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	status, _, cerr := repo.CheckBlob(sha)
	if cerr != nil || status == BlobDownloading {
		t.Fatalf("CheckBlob while only the legacy payload-side lock residue exists = %v (err=%v), want not-downloading", status, cerr)
	}
	// A fenced record with a free destination domain is reported stale.
	if status != BlobStale {
		t.Errorf("status with a free destination domain = %v, want BlobStale", status)
	}

	domainDst, err := repo.blobDomainDst(sha)
	if err != nil {
		t.Fatal(err)
	}
	domain, err := acquireBlobDomainScoped(context.Background(), repo.BlobPath(sha), domainDst, realm,
		[]string{repo.BlobPath(sha), staging, repo.IncompleteMetaPath(sha), domainDst}, repo.BlobPath(sha))
	if err != nil {
		t.Fatal(err)
	}
	status, _, cerr = repo.CheckBlob(sha)
	domain.release()
	if cerr != nil || status != BlobDownloading {
		t.Fatalf("CheckBlob while the destination domain is held = %v (err=%v), want BlobDownloading", status, cerr)
	}
	status, _, cerr = repo.CheckBlob(sha)
	if cerr != nil || status != BlobStale {
		t.Fatalf("CheckBlob after releasing the destination domain = %v (err=%v), want BlobStale", status, cerr)
	}
}
