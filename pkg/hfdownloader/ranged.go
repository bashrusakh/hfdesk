// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Ranged downloads write workers directly into one staging payload. Resume
// records track committed ranges and are trusted only for a matching pinned
// identity and valid framing. The destination ownership domain fences claim,
// transfer, proof, publication, and cleanup; only version-4 state resumes.
// See doc.go for the complete ownership, resume, and publication contract.

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

// stagingMetaVersion is the only resumable record version.
const stagingMetaVersion = 4

// linkStagingClaimRecord atomically installs a fresh record without replacement.
var linkStagingClaimRecord = os.Link

// renameStagingClaimRecord installs a staged record over an existing one.
var renameStagingClaimRecord = os.Rename

const stagingRecordTempSuffix = ".tmp"

var (
	// errBlobIncompleteOwned means the staging destination is unavailable due
	// to a proven live owner or an unproven record. The latter also wraps
	// ErrStagingOwnershipUnproven.
	errBlobIncompleteOwned = errors.New("incomplete download is owned by another live writer")

	// errBlobAlreadyComplete means the known content-addressed target exists.
	errBlobAlreadyComplete = errors.New("destination already complete")

	// errStagingLayoutIncomplete means committed ranges do not cover the payload.
	errStagingLayoutIncomplete = errors.New("staging range layout is incomplete")

	// errStagingPayloadSize means a complete-looking layout has the wrong file size.
	errStagingPayloadSize = errors.New("staging payload size does not match the expected size")
)

