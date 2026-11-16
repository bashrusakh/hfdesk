// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
	"github.com/bashrusakh/hfdesk/pkg/smartdl"
)

// backdate pins a path's mtimes to a known instant so a fixture's published
// content carries a date distinct from the fresh staging artifacts written
// alongside it.
func backdate(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// findRepoEntry returns the listed entry for repoID, or nil.
func findRepoEntry(repos []CachedRepoInfo, repoID string) *CachedRepoInfo {
	for i := range repos {
		if repos[i].Repo == repoID {
			return &repos[i]
		}
	}
	return nil
}

// listedCacheRepos fetches GET /api/cache and returns its repository entries.
func listedCacheRepos(t *testing.T, s *Server) []CachedRepoInfo {
	t.Helper()
	w := cacheRequest(t, s, "GET", "/api/cache", "")
	if w.Code != http.StatusOK {
		t.Fatalf("cache listing: status %d body %s", w.Code, w.Body.String())
	}
	var listed struct {
		Repos []CachedRepoInfo `json:"repos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	return listed.Repos
}

// TestLocalCacheDownloadedDateExcludesStagingArtifacts pins the Local Cache
// "downloaded" date rule on both mtime-derived date surfaces: the HF-cache
// listing (which previously read the blobs DIRECTORY's own mtime) and the
// local-scan walk's newest-mtime tracker. A published file's date must win
// over a fresh staging artifact written by an active or interrupted download;
// no new name matching is introduced — the one staging classifier decides.
func TestLocalCacheDownloadedDateExcludesStagingArtifacts(t *testing.T) {
	publishedWhen := time.Date(2021, 5, 4, 10, 0, 0, 0, time.UTC)
	wantDate := publishedWhen.Format("2006-01-02")
	today := time.Now().Format("2006-01-02")

	// Surface 1: HF-cache entries from GET /api/cache.
	isolateCacheState(t)
	cacheRoot := t.TempDir()
	blobs := filepath.Join(cacheRoot, "hub", "models--owner--stagedate", "blobs")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	publishedBlob := filepath.Join(blobs, stagingTestSHA)
	if err := os.WriteFile(publishedBlob, []byte(stagingTestContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobs, stagingTestSHA+".incomplete"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	backdate(t, publishedBlob, publishedWhen)

	hubEntry := findRepoEntry(listedCacheRepos(t, newTestServerWithConfig(t, Config{CacheDir: cacheRoot})), "owner/stagedate")
	if hubEntry == nil {
		t.Fatal("hub repository was not listed")
	}
	if hubEntry.Downloaded != wantDate {
		t.Errorf("HF-cache downloaded date = %q, want %q (the fresh staging artifact must not move the date to %q)",
			hubEntry.Downloaded, wantDate, today)
	}

	// Surface 2: the local-scan walk behind Local/Friendly entries.
	scanBase := t.TempDir()
	repoDir := filepath.Join(scanBase, "owner", "stagedate")
	if err := os.MkdirAll(filepath.Join(repoDir, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	localPublished := filepath.Join(repoDir, "model.gguf")
	if err := os.WriteFile(localPublished, []byte("real weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "blobs", stagingTestSHA+".incomplete"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	backdate(t, localPublished, publishedWhen)

	repos, err := scanLocalCachedRepos(t.TempDir(), "", []string{scanBase}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	localEntry := findRepoEntry(repos, "owner/stagedate")
	if localEntry == nil {
		t.Fatalf("local repository was not listed: %+v", repos)
	}
	if localEntry.Downloaded != wantDate {
		t.Errorf("local-scan downloaded date = %q, want %q (the fresh in-blobs staging artifact must not move the date to %q)",
			localEntry.Downloaded, wantDate, today)
	}
}

// TestLocalCacheDownloadedDateEmptyWhenOnlyStagingArtifacts pins the no-date
// representation: a hub repository whose blobs hold ONLY staging artifacts
// (an active or interrupted download) shows no downloaded date — it must not
// read like "today". The local-scan surface never lists a staging-only folder
// (TestLocalCacheScanDoesNotListStagingOnlyRepositories), so the HF-cache
// listing is the surface that can carry such an entry.
func TestLocalCacheDownloadedDateEmptyWhenOnlyStagingArtifacts(t *testing.T) {
	isolateCacheState(t)
	cacheRoot := t.TempDir()
	blobs := filepath.Join(cacheRoot, "hub", "models--owner--pending", "blobs")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range stagingArtifactNames() {
		if err := os.WriteFile(filepath.Join(blobs, name), []byte("staging artifact"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	entry := findRepoEntry(listedCacheRepos(t, newTestServerWithConfig(t, Config{CacheDir: cacheRoot})), "owner/pending")
	if entry == nil {
		t.Fatal("staging-only hub repository was not listed")
	}
	if entry.Downloaded != "" {
		t.Errorf("staging-only downloaded date = %q, want no date (it must not read like %q)",
			entry.Downloaded, time.Now().Format("2006-01-02"))
	}
}

// TestLocalCacheDownloadedDatePublishedOnlyUnchanged pins the preserved
// neighbor: a folder whose files are all published keeps the pre-fix date —
// the newest published mtime wins, on the local scan and on the HF-cache
// listing alike.
func TestLocalCacheDownloadedDatePublishedOnlyUnchanged(t *testing.T) {
	olderWhen := time.Date(2021, 5, 4, 10, 0, 0, 0, time.UTC)
	newerWhen := time.Date(2022, 7, 8, 9, 30, 0, 0, time.UTC)
	wantDate := newerWhen.Format("2006-01-02")

	// Local scan: two published files with distinct mtimes → newest wins.
	scanBase := t.TempDir()
	repoDir := filepath.Join(scanBase, "owner", "publishedonly")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, when := range map[string]time.Time{
		"model.gguf":        olderWhen,
		"extra.safetensors": newerWhen,
	} {
		p := filepath.Join(repoDir, name)
		if err := os.WriteFile(p, []byte("real weights"), 0o644); err != nil {
			t.Fatal(err)
		}
		backdate(t, p, when)
	}
	repos, err := scanLocalCachedRepos(t.TempDir(), "", []string{scanBase}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	entry := findRepoEntry(repos, "owner/publishedonly")
	if entry == nil {
		t.Fatalf("published-only repository was not listed: %+v", repos)
	}
	if entry.Downloaded != wantDate {
		t.Errorf("published-only local-scan downloaded date = %q, want %q", entry.Downloaded, wantDate)
	}

	// HF-cache listing: two published blobs, the blobs directory's own mtime
	// aligned with the newest one, so the listing date is the same before and
	// after the fix.
	isolateCacheState(t)
	cacheRoot := t.TempDir()
	blobs := filepath.Join(cacheRoot, "hub", "models--owner--publishedonly", "blobs")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	olderBlob := filepath.Join(blobs, stagingTestSHA)
	newerBlob := filepath.Join(blobs, "unusual-published-name.bin")
	for p, when := range map[string]time.Time{olderBlob: olderWhen, newerBlob: newerWhen} {
		if err := os.WriteFile(p, []byte("published content"), 0o644); err != nil {
			t.Fatal(err)
		}
		backdate(t, p, when)
	}
	backdate(t, blobs, newerWhen)

	hubEntry := findRepoEntry(listedCacheRepos(t, newTestServerWithConfig(t, Config{CacheDir: cacheRoot})), "owner/publishedonly")
	if hubEntry == nil {
		t.Fatal("published-only hub repository was not listed")
	}
	if hubEntry.Downloaded != wantDate {
		t.Errorf("published-only HF-cache downloaded date = %q, want %q", hubEntry.Downloaded, wantDate)
	}
}

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
	// F2(a): an in-namespace staging name nested under blobs/sub/ must not
	// make the staging-only directory listable either.
	subStaging := filepath.Join(stagingOnly, "blobs", "sub", "tmp-model.gguf")
	if err := os.MkdirAll(filepath.Dir(subStaging), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(subStaging, []byte("staging"), 0o644); err != nil {
		t.Fatal(err)
	}
	// F2(b): outside the repository-root blobs/ namespace, a staging-shaped
	// name in a nested snapshots/<c>/blobs/ directory is content and keeps
	// the directory's weight status.
	if err := os.MkdirAll(filepath.Join(scanBase, "owner", "nested-weight", "snapshots", "commit1", "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scanBase, "owner", "nested-weight", "snapshots", "commit1", "blobs", "tmp-model.gguf"), []byte("real weight bytes"), 0o644); err != nil {
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
	if !listed["owner/nested-weight"] {
		t.Error("a repo whose only weight sits under a nested snapshots/<c>/blobs/ dir was not listed")
	}
}

// TestLocalCacheStatsBlobsNamespaceAndStagingIdentity pins, on one scan:
// F2 stats cells — staging shapes anywhere under the repository-root blobs/
// subtree count nothing (even nested in blobs/sub/), while a nested
// snapshots/<c>/blobs/ directory is OUTSIDE the namespace and counts as
// content; F3 cells — a physical staging identity counts nowhere even
// through a content-named symlink or hard link, while an independent copy
// counts; and the grouped metadata cell — staging names never leak a
// phantom Quantizations entry.
func TestLocalCacheStatsBlobsNamespaceAndStagingIdentity(t *testing.T) {
	scanBase := t.TempDir()
	writeAll := func(files map[string]string) {
		for rel, data := range files {
			p := filepath.Join(scanBase, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	const staged, lock = "staged payload", "nested lock bytes"
	writeAll(map[string]string{
		"owner/proj-symlink/blobs/tmp-model.gguf":                          staged,
		"owner/proj-symlink/snapshots/commit1/weights.safetensors":         "weights",
		"owner/proj-hardlink/blobs/tmp-model.gguf":                         staged,
		"owner/proj-hardlink/snapshots/commit1/weights.safetensors":        "weights",
		"owner/proj-hardlink/snapshots/commit1/real-copy.gguf":             staged,
		"owner/nested-ns/blobs/tmp-Q5_K_XL.gguf":                           "phantom quant bait",
		"owner/nested-ns/blobs/sub/" + stagingTestSHA[:16] + ".incomplete": "in-ns nested staging",
		"owner/nested-ns/model-Q4_K_M.gguf":                                "real weight",
		"owner/nested-out/snapshots/commit1/blobs/x.lock":                  lock,
		"owner/nested-out/model.gguf":                                      "real weight",
	})
	if err := os.Symlink("../../blobs/tmp-model.gguf", filepath.Join(scanBase, filepath.FromSlash("owner/proj-symlink/snapshots/commit1/copied.gguf"))); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(scanBase, "owner", "proj-hardlink", "blobs", "tmp-model.gguf"), filepath.Join(scanBase, "owner", "proj-hardlink", "snapshots", "commit1", "linked.gguf")); err != nil {
		t.Fatal(err)
	}

	repos, err := scanLocalCachedRepos(t.TempDir(), "", []string{scanBase}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]CachedRepoInfo)
	for _, entry := range repos {
		byID[entry.Repo] = entry
	}
	checkStats := func(id string, wantSize int64, wantCount int) {
		entry, ok := byID[id]
		if !ok {
			t.Fatalf("%s was not listed", id)
		}
		if entry.Size != wantSize || entry.FileCount != wantCount {
			t.Errorf("%s size/count = %d/%d, want %d/%d", id, entry.Size, entry.FileCount, wantSize, wantCount)
		}
	}
	checkStats("owner/proj-symlink", int64(len("weights")), 1)              // F3(i): symlink to staging counts nothing
	checkStats("owner/proj-hardlink", int64(len("weights")+len(staged)), 2) // F3(ii): hard link to staging counts nothing, independent copy counts
	checkStats("owner/nested-ns", int64(len("real weight")), 1)             // F2(a): in-namespace nested staging excluded
	checkStats("owner/nested-out", int64(len(lock)+len("real weight")), 2)  // F2(b): outside the namespace counts

	if want := []string{"Q4_K_M"}; !reflect.DeepEqual(byID["owner/nested-ns"].Quantizations, want) {
		t.Errorf("nested-ns quantizations = %v, want %v (staging names must not leak metadata)", byID["owner/nested-ns"].Quantizations, want)
	}
}
