// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Deterministic local-server coverage for the issue #140 ranged multipart
// downloader. Every test runs against httptest servers (no network) and
// covers one required case from the issue:
//
//  1. parallel ranges completing out of order → exact final bytes, direct
//     writes into one staging file, no .part-NN payloads;
//  2. wrong/missing Content-Range → reject before publication;
//  3. short and oversized bodies → reject;
//  4. cancellation mid-range → checkpoint never passes written data
//     (TestDownloadMultipart_CancelMidStreamDoesNotClaim100 in
//     flaky_test.go, which compares the committed total against the bytes
//     the server actually delivered);
//  5. restart resumes only missing/uncommitted bytes;
//  6. a preallocated staging file at full logical size with missing ranges
//     never looks complete;
//  7. changing Concurrency across resume preserves committed bytes;
//  8. Verify=size on an LFS file performs no SHA-256 file reread;
//  9. Verify=sha256 detects corruption with exactly one final pass for a
//     parallel transfer and never publishes corrupt bytes;
//  10. a fresh Concurrency==1 strict transfer hashes on write (no second
//     pass); a resumed sequential transfer falls back to one final pass;
//  11. legacy .part-NN/.parts.json upgrade path
//     (TestDownloadMultipart_ResumesPartialPart and
//     TestDownloadMultipart_LegacyPartsWithoutLayoutAreDiscarded in
//     resume_test.go), plus single .part prefix migration;
//  12. two concurrent claims for the same staging file → only one writer;
//  13. stall/retry/cancel/fail-fast behavior (robustness_test.go suite).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rangedTestPayload builds a deterministic payload where each byte is its
// index mod 251.
func rangedTestPayload(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// rangedTestServer serves a fixed payload with correct HEAD/Range handling.
// Every ranged GET is recorded (under mu) in gotRanges.
func rangedTestServer(t *testing.T, full []byte) (*httptest.Server, *[]string) {
	t.Helper()
	var (
		mu        sync.Mutex
		gotRanges []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		mu.Lock()
		gotRanges = append(gotRanges, r.Header.Get("Range"))
		mu.Unlock()
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	}))
	t.Cleanup(srv.Close)
	return srv, &gotRanges
}

// rangedSeedCommit is the RESOLVED commit the seeded staging records carry.
// A staging record authorizes a resume only under a PINNED identity, so the
// seeded state names a resolved commit and states that it is pinned.
const rangedSeedCommit = "1111111111111111111111111111111111111111"

// seedRangedState writes a current-version staging record plus a preallocated staging
// file whose committed intervals hold the correct payload bytes (and
// garbage everywhere else), simulating an interrupted previous run.
//
// The record carries the resolved, PINNED identity (rangedSeedCommit): a
// record written under an unpinned identity never authorizes inheriting its
// bytes, so a resume fixture must state a pinned one.
func seedRangedState(t *testing.T, staging, repo, path string, size int64, ranges []RangeProgress, full []byte) {
	t.Helper()
	if err := os.WriteFile(staging, bytes.Repeat([]byte{0xA5}, int(size)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(staging, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ranges {
		r := ranges[i]
		committed := r.Committed
		if r.Done {
			committed = r.End - r.Start + 1
		}
		if committed > 0 {
			if _, err := out.WriteAt(full[r.Start:r.Start+committed], r.Start); err != nil {
				t.Fatal(err)
			}
		}
	}
	out.Close()
	meta := IncompleteMeta{
		PID:          os.Getpid(),
		Instance:     processInstanceToken, // same process instance: earlier run, no live writer
		Host:         processHostname,
		StartedAt:    time.Now().UTC(),
		Size:         size,
		Version:      stagingMetaVersion,
		Repo:         repo,
		Commit:       rangedSeedCommit,
		CommitPinned: true,
		Path:         path,
		Ranges:       ranges,
	}
	if err := writeStagingMeta(staging+".meta", &meta); err != nil {
		t.Fatal(err)
	}
}

// assertExactFile compares a file's content with want.
func assertExactFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s: content mismatch (got len=%d want len=%d)", filepath.Base(path), len(got), len(want))
	}
}

// containsRange reports whether want appears among the recorded Range request
// headers.
func containsRange(ranges []string, want string) bool {
	for _, r := range ranges {
		if r == want {
			return true
		}
	}
	return false
}

// 1. Parallel ranges completing out of order must produce exact final
// bytes, written directly into one staging file with no .part-NN payloads
// and no assembly copy.
func TestRangedMultipart_OutOfOrderRangesProduceExactBytes(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 20_000
	const nRanges = 4
	full := rangedTestPayload(total)

	dst := filepath.Join(tmpDir, "out-of-order.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	// Hold range 0's response until every other range has been fully
	// served, forcing the workers' writes to complete out of order.
	var servedOthers atomic.Int32
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if start == 0 {
			select {
			case <-releaseFirst:
			case <-r.Context().Done():
				return
			}
		}
		content := full[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(full)))
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusPartialContent)
		if _, err := w.Write(content); err == nil && start != 0 {
			if servedOthers.Add(1) == nRanges-1 {
				releaseOnce.Do(func() { close(releaseFirst) })
			}
		}
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "ooo.bin", URL: srv.URL + "/ooo.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: nRanges, Retries: 1, StallTimeout: "0"}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	assertExactFile(t, dst, full)

	// No payload-artifact residue: the destination plus the persistent
	// ownership lock file (never unlinked, by design) remain in the directory.
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		filepath.Base(dst): true,
		filepath.Base(stagingOwnershipLockPath(dst + ".incomplete")): true,
	}
	for _, e := range entries {
		if !allowed[e.Name()] {
			t.Errorf("unexpected artifact after publish: %s", e.Name())
		}
	}
	if len(entries) != len(allowed) {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("unexpected artifacts after publish: %v", names)
	}
	if _, err := os.Stat(stagingOwnershipLockPath(dst + ".incomplete")); err != nil {
		t.Errorf("the persistent ownership lock file was removed: %v", err)
	}
	if files, _ := multipartPartFiles(dst); len(files) != 0 {
		t.Errorf("no .part-NN payload files may be created, got %v", files)
	}

	// The out-of-order invariant really held: range 0 was the last served.
	if got := servedOthers.Load(); got != nRanges-1 {
		t.Errorf("servedOthers = %d, want %d (range 0 must be released last)", got, nRanges-1)
	}
}

