// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the shared staging-file ownership/resume state
// machine (issue #140 correction set). Each blocker test is written to fail
// against the pre-correction implementation:
//
//   - a failure after the final chunk must persist Done=true, not
//     Done=false with Committed==length (which planned bytes=End+1-End);
//   - a completed-looking record with a missing/wrong-sized payload must be
//     discarded and re-downloaded, never published as zeros;
//   - a foreign instance token with our own PID is not self-ownership;
//   - a resolved-commit mismatch invalidates the record;
//   - the claim heartbeat covers the finalization phase, and a heartbeat
//     failure while active is fatal.

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
	"sync/atomic"
	"testing"
	"time"
)

// TestDownload_OwnedClaimCountsAsSkipped verifies that the live-owner skip
// path (a staging file held by another writer, or an already-published blob
// discovered after the pre-download check) participates in
// skipOnce/skippedCount bookkeeping, so the final "skipped N" summary includes
// it. The live owner is simulated by holding the OS ownership lock, which is
// what a real writer holds for its whole interval; the pre-download lock probe
// (or, on the lease fallback, the claim refusal) turns it into a skip rather
// than a failure.
func TestDownload_OwnedClaimCountsAsSkipped(t *testing.T) {
	tmpDir := t.TempDir()
	const size = 64 * 1024
	full := rangedTestPayload(size)
	srv, sha, _ := fakeHubServer(t, full, nil)

	cacheDir := filepath.Join(tmpDir, "cache")
	blobsDir := filepath.Join(cacheDir, "hub", "models--o--r", "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A live writer's staging file: payload preallocated and an unreadable
	// (mid-claim) record, with the ownership lock held for the interval.
	staging := filepath.Join(blobsDir, sha+".incomplete")
	if err := os.WriteFile(staging, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging+".meta", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	hold, _ := acquireStagingOwnershipLock(staging)
	if hold != nil {
		defer hold.release()
	}

	cfg := Settings{
		CacheDir:           cacheDir,
		Endpoint:           srv.URL,
		Concurrency:        4,
		MaxActiveDownloads: 2,
		MultipartThreshold: "1KB",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
	var doneMessage string
	err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ev ProgressEvent) {
		if ev.Event == "done" {
			doneMessage = ev.Message
		}
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !strings.Contains(doneMessage, "skipped 1") {
		t.Errorf("post-claim skip was not counted: done message = %q", doneMessage)
	}
	if _, err := os.Stat(filepath.Join(blobsDir, sha)); err == nil {
		t.Error("a live-owned staging file was published")
	}
}

// TestRangedMultipart_ZeroConcurrencyStillCompletes is a safety guard: a
// direct caller that leaves Concurrency at zero must still get workers, never
// a "finalization" of queued-but-undownloaded ranges.
func TestRangedMultipart_ZeroConcurrencyStillCompletes(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 4000
	full := rangedTestPayload(total)
	dst := filepath.Join(tmpDir, "zero-concurrency.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, gotRanges := rangedTestServer(t, full)
	it := PlanItem{RelativePath: "zero-concurrency.bin", URL: srv.URL + "/zero-concurrency.bin", Size: total, AcceptRanges: true}
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 0, Retries: 0, StallTimeout: "0"}, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}
	assertExactFile(t, dst, full)
	if len(*gotRanges) == 0 {
		t.Error("zero concurrency published without downloading the queued ranges")
	}
}

// TestRangedMultipart_FinalChunkFailureNormalizesDone reproduces blocker 1
// with a cancel after the final byte: the caller cancels the job while the
// response is still open, but every requested byte has already been written.
// The persisted record must normalize to Done=true; the next run must finish
// without ever issuing a bytes=End+1-End request (416).
func TestRangedMultipart_FinalChunkFailureNormalizesDone(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 8000
	full := rangedTestPayload(total)

	dst := filepath.Join(tmpDir, "final-chunk.bin")
	staging := dst + ".incomplete"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	var (
		mu        sync.Mutex
		rangeHdrs []string
		firstGET  atomic.Int32
	)
	served := make(chan struct{})
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
		mu.Lock()
		rangeHdrs = append(rangeHdrs, r.Header.Get("Range"))
		mu.Unlock()

		if firstGET.Add(1) == 1 {
			// First run: deliver the whole requested range in flushed chunks,
			// then hold the response open until the test cancels. Every byte
			// is written before the cancellation surfaces.
			content := full[start : end+1]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
			w.WriteHeader(http.StatusPartialContent)
			for i := 0; i < len(content); i += 1024 {
				j := i + 1024
				if j > len(content) {
					j = len(content)
				}
				if _, err := w.Write(content[i:j]); err != nil {
					return
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			close(served)
			<-r.Context().Done()
			return
		}

		// Recovery run: reject the impossible bytes=End+1-End plan explicitly
		// so a non-normalized record cannot silently retry forever.
		if start < 0 || start > end || end >= total {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "final-chunk.bin", URL: srv.URL + "/final-chunk.bin", Size: total, AcceptRanges: true}
	firstCfg := Settings{Concurrency: 1, Retries: 0, StallTimeout: "0"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	reachedFull := make(chan struct{})
	var fullOnce sync.Once
	emit := func(ev ProgressEvent) {
		if ev.Event == "file_progress" && ev.Total > 0 && ev.Downloaded >= ev.Total {
			fullOnce.Do(func() { close(reachedFull) })
		}
	}
	go func() {
		_, err := downloadMultipart(ctx, srv.Client(), "", Job{Repo: "o/r"}, firstCfg, it, dst, multipartOpts{}, emit)
		done <- err
	}()
	select {
	case <-served:
	case err := <-done:
		t.Fatalf("first run finished before the body was served: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first run never served the body")
	}
	// The progress counter reaches Total only after every byte was written
	// to the staging file. Wait for that reading, then cancel while the
	// response is still open (the server holds it until cancellation).
	select {
	case <-reachedFull:
	case err := <-done:
		t.Fatalf("first run finished before the final byte was committed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first run never reported the final byte as written")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("first run error = %v, want context.Canceled", err)
	}

	meta, err := readStagingMeta(staging + ".meta")
	if err != nil {
		t.Fatalf("staging metadata missing after the cancelled run: %v", err)
	}
	if len(meta.Ranges) != 1 {
		t.Fatalf("unexpected range layout: %+v", meta.Ranges)
	}
	if !meta.Ranges[0].Done || meta.Ranges[0].Committed != total {
		t.Fatalf("final-chunk cancellation persisted non-normalized completion: %+v (want Done=true, Committed=%d)", meta.Ranges[0], total)
	}

	recoveryCfg := Settings{Concurrency: 1, Retries: 0, StallTimeout: "0"}
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, recoveryCfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("recovery run failed after the normalized checkpoint: %v", err)
	}
	assertExactFile(t, dst, full)

	mu.Lock()
	defer mu.Unlock()
	for _, rh := range rangeHdrs {
		var start, end int64
		if _, err := fmt.Sscanf(rh, "bytes=%d-%d", &start, &end); err != nil {
			continue
		}
		if start > end {
			t.Errorf("recovery issued an impossible range %q; the checkpoint was not normalized", rh)
		}
	}
}

// TestRangedMultipart_FinalChunkFailureDoesNotRetryPastEnd covers the retry
// variant of blocker 1: with a non-zero retry budget, a post-final-chunk
// failure must still complete the range instead of issuing bytes=End+1-End on
// the next attempt.
func TestRangedMultipart_FinalChunkFailureDoesNotRetryPastEnd(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 8000
	full := rangedTestPayload(total)
	dst := filepath.Join(tmpDir, "final-chunk-retry.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	var (
		mu        sync.Mutex
		rangeHdrs []string
		firstGET  atomic.Int32
	)
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
		mu.Lock()
		rangeHdrs = append(rangeHdrs, r.Header.Get("Range"))
		mu.Unlock()
		if start < 0 || start > end || end >= total {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if firstGET.Add(1) == 1 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
			w.WriteHeader(http.StatusPartialContent)
			if _, err := w.Write(full[start : end+1]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(400 * time.Millisecond)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "final-chunk-retry.bin", URL: srv.URL + "/final-chunk-retry.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 1, Retries: 2, StallTimeout: "80ms", BackoffInitial: "1ms", BackoffMax: "1ms"}
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart retried past the completed range: %v", err)
	}
	assertExactFile(t, dst, full)
	mu.Lock()
	defer mu.Unlock()
	for _, rh := range rangeHdrs {
		var start, end int64
		if _, err := fmt.Sscanf(rh, "bytes=%d-%d", &start, &end); err == nil && start > end {
			t.Errorf("retry issued an impossible range %q", rh)
		}
	}
}

// TestRangedMultipart_CompletePrefixRecordIsNormalizedNotReplayed seeds the
// exact pre-correction state blocker 1 produced (Committed==length with
// Done=false). Trust/normalization must turn it into a completed range at
// planning time and publish without any bytes=End+1-End request.
func TestRangedMultipart_CompletePrefixRecordIsNormalizedNotReplayed(t *testing.T) {
	const total = 8000
	full := rangedTestPayload(total)
	host, _ := os.Hostname()
	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "complete-prefix.bin")
	staging := dst + ".incomplete"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, full, 0o644); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(
		`{"pid":99999999,"instance":"foreign-dead","host":%q,"size":%d,"version":2,"repo":"o/r","path":"complete-prefix.bin","ranges":[{"start":0,"end":%d,"committed":%d,"done":false}]}`,
		host, total, total-1, total)
	if err := os.WriteFile(staging+".meta", []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * claimGracePeriod)
	for _, p := range []string{staging, staging + ".meta"} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}

	srv, gotRanges := rangedTestServer(t, full)
	it := PlanItem{RelativePath: "complete-prefix.bin", URL: srv.URL + "/complete-prefix.bin", Size: total, AcceptRanges: true}
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}
	assertExactFile(t, dst, full)
	if len(*gotRanges) != 0 {
		t.Errorf("complete-prefix record was not normalized; requests=%v", *gotRanges)
	}
}

// TestRangedMultipart_MissingOrWrongPayloadInvalidatesRecord reproduces
// blocker 2: a record can claim every range Done while its payload was deleted
// or replaced. The record must be discarded and the bytes re-fetched; a
// preallocated zero file must never be published in Verify=size mode.
func TestRangedMultipart_MissingOrWrongPayloadInvalidatesRecord(t *testing.T) {
	const total = 8000
	full := rangedTestPayload(total)
	host, _ := os.Hostname()

	cases := []struct {
		name    string
		payload func(t *testing.T, staging string)
	}{
		{"payload deleted", func(t *testing.T, staging string) {
			// Intentionally absent.
		}},
		{"payload wrong logical size", func(t *testing.T, staging string) {
			if err := os.WriteFile(staging, bytes.Repeat([]byte{0xA5}, total/2), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			dst := filepath.Join(tmpDir, "ghost.bin")
			staging := dst + ".incomplete"
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			tc.payload(t, staging)

			// A valid-looking completed record from a provably dead foreign
			// owner. Raw JSON keeps this fixture readable by the pre-fix code
			// (which ignores instance/host) for falsification.
			raw := fmt.Sprintf(
				`{"pid":99999999,"instance":"foreign-dead","host":%q,"size":%d,"version":2,"repo":"o/r","path":"ghost.bin","ranges":[{"start":0,"end":%d,"committed":%d,"done":true}]}`,
				host, total, total-1, total)
			if err := os.WriteFile(staging+".meta", []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}
			// A stale heartbeat makes the fixture independent of whether the
			// hostname lookup succeeded; a live owner would still be protected
			// by its fresh record.
			past := time.Now().Add(-2 * claimGracePeriod)
			if err := os.Chtimes(staging+".meta", past, past); err != nil {
				t.Fatal(err)
			}
			if _, serr := os.Stat(staging); serr == nil {
				if err := os.Chtimes(staging, past, past); err != nil {
					t.Fatal(err)
				}
			}

			srv, gotRanges := rangedTestServer(t, full)
			it := PlanItem{RelativePath: "ghost.bin", URL: srv.URL + "/ghost.bin", Size: total, AcceptRanges: true}
			cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}
			if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
				t.Fatalf("downloadMultipart: %v", err)
			}
			assertExactFile(t, dst, full)
			if len(*gotRanges) == 0 {
				t.Fatal("a completed-looking record with no valid payload was trusted; no bytes were fetched")
			}
			startedAtZero := false
			for _, rh := range *gotRanges {
				if strings.HasPrefix(rh, "bytes=0-") {
					startedAtZero = true
				}
			}
			if !startedAtZero {
				t.Errorf("fresh download did not include a request from offset 0: %v", *gotRanges)
			}
		})
	}
}

