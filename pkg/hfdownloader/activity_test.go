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
	"strconv"
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
	if readAt, writeAt, readBytes, inFlight := activity.snapshot(); !readAt.IsZero() || !writeAt.IsZero() || readBytes != 0 || inFlight {
		t.Fatalf("activity recorded before bytes arrived: read=%v write=%v bytes=%d inFlight=%v", readAt, writeAt, readBytes, inFlight)
	}

	close(reader.release)
	if err := <-done; err != nil {
		t.Fatalf("read: %v", err)
	}
	readAt, writeAt, readBytes, inFlight := activity.snapshot()
	if readAt.IsZero() {
		t.Fatal("read activity was not recorded after bytes arrived")
	}
	if readBytes != 4 {
		t.Fatalf("read bytes = %d, want 4", readBytes)
	}
	if inFlight {
		t.Fatal("a read must not mark local work in flight")
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
	if _, writeAt, readBytes, inFlight := activity.snapshot(); !writeAt.IsZero() || readBytes != 0 || !inFlight {
		t.Fatalf("blocked write state: write=%v bytes=%d inFlight=%v, want zero write, zero reads and in-flight", writeAt, readBytes, inFlight)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("write: %v", err)
	}
	_, writeAt, readBytes, inFlight := activity.snapshot()
	if writeAt.IsZero() {
		t.Fatal("write activity was not recorded after completion")
	}
	if inFlight {
		t.Fatal("local work is still marked in flight after completion")
	}
	if readBytes != 0 {
		t.Fatalf("read bytes = %d after a pure write, want 0", readBytes)
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
	prepInFlight := 0
	for _, s := range samples {
		if !s.evt.ActivityOnly {
			continue
		}
		prepSamples++
		if s.evt.Downloaded != 0 {
			t.Errorf("preparation sample reported %d transferred bytes, want 0", s.evt.Downloaded)
		}
		if s.evt.ReadBytes != 0 {
			t.Errorf("preparation sample reported %d read bytes, want 0", s.evt.ReadBytes)
		}
		if s.evt.LocalWorkInFlight {
			prepInFlight++
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
	if prepInFlight == 0 {
		t.Fatal("no preparation sample reported the slow truncate as local work in flight")
	}

	last := samples[len(samples)-1]
	if last.evt.ActivityOnly {
		t.Error("the final progress sample is still marked activity-only")
	}
	if last.evt.LastReadAt.IsZero() {
		t.Error("the final progress sample has no read activity")
	}
	if last.evt.ReadBytes <= 0 {
		t.Error("the final progress sample has no read bytes")
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
		if s.evt.LocalWorkInFlight {
			t.Errorf("waiting on the metadata gate reported local work in flight")
		}
		if s.evt.ReadBytes != 0 {
			t.Errorf("waiting on the metadata gate reported %d read bytes", s.evt.ReadBytes)
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

// TestSingleResumeReadCounterExcludesInheritedBytes checks that bytes already
// on disk from a previous run never enter the read counter used for speed/ETA.
func TestSingleResumeReadCounterExcludesInheritedBytes(t *testing.T) {
	full := rangedTestPayload(200)
	const partialN = 80
	dst := filepath.Join(t.TempDir(), "resume-activity.bin")
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "resume-activity.bin", Size: int64(len(full))}
	seedSinglePartial(t, dst, full[:partialN], job, it, singleTestIdentity)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	}))
	t.Cleanup(srv.Close)
	it.URL = srv.URL + "/resume-activity.bin"

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
	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, Settings{Retries: 0}, it, dst, singleTestIdentity, emit); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 {
		t.Fatal("no file_progress events were emitted")
	}
	last := events[len(events)-1]
	if last.Downloaded != int64(len(full)) {
		t.Fatalf("final downloaded = %d, want %d", last.Downloaded, len(full))
	}
	if last.ReadBytes != int64(len(full)-partialN) {
		t.Fatalf("read bytes = %d, want %d remaining bytes only", last.ReadBytes, len(full)-partialN)
	}
}

// TestSingleProgressTickerReportsActivityBeforeCompletion covers the single
// path's periodic activity-only samples: they carry read/in-flight state while
// the transfer runs, so a blocked write can be reported before it completes.
func TestSingleProgressTickerReportsActivityBeforeCompletion(t *testing.T) {
	full := rangedTestPayload(64 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		flusher, _ := w.(http.Flusher)
		for off := 0; off < len(full); off += 4096 {
			end := off + 4096
			if end > len(full) {
				end = len(full)
			}
			if _, err := w.Write(full[off:end]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(120 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)

	const relPath = "ticker-single.bin"
	dst := filepath.Join(t.TempDir(), relPath)
	var (
		mu      sync.Mutex
		samples []ProgressEvent
	)
	emit := func(ev ProgressEvent) {
		if ev.Event != "file_progress" {
			return
		}
		mu.Lock()
		samples = append(samples, ev)
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
	activitySamples := 0
	for _, ev := range samples {
		if !ev.ActivityOnly {
			continue
		}
		activitySamples++
		if ev.ReadBytes <= 0 {
			t.Errorf("activity-only sample has no read bytes")
		}
		if ev.LastReadAt.IsZero() {
			t.Errorf("activity-only sample has no read activity")
		}
	}
	if activitySamples < 2 {
		t.Fatalf("activity-only samples = %d, want at least two during the slow transfer", activitySamples)
	}
	last := samples[len(samples)-1]
	if last.ActivityOnly {
		t.Error("the final progress sample is still marked activity-only")
	}
	if last.ReadBytes != int64(len(full)) {
		t.Fatalf("final read bytes = %d, want %d", last.ReadBytes, len(full))
	}
	if last.Downloaded != int64(len(full)) {
		t.Fatalf("final downloaded = %d, want %d", last.Downloaded, len(full))
	}
}
