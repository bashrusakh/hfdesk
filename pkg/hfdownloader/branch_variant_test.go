// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const branchFixtureRepo = "owner/example-EXL3-model"

type branchHubRevision struct {
	sha   string
	files map[string]string
	lfs   map[string]hfLfsInfo
}

// fakeBranchVariantHub serves the minimal Hub API used by the downloader:
// revision metadata, recursive tree listings, and raw file bytes.
func fakeBranchVariantHub(t *testing.T, revisions map[string]branchHubRevision) *httptest.Server {
	t.Helper()
	// A real Hub serves a tree or file request addressed by the resolved commit
	// SHA as readily as one addressed by the branch name. Explicit keys win so a
	// test can give a moved branch and its old commit different contents.
	lookup := func(revision string) (branchHubRevision, bool) {
		if entry, ok := revisions[revision]; ok {
			return entry, true
		}
		for _, entry := range revisions {
			if entry.sha == revision {
				return entry, true
			}
		}
		return branchHubRevision{}, false
	}
	// A content job may address the tree, raw bytes, or LFS resolve URL by
	// commit; all three must resolve to that commit's entry.
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/revision/"):
			revision := path[strings.Index(path, "/revision/")+len("/revision/"):]
			entry, ok := lookup(revision)
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: entry.sha})
		case strings.Contains(path, "/tree/"):
			revision := path[strings.Index(path, "/tree/")+len("/tree/"):]
			entry, ok := lookup(revision)
			if !ok {
				http.NotFound(w, r)
				return
			}
			names := make([]string, 0, len(entry.files))
			for name := range entry.files {
				names = append(names, name)
			}
			sort.Strings(names)
			nodes := make([]hfNode, 0, len(names))
			for _, name := range names {
				node := hfNode{Type: "file", Path: name, Size: int64(len(entry.files[name]))}
				if lfs, ok := entry.lfs[name]; ok {
					lfsCopy := lfs
					node.LFS = &lfsCopy
					if lfsCopy.Size > 0 {
						node.Size = lfsCopy.Size
					}
				}
				nodes = append(nodes, node)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(nodes)
		case strings.Contains(path, "/raw/"), strings.Contains(path, "/resolve/"):
			marker := "/raw/"
			if !strings.Contains(path, marker) {
				marker = "/resolve/"
			}
			rest := path[strings.Index(path, marker)+len(marker):]
			parts := strings.SplitN(rest, "/", 2)
			if len(parts) != 2 {
				http.NotFound(w, r)
				return
			}
			entry, ok := lookup(parts[0])
			if !ok {
				http.NotFound(w, r)
				return
			}
			body, ok := entry.files[parts[1]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, body)
		default:
			t.Logf("unexpected branch fixture request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
}

func branchFixtureRevisions() map[string]branchHubRevision {
	return map[string]branchHubRevision{
		"main": {
			sha: "1111111111111111111111111111111111111111",
			files: map[string]string{
				"config.json":       `{"model_type":"main"}`,
				"model.safetensors": "ordinary-main-weights",
			},
		},
		"4.00bpw": {
			sha: "2222222222222222222222222222222222222222",
			files: map[string]string{
				"config.json":       `{"model_type":"exl3","bits":4}`,
				"model.safetensors": "branch-a-weights",
			},
		},
		"5.00bpw": {
			sha: "3333333333333333333333333333333333333333",
			files: map[string]string{
				"config.json":       `{"model_type":"exl3","bits":5}`,
				"model.safetensors": "branch-b-weights",
			},
		},
	}
}

func downloadBranchFixture(t *testing.T, cfg Settings, revision string) {
	t.Helper()
	job := Job{Repo: branchFixtureRepo, Revision: revision}
	if revision != "main" {
		job.BranchVariant = true
	}
	if err := Download(context.Background(), job, cfg, nil); err != nil {
		t.Fatalf("download %s: %v", revision, err)
	}
}

func readBranchFixtureManifest(t *testing.T, path string) *DownloadManifest {
	t.Helper()
	manifest, err := ReadManifest(path)
	if err != nil {
		t.Fatalf("read manifest %s: %v", path, err)
	}
	return manifest
}

func assertBranchFixtureManifest(t *testing.T, manifest *DownloadManifest, revision string, commit string) {
	t.Helper()
	if !manifest.BranchVariant {
		t.Errorf("%s manifest lost branch_variant", revision)
	}
	if manifest.Repo != branchFixtureRepo || manifest.Branch != revision || manifest.Commit != commit {
		t.Errorf("%s manifest identity = repo %q branch %q commit %q", revision, manifest.Repo, manifest.Branch, manifest.Commit)
	}
	if manifest.CompletedAt.IsZero() || manifest.TotalFiles != 2 || len(manifest.Files) != 2 {
		t.Errorf("%s manifest completion = at %v total %d files %d", revision, manifest.CompletedAt, manifest.TotalFiles, len(manifest.Files))
	}
}

func TestBranchVariantsIsolateIdenticalFileNamesInHFCache(t *testing.T) {
	revisions := branchFixtureRevisions()
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	cacheRoot := t.TempDir()
	cfg := Settings{CacheDir: cacheRoot, Endpoint: hub.URL, Verify: "size"}

	downloadBranchFixture(t, cfg, "4.00bpw")
	downloadBranchFixture(t, cfg, "5.00bpw")

	cache, err := cfg.BuildHFCache()
	if err != nil {
		t.Fatal(err)
	}
	repoDir, err := cache.Repo(branchFixtureRepo, RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}

	for _, revision := range []string{"4.00bpw", "5.00bpw"} {
		commit, err := repoDir.ReadRef(revision)
		if err != nil || commit != revisions[revision].sha {
			t.Fatalf("%s ref = %q err=%v, want %q", revision, commit, err, revisions[revision].sha)
		}
		manifestPath := filepath.Join(repoDir.Path(), BranchVariantMetadataDir, revision, commit, ManifestFilename)
		assertBranchFixtureManifest(t, readBranchFixtureManifest(t, manifestPath), revision, commit)
	}

	commitA := revisions["4.00bpw"].sha
	commitB := revisions["5.00bpw"].sha
	snapshotA := filepath.Join(repoDir.SnapshotsDir(), commitA, "model.safetensors")
	snapshotB := filepath.Join(repoDir.SnapshotsDir(), commitB, "model.safetensors")
	bodyA, err := os.ReadFile(snapshotA)
	if err != nil || string(bodyA) != "branch-a-weights" {
		t.Fatalf("variant A snapshot = %q err=%v", bodyA, err)
	}
	bodyB, err := os.ReadFile(snapshotB)
	if err != nil || string(bodyB) != "branch-b-weights" {
		t.Fatalf("variant B snapshot = %q err=%v", bodyB, err)
	}
	blobA, err := filepath.EvalSymlinks(snapshotA)
	if err != nil {
		t.Fatal(err)
	}
	blobB, err := filepath.EvalSymlinks(snapshotB)
	if err != nil {
		t.Fatal(err)
	}
	if blobA == blobB {
		t.Fatalf("different branch contents resolved to the same blob %q", blobA)
	}
	if !PathInside(repoDir.BlobsDir(), blobA) || !PathInside(repoDir.BlobsDir(), blobB) {
		t.Fatalf("snapshot blobs escaped blobs dir: %q %q", blobA, blobB)
	}

	// The friendly view has no revision dimension, so a branch variant must
	// not place colliding entries there.
	friendly := repoDir.FriendlyPath()
	for _, name := range []string{"model.safetensors", "config.json", ManifestFilename} {
		if _, err := os.Stat(filepath.Join(friendly, name)); !os.IsNotExist(err) {
			t.Errorf("friendly view received branch entry %q (err=%v)", name, err)
		}
	}
}

func TestBranchVariantRedownloadKeepsCompleteManifest(t *testing.T) {
	revisions := branchFixtureRevisions()
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	cacheRoot := t.TempDir()
	cfg := Settings{CacheDir: cacheRoot, Endpoint: hub.URL, Verify: "size"}

	downloadBranchFixture(t, cfg, "4.00bpw")
	downloadBranchFixture(t, cfg, "4.00bpw")

	cache, _ := cfg.BuildHFCache()
	repoDir, _ := cache.Repo(branchFixtureRepo, RepoTypeModel)
	commit := revisions["4.00bpw"].sha
	manifestPath := filepath.Join(repoDir.Path(), BranchVariantMetadataDir, "4.00bpw", commit, ManifestFilename)
	manifest := readBranchFixtureManifest(t, manifestPath)
	assertBranchFixtureManifest(t, manifest, "4.00bpw", commit)

	snapshotDir := filepath.Join(repoDir.SnapshotsDir(), commit)
	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("snapshot has %d entries after re-download, want 2", len(entries))
	}
	body, err := os.ReadFile(filepath.Join(snapshotDir, "model.safetensors"))
	if err != nil || string(body) != "branch-a-weights" {
		t.Fatalf("re-download corrupted snapshot: %q err=%v", body, err)
	}
	commits, err := repoDir.ListSnapshots()
	if err != nil || len(commits) != 1 || commits[0] != commit {
		t.Fatalf("re-download created extra snapshots: %v err=%v", commits, err)
	}
}

