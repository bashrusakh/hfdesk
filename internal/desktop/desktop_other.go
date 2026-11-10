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

// RunServiceAction reports that the elevated service helper is only supported
// on Windows.
//
// The helper drives the service control manager of Windows, so the other
// platforms have nothing it could perform. The service of those platforms is
// driven by the service command of the headless binary.
func RunServiceAction(_ ServiceActionOptions) error {
	return ErrUnsupportedServiceAction
}
