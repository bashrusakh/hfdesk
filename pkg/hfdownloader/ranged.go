// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Ranged multipart downloads (issue #140).
//
// The downloader writes every ranged worker directly into ONE staging file
// at its assigned offset (WriteAt on a preallocated file) instead of
// downloading per-connection .part-NN payload files and stitching them
// with a full-file assembly copy. The staging file is:
//
//   - HF cache mode, LFS blob with a known SHA-256:
//     blobs/<sha256>.incomplete (RepoDir.IncompletePath) — the future blob
//     itself, so finalization is a rename into blobs/<sha256> plus
//     snapshot/friendly link work, never another payload-sized copy.
//   - every other destination: <dst>.incomplete, adjacent to the final
//     file (same directory, so the final rename is atomic and cheap).
//
// Resume state lives in a versioned metadata file next to the staging file
// (staging+".meta", i.e. blobs/<sha>.incomplete.meta in cache mode — exactly
// RepoDir.IncompleteMetaPath). It extends IncompleteMeta with the range
// layout and per-range committed prefixes. The persisted range contract is
// authoritative for an existing incomplete download and is independent of the
// worker count, so changing Settings.Concurrency between runs re-fetches
// only the missing bytes.
//
// A record is trusted only when its repo/path/size/resolved-commit identity
// matches, its blob SHA is present on both sides or neither, its ranges tile
// [0,size) with normalized completion (Done means all bytes arrived under a
// response whose framing ended cleanly), and the staging payload exists at the expected logical size
// before any create/truncate. A record whose payload vanished is discarded and
// re-downloaded; it is never allowed to publish a preallocated zero file.
//
// Ownership has three levels. A random per-process instance token distinguishes
// this process instance from another (PID reuse, containers sharing PID 1),
// and a single heartbeat goroutine keeps the claim record fresh through
// transfers, retry waits, strict verification and finalization for identity
// and diagnostics. The exclusion fence is an OS-level exclusive lock on the
// persistent sibling file <staging>.lock, acquired before the claim transition
// and held for the whole ownership interval — claim read, create/resume,
// transfer, hash, publish/cleanup. A busy lock means a live owner, including a
// suspended process that stopped heartbeating but could still resume writing
// through an open handle; a crashed process releases the lock at death, so
// takeover/resume is immediate. Every record transition — claim, take-over,
// checkpoint, heartbeat, removal — use the per-path in-process metadata gate;
// the lifetime OS lock excludes every current-version foreign mutator. There
// is no lease-only mutation fallback: unsupported or failed lock acquisition
// returns an operation error before claim/payload mutation. A legacy record
// without LockFenced remains protected unless same-host owner death is proven.
// Publish re-checks the record under the metadata gate and verifies the held OS
// lock before rename; a missing or foreign record returns errStagingClaimLost.
//
// Completion is content-addressed: only a publish target whose name IS the
// known blob digest proves the transfer is complete. An arbitrary destination
// (a legacy/output dst, or a tmp-<sha> staging name for an item without a
// digest) that already exists is not trusted by the claim: the pre-download
// skip check is authoritative for those paths, and a declined file is
// re-downloaded and renamed over the old bytes.
//
// Crash-safety contract: payload data is written BEFORE its checkpoint is
// advanced; the metadata may lag the payload; metadata never claims bytes
// that were not successfully written. The staging file is preallocated to
// the expected logical size, and its on-disk length is NEVER treated as
// evidence of completion — a sparse/preallocated file reports its final
// size from the first truncate, so only the committed ranges count.
//
// Legacy bytes imported from a pre-#140 .part-NN/.parts.json family or a
// single .part have layout/prefix structure but no validated provenance.
// They are tagged LegacyMigrated and content-proven with one full-file SHA-256
// pass before publication whenever the item carries a digest and Verify is not
// "none" (the explicit one-time migration exception to the size/sha256
// contract). A mismatch discards the staging data and never publishes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// stagingMetaVersion is the IncompleteMeta.Version value written by this
// downloader. Version 3 marks range completion only after clean response
// framing; version 2 checkpoints are migrated with their last byte reserved
// for revalidation.
const stagingMetaVersion = 3

const priorStagingMetaVersion = 2

// linkStagingClaimRecord atomically publishes a complete fresh record without
// replacing an existing path. Tests replace it to exercise the crash/error
// boundary between temp completion and final claim installation, and to
// simulate a filesystem that cannot create hard links at all.
var linkStagingClaimRecord = os.Link

// renameStagingClaimRecord installs a staged record over an existing one. It
// is the replacement primitive and the install used on filesystems that cannot
// create hard links. Tests replace it to exercise the failure boundary between
// staging a complete record and installing it.
var renameStagingClaimRecord = os.Rename

// stagingRecordTempSuffix is the suffix of the staged-record path beside the
// authoritative staging record. The staged file holds a complete record that
// is installed by an atomic link or rename; it is never authoritative itself.
const stagingRecordTempSuffix = ".tmp"

// claimGracePeriod is the maximum diagnostic heartbeat interval before an
// owner rewrites its metadata. It is not ownership authority: only the lifetime
// OS lock permits or excludes a current-version mutation.
const claimGracePeriod = 10 * time.Second

// claimHeartbeatEvery is how often an owner rewrites diagnostic/resume metadata.
const claimHeartbeatEvery = claimGracePeriod / 2

// stagingHeartbeatEvery is the running heartbeat cadence. It is a variable so
// tests can shorten the finalization-length pause they simulate; production
// keeps the derived constant.
var stagingHeartbeatEvery = claimHeartbeatEvery

// errNoLegacyState reports that no usable legacy .part-NN/.parts.json state
// exists for a destination (missing layout, or a layout that does not tile
// the file, in which case the unvalidatable artifacts were discarded).
var errNoLegacyState = errors.New("no valid legacy multipart state")

var (
	// errBlobIncompleteOwned reports that the staging file for this
	// destination is currently owned by another live writer (another
	// process, or a concurrently running download in this process). The
	// caller must skip the file this run rather than touch the staging.
	errBlobIncompleteOwned = errors.New("incomplete download is owned by another live writer")

	// errBlobAlreadyComplete reports that a content-addressed publish target
	// (blobs/<known-sha>) already exists, so the transfer is unnecessary this
	// run. Arbitrary destinations are deliberately not completion proof: their
	// pre-download skip check is authoritative and a declined file must be
	// re-downloaded (a concurrent writer finishing is not a skip decision).
	errBlobAlreadyComplete = errors.New("destination already complete")
)

// stagingOwners is the in-process exclusion registry keyed by staging file
// path. It closes the same-process half of the CheckBlob/claim TOCTOU
// window: CheckBlob deliberately reports a staging record carrying our own
// PID as take-over-able (an earlier run of this process has no running
// writer and must be resumable), so the registry is what distinguishes that
// case from a concurrently running goroutine of this process. Each entry's
// channel is closed when the owning download returns; a claimant that finds
// a busy entry waits for the owner and then re-evaluates from scratch. The
// entry records the owning process instance token so the registry and the
// on-disk record describe the same ownership identity.
type stagingOwnerEntry struct {
	done     chan struct{}
	instance string
}

var (
	stagingOwnersMu sync.Mutex
	stagingOwners   = map[string]*stagingOwnerEntry{}
)

// stagingClaimActive reports whether this process currently has a live claim
// on the staging path. Cleanup helpers consult it before removing anything.
func stagingClaimActive(staging string) bool {
	stagingOwnersMu.Lock()
	entry, busy := stagingOwners[staging]
	stagingOwnersMu.Unlock()
	return busy && entry.instance == processInstanceToken
}

// removeStagingArtifactsIfUnclaimed removes a staging payload/meta pair only
// when no live claim remains. The liveness decision is re-made under the OS
// ownership lock and local metadata gate, so a writer that claimed (or was
// taken over) between the pre-check and the removal is not disturbed. A busy
// OS lock is a live owner (even a suspended one whose heartbeat went stale)
// and skips the removal. Best-effort: a skipped or failed removal is logged
// and never turns into a hard cleanup error.
func removeStagingArtifactsIfUnclaimed(staging, metaPath string) {
	// Cheap pre-check: an active in-process claim cannot be removed.
	if stagingClaimActive(staging) {
		log.Printf("warning: skip cleanup of staging file %s: a live claim remains", staging)
		return
	}
	// Fast path: a destination that never had ranged staging has nothing to
	// remove, and taking the ownership lock would create a persistent lock file
	// beside it for no reason. Removing nothing is safe even if a claim starts
	// immediately afterwards.
	if stagingArtifactsAbsent(staging, metaPath) {
		return
	}
	own, oerr := acquireStagingOwnershipLockWithin(context.Background(), staging, stagingLockCleanupWait)
	if errors.Is(oerr, errStagingOwnershipBusy) {
		log.Printf("warning: skip cleanup of staging file %s: a live owner holds the staging lock", staging)
		return
	}
	if oerr != nil {
		log.Printf("warning: skip cleanup of staging file %s: staging ownership lock unavailable: %v", staging, oerr)
		return
	}
	defer own.release()

	lock := &stagingStateLock{path: stagingLockPath(metaPath)}
	if err := lock.acquire(context.Background(), stagingLockCleanupWait); err != nil {
		log.Printf("warning: skip cleanup of staging file %s: local metadata gate unavailable: %v", staging, err)
		return
	}
	defer lock.release()

	// Re-check under the lock: a claim may have started after the pre-check.
	if stagingClaimActive(staging) {
		log.Printf("warning: skip cleanup of staging file %s: a live claim remains", staging)
		return
	}
	meta, err := readStagingMeta(metaPath)
	switch {
	case err == nil:
		// A free OS lock proves termination only for writers that used this
		// lock. Legacy/unfenced records remain protected unless same-host PID
		// evidence proves their owner dead.
		if !meta.LockFenced && !stagingOwnerProvablyDead(meta) && !stagingOwnerIsSelf(meta) {
			log.Printf("warning: skip cleanup of staging file %s: a live claim remains", staging)
			return
		}
	case errors.Is(err, os.ErrNotExist):
		// No record: remove an orphaned payload if one exists.
	default:
		// An unreadable record cannot establish that its writer is dead.
		log.Printf("warning: skip cleanup of staging file %s: ownership record is unreadable", staging)
		return
	}
	removeStagingArtifactsLocked(staging, metaPath)
}

// Observability counters for tests and the opt-in local-I/O benchmark.
// Package variables (not public API) in the established pattern of
// maxAttemptsPerRetry; package tests run sequentially and reset them.
var (
	// stagingWriteBytes counts all payload bytes written into staging files
	// by the ranged downloader: the network transfer landing on disk plus any
	// one-time legacy migration copy. It is not "extra" local I/O; compare it
	// with the file size to derive the transfer factor.
	stagingWriteBytes atomic.Int64
	// hashPassCount counts full-file SHA-256 passes over downloaded
	// payloads and hashPassBytes the bytes they read. Strict Verify=sha256
	// must perform at most one such pass per file on the normal path;
	// Verify=size performs none there (the one-time legacy migration content
	// proof is the documented exception).
	hashPassCount atomic.Int64
	hashPassBytes atomic.Int64
	// finalizationNanos is the wall time of the most recent successful
	// multipart finalization phase: from the last transferred byte (the
	// file_finalizing signal) through the optional strict hash and the publish
	// rename, stopping at that rename. It excludes caller-level work after
	// downloadMultipart returns (cache snapshot/friendly link creation, the
	// manifest, and other job finalization). The opt-in local-I/O benchmark
	// reports it as the network-complete -> file-published metric. It describes
	// the last successful file, so it is meaningful for sequential package
	// tests and the benchmark, not as a per-file multi-download aggregate.
	finalizationNanos atomic.Int64
)

// multipartOpts parameterizes downloadMultipart's staging/publish layout.
// The zero value stages at dst+".incomplete" and publishes with a rename
// to dst, which is what plain (non-cache) destinations and the package
// tests use. Download passes the blob-keyed paths for LFS files in HF
// cache mode so the staging file IS the future blob.
type multipartOpts struct {
	// Staging is the staging payload path. Empty means dst+".incomplete".
	Staging string
	// Publish is the success rename target. Empty means dst.
	Publish string
	// Commit is the resolved commit SHA of the plan that produced this item.
	// It is part of the resume identity; the requested branch name (Job.Revision)
	// is not enough because the branch may move between runs.
	Commit string
	// ContentAddressed marks Publish as a content-addressed blob path whose
	// existence alone proves the transfer complete (blobs/<known-sha>). When
	// false (legacy/output dst, tmp-<sha> staging for an unknown digest), an
	// existing publish target is stale bytes the claim must not skip: the
	// caller's pre-download skip decision is authoritative for that path.
	ContentAddressed bool
	// DirectBlobPublished reports whether this call actually published to
	// Publish. When size remains unknown after HEAD, downloadMultipart falls
	// back to the single-path temp destination instead; the caller must then
	// pass that temp through the normal cache-store owner.
	DirectBlobPublished *bool
	// Domain is the ownership domain of the content-addressed blob key this
	// item publishes to. The caller holds it for the whole operation on that
	// key. The publication needs it for the domain-exclusive placement tier
	// (a filesystem without hard links), where the existence check and the
	// placement are only exclusive to the domain while it is held.
	Domain *blobDomain
}

