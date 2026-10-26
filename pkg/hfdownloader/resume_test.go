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

// TestDownloadMultipart_ResumesPartialPart seeds a legacy pre-#140
// .part-NN/.parts.json state and verifies the one-time upgrade migration:
// a full part is imported as a PREFIX (its final byte is reserved and
// revalidated, never reused as completion proof), the half-done part 1
// resumes from its exact durable offset minus that reserved byte, and the
// final bytes are exact — with no new .part-NN payload files created and the
// legacy artifacts removed after the successful finalization.
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
	if err := writeMultipartResumeLayout(dst, buildMultipartResumeLayout(int64(len(full)), nParts, int64(partSize))); err != nil {
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

	_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	mu.Lock()
	gotRanges := append([]string(nil), rangeHdrs...)
	mu.Unlock()

	// Part 0 must never be re-downloaded as a whole: a full part is a prefix,
	// not completion proof, so only its final byte is revalidated.
	part0WholeRange := fmt.Sprintf("bytes=0-%d", partSize-1)
	part0LastByte := fmt.Sprintf("bytes=%d-%d", partSize-1, partSize-1)
	gotPart0LastByte := false
	for _, rh := range gotRanges {
		if rh == part0WholeRange {
			t.Errorf("part 0 was re-downloaded whole (range %q); ranges=%v", part0WholeRange, gotRanges)
			break
		}
		if rh == part0LastByte {
			gotPart0LastByte = true
		}
	}
	if !gotPart0LastByte {
		t.Errorf("part 0's reserved final byte was not revalidated: wanted %q in ranges=%v", part0LastByte, gotRanges)
	}

	// Part 1 is a prefix too: its own last byte is reserved, so the worker
	// resumes one byte before the durable offset and re-requests the suffix.
	wantPart1 := fmt.Sprintf("bytes=%d-%d", partSize+part1Have-1, 2*partSize-1)
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

// TestDownloadMultipart_FullLegacyPartReservesFinalByte is the .part-NN
// counterpart of the single-`.part` rule: a full legacy part is a PREFIX, not
// a completed range, because the pre-#140 layout records byte ranges and never
// clean response framing. Its reserved final byte is re-requested, so a torn
// last byte of a "complete" part can never be published, while the rest of the
// part is reused instead of re-transferred.
func TestDownloadMultipart_FullLegacyPartReservesFinalByte(t *testing.T) {
	const total = 4_000
	const nParts = 2
	const partSize = total / nParts
	full := rangedTestPayload(total)
	torn := append([]byte(nil), full...)
	torn[partSize-1] ^= 0xFF // the last byte of part 0

	dst := filepath.Join(t.TempDir(), "blobs", "tmp-full-part")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".part-00", torn[:partSize], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".part-01", torn[partSize:], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeMultipartResumeLayout(dst, buildMultipartResumeLayout(total, nParts, partSize)); err != nil {
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

	it := PlanItem{RelativePath: "full-part.bin", URL: srv.URL + "/full-part.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: nParts, Retries: 0, StallTimeout: "0", Verify: "size"}
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), rangeHdrs...)
	mu.Unlock()

	// Only the reserved final byte of each full part is re-requested; a whole
	// part is never downloaded again.
	want := []string{
		fmt.Sprintf("bytes=%d-%d", partSize-1, partSize-1),
		fmt.Sprintf("bytes=%d-%d", total-1, total-1),
	}
	if len(got) != len(want) || !containsRange(got, want[0]) || !containsRange(got, want[1]) {
		t.Errorf("payload GETs = %v, want exactly the reserved final bytes %v", got, want)
	}
	for _, rh := range got {
		if rh == fmt.Sprintf("bytes=0-%d", partSize-1) {
			t.Errorf("the full legacy part 0 was re-downloaded whole: %v", got)
		}
	}
	// The torn last byte of part 0 must never reach the published file.
	assertExactFile(t, dst, full)
}

// TestDownloadMultipart_LegacyPartsWithoutLayoutAreDiscarded verifies that
// pre-#140 part files with no .parts.json layout have an unknown byte-range
// contract and are not reused: the next run re-downloads from scratch and
// removes the stale parts. A valid layout, by contrast, maps 1:1 and preserves
// committed bytes across a concurrency change; see
// TestRangedMultipart_ConcurrencyChangePreservesCommitted.
func TestDownloadMultipart_LegacyPartsWithoutLayoutAreDiscarded(t *testing.T) {
	tmpDir := t.TempDir()
	const totalSize = 16000
	full := make([]byte, totalSize)
	for i := range full {
		full[i] = byte(i % 251)
	}

	dst := filepath.Join(tmpDir, "blobs", "tmp-change-concurrency")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	// Simulate an interrupted previous run with 8 connections but no
	// layout record: the parts cannot be mapped to byte ranges, so they must
	// be discarded rather than reused by index under a different worker count.
	const oldParts = 8
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	}))
	defer srv.Close()

	it := PlanItem{
		RelativePath: "change.bin",
		URL:          srv.URL + "/change.bin",
		Size:         int64(len(full)),
		AcceptRanges: true,
	}
	cfg := Settings{Concurrency: 4, Retries: 0}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("final content mismatch after concurrency change")
	}
	parts, err := multipartPartFiles(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 0 {
		t.Errorf("stale part files were not cleaned up: %v", parts)
	}
	if _, err := os.Stat(multipartLayoutMetaPath(dst)); !os.IsNotExist(err) {
		t.Errorf("multipart layout metadata should be removed after success, stat err: %v", err)
	}
}

