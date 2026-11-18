// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func mustMkdirAll(t *testing.T, path string) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertGone(t *testing.T, paths ...string) {
	for _, path := range paths {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived, stat err=%v", path, err)
		}
	}
}

func assertRealDir(t *testing.T, paths ...string) {
	for _, path := range paths {
		if info, err := os.Lstat(path); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s must survive as a real directory: %v", path, err)
		}
	}
}

func newFlatTestRealm(t *testing.T) *ownershipRealm {
	realm, err := newOwnershipRealm(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return realm
}

// mustOwnershipLocks skips the test on a platform without the OS ownership
// lock primitive these coordination tests require.
func mustOwnershipLocks(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
}

// vanishOnceAt installs an ownership gap hook that removes path exactly once
// when the named gap observes it, and fails the test if the gap never fired.
func vanishOnceAt(t *testing.T, site, path string) {
	fired := false
	ownershipPathGapHook = func(gotSite, got string) {
		if !fired && gotSite == site && got == path {
			fired = os.Remove(path) == nil
		}
	}
	t.Cleanup(func() {
		ownershipPathGapHook = nil
		if !fired {
			t.Errorf("fixture never forced the %s gap", site)
		}
	})
}

// Explicit cancel removes the partial and its whole empty directory chain up
// to — but not including — the output root.
func TestCancelCleanupPrunesEmptyDirectoryChainToRoot(t *testing.T) {
	const size = 64 * 1024
	full := rangedTestPayload(size)
	sha := singleTestSHA(full)
	root := t.TempDir()
	dst := filepath.Join(root, "vendor", "model", "file.bin")
	mustMkdirAll(t, filepath.Dir(dst))
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "file.bin", Size: size, LFS: true, SHA256: sha}
	seedSinglePartial(t, dst, full, job, it, completeSingleIdentity())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	singleProofStartHook = func(*os.File) { cancel() }
	t.Cleanup(func() { singleProofStartHook = nil })

	cfg := Settings{
		OutputDir:               root,
		Verify:                  "sha256",
		Retries:                 0,
		StallTimeout:            "0",
		CleanupPartialsOnCancel: func() bool { return true },
	}
	opts := completeSingleIdentity()
	opts.Domain = acquireTestDomain(t, dst)
	verified, err := downloadSingle(ctx, http.DefaultClient, "", job, cfg, it, dst, opts, func(ProgressEvent) {})
	if !errors.Is(err, context.Canceled) || verified {
		t.Fatalf("cancel during the content proof = (%v,%v), want canceled without publication", verified, err)
	}
	opts.Domain.release() // the chain pruning runs when the destination scope drops
	assertGone(t, dst+".part", dst+".part.meta", filepath.Join(root, "vendor", "model"), filepath.Join(root, "vendor"))
	assertRealDir(t, root)
}

// An ancestor that still holds any content survives and stops the chain, so
// empty ancestors above it survive too.
func TestCleanupPruneStopsAtAncestorWithContent(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "a", "b", "c", "file.bin")
	mustMkdirAll(t, filepath.Dir(dst))
	mustWriteFile(t, dst+".part", "partial")
	mustWriteFile(t, filepath.Join(root, "a", "b", "keep.txt"), "user content")

	if err := CleanupJobPartFiles(Settings{OutputDir: root, OwnershipDir: t.TempDir()}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	assertGone(t, filepath.Join(root, "a", "b", "c"))
	assertRealDir(t, filepath.Join(root, "a", "b"), filepath.Join(root, "a"))
}

// HF-cache mode removes the artifacts but never prunes the cache layout, even
// when the pruning would find completely empty directories.
func TestCleanupJobPartFilesNeverPrunesHFCacheLayout(t *testing.T) {
	cacheDir := t.TempDir()
	blobs := filepath.Join(cacheDir, "hub", "models--owner--repo", "blobs")
	mustMkdirAll(t, blobs)
	dst := filepath.Join(blobs, "tmp-shaA")
	mustWriteFile(t, dst+".part", "partial")
	mustWriteFile(t, dst+".parts.json", "partial")
	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	assertGone(t, dst+".part", dst+".parts.json")
	assertRealDir(t, blobs, filepath.Dir(blobs), filepath.Dir(filepath.Dir(blobs)))
}

