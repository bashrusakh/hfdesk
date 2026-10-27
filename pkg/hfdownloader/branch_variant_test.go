// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const branchFixtureRepo = "owner/example-EXL3-model"

// branchLocalSpaceDir returns the app-owned reserved space of the fixture
// repository under root. The path is spelled out literally, so the test pins
// the layout instead of re-deriving it from the implementation.
func branchLocalSpaceDir(root string) string {
	return filepath.Join(root, BranchVariantLocalSpaceName, "owner", "example-EXL3-model")
}

// branchLocalPayloadDir returns the payload directory of one revision in the
// fixture repository's reserved space.
func branchLocalPayloadDir(root, revision string) string {
	return filepath.Join(branchLocalSpaceDir(root), "payload", filepath.FromSlash(revision))
}

// branchLocalRecordsDir returns the completion-record subtree of the fixture
// repository's reserved space.
func branchLocalRecordsDir(root string) string {
	return filepath.Join(branchLocalSpaceDir(root), "records")
}

// branchLocalRecordDir returns the completion-record directory of one
// revision at one commit in the fixture repository's reserved space.
func branchLocalRecordDir(root, revision, commit string) string {
	return filepath.Join(branchLocalRecordsDir(root), filepath.FromSlash(revision), commit)
}

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
		payloadDir := branchLocalPayloadDir(root, revision)
		body, err := os.ReadFile(filepath.Join(payloadDir, "model.safetensors"))
		if err != nil || string(body) != want {
			t.Errorf("%s local file = %q err=%v", revision, body, err)
		}
		manifest := readBranchFixtureManifest(t, filepath.Join(branchLocalRecordDir(root, revision, revisions[revision].sha), ManifestFilename))
		assertBranchFixtureManifest(t, manifest, revision, revisions[revision].sha)
		for _, file := range manifest.Files {
			if file.Blob != "" {
				t.Errorf("%s local manifest blob = %q, want empty", revision, file.Blob)
			}
		}
		// The payload must not live at a repository-relative path: a
		// repository directory named like the revision would otherwise be
		// overwritten by the branch payload.
		if _, err := os.Stat(filepath.Join(repoRoot, revision)); !os.IsNotExist(err) {
			t.Errorf("%s local payload used the revision-named directory (err=%v)", revision, err)
		}
		// The reserved space lives outside the ordinary repository tree, so
		// the two never share a path.
		if PathInside(repoRoot, payloadDir) {
			t.Errorf("%s local payload %q is inside the repository tree %q", revision, payloadDir, repoRoot)
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
	payloadDir := branchLocalPayloadDir(root, revision)
	target := filepath.Join(payloadDir, "model.safetensors")
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

	manifest := readBranchFixtureManifest(t, filepath.Join(branchLocalRecordDir(root, revision, revisions[revision].sha), ManifestFilename))
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

	var (
		mu     sync.Mutex
		events []ProgressEvent
	)
	err := Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: "4.00bpw", BranchVariant: true}, cfg, func(ev ProgressEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	assertBranchVariantUnresolvedCommitFailure(t, err, events, "4.00bpw")
	cache, _ := cfg.BuildHFCache()
	repoDir, _ := cache.Repo(branchFixtureRepo, RepoTypeModel)
	if _, statErr := os.Stat(repoDir.Path()); !os.IsNotExist(statErr) {
		t.Fatalf("branch download wrote cache state before resolving commit: %v", statErr)
	}
}

// assertBranchVariantUnresolvedCommitFailure asserts the shared failure
// contract of a branch variant whose commit was not resolved: the job fails
// with the resolved-commit refusal, the failure is reported as an error event
// rather than a done event, and the refusal reaches the job outcome as the
// returned error. Both storage modes answer the same way, so the assertion is
// shared by the HF-cache and the local test.
func assertBranchVariantUnresolvedCommitFailure(t *testing.T, err error, events []ProgressEvent, revision string) {
	t.Helper()
	want := fmt.Sprintf("branch variant %q requires a resolved commit from the Hub", revision)
	if err == nil {
		t.Fatal("branch variant without a resolved commit reported success")
	}
	if err.Error() != want {
		t.Fatalf("branch variant error = %q, want %q", err, want)
	}
	errorEvent := false
	for _, ev := range events {
		switch ev.Event {
		case "done":
			t.Fatal("branch variant without a resolved commit reported done")
		case "error":
			errorEvent = true
			if ev.Message != want {
				t.Errorf("error event message = %q, want %q", ev.Message, want)
			}
		}
	}
	if !errorEvent {
		t.Fatal("branch variant without a resolved commit did not emit an error progress event")
	}
}

// unresolvedBranchHub serves a repository whose revision lookup fails: the
// revision endpoint answers 404, so the plan cannot resolve a commit and
// falls back to the revision name, while the tree and raw-file endpoints
// still serve the branch name. The returned accessor reports the revisions
// the content requests addressed, so a test can prove which location a
// download read.
//
// The served revision is matched exactly, so a request that addressed any
// other location gets no content: the fixture can then tell a download that
// read the branch name from one that read something else.
func unresolvedBranchHub(t *testing.T, revision string, files map[string]string) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu       sync.Mutex
		readFrom []string
	)
	record := func(addressed string) {
		mu.Lock()
		readFrom = append(readFrom, addressed)
		mu.Unlock()
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/revision/"):
			// The Hub cannot resolve the revision: the plan carries the
			// revision name where a commit belongs.
			http.NotFound(w, r)
		case strings.Contains(path, "/tree/"):
			addressed := path[strings.Index(path, "/tree/")+len("/tree/"):]
			if addressed != revision {
				http.NotFound(w, r)
				return
			}
			record(addressed)
			names := make([]string, 0, len(files))
			for name := range files {
				names = append(names, name)
			}
			sort.Strings(names)
			nodes := make([]hfNode, 0, len(names))
			for _, name := range names {
				node := hfNode{Type: "file", Path: name, Size: int64(len(files[name]))}
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
			prefix := revision + "/"
			if !strings.HasPrefix(rest, prefix) {
				http.NotFound(w, r)
				return
			}
			record(revision)
			body, ok := files[strings.TrimPrefix(rest, prefix)]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, body)
		default:
			t.Logf("unexpected unresolved-commit fixture request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), readFrom...)
	}
}

// TestLocalOrdinaryDownloadRefusesReservedOwnerName pins the reserved name:
// an ordinary local download of a repository whose owner is the app's reserved
// branch-variant space name must be refused with a clear error instead of
// writing into the app's own space.
//
// The name is reserved for the owner component only: a repository whose NAME
// is the reserved name lives one level deeper and never collides with the
// space, so it must still download.
func TestLocalOrdinaryDownloadRefusesReservedOwnerName(t *testing.T) {
	const reservedOwner = ".hfdesk-branch-variants"
	revisions := map[string]branchHubRevision{
		"main": {
			sha: "1111111111111111111111111111111111111111",
			files: map[string]string{
				"config.json":       `{"model_type":"main"}`,
				"model.safetensors": "ordinary-main-weights",
			},
		},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()

	t.Run("reserved owner is refused", func(t *testing.T) {
		root := t.TempDir()
		err := Download(context.Background(), Job{Repo: reservedOwner + "/model", Revision: "main"},
			Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}, nil)
		if err == nil {
			t.Fatal("ordinary download with the reserved owner name succeeded")
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("ordinary download error = %v, want the reserved-name refusal", err)
		}
		// Nothing was written into the app's own space.
		if _, statErr := os.Stat(filepath.Join(root, reservedOwner)); !os.IsNotExist(statErr) {
			t.Fatalf("ordinary download wrote into the reserved space (err=%v)", statErr)
		}
	})

	t.Run("reserved name is allowed", func(t *testing.T) {
		root := t.TempDir()
		repo := "owner/" + reservedOwner
		if err := Download(context.Background(), Job{Repo: repo, Revision: "main"},
			Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}, nil); err != nil {
			t.Fatalf("ordinary download with the reserved repository name: %v", err)
		}
		body, err := os.ReadFile(filepath.Join(root, "owner", reservedOwner, "config.json"))
		if err != nil || string(body) != `{"model_type":"main"}` {
			t.Fatalf("ordinary payload = %q err=%v, want the repository's own bytes", body, err)
		}
	})
}

