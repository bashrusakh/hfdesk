// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func selectedDeleteFixture(t *testing.T) (*RepoDir, string, string, string) {
	t.Helper()
	cache := NewHFCache(filepath.Join(t.TempDir(), "cache"), 0)
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

func TestDeleteSelectedGGUFRetainsDirectFriendlyBlobDependency(t *testing.T) {
	repo, blob, snapshot, friendly := selectedDeleteFixture(t)
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
	for _, name := range []string{blob, otherName, readme} {
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
	repo, err := NewHFCache(filepath.Join(t.TempDir(), "cache"), 0).Repo("owner/model", RepoTypeModel)
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