// 2. Every ranged response must carry an exactly matching Content-Range;
// anything else is rejected before publication.
func TestRangedMultipart_RejectsBadContentRange(t *testing.T) {
	const total = 8_000
	full := rangedTestPayload(total)

	cases := []struct {
		name  string
		serve func(w http.ResponseWriter, start, end int64)
	}{
		{"missing Content-Range", func(w http.ResponseWriter, start, end int64) {
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(full[start : end+1])
		}},
		{"wrong start", func(w http.ResponseWriter, start, end int64) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start+1, end, total))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(full[start : end+1])
		}},
		{"wrong end", func(w http.ResponseWriter, start, end int64) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, total))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(full[start : end+1])
		}},
		{"wrong advertised total", func(w http.ResponseWriter, start, end int64) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total+1))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(full[start : end+1])
		}},
		{"malformed header", func(w http.ResponseWriter, start, end int64) {
			w.Header().Set("Content-Range", "bytes=0-7999/8000")
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(full[start : end+1])
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			dst := filepath.Join(tmpDir, "bad-cr.bin")
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.Header().Set("Content-Length", strconv.Itoa(total))
					w.Header().Set("Accept-Ranges", "bytes")
					return
				}
				var start, end int64
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
					http.Error(w, "bad range", http.StatusBadRequest)
					return
				}
				tc.serve(w, start, end)
			}))
			defer srv.Close()

			it := PlanItem{RelativePath: "cr.bin", URL: srv.URL + "/cr.bin", Size: total, AcceptRanges: true}
			cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}

			_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {})
			if err == nil {
				t.Fatal("download succeeded despite a bad Content-Range; the body must be rejected")
			}
			if !strings.Contains(err.Error(), "Content-Range") && !strings.Contains(err.Error(), "Content-Length") {
				t.Errorf("error must identify the protocol violation, got %q", err.Error())
			}

			// No publication, and the staging record survives for resume.
			if _, serr := os.Stat(dst); serr == nil {
				t.Error("destination was published despite rejected ranged responses")
			}
			if _, serr := os.Stat(dst + ".incomplete"); serr != nil {
				t.Errorf("staging file must survive a protocol failure (stat err=%v)", serr)
			}
			if _, serr := os.Stat(dst + ".incomplete.meta"); serr != nil {
				t.Errorf("staging metadata must survive a protocol failure (stat err=%v)", serr)
			}
		})
	}
}

// 3. Short and oversized bodies are errors. The oversized case is caught by
// the header validation (a Content-Length longer than the requested range);
// the bounded writer is unit-checked below for the residual in-copy case.
func TestRangedMultipart_RejectsShortAndOversizedBody(t *testing.T) {
	const total = 8_000
	full := rangedTestPayload(total)

	t.Run("short body", func(t *testing.T) {
		tmpDir := t.TempDir()
		dst := filepath.Join(tmpDir, "short.bin")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", strconv.Itoa(total))
				w.Header().Set("Accept-Ranges", "bytes")
				return
			}
			var start, end int64
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
			content := full[start : end+1]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			w.WriteHeader(http.StatusPartialContent)
			// Send half the declared body, then abort.
			w.Write(content[:len(content)/2])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}))
		defer srv.Close()

		it := PlanItem{RelativePath: "short.bin", URL: srv.URL + "/short.bin", Size: total, AcceptRanges: true}
		cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}
		_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {})
		if err == nil {
			t.Fatal("download succeeded despite a short body")
		}
		if _, serr := os.Stat(dst); serr == nil {
			t.Error("destination was published despite a short body")
		}
		// The delivered prefix bytes are committed: a resume must pick up
		// from the durable high-water mark, not re-download them.
		meta, merr := readStagingMeta(dst + ".incomplete.meta")
		if merr != nil {
			t.Fatalf("staging metadata missing: %v", merr)
		}
		if got := committedTotal(meta.Ranges); got == 0 {
			t.Error("delivered prefix bytes were not committed to the staging record")
		}
		if got := committedTotal(meta.Ranges); got >= total {
			t.Errorf("committed=%d must stay below the total %d", got, total)
		}
	})

	t.Run("oversized body via Content-Length", func(t *testing.T) {
		tmpDir := t.TempDir()
		dst := filepath.Join(tmpDir, "over.bin")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", strconv.Itoa(total))
				w.Header().Set("Accept-Ranges", "bytes")
				return
			}
			var start, end int64
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
			content := full[start : end+1]
			// Advertise MORE bytes than the requested range: rejected before
			// any byte is written.
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
			w.Header().Set("Content-Length", strconv.Itoa(len(content)+64))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content)
			w.Write(make([]byte, 64))
		}))
		defer srv.Close()

		it := PlanItem{RelativePath: "over.bin", URL: srv.URL + "/over.bin", Size: total, AcceptRanges: true}
		cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}
		_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {})
		if err == nil {
			t.Fatal("download succeeded despite an oversized body")
		}
		if _, serr := os.Stat(dst); serr == nil {
			t.Error("destination was published despite an oversized body")
		}
	})

	t.Run("bounded writer refuses past the assigned range", func(t *testing.T) {
		tmpDir := t.TempDir()
		f, err := os.CreateTemp(tmpDir, "bound-*")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		w := newBoundedAtWriter(f, 0, 4)
		if n, err := w.Write([]byte("abcd")); err != nil || n != 4 {
			t.Fatalf("in-range write failed: n=%d err=%v", n, err)
		}
		if n, err := w.Write([]byte("e")); err == nil || n != 0 {
			t.Errorf("write past the assigned range must be refused, got n=%d err=%v", n, err)
		}
		if w.written != 4 {
			t.Errorf("written = %d, want 4 (the refused bytes must not count)", w.written)
		}
	})
}

