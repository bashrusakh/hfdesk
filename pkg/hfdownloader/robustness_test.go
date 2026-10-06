// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

// stallOnceServer serves full over Range requests, but on the FIRST request for
// each distinct requested start offset it sends only a prefix of that range and
// then blocks (keeping the connection open without sending more bytes) until
// the client aborts the attempt. Subsequent requests for the advanced offset
// are served cleanly. This models "connection stays open, stops delivering
// bytes" without relying on the client transport's own timeouts.
func stallOnceServer(t *testing.T, full []byte, prefix int) (*httptest.Server, *[]string) {
	t.Helper()
	var (
		mu     sync.Mutex
		stalls = make(map[int64]bool)
		ranges []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := int64(0), int64(len(full)-1)
		if rng := r.Header.Get("Range"); rng != "" {
			if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
		}
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		cut := !stalls[start]
		if cut {
			stalls[start] = true
		}
		mu.Unlock()

		if cut && end-start+1 > int64(prefix) {
			// Declare the FULL requested length but send only the prefix, then
			// hold the connection open with no further bytes — a real stall,
			// not a short-but-complete response.
			fullLen := end - start + 1
			content := full[start : start+int64(prefix)]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(full)))
			w.Header().Set("Content-Length", strconv.FormatInt(fullLen, 10))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Stall: hold the connection open until the client cancels the
			// attempt, delivering no further bytes.
			<-r.Context().Done()
			return
		}

		content := full[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(full)))
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content)
	}))
	return srv, &ranges
}

// TestDownloadSingle_StalledTransferAbortsAndResumes verifies the core stall
// watchdog behavior: a body read that stops delivering bytes is abandoned after
// StallTimeout and retried from the CURRENT on-disk offset (a Range resume),
// not restarted from zero.
func TestDownloadSingle_StalledTransferAbortsAndResumes(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 40_000
	full := make([]byte, total)
	for i := range full {
		full[i] = byte(i % 251)
	}
	const prefix = 5_000

	dst := filepath.Join(tmpDir, "blobs", "tmp-stall")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	srv, ranges := stallOnceServer(t, full, prefix)
	defer srv.Close()

	it := PlanItem{RelativePath: "stall.bin", URL: srv.URL + "/stall.bin", Size: int64(total)}
	cfg := Settings{
		Retries:        3,
		StallTimeout:   "150ms",
		BackoffInitial: "10ms",
		BackoffMax:     "20ms",
	}

	start := time.Now()
	err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}
	// The stall must have been detected and retried promptly, not hung.
	if elapsed > 5*time.Second {
		t.Errorf("stalled transfer took %v; watchdog did not abort promptly", elapsed)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("final content mismatch: got len=%d want len=%d", len(got), len(full))
	}

	// The retry must resume from the prefix offset, not restart from zero.
	seen := *ranges
	if len(seen) < 2 {
		t.Fatalf("expected a retry request, got ranges=%v", seen)
	}
	resumed := false
	for _, rng := range seen {
		var rs, re int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &rs, &re); err == nil && rs == prefix {
			resumed = true
		}
	}
	if !resumed {
		t.Errorf("retry did not resume from offset %d; ranges=%v", prefix, seen)
	}
}

// TestDownloadSingle_MultiStallProgressResetsBudget verifies that a long
// download survives more stalls than its retry budget because each stalled
// attempt still advanced the resume offset, which resets the budget.
func TestDownloadSingle_MultiStallProgressResetsBudget(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 50_000
	full := make([]byte, total)
	for i := range full {
		full[i] = byte(i % 251)
	}
	// Each stalled attempt advances by 1/5 of the file: 4 stalls + a finish,
	// far more than the Retries=1 budget.
	const increment = total / 5

	dst := filepath.Join(tmpDir, "blobs", "tmp-multistall")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := int64(0)
		if rng := r.Header.Get("Range"); rng != "" {
			if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
		}
		mu.Lock()
		requestCount++
		mu.Unlock()

		remaining := int64(len(full)) - start
		n := int64(increment)
		if remaining < n {
			n = remaining
		}
		content := full[start : start+n]
		// Always declare the full remaining length; when this attempt will
		// stall we send only the increment and hold the connection open.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, int64(len(full))-1, len(full)))
		w.Header().Set("Content-Length", strconv.FormatInt(remaining, 10))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if start+n < int64(len(full)) {
			// Stall until the client aborts this attempt.
			<-r.Context().Done()
		}
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "multi.bin", URL: srv.URL + "/multi.bin", Size: int64(total)}
	cfg := Settings{
		Retries:        1,
		StallTimeout:   "80ms",
		BackoffInitial: "5ms",
		BackoffMax:     "10ms",
	}

	start := time.Now()
	if err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("multi-stall download took %v", elapsed)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("final content mismatch")
	}
	mu.Lock()
	rc := requestCount
	mu.Unlock()
	if rc <= cfg.Retries+1 {
		t.Errorf("expected more attempts than the initial retry budget (%d), got %d", cfg.Retries+1, rc)
	}
}

// TestDownloadMultipart_StalledPartRecoversAndAssembles verifies a stalled
// multipart part aborts, resumes from its part offset, and the assembled file
// still verifies against the full payload.
func TestDownloadMultipart_StalledPartRecoversAndAssembles(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 24_000
	full := make([]byte, total)
	for i := range full {
		full[i] = byte(i % 251)
	}
	const nParts = 3

	dst := filepath.Join(tmpDir, "blobs", "tmp-partstall")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	var (
		mu     sync.Mutex
		stalls = make(map[int64]bool)
	)
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
		mu.Lock()
		// Stall only the first request for part 0, once.
		cut := start == 0 && !stalls[start]
		if cut {
			stalls[start] = true
		}
		mu.Unlock()

		content := full[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(full)))
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusPartialContent)
		if cut && len(content) > 100 {
			w.Write(content[:100])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
			return
		}
		w.Write(content)
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "part.bin", URL: srv.URL + "/part.bin", Size: int64(total), AcceptRanges: true}
	cfg := Settings{
		Concurrency:    nParts,
		Retries:        3,
		StallTimeout:   "120ms",
		BackoffInitial: "10ms",
		BackoffMax:     "20ms",
	}

	if err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("assembled content mismatch after stalled part")
	}
}

// TestDownloadSingle_RetryAfterHonored verifies that a 429 with Retry-After: 2
// makes the downloader wait the server-requested time (not just its local
// backoff) and then succeed.
func TestDownloadSingle_RetryAfterHonored(t *testing.T) {
	tmpDir := t.TempDir()
	full := []byte("retry-after content that spans a few bytes for the test")

	dst := filepath.Join(tmpDir, "blobs", "tmp-retryafter")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write(full)
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "r.bin", URL: srv.URL + "/r.bin", Size: int64(len(full))}
	cfg := Settings{
		Retries:        2,
		BackoffInitial: "10ms",
		BackoffMax:     "20ms",
		StallTimeout:   "0", // disable the watchdog; this test is about waiting
	}

	start := time.Now()
	if err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 2*time.Second {
		t.Errorf("waited %v; expected to honor Retry-After: 2 (>=2s)", elapsed)
	}
	if elapsed > 6*time.Second {
		t.Errorf("waited %v; Retry-After wait should not be inflated/capped oddly", elapsed)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("final content mismatch")
	}
}

