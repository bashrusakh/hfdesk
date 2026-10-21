// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"flag"
	"testing"
)

// TestCommandLineExitCode proves the exit code of a command line problem of
// the desktop host.
//
// A help request is not a failure: it ends the host with a success code.
// Every other problem ends the host with a non-zero code, so a launcher can
// tell that the desktop host did not start.
//
// The test does not show the message of the host: on Windows it is shown in
// a native message box, and a message box blocks an agent without an
// interactive window station.
func TestCommandLineExitCode(t *testing.T) {
	if code := commandLineExitCode(flag.ErrHelp); code != 0 {
		t.Errorf("a help request ends the host with the exit code %d, want 0", code)
	}

	code := commandLineExitCode(errors.New("flag provided but not defined: -bogus"))
	if code == 0 {
		t.Error("a command line error ends the host with the exit code 0")
	}
}
