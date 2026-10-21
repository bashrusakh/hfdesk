// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"os"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/pe"
)

// TestDesktopSubsystemCheckReadsTheRunningExecutable proves that the
// startup check of the subsystem reads the PE header of the very executable
// that is running.
//
// The test binary of a test run is a console subsystem program, so the
// check of the desktop host has to report it: that is the case the user of
// a mis-built desktop host runs into.
//
// The test never shows a message box: it drives the reader of the header
// and the warning directly, so it does not block the agent.
func TestDesktopSubsystemCheckReadsTheRunningExecutable(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("the path of the running executable: %v", err)
	}

	subsystem, err := pe.Subsystem(path)
	if err != nil {
		t.Fatalf("read the subsystem of %s: %v", path, err)
	}
	if subsystem == pe.SubsystemWindowsGUI {
		t.Fatalf("the test executable is a GUI subsystem program: the check of a console subsystem build would not be covered")
	}

	if warning := subsystemWarning(subsystem); warning == "" {
		t.Error("a console subsystem build is not reported to the user")
	}
}
