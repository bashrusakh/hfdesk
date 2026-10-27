// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// downloadSingleErr adapts downloadSingle's (contentVerified, error) result to
// the error-only shape used by goroutine result channels.
func downloadSingleErr(ctx context.Context, httpc *http.Client, token string, job Job, cfg Settings, it PlanItem, dst string, opts singleOpts, emit func(ProgressEvent)) error {
	_, err := downloadSingle(ctx, httpc, token, job, cfg, it, dst, opts, emit)
	return err
}

// TestDownloadSingle_ResumesFromExistingPartial verifies that when a .part file
// already exists on disk with partial bytes (e.g. left over from a Ctrl+C'd run),
// downloadSingle issues a Range request for the remaining bytes instead of
// re-downloading from zero. This is the core of the github issue #70 bug where
// single-file downloads always restarted from scratch after an interrupt.
func TestDownloadSingle_ResumesFromExistingPartial(t *testing.T) {
	tmpDir := t.TempDir()
	full := []byte("the quick brown fox jumps over the lazy dog, and then keeps running for a while longer")
	const partialN = 25

	dst := filepath.Join(tmpDir, "blobs", "tmp-single")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	job := Job{Repo: "o/r"}
	it := PlanItem{
		RelativePath: "test.bin",
		Size:         int64(len(full)),
		AcceptRanges: false,
	}
	// The staged bytes are inherited only when the identity record proves
	// they belong to this item, so the fixture carries that record.
	seedSinglePartial(t, dst, full[:partialN], job, it, singleTestIdentity)

	var (
		mu           sync.Mutex
		gotRanges    []string
		bytesServed  int64
		requestCount int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotRanges = append(gotRanges, r.Header.Get("Range"))
		requestCount++
		mu.Unlock()
		// ServeContent honors Range and emits 206 with correct Content-Range.
		rs := bytes.NewReader(full)
		http.ServeContent(w, r, "", time.Time{}, rs)
		// Count bytes actually served (after ServeContent). Not exact but acceptable for the assertion below.
		mu.Lock()
		if r.Header.Get("Range") != "" {
			bytesServed += int64(len(full) - partialN)
		} else {
			bytesServed += int64(len(full))
		}
		mu.Unlock()
	}))
	defer srv.Close()

	it.URL = srv.URL + "/test.bin"
	cfg := Settings{Retries: 0}

	_, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if requestCount != 1 {
		t.Errorf("expected 1 HTTP request, got %d", requestCount)
	}
	wantRange := fmt.Sprintf("bytes=%d-%d", partialN, len(full)-1)
	if len(gotRanges) == 0 || gotRanges[0] != wantRange {
		t.Errorf("expected Range header %q, got %v", wantRange, gotRanges)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("final content mismatch:\n got  %q\n want %q", got, full)
	}
	// .part should be removed after rename
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Errorf(".part file should not exist after success, stat err: %v", err)
	}
}

// TestDownloadSingle_CompletePartialIsFinalized verifies that if a .part file
// already contains all the bytes (e.g. interrupted right before rename), the
// next run finalizes it without a network request.
func TestDownloadSingle_CompletePartialIsFinalized(t *testing.T) {
	tmpDir := t.TempDir()
	full := []byte("already fully downloaded content")

	dst := filepath.Join(tmpDir, "blobs", "tmp-complete")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	job := Job{Repo: "o/r"}
	it := PlanItem{
		RelativePath: "test.bin",
		Size:         int64(len(full)),
	}
	// The complete partial is published without a request only when its
	// identity record proves the bytes belong to this item AND records that
	// the producing run saw the response body end cleanly at the exact size.
	seedSinglePartial(t, dst, full, job, it, completeSingleIdentity())

	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Write(full)
	}))
	defer srv.Close()

	it.URL = srv.URL + "/test.bin"
	_, err := downloadSingle(context.Background(), srv.Client(), "", job, Settings{Retries: 0}, it, dst, singleTestIdentity, func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}
	if requestCount != 0 {
		t.Errorf("expected 0 HTTP requests (already complete), got %d", requestCount)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("final content mismatch")
	}
}

// TestDownloadSingle_ServerIgnoresRangeReturnsFull verifies that when the server
// returns 200 OK (full body) in response to a Range request, the resume logic
// correctly restarts from zero instead of appending duplicate bytes.
func TestDownloadSingle_ServerIgnoresRangeReturnsFull(t *testing.T) {
	tmpDir := t.TempDir()
	full := []byte("server does not honor range requests at all content")
	const partialN = 10

	dst := filepath.Join(tmpDir, "blobs", "tmp-noRange")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	job := Job{Repo: "o/r"}
	it := PlanItem{
		RelativePath: "test.bin",
		Size:         int64(len(full)),
	}
	// Pre-seed with a garbage partial that the identity record ties to this
	// item, so the ignored-Range restart is what discards it.
	seedSinglePartial(t, dst, bytes.Repeat([]byte("x"), partialN), job, it, singleTestIdentity)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Intentionally ignore Range header and return 200 with full body.
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusOK)
		w.Write(full)
	}))
	defer srv.Close()

	it.URL = srv.URL + "/test.bin"
	_, err := downloadSingle(context.Background(), srv.Client(), "", job, Settings{Retries: 0}, it, dst, singleTestIdentity, func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("final content mismatch: got len=%d want len=%d", len(got), len(full))
	}
}

