// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the blob-key publication domain (issue #140 /
// PR #151).
//
// Two different routes write the same content-addressed blob key: the ranged
// route stages in blobs/<sha>.incomplete and publishes onto blobs/<sha>, the
// single-request route stages in blobs/tmp-<sha>.part and publishes through
// the cache store onto blobs/<sha>. Before this domain existed each route
// had its own private ownership, so:
//
//   - both routes could decide "the blob does not exist yet" and then both
//     place their bytes: the second placement REPLACED the first writer's
//     blob, and the cache ended up with bytes that do not hash to the key
//     they are stored under - a corruption CheckBlob then reports as
//     complete, permanently;
//   - the ranged route treated the single route's LIVE partial
//     (blobs/tmp-<sha>.part) as residue it could adopt and delete. The
//     published blob was then a MIXTURE of two writers' byte streams, and the
//     single writer's own job failed because its partial was gone.
//
// The invariant these tests pin:
//
//   - ONE ownership domain per blob key, held by both routes for the whole
//     operation on that key, so a live partial of one route is never
//     adopted or deleted by the other route;
//   - the publication places the bytes with a no-replace primitive, so an
//     existing blobs/<sha> is completion proof and is not replaced by a
//     writer of the same blob key (see blob_publish.go for the two tiers:
//     atomic no-replace with hard links, domain-exclusive without them);
//   - the published blob is exactly one writer's transferred bytes - never a
//     mixture of two writers.
//
// The hub serves a DIFFERENT byte stream to each route (a ranged GET gets
// stream A, a full GET gets stream B), so a mixed publication is detectable
// byte by byte. The single-request writer's response is held after its first
// half, so the other writer provably overlaps a LIVE partial; no sleep
// decides the outcome.

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

// publicationStreams holds the two byte streams the hub serves to the two
// routes.
type publicationStreams struct {
	a []byte
	b []byte
}

// newPublicationStreams builds two same-length, single-valued streams.
func newPublicationStreams(total int) publicationStreams {
	a := bytes.Repeat([]byte{0xA5}, total)
	b := bytes.Repeat([]byte{0x5A}, total)
	return publicationStreams{a: a, b: b}
}

// isSingleStream reports whether blob is exactly one of the two streams.
func (s publicationStreams) isSingleStream(blob []byte) bool {
	if bytes.Equal(blob, s.a) {
		return true
	}
	return bytes.Equal(blob, s.b)
}

// twoWriterHub is the hub shared by the two writers.
type twoWriterHub struct {
	srv *httptest.Server
	// releaseFull is closed to let the held full-body response finish.
	releaseFull chan struct{}
	// releaseOnce guards releaseFull.
	releaseOnce sync.Once
	// sameStream makes the full-body response serve the same bytes as the
	// ranged one, so both writers transfer identical content.
	sameStream bool
}

func (h *twoWriterHub) release() {
	h.releaseOnce.Do(func() { close(h.releaseFull) })
}

// newTwoWriterHub serves the plan endpoints and the two streams.
func newTwoWriterHub(t *testing.T, total int, sha string, streams publicationStreams) *twoWriterHub {
	t.Helper()
	hub := &twoWriterHub{releaseFull: make(chan struct{})}
	hub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte("[" + lfsTreeNode("model.bin", total, sha) + "]"))
		case strings.Contains(r.URL.Path, "/resolve/"):
			hub.serveFile(w, r, total, streams)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.srv.Close)
	return hub
}

