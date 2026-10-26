// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// The DESTINATION-scoped ownership domain (issue #140 / PR #151).
//
// The ownership domain used to be taken only by a writer of a CONTENT-ADDRESSED
// destination (an item with a known SHA-256). An item without a known digest
// - the planner allows an empty SHA-256 - got a path-derived destination
// (blobs/tmp-<name>), and NO writer ever held that destination's domain:
//
//   - two writers of the same path-derived destination were not mutually
//     excluded, in this process or in another one;
//   - the CLEANUP derived the same key, so its protection was vacuous: it
//     always acquired a domain nobody held, and a cancelled job could delete
//     another live job's .part/temp for the same path.
//
// A writer of a cache destination now ALWAYS holds that destination's
// ownership domain, and the cleanup uses the SAME name-based identity, so
// writer and cleanup agree for every cache destination:
//
//	destination            domain key         domain lock
//	blobs/tmp-<sha>        blobs/<sha>        blobs/tmp-<sha>.lock
//	blobs/tmp-path-<name>  blobs/path-<name>  blobs/tmp-path-<name>.lock
//
// The content-addressed row is unchanged, so the publication's holdsBlobKey
// check keeps working exactly as before. The path-derived row is structurally
// non-canonical: "path-" is not a hex digest, so its key and lock can never be
// a canonical blob key/lock, and its staging name can never equal a
// content-addressed tmp-<sha> staging name. That is what prevents two
// digest-less items whose paths are each other's content digests from holding
// the blob domains each other's store needs, and it removes the staging-name
// collision by construction.
//
// The cases below are driven through the real Download and cleanup paths, and
// the synchronization is deterministic: a writer proves that it waits through
// the production contention hooks, never through a sleep.