func TestOrdinaryDownloadPreservesFriendlyViewBesideBranchVariant(t *testing.T) {
	revisions := branchFixtureRevisions()
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	cacheRoot := t.TempDir()
	cfg := Settings{CacheDir: cacheRoot, Endpoint: hub.URL, Verify: "size"}

	downloadBranchFixture(t, cfg, "4.00bpw")
	downloadBranchFixture(t, cfg, "main")

	cache, _ := cfg.BuildHFCache()
	repoDir, _ := cache.Repo(branchFixtureRepo, RepoTypeModel)
	friendly := repoDir.FriendlyPath()
	body, err := os.ReadFile(filepath.Join(friendly, "model.safetensors"))
	if err != nil || string(body) != "ordinary-main-weights" {
		t.Fatalf("ordinary friendly entry = %q err=%v", body, err)
	}
	ordinaryManifest := readBranchFixtureManifest(t, filepath.Join(friendly, ManifestFilename))
	if ordinaryManifest.BranchVariant || ordinaryManifest.Branch != "main" {
		t.Fatalf("ordinary manifest = %+v", ordinaryManifest)
	}

	// The ordinary friendly entry must not replace the isolated branch
	// snapshot, and the branch manifest must remain complete.
	commit := revisions["4.00bpw"].sha
	branchBody, err := os.ReadFile(filepath.Join(repoDir.SnapshotsDir(), commit, "model.safetensors"))
	if err != nil || string(branchBody) != "branch-a-weights" {
		t.Fatalf("branch snapshot after ordinary download = %q err=%v", branchBody, err)
	}
	assertBranchFixtureManifest(t, readBranchFixtureManifest(t, filepath.Join(repoDir.Path(), BranchVariantMetadataDir, "4.00bpw", commit, ManifestFilename)), "4.00bpw", commit)
}

