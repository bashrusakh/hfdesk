// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SyncOptions configures the sync operation.
type SyncOptions struct {
	// Clean removes orphaned symlinks in the friendly view
	Clean bool
	// Verbose prints detailed progress
	Verbose bool
}

// SyncResult contains statistics from a sync operation.
type SyncResult struct {
	ReposScanned    int
	SymlinksCreated int
	SymlinksUpdated int
	OrphansRemoved  int
	Errors          []error
}

// Sync regenerates the friendly view (models/, datasets/) from the hub cache.
// It scans all repos in hub/, reads their refs to find current commits,
// repairs snapshot entries that are missing (see repairSnapshotEntries), and
// creates friendly-view entries pointing at snapshot files.
func (c *HFCache) Sync(opts SyncOptions) (*SyncResult, error) {
	result := &SyncResult{}

	hubDir := c.HubDir()
	if _, err := os.Stat(hubDir); errors.Is(err, os.ErrNotExist) {
		return result, nil // Nothing to sync
	}

	// Scan hub/ for repo directories
	entries, err := os.ReadDir(hubDir)
	if err != nil {
		return nil, fmt.Errorf("read hub directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Parse repo directory name (models--owner--name or datasets--owner--name)
		repoType, owner, name, ok := parseRepoDirName(entry.Name())
		if !ok {
			continue // Not a valid repo directory
		}

		result.ReposScanned++

		repoDir, err := c.Repo(owner+"/"+name, repoType)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("parse repo %s: %w", entry.Name(), err))
			continue
		}

		// Sync this repo's friendly view
		created, updated, repoErrs := c.syncRepoFriendlyView(repoDir, opts)
		for _, e := range repoErrs {
			result.Errors = append(result.Errors, fmt.Errorf("sync %s: %w", entry.Name(), e))
		}

		result.SymlinksCreated += created
		result.SymlinksUpdated += updated
	}

	// Clean orphaned symlinks if requested
	if opts.Clean {
		removed, err := c.cleanOrphanedSymlinks(opts)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("clean orphans: %w", err))
		}
		result.OrphansRemoved = removed
	}

	return result, nil
}

// parseRepoDirName extracts repo type, owner, and name from a hub directory name.
// Format: models--owner--name or datasets--owner--name
func parseRepoDirName(dirName string) (RepoType, string, string, bool) {
	var repoType RepoType
	var rest string

	if strings.HasPrefix(dirName, "models--") {
		repoType = RepoTypeModel
		rest = strings.TrimPrefix(dirName, "models--")
	} else if strings.HasPrefix(dirName, "datasets--") {
		repoType = RepoTypeDataset
		rest = strings.TrimPrefix(dirName, "datasets--")
	} else {
		return "", "", "", false
	}

	// Split owner--name
	parts := strings.SplitN(rest, "--", 2)
	if len(parts) != 2 {
		return "", "", "", false
	}

	return repoType, parts[0], parts[1], true
}

