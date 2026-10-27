// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

// BranchVariantMetadataDir is the app-owned directory beside a repository's
// payload where HF-cache branch-variant completion manifests live. Each
// variant has its own <revision>/<commit> subdirectory: two branches can
// point at the same commit yet still need separate completion records, and a
// revision name may itself contain slashes (for example "exl/4_25").
//
// The HF-cache layout is unchanged by the local branch-variant space: the
// completion record of a branch variant lives at
// <hubRepoDir>/.hfdesk-variants/<revision>/<commit>/hfd.yaml and its payload
// in snapshots/<commit>/. Local mode uses the reserved space below instead.
const BranchVariantMetadataDir = ".hfdesk-variants"

// BranchVariantLocalSpaceName is the reserved name of the app-owned local
// branch-variant space.
//
// The space holds every local branch variant of a local output root:
//
//	<root>/.hfdesk-branch-variants/<owner>/<repo>/payload/<revision>/
//	<root>/.hfdesk-branch-variants/<owner>/<repo>/records/<revision>/<commit>/hfd.yaml
//	<root>/.hfdesk-branch-variants/<owner>/<repo>/staging/<revision>/<commit>/
//
// The space lives OUTSIDE the ordinary repository tree on purpose. The
// ordinary downloader writes whatever the repository tree contains and
// reserves no name, so a tree that carries one of the app's former directory
// names would otherwise share those paths with the app's own writes.
//
// The space is a top-level entry of the local root, so it is not a repository
// owner: the local repository scan skips it, and an ordinary download whose
// repository owner is this name is refused instead of writing into the app's
// own space.
const BranchVariantLocalSpaceName = ".hfdesk-branch-variants"

// branchVariantLocalPayloadDirName, branchVariantLocalRecordsDirName and
// branchVariantLocalStagingDirName are the three subtrees of the app-owned
// local branch-variant space.
const (
	branchVariantLocalPayloadDirName = "payload"
	branchVariantLocalRecordsDirName = "records"
	branchVariantLocalStagingDirName = "staging"
)

// IsBranchVariantReservedName reports whether name is the reserved app-owned
// branch-variant space name.
//
// The comparison is case-insensitive: on a case-insensitive filesystem a
// differently cased spelling names the same directory, and the local
// repository scan hides the reserved name case-insensitively as well, so the
// download refusal and the scan exclusion have to agree.
func IsBranchVariantReservedName(name string) bool {
	return strings.EqualFold(name, BranchVariantLocalSpaceName)
}

// BranchVariantManifestDir returns the directory that receives hfd.yaml for an
// HF-cache branch variant revision at commit. The revision and commit are
// validated so the result stays inside the repository's metadata directory.
func BranchVariantManifestDir(repoDir *RepoDir, revision, commit string) (string, error) {
	if repoDir == nil {
		return "", errors.New("branch variant manifest requires a repository directory")
	}
	return branchVariantManifestDir(repoDir.Path(), revision, commit)
}

// BranchVariantLocalSpace is the app-owned local branch-variant space of one
// repository under a local output root.
//
// The space is the single owner of the local branch-variant layout: the
// payload directory, the completion-record directory, the record listing and
// the record invalidation are all derived from it, so the downloader, the
// server's presence lookup and the completion-record write cannot disagree
// about where a variant lives.
//
// See BranchVariantLocalSpaceName for the layout this space owns and why it
// lives outside the ordinary repository tree.
type BranchVariantLocalSpace struct {
	root    string
	repo    string
	owner   string
	name    string
	space   string
	payload string
	records string
	staging string
}