// stagingClaimState is what a successful claim produced.
type stagingClaimState struct {
	// ranges is the authoritative committed range layout resumed from the
	// previous owner's record; nil for a fresh claim.
	ranges []RangeProgress
	// fresh reports that no v2 range state existed, so the staging payload
	// holds no validated bytes.
	fresh bool
	// legacyMigrated reports that the record's bytes originated in a pre-#140
	// partial and therefore still need the one-time content proof (T2).
	legacyMigrated bool
	// proofPending reports that the record still carries the durable
	// known-digest proof gate; the run must prove before publication regardless
	// of the current mode (except Verify="none").
	proofPending bool
	// release deregisters the in-process ownership slot and releases the OS
	// ownership lock; the caller must invoke it exactly once when the
	// download returns.
	release func()
	// own is the required held OS-level lifetime ownership lock.
	own *stagingOwnershipLock
	// osExclusive reports that own fences the whole interval, so a fresh
	// foreign record is a crash remnant rather than a live writer.
	osExclusive bool
}

// claimStaging establishes exclusive ownership of the staging file.
//
// Exclusion combines local reservation/metadata serialization with the
// cross-process lifetime OS fence:
//
//   - in-process: the stagingOwners registry serializes downloads inside
//     this process (including two plan items that share one blob SHA);
//   - OS-level: the persistent <staging>.lock is taken exclusively and held
//     for the whole ownership interval. The acquisition waits out transient
//     contention (a read-only status probe shares the lock file briefly), so a
//     probe in flight does not read as an owner. Only a PROVEN exclusive
//     holder is a live owner (even one that stopped heartbeating while
//     suspended), so the caller skips with errBlobIncompleteOwned. A crashed
//     owner's lock is released by the kernel at death, so takeover is safe
//     only after the old process is gone. If the platform/filesystem cannot
//     supply the lock, the operation fails closed;
//   - record transitions: the per-path in-process metadata gate serializes
//     local read-modify-write operations. Current-version foreign processes
//     cannot reach them without first acquiring the lifetime OS lock. A
//     non-fenced legacy record is retained unless its same-host owner is proven
//     dead.
//
// A take-over preserves the record's committed ranges only when the record is
// a fully normalized, identity-matching v2 record AND its staging payload
// exists at the expected logical size. The evaluate/remove/create sequence
// runs under both gates, so one claimant cannot displace another. contentAddressed
// marks the publish target as a known blob digest, the only destination whose
// existence proves completion.
func claimStaging(ctx context.Context, staging, publish, metaPath string, it PlanItem, job Job, commit string, cfg Settings, contentAddressed bool) (*stagingClaimState, error) {
	// Durable proof gate: a fresh claim for a known-digest mode starts with
	// ProofPending set, and a take-over preserves the record's flag (or adds it
	// when the current mode/item still needs the proof). The claim write is
	// therefore durable before any transfer or proof starts.
	proofMode := it.SHA256 != "" && (cfg.Verify == "sha256" || cfg.Verify == "etag")
	proofForKnownDigest := it.SHA256 != "" && cfg.Verify != "none"

	for round := 0; round < 8; round++ {
		// A content-addressed blob may have been published while we were
		// getting here (or while we waited for the in-process owner above).
		// Re-checked every round because a waited-for owner often publishes
		// just before waking us. An arbitrary destination is never completion
		// proof here: the caller's authoritative skip check already declined it.
		if contentAddressed {
			if _, err := os.Stat(publish); err == nil {
				return nil, errBlobAlreadyComplete
			}
		}

		// In-process exclusion.
		stagingOwnersMu.Lock()
		entry, busy := stagingOwners[staging]
		if !busy {
			entry = &stagingOwnerEntry{done: make(chan struct{}), instance: processInstanceToken}
			stagingOwners[staging] = entry
		}
		stagingOwnersMu.Unlock()
		if busy {
			select {
			case <-entry.done:
				// Owner finished: re-evaluate (publish may now exist).
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		// OS-level exclusion for the whole ownership interval. This is the
		// fence the record lease cannot provide; it must be acquired before
		// the record is evaluated so a suspended predecessor can never resume
		// writing after we take its record over.
		//
		// The acquisition observes the lock non-owningly and waits out
		// transient contention: a read-only status probe touches the same
		// lock file briefly, and on Unix a probe's shared lock makes this
		// exclusive acquisition fail even though no writer exists. Only a
		// PROVEN exclusive holder skips the file; an unresolvable contention
		// is an actionable failure, never a successful skip.
		own, oerr := acquireStagingOwnershipLockCtx(ctx, staging)
		osExclusive := oerr == nil
		if oerr != nil {
			stagingOwnersMu.Lock()
			delete(stagingOwners, staging)
			stagingOwnersMu.Unlock()
			close(entry.done)
			if errors.Is(oerr, errStagingOwnershipBusy) {
				// A proven live writer owns this staging file.
				return nil, errBlobIncompleteOwned
			}
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			return nil, fmt.Errorf("cannot safely claim ranged staging %s: lifetime ownership lock acquisition failed: %w", staging, oerr)
		}
		release := func() {
			// Release the OS lock before waking an in-process waiter: the
			// waiter re-evaluates from scratch (and would otherwise observe
			// this process's own lock as busy and skip a finished file).
			own.release()
			stagingOwnersMu.Lock()
			delete(stagingOwners, staging)
			stagingOwnersMu.Unlock()
			close(entry.done)
		}

		// Local record-transition serialization; the lifetime OS lock already excludes other current-version processes.
		lock := &stagingStateLock{path: stagingLockPath(metaPath)}
		if lerr := lock.acquire(ctx, stagingLockWait); lerr != nil {
			release()
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			if errors.Is(lerr, errStagingLockBusy) {
				// Another process is mid-transition on this record; for this
				// run the staging file is owned.
				return nil, errBlobIncompleteOwned
			}
			return nil, lerr
		}
		state, retry, cerr := claimStagingLocked(staging, metaPath, it, job, commit, osExclusive, proofMode, proofForKnownDigest)
		lock.release()
		if cerr != nil {
			release()
			return nil, cerr
		}
		if retry {
			release()
			continue
		}
		state.release = release
		state.own = own
		state.osExclusive = osExclusive
		return state, nil
	}
	return nil, fmt.Errorf("could not claim staging file %s: claim did not converge", staging)
}

// claimStagingLocked performs one evaluate/transition pass while the caller
// holds the staging metadata gate and (when osExclusive) the OS ownership
// lock. retry=true asks the caller to re-evaluate after discarding unusable
// state or after losing an exclusive create to an older writer that does not
// participate in the lock.
func claimStagingLocked(staging, metaPath string, it PlanItem, job Job, commit string, osExclusive, proofMode, proofForKnownDigest bool) (*stagingClaimState, bool, error) {
	raw, rerr := os.ReadFile(metaPath)
	if errors.Is(rerr, os.ErrNotExist) {
		meta := newClaimMeta(it, job, commit)
		// A fresh known-digest mode claim persists the durable proof gate in
		// its very first record write, before any transfer can start. The
		// ownership mode is persisted too, so CheckBlob can use a free OS lock
		// as the authoritative no-writer signal on supported platforms.
		meta.ProofPending = proofMode
		meta.LockFenced = osExclusive
		return createStagingClaim(metaPath, meta)
	}
	if rerr != nil {
		return nil, false, rerr
	}
	var meta IncompleteMeta
	if json.Unmarshal(raw, &meta) != nil {
		// Unreadable record. With the OS ownership lock held, no live
		// current-version writer can be mid-claim: every current writer creates
		// this record while holding the lifetime lock. An unreadable legacy
		// record cannot establish that its owner is dead, regardless of age.
		return nil, false, errBlobIncompleteOwned
	}

	// A LockFenced record belongs to the current lifetime-lock protocol. A
	// legacy/unfenced record is not proven dead by a free new lock.
	if !meta.LockFenced && !stagingOwnerProvablyDead(&meta) && !stagingOwnerIsSelf(&meta) {
		return nil, false, errBlobIncompleteOwned
	}

	// Dead, stale, or same-instance owner: take over. A fully validated v2
	// record for this exact resolved object with a payload present at the
	// expected logical size preserves its committed ranges (resume). Anything
	// else is unvalidated data and is discarded per the downloader correctness
	// rules (never guess at unknown resume state). The metadata gate is
	// held, so the record is still the one evaluated above: a racer cannot
	// have replaced it, and removing it cannot displace a live claim.
	ranges, resumable := stagingMetaResumable(meta, it, job, commit)
	if !resumable || !stagingPayloadSizeOK(staging, it.Size) {
		removeStagingArtifactsLocked(staging, metaPath)
		return nil, true, nil
	}
	// Atomically replace the validated predecessor. Until the rename installs
	// the complete new record, the old resume authority remains intact; a crash
	// during temp creation cannot leave a new empty/partial claim at metaPath.
	take := newClaimMeta(it, job, commit)
	take.StartedAt = meta.StartedAt
	take.Ranges = ranges
	take.LegacyMigrated = meta.LegacyMigrated
	// Preserve the durable gate, add it for the current known-digest mode or
	// for an older record whose legacy-migrated bytes still need the proof.
	take.ProofPending = meta.ProofPending || proofMode || (meta.LegacyMigrated && proofForKnownDigest)
	// Record the ownership mode of THIS claim, not the predecessor's: the
	// current process holds the required OS lifetime lock.
	take.LockFenced = osExclusive
	if err := writeStagingMetaFile(metaPath, &take); err != nil {
		return nil, false, err
	}
	if !claimRecordOurs(metaPath) {
		return nil, false, errStagingClaimLost
	}
	return stagingClaimStateFromMeta(take), false, nil
}

// createStagingClaim atomically installs a complete record for a fresh claim
// without replacing a record that appeared after the caller's absent check.
//
// The complete JSON is staged in the sibling temp file <meta>.tmp and then
// installed by one atomic operation:
//
//   - a hard link of the staged inode to metaPath. Link creation is atomic and
//     no-replace, so a record that appeared after the caller's absent check is
//     never replaced.
//   - when the filesystem cannot create hard links at all, an atomic rename of
//     the staged file to metaPath. A rename does replace an existing target, so
//     it is used only after confirming that no record is present, and it is
//     safe because the lifetime ownership lock and the local metadata gate
//     exclude every foreign mutator for the whole transition.
//
// The staged file is created with O_EXCL, so it is always a fresh inode. A
// staged path left behind by an interrupted operation is therefore never
// written through: it may still be a hard link to the authoritative record
// (an install that crashed before removing its staged name), and truncating it
// would empty the authoritative record before it was replaced.
//
// A crash during staging leaves no authoritative claim; retry=true means
// another writer installed the record first and the caller must re-evaluate it.
func createStagingClaim(metaPath string, meta IncompleteMeta) (*stagingClaimState, bool, error) {
	data, err := json.Marshal(meta)
	if err != nil {
		return nil, false, err
	}
	tmp, err := stageStagingClaimRecord(metaPath, data)
	if err != nil {
		return nil, false, err
	}
	installed, retry, err := installStagedClaimRecord(metaPath, tmp)
	if !installed {
		// The staged copy was not installed: remove it so the residue cannot
		// be mistaken for an authoritative record. A removal failure is not
		// fatal: the staged path is never authoritative.
		if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			log.Printf("warning: remove staged claim record %s: %v", tmp, rmErr)
		}
		return nil, retry, err
	}
	// The staged name is dropped after the install. A hard-link install leaves
	// both names on one inode, so the staged name must not survive as an alias
	// of the authoritative record: a later run would clear and rewrite it.
	// A rename install already moved the staged file away.
	if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		log.Printf("warning: remove staged claim record %s: %v", tmp, rmErr)
	}
	// Read-back verification: the atomically installed record must still carry
	// our instance token.
	if !claimRecordOurs(metaPath) {
		return nil, false, errStagingClaimLost
	}
	return stagingClaimStateFromMeta(meta), false, nil
}

// stageStagingClaimRecord writes the complete record bytes to the sibling temp
// file <meta>.tmp and returns its path.
//
// The staged path is cleared first and then created with O_EXCL, so the staged
// file is always a fresh inode. A staged path that survived an interrupted
// operation may be a hard link to the authoritative record (an install that
// crashed between linking and removing its staged name); writing through it
// would truncate the authoritative record before it was replaced. Clearing the
// staged PATH never touches the authoritative record: unlinking an alias drops
// only the extra name, and the record keeps its own name and its bytes.
//
// A staged path that is not a regular file (a directory, say) is an error
// rather than something to clear: it cannot hold a staged record, and the
// operation must fail without touching the authoritative record.
func stageStagingClaimRecord(metaPath string, data []byte) (string, error) {
	tmp := metaPath + stagingRecordTempSuffix
	if err := clearStagingRecordTemp(tmp); err != nil {
		return "", err
	}
	// O_EXCL: the staged file is a fresh inode, never a truncation of a file
	// left by an earlier operation.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("stage initial claim record %s: %w", metaPath, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("stage initial claim record %s: %w", metaPath, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("stage initial claim record %s: %w", metaPath, err)
	}
	return tmp, nil
}

