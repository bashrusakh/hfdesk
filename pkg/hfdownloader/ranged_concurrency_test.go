// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Exercises concurrent checkpoint normalization of the mutable range layout
// and invalidation of strict hash-on-write state across workers.

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

// blockingRangeServer holds ranged GETs until release, then serves valid responses.
// Recorded Range headers are protected by mu and returned by requests.
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

// Done ranges in different positions must produce private missing-work tasks
// that request exactly the uncommitted suffixes and produce byte-exact output.
func TestRangedMultipart_ResumedTaskDerivationIsRaceFree(t *testing.T) {
	const total = 8000
	full := rangedTestPayload(total)
	const relPath = "resumed-race.bin"

	variants := []struct {
		name       string
		ranges     []RangeProgress
		wantRanges []string
	}{
		{
			name: "done range first",
			ranges: []RangeProgress{
				{Start: 0, End: 3999, Committed: 4000, Done: true},
				{Start: 4000, End: 7999},
			},
			wantRanges: []string{"bytes=4000-7999"},
		},
		{
			name: "done range last",
			ranges: []RangeProgress{
				{Start: 0, End: 3999},
				{Start: 4000, End: 7999, Committed: 4000, Done: true},
			},
			wantRanges: []string{"bytes=0-3999"},
		},
		{
			name: "done ranges on both ends",
			ranges: []RangeProgress{
				{Start: 0, End: 1999, Committed: 2000, Done: true},
				{Start: 2000, End: 5999},
				{Start: 6000, End: 7999, Committed: 2000, Done: true},
			},
			wantRanges: []string{"bytes=2000-5999"},
		},
		{
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

				srv, requests := rangedTestServer(t, full)
				it := PlanItem{RelativePath: relPath, URL: srv.URL + "/" + relPath, Size: total, AcceptRanges: true}
				cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}

				done := make(chan error, 1)
				go func() {
					_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r", Revision: "main"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}), func(ProgressEvent) {})
					done <- err
				}()

				if err := <-done; err != nil {
					t.Fatalf("iteration %d: downloadMultipart: %v", i, err)
				}

				if got := *requests; !slices.Equal(got, v.wantRanges) {
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

// Concurrent worker completions normalize and persist the shared layout under
// the metadata gate. Run with -race; hold finalization to inspect all-Done
// persisted ranges and the committed total before publication removes the record.
func TestRangedMultipart_ConcurrentCheckpointNormalizationIsRaceFree(t *testing.T) {
	const total = 64 * 1024
	const workers = 8
	full := rangedTestPayload(total)
	const relPath = "checkpoint-race.bin"

	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, relPath)
	staging := dst + ".incomplete"
	metaPath := staging + ".meta"
	ranges := buildRangeLayout(total, workers)
	seedRangedState(t, staging, "o/r", relPath, total, ranges, full)

	srv, requests, releaseTransfer := blockingRangeServer(t, full)
	reachedFinalize := make(chan struct{})
	releaseFinalize := make(chan struct{})
	var reachedOnce, releaseOnce sync.Once
	releaseFinalization := func() { releaseOnce.Do(func() { close(releaseFinalize) }) }
	// Unblock the server and the finalization gate if an assertion exits the
	// test early: httptest.Server.Close waits for outstanding handlers.
	defer releaseTransfer()
	defer releaseFinalization()
	emit := func(ev ProgressEvent) {
		if ev.Event == "file_finalizing" {
			reachedOnce.Do(func() { close(reachedFinalize) })
			<-releaseFinalize
		}
	}

	it := PlanItem{RelativePath: relPath, URL: srv.URL + "/" + relPath, Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: workers, Retries: 0, StallTimeout: "0"}
	done := make(chan error, 1)
	go func() {
		_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r", Revision: "main"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}), emit)
		done <- err
	}()

	// Wait for every ranged GET before releasing responses to competing workers.
	deadline := time.Now().Add(10 * time.Second)
	for len(requests()) < workers {
		select {
		case err := <-done:
			t.Fatalf("download finished before all workers started: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d range workers started", len(requests()), workers)
		}
	}
	releaseTransfer()

	select {
	case <-reachedFinalize:
	case err := <-done:
		t.Fatalf("download finished before finalization: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("download never reached the finalization phase")
	}

	meta, merr := readStagingMeta(metaPath)
	if merr != nil {
		t.Fatalf("read persisted record before publication: %v", merr)
	}
	if !stagingLayoutComplete(meta.Ranges, total) {
		t.Fatalf("concurrent checkpoint flushes persisted an incomplete layout: %+v", meta.Ranges)
	}
	if got := committedTotal(meta.Ranges); got != total {
		t.Fatalf("persisted committed total = %d, want %d", got, total)
	}

	releaseFinalization()
	if err := <-done; err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}
	assertExactFile(t, dst, full)
	if _, serr := os.Stat(metaPath); !os.IsNotExist(serr) {
		t.Errorf("claim record survived publication (stat err = %v)", serr)
	}
	got := requests()
	if len(got) != workers {
		t.Fatalf("ranged requests = %d (%v), want %d (one per range)", len(got), got, workers)
	}
	for i := range ranges {
		want := fmt.Sprintf("bytes=%d-%d", ranges[i].Start, ranges[i].End)
		if !slices.Contains(got, want) {
			t.Errorf("missing ranged request %q; got %v", want, got)
		}
	}
}

// overflowingChunkedServer sends chunked ranges one byte too long. First
// requests wait at a worker barrier; retries bypass it so they cannot deadlock.
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
		// Omit Content-Length so the excess reaches the body's framing check.
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

// Parallel oversized ranges exercise shared hash-state invalidation under -race.
// They must fail without publication and retain a valid, non-Done staging layout.
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

	_, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ProgressEvent) {})
	if err == nil {
		t.Fatal("oversized chunked ranged bodies were accepted and published")
	}
	if !strings.Contains(err.Error(), "exceeds assigned range") {
		t.Errorf("error = %v, want the bounded-writer protocol violation", err)
	}
	if n := hits(); n < workers {
		t.Errorf("ranged GET hits = %d, want at least %d (both workers must have reached the overflow path)", n, workers)
	}

	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Errorf("destination was published after an oversized body (stat err = %v)", serr)
	}
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
	// The preserved layout must remain structurally valid: tile the file and claim no bytes past a range end.
	if _, ok := normalizedRangeLayout(meta.Ranges, meta.Size); !ok {
		t.Errorf("persisted layout is not a valid resumable layout: %+v", meta.Ranges)
	}
}

// Sequential control: after an oversized response, a valid retry must drop
// hash-on-write state and verify with exactly one final full-file pass.
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
		w.Header().Set("Content-Length", strconv.Itoa(int(end-start+1)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(full[start : end+1])
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "sequential-overflow.bin", URL: srv.URL + "/sequential-overflow.bin", Size: total, AcceptRanges: true, LFS: true, SHA256: sha}
	cfg := Settings{Concurrency: 1, Retries: 2, StallTimeout: "0", BackoffInitial: "1ms", BackoffMax: "1ms", Verify: "sha256"}

	hashPassCount.Store(0)
	verified, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("downloadMultipart: %v (a valid retry after an overflow must verify and publish)", err)
	}
	if !verified {
		t.Error("the final full-file verification pass was not reported as completed")
	}
	// Overflow invalidates hash-on-write, requiring exactly one final full-file pass.
	if got := hashPassCount.Load(); got != 1 {
		t.Errorf("strict mode after an overflow retry performed %d full-file passes; want exactly 1", got)
	}
	assertExactFile(t, dst, full)
	if got := gets.Load(); got != 2 {
		t.Errorf("ranged body fetched %d times; want 2 (overflow attempt + valid retry)", got)
	}
}
