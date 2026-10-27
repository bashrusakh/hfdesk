// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
	"github.com/bashrusakh/hfdesk/pkg/smartdl"
)

var errInvalidBranchVariantPresence = errors.New("invalid branch variant presence request")

// branchVariantPresenceFile is one expected file in a variant completion
// manifest, reported back to the UI.
type branchVariantPresenceFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// branchVariantPresence is the qualified presence response for one EXL
// branch. downloaded is true only when a completed manifest for the exact
// revision/commit exists and every file it lists is present at the recorded
// size.
type branchVariantPresence struct {
	Repo            string                      `json:"repo"`
	Type            string                      `json:"type"`
	BranchVariant   bool                        `json:"branchVariant"`
	Revision        string                      `json:"revision"`
	Downloaded      bool                        `json:"downloaded"`
	DownloadStatus  string                      `json:"downloadStatus"`
	StorageMode     string                      `json:"storageMode"`
	DestinationRoot string                      `json:"destinationRoot,omitempty"`
	Commit          string                      `json:"commit,omitempty"`
	ArtifactPath    string                      `json:"artifactPath,omitempty"`
	ExpectedFiles   int                         `json:"expectedFiles"`
	Files           []branchVariantPresenceFile `json:"files"`
}

// handleBranchVariantPresence handles
// GET /api/cache/<repo>?branchVariant=true&revision=<rev>
// (plus optional routeKey/localDir). It reuses the download destination
// resolver so the query inspects exactly the location a download would use.
func (s *Server) handleBranchVariantPresence(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("type") != "" && query.Get("type") != string(hfdownloader.RepoTypeModel) {
		writeError(w, http.StatusBadRequest, "Invalid repository type", "Branch variants are model-only")
		return
	}
	revision := query.Get("revision")
	if revision == "" || !smartdl.BranchVariantRevisionShape(revision) {
		writeError(w, http.StatusBadRequest, "Invalid revision", "Expected an EXL bitrate branch name")
		return
	}
	repo := r.PathValue("repo")
	if repo == "" || !hfdownloader.IsValidModelName(repo) {
		writeError(w, http.StatusBadRequest, "Invalid repo format", "Expected owner/name")
		return
	}

	presence, err := branchVariantPresenceFor(s.snapshotConfig(), repo, revision, query.Get("routeKey"), query.Get("localDir"))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errInvalidRouteKey) || errors.Is(err, errInvalidBranchVariantPresence) {
			status = http.StatusBadRequest
		}
		writeError(w, status, "Branch variant presence lookup failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, presence)
}

func branchVariantPresenceFor(cfg Config, repo, revision, routeKey, localDir string) (branchVariantPresence, error) {
	presence := branchVariantPresence{
		Repo:          repo,
		Type:          string(hfdownloader.RepoTypeModel),
		BranchVariant: true,
		Revision:      revision,
		Files:         []branchVariantPresenceFile{},
	}
	if !hfdownloader.IsValidModelName(repo) {
		return presence, fmt.Errorf("%w: expected owner/name", errInvalidBranchVariantPresence)
	}
	if revision == "" || !smartdl.BranchVariantRevisionShape(revision) {
		return presence, fmt.Errorf("%w: expected an EXL bitrate branch name", errInvalidBranchVariantPresence)
	}

	// An explicit selector means the caller named one destination. Without
	// one, a variant downloaded to any configured route is still the same
	// analyzed branch, so check the default destination and then every route.
	if routeKey != "" || localDir != "" {
		return inspectBranchVariantDestination(cfg, presence, repo, revision, routeKey, localDir)
	}

	defaultPresence, err := inspectBranchVariantDestination(cfg, presence, repo, revision, "", "")
	if err != nil {
		return defaultPresence, err
	}
	if defaultPresence.Downloaded {
		return defaultPresence, nil
	}

	seen := map[string]bool{branchVariantDestinationIdentity(defaultPresence): true}
	keys := make([]string, 0, len(cfg.DownloadRoutes))
	for key := range cfg.DownloadRoutes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		candidate, err := inspectBranchVariantDestination(cfg, presence, repo, revision, key, "")
		if err != nil {
			continue
		}
		identity := branchVariantDestinationIdentity(candidate)
		if seen[identity] {
			continue
		}
		seen[identity] = true
		if candidate.Downloaded {
			return candidate, nil
		}
	}

	// Keep the existing response shape by reporting the default destination
	// when no destination holds a complete variant.
	return defaultPresence, nil
}

// branchVariantLocalRecord is one completion record found beside a local
// variant's payload directory.
type branchVariantLocalRecord struct {
	dir      string
	manifest *hfdownloader.DownloadManifest
}

