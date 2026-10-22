// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/runtimecoordination"
)

// TestCoordinationCarrierKeepsASharedAccountParentUsable pins the placement
// and the creation mechanics of the per-account coordination carrier inside a
// shared account base directory.
//
// A container gives every account the same home - the shared data root - and
// the cache of every account lives below it. The carrier therefore adds
// exactly one private directory below that shared parent: it must not create
// a directory of its own inside the parent, and it must not change the mode
// or the owner of the parent. A shared parent that the first account
// tightened takes the other accounts out of their own cache, and the second
// account cannot create a carrier of its own there.
func TestCoordinationCarrierKeepsASharedAccountParentUsable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shared parent mode is a Unix property")
	}
	shared := t.TempDir()
	// The container data root is world-writable and sticky, so every account
	// of the machine can create its own state inside it.
	if err := os.Chmod(shared, 0o1777); err != nil {
		t.Fatal(err)
	}
	restore := runtimecoordination.SetBaseDirProvider(nil)
	defer restore()

	withCoordinationAccount(t, coordinationAccount{ID: "uid:4242", Home: shared}, nil)
	first, err := defaultCoordinationBaseDir()
	if err != nil {
		t.Fatalf("defaultCoordinationBaseDir: %v", err)
	}
	if filepath.Dir(first) != shared {
		t.Fatalf("the carrier base %q is not a direct child of the account's own base directory %q", first, shared)
	}
	if _, err := ensurePrivateCoordinationDirectory(context.Background()); err != nil {
		t.Fatalf("establish the coordination carrier: %v", err)
	}
	assertSharedAccountParentUsable(t, shared)
	// The carrier adds exactly one directory to the shared parent. A
	// directory of its own inside the parent would be a shared directory the
	// first account created with a private mode, and the second account
	// could neither use it nor create its own carrier beside it.
	entries, err := os.ReadDir(shared)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(first) {
		t.Fatalf("the carrier created %v inside the shared parent %q; want only %q", entryNames(entries), shared, filepath.Base(first))
	}
	assertPrivateCoordinationCarrier(t, first)

	// A second account of the same machine resolves a carrier of its own in
	// the same shared parent and leaves that parent usable for both.
	withCoordinationAccount(t, coordinationAccount{ID: "uid:4243", Home: shared}, nil)
	second, err := defaultCoordinationBaseDir()
	if err != nil {
		t.Fatalf("defaultCoordinationBaseDir: %v", err)
	}
	if second == first {
		t.Fatalf("two accounts share one coordination carrier: %q", first)
	}
	if filepath.Dir(second) != shared {
		t.Fatalf("the second carrier base %q is not a direct child of the shared parent %q", second, shared)
	}
	if _, err := ensurePrivateCoordinationDirectory(context.Background()); err != nil {
		t.Fatalf("establish the second coordination carrier: %v", err)
	}
	assertSharedAccountParentUsable(t, shared)
	assertPrivateCoordinationCarrier(t, second)
}

// assertSharedAccountParentUsable pins that the shared account base directory
// still has the mode it had before the carrier was created, and that it is
// still reachable for the other accounts of the machine.
func assertSharedAccountParentUsable(t *testing.T, shared string) {
	t.Helper()
	info, err := os.Lstat(shared)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o777 {
		t.Fatalf("the shared account parent %q has mode %o; want 777", shared, info.Mode().Perm())
	}
	if info.Mode().Perm()&0o002 == 0 {
		t.Fatalf("the shared account parent %q is not writable by the other accounts of the machine", shared)
	}
}

// TestCreatePrivateCoordinationBaseLeavesTheParentAlone pins the creation
// mechanics of the carrier directory itself.
//
// The carrier is private to the account that created it, and the directory
// that holds it keeps its own mode and its own owner. A parent that does not
// exist yet is created with the ordinary directory mode, so the other
// accounts of the machine can use it too.
func TestCreatePrivateCoordinationBaseLeavesTheParentAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shared parent mode is a Unix property")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o1777); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(parent, "hfdesk-coord-first")
	if err := createPrivateCoordinationBase(base); err != nil {
		t.Fatalf("create the carrier base: %v", err)
	}
	assertSharedAccountParentUsable(t, parent)
	baseInfo, err := os.Lstat(base)
	if err != nil {
		t.Fatal(err)
	}
	if baseInfo.Mode().Perm() != 0o700 {
		t.Fatalf("the carrier base %q has mode %o; want 700", base, baseInfo.Mode().Perm())
	}
	// A second account of the same machine creates its own carrier in the
	// same parent and leaves that parent usable for both.
	other := filepath.Join(parent, "hfdesk-coord-second")
	if err := createPrivateCoordinationBase(other); err != nil {
		t.Fatalf("create the second carrier base: %v", err)
	}
	assertSharedAccountParentUsable(t, parent)
	otherInfo, err := os.Lstat(other)
	if err != nil {
		t.Fatal(err)
	}
	if otherInfo.Mode().Perm() != 0o700 {
		t.Fatalf("the second carrier base %q has mode %o; want 700", other, otherInfo.Mode().Perm())
	}

	// A parent that does not exist yet is created with the ordinary
	// directory mode: it stays usable for the other accounts.
	missing := filepath.Join(t.TempDir(), "data-root")
	created := filepath.Join(missing, "hfdesk-coord-first")
	if err := createPrivateCoordinationBase(created); err != nil {
		t.Fatalf("create the carrier base below a missing parent: %v", err)
	}
	missingInfo, err := os.Lstat(missing)
	if err != nil {
		t.Fatal(err)
	}
	if missingInfo.Mode().Perm()&0o007 == 0 {
		t.Fatalf("the created parent %q has mode %o; it must stay usable for the other accounts of the machine", missing, missingInfo.Mode().Perm())
	}
}

// assertPrivateCoordinationCarrier pins that the carrier is a private
// directory of the account that created it.
func assertPrivateCoordinationCarrier(t *testing.T, base string) {
	t.Helper()
	info, err := os.Lstat(base)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the carrier base %q is not a real directory", base)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("the carrier base %q has mode %o; want 700", base, info.Mode().Perm())
	}
	coordination := filepath.Join(base, coordinationDirectoryName)
	coordinationInfo, err := os.Lstat(coordination)
	if err != nil {
		t.Fatal(err)
	}
	if !coordinationInfo.IsDir() || coordinationInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the coordination directory %q is not a real directory", coordination)
	}
	if coordinationInfo.Mode().Perm() != 0o700 {
		t.Fatalf("the coordination directory %q has mode %o; want 700", coordination, coordinationInfo.Mode().Perm())
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
