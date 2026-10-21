// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"os/exec"
	"runtime"
)

// OpenBrowser opens the application URL in the user's default browser.
//
// It is the URL of the running server, so it always points at the port the
// server is really listening on.
func (a *App) OpenBrowser() error {
	return OpenBrowser(a.URL())
}

// OpenBrowser opens url in the user's default browser.
//
// It is exported so a host can open an arbitrary URL if it ever needs to.
//
// The browser is started, not waited for, so opening it never blocks the
// server.
func OpenBrowser(url string) error {
	cmd := browserCommand(url)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	// Reap the helper process, so it does not stay behind as a zombie.
	go func() { _ = cmd.Wait() }()
	return nil
}

// browserCommand returns the command that opens a URL in the default
// browser of the current platform.
func browserCommand(url string) *exec.Cmd {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		return exec.Command("open", url)
	default:
		return exec.Command("xdg-open", url)
	}
}