// TestDownloadMultipart_MigratesLegacySinglePart verifies that a leftover
// dst.part is a valid sequential prefix when no .part-NN/layout state exists:
// the first range(s) are committed up to min(part length, size), the rest are
// tiled at the configured worker count, and the obsolete .part file is removed
// only after the migrated layout is durable.
func TestDownloadMultipart_MigratesLegacySinglePart(t *testing.T) {
	tmpDir := t.TempDir()
	const totalSize = 12000
	full := make([]byte, totalSize)
	for i := range full {
		full[i] = byte(i % 251)
	}
	const prefix = 3000

	dst := filepath.Join(tmpDir, "blobs", "tmp-single-prefix")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".part", full[:prefix], 0o644); err != nil {
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

	it := PlanItem{RelativePath: "single-prefix.bin", URL: srv.URL + "/single-prefix.bin", Size: totalSize, AcceptRanges: true}
	cfg := Settings{Concurrency: 3, Retries: 0, StallTimeout: "0"}
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), rangeHdrs...)
	mu.Unlock()
	migratedPrefix := false
	for _, rh := range got {
		if rh == "bytes=3000-3999" {
			migratedPrefix = true
		}
	}
	if !migratedPrefix {
		t.Errorf("single .part prefix was not migrated into the first range: requests=%v", got)
	}
	assertExactFile(t, dst, full)
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Errorf("legacy .part must be removed after the migrated state is durable, stat err=%v", err)
	}
}

// TestDownloadMultipart_LegacyPartsWinOverSinglePart verifies the priority
// rule: a valid .part-NN/.parts.json migration wins over a leftover dst.part,
// and the obsolete .part is removed with the other legacy artifacts. A full
// legacy part is still only a prefix: its final byte is revalidated, and the
// short part 1 resumes one byte before its durable offset.
func TestDownloadMultipart_LegacyPartsWinOverSinglePart(t *testing.T) {
	tmpDir := t.TempDir()
	const totalSize = 10000
	full := make([]byte, totalSize)
	for i := range full {
		full[i] = byte(i % 251)
	}
	const nParts = 4
	const partSize = totalSize / nParts
	const part1Have = partSize / 2

	dst := filepath.Join(tmpDir, "blobs", "tmp-parts-win")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	// Valid multipart family: part 0 complete, part 1 half done.
	if err := os.WriteFile(dst+".part-00", full[:partSize], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".part-01", full[partSize:partSize+part1Have], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeMultipartResumeLayout(dst, buildMultipartResumeLayout(int64(len(full)), nParts, int64(partSize))); err != nil {
		t.Fatal(err)
	}
	// A stale single prefix also exists; it loses to the valid layout.
	if err := os.WriteFile(dst+".part", bytes.Repeat([]byte("x"), totalSize/2), 0o644); err != nil {
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

	it := PlanItem{RelativePath: "parts-win.bin", URL: srv.URL + "/parts-win.bin", Size: totalSize, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), rangeHdrs...)
	mu.Unlock()
	wantPart1 := fmt.Sprintf("bytes=%d-%d", partSize+part1Have-1, 2*partSize-1)
	part0LastByte := fmt.Sprintf("bytes=%d-%d", partSize-1, partSize-1)
	found := false
	foundPart0LastByte := false
	for _, rh := range got {
		if rh == fmt.Sprintf("bytes=0-%d", partSize-1) {
			t.Errorf("legacy part 0 was re-downloaded whole: %v", got)
		}
		if rh == part0LastByte {
			foundPart0LastByte = true
		}
		if rh == wantPart1 {
			found = true
		}
	}
	if !found {
		t.Errorf("valid .part-NN layout did not win: wanted %q in %v", wantPart1, got)
	}
	if !foundPart0LastByte {
		t.Errorf("legacy part 0's reserved final byte was not revalidated: wanted %q in %v", part0LastByte, got)
	}
	assertExactFile(t, dst, full)
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Errorf("obsolete .part must be removed when the .part-NN migration wins, stat err=%v", err)
	}
}

