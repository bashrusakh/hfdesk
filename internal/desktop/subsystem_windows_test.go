// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/app"
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

// TestDesktopSubsystemCheckRecordsTheBuildAndReturnsTheWarning proves that
// the startup check of the subsystem does two things: it records the problem
// in the log file, so the support material of the host shows it, and it
// returns the warning that the message box of the host shows.
//
// The check shows no message box itself: the warning is a property of the
// binary, so it belongs to the launch that runs the host. A launch that
// finds a running instance reports that instance and leaves the warning to
// the launch that runs the host.
//
// The test never shows a message box: it drives the check of the subsystem
// directly, so it does not block the agent.
func TestDesktopSubsystemCheckRecordsTheBuildAndReturnsTheWarning(t *testing.T) {
	// The desktop host has no console, so the log file is the only place
	// its records are written to.
	logFile := filepath.Join(t.TempDir(), "hfdesk.log")
	previous := slog.Default()
	pipeline, err := app.SetupLogging(app.LogOptions{
		Level:    "info",
		Console:  false,
		FilePath: logFile,
	})
	if err != nil {
		t.Fatalf("install the logging pipeline: %v", err)
	}
	defer func() {
		pipeline.Close()
		// The pipeline is the process wide default logger, so the logger
		// the other tests of the package expect is put back.
		slog.SetDefault(previous)
	}()

	warning := checkSubsystem()
	if warning == "" {
		t.Fatal("a console subsystem build is not reported to the user")
	}
	if !strings.Contains(warning, "-H=windowsgui") {
		t.Errorf("the warning %q does not name the missing linker flag", warning)
	}

	// The record of the problem reaches the log file.
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read the log file: %v", err)
	}
	if !strings.Contains(string(content), "Windows GUI subsystem") {
		t.Errorf("the log file does not report the wrong subsystem: %s", content)
	}
}
