// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the multipart staging claim's MaxActiveDownloads
// slot lease (issue #140 / PR #151 follow-up).
//
// A staging claim is a WAIT, not a transfer. The claim can wait for an
// in-process owner (the stagingOwners registry), for the cross-process
// lifetime OS lock (a read-only probe holding the lock briefly) and for the
// local metadata gate. Each of those waits used to run with the item's slot
// held, so with a low limit one waiting item kept every other file of the job
// from starting.
//
// The claim now signals the item's lease through the same context notice the
// destination-domain acquisition uses: it returns the slot for the duration of
// a wait and takes it back before it returns a claim the caller will transfer
// under. A failed claim keeps the slot returned (no claim reaches the caller,
// so the item reports the error instead of transferring).
//
// The synchronization is deterministic: the recorder's began channel is
// closed by the first beginWait, and the in-process owner/lock/gate is then
// released by the test. No sleep decides an outcome; every deadline only turns
// a missing implementation into a readable failure.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// stagingClaimTestFixture returns fresh claim paths and identity for the
// claim-lease cases. One temp dir per case means the in-process registry, the
// OS ownership lock and the metadata gate are unique to that case.
type stagingClaimTestFixture struct {
	staging string
	publish string
	meta    string
	job     Job
	it      PlanItem
}

func newStagingClaimTestFixture(t *testing.T) stagingClaimTestFixture {
	t.Helper()
	dir := t.TempDir()
	staging := filepath.Join(dir, "payload.bin.incomplete")
	return stagingClaimTestFixture{
		staging: staging,
		publish: filepath.Join(dir, "payload.bin"),
		meta:    staging + ".meta",
		job:     Job{Repo: "owner/model"},
		it:      PlanItem{RelativePath: "payload.bin", Size: 8},
	}
}

// claim runs one staging claim with the fixture's identity.
func (f stagingClaimTestFixture) claim(ctx context.Context) (*stagingClaimState, error) {
	return claimStaging(ctx, f.staging, f.publish, f.meta, f.it, f.job, "commit123", true,
		Settings{Verify: "none"}, false)
}

type stagingClaimResult struct {
	state *stagingClaimState
	err   error
}

// claimAsync runs one claim and reports its outcome.
func (f stagingClaimTestFixture) claimAsync(ctx context.Context) <-chan stagingClaimResult {
	result := make(chan stagingClaimResult, 1)
	go func() {
		state, err := f.claim(ctx)
		result <- stagingClaimResult{state: state, err: err}
	}()
	return result
}

// awaitStagingClaimBegin waits until the claim signalled that it returned the
// item's slot for a wait.
func awaitStagingClaimBegin(t *testing.T, notice *slotNoticeRecorder) {
	t.Helper()
	select {
	case <-notice.began:
	case <-time.After(15 * time.Second):
		t.Fatal("the staging claim never returned the item's transfer slot for its wait (no beginWait signal)")
	}
}

// assertClaimLeaseSignals asserts the recorded lease signal counts.
func assertClaimLeaseSignals(t *testing.T, notice *slotNoticeRecorder, wantBegins, wantEnds int, context string) {
	t.Helper()
	begins, ends := notice.counts()
	if begins != wantBegins || ends != wantEnds {
		t.Errorf("lease signals = begin %d / end %d, want %d/%d: %s", begins, ends, wantBegins, wantEnds, context)
	}
}

// installStagingOwnerForTest installs the in-process staging owner entry a
// live writer would hold, and returns an idempotent release that removes it
// and wakes the claim's in-process wait - the same order the production
// release closure uses (remove from the registry, then close done).
func installStagingOwnerForTest(t *testing.T, staging string) func() {
	t.Helper()
	entry := &stagingOwnerEntry{done: make(chan struct{}), instance: processInstanceToken}
	stagingOwnersMu.Lock()
	if _, busy := stagingOwners[staging]; busy {
		stagingOwnersMu.Unlock()
		t.Fatalf("staging path %s already has an in-process owner entry", staging)
	}
	stagingOwners[staging] = entry
	stagingOwnersMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			stagingOwnersMu.Lock()
			if stagingOwners[staging] == entry {
				delete(stagingOwners, staging)
			}
			stagingOwnersMu.Unlock()
			close(entry.done)
		})
	}
}

