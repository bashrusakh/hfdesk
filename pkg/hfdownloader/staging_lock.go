// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// The per-path metadata gate serializes intra-item record transitions. Shared
// staging mutation also requires the destination's lifetime OS lock; a local
// gate or record age cannot establish cross-process ownership.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

const stagingLockSuffix = ".lock"

var (
	// stagingLockWait bounds acquisition of the local metadata gate.
	stagingLockWait = 5 * time.Second

	// stagingLockCleanupWait bounds the cleanup path's acquisition. Cleanup is
	// best-effort and must not delay an explicit cancel for long.
	stagingLockCleanupWait = 500 * time.Millisecond
)

// errStagingLockBusy means the local metadata gate exceeded its wait bound.
// Claimants return errBlobIncompleteOwned rather than establishing a claim.
var errStagingLockBusy = errors.New("staging metadata gate is held by another local writer")

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

func stagingLockPath(metaPath string) string { return metaPath + stagingLockSuffix }

// acquire takes the bounded, cancellable per-path metadata gate. The destination
// domain excludes foreign writers, so this intra-item wait needs no slot notice.
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
	select {
	case gate.ch <- struct{}{}:
		l.gate = gate
		return nil
	default:
	}
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

func (l *stagingStateLock) release() {
	if l.gate == nil {
		return
	}
	<-l.gate.ch
	releaseStagingStateGate(l.path, l.gate)
	l.gate = nil
}

// errStagingClaimLost refuses mutation of a missing or foreign claim.
// Re-read identity under the metadata gate so a checkpoint cannot recreate
// ownership or overwrite a replacement record.
var errStagingClaimLost = errors.New("staging claim is no longer owned by this process")

// claimRecordOwnedLocked requires the caller to hold the metadata gate.
func claimRecordOwnedLocked(metaPath string) bool {
	meta, err := readStagingMeta(metaPath)
	return err == nil && meta.Instance != "" && meta.Instance == processInstanceToken
}

// removeOwnedStagingLocked checks record ownership before removal.
// The caller must hold the destination domain and metadata gate.
func removeOwnedStagingLocked(staging, metaPath string) bool {
	if !claimRecordOwnedLocked(metaPath) {
		return false
	}
	removeStagingArtifactsLocked(staging, metaPath)
	return true
}

// removeStagingArtifactsOwned discards a pair under the metadata gate only if
// it still names this claim. The caller must hold the destination domain.
func removeStagingArtifactsOwned(staging, metaPath string) bool {
	lock := &stagingStateLock{path: stagingLockPath(metaPath)}
	if err := lock.acquire(context.Background(), stagingLockCleanupWait); err != nil {
		log.Printf("warning: skip cleanup of staging file %s: local metadata gate unavailable: %v", staging, err)
		return false
	}
	defer lock.release()
	return removeOwnedStagingLocked(staging, metaPath)
}

// writeStagingMeta takes the local gate for test/seeding writes; it does not
// establish destination ownership. Downloader rewrites use
// writeStagingMetaOwnedLocked under the held domain and metadata gate.
func writeStagingMeta(metaPath string, meta *IncompleteMeta) error {
	lock := &stagingStateLock{path: stagingLockPath(metaPath)}
	if err := lock.acquire(context.Background(), stagingLockWait); err != nil {
		return fmt.Errorf("serialize staging record %s: %w", metaPath, err)
	}
	defer lock.release()
	return writeStagingMetaFile(metaPath, meta)
}

// writeStagingMetaOwnedLocked requires the metadata gate and destination
// domain. Missing or foreign instance identity returns errStagingClaimLost
// instead of recreating ownership.
func writeStagingMetaOwnedLocked(metaPath string, meta *IncompleteMeta) error {
	if !claimRecordOwnedLocked(metaPath) {
		return errStagingClaimLost
	}
	return writeStagingMetaFile(metaPath, meta)
}
