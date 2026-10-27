package hfdownloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RepoType indicates whether a repository is a model or dataset.
type RepoType string

const (
	RepoTypeModel   RepoType = "model"
	RepoTypeDataset RepoType = "dataset"
)

// HFCache represents the HuggingFace cache root directory.
// It follows the official HuggingFace Hub cache structure.
type HFCache struct {
	// Root is the cache root directory (e.g., ~/.cache/huggingface)
	Root   string
	hubDir string // Resolved once; never reinterpret ambient ENV in accessors.

	// StaleTimeout is the duration after which an .incomplete file
	// with no writes is considered stale and can be taken over.
	StaleTimeout time.Duration
}

// DefaultStaleTimeout is the default timeout for stale .incomplete files.
const DefaultStaleTimeout = 5 * time.Minute

// DefaultCacheDir returns the default HuggingFace cache directory.
// Priority: HF_HOME env > ~/.cache/huggingface
func DefaultCacheDir() string {
	if hfHome := os.Getenv("HF_HOME"); hfHome != "" {
		return hfHome
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".cache/huggingface"
	}
	return filepath.Join(home, ".cache", "huggingface")
}

// NewHFCache creates a new HFCache with the given root directory.
// If root is empty, it uses DefaultCacheDir().
func NewHFCache(root string, staleTimeout time.Duration) *HFCache {
	if root == "" {
		root = DefaultCacheDir()
	}
	return NewHFCacheResolved(root, os.Getenv("HF_HUB_CACHE"), staleTimeout)
}

// rootedOrFallback returns p when p is a rooted (absolute) filesystem path
// and fallback otherwise. The rooted test is stated with strings.HasPrefix,
// the primitive code scanning models as a path guard; returning p inside the
// true branch means taint in p cannot flow past this helper, so a
// caller-supplied root reaches the cache only through a proven-rooted value.
func rootedOrFallback(p, fallback string) string {
	if strings.HasPrefix(p, filepath.VolumeName(p)+string(filepath.Separator)) {
		return p
	}
	return fallback
}

// NewHFCacheResolved constructs a cache from an already selected root/Hub
// association without consulting ENV. An empty hubDir means root/hub.
// Callers freezing destinations must supply the resolved nonempty root.
func NewHFCacheResolved(root, hubDir string, staleTimeout time.Duration) *HFCache {
	// Cache instances carry physical paths, not paths that can silently change
	// meaning if the caller later changes its working directory.
	if absolute, err := filepath.Abs(root); err == nil {
		root = absolute
	}
	// filepath.Abs can only fail when the working directory is unavailable,
	// which would otherwise leave every path in this cache relative to a
	// directory that can change at any time. Fall back to the default cache
	// location so root is always rooted.
	root = rootedOrFallback(root, DefaultCacheDir())
	if hubDir == "" {
		hubDir = filepath.Join(root, "hub")
	}
	if absolute, err := filepath.Abs(hubDir); err == nil {
		hubDir = absolute
	}
	hubDir = rootedOrFallback(hubDir, filepath.Join(root, "hub"))
	if staleTimeout == 0 {
		staleTimeout = DefaultStaleTimeout
	}
	return &HFCache{
		Root:         root,
		hubDir:       hubDir,
		StaleTimeout: staleTimeout,
	}
}

// HubDir returns the path to the hub/ directory.
func (c *HFCache) HubDir() string {
	return c.hubDir
}

// ModelsDir returns the path to the friendly models/ directory.
func (c *HFCache) ModelsDir() string {
	return filepath.Join(c.Root, "models")
}

// DatasetsDir returns the path to the friendly datasets/ directory.
func (c *HFCache) DatasetsDir() string {
	return filepath.Join(c.Root, "datasets")
}

// RepoDir represents a single repository within the HF cache.
type RepoDir struct {
	cache    *HFCache
	repoType RepoType
	owner    string
	name     string
}

// Repo returns a RepoDir for the given repository.
// repoID should be in the format "owner/name".
func (c *HFCache) Repo(repoID string, repoType RepoType) (*RepoDir, error) {
	// IsValidModelName is the authoritative repo-ID rule (exact owner/name,
	// no traversal segments). filepath.IsLocal restates the traversal part of
	// that rule with the primitive code scanning models as a path barrier, so
	// the repoID-derived directory names below are provably non-escaping; it
	// never accepts anything IsValidModelName would reject afterwards.
	if !filepath.IsLocal(repoID) {
		return nil, fmt.Errorf("invalid repo ID: %q must be a local path", repoID)
	}
	// Reuse the single repo-ID validator so cache lookups reject the same
	// traversal/separator inputs as the HTTP handlers (e.g. "../foo").
	if !IsValidModelName(repoID) {
		return nil, fmt.Errorf("invalid repo ID: %q (expected owner/name)", repoID)
	}
	parts := strings.SplitN(repoID, "/", 2)
	return &RepoDir{
		cache:    c,
		repoType: repoType,
		owner:    parts[0],
		name:     parts[1],
	}, nil
}

// dirName returns the HF cache directory name for this repo.
// Format: models--{owner}--{name} or datasets--{owner}--{name}
func (r *RepoDir) dirName() string {
	prefix := "models"
	if r.repoType == RepoTypeDataset {
		prefix = "datasets"
	}
	return fmt.Sprintf("%s--%s--%s", prefix, r.owner, r.name)
}