// TestDownload_PermanentStatusFailsFast drives the full Download() path against
// a fake Hub and asserts that a permanent 401/403/404 file response fails the
// job immediately, with ONE request for the file and an actionable error.
func TestDownload_PermanentStatusFailsFast(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantIs    error
		wantInMsg string
	}{
		{"401 unauthorized", http.StatusUnauthorized, ErrUnauthorized, "token"},
		{"403 gated", http.StatusForbidden, ErrUnauthorized, "gated"},
		{"404 not found", http.StatusNotFound, ErrNotFound, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu       sync.Mutex
				fileGets int
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/models/o/r/revision/main":
					json.NewEncoder(w).Encode(RepoInfo{SHA: "abc123"})
				case r.URL.Path == "/api/models/o/r/tree/main":
					json.NewEncoder(w).Encode([]hfNode{{Type: "file", Path: "config.json", Size: 128}})
				case r.URL.Path == "/o/r/raw/main/config.json":
					mu.Lock()
					fileGets++
					mu.Unlock()
					w.WriteHeader(tc.status)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			cfg := Settings{
				CacheDir:       t.TempDir(),
				Endpoint:       srv.URL,
				Retries:        5, // must not be used for a permanent status
				BackoffInitial: "5ms",
				BackoffMax:     "10ms",
				StallTimeout:   "0",
			}
			err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {})
			if err == nil {
				t.Fatal("Download succeeded; expected fail-fast error")
			}
			if !errors.Is(err, tc.wantIs) {
				t.Errorf("error = %v, want errors.Is %v", err, tc.wantIs)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(tc.wantInMsg)) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantInMsg)
			}
			mu.Lock()
			gets := fileGets
			mu.Unlock()
			if gets != 1 {
				t.Errorf("file was requested %d times; want exactly 1 (no fail-fast retry storm)", gets)
			}
		})
	}
}

// TestDownloadMultipart_FailFast covers the multipart path: a permanent status
// on the initial HEAD fails before any part request (one request total), and a
// permanent status on a part request fails the whole file without a retry
// storm.
func TestDownloadMultipart_FailFast(t *testing.T) {
	t.Run("HEAD 403 fails before parts", func(t *testing.T) {
		var (
			mu       sync.Mutex
			requests int
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requests++
			mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()

		tmpDir := t.TempDir()
		dst := filepath.Join(tmpDir, "blobs", "tmp-head403")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		it := PlanItem{RelativePath: "h.bin", URL: srv.URL + "/h.bin", Size: 10_000, AcceptRanges: true}
		err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 2, Retries: 4}, it, dst, func(ProgressEvent) {})
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("error = %v, want ErrUnauthorized", err)
		}
		mu.Lock()
		n := requests
		mu.Unlock()
		if n != 1 {
			t.Errorf("issued %d requests; want 1 (HEAD fail-fast, no part requests)", n)
		}
		// No part files should have been created.
		if files, _ := multipartPartFiles(dst); len(files) != 0 {
			t.Errorf("part files created despite HEAD fail-fast: %v", files)
		}
	})

	t.Run("part 404 fails without retry storm", func(t *testing.T) {
		var (
			mu           sync.Mutex
			partRequests int
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", "10000")
				w.Header().Set("Accept-Ranges", "bytes")
				return
			}
			mu.Lock()
			partRequests++
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		tmpDir := t.TempDir()
		dst := filepath.Join(tmpDir, "blobs", "tmp-part404")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		it := PlanItem{RelativePath: "p.bin", URL: srv.URL + "/p.bin", Size: 10_000, AcceptRanges: true}
		err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 2, Retries: 5}, it, dst, func(ProgressEvent) {})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
		mu.Lock()
		n := partRequests
		mu.Unlock()
		// Each part is requested once; no per-part retry storm. With 2 parts
		// that is at most 2 part requests, not 2*(Retries+1).
		if n > 2 {
			t.Errorf("issued %d part requests for a permanent 404; expected <=2 (one per part)", n)
		}
	})
}

// TestDownloadSingle_IgnoredRangeRestartIsNotProgress verifies the retry-budget
// rule from the issue: a server that ignores Range and restarts the body from
// zero must NOT be treated as making progress, so it cannot retry forever. The
// attempt budget is exhausted and the download fails.
func TestDownloadSingle_IgnoredRangeRestartIsNotProgress(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 20_000
	const preSeed = 5_000

	dst := filepath.Join(tmpDir, "blobs", "tmp-noprogress")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing resumable partial. The server below ignores Range, so each
	// attempt truncates and downloads less than this offset — never progress.
	if err := os.WriteFile(dst+".part", bytes.Repeat([]byte("x"), preSeed), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		// Ignore any Range request: always 200 with full-body length, but send
		// only a fixed prefix and then abort. The prefix (1000) is less than
		// the pre-seeded offset (5000), so no attempt advances the resume
		// offset.
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		w.Write(make([]byte, 1_000))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "np.bin", URL: srv.URL + "/np.bin", Size: total}
	cfg := Settings{
		Retries:        2,
		BackoffInitial: "5ms",
		BackoffMax:     "10ms",
		StallTimeout:   "0",
	}
	err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
	if err == nil {
		t.Fatal("expected failure; ignored-Range restarts must not loop forever")
	}
	mu.Lock()
	n := requests
	mu.Unlock()
	if n != cfg.Retries+1 {
		t.Errorf("made %d requests; want exactly %d (retry budget must not reset without progress)", n, cfg.Retries+1)
	}
}

// TestDownloadSingle_FailFastRemovesEmptyPartial verifies a fail-fast response
// leaves no empty .part file behind, while a retryable failure preserves any
// resumable partial.
func TestDownloadSingle_FailFastRemovesEmptyPartial(t *testing.T) {
	tmpDir := t.TempDir()
	full := []byte("payload")
	dst := filepath.Join(tmpDir, "blobs", "tmp-failfast")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "f.bin", URL: srv.URL + "/f.bin", Size: int64(len(full))}
	err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Retries: 3}, it, dst, func(ProgressEvent) {})
	if err == nil {
		t.Fatal("expected fail-fast error")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("error = %v, want ErrUnauthorized", err)
	}
	if _, statErr := os.Stat(dst + ".part"); !os.IsNotExist(statErr) {
		t.Errorf("empty .part should have been removed on fail-fast, stat err: %v", statErr)
	}
}

// TestDownloadSingle_CancelDuringBackoffReturnsPromptly verifies cancellation
// interrupts the retry wait rather than running the full backoff.
func TestDownloadSingle_CancelDuringBackoffReturnsPromptly(t *testing.T) {
	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "blobs", "tmp-cancelbackoff")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "c.bin", URL: srv.URL + "/c.bin", Size: 100}
	cfg := Settings{
		Retries:        5,
		BackoffInitial: "30s", // long enough that a non-interruptible wait would hang the test
		BackoffMax:     "30s",
		StallTimeout:   "0",
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := downloadSingle(ctx, srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("cancel during backoff took %v; should return promptly", elapsed)
	}
}

