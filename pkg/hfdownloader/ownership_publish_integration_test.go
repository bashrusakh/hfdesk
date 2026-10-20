// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the sixth correction set (issue #140 / PR #151):
//   - publish must never overwrite a content-addressed blob that another writer
//     completed while the transfer was in flight;
//   - a free OS ownership lock is authoritative for a lock-fenced record, even
//     when the record carries a reused/live PID, so Download resumes instead of
//     skipping a crash state;
//   - a fallback writer that lost its claim must not poison a successor's
//     payload;
//   - single-path final-byte transport errors finalize instead of planning an
//     invalid range; file_finalizing precedes the proof and exact-size inherited
//     partials emit a full-progress reading;
//   - a skipped same-SHA/different-path item still gets its own snapshot and
//     friendly entries after the winner publishes;
//   - a known digest with an unknown expected size is proven before publication;
//   - a successful single-path download removes obsolete legacy multipart
//     artifacts, and cleanup removes the atomic record temp.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func blobDirFor(t *testing.T, cacheDir string) string {
	t.Helper()
	dir := filepath.Join(cacheDir, "hub", "models--owner--model", "blobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func lfsTreeNode(path string, size int, sha string) string {
	return fmt.Sprintf(`{"type":"file","path":%q,"size":%d,"lfs":{"oid":%q,"size":%d,"sha256":%q}}`,
		path, size, sha, size, sha)
}

