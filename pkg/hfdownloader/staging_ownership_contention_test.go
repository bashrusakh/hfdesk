// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// A shared status probe can conflict with an exclusive claim on Unix. That
// contention is not proof of a live writer: retry or fail rather than report a
// successful skip. Tests exercise this through Download.

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stagingProbeHolder holds a probe-like lock until the test releases it.
type stagingProbeHolder struct {
	release func()
	held    func() bool
}

type stagingProbeHoldMode int

const (
	// probeHoldShared holds the lock the way a read-only probe does.
	probeHoldShared stagingProbeHoldMode = iota
	// probeHoldExclusive holds the lock the way a real writer does.
	probeHoldExclusive
)

func startStagingProbeHolder(t *testing.T, staging string, mode stagingProbeHoldMode) *stagingProbeHolder {
	t.Helper()
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	// Make sure the lock file exists so it can be opened.
	own, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	own.release()

	h, err := openStagingProbeHandle(stagingOwnershipLockPath(staging), mode)
	if err != nil {
		t.Fatal(err)
	}
	return &stagingProbeHolder{
		release: func() { h.release() },
		held:    func() bool { return h.held() },
	}
}

type stagingProbeHandle struct {
	releaseFn func()
	heldFn    func() bool
}

func (h stagingProbeHandle) release() {
	if h.releaseFn != nil {
		h.releaseFn()
	}
}

func (h stagingProbeHandle) held() bool {
	return h.heldFn != nil && h.heldFn()
}

type contentionDownloadFixture struct {
	srv       *httptest.Server
	out       string
	dst       string
	staging   string
	metaPath  string
	sha       string
	full      []byte
	total     int
	getRanges func() []string
}

// newContentionDownloadFixture seeds a valid resumable prefix.
func newContentionDownloadFixture(t *testing.T) *contentionDownloadFixture {
	t.Helper()
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	srv, getRanges := recordingHub(t, full, "model.bin", true)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)
	staging := dst + ".incomplete"
	metaPath := staging + ".meta"
	seedRangedState(t, staging, "o/r", "model.bin", total,
		[]RangeProgress{{Start: 0, End: total - 1, Committed: total / 2}}, full)
	meta, err := readStagingMeta(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	meta.Commit = "commit123"
	meta.SHA256 = sha
	meta.LockFenced = true
	if err := writeStagingMeta(metaPath, meta); err != nil {
		t.Fatal(err)
	}
	return &contentionDownloadFixture{
		srv: srv, out: out, dst: dst, staging: staging, metaPath: metaPath,
		sha: sha, full: full, total: total, getRanges: getRanges,
	}
}