// clearStagingRecordTemp removes a staged-record residue so the path can be
// recreated as a fresh file. It reports an error when the path holds
// something that cannot be a staged record.
func clearStagingRecordTemp(tmp string) error {
	info, err := os.Lstat(tmp)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect staged claim record %s: %w", tmp, err)
	}
	if info.Mode()&os.ModeSymlink == 0 && !info.Mode().IsRegular() {
		return fmt.Errorf("staged claim record %s is not a regular file", tmp)
	}
	// lgtm[go/path-injection]
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear staged claim record %s: %w", tmp, err)
	}
	return nil
}

// installStagedClaimRecord installs the staged record at metaPath.
//
// The hard-link path is atomic and no-replace: a record that appeared after
// the caller's absent check is reported as "not installed" (retry) and is
// never replaced. When the filesystem cannot create hard links, the install
// falls back to an atomic rename, which does replace the target; it is used
// only when no record is present, and it is safe because the lifetime
// ownership lock and the local metadata gate exclude every foreign mutator
// for the whole transition.
//
// A hard-link failure that is NOT a lack of hard-link support is a real
// failure: it is reported, never silently downgraded to a replacing rename.
func installStagedClaimRecord(metaPath, tmp string) (installed bool, retry bool, err error) {
	linkErr := linkStagingClaimRecord(tmp, metaPath)
	if linkErr == nil {
		return true, false, nil
	}
	if errors.Is(linkErr, os.ErrExist) {
		// Another writer installed a record first. It is authoritative.
		return false, true, nil
	}
	if !stagingHardLinkUnsupported(linkErr) {
		return false, false, fmt.Errorf("install initial claim record %s: %w", metaPath, linkErr)
	}
	// This filesystem cannot create hard links. Install by rename instead,
	// but only while no record is present: a record that appeared is
	// authoritative and must not be replaced.
	if _, statErr := os.Lstat(metaPath); statErr == nil {
		return false, true, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return false, false, fmt.Errorf("install initial claim record %s: %w", metaPath, statErr)
	}
	if renameErr := renameStagingClaimRecord(tmp, metaPath); renameErr != nil {
		return false, false, fmt.Errorf("install initial claim record %s: %w", metaPath, renameErr)
	}
	return true, false, nil
}

// stagingHardLinkUnsupported reports whether err means the filesystem cannot
// create hard links at all, as opposed to a transient or specific failure
// (a full disk, an I/O error). Only the former justifies the rename-based
// install; every other failure must surface.
//
// The errnos are matched through the portable error interface, so the check
// behaves the same on Unix and on Windows without a platform-specific file.
func stagingHardLinkUnsupported(err error) bool {
	if err == nil {
		return false
	}
	// ENOSYS/ENOTSUP/EOPNOTSUPP: the filesystem has no hard links.
	if errors.Is(err, errors.ErrUnsupported) {
		return true
	}
	// EPERM: the filesystem refuses hard links (common on FAT/exFAT and on
	// Windows volumes mounted without link support).
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	// EMLINK: the link count of the staged inode is already at its maximum.
	if errors.Is(err, syscall.EMLINK) {
		return true
	}
	return false
}

func stagingClaimStateFromMeta(meta IncompleteMeta) *stagingClaimState {
	return &stagingClaimState{
		ranges:         meta.Ranges,
		fresh:          meta.Ranges == nil,
		legacyMigrated: meta.LegacyMigrated,
		proofPending:   meta.ProofPending,
	}
}

// claimRecordOurs re-reads the claim record and reports whether it still
// carries our process instance token (i.e. a racing take-over has not
// replaced it).
func claimRecordOurs(metaPath string) bool {
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return false
	}
	var m IncompleteMeta
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	return m.Instance != "" && m.Instance == processInstanceToken
}

// newClaimMeta builds the claim record for it/job: our PID and instance
// token, the expected logical size, and the resolved remote identity. Ranges
// are filled in by the caller once determined (fresh layout or legacy
// migration) and persisted with writeStagingMeta.
func newClaimMeta(it PlanItem, job Job, commit string) IncompleteMeta {
	return IncompleteMeta{
		PID:       os.Getpid(),
		Instance:  processInstanceToken,
		Host:      processHostname,
		StartedAt: time.Now().UTC(),
		Size:      it.Size,
		SHA256:    it.SHA256,
		Version:   stagingMetaVersion,
		Repo:      job.Repo,
		Commit:    commit,
		Path:      it.RelativePath,
	}
}

// stagingArtifactsAbsent reports whether neither the staging payload nor its
// record exists. It is a cleanup fast path: there is nothing to remove, so no
// ownership lock file needs to be created for a destination that never used the
// ranged path.
func stagingArtifactsAbsent(staging, metaPath string) bool {
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	if _, err := os.Stat(metaPath); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	if _, err := os.Stat(metaPath + stagingRecordTempSuffix); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	return true
}

// stagingPayloadSizeOK reports whether the staging payload exists and has
// exactly the expected logical size. Preallocation makes the length useless as
// completion evidence, but a missing or differently sized payload invalidates
// every committed range in the record: the bytes those ranges claim are not
// there. This check must run before any create/truncate of the staging path.
func stagingPayloadSizeOK(staging string, size int64) bool {
	fi, err := os.Stat(staging)
	return err == nil && fi.Size() == size
}

// stagingMetaResumable validates a staging record against the exact remote
// object and returns its normalized range layout. It is resumable only when
// version 2 or the current version, repo, path, size and resolved commit match,
// the blob SHA is
// present on both sides or neither (and equal when present), and every range
// carries a conservative completion marker. Version 2 completion is migrated
// by reserving its final byte for clean-framing revalidation.
func stagingMetaResumable(meta IncompleteMeta, it PlanItem, job Job, commit string) ([]RangeProgress, bool) {
	if (meta.Version != stagingMetaVersion && meta.Version != priorStagingMetaVersion) || meta.Size != it.Size || meta.Size <= 0 {
		return nil, false
	}
	if meta.Repo != job.Repo || meta.Path != it.RelativePath || meta.Commit != commit {
		return nil, false
	}
	// A blob SHA is an all-or-nothing identity claim. If either side has one,
	// both must be present and equal; a one-sided SHA cannot prove content
	// identity and must not authorize resuming old bytes.
	if (meta.SHA256 == "") != (it.SHA256 == "") {
		return nil, false
	}
	if meta.SHA256 != "" && !strings.EqualFold(meta.SHA256, it.SHA256) {
		return nil, false
	}
	ranges, ok := normalizedRangeLayout(meta.Ranges, meta.Size)
	if !ok {
		return nil, false
	}
	if meta.Version == priorStagingMetaVersion {
		// Version 2 could mark a range Done after every requested byte was
		// written even when response framing/EOF failed. Preserve its validated
		// prefix, but re-fetch each range's final byte before trusting completion.
		for i := range ranges {
			if ranges[i].Done {
				length := ranges[i].End - ranges[i].Start + 1
				ranges[i].Committed = length - 1
				ranges[i].Done = false
			}
		}
	}
	return ranges, true
}

// normalizedRangeLayout returns a normalized copy of ranges when they exactly
// tile [0, size), with in-range committed prefixes and no byte claimed past a
// range end. Done=true means the full range is committed after clean response
// framing. A full Committed prefix without Done is not framing proof and is
// reduced to length-1 so retry always requests a nonempty suffix.
//
// A record that cannot be normalized is not trustworthy resume data.
func normalizedRangeLayout(ranges []RangeProgress, size int64) ([]RangeProgress, bool) {
	out := append([]RangeProgress(nil), ranges...)
	if !normalizeRangesInPlace(out, size) {
		return nil, false
	}
	return out, true
}

// normalizeRangesInPlace validates and normalizes ranges in place. Callers
// that hold pointers into the slice (the range workers) rely on the in-place
// form; only the Done/Committed normalization is mutated.
func normalizeRangesInPlace(ranges []RangeProgress, size int64) bool {
	if len(ranges) == 0 || size <= 0 {
		return false
	}
	var off int64
	for i := range ranges {
		r := &ranges[i]
		if r.Start != off || r.End < r.Start || r.End >= size {
			return false
		}
		want := r.End - r.Start + 1
		if r.Done {
			r.Committed = want
		}
		if r.Committed < 0 || r.Committed > want {
			return false
		}
		if r.Committed == want && !r.Done {
			r.Committed = want - 1
		}
		off = r.End + 1
	}
	return off == size
}

// metaFreshWithin reports whether path's mtime is within d of now. A
// missing file is not fresh.
func metaFreshWithin(path string, d time.Duration) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) < d
}

// parseStaleTimeoutSetting resolves the configured stale timeout with the
// same defaults as Settings.BuildHFCache.
func parseStaleTimeoutSetting(cfg Settings) (time.Duration, error) {
	if cfg.StaleTimeout == "" {
		return DefaultStaleTimeout, nil
	}
	return time.ParseDuration(cfg.StaleTimeout)
}

// removeStagingArtifactsLocked removes the staging meta record and then the
// staging payload (best-effort; missing files are not an error). The caller
// holds the staging metadata gate, so no concurrent transition can recreate
// or replace the record between the two removals. Metadata is removed first so
// a crash between the two removals cannot leave a record claiming bytes whose
// payload no longer exists. The publish path is the deliberate exception: it
// renames the payload into place first, then removes the now-obsolete record.
func removeStagingArtifactsLocked(staging, metaPath string) {
	// The atomic record rewrite stages its JSON in metaPath+".tmp"; a crash
	// between create and rename leaves that transient artifact behind. It is
	// never a blob and must not survive cleanup.
	// lgtm[go/path-injection]
	if err := os.Remove(metaPath + stagingRecordTempSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: cleanup staging meta temp %s: %v", metaPath+stagingRecordTempSuffix, err)
	}
	// lgtm[go/path-injection]
	if err := os.Remove(metaPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Keep the payload if the record could not be removed: deleting the
		// payload first would leave a record claiming bytes that no longer
		// exist. The next claim's payload check discards such a record, but
		// preserving the ordering here avoids creating it.
		log.Printf("warning: cleanup staging meta %s (payload preserved): %v", metaPath, err)
		return
	}
	// lgtm[go/path-injection]
	if err := os.Remove(staging); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: cleanup staging file %s: %v", staging, err)
	}
}

// removeStagingArtifacts acquires the OS ownership lock (non-blocking) and
// the local metadata gate and removes a staging pair unconditionally. It is used
// only by the explicit-cancel cleanup helper after the caller has established
// that the claim is not (or is no longer) a live owner's; a busy ownership lock
// skips the removal. A cancel/hash-discard of our own claim uses
// removeStagingArtifactsOwned instead.
func removeStagingArtifacts(staging, metaPath string) {
	own, oerr := acquireStagingOwnershipLockWithin(context.Background(), staging, stagingLockCleanupWait)
	if errors.Is(oerr, errStagingOwnershipBusy) {
		log.Printf("warning: skip cleanup of staging file %s: a live owner holds the staging lock", staging)
		return
	}
	if oerr != nil {
		log.Printf("warning: skip cleanup of staging file %s: staging ownership lock unavailable: %v", staging, oerr)
		return
	}
	defer own.release()

	lock := &stagingStateLock{path: stagingLockPath(metaPath)}
	if err := lock.acquire(context.Background(), stagingLockCleanupWait); err != nil {
		log.Printf("warning: skip cleanup of staging file %s: local metadata gate unavailable: %v", staging, err)
		return
	}
	defer lock.release()
	removeStagingArtifactsLocked(staging, metaPath)
}

// readStagingMeta loads the staging record. It returns an error for a
// missing or unparseable record.
func readStagingMeta(metaPath string) (*IncompleteMeta, error) {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, err
	}
	var meta IncompleteMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// writeStagingMetaFile atomically rewrites the staging record (stage in the
// same directory, then rename) so a crash never leaves a partial record. The
// caller must hold the staging metadata gate (see staging_lock.go).
//
// The staged copy is written to a fresh inode at the sibling temp path, so a
// staged path left behind by an interrupted operation is never written
// through: it may still be a hard link to the authoritative record, and
// truncating it would empty the authoritative record before the rename
// replaced it.
func writeStagingMetaFile(metaPath string, meta *IncompleteMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp, err := stageStagingClaimRecord(metaPath, data)
	if err != nil {
		return err
	}
	if err := renameStagingClaimRecord(tmp, metaPath); err != nil {
		if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			log.Printf("warning: remove staged claim record %s: %v", tmp, rmErr)
		}
		return err
	}
	return nil
}

// buildRangeLayout partitions [0, size) into n contiguous ranges, matching
// the layout the legacy .parts.json resume used so a migration maps 1:1.
func buildRangeLayout(size int64, n int) []RangeProgress {
	if size <= 0 {
		return nil
	}
	if n < 1 || size/int64(n) <= 0 {
		n = 1
	}
	chunk := size / int64(n)
	ranges := make([]RangeProgress, n)
	for i := 0; i < n; i++ {
		start := int64(i) * chunk
		end := start + chunk - 1
		if i == n-1 {
			end = size - 1
		}
		ranges[i] = RangeProgress{Start: start, End: end}
	}
	return ranges
}

