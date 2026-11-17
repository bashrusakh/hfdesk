// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// blockingReader delivers its data only after release, so a test can observe
// that no read activity is recorded before bytes actually arrive.
type blockingReader struct {
	data    []byte
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingReader(data []byte) *blockingReader {
	return &blockingReader{data: data, started: make(chan struct{}), release: make(chan struct{})}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// TestActivityReaderNotesOnlyDeliveredBytes pins the read signal: the timestamp
// stays zero while a read is blocked and advances only when bytes arrive.
func TestActivityReaderNotesOnlyDeliveredBytes(t *testing.T) {
	var activity fileActivity
	reader := newBlockingReader([]byte("abcd"))
	noted := activityReader{r: reader, a: &activity}

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 4)
		_, err := io.ReadFull(noted, buf)
		done <- err
	}()

	<-reader.started
	if readAt, writeAt := activity.snapshot(); !readAt.IsZero() || !writeAt.IsZero() {
		t.Fatalf("activity recorded before bytes arrived: read=%v write=%v", readAt, writeAt)
	}

	close(reader.release)
	if err := <-done; err != nil {
		t.Fatalf("read: %v", err)
	}
	readAt, writeAt := activity.snapshot()
	if readAt.IsZero() {
		t.Fatal("read activity was not recorded after bytes arrived")
	}
	if !writeAt.IsZero() {
		t.Fatalf("write activity = %v, want none for a pure read", writeAt)
	}
	if age := time.Since(readAt); age > time.Second {
		t.Fatalf("read activity age = %v, want a recent instant", age)
	}
}

// TestBoundedAtWriterNotesWriteOnlyOnCompletion pins the write signal: a
// blocked WriteAt leaves the previous instant untouched, and completion
// advances it exactly once.
func TestBoundedAtWriterNotesWriteOnlyOnCompletion(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "activity-write-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var activity fileActivity
	started := make(chan struct{})
	release := make(chan struct{})
	w := newBoundedAtWriter(f, 0, 4)
	w.activity = &activity
	w.writeAt = func(p []byte, off int64) (int, error) {
		close(started)
		<-release
		return f.WriteAt(p, off)
	}

	done := make(chan error, 1)
	go func() {
		_, err := w.Write([]byte("abcd"))
		done <- err
	}()

	<-started
	if _, writeAt := activity.snapshot(); !writeAt.IsZero() {
		t.Fatalf("write activity recorded while WriteAt is still blocked: %v", writeAt)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("write: %v", err)
	}
	_, writeAt := activity.snapshot()
	if writeAt.IsZero() {
		t.Fatal("write activity was not recorded after completion")
	}
}

// TestMultipartPrepProgressReportsLocalActivity covers the user's creation
// case: a slow preallocation publishes activity-only progress events with a
// fresh local-write instant and no reads before the first transfer byte.
func TestMultipartPrepProgressReportsLocalActivity(t *testing.T) {
	oldTruncate := truncateStagingPayload
	truncateStagingPayload = func(f *os.File, size int64) error {
		time.Sleep(600 * time.Millisecond)
		return f.Truncate(size)
	}
	t.Cleanup(func() { truncateStagingPayload = oldTruncate })

	const total = 64 * 1024
	full := rangedTestPayload(total)
	const relPath = "prep-activity.bin"
	dst := filepath.Join(t.TempDir(), relPath)
	srv, _ := rangedTestServer(t, full)
	it := PlanItem{RelativePath: relPath, URL: srv.URL + "/" + relPath, Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}

	type sample struct {
		evt        ProgressEvent
		receivedAt time.Time
	}
	var (
		mu      sync.Mutex
		samples []sample
	)
	emit := func(ev ProgressEvent) {
		if ev.Event != "file_progress" {
			return
		}
		mu.Lock()
		samples = append(samples, sample{evt: ev, receivedAt: time.Now()})
		mu.Unlock()
	}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r", Revision: "main"},
		cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), emit); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	prepSamples := 0
	for _, s := range samples {
		if !s.evt.ActivityOnly {
			continue
		}
		prepSamples++
		if s.evt.Downloaded != 0 {
			t.Errorf("preparation sample reported %d transferred bytes, want 0", s.evt.Downloaded)
		}
		if !s.evt.LastReadAt.IsZero() {
			t.Errorf("preparation sample reported a read at %v", s.evt.LastReadAt)
		}
		if s.evt.LastLocalWriteAt.IsZero() {
			t.Errorf("preparation sample has no local-write activity")
		} else if age := s.receivedAt.Sub(s.evt.LastLocalWriteAt); age > time.Second {
			t.Errorf("preparation local-write age = %v, want a recent instant", age)
		}
	}
	if prepSamples < 2 {
		t.Fatalf("preparation activity samples = %d, want at least two during the 600 ms truncate", prepSamples)
	}

	last := samples[len(samples)-1]
	if last.evt.ActivityOnly {
		t.Error("the final progress sample is still marked activity-only")
	}
	if last.evt.LastReadAt.IsZero() {
		t.Error("the final progress sample has no read activity")
	}
	if last.evt.LastLocalWriteAt.IsZero() {
		t.Error("the final progress sample has no local-write activity")
	}
}

