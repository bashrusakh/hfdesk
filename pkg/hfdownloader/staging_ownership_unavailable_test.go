// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the unknown-ownership outcome of the cache status
// check (issue #140 / PR #151).
//
// The ownership check has three possible outcomes and must never guess:
//
//   - a PROVEN live owner (the persistent <staging>.lock is held
//     exclusively) -> BlobDownloading: the caller does NOT skip the item,
//     because the file is not present while the owner holds it. The downloader
//     waits for the blob key's ownership domain and re-checks the published
//     target instead;
//   - a PROVABLY dead owner -> BlobStale: the caller may resume the item;
//   - an UNKNOWN ownership state (the OS-lock probe failed, or the platform
//     cannot represent the lock) -> BlobOwnershipUnknown plus an error: the
//     caller must surface the failure.
//
// The defect this file pins: the unknown outcome used to be reported as
// BlobDownloading, i.e. as a PROVEN live owner. The downloader's skip check
// turned that into a successful skip, so the job reported "download complete
// (downloaded 0, skipped 1)" while the blob was still absent and the partial
// was untouched. The mutation path (the staging claim) already failed closed
// for the same condition, so the status path contradicted the downloader's
// own safety rule.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
	// The status path must not claim a live owner it could not prove. An
	// unknown ownership state is an observable error, never a successful
	// skip.
	status, _, err := repo.CheckBlob(sha)
	if !errors.Is(err, ErrBlobOwnershipUnknown) {
		t.Fatalf("CheckBlob with unavailable ownership probe = %v, %v; want an unknown-ownership error", status, err)
	}
	if status != BlobOwnershipUnknown {
		t.Fatalf("status with unavailable ownership probe = %v; want %v", status, BlobOwnershipUnknown)
	}
	// The incomplete payload is untouched: a retry can still resume it.
	gotPayload, err = os.ReadFile(cacheStaging)
	if err != nil || string(gotPayload) != string(payload) {
		t.Fatalf("the unknown-ownership check changed the incomplete payload: %q, %v", gotPayload, err)
	}
}

// TestDownloadUnknownOwnershipIsAnObservableError is the end-to-end half of
// the same invariant: with the ownership probe unavailable, the job must
// report the failure instead of a successful skip.
//
// Before the fix the job returned nil and reported "download complete
// (downloaded 0, skipped 1)" while the blob was absent.
func TestDownloadUnknownOwnershipIsAnObservableError(t *testing.T) {
	oldProbe := stagingOwnershipStatusProbe
	defer func() { stagingOwnershipStatusProbe = oldProbe }()
	probeErr := errors.New("injected ownership probe permission denied")
	stagingOwnershipStatusProbe = func(string) (bool, error) { return false, probeErr }

	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo, err := NewHFCache(cacheDir, time.Minute).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// A leftover incomplete download makes the ownership check run its probe.
	partial := []byte("leftover-partial")
	if err := os.WriteFile(repo.IncompletePath(sha), partial, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{
		PID: os.Getpid(), Instance: processInstanceToken, Host: processHostname,
		StartedAt: time.Now().UTC(), Size: int64(total), SHA256: sha,
		Version: stagingMetaVersion, Repo: "owner/model", Path: "model.bin",
		Commit: "commit123", LockFenced: true, Ranges: buildRangeLayout(int64(total), 1),
	}
	if err := writeStagingMeta(repo.IncompleteMetaPath(sha), &meta); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.HasSuffix(r.URL.Path, "/tree/main"):
			_, _ = w.Write([]byte("[" + lfsTreeNode("model.bin", total, sha) + "]"))
		case strings.Contains(r.URL.Path, "/resolve/"):
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := Settings{
		CacheDir: cacheDir, Endpoint: srv.URL, Concurrency: 1, MaxActiveDownloads: 1,
		MultipartThreshold: "1MB", Verify: "size", Retries: 0, StallTimeout: "0",
	}
	var skips []string
	dlErr := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ev ProgressEvent) {
		if ev.Event == "file_done" && strings.Contains(ev.Message, "skip") {
			skips = append(skips, ev.Message)
		}
	})
	if dlErr == nil {
		t.Fatalf("Download with an unavailable ownership probe reported success; skips=%v", skips)
	}
	if !errors.Is(dlErr, ErrBlobOwnershipUnknown) {
		t.Errorf("Download error = %v, want an unknown-ownership error", dlErr)
	}
	if len(skips) != 0 {
		t.Errorf("the job recorded a successful skip although the ownership state was unknown: %v", skips)
	}
	// The blob was never published and the partial survived: a retry can
	// still resume it.
	if _, serr := os.Stat(repo.BlobPath(sha)); !os.IsNotExist(serr) {
		t.Errorf("a blob was published although the ownership state was unknown, stat err=%v", serr)
	}
	got, rerr := os.ReadFile(repo.IncompletePath(sha))
	if rerr != nil || string(got) != string(partial) {
		t.Errorf("the incomplete payload did not survive the unknown-ownership failure: %q, %v", got, rerr)
	}
}

