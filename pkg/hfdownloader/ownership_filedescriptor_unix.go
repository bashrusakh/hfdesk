// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !solaris && !aix

package hfdownloader

import (
	"errors"
	"syscall"
)

// currentOwnershipFileLimit reports the process NOFILE soft and hard limits.
// Every rlimit field converts to uint64 at this boundary: syscall.Rlimit's
// fields are unsigned on most GOOSes in this build tag but signed (int64) on
// freebsd and dragonfly.
func currentOwnershipFileLimit() (soft, hard uint64, ok bool) {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return 0, 0, false
	}
	return uint64(limit.Cur), uint64(limit.Max), true
}

// setOwnershipSoftFileLimit moves the soft limit within the existing hard
// limit; the hard limit itself is never changed here. Both fields are written
// through the per-sign assignOwnershipRlimit shim so this file compiles on
// every GOOS in its build tag; the hard limit is written back unchanged
// (uint64/int64 conversions round-trip exactly).
func setOwnershipSoftFileLimit(soft uint64) error {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return err
	}
	assignOwnershipRlimit(&limit, soft, uint64(limit.Max))
	return syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit)
}

// ownershipFileDescriptorPressure identifies the per-process descriptor
// exhaustion EMFILE a tree admission can hit. ENFILE is system-wide rather
// than this process's soft/hard limit, so it keeps its original identity.
func ownershipFileDescriptorPressure(err error) bool {
	return errors.Is(err, syscall.EMFILE)
}