// TestStagingClaimLeaseInProcessOwnerWait pins the FIRST claim wait: an
// in-process owner holds the staging path, so the claim returns the item's
// slot before it waits for that owner, then takes it back before it returns
// the claim it will transfer under.
func TestStagingClaimLeaseInProcessOwnerWait(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	f := newStagingClaimTestFixture(t)
	releaseOwner := installStagingOwnerForTest(t, f.staging)
	defer releaseOwner()

	notice := &slotNoticeRecorder{began: make(chan struct{})}
	ctx := withDownloadSlotWaitNotice(context.Background(), notice)
	result := f.claimAsync(ctx)

	// The item is already WAITING for the in-process owner: it must not be
	// holding its transfer slot now.
	awaitStagingClaimBegin(t, notice)
	releaseOwner()

	res := <-result
	if res.err != nil {
		t.Fatalf("claim after the in-process owner released: %v", res.err)
	}
	if res.state == nil {
		t.Fatal("claim returned no state after the in-process owner released")
	}
	// The claim is a real claim: releasing it must deregister the path.
	res.state.release()
	assertClaimLeaseSignals(t, notice, 1, 1,
		"the claim must return the slot for the in-process wait and take it back for the transfer")
}

// TestStagingClaimLeaseInProcessOwnerWaitCancelled pins the cancellation path
// of the first claim wait: the claim reports the cancellation, returns no
// claim, and leaves the slot returned (there is nothing to transfer).
func TestStagingClaimLeaseInProcessOwnerWaitCancelled(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	f := newStagingClaimTestFixture(t)
	releaseOwner := installStagingOwnerForTest(t, f.staging)
	defer releaseOwner()

	notice := &slotNoticeRecorder{began: make(chan struct{})}
	ctx, cancel := context.WithCancel(withDownloadSlotWaitNotice(context.Background(), notice))
	defer cancel()
	result := f.claimAsync(ctx)
	awaitStagingClaimBegin(t, notice)

	cancel()
	res := <-result
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("claim error = %v, want context.Canceled", res.err)
	}
	if res.state != nil {
		res.state.release()
		t.Fatalf("a cancelled claim returned a claim state: %+v", res.state)
	}
	assertClaimLeaseSignals(t, notice, 1, 0,
		"a cancelled claim has nothing to transfer and must not retake the slot")
}

// TestStagingClaimLeaseFreePath pins the free (uncontended) path: a claim that
// never waits must neither return nor retake the item's slot.
func TestStagingClaimLeaseFreePath(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	f := newStagingClaimTestFixture(t)
	notice := &slotNoticeRecorder{}
	ctx := withDownloadSlotWaitNotice(context.Background(), notice)

	state, err := f.claim(ctx)
	if err != nil {
		t.Fatalf("uncontended claim: %v", err)
	}
	if state == nil {
		t.Fatal("uncontended claim returned no state")
	}
	state.release()
	assertClaimLeaseSignals(t, notice, 0, 0,
		"the free fast path must not signal any wait")
}

// TestStagingClaimLeaseLocalMetadataGateWait pins the THIRD claim wait: the
// local metadata gate is held, so the claim returns the item's slot before it
// waits for the gate and takes it back on success.
func TestStagingClaimLeaseLocalMetadataGateWait(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	f := newStagingClaimTestFixture(t)
	gate := &stagingStateLock{path: stagingLockPath(f.meta)}
	if err := gate.acquire(context.Background(), time.Second); err != nil {
		t.Fatalf("hold the local metadata gate: %v", err)
	}
	gateHeld := true
	releaseGate := func() {
		if !gateHeld {
			return
		}
		gateHeld = false
		gate.release()
	}
	defer releaseGate()

	notice := &slotNoticeRecorder{began: make(chan struct{})}
	ctx := withDownloadSlotWaitNotice(context.Background(), notice)
	result := f.claimAsync(ctx)
	awaitStagingClaimBegin(t, notice)
	releaseGate()

	res := <-result
	if res.err != nil {
		t.Fatalf("claim after the local metadata gate released: %v", res.err)
	}
	if res.state == nil {
		t.Fatal("claim returned no state after the local metadata gate released")
	}
	res.state.release()
	assertClaimLeaseSignals(t, notice, 1, 1,
		"the claim must return the slot for the metadata-gate wait and take it back for the transfer")
}

