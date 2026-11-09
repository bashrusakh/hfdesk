// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// See doc.go for the full protocol contract: held-scope reuse by ranged claims and publication, and the reserved tmp-path- identity for digest-less items.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// These errors come from the realm-scope acquisition in acquireBlobDomainScoped;
// the wait budget it feeds is stagingOwnershipAcquireWait.
var (
	// errStagingOwnershipBusy means a current external probe confirmed an
	// exclusive holder for the requested ownership scope.
	errStagingOwnershipBusy = errors.New("external ownership scope is exclusively held")

	// errStagingOwnershipUncertain means a failed acquisition could not establish
	// whether a live writer owns the lock; callers must fail closed.
	errStagingOwnershipUncertain = errors.New("staging ownership lock could not be acquired: the holder is not a proven live writer")

	// errStagingOwnershipSameProcess reports a wait expired against a shared
	// key this process provably holds: in-process, never a foreign owner.
	errStagingOwnershipSameProcess = errors.New("ownership scope is contended within this process")

	// stagingOwnershipAcquireWait is the base unit for the destination wait budget.
	stagingOwnershipAcquireWait = 5 * time.Second
)

// blobOwnershipWaitBudget bounds external ownership-scope waits; cleanup uses
// its shorter best-effort budget instead.
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

func blobOwnershipScopeError(sha, rel string, cause error) error {
	name := rel
	if sha != "" {
		name = "blob " + sha
	}
	if errors.Is(cause, errStagingOwnershipSameProcess) {
		return fmt.Errorf("ownership scope for %s is contended by a concurrent operation in this process; retry later: %w", name, cause)
	}
	if errors.Is(cause, errStagingOwnershipBusy) {
		return fmt.Errorf("ownership scope for %s remains exclusively held; try again later: %w", name, cause)
	}
	return fmt.Errorf("ownership scope for %s could not be proven free; retry later: %w", name, cause)
}

// classifyOwnershipConflict maps a refused probe's conflict to its holder
// class; doc.go carries the contract.
func classifyOwnershipConflict(conflict *ownershipConflictError, ownScope map[string]bool) error {
	if conflict == nil {
		return errStagingOwnershipUncertain
	}
	if ownScope[conflict.key] {
		return errStagingOwnershipBusy
	}
	if ownershipHeldExclusivelyHere(conflict.path) {
		return errStagingOwnershipSameProcess
	}
	return errStagingOwnershipBusy
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
	entry    *blobDomainEntry
	realm    *ownershipRealm
	scope    *ownershipLease
	proof    string
	released bool
}

type blobDomainFence struct {
	realm *ownershipRealm
	scope *ownershipLease
	proof string
}

func acquireDefaultScopedBlobDomain(ctx context.Context, key, dst string) (*blobDomain, error) {
	ownerDir, err := resolveOwnershipDir("")
	if err != nil {
		return nil, err
	}
	realm, err := newOwnershipRealm(ownerDir)
	if err != nil {
		return nil, err
	}
	proofPath := key
	paths := []string{key, dst}
	if strings.HasPrefix(key, flatDomainKeyPrefix) {
		proofPath = strings.TrimPrefix(key, flatDomainKeyPrefix)
		paths = appendUniqueOwnershipPaths(paths, proofPath, proofPath+".part", proofPath+".incomplete", proofPath+".incomplete.meta")
	} else if strings.HasPrefix(filepath.Base(key), "path-") {
		proofPath = dst
		paths = appendUniqueOwnershipPaths(paths, dst+".part", dst+".incomplete", dst+".incomplete.meta")
	} else {
		paths = appendUniqueOwnershipPaths(paths, dst+".part", dst+".incomplete", dst+".incomplete.meta", key+".incomplete", key+".incomplete.meta")
	}
	return acquireBlobDomainScoped(ctx, key, dst, realm, paths, proofPath)
}

func acquireDefaultScopedBlobDomainForCleanup(ctx context.Context, key, dst string) (*blobDomain, error) {
	ownerDir, err := resolveOwnershipDir("")
	if err != nil {
		return nil, err
	}
	realm, err := newOwnershipRealm(ownerDir)
	if err != nil {
		return nil, err
	}
	proofPath := key
	paths := []string{key, dst}
	if strings.HasPrefix(key, flatDomainKeyPrefix) {
		proofPath = strings.TrimPrefix(key, flatDomainKeyPrefix)
	}
	return acquireBlobDomainScopedForCleanup(ctx, key, dst, realm, paths, proofPath)
}

