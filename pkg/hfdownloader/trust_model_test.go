// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Trust-model regressions for the issue #140 ranged downloader: staging reuse
// (T1, already covered by ranged_state_test.go), content-addressed completion
// (T3), serialized ownership (T4, see staging_lock_test.go), cancel/phase
// semantics (T5) and mode compatibility (T6). The fixtures use only the pre-correction public/package API so the same
// tests can be run against an overlay of the previous HEAD to show the failure
// they pin.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHubServerWithCount mirrors fakeHubServer (a one-LFS-file Hub) and counts
// the content GETs the plan actually transfers.
func fakeHubServerWithCount(t *testing.T, content []byte) (*httptest.Server, string, *atomic.Int32) {
	t.Helper()
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte(fmt.Sprintf(
				`[{"type":"file","path":"model.bin","size":100,"lfs":{"oid":"%s","size":%d,"sha256":"%s"}}]`,
				sha, len(content), sha)))
		case strings.Contains(r.URL.Path, "/resolve/"):
			gets.Add(1)
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, sha, &gets
}

// T3: an existing legacy/output destination is not completion proof. The
// pre-download skip check is authoritative; because it declined (wrong size or
// corrupt same-size bytes), the file must be downloaded and atomically
// renamed over the old bytes.
func TestDownload_LegacyExistingDestinationIsNotCompletion(t *testing.T) {
	const size = 4096
	full := rangedTestPayload(size)
	cases := []struct {
		name string
		seed []byte
	}{
		{"wrong size destination", bytes.Repeat([]byte{0xEE}, 20)},
		{"same size corrupt destination", bytes.Repeat([]byte{0xEE}, size)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, gets := fakeHubServerWithCount(t, full)
			outDir := t.TempDir()
			// Legacy mode prefixes the repository id under OutputDir.
			dst := filepath.Join(outDir, "o", "r", "model.bin")
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, tc.seed, 0o644); err != nil {
				t.Fatal(err)
			}

			var (
				doneMessages []string
				skipMessages []string
			)
			cfg := Settings{
				OutputDir:          outDir,
				Endpoint:           srv.URL,
				Concurrency:        2,
				MaxActiveDownloads: 2,
				MultipartThreshold: "1KB",
				Verify:             "size",
				Retries:            0,
				StallTimeout:       "0",
			}
			err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ev ProgressEvent) {
				switch ev.Event {
				case "file_done":
					doneMessages = append(doneMessages, ev.Message)
					if strings.Contains(ev.Message, "skip") {
						skipMessages = append(skipMessages, ev.Message)
					}
				}
			})
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			for _, msg := range skipMessages {
				t.Errorf("an arbitrary destination was treated as complete: %q", msg)
			}
			if gets.Load() == 0 {
				t.Error("no content was fetched; the stale destination was trusted")
			}
			assertExactFile(t, dst, full)
		})
	}
}

// T3 preserved behavior: a blob-keyed publish target is content-addressed, so
// its existence alone proves completion and the transfer is skipped.
func TestDownload_ContentAddressedBlobExistenceStillSkips(t *testing.T) {
	const size = 4096
	full := rangedTestPayload(size)
	srv, sha, gets := fakeHubServerWithCount(t, full)

	cacheDir := t.TempDir()
	blobsDir := filepath.Join(cacheDir, "hub", "models--o--r", "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobsDir, sha), full, 0o644); err != nil {
		t.Fatal(err)
	}

	var doneMessages []string
	cfg := Settings{
		CacheDir:           cacheDir,
		Endpoint:           srv.URL,
		Concurrency:        2,
		MaxActiveDownloads: 2,
		MultipartThreshold: "1KB",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
	if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ev ProgressEvent) {
		if ev.Event == "file_done" {
			doneMessages = append(doneMessages, ev.Message)
		}
	}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if gets.Load() != 0 {
		t.Errorf("content-addressed blob that already exists was re-downloaded (%d GETs)", gets.Load())
	}
	if len(doneMessages) != 1 || !strings.Contains(doneMessages[0], "skip (blob exists)") {
		t.Errorf("completion skip was not reported: %v", doneMessages)
	}
	snap := filepath.Join(cacheDir, "hub", "models--o--r", "snapshots", "commit123", "model.bin")
	if _, err := os.Lstat(snap); err != nil {
		t.Errorf("snapshot entry missing for the skipped complete blob: %v", err)
	}
}

