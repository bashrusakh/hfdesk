// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
	"github.com/bashrusakh/hfdesk/pkg/smartdl"
)

// TestLocalCacheStatsExcludeStagingAndCountPhysicalFilesOnce pins the local
// scan statistics rule (issue #146): size/fileCount count each distinct
// published physical file exactly once — a snapshot or friendly-view symlink
// contributes only through the physical file it resolves to, and a dangling
// link contributes nothing — while staging artifacts under blobs/ never
// contribute at all. The files[] path enumeration and the listing skip of a
// repository without any weight-named file stay unchanged.
func TestLocalCacheStatsExcludeStagingAndCountPhysicalFilesOnce(t *testing.T) {
	scanBase := t.TempDir()
	hub := filepath.Join(scanBase, "hub")
	repo, published := stagingFixtureRepo(t, hub)
	// A blobs-only repository (published + staging names, no weight-named
	// file anywhere) must stay unlisted by the scan.
	stagingBlobsOnlyRepo(t, hub)

	snapshots := filepath.Join(repo, "snapshots", "commit123")
	danglingTarget := filepath.Join("..", "..", "blobs",
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if err := os.Symlink(danglingTarget, filepath.Join(snapshots, "lost.gguf")); err != nil {
		t.Fatal(err)
	}

	cacheDir := t.TempDir()
	friendly := filepath.Join(cacheDir, "models", "friendowner", "friendmodel")
	if err := os.MkdirAll(friendly, 0o755); err != nil {
		t.Fatal(err)
	}
	// The friendly leaf chains through the snapshot symlink to the blob.
	if err := os.Symlink(filepath.Join(snapshots, "weights.safetensors"), filepath.Join(friendly, "weights.gguf")); err != nil {
		t.Fatal(err)
	}

	repos, err := scanLocalCachedRepos(cacheDir, "", []string{scanBase}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]CachedRepoInfo)
	for _, entry := range repos {
		byID[entry.Repo] = entry
	}
	if len(repos) != 2 {
		t.Fatalf("scan listed %d repositories (%v), want exactly the hub-layout repo and the friendly repo", len(repos), byID)
	}
	if _, ok := byID["hub/models--owner--blobs-only"]; ok {
		t.Error("a repository without any GGUF/safetensors-named file was listed")
	}

	hubRepo, ok := byID["hub/models--owner--model"]
	if !ok {
		t.Fatalf("hub-layout repository was not listed: %v", byID)
	}
	publishedSize := int64(0)
	for _, content := range published {
		publishedSize += int64(len(content))
	}
	// 3 published blobs + refs/main + the snapshot .meta file = 5 physical
	// files. The two snapshot symlinks resolve to already-counted blobs, the
	// dangling link resolves to nothing, and the 12 staging artifacts in
	// blobs/ never contribute.
	wantSize := publishedSize + int64(len("commit123")) + int64(len("snapshot metadata"))
	if hubRepo.Size != wantSize || hubRepo.FileCount != 5 {
		t.Errorf("hub stats size/count = %d/%d, want %d/5 (staging excluded, each physical file counted once)",
			hubRepo.Size, hubRepo.FileCount, wantSize)
	}

	// The statistics fix changes only size/fileCount: files[] still
	// enumerates every path present (3 blobs + 12 staging + refs + snapshot
	// meta + 2 snapshot links + 1 dangling link = 20).
	if len(hubRepo.Files) != 20 {
		t.Errorf("files[] lists %d entries, want the unchanged enumeration of 20", len(hubRepo.Files))
	}
	listedNames := make(map[string]bool)
	for _, file := range hubRepo.Files {
		listedNames[file.Name] = true
	}
	for _, name := range []string{
		filepath.Join("blobs", stagingTestSHA+".incomplete"),
		filepath.Join("blobs", stagingTestSHA+".incomplete.meta"),
		filepath.Join("blobs", "tmp-"+stagingTestSHA+".part"),
		filepath.Join("snapshots", "commit123", "model-Q4_K_M.gguf"),
	} {
		if !listedNames[name] {
			t.Errorf("files[] no longer enumerates %s", name)
		}
	}

	friendlyRepo, ok := byID["friendowner/friendmodel"]
	if !ok {
		t.Fatalf("friendly-view repository was not listed: %v", byID)
	}
	if friendlyRepo.Size != int64(len(stagingTestContent)) || friendlyRepo.FileCount != 1 {
		t.Errorf("friendly stats size/count = %d/%d, want %d/1 (the link contributes its target exactly once)",
			friendlyRepo.Size, friendlyRepo.FileCount, int64(len(stagingTestContent)))
	}
}

