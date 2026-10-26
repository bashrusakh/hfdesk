// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Concurrency coverage for the shared state of a ranged download
// (issue #140).
//
// Two owner-level shared objects are covered here:
//
//  1. The staging record's range layout. The claim heartbeat rewrites the
//     record periodically and normalizes the layout in place while it holds
//     the download's metadata gate; the download itself derives its task list
//     and its committed baseline from the same layout. Every access to the
//     layout's mutable fields (RangeProgress.Committed/Done) must be
//     serialized by that gate, otherwise the heartbeat's normalization and
//     the task derivation race on the same bytes. The range layout's
//     Start/End bounds are immutable once the layout is installed, so only
//     Committed/Done need the gate.
//
//  2. The strict hash-on-write state. Every range worker inspects it on each
//     write and invalidates it when a response's framing cannot be trusted,
//     so parallel workers can reach the invalidation concurrently. The
//     invalidation, the hash's own bytes and the final digest must be taken
//     together under one lock, so no worker can invalidate a hash another
//     worker is still feeding and no digest can be summed from a hash that
//     was invalidated underneath it.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingRangeServer serves a fixed payload with correct HEAD/Range
// handling, but holds every ranged GET until release() is called. That keeps
// the download inside its post-claim phase while the test observes the claim
// record, so the heartbeat's liveness is established rather than assumed.
//
// The recorded Range headers are appended under mu and returned by requests().
func blockingRangeServer(t *testing.T, full []byte) (srv *httptest.Server, requests func() []string, release func()) {
	t.Helper()
	var (
		mu        sync.Mutex
		gotRanges []string
	)
	released := make(chan struct{})
	var releaseOnce sync.Once
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		mu.Lock()
		gotRanges = append(gotRanges, r.Header.Get("Range"))
		mu.Unlock()
		<-released
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(gotRanges)
	}, func() { releaseOnce.Do(func() { close(released) }) }
}

// recordRewriter observes the claim record at metaPath and reports whether
// it was ever replaced by a different file while it watched.
//
// The claim record is rewritten atomically (staged file plus rename), so a
// changed file identity proves a real rewrite happened. That is the
// liveness precondition of the race this file covers: without a live
// heartbeat the shared layout is never normalized concurrently and the
// race-detector result would be vacuous.
type recordRewriter struct {
	mu      sync.Mutex
	changes int
	stop    chan struct{}
	done    chan struct{}
}

