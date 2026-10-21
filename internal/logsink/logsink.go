// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

// Package logsink provides a small size-bounded rotating log file writer.
//
// A supervised HFDesk process writes its own log file. The sink keeps the
// on-disk size bounded by rotating the active file into a fixed number of
// generations and discarding the oldest one.
package logsink

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	// MaxFileSize is the maximum size of a single log file. A write that
	// would exceed it rotates the file first.
	MaxFileSize int64 = 2 << 20 // 2 MiB
	// MaxRotatedFiles is the number of rotated generations kept besides the
	// active file: hfdesk.log, hfdesk.log.1, hfdesk.log.2, hfdesk.log.3.
	MaxRotatedFiles = 3
	// FileName is the name of the active log file inside the log directory.
	FileName = "hfdesk.log"
	// DirMode is the permission mode of the log directory.
	DirMode os.FileMode = 0o700
	// FileMode is the permission mode of a log file.
	FileMode os.FileMode = 0o600
)

// Sink is a size-bounded rotating file writer. Safe for concurrent use.
type Sink struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

// Open creates dir (0700) if needed and opens <dir>/hfdesk.log (0600).
// An existing directory or file is tightened to the same restrictive modes.
func Open(dir string) (*Sink, error) {
	if dir == "" {
		return nil, errors.New("logsink: log directory is required")
	}
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return nil, fmt.Errorf("logsink: create log dir: %w", err)
	}
	if err := os.Chmod(dir, DirMode); err != nil {
		return nil, fmt.Errorf("logsink: restrict log dir: %w", err)
	}

	path := filepath.Join(dir, FileName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, FileMode)
	if err != nil {
		return nil, fmt.Errorf("logsink: open log file: %w", err)
	}
	if err := file.Chmod(FileMode); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("logsink: restrict log file: %w", err)
	}

	s := &Sink{path: path, file: file}
	if info, err := file.Stat(); err == nil {
		s.size = info.Size()
	}
	return s, nil
}

// Write implements io.Writer under a mutex. The active file is rotated
// before a write that would exceed MaxFileSize. A single write larger than
// the limit is written whole instead of being split.
func (s *Sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file == nil {
		return 0, os.ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	// An empty active file has nothing to rotate: an oversized single write
	// is written whole rather than looping forever.
	if s.size > 0 && s.size+int64(len(p)) > MaxFileSize {
		if err := s.rotate(); err != nil {
			return 0, err
		}
	}

	n, err := s.file.Write(p)
	s.size += int64(n)
	return n, err
}

// Path returns the active log file path.
func (s *Sink) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// Close flushes and closes the file.
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// rotate shifts the generations: the active file becomes .1, .1 becomes .2
// and .2 becomes .3. The oldest generation is discarded, so the on-disk size
// stays bounded. The caller must hold the mutex.
func (s *Sink) rotate() error {
	if err := os.Remove(s.generation(MaxRotatedFiles)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("logsink: remove oldest log: %w", err)
	}
	for i := MaxRotatedFiles - 1; i >= 1; i-- {
		if err := os.Rename(s.generation(i), s.generation(i+1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("logsink: rotate log generation %d: %w", i, err)
		}
	}

	if err := s.file.Close(); err != nil {
		return fmt.Errorf("logsink: close log before rotation: %w", err)
	}
	s.file = nil

	if err := os.Rename(s.path, s.generation(1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		// The active file was not replaced: keep the sink usable.
		s.reopenActive()
		return fmt.Errorf("logsink: rotate log: %w", err)
	}

	file, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_APPEND, FileMode)
	if err != nil {
		return fmt.Errorf("logsink: reopen log after rotation: %w", err)
	}
	if err := file.Chmod(FileMode); err != nil {
		_ = file.Close()
		return fmt.Errorf("logsink: restrict log file: %w", err)
	}
	s.file = file
	s.size = 0
	return nil
}

// reopenActive reopens the active file for appending after a failed
// rotation. The caller must hold the mutex.
func (s *Sink) reopenActive() {
	file, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, FileMode)
	if err != nil {
		return
	}
	s.file = file
	if info, err := file.Stat(); err == nil {
		s.size = info.Size()
	}
}

// generation returns the path of a rotated generation of the active file.
func (s *Sink) generation(n int) string {
	return fmt.Sprintf("%s.%d", s.path, n)
}
