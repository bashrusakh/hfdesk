// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathInside(t *testing.T) {
	base := filepath.Join("home", "cache")
	cases := []struct {
		target string
		want   bool
	}{
		{filepath.Join(base), true},
		{filepath.Join(base, "a", "b"), true},
		{filepath.Join(base, "..", "etc"), false},
		{filepath.Join("home", "cacheX"), false}, // shared prefix but not nested
		{filepath.Join("etc", "passwd"), false},
	}
	for _, c := range cases {
		if got := PathInside(base, c.target); got != c.want {
			t.Errorf("PathInside(%q,%q)=%v want %v", base, c.target, got, c.want)
		}
	}
}

func TestSafeJoin(t *testing.T) {
	base := filepath.Join("home", "cache")
	if _, err := SafeJoin(base, filepath.Join("a", "b.txt")); err != nil {
		t.Errorf("expected ok, got %v", err)
	}
	// Note: "C:\\win" is only rejected on Windows (filepath.VolumeName is
	// OS-specific), so it is not part of this cross-platform list.
	for _, rel := range []string{"../escape", "a/../../escape", "../../etc/passwd", "/abs/escape"} {
		if _, err := SafeJoin(base, rel); err == nil {
			t.Errorf("SafeJoin(%q,%q) expected error, got nil", base, rel)
		}
	}
}

func TestRepoRejectsTraversal(t *testing.T) {
	c := NewHFCache("/root", 0)
	for _, bad := range []string{"../foo", "foo/..", "owner/..", "a/b/c", ""} {
		if _, err := c.Repo(bad, RepoTypeModel); err == nil {
			t.Errorf("Repo(%q) expected error, got nil", bad)
		}
	}
	if _, err := c.Repo("owner/name", RepoTypeModel); err != nil {
		t.Errorf("Repo(owner/name) unexpected error: %v", err)
	}
}

func TestRefAndSnapshotPropagateError(t *testing.T) {
	c := NewHFCache("/root", 0)
	r, err := c.Repo("owner/name", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RefPath("../escape"); err == nil {
		t.Error("RefPath traversal: expected error, got nil")
	}
	if _, err := r.SnapshotDir("../escape"); err == nil {
		t.Error("SnapshotDir traversal: expected error, got nil")
	}
	if _, err := r.SnapshotPath("commit", "../escape"); err == nil {
		t.Error("SnapshotPath traversal: expected error, got nil")
	}
}

// TestSafeJoinRejectsNonLocal pins the tightened SafeJoin contract: values
// that filepath.IsLocal rejects (empty, "..", escaping segments) return an
// error instead of a path, while every legitimate relative shape keeps the
// exact cleaned-join result.
func TestSafeJoinRejectsNonLocal(t *testing.T) {
	base := filepath.Join("home", "cache")
	for _, rel := range []string{"", "..", "../escape", "a/../../escape"} {
		if _, err := SafeJoin(base, rel); err == nil {
			t.Errorf("SafeJoin(%q, %q) expected error, got nil", base, rel)
		}
	}
	for _, rel := range []string{".", "file.bin", filepath.Join("sub", "file.bin")} {
		got, err := SafeJoin(base, rel)
		if err != nil {
			t.Errorf("SafeJoin(%q, %q) unexpected error: %v", base, rel, err)
			continue
		}
		if want := filepath.Clean(filepath.Join(base, rel)); got != want {
			t.Errorf("SafeJoin(%q, %q) = %q, want %q", base, rel, got, want)
		}
	}
}

// TestPlanRepoRejectsTraversalSHA is the chokepoint regression test: a
// crafted tree response carrying a traversal value in any of the SHA fields
// (top-level sha256, lfs.sha256, lfs.oid) must fail the whole plan loudly
// before the value can reach any filesystem path.
func TestPlanRepoRejectsTraversalSHA(t *testing.T) {
	cases := []struct {
		name string
		node hfNode
	}{
		{"top-level sha256", hfNode{Type: "file", Path: "weights.bin", Size: 10, Sha256: "../../../../tmp/evil"}},
		{"lfs sha256", hfNode{Type: "file", Path: "weights.bin", Size: 10, LFS: &hfLfsInfo{Sha256: "../../../../tmp/evil", Size: 10}}},
		{"lfs oid", hfNode{Type: "file", Path: "weights.bin", Size: 10, LFS: &hfLfsInfo{Oid: "../../../../tmp/evil", Size: 10}}},
		{"parent dir", hfNode{Type: "file", Path: "weights.bin", Size: 10, Sha256: ".."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := mockHFServerForPlan(t, []hfNode{c.node})
			defer srv.Close()
			_, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "main"}, Settings{Endpoint: srv.URL})
			if err == nil {
				t.Fatal("PlanRepo accepted a traversal sha256 from the tree API")
			}
			if !strings.Contains(err.Error(), "sha256") {
				t.Errorf("error should identify the unsafe sha256, got: %v", err)
			}
		})
	}
}