// migrateLegacyMultipart converts usable pre-#140 legacy state for dst into
// range progress over the already-open (and freshly truncated) staging file.
//
// Two legacy shapes are recognized, in priority order:
//
//  1. a valid .part-NN + .parts.json layout (the pre-#140 multipart state),
//     copied range-by-range; this wins whenever the family/layout exists;
//  2. a single dst.part sequential prefix (an interrupted single download or
//     an interrupted old assembly), copied as a committed prefix and tiled
//     across the first ranges at the configured worker count.
//
// If a valid .part-NN layout exists, dst.part is obsolete and is removed with
// the other legacy artifacts by the caller after the migrated state is
// durable. A layout whose every part is missing, empty or oversized
// contributes no bytes and is not usable: its family is discarded, then
// dst.part is migrated if present. If no usable multipart state exists,
// unvalidatable .part-NN files are discarded first (their byte-range contract
// is unknown), then dst.part is migrated if present. No usable legacy state
// returns errNoLegacyState. LegacyMigrated is reported only when actual legacy
// bytes were copied.
//
// The copy is context-bounded (contextReader checks ctx before every chunk),
// so an explicit cancel during a long migration stops promptly instead of
// waiting for a multi-gigabyte local copy.
//
// On success the legacy artifacts are deliberately NOT removed yet: the
// staging record does not describe the migrated bytes until the caller
// persists the returned layout, and removing the parts before that flush
// would leave the staging payload as the only copy of a valid partial under a
// record with no range layout — unvalidatable state the next run must discard.
// The caller removes the obsolete artifacts only after the migrated layout is
// durably persisted (issue #140, criterion 11: a valid legacy partial must
// survive the upgrade until its replacement is durable). The caller also
// records LegacyMigrated so the imported bytes are content-proven before
// publication when the item has a known SHA-256.
func migrateLegacyMultipart(ctx context.Context, dst string, out *os.File, size int64, workers int) ([]RangeProgress, bool, error) {
	data, err := os.ReadFile(multipartLayoutMetaPath(dst))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A real I/O failure must not silently discard legacy data.
		return nil, false, err
	}
	if err == nil {
		ranges, ok, merr := migrateLegacyParts(ctx, dst, out, size, data)
		if merr != nil {
			return nil, false, merr
		}
		if ok {
			return ranges, true, nil
		}
	}
	// No usable .part-NN/.parts.json layout. Any part files present have an
	// unknown byte-range contract and must not be reused; dst.part is a
	// sequential prefix and remains migratable below.
	_ = removeMultipartResumeFiles(dst)
	return migrateLegacySinglePart(ctx, dst, out, size, workers)
}

// validateLegacyPartsLayout parses a legacy .parts.json record and checks that
// it tiles [0, size) exactly with contiguous, indexed ranges. It is shared by
// the one-time migration and by the pre-routing resume-state detection so both
// accept exactly the same layouts.
func validateLegacyPartsLayout(data []byte, size int64) ([]multipartPartRange, bool) {
	var layout multipartResumeLayout
	if err := json.Unmarshal(data, &layout); err != nil {
		return nil, false
	}
	if layout.Size != size || len(layout.Parts) == 0 {
		return nil, false
	}
	var off int64
	for i, p := range layout.Parts {
		if p.Index != i || p.Start != off || p.End < p.Start || p.End >= size {
			return nil, false
		}
		off = p.End + 1
	}
	if off != size {
		return nil, false
	}
	return layout.Parts, true
}

// multipartResumeState describes usable pre-existing multipart state detected
// before an item is routed to the single-request path.
type multipartResumeState struct {
	// legacy reports a valid pre-#140 .part-NN/.parts.json layout with at
	// least one part contributing usable bytes.
	legacy bool
	// v2Staging is the staging payload path of a valid, identity-matching v2
	// pair, or empty when no such pair exists.
	v2Staging string
}

// hasUsableLegacyMultipartState reports whether dst has a valid pre-#140
// .part-NN/.parts.json layout with at least one part that contributes usable
// bytes. It is used before routing an item to the single-request path: routing
// a ranged-capable item with valid legacy state to a full single GET would
// ignore the partial and (after success) delete it, violating the upgrade
// contract. A layout whose every part is missing/empty/oversized is not
// "usable" and is left to the normal migration/discard path.
func hasUsableLegacyMultipartState(dst string, size int64) bool {
	if size <= 0 {
		return false
	}
	data, err := os.ReadFile(multipartLayoutMetaPath(dst))
	if err != nil {
		return false
	}
	parts, ok := validateLegacyPartsLayout(data, size)
	if !ok {
		return false
	}
	for i, p := range parts {
		fi, err := os.Stat(fmt.Sprintf("%s.part-%02d", dst, i))
		if err != nil {
			continue
		}
		want := p.End - p.Start + 1
		if fi.Size() > 0 && fi.Size() <= want {
			return true
		}
	}
	return false
}

// hasResumableStagingState reports whether a v2 staging pair carries a valid,
// identity-matching range layout and a payload at the expected logical size.
// Ownership/liveness is deliberately not part of this check: the multipart
// claim applies it and either resumes or backs off as owned. The detection only
// decides whether existing state should keep the multipart route.
func hasResumableStagingState(staging, metaPath string, it PlanItem, job Job, commit string) bool {
	meta, err := readStagingMeta(metaPath)
	if err != nil || meta == nil {
		return false
	}
	if _, ok := stagingMetaResumable(*meta, it, job, commit); !ok {
		return false
	}
	return stagingPayloadSizeOK(staging, it.Size)
}

// migrateLegacyParts validates a .parts.json layout and copies each part's
// valid prefix into the staging file. ok=false means the layout is absent,
// unvalidatable, or contributes no bytes at all (every part missing, empty or
// oversized); the caller then discards the part family and tries the
// single-part path. A layout that contributes nothing must not be reported as
// a usable migrated state, because the caller would delete a valid dst.part on
// the strength of an empty migration.
func migrateLegacyParts(ctx context.Context, dst string, out *os.File, size int64, data []byte) ([]RangeProgress, bool, error) {
	parts, ok := validateLegacyPartsLayout(data, size)
	if !ok {
		return nil, false, nil
	}
	ranges := make([]RangeProgress, len(parts))
	for i, p := range parts {
		ranges[i] = RangeProgress{Start: p.Start, End: p.End}
	}

	copiedAny := false
	for i := range ranges {
		partPath := fmt.Sprintf("%s.part-%02d", dst, i)
		fi, err := os.Stat(partPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // missing part: nothing committed for this range
			}
			return nil, false, err
		}
		want := ranges[i].End - ranges[i].Start + 1
		committed := fi.Size()
		if committed == 0 || committed > want {
			// Empty or oversize (stale/corrupt) part: its bytes are not
			// valid.
			continue
		}
		in, err := os.Open(partPath)
		if err != nil {
			return nil, false, err
		}
		_, cerr := io.CopyN(newBoundedAtWriter(out, ranges[i].Start, committed), contextReader{ctx: ctx, r: in}, committed)
		in.Close()
		if cerr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, false, ctxErr
			}
			return nil, false, cerr
		}
		ranges[i].Committed = committed
		ranges[i].Done = committed == want
		copiedAny = true
	}
	if !copiedAny {
		// The layout exists but no part contributed usable bytes: this is not
		// migrated state. The caller removes the unusable family and falls
		// back to dst.part instead of trusting a layout with zero committed
		// bytes (which would set LegacyMigrated without importing anything).
		return nil, false, nil
	}
	return ranges, true, nil
}

// migrateLegacySinglePart migrates a leftover sequential dst.part prefix. The
// file must be no larger than the expected size: the old single downloader
// truncated at the expected size and never wrote past it, so an oversized
// .part is stale/corrupt and is discarded rather than clamped to a prefix that
// nobody can vouch for ("discard stale/mismatched part files rather than
// guessing"). The prefix is tiled across the first ranges of the fresh worker
// layout, so the caller can persist one normal v2 record and resume with any
// future concurrency.
func migrateLegacySinglePart(ctx context.Context, dst string, out *os.File, size int64, workers int) ([]RangeProgress, bool, error) {
	partPath := dst + ".part"
	fi, err := os.Stat(partPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, errNoLegacyState
		}
		return nil, false, err
	}
	committed := fi.Size()
	if committed <= 0 {
		// An empty partial has no bytes worth preserving.
		_ = os.Remove(partPath)
		return nil, false, errNoLegacyState
	}
	if committed > size {
		// Oversized: not a prefix this destination can ever finish.
		_ = os.Remove(partPath)
		return nil, false, errNoLegacyState
	}
	ranges := buildRangeLayout(size, workers)
	if len(ranges) == 0 {
		return nil, false, errNoLegacyState
	}
	remaining := committed
	for i := range ranges {
		want := ranges[i].End - ranges[i].Start + 1
		switch {
		case remaining >= want:
			ranges[i].Committed = want
			ranges[i].Done = true
			remaining -= want
		case remaining > 0:
			ranges[i].Committed = remaining
			remaining = 0
		}
		if remaining == 0 {
			break
		}
	}

	in, err := os.Open(partPath)
	if err != nil {
		return nil, false, err
	}
	defer in.Close()
	if _, cerr := io.CopyN(newBoundedAtWriter(out, 0, committed), contextReader{ctx: ctx, r: in}, committed); cerr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		return nil, false, cerr
	}
	return ranges, true, nil
}

// parseContentRange parses a Content-Range header value strictly. Accepted
// forms: "bytes <start>-<end>/<total>" and "bytes <start>-<end>/*" (the
// unsatisfied-total form, which cannot confirm the expected size — the
// caller decides whether that is acceptable). Anything else is an error; no
// generic 206 is ever accepted without an exact match.
func parseContentRange(v string) (start, end, total int64, hasTotal bool, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(v), "bytes ")
	if !ok {
		return 0, 0, 0, false, fmt.Errorf("malformed Content-Range %q", v)
	}
	rangePart, sizePart, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, 0, 0, false, fmt.Errorf("malformed Content-Range %q", v)
	}
	startStr, endStr, ok := strings.Cut(rangePart, "-")
	if !ok {
		return 0, 0, 0, false, fmt.Errorf("malformed Content-Range %q", v)
	}
	start, err = strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("malformed Content-Range %q", v)
	}
	end, err = strconv.ParseInt(endStr, 10, 64)
	if err != nil || end < start {
		return 0, 0, 0, false, fmt.Errorf("malformed Content-Range %q", v)
	}
	if sizePart != "*" {
		total, err = strconv.ParseInt(sizePart, 10, 64)
		if err != nil || total <= end {
			return 0, 0, 0, false, fmt.Errorf("malformed Content-Range %q", v)
		}
		hasTotal = true
	}
	return start, end, total, hasTotal, nil
}

// validateRangeResponse enforces the strict ranged-response contract before
// any byte of the body may be written:
//
//   - the response must carry a Content-Range of the exact requested
//     [start, end] (both the "<total>" and "/*" forms match on the range;
//     only "/*" leaves the total unconfirmed),
//   - an advertised total must equal the expected logical size when both
//     are known,
//   - a parseable Content-Length must equal the requested byte count.
//
// The exact body byte count is enforced by the bounded writer at copy time: a
// short body is rejected after the copy, and an oversized body is refused as
// errRangedBodyOverflow. The latter is a protocol violation, not a completion:
// runRange never normalizes it to Done even when the requested prefix was
// already written, and a worker can never write before or after its assigned
// range.
func validateRangeResponse(rs *http.Response, wantStart, wantEnd, expectedSize int64) error {
	cr := rs.Header.Get("Content-Range")
	if cr == "" {
		return fmt.Errorf("ranged response missing Content-Range for bytes %d-%d", wantStart, wantEnd)
	}
	s, e, total, hasTotal, perr := parseContentRange(cr)
	if perr != nil {
		return perr
	}
	if s != wantStart || e != wantEnd {
		return fmt.Errorf("Content-Range %q does not match requested bytes %d-%d", cr, wantStart, wantEnd)
	}
	if hasTotal && expectedSize > 0 && total != expectedSize {
		return fmt.Errorf("Content-Range total %d does not match expected size %d", total, expectedSize)
	}
	if cl := rs.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n != wantEnd-wantStart+1 {
			return fmt.Errorf("Content-Length %d does not match requested range length %d", n, wantEnd-wantStart+1)
		}
	}
	return nil
}

// errRangedBodyOverflow marks a bounded-writer protocol violation: the
// response tried to write past the worker's assigned range. It is not a
// transport failure, so it must never be normalized into a completed range —
// even when every requested byte had already been accepted before the excess
// arrived. The attempt is retried only within the normal retry budget; a
// persistent violation fails the range and preserves the resumable staging
// state instead of publishing.
var errRangedBodyOverflow = errors.New("ranged body exceeds assigned range")

// boundedAtWriter writes into a file at a fixed base offset and refuses to
// write more than limit bytes, so a worker can never write before or after
// its assigned range no matter what the server sends. Each successful
// Write advances the position; written reports the bytes accepted. A would-be
// overflow returns errRangedBodyOverflow (wrapped with the counts) instead of
// silently truncating, so the caller can distinguish a protocol violation
// from an ordinary transport failure.
type boundedAtWriter struct {
	f       *os.File
	base    int64
	limit   int64
	written int64
	onWrite func(p []byte) // optional per-write hook (progress/hash feed)
}

func newBoundedAtWriter(f *os.File, base, limit int64) *boundedAtWriter {
	return &boundedAtWriter{f: f, base: base, limit: limit}
}

