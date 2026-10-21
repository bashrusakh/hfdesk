// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"bytes"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestPipeline builds a pipeline writing to the returned buffer.
func newTestPipeline(t *testing.T, opts Options) (*Pipeline, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	opts.Console = true
	opts.ConsoleWriter = &buf
	p, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, &buf
}

// installForTest installs a pipeline as the process wide default and restores
// the previous logger when the test ends.
func installForTest(t *testing.T, opts Options) (*Pipeline, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	opts.Console = true
	opts.ConsoleWriter = &buf

	previousLogger := slog.Default()
	previousWriter := log.Writer()
	previousFlags := log.Flags()

	p, err := Install(opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Close()
		slog.SetDefault(previousLogger)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		defaultPipeline.Store(nil)
	})
	return p, &buf
}

func TestPipelineRecordCarriesShape(t *testing.T) {
	p, _ := newTestPipeline(t, Options{})

	p.Logger().Info("download started", "repo", "org/model", "bytes", 42)

	records, truncated := p.Records(0, OldestFirst)
	if truncated {
		t.Fatalf("records were reported as truncated")
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec := records[0]
	if rec.Time.IsZero() {
		t.Errorf("record has no timestamp")
	}
	if time.Since(rec.Time) > time.Minute {
		t.Errorf("record timestamp %v is not recent", rec.Time)
	}
	if rec.Level != slog.LevelInfo {
		t.Errorf("level = %v, want %v", rec.Level, slog.LevelInfo)
	}
	if rec.Message != "download started" {
		t.Errorf("message = %q, want %q", rec.Message, "download started")
	}
	if rec.Seq == 0 {
		t.Errorf("record has no sequence number")
	}
	if got := rec.Attrs["repo"]; got != "org/model" {
		t.Errorf("attrs[repo] = %v, want %q", got, "org/model")
	}
	if got := rec.Attrs["bytes"]; got != int64(42) {
		t.Errorf("attrs[bytes] = %v (%T), want 42", got, got)
	}
}

func TestPipelineComponentReachesRecord(t *testing.T) {
	p, buf := installForTest(t, Options{})

	Component("jobs").Info("queued", "job", "42")

	records, _ := p.Records(0, OldestFirst)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Component != "jobs" {
		t.Errorf("component = %q, want %q", records[0].Component, "jobs")
	}
	if _, ok := records[0].Attrs[ComponentKey]; ok {
		t.Errorf("component must not be repeated in the attribute set")
	}
	if !bytes.Contains(buf.Bytes(), []byte("[jobs] queued")) {
		t.Errorf("console output %q does not carry the component", buf.String())
	}
}

func TestPipelineLevelFiltering(t *testing.T) {
	p, _ := newTestPipeline(t, Options{Level: slog.LevelWarn})

	logger := p.Logger()
	logger.Debug("debug message")
	logger.Info("info message")
	logger.Warn("warn message")
	logger.Error("error message")

	records, _ := p.Records(0, OldestFirst)
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	if records[0].Message != "warn message" || records[1].Message != "error message" {
		t.Errorf("unexpected records: %q, %q", records[0].Message, records[1].Message)
	}
}

func TestPipelineSetLevelAppliesImmediately(t *testing.T) {
	p, _ := newTestPipeline(t, Options{Level: slog.LevelError})

	p.Logger().Info("hidden")
	p.SetLevel(slog.LevelDebug)
	p.Logger().Debug("visible")

	records, _ := p.Records(0, OldestFirst)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Message != "visible" {
		t.Errorf("message = %q, want %q", records[0].Message, "visible")
	}
	if p.Level() != slog.LevelDebug {
		t.Errorf("level = %v, want %v", p.Level(), slog.LevelDebug)
	}
}

func TestInstallMakesPipelineTheProcessDefault(t *testing.T) {
	p, _ := installForTest(t, Options{})

	slog.Info("through the default logger", "source", "stdlib")

	records, _ := p.Records(0, OldestFirst)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Message != "through the default logger" {
		t.Errorf("message = %q", records[0].Message)
	}
	if got := records[0].Attrs["source"]; got != "stdlib" {
		t.Errorf("attrs[source] = %v, want %q", got, "stdlib")
	}
	if Default() != p {
		t.Errorf("Default() did not return the installed pipeline")
	}
}

func TestComponentResolvesTheCurrentDefaultAtCallTime(t *testing.T) {
	// The default logger is resolved when the record is emitted, not when
	// the helper is called at package initialization time.
	p, _ := installForTest(t, Options{})

	logger := Component("download")
	logger.Info("started")

	records, _ := p.Records(0, OldestFirst)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Component != "download" {
		t.Errorf("component = %q, want %q", records[0].Component, "download")
	}
}

// packageLevelLogger is bound at package initialization time, before any
// pipeline exists. It must still reach the pipeline once one is installed.
var packageLevelLogger = NewComponentLogger("cache")

func TestPackageLevelComponentLoggerReachesInstalledPipeline(t *testing.T) {
	p, _ := installForTest(t, Options{})

	packageLevelLogger.Info("rebuilt", "entries", 3)

	records, _ := p.Records(0, OldestFirst)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Component != "cache" {
		t.Errorf("component = %q, want %q", records[0].Component, "cache")
	}
	if got := records[0].Attrs["entries"]; got != int64(3) {
		t.Errorf("attrs[entries] = %v, want 3", got)
	}
}

func TestComponentLoggerLevelsAndAttributes(t *testing.T) {
	p, _ := installForTest(t, Options{})

	logger := NewComponentLogger("jobs").With("queue", "default")
	logger.Debug("debug")
	logger.Info("info")
	logger.Warn("warn")
	logger.Error("error")

	records, _ := p.Records(0, OldestFirst)
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3 (debug is filtered)", len(records))
	}
	for _, rec := range records {
		if rec.Component != "jobs" {
			t.Errorf("component = %q, want %q", rec.Component, "jobs")
		}
		if got := rec.Attrs["queue"]; got != "default" {
			t.Errorf("attrs[queue] = %v, want %q", got, "default")
		}
	}
}

