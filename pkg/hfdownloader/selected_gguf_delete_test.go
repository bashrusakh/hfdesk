// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestStoreDownloadedFileAllowsReservedCollisionButDeleteRefusesIt(t *testing.T) {
	repo, err := NewHFCache(filepath.Join(t.TempDir(), "cache"), 0).Repo("owner/model", RepoTypeModel)
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
	if err != nil {
		t.Fatalf("reserved collision must remain creatable through the public storage API: %v", err)
	}
	result := repo.DeleteSelectedGGUF([]SelectedGGUFEntry{{Path: "model-Q4_K_M.gguf", Versions: []string{"version-a"}}})
	if result.Attempted || len(result.Errors) == 0 || len(result.Removed) != 0 {
		t.Fatalf("reserved collision was not refused at preflight: %+v", result)
	}
	for _, name := range []string{stored.SnapshotPath, stored.FriendlyPath, stored.BlobPath} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("collision refusal changed %s: %v", name, err)
		}
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
	for _, name := range []string{"model.part", "model.parts.json", "abc123.gguf", "opaque-key", "tmp-", "key.tmp-", ".tmp-tail", "key.tmp-"} {
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