// Path returns the full path to this repo's cache directory.
// Example: ~/.cache/huggingface/hub/models--TheBloke--Mistral-7B-GGUF
func (r *RepoDir) Path() string {
	return filepath.Join(r.cache.HubDir(), r.dirName())
}

// BlobsDir returns the path to the blobs/ directory.
func (r *RepoDir) BlobsDir() string {
	return filepath.Join(r.Path(), "blobs")
}

// RefsDir returns the path to the refs/ directory.
func (r *RepoDir) RefsDir() string {
	return filepath.Join(r.Path(), "refs")
}

// SnapshotsDir returns the path to the snapshots/ directory.
func (r *RepoDir) SnapshotsDir() string {
	return filepath.Join(r.Path(), "snapshots")
}

// invalidBlobName is the contained placeholder BlobPath returns when handed a
// blob name that would escape the blobs directory. It is a fixed string (so a
// tainted name can never influence the returned path) that cannot collide with
// real blob names, which are hex digests.
const invalidBlobName = "invalid-blob-name"

// errReservedBlobName reports a blob key whose NAME belongs to the
// downloader's staging namespace.
//
// The blobs/ directory holds two kinds of entries: published content and the
// downloader's own staging artifacts. The shared classifier (see
// ClassifyBlobEntryName) excludes a staging-shaped name from EVERY content
// surface - the cache statistics, the listing, the selection, the mirror and
// the deletion path - so a blob published under such a name is invisible:
// stored, counted as nothing, never mirrored, never selected. That is a
// successful store with no reachable result, so the boundary that chooses the
// blob name refuses the name instead.
//
// The refusal is deliberately narrow: it applies to the NAME SHAPE only. A
// non-canonical but non-reserved name (an unusual published name) stays
// accepted, because the public storage API's tolerance for unusual names is
// unchanged.
var errReservedBlobName = errors.New("blob name belongs to the downloader's staging namespace")

// checkPublishableBlobName refuses a blob key whose name classifies as a
// downloader staging artifact.
//
// It is called by every public path that chooses a blob name from a
// caller-provided key, so the invariant holds at the boundary where the name
// enters the cache: a staging-shaped name is never published, and therefore
// never becomes an invisible blob.
func checkPublishableBlobName(name string) error {
	if IsStagingArtifactName(name) {
		return fmt.Errorf("refuse blob name %q: %w; a published blob may not use one of the downloader's staging artifact names", name, errReservedBlobName)
	}
	return nil
}

// BlobPath returns the path where a blob with the given SHA256 should be stored.
// The name is confined to the blobs directory the same way RefPath and
// SnapshotDir confine theirs: SafeJoin rejects values that are not local
// (filepath.IsLocal, the CodeQL-modeled path-injection barrier), so no blob
// name can point outside blobs/. Malformed remote SHAs are already rejected
// loudly by scanRepo; the placeholder below only covers a direct API misuse
// because this method cannot return an error without breaking its callers.
func (r *RepoDir) BlobPath(sha256 string) string {
	blobPath, err := SafeJoin(r.BlobsDir(), sha256)
	if err != nil {
		return filepath.Join(r.BlobsDir(), invalidBlobName)
	}
	return blobPath
}

// IncompletePath returns the path for an incomplete download.
// The .incomplete file contains the partial data.
func (r *RepoDir) IncompletePath(sha256 string) string {
	return filepath.Join(r.BlobsDir(), sha256+".incomplete")
}

// IncompleteMetaPath returns the path for the incomplete metadata file.
func (r *RepoDir) IncompleteMetaPath(sha256 string) string {
	return filepath.Join(r.BlobsDir(), sha256+".incomplete.meta")
}

// RefPath returns the path to a ref file (e.g., refs/main).
// It validates that the resolved path stays inside the refs directory.
func (r *RepoDir) RefPath(ref string) (string, error) {
	return SafeJoin(r.RefsDir(), ref)
}

// SnapshotDir returns the path to a snapshot directory for a given commit.
// It validates that the resolved path stays inside the snapshots directory.
func (r *RepoDir) SnapshotDir(commit string) (string, error) {
	return SafeJoin(r.SnapshotsDir(), commit)
}

// EnsureDirs creates all necessary directories for this repo.
func (r *RepoDir) EnsureDirs() error {
	dirs := []string{
		r.BlobsDir(),
		r.RefsDir(),
		r.SnapshotsDir(),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}
	return nil
}

