// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"runtime"
	"strings"
	"testing"
)

// TestBrowserCommandUsesThePlatformOpener proves that the browser is opened
// with the opener of the current platform.
func TestBrowserCommandUsesThePlatformOpener(t *testing.T) {
	cmd := browserCommand("http://127.0.0.1:8080")

	switch runtime.GOOS {
	case "windows":
		if len(cmd.Args) < 3 || cmd.Args[0] != "rundll32" || cmd.Args[1] != "url.dll,FileProtocolHandler" {
			t.Errorf("command = %v, want the windows protocol handler", cmd.Args)
		}
	case "darwin":
		if len(cmd.Args) < 2 || cmd.Args[0] != "open" {
			t.Errorf("command = %v, want the mac opener", cmd.Args)
		}
	default:
		if len(cmd.Args) < 2 || cmd.Args[0] != "xdg-open" {
			t.Errorf("command = %v, want xdg-open", cmd.Args)
		}
	}

	if len(cmd.Args) == 0 || cmd.Args[len(cmd.Args)-1] != "http://127.0.0.1:8080" {
		t.Errorf("command = %v, want it to carry the url", cmd.Args)
	}
}

// TestBrowserCommandCarriesTheURL proves that the URL is passed to the
// opener unchanged, so the running server is opened and not a rebuilt one.
func TestBrowserCommandCarriesTheURL(t *testing.T) {
	const url = "http://127.0.0.1:34567"
	cmd := browserCommand(url)

	if !strings.Contains(strings.Join(cmd.Args, " "), url) {
		t.Errorf("command = %v, want it to carry %q", cmd.Args, url)
	}
}