// TestRangedMultipart_RejectsOversizedChunkedBodyAfterRequestedBytes pins the
// protocol hole where a 206 carries the exact Content-Range but no
// Content-Length, delivers every requested byte, and then continues with extra
// chunked bytes. The requested prefix is valid, so the former normalization
// treated the range as complete and published the destination; the oversize is
// a bounded-writer protocol violation and must instead fail the range, stay
// not-Done in the record, and leave the staging state resumable.
func TestRangedMultipart_RejectsOversizedChunkedBodyAfterRequestedBytes(t *testing.T) {
	const total = 8_000
	full := rangedTestPayload(total)

	for _, retries := range []int{0, 1} {
		t.Run(fmt.Sprintf("retries=%d", retries), func(t *testing.T) {
			tmpDir := t.TempDir()
			dst := filepath.Join(tmpDir, "chunked-over.bin")
			staging := dst + ".incomplete"
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}

			var gets atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.Header().Set("Content-Length", strconv.Itoa(total))
					w.Header().Set("Accept-Ranges", "bytes")
					return
				}
				gets.Add(1)
				var start, end int64
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start != 0 || end != total-1 {
					http.Error(w, "unexpected range", http.StatusBadRequest)
					return
				}
				// No Content-Length: flushing the headers forces a chunked body.
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
				w.WriteHeader(http.StatusPartialContent)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				// Deliver every requested byte, flushed in small pieces so the
				// bounded writer accepts exactly wantLen bytes...
				for i := 0; i < len(full); i += 512 {
					j := i + 512
					if j > len(full) {
						j = len(full)
					}
					if _, err := w.Write(full[i:j]); err != nil {
						return
					}
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					time.Sleep(200 * time.Microsecond)
				}
				// ...and then send bytes past the assigned range.
				time.Sleep(20 * time.Millisecond)
				_, _ = w.Write(bytes.Repeat([]byte{0xEE}, 4096))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}))
			defer srv.Close()

			it := PlanItem{RelativePath: "chunked-over.bin", URL: srv.URL + "/chunked-over.bin", Size: total, AcceptRanges: true}
			cfg := Settings{Concurrency: 1, Retries: retries, StallTimeout: "0", BackoffInitial: "1ms", BackoffMax: "1ms", Verify: "size"}
			_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {})
			if err == nil {
				t.Fatal("oversized chunked ranged body was accepted and published")
			}
			if !strings.Contains(err.Error(), "exceeds assigned range") {
				t.Errorf("error = %v, want the bounded-writer protocol violation", err)
			}
			if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
				t.Errorf("destination was published after an oversized body, stat err=%v", serr)
			}
			if _, serr := os.Stat(staging); serr != nil {
				t.Errorf("staging payload was not preserved for resume: %v", serr)
			}
			meta, merr := readStagingMeta(staging + ".meta")
			if merr != nil {
				t.Fatalf("staging record was not preserved: %v", merr)
			}
			for _, rp := range meta.Ranges {
				if rp.Done {
					t.Errorf("oversized-body range was persisted as Done: %+v", rp)
				}
			}
			if want := int32(retries + 1); gets.Load() != want {
				t.Errorf("ranged body fetched %d times, want %d (existing retry budget)", gets.Load(), want)
			}
		})
	}
}

