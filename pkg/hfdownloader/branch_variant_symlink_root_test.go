// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBranchVariantPayloadLinkStaysInsideSymlinkedDestinationRoot proves that
// the app-owned branch payload entry names the snapshot entry of its own
// destination root when the root is reached through a linked parent.
//
// The service tree is addressed in the destination root's canonical
// (symlink-resolved) spelling. A link target computed against the lexical
// spelling of the Hub instead leaves the root and re-enters it through the
// alias, so a copied root keeps a payload link that still points at the
// source root and the copy is no longer recognized as a valid store.
func TestBranchVariantPayloadLinkStaysInsideSymlinkedDestinationRoot(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(base, "linked")
	if err := os.Symlink(realRoot, linkedParent); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	cacheRoot := filepath.Join(linkedParent, "cache")
	revisions := branchVariantTestRevisions()
	hub := newBranchVariantTestHub(t, revisions)
	defer hub.Close()
	job := Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}
	if err := Download(context.Background(), job, Settings{
		CacheDir: cacheRoot, Endpoint: hub.URL, Concurrency: 1, Verify: "sha256",
	}, nil); err != nil {
		t.Fatalf("branch download into a symlinked destination root: %v", err)
	}

	artifact, err := ReadBranchVariantArtifact(cacheRoot, RepoTypeModel, job.Repo, job.Revision, revisions[job.Revision].commit)
	if err != nil || artifact == nil {
		t.Fatalf("branch artifact=%+v err=%v", artifact, err)
	}
	repoDir, err := NewHFCache(cacheRoot, 0).Repo(job.Repo, RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	for name := range revisions[job.Revision].files {
		payloadEntry := filepath.Join(artifact.PayloadRoot, name)
		entryInfo, err := os.Lstat(payloadEntry)
		if err != nil {
			t.Fatalf("inspect branch payload entry %q: %v", payloadEntry, err)
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(payloadEntry)
			if err != nil {
				t.Fatalf("read branch payload link %q: %v", payloadEntry, err)
			}
			// The link target may not leave the destination root and re-enter
			// it through the alias: the root has to stay self-contained.
			joined := filepath.Join(filepath.Dir(payloadEntry), target)
			rel, err := filepath.Rel(canonicalRoot, joined)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("branch payload link %q leaves the destination root: target=%q joined=%q root=%q",
					payloadEntry, target, joined, canonicalRoot)
			}
		}
		// The entry must name the snapshot entry of the same root.
		snapshot, err := repoDir.SnapshotPath(revisions[job.Revision].commit, name)
		if err != nil {
			t.Fatal(err)
		}
		payloadStat, err := os.Stat(payloadEntry)
		if err != nil {
			t.Fatalf("resolve branch payload entry %q: %v", payloadEntry, err)
		}
		snapshotStat, err := os.Stat(snapshot)
		if err != nil {
			t.Fatalf("resolve snapshot entry %q: %v", snapshot, err)
		}
		if !os.SameFile(payloadStat, snapshotStat) {
			t.Fatalf("branch payload entry %q does not name the snapshot entry %q of the same root", payloadEntry, snapshot)
		}
	}
}
