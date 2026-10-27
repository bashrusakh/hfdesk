// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the no-replace publication tiers and for the
// writers of a content-addressed blob (issue #140 / PR #151).
//
// The publication primitive has two tiers, and they guarantee DIFFERENT
// things:
//
//   - ATOMIC NO-REPLACE: the payload is placed with a hard link, which
//     creates the final name and fails when the name already exists. The
//     existence decision and the placement are ONE filesystem operation.
//   - DOMAIN-EXCLUSIVE: on a filesystem that cannot create hard links there
//     is no standard-library atomic no-replace rename (os.Rename replaces on
//     Unix and on Windows), so the placement is a check-then-rename performed
//     while the blob key's ownership domain is held exclusively. The property
//     that holds there is "an existing blobs/<sha> is never replaced by a
//     DOMAIN PARTICIPANT", not "never replaced" unconditionally.
//
// The defect this file pins: the fallback tier shipped a Stat-then-rename
// that REPLACED the target whenever it appeared between the stat and the
// rename, while the documentation claimed the fallback "still refuses to
// replace an existing target". The link seam below makes that branch
// reachable from a committed test, which is what no shipped test could do
// before.
//
// The file also pins:
//   - FinalizeBlob, an exported writer of blobs/<sha>, no longer replaces an
//     existing content-addressed blob and participates in the blob key's
//     ownership domain;
//   - the single-request route fails BEFORE it writes a payload when the
//     lifetime ownership of the blob key is unavailable.

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

// forceUnsupportedHardLinks makes the placement take its domain-exclusive
// tier: the filesystem "cannot create hard links".
//
// The seam is narrow on purpose - it replaces the link operation only, so the
// rest of the placement (the existence check, the rename, the domain
// requirement) is the shipped code.
func forceUnsupportedHardLinks(t *testing.T) {
	t.Helper()
	old := blobLink
	blobLink = func(old, new string) error {
		return &os.LinkError{Op: "link", Old: old, New: new, Err: os.ErrPermission}
	}
	t.Cleanup(func() { blobLink = old })
}

// newPublicationRepo creates an HF cache repo with its directories.
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

// acquirePublicationDomain takes the ownership domain of one blob key, the
// way every writer of blobs/<sha> does.
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

// TestPublicationWithoutHardLinksNeverReplacesTheProvenBlob forces the
// domain-exclusive tier and pins the property that tier actually has: an
// existing content-addressed blob is the completion proof, and a domain
// participant does not replace it.
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
	// The refused payload is left for the caller to discard.
	if _, serr := os.Stat(src); serr != nil {
		t.Errorf("the refused payload was removed by the placement: %v", serr)
	}
}

// TestPublicationWithoutHardLinksPublishesANewBlob pins that hard-link
// support is not a prerequisite for downloading: the domain-exclusive tier
// still publishes a blob that does not exist yet.
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
	// The staging name is dropped, so the published blob is the only name
	// left for those bytes.
	if _, serr := os.Stat(src); serr == nil {
		t.Error("the staging name still exists after the publication")
	}
}

// TestPublicationWithoutHardLinksRefusesWithoutTheDomain pins that the
// domain-exclusive tier cannot be reached by accident: without the ownership
// domain of the blob key it publishes, the placement refuses instead of
// running a rename that could replace the target.
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

