// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// rebuildCoordinationContentionHook is an unexported deterministic test seam;
// production leaves it nil.
var rebuildCoordinationContentionHook func(string, bool)
var rebuildCoordinationAdmissionLeaseAcquiredHook func(string, bool)

// RebuildCoordinationPath returns the OS-private data-lease file for the
// selected root's current filesystem object and canonical physical path.
func (c *HFCache) RebuildCoordinationPath() string {
	path, err := rebuildCoordinationFilePath(c.Root)
	if err != nil {
		return ""
	}
	return path
}

// AcquireRebuildCoordinationLease serializes one selected root with the Go
// rebuild owner. Downloads use shared leases; Sync and recursive delete use
// exclusive data leases. Its per-root OS-temp key uses the current physical
// object and canonical path, independent of ambient HOME and service ownership.
func (c *HFCache) AcquireRebuildCoordinationLease(exclusive bool) (func(), error) {
	return AcquireRebuildCoordinationLeases([]string{c.Root}, exclusive)
}

// AcquireRebuildCoordinationLeaseContext is the cancellable single-root form
// used by HTTP operations waiting for another app/CLI mutation.
func (c *HFCache) AcquireRebuildCoordinationLeaseContext(ctx context.Context, exclusive bool) (func(), error) {
	return acquireCompleteRebuildCoordinationLeases(ctx, []string{c.Root}, exclusive, nil)
}

// AcquireRebuildCoordinationLeases fences a rebuild or download against the
// other supported Go actor (app or CLI) for each selected physical root. It
// holds both the data lease and the service-claim admission lease. Their files
// live only in the per-user coordination directory, never in model/cache paths
// or service ownership metadata. Shared leases allow independent downloads to
// remain concurrent; rebuild/recursive delete take exclusive data leases.
func AcquireRebuildCoordinationLeases(roots []string, exclusive bool) (func(), error) {
	return acquireCompleteRebuildCoordinationLeases(context.Background(), roots, exclusive, nil)
}

func acquireCompleteRebuildCoordinationLeases(ctx context.Context, roots []string, exclusive bool, onWait func()) (func(), error) {
	dataRelease, err := acquireRebuildCoordinationLeases(ctx, roots, exclusive, onWait)
	if err != nil {
		return nil, err
	}
	admissionRelease, err := acquireRebuildAdmissionLeases(ctx, roots, exclusive, onWait)
	if err != nil {
		dataRelease()
		return nil, err
	}
	return func() {
		admissionRelease()
		dataRelease()
	}, nil
}

func acquireRebuildCoordinationLeases(ctx context.Context, roots []string, exclusive bool, onWait func()) (func(), error) {
	return acquirePhysicalRootLeases(ctx, roots, exclusive, onWait, false)
}

func acquireRebuildAdmissionLeases(ctx context.Context, roots []string, exclusive bool, onWait func()) (func(), error) {
	return acquirePhysicalRootLeases(ctx, roots, exclusive, onWait, true)
}

