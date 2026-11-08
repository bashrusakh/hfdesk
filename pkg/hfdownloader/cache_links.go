// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// A cache item is reachable through its snapshot entry and, when enabled, its
// friendly entry; a blob alone is insufficient.

import (
	"context"
	"fmt"
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

// cacheEntryLinkCreate is replaceable so tests can inject link failures.
var cacheEntryLinkCreate = createCacheEntryLinks