// A live writer in the same directory is never disrupted: its writer scope
// refuses the pruning and its directory survives. This is the real refusal
// case for the pruning's "owned by another live writer" stop. The refusal
// leaves no sticky state: once the writer releases its scope, the same cleanup
// converges and the now-empty chain prunes away.
func TestEmptyChainPruneRefusesLiveWriterInChain(t *testing.T) {
	mustOwnershipLocks(t)
	root := t.TempDir()
	ownDir := t.TempDir()
	dir := filepath.Join(root, "vendor", "model")
	dst := filepath.Join(dir, "target.bin")
	mustMkdirAll(t, dir)
	mustWriteFile(t, dst+".part", "partial")
	realm, err := newOwnershipRealm(ownDir)
	if err != nil {
		t.Fatal(err)
	}
	// A live download writing into the same directory holds its writer scope
	// for the whole transfer.
	owner, err := realm.acquireWriter(context.Background(), []string{filepath.Join(dir, "sibling.bin")}, 0)
	if err != nil {
		t.Fatal(err)
	}

	cfg := Settings{OutputDir: root, OwnershipDir: ownDir}
	if err := CleanupJobPartFiles(cfg, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	assertGone(t, dst+".part")
	assertRealDir(t, dir, filepath.Join(root, "vendor"))
	owner.Release()
	// The refusal left no sticky state: with the writer's scope released, the
	// same cleanup converges — the now-empty chain prunes away.
	if err := CleanupJobPartFiles(cfg, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles after release: %v", err)
	}
	assertGone(t, dir, filepath.Join(root, "vendor"))
}

// A symlink or a non-directory in the chain stops it safely: nothing outside
// the output root's real tree is ever removed.
func TestEmptyChainPruneStopsAtSymlinkOrNonDirectory(t *testing.T) {
	t.Run("symlink in the chain stops it", func(t *testing.T) {
		root := t.TempDir()
		real := filepath.Join(root, "outer", "real")
		mustMkdirAll(t, filepath.Join(real, "sub"))
		if err := os.Symlink(real, filepath.Join(root, "link")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		dst := filepath.Join(root, "link", "sub", "file.bin")
		mustWriteFile(t, dst+".part", "partial")
		if err := CleanupJobPartFiles(Settings{OutputDir: root, OwnershipDir: t.TempDir()}, []string{dst}); err != nil {
			t.Fatalf("CleanupJobPartFiles: %v", err)
		}
		assertGone(t, filepath.Join(real, "sub"))
		if info, err := os.Lstat(filepath.Join(root, "link")); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("the chain's symlink was removed or replaced: %v", err)
		}
		// Never visited: the real directory behind the symlink and its real
		// ancestor survive untouched as real directories — a prune that follows
		// the link continues up the real chain and removes them.
		assertRealDir(t, filepath.Join(root, "outer"), real)
	})

	t.Run("pruning never follows a chain out of the root", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		sub := filepath.Join(outside, "sub")
		mustMkdirAll(t, sub)
		if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		pruneEmptyAncestors(filepath.Join(root, "link", "sub", "file.bin"), root, newFlatTestRealm(t))
		assertRealDir(t, sub)
	})

	t.Run("non-directory in the chain stops it", func(t *testing.T) {
		root := t.TempDir()
		mustMkdirAll(t, filepath.Join(root, "x"))
		blocker := filepath.Join(root, "x", "a")
		mustWriteFile(t, blocker, "file")
		pruneEmptyAncestors(filepath.Join(blocker, "b", "file.bin"), root, newFlatTestRealm(t))
		if _, err := os.Lstat(blocker); err != nil {
			t.Errorf("the non-directory in the chain was disturbed: %v", err)
		}
	})
}

