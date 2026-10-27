// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Both transfer routes target the same blob key. The hub returns distinct
// payloads and holds the single-request writer mid-transfer, making a mixed
// publication or deletion of a live partial observable without sleeps.

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

type publicationStreams struct {
	a []byte
	b []byte
}

func newPublicationStreams(total int) publicationStreams {
	a := bytes.Repeat([]byte{0xA5}, total)
	b := bytes.Repeat([]byte{0x5A}, total)
	return publicationStreams{a: a, b: b}
}

func (s publicationStreams) isSingleStream(blob []byte) bool {
	if bytes.Equal(blob, s.a) {
		return true
	}
	return bytes.Equal(blob, s.b)
}

type twoWriterHub struct {
	srv         *httptest.Server
	releaseFull chan struct{}
	releaseOnce sync.Once
	// sameStream makes the full-body response serve the same bytes as the
	// ranged one, so both writers transfer identical content.
	sameStream bool
}

func (h *twoWriterHub) release() {
	h.releaseOnce.Do(func() { close(h.releaseFull) })
}

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

// Ranged GETs serve A; full GETs serve B (or A with sameStream) and pause halfway.
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

type writerResult struct {
	err   error
	skips []string
}

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

type twoWriterOutcome struct {
	single        writerResult
	ranged        writerResult
	blob          []byte
	blobExists    bool
	partialSize   int64
	partialPrefix []byte
	domainWaited  bool
}

// runTwoWriters overlaps a held single-request partial with a ranged writer
// and captures state before releasing the first transfer.
func runTwoWriters(t *testing.T, hub *twoWriterHub, cacheDir, sha string, streams publicationStreams) twoWriterOutcome {
	t.Helper()
	repo, err := NewHFCache(cacheDir).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	partialPath := filepath.Join(repo.BlobsDir(), "tmp-"+sha+".part")

	// Observe actual domain contention before releasing the first writer.
	domainWait := make(chan struct{}, 1)
	oldHook := blobDomainContentionHook
	var hookOnce sync.Once
	blobDomainContentionHook = func(string) {
		hookOnce.Do(func() { domainWait <- struct{}{} })
	}
	defer func() { blobDomainContentionHook = oldHook }()

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
		// The ranged writer finished before domain contention was observed.
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

// The identical-payload control verifies the published digest and complete status.
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
	repo, err := NewHFCache(cacheDir).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	status, _, cerr := repo.CheckBlob(sha)
	if cerr != nil || status != BlobComplete {
		t.Errorf("cache status after the publication = %v, %v; want %v", status, cerr, BlobComplete)
	}
}

// The ranged route must neither adopt nor delete a live single-route partial.
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
	if out.ranged.err != nil {
		t.Errorf("the ranged writer failed: %v", out.ranged.err)
	}
	if !out.blobExists {
		t.Fatal("no blob was published")
	}
	if !streams.isSingleStream(out.blob) {
		t.Error("the published blob mixes the two writers' byte streams")
	}
}

// Publication refuses an existing target without changing either payload.
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
	// Hold the destination's domain for fallback placement too.
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
	if _, serr := os.Stat(src); serr != nil {
		t.Errorf("the refused payload was removed by the placement: %v", serr)
	}
}
