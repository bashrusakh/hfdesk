// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Cross-checks ownership, publication, resume, and cleanup across transfer
// routes, including progress and cache-link outcomes.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// A blob published after claim but before publish must remain the winner.
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
		case strings.Contains(r.URL.Path, "/tree/"):
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

// A free lock makes a fenced record stale even when its recorded PID is live.
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
	// The foreign instance token prevents treating our PID as self; the free
	// domain lock is authoritative because this record is fenced.
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
	// An obsolete staging lock file may remain, but the destination-domain lock is free.
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

// A writer that lost an unfenced claim must not truncate its successor's data.
func TestPoisonStagingSkipsLostUnfencedClaim(t *testing.T) {
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

	// The same-instance record permits poisoning under the metadata gate.
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

// If a response ends before terminal framing, revalidate the final byte even
// when all requested bytes arrived.
func TestDownloadSingle_FinalByteRequiresResponseFraming(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	var requests atomic.Int32
	var badRanges atomic.Int32
	var lastRange atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			if r.Header.Get("Range") != "" {
				badRanges.Add(1)
				http.Error(w, "unexpected initial range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			// Chunked body: deliver all bytes, then abort without the
			// terminating chunk so the final byte lacks framing proof.
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			_, _ = w.Write(full)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		rng := r.Header.Get("Range")
		lastRange.Store(rng)
		if rng != fmt.Sprintf("bytes=%d-%d", total-1, total-1) {
			badRanges.Add(1)
			http.Error(w, "unexpected retry range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", total-1, total-1, total))
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(full[total-1:])
	}))
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "final-byte.bin")
	it := PlanItem{RelativePath: "final-byte.bin", URL: srv.URL, Size: total}
	cfg := Settings{Verify: "size", Retries: 1, BackoffInitial: "1ms", BackoffMax: "1ms", StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, singleOpts{}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v (clean final-byte framing retry should recover)", err)
	}
	assertExactFile(t, dst, full)
	if got := requests.Load(); got != 2 {
		t.Errorf("issued %d requests; want initial unframed body plus final-byte revalidation", got)
	}
	if got, _ := lastRange.Load().(string); got != fmt.Sprintf("bytes=%d-%d", total-1, total-1) {
		t.Errorf("retry range=%q, want only the unframed final byte", got)
	}
	if got := badRanges.Load(); got != 0 {
		t.Errorf("issued %d invalid ranges; want 0", got)
	}
}

// An exact-size inherited partial reports full progress and finalizing before
// its pre-publication proof.
func TestDownloadSingle_EmitsFinalizingBeforeProofAndFullProgress(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	dir := t.TempDir()
	dst := filepath.Join(dir, "inherited.bin")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no network request expected for a complete .part", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var events []ProgressEvent
	it := PlanItem{RelativePath: "inherited.bin", URL: srv.URL, Size: total, LFS: true, SHA256: sha}
	// The complete partial is published without a request only when its
	// identity record proves the bytes belong to this item and records a
	// cleanly-framed body.
	seedSinglePartial(t, dst, full, Job{Repo: "o/r"}, it, completeSingleIdentity())
	cfg := Settings{Verify: "size", Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, singleTestIdentity, func(ev ProgressEvent) {
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

// Same-SHA items with different paths wait for publication and then get their
// own links; another item keeps the job active during the wait.
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

	release := make(chan struct{})
	var releaseOnce, startedOnce sync.Once
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
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

	// Hold the shared key before either same-SHA path is scheduled.
	repo, err := NewHFCache(cacheDir).Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	domain, err := blobDomainAcquire(context.Background(), sharedBlob, mustDomainDst(t, repo, sharedSHA))
	if err != nil {
		t.Fatalf("pre-claim shared domain: %v", err)
	}
	defer domain.release()

	waiting := make(chan struct{}, 4)
	oldContention := blobDomainContentionHook
	blobDomainContentionHook = func(string) {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}
	defer func() { blobDomainContentionHook = oldContention }()

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
		t.Fatal("the blocker transfer never started")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-waiting:
		case <-time.After(30 * time.Second):
			releaseOnce.Do(func() { close(release) })
			t.Fatal("the same-SHA paths did not wait for the ownership domain")
		}
	}

	if err := os.WriteFile(sharedBlob, shared, 0o644); err != nil {
		releaseOnce.Do(func() { close(release) })
		t.Fatal(err)
	}
	domain.release()
	releaseOnce.Do(func() { close(release) })

	for i := 0; i < 2; i++ {
		select {
		case <-skipCh:
		case <-time.After(30 * time.Second):
			t.Fatal("the same-SHA paths did not skip after the owner published the blob")
		}
	}
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

// A known digest with unknown size requires proof before rename, including in
// size mode.
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
			verified, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, singleOpts{}, func(ProgressEvent) {})
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

