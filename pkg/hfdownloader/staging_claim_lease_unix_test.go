// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !solaris && !aix

package hfdownloader

// The cross-process claim wait: a read-only status probe holds a SHARED lock
// on the persistent staging ownership lock file. The exclusive acquisition
// therefore fails, the non-owning observation cannot prove an exclusive
// holder, and the claim waits out the probe inside its acquisition window. The
// claim must return the item's MaxActiveDownloads slot for that wait and take
// it back before it returns the claim.
//
// A PROVEN exclusive holder is a different case: the claim reports it at once
// as an owned staging file (see TestStagingClaimLeaseLiveOwnerFailureKeepsSlotReturned),
// so it never waits out a live writer - that is the domain protocol's job.

import (
	"context"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestStagingClaimLeaseCrossProcessLockWait(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	f := newStagingClaimTestFixture(t)

	// Create the persistent lock file first: the probe opens it read-only.
	seed, err := acquireStagingOwnershipLock(f.staging)
	if err != nil {
		t.Fatal(err)
	}
	seed.release()

	probe, err := os.Open(stagingOwnershipLockPath(f.staging))
	if err != nil {
		t.Fatalf("open the staging ownership lock file: %v", err)
	}
	defer probe.Close()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatalf("take the status probe's shared ownership lock: %v", err)
	}
	probeHeld := true
	releaseProbe := func() {
		if !probeHeld {
			return
		}
		probeHeld = false
		_ = syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
	}
	defer releaseProbe()

	// Shorten the acquisition budget so an implementation that neither yields
	// the slot nor releases the probe cannot stall the suite. The test releases
	// the probe immediately after the claim observed the contention, far inside
	// this window.
	oldWait := stagingOwnershipAcquireWait
	stagingOwnershipAcquireWait = 2 * time.Second
	t.Cleanup(func() { stagingOwnershipAcquireWait = oldWait })

	// Prove the contention really happened before the probe is released.
	contended := make(chan struct{})
	var once sync.Once
	oldHook := stagingOwnershipContentionHook
	stagingOwnershipContentionHook = func(string) { once.Do(func() { close(contended) }) }
	t.Cleanup(func() { stagingOwnershipContentionHook = oldHook })

	notice := &slotNoticeRecorder{began: make(chan struct{})}
	ctx := withDownloadSlotWaitNotice(context.Background(), notice)
	result := f.claimAsync(ctx)

	select {
	case <-contended:
	case <-time.After(15 * time.Second):
		t.Fatal("the claim never observed the shared staging ownership lock as contended")
	}
	// The contention hook runs AFTER onWait, so the item's slot is already
	// returned while the claim waits.
	releaseProbe()

	res := <-result
	if res.err != nil {
		t.Fatalf("claim after the probe released the lock: %v", res.err)
	}
	if res.state == nil {
		t.Fatal("claim returned no state after the probe released the lock")
	}
	res.state.release()
	assertClaimLeaseSignals(t, notice, 1, 1,
		"the claim must return the slot for the OS-lock wait and take it back for the transfer")
}