// NewBranchVariantLocalSpace returns the app-owned local branch-variant space
// of repo under the local output root.
//
// The root is resolved to an absolute path, so the space is the same
// directory whether the caller passes a relative or an absolute root. The
// repository id must be an owner/name pair of plain path components, and
// every path component of the space is resolved through SafeJoin, the
// package's containment primitive.
func NewBranchVariantLocalSpace(root, repo string) (BranchVariantLocalSpace, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return BranchVariantLocalSpace{}, fmt.Errorf("invalid repository id %q (expected owner/name)", repo)
	}
	owner, name := parts[0], parts[1]
	if !filepath.IsLocal(owner) || !filepath.IsLocal(name) {
		return BranchVariantLocalSpace{}, fmt.Errorf("invalid repository id %q", repo)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return BranchVariantLocalSpace{}, fmt.Errorf("resolve local root %q: %w", root, err)
	}
	space, err := SafeJoin(rootAbs, filepath.Join(BranchVariantLocalSpaceName, owner, name))
	if err != nil {
		return BranchVariantLocalSpace{}, fmt.Errorf("resolve branch variant space: %w", err)
	}
	payload, err := SafeJoin(space, branchVariantLocalPayloadDirName)
	if err != nil {
		return BranchVariantLocalSpace{}, fmt.Errorf("resolve branch variant payload directory: %w", err)
	}
	records, err := SafeJoin(space, branchVariantLocalRecordsDirName)
	if err != nil {
		return BranchVariantLocalSpace{}, fmt.Errorf("resolve branch variant records directory: %w", err)
	}
	staging, err := SafeJoin(space, branchVariantLocalStagingDirName)
	if err != nil {
		return BranchVariantLocalSpace{}, fmt.Errorf("resolve branch variant staging directory: %w", err)
	}
	return BranchVariantLocalSpace{
		root:    rootAbs,
		repo:    repo,
		owner:   owner,
		name:    name,
		space:   space,
		payload: payload,
		records: records,
		staging: staging,
	}, nil
}

// Path returns the directory of the space itself:
// <root>/.hfdesk-branch-variants/<owner>/<repo>.
func (space BranchVariantLocalSpace) Path() string {
	return space.space
}

// PayloadDir returns the directory that receives the payload files of one
// revision inside the space: <space>/payload/<revision>.
//
// The payload must not live at a repository-relative path: a repository whose
// ordinary tree contains a top-level directory named like the revision (for
// example "4.00bpw/config.json") would otherwise make the branch download
// write the very same file as the ordinary download and replace its bytes.
// See BranchVariantLocalSpaceName for the reserved space that holds it.
func (space BranchVariantLocalSpace) PayloadDir(revision string) (string, error) {
	if !ValidBranchVariantRevision(revision) {
		return "", fmt.Errorf("invalid branch variant revision %q", revision)
	}
	return SafeJoin(space.payload, revision)
}

// RecordDir returns the directory that receives hfd.yaml for one revision at
// one commit inside the space: <space>/records/<revision>/<commit>. It is
// never inside the payload directory, so a repository file named hfd.yaml
// cannot collide with the completion record.
func (space BranchVariantLocalSpace) RecordDir(revision, commit string) (string, error) {
	if !ValidBranchVariantRevision(revision) {
		return "", fmt.Errorf("invalid branch variant revision %q", revision)
	}
	if !safeCommitName(commit) {
		return "", fmt.Errorf("invalid branch variant commit %q", commit)
	}
	return SafeJoin(space.records, filepath.Join(revision, commit))
}

// stagingDir returns the commit-scoped staging directory of one revision
// inside the space: <space>/staging/<revision>/<commit>.
//
// The staging directory is app-owned and transient. It holds the bytes of one
// transfer until they are verified and published into the payload directory.
// Its identity is the revision and the resolved commit, so the partial of an
// interrupted transfer belongs to exactly one commit: a transfer of a moved
// branch starts from zero instead of resuming another commit's bytes, and a
// retry of the same commit finds and resumes its own partial.
//
// Every level of the path is a single safe component: the revision is a
// validated component sequence (see ValidBranchVariantRevision) and the
// commit is a plain name (see safeCommitName).
func (space BranchVariantLocalSpace) stagingDir(revision, commit string) (string, error) {
	if !ValidBranchVariantRevision(revision) {
		return "", fmt.Errorf("invalid branch variant revision %q", revision)
	}
	if !safeCommitName(commit) {
		return "", fmt.Errorf("invalid branch variant commit %q", commit)
	}
	dir, err := SafeJoin(space.staging, revision)
	if err != nil {
		return "", err
	}
	return SafeJoin(dir, commit)
}