// removeStagingArtifactsIfUnowned requires the caller's destination domain. It
// takes the metadata gate and preserves live or unproven records; cleanup is
// best-effort and skipped removals are logged.
func removeStagingArtifactsIfUnowned(staging, metaPath string) {
	if stagingArtifactsAbsent(staging, metaPath) {
		return
	}
	lock := &stagingStateLock{path: stagingLockPath(metaPath)}
	if err := lock.acquire(context.Background(), stagingLockCleanupWait); err != nil {
		log.Printf("warning: skip cleanup of staging file %s: local metadata gate unavailable: %v", staging, err)
		return
	}
	defer lock.release()

	meta, err := readStagingMeta(metaPath)
	switch {
	case err == nil:
		// A record written under the ownership-domain fence belongs to a
		// writer that cannot be alive while this caller holds the domain.
		// An unfenced record is not proven dead by that fact alone.
		if !meta.LockFenced && !stagingOwnerProvablyDead(meta) && !stagingOwnerIsSelf(meta) {
			log.Printf("warning: skip cleanup of staging file %s: a live claim remains", staging)
			return
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		// An unreadable record cannot establish that its writer is dead.
		log.Printf("warning: skip cleanup of staging file %s: ownership record is unreadable", staging)
		return
	}
	removeStagingArtifactsLocked(staging, metaPath)
}

// Observability counters used by tests and the opt-in local-I/O benchmark.
var (
	stagingWriteBytes atomic.Int64
	// hashPassCount and hashPassBytes count full-file verification passes.
	hashPassCount atomic.Int64
	hashPassBytes atomic.Int64
	// finalizationNanos measures the last successful transfer's finalization
	// through publication, excluding caller-side cache and job work.
	finalizationNanos atomic.Int64
)

type multipartOpts struct {
	// Staging is the payload path; empty defaults to dst+".incomplete".
	Staging string
	// Publish is the final path; empty defaults to dst.
	Publish string
	// Commit is the resolved commit, or the requested revision when unresolved.
	Commit string
	// CommitPinned is false when Commit is only the requested-revision fallback.
	CommitPinned bool
	// ContentAddressed makes an existing Publish target completion proof.
	ContentAddressed bool
	// DirectBlobPublished reports whether this call published directly to Publish.
	DirectBlobPublished *bool
	// Domain is required and held by the caller for this item's full operation;
	// the claim verifies it covers staging and takes no second lock.
	Domain *blobDomain
}

type stagingClaimState struct {
	// ranges is the resumed layout, or nil for a fresh claim.
	ranges []RangeProgress
	// fresh means no resumable range state existed.
	fresh bool
	// proofPending means known-digest proof is required before publication.
	proofPending bool
}

// claimStaging validates or creates resume state while the caller holds the
// destination domain. It fails closed if that domain does not cover staging;
// only normalized state for the matching pinned identity can be resumed.
func claimStaging(ctx context.Context, staging, publish, metaPath string, it PlanItem, job Job, commit string, commitPinned bool, cfg Settings, contentAddressed bool, domain *blobDomain) (state *stagingClaimState, err error) {
	if !domain.coversStaging(staging) {
		return nil, fmt.Errorf("refuse to claim staging %s: the held destination ownership domain does not cover it", staging)
	}
	// Persist the required proof gate before transfer; a resumed claim preserves
	// any pending proof even if the current mode would not otherwise require it.
	proofMode := it.SHA256 != "" && (cfg.Verify == "sha256" || cfg.Verify == "etag")

	for round := 0; round < 8; round++ {
		// Re-check content-addressed completion after domain acquisition. An arbitrary
		// target is not proof: the caller's skip decision already declined it.
		if contentAddressed {
			if _, err := os.Stat(publish); err == nil {
				return nil, errBlobAlreadyComplete
			}
		}

		// Local record-transition serialization. The destination domain
		// already excludes every other writer of this destination.
		lock := &stagingStateLock{path: stagingLockPath(metaPath)}
		if lerr := lock.acquire(ctx, stagingLockWait); lerr != nil {
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			if errors.Is(lerr, errStagingLockBusy) {
				return nil, errBlobIncompleteOwned
			}
			return nil, lerr
		}
		var retry bool
		var cerr error
		state, retry, cerr = claimStagingLocked(staging, metaPath, it, job, commit, commitPinned, proofMode)
		lock.release()
		if cerr != nil {
			return nil, cerr
		}
		if retry {
			continue
		}
		return state, nil
	}
	return nil, fmt.Errorf("could not claim staging file %s: claim did not converge", staging)
}

// claimStagingLocked evaluates or transitions state under both ownership gates.
// retry requests a fresh evaluation after unusable state or a create race.
func claimStagingLocked(staging, metaPath string, it PlanItem, job Job, commit string, commitPinned bool, proofMode bool) (*stagingClaimState, bool, error) {
	raw, rerr := os.ReadFile(metaPath)
	if errors.Is(rerr, os.ErrNotExist) {
		meta := newClaimMeta(it, job, commit, commitPinned)
		meta.ProofPending = proofMode
		meta.LockFenced = true
		return createStagingClaim(metaPath, meta)
	}
	if rerr != nil {
		// Preserve unreadable state and its cause: neither owner liveness nor
		// safe takeover can be established.
		return nil, false, fmt.Errorf("%w: read staging record %s: %w",
			newStagingUnprovenOwnerError(it, staging, metaPath), metaPath, rerr)
	}
	var meta IncompleteMeta
	if json.Unmarshal(raw, &meta) != nil {
		// Preserve malformed metadata: holding the domain does not establish who wrote it.
		return nil, false, newStagingUnprovenOwnerError(it, staging, metaPath)
	}

	// A free domain proves death only for records created under that fence.
	// Preserve unfenced records unless same-host death is independently proven.
	if !meta.LockFenced && !stagingOwnerProvablyDead(&meta) && !stagingOwnerIsSelf(&meta) {
		return nil, false, newStagingUnprovenOwnerError(it, staging, metaPath)
	}

	// After ownership is established, resume only current-version pinned matching
	// state with valid ranges and payload size; otherwise discard and retry.
	if meta.Version != stagingMetaVersion {
		downloadLog.Warn("discarding a staging record from an unsupported format; re-downloading from zero",
			"staging", staging,
			"record_version", meta.Version,
			"current_version", stagingMetaVersion,
		)
	}
	ranges, resumable := stagingMetaResumable(meta, it, job, commit, commitPinned)
	if !resumable || !stagingPayloadSizeOK(staging, it.Size) {
		removeStagingArtifactsLocked(staging, metaPath)
		return nil, true, nil
	}
	// Atomically replace the validated predecessor. Until the rename installs
	// the complete new record, the old resume authority remains intact; a crash
	// during temp creation cannot leave a new empty/partial claim at metaPath.
	take := newClaimMeta(it, job, commit, commitPinned)
	take.StartedAt = meta.StartedAt
	take.Ranges = ranges
	take.ProofPending = meta.ProofPending || proofMode
	// This claim is fenced by the held domain, regardless of its predecessor.
	take.LockFenced = true
	if err := writeStagingMetaFile(metaPath, &take); err != nil {
		return nil, false, err
	}
	if !claimRecordOurs(metaPath) {
		return nil, false, errStagingClaimLost
	}
	return stagingClaimStateFromMeta(take), false, nil
}

// createStagingClaim atomically installs a complete record without replacing a
// concurrent claim. It stages to a fresh sibling inode, then links or renames
// under the ownership gates. Never truncate leftover temp state: it may alias
// the authoritative record after an interrupted hard-link install.
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
		// A failed install leaves only non-authoritative temp state; remove it best-effort.
		if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			log.Printf("warning: remove staged claim record %s: %v", tmp, rmErr)
		}
		return nil, retry, err
	}
	// Drop the temporary alias after a hard-link install. A rename install has
	// already removed that name; future staging must always use a fresh inode.
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

// stageStagingClaimRecord writes a complete record to a fresh sibling temp.
// An existing temp may alias the authoritative record; unlink the name rather
// than truncating it. Non-regular residue is an error.
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

// clearStagingRecordTemp removes residue before fresh creation and rejects
// non-file entries other than symlinks.
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

