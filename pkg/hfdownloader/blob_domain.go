// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Blob destinations use one ownership domain across ranged and single-request
// routes. The item holds it from before HEAD through publication and cleanup;
// the ranged claim reuses that fence. Digest-less items use the reserved
// tmp-path- name so their domain cannot collide with a content-addressed key.
// Lock files are persistent: unlinking one could let another process lock a
// new inode while the current owner still holds the old one. See doc.go for the
// complete ownership and waiter contract.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// blobOwnershipWaitBudget bounds live-owner waits. It is an internal protocol
// setting; cleanup uses its shorter best-effort budget instead.
var blobOwnershipWaitBudget = 60 * stagingOwnershipAcquireWait

// errBlobOwnedByLiveWriter identifies a bounded wait that expired while a
// proven live owner still held the key.
var errBlobOwnedByLiveWriter = errors.New("owned by another live writer")

func blobLiveOwnerError(sha, rel string) error {
	if sha == "" {
		return fmt.Errorf("%s is being downloaded by another process; try again later: %w", rel, errBlobOwnedByLiveWriter)
	}
	return fmt.Errorf("blob %s is being downloaded by another process; try again later: %w", sha, errBlobOwnedByLiveWriter)
}

// stagingUnprovenOwnerError reports a record whose owner cannot be proven live
// or dead. The partial is preserved; the error identifies the staging pair for
// manual recovery without exposing unrelated paths or credentials.
type stagingUnprovenOwnerError struct {
	sha     string
	rel     string
	staging string
	meta    string
}

func newStagingUnprovenOwnerError(it PlanItem, staging, metaPath string) error {
	return &stagingUnprovenOwnerError{
		sha:     it.SHA256,
		rel:     it.RelativePath,
		staging: staging,
		meta:    metaPath,
	}
}

func (e *stagingUnprovenOwnerError) Error() string {
	what := e.rel
	if e.sha != "" {
		what = "blob " + e.sha
	}
	return fmt.Sprintf("the staging state for %s cannot be proven to belong to a live or dead owner (legacy or unreadable record); the partial is preserved - remove %s and %s to retry",
		what, e.staging, e.meta)
}

func (e *stagingUnprovenOwnerError) Unwrap() error { return ErrStagingOwnershipUnproven }

// Is preserves the shared errBlobIncompleteOwned classification.
func (e *stagingUnprovenOwnerError) Is(target error) bool {
	return target == errBlobIncompleteOwned
}

type blobDomainEntry struct {
	done chan struct{}
}

var blobDomainOwners = struct {
	sync.Mutex
	m map[string]*blobDomainEntry
}{m: map[string]*blobDomainEntry{}}

type blobDomain struct {
	// key is the cache blob path or the namespaced flat-destination key.
	key string
	// dest is the destination whose multipart staging names this domain
	// covers: blobs/tmp-<name> for a cache destination (which covers
	// blobs/tmp-<name>.incomplete and blobs/<name>.incomplete), or the
	// <destination>.incomplete staging name for a flat one.
	dest     string
	lock     *stagingOwnershipLock
	entry    *blobDomainEntry
	released bool
}

// acquireBlobDomain waits for an in-process holder and re-evaluates the key.
// Proven foreign owners are waited out within the budget; unresolved lock state
// returns an error, never a successful skip.
func acquireBlobDomain(ctx context.Context, key, dst string) (*blobDomain, error) {
	return acquireBlobDomainWithin(ctx, key, dst, 0, blobOwnershipWaitBudget, true)
}

// acquireBlobDomainForCleanup uses a short best-effort wait and never waits out
// a live owner.
func acquireBlobDomainForCleanup(ctx context.Context, key, dst string) (*blobDomain, error) {
	return acquireBlobDomainWithin(ctx, key, dst, stagingLockCleanupWait, stagingLockCleanupWait, false)
}

var blobDomainCleanupAcquire = acquireBlobDomainForCleanup

