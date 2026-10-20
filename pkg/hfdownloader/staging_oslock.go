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
// Platform support:
//   - Unix with flock: syscall.Flock(LOCK_EX|LOCK_NB).
//   - Windows: syscall.CreateFile with dwShareMode=0 (exclusive share mode),
//     released by closing the handle.
//   - Other platforms and filesystems unable to represent the lock: mutating
//     operations fail safely at runtime, preserving any existing partial. This
//     does not declare permanent platform support policy.

import (
	"errors"
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
	// cannot provide the required OS-level lifetime exclusion.
	errStagingOSLockUnsupported = errors.New("OS staging ownership lock is unsupported here")
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
