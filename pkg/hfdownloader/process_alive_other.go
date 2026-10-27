// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !unix && !windows

package hfdownloader

// Without a reliable liveness probe, a positive PID remains possibly alive.
// Preserve unproven owners rather than inferring death from age.
func isProcessAlive(pid int) bool {
	return pid > 0
}
