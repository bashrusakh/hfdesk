// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func selectedDeleteCacheRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve physical test temp root: %v", err)
	}
	return filepath.Join(root, "cache")
}

func selectedDeleteFixture(t *testing.T) (*RepoDir, string, string, string) {
	t.Helper()
	cache := NewHFCache(selectedDeleteCacheRoot(t))
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	blob := repo.BlobPath("blob-a")
	if err := os.WriteFile(blob, []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(repo.SnapshotsDir(), "version-a", "model-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(snapshot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "blobs", "blob-a"), snapshot); err != nil {
		t.Fatal(err)
	}
	friendly := filepath.Join(repo.FriendlyPath(), "model-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(friendly), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(snapshot, friendly); err != nil {
		t.Fatal(err)
	}
	return repo, blob, snapshot, friendly
}

func TestCleanHFRelativeUsesSlashNormalizedWirePaths(t *testing.T) {
	if !cleanHFRelative("subdir/model-Q4_K_M.gguf") {
		t.Fatal("slash-normalized relative path was rejected")
	}
	for _, name := range []string{`subdir\\model.gguf`, "/absolute/model.gguf", "C:/outside.gguf", "../outside.gguf", "subdir/../model.gguf"} {
		if cleanHFRelative(name) {
			t.Errorf("unsafe wire path accepted: %q", name)
		}
	}
}

func TestDeleteSelectedGGUFSupportsLegacySplitSuffix(t *testing.T) {
	repo, blob, _, _ := selectedDeleteFixture(t)
	const name = "model-Q4_K_M-00001-of-00002.gguf-split"
	snapshot := filepath.Join(repo.SnapshotsDir(), "version-split", name)
	if err := os.MkdirAll(filepath.Dir(snapshot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "blobs", "blob-a"), snapshot); err != nil {
		t.Fatal(err)
	}
	friendly := filepath.Join(repo.FriendlyPath(), name)
	if err := os.MkdirAll(filepath.Dir(friendly), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(snapshot, friendly); err != nil {
		t.Fatal(err)
	}
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: name, Versions: []string{"version-split"}}})
	if len(result.Errors) != 0 {
		t.Fatalf("delete errors: %v", result.Errors)
	}
	for _, path := range []string{snapshot, friendly} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("selected .gguf-split entry remains: %s (%v)", path, err)
		}
	}
	if _, err := os.Lstat(blob); err != nil {
		t.Fatalf("shared blob changed by .gguf-split deletion: %v", err)
	}
}

func TestDeleteSelectedGGUFRetainsDirectFriendlyBlobDependency(t *testing.T) {
	repo, blob, snapshot, friendly := selectedDeleteFixture(t)
	unrelatedPartial := repo.IncompletePath("unrelated")
	unrelatedMeta := repo.IncompleteMetaPath("unrelated")
	for _, name := range []string{unrelatedPartial, unrelatedMeta} {
		if err := os.WriteFile(name, []byte("keep"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	otherName := filepath.Join(repo.FriendlyPath(), "model-Q5_K_M.gguf")
	if err := os.Symlink(blob, otherName); err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(repo.FriendlyPath(), "README.md")
	if err := os.WriteFile(readme, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
	if len(result.Errors) != 0 {
		t.Fatalf("delete errors: %v", result.Errors)
	}
	if _, err := os.Lstat(snapshot); !os.IsNotExist(err) {
		t.Fatalf("selected snapshot remains: %v", err)
	}
	if _, err := os.Lstat(friendly); !os.IsNotExist(err) {
		t.Fatalf("selected friendly projection remains: %v", err)
	}
	for _, name := range []string{blob, otherName, readme, unrelatedPartial, unrelatedMeta} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("retained entry %q changed: %v", name, err)
		}
	}
	if len(result.RetainedBlobs) != 1 || result.RetainedBlobs[0] != blob {
		t.Fatalf("retained payload reporting=%v, want %q", result.RetainedBlobs, blob)
	}
}

func TestDeleteSelectedGGUFRetainsSnapshotWhenFriendlyUnlinkFails(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permission denial is not reliable on Windows or when running as root")
	}
	repo, blob, snapshot, friendly := selectedDeleteFixture(t)
	// Removing write permission makes unlinking the friendly name fail after
	// the full preflight has succeeded for an unprivileged test process.
	parent := filepath.Dir(friendly)
	if err := os.Chmod(parent, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0755); err != nil {
			t.Errorf("restore friendly directory permissions: %v", err)
		}
	})
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
	if !result.Attempted || len(result.Errors) == 0 || len(result.Remaining) == 0 {
		t.Fatalf("expected partial deletion after friendly unlink failure, got %+v", result)
	}
	if _, err := os.Lstat(friendly); err != nil {
		t.Fatalf("friendly name did not remain after unlink failure: %v", err)
	}
	if _, err := os.Lstat(snapshot); err != nil {
		t.Fatalf("selected snapshot was removed despite retained friendly name: %v", err)
	}
	if _, err := os.Stat(snapshot); err != nil {
		t.Fatalf("retained snapshot no longer resolves to its payload: %v", err)
	}
	if _, err := os.Stat(blob); err != nil {
		t.Fatalf("required blob was removed with retained snapshot: %v", err)
	}
	remaining := map[string]bool{}
	for _, name := range result.Remaining {
		remaining[name] = true
	}
	if !remaining[snapshot] || !remaining[friendly] {
		t.Fatalf("partial result omits retained dependency: %+v", result)
	}
}

