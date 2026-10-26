// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the shared staging-file ownership/resume state
// machine (issue #140 correction set). Each blocker test is written to fail
// against the pre-correction implementation:
//
//   - a body error after the final requested byte must leave a nonempty
//     revalidation suffix, never publish an unframed response or request an
//     impossible bytes=End+1-End range;
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

// TestDownload_OwnedClaimIsNotCountedAsSkipped verifies that a live owner's
// claim is NOT counted as a skip.
//
// The file is not present while another writer holds the item's staging, so
// the item must fail with the actionable live-owner error instead of being
// counted in the "skipped N" summary: a job that reports success must mean
// the requested files are present. The live owner is simulated by holding the
// OS ownership lock, which is what a real writer holds for its whole
// interval.
func TestDownload_OwnedClaimIsNotCountedAsSkipped(t *testing.T) {
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
	var skips []string
	err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ev ProgressEvent) {
		if ev.Event == "done" {
			doneMessage = ev.Message
		}
		if ev.Event == "file_done" && strings.Contains(ev.Message, "skip") {
			skips = append(skips, ev.Message)
		}
	})
	if err == nil {
		t.Fatalf("Download reported success although a live writer owns the item (done=%q)", doneMessage)
	}
	if !errors.Is(err, errBlobOwnedByLiveWriter) {
		t.Errorf("Download error = %v, want the actionable live-owner error", err)
	}
	if len(skips) != 0 {
		t.Errorf("the live-owned item was counted as a skip: %v", skips)
	}
	if strings.Contains(doneMessage, "skipped 1") {
		t.Errorf("the live-owned item was counted in the summary: done message = %q", doneMessage)
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

// TestRangedMultipart_FinalChunkFailureRequiresFramingRevalidation cancels
// after every requested byte is written but before the chunked response ends.
// The unframed last byte stays uncommitted; a restart requests that byte and
// only then publishes after a clean response.
func TestRangedMultipart_FinalChunkFailureRequiresFramingRevalidation(t *testing.T) {
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
		_, err := downloadMultipart(ctx, srv.Client(), "", Job{Repo: "o/r"}, firstCfg, it, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}, emit)
		done <- err
	}()
	select {
	case <-served:
	case err := <-done:
		t.Fatalf("first run finished before the body was served: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first run never served the body")
	}
	// The progress counter reaches Total after every byte was written. Cancel
	// while the chunked response is still open: bytes alone are not framing EOF.
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
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("unframed response was published before recovery: %v", err)
	}

	meta, err := readStagingMeta(staging + ".meta")
	if err != nil {
		t.Fatalf("staging metadata missing after the cancelled run: %v", err)
	}
	if len(meta.Ranges) != 1 {
		t.Fatalf("unexpected range layout: %+v", meta.Ranges)
	}
	if meta.Ranges[0].Done || meta.Ranges[0].Committed != total-1 {
		t.Fatalf("unframed final byte was committed: %+v (want Done=false, Committed=%d)", meta.Ranges[0], total-1)
	}

	recoveryCfg := Settings{Concurrency: 1, Retries: 0, StallTimeout: "0"}
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, recoveryCfg, it, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("recovery run failed after the normalized checkpoint: %v", err)
	}
	assertExactFile(t, dst, full)

	mu.Lock()
	defer mu.Unlock()
	if len(rangeHdrs) != 2 || rangeHdrs[1] != fmt.Sprintf("bytes=%d-%d", total-1, total-1) {
		t.Fatalf("recovery should re-request only the unframed final byte, ranges=%v", rangeHdrs)
	}
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

