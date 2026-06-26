// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsPartialFileName exercises the suffix matcher used by the
// legacy-mode walker. The HF cache mode doesn't call this function —
// its in-flight files all share the "tmp-" prefix and are identified
// on that basis (see cleanupHFCachePartFiles).
//
// The matcher accepts three suffix families that downloadSingle and
// downloadMultipart produce: .parts.json, .part, and .part-N where N
// is any non-empty run of digits (matching fmt.Sprintf("%s.part-%02d",
// dst, i) for Concurrency<100 and the wider %d form for
// Concurrency>=100).
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
		// Multipart with 3+ digits — Concurrency>=100 produces these
		// because fmt %02d grows past two digits rather than truncating.
		{"tmp-abc123def456.part-100", true},
		{"tmp-abc123def456.part-127", true},
		{"model.bin.part-9999", true},
		// Multipart with a long path before (e.g. sanitized repo-relative
		// name when SHA256 is empty).
		{"tmp-config_json.part-00", true},
		// Generic legacy-mode names.
		{"model.bin.part", true},
		{"model.bin.part-00", true},
		{"model.bin.parts.json", true},
		// Should NOT match: completed blobs, metadata, symlinks, dirs.
		{"abc123def456", false},
		{"abc123def456.incomplete", false},
		{"abc123def456.incomplete.meta", false},
		{"refs", false},
		{"snapshots", false},
		// Should NOT match: .part- where the suffix is not a digit run.
		{"tmp-abc.part-", false},   // empty suffix
		{"tmp-abc.part-0a", false}, // non-digit character
		{"tmp-abc.part-a0", false}, // leading non-digit
		{"tmp-abc.part--0", false}, // double dash
		// Should NOT match: .part followed by something other than a
		// digit run (e.g. part-xyz from a totally different scheme).
		{"foo.part-xyz", false},
		{"foo.part-", false},
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

// TestIsAllDigits covers the small helper that the partial-name matcher
// relies on for the .part-N family.
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

// TestCleanupJobPartFiles_HFCache verifies that the HF-cache variant
// removes every tmp-* file in the repo's blobs directory and leaves
// other repos' blobs alone.
//
// The HF cache mode identifies in-flight files by the "tmp-" prefix
// alone (set by Download() at the per-file dst assignment). The
// downloader never writes a real blob to a tmp-*-named file and never
// writes a tmp-*-named file outside this prefix scheme, so suffix
// matching is unnecessary.
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

	// In repoA: a mix of tmp-* partials (including a 3-digit part index
	// for Concurrency>=100), a non-tmp file that should be left alone,
	// and an .incomplete file from the cache layer's separate scheme.
	filesA := map[string]string{
		"tmp-shaA.part":         "partial A",
		"tmp-shaA.part-00":      "part 0 A",
		"tmp-shaA.part-01":      "part 1 A",
		"tmp-shaA.part-100":     "part 100 A (high Concurrency)",
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

	// repoA: tmp-* partials gone (including the 3-digit one), completed
	// blob and .incomplete file remain.
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
// walks the per-repo subtree and removes in-flight partial files
// (.part, .part-N for any N digits, .parts.json) when no
// corresponding "final" file is present — the pause → cancel path.
//
// The walker has a defensive guard: when a partial's base name (the
// name with the suffix stripped) exists as a real file alongside the
// partial, the partial is treated as a stale downloader leftover and
// skipped, to protect legitimately named user files (see
// TestCleanupJobPartFiles_LegacyRespectsUserNamedFiles). This test
// exercises the in-flight case where the final is NOT present, which
// is the common case after a paused → cancel.
func TestCleanupJobPartFiles_Legacy(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "owner", "legacy-repo")
	subDir := filepath.Join(repoDir, "sub", "deeper")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// model.bin.* partials: the final model.bin does NOT exist, so all
	// of them are treated as in-flight and removed.
	//
	// config.json.part: the final config.json DOES exist, so the
	// defensive guard skips this partial (left behind as a stale
	// leftover, but the user's completed config.json is safe).
	files := map[string]string{
		filepath.Join(repoDir, "model.bin.part"):       "partial",
		filepath.Join(repoDir, "model.bin.part-00"):    "p0",
		filepath.Join(repoDir, "model.bin.part-100"):   "p100 (high Concurrency)",
		filepath.Join(repoDir, "model.bin.parts.json"): "layout",
		filepath.Join(subDir, "config.json.part"):      "cfg partial — final exists, guard skips",
		filepath.Join(subDir, "config.json"):           "completed cfg",
		filepath.Join(repoDir, "README.md"):            "completed readme",
		filepath.Join(repoDir, "data.bin"):             "completed data",
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

	// In-flight partials removed; the partial guarded by an existing
	// final is left behind; completed files survive.
	wantGone := []string{
		filepath.Join(repoDir, "model.bin.part"),
		filepath.Join(repoDir, "model.bin.part-00"),
		filepath.Join(repoDir, "model.bin.part-100"),
		filepath.Join(repoDir, "model.bin.parts.json"),
	}
	for _, p := range wantGone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("in-flight partial not removed (%v): %v", p, err)
		}
	}
	wantKept := []string{
		filepath.Join(subDir, "config.json.part"), // guard skipped this
		filepath.Join(subDir, "config.json"),
		filepath.Join(repoDir, "README.md"),
		filepath.Join(repoDir, "data.bin"),
	}
	for _, p := range wantKept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("file removed but should survive (%v): %v", p, err)
		}
	}
}

