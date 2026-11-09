// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package desktop

// Run reports that the desktop mode is only supported on Windows.
//
// The desktop host needs the system tray and the WebView2 window of
// Windows, so it has no counterpart on the other platforms. The headless
// binary of cmd/hfdesk is the host there.
func Run(o Options) error {
	return ErrUnsupportedPlatform
}