func acquirePhysicalRootLeases(ctx context.Context, roots []string, exclusive bool, onWait func(), admission bool) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	paths := make(map[string]string, len(roots))
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if root == "" {
			continue
		}
		physical, err := resolvedPathForCreate(root)
		if err != nil {
			return nil, fmt.Errorf("resolve rebuild coordination root %q: %w", root, err)
		}
		info, err := os.Stat(physical)
		if errors.Is(err, os.ErrNotExist) {
			// The physical spelling of the selected root is created instead of
			// its original spelling: it names the same directory, and the
			// resolver already proved which location that is.
			if err := os.MkdirAll(physical, 0o755); err != nil {
				return nil, fmt.Errorf("create rebuild coordination root %q: %w", root, err)
			}
			physical, err = resolvedPathForCreate(root)
			if err != nil {
				return nil, fmt.Errorf("resolve created rebuild coordination root %q: %w", root, err)
			}
			info, err = os.Stat(physical)
		}
		if err != nil {
			return nil, fmt.Errorf("inspect rebuild coordination root %q: %w", root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("rebuild coordination root %q is not a directory", root)
		}
		identity, err := rebuildCoordinationIdentity(physical)
		if err != nil {
			return nil, fmt.Errorf("identify rebuild coordination root %q: %w", root, err)
		}
		lockPath, err := coordinationFilePathForIdentity(ctx, identity, admission)
		if err != nil {
			return nil, err
		}
		paths[identity] = lockPath
	}
	ordered := make([]string, 0, len(paths))
	for key := range paths {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	var releases []func()
	for _, key := range ordered {
		lockPath := paths[key]
		file, err := openRebuildCoordinationFile(ctx, lockPath, key, admission)
		if err != nil {
			releaseAll(releases)
			return nil, err
		}
		wait := func() {
			if onWait != nil {
				onWait()
			}
			if rebuildCoordinationContentionHook != nil {
				rebuildCoordinationContentionHook(lockPath, exclusive)
			}
		}
		release, err := acquireBranchVariantOSFileLock(ctx, file, exclusive, wait)
		if err != nil {
			_ = file.Close()
			releaseAll(releases)
			return nil, fmt.Errorf("acquire rebuild coordination lease %q: %w", lockPath, err)
		}
		current, verifyErr := os.Lstat(lockPath)
		opened, statErr := file.Stat()
		if verifyErr != nil || statErr != nil || !current.Mode().IsRegular() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, opened) {
			release()
			releaseAll(releases)
			if verifyErr != nil {
				return nil, fmt.Errorf("verify rebuild coordination object %q: %w", lockPath, verifyErr)
			}
			if statErr != nil {
				return nil, fmt.Errorf("inspect rebuild coordination object %q: %w", lockPath, statErr)
			}
			return nil, fmt.Errorf("rebuild coordination object %q changed while acquiring its lease", lockPath)
		}
		if err := ctx.Err(); err != nil {
			release()
			releaseAll(releases)
			return nil, err
		}
		releases = append(releases, release)
		if admission && rebuildCoordinationAdmissionLeaseAcquiredHook != nil {
			rebuildCoordinationAdmissionLeaseAcquiredHook(key, exclusive)
		}
	}
	return func() { releaseAll(releases) }, nil
}

func rebuildCoordinationFilePath(root string) (string, error) {
	physical, err := resolvedPathForCreate(root)
	if err != nil {
		return "", fmt.Errorf("resolve rebuild coordination root %q: %w", root, err)
	}
	identity, err := rebuildCoordinationIdentity(physical)
	if err != nil {
		return "", fmt.Errorf("identify rebuild coordination root %q: %w", root, err)
	}
	return coordinationFilePathForIdentity(context.Background(), identity, false)
}

func rebuildCoordinationAdmissionFilePath(root string) (string, error) {
	physical, err := resolvedPathForCreate(root)
	if err != nil {
		return "", fmt.Errorf("resolve rebuild admission root %q: %w", root, err)
	}
	identity, err := rebuildCoordinationIdentity(physical)
	if err != nil {
		return "", fmt.Errorf("identify rebuild admission root %q: %w", root, err)
	}
	return coordinationFilePathForIdentity(context.Background(), identity, true)
}

// rebuildCoordinationIdentity combines the current filesystem object ID with
// its canonical physical path. Symlink/route aliases resolve to the same key;
// a later temporary root reusing a deleted directory's inode does not inherit
// that root's stale coordinator entry.
func rebuildCoordinationIdentity(physical string) (string, error) {
	objectID, err := branchVariantFilesystemIdentity(physical)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(physical)
	if err != nil {
		return "", err
	}
	pathID, err := branchVariantPathLockKey(canonical)
	if err != nil {
		return "", err
	}
	return objectID + "\x00" + pathID, nil
}

func coordinationFilePathForIdentity(ctx context.Context, identity string, admission bool) (string, error) {
	dir, err := ensurePrivateCoordinationDirectory(ctx)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(identity))
	suffix := ".lock"
	if admission {
		suffix = ".admission.lock"
	}
	return filepath.Join(dir, hex.EncodeToString(digest[:])+suffix), nil
}

func releaseAll(releases []func()) {
	for i := len(releases) - 1; i >= 0; i-- {
		releases[i]()
	}
}

func existingRebuildCoordinationRoots(roots []string) []string {
	existing := make([]string, 0, len(roots))
	for _, root := range roots {
		if root == "" {
			continue
		}
		abs, err := rootedPath(root)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
			continue
		}
		existing = append(existing, root)
	}
	return existing
}
