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
//
// A writer that finds the key owned by a PROVEN live owner WAITS for it
// (bounded, context-aware) and re-checks the published content-addressed
// target afterwards, instead of skipping the item: the file is not present
// while the owner holds the key, so a skip would report a success the cache
// does not have. When the wait budget expires with the owner still there, the
// item fails with an actionable error. The same protocol governs the CLEANUP
// of a blob key's artifacts: they are removed only while the key's ownership
// domain is held, so a cancelled job never deletes another live writer's
// partial.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// blobOwnershipWaitBudget bounds how long a writer waits for a PROVEN live
// owner of a blob key to release it.
//
// The budget reuses the existing ownership-lock convention: ONE acquisition
// attempt is bounded by stagingOwnershipAcquireWait (5s - long enough for a
// status probe to finish, short enough not to stall a download), while a live
// owner is transferring a file and may hold the key for minutes. The budget is
// therefore a multiple of that window, and it is FINITE: a suspended owner
// keeps the lock for as long as its process lives, so the wait has to end in
// an actionable error instead of blocking the job forever.
//
// It is a package-level knob, not a user setting: the wait is an internal
// detail of the ownership protocol, so the Settings API stays unchanged. The
// cleanup path uses the shorter, existing cleanup convention
// (stagingLockCleanupWait) instead, because a cleanup is best-effort and must
// not delay an explicit cancel.
var blobOwnershipWaitBudget = 60 * stagingOwnershipAcquireWait

// errBlobOwnedByLiveWriter reports that a PROVEN live writer still owns a
// blob key (or its staging file) after the bounded wait for it expired.
//
// The file is NOT present in that state, so the item fails with this
// actionable error instead of a successful skip: a job that reports success
// must mean the requested files are present. Distinguishable via errors.Is.
var errBlobOwnedByLiveWriter = errors.New("owned by another live writer")

// blobLiveOwnerError is the actionable error of an item whose file is owned
// by another live writer.
//
// The message names the blob key when the item has a known digest, and the
// item's path otherwise (a legacy/output destination has no blob key). It
// always names the retry action, because the condition is transient: the
// owner may publish the file at any moment.
func blobLiveOwnerError(sha, rel string) error {
	if sha == "" {
		return fmt.Errorf("%s is being downloaded by another process; try again later: %w", rel, errBlobOwnedByLiveWriter)
	}
	return fmt.Errorf("blob %s is being downloaded by another process; try again later: %w", sha, errBlobOwnedByLiveWriter)
}

// stagingUnprovenOwnerError is the actionable error of an item whose staging
// record cannot be proven to belong to a live owner or to a dead one.
//
// The condition is NOT "another process is downloading this": the record is
// either a legacy/unfenced one (written without the lifetime ownership lock
// fence) or one that cannot be read or parsed at all, so its owner can be
// proven neither alive nor dead. The refusal is deliberate and conservative:
// the record is preserved and never taken over, because no age-based takeover
// and no takeover without proof of same-host death is allowed.
//
// The message therefore says what the state is AND what the operator can do:
// the staging pair has to be removed by hand, because the automatic recovery
// is deliberately absent. It names the blob key (or the item's path when the
// item has no known digest) and the staging pair - and nothing else: no
// credential, and no other absolute path.
type stagingUnprovenOwnerError struct {
	sha     string
	rel     string
	staging string
	meta    string
}

// newStagingUnprovenOwnerError builds the operator-visible error of an
// unproven staging record.
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

// Unwrap exposes the exported sentinel, so a caller can tell an unproven
// record apart from a proven live owner.
func (e *stagingUnprovenOwnerError) Unwrap() error { return ErrStagingOwnershipUnproven }

// Is keeps the "the staging file is not available to this writer"
// classification of errBlobIncompleteOwned: in both the proven-live-owner and
// the unproven case the staging file cannot be used by this writer. The two
// are still told apart by ErrStagingOwnershipUnproven, which only the
// unproven case carries.
func (e *stagingUnprovenOwnerError) Is(target error) bool {
	return target == errBlobIncompleteOwned
}

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
// A PROVEN live owner in another process is WAITED FOR, not reported as a
// skip: the file is not present while the owner holds the key, so a skip would
// report a success the cache does not have. The wait is bounded
// (blobOwnershipWaitBudget) and context-aware, and it is never a takeover: a
// live owner keeps its lock. When the budget expires with the owner still
// there, the acquisition reports errStagingOwnershipBusy, which the caller
// turns into an actionable per-item error.
//
// An unresolvable contention is an actionable error too
// (errStagingOwnershipUncertain or a lock error). Neither outcome is ever
// reported as a successful skip of the key.
func acquireBlobDomain(ctx context.Context, key, dst string) (*blobDomain, error) {
	// The in-process slot wait is bounded by the caller's context only: its
	// holder is a goroutine of this process that finishes or is cancelled,
	// and the slot already implements the wait-then-re-evaluate shape the
	// protocol needs. The cross-process wait is bounded by the budget.
	return acquireBlobDomainWithin(ctx, key, dst, 0, blobOwnershipWaitBudget, true)
}