// TestPublicationWithoutHardLinksIsExclusiveToTheDomain pins the exclusivity
// the domain-exclusive tier relies on: a second writer of the same blob key
// waits for the domain and then finds the published blob instead of
// replacing it.
func TestPublicationWithoutHardLinksIsExclusiveToTheDomain(t *testing.T) {
	forceUnsupportedHardLinks(t)
	repo := newPublicationRepo(t)

	sha := strings.Repeat("e", 64)
	blobPath := repo.BlobPath(sha)
	winner := []byte("the proven winner")
	loser := []byte("the unproven loser")
	winnerSrc := filepath.Join(repo.BlobsDir(), "tmp-"+sha+".part")
	loserSrc := filepath.Join(repo.BlobsDir(), "tmp-"+sha+".other")
	if err := os.WriteFile(winnerSrc, winner, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loserSrc, loser, 0o644); err != nil {
		t.Fatal(err)
	}

	// The winner holds the domain and publishes.
	domain := acquirePublicationDomain(t, repo, sha)
	if err := blobPublishPlace(winnerSrc, blobPath, domain); err != nil {
		t.Fatalf("the winner's publication = %v, want success", err)
	}

	// The second writer starts while the winner still holds the domain.
	domainWait := make(chan struct{}, 1)
	oldHook := blobDomainContentionHook
	var hookOnce sync.Once
	blobDomainContentionHook = func(string) {
		hookOnce.Do(func() { domainWait <- struct{}{} })
	}
	defer func() { blobDomainContentionHook = oldHook }()

	loserDone := make(chan error, 1)
	go func() {
		d, derr := acquireBlobDomain(t.Context(), blobPath, mustDomainDst(t, repo, sha))
		if derr != nil {
			loserDone <- derr
			return
		}
		defer d.release()
		loserDone <- blobPublishPlace(loserSrc, blobPath, d)
	}()

	select {
	case <-domainWait:
	case err := <-loserDone:
		t.Fatalf("the second writer did not wait for the ownership domain: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the second writer neither waited for the ownership domain nor finished")
	}

	// The winner releases the domain; the second writer then finds the
	// published blob.
	domain.release()
	select {
	case err := <-loserDone:
		if !errors.Is(err, errBlobTargetExists) {
			t.Fatalf("the second writer's placement = %v, want errBlobTargetExists", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the second writer never finished")
	}

	got, rerr := os.ReadFile(blobPath)
	if rerr != nil || !bytes.Equal(got, winner) {
		t.Fatalf("the published blob = %q, %v; want the winner's bytes", got, rerr)
	}
}

// mustDomainDst resolves the ownership domain destination of one blob key.
func mustDomainDst(t *testing.T, repo *RepoDir, sha string) string {
	t.Helper()
	dst, err := repo.blobDomainDst(sha)
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestFinalizeBlobNeverReplacesAnExistingBlob pins the exported writer of
// blobs/<sha>: a finalized blob that another writer already published is
// the completion proof and keeps its bytes.
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
	// The refused payload is preserved for the caller to discard.
	if _, serr := os.Stat(repo.IncompletePath(sha)); serr != nil {
		t.Errorf("the refused payload was removed: %v", serr)
	}
}

// TestFinalizeBlobParticipatesInTheBlobKeyDomain pins that FinalizeBlob
// writes into the blob key's ownership domain like every other writer of
// blobs/<sha>: it waits for the domain instead of racing the writer that
// holds it.
func TestFinalizeBlobParticipatesInTheBlobKeyDomain(t *testing.T) {
	repo := newPublicationRepo(t)

	sha := strings.Repeat("f", 64)
	payload := []byte("the finalized bytes")
	if err := os.WriteFile(repo.IncompletePath(sha), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	// Another writer holds the domain of this blob key.
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

// newPlainHub serves the plan endpoints and the file body without holding
// the response, so a test can observe whether the transfer started at all.
// The returned counter is the number of file body requests the hub served.
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

// TestSingleRouteFailsBeforePayloadMutationWhenTheDomainIsUnavailable pins
// that the single-request route does not write a payload for a blob key it
// cannot own.
//
// The lifetime ownership of the blob key is the fence that keeps two writers
// of the same key apart, so an operation that cannot take it must fail
// BEFORE it mutates a claim or a payload, not after the transfer wrote one.
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
	// The transfer must not have run at all: the operation failed before it
	// could write a payload.
	if n := served(); n != 0 {
		t.Errorf("the single-request route transferred the file %d times although the ownership domain was unavailable", n)
	}

	repo, rerr := NewHFCache(cacheDir).Repo("owner/model", RepoTypeModel)
	if rerr != nil {
		t.Fatal(rerr)
	}
	// The single-request route writes blobs/tmp-<sha>.part. It must not have
	// written it: the operation failed before the transfer.
	if _, serr := os.Stat(filepath.Join(repo.BlobsDir(), "tmp-"+sha+".part")); serr == nil {
		t.Error("the single-request route wrote a payload although the ownership domain was unavailable")
	}
	if _, serr := os.Stat(repo.BlobPath(sha)); serr == nil {
		t.Error("the blob was published although the ownership domain was unavailable")
	}
}

// TestSingleRouteFailsBeforePayloadMutationWithADeniedOwnershipLock is the
// same invariant for a denied (not merely unsupported) lifetime lock: the
// operation fails before it mutates a payload.
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

// TestSingleRouteKeepsTheEarlierPartialWhenTheDomainIsUnavailable is the
// data-loss half of the same invariant: the failure must not remove a
// partial the failing operation did not write, and the transfer must not
// have started at all.
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

// TestPublishedBlobSurvivesTheDomainExclusiveTierEndToEnd pins the whole
// single-request route on a filesystem that cannot create hard links: the
// blob is published, and it holds exactly the transferred bytes.
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

// TestPlacementRejectsAForeignDomain pins that the domain-exclusive tier
// only runs under the domain of the blob key it publishes: a domain of
// another key is not enough.
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

// TestPublicationWithoutHardLinksReportsTheLinkFailure pins that a placement
// which cannot use the hard-link tier and cannot use the domain-exclusive
// tier says so, instead of publishing a blob that might replace another
// writer's bytes.
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
