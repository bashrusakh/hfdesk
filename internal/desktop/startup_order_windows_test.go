// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecondLaunchDoesNotOpenTheLogFile proves that a launch that finds a
// running instance does not open the rotating log file.
//
// The log file belongs to the launch that runs the host. A second launch
// that opened it would hold the very same file as the running instance,
// and the rotation of the file would fail because the other process holds
// it: a failed rotation stops the persistent log.
//
// The single instance lock is therefore taken before the log file is
// opened, so the second launch finds the running instance and stops before
// the log file exists. The user is still told that HFDesk is already
// running.
//
// The test drives the real startup of the host, so the order of the two
// steps cannot be swapped again: a logging setup in front of the lock
// opens the file and the test fails.
func TestSecondLaunchDoesNotOpenTheLogFile(t *testing.T) {
	useTemporaryStateDir(t)

	// The first instance holds the lock of the desktop host.
	first, err := acquireInstanceLock()
	if err != nil {
		t.Fatalf("acquire the single instance lock: %v", err)
	}
	defer first.Release()

	// The message box is recorded instead of shown: a message box blocks
	// an agent without an interactive window station.
	var messages []string
	previous := showMessage
	showMessage = func(caption, text string, icon uint32) {
		messages = append(messages, caption+": "+text)
	}
	defer func() { showMessage = previous }()

	logFile := filepath.Join(t.TempDir(), "hfdesk.log")
	if err := run(Options{}, logFile); err != nil {
		t.Fatalf("the second launch failed: %v", err)
	}

	// The user is told that HFDesk is already running.
	if len(messages) != 1 {
		t.Fatalf("the second launch showed %d message boxes, want 1: %v", len(messages), messages)
	}
	if !strings.Contains(messages[0], "already running") {
		t.Errorf("the message of the second launch is %q, want the notice that HFDesk is already running",
			messages[0])
	}

	// The second launch did not open the log file: the file belongs to
	// the instance that runs the host.
	if _, err := os.Stat(logFile); !os.IsNotExist(err) {
		t.Errorf("the second launch opened the log file %s: %v", logFile, err)
	}
}