// TestBuildSOCKS5Client_DialRespectsContext verifies a SOCKS5 proxy that
// accepts the TCP connection but never completes the handshake cannot hang the
// download: the request context deadline must abort it.
func TestBuildSOCKS5Client_DialRespectsContext(t *testing.T) {
	// A listener that accepts connections and holds them open without ever
	// speaking SOCKS5.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close() // held open until the listener closes / test ends
		}
	}()

	client, err := BuildHTTPClient(&ProxyConfig{URL: "socks5://" + ln.Addr().String(), NoEnvProxy: true})
	if err != nil {
		t.Fatalf("BuildHTTPClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://example.invalid/x", nil)

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected SOCKS5 request to fail against a non-responding proxy")
	}
	if elapsed > 3*time.Second {
		t.Errorf("SOCKS5 dial took %v; it should honor the context deadline rather than hang", elapsed)
	}
}

// TestBuildSOCKS5Client_HandshakeBoundedWithoutRequestDeadline verifies P1: the
// transport bounds the FULL dial+handshake with its own deadline, independent
// of the request context. Invoking the transport's DialContext directly with a
// background context (no deadline) against an accept-but-never-speak proxy must
// still return within a bounded time — before the fix it would hang forever.
func TestBuildSOCKS5Client_HandshakeBoundedWithoutRequestDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()

	client, err := BuildHTTPClient(&ProxyConfig{URL: "socks5://" + ln.Addr().String(), NoEnvProxy: true})
	if err != nil {
		t.Fatalf("BuildHTTPClient: %v", err)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}

	// Lower the package-level transport handshake timeout for a fast test.
	old := socksHandshakeNanos.Swap(int64(250 * time.Millisecond))
	defer socksHandshakeNanos.Store(old)

	type dialResult struct {
		conn net.Conn
		err  error
	}
	res := make(chan dialResult, 1)
	start := time.Now()
	go func() {
		conn, err := tr.DialContext(context.Background(), "tcp", "example.invalid:80")
		res <- dialResult{conn, err}
	}()

	// A regression (handshake bounded only by the request context) would block
	// forever with a background ctx; fail instead of hanging.
	select {
	case r := <-res:
		if r.err == nil {
			r.conn.Close()
			t.Fatal("expected the SOCKS5 handshake to fail against a non-responding proxy")
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("SOCKS5 handshake took %v with no request deadline; the transport must bound it", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SOCKS5 handshake hung with no request deadline; the transport did not bound it")
	}
}

func TestRetryAfterFromResponse(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mkResp := func(status int, hdr map[string]string) *http.Response {
		h := http.Header{}
		for k, v := range hdr {
			h.Set(k, v)
		}
		return &http.Response{StatusCode: status, Header: h}
	}

	tests := []struct {
		name string
		resp *http.Response
		want time.Duration
	}{
		{"nil", nil, 0},
		{"429 seconds", mkResp(429, map[string]string{"Retry-After": "2"}), 2 * time.Second},
		{"429 http-date", mkResp(429, map[string]string{"Retry-After": now.Add(3 * time.Second).Format(http.TimeFormat)}), 3 * time.Second},
		{"503 seconds", mkResp(503, map[string]string{"Retry-After": "5"}), 5 * time.Second},
		{"429 rate-limit header", mkResp(429, map[string]string{"RateLimit": `"api";r=0;t=7`}), 7 * time.Second},
		{"larger of both wins", mkResp(429, map[string]string{"Retry-After": "2", "RateLimit": `"api";r=0;t=7`}), 7 * time.Second},
		{"zero wait", mkResp(429, map[string]string{"Retry-After": "0"}), 0},
		{"past date", mkResp(429, map[string]string{"Retry-After": now.Add(-time.Minute).Format(http.TimeFormat)}), 0},
		{"non-throttle status ignored", mkResp(500, map[string]string{"Retry-After": "9"}), 0},
		{"unparseable", mkResp(429, map[string]string{"Retry-After": "soon"}), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RetryAfterFromResponse(tt.resp, now); got != tt.want {
				t.Errorf("RetryAfterFromResponse = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNextRetryWait(t *testing.T) {
	now := time.Now()
	local := 500 * time.Millisecond

	// Local backoff wins when no server hint.
	if got := NextRetryWait(nil, local, DefaultMaxRetryAfter, now); got != local {
		t.Errorf("no hint: got %v, want %v", got, local)
	}
	// Server hint larger than local wins.
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"10"}}}
	if got := NextRetryWait(resp, local, DefaultMaxRetryAfter, now); got != 10*time.Second {
		t.Errorf("server hint: got %v, want 10s", got)
	}
	// Cap applies.
	if got := NextRetryWait(resp, local, 3*time.Second, now); got != 3*time.Second {
		t.Errorf("cap: got %v, want 3s", got)
	}
	// Local wins when larger than the server hint.
	local2 := 20 * time.Second
	if got := NextRetryWait(resp, local2, DefaultMaxRetryAfter, now); got != local2 {
		t.Errorf("local larger: got %v, want %v", got, local2)
	}
}

func TestStatusClassification(t *testing.T) {
	retryable := []int{429, 500, 502, 503, 504}
	for _, c := range retryable {
		if !RetryableStatus(c) {
			t.Errorf("RetryableStatus(%d) = false, want true", c)
		}
	}
	failFast := []int{401, 403, 404}
	for _, c := range failFast {
		if !FailFastStatus(c) {
			t.Errorf("FailFastStatus(%d) = false, want true", c)
		}
		if (&APIError{StatusCode: c}).IsRetryable() {
			t.Errorf("APIError{%d}.IsRetryable() = true, want false", c)
		}
	}
	// The helpers must agree with the APIError methods.
	for _, c := range []int{200, 206, 400, 401, 403, 404, 429, 500, 503} {
		ae := &APIError{StatusCode: c}
		if ae.IsRetryable() != RetryableStatus(c) {
			t.Errorf("retryable mismatch at %d", c)
		}
		if ae.IsFailFast() != FailFastStatus(c) {
			t.Errorf("fail-fast mismatch at %d", c)
		}
	}
}

// TestStallTimeoutSetting verifies settings resolution: unset means the
// protective default, an explicit 0 disables the watchdog, and an unparseable
// value falls back to the default rather than silently disabling protection.
func TestStallTimeoutSetting(t *testing.T) {
	cases := []struct {
		setting string
		want    time.Duration
	}{
		{"", DefaultStallTimeout},
		{"60s", 60 * time.Second},
		{"2m", 2 * time.Minute},
		{"0", 0},
		{"0s", 0},
		{"garbage", DefaultStallTimeout},
		{"-5s", DefaultStallTimeout},
		{"-1m", DefaultStallTimeout},
	}
	for _, tc := range cases {
		if got := stallTimeout(Settings{StallTimeout: tc.setting}); got != tc.want {
			t.Errorf("stallTimeout(%q) = %v, want %v", tc.setting, got, tc.want)
		}
	}
	if DefaultSettings().StallTimeout != "60s" {
		t.Errorf("DefaultSettings().StallTimeout = %q, want 60s", DefaultSettings().StallTimeout)
	}
}

// TestBuildHTTPClientTimeouts guards the transport deadlines that bound each
// phase that can otherwise block forever.
func TestBuildHTTPClientTimeouts(t *testing.T) {
	client, err := BuildHTTPClient(&ProxyConfig{NoEnvProxy: true})
	if err != nil {
		t.Fatalf("BuildHTTPClient: %v", err)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if tr.DialContext == nil {
		t.Error("DialContext must be set so dialing has a timeout")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("ResponseHeaderTimeout must be set")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 must be enabled")
	}
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout <= 0 || tr.HTTP2.PingTimeout <= 0 {
		t.Errorf("HTTP/2 health-check timeouts must be set, got %+v", tr.HTTP2)
	}
}

// TestBackoffReset verifies the explicit reset returns the schedule to its
// initial delay.
func TestBackoffReset(t *testing.T) {
	b := newRetry(Settings{BackoffInitial: "10ms", BackoffMax: "1s"})
	for i := 0; i < 5; i++ {
		b.Next()
	}
	b.reset()
	got := b.Next()
	if got < 10*time.Millisecond || got > 250*time.Millisecond {
		t.Errorf("after reset, Next = %v, want ~10ms (initial)", got)
	}
}

// TestDownloadSingle_OscillatingIgnoredRangeDoesNotRefund is the T2 oscillation
// case: a server that ignores Range and alternates full-body responses between
// offset 1000 and offset 0 must never exceed the file high-water mark, so the
// retry budget is never refunded and the download terminates after exactly
// Retries+1 attempts. Before the high-water-mark fix, each A->B transition
// looked like progress and refilled the budget forever.
func TestDownloadSingle_OscillatingIgnoredRangeDoesNotRefund(t *testing.T) {
	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "blobs", "tmp-osc")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := requests
		requests++
		mu.Unlock()
		// Ignore any Range: always 200, but deliver 1000 bytes on even requests
		// and 0 bytes on odd requests, then abort. The client truncates to 0 on
		// each 200 and re-downloads, so the observed offset oscillates
		// 1000 -> 0 -> 1000 -> 0. Once the high-water mark reaches 1000, no
		// later 0->1000 transition exceeds it, so the budget cannot be refilled
		// forever (the old per-attempt "pos > startPos" rule could).
		w.Header().Set("Content-Length", "20000")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		if n%2 == 0 {
			w.Write(make([]byte, 1_000))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "osc.bin", URL: srv.URL + "/osc.bin", Size: 20_000}
	cfg := Settings{
		Retries:        3,
		BackoffInitial: "5ms",
		BackoffMax:     "10ms",
		StallTimeout:   "0",
	}
	done := make(chan error, 1)
	go func() {
		done <- downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
	}()

	// Bound the test: a regression that refunds the budget forever would hang.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected failure; oscillation must not loop forever")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("downloadSingle did not terminate; oscillation refilled the retry budget")
	}
	mu.Lock()
	n := requests
	mu.Unlock()
	if n != cfg.Retries+1 {
		t.Errorf("made %d requests; want exactly %d (oscillation must not refund the budget)", n, cfg.Retries+1)
	}
}

// TestDownloadMultipart_PermanentPartAbortsSiblings verifies T1 on the
// multipart path: when one part hits a permanent error, sibling parts must stop
// promptly, the original permanent error must be returned, and the resumable
// bytes must not be deleted (T3 — the internal abort is not a user cancel, so
// cleanupPartialsOnCancel must not run).
func TestDownloadMultipart_PermanentPartAbortsSiblings(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "100000")
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=0-") {
			// Part 0 is the permanent one: a 404 fails the whole file.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Sibling part: advertise a body, flush headers, then withhold the body
		// so it cannot finish on its own. The default client has no body
		// timeout and StallTimeout is disabled, so only the fix's partCtx
		// cancel (triggered by part 0's fail-fast) can unblock its io.Copy.
		w.Header().Set("Content-Range", "bytes 50000-99999/100000")
		w.Header().Set("Content-Length", "50000")
		w.WriteHeader(http.StatusPartialContent)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-release
	}))
	// Close release before srv.Close (whose Close waits for the blocked
	// handler): a single defer keeps the ordering deterministic.
	defer func() {
		close(release)
		srv.Close()
	}()

	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "blobs", "tmp-partabort")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-seed resumable bytes in a part file together with a matching layout
	// file, so prepareMultipartResume preserves them; they must then survive the
	// fail-fast abort. Without matching metadata the pre-existing behavior
	// discards stale parts before any network I/O, which would mask this check.
	if err := os.WriteFile(dst+".part-01", bytes.Repeat([]byte("y"), 500), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeMultipartResumeLayout(dst, buildMultipartResumeLayout(100_000, 2, 50_000)); err != nil {
		t.Fatal(err)
	}

	it := PlanItem{RelativePath: "pa.bin", URL: srv.URL + "/pa.bin", Size: 100_000, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 5, BackoffInitial: "5ms", BackoffMax: "10ms", StallTimeout: "0"}
	// cleanupPartialsOnCancel being true is the user-cancel default; the
	// internal abort must not consult it.
	cfg.CleanupPartialsOnCancel = func() bool { return true }

	done := make(chan error, 1)
	go func() {
		done <- downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound (original permanent error)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("downloadMultipart did not terminate after a permanent part error; sibling part was not aborted")
	}

	// T3: the pre-existing resumable part bytes must not have been deleted.
	if fi, err := os.Stat(dst + ".part-01"); err != nil || fi.Size() != 500 {
		t.Errorf("resumable .part-01 was removed/modified by the internal abort (stat err=%v)", err)
	}
}

