// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

// Package opener opens a file or directory with the default handler of the
// operating system.
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
	cmd := exec.Command(name, args...)
	// The standard streams of the helper stay the null device, so the
	// helper never writes into the console of the application and never
	// takes its standard input away from it.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open %s: %w", path, err)
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
