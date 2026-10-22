// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ManifestFilename is the standard name for manifest files.
const ManifestFilename = "hfd.yaml"

// DownloadManifest captures the complete record of a download job.
// This file is written to the friendly view root (e.g., models/Owner/Repo/hfd.yaml)
// and contains all information needed to understand and reproduce the download.
type DownloadManifest struct {
	// Metadata
	Version string `yaml:"version" json:"version"` // Manifest format version

	// Repository info
	Type           string `yaml:"type" json:"type"`     // "model" or "dataset"
	Repo           string `yaml:"repo" json:"repo"`     // "owner/name"
	Branch         string `yaml:"branch" json:"branch"` // Branch name (usually "main")
	Commit         string `yaml:"commit" json:"commit"` // Commit hash
	BranchVariant  bool   `yaml:"branch_variant,omitempty" json:"branchVariant,omitempty"`
	CommitResolved bool   `yaml:"commit_resolved,omitempty" json:"commitResolved,omitempty"`
	RepoPath       string `yaml:"repo_path" json:"repo_path"` // Relative to app root; absolute if Hub is on another volume.

	// Timing
	StartedAt   time.Time `yaml:"started_at" json:"started_at"`     // When download started
	CompletedAt time.Time `yaml:"completed_at" json:"completed_at"` // When download completed

	// CLI command used (token stripped for security)
	Command string `yaml:"command" json:"command"`

	// Results
	TotalSize  int64          `yaml:"total_size" json:"total_size"`   // Total bytes downloaded
	TotalFiles int            `yaml:"total_files" json:"total_files"` // Number of files
	Files      []ManifestFile `yaml:"files" json:"files"`

	// Deleted records snapshot names intentionally removed after the download
	// (selected-GGUF deletion). Rebuild/repair must never recreate them from
	// the file mapping above: a retained payload does not imply consent to
	// restore a deleted entry. Absent on manifests written before this field
	// existed (old readers ignore it, old manifests simply have no tombstones).
	Deleted []ManifestDeletion `yaml:"deleted,omitempty" json:"deleted,omitempty"`
}

// ManifestDeletion tombstones one intentionally removed repo-relative name,
// bound to the snapshot versions it was removed from. An empty Versions list
// means every version of that name.
type ManifestDeletion struct {
	Path     string   `yaml:"path" json:"path"`
	Versions []string `yaml:"versions,omitempty" json:"versions,omitempty"`
}

// ManifestFile represents a single file in the manifest.
// The Blob field contains the relative path from the repo directory to the blob,
// making it easy for automation scripts to locate and copy files.
type ManifestFile struct {
	Name string `yaml:"name" json:"name"` // Original filename (e.g., "model.Q4_K_M.gguf")
	Blob string `yaml:"blob" json:"blob"` // Relative path to blob (e.g., "blobs/abc123...")
	Size int64  `yaml:"size" json:"size"` // File size in bytes
	LFS  bool   `yaml:"lfs" json:"lfs"`   // Is LFS file
}

// ManifestBuilder helps construct a manifest during download.
type ManifestBuilder struct {
	manifest  *DownloadManifest
	startTime time.Time
}

// NewManifestBuilder creates a new manifest builder for a download job.
func NewManifestBuilder(job Job, command string) *ManifestBuilder {
	repoType := "model"
	if job.IsDataset {
		repoType = "dataset"
	}

	branch := job.Revision
	if branch == "" {
		branch = "main"
	}

	// Build repo path (relative to cache root)
	repoPath := "hub/" + RepoTypeName(job.IsDataset) + "--" + strings.ReplaceAll(job.Repo, "/", "--")

	return &ManifestBuilder{
		startTime: time.Now().UTC(),
		manifest: &DownloadManifest{
			Version:       "1.0",
			Type:          repoType,
			Repo:          job.Repo,
			Branch:        branch,
			BranchVariant: job.BranchVariant,
			RepoPath:      repoPath,
			StartedAt:     time.Now().UTC(),
			Command:       command,
			Files:         []ManifestFile{},
		},
	}
}

// SetCommit sets the commit hash.
func (b *ManifestBuilder) SetCommit(commit string) {
	b.manifest.Commit = commit
}