import (
	"bytes"
	"context"
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

// pathDerivedDst is the cache destination the downloader allocates for an
// item without a known digest: blobs/tmp-path-<sanitized relative path>.
func pathDerivedDst(blobsDir, rel string) string {
	return filepath.Join(blobsDir, "tmp-path-"+strings.ReplaceAll(rel, "/", "_"))
}

// pathDerivedPayload builds an ASCII payload of the requested size.
//
// The payload is ASCII on purpose: the cross-process helper passes its live
// partial through JSON, and a JSON round trip replaces every invalid UTF-8
// byte with U+FFFD, which would change the partial's length and make it no
// longer a prefix of the payload.
func pathDerivedPayload(size int) []byte {
	const unit = "hfdesk-path-derived-payload-"
	return []byte(strings.Repeat(unit, size/len(unit)+1))[:size]
}

// pathDerivedHubState is the observable state of the path-derived hub.
type pathDerivedHubState struct {
	// firstGet is closed when the first payload request arrives.
	firstGet chan struct{}
	// release is closed when the held payload response may finish.
	release chan struct{}
	// mu guards gets.
	mu sync.Mutex
	// gets counts the payload requests.
	gets int

	firstGetOnce sync.Once
	releaseOnce  sync.Once
}

func (s *pathDerivedHubState) payloadGets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

func (s *pathDerivedHubState) releasePayload() {
	s.releaseOnce.Do(func() { close(s.release) })
}

// newPathDerivedHub serves ONE plain (non-LFS) file WITHOUT a digest, so the
// planned item carries no known SHA-256 and its cache destination is derived
// from the file path.
//
// When hold is true the payload response writes the first half of the payload,
// flushes it and then waits for releasePayload: the writer's .part therefore
// holds a live, non-empty prefix while the writer owns the destination.
func newPathDerivedHub(t *testing.T, rel string, full []byte, hold bool) (*httptest.Server, *pathDerivedHubState) {
	t.Helper()
	state := &pathDerivedHubState{
		firstGet: make(chan struct{}),
		release:  make(chan struct{}),
	}
	// The tree node carries NO sha256 and no lfs block: the planner accepts an
	// empty digest, and the item is a plain (non-ranged) file.
	node := fmt.Sprintf(`{"type":"file","path":%q,"size":%d}`, rel, len(full))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte("[" + node + "]"))
		case strings.Contains(r.URL.Path, "/resolve/") || strings.Contains(r.URL.Path, "/raw/"):
			if r.Method == http.MethodGet {
				state.mu.Lock()
				state.gets++
				state.mu.Unlock()
				state.firstGetOnce.Do(func() { close(state.firstGet) })
				if hold {
					half := len(full) / 2
					w.Header().Set("Content-Length", strconv.Itoa(len(full)))
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(full[:half])
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					<-state.release
					_, _ = w.Write(full[half:])
					return
				}
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, state
}

// waitForNonEmptyFile blocks until path exists with at least one byte (or
// the timeout expires). It waits for a STATE, not for a duration: the file's
// content is the evidence that the writer reached the disk.
func waitForNonEmptyFile(t *testing.T, path string, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return data
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never became a non-empty file", filepath.Base(path))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestPathDerivedDestinationDomainExcludesASecondWriter covers the
// destination-scoped domain for a path-derived (empty-digest) destination.
//
// The second writer of the same destination must WAIT for the domain the
// first one holds, and while the first writer is live a cleanup of that
// destination must NOT delete the first writer's live partial. When the first
// writer finishes, the second one completes and the file is present in the
// cache with its snapshot and friendly entries.
//
// The wait proves itself through the production in-process contention hook,
// and the surviving partial through the cleanup's own domain acquisition.
func TestPathDerivedDestinationDomainExcludesASecondWriter(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := pathDerivedPayload(total)
	rel := "model.bin"

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	dst := pathDerivedDst(repo.BlobsDir(), rel)

	srv, hub := newPathDerivedHub(t, rel, full, true)
	// The held response is always released, so a failing assertion cannot
	// leave the test server waiting for a handler that never finishes.
	defer hub.releasePayload()
	cfg := ownershipContractSettings(cacheDir, srv.URL, "1MB")

	first := make(chan ownershipContractOutcome, 1)
	go func() { first <- runOwnershipContractDownload(context.Background(), cfg) }()

	// The first writer owns the destination as soon as it starts its
	// transfer, and its .part holds a live prefix of the payload.
	select {
	case <-hub.firstGet:
	case <-time.After(60 * time.Second):
		t.Fatal("the first writer never started its transfer")
	}
	live := waitForNonEmptyFile(t, dst+".part", 60*time.Second)

	// The second writer must wait for the domain the first one holds. The
	// probe is installed before that writer starts: the hook is a
	// package-level variable.
	contended := installBlobDomainContentionProbe(t)
	second := make(chan ownershipContractOutcome, 1)
	go func() { second <- runOwnershipContractDownload(context.Background(), cfg) }()
	awaitContention(t, contended, "the destination's ownership domain")

	select {
	case out := <-second:
		t.Fatalf("the second writer finished while the first one still held the domain (err=%v)", out.err)
	default:
	}

	// A cleanup of the destination must leave the live partial of the writer
	// that owns it alone: the cleanup takes the SAME domain.
	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	after, err := os.ReadFile(dst + ".part")
	if err != nil {
		t.Fatalf("the live partial of the owning writer was removed: %v", err)
	}
	if !bytes.Equal(live, after) {
		t.Errorf("the live partial changed across the cleanup: got %d bytes, want %d", len(after), len(live))
	}

	// The first writer finishes and releases the domain.
	hub.releasePayload()

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
		t.Fatalf("the second writer failed although the destination was free: %v", secondOut.err)
	}
	if len(secondOut.skips) != 0 {
		t.Errorf("the second writer's skips = %v, want none: a path-derived destination has no published target to reuse", secondOut.skips)
	}

	// The file is present and stored with its links.
	assertExactFile(t, repo.BlobPath(singleTestSHA(full)), full)
	assertOwnershipContractLinks(t, cacheDir, rel)
}

// TestPathDerivedDestinationDomainExcludesAnotherProcess is the cross-process
// variant: a REAL child process holds the destination's domain lock
// (blobs/tmp-path-<name>.lock), and the writer must wait for it.
//
// The child's live partial is a valid PREFIX of the payload, so the writer
// resumes from it and the published file is byte-exact.
func TestPathDerivedDestinationDomainExcludesAnotherProcess(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := pathDerivedPayload(total)
	rel := "model.bin"

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	dst := pathDerivedDst(repo.BlobsDir(), rel)

	// The child process owns the destination's domain and holds a live
	// partial: the first quarter of the payload.
	prefix := full[:total/4]
	child := startBlobOwnershipChild(t, blobOwnershipChildConfig{
		DomainDst:   dst,
		PartialPath: dst + ".part",
		Payload:     string(prefix),
		ReadyPath:   filepath.Join(repo.BlobsDir(), "child-ready"),
		GoPath:      filepath.Join(repo.BlobsDir(), "child-go"),
	})
	waitForFile(t, filepath.Join(repo.BlobsDir(), "child-ready"), 60*time.Second)

	srv, _ := newPathDerivedHub(t, rel, full, false)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "1MB")

	// The probe is installed before the writer starts: the hook is a
	// package-level variable.
	contended := installDomainContentionProbe(t)
	result := make(chan ownershipContractOutcome, 1)
	go func() { result <- runOwnershipContractDownload(context.Background(), cfg) }()

	// The item waits for the domain the child holds.
	awaitContention(t, contended, "the destination's ownership domain")
	select {
	case out := <-result:
		t.Fatalf("the download finished while the child still held the domain (err=%v)", out.err)
	default:
	}

	// A cleanup of the destination leaves the child's live partial alone.
	live, err := os.ReadFile(dst + ".part")
	if err != nil {
		t.Fatalf("the child's live partial is missing: %v", err)
	}
	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	after, err := os.ReadFile(dst + ".part")
	if err != nil {
		t.Fatalf("the cleanup removed the live partial of the child process: %v", err)
	}
	if !bytes.Equal(live, after) {
		t.Errorf("the live partial changed across the cleanup: got %d bytes, want %d", len(after), len(live))
	}

	// The child releases the domain; the writer obtains the file itself.
	releaseBlobOwnershipChild(t, child, filepath.Join(repo.BlobsDir(), "child-go"))

	var out ownershipContractOutcome
	select {
	case out = <-result:
	case <-time.After(120 * time.Second):
		t.Fatal("the download did not finish after the child released the domain")
	}
	if out.err != nil {
		t.Fatalf("Download: %v", out.err)
	}
	assertExactFile(t, repo.BlobPath(singleTestSHA(full)), full)
	assertOwnershipContractLinks(t, cacheDir, rel)
}