// TestPlanRepoKeepsLegitimateSHAs is the guard against an over-strict gate:
// every SHA shape the tree API legitimately carries must pass through the
// plan unchanged, including the empty (absent) shape and the LFS-spec
// "sha256:"-prefixed oid.
func TestPlanRepoKeepsLegitimateSHAs(t *testing.T) {
	const hex64 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	const hex40 = "a6344aac8c09253b3b630fb776ae94478aa0275b"
	files := []hfNode{
		{Type: "file", Path: "config.json", Size: 3},
		{Type: "file", Path: "model.gguf", Size: 4, Sha256: hex64},
		{Type: "file", Path: "a.bin", Size: 5, LFS: &hfLfsInfo{Sha256: hex40, Size: 5}},
		{Type: "file", Path: "b.bin", Size: 6, LFS: &hfLfsInfo{Oid: hex64, Size: 6}},
		{Type: "file", Path: "c.bin", Size: 7, LFS: &hfLfsInfo{Oid: "sha256:" + hex64, Size: 7}},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "main"}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo rejected legitimate SHA shapes: %v", err)
	}
	want := map[string]string{
		"config.json": "",
		"model.gguf":  hex64,
		"a.bin":       hex40,
		"b.bin":       hex64,
		"c.bin":       "sha256:" + hex64,
	}
	if len(plan.Items) != len(want) {
		t.Fatalf("got %d plan items, want %d", len(plan.Items), len(want))
	}
	for _, it := range plan.Items {
		w, ok := want[it.RelativePath]
		if !ok {
			t.Errorf("unexpected plan item %q", it.RelativePath)
			continue
		}
		if it.SHA256 != w {
			t.Errorf("item %q SHA256 = %q, want %q (must pass through unchanged)", it.RelativePath, it.SHA256, w)
		}
	}
}

// TestDownloadTraversalSHAStaysInsideCacheRoot exercises the actual broken
// contract end to end: a crafted tree response whose sha256 contains
// traversal segments must not cause any directory or file to be created
// outside the configured cache root, and the download must fail loudly.
func TestDownloadTraversalSHAStaysInsideCacheRoot(t *testing.T) {
	body := []byte("payload")
	sha := "../../../../../../hfdesk-path-injection-canary"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.Contains(req.URL.Path, "/revision/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "commit123"})
		case strings.Contains(req.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"type": "file", "path": "file.bin", "size": len(body), "sha256": sha}})
		case strings.Contains(req.URL.Path, "/raw/"), strings.Contains(req.URL.Path, "/resolve/"):
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			if req.Method != http.MethodHead {
				_, _ = w.Write(body)
			}
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()

	cacheRoot := filepath.Join(t.TempDir(), "cache")
	cfg := Settings{CacheDir: cacheRoot, Endpoint: srv.URL, Concurrency: 1}
	err := Download(context.Background(), Job{Repo: "owner/model"}, cfg, nil)

	// Both legs of the unvalidated SHA: the tmp-<sha> staging path in Download
	// and the blobs/<sha> path BlobPath hands to the store.
	blobs := filepath.Join(cacheRoot, "hub", "models--owner--model", "blobs")
	escape := filepath.Clean(filepath.Join(blobs, "tmp-"+sha))
	blobEscape := filepath.Clean(filepath.Join(blobs, sha))
	if PathInside(cacheRoot, escape) {
		t.Fatalf("test payload does not escape the cache root: %q", escape)
	}
	if PathInside(cacheRoot, blobEscape) {
		t.Fatalf("test payload does not escape the cache root: %q", blobEscape)
	}
	for _, artifact := range []string{escape, escape + ".part", blobEscape} {
		artifact := artifact
		defer os.Remove(artifact)
		if _, statErr := os.Stat(artifact); statErr == nil {
			t.Errorf("file created outside the cache root: %q", artifact)
		}
	}
	if err == nil {
		t.Fatal("Download accepted a traversal sha256 from the tree API")
	}
}

// TestDestinationBaseRejectsTraversal pins the destination-base contract: the
// repo-derived folder segment joined onto cfg.OutputDir must be local. job.Repo
// is validated at the Download boundary, but LocalRepo and direct library
// callers reach this join unchecked, so a "../" (or absolute) segment would
// move the whole destination — and every downstream os.* path — outside the
// configured output root.
func TestDestinationBaseRejectsTraversal(t *testing.T) {
	cfg := Settings{OutputDir: filepath.Join("home", "out")}
	bad := []Job{
		{Repo: "owner/../../escape"},
		{Repo: "owner/model", LocalRepo: "../../escape"},
		{Repo: "owner/model", LocalRepo: ".."},
		{Repo: "owner/model", LocalRepo: "/abs/escape"},
	}
	for _, job := range bad {
		got, err := destinationBase(job, cfg)
		if err == nil {
			t.Errorf("destinationBase(%+v) = %q, want error", job, got)
		}
	}
	good := []struct {
		job  Job
		want string
	}{
		{Job{Repo: "owner/model"}, filepath.Join("home", "out", "owner", "model")},
		{Job{Repo: "owner/model", LocalRepo: "vendor/model-a"}, filepath.Join("home", "out", "vendor", "model-a")},
	}
	for _, c := range good {
		got, err := destinationBase(c.job, cfg)
		if err != nil {
			t.Errorf("destinationBase(%+v) unexpected error: %v", c.job, err)
			continue
		}
		if got != c.want {
			t.Errorf("destinationBase(%+v) = %q, want %q", c.job, got, c.want)
		}
	}
}

