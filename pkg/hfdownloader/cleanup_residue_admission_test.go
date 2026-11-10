// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCleanupResidueFamilyRefusedUnderLiveEntryAdmission pins the admission
// covering for the obsolete pre-#140 <dst>.part-<digits> family. The
// destination scope does not cover those keys, so CleanupJobPartFiles may
// unlink a residue file only under that exact path's entry admission in the
// destination realm. Cache mode is used because it is the default mode and
// the realm identity is the Hub-derived control realm a competing HFDesk
// instance resolves too: the holder takes that admission exactly as a
// competitor would (OpenCacheAdmission + AdmitFileWrite on the same Hub
// root), while the destination domain itself stays free — the residue entry,
// not the domain, is what is held.
//
// Before the admission fix the residue file was unlinked with a bare
// os.Remove outside every scope, so the first survival assertion failed
// while the cleanup still reported success.
func TestCleanupResidueFamilyRefusedUnderLiveEntryAdmission(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	cacheDir := t.TempDir()
	hubDir := filepath.Join(cacheDir, "hub")
	blobsDir := filepath.Join(hubDir, "models--owner--model", "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(blobsDir, "tmp-residueholder")
	heldResidue := dst + ".part-00"
	freeResidue := dst + ".part-01"
	layout := dst + ".parts.json"
	for path, body := range map[string]string{
		heldResidue: "another writer's residue",
		freeResidue: "unowned residue",
		layout:      `{"size":2}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	admission, err := OpenCacheAdmission(context.Background(), hubDir)
	if err != nil {
		t.Fatalf("open cache admission: %v", err)
	}
	release, err := admission.AdmitFileWrite(context.Background(), heldResidue)
	if err != nil {
		t.Fatalf("hold the residue entry admission: %v", err)
	}

	// The surrounding operation keeps its outcome: the destination domain is
	// free, so cleanup succeeds even though one residue admission is refused.
	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles with a refused residue admission: %v", err)
	}
	if _, err := os.Stat(heldResidue); err != nil {
		t.Errorf("the live holder's residue file was removed despite the refused admission: %v", err)
	}
	// Per-file granularity: an unadmitted sibling residue and the
	// scope-covered layout record are still removed by the same cleanup.
	if _, err := os.Stat(freeResidue); !os.IsNotExist(err) {
		t.Errorf("unowned residue survived cleanup, stat err=%v", err)
	}
	if _, err := os.Stat(layout); !os.IsNotExist(err) {
		t.Errorf(".parts.json survived cleanup although the destination scope covers it, stat err=%v", err)
	}

	// Complementary case: with the holder gone the residue file is removed,
	// the pre-existing oracle behavior.
	release()
	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles after the admission was released: %v", err)
	}
	if _, err := os.Stat(heldResidue); !os.IsNotExist(err) {
		t.Errorf("residue file survived a free-admission cleanup, stat err=%v", err)
	}
}
