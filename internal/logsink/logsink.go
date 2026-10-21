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
	"io/fs"
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
//
// The mode of a directory or of a file that already exists is left alone.
// The log directory of an instance may be a directory that existed before
// the installation and that belongs to someone else: the instance may only
// be able to write there. Tightening it needs an ownership the installation
// never checked, and it would keep the instance from starting at all.
func Open(dir string) (*Sink, error) {
	if dir == "" {
		return nil, errors.New("logsink: log directory is required")
	}
	if err := createDir(dir, DirMode); err != nil {
		return nil, fmt.Errorf("logsink: create log dir: %w", err)
	}

	path := filepath.Join(dir, FileName)
	file, err := createFile(path, os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return nil, fmt.Errorf("logsink: open log file: %w", err)
	}

	s := &Sink{path: path, file: file}
	if info, err := file.Stat(); err == nil {
		s.size = info.Size()
	}
	return s, nil
}

// createDir creates the directory and the missing parents of it.
//
// A directory that already exists keeps its mode: it belongs to whoever
// named it, and the instance may not own it. The directories created here
// belong to this process, so their mode is set explicitly: the mode of a
// create call is filtered by the umask of the process.
func createDir(path string, mode os.FileMode) error {
	missing, err := missingDirs(path)
	if err != nil {
		return err
	}
	// The directories are created from the top down, so every parent
	// exists before its child.
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], mode); err != nil {
			if errors.Is(err, fs.ErrExist) {
				// Another process created the directory in the
				// meantime: its mode belongs to whoever created it.
				continue
			}
			return err
		}
		if err := os.Chmod(missing[i], mode); err != nil {
			return err
		}
	}
	return nil
}

// missingDirs returns the directories of path that do not exist yet, the
// closest one first.
//
// The path itself has to be a directory: a file at that path is reported
// here instead of surfacing later as an unusable log file.
func missingDirs(path string) ([]string, error) {
	var missing []string
	for p := path; ; {
		info, err := os.Stat(p)
		if err == nil {
			if !info.IsDir() {
				return nil, fmt.Errorf("%q exists and is not a directory", p)
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return missing, nil
}

// createFile opens the file at path for writing and creates it with the
// owner-only mode when it is missing.
//
// The mode of a file that already exists is left alone: the log file may
// belong to another account that only granted the instance write access,
// and a chmod on such a file fails with "operation not permitted".
func createFile(path string, flag int) (*os.File, error) {
	file, err := os.OpenFile(path, flag|os.O_CREATE|os.O_EXCL, FileMode)
	if err == nil {
		// The file was created here, so it belongs to this process: the
		// umask of the process may have filtered the mode, so it is set
		// explicitly.
		if err := file.Chmod(FileMode); err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	// The file already exists: it is opened and never tightened. A file
	// that vanished between the two calls is created here instead, with
	// the mode of the create call, which the umask of the process may
	// have narrowed.
	return os.OpenFile(path, flag|os.O_CREATE, FileMode)
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

	file, err := createFile(s.path, os.O_WRONLY|os.O_TRUNC|os.O_APPEND)
	if err != nil {
		return fmt.Errorf("logsink: reopen log after rotation: %w", err)
	}
	s.file = file
	s.size = 0
	return nil
}

// reopenActive reopens the active file for appending after a failed
// rotation. The caller must hold the mutex.
func (s *Sink) reopenActive() {
	file, err := createFile(s.path, os.O_WRONLY|os.O_APPEND)
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
