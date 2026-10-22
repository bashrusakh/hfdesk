// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"context"
	"sync"
	"time"
)

// The app and separate rebuild CLI coordinate process-visible effects through
// the byte-range lease on the owned service-root record in
// rebuild_coordination_windows.go. These narrower directory/artifact leases
// preserve in-process writer concurrency; multi-server-process coordination is
// not part of the supported contract.
var windowsBranchVariantPathLeases = struct {
	sync.Mutex
	locks map[string]*windowsBranchVariantPathLease
}{locks: make(map[string]*windowsBranchVariantPathLease)}

type windowsBranchVariantPathLease struct {
	sync.RWMutex
	users int
}

// acquireBranchVariantOSDirectoryLock takes the process-local directory lease.
// The acquisition is cancellable: a bounded reader that competes with a
// long-running writer must be able to give up instead of blocking until that
// writer finishes.
//
// The lease provides mutual exclusion between an exclusive holder and shared
// holders. It does not announce a pending writer, so a new shared lease can
// still be granted while an exclusive waiter polls, and that waiter can be
// delayed by later shared leases. The coordination model does not require
// writer preference, so the retry loop polls the non-blocking TryLock/TryRLock
// forms instead of a plain Lock/RLock call, which would wait for the lock
// without observing the context.
func acquireBranchVariantOSDirectoryLock(ctx context.Context, path string, exclusive bool) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := branchVariantDirectoryKey(path)
	windowsBranchVariantPathLeases.Lock()
	lock := windowsBranchVariantPathLeases.locks[key]
	if lock == nil {
		lock = &windowsBranchVariantPathLease{}
		windowsBranchVariantPathLeases.locks[key] = lock
	}
	lock.users++
	windowsBranchVariantPathLeases.Unlock()

	// drop releases the map reference held by this acquisition. It runs both
	// after a successful lock and after a cancelled attempt, so a waiter that
	// gives up never keeps a lease object alive.
	drop := func() {
		windowsBranchVariantPathLeases.Lock()
		lock.users--
		if lock.users == 0 && windowsBranchVariantPathLeases.locks[key] == lock {
			delete(windowsBranchVariantPathLeases.locks, key)
		}
		windowsBranchVariantPathLeases.Unlock()
	}
	release := func() {
		if exclusive {
			lock.Unlock()
		} else {
			lock.RUnlock()
		}
		drop()
	}
	for {
		if exclusive {
			if lock.TryLock() {
				return release, nil
			}
		} else if lock.TryRLock() {
			return release, nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			drop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
