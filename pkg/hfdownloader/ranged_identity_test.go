// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Resume identity of the RANGED staging path (issue #140 follow-up).
//
// The ranged path stages its bytes in <dst>.incomplete and describes them in
// the versioned record <dst>.incomplete.meta. A record authorizes a resume
// only when it identifies ONE immutable object: the same repository, the same
// item path, the same expected size and the same RESOLVED commit - and the
// identity must be PINNED on both sides (the record's own flag and the
// current plan's). An unpinned commit is the planner's fallback to the
// requested revision: the content behind that name can move between two runs,
// so bytes staged under it may never be published as the new revision's
// content. This is the same rule the single-request path applies.
//
// The regressions below cover the decision table:
//
//	(a) a record with an unpinned identity (the flag false, or absent) is
//	    discarded and the payload re-downloaded, never published;
//	(b) a record whose CURRENT plan is unpinned is discarded as well;
//	(c) a pinned record that matches repo/commit/path/size is still inherited,
//	    and only the missing suffix is requested (no over-refusal);
//	(d) a pinned record whose commit differs is discarded - the commit-string
//	    dimension is covered by
//	    TestRangedMultipart_ResolvedCommitMismatchDiscardsStaging;
//	(e) end to end through Download: an item with NO known digest whose commit
//	    could not be resolved does not inherit staged bytes;
//	(f) the negative control: with the pinned rule removed, (a) publishes the
//	    stale bytes.

import (
	"bytes"
	"context"
	"encoding/json"
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

// staleRangedPayload is the content an earlier run of a moved branch staged.
// It differs from the payload the server serves now, so inherited bytes are
// observable in the published file.
func staleRangedPayload(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = 0xA5
	}
	return b
}

// requestTouchesStart reports whether any recorded request starts at byte 0.
// A re-download of the whole file always does; inheriting a committed prefix
// never does.
func requestTouchesStart(ranges []string) bool {
	for _, rh := range ranges {
		if strings.HasPrefix(rh, "bytes=0-") {
			return true
		}
	}
	return false
}

// writeRecordWithoutPinnedFlag rewrites the record so the pinned flag is
// absent, exactly like a record written before the field existed.
func writeRecordWithoutPinnedFlag(t *testing.T, metaPath string) {
	t.Helper()
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["commit_pinned"]; !ok {
		t.Fatalf("the record does not carry a pinned flag: %s", raw)
	}
	delete(doc, "commit_pinned")
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRangedResumeRejectsAnUnpinnedIdentity is regression (a)+(b): a staging
// record whose identity is not PINNED on both sides is discarded, and the
// payload is downloaded again. The stale bytes an earlier run staged must
// never be published as this item's content.
//
// The item carries no known digest, so nothing can content-prove the
// published bytes either: in the default Verify=size mode an inherited
// stale prefix would be published silently.
func TestRangedResumeRejectsAnUnpinnedIdentity(t *testing.T) {
	const total = 8000
	fresh := rangedTestPayload(total)
	stale := staleRangedPayload(total)

	cases := []struct {
		name string
		// record rewrites the seeded record on disk.
		record func(t *testing.T, metaPath string)
		// planPinned is the current plan's pinned identity.
		planPinned bool
	}{
		{
			name: "the record is not pinned",
			record: func(t *testing.T, metaPath string) {
				meta, err := readStagingMeta(metaPath)
				if err != nil {
					t.Fatal(err)
				}
				meta.CommitPinned = false
				if err := writeStagingMeta(metaPath, meta); err != nil {
					t.Fatal(err)
				}
			},
			planPinned: true,
		},
		{
			name:       "the record carries no pinned flag",
			record:     writeRecordWithoutPinnedFlag,
			planPinned: true,
		},
		{
			name:       "the current plan is not pinned",
			record:     func(t *testing.T, metaPath string) {},
			planPinned: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "unpinned.bin")
			staging := dst + ".incomplete"
			// The earlier run committed the first half of the payload and
			// stopped. Those bytes are stale: the branch moved.
			ranges := []RangeProgress{
				{Start: 0, End: total/2 - 1, Committed: total / 2, Done: true},
				{Start: total / 2, End: total - 1},
			}
			seedRangedState(t, staging, "o/r", "unpinned.bin", total, ranges, stale)
			tc.record(t, staging+".meta")

			srv, gotRanges := rangedTestServer(t, fresh)
			it := PlanItem{RelativePath: "unpinned.bin", URL: srv.URL + "/unpinned.bin", Size: total, AcceptRanges: true}
			cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "size"}

			if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst,
				multipartOpts{Commit: rangedSeedCommit, CommitPinned: tc.planPinned}, func(ProgressEvent) {}); err != nil {
				t.Fatalf("downloadMultipart: %v", err)
			}

			assertExactFile(t, dst, fresh)
			if !requestTouchesStart(*gotRanges) {
				t.Errorf("the stale committed range was inherited; requests=%v", *gotRanges)
			}
		})
	}
}