// 5. A restart resumes only the missing/uncommitted bytes: no committed
// interval is ever re-requested.
func TestRangedMultipart_RestartResumesOnlyMissingBytes(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 12_000
	full := rangedTestPayload(total)

	dst := filepath.Join(tmpDir, "resume-missing.bin")
	staging := dst + ".incomplete"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	// Simulate an interrupted previous run with a 3-range layout:
	// range 0 fully done, range 1 half committed, range 2 untouched.
	ranges := []RangeProgress{
		{Start: 0, End: 3999, Committed: 4000, Done: true},
		{Start: 4000, End: 7999, Committed: 2000},
		{Start: 8000, End: 11999},
	}
	seedRangedState(t, staging, "o/r", "resume-missing.bin", total, ranges, full)

	srv, gotRanges := rangedTestServer(t, full)

	it := PlanItem{RelativePath: "resume-missing.bin", URL: srv.URL + "/resume-missing.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 3, Retries: 0, StallTimeout: "0"}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	assertExactFile(t, dst, full)

	want := map[string]bool{
		"bytes=6000-7999":  false, // range 1 suffix
		"bytes=8000-11999": false, // range 2 in full
	}
	for _, rh := range *gotRanges {
		if _, ok := want[rh]; ok {
			want[rh] = true
		}
		if strings.HasPrefix(rh, "bytes=0-") || strings.HasPrefix(rh, "bytes=4000-") || strings.HasPrefix(rh, "bytes=2000-") {
			// Any request touching committed prefixes (0-3999, 4000-5999)
			// would mean committed bytes were not honored.
			if !strings.HasPrefix(rh, "bytes=6000-") {
				t.Errorf("committed interval re-requested: %q", rh)
			}
		}
	}
	for rh, seen := range want {
		if !seen {
			t.Errorf("missing interval never requested: %q (got %v)", rh, *gotRanges)
		}
	}
	if len(*gotRanges) != 2 {
		t.Errorf("expected exactly 2 range requests, got %v", *gotRanges)
	}
}

// 6. A preallocated staging file at the full logical size with missing
// ranges must never look complete: the length is not evidence, only the
// committed ranges are.
func TestRangedMultipart_PreallocatedNeverLooksComplete(t *testing.T) {
	t.Run("full-size staging with uncommitted ranges is refetched", func(t *testing.T) {
		tmpDir := t.TempDir()
		const total = 12_000
		full := rangedTestPayload(total)

		dst := filepath.Join(tmpDir, "sparse.bin")
		staging := dst + ".incomplete"
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		// Preallocated to the FULL logical size (as the downloader itself
		// does), with one range committed and the rest missing. The bytes
		// outside the committed interval are garbage.
		ranges := []RangeProgress{
			{Start: 0, End: 3999, Committed: 4000, Done: true},
			{Start: 4000, End: 7999},
			{Start: 8000, End: 11999},
		}
		seedRangedState(t, staging, "o/r", "sparse.bin", total, ranges, full)

		srv, gotRanges := rangedTestServer(t, full)

		it := PlanItem{RelativePath: "sparse.bin", URL: srv.URL + "/sparse.bin", Size: total, AcceptRanges: true}
		cfg := Settings{Concurrency: 3, Retries: 0, StallTimeout: "0"}

		if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}, func(ProgressEvent) {}); err != nil {
			t.Fatalf("downloadMultipart: %v", err)
		}

		// The missing ranges were fetched even though the staging file
		// already reported the full logical size.
		if len(*gotRanges) == 0 {
			t.Fatal("full-size staging was treated as complete; missing ranges were never fetched")
		}
		assertExactFile(t, dst, full)
	})

	t.Run("full-size staging without metadata is discarded, not trusted", func(t *testing.T) {
		tmpDir := t.TempDir()
		const total = 8_000
		full := rangedTestPayload(total)

		dst := filepath.Join(tmpDir, "nometa.bin")
		staging := dst + ".incomplete"
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		// A full-size file with plausible content but NO resume metadata:
		// its bytes are unvalidated and must be refetched, never published
		// as-is.
		if err := os.WriteFile(staging, full, 0o644); err != nil {
			t.Fatal(err)
		}

		srv, gotRanges := rangedTestServer(t, full)

		it := PlanItem{RelativePath: "nometa.bin", URL: srv.URL + "/nometa.bin", Size: total, AcceptRanges: true}
		cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}

		if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
			t.Fatalf("downloadMultipart: %v", err)
		}
		if len(*gotRanges) == 0 {
			t.Fatal("unvalidated full-size staging was trusted; everything must be refetched")
		}
		assertExactFile(t, dst, full)
	})
}

// 7. Changing Concurrency between runs must not invalidate committed bytes:
// the persisted range contract stays authoritative and only missing bytes
// are fetched with whatever worker count is configured.
func TestRangedMultipart_ConcurrencyChangePreservesCommitted(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 16_000
	full := rangedTestPayload(total)

	dst := filepath.Join(tmpDir, "conc.bin")
	staging := dst + ".incomplete"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	// A previous run with Concurrency=4 produced this 4-range layout and
	// committed part of it. The next run uses Concurrency=2.
	ranges := []RangeProgress{
		{Start: 0, End: 3999, Committed: 4000, Done: true},
		{Start: 4000, End: 7999, Committed: 4000, Done: true},
		{Start: 8000, End: 11999, Committed: 1000},
		{Start: 12000, End: 15999},
	}
	seedRangedState(t, staging, "o/r", "conc.bin", total, ranges, full)

	srv, gotRanges := rangedTestServer(t, full)

	it := PlanItem{RelativePath: "conc.bin", URL: srv.URL + "/conc.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	assertExactFile(t, dst, full)

	// The persisted 4-range layout is authoritative: the requests are the
	// uncommitted suffixes of ranges 2 and 3, NOT a fresh 2-range
	// partition of the whole file.
	want := map[string]bool{
		"bytes=9000-11999":  false,
		"bytes=12000-15999": false,
	}
	for _, rh := range *gotRanges {
		if _, ok := want[rh]; ok {
			want[rh] = true
		}
	}
	for rh, seen := range want {
		if !seen {
			t.Errorf("expected request %q not observed; got %v", rh, *gotRanges)
		}
	}
	for _, rh := range *gotRanges {
		if strings.HasPrefix(rh, "bytes=0-") || strings.HasPrefix(rh, "bytes=8000-") {
			t.Errorf("committed bytes invalidated by the concurrency change: %q re-requested", rh)
		}
	}
	if len(*gotRanges) != 2 {
		t.Errorf("expected exactly 2 range requests, got %v", *gotRanges)
	}
}

// fakeHubServer builds a local fake Hub serving one LFS file whose content
// is `content` (or `corrupt` when non-nil). It returns the server and the
// canonical SHA-256 of the content.
func fakeHubServer(t *testing.T, content, corrupt []byte) (*httptest.Server, string, string) {
	t.Helper()
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte(fmt.Sprintf(
				`[{"type":"file","path":"model.bin","size":100,"lfs":{"oid":"%s","size":%d,"sha256":"%s"}}]`,
				sha, len(content), sha)))
		case strings.Contains(r.URL.Path, "/resolve/"):
			body := content
			if corrupt != nil {
				var start, end int64
				_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
				if start > 0 {
					// Corrupt any range after the first while keeping every
					// protocol field valid.
					body = corrupt
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
					w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
					w.WriteHeader(http.StatusPartialContent)
					_, _ = w.Write(body[start : end+1])
					return
				}
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, sha, "commit123"
}

// 8. Verify=size on an LFS file performs NO SHA-256 reread: the download
// publishes the blob via direct ranged writes plus a rename, with zero
// full-file hash passes.
func TestDownload_VerifySizeLFSNoSHAReread(t *testing.T) {
	tmpDir := t.TempDir()
	const size = 64 * 1024 // above the 1KB test threshold → multipart
	full := rangedTestPayload(size)
	srv, sha, _ := fakeHubServer(t, full, nil)

	cacheDir := filepath.Join(tmpDir, "cache")
	cfg := Settings{
		CacheDir:           cacheDir,
		Endpoint:           srv.URL,
		Concurrency:        4,
		MaxActiveDownloads: 2,
		MultipartThreshold: "1KB",
		Verify:             "size",
		Retries:            1,
		StallTimeout:       "0",
	}

	hashPassCount.Store(0)
	hashPassBytes.Store(0)
	if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
		t.Fatalf("Download: %v", err)
	}

	if got := hashPassCount.Load(); got != 0 {
		t.Errorf("Verify=size performed %d full-file hash passes; want 0", got)
	}

	// The blob was published directly from the staging file.
	blob := filepath.Join(cacheDir, "hub", "models--o--r", "blobs", sha)
	assertExactFile(t, blob, full)
	if _, err := os.Stat(filepath.Join(cacheDir, "hub", "models--o--r", "blobs", "tmp-"+sha)); err == nil {
		t.Error("a tmp-<sha> restage file exists; the blob must be published from the staging rename")
	}
	snap := filepath.Join(cacheDir, "hub", "models--o--r", "snapshots", "commit123", "model.bin")
	if _, err := os.Lstat(snap); err != nil {
		t.Errorf("snapshot entry missing: %v", err)
	}
}