// TestMultipartClaimWaitIsNotLocalActivity checks that waiting on another
// writer's metadata gate does not advance the local-write instant, while the
// prep ticker still emits activity-only samples.
func TestMultipartClaimWaitIsNotLocalActivity(t *testing.T) {
	const total = 64 * 1024
	full := rangedTestPayload(total)
	const relPath = "claim-wait.bin"
	dst := filepath.Join(t.TempDir(), relPath)
	staging := dst + ".incomplete"
	metaPath := staging + ".meta"

	gate := &stagingStateLock{path: stagingLockPath(metaPath)}
	if err := gate.acquire(context.Background(), time.Second); err != nil {
		t.Fatalf("hold metadata gate: %v", err)
	}
	releaseAt := make(chan time.Time, 1)
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(700 * time.Millisecond)
		releaseAt <- time.Now()
		gate.release()
	}()
	t.Cleanup(func() { <-released })

	srv, _ := rangedTestServer(t, full)
	it := PlanItem{RelativePath: relPath, URL: srv.URL + "/" + relPath, Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}

	type sample struct {
		evt        ProgressEvent
		receivedAt time.Time
	}
	var (
		mu      sync.Mutex
		samples []sample
	)
	emit := func(ev ProgressEvent) {
		if ev.Event != "file_progress" {
			return
		}
		mu.Lock()
		samples = append(samples, sample{evt: ev, receivedAt: time.Now()})
		mu.Unlock()
	}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r", Revision: "main"},
		cfg, it, dst, withTestDomain(t, dst, multipartOpts{}), emit); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}
	releasedAt := <-releaseAt

	mu.Lock()
	defer mu.Unlock()
	duringWait := 0
	for _, s := range samples {
		if s.receivedAt.After(releasedAt) {
			continue
		}
		duringWait++
		if !s.evt.ActivityOnly {
			t.Errorf("sample before the gate release is not activity-only")
		}
		if !s.evt.LastLocalWriteAt.IsZero() {
			t.Errorf("waiting on the metadata gate recorded local-write activity at %v", s.evt.LastLocalWriteAt)
		}
		if !s.evt.LastReadAt.IsZero() {
			t.Errorf("waiting on the metadata gate recorded read activity at %v", s.evt.LastReadAt)
		}
	}
	if duringWait < 2 {
		t.Fatalf("samples during the 700 ms claim wait = %d, want at least two", duringWait)
	}
}

// TestSingleProgressCarriesActivityTimestamps covers the single path's existing
// emission points: both activity instants ride the final progress event.
func TestSingleProgressCarriesActivityTimestamps(t *testing.T) {
	full := rangedTestPayload(64 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	}))
	t.Cleanup(srv.Close)
	const relPath = "single-activity.bin"
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
	last := events[len(events)-1]
	if last.LastReadAt.IsZero() || last.LastLocalWriteAt.IsZero() {
		t.Fatalf("final activity = read %v, write %v; want both set",
			last.LastReadAt, last.LastLocalWriteAt)
	}
}