// TestDownloadMultipart_SinglePartMigrationSurvivesFlushFailure pins the
// durability order for the single .part migration: the prefix must survive a
// failed first metadata flush and be migratable again on recovery.
func TestDownloadMultipart_SinglePartMigrationSurvivesFlushFailure(t *testing.T) {
	tmpDir := t.TempDir()
	const totalSize = 8000
	full := make([]byte, totalSize)
	for i := range full {
		full[i] = byte(i % 251)
	}
	const prefix = 2500

	dst := filepath.Join(tmpDir, "blobs", "tmp-single-survive")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".part", full[:prefix], 0o644); err != nil {
		t.Fatal(err)
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

	it := PlanItem{RelativePath: "single-survive.bin", URL: srv.URL + "/single-survive.bin", Size: totalSize, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}
	if err := os.Mkdir(dst+".incomplete.meta.tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err == nil {
		t.Fatal("download succeeded despite the forced metadata flush failure")
	}
	gotPart, err := os.ReadFile(dst + ".part")
	if err != nil {
		t.Fatalf("single .part was removed before the migrated state was durable: %v", err)
	}
	if !bytes.Equal(gotPart, full[:prefix]) {
		t.Errorf("single .part changed during the failed migration")
	}

	if err := os.Remove(dst + ".incomplete.meta.tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("recovery downloadMultipart: %v", err)
	}
	assertExactFile(t, dst, full)
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Errorf("single .part survived successful recovery, stat err=%v", err)
	}
}

