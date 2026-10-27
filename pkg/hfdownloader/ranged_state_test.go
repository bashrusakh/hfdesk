// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

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

// A held destination lock with no published blob must time out as a live-owner
// error, not contribute a successful skip.
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
	// Preallocate payload and seed an unreadable record while holding its domain.
	staging := filepath.Join(blobsDir, sha+".incomplete")
	if err := os.WriteFile(staging, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging+".meta", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	shortBlobOwnershipWait(t, 250*time.Millisecond)
	hold, _ := acquireStagingOwnershipLock(filepath.Join(blobsDir, "tmp-"+sha))
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

// Zero Concurrency must still transfer queued ranges before publication.
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
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 0, Retries: 0, StallTimeout: "0"}, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ProgressEvent) {}); err != nil {
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
		_, err := downloadMultipart(ctx, srv.Client(), "", Job{Repo: "o/r"}, firstCfg, it, dst, withTestDomain(t, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}), emit)
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
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, recoveryCfg, it, dst, withTestDomain(t, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}), func(ProgressEvent) {}); err != nil {
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
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ev ProgressEvent) {
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

// TestRangedMultipart_MissingOrWrongPayloadInvalidatesRecord ensures a complete
// record cannot authorize resume when its payload is missing or wrong-sized.
func TestRangedMultipart_MissingOrWrongPayloadInvalidatesRecord(t *testing.T) {
	const total = 8000
	full := rangedTestPayload(total)
	host, _ := os.Hostname()

	cases := []struct {
		name    string
		payload func(t *testing.T, staging string)
	}{
		{"payload deleted", func(t *testing.T, staging string) {
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

			// Seed a complete, pinned record from a dead owner so payload
			// validity is the only condition under test.
			raw := fmt.Sprintf(
				`{"pid":99999999,"instance":"foreign-dead","host":%q,"size":%d,"version":4,"repo":"o/r","path":"ghost.bin","commit":%q,"commit_pinned":true,"ranges":[{"start":0,"end":%d,"committed":%d,"done":true}]}`,
				host, total, rangedSeedCommit, total-1, total)
			if err := os.WriteFile(staging+".meta", []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}

			srv, gotRanges := rangedTestServer(t, full)
			it := PlanItem{RelativePath: "ghost.bin", URL: srv.URL + "/ghost.bin", Size: total, AcceptRanges: true}
			cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}
			if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}), func(ProgressEvent) {}); err != nil {
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

// A foreign instance token with our PID is not self: an unfenced record whose
// owner is not provably dead must be refused even under a free domain.
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
		`{"pid":%d,"instance":"different-process-instance","host":%q,"size":%d,"version":4,"repo":"o/r","path":"pid-reuse.bin","ranges":[{"start":0,"end":%d,"committed":0,"done":false}]}`,
		os.Getpid(), host, total, total-1)
	if err := os.WriteFile(staging+".meta", []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	// The held domain does not prove an unfenced owner dead; preserve this record.
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
	_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ProgressEvent) {})
	if !errors.Is(err, errBlobIncompleteOwned) || !errors.Is(err, ErrStagingOwnershipUnproven) {
		t.Fatalf("error = %v, want the unproven-ownership refusal: a different instance token with our PID must not be taken over", err)
	}

	// A dead same-host owner with matching pinned v4 identity may resume its prefix.
	staleMeta := fmt.Sprintf(
		`{"pid":99999999,"instance":"different-process-instance","host":%q,"size":%d,"version":4,"repo":"o/r","path":"pid-reuse.bin","commit":%q,"commit_pinned":true,"ranges":[{"start":0,"end":799,"committed":800,"done":true},{"start":800,"end":%d,"committed":0,"done":false}]}`,
		host, total, rangedSeedCommit, total-1)
	if err := os.WriteFile(staging+".meta", []byte(staleMeta), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(staging, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.WriteAt(full[:800], 0); err != nil {
		out.Close()
		t.Fatal(err)
	}
	out.Close()

	srv2, gotRanges := rangedTestServer(t, full)
	it.URL = srv2.URL + "/pid-reuse.bin"
	if _, err := downloadMultipart(context.Background(), srv2.Client(), "", Job{Repo: "o/r"}, Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}, it, dst, withTestDomain(t, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}), func(ProgressEvent) {}); err != nil {
		t.Fatalf("take-over of a dead same-host owner failed: %v", err)
	}
	assertExactFile(t, dst, full)
	// The Done prefix is clean-framed; request only bytes 800-7999.
	wantRanges := map[string]bool{"bytes=800-7999": false}
	for _, rh := range *gotRanges {
		if _, ok := wantRanges[rh]; !ok {
			t.Errorf("unexpected take-over range %q (all ranges=%v)", rh, *gotRanges)
			continue
		}
		wantRanges[rh] = true
	}
	if len(*gotRanges) != len(wantRanges) {
		t.Errorf("take-over ranges=%v, want only the missing suffix", *gotRanges)
	}
	for rh, seen := range wantRanges {
		if !seen {
			t.Errorf("take-over did not fetch %s; ranges=%v", rh, *gotRanges)
		}
	}
}

