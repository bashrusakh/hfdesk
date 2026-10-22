// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCopiedHFRootRetainsLogicalOwnerWithNewPhysicalLease(t *testing.T) {
	ensureTestHFDeskBinary(t)
	base := t.TempDir()
	sourceRoot := filepath.Join(base, "source cache")
	copyRoot := filepath.Join(base, "copied cache")
	revisions := branchVariantTestRevisions()
	revisions["dev"] = branchVariantTestRevision{
		commit: "commit-z",
		files: map[string][]byte{
			"config.json":         []byte(`{"model":"ordinary-dev"}`),
			"weights.safetensors": []byte("ordinary-weights"),
		},
	}
	hub := newBranchVariantTestHub(t, revisions)
	defer hub.Close()
	settings := Settings{CacheDir: sourceRoot, Endpoint: hub.URL, Concurrency: 1, Verify: "sha256"}
	for _, job := range []Job{
		{Repo: "owner/model", Revision: "dev"},
		{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true},
	} {
		if err := Download(context.Background(), job, settings, nil); err != nil {
			t.Fatalf("download %s: %v", job.Revision, err)
		}
	}

	sourceService, err := BranchVariantServiceRoot(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	sourceServiceInfo, err := os.Stat(sourceService)
	if err != nil {
		t.Fatal(err)
	}
	ownerPath := filepath.Join(sourceService, branchVariantStoreOwnerFilename)
	ownerBefore, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewHFCache(sourceRoot, 0).WriteRebuildScript(); err != nil {
		t.Fatalf("write generated wrapper before relocating root: %v", err)
	}
	copyDirectoryAsBytes(t, sourceRoot, copyRoot)
	invalidCopy := filepath.Join(base, "incomplete copy")
	copyDirectoryAsBytes(t, sourceRoot, invalidCopy)
	invalidArtifact, err := branchVariantArtifactPaths(filepath.Join(invalidCopy, BranchVariantServiceDirName), "", RepoTypeModel, "owner/model", "4.00bpw", "commit-a")
	if err != nil {
		t.Fatal(err)
	}
	missingMember := filepath.Join(invalidArtifact.PayloadRoot, "weights.safetensors")
	if err := os.Remove(missingMember); err != nil {
		t.Fatalf("prepare incomplete copied-owner control: %v", err)
	}
	invalidOwnerPath := filepath.Join(invalidCopy, BranchVariantServiceDirName, branchVariantStoreOwnerFilename)
	invalidOwnerBefore, err := os.ReadFile(invalidOwnerPath)
	if err != nil {
		t.Fatal(err)
	}
	if owned, err := BranchVariantServiceOwned(invalidCopy); err == nil || owned {
		t.Fatalf("incomplete copied root was adopted: owned=%t err=%v", owned, err)
	}
	if got, err := os.ReadFile(invalidOwnerPath); err != nil || string(got) != string(invalidOwnerBefore) {
		t.Fatalf("incomplete copy rejection changed its owner marker: %q err=%v", got, err)
	}
	if _, err := os.Stat(missingMember); !os.IsNotExist(err) {
		t.Fatalf("incomplete copy rejection repaired or replaced missing payload: %v", err)
	}
	copiedService, err := BranchVariantServiceRoot(copyRoot)
	if err != nil {
		t.Fatal(err)
	}
	copiedServiceInfo, err := os.Stat(copiedService)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(sourceServiceInfo, copiedServiceInfo) {
		t.Fatal("copy fixture retained the source service-root inode; it does not exercise a new physical root")
	}
	sourceLeasePath := NewHFCache(sourceRoot, 0).RebuildCoordinationPath()
	copiedLeasePath := NewHFCache(copyRoot, 0).RebuildCoordinationPath()
	if sourceLeasePath == copiedLeasePath {
		t.Fatalf("physical copies resolved to one runtime lease file: %q", sourceLeasePath)
	}
	sourceRelease, err := AcquireRebuildCoordinationLeases([]string{sourceRoot}, true)
	if err != nil {
		t.Fatalf("hold source-root lease: %v", err)
	}
	copyLeaseCtx, cancelCopyLease := context.WithTimeout(context.Background(), time.Second)
	copyRelease, err := acquireRebuildCoordinationLeases(copyLeaseCtx, []string{copyRoot}, false, nil)
	cancelCopyLease()
	if err != nil {
		sourceRelease()
		t.Fatalf("independent physical copy waited on source-root lease: %v", err)
	}
	copyRelease()
	sourceRelease()
	if err := os.RemoveAll(sourceRoot); err != nil {
		t.Fatalf("remove original root before copy use: %v", err)
	}

	owned, err := BranchVariantServiceOwned(copyRoot)
	if err != nil || !owned {
		t.Fatalf("copied logical owner owned=%t err=%v; want recognized without source", owned, err)
	}
	ownerReadOnly, err := os.ReadFile(filepath.Join(copiedService, branchVariantStoreOwnerFilename))
	if err != nil || string(ownerReadOnly) != string(ownerBefore) {
		t.Fatalf("read-only copy recognition rewrote owner metadata: got=%q err=%v", ownerReadOnly, err)
	}
	cache := NewHFCache(copyRoot, 0)
	if _, err := cache.Sync(SyncOptions{}); err != nil {
		t.Fatalf("Sync copied root after source removal: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(copyRoot, "models", "owner", "model", "config.json")); err != nil || string(got) != `{"model":"ordinary-dev"}` {
		t.Fatalf("ordinary view after copy Sync=%q err=%v", got, err)
	}
	artifact, err := ReadBranchVariantArtifact(copyRoot, RepoTypeModel, "owner/model", "4.00bpw", "commit-a")
	if err != nil || artifact == nil {
		t.Fatalf("copied branch artifact=%+v err=%v", artifact, err)
	}
	if got, err := os.ReadFile(filepath.Join(artifact.PayloadRoot, "config.json")); err != nil || string(got) != `{"model":"A"}` {
		t.Fatalf("copied branch payload=%q err=%v", got, err)
	}
	ownerAfter, err := os.ReadFile(filepath.Join(copiedService, branchVariantStoreOwnerFilename))
	if err != nil {
		t.Fatal(err)
	}
	var ownerBeforeRecord, ownerAfterRecord branchVariantStoreOwner
	if err := json.Unmarshal(ownerBefore, &ownerBeforeRecord); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ownerAfter, &ownerAfterRecord); err != nil {
		t.Fatal(err)
	}
	currentServiceID, err := branchVariantFilesystemIdentity(copiedService)
	if err != nil {
		t.Fatal(err)
	}
	if ownerAfterRecord.StoreID != ownerBeforeRecord.StoreID || ownerAfterRecord.FileID != currentServiceID || ownerAfterRecord.FileID == ownerBeforeRecord.FileID {
		t.Fatalf("Sync did not refresh only the copied root's physical binding: before=%+v after=%+v current=%q", ownerBeforeRecord, ownerAfterRecord, currentServiceID)
	}

	if runtime.GOOS == "windows" {
		registerProfileCoordinationCleanup(t, copyRoot)
		command := exec.Command(os.Getenv("HFDESK_BIN"), "--rebuild", "--cache-dir="+copyRoot, "--hub-dir="+filepath.Join(copyRoot, "hub"))
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("CLI rebuild copied root: %v: %s", err, output)
		}
	} else {
		if output, err := runGeneratedRebuildScript(t, filepath.Join(copyRoot, "rebuild.sh")); err != nil {
			t.Fatalf("wrapper rebuild copied root: %v: %s", err, output)
		}
	}
	owned, err = BranchVariantServiceOwned(copyRoot)
	if err != nil || !owned {
		t.Fatalf("copied owner after CLI rebuild owned=%t err=%v", owned, err)
	}
	if got, err := os.ReadFile(filepath.Join(copyRoot, "models", "owner", "model", "config.json")); err != nil || string(got) != `{"model":"ordinary-dev"}` {
		t.Fatalf("ordinary view after CLI rebuild=%q err=%v", got, err)
	}
	artifact, err = ReadBranchVariantArtifact(copyRoot, RepoTypeModel, "owner/model", "4.00bpw", "commit-a")
	if err != nil || artifact == nil {
		t.Fatalf("branch artifact after CLI rebuild=%+v err=%v", artifact, err)
	}
	for name, want := range branchVariantTestRevisions()["4.00bpw"].files {
		if got, err := os.ReadFile(filepath.Join(artifact.PayloadRoot, name)); err != nil || string(got) != string(want) {
			t.Errorf("branch payload %q after CLI rebuild=%q err=%v; want %q", name, got, err, want)
		}
	}
}

func copyDirectoryAsBytes(t *testing.T, source, destination string) {
	t.Helper()
	rootInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destination, rootInfo.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(destination, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected non-regular cache entry: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if mode == 0 {
			mode = 0o600
		}
		return os.WriteFile(target, data, mode)
	})
	if err != nil {
		t.Fatal(err)
	}
}
