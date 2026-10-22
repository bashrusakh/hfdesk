// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// recognizeCopiedBranchVariantStore recognizes a physical copy of a service
// store artifact by artifact. A complete, self-consistent artifact — a
// store-bound control record, a matching completion manifest, and the
// expected payload files — keeps its identity regardless of its siblings. An
// artifact that does not satisfy those conditions is simply not complete; it
// never declassifies the whole store. The store is recognized when at least
// one artifact corroborates its logical identity.
//
// It reads file sizes and symlink identity, not model contents, and never
// mutates the copy.
func recognizeCopiedBranchVariantStore(serviceRoot string, storeOwner *branchVariantStoreOwner) error {
	rootEntries, err := os.ReadDir(serviceRoot)
	if err != nil {
		return err
	}
	controlBase := filepath.Join(serviceRoot, "control")
	seenControl := false
	seenPayload := false
	for _, entry := range rootEntries {
		switch entry.Name() {
		case branchVariantStoreOwnerFilename:
			if !entry.Type().IsRegular() || entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("root owner record is not a regular file")
			}
		case "control":
			seenControl = true
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("control area is not an ordinary directory")
			}
		case "payloads":
			seenPayload = true
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("payload area is not an ordinary directory")
			}
		default:
			return fmt.Errorf("unrecognized service-root entry %q", entry.Name())
		}
	}
	if !seenControl || !seenPayload {
		return fmt.Errorf("copied service root is incomplete")
	}

	recognized := 0
	err = filepath.WalkDir(controlBase, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() || entry.Name() != branchVariantArtifactOwnerFilename {
			return nil
		}
		controlRoot := filepath.Dir(path)
		artifact, manifest, artifactErr := readCopiedBranchArtifact(serviceRoot, storeOwner.StoreID, controlRoot)
		if artifactErr != nil {
			// This artifact is not complete. It does not declassify the store:
			// its siblings keep their identity.
			return nil
		}
		for _, file := range manifest.Files {
			payloadPath, joinErr := SafeJoin(artifact.PayloadRoot, file.Name)
			if joinErr != nil {
				return nil
			}
			if validateCopiedBranchPayloadFile(payloadPath, serviceRoot, manifest, file) != nil {
				return nil
			}
		}
		recognized++
		return filepath.SkipAll
	})
	if err != nil {
		return err
	}
	if recognized == 0 {
		return fmt.Errorf("copied service root has no complete artifact controls")
	}
	return nil
}

type copiedBranchArtifact struct {
	artifact *BranchVariantArtifact
	manifest *DownloadManifest
}

type copiedPayloadExpectation struct {
	manifest *DownloadManifest
	file     ManifestFile
}

func validateCopiedBranchPayloadFile(path, serviceRoot string, manifest *DownloadManifest, file ManifestFile) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("payload is unavailable: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if manifest.RepoPath == "" {
			return fmt.Errorf("local payload is unexpectedly symlinked")
		}
		repoPath := filepath.FromSlash(manifest.RepoPath)
		if !filepath.IsAbs(repoPath) {
			repoPath = filepath.Join(filepath.Dir(serviceRoot), repoPath)
		}
		expectedSnapshot, err := SafeJoin(repoPath, filepath.Join("snapshots", manifest.Commit, filepath.FromSlash(file.Name)))
		if err != nil {
			return fmt.Errorf("resolve expected snapshot: %w", err)
		}
		info, err = os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			if err != nil {
				return fmt.Errorf("resolve payload link: %w", err)
			}
			return fmt.Errorf("payload link does not resolve to a regular file")
		}
		expectedInfo, err := os.Stat(expectedSnapshot)
		if err != nil || !expectedInfo.Mode().IsRegular() || !os.SameFile(info, expectedInfo) {
			if err != nil {
				return fmt.Errorf("expected snapshot is unavailable: %w", err)
			}
			return fmt.Errorf("payload link does not resolve to its recorded snapshot entry")
		}
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("payload is not a regular file")
	}
	if info.Size() != file.Size {
		return fmt.Errorf("payload has size %d, want %d", info.Size(), file.Size)
	}
	return nil
}

func readCopiedBranchArtifact(serviceRoot, storeID, controlRoot string) (*BranchVariantArtifact, *DownloadManifest, error) {
	if !ownedPathComponentsSafe(serviceRoot, controlRoot) {
		return nil, nil, fmt.Errorf("control path is unsafe")
	}
	data, err := readRegularJSON(filepath.Join(controlRoot, branchVariantArtifactOwnerFilename))
	if err != nil {
		return nil, nil, err
	}
	var owner branchVariantArtifactOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return nil, nil, err
	}
	if owner.Version != branchVariantStoreVersion || owner.StoreID != storeID || owner.Repo == "" || owner.Revision == "" || owner.Commit == "" {
		return nil, nil, fmt.Errorf("artifact owner does not match the copied store")
	}
	artifact, err := branchVariantArtifactPaths(serviceRoot, storeID, owner.RepoType, owner.Repo, owner.Revision, owner.Commit)
	if err != nil || !sameBranchVariantPath(artifact.ControlRoot, controlRoot) {
		return nil, nil, fmt.Errorf("artifact owner path is inconsistent")
	}
	manifest, err := ReadManifest(artifact.ManifestPath)
	if err != nil {
		return nil, nil, err
	}
	if !manifest.IsCompleteBranchVariant(owner.Repo, owner.RepoType, owner.Revision) || manifest.Commit != owner.Commit {
		return nil, nil, fmt.Errorf("artifact manifest is incomplete or mismatched")
	}
	return artifact, manifest, nil
}