// IncompleteMeta contains metadata about an incomplete download.
//
// The base fields (PID, StartedAt, Size, SHA256) describe the transfer, while
// Instance and Host carry the cross-process identity and same-host liveness
// hints used by CheckBlob and the claim/take-over logic. PID alone is only a
// hint: a reused PID or two containers reporting PID 1 must not be treated as
// the same owner. The Version/Ranges fields extend the record into the
// ranged-download resume state (issue #140): versioned metadata describes a
// direct-write staging file whose valid bytes are the committed prefixes of
// the listed ranges. Ranges is the authoritative byte contract for an existing
// incomplete download, deliberately decoupled from worker count so changed
// Settings.Concurrency re-fetches only missing bytes. Crash-safety contract:
// payload data is written before Committed is advanced, and Committed must not
// claim bytes that were not written. The current version is 4; Done requires
// clean response framing. A record of any other version is not understood: it
// is discarded under the claim's ownership and the file is re-downloaded from
// zero (old/unsupported partials are never migrated). The staging file is
// preallocated, so its length is never evidence of completion — only Ranges is,
// after payload existence and logical size have been checked.
type IncompleteMeta struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	// Instance is a random per-process identity token. It distinguishes an
	// earlier run of this process instance from a different process that
	// happens to report the same PID (PID reuse, containers sharing PID 1).
	// PID and Host are hints; Instance is never inferred from them.
	Instance string `json:"instance,omitempty"`
	// Host is the hostname that wrote the record. It allows a same-host
	// liveness check of PID; an empty or different host disables PID-based
	// liveness, so the owner cannot be proven dead.
	Host string `json:"host,omitempty"`
	// LockFenced records that this claim was made while holding the
	// destination's ownership domain: the OS-level exclusive lock on the
	// persistent domain lock (blobs/tmp-<name>.lock for a cache destination,
	// <destination>.incomplete.lock for a flat/output-mode one). On a platform
	// that supports the lock, its state is then authoritative for CheckBlob:
	// busy means a live writer, and a free lock means the owner crashed or
	// released the claim, so the record is stale and take-over-able regardless
	// of PID reuse or record age. Records written on a fallback platform (or by
	// an older version) carry false: they are protected unless same-host owner
	// death is proven, and a free domain lock never makes them stale.
	LockFenced bool `json:"lock_fenced,omitempty"`
	// Version is 0 for pre-range records and 4 for the ranged staging records
	// written by this downloader. Only version 4 is resumable: every other
	// version is an old or unsupported format, so its record and partial are
	// discarded under the claim's ownership and the file is re-downloaded from
	// zero. A version-3 record written by an earlier branch (whose
	// legacy_migrated flag described migrated bytes) is therefore never
	// trusted as ordinary resume state.
	Version int `json:"version,omitempty"`
	// Repo, Path and Commit identify the resolved remote object the staging
	// file belongs to, so a reused destination or moved branch cannot
	// silently resume another revision's bytes. The commit is the resolved
	// SHA from the plan, not the requested branch name. CommitPinned states
	// whether that commit was actually resolved: an unpinned identity (the
	// planner's fallback to the requested revision) pins nothing, so the
	// record is inheritable only when the flag is set here AND on the
	// current plan.
	Repo   string `json:"repo,omitempty"`
	Commit string `json:"commit,omitempty"`
	Path   string `json:"path,omitempty"`
	// CommitPinned records whether Commit is a RESOLVED, immutable commit
	// identity rather than the planner's fallback to the requested revision.
	// An unpinned identity pins nothing: the branch may have moved, so the
	// bytes staged under that name may not be inherited by a later run. The
	// record is inheritable only when the flag is set on the record AND on
	// the current plan. A record written without the flag is unpinned, and
	// therefore not inheritable: the safe direction, and no released version
	// ever wrote these records.
	CommitPinned bool `json:"commit_pinned"`
	// Ranges lists the persisted range layout and per-range committed
	// progress; see RangeProgress. Empty only for the brief ownership-only
	// window before the fresh layout is flushed.
	Ranges []RangeProgress `json:"ranges,omitempty"`
	// ProofPending is the durable publication gate for known-digest
	// verification. It is set (and flushed) before any pre-publication
	// SHA-256 proof starts — a fresh sha256/etag multipart transfer or an
	// unpinned-identity proof — and cleared (and flushed) only after
	// that proof succeeds, immediately before the publish rename. A record that
	// still carries the flag forces the proof on the next claim regardless of
	// the current Verify mode (including "size") whenever the item has a known
	// digest and Verify is not "none", so a crash mid-proof, a mismatch whose
	// cleanup could not take the destination domain, or a later mode change cannot
	// turn unproven all-Done ranges into a publishable blob. "none" is the
	// explicit no-hash opt-out. On a mismatch the flag stays set and the
	// payload is poisoned (truncated) in addition. The single-request path has
	// no staging record: its .part is only renamed after the proof, so the
	// .part itself is the durable pending marker.
	ProofPending bool `json:"proof_pending,omitempty"`
}

// RangeProgress is the durable state of one persisted download range.
// Bytes [Start, Start+Committed) are the durable range prefix. Done marks the
// whole range [Start, End] as committed after clean response framing. A full
// prefix with Done=false reserves the last byte for framing revalidation.
type RangeProgress struct {
	Start     int64 `json:"start"`
	End       int64 `json:"end"`
	Committed int64 `json:"committed"`
	Done      bool  `json:"done"`
}

// BlobStatus represents the status of a blob in the cache.
type BlobStatus int

const (
	// BlobMissing means the blob doesn't exist and no download is in progress.
	BlobMissing BlobStatus = iota

	// BlobComplete means the blob exists and is complete.
	BlobComplete

	// BlobDownloading means the blob is not available and its state is not
	// stale. In the proven case a live owner - another process, or another
	// writer of this one - holds the destination's ownership domain, so the
	// file is not present and the caller must not skip the item. A free domain
	// lock plus an UNFENCED record is conservatively reported as downloading
	// too: no live owner is proven there, but the record is not provably stale
	// either, so the claim refuses it as unproven instead of taking it over.
	BlobDownloading

	// BlobStale means an incomplete download exists but is stale (can be taken over).
	BlobStale

	// BlobOwnershipUnknown means the ownership of the incomplete download could
	// not be determined: the OS-level ownership probe failed (or this platform
	// cannot represent the lock), so neither "a live writer owns it" nor "its
	// owner is gone" is proven.
	//
	// CheckBlob returns this status together with a non-nil error that wraps
	// ErrBlobOwnershipUnknown. An unknown ownership state is an observable
	// failure, never a successful skip: reporting it as "downloading by
	// another process" would claim a live owner that was never proven, and the
	// job would then report success with the blob still missing.
	BlobOwnershipUnknown
)