// stagingPath returns the commit-scoped staging path of one file of one
// revision: <space>/staging/<revision>/<commit>/<name>.
//
// The name is derived from the file's relative path, so the staging path is
// deterministic for the same (revision, commit, relative path) and a retry of
// the same commit resumes its own partial file. The relative path itself is
// never a path component of the staging path (see
// branchVariantStagingFileName), so a repository file name can neither
// collide with a staging file nor escape the staging directory.
func (space BranchVariantLocalSpace) stagingPath(revision, commit, relPath string) (string, error) {
	dir, err := space.stagingDir(revision, commit)
	if err != nil {
		return "", err
	}
	return SafeJoin(dir, branchVariantStagingFileName(relPath))
}

// ensureStagingTree verifies that every component of the commit-scoped
// staging tree is a real directory inside the local root and never a
// symlink, so a staging write cannot be redirected outside the reserved
// space (see ensureLocalBranchVariantTree).
//
// The staging tree is app-owned like the payload and record trees, so it
// carries the same per-component check. A missing component is not unsafe:
// nothing below it can exist yet, and the downloader creates the tree.
func (space BranchVariantLocalSpace) ensureStagingTree(revision, commit string) error {
	if !ValidBranchVariantRevision(revision) {
		return fmt.Errorf("invalid branch variant revision %q", revision)
	}
	if !safeCommitName(commit) {
		return fmt.Errorf("invalid branch variant commit %q", commit)
	}
	components := append(space.treeComponents(branchVariantLocalStagingDirName, revision), commit)
	return ensureLocalBranchVariantTree(space.root, components)
}

// branchVariantStagingFileName returns the single path component that names
// one file's staging file inside a commit-scoped staging directory.
//
// The relative path is hashed instead of naming the file: the staging area
// holds one file per repository path, and a repository path can carry
// separators and characters that are not valid in a path component on every
// platform. The digest keeps the name one safe component and keeps distinct
// paths distinct, so paths such as "a/b_c" and "a_b/c" never share a staging
// file.
func branchVariantStagingFileName(relPath string) string {
	sum := sha256.Sum256([]byte(relPath))
	return "f-" + hex.EncodeToString(sum[:])
}

// RecordDirs lists the existing completion-record directories of one revision
// inside the space, in lexical commit order. Local/flat mode has no ref file,
// so presence resolves the commit from the records themselves.
func (space BranchVariantLocalSpace) RecordDirs(revision string) ([]string, error) {
	if !ValidBranchVariantRevision(revision) {
		return nil, fmt.Errorf("invalid branch variant revision %q", revision)
	}
	revisionDir, err := space.recordRevisionDir(revision)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(revisionDir)
	if err != nil {
		// A missing, obstructed, or unreadable record store holds no
		// completion record; presence reports the revision as not downloaded.
		return nil, nil
	}
	dirs := make([]string, 0, len(entries))
	for _, entry := range branchVariantRecordEntries(entries) {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(revisionDir, entry.Name()))
		}
	}
	return dirs, nil
}

// PayloadRepoPath returns the payload directory of one revision relative to
// the local output root: the form the completion record's repo_path field
// uses.
func (space BranchVariantLocalSpace) PayloadRepoPath(revision string) (string, error) {
	dir, err := space.PayloadDir(revision)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(space.root, dir)
	if err != nil {
		return "", fmt.Errorf("resolve branch variant payload path: %w", err)
	}
	return filepath.ToSlash(rel), nil
}

// recordRevisionDir returns the directory that holds every completion record
// of one revision inside the space.
func (space BranchVariantLocalSpace) recordRevisionDir(revision string) (string, error) {
	if !ValidBranchVariantRevision(revision) {
		return "", fmt.Errorf("invalid branch variant revision %q", revision)
	}
	return SafeJoin(space.records, revision)
}

// treeComponents returns the path components of one app-owned tree of the
// space, relative to the local output root. A revision may contain slashes (a
// real branch like "exl/4_25"), so every component below the tree is checked
// on its own.
func (space BranchVariantLocalSpace) treeComponents(tree, revision string) []string {
	return append([]string{BranchVariantLocalSpaceName, space.owner, space.name, tree}, strings.Split(revision, "/")...)
}

// branchVariantManifestDir returns the completion-manifest directory of an
// HF-cache branch variant revision at commit, composed from
// BranchVariantMetadataDir.
func branchVariantManifestDir(base, revision, commit string) (string, error) {
	if !ValidBranchVariantRevision(revision) {
		return "", fmt.Errorf("invalid branch variant revision %q", revision)
	}
	if !safeCommitName(commit) {
		return "", fmt.Errorf("invalid branch variant commit %q", commit)
	}
	return SafeJoin(base, filepath.Join(BranchVariantMetadataDir, revision, commit))
}

