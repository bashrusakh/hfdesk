// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// The cache entry link contract (issue #140 / PR #151).
//
// A cache item is reachable through TWO entries: the snapshot entry
// (hub/<repo>/snapshots/<commit>/<path>) and the friendly entry
// (models/<owner>/<repo>/<path>, or datasets/... for a dataset). The payload
// in blobs/ is only the content; the entries are what a reader actually
// opens.
//
// The transfer path has always treated a failed link creation as fatal for
// the item (the cache store returns the error). The skip paths did not: they
// ignored the error and reported the item as a successful skip, so a job
// could report success while the requested path was not reachable at all.
//
// The two paths now agree: a link-creation failure fails the item with an
// actionable error, and the end-of-job reconciliation of skipped items
// reports its link failures as a job error instead of only a warning.
//
// The friendly entry is deliberately ABSENT when the friendly view is
// disabled (Settings.NoFriendlyView), so that is not an error: the requested
// path is the snapshot entry, and the friendly view is not part of the
// request in that mode.

import (
	"context"
	"fmt"
)

// createCacheEntryLinks creates the snapshot entry of one item and, unless
// the friendly view is disabled, its friendly entry.
//
// Every failure is returned as an error that wraps ErrCacheLinkUnreachable:
// the payload may be present, but the requested path is not reachable
// through it, and the caller must not report the item as a successful skip.
// The disabled friendly view is not a failure: the friendly entry is
// deliberately absent in that mode.
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

// cacheEntryLinkCreate is the seam of the cache entry link creation. It is a
// package variable so a regression test can install the historical behavior
// (a swallowed link failure) and prove that the link regressions actually
// detect it.
var cacheEntryLinkCreate = createCacheEntryLinks
