// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsPartialFileName exercises the suffix matcher that decides which
// files in a blobs dir or output tree count as partial-download
// artifacts (.part, .part-NN, .parts.json).
func TestIsPartialFileName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Real partial-file names.
		{"tmp-abc123def456.part", true},
		{"tmp-abc123def456.part-00", true},
		{"tmp-abc123def456.part-01", true},
		{"tmp-abc123def456.part-99", true},
		{"tmp-abc123def456.parts.json", true},
		// Multipart part-NN with a long path before (e.g. sanitized
		// repo-relative name when SHA256 is empty).
		{"tmp-config_json.part-00", true},
		// Generic legacy-mode names.
		{"model.bin.part", true},
		{"model.bin.part-00", true},
		{"model.bin.parts.json", true},
		// Should NOT match: completed blobs, metadata, symlinks.
		{"abc123def456", false},
		{"abc123def456.incomplete", false},
		{"abc123def456.incomplete.meta", false},
		{"refs", false},
		{"snapshots", false},
		// Should NOT match: .part followed by something other than two digits.
		{"tmp-abc.part-0", false},   // one digit
		{"tmp-abc.part-100", false}, // three digits
		{"tmp-abc.part-", false},    // empty suffix
		{"tmp-abc.part-0a", false},  // non-digit
		// Edge cases.
		{"", false},
		{".part", true},       // bare suffix is allowed (defensive)
		{".part.json", false}, // unrelated JSON
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPartialFileName(tc.name); got != tc.want {
				t.Errorf("isPartialFileName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestCleanupJobPartFiles_HFCache verifies that the HF-cache variant
// removes every .part / .part-NN / .parts.json file in the repo's
// blobs directory and leaves other repos' blobs alone.
func TestCleanupJobPartFiles_HFCache(t *testing.T) {
	cacheDir := t.TempDir()
	hubDir := filepath.Join(cacheDir, "hub")
	blobsA := filepath.Join(hubDir, "models--owner--repoA", "blobs")
	blobsB := filepath.Join(hubDir, "models--owner--repoB", "blobs")
	if err := os.MkdirAll(blobsA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(blobsB, 0o755); err != nil {
		t.Fatal(err)
	}

	// In repoA: a mix of partial files plus a "completed" blob that
	// must survive cleanup.
	filesA := map[string]string{
		"tmp-shaA.part":         "partial A",
		"tmp-shaA.part-00":      "part 0 A",
		"tmp-shaA.part-01":      "part 1 A",
		"tmp-shaA.parts.json":   `{"layout":"v1"}`,
		"tmp-pathA.part":        "synth path A",
		"deadbeef00000000":      "completed blob A", // real blob, must NOT be deleted
		"other-file.incomplete": "incomplete A",     // belongs to .incomplete scheme, not us
	}
	for name, body := range filesA {
		if err := os.WriteFile(filepath.Join(blobsA, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// In repoB: a partial file that must be left alone when we only
	// clean repoA.
	filesB := map[string]string{
		"tmp-shaB.part":    "partial B",
		"tmp-shaB.part-00": "part 0 B",
	}
	for name, body := range filesB {
		if err := os.WriteFile(filepath.Join(blobsB, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	settings := Settings{CacheDir: cacheDir}
	job := Job{Repo: "owner/repoA"}
	if err := CleanupJobPartFiles(settings, job); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}

	// repoA: partial files gone, completed blob and .incomplete file
	// remain.
	for name := range filesA {
		_, err := os.Stat(filepath.Join(blobsA, name))
		shouldExist := name == "deadbeef00000000" || name == "other-file.incomplete"
		switch {
		case shouldExist && err != nil:
			t.Errorf("repoA: %q was removed but should survive: %v", name, err)
		case !shouldExist && err == nil:
			t.Errorf("repoA: %q survived cleanup but should have been removed", name)
		}
	}

	// repoB: nothing changed.
	for name := range filesB {
		if _, err := os.Stat(filepath.Join(blobsB, name)); err != nil {
			t.Errorf("repoB: %q was removed but should survive: %v", name, err)
		}
	}
}

// TestCleanupJobPartFiles_LocalRepo verifies that LocalRepo reroutes
// the cleanup target to the destination folder, not the source repo.
func TestCleanupJobPartFiles_LocalRepo(t *testing.T) {
	cacheDir := t.TempDir()
	hubDir := filepath.Join(cacheDir, "hub")
	blobsLocal := filepath.Join(hubDir, "models--target--final", "blobs")
	blobsUpstream := filepath.Join(hubDir, "models--upstream--mmproj", "blobs")
	for _, d := range []string{blobsLocal, blobsUpstream} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(blobsLocal, "tmp-ll.part"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobsUpstream, "tmp-up.part"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	settings := Settings{CacheDir: cacheDir}
	job := Job{Repo: "upstream/mmproj", LocalRepo: "target/final"}
	if err := CleanupJobPartFiles(settings, job); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(blobsLocal, "tmp-ll.part")); !os.IsNotExist(err) {
		t.Errorf("local repo partial not removed: stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(blobsUpstream, "tmp-up.part")); err != nil {
		t.Errorf("upstream repo partial was removed but should survive: %v", err)
	}
}

// TestCleanupJobPartFiles_Legacy verifies that legacy (OutputDir) mode
// walks the per-repo subtree and removes only .part/.part-NN/.parts.json
// files.
func TestCleanupJobPartFiles_Legacy(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "owner", "legacy-repo")
	subDir := filepath.Join(repoDir, "sub", "deeper")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{
		filepath.Join(repoDir, "model.bin.part"):       "partial",
		filepath.Join(repoDir, "model.bin.part-00"):    "p0",
		filepath.Join(repoDir, "model.bin.parts.json"): "layout",
		filepath.Join(subDir, "config.json.part"):      "cfg partial",
		filepath.Join(subDir, "config.json"):           "completed cfg", // must NOT be deleted
		filepath.Join(repoDir, "README.md"):            "completed readme",
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	settings := Settings{OutputDir: tmpDir}
	job := Job{Repo: "owner/legacy-repo"}
	if err := CleanupJobPartFiles(settings, job); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}

	// All .part* files removed; completed files survive.
	wantGone := []string{
		filepath.Join(repoDir, "model.bin.part"),
		filepath.Join(repoDir, "model.bin.part-00"),
		filepath.Join(repoDir, "model.bin.parts.json"),
		filepath.Join(subDir, "config.json.part"),
	}
	for _, p := range wantGone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("partial not removed (%v): %v", p, err)
		}
	}
	wantKept := []string{
		filepath.Join(subDir, "config.json"),
		filepath.Join(repoDir, "README.md"),
	}
	for _, p := range wantKept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("completed file removed (%v): %v", p, err)
		}
	}
}

// TestCleanupJobPartFiles_NoBlobsDir is best-effort: the function must
// not return an error when the repo's blobs directory does not exist
// yet (e.g. a job that was paused before the first download was
// scheduled).
func TestCleanupJobPartFiles_NoBlobsDir(t *testing.T) {
	cacheDir := t.TempDir()
	settings := Settings{CacheDir: cacheDir}
	job := Job{Repo: "owner/never-touched"}
	if err := CleanupJobPartFiles(settings, job); err != nil {
		t.Errorf("CleanupJobPartFiles on missing blobs dir returned err: %v", err)
	}
}

// TestCleanupJobPartFiles_BadRepo ensures that an invalid repo ID is a
// no-op rather than an error — callers must not be punished for a
// config that we couldn't have written to disk in the first place.
func TestCleanupJobPartFiles_BadRepo(t *testing.T) {
	cacheDir := t.TempDir()
	settings := Settings{CacheDir: cacheDir}
	job := Job{Repo: "no-slash"}
	if err := CleanupJobPartFiles(settings, job); err != nil {
		t.Errorf("CleanupJobPartFiles on bad repo ID returned err: %v", err)
	}
}