// syncRepoFriendlyView syncs a single repo's friendly view.
// It first repairs snapshot entries that are missing (see repairSnapshotEntries),
// then mirrors the snapshot into the friendly view.
// Returns (created, updated, errors); per-entry failures are reported without
// aborting the remaining entries.
func (c *HFCache) syncRepoFriendlyView(repoDir *RepoDir, opts SyncOptions) (int, int, []error) {
	created := 0
	updated := 0
	var errs []error

	// Find the current commit from refs
	// Try common refs: main, master
	var commit string
	for _, ref := range []string{"main", "master"} {
		c, err := repoDir.ReadRef(ref)
		if err != nil {
			return 0, 0, []error{fmt.Errorf("read ref %s: %w", ref, err)}
		}
		if c != "" {
			commit = c
			break
		}
	}

	if commit == "" {
		// No refs found, try to find any snapshot
		snapshots, err := repoDir.ListSnapshots()
		if err != nil {
			return 0, 0, []error{fmt.Errorf("list snapshots: %w", err)}
		}
		if len(snapshots) > 0 {
			commit = snapshots[0] // Use first available snapshot
		}
	}

	if commit == "" {
		// Blobs-only cache (for example one downloaded on a filesystem where
		// link creation was skipped entirely): the offline download manifest
		// still names the commit its blobs belong to.
		if m := readRepoManifest(repoDir); m != nil {
			commit = m.Commit
		}
	}

	if commit == "" {
		return 0, 0, nil // Nothing to sync
	}

	// Recreate snapshot entries a blobs-only or partially repaired cache is
	// missing before mirroring them into the friendly view.
	repaired, repairErrs := repoDir.repairSnapshotEntries(commit)
	created += repaired
	errs = append(errs, repairErrs...)

	// Get snapshot directory
	snapshotDir, err := repoDir.SnapshotDir(commit)
	if err != nil {
		return created, updated, append(errs, err)
	}
	if _, err := os.Stat(snapshotDir); errors.Is(err, os.ErrNotExist) {
		return created, updated, errs // Snapshot doesn't exist
	}

	// Ensure friendly directory exists
	if err := repoDir.EnsureFriendlyDir(); err != nil {
		return created, updated, append(errs, fmt.Errorf("ensure friendly dir: %w", err))
	}

	// Walk snapshot and create friendly entries
	err = filepath.Walk(snapshotDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Get relative path within snapshot
		relPath, err := filepath.Rel(snapshotDir, path)
		if err != nil {
			return err
		}

		friendlyPath := filepath.Join(repoDir.FriendlyPath(), relPath)

		snapshotPath, perr := repoDir.SnapshotPath(commit, relPath)
		if perr != nil {
			return perr
		}
		expectedTarget, _ := filepath.Rel(filepath.Dir(friendlyPath), snapshotPath)

		// Check if the friendly entry already exists and is correct
		correct, cerr := cacheEntryCorrect(friendlyPath, snapshotPath, expectedTarget)
		if cerr != nil {
			errs = append(errs, fmt.Errorf("inspect friendly entry %s: %w", relPath, cerr))
			return nil
		}
		if correct {
			return nil
		}

		// Create or update entry
		_, statErr := os.Lstat(friendlyPath)
		if err := repoDir.createFriendlySymlinkCtx(context.Background(), commit, relPath, ""); err != nil {
			errs = append(errs, fmt.Errorf("create friendly entry for %s: %w", relPath, err))
			return nil
		}

		if statErr != nil {
			created++
		} else {
			updated++
		}

		return nil
	})

	if err != nil {
		return created, updated, append(errs, fmt.Errorf("walk snapshot: %w", err))
	}

	return created, updated, errs
}

// readRepoManifest returns the offline download manifest at this repo's
// friendly view root when it belongs to this repository. A manifest from a
// different repo must never be used as a repair mapping: its recorded paths
// would be placed into an unrelated snapshot.
func readRepoManifest(repoDir *RepoDir) *DownloadManifest {
	m, err := ReadManifest(filepath.Join(repoDir.FriendlyPath(), ManifestFilename))
	if err != nil {
		return nil
	}
	if !strings.EqualFold(m.Repo, repoDir.RepoID()) {
		return nil
	}
	return m
}

