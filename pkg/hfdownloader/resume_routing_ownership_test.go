// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Correction #7 regressions (issue #140 / PR #151):
//   - a fallback (LockFenced=false) record with a live foreign owner must not
//     be displaced by a lock-capable process in claim or cleanup;
//   - a lock-fenced record with a free lock remains take-over-able;
//   - usable legacy or versioned staging state keeps the multipart route when
//     the item is ranged-capable even below the configured threshold;
//   - without range support the single path warns, then removes the obsolete
//     versioned staging pair only after the final file is durable and
//     claim-checked;
//   - an ignored-Range 200 restart clears the inherited-bytes proof flag.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingHub declares one file (LFS or plain) with the SHA-256 of full and
// serves full for payload requests. It records every ranged GET.
func recordingHub(t *testing.T, full []byte, rel string, lfs bool) (*httptest.Server, func() []string) {
	t.Helper()
	sha := singleTestSHA(full)
	var (
		mu     sync.Mutex
		ranges []string
	)
	node := fmt.Sprintf(`{"type":"file","path":%q,"size":%d,"sha256":%q}`, rel, len(full), sha)
	if lfs {
		node = fmt.Sprintf(`{"type":"file","path":%q,"size":%d,"lfs":{"oid":%q,"size":%d,"sha256":%q}}`,
			rel, len(full), sha, len(full), sha)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.HasSuffix(r.URL.Path, "/tree/main"):
			_, _ = w.Write([]byte("[" + node + "]"))
		case strings.Contains(r.URL.Path, "/resolve/") || strings.Contains(r.URL.Path, "/raw/"):
			if r.Method == http.MethodGet {
				mu.Lock()
				ranges = append(ranges, r.Header.Get("Range"))
				mu.Unlock()
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), ranges...)
	}
}

func legacyOutputDst(t *testing.T, out string) string {
	t.Helper()
	base, err := DestinationBase(out, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(base, "model.bin")
}

func TestClaimStaging_FallbackLiveOwnerIsNotTakenOver(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	foreign := os.Getppid()
	if foreign == os.Getpid() || !isProcessAlive(foreign) {
		t.Skip("no live foreign PID available for this test process")
	}
	dir := t.TempDir()
	staging := filepath.Join(dir, "fallback.bin.incomplete")
	metaPath := staging + ".meta"
	pub := filepath.Join(dir, "fallback.bin")
	if err := os.WriteFile(staging, full, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{
		PID:        foreign,
		Instance:   "fallback-live-instance",
		Host:       processHostname,
		StartedAt:  time.Now().UTC(),
		Size:       total,
		SHA256:     sha,
		Version:    stagingMetaVersion,
		Repo:       "o/r",
		Commit:     "commit123",
		Path:       "fallback.bin",
		Ranges:     buildRangeLayout(total, 2),
		LockFenced: false,
	}
	if err := writeStagingMeta(metaPath, &meta); err != nil {
		t.Fatal(err)
	}
	beforeMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	it := PlanItem{RelativePath: "fallback.bin", Size: total, LFS: true, SHA256: sha}
	state, err := claimStaging(context.Background(), staging, pub, metaPath, it, Job{Repo: "o/r"}, "commit123",
		Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}, false)
	if state != nil {
		state.release()
	}
	if !errors.Is(err, errBlobIncompleteOwned) {
		t.Fatalf("claim of a fallback record with a live foreign owner = %v, want errBlobIncompleteOwned", err)
	}
	assertExactFile(t, staging, full)
	afterMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeMeta, afterMeta) {
		t.Error("the live fallback owner's record was rewritten")
	}
	if _, serr := os.Stat(pub); !os.IsNotExist(serr) {
		t.Errorf("the fallback owner's staging was published, stat err=%v", serr)
	}
}

