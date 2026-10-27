// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Uses real child processes to verify cross-process publish/reuse and
// cancellation while waiting. The contention hook and child control files
// establish ordering without sleeps.

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func encodeOwnershipPayload(payload []byte) string {
	return base64.StdEncoding.EncodeToString(payload)
}

// Both routes must reuse a foreign owner's publication; the single route must re-check too.
func TestDownloadWaitsForForeignProcessOwnerThatPublishesAndReusesTheBlob(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	for _, tc := range []struct {
		name      string
		threshold string
		// partial selects the live staging artifact for each transfer route.
		partial func(t *testing.T, repo *RepoDir, sha string) string
	}{
		{
			name:      "multipart route",
			threshold: "0",
			partial: func(t *testing.T, repo *RepoDir, sha string) string {
				return repo.IncompletePath(sha)
			},
		},
		{
			name:      "single-request route",
			threshold: "1MB",
			partial: func(t *testing.T, repo *RepoDir, sha string) string {
				return mustDomainDst(t, repo, sha) + ".part"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testDownloadWaitsForForeignProcessOwnerThatPublishes(t, tc.threshold, tc.partial)
		})
	}
}

func testDownloadWaitsForForeignProcessOwnerThatPublishes(t *testing.T, threshold string,
	partial func(t *testing.T, repo *RepoDir, sha string) string) {
	t.Helper()
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--model", "blobs")

	child := startBlobOwnershipChild(t, blobOwnershipChildConfig{
		DomainDst:   mustDomainDst(t, repo, sha),
		PartialPath: partial(t, repo, sha),
		Payload:     "the live partial of the other process",
		ReadyPath:   filepath.Join(blobsDir, "child-ready"),
		GoPath:      filepath.Join(blobsDir, "child-go"),
		// Publish only after the parent confirms it is waiting.
		PublishPath:          repo.BlobPath(sha),
		PublishPayloadBase64: encodeOwnershipPayload(full),
		PublishGoPath:        filepath.Join(blobsDir, "child-publish"),
	})
	waitForFile(t, filepath.Join(blobsDir, "child-ready"), 60*time.Second)
	defer releaseBlobOwnershipChild(t, child, filepath.Join(blobsDir, "child-go"))

	srv, getRanges := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, threshold)

	contended := installDomainContentionProbe(t)
	result := make(chan ownershipContractOutcome, 1)
	go func() { result <- runOwnershipContractDownload(context.Background(), cfg) }()

	awaitContention(t, contended, "the blob key's ownership domain")
	select {
	case out := <-result:
		t.Fatalf("the download finished while the foreign owner still held the domain (err=%v, skips=%v)",
			out.err, out.skips)
	default:
	}

	if err := os.WriteFile(filepath.Join(blobsDir, "child-publish"), []byte("publish"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, repo.BlobPath(sha), 60*time.Second)
	releaseBlobOwnershipChild(t, child, filepath.Join(blobsDir, "child-go"))

	var out ownershipContractOutcome
	select {
	case out = <-result:
	case <-time.After(120 * time.Second):
		t.Fatal("the download did not finish after the foreign owner released the domain")
	}
	if out.err != nil {
		t.Fatalf("Download: %v", out.err)
	}
	if len(out.skips) != 1 || !strings.Contains(out.skips[0], "blob exists") {
		t.Fatalf("skips = %v, want exactly one 'blob exists' skip", out.skips)
	}
	if got := getRanges(); len(got) != 0 {
		t.Errorf("the waiting item issued %d range GETs (%v), want 0: the published blob must be reused", len(got), got)
	}
	assertExactFile(t, repo.BlobPath(sha), full)
	assertOwnershipContractLinks(t, cacheDir, "model.bin")
}

// Cancellation must interrupt a foreign-owner wait without reporting success or deleting that owner's partial.
func TestDownloadCancelWhileWaitingForForeignProcessOwnerAbortsPromptly(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--model", "blobs")
	dst := mustDomainDst(t, repo, sha)
	partial := dst + ".part"
	live := []byte("the live partial of the other process")
	if err := os.WriteFile(partial, live, 0o644); err != nil {
		t.Fatal(err)
	}

	// The child process owns the domain and holds it. It never publishes.
	child := startBlobOwnershipChild(t, blobOwnershipChildConfig{
		DomainDst:   dst,
		PartialPath: partial,
		Payload:     string(live),
		ReadyPath:   filepath.Join(blobsDir, "child-ready"),
		GoPath:      filepath.Join(blobsDir, "child-go"),
	})
	waitForFile(t, filepath.Join(blobsDir, "child-ready"), 60*time.Second)
	defer releaseBlobOwnershipChild(t, child, filepath.Join(blobsDir, "child-go"))

	srv, getRanges := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "1MB")
	// Enable explicit-cancel cleanup to verify it still preserves a foreign owner's partial.
	cfg.CleanupPartialsOnCancel = func() bool { return true }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The probe is installed before the writer starts: the hook is a
	// package-level variable.
	contended := installDomainContentionProbe(t)
	result := make(chan ownershipContractOutcome, 1)
	go func() { result <- runOwnershipContractDownload(ctx, cfg) }()

	awaitContention(t, contended, "the blob key's ownership domain")

	cancelled := time.Now()
	cancel()

	var out ownershipContractOutcome
	select {
	case out = <-result:
	case <-time.After(60 * time.Second):
		t.Fatal("the download did not abort after the cancel")
	}
	elapsed := time.Since(cancelled)
	if elapsed > 30*time.Second {
		t.Errorf("the wait took %v to abort after the cancel, want far less than the 5 minute budget", elapsed)
	}
	if out.err == nil {
		t.Fatalf("the download reported success although it was cancelled (skips=%v)", out.skips)
	}
	if !errors.Is(out.err, context.Canceled) {
		t.Errorf("Download error = %v, want the cancellation", out.err)
	}
	if len(out.skips) != 0 {
		t.Errorf("skips = %v, want none: the file is not present", out.skips)
	}
	if got := getRanges(); len(got) != 0 {
		t.Errorf("the item issued %d range GETs (%v), want 0: it never owned the key", len(got), got)
	}
	got, err := os.ReadFile(partial)
	if err != nil {
		t.Fatalf("the live partial of the other process was removed: %v", err)
	}
	if string(got) != string(live) {
		t.Errorf("the live partial holds %q, want %q", got, live)
	}
	if _, serr := os.Stat(repo.BlobPath(sha)); !os.IsNotExist(serr) {
		t.Errorf("a blob was published although the item never owned the key, stat err=%v", serr)
	}
}
