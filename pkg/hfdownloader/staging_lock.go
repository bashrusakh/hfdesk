// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Cross-process serialization of staging-record transitions (issue #140).
//
// The staging record (the .meta file) is both the resume state and the
// ownership identity. Deciding to take over a stale record, creating a claim,
// rewriting a checkpoint, heartbeating, and removing the record are all
// read-modify-write transitions over that one file. Without serialization the
// take-over window is a race: two contenders can both read the same stale
// record, and the slower one can remove the faster one's freshly written claim
// before creating its own record. Both then believe they own the staging
// payload and both write into it.
//
// A sibling lock file (metaPath + ".lock") created with O_CREATE|O_EXCL is the
// portable single-winner primitive for those transitions: the filesystem
// guarantees exactly one creator succeeds on every platform Go supports, with
// no new dependency. Every transition above runs inside that lock, and every
// mutation re-reads the record after acquiring the lock, so a contender that
// lost cannot delete or replace a live owner's claim, and a heartbeat cannot
// resurrect ownership after a takeover.
//
// This file is a mutex over record transitions, not the ownership fence. The
// fence is the OS-level ownership lock held for the whole ownership interval
// (staging_oslock.go): a writer that keeps an open payload handle cannot be
// excluded by a record lease, so a claimant only takes over after acquiring
// that lock. On platforms/filesystems without the OS lock, this mutex plus the
// heartbeat/instance-token lease is the fallback and the weaker guarantee is
// documented on the package.
//
// Critical sections are tiny (read, compare and rewrite a small JSON record),
// acquisition is bounded, and an abandoned lock (crashed holder) is broken
// after stagingLockStaleAfter. The exclusive create that follows a stale-lock
// break still has exactly one winner even when several processes break the
// same lock concurrently.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"
)

// stagingLockSuffix is appended to the staging meta path. The lock is a
// sibling of the record, never the record itself, so removing the record
// cannot orphan a held lock and vice versa.
const stagingLockSuffix = ".lock"

// stagingLockRetry is the polling cadence while a live holder owns the lock.
const stagingLockRetry = 2 * time.Millisecond

var (
	// stagingLockWait bounds one acquisition of the transition lock. Critical
	// sections only rewrite a small record, so a healthy holder releases far
	// sooner; a longer wait means the holder is wedged and the caller must
	// back off rather than block the download.
	stagingLockWait = 5 * time.Second

	// stagingLockCleanupWait bounds the cleanup path's acquisition. Cleanup is
	// best-effort and must not delay an explicit cancel for long.
	stagingLockCleanupWait = 500 * time.Millisecond

	// stagingLockStaleAfter is the age after which a lock left behind by a
	// crashed process may be broken. Only a transition that never returned can
	// hold a lock this long.
	stagingLockStaleAfter = claimGracePeriod
)

// errStagingLockBusy reports that the transition lock stayed held for the
// whole bounded acquisition window. Claimants translate it to
// errBlobIncompleteOwned (the staging file is owned by someone else).
var errStagingLockBusy = errors.New("staging transition lock is held by another writer")

// stagingLockRecord is the diagnostic content of the lock file. Nonce makes
// release safe: a holder whose stale lock was broken must not remove the lock
// of the process that succeeded it.
type stagingLockRecord struct {
	Instance string    `json:"instance"`
	Nonce    string    `json:"nonce"`
	PID      int       `json:"pid"`
	Host     string    `json:"host"`
	At       time.Time `json:"at"`
}

// stagingStateLock is one acquisition of the sibling transition lock.
type stagingStateLock struct {
	path  string
	nonce string
}

// stagingLockPath derives the lock path from the staging record path.
func stagingLockPath(metaPath string) string { return metaPath + stagingLockSuffix }

