// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// Cache entries (snapshots/<commit>/... and the friendly models/ and
// datasets/ views) are published with a link cascade: symlink first (the
// classic HF layout), then a hard link to the same payload, then an atomic
// copy. Probing beats GOOS checks: symlinks can fail on non-Windows
// filesystems and can succeed on Windows with Developer Mode, and a hard
// link can fail across volumes even when the filesystem supports links.

// Link primitives are package variables so tests can simulate filesystems
// without symlink or hardlink support (and cross-volume hardlink failures)
// without needing a real Windows box.
var (
	symlinkFn  = os.Symlink
	hardlinkFn = os.Link
)

// linkNoticeOut receives the once-per-root fallback notices; tests swap it to
// capture the notice without polluting stderr.
var linkNoticeOut io.Writer = os.Stderr

// cacheLinkMode names the placement kind actually used for one cache entry.
type cacheLinkMode int

const (
	cacheLinkSymlink cacheLinkMode = iota + 1
	cacheLinkHardlink
	cacheLinkCopy
)

func (m cacheLinkMode) String() string {
	switch m {
	case cacheLinkSymlink:
		return "symlink"
	case cacheLinkHardlink:
		return "hard link"
	case cacheLinkCopy:
		return "file copy"
	}
	return "unknown link mode"
}

// linkRootState memoizes, per placement root, which link kinds have already
// failed, so a failing os.Symlink/os.Link is attempted once per root instead
// of being retried for every file. A per-file failure still falls back for
// that file without being treated as a capability probe result again.
type linkRootState struct {
	symlinkFailed  bool
	hardlinkFailed bool
	announced      cacheLinkMode
}

var (
	linkRootsMu sync.Mutex
	linkRoots   = map[string]*linkRootState{}
)

func linkRootStateFor(root string) *linkRootState {
	linkRootsMu.Lock()
	defer linkRootsMu.Unlock()
	state := linkRoots[root]
	if state == nil {
		state = &linkRootState{}
		linkRoots[root] = state
	}
	return state
}

func linkKindFailed(root string, state *linkRootState, mode cacheLinkMode) {
	linkRootsMu.Lock()
	defer linkRootsMu.Unlock()
	switch mode {
	case cacheLinkSymlink:
		state.symlinkFailed = true
	case cacheLinkHardlink:
		state.hardlinkFailed = true
	}
}

// announceLinkFallback logs one clear notice per root naming the fallback in
// use, when a fallback is actually used. It replaces the old unconditional
// "symlinks not supported on Windows" warning, which was both misleading (it
// fired on a GOOS check instead of an actual failure) and hid what happened
// to the cache entries.
func announceLinkFallback(root string, state *linkRootState, mode cacheLinkMode) {
	if mode == cacheLinkSymlink {
		return
	}
	linkRootsMu.Lock()
	already := state.announced == mode
	if !already {
		state.announced = mode
	}
	linkRootsMu.Unlock()
	if already {
		return
	}
	if mode == cacheLinkHardlink {
		fmt.Fprintf(linkNoticeOut, "[WARN] %s: symlinks unavailable; cache entries are created as hard links instead\n", root)
		return
	}
	fmt.Fprintf(linkNoticeOut, "[WARN] %s: symlinks and hard links unavailable; cache entries are created as file copies instead\n", root)
}

// placeCacheEntry publishes src at dst using the link cascade and reports the
// kind used. relTarget is the relative symlink target; it is used verbatim
// only for the symlink step (the classic HF relative-link layout). The
// hardlink and copy steps read src, so they resolve it through
// linkSourcePath, which refuses sources that resolve outside the repository.
// A copy is staged and renamed (copyFileAtomicCtx), so a failed or cancelled
// copy never leaves a partial file at dst.
func (r *RepoDir) placeCacheEntry(ctx context.Context, linkRoot, dst, src, relTarget string) (cacheLinkMode, error) {
	state := linkRootStateFor(linkRoot)

	linkRootsMu.Lock()
	trySymlink := !state.symlinkFailed
	tryHardlink := !state.hardlinkFailed
	linkRootsMu.Unlock()

	if trySymlink {
		if err := symlinkFn(relTarget, dst); err == nil {
			return cacheLinkSymlink, nil
		} else {
			linkKindFailed(linkRoot, state, cacheLinkSymlink)
		}
	}
	if tryHardlink {
		source, err := r.linkSourcePath(src)
		if err == nil {
			if err := hardlinkFn(source, dst); err == nil {
				announceLinkFallback(linkRoot, state, cacheLinkHardlink)
				return cacheLinkHardlink, nil
			} else {
				linkKindFailed(linkRoot, state, cacheLinkHardlink)
			}
		}
	}
	source, err := r.linkSourcePath(src)
	if err != nil {
		return cacheLinkCopy, err
	}
	if err := copyFileAtomicCtx(ctx, source, dst); err != nil {
		return cacheLinkCopy, err
	}
	announceLinkFallback(linkRoot, state, cacheLinkCopy)
	return cacheLinkCopy, nil
}