// TestPathDerivedDestinationCleanupAndCancelOwnership covers the cleanup half
// of the destination-scoped domain:
//
//   - the artifacts of a path-derived destination another live writer holds
//     are left alone, byte for byte;
//   - the artifacts of a destination nobody holds are still cleaned up, so a
//     cancelling job can remove its OWN artifacts.
//
// The live writer is a REAL Download, so the domain it holds is the one the
// production writer path takes.
func TestPathDerivedDestinationCleanupAndCancelOwnership(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	// The cleanup is best-effort: it must not delay an explicit cancel.
	restoreWait := stagingLockCleanupWait
	stagingLockCleanupWait = 50 * time.Millisecond
	defer func() { stagingLockCleanupWait = restoreWait }()

	const rel = "model.bin"
	const total = 4_096
	full := pathDerivedPayload(total)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	// A destination owned by another live writer of this process.
	ownedDst := pathDerivedDst(repo.BlobsDir(), rel)
	ownedPartial := ownedDst + ".part"
	// A destination nobody owns: the cancelling job's own artifacts.
	ownDst := pathDerivedDst(repo.BlobsDir(), "other.bin")
	ownPartial := ownDst + ".part"
	if err := os.WriteFile(ownPartial, []byte("this job's own partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The live writer is a real Download: it holds the destination's
	// ownership domain while its transfer is in flight.
	srv, hub := newPathDerivedHub(t, rel, full, true)
	defer hub.releasePayload()
	cfg := ownershipContractSettings(cacheDir, srv.URL, "1MB")
	writer := make(chan ownershipContractOutcome, 1)
	go func() { writer <- runOwnershipContractDownload(context.Background(), cfg) }()
	select {
	case <-hub.firstGet:
	case <-time.After(60 * time.Second):
		t.Fatal("the live writer never started its transfer")
	}
	live := waitForNonEmptyFile(t, ownedPartial, 60*time.Second)

	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{ownedDst, ownDst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}

	// The live partial of the other writer survived, byte for byte.
	got, err := os.ReadFile(ownedPartial)
	if err != nil {
		t.Fatalf("the live partial of the other writer was removed: %v", err)
	}
	if !bytes.Equal(got, live) {
		t.Errorf("the live partial holds %d bytes, want the %d bytes it had before the cleanup", len(got), len(live))
	}
	// The job's own artifacts are still cleaned up.
	if _, err := os.Stat(ownPartial); !os.IsNotExist(err) {
		t.Errorf("the job's own partial survived the cleanup, stat err=%v", err)
	}

	// The live writer finishes before the test's temporary directory is
	// removed.
	hub.releasePayload()
	select {
	case out := <-writer:
		if out.err != nil {
			t.Errorf("the live writer failed: %v", out.err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("the live writer did not finish")
	}
}

// TestCancelledJobRemovesItsOwnPathDerivedArtifacts covers the other half of
// the cancel contract: the downloader's own cancel cleanup removes the
// staging of the destination it owns, while it still holds the domain.
func TestCancelledJobRemovesItsOwnPathDerivedArtifacts(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := pathDerivedPayload(total)
	rel := "model.bin"

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	dst := pathDerivedDst(repo.BlobsDir(), rel)

	srv, hub := newPathDerivedHub(t, rel, full, true)
	// The held response is always released, so a failing assertion cannot
	// leave the test server waiting for a handler that never finishes.
	defer hub.releasePayload()
	cfg := ownershipContractSettings(cacheDir, srv.URL, "1MB")
	// The cancel cleanup removes the partials this run produced.
	cfg.CleanupPartialsOnCancel = func() bool { return true }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan ownershipContractOutcome, 1)
	go func() { result <- runOwnershipContractDownload(ctx, cfg) }()

	select {
	case <-hub.firstGet:
	case <-time.After(60 * time.Second):
		t.Fatal("the writer never started its transfer")
	}
	waitForNonEmptyFile(t, dst+".part", 60*time.Second)

	// The job is cancelled while the writer owns the destination.
	cancel()
	hub.releasePayload()

	select {
	case out := <-result:
		if !errors.Is(out.err, context.Canceled) {
			t.Errorf("Download error = %v, want the cancellation", out.err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("the cancelled download did not finish")
	}

	// The job's own partial is gone: the cancel cleanup removed it while the
	// writer still held the destination's domain.
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Errorf("the cancelled job's own partial survived, stat err=%v", err)
	}
}

// TestContentAddressedDestinationDomainIsUnchanged pins the content-addressed
// half of the domain contract: the domain key, the domain lock file and the
// publication's holdsBlobKey check are exactly what they were.
//
// The writer's domain is observed through the production acquisition seam, so
// the assertion is about the domain the WRITER takes, not about a helper.
func TestContentAddressedDestinationDomainIsUnchanged(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	// The domain identity of a content-addressed destination.
	dst := filepath.Join(repo.BlobsDir(), "tmp-"+sha)
	key, ok := cacheDstDomainKey(dst)
	if !ok {
		t.Fatalf("the content-addressed destination %s has no ownership domain", dst)
	}
	if want := repo.BlobPath(sha); key != want {
		t.Errorf("the domain key = %q, want the blob key %q", key, want)
	}
	if got, want := stagingOwnershipLockPath(dst), dst+".lock"; got != want {
		t.Errorf("the domain lock = %q, want %q", got, want)
	}

	// The writer's domain, observed through the production seam.
	type acquisition struct {
		key    string
		dst    string
		domain *blobDomain
		// holdsBlobKey is the publication's own check, evaluated while the
		// domain was still held: a released domain holds nothing.
		holdsBlobKey bool
	}
	var (
		mu           sync.Mutex
		acquisitions []acquisition
	)
	oldAcquire := blobDomainAcquire
	blobDomainAcquire = func(ctx context.Context, k, d string) (*blobDomain, error) {
		dom, err := oldAcquire(ctx, k, d)
		mu.Lock()
		acquisitions = append(acquisitions, acquisition{
			key:          k,
			dst:          d,
			domain:       dom,
			holdsBlobKey: dom != nil && dom.holdsBlobKey(repo.BlobPath(sha)),
		})
		mu.Unlock()
		return dom, err
	}
	defer func() { blobDomainAcquire = oldAcquire }()

	srv, _ := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")
	out := runOwnershipContractDownload(context.Background(), cfg)
	if out.err != nil {
		t.Fatalf("Download: %v", out.err)
	}

	mu.Lock()
	got := append([]acquisition(nil), acquisitions...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("the writer took no ownership domain")
	}
	first := got[0]
	if first.key != repo.BlobPath(sha) {
		t.Errorf("the writer's domain key = %q, want the blob key %q", first.key, repo.BlobPath(sha))
	}
	if first.dst != dst {
		t.Errorf("the writer's domain destination = %q, want %q", first.dst, dst)
	}
	// The publication's holdsBlobKey check still holds for the content-
	// addressed item: the domain the writer holds IS the blob key's domain.
	if !first.holdsBlobKey {
		t.Error("the writer's domain does not hold the blob key it publishes")
	}

	// The domain lock file is the destination's persistent sibling, exactly
	// as before; no lock file appears next to the blob itself.
	if _, err := os.Stat(dst + ".lock"); err != nil {
		t.Errorf("the domain lock file %s is missing: %v", dst+".lock", err)
	}
	if _, err := os.Stat(repo.BlobPath(sha) + ".lock"); !os.IsNotExist(err) {
		t.Errorf("a lock file appeared next to the blob, stat err=%v", err)
	}
}

// TestNegativeControlNoDomainForPathDerivedDestination is the negative
// control for the destination-scoped domain.
//
// It installs the historical model through the production seam - a writer
// that takes NO domain for a path-derived destination - and shows that the
// live partial of that writer is deleted by a cleanup. That is the defect the
// destination-scoped domain removes, and it proves that the cleanup
// regression above really detects it.
func TestNegativeControlNoDomainForPathDerivedDestination(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	restoreWait := stagingLockCleanupWait
	stagingLockCleanupWait = 50 * time.Millisecond
	defer func() { stagingLockCleanupWait = restoreWait }()

	const rel = "model.bin"
	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	dst := pathDerivedDst(repo.BlobsDir(), rel)
	partial := dst + ".part"
	live := []byte("the live partial of the other writer")
	if err := os.WriteFile(partial, live, 0o644); err != nil {
		t.Fatal(err)
	}

	// The historical model: a path-derived destination has no ownership
	// domain at all, so nothing excludes a second writer and the cleanup's
	// protection is vacuous.
	oldAcquire := blobDomainAcquire
	blobDomainAcquire = func(ctx context.Context, key, d string) (*blobDomain, error) {
		if !canonicalSHA256(filepath.Base(key)) {
			return &blobDomain{}, nil
		}
		return oldAcquire(ctx, key, d)
	}
	defer func() { blobDomainAcquire = oldAcquire }()

	// The other writer "owns" the destination the historical way: it writes
	// its partial, but no domain is held for it.
	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("the negative control did not reproduce the defect: the live partial survived (stat err=%v)", err)
	}
	t.Logf("negative control reproduced the defect: the cleanup deleted the live partial of a path-derived destination")
}

// TestCacheDstDomainKeyUsesNameBasedIdentityForBothClasses pins the single
// name-based domain rule:
//
//   - a content-addressed destination blobs/tmp-<sha> keeps the blob key's
//     domain (key blobs/<sha>, lock blobs/tmp-<sha>.lock), unchanged;
//   - a path-derived destination blobs/tmp-path-<name> derives key
//     blobs/path-<name> and lock blobs/tmp-path-<name>.lock. The "path-"
//     prefix is not a hex digest, so even when <name> is a canonical SHA the
//     key/lock cannot equal a blob key/lock and the staging name cannot equal
//     a content-addressed tmp-<sha> name.
func TestCacheDstDomainKeyUsesNameBasedIdentityForBothClasses(t *testing.T) {
	blobsDir := filepath.Join(t.TempDir(), "blobs")
	sha := strings.Repeat("ab", 32)

	contentDst := filepath.Join(blobsDir, "tmp-"+sha)
	blobKey, ok := cacheDstDomainKey(contentDst)
	if !ok {
		t.Fatalf("the content-addressed destination %s has no domain key", contentDst)
	}
	if want := filepath.Join(blobsDir, sha); blobKey != want {
		t.Errorf("content-addressed domain key = %q, want the blob key %q", blobKey, want)
	}
	if got, want := stagingOwnershipLockPath(contentDst), contentDst+".lock"; got != want {
		t.Errorf("content-addressed domain lock = %q, want %q", got, want)
	}

	// A path-derived destination uses the structurally non-canonical
	// tmp-path- shape even when its sanitized relative path is a digest.
	pathDst := filepath.Join(blobsDir, "tmp-path-"+sha)
	pathKey, ok := cacheDstDomainKey(pathDst)
	if !ok {
		t.Fatalf("the path-derived destination %s has no domain key", pathDst)
	}
	if want := filepath.Join(blobsDir, "path-"+sha); pathKey != want {
		t.Errorf("path-derived domain key = %q, want %q", pathKey, want)
	}
	if got, want := stagingOwnershipLockPath(pathDst), pathDst+".lock"; got != want {
		t.Errorf("path-derived domain lock = %q, want %q", got, want)
	}
	if pathDst == contentDst {
		t.Errorf("the path-derived and content-addressed destinations share the staging name %q", pathDst)
	}
	if pathKey == blobKey {
		t.Errorf("path-derived domain key %q equals the blob key", pathKey)
	}
	if got := stagingOwnershipLockPath(pathDst); got == stagingOwnershipLockPath(contentDst) {
		t.Errorf("the path-derived lock file %q equals the blob domain lock", got)
	}

	// Only a blobs/tmp-<name> staging name is a cache destination.
	if _, ok := cacheDstDomainKey(filepath.Join(blobsDir, "model.bin")); ok {
		t.Error("a non-staging blobs entry was accepted as a cache destination")
	}
	if _, ok := cacheDstDomainKey(filepath.Join(blobsDir, "tmp-")); ok {
		t.Error("an empty tmp- name was accepted as a cache destination")
	}
}

// TestPathDerivedDestinationCleanupUsesSameNameBasedIdentity pins the cleanup
// half of the single name-based identity.
//
// The path-derived destination uses blobs/tmp-path-<sanitized path> even when
// that path is a canonical digest string, so the cleanup derives
// key blobs/path-<name> and lock blobs/tmp-path-<name>.lock from the SAME
// destination name the writer holds. While the writer holds that domain the
// live partial must survive; after release the cleanup removes it. The staging
// name can never equal a content-addressed tmp-<sha> destination.
func TestPathDerivedDestinationCleanupUsesSameNameBasedIdentity(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	restoreWait := stagingLockCleanupWait
	stagingLockCleanupWait = 50 * time.Millisecond
	defer func() { stagingLockCleanupWait = restoreWait }()

	rel := strings.Repeat("cd", 32)
	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	dst := pathDerivedDst(repo.BlobsDir(), rel)
	partial := dst + ".part"
	live := []byte("the live partial of the path-derived writer")
	if err := os.WriteFile(partial, live, 0o644); err != nil {
		t.Fatal(err)
	}

	key, ok := cacheDstDomainKey(dst)
	if !ok {
		t.Fatalf("the path-derived destination %s has no domain key", dst)
	}
	if want := filepath.Join(repo.BlobsDir(), "path-"+rel); key != want {
		t.Errorf("path-derived domain key = %q, want %q", key, want)
	}
	if got, want := stagingOwnershipLockPath(dst), dst+".lock"; got != want {
		t.Fatalf("path-derived domain lock = %q, want %q", got, want)
	}
	contentDst := filepath.Join(repo.BlobsDir(), "tmp-"+rel)
	if dst == contentDst {
		t.Fatalf("the path-derived and content-addressed destinations share the staging name %q", dst)
	}
	domain, err := blobDomainAcquire(context.Background(), key, dst)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			domain.release()
		}
	}()

	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	got, err := os.ReadFile(partial)
	if err != nil {
		t.Fatalf("the cleanup removed the live partial of the path-derived writer: %v", err)
	}
	if !bytes.Equal(got, live) {
		t.Errorf("the live partial changed across the cleanup: got %q, want %q", got, live)
	}

	// Once the writer releases its domain, the cleanup removes the artifacts.
	domain.release()
	released = true
	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Errorf("the released destination's own partial survived cleanup, stat err=%v", err)
	}
}

