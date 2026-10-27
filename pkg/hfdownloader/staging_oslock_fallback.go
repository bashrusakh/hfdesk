// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && (!unix || solaris || aix)

package hfdownloader

// Without an OS ownership lock, destination acquisition and cleanup fail closed.

// stagingOSLockAvailable is false: this platform has no ownership-lock implementation.
const stagingOSLockAvailable = false

// stagingOSLockHandle is never created on this platform.
type stagingOSLockHandle struct{}

// openStagingOSLock reports that safe lifetime exclusion is unavailable.
func openStagingOSLock(string) (stagingOSLockHandle, error) {
	return stagingOSLockHandle{}, errStagingOSLockUnsupported
}

func (stagingOSLockHandle) release() {}

// probeStagingOSLock cannot detect a holder without an OS lock.
func probeStagingOSLock(string) (bool, error) { return false, errStagingOSLockUnsupported }
