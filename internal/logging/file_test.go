// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileSinkWritesJSONLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hfdesk.log")

	p, err := New(Options{File: true, FilePath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	p.Logger().Info("download started", "repo", "org/model")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}

	var entry struct {
		Seq       uint64         `json:"seq"`
		Time      string         `json:"time"`
		Level     string         `json:"level"`
		Component string         `json:"component"`
		Message   string         `json:"message"`
		Attrs     map[string]any `json:"attrs"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("decode log line: %v", err)
	}
	if entry.Seq == 0 {
		t.Errorf("log line has no sequence number")
	}
	if entry.Time == "" {
		t.Errorf("log line has no timestamp")
	}
	if entry.Level != "INFO" {
		t.Errorf("level = %q, want %q", entry.Level, "INFO")
	}
	if entry.Message != "download started" {
		t.Errorf("message = %q, want %q", entry.Message, "download started")
	}
	if entry.Attrs["repo"] != "org/model" {
		t.Errorf("attrs[repo] = %v, want %q", entry.Attrs["repo"], "org/model")
	}
}

func TestFileSinkRotatesAndBoundsRetainedBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hfdesk.log")
	const maxSize = 512
	const maxBackups = 2

	p, err := New(Options{
		File:           true,
		FilePath:       path,
		FileMaxSize:    maxSize,
		FileMaxBackups: maxBackups,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	logger := p.Logger()
	for i := range 50 {
		logger.Info("rotating record", "index", i, "payload", strings.Repeat("x", 64))
	}

	matches, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) > maxBackups+1 {
		t.Fatalf("retained %d files, want at most %d", len(matches), maxBackups+1)
	}

	active, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat active file: %v", err)
	}
	if active.Size() > maxSize {
		t.Errorf("active file is %d bytes, want at most %d", active.Size(), maxSize)
	}

	total := int64(0)
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			t.Fatalf("stat %s: %v", match, err)
		}
		total += info.Size()
	}
	if limit := int64(maxSize) * (maxBackups + 1); total > limit {
		t.Errorf("log files use %d bytes, want at most %d", total, limit)
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("no rotated backup was created: %v", err)
	}
}

func TestFileSinkKeepsContentAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hfdesk.log")

	first, err := New(Options{File: true, FilePath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first.Logger().Info("before restart")
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := New(Options{File: true, FilePath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer second.Close()
	second.Logger().Info("after restart")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "before restart") {
		t.Errorf("the existing log file was truncated: %q", string(data))
	}
	if !strings.Contains(string(data), "after restart") {
		t.Errorf("the new record was not appended: %q", string(data))
	}
}

func TestFileSinkWithoutBackupsKeepsOnlyTheActiveFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hfdesk.log")

	p, err := New(Options{
		File:           true,
		FilePath:       path,
		FileMaxSize:    512,
		FileMaxBackups: 0,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	logger := p.Logger()
	for i := range 20 {
		logger.Info("rotating record", "index", i, "payload", strings.Repeat("x", 64))
	}

	matches, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("retained %d files, want only the active file", len(matches))
	}
}

func TestFileSinkRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewFileSink("", 1024, 1); err == nil {
		t.Errorf("expected an error for an empty path")
	}
	if _, err := NewFileSink(filepath.Join(t.TempDir(), "x.log"), 0, 1); err == nil {
		t.Errorf("expected an error for a non positive maximum size")
	}
}

func TestFileSinkCreatesFileWithOwnerOnlyPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hfdesk.log")

	p, err := New(Options{File: true, FilePath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("log file permissions are %o, want owner only", perm)
	}
}