// repairSnapshotEntries recreates snapshot entries the manifest records for
// commit but the cache is missing (or holds as a dangling symlink). The
// manifest is the offline relative path -> blob mapping written at download
// time, so a blobs-only cache can be repaired without contacting the Hub.
// Entries whose blob is absent, whose recorded path would escape the snapshot
// directory, or whose manifest shape is unusable are skipped or reported;
// existing snapshot entries (of any link kind) are left untouched.
// Returns (created, errors).
func (r *RepoDir) repairSnapshotEntries(commit string) (int, []error) {
	created := 0
	var errs []error

	m := readRepoManifest(r)
	if m == nil || m.Commit != commit {
		return 0, nil
	}

	for _, f := range m.Files {
		rel := filepath.ToSlash(f.Name)
		if unsafeRepoPath(rel) {
			errs = append(errs, fmt.Errorf("manifest path %q is not a usable repository path", f.Name))
			continue
		}
		if !strings.HasPrefix(f.Blob, "blobs/") {
			errs = append(errs, fmt.Errorf("manifest blob %q for %q is not a cache blob reference", f.Blob, f.Name))
			continue
		}
		hash := strings.TrimPrefix(f.Blob, "blobs/")
		if !canonicalSHA256(hash) {
			errs = append(errs, fmt.Errorf("manifest blob %q for %q is not named by a canonical SHA-256", f.Blob, f.Name))
			continue
		}

		info, err := os.Lstat(r.BlobPath(hash))
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue // No usable blob to link from
		}

		linkPath, err := r.SnapshotPath(commit, rel)
		if err != nil {
			errs = append(errs, fmt.Errorf("manifest path %q would escape the snapshot directory: %w", f.Name, err))
			continue
		}
		if !snapshotEntryNeedsRepair(linkPath) {
			continue
		}
		if err := r.createSnapshotSymlink(context.Background(), commit, rel, hash); err != nil {
			errs = append(errs, fmt.Errorf("repair snapshot entry %s: %w", rel, err))
			continue
		}
		created++
	}

	return created, errs
}

// snapshotEntryNeedsRepair reports whether the snapshot entry at linkPath is
// missing or a dangling symlink. A hard link or copy carries its content and
// is left alone; repair recreates absent entries, it does not second-guess
// existing ones.
func snapshotEntryNeedsRepair(linkPath string) bool {
	info, err := os.Lstat(linkPath)
	if err != nil {
		return true
	}
	if info.Mode()&os.ModeSymlink != 0 {
		_, err := os.Stat(linkPath) // follow the link
		return errors.Is(err, os.ErrNotExist)
	}
	return false
}

// cleanOrphanedSymlinks removes symlinks in friendly view that point to non-existent files.
func (c *HFCache) cleanOrphanedSymlinks(opts SyncOptions) (int, error) {
	removed := 0

	// Clean models/
	modelsRemoved, err := cleanOrphansInDir(c.ModelsDir())
	if err != nil {
		return removed, fmt.Errorf("clean models: %w", err)
	}
	removed += modelsRemoved

	// Clean datasets/
	datasetsRemoved, err := cleanOrphansInDir(c.DatasetsDir())
	if err != nil {
		return removed, fmt.Errorf("clean datasets: %w", err)
	}
	removed += datasetsRemoved

	return removed, nil
}

// cleanOrphansInDir removes broken symlinks in a directory tree.
func cleanOrphansInDir(dir string) (int, error) {
	removed := 0

	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Skip errors (e.g., permission denied)
			return nil
		}

		// Check if it's a symlink
		if info.Mode()&os.ModeSymlink == 0 {
			return nil
		}

		// Check if symlink target exists
		target, err := os.Readlink(path)
		if err != nil {
			return nil // Can't read symlink, skip
		}

		// Resolve relative to symlink location
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}

		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			// Broken symlink, remove it
			if err := os.Remove(path); err != nil {
				return nil // Skip errors
			}
			removed++
		}

		return nil
	})

	if err != nil {
		return removed, err
	}

	// Clean up empty directories
	cleanEmptyDirs(dir)

	return removed, nil
}

// cleanEmptyDirs removes empty directories from bottom up.
func cleanEmptyDirs(dir string) {
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}

		// Try to remove (will fail if not empty)
		os.Remove(path)
		return nil
	})
}

// ListRepos returns all repositories in the cache.
func (c *HFCache) ListRepos() ([]*RepoDir, error) {
	var repos []*RepoDir

	hubDir := c.HubDir()
	if _, err := os.Stat(hubDir); errors.Is(err, os.ErrNotExist) {
		return repos, nil
	}

	entries, err := os.ReadDir(hubDir)
	if err != nil {
		return nil, fmt.Errorf("read hub directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		repoType, owner, name, ok := parseRepoDirName(entry.Name())
		if !ok {
			continue
		}

		repoDir, err := c.Repo(owner+"/"+name, repoType)
		if err != nil {
			continue
		}

		repos = append(repos, repoDir)
	}

	return repos, nil
}