func (w *boundedAtWriter) Write(p []byte) (int, error) {
	// Compare against the remaining budget instead of computing
	// written+len(p), which could overflow for an unbounded limit.
	if int64(len(p)) > w.limit-w.written {
		return 0, fmt.Errorf("%w: wrote %d of %d bytes, got %d more", errRangedBodyOverflow, w.written, w.limit, len(p))
	}
	n, err := w.f.WriteAt(p, w.base+w.written)
	if n > 0 {
		w.written += int64(n)
		stagingWriteBytes.Add(int64(n))
		if w.onWrite != nil {
			w.onWrite(p[:n])
		}
	}
	if err != nil {
		return n, err
	}
	return n, nil
}

// stagingDownload is the shared state of one ranged multipart download.
type stagingDownload struct {
	ctx    context.Context
	httpc  *http.Client
	token  string
	job    Job
	cfg    Settings
	it     PlanItem
	dst    string
	stg    string // staging payload path
	pub    string // publish (rename) target
	meta   string // staging meta path
	emit   func(ProgressEvent)
	out    *os.File
	ranges []RangeProgress

	// commit is the resolved plan commit forming part of the resume identity.
	commit string

	// startedAt is this run's claim start (now). A resumed record's original
	// StartedAt is diagnostic only; it is deliberately not inherited so the
	// record always describes the current claim.
	startedAt time.Time

	// own is the OS-level ownership lock held for the whole download
	// interval. osExclusive reports that own
	// fences the interval, so publish requires it to still be held.
	own         *stagingOwnershipLock
	osExclusive bool

	// contentAddressed marks pub as blobs/<known-sha>, whose existence alone
	// proves completion. Publish must not overwrite it if another writer
	// (single path, legacy tmp-<sha>+store, another staging family) published
	// the blob while this transfer was in flight.
	contentAddressed bool

	// domain is the ownership domain of the blob key pub belongs to. It is
	// held for the whole download, so the publication's existence decision
	// and the placement that follows it cannot be overtaken by another
	// writer of the same key.
	domain *blobDomain

	// partCtx is the part-scoped cancellable context, stored so every
	// post-claim phase (migration, transfer, hash, publish) can apply the same
	// cancel-cleanup semantics.
	partCtx context.Context

	// legacyMigrated marks bytes imported from a pre-#140 partial. They have
	// layout/prefix structure but no validated provenance, so one full-file
	// content proof is required before publication when a digest is known.
	// The flag is persisted in the record so a crash/resume cannot skip it.
	legacyMigrated bool

	// proofPending is the durable known-digest publication gate. It is set
	// before any pre-publication proof starts and cleared only after the proof
	// succeeds (both transitions flushed to the staging record). A resumed
	// record carrying it forces the proof in every mode except Verify="none".
	proofPending bool

	// metaMu guards the range layout and every meta rewrite. The layout's
	// mutable fields (RangeProgress.Committed/Done) are owner-shared state:
	// the claim heartbeat normalizes them in place, and every range worker
	// advances its own range's committed prefix. They are therefore only
	// read or written while this gate is held.
	metaMu sync.Mutex

	// progress counts bytes written this run; emitted progress is
	// baseCommitted + progress, and never decreases within a run.
	progress atomic.Int64
	// baseCommitted is the committed total at claim time (excluded from
	// this-run speed accounting, consistent with the resumed/skipped-bytes
	// rule the single path already follows).
	baseCommitted int64

	// seqHash, when non-nil, is fed every staging write in order: strict
	// hash-on-write for the single fresh sequential transfer.
	//
	// It is owner-shared state: every range worker inspects it on each write
	// and may invalidate it when a response's framing cannot be trusted. It
	// is therefore only read or written while seqMu is held, and the hash
	// itself is only fed while seqMu is held, so the hash's internal state
	// and the nil/non-nil decision can never be observed inconsistently.
	//
	// It is only ever non-nil for a strictly sequential transfer (one task
	// covering the whole payload from offset 0), which also means only one
	// worker exists while it is live.
	seqHash hashWriter
	// seqMu guards seqHash: its nil/non-nil state, the hash's own bytes,
	// and the invalidation that switches the transfer over to the single
	// final full-file pass. It is never held across network I/O.
	seqMu sync.Mutex

	// One heartbeat goroutine per download keeps the claim record fresh
	// through every post-claim phase: transfers, retry/Retry-After waits, the
	// strict SHA pass, and finalization. It shares metaMu with range flushes,
	// so writeStagingMeta's temp+rename atomicity is never violated.
	heartbeatStop chan struct{}
	heartbeatDone chan struct{}
	heartbeatOnce sync.Once
	heartbeatErr  error
	heartbeatMu   sync.Mutex
}

// hashWriter is the subset of hash.Hash used for hash-on-write.
type hashWriter interface {
	io.Writer
	Sum([]byte) []byte
}

// flushMeta persists the current range state atomically.
func (d *stagingDownload) flushMeta() error {
	d.metaMu.Lock()
	defer d.metaMu.Unlock()
	return d.flushMetaLocked()
}

// flushMetaLocked persists the current range state; caller holds metaMu. The
// local per-path metadata gate is acquired inside, and the record is re-read
// and must still name this process instance before it is replaced: a flush or
// heartbeat can never race a take-over, overwrite the new owner's claim, or
// resurrect ownership this process has lost. A nil range layout (the window
// between claim creation and the initial migration/flush) writes an
// ownership-only record that claims no bytes.
func (d *stagingDownload) flushMetaLocked() error {
	meta := newClaimMeta(d.it, d.job, d.commit)
	meta.StartedAt = d.startedAt
	meta.LegacyMigrated = d.legacyMigrated
	meta.ProofPending = d.proofPending
	meta.LockFenced = d.osExclusive
	if d.ranges != nil {
		if !normalizeRangesInPlace(d.ranges, d.it.Size) {
			return fmt.Errorf("staging metadata flush: invalid range layout for %s", d.it.RelativePath)
		}
		meta.Ranges = d.ranges
	}
	lock := &stagingStateLock{path: stagingLockPath(d.meta)}
	// A checkpoint must still persist the durable high-water mark when the
	// user cancels; that is what makes the next run resumable. The lock
	// acquisition therefore does not derive from d.ctx — critical sections are
	// tiny, and the bounded wait prevents an unbounded stall.
	if err := lock.acquire(context.Background(), stagingLockWait); err != nil {
		return err
	}
	defer lock.release()
	return writeStagingMetaOwnedLocked(d.meta, &meta)
}

// startHeartbeat runs the single claim heartbeat for this download. It begins
// immediately after the claim is opened and covers every post-claim phase —
// the one-time legacy migration copy, transfers, retry waits, strict
// verification and finalization — so even a multi-gigabyte local migration
// cannot let the claim go stale. It is stopped before any removal of the meta
// record (including the publish path). A heartbeat write failure is recorded
// and cancels the part context: staying active under a stale record (or after
// a take-over) would invite a second writer to write the same staging file.
func (d *stagingDownload) startHeartbeat(interval time.Duration, cancel context.CancelCauseFunc) {
	if interval <= 0 {
		return
	}
	d.heartbeatStop = make(chan struct{})
	d.heartbeatDone = make(chan struct{})
	go func() {
		defer close(d.heartbeatDone)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-d.heartbeatStop:
				return
			case <-t.C:
				if err := d.flushMeta(); err != nil {
					d.heartbeatMu.Lock()
					if d.heartbeatErr == nil {
						d.heartbeatErr = err
					}
					d.heartbeatMu.Unlock()
					if cancel != nil {
						cancel(abortError(err))
					}
					return
				}
			}
		}
	}()
}

// stopHeartbeat stops and joins the heartbeat goroutine. It is idempotent and
// safe to call when no heartbeat was started. Removing the meta record before
// this returns could race a heartbeat that recreates it.
func (d *stagingDownload) stopHeartbeat() {
	d.heartbeatOnce.Do(func() {
		if d.heartbeatStop != nil {
			close(d.heartbeatStop)
		}
		if d.heartbeatDone != nil {
			<-d.heartbeatDone
		}
	})
}

// heartbeatError returns the first heartbeat write failure, if any.
func (d *stagingDownload) heartbeatError() error {
	d.heartbeatMu.Lock()
	defer d.heartbeatMu.Unlock()
	return d.heartbeatErr
}

// seqHashDigest returns the hex digest of the hash-on-write state and
// whether that state was usable. The decision and the digest are taken
// together under seqMu, so the digest can never be read from a hash that a
// worker invalidated concurrently, and the hash's internal state is never
// summed while another worker is feeding it.
//
// A false result means the caller must fall back to the single final
// full-file pass: the hash-on-write state is absent, or it was invalidated
// because a response's framing could not be trusted.
func (d *stagingDownload) seqHashDigest() (string, bool) {
	d.seqMu.Lock()
	defer d.seqMu.Unlock()
	if d.seqHash == nil {
		return "", false
	}
	return hex.EncodeToString(d.seqHash.Sum(nil)), true
}

// closeOut closes the staging handle exactly once so Windows can unlink the
// file before cancel cleanup or the publish rename.
// closeOut closes the staging handle exactly once so Windows can unlink the
// file before cancel cleanup or the publish rename. The returned error is a
// real finalization signal on the success path (a buffered write or close
// failure can lose bytes); best-effort cleanup paths discard it.
func (d *stagingDownload) closeOut() error {
	if d.out != nil {
		err := d.out.Close()
		d.out = nil
		return err
	}
	return nil
}

// requiresPrePublicationProof reports whether the current item, Verify mode
// and record state require the known-digest proof before publication:
//
//   - Verify=sha256 or Verify=etag with a known digest (fresh or resumed);
//   - Verify=size (or any other non-"none" value) with legacy-migrated bytes,
//     which have no validated provenance (the migration exception);
//   - any non-"none" mode whose record still carries ProofPending, so a crash
//     mid-proof, a failed mismatch cleanup, or a later mode change cannot
//     launder unproven all-Done ranges into a published blob.
//
// Verify="none" never requires it (the explicit no-hash opt-out). The
// single-request path is not a staging record: its .part is only renamed after
// the proof, so every restart sees inherited bytes and re-proves them in any
// non-"none" mode.
func (d *stagingDownload) requiresPrePublicationProof() bool {
	if d.it.SHA256 == "" || d.cfg.Verify == "none" {
		return false
	}
	return d.cfg.Verify == "sha256" || d.cfg.Verify == "etag" || d.legacyMigrated || d.proofPending
}

// poisonStagingLocked truncates the staging payload to zero so a later claim
// cannot trust the record's all-Done ranges even if the durable ProofPending
// flag is missing. The caller must hold the metadata gate AND have re-read
// the record as owned by this process; this prevents an
// stale writer from truncating another owner's payload.
// It is best-effort: failures are logged and the durable flag remains the
// primary guard.
func (d *stagingDownload) poisonStagingLocked() {
	_ = d.closeOut()
	if err := os.Truncate(d.stg, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: could not poison staging file %s after an unproven or failed known-digest proof: %v", d.stg, err)
	}
}

// poisonStagingIfOwned poisons the payload only while the claim is still owned
// under the metadata gate. The caller already holds the OS ownership lock, so no current-version
// successor can exist. If the metadata gate is unavailable,
// the poison is skipped: the durable ProofPending flag already prevents the
// unproven bytes from being published.
func (d *stagingDownload) poisonStagingIfOwned() {
	lock := &stagingStateLock{path: stagingLockPath(d.meta)}
	if err := lock.acquire(context.Background(), stagingLockCleanupWait); err != nil {
		log.Printf("warning: skip poisoning staging file %s: local metadata gate unavailable: %v", d.stg, err)
		return
	}
	defer lock.release()
	if !claimRecordOwnedLocked(d.meta) {
		return
	}
	d.poisonStagingLocked()
}

// clearProofPending clears the durable proof gate after a successful proof and
// flushes the record before publication. A failed flush restores the in-memory
// flag and fails finalization instead of publishing, leaving the record safe
// for a later re-proof.
func (d *stagingDownload) clearProofPending() error {
	d.metaMu.Lock()
	if !d.proofPending {
		d.metaMu.Unlock()
		return nil
	}
	d.proofPending = false
	d.metaMu.Unlock()
	if err := d.flushMeta(); err != nil {
		d.metaMu.Lock()
		d.proofPending = true
		d.metaMu.Unlock()
		return fmt.Errorf("record successful known-digest proof for %s: %w", d.it.RelativePath, err)
	}
	return nil
}

// applyCancelCleanup applies the caller's explicit-cancel cleanup policy after
// a post-claim phase observed a done part context. Internal aborts (a
// fail-fast sibling or a heartbeat/claim failure) preserve the resumable
// staging bytes. The ownership check and every removal happen inside one
// locked transition, so a take-over cannot interleave between the check and
// the deletions, and a writer that lost its claim never removes the new
// owner's data.
func (d *stagingDownload) applyCancelCleanup() {
	if d.partCtx == nil || d.partCtx.Err() == nil {
		return
	}
	if abortPartAttempt(d.partCtx) {
		return
	}
	d.stopHeartbeat()
	_ = d.closeOut()
	if d.cfg.CleanupPartialsOnCancel == nil || !d.cfg.CleanupPartialsOnCancel() {
		return
	}
	lock := &stagingStateLock{path: stagingLockPath(d.meta)}
	if err := lock.acquire(context.Background(), stagingLockCleanupWait); err != nil {
		log.Printf("warning: skip cancel cleanup of staging file %s: local metadata gate unavailable: %v", d.stg, err)
		return
	}
	defer lock.release()
	if !claimRecordOwnedLocked(d.meta) {
		return
	}
	cleanupLegacyPartials(d.dst)
	removeStagingArtifactsLocked(d.stg, d.meta)
}