// String returns a human-readable status.
func (s BlobStatus) String() string {
	switch s {
	case BlobMissing:
		return "missing"
	case BlobComplete:
		return "complete"
	case BlobDownloading:
		return "downloading"
	case BlobStale:
		return "stale"
	case BlobOwnershipUnknown:
		return "ownership-unknown"
	default:
		return "unknown"
	}
}

// CheckBlob checks the status of a blob in the cache.
// Returns the status and any existing incomplete metadata.
//
// The result distinguishes three ownership outcomes, and never guesses:
//
//   - a PROVEN live owner (the persistent destination domain lock
//     blobs/tmp-<sha>.lock is held exclusively) is reported as
//     BlobDownloading. The caller must NOT skip the item: the file is not
//     present while that owner holds the key, so the caller waits for the
//     blob key's ownership domain and re-checks the published
//     content-addressed target afterwards;
//   - a PROVABLY dead owner (a free lock plus a lock-fenced record, or
//     same-host process evidence that the recorded owner is gone) is reported
//     as BlobStale, so the caller may resume it;
//   - an UNKNOWN ownership state (the ownership probe failed, or this
//     platform cannot represent the lock) is reported as
//     BlobOwnershipUnknown together with an error that wraps
//     ErrBlobOwnershipUnknown. The caller must surface that error instead of
//     skipping the item: no live owner is proven, so a skip would be an
//     unproven claim that turns into a successful download with the blob
//     still missing.
//
// A held OS-level DESTINATION-domain ownership lock (the persistent
// blobs/tmp-<sha>.lock) means a live writer owns the destination and is
// reported as BlobDownloading before any record heuristic: a suspended owner
// can still resume writing through an open handle, so the lock, not record age,
// decides liveness wherever it is supported. A free lock
// plus a lock-fenced record proves the current-version owner is gone. A free
// lock plus an UNFENCED record is not stale: no live owner is proven either, so
// the record is preserved, and the claim refuses it with the unproven-ownership
// error rather than taking it over.
func (r *RepoDir) CheckBlob(sha256 string) (BlobStatus, *IncompleteMeta, error) {
	blobPath := r.BlobPath(sha256)
	incompletePath := r.IncompletePath(sha256)
	metaPath := r.IncompleteMetaPath(sha256)

	// Check if complete blob exists
	if _, err := os.Stat(blobPath); err == nil {
		return BlobComplete, nil, nil
	}

	// Check if incomplete download exists
	_, err := os.Stat(incompletePath)
	if errors.Is(err, os.ErrNotExist) {
		return BlobMissing, nil, nil
	}
	if err != nil {
		return BlobMissing, nil, fmt.Errorf("stat incomplete file: %w", err)
	}

	// A busy destination ownership lock is the authoritative "a live writer
	// owns this destination" signal. It covers a suspended owner (which could
	// still resume writing through an open handle) and an in-process download
	// whose record looks take-over-able. The probe never blocks, never creates
	// the lock file, and releases immediately.
	//
	// The probed lock is the DESTINATION DOMAIN's: blobs/tmp-<sha>.lock. That
	// is the fence every writer of this destination holds for its whole
	// operation (both routes and the cache store), so it is the only lock a
	// status query may interpret. The obsolete <staging>.lock of earlier
	// branch-only builds is deliberately not consulted: no current writer
	// takes it, and treating it as ownership would report a live writer that
	// does not exist.
	domainDst, derr := r.blobDomainDst(sha256)
	if derr != nil {
		meta, _ := r.readIncompleteMeta(metaPath)
		return BlobOwnershipUnknown, meta, fmt.Errorf("%w: cannot resolve the ownership domain of the incomplete download %s: %w",
			ErrBlobOwnershipUnknown, incompletePath, derr)
	}
	busy, probeErr := stagingOwnershipStatusProbe(domainDst)
	if probeErr == nil && busy {
		// A PROVEN live writer owns this staging file.
		return BlobDownloading, nil, nil
	}
	if probeErr != nil || !stagingOSLockAvailable {
		// The ownership state is UNKNOWN: the probe failed, or this platform
		// cannot represent the lifetime lock. Neither a live owner nor a dead
		// owner is proven. The conservative reading ("somebody owns it") would
		// be an unproven claim, and it becomes a successful skip with the blob
		// still missing - the exact false completion this check must not
		// report. Report an actionable error instead; the incomplete payload
		// is left untouched so a retry can resume it.
		meta, _ := r.readIncompleteMeta(metaPath)
		cause := probeErr
		if cause == nil {
			cause = errStagingOSLockUnsupported
		}
		return BlobOwnershipUnknown, meta, fmt.Errorf("%w: cannot determine the owner of the incomplete download %s: %w",
			ErrBlobOwnershipUnknown, incompletePath, cause)
	}

	// Read metadata
	meta, err := r.readIncompleteMeta(metaPath)
	if err != nil {
		// If we can't read meta, treat as stale
		return BlobStale, nil, nil
	}

	// A current-version record plus a free lifetime lock proves the owner is
	// gone. An unfenced record is resumable only when same-host process
	// evidence proves owner death; age or remote/ambiguous identity is not proof.
	if meta.LockFenced {
		return BlobStale, meta, nil
	}
	if stagingOwnerProvablyDead(meta) {
		return BlobStale, meta, nil
	}
	return BlobDownloading, meta, nil
}

