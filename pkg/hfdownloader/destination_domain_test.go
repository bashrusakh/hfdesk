// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

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

// pathDerivedDst returns the cache staging name for a digest-less item.
func pathDerivedDst(blobsDir, rel string) string {
	return filepath.Join(blobsDir, "tmp-path-"+strings.ReplaceAll(rel, "/", "_"))
}

// pathDerivedPayload uses ASCII so JSON encoding preserves byte length.
func pathDerivedPayload(size int) []byte {
	const unit = "hfdesk-path-derived-payload-"
	return []byte(strings.Repeat(unit, size/len(unit)+1))[:size]
}

type pathDerivedHubState struct {
	firstGet chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	gets     int

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

// newPathDerivedHub serves a digest-less plain file; hold leaves a live partial.
func newPathDerivedHub(t *testing.T, rel string, full []byte, hold bool) (*httptest.Server, *pathDerivedHubState) {
	t.Helper()
	state := &pathDerivedHubState{
		firstGet: make(chan struct{}),
		release:  make(chan struct{}),
	}
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

	// Wait for a live on-disk prefix before starting the competing writer.
	select {
	case <-hub.firstGet:
	case <-time.After(60 * time.Second):
		t.Fatal("the first writer never started its transfer")
	}
	live := waitForNonEmptyFile(t, dst+".part", 60*time.Second)

	// Install the global contention hook before starting the second writer.
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
	// that owns it alone, while still removing a free destination's partial.
	otherDst := pathDerivedDst(repo.BlobsDir(), "other.bin")
	if err := os.WriteFile(otherDst+".part", []byte("this job's own partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst, otherDst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	after, err := os.ReadFile(dst + ".part")
	if err != nil {
		t.Fatalf("the live partial of the owning writer was removed: %v", err)
	}
	if !bytes.Equal(live, after) {
		t.Errorf("the live partial changed across the cleanup: got %d bytes, want %d", len(after), len(live))
	}
	if _, err := os.Stat(otherDst + ".part"); !os.IsNotExist(err) {
		t.Errorf("the free destination's partial survived the cleanup, stat err=%v", err)
	}

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

	assertExactFile(t, repo.BlobPath(singleTestSHA(full)), full)
	assertOwnershipContractLinks(t, cacheDir, rel)
}

// A child holds the path-derived domain and a sidecarless partial. The writer
// must wait for release, then obtain the file without inheriting unproven bytes.
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

	// The child holds the domain and seeds the first quarter without an identity sidecar.
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

	awaitContention(t, contended, "the destination's ownership domain")
	select {
	case out := <-result:
		t.Fatalf("the download finished while the child still held the domain (err=%v)", out.err)
	default:
	}

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

// Explicit cancellation removes this writer's staging while it still holds the domain.
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

	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Errorf("the cancelled job's own partial survived, stat err=%v", err)
	}
}

func TestContentAddressedDestinationDomainIsUnchanged(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)

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
		key          string
		dst          string
		domain       *blobDomain
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
	// Publication must hold the domain of the content-addressed key it publishes.
	if !first.holdsBlobKey {
		t.Error("the writer's domain does not hold the blob key it publishes")
	}

	// The persistent lock belongs beside tmp-<sha>, not beside the published blob.
	if _, err := os.Stat(dst + ".lock"); err != nil {
		t.Errorf("the domain lock file %s is missing: %v", dst+".lock", err)
	}
	if _, err := os.Stat(repo.BlobPath(sha) + ".lock"); !os.IsNotExist(err) {
		t.Errorf("a lock file appeared next to the blob, stat err=%v", err)
	}
}

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

	domain.release()
	released = true
	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Errorf("the released destination's own partial survived cleanup, stat err=%v", err)
	}
}

type twoPathDerivedFile struct {
	rel  string
	body []byte
}

// newTwoPathDerivedHub gates payloads so all transfers overlap before cache storage.
func newTwoPathDerivedHub(t *testing.T, files []twoPathDerivedFile) (*httptest.Server, <-chan struct{}, func()) {
	t.Helper()
	var nodes []string
	byPath := map[string][]byte{}
	for _, f := range files {
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

// Builds the symmetric path/content-digest cycle described in doc.go. The gate
// overlaps both transfers; success and exact published bytes verify the
// path-derived namespace keeps their domains disjoint.
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

	assertExactFile(t, repo.BlobPath(shaX), x)
	assertExactFile(t, repo.BlobPath(shaY), y)
	assertOwnershipContractLinks(t, cacheDir, shaX)
	assertOwnershipContractLinks(t, cacheDir, shaY)

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