// The ranged route's explicit-cancel cleanup schedules the same chain pruning.
func TestRangedCancelCleanupPrunesEmptyDirectoryChain(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "vendor", "file.bin")
	staging := dst + ".incomplete"
	mustMkdirAll(t, filepath.Dir(dst))
	mustWriteFile(t, staging, "staged")
	dom := acquireTestDomain(t, dst)
	if err := writeStagingMeta(staging+".meta", &IncompleteMeta{Instance: processInstanceToken, Version: stagingMetaVersion}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &stagingDownload{ctx: ctx, partCtx: ctx, dst: dst, stg: staging, meta: staging + ".meta", domain: dom,
		cfg: Settings{OutputDir: root, CleanupPartialsOnCancel: func() bool { return true }}}
	d.applyCancelCleanup()
	dom.release()
	assertGone(t, staging, staging+".meta", filepath.Join(root, "vendor"))
}

// A writer whose ownership path resolution races the pruning never surfaces a
// spurious failure. Each observed escape class is forced deterministically at
// its exact gap — the stat after symlink resolution ("canonical") and the
// identity capture after the existence stat ("identity") — through the
// download path's real domain acquisition: the vanished component resolves as
// missing and the writer's create-parent step recreates the chain.
func TestWriterResolutionVanishHealsWriter(t *testing.T) {
	mustOwnershipLocks(t)
	for _, gap := range []string{"canonical", "identity"} {
		t.Run(gap, func(t *testing.T) {
			root := t.TempDir()
			realm := newFlatTestRealm(t)
			dirs := filepath.Join(root, "vendor", "model")
			mustMkdirAll(t, dirs)
			dst := filepath.Join(dirs, "file.bin")
			vanishOnceAt(t, gap, dirs)
			dom, err := acquireBlobDomainScoped(context.Background(), flatDomainKey(dst), dst+".incomplete", realm, appendPartialOwnershipPaths(nil, dst), dst)
			if err != nil {
				t.Fatalf("writer surfaced the pruning race: %T: %v", err, err)
			}
			mustWriteFile(t, dst, "payload")
			assertRealDir(t, dirs)
			dom.release()
		})
	}
}

// A directory that vanishes between the pruning's emptiness check and its tree
// admission is already gone: the chain keeps walking upward instead of
// stopping at a "live writer" that does not exist.
func TestPruneContinuesWhenDirectoryVanishesBeforeAdmission(t *testing.T) {
	mustOwnershipLocks(t)
	root := t.TempDir()
	y := filepath.Join(root, "x", "y")
	mustMkdirAll(t, y)
	vanishOnceAt(t, "admit", y)
	pruneEmptyAncestors(filepath.Join(y, "file.bin"), root, newFlatTestRealm(t))
	assertGone(t, y, filepath.Join(root, "x"))
}

// The tree-removal limb of the shared vanish rule: one vanish event at the
// removal target, placed before resolution and inside the resolver's identity
// observation gap, must behave identically at the flat tree-removal consumer —
// the documented re-verified-vanish flow (tolerant removal granted) with the
// vanish re-verified while the creator-side keys are held, so a creator of the
// target is excluded until release. Without the fix the gap placement fails
// closed as an unclassified "ownership tree target … is missing" error and
// skips that flow.
func TestFlatTreeRemovalGapVanishMatchesPreResolutionVanish(t *testing.T) {
	mustOwnershipLocks(t)
	for _, c := range []struct{ name, gap string }{{"pre-resolution vanish", ""}, {"identity-gap vanish", "identity"}} {
		t.Run(c.name, func(t *testing.T) {
			tree := filepath.Join(t.TempDir(), "model")
			mustMkdirAll(t, tree)
			if c.gap == "" {
				os.RemoveAll(tree)
			} else {
				vanishOnceAt(t, c.gap, tree)
			}
			admission := &FlatAdmission{realm: newFlatTestRealm(t)}
			release, err := admission.AdmitTreeRemoval(context.Background(), tree)
			if err != nil {
				t.Fatalf("%s must take the documented re-verified-vanish flow: %T: %v", c.name, err, err)
			}
			if _, err := admission.AdmitFileWrites(context.Background(), []string{tree}); !errors.Is(err, ErrOwnershipBusy) {
				t.Errorf("%s: a creator of the vanished target must stay excluded until release: %v", c.name, err)
			}
			release()
		})
	}
}