// readIncompleteMeta reads the metadata file for an incomplete download.
func (r *RepoDir) readIncompleteMeta(path string) (*IncompleteMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta IncompleteMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// CleanupIncomplete removes the .incomplete.meta and .incomplete files. The
// metadata is removed first so a crash cannot leave a record claiming bytes
// whose payload no longer exists.
func (r *RepoDir) CleanupIncomplete(sha256 string) error {
	var errs []error
	if err := os.Remove(r.IncompleteMetaPath(sha256)); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Do not delete the payload if its record could not be removed:
		// that would leave a record claiming bytes that no longer exist.
		errs = append(errs, err)
	} else if err := os.Remove(r.IncompletePath(sha256)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("cleanup incomplete: %v", errs)
	}
	return nil
}

// FinalizeBlob moves a completed .incomplete file to its final blob location.
//
// The publication participates in the blob key's ownership domain and runs
// through the shared no-replace placement primitive, exactly like the ranged
// publish and the cache store. An existing content-addressed blob is
// therefore NEVER replaced: its bytes are the completion proof, the
// .incomplete payload is left untouched for the caller to discard, and the
// call reports the conflict instead of silently overwriting proven bytes.
func (r *RepoDir) FinalizeBlob(sha256 string) error {
	// The blob key is chosen from a caller-provided value, so the boundary
	// that accepts it refuses a name that belongs to the downloader's staging
	// namespace: such a blob would be invisible to every content surface.
	// The refusal happens BEFORE anything is created or moved.
	if err := checkPublishableBlobName(sha256); err != nil {
		return err
	}

	incompletePath := r.IncompletePath(sha256)
	blobPath := r.BlobPath(sha256)

	// The blobs directory must exist before the domain's lock file can be
	// created next to the destination.
	if err := os.MkdirAll(filepath.Dir(blobPath), 0755); err != nil {
		return fmt.Errorf("create blobs directory: %w", err)
	}

	// The publication runs inside the blob key's ownership domain, so it
	// cannot race another writer of the same key.
	dst, err := r.blobDomainDst(sha256)
	if err != nil {
		return fmt.Errorf("resolve the ownership domain of blob %s: %w", sha256, err)
	}
	domain, err := blobDomainAcquire(context.Background(), blobPath, dst)
	if err != nil {
		return fmt.Errorf("acquire the ownership domain of blob %s: %w", sha256, err)
	}
	defer domain.release()

	// Move incomplete to final location without replacing an existing blob.
	if err := blobPublishPlace(incompletePath, blobPath, domain); err != nil {
		return fmt.Errorf("finalize blob %s: %w", sha256, err)
	}

	// Remove metadata file
	_ = os.Remove(r.IncompleteMetaPath(sha256))

	return nil
}

// FriendlyPath returns the path in the friendly view for this repo.
// Example: ~/.cache/huggingface/models/TheBloke/Mistral-7B-Instruct-v0.2-GGUF
func (r *RepoDir) FriendlyPath() string {
	if r.repoType == RepoTypeDataset {
		return filepath.Join(r.cache.DatasetsDir(), r.owner, r.name)
	}
	return filepath.Join(r.cache.ModelsDir(), r.owner, r.name)
}

// RepoID returns the repository ID in owner/name format.
func (r *RepoDir) RepoID() string {
	return r.owner + "/" + r.name
}

// Owner returns the repository owner.
func (r *RepoDir) Owner() string {
	return r.owner
}

// Name returns the repository name.
func (r *RepoDir) Name() string {
	return r.name
}

// Type returns the repository type (model or dataset).
func (r *RepoDir) Type() RepoType {
	return r.repoType
}

// --- Refs Management ---

// WriteRef writes a commit hash to a ref file.
// Example: WriteRef("main", "a1b2c3d4...") writes to refs/main
func (r *RepoDir) WriteRef(ref, commit string) error {
	refPath, err := r.RefPath(ref)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(refPath), 0755); err != nil {
		return fmt.Errorf("create refs directory: %w", err)
	}
	return os.WriteFile(refPath, []byte(commit), 0644)
}