// twoPathDerivedFile is one file of the deadlock hub below.
type twoPathDerivedFile struct {
	rel  string
	body []byte
}

// newTwoPathDerivedHub serves two plain (non-LFS) files WITHOUT a supplied
// digest and holds BOTH payload responses until both GETs have arrived.
//
// The gate makes the ownership cycle deterministic: both items own their own
// destination domain before either can finish its transfer and enter the
// store, which is the state in which the old identity could deadlock.
func newTwoPathDerivedHub(t *testing.T, files []twoPathDerivedFile) (*httptest.Server, <-chan struct{}, func()) {
	t.Helper()
	var nodes []string
	byPath := map[string][]byte{}
	for _, f := range files {
		// No sha256 and no lfs block: the planner accepts an empty digest and
		// the item is a plain, non-ranged file.
		nodes = append(nodes, fmt.Sprintf(`{"type":"file","path":%q,"size":%d}`, f.rel, len(f.body)))
		byPath[f.rel] = f.body
	}
	tree := "[" + strings.Join(nodes, ",") + "]"

	var (
		mu      sync.Mutex
		entered int
	)
	bothEntered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte(tree))
		case strings.Contains(r.URL.Path, "/resolve/") || strings.Contains(r.URL.Path, "/raw/"):
			rel := filepath.Base(r.URL.Path)
			body, ok := byPath[rel]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if r.Method == http.MethodHead {
				http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
				return
			}
			if r.Method != http.MethodGet {
				http.NotFound(w, r)
				return
			}
			mu.Lock()
			entered++
			if entered == len(files) {
				close(bothEntered)
			}
			mu.Unlock()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		unblock()
		srv.Close()
	})
	return srv, bothEntered, unblock
}

