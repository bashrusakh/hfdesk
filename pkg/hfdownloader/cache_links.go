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

// cacheEntryLinkCreate is replaceable so tests can inject link failures.
var cacheEntryLinkCreate = createCacheEntryLinks
