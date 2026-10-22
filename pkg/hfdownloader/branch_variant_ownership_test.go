// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOrdinaryDownloadsIgnoreUnmarkedServiceCandidateRoot covers a directory
// that only shares the service component name and carries no application
// ownership evidence. It stays ordinary data: unrelated ordinary downloads
// into the same root keep working, no ownership record is introduced, and a
// direct EXL claim at that exact root is still refused before any change.
func TestOrdinaryDownloadsIgnoreUnmarkedServiceCandidateRoot(t *testing.T) {
	ordinaryHub := newBranchVariantTestHubForRepo(t, "other/model", map[string]branchVariantTestRevision{
		"main": {commit: "ordinary-commit", files: map[string][]byte{"config.json": []byte("unrelated ordinary file")}},
	}, nil)
	defer ordinaryHub.Close()
	branchHub := newBranchVariantTestHub(t, map[string]branchVariantTestRevision{
		"4.00bpw": branchVariantTestRevisions()["4.00bpw"],
	})
	defer branchHub.Close()

	for _, mode := range []string{"hf-cache", "local"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			serviceRoot, err := BranchVariantServiceRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(serviceRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			foreign := filepath.Join(serviceRoot, "foreign.txt")
			foreignBytes := []byte("keep-unowned")
			if err := os.WriteFile(foreign, foreignBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			ownerPath := filepath.Join(serviceRoot, branchVariantStoreOwnerFilename)

			settings := Settings{Endpoint: ordinaryHub.URL, Concurrency: 1, Verify: "size"}
			ordinaryPath := filepath.Join(root, "models", "other", "model", "config.json")
			if mode == "hf-cache" {
				settings.CacheDir = root
			} else {
				settings.OutputDir = root
				ordinaryPath = filepath.Join(root, "other", "model", "config.json")
			}
			if err := Download(context.Background(), Job{Repo: "other/model", Revision: "main"}, settings, nil); err != nil {
				t.Fatalf("unrelated ordinary download was blocked by an unmarked service candidate: %v", err)
			}
			if got, err := os.ReadFile(ordinaryPath); err != nil || string(got) != "unrelated ordinary file" {
				t.Fatalf("ordinary download output=%q err=%v", got, err)
			}
			if got, err := os.ReadFile(foreign); err != nil || !bytes.Equal(got, foreignBytes) {
				t.Fatalf("ordinary download changed the unmarked service candidate: %q err=%v", got, err)
			}
			if _, err := os.Lstat(ownerPath); !os.IsNotExist(err) {
				t.Fatalf("ordinary download installed an ownership record: %v", err)
			}

			claimSettings := Settings{Endpoint: branchHub.URL, Concurrency: 1, Verify: "sha256"}
			if mode == "hf-cache" {
				claimSettings.CacheDir = root
			} else {
				claimSettings.OutputDir = root
			}
			err = Download(context.Background(), Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, claimSettings, nil)
			if err == nil || !strings.Contains(err.Error(), "unowned branch variant service tree") {
				t.Fatalf("EXL claim error=%v; want fail-before-change on the unmarked service candidate", err)
			}
			if got, err := os.ReadFile(foreign); err != nil || !bytes.Equal(got, foreignBytes) {
				t.Fatalf("refused EXL claim changed the unmarked service candidate: %q err=%v", got, err)
			}
			if _, err := os.Lstat(ownerPath); !os.IsNotExist(err) {
				t.Fatalf("refused EXL claim installed an ownership record: %v", err)
			}
		})
	}
}