// 9. Verify=sha256 detects corruption in a parallel transfer with exactly
// ONE final full-file pass, and never publishes corrupt bytes: a mismatch
// leaves the blob unpublished and removes the untrustworthy staging data
// (explicit redownload policy).
func TestDownload_VerifySHA256DetectsCorruption(t *testing.T) {
	tmpDir := t.TempDir()
	const size = 64 * 1024
	full := rangedTestPayload(size)
	corrupt := rangedTestPayload(size)
	corrupt[20000] ^= 0xFF // wrong bytes inside a non-first range
	srv, sha, _ := fakeHubServer(t, full, corrupt)

	cacheDir := filepath.Join(tmpDir, "cache")
	cfg := Settings{
		CacheDir:           cacheDir,
		Endpoint:           srv.URL,
		Concurrency:        4, // parallel → the one final pass, not hash-on-write
		MaxActiveDownloads: 2,
		MultipartThreshold: "1KB",
		Verify:             "sha256",
		Retries:            0,
		StallTimeout:       "0",
	}

	hashPassCount.Store(0)
	err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {})
	if err == nil {
		t.Fatal("Download succeeded despite corrupted range bytes")
	}
	if !strings.Contains(err.Error(), "sha256 verify failed") {
		t.Errorf("error must be an actionable verification failure, got %q", err.Error())
	}
	if got := hashPassCount.Load(); got != 1 {
		t.Errorf("strict sha256 verification performed %d full-file passes; want exactly 1", got)
	}

	blobsDir := filepath.Join(cacheDir, "hub", "models--o--r", "blobs")
	if _, serr := os.Stat(filepath.Join(blobsDir, sha)); serr == nil {
		t.Error("corrupt bytes were published to the final blob key")
	}
	if _, serr := os.Stat(filepath.Join(blobsDir, sha+".incomplete")); serr == nil {
		t.Error("untrustworthy staging file survived the verification failure")
	}
	if _, serr := os.Stat(filepath.Join(blobsDir, sha+".incomplete.meta")); serr == nil {
		t.Error("untrustworthy staging metadata survived the verification failure")
	}
}

// 10. A fresh Concurrency==1 strict transfer feeds the SHA-256 while the
// bytes are written: zero additional full-file passes. A resumed sequential
// transfer cannot restore the hash state and falls back to exactly one
// final pass.
func TestRangedMultipart_SequentialHashOnWrite(t *testing.T) {
	const total = 16_000
	full := rangedTestPayload(total)
	sum := sha256.Sum256(full)
	sha := hex.EncodeToString(sum[:])

	t.Run("fresh sequential transfer hashes on write", func(t *testing.T) {
		tmpDir := t.TempDir()
		dst := filepath.Join(tmpDir, "seq.bin")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		srv, _ := rangedTestServer(t, full)

		it := PlanItem{RelativePath: "seq.bin", URL: srv.URL + "/seq.bin", Size: total, AcceptRanges: true, LFS: true, SHA256: sha}
		cfg := Settings{Concurrency: 1, Retries: 0, StallTimeout: "0", Verify: "sha256"}

		hashPassCount.Store(0)
		verified, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {})
		if err != nil {
			t.Fatalf("downloadMultipart: %v", err)
		}
		if !verified {
			t.Error("hash-on-write verification was not reported as completed")
		}
		if got := hashPassCount.Load(); got != 0 {
			t.Errorf("fresh sequential strict transfer performed %d full-file passes; want 0 (hash-on-write)", got)
		}
		assertExactFile(t, dst, full)
	})

	t.Run("resumed sequential transfer uses one final pass", func(t *testing.T) {
		tmpDir := t.TempDir()
		dst := filepath.Join(tmpDir, "seq-resume.bin")
		staging := dst + ".incomplete"
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		// Half of the single range is already committed: the hash state for
		// the existing prefix cannot be restored, so the one final pass must
		// run.
		ranges := []RangeProgress{{Start: 0, End: total - 1, Committed: total / 2}}
		seedRangedState(t, staging, "o/r", "seq-resume.bin", total, ranges, full)
		// The item is an LFS blob with a digest, so the record must carry the
		// same digest: a one-sided SHA is no longer resumable (S5).
		seeded, err := readStagingMeta(staging + ".meta")
		if err != nil {
			t.Fatal(err)
		}
		seeded.SHA256 = sha
		if err := writeStagingMeta(staging+".meta", seeded); err != nil {
			t.Fatal(err)
		}

		srv, _ := rangedTestServer(t, full)

		it := PlanItem{RelativePath: "seq-resume.bin", URL: srv.URL + "/seq-resume.bin", Size: total, AcceptRanges: true, LFS: true, SHA256: sha}
		cfg := Settings{Concurrency: 1, Retries: 0, StallTimeout: "0", Verify: "sha256"}

		hashPassCount.Store(0)
		verified, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}, func(ProgressEvent) {})
		if err != nil {
			t.Fatalf("downloadMultipart: %v", err)
		}
		if !verified {
			t.Error("the final verification pass was not reported as completed")
		}
		if got := hashPassCount.Load(); got != 1 {
			t.Errorf("resumed sequential strict transfer performed %d full-file passes; want exactly 1", got)
		}
		assertExactFile(t, dst, full)
	})
}

