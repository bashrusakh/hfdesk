// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !solaris && !aix && !freebsd && !dragonfly

package hfdownloader

import "syscall"

// assignOwnershipRlimit writes both rlimit fields from this package's uint64
// boundary representation. On this platform family syscall.Rlimit's fields are
// unsigned, so the values pass through unchanged.
func assignOwnershipRlimit(limit *syscall.Rlimit, soft, hard uint64) {
	limit.Cur = soft
	limit.Max = hard
}