// serveFile serves one file request. A ranged GET gets stream A; a full GET
// gets stream B and is held after its first half until the test releases it.
func (h *twoWriterHub) serveFile(w http.ResponseWriter, r *http.Request, total int, streams publicationStreams) {
	if r.Method == http.MethodHead {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		start, end, ok := parseByteRange(rng, total)
		if !ok {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(streams.a[start : end+1])
		return
	}
	body := streams.b
	if h.sameStream {
		body = streams.a
	}
	w.Header().Set("Content-Length", strconv.Itoa(total))
	w.WriteHeader(http.StatusOK)
	half := total / 2
	_, _ = w.Write(body[:half])
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	// Hold the response so the other writer overlaps a LIVE partial.
	<-h.releaseFull
	_, _ = w.Write(body[half:])
}

// parseByteRange parses a "bytes=start-end" request header.
func parseByteRange(header string, total int) (int, int, bool) {
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, err := strconv.Atoi(parts[0])
	if err != nil || start < 0 || start >= total {
		return 0, 0, false
	}
	end := total - 1
	if parts[1] != "" {
		end, err = strconv.Atoi(parts[1])
		if err != nil || end < start || end >= total {
			return 0, 0, false
		}
	}
	return start, end, true
}

// writerResult is the outcome of one Download.
type writerResult struct {
	err   error
	skips []string
}

// runCacheWriter runs one Download against the shared hub and cache.
func runCacheWriter(t *testing.T, hub *twoWriterHub, cacheDir, threshold string, onProgress func(ProgressEvent)) writerResult {
	t.Helper()
	cfg := Settings{
		CacheDir: cacheDir, Endpoint: hub.srv.URL, Concurrency: 1, MaxActiveDownloads: 1,
		MultipartThreshold: threshold, Verify: "none", Retries: 0, StallTimeout: "0",
	}
	res := writerResult{}
	res.err = Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ev ProgressEvent) {
		if ev.Event == "file_done" && strings.Contains(ev.Message, "skip") {
			res.skips = append(res.skips, ev.Message)
		}
		if onProgress != nil {
			onProgress(ev)
		}
	})
	return res
}

// twoWriterOutcome is what one two-writer run observed.
type twoWriterOutcome struct {
	single writerResult
	ranged writerResult
	// blob is the published blob, if any.
	blob []byte
	// blobExists reports whether blobs/<sha> exists.
	blobExists bool
	// partialSize is the size of the single writer's live partial while the
	// other writer was active (-1 when it is already gone).
	partialSize int64
	// partialPrefix is the live partial's content while the other writer was
	// active.
	partialPrefix []byte
	// domainWaited reports that the second writer waited for the blob key's
	// ownership domain instead of proceeding.
	domainWaited bool
}

// runTwoWriters runs the two-writer scenario: the single-request writer
// starts first and is held mid-transfer, then the ranged writer starts while
// the first writer's partial is live.
//
// The observation of the live partial is taken while the first writer is
// still blocked, so the "was it adopted or deleted?" question is answered
// about the live state and not about the residue.
func runTwoWriters(t *testing.T, hub *twoWriterHub, cacheDir, sha string, streams publicationStreams) twoWriterOutcome {
	t.Helper()
	repo, err := NewHFCache(cacheDir, time.Minute).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	partialPath := filepath.Join(repo.BlobsDir(), "tmp-"+sha+".part")

	// The domain contention hook tells the test that the second writer
	// really waited for the blob key's domain instead of proceeding.
	domainWait := make(chan struct{}, 1)
	oldHook := blobDomainContentionHook
	var hookOnce sync.Once
	blobDomainContentionHook = func(string) {
		hookOnce.Do(func() { domainWait <- struct{}{} })
	}
	defer func() { blobDomainContentionHook = oldHook }()

	// The single writer's first bytes on disk.
	firstBytes := make(chan struct{}, 1)
	var firstOnce sync.Once
	singleDone := make(chan writerResult, 1)
	go func() {
		singleDone <- runCacheWriter(t, hub, cacheDir, "1MB", func(ev ProgressEvent) {
			if ev.Event == "file_progress" && ev.Downloaded > 0 {
				firstOnce.Do(func() { firstBytes <- struct{}{} })
			}
		})
	}()

	select {
	case <-firstBytes:
	case <-time.After(60 * time.Second):
		hub.release()
		t.Fatal("the single-request writer never reported its first bytes")
	}

	rangedDone := make(chan writerResult, 1)
	go func() {
		rangedDone <- runCacheWriter(t, hub, cacheDir, "0", nil)
	}()

	out := twoWriterOutcome{partialSize: -1}
	var rangedResult writerResult
	rangedFinished := false
	select {
	case <-domainWait:
		out.domainWaited = true
	case rangedResult = <-rangedDone:
		// The ranged writer finished without waiting for the domain: it
		// proceeded into the other writer's live partial (the defect the
		// negative control installs).
		rangedFinished = true
	case <-time.After(60 * time.Second):
		hub.release()
		t.Fatal("the ranged writer neither waited for the ownership domain nor finished")
	}

	// Observe the live partial while the first writer still holds it.
	if fi, serr := os.Stat(partialPath); serr == nil {
		out.partialSize = fi.Size()
		if data, rerr := os.ReadFile(partialPath); rerr == nil {
			out.partialPrefix = data
		}
	}

	// Let the single writer finish.
	hub.release()
	out.single = <-singleDone
	if rangedFinished {
		out.ranged = rangedResult
	} else {
		out.ranged = <-rangedDone
	}

	if data, rerr := os.ReadFile(repo.BlobPath(sha)); rerr == nil {
		out.blobExists = true
		out.blob = data
	}
	return out
}

