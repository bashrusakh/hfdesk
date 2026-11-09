// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func newTestOwnershipRealm(t *testing.T) (*ownershipRealm, string) {
	t.Helper()
	base := t.TempDir()
	data := filepath.Join(base, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	realm, err := newOwnershipRealm(filepath.Join(base, "ownership"))
	if err != nil {
		t.Fatal(err)
	}
	return realm, data
}

func TestOwnershipRealmScopesAreSharedAndIndependent(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no cross-process ownership primitive")
	}
	realm, data := newTestOwnershipRealm(t)
	aDir := filepath.Join(data, "repo-a")
	cDir := filepath.Join(data, "repo-c")
	if err := os.MkdirAll(filepath.Join(aDir, "same"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	a, err := realm.acquireWriter(ctx, []string{filepath.Join(aDir, "same", "a.bin")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := realm.acquireWriter(ctx, []string{filepath.Join(aDir, "same", "b.bin")}, 0)
	if err != nil {
		t.Fatalf("independent sibling writer blocked: %v", err)
	}
	defer b.Release()
	foo, err := realm.acquireWriter(ctx, []string{filepath.Join(aDir, "same", "foo")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer foo.Release()
	lockShapedPayload, err := realm.acquireWriter(ctx, []string{filepath.Join(aDir, "same", "foo.incomplete.lock")}, 0)
	if err != nil {
		t.Fatalf("ordinary suffix-shaped payload was coupled to foo: %v", err)
	}
	defer lockShapedPayload.Release()

	cDelete, err := realm.acquireTreeRemoval(ctx, cDir, 0)
	if err != nil {
		t.Fatalf("disjoint repo deletion blocked: %v", err)
	}
	cDelete.Release()
	if _, err := realm.acquireTreeRemoval(ctx, aDir, 0); !errors.Is(err, errOwnershipBusy) {
		t.Fatalf("deleting the writers' tree = %v, want busy", err)
	}

	created := filepath.Join(aDir, "created")
	prep, err := realm.prepareWriter(ctx, []string{filepath.Join(created, "first.bin")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := prep.CreateParents(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := prep.Promote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := realm.acquireWriter(ctx, []string{filepath.Join(created, "second.bin")}, 0)
	if err != nil {
		t.Fatalf("preparation gate remained exclusive for sibling transfer: %v", err)
	}
	second.Release()
}

func TestOwnershipTreeAdmissionStableUntilRelease(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no cross-process ownership primitive")
	}
	realm, data := newTestOwnershipRealm(t)
	tree := filepath.Join(data, "repo")
	file := filepath.Join(tree, "sub", "payload")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("preserve"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	writer, err := realm.acquireWriter(ctx, []string{file}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := realm.acquireTreeRemoval(ctx, tree, 0); !errors.Is(err, errOwnershipBusy) {
		writer.Release()
		t.Fatalf("tree removal with a live descendant = %v, want busy", err)
	}
	got, err := os.ReadFile(file)
	if err != nil || string(got) != "preserve" {
		writer.Release()
		t.Fatalf("failed admission changed payload: %q, %v", got, err)
	}
	writer.Release()

	deleting, err := realm.acquireTreeRemoval(ctx, tree, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := deleting.Validate(); err != nil {
		t.Fatalf("successful tree admission failed revalidation: %v", err)
	}
	if _, err := realm.acquireWriter(ctx, []string{file}, 0); !errors.Is(err, errOwnershipBusy) {
		deleting.Release()
		t.Fatalf("writer entered while tree lease was held: %v", err)
	}
	deleting.Release()
	writer, err = realm.acquireWriter(ctx, []string{file}, 0)
	if err != nil {
		t.Fatalf("writer did not resume after tree lease release: %v", err)
	}
	writer.Release()
}

func TestOwnershipSetAcquisitionRollsBackAndHonorsCancellation(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no cross-process ownership primitive")
	}
	realm, _ := newTestOwnershipRealm(t)
	ctx := context.Background()
	holder, err := realm.acquire(ctx, []ownershipLockRequest{{key: "z-held", mode: ownershipExclusive}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	requests := []ownershipLockRequest{
		{key: "a-partial", mode: ownershipExclusive},
		{key: "z-held", mode: ownershipExclusive},
	}
	if _, err := realm.acquire(ctx, requests, 0); !errors.Is(err, errOwnershipBusy) {
		t.Fatalf("conflicting set acquisition = %v, want busy", err)
	}
	partial, err := realm.acquire(ctx, []ownershipLockRequest{{key: "a-partial", mode: ownershipExclusive}}, 0)
	if err != nil {
		t.Fatalf("failed set acquisition leaked its partial lock: %v", err)
	}
	partial.Release()

	deadline, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	if _, err := realm.acquire(deadline, []ownershipLockRequest{{key: "z-held", mode: ownershipExclusive}}, time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellable wait = %v, want context deadline", err)
	}
}

func TestOwnershipPreparationPromotionFailureReleasesCreationGates(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no cross-process ownership primitive")
	}
	realm, data := newTestOwnershipRealm(t)
	path := filepath.Join(data, "new", "deep", "payload")
	before, err := realm.resolveOwnershipPath(path, false)
	if err != nil {
		t.Fatal(err)
	}
	var creationKey string
	for _, dir := range before.directories {
		if dir.objectKey == "" {
			creationKey = dir.entryKey
			break
		}
	}
	if creationKey == "" {
		t.Fatal("fixture did not contain a missing directory")
	}
	prep, err := realm.prepareWriter(context.Background(), []string{path}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := prep.CreateParents(context.Background()); err != nil {
		t.Fatal(err)
	}
	resolved, err := realm.resolveOwnershipPath(path, false)
	if err != nil {
		prep.Release()
		t.Fatal(err)
	}
	blocker, err := realm.acquire(context.Background(), []ownershipLockRequest{{key: resolved.itemKeys[1], mode: ownershipExclusive}}, 0)
	if err != nil {
		prep.Release()
		t.Fatal(err)
	}
	if _, err := prep.Promote(context.Background()); !errors.Is(err, errOwnershipBusy) {
		blocker.Release()
		t.Fatalf("promotion with a blocked final item key = %v, want busy", err)
	}
	blocker.Release()
	creation, err := realm.acquire(context.Background(), []ownershipLockRequest{{key: creationKey, mode: ownershipExclusive}}, 0)
	if err != nil {
		t.Fatalf("failed promotion leaked its temporary creation gate: %v", err)
	}
	creation.Release()
}

func TestOwnershipLockIsReleasedWhenProcessExits(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no cross-process ownership primitive")
	}
	if os.Getenv("HFDESK_OWNERSHIP_HELPER") == "1" {
		runOwnershipLockHelper(t)
		return
	}
	realm, _ := newTestOwnershipRealm(t)
	base := t.TempDir()
	ready := filepath.Join(base, "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestOwnershipLockIsReleasedWhenProcessExits$")
	cmd.Env = append(os.Environ(),
		"HFDESK_OWNERSHIP_HELPER=1",
		"HFDESK_OWNERSHIP_REALM="+realm.root,
		"HFDESK_OWNERSHIP_READY="+ready,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	waitOwnershipFile(t, ready)
	if _, err := realm.acquire(context.Background(), []ownershipLockRequest{{key: "process-owner", mode: ownershipExclusive}}, 0); !errors.Is(err, errOwnershipBusy) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("live process lock = %v, want busy", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	lease, err := realm.acquire(context.Background(), []ownershipLockRequest{{key: "process-owner", mode: ownershipExclusive}}, 0)
	if err != nil {
		t.Fatalf("kernel ownership survived process exit: %v", err)
	}
	lease.Release()
}

func runOwnershipLockHelper(t *testing.T) {
	realm, err := newOwnershipRealm(os.Getenv("HFDESK_OWNERSHIP_REALM"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := realm.acquire(context.Background(), []ownershipLockRequest{{key: "process-owner", mode: ownershipExclusive}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := os.WriteFile(os.Getenv("HFDESK_OWNERSHIP_READY"), []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func waitOwnershipFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ownership helper did not acquire its lock")
}

func TestOwnershipLockFilesSurvivePayloadDeletionAndReusePathKey(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no cross-process ownership primitive")
	}
	realm, data := newTestOwnershipRealm(t)
	dir := filepath.Join(data, "repo")
	path := filepath.Join(dir, "item")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lease, err := realm.acquireWriter(context.Background(), []string{path}, 0)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := realm.resolveOwnershipPath(path, false)
	if err != nil {
		lease.Release()
		t.Fatal(err)
	}
	lockPath := realm.lockPath(resolved.itemKeys[0])
	before, err := os.Stat(lockPath)
	if err != nil {
		lease.Release()
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		lease.Release()
		t.Fatal(err)
	}
	after, err := os.Stat(lockPath)
	if err != nil || !os.SameFile(before, after) {
		lease.Release()
		t.Fatalf("external lock did not survive payload deletion: stat=%v, err=%v", after, err)
	}
	if _, err := realm.acquireWriter(context.Background(), []string{path}, 0); !errors.Is(err, errOwnershipBusy) {
		lease.Release()
		t.Fatalf("delete/recreate path split a live item key: %v", err)
	}
	lease.Release()
	lease, err = realm.acquireWriter(context.Background(), []string{path}, 0)
	if err != nil {
		t.Fatalf("same path key did not become available after release: %v", err)
	}
	lease.Release()
}

func TestOwnershipTreeAdmissionRejectsRealmDeletion(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no cross-process ownership primitive")
	}
	realm, data := newTestOwnershipRealm(t)
	base := filepath.Dir(data)
	lease, err := realm.acquire(context.Background(), []ownershipLockRequest{{key: "realm-survives", mode: ownershipExclusive}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	lockPath := realm.lockPath("realm-survives")
	before, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := realm.acquireTreeRemoval(context.Background(), base, 0); err == nil {
		t.Fatal("tree admission allowed a scope containing the external ownership realm")
	}
	after, err := os.Stat(lockPath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("rejected tree admission changed the realm lock: %v", err)
	}
}
