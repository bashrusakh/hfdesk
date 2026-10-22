// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// BranchVariantPresenceResponse is returned only by the additive qualified
// cache query. It identifies the selected branch, its resolved artifact root,
// and the exact destination that was checked.
type BranchVariantPresenceResponse struct {
	Repo            string                      `json:"repo"`
	Type            string                      `json:"type"`
	BranchVariant   bool                        `json:"branchVariant"`
	Revision        string                      `json:"revision"`
	Downloaded      bool                        `json:"downloaded"`
	DownloadStatus  string                      `json:"downloadStatus"`
	StorageMode     string                      `json:"storageMode"`
	DestinationRoot string                      `json:"destinationRoot"`
	HubDir          string                      `json:"hubDir,omitempty"`
	VariantPath     string                      `json:"variantPath"`
	ControlPath     string                      `json:"controlPath,omitempty"`
	ArtifactPath    string                      `json:"artifactPath,omitempty"`
	Commit          string                      `json:"commit,omitempty"`
	ExpectedFiles   int                         `json:"expectedFiles,omitempty"`
	Files           []BranchVariantPresenceFile `json:"files,omitempty"`
	Warning         string                      `json:"warning,omitempty"`
}

type BranchVariantPresenceFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func (s *Server) handleBranchVariantPresence(w http.ResponseWriter, r *http.Request, repo string) {
	query := r.URL.Query()
	selector := query["branchVariant"]
	if len(selector) != 1 || selector[0] != "true" {
		writeError(w, http.StatusBadRequest, "Invalid branchVariant selector", "Expected exactly one branchVariant=true")
		return
	}
	readOne := func(name string) (string, bool, bool) {
		values, supplied := query[name]
		if !supplied {
			return "", false, true
		}
		if len(values) != 1 {
			return "", true, false
		}
		return values[0], true, true
	}
	revision, supplied, valid := readOne("revision")
	if !valid || !supplied || revision == "" {
		writeError(w, http.StatusBadRequest, "Missing branch revision", "A branchVariant cache query requires one non-empty revision")
		return
	}
	repoTypeValue, suppliedType, valid := readOne("type")
	if !valid || suppliedType && repoTypeValue != string(hfdownloader.RepoTypeModel) {
		writeError(w, http.StatusBadRequest, "Invalid repository type", "EXL branch variants require type=model")
		return
	}
	localDir, _, valid := readOne("localDir")
	if !valid {
		writeError(w, http.StatusBadRequest, "Invalid destination", "Expected at most one localDir")
		return
	}
	routeKey, _, valid := readOne("routeKey")
	if !valid {
		writeError(w, http.StatusBadRequest, "Invalid routeKey", "Expected at most one routeKey")
		return
	}

	req := DownloadRequest{Repo: repo, Revision: revision, BranchVariant: true, LocalDir: localDir, RouteKey: routeKey}
	if err := validateBranchVariantRequest(req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid branchVariant", "EXL branch variants require an explicit unfiltered model revision")
		return
	}
	cfg := s.snapshotConfig()
	if routeKey != "" && localDir == "" && resolveRoute(cfg.DownloadRoutes, routeKey) == "" {
		writeError(w, http.StatusBadRequest, "Unconfigured routeKey", "The selected routeKey has no configured destination")
		return
	}
	_, effectiveLocalDir, _, err := resolveDownloadDestination(cfg, req)
	if err != nil {
		if err == errInvalidRouteKey {
			writeError(w, http.StatusBadRequest, "Invalid routeKey", "routeKey must be one of the configured route keys")
			return
		}
		writeError(w, http.StatusBadRequest, "Invalid destination", err.Error())
		return
	}

	response := BranchVariantPresenceResponse{
		Repo: repo, Type: string(hfdownloader.RepoTypeModel), BranchVariant: true,
		Revision: revision, DownloadStatus: "unknown", Files: []BranchVariantPresenceFile{},
	}
	if effectiveLocalDir != "" {
		root, err := filepath.Abs(effectiveLocalDir)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid destination", err.Error())
			return
		}
		variantBase, err := hfdownloader.BranchVariantRevisionPayloadPath(root, hfdownloader.RepoTypeModel, repo, revision)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid branch revision", err.Error())
			return
		}
		controlBase, err := hfdownloader.BranchVariantMetadataRevisionPath(root, hfdownloader.RepoTypeModel, repo, revision)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid branch revision", err.Error())
			return
		}
		response.StorageMode = "local"
		response.DestinationRoot = root
		response.VariantPath = variantBase
		response.ControlPath = controlBase
		manifest, artifactPath, controlPath, warning := findLocalBranchVariant(root, controlBase, repo, revision)
		response.Warning = warning
		if manifest != nil {
			setBranchVariantPresence(&response, manifest, artifactPath, controlPath)
		}
		writeJSON(w, http.StatusOK, response)
		return
	}

	cache := cfg.cache()
	repoDir, err := cache.Repo(repo, hfdownloader.RepoTypeModel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid repository format", err.Error())
		return
	}
	metadataBase, err := repoDir.BranchVariantMetadataRevisionPath(revision)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid branch revision", err.Error())
		return
	}
	variantBase, err := hfdownloader.BranchVariantRevisionPayloadPath(cache.Root, repoDir.Type(), repoDir.RepoID(), revision)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid branch revision", err.Error())
		return
	}
	response.StorageMode = "hf-cache"
	response.DestinationRoot = cache.Root
	response.HubDir = cache.HubDir()
	response.VariantPath = variantBase
	response.ControlPath = metadataBase
	manifest, artifactPath, controlPath, warning := findHFBranchVariant(cache, repoDir, metadataBase, revision)
	response.Warning = warning
	if manifest != nil {
		setBranchVariantPresence(&response, manifest, artifactPath, controlPath)
	}
	writeJSON(w, http.StatusOK, response)
}