// TestCopiedLocalRootKeepsCompleteArtifactWithIncompleteSibling covers copy
// recognition per artifact. A complete, self-consistent artifact keeps its
// identity regardless of its siblings, so one artifact left incomplete by a
// failed trusted-hash retry no longer declassifies the whole copied store:
// the store stays owned, its complete artifact stays readable, and ordinary
// writes into the service tree remain refused.
func TestCopiedLocalRootKeepsCompleteArtifactWithIncompleteSibling(t *testing.T) {
	base := t.TempDir()
	sourceRoot := filepath.Join(base, "source-local")
	copiedRoot := filepath.Join(base, "copied-local")
	completeFile := "weights.safetensors"
	completeBytes := []byte("AAAAAAAA")
	ordinaryRelative := filepath.ToSlash(filepath.Join(
		BranchVariantRevisionDirName("4.00bpw"),
		BranchVariantCommitDirName("commit-a"), "files", completeFile,
	))
	hub := newBranchVariantTrustedHashHub(t, "owner/model", map[string]branchVariantTrustedHashRevision{
		"4.00bpw": {
			commit: "commit-a",
			files:  []branchVariantTrustedHashFile{{name: completeFile, expected: completeBytes, served: completeBytes}},
		},
		"5_00bpw": {
			commit: "commit-b",
			files:  []branchVariantTrustedHashFile{{name: completeFile, expected: []byte("BBBBBBBB"), served: []byte("CCCCCCCC")}},
		},
		"main": {
			commit: "ordinary-commit",
			files:  []branchVariantTrustedHashFile{{name: ordinaryRelative, expected: []byte("DDDDDDDD"), served: []byte("DDDDDDDD")}},
		},
	})
	defer hub.Close()

	settings := Settings{OutputDir: sourceRoot, Endpoint: hub.URL, Concurrency: 1, Verify: "sha256"}
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, settings, nil); err != nil {
		t.Fatalf("download complete artifact A: %v", err)
	}
	incompleteErr := Download(context.Background(), Job{Repo: "owner/model", Revision: "5_00bpw", BranchVariant: true}, settings, nil)
	if incompleteErr == nil || !strings.Contains(incompleteErr.Error(), "sha256 verify failed") {
		t.Fatalf("artifact B failure=%v; want trusted-hash failure", incompleteErr)
	}

	sourceService, err := BranchVariantServiceRoot(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(sourceService)
	if err != nil {
		t.Fatal(err)
	}
	copyDirectoryAsBytes(t, sourceRoot, copiedRoot)
	copiedService, err := BranchVariantServiceRoot(copiedRoot)
	if err != nil {
		t.Fatal(err)
	}
	copiedInfo, err := os.Stat(copiedService)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(sourceInfo, copiedInfo) {
		t.Fatal("copy fixture retained the source service-root object")
	}
	if err := os.RemoveAll(sourceRoot); err != nil {
		t.Fatalf("remove source root before copied-root use: %v", err)
	}

	owned, err := BranchVariantServiceOwned(copiedRoot)
	if err != nil || !owned {
		t.Fatalf("copied store with one incomplete sibling owned=%t err=%v; want the store to stay owned", owned, err)
	}
	artifact, err := ReadBranchVariantArtifact(copiedRoot, RepoTypeModel, "owner/model", "4.00bpw", "commit-a")
	if err != nil || artifact == nil {
		t.Fatalf("complete artifact A in the copied store=%+v err=%v", artifact, err)
	}
	if got, err := os.ReadFile(filepath.Join(artifact.PayloadRoot, completeFile)); err != nil || !bytes.Equal(got, completeBytes) {
		t.Fatalf("complete artifact A payload=%q err=%v", got, err)
	}

	// The incomplete sibling keeps no authoritative completion proof.
	incompleteControl, err := BranchVariantMetadataPath(copiedRoot, RepoTypeModel, "owner/model", "5_00bpw", "commit-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(incompleteControl, ManifestFilename)); !os.IsNotExist(err) {
		t.Fatalf("incomplete sibling artifact retained a completion manifest: %v", err)
	}

	// An ordinary write into the owned service tree stays refused.
	ordinaryTarget := filepath.Join(copiedService, "payloads", "models")
	ordinaryErr := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, Settings{
		OutputDir: ordinaryTarget, RebuildCoordinationRoots: []string{copiedRoot}, Endpoint: hub.URL, Concurrency: 1, Verify: "size",
	}, nil)
	if ordinaryErr == nil || !strings.Contains(ordinaryErr.Error(), "app-owned EXL branch variant service tree") {
		t.Fatalf("ordinary write into the copied service tree error=%v; want service-ownership refusal", ordinaryErr)
	}
	if got, err := os.ReadFile(filepath.Join(artifact.PayloadRoot, completeFile)); err != nil || !bytes.Equal(got, completeBytes) {
		t.Fatalf("refused ordinary write changed artifact A: %q err=%v", got, err)
	}

	// Retrying the incomplete sibling reaches the payload stage instead of
	// being refused as an unowned service tree.
	retryErr := Download(context.Background(), Job{Repo: "owner/model", Revision: "5_00bpw", BranchVariant: true}, Settings{
		OutputDir: copiedRoot, RebuildCoordinationRoots: []string{copiedRoot}, Endpoint: hub.URL, Concurrency: 1, Verify: "sha256",
	}, nil)
	if retryErr == nil || !strings.Contains(retryErr.Error(), "sha256 verify failed") {
		t.Fatalf("retry of the incomplete sibling error=%v; want the trusted-hash failure instead of an ownership refusal", retryErr)
	}
	if strings.Contains(retryErr.Error(), "unowned branch variant service tree") {
		t.Fatalf("retry of the incomplete sibling was refused as unowned: %v", retryErr)
	}
}

