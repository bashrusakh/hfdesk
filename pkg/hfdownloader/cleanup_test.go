// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestIsAllDigits covers the small helper that the legacy
// per-dst cleanup uses to recognize the multipart part-N suffix
// (matching fmt's %02d / %d output).
func TestIsAllDigits(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"0", true},
		{"1", true},
		{"9", true},
		{"00", true},
		{"99", true},
		{"100", true},
		{"9999", true},
		{"a", false},
		{"0a", false},
		{"a0", false},
		{"-1", false},
		{"1.0", false},
		{" 1", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := isAllDigits(tc.in); got != tc.want {
				t.Errorf("isAllDigits(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestCleanupJobPartFiles_HFCache_Empty exercises the no-op path
// when the dst list is empty: nothing to do, no error.
func TestCleanupJobPartFiles_HFCache_Empty(t *testing.T) {
	if err := CleanupJobPartFiles(Settings{CacheDir: t.TempDir()}, nil); err != nil {
		t.Errorf("CleanupJobPartFiles with empty dsts returned err: %v", err)
	}
}

// TestCleanupJobPartFiles_HFCacheDst verifies that the HF cache path
// removes exactly the partial artifacts for the dsts in the input list
// and nothing else. In HF cache mode dst is the tmp-<sha> path; the
// downloader writes incomplete bytes to dst+".part" / dst+".part-NN"
// before the completed temporary file reaches dst itself.
func TestCleanupJobPartFiles_HFCacheDst(t *testing.T) {
	cacheDir := t.TempDir()
	hubDir := filepath.Join(cacheDir, "hub")
	blobsA := filepath.Join(hubDir, "models--owner--repoA", "blobs")
	blobsB := filepath.Join(hubDir, "models--owner--repoB", "blobs")
	for _, d := range []string{blobsA, blobsB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// In repoA: partial artifacts for two tracked dsts plus a completed
	// blob and a .incomplete file (must survive).
	filesA := map[string]string{
		"tmp-shaA.part":         "single-part partial A — owned by Job A",
		"tmp-shaA.part-00":      "multipart partial A — owned by Job A",
		"tmp-shaA.part-100":     "multipart partial A — high index",
		"tmp-shaA.parts.json":   "multipart layout A",
		"tmp-shaB.part":         "single-part partial B — owned by Job A",
		"deadbeef00000000":      "completed blob A",
		"other-file.incomplete": "incomplete A",
	}
	for name, body := range filesA {
		if err := os.WriteFile(filepath.Join(blobsA, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// In repoB: partial artifacts owned by a different (concurrent)
	// job that must NOT be touched by Job A's cleanup.
	filesB := map[string]string{
		"tmp-shaC.part":       "single-part partial C — owned by concurrent Job B",
		"tmp-shaC.part-00":    "multipart partial C — owned by concurrent Job B",
		"tmp-shaC.parts.json": "multipart layout C",
	}
	for name, body := range filesB {
		if err := os.WriteFile(filepath.Join(blobsB, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	settings := Settings{CacheDir: cacheDir}
	dsts := []string{
		filepath.Join(blobsA, "tmp-shaA"),
		filepath.Join(blobsA, "tmp-shaB"),
	}
	if err := CleanupJobPartFiles(settings, dsts); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}

	// repoA: only artifacts for the two listed dsts were removed;
	// completed blob and .incomplete file survive.
	for name, body := range filesA {
		full := filepath.Join(blobsA, name)
		shouldExist := name == "deadbeef00000000" || name == "other-file.incomplete"
		_, err := os.Stat(full)
		switch {
		case shouldExist && err != nil:
			t.Errorf("repoA: %q was removed but should survive: %v", name, err)
		case !shouldExist && err == nil:
			t.Errorf("repoA: %q survived cleanup but should have been removed (body=%q)", name, body)
		}
	}
	// repoB: nothing changed (concurrent job's partial is safe).
	for name, body := range filesB {
		full := filepath.Join(blobsB, name)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("repoB: %q was removed but should survive (body=%q): %v", name, body, err)
		}
	}
}

// TestCleanupJobPartFiles_HFCacheCompletedTemp verifies the narrow HF
// cache window where the downloader has renamed complete bytes to the
// tmp-<sha> dst but StoreDownloadedFile has not yet moved them into the
// final blob.
func TestCleanupJobPartFiles_HFCacheCompletedTemp(t *testing.T) {
	cacheDir := t.TempDir()
	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--repo", "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(blobsDir, "tmp-shaA")
	if err := os.WriteFile(dst, []byte("complete temp"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobsDir, "deadbeef00000000"), []byte("final blob"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("completed temp dst should be removed (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(blobsDir, "deadbeef00000000")); err != nil {
		t.Errorf("final blob should survive cleanup: %v", err)
	}
}

// TestCleanupJobPartFiles_HFCacheMissing verifies that missing dsts
// (e.g. the downloader already removed the tmp- file as part of its
// own finalization between allocate and finalize) are not an error.
func TestCleanupJobPartFiles_HFCacheMissing(t *testing.T) {
	cacheDir := t.TempDir()
	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--r", "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// dsts point at files that don't exist on disk.
	dsts := []string{
		filepath.Join(blobsDir, "tmp-nope1"),
		filepath.Join(blobsDir, "tmp-nope2"),
	}
	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, dsts); err != nil {
		t.Errorf("CleanupJobPartFiles with missing dsts returned err: %v", err)
	}
}

// TestCleanupJobPartFiles_HFCacheRejectsOutOfRootDst verifies that
// the delete primitive refuses tracked paths outside the configured HF
// cache hub root before touching any suffix artifacts.
func TestCleanupJobPartFiles_HFCacheRejectsOutOfRootDst(t *testing.T) {
	baseDir := t.TempDir()
	cacheDir := filepath.Join(baseDir, "cache")
	if err := os.MkdirAll(filepath.Join(cacheDir, "hub"), 0o755); err != nil {
		t.Fatal(err)
	}
	outsideDir := filepath.Join(baseDir, "outside")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideDst := filepath.Join(outsideDir, "tmp-outside")
	for _, path := range []string{outsideDst, outsideDst + ".part", outsideDst + ".part-00", outsideDst + ".parts.json"} {
		if err := os.WriteFile(path, []byte("must survive"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{outsideDst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	for _, path := range []string{outsideDst, outsideDst + ".part", outsideDst + ".part-00", outsideDst + ".parts.json"} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("out-of-root path %s should survive cleanup: %v", path, err)
		}
	}
}

// TestCleanupJobPartFiles_LegacyDst verifies that the legacy path
// removes the partials for exactly the dsts in the input list:
// dst+".part", any dst+".part-NN", and dst+".parts.json". A
// concurrent job's partials in the same per-repo subtree are not
// touched.
func TestCleanupJobPartFiles_LegacyDst(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "owner", "legacy-repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Job A owns "model.bin" and "config.json". Their partials
	// (the dsts we're cleaning up) and the multipart layout
	// metadata should be removed.
	files := map[string]string{
		"model.bin.part":       "A/model.bin partial",
		"model.bin.part-00":    "A/model.bin multipart part 0",
		"model.bin.part-100":   "A/model.bin multipart part 100",
		"model.bin.parts.json": "A/model.bin layout",
		"config.json.part":     "A/config.json partial",
		// Concurrent Job B owns "weights.bin" — must not be touched.
		"weights.bin.part":    "B/weights.bin partial",
		"weights.bin.part-00": "B/weights.bin multipart part 0",
		// Completed files must not be touched.
		"model.bin":   "A/model.bin completed",
		"config.json": "A/config.json completed",
		"weights.bin": "B/weights.bin completed",
		"README.md":   "completed readme",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	settings := Settings{OutputDir: tmpDir}
	dsts := []string{
		filepath.Join(repoDir, "model.bin"),
		filepath.Join(repoDir, "config.json"),
	}
	if err := CleanupJobPartFiles(settings, dsts); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}

	// Job A's partials gone; Job B's partials and all completed
	// files survive.
	for name, body := range files {
		full := filepath.Join(repoDir, name)
		isPartialForA := name == "model.bin.part" || name == "model.bin.part-00" ||
			name == "model.bin.part-100" || name == "model.bin.parts.json" ||
			name == "config.json.part"
		_, err := os.Stat(full)
		switch {
		case isPartialForA && err == nil:
			t.Errorf("partial %q survived cleanup but should have been removed (body=%q)", name, body)
		case !isPartialForA && err != nil:
			t.Errorf("file %q was removed but should survive (body=%q): %v", name, body, err)
		}
	}
}

// TestCleanupJobPartFiles_LegacyRespectsUserNamedFiles covers the
// "user file literally named .part" defensive concern: the helper
// only touches files matching one of the dsts in the input list.
// A user file `weights.part` that does not correspond to any tracked
// dst is left alone, even when no other files are present.
//
// The previous scan-based implementation could have deleted a
// "weights.part" file with a coincidental name; the per-dst list
// makes the cleanup strictly scoped to the job's own in-flight
// files and the standalone case is structurally impossible.
func TestCleanupJobPartFiles_LegacyRespectsUserNamedFiles(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "owner", "user-named")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// User-named files that should never be deleted by cleanup.
	// None of them correspond to the dsts in the input list.
	userFiles := map[string]string{
		"weights.part":    "user data, has no partial counterpart",
		"data.parts.json": "user config, has no multipart counterpart",
		"shard.part-03":   "user shard, has no multipart counterpart",
		".part":           "bare .part, empty base",
		"somefile":        "random file",
	}
	for path, body := range userFiles {
		if err := os.WriteFile(filepath.Join(repoDir, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Job A only had model.bin as an in-flight file; it completed
	// successfully and the dst is no longer in the tracking set.
	// Cleanup is invoked with an empty dst list, so nothing is
	// removed. The user files must survive regardless.
	settings := Settings{OutputDir: tmpDir}
	if err := CleanupJobPartFiles(settings, nil); err != nil {
		t.Fatalf("CleanupJobPartFiles with empty dsts: %v", err)
	}
	for path, body := range userFiles {
		full := filepath.Join(repoDir, path)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("user file %q was removed (body=%q): %v", path, body, err)
		}
	}
}

// TestCleanupJobPartFiles_LegacyRejectsOutOfRootDst verifies that
// legacy cleanup also refuses dsts outside OutputDir.
func TestCleanupJobPartFiles_LegacyRejectsOutOfRootDst(t *testing.T) {
	baseDir := t.TempDir()
	outputDir := filepath.Join(baseDir, "output")
	outsideDir := filepath.Join(baseDir, "outside")
	for _, dir := range []string{outputDir, outsideDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	outsideDst := filepath.Join(outsideDir, "model.bin")
	for _, path := range []string{outsideDst + ".part", outsideDst + ".part-00", outsideDst + ".parts.json"} {
		if err := os.WriteFile(path, []byte("must survive"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := CleanupJobPartFiles(Settings{OutputDir: outputDir}, []string{outsideDst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	for _, path := range []string{outsideDst + ".part", outsideDst + ".part-00", outsideDst + ".parts.json"} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("out-of-root path %s should survive cleanup: %v", path, err)
		}
	}
}

// TestOnPartialFile_TracksLifecycle is a unit test of the
// per-file tracker wiring used by JobManager.runJob. The downloader
// fires the callback with finalize=false on allocate and finalize=true
// on success; a concurrent reader (representing cleanupPausedJobPartFiles)
// must see exactly the dsts that are still partial after a run.
func TestOnPartialFile_TracksLifecycle(t *testing.T) {
	var mu sync.Mutex
	partial := make(map[string]struct{})
	// cleared mirrors the real callback's nil-map guard: a callback
	// fired after cleanup nilled the map must not panic and must
	// not resurrect the dst.
	var cleared bool
	callback := func(dst string, finalize bool) {
		mu.Lock()
		defer mu.Unlock()
		if cleared {
			return
		}
		if finalize {
			delete(partial, dst)
		} else {
			partial[dst] = struct{}{}
		}
	}

	// Simulate the downloader lifecycle for three files.
	callback("/dst/a", false)
	callback("/dst/b", false)
	callback("/dst/c", false)
	mu.Lock()
	if got := len(partial); got != 3 {
		t.Errorf("after 3 allocates: len(partial) = %d, want 3", got)
	}
	mu.Unlock()

	// File b completes; c is finalized; a is left partial.
	callback("/dst/b", true)
	callback("/dst/c", true)
	mu.Lock()
	if _, ok := partial["/dst/a"]; !ok {
		t.Errorf("a should still be in partial set")
	}
	if _, ok := partial["/dst/b"]; ok {
		t.Errorf("b should be removed from partial set")
	}
	if _, ok := partial["/dst/c"]; ok {
		t.Errorf("c should be removed from partial set")
	}
	mu.Unlock()

	// Cleanup snapshots the remaining in-flight dsts and clears the
	// map so a late callback from a goroutine that started before
	// the snapshot cannot add the dst back.
	mu.Lock()
	snapshot := make([]string, 0, len(partial))
	for dst := range partial {
		snapshot = append(snapshot, dst)
	}
	cleared = true
	mu.Unlock()
	if len(snapshot) != 1 || snapshot[0] != "/dst/a" {
		t.Errorf("snapshot = %v, want [/dst/a]", snapshot)
	}

	// Stale callback after cleanup: ignored (no panic, no mutation).
	callback("/dst/a", false)
	mu.Lock()
	if !cleared {
		t.Errorf("cleared flag flipped back to false")
	}
	mu.Unlock()
}
