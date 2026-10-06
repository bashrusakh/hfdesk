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
