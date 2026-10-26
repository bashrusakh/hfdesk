// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Resume identity of the single-request path (issue #140 follow-up, group 1).
//
// The single-request path stages its bytes in <dst>.part. For an item without
// a known digest the destination name is derived from the item's path
// (tmp-<sanitized path>), so two different objects - and two revisions of one
// object - share one staging name. The bytes already on disk are inherited
// only when the identity sidecar <dst>.part.meta proves they belong to this
// exact object: same repository, same RESOLVED commit, same item path, same
// expected size, and a PINNED identity on both sides.
//
// The regressions below cover the decision table:
//
//	(a) two items whose paths collide in the staging name do not share bytes;
//	(b) the same path at two different resolved commits does not inherit;
//	(c) the same path at the same resolved commit keeps the resume;
//	(d) an unresolved commit inherits nothing;
//	(e) a legacy partial without a sidecar inherits nothing;
//	(f) a successful download leaves neither a partial nor a sidecar;
//	(g) an interrupted download leaves a partial WITH its sidecar, and the
//	    next run can evaluate that record.

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

// singleTestIdentity is the pinned resolved-commit identity the single-path
// fixtures use: a resolved, immutable commit.
var singleTestIdentity = singleOpts{
	Commit:       "1111111111111111111111111111111111111111",
	CommitPinned: true,
}

// pinnedSingleIdentity returns the pinned resolved-commit identity the
// single-path fixtures use.
func pinnedSingleIdentity() singleOpts {
	return singleTestIdentity
}

// seedSingleIdentity writes the identity sidecar that authorizes inheriting
// the partial staged at dst.
func seedSingleIdentity(t *testing.T, dst string, job Job, it PlanItem, opts singleOpts) {
	t.Helper()
	if err := writeSinglePartIdentity(singlePartIdentityPath(dst), singleIdentityFor(job, it, opts)); err != nil {
		t.Fatal(err)
	}
}

// seedSinglePartial stages a partial payload together with the identity
// sidecar that authorizes inheriting it, so a fixture matches the
// single-request identity contract.
func seedSinglePartial(t *testing.T, dst string, data []byte, job Job, it PlanItem, opts singleOpts) {
	t.Helper()
	if err := os.WriteFile(dst+".part", data, 0o644); err != nil {
		t.Fatal(err)
	}
	seedSingleIdentity(t, dst, job, it, opts)
}

// singleIdentityFixture is a staged destination plus the item identity the
// transfer under test uses.
type singleIdentityFixture struct {
	dst  string
	job  Job
	it   PlanItem
	opts singleOpts
}

func newSingleIdentityFixture(t *testing.T, name string, size int64) singleIdentityFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	return singleIdentityFixture{
		dst:  filepath.Join(dir, "blobs", name),
		job:  Job{Repo: "o/r"},
		it:   PlanItem{RelativePath: name, Size: size},
		opts: singleTestIdentity,
	}
}