// resetClaim discards a claim whose validated payload no longer matched at
// open time and installs a fresh, layout-less claim for the same object under
// the metadata gate. It exists because the claim's payload check and the
// open are separate filesystem operations: if the payload vanished or changed
// size in between, every committed range in the record is untrustworthy and
// must not be truncated in place under the old record.
//
// open may be nil (the payload vanished); the helper creates it. When open is
// non-nil the payload is truncated in place while the lock is held, so no
// contender can claim the path between the discard and the recreate.
func (d *stagingDownload) resetClaim(open *os.File) (*os.File, error) {
	lock := &stagingStateLock{path: stagingLockPath(d.meta)}
	if err := lock.acquire(d.ctx, stagingLockWait); err != nil {
		return nil, err
	}
	defer lock.release()
	if !claimRecordOwnedLocked(d.meta) {
		return nil, errStagingClaimLost
	}
	if err := os.Remove(d.meta); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	fresh := newClaimMeta(d.it, d.job, d.commit)
	fresh.LockFenced = d.osExclusive
	if _, retry, err := createStagingClaim(d.meta, fresh); err != nil {
		return nil, err
	} else if retry {
		return nil, fmt.Errorf("could not re-claim staging file %s", d.stg)
	}
	if open == nil {
		f, err := os.OpenFile(d.stg, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return nil, err
		}
		if err := f.Truncate(d.it.Size); err != nil {
			_ = f.Close()
			return nil, err
		}
		return f, nil
	}
	if err := open.Truncate(d.it.Size); err != nil {
		return nil, err
	}
	return open, nil
}

// retryWaitSleep waits out a retry/backoff pause. Claim freshness during the
// wait is maintained by the single heartbeat goroutine, not by rewriting the
// record from this loop; sleepCtx keeps pause/cancel/shutdown prompt.
//
// ok reports whether the full wait elapsed; false means ctx was done and the
// caller must treat the attempt as cancelled (as with sleepCtx).
func (d *stagingDownload) retryWaitSleep(ctx context.Context, wait time.Duration) (ok bool, err error) {
	return sleepCtx(ctx, wait), nil
}

// rangeTask is one unit of missing work: the uncommitted suffix of a range.
type rangeTask struct {
	idx  int
	from int64 // absolute resume offset (range Start + committed prefix)
}