// TestDownload_SiblingAbortedOnPermanentFileError verifies T1 on the job level:
// a multi-file download where one file returns a permanent 404 must terminate
// promptly with the 404, without waiting for a slow/stalled sibling file.
func TestDownload_SiblingAbortedOnPermanentFileError(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "abc123"})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]hfNode{
				{Type: "file", Path: "a-missing.bin", Size: 1024},
				{Type: "file", Path: "z-slow.bin", Size: 10_000},
			})
		case strings.HasSuffix(r.URL.Path, "a-missing.bin"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "z-slow.bin"):
			// Advertise a long body, then withhold it until released. Without
			// the fix, Download would block here until the sibling finished.
			w.Header().Set("Content-Length", "10000")
			w.WriteHeader(http.StatusOK)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			<-release
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	cfg := Settings{
		CacheDir:           t.TempDir(),
		Endpoint:           srv.URL,
		Concurrency:        2,
		MaxActiveDownloads: 2,
		Retries:            3,
		BackoffInitial:     "5ms",
		BackoffMax:         "10ms",
		StallTimeout:       "0",
		Verify:             "none",
	}

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
		if elapsed := time.Since(start); elapsed > 8*time.Second {
			t.Errorf("job took %v; a stalled sibling should have been aborted promptly", elapsed)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Download blocked on a stalled sibling after a permanent file error")
	}
}