// TestConcurrentBlobPublicationNeverMixesWriters is the core publication
// regression: two routes writing the same blob key must never produce a blob
// that mixes their byte streams, and the loser must not corrupt the winner.
func TestConcurrentBlobPublicationNeverMixesWriters(t *testing.T) {
	const total = 4_096
	streams := newPublicationStreams(total)
	sha := singleTestSHA(streams.a)
	cacheDir := t.TempDir()
	hub := newTwoWriterHub(t, total, sha, streams)

	out := runTwoWriters(t, hub, cacheDir, sha, streams)

	if !out.domainWaited {
		t.Error("the second writer did not wait for the blob key's ownership domain")
	}
	if out.single.err != nil {
		t.Errorf("the single-request writer failed although it owned the blob key: %v", out.single.err)
	}
	if out.ranged.err != nil {
		t.Errorf("the ranged writer failed: %v", out.ranged.err)
	}
	if !out.blobExists {
		t.Fatal("no blob was published")
	}
	if !streams.isSingleStream(out.blob) {
		t.Errorf("the published blob mixes the two writers' byte streams (%d bytes, first=%#x last=%#x)",
			len(out.blob), out.blob[0], out.blob[len(out.blob)-1])
	}
}

// TestConcurrentBlobPublicationKeepsTheProvenDigest covers the digest
// oracle: when both routes transfer the same bytes, the published blob must
// hash to the declared digest and the cache status must report it complete.
//
// This case does not discriminate against the historical behavior (identical
// bytes cannot be mixed into something else); it pins the property that the
// published blob is the transferred content and nothing else.
func TestConcurrentBlobPublicationKeepsTheProvenDigest(t *testing.T) {
	const total = 4_096
	streams := newPublicationStreams(total)
	sha := singleTestSHA(streams.a)
	cacheDir := t.TempDir()
	hub := newTwoWriterHub(t, total, sha, streams)
	hub.sameStream = true

	out := runTwoWriters(t, hub, cacheDir, sha, streams)

	if out.single.err != nil {
		t.Errorf("the single-request writer failed: %v", out.single.err)
	}
	if !out.blobExists {
		t.Fatal("no blob was published")
	}
	if got := singleTestSHA(out.blob); got != sha {
		t.Errorf("the published blob hashes to %s, want %s", got, sha)
	}
	repo, err := NewHFCache(cacheDir, time.Minute).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	status, _, cerr := repo.CheckBlob(sha)
	if cerr != nil || status != BlobComplete {
		t.Errorf("cache status after the publication = %v, %v; want %v", status, cerr, BlobComplete)
	}
}

