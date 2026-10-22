// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

// Package desktop provides the Windows desktop host of HFDesk.
//
// The desktop host is a second binary of HFDesk. It runs the very same
// server as the CLI host, in the same process: it does not spawn the CLI
// binary and it does not implement a second server. On top of the server it
// adds the desktop shell:
//
//   - a system tray icon with the actions of the host,
//   - a small Runtime / Logs window that renders the runtime page of the
//     running server,
//   - the single instance lock that keeps one desktop host per interactive
//     user session,
//   - the failure reporting a windowless process cannot do: every startup
//     problem is shown in a native message box.
//
// The desktop specific dependencies live behind Windows build tags, so the
// headless builds for Linux, macOS and Docker never contain them. The
// package itself builds everywhere: on a platform without a desktop host Run
// reports that the desktop mode is only supported on Windows.
package desktop

import (
	"errors"

	"github.com/bashrusakh/hfdesk/internal/app"
)

// ErrUnsupportedPlatform is returned by Run on a platform that has no
// desktop host.
var ErrUnsupportedPlatform = errors.New("desktop: the desktop mode is only supported on Windows")

// Options configures the desktop host.
type Options struct {
	// App holds the shared startup options.
	//
	// The desktop host uses the shared configuration assembly and the
	// shared server startup, so it behaves like the CLI host: the same
	// flags, the same configuration file and the same precedence.
	App app.Options

	// Version is the build version of the binary. It is reported through
	// the API and shown in the Runtime window.
	Version string
}