// downloadMultipart downloads a file using multiple parallel range
// requests, writing every worker directly into one staging file at its
// assigned offset.
//
// opts selects the staging layout: blob-keyed staging for LFS files in HF
// cache mode (the staging file is the future blob, so finalization is
// rename-only), dst-adjacent staging otherwise. The returned bool reports
// whether a content proof against it.SHA256 already completed inside this
// call before the publish rename (hash-on-write, the single pre-publish
// pass, or the one-time legacy-migration proof), so the caller must not
// repeat it. A known digest is proven pre-publication in both Verify=sha256
// and Verify=etag modes.
//
// Reliability invariants preserved from the previous implementation:
// stall watchdog + resume from the durable current offset, bounded retry
// budget with progress refunds and the absolute attempt ceiling,
// Retry-After/RateLimit-aware waits, fail-fast 401/403/404 with
// partCancel(abortError(...)) cause propagation, job/part cancellation
// propagation, pause preserving resumable data, and the user-cancel
// cleanup callback distinction (internal aborts never delete resumable
// bytes).
func downloadMultipart(ctx context.Context, httpc *http.Client, token string, job Job, cfg Settings, it PlanItem, dst string, opts multipartOpts, emit func(ProgressEvent)) (bool, error) {
	// HEAD to resolve size, reissued only when the server throttles (429/503)
	// and bounded by the same attempt/backoff conventions as the transfer
	// loops: wait the larger of the server-requested Retry-After and the
	// local backoff (capped at DefaultMaxRetryAfter) with a context-aware
	// sleep so pause/cancel/shutdown stays prompt, then reissue the HEAD
	// within the configured retry budget.
	retry := newRetry(cfg)
	remaining := cfg.Retries
	if remaining < 0 {
		remaining = 0
	}
	attempt := 0
	for {
		req, _ := http.NewRequestWithContext(ctx, "HEAD", it.URL, nil)
		addAuth(req, token)
		resp, err := httpc.Do(req)
		if err != nil {
			return false, err
		}
		// A HEAD response carries headers only, so its body is closed
		// immediately instead of being held open (via defer) for the whole
		// parallel transfer. Status and headers stay readable after Close
		// for the classification and size resolution below.
		resp.Body.Close()

		apiErr := classifyFileResponse(resp, it.URL)

		// A permanent gated/private/missing response on HEAD fails the whole
		// job immediately with the actionable message, before any part
		// requests are issued.
		if apiErr != nil && apiErr.IsFailFast() {
			removeEmptyPartialFile(dst + ".part")
			return false, apiErr
		}

		// A throttling 429/503 must not be accepted as the resolved HEAD.
		// Every other status keeps the previous behavior (proceed and let
		// the range loop classify it).
		if resp.StatusCode != http.StatusTooManyRequests &&
			resp.StatusCode != http.StatusServiceUnavailable {
			if it.Size == 0 {
				if clen := resp.Header.Get("Content-Length"); clen != "" {
					var n int64
					fmt.Sscan(clen, &n)
					it.Size = n
				}
			}
			break
		}

		// Throttled: wait out the server-requested Retry-After (or the local
		// backoff, whichever is larger) and reissue the HEAD. When the retry
		// budget is exhausted, surface the transient error like an exhausted
		// transfer retry instead of silently proceeding with a throttled
		// response.
		if remaining == 0 {
			return false, apiErr
		}
		remaining--
		attempt++
		emit(ProgressEvent{Event: "retry", Path: it.RelativePath, Attempt: attempt, Message: apiErr.Error()})
		if d := NextRetryWait(resp, retry.Next(), DefaultMaxRetryAfter, time.Now()); !sleepCtx(ctx, d) {
			return false, ctx.Err()
		}
	}
	if it.Size == 0 {
		if opts.DirectBlobPublished != nil {
			*opts.DirectBlobPublished = false
		}
		return downloadSingle(ctx, httpc, token, job, cfg, it, dst, emit)
	}

	stg := opts.Staging
	if stg == "" {
		stg = dst + ".incomplete"
	}
	pub := opts.Publish
	if pub == "" {
		pub = dst
	}
	metaPath := stg + ".meta"

	// Claim the staging file exclusively: the in-process registry, the
	// OS-level ownership lock held for the whole interval, the local metadata
	// gate, and atomic complete-record installation. A live owner — including a suspended
	// one that stopped heartbeating — short-circuits the file to a skip in the
	// caller; only a content-addressed publish target is treated as already
	// complete.
	state, err := claimStaging(ctx, stg, pub, metaPath, it, job, opts.Commit, cfg, opts.ContentAddressed)
	if err != nil {
		return false, err
	}
	defer state.release()

	d := &stagingDownload{
		ctx:              ctx,
		httpc:            httpc,
		token:            token,
		job:              job,
		cfg:              cfg,
		it:               it,
		dst:              dst,
		stg:              stg,
		pub:              pub,
		meta:             metaPath,
		emit:             emit,
		commit:           opts.Commit,
		startedAt:        time.Now().UTC(),
		legacyMigrated:   state.legacyMigrated,
		proofPending:     state.proofPending,
		own:              state.own,
		osExclusive:      state.osExclusive,
		contentAddressed: opts.ContentAddressed,
		domain:           opts.Domain,
	}

	// Part-scoped cancellable context: a permanent (fail-fast) error on any
	// one range cancels the whole file so sibling workers stop promptly
	// instead of downloading/retrying/waiting until wg.Wait() returns. The
	// cancel cause is an abortCause wrapping the original error (see
	// abortError), which lets a sibling worker that exits via its ctx path
	// distinguish an internal abort from a user cancel/pause. Every post-claim
	// phase (migration, transfer, strict hash, publish) uses this same context.
	partCtx, partCancel := context.WithCancelCause(ctx)
	defer partCancel(nil)
	d.partCtx = partCtx

	// Open the staging payload. A resumed claim opens the payload the claim
	// already validated; a fresh claim creates/truncates it. S1: the payload
	// existence/size check happened in claimStaging BEFORE this create, so old
	// ranges are never trusted after a create+truncate. Re-check here against
	// a cleanup race: if the payload vanished or changed size, the record's
	// ranges are no longer trustworthy, so discard the record first (meta
	// before payload semantics; the payload is then truncated, not trusted) and
	// re-establish a fresh claim under the metadata gate.
	openFlags := os.O_RDWR
	if state.fresh {
		openFlags |= os.O_CREATE
	}
	out, err := os.OpenFile(stg, openFlags, 0o644)
	if err != nil && !state.fresh && errors.Is(err, os.ErrNotExist) {
		out, err = d.resetClaim(nil)
		if err == nil {
			state.fresh = true
			state.ranges = nil
			d.metaMu.Lock()
			d.ranges = nil
			d.legacyMigrated = false
			d.metaMu.Unlock()
		}
	}
	if err != nil {
		return false, err
	}
	if !state.fresh {
		fi, serr := out.Stat()
		if serr != nil || fi.Size() != it.Size {
			newOut, rerr := d.resetClaim(out)
			if rerr != nil {
				_ = out.Close()
				return false, rerr
			}
			out = newOut
			state.fresh = true
			state.ranges = nil
			d.metaMu.Lock()
			d.ranges = nil
			d.legacyMigrated = false
			d.metaMu.Unlock()
		}
	}
	d.out = out
	defer d.closeOut()
	if state.fresh {
		if err := out.Truncate(it.Size); err != nil {
			return false, err
		}
	} else {
		// Install the validated layout BEFORE the heartbeat starts: a
		// heartbeat tick must never rewrite the record with a nil layout,
		// which would discard resumable committed ranges on the next claim.
		d.metaMu.Lock()
		d.ranges = append([]RangeProgress(nil), state.ranges...)
		d.legacyMigrated = state.legacyMigrated
		d.startedAt = time.Now().UTC() // resumed run keeps its own clock
		d.metaMu.Unlock()
	}

	// One heartbeat for every post-claim phase: the one-time legacy migration
	// copy, transfers, retry/Retry-After waits, the strict SHA pass, and
	// finalization. It shares metaMu with range flushes (so the temp+rename
	// record write stays atomic) and the metadata gate with every other
	// record mutation. It is started BEFORE the migration so a long local copy
	// cannot let the claim go stale. Stops before any meta removal.
	d.startHeartbeat(stagingHeartbeatEvery, partCancel)
	defer d.stopHeartbeat()

	// Determine the authoritative range layout. The copy is context-bounded:
	// an explicit cancel during a multi-gigabyte legacy migration returns
	// promptly instead of after the whole local copy.
	legacyCleanup := false
	if state.fresh {
		// No valid v2 state: legacy .part-NN/.parts.json state or a single
		// .part sequential prefix may hold validated bytes; migrate once.
		ranges, migrated, merr := migrateLegacyMultipart(partCtx, dst, out, it.Size, cfg.Concurrency)
		switch {
		case merr == nil:
			d.metaMu.Lock()
			d.ranges = ranges
			// Migrated bytes have layout/prefix structure but no validated
			// provenance: persist the flag so the content proof below still
			// runs after a crash/resume. Set it in the same critical section
			// that publishes the ranges, so a heartbeat tick can never flush
			// all-Done migrated ranges without the durable gate.
			d.legacyMigrated = migrated
			if migrated && it.SHA256 != "" && cfg.Verify != "none" {
				d.proofPending = true
			}
			d.metaMu.Unlock()
			// The migrated layout replaces the legacy artifacts as the
			// resume authority — but only once it is durable. Until the
			// flush below succeeds, the legacy bytes remain the only
			// finishable copy of the partial (criterion 11).
			legacyCleanup = migrated
		case errors.Is(merr, errNoLegacyState):
			d.metaMu.Lock()
			d.ranges = buildRangeLayout(it.Size, cfg.Concurrency)
			d.metaMu.Unlock()
		default:
			// A real I/O failure must not silently discard legacy data; a
			// user cancel runs the shared cancel-cleanup policy.
			d.applyCancelCleanup()
			if cerr := partCtx.Err(); cerr != nil {
				if herr := d.heartbeatError(); herr != nil {
					return false, herr
				}
				return false, cerr
			}
			return false, merr
		}
	}
	// The legacy migration may have discovered migrated bytes that were not
	// visible at claim time. If they now require the proof, set the flag so the
	// layout flush below persists it before any transfer.
	d.metaMu.Lock()
	if d.requiresPrePublicationProof() {
		d.proofPending = true
	}
	proofPendingForFlush := d.proofPending
	// The committed baseline is read from the shared layout, so it is
	// derived under the same gate that guards the layout. The heartbeat
	// rewrites the layout concurrently; reading it unlocked would race
	// with the heartbeat's in-place normalization.
	d.baseCommitted = committedTotal(d.ranges)
	d.metaMu.Unlock()

	if err := d.flushMeta(); err != nil {
		d.applyCancelCleanup()
		if proofPendingForFlush {
			// The flag may not be durable; the zero-length payload is then the
			// durable marker that forces a discard/re-download next run.
			d.poisonStagingIfOwned()
		}
		if cerr := partCtx.Err(); cerr != nil {
			if herr := d.heartbeatError(); herr != nil {
				return false, herr
			}
			return false, cerr
		}
		return false, err
	}
	if legacyCleanup {
		// The migrated range layout is now durable in the staging record,
		// so the legacy artifacts are obsolete and can be removed. A
		// single dst.part loses to a valid .part-NN/layout or was itself the
		// migrated prefix; either way it is removed only after the flush.
		// A removal failure is surfaced (not hidden) and leaves the
		// artifacts for a later retry.
		if err := removeMultipartResumeFiles(dst); err != nil {
			return false, err
		}
		if err := removeLegacySinglePart(dst); err != nil {
			return false, err
		}
	}

	// Missing work: the uncommitted suffix of every incomplete range. The
	// persisted layout stays authoritative regardless of the configured
	// worker count, so a changed Concurrency re-fetches only missing
	// bytes and never invalidates committed ones. Completion is normalized
	// defensively here too: a complete-prefix range is never turned into a
	// bytes=End+1-End request.
	//
	// The layout is owner-shared state: the claim heartbeat normalizes the
	// same Committed/Done fields in place while this download derives its
	// work. The derivation therefore runs under the metadata gate, so the
	// heartbeat's normalization and this read-modify-write of the same
	// fields are serialized. The gate is released before any network work:
	// the derived task list is a private snapshot of the layout.
	d.metaMu.Lock()
	tasks := make([]rangeTask, 0, len(d.ranges))
	for i := range d.ranges {
		r := &d.ranges[i]
		want := r.End - r.Start + 1
		if r.Done || r.Committed >= want {
			r.Committed = want
			r.Done = true
			continue
		}
		tasks = append(tasks, rangeTask{idx: i, from: r.Start + r.Committed})
	}
	// The strict hash-on-write precondition reads the layout's bounds for
	// the single derived task; it is read under the same gate.
	seqHashEligible := cfg.Verify == "sha256" && it.LFS && it.SHA256 != "" &&
		len(tasks) == 1 && d.baseCommitted == 0 &&
		d.ranges[tasks[0].idx].Start == 0 && d.ranges[tasks[0].idx].End == it.Size-1
	d.metaMu.Unlock()

	// Strict hash-on-write is only possible when this run writes the whole
	// payload sequentially from offset 0: one task covering [0, size) with
	// no pre-committed bytes. Resumed or out-of-order transfers fall back
	// to the single pre-publish full-file pass below. Only Verify=sha256
	// feeds the hash on write; Verify=etag uses the same single final pass
	// (at most one full-file read either way).
	if seqHashEligible {
		d.seqMu.Lock()
		d.seqHash = sha256.New()
		d.seqMu.Unlock()
	}

	// partErr records the first permanent range error so the caller
	// surfaces the original error rather than a sibling's "context
	// canceled".
	var (
		partErrMu sync.Mutex
		partErr   error
	)
	setPartErr := func(err error) {
		partErrMu.Lock()
		if partErr == nil {
			partErr = err
		}
		partErrMu.Unlock()
		partCancel(abortError(err))
	}

	taskCh := make(chan rangeTask)
	workers := cfg.Concurrency
	if workers < 1 {
		// Download() normalizes the setting, but a direct caller must never
		// reach finalization with tasks queued and zero consumers: that would
		// publish a preallocated, incomplete staging file.
		workers = 1
	}
	if workers > len(tasks) {
		workers = len(tasks)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(tasks)+1)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskCh {
				if err := d.runRange(partCtx, task, setPartErr); err != nil {
					// Losing the claim is fatal for the whole file: stop the
					// siblings immediately instead of letting them keep writing
					// into a payload whose record now names another owner.
					if errors.Is(err, errStagingClaimLost) {
						setPartErr(err)
					}
					select {
					case errCh <- err:
					default:
					}
				}
			}
		}()
	}

	// Emit periodic progress from the in-memory counter: workers update it
	// atomically after successful writes, and the ticker only reads it —
	// no stat-scanning of any file. The ticker is stopped before the
	// finalization phase so it can never observe mid-publish state.
	tickerDone := make(chan struct{})
	var tickerWG sync.WaitGroup
	tickerWG.Add(1)
	go func() {
		defer tickerWG.Done()
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tickerDone:
				return
			case <-t.C:
				emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath,
					Downloaded: d.baseCommitted + d.progress.Load(), Total: it.Size})
			}
		}
	}()

	// Feed the workers; the channel closes after every task is enqueued,
	// or early when partCtx is done (the workers then drain and exit).
	go func() {
		for _, t := range tasks {
			select {
			case taskCh <- t:
			case <-partCtx.Done():
				close(taskCh)
				return
			}
		}
		close(taskCh)
	}()

	wg.Wait()
	close(tickerDone)
	tickerWG.Wait()

	// A permanent (fail-fast) range error cancels partCtx and is the error
	// the caller must see, in preference to a sibling's "context canceled".
	// An internal abort must NOT run cancel-cleanup (T3): the staging file
	// preserves the committed ranges for a later retry.
	partErrMu.Lock()
	permanentErr := partErr
	partErrMu.Unlock()
	if permanentErr != nil {
		return false, permanentErr
	}

	// A heartbeat write failure while this download is still active is fatal:
	// without a durable, fresh record the claim cannot be defended, and
	// staying active would invite a second writer to take over. Surface it
	// before the cancellation guard (the heartbeat itself cancels partCtx with
	// an abort cause, which must not be mistaken for a user cancel).
	if herr := d.heartbeatError(); herr != nil {
		return false, herr
	}

	// If the context was cancelled while workers were running (pause /
	// abort / timeout), return the cancellation error immediately. Workers
	// that exit via their ctx-aware retry/sleep path do NOT push to errCh,
	// so the ctx check must happen explicitly — and it is what prevents a
	// bogus "downloaded == total" progress emit and stops an incomplete
	// staging file from being published. Every attempt boundary already
	// flushed the committed high-water marks, so a pause preserves exactly
	// the durable resume state.
	if ctx.Err() != nil || partCtx.Err() != nil {
		// User cancel: stop the heartbeat, close the staging handle (Windows
		// cannot unlink an open file) and run the caller's cancel-cleanup
		// policy, all only while this process still owns the claim. An internal
		// abort preserves the resumable bytes.
		d.applyCancelCleanup()
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, partCtx.Err()
	}

	// Surface the first transfer failure (retry budget exhausted, terminal
	// protocol violation, ...). The staging file and its committed ranges
	// stay on disk for resume.
	select {
	case e := <-errCh:
		return false, e
	default:
	}

	// Everything past this point is metadata-only finalization work: the
	// optional strict hash pass and the atomic rename. Emit one explicit
	// full-progress reading first so the caller's last observed
	// file_progress value is the full byte count, then signal the
	// finalizing phase so the UI doesn't sit silently at 100%.
	emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath, Downloaded: it.Size, Total: it.Size})
	emit(ProgressEvent{Event: "file_finalizing", Path: it.RelativePath, Message: "finalizing"})

	// Finalization timing: network-complete (the last transferred byte) ->
	// file-done (the publish rename). Reset per call so a failure never reports
	// a stale reading; the opt-in benchmark reports this as its phase metric.
	finalizationNanos.Store(0)
	finalizeStart := time.Now()

	// Abort before finalizing if the job/part context was cancelled between
	// the guard above and here. An internal fail-fast abort preserves the
	// staging bytes; a user cancel runs the same cancel-cleanup as every other
	// post-claim phase.
	if cerr := partCtx.Err(); cerr != nil {
		// A heartbeat failure cancels partCtx with an abort cause; surface the
		// durable-record failure rather than its "context canceled" symptom.
		if herr := d.heartbeatError(); herr != nil {
			return false, herr
		}
		d.applyCancelCleanup()
		return false, cerr
	}

	// The claim must stay fresh through the finalization phase too; a
	// heartbeat failure since the guard above is fatal for the same reason.
	if herr := d.heartbeatError(); herr != nil {
		return false, herr
	}

	// Content proof before publication.
	//
	// The durable ProofPending flag was already set and flushed before the
	// transfer (and again after a legacy migration); it forces the proof here
	// regardless of the current mode, including size, whenever the item has a
	// known digest and Verify is not "none". This is why a crash mid-proof, a
	// mismatch whose cleanup failed, or a later
	// mode change cannot publish an all-Done but unproven record.
	//
	//   - Verify=sha256 or Verify=etag with a known digest: at most one full
	//     read, always before the rename/publish. With parallel direct writes
	//     the hash cannot be composed from range hashes, so a fresh sequential
	//     sha256 transfer may have fed the hash on write (eliminating the pass);
	//     every other case performs exactly one sequential pass here. This is
	//     why a known-digest mismatch can never reach the final destination or
	//     the content-addressed blob key: publication follows this gate.
	//   - Legacy-migrated bytes: layout/prefix structure only, no validated
	//     provenance. When the item carries a digest and Verify is not "none",
	//     force one full-file pass even in size mode (the documented one-time
	//     migration exception). The flag is persisted in the record, so a
	//     crash/resume repeats the proof before publication.
	//
	// No known digest means only the size/coverage contract applies here; a
	// non-LFS ETag fallback HEAD is a separate, historical post-publication
	// check performed by the caller.
	//
	// A mismatch keeps ProofPending set, poisons the payload, attempts the
	// owned discard, and never publishes corrupt bytes.
	needContentProof := d.requiresPrePublicationProof()
	contentVerified := false
	if needContentProof {
		sum := ""
		usedSeqHash := false
		if cfg.Verify == "sha256" && it.LFS {
			// The single sequential task fed the hash with every staging
			// write, in order, from offset 0. The digest and the decision to
			// use it are taken together, so a hash that a worker invalidated
			// (untrusted framing) is never summed.
			if digest, ok := d.seqHashDigest(); ok {
				sum = digest
				usedSeqHash = true
			}
		}
		if !usedSeqHash {
			// One sequential full-file pass over the completed staging file,
			// bounded by the part context so a user cancel, a heartbeat failure
			// or a sibling abort stops the pass promptly; computeSHA256Ctx
			// records it in the hash-pass counters.
			computed, herr := computeSHA256Ctx(partCtx, stg)
			if herr != nil {
				if heartbeatErr := d.heartbeatError(); heartbeatErr != nil {
					return false, heartbeatErr
				}
				// User cancel during the strict hash: the same stop-heartbeat /
				// close-handle / cleanup sequence as every other post-claim
				// phase. An internal abort keeps the resumable bytes; the
				// durable ProofPending flag keeps them untrusted.
				d.applyCancelCleanup()
				if cerr := ctx.Err(); cerr != nil {
					return false, cerr
				}
				if cerr := partCtx.Err(); cerr != nil {
					return false, cerr
				}
				return false, fmt.Errorf("sha256 verify: %w", herr)
			}
			sum = computed
		}
		if !strings.EqualFold(sum, it.SHA256) {
			// Corruption detected: never publish. ProofPending stays set; the
			// payload is poisoned to zero so a later run cannot trust the
			// all-Done ranges even if the record flag is lost, and the owned
			// discard is attempted. The durable flag plus the zero-length
			// payload make a failed discard safe: the next claim either
			// discards (size mismatch) or re-proves (flag) and refuses again.
			// The explicit redownload policy remains this actionable error plus
			// a fresh transfer on retry.
			d.stopHeartbeat()
			d.poisonStagingIfOwned()
			if !removeStagingArtifactsOwned(stg, metaPath) {
				log.Printf("warning: staging data for %s survived a known-digest mismatch; "+
					"its durable ProofPending flag and zero-length payload force the next run to re-prove or discard it",
					it.RelativePath)
			}
			if err := removeMultipartResumeFiles(dst); err != nil {
				log.Printf("warning: cleanup legacy multipart artifacts for %s after a known-digest mismatch: %v", dst, err)
			}
			if err := removeLegacySinglePart(dst); err != nil {
				log.Printf("warning: cleanup legacy single-part artifact for %s after a known-digest mismatch: %v", dst, err)
			}
			return false, fmt.Errorf("sha256 verify failed: %s: expected %s got %s", it.RelativePath, it.SHA256, sum)
		}
		// The proof succeeded: clear the durable gate before the publish
		// rename. A crash after this flush resumes with proven bytes; a crash
		// before it leaves the gate set and re-proves next run.
		if err := d.clearProofPending(); err != nil {
			return false, err
		}
		contentVerified = true
	}

	err = d.publish()
	if err == nil {
		finalizationNanos.Store(time.Since(finalizeStart).Nanoseconds())
	}
	return contentVerified, err
}

