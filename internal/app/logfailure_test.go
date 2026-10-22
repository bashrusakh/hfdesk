// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/logging"
)

// newFailingLogPipeline builds a logging pipeline whose file sink fails to
// rotate the log file.
//
// The rotation renames the active file next to the rotated one. A directory
// in the place of the rotated file cannot be renamed over, and it cannot be
// removed either, so the rotation fails on every platform. It is the
// failure a user sees on Windows, where another process holds the log file
// open while the rotation renames it.
func newFailingLogPipeline(t *testing.T, path string) *logging.Pipeline {
	t.Helper()

	backup := path + ".1"
	if err := os.Mkdir(backup, 0o755); err != nil {
		t.Fatalf("create the blocking rotated file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backup, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatalf("fill the blocking rotated file: %v", err)
	}

	pipeline, err := logging.New(logging.Options{
		Level:          slog.LevelInfo,
		File:           true,
		FilePath:       path,
		FileMaxSize:    512,
		FileMaxBackups: 1,
	})
	if err != nil {
		t.Fatalf("build the logging pipeline: %v", err)
	}
	t.Cleanup(func() { _ = pipeline.Close() })
	return pipeline
}

// failTheRotation writes records until the file sink fails to rotate the
// log file.
func failTheRotation(t *testing.T, pipeline *logging.Pipeline) {
	t.Helper()

	logger := pipeline.Logger()
	payload := strings.Repeat("x", 256)
	for i := range 10 {
		logger.Info("rotating record", "index", i, "payload", payload)
	}

	if pipeline.Err() == nil {
		t.Fatal("the pipeline holds no failure after the rotation failed")
	}
}

// TestLogFailureReporterReportsAFailedRotation proves that the failure of
// the persistent log reaches the user instead of being swallowed.
//
// The file sink fails to rotate the log file, and the reporter shows the
// failure. The text names the problem and the log file, so the user can
// find the file that stopped working and can tell that it stopped working
// at all.
func TestLogFailureReporterReportsAFailedRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hfdesk.log")
	pipeline := newFailingLogPipeline(t, path)

	reports := make(chan string, 4)
	reporter := NewLogFailureReporter(pipeline, func(text string) { reports <- text })
	reporter.Subscribe()

	failTheRotation(t, pipeline)

	text := waitForLogFailure(t, reports)
	for _, want := range []string{"could not", "rotate", path} {
		if !strings.Contains(text, want) {
			t.Errorf("the report %q does not mention %q", text, want)
		}
	}
}

// TestLogFailureReporterReportsTheFailureAtTheShutdown proves that the
// failure the pipeline holds is reported when the host checks it before it
// closes the pipeline.
//
// The subscription is not started in this case: the check at the shutdown is
// the last chance to tell the user that the log file stopped working, and
// the error of the pipeline is still available then.
func TestLogFailureReporterReportsTheFailureAtTheShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hfdesk.log")
	pipeline := newFailingLogPipeline(t, path)

	// The failure happens before the host starts watching.
	failTheRotation(t, pipeline)

	reports := make(chan string, 4)
	reporter := NewLogFailureReporter(pipeline, func(text string) { reports <- text })
	reporter.Report()

	select {
	case text := <-reports:
		if !strings.Contains(text, path) {
			t.Errorf("the report %q does not name the log file", text)
		}
	default:
		t.Fatal("the failure of the log file was not reported at the shutdown")
	}
}

// TestLogFailureReporterReportsTheFailureOnce proves that the failure is
// reported once, even when the subscription and the check at the shutdown
// both see it.
func TestLogFailureReporterReportsTheFailureOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hfdesk.log")
	pipeline := newFailingLogPipeline(t, path)

	reports := make(chan string, 8)
	reporter := NewLogFailureReporter(pipeline, func(text string) { reports <- text })
	reporter.Subscribe()

	failTheRotation(t, pipeline)

	// The check at the shutdown finds the failure that the subscription
	// may have reported already.
	reporter.Report()

	// The subscription reports the failure on a goroutine of its own, so
	// the test waits for it before it counts the reports.
	time.Sleep(200 * time.Millisecond)
	if got := len(reports); got != 1 {
		t.Errorf("the failure of the log file was reported %d times, want once", got)
	}
}

// TestLogFailureReporterReportsNothingForAHealthyLog proves that a healthy
// log file produces no report.
//
// The report belongs to a log file that stopped working: a healthy shutdown
// must not print anything and must not show a message box.
func TestLogFailureReporterReportsNothingForAHealthyLog(t *testing.T) {
	pipeline, err := logging.New(logging.Options{Level: slog.LevelInfo})
	if err != nil {
		t.Fatalf("build the logging pipeline: %v", err)
	}
	t.Cleanup(func() { _ = pipeline.Close() })

	reports := make(chan string, 4)
	reporter := NewLogFailureReporter(pipeline, func(text string) { reports <- text })
	reporter.Subscribe()

	pipeline.Logger().Info("a record of a healthy log")
	reporter.Report()

	// The subscription reports on a goroutine of its own, so the test
	// waits before it counts the reports.
	time.Sleep(200 * time.Millisecond)
	if got := len(reports); got != 0 {
		t.Errorf("a healthy log produced %d reports, want none", got)
	}
}

// TestLogFailureTextNamesTheProblemAndTheFile proves that the text of the
// report names the problem and the log file.
//
// The user has no other way to learn that the log file stopped working: the
// text is the whole report.
func TestLogFailureTextNamesTheProblemAndTheFile(t *testing.T) {
	const path = "/home/user/.cache/HFDesk/logs/hfdesk.log"

	text := LogFailureText(errors.New("logging: the file sink failed: logging: rotate log file: access is denied"), path)

	for _, want := range []string{"could not", "rotate log file", path} {
		if !strings.Contains(text, want) {
			t.Errorf("the text %q does not mention %q", text, want)
		}
	}
	if strings.Contains(text, "\n\n\n") {
		t.Errorf("the text %q carries an empty paragraph", text)
	}
}

// waitForLogFailure waits for the report of a failure of the log file.
func waitForLogFailure(t *testing.T, reports <-chan string) string {
	t.Helper()

	select {
	case text := <-reports:
		return text
	case <-time.After(10 * time.Second):
		t.Fatal("the failure of the log file was not reported")
		return ""
	}
}
