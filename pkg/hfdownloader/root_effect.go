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
	"sort"
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

// walkOwnedEntries observes one configured namespace slot while excluding
// entries owned by nested roots. Directory identities are revisited through
// distinct paths because those paths can expose different namespace regions.
func (set *ManagedRootSet) walkOwnedEntries(rootID, start string, expectedRoot os.FileInfo, fn filepath.WalkFunc, boundaryFn func(string, os.FileInfo, string) error, chargeComparisons, chargeWork func(int) error) error {
	root, ok := set.Root(rootID)
	if !ok {
		return fmt.Errorf("unknown managed root %q", rootID)
	}
	comparisons, work := 0, 0
	if chargeComparisons == nil {
		chargeComparisons = func(n int) error {
			comparisons += n
			if comparisons > maxNamespaceIdentityComparisons {
				return fmt.Errorf("owned walk identity comparisons exceed %d", maxNamespaceIdentityComparisons)
			}
			return nil
		}
	}
	if chargeWork == nil {
		chargeWork = func(n int) error {
			work += n
			if work > maxNamespaceWork {
				return fmt.Errorf("owned walk exceeds %d entries", maxNamespaceWork)
			}
			return nil
		}
	}
	observer := set.effectObserver()
	active := make([]os.FileInfo, 0, 16)
	var visit func(string, int, os.FileInfo) error
	visit = func(path string, depth int, expected os.FileInfo) (resultErr error) {
		if depth > maxEffectDepth {
			return fmt.Errorf("owned walk depth exceeds %d", maxEffectDepth)
		}
		owned, err := set.ownedWalkPath(root, path, chargeComparisons)
		if err != nil {
			return err
		}
		if !owned {
			if expected == nil || !expected.IsDir() || boundaryFn == nil {
				return nil
			}
			dir, openErr := observer.openDir(path)
			if openErr != nil {
				return fmt.Errorf("open ownership boundary %q: %w", path, openErr)
			}
			info, statErr := dir.stat()
			closeErr := dir.close()
			if statErr != nil {
				return fmt.Errorf("identify ownership boundary %q: %w", path, statErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close ownership boundary %q: %w", path, closeErr)
			}
			if !info.IsDir() || !os.SameFile(info, expected) {
				return fmt.Errorf("ownership boundary changed during observation: %q", path)
			}
			owner, ownerErr := set.OwnerForPath(path)
			if ownerErr != nil {
				return fmt.Errorf("resolve ownership boundary %q: %w", path, ownerErr)
			}
			return boundaryFn(path, info, owner.ID)
		}
		dir, err := observer.openDir(path)
		if err != nil {
			return fmt.Errorf("open owned directory %q: %w", path, err)
		}
		defer func() {
			if closeErr := dir.close(); resultErr == nil && closeErr != nil {
				resultErr = fmt.Errorf("close owned directory %q: %w", path, closeErr)
			}
		}()
		info, err := dir.stat()
		if err != nil {
			return fmt.Errorf("identify owned directory %q: %w", path, err)
		}
		if !info.IsDir() || !os.SameFile(info, info) {
			return fmt.Errorf("owned directory identity is unknown: %q", path)
		}
		if expected != nil {
			if err := chargeComparisons(1); err != nil {
				return err
			}
			if !os.SameFile(info, expected) {
				return fmt.Errorf("owned directory changed during observation: %q", path)
			}
		}
		for _, ancestor := range active {
			if err := chargeComparisons(1); err != nil {
				return err
			}
			if os.SameFile(ancestor, info) {
				return fmt.Errorf("owned directory cycle at %q", path)
			}
		}
		active = append(active, info)
		defer func() { active = active[:len(active)-1] }()
		if err := chargeWork(1); err != nil {
			return err
		}
		if fn != nil {
			if err := fn(path, info, nil); err != nil {
				if errors.Is(err, filepath.SkipDir) {
					return nil
				}
				return err
			}
		}
		var entries []namespaceEntry
		for {
			batch, readErr := dir.readDir(namespaceBatch)
			for _, entry := range batch {
				if err := chargeWork(1); err != nil {
					return err
				}
				entryInfo, infoErr := entry.Info()
				if infoErr != nil {
					return fmt.Errorf("identify owned entry %q: %w", filepath.Join(path, entry.Name()), infoErr)
				}
				entries = append(entries, namespaceEntry{name: entry.Name(), info: entryInfo})
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return fmt.Errorf("read owned directory %q: %w", path, readErr)
			}
			if len(batch) == 0 {
				return fmt.Errorf("read owned directory %q made no progress without EOF", path)
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
		for _, entry := range entries {
			entryPath := filepath.Join(path, entry.name)
			if entry.info.IsDir() && entry.info.Mode()&os.ModeSymlink == 0 {
				if err := visit(entryPath, depth+1, entry.info); err != nil {
					return err
				}
				continue
			}
			currentInfo, statErr := os.Lstat(entryPath)
			if statErr != nil {
				return fmt.Errorf("reinspect owned entry %q: %w", entryPath, statErr)
			}
			if err := chargeComparisons(1); err != nil {
				return err
			}
			if !os.SameFile(currentInfo, entry.info) {
				return fmt.Errorf("owned entry changed during observation: %q", entryPath)
			}
			owned, err := set.ownedWalkPath(root, entryPath, chargeComparisons)
			if err != nil {
				return err
			}
			if owned && fn != nil {
				if err := fn(entryPath, entry.info, nil); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return visit(ConfiguredPath(start, set.base), 0, expectedRoot)
}

func (set *ManagedRootSet) ownedWalkPath(root ManagedRoot, path string, chargeComparisons func(int) error) (bool, error) {
	if err := chargeComparisons(len(set.roots)); err != nil {
		return false, err
	}
	owner, err := set.OwnerForPath(path)
	if err != nil {
		return false, fmt.Errorf("resolve owned path %q: %w", path, err)
	}
	if owner.ID == root.ID {
		return true, nil
	}
	if err := chargeComparisons(1); err != nil {
		return false, err
	}
	same, err := set.SameDirectoryObjectFacts(owner.AbsolutePath, root.AbsolutePath)
	if err != nil {
		return false, err
	}
	return same, nil
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
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				_ = dir.close()
				return fmt.Errorf("read effect directory %q: %w", path, readErr)
			}
			// A short successful batch is not proof of exhaustion. Readers may
			// omit entries that disappear while being enumerated; keep reading
			// until explicit EOF, but reject zero-progress success to avoid a loop.
			if len(entries) == 0 {
				_ = dir.close()
				return fmt.Errorf("read effect directory %q made no progress without EOF", path)
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
	info     os.FileInfo
	existing bool
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
			return protectedDirectory{info: info, existing: current == path}, nil
		}
		if !os.IsNotExist(err) {
			return protectedDirectory{}, fmt.Errorf("inspect protected root %q: %w", current, err)
		}
		// Windows may report ENOTDIR for a missing descendant as well as for
		// a path below a file. Continue toward the volume root: a real file
		// prefix is rejected when reached; an absent suffix can anchor here.
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