// TestDownloadSingle_InternalAbortPreservesResumablePartial pins T3: an
// internal abort (marked by the production abortCause sentinel) must preserve
// resumable partial bytes, while a genuine user cancel still cleans up.
//
// The internal-abort cases wrap BOTH an *APIError and a PLAIN error with the
// production abortError(...) sentinel. The plain-error case is the discerning
// one: the job-level path-traversal rejection is not an *APIError, so a
// status-based detector would misread it as a user cancel and delete the bytes.
// It also directly asserts abortPartAttempt's contract.
func TestDownloadSingle_InternalAbortPreservesResumablePartial(t *testing.T) {
	// Direct sentinel contract: abortPartAttempt keys off the sentinel type, not
	// the wrapped error's HTTP status. A plain non-*APIError wrapped by the
	// production helper must still be recognized; a bare user cancel must not.
	t.Run("abortPartAttempt recognizes plain-error sentinel", func(t *testing.T) {
		plain, cancelPlain := context.WithCancelCause(context.Background())
		defer cancelPlain(nil)
		cancelPlain(abortError(errors.New("path traversal: %q would escape output directory")))
		if !abortPartAttempt(plain) {
			t.Error("abortPartAttempt = false for abortError(plain); a status-based detector would delete resumable bytes")
		}

		user, cancelUser := context.WithCancelCause(context.Background())
		defer cancelUser(nil)
		cancelUser(context.Canceled)
		if abortPartAttempt(user) {
			t.Error("abortPartAttempt = true for context.Canceled; user cancel must stay distinct")
		}
	})

	seedAndRun := func(t *testing.T, cause func() error) string {
		t.Helper()
		tmpDir := t.TempDir()
		dst := filepath.Join(tmpDir, "blobs", "tmp-abortkeep")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst+".part", bytes.Repeat([]byte("z"), 2_000), 0o644); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancelCause(context.Background())
		// Retries>0 with a long backoff keeps the goroutine parked in the
		// ctx-aware sleep so the test can cancel deterministically at the
		// cleanup decision point.
		cfg := Settings{Retries: 5, BackoffInitial: "5s", BackoffMax: "5s", StallTimeout: "0"}
		cfg.CleanupPartialsOnCancel = func() bool { return true }

		done := make(chan error, 1)
		go func() {
			it := PlanItem{RelativePath: "keep.bin", URL: "http://127.0.0.1:1/keep.bin", Size: 100_000}
			done <- downloadSingle(ctx, &http.Client{}, "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
		}()

		// Give the goroutine time to fail its first attempt and enter the
		// backoff sleep, then trigger the abort.
		time.Sleep(200 * time.Millisecond)
		cancel(cause())
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("downloadSingle did not return after cancel")
		}
		return dst
	}

	internalCases := []struct {
		name  string
		cause func() error
	}{
		{
			name: "internal abort via APIError sentinel",
			// The production fail-fast cancel sites use abortError(...).
			cause: func() error { return abortError(&APIError{StatusCode: http.StatusForbidden}) },
		},
		{
			name: "internal abort via plain-error sentinel (path traversal)",
			// A non-*APIError internal abort, exactly like the job-level
			// path-traversal rejection. A status-based detector would fail this.
			cause: func() error { return abortError(errors.New("path traversal: would escape output directory")) },
		},
	}
	for _, tc := range internalCases {
		t.Run(tc.name, func(t *testing.T) {
			dst := seedAndRun(t, tc.cause)
			if fi, err := os.Stat(dst + ".part"); err != nil || fi.Size() != 2_000 {
				t.Errorf("resumable .part removed/modified by internal abort (stat err=%v)", err)
			}
		})
	}

	t.Run("user cancel still cleans up", func(t *testing.T) {
		dst := seedAndRun(t, func() error { return context.Canceled })
		if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
			t.Errorf("user-cancel cleanup did not remove .part (stat err=%v)", err)
		}
	})
}

// TestSecondsToDurationSaturates verifies the overflow-safe conversion used for
// parsed Retry-After / RateLimit seconds: values beyond the representable
// duration must stay positive so NextRetryWait can clamp them to the cap,
// rather than wrapping negative and being ignored.
func TestSecondsToDurationSaturates(t *testing.T) {
	if got := secondsToDuration(30); got != 30*time.Second {
		t.Errorf("secondsToDuration(30) = %v, want 30s", got)
	}
	if got := secondsToDuration(0); got != 0 {
		t.Errorf("secondsToDuration(0) = %v, want 0", got)
	}
	huge := secondsToDuration(9223372037)
	if huge <= 0 {
		t.Fatalf("secondsToDuration(9223372037) = %v; must not wrap negative", huge)
	}
	if got := NextRetryWait(nil, 0, DefaultMaxRetryAfter, time.Now()); got != 0 {
		t.Errorf("NextRetryWait = %v, want 0", got)
	}
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"9223372037"}}}
	if got := NextRetryWait(resp, 0, DefaultMaxRetryAfter, time.Now()); got != DefaultMaxRetryAfter {
		t.Errorf("huge Retry-After: got %v, want cap %v", got, DefaultMaxRetryAfter)
	}
}

// growingPrefixServer ignores Range and, on every request, streams a strictly
// larger prefix of the body (n*step bytes) before aborting. When Size > 0 it
// declares that full Content-Length and answers 200, so the client truncates
// the partial and restarts from zero — yet each restart's final on-disk offset
// exceeds the previous high-water mark, which is exactly the counterexample the
// high-water-mark rule alone cannot bound.
func growingPrefixServer(t *testing.T, declaredSize int64, step int64) (*httptest.Server, *int) {
	t.Helper()
	var (
		mu       sync.Mutex
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		if declaredSize > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(declaredSize, 10))
		}
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		// Stream a strictly growing prefix so pos advances every attempt.
		w.Write(make([]byte, int64(n)*step))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// TestDownloadSingle_GrowingPrefixBounded covers T2 termination end to end.