// TestPublishKeepsMidFlightContentAddressedBlob covers B1: another writer
// completes blobs/<sha> between claim and publish. The publish guard must keep
// the good blob and treat this item as a completed skip.
func TestPublishKeepsMidFlightContentAddressedBlob(t *testing.T) {
	const total = 64 * 1024
	good := rangedTestPayload(total)
	bad := corruptSameSize(good)
	sha := singleTestSHA(good)

	cacheDir := t.TempDir()
	blobPath := filepath.Join(blobDirFor(t, cacheDir), sha)
	var seeded sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.HasSuffix(r.URL.Path, "/tree/main"):
			_, _ = w.Write([]byte("[" + lfsTreeNode("model.bin", total, sha) + "]"))
		case strings.Contains(r.URL.Path, "/resolve/"):
			// Seed the good blob on the first ranged GET, i.e. after the claim
			// and while this transfer is in flight. Seeding on the HEAD would
			// let the claim observe it and skip before transferring.
			if r.Method == http.MethodGet && r.Header.Get("Range") != "" {
				seeded.Do(func() {
					if werr := os.WriteFile(blobPath, good, 0o644); werr != nil {
						t.Errorf("seed blob: %v", werr)
					}
				})
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(bad))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := Settings{
		CacheDir:           cacheDir,
		Endpoint:           srv.URL,
		Concurrency:        4,
		MaxActiveDownloads: 1,
		MultipartThreshold: "1KB",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
	var doneMessage string
	err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ev ProgressEvent) {
		if ev.Event == "done" {
			doneMessage = ev.Message
		}
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertExactFile(t, blobPath, good)
	if !strings.Contains(doneMessage, "skipped 1") {
		t.Errorf("mid-flight completion was not treated as a skip: %q", doneMessage)
	}
	blobsDir := filepath.Dir(blobPath)
	for _, name := range []string{sha + ".incomplete", sha + ".incomplete.meta"} {
		if _, serr := os.Stat(filepath.Join(blobsDir, name)); !os.IsNotExist(serr) {
			t.Errorf("staging artifact %s survived the completed-skip publish, stat err=%v", name, serr)
		}
	}
}

// TestDownload_FreeLockFencedRecordWithLivePIDResumes covers B2: a lock-fenced
// record whose owner lock is free is stale even when the record carries our
// live PID and a fresh heartbeat. Download must resume and publish, not skip.
func TestDownload_FreeLockFencedRecordWithLivePIDResumes(t *testing.T) {
	const total = 64 * 1024
	good := rangedTestPayload(total)
	sha := singleTestSHA(good)
	cacheDir := t.TempDir()
	blobsDir := blobDirFor(t, cacheDir)
	staging := filepath.Join(blobsDir, sha+".incomplete")
	ranges := []RangeProgress{{Start: 0, End: total - 1, Committed: total / 2}}
	seedRangedState(t, staging, "o/r", "model.bin", total, ranges, good)
	meta, err := readStagingMeta(staging + ".meta")
	if err != nil {
		t.Fatal(err)
	}
	// A foreign instance token with our live PID and a fresh record: the lease
	// heuristic alone would call this "downloading". The free lock says the
	// owner is gone.
	meta.Instance = "foreign-reused-pid"
	meta.PID = os.Getpid()
	meta.Host = processHostname
	meta.Commit = "commit123"
	meta.SHA256 = sha
	meta.LockFenced = true
	meta.ProofPending = false
	if err := writeStagingMeta(staging+".meta", meta); err != nil {
		t.Fatal(err)
	}
	// The persistent lock file exists but is held by nobody.
	if err := os.WriteFile(stagingOwnershipLockPath(staging), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	srv, gets := proofRecoveryHub(t, good)
	cfg := Settings{
		CacheDir:           cacheDir,
		Endpoint:           srv.URL,
		Concurrency:        1,
		MaxActiveDownloads: 1,
		MultipartThreshold: "1KB",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
	var doneMessage string
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ev ProgressEvent) {
		if ev.Event == "done" {
			doneMessage = ev.Message
		}
	}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if gets.Load() == 0 {
		t.Error("the resumable crash record was skipped instead of resumed")
	}
	assertExactFile(t, filepath.Join(blobsDir, sha), good)
	if !strings.Contains(doneMessage, "downloaded 1") {
		t.Errorf("resume was not counted as a download: %q", doneMessage)
	}
}

// TestPoisonStagingSkipsLostFallbackClaim covers B3: on the lease fallback an
// old writer that lost its claim must not truncate the successor's payload.
func TestPoisonStagingSkipsLostFallbackClaim(t *testing.T) {
	const size = 128
	dir := t.TempDir()
	staging := filepath.Join(dir, "x.incomplete")
	metaPath := staging + ".meta"
	successor := bytes.Repeat([]byte("successor-payload"), 8)[:size]
	if err := os.WriteFile(staging, successor, 0o644); err != nil {
		t.Fatal(err)
	}
	foreign := IncompleteMeta{
		PID:       99999999,
		Instance:  "successor-instance",
		Host:      processHostname,
		StartedAt: time.Now().UTC(),
		Size:      size,
		Version:   stagingMetaVersion,
		Repo:      "o/r",
		Path:      "x.bin",
		Ranges:    buildRangeLayout(size, 1),
	}
	if err := writeStagingMeta(metaPath, &foreign); err != nil {
		t.Fatal(err)
	}
	d := &stagingDownload{
		ctx:  context.Background(),
		stg:  staging,
		meta: metaPath,
		it:   PlanItem{RelativePath: "x.bin", Size: size},
		cfg:  Settings{Verify: "size"},
	}
	d.poisonStagingIfOwned()
	assertExactFile(t, staging, successor)

	// A record still owned by this process is poisoned under the lock.
	ours := foreign
	ours.Instance = processInstanceToken
	if err := writeStagingMeta(metaPath, &ours); err != nil {
		t.Fatal(err)
	}
	d.poisonStagingIfOwned()
	fi, err := os.Stat(staging)
	if err != nil {
		t.Fatalf("staging payload: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("owned staging payload was not poisoned: size=%d, want 0", fi.Size())
	}
}

// TestDownloadSingle_FinalByteTransportErrorFinalizes covers M1: all expected
// bytes reached disk before the transport error; the single path must finalize
// rather than retrying an invalid bytes=size-(size-1) range (416).
func TestDownloadSingle_FinalByteTransportErrorFinalizes(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	var requests atomic.Int32
	var badRanges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if rng := r.Header.Get("Range"); rng != "" {
			badRanges.Add(1)
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		// Chunked body: deliver all bytes, then abort without the terminating
		// chunk so the client sees a transport error after the final byte.
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = w.Write(full)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "final-byte.bin")
	it := PlanItem{RelativePath: "final-byte.bin", URL: srv.URL, Size: total}
	cfg := Settings{Verify: "size", Retries: 1, BackoffInitial: "1ms", BackoffMax: "1ms", StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v (the final-byte error must finalize, not retry a bad range)", err)
	}
	assertExactFile(t, dst, full)
	if got := requests.Load(); got != 1 {
		t.Errorf("issued %d requests; want 1 (no retry after the final byte)", got)
	}
	if got := badRanges.Load(); got != 0 {
		t.Errorf("issued %d invalid bytes=size-(size-1) ranges; want 0", got)
	}
}

// TestDownloadSingle_EmitsFinalizingBeforeProofAndFullProgress covers M2: an
// exact-size inherited partial must emit a full-progress reading and
// file_finalizing before the pre-publish proof/rename.
func TestDownloadSingle_EmitsFinalizingBeforeProofAndFullProgress(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	dir := t.TempDir()
	dst := filepath.Join(dir, "inherited.bin")
	if err := os.WriteFile(dst+".part", full, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no network request expected for a complete .part", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var events []ProgressEvent
	it := PlanItem{RelativePath: "inherited.bin", URL: srv.URL, Size: total, LFS: true, SHA256: sha}
	cfg := Settings{Verify: "size", Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ev ProgressEvent) {
		events = append(events, ev)
	}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}
	fullProgressAt, finalizingAt := -1, -1
	for i, ev := range events {
		if ev.Event == "file_progress" && ev.Downloaded == total && ev.Total == total && fullProgressAt == -1 {
			fullProgressAt = i
		}
		if ev.Event == "file_finalizing" && finalizingAt == -1 {
			finalizingAt = i
		}
	}
	if fullProgressAt == -1 {
		t.Fatal("exact-size inherited partial emitted no full-progress reading")
	}
	if finalizingAt == -1 {
		t.Fatal("single path emitted no file_finalizing event")
	}
	if fullProgressAt > finalizingAt {
		t.Errorf("full progress (index %d) was emitted after file_finalizing (index %d)", fullProgressAt, finalizingAt)
	}
	assertExactFile(t, dst, full)
}

// TestDownload_SharedBlobSkippedPathsGetLinks covers M4: two plan items with
// the same SHA and different paths skip while a live owner holds the shared
// staging. After that owner publishes, both paths still get their own snapshot
// and friendly entries. A third, different-SHA item keeps Download alive until
// the shared blob is published, so the post-wg.Wait reconcile can run.
func TestDownload_SharedBlobSkippedPathsGetLinks(t *testing.T) {
	const total = 64 * 1024
	shared := rangedTestPayload(total)
	blocker := append([]byte(nil), shared...)
	blocker[0] ^= 0xFF
	sharedSHA := singleTestSHA(shared)
	blockerSHA := singleTestSHA(blocker)

	cacheDir := t.TempDir()
	blobsDir := blobDirFor(t, cacheDir)
	sharedBlob := filepath.Join(blobsDir, sharedSHA)
	sharedStaging := filepath.Join(blobsDir, sharedSHA+".incomplete")

	release := make(chan struct{})
	var releaseOnce, startedOnce sync.Once
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.HasSuffix(r.URL.Path, "/tree/main"):
			_, _ = w.Write([]byte("[" +
				lfsTreeNode("a/model.bin", total, sharedSHA) + "," +
				lfsTreeNode("b/model.bin", total, sharedSHA) + "," +
				lfsTreeNode("blocker.bin", total, blockerSHA) + "]"))
		case strings.Contains(r.URL.Path, "/resolve/") && strings.HasSuffix(r.URL.Path, "/blocker.bin"):
			if r.Method == http.MethodGet {
				startedOnce.Do(func() {
					select {
					case started <- struct{}{}:
					default:
					}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				})
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(blocker))
		case strings.Contains(r.URL.Path, "/resolve/"):
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(shared))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := Settings{
		CacheDir:           cacheDir,
		Endpoint:           srv.URL,
		Concurrency:        1,
		MaxActiveDownloads: 3,
		MultipartThreshold: "1KB",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}

	// A live owner (this test) holds the shared staging, so both same-SHA paths
	// skip via BlobDownloading while the blob is still absent.
	state, err := claimStaging(context.Background(), sharedStaging, sharedBlob, sharedStaging+".meta",
		PlanItem{RelativePath: "a/model.bin", Size: total, SHA256: sharedSHA},
		Job{Repo: "owner/model"}, "commit123", cfg, true)
	if err != nil {
		t.Fatalf("pre-claim shared staging: %v", err)
	}
	f, err := os.OpenFile(sharedStaging, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		state.release()
		t.Fatal(err)
	}
	if err := f.Truncate(total); err != nil {
		f.Close()
		state.release()
		t.Fatal(err)
	}
	f.Close()

	skipCh := make(chan struct{}, 4)
	done := make(chan error, 1)
	go func() {
		done <- Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ev ProgressEvent) {
			if ev.Event == "file_done" && strings.Contains(ev.Message, "skip") {
				select {
				case skipCh <- struct{}{}:
				default:
				}
			}
		})
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		releaseOnce.Do(func() { close(release) })
		state.release()
		t.Fatal("the blocker transfer never started")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-skipCh:
		case <-time.After(10 * time.Second):
			releaseOnce.Do(func() { close(release) })
			state.release()
			t.Fatal("the same-SHA paths did not skip while the shared staging was owned")
		}
	}

	// The live owner publishes the shared blob; only then is the blocker
	// released so Download's post-wait reconcile sees BlobComplete.
	state.release()
	if err := os.WriteFile(sharedBlob, shared, 0o644); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatalf("Download: %v", err)
	}

	repoDir := filepath.Join(cacheDir, "hub", "models--owner--model")
	for _, rel := range []string{"a/model.bin", "b/model.bin"} {
		snap := filepath.Join(repoDir, "snapshots", "commit123", filepath.FromSlash(rel))
		if _, err := os.Lstat(snap); err != nil {
			t.Errorf("snapshot entry %s missing: %v", rel, err)
		}
		friendly := filepath.Join(cacheDir, "models", "owner", "model", filepath.FromSlash(rel))
		if _, err := os.Lstat(friendly); err != nil {
			t.Errorf("friendly entry %s missing: %v", rel, err)
		}
	}
	assertExactFile(t, sharedBlob, shared)
	assertExactFile(t, filepath.Join(blobsDir, blockerSHA), blocker)
}