// TestDownloadMultipart_LegacyMigrationSurvivesFlushFailure pins the
// failure window of the one-time legacy upgrade migration (issue #140,
// criterion 11): the legacy .part-NN/.parts.json artifacts are the only
// finishable copy of a valid partial until the migrated range layout is
// durably persisted in the staging record, so they must survive a
// staging-metadata flush failure. The first flushMeta is forced to fail
// deterministically by pre-creating its atomic-write staging path
// (staging+".meta.tmp") as a directory, which makes the write inside
// writeStagingMeta fail; the legacy bytes must still be on disk so a
// later run can migrate again, and that recovery run must finish the
// download and then remove the obsolete artifacts.
func TestDownloadMultipart_LegacyMigrationSurvivesFlushFailure(t *testing.T) {
	tmpDir := t.TempDir()
	const totalSize = 10000
	full := make([]byte, totalSize)
	for i := range full {
		full[i] = byte(i % 251)
	}
	const nParts = 4
	const partSize = totalSize / nParts

	dst := filepath.Join(tmpDir, "blobs", "tmp-migrate")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	// Legacy pre-#140 state: part 0 fully complete, part 1 half done.
	part0 := full[0:partSize]
	const part1Have = partSize / 2
	if err := os.WriteFile(dst+".part-00", part0, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".part-01", full[partSize:partSize+part1Have], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeMultipartResumeLayout(dst, buildMultipartResumeLayout(int64(len(full)), nParts, int64(partSize))); err != nil {
		t.Fatal(err)
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

	it := PlanItem{
		RelativePath: "test.bin",
		URL:          srv.URL + "/test.bin",
		Size:         int64(len(full)),
		AcceptRanges: true,
	}
	cfg := Settings{Concurrency: nParts, Retries: 0}

	// Force the first staging-metadata flush to fail: writeStagingMeta
	// stages through staging+".meta.tmp", and a directory at that path
	// cannot be opened for writing.
	if err := os.Mkdir(dst+".incomplete.meta.tmp", 0o755); err != nil {
		t.Fatal(err)
	}

	// Run 1: the migration copies the part bytes into the staging file,
	// then the first flush of the migrated layout fails.
	_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {})
	if err == nil {
		t.Fatal("download succeeded despite the forced metadata flush failure")
	}

	// The legacy artifacts — and their bytes — must survive the failure
	// so a later run can still finish the partial (criterion 11: never
	// silently discard a valid partial after upgrade).
	assertPartBytes := func(path string, want []byte) {
		t.Helper()
		got, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Errorf("legacy artifact %s was discarded before the migrated state was durable: %v", filepath.Base(path), rerr)
			return
		}
		if !bytes.Equal(got, want) {
			t.Errorf("legacy artifact %s changed: got %d bytes, want %d", filepath.Base(path), len(got), len(want))
		}
	}
	assertPartBytes(dst+".part-00", part0)
	assertPartBytes(dst+".part-01", full[partSize:partSize+part1Have])
	if _, lerr := os.Stat(multipartLayoutMetaPath(dst)); lerr != nil {
		t.Errorf("legacy layout metadata must survive the flush failure: %v", lerr)
	}

	// Recovery: with the flush failure cleared, the next run re-migrates
	// the legacy partial, finishes it, and removes the obsolete artifacts
	// once the migrated layout is durable.
	if err := os.Remove(dst + ".incomplete.meta.tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("recovery downloadMultipart: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("final content mismatch: got len=%d want len=%d", len(got), len(full))
	}
	parts, err := multipartPartFiles(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 0 {
		t.Errorf("legacy part files must be removed after the migrated state is durable: %v", parts)
	}
	if _, err := os.Stat(multipartLayoutMetaPath(dst)); !os.IsNotExist(err) {
		t.Errorf("legacy layout metadata must be removed after successful migration, stat err: %v", err)
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

// TestDownloadMultipart_UnusablePartLayoutFallsBackToSinglePart covers M1: a
// pre-#140 .parts.json layout whose every part is missing/empty/oversized
// contributes no bytes and is not a usable migrated state. The layout family
// must be discarded and a valid dst.part prefix migrated instead — with
// LegacyMigrated set only because bytes were actually copied.
func TestDownloadMultipart_UnusablePartLayoutFallsBackToSinglePart(t *testing.T) {
	const total = 8_000
	const prefix = 3_000
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "unusable-layout.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	// A syntactically valid two-part layout that tiles [0,total)...
	layout := buildMultipartResumeLayout(total, 2, total/2)
	if err := writeMultipartResumeLayout(dst, layout); err != nil {
		t.Fatal(err)
	}
	// ...whose parts are useless: part 0 is oversized, part 1 is empty.
	if err := os.WriteFile(fmt.Sprintf("%s.part-%02d", dst, 0), make([]byte, total/2+64), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fmt.Sprintf("%s.part-%02d", dst, 1), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// The valid sequential prefix that must survive the upgrade.
	if err := os.WriteFile(dst+".part", full[:prefix], 0o644); err != nil {
		t.Fatal(err)
	}

	srv, gotRanges := rangedTestServer(t, full)
	it := PlanItem{RelativePath: "unusable-layout.bin", URL: srv.URL + "/unusable-layout.bin", Size: total, AcceptRanges: true, LFS: true, SHA256: sha}
	cfg := Settings{Concurrency: 1, Retries: 0, StallTimeout: "0", Verify: "size"}

	hashPassCount.Store(0)
	verified, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}
	// The migrated prefix was content-proven (one pass) and reported as
	// already verified.
	if !verified {
		t.Error("migrated .part bytes were not content-proven")
	}
	if got := hashPassCount.Load(); got != 1 {
		t.Errorf("migration proof performed %d full-file passes; want exactly 1", got)
	}
	// The valid .part prefix was resumed instead of being deleted and
	// re-downloaded from zero.
	if len(*gotRanges) != 1 || (*gotRanges)[0] != fmt.Sprintf("bytes=%d-%d", prefix, total-1) {
		t.Errorf("fetched %v, want exactly the suffix bytes=%d-%d (the layout had no usable bytes)", *gotRanges, prefix, total-1)
	}
	assertExactFile(t, dst, full)
	for _, p := range []string{
		multipartLayoutMetaPath(dst),
		fmt.Sprintf("%s.part-%02d", dst, 0),
		fmt.Sprintf("%s.part-%02d", dst, 1),
		dst + ".part",
	} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("legacy artifact %s survived finalization, stat err=%v", filepath.Base(p), serr)
		}
	}
}
