// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !unix && !windows

package hfdownloader

// isProcessAlive has no reliable implementation on this platform. Treat a
// positive PID as possibly alive so ownership decisions fall back to heartbeat
// freshness: a live owner is never displaced on an uncertain answer, while a
// genuinely stale/frozen owner is still recovered after the grace window.
func isProcessAlive(pid int) bool {
	return pid > 0
}