// T3: an HF-cache tmp-<sha> destination for an item without a canonical digest
// must not be treated as complete by the claim. A leftover tmp file from an
// interrupted earlier run is stale: the file is downloaded again and renamed
// over it.
func TestDownloadMultipart_TempDestinationIsNotCompletion(t *testing.T) {
	const total = 8000
	full := rangedTestPayload(total)
	tmpDir := t.TempDir()
	blobsDir := filepath.Join(tmpDir, "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(blobsDir, "tmp-"+strings.Repeat("a", 64))
	if err := os.WriteFile(dst, bytes.Repeat([]byte{0xEE}, 20), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, gotRanges := rangedTestServer(t, full)
	it := PlanItem{RelativePath: "x.bin", URL: srv.URL + "/x.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "size"}
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		if errors.Is(err, errBlobAlreadyComplete) {
			t.Fatalf("claim treated a leftover tmp destination as a complete blob")
		}
		t.Fatalf("downloadMultipart: %v", err)
	}
	if len(*gotRanges) == 0 {
		t.Fatal("no range was fetched; the leftover temp destination was trusted")
	}
	assertExactFile(t, dst, full)
}

// T2: an oversized single .part is not a prefix anyone can vouch for. The
// single-request path must discard it (never clamp it) and download the file
// from offset 0; the multipart route neither reads nor removes it.
func TestDownloadSingle_OversizedPartialIsDiscarded(t *testing.T) {
	const total = 8000
	full := rangedTestPayload(total)
	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "oversized.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".part", bytes.Repeat([]byte{0xFF}, 20000), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, gotRanges := rangedTestServer(t, full)
	it := PlanItem{RelativePath: "oversized.bin", URL: srv.URL + "/oversized.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "size"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}
	assertExactFile(t, dst, full)
	if got := *gotRanges; len(got) != 1 || got[0] != "" {
		t.Errorf("payload GETs = %v, want one whole-file request (the oversized partial must be discarded)", got)
	}
	if _, serr := os.Stat(dst + ".part"); !os.IsNotExist(serr) {
		t.Errorf("the invalid oversized .part survived, stat err=%v", serr)
	}
}

// headOnlyServer serves HEAD metadata for size, so a download can reach the
// finalization phase without any range body being requested.
func headOnlyServer(t *testing.T, size int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			http.Error(w, "body must not be requested", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("Accept-Ranges", "bytes")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// T5: cancel during the strict hash pass. The shared cancel semantics must stop
// the heartbeat, close the staging handle and run cleanupPartialsOnCancel,
// instead of leaving the staging pair behind.
func TestDownloadMultipart_CancelDuringStrictHashCleansUp(t *testing.T) {
	// A sparse 1 GiB payload keeps the single hash pass running far longer than
	// the 2 ms cancel delay while costing almost no disk space.
	const total = int64(1) << 30
	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "hash-cancel.bin")
	staging := dst + ".incomplete"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(staging, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(total); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	mid := total / 2
	meta := IncompleteMeta{
		PID:          os.Getpid(),
		Instance:     processInstanceToken,
		Host:         processHostname,
		StartedAt:    time.Now().UTC(),
		Size:         total,
		SHA256:       strings.Repeat("a", 64),
		Version:      stagingMetaVersion,
		Repo:         "o/r",
		Path:         "hash-cancel.bin",
		Commit:       "c1",
		CommitPinned: true,
		Ranges: []RangeProgress{
			{Start: 0, End: mid - 1, Committed: mid, Done: true},
			{Start: mid, End: total - 1, Committed: total - mid, Done: true},
		},
	}
	if err := writeStagingMeta(staging+".meta", &meta); err != nil {
		t.Fatal(err)
	}

	srv := headOnlyServer(t, total)
	it := PlanItem{RelativePath: "hash-cancel.bin", URL: srv.URL + "/hash-cancel.bin", Size: total, AcceptRanges: true, LFS: true, SHA256: meta.SHA256}
	cfg := Settings{
		Concurrency:             2,
		Retries:                 0,
		StallTimeout:            "0",
		Verify:                  "sha256",
		CleanupPartialsOnCancel: func() bool { return true },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reached := make(chan struct{})
	var once atomic.Bool
	done := make(chan error, 1)
	hashPassCount.Store(0)
	go func() {
		_, err := downloadMultipart(ctx, srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{Commit: "c1", CommitPinned: true}, func(ev ProgressEvent) {
			if ev.Event == "file_finalizing" && once.CompareAndSwap(false, true) {
				close(reached)
			}
		})
		done <- err
	}()
	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("download finished before reaching the strict hash: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("download never reached the finalization phase")
	}
	time.Sleep(2 * time.Millisecond)
	cancelAt := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(cancelAt); elapsed > 2*time.Second {
			t.Errorf("strict-hash cancel took %v; the hash pass is not context-aware", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("download did not return after cancel during the strict hash")
	}
	if got := hashPassCount.Load(); got == 0 {
		t.Fatal("the strict hash pass never started; the fixture did not exercise the cancel path")
	}
	for _, p := range []string{staging, staging + ".meta"} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("explicit cancel during the strict hash left %s behind (stat err=%v)", filepath.Base(p), serr)
		}
	}
}

// T6: etag preserves the historical LFS behavior — a known-SHA LFS file was
// hashed in every mode before the ETag branch, so it still is (exactly once).
func TestDownload_VerifyETagHashesKnownLFS(t *testing.T) {
	const size = 64 * 1024
	full := rangedTestPayload(size)
	srv, sha, _ := fakeHubServer(t, full, nil)

	cacheDir := t.TempDir()
	cfg := Settings{
		CacheDir:           cacheDir,
		Endpoint:           srv.URL,
		Concurrency:        4,
		MaxActiveDownloads: 2,
		MultipartThreshold: "1KB",
		Verify:             "etag",
		Retries:            0,
		StallTimeout:       "0",
	}
	hashPassCount.Store(0)
	hashPassBytes.Store(0)
	if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := hashPassCount.Load(); got != 1 {
		t.Errorf("etag with a known-SHA LFS file performed %d hash passes; want the historical single pass", got)
	}
	blob := filepath.Join(cacheDir, "hub", "models--o--r", "blobs", sha)
	assertExactFile(t, blob, full)
}