// InvalidateRecords removes the app's own completion records for one revision
// from the space.
//
// Only the app's own records are removed: the completion record
// <space>/records/<revision>/<commit>/hfd.yaml of every record directory
// <commit> under the revision directory, plus the record and revision
// directories themselves once they are empty. Nothing else is removed.
//
// A file that carries the record's name at the record path is not a
// completion record and survives: the content decides, through the shared
// record contract (see BranchVariantRecordMatches). A file that does not
// parse as a manifest, or that describes another repository, revision or
// commit, is left completely untouched.
//
// A local branch-variant download calls this after planning and before any
// payload effect, so a failed or cancelled re-download cannot leave an older
// record claiming a mixed or replaced payload is complete.
//
// The app-owned trees the space uses are verified first, so a symlinked tree
// cannot redirect the removal or the later payload writes outside the space
// (see ensureLocalBranchVariantTree). A missing tree is not unsafe: the first
// download creates it.
func (space BranchVariantLocalSpace) InvalidateRecords(revision string) error {
	if !ValidBranchVariantRevision(revision) {
		return fmt.Errorf("invalid branch variant revision %q", revision)
	}
	for _, tree := range []string{branchVariantLocalPayloadDirName, branchVariantLocalRecordsDirName} {
		if err := ensureLocalBranchVariantTree(space.root, space.treeComponents(tree, revision)); err != nil {
			return fmt.Errorf("unsafe branch variant layout: %w", err)
		}
	}
	revisionDir, err := space.recordRevisionDir(revision)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(revisionDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No revision directory means no completion record.
			return nil
		}
		return fmt.Errorf("read branch variant completion records: %w", err)
	}
	// The record directories are the ones the presence lookup enumerates: a
	// plain directory named like a commit. Every other entry of the revision
	// directory is left alone. The name only selects a candidate: the record
	// file inside it is removed only when its content proves that it is this
	// variant's completion record (see BranchVariantRecordMatches).
	for _, entry := range branchVariantRecordEntries(entries) {
		recordDir := filepath.Join(revisionDir, entry.Name())
		// Inspect the record directory before reaching into it: a symlinked
		// one would make the record removal below delete a file outside the
		// space, and would make the completion record written at the end of
		// the download land outside it too.
		info, err := os.Lstat(recordDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("inspect branch variant completion record %s: %w", recordDir, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe branch variant layout: %s is a symbolic link", recordDir)
		}
		if !info.IsDir() {
			// Not a record directory: the app-owned tree must be a real
			// directory at every level it walks. A regular file in a record
			// slot would make the completion record written at the end of the
			// download fail, so the download is refused here, before any
			// payload byte is written.
			return fmt.Errorf("unsafe branch variant layout: %s is not a directory", recordDir)
		}
		if err := removeBranchVariantCompletionRecord(filepath.Join(recordDir, ManifestFilename), space.repo, revision, entry.Name()); err != nil {
			return err
		}
		// Prune the record directory when the record was its only content.
		// os.Remove fails on a non-empty directory, which is exactly the case
		// where content that must survive lives in it.
		_ = os.Remove(recordDir)
	}
	// Prune the revision directory when the removed records were its only
	// content. A revision directory that still holds content stays exactly
	// where it is.
	_ = os.Remove(revisionDir)
	return nil
}

// removeBranchVariantCompletionRecord removes one app-owned completion record
// file, but only when the file really is the completion record of the branch
// variant of repo at revision and commit.
//
// The file name is not proof: a file that carries the record's name can sit
// at exactly the record path. The content decides, through the shared record
// contract (see BranchVariantRecordMatches).
//
// Only the record file is removed. A missing record is not an error, and a
// record path that is a directory is left alone: it is not a record the app
// wrote, and os.Remove would delete a directory that may hold other content.
// A symlink is removed as a link only, so a record that points outside the
// space keeps its target.
func removeBranchVariantCompletionRecord(record, repo, revision, commit string) error {
	info, err := os.Lstat(record)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect branch variant completion record %s: %w", record, err)
	}
	if info.IsDir() {
		return nil
	}
	manifest, err := ReadManifest(record)
	if err != nil {
		// Not a manifest at all: repository content that carries the record's
		// name, so it stays.
		return nil
	}
	if !BranchVariantRecordMatches(manifest, repo, revision, commit) {
		// A manifest of something else: it is not this variant's record, so
		// it stays.
		return nil
	}
	if err := os.Remove(record); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove branch variant completion record %s: %w", record, err)
	}
	return nil
}

