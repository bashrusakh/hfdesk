// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Ownership-identity and claim-aware-cleanup coverage for the issue #140
// correction set. The cleanup tests reproduce blocker 3a: a cancelled job must
// not remove a staging payload/meta pair still owned by a live foreign claim,
// while its own (same process instance) staging remains removable.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRawStagingMeta(t *testing.T, metaPath, raw string) {
	t.Helper()
	if err := os.WriteFile(metaPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRemoveStagingArtifacts_MetaFirstFailurePreservesPayload pins S3's
// ordering consequence: if the record cannot be removed, the payload must be
// preserved rather than left orphaned under a record that claims its bytes.
func TestRemoveStagingArtifacts_MetaFirstFailurePreservesPayload(t *testing.T) {
	tmpDir := t.TempDir()
	staging := filepath.Join(tmpDir, "x.incomplete")
	metaPath := staging + ".meta"
	if err := os.WriteFile(staging, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory at the meta path cannot be removed by os.Remove.
	if err := os.Mkdir(metaPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaPath, "child"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	acquireTestDomain(t, filepath.Join(tmpDir, "x"))
	removeStagingArtifactsIfUnowned(staging, metaPath)
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("payload was removed after the record removal failed: %v", err)
	}
	// Once the record can be removed, the payload follows.
	if err := os.RemoveAll(metaPath); err != nil {
		t.Fatal(err)
	}
	removeStagingArtifactsIfUnowned(staging, metaPath)
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("payload survived a successful metadata removal, stat err=%v", err)
	}
}

func TestCleanupJobPartFiles_RespectsLiveClaim(t *testing.T) {
	const total = 4000
	host, _ := os.Hostname()

	t.Run("live foreign claim survives", func(t *testing.T) {
		cacheDir := t.TempDir()
		blobsDir := filepath.Join(cacheDir, "hub", "models--owner--repo", "blobs")
		if err := os.MkdirAll(blobsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		sha := strings.Repeat("a", 64)
		dst := filepath.Join(blobsDir, "tmp-"+sha)
		staging := filepath.Join(blobsDir, sha+".incomplete")
		metaPath := staging + ".meta"
		if err := os.WriteFile(staging, make([]byte, total), 0o644); err != nil {
			t.Fatal(err)
		}
		raw := fmt.Sprintf(
			`{"pid":%d,"instance":"foreign-live-instance","host":%q,"size":%d,"version":%d,"repo":"o/r","path":"x.bin","ranges":[{"start":0,"end":%d,"committed":0,"done":false}]}`,
			os.Getppid(), host, total, stagingMetaVersion, total-1)
		writeRawStagingMeta(t, metaPath, raw)
		// A live owner holds the destination's ownership-domain lock for its
		// whole interval; cleanup must treat that, not record age, as the
		// authoritative ownership signal.
		hold, _ := acquireStagingOwnershipLock(dst)
		if hold != nil {
			defer hold.release()
		}

		if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
			t.Fatalf("CleanupJobPartFiles: %v", err)
		}
		for _, p := range []string{staging, metaPath} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("cleanup removed a live foreign claim's %s: %v", filepath.Base(p), err)
			}
		}
	})

	t.Run("same process instance staging is removed", func(t *testing.T) {
		cacheDir := t.TempDir()
		blobsDir := filepath.Join(cacheDir, "hub", "models--owner--repo", "blobs")
		if err := os.MkdirAll(blobsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		sha := strings.Repeat("b", 64)
		dst := filepath.Join(blobsDir, "tmp-"+sha)
		staging := filepath.Join(blobsDir, sha+".incomplete")
		metaPath := staging + ".meta"
		if err := os.WriteFile(staging, make([]byte, total), 0o644); err != nil {
			t.Fatal(err)
		}
		meta := IncompleteMeta{
			PID:       os.Getpid(),
			Instance:  processInstanceToken,
			Host:      processHostname,
			StartedAt: time.Now().UTC(),
			Size:      total,
			Version:   stagingMetaVersion,
			Repo:      "o/r",
			Path:      "x.bin",
			Ranges:    buildRangeLayout(total, 2),
		}
		if err := writeStagingMeta(metaPath, &meta); err != nil {
			t.Fatal(err)
		}

		if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
			t.Fatalf("CleanupJobPartFiles: %v", err)
		}
		for _, p := range []string{staging, metaPath} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("own paused staging %s survived cleanup (stat err=%v)", filepath.Base(p), err)
			}
		}
	})

	t.Run("in-process active claim survives even with our token", func(t *testing.T) {
		cacheDir := t.TempDir()
		blobsDir := filepath.Join(cacheDir, "hub", "models--owner--repo", "blobs")
		if err := os.MkdirAll(blobsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		sha := strings.Repeat("c", 64)
		dst := filepath.Join(blobsDir, "tmp-"+sha)
		staging := filepath.Join(blobsDir, sha+".incomplete")
		metaPath := staging + ".meta"
		if err := os.WriteFile(staging, make([]byte, total), 0o644); err != nil {
			t.Fatal(err)
		}
		meta := IncompleteMeta{
			PID: os.Getpid(), Instance: processInstanceToken, Host: processHostname,
			StartedAt: time.Now().UTC(), Size: total, Version: stagingMetaVersion,
			Repo: "o/r", Path: "x.bin", Ranges: buildRangeLayout(total, 2),
		}
		if err := writeStagingMeta(metaPath, &meta); err != nil {
			t.Fatal(err)
		}

		// A live in-process writer holds the destination's ownership domain.
		// The cleanup's own domain acquisition must report that live owner
		// instead of removing the pair.
		key, keyed := cacheDstDomainKey(dst)
		if !keyed {
			t.Fatalf("the cache destination %s has no ownership-domain key", dst)
		}
		owner, derr := blobDomainAcquire(context.Background(), key, dst)
		if derr != nil {
			t.Fatalf("acquire the live in-process domain: %v", derr)
		}

		if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
			t.Fatalf("CleanupJobPartFiles: %v", err)
		}
		for _, p := range []string{staging, metaPath} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("cleanup removed an actively claimed %s: %v", filepath.Base(p), err)
			}
		}

		// Once the domain is released, the same fixtures are removable.
		owner.release()
		if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
			t.Fatalf("CleanupJobPartFiles after release: %v", err)
		}
		for _, p := range []string{staging, metaPath} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("%s survived cleanup after the claim was released", filepath.Base(p))
			}
		}
	})
}