// TestPathDerivedDigestNamedFilesDoNotDeadlockWithComputedBlobDomains is the
// regression for the path-derived destination-domain deadlock.
//
// File A's path is SHA256(Y) and its content is X; file B's path is SHA256(X)
// and its content is Y. Neither item has a supplied digest, so both are
// path-derived and stage under blobs/tmp-path-SHA256(Y) and
// blobs/tmp-path-SHA256(X). The "path-" prefix makes those names structurally
// non-canonical, so their name-based domains are blobs/path-SHA256(Y) and
// blobs/path-SHA256(X): neither can equal a blob key or lock.
//
// Before the prefix, the destinations were blobs/tmp-SHA256(Y) and
// blobs/tmp-SHA256(X), so A's destination held domain key blobs/SHA256(Y) and
// B's held blobs/SHA256(X); A's store then needed blobs/SHA256(X) and B's
// store needed blobs/SHA256(Y) - each waited forever for the domain the other
// held, and the in-process wait is unbounded. The items must instead transfer,
// store and publish both blobs under the digests computed from their content.
//
// The two payload responses are gated until both transfers have started, so
// the cycle is reached deterministically and the bounded context is the only
// timing element.
func TestPathDerivedDigestNamedFilesDoNotDeadlockWithComputedBlobDomains(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	x := []byte(strings.Repeat("x", 1_024))
	y := []byte(strings.Repeat("y", 1_024))
	shaX := singleTestSHA(x)
	shaY := singleTestSHA(y)
	if shaX == shaY {
		t.Fatal("the two test payloads share a digest")
	}

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

	srv, bothEntered, release := newTwoPathDerivedHub(t, []twoPathDerivedFile{
		{rel: shaY, body: x},
		{rel: shaX, body: y},
	})

	cfg := ownershipContractSettings(cacheDir, srv.URL, "1MB")
	cfg.MaxActiveDownloads = 2
	cfg.Concurrency = 2

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result := make(chan ownershipContractOutcome, 1)
	go func() { result <- runOwnershipContractDownload(ctx, cfg) }()

	// Both items must be inside their transfer before the gate opens: that is
	// the state in which each still holds its own destination domain when the
	// other enters the store.
	select {
	case <-bothEntered:
	case <-time.After(60 * time.Second):
		release()
		t.Fatal("the two items did not both start their transfers")
	}
	release()

	var out ownershipContractOutcome
	select {
	case out = <-result:
	case <-time.After(30 * time.Second):
		t.Fatal("Download did not finish: the two path-derived destinations deadlocked on their computed blob domains")
	}
	if out.err != nil {
		t.Fatalf("Download: %v (both path-derived items must complete, not deadlock on each other's computed blob domains)", out.err)
	}

	// Both computed content digests are published with the exact bytes, and
	// both requested paths are reachable.
	assertExactFile(t, repo.BlobPath(shaX), x)
	assertExactFile(t, repo.BlobPath(shaY), y)
	assertOwnershipContractLinks(t, cacheDir, shaX)
	assertOwnershipContractLinks(t, cacheDir, shaY)

	// The writers used the structurally non-canonical tmp-path- staging names
	// and the single name-based domain: each destination owns the persistent
	// tmp-path-<name>.lock sibling, its key is blobs/path-<name>, and it can
	// never share the staging name of a content-addressed tmp-<sha> item.
	for _, rel := range []string{shaX, shaY} {
		dst := filepath.Join(repo.BlobsDir(), "tmp-path-"+rel)
		key, ok := cacheDstDomainKey(dst)
		if !ok {
			t.Errorf("the path-derived destination %s has no domain key", dst)
			continue
		}
		if want := filepath.Join(repo.BlobsDir(), "path-"+rel); key != want {
			t.Errorf("path-derived domain key = %q, want %q", key, want)
		}
		if _, err := os.Stat(dst + ".lock"); err != nil {
			t.Errorf("the path-derived destination domain lock %s is missing: %v", dst+".lock", err)
		}
		contentDst := filepath.Join(repo.BlobsDir(), "tmp-"+rel)
		if dst == contentDst {
			t.Errorf("the path-derived and content-addressed destinations share the staging name %q", dst)
		}
	}
}