// acquireBlobDomainForCleanup takes the ownership domain of one blob key for a
// cleanup operation.
//
// The cleanup uses the existing cleanup convention (a short, best-effort
// window) instead of the download budget: a cleanup must not delay an
// explicit cancel, and it must never displace or wait out a live owner. A
// live owner - in this process or in another one - means the key's artifacts
// belong to somebody else, so the caller leaves them alone.
func acquireBlobDomainForCleanup(ctx context.Context, key, dst string) (*blobDomain, error) {
	return acquireBlobDomainWithin(ctx, key, dst, stagingLockCleanupWait, stagingLockCleanupWait, false)
}

// blobDomainCleanupAcquire is the domain acquisition seam of the cleanup
// path. It is a package variable so a regression test can install the
// historical behavior (cleanup WITHOUT the blob key's ownership domain) and
// prove that the cleanup regressions actually detect it.
var blobDomainCleanupAcquire = acquireBlobDomainForCleanup

// acquireBlobDomainWithin is acquireBlobDomain with explicit wait budgets.
//
// The two waits are deliberately different:
//
//   - the IN-PROCESS slot wait is bounded by slotWait (zero means: wait until
//     the caller's context is done). Its holder is a goroutine of this process
//     that finishes or is cancelled, and the domain slot already implements
//     the wait-then-re-evaluate shape the protocol needs: the holder publishes
//     the blob and releases, and the waiter re-evaluates the key from scratch.
//     A budget here would fail a legitimate sibling download of a large file,
//     so the download path leaves it unbounded and the cleanup path bounds it
//     (a cleanup must not block on a live writer);
//   - the CROSS-PROCESS OS lock wait is bounded by lockWait, because the
//     holder is another process whose progress this one cannot observe. When
//     the budget expires with the owner still there, the acquisition reports
//     errStagingOwnershipBusy, which the caller turns into an actionable
//     per-item error instead of a successful skip.
//
// tolerateLiveOwner selects what a PROVEN live owner in another process means:
// with it true the owner is waited out (the download path: the file is not
// present while it holds the key), with it false the contention is reported
// at once (the cleanup path: the artifacts belong to the live owner).
//
// When the context carries an item's download-slot notice (see
// download_slot.go), the acquisition returns that item's MaxActiveDownloads
// slot for the DURATION of every wait. It takes the slot back before it
// returns a domain the caller will transfer under. A FAILED acquisition keeps
// the slot returned: no domain reaches the caller, so the item reports the
// error instead of transferring, and re-taking the slot there would make the
// failure wait for an unrelated transfer. A waiter is not transferring, so it
// must not hold a transfer slot; a returned domain still reaches the caller
// only with a slot held.
func acquireBlobDomainWithin(ctx context.Context, key, dst string, slotWait, lockWait time.Duration, tolerateLiveOwner bool) (domain *blobDomain, err error) {
	// The wait is signalled by THIS function, not guessed by the caller: it is
	// the only place that knows both waits (the in-process domain slot and the
	// cross-process lifetime lock). The notice releases the slot once, before
	// the first blocking wait. The deferred endWait takes it back only when a
	// domain is returned for transfer; a failed acquisition returns no domain,
	// and the lease's deferred release is a no-op while the slot is still
	// returned, so the lease stays balanced on both paths.
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
			// The acquisition FAILED, so no domain reaches the caller and the
			// item is about to report the error. There is nothing to transfer,
			// so the slot must NOT be taken back here: a wait-budget expiry
			// would otherwise block until some other (possibly long) transfer
			// released a slot, delaying the actionable failure the caller is
			// about to report. The lease's deferred release is a no-op while
			// the slot is still returned.
			if domain != nil {
				domain.release()
				domain = nil
			}
			return
		}
		if !notice.endWait() {
			// The item's context ended while it waited, so the slot cannot be
			// taken back. A domain acquired in that instant must not reach the
			// caller: the item would transfer without a slot. Release it and
			// report the cancellation instead.
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
		// The in-process slot is busy, so this acquisition is about to wait
		// for its holder: return the item's download slot for the wait.
		beginWait()
		// A nil timeout channel blocks forever, which is the unbounded
		// (context-only) wait of the download path.
		var timeout <-chan time.Time
		var timer *time.Timer
		if slotWait > 0 {
			timer = time.NewTimer(slotWait)
			timeout = timer.C
		}
		select {
		case <-held.done:
			// The holder finished: re-evaluate the domain from scratch.
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
			// The in-process holder of this blob key did not release it
			// within the budget. The holder is not displaced: the caller
			// reports the contention so it can leave the key's artifacts
			// alone.
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

// cacheDstDomainKey derives the ownership-domain key of a cache destination.
//
// The domain is keyed on the DESTINATION, not on the blob key the item may
// end up in:
//
//   - a cache destination is the staging name the downloader allocates for an
//     item: blobs/tmp-<name>;
//   - its domain key is blobs/<name>;
//   - its domain lock is the persistent sibling blobs/tmp-<name>.lock.
//
// For a content-addressed item the name is the SHA-256, so the derivation
// yields exactly the blob key's domain of today: key blobs/<sha>, lock
// blobs/tmp-<sha>.lock. The publication's holdsBlobKey check therefore keeps
// working unchanged, and the cleanup of a blob key's artifacts keeps taking
// the same domain the writer holds.
//
// For a path-derived destination (an item without a known digest) the same
// derivation gives the destination its own domain, so a writer of that
// destination and the cleanup of that destination agree on ONE identity
// instead of protecting a domain nobody holds.
//
// A destination outside a blobs directory, or one that is not a tmp-<name>
// staging name, has no domain key: it is not a cache destination and keeps
// its historical behavior.
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

// flatDomainKeyPrefix namespaces the flat/output-mode domain keys in the
// shared in-process registry, so a flat destination can never collide with a
// cache-mode blob-key domain (the two namespaces are disjoint by construction,
// and the prefix makes that explicit rather than incidental).
const flatDomainKeyPrefix = "flat:"

// flatDomainKey derives the ownership-domain key of a flat/output-mode
// destination.
//
// The flat namespace has no blobs/-style internal identity, so the key is the
// destination path itself. The LIFETIME LOCK of that domain is not derived
// from the key but from the destination's MULTIPART staging name (see
// flatDomainLockSrc): both routes and the cleanup must resolve the SAME lock
// file, and that file is the one the multipart route's staging claim already
// takes.
func flatDomainKey(dst string) string {
	return flatDomainKeyPrefix + filepath.Clean(dst)
}

// flatDomainLockSrc is the staging name whose persistent ".lock" sibling is
// the lifetime OS ownership lock of a flat destination's domain.
//
// It is the MULTIPART staging name (<destination>.incomplete), because the
// multipart route's staging claim already takes exactly that lock. Reusing it
// is what excludes the single-request route from the multipart route - and
// the cleanup from both - without a second artifact next to the user's model
// file: the lock file is <destination>.incomplete.lock, the zero-byte
// artifact the multipart route already leaves there and never unlinks.
func flatDomainLockSrc(dst string) string {
	return dst + ".incomplete"
}

// flatDestinationDomain names the ownership domain of one flat/output-mode
// destination.
//
// It is the identity the single-request route holds while it transfers, the
// identity the cleanup holds while it removes the destination's artifacts,
// and the identity the multipart route hands to the single-request path when
// it falls back to it (the fall-back happens BEFORE the multipart route
// claims the staging, so the domain is still free there).
type flatDestinationDomain struct {
	// key is the domain identity: the in-process registry key.
	key string
	// lockSrc is the staging name whose persistent ".lock" sibling is the
	// domain's lifetime OS ownership lock.
	lockSrc string
}

// newFlatDestinationDomain derives the ownership domain of one flat/output
// mode destination.
func newFlatDestinationDomain(dst string) *flatDestinationDomain {
	return &flatDestinationDomain{key: flatDomainKey(dst), lockSrc: flatDomainLockSrc(dst)}
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

// holdsStagingLock reports whether this holder owns the lifetime ownership
// lock of the staging pair named by staging.
//
// It is the check the flat/output-mode paths need: there the destination's
// domain lock and the staging pair's lock are the SAME file
// (<destination>.incomplete.lock), so a removal that took that lock again
// would report the caller's OWN hold as a live owner and skip the removal.
//
// In HF cache mode the two are different files (blobs/tmp-<name>.lock vs
// blobs/tmp-<name>.incomplete.lock), so the answer is false there and the
// ordinary claim-checked removal applies.
func (d *blobDomain) holdsStagingLock(staging string) bool {
	if d == nil || d.released || d.lock == nil || staging == "" {
		return false
	}
	return d.lock.path == stagingOwnershipLockPath(staging)
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
