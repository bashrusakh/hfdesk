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
	if err := os.WriteFile(dst+".part", full[:partialN], 0o644); err != nil {
		t.Fatal(err)
	}

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

	it := PlanItem{
		RelativePath: "test.bin",
		URL:          srv.URL + "/test.bin",
		Size:         int64(len(full)),
		AcceptRanges: false,
	}
	cfg := Settings{Retries: 0}

	err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
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
	if err := os.WriteFile(dst+".part", full, 0o644); err != nil {
		t.Fatal(err)
	}

	var requestCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Write(full)
	}))
	defer srv.Close()

	it := PlanItem{
		RelativePath: "test.bin",
		URL:          srv.URL + "/test.bin",
		Size:         int64(len(full)),
	}
	err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Retries: 0}, it, dst, func(ProgressEvent) {})
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
	// Pre-seed with garbage partial
	if err := os.WriteFile(dst+".part", bytes.Repeat([]byte("x"), partialN), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Intentionally ignore Range header and return 200 with full body.
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusOK)
		w.Write(full)
	}))
	defer srv.Close()

	it := PlanItem{
		RelativePath: "test.bin",
		URL:          srv.URL + "/test.bin",
		Size:         int64(len(full)),
	}
	err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Retries: 0}, it, dst, func(ProgressEvent) {})
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

// TestDownloadMultipart_ResumesPartialPart verifies that when a part file has
// been partially written (not fully complete, not empty), downloadMultipart
// resumes from the existing offset via a Range request rather than truncating
// and restarting. Also verifies that fully-complete parts are skipped.
func TestDownloadMultipart_ResumesPartialPart(t *testing.T) {
	tmpDir := t.TempDir()
	// Build a deterministic payload where each byte is its index mod 251.
	const totalSize = 10000
	full := make([]byte, totalSize)
	for i := range full {
		full[i] = byte(i % 251)
	}
	const nParts = 4
	const partSize = totalSize / nParts // 2500

	dst := filepath.Join(tmpDir, "blobs", "tmp-multi")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	// Part 0: fully complete
	if err := os.WriteFile(dst+".part-00", full[0:partSize], 0o644); err != nil {
		t.Fatal(err)
	}
	// Part 1: half done (correct prefix)
	const part1Have = partSize / 2
	if err := os.WriteFile(dst+".part-01", full[partSize:partSize+part1Have], 0o644); err != nil {
		t.Fatal(err)
	}
	// Parts 2, 3: missing

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
		RelativePath: "test.bin",
		URL:          srv.URL + "/test.bin",
		Size:         int64(len(full)),
		AcceptRanges: true,
	}
	cfg := Settings{Concurrency: nParts, Retries: 0}

	err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	mu.Lock()
	gotRanges := append([]string(nil), rangeHdrs...)
	mu.Unlock()

	// Part 0 should never have been requested (fully complete on disk).
	part0Range := fmt.Sprintf("bytes=0-%d", partSize-1)
	for _, rh := range gotRanges {
		if rh == part0Range {
			t.Errorf("part 0 was re-downloaded (range %q); ranges=%v", part0Range, gotRanges)
			break
		}
	}

	// Part 1 should have been resumed from part1Have bytes in, not from the part start.
	wantPart1 := fmt.Sprintf("bytes=%d-%d", partSize+part1Have, 2*partSize-1)
	found := false
	for _, rh := range gotRanges {
		if rh == wantPart1 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("part 1 not resumed from offset: wanted %q in ranges=%v", wantPart1, gotRanges)
	}

	// Final file should match.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("final content mismatch: len got=%d want=%d", len(got), len(full))
	}
	// Part files should have been cleaned up after assembly.
	for i := 0; i < nParts; i++ {
		p := fmt.Sprintf("%s.part-%02d", dst, i)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("part file %s should be removed, stat err: %v", p, err)
		}
	}
}