// installStagedClaimRecord installs without replacing a predecessor. Rename
// fallback is allowed only when hard links are unsupported and both ownership
// gates exclude other mutators.
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
	// Rename fallback is safe only while no authoritative record exists.
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

// stagingHardLinkUnsupported identifies unsupported-link errors; other
// failures must not trigger the replacing-rename fallback.
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
	if errors.Is(err, syscall.EMLINK) {
		return true
	}
	return false
}

func stagingClaimStateFromMeta(meta IncompleteMeta) *stagingClaimState {
	return &stagingClaimState{
		ranges:       meta.Ranges,
		fresh:        meta.Ranges == nil,
		proofPending: meta.ProofPending,
	}
}

// claimRecordOurs checks the record's process-instance token.
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

// newClaimMeta builds this run's identity and diagnostics; callers add layout
// and lifecycle flags before installation. Unpinned identity cannot authorize resume.
func newClaimMeta(it PlanItem, job Job, commit string, commitPinned bool) IncompleteMeta {
	return IncompleteMeta{
		PID:          os.Getpid(),
		Instance:     processInstanceToken,
		Host:         processHostname,
		StartedAt:    time.Now().UTC(),
		Size:         it.Size,
		SHA256:       it.SHA256,
		Version:      stagingMetaVersion,
		Repo:         job.Repo,
		Commit:       commit,
		CommitPinned: commitPinned,
		Path:         it.RelativePath,
	}
}

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

// stagingPayloadSizeOK checks that the staging payload exists at the expected
// size. Preallocation size is not completion evidence, but mismatch invalidates
// committed ranges; check before any create or truncate.
func stagingPayloadSizeOK(staging string, size int64) bool {
	fi, err := os.Stat(staging)
	return err == nil && fi.Size() == size
}

// checkStagingPayloadSize verifies the closed payload length before publication.
// On mismatch it returns an error and preserves the partial for retry.
func checkStagingPayloadSize(staging string, size int64) error {
	fi, err := os.Stat(staging)
	if err != nil {
		return fmt.Errorf("refusing to publish: the staging payload %s cannot be read: %w", staging, err)
	}
	if fi.Size() != size {
		return fmt.Errorf("refusing to publish: the staging payload %s holds %d bytes, expected %d; the staged bytes are preserved for a resume: %w",
			staging, fi.Size(), size, errStagingPayloadSize)
	}
	return nil
}

// stagingMetaResumable checks version, shared pinned identity, digest
// consistency, and normalized ranges before authorizing resume.
func stagingMetaResumable(meta IncompleteMeta, it PlanItem, job Job, commit string, commitPinned bool) ([]RangeProgress, bool) {
	if meta.Version != stagingMetaVersion || meta.Size <= 0 {
		return nil, false
	}
	if !meta.identity().matches(resumeIdentity{
		Repo:   job.Repo,
		Commit: commit,
		Path:   it.RelativePath,
		Size:   it.Size,
		Pinned: commitPinned,
	}) {
		return nil, false
	}
	// Digest identity must be present on both sides or neither, and equal when present.
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
	return ranges, true
}

// normalizedRangeLayout validates an exact tiling and committed prefixes.
// A full prefix without Done lacks framing proof, so it reserves the final byte.
func normalizedRangeLayout(ranges []RangeProgress, size int64) ([]RangeProgress, bool) {
	out := append([]RangeProgress(nil), ranges...)
	if !normalizeRangesInPlace(out, size) {
		return nil, false
	}
	return out, true
}

// stagingLayoutComplete checks that all normalized ranges are Done and total
// the payload size. It shares resume validation and does not mutate the input.
func stagingLayoutComplete(ranges []RangeProgress, size int64) bool {
	layout, ok := normalizedRangeLayout(ranges, size)
	if !ok {
		return false
	}
	for i := range layout {
		if !layout[i].Done {
			return false
		}
	}
	return committedTotal(layout) == size
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

// removeStagingArtifactsLocked requires the destination domain and metadata
// gate. Remove metadata before payload so a crash cannot leave trusted ranges
// over missing bytes. Publication deliberately places the payload first.
func removeStagingArtifactsLocked(staging, metaPath string) {
	// Remove any interrupted atomic-record temp as well.
	// lgtm[go/path-injection]
	if err := os.Remove(metaPath + stagingRecordTempSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: cleanup staging meta temp %s: %v", metaPath+stagingRecordTempSuffix, err)
	}
	// lgtm[go/path-injection]
	if err := os.Remove(metaPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Preserve the payload if its record could not be removed.
		log.Printf("warning: cleanup staging meta %s (payload preserved): %v", metaPath, err)
		return
	}
	// lgtm[go/path-injection]
	if err := os.Remove(staging); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: cleanup staging file %s: %v", staging, err)
	}
}

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

// writeStagingMetaFile requires the metadata gate and atomically replaces the
// record from a fresh sibling inode, never truncating a leftover hard-link alias.
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