// serveWhole serves body for every request, honoring a Range header the way a
// real file server does, and records the Range headers it was asked for.
func serveWhole(t *testing.T, body []byte, ranges *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ranges != nil {
			mu.Lock()
			*ranges = append(*ranges, r.Header.Get("Range"))
			mu.Unlock()
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSinglePathIdentityDoesNotShareBytesBetweenCollidingPaths is regression
// (a): two items whose paths collide in the staging name ("a/b" and "a_b" both
// stage under tmp-a_b) must never share bytes.
//
// The first item leaves a staged prefix behind (an interrupted transfer). The
// second item, a DIFFERENT object that happens to use the same staging name,
// must not inherit that prefix: it downloads its own content from zero and
// publishes exactly what the server served for it.
func TestSinglePathIdentityDoesNotShareBytesBetweenCollidingPaths(t *testing.T) {
	first := []byte("AAAAA-first-object-bytes")
	second := []byte("BBBBB-second-object-bytes")

	// The staging name collides: both items stage under tmp-a_b.
	dst := filepath.Join(t.TempDir(), "blobs", "tmp-a_b")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	job := Job{Repo: "o/r"}

	// The first item is interrupted after a prefix was written.
	firstItem := PlanItem{RelativePath: "a/b", Size: int64(len(first))}
	prefix := first[:5]
	if err := os.WriteFile(dst+".part", prefix, 0o644); err != nil {
		t.Fatal(err)
	}
	seedSingleIdentity(t, dst, job, firstItem, singleTestIdentity)

	// The second item is a different object with the same staging name.
	secondItem := PlanItem{RelativePath: "a_b", Size: int64(len(second))}
	var (
		mu     sync.Mutex
		ranges []string
	)
	srv := serveWhole(t, second, &ranges, &mu)
	secondItem.URL = srv.URL + "/a_b"

	cfg := Settings{Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, secondItem, dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "" {
		t.Errorf("requests = %v, want one full request (the other object's bytes must not be resumed)", got)
	}
	published, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if !bytes.Equal(published, second) {
		t.Errorf("published bytes = %q, want exactly the content served for a_b (%q)", published, second)
	}
	if bytes.Equal(published, first) {
		t.Error("the other object's bytes were published")
	}
}

// TestSinglePathIdentityDoesNotInheritAcrossRevisions is regression (b): the
// same path at a DIFFERENT resolved commit inherits nothing. The record names
// the earlier commit, so the staged bytes belong to another revision.
func TestSinglePathIdentityDoesNotInheritAcrossRevisions(t *testing.T) {
	full := []byte("the content of the newer revision")
	f := newSingleIdentityFixture(t, "model.bin", int64(len(full)))
	f.it.URL = "http://127.0.0.1:1/model.bin"

	// A partial staged by the earlier revision, with its identity record.
	seedSinglePartial(t, f.dst, full[:6], f.job, f.it, singleTestIdentity)

	newer := singleOpts{Commit: "2222222222222222222222222222222222222222", CommitPinned: true}
	var (
		mu     sync.Mutex
		ranges []string
	)
	srv := serveWhole(t, full, &ranges, &mu)
	f.it.URL = srv.URL + "/model.bin"

	cfg := Settings{Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", f.job, cfg, f.it, f.dst, newer, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "" {
		t.Errorf("requests = %v, want one full request (another revision's bytes must not be resumed)", got)
	}
	assertExactFile(t, f.dst, full)
}

// TestSinglePathIdentityKeepsResumeForTheSameRevision is regression (c): the
// same path at the SAME resolved commit keeps the resume - only the missing
// suffix is requested and the published bytes are exact.
func TestSinglePathIdentityKeepsResumeForTheSameRevision(t *testing.T) {
	full := []byte("resumable content of exactly this revision")
	const prefix = 9
	f := newSingleIdentityFixture(t, "model.bin", int64(len(full)))
	seedSinglePartial(t, f.dst, full[:prefix], f.job, f.it, singleTestIdentity)

	var (
		mu     sync.Mutex
		ranges []string
	)
	srv := serveWhole(t, full, &ranges, &mu)
	f.it.URL = srv.URL + "/model.bin"

	cfg := Settings{Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", f.job, cfg, f.it, f.dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	want := fmt.Sprintf("bytes=%d-%d", prefix, len(full)-1)
	if len(got) != 1 || got[0] != want {
		t.Errorf("requests = %v, want the exact missing suffix %q", got, want)
	}
	assertExactFile(t, f.dst, full)
}

// TestSinglePathIdentityUnresolvedCommitInheritsNothing is regression (d): an
// unresolved commit pins nothing, so no staged bytes are inherited - neither
// from a record that was pinned nor from one that was not.
func TestSinglePathIdentityUnresolvedCommitInheritsNothing(t *testing.T) {
	full := []byte("content of a revision that could not be resolved")
	for _, tc := range []struct {
		name     string
		recorded singleOpts
	}{
		{
			name:     "record was pinned",
			recorded: singleOpts{Commit: "3333333333333333333333333333333333333333", CommitPinned: true},
		},
		{
			name:     "record was unpinned",
			recorded: singleOpts{Commit: "", CommitPinned: false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSingleIdentityFixture(t, "model.bin", int64(len(full)))
			seedSinglePartial(t, f.dst, full[:5], f.job, f.it, tc.recorded)

			var (
				mu     sync.Mutex
				ranges []string
			)
			srv := serveWhole(t, full, &ranges, &mu)
			f.it.URL = srv.URL + "/model.bin"

			unpinned := singleOpts{Commit: "", CommitPinned: false}
			cfg := Settings{Retries: 0, StallTimeout: "0"}
			if _, err := downloadSingle(context.Background(), srv.Client(), "", f.job, cfg, f.it, f.dst, unpinned, func(ProgressEvent) {}); err != nil {
				t.Fatalf("downloadSingle: %v", err)
			}

			mu.Lock()
			got := append([]string(nil), ranges...)
			mu.Unlock()
			if len(got) != 1 || got[0] != "" {
				t.Errorf("requests = %v, want one full request (an unresolved commit pins nothing)", got)
			}
			assertExactFile(t, f.dst, full)
		})
	}
}

// TestSinglePathIdentityLegacyPartialWithoutRecordIsNotInherited is
// regression (e): a partial left by an older run has no identity record, so
// its bytes are unproven and are discarded.
func TestSinglePathIdentityLegacyPartialWithoutRecordIsNotInherited(t *testing.T) {
	full := []byte("content whose predecessor left an unproven partial")
	f := newSingleIdentityFixture(t, "model.bin", int64(len(full)))
	if err := os.MkdirAll(filepath.Dir(f.dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.dst+".part", full[:7], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(singlePartIdentityPath(f.dst)); !os.IsNotExist(err) {
		t.Fatalf("fixture precondition failed: a legacy partial has no identity record (stat err=%v)", err)
	}

	var (
		mu     sync.Mutex
		ranges []string
	)
	srv := serveWhole(t, full, &ranges, &mu)
	f.it.URL = srv.URL + "/model.bin"

	cfg := Settings{Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", f.job, cfg, f.it, f.dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "" {
		t.Errorf("requests = %v, want one full request (an unproven legacy partial must not be resumed)", got)
	}
	assertExactFile(t, f.dst, full)
}

// TestSinglePathIdentityRecordIsRemovedWithThePartial is regression (f): a
// successful download leaves neither the partial nor its identity record, so
// no record outlives the bytes it describes.
func TestSinglePathIdentityRecordIsRemovedWithThePartial(t *testing.T) {
	full := []byte("a complete transfer")
	f := newSingleIdentityFixture(t, "model.bin", int64(len(full)))
	if err := os.MkdirAll(filepath.Dir(f.dst), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := serveWhole(t, full, nil, nil)
	f.it.URL = srv.URL + "/model.bin"

	cfg := Settings{Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", f.job, cfg, f.it, f.dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}
	assertExactFile(t, f.dst, full)
	for _, p := range []string{f.dst + ".part", singlePartIdentityPath(f.dst)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived a successful download (stat err=%v)", filepath.Base(p), err)
		}
	}
}

// TestSinglePathIdentityRecordSurvivesAnInterruptedTransfer is regression
// (g): an interrupted transfer leaves the partial WITH its identity record,
// and a later run of the same item evaluates that record and resumes.
func TestSinglePathIdentityRecordSurvivesAnInterruptedTransfer(t *testing.T) {
	full := []byte("content of a transfer that is interrupted half way")
	const written = 11
	f := newSingleIdentityFixture(t, "model.bin", int64(len(full)))
	if err := os.MkdirAll(filepath.Dir(f.dst), 0o755); err != nil {
		t.Fatal(err)
	}

	// The server announces the full length but stops half way: the transfer
	// fails with a truncated body and leaves the accepted prefix on disk.
	trunc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		_, _ = w.Write(full[:written])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer trunc.Close()
	f.it.URL = trunc.URL + "/model.bin"

	cfg := Settings{Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), trunc.Client(), "", f.job, cfg, f.it, f.dst, singleTestIdentity, func(ProgressEvent) {}); err == nil {
		t.Fatal("the truncated transfer unexpectedly succeeded")
	}

	part, err := os.ReadFile(f.dst + ".part")
	if err != nil {
		t.Fatalf("the interrupted transfer left no partial: %v", err)
	}
	if !bytes.Equal(part, full[:written]) {
		t.Errorf("staged prefix = %q, want %q", part, full[:written])
	}
	record, err := readSinglePartIdentity(singlePartIdentityPath(f.dst))
	if err != nil {
		t.Fatalf("the interrupted transfer left no usable identity record: %v", err)
	}
	if record == nil {
		t.Fatal("the interrupted transfer left no identity record")
	}
	if record.Path != f.it.RelativePath || record.Repo != f.job.Repo || !record.Pinned || record.Commit != singleTestIdentity.Commit {
		t.Errorf("identity record = %+v, want the item's pinned identity", record)
	}

	// The next run of the SAME item evaluates that record and resumes.
	var (
		mu     sync.Mutex
		ranges []string
	)
	srv := serveWhole(t, full, &ranges, &mu)
	f.it.URL = srv.URL + "/model.bin"
	if _, err := downloadSingle(context.Background(), srv.Client(), "", f.job, cfg, f.it, f.dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("resumed downloadSingle: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	if len(got) != 1 || got[0] != fmt.Sprintf("bytes=%d-%d", written, len(full)-1) {
		t.Errorf("requests = %v, want the exact missing suffix", got)
	}
	assertExactFile(t, f.dst, full)
}

// TestDownloadSinglePlanWithCollidingStagingNamesPublishesExactBytes covers
// requirement (a) end to end: ONE plan with the colliding paths "a/b" and
// "a_b" publishes, for each item, exactly the bytes the server served for
// that item. Neither item may end up with the other's content.
func TestDownloadSinglePlanWithCollidingStagingNamesPublishesExactBytes(t *testing.T) {
	files := map[string][]byte{
		"a/b": []byte("bytes served for the nested path a/b"),
		"a_b": []byte("bytes served for the flat path a_b"),
	}
	commit := "4444444444444444444444444444444444444444"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/o/r/revision/main":
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: commit})
		case r.URL.Path == "/api/models/o/r/tree/"+commit:
			nodes := make([]hfNode, 0, len(files))
			for path, body := range files {
				nodes = append(nodes, hfNode{Type: "file", Path: path, Size: int64(len(body))})
			}
			_ = json.NewEncoder(w).Encode(nodes)
		default:
			body, ok := files[strings.TrimPrefix(r.URL.Path, "/o/r/raw/"+commit+"/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body)
		}
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	cfg := Settings{CacheDir: cacheDir, Endpoint: srv.URL, Retries: 0, StallTimeout: "0"}
	if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
		t.Fatalf("Download: %v", err)
	}

	repoDir, err := NewHFCache(cacheDir, 0).Repo("o/r", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range files {
		published, err := repoDir.SnapshotPath(commit, path)
		if err != nil {
			t.Fatalf("SnapshotPath(%q): %v", path, err)
		}
		got, err := os.ReadFile(published)
		if err != nil {
			t.Fatalf("read published %q: %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("published %q = %q, want %q", path, got, want)
		}
	}
}

// TestNegativeControlSinglePathResumesBySizeAlone is the negative control for
// the identity rule: with the historical rule installed (inherit the staged
// bytes on their size alone), the cross-item scenario DOES publish the other
// object's bytes. With the identity record enforced, the same scenario
// publishes exactly the served content.
//
// This is the proof that the positive regressions above are discriminating.
func TestNegativeControlSinglePathResumesBySizeAlone(t *testing.T) {
	first := []byte("AAAAA-first-object-bytes")
	second := []byte("BBBBB-second-object-bytes")

	old := singlePartialInheritable
	defer func() { singlePartialInheritable = old }()
	// The historical rule: the staged bytes are inherited on their size
	// alone, with no binding to the object they belong to.
	singlePartialInheritable = func(recorded *singlePartIdentity, current singlePartIdentity, staged int64) bool {
		return staged > 0
	}

	dst := filepath.Join(t.TempDir(), "blobs", "tmp-a_b")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	job := Job{Repo: "o/r"}
	firstItem := PlanItem{RelativePath: "a/b", Size: int64(len(first))}
	if err := os.WriteFile(dst+".part", first[:5], 0o644); err != nil {
		t.Fatal(err)
	}
	seedSingleIdentity(t, dst, job, firstItem, singleTestIdentity)

	secondItem := PlanItem{RelativePath: "a_b", Size: int64(len(second))}
	srv := serveWhole(t, second, nil, nil)
	secondItem.URL = srv.URL + "/a_b"

	cfg := Settings{Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, secondItem, dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}
	published, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if bytes.Equal(published, second) {
		t.Fatal("the negative control did not reproduce the defect: the published bytes are the served content")
	}
	if !bytes.HasPrefix(published, first[:5]) {
		t.Fatalf("the negative control did not reproduce the defect: published %q does not carry the other object's prefix", published)
	}
	t.Logf("negative control reproduced the mixed publication: %q", published)
}