// TestRangedMultipart_ForeignInstanceWithOurPIDIsNotSelf reproduces blocker
// 3b's namespace/PID-reuse half: our PID with a different instance token is a
// foreign live owner while it holds the OS ownership lock (the fence a real
// writer keeps for its whole interval), and only becomes take-over-able when
// that lock is gone and the same-host owner is provably dead with a stale
// heartbeat.
func TestRangedMultipart_ForeignInstanceWithOurPIDIsNotSelf(t *testing.T) {
	const total = 8000
	full := rangedTestPayload(total)
	host, _ := os.Hostname()

	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "pid-reuse.bin")
	staging := dst + ".incomplete"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, bytes.Repeat([]byte{0xA5}, total), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(
		`{"pid":%d,"instance":"different-process-instance","host":%q,"size":%d,"version":2,"repo":"o/r","path":"pid-reuse.bin","ranges":[{"start":0,"end":%d,"committed":0,"done":false}]}`,
		os.Getpid(), host, total, total-1)
	if err := os.WriteFile(staging+".meta", []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
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
		http.Error(w, "body must not be requested while the foreign instance holds a fresh claim", http.StatusInternalServerError)
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "pid-reuse.bin", URL: srv.URL + "/pid-reuse.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 0}
	_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {})
	if !errors.Is(err, errBlobIncompleteOwned) {
		t.Fatalf("error = %v, want errBlobIncompleteOwned: a live foreign owner (or a different instance token with our PID) must not be taken over", err)
	}
	if hold != nil {
		hold.release()
	}

	// Stale heartbeat + provably dead same-host owner and no ownership lock:
	// take-over must still recover the committed bytes rather than locking the
	// file forever.
	staleMeta := fmt.Sprintf(
		`{"pid":99999999,"instance":"different-process-instance","host":%q,"size":%d,"version":2,"repo":"o/r","path":"pid-reuse.bin","ranges":[{"start":0,"end":799,"committed":800,"done":true},{"start":800,"end":%d,"committed":0,"done":false}]}`,
		host, total, total-1)
	if err := os.WriteFile(staging+".meta", []byte(staleMeta), 0o644); err != nil {
		t.Fatal(err)
	}
	// Put the committed prefix bytes in place.
	out, err := os.OpenFile(staging, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.WriteAt(full[:800], 0); err != nil {
		out.Close()
		t.Fatal(err)
	}
	out.Close()
	past := time.Now().Add(-2 * DefaultStaleTimeout)
	for _, p := range []string{staging, staging + ".meta"} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}

	srv2, gotRanges := rangedTestServer(t, full)
	it.URL = srv2.URL + "/pid-reuse.bin"
	if _, err := downloadMultipart(context.Background(), srv2.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("take-over of a dead same-host owner failed: %v", err)
	}
	assertExactFile(t, dst, full)
	if len(*gotRanges) == 0 || (*gotRanges)[0] != "bytes=800-7999" {
		t.Errorf("take-over did not resume the committed prefix: %v", *gotRanges)
	}
}

