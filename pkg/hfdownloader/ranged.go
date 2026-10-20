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
// authoritative for an existing incomplete download and is independent of
// the worker count, so changing Settings.Concurrency between runs re-fetches
// only the missing bytes.
//
// Crash-safety contract: payload data is written BEFORE its checkpoint is
// advanced; the metadata may lag the payload; metadata never claims bytes
// that were not successfully written. The staging file is preallocated to
// the expected logical size, and its on-disk length is NEVER treated as
// evidence of completion — a sparse/preallocated file reports its final
// size from the first truncate, so only the committed ranges count.

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
	"time"
)

// stagingMetaVersion is the IncompleteMeta.Version value written by this
// downloader. A record without it (0) carries no range state and cannot be
// trusted to describe a staging file's valid bytes.
const stagingMetaVersion = 2

// claimGracePeriod is how recently a staging meta file must have been
// created or rewritten for an unreadable/empty record — or for a live
// foreign owner's still-absent payload — to be treated as "another writer
// is mid-claim" instead of stale. The O_EXCL claim creates the file empty
// and writes the record immediately after, so this only covers the window
// in between.
const claimGracePeriod = 10 * time.Second

// claimHeartbeatEvery is how often an owner parked in a retry wait
// rewrites its claim record. It is derived from claimGracePeriod so the
// record cannot go staler than the grace window a claimant uses to decide
// a live foreign owner still owns the staging file.
const claimHeartbeatEvery = claimGracePeriod / 2

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

	// errBlobAlreadyComplete reports that the publish target already
	// exists, so the transfer is unnecessary this run (a concurrent writer
	// finished it between the caller's skip check and the claim).
	errBlobAlreadyComplete = errors.New("destination already complete")
)

// stagingOwners is the in-process exclusion registry keyed by staging file
// path. It closes the same-process half of the CheckBlob/claim TOCTOU
// window: CheckBlob deliberately reports a staging record carrying our own
// PID as take-over-able (an earlier run of this process has no running
// writer and must be resumable), so the registry is what distinguishes that
// case from a concurrently running goroutine of this process. Each entry's
// channel is closed when the owning download returns; a claimant that finds
// a busy entry waits for the owner and then re-evaluates from scratch.
var (
	stagingOwnersMu sync.Mutex
	stagingOwners   = map[string]chan struct{}{}
)

// Observability counters for tests and the opt-in local-I/O benchmark.
// Package variables (not public API) in the established pattern of
// maxAttemptsPerRetry; package tests run sequentially and reset them.
var (
	// stagingWriteBytes counts payload bytes written into staging files by
	// the ranged downloader (the transfer write itself, plus any one-time
	// legacy migration copy).
	stagingWriteBytes atomic.Int64
	// hashPassCount counts full-file SHA-256 passes over downloaded
	// payloads and hashPassBytes the bytes they read. Strict Verify=sha256
	// must perform at most one such pass per file; Verify=size none.
	hashPassCount atomic.Int64
	hashPassBytes atomic.Int64
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
}

// stagingClaimState is what a successful claim produced.
type stagingClaimState struct {
	// ranges is the authoritative committed range layout resumed from the
	// previous owner's record; nil for a fresh claim.
	ranges []RangeProgress
	// fresh reports that no v2 range state existed, so the staging payload
	// holds no validated bytes.
	fresh bool
	// release deregisters the in-process ownership slot; the caller must
	// invoke it exactly once when the download returns.
	release func()
}

