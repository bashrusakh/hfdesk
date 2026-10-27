// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

// Classification of the downloader's staging artifacts (issue #140
// follow-up, group 2).
//
// A cache's blobs/ directory holds published content and the downloader's own
// staging artifacts. The mirror, the cache statistics, the cache listing, the
// cache selection and the cache deletion must all agree on that distinction:
// an unfinished download or a legacy multipart residue is not ready content.
//
// The regressions below cover:
//
//	(a) every staging shape the downloader creates is excluded from the
//	    content surfaces, and the deletion path leaves the real blobs alone;
//	(b) a legitimate file OUTSIDE blobs/ - a snapshot entry named
//	    "tokenizer.model.meta" - is copied AND verified: copy and verify agree;
//	(c) a published blobs/<64-hex> is copied and verified as before;
//	(d) an unusual published blob name (the public storage API accepts it) is
//	    still copied, verified and counted;
//	(e) the negative controls: with the historical suffix-only classifier and
//	    the historical blobs/-only verification scope restored, the defects are
//	    observed again.

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

// stagingArtifactNames is the full set of staging shapes the downloader
// creates, in the blobs/ namespace.
func stagingArtifactNames() []string {
	return []string{
		"tmp-" + stagingTestSHA,
		"tmp-" + stagingTestSHA + ".part",
		"tmp-" + stagingTestSHA + ".part.meta",
		"tmp-" + stagingTestSHA + ".lock",
		stagingTestSHA + ".incomplete",
		stagingTestSHA + ".incomplete.meta",
		stagingTestSHA + ".incomplete.meta.tmp",
		stagingTestSHA + ".lock",
		stagingTestSHA + ".part-00",
		stagingTestSHA + ".parts.json",
		stagingTestSHA + ".tmp-123456789",
	}
}

// stagingFixtureRepo builds a cache repository with published content, an
// unusual published name, a full set of staging artifacts and snapshot
// entries. It returns the repository path and the published entries with
// their content.
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
	// A regular snapshot entry that links to the published blob.
	if err := os.Symlink(filepath.Join("..", "..", "blobs", stagingTestSHA), filepath.Join(snapshots, "weights.safetensors")); err != nil {
		t.Fatal(err)
	}
	// A selectable GGUF entry with its own published payload.
	if err := os.Symlink(filepath.Join("..", "..", "blobs", "selected-blob"), filepath.Join(snapshots, "model-Q4_K_M.gguf")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "refs", "main"), []byte("commit123"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, published
}

// stagingBlobsOnlyRepo builds a repository whose blobs/ directory holds one
// published blob and the full set of staging artifacts, and no snapshots: the
// cache listing falls back to the blobs/ directory for such a repository.
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

// TestStagingArtifactsAreNotCacheContent is regression (a): the mirror, the
// cache statistics, the cache listing and the cache selection must not treat
// a staging artifact as ready content.
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

	// The mirror's blob count and scan size see the published content only.
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

	// The mirror copies the published content and none of the staging
	// artifacts.
	dstHub := filepath.Join(t.TempDir(), "hub")
	if err := copyRepoCache(repo, hub, dstHub); err != nil {
		t.Fatalf("copyRepoCache: %v", err)
	}
	dstBlobs := filepath.Join(dstHub, "models--owner--model", "blobs")
	for name := range published {
		if _, err := os.Stat(filepath.Join(dstBlobs, name)); err != nil {
			t.Errorf("published blob %s was not copied: %v", name, err)
		}
	}
	for _, name := range stagingArtifactNames() {
		if _, err := os.Stat(filepath.Join(dstBlobs, name)); !os.IsNotExist(err) {
			t.Errorf("staging artifact %s was mirrored, stat err=%v", name, err)
		}
	}

	// The cache statistics count the published content only.
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

	// The cache listing of a repository without snapshots walks blobs/: it
	// shows the published blob only.
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

	// The cache selection ignores a blobs/ directory that holds nothing but
	// staging artifacts.
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

// TestStagingArtifactsDoNotAffectTheDeletionPath is the deletion half of
// regression (a): the deletion path must not mistake a staging artifact for a
// blob, and the real blobs keep the fate their selection gives them.
func TestStagingArtifactsDoNotAffectTheDeletionPath(t *testing.T) {
	root := t.TempDir()
	hub := filepath.Join(root, "hub")
	stagingFixtureRepo(t, hub)

	cache := hfdownloader.NewHFCache(root)
	rd, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	// The selection names ONE entry; the other published blobs are not
	// selected and must survive the deletion untouched.
	result := rd.DeleteSelectedGGUF([]hfdownloader.SelectedGGUFEntry{
		{Path: "model-Q4_K_M.gguf", Versions: []string{"commit123"}},
	})
	if len(result.Errors) != 0 {
		t.Fatalf("DeleteSelectedGGUF: %+v", result)
	}
	if len(result.Removed) == 0 {
		t.Fatalf("the selected entry was not removed: %+v", result)
	}
	// The published blob of the unselected entry is untouched.
	if _, err := os.Stat(filepath.Join(rd.BlobsDir(), stagingTestSHA)); err != nil {
		t.Errorf("the unselected published blob was removed by the deletion path: %v", err)
	}
	// The staging artifacts are not blobs: they are left alone.
	for _, name := range stagingArtifactNames() {
		if _, err := os.Stat(filepath.Join(rd.BlobsDir(), name)); err != nil {
			t.Errorf("staging artifact %s was removed by the deletion path: %v", name, err)
		}
	}
	// The unusual published name is a real blob: the deletion path treats it
	// as content, not as an artifact.
	if _, err := os.Stat(filepath.Join(rd.BlobsDir(), "unusual-published-name.bin")); err != nil {
		t.Errorf("the unusual published blob was removed by the deletion path: %v", err)
	}
}