// 12. Two concurrent claims for the same staging file: only one writer owns
// it. The in-process registry serializes the two downloads; the loser
// observes the winner's completed publish and skips with
// errBlobAlreadyComplete — the server sees every range exactly once.
func TestRangedMultipart_TwoClaimsOneWriter(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 12_000
	full := rangedTestPayload(total)
	sum := sha256.Sum256(full)
	// A content-addressed publish target is the one destination whose
	// existence proves completion, so the loser of the claim can back off as
	// errBlobAlreadyComplete when the winner publishes. An arbitrary
	// destination is deliberately not completion proof (the caller's
	// pre-download skip decision is authoritative there); that behavior is
	// covered by TestDownload_LegacyExistingDestinationIsNotCompletion.
	blob := filepath.Join(tmpDir, "blobs", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := multipartOpts{Staging: blob + ".incomplete", Publish: blob, ContentAddressed: true}

	var mu sync.Mutex
	rangeGets := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(total))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		mu.Lock()
		rangeGets[r.Header.Get("Range")]++
		mu.Unlock()
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		content := full[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusPartialContent)
		// Serve slowly enough that both claimants get to race the claim.
		const chunk = 512
		for i := 0; i < len(content); i += chunk {
			j := i + chunk
			if j > len(content) {
				j = len(content)
			}
			w.Write(content[i:j])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(2 * time.Millisecond)
		}
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "claim.bin", URL: srv.URL + "/claim.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 1, StallTimeout: "0"}

	run := func() (bool, error) {
		return downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, blob, opts, func(ProgressEvent) {})
	}
	done := make(chan struct{})
	results := make([]error, 2)
	go func() { _, results[0] = run(); close(done) }()
	// Give the first claimant a moment to win the claim, then race the
	// second; it must not duplicate any write.
	time.Sleep(20 * time.Millisecond)
	_, results[1] = run()
	<-done

	if results[0] != nil {
		t.Errorf("first claimant failed: %v", results[0])
	}
	if !errors.Is(results[1], errBlobAlreadyComplete) {
		t.Errorf("second claimant error = %v, want errBlobAlreadyComplete (the winner owns the staging)", results[1])
	}

	assertExactFile(t, blob, full)
	mu.Lock()
	defer mu.Unlock()
	for rh, n := range rangeGets {
		if n != 1 {
			t.Errorf("range %q requested %d times; every range must be fetched exactly once (one writer)", rh, n)
		}
	}
}

// Cross-process half of the claim exclusion: a staging record owned by a
// live FOREIGN process that holds the OS ownership lock means "owned" — the
// claimant must not touch the staging.
func TestRangedMultipart_ClaimSeesLiveForeignOwner(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 8_000
	full := rangedTestPayload(total)

	dst := filepath.Join(tmpDir, "foreign.bin")
	staging := dst + ".incomplete"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, full[:1000], 0o644); err != nil {
		t.Fatal(err)
	}
	// A live foreign PID: the parent of this test process is alive for the
	// duration of the test.
	foreign := os.Getppid()
	if foreign == os.Getpid() || !isProcessAlive(foreign) {
		t.Skip("no live foreign PID available for this test process")
	}
	meta := IncompleteMeta{
		PID:       foreign,
		StartedAt: time.Now().UTC(),
		Size:      total,
		Version:   stagingMetaVersion,
		Repo:      "o/r",
		Path:      "foreign.bin",
		Ranges:    buildRangeLayout(total, 2),
	}
	if err := writeStagingMeta(staging+".meta", &meta); err != nil {
		t.Fatal(err)
	}
	// The live owner holds the OS ownership lock for its whole interval.
	hold, _ := acquireStagingOwnershipLock(staging)
	if hold != nil {
		defer hold.release()
	}

	// The size HEAD must succeed for the download to reach the claim; the
	// body requests must never be reached (the claim refuses first).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(total))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		http.Error(w, "body must not be requested while another process owns the staging", http.StatusInternalServerError)
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "foreign.bin", URL: srv.URL + "/foreign.bin", Size: total, AcceptRanges: true}
	_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 2, Retries: 0}, it, dst, multipartOpts{}, func(ProgressEvent) {})
	if !errors.Is(err, errBlobIncompleteOwned) {
		t.Fatalf("error = %v, want errBlobIncompleteOwned for a live foreign owner", err)
	}
	// The foreign owner's staging data was left untouched.
	assertExactFile(t, staging, full[:1000])
}