// A different resolved commit invalidates the record even without a blob SHA.
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
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r", Revision: "main"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{Commit: "new-commit", CommitPinned: true}), func(ProgressEvent) {}); err != nil {
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
			PlanItem{RelativePath: "cancel-gate.bin", URL: srv.URL + "/cancel-gate.bin", Size: total, AcceptRanges: true}, dst, withTestDomain(t, dst, multipartOpts{}),
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
	domain := acquireTestDomain(t, dst)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &stagingDownload{ctx: ctx, stg: staging, dst: dst, pub: dst, meta: metaPath, it: it, job: Job{Repo: "o/r"}, domain: domain}
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

// cancelAfterEarlyGuardContext cancels its inner context the first time Err is
// consulted and reports nil for that first consultation. It models a
// cancellation that arrives after publish's early ownership/cancellation guard
// but before placement, so only the final pre-placement check can observe it.
type cancelAfterEarlyGuardContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelAfterEarlyGuardContext) Err() error {
	c.checks++
	if c.checks == 1 {
		c.cancel()
		return nil
	}
	return c.Context.Err()
}

// Cancellation after the early guard must prevent placement and preserve
// both payload and record for resume.
func TestStagingPublishCancellationAfterEarlyGuardStopsBeforePlacement(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "late-cancel.incomplete")
	dst := filepath.Join(dir, "late-cancel.bin")
	metaPath := staging + ".meta"
	if err := os.WriteFile(staging, []byte("complete"), 0o644); err != nil {
		t.Fatal(err)
	}
	it := PlanItem{RelativePath: "late-cancel.bin", Size: 8}
	meta := newClaimMeta(it, Job{Repo: "o/r"}, "commit", true)
	meta.LockFenced = true
	meta.Ranges = buildRangeLayout(8, 1)
	meta.Ranges[0].Committed, meta.Ranges[0].Done = 8, true
	if err := writeStagingMeta(metaPath, &meta); err != nil {
		t.Fatal(err)
	}
	domain := acquireTestDomain(t, dst)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelAfterEarlyGuardContext{Context: base, cancel: cancel}
	d := &stagingDownload{ctx: ctx, stg: staging, dst: dst, pub: dst, meta: metaPath, it: it, job: Job{Repo: "o/r"}, domain: domain, ranges: meta.Ranges}
	if err := d.publish(); !errors.Is(err, context.Canceled) {
		t.Fatalf("publish error = %v, want context canceled by the final pre-placement guard", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("destination published after the late cancellation: %v", err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("the staged payload was not preserved for a resume: %v", err)
	}
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("the staged record was not preserved for a resume: %v", err)
	}
}