// claimStaging establishes exclusive ownership of the staging file.
//
// Exclusion is two-level:
//
//   - in-process: the stagingOwners registry serializes downloads inside
//     this process (including two plan items that share one blob SHA);
//   - cross-process: an O_CREATE|O_EXCL create of the meta record. On
//     EEXIST, a readable record owned by a live foreign process with a
//     fresh staging payload means "owned" (the caller skips); anything
//     else (dead PID, stale payload, unreadable old record, identity
//     mismatch) is a take-over, which preserves the record's committed
//     ranges so resume survives server restarts and crashes.
//
// The take-over rewrites the record under O_EXCL only after confirming the
// on-disk record still matches the one it evaluated, and every successful
// claim re-reads the record to confirm it carries our PID, so a racing
// take-over cannot silently leave two believing owners.
func claimStaging(ctx context.Context, staging, publish, metaPath string, it PlanItem, job Job, cfg Settings) (*stagingClaimState, error) {
	staleTimeout := DefaultStaleTimeout
	if d, err := parseStaleTimeoutSetting(cfg); err == nil {
		staleTimeout = d
	}

	for round := 0; round < 8; round++ {
		// A concurrent writer may have published while we were getting here
		// (or while we waited for the in-process owner above); never
		// re-download a complete destination. Re-checked every round
		// because a waited-for owner often publishes just before waking us.
		if _, err := os.Stat(publish); err == nil {
			return nil, errBlobAlreadyComplete
		}

		// In-process exclusion.
		stagingOwnersMu.Lock()
		done, busy := stagingOwners[staging]
		if !busy {
			done = make(chan struct{})
			stagingOwners[staging] = done
		}
		stagingOwnersMu.Unlock()
		if busy {
			select {
			case <-done:
				// Owner finished: re-evaluate (publish may now exist).
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		release := func() {
			stagingOwnersMu.Lock()
			delete(stagingOwners, staging)
			stagingOwnersMu.Unlock()
			close(done)
		}

		// Cross-process exclusion: exclusive-create the meta record.
		f, err := os.OpenFile(metaPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			werr := writeClaimRecord(f, metaPath, newClaimMeta(it, job))
			if werr != nil {
				release()
				return nil, werr
			}
			// Read-back verification: a racing take-over that removed our
			// fresh record and recreated its own leaves a record that does
			// not carry our PID; we must not believe we own the staging.
			if claimRecordOurs(metaPath) {
				return &stagingClaimState{fresh: true, release: release}, nil
			}
			release()
			continue
		}
		if !errors.Is(err, os.ErrExist) {
			release()
			return nil, err
		}

		// A record exists: evaluate whether its owner is still live.
		raw, rerr := os.ReadFile(metaPath)
		var meta IncompleteMeta
		readable := rerr == nil && json.Unmarshal(raw, &meta) == nil
		if !readable {
			// Unreadable record. A very recent one is another writer
			// mid-claim; an old one is corrupt and takes the stale path.
			if metaFreshWithin(metaPath, claimGracePeriod) {
				release()
				return nil, errBlobIncompleteOwned
			}
			removeStagingArtifacts(staging, metaPath)
			release()
			continue
		}

		foreignLive := meta.PID != 0 && meta.PID != os.Getpid() && isProcessAlive(meta.PID)
		if foreignLive && (payloadMtimeFresh(staging, staleTimeout) || metaFreshWithin(metaPath, claimGracePeriod)) {
			release()
			return nil, errBlobIncompleteOwned
		}

		// Dead or stale owner: take over. A valid v2 record for this same
		// object preserves its committed ranges (resume); anything else is
		// unvalidated data and is discarded per the downloader correctness
		// rules (never guess at unknown resume state).
		if !stagingMetaResumable(meta, it, job) {
			removeStagingArtifacts(staging, metaPath)
			release()
			continue
		}
		// Remove only if unchanged since we evaluated it, then win the
		// exclusive create ourselves. A racer that rewrote the record in
		// between makes the re-read differ and sends us back to
		// re-evaluation instead of destroying its claim.
		if cur, cerr := os.ReadFile(metaPath); cerr != nil || string(cur) != string(raw) {
			release()
			continue
		}
		if rmErr := os.Remove(metaPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			release()
			return nil, rmErr
		}
		f, err = os.OpenFile(metaPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			// Another writer took over; re-evaluate.
			release()
			continue
		}
		take := newClaimMeta(it, job)
		take.StartedAt = meta.StartedAt
		take.Ranges = meta.Ranges
		if werr := writeClaimRecord(f, metaPath, take); werr != nil {
			release()
			return nil, werr
		}
		if claimRecordOurs(metaPath) {
			return &stagingClaimState{ranges: take.Ranges, release: release}, nil
		}
		release()
		continue
	}
	return nil, fmt.Errorf("could not claim staging file %s: claim did not converge", staging)
}

// writeClaimRecord writes the claim record into the exclusively created
// meta file and closes it.
func writeClaimRecord(f *os.File, metaPath string, meta IncompleteMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// claimRecordOurs re-reads the claim record and reports whether it still
// carries our PID (i.e. a racing take-over has not replaced it).
func claimRecordOurs(metaPath string) bool {
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return false
	}
	var m IncompleteMeta
	return json.Unmarshal(raw, &m) == nil && m.PID == os.Getpid()
}

// newClaimMeta builds the claim record for it/job: our PID, the expected
// logical size, and the remote identity. Ranges are filled in by the caller
// once determined (fresh layout or legacy migration) and persisted with
// writeStagingMeta.
func newClaimMeta(it PlanItem, job Job) IncompleteMeta {
	return IncompleteMeta{
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC(),
		Size:      it.Size,
		SHA256:    it.SHA256,
		Version:   stagingMetaVersion,
		Repo:      job.Repo,
		Revision:  job.Revision,
		Path:      it.RelativePath,
	}
}

// stagingMetaResumable reports whether a staging record describes this
// exact remote object with a well-formed range layout, making its
// committed prefixes trustworthy resume data.
func stagingMetaResumable(meta IncompleteMeta, it PlanItem, job Job) bool {
	if meta.Version != stagingMetaVersion || meta.Size != it.Size || meta.Size <= 0 {
		return false
	}
	if meta.SHA256 != "" && it.SHA256 != "" && !strings.EqualFold(meta.SHA256, it.SHA256) {
		return false
	}
	// Without a blob SHA the record must at least identify the same remote
	// object by repo and path, and the same revision: with no digest to
	// validate content identity, a moved branch could otherwise resume
	// another revision's bytes into this file.
	if it.SHA256 == "" && (meta.Repo != job.Repo || meta.Path != it.RelativePath || meta.Revision != job.Revision) {
		return false
	}
	return rangeLayoutValid(meta.Ranges, meta.Size)
}

// rangeLayoutValid reports whether ranges exactly tiles [0, size) with
// in-range committed prefixes and no byte claimed past a range end.
func rangeLayoutValid(ranges []RangeProgress, size int64) bool {
	if len(ranges) == 0 || size <= 0 {
		return false
	}
	var off int64
	for i, r := range ranges {
		if r.Start != off || r.End < r.Start || r.End >= size {
			return false
		}
		committed := r.Committed
		if r.Done {
			committed = r.End - r.Start + 1
		}
		if committed < 0 || r.Start+committed > r.End+1 {
			return false
		}
		off = r.End + 1
		if i == len(ranges)-1 {
			return off == size
		}
	}
	return false // unreachable for non-empty ranges
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

// payloadMtimeFresh reports whether the staging payload received writes
// recently. A missing payload is never fresh (mirrors CheckBlob's
// BlobMissing for a vanished staging file).
func payloadMtimeFresh(staging string, staleTimeout time.Duration) bool {
	fi, err := os.Stat(staging)
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) < staleTimeout
}

// parseStaleTimeoutSetting resolves the configured stale timeout with the
// same defaults as Settings.BuildHFCache.
func parseStaleTimeoutSetting(cfg Settings) (time.Duration, error) {
	if cfg.StaleTimeout == "" {
		return DefaultStaleTimeout, nil
	}
	return time.ParseDuration(cfg.StaleTimeout)
}

// removeStagingArtifacts removes the staging payload and its meta record
// (best-effort; missing files are not an error).
func removeStagingArtifacts(staging, metaPath string) {
	// lgtm[go/path-injection]
	if err := os.Remove(staging); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: cleanup staging file %s: %v", staging, err)
	}
	// lgtm[go/path-injection]
	if err := os.Remove(metaPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: cleanup staging meta %s: %v", metaPath, err)
	}
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

// writeStagingMeta atomically rewrites the staging record (stage in the
// same directory, then rename) so a crash never leaves a partial record.
func writeStagingMeta(metaPath string, meta *IncompleteMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp := metaPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, metaPath)
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

// migrateLegacyMultipart converts a valid legacy .part-NN + .parts.json
// layout for dst into range progress over the staging file, copying each
// part's valid prefix into place. It returns the migrated layout, or
// errNoLegacyState when no usable legacy state exists — in which case any
// unvalidatable artifacts (parts without a layout, or a layout that does
// not tile the file) have been discarded, per the downloader correctness
// rules: part files are only reusable when their byte-range layout is
// known to match. A copy I/O error is returned as-is and leaves the
// legacy artifacts untouched for a retry.
//
// On success the legacy artifacts are deliberately NOT removed yet: the
// staging record does not describe the migrated bytes until the caller
// persists the returned layout, and removing the parts before that flush
// would leave the staging payload as the only copy of a valid partial
// under a record with no range layout — unvalidatable state the next run
// must discard. The caller removes the obsolete artifacts only after the
// migrated layout is durably persisted (issue #140, criterion 11: a valid
// legacy partial must survive the upgrade until its replacement is
// durable).
func migrateLegacyMultipart(dst, staging string, size int64) ([]RangeProgress, error) {
	data, err := os.ReadFile(multipartLayoutMetaPath(dst))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No layout: part files (if any) have an unknown byte-range
			// contract and must not be reused.
			_ = removeMultipartResumeFiles(dst)
			return nil, errNoLegacyState
		}
		return nil, err
	}
	var layout multipartResumeLayout
	if err := json.Unmarshal(data, &layout); err != nil {
		_ = removeMultipartResumeFiles(dst)
		return nil, errNoLegacyState
	}
	if layout.Size != size || len(layout.Parts) == 0 {
		_ = removeMultipartResumeFiles(dst)
		return nil, errNoLegacyState
	}
	// The legacy layout must tile [0, size) exactly; anything else is
	// corrupt and unvalidatable.
	var off int64
	ranges := make([]RangeProgress, len(layout.Parts))
	for i, p := range layout.Parts {
		if p.Index != i || p.Start != off || p.End < p.Start || p.End >= size {
			_ = removeMultipartResumeFiles(dst)
			return nil, errNoLegacyState
		}
		off = p.End + 1
		ranges[i] = RangeProgress{Start: p.Start, End: p.End}
	}
	if off != size {
		_ = removeMultipartResumeFiles(dst)
		return nil, errNoLegacyState
	}

	out, err := os.OpenFile(staging, os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	for i := range ranges {
		partPath := fmt.Sprintf("%s.part-%02d", dst, i)
		fi, err := os.Stat(partPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // missing part: nothing committed for this range
			}
			return nil, err
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
			return nil, err
		}
		_, cerr := io.CopyN(newBoundedAtWriter(out, ranges[i].Start, committed), in, committed)
		in.Close()
		if cerr != nil {
			return nil, cerr
		}
		ranges[i].Committed = committed
		ranges[i].Done = committed == want
	}
	return ranges, nil
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
// The exact body byte count is enforced by the bounded writer at copy
// time: short and oversized bodies are both errors, and a worker can never
// write before or after its assigned range.
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

// boundedAtWriter writes into a file at a fixed base offset and refuses to
// write more than limit bytes, so a worker can never write before or after
// its assigned range no matter what the server sends. Each successful
// Write advances the position; written reports the bytes accepted.
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
	if w.written+int64(len(p)) > w.limit {
		return 0, fmt.Errorf("ranged body exceeds assigned range: wrote %d of %d bytes, got %d more", w.written, w.limit, len(p))
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

	// startedAt preserves the original StartedAt of a resumed record.
	startedAt time.Time

	metaMu sync.Mutex // guards ranges + meta rewrites

	// progress counts bytes written this run; emitted progress is
	// baseCommitted + progress, and never decreases within a run.
	progress atomic.Int64
	// baseCommitted is the committed total at claim time (excluded from
	// this-run speed accounting, consistent with the resumed/skipped-bytes
	// rule the single path already follows).
	baseCommitted int64

	// seqHash, when non-nil, is fed every staging write in order: strict
	// hash-on-write for the single fresh sequential transfer.
	seqHash hashWriter
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

// flushMetaLocked persists the current range state; caller holds metaMu.
func (d *stagingDownload) flushMetaLocked() error {
	meta := newClaimMeta(d.it, d.job)
	meta.StartedAt = d.startedAt
	meta.Ranges = d.ranges
	return writeStagingMeta(d.meta, &meta)
}

// retryWaitSleep waits out a retry/backoff pause while keeping this
// owner's claim on the staging file. A live owner legitimately parked in
// a long server-requested wait (Retry-After up to DefaultMaxRetryAfter,
// which equals the default stale timeout) writes no payload bytes and
// would otherwise look exactly like a stale owner: both the payload and
// the claim record go stale, and a second process takes over the staging
// file while this owner keeps writing — two writers on one payload
// (issue #140, criterion 12). The wait is therefore chunked and the
// claim record rewritten between chunks, so record freshness tracks
// owner liveness instead of transfer progress. A genuinely hung owner is
// still recovered: one that writes nothing and rewrites no record for
// the grace/stale windows loses its claim exactly as before.
//
// ok reports whether the full wait elapsed; false means ctx was done and
// the caller must treat the attempt as cancelled (as with sleepCtx). A
// record-rewrite failure is returned as an error and is fatal exactly
// like any other metadata flush: without a durable, fresh record the
// claim cannot be defended.
func (d *stagingDownload) retryWaitSleep(ctx context.Context, wait time.Duration) (ok bool, err error) {
	for wait > 0 {
		step := wait
		if step > claimHeartbeatEvery {
			step = claimHeartbeatEvery
		}
		if !sleepCtx(ctx, step) {
			return false, nil
		}
		wait -= step
		if wait <= 0 {
			return true, nil
		}
		if err := d.flushMeta(); err != nil {
			return false, err
		}
	}
	return true, nil
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
// whether a strict SHA-256 verification of the payload already completed
// inside this call (hash-on-write or the single post-range pass), so the
// caller must not repeat it.
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
		return false, downloadSingle(ctx, httpc, token, job, cfg, it, dst, emit)
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

	// Claim the staging file exclusively (in-process registry + O_EXCL
	// meta record). A live foreign owner or an already-published
	// destination short-circuits the file to a skip in the caller.
	state, err := claimStaging(ctx, stg, pub, metaPath, it, job, cfg)
	if err != nil {
		return false, err
	}
	defer state.release()

	d := &stagingDownload{
		ctx:       ctx,
		httpc:     httpc,
		token:     token,
		job:       job,
		cfg:       cfg,
		it:        it,
		dst:       dst,
		stg:       stg,
		pub:       pub,
		meta:      metaPath,
		emit:      emit,
		startedAt: time.Now().UTC(),
	}

	// Open (or create) the staging payload and preallocate it to the
	// expected logical size. Preallocation makes the ranged writes cheap,
	// but the resulting file length is never evidence of completion.
	out, err := os.OpenFile(stg, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false, err
	}
	defer out.Close()
	d.out = out
	if err := out.Truncate(it.Size); err != nil {
		return false, err
	}

	// Determine the authoritative range layout.
	legacyCleanup := false
	if state.fresh {
		// No valid v2 state: a legacy .part-NN/.parts.json layout may hold
		// validated bytes; migrate it once. Without a usable layout the
		// part files are unvalidatable and were discarded by the
		// migration helper.
		ranges, merr := migrateLegacyMultipart(dst, stg, it.Size)
		switch {
		case merr == nil:
			d.ranges = ranges
			// The migrated layout replaces the legacy artifacts as the
			// resume authority — but only once it is durable. Until the
			// flush below succeeds, the legacy part files remain the
			// only finishable copy of the partial (criterion 11).
			legacyCleanup = true
		case errors.Is(merr, errNoLegacyState):
			d.ranges = buildRangeLayout(it.Size, cfg.Concurrency)
		default:
			// A real I/O failure must not silently discard legacy data.
			return false, merr
		}
	} else {
		d.ranges = append([]RangeProgress(nil), state.ranges...)
		d.startedAt = time.Now().UTC() // resumed run keeps its own clock
	}
	d.baseCommitted = committedTotal(d.ranges)

	if err := d.flushMeta(); err != nil {
		return false, err
	}
	if legacyCleanup {
		// The migrated range layout is now durable in the staging record,
		// so the legacy .part-NN/.parts.json artifacts are obsolete and
		// can be removed. A removal failure is surfaced (not hidden) and
		// leaves the artifacts for a later retry, exactly as before.
		if err := removeMultipartResumeFiles(dst); err != nil {
			return false, err
		}
	}

	// Missing work: the uncommitted suffix of every incomplete range. The
	// persisted layout stays authoritative regardless of the configured
	// worker count, so a changed Concurrency re-fetches only missing
	// bytes and never invalidates committed ones.
	tasks := make([]rangeTask, 0, len(d.ranges))
	for i := range d.ranges {
		r := &d.ranges[i]
		if r.Done {
			continue
		}
		tasks = append(tasks, rangeTask{idx: i, from: r.Start + r.Committed})
	}

	// Strict hash-on-write is only possible when this run writes the whole
	// payload sequentially from offset 0: one task covering [0, size) with
	// no pre-committed bytes. Resumed or out-of-order transfers fall back
	// to the single post-range full-file pass below.
	if cfg.Verify == "sha256" && it.LFS && it.SHA256 != "" &&
		len(tasks) == 1 && d.baseCommitted == 0 &&
		d.ranges[tasks[0].idx].Start == 0 && d.ranges[tasks[0].idx].End == it.Size-1 {
		d.seqHash = sha256.New()
	}

	// Part-scoped cancellable context: a permanent (fail-fast) error on any
	// one range cancels the whole file so sibling workers stop promptly
	// instead of downloading/retrying/waiting until wg.Wait() returns. The
	// cancel cause is an abortCause wrapping the original error (see
	// abortError), which lets a sibling worker that exits via its ctx path
	// distinguish an internal abort from a user cancel/pause.
	partCtx, partCancel := context.WithCancelCause(ctx)
	defer partCancel(nil)

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

	// If the context was cancelled while workers were running (pause /
	// abort / timeout), return the cancellation error immediately. Workers
	// that exit via their ctx-aware retry/sleep path do NOT push to errCh,
	// so the ctx check must happen explicitly — and it is what prevents a
	// bogus "downloaded == total" progress emit and stops an incomplete
	// staging file from being published. Every attempt boundary already
	// flushed the committed high-water marks, so a pause preserves exactly
	// the durable resume state.
	if ctx.Err() != nil || partCtx.Err() != nil {
		if !abortPartAttempt(partCtx) {
			cleanupPartialsOnCancel(cfg, dst, stg)
		}
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

	// Abort before finalizing if the job/part context was cancelled between
	// the guard above and here. An internal fail-fast abort preserves the
	// staging bytes; a user cancel runs the cancel-cleanup.
	if cerr := partCtx.Err(); cerr != nil {
		if !abortPartAttempt(partCtx) {
			cleanupPartialsOnCancel(cfg, dst, stg)
		}
		return false, cerr
	}

	// Strict SHA-256 mode: at most one full read of the payload. With
	// parallel direct writes the hash cannot be composed from range
	// hashes, so a fresh sequential transfer may have fed the hash on
	// write (eliminating the pass); every other case performs exactly one
	// sequential pass here, BEFORE publication, so corrupt bytes are
	// never renamed to the final destination.
	if cfg.Verify == "sha256" && it.LFS && it.SHA256 != "" {
		sum := ""
		if d.seqHash != nil {
			// The single sequential task fed the hash with every staging
			// write, in order, from offset 0.
			sum = hex.EncodeToString(d.seqHash.Sum(nil))
		} else {
			// One sequential full-file pass over the completed staging file,
			// bounded by ctx; computeSHA256Ctx records it in the
			// hash-pass counters.
			computed, herr := computeSHA256Ctx(ctx, stg)
			if herr != nil {
				if cerr := ctx.Err(); cerr != nil {
					return false, cerr
				}
				return false, fmt.Errorf("sha256 verify: %w", herr)
			}
			sum = computed
		}
		if !strings.EqualFold(sum, it.SHA256) {
			// Corruption detected: never publish, and remove the staging
			// data — its "committed" ranges produced a wrong digest, so
			// none of it is trustworthy. The explicit redownload policy is
			// this actionable error plus a fresh transfer on retry.
			removeStagingArtifacts(stg, metaPath)
			_ = removeMultipartResumeFiles(dst)
			return false, fmt.Errorf("sha256 verify failed: %s: expected %s got %s", it.RelativePath, it.SHA256, sum)
		}
		return true, d.publish()
	}

	return false, d.publish()
}

// publish atomically renames the staging file onto the destination and
// removes the resume metadata, then removes any obsolete legacy part
// artifacts for dst. The staging file and its publish target share a
// directory, so the normal path is rename-only — no payload copy.
func (d *stagingDownload) publish() error {
	// Close before renaming so the unlink-on-rename also works on Windows.
	d.out.Close()
	if err := os.Rename(d.stg, d.pub); err != nil {
		return err
	}
	// lgtm[go/path-injection]
	_ = os.Remove(d.meta)
	// Obsolete artifacts from a pre-#140 version, if any linger next to
	// dst, must not survive a successful finalization.
	_ = removeMultipartResumeFiles(d.dst)
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
			// copy, so short and oversized bodies are both errors.
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
					d.progress.Add(int64(len(p)))
					if d.seqHash != nil {
						_, _ = d.seqHash.Write(p)
					}
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
				// A fired stall watchdog marks this attempt's failure as a
				// stall; a done partCtx keeps the genuine-cancel error.
				taskErr = classifyStall(partCtx, body, cerr)
				// pos advances by the bytes actually written, so the next
				// retry's Range request resumes from the durable offset and
				// the committed checkpoint never passes the written bytes.
				pos += w.written
				d.metaMu.Lock()
				r.Committed = commit0 + pos
				ferr := d.flushMetaLocked()
				d.metaMu.Unlock()
				if ferr != nil {
					attemptCancel()
					return ferr
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