func TestLocalBranchVariantsUseRevisionDirectories(t *testing.T) {
	revisions := branchFixtureRevisions()
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	root := t.TempDir()
	cfg := Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}

	downloadBranchFixture(t, cfg, "4.00bpw")
	downloadBranchFixture(t, cfg, "5.00bpw")
	downloadBranchFixture(t, cfg, "main")

	repoRoot := filepath.Join(root, branchFixtureRepo)
	for revision, want := range map[string]string{
		"4.00bpw": "branch-a-weights",
		"5.00bpw": "branch-b-weights",
	} {
		body, err := os.ReadFile(filepath.Join(repoRoot, revision, "model.safetensors"))
		if err != nil || string(body) != want {
			t.Errorf("%s local file = %q err=%v", revision, body, err)
		}
		manifest := readBranchFixtureManifest(t, filepath.Join(repoRoot, BranchVariantMetadataDir, revision, revisions[revision].sha, ManifestFilename))
		assertBranchFixtureManifest(t, manifest, revision, revisions[revision].sha)
		for _, file := range manifest.Files {
			if file.Blob != "" {
				t.Errorf("%s local manifest blob = %q, want empty", revision, file.Blob)
			}
		}
	}
	body, err := os.ReadFile(filepath.Join(repoRoot, "model.safetensors"))
	if err != nil || string(body) != "ordinary-main-weights" {
		t.Fatalf("ordinary local file = %q err=%v", body, err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ManifestFilename)); !os.IsNotExist(err) {
		t.Errorf("ordinary local download gained a manifest (err=%v)", err)
	}
}

