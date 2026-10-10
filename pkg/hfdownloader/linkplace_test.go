// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// withLinkFakes swaps the injectable link primitives for the duration of a
// test. This simulates the Windows/no-symlink and cross-volume situations
// without needing a real Windows box; tests using it say "Simulated" in their
// name. Tests using these fakes must not use t.Parallel.
func withLinkFakes(t *testing.T, symlink, hardlink func(oldname, newname string) error) {
	t.Helper()
	origSymlink, origHardlink := symlinkFn, hardlinkFn
	if symlink != nil {
		symlinkFn = symlink
	}
	if hardlink != nil {
		hardlinkFn = hardlink
	}
	t.Cleanup(func() {
		symlinkFn, hardlinkFn = origSymlink, origHardlink
	})
}

func captureLinkNotices(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := linkNoticeOut
	linkNoticeOut = &buf
	t.Cleanup(func() { linkNoticeOut = orig })
	return &buf
}

func denySymlink(oldname, newname string) error {
	return errors.New("simulated: symlinks unavailable")
}

func denyHardlink(oldname, newname string) error {
	return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
}

func crossVolumeHardlink(oldname, newname string) error {
	return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EXDEV}
}

func linkTestRepo(t *testing.T) *RepoDir {
	t.Helper()
	cache := NewHFCache(filepath.Join(t.TempDir(), "cache"), 0)
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return repo
}

func linkTestStore(t *testing.T, repo *RepoDir, content []byte, name string) (*StoreFileResult, string) {
	t.Helper()
	temp := filepath.Join(t.TempDir(), "download-"+name)
	if err := os.WriteFile(temp, content, 0644); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	stored, err := repo.StoreDownloadedFile(temp, name, "commit-a", hash, "", false)
	if err != nil {
		t.Fatalf("StoreDownloadedFile: %v", err)
	}
	return stored, hash
}

func requireRegularFileWithContent(t *testing.T, path string, content []byte) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("entry %s missing: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatalf("entry %s is not a regular file: %v", path, info.Mode())
	}
	requireContent(t, path, content)
	return info
}

// requireContent checks the bytes an entry resolves to, following a symlink
// leaf the way a reader would.
func requireContent(t *testing.T, path string, content []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("entry %s content=%q %v", path, got, err)
	}
}