func TestDeleteSelectedGGUFRefusesUnselectedFriendlySnapshotDependency(t *testing.T) {
	repo, blob, snapshot, friendly := selectedDeleteFixture(t)
	otherName := filepath.Join(repo.FriendlyPath(), "model-Q5_K_M.gguf")
	if err := os.Symlink(snapshot, otherName); err != nil {
		t.Fatal(err)
	}
	chainName := filepath.Join(repo.FriendlyPath(), "model-Q6_K.gguf")
	if err := os.Symlink(friendly, chainName); err != nil {
		t.Fatal(err)
	}
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
	if len(result.Errors) == 0 || len(result.Removed) != 0 {
		t.Fatalf("dependent friendly reference was not refused before unlink: %+v", result)
	}
	for _, name := range []string{blob, snapshot, friendly, otherName, chainName} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("refused operation changed %q: %v", name, err)
		}
	}
}

func TestDeleteSelectedGGUFRefusesUnknownFriendlyLinkCycleBeforeUnlink(t *testing.T) {
	repo, blob, snapshot, friendly := selectedDeleteFixture(t)
	a := filepath.Join(repo.FriendlyPath(), "a.gguf")
	b := filepath.Join(repo.FriendlyPath(), "b.gguf")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
	if len(result.Errors) == 0 || len(result.Removed) != 0 {
		t.Fatalf("friendly cycle was not refused before unlink: %+v", result)
	}
	for _, name := range []string{blob, snapshot, friendly, a, b} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("refused operation changed %q: %v", name, err)
		}
	}
}

func TestDeleteSelectedGGUFRetainsBlobDependencyThroughFriendlyDirectoryAlias(t *testing.T) {
	repo, blob, snapshot, friendly := selectedDeleteFixture(t)
	blobView := filepath.Join(repo.FriendlyPath(), "blob-view")
	if err := os.Symlink(repo.BlobsDir(), blobView); err != nil {
		t.Fatal(err)
	}
	otherName := filepath.Join(repo.FriendlyPath(), "model-Q5_K_M.gguf")
	if err := os.Symlink(filepath.Join(blobView, filepath.Base(blob)), otherName); err != nil {
		t.Fatal(err)
	}
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
	if len(result.Errors) != 0 || len(result.RetainedBlobs) != 1 || result.RetainedBlobs[0] != blob {
		t.Fatalf("directory-alias blob dependency was not retained/reported: %+v", result)
	}
	if _, err := os.Stat(otherName); err != nil {
		t.Fatalf("retained Q5 view no longer resolves after selected deletion: %v", err)
	}
	for _, name := range []string{blob, blobView, otherName} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("retained entry %q changed: %v", name, err)
		}
	}
	for _, name := range []string{snapshot, friendly} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Fatalf("selected entry %q was not removed: %v", name, err)
		}
	}
}

func TestDeleteSelectedGGUFRefusesRegularSnapshotDependencyThroughFriendlyDirectoryAlias(t *testing.T) {
	repo, err := NewHFCache(selectedDeleteCacheRoot(t)).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(repo.SnapshotsDir(), "version-a", "model-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(snapshot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, []byte("selected regular snapshot"), 0644); err != nil {
		t.Fatal(err)
	}
	friendly := filepath.Join(repo.FriendlyPath(), "model-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(friendly), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(snapshot, friendly); err != nil {
		t.Fatal(err)
	}
	snapshotView := filepath.Join(repo.FriendlyPath(), "snapshot-view")
	if err := os.Symlink(filepath.Dir(snapshot), snapshotView); err != nil {
		t.Fatal(err)
	}
	otherName := filepath.Join(repo.FriendlyPath(), "model-Q5_K_M.gguf")
	if err := os.Symlink(filepath.Join(snapshotView, filepath.Base(snapshot)), otherName); err != nil {
		t.Fatal(err)
	}
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
	if len(result.Errors) == 0 || len(result.Removed) != 0 {
		t.Fatalf("regular snapshot alias dependency was not refused before unlink: %+v", result)
	}
	for _, name := range []string{snapshot, friendly, snapshotView, otherName} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("refused operation changed %q: %v", name, err)
		}
	}
}

func TestDeleteSelectedGGUFRejectsNestedBlobTargetBeforeAnyUnlink(t *testing.T) {
	repo, _, snapshot, friendly := selectedDeleteFixture(t)
	nested := filepath.Join(repo.BlobsDir(), "subdir", "blob-a")
	if err := os.MkdirAll(filepath.Dir(nested), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("nested payload"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "blobs", "subdir", "blob-a"), snapshot); err != nil {
		t.Fatal(err)
	}
	decoy := repo.BlobPath("blob-a")
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
	if len(result.Errors) == 0 || len(result.Removed) != 0 {
		t.Fatalf("nested blob target was not refused before unlink: %+v", result)
	}
	for _, name := range []string{snapshot, friendly, nested, decoy} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("refused operation changed %q: %v", name, err)
		}
	}
}