func TestLocalBranchVariantSkippedLFSManifestOmitsBlob(t *testing.T) {
	const revision = "6.00bpw"
	const body = "lfs-local-branch"
	sum := sha256.Sum256([]byte(body))
	oid := hex.EncodeToString(sum[:])
	revisions := map[string]branchHubRevision{
		revision: {
			sha:   "4444444444444444444444444444444444444444",
			files: map[string]string{"model.safetensors": body},
			lfs: map[string]hfLfsInfo{
				"model.safetensors": {Oid: oid, Size: int64(len(body))},
			},
		},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	root := t.TempDir()
	cfg := Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}

	// The expected destination already exists with the LFS content, so the
	// download takes the skip path and records the file from it.
	target := filepath.Join(root, branchFixtureRepo, revision, "model.safetensors")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	var skipped bool
	err := Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: revision, BranchVariant: true}, cfg, func(ev ProgressEvent) {
		if ev.Event == "file_done" && strings.Contains(ev.Message, "skip") {
			skipped = true
		}
	})
	if err != nil {
		t.Fatalf("download skipped local branch: %v", err)
	}
	if !skipped {
		t.Fatal("local branch download did not take the skip path")
	}

	manifest := readBranchFixtureManifest(t, filepath.Join(root, branchFixtureRepo, BranchVariantMetadataDir, revision, revisions[revision].sha, ManifestFilename))
	if !manifest.BranchVariant || manifest.Branch != revision || len(manifest.Files) != 1 {
		t.Fatalf("local skipped manifest = %+v", manifest)
	}
	file := manifest.Files[0]
	if !file.LFS {
		t.Fatalf("local skipped manifest did not record LFS provenance: %+v", file)
	}
	if file.Blob != "" {
		t.Fatalf("local LFS manifest blob = %q, want empty", file.Blob)
	}
}

func TestBranchVariantNoManifestStillWritesCompletionManifest(t *testing.T) {
	revisions := branchFixtureRevisions()
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	cacheRoot := t.TempDir()
	cfg := Settings{CacheDir: cacheRoot, Endpoint: hub.URL, Verify: "size", NoManifest: true, NoFriendlyView: true}

	downloadBranchFixture(t, cfg, "4.00bpw")

	cache, _ := cfg.BuildHFCache()
	repoDir, _ := cache.Repo(branchFixtureRepo, RepoTypeModel)
	commit := revisions["4.00bpw"].sha
	if body, err := os.ReadFile(filepath.Join(repoDir.SnapshotsDir(), commit, "model.safetensors")); err != nil || string(body) != "branch-a-weights" {
		t.Fatalf("no-manifest branch snapshot = %q err=%v", body, err)
	}
	// NoManifest suppresses only the optional friendly-view manifest. The
	// completion record is what presence reads, so it is still written.
	manifestPath := filepath.Join(repoDir.Path(), BranchVariantMetadataDir, "4.00bpw", commit, ManifestFilename)
	assertBranchFixtureManifest(t, readBranchFixtureManifest(t, manifestPath), "4.00bpw", commit)
	if _, err := os.Stat(filepath.Join(repoDir.FriendlyPath(), ManifestFilename)); !os.IsNotExist(err) {
		t.Fatalf("NoManifest friendly manifest was written (err=%v)", err)
	}
}

func TestBranchVariantRequiresResolvedCommitBeforeWriting(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tree/") {
			_ = json.NewEncoder(w).Encode([]hfNode{{Type: "file", Path: "model.safetensors", Size: 4}})
			return
		}
		http.NotFound(w, r)
	}))
	defer hub.Close()
	cacheRoot := t.TempDir()
	cfg := Settings{CacheDir: cacheRoot, Endpoint: hub.URL, Verify: "size"}

	err := Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: "4.00bpw", BranchVariant: true}, cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "resolved commit") {
		t.Fatalf("unresolved-commit branch download error = %v", err)
	}
	cache, _ := cfg.BuildHFCache()
	repoDir, _ := cache.Repo(branchFixtureRepo, RepoTypeModel)
	if _, statErr := os.Stat(repoDir.Path()); !os.IsNotExist(statErr) {
		t.Fatalf("branch download wrote cache state before resolving commit: %v", statErr)
	}
}

