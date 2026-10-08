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

	// Filesystem/volume root bases: filepath.Clean leaves the root's
	// trailing separator in place ("/" on Unix; on Windows a drive root
	// such as "C:\"), so the separator-strict prefix must not double it —
	// the root itself and every cleaned absolute target are inside the
	// root, while relative targets are not.
	rootBase := string(filepath.Separator)
	rootCases := []struct {
		target string
		want   bool
	}{
		{rootBase, true},
		{filepath.Join(rootBase, "owner", "model"), true},
		{filepath.Join(rootBase, "a", "b", "c"), true},
		{"owner/model", false}, // relative target is not inside an absolute root
	}
	for _, c := range rootCases {
		if got := PathInside(rootBase, c.target); got != c.want {
			t.Errorf("PathInside(%q,%q)=%v want %v", rootBase, c.target, got, c.want)
		}
	}
}

// TestSafeJoinFilesystemRootBase pins the latent sibling of the same defect:
// SafeJoin gates `rel` with filepath.IsLocal (rejecting absolute and
// escaping values) and then proves containment with PathInside, so a
// filesystem-root base must accept genuinely-local joins instead of
// rejecting every destination under it as an escape. Escape attempts keep
// failing at the IsLocal/absolute gates, before PathInside is consulted.
func TestSafeJoinFilesystemRootBase(t *testing.T) {
	rootBase := string(filepath.Separator)
	got, err := SafeJoin(rootBase, filepath.Join("owner", "model"))
	if err != nil {
		t.Fatalf("SafeJoin with filesystem-root base: %v", err)
	}
	if want := filepath.Join(rootBase, "owner", "model"); got != want {
		t.Errorf("SafeJoin(%q, owner/model) = %q, want %q", rootBase, got, want)
	}
	for _, rel := range []string{"../escape", "/abs/escape", ""} {
		if _, err := SafeJoin(rootBase, rel); err == nil {
			t.Errorf("SafeJoin(%q, %q) expected error, got nil", rootBase, rel)
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
// plan unchanged: the empty (absent) shape and canonical 64-hex values in
// any of the modeled fields (top-level sha256, lfs.sha256, lfs.oid).
func TestPlanRepoKeepsLegitimateSHAs(t *testing.T) {
	const hex64 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	files := []hfNode{
		{Type: "file", Path: "config.json", Size: 3},
		{Type: "file", Path: "model.gguf", Size: 4, Sha256: hex64},
		{Type: "file", Path: "a.bin", Size: 5, LFS: &hfLfsInfo{Sha256: hex64, Size: 5}},
		{Type: "file", Path: "b.bin", Size: 6, LFS: &hfLfsInfo{Oid: hex64, Size: 6}},
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
		"a.bin":       hex64,
		"b.bin":       hex64,
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

// TestPlanRepoRejectsNonCanonicalSHA is the canonical-form regression test:
// a tree response whose SHA field carries anything other than empty or 64
// hex characters must fail the whole plan loudly. The rejected shapes -- a
// 40-hex git-OID value and an LFS-spec "sha256:"-prefixed oid -- can never
// match a computed 64-hex digest at verify time, so the previous acceptance
// only deferred a guaranteed verification failure while writing
// non-canonical values into tmp-/blobs/ paths.
func TestPlanRepoRejectsNonCanonicalSHA(t *testing.T) {
	const hex64 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	const hex40 = "a6344aac8c09253b3b630fb776ae94478aa0275b"
	cases := []struct {
		name string
		node hfNode
		bad  string // offending value the error must identify
	}{
		{"top-level sha256 is 40-hex", hfNode{Type: "file", Path: "weights.bin", Size: 10, Sha256: hex40}, hex40},
		{"lfs sha256 is 40-hex", hfNode{Type: "file", Path: "weights.bin", Size: 10, LFS: &hfLfsInfo{Sha256: hex40, Size: 10}}, hex40},
		{"lfs oid is 40-hex", hfNode{Type: "file", Path: "weights.bin", Size: 10, LFS: &hfLfsInfo{Oid: hex40, Size: 10}}, hex40},
		{"lfs oid is sha256-prefixed", hfNode{Type: "file", Path: "weights.bin", Size: 10, LFS: &hfLfsInfo{Oid: "sha256:" + hex64, Size: 10}}, "sha256:" + hex64},
		{"truncated hex", hfNode{Type: "file", Path: "weights.bin", Size: 10, Sha256: hex64[:63]}, hex64[:63]},
		{"64 chars but not hex", hfNode{Type: "file", Path: "weights.bin", Size: 10, Sha256: strings.Repeat("g", 64)}, strings.Repeat("g", 64)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := mockHFServerForPlan(t, []hfNode{c.node})
			defer srv.Close()
			_, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "main"}, Settings{Endpoint: srv.URL})
			if err == nil {
				t.Fatal("PlanRepo accepted a non-canonical sha256 from the tree API")
			}
			if !strings.Contains(err.Error(), "sha256") {
				t.Errorf("error should identify the sha256 field, got: %v", err)
			}
			if !strings.Contains(err.Error(), c.bad) {
				t.Errorf("error should identify the offending value %q, got: %v", c.bad, err)
			}
		})
	}
}

// TestPlanRepoLowercasesUppercaseSHA pins the case-handling choice: hex is
// accepted case-insensitively, but the plan stores the canonical lowercase
// spelling so path/verify consumers only ever see "" or lowercase hex.
func TestPlanRepoLowercasesUppercaseSHA(t *testing.T) {
	const hexUpper = "E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855"
	const hexLower = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	files := []hfNode{
		{Type: "file", Path: "a.bin", Size: 5, Sha256: hexUpper},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "main"}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo rejected uppercase canonical hex: %v", err)
	}
	if len(plan.Items) != 1 {
		t.Fatalf("got %d plan items, want 1", len(plan.Items))
	}
	if got := plan.Items[0].SHA256; got != hexLower {
		t.Errorf("item SHA256 = %q, want lowercase %q", got, hexLower)
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
	// The containment proof resolves the configured root to an absolute,
	// cleaned path first, so legitimate destinations resolve under the
	// effective root rather than echoing the configured spelling.
	rootAbs, err := filepath.Abs(cfg.OutputDir)
	if err != nil {
		t.Fatalf("abs(%q): %v", cfg.OutputDir, err)
	}
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
		{Job{Repo: "owner/model"}, filepath.Join(rootAbs, "owner", "model")},
		{Job{Repo: "owner/model", LocalRepo: "vendor/model-a"}, filepath.Join(rootAbs, "vendor", "model-a")},
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
		if !PathInside(rootAbs, got) {
			t.Errorf("destinationBase(%+v) = %q, outside effective root %q", c.job, got, rootAbs)
		}
	}
}

// TestDestinationBaseCwdShapedRootsStayInsideCwd pins the legacy-compat
// contract for cwd-shaped output roots: Settings.OutputDir values of ".",
// "./", and other relative forms must succeed and resolve inside the current
// working directory. filepath.Join cleans a root of "." away entirely, so the
// previous containment proof against the raw configured string rejected these
// legitimate roots as escapes even though they never leave the working
// directory.
func TestDestinationBaseCwdShapedRootsStayInsideCwd(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for _, root := range []string{".", "./", "./Models"} {
		cfg := Settings{OutputDir: root}
		got, err := destinationBase(Job{Repo: "owner/model"}, cfg)
		if err != nil {
			t.Errorf("destinationBase with OutputDir %q: %v", root, err)
			continue
		}
		abs, err := filepath.Abs(got)
		if err != nil {
			t.Errorf("abs(%q): %v", got, err)
			continue
		}
		if !PathInside(cwd, abs) {
			t.Errorf("destinationBase with OutputDir %q = %q resolves to %q, outside cwd %q", root, got, abs, cwd)
		}
		// The containment property: the destination never leaves the
		// effective (resolved) root either.
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			t.Errorf("abs(%q): %v", root, err)
			continue
		}
		if !PathInside(rootAbs, abs) {
			t.Errorf("destinationBase with OutputDir %q = %q resolves to %q, outside resolved root %q", root, got, abs, rootAbs)
		}
	}
}