func TestDeleteSelectedGGUFRefusesIncompleteBlobTargetsBeforeAnyUnlink(t *testing.T) {
	for _, suffix := range []string{".incomplete", ".incomplete.meta"} {
		t.Run(suffix, func(t *testing.T) {
			repo, _, snapshot, friendly := selectedDeleteFixture(t)
			staging := repo.BlobPath("staging" + suffix)
			if err := os.WriteFile(staging, []byte("resume state"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(snapshot); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("..", "..", "blobs", filepath.Base(staging)), snapshot); err != nil {
				t.Fatal(err)
			}
			result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
			if result.Attempted || len(result.Errors) == 0 || len(result.Removed) != 0 {
				t.Fatalf("staging target was not refused during preflight: %+v", result)
			}
			for _, name := range []string{snapshot, friendly, staging} {
				if _, err := os.Lstat(name); err != nil {
					t.Fatalf("preflight refusal changed %q: %v", name, err)
				}
			}
		})
	}
}

func TestDeleteSelectedGGUFRefusesProducerShapedBlobTargetsBeforeAnyUnlink(t *testing.T) {
	for _, blobName := range []string{
		"tmp-" + strings.Repeat("a", 64),
		"tmp-model_config.json",
		"tmp-" + strings.Repeat("b", 64) + ".part",
		"tmp-" + strings.Repeat("b", 64) + ".part-00",
		"tmp-" + strings.Repeat("b", 64) + ".part-12",
		"tmp-" + strings.Repeat("b", 64) + ".parts.json",
		"tmp-" + strings.Repeat("b", 64) + ".parts.json.tmp",
		"tmp-" + strings.Repeat("b", 64) + ".tmp-random-string",
	} {
		t.Run(blobName, func(t *testing.T) {
			repo, _, snapshot, friendly := selectedDeleteFixture(t)
			staging := repo.BlobPath(blobName)
			if err := os.WriteFile(staging, []byte("producer state"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(snapshot); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("..", "..", "blobs", blobName), snapshot); err != nil {
				t.Fatal(err)
			}
			result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
			if result.Attempted || len(result.Errors) == 0 || len(result.Removed) != 0 {
				t.Fatalf("producer target was not refused before effects: %+v", result)
			}
			for _, name := range []string{snapshot, friendly, staging, repo.BlobPath("blob-a")} {
				if _, err := os.Lstat(name); err != nil {
					t.Fatalf("preflight refusal changed %q: %v", name, err)
				}
			}
		})
	}
}

func TestDeleteSelectedGGUFRefusesCreateTempPublicationTarget(t *testing.T) {
	for _, key := range []string{strings.Repeat("c", 64), "opaque-key"} {
		t.Run(key, func(t *testing.T) {
			repo, _, snapshot, friendly := selectedDeleteFixture(t)
			staged, err := os.CreateTemp(repo.BlobsDir(), key+".tmp-*")
			if err != nil {
				t.Fatal(err)
			}
			staging := staged.Name()
			if _, err := staged.Write([]byte("copy-in-progress")); err != nil {
				t.Fatal(err)
			}
			if err := staged.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(snapshot); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("..", "..", "blobs", filepath.Base(staging)), snapshot); err != nil {
				t.Fatal(err)
			}
			result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
			if result.Attempted || len(result.Errors) == 0 || len(result.Removed) != 0 {
				t.Fatalf("CreateTemp publication target was not refused before effects: %+v", result)
			}
			for _, name := range []string{snapshot, friendly, staging} {
				if _, err := os.Lstat(name); err != nil {
					t.Fatalf("preflight refusal changed %q: %v", name, err)
				}
			}
		})
	}
}

// TestStoreDownloadedFileRefusesReservedCollisionName pins the store
// boundary: a blob key whose NAME belongs to the downloader's staging
// namespace is refused.
//
// The shared classifier excludes a staging-shaped name from EVERY content
// surface - the cache statistics, the listing, the selection, the mirror and
// the deletion path - so a blob published under such a name would be stored
// and invisible. The boundary that accepts the name therefore refuses it.
func TestStoreDownloadedFileRefusesReservedCollisionName(t *testing.T) {
	repo, err := NewHFCache(selectedDeleteCacheRoot(t)).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	key := "tmp-" + strings.Repeat("f", 64)
	temp := filepath.Join(t.TempDir(), "download")
	if err := os.WriteFile(temp, []byte("published collision"), 0644); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.StoreDownloadedFile(temp, "model-Q4_K_M.gguf", "version-a", key, "", false)
	if err == nil {
		t.Fatalf("the store accepted a staging-shaped blob name: %+v", stored)
	}
	if !errors.Is(err, errReservedBlobName) {
		t.Fatalf("store error = %v, want the reserved-name refusal", err)
	}
	if !strings.Contains(err.Error(), "staging") {
		t.Errorf("store error %q does not explain that the name belongs to the staging namespace", err)
	}
	// Nothing was published: the refusal happens before the payload is placed.
	if _, serr := os.Stat(repo.BlobPath(key)); !os.IsNotExist(serr) {
		t.Errorf("the refused store published the blob, stat err=%v", serr)
	}
	if _, serr := os.Stat(filepath.Join(repo.SnapshotsDir(), "version-a", "model-Q4_K_M.gguf")); !os.IsNotExist(serr) {
		t.Errorf("the refused store created the snapshot entry, stat err=%v", serr)
	}
	// The payload is untouched: the caller still owns it.
	if _, serr := os.Stat(temp); serr != nil {
		t.Errorf("the refused store consumed the payload: %v", serr)
	}
}

// TestDeleteSelectedGGUFRefusesStagingShapedBlobTarget keeps the deletion
// path's coverage of the reserved namespace: a snapshot entry that links to a
// staging-shaped blob is refused before anything is unlinked.
//
// The state is built directly on disk because the public store no longer
// creates it (see TestStoreDownloadedFileRefusesReservedCollisionName); the
// deletion path must still refuse such a target if it finds one.
func TestDeleteSelectedGGUFRefusesStagingShapedBlobTarget(t *testing.T) {
	repo, err := NewHFCache(selectedDeleteCacheRoot(t)).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	key := "tmp-" + strings.Repeat("f", 64)
	blob := repo.BlobPath(key)
	if err := os.WriteFile(blob, []byte("published collision"), 0644); err != nil {
		t.Fatal(err)
	}
	snapshotDir := filepath.Join(repo.SnapshotsDir(), "version-a")
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(snapshotDir, "model-Q4_K_M.gguf")
	if err := os.Symlink(filepath.Join("..", "..", "blobs", key), snapshot); err != nil {
		t.Fatal(err)
	}
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
	if result.Attempted || len(result.Errors) == 0 || len(result.Removed) != 0 {
		t.Fatalf("reserved collision was not refused at preflight: %+v", result)
	}
	for _, name := range []string{snapshot, blob} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("collision refusal changed %s: %v", name, err)
		}
	}
}