// watchRecordRewrites starts observing the claim record at metaPath.
func watchRecordRewrites(metaPath string) *recordRewriter {
	rw := &recordRewriter{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(rw.done)
		var prev os.FileInfo
		for {
			select {
			case <-rw.stop:
				return
			default:
			}
			if fi, err := os.Stat(metaPath); err == nil {
				rw.mu.Lock()
				if prev != nil && !os.SameFile(prev, fi) {
					rw.changes++
				}
				prev = fi
				rw.mu.Unlock()
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	return rw
}

// stop ends the observation and returns the number of observed rewrites.
func (rw *recordRewriter) stopWatching() int {
	close(rw.stop)
	<-rw.done
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.changes
}

// TestRangedMultipart_ResumedTaskDerivationIsRaceFree drives a resumed
// download whose layout already carries Done ranges while the claim
// heartbeat runs at its shortest cadence, so the heartbeat's in-place
// normalization of the shared layout overlaps the derivation of the missing
// work.
//
// The download must (a) issue exactly the missing suffix requests, (b)
// produce byte-exact output, and (c) run without a data race between the
// heartbeat and the task derivation. Under -race the third property is
// enforced by the race detector itself.
//
// The layout variants place the Done ranges at different positions and use
// different missing-work shapes, so the derived task index and the requested
// byte ranges differ per variant: a fix that only handled one position, or
// one missing-work shape, would fail the others.
//
// The heartbeat interval is a package-level seam. It is restored only after
// every download returned; downloadMultipart joins its heartbeat goroutine
// before it returns, so the seam is never mutated while a heartbeat is
// alive.
func TestRangedMultipart_ResumedTaskDerivationIsRaceFree(t *testing.T) {
	oldInterval := stagingHeartbeatEvery
	stagingHeartbeatEvery = time.Nanosecond
	t.Cleanup(func() { stagingHeartbeatEvery = oldInterval })

	const total = 8000
	full := rangedTestPayload(total)
	const relPath = "resumed-race.bin"

	variants := []struct {
		name       string
		ranges     []RangeProgress
		wantRanges []string
	}{
		{
			// The Done range is first: the derived task is the second range
			// and the request starts at that range's own committed prefix.
			name: "done range first",
			ranges: []RangeProgress{
				{Start: 0, End: 3999, Committed: 4000, Done: true},
				{Start: 4000, End: 7999},
			},
			wantRanges: []string{"bytes=4000-7999"},
		},
		{
			// The Done range is last: the derived task is the first range,
			// so the request starts at offset 0 and the derived task index
			// differs from the first variant.
			name: "done range last",
			ranges: []RangeProgress{
				{Start: 0, End: 3999},
				{Start: 4000, End: 7999, Committed: 4000, Done: true},
			},
			wantRanges: []string{"bytes=0-3999"},
		},
		{
			// Done ranges on both ends with the missing work in the middle:
			// the derivation walks Done, incomplete, Done.
			name: "done ranges on both ends",
			ranges: []RangeProgress{
				{Start: 0, End: 1999, Committed: 2000, Done: true},
				{Start: 2000, End: 5999},
				{Start: 6000, End: 7999, Committed: 2000, Done: true},
			},
			wantRanges: []string{"bytes=2000-5999"},
		},
		{
			// A partially committed (not Done) range next to a Done range:
			// the request must resume at the committed prefix, and the Done
			// range must not be re-requested.
			name: "partial prefix next to done range",
			ranges: []RangeProgress{
				{Start: 0, End: 3999, Committed: 1000},
				{Start: 4000, End: 7999, Committed: 4000, Done: true},
			},
			wantRanges: []string{"bytes=1000-3999"},
		},
	}

	for _, v := range variants {
		for i := 0; i < 3; i++ {
			t.Run(v.name, func(t *testing.T) {
				tmpDir := t.TempDir()
				dst := filepath.Join(tmpDir, relPath)
				staging := dst + ".incomplete"
				metaPath := staging + ".meta"
				seedRangedState(t, staging, "o/r", relPath, total, v.ranges, full)

				srv, requests, release := blockingRangeServer(t, full)
				it := PlanItem{RelativePath: relPath, URL: srv.URL + "/" + relPath, Size: total, AcceptRanges: true}
				cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}

				done := make(chan error, 1)
				go func() {
					_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r", Revision: "main"}, cfg, it, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}, func(ProgressEvent) {})
					done <- err
				}()

				// The download is blocked in its ranged GET, so the claim
				// heartbeat is live. The record must be rewritten while it
				// watches: that proves the heartbeat's shared-layout
				// normalization really ran, so the race result below is not
				// vacuous.
				rw := watchRecordRewrites(metaPath)
				time.Sleep(50 * time.Millisecond)
				changes := rw.stopWatching()
				release()

				if err := <-done; err != nil {
					t.Fatalf("iteration %d: downloadMultipart: %v", i, err)
				}
				if changes == 0 {
					t.Fatalf("iteration %d: claim record was never rewritten while the download was active: "+
						"the heartbeat never ran, so this run would not exercise the concurrent normalization", i)
				}

				// Exact missing-range request oracle: only the uncommitted
				// suffix of the incomplete range is requested, and the
				// already-Done range is never re-requested.
				if got := requests(); !slices.Equal(got, v.wantRanges) {
					t.Errorf("iteration %d: ranged requests = %v, want %v", i, got, v.wantRanges)
				}
				assertExactFile(t, dst, full)
				if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
					t.Errorf("iteration %d: claim record survived publication (stat err = %v)", i, err)
				}
			})
		}
	}
}

// overflowingChunkedServer serves a payload whose ranged responses are
// chunked (no Content-Length) and one byte longer than the requested range.
// The bounded writer refuses the excess, so every ranged response is a
// protocol violation: the range is never completed and the bytes are never
// published.
//
// The first response for each distinct range is held until every worker has
// arrived, so the parallel workers are inside the transfer simultaneously
// and really overlap their invalidations. Later attempts (retries) are
// served immediately so the test cannot deadlock on the barrier.
func overflowingChunkedServer(t *testing.T, full []byte, workers int) (*httptest.Server, func() int) {
	t.Helper()
	var (
		mu      sync.Mutex
		hits    int
		seen    = make(map[string]bool)
		arrived = make(chan struct{})
	)
	var arriveOnce sync.Once
	var arriveMu sync.Mutex
	arrivedCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "unexpected range", http.StatusBadRequest)
			return
		}
		mu.Lock()
		hits++
		first := !seen[r.Header.Get("Range")]
		seen[r.Header.Get("Range")] = true
		mu.Unlock()
		if first {
			arriveMu.Lock()
			arrivedCount++
			if arrivedCount >= workers {
				arriveOnce.Do(func() { close(arrived) })
			}
			arriveMu.Unlock()
			// Bounded wait: the barrier must never be able to hang the
			// suite if a worker never arrives.
			select {
			case <-arrived:
			case <-time.After(2 * time.Second):
			}
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(full)))
		// No Content-Length: chunked framing, so the client reads every byte.
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(bytes.Repeat([]byte{0x7}, int(end-start+2)))
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return hits
	}
}