// BranchVariantRecordMatches reports whether manifest is the completion
// record of the branch variant of repo at revision and commit.
//
// It is the single owner of the question "is this file this variant's
// completion record?", so the presence lookup and the local invalidation
// answer it the same way and the rule cannot drift. The conditions are the
// ones the presence lookup requires: the manifest must be a branch-variant
// manifest, it must name the expected repository, revision and commit, and it
// must be a complete record (a completion time, and a file list that matches
// the recorded file count).
//
// A file that is not a manifest, or a manifest for another repository,
// revision or commit, is not a record of this variant: it is repository
// content that happens to carry the record's name.
func BranchVariantRecordMatches(manifest *DownloadManifest, repo, revision, commit string) bool {
	return manifest != nil &&
		manifest.BranchVariant &&
		manifest.Repo == repo &&
		manifest.Branch == revision &&
		manifest.Commit == commit &&
		!manifest.CompletedAt.IsZero() &&
		manifest.TotalFiles > 0 &&
		len(manifest.Files) == manifest.TotalFiles
}

// ensureLocalBranchVariantTree verifies that every component of an app-owned
// local branch-variant tree is a real directory inside base and never a
// symlink.
//
// Every component is resolved through SafeJoin, the package's containment
// primitive: the revision reaches this function from an HTTP request, and
// SafeJoin's filepath.IsLocal check is the barrier code scanning
// (go/path-injection) recognizes, so a component is proven to stay under the
// component above it before it reaches the filesystem. A component SafeJoin
// refuses is reported as an unsafe layout instead of being inspected.
//
// SafeJoin proves textual containment only: it never inspects the
// filesystem, so a symlinked component would silently redirect the tree
// outside the space. The check uses os.Lstat so a symlink is detected
// instead of followed, and it inspects every component rather than only the
// leaf, because a symlinked intermediate component redirects everything below
// it: the completion records the invalidation removes and the payload bytes
// the download writes. A missing component is not unsafe: nothing below it
// can exist, and the downloader creates the tree.
func ensureLocalBranchVariantTree(base string, components []string) error {
	current := base
	for _, part := range components {
		next, err := SafeJoin(current, part)
		if err != nil {
			return fmt.Errorf("resolve %q inside %s: %w", part, current, err)
		}
		current = next
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", current)
		}
	}
	return nil
}

// branchVariantRecordEntries returns the entries of a revision directory that
// name a completion record directory, using the one name rule the completion
// record layout is keyed by: a plain, non-escaping commit name. The callers
// decide what to do with an entry that is not a plain directory: the presence
// lookup ignores it, and the invalidation refuses it unless it is a real
// directory.
func branchVariantRecordEntries(entries []os.DirEntry) []os.DirEntry {
	records := make([]os.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if safeCommitName(entry.Name()) {
			records = append(records, entry)
		}
	}
	return records
}

// ValidBranchVariantRevision reports whether revision is a slash-separated
// sequence of plain path components accepted by the branch-variant
// completion-manifest layout. Revision names may contain "/" (a real branch
// like "exl/4_25"), so each component is validated instead of rejecting the
// separator outright: no empty, "." or ".." component, no backslash (a Windows
// separator and never part of a Hub revision name), no control characters, and
// no component longer than 255 bytes. The server's branch-variant admission
// uses this validator so a revision that cannot become a manifest path never
// reaches a download.
func ValidBranchVariantRevision(revision string) bool {
	if revision == "" {
		return false
	}
	for _, part := range strings.Split(revision, "/") {
		if part == "" || part == "." || part == ".." || len(part) > 255 {
			return false
		}
		for _, r := range part {
			if r == '\\' || r < 0x20 || r == 0x7f {
				return false
			}
		}
	}
	return true
}