// The claim take-over path: a record from a dead owner with a valid
// versioned range layout is resumed, not discarded.
func TestRangedMultipart_ClaimTakesOverDeadOwner(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 8_000
	full := rangedTestPayload(total)

	dst := filepath.Join(tmpDir, "dead.bin")
	staging := dst + ".incomplete"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	ranges := []RangeProgress{
		{Start: 0, End: 3999, Committed: 4000, Done: true},
		{Start: 4000, End: 7999},
	}
	seedRangedState(t, staging, "o/r", "dead.bin", total, ranges, full)
	// seedRangedState writes our own PID and process instance token — the
	// take-over case this test wants: a record from an earlier run of this
	// process has no live writer, so the claim takes it over and resumes the
	// committed range.

	srv, gotRanges := rangedTestServer(t, full)
	it := PlanItem{RelativePath: "dead.bin", URL: srv.URL + "/dead.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}
	assertExactFile(t, dst, full)
	// Only the missing range was fetched.
	if len(*gotRanges) != 1 || (*gotRanges)[0] != "bytes=4000-7999" {
		t.Errorf("resume after take-over must fetch only the missing range, got %v", *gotRanges)
	}
}

// A live owner parked in a long Retry-After wait continues to refresh its
// diagnostic claim record. The held lifetime OS lock, not record age, is the
// exclusion fence; the test also preserves prompt cancellation of the wait.
func TestRangedMultipart_LongRetryWaitKeepsClaimFresh(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 8_000

	dst := filepath.Join(tmpDir, "heartbeat.bin")
	staging := dst + ".incomplete"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	// HEAD resolves the size; every range GET is throttled with a Retry-After
	// larger than the diagnostic heartbeat interval, so the owner spends the
	// test inside a wait with no payload writes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(total))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "heartbeat.bin", URL: srv.URL + "/heartbeat.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 1, Retries: 2, StallTimeout: "0"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type runResult struct {
		err error
	}
	done := make(chan runResult, 1)
	go func() {
		_, err := downloadMultipart(ctx, srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {})
		done <- runResult{err}
	}()

	// Observe the claim record as soon as the owner claims the staging.
	metaPath := staging + ".meta"
	metaDeadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(metaPath); err == nil {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("download finished before the claim was observed: %v", r.err)
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(metaDeadline) {
			t.Fatal("claim record was never created")
		}
	}
	metaBefore, err := os.Stat(metaPath)
	if err != nil {
		t.Fatal(err)
	}

	// Wait past the diagnostic heartbeat interval with zero payload writes.
	time.Sleep(claimGracePeriod + 2*time.Second)

	metaAfter, err := os.Stat(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !metaAfter.ModTime().After(metaBefore.ModTime()) {
		t.Errorf("claim record was not rewritten while the live owner waited: mtime still %v", metaAfter.ModTime())
	}
	if !metaFreshWithin(metaPath, claimGracePeriod) {
		t.Error("claim metadata was not refreshed during the retry wait")
	}

	// The heartbeat wait stays cancellation-prompt.
	cancel()
	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("download error = %v, want context.Canceled after cancel", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("download did not stop promptly after cancel")
	}
}

// The claimant's side of the heartbeat contract, both sides of the
// freshness boundary: a live foreign owner whose payload has gone stale
// still owns the staging while its record is fresh (heartbeatted), and
// loses the claim only once BOTH the payload and the record have gone
// stale — a hung process must not lock a blob forever.
func TestRangedMultipart_ClaimHeartbeatBoundary(t *testing.T) {
	foreign := os.Getppid()
	if foreign == os.Getpid() || !isProcessAlive(foreign) {
		t.Skip("no live foreign PID available for this test process")
	}
	const total = 8_000

	// seedForeignOwned stages a payload preallocated to the full size
	// with a valid committed prefix [0, committed) and a versioned record
	// owned by the live foreign PID.
	seedForeignOwned := func(t *testing.T, staging string, full []byte, committed int64) {
		t.Helper()
		if err := os.WriteFile(staging, bytes.Repeat([]byte{0xA5}, len(full)), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := os.OpenFile(staging, os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := out.WriteAt(full[:committed], 0); err != nil {
			t.Fatal(err)
		}
		out.Close()
		meta := IncompleteMeta{
			PID:       foreign,
			StartedAt: time.Now().UTC(),
			Size:      int64(len(full)),
			Version:   stagingMetaVersion,
			Repo:      "o/r",
			Path:      "hb.bin",
			Ranges: []RangeProgress{
				{Start: 0, End: committed - 1, Committed: committed, Done: true},
				{Start: committed, End: int64(len(full)) - 1},
			},
		}
		if err := writeStagingMeta(staging+".meta", &meta); err != nil {
			t.Fatal(err)
		}
	}
	staleMtime := func(t *testing.T, path string) {
		t.Helper()
		past := time.Now().Add(-2 * DefaultStaleTimeout)
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("fresh record with stale payload is not taken over", func(t *testing.T) {
		tmpDir := t.TempDir()
		full := rangedTestPayload(total)
		dst := filepath.Join(tmpDir, "hb-fresh.bin")
		staging := dst + ".incomplete"
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		seedForeignOwned(t, staging, full, 4000)
		staleMtime(t, staging) // payload silent for > stale timeout
		// The record was just rewritten: the owner heartbeated.
		before, err := os.ReadFile(staging)
		if err != nil {
			t.Fatal(err)
		}
		// The live owner holds the OS ownership lock; the fresh record is its
		// diagnostic identity, not the fence.
		hold, _ := acquireStagingOwnershipLock(staging)
		if hold != nil {
			defer hold.release()
		}

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", strconv.Itoa(total))
				w.Header().Set("Accept-Ranges", "bytes")
				return
			}
			http.Error(w, "body must not be requested while a heartbeatted live owner holds the staging", http.StatusInternalServerError)
		}))
		defer srv.Close()

		it := PlanItem{RelativePath: "hb.bin", URL: srv.URL + "/hb.bin", Size: total, AcceptRanges: true}
		_, err = downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 2, Retries: 0}, it, dst, multipartOpts{}, func(ProgressEvent) {})
		if !errors.Is(err, errBlobIncompleteOwned) {
			t.Fatalf("error = %v, want errBlobIncompleteOwned for a live owner with a fresh (heartbeatted) record and a stale payload", err)
		}
		assertExactFile(t, staging, before)
	})

	t.Run("stale unfenced record remains protected", func(t *testing.T) {
		tmpDir := t.TempDir()
		full := rangedTestPayload(total)
		dst := filepath.Join(tmpDir, "hb-stale.bin")
		staging := dst + ".incomplete"
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		seedForeignOwned(t, staging, full, 4000)
		staleMtime(t, staging)
		staleMtime(t, staging+".meta")
		before, err := os.ReadFile(staging)
		if err != nil {
			t.Fatal(err)
		}
		// A free current-version lock cannot prove that a legacy/unfenced
		// predecessor is unable to resume writing.

		srv, gotRanges := rangedTestServer(t, full)
		it := PlanItem{RelativePath: "hb.bin", URL: srv.URL + "/hb.bin", Size: total, AcceptRanges: true}
		cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}

		if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); !errors.Is(err, errBlobIncompleteOwned) {
			t.Fatalf("unfenced predecessor error = %v, want owned/busy", err)
		}
		after, readErr := os.ReadFile(staging)
		if readErr != nil || !bytes.Equal(after, before) {
			t.Errorf("unfenced staging payload changed (read err=%v)", readErr)
		}
		if len(*gotRanges) != 0 {
			t.Errorf("unfenced predecessor triggered network writes: %v", *gotRanges)
		}
	})
}

