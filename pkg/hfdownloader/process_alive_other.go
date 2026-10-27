// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !unix && !windows

package hfdownloader

// isProcessAlive has no reliable implementation on this platform. Treat a
// positive PID as possibly alive so ownership decisions never displace a live
// owner on an uncertain answer. A same-host owner whose death cannot be proven
// is preserved and refused as unproven rather than recovered by age.
func isProcessAlive(pid int) bool {
	return pid > 0
}
