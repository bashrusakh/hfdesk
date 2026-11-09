// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build freebsd || dragonfly

package hfdownloader

import "syscall"

// assignOwnershipRlimit writes both rlimit fields from this package's uint64
// boundary representation. syscall.Rlimit's fields are signed (int64) on
// freebsd and dragonfly, so each value converts at this one boundary; the
// conversion round-trips exactly because uint64/int64 conversions are modular
// inverses — every value written here was read from an int64 field or is a
// small non-negative literal.
func assignOwnershipRlimit(limit *syscall.Rlimit, soft, hard uint64) {
	limit.Cur = int64(soft)
	limit.Max = int64(hard)
}