// TestRangedMultipart_ResolvedCommitMismatchDiscardsStaging verifies S5: a
// moved branch / different resolved commit invalidates the record even when
// no blob SHA is available to distinguish the bytes.
func TestRangedMultipart_ResolvedCommitMismatchDiscardsStaging(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 8000
	full := rangedTestPayload(total)
	dst := filepath.Join(tmpDir, "commit.bin")
	staging := dst + ".incomplete"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	ranges := []RangeProgress{
		{Start: 0, End: 3999, Committed: 4000, Done: true},
		{Start: 4000, End: 7999},
	}
	seedRangedState(t, staging, "o/r", "commit.bin", total, ranges, full)
	seeded, err := readStagingMeta(staging + ".meta")
	if err != nil {
		t.Fatal(err)
	}
	seeded.Commit = "old-commit"
	if err := writeStagingMeta(staging+".meta", seeded); err != nil {
		t.Fatal(err)
	}

	srv, gotRanges := rangedTestServer(t, full)
	it := PlanItem{RelativePath: "commit.bin", URL: srv.URL + "/commit.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r", Revision: "main"}, cfg, it, dst, multipartOpts{Commit: "new-commit"}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}
	assertExactFile(t, dst, full)
	startedAtZero := false
	for _, rh := range *gotRanges {
		if strings.HasPrefix(rh, "bytes=0-") {
			startedAtZero = true
		}
	}
	if !startedAtZero {
		t.Errorf("commit mismatch resumed old bytes; requests=%v", *gotRanges)
	}
}

