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
	"sync/atomic"
	"testing"
	"time"
)

// flakyRangeServer serves HTTP Range requests for a fixed byte slice but aborts
// the connection mid-stream on the FIRST request it sees for each distinct
// "end" byte (i.e. once per part). Subsequent retries for the same part — which
// carry the same `end` but a higher `start` — are served cleanly. This
// simulates a flaky network where each part gets interrupted once.
// Returns the test server and a pointer to the ordered list of Range headers
// the server observed.
func flakyRangeServer(t *testing.T, full []byte) (*httptest.Server, *[]string) {
	t.Helper()
	var (
		mu        sync.Mutex
		cutEnds   = make(map[int64]bool)
		rangeHdrs []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		rng := r.Header.Get("Range")
		var start, end int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil || end >= int64(len(full)) {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}

		mu.Lock()
		rangeHdrs = append(rangeHdrs, rng)
		cut := !cutEnds[end]
		if cut {
			cutEnds[end] = true
		}
		mu.Unlock()

		content := full[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(full)))
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusPartialContent)

		if cut && len(content) > 4 {
			// Write half the bytes then abort. The client sees a short body
			// and falls into its retry path.
			half := len(content) / 2
			w.Write(content[:half])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		w.Write(content)
	}))
	return srv, &rangeHdrs
}

// TestDownloadMultipart_ResumesAfterFlakyConnection simulates the scenario
// from github issue #75: a slow/flaky network where each range request gets
// cut mid-stream, the client retries, and the download should converge. Before
// the fix for issue #70 each part restarted from zero on every retry, so the
// user saw progress oscillating between two values for hours. This test
// asserts: (a) the download completes, (b) the final bytes are correct, and
// (c) the retries use Range requests that RESUME from the cut point rather
// than re-requesting the full part range.
func TestDownloadMultipart_ResumesAfterFlakyConnection(t *testing.T) {
	tmpDir := t.TempDir()
	const totalSize = 20000
	full := make([]byte, totalSize)
	for i := range full {
		full[i] = byte(i % 251)
	}
	const nParts = 4
	const partSize = totalSize / nParts // 5000

	dst := filepath.Join(tmpDir, "blobs", "tmp-flaky")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	srv, rangeHdrs := flakyRangeServer(t, full)
	defer srv.Close()

	it := PlanItem{
		RelativePath: "flaky.bin",
		URL:          srv.URL + "/flaky.bin",
		Size:         int64(len(full)),
		AcceptRanges: true,
	}
	cfg := Settings{Concurrency: nParts, Retries: 5}

	err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("final content mismatch: got len=%d want len=%d", len(got), len(full))
	}

	// For each part, verify:
	//   - The initial "bytes=start-end" request was made (got cut).
	//   - A SECOND request was made with start = start+half, not start=start
	//     (the latter would indicate truncate-and-restart behavior).
	seen := *rangeHdrs
	for i := 0; i < nParts; i++ {
		start := int64(i * partSize)
		end := int64((i+1)*partSize - 1)
		initial := fmt.Sprintf("bytes=%d-%d", start, end)

		// Must have at least one initial request.
		foundInitial := false
		for _, r := range seen {
			if r == initial {
				foundInitial = true
				break
			}
		}
		if !foundInitial {
			t.Errorf("part %d: no initial request %q seen; requests=%v", i, initial, seen)
		}

		// Must have a resume request: start somewhere > start, end = end.
		foundResume := false
		for _, r := range seen {
			var rs, re int64
			if _, err := fmt.Sscanf(r, "bytes=%d-%d", &rs, &re); err != nil {
				continue
			}
			if rs > start && re == end {
				foundResume = true
				break
			}
		}
		if !foundResume {
			t.Errorf("part %d: no resume request (start > %d, end = %d) seen; requests=%v",
				i, start, end, seen)
		}
	}
}

// slowRangeServer serves Range requests at a bounded byte rate so the test
// can cancel context mid-stream deterministically. It streams bytes in small
// chunks with a short sleep between chunks.
func slowRangeServer(t *testing.T, full []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		rng := r.Header.Get("Range")
		var start, end int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil || end >= int64(len(full)) {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		content := full[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(full)))
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusPartialContent)

		flusher, _ := w.(http.Flusher)
		const chunk = 512
		for i := 0; i < len(content); i += chunk {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			j := i + chunk
			if j > len(content) {
				j = len(content)
			}
			if _, err := w.Write(content[i:j]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}))
}

