// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStagingOwnershipUnavailableFailsClosed(t *testing.T) {
	oldAcquire, oldProbe := stagingOwnershipAcquire, stagingOwnershipStatusProbe
	defer func() { stagingOwnershipAcquire, stagingOwnershipStatusProbe = oldAcquire, oldProbe }()
	lockErr := errors.New("injected ownership lock permission denied")
	stagingOwnershipAcquire = func(string) (*stagingOwnershipLock, error) { return nil, lockErr }
	stagingOwnershipStatusProbe = func(string) (bool, error) { return false, lockErr }

	dir := t.TempDir()
	staging := filepath.Join(dir, "payload.incomplete")
	metaPath := staging + ".meta"
	payload := []byte("committed-prefix")
	if err := os.WriteFile(staging, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{PID: 99999999, Instance: "old", Host: "other", Size: int64(len(payload)), Version: stagingMetaVersion, Repo: "o/r", Path: "payload", LockFenced: true, Ranges: buildRangeLayout(int64(len(payload)), 1)}
	if err := writeStagingMeta(metaPath, &meta); err != nil {
		t.Fatal(err)
	}
	beforeMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = claimStaging(t.Context(), staging, filepath.Join(dir, "published"), metaPath,
		PlanItem{RelativePath: "payload", Size: int64(len(payload))}, Job{Repo: "o/r"}, "commit", Settings{}, false)
	if !errors.Is(err, lockErr) || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("claim error = %v, want actionable ownership failure", err)
	}
	removeStagingArtifactsIfUnclaimed(staging, metaPath)
	gotPayload, err := os.ReadFile(staging)
	if err != nil || string(gotPayload) != string(payload) {
		t.Fatalf("cleanup changed payload: %q, %v", gotPayload, err)
	}
	afterMeta, err := os.ReadFile(metaPath)
	if err != nil || string(afterMeta) != string(beforeMeta) {
		t.Fatalf("claim/cleanup changed metadata: err=%v", err)
	}

	cache := NewHFCache(dir, time.Minute)
	repo, err := cache.Repo("o/r", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 64)
	cacheStaging := repo.IncompletePath(sha)
	if err := os.WriteFile(cacheStaging, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	cacheMeta := meta
	cacheMeta.SHA256 = sha
	if err := writeStagingMeta(repo.IncompleteMetaPath(sha), &cacheMeta); err != nil {
		t.Fatal(err)
	}
	status, _, err := repo.CheckBlob(sha)
	if err != nil || status != BlobDownloading {
		t.Fatalf("status with unavailable ownership probe = %v, %v; want downloading", status, err)
	}
}

func TestCheckBlobProbeErrorPrecedesMissingOrMalformedMetadata(t *testing.T) {
	oldProbe := stagingOwnershipStatusProbe
	defer func() { stagingOwnershipStatusProbe = oldProbe }()
	stagingOwnershipStatusProbe = func(string) (bool, error) {
		return false, errStagingOSLockUnsupported
	}

	for _, tc := range []struct {
		name string
		meta []byte
	}{
		{name: "missing metadata"},
		{name: "malformed metadata", meta: []byte("{")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewHFCache(t.TempDir(), time.Minute)
			repo, err := cache.Repo("owner/repo", RepoTypeModel)
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			sha := strings.Repeat("c", 64)
			if err := os.WriteFile(repo.IncompletePath(sha), []byte("partial"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.meta != nil {
				if err := os.WriteFile(repo.IncompleteMetaPath(sha), tc.meta, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			status, _, err := repo.CheckBlob(sha)
			if err != nil || status != BlobDownloading {
				t.Fatalf("CheckBlob with unavailable lock probe = %v, %v; want downloading", status, err)
			}
		})
	}
}
