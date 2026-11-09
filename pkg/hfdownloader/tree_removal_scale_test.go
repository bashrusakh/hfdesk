// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Scale regression for whole-tree removal admission: a repository with
// thousands of directories must be wholly admitted and removable under the
// common 1024-fd soft file-descriptor limit (the admission holds two open
// lock files per directory for the whole tree), the whole-tree-EX-before-
// removal rule must keep refusing a concurrent writer on a deep descendant,
// and a tree that exceeds even the raiseable hard limit must fail with an
// actionable limit error naming the remedy instead of a raw EMFILE.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	// treeRemovalScaleDirectories is the flat directory count the admission
	// must handle under a default 1024 soft file-descriptor limit.
	treeRemovalScaleDirectories = 10000
	// treeRemovalDefaultSoftLimit is the common desktop/systemd soft
	// file-descriptor limit at which admission used to fail with EMFILE.
	treeRemovalDefaultSoftLimit = 1024
	// treeRemovalDepthChain is the deep descendant chain that carries the
	// concurrent writer. Its components sort before "data", so a refused
	// admission reaches the writer conflict without walking the whole flat
	// tree first.
	treeRemovalDepthChain = 48
	// treeRemovalFdChildEnv marks the disposable child process of the
	// actionable-limit test: lowering a hard limit cannot be undone, so it
	// happens only in the child.
	treeRemovalFdChildEnv = "HFDESK_TREE_REMOVAL_FD_LIMIT_CHILD"
)

// buildTreeRemovalScaleTree creates a repository under hub with
// treeRemovalScaleDirectories flat directories plus a deep chain whose leaf
// carries payloadFile.
func buildTreeRemovalScaleTree(t *testing.T, hub string) (repo, deepFile string) {
	t.Helper()
	repo = filepath.Join(hub, "models--owner--scale")
	data := filepath.Join(repo, "data")
	for i := 0; i < treeRemovalScaleDirectories; i++ {
		if err := os.MkdirAll(filepath.Join(data, fmt.Sprintf("dir-%05d", i)), 0o755); err != nil {
			t.Fatalf("create scale directory %d: %v", i, err)
		}
	}
	deep := repo
	for level := 0; level < treeRemovalDepthChain; level++ {
		deep = filepath.Join(deep, fmt.Sprintf("a%02d", level))
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("create deep chain: %v", err)
	}
	deepFile = filepath.Join(deep, "payload.bin")
	if err := os.WriteFile(deepFile, []byte("scale"), 0o644); err != nil {
		t.Fatalf("create deep descendant file: %v", err)
	}
	return repo, deepFile
}

func currentSoftFileLimit(t *testing.T) uint64 {
	t.Helper()
	soft, _, ok := currentOwnershipFileLimit()
	if !ok {
		t.Fatal("process file-descriptor limits are unavailable")
	}
	return soft
}

