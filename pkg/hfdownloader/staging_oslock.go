// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// OS-level staging ownership lock (issue #140).
//
// The per-path in-process metadata gate serializes local record updates. The
// lifetime lock is the cross-process fence: a suspended owner keeps it for as
// long as its process and open handle live, regardless of heartbeat age.
//
// This file adds the long-lived exclusivity primitive: an OS-level exclusive
// lock on the persistent sibling file "<staging>.lock", acquired before the
// claim transition and held through transfer, hashing and publish/cleanup. A
// busy lock means a live owner. The kernel releases it when the process exits
// or the handle is closed, so a crash is recoverable immediately, while a
// suspended (but alive) process keeps ownership — the documented tradeoff:
// there is no takeover while the owner is alive; terminate it to recover.
//
// The record and heartbeat remain for identity and diagnostics. Unsupported or
// failed lock acquisition is an operation error; no lease-only mutation path is
// used. Existing partial data remains untouched on that error.
//
// The lock file is never unlinked. Removing it while one process holds the old
// inode and another is about to open the path would let the second process
// lock a fresh inode and create two owners. It is a persistent zero-byte
// artifact of the staging pair and is excluded from cache statistics and
// cleanup.
//
// The lock file serves two roles: a writer holds it exclusively, and a
// read-only status probe touches it briefly. Those two conflict — on Unix a
// probe's shared flock refuses a claimant's exclusive flock, even inside one
// process, because flock locks belong to the open file description.
//
// Only the real lifetime writer may take the EXCLUSIVE lock. Every
// observation — the status probe and the contention classifier alike — must
// stay non-owning, because an observer that took the exclusive lock would
// make every peer (a concurrent claimant, a concurrent status probe) see a
// genuine writer that does not exist. That ambiguity is exactly what turns a
// resumable download into a falsely skipped file.
//
// The observation therefore asks one native question with ONE atomic
// operation: "is an exclusive holder present?" A shared (non-owning) probe
// is granted unless an exclusive holder exists, so:
//
//   - the shared probe is granted => no exclusive holder => not a live
//     writer; the claimant retries its own acquisition (bounded);
//   - the shared probe is refused  => an exclusive holder exists => a real
//     live writer; the claimant skips the file;
//   - the shared probe errors      => the state is unknown => the claimant
//     fails closed with an actionable error.
//
// The observation does NOT distinguish "a reader holds it" from "nobody holds
// it", and it does not claim to: a shared probe is granted in both cases.
// That ambiguity is harmless, because the only decision the claimant needs is
// "exclusive writer or not", and a reader is resolved by simply retrying the
// real acquisition.
//
// Platform support:
//   - Unix with flock: syscall.Flock(LOCK_EX|LOCK_NB).
//   - Windows: syscall.CreateFile with dwShareMode=0 (exclusive share mode),
//     released by closing the handle.
//   - Other platforms and filesystems unable to represent the lock: mutating
//     operations fail safely at runtime, preserving any existing partial. This
//     does not declare permanent platform support policy.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// stagingOwnershipLockPath is the persistent sibling lock file for a staging
// payload. It is derived from the payload path (not the record), so the record
// can be removed and recreated without ever changing the lock inode.
func stagingOwnershipLockPath(staging string) string { return staging + stagingLockSuffix }

var (
	// errStagingOwnershipBusy reports that another open handle (another
	// process, or another open file description) holds the ownership lock
	// EXCLUSIVELY: a live writer owns the staging file. Claimants translate
	// it to errBlobIncompleteOwned; cleanup skips.
	errStagingOwnershipBusy = errors.New("staging ownership lock is held by another live writer")

	// errStagingOwnershipUncertain reports that the exclusive ownership lock
	// could not be acquired and the contention could not be resolved as
	// "no live owner" within the bounded acquisition window. The holder may
	// be a live writer or may have been a reader that never released; the
	// caller cannot tell, so it must not treat the staging file as owned by
	// someone else and skip it. The caller reports an actionable retryable
	// failure instead of a successful skip.
	errStagingOwnershipUncertain = errors.New("staging ownership lock could not be acquired: the holder is not a proven live writer")

	// errStagingOSLockUnsupported reports that this platform or filesystem
	// cannot provide the required OS-level lifetime exclusion.
	errStagingOSLockUnsupported = errors.New("OS staging ownership lock is unsupported here")
)