// TestDownloadSingle_UnknownSizeKnownDigestRequiresProof covers the minor: a
// known digest with an unknown expected size cannot rely on a size check, so it
// must be proven before the rename even in size mode.
func TestDownloadSingle_UnknownSizeKnownDigestRequiresProof(t *testing.T) {
	const total = 4_096
	good := rangedTestPayload(total)
	bad := corruptSameSize(good)
	sha := singleTestSHA(good)
	cases := []struct {
		name      string
		served    []byte
		wantError bool
	}{
		{name: "mismatch", served: bad, wantError: true},
		{name: "match", served: good, wantError: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "unknown.bin")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tc.served)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(tc.served)
			}))
			defer srv.Close()
			it := PlanItem{RelativePath: "unknown.bin", URL: srv.URL, Size: 0, LFS: true, SHA256: sha}
			cfg := Settings{Verify: "size", Retries: 0, StallTimeout: "0"}
			hashPassCount.Store(0)
			verified, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
			if got := hashPassCount.Load(); got != 1 {
				t.Errorf("unknown-size known-digest transfer performed %d full-file passes; want exactly 1", got)
			}
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "sha256 verify failed") {
					t.Fatalf("error = %v, want a sha256 verification failure", err)
				}
				if verified {
					t.Error("a failed unknown-size proof reported contentVerified")
				}
				if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
					t.Errorf("unproven unknown-size bytes were published, stat err=%v", serr)
				}
				return
			}
			if err != nil {
				t.Fatalf("downloadSingle: %v", err)
			}
			if !verified {
				t.Error("unknown-size proof did not report contentVerified")
			}
			assertExactFile(t, dst, good)
		})
	}
}

