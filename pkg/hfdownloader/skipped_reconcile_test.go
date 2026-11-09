// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers final reconciliation of skipped shared blobs: job success requires
// every skipped blob to still be complete and reachable at its paths.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Removing a skipped blob after its skip decision but before final
// reconciliation must fail the job with the distinct unavailable-blob error:
// no re-transfer, no success, and not the link-failure or check-failure class.
func TestSkippedBlobRemovedBeforeReconciliationFailsTheJob(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	publishBlob(t, repo, sha, full)

	srv, gets := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")

	var (
		mu      sync.Mutex
		skips   []string
		removed bool
	)
	err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ev ProgressEvent) {
		if ev.Event != "file_done" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if !strings.Contains(ev.Message, "skip") {
			return
		}
		skips = append(skips, ev.Message)
		if !removed {
			// The skip decision recorded the link; final reconciliation has not
			// re-confirmed it yet. This simulates an external removal.
			removed = true
			if rerr := os.Remove(repo.BlobPath(sha)); rerr != nil {
				t.Errorf("remove the skipped blob: %v", rerr)
			}
		}
	})

	if err == nil {
		t.Fatalf("the job reported success although the skipped blob vanished (skips=%v)", skips)
	}
	if !errors.Is(err, ErrCacheBlobUnavailable) {
		t.Errorf("Download error = %v, want the skipped-blob-unavailable error", err)
	}
	if errors.Is(err, ErrCacheLinkUnreachable) || errors.Is(err, ErrCacheBlobCheckFailed) {
		t.Errorf("Download error = %v, want the vanished-blob class distinct from link/check failures", err)
	}
	if !containsAll(err.Error(), sha, "model.bin", "missing") {
		t.Errorf("Download error = %v, want the blob, the item path and its status in the message", err)
	}
	if len(skips) != 1 {
		t.Errorf("skips = %v, want exactly one skip decision", skips)
	}
	if got := gets(); len(got) != 0 {
		t.Errorf("the vanished skipped blob issued %d payload GETs (%v), want 0: reconciliation fails the item instead of re-transferring", len(got), got)
	}
}