var blobDomainCleanupAcquire = acquireDefaultScopedBlobDomainForCleanup

func acquireBlobDomainScoped(ctx context.Context, key, dst string, realm *ownershipRealm, paths []string, proofPath string) (*blobDomain, error) {
	return acquireBlobDomainScopedWithin(ctx, key, dst, realm, paths, proofPath, 0, blobOwnershipWaitBudget)
}

func acquireBlobDomainScopedForCleanup(ctx context.Context, key, dst string, realm *ownershipRealm, paths []string, proofPath string) (*blobDomain, error) {
	return acquireBlobDomainScopedWithin(ctx, key, dst, realm, paths, proofPath, stagingLockCleanupWait, stagingLockCleanupWait)
}

func acquireBlobDomainScopedWithin(ctx context.Context, key, dst string, realm *ownershipRealm, paths []string, proofPath string, slotWait, lockWait time.Duration) (*blobDomain, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if realm == nil {
		return nil, fmt.Errorf("external ownership realm is unavailable")
	}
	return acquireBlobDomainUsing(ctx, key, dst, slotWait, func(beginWait func()) (blobDomainFence, error) {
		scope, err := realm.acquireWriterWithWait(ctx, paths, lockWait, beginWait)
		if err != nil {
			if errors.Is(err, errOwnershipBusy) {
				classified, probeErr := realm.probeWriterClassified(ctx, paths)
				if probeErr != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return blobDomainFence{}, ctxErr
					}
					return blobDomainFence{}, fmt.Errorf("%w: observe external ownership scope: %v", errStagingOwnershipUncertain, probeErr)
				}
				if classified != nil {
					return blobDomainFence{}, classified
				}
				return blobDomainFence{}, errStagingOwnershipUncertain
			}
			return blobDomainFence{}, err
		}
		if !scope.holdsCoverage(realm, proofPath) {
			scope.Release()
			return blobDomainFence{}, fmt.Errorf("external ownership lease does not cover its proof key")
		}
		proof, err := realm.fenceProofForHeldPath(proofPath)
		if err != nil {
			scope.Release()
			return blobDomainFence{}, err
		}
		return blobDomainFence{realm: realm, scope: scope, proof: proof}, nil
	})
}

func acquireBlobDomainUsing(ctx context.Context, key, dst string, slotWait time.Duration, acquire func(beginWait func()) (blobDomainFence, error)) (domain *blobDomain, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	notice := downloadSlotWaitNoticeFromContext(ctx)
	waiting := false
	beginWait := func() {
		if waiting {
			return
		}
		waiting = true
		if blobDomainContentionHook != nil {
			blobDomainContentionHook(key)
		}
		if notice != nil {
			notice.beginWait()
		}
	}
	defer func() {
		if !waiting || notice == nil {
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
			// The keyed waiter map is process-local: the holder is this process.
			return nil, errStagingOwnershipSameProcess
		}
	}

	fence, err := acquire(beginWait)
	if err != nil {
		blobDomainOwners.Lock()
		if blobDomainOwners.m[key] == entry {
			delete(blobDomainOwners.m, key)
		}
		blobDomainOwners.Unlock()
		close(entry.done)
		return nil, err
	}
	return &blobDomain{
		key: key, dest: dst, entry: entry,
		realm: fence.realm, scope: fence.scope, proof: fence.proof,
	}, nil
}

var blobDomainAcquire = acquireDefaultScopedBlobDomain

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

// flatDomainLockSrc returns the shared ranged-staging path for legacy callers;
// current ownership locks are external and never use this as a lock filename.
func flatDomainLockSrc(dst string) string {
	return dst + ".incomplete"
}

type flatDestinationDomain struct {
	key string
	// lockSrc is the legacy field name for the ranged-staging path.
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
	if d == nil || d.released {
		return false
	}
	return d.scope != nil && d.scope.Validate() == nil
}

func (d *blobDomain) ownershipFenceProof() string {
	if d == nil || d.scope == nil || !d.lockHeld() {
		return ""
	}
	return d.proof
}

func (d *blobDomain) release() {
	if d == nil || d.released {
		return
	}
	d.released = true
	if d.scope != nil {
		d.scope.Release()
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