// TestLocalBranchVariantPreservesFormerAppOwnedRepositoryFiles discriminates
// the layout defect: the app's own trees used to live inside the ordinary
// repository tree, so a repository whose own tree carried the app's directory
// names lost those files to a branch-variant download of the same repository.
// The branch variant now writes only inside the reserved space, so both files
// survive with their original content.
func TestLocalBranchVariantPreservesFormerAppOwnedRepositoryFiles(t *testing.T) {
	const (
		revision = "4.00bpw"
		commit   = "2222222222222222222222222222222222222222"
	)
	// The repository's own tree carries files at both of the app's former
	// directory names.
	formerPayloadFile := ".hfdesk-variants-payload/" + revision + "/config.json"
	formerRecordFile := ".hfdesk-variants/" + revision + "/config.json"
	formerPayloadBytes := `{"model_type":"main","generation":"ordinary-payload"}`
	formerRecordBytes := `{"model_type":"main","generation":"ordinary-record"}`
	revisions := map[string]branchHubRevision{
		"main": {
			sha: "1111111111111111111111111111111111111111",
			files: map[string]string{
				formerPayloadFile:   formerPayloadBytes,
				formerRecordFile:    formerRecordBytes,
				"model.safetensors": "ordinary-main-weights",
			},
		},
		revision: {
			sha: commit,
			files: map[string]string{
				"config.json":       `{"model_type":"exl3","bits":4}`,
				"model.safetensors": "branch-a-weights",
			},
		},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	root := t.TempDir()
	cfg := Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}
	repoBase := filepath.Join(root, branchFixtureRepo)

	// The ordinary download writes the repository's own tree, including the
	// files at the app's former names.
	downloadBranchFixture(t, cfg, "main")
	for path, want := range map[string]string{
		formerPayloadFile: formerPayloadBytes,
		formerRecordFile:  formerRecordBytes,
	} {
		got, err := os.ReadFile(filepath.Join(repoBase, filepath.FromSlash(path)))
		if err != nil || string(got) != want {
			t.Fatalf("ordinary file %s = %q err=%v, want %q", path, got, err, want)
		}
	}

	// The branch-variant download of the same revision must leave both files
	// alone.
	downloadBranchFixture(t, cfg, revision)
	for path, want := range map[string]string{
		formerPayloadFile: formerPayloadBytes,
		formerRecordFile:  formerRecordBytes,
	} {
		got, err := os.ReadFile(filepath.Join(repoBase, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("repository file %s was removed by the branch download: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("repository file %s = %q, want %q", path, got, want)
		}
	}

	// The branch variant's own payload and completion record are complete in
	// the reserved space.
	payload, err := os.ReadFile(filepath.Join(branchLocalPayloadDir(root, revision), "model.safetensors"))
	if err != nil || string(payload) != "branch-a-weights" {
		t.Fatalf("branch payload = %q err=%v, want %q", payload, err, "branch-a-weights")
	}
	manifest := readBranchFixtureManifest(t, filepath.Join(branchLocalRecordDir(root, revision, commit), ManifestFilename))
	assertBranchFixtureManifest(t, manifest, revision, commit)
}

// TestLocalBranchVariantRequiresResolvedCommitBeforeWriting discriminates the
// unresolved-commit defect in local mode: the resolved-commit requirement was
// enforced only in HF-cache mode, so a local branch variant whose revision
// lookup failed kept going, wrote the whole payload, and only then failed —
// with the revision name where a commit belongs, or with a record path that
// does not even exist for a revision that contains a slash. The download must
// fail before it writes anything, and it must leave nothing behind.
func TestLocalBranchVariantRequiresResolvedCommitBeforeWriting(t *testing.T) {
	for _, revision := range []string{"4.00bpw", "exl/4_25"} {
		t.Run(revision, func(t *testing.T) {
			hub, _ := unresolvedBranchHub(t, revision, map[string]string{
				"config.json":       `{"model_type":"exl3","bits":4}`,
				"model.safetensors": "branch-a-weights",
			})
			defer hub.Close()
			root := t.TempDir()
			cfg := Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}
			job := Job{Repo: branchFixtureRepo, Revision: revision, BranchVariant: true}

			// The fixture produces exactly the condition under test: a
			// healthy plan whose commit is the unresolved revision name.
			plan, err := PlanRepo(context.Background(), job, cfg)
			if err != nil {
				t.Fatalf("PlanRepo: %v", err)
			}
			if plan.CommitResolved || plan.Commit != revision {
				t.Fatalf("plan commit = %q resolved=%v, want the unresolved revision name %q", plan.Commit, plan.CommitResolved, revision)
			}
			if len(plan.Items) == 0 {
				t.Fatal("plan has no items, the fixture cannot prove a download would have written a payload")
			}

			var (
				mu     sync.Mutex
				events []ProgressEvent
			)
			err = Download(context.Background(), job, cfg, func(ev ProgressEvent) {
				mu.Lock()
				events = append(events, ev)
				mu.Unlock()
			})
			assertBranchVariantUnresolvedCommitFailure(t, err, events, revision)

			// Nothing is left behind: no payload, no completion record and no
			// directory created for the variant.
			repoBase := filepath.Join(root, branchFixtureRepo)
			if _, statErr := os.Stat(repoBase); !os.IsNotExist(statErr) {
				t.Fatalf("branch download created the variant directory (err=%v)", statErr)
			}
			if _, statErr := os.Stat(branchLocalSpaceDir(root)); !os.IsNotExist(statErr) {
				t.Fatalf("branch download created the reserved space (err=%v)", statErr)
			}
		})
	}
}

// TestLocalBranchVariantWithSlashRevisionRecordsResolvedCommit pins the local
// completion record of a branch variant whose revision contains a slash: the
// record is keyed by the resolved commit, so it lives at
// <base>/<revision>/<commit>/hfd.yaml and the revision name never stands in
// for the commit.
func TestLocalBranchVariantWithSlashRevisionRecordsResolvedCommit(t *testing.T) {
	const (
		revision = "exl/4_25"
		commit   = "4444444444444444444444444444444444444444"
	)
	revisions := map[string]branchHubRevision{
		revision: {
			sha: commit,
			files: map[string]string{
				"config.json":       `{"model_type":"exl3","bits":4}`,
				"model.safetensors": "exl-branch-weights",
			},
		},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	root := t.TempDir()
	cfg := Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}

	if err := Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: revision, BranchVariant: true}, cfg, nil); err != nil {
		t.Fatalf("local branch download of a slash revision: %v", err)
	}

	repoBase := filepath.Join(root, branchFixtureRepo)
	space, err := NewBranchVariantLocalSpace(root, branchFixtureRepo)
	if err != nil {
		t.Fatalf("NewBranchVariantLocalSpace: %v", err)
	}
	recordDir, err := space.RecordDir(revision, commit)
	if err != nil {
		t.Fatalf("RecordDir: %v", err)
	}
	if want := branchLocalRecordDir(root, revision, commit); recordDir != want {
		t.Fatalf("completion record dir = %q, want %q", recordDir, want)
	}
	manifest := readBranchFixtureManifest(t, filepath.Join(recordDir, ManifestFilename))
	assertBranchFixtureManifest(t, manifest, revision, commit)

	// The revision name must not stand in for the commit in the record path.
	if _, err := os.Stat(filepath.Join(branchLocalRecordsDir(root), "exl", "4_25", revision, ManifestFilename)); !os.IsNotExist(err) {
		t.Fatalf("completion record was keyed by the revision name (err=%v)", err)
	}

	// The payload sits beside the record, in the app-owned payload tree.
	body, err := os.ReadFile(filepath.Join(branchLocalPayloadDir(root, revision), "model.safetensors"))
	if err != nil || string(body) != "exl-branch-weights" {
		t.Fatalf("slash-revision payload = %q err=%v, want %q", body, err, "exl-branch-weights")
	}
	// The reserved space lives outside the ordinary repository tree.
	if PathInside(repoBase, branchLocalSpaceDir(root)) {
		t.Fatalf("reserved space %q is inside the repository tree %q", branchLocalSpaceDir(root), repoBase)
	}
}