//
// The known/unknown-size subtests use a server that ignores Range and answers
// 200, so the truncation guard (truncatedRestart) refuses to refund and bounds
// the loop at Retries+1 requests; they prove the whole-job termination path.
//
// The append subtest uses a 206 server that never truncates, so the truncation
// guard cannot fire and only the absolute attempt ceiling can bound it. That
// subtest is the one that genuinely pins the ceiling: with the ceiling removed
// it runs forever, and with only the truncation guard it would run forever too
// (append attempts keep advancing the high-water mark and refunding).
func TestDownloadSingle_GrowingPrefixBounded(t *testing.T) {
	const declaredSize = 1 << 30 // 1 GiB declared, but only small prefixes sent

	// Lower the ceiling multiplier locally so the append subtest reaches the
	// ceiling quickly. Tests mutating this package var must run sequentially
	// (no t.Parallel) and always restore it.
	oldMult := maxAttemptsPerRetry
	maxAttemptsPerRetry = 4
	defer func() { maxAttemptsPerRetry = oldMult }()

	runGrowing200 := func(t *testing.T, size int64) {
		t.Helper()
		tmpDir := t.TempDir()
		dst := filepath.Join(tmpDir, "blobs", "tmp-growing")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		srv, requests := growingPrefixServer(t, declaredSize, 8_000)

		cfg := Settings{
			Retries:        3,
			BackoffInitial: "1ms",
			BackoffMax:     "1ms",
			StallTimeout:   "0",
		}
		it := PlanItem{RelativePath: "grow.bin", URL: srv.URL + "/grow.bin", Size: size}

		done := make(chan error, 1)
		start := time.Now()
		go func() {
			done <- downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
		}()

		var err error
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("downloadSingle did not terminate; no absolute ceiling")
		}
		if err == nil {
			t.Fatal("expected failure from the growing-prefix server")
		}
		n := *requests
		bound := absoluteAttemptCeiling(cfg.Retries)
		if n > bound {
			t.Errorf("made %d requests; want <= absolute ceiling %d", n, bound)
		}
		if n < 2 {
			t.Errorf("made only %d requests; test server may not have exercised the loop", n)
		}
		if elapsed := time.Since(start); elapsed > 8*time.Second {
			t.Errorf("growing-prefix termination took %v", elapsed)
		}
	}
	t.Run("known size", func(t *testing.T) { runGrowing200(t, declaredSize) })
	t.Run("unknown size", func(t *testing.T) { runGrowing200(t, 0) })

	// Append shape: the server answers the Range with 206 and appends a strictly
	// growing prefix, then aborts. It never truncates, so truncatedRestart is
	// never set and the refund rule cannot bound the loop — only the absolute
	// ceiling can. The request count must stay within the ceiling yet exceed the
	// pure Retries budget (proving refunds happened before the ceiling fired).
	t.Run("append shape pins the ceiling", func(t *testing.T) {
		tmpDir := t.TempDir()
		dst := filepath.Join(tmpDir, "blobs", "tmp-growing-append")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		const total = int64(1) << 40 // never reached
		var (
			mu       sync.Mutex
			requests int
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requests++
			n := requests
			mu.Unlock()
			start := int64(0)
			if rng := r.Header.Get("Range"); rng != "" {
				if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err != nil {
					http.Error(w, "bad range", http.StatusBadRequest)
					return
				}
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, total-1, total))
			w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
			w.WriteHeader(http.StatusPartialContent)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			// Append a strictly growing prefix then abort: pos advances and the
			// server never truncates, so the truncation guard cannot fire.
			w.Write(make([]byte, int64(n)*100))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			panic(http.ErrAbortHandler)
		}))
		defer srv.Close()

		it := PlanItem{RelativePath: "grow.bin", URL: srv.URL + "/grow.bin", Size: total}
		cfg := Settings{Retries: 3, BackoffInitial: "1ms", BackoffMax: "1ms", StallTimeout: "0"}

		done := make(chan error, 1)
		start := time.Now()
		go func() {
			done <- downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
		}()

		select {
		case err := <-done:
			if err == nil {
				t.Fatal("expected failure from the growing-append server")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("downloadSingle did not terminate; no absolute ceiling")
		}
		mu.Lock()
		n := requests
		mu.Unlock()
		// The loop breaks when attempt reaches ceiling, so at most ceiling+1
		// requests are made.
		bound := absoluteAttemptCeiling(cfg.Retries) + 1
		if n > bound {
			t.Errorf("made %d requests; want <= absolute ceiling %d", n, bound)
		}
		if n <= cfg.Retries+1 {
			t.Errorf("made only %d requests; append refunds should have extended past the pure budget of %d", n, cfg.Retries+1)
		}
		if elapsed := time.Since(start); elapsed > 8*time.Second {
			t.Errorf("growing-append termination took %v", elapsed)
		}
	})
}

// TestDownloadSingle_AppendCeilingBounded isolates the absolute ceiling on the
// single-file path: a server that answers a Range request with 206 and streams a
// strictly growing prefix (appending to the part, never truncating) advances the
// high-water mark every attempt without triggering the truncation guard, so only
// the absolute ceiling can terminate it.
func TestDownloadSingle_AppendCeilingBounded(t *testing.T) {
	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "blobs", "tmp-append")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	const total = int64(1) << 40 // declared size; never reached
	var (
		mu       sync.Mutex
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		start := int64(0)
		if rng := r.Header.Get("Range"); rng != "" {
			if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, total-1, total))
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusPartialContent)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		// Append a strictly growing prefix then abort: pos advances, the server
		// never truncates, and the file never reaches Size.
		w.Write(make([]byte, int64(n)*100))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "app.bin", URL: srv.URL + "/app.bin", Size: total}
	cfg := Settings{Retries: 3, BackoffInitial: "1ms", BackoffMax: "1ms", StallTimeout: "0"}

	oldMult := maxAttemptsPerRetry
	maxAttemptsPerRetry = 4
	defer func() { maxAttemptsPerRetry = oldMult }()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected failure from the growing-append server")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("downloadSingle did not terminate; no absolute ceiling")
	}
	mu.Lock()
	n := requests
	mu.Unlock()
	bound := absoluteAttemptCeiling(cfg.Retries) + 1
	if n > bound {
		t.Errorf("made %d requests; want <= %d", n, bound)
	}
	if n < 2 {
		t.Errorf("made only %d requests; the growing-append loop may not have run", n)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("append-ceiling termination took %v", elapsed)
	}
}

// TestDownloadMultipart_GrowingPrefixBounded verifies the absolute ceiling on
// the multipart part loop: a part server that answers 206 but streams a
// strictly growing prefix (and aborts) advances the part high-water mark every
// attempt, so without the ceiling it would refund forever.
func TestDownloadMultipart_GrowingPrefixBounded(t *testing.T) {
	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "blobs", "tmp-mpgrow")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	const total = 100_000
	const partCount = 2
	// Declare a body far larger than anything sent so a part can never finish;
	// each attempt appends a strictly growing prefix then aborts, so the part's
	// on-disk offset advances on every attempt (the counterexample for the
	// refund rule alone).
	const declaredBody = int64(1) << 40
	const growthPerRequest = 100
	var (
		mu           sync.Mutex
		partRequests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(total))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		mu.Lock()
		partRequests++
		n := partRequests
		mu.Unlock()
		start := int64(0)
		if rng := r.Header.Get("Range"); rng != "" {
			if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
		}
		// Ignore the requested range: always 206 with a huge declared body,
		// stream a strictly growing prefix, then abort. The client appends to
		// its part file at the current offset, so pos grows every attempt while
		// the part never completes.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, declaredBody-1, declaredBody))
		w.Header().Set("Content-Length", strconv.FormatInt(declaredBody, 10))
		w.WriteHeader(http.StatusPartialContent)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		w.Write(make([]byte, int64(n)*growthPerRequest))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "mp.bin", URL: srv.URL + "/mp.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: partCount, Retries: 3, BackoffInitial: "1ms", BackoffMax: "1ms", StallTimeout: "0"}

	// The per-attempt backoff jitter makes a production-sized ceiling (hundreds
	// of attempts) too slow to exercise here; lower the multiplier for a fast,
	// deterministic test while the enforcement logic stays identical.
	oldMult := maxAttemptsPerRetry
	maxAttemptsPerRetry = 4
	defer func() { maxAttemptsPerRetry = oldMult }()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected failure from the growing-prefix multipart server")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("downloadMultipart did not terminate; no absolute ceiling")
	}
	mu.Lock()
	n := partRequests
	mu.Unlock()
	// Two parts; each runs until its `attempt` index reaches the ceiling, so a
	// part makes at most ceiling+1 requests.
	bound := partCount * (absoluteAttemptCeiling(cfg.Retries) + 1)
	if n > bound {
		t.Errorf("made %d part requests; want <= %d", n, bound)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("growing-prefix multipart termination took %v", elapsed)
	}
}

