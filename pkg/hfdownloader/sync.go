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
// creates friendly-view entries pointing at snapshot files. Every entry it
// creates, replaces or removes and every directory it prunes does so under the
// shared cache-mutation admission, so a second HFDesk instance holding an
// entry's scope excludes this one.
func (c *HFCache) Sync(opts SyncOptions) (*SyncResult, error) {
	result := &SyncResult{}

	hubDir := c.HubDir()
	if _, err := os.Stat(hubDir); errors.Is(err, os.ErrNotExist) {
		return result, nil // Nothing to sync
	}

	// Both realms the sync path mutates through: snapshot entries live in the
	// hub-owned control tree, friendly entries and cleanup in the cache-root one.
	hubRealm, err := c.ownershipRealmForDataContext(context.Background())
	if err != nil {
		return nil, fmt.Errorf("resolve hub ownership realm: %w", err)
	}
	friendlyRealm, err := c.ownershipRealmForFriendlyContext(context.Background())
	if err != nil {
		return nil, fmt.Errorf("resolve friendly ownership realm: %w", err)
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
		created, updated, repoErrs := c.syncRepoFriendlyView(repoDir, opts, hubRealm, friendlyRealm)
		for _, e := range repoErrs {
			result.Errors = append(result.Errors, fmt.Errorf("sync %s: %w", entry.Name(), e))
		}

		result.SymlinksCreated += created
		result.SymlinksUpdated += updated
	}

	// Clean orphaned symlinks if requested
	if opts.Clean {
		removed, err := c.cleanOrphanedSymlinks(opts, friendlyRealm)
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
// then mirrors the snapshot into the friendly view. Entry creation and repair run
// through the scoped link funnels (hubRealm for snapshot entries, friendlyRealm
// for friendly entries), so they cannot mutate an entry another participant owns.
// Returns (created, updated, errors); per-entry failures are reported without
// aborting the remaining entries.
func (c *HFCache) syncRepoFriendlyView(repoDir *RepoDir, opts SyncOptions, hubRealm, friendlyRealm *ownershipRealm) (int, int, []error) {
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
		m, err := readRepoManifest(repoDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("read download manifest: %w", err))
		} else if m != nil {
			commit = m.Commit
		}
	}

	if commit == "" {
		return 0, 0, errs // Nothing to sync, but retain manifest read failures
	}

	// Recreate snapshot entries a blobs-only or partially repaired cache is
	// missing before mirroring them into the friendly view.
	repaired, repairErrs := repoDir.repairSnapshotEntries(commit, hubRealm)
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
		expectedTarget := friendlyLinkTarget(filepath.Dir(friendlyPath), snapshotPath)

		// Check if the friendly entry already exists and is correct
		correct, cerr := cacheEntryCorrect(context.Background(), friendlyPath, snapshotPath, expectedTarget)
		if cerr != nil {
			errs = append(errs, fmt.Errorf("inspect friendly entry %s: %w", relPath, cerr))
			return nil
		}
		if correct {
			return nil
		}

		_, statErr := os.Lstat(friendlyPath)
		if err := createFriendlyEntryScoped(context.Background(), repoDir, commit, relPath, "", friendlyRealm); err != nil {
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
func readRepoManifest(repoDir *RepoDir) (*DownloadManifest, error) {
	m, err := ReadManifest(filepath.Join(repoDir.FriendlyPath(), ManifestFilename))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !strings.EqualFold(m.Repo, repoDir.RepoID()) {
		return nil, nil
	}
	return m, nil
}

// repairSnapshotEntries recreates snapshot entries the manifest records for
// commit but the cache is missing (or holds as a dangling symlink). The
// manifest is the offline relative path -> blob mapping written at download
// time, so a blobs-only cache can be repaired without contacting the Hub.
// Names tombstoned as intentionally deleted (DownloadManifest.Deleted) are
// never recreated: a retained payload does not imply consent to restore a
// deleted entry. Entries whose blob is absent, whose recorded path would
// escape the snapshot directory, or whose manifest shape is unusable are
// skipped or reported; existing snapshot entries (of any link kind) are left
// untouched. Each recreated entry is placed under its ownership scope in
// hubRealm, so a live writer of that entry excludes the repair.
// Returns (created, errors).
func (r *RepoDir) repairSnapshotEntries(commit string, hubRealm *ownershipRealm) (int, []error) {
	created := 0
	var errs []error

	m, err := readRepoManifest(r)
	if err != nil {
		return 0, []error{fmt.Errorf("read download manifest: %w", err)}
	}
	if m == nil || m.Commit != commit {
		return 0, nil
	}

	for _, f := range m.Files {
		rel := filepath.ToSlash(f.Name)
		if m.IsDeleted(rel, commit) {
			continue // Intentionally removed: rebuild must not resurrect it
		}
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
		if err := createSnapshotEntryScoped(context.Background(), r, commit, rel, hash, hubRealm); err != nil {
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

// cleanOrphanedSymlinks removes symlinks in friendly view that point to non-existent files,
// each removal under its entry's ownership scope in friendlyRealm.
func (c *HFCache) cleanOrphanedSymlinks(opts SyncOptions, friendlyRealm *ownershipRealm) (int, error) {
	removed := 0

	// Clean models/
	modelsRemoved, err := cleanOrphansInDir(c.ModelsDir(), friendlyRealm)
	if err != nil {
		return removed, fmt.Errorf("clean models: %w", err)
	}
	removed += modelsRemoved

	// Clean datasets/
	datasetsRemoved, err := cleanOrphansInDir(c.DatasetsDir(), friendlyRealm)
	if err != nil {
		return removed, fmt.Errorf("clean datasets: %w", err)
	}
	removed += datasetsRemoved

	return removed, nil
}

// cleanOrphansInDir removes broken symlinks in a directory tree. A removal
// whose entry scope is held elsewhere is skipped like any other failed unlink;
// the orphan stays for a later pass instead of being forced out.
func cleanOrphansInDir(dir string, friendlyRealm *ownershipRealm) (int, error) {
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
			if err := removeCacheEntryScoped(context.Background(), friendlyRealm, path); err != nil {
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
	cleanEmptyDirs(dir, friendlyRealm)

	return removed, nil
}

// cleanEmptyDirs removes empty directories bottom up. Each removal first takes
// tree admission for the directory in friendlyRealm: a non-empty directory
// needs no admission because its unlink would fail anyway, and a tree held by
// another participant is left alone like a failed os.Remove.
func cleanEmptyDirs(dir string, friendlyRealm *ownershipRealm) {
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		if entries, readErr := os.ReadDir(path); readErr != nil || len(entries) > 0 {
			return nil
		}
		if friendlyRealm == nil {
			return nil
		}
		lease, leaseErr := friendlyRealm.acquireTreeRemoval(context.Background(), path, 0)
		if leaseErr != nil {
			return nil // Skip: another participant owns this tree
		}
		_ = os.Remove(path)
		lease.Release()
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