// stagingLockHolderKind is what a single non-owning observation can prove
// about the staging ownership lock.
//
// The observation is deliberately one-way. A shared (non-owning) probe is
// granted unless an EXCLUSIVE holder exists, so it can prove the presence of
// a live writer but cannot prove the absence of every other kind of holder.
// Anything more specific would require an exclusive probe, and an exclusive
// probe is itself a writer as far as every peer is concerned: it would make
// concurrent claimants and status probes see a live writer that does not
// exist. So the observation stops at what it can actually prove.
type stagingLockHolderKind int

const (
	// stagingLockHolderExclusive means an EXCLUSIVE holder was observed: a
	// real live writer owns the staging file and must not be displaced.
	stagingLockHolderExclusive stagingLockHolderKind = iota
	// stagingLockHolderNotExclusive means NO exclusive holder was observed.
	// The lock may be held by a reader (a status probe) or by nobody at
	// all; the observation cannot and does not distinguish the two. The
	// claimant must resolve it by attempting the real acquisition.
	stagingLockHolderNotExclusive
	// stagingLockHolderUnknown means the observation itself failed, so the
	// state is unknown. The caller must fail closed.
	stagingLockHolderUnknown
)

// stagingOwnershipLock is one held OS-level ownership lock.
type stagingOwnershipLock struct {
	path     string
	handle   stagingOSLockHandle
	released bool
}

// acquireStagingOwnershipLock takes the non-blocking exclusive ownership lock
// for staging, creating the persistent lock file if needed. It returns
// errStagingOwnershipBusy when a live owner holds it and
// errStagingOSLockUnsupported when this platform/filesystem cannot represent
// the lock.
func acquireStagingOwnershipLock(staging string) (*stagingOwnershipLock, error) {
	path := stagingOwnershipLockPath(staging)
	h, err := openStagingOSLock(path)
	if err != nil {
		return nil, err
	}
	return &stagingOwnershipLock{path: path, handle: h}, nil
}

// stagingOwnershipAcquire is the single mutation-side capability provider.
// Keeping it replaceable lets package tests exercise lock-open failures without
// relying on root-sensitive filesystem permissions.
var stagingOwnershipAcquire = acquireStagingOwnershipLock

// stagingOwnershipAcquireWait bounds how long a claimant waits for a
// contended ownership lock before it must decide what the contention meant.
//
// The wait exists because the lock file serves two purposes: a real writer
// holds it exclusively, and a read-only status probe touches it briefly. A
// non-owning observation tells the claimant whether an EXCLUSIVE holder
// exists, but a reader may still be mid-probe when the claimant looks, so the
// claimant waits for the reader to finish instead of deciding from a single
// observation.
//
// The bound is short and finite: a suspended owner keeps the lock
// indefinitely, so waiting forever would hang the download instead of
// reporting a usable outcome.
var stagingOwnershipAcquireWait = 5 * time.Second

// stagingOwnershipRetryDelay is the pause between two acquisition attempts
// while the lock stays contended.
var stagingOwnershipRetryDelay = 25 * time.Millisecond

// stagingOwnershipObserveHolder is the non-owning holder observation. It is a
// seam so package tests can exercise the contention handling independently of
// host filesystem capabilities.
var stagingOwnershipObserveHolder = observeStagingOSLockHolder

// observeStagingOSLockHolder reports whether an EXCLUSIVE holder owns the
// lock file, using the existing NON-OWNING status probe.
//
// The status probe is exactly the right primitive: a shared (read-only)
// probe is granted unless an exclusive holder exists, so
//
//   - the probe is REFUSED => an exclusive holder exists => a real live
//     writer; and
//   - the probe is GRANTED => no exclusive holder exists.
//
// A granted probe cannot distinguish "a reader holds it" from "nobody holds
// it", and it does not need to: the claimant resolves that case by
// attempting the real acquisition, which is the only operation allowed to
// take the exclusive lock.
//
// The observation deliberately never takes the exclusive lock. An exclusive
// observer is itself an exclusive holder, so every peer — a concurrent
// claimant or a concurrent status probe — would see a live writer that does
// not exist and would skip files nobody is downloading.
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

