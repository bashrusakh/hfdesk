// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/logging"
)

// TestSetupLoggingRejectsAnInvalidLevel proves that an unknown level is an
// error and never a silent fallback.
func TestSetupLoggingRejectsAnInvalidLevel(t *testing.T) {
	restoreDefaultLogger(t)

	if _, err := SetupLogging(LogOptions{Level: "verbose", Console: true}); err == nil {
		t.Fatal("SetupLogging accepted an unknown level")
	} else if !strings.Contains(err.Error(), "verbose") {
		t.Errorf("error = %q, want it to name the rejected level", err.Error())
	}
}

// TestSetupLoggingAcceptsTheKnownLevels proves that every documented level
// is accepted and that an empty level means info.
func TestSetupLoggingAcceptsTheKnownLevels(t *testing.T) {
	restoreDefaultLogger(t)

	for _, level := range []string{"", "debug", "info", "warn", "error"} {
		pipeline, err := SetupLogging(LogOptions{Level: level, Console: true})
		if err != nil {
			t.Fatalf("SetupLogging with level %q: %v", level, err)
		}
		if pipeline == nil {
			t.Fatalf("SetupLogging with level %q returned no pipeline", level)
		}
		if err := pipeline.Close(); err != nil {
			t.Fatalf("close pipeline: %v", err)
		}
	}
}

// TestSetupLoggingConsoleWritesToStderr proves that the console sink writes
// to standard error, so redirecting stdout keeps working for scripts.
func TestSetupLoggingConsoleWritesToStderr(t *testing.T) {
	restoreDefaultLogger(t)

	stderr, stdout := captureOutput(t, func() {
		pipeline, err := SetupLogging(LogOptions{Level: "info", Console: true})
		if err != nil {
			t.Fatalf("SetupLogging: %v", err)
		}
		defer pipeline.Close()
		logging.Component("app").Info("record for the console sink")
	})

	if !strings.Contains(stderr, "record for the console sink") {
		t.Errorf("stderr = %q, want the record", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want it to stay empty", stdout)
	}
}

// TestSetupLoggingWithoutAFileSinkWritesNoFile proves that a host that does
// not ask for a log file gets no file sink.
func TestSetupLoggingWithoutAFileSinkWritesNoFile(t *testing.T) {
	restoreDefaultLogger(t)

	pipeline, err := SetupLogging(LogOptions{Level: "info", Console: true})
	if err != nil {
		t.Fatalf("SetupLogging: %v", err)
	}
	defer pipeline.Close()

	logging.Component("app").Info("record without a file sink")
	if err := pipeline.Err(); err != nil {
		t.Fatalf("pipeline reported %v", err)
	}
}

// TestSetupLoggingWritesTheRequestedFile proves that the file sink writes
// the records to the requested path.
func TestSetupLoggingWritesTheRequestedFile(t *testing.T) {
	restoreDefaultLogger(t)

	path := filepath.Join(t.TempDir(), "logs", "hfdesk.log")
	pipeline, err := SetupLogging(LogOptions{Level: "info", FilePath: path})
	if err != nil {
		t.Fatalf("SetupLogging: %v", err)
	}
	logging.Component("app").Info("record for the file sink")
	if err := pipeline.Close(); err != nil {
		t.Fatalf("close pipeline: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(content), "record for the file sink") {
		t.Errorf("log file = %q, want the record", content)
	}
}

// TestSetupLoggingKeepsRotatedBackups proves that the production logging
// configuration keeps the history of the log: a rotated backup file appears
// next to the active file.
//
// The desktop host writes its log file because the file is its support
// material, so a run that keeps no history of the log loses the records of
// the previous runs. The test writes the records of a host that says
// nothing about the backups, so it covers the configuration the product
// really uses.
func TestSetupLoggingKeepsRotatedBackups(t *testing.T) {
	restoreDefaultLogger(t)

	path := filepath.Join(t.TempDir(), "hfdesk.log")
	pipeline, err := SetupLogging(LogOptions{Level: "info", FilePath: path})
	if err != nil {
		t.Fatalf("SetupLogging: %v", err)
	}
	defer pipeline.Close()

	// The records are large enough to push the file past its maximum size
	// after a few of them, so the sink rotates while the test writes.
	logger := logging.Component("app")
	payload := strings.Repeat("x", 4096)
	for i := range 2000 {
		logger.Info("rotating record", "index", i, "payload", payload)
		if _, err := os.Stat(path + ".1"); err == nil {
			break
		}
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no rotated backup was kept next to the active log file: %v", err)
	}

	// The backups are bounded: the log files never use more disk than the
	// maximum size of the active file times the number of kept backups.
	matches, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if limit := logging.DefaultFileMaxBackups + 1; len(matches) > limit {
		t.Errorf("retained %d log files, want at most %d", len(matches), limit)
	}
}

// TestSetupLoggingKeepsNoRotatedFile proves the mapping of the rotated file
// count of this layer onto the logging package: a negative value keeps no
// rotated file at all, while the zero value keeps the default history.
//
// The two layers spell the zero and the negative value the other way round,
// so the test pins the translation of SetupLogging: the logging package only
// ever sees a count it reads the same way.
func TestSetupLoggingKeepsNoRotatedFile(t *testing.T) {
	restoreDefaultLogger(t)

	path := filepath.Join(t.TempDir(), "hfdesk.log")
	pipeline, err := SetupLogging(LogOptions{Level: "info", FilePath: path, RotatedFiles: -1})
	if err != nil {
		t.Fatalf("SetupLogging: %v", err)
	}
	defer pipeline.Close()

	// The records are large enough to push the file past its maximum size
	// after a few of them, so the sink rotates while the test writes.
	logger := logging.Component("app")
	payload := strings.Repeat("x", 4096)
	const records = 2000
	for i := range records {
		logger.Info("rotating record", "index", i, "payload", payload)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the active log file: %v", err)
	}
	if info.Size() >= int64(records*len(payload)) {
		t.Fatalf("the active log file holds %d bytes, want a file that rotated", info.Size())
	}

	matches, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Errorf("retained %d log files (%v), want only the active file", len(matches), matches)
	}
}

// TestDefaultLogPathDoesNotCreateAnything proves that the default log path
// is a per-user path and that asking for it creates nothing.
func TestDefaultLogPathDoesNotCreateAnything(t *testing.T) {
	root := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("LocalAppData", root)
	case "darwin":
		t.Setenv("HOME", root)
	default:
		t.Setenv("XDG_CACHE_HOME", root)
	}

	path := DefaultLogPath()
	if !strings.HasSuffix(path, filepath.Join("HFDesk", "logs", "hfdesk.log")) {
		t.Errorf("log path = %q, want it to end in the per-user log file", path)
	}
	if !strings.HasPrefix(path, root) {
		t.Errorf("log path = %q, want it under the per-user cache dir", path)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("DefaultLogPath created %q", filepath.Dir(path))
	}
}

// restoreDefaultLogger restores the process wide default logger and the
// standard logger after a test installed a pipeline.
func restoreDefaultLogger(t *testing.T) {
	t.Helper()

	previousLogger := slog.Default()
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
}