// TestDownloadMultipart_CancelMidStreamDoesNotClaim100 is a regression test
// for the UI bug: when a user paused mid-download, downloadMultipart used to
// emit a "file_progress downloaded==total" event via its tail path even
// though the parts were incomplete, then ran assembly over the partial set
// and destroyed the part files. On a subsequent resume the UI had already
// snapped to 100%, assembly had corrupted/removed the partial bytes, and
// the real resume appeared to start over from zero.
//
// This test cancels the context mid-download and asserts:
//   - downloadMultipart returns a context error (not nil success)
//   - no file_progress event ever reports downloaded == total
//   - at least one part-XX file survives on disk after return, proving the
//     partial bytes were not destroyed by an erroneous assembly pass
func TestDownloadMultipart_CancelMidStreamDoesNotClaim100(t *testing.T) {
	tmpDir := t.TempDir()
	const totalSize = 200_000
	full := make([]byte, totalSize)
	for i := range full {
		full[i] = byte(i % 251)
	}
	const nParts = 4

	dst := filepath.Join(tmpDir, "blobs", "tmp-cancel")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	srv := slowRangeServer(t, full)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	var (
		mu             sync.Mutex
		saw100         bool
		maxObserved    int64
		lastDownloaded int64
	)
	emit := func(ev ProgressEvent) {
		if ev.Event != "file_progress" || ev.Path != "cancel.bin" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		lastDownloaded = ev.Downloaded
		if ev.Downloaded > maxObserved {
			maxObserved = ev.Downloaded
		}
		if ev.Downloaded == ev.Total && ev.Total == int64(totalSize) {
			saw100 = true
		}
	}

	// Cancel once we've made real forward progress.
	go func() {
		for {
			mu.Lock()
			p := maxObserved
			mu.Unlock()
			if p > 0 && p < totalSize/2 {
				cancel()
				return
			}
			if p >= totalSize/2 {
				// Download is racing ahead of us; cancel anyway to
				// still exercise the early-exit path.
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	it := PlanItem{
		RelativePath: "cancel.bin",
		URL:          srv.URL + "/cancel.bin",
		Size:         int64(len(full)),
		AcceptRanges: true,
	}
	// Use the default-ish retry count: with Retries>0, a goroutine that
	// gets a context error during its request enters the retry branch and
	// returns silently via sleepCtx without pushing to errCh — exactly the
	// path that triggered the regression in production.
	cfg := Settings{
		Concurrency:    nParts,
		Retries:        4,
		BackoffInitial: "50ms",
		BackoffMax:     "100ms",
	}

	err := downloadMultipart(ctx, srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, emit)
	if err == nil {
		t.Fatal("downloadMultipart succeeded despite cancellation — expected a context error")
	}

	mu.Lock()
	got100 := saw100
	lastSeen := lastDownloaded
	mu.Unlock()

	if got100 {
		t.Errorf("observed a 100%% file_progress event on cancel (last downloaded=%d, total=%d)",
			lastSeen, totalSize)
	}

	// At least one part file should survive on disk — assembly must not
	// have run and deleted them.
	survived := 0
	for i := 0; i < nParts; i++ {
		p := fmt.Sprintf("%s.part-%02d", dst, i)
		if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
			survived++
		}
	}
	if survived == 0 {
		// It is valid for zero part files to exist if the cancel landed
		// before any body bytes were written, but we selected the cancel
		// point after the first progress event, so at least one part
		// should have committed bytes.
		t.Errorf("no part-NN files survived after cancel; assembly may have run and deleted them")
	}

	// The assembled final file must NOT exist.
	if _, err := os.Stat(dst); err == nil {
		t.Error("final dst file exists after cancellation; should have been left alone")
	}
}

// TestDownloadMultipart_ProgressMonotonic captures every file_progress event
// emitted during a flaky download and asserts that the reported downloaded
// byte count never regresses — i.e. the UI never jumps backwards. This is
// what the user of issue #75 observed ("2.4% → 2.5% → 2.4% for hours"), which
// happens when retries truncate a part file or the progress ticker observes
// parts being deleted during assembly / the verify step.
//
// We wait a short window after downloadMultipart returns to let the periodic
// ticker fire at least once in the post-assembly state. Before the fix, the
// ticker continued stating the now-deleted part files and emitted a
// downloaded=0 event, producing a steep backwards jump on the progress bar.
func TestDownloadMultipart_ProgressMonotonic(t *testing.T) {
	tmpDir := t.TempDir()
	const totalSize = 200_000
	full := make([]byte, totalSize)
	for i := range full {
		full[i] = byte(i % 251)
	}
	const nParts = 4

	dst := filepath.Join(tmpDir, "blobs", "tmp-mono")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	srv, _ := flakyRangeServer(t, full)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu        sync.Mutex
		events    []int64
		maxSoFar  atomic.Int64
		regressed atomic.Int32
	)
	emit := func(ev ProgressEvent) {
		if ev.Event != "file_progress" || ev.Path != "mono.bin" {
			return
		}
		mu.Lock()
		events = append(events, ev.Downloaded)
		mu.Unlock()
		for {
			cur := maxSoFar.Load()
			if ev.Downloaded >= cur {
				if maxSoFar.CompareAndSwap(cur, ev.Downloaded) {
					break
				}
				continue
			}
			// Regression: downloaded < previous max
			regressed.Add(1)
			break
		}
	}

	it := PlanItem{
		RelativePath: "mono.bin",
		URL:          srv.URL + "/mono.bin",
		Size:         int64(len(full)),
		AcceptRanges: true,
	}
	cfg := Settings{Concurrency: nParts, Retries: 5}

	err := downloadMultipart(ctx, srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, emit)
	if err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	// Allow the progress ticker to fire at least once post-assembly. On the
	// unfixed code, the ticker stats deleted part files and emits a
	// downloaded=0 event here.
	time.Sleep(300 * time.Millisecond)

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("final content mismatch")
	}

	if regressed.Load() > 0 {
		mu.Lock()
		evs := append([]int64(nil), events...)
		mu.Unlock()
		t.Errorf("observed %d progress regression(s) — events=%v", regressed.Load(), evs)
	}
}
