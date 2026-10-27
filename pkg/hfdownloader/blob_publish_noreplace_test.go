// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers both publication tiers: hard-link placement is atomic no-replace;
// the no-hard-link fallback protects only against domain participants. Also
// covers FinalizeBlob's domain participation and fail-closed behavior.

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// forceUnsupportedHardLinks selects the fallback without replacing its other
// production steps.
func forceUnsupportedHardLinks(t *testing.T) {
	t.Helper()
	old := blobLink
	blobLink = func(old, new string) error {
		return &os.LinkError{Op: "link", Old: old, New: new, Err: os.ErrPermission}
	}
	t.Cleanup(func() { blobLink = old })
}

func newPublicationRepo(t *testing.T) *RepoDir {
	t.Helper()
	repo, err := NewHFCache(t.TempDir()).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return repo
}

func acquirePublicationDomain(t *testing.T, repo *RepoDir, sha string) *blobDomain {
	t.Helper()
	dst, err := repo.blobDomainDst(sha)
	if err != nil {
		t.Fatal(err)
	}
	domain, err := acquireBlobDomain(t.Context(), repo.BlobPath(sha), dst)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(domain.release)
	return domain
}

func TestPublicationWithoutHardLinksNeverReplacesTheProvenBlob(t *testing.T) {
	forceUnsupportedHardLinks(t)
	repo := newPublicationRepo(t)

	sha := strings.Repeat("a", 64)
	blobPath := repo.BlobPath(sha)
	winner := []byte("the proven winner")
	loser := []byte("the unproven loser")
	if err := os.WriteFile(blobPath, winner, 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(repo.BlobsDir(), "tmp-"+sha+".part")
	if err := os.WriteFile(src, loser, 0o644); err != nil {
		t.Fatal(err)
	}

	domain := acquirePublicationDomain(t, repo, sha)

	err := blobPublishPlace(src, blobPath, domain)
	if !errors.Is(err, errBlobTargetExists) {
		t.Fatalf("placement over an existing blob without hard links = %v, want errBlobTargetExists", err)
	}
	got, rerr := os.ReadFile(blobPath)
	if rerr != nil || !bytes.Equal(got, winner) {
		t.Fatalf("the proven blob was replaced: %q, %v", got, rerr)
	}
	if _, serr := os.Stat(src); serr != nil {
		t.Errorf("the refused payload was removed by the placement: %v", serr)
	}
}

func TestPublicationWithoutHardLinksPublishesANewBlob(t *testing.T) {
	forceUnsupportedHardLinks(t)
	repo := newPublicationRepo(t)

	sha := strings.Repeat("c", 64)
	blobPath := repo.BlobPath(sha)
	payload := []byte("the payload of the only writer")
	src := filepath.Join(repo.BlobsDir(), "tmp-"+sha+".part")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	domain := acquirePublicationDomain(t, repo, sha)

	if err := blobPublishPlace(src, blobPath, domain); err != nil {
		t.Fatalf("publication without hard links = %v, want success", err)
	}
	got, rerr := os.ReadFile(blobPath)
	if rerr != nil || !bytes.Equal(got, payload) {
		t.Fatalf("the published blob = %q, %v; want the payload's bytes", got, rerr)
	}
	if _, serr := os.Stat(src); serr == nil {
		t.Error("the staging name still exists after the publication")
	}
}

func TestPublicationWithoutHardLinksRefusesWithoutTheDomain(t *testing.T) {
	forceUnsupportedHardLinks(t)
	repo := newPublicationRepo(t)

	sha := strings.Repeat("d", 64)
	blobPath := repo.BlobPath(sha)
	winner := []byte("the proven winner")
	loser := []byte("the unproven loser")
	if err := os.WriteFile(blobPath, winner, 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(repo.BlobsDir(), "tmp-"+sha+".part")
	if err := os.WriteFile(src, loser, 0o644); err != nil {
		t.Fatal(err)
	}

	err := blobPublishPlace(src, blobPath, nil)
	if !errors.Is(err, errBlobPublicationDomainRequired) {
		t.Fatalf("publication without the ownership domain = %v, want errBlobPublicationDomainRequired", err)
	}
	got, rerr := os.ReadFile(blobPath)
	if rerr != nil || !bytes.Equal(got, winner) {
		t.Fatalf("the proven blob was replaced: %q, %v", got, rerr)
	}
}

func mustDomainDst(t *testing.T, repo *RepoDir, sha string) string {
	t.Helper()
	dst, err := repo.blobDomainDst(sha)
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestFinalizeBlobNeverReplacesAnExistingBlob(t *testing.T) {
	repo := newPublicationRepo(t)

	sha := strings.Repeat("b", 64)
	winner := []byte("the proven winner")
	loser := []byte("the unproven loser")
	if err := os.WriteFile(repo.BlobPath(sha), winner, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repo.IncompletePath(sha), loser, 0o644); err != nil {
		t.Fatal(err)
	}

	err := repo.FinalizeBlob(sha)
	if err == nil {
		t.Fatal("FinalizeBlob reported success although the blob already exists")
	}
	if !errors.Is(err, errBlobTargetExists) {
		t.Fatalf("FinalizeBlob error = %v, want the target-exists conflict", err)
	}
	got, rerr := os.ReadFile(repo.BlobPath(sha))
	if rerr != nil || !bytes.Equal(got, winner) {
		t.Fatalf("the proven blob was replaced: %q, %v", got, rerr)
	}
	if _, serr := os.Stat(repo.IncompletePath(sha)); serr != nil {
		t.Errorf("the refused payload was removed: %v", serr)
	}
}

func TestFinalizeBlobParticipatesInTheBlobKeyDomain(t *testing.T) {
	repo := newPublicationRepo(t)

	sha := strings.Repeat("f", 64)
	payload := []byte("the finalized bytes")
	if err := os.WriteFile(repo.IncompletePath(sha), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	holder := acquirePublicationDomain(t, repo, sha)

	domainWait := make(chan struct{}, 1)
	oldHook := blobDomainContentionHook
	var hookOnce sync.Once
	blobDomainContentionHook = func(string) {
		hookOnce.Do(func() { domainWait <- struct{}{} })
	}
	defer func() { blobDomainContentionHook = oldHook }()

	done := make(chan error, 1)
	go func() { done <- repo.FinalizeBlob(sha) }()

	select {
	case <-domainWait:
	case err := <-done:
		t.Fatalf("FinalizeBlob did not wait for the ownership domain: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("FinalizeBlob neither waited for the ownership domain nor finished")
	}

	holder.release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("FinalizeBlob = %v, want success", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("FinalizeBlob never finished")
	}

	got, rerr := os.ReadFile(repo.BlobPath(sha))
	if rerr != nil || !bytes.Equal(got, payload) {
		t.Fatalf("the finalized blob = %q, %v; want the payload's bytes", got, rerr)
	}
}

// newPlainHub serves full-body responses and counts payload GETs.
func newPlainHub(t *testing.T, total int, sha string, body []byte) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte("[" + lfsTreeNode("model.bin", total, sha) + "]"))
		case strings.Contains(r.URL.Path, "/resolve/"):
			if r.Method == http.MethodGet {
				mu.Lock()
				served++
				mu.Unlock()
			}
			w.Header().Set("Content-Length", strconv.Itoa(total))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return served
	}
}

func TestSingleRouteFailsBeforePayloadMutationWhenTheDomainIsUnavailable(t *testing.T) {
	oldAcquire := stagingOwnershipAcquire
	stagingOwnershipAcquire = func(string) (*stagingOwnershipLock, error) {
		return nil, errStagingOSLockUnsupported
	}
	defer func() { stagingOwnershipAcquire = oldAcquire }()

	const total = 4_096
	body := bytes.Repeat([]byte{0x5A}, total)
	sha := singleTestSHA(body)
	cacheDir := t.TempDir()
	hub, served := newPlainHub(t, total, sha, body)

	cfg := Settings{
		CacheDir: cacheDir, Endpoint: hub.URL, Concurrency: 1, MaxActiveDownloads: 1,
		MultipartThreshold: "1MB", Verify: "none", Retries: 0, StallTimeout: "0",
	}
	err := Download(t.Context(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ProgressEvent) {})
	if err == nil {
		t.Fatal("the download succeeded although the ownership domain was unavailable")
	}
	if !strings.Contains(err.Error(), "ownership domain") {
		t.Fatalf("the download error = %v, want an actionable ownership-domain failure", err)
	}
	if n := served(); n != 0 {
		t.Errorf("the single-request route transferred the file %d times although the ownership domain was unavailable", n)
	}

	repo, rerr := NewHFCache(cacheDir).Repo("owner/model", RepoTypeModel)
	if rerr != nil {
		t.Fatal(rerr)
	}
	// The single-request route would stage at blobs/tmp-<sha>.part.
	if _, serr := os.Stat(filepath.Join(repo.BlobsDir(), "tmp-"+sha+".part")); serr == nil {
		t.Error("the single-request route wrote a payload although the ownership domain was unavailable")
	}
	if _, serr := os.Stat(repo.BlobPath(sha)); serr == nil {
		t.Error("the blob was published although the ownership domain was unavailable")
	}
}

func TestSingleRouteFailsBeforePayloadMutationWithADeniedOwnershipLock(t *testing.T) {
	oldAcquire := stagingOwnershipAcquire
	denied := errors.New("injected ownership lock permission denied")
	stagingOwnershipAcquire = func(string) (*stagingOwnershipLock, error) { return nil, denied }
	defer func() { stagingOwnershipAcquire = oldAcquire }()

	const total = 4_096
	body := bytes.Repeat([]byte{0x33}, total)
	sha := singleTestSHA(body)
	cacheDir := t.TempDir()
	hub, served := newPlainHub(t, total, sha, body)

	cfg := Settings{
		CacheDir: cacheDir, Endpoint: hub.URL, Concurrency: 1, MaxActiveDownloads: 1,
		MultipartThreshold: "1MB", Verify: "none", Retries: 0, StallTimeout: "0",
	}
	err := Download(t.Context(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ProgressEvent) {})
	if err == nil {
		t.Fatal("the download succeeded although the ownership lock was denied")
	}
	if !strings.Contains(err.Error(), "ownership domain") {
		t.Fatalf("the download error = %v, want an actionable ownership-domain failure", err)
	}
	if n := served(); n != 0 {
		t.Errorf("the single-request route transferred the file %d times although the ownership lock was denied", n)
	}

	repo, rerr := NewHFCache(cacheDir).Repo("owner/model", RepoTypeModel)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if _, serr := os.Stat(filepath.Join(repo.BlobsDir(), "tmp-"+sha+".part")); serr == nil {
		t.Error("the single-request route wrote a payload although the ownership lock was denied")
	}
}

func TestSingleRouteKeepsTheEarlierPartialWhenTheDomainIsUnavailable(t *testing.T) {
	oldAcquire := stagingOwnershipAcquire
	stagingOwnershipAcquire = func(string) (*stagingOwnershipLock, error) {
		return nil, errStagingOSLockUnsupported
	}
	defer func() { stagingOwnershipAcquire = oldAcquire }()

	const total = 4_096
	body := bytes.Repeat([]byte{0x77}, total)
	sha := singleTestSHA(body)
	cacheDir := t.TempDir()
	hub, served := newPlainHub(t, total, sha, body)

	repo, err := NewHFCache(cacheDir).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(repo.BlobsDir(), "tmp-"+sha+".part")
	earlier := []byte("the earlier run's bytes")
	if err := os.WriteFile(partial, earlier, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := Settings{
		CacheDir: cacheDir, Endpoint: hub.URL, Concurrency: 1, MaxActiveDownloads: 1,
		MultipartThreshold: "1MB", Verify: "none", Retries: 0, StallTimeout: "0",
	}
	if err := Download(t.Context(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ProgressEvent) {}); err == nil {
		t.Fatal("the download succeeded although the ownership domain was unavailable")
	}
	if n := served(); n != 0 {
		t.Errorf("the single-request route transferred the file %d times although the ownership domain was unavailable", n)
	}

	got, rerr := os.ReadFile(partial)
	if rerr != nil || !bytes.Equal(got, earlier) {
		t.Fatalf("the earlier partial = %q, %v; want it preserved", got, rerr)
	}
}

func TestPublishedBlobSurvivesTheDomainExclusiveTierEndToEnd(t *testing.T) {
	forceUnsupportedHardLinks(t)

	const total = 4_096
	body := bytes.Repeat([]byte{0xA1}, total)
	sha := singleTestSHA(body)
	cacheDir := t.TempDir()
	hub, _ := newPlainHub(t, total, sha, body)

	cfg := Settings{
		CacheDir: cacheDir, Endpoint: hub.URL, Concurrency: 1, MaxActiveDownloads: 1,
		MultipartThreshold: "1MB", Verify: "none", Retries: 0, StallTimeout: "0",
	}
	if err := Download(t.Context(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
		t.Fatalf("the download failed: %v", err)
	}

	repo, err := NewHFCache(cacheDir).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	got, rerr := os.ReadFile(repo.BlobPath(sha))
	if rerr != nil {
		t.Fatalf("the blob was not published: %v", rerr)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("the published blob holds %d bytes, want the transferred %d bytes", len(got), len(body))
	}
	if got := singleTestSHA(got); got != sha {
		t.Fatalf("the published blob hashes to %s, want %s", got, sha)
	}
}

func TestPlacementRejectsAForeignDomain(t *testing.T) {
	forceUnsupportedHardLinks(t)
	repo := newPublicationRepo(t)

	sha := strings.Repeat("1", 64)
	other := strings.Repeat("2", 64)
	blobPath := repo.BlobPath(sha)
	src := filepath.Join(repo.BlobsDir(), "tmp-"+sha+".part")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	foreign := acquirePublicationDomain(t, repo, other)
	err := blobPublishPlace(src, blobPath, foreign)
	if !errors.Is(err, errBlobPublicationDomainRequired) {
		t.Fatalf("publication under a foreign domain = %v, want errBlobPublicationDomainRequired", err)
	}
	if _, serr := os.Stat(blobPath); serr == nil {
		t.Error("the blob was published under a foreign domain")
	}
}

func TestPublicationWithoutHardLinksReportsTheLinkFailure(t *testing.T) {
	forceUnsupportedHardLinks(t)
	repo := newPublicationRepo(t)

	sha := strings.Repeat("3", 64)
	blobPath := repo.BlobPath(sha)
	src := filepath.Join(repo.BlobsDir(), "tmp-"+sha+".part")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := blobPublishPlace(src, blobPath, nil)
	if err == nil {
		t.Fatal("the placement published a blob without the ownership domain")
	}
	if !strings.Contains(err.Error(), "ownership domain") {
		t.Fatalf("the placement error = %v, want an actionable ownership-domain failure", err)
	}
	if _, serr := os.Stat(blobPath); serr == nil {
		t.Error("the blob was published although the placement could not prove it would not replace one")
	}
	_ = fmt.Sprint(err)
}