// TestRangedMultipart_HeartbeatCoversFinalization exercises the single
// heartbeat during a finalization-length pause. The download blocks in the
// file_finalizing event (emitted before the strict hash/publish work); the
// claim record must still be rewritten while it is blocked.
func TestRangedMultipart_HeartbeatCoversFinalization(t *testing.T) {
	oldInterval := stagingHeartbeatEvery
	stagingHeartbeatEvery = 20 * time.Millisecond
	defer func() { stagingHeartbeatEvery = oldInterval }()

	tmpDir := t.TempDir()
	const total = 8000
	full := rangedTestPayload(total)
	dst := filepath.Join(tmpDir, "heartbeat-finalize.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, _ := rangedTestServer(t, full)

	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	emit := func(ev ProgressEvent) {
		if ev.Event == "file_finalizing" {
			once.Do(func() { close(reached) })
			<-release
		}
	}

	done := make(chan error, 1)
	it := PlanItem{RelativePath: "heartbeat-finalize.bin", URL: srv.URL + "/heartbeat-finalize.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}
	go func() {
		_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, emit)
		done <- err
	}()

	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("download finished before finalization: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("download never reached the finalization phase")
	}

	metaPath := dst + ".incomplete.meta"
	before, err := os.Stat(metaPath)
	if err != nil {
		t.Fatalf("claim record missing during finalization: %v", err)
	}
	time.Sleep(4*stagingHeartbeatEvery + 30*time.Millisecond)
	during, err := os.Stat(metaPath)
	if err != nil {
		t.Fatalf("claim record disappeared during finalization: %v", err)
	}
	if !during.ModTime().After(before.ModTime()) {
		t.Errorf("claim record was not heartbeated during finalization: mtime stayed %v", before.ModTime())
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}
	assertExactFile(t, dst, full)
}

// TestRangedMultipart_HeartbeatFailureIsFatal forces the heartbeat's atomic
// meta rewrite to fail while the download is still active. The failure must
// be surfaced and the payload must not be published.
func TestRangedMultipart_HeartbeatFailureIsFatal(t *testing.T) {
	oldInterval := stagingHeartbeatEvery
	stagingHeartbeatEvery = 20 * time.Millisecond
	defer func() { stagingHeartbeatEvery = oldInterval }()

	tmpDir := t.TempDir()
	const total = 4000
	full := rangedTestPayload(total)
	dst := filepath.Join(tmpDir, "HeartbeatFailure.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, _ := rangedTestServer(t, full)

	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	emit := func(ev ProgressEvent) {
		if ev.Event == "file_finalizing" {
			once.Do(func() { close(reached) })
			<-release
		}
	}
	done := make(chan error, 1)
	it := PlanItem{RelativePath: "heartbeat-failure.bin", URL: srv.URL + "/heartbeat-failure.bin", Size: total, AcceptRanges: true}
	go func() {
		_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}, it, dst, multipartOpts{}, emit)
		done <- err
	}()

	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("download finished before finalization: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("download never reached the finalization phase")
	}
	// The atomic meta write stages through meta+".tmp"; a directory there
	// makes every heartbeat write fail.
	if err := os.Mkdir(dst+".incomplete.meta.tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4*stagingHeartbeatEvery + 50*time.Millisecond)
	close(release)

	err := <-done
	if err == nil {
		t.Fatal("download succeeded despite a heartbeat write failure")
	}
	if _, serr := os.Stat(dst); serr == nil {
		t.Error("payload was published after the claim heartbeat failed")
	}
}