// TestFinalizeBlobRefusesReservedCollisionName pins the same boundary on the
// exported writer of blobs/<key>: a staging-shaped blob key is refused before
// anything is created or moved.
func TestFinalizeBlobRefusesReservedCollisionName(t *testing.T) {
	repo, err := NewHFCache(selectedDeleteCacheRoot(t)).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	key := "tmp-" + strings.Repeat("f", 64)
	// The payload the caller would have published.
	if err := os.WriteFile(repo.IncompletePath(key), []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}
	err = repo.FinalizeBlob(key)
	if err == nil {
		t.Fatal("FinalizeBlob accepted a staging-shaped blob name")
	}
	if !errors.Is(err, errReservedBlobName) {
		t.Fatalf("FinalizeBlob error = %v, want the reserved-name refusal", err)
	}
	// Nothing was published.
	if _, serr := os.Stat(repo.BlobPath(key)); !os.IsNotExist(serr) {
		t.Errorf("the refused finalize published the blob, stat err=%v", serr)
	}
}

// TestStoreDownloadedFileAcceptsUnusualNonReservedName pins the other side
// of the boundary: a non-canonical but NON-reserved name is still accepted.
// The public storage API's tolerance for unusual published names is
// unchanged; only the downloader's own staging namespace is refused.
func TestStoreDownloadedFileAcceptsUnusualNonReservedName(t *testing.T) {
	repo, err := NewHFCache(selectedDeleteCacheRoot(t)).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	key := "unusual-published-name.bin"
	if IsStagingArtifactName(key) {
		t.Fatalf("the fixture name %q is a staging artifact", key)
	}
	temp := filepath.Join(t.TempDir(), "download")
	if err := os.WriteFile(temp, []byte("unusual content"), 0644); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.StoreDownloadedFile(temp, "model-Q4_K_M.gguf", "version-a", key, "", false)
	if err != nil {
		t.Fatalf("an unusual but non-reserved blob name was refused: %v", err)
	}
	if stored.SHA256 != key {
		t.Errorf("stored SHA256 = %q, want %q", stored.SHA256, key)
	}
	got, err := os.ReadFile(stored.BlobPath)
	if err != nil || string(got) != "unusual content" {
		t.Fatalf("published blob = %q (err=%v)", got, err)
	}
	if _, err := os.Lstat(stored.SnapshotPath); err != nil {
		t.Errorf("the snapshot entry was not created: %v", err)
	}
	if _, err := os.Lstat(stored.FriendlyPath); err != nil {
		t.Errorf("the friendly entry was not created: %v", err)
	}
}

