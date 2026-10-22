// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Cross-process coverage of the blob key's ownership domain (issue #140 /
// PR #151).
//
// The shipped suite emulates a foreign owner IN-PROCESS for the
// publish/release/hold-past-budget cases: the ownership lock belongs to the
// OPEN FILE DESCRIPTION, not to the process, so a second open in this process
// conflicts exactly like another process's writer does. The cases below use a
// REAL child process instead, so the cross-process path is covered by
// execution and not only by that equivalence argument.
//
// Two behaviors are covered:
//
//   - a foreign owner that PUBLISHES the blob while the item waits: the item
//     reuses the published blob with ZERO additional range GETs, finishes as
//     an ordinary "blob exists" skip with its own cache links, and the job
//     reports success;
//   - a cancel while the item WAITS: the wait aborts promptly (far below the
//     5 minute budget) and the foreign owner's live partial survives
//     untouched.
//
// The synchronization is deterministic: the parent proves that its item is
// waiting through the production contention hook, and the child acts only
// after the parent asks it to.

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

// encodeOwnershipPayload encodes a binary payload for the child process's
// JSON configuration.
func encodeOwnershipPayload(payload []byte) string {
	return base64.StdEncoding.EncodeToString(payload)
}

// TestDownloadWaitsForForeignProcessOwnerThatPublishesAndReusesTheBlob
// covers the cross-process publish case with a REAL child process as the
// owner of the blob key's ownership domain.
//
// The item must WAIT for the domain, and when the owner publishes the blob
// the item must finish as an ordinary "blob exists" skip: the file is
// present, the item's own cache links exist, NO additional range GET is
// issued, and the job reports success.
//
// Both routes are covered: the multipart route (which stages in the blob key's
// staging file) and the single-request route (which stages in
// blobs/tmp-<sha>.part). The single-request route is the discriminating one
// for the re-check after the domain: without it, that route has no
// claim-level re-check to fall back on and would transfer the file a second
// time.
func TestDownloadWaitsForForeignProcessOwnerThatPublishesAndReusesTheBlob(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	for _, tc := range []struct {
		name      string
		threshold string
		// partial is the live partial the foreign owner writes: the blob
		// key's staging file on the multipart route, the single-request
		// staging name on the single-request route.
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

// testDownloadWaitsForForeignProcessOwnerThatPublishes is the body of the
// cross-process publish case, once per route.
func testDownloadWaitsForForeignProcessOwnerThatPublishes(t *testing.T, threshold string,
	partial func(t *testing.T, repo *RepoDir, sha string) string) {
	t.Helper()
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--model", "blobs")

	// The child process owns the blob key's ownership domain: it holds the
	// lifetime OS lock of the key and writes a live partial.
	child := startBlobOwnershipChild(t, blobOwnershipChildConfig{
		DomainDst:   mustDomainDst(t, repo, sha),
		PartialPath: partial(t, repo, sha),
		Payload:     "the live partial of the other process",
		ReadyPath:   filepath.Join(blobsDir, "child-ready"),
		GoPath:      filepath.Join(blobsDir, "child-go"),
		// The child publishes the blob only after the parent asks for it,
		// so the blob can never predate the wait.
		PublishPath:          repo.BlobPath(sha),
		PublishPayloadBase64: encodeOwnershipPayload(full),
		PublishGoPath:        filepath.Join(blobsDir, "child-publish"),
	})
	waitForFile(t, filepath.Join(blobsDir, "child-ready"), 60*time.Second)
	defer releaseBlobOwnershipChild(t, child, filepath.Join(blobsDir, "child-go"))

	srv, getRanges := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, threshold)

	// The probe is installed before the writer starts: the hook is a
	// package-level variable.
	contended := installDomainContentionProbe(t)
	result := make(chan ownershipContractOutcome, 1)
	go func() { result <- runOwnershipContractDownload(context.Background(), cfg) }()

	// The item must wait for the domain instead of skipping the file.
	awaitContention(t, contended, "the blob key's ownership domain")
	select {
	case out := <-result:
		t.Fatalf("the download finished while the foreign owner still held the domain (err=%v, skips=%v)",
			out.err, out.skips)
	default:
	}

	// The foreign owner publishes the blob and then releases the domain.
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
	// The file is present and the item's own links exist.
	assertExactFile(t, repo.BlobPath(sha), full)
	assertOwnershipContractLinks(t, cacheDir, "model.bin")
}

// TestDownloadCancelWhileWaitingForForeignProcessOwnerAbortsPromptly covers
// the cancel path of the domain wait with a REAL child process as the owner
// of the blob key's ownership domain.
//
// The wait is bounded by the 5 minute budget, but a cancel must abort it
// promptly: the item gives up the domain wait, the job does not report
// success, and the foreign owner's LIVE partial survives the cancel untouched
// (the cancelled job never owned the key, so it must not remove the owner's
// bytes).
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
	// An explicit cancel cleans the partials of the cancelled job.
	cfg.CleanupPartialsOnCancel = func() bool { return true }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The probe is installed before the writer starts: the hook is a
	// package-level variable.
	contended := installDomainContentionProbe(t)
	result := make(chan ownershipContractOutcome, 1)
	go func() { result <- runOwnershipContractDownload(ctx, cfg) }()

	// The item waits for the domain of the foreign owner.
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
	// The foreign owner's live partial survived the cancel, byte for byte.
	got, err := os.ReadFile(partial)
	if err != nil {
		t.Fatalf("the live partial of the other process was removed: %v", err)
	}
	if string(got) != string(live) {
		t.Errorf("the live partial holds %q, want %q", got, live)
	}
	// Nothing was published: the item never owned the key.
	if _, serr := os.Stat(repo.BlobPath(sha)); !os.IsNotExist(serr) {
		t.Errorf("a blob was published although the item never owned the key, stat err=%v", serr)
	}
}