// A version-3 record with legacy_migrated is discarded under the claim; the
// file restarts from zero and publishes the exact current content.
func TestRangedMultipart_V3LegacyMigratedRecordIsDiscardedUnderV4(t *testing.T) {
	const total = 8000
	full := rangedTestPayload(total)
	dst := filepath.Join(t.TempDir(), "v3-old.bin")
	staging := dst + ".incomplete"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	// A complete-looking version-3 layout over the WRONG bytes, with the
	// branch-only legacy_migrated marker set.
	if err := os.WriteFile(staging, bytes.Repeat([]byte{0xFF}, total), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(
		`{"pid":%d,"instance":%q,"host":%q,"size":%d,"version":3,"repo":"o/r","path":"v3-old.bin","commit":%q,"commit_pinned":true,"lock_fenced":true,"ranges":[{"start":0,"end":%d,"committed":%d,"done":true}],"legacy_migrated":true}`,
		os.Getpid(), processInstanceToken, processHostname, total, rangedSeedCommit, total-1, total)
	if err := os.WriteFile(staging+".meta", []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, gotRanges := rangedTestServer(t, full)
	it := PlanItem{RelativePath: "v3-old.bin", URL: srv.URL + "/v3-old.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 1, Retries: 0, StallTimeout: "0", Verify: "size"}
	hashPassCount.Store(0)
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst,
		withTestDomain(t, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}), func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}
	assertExactFile(t, dst, full)
	if got := *gotRanges; len(got) != 1 || got[0] != fmt.Sprintf("bytes=0-%d", total-1) {
		t.Errorf("payload GETs = %v, want one whole-file request (the v3 record must be discarded)", got)
	}
	if got := hashPassCount.Load(); got != 0 {
		t.Errorf("the discarded v3 record triggered %d content proofs; want 0", got)
	}
}

// Only the current record version is eligible for resume.
func TestStagingMetaResumableAcceptsOnlyTheCurrentVersion(t *testing.T) {
	const size = 100
	it := PlanItem{RelativePath: "x.bin", Size: size}
	job := Job{Repo: "o/r"}
	complete := []RangeProgress{{Start: 0, End: size - 1, Committed: size, Done: true}}
	for _, version := range []int{0, 2, 3, 5, 99} {
		meta := IncompleteMeta{
			Version: version, Repo: "o/r", Path: "x.bin", Size: size,
			Commit: "c1", CommitPinned: true, Ranges: complete,
		}
		if _, ok := stagingMetaResumable(meta, it, job, "c1", true); ok {
			t.Errorf("version %d record was accepted as resume state", version)
		}
	}
	meta := IncompleteMeta{
		Version: stagingMetaVersion, Repo: "o/r", Path: "x.bin", Size: size,
		Commit: "c1", CommitPinned: true, Ranges: complete,
	}
	ranges, ok := stagingMetaResumable(meta, it, job, "c1", true)
	if !ok || len(ranges) != 1 || !ranges[0].Done {
		t.Fatalf("current-version record = (%+v, %v), want a resumable complete range", ranges, ok)
	}
}

// The current format still resumes its committed prefix.
func TestRangedMultipart_CurrentFormatV4RecordResumesCommittedPrefix(t *testing.T) {
	const total = 8000
	full := rangedTestPayload(total)
	dst := filepath.Join(t.TempDir(), "v4.bin")
	staging := dst + ".incomplete"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	seedRangedState(t, staging, "o/r", "v4.bin", total, []RangeProgress{
		{Start: 0, End: 3999, Committed: 4000, Done: true},
		{Start: 4000, End: 7999},
	}, full)

	srv, gotRanges := rangedTestServer(t, full)
	it := PlanItem{RelativePath: "v4.bin", URL: srv.URL + "/v4.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 1, Retries: 0, StallTimeout: "0", Verify: "size"}
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst,
		withTestDomain(t, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}), func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}
	assertExactFile(t, dst, full)
	if got := *gotRanges; len(got) != 1 || got[0] != "bytes=4000-7999" {
		t.Errorf("payload GETs = %v, want only the missing suffix bytes=4000-7999", got)
	}
}