// parseContentRange unit coverage, including the "/*" unsatisfied-total form
// which matches the range but cannot confirm the total.
func TestParseContentRange(t *testing.T) {
	cases := []struct {
		in       string
		start    int64
		end      int64
		total    int64
		hasTotal bool
		wantErr  bool
	}{
		{"bytes 0-499/1000", 0, 499, 1000, true, false},
		{"bytes 500-999/1000", 500, 999, 1000, true, false},
		{"bytes 0-499/*", 0, 499, 0, false, false},
		{"bytes=0-499/1000", 0, 0, 0, false, true},
		{"bytes 500/1000", 0, 0, 0, false, true},
		{"bytes 500-499/1000", 0, 0, 0, false, true},
		{"bytes 0-499/499", 0, 0, 0, false, true},
		{"bytes 0-499", 0, 0, 0, false, true},
		{"", 0, 0, 0, false, true},
		{"bytes a-b/1000", 0, 0, 0, false, true},
	}
	for _, tc := range cases {
		s, e, tot, hasTot, err := parseContentRange(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseContentRange(%q) = no error; want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseContentRange(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if s != tc.start || e != tc.end || tot != tc.total || hasTot != tc.hasTotal {
			t.Errorf("parseContentRange(%q) = (%d,%d,%d,%v); want (%d,%d,%d,%v)",
				tc.in, s, e, tot, hasTot, tc.start, tc.end, tc.total, tc.hasTotal)
		}
	}
}

// TestRangedMultipart_StrictHashSurvivesOverflowRetry covers M2: an oversized
// chunked body on the first attempt is a protocol violation that does not
// advance the durable offset, so the retry re-requests the same range. The
// sequential hash-on-write state from the first attempt must be invalidated —
// otherwise the retry feeds the same bytes into the hash a second time and no
// valid retry can ever verify. The fallback is exactly one final full-file pass,
// and progress must not count the overflowing attempt's bytes twice.
func TestRangedMultipart_StrictHashSurvivesOverflowRetry(t *testing.T) {
	const total = 8_000
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "strict-overflow.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(total))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		n := gets.Add(1)
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start != 0 || end != total-1 {
			http.Error(w, "unexpected range", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		if n == 1 {
			// First attempt: exact Content-Range, no Content-Length (chunked),
			// every requested byte, then extra bytes past the assigned range.
			w.WriteHeader(http.StatusPartialContent)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			for i := 0; i < len(full); i += 512 {
				j := i + 512
				if j > len(full) {
					j = len(full)
				}
				_, _ = w.Write(full[i:j])
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(200 * time.Microsecond)
			}
			time.Sleep(20 * time.Millisecond)
			_, _ = w.Write(bytes.Repeat([]byte{0xEE}, 1024))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		// Valid retry: the same exact range, served slowly enough that the
		// progress ticker observes the bytes during the attempt.
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusPartialContent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		for i := 0; i < len(full); i += 32 {
			j := i + 32
			if j > len(full) {
				j = len(full)
			}
			_, _ = w.Write(full[i:j])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(3 * time.Millisecond)
		}
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "strict-overflow.bin", URL: srv.URL + "/strict-overflow.bin", Size: total, AcceptRanges: true, LFS: true, SHA256: sha}
	cfg := Settings{Concurrency: 1, Retries: 2, StallTimeout: "0", BackoffInitial: "1ms", BackoffMax: "1ms", Verify: "sha256"}

	var maxProgress atomic.Int64
	hashPassCount.Store(0)
	verified, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ev ProgressEvent) {
		if ev.Event != "file_progress" {
			return
		}
		for {
			cur := maxProgress.Load()
			if ev.Downloaded <= cur || maxProgress.CompareAndSwap(cur, ev.Downloaded) {
				break
			}
		}
	})
	if err != nil {
		t.Fatalf("downloadMultipart: %v (a valid retry after an overflow must verify and publish)", err)
	}
	if !verified {
		t.Error("the final full-file verification pass was not reported as completed")
	}
	if got := hashPassCount.Load(); got != 1 {
		t.Errorf("strict mode after an overflow retry performed %d full-file passes; want exactly 1 (poisoned hash-on-write must be dropped)", got)
	}
	assertExactFile(t, dst, full)
	if got := gets.Load(); got != 2 {
		t.Errorf("ranged body fetched %d times; want 2 (overflow attempt + valid retry)", got)
	}
	if got := maxProgress.Load(); got > total {
		t.Errorf("progress reported %d bytes for a %d-byte file; overflow-attempt bytes were counted twice", got, total)
	}
}
