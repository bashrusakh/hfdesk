// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// In-process serialization of staging-record transitions (issue #140).
// Current-version cross-process exclusion is provided by the lifetime OS lock
// in staging_oslock.go, which every shared staging mutator must hold. This
// per-path gate serializes short metadata updates and is deliberately not a
// persistent lock: age can never prove that a suspended holder will not resume.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// stagingLockSuffix supplies a stable key for the per-path in-process gate.
const stagingLockSuffix = ".lock"

var (
	// stagingLockWait bounds one acquisition of the local metadata gate. Critical
	// sections only rewrite a small record, so a healthy holder releases far
	// sooner; a longer wait means the holder is wedged and the caller must
	// back off rather than block the download.
	stagingLockWait = 5 * time.Second

	// stagingLockCleanupWait bounds the cleanup path's acquisition. Cleanup is
	// best-effort and must not delay an explicit cancel for long.
	stagingLockCleanupWait = 500 * time.Millisecond
)

// errStagingLockBusy reports that the local metadata gate stayed held for the
// whole bounded acquisition window. Claimants translate it to
// errBlobIncompleteOwned (the staging file is owned by someone else).
var errStagingLockBusy = errors.New("staging metadata gate is held by another local writer")

// stagingStateLock is one acquisition of a per-path in-process gate.
type stagingStateLock struct {
	path string
	gate *stagingStateGate
}

type stagingStateGate struct {
	ch   chan struct{}
	refs int
}

var stagingStateGates = struct {
	sync.Mutex
	m map[string]*stagingStateGate
}{m: make(map[string]*stagingStateGate)}

// stagingLockPath derives the stable gate key from the staging record path.
func stagingLockPath(metaPath string) string { return metaPath + stagingLockSuffix }

// acquire waits at most wait for the local gate, while honoring cancellation.
func (l *stagingStateLock) acquire(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		wait = stagingLockWait
	}
	stagingStateGates.Lock()
	gate := stagingStateGates.m[l.path]
	if gate == nil {
		gate = &stagingStateGate{ch: make(chan struct{}, 1)}
		stagingStateGates.m[l.path] = gate
	}
	gate.refs++
	stagingStateGates.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case gate.ch <- struct{}{}:
		l.gate = gate
		return nil
	case <-ctx.Done():
		releaseStagingStateGate(l.path, gate)
		return ctx.Err()
	case <-timer.C:
		releaseStagingStateGate(l.path, gate)
		return errStagingLockBusy
	}
}

func releaseStagingStateGate(key string, gate *stagingStateGate) {
	stagingStateGates.Lock()
	gate.refs--
	if gate.refs == 0 && stagingStateGates.m[key] == gate {
		delete(stagingStateGates.m, key)
	}
	stagingStateGates.Unlock()
}

// release returns the local gate to its next waiter.
func (l *stagingStateLock) release() {
	if l.gate == nil {
		return
	}
	<-l.gate.ch
	releaseStagingStateGate(l.path, l.gate)
	l.gate = nil
}

// errStagingClaimLost reports that this process no longer owns the staging
// record: a take-over replaced it while this writer was active. Every metadata
// mutation checks ownership under the metadata gate, so a heartbeat or
// checkpoint can never overwrite (and thereby steal back) a newer claim.
var errStagingClaimLost = errors.New("staging claim is no longer owned by this process")

// claimRecordOwnedLocked reports whether the record still names this process
// instance. The caller holds the metadata gate.
func claimRecordOwnedLocked(metaPath string) bool {
	meta, err := readStagingMeta(metaPath)
	return err == nil && meta.Instance != "" && meta.Instance == processInstanceToken
}

// removeOwnedStagingLocked removes the record and its payload only while the
// record still names this process instance. The caller holds the transition
// lock and the staging ownership lock for the pair. It returns false (removing
// nothing) when the claim moved elsewhere.
func removeOwnedStagingLocked(staging, metaPath string) bool {
	if !claimRecordOwnedLocked(metaPath) {
		return false
	}
	removeStagingArtifactsLocked(staging, metaPath)
	return true
}

// removeStagingArtifactsOwned acquires the metadata gate and removes the
// pair only if the record still names this process instance. Owner-side
// discard paths (hash mismatch) use it so a writer that lost a stale claim
// cannot delete the new owner's data. The caller must already hold the staging
// ownership lock for the pair; re-acquiring a lock this process already holds
// on the same file would self-conflict, so this helper never takes it.
func removeStagingArtifactsOwned(staging, metaPath string) bool {
	lock := &stagingStateLock{path: stagingLockPath(metaPath)}
	if err := lock.acquire(context.Background(), stagingLockCleanupWait); err != nil {
		log.Printf("warning: skip cleanup of staging file %s: local metadata gate unavailable: %v", staging, err)
		return false
	}
	defer lock.release()
	return removeOwnedStagingLocked(staging, metaPath)
}

// writeStagingMeta writes a record through the metadata gate. It is the
// safe primitive for callers that do not already hold the lock (tests and
// one-off seeding); the downloader itself uses writeStagingMetaOwnedLocked so
// it also enforces claim ownership.
func writeStagingMeta(metaPath string, meta *IncompleteMeta) error {
	lock := &stagingStateLock{path: stagingLockPath(metaPath)}
	if err := lock.acquire(context.Background(), stagingLockWait); err != nil {
		return fmt.Errorf("serialize staging record %s: %w", metaPath, err)
	}
	defer lock.release()
	return writeStagingMetaFile(metaPath, meta)
}

// writeStagingMetaOwnedLocked rewrites the record only while it still names
// this process instance. The caller holds the metadata gate. A missing or
// replaced record means the claim was taken over: the write is refused with
// errStagingClaimLost instead of silently recreating ownership.
func writeStagingMetaOwnedLocked(metaPath string, meta *IncompleteMeta) error {
	if !claimRecordOwnedLocked(metaPath) {
		return errStagingClaimLost
	}
	return writeStagingMetaFile(metaPath, meta)
}