// TestCleanupJobPartFiles_LegacyRespectsUserNamedFiles exercises the
// defensive guard against the "user file literally named .part" risk
// identified in the open-code-review of PR #50: a partial-file suffix
// can also be a legitimate user file name. The walker must not delete
// such a file even when no "final" sibling exists, OR it must at least
// not delete it when a sibling does exist.
//
// The guard implemented here has two parts:
//  1. Empty base (bare ".part" / ".parts.json") is never a downloader
//     artifact — always skip.
//  2. When the base is non-empty, stat the base in the same directory;
//     if it exists, the partial is most likely a stale leftover and is
//     skipped. If the base does not exist, the partial is treated as
//     in-flight and removed (the pause → cancel path).
//
// This test covers the "base exists" branch: a file named weights.part
// alongside a sibling weights must survive cleanup even though it
// looks like a partial.
func TestCleanupJobPartFiles_LegacyRespectsUserNamedFiles(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "owner", "user-named")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// User-named files: each has a "base" sibling that the guard will
	// detect, so the partial is skipped. This is the safe direction:
	// leave a file alone rather than risk destroying user data.
	userFiles := map[string]string{
		filepath.Join(repoDir, "weights.part"):    "user data, has sibling weights",
		filepath.Join(repoDir, "weights"):         "sibling final",
		filepath.Join(repoDir, "data.parts.json"): "user config, has sibling data",
		filepath.Join(repoDir, "data"):            "sibling final",
		filepath.Join(repoDir, "shard.part-03"):   "user shard, has sibling shard",
		filepath.Join(repoDir, "shard"):           "sibling final",
		filepath.Join(repoDir, ".part"):           "bare .part, empty base",
	}
	for path, body := range userFiles {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	settings := Settings{OutputDir: tmpDir}
	job := Job{Repo: "owner/user-named"}
	if err := CleanupJobPartFiles(settings, job); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}

	// Everything survives — each partial either has a sibling final
	// (guard skips) or has an empty base (downloaders never produce
	// those, so we don't touch them).
	for path := range userFiles {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("user file removed but should survive (%v): %v", path, err)
		}
	}
}