func TestClaimStaging_LockFencedFreeLockIsTakenOver(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	foreign := os.Getppid()
	if foreign == os.Getpid() || !isProcessAlive(foreign) {
		t.Skip("no live foreign PID available for this test process")
	}
	dir := t.TempDir()
	staging := filepath.Join(dir, "fenced.bin.incomplete")
	metaPath := staging + ".meta"
	pub := filepath.Join(dir, "fenced.bin")
	ranges := []RangeProgress{{Start: 0, End: total/2 - 1, Committed: total / 2, Done: true}, {Start: total / 2, End: total - 1}}
	seedRangedState(t, staging, "o/r", "fenced.bin", total, ranges, full)
	meta, err := readStagingMeta(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	meta.PID = foreign
	meta.Instance = "lock-fenced-original"
	meta.Host = processHostname
	meta.Commit = "commit123"
	meta.SHA256 = sha
	meta.LockFenced = true
	if err := writeStagingMeta(metaPath, meta); err != nil {
		t.Fatal(err)
	}
	// The persistent lock file exists but is held by nobody.
	if err := os.WriteFile(stagingOwnershipLockPath(staging), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	it := PlanItem{RelativePath: "fenced.bin", Size: total, LFS: true, SHA256: sha}
	state, err := claimStaging(context.Background(), staging, pub, metaPath, it, Job{Repo: "o/r"}, "commit123",
		Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}, false)
	if err != nil {
		t.Fatalf("lock-fenced record with a free lock must be take-over-able: %v", err)
	}
	defer state.release()
	if state.fresh {
		t.Error("take-over reported a fresh claim")
	}
	if len(state.ranges) != 2 || state.ranges[0].Committed != total/2 {
		t.Errorf("take-over ranges = %+v, want the validated committed prefix", state.ranges)
	}
}

func TestCleanupJobPartFiles_FallbackLiveOwnerSurvives(t *testing.T) {
	const total = 4_096
	foreign := os.Getppid()
	if foreign == os.Getpid() || !isProcessAlive(foreign) {
		t.Skip("no live foreign PID available for this test process")
	}
	cacheDir := t.TempDir()
	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--model", "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("f", 64)
	dst := filepath.Join(blobsDir, "tmp-"+sha)
	staging := filepath.Join(blobsDir, sha+".incomplete")
	metaPath := staging + ".meta"
	if err := os.WriteFile(staging, make([]byte, total), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{
		PID:        foreign,
		Instance:   "fallback-live-instance",
		Host:       processHostname,
		StartedAt:  time.Now().UTC(),
		Size:       total,
		Version:    stagingMetaVersion,
		Repo:       "o/r",
		Path:       "x.bin",
		Ranges:     buildRangeLayout(total, 2),
		LockFenced: false,
	}
	if err := writeStagingMeta(metaPath, &meta); err != nil {
		t.Fatal(err)
	}
	if err := CleanupJobPartFiles(Settings{CacheDir: cacheDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	for _, p := range []string{staging, metaPath} {
		if _, serr := os.Stat(p); serr != nil {
			t.Errorf("cleanup removed a live fallback owner's %s: %v", filepath.Base(p), serr)
		}
	}
}

func TestDownload_LowThresholdLegacyLayoutResumesMultipart(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	srv, getRanges := recordingHub(t, full, "model.bin", true)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)
	layout := buildMultipartResumeLayout(total, 2, total/2)
	if err := writeMultipartResumeLayout(dst, layout); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fmt.Sprintf("%s.part-%02d", dst, 0), full[:total/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fmt.Sprintf("%s.part-%02d", dst, 1), full[total/2:total/2+total/4], 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := Settings{
		OutputDir:          out,
		Endpoint:           srv.URL,
		Concurrency:        2,
		MaxActiveDownloads: 1,
		MultipartThreshold: "1MB", // would route the 4 KiB item to the single path
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
	hashPassCount.Store(0)
	if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertExactFile(t, dst, full)
	want := fmt.Sprintf("bytes=%d-%d", total/2+total/4, total-1)
	if got := getRanges(); len(got) != 1 || got[0] != want {
		t.Errorf("ranged GETs = %v, want exactly the missing suffix %q", got, want)
	}
	if got := hashPassCount.Load(); got != 1 {
		t.Errorf("legacy migration proof performed %d full-file passes; want exactly 1", got)
	}
	for _, p := range []string{dst + ".parts.json", fmt.Sprintf("%s.part-%02d", dst, 0), fmt.Sprintf("%s.part-%02d", dst, 1)} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("legacy artifact %s survived finalization, stat err=%v", filepath.Base(p), serr)
		}
	}
}

func TestDownload_LowThresholdVersionedPairResumesMultipart(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	srv, getRanges := recordingHub(t, full, "model.bin", true)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)
	staging := dst + ".incomplete"
	metaPath := staging + ".meta"
	seedRangedState(t, staging, "o/r", "model.bin", total,
		[]RangeProgress{{Start: 0, End: total - 1, Committed: total / 2}}, full)
	meta, err := readStagingMeta(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	meta.Commit = "commit123"
	meta.SHA256 = sha
	meta.LockFenced = true
	if err := writeStagingMeta(metaPath, meta); err != nil {
		t.Fatal(err)
	}

	cfg := Settings{
		OutputDir:          out,
		Endpoint:           srv.URL,
		Concurrency:        1,
		MaxActiveDownloads: 1,
		MultipartThreshold: "1MB",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
	if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertExactFile(t, dst, full)
	want := fmt.Sprintf("bytes=%d-%d", total/2, total-1)
	if got := getRanges(); len(got) != 1 || got[0] != want {
		t.Errorf("ranged GETs = %v, want exactly the uncommitted suffix %q", got, want)
	}
	for _, p := range []string{staging, metaPath} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("%s survived a successful resume publish, stat err=%v", filepath.Base(p), serr)
		}
	}
}

func TestDownload_NoRangeSupportWarnsAndCleansVersionedResumeState(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	srv, getRanges := recordingHub(t, full, "model.bin", false) // no LFS -> AcceptRanges=false
	out := t.TempDir()
	dst := legacyOutputDst(t, out)
	staging := dst + ".incomplete"
	metaPath := staging + ".meta"
	seedRangedState(t, staging, "o/r", "model.bin", total,
		[]RangeProgress{{Start: 0, End: total/2 - 1, Committed: total / 2}, {Start: total / 2, End: total - 1}}, full)
	meta, err := readStagingMeta(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	meta.Commit = "commit123"
	meta.SHA256 = sha
	if err := writeStagingMeta(metaPath, meta); err != nil {
		t.Fatal(err)
	}

	cfg := Settings{
		OutputDir:          out,
		Endpoint:           srv.URL,
		Concurrency:        1,
		MaxActiveDownloads: 1,
		MultipartThreshold: "1MB",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
	var warning string
	if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ev ProgressEvent) {
		if ev.Level == "warn" && strings.Contains(ev.Message, "without HTTP range support") {
			warning = ev.Message
		}
	}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if warning == "" {
		t.Error("no explicit warning for unusable resume state without range support")
	}
	assertExactFile(t, dst, full)
	if got := getRanges(); len(got) != 1 || got[0] != "" {
		t.Errorf("payload GETs = %v, want one full (non-ranged) GET on the single path", got)
	}
	for _, p := range []string{staging, metaPath} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("obsolete versioned resume artifact %s survived a successful download, stat err=%v", filepath.Base(p), serr)
		}
	}
}

func TestDownloadSingle_IgnoredRangeRestartClearsInheritedProof(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	dir := t.TempDir()
	dst := filepath.Join(dir, "restart.bin")
	if err := os.WriteFile(dst+".part", full[:100], 0o644); err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		ranges []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		// Ignore the requested range and return the whole body with 200.
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(full)
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "restart.bin", URL: srv.URL, Size: total, LFS: true, SHA256: sha}
	hashPassCount.Store(0)
	if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"},
		Settings{Verify: "size", Retries: 0, StallTimeout: "0"}, it, dst, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}
	assertExactFile(t, dst, full)
	if got := hashPassCount.Load(); got != 0 {
		t.Errorf("ignored-Range restart performed %d inherited-bytes proof passes; want 0", got)
	}
	mu.Lock()
	gotRanges := append([]string(nil), ranges...)
	mu.Unlock()
	want := fmt.Sprintf("bytes=%d-%d", 100, total-1)
	if len(gotRanges) != 1 || gotRanges[0] != want {
		t.Errorf("requests = %v, want the initial resume range %q", gotRanges, want)
	}
}