// TestTreeRemovalAdmitsAndRemovesALargeTreeUnderDefaultFdLimit pins the
// large-tree envelope: whole-repo tree admission (the shared path behind
// DELETE /api/cache/..., mirror deleteExtra and HF selected-GGUF delete)
// admits a >10,000-directory repository with the soft limit at the common
// default 1024, removes it while the admission is held, and keeps the
// whole-tree-EX-before-removal rule: a concurrent writer on a deep
// descendant is refused while the admission is held, and a held deep writer
// refuses admission outright. The raise covers exactly the admission window.
func TestTreeRemovalAdmitsAndRemovesALargeTreeUnderDefaultFdLimit(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	originalSoft, hard, ok := currentOwnershipFileLimit()
	if !ok {
		t.Skip("this platform exposes no process file-descriptor limit model")
	}
	// Two held locks per directory must fit after the raise toward the hard
	// limit; a hard limit that cannot cover the tree is the documented
	// actionable-error envelope (exercised by the companion test), not a
	// valid host for this one.
	needed := uint64(2*(treeRemovalScaleDirectories+treeRemovalDepthChain) + 512)
	if hard < needed {
		t.Skipf("hard file-descriptor limit %d cannot cover the %d-directory tree (~%d locks); this environment only supports the actionable-error envelope",
			hard, treeRemovalScaleDirectories, needed)
	}
	// Simulate the common default: soft limit 1024.
	if err := setOwnershipSoftFileLimit(treeRemovalDefaultSoftLimit); err != nil {
		t.Fatalf("lower the soft file-descriptor limit to the common default: %v", err)
	}
	defer func() {
		if err := setOwnershipSoftFileLimit(originalSoft); err != nil {
			t.Errorf("restore the original soft file-descriptor limit: %v", err)
		}
	}()

	ctx := t.Context()
	hub := t.TempDir()
	repo, deepFile := buildTreeRemovalScaleTree(t, hub)
	admission, err := OpenCacheAdmission(ctx, hub)
	if err != nil {
		t.Fatalf("open cache admission: %v", err)
	}

	// Direction 1: a concurrent writer on a deep descendant refuses tree
	// admission entirely — no partial admission may stand beside a live
	// writer. (Its chain sorts first, so the refusal arrives without a full
	// walk; nothing is removed either way.)
	writerRelease, err := admission.AdmitFileWrite(ctx, deepFile)
	if err != nil {
		t.Fatalf("admit the deep writer before tree admission: %v", err)
	}
	if release, err := admission.AdmitTreeRemoval(ctx, repo); err == nil {
		release()
		t.Fatal("tree admission succeeded while a deep writer held its item scope")
	} else if !errors.Is(err, ErrOwnershipBusy) {
		t.Fatalf("tree admission over a live deep writer = %v, want ErrOwnershipBusy", err)
	}
	writerRelease()

	// The whole tree must admit under the default soft limit. Before the
	// soft-limit raise this failed with raw EMFILE after roughly 500
	// directories at soft=1024.
	admitStart := time.Now()
	release, err := admission.AdmitTreeRemoval(ctx, repo)
	admitElapsed := time.Since(admitStart)
	if err != nil {
		t.Fatalf("a %d-directory tree did not admit under the default soft fd limit %d (hard %d): %v",
			treeRemovalScaleDirectories, treeRemovalDefaultSoftLimit, hard, err)
	}
	// While the admission is held, the soft limit is raised toward the hard
	// limit so the held lock handles fit; it is restored with the release.
	if heldSoft := currentSoftFileLimit(t); heldSoft <= treeRemovalDefaultSoftLimit {
		t.Errorf("soft fd limit while the tree admission is held = %d, want raised above %d", heldSoft, treeRemovalDefaultSoftLimit)
	}

	// Direction 2 (whole-tree-EX-before-removal): with the admission held
	// and removal not yet run, a writer on the same deep descendant is
	// refused — the whole tree, deep nodes included, is exclusively admitted
	// before any removal happens.
	if _, err := admission.AdmitFileWrite(ctx, deepFile); !errors.Is(err, ErrOwnershipBusy) {
		t.Errorf("deep writer during held tree admission = %v, want ErrOwnershipBusy", err)
	}

	// Removal proceeds under the held admission, exactly as the server,
	// mirror and selected-delete callers do.
	removeStart := time.Now()
	if err := os.RemoveAll(repo); err != nil {
		release()
		t.Fatalf("remove the admitted tree: %v", err)
	}
	removeElapsed := time.Since(removeStart)
	if _, err := os.Lstat(repo); !errors.Is(err, os.ErrNotExist) {
		release()
		t.Fatalf("repository still present after admitted removal: %v", err)
	}
	release()

	if restored := currentSoftFileLimit(t); restored != treeRemovalDefaultSoftLimit {
		t.Errorf("soft fd limit after the admission release = %d, want restored to %d", restored, treeRemovalDefaultSoftLimit)
	}
	t.Logf("admitted and removed %d directories under soft fd limit %d (hard %d): admission %v, removal %v",
		treeRemovalScaleDirectories+treeRemovalDepthChain+2, treeRemovalDefaultSoftLimit, hard, admitElapsed, removeElapsed)
}

// TestTreeRemovalAdmissionReportsActionableFileLimitError verifies the
// beyond-the-hard-limit envelope in a disposable child process: when neither
// the soft limit nor the hard limit can cover the tree, tree admission fails
// with an actionable error naming the observed limits and the remedy — never
// a raw EMFILE, never an error readable as a vanished target — and nothing
// is removed. The limits are lowered in the child
// because lowering a hard limit cannot be undone in this process.
func TestTreeRemovalAdmissionReportsActionableFileLimitError(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	if _, _, ok := currentOwnershipFileLimit(); !ok {
		t.Skip("this platform exposes no process file-descriptor limit model")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTreeRemovalAdmissionFileLimitChild$", "-test.v")
	cmd.Env = append(os.Environ(), treeRemovalFdChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("actionable file-limit child failed: %v\n%s", err, out)
	}
}