// DownloadManifest captures the complete record of a download job.
// This file is written to the friendly view root (e.g., models/Owner/Repo/hfd.yaml)
// and contains all information needed to understand and reproduce the download.
//
// A branch variant's manifest is its completion record rather than a
// friendly-view manifest: it is written to the branch-variant completion-record
// directory (see BranchVariantMetadataDir in HF-cache mode and
// BranchVariantLocalSpace.RecordDir in local mode).
//
// Type is "model" or "dataset" and Repo is an "owner/name" id. Branch is the
// revision name (usually "main"). RepoPath is relative to the app root, and
// absolute when the Hub is on another volume. TotalSize is the sum of the
// recorded file sizes, in bytes.
type DownloadManifest struct {
	Version string `yaml:"version" json:"version"`

	Type     string `yaml:"type" json:"type"`
	Repo     string `yaml:"repo" json:"repo"`
	Branch   string `yaml:"branch" json:"branch"`
	Commit   string `yaml:"commit" json:"commit"`
	RepoPath string `yaml:"repo_path" json:"repo_path"`

	// BranchVariant marks a manifest for an analyzer-recognized EXL2/EXL3
	// branch stored in isolation. Ordinary repo manifests leave it false.
	BranchVariant bool `yaml:"branch_variant,omitempty" json:"branch_variant,omitempty"`

	StartedAt   time.Time `yaml:"started_at" json:"started_at"`
	CompletedAt time.Time `yaml:"completed_at" json:"completed_at"`

	// CLI command used (token stripped for security)
	Command string `yaml:"command" json:"command"`

	TotalSize  int64          `yaml:"total_size" json:"total_size"`
	TotalFiles int            `yaml:"total_files" json:"total_files"`
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

// ManifestFile is one file in the manifest. The Blob field contains the
// relative path from the repo directory to the blob, making it easy for
// automation scripts to locate and copy files. Size is the file size in bytes.
type ManifestFile struct {
	Name string `yaml:"name" json:"name"`
	Blob string `yaml:"blob" json:"blob"`
	Size int64  `yaml:"size" json:"size"`
	LFS  bool   `yaml:"lfs" json:"lfs"`
}

type ManifestBuilder struct {
	manifest  *DownloadManifest
	startTime time.Time
}

func NewManifestBuilder(job Job, command string) *ManifestBuilder {
	repoType := "model"
	if job.IsDataset {
		repoType = "dataset"
	}

	branch := job.Revision
	if branch == "" {
		branch = "main"
	}

	repoPath := "hub/" + RepoTypeName(job.IsDataset) + "--" + strings.ReplaceAll(job.Repo, "/", "--")

	return &ManifestBuilder{
		startTime: time.Now().UTC(),
		manifest: &DownloadManifest{
			Version:       "1.0",
			Type:          repoType,
			Repo:          job.Repo,
			Branch:        branch,
			RepoPath:      repoPath,
			BranchVariant: job.BranchVariant,
			StartedAt:     time.Now().UTC(),
			Command:       command,
			Files:         []ManifestFile{},
		},
	}
}

func (b *ManifestBuilder) SetCommit(commit string) {
	b.manifest.Commit = commit
}

// AddFile adds a file to the manifest and accumulates the size and file count.
func (b *ManifestBuilder) AddFile(name, blobHash string, size int64, isLFS bool) {
	// An HF-cache entry points at its content-addressed blob. Local/flat mode
	// has no blob, so leave the field empty instead of emitting a broken
	// "blobs/" prefix.
	blob := ""
	if blobHash != "" {
		blob = "blobs/" + blobHash
	}
	b.manifest.Files = append(b.manifest.Files, ManifestFile{
		Name: name,
		Blob: blob,
		Size: size,
		LFS:  isLFS,
	})
	b.manifest.TotalSize += size
	b.manifest.TotalFiles++
}

// Build finalizes the manifest and stamps its completion time.
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

	if err := os.MkdirAll(friendlyPath, 0o755); err != nil {
		return "", fmt.Errorf("create manifest dir: %w", err)
	}

	data, err := yaml.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal manifest: %w", err)
	}

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
var manifestCreateTemp = createManifestTemp

// createManifestTemp creates an exclusive sibling staging file using the
// requested permissions. Unlike os.CreateTemp (which always uses 0600),
// OpenFile applies the process umask to 0644.
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

func RepoTypeName(isDataset bool) string {
	if isDataset {
		return "datasets"
	}
	return "models"
}