// TestDownloadSingle_RemovesLegacyMultipartArtifacts covers the minor: a
// successful single-path finalization clears an obsolete .part-NN/.parts.json
// family for the same destination.
func TestDownloadSingle_RemovesLegacyMultipartArtifacts(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	dir := t.TempDir()
	dst := filepath.Join(dir, "single.bin")
	if err := os.WriteFile(dst+".part-00", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst+".parts.json", []byte(`{"size":4096}`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	}))
	defer srv.Close()
	it := PlanItem{RelativePath: "single.bin", URL: srv.URL, Size: total}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, Settings{Verify: "size", Retries: 0, StallTimeout: "0"}, it, dst, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}
	assertExactFile(t, dst, full)
	for _, p := range []string{dst + ".part-00", dst + ".parts.json"} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("legacy multipart artifact %s survived single-path success, stat err=%v", filepath.Base(p), serr)
		}
	}
}

// TestRemoveStagingArtifactsRemovesRecordTemp covers the atomic record-temp
// cleanup: a crashed meta rewrite must not leave <staging>.meta.tmp behind.
func TestRemoveStagingArtifactsRemovesRecordTemp(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "x.incomplete")
	metaPath := staging + ".meta"
	for _, p := range []string{staging, metaPath, metaPath + ".tmp"} {
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removeStagingArtifactsLocked(staging, metaPath)
	for _, p := range []string{staging, metaPath, metaPath + ".tmp"} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("%s survived cleanup, stat err=%v", filepath.Base(p), serr)
		}
	}
}