// buildRangeLayout partitions [0, size) into n contiguous ranges. The layout
// is persisted as the authoritative work contract, so a later run with a
// different worker count re-fetches only the missing bytes.
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

// hasResumableStagingState checks pinned identity, version, ranges, and payload
// size to retain multipart routing. It does not establish ownership; the claim
// performs that check under the destination domain.
func hasResumableStagingState(staging, metaPath string, it PlanItem, job Job, commit string, commitPinned bool) bool {
	meta, err := readStagingMeta(metaPath)
	if err != nil || meta == nil {
		return false
	}
	if _, ok := stagingMetaResumable(*meta, it, job, commit, commitPinned); !ok {
		return false
	}
	return stagingPayloadSizeOK(staging, it.Size)
}

// parseContentRange accepts "bytes <start>-<end>/<total>" and the unknown-total
// form "bytes <start>-<end>/*"; callers validate them against the requested range.
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

// validateRangeResponse checks exact inclusive bounds, any advertised total
// against the known size, and any parseable Content-Length against the request.
// The bounded writer and copy result separately enforce body length; overflow
// is a protocol error, never framing-complete coverage.
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

// errRangedBodyOverflow is a protocol violation, even after the requested prefix
// was written. It must never mark a range Done.
var errRangedBodyOverflow = errors.New("ranged body exceeds assigned range")

// boundedAtWriter confines writes to its assigned range and returns
// errRangedBodyOverflow rather than silently truncating excess data.
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

type stagingDownload struct {
	ctx    context.Context
	httpc  *http.Client
	token  string
	job    Job
	cfg    Settings
	it     PlanItem
	dst    string
	stg    string // staging payload path
	pub    string // publication target
	meta   string // staging metadata path
	emit   func(ProgressEvent)
	out    *os.File
	ranges []RangeProgress

	commit string

	// commitPinned is false for requested-revision fallback identities; their
	// staged bytes are never inherited.
	commitPinned bool

	// startedAt describes this run's claim, not the predecessor's diagnostic clock.
	startedAt time.Time

	// contentAddressed makes pub a known-digest blob whose existence proves
	// completion; publication uses the blob no-replace policy.
	contentAddressed bool

	// domain fences this item's claim, transfer, publication, and cancel cleanup.
	// It excludes other domain participants; the claim takes no second domain.
	domain *blobDomain

	// partCtx is the part-scoped cancellable context, stored so every
	// post-claim phase (transfer, hash, publish) can apply the same
	// cancel-cleanup semantics.
	partCtx context.Context

	// proofPending is persisted before proof and cleared durably after success;
	// a resumed known-digest record forces re-proof unless Verify is "none".
	proofPending bool

	// metaMu guards metadata rewrites and range Committed/Done fields, including
	// in-place normalization during checkpoints.
	metaMu sync.Mutex

	// progress counts unique bytes written this run; emitted progress is
	// baseCommitted + progress and never decreases within a run.
	progress atomic.Int64
	// baseCommitted is the committed total at claim time (excluded from
	// this-run speed accounting, consistent with the resumed/skipped-bytes
	// rule the single path already follows).
	baseCommitted int64

	// seqHash hashes writes only for a fresh whole-file sequential task. Untrusted
	// framing invalidates it, requiring a final full-file pass. Access requires seqMu.
	seqHash hashWriter
	// seqMu guards seqHash: its nil/non-nil state, the hash's own bytes,
	// and the invalidation that switches the transfer over to the single
	// final full-file pass. It is never held across network I/O.
	seqMu sync.Mutex
}

type hashWriter interface {
	io.Writer
	Sum([]byte) []byte
}

// fenced reports whether the destination domain and its OS lock remain held;
// publication requires that fence.
func (d *stagingDownload) fenced() bool {
	return d.domain.lockHeld()
}

func (d *stagingDownload) flushMeta() error {
	d.metaMu.Lock()
	defer d.metaMu.Unlock()
	return d.flushMetaLocked()
}

// flushMetaLocked requires metaMu, then takes the metadata gate and rechecks
// the process-instance token before replacement. A nil layout claims no bytes.
func (d *stagingDownload) flushMetaLocked() error {
	meta := newClaimMeta(d.it, d.job, d.commit, d.commitPinned)
	meta.StartedAt = d.startedAt
	meta.ProofPending = d.proofPending
	meta.LockFenced = d.fenced()
	if d.ranges != nil {
		if !normalizeRangesInPlace(d.ranges, d.it.Size) {
			return fmt.Errorf("staging metadata flush: invalid range layout for %s", d.it.RelativePath)
		}
		meta.Ranges = d.ranges
	}
	lock := &stagingStateLock{path: stagingLockPath(d.meta)}
	// Persist the checkpoint even after cancellation; use a bounded metadata-gate
	// wait independent of d.ctx so the next run can resume committed bytes.
	if err := lock.acquire(context.Background(), stagingLockWait); err != nil {
		return err
	}
	defer lock.release()
	return writeStagingMetaOwnedLocked(d.meta, &meta)
}