func TestIncompleteBlobPathClassifierFollowsPlatformCaseSemantics(t *testing.T) {
	repo, _, _, _ := selectedDeleteFixture(t)
	path := repo.IncompletePath("staging")
	upperSuffix := strings.TrimSuffix(path, ".incomplete") + ".INCOMPLETE"
	wantAlias := runtime.GOOS == "windows"
	if got := repo.isIncompleteBlobPath(upperSuffix); got != wantAlias {
		t.Fatalf("uppercase suffix classified=%v, want %v on %s", got, wantAlias, runtime.GOOS)
	}
	if !repo.isIncompleteBlobPath(path) {
		t.Fatal("canonical incomplete data path was not classified")
	}
	meta := repo.IncompleteMetaPath("staging")
	upperMeta := strings.TrimSuffix(meta, ".incomplete.meta") + ".INCOMPLETE.META"
	if got := repo.isIncompleteBlobPath(upperMeta); got != wantAlias {
		t.Fatalf("uppercase metadata suffix classified=%v, want %v on %s", got, wantAlias, runtime.GOOS)
	}
}

func TestStagingBlobPathClassifierMatchesProducerNamespace(t *testing.T) {
	repo, _, _, _ := selectedDeleteFixture(t)
	for _, name := range []string{
		"tmp-" + strings.Repeat("a", 64), // hash destination / verified intermediate
		"tmp-model_config.json",          // path-fallback destination
		"tmp-" + strings.Repeat("b", 64) + ".part",
		"tmp-" + strings.Repeat("b", 64) + ".part-00",
		"tmp-" + strings.Repeat("b", 64) + ".part-17",
		"tmp-" + strings.Repeat("b", 64) + ".parts.json",
		"tmp-" + strings.Repeat("b", 64) + ".parts.json.tmp",
		"tmp-" + strings.Repeat("b", 64) + ".tmp-random-string",
	} {
		got := repo.isIncompleteBlobPath(repo.BlobPath(name))
		if !got {
			t.Errorf("producer staging path %q was not classified", name)
		}
	}
	// The persistent ownership lock, any legacy transition-lock file, and atomic record temp
	// are staging artifacts, never deletable blobs.
	for _, name := range []string{
		strings.Repeat("c", 64) + ".incomplete.meta.tmp",
		strings.Repeat("c", 64) + ".incomplete.lock",
		strings.Repeat("c", 64) + ".incomplete.meta.lock",
	} {
		if got := repo.isIncompleteBlobPath(repo.BlobPath(name)); !got {
			t.Errorf("staging lock path %q was not classified", name)
		}
	}
	// The exhaustive producer-shape list also covers the shapes that are
	// produced OUTSIDE the blobs/ namespace and therefore never occur there:
	// the single-request partial of a flat-mode destination ("<name>.part"),
	// the legacy layout metadata ("<name>.parts.json") and the persistent
	// ownership lock ("<name>.lock"). A blob named after one of them is
	// refused exactly like the other producer shapes.
	for _, name := range []string{"model.part", "model.parts.json", "abc.lock", "opaque-key.lock"} {
		if got := repo.isIncompleteBlobPath(repo.BlobPath(name)); !got {
			t.Errorf("producer staging path %q was not classified", name)
		}
	}
	for _, name := range []string{"abc123.gguf", "opaque-key", "tmp-", "key.tmp-", ".tmp-tail", "key.tmp-"} {
		if got := repo.isIncompleteBlobPath(repo.BlobPath(name)); got {
			t.Errorf("ordinary/non-producer blob path %q was classified", name)
		}
	}
	for _, name := range []string{strings.Repeat("a", 64) + ".tmp-random", "opaque-key.tmp-random"} {
		if !repo.isIncompleteBlobPath(repo.BlobPath(name)) {
			t.Errorf("atomic publication staging path %q was not classified", name)
		}
	}
	upper := repo.BlobPath("TMP-" + strings.Repeat("a", 64) + ".PART-00")
	if got, want := repo.isIncompleteBlobPath(upper), runtime.GOOS == "windows"; got != want {
		t.Fatalf("staging identity case behavior=%v, want %v on %s", got, want, runtime.GOOS)
	}
}

func TestAtomicCopyStagingMatcherUsesConsistentWindowsNormalization(t *testing.T) {
	// Exercise the Windows normalization branch deterministically on every host;
	// Go's case conversion can change UTF-8 byte length in either direction.
	expandingKey := strings.Repeat("Ⱥ", 16)
	staged, err := os.CreateTemp(t.TempDir(), expandingKey+".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	createdName := filepath.Base(staged.Name())
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	if !isAtomicCopyStagingName(createdName, true) {
		t.Fatalf("actual CreateTemp producer name %q not recognized under Windows normalization", createdName)
	}
	for _, tc := range []struct {
		name string
		win  bool
		want bool
	}{
		{strings.Repeat("Ⱥ", 16) + ".tmp-random-tail", true, true},
		{"K.tmp-", true, false}, // Unicode fold shrinks the base; tail is still empty.
		{"opaque.tmp-random-tail", true, true},
		{"opaque.TMP-random-tail", false, false},
		{"opaque.tmp-", false, false},
		{".tmp-tail", true, false},
	} {
		if got := isAtomicCopyStagingName(tc.name, tc.win); got != tc.want {
			t.Errorf("isAtomicCopyStagingName(%q, windows=%v)=%v, want %v", tc.name, tc.win, got, tc.want)
		}
	}
}

func TestDeleteSelectedGGUFRefusesMixedIncompleteCompositionBeforeAnyUnlink(t *testing.T) {
	repo, _, snapshot, friendly := selectedDeleteFixture(t)
	staging := repo.BlobPath("staging.incomplete")
	if err := os.WriteFile(staging, []byte("partial bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	meta := repo.IncompleteMetaPath("staging")
	if err := os.WriteFile(meta, []byte("resume metadata"), 0644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(repo.SnapshotsDir(), "version-a", "model-Q5_K_M.gguf")
	if err := os.Symlink(filepath.Join("..", "..", "blobs", filepath.Base(staging)), other); err != nil {
		t.Fatal(err)
	}
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}, {Path: "model-Q5_K_M.gguf", Versions: []string{"version-a"}}})
	if result.Attempted || len(result.Errors) == 0 || len(result.Removed) != 0 {
		t.Fatalf("mixed staging composition was not refused during preflight: %+v", result)
	}
	for _, name := range []string{snapshot, friendly, other, repo.BlobPath("blob-a"), staging, meta} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("preflight refusal changed %q: %v", name, err)
		}
	}
}

