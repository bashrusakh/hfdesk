// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Trust-model regressions for the issue #140 ranged downloader: staging reuse
// (T1, already covered by ranged_state_test.go), legacy-migration content proof
// (T2), content-addressed completion (T3), serialized ownership (T4, see
// staging_lock_test.go), cancel/phase semantics (T5) and mode compatibility
// (T6). The fixtures use only the pre-correction public/package API so the same
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
		case strings.HasSuffix(r.URL.Path, "/tree/main"):
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

// T2: valid-looking legacy .part-NN bytes for a *moved* commit (the resume
// identity is the current resolved commit; only the digest can prove content)
// must not be published in size mode. The forced one-time migration hash
// detects the mismatch, discards the staging data, and a retry re-downloads.
func TestDownloadMultipart_LegacyPartsRequireContentProof(t *testing.T) {
	const total = 8000
	full := rangedTestPayload(total)
	sum := sha256.Sum256(full)
	sha := hex.EncodeToString(sum[:])

	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "moved-commit.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	// Structurally valid legacy layout, wrong content (a previous commit's
	// bytes that happen to have the same geometry).
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(fmt.Sprintf("%s.part-%02d", dst, i), bytes.Repeat([]byte{0xFF}, total/2), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeMultipartResumeLayout(dst, buildMultipartResumeLayout(total, 2, total/2)); err != nil {
		t.Fatal(err)
	}

	srv, gotRanges := rangedTestServer(t, full)
	it := PlanItem{RelativePath: "moved-commit.bin", URL: srv.URL + "/moved-commit.bin", Size: total, AcceptRanges: true, LFS: true, SHA256: sha}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "size"}
	opts := multipartOpts{Commit: "resolved-new-commit"}

	hashPassCount.Store(0)
	hashPassBytes.Store(0)
	verified, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, opts, func(ProgressEvent) {})
	if err == nil {
		t.Fatalf("wrong-content legacy bytes were published in size mode (verified=%v)", verified)
	}
	if verified {
		t.Error("a failed content proof reported the payload as verified")
	}
	if got := hashPassCount.Load(); got != 1 {
		t.Errorf("legacy migration performed %d full-file hash passes; want exactly 1", got)
	}
	if len(*gotRanges) != 0 {
		t.Errorf("the first run fetched %v; fully committed legacy ranges must be content-proven, not re-downloaded", *gotRanges)
	}
	for _, p := range []string{dst, dst + ".incomplete", dst + ".incomplete.meta"} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("corrupt migrated state survived the content-proof failure: %s (stat err=%v)", filepath.Base(p), serr)
		}
	}

	// Retry policy: the discard leaves no poison, so a fresh transfer publishes
	// the exact bytes.
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, opts, func(ProgressEvent) {}); err != nil {
		t.Fatalf("retry after the content-proof failure: %v", err)
	}
	assertExactFile(t, dst, full)
}

// T2: the single .part prefix variant of the same rule.
func TestDownloadMultipart_LegacySinglePartRequiresContentProof(t *testing.T) {
	const total = 8000
	full := rangedTestPayload(total)
	sum := sha256.Sum256(full)
	sha := hex.EncodeToString(sum[:])

	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "single-wrong.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".part", bytes.Repeat([]byte{0xFF}, total), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, gotRanges := rangedTestServer(t, full)
	it := PlanItem{RelativePath: "single-wrong.bin", URL: srv.URL + "/single-wrong.bin", Size: total, AcceptRanges: true, LFS: true, SHA256: sha}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "size"}

	hashPassCount.Store(0)
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err == nil {
		t.Fatal("wrong-content single .part bytes were published in size mode")
	}
	if got := hashPassCount.Load(); got != 1 {
		t.Errorf("legacy single-.part migration performed %d hash passes; want exactly 1", got)
	}
	if len(*gotRanges) != 0 {
		t.Errorf("fully committed single .part was re-downloaded before the content proof: %v", *gotRanges)
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Error("corrupt single-.part bytes were published")
	}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("retry after the content-proof failure: %v", err)
	}
	assertExactFile(t, dst, full)
}