// TestOrdinaryDownloadSucceedsWithoutResolvedCommit pins the preserved
// ordinary path: an ordinary download never requires a resolved commit. The
// revision lookup fails here, so the plan carries the branch name and the
// download reads the branch name, exactly as before.
func TestOrdinaryDownloadSucceedsWithoutResolvedCommit(t *testing.T) {
	const revision = "main"
	hub, readFrom := unresolvedBranchHub(t, revision, map[string]string{
		"config.json": `{"model_type":"main"}`,
	})
	defer hub.Close()
	root := t.TempDir()
	cfg := Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}
	job := Job{Repo: branchFixtureRepo, Revision: revision}

	plan, err := PlanRepo(context.Background(), job, cfg)
	if err != nil {
		t.Fatalf("PlanRepo ordinary: %v", err)
	}
	if plan.CommitResolved || plan.Commit != revision {
		t.Fatalf("ordinary plan commit = %q resolved=%v, want the unresolved branch name %q", plan.Commit, plan.CommitResolved, revision)
	}

	if err := Download(context.Background(), job, cfg, nil); err != nil {
		t.Fatalf("ordinary download without a resolved commit: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(root, branchFixtureRepo, "config.json"))
	if err != nil || string(body) != `{"model_type":"main"}` {
		t.Fatalf("ordinary payload = %q err=%v, want %q", body, err, `{"model_type":"main"}`)
	}
	// The download addressed the branch name, not a commit.
	for _, read := range readFrom() {
		if read != revision {
			t.Fatalf("ordinary download read %q, want the branch name %q", read, revision)
		}
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

	payloadDir := branchLocalPayloadDir(root, revision)
	payload, err := os.ReadFile(filepath.Join(payloadDir, ManifestFilename))
	if err != nil || string(payload) != repositoryManifest {
		t.Fatalf("payload hfd.yaml = %q err=%v, want repository content %q", payload, err, repositoryManifest)
	}
	manifestPath := filepath.Join(branchLocalRecordDir(root, revision, commit), ManifestFilename)
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
	localSpace, err := NewBranchVariantLocalSpace(localBase, branchFixtureRepo)
	if err != nil {
		t.Fatalf("NewBranchVariantLocalSpace: %v", err)
	}
	localGot, err := localSpace.RecordDir(revision, commit)
	if err != nil {
		t.Fatalf("RecordDir: %v", err)
	}
	localWant := branchLocalRecordDir(localBase, revision, commit)
	if localGot != localWant || !PathInside(localBase, localGot) {
		t.Fatalf("local manifest dir = %q, want %q inside base", localGot, localWant)
	}

	for _, bad := range []string{"", ".", "..", "../x", "a/../../b", "a//b", `a\b`, "/abs", "exl/."} {
		if _, err := BranchVariantManifestDir(repoDir, bad, commit); err == nil {
			t.Errorf("unsafe revision %q accepted", bad)
		}
		if _, err := localSpace.RecordDir(bad, commit); err == nil {
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
	// A regular file where the app-owned records directory belongs makes the
	// invalidation helper fail.
	if err := os.MkdirAll(branchLocalSpaceDir(root), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(branchLocalSpaceDir(root), "records"), []byte("blocked"), 0644); err != nil {
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
	if _, statErr := os.Stat(branchLocalPayloadDir(root, revision)); !os.IsNotExist(statErr) {
		t.Fatalf("payload directory was created after invalidation failure: %v", statErr)
	}
}

// TestLocalBranchVariantRefusesNonDirectoryRecordSlot covers the record slot
// of the app-owned tree: a regular file where a record directory belongs makes
// the completion record unwritable, so the download must be refused at the
// invalidation, before any payload byte is written.
func TestLocalBranchVariantRefusesNonDirectoryRecordSlot(t *testing.T) {
	const (
		revision = "4.00bpw"
		commit   = "2222222222222222222222222222222222222222"
	)
	hub := fakeBranchVariantHub(t, branchFixtureRevisions())
	defer hub.Close()
	root := t.TempDir()
	// A regular file occupies the record slot of this revision's commit.
	recordSlot := filepath.Join(branchLocalRecordDir(root, revision, commit))
	if err := os.MkdirAll(filepath.Dir(recordSlot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordSlot, []byte("blocked"), 0644); err != nil {
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
	if err == nil || !strings.Contains(err.Error(), "unsafe branch variant layout") {
		t.Fatalf("non-directory record slot error = %v, want the unsafe-layout refusal", err)
	}
	if done {
		t.Fatal("failed branch variant still reported done")
	}
	if !errorEvent {
		t.Fatal("failed branch variant did not emit an error progress event")
	}
	// The failure happened before the payload was written.
	if _, statErr := os.Stat(branchLocalPayloadDir(root, revision)); !os.IsNotExist(statErr) {
		t.Fatalf("payload directory was created after the unsafe-layout refusal: %v", statErr)
	}
}

// TestOrdinaryDownloadUsesBranchNameWhileBranchVariantUsesCommit pins the
// commit-pinning scope: only a branch variant reads the tree and the file
// bytes at the resolved commit. An ordinary download keeps addressing the
// caller's branch name, so a mirror that resolves the revision to a SHA but
// serves the file bytes only by branch name keeps working.
func TestOrdinaryDownloadUsesBranchNameWhileBranchVariantUsesCommit(t *testing.T) {
	const (
		mainCommit     = "1111111111111111111111111111111111111111"
		branchRevision = "4.00bpw"
		branchCommit   = "2222222222222222222222222222222222222222"
	)
	// A branch name and the commit it resolves to serve different content,
	// so the location a plan reads is observable in the bytes on disk.
	revisions := map[string]branchHubRevision{
		"main": {
			sha:   mainCommit,
			files: map[string]string{"config.json": "main-branch-bytes"},
		},
		mainCommit: {
			sha:   mainCommit,
			files: map[string]string{"config.json": "main-commit-bytes"},
		},
		branchRevision: {
			sha:   branchCommit,
			files: map[string]string{"config.json": "branch-name-bytes"},
		},
		branchCommit: {
			sha:   branchCommit,
			files: map[string]string{"config.json": "branch-commit-bytes"},
		},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	root := t.TempDir()
	cfg := Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}

	ordinaryPlan, err := PlanRepo(context.Background(), Job{Repo: branchFixtureRepo, Revision: "main"}, cfg)
	if err != nil {
		t.Fatalf("PlanRepo ordinary: %v", err)
	}
	if ordinaryPlan.Commit != mainCommit || !ordinaryPlan.CommitResolved {
		t.Fatalf("ordinary plan commit = %q resolved=%v, want %q resolved", ordinaryPlan.Commit, ordinaryPlan.CommitResolved, mainCommit)
	}
	if len(ordinaryPlan.Items) != 1 {
		t.Fatalf("ordinary plan items = %+v, want one file", ordinaryPlan.Items)
	}
	if want := hub.URL + "/" + branchFixtureRepo + "/raw/main/config.json"; ordinaryPlan.Items[0].URL != want {
		t.Errorf("ordinary URL = %q, want the branch URL %q", ordinaryPlan.Items[0].URL, want)
	}

	branchPlan, err := PlanRepo(context.Background(), Job{Repo: branchFixtureRepo, Revision: branchRevision, BranchVariant: true}, cfg)
	if err != nil {
		t.Fatalf("PlanRepo branch variant: %v", err)
	}
	if branchPlan.Commit != branchCommit || !branchPlan.CommitResolved {
		t.Fatalf("branch plan commit = %q resolved=%v, want %q resolved", branchPlan.Commit, branchPlan.CommitResolved, branchCommit)
	}
	if len(branchPlan.Items) != 1 {
		t.Fatalf("branch plan items = %+v, want one file", branchPlan.Items)
	}
	if want := hub.URL + "/" + branchFixtureRepo + "/raw/" + branchCommit + "/config.json"; branchPlan.Items[0].URL != want {
		t.Errorf("branch variant URL = %q, want the commit-pinned URL %q", branchPlan.Items[0].URL, want)
	}

	// The bytes on disk prove the read location, not only the planned URL.
	downloadBranchFixture(t, cfg, "main")
	downloadBranchFixture(t, cfg, branchRevision)

	ordinaryBody, err := os.ReadFile(filepath.Join(root, branchFixtureRepo, "config.json"))
	if err != nil || string(ordinaryBody) != "main-branch-bytes" {
		t.Errorf("ordinary payload = %q err=%v, want the branch-name bytes %q", ordinaryBody, err, "main-branch-bytes")
	}
	branchBody, err := os.ReadFile(filepath.Join(branchLocalPayloadDir(root, branchRevision), "config.json"))
	if err != nil || string(branchBody) != "branch-commit-bytes" {
		t.Errorf("branch payload = %q err=%v, want the commit bytes %q", branchBody, err, "branch-commit-bytes")
	}
}

// TestLocalBranchVariantPayloadDoesNotOverwriteOrdinaryDownload discriminates
// the local payload collision: the branch payload used to live at
// <base>/<revision>/, so a repository whose ordinary tree contains a top-level
// directory named like the branch revision made the branch download write the
// very same file and replace the ordinary bytes. The payload now lives in the
// app-owned payload directory, so the ordinary file survives.
func TestLocalBranchVariantPayloadDoesNotOverwriteOrdinaryDownload(t *testing.T) {
	const (
		revision      = "4.00bpw"
		commit        = "2222222222222222222222222222222222222222"
		ordinaryBytes = `{"model_type":"main","generation":"ordinary"}`
		branchBytes   = `{"model_type":"exl3","bits":4,"generation":"branch"}`
	)
	revisions := map[string]branchHubRevision{
		// The ordinary tree carries a top-level directory named like the
		// branch revision, so its file lands on the path the branch payload
		// used to occupy.
		"main": {
			sha: "1111111111111111111111111111111111111111",
			files: map[string]string{
				revision + "/config.json": ordinaryBytes,
				"model.safetensors":       "ordinary-main-weights",
			},
		},
		revision: {
			sha: commit,
			files: map[string]string{
				"config.json":       branchBytes,
				"model.safetensors": "branch-a-weights",
			},
		},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	root := t.TempDir()
	cfg := Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}

	downloadBranchFixture(t, cfg, "main")
	ordinaryPath := filepath.Join(root, branchFixtureRepo, revision, "config.json")
	body, err := os.ReadFile(ordinaryPath)
	if err != nil || string(body) != ordinaryBytes {
		t.Fatalf("ordinary file = %q err=%v, want %q", body, err, ordinaryBytes)
	}

	// A branch variant re-fetches hashless/non-LFS files, so under the old
	// layout this download replaced the ordinary bytes at exactly this path.
	downloadBranchFixture(t, cfg, revision)
	body, err = os.ReadFile(ordinaryPath)
	if err != nil || string(body) != ordinaryBytes {
		t.Fatalf("ordinary file after branch download = %q err=%v, want %q", body, err, ordinaryBytes)
	}

	// The branch payload carries the branch's own bytes, in the app-owned
	// reserved space.
	branchBody, err := os.ReadFile(filepath.Join(branchLocalPayloadDir(root, revision), "config.json"))
	if err != nil || string(branchBody) != branchBytes {
		t.Fatalf("branch payload = %q err=%v, want %q", branchBody, err, branchBytes)
	}
}

// TestLocalBranchVariantPayloadAndCompletionRecordLocations pins the local
// branch-variant layout: the payload lives in the app-owned payload
// directory, the completion record in the metadata tree beside it, and the
// completion-record lookup never reports a payload directory.
func TestLocalBranchVariantPayloadAndCompletionRecordLocations(t *testing.T) {
	const (
		revision = "4.00bpw"
		commit   = "2222222222222222222222222222222222222222"
	)
	revisions := map[string]branchHubRevision{
		revision: {
			sha: commit,
			files: map[string]string{
				"config.json":       `{"model_type":"exl3","bits":4}`,
				"model.safetensors": "branch-a-weights",
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

	repoBase := filepath.Join(root, branchFixtureRepo)
	space, err := NewBranchVariantLocalSpace(root, branchFixtureRepo)
	if err != nil {
		t.Fatalf("NewBranchVariantLocalSpace: %v", err)
	}
	payloadDir, err := space.PayloadDir(revision)
	if err != nil {
		t.Fatalf("PayloadDir: %v", err)
	}
	if want := branchLocalPayloadDir(root, revision); payloadDir != want {
		t.Fatalf("payload dir = %q, want %q", payloadDir, want)
	}
	if _, err := os.Stat(filepath.Join(payloadDir, "model.safetensors")); err != nil {
		t.Fatalf("payload file: %v", err)
	}

	// The completion record stays in the records tree, outside the payload
	// tree, and both trees have different parents.
	recordDir, err := space.RecordDir(revision, commit)
	if err != nil {
		t.Fatalf("RecordDir: %v", err)
	}
	if want := branchLocalRecordDir(root, revision, commit); recordDir != want {
		t.Fatalf("completion record dir = %q, want %q", recordDir, want)
	}
	if PathInside(payloadDir, recordDir) || PathInside(recordDir, payloadDir) {
		t.Fatalf("payload dir %q and completion record dir %q overlap", payloadDir, recordDir)
	}
	manifest := readBranchFixtureManifest(t, filepath.Join(recordDir, ManifestFilename))
	assertBranchFixtureManifest(t, manifest, revision, commit)
	if want := filepath.ToSlash(filepath.Join(BranchVariantLocalSpaceName, "owner", "example-EXL3-model", "payload", revision)); manifest.RepoPath != want {
		t.Errorf("completion record repo path = %q, want %q", manifest.RepoPath, want)
	}

	// The completion-record lookup reports record directories only.
	dirs, err := space.RecordDirs(revision)
	if err != nil {
		t.Fatalf("RecordDirs: %v", err)
	}
	if len(dirs) != 1 || dirs[0] != recordDir {
		t.Fatalf("completion record dirs = %v, want [%q]", dirs, recordDir)
	}
	for _, dir := range dirs {
		if PathInside(payloadDir, dir) {
			t.Errorf("completion record dir %q is inside the payload tree %q", dir, payloadDir)
		}
	}

	// The reserved space lives outside the ordinary repository tree.
	if PathInside(repoBase, space.Path()) {
		t.Fatalf("reserved space %q is inside the repository tree %q", space.Path(), repoBase)
	}

	// The payload path owner rejects a revision that cannot become a safe
	// path component sequence.
	for _, bad := range []string{"", ".", "..", "../x", "a/../../b", "a//b", `a\b`, "/abs"} {
		if _, err := space.PayloadDir(bad); err == nil {
			t.Errorf("unsafe revision %q accepted", bad)
		}
	}
}

// TestLocalBranchVariantInvalidationKeepsRepositoryContent discriminates the
// local invalidation data loss: the invalidation removed the whole revision
// directory, so a repository file that happened to live in the app-owned
// metadata tree was deleted by a branch-variant download of the same
// repository. The invalidation must remove the app's own completion records
// and nothing else.
//
// The repository file keeps the app's former directory name on purpose: it is
// the shape of content the invalidation used to prune.
func TestLocalBranchVariantInvalidationKeepsRepositoryContent(t *testing.T) {
	const (
		revision      = "4.00bpw"
		firstCommit   = "2222222222222222222222222222222222222222"
		secondCommit  = "3333333333333333333333333333333333333333"
		ordinaryFile  = ".hfdesk-variants/4.00bpw/config.json"
		ordinaryBytes = `{"model_type":"main"}`
	)
	revisions := map[string]branchHubRevision{
		// The ordinary tree carries a file inside a directory named like the
		// app's former completion-record tree, so the ordinary download writes
		// it inside the repository directory.
		"main": {
			sha: "1111111111111111111111111111111111111111",
			files: map[string]string{
				ordinaryFile:        ordinaryBytes,
				"model.safetensors": "ordinary-main-weights",
			},
		},
		revision: {
			sha: firstCommit,
			files: map[string]string{
				"config.json":       `{"model_type":"exl3","bits":4}`,
				"model.safetensors": "branch-a-weights",
			},
		},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	root := t.TempDir()
	cfg := Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}
	repoBase := filepath.Join(root, branchFixtureRepo)
	ordinaryPath := filepath.Join(repoBase, filepath.FromSlash(ordinaryFile))

	// The ordinary download writes the repository's own tree into the
	// repository directory, including the file inside the app's former
	// completion-record directory name.
	downloadBranchFixture(t, cfg, "main")
	body, err := os.ReadFile(ordinaryPath)
	if err != nil || string(body) != ordinaryBytes {
		t.Fatalf("ordinary file = %q err=%v, want %q", body, err, ordinaryBytes)
	}

	// The first branch-variant download publishes a completion record in the
	// app-owned reserved space.
	downloadBranchFixture(t, cfg, revision)
	staleRecord := filepath.Join(branchLocalRecordDir(root, revision, firstCommit), ManifestFilename)
	if _, err := os.Stat(staleRecord); err != nil {
		t.Fatalf("first completion record: %v", err)
	}

	// The branch moves to another commit, so the next download invalidates the
	// first record and publishes a new one.
	revisions[revision] = branchHubRevision{
		sha: secondCommit,
		files: map[string]string{
			"config.json":       `{"model_type":"exl3","bits":4}`,
			"model.safetensors": "branch-b-weights",
		},
	}
	downloadBranchFixture(t, cfg, revision)

	// The repository's own file survives the invalidation.
	body, err = os.ReadFile(ordinaryPath)
	if err != nil || string(body) != ordinaryBytes {
		t.Fatalf("ordinary file after the branch download = %q err=%v, want %q", body, err, ordinaryBytes)
	}
	// The stale completion record is gone, and the new one is published.
	if _, err := os.Stat(staleRecord); !os.IsNotExist(err) {
		t.Fatalf("stale completion record survived the invalidation (err=%v)", err)
	}
	manifest := readBranchFixtureManifest(t, filepath.Join(branchLocalRecordDir(root, revision, secondCommit), ManifestFilename))
	if !manifest.BranchVariant || manifest.Branch != revision || manifest.Commit != secondCommit {
		t.Fatalf("completion record = %+v, want the second commit", manifest)
	}
}

// TestLocalBranchVariantInvalidationKeepsForeignCompletionRecords
// discriminates the third local invalidation defect: a record directory name
// was treated as proof that the hfd.yaml inside it is a completion record, so
// a file that carried the app's record path lost that file to a
// branch-variant download of the same repository. The name only selects a
// candidate: the content decides whether the file is this variant's
// completion record, and a foreign file is left completely untouched.
//
// The reserved space is app-owned and lives outside the ordinary repository
// tree, so the files are planted in the space directly: the contract under
// test is the content check, not how the files got there.
func TestLocalBranchVariantInvalidationKeepsForeignCompletionRecords(t *testing.T) {
	const (
		revision    = "4.00bpw"
		commit      = "2222222222222222222222222222222222222222"
		otherCommit = "3333333333333333333333333333333333333333"
	)
	// The space carries files at the app's own record paths: one that is not
	// a manifest at all, one that is a real completion record of another
	// repository, and one at the record path of this variant's own commit.
	notAManifest := "# foreign hfd.yaml\nnot a completion record\n"
	foreignRecord := &DownloadManifest{
		Version:       "1.0",
		Type:          "model",
		Repo:          "owner/other-model",
		Branch:        revision,
		Commit:        commit,
		BranchVariant: true,
		StartedAt:     time.Now().UTC(),
		CompletedAt:   time.Now().UTC(),
		TotalFiles:    1,
		Files:         []ManifestFile{{Name: "config.json", Size: 12}},
	}
	// The foreign record is a complete record, but of another repository: only
	// the identity differs, so the content check has to reject it.
	if BranchVariantRecordMatches(foreignRecord, branchFixtureRepo, revision, commit) {
		t.Fatal("a record of another repository matches this variant's record contract")
	}
	foreignRecordPath, err := foreignRecord.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	foreignRecordBytes, err := os.ReadFile(foreignRecordPath)
	if err != nil {
		t.Fatal(err)
	}
	otherRepoManifest := string(foreignRecordBytes)
	foreignFiles := map[string]string{
		filepath.Join("records", revision, otherCommit, ManifestFilename): notAManifest,
		filepath.Join("records", revision, "docs", ManifestFilename):      otherRepoManifest,
		filepath.Join("records", revision, commit, ManifestFilename):      notAManifest,
	}
	foreignAtOwnRecordPath := filepath.Join("records", revision, commit, ManifestFilename)

	revisions := map[string]branchHubRevision{
		"main": {
			sha: "1111111111111111111111111111111111111111",
			files: map[string]string{
				"config.json":       `{"model_type":"main"}`,
				"model.safetensors": "ordinary-main-weights",
			},
		},
		revision: {
			sha: commit,
			files: map[string]string{
				"config.json":       `{"model_type":"exl3","bits":4}`,
				"model.safetensors": "branch-a-weights",
			},
		},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	root := t.TempDir()
	cfg := Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}
	spaceDir := branchLocalSpaceDir(root)

	// The foreign files sit at the app's own record paths inside the space.
	for path, body := range foreignFiles {
		full := filepath.Join(spaceDir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// The branch-variant download invalidates the revision's prior completion
	// records before it writes any payload byte.
	downloadBranchFixture(t, cfg, revision)

	// The foreign files at the app's record paths survive the invalidation
	// with their original content.
	for path, body := range foreignFiles {
		if path == foreignAtOwnRecordPath {
			// This one is the record path of the variant's own commit, so the
			// variant's own record replaces it below.
			continue
		}
		got, err := os.ReadFile(filepath.Join(spaceDir, path))
		if err != nil {
			t.Fatalf("foreign file %s was removed by the invalidation: %v", path, err)
		}
		if string(got) != body {
			t.Fatalf("foreign file %s = %q, want %q", path, got, body)
		}
	}

	// The variant's own record is present and correct at its record path.
	manifest := readBranchFixtureManifest(t, filepath.Join(spaceDir, foreignAtOwnRecordPath))
	assertBranchFixtureManifest(t, manifest, revision, commit)
}

// TestLocalBranchVariantInvalidationKeepsForeignRecordAndRepositoryFile is
// the end-to-end form of the invalidation's content rule: an ordinary
// download of the same repository and a local branch-variant download of one
// revision must leave every file that is not the app's own completion record
// untouched.
//
// Two foreign files are in play. The repository's own tree carries a file at
// the app's former record path, so the ordinary download writes it into the
// ordinary repository directory. The app-owned records tree carries a file at
// the record path of a commit that is not this variant's own, holding a
// complete manifest of another repository: only its identity separates it
// from a record of this variant.
//
// A removal that trusted the record directory's name would delete that second
// file. The name only selects a candidate: the content decides, through the
// shared record contract.
func TestLocalBranchVariantInvalidationKeepsForeignRecordAndRepositoryFile(t *testing.T) {
	const (
		revision     = "4.00bpw"
		commit       = "2222222222222222222222222222222222222222"
		otherCommit  = "3333333333333333333333333333333333333333"
		ordinaryFile = ".hfdesk-variants/4.00bpw/" + otherCommit + "/" + ManifestFilename
		ordinaryBody = "# the repository's own hfd.yaml\nnot a completion record\n"
	)
	revisions := map[string]branchHubRevision{
		"main": {
			sha: "1111111111111111111111111111111111111111",
			files: map[string]string{
				ordinaryFile:        ordinaryBody,
				"model.safetensors": "ordinary-main-weights",
			},
		},
		revision: {
			sha: commit,
			files: map[string]string{
				"config.json":       `{"model_type":"exl3","bits":4}`,
				"model.safetensors": "branch-a-weights",
			},
		},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()
	root := t.TempDir()
	cfg := Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}

	// The ordinary download writes the repository's own tree, including the
	// file at the app's former record path.
	downloadBranchFixture(t, cfg, "main")
	ordinaryPath := filepath.Join(root, branchFixtureRepo, filepath.FromSlash(ordinaryFile))
	body, err := os.ReadFile(ordinaryPath)
	if err != nil || string(body) != ordinaryBody {
		t.Fatalf("ordinary file = %q err=%v, want %q", body, err, ordinaryBody)
	}

	// The app-owned records tree carries a file at the record path of another
	// commit: a complete manifest, but of another repository. The reserved
	// space is app-owned and lives outside the ordinary repository tree, so
	// the file is planted in the space directly.
	foreign := &DownloadManifest{
		Version:       "1.0",
		Type:          "model",
		Repo:          "owner/other-model",
		Branch:        revision,
		Commit:        otherCommit,
		BranchVariant: true,
		StartedAt:     time.Now().UTC(),
		CompletedAt:   time.Now().UTC(),
		TotalFiles:    1,
		Files:         []ManifestFile{{Name: "config.json", Size: 12}},
	}
	foreignPath, err := foreign.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	foreignBytes, err := os.ReadFile(foreignPath)
	if err != nil {
		t.Fatal(err)
	}
	foreignRecordPath := filepath.Join(branchLocalRecordDir(root, revision, otherCommit), ManifestFilename)
	if err := os.MkdirAll(filepath.Dir(foreignRecordPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreignRecordPath, foreignBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// The branch-variant download invalidates the revision's prior completion
	// records before it writes any payload byte.
	downloadBranchFixture(t, cfg, revision)

	// The foreign file at the app's own record path survives the invalidation
	// with its original bytes.
	got, err := os.ReadFile(foreignRecordPath)
	if err != nil {
		t.Fatalf("foreign record file was removed by the invalidation: %v", err)
	}
	if string(got) != string(foreignBytes) {
		t.Fatalf("foreign record file = %q, want %q", got, foreignBytes)
	}
	// The repository's own file at the app's former record path survives the
	// branch-variant download too.
	body, err = os.ReadFile(ordinaryPath)
	if err != nil || string(body) != ordinaryBody {
		t.Fatalf("ordinary file after the branch download = %q err=%v, want %q", body, err, ordinaryBody)
	}
	// The variant's own record is present and correct.
	manifest := readBranchFixtureManifest(t, filepath.Join(branchLocalRecordDir(root, revision, commit), ManifestFilename))
	assertBranchFixtureManifest(t, manifest, revision, commit)
}

// TestInvalidateBranchVariantLocalManifestsRemovesOnlyItsOwnRecords pins the
// decision table of the invalidation's content check. The record directory
// name only selects a candidate, and the content decides whether the file is
// this variant's completion record.
func TestInvalidateBranchVariantLocalManifestsRemovesOnlyItsOwnRecords(t *testing.T) {
	const (
		revision = "4.00bpw"
		commit   = "2222222222222222222222222222222222222222"
		repo     = branchFixtureRepo
	)
	newRecord := func(recordRepo, recordRevision, recordCommit string) *DownloadManifest {
		return &DownloadManifest{
			Version:       "1.0",
			Type:          "model",
			Repo:          recordRepo,
			Branch:        recordRevision,
			Commit:        recordCommit,
			BranchVariant: true,
			StartedAt:     time.Now().UTC(),
			CompletedAt:   time.Now().UTC(),
			TotalFiles:    1,
			Files:         []ManifestFile{{Name: "config.json", Size: 12}},
		}
	}

	base := t.TempDir()
	space, err := NewBranchVariantLocalSpace(base, repo)
	if err != nil {
		t.Fatal(err)
	}
	revisionDir := filepath.Join(branchLocalRecordsDir(base), revision)
	// Every candidate lives in a commit-shaped directory under the revision
	// directory, so the name rule alone cannot tell them apart.
	ownRecordDir, err := space.RecordDir(revision, commit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newRecord(repo, revision, commit).Write(ownRecordDir); err != nil {
		t.Fatal(err)
	}
	wrongCommitDir, err := space.RecordDir(revision, "3333333333333333333333333333333333333333")
	if err != nil {
		t.Fatal(err)
	}
	// The record names another commit than the directory it sits in.
	if _, err := newRecord(repo, revision, commit).Write(wrongCommitDir); err != nil {
		t.Fatal(err)
	}
	wrongRevisionDir := filepath.Join(revisionDir, "5.00bpw")
	if _, err := newRecord(repo, "5.00bpw", commit).Write(wrongRevisionDir); err != nil {
		t.Fatal(err)
	}
	wrongRepoDir, err := space.RecordDir(revision, "4444444444444444444444444444444444444444")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newRecord("owner/other-model", revision, commit).Write(wrongRepoDir); err != nil {
		t.Fatal(err)
	}
	// A file that is not a manifest at all, under a commit-shaped directory.
	plainDir := filepath.Join(revisionDir, "docs")
	if err := os.MkdirAll(plainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plainBody := []byte("# foreign hfd.yaml\nnot a completion record\n")
	if err := os.WriteFile(filepath.Join(plainDir, ManifestFilename), plainBody, 0o644); err != nil {
		t.Fatal(err)
	}
	// A record path that is a directory rather than a record file.
	dirRecordDir := filepath.Join(revisionDir, "dirrecord")
	if err := os.MkdirAll(filepath.Join(dirRecordDir, ManifestFilename), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := space.InvalidateRecords(revision); err != nil {
		t.Fatalf("InvalidateRecords: %v", err)
	}

	// The variant's own record is gone, and its empty record directory is
	// pruned with it.
	if _, err := os.Stat(filepath.Join(ownRecordDir, ManifestFilename)); !os.IsNotExist(err) {
		t.Fatalf("own completion record survived the invalidation (err=%v)", err)
	}
	if _, err := os.Stat(ownRecordDir); !os.IsNotExist(err) {
		t.Fatalf("emptied record directory survived the invalidation (err=%v)", err)
	}

	// Every other file at a record path is untouched.
	for _, dir := range []string{wrongCommitDir, wrongRevisionDir, wrongRepoDir, plainDir} {
		got, err := os.ReadFile(filepath.Join(dir, ManifestFilename))
		if err != nil {
			t.Fatalf("record file in %s was removed by the invalidation: %v", dir, err)
		}
		if dir == plainDir && string(got) != string(plainBody) {
			t.Fatalf("record file in %s = %q, want %q", dir, got, plainBody)
		}
	}
	// The record path that is a directory is still a directory.
	info, err := os.Stat(filepath.Join(dirRecordDir, ManifestFilename))
	if err != nil || !info.IsDir() {
		t.Fatalf("record directory %s was changed by the invalidation (err=%v)", dirRecordDir, err)
	}
}

// TestLocalBranchVariantRefusesSymlinkedAppOwnedComponent covers the
// intermediate components of the app-owned tree: a symlinked revision
// directory or record directory would redirect the completion records and the
// payload writes outside the reserved space, so the download must fail before
// anything is removed or written.
func TestLocalBranchVariantRefusesSymlinkedAppOwnedComponent(t *testing.T) {
	const (
		revision    = "4.00bpw"
		commit      = "2222222222222222222222222222222222222222"
		multiRev    = "exl/4_25"
		multiCommit = "4444444444444444444444444444444444444444"
	)
	revisions := map[string]branchHubRevision{
		revision: {
			sha: commit,
			files: map[string]string{
				"config.json":       `{"model_type":"exl3","bits":4}`,
				"model.safetensors": "branch-a-weights",
			},
		},
		multiRev: {
			sha: multiCommit,
			files: map[string]string{
				"config.json":       `{"model_type":"exl3","bits":4}`,
				"model.safetensors": "exl-branch-weights",
			},
		},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()

	cases := []struct {
		name     string
		revision string
		// link is the path below the reserved space that is a symlink.
		link []string
	}{
		{name: "revision directory", revision: revision, link: []string{"records", revision}},
		{name: "revision component", revision: multiRev, link: []string{"records", "exl"}},
		{name: "record directory", revision: revision, link: []string{"records", revision, commit}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			external := t.TempDir()
			keep := filepath.Join(external, "keep.txt")
			if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			link := filepath.Join(append([]string{branchLocalSpaceDir(root)}, tc.link...)...)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			// The symlink points outside the reserved space.
			if err := os.Symlink(external, link); err != nil {
				t.Skipf("symlink creation is not permitted on this platform: %v", err)
			}

			var (
				mu     sync.Mutex
				events []ProgressEvent
			)
			err := Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: tc.revision, BranchVariant: true},
				Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}, func(ev ProgressEvent) {
					mu.Lock()
					events = append(events, ev)
					mu.Unlock()
				})
			if err == nil {
				t.Fatal("local branch variant accepted a symlinked app-owned component")
			}
			if !strings.Contains(err.Error(), "unsafe branch variant layout") {
				t.Fatalf("local branch variant error = %v, want the unsafe-layout refusal", err)
			}
			mu.Lock()
			for _, ev := range events {
				if ev.Event == "done" {
					mu.Unlock()
					t.Fatal("local branch variant with a symlinked component reported done")
				}
			}
			mu.Unlock()

			// Nothing outside the reserved space was removed or written.
			entries, readErr := os.ReadDir(external)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 1 || entries[0].Name() != "keep.txt" {
				names := make([]string, 0, len(entries))
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				t.Fatalf("external tree entries = %v, want only keep.txt", names)
			}
			// The payload was not written either.
			if _, statErr := os.Stat(filepath.Join(branchLocalSpaceDir(root), "payload")); !os.IsNotExist(statErr) {
				t.Fatalf("payload directory was created after the unsafe-layout refusal (err=%v)", statErr)
			}
		})
	}
}

// TestLocalBranchVariantRefusesSymlinkedPayloadRevisionDirectory covers the
// payload side of the app-owned tree check: a symlinked revision directory
// inside the payload tree would redirect the payload writes outside the
// reserved space, so the download must refuse before anything is written and
// leave the external target untouched.
func TestLocalBranchVariantRefusesSymlinkedPayloadRevisionDirectory(t *testing.T) {
	const (
		revision = "4.00bpw"
		commit   = "2222222222222222222222222222222222222222"
	)
	revisions := map[string]branchHubRevision{
		revision: {
			sha: commit,
			files: map[string]string{
				"config.json":       `{"model_type":"exl3","bits":4}`,
				"model.safetensors": "branch-a-weights",
			},
		},
	}
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()

	external := t.TempDir()
	keep := filepath.Join(external, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	payloadTree := filepath.Join(branchLocalSpaceDir(root), "payload")
	if err := os.MkdirAll(payloadTree, 0o755); err != nil {
		t.Fatal(err)
	}
	// The revision directory inside the payload tree is a symlink to a
	// directory outside the reserved space.
	if err := os.Symlink(external, filepath.Join(payloadTree, revision)); err != nil {
		t.Skipf("symlink creation is not permitted on this platform: %v", err)
	}

	var (
		mu     sync.Mutex
		events []ProgressEvent
	)
	err := Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: revision, BranchVariant: true},
		Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}, func(ev ProgressEvent) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		})
	if err == nil {
		t.Fatal("local branch variant accepted a symlinked payload revision directory")
	}
	if !strings.Contains(err.Error(), "unsafe branch variant layout") {
		t.Fatalf("local branch variant error = %v, want the unsafe-layout refusal", err)
	}
	mu.Lock()
	for _, ev := range events {
		if ev.Event == "done" {
			mu.Unlock()
			t.Fatal("local branch variant with a symlinked payload directory reported done")
		}
	}
	mu.Unlock()

	// Nothing outside the reserved space was written: the external target
	// keeps exactly the content it had.
	entries, readErr := os.ReadDir(external)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 || entries[0].Name() != "keep.txt" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("external tree entries = %v, want only keep.txt", names)
	}
	body, readErr := os.ReadFile(keep)
	if readErr != nil || string(body) != "keep" {
		t.Fatalf("external file = %q err=%v, want %q", body, readErr, "keep")
	}
	// The payload was not written through the symlink either.
	if _, statErr := os.Stat(filepath.Join(external, "config.json")); !os.IsNotExist(statErr) {
		t.Fatalf("payload was written through the symlink (err=%v)", statErr)
	}
}

// TestBranchVariantFetchesSharedBlobWhenSharedDownloadIsStale covers the
// stale end of the shared-blob wait: the other process was abandoned but its
// .incomplete file is still there, so the blob status is stale rather than
// missing. The branch variant must fetch the file itself and end up with a
// correct snapshot entry and a correct completion record.
func TestBranchVariantFetchesSharedBlobWhenSharedDownloadIsStale(t *testing.T) {
	const (
		revision = "4.00bpw"
		commit   = "2222222222222222222222222222222222222222"
		body     = "branch-a-weights"
	)
	revisions := sharedBlobRevision(revision, commit, body)
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
	sum := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(sum[:])
	// The abandoned download left its .incomplete file behind without an
	// owner record, so the blob status is stale instead of missing.
	if err := os.WriteFile(repoDir.IncompletePath(sha), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, _, err := repoDir.CheckBlob(sha)
	if err != nil {
		t.Fatal(err)
	}
	if status != BlobStale {
		t.Fatalf("abandoned blob status = %v, want %v", status, BlobStale)
	}

	// The download must not wait for a shared download that is already
	// abandoned, so it is bounded by a deadline instead of the shared-blob
	// wait budget.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Download(ctx, Job{Repo: branchFixtureRepo, Revision: revision, BranchVariant: true}, cfg, nil); err != nil {
		t.Fatalf("branch variant after the shared download was abandoned: %v", err)
	}

	// The branch variant fetched the file itself, so its own snapshot holds
	// it and its completion record lists it.
	snapshot := filepath.Join(repoDir.SnapshotsDir(), commit, "model.safetensors")
	got, err := os.ReadFile(snapshot)
	if err != nil || string(got) != body {
		t.Fatalf("branch snapshot = %q err=%v, want %q", got, err, body)
	}
	manifest := readBranchFixtureManifest(t, filepath.Join(repoDir.Path(), BranchVariantMetadataDir, revision, commit, ManifestFilename))
	if !manifest.BranchVariant || manifest.Branch != revision || manifest.Commit != commit {
		t.Fatalf("branch completion record identity = %+v", manifest)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Name != "model.safetensors" {
		t.Fatalf("branch completion record files = %+v", manifest.Files)
	}
}

// --- Shared-blob fixtures: another process is downloading the same blob ---

// sharedBlobRevision builds a revision whose only file is an LFS payload, so
// the plan carries a content SHA and the downloader consults the blob state
// instead of downloading the bytes.
func sharedBlobRevision(revision, commit, body string) map[string]branchHubRevision {
	sum := sha256.Sum256([]byte(body))
	oid := hex.EncodeToString(sum[:])
	return map[string]branchHubRevision{
		revision: {
			sha:   commit,
			files: map[string]string{"model.safetensors": body},
			lfs: map[string]hfLfsInfo{
				"model.safetensors": {Oid: oid, Size: int64(len(body))},
			},
		},
	}
}

// startSharedBlobDownload simulates another process that is currently
// fetching the blob: an .incomplete file with a live owner record.
func startSharedBlobDownload(t *testing.T, repoDir *RepoDir, sha string) {
	t.Helper()
	if err := repoDir.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoDir.IncompletePath(sha), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repoDir.WriteIncompleteMeta(sha, 0); err != nil {
		t.Fatal(err)
	}
	status, _, err := repoDir.CheckBlob(sha)
	if err != nil {
		t.Fatal(err)
	}
	if status != BlobDownloading {
		t.Fatalf("shared blob status = %v, want %v", status, BlobDownloading)
	}
}

// TestBranchVariantWaitsForSharedBlobBeforeReportingSuccess discriminates
// the first branch-variant defect: a blob another process is still fetching
// was treated as an already-complete file, so the job skipped it, published
// its completion record and reported success while the branch's own snapshot
// never held the file. The branch variant must wait for the shared download
// and materialize its own snapshot entry instead.
func TestBranchVariantWaitsForSharedBlobBeforeReportingSuccess(t *testing.T) {
	const (
		revision = "4.00bpw"
		commit   = "2222222222222222222222222222222222222222"
		body     = "branch-a-weights"
	)
	revisions := sharedBlobRevision(revision, commit, body)
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
	sum := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(sum[:])
	startSharedBlobDownload(t, repoDir, sha)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		mu     sync.Mutex
		events []ProgressEvent
	)
	done := make(chan error, 1)
	go func() {
		done <- Download(ctx, Job{Repo: branchFixtureRepo, Revision: revision, BranchVariant: true}, cfg, func(ev ProgressEvent) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		})
	}()

	// The job must not finish while the shared download is still running: a
	// skip would publish a completion record for a file the branch's own
	// snapshot does not contain.
	select {
	case err := <-done:
		t.Fatalf("branch variant finished while the blob was still downloading: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	mu.Lock()
	for _, ev := range events {
		if ev.Event == "file_done" && strings.Contains(ev.Message, "skip") {
			mu.Unlock()
			t.Fatalf("branch variant skipped the shared blob: %q", ev.Message)
		}
	}
	mu.Unlock()
	snapshot := filepath.Join(repoDir.SnapshotsDir(), commit, "model.safetensors")
	if _, err := os.Stat(snapshot); !os.IsNotExist(err) {
		t.Fatalf("branch snapshot entry exists before the shared download finished (err=%v)", err)
	}

	// The shared download publishes the blob.
	if err := os.WriteFile(repoDir.BlobPath(sha), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repoDir.CleanupIncomplete(sha); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("branch variant after the shared download completed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("branch variant did not finish after the shared download completed")
	}

	// The branch's own snapshot entry exists and the completion record lists
	// the file.
	got, err := os.ReadFile(snapshot)
	if err != nil || string(got) != body {
		t.Fatalf("branch snapshot = %q err=%v, want %q", got, err, body)
	}
	manifest := readBranchFixtureManifest(t, filepath.Join(repoDir.Path(), BranchVariantMetadataDir, revision, commit, ManifestFilename))
	if !manifest.BranchVariant || manifest.Branch != revision || manifest.Commit != commit {
		t.Fatalf("branch completion record identity = %+v", manifest)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Name != "model.safetensors" {
		t.Fatalf("branch completion record files = %+v", manifest.Files)
	}
}

// TestBranchVariantSharedBlobWaitFailsWhenCancelled pins the cancellation
// contract of the shared-blob wait: a wait that cannot finish must end as an
// error, never as a success, and it must observe the job context so a
// cancelled job stops promptly.
func TestBranchVariantSharedBlobWaitFailsWhenCancelled(t *testing.T) {
	const (
		revision = "4.00bpw"
		commit   = "2222222222222222222222222222222222222222"
		body     = "branch-a-weights"
	)
	revisions := sharedBlobRevision(revision, commit, body)
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
	sum := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(sum[:])
	startSharedBlobDownload(t, repoDir, sha)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Download(ctx, Job{Repo: branchFixtureRepo, Revision: revision, BranchVariant: true}, cfg, nil)
	}()

	// The job is held by the wait: the only planned file is the shared blob.
	select {
	case err := <-done:
		t.Fatalf("branch variant finished while the blob was still downloading: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled branch variant reported success")
		}
		if !strings.Contains(err.Error(), "wait for shared blob") {
			t.Fatalf("cancelled branch variant error = %v, want the shared-blob wait error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled branch variant did not stop promptly")
	}

	// A failed job publishes no completion record, so the variant is not
	// reported as downloaded.
	manifestPath := filepath.Join(repoDir.Path(), BranchVariantMetadataDir, revision, commit, ManifestFilename)
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("failed branch variant wrote a completion record (err=%v)", err)
	}
}

// TestOrdinaryDownloadKeepsSharedBlobSkip pins the preserved ordinary
// behavior: a blob another process is downloading is skipped with the
// existing reason, and the job still succeeds.
func TestOrdinaryDownloadKeepsSharedBlobSkip(t *testing.T) {
	const (
		revision = "main"
		commit   = "1111111111111111111111111111111111111111"
		body     = "ordinary-main-weights"
	)
	revisions := sharedBlobRevision(revision, commit, body)
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
	sum := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(sum[:])
	startSharedBlobDownload(t, repoDir, sha)

	var (
		mu     sync.Mutex
		events []ProgressEvent
	)
	// The download is bounded by a deadline: if the ordinary path ever
	// regressed to waiting for the shared blob, the job ends with a clear
	// error instead of blocking for the whole shared-blob wait budget.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := Download(ctx, Job{Repo: branchFixtureRepo, Revision: revision}, cfg, func(ev ProgressEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}); err != nil {
		t.Fatalf("ordinary download with a shared blob: %v", err)
	}

	mu.Lock()
	skipped := ""
	for _, ev := range events {
		if ev.Event == "file_done" && strings.Contains(ev.Message, "skip") {
			skipped = ev.Message
		}
	}
	mu.Unlock()
	if !strings.Contains(skipped, "downloading by another process") {
		t.Fatalf("ordinary download skip reason = %q, want the shared-blob skip", skipped)
	}

	// The ordinary skip keeps its lenient shape: the file is recorded in the
	// friendly manifest even though no snapshot entry was materialized.
	manifest := readBranchFixtureManifest(t, filepath.Join(repoDir.FriendlyPath(), ManifestFilename))
	if manifest.BranchVariant {
		t.Fatalf("ordinary manifest gained branch_variant: %+v", manifest)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Name != "model.safetensors" {
		t.Fatalf("ordinary manifest files = %+v", manifest.Files)
	}
	if _, err := os.Stat(filepath.Join(repoDir.SnapshotsDir(), commit, "model.safetensors")); !os.IsNotExist(err) {
		t.Fatalf("ordinary shared-blob skip created a snapshot entry (err=%v)", err)
	}
}

// TestWaitForSharedBlobTimesOut pins the bounded-wait contract: a wait that
// cannot finish ends as an error instead of an endless wait.
func TestWaitForSharedBlobTimesOut(t *testing.T) {
	cache := NewHFCacheResolved(t.TempDir(), "", 0)
	repoDir, err := cache.Repo(branchFixtureRepo, RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	const sha = "3333333333333333333333333333333333333333333333333333333333333333"
	startSharedBlobDownload(t, repoDir, sha)

	status, err := waitForSharedBlob(context.Background(), repoDir, sha, 50*time.Millisecond)
	if err == nil {
		t.Fatalf("wait for an unresolvable shared blob = %v, want an error", status)
	}
	if status != BlobDownloading {
		t.Fatalf("timed-out wait status = %v, want %v", status, BlobDownloading)
	}
}

// TestBranchVariantFailsWhenSnapshotEntryCannotBeCreated discriminates the
// second branch-variant defect: a failed snapshot materialization was
// discarded, so the job reported success although the branch's snapshot never
// held the file.
func TestBranchVariantFailsWhenSnapshotEntryCannotBeCreated(t *testing.T) {
	const (
		revision = "4.00bpw"
		commit   = "2222222222222222222222222222222222222222"
		body     = "branch-a-weights"
	)
	revisions := sharedBlobRevision(revision, commit, body)
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
	sum := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(sum[:])
	// The blob already exists, so the file takes the skip path and the
	// snapshot entry is the only thing that can fail.
	if err := os.WriteFile(repoDir.BlobPath(sha), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// A regular file where the snapshot directory belongs makes the snapshot
	// entry impossible to create.
	if err := os.WriteFile(filepath.Join(repoDir.SnapshotsDir(), commit), []byte("blocked"), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		mu     sync.Mutex
		events []ProgressEvent
	)
	err = Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: revision, BranchVariant: true}, cfg, func(ev ProgressEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	if err == nil {
		t.Fatal("branch variant with an impossible snapshot entry reported success")
	}
	if !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("branch variant error = %v, want the snapshot-entry error", err)
	}
	mu.Lock()
	for _, ev := range events {
		if ev.Event == "done" {
			mu.Unlock()
			t.Fatal("failed branch variant still reported done")
		}
	}
	mu.Unlock()
	// The completion record must not claim a file the branch's snapshot does
	// not hold.
	manifestPath := filepath.Join(repoDir.Path(), BranchVariantMetadataDir, revision, commit, ManifestFilename)
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("failed branch variant wrote a completion record (err=%v)", statErr)
	}
}

// TestBranchVariantFailsWhenSnapshotEntryCannotBeCreatedAfterSharedBlobCompletes
// covers the shared-blob wait path of the same contract: the blob becomes
// complete while the branch variant waits for it, and the branch variant's
// own snapshot entry then cannot be created. The job must fail and must not
// publish a completion record.
func TestBranchVariantFailsWhenSnapshotEntryCannotBeCreatedAfterSharedBlobCompletes(t *testing.T) {
	const (
		revision = "4.00bpw"
		commit   = "2222222222222222222222222222222222222222"
		body     = "branch-a-weights"
	)
	revisions := sharedBlobRevision(revision, commit, body)
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
	sum := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(sum[:])
	// The blob is still being fetched by another process when the branch
	// variant starts.
	startSharedBlobDownload(t, repoDir, sha)
	// A regular file where the snapshot directory belongs makes the snapshot
	// entry impossible to create.
	if err := os.WriteFile(filepath.Join(repoDir.SnapshotsDir(), commit), []byte("blocked"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: revision, BranchVariant: true}, cfg, nil)
	}()

	// The job waits for the shared download: it cannot finish while the blob
	// is still incomplete, so the wait is the path under test.
	select {
	case err := <-done:
		t.Fatalf("branch variant finished while the blob was still downloading: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	// The shared download publishes the blob while the branch variant waits.
	if err := os.WriteFile(repoDir.BlobPath(sha), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repoDir.CleanupIncomplete(sha); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("branch variant with an impossible snapshot entry reported success")
		}
		if !strings.Contains(err.Error(), "snapshot") {
			t.Fatalf("branch variant error = %v, want the snapshot-entry error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("branch variant did not finish after the shared download completed")
	}

	// The failed job publishes no completion record, so the variant is not
	// reported as downloaded.
	manifestPath := filepath.Join(repoDir.Path(), BranchVariantMetadataDir, revision, commit, ManifestFilename)
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("failed branch variant wrote a completion record (err=%v)", statErr)
	}
}

// TestLocalBranchVariantRefusesSymlinkedAppOwnedTree discriminates the
// security defect: os.RemoveAll resolves an intermediate symlink, so a
// symlinked app-owned tree made the cleanup delete a directory outside the
// reserved space, and the payload writes landed outside it too.
func TestLocalBranchVariantRefusesSymlinkedAppOwnedTree(t *testing.T) {
	const (
		revision = "4.00bpw"
		commit   = "2222222222222222222222222222222222222222"
		body     = "branch-a-weights"
	)
	revisions := sharedBlobRevision(revision, commit, body)
	hub := fakeBranchVariantHub(t, revisions)
	defer hub.Close()

	for _, tree := range []string{branchVariantLocalRecordsDirName, branchVariantLocalPayloadDirName} {
		t.Run(tree, func(t *testing.T) {
			external := t.TempDir()
			keepDir := filepath.Join(external, revision)
			if err := os.MkdirAll(keepDir, 0o755); err != nil {
				t.Fatal(err)
			}
			keep := filepath.Join(keepDir, "keep.txt")
			if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			spaceDir := branchLocalSpaceDir(root)
			if err := os.MkdirAll(spaceDir, 0o755); err != nil {
				t.Fatal(err)
			}
			// The app-owned tree is a symlink to a directory outside the
			// reserved space.
			if err := os.Symlink(external, filepath.Join(spaceDir, tree)); err != nil {
				t.Skipf("symlink creation is not permitted on this platform: %v", err)
			}

			var (
				mu     sync.Mutex
				events []ProgressEvent
			)
			err := Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: revision, BranchVariant: true},
				Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}, func(ev ProgressEvent) {
					mu.Lock()
					events = append(events, ev)
					mu.Unlock()
				})
			if err == nil {
				t.Fatal("local branch variant accepted a symlinked app-owned tree")
			}
			mu.Lock()
			for _, ev := range events {
				if ev.Event == "done" {
					mu.Unlock()
					t.Fatal("local branch variant with a symlinked tree reported done")
				}
			}
			mu.Unlock()

			// Nothing outside the reserved space was removed or written.
			if _, statErr := os.Stat(keep); statErr != nil {
				t.Fatalf("external file was removed through the symlink: %v", statErr)
			}
			entries, readErr := os.ReadDir(external)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 1 || entries[0].Name() != revision {
				names := make([]string, 0, len(entries))
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				t.Fatalf("external tree entries = %v, want only %q", names, revision)
			}
			inside, readErr := os.ReadDir(filepath.Join(keepDir))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(inside) != 1 || inside[0].Name() != "keep.txt" {
				names := make([]string, 0, len(inside))
				for _, entry := range inside {
					names = append(names, entry.Name())
				}
				t.Fatalf("external revision entries = %v, want only keep.txt", names)
			}
		})
	}
}

// TestEnsureLocalBranchVariantTreeRefusesEscapingComponent covers the
// containment barrier of the app-owned tree check: a component that is not a
// plain name inside the tree is refused with a clear error, and the check
// never walks out of the base.
//
// The revision that reaches the check is validated before the call, so this
// is the last line of defense for a component that would otherwise leave the
// reserved space. The escape target exists in these cases, so a check that
// followed the component would report the layout as safe.
func TestEnsureLocalBranchVariantTreeRefusesEscapingComponent(t *testing.T) {
	// Each case gets its own base so the cases cannot influence each other.
	newBase := func(t *testing.T) (base, outside string) {
		t.Helper()
		root := t.TempDir()
		base = filepath.Join(root, "repo")
		outside = filepath.Join(root, "outside")
		for _, dir := range []string{filepath.Join(base, branchVariantLocalRecordsDirName), outside} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return base, outside
	}

	t.Run("parent component", func(t *testing.T) {
		base, _ := newBase(t)
		err := ensureLocalBranchVariantTree(base, []string{branchVariantLocalRecordsDirName, "..", "..", "outside"})
		if err == nil {
			t.Fatal("app-owned tree check accepted a component that leaves the base")
		}
		if !strings.Contains(err.Error(), "must be local") && !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("error = %v, want a containment refusal", err)
		}
	})

	t.Run("absolute component", func(t *testing.T) {
		base, outside := newBase(t)
		err := ensureLocalBranchVariantTree(base, []string{branchVariantLocalRecordsDirName, outside})
		if err == nil {
			t.Fatal("app-owned tree check accepted an absolute component")
		}
		if !strings.Contains(err.Error(), "must be relative") && !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("error = %v, want a containment refusal", err)
		}
	})

	// The containment barrier must not weaken the filesystem check: a
	// symlinked component is still refused on the resolved path.
	t.Run("symlinked component", func(t *testing.T) {
		base, outside := newBase(t)
		if err := os.Symlink(outside, filepath.Join(base, branchVariantLocalRecordsDirName, "exl")); err != nil {
			t.Skipf("symlink creation is not permitted on this platform: %v", err)
		}
		err := ensureLocalBranchVariantTree(base, []string{branchVariantLocalRecordsDirName, "exl"})
		if err == nil {
			t.Fatal("app-owned tree check accepted a symlinked component")
		}
		if !strings.Contains(err.Error(), "is a symbolic link") {
			t.Fatalf("error = %v, want the symlink refusal", err)
		}
	})

	// A missing component is not unsafe: nothing below it can exist yet, and
	// the downloader creates the tree.
	t.Run("missing component", func(t *testing.T) {
		base, _ := newBase(t)
		if err := ensureLocalBranchVariantTree(base, []string{branchVariantLocalRecordsDirName, "exl", "4_25"}); err != nil {
			t.Fatalf("error = %v, want a missing component to be accepted", err)
		}
	})
}

// TestBranchVariantFetchesSharedBlobWhenSharedDownloadDisappears covers the
// other end of the shared-blob wait: when the other process goes away without
// publishing the blob, the branch variant must fetch the file itself instead
// of waiting or claiming success.
func TestBranchVariantFetchesSharedBlobWhenSharedDownloadDisappears(t *testing.T) {
	const (
		revision = "4.00bpw"
		commit   = "2222222222222222222222222222222222222222"
		body     = "branch-a-weights"
	)
	revisions := sharedBlobRevision(revision, commit, body)
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
	sum := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(sum[:])
	startSharedBlobDownload(t, repoDir, sha)

	done := make(chan error, 1)
	go func() {
		done <- Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: revision, BranchVariant: true}, cfg, nil)
	}()

	// The job waits for the shared download, which then disappears.
	select {
	case err := <-done:
		t.Fatalf("branch variant finished while the blob was still downloading: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := repoDir.CleanupIncomplete(sha); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("branch variant after the shared download disappeared: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("branch variant did not finish after the shared download disappeared")
	}

	// The branch variant fetched the file itself, so its own snapshot holds it.
	snapshot := filepath.Join(repoDir.SnapshotsDir(), commit, "model.safetensors")
	got, err := os.ReadFile(snapshot)
	if err != nil || string(got) != body {
		t.Fatalf("branch snapshot = %q err=%v, want %q", got, err, body)
	}
	manifest := readBranchFixtureManifest(t, filepath.Join(repoDir.Path(), BranchVariantMetadataDir, revision, commit, ManifestFilename))
	if len(manifest.Files) != 1 || manifest.Files[0].Name != "model.safetensors" {
		t.Fatalf("branch completion record files = %+v", manifest.Files)
	}
}

// TestOrdinaryDownloadKeepsLenientSnapshotFailure pins the preserved
// asymmetry: an ordinary download still treats a failed snapshot entry as a
// skip, so only a branch variant fails the job.
func TestOrdinaryDownloadKeepsLenientSnapshotFailure(t *testing.T) {
	const (
		revision = "main"
		commit   = "1111111111111111111111111111111111111111"
		body     = "ordinary-main-weights"
	)
	revisions := sharedBlobRevision(revision, commit, body)
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
	sum := sha256.Sum256([]byte(body))
	sha := hex.EncodeToString(sum[:])
	// The blob already exists, so the file takes the skip path.
	if err := os.WriteFile(repoDir.BlobPath(sha), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// A regular file where the snapshot directory belongs makes the snapshot
	// entry impossible to create.
	if err := os.WriteFile(filepath.Join(repoDir.SnapshotsDir(), commit), []byte("blocked"), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		mu     sync.Mutex
		events []ProgressEvent
	)
	if err := Download(context.Background(), Job{Repo: branchFixtureRepo, Revision: revision}, cfg, func(ev ProgressEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}); err != nil {
		t.Fatalf("ordinary download with an impossible snapshot entry: %v", err)
	}
	mu.Lock()
	skipped := ""
	for _, ev := range events {
		if ev.Event == "file_done" && strings.Contains(ev.Message, "skip") {
			skipped = ev.Message
		}
	}
	mu.Unlock()
	if !strings.Contains(skipped, "blob exists") {
		t.Fatalf("ordinary download skip reason = %q, want the blob-exists skip", skipped)
	}
	manifest := readBranchFixtureManifest(t, filepath.Join(repoDir.FriendlyPath(), ManifestFilename))
	if len(manifest.Files) != 1 || manifest.Files[0].Name != "model.safetensors" {
		t.Fatalf("ordinary manifest files = %+v", manifest.Files)
	}
}
