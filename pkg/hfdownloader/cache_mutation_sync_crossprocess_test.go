// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startCacheScopeHolder re-executes the existing child-process helper to hold
// one cache admission scope — an entry's scope or a tree's — until the
// returned release runs, so a mutating operation in this process is driven
// against a live holder in ANOTHER process.
func startCacheScopeHolder(t *testing.T, cacheRoot, entryPath, treePath string) func() {
	t.Helper()
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no external ownership lock primitive")
	}
	markerDir := t.TempDir()
	readyPath := filepath.Join(markerDir, "ready")
	goPath := filepath.Join(markerDir, "go")
	child := startBlobOwnershipChild(t, blobOwnershipChildConfig{
		HoldCacheRoot: cacheRoot,
		HoldEntryPath: entryPath,
		HoldTreePath:  treePath,
		ReadyPath:     readyPath,
		GoPath:        goPath,
	})
	waitForFile(t, readyPath, 60*time.Second)
	return func() { releaseBlobOwnershipChild(t, child, goPath) }
}

func requireRefusalError(t *testing.T, errs []error, what string) {
	t.Helper()
	if len(errs) == 0 {
		t.Fatalf("%s mutated without reporting the held scope", what)
	}
	if got := errs[0].Error(); !strings.Contains(got, "ownership scope") {
		t.Fatalf("%s did not report the actionable ownership refusal: %v", what, errs)
	}
}

