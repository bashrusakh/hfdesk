// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// newConsoleSink returns a console sink and the buffer it writes to.
func newConsoleSink() (*ConsoleSink, *bytes.Buffer) {
	var buf bytes.Buffer
	return NewConsoleSink(&buf), &buf
}

func TestConsoleSinkRendersReadableLine(t *testing.T) {
	sink, buf := newConsoleSink()

	rec := Record{
		Seq:       7,
		Time:      time.Date(2026, 10, 21, 18, 30, 0, 123_000_000, time.UTC),
		Level:     slog.LevelInfo,
		Component: "download",
		Message:   "started",
		Attrs:     map[string]any{"repo": "org/model", "bytes": int64(42)},
	}

	if err := sink.Write(rec); err != nil {
		t.Fatalf("Write: %v", err)
	}
	want := "2026-10-21T18:30:00.123Z INFO  [download] started bytes=42 repo=org/model\n"
	if got := buf.String(); got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
}

func TestConsoleSinkOmitsAnEmptyComponent(t *testing.T) {
	sink, buf := newConsoleSink()

	rec := Record{
		Time:    time.Date(2026, 10, 21, 18, 30, 0, 0, time.UTC),
		Level:   slog.LevelError,
		Message: "boom",
	}
	if err := sink.Write(rec); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "ERROR boom") {
		t.Errorf("line = %q, want it to contain %q", got, "ERROR boom")
	}
}

func TestConsoleSinkQuotesValuesWithSpaces(t *testing.T) {
	sink, buf := newConsoleSink()

	rec := Record{
		Time:    time.Date(2026, 10, 21, 18, 30, 0, 0, time.UTC),
		Level:   slog.LevelInfo,
		Message: "message",
		Attrs:   map[string]any{"path": "/tmp/some dir/file.bin"},
	}
	if err := sink.Write(rec); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, `path="/tmp/some dir/file.bin"`) {
		t.Errorf("line = %q, want the value to be quoted", got)
	}
}

func TestConsoleSinkWritesToStandardErrorByDefault(t *testing.T) {
	if DefaultConsoleWriter() != os.Stderr {
		t.Errorf("the default console writer is not standard error")
	}

	p, err := New(Options{Console: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	if p.console == nil {
		t.Fatalf("the console sink was not created")
	}
	if p.console.w != os.Stderr {
		t.Errorf("the console sink does not write to standard error")
	}
}

func TestConsoleSinkIsNotEnabledByDefault(t *testing.T) {
	p, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	if p.console != nil {
		t.Errorf("the console sink must be opt in")
	}
}
