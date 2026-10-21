// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestOpenCreatesRestrictiveDirAndFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")

	sink, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	if got, want := sink.Path(), filepath.Join(dir, "hfdesk.log"); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("log dir mode = %04o, want 0700", got)
	}

	fileInfo, err := os.Stat(sink.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("log file mode = %04o, want 0600", got)
	}
}

func TestOpenCreatesMissingParentsOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "logs")

	sink, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	for _, path := range []string{dir, filepath.Dir(dir), filepath.Dir(filepath.Dir(dir))} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("dir %s mode = %04o, want 0700", path, got)
		}
	}
}

func TestOpenKeepsExistingDirMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	sink, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	// The mode of a directory that already exists belongs to whoever
	// named it: the instance writes there and leaves the mode alone.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("log dir mode = %04o, want 0755", got)
	}

	if _, err := sink.Write([]byte("line\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sink.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "line\n"; got != want {
		t.Errorf("log content = %q, want %q", got, want)
	}
}

func TestOpenKeepsExistingFileMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(dir, FileName)
	if err := os.WriteFile(logFile, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(logFile, 0o644); err != nil {
		t.Fatal(err)
	}

	sink, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	// The mode of a file that already exists belongs to whoever owns it:
	// the instance appends to it and leaves the mode alone.
	fileInfo, err := os.Stat(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o644 {
		t.Errorf("log file mode = %04o, want 0644", got)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o755 {
		t.Errorf("log dir mode = %04o, want 0755", got)
	}

	// Existing content is kept and new writes are appended.
	if _, err := sink.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "old\nnew\n"; got != want {
		t.Errorf("log content = %q, want %q", got, want)
	}
}

func TestWriteAppendsToActiveFile(t *testing.T) {
	dir := t.TempDir()
	sink, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	for i := range 3 {
		if _, err := sink.Write([]byte(fmt.Sprintf("line %d\n", i))); err != nil {
			t.Fatal(err)
		}
	}

	data, err := os.ReadFile(sink.Path())
	if err != nil {
		t.Fatal(err)
	}
	want := "line 0\nline 1\nline 2\n"
	if string(data) != want {
		t.Errorf("log content = %q, want %q", data, want)
	}
}

func TestRotationKeepsBoundedFileSet(t *testing.T) {
	dir := t.TempDir()
	sink, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	const chunkSize = 512 * 1024
	const chunks = 40
	var payloads [][]byte
	for i := range chunks {
		marker := fmt.Sprintf("[%04d]", i)
		payloads = append(payloads, []byte(marker+strings.Repeat("x", chunkSize-len(marker))))
		if _, err := sink.Write(payloads[i]); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	seen := map[string]bool{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		seen[entry.Name()] = true
		if info.Size() > MaxFileSize {
			t.Errorf("%s is %d bytes, above the %d byte limit", entry.Name(), info.Size(), MaxFileSize)
		}
		total += info.Size()
	}

	for _, name := range []string{FileName, FileName + ".1", FileName + ".2", FileName + ".3"} {
		if !seen[name] {
			t.Errorf("missing rotated log %s", name)
		}
	}
	if seen[FileName+".4"] {
		t.Errorf("unexpected log generation %s", FileName+".4")
	}
	if total > 4*MaxFileSize {
		t.Errorf("total log size is %d bytes, above the %d byte bound", total, 4*MaxFileSize)
	}

	// The oldest generation is discarded, the newest content is kept.
	oldest, err := os.ReadFile(filepath.Join(dir, FileName+".3"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(oldest, []byte("[0000]")) {
		t.Error("the oldest generation was not discarded")
	}
	newest, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(newest, payloads[chunks-1]) {
		t.Error("the newest write is missing from the active log")
	}
}

func TestOversizedWriteIsWrittenWhole(t *testing.T) {
	dir := t.TempDir()
	sink, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	big := bytes.Repeat([]byte("z"), int(MaxFileSize)+1024)
	n, err := sink.Write(big)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(big) {
		t.Fatalf("wrote %d bytes, want %d", n, len(big))
	}

	info, err := os.Stat(sink.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(big)) {
		t.Fatalf("log size = %d, want %d", info.Size(), len(big))
	}

	// The sink stays usable afterwards.
	if _, err := sink.Write([]byte("tail\n")); err != nil {
		t.Fatalf("write after an oversized write: %v", err)
	}
}

func TestConcurrentWritesAreRaceFree(t *testing.T) {
	dir := t.TempDir()
	sink, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	const writers = 8
	const writes = 50
	const payloadSize = 4096
	payload := bytes.Repeat([]byte("c"), payloadSize)

	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range writes {
				if _, err := sink.Write(payload); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// The total stays below the rotation limit, so nothing is discarded.
	info, err := os.Stat(sink.Path())
	if err != nil {
		t.Fatal(err)
	}
	want := int64(writers * writes * payloadSize)
	if info.Size() != want {
		t.Fatalf("log size = %d, want %d", info.Size(), want)
	}
}

func TestCloseStopsWrites(t *testing.T) {
	dir := t.TempDir()
	sink, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := sink.Write([]byte("line\n")); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("after close\n")); err == nil {
		t.Error("write after close must fail")
	}
	if err := sink.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}

	data, err := os.ReadFile(sink.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "line\n"; got != want {
		t.Errorf("log content = %q, want %q", got, want)
	}
}

func TestOpenRejectsEmptyDir(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("Open(\"\") must fail")
	}
}