// TestPlanAndDownloadUseResolvedCommitForTreeAndFiles discriminates defect 1:
// the branch name moves between the revision lookup and the content reads. The
// revision endpoint returns the old commit, while the branch name now serves
// new content. Both the tree and the file URL must use the resolved commit.
func TestPlanAndDownloadUseResolvedCommitForTreeAndFiles(t *testing.T) {
	const (
		branch = "4.00bpw"
		commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	const (
		oldConfig  = `{"model_type":"exl3","bits":4,"generation":"old"}`
		oldWeights = "old-branch-weights"
	)
	revisions := map[string]branchHubRevision{
		// The revision lookup still resolves to commit; the branch has since
		// moved to different content and gained a file.
		branch: {
			sha: commit,
			files: map[string]string{
				"config.json":       `{"model_type":"exl3","bits":4,"generation":"new"}`,
				"model.safetensors": "new-branch-weights",
				"tokenizer.json":    "new-tokenizer",
				"new-only.json":     "new",
			},
			lfs: map[string]hfLfsInfo{
				"tokenizer.json": {Size: int64(len("new-tokenizer"))},
			},
		},
		// The resolved commit holds the plan and bytes the revision named.
		commit: {
			sha: commit,
			files: map[string]string{
				"config.json":       oldConfig,
				"model.safetensors": oldWeights,
				"tokenizer.json":    "old-tokenizer",
			},
			lfs: map[string]hfLfsInfo{
				"tokenizer.json": {Size: int64(len("old-tokenizer"))},
			},
		},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	cfg := Settings{CacheDir: t.TempDir(), Endpoint: hub.URL, Verify: "size"}

	plan, err := PlanRepo(context.Background(), Job{Repo: branchFixtureRepo, Revision: branch, BranchVariant: true}, cfg)
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	if plan.Commit != commit || !plan.CommitResolved {
		t.Fatalf("plan commit = %q resolved=%v, want %q resolved", plan.Commit, plan.CommitResolved, commit)
	}
	if len(plan.Items) != 3 {
		t.Fatalf("plan items = %+v, want the three files at commit %s", plan.Items, commit)
	}
	for _, item := range plan.Items {
		marker := "/raw/"
		if item.LFS {
			marker = "/resolve/"
		}
		wantURL := hub.URL + "/" + branchFixtureRepo + marker + commit + "/" + item.RelativePath
		if item.URL != wantURL {
			t.Errorf("%s URL = %q, want commit-pinned %q", item.RelativePath, item.URL, wantURL)
		}
	}

	downloadBranchFixture(t, cfg, branch)

	cache, err := cfg.BuildHFCache()
	if err != nil {
		t.Fatal(err)
	}
	repoDir, err := cache.Repo(branchFixtureRepo, RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(repoDir.SnapshotsDir(), commit)
	for name, want := range map[string]string{"config.json": oldConfig, "model.safetensors": oldWeights, "tokenizer.json": "old-tokenizer"} {
		body, err := os.ReadFile(filepath.Join(snapshot, name))
		if err != nil || string(body) != want {
			t.Errorf("%s payload = %q err=%v, want old commit content %q", name, body, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(snapshot, "new-only.json")); !os.IsNotExist(err) {
		t.Errorf("plan used the moved branch tree: new-only.json exists (err=%v)", err)
	}
}

// TestBranchVariantsSharingCommitKeepDistinctManifests discriminates defect 2:
// keying the completion record by commit alone let the second branch overwrite
// the first branch's record and its revision reported not downloaded.
func TestBranchVariantsSharingCommitKeepDistinctManifests(t *testing.T) {
	const commit = "9999999999999999999999999999999999999999"
	files := map[string]string{
		"config.json":       `{"model_type":"exl3","bits":4}`,
		"model.safetensors": "shared-commit-weights",
	}
	revisions := map[string]branchHubRevision{
		"4.00bpw": {sha: commit, files: files},
		"5.00bpw": {sha: commit, files: files},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	cfg := Settings{CacheDir: t.TempDir(), Endpoint: hub.URL, Verify: "size"}

	downloadBranchFixture(t, cfg, "4.00bpw")
	downloadBranchFixture(t, cfg, "5.00bpw")

	cache, err := cfg.BuildHFCache()
	if err != nil {
		t.Fatal(err)
	}
	repoDir, err := cache.Repo(branchFixtureRepo, RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{"4.00bpw", "5.00bpw"} {
		manifestPath := filepath.Join(repoDir.Path(), BranchVariantMetadataDir, revision, commit, ManifestFilename)
		manifest := readBranchFixtureManifest(t, manifestPath)
		assertBranchFixtureManifest(t, manifest, revision, commit)
	}
}

// TestBranchVariantCompletionManifestFailureFailsDownload discriminates
// defect 3: a completion-record write failure used to be a warning, so the job
// reported done even though presence could never report the variant downloaded.
func TestBranchVariantCompletionManifestFailureFailsDownload(t *testing.T) {
	revisions := branchFixtureRevisions()
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	cfg := Settings{CacheDir: t.TempDir(), Endpoint: hub.URL, Verify: "size"}

	cache, err := cfg.BuildHFCache()
	if err != nil {
		t.Fatal(err)
	}
	repoDir, err := cache.Repo(branchFixtureRepo, RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repoDir.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// A regular file where the app-owned metadata directory belongs makes the
	// completion-manifest write impossible after every payload file succeeds.
	if err := os.WriteFile(filepath.Join(repoDir.Path(), BranchVariantMetadataDir), []byte("blocked"), 0644); err != nil {
		t.Fatal(err)
	}

	var done, errorEvent bool
	err = Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: "4.00bpw", BranchVariant: true}, cfg, func(ev ProgressEvent) {
		switch ev.Event {
		case "done":
			done = true
		case "error":
			errorEvent = true
		}
	})
	if err == nil || !strings.Contains(err.Error(), "completion manifest") {
		t.Fatalf("obstructed completion manifest error = %v", err)
	}
	if done {
		t.Fatal("failed branch variant still reported done")
	}
	if !errorEvent {
		t.Fatal("failed branch variant did not emit an error progress event")
	}
}

// TestBranchVariantRefFailureFailsDownload covers the other half of defect 3:
// presence requires the ref as well as the completion manifest, so a failed
// ref write must fail the job instead of reporting done.
func TestBranchVariantRefFailureFailsDownload(t *testing.T) {
	revisions := branchFixtureRevisions()
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	cfg := Settings{CacheDir: t.TempDir(), Endpoint: hub.URL, Verify: "size"}

	cache, err := cfg.BuildHFCache()
	if err != nil {
		t.Fatal(err)
	}
	repoDir, err := cache.Repo(branchFixtureRepo, RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repoDir.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// A directory where the ref file belongs makes WriteRef fail.
	if err := os.MkdirAll(filepath.Join(repoDir.RefsDir(), "4.00bpw"), 0755); err != nil {
		t.Fatal(err)
	}

	var done, errorEvent bool
	err = Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: "4.00bpw", BranchVariant: true}, cfg, func(ev ProgressEvent) {
		switch ev.Event {
		case "done":
			done = true
		case "error":
			errorEvent = true
		}
	})
	if err == nil || !strings.Contains(err.Error(), "write ref") {
		t.Fatalf("obstructed ref error = %v", err)
	}
	if done {
		t.Fatal("failed branch variant still reported done")
	}
	if !errorEvent {
		t.Fatal("failed branch variant did not emit an error progress event")
	}
}

// TestLocalBranchVariantManifestDoesNotOverwriteRepositoryFile discriminates
// defect 4: a repository-owned hfd.yaml must remain payload, never be replaced
// by the completion record, which lives outside the revision payload tree.
func TestLocalBranchVariantManifestDoesNotOverwriteRepositoryFile(t *testing.T) {
	const (
		revision           = "7.00bpw"
		commit             = "7777777777777777777777777777777777777777"
		repositoryManifest = "# repository-owned hfd.yaml\n"
	)
	revisions := map[string]branchHubRevision{
		revision: {
			sha: commit,
			files: map[string]string{
				ManifestFilename:    repositoryManifest,
				"model.safetensors": "branch-weights",
			},
		},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	root := t.TempDir()
	cfg := Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}

	if err := Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: revision, BranchVariant: true}, cfg, nil); err != nil {
		t.Fatalf("local branch download: %v", err)
	}

	payloadDir := filepath.Join(root, branchFixtureRepo, revision)
	payload, err := os.ReadFile(filepath.Join(payloadDir, ManifestFilename))
	if err != nil || string(payload) != repositoryManifest {
		t.Fatalf("payload hfd.yaml = %q err=%v, want repository content %q", payload, err, repositoryManifest)
	}
	manifestPath := filepath.Join(root, branchFixtureRepo, BranchVariantMetadataDir, revision, commit, ManifestFilename)
	if PathInside(payloadDir, manifestPath) {
		t.Fatalf("completion manifest %q is inside the payload tree %q", manifestPath, payloadDir)
	}
	manifest := readBranchFixtureManifest(t, manifestPath)
	assertBranchFixtureManifest(t, manifest, revision, commit)
}

// TestBranchVariantManifestDirKeysByRevisionAndCommit pins the completion
// layout for a slash-containing revision and rejects revision components that
// would escape or collapse the metadata path.
func TestBranchVariantManifestDirKeysByRevisionAndCommit(t *testing.T) {
	const (
		commit   = "1234567890123456789012345678901234567890"
		revision = "exl/4_25"
	)
	cache := NewHFCacheResolved(t.TempDir(), "", 0)
	repoDir, err := cache.Repo(branchFixtureRepo, RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	got, err := BranchVariantManifestDir(repoDir, revision, commit)
	if err != nil {
		t.Fatalf("BranchVariantManifestDir: %v", err)
	}
	want := filepath.Join(repoDir.Path(), BranchVariantMetadataDir, "exl", "4_25", commit)
	if got != want || !PathInside(repoDir.Path(), got) {
		t.Fatalf("manifest dir = %q, want %q inside repository", got, want)
	}

	localBase := t.TempDir()
	localGot, err := BranchVariantLocalManifestDir(localBase, revision, commit)
	if err != nil {
		t.Fatalf("BranchVariantLocalManifestDir: %v", err)
	}
	localWant := filepath.Join(localBase, BranchVariantMetadataDir, "exl", "4_25", commit)
	if localGot != localWant || !PathInside(localBase, localGot) {
		t.Fatalf("local manifest dir = %q, want %q inside base", localGot, localWant)
	}

	for _, bad := range []string{"", ".", "..", "../x", "a/../../b", "a//b", `a\b`, "/abs", "exl/."} {
		if _, err := BranchVariantManifestDir(repoDir, bad, commit); err == nil {
			t.Errorf("unsafe revision %q accepted", bad)
		}
		if _, err := BranchVariantLocalManifestDir(localBase, bad, commit); err == nil {
			t.Errorf("unsafe local revision %q accepted", bad)
		}
	}
	if _, err := BranchVariantManifestDir(repoDir, revision, "../commit"); err == nil {
		t.Error("unsafe commit accepted")
	}
}

// TestBranchVariantStagingNameSeparatesCollidingHashlessPaths discriminates
// defect 5: replacing "/" with "_" made a/b_c and a_b/c share one staging file.
func TestBranchVariantStagingNameSeparatesCollidingHashlessPaths(t *testing.T) {
	const commit = "8888888888888888888888888888888888888888"
	names := []string{
		branchVariantStagingName(commit, PlanItem{RelativePath: "a/b_c"}),
		branchVariantStagingName(commit, PlanItem{RelativePath: "a_b/c"}),
	}
	if names[0] == names[1] {
		t.Fatalf("collapsing paths share staging name %q", names[0])
	}
	prefix := "tmp-" + commit + "-"
	for i, name := range names {
		if !strings.HasPrefix(name, prefix) {
			t.Errorf("staging name %q lacks commit-scoped prefix %q", name, prefix)
		}
		if strings.ContainsAny(name, `/\`) {
			t.Errorf("staging name %q contains a path separator", name)
		}
		digest := strings.TrimPrefix(name, prefix)
		if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != sha256.Size {
			t.Errorf("staging name %d digest %q is not a hex sha256 (err=%v)", i, digest, err)
		}
	}

	const contentSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := branchVariantStagingName(commit, PlanItem{RelativePath: "a/b_c", SHA256: contentSHA}); got != "tmp-"+contentSHA {
		t.Errorf("content-addressed staging name = %q, want tmp-%s", got, contentSHA)
	}
}

// TestBranchVariantDownloadsHashlessPathsWithCollidingStagingNames is the
// end-to-end counterpart of the staging-name unit test: both files must land
// with their own contents even when downloaded concurrently.
func TestBranchVariantDownloadsHashlessPathsWithCollidingStagingNames(t *testing.T) {
	const (
		revision = "8.00bpw"
		commit   = "8888888888888888888888888888888888888888"
	)
	files := map[string]string{
		"a/b_c": "slash-separated-path-body",
		"a_b/c": "underscore-joined-path-body",
	}
	revisions := map[string]branchHubRevision{
		revision: {sha: commit, files: files},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	cfg := Settings{CacheDir: t.TempDir(), Endpoint: hub.URL, Verify: "size", MaxActiveDownloads: 2}

	downloadBranchFixture(t, cfg, revision)

	cache, err := cfg.BuildHFCache()
	if err != nil {
		t.Fatal(err)
	}
	repoDir, err := cache.Repo(branchFixtureRepo, RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	blobs := make(map[string]string, len(files))
	for rel, want := range files {
		snapshot := filepath.Join(repoDir.SnapshotsDir(), commit, filepath.FromSlash(rel))
		body, err := os.ReadFile(snapshot)
		if err != nil || string(body) != want {
			t.Errorf("%s payload = %q err=%v, want %q", rel, body, err, want)
		}
		blob, err := filepath.EvalSymlinks(snapshot)
		if err != nil {
			t.Errorf("%s blob resolution: %v", rel, err)
			continue
		}
		if !PathInside(repoDir.BlobsDir(), blob) {
			t.Errorf("%s blob %q escaped blobs dir", rel, blob)
		}
		blobs[rel] = blob
	}
	if blobs["a/b_c"] != "" && blobs["a/b_c"] == blobs["a_b/c"] {
		t.Errorf("distinct paths resolved to the same blob %q", blobs["a/b_c"])
	}
}

// TestLocalBranchVariantInvalidationFailureFailsBeforePayload pins the local
// re-download contract: if the prior completion record cannot be invalidated,
// the download fails before creating or changing the payload directory.
func TestLocalBranchVariantInvalidationFailureFailsBeforePayload(t *testing.T) {
	const revision = "4.00bpw"
	revisions := branchFixtureRevisions()
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	root := t.TempDir()
	repoBase := filepath.Join(root, branchFixtureRepo)
	if err := os.MkdirAll(repoBase, 0755); err != nil {
		t.Fatal(err)
	}
	// A regular file where the app-owned metadata directory belongs makes the
	// invalidation helper fail.
	if err := os.WriteFile(filepath.Join(repoBase, BranchVariantMetadataDir), []byte("blocked"), 0644); err != nil {
		t.Fatal(err)
	}

	var done, errorEvent bool
	err := Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: revision, BranchVariant: true},
		Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}, func(ev ProgressEvent) {
			switch ev.Event {
			case "done":
				done = true
			case "error":
				errorEvent = true
			}
		})
	if err == nil || !strings.Contains(err.Error(), "invalidate") {
		t.Fatalf("obstructed invalidation error = %v", err)
	}
	if done {
		t.Fatal("failed branch variant still reported done")
	}
	if !errorEvent {
		t.Fatal("failed branch variant did not emit an error progress event")
	}
	if _, statErr := os.Stat(filepath.Join(repoBase, revision)); !os.IsNotExist(statErr) {
		t.Fatalf("payload directory was created after invalidation failure: %v", statErr)
	}
}