// acquireBlobDomainWithin applies separate in-process and OS-lock budgets.
// Download waits for in-process holders until context cancellation; cleanup
// bounds that wait and reports live owners without waiting. With a slot notice,
// it returns the transfer slot during waits and reacquires it only on success.
func acquireBlobDomainWithin(ctx context.Context, key, dst string, slotWait, lockWait time.Duration, tolerateLiveOwner bool) (domain *blobDomain, err error) {
	notice := downloadSlotWaitNoticeFromContext(ctx)
	waiting := false
	beginWait := func() {
		if notice == nil || waiting {
			return
		}
		waiting = true
		notice.beginWait()
	}
	defer func() {
		if !waiting {
			return
		}
		if domain == nil || err != nil {
			// No domain reaches the caller, so keep the slot returned while
			// reporting the failure.
			if domain != nil {
				domain.release()
				domain = nil
			}
			return
		}
		if !notice.endWait() {
			// Do not return a domain after cancellation; that would transfer
			// without a slot.
			domain.release()
			domain = nil
			if err == nil {
				err = ctx.Err()
			}
		}
	}()

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
		beginWait()
		// A nil timeout disables only the timeout case; the context still bounds the wait.
		var timeout <-chan time.Time
		var timer *time.Timer
		if slotWait > 0 {
			timer = time.NewTimer(slotWait)
			timeout = timer.C
		}
		select {
		case <-held.done:
			if timer != nil {
				timer.Stop()
			}
			continue
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil, ctx.Err()
		case <-timeout:
			// Leave the holder and its artifacts untouched on timeout.
			return nil, errStagingOwnershipBusy
		}
	}

	var lock *stagingOwnershipLock
	if tolerateLiveOwner {
		lock, err = acquireStagingOwnershipLockWaiting(ctx, dst, lockWait, beginWait)
	} else {
		lock, err = acquireStagingOwnershipLockWithin(ctx, dst, lockWait)
	}
	if err != nil {
		blobDomainOwners.Lock()
		if blobDomainOwners.m[key] == entry {
			delete(blobDomainOwners.m, key)
		}
		blobDomainOwners.Unlock()
		close(entry.done)
		return nil, err
	}
	return &blobDomain{key: key, dest: dst, lock: lock, entry: entry}, nil
}

var blobDomainAcquire = acquireBlobDomain

// blobDomainContentionHook lets tests observe in-process contention.
var blobDomainContentionHook func(key string)

// blobDomainDst returns the SafeJoin-confined staging destination for a blob key.
func (r *RepoDir) blobDomainDst(sha string) (string, error) {
	return SafeJoin(r.BlobsDir(), "tmp-"+sha)
}

// cacheDstDomainKey maps blobs/tmp-<name> to key blobs/<name>. Digest-less
// destinations use the reserved tmp-path- prefix so their domains cannot
// collide with canonical blob keys. Other paths are not cache destinations.
func cacheDstDomainKey(dst string) (string, bool) {
	dir := filepath.Dir(dst)
	if filepath.Base(dir) != "blobs" {
		return "", false
	}
	name, found := strings.CutPrefix(filepath.Base(dst), "tmp-")
	if !found || name == "" {
		return "", false
	}
	key, err := SafeJoin(dir, name)
	if err != nil {
		return "", false
	}
	return key, true
}

// flatDomainKeyPrefix separates flat destinations from cache blob keys.
const flatDomainKeyPrefix = "flat:"

func flatDomainKey(dst string) string {
	return flatDomainKeyPrefix + filepath.Clean(dst)
}

// flatDomainLockSrc uses the multipart staging name so all routes share the
// persistent <destination>.incomplete.lock.
func flatDomainLockSrc(dst string) string {
	return dst + ".incomplete"
}

type flatDestinationDomain struct {
	key string
	// lockSrc is the name whose persistent .lock file fences the domain.
	lockSrc string
}

func newFlatDestinationDomain(dst string) *flatDestinationDomain {
	return &flatDestinationDomain{key: flatDomainKey(dst), lockSrc: flatDomainLockSrc(dst)}
}

// holdsBlobKey checks the domain-exclusive publication precondition.
func (d *blobDomain) holdsBlobKey(key string) bool {
	if d == nil || d.released || d.key == "" || key == "" {
		return false
	}
	return filepath.Clean(d.key) == filepath.Clean(key)
}

// coversStaging is the claim's fail-closed ownership check for cache and flat
// staging paths.
func (d *blobDomain) coversStaging(staging string) bool {
	if d == nil || d.released || d.dest == "" || staging == "" {
		return false
	}
	clean := filepath.Clean(staging)
	if clean == filepath.Clean(d.dest) {
		return true
	}
	if strings.HasPrefix(d.key, flatDomainKeyPrefix) {
		return false
	}
	return clean == filepath.Clean(d.dest+".incomplete") ||
		clean == filepath.Clean(d.key+".incomplete")
}

func (d *blobDomain) lockHeld() bool {
	return d != nil && !d.released && d.lock != nil && d.lock.held()
}

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