// TestRangedMultipart_ParallelOverflowInvalidationIsRaceFree drives two
// parallel range workers whose responses both overflow their assigned range.
//
// Each worker reaches the invalidation of the shared hash-on-write state
// ("d.seqHash = nil") because the response framing cannot be trusted. Those
// invalidations are concurrent writes to one shared field, so they must be
// synchronized: the download must (a) run without a data race, (b) reject
// both oversized bodies as protocol violations, (c) publish nothing, and
// (d) leave the staging record with a valid, non-Done layout so the next run
// resumes instead of trusting the partial bytes.
//
// The parallel case is what makes the invalidation shared: with one worker
// there is no second writer.
func TestRangedMultipart_ParallelOverflowInvalidationIsRaceFree(t *testing.T) {
	const total = 20_000
	full := rangedTestPayload(total)
	const relPath = "parallel-overflow.bin"

	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, relPath)
	staging := dst + ".incomplete"
	metaPath := staging + ".meta"

	const workers = 2
	srv, hits := overflowingChunkedServer(t, full, workers)
	it := PlanItem{RelativePath: relPath, URL: srv.URL + "/" + relPath, Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: workers, Retries: 1, StallTimeout: "0", BackoffInitial: "1ms", BackoffMax: "1ms", Verify: "size"}

	_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {})
	if err == nil {
		t.Fatal("oversized chunked ranged bodies were accepted and published")
	}
	if !strings.Contains(err.Error(), "exceeds assigned range") {
		t.Errorf("error = %v, want the bounded-writer protocol violation", err)
	}
	if n := hits(); n < workers {
		t.Errorf("ranged GET hits = %d, want at least %d (both workers must have reached the overflow path)", n, workers)
	}

	// Nothing may be published after a protocol violation.
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Errorf("destination was published after an oversized body (stat err = %v)", serr)
	}
	// The staging payload and its record are preserved for a later resume.
	if _, serr := os.Stat(staging); serr != nil {
		t.Errorf("staging payload was not preserved for resume: %v", serr)
	}
	meta, merr := readStagingMeta(metaPath)
	if merr != nil {
		t.Fatalf("staging record was not preserved: %v", merr)
	}
	// The persisted layout must not claim any range is complete: no range
	// ever delivered a clean, correctly framed response.
	for i, r := range meta.Ranges {
		if r.Done {
			t.Errorf("range %d was persisted as Done after an oversized body: %+v", i, r)
		}
	}
	// The record must still be a valid layout that the next run can resume
	// from: it tiles the file and claims no byte past a range end.
	if _, ok := normalizedRangeLayout(meta.Ranges, meta.Size); !ok {
		t.Errorf("persisted layout is not a valid resumable layout: %+v", meta.Ranges)
	}
}

// TestRangedMultipart_SequentialOverflowKeepsSingleFinalHash is the
// neighbouring single-worker control for the parallel invalidation case.
//
// One worker, one oversized chunked body, then a valid retry: the
// hash-on-write state must be invalidated exactly once, the retry must
// verify against the known digest, and the transfer must fall back to
// exactly one final full-file pass (never more, never none). This is the
// sequential meaning the parallel case must not weaken.
func TestRangedMultipart_SequentialOverflowKeepsSingleFinalHash(t *testing.T) {
	const total = 8000
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "sequential-overflow.bin")

	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(total))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		n := gets.Add(1)
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "unexpected range", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		if n == 1 {
			// Chunked body with one byte past the assigned range: the
			// framing cannot be trusted, so the hash-on-write state must
			// be invalidated.
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(bytes.Repeat([]byte{0xEE}, int(end-start+2)))
			return
		}
		// Valid retry of the same range.
		w.Header().Set("Content-Length", strconv.Itoa(int(end-start+1)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(full[start : end+1])
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "sequential-overflow.bin", URL: srv.URL + "/sequential-overflow.bin", Size: total, AcceptRanges: true, LFS: true, SHA256: sha}
	cfg := Settings{Concurrency: 1, Retries: 2, StallTimeout: "0", BackoffInitial: "1ms", BackoffMax: "1ms", Verify: "sha256"}

	hashPassCount.Store(0)
	verified, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("downloadMultipart: %v (a valid retry after an overflow must verify and publish)", err)
	}
	if !verified {
		t.Error("the final full-file verification pass was not reported as completed")
	}
	// The invalidated hash-on-write state must fall back to exactly one
	// final full-file pass: not zero (the poisoned hash would be used) and
	// not more than one.
	if got := hashPassCount.Load(); got != 1 {
		t.Errorf("strict mode after an overflow retry performed %d full-file passes; want exactly 1", got)
	}
	assertExactFile(t, dst, full)
	if got := gets.Load(); got != 2 {
		t.Errorf("ranged body fetched %d times; want 2 (overflow attempt + valid retry)", got)
	}
}
