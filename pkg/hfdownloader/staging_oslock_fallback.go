// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && (!unix || solaris || aix)

package hfdownloader

// Platforms without a stdlib exclusive-file-lock primitive cannot safely
// mutate shared ranged staging state. Claim/cleanup fail closed.

// stagingOSLockAvailable reports that this platform implements the OS
// ownership lock.
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
