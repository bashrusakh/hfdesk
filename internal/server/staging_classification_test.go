// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

const (
	stagingTestSHA     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	stagingTestContent = "published blob content"
)

func stagingArtifactNames() []string {
	return []string{
		"tmp-" + stagingTestSHA,
		"tmp-" + stagingTestSHA + ".part",
		"tmp-" + stagingTestSHA + ".part.meta",
		"tmp-" + stagingTestSHA + ".lock",
		stagingTestSHA + ".incomplete",
		stagingTestSHA + ".incomplete.meta",
		stagingTestSHA + ".incomplete.meta.tmp",
		stagingTestSHA + ".incomplete.lock",
		stagingTestSHA + ".lock",
		stagingTestSHA + ".part-00",
		stagingTestSHA + ".parts.json",
		stagingTestSHA + ".tmp-123456789",
	}
}

func stagingFixtureRepo(t *testing.T, hub string) (string, map[string]string) {
	t.Helper()
	repo := filepath.Join(hub, "models--owner--model")
	blobs := filepath.Join(repo, "blobs")
	snapshots := filepath.Join(repo, "snapshots", "commit123")
	for _, dir := range []string{blobs, snapshots, filepath.Join(repo, "refs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	published := map[string]string{
		stagingTestSHA:               stagingTestContent,
		"unusual-published-name.bin": "unusual content",
		"selected-blob":              "selected payload",
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(blobs, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range published {
		write(name, content)
	}
	for _, name := range stagingArtifactNames() {
		write(name, "staging artifact")
	}

	// A snapshot entry whose name has a staging shape OUTSIDE blobs/: it is
	// content and must be copied and verified.
	if err := os.WriteFile(filepath.Join(snapshots, "tokenizer.model.meta"), []byte("snapshot metadata"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "blobs", stagingTestSHA), filepath.Join(snapshots, "weights.safetensors")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "blobs", "selected-blob"), filepath.Join(snapshots, "model-Q4_K_M.gguf")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "refs", "main"), []byte("commit123"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, published
}

func stagingBlobsOnlyRepo(t *testing.T, hub string) string {
	t.Helper()
	repo := filepath.Join(hub, "models--owner--blobs-only")
	blobs := filepath.Join(repo, "blobs")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobs, stagingTestSHA), []byte(stagingTestContent), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range stagingArtifactNames() {
		if err := os.WriteFile(filepath.Join(blobs, name), []byte("staging artifact"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func TestStagingArtifactsAreNotCacheContent(t *testing.T) {
	isolateCacheState(t)
	cacheRoot := t.TempDir()
	hub := filepath.Join(cacheRoot, "hub")
	repo, published := stagingFixtureRepo(t, hub)
	blobsOnly := stagingBlobsOnlyRepo(t, hub)

	publishedCount := len(published) + 1
	publishedSize := int64(len(stagingTestContent))
	for _, content := range published {
		publishedSize += int64(len(content))
	}

	if got := countBlobs(repo); got != len(published) {
		t.Errorf("countBlobs = %d, want %d published blobs", got, len(published))
	}
	if got := countBlobs(blobsOnly); got != 1 {
		t.Errorf("countBlobs of the blobs-only repository = %d, want 1 published blob", got)
	}
	entries, err := scanHubForMirror(hub)
	if err != nil {
		t.Fatalf("scanHubForMirror: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("scanHubForMirror returned %d repos, want 2", len(entries))
	}
	modelSize := int64(0)
	for _, content := range published {
		modelSize += int64(len(content))
	}
	for _, e := range entries {
		want := modelSize
		if e.Repo == "owner/blobs-only" {
			want = int64(len(stagingTestContent))
		}
		if e.Size != want {
			t.Errorf("mirror size of %s = %d, want the published content only (%d)", e.Repo, e.Size, want)
		}
	}

	dstHub := filepath.Join(t.TempDir(), "hub")
	if err := copyRepoCache(repo, hub, dstHub); err != nil {
		t.Fatalf("copyRepoCache: %v", err)
	}
	dstBlobs := filepath.Join(dstHub, "models--owner--model", "blobs")
	for name, content := range published {
		if data, err := os.ReadFile(filepath.Join(dstBlobs, name)); err != nil || string(data) != content {
			t.Errorf("published blob %s was not copied byte-for-byte: %q %v", name, data, err)
		}
	}
	for _, name := range stagingArtifactNames() {
		if _, err := os.Stat(filepath.Join(dstBlobs, name)); !os.IsNotExist(err) {
			t.Errorf("staging artifact %s was mirrored, stat err=%v", name, err)
		}
	}

	s := newTestServerWithConfig(t, Config{CacheDir: cacheRoot})
	w := cacheRequest(t, s, "GET", "/api/cache", "")
	if w.Code != http.StatusOK {
		t.Fatalf("cache stats: status %d body %s", w.Code, w.Body.String())
	}
	var listed struct {
		Stats CacheStats `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Stats.TotalFiles != publishedCount {
		t.Errorf("cache stats TotalFiles = %d, want %d published files", listed.Stats.TotalFiles, publishedCount)
	}
	if listed.Stats.TotalSize != publishedSize {
		t.Errorf("cache stats TotalSize = %d, want %d", listed.Stats.TotalSize, publishedSize)
	}

	w = cacheRequest(t, s, "GET", "/api/cache/owner/blobs-only", "")
	if w.Code != http.StatusOK {
		t.Fatalf("cache listing: status %d body %s", w.Code, w.Body.String())
	}
	var info CachedRepoInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if len(info.Files) != 1 {
		t.Errorf("cache listing shows %d files, want 1 published file: %+v", len(info.Files), info.Files)
	}
	for _, f := range info.Files {
		if f.Name != stagingTestSHA {
			t.Errorf("cache listing shows %q, want the published blob %q", f.Name, stagingTestSHA)
		}
	}

	stageOnly := filepath.Join(t.TempDir(), "models--owner--staging")
	if err := os.MkdirAll(filepath.Join(stageOnly, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageOnly, "blobs", "tmp-"+stagingTestSHA+".part"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	stageEntries, err := os.ReadDir(filepath.Join(stageOnly, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	if hasStoredBlobEntries(stageEntries) {
		t.Error("a blobs/ directory with only staging artifacts was treated as populated")
	}
}

func TestStagingArtifactsDoNotAffectTheDeletionPath(t *testing.T) {
	root := t.TempDir()
	hub := filepath.Join(root, "hub")
	stagingFixtureRepo(t, hub)

	cache := hfdownloader.NewHFCache(root)
	rd, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	result := rd.DeleteSelectedGGUF([]hfdownloader.SelectedGGUFEntry{
		{Path: "model-Q4_K_M.gguf", Versions: []string{"commit123"}},
	})
	if len(result.Errors) != 0 {
		t.Fatalf("DeleteSelectedGGUF: %+v", result)
	}
	if len(result.Removed) == 0 {
		t.Fatalf("the selected entry was not removed: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(rd.BlobsDir(), stagingTestSHA)); err != nil {
		t.Errorf("the unselected published blob was removed by the deletion path: %v", err)
	}
	for _, name := range stagingArtifactNames() {
		if _, err := os.Stat(filepath.Join(rd.BlobsDir(), name)); err != nil {
			t.Errorf("staging artifact %s was removed by the deletion path: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rd.BlobsDir(), "unusual-published-name.bin")); err != nil {
		t.Errorf("the unusual published blob was removed by the deletion path: %v", err)
	}
}

// A non-reserved unusual name remains valid content across storage and mirror
// surfaces; only reserved staging shapes are excluded.
func TestUnusualPublishedBlobNameIsStoredVisibleAndMirrored(t *testing.T) {
	isolateCacheState(t)
	cacheRoot := t.TempDir()
	hub := filepath.Join(cacheRoot, "hub")

	cache := hfdownloader.NewHFCache(cacheRoot)
	rd, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	const name = "unusual-published-name.bin"
	if hfdownloader.IsStagingArtifactName(name) {
		t.Fatalf("the fixture name %q is a staging artifact", name)
	}
	content := "unusual published content"
	temp := filepath.Join(t.TempDir(), "download")
	if err := os.WriteFile(temp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	stored, err := rd.StoreDownloadedFile(temp, "model-Q4_K_M.gguf", "commit123", name, "", false)
	if err != nil {
		t.Fatalf("the public storage API refused an unusual but non-reserved name: %v", err)
	}
	if stored.SHA256 != name {
		t.Errorf("stored SHA256 = %q, want %q", stored.SHA256, name)
	}
	if data, err := os.ReadFile(stored.BlobPath); err != nil || string(data) != content {
		t.Fatalf("published blob = %q (err=%v)", data, err)
	}

	repo := filepath.Join(hub, "models--owner--model")
	if got := countBlobs(repo); got != 1 {
		t.Errorf("countBlobs = %d, want the unusual published blob", got)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: cacheRoot})
	w := cacheRequest(t, s, "GET", "/api/cache", "")
	if w.Code != http.StatusOK {
		t.Fatalf("cache stats: status %d body %s", w.Code, w.Body.String())
	}
	var listed struct {
		Stats CacheStats `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Stats.TotalFiles != 1 {
		t.Errorf("cache stats TotalFiles = %d, want 1 published file", listed.Stats.TotalFiles)
	}
	if listed.Stats.TotalSize != int64(len(content)) {
		t.Errorf("cache stats TotalSize = %d, want %d", listed.Stats.TotalSize, len(content))
	}

	entries, err := scanHubForMirror(hub)
	if err != nil {
		t.Fatalf("scanHubForMirror: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("scanHubForMirror returned %d repos, want 1", len(entries))
	}
	dstHub := filepath.Join(t.TempDir(), "hub")
	if err := copyRepoCache(repo, hub, dstHub); err != nil {
		t.Fatalf("copyRepoCache: %v", err)
	}
	dstBlob := filepath.Join(dstHub, "models--owner--model", "blobs", name)
	if data, err := os.ReadFile(dstBlob); err != nil || string(data) != content {
		t.Fatalf("the unusual published blob was not mirrored: %q %v", data, err)
	}
	if err := verifyRepoCache(repo, hub, dstHub); err != nil {
		t.Errorf("verifyRepoCache: %v", err)
	}
}

// Copy and verification must agree on snapshot .meta files and unusual blobs.
func TestMirrorCopyAndVerifyAgreeOnTheSameSet(t *testing.T) {
	hub := t.TempDir()
	repo, published := stagingFixtureRepo(t, hub)

	dstHub := filepath.Join(t.TempDir(), "hub")
	if err := copyRepoCache(repo, hub, dstHub); err != nil {
		t.Fatalf("copyRepoCache: %v", err)
	}
	dstRepo := filepath.Join(dstHub, "models--owner--model")

	snapshot := filepath.Join(dstRepo, "snapshots", "commit123", "tokenizer.model.meta")
	if data, err := os.ReadFile(snapshot); err != nil || string(data) != "snapshot metadata" {
		t.Fatalf("the snapshot entry was not copied: %q %v", data, err)
	}
	for name := range published {
		if _, err := os.Stat(filepath.Join(dstRepo, "blobs", name)); err != nil {
			t.Errorf("published blob %s was not copied: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dstRepo, "blobs", "tmp-"+stagingTestSHA+".part")); !os.IsNotExist(err) {
		t.Errorf("a staging artifact was copied, stat err=%v", err)
	}

	if err := verifyRepoCache(repo, hub, dstHub); err != nil {
		t.Fatalf("verifyRepoCache on a fresh copy: %v", err)
	}

	if err := os.WriteFile(snapshot, []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	verifyErr := verifyRepoCache(repo, hub, dstHub)
	if verifyErr == nil {
		t.Fatal("a corrupted snapshot entry was not detected: copy and verify disagree")
	}
	if !strings.Contains(verifyErr.Error(), "tokenizer.model.meta") {
		t.Errorf("verification error = %v, want it to name the corrupted snapshot entry", verifyErr)
	}

	if err := os.WriteFile(snapshot, []byte("snapshot metadata"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dstRepo, "blobs", stagingTestSHA), []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyRepoCache(repo, hub, dstHub); err == nil {
		t.Error("a corrupted published blob was not detected")
	}
}