// acquire creates and holds the transition lock, waiting at most wait for a
// live holder to release it. A lock older than stagingLockStaleAfter is broken;
// the subsequent exclusive create is still the single-winner operation.
func (l *stagingStateLock) acquire(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		wait = stagingLockWait
	}
	deadline := time.Now().Add(wait)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			if l.nonce == "" {
				l.nonce = newRandomToken()
			}
			rec := stagingLockRecord{
				Instance: processInstanceToken,
				Nonce:    l.nonce,
				PID:      os.Getpid(),
				Host:     processHostname,
				At:       time.Now().UTC(),
			}
			data, merr := json.Marshal(rec)
			if merr == nil {
				_, merr = f.Write(data)
			}
			if cerr := f.Close(); merr == nil {
				merr = cerr
			}
			if merr != nil {
				_ = os.Remove(l.path)
				return merr
			}
			return nil
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if fi, serr := os.Stat(l.path); serr == nil && time.Since(fi.ModTime()) >= stagingLockStaleAfter {
			// Abandoned by a crashed holder. Removing is racy between several
			// breakers, but the exclusive create above is not: exactly one of
			// them recreates the lock.
			_ = os.Remove(l.path)
			continue
		}
		if !time.Now().Before(deadline) {
			return errStagingLockBusy
		}
		if !sleepCtx(ctx, stagingLockRetry) {
			return ctx.Err()
		}
	}
}

// release removes the lock only while it still carries this acquisition's
// nonce, so a holder whose stale lock was broken cannot delete its successor.
func (l *stagingStateLock) release() {
	if l.nonce == "" {
		return
	}
	defer func() { l.nonce = "" }()
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	var rec stagingLockRecord
	if json.Unmarshal(raw, &rec) != nil || rec.Nonce != l.nonce {
		return
	}
	_ = os.Remove(l.path)
}

// errStagingClaimLost reports that this process no longer owns the staging
// record: a take-over replaced it while this writer was active. Every metadata
// mutation checks ownership under the transition lock, so a heartbeat or
// checkpoint can never overwrite (and thereby steal back) a newer claim.
var errStagingClaimLost = errors.New("staging claim is no longer owned by this process")

// claimRecordOwnedLocked reports whether the record still names this process
// instance. The caller holds the transition lock.
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

// removeStagingArtifactsOwned acquires the transition lock and removes the
// pair only if the record still names this process instance. Owner-side
// discard paths (hash mismatch) use it so a writer that lost a stale claim
// cannot delete the new owner's data. The caller must already hold the staging
// ownership lock for the pair; re-acquiring a lock this process already holds
// on the same file would self-conflict, so this helper never takes it.
func removeStagingArtifactsOwned(staging, metaPath string) bool {
	lock := &stagingStateLock{path: stagingLockPath(metaPath)}
	if err := lock.acquire(context.Background(), stagingLockCleanupWait); err != nil {
		log.Printf("warning: skip cleanup of staging file %s: claim transition lock unavailable: %v", staging, err)
		return false
	}
	defer lock.release()
	return removeOwnedStagingLocked(staging, metaPath)
}

// writeStagingMeta writes a record through the transition lock. It is the
// safe primitive for callers that do not already hold the lock (tests and
// one-off seeding); the downloader itself uses writeStagingMetaOwnedLocked so
// it also enforces claim ownership.
func writeStagingMeta(metaPath string, meta *IncompleteMeta) error {
	lock := &stagingStateLock{path: stagingLockPath(metaPath)}
	if err := lock.acquire(context.Background(), stagingLockWait); err != nil {
		return fmt.Errorf("lock staging record %s: %w", metaPath, err)
	}
	defer lock.release()
	return writeStagingMetaFile(metaPath, meta)
}

// writeStagingMetaOwnedLocked rewrites the record only while it still names
// this process instance. The caller holds the transition lock. A missing or
// replaced record means the claim was taken over: the write is refused with
// errStagingClaimLost instead of silently recreating ownership.
func writeStagingMetaOwnedLocked(metaPath string, meta *IncompleteMeta) error {
	if !claimRecordOwnedLocked(metaPath) {
		return errStagingClaimLost
	}
	return writeStagingMetaFile(metaPath, meta)
}
