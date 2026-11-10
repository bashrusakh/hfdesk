// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package service

// currentPublishedStateScope reports no session scope: the desktop host
// that publishes claims exists on Windows, so a platform without it has no
// session to name and keeps the plain per-user file name — deterministic,
// and a path no launch ever publishes on this platform.
func currentPublishedStateScope() string {
	return ""
}
