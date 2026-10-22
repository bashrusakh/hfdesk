// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// The blob-key ownership domain (issue #140 / PR #151).
//
// A content-addressed blob key owns exactly ONE ownership domain. The domain
// is keyed on the blob key, never on the staging path one particular route
// happens to use, because two different routes write the same blob key:
//
//   - the ranged route stages in blobs/<sha>.incomplete and publishes with a
//     rename onto blobs/<sha>;
//   - the single-request route stages in blobs/tmp-<sha>.part and publishes
//     through the cache store onto blobs/<sha>.
//
// Keying the domain on the staging path gave every route its own private
// domain, so a "the blob does not exist yet" decision in one route could be
// overtaken by the other route's placement, and one route could adopt the
// other route's LIVE partial as if it were a pre-#140 leftover.
//
// The domain has two layers:
//
//   - an in-process slot, so two goroutines of one process never own the same
//     blob key at the same time (the single-request route's partial
//     blobs/tmp-<sha>.part is shared by every writer of that key);
//   - the lifetime OS ownership lock on the persistent sibling
//     blobs/tmp-<sha>.lock, so a writer in another process cannot own the
//     same blob key either.
//
// The lock file is the sibling of the item's cache destination, so it needs no
// new artifact name: every ".lock" entry is already persistent, never
// unlinked, and excluded from cache statistics, listings, mirrors and cache
// deletion.
//
// A writer holds the domain for its whole operation on the key: the partial
// namespace (so no other route can adopt, migrate or delete a live partial of
// the same key) and the publication (so the existence decision and the
// placement that follows it cannot be overtaken).

import (
	"context"
	"path/filepath"
	"sync"
)

// blobDomainEntry is one in-process holder of a blob key's domain. Its done
// channel is closed when the holder releases the domain.
type blobDomainEntry struct {
	done chan struct{}
}

// blobDomainOwners is the in-process exclusion registry keyed by blob path.
var blobDomainOwners = struct {
	sync.Mutex
	m map[string]*blobDomainEntry
}{m: map[string]*blobDomainEntry{}}

// blobDomain is a held ownership domain of one blob key.
type blobDomain struct {
	// key is the domain identity: the content-addressed blob path.
	key string
	// lock is the held lifetime OS ownership lock.
	lock *stagingOwnershipLock
	// entry is the in-process slot this holder owns.
	entry *blobDomainEntry
	// released guards the one-shot release.
	released bool
}

// acquireBlobDomain takes the ownership domain of one blob key.
//
// The in-process slot is taken first: a second writer of the same key in this
// process waits for the current holder to finish and then re-evaluates (the
// holder may have published the blob in the meantime). The lifetime OS lock
// then excludes writers in other processes.
//
// A busy OS lock is a PROVEN live owner (errStagingOwnershipBusy) and an
// unresolvable contention is an actionable error (errStagingOwnershipUncertain
// or a lock error). Neither is ever reported as a successful skip of the key.
func acquireBlobDomain(ctx context.Context, key, dst string) (*blobDomain, error) {
	var entry *blobDomainEntry
	for {
		blobDomainOwners.Lock()
		held, busy := blobDomainOwners.m[key]
		if !busy {
			held = &blobDomainEntry{done: make(chan struct{})}
			blobDomainOwners.m[key] = held
		}
		blobDomainOwners.Unlock()
		if !busy {
			entry = held
			break
		}
		if blobDomainContentionHook != nil {
			blobDomainContentionHook(key)
		}
		select {
		case <-held.done:
			// The holder finished: re-evaluate the domain from scratch.
			continue
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	lock, err := acquireStagingOwnershipLockCtx(ctx, dst)
	if err != nil {
		blobDomainOwners.Lock()
		if blobDomainOwners.m[key] == entry {
			delete(blobDomainOwners.m, key)
		}
		blobDomainOwners.Unlock()
		close(entry.done)
		return nil, err
	}
	return &blobDomain{key: key, lock: lock, entry: entry}, nil
}

// blobDomainAcquire is the domain acquisition seam. It is a package variable
// so a regression test can install the historical behavior (no shared domain
// at all) and prove that the publication regressions actually detect it.
var blobDomainAcquire = acquireBlobDomain

// blobDomainContentionHook is called with the blob key whenever a domain
// acquisition finds the key busy in this process. It is a test seam: it lets a
// regression prove that a second writer really waited for the domain instead
// of inferring it from a sleep.
var blobDomainContentionHook func(key string)

// blobDomainDst returns the cache destination whose persistent sibling
// ".lock" file is the ownership domain lock of one blob key.
//
// It is the destination the downloader stages under (blobs/tmp-<sha>), so the
// ranged route, the single-request route and the cache store all resolve the
// SAME domain for the same key. The name is confined to the blobs directory
// with SafeJoin, exactly like the downloader's own destination.
func (r *RepoDir) blobDomainDst(sha string) (string, error) {
	return SafeJoin(r.BlobsDir(), "tmp-"+sha)
}

// holdsBlobKey reports whether this holder owns the ownership domain of the
// given content-addressed blob path.
//
// The domain-exclusive publication tier needs exactly this answer: the
// existence check and the placement that follows it are only exclusive to
// the domain while the domain of THAT blob key is held, so the placement
// verifies the domain instead of trusting the caller to have taken it. A
// released or foreign domain does not hold the key.
func (d *blobDomain) holdsBlobKey(key string) bool {
	if d == nil || d.released || d.key == "" || key == "" {
		return false
	}
	return filepath.Clean(d.key) == filepath.Clean(key)
}

// release drops the domain: the OS lock is closed and the in-process slot is
// freed, which wakes a writer waiting for this key.
func (d *blobDomain) release() {
	if d == nil || d.released {
		return
	}
	d.released = true
	if d.lock != nil {
		d.lock.release()
	}
	if d.entry == nil {
		return
	}
	blobDomainOwners.Lock()
	if blobDomainOwners.m[d.key] == d.entry {
		delete(blobDomainOwners.m, d.key)
	}
	blobDomainOwners.Unlock()
	close(d.entry.done)
}