// ReadRef reads the commit hash from a ref file.
// Returns empty string if the ref doesn't exist.
func (r *RepoDir) ReadRef(ref string) (string, error) {
	refPath, err := r.RefPath(ref)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(refPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// --- Snapshots Management ---

// SnapshotFile represents a file in a snapshot with its blob hash.
type SnapshotFile struct {
	// RelativePath is the path within the repo (e.g., "config.json" or "subdir/file.txt")
	RelativePath string
	// SHA256 is the blob hash for this file
	SHA256 string
}

// CreateSnapshot creates or updates a snapshot directory with entries
// pointing at blobs. Entries use relative symlinks for portability and fall
// back to hard links and copies where symlinks are unavailable.
func (r *RepoDir) CreateSnapshot(commit string, files []SnapshotFile) error {
	snapshotDir, err := r.SnapshotDir(commit)
	if err != nil {
		return err
	}

	// Create snapshot directory
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}

	for _, f := range files {
		if err := r.createSnapshotSymlink(context.Background(), commit, f.RelativePath, f.SHA256); err != nil {
			return fmt.Errorf("create symlink for %s: %w", f.RelativePath, err)
		}
	}

	return nil
}

// createSnapshotSymlink creates a single snapshot entry for a blob.
// The symlink form uses relative symlinks: snapshots/{commit}/{path} -> ../../blobs/{sha256}
// Where symlinks are unavailable it falls back to a hard link and then a
// copy, so the snapshot entry is always a usable file.
func (r *RepoDir) createSnapshotSymlink(ctx context.Context, commit, relativePath, sha256 string) error {
	// The blob name is used verbatim as a file name under blobs/ for the
	// hardlink/copy source, so reject empty and non-local keys up front
	// instead of resolving them onto BlobPath's contained placeholder.
	if unsafeBlobFileName(sha256) {
		return fmt.Errorf("invalid blob name %q", sha256)
	}

	// Validate linkPath stays inside the snapshot dir to prevent path traversal.
	linkPath, err := r.SnapshotPath(commit, relativePath)
	if err != nil {
		return fmt.Errorf("symlink path %q would escape snapshot directory: %w", relativePath, err)
	}

	// Create parent directories if needed (for nested paths like "subdir/file.txt")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	// Calculate relative path from link location to blob from the normalized
	// link location, not from the raw wire-form relativePath: SafeJoin/Clean
	// above may have collapsed "a/../", "./", or "//" segments that are
	// allowed by unsafeRepoPath, and counting separators in the raw string
	// would then emit too many "../" components and leave a dangling
	// symlink (os.Symlink succeeds, so the hardlink/copy fallback never
	// runs). Both paths sit under the same Hub dir, so Rel is not expected
	// to fail; if it ever does (e.g. a cross-volume Hub layout), fall back
	// to the absolute blob path so the symlink step of the placement
	// cascade stays reachable and a target-computation detail can neither
	// drop the entry nor fail the download.
	blobPath := r.BlobPath(sha256)
	target := blobPath
	if rel, err := relPathFn(filepath.Dir(linkPath), blobPath); err == nil {
		target = rel
	}

	// Leave an already-correct entry untouched: re-placing it would churn a
	// full copy under the copy fallback and briefly remove a good entry.
	correct, cerr := cacheEntryCorrect(ctx, linkPath, blobPath, target)
	if cerr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(cerr, context.Canceled) || errors.Is(cerr, context.DeadlineExceeded) {
			return cerr
		}
	} else if correct {
		return nil
	}

	// Remove existing entry if it exists
	if _, err := os.Lstat(linkPath); err == nil {
		if err := os.Remove(linkPath); err != nil {
			return fmt.Errorf("remove existing symlink: %w", err)
		}
	}

	// Create the entry: symlink -> hard link -> copy.
	if _, err := r.placeCacheEntry(ctx, r.cache.HubDir(), linkPath, blobPath, target); err != nil {
		return fmt.Errorf("create symlink: %w", err)
	}

	return nil
}

// SnapshotPath returns the path to a file within a snapshot.
func (r *RepoDir) SnapshotPath(commit, relativePath string) (string, error) {
	dir, err := r.SnapshotDir(commit)
	if err != nil {
		return "", err
	}
	return SafeJoin(dir, relativePath)
}

