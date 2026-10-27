// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

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

var singleTestIdentity = singleOpts{
	Commit:       "1111111111111111111111111111111111111111",
	CommitPinned: true,
}

func pinnedSingleIdentity() singleOpts {
	return singleTestIdentity
}

// completeSingleIdentity adds the clean-framing proof required to publish an
// exact-size partial without a request.
func completeSingleIdentity() singleOpts {
	opts := singleTestIdentity
	opts.FramingComplete = true
	return opts
}

func seedSingleIdentity(t *testing.T, dst string, job Job, it PlanItem, opts singleOpts) {
	t.Helper()
	if err := writeSinglePartIdentity(singlePartIdentityPath(dst), singleIdentityFor(job, it, opts)); err != nil {
		t.Fatal(err)
	}
}

func seedSinglePartial(t *testing.T, dst string, data []byte, job Job, it PlanItem, opts singleOpts) {
	t.Helper()
	if err := os.WriteFile(dst+".part", data, 0o644); err != nil {
		t.Fatal(err)
	}
	seedSingleIdentity(t, dst, job, it, opts)
}

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

// serveWhole honors Range requests and optionally records their headers.
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

// Distinct paths that normalize to the same staging name must not share bytes.
func TestSinglePathIdentityDoesNotShareBytesBetweenCollidingPaths(t *testing.T) {
	first := []byte("AAAAA-first--object-bytes")
	second := []byte("BBBBB-second-object-bytes")
	if len(first) != len(second) {
		t.Fatal("fixture sizes must match so only the path identity rejects resume")
	}

	dst := filepath.Join(t.TempDir(), "blobs", "tmp-a_b")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	job := Job{Repo: "o/r"}

	firstItem := PlanItem{RelativePath: "a/b", Size: int64(len(first))}
	prefix := first[:5]
	if err := os.WriteFile(dst+".part", prefix, 0o644); err != nil {
		t.Fatal(err)
	}
	seedSingleIdentity(t, dst, job, firstItem, singleTestIdentity)

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

// A staged prefix from a different resolved commit is not inherited.
func TestSinglePathIdentityDoesNotInheritAcrossRevisions(t *testing.T) {
	full := []byte("the content of the newer revision")
	f := newSingleIdentityFixture(t, "model.bin", int64(len(full)))
	f.it.URL = "http://127.0.0.1:1/model.bin"

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

// An unresolved current commit inherits no bytes, regardless of record pinning.
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

// A sidecar-less partial is unproven and must restart from zero.
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

// An interrupted transfer retains its sidecar so the same item can resume.
func TestSinglePathIdentityRecordSurvivesAnInterruptedTransfer(t *testing.T) {
	full := []byte("content of a transfer that is interrupted half way")
	const written = 11
	f := newSingleIdentityFixture(t, "model.bin", int64(len(full)))
	if err := os.MkdirAll(filepath.Dir(f.dst), 0o755); err != nil {
		t.Fatal(err)
	}

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

// The full Download plan must keep colliding path-derived names bound to each
// item's served bytes.
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

	repoDir, err := NewHFCache(cacheDir).Repo("o/r", RepoTypeModel)
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