// TestCrossRouteLivePartialIsNeitherAdoptedNorDeleted pins the partial
// namespace rule: while the single-request writer owns the blob key, its LIVE
// partial must be neither adopted nor deleted by the ranged route.
func TestCrossRouteLivePartialIsNeitherAdoptedNorDeleted(t *testing.T) {
	const total = 4_096
	streams := newPublicationStreams(total)
	sha := singleTestSHA(streams.a)
	cacheDir := t.TempDir()
	hub := newTwoWriterHub(t, total, sha, streams)

	out := runTwoWriters(t, hub, cacheDir, sha, streams)

	if !out.domainWaited {
		t.Fatal("the second writer did not wait for the blob key's ownership domain")
	}
	if out.partialSize < 0 {
		t.Fatal("the single-request writer's live partial was deleted by the other route")
	}
	if want := int64(total / 2); out.partialSize != want {
		t.Errorf("the live partial is %d bytes, want %d: it was rewritten by the other route", out.partialSize, want)
	}
	if !bytes.Equal(out.partialPrefix, streams.b[:total/2]) {
		t.Error("the live partial's content was rewritten by the other route")
	}
	if out.single.err != nil {
		t.Errorf("the single-request writer failed although the other route never touched its partial: %v", out.single.err)
	}
	if !streams.isSingleStream(out.blob) {
		t.Error("the published blob mixes the two writers' byte streams")
	}
}

// TestPublishBlobNoReplaceRefusesToOverwrite pins the placement primitive
// itself: an existing content-addressed blob is completion proof and is never
// replaced.
func TestPublishBlobNoReplaceRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "staging")
	dst := filepath.Join(dir, "blob")
	winner := []byte("the proven winner")
	loser := []byte("the unproven loser")
	if err := os.WriteFile(src, loser, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, winner, 0o644); err != nil {
		t.Fatal(err)
	}
	// The publication runs as a domain participant: the domain of the blob
	// key it publishes.
	domain, err := acquireBlobDomain(t.Context(), dst, filepath.Join(dir, "owner"))
	if err != nil {
		t.Fatal(err)
	}
	defer domain.release()

	err = blobPublishPlace(src, dst, domain)
	if !errors.Is(err, errBlobTargetExists) {
		t.Fatalf("placement over an existing blob = %v, want errBlobTargetExists", err)
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil || !bytes.Equal(got, winner) {
		t.Fatalf("the published blob was replaced: %q, %v", got, rerr)
	}
	// The refused payload is left for the caller to discard.
	if _, serr := os.Stat(src); serr != nil {
		t.Errorf("the refused payload was removed by the placement: %v", serr)
	}
}

// TestNegativeControlStatThenRenameReplacesTheWinner is the negative control
// for the placement primitive: the historical "stat the target, then rename
// over it" behavior replaces a blob that another writer already published.
//
// The control interleaves the two placements between the stat and the rename,
// which is exactly the window the old code left open.
func TestNegativeControlStatThenRenameReplacesTheWinner(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "staging-1")
	second := filepath.Join(dir, "staging-2")
	dst := filepath.Join(dir, "blob")
	winner := []byte("the proven winner")
	loser := []byte("the unproven loser")
	if err := os.WriteFile(first, winner, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, loser, 0o644); err != nil {
		t.Fatal(err)
	}

	// The historical placement: stat, then a rename that replaces whatever
	// is at the target. Both placements pass their stat before either
	// renames, which is the window the no-replace primitive closes.
	gate := make(chan struct{})
	winnerRenamed := make(chan struct{})
	var arrived int
	var mu sync.Mutex
	oldPlace := blobPublishPlace
	blobPublishPlace = func(src, target string, _ *blobDomain) error {
		if _, err := os.Stat(target); err == nil {
			return errBlobTargetExists
		}
		mu.Lock()
		arrived++
		if arrived == 2 {
			close(gate)
		}
		mu.Unlock()
		<-gate
		if src == second {
			// The loser publishes after the winner, so the replacement is
			// deterministic rather than a race.
			<-winnerRenamed
		}
		renameErr := os.Rename(src, target)
		if src == first {
			close(winnerRenamed)
		}
		return renameErr
	}
	defer func() { blobPublishPlace = oldPlace }()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = blobPublishPlace(first, dst, nil) }()
	go func() { defer wg.Done(); _ = blobPublishPlace(second, dst, nil) }()
	wg.Wait()

	got, rerr := os.ReadFile(dst)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !bytes.Equal(got, loser) {
		t.Fatalf("the historical stat-then-rename placement did not replace the winner: %q", got)
	}
	t.Logf("negative control: the historical stat-then-rename placement replaced the winner's bytes with %q", got)
}