// selectedDeleteFallbackFixture builds the fallback-shaped cache the
// hardlink/copy link placement creates: a blob named by its content digest and
// snapshot/friendly entries that are plain files (hard links when link is
// true, independent copies otherwise) instead of symlinks.
func selectedDeleteFallbackFixture(t *testing.T, hardlink bool) (*RepoDir, string, string, string, []byte) {
	t.Helper()
	repo, err := NewHFCache(selectedDeleteCacheRoot(t)).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	content := []byte("fallback placement payload")
	blob := repo.BlobPath(fmt.Sprintf("%x", sha256.Sum256(content)))
	if err := os.WriteFile(blob, content, 0644); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(repo.SnapshotsDir(), "version-a", "model-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(snapshot), 0755); err != nil {
		t.Fatal(err)
	}
	if hardlink {
		if err := os.Link(blob, snapshot); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(snapshot, content, 0644); err != nil {
		t.Fatal(err)
	}
	friendly := filepath.Join(repo.FriendlyPath(), "model-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(friendly), 0755); err != nil {
		t.Fatal(err)
	}
	if hardlink {
		if err := os.Link(snapshot, friendly); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(friendly, content, 0644); err != nil {
		t.Fatal(err)
	}
	return repo, blob, snapshot, friendly, content
}

// TestDeleteSelectedGGUFRemovesHardLinkEntriesAndSharedPayload covers
// deletion when snapshot and friendly entries are hard links (the fallback
// placement on filesystems without symlinks): the selected entries and their
// provably shared payload must all be removed.
func TestDeleteSelectedGGUFRemovesHardLinkEntriesAndSharedPayload(t *testing.T) {
	repo, blob, snapshot, friendly, _ := selectedDeleteFallbackFixture(t, true)
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
	if len(result.Errors) != 0 {
		t.Fatalf("delete errors: %v", result.Errors)
	}
	for _, name := range []string{snapshot, friendly, blob} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Fatalf("entry %q was not removed: %v", name, err)
		}
	}
}

// TestDeleteSelectedGGUFRetainsHardLinkFriendlyAliasPayload is the hardlink
// twin of TestDeleteSelectedGGUFRetainsDirectFriendlyBlobDependency: a
// retained regular friendly name sharing the payload's file keeps the payload
// and is reported, exactly like a retained friendly symlink to the blob.
func TestDeleteSelectedGGUFRetainsHardLinkFriendlyAliasPayload(t *testing.T) {
	repo, blob, snapshot, friendly, content := selectedDeleteFallbackFixture(t, true)
	otherName := filepath.Join(repo.FriendlyPath(), "model-Q5_K_M.gguf")
	if err := os.Link(blob, otherName); err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(repo.FriendlyPath(), "README.md")
	if err := os.WriteFile(readme, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
	if len(result.Errors) != 0 {
		t.Fatalf("delete errors: %v", result.Errors)
	}
	for _, name := range []string{snapshot, friendly} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Fatalf("selected entry %q was not removed: %v", name, err)
		}
	}
	for _, name := range []string{blob, otherName, readme} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("retained entry %q changed: %v", name, err)
		}
	}
	got, err := os.ReadFile(otherName)
	if err != nil || string(got) != string(content) {
		t.Fatalf("retained alias content=%q %v", got, err)
	}
	if len(result.RetainedBlobs) != 1 || result.RetainedBlobs[0] != blob {
		t.Fatalf("retained payload reporting=%v, want %q", result.RetainedBlobs, blob)
	}
}

// TestDeleteSelectedGGUFRemovesCopyEntriesAndContentAddressedPayload covers
// deletion when snapshot and friendly entries are independent copies (the
// fallback on filesystems without any links): the payload association is
// proven by content digest against the blob's canonical name, so the selected
// entries and their payload are all removed.
func TestDeleteSelectedGGUFRemovesCopyEntriesAndContentAddressedPayload(t *testing.T) {
	repo, blob, snapshot, friendly, _ := selectedDeleteFallbackFixture(t, false)
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
	if len(result.Errors) != 0 {
		t.Fatalf("delete errors: %v", result.Errors)
	}
	for _, name := range []string{snapshot, friendly, blob} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Fatalf("entry %q was not removed: %v", name, err)
		}
	}
}

