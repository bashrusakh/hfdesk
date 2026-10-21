// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package opener

import (
	"reflect"
	"runtime"
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
