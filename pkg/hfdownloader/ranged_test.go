// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Local-server tests cover ranged framing, resume, verification, cancellation,
// and concurrent ownership without external network access.
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

// rangedSeedCommit is the immutable identity used by inheritable staging fixtures.
const rangedSeedCommit = "1111111111111111111111111111111111111111"

// seedRangedState writes a pinned current-format record and preallocated payload:
// committed intervals contain full's bytes; all other bytes are garbage.
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

func containsRange(ranges []string, want string) bool {
	for _, r := range ranges {
		if r == want {
			return true
		}
	}
	return false
}

func TestRangedMultipart_OutOfOrderRangesProduceExactBytes(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 20_000
	const nRanges = 4
	full := rangedTestPayload(total)

	dst := filepath.Join(tmpDir, "out-of-order.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	// Hold range 0 until the server has served every other range.
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

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ProgressEvent) {}); err != nil {
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

	// Every other range was served before range 0 was released.
	if got := servedOthers.Load(); got != nRanges-1 {
		t.Errorf("servedOthers = %d, want %d (range 0 must be released last)", got, nRanges-1)
	}
}

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

			_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ProgressEvent) {})
			if err == nil {
				t.Fatal("download succeeded despite a bad Content-Range; the body must be rejected")
			}
			if !strings.Contains(err.Error(), "Content-Range") && !strings.Contains(err.Error(), "Content-Length") {
				t.Errorf("error must identify the protocol violation, got %q", err.Error())
			}

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
			w.Write(content[:len(content)/2])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}))
		defer srv.Close()

		it := PlanItem{RelativePath: "short.bin", URL: srv.URL + "/short.bin", Size: total, AcceptRanges: true}
		cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}
		_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ProgressEvent) {})
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
		_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ProgressEvent) {})
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

// An overlong chunked 206 must not mark the range Done or publish, even when
// all requested bytes arrived before the excess.
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
				// Flush all requested bytes before sending the oversized suffix.
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
				time.Sleep(20 * time.Millisecond)
				_, _ = w.Write(bytes.Repeat([]byte{0xEE}, 4096))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}))
			defer srv.Close()

			it := PlanItem{RelativePath: "chunked-over.bin", URL: srv.URL + "/chunked-over.bin", Size: total, AcceptRanges: true}
			cfg := Settings{Concurrency: 1, Retries: retries, StallTimeout: "0", BackoffInitial: "1ms", BackoffMax: "1ms", Verify: "size"}
			_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ProgressEvent) {})
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

func TestRangedMultipart_RestartResumesOnlyMissingBytes(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 12_000
	full := rangedTestPayload(total)

	dst := filepath.Join(tmpDir, "resume-missing.bin")
	staging := dst + ".incomplete"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	ranges := []RangeProgress{
		{Start: 0, End: 3999, Committed: 4000, Done: true},
		{Start: 4000, End: 7999, Committed: 2000},
		{Start: 8000, End: 11999},
	}
	seedRangedState(t, staging, "o/r", "resume-missing.bin", total, ranges, full)

	srv, gotRanges := rangedTestServer(t, full)

	it := PlanItem{RelativePath: "resume-missing.bin", URL: srv.URL + "/resume-missing.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 3, Retries: 0, StallTimeout: "0"}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}), func(ProgressEvent) {}); err != nil {
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

// Preallocated file length is not evidence that all ranges are complete.
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

		if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}), func(ProgressEvent) {}); err != nil {
			t.Fatalf("downloadMultipart: %v", err)
		}

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

		if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ProgressEvent) {}); err != nil {
			t.Fatalf("downloadMultipart: %v", err)
		}
		if len(*gotRanges) == 0 {
			t.Fatal("unvalidated full-size staging was trusted; everything must be refetched")
		}
		assertExactFile(t, dst, full)
	})
}

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

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}), func(ProgressEvent) {}); err != nil {
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

// fakeHubServer serves one LFS file, substituting corrupt for ranges after the
// first when non-nil. It returns the server, content digest and resolved commit.
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

// Verify=size on LFS performs no normal full-file hash reread.
func TestDownload_VerifySizeLFSNoSHAReread(t *testing.T) {
	tmpDir := t.TempDir()
	const size = 64 * 1024
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

// Strict verification rejects corrupted parallel-transfer content with one
// full-file pass.
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

// Fresh sequential strict transfers hash on write; resumed ones use one final pass.
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
		verified, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ProgressEvent) {})
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
		// Match the record's digest identity to the plan.
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
		verified, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}), func(ProgressEvent) {})
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