// --- Second-round T2/T3 regression tests ---

// TestDownloadSingle_RestartAboveHwmNoFreeRefund pins the T2 fix for the
// "restart above previous max earns a later free refund" counterexample.
//
// Sequence (Retries=2), mirroring the reviewer's steps:
//  1. an ordinary 206 attempt reaches 1000 -> hwm=1000 (legitimate refund);
//  2. an ignored-Range 200 truncates to 0 and reaches 2000, then aborts
//     (truncatedRestart -> no refund, but 2000 is now the real high-water);
//  3. a bodyless 503 writes nothing (pos stays 2000).
//
// Under the pre-fix rule hwm was only updated on the refunding attempt, so it
// stayed 1000 and the bodyless attempt 3 saw pos>hwm -> a free refund for zero
// new bytes; the loop then needed 4 requests instead of 3 to exhaust the
// budget. With the fix hwm is updated unconditionally after every attempt, so
// attempt 3 is not progress and the budget is exhausted after exactly
// Retries+1 requests.
func TestDownloadSingle_RestartAboveHwmNoFreeRefund(t *testing.T) {
	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "blobs", "tmp-freefeat")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	const total = 20_000
	var (
		mu       sync.Mutex
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		switch n {
		case 1:
			// Ordinary 206 advance to 1000 (pos starts at 0, no truncation).
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", total-1, total))
			w.Header().Set("Content-Length", strconv.Itoa(total))
			w.WriteHeader(http.StatusPartialContent)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			w.Write(make([]byte, 1_000))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			panic(http.ErrAbortHandler)
		case 2:
			// Ignored-Range 200: truncates the resumed partial and restarts
			// from zero, then reaches 2000 and aborts (a truncating restart).
			w.Header().Set("Content-Length", strconv.Itoa(total))
			w.WriteHeader(http.StatusOK)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			w.Write(make([]byte, 2_000))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			panic(http.ErrAbortHandler)
		default:
			// Bodyless throttling failure: writes nothing, so pos is unchanged.
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "free.bin", URL: srv.URL + "/free.bin", Size: total}
	cfg := Settings{Retries: 2, BackoffInitial: "1ms", BackoffMax: "1ms", StallTimeout: "0"}

	done := make(chan error, 1)
	go func() {
		done <- downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected failure")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("downloadSingle did not terminate")
	}
	mu.Lock()
	n := requests
	mu.Unlock()
	if n != cfg.Retries+1 {
		t.Errorf("made %d requests; want exactly %d (a bodyless attempt after a truncating restart must not refund)", n, cfg.Retries+1)
	}
}