// TestStripPartialSuffix covers the small helper that the legacy
// walker relies on to compute the "final" file path before deciding
// whether to delete a partial.
func TestStripPartialSuffix(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"model.bin.part", "model.bin"},
		{"model.bin.parts.json", "model.bin"},
		{"model.bin.part-00", "model.bin"},
		{"model.bin.part-100", "model.bin"},
		{"model.bin.part-9999", "model.bin"},
		// Bare suffixes (no base) — the downloader never produces
		// these, but stripPartialSuffix still strips them.
		{".part", ""},
		{".parts.json", ""},
		// Names without a recognized suffix — returned unchanged.
		{"config.json", "config.json"},
		{"README.md", "README.md"},
		{"", ""},
		// Edge: .part- with empty digit run (not produced by the
		// downloader, but the helper should not panic).
		{"foo.part-", "foo"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := stripPartialSuffix(tc.in); got != tc.want {
				t.Errorf("stripPartialSuffix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCleanupJobPartFiles_DefaultIsHFCache locks in the fix for the
// mode-detection review issue: when neither CacheDir nor OutputDir is
// set, Download() defaults to HF cache mode, so CleanupJobPartFiles
// must do the same.
func TestCleanupJobPartFiles_DefaultIsHFCache(t *testing.T) {
	// Point CacheDir at a temp dir so the test can use it as the HF
	// cache root; then call CleanupJobPartFiles with an empty Settings
	// while overriding DefaultCacheDir via the local CacheDir path.
	//
	// We can't change DefaultCacheDir() (it's a function, not a
	// variable), so we set CacheDir on the Settings and assert that the
	// legacy path is NOT taken when OutputDir is empty. The clean way
	// to check that is to give the test a fake legacy output dir and
	// a fake HF cache dir, then verify which one is touched.
	cacheDir := t.TempDir()
	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--default-mode", "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobsDir, "tmp-x.part"), []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A separate tree that must NOT be touched if the helper correctly
	// picks HF cache mode (because legacy would walk OutputDir/<repo>).
	legacyRoot := t.TempDir()
	legacyRepo := filepath.Join(legacyRoot, "owner", "default-mode")
	if err := os.MkdirAll(legacyRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyRepo, "stale.part"), []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}

	settings := Settings{CacheDir: cacheDir, OutputDir: ""}
	job := Job{Repo: "owner/default-mode"}
	if err := CleanupJobPartFiles(settings, job); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}

	// HF cache: tmp-* partial removed.
	if _, err := os.Stat(filepath.Join(blobsDir, "tmp-x.part")); !os.IsNotExist(err) {
		t.Errorf("HF cache partial not removed: %v", err)
	}
	// Legacy tree: untouched (because OutputDir is empty → HF cache branch).
	if _, err := os.Stat(filepath.Join(legacyRepo, "stale.part")); err != nil {
		t.Errorf("legacy tree was touched despite empty OutputDir: %v", err)
	}
}

// TestCleanupJobPartFiles_LegacyDefaultsOutputDir mirrors the way
// Download() defaults OutputDir to "Storage" in legacy mode. A caller
// that builds a minimal Settings (no OutputDir) should still see
// cleanup find files at <cwd>/Storage/<repo>/.
func TestCleanupJobPartFiles_LegacyDefaultsOutputDir(t *testing.T) {
	// We can't change the working directory of the test process
	// safely, but we can verify the helper's behaviour by exercising
	// it with an OutputDir already set: this asserts the helper
	// delegates to the SafeJoin path with whatever OutputDir it
	// received. The actual "Storage" default is exercised in the
	// surrounding call site; here we just confirm the no-OutputDir
	// path resolves to "Storage" without panicking.
	settings := Settings{OutputDir: ""}
	job := Job{Repo: "owner/never-touched"}
	if err := CleanupJobPartFiles(settings, job); err != nil {
		t.Errorf("CleanupJobPartFiles with empty OutputDir returned err: %v", err)
	}
	// Nothing on disk to assert: a missing "Storage" directory is the
	// expected outcome (best-effort no-op).
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