// TestSnapshotEntrySimulatedWindowsSymlinkFailureUsesHardLink covers the
// simulated Windows shape from issue #99: symlink creation fails, hard links
// work, and the snapshot entry must still be created and resolve to the blob
// content instead of leaving snapshots/<commit> empty.
func TestSnapshotEntrySimulatedWindowsSymlinkFailureUsesHardLink(t *testing.T) {
	withLinkFakes(t, denySymlink, os.Link)
	repo := linkTestRepo(t)
	content := []byte("gguf payload")
	stored, _ := linkTestStore(t, repo, content, "model-Q4_K_M.gguf")

	snapInfo := requireRegularFileWithContent(t, stored.SnapshotPath, content)
	blobInfo, err := os.Lstat(stored.BlobPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(snapInfo, blobInfo) {
		t.Fatal("snapshot entry is not a hard link to the blob")
	}

	friendlyInfo := requireRegularFileWithContent(t, stored.FriendlyPath, content)
	if !os.SameFile(friendlyInfo, snapInfo) {
		t.Fatal("friendly entry is not a hard link to the snapshot entry")
	}
}

// TestSnapshotEntrySimulatedWindowsSymlinkAndHardLinkFailureCopies covers a
// filesystem without links of any kind: the entries must fall back to copies
// (regular files with the blob content) rather than failing or skipping.
func TestSnapshotEntrySimulatedWindowsSymlinkAndHardLinkFailureCopies(t *testing.T) {
	withLinkFakes(t, denySymlink, denyHardlink)
	repo := linkTestRepo(t)
	content := []byte("gguf payload to copy")
	stored, _ := linkTestStore(t, repo, content, "model-Q4_K_M.gguf")

	snapInfo := requireRegularFileWithContent(t, stored.SnapshotPath, content)
	blobInfo, err := os.Lstat(stored.BlobPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(snapInfo, blobInfo) {
		t.Fatal("snapshot entry unexpectedly shares the blob's file identity")
	}
	requireRegularFileWithContent(t, stored.FriendlyPath, content)
}

// TestCrossVolumeHardLinkFailureFallsBackToCopySimulated simulates the
// cross-volume hardlink error (EXDEV): placement must fall back to copy.
func TestCrossVolumeHardLinkFailureFallsBackToCopySimulated(t *testing.T) {
	withLinkFakes(t, denySymlink, crossVolumeHardlink)
	repo := linkTestRepo(t)
	content := []byte("cross volume payload")
	stored, _ := linkTestStore(t, repo, content, "model-Q5_K_M.gguf")

	requireRegularFileWithContent(t, stored.SnapshotPath, content)
	requireRegularFileWithContent(t, stored.FriendlyPath, content)
}

// TestLinkFallbackProbeMemoizedPerRootSimulated verifies a failing
// os.Symlink/os.Link is attempted once per placement root instead of once per
// file, and that one clear notice per root names the fallback in use.
func TestLinkFallbackProbeMemoizedPerRootSimulated(t *testing.T) {
	t.Run("hard link fallback", func(t *testing.T) {
		symlinkCalls, hardlinkCalls := 0, 0
		withLinkFakes(t,
			func(oldname, newname string) error { symlinkCalls++; return denySymlink(oldname, newname) },
			func(oldname, newname string) error { hardlinkCalls++; return os.Link(oldname, newname) },
		)
		notices := captureLinkNotices(t)
		repo := linkTestRepo(t)
		for _, name := range []string{"a.gguf", "b.gguf", "c.gguf"} {
			linkTestStore(t, repo, []byte("content-"+name), name)
		}
		// Two placement roots (hub/ for snapshots, models/ for the friendly
		// view): the failing symlink primitive is probed once per root.
		if symlinkCalls != 2 {
			t.Fatalf("symlink attempts=%d, want 2 (one per root)", symlinkCalls)
		}
		if hardlinkCalls != 6 {
			t.Fatalf("hardlink attempts=%d, want 6 (one per placed entry)", hardlinkCalls)
		}
		if got := strings.Count(notices.String(), "hard links instead"); got != 2 {
			t.Fatalf("fallback notices=%d (%q), want one per root", got, notices.String())
		}
	})

	t.Run("copy fallback", func(t *testing.T) {
		symlinkCalls, hardlinkCalls := 0, 0
		withLinkFakes(t,
			func(oldname, newname string) error { symlinkCalls++; return denySymlink(oldname, newname) },
			func(oldname, newname string) error { hardlinkCalls++; return denyHardlink(oldname, newname) },
		)
		notices := captureLinkNotices(t)
		repo := linkTestRepo(t)
		for _, name := range []string{"a.gguf", "b.gguf", "c.gguf"} {
			linkTestStore(t, repo, []byte("content-"+name), name)
		}
		if symlinkCalls != 2 {
			t.Fatalf("symlink attempts=%d, want 2 (one per root)", symlinkCalls)
		}
		if hardlinkCalls != 2 {
			t.Fatalf("hardlink attempts=%d, want 2 (one per root)", hardlinkCalls)
		}
		if got := strings.Count(notices.String(), "file copies instead"); got != 2 {
			t.Fatalf("fallback notices=%d (%q), want one per root", got, notices.String())
		}
	})
}

// TestDedupStoreStillCreatesSnapshotEntrySimulated preserves the dedup path:
// when the blob already exists the temp file is removed, but the snapshot and
// friendly entries must still be created/updated.
func TestDedupStoreStillCreatesSnapshotEntrySimulated(t *testing.T) {
	withLinkFakes(t, denySymlink, denyHardlink)
	repo := linkTestRepo(t)
	content := []byte("deduplicated payload")

	first, _ := linkTestStore(t, repo, content, "model-Q4_K_M.gguf")
	temp := filepath.Join(t.TempDir(), "download-again")
	if err := os.WriteFile(temp, content, 0644); err != nil {
		t.Fatal(err)
	}
	second, err := repo.StoreDownloadedFile(temp, "model-Q5_K_M.gguf", "commit-a", fmt.Sprintf("%x", sha256.Sum256(content)), "", false)
	if err != nil {
		t.Fatalf("dedup store: %v", err)
	}
	if _, err := os.Lstat(temp); !os.IsNotExist(err) {
		t.Fatalf("temp file not removed on dedup: %v", err)
	}
	if first.BlobPath != second.BlobPath {
		t.Fatalf("dedup produced a second blob: %q vs %q", first.BlobPath, second.BlobPath)
	}
	requireRegularFileWithContent(t, second.SnapshotPath, content)
	requireRegularFileWithContent(t, second.FriendlyPath, content)
}

// TestLocalFlatModeUnaffectedByLinkFallbackSimulated proves flat/local mode
// (OutputDir downloads, real files, no links) never touches link placement:
// even with both link primitives failing, the download lands as a regular
// file and the link primitives are never called.
func TestLocalFlatModeUnaffectedByLinkFallbackSimulated(t *testing.T) {
	symlinkCalls, hardlinkCalls := 0, 0
	withLinkFakes(t,
		func(oldname, newname string) error { symlinkCalls++; return denySymlink(oldname, newname) },
		func(oldname, newname string) error { hardlinkCalls++; return denyHardlink(oldname, newname) },
	)
	body := []byte("flat mode payload")
	srv := newMockHFFileServer(t, body)
	outRoot := filepath.Join(t.TempDir(), "out")
	cfg := Settings{OutputDir: outRoot, Endpoint: srv.URL, Concurrency: 1}
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, nil); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(outRoot, "owner", "model", "file.bin")
	info := requireRegularFileWithContent(t, dst, body)
	_ = info
	if symlinkCalls != 0 || hardlinkCalls != 0 {
		t.Fatalf("flat mode attempted link placement: symlink=%d hardlink=%d", symlinkCalls, hardlinkCalls)
	}
}

// blobsOnlyCacheFixture builds the exact broken cache shape from issue #99:
// blobs and a completed download manifest, but no snapshot entries at all.
func blobsOnlyCacheFixture(t *testing.T, content []byte) (*HFCache, *RepoDir, string) {
	t.Helper()
	cache := NewHFCache(filepath.Join(t.TempDir(), "cache"), 0)
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	if err := os.WriteFile(repo.BlobPath(hash), content, 0644); err != nil {
		t.Fatal(err)
	}
	builder := NewManifestBuilder(Job{Repo: "owner/model"}, "hfdesk test")
	builder.SetCommit("commit-a")
	builder.AddFile("model-Q4_K_M.gguf", hash, int64(len(content)), true)
	if _, err := builder.Build().Write(repo.FriendlyPath()); err != nil {
		t.Fatal(err)
	}
	return cache, repo, hash
}

// TestSyncRepairsBlobsOnlyCacheSnapshotEntries covers the repair criterion of
// issue #99: re-running HFCache.Sync over a cache that has blobs but no
// snapshot entries must recreate them (and the friendly view) from the
// offline download manifest.
func TestSyncRepairsBlobsOnlyCacheSnapshotEntries(t *testing.T) {
	content := []byte("blobs only payload")

	t.Run("links available", func(t *testing.T) {
		cache, repo, _ := blobsOnlyCacheFixture(t, content)
		result, err := cache.Sync(SyncOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Errors) != 0 {
			t.Fatalf("sync errors: %v", result.Errors)
		}
		snapPath, err := repo.SnapshotPath("commit-a", "model-Q4_K_M.gguf")
		if err != nil {
			t.Fatal(err)
		}
		requireContent(t, snapPath, content) // follows the symlink
		friendly := filepath.Join(repo.FriendlyPath(), "model-Q4_K_M.gguf")
		requireContent(t, friendly, content)
	})

	t.Run("SimulatedWindows no links available", func(t *testing.T) {
		withLinkFakes(t, denySymlink, denyHardlink)
		cache, repo, _ := blobsOnlyCacheFixture(t, content)
		result, err := cache.Sync(SyncOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Errors) != 0 {
			t.Fatalf("sync errors: %v", result.Errors)
		}
		snapPath, err := repo.SnapshotPath("commit-a", "model-Q4_K_M.gguf")
		if err != nil {
			t.Fatal(err)
		}
		snapInfo := requireRegularFileWithContent(t, snapPath, content)
		friendly := filepath.Join(repo.FriendlyPath(), "model-Q4_K_M.gguf")
		friendlyInfo := requireRegularFileWithContent(t, friendly, content)

		// A repeated rebuild must recognize the existing copy projections as
		// correct instead of re-copying them (file identity preserved).
		if _, err := cache.Sync(SyncOptions{}); err != nil {
			t.Fatal(err)
		}
		snapAgain, err := os.Lstat(snapPath)
		if err != nil || !os.SameFile(snapInfo, snapAgain) {
			t.Fatalf("second sync replaced the repaired snapshot entry: %v", err)
		}
		friendlyAgain, err := os.Lstat(friendly)
		if err != nil || !os.SameFile(friendlyInfo, friendlyAgain) {
			t.Fatalf("second sync replaced the friendly copy: %v", err)
		}
	})

	t.Run("mismatched manifest commit is not used", func(t *testing.T) {
		cache, repo, _ := blobsOnlyCacheFixture(t, content)
		// The ref selects commit-a while the manifest records another commit:
		// the manifest mapping must not be applied to either.
		if err := repo.WriteRef("main", "commit-a"); err != nil {
			t.Fatal(err)
		}
		builder := NewManifestBuilder(Job{Repo: "owner/model"}, "hfdesk test")
		builder.SetCommit("other-commit")
		builder.AddFile("model-Q4_K_M.gguf", fmt.Sprintf("%x", sha256.Sum256(content)), int64(len(content)), true)
		if _, err := builder.Build().Write(repo.FriendlyPath()); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.Sync(SyncOptions{}); err != nil {
			t.Fatal(err)
		}
		for _, commit := range []string{"commit-a", "other-commit"} {
			snapPath, err := repo.SnapshotPath(commit, "model-Q4_K_M.gguf")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(snapPath); !os.IsNotExist(err) {
				t.Fatalf("snapshot entry created under %s from a mismatched manifest: %v", commit, err)
			}
		}
	})
}

// TestCleanOrphanedSymlinksKeepsHardLinkAndCopyEntries verifies the Clean
// pass treats hardlink/copy entries as content, not as orphaned links: only
// broken symlinks are removed.
func TestCleanOrphanedSymlinksKeepsHardLinkAndCopyEntries(t *testing.T) {
	cache, repo, _ := blobsOnlyCacheFixture(t, []byte("content"))
	if err := repo.EnsureFriendlyDir(); err != nil {
		t.Fatal(err)
	}
	friendly := repo.FriendlyPath()

	hardlink := filepath.Join(friendly, "hardlink.gguf")
	if err := os.Link(repo.BlobPath(fmt.Sprintf("%x", sha256.Sum256([]byte("content")))), hardlink); err != nil {
		t.Fatal(err)
	}
	copyEntry := filepath.Join(friendly, "copy.gguf")
	if err := os.WriteFile(copyEntry, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(friendly, "broken.gguf")
	if err := os.Symlink(filepath.Join("missing", "target"), broken); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	result, err := cache.Sync(SyncOptions{Clean: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("clean errors: %v", result.Errors)
	}
	if _, err := os.Lstat(broken); !os.IsNotExist(err) {
		t.Fatalf("broken symlink not removed: %v", err)
	}
	for _, name := range []string{hardlink, copyEntry} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("regular entry %s was removed by orphan cleanup: %v", name, err)
		}
	}
}

// TestCacheDownloadSimulatedWindowsLeavesUsableSnapshotEntries covers the
// primary criterion of issue #99 end to end: a completed HF-cache download on
// a simulated no-symlink platform leaves a usable snapshots/<commit> entry
// and friendly-view entry for every downloaded file, instead of blobs only.
func TestCacheDownloadSimulatedWindowsLeavesUsableSnapshotEntries(t *testing.T) {
	withLinkFakes(t, denySymlink, denyHardlink)
	body := []byte("downloaded payload")
	srv := newMockHFFileServer(t, body)
	root := filepath.Join(t.TempDir(), "cache")
	t.Setenv("HF_HUB_CACHE", "")
	cfg := Settings{CacheDir: root, Endpoint: srv.URL, Concurrency: 1}
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, nil); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "hub", "models--owner--model")
	snapshot := filepath.Join(repo, "snapshots", "commit123", "file.bin")
	snapInfo := requireRegularFileWithContent(t, snapshot, body)
	blobInfo, err := os.Lstat(filepath.Join(repo, "blobs", fmt.Sprintf("%x", sha256.Sum256(body))))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(snapInfo, blobInfo) {
		t.Fatal("copy fallback unexpectedly shared the blob's file identity")
	}
	requireRegularFileWithContent(t, filepath.Join(root, "models", "owner", "model", "file.bin"), body)
}