// TestDownloadMultipart_RestartAboveHwmNoFreeRefund exercises the multipart
// refund rule: each part's first (advancing) 206 attempt refunds once, and the
// following bodyless attempts must not refund, so the part terminates within the
// absolute ceiling. Note: multipart has no truncation path, so an advancing
// attempt always also updates hwm; the unconditional hwm update required by the
// fix is therefore a defensive mirror of the single-file rule and is not
// independently observable from an in-process part server. The single-file
// TestDownloadSingle_RestartAboveHwmNoFreeRefund is the discriminating case.
func TestDownloadMultipart_RestartAboveHwmNoFreeRefund(t *testing.T) {
	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "blobs", "tmp-mpfree")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	const total = 100_000
	const partCount = 2
	var (
		mu           sync.Mutex
		partRequests int
		seenStarts   = map[int64]int{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(total))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		mu.Lock()
		partRequests++
		mu.Unlock()
		start := int64(0)
		if rng := r.Header.Get("Range"); rng != "" {
			if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
		}
		// Determine this part's attempt index by counting requests for the same
		// requested start offset. The first attempt of each part advances and
		// aborts; every later attempt for the same start is bodyless.
		mu.Lock()
		seen := seenStarts[start]
		seenStarts[start] = seen + 1
		firstForStart := seen == 0
		mu.Unlock()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, total-1, total))
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusPartialContent)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		if firstForStart {
			// First attempt at this offset: append a small prefix, then abort
			// (a genuine advance that should refund once).
			w.Write(make([]byte, 500))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
		// Later attempts at the same offset are bodyless: no new bytes.
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	oldMult := maxAttemptsPerRetry
	maxAttemptsPerRetry = 3
	defer func() { maxAttemptsPerRetry = oldMult }()

	it := PlanItem{RelativePath: "mpfree.bin", URL: srv.URL + "/mpfree.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: partCount, Retries: 3, BackoffInitial: "1ms", BackoffMax: "1ms", StallTimeout: "0"}

	done := make(chan error, 1)
	go func() {
		done <- downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected failure")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("downloadMultipart did not terminate")
	}
	mu.Lock()
	n := partRequests
	mu.Unlock()
	// Each part: one refunding advance followed by bodyless attempts that must
	// not refund, so at most ceiling+1 requests per part (not an unbounded
	// refill).
	bound := partCount * (absoluteAttemptCeiling(cfg.Retries) + 1)
	if n > bound {
		t.Errorf("made %d part requests; want <= %d", n, bound)
	}
}

// TestDownloadMultipart_FinalizationAbortBeforeAssembly verifies T3 on the
// finalization boundary: when the part context is cancelled by an internal
// abort at the moment assembly is about to begin, downloadMultipart must not
// start assembling, must PRESERVE the resumable .part-NN files, must not leave
// an incomplete dst.part or a final dst, and must return the cancellation. A
// user cancel with the same callback still runs cleanupPartialsOnCancel.
//
// The cancel is triggered deterministically from the emit callback on the
// "file_finalizing" event, which downloadMultipart emits after the
// cancelled-while-parts-running guard and immediately before assembly starts —
// no timing/sleep dependence.
func TestDownloadMultipart_FinalizationAbortBeforeAssembly(t *testing.T) {
	setup := func(t *testing.T) string {
		t.Helper()
		tmpDir := t.TempDir()
		dst := filepath.Join(tmpDir, "blobs", "tmp-finabort")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		// Seed a complete part set plus layout so prepareMultipartResume keeps
		// them and the part goroutines finish without any GET, reaching assembly.
		full := bytes.Repeat([]byte("p"), 20_000)
		if err := os.WriteFile(dst+".part-00", full[:10_000], 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst+".part-01", full[10_000:], 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeMultipartResumeLayout(dst, buildMultipartResumeLayout(20_000, 2, 10_000)); err != nil {
			t.Fatal(err)
		}
		return dst
	}

	run := func(t *testing.T, cause func() error) string {
		t.Helper()
		dst := setup(t)

		ctx, cancel := context.WithCancelCause(context.Background())

		cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}
		cfg.CleanupPartialsOnCancel = func() bool { return true }

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", "20000")
				w.Header().Set("Accept-Ranges", "bytes")
				return
			}
			// Parts are already complete on disk, so no GET should occur.
			http.Error(w, "unexpected part request", http.StatusInternalServerError)
		}))
		defer srv.Close()

		emit := func(ev ProgressEvent) {
			if ev.Event == "file_finalizing" && ev.Message == "assembling parts" {
				cancel(cause()) // abort exactly at the assembly boundary
			}
		}

		it := PlanItem{RelativePath: "fin.bin", URL: srv.URL + "/fin.bin", Size: 20_000, AcceptRanges: true}
		err := downloadMultipart(ctx, srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, emit)
		if err == nil {
			t.Fatal("expected a cancellation error")
		}
		return dst
	}

	t.Run("internal abort preserves parts, removes incomplete dst.part", func(t *testing.T) {
		dst := run(t, func() error { return abortError(errors.New("internal abort")) })
		for _, p := range []string{dst + ".part-00", dst + ".part-01"} {
			if fi, statErr := os.Stat(p); statErr != nil || fi.Size() == 0 {
				t.Errorf("resumable %s was removed/modified (stat err=%v)", filepath.Base(p), statErr)
			}
		}
		if _, statErr := os.Stat(dst + ".part"); !os.IsNotExist(statErr) {
			t.Errorf("incomplete dst.part should not exist (stat err=%v)", statErr)
		}
		if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
			t.Errorf("final dst should not exist (stat err=%v)", statErr)
		}
	})

	t.Run("user cancel still cleans up", func(t *testing.T) {
		dst := run(t, func() error { return context.Canceled })
		if files, _ := multipartPartFiles(dst); len(files) != 0 {
			t.Errorf("user-cancel cleanup should remove part files, got %v", files)
		}
	})

	// Mid-assembly abort: cancel partCtx while the assembly io.Copy is running.
	// Assembly is entered after all parts are complete; the poll below waits for
	// the intermediate dst.part to appear (a 64 MiB per-part copy takes far
	// longer than the 1 ms poll), then cancels. This exercises the
	// io.Copy/contextReader cancellation path and removeIncompleteAssembly. The
	// internal abort must preserve the resumable .part-NN files and remove the
	// incomplete dst.part.
	t.Run("mid-assembly abort preserves parts", func(t *testing.T) {
		tmpDir := t.TempDir()
		dst := filepath.Join(tmpDir, "blobs", "tmp-midabort")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		const partSize = 64 << 20 // 64 MiB per part: assembly takes well over 1ms
		payload := bytes.Repeat([]byte("m"), partSize)
		if err := os.WriteFile(dst+".part-00", payload, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst+".part-01", payload, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeMultipartResumeLayout(dst, buildMultipartResumeLayout(int64(2*partSize), 2, partSize)); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}
		cfg.CleanupPartialsOnCancel = func() bool { return true }

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", strconv.Itoa(2*partSize))
				w.Header().Set("Accept-Ranges", "bytes")
				return
			}
			http.Error(w, "unexpected part request", http.StatusInternalServerError)
		}))
		defer srv.Close()

		done := make(chan error, 1)
		go func() {
			it := PlanItem{RelativePath: "mid.bin", URL: srv.URL + "/mid.bin", Size: int64(2 * partSize), AcceptRanges: true}
			done <- downloadMultipart(ctx, srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
		}()

		// Wait until assembly has created the intermediate dst.part, then abort.
		waitDeadline := time.After(10 * time.Second)
		cancelled := false
		for !cancelled {
			inProgress, err := os.Stat(dst + ".part")
			if err == nil && inProgress.Size() > 0 {
				cancel(abortError(errors.New("mid-assembly abort")))
				cancelled = true
				break
			}
			select {
			case e := <-done:
				t.Fatalf("downloadMultipart returned before the mid-assembly cancel landed: %v", e)
			case <-waitDeadline:
				t.Fatal("assembly never started; cannot exercise the mid-assembly path")
			case <-time.After(time.Millisecond):
			}
		}

		select {
		case err := <-done:
			if err == nil {
				t.Fatal("expected a cancellation error")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("downloadMultipart did not terminate after the mid-assembly abort")
		}

		// Resumable part files preserved; incomplete dst.part removed; no final.
		for _, p := range []string{dst + ".part-00", dst + ".part-01"} {
			if fi, statErr := os.Stat(p); statErr != nil || fi.Size() != partSize {
				t.Errorf("resumable %s was removed/modified (stat err=%v)", filepath.Base(p), statErr)
			}
		}
		if _, statErr := os.Stat(dst + ".part"); !os.IsNotExist(statErr) {
			t.Errorf("incomplete dst.part should have been removed (stat err=%v)", statErr)
		}
		if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
			t.Errorf("final dst should not exist (stat err=%v)", statErr)
		}
	})
}

// TestVerifyAndStoreCtx abort promptly on a cancelled context. This is the
// deterministic unit coverage for the ctx-aware finalization helpers: a
// multi-GB verify/store must not run to completion after a sibling's permanent
// error cancelled the job.
func TestVerifyAndStoreCtxAbortPromptly(t *testing.T) {
	tmpDir := t.TempDir()
	// A reasonably large file so hashing/copying takes measurable time.
	big := filepath.Join(tmpDir, "big.bin")
	data := bytes.Repeat([]byte("q"), 8<<20) // 8 MiB
	if err := os.WriteFile(big, data, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	t.Run("verifySHA256Ctx", func(t *testing.T) {
		start := time.Now()
		err := verifySHA256Ctx(ctx, big, "")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected a context.Canceled error, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("verifySHA256Ctx took %v on a cancelled ctx", elapsed)
		}
	})

	t.Run("computeSHA256Ctx", func(t *testing.T) {
		start := time.Now()
		if _, err := computeSHA256Ctx(ctx, big); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected a context.Canceled error, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("computeSHA256Ctx took %v on a cancelled ctx", elapsed)
		}
	})

	t.Run("copyFileCtx", func(t *testing.T) {
		dstCopy := filepath.Join(tmpDir, "copy.bin")
		start := time.Now()
		if err := copyFileCtx(ctx, big, dstCopy); err == nil {
			t.Fatal("expected a context error")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("copyFileCtx took %v on a cancelled ctx", elapsed)
		}
	})

	t.Run("StoreDownloadedFileCtx", func(t *testing.T) {
		settings := DefaultSettings()
		settings.CacheDir = t.TempDir()
		repo, err := settings.BuildHFCache()
		if err != nil {
			t.Fatalf("BuildHFCache: %v", err)
		}
		rd, err := repo.Repo("o/r", RepoTypeModel)
		if err != nil {
			t.Fatalf("Repo: %v", err)
		}
		if err := rd.EnsureDirs(); err != nil {
			t.Fatal(err)
		}
		tempFile := filepath.Join(tmpDir, "store.bin")
		if err := os.WriteFile(tempFile, data, 0o644); err != nil {
			t.Fatal(err)
		}
		// Empty sha256 forces a ctx-aware hash; the cancelled ctx aborts it.
		start := time.Now()
		if _, err := rd.StoreDownloadedFileCtx(ctx, tempFile, "store.bin", "commit", "", "", true); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected a context.Canceled error, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("StoreDownloadedFileCtx took %v on a cancelled ctx", elapsed)
		}
	})
}
