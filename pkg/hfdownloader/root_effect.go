// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// effectObserver is a private seam for coherent namespace observations. The
// production implementation uses opened directories and their actual entries;
// tests can model multiple namespace views of one real directory object.
type effectObserver interface {
	openDir(string) (effectDirectory, error)
	statDir(string) (os.FileInfo, error)
}

type effectDirectory interface {
	stat() (os.FileInfo, error)
	readDir(int) ([]os.DirEntry, error)
	close() error
}

type osEffectObserver struct{}
type osEffectDirectory struct{ file *os.File }

func (osEffectObserver) openDir(path string) (effectDirectory, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return osEffectDirectory{file: f}, nil
}
func (osEffectObserver) statDir(path string) (os.FileInfo, error) { return os.Stat(path) }
func (d osEffectDirectory) stat() (os.FileInfo, error)            { return d.file.Stat() }
func (d osEffectDirectory) readDir(n int) ([]os.DirEntry, error)  { return d.file.ReadDir(n) }
func (d osEffectDirectory) close() error                          { return d.file.Close() }

const (
	maxEffectDirectories = 100000
	maxEffectDepth       = 256
	effectReadBatch      = 128
)

type reachedDirectory struct {
	path string
	info os.FileInfo
}

// observeEffectDirectories follows the directory-entry recursion used by
// Go's RemoveAll. Symlinks are leaves. Repeated object identities are visited
// again at distinct namespace paths because their child mount views may differ.
func (set *ManagedRootSet) observeEffectDirectories(root string) ([]reachedDirectory, error) {
	observer := set.effectObserver()
	var reached []reachedDirectory
	active := make([]os.FileInfo, 0, 16)
	work := 0
	var visit func(string, int) error
	visit = func(path string, depth int) error {
		if depth > maxEffectDepth {
			return fmt.Errorf("effect observation depth exceeds %d", maxEffectDepth)
		}
		dir, err := observer.openDir(path)
		if err != nil {
			return fmt.Errorf("open effect directory %q: %w", path, err)
		}
		info, statErr := dir.stat()
		if statErr != nil {
			_ = dir.close()
			return fmt.Errorf("identify effect directory %q: %w", path, statErr)
		}
		if !info.IsDir() || !os.SameFile(info, info) {
			_ = dir.close()
			return fmt.Errorf("effect directory identity is not established for %q", path)
		}
		for _, ancestor := range active {
			if os.SameFile(ancestor, info) {
				_ = dir.close()
				return fmt.Errorf("effect directory cycle at %q", path)
			}
		}
		work++
		if work > maxEffectDirectories {
			_ = dir.close()
			return fmt.Errorf("effect observation exceeds %d directories", maxEffectDirectories)
		}
		reached = append(reached, reachedDirectory{path: path, info: info})
		active = append(active, info)
		defer func() { active = active[:len(active)-1] }()
		for {
			entries, readErr := dir.readDir(effectReadBatch)
			for _, entry := range entries {
				work++
				if work > maxEffectDirectories {
					_ = dir.close()
					return fmt.Errorf("effect observation exceeds %d entries", maxEffectDirectories)
				}
				entryInfo, infoErr := entry.Info()
				if infoErr != nil {
					_ = dir.close()
					return fmt.Errorf("identify effect entry %q: %w", filepath.Join(path, entry.Name()), infoErr)
				}
				// RemoveAll unlinks symlink entries rather than traversing them.
				if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.IsDir() {
					continue
				}
				if err := visit(filepath.Join(path, entry.Name()), depth+1); err != nil {
					_ = dir.close()
					return err
				}
			}
			if errors.Is(readErr, fs.ErrNotExist) {
				_ = dir.close()
				return fmt.Errorf("effect directory disappeared during observation %q: %w", path, readErr)
			}
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				_ = dir.close()
				return fmt.Errorf("read effect directory %q: %w", path, readErr)
			}
			if len(entries) < effectReadBatch || readErr != nil {
				break
			}
		}
		if err := dir.close(); err != nil {
			return fmt.Errorf("close effect directory %q: %w", path, err)
		}
		return nil
	}
	if err := visit(root, 0); err != nil {
		return nil, err
	}
	return reached, nil
}

type protectedDirectory struct {
	info os.FileInfo
}

// protectedDirectoryFact retains either the existing protected directory or
// the deepest existing directory object anchoring an ordinary missing suffix.
func protectedDirectoryFact(root ManagedRoot, observer effectObserver) (protectedDirectory, error) {
	path := filepath.Clean(root.AbsolutePath)
	if _, _, err := resolveObservedPath(path); err != nil {
		return protectedDirectory{}, fmt.Errorf("resolve protected root %q: %w", path, err)
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := observer.statDir(current)
		if err == nil {
			if !info.IsDir() || !os.SameFile(info, info) {
				return protectedDirectory{}, fmt.Errorf("protected root identity is not established: %q", current)
			}
			return protectedDirectory{info: info}, nil
		}
		if !os.IsNotExist(err) {
			return protectedDirectory{}, fmt.Errorf("inspect protected root %q: %w", current, err)
		}
		if errors.Is(err, syscall.ENOTDIR) {
			return protectedDirectory{}, fmt.Errorf("protected root has a non-directory path component: %q", current)
		}
		if filepath.Dir(current) == current {
			return protectedDirectory{}, fmt.Errorf("no existing directory anchor for protected root %q", path)
		}
	}
}

func (set *ManagedRootSet) effectObserver() effectObserver {
	if set.observe != nil {
		return set.observe
	}
	return osEffectObserver{}
}

func effectIntersectsProtected(reached []reachedDirectory, protected protectedDirectory, budget *int) (bool, error) {
	for _, dir := range reached {
		*budget = *budget - 1
		if *budget < 0 {
			return false, fmt.Errorf("protected-root comparison budget exceeded")
		}
		if os.SameFile(dir.info, protected.info) {
			return true, nil
		}
	}
	return false, nil
}