func (f *contentionDownloadFixture) settings() Settings {
	return Settings{
		OutputDir:          f.out,
		Endpoint:           f.srv.URL,
		Concurrency:        1,
		MaxActiveDownloads: 1,
		MultipartThreshold: "1MB",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
}

type downloadOutcome struct {
	skips  []string
	err    error
	fatal  bool
	events []ProgressEvent
}

func (f *contentionDownloadFixture) runDownload(t *testing.T, ctx context.Context) downloadOutcome {
	t.Helper()
	out := downloadOutcome{}
	err := Download(ctx, Job{Repo: "o/r", Revision: "main"}, f.settings(), func(ev ProgressEvent) {
		out.events = append(out.events, ev)
		if ev.Event == "file_done" && strings.Contains(ev.Message, "skip") {
			out.skips = append(out.skips, ev.Message)
		}
	})
	out.err = err
	return out
}

// A shared status probe may block exclusive acquisition without being a
// writer; the claim must retry and publish. The hook signals the exact wait.
func TestDownload_StatusProbeInFlightDoesNotSkipTheFile(t *testing.T) {
	f := newContentionDownloadFixture(t)

	probe := startStagingProbeHolder(t, f.staging, probeHoldShared)
	if !probe.held() {
		t.Fatal("the probe did not take the lock file")
	}
	defer probe.release()

	// The acquisition hook signals contention while the test still holds the probe.
	contended := make(chan struct{})
	var once sync.Once
	restore := stagingOwnershipContentionHook
	stagingOwnershipContentionHook = func(staging string) {
		once.Do(func() { close(contended) })
	}
	defer func() { stagingOwnershipContentionHook = restore }()

	result := make(chan downloadOutcome, 1)
	go func() { result <- f.runDownload(t, context.Background()) }()

	select {
	case <-contended:
	case out := <-result:
		// Early completion is acceptable only as non-skipping success.
		if len(out.skips) > 0 {
			t.Fatalf("the job skipped the file while a probe held the lock: %v", out.skips)
		}
		if out.err != nil {
			t.Fatalf("Download: %v", out.err)
		}
		return
	case <-time.After(30 * time.Second):
		t.Fatal("the claim never observed the contended ownership lock")
	}

	probe.release()

	var out downloadOutcome
	select {
	case out = <-result:
	case <-time.After(30 * time.Second):
		t.Fatal("Download did not finish after the probe released the lock")
	}

	if len(out.skips) > 0 {
		t.Fatalf("the job recorded a successful skip while a probe held the lock: %v", out.skips)
	}
	if out.err != nil {
		t.Fatalf("Download: %v", out.err)
	}
	assertExactFile(t, f.dst, f.full)
	if got := f.getRanges(); len(got) != 1 || got[0] != fmt.Sprintf("bytes=%d-%d", f.total/2, f.total-1) {
		t.Errorf("ranged GETs = %v, want exactly the uncommitted suffix", got)
	}
}

// Unresolved probe contention must fail instead of skipping a missing file.
func TestDownload_UnresolvableContentionFailsInsteadOfSkipping(t *testing.T) {
	f := newContentionDownloadFixture(t)

	probe := startStagingProbeHolder(t, f.staging, probeHoldShared)
	defer probe.release()

	// Shrink the acquisition window so the test does not wait the production
	// bound; the behavior under test is the classification, not the duration.
	shortBlobOwnershipWait(t, 300*time.Millisecond)

	// Prove the contention really happened while the probe held the lock.
	contended := make(chan struct{})
	var once sync.Once
	restoreHook := stagingOwnershipContentionHook
	stagingOwnershipContentionHook = func(staging string) {
		once.Do(func() { close(contended) })
	}
	defer func() { stagingOwnershipContentionHook = restoreHook }()

	out := f.runDownload(t, context.Background())

	select {
	case <-contended:
	default:
		t.Fatal("the claim never observed the contended ownership lock")
	}
	if len(out.skips) > 0 {
		t.Fatalf("the job recorded a successful skip although no live writer was proven: %v", out.skips)
	}
	if out.err == nil {
		t.Fatal("Download succeeded although the ownership lock never became free")
	}
	if !errors.Is(out.err, errStagingOwnershipUncertain) {
		t.Errorf("Download error = %v, want errStagingOwnershipUncertain", out.err)
	}
	if _, err := os.Stat(f.dst); err == nil {
		t.Error("the destination exists although the download failed")
	}
	if _, err := os.Stat(f.staging); err != nil {
		t.Errorf("the resumable staging file was removed by the failed claim: %v", err)
	}
}

// Cancellation interrupts a contended ownership wait without a skip.
func TestDownload_CancellationWhileWaitingForOwnershipIsPrompt(t *testing.T) {
	f := newContentionDownloadFixture(t)

	probe := startStagingProbeHolder(t, f.staging, probeHoldShared)
	defer probe.release()

	// Cancel only after destination-domain acquisition reports lock contention.
	contended := make(chan struct{})
	var once sync.Once
	restoreHook := stagingOwnershipContentionHook
	stagingOwnershipContentionHook = func(staging string) {
		once.Do(func() { close(contended) })
	}
	defer func() { stagingOwnershipContentionHook = restoreHook }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan downloadOutcome, 1)
	go func() { result <- f.runDownload(t, ctx) }()

	select {
	case <-contended:
	case out := <-result:
		if len(out.skips) > 0 {
			t.Fatalf("the job skipped the file although it was cancelled: %v", out.skips)
		}
		if out.err == nil {
			t.Fatal("Download succeeded although it was cancelled")
		}
		return
	case <-time.After(30 * time.Second):
		t.Fatal("the claim never observed the contended ownership lock")
	}

	cancel()

	var out downloadOutcome
	select {
	case out = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("Download did not stop after its context was cancelled")
	}
	if len(out.skips) > 0 {
		t.Fatalf("the job recorded a successful skip although it was cancelled: %v", out.skips)
	}
	if out.err == nil {
		t.Fatal("Download succeeded although it was cancelled")
	}
	if !errors.Is(out.err, context.Canceled) {
		t.Errorf("Download error = %v, want context.Canceled", out.err)
	}
}

