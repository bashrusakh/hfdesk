// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"sync"
	"time"
)

// processLocalCoordinationLeases holds the in-process half of the
// coordination lease.
//
// The OS-private carrier adds the cross-process half of the lease. This
// registry always holds the in-process half, so two operations of one
// process exclude each other even when the carrier is unavailable, and the
// coordination keeps one consistent meaning in both cases.
var processLocalCoordinationLeases = struct {
	sync.Mutex
	leases map[string]*processLocalCoordinationLease
}{leases: map[string]*processLocalCoordinationLease{}}

type processLocalCoordinationLease struct {
	sync.RWMutex
	users int
}

// acquireProcessLocalCoordinationLease takes the in-process half of one
// coordination lease.
//
// Shared leases coexist; an exclusive lease waits for the shared holders and
// excludes later shared holders. The wait observes the context, so a caller
// that competes with a long transfer can give up instead of staying blocked.
func acquireProcessLocalCoordinationLease(ctx context.Context, key string, exclusive bool) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	processLocalCoordinationLeases.Lock()
	lease := processLocalCoordinationLeases.leases[key]
	if lease == nil {
		lease = &processLocalCoordinationLease{}
		processLocalCoordinationLeases.leases[key] = lease
	}
	lease.users++
	processLocalCoordinationLeases.Unlock()

	// drop releases the map reference held by this acquisition. It runs both
	// after a successful lock and after a cancelled attempt, so a waiter that
	// gives up never keeps a lease object alive.
	drop := func() {
		processLocalCoordinationLeases.Lock()
		lease.users--
		if lease.users == 0 && processLocalCoordinationLeases.leases[key] == lease {
			delete(processLocalCoordinationLeases.leases, key)
		}
		processLocalCoordinationLeases.Unlock()
	}
	release := func() {
		if exclusive {
			lease.Unlock()
		} else {
			lease.RUnlock()
		}
		drop()
	}
	for {
		if exclusive {
			if lease.TryLock() {
				return release, nil
			}
		} else if lease.TryRLock() {
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

// processLocalCoordinationKey names the in-process half of one lease. The
// purpose keeps the data lease and the admission lease of the same root
// apart, exactly as the two carrier objects do.
func processLocalCoordinationKey(identity string, admission bool) string {
	if admission {
		return identity + "\x00admission"
	}
	return identity + "\x00data"
}
