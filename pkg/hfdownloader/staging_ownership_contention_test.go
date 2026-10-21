// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the staging ownership contention bug (issue #140 /
// PR #151 review).
//
// The staging ownership lock file serves two purposes at once: a real writer
// holds it exclusively, and a read-only status probe touches it briefly. On
// Unix a probe takes a shared flock while a claimant takes an exclusive one,
// and those two conflict on the same inode — even inside one process, because
// flock locks belong to the open file description, not the process.
//
// The bug: a status probe in flight made the claim's exclusive acquisition
// fail, and the claim mapped that straight to errBlobIncompleteOwned. The job
// then recorded a SUCCESSFUL skip ("downloading by another process") even
// though no writer existed, so the file was never downloaded and never
// published.
//
// The fix classifies the holder natively (flock(2) grants a shared lock to a
// reader only when no exclusive holder exists), waits out a reader within a
// bounded, context-aware window, skips only for a PROVEN live writer, and
// reports an actionable retryable failure when the contention cannot be
// resolved — never a successful skip.
//
// The cases below are driven through the real Download path so they exercise
// the product routing, not just the raw lock primitive.

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

// stagingProbeHolder is a deterministic, controllable stand-in for an in-flight
// status probe: it holds the lock file the way a probe does (shared on Unix,
// shared-access on Windows) and releases it only when the test says so.
//
// The tests use it instead of a sleep so the contention window is exact: the
// probe is provably held while the claim attempts its acquisition, and the
// release happens on a signal rather than after a guessed delay.
type stagingProbeHolder struct {
	release func()
	held    func() bool
}

// stagingProbeHoldMode selects how the probe holds the lock file.
type stagingProbeHoldMode int

const (
	// probeHoldShared holds the lock the way a read-only probe does.
	probeHoldShared stagingProbeHoldMode = iota
	// probeHoldExclusive holds the lock the way a real writer does.
	probeHoldExclusive
)

// startStagingProbeHolder takes the lock file for staging in the given mode
// and returns a holder that can release it deterministically.
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

// stagingProbeHandle is one held probe lock.
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

// contentionDownloadFixture is a ready-to-run Download fixture: a hub that
// serves one LFS file with range support, a legacy output destination and a
// seeded, resumable staging state.
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

// newContentionDownloadFixture builds the fixture. The staging payload holds
// a validated committed prefix, so the item is genuinely resumable.
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

// settings returns the Download settings for the fixture.
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

// downloadOutcome collects what the job reported for the fixture's file.
type downloadOutcome struct {
	skips  []string
	err    error
	fatal  bool
	events []ProgressEvent
}

// runDownload runs the fixture's Download and collects the outcome.
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