// TestDownloadMultipart_OldMultipartArtifactsAreNotResumed pins the S0
// behavior: a pre-#140 .part-NN/.parts.json family and a sidecarless
// single-route .part are old/unsupported formats, so they are neither resumed
// nor migrated. The file is downloaded from zero (one fresh range layout), the
// obsolete multipart family is discarded under the claim, the single-route
// .part is left byte-identically untouched, and the published content is
// exactly the served payload.
func TestDownloadMultipart_OldMultipartArtifactsAreNotResumed(t *testing.T) {
	tmpDir := t.TempDir()
	const totalSize = 16000
	full := make([]byte, totalSize)
	for i := range full {
		full[i] = byte(i % 251)
	}

	dst := filepath.Join(tmpDir, "blobs", "tmp-old-family")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	// A structurally valid pre-#140 layout with two fully-sized parts. The
	// layout is irrelevant now: nothing parses it.
	const oldParts = 2
	oldChunk := totalSize / oldParts
	for i := 0; i < oldParts; i++ {
		start := i * oldChunk
		end := start + oldChunk
		if i == oldParts-1 {
			end = totalSize
		}
		if err := os.WriteFile(fmt.Sprintf("%s.part-%02d", dst, i), full[start:end], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(multipartLayoutMetaPath(dst), []byte(`{"size":16000,"concurrency":2,"parts":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// The single-request route's own staging file: a sidecarless prefix of
	// DIFFERENT bytes. The multipart route must neither import it nor remove
	// it (B1: a route removes only the artifacts it creates).
	stalePrefix := bytes.Repeat([]byte("x"), 4_000)
	if err := os.WriteFile(dst+".part", stalePrefix, 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		mu        sync.Mutex
		rangeHdrs []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		mu.Lock()
		rangeHdrs = append(rangeHdrs, r.Header.Get("Range"))
		mu.Unlock()
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	}))
	defer srv.Close()

	it := PlanItem{
		RelativePath: "old.bin",
		URL:          srv.URL + "/old.bin",
		Size:         int64(len(full)),
		AcceptRanges: true,
	}
	cfg := Settings{Concurrency: 4, Retries: 0, StallTimeout: "0"}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	mu.Lock()
	gotRanges := append([]string(nil), rangeHdrs...)
	mu.Unlock()
	// The fresh claim re-downloads the whole file with the configured worker
	// count; no old-format prefix may contribute a committed byte.
	wantLayout := buildRangeLayout(int64(len(full)), cfg.Concurrency)
	if len(gotRanges) != len(wantLayout) {
		t.Fatalf("payload GET ranges = %v, want one request per fresh range (%d)", gotRanges, len(wantLayout))
	}
	for _, r := range wantLayout {
		want := fmt.Sprintf("bytes=%d-%d", r.Start, r.End)
		if !containsRange(gotRanges, want) {
			t.Errorf("payload GET ranges = %v, missing fresh range %q", gotRanges, want)
		}
	}

	assertExactFile(t, dst, full)

	parts, err := multipartPartFiles(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 0 {
		t.Errorf("obsolete pre-#140 part files were not discarded: %v", parts)
	}
	if _, err := os.Stat(multipartLayoutMetaPath(dst)); !os.IsNotExist(err) {
		t.Errorf("obsolete pre-#140 layout should be discarded after success, stat err: %v", err)
	}
	gotPart, err := os.ReadFile(dst + ".part")
	if err != nil {
		t.Fatalf("the multipart route removed the single-route .part: %v", err)
	}
	if !bytes.Equal(gotPart, stalePrefix) {
		t.Errorf("the multipart route rewrote the single-route .part: got %d bytes, want %d", len(gotPart), len(stalePrefix))
	}
}

// TestDownloadMultipart_EmitsFileFinalizing verifies that once all parts are
// transferred, downloadMultipart announces the local post-processing window
// (part assembly, then verify/store in the caller) with a file_finalizing
// event after the explicit 100% progress reading. Without it the UI sits at
// 100% with no status while multi-GB files are stitched and hashed.
func TestDownloadMultipart_EmitsFileFinalizing(t *testing.T) {
	tmpDir := t.TempDir()
	full := make([]byte, 10000)
	for i := range full {
		full[i] = byte(i % 251)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	}))
	defer srv.Close()

	dst := filepath.Join(tmpDir, "fin.bin")
	it := PlanItem{
		RelativePath: "fin.bin",
		URL:          srv.URL + "/fin.bin",
		Size:         int64(len(full)),
		AcceptRanges: true,
	}

	// The progress ticker emits from its own goroutine, so guard the slice.
	var (
		mu     sync.Mutex
		events []ProgressEvent
	)
	emit := func(ev ProgressEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 4, Retries: 0}, it, dst, multipartOpts{}, emit); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	fullProgressAt, finalizingAt := -1, -1
	for i, ev := range events {
		switch {
		case ev.Event == "file_progress" && ev.Downloaded == ev.Total:
			fullProgressAt = i
		case ev.Event == "file_finalizing":
			finalizingAt = i
		}
	}
	if finalizingAt == -1 {
		t.Fatal("no file_finalizing event emitted")
	}
	if fullProgressAt == -1 {
		t.Fatal("no 100% file_progress event emitted")
	}
	if finalizingAt < fullProgressAt {
		t.Errorf("file_finalizing (index %d) emitted before the 100%% progress reading (index %d)", finalizingAt, fullProgressAt)
	}
}
