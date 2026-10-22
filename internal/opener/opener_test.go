// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package opener

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// TestCommandForUsesThePlatformOpener proves that every platform opens a
// path with its own default handler, and that the path is handed over
// unchanged.
func TestCommandForUsesThePlatformOpener(t *testing.T) {
	const path = "/home/user/.cache/HFDesk/logs"

	cases := []struct {
		goos     string
		wantName string
	}{
		{goos: "windows", wantName: "explorer.exe"},
		{goos: "darwin", wantName: "open"},
		{goos: "linux", wantName: "xdg-open"},
		{goos: "freebsd", wantName: "xdg-open"},
	}

	for _, c := range cases {
		t.Run(c.goos, func(t *testing.T) {
			name, args := commandFor(c.goos, path)
			if name != c.wantName {
				t.Errorf("command = %q, want %q", name, c.wantName)
			}
			if !reflect.DeepEqual(args, []string{path}) {
				t.Errorf("arguments = %v, want the path %q", args, path)
			}
		})
	}
}

// TestCommandForTheCurrentPlatform proves that the running platform resolves
// to the handler of the operating system the process runs on.
func TestCommandForTheCurrentPlatform(t *testing.T) {
	name, args := commandFor(runtime.GOOS, "/tmp/hfdesk.log")

	want := "xdg-open"
	switch runtime.GOOS {
	case "windows":
		want = "explorer.exe"
	case "darwin":
		want = "open"
	}
	if name != want {
		t.Errorf("command = %q, want %q", name, want)
	}
	if len(args) != 1 || args[0] != "/tmp/hfdesk.log" {
		t.Errorf("arguments = %v, want the path", args)
	}
}

// TestOpenRejectsAnEmptyPath proves that an empty path is refused instead of
// starting a handler without a target, which would open something the caller
// never asked for.
func TestOpenRejectsAnEmptyPath(t *testing.T) {
	if err := Open(""); err == nil {
		t.Error("Open accepted an empty path")
	}
}

// TestURLCommandForUsesThePlatformBrowser proves that every platform opens a
// URL with its own default browser, and that the URL is handed over
// unchanged.
//
// The URL and the path are different jobs: the handler of a URL is the
// browser and the handler of a path is the file manager. On Windows the URL
// reaches the browser through the protocol handler of the shell, because the
// file manager of a URL opens the folder of the URL instead of the page.
func TestURLCommandForUsesThePlatformBrowser(t *testing.T) {
	const url = "http://127.0.0.1:8080"

	cases := []struct {
		goos     string
		wantName string
		wantArgs []string
	}{
		{goos: "windows", wantName: "rundll32", wantArgs: []string{"url.dll,FileProtocolHandler", url}},
		{goos: "darwin", wantName: "open", wantArgs: []string{url}},
		{goos: "linux", wantName: "xdg-open", wantArgs: []string{url}},
		{goos: "freebsd", wantName: "xdg-open", wantArgs: []string{url}},
	}

	for _, c := range cases {
		t.Run(c.goos, func(t *testing.T) {
			name, args := urlCommandFor(c.goos, url)
			if name != c.wantName {
				t.Errorf("command = %q, want %q", name, c.wantName)
			}
			if !reflect.DeepEqual(args, c.wantArgs) {
				t.Errorf("arguments = %v, want %v", args, c.wantArgs)
			}
		})
	}
}

// TestURLCommandForTheCurrentPlatform proves that the running platform
// resolves to the browser of the operating system the process runs on.
func TestURLCommandForTheCurrentPlatform(t *testing.T) {
	const url = "http://127.0.0.1:8080"
	name, args := urlCommandFor(runtime.GOOS, url)

	want := "xdg-open"
	switch runtime.GOOS {
	case "windows":
		want = "rundll32"
	case "darwin":
		want = "open"
	}
	if name != want {
		t.Errorf("command = %q, want %q", name, want)
	}
	if len(args) == 0 || args[len(args)-1] != url {
		t.Errorf("arguments = %v, want them to carry the url", args)
	}
}

// TestOpenURLRejectsAnEmptyURL proves that an empty URL is refused instead of
// starting a browser without a target, which would open something the caller
// never asked for.
func TestOpenURLRejectsAnEmptyURL(t *testing.T) {
	if err := OpenURL(""); err == nil {
		t.Error("OpenURL accepted an empty url")
	}
}

// TestOpenURLReportsTheFailureOfTheBrowser proves that a browser that cannot
// be started is reported with the URL that could not be opened, so every
// caller of the browser reports the same failure.
func TestOpenURLReportsTheFailureOfTheBrowser(t *testing.T) {
	// A command that does not exist fails on every platform, so the
	// failure of the launch is deterministic.
	err := start("hfdesk-no-such-browser", []string{"http://127.0.0.1:8080"}, "http://127.0.0.1:8080")
	if err == nil {
		t.Fatal("the launch of a missing browser reported no error")
	}
	if !strings.Contains(err.Error(), "http://127.0.0.1:8080") {
		t.Errorf("error = %q, want it to name the url", err.Error())
	}
}