func TestRangedMultipart_TwoClaimsOneWriter(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 12_000
	full := rangedTestPayload(total)
	sum := sha256.Sum256(full)
	// Only the content-addressed target is completion proof for the loser.
	blob := filepath.Join(tmpDir, "blobs", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		t.Fatal(err)
	}
	key := blob
	lockSrc := filepath.Join(filepath.Dir(blob), "tmp-"+filepath.Base(blob))

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
		// Slow the transfer to give the second acquisition a chance to contend.
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
		domain, derr := acquireBlobDomain(context.Background(), key, lockSrc)
		if derr != nil {
			return false, derr
		}
		defer domain.release()
		opts := multipartOpts{Staging: blob + ".incomplete", Publish: blob, ContentAddressed: true, Domain: domain}
		return downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, blob, opts, func(ProgressEvent) {})
	}
	done := make(chan struct{})
	results := make([]error, 2)
	go func() { _, results[0] = run(); close(done) }()
	// Stagger the second acquisition; it must not duplicate range writes.
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

// An unfenced foreign-owner record is refused without modifying its staging payload.
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
	// Use an available live foreign PID for the unfenced record.
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
	// An unfenced record of a live owner remains unproven and is refused.

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
	_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 2, Retries: 0}, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ProgressEvent) {})
	if !errors.Is(err, errBlobIncompleteOwned) {
		t.Fatalf("error = %v, want errBlobIncompleteOwned for a live foreign owner", err)
	}
	assertExactFile(t, staging, full[:1000])
}

// Foreign unfenced records remain protected without same-host death proof,
// regardless of age; a free new domain lock does not prove their owner gone.
func TestRangedMultipart_UnfencedRecordRefusal(t *testing.T) {
	foreign := os.Getppid()
	if foreign == os.Getpid() || !isProcessAlive(foreign) {
		t.Skip("no live foreign PID available for this test process")
	}
	const total = 8_000

	// Seed a full-sized payload with a valid committed prefix and an unfenced foreign-PID record.
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
		past := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("live foreign owner is not taken over", func(t *testing.T) {
		tmpDir := t.TempDir()
		full := rangedTestPayload(total)
		dst := filepath.Join(tmpDir, "hb-fresh.bin")
		staging := dst + ".incomplete"
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		seedForeignOwned(t, staging, full, 4000)
		staleMtime(t, staging) // Old mtime must not authorize takeover.
		before, err := os.ReadFile(staging)
		if err != nil {
			t.Fatal(err)
		}
		// A foreign unfenced record without owner-death proof is preserved despite
		// its age and a free domain lock.

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", strconv.Itoa(total))
				w.Header().Set("Accept-Ranges", "bytes")
				return
			}
			http.Error(w, "body must not be requested while a live foreign owner holds the staging", http.StatusInternalServerError)
		}))
		defer srv.Close()

		it := PlanItem{RelativePath: "hb.bin", URL: srv.URL + "/hb.bin", Size: total, AcceptRanges: true}
		_, err = downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 2, Retries: 0}, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ProgressEvent) {})
		if !errors.Is(err, errBlobIncompleteOwned) {
			t.Fatalf("error = %v, want errBlobIncompleteOwned for a live foreign owner with an unfenced record and a stale payload", err)
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

		if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ProgressEvent) {}); !errors.Is(err, errBlobIncompleteOwned) {
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

// parseContentRange covers unknown totals (bytes start-end/*), which cannot confirm the expected size.
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

// An overflowing response must not commit its range. A valid retry of the same
// bytes invalidates hash-on-write and uses one final proof without double-counting progress.
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
		// Serve the valid retry slowly to give the progress ticker time to sample it.
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
	verified, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ev ProgressEvent) {
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