// TestDeleteSelectedGGUFRefusesAliasDependingOnRegularProjection is the
// fallback twin of TestDeleteSelectedGGUFRefusesUnselectedFriendlySnapshotDependency:
// a retained friendly symlink that depends on a removable copy projection
// would dangle after deletion, so the whole operation is refused before any
// unlink.
func TestDeleteSelectedGGUFRefusesAliasDependingOnRegularProjection(t *testing.T) {
	repo, _, snapshot, friendly, _ := selectedDeleteFallbackFixture(t, false)
	alias := filepath.Join(repo.FriendlyPath(), "model-Q5_K_M.gguf")
	if err := os.Symlink(friendly, alias); err != nil {
		t.Fatal(err)
	}
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
	if len(result.Errors) == 0 || len(result.Removed) != 0 {
		t.Fatalf("dependent friendly reference was not refused before unlink: %+v", result)
	}
	for _, name := range []string{snapshot, friendly, alias} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("refused operation changed %q: %v", name, err)
		}
	}
}

// TestDeleteSelectedGGUFRetainsPayloadReferencedByHardLinkSnapshotEntry
// covers a mixed-shape cache: a selected snapshot symlink and another saved
// version's hard link to the same payload. Removing the payload would leave
// the unselected version's entry unreferenced in the blob store, so the
// payload must be retained and reported.
func TestDeleteSelectedGGUFRetainsPayloadReferencedByHardLinkSnapshotEntry(t *testing.T) {
	repo, blob, snapshot, _, _ := selectedDeleteFallbackFixture(t, true)
	other := filepath.Join(repo.SnapshotsDir(), "version-b", "model-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(other), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(blob, other); err != nil {
		t.Fatal(err)
	}
	// Replace the selected version-a entry with a symlink, the shape the
	// classic HF layout produces.
	if err := os.Remove(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "blobs", filepath.Base(blob)), snapshot); err != nil {
		t.Fatal(err)
	}
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
	if len(result.Errors) != 0 {
		t.Fatalf("delete errors: %v", result.Errors)
	}
	if _, err := os.Lstat(snapshot); !os.IsNotExist(err) {
		t.Fatalf("selected snapshot entry was not removed: %v", err)
	}
	for _, name := range []string{blob, other} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("hardlink-referenced entry %q changed: %v", name, err)
		}
	}
	got, err := os.ReadFile(other)
	if err != nil || len(got) == 0 {
		t.Fatalf("unselected version lost its content: %q %v", got, err)
	}
	if len(result.RetainedBlobs) != 1 || result.RetainedBlobs[0] != blob {
		t.Fatalf("retained payload reporting=%v, want %q", result.RetainedBlobs, blob)
	}
}

// writeFallbackManifest records the fallback fixture's file in a download
// manifest, the repair mapping a later rebuild would use.
func writeFallbackManifest(t *testing.T, repo *RepoDir, name string, content []byte) {
	t.Helper()
	builder := NewManifestBuilder(Job{Repo: "owner/model"}, "hfdesk test")
	builder.SetCommit("version-a")
	builder.AddFile(name, fmt.Sprintf("%x", sha256.Sum256(content)), int64(len(content)), true)
	if _, err := builder.Build().Write(repo.FriendlyPath()); err != nil {
		t.Fatal(err)
	}
}