// TestCheckBlob_OwnershipIdentity pins the CheckBlob consumer of the ownership
// classification. A lock-fenced record on a platform with the OS lock is
// authoritative: a free lock means the current-version owner is gone even when
// the record carries our live PID. Unfenced records stay "downloading" unless
// same-host process death can be proven.
func TestCheckBlob_OwnershipIdentity(t *testing.T) {
	tmpDir := t.TempDir()
	cache := NewHFCache(tmpDir, time.Minute)
	repo, err := cache.Repo("owner/name", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("d", 64)
	staging := repo.IncompletePath(sha)
	if err := os.WriteFile(staging, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := IncompleteMeta{
		PID:        os.Getpid(),
		Instance:   processInstanceToken,
		Host:       processHostname,
		StartedAt:  time.Now().UTC(),
		Size:       100,
		Version:    stagingMetaVersion,
		Repo:       "o/r",
		Path:       "x.bin",
		Ranges:     buildRangeLayout(100, 2),
		LockFenced: true,
	}
	if err := writeStagingMeta(repo.IncompleteMetaPath(sha), &base); err != nil {
		t.Fatal(err)
	}
	if status, _, err := repo.CheckBlob(sha); err != nil || status != BlobStale {
		t.Fatalf("same-instance record status = %v (err=%v), want BlobStale", status, err)
	}

	foreign := base
	foreign.Instance = "different-process-instance"
	if err := writeStagingMeta(repo.IncompleteMetaPath(sha), &foreign); err != nil {
		t.Fatal(err)
	}
	if status, _, err := repo.CheckBlob(sha); err != nil || status != BlobStale {
		t.Fatalf("lock-fenced foreign instance with our PID and a free lock = %v (err=%v), want BlobStale", status, err)
	}

	// An unfenced record does not become safe merely because the new lock probe
	// is free: a fresh foreign record with our live PID remains "downloading".
	fallback := foreign
	fallback.LockFenced = false
	if err := writeStagingMeta(repo.IncompleteMetaPath(sha), &fallback); err != nil {
		t.Fatal(err)
	}
	if status, _, err := repo.CheckBlob(sha); err != nil || status != BlobDownloading {
		t.Fatalf("fallback foreign instance with our PID status = %v (err=%v), want BlobDownloading", status, err)
	}

	dead := foreign
	dead.PID = 99999999
	if err := writeStagingMeta(repo.IncompleteMetaPath(sha), &dead); err != nil {
		t.Fatal(err)
	}
	if status, _, err := repo.CheckBlob(sha); err != nil || status != BlobStale {
		t.Fatalf("provably dead owner status = %v (err=%v), want BlobStale", status, err)
	}
}

// TestStagingMetaResumableIdentity pins S5's all-or-nothing identity rules.
func TestStagingMetaResumableIdentity(t *testing.T) {
	const size = int64(1000)
	shaA := strings.Repeat("a", 64)
	shaB := strings.Repeat("b", 64)
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "x.bin", Size: size, SHA256: shaA}
	base := IncompleteMeta{
		Version:      stagingMetaVersion,
		Size:         size,
		Repo:         "o/r",
		Path:         "x.bin",
		Commit:       "commit-a",
		CommitPinned: true,
		SHA256:       shaA,
		Ranges:       buildRangeLayout(size, 2),
	}

	if _, ok := stagingMetaResumable(base, it, job, "commit-a", true); !ok {
		t.Error("matching identity was rejected; a valid record must resume")
	}

	cases := []struct {
		name   string
		mutate func(*IncompleteMeta)
		item   PlanItem
		commit string
		pinned bool
	}{
		{name: "commit mismatch", mutate: func(m *IncompleteMeta) { m.Commit = "commit-b" }, item: it, commit: "commit-a", pinned: true},
		{name: "repo mismatch", mutate: func(m *IncompleteMeta) { m.Repo = "o/other" }, item: it, commit: "commit-a", pinned: true},
		{name: "path mismatch", mutate: func(m *IncompleteMeta) { m.Path = "y.bin" }, item: it, commit: "commit-a", pinned: true},
		{name: "one-sided record SHA", mutate: func(m *IncompleteMeta) { m.SHA256 = "" }, item: it, commit: "commit-a", pinned: true},
		{name: "one-sided item SHA", mutate: func(m *IncompleteMeta) {}, item: PlanItem{RelativePath: "x.bin", Size: size}, commit: "commit-a", pinned: true},
		{name: "different SHA", mutate: func(m *IncompleteMeta) { m.SHA256 = shaB }, item: it, commit: "commit-a", pinned: true},
		{name: "legacy revision does not substitute for commit", mutate: func(m *IncompleteMeta) {
			m.Commit = ""
			m.Revision = "main"
		}, item: it, commit: "main", pinned: true},
		// The pinned rule: an unpinned identity names a revision whose
		// content may have moved, so it inherits nothing.
		{name: "unpinned record", mutate: func(m *IncompleteMeta) { m.CommitPinned = false }, item: it, commit: "commit-a", pinned: true},
		{name: "unpinned current plan", mutate: func(m *IncompleteMeta) {}, item: it, commit: "commit-a", pinned: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := base
			meta.Ranges = append([]RangeProgress(nil), base.Ranges...)
			tc.mutate(&meta)
			if _, ok := stagingMetaResumable(meta, tc.item, job, tc.commit, tc.pinned); ok {
				t.Errorf("unprovable identity %q was accepted for resume", tc.name)
			}
		})
	}

	// A range whose byte count is full but whose Done marker is absent has no
	// clean-framing proof; retain all but the final byte for revalidation.
	unnormalized := base
	unnormalized.SHA256 = "" // item below has no SHA: both-or-neither
	unnormalized.Ranges = []RangeProgress{{Start: 0, End: 499, Committed: 500}, {Start: 500, End: 999}}
	ranges, ok := stagingMetaResumable(unnormalized, PlanItem{RelativePath: "x.bin", Size: size}, job, "commit-a", true)
	if !ok {
		t.Fatal("normalizable record was rejected")
	}
	if ranges[0].Done || ranges[0].Committed != 499 {
		t.Errorf("unframed full prefix was not reduced to a nonempty revalidation suffix: %+v", ranges[0])
	}

	overclaim := base
	overclaim.SHA256 = ""
	overclaim.Ranges = []RangeProgress{{Start: 0, End: 499, Committed: 501}, {Start: 500, End: 999}}
	if _, ok := stagingMetaResumable(overclaim, PlanItem{RelativePath: "x.bin", Size: size}, job, "commit-a", true); ok {
		t.Error("a record claiming bytes past its range end was accepted")
	}
}