// A cancellation landing just after a reconcile error was recorded must still
// report the cancellation as the job's terminal state: the recorded reconcile
// side effect is not a failure of a cancelled job. Two skipped blobs make the
// ordering deterministic — the first reconcile pass records its error, the
// second pass cancels.
func TestCancelledJobReportsCancellationNotReconcileFailure(t *testing.T) {
	const total = 4_096
	first := rangedTestPayload(total)
	second := rangedTestPayload(total)
	second[0] ^= 0xFF // distinct content, so the two files get distinct blobs
	shaFirst := singleTestSHA(first)
	shaSecond := singleTestSHA(second)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	publishBlob(t, repo, shaFirst, first)
	publishBlob(t, repo, shaSecond, second)

	node := func(rel, sha string, body []byte) string {
		return fmt.Sprintf(`{"type":"file","path":%q,"size":%d,"lfs":{"oid":%q,"size":%d,"sha256":%q}}`,
			rel, len(body), sha, len(body), sha)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte("[" + node("model.bin", shaFirst, first) + "," +
				node("aux.bin", shaSecond, second) + "]"))
		case strings.Contains(r.URL.Path, "/resolve/") || strings.Contains(r.URL.Path, "/raw/"):
			body := first
			if strings.HasSuffix(r.URL.Path, "/aux.bin") {
				body = second
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu         sync.Mutex
		skips      []string
		probeCalls int
	)
	// Both blobs vanish after their skip decision but before reconciliation,
	// so each reconcile pass resolves the staged payload's owner — and fails.
	// The second pass' probe cancels the job after the first pass' reconcile
	// error is already recorded.
	oldProbe := ownershipWriterProbe
	ownershipWriterProbe = func(context.Context, *ownershipRealm, []string) (*ownershipLease, error) {
		mu.Lock()
		probeCalls++
		n := probeCalls
		mu.Unlock()
		if n == 2 {
			cancel()
		}
		return nil, errors.New("injected ownership probe failure")
	}
	t.Cleanup(func() { ownershipWriterProbe = oldProbe })

	err := Download(ctx, Job{Repo: "owner/model", Revision: "main"}, cfg, func(ev ProgressEvent) {
		if ev.Event != "file_done" || !strings.Contains(ev.Message, "skip") {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		skips = append(skips, ev.Message)
		sha := shaFirst
		if strings.HasSuffix(ev.Path, "aux.bin") {
			sha = shaSecond
		}
		if rerr := os.Remove(repo.BlobPath(sha)); rerr != nil {
			t.Errorf("remove the skipped blob %s: %v", sha, rerr)
		}
		if werr := os.WriteFile(repo.IncompletePath(sha), []byte("partial"), 0o644); werr != nil {
			t.Errorf("stage an incomplete payload for %s: %v", sha, werr)
		}
	})

	mu.Lock()
	defer mu.Unlock()
	if len(skips) != 2 {
		t.Errorf("skips = %v, want both files skipped", skips)
	}
	if probeCalls < 2 {
		t.Fatalf("ownership probes = %d, want at least 2 so the cancellation lands after a reconcile error was recorded", probeCalls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Download error = %v, want the cancellation as the terminal state", err)
	}
	if errors.Is(err, ErrCacheBlobCheckFailed) || errors.Is(err, ErrCacheBlobUnavailable) || errors.Is(err, ErrCacheLinkUnreachable) {
		t.Errorf("cancelled job reported a reconcile failure: %v", err)
	}
}

// An ownership/verification CheckBlob error at final reconciliation must fail
// the job with the distinct check-failure error: never silently skipped, and
// neither the vanish class nor the link-failure class.
func TestReconciliationCheckErrorFailsTheJobDistinctly(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	publishBlob(t, repo, sha, full)

	// CheckBlob probes ownership only when the blob is gone but a staging
	// payload remains; inject the probe failure for that reconcile pass.
	oldProbe := ownershipWriterProbe
	ownershipWriterProbe = func(context.Context, *ownershipRealm, []string) (*ownershipLease, error) {
		return nil, errors.New("injected ownership probe failure")
	}
	t.Cleanup(func() { ownershipWriterProbe = oldProbe })

	srv, gets := recordingHub(t, full, "model.bin", true)
	cfg := ownershipContractSettings(cacheDir, srv.URL, "0")

	var (
		mu      sync.Mutex
		skipped bool
	)
	err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ev ProgressEvent) {
		if ev.Event != "file_done" || !strings.Contains(ev.Message, "skip") {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if skipped {
			return
		}
		skipped = true
		// The blob vanishes while a staging payload remains, so reconciliation
		// must resolve the owner of the incomplete download - and fail.
		if rerr := os.Remove(repo.BlobPath(sha)); rerr != nil {
			t.Errorf("remove the skipped blob: %v", rerr)
		}
		if werr := os.WriteFile(repo.IncompletePath(sha), []byte("partial"), 0o644); werr != nil {
			t.Errorf("stage an incomplete payload: %v", werr)
		}
	})

	if err == nil {
		t.Fatalf("the job reported success although the blob re-confirmation failed (skipped=%v)", skipped)
	}
	if !errors.Is(err, ErrCacheBlobCheckFailed) {
		t.Errorf("Download error = %v, want the blob check-failure error", err)
	}
	if errors.Is(err, ErrCacheBlobUnavailable) || errors.Is(err, ErrCacheLinkUnreachable) {
		t.Errorf("Download error = %v, want the check-failure class distinct from vanish/link failures", err)
	}
	if !containsAll(err.Error(), "injected ownership probe failure", "model.bin") {
		t.Errorf("Download error = %v, want the underlying cause and the item path in the message", err)
	}
	if got := gets(); len(got) != 0 {
		t.Errorf("the failed re-confirmation issued %d payload GETs (%v), want 0", len(got), got)
	}
}
