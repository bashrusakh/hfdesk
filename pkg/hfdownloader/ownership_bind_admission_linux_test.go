// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hfdownloader

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOwnershipTreeAdmissionBindView(t *testing.T) {
	if os.Getenv("HFDESK_OWNERSHIP_BIND_CHILD") == "1" {
		runOwnershipBindAdmissionChild(t)
		return
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skip("unshare is unavailable; bind-view admission probe skipped")
	}
	base := t.TempDir()
	a := filepath.Join(base, "tree-a")
	b := filepath.Join(a, "B")
	view := filepath.Join(base, "outside-view")
	viewRoot := filepath.Join(base, "outside-tree-view")
	data := filepath.Join(b, "payload")
	for _, dir := range []string{b, view, viewRoot, filepath.Join(base, "ownership")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(data, []byte("bind-preserved"), 0o644); err != nil {
		t.Fatal(err)
	}
	skipFile := filepath.Join(base, "bind-skip")
	cmd := exec.Command(unshare, "-Ur", "-m", os.Args[0], "-test.run=^TestOwnershipTreeAdmissionBindView$")
	cmd.Env = append(os.Environ(),
		"HFDESK_OWNERSHIP_BIND_CHILD=1",
		"HFDESK_OWNERSHIP_BIND_ROOT="+base,
		"HFDESK_OWNERSHIP_BIND_SKIP="+skipFile,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		lower := strings.ToLower(string(output))
		// Skip only environment capability denial: the runner refusing
		// namespace creation or the uid_map/gid_map write (EPERM/EACCES/EINVAL).
		// Once the child test itself has run, its failure is a genuine mechanism
		// breakage and must fail, whatever the message says.
		capabilityDenied := (strings.Contains(lower, "operation not permitted") ||
			strings.Contains(lower, "permission denied") ||
			strings.Contains(lower, "invalid argument")) &&
			(strings.Contains(lower, "unshare") ||
				strings.Contains(lower, "uid_map") ||
				strings.Contains(lower, "gid_map"))
		childRan := strings.Contains(lower, "--- fail") ||
			strings.Contains(lower, "=== run") ||
			strings.Contains(lower, "panic")
		if capabilityDenied && !childRan {
			t.Skipf("namespace setup is denied by this environment: %s", strings.TrimSpace(string(output)))
		}
		t.Fatalf("bind-view child failed: %v\n%s", err, output)
	}
	if reason, err := os.ReadFile(skipFile); err == nil {
		t.Skip(strings.TrimSpace(string(reason)))
	}
}

func runOwnershipBindAdmissionChild(t *testing.T) {
	base := os.Getenv("HFDESK_OWNERSHIP_BIND_ROOT")
	a := filepath.Join(base, "tree-a")
	b := filepath.Join(a, "B")
	view := filepath.Join(base, "outside-view")
	viewRoot := filepath.Join(base, "outside-tree-view")
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		ownershipBindSkip(t, err)
	}
	if err := unix.Mount(b, view, "", unix.MS_BIND, ""); err != nil {
		ownershipBindSkip(t, err)
	}
	defer unix.Unmount(view, 0)
	if err := unix.Mount(a, viewRoot, "", unix.MS_BIND, ""); err != nil {
		ownershipBindSkip(t, err)
	}
	defer unix.Unmount(viewRoot, 0)

	directInfo, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	aliased, err := os.Stat(view)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(directInfo, aliased) {
		t.Fatal("bind view did not resolve to the visited directory object")
	}

	realm, err := newOwnershipRealm(filepath.Join(base, "ownership"))
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(view, "payload")
	writer, err := realm.acquireWriter(context.Background(), []string{payload}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Release()

	if _, err := realm.acquireTreeRemoval(context.Background(), a, 0); !errors.Is(err, errOwnershipBusy) {
		t.Fatalf("recursive admission through tree A = %v, want B's live alias to block", err)
	}
	data := filepath.Join(b, "payload")
	got, err := os.ReadFile(data)
	if err != nil || string(got) != "bind-preserved" {
		t.Fatalf("failed admission touched B payload: %q, %v", got, err)
	}

	resolvedA, err := realm.resolveOwnershipPath(a, true)
	if err != nil {
		t.Fatal(err)
	}
	rootOnly, err := realm.acquire(context.Background(), ownershipDirectoryRequests(resolvedA.directories[len(resolvedA.directories)-1], ownershipExclusive), 0)
	if err != nil {
		t.Fatalf("root-only EX should not conflict with the outside B view: %v", err)
	}
	rootOnly.Release()

	writer.Release()
	tree, err := realm.acquireTreeRemoval(context.Background(), a, 0)
	if err != nil {
		t.Fatalf("tree admission did not succeed after B writer released: %v", err)
	}
	if err := tree.Validate(); err != nil {
		t.Fatalf("tree admission did not remain valid: %v", err)
	}
	if _, err := realm.acquireWriter(context.Background(), []string{payload}, 0); !errors.Is(err, errOwnershipBusy) {
		tree.Release()
		t.Fatalf("outside-view writer entered while tree lease was held: %v", err)
	}
	tree.Release()

	// A acquires while both parent directories are missing. Once those
	// directories appear through the direct path, the bind spelling must still
	// contend before A promotes its preparation into a writer lease.
	direct := filepath.Join(a, "new", "deep", "payload")
	viaView := filepath.Join(viewRoot, "new", "deep", "payload")
	preparation, err := realm.prepareWriter(context.Background(), []string{direct}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := preparation.CreateParents(context.Background()); err != nil {
		t.Fatal(err)
	}
	directParent, err := os.Stat(filepath.Dir(direct))
	if err != nil {
		preparation.Release()
		t.Fatal(err)
	}
	viewParent, err := os.Stat(filepath.Dir(viaView))
	if err != nil || !os.SameFile(directParent, viewParent) {
		preparation.Release()
		t.Fatalf("created parents through direct/view paths differ: %v", err)
	}
	otherPreparation, writerErr := realm.prepareWriter(context.Background(), []string{viaView}, 0)
	if otherPreparation != nil {
		otherPreparation.Release()
	}
	if !errors.Is(writerErr, errOwnershipBusy) {
		t.Errorf("writer via existing bind parents = %v, want contention before A's identity refresh", writerErr)
	}
	deleteLease, deleteErr := realm.acquireTreeRemoval(context.Background(), filepath.Dir(viaView), 0)
	if deleteLease != nil {
		deleteLease.Release()
	}
	if !errors.Is(deleteErr, errOwnershipBusy) {
		t.Errorf("delete via existing bind parents = %v, want contention before removal", deleteErr)
	}
	writerA, err := preparation.Promote(context.Background())
	if err != nil {
		t.Fatalf("prepared writer did not promote after directory creation: %v", err)
	}
	if _, err := realm.acquireWriter(context.Background(), []string{viaView}, 0); !errors.Is(err, errOwnershipBusy) {
		writerA.Release()
		t.Fatalf("writer through alias entered after promotion: %v", err)
	}
	if _, err := realm.acquireTreeRemoval(context.Background(), filepath.Dir(viaView), 0); !errors.Is(err, errOwnershipBusy) {
		writerA.Release()
		t.Fatalf("delete through alias entered after promotion: %v", err)
	}
	writerA.Release()
	writerB, err := realm.acquireWriter(context.Background(), []string{viaView}, 0)
	if err != nil {
		t.Fatalf("alias writer did not proceed after promotion lease release: %v", err)
	}
	writerB.Release()
}

func ownershipBindSkip(t *testing.T, cause error) {
	t.Helper()
	_ = os.WriteFile(os.Getenv("HFDESK_OWNERSHIP_BIND_SKIP"), []byte(cause.Error()), 0o600)
	t.Skipf("private bind mount unavailable: %v", cause)
}