// newMockHFFileServer serves the revision, tree, and file endpoints the
// downloader needs for a single "file.bin" repository, mirroring the other
// pathsafe end-to-end tests.
func newMockHFFileServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.Contains(req.URL.Path, "/revision/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "commit123"})
		case strings.Contains(req.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"type": "file", "path": "file.bin", "size": len(body)}})
		case strings.Contains(req.URL.Path, "/raw/"), strings.Contains(req.URL.Path, "/resolve/"):
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			if req.Method != http.MethodHead {
				_, _ = w.Write(body)
			}
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDownloadTraversalLocalRepoStaysInsideOutputDir exercises the actual
// broken contract end to end in flat (local-dir) mode: a job whose LocalRepo
// override contains traversal segments must fail loudly before any directory
// or file is created outside the configured output root — previously the
// override moved destinationBase itself outside the root and the download
// happily wrote there.
func TestDownloadTraversalLocalRepoStaysInsideOutputDir(t *testing.T) {
	srv := newMockHFFileServer(t, []byte("payload"))

	outRoot := filepath.Join(t.TempDir(), "out")
	canaryRel := "../hfdesk-localrepo-escape-canary"
	cfg := Settings{OutputDir: outRoot, Endpoint: srv.URL, Concurrency: 1}
	err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main", LocalRepo: canaryRel}, cfg, nil)

	escaped := filepath.Clean(filepath.Join(outRoot, canaryRel, "file.bin"))
	if PathInside(outRoot, escaped) {
		t.Fatalf("test payload does not escape the output root: %q", escaped)
	}
	if _, statErr := os.Stat(escaped); statErr == nil {
		_ = os.Remove(escaped)
		_ = os.Remove(filepath.Dir(escaped))
		t.Errorf("file created outside the output root: %q", escaped)
	}
	if err == nil {
		t.Fatal("Download accepted a traversal LocalRepo override")
	}
}

// TestDownloadOutputDirTraversalSegmentsStayInsideCleanRoot pins the
// preserved contract for the output-root leg: an output directory containing
// ".." segments resolves to its cleaned location, and every file the
// downloader writes stays inside that cleaned root.
func TestDownloadOutputDirTraversalSegmentsStayInsideCleanRoot(t *testing.T) {
	srv := newMockHFFileServer(t, []byte("payload"))

	tmp := t.TempDir()
	outRoot := filepath.Join(tmp, "a", "..", "b") // cleans to tmp/b
	cfg := Settings{OutputDir: outRoot, Endpoint: srv.URL, Concurrency: 1}
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, nil); err != nil {
		t.Fatalf("Download with traversal-segment output dir failed: %v", err)
	}
	cleanRoot := filepath.Clean(outRoot)
	written := filepath.Join(cleanRoot, "owner", "model", "file.bin")
	if _, err := os.Stat(written); err != nil {
		t.Fatalf("expected file under cleaned output root: %v", err)
	}
	if !PathInside(cleanRoot, written) {
		t.Errorf("file %q escaped the cleaned output root %q", written, cleanRoot)
	}
}

// TestDownloadCacheDirTraversalSegmentsStaysInsideCleanRoot pins the
// preserved contract for the cache-root leg: a cache directory containing
// ".." segments resolves to its cleaned physical root and all cache content
// stays inside it.
func TestDownloadCacheDirTraversalSegmentsStaysInsideCleanRoot(t *testing.T) {
	srv := newMockHFFileServer(t, []byte("payload"))

	tmp := t.TempDir()
	cacheRoot := filepath.Join(tmp, "c1", "..", "cache") // cleans to tmp/cache
	cfg := Settings{CacheDir: cacheRoot, Endpoint: srv.URL, Concurrency: 1}
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, nil); err != nil {
		t.Fatalf("Download with traversal-segment cache dir failed: %v", err)
	}
	cleanRoot := filepath.Clean(cacheRoot)
	var outside []string
	if err := filepath.Walk(cleanRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !PathInside(cleanRoot, p) {
			outside = append(outside, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk cleaned cache root: %v", err)
	}
	if len(outside) > 0 {
		t.Errorf("cache content outside cleaned root: %v", outside)
	}
	if _, err := os.Stat(filepath.Join(cleanRoot, "hub")); err != nil {
		t.Errorf("expected hub dir under cleaned cache root: %v", err)
	}
}

func TestIsValidModelNameRejectsTraversal(t *testing.T) {
	bad := []string{"../foo", "foo/..", "../..", "owner/..", "..\\name", "owner/na\x00me"}
	for _, b := range bad {
		if IsValidModelName(b) {
			t.Errorf("IsValidModelName(%q)=true, want false", b)
		}
	}
	good := []string{"TheBloke/Mistral-7B", "facebook/opt-1.3b", "owner/name..v2"}
	for _, g := range good {
		if !IsValidModelName(g) {
			t.Errorf("IsValidModelName(%q)=false, want true", g)
		}
	}
}