// TestRangedResumeKeepsAPinnedMatchingIdentity is regression (c): a pinned
// record whose repository, commit, path and size match the current item is
// still inherited, and only the missing suffix is requested. The pinned gate
// must not over-refuse a resume that is proven.
func TestRangedResumeKeepsAPinnedMatchingIdentity(t *testing.T) {
	const total = 8000
	fresh := rangedTestPayload(total)

	dir := t.TempDir()
	dst := filepath.Join(dir, "pinned.bin")
	staging := dst + ".incomplete"
	ranges := []RangeProgress{
		{Start: 0, End: total/2 - 1, Committed: total / 2, Done: true},
		{Start: total / 2, End: total - 1},
	}
	seedRangedState(t, staging, "o/r", "pinned.bin", total, ranges, fresh)

	srv, gotRanges := rangedTestServer(t, fresh)
	it := PlanItem{RelativePath: "pinned.bin", URL: srv.URL + "/pinned.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "size"}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst,
		multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	assertExactFile(t, dst, fresh)
	want := fmt.Sprintf("bytes=%d-%d", total/2, total-1)
	if len(*gotRanges) != 1 || (*gotRanges)[0] != want {
		t.Errorf("ranged requests = %v, want exactly the missing suffix %q", *gotRanges, want)
	}
}

// TestDownloadUnresolvedCommitDoesNotInheritStagedBytes is regression (e):
// the whole path, end to end through Download.
//
// The endpoint does not resolve the requested revision, so the plan falls back
// to the revision string, which pins nothing. The item is an LFS file
// WITHOUT a known digest, so there is no content proof either. A staging
// record from an earlier run therefore names the same revision string and the
// same size, and its committed bytes are STALE: the branch moved. They must
// not be published as the new revision's content.
func TestDownloadUnresolvedCommitDoesNotInheritStagedBytes(t *testing.T) {
	const total = 8000
	fresh := rangedTestPayload(total)
	stale := staleRangedPayload(total)

	var (
		mu        sync.Mutex
		gotRanges []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			// The revision cannot be resolved: the plan falls back to the
			// requested revision, which pins nothing.
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/tree/main"):
			// An LFS file WITHOUT a digest: the planner permits an empty
			// SHA-256, and without one no content proof is possible.
			_, _ = w.Write([]byte(fmt.Sprintf(
				`[{"type":"file","path":"model.bin","size":%d,"lfs":{"size":%d}}]`, total, total)))
		case strings.Contains(r.URL.Path, "/resolve/"):
			if r.Method == http.MethodGet {
				mu.Lock()
				gotRanges = append(gotRanges, r.Header.Get("Range"))
				mu.Unlock()
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(fresh)))
			w.Header().Set("Accept-Ranges", "bytes")
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(fresh))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	out := t.TempDir()
	dst := legacyOutputDst(t, out)
	staging := dst + ".incomplete"
	ranges := []RangeProgress{
		{Start: 0, End: total/2 - 1, Committed: total / 2, Done: true},
		{Start: total / 2, End: total - 1},
	}
	seedRangedState(t, staging, "o/r", "model.bin", total, ranges, stale)
	// The earlier run could not resolve the commit either, so its record
	// names the requested revision and is NOT pinned.
	meta, err := readStagingMeta(staging + ".meta")
	if err != nil {
		t.Fatal(err)
	}
	meta.Commit = "main"
	meta.CommitPinned = false
	if err := writeStagingMeta(staging+".meta", meta); err != nil {
		t.Fatal(err)
	}

	cfg := Settings{
		OutputDir:          out,
		Endpoint:           srv.URL,
		Concurrency:        2,
		MaxActiveDownloads: 1,
		MultipartThreshold: "1",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
	if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
		t.Fatalf("Download: %v", err)
	}

	assertExactFile(t, dst, fresh)
	mu.Lock()
	ranges2 := append([]string(nil), gotRanges...)
	mu.Unlock()
	if !requestTouchesStart(ranges2) {
		t.Errorf("the stale committed range was inherited; requests=%v", ranges2)
	}
}

// TestNegativeControlRangedResumeIgnoresThePinnedFlag is the negative
// control for the pinned gate: with the historical rule installed (the
// commit string alone decides), the stale bytes staged by an earlier run ARE
// inherited and published. With the identity rule enforced, the same
// scenario publishes exactly the served content.
//
// This is the proof that the regressions above are discriminating.
func TestNegativeControlRangedResumeIgnoresThePinnedFlag(t *testing.T) {
	const total = 8000
	fresh := rangedTestPayload(total)
	stale := staleRangedPayload(total)

	old := resumeIdentityPinned
	defer func() { resumeIdentityPinned = old }()
	// The historical rule: the commit string alone decides, so a record
	// written under an unpinned identity is inherited.
	resumeIdentityPinned = func(recorded, current bool) bool { return true }

	dir := t.TempDir()
	dst := filepath.Join(dir, "unpinned.bin")
	staging := dst + ".incomplete"
	ranges := []RangeProgress{
		{Start: 0, End: total/2 - 1, Committed: total / 2, Done: true},
		{Start: total / 2, End: total - 1},
	}
	seedRangedState(t, staging, "o/r", "unpinned.bin", total, ranges, stale)
	meta, err := readStagingMeta(staging + ".meta")
	if err != nil {
		t.Fatal(err)
	}
	meta.CommitPinned = false
	if err := writeStagingMeta(staging+".meta", meta); err != nil {
		t.Fatal(err)
	}

	srv, gotRanges := rangedTestServer(t, fresh)
	it := PlanItem{RelativePath: "unpinned.bin", URL: srv.URL + "/unpinned.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "size"}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst,
		multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	published, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if bytes.Equal(published, fresh) {
		t.Fatal("the negative control did not reproduce the defect: the published bytes are the served content")
	}
	if !bytes.HasPrefix(published, stale[:total/2]) {
		t.Fatalf("the negative control did not reproduce the defect: the published bytes do not carry the stale prefix")
	}
	if requestTouchesStart(*gotRanges) {
		t.Errorf("the negative control did not reproduce the defect: the stale range was re-requested: %v", *gotRanges)
	}
	t.Logf("negative control reproduced the unpinned resume: published %d bytes with the stale prefix", len(published))
}