// publish atomically renames the staging file onto the destination and
// removes the resume metadata, then removes any obsolete legacy part
// artifacts for dst. The staging file and its publish target share a
// directory, so the normal path is rename-only — no payload copy. The rename
// replaces any pre-existing file at the target atomically: for a
// non-content-addressed destination the claim does not treat that file as
// complete (the caller's skip decision is authoritative), so the new bytes
// overwrite stale/incomplete ones in one step.
//
// Publication is claim-checked. The rename happens only while this process
// still owns the staging pair: the record is re-read under the metadata gate
// (the take-over serialization point) and the OS ownership lock must still be
// held for the same interval. A missing or foreign record returns
// errStagingClaimLost and leaves the staging payload untouched for its actual
// owner; nothing is renamed. This closes the window where a writer whose claim
// was taken over (or whose record was removed) could still publish a foreign
// staging file.
func (d *stagingDownload) publish() error {
	// A heartbeat failure while finalization is still active is fatal; never
	// rename a payload whose claim record could no longer be kept fresh.
	if herr := d.heartbeatError(); herr != nil {
		return herr
	}
	// Publication is gated on the durable known-digest proof. The finalization
	// path clears ProofPending only after a successful proof; reaching publish
	// with it still set means the proof was skipped or its clear was not made
	// durable. Refusing here leaves the record + payload for a later re-proof.
	if d.proofPending && d.it.SHA256 != "" && d.cfg.Verify != "none" {
		return fmt.Errorf("refusing to publish %s: durable known-digest proof is still pending", d.it.RelativePath)
	}
	// Stop the heartbeat before taking the metadata gate: the heartbeat
	// needs that same gate for every record flush, and it must not rewrite the
	// record after publication. Close before renaming so the unlink-on-rename
	// also works on Windows.
	d.stopHeartbeat()
	if err := d.closeOut(); err != nil {
		return fmt.Errorf("close staging file %s before publish: %w", d.stg, err)
	}

	lock := &stagingStateLock{path: stagingLockPath(d.meta)}
	if err := lock.acquire(d.ctx, stagingLockWait); err != nil {
		return err
	}
	defer lock.release()

	// Ownership re-read under the metadata gate. On an OS-lock platform the
	// lock itself must still be held: losing it would mean the interval this
	// process believed it owned was already handed to a successor.
	if d.osExclusive && !d.own.held() {
		return errStagingClaimLost
	}
	if !claimRecordOwnedLocked(d.meta) {
		return errStagingClaimLost
	}
	if err := d.ctx.Err(); err != nil {
		return err
	}
	if d.contentAddressed {
		// The target is a content-addressed blob: its existence is completion
		// proof and it is NEVER replaced. The placement is one no-replace
		// step, so a concurrent publisher of the same blob key cannot be
		// overtaken between the decision and the placement (a Stat-then-
		// rename would let the other writer's rename replace this blob,
		// leaving bytes that do not hash to the key they are stored under -
		// corruption CheckBlob would then report as complete). On a
		// filesystem with hard links the placement is atomic no-replace; on
		// one without, it is exclusive to the blob key's ownership domain
		// this download holds, so no domain participant can create the
		// target in between.
		if err := blobPublishPlace(d.stg, d.pub, d.domain); err != nil {
			if errors.Is(err, errBlobTargetExists) {
				// Another writer published this blob first. Its bytes are the
				// completion proof; discard this process's staging pair and
				// report the item as complete so the caller treats it as a
				// skip and creates its own links.
				removeStagingArtifactsLocked(d.stg, d.meta)
				return errBlobAlreadyComplete
			}
			return fmt.Errorf("publish %s: %w", d.pub, err)
		}
	} else {
		// An arbitrary destination is not completion proof: the caller's
		// authoritative pre-download skip decision already declined it, so the
		// new bytes overwrite stale/incomplete ones in one step.
		if err := os.Rename(d.stg, d.pub); err != nil {
			return err
		}
	}
	// The payload is now authoritative. Removing the obsolete record under the
	// same lock cannot race a successor: it would have had to observe this
	// claim first.
	removeStagingArtifactsLocked(d.stg, d.meta)
	// Obsolete artifacts from a pre-#140 version, if any linger next to
	// dst, must not survive a successful finalization. Finalization has
	// already succeeded, so a cleanup failure is logged and does not turn the
	// published file into an error.
	if err := removeMultipartResumeFiles(d.dst); err != nil {
		log.Printf("warning: cleanup legacy multipart artifacts for %s after publish: %v", d.dst, err)
	}
	if err := removeLegacySinglePart(d.dst); err != nil {
		log.Printf("warning: cleanup legacy single-part artifact for %s after publish: %v", d.dst, err)
	}
	return nil
}

// committedTotal sums the committed bytes of a range layout.
func committedTotal(ranges []RangeProgress) int64 {
	var n int64
	for i := range ranges {
		if ranges[i].Done {
			n += ranges[i].End - ranges[i].Start + 1
			continue
		}
		n += ranges[i].Committed
	}
	return n
}

// runRange downloads the uncommitted suffix of one range with the full
// per-attempt retry discipline: stall watchdog, high-water-mark refunds,
// the absolute attempt ceiling, Retry-After-aware waits, and fail-fast
// propagation. It writes only into [from, r.End] of the staging file via a
// bounded writer, strictly validates every ranged response, and advances
// the durable committed checkpoint only after bytes were successfully
// written — the checkpoint can never pass the bytes actually on disk.
//
// It returns nil when the range completed or the part context was
// cancelled (the aggregate ctx checks turn that into the cancellation the
// caller must see), and an error when the range terminally failed.
func (d *stagingDownload) runRange(partCtx context.Context, task rangeTask, setPartErr func(error)) error {
	r := &d.ranges[task.idx]
	commit0 := task.from - r.Start // durable prefix of this range at task start

	retry := newRetry(d.cfg)
	stall := stallTimeout(d.cfg)
	attempts := d.cfg.Retries
	if attempts < 0 {
		attempts = 0
	}
	remaining := attempts
	attempt := 0
	ceiling := absoluteAttemptCeiling(attempts)
	var lastErr error
	// pos is the bytes this run has added to the range's committed prefix;
	// hwm bounds refunds exactly as the legacy part loop did (see
	// downloadSingle for the two-term rationale).
	pos := int64(0)
	hwm := int64(0)
	// countedUpTo is the absolute end of the bytes this run has already
	// reported as progress for this range. It is initialized to the task's
	// durable start (bytes before it belong to baseCommitted) and advances
	// only when a write lands beyond it, so an overflowing attempt's accepted
	// prefix is not counted a second time when the retry rewrites it.
	countedUpTo := task.from

	for {
		select {
		case <-partCtx.Done():
			return nil
		default:
		}

		// Per-attempt context: the stall watchdog cancels only this
		// attempt, leaving sibling workers and the job untouched.
		attemptCtx, attemptCancel := context.WithCancel(partCtx)

		wantStart := task.from + pos
		wantEnd := r.End
		wantLen := wantEnd - wantStart + 1

		rq, _ := http.NewRequestWithContext(attemptCtx, "GET", d.it.URL, nil)
		addAuth(rq, d.token)
		rq.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", wantStart, wantEnd))

		preHwm := hwm
		var waitResp *http.Response
		taskErr := error(nil)

		rs, err := d.httpc.Do(rq)
		switch {
		case err != nil:
			taskErr = err
		case rs.StatusCode == http.StatusPartialContent:
			// Strict protocol validation BEFORE any body byte may be
			// written: exact Content-Range, expected total, and a
			// Content-Length matching the requested byte count. The
			// bounded writer enforces the exact body count during the
			// copy: a short body is rejected after the copy, and an
			// oversized body is refused as errRangedBodyOverflow. An
			// oversized response is a protocol violation, never a
			// completed range, even if it delivered every requested byte
			// before the excess (see the error branch below).
			if verr := validateRangeResponse(rs, wantStart, wantEnd, d.it.Size); verr != nil {
				taskErr = verr
				rs.Body.Close()
			} else {
				// The stall watchdog wraps the raw network body before the
				// speed limiter's pacing, so deliberate throttling is not
				// mistaken for a stalled peer.
				body := newStallReader(rs.Body, stall, attemptCancel)
				paced := d.cfg.SpeedLimiter.Reader(attemptCtx, body)
				w := newBoundedAtWriter(d.out, wantStart, wantLen)
				w.onWrite = func(p []byte) {
					// Progress counts durable, unique bytes: only the part of
					// this write beyond the highest absolute offset already
					// counted for the range. An overflowing attempt does not
					// advance pos, so its accepted prefix must remain counted
					// here; the retry rewrites the same range and adds nothing
					// until it passes countedUpTo.
					end := wantStart + w.written
					if end > countedUpTo {
						d.progress.Add(end - countedUpTo)
						countedUpTo = end
					}
					// The hash-on-write state is owner-shared: another worker
					// may invalidate it concurrently. The decision and the
					// hash's own bytes are taken together under seqMu, so a
					// byte is never fed into a hash that is being dropped,
					// and the hash's internal state is never mutated
					// concurrently with a read of its digest.
					d.seqMu.Lock()
					if d.seqHash != nil {
						_, _ = d.seqHash.Write(p)
					}
					d.seqMu.Unlock()
				}
				_, cerr := io.Copy(w, paced)
				rs.Body.Close()
				if cerr == nil && w.written != wantLen {
					cerr = fmt.Errorf("ranged body short: got %d of %d bytes for bytes %d-%d",
						w.written, wantLen, wantStart, wantEnd)
				}
				if cerr == nil {
					// Range complete: advance the durable checkpoint to the
					// full range before any later failure can lose it.
					d.metaMu.Lock()
					r.Committed = r.End - r.Start + 1
					r.Done = true
					ferr := d.flushMetaLocked()
					d.metaMu.Unlock()
					attemptCancel()
					if ferr != nil {
						return ferr
					}
					return nil
				}
				// A bounded-writer overflow is a protocol violation, not a
				// transport failure. Detect it from the raw copy error before
				// classifyStall can replace it with a concurrent stall.
				overflow := errors.Is(cerr, errRangedBodyOverflow)
				// A fired stall watchdog marks this attempt's failure as a
				// stall; a done partCtx keeps the genuine-cancel error.
				taskErr = classifyStall(partCtx, body, cerr)
				if !overflow {
					// pos advances by the bytes actually written, so the next
					// retry's Range request resumes from the durable offset and
					// the committed checkpoint never passes the written bytes.
					// Written bytes do not prove response framing: if the final
					// requested byte arrived but EOF/framing failed, keep that byte
					// uncommitted so retry requests a non-empty suffix.
					accepted := w.written
					if accepted == wantLen {
						accepted--
						// The hash consumed this byte, and the retry will feed it
						// again. Drop hash-on-write for one final full-file pass.
						// The invalidation is a shared-state write, so it
						// happens under seqMu like every other access to the
						// hash-on-write state.
						d.seqMu.Lock()
						d.seqHash = nil
						d.seqMu.Unlock()
					}
					pos += accepted
					d.metaMu.Lock()
					r.Committed = commit0 + pos
					r.Done = false
					ferr := d.flushMetaLocked()
					d.metaMu.Unlock()
					if ferr != nil {
						attemptCancel()
						return ferr
					}
				}
				// An oversized response is never completion, even when every
				// requested byte had already been accepted before the excess:
				// pos deliberately does not advance (a retry must re-request
				// the same suffix rather than plan bytes=End+1-End), the
				// checkpoint is never persisted as Done, and the normal retry
				// budget decides whether this attempt is retried. A persistent
				// violation fails the range with the staging state preserved.
				//
				// It also poisons the sequential hash-on-write: the retry
				// re-requests this same range and would feed its bytes into the
				// same hash a second time, so no valid retry could ever verify.
				// Drop it and fall back to exactly one final full-file pass.
				//
				// The invalidation is a shared-state write: parallel workers
				// can reach this branch concurrently, so it happens under
				// seqMu like every other access to the hash-on-write state.
				if overflow {
					d.seqMu.Lock()
					d.seqHash = nil
					d.seqMu.Unlock()
				}
			}
		default:
			// Any non-206 is a failure. A permanent 401/403/404 fails the
			// whole job immediately; a throttling 429/503 shapes the next
			// wait; any other status (including a 200 that ignored Range)
			// is a multipart protocol violation and is retried.
			rs.Body.Close()
			if apiErr := classifyFileResponse(rs, d.it.URL); apiErr != nil {
				taskErr = apiErr
				if apiErr.IsFailFast() || !apiErr.IsRetryable() {
					// Terminal for the whole file: cancel sibling workers
					// with the original error as the cause and record it for
					// the caller. No cleanup runs — the committed ranges are
					// resumable data.
					setPartErr(apiErr)
					attemptCancel()
					return apiErr
				}
				waitResp = rs
			} else {
				taskErr = fmt.Errorf("range not supported (status %s)", rs.Status)
			}
		}
		attemptCancel()
		lastErr = taskErr

		// Refund the retry budget only when this attempt moved STRICTLY
		// above the high-water mark captured BEFORE the attempt. preHwm is
		// read first because hwm is updated unconditionally below, so an
		// attempt's higher offset is recorded and can never later earn a
		// free refund from a bodyless failure.
		advanced := pos > preHwm
		if pos > hwm {
			hwm = pos
		}
		if advanced {
			retry.reset()
			remaining = attempts
		}

		if remaining == 0 || attempt >= ceiling {
			break
		}
		remaining--
		attempt++
		d.emit(ProgressEvent{Event: "retry", Path: d.it.RelativePath, Attempt: attempt, Message: lastErr.Error()})
		wait := NextRetryWait(waitResp, retry.Next(), DefaultMaxRetryAfter, time.Now())
		ok, werr := d.retryWaitSleep(partCtx, wait)
		if werr != nil {
			return werr
		}
		if !ok {
			return nil
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("range %d-%d failed", task.from, r.End)
	}
	return lastErr
}