// TestDestinationBaseFilesystemRootsStayInsideRoot pins the sibling contract
// of the cwd-shaped roots test: OutputDir values that resolve to a
// filesystem root ("/", "/.", "/tmp/..") are legitimate roots, not escapes.
// filepath.Abs collapses each of them to the root itself, and the joined
// destination must be accepted and genuinely inside that root. The
// separator-strict containment proof previously appended a second separator
// to the already separator-terminated root (prefix "//"), so every
// destination under it was rejected as escaping the root. cwd == "/" with a
// cwd-shaped OutputDir resolves through the same Abs path, so it is
// exercised here as well. destinationBase is pure path computation — no
// directories are created — so this test touches no filesystem state
// outside its chdir window.
func TestDestinationBaseFilesystemRootsStayInsideRoot(t *testing.T) {
	// Independent containment predicate (filepath.Rel form), deliberately
	// NOT the PathInside predicate under test: the produced destination
	// must be genuinely inside the resolved root.
	inside := func(root, p string) bool {
		rel, err := filepath.Rel(root, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	check := func(t *testing.T, root string) {
		t.Helper()
		got, err := destinationBase(Job{Repo: "owner/model"}, Settings{OutputDir: root})
		if err != nil {
			t.Errorf("destinationBase with OutputDir %q: %v", root, err)
			return
		}
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			t.Errorf("abs(%q): %v", root, err)
			return
		}
		if want := filepath.Join(rootAbs, "owner", "model"); got != want {
			t.Errorf("destinationBase with OutputDir %q = %q, want %q", root, got, want)
		}
		if !inside(rootAbs, got) {
			t.Errorf("destinationBase with OutputDir %q = %q, not inside resolved root %q", root, got, rootAbs)
		}
	}
	for _, root := range []string{"/", "/.", "/tmp/.."} {
		check(t, root)
	}

	// cwd == "/" with a cwd-shaped OutputDir collapses to the same case:
	// filepath.Abs(".") resolves against the root.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir("/"); err != nil {
		t.Skipf("cannot chdir to filesystem root: %v", err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Errorf("restore cwd %q: %v", cwd, err)
		}
	}()
	check(t, ".")
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