// TestRangedMultipart_FinalChunkFailureRetriesLastByteUntilFramed covers the
// retry variant: all bytes arrive, but the first chunked response remains open
// past the stall deadline. The retry must request the final byte, not publish
// the unframed attempt or plan an empty range.
func TestRangedMultipart_FinalChunkFailureRetriesLastByteUntilFramed(t *testing.T) {
	tmpDir := t.TempDir()
	const total = 8000
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
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
			_, _ = w.Write([]byte("late overflow after stall deadline"))
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "final-chunk-retry.bin", URL: srv.URL + "/final-chunk-retry.bin", Size: total, AcceptRanges: true, LFS: true, SHA256: sha}
	cfg := Settings{Concurrency: 1, Retries: 2, StallTimeout: "80ms", BackoffInitial: "1ms", BackoffMax: "1ms", Verify: "sha256"}
	var progress []int64
	hashPassCount.Store(0)
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ev ProgressEvent) {
		if ev.Event == "file_progress" {
			progress = append(progress, ev.Downloaded)
		}
	}); err != nil {
		t.Fatalf("downloadMultipart did not complete after a clean final-byte retry: %v", err)
	}
	assertExactFile(t, dst, full)
	if got := hashPassCount.Load(); got != 1 {
		t.Errorf("strict retry performed %d full-file passes, want one after invalidating hash-on-write", got)
	}
	for i, n := range progress {
		if n > total || (i > 0 && n < progress[i-1]) {
			t.Errorf("progress was not monotonic unique bytes: %v", progress)
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(rangeHdrs) != 2 || rangeHdrs[1] != fmt.Sprintf("bytes=%d-%d", total-1, total-1) {
		t.Errorf("unframed completion did not retry the last byte: ranges=%v", rangeHdrs)
	}
}

// TestRangedMultipart_V2UnframedCompletePrefixRevalidatesLastByte seeds the
// version-2 Done state produced when all bytes were written but response
// framing failed. Migration keeps the prefix and re-fetches the final byte
// before publication.
func TestRangedMultipart_V2UnframedCompletePrefixRevalidatesLastByte(t *testing.T) {
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
	// The record states a RESOLVED, PINNED commit, so the fixture exercises the
	// version-2 LAYOUT migration (the final byte is revalidated) and not the
	// pinned-identity gate.
	raw := fmt.Sprintf(
		`{"pid":99999999,"instance":"foreign-dead","host":%q,"size":%d,"version":2,"repo":"o/r","path":"complete-prefix.bin","commit":%q,"commit_pinned":true,"ranges":[{"start":0,"end":%d,"committed":%d,"done":true}]}`,
		host, total, rangedSeedCommit, total-1, total)
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
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}, it, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}
	assertExactFile(t, dst, full)
	if len(*gotRanges) != 1 || (*gotRanges)[0] != fmt.Sprintf("bytes=%d-%d", total-1, total-1) {
		t.Errorf("unframed complete-prefix record did not revalidate exactly its final byte; requests=%v", *gotRanges)
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
			// (which ignores instance/host) for falsification. The record
			// states a RESOLVED, PINNED commit, so the only reason it may be
			// discarded is the payload check this test exercises.
			raw := fmt.Sprintf(
				`{"pid":99999999,"instance":"foreign-dead","host":%q,"size":%d,"version":2,"repo":"o/r","path":"ghost.bin","commit":%q,"commit_pinned":true,"ranges":[{"start":0,"end":%d,"committed":%d,"done":true}]}`,
				host, total, rangedSeedCommit, total-1, total)
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
			if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}, func(ProgressEvent) {}); err != nil {
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
	// take-over resumes its validated prefix, while the version-2 completed
	// range revalidates its final byte under the newer framing contract. The
	// record states a RESOLVED, PINNED commit, so the fixture exercises the
	// take-over and not the pinned-identity gate.
	staleMeta := fmt.Sprintf(
		`{"pid":99999999,"instance":"different-process-instance","host":%q,"size":%d,"version":2,"repo":"o/r","path":"pid-reuse.bin","commit":%q,"commit_pinned":true,"ranges":[{"start":0,"end":799,"committed":800,"done":true},{"start":800,"end":%d,"committed":0,"done":false}]}`,
		host, total, rangedSeedCommit, total-1)
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
	if _, err := downloadMultipart(context.Background(), srv2.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}, it, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("take-over of a dead same-host owner failed: %v", err)
	}
	assertExactFile(t, dst, full)
	wantRanges := map[string]bool{"bytes=799-799": false, "bytes=800-7999": false}
	for _, rh := range *gotRanges {
		if _, ok := wantRanges[rh]; !ok {
			t.Errorf("unexpected take-over range %q (all ranges=%v)", rh, *gotRanges)
			continue
		}
		wantRanges[rh] = true
	}
	if len(*gotRanges) != len(wantRanges) {
		t.Errorf("take-over ranges=%v, want prefix revalidation plus missing suffix", *gotRanges)
	}
	for rh, seen := range wantRanges {
		if !seen {
			t.Errorf("take-over did not fetch %s; ranges=%v", rh, *gotRanges)
		}
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
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r", Revision: "main"}, cfg, it, dst, multipartOpts{Commit: "new-commit", CommitPinned: true}, func(ProgressEvent) {}); err != nil {
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

func TestRangedMultipart_CancelWhilePublicationGateHeld(t *testing.T) {
	const total = 32_000
	full := rangedTestPayload(total)
	dir := t.TempDir()
	dst := filepath.Join(dir, "cancel-gate.bin")
	staging := dst + ".incomplete"
	metaPath := staging + ".meta"
	srv, _ := rangedTestServer(t, full)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	held := make(chan struct{})
	var gate *stagingStateLock
	result := make(chan error, 1)
	go func() {
		_, err := downloadMultipart(ctx, srv.Client(), "", Job{Repo: "o/r"},
			Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "none"},
			PlanItem{RelativePath: "cancel-gate.bin", URL: srv.URL + "/cancel-gate.bin", Size: total, AcceptRanges: true}, dst, multipartOpts{},
			func(ev ProgressEvent) {
				if ev.Event != "file_finalizing" || gate != nil {
					return
				}
				gate = &stagingStateLock{path: stagingLockPath(metaPath)}
				if err := gate.acquire(context.Background(), time.Second); err != nil {
					result <- err
					return
				}
				close(held)
			})
		result <- err
	}()
	select {
	case <-held:
	case err := <-result:
		t.Fatalf("download returned before finalization gate was held: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("download did not reach finalization")
	}
	// Wait until publish has registered as a waiter behind the held gate. This
	// observes the publication serialization attempt, rather than relying on a
	// timing delay after file_finalizing.
	deadline := time.Now().Add(time.Second)
	for {
		stagingStateGates.Lock()
		refs := stagingStateGates.m[gate.path].refs
		stagingStateGates.Unlock()
		if refs >= 2 {
			break
		}
		if time.Now().After(deadline) {
			gate.release()
			t.Fatal("publish never queued behind the held metadata gate")
		}
		time.Sleep(time.Millisecond)
	}
	// Cancellation must interrupt that acquisition even though the gate stays
	// held.
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("download error=%v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		gate.release()
		t.Fatal("publication wait did not observe cancellation while gate remained held")
	}
	gate.release()
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("destination published after cancellation: %v", err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("pause/cancel should preserve resumable staging: %v", err)
	}
}

func TestStagingPublishAlreadyCanceledContextCannotPublish(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "already-canceled.incomplete")
	dst := filepath.Join(dir, "already-canceled.bin")
	metaPath := staging + ".meta"
	if err := os.WriteFile(staging, []byte("complete"), 0o644); err != nil {
		t.Fatal(err)
	}
	it := PlanItem{RelativePath: "already-canceled.bin", Size: 8}
	meta := newClaimMeta(it, Job{Repo: "o/r"}, "commit", true)
	meta.LockFenced = true
	meta.Ranges = buildRangeLayout(8, 1)
	meta.Ranges[0].Committed, meta.Ranges[0].Done = 8, true
	if err := writeStagingMeta(metaPath, &meta); err != nil {
		t.Fatal(err)
	}
	own, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	defer own.release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &stagingDownload{ctx: ctx, stg: staging, dst: dst, pub: dst, meta: metaPath, it: it, job: Job{Repo: "o/r"}, own: own, osExclusive: true}
	if err := d.publish(); !errors.Is(err, context.Canceled) {
		t.Fatalf("publish error=%v, want context canceled", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("already-canceled publish created destination: %v", err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("staging was not preserved: %v", err)
	}
}