// TestStagingClaimLeaseLiveOwnerFailureKeepsSlotReturned pins the claim's
// failure path: a PROVEN live owner holds the OS ownership lock, so no claim
// reaches the caller. The item is about to report the actionable error, so the
// slot must stay returned instead of waiting for an unrelated transfer.
//
// The claim does not tolerate a live owner (unlike the blob-key domain): a
// PROVEN exclusive holder is reported at once, so the failure happens on the
// first contended attempt instead of after the acquisition budget. Only a
// holder that cannot be classified keeps the acquisition waiting, and that is
// a different (uncertain) error; the lease rule asserted here is identical.
func TestStagingClaimLeaseLiveOwnerFailureKeepsSlotReturned(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	f := newStagingClaimTestFixture(t)
	owner, err := acquireStagingOwnershipLock(f.staging)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.release()

	notice := &slotNoticeRecorder{began: make(chan struct{})}
	ctx := withDownloadSlotWaitNotice(context.Background(), notice)
	state, err := f.claim(ctx)
	if state != nil {
		state.release()
		t.Fatalf("claim returned a claim state (%+v) together with its error", state)
	}
	if !errors.Is(err, errBlobIncompleteOwned) {
		t.Fatalf("claim error = %v, want errBlobIncompleteOwned", err)
	}
	assertClaimLeaseSignals(t, notice, 1, 0,
		"a failed claim has nothing to transfer and must not retake the slot")
}

// TestStagingClaimLeaseMetadataGateBudgetFailureKeepsSlotReturned pins the
// failure-by-budget path of a claim wait: the local metadata gate stays held
// for the whole (shortened) acquisition window, so the claim reports the
// staging file as owned, returns no claim, and leaves the item's slot
// returned. Re-taking it would make the actionable failure wait for an
// unrelated transfer.
func TestStagingClaimLeaseMetadataGateBudgetFailureKeepsSlotReturned(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	f := newStagingClaimTestFixture(t)
	gate := &stagingStateLock{path: stagingLockPath(f.meta)}
	if err := gate.acquire(context.Background(), time.Second); err != nil {
		t.Fatalf("hold the local metadata gate: %v", err)
	}
	defer gate.release()

	oldWait := stagingLockWait
	stagingLockWait = 100 * time.Millisecond
	t.Cleanup(func() { stagingLockWait = oldWait })

	notice := &slotNoticeRecorder{began: make(chan struct{})}
	ctx := withDownloadSlotWaitNotice(context.Background(), notice)
	state, err := f.claim(ctx)
	if state != nil {
		state.release()
		t.Fatalf("claim returned a claim state (%+v) together with its error", state)
	}
	if !errors.Is(err, errBlobIncompleteOwned) {
		t.Fatalf("claim error = %v, want errBlobIncompleteOwned", err)
	}
	assertClaimLeaseSignals(t, notice, 1, 0,
		"a claim that exhausted the metadata-gate budget has nothing to transfer and must not retake the slot")
}

// TestDownloadSlotLeaseStagingClaimWait is the download-level case: with
// MaxActiveDownloads=1 and the multipart route selected, the test holds file
// A's in-process staging claim. File B must transfer and complete while A
// waits for that claim - the claim wait must not consume the only transfer
// slot. A must not transfer before the claim is released.
func TestDownloadSlotLeaseStagingClaimWait(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	payloadA := bytes.Repeat([]byte("a"), 4_096)
	payloadB := bytes.Repeat([]byte("b"), 4_096)
	hub := newSlotLeaseHub(t, []slotLeaseFile{
		{rel: "a.bin", body: payloadA},
		{rel: "b.bin", body: payloadB},
	}, false)
	out := t.TempDir()
	dstA := slotLeaseFlatDst(t, out, "a.bin")
	dstB := slotLeaseFlatDst(t, out, "b.bin")

	// The live in-process owner of a.bin's multipart staging path. The flat
	// settings' threshold "0" selects the multipart route even for these small
	// files, so the item's first wait is the staging claim.
	releaseOwner := installStagingOwnerForTest(t, dstA+".incomplete")
	defer releaseOwner()

	cfg := slotLeaseFlatSettings(out, hub.srv.URL)
	cfg.MultipartThreshold = "0"
	done := make(chan string, 8)
	result := runSlotLeaseDownload(context.Background(), cfg, done)

	// b.bin must transfer and complete while a.bin waits for its claim.
	awaitSlotFileDone(t, done, "b.bin")
	assertExactFile(t, dstB, payloadB)

	// a.bin must not have transferred or appeared while its claim is held.
	if _, serr := os.Stat(dstA); !os.IsNotExist(serr) {
		t.Fatalf("a.bin exists while its staging claim is held by another writer, stat err=%v", serr)
	}
	if got := hub.payloadCount("a.bin"); got != 0 {
		t.Fatalf("a.bin issued %d payload requests while its staging claim was held, want 0", got)
	}

	// The owner releases the claim: the waiting item takes its slot back,
	// transfers exactly once, and the job completes.
	releaseOwner()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Download: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Download did not finish after the staging claim was released")
	}
	assertExactFile(t, dstA, payloadA)
	assertExactFile(t, dstB, payloadB)
	if got := hub.payloadCount("a.bin"); got != 1 {
		t.Errorf("a.bin payload requests = %d, want exactly 1", got)
	}
}