// SetCommitResolved records whether Commit is the concrete SHA returned by the
// Hub at the start of a branch-variant download rather than a revision fallback.
func (b *ManifestBuilder) SetCommitResolved(resolved bool) {
	b.manifest.CommitResolved = resolved
}

// AddFile adds a file to the manifest.
func (b *ManifestBuilder) AddFile(name, blobHash string, size int64, isLFS bool) {
	b.manifest.Files = append(b.manifest.Files, ManifestFile{
		Name: name,
		Blob: "blobs/" + blobHash,
		Size: size,
		LFS:  isLFS,
	})
	b.manifest.TotalSize += size
	b.manifest.TotalFiles++
}

// Build finalizes the manifest with completion time.
func (b *ManifestBuilder) Build() *DownloadManifest {
	b.manifest.CompletedAt = time.Now().UTC()
	return b.manifest
}

// Write writes the manifest to the friendly view directory.
// The write is staged in a sibling temporary file and then renamed into
// place, so a staging failure cannot truncate a previously good manifest:
// the manifest is load-bearing state for deletion durability and cache repair.
// Same-directory rename provides atomic visibility on platforms/filesystems
// that support it; Go does not guarantee atomic Rename on non-Unix platforms.
// The staging file is not fsynced, so this does not provide crash durability.
func (m *DownloadManifest) Write(friendlyPath string) (string, error) {
	manifestPath := filepath.Join(friendlyPath, ManifestFilename)
	release, err := acquireBranchVariantEntryMutationLease(manifestPath)
	if err != nil {
		return "", fmt.Errorf("manifest write conflicts with app-owned branch variant storage: %w", err)
	}
	defer release()

	// Ensure directory exists
	if err := os.MkdirAll(friendlyPath, 0o755); err != nil {
		return "", fmt.Errorf("create manifest dir: %w", err)
	}

	data, err := yaml.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal manifest: %w", err)
	}

	// Add header comment
	header := fmt.Sprintf("# HuggingFace %s: %s\n", m.Type, m.Repo)
	header += fmt.Sprintf("# Downloaded: %s\n", m.CompletedAt.Format(time.RFC3339))
	header += "# Generated by hfdesk\n"
	header += "# Run 'hfdesk' and open the Local Cache tab to browse downloaded repos\n\n"
	content := header + string(data)

	if err := writeFileAtomic(manifestPath, []byte(content)); err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}

	return manifestPath, nil
}

// WriteBranchVariant writes the durable per-variant manifest as JSON. JSON is
// also valid YAML, so the existing ReadManifest API remains compatible; the
// standalone rebuild script can read the additive record with Python's
// standard-library JSON parser without requiring PyYAML.
func (m *DownloadManifest) WriteBranchVariant(dir string) (string, error) {
	manifestPath := filepath.Join(dir, ManifestFilename)
	if err := rejectOwnedCoordinationMutationTargets(false, manifestPath); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create branch variant manifest dir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal branch variant manifest: %w", err)
	}
	if err := writeFileAtomic(manifestPath, data); err != nil {
		return "", fmt.Errorf("write branch variant manifest: %w", err)
	}
	return manifestPath, nil
}

// manifestCreateTemp is the staging seam for writeFileAtomic; tests inject
// failures here to prove a failed manifest write leaves the previous file
// intact.
var manifestCreateTemp = createManifestTemp

// createManifestTemp creates an exclusive sibling staging file using the
// requested permissions. Unlike os.CreateTemp (which always uses 0600),
// OpenFile applies the process umask to 0644 just as the former WriteFile did.
func createManifestTemp(dir, pattern string) (*os.File, error) {
	for range 100 {
		var suffix [12]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, err
		}
		name := filepath.Join(dir, strings.Replace(pattern, "*", hex.EncodeToString(suffix[:]), 1))
		file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("create manifest staging file: too many name collisions")
}

