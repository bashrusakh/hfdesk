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
	"github.com/bashrusakh/hfdesk/internal/logging"
	"golang.org/x/sys/windows"
)

// TestDesktopHostReportsAFailedLogFile proves that the desktop host shows
// the failure of the persistent log to the user.
//
// The desktop host has no console: a log file that stopped working is only
// visible to the user in a message box. The report happens before the
// pipeline is closed, because the error of the pipeline is still available
// then.
//
// The test drives the end of the host: the message box is recorded instead
// of shown, because a message box blocks a process without an interactive
// window station.
func TestDesktopHostReportsAFailedLogFile(t *testing.T) {
	// The message box is recorded instead of shown.
	var messages []shownMessage
	previousMessage := showMessage
	showMessage = func(caption, text string, icon uint32) {
		messages = append(messages, shownMessage{caption: caption, text: text, icon: icon})
	}
	defer func() { showMessage = previousMessage }()

	logFile := filepath.Join(t.TempDir(), "hfdesk.log")

	// The rotated file is a directory that cannot be renamed over and
	// that cannot be removed, so the rotation of the log file fails.
	backup := logFile + ".1"
	if err := os.Mkdir(backup, 0o755); err != nil {
		t.Fatalf("create the blocking rotated file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backup, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatalf("fill the blocking rotated file: %v", err)
	}

	pipeline, err := logging.New(logging.Options{
		Level:          slog.LevelInfo,
		File:           true,
		FilePath:       logFile,
		FileMaxSize:    512,
		FileMaxBackups: 1,
	})
	if err != nil {
		t.Fatalf("build the logging pipeline: %v", err)
	}
	// The pipeline is closed at the end of the test, and the cleanup
	// closes it again when the test ends early: an open log file cannot
	// be removed on Windows, and the temporary directory of the test has
	// to be removable.
	t.Cleanup(func() { _ = pipeline.Close() })

	reporter := app.NewLogFailureReporter(pipeline, func(text string) {
		showMessage(logFailureCaption, text, windows.MB_ICONERROR)
	})

	// The records are large enough to push the log file past its maximum
	// size, so the sink rotates and the rotation fails.
	logger := pipeline.Logger()
	payload := strings.Repeat("x", 256)
	for i := range 10 {
		logger.Info("rotating record", "index", i, "payload", payload)
	}
	if pipeline.Err() == nil {
		t.Fatal("the pipeline holds no failure after the rotation failed")
	}

	// The host ends: the failure is reported before the pipeline is closed.
	closePipeline(pipeline, reporter)

	if len(messages) != 1 {
		t.Fatalf("the host showed %d message boxes, want 1: %v", len(messages), messages)
	}
	if messages[0].caption != logFailureCaption {
		t.Errorf("caption = %q, want %q", messages[0].caption, logFailureCaption)
	}
	if !strings.Contains(messages[0].text, logFile) {
		t.Errorf("the message %q does not name the log file", messages[0].text)
	}
	if !strings.Contains(messages[0].text, "could not") {
		t.Errorf("the message %q does not name the problem", messages[0].text)
	}
	if messages[0].icon != windows.MB_ICONERROR {
		t.Errorf("the icon of the message = %d, want the error icon %d",
			messages[0].icon, windows.MB_ICONERROR)
	}
}
