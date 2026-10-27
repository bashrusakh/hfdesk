// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// The persistent destination-domain lock is the cross-process ownership fence;
// records retain identity and resume state. The lock file is never unlinked,
// since replacing its inode could create two simultaneous owners. Observers
// use a non-owning shared probe: it detects an exclusive writer but cannot
// distinguish a reader from no holder, so claimants retry acquisition. Unix
// uses flock, Windows uses exclusive CreateFile sharing; unsupported lock
// operations fail closed without changing partial data.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// stagingOwnershipLockPath appends the persistent .lock suffix to the
// caller-selected lock source; records can change without replacing this inode.
func stagingOwnershipLockPath(staging string) string { return staging + stagingLockSuffix }

var (
	// errStagingOwnershipBusy means exclusive acquisition was refused; observing
	// the holder distinguishes a writer from shared readers.
	errStagingOwnershipBusy = errors.New("staging ownership lock is held by another live writer")

	// errStagingOwnershipUncertain means a failed acquisition could not establish
	// whether a live writer owns the lock; callers must fail closed.
	errStagingOwnershipUncertain = errors.New("staging ownership lock could not be acquired: the holder is not a proven live writer")

	// errStagingOSLockUnsupported reports that this platform or filesystem
	// cannot provide the required OS-level lifetime exclusion.
	errStagingOSLockUnsupported = errors.New("OS staging ownership lock is unsupported here")
)

type stagingLockHolderKind int

const (
	// stagingLockHolderExclusive means a live writer was observed.
	stagingLockHolderExclusive stagingLockHolderKind = iota
	// stagingLockHolderNotExclusive is inconclusive; acquisition must be retried.
	stagingLockHolderNotExclusive
	// stagingLockHolderUnknown means observation failed; callers fail closed.
	stagingLockHolderUnknown
)

type stagingOwnershipLock struct {
	path     string
	handle   stagingOSLockHandle
	released bool
}

// acquireStagingOwnershipLock creates/opens the persistent source lock and
// attempts non-blocking exclusive acquisition. Busy requires holder observation;
// unsupported exclusion returns errStagingOSLockUnsupported.
func acquireStagingOwnershipLock(staging string) (*stagingOwnershipLock, error) {
	path := stagingOwnershipLockPath(staging)
	h, err := openStagingOSLock(path)
	if err != nil {
		return nil, err
	}
	return &stagingOwnershipLock{path: path, handle: h}, nil
}

// stagingOwnershipAcquire is replaceable so tests can simulate lock failures.
var stagingOwnershipAcquire = acquireStagingOwnershipLock

// stagingOwnershipAcquireWait is the base unit for the destination wait budget.
var stagingOwnershipAcquireWait = 5 * time.Second

var stagingOwnershipRetryDelay = 25 * time.Millisecond

// stagingOwnershipObserveHolder is replaceable for platform-independent tests.
var stagingOwnershipObserveHolder = observeStagingOSLockHolder

// observeStagingOSLockHolder uses a shared, non-owning probe. Refusal proves
// an exclusive writer; success is resolved by retrying acquisition (it may
// mean another reader or no holder). An exclusive probe would impersonate a writer.
func observeStagingOSLockHolder(path string) (stagingLockHolderKind, error) {
	exclusive, err := probeStagingOSLock(path)
	if err != nil {
		return stagingLockHolderUnknown, err
	}
	if exclusive {
		return stagingLockHolderExclusive, nil
	}
	return stagingLockHolderNotExclusive, nil
}

// stagingOwnershipContentionHook lets tests observe a contended acquisition.
var stagingOwnershipContentionHook func(staging string)

// acquireStagingOwnershipLockWithin retries boundedly and cancellably. A
// proven exclusive holder returns busy immediately; otherwise retry real
// acquisition and return uncertain if the window expires without proof.
func acquireStagingOwnershipLockWithin(ctx context.Context, staging string, wait time.Duration) (*stagingOwnershipLock, error) {
	return acquireStagingOwnershipLockWithinMode(ctx, staging, wait, false, nil)
}

// acquireStagingOwnershipLockWaiting waits within the budget even for a
// proven live owner. onWait signals contention so an item can return its
// transfer slot; the free path does not signal.
func acquireStagingOwnershipLockWaiting(ctx context.Context, staging string, wait time.Duration, onWait func()) (*stagingOwnershipLock, error) {
	return acquireStagingOwnershipLockWithinMode(ctx, staging, wait, true, onWait)
}

func acquireStagingOwnershipLockWithinMode(ctx context.Context, staging string, wait time.Duration, tolerateLiveOwner bool, onWait func()) (*stagingOwnershipLock, error) {
	deadline := time.Now().Add(wait)
	// The last observation selects busy versus uncertain at timeout.
	liveOwner := false
	for {
		lock, err := stagingOwnershipAcquire(staging)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, errStagingOwnershipBusy) {
			return nil, err
		}

		if onWait != nil {
			onWait()
		}
		// An exclusive observer would impersonate a writer; use the non-owning probe.
		if stagingOwnershipContentionHook != nil {
			stagingOwnershipContentionHook(staging)
		}
		kind, oerr := stagingOwnershipObserveHolder(stagingOwnershipLockPath(staging))
		switch {
		case oerr != nil:
			// Retry unknown observations within the window; unresolved state fails uncertain.
			log.Printf("warning: cannot observe the holder of the staging ownership lock %s: %v", staging, oerr)
			liveOwner = false
		case kind == stagingLockHolderExclusive:

			liveOwner = true
			if !tolerateLiveOwner {
				return nil, errStagingOwnershipBusy
			}
			// Wait for release rather than treating ownership as completed content.
		default:
			// A shared reader and no holder are indistinguishable; retry exclusive acquisition.
			liveOwner = false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if liveOwner {

				return nil, errStagingOwnershipBusy
			}
			// No exclusive writer was proven before timeout; fail uncertain, never skip.
			return nil, fmt.Errorf("%w: %s is still locked after %v; retry the download", errStagingOwnershipUncertain, staging, wait)
		}
		pause := stagingOwnershipRetryDelay
		if pause > remaining {
			pause = remaining
		}
		if !sleepCtx(ctx, pause) {
			return nil, ctx.Err()
		}
	}
}

// release closes the platform handle, which drops the OS lock. It never
// unlinks the lock file (see the package comment in this file).
func (l *stagingOwnershipLock) release() {
	if l == nil || l.released {
		return
	}
	l.released = true
	l.handle.release()
}

func (l *stagingOwnershipLock) held() bool { return l != nil && !l.released }

// stagingOwnershipBusy probes without blocking or creating a lock file.
// A missing file is not busy; unsupported or failed probes return an error,
// not proof that the destination is free.
func stagingOwnershipBusy(staging string) (bool, error) {
	return probeStagingOSLock(stagingOwnershipLockPath(staging))
}

// stagingOwnershipStatusProbe is injectable so CheckBlob's fail-closed status
// behavior can be tested independently of host filesystem capabilities.
var stagingOwnershipStatusProbe = stagingOwnershipBusy