// ListSnapshots returns all commit hashes that have snapshots.
func (r *RepoDir) ListSnapshots() ([]string, error) {
	entries, err := os.ReadDir(r.SnapshotsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var commits []string
	for _, e := range entries {
		if e.IsDir() {
			commits = append(commits, e.Name())
		}
	}
	return commits, nil
}

// --- Friendly View Management ---

// CreateFriendlySymlink creates an entry in the friendly view pointing to a
// snapshot file. Uses relative symlinks for portability, falling back to a
// hard link and then a copy where symlinks are unavailable.
// filterSubdir is optional - if provided, creates the entry in a subdirectory (e.g., "q4_k_m")
func (r *RepoDir) CreateFriendlySymlink(commit, relativePath, filterSubdir string) error {
	return r.createFriendlySymlinkCtx(context.Background(), commit, relativePath, filterSubdir)
}

// createFriendlySymlinkCtx is the context-bounded implementation used by the
// downloader and sync paths; the existing public context-free API remains
// unchanged.
func (r *RepoDir) createFriendlySymlinkCtx(ctx context.Context, commit, relativePath, filterSubdir string) error {
	// Build the (optionally filtered) base safely first, THEN join relativePath
	// onto it. Joining filterSubdir and relativePath together first would let a
	// "../" in relativePath cancel the filter subdir before validation.
	base := r.FriendlyPath()
	if filterSubdir != "" {
		filtered, err := SafeJoin(base, filterSubdir)
		if err != nil {
			return fmt.Errorf("invalid filter subdir %q: %w", filterSubdir, err)
		}
		base = filtered
	}
	linkPath, err := SafeJoin(base, relativePath)
	if err != nil {
		return fmt.Errorf("symlink path %q would escape friendly view base: %w", relativePath, err)
	}

	// Create parent directories
	if err := os.MkdirAll(filepath.Dir(linkPath), 0755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	// Calculate the symlink target from link location to snapshot
	// Need to go from: models/{owner}/{name}/[filterSubdir/]{relativePath}
	// To:              hub/models--{owner}--{name}/snapshots/{commit}/{relativePath}
	// A relative target is used for portability; when Hub and friendly roots
	// are on different volumes the two paths cannot be related and the
	// absolute snapshot path is the target instead (see friendlyLinkTarget).
	snapshotPath, err := r.SnapshotPath(commit, relativePath)
	if err != nil {
		return fmt.Errorf("resolve snapshot path: %w", err)
	}
	target := friendlyLinkTarget(filepath.Dir(linkPath), snapshotPath)

	// Leave an already-correct entry untouched: re-placing it would churn a
	// full copy under the copy fallback and briefly remove a good entry.
	correct, cerr := cacheEntryCorrect(ctx, linkPath, snapshotPath, target)
	if cerr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(cerr, context.Canceled) || errors.Is(cerr, context.DeadlineExceeded) {
			return cerr
		}
	} else if correct {
		return nil
	}

	// Remove existing entry if it exists
	if _, err := os.Lstat(linkPath); err == nil {
		if err := os.Remove(linkPath); err != nil {
			return fmt.Errorf("remove existing symlink: %w", err)
		}
	}

	// Create the entry: symlink -> hard link -> copy. The source is the
	// snapshot entry, so a fallback entry mirrors the snapshot content even
	// when the two directories live on different volumes (the hardlink step
	// then fails per-volume and the copy step applies).
	if _, err := r.placeCacheEntry(ctx, r.friendlyLinkRoot(), linkPath, snapshotPath, target); err != nil {
		return fmt.Errorf("create symlink: %w", err)
	}

	return nil
}

// friendlyLinkRoot is the placement root (and fallback-memo key) for friendly
// view entries: the friendly tree root of this repo's kind, which sits on the
// volume friendly entries are created on.
func (r *RepoDir) friendlyLinkRoot() string {
	if r.repoType == RepoTypeDataset {
		return r.cache.DatasetsDir()
	}
	return r.cache.ModelsDir()
}

// EnsureFriendlyDir creates the friendly view directory for this repo.
func (r *RepoDir) EnsureFriendlyDir() error {
	return os.MkdirAll(r.FriendlyPath(), 0755)
}

// --- Download Workflow Helpers ---

// StoreFileResult contains the result of storing a downloaded file in the cache.
type StoreFileResult struct {
	BlobPath     string // Path to the blob file
	SnapshotPath string // Path to the snapshot symlink
	FriendlyPath string // Path to the friendly view symlink
	SHA256       string // The SHA256 hash used for the blob
}

// StoreDownloadedFile moves a downloaded file into the HF cache structure.
// It handles:
//  1. Computing SHA256 if not provided (for non-LFS files)
//  2. Moving/copying the file to blobs/{sha256}
//  3. Creating snapshot symlink
//  4. Creating friendly view symlink
//
// Parameters:
//   - tempFile: path to the downloaded file (will be moved/removed)
//   - relativePath: the file's path within the repo (e.g., "config.json")
//   - commit: the commit hash for the snapshot
//   - sha256: the known SHA256 (empty string if unknown, will be computed)
//   - filterSubdir: optional filter subdirectory for friendly view
//   - noFriendly: if true, skip creating friendly view symlink
func (r *RepoDir) StoreDownloadedFile(tempFile, relativePath, commit, sha256, filterSubdir string, noFriendly bool) (*StoreFileResult, error) {
	return r.storeDownloadedFileCtx(context.Background(), tempFile, relativePath, commit, sha256, filterSubdir, noFriendly)
}

// storeDownloadedFileCtx is the context-bounded implementation used by the
// downloader; the existing public context-free API remains unchanged.
func (r *RepoDir) storeDownloadedFileCtx(ctx context.Context, tempFile, relativePath, commit, sha256, filterSubdir string, noFriendly bool) (*StoreFileResult, error) {
	return r.storeDownloadedFileInDomain(ctx, tempFile, relativePath, commit, sha256, filterSubdir, noFriendly, nil)
}

// storeDownloadedFileInDomain is storeDownloadedFileCtx with an explicit
// ownership domain.
//
// The domain is the blob key's single ownership domain (see blob_domain.go).
// The downloader already holds it for the whole operation on that key and
// passes it in; a direct caller passes nil and the store takes the domain
// itself. The publication therefore always happens inside the domain, so the
// "the blob does not exist yet" decision and the placement that follows it
// cannot be overtaken by another writer of the same key.
func (r *RepoDir) storeDownloadedFileInDomain(ctx context.Context, tempFile, relativePath, commit, sha256, filterSubdir string, noFriendly bool, domain *blobDomain) (*StoreFileResult, error) {
	// Compute SHA256 if not provided
	if sha256 == "" {
		computed, err := computeSHA256Ctx(ctx, tempFile)
		if err != nil {
			return nil, fmt.Errorf("compute sha256: %w", err)
		}
		sha256 = computed
	}

	// The blob key is chosen from a caller-provided value, so the boundary
	// that accepts it refuses a name that belongs to the downloader's staging
	// namespace: such a blob would be invisible to every content surface (the
	// statistics, the listing, the selection, the mirror and the deletion
	// path all exclude staging-shaped names). The store fails BEFORE it
	// places anything, so no payload is moved and no entry is created.
	if err := checkPublishableBlobName(sha256); err != nil {
		return nil, err
	}

	blobPath := r.BlobPath(sha256)

	// The blobs directory must exist before the domain's lock file can be
	// created next to the destination.
	if err := os.MkdirAll(filepath.Dir(blobPath), 0755); err != nil {
		return nil, fmt.Errorf("create blobs directory: %w", err)
	}

	// The publication runs inside the blob key's ownership domain.
	if domain == nil || !domain.holdsBlobKey(blobPath) {
		dst, derr := r.blobDomainDst(sha256)
		if derr != nil {
			return nil, fmt.Errorf("resolve the ownership domain of blob %s: %w", sha256, derr)
		}
		d, derr := blobDomainAcquire(ctx, blobPath, dst)
		if derr != nil {
			return nil, fmt.Errorf("acquire the ownership domain of blob %s: %w", sha256, derr)
		}
		defer d.release()
		domain = d
	}

	// Place the payload into the blob namespace. The placement never replaces
	// an existing blob: a content-addressed blob that already exists is
	// completion proof, so a concurrent publisher's bytes are not overwritten
	// by ours (and ours are not published over theirs).
	if err := blobPublishPlace(tempFile, blobPath, domain); err != nil {
		switch {
		case errors.Is(err, errBlobTargetExists):
			// Another writer published this blob first. Its bytes are the
			// completion proof; this payload is discarded.
			os.Remove(tempFile)
		case errors.Is(err, errBlobPublicationDomainRequired):
			// The placement cannot prove that it will not replace an
			// existing blob, and the copy fallback below is no stronger.
			// The operation fails instead of guessing.
			return nil, fmt.Errorf("move file to blob: %w", err)
		default:
			// The payload could not be placed by a link or a rename (a
			// cross-device move, for example). Fall back to an atomic copy:
			// stage in a sibling temp file, then atomically rename it into
			// place. The copy is exclusive to the blob key's ownership
			// domain this store holds, so no domain participant can create
			// the target between the existence check and the copy. A
			// cancelled or failed copy must never leave a partial file at
			// the FINAL blob path (which CheckBlob would report complete,
			// poisoning the cache).
			if _, statErr := os.Stat(blobPath); statErr == nil {
				// The blob appeared in the meantime: it is the completion
				// proof, so this payload is discarded.
				os.Remove(tempFile)
			} else {
				if cerr := copyFileAtomicCtx(ctx, tempFile, blobPath); cerr != nil {
					return nil, fmt.Errorf("move file to blob: %w", cerr)
				}
				os.Remove(tempFile)
			}
		}
	}

	// Create the snapshot entry and, unless the friendly view is disabled,
	// the friendly entry. They are created through the SAME helper the skip
	// paths use, so a link-creation failure carries the exported
	// ErrCacheLinkUnreachable sentinel on every path that creates the cache
	// entries: the payload may be in blobs/, but the requested path is not
	// reachable through it, and the caller must not report the item as
	// successful. The friendly entry stays deliberately absent when the
	// friendly view is disabled, which is not an error.
	if err := cacheEntryLinkCreate(ctx, r, commit, relativePath, filterSubdir, sha256, noFriendly); err != nil {
		return nil, err
	}

	snapPath, err := r.SnapshotPath(commit, relativePath)
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot path: %w", err)
	}
	result := &StoreFileResult{
		BlobPath:     blobPath,
		SnapshotPath: snapPath,
		SHA256:       sha256,
	}
	if !noFriendly {
		if filterSubdir != "" {
			result.FriendlyPath = filepath.Join(r.FriendlyPath(), filterSubdir, relativePath)
		} else {
			result.FriendlyPath = filepath.Join(r.FriendlyPath(), relativePath)
		}
	}

	return result, nil
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// copyFileAtomicCtx copies src to dst atomically: it streams into a sibling
// temporary file in the same directory, closes it, then renames it onto dst. On
// ctx cancellation or any error it removes the staging file and leaves both src
// intact and dst absent/unchanged, so a cancelled copy can never leave a partial
// file at dst. It is the publish variant used for cache blobs, where a partial
// file at the final path would be mistaken for a complete blob.
//
// The staged bytes are copied to their final mode (matching what the same-device
// rename path publishes) so a cross-device fallback does not change blob
// permissions.
//
// This provides atomic visibility, not crash durability: it does not fsync the
// staged file or containing directory.
func copyFileAtomicCtx(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// Stage in the destination's directory so the final rename is atomic
	// (same filesystem).
	staging, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	stagingPath := staging.Name()
	// Clean up the staging file on every non-success path. Once the rename
	// succeeds the staging path no longer exists, so os.Remove is a no-op.
	defer os.Remove(stagingPath)

	if _, err := io.Copy(staging, contextReader{ctx: ctx, r: in}); err != nil {
		staging.Close()
		return err
	}

	// Match the mode the same-device rename path publishes. The downloader
	// creates the source temp at 0o644, but os.CreateTemp stages at 0600, so
	// without this a cross-device copy would make blobs owner-only and break a
	// shared HF cache (EACCES for other users/containers). Best-effort read of
	// the source mode, falling back to 0644.
	mode := os.FileMode(0o644)
	if fi, statErr := os.Stat(src); statErr == nil {
		mode = fi.Mode().Perm()
	}
	if err := staging.Chmod(mode); err != nil {
		staging.Close()
		return err
	}
	if err := staging.Close(); err != nil {
		return err
	}
	return os.Rename(stagingPath, dst)
}
