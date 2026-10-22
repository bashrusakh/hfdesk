// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"context"
	"sync"
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

func acquireBranchVariantOSDirectoryLock(ctx context.Context, path string, exclusive bool) (func(), error) {
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
	if exclusive {
		lock.Lock()
	} else {
		lock.RLock()
	}
	return func() {
		if exclusive {
			lock.Unlock()
		} else {
			lock.RUnlock()
		}
		windowsBranchVariantPathLeases.Lock()
		lock.users--
		if lock.users == 0 && windowsBranchVariantPathLeases.locks[key] == lock {
			delete(windowsBranchVariantPathLeases.locks, key)
		}
		windowsBranchVariantPathLeases.Unlock()
	}, nil
}