// TestUnusualPublishedBlobNameIsStoredVisibleAndMirrored pins the other side
// of the store boundary (issue #140 follow-up).
//
// The public storage API refuses a blob key whose NAME belongs to the
// downloader's staging namespace, because such a blob would be invisible to
// every content surface. The refusal is narrow: an unusual but NON-reserved
// name is still accepted, and the blob it produces is stored, visible to the
// cache surfaces and mirrored.
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
	// VISIBLE: the blob is counted as content, and the domain lock the store
	// created next to it is not.
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

	// MIRRORED: the mirror copies the unusual published name and verifies it.
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

// TestMirrorCopyAndVerifyAgreeOnTheSameSet covers regressions (b), (c) and
// (d): the copy and the verification must cover the same set - a published
// blob, an unusual published name and a snapshot entry whose name has a
// staging shape - and none of the staging artifacts.
func TestMirrorCopyAndVerifyAgreeOnTheSameSet(t *testing.T) {
	hub := t.TempDir()
	repo, published := stagingFixtureRepo(t, hub)

	dstHub := filepath.Join(t.TempDir(), "hub")
	if err := copyRepoCache(repo, hub, dstHub); err != nil {
		t.Fatalf("copyRepoCache: %v", err)
	}
	dstRepo := filepath.Join(dstHub, "models--owner--model")

	// The snapshot entry with a staging-shaped name outside blobs/ is copied.
	snapshot := filepath.Join(dstRepo, "snapshots", "commit123", "tokenizer.model.meta")
	if data, err := os.ReadFile(snapshot); err != nil || string(data) != "snapshot metadata" {
		t.Fatalf("the snapshot entry was not copied: %q %v", data, err)
	}
	// The published content is copied, the staging artifacts are not.
	for name := range published {
		if _, err := os.Stat(filepath.Join(dstRepo, "blobs", name)); err != nil {
			t.Errorf("published blob %s was not copied: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dstRepo, "blobs", "tmp-"+stagingTestSHA+".part")); !os.IsNotExist(err) {
		t.Errorf("a staging artifact was copied, stat err=%v", err)
	}

	// A fresh copy verifies.
	if err := verifyRepoCache(repo, hub, dstHub); err != nil {
		t.Fatalf("verifyRepoCache on a fresh copy: %v", err)
	}

	// A corrupted snapshot entry is DETECTED: the copy and the verification
	// cover the same set.
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

	// A corrupted published blob is detected as well.
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

// TestNegativeControlOldSuffixOnlyClassifierCountsStagingArtifacts is the
// negative control for the classification fix: with the historical
// suffix-only predicate restored, the staging artifacts ARE counted and
// copied - the defect the exhaustive classifier removes.
func TestNegativeControlOldSuffixOnlyClassifierCountsStagingArtifacts(t *testing.T) {
	old := isBlobStagingArtifactName
	defer func() { isBlobStagingArtifactName = old }()
	// The historical predicate: the four suffixes the old classifier knew.
	isBlobStagingArtifactName = func(name string) bool {
		return strings.HasSuffix(name, ".incomplete") ||
			strings.HasSuffix(name, ".meta") ||
			strings.HasSuffix(name, ".meta.tmp") ||
			strings.HasSuffix(name, ".lock")
	}

	hub := t.TempDir()
	repo, published := stagingFixtureRepo(t, hub)

	if got := countBlobs(repo); got <= len(published) {
		t.Fatalf("the negative control did not reproduce the defect: countBlobs = %d, want more than the %d published blobs", got, len(published))
	}
	dstHub := filepath.Join(t.TempDir(), "hub")
	if err := copyRepoCache(repo, hub, dstHub); err != nil {
		t.Fatalf("copyRepoCache: %v", err)
	}
	dstBlobs := filepath.Join(dstHub, "models--owner--model", "blobs")
	if _, err := os.Stat(filepath.Join(dstBlobs, "tmp-"+stagingTestSHA+".part")); err != nil {
		t.Fatalf("the negative control did not reproduce the defect: the single-request partial was not copied: %v", err)
	}
	t.Logf("negative control reproduced the miscount: countBlobs = %d", countBlobs(repo))
}

// TestNegativeControlOldVerifyScopeMissesSnapshotEntries is the negative
// control for the copy/verify agreement: with the historical blobs/-only
// verification scope restored, a corrupted snapshot entry is NOT detected.
func TestNegativeControlOldVerifyScopeMissesSnapshotEntries(t *testing.T) {
	old := mirrorVerifyCoversEntry
	defer func() { mirrorVerifyCoversEntry = old }()
	// The historical scope: only the blobs/ directory was verified.
	mirrorVerifyCoversEntry = func(rel string) bool {
		return filepath.Dir(rel) == "blobs"
	}

	hub := t.TempDir()
	repo, _ := stagingFixtureRepo(t, hub)
	dstHub := filepath.Join(t.TempDir(), "hub")
	if err := copyRepoCache(repo, hub, dstHub); err != nil {
		t.Fatalf("copyRepoCache: %v", err)
	}
	snapshot := filepath.Join(dstHub, "models--owner--model", "snapshots", "commit123", "tokenizer.model.meta")
	if err := os.WriteFile(snapshot, []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyRepoCache(repo, hub, dstHub); err != nil {
		t.Fatalf("the negative control did not reproduce the defect: the corrupted snapshot entry was detected: %v", err)
	}
	t.Log("negative control reproduced the unverified snapshot entry")
}