// TestLocalCacheStatsCountHardLinkedProjectionsOnce pins physical-file
// identity: the no-symlink linkplace fallback publishes payloads into
// snapshot paths as HARD LINKS (one physical file, two paths), so such a
// projection must count once, while an independent copy is a second
// physical file and must count twice.
func TestLocalCacheStatsCountHardLinkedProjectionsOnce(t *testing.T) {
	scanBase := t.TempDir()
	repo := filepath.Join(scanBase, "owner", "linked")
	blobs := filepath.Join(repo, "blobs")
	snapshots := filepath.Join(repo, "snapshots", "commit123")
	for _, dir := range []string{blobs, snapshots} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	content := "hard linked payload"
	blob := filepath.Join(blobs, stagingTestSHA)
	if err := os.WriteFile(blob, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(blob, filepath.Join(snapshots, "weights.safetensors")); err != nil {
		t.Fatalf("hard-link fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "copied.safetensors"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	repos, err := scanLocalCachedRepos(t.TempDir(), "", []string{scanBase}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var entry *CachedRepoInfo
	for i := range repos {
		if repos[i].Repo == "owner/linked" {
			entry = &repos[i]
		}
	}
	if entry == nil {
		t.Fatalf("hard-linked repository was not listed: %v", repos)
	}
	wantSize := int64(2 * len(content))
	if entry.Size != wantSize || entry.FileCount != 2 {
		t.Errorf("size/count = %d/%d, want %d/2 (hard-linked projection counted once, independent copy counted separately)",
			entry.Size, entry.FileCount, wantSize)
	}
}

// TestLocalCacheScanDoesNotListStagingOnlyRepositories pins the listing
// invariant against the .gguf-named staging destination: blobs/tmp-<file>
// .gguf carries a weight-like name but is a staging artifact, so a directory
// whose only weight-like names are staging shapes must not be listed, while
// one real GGUF beside staging artifacts still qualifies a directory.
func TestLocalCacheScanDoesNotListStagingOnlyRepositories(t *testing.T) {
	if !hfdownloader.IsStagingArtifactName("tmp-model.gguf") || !smartdl.IsGGUFFileName("tmp-model.gguf") {
		t.Fatal("fixture assumption changed: tmp-<file>.gguf must be a staging artifact with a weight-like name")
	}
	scanBase := t.TempDir()
	stagingOnly := filepath.Join(scanBase, "owner", "staging-only")
	mixed := filepath.Join(scanBase, "owner", "mixed")
	stagingNames := []string{
		"tmp-model.gguf",
		"tmp-model.gguf.part",
		stagingTestSHA + ".incomplete",
		stagingTestSHA + ".incomplete.meta",
	}
	for _, repoDir := range []string{stagingOnly, mixed} {
		blobs := filepath.Join(repoDir, "blobs")
		if err := os.MkdirAll(blobs, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range stagingNames {
			if err := os.WriteFile(filepath.Join(blobs, name), []byte("staging"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(mixed, "model.gguf"), []byte("real weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Second shape: a scan root pointing at a hub makes the staging-only
	// blobs directory itself a candidate repository directory.
	hubRoot := filepath.Join(t.TempDir(), "hub")
	directBlobs := filepath.Join(hubRoot, "models--owner--staging", "blobs")
	if err := os.MkdirAll(directBlobs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range stagingNames {
		if err := os.WriteFile(filepath.Join(directBlobs, name), []byte("staging"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	repos, err := scanLocalCachedRepos(t.TempDir(), "", []string{scanBase, hubRoot}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	listed := make(map[string]bool)
	for _, entry := range repos {
		listed[entry.Repo] = true
	}
	if listed["owner/staging-only"] {
		t.Error("a directory whose only weight-like name is a staging artifact was listed as a repository")
	}
	if listed["models--owner--staging/blobs"] {
		t.Error("a staging-only blobs directory was listed as a repository")
	}
	if !listed["owner/mixed"] {
		t.Error("a directory with one real GGUF beside staging artifacts was not listed")
	}
}