func setBranchVariantPresence(response *BranchVariantPresenceResponse, manifest *hfdownloader.DownloadManifest, artifactPath, controlPath string) {
	response.Downloaded = true
	response.DownloadStatus = "complete"
	response.ArtifactPath = artifactPath
	response.ControlPath = controlPath
	response.Commit = manifest.Commit
	response.ExpectedFiles = len(manifest.Files)
	response.Files = make([]BranchVariantPresenceFile, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		response.Files = append(response.Files, BranchVariantPresenceFile{Name: file.Name, Size: file.Size})
	}
	sort.Slice(response.Files, func(i, j int) bool { return response.Files[i].Name < response.Files[j].Name })
}

func findLocalBranchVariant(destinationRoot, controlBase, repo, revision string) (*hfdownloader.DownloadManifest, string, string, string) {
	serviceRoot, err := hfdownloader.BranchVariantServiceRoot(destinationRoot)
	if err != nil {
		return nil, "", "", fmt.Sprintf("Could not resolve the selected local service tree: %v", err)
	}
	rootReal := destinationRoot
	if resolved, resolveErr := filepath.EvalSymlinks(destinationRoot); resolveErr == nil {
		rootReal = resolved
	}
	if !pathComponentsNoSymlinks(rootReal, serviceRoot) || !pathComponentsNoSymlinks(serviceRoot, controlBase) {
		return nil, "", "", "The selected local variant control path contains a symlink"
	}
	entries, err := os.ReadDir(controlBase)
	if os.IsNotExist(err) {
		return nil, "", "", ""
	}
	if err != nil {
		return nil, "", "", fmt.Sprintf("Could not inspect the selected local destination: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		metadataDir := filepath.Join(controlBase, entry.Name())
		lease, leaseErr := hfdownloader.AcquireBranchVariantDirectoryLease(metadataDir, false)
		if leaseErr != nil {
			continue
		}
		manifest, readErr := readBranchVariantManifest(metadataDir, repo, revision, hfdownloader.RepoTypeModel, entry.Name())
		if readErr != nil {
			lease()
			continue
		}
		if manifest == nil {
			lease()
			continue
		}
		artifact, ownerErr := hfdownloader.ReadBranchVariantArtifactAtControl(destinationRoot, hfdownloader.RepoTypeModel, repo, revision, metadataDir)
		if ownerErr != nil || artifact == nil || !localBranchVariantFilesPresent(destinationRoot, artifact.PayloadRoot, manifest) {
			lease()
			continue
		}
		lease()
		return manifest, artifact.PayloadRoot, artifact.ControlRoot, ""
	}
	return nil, "", "", ""
}

func findHFBranchVariant(cache *hfdownloader.HFCache, repoDir *hfdownloader.RepoDir, metadataBase, revision string) (*hfdownloader.DownloadManifest, string, string, string) {
	serviceRoot, err := hfdownloader.BranchVariantServiceRoot(cache.Root)
	if os.IsNotExist(err) {
		return nil, "", "", ""
	}
	if err != nil {
		return nil, "", "", "The selected service tree cannot be resolved"
	}
	serviceReal, err := filepath.EvalSymlinks(serviceRoot)
	if os.IsNotExist(err) {
		return nil, "", "", ""
	}
	if err != nil {
		return nil, "", "", "The selected service tree cannot be resolved"
	}
	cacheReal, err := filepath.EvalSymlinks(cache.Root)
	if err != nil || !hfdownloader.PathInside(cacheReal, serviceReal) || !pathComponentsNoSymlinks(serviceRoot, metadataBase) {
		return nil, "", "", "The selected branch variant service path is not confined"
	}
	entries, err := os.ReadDir(metadataBase)
	if os.IsNotExist(err) {
		return nil, "", "", ""
	}
	if err != nil {
		return nil, "", "", fmt.Sprintf("Could not inspect the selected HF cache destination: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		metadataDir := filepath.Join(metadataBase, entry.Name())
		lease, leaseErr := hfdownloader.AcquireBranchVariantDirectoryLease(metadataDir, false)
		if leaseErr != nil {
			continue
		}
		manifest, readErr := readBranchVariantManifest(metadataDir, repoDir.RepoID(), revision, repoDir.Type(), entry.Name())
		if readErr != nil || manifest == nil || !manifestBelongsToRepo(cache, repoDir, manifest) {
			lease()
			continue
		}
		artifact, ownerErr := hfdownloader.ReadBranchVariantArtifactAtControl(cache.Root, repoDir.Type(), repoDir.RepoID(), revision, metadataDir)
		if ownerErr != nil || artifact == nil || !hfBranchVariantFilesPresent(cache.Root, repoDir, artifact, manifest) {
			lease()
			continue
		}
		lease()
		return manifest, artifact.PayloadRoot, artifact.ControlRoot, ""
	}
	return nil, "", "", ""
}

func readBranchVariantManifest(dir, repo, revision string, repoType hfdownloader.RepoType, commitDirName string) (*hfdownloader.DownloadManifest, error) {
	manifestPath := filepath.Join(dir, hfdownloader.ManifestFilename)
	info, err := os.Lstat(manifestPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("branch variant manifest is not a regular file")
	}
	manifest, err := hfdownloader.ReadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	if !manifest.IsCompleteBranchVariant(repo, repoType, revision) || commitDirName != hfdownloader.BranchVariantCommitDirName(manifest.Commit) {
		return nil, fmt.Errorf("branch variant manifest identity does not match its location")
	}
	return manifest, nil
}

func localBranchVariantFilesPresent(destinationRoot, artifactPath string, manifest *hfdownloader.DownloadManifest) bool {
	rootReal := destinationRoot
	if resolved, err := filepath.EvalSymlinks(destinationRoot); err == nil {
		rootReal = resolved
	}
	artifactInfo, err := os.Lstat(artifactPath)
	if err != nil || artifactInfo.Mode()&os.ModeSymlink != 0 || !artifactInfo.IsDir() {
		return false
	}
	if !pathComponentsNoSymlinks(rootReal, artifactPath) {
		return false
	}
	artifactReal, err := filepath.EvalSymlinks(artifactPath)
	if err != nil || !hfdownloader.PathInside(rootReal, artifactReal) {
		return false
	}
	for _, file := range manifest.Files {
		path, err := hfdownloader.SafeJoin(artifactPath, file.Name)
		if err != nil {
			return false
		}
		if !pathComponentsNoSymlinks(artifactPath, filepath.Dir(path)) {
			return false
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != file.Size {
			return false
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !hfdownloader.PathInside(artifactReal, resolved) {
			return false
		}
	}
	return true
}

func hfBranchVariantFilesPresent(cacheRoot string, repoDir *hfdownloader.RepoDir, artifact *hfdownloader.BranchVariantArtifact, manifest *hfdownloader.DownloadManifest) bool {
	cacheReal, err := filepath.EvalSymlinks(cacheRoot)
	if err != nil {
		return false
	}
	serviceReal, err := filepath.EvalSymlinks(artifact.ServiceRoot)
	payloadReal, payloadErr := filepath.EvalSymlinks(artifact.PayloadRoot)
	if err != nil || payloadErr != nil || !hfdownloader.PathInside(cacheReal, serviceReal) ||
		!hfdownloader.PathInside(serviceReal, payloadReal) ||
		!pathComponentsNoSymlinks(artifact.ServiceRoot, artifact.ControlRoot) ||
		!pathComponentsNoSymlinks(artifact.ServiceRoot, artifact.PayloadRoot) {
		return false
	}
	for _, file := range manifest.Files {
		hash := strings.TrimPrefix(file.Blob, "blobs/")
		blobPath := repoDir.BlobPath(hash)
		snapshotPath, err := repoDir.SnapshotPath(manifest.Commit, file.Name)
		blobInfo, blobErr := os.Lstat(blobPath)
		if err != nil || blobErr != nil || !blobInfo.Mode().IsRegular() || blobInfo.Mode()&os.ModeSymlink != 0 ||
			!pathComponentsNoSymlinks(repoDir.Path(), filepath.Dir(snapshotPath)) ||
			!pathComponentsNoSymlinks(repoDir.Path(), filepath.Dir(blobPath)) || !verifiedCacheMaterialization(snapshotPath, blobPath, hash, file.Size) {
			return false
		}
		payloadPath, err := hfdownloader.SafeJoin(artifact.PayloadRoot, file.Name)
		if err != nil || !pathComponentsNoSymlinks(artifact.PayloadRoot, filepath.Dir(payloadPath)) ||
			!verifiedCacheMaterialization(payloadPath, snapshotPath, hash, file.Size) {
			return false
		}
	}
	return true
}

func pathComponentsNoSymlinks(root, target string) bool {
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return false
	}
	targetAbs, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	if rel == "." {
		return true
	}
	current := rootAbs
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return false
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return true
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

// verifiedCacheMaterialization recognizes the normal symlink/hardlink cache
// forms without rereading multi-gigabyte files. When the platform falls back
// to a regular copy, it verifies that copy against the durable content hash.
func verifiedCacheMaterialization(path, source, expectedHash string, expectedSize int64) bool {
	pathInfo, err := os.Stat(path)
	if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Size() != expectedSize {
		return false
	}
	sourceInfo, err := os.Stat(source)
	if err != nil || !sourceInfo.Mode().IsRegular() || sourceInfo.Size() != expectedSize {
		return false
	}
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		actual, err := filepath.EvalSymlinks(path)
		expected, expectedErr := filepath.EvalSymlinks(source)
		return err == nil && expectedErr == nil && pathIdentityKey(actual) == pathIdentityKey(expected)
	}
	if os.SameFile(pathInfo, sourceInfo) {
		return true
	}
	actualHash, err := hashFileSHA256(path)
	return err == nil && actualHash == expectedHash
}

func hashFileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