// TestWindowsRealStoreCreatesUsableSnapshotEntry is the real-OS assertion for
// Windows: whatever the platform decides (symlink with Developer Mode, hard
// link, or copy), a completed store must leave entries that resolve to the
// blob content — so the assertion accepts any entry kind. Real-Windows-only
// assertions are guarded on runtime.GOOS.
func TestWindowsRealStoreCreatesUsableSnapshotEntry(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("real-Windows assertion; simulated cases cover other platforms")
	}
	repo := linkTestRepo(t)
	content := []byte("real windows payload")
	stored, _ := linkTestStore(t, repo, content, "model-Q4_K_M.gguf")
	requireContent(t, stored.SnapshotPath, content)
	requireContent(t, stored.FriendlyPath, content)
}

// TestPlacementLeavesCorrectEntriesUntouchedAndReplacesStale pins the
// re-placement invariant behind the dedupe-hit path: an entry that already
// mirrors its source is left untouched (under the copy fallback a
// remove/re-place would re-copy full content per entry and briefly leave no
// entry at all), while missing, stale, or wrong-shaped entries are still
// created or replaced.
func TestPlacementLeavesCorrectEntriesUntouchedAndReplacesStale(t *testing.T) {
	t.Run("Simulated copy fallback leaves correct entries untouched", func(t *testing.T) {
		withLinkFakes(t, denySymlink, denyHardlink)
		repo := linkTestRepo(t)
		content := []byte("stable placement payload")
		stored, hash := linkTestStore(t, repo, content, "model-Q4_K_M.gguf")
		snapBefore, err := os.Lstat(stored.SnapshotPath)
		if err != nil {
			t.Fatal(err)
		}
		friendlyBefore, err := os.Lstat(stored.FriendlyPath)
		if err != nil {
			t.Fatal(err)
		}

		// Re-run the store: the blob exists (dedupe hit) and the entries are
		// already correct, so neither may be removed and re-copied.
		temp := filepath.Join(t.TempDir(), "download-again")
		if err := os.WriteFile(temp, content, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.StoreDownloadedFile(temp, "model-Q4_K_M.gguf", "commit-a", hash, "", false); err != nil {
			t.Fatal(err)
		}
		snapAfter, err := os.Lstat(stored.SnapshotPath)
		if err != nil || !os.SameFile(snapBefore, snapAfter) {
			t.Fatalf("snapshot entry was re-placed despite being correct: %v", err)
		}
		friendlyAfter, err := os.Lstat(stored.FriendlyPath)
		if err != nil || !os.SameFile(friendlyBefore, friendlyAfter) {
			t.Fatalf("friendly entry was re-placed despite being correct: %v", err)
		}
	})

	t.Run("Simulated copy fallback replaces stale entries", func(t *testing.T) {
		withLinkFakes(t, denySymlink, denyHardlink)
		repo := linkTestRepo(t)
		content := []byte("replacement payload")
		hash := fmt.Sprintf("%x", sha256.Sum256(content))
		snapPath, err := repo.SnapshotPath("commit-a", "model-Q4_K_M.gguf")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(snapPath), 0755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{snapPath, filepath.Join(repo.FriendlyPath(), "model-Q4_K_M.gguf")} {
			if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, []byte("stale content"), 0644); err != nil {
				t.Fatal(err)
			}
		}

		temp := filepath.Join(t.TempDir(), "download")
		if err := os.WriteFile(temp, content, 0644); err != nil {
			t.Fatal(err)
		}
		stored, err := repo.StoreDownloadedFile(temp, "model-Q4_K_M.gguf", "commit-a", hash, "", false)
		if err != nil {
			t.Fatal(err)
		}
		requireRegularFileWithContent(t, stored.SnapshotPath, content)
		requireRegularFileWithContent(t, stored.FriendlyPath, content)
	})

	t.Run("wrong symlink target is replaced", func(t *testing.T) {
		repo := linkTestRepo(t)
		content := []byte("symlink replacement payload")
		snapPath, err := repo.SnapshotPath("commit-a", "model-Q4_K_M.gguf")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(snapPath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", "..", "blobs", "wrong-blob"), snapPath); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}

		temp := filepath.Join(t.TempDir(), "download")
		if err := os.WriteFile(temp, content, 0644); err != nil {
			t.Fatal(err)
		}
		stored, err := repo.StoreDownloadedFile(temp, "model-Q4_K_M.gguf", "commit-a", fmt.Sprintf("%x", sha256.Sum256(content)), "", false)
		if err != nil {
			t.Fatal(err)
		}
		target, err := os.Readlink(stored.SnapshotPath)
		if err != nil {
			t.Fatalf("snapshot entry is not a symlink after replacement: %v", err)
		}
		if !strings.HasSuffix(filepath.ToSlash(target), "blobs/"+fmt.Sprintf("%x", sha256.Sum256(content))) {
			t.Fatalf("snapshot symlink target=%q not aimed at the blob", target)
		}
		requireContent(t, stored.SnapshotPath, content)
	})
}
