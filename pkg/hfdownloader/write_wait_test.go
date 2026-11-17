// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestBoundedAtWriterMeasuresWriteWait pins the measurement primitive: the
// time spent inside WriteAt accumulates in the supplied counter, which is what
// the server classifies as local write wait.
func TestBoundedAtWriterMeasuresWriteWait(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "write-wait-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var wait atomic.Int64
	w := newBoundedAtWriter(f, 0, 4)
	w.writeWait = &wait
	if n, err := w.Write([]byte("abcd")); err != nil || n != 4 {
		t.Fatalf("write failed: n=%d err=%v", n, err)
	}
	if wait.Load() <= 0 {
		t.Fatalf("write wait = %d ns, want a positive measurement", wait.Load())
	}
}

// TestMultipartProgressCarriesCumulativeWriteWait runs a real ranged transfer
// and checks that every periodic file_progress carries a non-decreasing
// cumulative write-wait reading and that writes were measured at all.
func TestMultipartProgressCarriesCumulativeWriteWait(t *testing.T) {
	const total = 256 * 1024
	full := rangedTestPayload(total)
	const relPath = "write-wait.bin"

	dst := filepath.Join(t.TempDir(), relPath)
	srv, _ := rangedTestServer(t, full)
	it := PlanItem{RelativePath: relPath, URL: srv.URL + "/" + relPath, Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 4, Retries: 0, StallTimeout: "0"}

	var (
		mu     sync.Mutex
		events []ProgressEvent
	)
	emit := func(ev ProgressEvent) {
		if ev.Event != "file_progress" {
			return
		}
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r", Revision: "main"},
		cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), emit); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 {
		t.Fatal("no file_progress events were emitted")
	}
	var previous int64
	for i, ev := range events {
		if ev.WriteWaitNanos < previous {
			t.Fatalf("event %d: write wait went backwards: %d -> %d", i, previous, ev.WriteWaitNanos)
		}
		previous = ev.WriteWaitNanos
	}
	if previous <= 0 {
		t.Fatalf("final cumulative write wait = %d ns, want a positive measurement", previous)
	}
	if last := events[len(events)-1]; last.Downloaded != total {
		t.Fatalf("final progress = %d/%d, want full transfer", last.Downloaded, total)
	}
}

// TestSingleProgressCarriesWriteWait covers the single-request path's existing
// emission points: it has no ticker, so its write hook reports the same
// cumulative measurement.
func TestSingleProgressCarriesWriteWait(t *testing.T) {
	full := rangedTestPayload(64 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	}))
	t.Cleanup(srv.Close)
	const relPath = "single-write-wait.bin"
	dst := filepath.Join(t.TempDir(), relPath)

	var (
		mu     sync.Mutex
		events []ProgressEvent
	)
	emit := func(ev ProgressEvent) {
		if ev.Event != "file_progress" {
			return
		}
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}

	it := PlanItem{RelativePath: relPath, URL: srv.URL + "/" + relPath, Size: int64(len(full))}
	cfg := Settings{Concurrency: 1, Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r", Revision: "main"},
		cfg, it, dst, singleOpts{Commit: rangedSeedCommit, CommitPinned: true, Domain: acquireTestDomain(t, dst)}, emit); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 {
		t.Fatal("no file_progress events were emitted")
	}
	if events[len(events)-1].WriteWaitNanos <= 0 {
		t.Fatalf("write wait = %d ns, want a positive measurement", events[len(events)-1].WriteWaitNanos)
	}
}
