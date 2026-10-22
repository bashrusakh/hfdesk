// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

// Mirror scans, copies, verification and blob counts must treat downloader
// staging artifacts (payload, resume record, its atomic-write temp, and the
// persistent ownership/transition locks) as non-content, consistently with the
// cache list/stats and cache-selection code.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMirrorSkipsStagingArtifacts(t *testing.T) {
	base := t.TempDir()
	srcHub := filepath.Join(base, "src-hub")
	repo := filepath.Join(srcHub, "models--owner--model")
	blobs := filepath.Join(repo, "blobs")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 64)
	content := "finished-blob"
	files := map[string]string{
		sha:                          content,
		sha + ".incomplete":          "partial-payload",
		sha + ".incomplete.meta":     `{"version":2}`,
		sha + ".incomplete.meta.tmp": `{"version":2}`,
		sha + ".incomplete.lock":     "",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(blobs, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := scanHubForMirror(srcHub)
	if err != nil {
		t.Fatalf("scanHubForMirror: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("scanHubForMirror returned %d repos, want 1", len(entries))
	}
	if entries[0].Size != int64(len(content)) {
		t.Errorf("mirror repo size = %d, want only the finished blob (%d)", entries[0].Size, len(content))
	}
	if got := countBlobs(repo); got != 1 {
		t.Errorf("countBlobs = %d, want 1 finished blob", got)
	}

	dstHub := filepath.Join(base, "dst-hub")
	if err := copyRepoCache(repo, srcHub, dstHub); err != nil {
		t.Fatalf("copyRepoCache: %v", err)
	}
	dstBlobs := filepath.Join(dstHub, "models--owner--model", "blobs")
	if data, err := os.ReadFile(filepath.Join(dstBlobs, sha)); err != nil || string(data) != content {
		t.Fatalf("finished blob not copied: %q %v", data, err)
	}
	for name := range files {
		if name == sha {
			continue
		}
		if _, err := os.Stat(filepath.Join(dstBlobs, name)); !os.IsNotExist(err) {
			t.Errorf("staging artifact %s was mirrored, stat err=%v", name, err)
		}
	}
	if err := verifyRepoCache(repo, srcHub, dstHub); err != nil {
		t.Errorf("verifyRepoCache: %v", err)
	}
}