// TestCheckBlobProbeErrorPrecedesMissingOrMalformedMetadata pins that the
// ownership probe decides first: an unreadable or missing record must not
// turn an unknown ownership state into a resumable "stale" verdict.
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
			if !errors.Is(err, ErrBlobOwnershipUnknown) || status != BlobOwnershipUnknown {
				t.Fatalf("CheckBlob with unavailable lock probe = %v, %v; want an unknown-ownership error", status, err)
			}
		})
	}
}

// TestCheckBlobProvenOwnerAndStaleRemainDistinct pins the two other outcomes
// of the same check: a PROVEN live owner may be skipped, a PROVABLY dead
// owner may be resumed. The unknown-ownership fix must not blur them.
func TestCheckBlobProvenOwnerAndStaleRemainDistinct(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	cache := NewHFCache(t.TempDir(), time.Minute)
	repo, err := cache.Repo("owner/repo", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("e", 64)
	staging := repo.IncompletePath(sha)
	if err := os.WriteFile(staging, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{
		PID: os.Getpid(), Instance: processInstanceToken, Host: processHostname,
		StartedAt: time.Now().UTC(), Size: 7, SHA256: sha, Version: stagingMetaVersion,
		Repo: "owner/repo", Path: "model.bin", Ranges: buildRangeLayout(7, 1), LockFenced: true,
	}
	if err := writeStagingMeta(repo.IncompleteMetaPath(sha), &meta); err != nil {
		t.Fatal(err)
	}

	// A proven live owner: the lifetime lock is held exclusively.
	own, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	status, _, err := repo.CheckBlob(sha)
	if err != nil || status != BlobDownloading {
		t.Fatalf("status with a held ownership lock = %v, %v; want %v", status, err, BlobDownloading)
	}
	own.release()

	// A provably dead owner: a free lock plus a lock-fenced record.
	status, _, err = repo.CheckBlob(sha)
	if err != nil || status != BlobStale {
		t.Fatalf("status with a free ownership lock = %v, %v; want %v", status, err, BlobStale)
	}
}

// TestDownloadProvenOwnerIsNotASuccessfulSkip is the download-level half of
// the same distinction: a PROVEN live owner is NOT a successful skip.
//
// The file is not present while the live owner holds the item's staging, so
// the job must not report the item as done. The item waits for the blob key's
// ownership domain and re-checks the published target; when the owner still
// owns the item, the item fails with the actionable live-owner error instead
// of a skip that would report a completion the cache does not have.
func TestDownloadProvenOwnerIsNotASuccessfulSkip(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo, err := NewHFCache(cacheDir, time.Minute).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	partial := []byte("partial")
	if err := os.WriteFile(repo.IncompletePath(sha), partial, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{
		PID: os.Getpid(), Instance: processInstanceToken, Host: processHostname,
		StartedAt: time.Now().UTC(), Size: int64(total), SHA256: sha,
		Version: stagingMetaVersion, Repo: "owner/model", Path: "model.bin",
		Commit: "commit123", LockFenced: true, Ranges: buildRangeLayout(int64(total), 1),
	}
	if err := writeStagingMeta(repo.IncompleteMetaPath(sha), &meta); err != nil {
		t.Fatal(err)
	}
	// A real writer holds the lifetime lock for the whole run.
	own, err := acquireStagingOwnershipLock(repo.IncompletePath(sha))
	if err != nil {
		t.Fatal(err)
	}
	defer own.release()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.HasSuffix(r.URL.Path, "/tree/main"):
			_, _ = w.Write([]byte("[" + lfsTreeNode("model.bin", total, sha) + "]"))
		case strings.Contains(r.URL.Path, "/resolve/"):
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := Settings{
		CacheDir: cacheDir, Endpoint: srv.URL, Concurrency: 1, MaxActiveDownloads: 1,
		// The multipart route is the one that claims the blob key's staging
		// file, so the live owner's lock is the fence under test.
		MultipartThreshold: "0", Verify: "size", Retries: 0, StallTimeout: "0",
	}
	var skips []string
	dlErr := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ev ProgressEvent) {
		if ev.Event == "file_done" && strings.Contains(ev.Message, "skip") {
			skips = append(skips, ev.Message)
		}
	})
	if dlErr == nil {
		t.Fatalf("Download reported success although a live owner holds the item; skips=%v", skips)
	}
	if !errors.Is(dlErr, errBlobOwnedByLiveWriter) {
		t.Errorf("Download error = %v, want the actionable live-owner error", dlErr)
	}
	if len(skips) != 0 {
		t.Errorf("the job recorded a successful skip although the live owner holds the item: %v", skips)
	}
	// The file is not present and the owner's staging is untouched.
	if _, serr := os.Stat(repo.BlobPath(sha)); !os.IsNotExist(serr) {
		t.Errorf("a blob was published although the live owner holds the item, stat err=%v", serr)
	}
	got, rerr := os.ReadFile(repo.IncompletePath(sha))
	if rerr != nil || !bytes.Equal(got, partial) {
		t.Errorf("the live owner's staging changed: %q, %v", got, rerr)
	}
}