// T2: an oversized single .part is not a prefix anyone can vouch for. It must
// be discarded (never clamped) and the file downloaded from offset 0.
func TestDownloadMultipart_OversizedSinglePartIsDiscarded(t *testing.T) {
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
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}
	assertExactFile(t, dst, full)
	if len(*gotRanges) == 0 {
		t.Fatal("the oversized .part was clamped instead of discarded; nothing was fetched")
	}
	if _, serr := os.Stat(dst + ".part"); !os.IsNotExist(serr) {
		t.Errorf("the invalid oversized .part survived, stat err=%v", serr)
	}
}

// T2: the LegacyMigrated marker is persisted in the record, so a crash after
// the migration flush still forces the content proof on resume.
func TestDownloadMultipart_LegacyMigratedFlagSurvivesResume(t *testing.T) {
	const total = 8000
	full := rangedTestPayload(total)
	sum := sha256.Sum256(full)
	sha := hex.EncodeToString(sum[:])

	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "flag.bin")
	staging := dst + ".incomplete"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	// The staged payload looks complete (all ranges Done) but is wrong; the
	// legacy artifacts that produced it are already gone (the crash happened
	// after the migrated layout became durable). Raw JSON keeps the fixture
	// readable by the pre-correction code, which ignores the new field.
	if err := os.WriteFile(staging, bytes.Repeat([]byte{0xFF}, total), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(
		`{"pid":%d,"instance":%q,"host":%q,"started":"2025-01-01T00:00:00Z","size":%d,"sha256":%q,`+
			`"version":%d,"repo":"o/r","path":"flag.bin","commit":"c1",`+
			`"ranges":[{"start":0,"end":%d,"committed":%d,"done":true}],"legacy_migrated":true}`,
		os.Getpid(), processInstanceToken, processHostname, total, sha, stagingMetaVersion, total-1, total)
	if err := os.WriteFile(staging+".meta", []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, gotRanges := rangedTestServer(t, full)
	it := PlanItem{RelativePath: "flag.bin", URL: srv.URL + "/flag.bin", Size: total, AcceptRanges: true, LFS: true, SHA256: sha}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "size"}

	hashPassCount.Store(0)
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{Commit: "c1"}, func(ProgressEvent) {}); err == nil {
		t.Fatal("a resumed LegacyMigrated record skipped its content proof")
	}
	if got := hashPassCount.Load(); got != 1 {
		t.Errorf("resumed legacy record performed %d hash passes; want 1", got)
	}
	if len(*gotRanges) != 0 {
		t.Errorf("a fully committed record was re-downloaded before the proof: %v", *gotRanges)
	}
	for _, p := range []string{dst, staging, staging + ".meta"} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("%s survived the failed content proof, stat err=%v", filepath.Base(p), serr)
		}
	}
}