// TestDownload_StatusProbeInFlightDoesNotSkipTheFile is the core regression.
//
// A status probe holds the lock file while the claim attempts its exclusive
// acquisition. The claim must wait the probe out and then download and
// publish the file. Before the fix the claim saw a busy lock, reported
// errBlobIncompleteOwned and the job recorded a successful skip with no file
// published.
//
// The synchronization is deterministic: the claim signals through the
// contention hook that it really hit the busy lock while the probe held it,
// and the test releases the probe only then. No sleep decides the outcome.
func TestDownload_StatusProbeInFlightDoesNotSkipTheFile(t *testing.T) {
	f := newContentionDownloadFixture(t)

	probe := startStagingProbeHolder(t, f.staging, probeHoldShared)
	if !probe.held() {
		t.Fatal("the probe did not take the lock file")
	}
	defer probe.release()

	// The claim reports the contention it observed. The probe is provably
	// held at that moment.
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
		// The download finished before any contention was observed. That is
		// only acceptable if it did not skip.
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

	// The probe is released now, so the claim can take the lock.
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

// TestDownload_UnresolvableContentionFailsInsteadOfSkipping covers the
// timeout case: the contention never resolves, so no live writer is proven.
// The job must fail with an actionable error and must NOT report a successful
// skip with a missing file.
func TestDownload_UnresolvableContentionFailsInsteadOfSkipping(t *testing.T) {
	f := newContentionDownloadFixture(t)

	probe := startStagingProbeHolder(t, f.staging, probeHoldShared)
	defer probe.release()

	// Shrink the acquisition window so the test does not wait the production
	// bound; the behavior under test is the classification, not the duration.
	restore := stagingOwnershipAcquireWait
	stagingOwnershipAcquireWait = 300 * time.Millisecond
	defer func() { stagingOwnershipAcquireWait = restore }()

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
	// The staging state of the resumable file must survive the failure.
	if _, err := os.Stat(f.staging); err != nil {
		t.Errorf("the resumable staging file was removed by the failed claim: %v", err)
	}
}

// TestDownload_CancellationWhileWaitingForOwnershipIsPrompt covers
// cancellation: a claimant that is waiting out a contended lock must stop
// promptly when its context is cancelled, and must not report a skip.
func TestDownload_CancellationWhileWaitingForOwnershipIsPrompt(t *testing.T) {
	f := newContentionDownloadFixture(t)

	probe := startStagingProbeHolder(t, f.staging, probeHoldShared)
	defer probe.release()

	// Cancel only once the claim is provably waiting on the contended lock.
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

// TestClaimStaging_LiveOwnerIsStillRecognizedWithoutWaiting pins the genuine
// owner behavior: a proven live writer must be reported as a live owner (and
// the caller skips) promptly, without waiting out the acquisition window.
func TestClaimStaging_LiveOwnerIsStillRecognizedWithoutWaiting(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	dir := t.TempDir()
	staging := filepath.Join(dir, "owned.bin.incomplete")
	metaPath := staging + ".meta"
	pub := filepath.Join(dir, "owned.bin")

	// A real owner holds the lock exclusively for the whole interval.
	owner, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.release()

	// Give the lock file its persistent presence with the owner already
	// holding it.
	if err := os.WriteFile(staging, full, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{
		PID: os.Getpid(), Instance: "other-instance", Host: processHostname,
		StartedAt: time.Now().UTC(), Size: total, SHA256: sha, Version: stagingMetaVersion,
		Repo: "o/r", Commit: "commit123", Path: "owned.bin",
		Ranges: buildRangeLayout(total, 1), LockFenced: true,
	}
	if err := writeStagingMeta(metaPath, &meta); err != nil {
		t.Fatal(err)
	}

	// Widen the acquisition window: a live owner must be recognized from the
	// native classification, not by timing out the window.
	restore := stagingOwnershipAcquireWait
	stagingOwnershipAcquireWait = 30 * time.Second
	defer func() { stagingOwnershipAcquireWait = restore }()

	start := time.Now()
	state, err := claimStaging(context.Background(), staging, pub, metaPath,
		PlanItem{RelativePath: "owned.bin", Size: total, LFS: true, SHA256: sha},
		Job{Repo: "o/r"}, "commit123", Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}, false)
	elapsed := time.Since(start)
	if state != nil {
		state.release()
	}
	if !errors.Is(err, errBlobIncompleteOwned) {
		t.Fatalf("claim error = %v, want errBlobIncompleteOwned for a live owner", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("a live owner took %v to be recognized; the classification must not wait out the window", elapsed)
	}
	// The owner's staging must be untouched.
	assertExactFile(t, staging, full)
	if _, serr := os.Stat(pub); !os.IsNotExist(serr) {
		t.Errorf("the live owner's staging was published, stat err=%v", serr)
	}
}

// TestDownload_ConcurrentProbeAndClaimStressNeverReportsFalseSuccess is the
// stress case: a continuous stream of status probes runs against repeated
// claims. The invariant is that no claim ever reports "another live writer
// owns this" while no writer exists — that verdict is what produced the
// reported false success (the job counted the file as done, and the file was
// never downloaded).
//
// Before the fix, a probe in flight made the claim fail its exclusive
// acquisition and the claim mapped that straight to errBlobIncompleteOwned.
func TestDownload_ConcurrentProbeAndClaimStressNeverReportsFalseSuccess(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	dir := t.TempDir()
	staging := filepath.Join(dir, "stress.bin.incomplete")
	metaPath := staging + ".meta"
	pub := filepath.Join(dir, "stress.bin")
	if err := os.WriteFile(staging, full, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{
		PID: os.Getpid(), Instance: "stress-instance", Host: processHostname,
		StartedAt: time.Now().UTC(), Size: total, SHA256: sha, Version: stagingMetaVersion,
		Repo: "o/r", Commit: "commit123", Path: "stress.bin",
		Ranges: buildRangeLayout(total, 1), LockFenced: true,
	}
	if err := writeStagingMeta(metaPath, &meta); err != nil {
		t.Fatal(err)
	}

	// Probes run continuously against the lock file.
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
			// Hold the reader lock briefly, exactly like a status probe.
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

	it := PlanItem{RelativePath: "stress.bin", Size: total, LFS: true, SHA256: sha}
	job := Job{Repo: "o/r"}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}

	// Keep the contention window short so the stress finishes quickly; the
	// behavior under test is the classification, not the duration.
	restoreWait := stagingOwnershipAcquireWait
	stagingOwnershipAcquireWait = 200 * time.Millisecond
	defer func() { stagingOwnershipAcquireWait = restoreWait }()

	// Repeated claims under continuous probe pressure. None of them may
	// report a live owner: no writer exists in this test.
	var falseSkips int
	var uncertain int
	for i := 0; i < 60; i++ {
		state, err := claimStaging(context.Background(), staging, pub, metaPath, it, job, "commit123", cfg, false)
		if state != nil {
			state.release()
			continue
		}
		if errors.Is(err, errBlobIncompleteOwned) {
			falseSkips++
			continue
		}
		if errors.Is(err, errStagingOwnershipUncertain) {
			uncertain++
			continue
		}
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
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

// TestDownload_ConcurrentProbeStressCompletesTheFile drives the whole
// Download path while probes run continuously. The reported outcome must
// match reality: the file exists and is byte-exact when the job reports
// success.
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
	// A reported success must be real: the file exists and is byte-exact.
	assertExactFile(t, f.dst, f.full)
}

// TestStagingOwnershipHolderClassification pins the native classification the
// fix depends on: a shared reader and an exclusive writer must be told apart
// without any timing heuristic.
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

	// A shared reader (a status probe) must not be observed as a writer.
	reader := startStagingProbeHolder(t, staging, probeHoldShared)
	kind, err = observeStagingOSLockHolder(lockPath)
	if err != nil {
		t.Fatalf("observe with a reader: %v", err)
	}
	if kind != stagingLockHolderNotExclusive {
		t.Errorf("holder kind with a shared reader = %v, want stagingLockHolderNotExclusive", kind)
	}
	reader.release()

	// An exclusive holder (a real writer) must be observed as exclusive.
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

// TestStagingOwnershipObservationNeverTakesTheExclusiveLock is the negative
// control for the native observer.
//
// The observation must be non-owning: if it took the exclusive lock, every
// peer would see a live writer that does not exist. This test proves the
// observation leaves the lock free for a real writer, and — the decisive
// check — that a concurrent observer does not make the lock look exclusively
// held to another observer.
func TestStagingOwnershipObservationNeverTakesTheExclusiveLock(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	dir := t.TempDir()
	staging := filepath.Join(dir, "x.incomplete")
	lockPath := stagingOwnershipLockPath(staging)

	own, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	own.release()

	// A real writer must be able to take the lock right after an
	// observation: the observation leaves no exclusive lock behind.
	for i := 0; i < 20; i++ {
		kind, oerr := observeStagingOSLockHolder(lockPath)
		if oerr != nil {
			t.Fatalf("observe: %v", oerr)
		}
		if kind != stagingLockHolderNotExclusive {
			t.Fatalf("observation %d reported %v with no writer present", i, kind)
		}
	}
	writer, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatalf("a real writer could not take the lock after the observations: %v", err)
	}
	writer.release()

	// The decisive check: while a concurrent observer runs, another observer
	// must NOT see an exclusive holder. An exclusive observer would make
	// this fail.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	observing := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				kind, oerr := observeStagingOSLockHolder(lockPath)
				if oerr != nil {
					continue
				}
				if kind == stagingLockHolderExclusive {
					// A concurrent observer must never look like a writer.
					t.Errorf("a concurrent observer was observed as an exclusive holder")
					return
				}
			}
		}()
	}
	close(observing)
	// Let the observers run, then stop them.
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	// After the observers stopped, the lock must be free for a real writer.
	writer, err = acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatalf("a real writer could not take the lock after the observers stopped: %v", err)
	}
	writer.release()
}
