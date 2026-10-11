// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
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
	Type     string `yaml:"type" json:"type"`           // "model" or "dataset"
	Repo     string `yaml:"repo" json:"repo"`           // "owner/name"
	Branch   string `yaml:"branch" json:"branch"`       // Branch name (usually "main")
	Commit   string `yaml:"commit" json:"commit"`       // Commit hash
	RepoPath string `yaml:"repo_path" json:"repo_path"` // Relative to app root; absolute if Hub is on another volume.

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
			Version:   "1.0",
			Type:      repoType,
			Repo:      job.Repo,
			Branch:    branch,
			RepoPath:  repoPath,
			StartedAt: time.Now().UTC(),
			Command:   command,
			Files:     []ManifestFile{},
		},
	}
}

// SetCommit sets the commit hash.
func (b *ManifestBuilder) SetCommit(commit string) {
	b.manifest.Commit = commit
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

// manifestCreateTemp is the staging seam for writeFileAtomic; tests inject
// failures here to prove a failed manifest write leaves the previous file
// intact.
var manifestCreateTemp = os.CreateTemp

// writeFileAtomic publishes data at path via a staged sibling temporary file
// and a rename. On any failure the staging file is removed and path is left
// exactly as it was, so a failed write cannot truncate an existing file the
// way os.WriteFile would. Modes follow the previous truncate-in-place write:
// a new file lands at 0644 and a rewritten file keeps its existing mode.
func writeFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
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
	if err := staging.Chmod(mode); err != nil {
		staging.Close()
		return err
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

	var manifest DownloadManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}

	return &manifest, nil
}

// RecordDeletion tombstones name for the given snapshot versions so rebuild
// and repair will not recreate it. Repeated records merge versions into the
// existing tombstone. It reports whether the manifest changed.
func (m *DownloadManifest) RecordDeletion(name string, versions []string) bool {
	name = filepath.ToSlash(name)
	for i := range m.Deleted {
		if m.Deleted[i].Path != name {
			continue
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