// T2: in strict sha256 mode the migration proof and the normal strict pass are
// the same single pass (never two), and a mismatch still never publishes.
func TestDownloadMultipart_LegacyMigrationSHA256ModeUsesOnePass(t *testing.T) {
	const total = 8000
	full := rangedTestPayload(total)
	sum := sha256.Sum256(full)
	sha := hex.EncodeToString(sum[:])

	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "strict-migrate.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(fmt.Sprintf("%s.part-%02d", dst, i), bytes.Repeat([]byte{0xFF}, total/2), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeMultipartResumeLayout(dst, buildMultipartResumeLayout(total, 2, total/2)); err != nil {
		t.Fatal(err)
	}

	srv, gotRanges := rangedTestServer(t, full)
	it := PlanItem{RelativePath: "strict-migrate.bin", URL: srv.URL + "/strict-migrate.bin", Size: total, AcceptRanges: true, LFS: true, SHA256: sha}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "sha256"}
	hashPassCount.Store(0)
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err == nil {
		t.Fatal("wrong-content legacy bytes were published in sha256 mode")
	}
	if got := hashPassCount.Load(); got != 1 {
		t.Errorf("sha256-mode legacy migration performed %d hash passes; want exactly 1", got)
	}
	if len(*gotRanges) != 0 {
		t.Errorf("committed legacy ranges were re-downloaded in strict mode: %v", *gotRanges)
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Error("corrupt legacy bytes were published in strict mode")
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
		PID:       os.Getpid(),
		Instance:  processInstanceToken,
		Host:      processHostname,
		StartedAt: time.Now().UTC(),
		Size:      total,
		SHA256:    strings.Repeat("a", 64),
		Version:   stagingMetaVersion,
		Repo:      "o/r",
		Path:      "hash-cancel.bin",
		Commit:    "c1",
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
		_, err := downloadMultipart(ctx, srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{Commit: "c1"}, func(ev ProgressEvent) {
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

// T5: the legacy migration copy is context-aware. The deterministic signal is
// the staging payload's first byte: cancel as soon as the copy has started, then
// assert the final source byte was never reached (an unbounded copy would have
// written the whole source).
func TestDownloadMultipart_CancelDuringLegacyMigrationIsPrompt(t *testing.T) {
	const partSize = 64 << 20
	runCase := func(t *testing.T, cleanup bool) {
		tmpDir := t.TempDir()
		dst := filepath.Join(tmpDir, "migrate-cancel.bin")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst+".part", bytes.Repeat([]byte{0xFF}, partSize), 0o644); err != nil {
			t.Fatal(err)
		}
		staging := dst + ".incomplete"

		srv := headOnlyServer(t, partSize+1)
		it := PlanItem{RelativePath: "migrate-cancel.bin", URL: srv.URL + "/migrate-cancel.bin", Size: partSize + 1, AcceptRanges: true}
		cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "size"}
		if cleanup {
			cfg.CleanupPartialsOnCancel = func() bool { return true }
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := downloadMultipart(ctx, srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {})
			done <- err
		}()

		// Wait until the migration copy has written its first byte.
		deadline := time.Now().Add(10 * time.Second)
		buf := make([]byte, 1)
		for {
			if f, err := os.Open(staging); err == nil {
				_, rerr := f.ReadAt(buf, 0)
				f.Close()
				if rerr == nil && buf[0] == 0xFF {
					break
				}
			}
			select {
			case err := <-done:
				t.Fatalf("download finished before the migration copy started: %v", err)
			case <-time.After(200 * time.Microsecond):
			}
			if time.Now().After(deadline) {
				t.Fatal("legacy migration copy never started")
			}
		}

		cancelAt := time.Now()
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			if elapsed := time.Since(cancelAt); elapsed > 2*time.Second {
				t.Errorf("cancel mid-migration took %v", elapsed)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("download did not return after cancel mid-migration")
		}

		if cleanup {
			for _, p := range []string{staging, staging + ".meta", dst + ".part"} {
				if _, serr := os.Stat(p); !os.IsNotExist(serr) {
					t.Errorf("explicit cancel mid-migration left %s behind (stat err=%v)", filepath.Base(p), serr)
				}
			}
			return
		}

		// The unbounded copy would have written every source byte. Only a
		// context-aware copy can leave the tail unwritten.
		f, err := os.Open(staging)
		if err != nil {
			t.Fatalf("staging payload missing after the aborted migration: %v", err)
		}
		defer f.Close()
		if _, err := f.ReadAt(buf, partSize-1); err != nil {
			t.Fatalf("read staging tail: %v", err)
		}
		if buf[0] != 0 {
			t.Fatal("the legacy migration copy ran to completion after cancel; the copy is not context-aware")
		}
	}

	t.Run("aborts the copy", func(t *testing.T) { runCase(t, false) })
	t.Run("runs cancel cleanup", func(t *testing.T) { runCase(t, true) })
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
