// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Unknown ownership is an error, not BlobDownloading. Only proven live
// ownership may produce that status; acquisition and cleanup fail closed.

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

	// Domain acquisition must fail before claim or payload mutation.
	if _, derr := blobDomainAcquire(context.Background(), flatDomainKey(filepath.Join(dir, "payload")),
		flatDomainLockSrc(filepath.Join(dir, "payload"))); !errors.Is(derr, lockErr) || !strings.Contains(derr.Error(), "ownership") {
		t.Fatalf("domain acquisition error = %v, want actionable ownership failure", derr)
	}
	// A cleanup that cannot hold the destination's domain removes nothing.
	if err := CleanupJobPartFiles(Settings{OutputDir: dir}, []string{filepath.Join(dir, "payload")}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	gotPayload, err := os.ReadFile(staging)
	if err != nil || string(gotPayload) != string(payload) {
		t.Fatalf("cleanup changed payload: %q, %v", gotPayload, err)
	}
	afterMeta, err := os.ReadFile(metaPath)
	if err != nil || string(afterMeta) != string(beforeMeta) {
		t.Fatalf("claim/cleanup changed metadata: err=%v", err)
	}

	cache := NewHFCache(dir)
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
	// An unavailable probe must return BlobOwnershipUnknown with its error.
	status, _, err := repo.CheckBlob(sha)
	if !errors.Is(err, ErrBlobOwnershipUnknown) {
		t.Fatalf("CheckBlob with unavailable ownership probe = %v, %v; want an unknown-ownership error", status, err)
	}
	if status != BlobOwnershipUnknown {
		t.Fatalf("status with unavailable ownership probe = %v; want %v", status, BlobOwnershipUnknown)
	}
	// The status probe must leave the incomplete payload untouched.
	gotPayload, err = os.ReadFile(cacheStaging)
	if err != nil || string(gotPayload) != string(payload) {
		t.Fatalf("the unknown-ownership check changed the incomplete payload: %q, %v", gotPayload, err)
	}
}

// Download must report an unavailable ownership probe instead of successful skip.
func TestDownloadUnknownOwnershipIsAnObservableError(t *testing.T) {
	oldProbe := stagingOwnershipStatusProbe
	defer func() { stagingOwnershipStatusProbe = oldProbe }()
	probeErr := errors.New("injected ownership probe permission denied")
	stagingOwnershipStatusProbe = func(string) (bool, error) { return false, probeErr }

	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo, err := NewHFCache(cacheDir).Repo("owner/model", RepoTypeModel)
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
		case strings.Contains(r.URL.Path, "/tree/"):
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
	// No blob is published and the existing partial is preserved.
	if _, serr := os.Stat(repo.BlobPath(sha)); !os.IsNotExist(serr) {
		t.Errorf("a blob was published although the ownership state was unknown, stat err=%v", serr)
	}
	got, rerr := os.ReadFile(repo.IncompletePath(sha))
	if rerr != nil || string(got) != string(partial) {
		t.Errorf("the incomplete payload did not survive the unknown-ownership failure: %q, %v", got, rerr)
	}
}

// Probe failure must remain unknown ownership even with missing or malformed
// metadata; it must not become BlobStale.
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
			cache := NewHFCache(t.TempDir())
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

// Download waits for a proven live owner; if it remains past the bounded
// budget, return the live-owner error rather than successful skip.
func TestDownloadProvenOwnerIsNotASuccessfulSkip(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo, err := NewHFCache(cacheDir).Repo("owner/model", RepoTypeModel)
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
	// A real writer holds the destination domain's lifetime lock for the whole
	// run. The item's bounded domain acquisition then reports the live owner.
	shortBlobOwnershipWait(t, 250*time.Millisecond)
	own, err := acquireStagingOwnershipLock(mustDomainDst(t, repo, sha))
	if err != nil {
		t.Fatal(err)
	}
	defer own.release()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
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
