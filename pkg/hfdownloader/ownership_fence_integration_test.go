// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestV4ResumeAndCleanupRequireCurrentFenceOrIndependentDeath(t *testing.T) {
	const size = 4_096
	sha := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name       string
		fence      string
		dead       bool
		wantResume bool
	}{
		{name: "current realm and blob key", wantResume: true},
		{name: "legacy lock-fenced live owner"},
		{name: "wrong realm live owner", fence: "v1:" + strings.Repeat("0", 64)},
		{name: "dead legacy owner resumes", dead: true, wantResume: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cacheDir := t.TempDir()
			cache := NewHFCache(cacheDir)
			repo, err := cache.Repo("owner/model", RepoTypeModel)
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			realm, err := cache.ownershipRealmForData()
			if err != nil {
				t.Fatal(err)
			}
			blobPath := repo.BlobPath(sha)
			staging, metaPath := repo.IncompletePath(sha), repo.IncompleteMetaPath(sha)
			payload := bytes.Repeat([]byte("r"), size)
			if err := os.WriteFile(staging, payload, 0o644); err != nil {
				t.Fatal(err)
			}
			currentFence, err := realm.fenceProof(blobPath)
			if err != nil {
				t.Fatal(err)
			}
			recordFence := tc.fence
			if tc.name == "current realm and blob key" {
				recordFence = currentFence
			}
			pid := os.Getpid()
			if tc.dead {
				pid = 99_999_999
			}
			meta := IncompleteMeta{
				PID: pid, Instance: "different-instance", Host: processHostname,
				StartedAt: time.Now().UTC(), Size: size, SHA256: sha, Version: stagingMetaVersion,
				Repo: "owner/model", Commit: "commit123", Path: "model.bin", CommitPinned: true,
				Ranges:       []RangeProgress{{Start: 0, End: size - 1, Committed: size / 2}},
				ProofPending: true, LockFenced: true, OwnershipFence: recordFence,
			}
			if err := writeStagingMeta(metaPath, &meta); err != nil {
				t.Fatal(err)
			}

			wantStatus := BlobDownloading
			if tc.wantResume {
				wantStatus = BlobStale
			}
			status, _, err := repo.CheckBlob(sha)
			if err != nil || status != wantStatus {
				t.Fatalf("CheckBlob before claim = %v, %v; want %v", status, err, wantStatus)
			}

			dst, err := repo.blobDomainDst(sha)
			if err != nil {
				t.Fatal(err)
			}
			domain, err := acquireBlobDomainScoped(context.Background(), blobPath, dst, realm,
				[]string{blobPath, dst, staging, metaPath}, blobPath)
			if err != nil {
				t.Fatalf("acquire cache-owned item domain: %v", err)
			}
			item := PlanItem{RelativePath: "model.bin", Size: size, SHA256: sha, LFS: true}
			state, claimErr := claimStaging(context.Background(), staging, blobPath, metaPath,
				item, Job{Repo: "owner/model"}, "commit123", true, Settings{Verify: "size"}, true, domain)
			if tc.wantResume {
				if claimErr != nil {
					t.Fatalf("claim valid suffix: %v", claimErr)
				}
				if state == nil || len(state.ranges) != 1 || state.ranges[0].Committed != size/2 || !state.proofPending {
					t.Fatalf("resumed state = %+v, want the matching suffix and pending proof preserved", state)
				}
				updated, err := readStagingMeta(metaPath)
				if err != nil || !updated.LockFenced || updated.OwnershipFence != currentFence {
					t.Fatalf("claimed record fence = %+v, %v; want current proof %q", updated, err, currentFence)
				}
			} else {
				if !errors.Is(claimErr, ErrStagingOwnershipUnproven) {
					t.Fatalf("claim of live legacy/wrong-key record = %v, want unproven", claimErr)
				}
			}
			domain.release()

			status, _, err = repo.CheckBlob(sha)
			if err != nil || status != wantStatus {
				t.Fatalf("CheckBlob after claim = %v, %v; want %v", status, err, wantStatus)
			}
			if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
				t.Fatalf("CleanupJobPartFiles: %v", err)
			}
			_, payloadErr := os.Stat(staging)
			_, metaErr := os.Stat(metaPath)
			if tc.wantResume {
				if !errors.Is(payloadErr, os.ErrNotExist) || !errors.Is(metaErr, os.ErrNotExist) {
					t.Fatalf("cleanup did not remove proven stale state: payload=%v metadata=%v", payloadErr, metaErr)
				}
			} else if payloadErr != nil || metaErr != nil {
				t.Fatalf("cleanup removed unproven live state: payload=%v metadata=%v", payloadErr, metaErr)
			}
		})
	}
}

func TestOwnershipRealmRejectsOverlapWithStorage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "storage")
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	realm, err := newOwnershipRealm(filepath.Join(root, "ownership"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOwnershipRealmOutside(realm, root); err == nil {
		t.Fatal("a lock realm inside the storage tree was accepted")
	}
}

func TestFinalizeBlobRefusesLegacyFenceAndAcceptsCurrentFence(t *testing.T) {
	cache := NewHFCache(t.TempDir())
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	staging, metaPath, blob := repo.IncompletePath(sha), repo.IncompleteMetaPath(sha), repo.BlobPath(sha)
	payload := []byte("complete staged payload")
	if err := os.WriteFile(staging, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{
		PID: os.Getpid(), Instance: "foreign-instance", Host: processHostname,
		Version: stagingMetaVersion, LockFenced: true,
	}
	writeMeta := func() {
		t.Helper()
		data, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(metaPath, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeMeta()
	if err := repo.FinalizeBlob(sha); !errors.Is(err, ErrStagingOwnershipUnproven) {
		t.Fatalf("FinalizeBlob with legacy LockFenced-only metadata = %v, want unproven", err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("legacy staging payload was not preserved: %v", err)
	}
	if _, err := os.Stat(blob); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy staging was published, stat err=%v", err)
	}

	realm, err := cache.ownershipRealmForData()
	if err != nil {
		t.Fatal(err)
	}
	meta.OwnershipFence, err = realm.fenceProof(blob)
	if err != nil {
		t.Fatal(err)
	}
	writeMeta()
	if err := repo.FinalizeBlob(sha); err != nil {
		t.Fatalf("FinalizeBlob with matching current fence: %v", err)
	}
	assertExactFile(t, blob, payload)
	if _, err := os.Stat(metaPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("current fenced metadata survived publication, stat err=%v", err)
	}
}