// Repeated claims under shared-probe pressure must not report a live writer
// when none exists.
func TestDownload_ConcurrentProbeAndClaimStressNeverReportsFalseSuccess(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	dir := t.TempDir()
	pub := filepath.Join(dir, "stress.bin")
	staging := flatDomainLockSrc(pub)

	// Give the domain lock its persistent presence so the probes can open it.
	own, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	own.release()

	stop := make(chan struct{})
	var probes sync.WaitGroup
	probes.Add(1)
	go func() {
		defer probes.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			handle, err := openStagingProbeHandle(stagingOwnershipLockPath(staging), probeHoldShared)
			if err != nil {
				continue
			}
			// Hold the shared probe longer to increase the contention window.
			time.Sleep(500 * time.Microsecond)
			handle.release()
		}
	}()

	var contentionCount atomic.Int64
	restoreHook := stagingOwnershipContentionHook
	stagingOwnershipContentionHook = func(staging string) {
		contentionCount.Add(1)
	}
	defer func() { stagingOwnershipContentionHook = restoreHook }()

	// Keep the contention window short so the stress finishes quickly; the
	// behavior under test is the classification, not the duration.
	shortBlobOwnershipWait(t, 200*time.Millisecond)

	// Repeated acquisitions under shared-reader pressure must not report a foreign
	// live owner; no competing writer exists in this test.
	key, lockSrc := flatDomainKey(pub), flatDomainLockSrc(pub)
	var falseSkips int
	var uncertain int
	for i := 0; i < 60; i++ {
		domain, err := acquireBlobDomain(context.Background(), key, lockSrc)
		if err == nil {
			domain.release()
			continue
		}
		if errors.Is(err, errStagingOwnershipBusy) {
			falseSkips++
			continue
		}
		if errors.Is(err, errStagingOwnershipUncertain) {
			uncertain++
			continue
		}
		t.Fatalf("acquisition %d: %v", i, err)
	}
	close(stop)
	probes.Wait()

	if contentionCount.Load() == 0 {
		t.Skip("no contention was observed; the probes did not overlap the claims")
	}
	if falseSkips != 0 {
		t.Fatalf("%d claims reported a live owner although no writer exists (false successful skip)", falseSkips)
	}
	// A claim that could not resolve the contention must be an explicit
	// uncertain failure, never a successful skip.
	if uncertain > 0 {
		t.Logf("%d claims ended uncertain (actionable failure, not a skip)", uncertain)
	}
}

func TestDownload_ConcurrentProbeStressCompletesTheFile(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	f := newContentionDownloadFixture(t)

	stop := make(chan struct{})
	var probes sync.WaitGroup
	probes.Add(1)
	go func() {
		defer probes.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			handle, err := openStagingProbeHandle(stagingOwnershipLockPath(f.staging), probeHoldShared)
			if err != nil {
				continue
			}
			time.Sleep(500 * time.Microsecond)
			handle.release()
		}
	}()

	out := f.runDownload(t, context.Background())
	close(stop)
	probes.Wait()

	if len(out.skips) > 0 {
		t.Fatalf("the job recorded a successful skip under probe contention: %v", out.skips)
	}
	if out.err != nil {
		t.Fatalf("Download under probe contention: %v", out.err)
	}
	assertExactFile(t, f.dst, f.full)
}

// The non-owning probe detects exclusive writers, not other readers.
func TestStagingOwnershipHolderObservation(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	dir := t.TempDir()
	staging := filepath.Join(dir, "x.incomplete")
	lockPath := stagingOwnershipLockPath(staging)

	// The lock file must exist for the probes to open it.
	own, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	own.release()

	// With no holder, no exclusive holder is observed. The observation does
	// NOT claim to distinguish "reader" from "nobody": a granted shared
	// probe means exactly "no exclusive holder".
	kind, err := observeStagingOSLockHolder(lockPath)
	if err != nil {
		t.Fatalf("observe with no holder: %v", err)
	}
	if kind != stagingLockHolderNotExclusive {
		t.Errorf("holder kind with no holder = %v, want stagingLockHolderNotExclusive", kind)
	}

	reader := startStagingProbeHolder(t, staging, probeHoldShared)
	kind, err = observeStagingOSLockHolder(lockPath)
	if err != nil {
		t.Fatalf("observe with a reader: %v", err)
	}
	if kind != stagingLockHolderNotExclusive {
		t.Errorf("holder kind with a shared reader = %v, want stagingLockHolderNotExclusive", kind)
	}
	reader.release()

	writer, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	kind, err = observeStagingOSLockHolder(lockPath)
	if err != nil {
		t.Fatalf("observe with a writer: %v", err)
	}
	if kind != stagingLockHolderExclusive {
		t.Errorf("holder kind with an exclusive writer = %v, want stagingLockHolderExclusive", kind)
	}
	writer.release()

	kind, err = observeStagingOSLockHolder(lockPath)
	if err != nil {
		t.Fatalf("observe after release: %v", err)
	}
	if kind != stagingLockHolderNotExclusive {
		t.Errorf("holder kind after release = %v, want stagingLockHolderNotExclusive", kind)
	}
}
