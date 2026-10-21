// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileSink writes records as JSON lines to a rotating file.
//
// The sink is bounded twice: the active file rotates once it would grow past
// the configured maximum size, and only the configured number of rotated
// files is kept. The total size on disk therefore stays below
// maxSize * (1 + maxBackups).
//
// A single record that is larger than the maximum size is written to an empty
// file, so the active file can exceed the maximum size by that record.
type FileSink struct {
	mu         sync.Mutex
	path       string
	maxSize    int64
	maxBackups int
	file       *os.File
	size       int64
	err        error
	closed     bool
}

// NewFileSink opens (or creates) the log file at path and returns a sink
// that rotates it at maxSize bytes, keeping maxBackups rotated files.
//
// The file is created with owner only permissions because logs may contain
// information that is sensitive even after redaction.
func NewFileSink(path string, maxSize int64, maxBackups int) (*FileSink, error) {
	if path == "" {
		return nil, fmt.Errorf("logging: file sink requires a path")
	}
	if maxSize <= 0 {
		return nil, fmt.Errorf("logging: file sink requires a positive maximum size")
	}
	if maxBackups < 0 {
		maxBackups = 0
	}
	sink := &FileSink{
		path:       path,
		maxSize:    maxSize,
		maxBackups: maxBackups,
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("logging: create log directory: %w", err)
		}
	}
	if err := sink.open(true); err != nil {
		return nil, err
	}
	return sink, nil
}

// Path returns the path of the active log file.
func (f *FileSink) Path() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.path
}

// Err returns the first error the sink encountered, or nil.
func (f *FileSink) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// Close closes the active log file. Further writes are ignored.
func (f *FileSink) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	if f.file == nil {
		return nil
	}
	err := f.file.Close()
	f.file = nil
	return err
}

// Write appends the record to the active file, rotating it first when the
// record would push the file past the maximum size.
func (f *FileSink) Write(rec Record) error {
	line, err := encodeFileRecord(rec)
	if err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.file == nil {
		return nil
	}
	if f.size > 0 && f.size+int64(len(line)) > f.maxSize {
		if err := f.rotate(); err != nil {
			f.recordErr(err)
			return err
		}
	}
	n, err := f.file.Write(line)
	f.size += int64(n)
	if err != nil {
		f.recordErr(err)
		return err
	}
	return nil
}

// recordErr stores the first error the sink encountered.
func (f *FileSink) recordErr(err error) {
	if f.err == nil {
		f.err = err
	}
}

// open opens the active log file. When append is true an existing file is
// kept, otherwise the file is truncated.
func (f *FileSink) open(append bool) error {
	flags := os.O_CREATE | os.O_WRONLY
	if append {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(f.path, flags, 0o600)
	if err != nil {
		return fmt.Errorf("logging: open log file: %w", err)
	}
	f.file = file
	f.size = 0
	if append {
		if info, err := file.Stat(); err == nil {
			f.size = info.Size()
		}
	}
	return nil
}

// rotate moves the active file aside and starts a new one.
func (f *FileSink) rotate() error {
	if f.file != nil {
		if err := f.file.Close(); err != nil {
			return fmt.Errorf("logging: close log file: %w", err)
		}
		f.file = nil
	}
	if f.maxBackups > 0 {
		_ = os.Remove(f.backupPath(f.maxBackups))
		for i := f.maxBackups - 1; i >= 1; i-- {
			source := f.backupPath(i)
			if _, err := os.Stat(source); err != nil {
				continue
			}
			if err := os.Rename(source, f.backupPath(i+1)); err != nil {
				return fmt.Errorf("logging: rotate log file: %w", err)
			}
		}
		if err := os.Rename(f.path, f.backupPath(1)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("logging: rotate log file: %w", err)
		}
	} else if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("logging: rotate log file: %w", err)
	}
	return f.open(false)
}

// backupPath returns the path of the n-th rotated file.
func (f *FileSink) backupPath(n int) string {
	return fmt.Sprintf("%s.%d", f.path, n)
}

// fileRecord is the JSON shape written by the file sink.
type fileRecord struct {
	Seq       uint64         `json:"seq"`
	Time      string         `json:"time"`
	Level     string         `json:"level"`
	Component string         `json:"component,omitempty"`
	Message   string         `json:"message"`
	Attrs     map[string]any `json:"attrs,omitempty"`
}

// encodeFileRecord renders a record as a single JSON line.
func encodeFileRecord(rec Record) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	line := fileRecord{
		Seq:       rec.Seq,
		Time:      rec.Time.Format(timeLayout),
		Level:     rec.Level.String(),
		Component: rec.Component,
		Message:   rec.Message,
		Attrs:     rec.Attrs,
	}
	if err := encoder.Encode(line); err != nil {
		return nil, fmt.Errorf("logging: encode log record: %w", err)
	}
	return buf.Bytes(), nil
}