// linkSourcePath resolves the file a hard link or copy must be created from.
// Symlink leaves are resolved first: a hard link to a symlink leaf would pin
// the link text, which is relative to a different directory and would resolve
// to nothing at the new location. The resolved file must stay inside this
// repository: a symlink target is an inert string, but hard linking and
// copying read the source, so an entry pointing outside the cache must never
// be materialized (it would import out-of-root data into the cache). A cache
// root reached through a directory alias is resolved on both sides before
// the containment check, so symlinked Hub roots keep working.
func (r *RepoDir) linkSourcePath(name string) (string, error) {
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", err
	}
	root := r.Path()
	if resolvedRoot, rerr := filepath.EvalSymlinks(root); rerr == nil {
		root = resolvedRoot
	}
	if !PathInside(root, resolved) {
		return "", fmt.Errorf("cache entry source %q resolves outside the repository", name)
	}
	return resolved, nil
}

// unsafeBlobFileName reports whether a blob key cannot be used as a plain
// file name inside blobs/. BlobPath already contains malformed names to a
// fixed placeholder via SafeJoin, but a snapshot entry must name a real blob
// key, so placement rejects empty and non-local keys explicitly instead of
// linking or copying the placeholder. Unlike unsafeBlobName this does not
// demand the canonical 64-hex form: opaque keys such as the reserved
// staging-shaped names the public storage API accepts remain valid names
// here (the selected-GGUF delete path, not placement, refuses those).
func unsafeBlobFileName(name string) bool {
	if name == "" || strings.ContainsAny(name, `/\`) || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return true
	}
	cleaned := path.Clean(name)
	return cleaned != name || cleaned == "." || cleaned == ".."
}

// entryContentSHA256 returns the SHA-256 of a file's content, following a
// symlink leaf the way a reader would. It is the content-identity proof used
// for entries placed by the copy fallback, where no link relationship exists.
func entryContentSHA256(name string) (string, error) {
	return computeSHA256(name)
}

// sameEntryContent reports whether a and b hold identical bytes. Shared file
// identity (hard links) is recognized first, then size, then a full digest
// comparison. Callers accept the digest read cost only for copy-placed
// entries, where content equality is the sole available correctness proof.
func sameEntryContent(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if os.SameFile(ai, bi) {
		return true, nil
	}
	if ai.Size() != bi.Size() {
		return false, nil
	}
	ah, err := entryContentSHA256(a)
	if err != nil {
		return false, err
	}
	bh, err := entryContentSHA256(b)
	if err != nil {
		return false, err
	}
	return ah == bh, nil
}

// cacheEntryCorrect reports whether the cache entry at entryPath already
// mirrors sourcePath the way placement would publish it: a symlink with the
// expected relative target, a hard link sharing the source's file, or a copy
// with identical content. It is the already-correct guard used by Sync (leave
// a correct friendly projection alone) and by placement (do not remove and
// re-place a correct entry: under the copy fallback that would re-copy full
// content per entry and briefly leave no entry at all, so a failed re-placement
// could lose a previously good entry).
func cacheEntryCorrect(entryPath, sourcePath, expectedTarget string) (bool, error) {
	info, err := os.Lstat(entryPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(entryPath)
		if err != nil {
			return false, err
		}
		return target == expectedTarget, nil
	}
	if sourceInfo, err := os.Stat(sourcePath); err == nil && os.SameFile(info, sourceInfo) {
		return true, nil
	}
	return sameEntryContent(entryPath, sourcePath)
}