// The rebuild path's own entry mutations and the rebuild-script write must
// not mutate an entry whose scope a live holder in ANOTHER process owns: each
// waits its bounded per-entry budget and then fails with the actionable
// ownership error, the entry stays untouched, and every operation proceeds
// normally once the holder releases. This process's own state sees no job at
// all, so only the cross-process admission can refuse.
func TestRebuildMutationsRefuseLiveForeignHolder(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no external ownership lock primitive")
	}
	shortBlobOwnershipWait(t, 300*time.Millisecond)

	t.Run("friendly entry replacement", func(t *testing.T) {
		cacheDir := t.TempDir()
		cache := NewHFCache(cacheDir)
		repo, err := cache.Repo("owner/model", RepoTypeModel)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.EnsureDirs(); err != nil {
			t.Fatal(err)
		}
		content := []byte("friendly replacement payload")
		blobName := strings.Repeat("1", 64)
		if err := os.WriteFile(repo.BlobPath(blobName), content, 0o644); err != nil {
			t.Fatal(err)
		}
		snapPath, err := repo.SnapshotPath("commit123", "model.bin")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(snapPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", "..", "blobs", blobName), snapPath); err != nil {
			t.Fatal(err)
		}
		if err := repo.WriteRef("main", "commit123"); err != nil {
			t.Fatal(err)
		}
		friendlyPath := filepath.Join(repo.FriendlyPath(), "model.bin")
		if err := os.MkdirAll(filepath.Dir(friendlyPath), 0o755); err != nil {
			t.Fatal(err)
		}
		wrongTarget := filepath.Join("..", "..", "..", "hub", "models--owner--model", "blobs", "missing")
		if err := os.Symlink(wrongTarget, friendlyPath); err != nil {
			t.Fatal(err)
		}

		release := startCacheScopeHolder(t, cacheDir, friendlyPath, "")
		result, err := cache.Sync(SyncOptions{})
		if err != nil {
			t.Fatal(err)
		}
		requireRefusalError(t, result.Errors, "friendly replacement")
		if got, statErr := os.Readlink(friendlyPath); statErr != nil || got != wrongTarget {
			t.Fatalf("friendly entry mutated while its scope was held: %q %v", got, statErr)
		}

		release()
		result, err = cache.Sync(SyncOptions{})
		if err != nil || len(result.Errors) != 0 {
			t.Fatalf("sync after release: %v %v", err, result.Errors)
		}
		requireContent(t, friendlyPath, content)
	})

	t.Run("snapshot repair", func(t *testing.T) {
		content := []byte("blobs only payload")
		cache, repo, _ := blobsOnlyCacheFixture(t, content)
		snapPath, err := repo.SnapshotPath("commit-a", "model-Q4_K_M.gguf")
		if err != nil {
			t.Fatal(err)
		}

		release := startCacheScopeHolder(t, cache.HubDir(), snapPath, "")
		result, err := cache.Sync(SyncOptions{})
		if err != nil {
			t.Fatal(err)
		}
		requireRefusalError(t, result.Errors, "snapshot repair")
		if _, statErr := os.Lstat(snapPath); !os.IsNotExist(statErr) {
			t.Fatalf("snapshot entry materialized while its scope was held: %v", statErr)
		}

		release()
		result, err = cache.Sync(SyncOptions{})
		if err != nil || len(result.Errors) != 0 {
			t.Fatalf("sync after release: %v %v", err, result.Errors)
		}
		requireContent(t, snapPath, content)
	})

	t.Run("orphan removal", func(t *testing.T) {
		cacheDir := t.TempDir()
		cache := NewHFCache(cacheDir)
		repo, err := cache.Repo("owner/model", RepoTypeModel)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.EnsureDirs(); err != nil {
			t.Fatal(err)
		}
		orphanPath := filepath.Join(repo.FriendlyPath(), "orphan.bin")
		if err := os.MkdirAll(filepath.Dir(orphanPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", "gone-target"), orphanPath); err != nil {
			t.Fatal(err)
		}

		release := startCacheScopeHolder(t, cacheDir, orphanPath, "")
		result, err := cache.Sync(SyncOptions{Clean: true})
		if err != nil {
			t.Fatal(err)
		}
		if result.OrphansRemoved != 0 {
			t.Fatalf("orphan removal counted while its scope was held: %d", result.OrphansRemoved)
		}
		if _, statErr := os.Lstat(orphanPath); statErr != nil {
			t.Fatalf("orphan was removed while its scope was held: %v", statErr)
		}

		release()
		result, err = cache.Sync(SyncOptions{Clean: true})
		if err != nil || result.OrphansRemoved != 1 {
			t.Fatalf("orphan clean after release: %v removed=%d", err, result.OrphansRemoved)
		}
		if _, statErr := os.Lstat(orphanPath); !os.IsNotExist(statErr) {
			t.Fatalf("orphan survived its admitted removal: %v", statErr)
		}
	})

	t.Run("empty directory pruning", func(t *testing.T) {
		cacheDir := t.TempDir()
		cache := NewHFCache(cacheDir)
		repo, err := cache.Repo("owner/model", RepoTypeModel)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.EnsureDirs(); err != nil {
			t.Fatal(err)
		}
		emptyDir := filepath.Join(repo.FriendlyPath(), "empty-subdir")
		if err := os.MkdirAll(emptyDir, 0o755); err != nil {
			t.Fatal(err)
		}

		release := startCacheScopeHolder(t, cacheDir, "", emptyDir)
		if _, err := cache.Sync(SyncOptions{Clean: true}); err != nil {
			t.Fatal(err)
		}
		if info, statErr := os.Lstat(emptyDir); statErr != nil || !info.IsDir() {
			t.Fatalf("empty directory pruned while its tree was held: %v", statErr)
		}

		release()
		if _, err := cache.Sync(SyncOptions{Clean: true}); err != nil {
			t.Fatal(err)
		}
		if _, statErr := os.Lstat(emptyDir); !os.IsNotExist(statErr) {
			t.Fatalf("empty directory survived its admitted pruning: %v", statErr)
		}
	})

	t.Run("rebuild script write", func(t *testing.T) {
		cacheDir := t.TempDir()
		cache := NewHFCache(cacheDir)
		stale := []byte("# stale rebuild script\n")
		if err := os.WriteFile(cache.RebuildScriptPath(), stale, 0o755); err != nil {
			t.Fatal(err)
		}

		release := startCacheScopeHolder(t, cacheDir, cache.RebuildScriptPath(), "")
		if _, err := cache.WriteRebuildScript(); err == nil || !strings.Contains(err.Error(), "ownership scope") {
			t.Fatalf("script write proceeded or was not actionable while its scope was held: %v", err)
		}
		got, readErr := os.ReadFile(cache.RebuildScriptPath())
		if readErr != nil || string(got) != string(stale) {
			t.Fatalf("script mutated while its scope was held: %q %v", got, readErr)
		}

		release()
		path, err := cache.WriteRebuildScript()
		if err != nil {
			t.Fatalf("script write after release: %v", err)
		}
		script, readErr := os.ReadFile(path)
		if readErr != nil || !strings.Contains(string(script), RebuildScriptVersion) {
			t.Fatalf("script not written after release: %v", readErr)
		}
	})
}