// stagingOwnershipContentionHook is called with the staging path whenever an
// acquisition finds the ownership lock busy. It is a test seam: it lets a
// regression prove that the contention really happened while a probe held the
// lock, instead of inferring it from a sleep.
var stagingOwnershipContentionHook func(staging string)

// acquireStagingOwnershipLockCtx takes the exclusive ownership lock for
// staging, waiting out transient contention within a bounded, context-aware
// window.
//
// The native lock is still the only authority: the claimant never mutates
// anything without holding it. The retry loop only decides WHEN to attempt
// the acquisition, and it uses a single non-owning observation to decide what
// a busy lock means:
//
//   - An EXCLUSIVE holder was observed: a real live writer owns the staging
//     file. The claimant receives errStagingOwnershipBusy and the caller
//     skips the file, exactly as before. A writer may be suspended, so its
//     lock is never taken over.
//   - NO exclusive holder was observed: the lock is held by a reader (a
//     status probe) or by nobody. The claimant waits and retries its own
//     acquisition, which is the only operation allowed to take the exclusive
//     lock. A probe is not a writer, so treating it as one would skip a file
//     that nobody is downloading.
//   - The observation failed: the state is unknown, so the claimant keeps
//     waiting inside the window and reports an uncertain failure if it never
//     resolves. It must NOT report a successful skip, because no live writer
//     is proven to exist.
//
// A cancellation ends the wait promptly with ctx.Err().
func acquireStagingOwnershipLockCtx(ctx context.Context, staging string) (*stagingOwnershipLock, error) {
	return acquireStagingOwnershipLockWithin(ctx, staging, stagingOwnershipAcquireWait)
}

// acquireStagingOwnershipLockWithin is acquireStagingOwnershipLockCtx with an
// explicit contention window. Cleanup uses a shorter window because it is
// best-effort and must not delay an explicit cancel.
func acquireStagingOwnershipLockWithin(ctx context.Context, staging string, wait time.Duration) (*stagingOwnershipLock, error) {
	deadline := time.Now().Add(wait)
	for {
		lock, err := stagingOwnershipAcquire(staging)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, errStagingOwnershipBusy) {
			return nil, err
		}
		// The lock is busy. Observe it WITHOUT taking the exclusive lock: an
		// exclusive observer would itself look like a live writer to every
		// peer and would make them skip files nobody is downloading.
		if stagingOwnershipContentionHook != nil {
			stagingOwnershipContentionHook(staging)
		}
		kind, oerr := stagingOwnershipObserveHolder(stagingOwnershipLockPath(staging))
		switch {
		case oerr != nil:
			// The observation failed, so the state is unknown. It may be a
			// live writer, so the safe outcome is to keep waiting inside the
			// window and report an uncertain failure if it never resolves.
			log.Printf("warning: cannot observe the holder of the staging ownership lock %s: %v", staging, oerr)
		case kind == stagingLockHolderExclusive:
			// A proven live writer owns this staging file.
			return nil, errStagingOwnershipBusy
		default:
			// No exclusive holder: a reader (status probe) or nobody. The
			// real acquisition is the only thing that can resolve it, so
			// wait and retry it.
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// The window closed with the lock still held and no exclusive
			// holder observed. The holder is a reader that never released,
			// or the state stayed unknown: no live writer is proven, so the
			// caller must not report a successful skip. The message names
			// the retry action so the failure is actionable.
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

// held reports whether this handle still represents a held ownership lock.
func (l *stagingOwnershipLock) held() bool { return l != nil && !l.released }

// stagingOwnershipBusy probes whether staging's ownership lock is currently
// held. It never blocks and never creates the lock file, so a read-only status
// query does not mutate the cache. A missing lock file or an unsupported
// platform is reported as not busy.
func stagingOwnershipBusy(staging string) (bool, error) {
	return probeStagingOSLock(stagingOwnershipLockPath(staging))
}

// stagingOwnershipStatusProbe is injectable so CheckBlob's fail-closed status
// behavior can be tested independently of host filesystem capabilities.
var stagingOwnershipStatusProbe = stagingOwnershipBusy