// TestUnverifiableServiceCandidateFailsClosedForIntersectingWrites covers a
// candidate that carries application-shaped ownership evidence which cannot be
// established. It is neither ordinary nor owned: writes that physically
// intersect it fail closed, and its data is neither adopted nor deleted.
func TestUnverifiableServiceCandidateFailsClosedForIntersectingWrites(t *testing.T) {
	root := t.TempDir()
	serviceRoot, err := BranchVariantServiceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(serviceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(serviceRoot, "foreign.bin")
	foreignBytes := []byte("unverifiable candidate data")
	if err := os.WriteFile(foreign, foreignBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// An application-shaped record whose physical binding no longer matches
	// this directory and whose artifacts do not corroborate a copied store.
	record, err := json.Marshal(branchVariantStoreOwner{
		Version: branchVariantStoreVersion, StoreID: "unverifiable-store-id",
		Purpose: "exl-branch-variants", FileID: "stale-physical-binding",
	})
	if err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(serviceRoot, branchVariantStoreOwnerFilename)
	if err := os.WriteFile(recordPath, record, 0o600); err != nil {
		t.Fatal(err)
	}

	owned, err := BranchVariantServiceOwned(root)
	if err == nil || owned {
		t.Fatalf("unverifiable candidate owned=%t err=%v; want an ownership failure", owned, err)
	}
	if pathOwned, err := IsOwnedBranchVariantServicePath(serviceRoot); err != nil || pathOwned {
		t.Fatalf("unverifiable candidate reported owned=%t err=%v", pathOwned, err)
	}

	hub := newBranchVariantTestHubForRepo(t, BranchVariantServiceDirName+"/model", map[string]branchVariantTestRevision{
		"main": {commit: "ordinary-commit", files: map[string][]byte{"config.json": []byte("ordinary bytes")}},
	}, nil)
	defer hub.Close()
	intersecting := Download(context.Background(), Job{Repo: BranchVariantServiceDirName + "/model", Revision: "main"}, Settings{
		OutputDir: root, Endpoint: hub.URL, Concurrency: 1, Verify: "size",
	}, nil)
	if intersecting == nil || !strings.Contains(intersecting.Error(), "ownership could not be established") {
		t.Fatalf("ordinary write into an unverifiable candidate error=%v; want a fail-closed refusal", intersecting)
	}
	if got, err := os.ReadFile(foreign); err != nil || !bytes.Equal(got, foreignBytes) {
		t.Fatalf("refused ordinary write changed the unverifiable candidate: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(serviceRoot, "model", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("refused ordinary write created data inside the unverifiable candidate: %v", err)
	}

	if err := RejectBranchVariantTreeEffects(root); err == nil ||
		!strings.Contains(err.Error(), "ownership could not be established") {
		t.Fatalf("recursive effect over an unverifiable candidate error=%v; want a fail-closed refusal", err)
	}
	if got, err := os.ReadFile(foreign); err != nil || !bytes.Equal(got, foreignBytes) {
		t.Fatalf("recursive preflight changed the unverifiable candidate: %q err=%v", got, err)
	}
}

type branchVariantTrustedHashRevision struct {
	commit string
	files  []branchVariantTrustedHashFile
}

type branchVariantTrustedHashFile struct {
	name     string
	expected []byte
	served   []byte
}

// newBranchVariantTrustedHashHub serves a declared trusted digest that may
// differ from the delivered bytes so a trusted-hash failure can be produced
// deterministically.
func newBranchVariantTrustedHashHub(t *testing.T, repoID string, revisions map[string]branchVariantTrustedHashRevision) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repoPrefix := "/api/models/" + repoID + "/"
		if strings.HasPrefix(r.URL.Path, repoPrefix+"revision/") {
			revision := strings.TrimPrefix(r.URL.Path, repoPrefix+"revision/")
			fixture, ok := revisions[revision]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: fixture.commit})
			return
		}
		if strings.HasPrefix(r.URL.Path, repoPrefix+"tree/") {
			revision := strings.TrimPrefix(r.URL.Path, repoPrefix+"tree/")
			fixture, ok := revisions[revision]
			if !ok {
				for _, candidate := range revisions {
					if candidate.commit == revision {
						fixture, ok = candidate, true
						break
					}
				}
			}
			if !ok {
				http.NotFound(w, r)
				return
			}
			nodes := make([]hfNode, 0, len(fixture.files))
			for _, file := range fixture.files {
				digest := sha256.Sum256(file.expected)
				nodes = append(nodes, hfNode{
					Type: "file", Path: file.name, Size: int64(len(file.expected)),
					LFS: &hfLfsInfo{Oid: hex.EncodeToString(digest[:]), Size: int64(len(file.expected))},
				})
			}
			_ = json.NewEncoder(w).Encode(nodes)
			return
		}
		for _, prefix := range []string{"/" + repoID + "/raw/", "/" + repoID + "/resolve/"} {
			if !strings.HasPrefix(r.URL.Path, prefix) {
				continue
			}
			rest := strings.TrimPrefix(r.URL.Path, prefix)
			revision, name, _ := strings.Cut(rest, "/")
			fixture, ok := revisions[revision]
			if !ok {
				for _, candidate := range revisions {
					if candidate.commit == revision {
						fixture, ok = candidate, true
						break
					}
				}
			}
			if !ok {
				http.NotFound(w, r)
				return
			}
			for _, file := range fixture.files {
				if file.name != name {
					continue
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(file.served)))
				if r.Method != http.MethodHead {
					_, _ = w.Write(file.served)
				}
				return
			}
			http.NotFound(w, r)
			return
		}
		http.NotFound(w, r)
	}))
}
