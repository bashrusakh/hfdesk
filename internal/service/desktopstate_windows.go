// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import "fmt"

// currentPublishedStateScope is the session scope the claim file name
// carries on Windows: the user and the session of this process — the same
// identity the per-session desktop singleton is named with, so the published
// claim of a desktop and the singleton it holds belong to one session.
//
// A session identity that cannot be read gets an explicit unscoped marker
// instead of a fallback to the shared per-user name: no launch, hold or
// command ever reaches a claim with it — the desktop singleton cannot be
// named either, which ends them first — so the marker only keeps the failure
// visible rather than silently reading a file another session may own.
func currentPublishedStateScope() string {
	sid, session, err := desktopSessionIdentity()
	if err != nil {
		return "unscoped"
	}
	return fmt.Sprintf("%s.%d", sid, session)
}
