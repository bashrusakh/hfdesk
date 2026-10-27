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
}

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
func NewHFCache(root string) *HFCache {
	if root == "" {
		root = DefaultCacheDir()
	}
	return NewHFCacheResolved(root, os.Getenv("HF_HUB_CACHE"))
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
func NewHFCacheResolved(root, hubDir string) *HFCache {
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
	return &HFCache{
		Root:   root,
		hubDir: hubDir,
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

// errReservedBlobName identifies a caller-provided blob key in the staging
// namespace. Non-reserved unusual names remain accepted.
var errReservedBlobName = errors.New("blob name belongs to the downloader's staging namespace")

func checkPublishableBlobName(name string) error {
	if IsStagingArtifactName(name) {
		return fmt.Errorf("refuse blob name %q: %w; a published blob may not use one of the downloader's staging artifact names", name, errReservedBlobName)
	}
	return nil
}

// BlobPath returns the contained path for a blob key. Invalid/non-local names
// map to a fixed placeholder because this method cannot return an error;
// scanRepo validates remote SHAs before calling it.
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

// IncompleteMeta records transfer identity, ownership, and resumable range
// state. Only committed range prefixes count as received bytes; see doc.go for
// the version, fencing, and recovery contract.
type IncompleteMeta struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	// Instance is a per-process token; PID reuse or shared container PIDs do not imply the same owner.
	Instance string `json:"instance,omitempty"`
	// Host permits same-host PID liveness checks; an empty or different host disables that evidence.
	Host string `json:"host,omitempty"`
	// LockFenced means the record was created under the destination domain lock.
	// An unfenced record requires matching process-instance identity or same-host
	// owner-death proof before takeover.
	LockFenced bool `json:"lock_fenced,omitempty"`
	// Version identifies the record format; only the current version is resumable.
	Version int    `json:"version,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Path    string `json:"path,omitempty"`
	// CommitPinned says Commit is immutable and resolved; resume requires it on
	// both the record and current plan.
	CommitPinned bool `json:"commit_pinned"`
	// Ranges lists the persisted range layout and per-range committed
	// progress; see RangeProgress. Empty only for the brief ownership-only
	// window before the fresh layout is flushed.
	Ranges []RangeProgress `json:"ranges,omitempty"`
	// ProofPending requires known-digest proof on resume except in Verify="none".
	// When proof is required, the single path leaves its partial unpublished until it succeeds.
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
	// BlobMissing means neither a published blob nor a ranged staging payload exists.
	// A single-request writer may still be active; callers acquire its domain.
	BlobMissing BlobStatus = iota

	// BlobComplete means the content-addressed blob exists.
	BlobComplete

	// BlobDownloading means a live owner holds the domain, or an unfenced
	// record cannot yet be proven stale. It is not a successful skip.
	BlobDownloading

	// BlobStale means no live owner was proven. An unreadable record can be
	// reported stale conservatively; the claim still refuses it as unproven.
	BlobStale

	// BlobOwnershipUnknown means the ownership probe failed or the lock is
	// unsupported. CheckBlob returns ErrBlobOwnershipUnknown; callers must not
	// skip the item.
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

// CheckBlob returns published-blob status and available ranged metadata.
// If no blob exists, an .incomplete payload triggers a destination-lock probe:
// exclusive ownership is BlobDownloading; probe failure returns BlobOwnershipUnknown
// with an error. BlobStale alone does not prove resumability; the claim checks it.
// With neither blob nor staged payload it returns BlobMissing, even if a single
// writer holds the domain. Callers still acquire the domain before work.
func (r *RepoDir) CheckBlob(sha256 string) (BlobStatus, *IncompleteMeta, error) {
	blobPath := r.BlobPath(sha256)
	incompletePath := r.IncompletePath(sha256)
	metaPath := r.IncompleteMetaPath(sha256)

	if _, err := os.Stat(blobPath); err == nil {
		return BlobComplete, nil, nil
	}

	_, err := os.Stat(incompletePath)
	if errors.Is(err, os.ErrNotExist) {
		return BlobMissing, nil, nil
	}
	if err != nil {
		return BlobMissing, nil, fmt.Errorf("stat incomplete file: %w", err)
	}

	// Probe the persistent destination domain, not the obsolete staging lock.
	// The non-blocking probe covers suspended and in-process writers.
	domainDst, derr := r.blobDomainDst(sha256)
	if derr != nil {
		meta, _ := r.readIncompleteMeta(metaPath)
		return BlobOwnershipUnknown, meta, fmt.Errorf("%w: cannot resolve the ownership domain of the incomplete download %s: %w",
			ErrBlobOwnershipUnknown, incompletePath, derr)
	}
	busy, probeErr := stagingOwnershipStatusProbe(domainDst)
	if probeErr == nil && busy {
		return BlobDownloading, nil, nil
	}
	if probeErr != nil || !stagingOSLockAvailable {
		meta, _ := r.readIncompleteMeta(metaPath)
		cause := probeErr
		if cause == nil {
			cause = errStagingOSLockUnsupported
		}
		return BlobOwnershipUnknown, meta, fmt.Errorf("%w: cannot determine the owner of the incomplete download %s: %w",
			ErrBlobOwnershipUnknown, incompletePath, cause)
	}

	meta, err := r.readIncompleteMeta(metaPath)
	if err != nil {
		return BlobStale, nil, nil
	}

	// A free lock proves death only for fenced records; otherwise require
	// same-host process evidence.
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

// FinalizeBlob publishes a completed .incomplete payload under the blob key's domain
// through the shared no-replace primitive. A conflict preserves both the existing
// blob and the .incomplete payload and returns an error.
func (r *RepoDir) FinalizeBlob(sha256 string) error {
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

	dst, err := r.blobDomainDst(sha256)
	if err != nil {
		return fmt.Errorf("resolve the ownership domain of blob %s: %w", sha256, err)
	}
	domain, err := blobDomainAcquire(context.Background(), blobPath, dst)
	if err != nil {
		return fmt.Errorf("acquire the ownership domain of blob %s: %w", sha256, err)
	}
	defer domain.release()

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

// StoreDownloadedFile moves a completed temp into blobs (computing SHA-256 if
// omitted) and creates snapshot and optional friendly entries. It returns the
// resulting blob path and digest; the temp is consumed after successful store.
// relativePath and commit identify the snapshot; filterSubdir is the optional
// friendly-view prefix, and noFriendly disables that view.
func (r *RepoDir) StoreDownloadedFile(tempFile, relativePath, commit, sha256, filterSubdir string, noFriendly bool) (*StoreFileResult, error) {
	return r.storeDownloadedFileCtx(context.Background(), tempFile, relativePath, commit, sha256, filterSubdir, noFriendly)
}

// storeDownloadedFileCtx is the context-bounded implementation used by the
// downloader; the existing public context-free API remains unchanged.
func (r *RepoDir) storeDownloadedFileCtx(ctx context.Context, tempFile, relativePath, commit, sha256, filterSubdir string, noFriendly bool) (*StoreFileResult, error) {
	return r.storeDownloadedFileInDomain(ctx, tempFile, relativePath, commit, sha256, filterSubdir, noFriendly, nil)
}

// storeDownloadedFileInDomain reuses a matching blob-key domain or acquires the required one.
func (r *RepoDir) storeDownloadedFileInDomain(ctx context.Context, tempFile, relativePath, commit, sha256, filterSubdir string, noFriendly bool, domain *blobDomain) (*StoreFileResult, error) {
	// Compute SHA256 if not provided
	if sha256 == "" {
		computed, err := computeSHA256Ctx(ctx, tempFile)
		if err != nil {
			return nil, fmt.Errorf("compute sha256: %w", err)
		}
		sha256 = computed
	}

	if err := checkPublishableBlobName(sha256); err != nil {
		return nil, err
	}

	blobPath := r.BlobPath(sha256)

	if err := os.MkdirAll(filepath.Dir(blobPath), 0755); err != nil {
		return nil, fmt.Errorf("create blobs directory: %w", err)
	}

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

	if err := blobPublishPlace(tempFile, blobPath, domain); err != nil {
		switch {
		case errors.Is(err, errBlobTargetExists):
			// Another writer's blob is the completion proof.
			os.Remove(tempFile)
		case errors.Is(err, errBlobPublicationDomainRequired):
			// Fail closed when placement cannot preserve the no-replace guarantee.
			return nil, fmt.Errorf("move file to blob: %w", err)
		default:
			// Stage cross-device copies beside the target and rename only on
			// success, so a failed/cancelled copy cannot expose a partial blob.
			if _, statErr := os.Stat(blobPath); statErr == nil {
				os.Remove(tempFile)
			} else {
				if cerr := copyFileAtomicCtx(ctx, tempFile, blobPath); cerr != nil {
					return nil, fmt.Errorf("move file to blob: %w", cerr)
				}
				os.Remove(tempFile)
			}
		}
	}

	// Required cache-link failures propagate as ErrCacheLinkUnreachable; noFriendly omits only the friendly link.
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

// copyFileAtomicCtx copies to a sibling temp and renames only on success, so
// failed/cancelled copies leave dst unchanged and src intact. It preserves the
// source mode. Visibility is atomic, but the operation is not crash-durable.
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