// writeFileAtomic publishes data at path via a staged sibling temporary file
// and a rename. On any failure the staging file is removed and path is left
// exactly as it was, so a failed write cannot truncate an existing file the
// way os.WriteFile would. New-file permissions are filtered through the
// process umask; a rewritten file keeps its existing mode.
func writeFileAtomic(path string, data []byte) error {
	if err := rejectOwnedCoordinationMutationTargets(false, path); err != nil {
		return err
	}
	var mode os.FileMode
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		mode = info.Mode().Perm()
	}

	staging, err := manifestCreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	stagingPath := staging.Name()
	// Clean up the staging file on every non-success path. Once the rename
	// succeeds the staging path no longer exists, so os.Remove is a no-op.
	defer os.Remove(stagingPath)

	if _, err := staging.Write(data); err != nil {
		staging.Close()
		return err
	}
	if mode != 0 {
		if err := staging.Chmod(mode); err != nil {
			staging.Close()
			return err
		}
	}
	if err := staging.Close(); err != nil {
		return err
	}
	return os.Rename(stagingPath, path)
}

// ReadManifest reads a manifest from a file.
func ReadManifest(path string) (*DownloadManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var manifest DownloadManifest
		if err := json.Unmarshal(trimmed, &manifest); err != nil {
			return nil, err
		}
		return &manifest, nil
	}

	var manifest DownloadManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}

	return &manifest, nil
}

// IsCompleteBranchVariant validates the durable identity and expected file set
// recorded for a finished EXL branch artifact. Callers still verify each file
// in the requested physical destination before claiming presence.
func (m *DownloadManifest) IsCompleteBranchVariant(repoID string, repoType RepoType, revision string) bool {
	if m == nil || !m.BranchVariant || !m.CommitResolved || m.Commit == "" || m.Branch != revision ||
		!strings.EqualFold(m.Repo, repoID) || m.Type != string(repoType) ||
		m.CompletedAt.IsZero() || m.TotalFiles <= 0 || len(m.Files) != m.TotalFiles {
		return false
	}
	seen := make(map[string]struct{}, len(m.Files))
	var totalSize int64
	for _, file := range m.Files {
		name := filepath.ToSlash(file.Name)
		if unsafeRepoPath(name) || name != file.Name || file.Size < 0 {
			return false
		}
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			return false
		}
		seen[key] = struct{}{}
		if !strings.HasPrefix(file.Blob, "blobs/") || !canonicalSHA256(strings.TrimPrefix(file.Blob, "blobs/")) {
			return false
		}
		if totalSize > int64(^uint64(0)>>1)-file.Size {
			return false
		}
		totalSize += file.Size
	}
	return totalSize == m.TotalSize
}

// RecordDeletion tombstones name for the given snapshot versions so rebuild
// and repair will not recreate it. Repeated records merge versions into the
// existing tombstone. An empty versions list means every version of that
// name, and is the absorbing state: recording a specific version into an
// existing all-version tombstone is a no-op (reports unchanged), while
// recording an all-version deletion into a version-specific tombstone
// upgrades it to all (clears Versions). It reports whether the manifest changed.
func (m *DownloadManifest) RecordDeletion(name string, versions []string) bool {
	name = filepath.ToSlash(name)
	for i := range m.Deleted {
		if m.Deleted[i].Path != name {
			continue
		}
		if len(m.Deleted[i].Versions) == 0 {
			// Already tombstoned for all versions: specific records add nothing.
			return false
		}
		if len(versions) == 0 {
			// Upgrade a version-specific tombstone to all versions.
			m.Deleted[i].Versions = nil
			return true
		}
		changed := false
		for _, v := range versions {
			if !slices.Contains(m.Deleted[i].Versions, v) {
				m.Deleted[i].Versions = append(m.Deleted[i].Versions, v)
				changed = true
			}
		}
		return changed
	}
	m.Deleted = append(m.Deleted, ManifestDeletion{Path: name, Versions: append([]string(nil), versions...)})
	return true
}

// IsDeleted reports whether name at the given snapshot version was
// intentionally removed. A tombstone without versions covers every version.
func (m *DownloadManifest) IsDeleted(name, version string) bool {
	name = filepath.ToSlash(name)
	for _, d := range m.Deleted {
		if d.Path != name {
			continue
		}
		if len(d.Versions) == 0 {
			return true
		}
		if slices.Contains(d.Versions, version) {
			return true
		}
	}
	return false
}

// RepoTypeName returns the type name prefix for repo directories.
func RepoTypeName(isDataset bool) string {
	if isDataset {
		return "datasets"
	}
	return "models"
}
