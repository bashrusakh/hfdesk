// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// A cache item is reachable through its snapshot entry and, when enabled, its
// friendly entry; a blob alone is insufficient.

import (
	"context"
	"fmt"
	"os"
)

// createCacheEntryLinks creates the required cache entries and wraps failures
// with ErrCacheLinkUnreachable. noFriendly omits the optional friendly entry.
func createCacheEntryLinks(ctx context.Context, repoDir *RepoDir, commit, relativePath, filterSubdir, sha256 string, noFriendly bool) error {
	if err := repoDir.createSnapshotSymlink(ctx, commit, relativePath, sha256); err != nil {
		return fmt.Errorf("%w: the snapshot entry of %s could not be created: %w", ErrCacheLinkUnreachable, relativePath, err)
	}
	if noFriendly {
		return nil
	}
	if err := repoDir.createFriendlySymlinkCtx(ctx, commit, relativePath, filterSubdir); err != nil {
		return fmt.Errorf("%w: the friendly entry of %s could not be created: %w", ErrCacheLinkUnreachable, relativePath, err)
	}
	return nil
}

func cacheEntryLinkPaths(repoDir *RepoDir, commit, relativePath, filterSubdir string, noFriendly bool) ([]string, error) {
	snapshot, err := repoDir.SnapshotPath(commit, relativePath)
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot path: %w", err)
	}
	paths := []string{snapshot}
	if noFriendly {
		return paths, nil
	}
	base := repoDir.FriendlyPath()
	if filterSubdir != "" {
		base, err = SafeJoin(base, filterSubdir)
		if err != nil {
			return nil, fmt.Errorf("resolve friendly subdirectory: %w", err)
		}
	}
	friendly, err := SafeJoin(base, relativePath)
	if err != nil {
		return nil, fmt.Errorf("resolve friendly path: %w", err)
	}
	return append(paths, friendly), nil
}

func createCacheEntryLinksScoped(ctx context.Context, repoDir *RepoDir, commit, relativePath, filterSubdir, sha256 string, noFriendly bool, hubRealm, friendlyRealm *ownershipRealm) error {
	paths, err := cacheEntryLinkPaths(repoDir, commit, relativePath, filterSubdir, noFriendly)
	if err != nil {
		return fmt.Errorf("%w: the snapshot entry of %s could not be created: %w", ErrCacheLinkUnreachable, relativePath, err)
	}
	if hubRealm == nil {
		return fmt.Errorf("%w: the snapshot entry of %s has no ownership realm", ErrCacheLinkUnreachable, relativePath)
	}
	sets := []ownershipPathSet{{realm: hubRealm, paths: paths[:1]}}
	if !noFriendly {
		if friendlyRealm == nil {
			return fmt.Errorf("%w: the friendly entry of %s has no ownership realm", ErrCacheLinkUnreachable, relativePath)
		}
		sets = append(sets, ownershipPathSet{realm: friendlyRealm, paths: paths[1:]})
	}
	lease, err := acquireOwnershipPathSets(ctx, blobOwnershipWaitBudget, sets...)
	if err != nil {
		return fmt.Errorf("%w: the cache entries of %s could not be protected: %w", ErrCacheLinkUnreachable, relativePath, err)
	}
	defer lease.Release()
	return cacheEntryLinkCreate(ctx, repoDir, commit, relativePath, filterSubdir, sha256, noFriendly)
}

// withEntryScope holds one cache entry's ownership scope for the duration of
// create: the per-entry scoped-funnel semantics (bounded wait, then the
// actionable ownership error) for callers outside the download path.
func withEntryScope(ctx context.Context, realm *ownershipRealm, what, target string, create func() error) error {
	if realm == nil {
		return fmt.Errorf("create %s: no ownership realm", what)
	}
	lease, err := acquireOwnershipPathSets(ctx, blobOwnershipWaitBudget, ownershipPathSet{realm: realm, paths: []string{target}})
	if err != nil {
		return fmt.Errorf("create %s: hold its ownership scope: %w", what, err)
	}
	defer lease.Release()
	return create()
}

// createSnapshotEntryScoped creates one snapshot entry while holding that
// entry's scope in hubRealm; it is the scoped link funnel for the sync/repair
// path, whose direct calls would bypass admission.
func createSnapshotEntryScoped(ctx context.Context, repoDir *RepoDir, commit, relativePath, sha256 string, hubRealm *ownershipRealm) error {
	target, err := repoDir.SnapshotPath(commit, relativePath)
	if err != nil {
		return fmt.Errorf("create snapshot entry %s: %w", relativePath, err)
	}
	return withEntryScope(ctx, hubRealm, "snapshot entry "+relativePath, target, func() error {
		return repoDir.createSnapshotSymlink(ctx, commit, relativePath, sha256)
	})
}

// createFriendlyEntryScoped creates one friendly entry while holding that
// entry's scope in friendlyRealm; it is the scoped link funnel for the
// sync/rebuild path.
func createFriendlyEntryScoped(ctx context.Context, repoDir *RepoDir, commit, relativePath, filterSubdir string, friendlyRealm *ownershipRealm) error {
	paths, err := cacheEntryLinkPaths(repoDir, commit, relativePath, filterSubdir, false)
	if err != nil {
		return fmt.Errorf("create friendly entry %s: %w", relativePath, err)
	}
	target := paths[len(paths)-1]
	return withEntryScope(ctx, friendlyRealm, "friendly entry "+relativePath, target, func() error {
		return repoDir.createFriendlySymlinkCtx(ctx, commit, relativePath, filterSubdir)
	})
}

// removeCacheEntryScoped removes one single entry while holding that entry's
// scope in realm: the scoped funnel for orphan cleanup, which would otherwise
// unlink across participants.
func removeCacheEntryScoped(ctx context.Context, realm *ownershipRealm, path string) error {
	if realm == nil {
		return fmt.Errorf("remove cache entry %q: no ownership realm", path)
	}
	return withExternalWriter(ctx, realm, path, func() error { return os.Remove(path) })
}

// cacheEntryLinkCreate is replaceable so tests can inject link failures.
var cacheEntryLinkCreate = createCacheEntryLinks