func TestSubscriptionReceivesNewRecordsAndUnsubscribes(t *testing.T) {
	p, _ := newTestPipeline(t, Options{})

	sub := p.Subscribe()
	p.Logger().Info("first")

	select {
	case rec := <-sub.C():
		if rec.Message != "first" {
			t.Errorf("message = %q, want %q", rec.Message, "first")
		}
	case <-time.After(time.Second):
		t.Fatalf("no record received")
	}

	sub.Close()
	p.Logger().Info("second")

	select {
	case rec, ok := <-sub.C():
		if ok {
			t.Errorf("received %q after unsubscribing", rec.Message)
		}
	case <-time.After(100 * time.Millisecond):
		t.Errorf("subscription channel was not closed")
	}

	// The pipeline must keep working after a subscription was closed.
	if records, _ := p.Records(0, OldestFirst); len(records) != 2 {
		t.Errorf("got %d records, want 2", len(records))
	}
}

func TestSubscriptionDropsRecordsWhenConsumerIsSlow(t *testing.T) {
	p, _ := newTestPipeline(t, Options{SubscriptionBuffer: 1})

	sub := p.Subscribe()
	for range 10 {
		p.Logger().Info("burst")
	}
	if sub.Dropped() == 0 {
		t.Errorf("expected dropped records for a slow consumer")
	}
	sub.Close()
}

func TestPipelineCursorTracksTheLatestRecord(t *testing.T) {
	p, _ := newTestPipeline(t, Options{})

	if got := p.Cursor(); got != 0 {
		t.Errorf("cursor = %d, want 0 before the first record", got)
	}
	p.Logger().Info("first")
	if got := p.Cursor(); got != 1 {
		t.Errorf("cursor = %d, want 1", got)
	}
	p.Logger().Info("second")
	if got := p.Cursor(); got != 2 {
		t.Errorf("cursor = %d, want 2", got)
	}
}

func TestPipelineEndToEndUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hfdesk.log")

	p, err := Install(Options{
		Console:       true,
		ConsoleWriter: io.Discard,
		File:          true,
		FilePath:      path,
		RingCapacity:  10,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	// The migration path other packages will use.
	NewComponentLogger("server").Info("listening", "addr", "127.0.0.1:8080")
	slog.Warn("slow response", "duration", "2s")
	log.Printf("standard library message")

	records, truncated := p.Records(0, OldestFirst)
	if truncated {
		t.Errorf("truncated = true, want false")
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
	if records[0].Component != "server" {
		t.Errorf("component = %q, want %q", records[0].Component, "server")
	}
	if records[1].Level != slog.LevelWarn {
		t.Errorf("level = %v, want %v", records[1].Level, slog.LevelWarn)
	}
	if !strings.Contains(records[2].Message, "standard library message") {
		t.Errorf("message = %q, want the standard library message", records[2].Message)
	}

	// The file sink received the same records.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1
	if lines != 3 {
		t.Errorf("the log file has %d lines, want 3", lines)
	}

	// A reader continues from the cursor of the last record it saw.
	cursor := records[len(records)-1].Seq
	if got := p.Cursor(); got != cursor {
		t.Errorf("cursor = %d, want %d", got, cursor)
	}
	records, truncated = p.Records(cursor, OldestFirst)
	if len(records) != 0 || truncated {
		t.Errorf("got %d records (truncated %v), want none", len(records), truncated)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"INFO":    slog.LevelInfo,
		" warn ":  slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for input, want := range cases {
		got, err := ParseLevel(input)
		if err != nil {
			t.Fatalf("ParseLevel(%q): %v", input, err)
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", input, got, want)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Errorf("ParseLevel(\"verbose\") returned no error")
	}
}

func TestNewRejectsFileSinkWithoutPath(t *testing.T) {
	if _, err := New(Options{File: true}); err == nil {
		t.Fatalf("expected an error for a file sink without a path")
	}
}

func TestFileSinkRequiresWritablePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "hfdesk.log")
	p, err := New(Options{File: true, FilePath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	p.Logger().Info("hello")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log file was not created: %v", err)
	}
}
