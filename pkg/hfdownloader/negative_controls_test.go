// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Negative controls for the ownership and publication regressions (issue #140
// / PR #151).
//
// A negative control installs the historical behavior through the production
// seam and asserts that the defect IS observed. It is the proof that the
// positive regressions are discriminating: with the seam installed the same
// scenario fails, without it the scenario passes.
//
// Two historical behaviors are covered:
//
//   - the exclusive observer: the holder was classified by TAKING the
//     exclusive lock, which made every peer see a live writer that did not
//     exist, so a file nobody was downloading was reported as owned by a live
//     writer;
//   - the stat-then-rename publication: the "the blob does not exist yet"
//     decision and the placement that followed it were two separate steps, so
//     a concurrent writer's blob could be replaced by bytes that do not hash
//     to the key they are stored under.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestNegativeControlExclusiveObserverInventsALiveWriter is the negative
// control for the non-owning ownership observation.
//
// The historical observer took the EXCLUSIVE lock to classify the holder. A
// peer that holds the lock therefore looks like a live writer, and the claim
// reports a live owner although nobody is downloading the file.
//
// Under the job contract that invented verdict is not a successful skip
// anymore - the item fails with the actionable live-owner error - but it is
// still a FALSE verdict: the control asserts that the claim invents the live
// writer, which is the defect the non-owning observation removes.
func TestNegativeControlExclusiveObserverInventsALiveWriter(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	oldObserve := stagingOwnershipObserveHolder
	defer func() { stagingOwnershipObserveHolder = oldObserve }()
	// The historical observer: classify the holder by taking the exclusive
	// lock, which is itself an exclusive holder for every peer.
	stagingOwnershipObserveHolder = func(lockPath string) (stagingLockHolderKind, error) {
		staging := strings.TrimSuffix(lockPath, stagingLockSuffix)
		own, err := acquireStagingOwnershipLock(staging)
		if err != nil {
			// The exclusive acquisition failed: somebody holds the lock.
			return stagingLockHolderExclusive, nil
		}
		own.release()
		return stagingLockHolderNotExclusive, nil
	}

	f := newContentionDownloadFixture(t)
	// A peer that holds the ownership lock for the whole run, exactly like a
	// concurrent observer of the historical implementation.
	release := make(chan struct{})
	peerReady := make(chan struct{})
	go func() {
		own, err := acquireStagingOwnershipLock(f.staging)
		if err != nil {
			return
		}
		close(peerReady)
		<-release
		own.release()
	}()
	select {
	case <-peerReady:
	case <-time.After(30 * time.Second):
		t.Fatal("the peer never took the ownership lock")
	}

	out := f.runDownload(t, context.Background())
	close(release)

	if len(out.skips) != 0 {
		t.Fatalf("the negative control did not reproduce the invented live writer: the job reported a successful skip: %v", out.skips)
	}
	if !errors.Is(out.err, errBlobOwnedByLiveWriter) {
		t.Fatalf("the negative control did not reproduce the invented live writer: err=%v", out.err)
	}
	if _, serr := os.Stat(f.dst); serr == nil {
		t.Error("the file was downloaded although the claim reported a live owner")
	}
	t.Logf("negative control reproduced the invented live writer: %v", out.err)
}
