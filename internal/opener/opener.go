// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

// Package opener opens a file, a directory or a URL with the default
// handler of the operating system.
package opener

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
)

// Open opens path with the default handler: the file manager for a
// directory, the associated application for a file. The process is started
// and not waited for, so the caller is not blocked. It returns an error when
// the command cannot be started.
func Open(path string) error {
	if path == "" {
		return errors.New("opener: no path to open")
	}

	name, args := commandFor(runtime.GOOS, path)
	return start(name, args, path)
}

// OpenURL opens url with the default handler of the operating system: the
// default browser.
//
// It is the one implementation of "open a URL with the default handler". A
// URL and a file are different jobs: the handler of a URL is the browser and
// the handler of a file is the associated application, so the two have their
// own mechanism. The process is started and not waited for, so the caller is
// not blocked. It returns an error when the command cannot be started.
func OpenURL(url string) error {
	if url == "" {
		return errors.New("opener: no url to open")
	}

	name, args := urlCommandFor(runtime.GOOS, url)
	return start(name, args, url)
}

// start runs the command that opens the target and reaps it in the
// background.
//
// The target is named in the error, so a failure of the browser or of the
// file manager is reported the same way for every caller.
func start(name string, args []string, target string) error {
	cmd := exec.Command(name, args...)
	// The standard streams of the helper stay the null device, so the
	// helper never writes into the console of the application and never
	// takes its standard input away from it.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open %s: %w", target, err)
	}
	// The helper is reaped in the background, so it does not stay behind as
	// a zombie and the caller is not blocked by it.
	go func() { _ = cmd.Wait() }()
	return nil
}

// commandFor returns the command and the arguments that open path on the
// given operating system.
//
// Windows opens the containing folder and selects the file, macOS and the
// other systems hand the path to the default handler of the desktop.
func commandFor(goos, path string) (string, []string) {
	switch goos {
	case "windows":
		return "explorer.exe", []string{path}
	case "darwin":
		return "open", []string{path}
	default:
		return "xdg-open", []string{path}
	}
}

// urlCommandFor returns the command and the arguments that open url with
// the default browser on the given operating system.
//
// Windows hands the URL to the protocol handler of the shell, which opens
// it with the browser of the user. That is the mechanism that reliably
// reaches the default browser on Windows: the file manager of a URL opens
// the folder of the URL instead of the page.
//
// macOS and the other systems hand the URL to the default handler of the
// desktop, which opens it with the browser.
func urlCommandFor(goos, url string) (string, []string) {
	switch goos {
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		return "open", []string{url}
	default:
		return "xdg-open", []string{url}
	}
}