// seqHashDigest reads digest and usability together under seqMu. False requires
// the final full-file pass, including after framing invalidated hash-on-write.
func (d *stagingDownload) seqHashDigest() (string, bool) {
	d.seqMu.Lock()
	defer d.seqMu.Unlock()
	if d.seqHash == nil {
		return "", false
	}
	return hex.EncodeToString(d.seqHash.Sum(nil)), true
}

// closeOut closes the handle once so Windows can rename or remove staging.
// Publication propagates close errors; best-effort cleanup ignores them.
func (d *stagingDownload) closeOut() error {
	if d.out != nil {
		err := d.out.Close()
		d.out = nil
		return err
	}
	return nil
}

// requiresPrePublicationProof checks a known digest for sha256/etag, pending
// proof, or unpinned identity; Verify="none" explicitly opts out.
func (d *stagingDownload) requiresPrePublicationProof() bool {
	if d.it.SHA256 == "" || d.cfg.Verify == "none" {
		return false
	}
	if !d.commitPinned {
		// An unresolved revision is not immutable; prove its known digest even in size
		// mode. Pinned size-mode transfers retain the no-reread normal path.
		return true
	}
	return d.cfg.Verify == "sha256" || d.cfg.Verify == "etag" || d.proofPending
}

// poisonStagingLocked requires the domain, metadata gate, and an owned-record
// recheck. Zeroing invalidates all-Done coverage if the pending flag is lost;
// failure is best-effort because ProofPending remains the primary guard.
func (d *stagingDownload) poisonStagingLocked() {
	_ = d.closeOut()
	if err := os.Truncate(d.stg, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: could not poison staging file %s after an unproven or failed known-digest proof: %v", d.stg, err)
	}
}

// poisonStagingIfOwned requires the held domain and rechecks ownership under
// the metadata gate. If that gate is unavailable, ProofPending still blocks
// unchecked publication (except for Verify="none").
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

// applyCancelCleanup preserves internal-abort state. Explicit cancel follows
// the caller's policy, checking ownership and removing artifacts in one
// metadata-gated transition while the destination domain remains held.
func (d *stagingDownload) applyCancelCleanup() {
	if d.partCtx == nil || d.partCtx.Err() == nil {
		return
	}
	if abortPartAttempt(d.partCtx) {
		return
	}
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
	// Only the obsolete pre-#140 multipart family is discarded here. The
	// single-route <dst>.part and its sidecar are NOT this route's artifacts:
	// the multipart route creates no .part, so that route owns their cleanup.
	if err := removeMultipartResumeFiles(d.dst); err != nil {
		downloadLog.Warn("cleanup obsolete multipart artifacts failed", "path", d.dst, "error", err)
	}
	removeStagingArtifactsLocked(d.stg, d.meta)
}

// resetClaim replaces metadata before resetting a payload that changed between
// claim validation and open. The caller holds the domain; this helper takes the
// metadata gate. nil creates a handle, otherwise the supplied handle is truncated.
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
	fresh := newClaimMeta(d.it, d.job, d.commit, d.commitPinned)
	fresh.LockFenced = d.fenced()
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

// retryWaitSleep waits cancellably without rewriting durable checkpoints:
// the held domain, not record age, proves ownership. False means ctx ended.
func (d *stagingDownload) retryWaitSleep(ctx context.Context, wait time.Duration) (ok bool, err error) {
	return sleepCtx(ctx, wait), nil
}

type rangeTask struct {
	idx  int
	from int64 // absolute resume offset (range Start + committed prefix)
}