// inspectBranchVariantDestination resolves and inspects exactly one download
// destination. storageMode plus destinationRoot identify that destination for
// fallback deduplication.
func inspectBranchVariantDestination(cfg Config, presence branchVariantPresence, repo, revision, routeKey, localDir string) (branchVariantPresence, error) {
	presence.Downloaded = false
	presence.DownloadStatus = ""
	presence.StorageMode = ""
	presence.DestinationRoot = ""
	presence.Commit = ""
	presence.ArtifactPath = ""
	presence.ExpectedFiles = 0
	presence.Files = []branchVariantPresenceFile{}

	_, resolvedLocalDir, outputDir, err := resolveDownloadDestination(cfg, DownloadRequest{
		Repo:     repo,
		Revision: revision,
		RouteKey: routeKey,
		LocalDir: localDir,
	})
	if err != nil {
		return presence, err
	}

	if resolvedLocalDir != "" {
		// Mirror CreateJob: the flat destination is frozen as an absolute path.
		absolute, err := filepath.Abs(resolvedLocalDir)
		if err != nil {
			return presence, fmt.Errorf("%w: %v", errInvalidBranchVariantPresence, err)
		}
		outputDir = absolute
		presence.StorageMode = "local"
		// The local branch variant lives in the app-owned reserved space under
		// the local root, outside the ordinary repository tree. The space is
		// the single owner of that layout, so the payload directory and the
		// completion records are resolved the same way the downloader writes
		// them.
		space, err := hfdownloader.NewBranchVariantLocalSpace(outputDir, repo)
		if err != nil {
			return presence, fmt.Errorf("%w: %v", errInvalidBranchVariantPresence, err)
		}
		variantDir, err := space.PayloadDir(revision)
		if err != nil {
			return presence, fmt.Errorf("%w: %v", errInvalidBranchVariantPresence, err)
		}
		presence.DestinationRoot = variantDir
		// The completion records live in the app-owned records tree beside
		// the payload tree, keyed by revision and commit. Local mode has no ref
		// file, so every existing record for this revision is inspected; the
		// payload directory is shared across commits of one revision.
		manifestDirs, err := space.RecordDirs(revision)
		if err != nil {
			return presence, fmt.Errorf("%w: %v", errInvalidBranchVariantPresence, err)
		}
		records := make([]branchVariantLocalRecord, 0, len(manifestDirs))
		for _, manifestDir := range manifestDirs {
			manifest, err := hfdownloader.ReadManifest(filepath.Join(manifestDir, hfdownloader.ManifestFilename))
			// The directory name is the commit the record must describe.
			if err != nil || !hfdownloader.BranchVariantRecordMatches(manifest, repo, revision, filepath.Base(manifestDir)) {
				continue
			}
			records = append(records, branchVariantLocalRecord{dir: manifestDir, manifest: manifest})
		}
		if len(records) == 0 {
			presence.DownloadStatus = "missing"
			return presence, nil
		}
		// Only the newest valid record describes the current payload. Local
		// records carry no content hash, so size-only checks cannot distinguish
		// two commits' bytes; falling back to an older record would claim a
		// mixed or replaced payload is complete. Equal completion times are
		// broken lexicographically by commit so the choice is deterministic.
		sort.SliceStable(records, func(i, j int) bool {
			if !records[i].manifest.CompletedAt.Equal(records[j].manifest.CompletedAt) {
				return records[i].manifest.CompletedAt.After(records[j].manifest.CompletedAt)
			}
			return records[i].manifest.Commit < records[j].manifest.Commit
		})
		newest := records[0]
		presence.Commit = newest.manifest.Commit
		presence.ArtifactPath = filepath.Join(newest.dir, hfdownloader.ManifestFilename)
		presence.ExpectedFiles = newest.manifest.TotalFiles
		probe := presence
		probe.Files = nil
		if !branchVariantLocalFilesPresent(variantDir, newest.manifest, &probe) {
			presence.DownloadStatus = "missing"
			return presence, nil
		}
		presence.Files = probe.Files
		presence.Downloaded = true
		presence.DownloadStatus = "complete"
		return presence, nil
	}

	cache := cfg.cache()
	repoDir, err := cache.Repo(repo, hfdownloader.RepoTypeModel)
	if err != nil {
		return presence, fmt.Errorf("%w: %v", errInvalidBranchVariantPresence, err)
	}
	presence.StorageMode = "hf-cache"
	presence.DestinationRoot = repoDir.Path()

	commit, err := repoDir.ReadRef(revision)
	if err != nil || commit == "" {
		presence.DownloadStatus = "missing"
		return presence, nil
	}
	manifestDir, err := hfdownloader.BranchVariantManifestDir(repoDir, revision, commit)
	if err != nil {
		presence.DownloadStatus = "missing"
		return presence, nil
	}
	presence.Commit = commit
	presence.ArtifactPath = filepath.Join(manifestDir, hfdownloader.ManifestFilename)
	manifest, err := hfdownloader.ReadManifest(presence.ArtifactPath)
	if err != nil || !hfdownloader.BranchVariantRecordMatches(manifest, repo, revision, commit) {
		presence.DownloadStatus = "missing"
		return presence, nil
	}
	presence.ExpectedFiles = manifest.TotalFiles
	if !branchVariantSnapshotFilesPresent(repoDir, commit, manifest, &presence) {
		presence.DownloadStatus = "missing"
		presence.Files = []branchVariantPresenceFile{}
		return presence, nil
	}
	presence.Downloaded = true
	presence.DownloadStatus = "complete"
	return presence, nil
}

func branchVariantDestinationIdentity(presence branchVariantPresence) string {
	return presence.StorageMode + "\x00" + presence.DestinationRoot
}

func branchVariantSnapshotFilesPresent(repoDir *hfdownloader.RepoDir, commit string, manifest *hfdownloader.DownloadManifest, presence *branchVariantPresence) bool {
	for _, file := range manifest.Files {
		path, err := repoDir.SnapshotPath(commit, file.Name)
		if err != nil || !regularFileWithSize(path, file.Size) {
			return false
		}
		presence.Files = append(presence.Files, branchVariantPresenceFile{Name: file.Name, Size: file.Size})
	}
	return true
}

func branchVariantLocalFilesPresent(variantDir string, manifest *hfdownloader.DownloadManifest, presence *branchVariantPresence) bool {
	for _, file := range manifest.Files {
		path, err := hfdownloader.SafeJoin(variantDir, file.Name)
		if err != nil || !regularFileWithSize(path, file.Size) {
			return false
		}
		presence.Files = append(presence.Files, branchVariantPresenceFile{Name: file.Name, Size: file.Size})
	}
	return true
}

func regularFileWithSize(path string, size int64) bool {
	info, err := os.Stat(path) // follows a snapshot symlink to its blob
	return err == nil && info.Mode().IsRegular() && info.Size() == size
}
