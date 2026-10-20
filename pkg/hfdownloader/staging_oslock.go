// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// OS-level staging ownership lock (issue #140).
//
// The sibling O_EXCL transition lock in staging_lock.go serializes short
// record read-modify-write transitions. It is a mutex, not an ownership fence:
// a writer that already holds the claim record keeps writing through its open
// payload handle while the record is fresh, and a suspended writer that stops
// heartbeating can resume through that same handle after another process takes
// the record over. A lease alone therefore cannot exclude in-flight payload
// writes.
//
// This file adds the long-lived exclusivity primitive: an OS-level exclusive
// lock on the persistent sibling file "<staging>.lock", acquired before the
// claim transition and held through transfer, hashing and publish/cleanup. A
// busy lock means a live owner. The kernel releases it when the process exits
// or the handle is closed, so a crash is recoverable immediately, while a
// suspended (but alive) process keeps ownership — the documented tradeoff:
// there is no takeover while the owner is alive; terminate it to recover.
//
// The record and heartbeat remain for identity, diagnostics and the fallback
// platforms, and every record mutation still runs under the transition lock.
//
// The lock file is never unlinked. Removing it while one process holds the old
// inode and another is about to open the path would let the second process
// lock a fresh inode and create two owners. It is a persistent zero-byte
// artifact of the staging pair and is excluded from cache statistics and
// cleanup.
//
// Platform support:
//   - Unix with flock: syscall.Flock(LOCK_EX|LOCK_NB).
//   - Windows: syscall.CreateFile with dwShareMode=0 (exclusive share mode),
//     released by closing the handle.
//   - Other platforms, and filesystems that cannot represent the lock: the
//     platform probe reports "unsupported" and that file keeps the original
//     transition-lock/heartbeat lease model — a weaker guarantee documented on
//     the package.

import (
	"errors"
	"log"
	"sync"
)

// stagingOwnershipLockPath is the persistent sibling lock file for a staging
// payload. It is derived from the payload path (not the record), so the record
// can be removed and recreated without ever changing the lock inode.
func stagingOwnershipLockPath(staging string) string { return staging + stagingLockSuffix }

var (
	// errStagingOwnershipBusy reports that another open handle (another
	// process, or another open file description) holds the ownership lock.
	// Claimants translate it to errBlobIncompleteOwned; cleanup skips.
	errStagingOwnershipBusy = errors.New("staging ownership lock is held by another live writer")

	// errStagingOSLockUnsupported reports that this platform or filesystem
	// cannot provide the OS lock, so the caller must degrade to the
	// transition-lock/heartbeat lease for this file.
	errStagingOSLockUnsupported = errors.New("OS staging ownership lock is unsupported here")

	stagingOwnershipFallbackOnce sync.Once
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

// logStagingOwnershipFallback records once that a file is running with the
// weaker lease-only guarantee (unsupported platform or filesystem).
func logStagingOwnershipFallback(staging string) {
	stagingOwnershipFallbackOnce.Do(func() {
		log.Printf("warning: OS staging ownership lock unavailable for %s; "+
			"falling back to the transition-lock/heartbeat lease (no fencing of in-flight writes)", staging)
	})
}
