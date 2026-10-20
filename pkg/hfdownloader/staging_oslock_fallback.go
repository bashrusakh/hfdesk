// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && (!unix || solaris || aix)

package hfdownloader

// Platforms without a stdlib exclusive-file-lock primitive (plan9, js/wasm,
// Solaris/AIX) keep the original transition-lock/heartbeat lease model. The
// claim/heartbeat freshness checks in staging_identity.go remain the ownership
// decision, and the weaker guarantee (no fencing of in-flight payload writes)
// is documented on the package.

// stagingOSLockAvailable reports that this platform implements the OS
// ownership lock.
const stagingOSLockAvailable = false

// stagingOSLockHandle is never created on this platform.
type stagingOSLockHandle struct{}

// openStagingOSLock always reports the fallback so the caller uses the lease.
func openStagingOSLock(string) (stagingOSLockHandle, error) {
	return stagingOSLockHandle{}, errStagingOSLockUnsupported
}

func (stagingOSLockHandle) release() {}

// probeStagingOSLock cannot detect a holder without an OS lock.
func probeStagingOSLock(string) (bool, error) { return false, nil }