// TestDeleteSelectedGGUFDeletionDurableAcrossSync pins the deletion-durability
// contract: a rebuild after a selected-GGUF deletion must not resurrect the
// deleted names from the download manifest's repair mapping — including when
// the payload is retained (shared entry, retained alias, or failed blob
// unlink) — while retained payloads and refused deletions stay intact.
func TestDeleteSelectedGGUFDeletionDurableAcrossSync(t *testing.T) {
	t.Run("manifest parse failure is reported while deletion proceeds", func(t *testing.T) {
		repo, blob, snapshot, friendly, _ := selectedDeleteFallbackFixture(t, true)
		retainedAlias := filepath.Join(repo.FriendlyPath(), "model-Q5_K_M.gguf")
		if err := os.Link(blob, retainedAlias); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo.FriendlyPath(), ManifestFilename), []byte("invalid: [yaml"), 0644); err != nil {
			t.Fatal(err)
		}

		result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
		if !result.Attempted || len(result.Errors) == 0 || !strings.Contains(strings.Join(result.Errors, "\n"), "read download manifest") {
			t.Fatalf("manifest parse failure was not reported: %+v", result)
		}
		for _, name := range []string{snapshot, friendly} {
			if _, err := os.Lstat(name); !os.IsNotExist(err) {
				t.Fatalf("deletion did not proceed after manifest read failure for %q: %v", name, err)
			}
		}
		if _, err := os.Stat(retainedAlias); err != nil {
			t.Fatalf("retained payload alias was lost: %v", err)
		}
	})
	t.Run("manifest read failure is reported while deletion proceeds", func(t *testing.T) {
		repo, _, snapshot, friendly, _ := selectedDeleteFallbackFixture(t, true)
		manifestPath := filepath.Join(repo.FriendlyPath(), ManifestFilename)
		if err := os.Mkdir(manifestPath, 0755); err != nil {
			t.Fatal(err)
		}

		result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
		if !result.Attempted || len(result.Errors) == 0 || !strings.Contains(strings.Join(result.Errors, "\n"), "read download manifest") {
			t.Fatalf("manifest read failure was not reported: %+v", result)
		}
		for _, name := range []string{snapshot, friendly} {
			if _, err := os.Lstat(name); !os.IsNotExist(err) {
				t.Fatalf("deletion did not proceed after manifest read failure for %q: %v", name, err)
			}
		}
	})

	for _, tc := range []struct {
		name    string
		foreign bool
	}{
		{name: "manifest absent"},
		{name: "foreign manifest ignored", foreign: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _, snapshot, _, _ := selectedDeleteFallbackFixture(t, true)
			if tc.foreign {
				m := &DownloadManifest{Repo: "other/repo", Type: "model"}
				if _, err := m.Write(repo.FriendlyPath()); err != nil {
					t.Fatal(err)
				}
			}
			result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
			if !result.Attempted || len(result.Errors) != 0 {
				t.Fatalf("absence/foreign identity reported as a manifest failure: %v", result.Errors)
			}
			if _, err := os.Lstat(snapshot); !os.IsNotExist(err) {
				t.Fatalf("deletion did not proceed for absent/foreign manifest: %v", err)
			}
		})
	}

	t.Run("retained payload is not resurrected", func(t *testing.T) {
		repo, blob, snapshot, friendly, content := selectedDeleteFallbackFixture(t, true)
		otherName := filepath.Join(repo.FriendlyPath(), "model-Q5_K_M.gguf")
		if err := os.Link(blob, otherName); err != nil {
			t.Fatal(err)
		}
		writeFallbackManifest(t, repo, "model-Q4_K_M.gguf", content)

		result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
		if len(result.Errors) != 0 {
			t.Fatalf("delete errors: %v", result.Errors)
		}
		if len(result.RetainedBlobs) != 1 || result.RetainedBlobs[0] != blob {
			t.Fatalf("retained payload reporting=%v, want %q", result.RetainedBlobs, blob)
		}

		if _, err := repo.cache.Sync(SyncOptions{}); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{snapshot, friendly} {
			if _, err := os.Lstat(name); !os.IsNotExist(err) {
				t.Fatalf("deleted entry %q was resurrected by sync: %v", name, err)
			}
		}
		for _, name := range []string{blob, otherName} {
			if _, err := os.Lstat(name); err != nil {
				t.Fatalf("retained entry %q changed after sync: %v", name, err)
			}
		}
		got, err := os.ReadFile(otherName)
		if err != nil || string(got) != string(content) {
			t.Fatalf("retained alias content=%q %v", got, err)
		}
	})

	t.Run("plain deletion is not resurrected", func(t *testing.T) {
		repo, _, snapshot, friendly, content := selectedDeleteFallbackFixture(t, true)
		writeFallbackManifest(t, repo, "model-Q4_K_M.gguf", content)

		result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
		if len(result.Errors) != 0 {
			t.Fatalf("delete errors: %v", result.Errors)
		}
		m, err := ReadManifest(filepath.Join(repo.FriendlyPath(), ManifestFilename))
		if err != nil {
			t.Fatal(err)
		}
		if !m.IsDeleted("model-Q4_K_M.gguf", "version-a") {
			t.Fatal("deletion was not recorded in the manifest")
		}

		if _, err := repo.cache.Sync(SyncOptions{}); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{snapshot, friendly} {
			if _, err := os.Lstat(name); !os.IsNotExist(err) {
				t.Fatalf("deleted entry %q was resurrected by sync: %v", name, err)
			}
		}
	})

	t.Run("refused deletion leaves the manifest untouched", func(t *testing.T) {
		repo, _, snapshot, friendly, content := selectedDeleteFallbackFixture(t, false)
		alias := filepath.Join(repo.FriendlyPath(), "model-Q5_K_M.gguf")
		if err := os.Symlink(friendly, alias); err != nil {
			t.Fatal(err)
		}
		writeFallbackManifest(t, repo, "model-Q4_K_M.gguf", content)

		result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
		if len(result.Errors) == 0 || len(result.Removed) != 0 {
			t.Fatalf("dependent friendly reference was not refused before unlink: %+v", result)
		}
		m, err := ReadManifest(filepath.Join(repo.FriendlyPath(), ManifestFilename))
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Deleted) != 0 {
			t.Fatalf("refused deletion recorded manifest tombstones: %+v", m.Deleted)
		}
		for _, name := range []string{snapshot, friendly, alias} {
			if _, err := os.Lstat(name); err != nil {
				t.Fatalf("refused operation changed %q: %v", name, err)
			}
		}
	})
}