// downloadMultipart writes ranges directly into one staging payload, blob-keyed
// in LFS cache mode and dst-adjacent otherwise. The bool reports known-digest
// proof completed here before publication, so the caller must not repeat it.
// Retry, cancellation, and fail-fast contracts are in doc.go.
func downloadMultipart(ctx context.Context, httpc *http.Client, token string, job Job, cfg Settings, it PlanItem, dst string, opts multipartOpts, emit func(ProgressEvent)) (bool, error) {
	// Resolve size with HEAD; retry only 429/503 within the configured budget,
	// sleeping cancellably for the larger server-requested or local backoff
	// (capped at DefaultMaxRetryAfter).
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
		// Close the header-only response before the potentially long transfer.
		resp.Body.Close()

		apiErr := classifyFileResponse(resp, it.URL)

		// A permanent HEAD failure creates no multipart payload. The held destination
		// domain does not make the single route's .part/sidecar ours to remove: preserve
		// that route's resume state, including bytes left by an earlier run.
		if apiErr != nil && apiErr.IsFailFast() {
			return false, apiErr
		}

		// Non-throttling HEAD statuses defer final classification to the range request.
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

		if remaining == 0 {
			return false, apiErr
		}
		remaining--
		attempt++
		emit(ProgressEvent{Event: "retry", Path: it.RelativePath, Attempt: attempt, Message: apiErr.Error()})
		downloadLog.Warn("download retry",
			"repo", job.Repo,
			"path", it.RelativePath,
			"error", apiErr,
		)
		if d := NextRetryWait(resp, retry.Next(), DefaultMaxRetryAfter, time.Now()); !sleepCtx(ctx, d) {
			return false, ctx.Err()
		}
	}
	if it.Size == 0 {
		if opts.DirectBlobPublished != nil {
			*opts.DirectBlobPublished = false
		}
		// Unknown size requires the single path. Pass the already-held domain rather
		// than locking again; its sidecar still governs any inherited bytes.
		return downloadSingle(ctx, httpc, token, job, cfg, it, dst,
			singleOpts{Commit: opts.Commit, CommitPinned: opts.CommitPinned, Domain: opts.Domain}, emit)
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

	// Claim under the item's held domain and local metadata gate; only a known
	// content-addressed publication target is completion proof.
	state, err := claimStaging(ctx, stg, pub, metaPath, it, job, opts.Commit, opts.CommitPinned, cfg, opts.ContentAddressed, opts.Domain)
	if err != nil {
		return false, err
	}

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
		commitPinned:     opts.CommitPinned,
		startedAt:        time.Now().UTC(),
		proofPending:     state.proofPending,
		contentAddressed: opts.ContentAddressed,
		domain:           opts.Domain,
	}

	// A permanent range error cancels siblings with an abortCause, preserving the
	// original error and distinguishing internal abort from user cancel/pause.
	// Transfer, proof, and publication share this post-claim context.
	partCtx, partCancel := context.WithCancelCause(ctx)
	defer partCancel(nil)
	d.partCtx = partCtx

	// Resume only the payload validated before any create/truncate. Recheck after
	// open; if it vanished or resized, reset metadata before trusting any ranges.
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
		// Install the validated layout before the initial flush, so the first
		// persisted record carries the resumable committed ranges instead of
		// a nil layout that would discard them on the next claim.
		d.metaMu.Lock()
		d.ranges = append([]RangeProgress(nil), state.ranges...)
		d.startedAt = time.Now().UTC()
		d.metaMu.Unlock()
	}

	// Fresh claims start from zero. Discard obsolete .part-NN/.parts.json while
	// holding the domain; never import them or touch the single route's .part/sidecar.
	if state.fresh {
		d.metaMu.Lock()
		d.ranges = buildRangeLayout(it.Size, cfg.Concurrency)
		d.metaMu.Unlock()

		var obsolete []string
		if files, ferr := multipartPartFiles(dst); ferr == nil {
			obsolete = files
		}
		if _, lerr := os.Stat(multipartLayoutMetaPath(dst)); lerr == nil {
			obsolete = append(obsolete, multipartLayoutMetaPath(dst))
		}
		if len(obsolete) > 0 {
			downloadLog.Warn("discarding obsolete multipart artifacts from an older format; re-downloading from zero",
				"path", dst,
				"artifacts", len(obsolete),
			)
		}
		if err := removeMultipartResumeFiles(dst); err != nil {
			// The obsolete family is ignored resume state, not a reason to
			// fail the transfer; it is left for the existing success/cleanup
			// path.
			downloadLog.Warn("cleanup obsolete multipart artifacts failed", "path", dst, "error", err)
		}
	}
	d.metaMu.Lock()
	if d.requiresPrePublicationProof() {
		d.proofPending = true
	}
	proofPendingForFlush := d.proofPending
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
			return false, cerr
		}
		return false, err
	}

	// Derive missing suffixes under metaMu: checkpoint normalization mutates the
	// same layout. Persisted partitions survive Concurrency changes; full prefixes
	// without framing proof reserve the last byte. Release the gate before network I/O.
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
	seqHashEligible := cfg.Verify == "sha256" && it.LFS && it.SHA256 != "" &&
		len(tasks) == 1 && d.baseCommitted == 0 &&
		d.ranges[tasks[0].idx].Start == 0 && d.ranges[tasks[0].idx].End == it.Size-1
	d.metaMu.Unlock()

	// Hash-on-write is limited to sha256 LFS with one whole-file task and no
	// committed baseline. Resumed/out-of-order transfers and etag use the final pass.
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
		// Direct callers also need at least one worker; queued work must not reach
		// publication merely because no consumer ran.
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

	// Report the atomic in-memory count; join the ticker before finalization so
	// it cannot observe publication in progress.
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

	// Return the original permanent error, not a sibling's cancellation. Internal
	// abort preserves committed staging ranges rather than running user cleanup.
	partErrMu.Lock()
	permanentErr := partErr
	partErrMu.Unlock()
	if permanentErr != nil {
		return false, permanentErr
	}

	// Workers can return nil on cancellation without sending errCh. Check context
	// before terminal progress or publication; attempt-boundary checkpoints preserve
	// resume state for pause/internal abort.
	if ctx.Err() != nil || partCtx.Err() != nil {
		// Close before user-cancel cleanup (Windows cannot unlink the open payload);
		// internal abort preserves resumable bytes.
		d.applyCancelCleanup()
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, partCtx.Err()
	}

	// Transfer failures preserve staging and committed ranges for resume.
	select {
	case e := <-errCh:
		return false, e
	default:
	}

	// Report full transfer progress, then finalizing before optional content proof
	// and publication so the UI does not sit silently at 100%.
	emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath, Downloaded: it.Size, Total: it.Size})
	emit(ProgressEvent{Event: "file_finalizing", Path: it.RelativePath, Message: "finalizing"})

	// Measure this call's proof/publication interval after the finalizing callback;
	// clear the previous reading so a failure cannot report stale timing.
	finalizationNanos.Store(0)
	finalizeStart := time.Now()

	// Recheck cancellation after file_finalizing; user cleanup applies, while an
	// internal abort preserves staging.
	if cerr := partCtx.Err(); cerr != nil {
		d.applyCancelCleanup()
		return false, cerr
	}

	// Prove a known digest before publication when required, including a resumed
	// ProofPending record in size mode (Verify="none" opts out). Reuse eligible
	// sequential sha256 hash-on-write, otherwise make one full-file pass.
	// No known digest means size/coverage here; the caller's historical sha256
	// HEAD-ETag fallback remains post-publication. Mismatch preserves the pending
	// gate, attempts poison and owned discard, and never publishes these bytes.
	needContentProof := d.requiresPrePublicationProof()
	contentVerified := false
	if needContentProof {
		sum := ""
		usedSeqHash := false
		if cfg.Verify == "sha256" && it.LFS {
			// Read hash usability and digest together; untrusted framing invalidates it.
			if digest, ok := d.seqHashDigest(); ok {
				sum = digest
				usedSeqHash = true
			}
		}
		if !usedSeqHash {
			// The single full-file pass honors partCtx and records hash-pass counters.
			computed, herr := computeSHA256Ctx(partCtx, stg)
			if herr != nil {
				// User cancel closes and cleans up per policy; internal abort preserves bytes
				// with ProofPending still guarding them.
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
			// Never publish a mismatch. Keep ProofPending set and attempt owner-checked
			// poison/discard; if either fails, the durable gate or zero-length payload
			// forces re-proof/discard on the next run.
			d.poisonStagingIfOwned()
			if !removeStagingArtifactsOwned(stg, metaPath) {
				log.Printf("warning: staging data for %s survived a known-digest mismatch; "+
					"its durable ProofPending flag and zero-length payload force the next run to re-prove or discard it",
					it.RelativePath)
			}
			if err := removeMultipartResumeFiles(dst); err != nil {
				log.Printf("warning: cleanup obsolete multipart artifacts for %s after a known-digest mismatch: %v", dst, err)
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

// publish requires the held domain and owned record, complete live and persisted
// layouts, exact payload size, cleared required proof, and a final cancellation
// check. Blob placement follows the no-replace tiers in doc.go; arbitrary targets
// replace bytes the caller declined to skip. Success removes owned staging and
// obsolete multipart artifacts, never the single route's .part/sidecar.
func (d *stagingDownload) publish() error {
	// A pending known-digest proof blocks publication unless Verify="none";
	// preserve the pair for re-proof on a later run.
	if d.proofPending && d.it.SHA256 != "" && d.cfg.Verify != "none" {
		return fmt.Errorf("refusing to publish %s: durable known-digest proof is still pending", d.it.RelativePath)
	}
	// Close before renaming so the unlink-on-rename also works on Windows.
	if err := d.closeOut(); err != nil {
		return fmt.Errorf("close staging file %s before publish: %w", d.stg, err)
	}
	// Check physical payload length independently of range metadata: complete-looking
	// layouts can refer to a short file. The handle is closed before placement.
	if err := checkStagingPayloadSize(d.stg, d.it.Size); err != nil {
		return err
	}

	// Snapshot under metaMu before taking the metadata gate, matching checkpoint
	// lock order. Workers have joined, so this layout will no longer change.
	d.metaMu.Lock()
	live := append([]RangeProgress(nil), d.ranges...)
	d.metaMu.Unlock()

	lock := &stagingStateLock{path: stagingLockPath(d.meta)}
	if err := lock.acquire(d.ctx, stagingLockWait); err != nil {
		return err
	}
	defer lock.release()

	// Recheck both the held destination domain and record ownership under the gate.
	if !d.fenced() {
		return errStagingClaimLost
	}
	if !claimRecordOwnedLocked(d.meta) {
		return errStagingClaimLost
	}
	if err := d.ctx.Err(); err != nil {
		return err
	}
	// Both live and persisted layouts must tile the expected payload with all ranges
	// Done and exactly size committed bytes. Refusal preserves staging for resume.
	if !stagingLayoutComplete(live, d.it.Size) {
		return fmt.Errorf("refusing to publish %s: the staging range layout is incomplete (%d of %d bytes committed); the staged bytes are preserved for a resume: %w",
			d.it.RelativePath, committedTotal(live), d.it.Size, errStagingLayoutIncomplete)
	}
	meta, merr := readStagingMeta(d.meta)
	if merr != nil {
		return fmt.Errorf("refusing to publish %s: the staging record cannot be read: %w", d.it.RelativePath, merr)
	}
	if meta.Size != d.it.Size || !stagingLayoutComplete(meta.Ranges, d.it.Size) {
		return fmt.Errorf("refusing to publish %s: the persisted staging layout is incomplete (%d of %d bytes committed); the staged bytes are preserved for a resume: %w",
			d.it.RelativePath, committedTotal(meta.Ranges), d.it.Size, errStagingLayoutIncomplete)
	}
	// Final cancellation check immediately before placement: everything above
	// validated the staged payload, but a cancellation that arrived after the
	// early ownership guard must still stop the publication. The staged bytes
	// stay on disk for a resume.
	if err := d.ctx.Err(); err != nil {
		return err
	}
	if d.contentAddressed {
		// Preserve existing content-addressed completion: hard-link placement is atomic
		// no-replace; without hard links, check/rename excludes only domain participants.
		// External writers are not fenced by that fallback (see doc.go).
		if err := blobPublishPlace(d.stg, d.pub, d.domain); err != nil {
			if errors.Is(err, errBlobTargetExists) {
				// Reuse the existing blob: discard our staging pair and let the caller create
				// this item's links as a blob-exists skip.
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
	// Placement succeeded; remove our now-obsolete staging record under the same gate.
	removeStagingArtifactsLocked(d.stg, d.meta)
	// Legacy multipart cleanup is nonfatal after successful publication. Leave the
	// single route's .part and identity sidecar to that route.
	if err := removeMultipartResumeFiles(d.dst); err != nil {
		log.Printf("warning: cleanup obsolete multipart artifacts for %s after publish: %v", d.dst, err)
	}
	return nil
}

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

// runRange transfers only the missing suffix within its assigned bounds,
// checkpointing bytes actually written. It applies stall/backoff/refund and
// lifetime retry limits. nil means complete OR partCtx canceled; the caller
// must check cancellation before publishing. Other terminal failures return errors.
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
	// pos is this run's added prefix; hwm prevents duplicate progress refunds.
	pos := int64(0)
	hwm := int64(0)
	// countedUpTo excludes the inherited baseline and counts each newly written byte
	// once, even when overflow forces the same suffix to be rewritten.
	countedUpTo := task.from
	if commit0 > 0 {
		downloadLog.Info("resuming download",
			"repo", d.job.Repo,
			"path", d.it.RelativePath,
			"part", task.idx,
			"offset", task.from,
		)
	}

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
			// Validate range headers before writing; the bounded copy rejects short or
			// oversized bodies, and overflow never completes a range.
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
					end := wantStart + w.written
					if end > countedUpTo {
						d.progress.Add(end - countedUpTo)
						countedUpTo = end
					}
					// Serialize hash writes with invalidation and digest reads under seqMu.
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
					// Resume from accepted writes, but reserve the last byte if framing failed
					// after all requested bytes arrived; retry must request a nonempty suffix.
					accepted := w.written
					if accepted == wantLen {
						accepted--
						// Retrying the reserved byte would hash it twice; invalidate under seqMu and
						// use the final full-file proof instead.
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
				// Overflow commits no new prefix or Done state, even after all requested bytes
				// were accepted. Retry the same suffix within budget and invalidate hash-on-write
				// under seqMu to avoid hashing rewritten bytes twice.
				if overflow {
					d.seqMu.Lock()
					d.seqHash = nil
					d.seqMu.Unlock()
				}
			}
		default:
			// Non-206 fails the attempt. 401/403/404 fail the job; other terminal 4xx fail
			// the file. Retryable statuses use server-aware backoff; an ignored-Range 200
			// remains a retried multipart protocol violation.
			rs.Body.Close()
			if apiErr := classifyFileResponse(rs, d.it.URL); apiErr != nil {
				taskErr = apiErr
				if apiErr.IsFailFast() || !apiErr.IsRetryable() {
					// Cancel siblings with the original terminal error; preserve committed staging.
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

		// Compare with the pre-attempt high-water mark before recording the new one;
		// a later bodyless failure must not earn a second refund for old progress.
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
		downloadLog.Warn("download retry",
			"repo", d.job.Repo,
			"path", d.it.RelativePath,
			"part", task.idx,
			"attempt", attempt,
			"error", lastErr,
		)
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