func TestDownloadUnknownSizeLFSStoresSinglePathFallbackCorrectly(t *testing.T) {
	const total = 4096
	good := rangedTestPayload(total)
	bad := corruptSameSize(good)
	sha := singleTestSHA(good)
	cases := []struct {
		name      string
		verify    string
		body      []byte
		wantError bool
	}{
		{name: "size mode known digest", verify: "size", body: good},
		{name: "sha256 mode", verify: "sha256", body: good},
		{name: "digest mismatch stays unpublished", verify: "sha256", body: bad, wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cacheDir := t.TempDir()
			var gets atomic.Int32
			var heads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/revision/main"):
					_, _ = w.Write([]byte(`{"sha":"commit-unknown-size"}`))
				case strings.Contains(r.URL.Path, "/tree/"):
					// Both tree Size and LFS.Size are intentionally omitted.
					node := fmt.Sprintf(`{"type":"file","path":"unknown.bin","lfs":{"oid":%q,"sha256":%q}}`, sha, sha)
					_, _ = w.Write([]byte("[" + node + "]"))
				case strings.Contains(r.URL.Path, "/resolve/") && r.Method == http.MethodHead:
					// The HEAD advertises range support but omits Content-Length too.
					heads.Add(1)
					w.Header().Set("Accept-Ranges", "bytes")
				case strings.Contains(r.URL.Path, "/resolve/") && r.Method == http.MethodGet:
					gets.Add(1)
					if r.Header.Get("Range") != "" {
						http.Error(w, "unknown-size single fallback must not range", http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)))
					_, _ = w.Write(tc.body)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			cfg := Settings{
				CacheDir: cacheDir, Endpoint: srv.URL, MultipartThreshold: "0",
				Concurrency: 2, MaxActiveDownloads: 1, Verify: tc.verify, Retries: 0,
			}
			job := Job{Repo: "owner/model", Revision: "main"}
			var completedPlan Plan
			cfg.OnPlanComplete = func(plan Plan) { completedPlan = plan }
			hashPassCount.Store(0)
			err := Download(context.Background(), job, cfg, nil)
			if len(completedPlan.Items) != 1 || completedPlan.Items[0].Size != 0 {
				t.Fatalf("fixture did not retain unknown plan size: %+v", completedPlan.Items)
			}
			if gets.Load() != 1 {
				t.Fatalf("GET count=%d, want one non-ranged fetch", gets.Load())
			}
			if heads.Load() == 0 {
				t.Fatal("unknown-size multipart probe did not exercise a HEAD response without Content-Length")
			}
			if hashPassCount.Load() != 1 {
				t.Fatalf("unknown-size known-digest verification performed %d passes, want one", hashPassCount.Load())
			}

			repo, rerr := NewHFCache(cacheDir).Repo(job.Repo, RepoTypeModel)
			if rerr != nil {
				t.Fatal(rerr)
			}
			blob := repo.BlobPath(sha)
			snapshot, rerr := repo.SnapshotPath("commit-unknown-size", "unknown.bin")
			if rerr != nil {
				t.Fatal(rerr)
			}
			friendly := filepath.Join(repo.FriendlyPath(), "unknown.bin")
			tmp := filepath.Join(repo.BlobsDir(), "tmp-"+sha)
			for _, p := range []string{tmp, tmp + ".part", repo.IncompletePath(sha), repo.IncompleteMetaPath(sha)} {
				if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
					t.Errorf("temporary artifact %s remains (err=%v)", p, statErr)
				}
			}
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "sha256 verify failed") {
					t.Fatalf("mismatch error=%v, want prepublication SHA failure", err)
				}
				for _, p := range []string{blob, snapshot, friendly} {
					if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
						t.Errorf("mismatched content published at %s (err=%v)", p, statErr)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			for _, p := range []string{blob, snapshot, friendly} {
				if _, statErr := os.Stat(p); statErr != nil {
					t.Errorf("expected cache destination %s missing: %v", p, statErr)
				}
			}
			assertExactFile(t, blob, good)
		})
	}
}

// Cleanup must remove the atomic record rewrite's <staging>.meta.tmp residue.
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
