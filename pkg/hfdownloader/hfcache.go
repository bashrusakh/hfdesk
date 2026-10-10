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
	"syscall"
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
type IncompleteMeta struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
}

// BlobStatus represents the status of a blob in the cache.
type BlobStatus int

const (
	// BlobMissing means the blob doesn't exist and no download is in progress.
	BlobMissing BlobStatus = iota

	// BlobComplete means the blob exists and is complete.
	BlobComplete

	// BlobDownloading means another process is currently downloading this blob.
	BlobDownloading

	// BlobStale means an incomplete download exists but is stale (can be taken over).
	BlobStale
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
	default:
		return "unknown"
	}
}

// CheckBlob checks the status of a blob in the cache.
// Returns the status and any existing incomplete metadata.
func (r *RepoDir) CheckBlob(sha256 string) (BlobStatus, *IncompleteMeta, error) {
	blobPath := r.BlobPath(sha256)
	incompletePath := r.IncompletePath(sha256)
	metaPath := r.IncompleteMetaPath(sha256)

	// Check if complete blob exists
	if _, err := os.Stat(blobPath); err == nil {
		return BlobComplete, nil, nil
	}

	// Check if incomplete download exists
	incompleteStat, err := os.Stat(incompletePath)
	if errors.Is(err, os.ErrNotExist) {
		return BlobMissing, nil, nil
	}
	if err != nil {
		return BlobMissing, nil, fmt.Errorf("stat incomplete file: %w", err)
	}

	// Read metadata
	meta, err := r.readIncompleteMeta(metaPath)
	if err != nil {
		// If we can't read meta, treat as stale
		return BlobStale, nil, nil
	}

	// Check if the process is still alive
	if isProcessAlive(meta.PID) {
		// Check if file was recently modified
		if time.Since(incompleteStat.ModTime()) < r.cache.StaleTimeout {
			return BlobDownloading, meta, nil
		}
	}

	// Process is dead or file is stale
	return BlobStale, meta, nil
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

// WriteIncompleteMeta writes metadata for an incomplete download.
func (r *RepoDir) WriteIncompleteMeta(sha256 string, size int64) error {
	meta := IncompleteMeta{
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC(),
		Size:      size,
		SHA256:    sha256,
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.IncompleteMetaPath(sha256), data, 0644)
}

// CleanupIncomplete removes the .incomplete and .incomplete.meta files.
func (r *RepoDir) CleanupIncomplete(sha256 string) error {
	var errs []error
	if err := os.Remove(r.IncompletePath(sha256)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := os.Remove(r.IncompleteMetaPath(sha256)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("cleanup incomplete: %v", errs)
	}
	return nil
}

// FinalizeBlob moves a completed .incomplete file to its final blob location.
func (r *RepoDir) FinalizeBlob(sha256 string) error {
	incompletePath := r.IncompletePath(sha256)
	blobPath := r.BlobPath(sha256)

	// Move incomplete to final location
	if err := os.Rename(incompletePath, blobPath); err != nil {
		return fmt.Errorf("rename incomplete to blob: %w", err)
	}

	// Remove metadata file
	_ = os.Remove(r.IncompleteMetaPath(sha256))

	return nil
}

// isProcessAlive checks if a process with the given PID is still running.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds. We need to send signal 0 to check.
	err = process.Signal(syscall.Signal(0))
	return err == nil
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

	// Calculate relative path from link location to blob
	// From: snapshots/{commit}/{relativePath}
	// To:   blobs/{sha256}
	// Need: ../../blobs/{sha256} (or more ../ for nested paths)
	// Count slash-separated segments: relativePath arrives in wire form
	// ("subdir/file.txt") from the Hub tree, so counting filepath.Separator
	// would under-count on Windows and produce a dangling target there.
	depth := strings.Count(filepath.ToSlash(relativePath), "/") + 1 // +1 for commit dir
	relPrefix := strings.Repeat("../", depth+1)                     // +1 to get from snapshots/ to repo root
	target := relPrefix + "blobs/" + sha256
	blobPath := r.BlobPath(sha256)

	// Leave an already-correct entry untouched: re-placing it would churn a
	// full copy under the copy fallback and briefly remove a good entry.
	if correct, cerr := cacheEntryCorrect(linkPath, blobPath, target); cerr == nil && correct {
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

	// Calculate relative path from link location to snapshot
	// Need to go from: models/{owner}/{name}/[filterSubdir/]{relativePath}
	// To:              hub/models--{owner}--{name}/snapshots/{commit}/{relativePath}
	snapshotPath, err := r.SnapshotPath(commit, relativePath)
	if err != nil {
		return fmt.Errorf("resolve snapshot path: %w", err)
	}
	target, err := filepath.Rel(filepath.Dir(linkPath), snapshotPath)
	if err != nil {
		return fmt.Errorf("calculate relative path: %w", err)
	}

	// Leave an already-correct entry untouched: re-placing it would churn a
	// full copy under the copy fallback and briefly remove a good entry.
	if correct, cerr := cacheEntryCorrect(linkPath, snapshotPath, target); cerr == nil && correct {
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
	// when the two directories live on different volumes.
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
	// Compute SHA256 if not provided
	if sha256 == "" {
		computed, err := computeSHA256Ctx(ctx, tempFile)
		if err != nil {
			return nil, fmt.Errorf("compute sha256: %w", err)
		}
		sha256 = computed
	}

	blobPath := r.BlobPath(sha256)

	// Check if blob already exists (deduplication)
	if _, err := os.Stat(blobPath); err == nil {
		// Blob exists, just remove temp file
		os.Remove(tempFile)
	} else {
		// Move temp file to blob location
		if err := os.MkdirAll(filepath.Dir(blobPath), 0755); err != nil {
			return nil, fmt.Errorf("create blobs directory: %w", err)
		}
		if err := os.Rename(tempFile, blobPath); err != nil {
			// Rename failed (cross-device?), try an atomic copy: stage in a
			// sibling temp file, then atomically rename it into place. A cancelled or
			// failed copy must never leave a partial file at the FINAL blob
			// path (which CheckBlob would report complete, poisoning the cache).
			if err := copyFileAtomicCtx(ctx, tempFile, blobPath); err != nil {
				return nil, fmt.Errorf("move file to blob: %w", err)
			}
			os.Remove(tempFile)
		}
	}

	// Create snapshot entry
	if err := r.createSnapshotSymlink(ctx, commit, relativePath, sha256); err != nil {
		return nil, fmt.Errorf("create snapshot symlink: %w", err)
	}

	// Create friendly view entry (unless disabled)
	if !noFriendly {
		if err := r.createFriendlySymlinkCtx(ctx, commit, relativePath, filterSubdir); err != nil {
			return nil, fmt.Errorf("create friendly symlink: %w", err)
		}
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
