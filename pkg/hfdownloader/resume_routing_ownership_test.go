// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers ownership fencing, resumable-route selection, and cleanup timing when
// range support is unavailable or an ignored-Range response restarts transfer.

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

// recordingHub declares one LFS/plain file with full's digest and serves full.
// It records each payload GET's Range header, including empty full-body headers.
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
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte("[" + node + "]"))
		case strings.Contains(r.URL.Path, "/resolve/") || strings.Contains(r.URL.Path, "/raw/"):
			if r.Method == http.MethodGet && r.Header.Get("X-HFDesk-Addressing-Probe") != "1" {
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
	_, err = claimStaging(context.Background(), staging, pub, metaPath, it, Job{Repo: "o/r"}, "commit123", true,
		Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}, false, acquireTestDomain(t, pub))
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

func TestClaimStaging_LegacyLockFencedRecordRemainsUnproven(t *testing.T) {
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
	// The old payload-side lock may be free; LockFenced alone is not a
	// proof for the current external realm/key protocol.
	if err := os.WriteFile(staging+stagingLockSuffix, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	beforePayload, err := os.ReadFile(staging)
	if err != nil {
		t.Fatal(err)
	}
	beforeMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	it := PlanItem{RelativePath: "fenced.bin", Size: total, LFS: true, SHA256: sha}
	_, err = claimStaging(context.Background(), staging, pub, metaPath, it, Job{Repo: "o/r"}, "commit123", true,
		Settings{Concurrency: 2, Retries: 0, StallTimeout: "0"}, false, acquireTestDomain(t, pub))
	if !errors.Is(err, ErrStagingOwnershipUnproven) {
		t.Fatalf("legacy lock-fenced record error = %v, want ownership unproven", err)
	}
	afterPayload, err := os.ReadFile(staging)
	if err != nil || !bytes.Equal(afterPayload, beforePayload) {
		t.Errorf("legacy payload changed: %v", err)
	}
	afterMeta, err := os.ReadFile(metaPath)
	if err != nil || !bytes.Equal(afterMeta, beforeMeta) {
		t.Errorf("legacy metadata changed: %v", err)
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

// Pre-#140 part families are not resume state: an above-size threshold must
// keep the single route, fetch from zero and clean up the obsolete family.
func TestDownload_LowThresholdOldMultipartFamilyIsReDownloaded(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	srv, getRanges := recordingHub(t, full, "model.bin", true)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)
	// A valid-looking pre-#140 layout plus two parts; both are ignored.
	if err := os.WriteFile(multipartLayoutMetaPath(dst),
		[]byte(fmt.Sprintf(`{"size":%d,"concurrency":2,"parts":[]}`, total)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fmt.Sprintf("%s.part-%02d", dst, 0), full[:total/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fmt.Sprintf("%s.part-%02d", dst, 1), full[total/2:], 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := Settings{
		OutputDir:          out,
		Endpoint:           srv.URL,
		Concurrency:        2,
		MaxActiveDownloads: 1,
		MultipartThreshold: "1MB",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
	hashPassCount.Store(0)
	if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertExactFile(t, dst, full)
	if got := getRanges(); len(got) != 1 || got[0] != "" {
		t.Errorf("payload GETs = %v, want exactly one full-body request (no old-format resume)", got)
	}
	if got := hashPassCount.Load(); got != 0 {
		t.Errorf("old-format artifacts triggered %d content proofs; want 0", got)
	}
	for _, p := range []string{multipartLayoutMetaPath(dst), fmt.Sprintf("%s.part-%02d", dst, 0), fmt.Sprintf("%s.part-%02d", dst, 1)} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("obsolete pre-#140 artifact %s survived finalization, stat err=%v", filepath.Base(p), serr)
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
	it := PlanItem{RelativePath: "restart.bin", Size: total, LFS: true, SHA256: sha}
	// The staged prefix is inheritable, so the ignored-Range restart is what
	// discards it and clears the inherited-bytes proof.
	seedSinglePartial(t, dst, full[:100], Job{Repo: "o/r"}, it, singleTestIdentity)
	var (
		mu     sync.Mutex
		ranges []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(full)
	}))
	defer srv.Close()

	it.URL = srv.URL
	hashPassCount.Store(0)
	if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"},
		Settings{Verify: "size", Retries: 0, StallTimeout: "0"}, it, dst, singleTestIdentity,
		func(ProgressEvent) {}); err != nil {
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

// nonRangeHub serves one non-LFS file per path in rels with the same payload
// bytes. Planning sets AcceptRanges=false for non-LFS items, so every file
// of the plan is a no-range item.
func nonRangeHub(t *testing.T, full []byte, rels []string) *httptest.Server {
	t.Helper()
	sha := singleTestSHA(full)
	nodes := make([]string, 0, len(rels))
	for _, rel := range rels {
		nodes = append(nodes, fmt.Sprintf(`{"type":"file","path":%q,"size":%d,"sha256":%q}`, rel, len(full), sha))
	}
	tree := "[" + strings.Join(nodes, ",") + "]"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte(tree))
		case strings.Contains(r.URL.Path, "/resolve/") || strings.Contains(r.URL.Path, "/raw/"):
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// flatDst returns the flat-mode destination for rel under out for o/r.
func flatDst(t *testing.T, out, rel string) string {
	t.Helper()
	base, err := DestinationBase(out, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(base, rel)
}

// seedFlatResumeState writes a pinned current-format versioned pair at
// dst+".incomplete" whose identity matches this test's plan (repo o/r,
// commit123, path rel, size total, the payload's digest).
func seedFlatResumeState(t *testing.T, dst, rel string, total int64, full []byte) {
	t.Helper()
	staging := dst + ".incomplete"
	seedRangedState(t, staging, "o/r", rel, total,
		[]RangeProgress{{Start: 0, End: total/2 - 1, Committed: total / 2}, {Start: total / 2, End: total - 1}}, full)
	meta, err := readStagingMeta(staging + ".meta")
	if err != nil {
		t.Fatal(err)
	}
	meta.Commit = "commit123"
	meta.SHA256 = singleTestSHA(full)
	if err := writeStagingMeta(staging+".meta", meta); err != nil {
		t.Fatal(err)
	}
}

// collectWarnings runs Download with cfg and returns every warning event's
// message, in emission order.
func collectWarnings(t *testing.T, cfg Settings, msgs *[]string, mu *sync.Mutex) {
	t.Helper()
	if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ev ProgressEvent) {
		if ev.Event == "warning" {
			mu.Lock()
			*msgs = append(*msgs, ev.Message)
			mu.Unlock()
		}
	}); err != nil {
		t.Fatalf("Download: %v", err)
	}
}

// TestDownload_NoRangeSupportWarnsOnceForAllAffectedFiles pins the
// aggregation: three files that each carry usable resume state while the
// plan has no range support produce exactly ONE warning event, whose text
// names the count (3) and the affected paths sorted, and every file still
// completes at its final path — the transfer is unchanged.
func TestDownload_NoRangeSupportWarnsOnceForAllAffectedFiles(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	rels := []string{"f00.bin", "f01.bin", "f02.bin"}
	srv := nonRangeHub(t, full, rels)
	out := t.TempDir()
	for _, rel := range rels {
		seedFlatResumeState(t, flatDst(t, out, rel), rel, total, full)
	}

	cfg := Settings{
		OutputDir:          out,
		Endpoint:           srv.URL,
		Concurrency:        2,
		MaxActiveDownloads: 2,
		MultipartThreshold: "1MB",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
	var (
		mu       sync.Mutex
		warnings []string
	)
	collectWarnings(t, cfg, &warnings, &mu)

	mu.Lock()
	defer mu.Unlock()
	want := "resume state for 3 files cannot be used without HTTP range support; " +
		"downloading the full files and attempting to discard the obsolete state after success: " +
		"f00.bin, f01.bin, f02.bin"
	if len(warnings) != 1 {
		t.Fatalf("warning events = %d (%q), want exactly one aggregated warning", len(warnings), warnings)
	}
	if warnings[0] != want {
		t.Errorf("warning = %q, want %q", warnings[0], want)
	}
	for _, rel := range rels {
		assertExactFile(t, flatDst(t, out, rel), full)
	}
}

// TestDownload_NoRangeSupportWarningBoundsAffectedPaths pins the bound: more
// affected files than the plan notice's path cap still yield one warning
// that names the count, the first sorted paths only, and how many more were
// left unnamed — the unnamed paths stay out of the text.
func TestDownload_NoRangeSupportWarningBoundsAffectedPaths(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	rels := []string{"f00.bin", "f01.bin", "f02.bin", "f03.bin", "f04.bin", "f05.bin", "f06.bin", "f07.bin"}
	srv := nonRangeHub(t, full, rels)
	out := t.TempDir()
	for _, rel := range rels {
		seedFlatResumeState(t, flatDst(t, out, rel), rel, total, full)
	}

	cfg := Settings{
		OutputDir:          out,
		Endpoint:           srv.URL,
		Concurrency:        2,
		MaxActiveDownloads: 2,
		MultipartThreshold: "1MB",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
	var (
		mu       sync.Mutex
		warnings []string
	)
	collectWarnings(t, cfg, &warnings, &mu)

	mu.Lock()
	defer mu.Unlock()
	want := "resume state for 8 files cannot be used without HTTP range support; " +
		"downloading the full files and attempting to discard the obsolete state after success: " +
		"f00.bin, f01.bin, f02.bin, f03.bin, f04.bin, and 3 more"
	if len(warnings) != 1 {
		t.Fatalf("warning events = %d (%q), want exactly one aggregated warning", len(warnings), warnings)
	}
	if warnings[0] != want {
		t.Errorf("warning = %q, want %q", warnings[0], want)
	}
	for _, rel := range []string{"f05.bin", "f06.bin", "f07.bin"} {
		if strings.Contains(warnings[0], rel) {
			t.Errorf("warning names the unnamed path %q: %q", rel, warnings[0])
		}
	}
	for _, rel := range rels {
		assertExactFile(t, flatDst(t, out, rel), full)
	}
}

// TestDownload_NoResumeStateEmitsNoWarning pins the negative half: a
// download whose items carry no resume state emits no warning event at all,
// and the files still complete.
func TestDownload_NoResumeStateEmitsNoWarning(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	rels := []string{"f00.bin", "f01.bin"}
	srv := nonRangeHub(t, full, rels)
	out := t.TempDir()

	cfg := Settings{
		OutputDir:          out,
		Endpoint:           srv.URL,
		Concurrency:        2,
		MaxActiveDownloads: 2,
		MultipartThreshold: "1MB",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
	var (
		mu       sync.Mutex
		warnings []string
	)
	collectWarnings(t, cfg, &warnings, &mu)

	mu.Lock()
	defer mu.Unlock()
	if len(warnings) != 0 {
		t.Fatalf("warning events = %q, want none without resume state", warnings)
	}
	for _, rel := range rels {
		assertExactFile(t, flatDst(t, out, rel), full)
	}
}

// TestFormatRangelessResumeNotice pins the sentence itself: singular and
// plural counts, sorted bounded paths with the unnamed remainder stated,
// and the path cap shared with the plan notice — so a wording change cannot
// slip through and the message never grows with the number of files.
func TestFormatRangelessResumeNotice(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		want  string
	}{
		{
			name:  "one file",
			paths: []string{"only.bin"},
			want: "resume state for 1 file cannot be used without HTTP range support; " +
				"downloading the full file and attempting to discard the obsolete state after success: only.bin",
		},
		{
			name:  "three files",
			paths: []string{"a.bin", "b.bin", "c.bin"},
			want: "resume state for 3 files cannot be used without HTTP range support; " +
				"downloading the full files and attempting to discard the obsolete state after success: a.bin, b.bin, c.bin",
		},
		{
			name:  "over the cap",
			paths: []string{"p0.bin", "p1.bin", "p2.bin", "p3.bin", "p4.bin", "p5.bin", "p6.bin", "p7.bin"},
			want: "resume state for 8 files cannot be used without HTTP range support; " +
				"downloading the full files and attempting to discard the obsolete state after success: " +
				"p0.bin, p1.bin, p2.bin, p3.bin, p4.bin, and 3 more",
		},
	}
	for _, tc := range cases {
		if got := formatRangelessResumeNotice(tc.paths); got != tc.want {
			t.Errorf("%s: formatRangelessResumeNotice = %q, want %q", tc.name, got, tc.want)
		}
	}
	if splitMentionNoticePathCap != 5 {
		t.Errorf("splitMentionNoticePathCap = %d, want 5 (the messages above name five paths)", splitMentionNoticePathCap)
	}
}
