package hfdownloader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type branchVariantTestRevision struct {
	commit string
	files  map[string][]byte
}

type observedWaitContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func branchVariantTestRevisions() map[string]branchVariantTestRevision {
	common := []byte("shared-content-hash")
	return map[string]branchVariantTestRevision{
		"4.00bpw": {
			commit: "commit-a",
			files: map[string][]byte{
				"config.json":         []byte(`{"model":"A"}`),
				"weights.safetensors": []byte("AAAAAAAA"),
				"support-a.json":      []byte(`{"variant":"A"}`),
				"common.bin":          common,
			},
		},
		"4_25": {
			commit: "commit-b",
			files: map[string][]byte{
				"config.json":         []byte(`{"model":"B"}`),
				"weights.safetensors": []byte("BBBBBBBB"),
				"support-b.json":      []byte(`{"variant":"B"}`),
				"common.bin":          common,
			},
		},
	}
}

func newBranchVariantTestHub(t *testing.T, revisions map[string]branchVariantTestRevision) *httptest.Server {
	return newBranchVariantTestHubForRepo(t, "owner/model", revisions, nil)
}

func newBranchVariantTestHubWithHook(t *testing.T, revisions map[string]branchVariantTestRevision, beforeFile func(commit, name string)) *httptest.Server {
	return newBranchVariantTestHubForRepo(t, "owner/model", revisions, beforeFile)
}

func newBranchVariantTestHubForRepo(t *testing.T, repoID string, revisions map[string]branchVariantTestRevision, beforeFile func(commit, name string)) *httptest.Server {
	t.Helper()
	lookup := func(revision string) (branchVariantTestRevision, bool) {
		if info, ok := revisions[revision]; ok {
			return info, true
		}
		for _, info := range revisions {
			if info.commit == revision {
				return info, true
			}
		}
		return branchVariantTestRevision{}, false
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repoPrefix := "/api/models/" + repoID + "/"
		if strings.HasPrefix(r.URL.Path, repoPrefix+"revision/") {
			revision := strings.TrimPrefix(r.URL.Path, repoPrefix+"revision/")
			info, ok := lookup(revision)
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: info.commit})
			return
		}
		if strings.HasPrefix(r.URL.Path, repoPrefix+"tree/") {
			revision := strings.TrimPrefix(r.URL.Path, repoPrefix+"tree/")
			info, ok := lookup(revision)
			if !ok {
				http.NotFound(w, r)
				return
			}
			names := make([]string, 0, len(info.files))
			for name := range info.files {
				names = append(names, name)
			}
			sort.Strings(names)
			nodes := make([]hfNode, 0, len(info.files))
			for _, name := range names {
				body := info.files[name]
				node := hfNode{Type: "file", Path: name, Size: int64(len(body))}
				if name == "common.bin" {
					digest := sha256.Sum256(body)
					hash := hex.EncodeToString(digest[:])
					node.LFS = &hfLfsInfo{Oid: hash, Size: int64(len(body))}
				}
				nodes = append(nodes, node)
			}
			_ = json.NewEncoder(w).Encode(nodes)
			return
		}
		for _, prefix := range []string{"/" + repoID + "/raw/", "/" + repoID + "/resolve/"} {
			if !strings.HasPrefix(r.URL.Path, prefix) {
				continue
			}
			rest := strings.TrimPrefix(r.URL.Path, prefix)
			revision, name, ok := strings.Cut(rest, "/")
			if !ok {
				http.NotFound(w, r)
				return
			}
			info, found := lookup(revision)
			body, hasFile := info.files[name]
			if !found || !hasFile {
				http.NotFound(w, r)
				return
			}
			if beforeFile != nil {
				beforeFile(info.commit, name)
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(body)
			}
			return
		}
		http.NotFound(w, r)
	}))
}

// Unmarked revision requests retain the legacy path even when two remote refs
// happen to resemble EXL choices. The analyzer's explicit BranchVariant marker
// is required to opt the EXL selection flow into isolated storage.
func TestOrdinaryRevisionDownloadsRemainInLegacyLocalRoot(t *testing.T) {
	revisions := branchVariantTestRevisions()
	srv := newBranchVariantTestHub(t, revisions)
	defer srv.Close()

	root := t.TempDir()
	settings := Settings{OutputDir: root, Endpoint: srv.URL, Concurrency: 1, Verify: "size"}
	for _, revision := range []string{"4.00bpw", "4_25"} {
		if err := Download(context.Background(), Job{Repo: "owner/model", Revision: revision}, settings, nil); err != nil {
			t.Fatalf("download %s: %v", revision, err)
		}
	}

	shared := filepath.Join(root, "owner", "model")
	for name, want := range map[string][]byte{
		"config.json":         revisions["4.00bpw"].files["config.json"],
		"weights.safetensors": revisions["4.00bpw"].files["weights.safetensors"],
		"support-a.json":      revisions["4.00bpw"].files["support-a.json"],
		"support-b.json":      revisions["4_25"].files["support-b.json"],
	} {
		got, err := os.ReadFile(filepath.Join(shared, name))
		if err != nil || string(got) != string(want) {
			t.Fatalf("reproduced shared-path state %s = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestBranchVariantsMaterializeIndependentLocalArtifacts(t *testing.T) {
	revisions := branchVariantTestRevisions()
	srv := newBranchVariantTestHub(t, revisions)
	defer srv.Close()

	root := t.TempDir()
	settings := Settings{OutputDir: root, Endpoint: srv.URL, Concurrency: 1, Verify: "size"}
	for _, revision := range []string{"4.00bpw", "4_25"} {
		if err := Download(context.Background(), Job{Repo: "owner/model", Revision: revision, BranchVariant: true}, settings, nil); err != nil {
			t.Fatalf("download %s: %v", revision, err)
		}
	}

	for revision, info := range revisions {
		variantDir, err := BranchVariantPath(root, RepoTypeModel, "owner/model", revision, info.commit)
		if err != nil {
			t.Fatal(err)
		}
		for name, want := range info.files {
			got, err := os.ReadFile(filepath.Join(variantDir, name))
			if err != nil || string(got) != string(want) {
				t.Fatalf("local %s/%s = %q, %v; want %q", revision, name, got, err, want)
			}
		}
	}
	legacyPath := filepath.Join(root, "owner", "model", "config.json")
	if _, err := os.Lstat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("branch download wrote to legacy common path %s: %v", legacyPath, err)
	}
}

func TestBranchVariantsMaterializeIndependentHFFriendlyArtifacts(t *testing.T) {
	revisions := branchVariantTestRevisions()
	srv := newBranchVariantTestHub(t, revisions)
	defer srv.Close()

	cacheRoot := t.TempDir()
	cache := NewHFCache(cacheRoot, 0)
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	settings := Settings{CacheDir: cacheRoot, Endpoint: srv.URL, Concurrency: 1, Verify: "sha256"}
	for _, revision := range []string{"4.00bpw", "4_25"} {
		if err := Download(context.Background(), Job{Repo: "owner/model", Revision: revision, BranchVariant: true}, settings, nil); err != nil {
			t.Fatalf("download %s: %v", revision, err)
		}
	}

	for revision, info := range revisions {
		friendlyDir, err := repo.BranchVariantPath(revision, info.commit)
		if err != nil {
			t.Fatal(err)
		}
		for name, want := range info.files {
			friendly := filepath.Join(friendlyDir, name)
			got, err := os.ReadFile(friendly)
			if err != nil || string(got) != string(want) {
				t.Fatalf("friendly %s/%s = %q, %v; want %q", revision, name, got, err, want)
			}
			snapshot, err := repo.SnapshotPath(info.commit, name)
			if err != nil {
				t.Fatal(err)
			}
			got, err = os.ReadFile(snapshot)
			if err != nil || string(got) != string(want) {
				t.Fatalf("snapshot %s/%s = %q, %v; want %q", revision, name, got, err, want)
			}
		}
	}
	common := revisions["4.00bpw"].files["common.bin"]
	commonHash := sha256.Sum256(common)
	if got := repo.BlobPath(hex.EncodeToString(commonHash[:])); func() bool { _, err := os.Stat(got); return err == nil }() == false {
		t.Fatalf("shared trusted blob was not retained: %s", got)
	}
	for revision, info := range revisions {
		manifestDir, err := repo.BranchVariantMetadataPath(revision, info.commit)
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := ReadManifest(filepath.Join(manifestDir, ManifestFilename))
		if err != nil || !manifest.BranchVariant || !manifest.CommitResolved || manifest.Branch != revision || manifest.Commit != info.commit || manifest.TotalFiles != len(info.files) {
			t.Fatalf("branch manifest %s = %+v, %v", revision, manifest, err)
		}
	}
}

func TestOrdinaryTreeCanUseFormerVariantRelativePathWithoutReplacingVariant(t *testing.T) {
	revision := "4.00bpw"
	branch := branchVariantTestRevisions()[revision]
	legacyVariantRelative := filepath.ToSlash(filepath.Join(
		"variants",
		BranchVariantRevisionDirName(revision),
		BranchVariantCommitDirName(branch.commit),
		"weights.safetensors",
	))
	main := branchVariantTestRevision{
		commit: "main-commit",
		files:  map[string][]byte{legacyVariantRelative: []byte("BBBBBBBB")},
	}
	revisions := map[string]branchVariantTestRevision{revision: branch, "main": main}

	for _, mode := range []string{"local", "hf-cache"} {
		for _, order := range []string{"branch-first", "ordinary-first"} {
			t.Run(mode+"/"+order, func(t *testing.T) {
				srv := newBranchVariantTestHub(t, revisions)
				defer srv.Close()
				root := t.TempDir()
				settings := Settings{Endpoint: srv.URL, Concurrency: 1, Verify: "sha256"}
				var ordinaryPath string
				if mode == "local" {
					settings.OutputDir = root
					ordinaryPath = filepath.Join(root, "owner", "model", filepath.FromSlash(legacyVariantRelative))
				} else {
					settings.CacheDir = root
					cache := NewHFCache(root, 0)
					repo, err := cache.Repo("owner/model", RepoTypeModel)
					if err != nil {
						t.Fatal(err)
					}
					ordinaryPath = filepath.Join(repo.FriendlyPath(), filepath.FromSlash(legacyVariantRelative))
				}
				downloadBranch := func() error {
					return Download(context.Background(), Job{Repo: "owner/model", Revision: revision, BranchVariant: true}, settings, nil)
				}
				downloadOrdinary := func() error {
					return Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, settings, nil)
				}
				if order == "branch-first" {
					if err := downloadBranch(); err != nil {
						t.Fatal(err)
					}
					if mode == "local" {
						before, _ := BranchVariantPath(root, RepoTypeModel, "owner/model", revision, branch.commit)
						if data, readErr := os.ReadFile(filepath.Join(before, "weights.safetensors")); readErr != nil || string(data) != "AAAAAAAA" {
							t.Fatalf("branch precondition at %q = %q, %v", before, data, readErr)
						}
					}
					if err := downloadOrdinary(); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := downloadOrdinary(); err != nil {
						t.Fatal(err)
					}
					if err := downloadBranch(); err != nil {
						t.Fatal(err)
					}
				}

				variantRoot := ""
				if mode == "local" {
					variantRoot, _ = BranchVariantPath(root, RepoTypeModel, "owner/model", revision, branch.commit)
				} else {
					repo, _ := NewHFCache(root, 0).Repo("owner/model", RepoTypeModel)
					variantRoot, _ = repo.BranchVariantPath(revision, branch.commit)
				}
				if got, err := os.ReadFile(filepath.Join(variantRoot, "weights.safetensors")); err != nil || string(got) != "AAAAAAAA" {
					t.Fatalf("branch artifact %q was changed by ordinary path %q (ordinary=%q): got %q, err=%v; want branch bytes AAAAAAAA", variantRoot, legacyVariantRelative, ordinaryPath, got, err)
				}
				if got, err := os.ReadFile(ordinaryPath); err != nil || string(got) != "BBBBBBBB" {
					t.Fatalf("ordinary path %q = %q, %v; want bytes BBBBBBBB", ordinaryPath, got, err)
				}
			})
		}
	}
}

func TestOwnedBranchServiceTreeRefusesOrdinaryPayloadAndControlWriters(t *testing.T) {
	revision := "4.00bpw"
	branch := branchVariantTestRevisions()[revision]
	branch.files["manifest"] = []byte("model payload named manifest")
	branch.files[ManifestFilename] = []byte("model payload named hfd.yaml")
	root := t.TempDir()
	branchHub := newBranchVariantTestHub(t, map[string]branchVariantTestRevision{revision: branch})
	defer branchHub.Close()
	settings := Settings{OutputDir: root, Endpoint: branchHub.URL, Concurrency: 1, Verify: "sha256"}
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: revision, BranchVariant: true}, settings, nil); err != nil {
		t.Fatal(err)
	}

	serviceRoot, err := BranchVariantServiceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	payloadRoot, err := BranchVariantPath(root, RepoTypeModel, "owner/model", revision, branch.commit)
	if err != nil {
		t.Fatal(err)
	}
	controlRoot, err := BranchVariantMetadataPath(root, RepoTypeModel, "owner/model", revision, branch.commit)
	if err != nil {
		t.Fatal(err)
	}
	controlPath := filepath.Join(controlRoot, ManifestFilename)
	controlBefore, err := os.ReadFile(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{
		"manifest":            []byte("model payload named manifest"),
		ManifestFilename:      []byte("model payload named hfd.yaml"),
		"weights.safetensors": branch.files["weights.safetensors"],
	} {
		if got, err := os.ReadFile(filepath.Join(payloadRoot, name)); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("branch payload %s=%q, %v; want %q", name, got, err, want)
		}
	}

	for _, area := range []string{"payloads", "control"} {
		var target string
		if area == "payloads" {
			target = filepath.Join(payloadRoot, "weights.safetensors")
		} else {
			target = controlPath
		}
		ordinaryRepo := filepath.ToSlash(filepath.Join(BranchVariantServiceDirName, area))
		ordinaryBase, err := DestinationBase(root, ordinaryRepo)
		if err != nil {
			t.Fatalf("ordinary repo %q: %v", ordinaryRepo, err)
		}
		rel, err := filepath.Rel(ordinaryBase, target)
		if err != nil || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
			t.Fatalf("cannot derive valid ordinary collision path: base=%q target=%q rel=%q err=%v", ordinaryBase, target, rel, err)
		}
		ordinaryHub := newBranchVariantTestHubForRepo(t, ordinaryRepo, map[string]branchVariantTestRevision{
			"main": {commit: "ordinary-commit", files: map[string][]byte{filepath.ToSlash(rel): []byte("BBBBBBBB")}},
		}, nil)
		ordinaryErr := Download(context.Background(), Job{Repo: ordinaryRepo, Revision: "main"}, Settings{OutputDir: root, Endpoint: ordinaryHub.URL, Concurrency: 1, Verify: "sha256"}, nil)
		ordinaryHub.Close()
		if ordinaryErr == nil || !strings.Contains(ordinaryErr.Error(), "app-owned EXL branch variant service tree") {
			t.Fatalf("ordinary %s collision error=%v; want explicit service-ownership refusal", area, ordinaryErr)
		}
		if got, err := os.ReadFile(controlPath); err != nil || !bytes.Equal(got, controlBefore) {
			t.Fatalf("ordinary %s conflict changed control evidence: got=%q err=%v", area, got, err)
		}
		if got, err := os.ReadFile(filepath.Join(payloadRoot, "weights.safetensors")); err != nil || !bytes.Equal(got, branch.files["weights.safetensors"]) {
			t.Fatalf("ordinary %s conflict changed protected payload: got=%q err=%v", area, got, err)
		}
	}

	// The same marker spelling under an ordinary model directory is not owned
	// merely because it matches the service-root component name.
	ordinaryPath := ".hfdesk-variant-store/ordinary.bin"
	ordinaryHub := newBranchVariantTestHub(t, map[string]branchVariantTestRevision{
		"main": {commit: "ordinary-marker-commit", files: map[string][]byte{ordinaryPath: []byte("ordinary")}},
	})
	defer ordinaryHub.Close()
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, Settings{OutputDir: root, Endpoint: ordinaryHub.URL, Concurrency: 1}, nil); err != nil {
		t.Fatalf("ordinary same-spelled path outside owned service area was refused: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "owner", "model", filepath.FromSlash(ordinaryPath))); err != nil || string(got) != "ordinary" {
		t.Fatalf("ordinary same-spelled path=%q, %v", got, err)
	}
	if _, err := os.Stat(serviceRoot); err != nil {
		t.Fatalf("branch service root disappeared: %v", err)
	}
}

func TestBranchVariantDoesNotAdoptPreexistingServiceDirectory(t *testing.T) {
	root := t.TempDir()
	serviceRoot := filepath.Join(root, BranchVariantServiceDirName)
	foreign := filepath.Join(serviceRoot, "foreign-marker")
	if err := os.MkdirAll(serviceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("preserve"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A completion-shaped manifest and a copied owner record are not proof that
	// this app claimed the pre-existing directory. The persisted filesystem
	// identity must match the actual service-root directory.
	forgedOwner := branchVariantStoreOwner{Version: branchVariantStoreVersion, StoreID: "forged-store-id", Purpose: "exl-branch-variants", FileID: "not-this-directory"}
	forgedData, err := json.Marshal(forgedOwner)
	if err != nil {
		t.Fatal(err)
	}
	ownerPath := filepath.Join(serviceRoot, branchVariantStoreOwnerFilename)
	if err := os.WriteFile(ownerPath, forgedData, 0o600); err != nil {
		t.Fatal(err)
	}
	controlRoot := filepath.Join(serviceRoot, "control", "models", "owner", "model", BranchVariantRevisionDirName("4.00bpw"), BranchVariantCommitDirName("commit-a"))
	if err := os.MkdirAll(controlRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	forgedArtifact := branchVariantArtifactOwner{Version: branchVariantStoreVersion, StoreID: forgedOwner.StoreID, RepoType: RepoTypeModel, Repo: "owner/model", Revision: "4.00bpw", Commit: "commit-a"}
	artifactBytes, err := json.Marshal(forgedArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(controlRoot, branchVariantArtifactOwnerFilename), artifactBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	forgedManifest := []byte(`{"version":"1.0","type":"model","repo":"owner/model","branch":"4.00bpw","commit":"commit-a","branchVariant":true,"commitResolved":true,"completed_at":"2026-01-01T00:00:00Z","total_files":1,"total_size":8,"files":[{"name":"weights.safetensors","blob":"blobs/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":8}]}`)
	if err := os.WriteFile(filepath.Join(controlRoot, ManifestFilename), forgedManifest, 0o600); err != nil {
		t.Fatal(err)
	}
	revisions := branchVariantTestRevisions()
	srv := newBranchVariantTestHub(t, revisions)
	defer srv.Close()
	err = Download(context.Background(), Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, Settings{OutputDir: root, Endpoint: srv.URL, Concurrency: 1}, nil)
	if err == nil || !strings.Contains(err.Error(), "unowned branch variant service tree") {
		t.Fatalf("pre-existing service path error=%v; want explicit refusal", err)
	}
	if got, readErr := os.ReadFile(foreign); readErr != nil || string(got) != "preserve" {
		t.Fatalf("pre-existing foreign service data changed: got=%q err=%v", got, readErr)
	}
	if got, readErr := os.ReadFile(ownerPath); readErr != nil || !bytes.Equal(got, forgedData) {
		t.Fatalf("forged ownership record changed: got=%q err=%v", got, readErr)
	}
	if got, readErr := os.ReadFile(filepath.Join(controlRoot, ManifestFilename)); readErr != nil || !bytes.Equal(got, forgedManifest) {
		t.Fatalf("forged completion manifest changed: got=%q err=%v", got, readErr)
	}
}

func TestOrdinaryFirstServicePathRemainsUntouchedWhenBranchClaimRefuses(t *testing.T) {
	root := t.TempDir()
	serviceRoot, err := BranchVariantServiceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	branch := branchVariantTestRevisions()["4.00bpw"]
	payloadRoot, err := BranchVariantPath(root, RepoTypeModel, "owner/model", "4.00bpw", branch.commit)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(payloadRoot, "weights.safetensors")
	ordinaryRepo := BranchVariantServiceDirName + "/payloads"
	ordinaryBase, err := DestinationBase(root, ordinaryRepo)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(ordinaryBase, target)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("cannot derive ordinary first-writer path: base=%q target=%q rel=%q err=%v", ordinaryBase, target, rel, err)
	}
	ordinaryHub := newBranchVariantTestHubForRepo(t, ordinaryRepo, map[string]branchVariantTestRevision{
		"main": {commit: "ordinary-first", files: map[string][]byte{filepath.ToSlash(rel): []byte("BBBBBBBB")}},
	}, nil)
	defer ordinaryHub.Close()
	if err := Download(context.Background(), Job{Repo: ordinaryRepo, Revision: "main"}, Settings{OutputDir: root, Endpoint: ordinaryHub.URL, Concurrency: 1}, nil); err != nil {
		t.Fatalf("ordinary path outside an unclaimed service tree should remain valid: %v", err)
	}

	branchHub := newBranchVariantTestHub(t, map[string]branchVariantTestRevision{"4.00bpw": branch})
	defer branchHub.Close()
	err = Download(context.Background(), Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, Settings{OutputDir: root, Endpoint: branchHub.URL, Concurrency: 1}, nil)
	if err == nil || !strings.Contains(err.Error(), "unowned branch variant service tree") {
		t.Fatalf("branch claim after ordinary write error=%v; want refusal before adopting data", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "BBBBBBBB" {
		t.Fatalf("ordinary-first data changed after branch refusal: got=%q err=%v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(serviceRoot, branchVariantStoreOwnerFilename)); !os.IsNotExist(err) {
		t.Fatalf("ordinary-first directory acquired a branch ownership marker: %v", err)
	}
}

func TestOwnedBranchServiceTreeConflictsThroughOverlappingAndSymlinkRoots(t *testing.T) {
	for _, mode := range []string{"overlapping-root", "symlink-alias"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			branchRoot := filepath.Join(base, "selected")
			ordinaryRoot := base
			ordinaryRepo := filepath.ToSlash(filepath.Join("selected", BranchVariantServiceDirName))
			if mode == "symlink-alias" {
				branchRoot = filepath.Join(base, "physical")
				ordinaryRoot = filepath.Join(base, "alias")
				if err := os.Symlink(branchRoot, ordinaryRoot); err != nil {
					t.Skipf("directory symlink is unavailable: %v", err)
				}
				ordinaryRepo = filepath.ToSlash(filepath.Join(BranchVariantServiceDirName, "payloads"))
			}
			revision := "4.00bpw"
			branch := branchVariantTestRevisions()[revision]
			branchHub := newBranchVariantTestHub(t, map[string]branchVariantTestRevision{revision: branch})
			defer branchHub.Close()
			if err := Download(context.Background(), Job{Repo: "owner/model", Revision: revision, BranchVariant: true}, Settings{OutputDir: branchRoot, Endpoint: branchHub.URL, Concurrency: 1}, nil); err != nil {
				t.Fatal(err)
			}
			payloadRoot, err := BranchVariantPath(branchRoot, RepoTypeModel, "owner/model", revision, branch.commit)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(payloadRoot, "weights.safetensors")
			targetForOrdinary := target
			if mode == "symlink-alias" {
				targetForOrdinary = filepath.Join(ordinaryRoot, BranchVariantServiceDirName, "payloads", "models", "owner", "model",
					BranchVariantRevisionDirName(revision), BranchVariantCommitDirName(branch.commit), "files", "weights.safetensors")
			}
			ordinaryBase, err := DestinationBase(ordinaryRoot, ordinaryRepo)
			if err != nil {
				t.Fatalf("ordinary repo %q: %v", ordinaryRepo, err)
			}
			rel, err := filepath.Rel(ordinaryBase, targetForOrdinary)
			if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("cannot derive ordinary alias collision: base=%q target=%q rel=%q err=%v", ordinaryBase, targetForOrdinary, rel, err)
			}
			ordinaryHub := newBranchVariantTestHubForRepo(t, ordinaryRepo, map[string]branchVariantTestRevision{
				"main": {commit: "ordinary-alias", files: map[string][]byte{filepath.ToSlash(rel): []byte("BBBBBBBB")}},
			}, nil)
			defer ordinaryHub.Close()
			err = Download(context.Background(), Job{Repo: ordinaryRepo, Revision: "main"}, Settings{OutputDir: ordinaryRoot, Endpoint: ordinaryHub.URL, Concurrency: 1}, nil)
			if err == nil || !strings.Contains(err.Error(), "app-owned EXL branch variant service tree") {
				t.Fatalf("ordinary %s conflict error=%v; want physical ownership refusal", mode, err)
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != "AAAAAAAA" {
				t.Fatalf("ordinary %s conflict changed branch bytes: got=%q err=%v", mode, got, err)
			}
		})
	}
}

func TestOrdinaryHFHubPathCannotWriteInsideOwnedBranchServiceTree(t *testing.T) {
	root := t.TempDir()
	revision := "4.00bpw"
	branch := branchVariantTestRevisions()[revision]
	branchHub := newBranchVariantTestHub(t, map[string]branchVariantTestRevision{revision: branch})
	defer branchHub.Close()
	branchSettings := Settings{CacheDir: root, Endpoint: branchHub.URL, Concurrency: 1, Verify: "sha256"}
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: revision, BranchVariant: true}, branchSettings, nil); err != nil {
		t.Fatal(err)
	}
	payloadRoot, err := BranchVariantPath(root, RepoTypeModel, "owner/model", revision, branch.commit)
	if err != nil {
		t.Fatal(err)
	}
	controlRoot, err := BranchVariantMetadataPath(root, RepoTypeModel, "owner/model", revision, branch.commit)
	if err != nil {
		t.Fatal(err)
	}
	controlPath := filepath.Join(controlRoot, ManifestFilename)
	controlBefore, err := os.ReadFile(controlPath)
	if err != nil {
		t.Fatal(err)
	}

	ordinary := branchVariantTestRevision{commit: "ordinary-commit", files: map[string][]byte{"weights.safetensors": []byte("BBBBBBBB")}}
	ordinaryHub := newBranchVariantTestHub(t, map[string]branchVariantTestRevision{"main": ordinary})
	defer ordinaryHub.Close()
	ordinaryHubDir := filepath.Join(root, BranchVariantServiceDirName, "ordinary-hub")
	err = Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, Settings{
		CacheDir: root, HubDir: ordinaryHubDir, Endpoint: ordinaryHub.URL, Concurrency: 1,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "app-owned EXL branch variant service tree") {
		t.Fatalf("ordinary HF writer error=%v; want service-ownership refusal", err)
	}
	if got, err := os.ReadFile(filepath.Join(payloadRoot, "weights.safetensors")); err != nil || !bytes.Equal(got, branch.files["weights.safetensors"]) {
		t.Fatalf("ordinary HF conflict changed branch payload: got=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(controlPath); err != nil || !bytes.Equal(got, controlBefore) {
		t.Fatalf("ordinary HF conflict changed branch control: got=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(ordinaryHubDir, "models--owner--model")); !os.IsNotExist(err) {
		t.Fatalf("ordinary HF conflict created repo storage inside service tree: %v", err)
	}
}

func TestOrdinaryWritersCannotReplaceBranchLeafThroughFriendlyDirectoryAlias(t *testing.T) {
	for _, mode := range []string{"download", "sync", "script"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "script" && runtime.GOOS == "windows" {
				t.Skip("generated rebuild wrapper is a POSIX Bash script")
			}
			root := t.TempDir()
			revision := "4.00bpw"
			branch := branchVariantTestRevisions()[revision]
			branchHub := newBranchVariantTestHub(t, map[string]branchVariantTestRevision{revision: branch})
			defer branchHub.Close()
			if err := Download(context.Background(), Job{Repo: "owner/model", Revision: revision, BranchVariant: true}, Settings{
				CacheDir: root, Endpoint: branchHub.URL, Concurrency: 1, Verify: "sha256",
			}, nil); err != nil {
				t.Fatal(err)
			}
			cache := NewHFCache(root, 0)
			artifact, err := ReadBranchVariantArtifact(root, RepoTypeModel, "owner/model", revision, branch.commit)
			if err != nil || artifact == nil {
				t.Fatalf("read branch artifact=%+v err=%v", artifact, err)
			}
			payload := filepath.Join(artifact.PayloadRoot, "weights.safetensors")
			payloadInfo, err := os.Lstat(payload)
			if err != nil || payloadInfo.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("fixture leaf=%v err=%v; want branch payload symlink to external blob", payloadInfo, err)
			}
			controlBefore, err := os.ReadFile(artifact.ManifestPath)
			if err != nil {
				t.Fatal(err)
			}

			ordinary, err := cache.Repo("zzz/model", RepoTypeModel)
			if err != nil {
				t.Fatal(err)
			}
			if err := ordinary.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			if err := ordinary.WriteRef("main", "ordinary-commit"); err != nil {
				t.Fatal(err)
			}
			snapshot, err := ordinary.SnapshotPath("ordinary-commit", "alias/weights.safetensors")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(snapshot), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(snapshot, []byte("BBBBBBBB"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(ordinary.FriendlyPath(), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(artifact.PayloadRoot, filepath.Join(ordinary.FriendlyPath(), "alias")); err != nil {
				t.Skipf("directory symlink unavailable: %v", err)
			}

			var operationErr error
			switch mode {
			case "download":
				ordinaryHub := newBranchVariantTestHubForRepo(t, "zzz/model", map[string]branchVariantTestRevision{
					"main": {commit: "ordinary-commit", files: map[string][]byte{"alias/weights.safetensors": []byte("BBBBBBBB")}},
				}, nil)
				operationErr = Download(context.Background(), Job{Repo: "zzz/model", Revision: "main"}, Settings{
					CacheDir: root, Endpoint: ordinaryHub.URL, Concurrency: 1,
				}, nil)
				ordinaryHub.Close()
			case "sync":
				result, syncErr := cache.Sync(SyncOptions{})
				operationErr = syncErr
				if result != nil && len(result.Errors) > 0 {
					operationErr = errors.Join(append([]error{syncErr}, result.Errors...)...)
				}
			case "script":
				if _, err := cache.WriteRebuildScript(); err != nil {
					t.Fatal(err)
				}
				output, runErr := runGeneratedRebuildScript(t, cache.RebuildScriptPath())
				if runErr != nil {
					operationErr = fmt.Errorf("%w: %s", runErr, output)
				} else if !bytes.Contains(output, []byte("ordinary download path")) {
					t.Fatalf("script did not report the alias conflict: %s", output)
				}
			}
			explicitConflict := operationErr != nil && (strings.Contains(operationErr.Error(), "branch variant service tree") || strings.Contains(operationErr.Error(), "app-owned EXL branch service tree"))
			if !explicitConflict {
				t.Fatalf("%s alias operation error=%v; want explicit protected-entry conflict", mode, operationErr)
			}
			if got, err := os.ReadFile(payload); err != nil || !bytes.Equal(got, branch.files["weights.safetensors"]) {
				t.Fatalf("%s changed branch leaf payload: got=%q err=%v", mode, got, err)
			}
			if got, err := os.ReadFile(artifact.ManifestPath); err != nil || !bytes.Equal(got, controlBefore) {
				t.Fatalf("%s changed completion control: err=%v", mode, err)
			}
		})
	}
}

func TestBranchVariantMutationGuardDistinguishesEntryFromReferent(t *testing.T) {
	root := t.TempDir()
	artifact, err := ClaimBranchVariantArtifact(root, RepoTypeModel, "owner/model", "4.00bpw", "commit-a")
	if err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(artifact.PayloadRoot, "weights.safetensors")
	if err := os.WriteFile(protected, []byte("AAAAAAAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	ordinary := filepath.Join(root, "ordinary", "weights.safetensors")
	if err := os.MkdirAll(filepath.Dir(ordinary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(protected, ordinary); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := RejectOwnedBranchVariantEntryWrites(ordinary); err != nil {
		t.Fatalf("replacing an ordinary leaf symlink was falsely classified as a service effect: %v", err)
	}
	if err := RejectOwnedBranchVariantContentWrites(ordinary); err == nil || !strings.Contains(err.Error(), "branch variant service tree") {
		t.Fatalf("content write through leaf symlink error=%v; want protected referent refusal", err)
	}
}

func TestBranchVariantPartialCleanupKeepsOwnershipAndCompletion(t *testing.T) {
	root := t.TempDir()
	revision := "4.00bpw"
	branch := branchVariantTestRevisions()[revision]
	hub := newBranchVariantTestHub(t, map[string]branchVariantTestRevision{revision: branch})
	defer hub.Close()
	settings := Settings{OutputDir: root, Endpoint: hub.URL, Concurrency: 1, Verify: "sha256"}
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: revision, BranchVariant: true}, settings, nil); err != nil {
		t.Fatal(err)
	}
	artifact, err := ReadBranchVariantArtifact(root, RepoTypeModel, "owner/model", revision, branch.commit)
	if err != nil || artifact == nil {
		t.Fatalf("read artifact=%+v err=%v", artifact, err)
	}
	payload := filepath.Join(artifact.PayloadRoot, "weights.safetensors")
	manifestBefore, err := os.ReadFile(artifact.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	part := payload + ".part"
	if err := os.WriteFile(part, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payload+".parts.json", []byte("resume metadata"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CleanupBranchVariantJobPartFiles(settings, []string{payload}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{part, payload + ".parts.json"} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Errorf("branch partial %q remained after tracked cleanup: %v", name, err)
		}
	}
	if got, err := os.ReadFile(payload); err != nil || !bytes.Equal(got, branch.files["weights.safetensors"]) {
		t.Fatalf("partial cleanup changed completed branch payload: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(artifact.ManifestPath); err != nil || !bytes.Equal(got, manifestBefore) {
		t.Fatalf("partial cleanup changed completion proof: err=%v", err)
	}

	ordinaryPart := filepath.Join(artifact.PayloadRoot, "support-a.json.part")
	if err := os.WriteFile(ordinaryPart, []byte("do not remove"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CleanupJobPartFiles(Settings{OutputDir: root}, []string{filepath.Join(artifact.PayloadRoot, "support-a.json")}); err != nil {
		t.Fatalf("ordinary cleanup should remain best-effort: %v", err)
	}
	if got, err := os.ReadFile(ordinaryPart); err != nil || string(got) != "do not remove" {
		t.Fatalf("ordinary cleanup crossed into branch-owned payload: %q err=%v", got, err)
	}
}

func TestWaitForDownloadingSharedBlobDoesNotReturnBeforeCompletion(t *testing.T) {
	cache := NewHFCache(filepath.Join(t.TempDir(), "cache"), 0)
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	body := []byte("shared-content-hash")
	digest := sha256.Sum256(body)
	hash := hex.EncodeToString(digest[:])
	if err := os.WriteFile(repo.IncompletePath(hash), body[:4], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.WriteIncompleteMeta(hash, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if status, _, err := repo.CheckBlob(hash); err != nil || status != BlobDownloading {
		t.Fatalf("precondition CheckBlob=(%s,%v); want downloading", status, err)
	}

	ctx := &observedWaitContext{Context: context.Background(), observed: make(chan struct{})}
	result := make(chan struct {
		status BlobStatus
		err    error
	}, 1)
	go func() {
		status, err := repo.WaitForBlob(ctx, hash)
		result <- struct {
			status BlobStatus
			err    error
		}{status, err}
	}()
	select {
	case <-ctx.observed:
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not observe the in-progress shared blob")
	}
	if err := os.WriteFile(repo.BlobPath(hash), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.CleanupIncomplete(hash); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.status != BlobComplete {
			t.Fatalf("WaitForBlob=(%s,%v), want complete", got.status, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not observe completed shared blob")
	}
}

func TestBranchVariantStorageKeysConfineSlashRevisionsAndSeparateCommits(t *testing.T) {
	base := filepath.Join(t.TempDir(), "owner", "model")
	first, err := BranchVariantPath(base, RepoTypeModel, "owner/model", "SC_6.00bpw_H6/V6", "commit-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := BranchVariantPath(base, RepoTypeModel, "owner/model", "SC_6.00bpw_H6/V6", "commit-b")
	if err != nil {
		t.Fatal(err)
	}
	if !PathInside(base, first) || !PathInside(base, second) || first == second {
		t.Fatalf("variant paths are not confined/distinct: %q %q", first, second)
	}
	if strings.Contains(filepath.Base(first), "/") || strings.Contains(filepath.Base(first), `\`) {
		t.Fatalf("revision leaked into portable path component: %q", first)
	}
}

func TestConcurrentBranchVariantsMaterializeSharedTrustedBlobPerCommit(t *testing.T) {
	revisions := branchVariantTestRevisions()
	bothStarted := make(chan struct{})
	release := make(chan struct{})
	var started int
	var startedMu sync.Mutex
	srv := newBranchVariantTestHubWithHook(t, revisions, func(commit, name string) {
		if name != "common.bin" {
			return
		}
		startedMu.Lock()
		started++
		if started == 2 {
			close(bothStarted)
		}
		startedMu.Unlock()
		<-release
	})
	defer srv.Close()

	cacheRoot := t.TempDir()
	cache := NewHFCache(cacheRoot, 0)
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	var partialMu sync.Mutex
	partialPaths := make(map[string]struct{})
	settings := Settings{
		CacheDir: cacheRoot, Endpoint: srv.URL, Concurrency: 1, Verify: "sha256",
		OnPartialFile: func(dst string, finalize bool) {
			common := revisions["4.00bpw"].files["common.bin"]
			hash := sha256.Sum256(common)
			if !finalize && strings.HasPrefix(filepath.Base(dst), "tmp-"+hex.EncodeToString(hash[:])) {
				partialMu.Lock()
				partialPaths[dst] = struct{}{}
				partialMu.Unlock()
			}
		},
	}
	results := make(chan error, 2)
	for _, revision := range []string{"4.00bpw", "4_25"} {
		revision := revision
		go func() {
			results <- Download(context.Background(), Job{Repo: "owner/model", Revision: revision, BranchVariant: true}, settings, nil)
		}()
	}
	select {
	case <-bothStarted:
		close(release)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("both variants did not reach the shared trusted blob")
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent branch download: %v", err)
		}
	}
	partialMu.Lock()
	if len(partialPaths) != 2 {
		partialMu.Unlock()
		t.Fatalf("shared-blob partial paths = %v, want one isolated path per variant", partialPaths)
	}
	partialMu.Unlock()
	for revision, info := range revisions {
		variantDir, err := repo.BranchVariantPath(revision, info.commit)
		if err != nil {
			t.Fatal(err)
		}
		for name, want := range info.files {
			got, err := os.ReadFile(filepath.Join(variantDir, name))
			if err != nil || string(got) != string(want) {
				t.Fatalf("concurrent friendly %s/%s = %q, %v; want %q", revision, name, got, err, want)
			}
			snapshot, err := repo.SnapshotPath(info.commit, name)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(snapshot); err != nil || string(got) != string(want) {
				t.Fatalf("concurrent snapshot %s/%s = %q, %v; want %q", revision, name, got, err, want)
			}
		}
	}
}

// TestBranchVariantWaitsForAContendedSharedBlob pins the branch side of the
// shared-blob boundary.
//
// A blob another transfer is writing is contended state. An ordinary
// download reports it as skipped, but a branch variant must not claim the
// other transfer's snapshot by skipping the file: it waits for that transfer,
// reuses the shared blob without fetching it again, and then materializes its
// own snapshot entry and its own variant payload.
func TestBranchVariantWaitsForAContendedSharedBlob(t *testing.T) {
	shared := []byte("shared-content-hash")
	digest := sha256.Sum256(shared)
	hash := hex.EncodeToString(digest[:])
	files := map[string][]byte{
		"a-config.json":  []byte(`{"model":"A"}`),
		"b-weights.bin":  shared,
		"z-support.json": []byte(`{"variant":"A"}`),
	}
	nodes := make([]hfNode, 0, len(files))
	for _, name := range []string{"a-config.json", "b-weights.bin", "z-support.json"} {
		body := files[name]
		node := hfNode{Type: "file", Path: name, Size: int64(len(body))}
		if name == "b-weights.bin" {
			node.LFS = &hfLfsInfo{Oid: hash, Size: int64(len(body))}
		}
		nodes = append(nodes, node)
	}

	var requestsMu sync.Mutex
	requests := map[string]int{}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/4.00bpw"):
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "commit-a"})
			return
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode(nodes)
			return
		}
		name := ""
		for candidate := range files {
			if strings.HasSuffix(r.URL.Path, "/"+candidate) {
				name = candidate
				break
			}
		}
		if name == "" {
			http.NotFound(w, r)
			return
		}
		requestsMu.Lock()
		requests[name]++
		requestsMu.Unlock()
		body := files[name]
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	}))
	defer hub.Close()

	cacheRoot := t.TempDir()
	cache := NewHFCache(cacheRoot, 0)
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// Another live transfer owns the blob: the incomplete file and its
	// metadata name this process, so the blob reads as downloading.
	if err := os.WriteFile(repo.IncompletePath(hash), shared[:4], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.WriteIncompleteMeta(hash, int64(len(shared))); err != nil {
		t.Fatal(err)
	}
	if status, _, err := repo.CheckBlob(hash); err != nil || status != BlobDownloading {
		t.Fatalf("precondition CheckBlob=(%s,%v); want downloading", status, err)
	}

	done := make(chan error, 1)
	go func() {
		done <- Download(context.Background(), Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true},
			Settings{CacheDir: cacheRoot, Endpoint: hub.URL, Concurrency: 1, Verify: "sha256"}, nil)
	}()

	// The transfer claims the branch variant storage before it moves any
	// file, so the service root is the signal that the transfer started.
	serviceRoot, err := BranchVariantServiceRoot(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	waitForPath(t, serviceRoot)
	// The other transfer still owns the blob, so the branch variant waits
	// for it instead of skipping the file and claiming the other snapshot.
	select {
	case err := <-done:
		t.Fatalf("the branch variant finished while the other transfer still owned the blob: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	// The other transfer finishes and the shared blob becomes complete.
	if err := os.WriteFile(repo.BlobPath(hash), shared, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.CleanupIncomplete(hash); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the branch variant download failed: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the branch variant did not finish after the shared blob completed")
	}

	// The shared blob was reused, not fetched again.
	requestsMu.Lock()
	contended := requests["b-weights.bin"]
	requestsMu.Unlock()
	if contended != 0 {
		t.Fatalf("the branch variant fetched the contended blob %d times; want 0", contended)
	}
	// The branch variant materialized its own snapshot entry ...
	snapshot, err := repo.SnapshotPath("commit-a", "b-weights.bin")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(snapshot); err != nil || string(got) != string(shared) {
		t.Fatalf("the branch variant snapshot entry %s = %q, %v; want %q", snapshot, got, err, shared)
	}
	// ... and its own variant payload.
	variantDir, err := repo.BranchVariantPath("4.00bpw", "commit-a")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		payload := filepath.Join(variantDir, name)
		if got, err := os.ReadFile(payload); err != nil || string(got) != string(want) {
			t.Fatalf("the branch variant payload %s = %q, %v; want %q", payload, got, err, want)
		}
	}
	manifestDir, err := repo.BranchVariantMetadataPath("4.00bpw", "commit-a")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ReadManifest(filepath.Join(manifestDir, ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.BranchVariant || manifest.Commit != "commit-a" || manifest.TotalFiles != len(files) {
		t.Fatalf("the branch variant manifest = %+v; want the complete variant proof", manifest)
	}
}

// waitForPath waits until one path exists.
func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Lstat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the transfer did not create %q", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCancelledBranchVariantRetryDoesNotTouchOtherVariant(t *testing.T) {
	revisions := branchVariantTestRevisions()
	blocked := make(chan struct{})
	release := make(chan struct{})
	var block bool
	var blockMu sync.Mutex
	srv := newBranchVariantTestHubWithHook(t, revisions, func(commit, name string) {
		blockMu.Lock()
		shouldBlock := block && commit == "commit-b" && name == "weights.safetensors"
		if shouldBlock {
			block = false
			close(blocked)
		}
		blockMu.Unlock()
		if shouldBlock {
			<-release
		}
	})
	defer srv.Close()

	root := t.TempDir()
	settings := Settings{OutputDir: root, Endpoint: srv.URL, Concurrency: 1, Verify: "size"}
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, settings, nil); err != nil {
		t.Fatal(err)
	}
	branchA, err := BranchVariantPath(root, RepoTypeModel, "owner/model", "4.00bpw", "commit-a")
	if err != nil {
		t.Fatal(err)
	}
	blockMu.Lock()
	block = true
	blockMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- Download(ctx, Job{Repo: "owner/model", Revision: "4_25", BranchVariant: true}, settings, nil)
	}()
	select {
	case <-blocked:
		cancel()
		close(release)
	case <-time.After(5 * time.Second):
		cancel()
		close(release)
		t.Fatal("second variant did not reach the cancellation gate after writing its colliding config")
	}
	if err := <-result; err == nil {
		t.Fatal("cancelled branch variant download returned success")
	}
	for name, want := range revisions["4.00bpw"].files {
		got, err := os.ReadFile(filepath.Join(branchA, name))
		if err != nil || string(got) != string(want) {
			t.Fatalf("cancelled branch changed first variant %s = %q, %v; want %q", name, got, err, want)
		}
	}
	branchB, err := BranchVariantPath(root, RepoTypeModel, "owner/model", "4_25", "commit-b")
	if err != nil {
		t.Fatal(err)
	}
	controlB, err := BranchVariantMetadataPath(root, RepoTypeModel, "owner/model", "4_25", "commit-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(controlB, ManifestFilename)); !os.IsNotExist(err) {
		t.Fatalf("cancelled variant has completion evidence: %v", err)
	}
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "4_25", BranchVariant: true}, settings, nil); err != nil {
		t.Fatalf("retry variant B: %v", err)
	}
	for name, want := range revisions["4_25"].files {
		got, err := os.ReadFile(filepath.Join(branchB, name))
		if err != nil || string(got) != string(want) {
			t.Fatalf("retried branch B %s = %q, %v; want %q", name, got, err, want)
		}
	}
	for name, want := range revisions["4.00bpw"].files {
		got, err := os.ReadFile(filepath.Join(branchA, name))
		if err != nil || string(got) != string(want) {
			t.Fatalf("retry changed first variant %s = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestSyncAndRebuildPreserveAndRestoreBranchVariantViews(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the generated rebuild script is a Bash script")
	}
	revisions := branchVariantTestRevisions()
	ordinaryRelative := filepath.ToSlash(filepath.Join(
		"variants",
		BranchVariantRevisionDirName("4.00bpw"),
		BranchVariantCommitDirName(revisions["4.00bpw"].commit),
		"config.json",
	))
	revisions["main"] = branchVariantTestRevision{commit: "main-commit", files: map[string][]byte{ordinaryRelative: []byte(`{"model":"ordinary"}`)}}
	srv := newBranchVariantTestHub(t, revisions)
	defer srv.Close()

	cacheRoot := t.TempDir()
	cache := NewHFCache(cacheRoot, 0)
	settings := Settings{CacheDir: cacheRoot, Endpoint: srv.URL, Concurrency: 1, Verify: "sha256"}
	for _, revision := range []string{"4.00bpw", "4_25"} {
		if err := Download(context.Background(), Job{Repo: "owner/model", Revision: revision, BranchVariant: true}, settings, nil); err != nil {
			t.Fatalf("download %s: %v", revision, err)
		}
	}
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, settings, nil); err != nil {
		t.Fatalf("ordinary main download at the former variant-relative path: %v", err)
	}
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	first, err := repo.BranchVariantPath("4.00bpw", revisions["4.00bpw"].commit)
	if err != nil {
		t.Fatal(err)
	}
	firstConfig := filepath.Join(first, "config.json")
	if err := os.Remove(firstConfig); err != nil {
		t.Fatalf("remove friendly variant entry: %v", err)
	}
	if _, err := cache.Sync(SyncOptions{Clean: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(firstConfig); err != nil || string(got) != string(revisions["4.00bpw"].files["config.json"]) {
		t.Fatalf("sync restored branch config = %q, %v", got, err)
	}
	ordinaryConfig := filepath.Join(repo.FriendlyPath(), filepath.FromSlash(ordinaryRelative))
	if got, err := os.ReadFile(ordinaryConfig); err != nil || string(got) != `{"model":"ordinary"}` {
		t.Fatalf("sync changed ordinary legacy path = %q, %v", got, err)
	}
	legacyConfig := filepath.Join(repo.FriendlyPath(), "config.json")
	if _, err := os.Lstat(legacyConfig); !os.IsNotExist(err) {
		t.Fatalf("sync projected a branch into the legacy repo root: %v", err)
	}

	if err := os.Remove(firstConfig); err != nil {
		t.Fatalf("remove branch entry before script rebuild: %v", err)
	}
	script := filepath.Join(cacheRoot, "rebuild.sh")
	output, err := runGeneratedRebuildScript(t, script, "--verbose")
	if err != nil {
		t.Fatalf("rebuild script: %v: %s", err, output)
	}
	if got, err := os.ReadFile(firstConfig); err != nil || string(got) != string(revisions["4.00bpw"].files["config.json"]) {
		t.Fatalf("script restored branch config = %q, %v: %s", got, err, output)
	}
	if got, err := os.ReadFile(ordinaryConfig); err != nil || string(got) != `{"model":"ordinary"}` {
		t.Fatalf("script changed ordinary legacy path = %q, %v: %s", got, err, output)
	}
	second, err := repo.BranchVariantPath("4_25", revisions["4_25"].commit)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(second, "config.json")); err != nil || string(got) != string(revisions["4_25"].files["config.json"]) {
		t.Fatalf("rebuild changed second variant = %q, %v", got, err)
	}
}

func TestSyncAndScriptUseOrdinaryManifestInsteadOfBranchSnapshotFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the generated rebuild script is a POSIX Bash script")
	}
	revisions := branchVariantTestRevisions()
	revisions["dev"] = branchVariantTestRevision{commit: "commit-z", files: map[string][]byte{"config.json": []byte(`{"model":"ordinary-dev"}`)}}
	hub := newBranchVariantTestHub(t, revisions)
	defer hub.Close()
	root := t.TempDir()
	cache := NewHFCache(root, 0)
	settings := Settings{CacheDir: root, Endpoint: hub.URL, Concurrency: 1, Verify: "sha256"}
	for _, job := range []Job{
		{Repo: "owner/model", Revision: "dev"},
		{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true},
	} {
		if err := Download(context.Background(), job, settings, nil); err != nil {
			t.Fatalf("download %s: %v", job.Revision, err)
		}
	}
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryConfig := filepath.Join(repo.FriendlyPath(), "config.json")
	branchConfig, err := repo.BranchVariantPath("4.00bpw", "commit-a")
	if err != nil {
		t.Fatal(err)
	}
	branchConfig = filepath.Join(branchConfig, "config.json")
	if _, err := cache.Sync(SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(ordinaryConfig); err != nil || string(got) != `{"model":"ordinary-dev"}` {
		t.Fatalf("Go Sync ordinary config=%q err=%v; want dev@commit-z", got, err)
	}
	if got, err := os.ReadFile(branchConfig); err != nil || string(got) != `{"model":"A"}` {
		t.Fatalf("Go Sync branch config=%q err=%v; want commit-a", got, err)
	}
	if _, err := cache.WriteRebuildScript(); err != nil {
		t.Fatal(err)
	}
	output, err := runGeneratedRebuildScript(t, cache.RebuildScriptPath())
	if err != nil {
		t.Fatalf("generated rebuild: %v: %s", err, output)
	}
	if got, err := os.ReadFile(ordinaryConfig); err != nil || string(got) != `{"model":"ordinary-dev"}` {
		t.Fatalf("rebuild script ordinary config=%q err=%v; want dev@commit-z: %s", got, err, output)
	}
	if got, err := os.ReadFile(branchConfig); err != nil || string(got) != `{"model":"A"}` {
		t.Fatalf("rebuild script branch config=%q err=%v; want commit-a: %s", got, err, output)
	}

	branchOnlyRoot := t.TempDir()
	branchOnlyHub := newBranchVariantTestHub(t, map[string]branchVariantTestRevision{"4.00bpw": branchVariantTestRevisions()["4.00bpw"]})
	defer branchOnlyHub.Close()
	branchOnlyCache := NewHFCache(branchOnlyRoot, 0)
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, Settings{
		CacheDir: branchOnlyRoot, Endpoint: branchOnlyHub.URL, Concurrency: 1,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := branchOnlyCache.WriteRebuildScript(); err != nil {
		t.Fatal(err)
	}
	if output, err := runGeneratedRebuildScript(t, branchOnlyCache.RebuildScriptPath()); err != nil {
		t.Fatalf("branch-only rebuild: %v: %s", err, output)
	}
	branchOnlyRepo, err := branchOnlyCache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(branchOnlyRepo.FriendlyPath(), "config.json")); !os.IsNotExist(err) {
		t.Fatalf("branch-only cache projected a branch snapshot into ordinary config.json: %v", err)
	}
}

func TestSyncAndRebuildRefuseOrdinarySymlinkProjectionIntoOwnedServiceTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the generated rebuild script is a Bash script and directory symlink setup is unavailable in some Windows environments")
	}
	cacheRoot := t.TempDir()
	cache := NewHFCache(cacheRoot, 0)
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	artifact, err := ClaimBranchVariantArtifact(cacheRoot, RepoTypeModel, "owner/model", "4.00bpw", "commit-a")
	if err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(artifact.PayloadRoot, "weights.safetensors")
	if err := os.WriteFile(protected, []byte("AAAAAAAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.WriteRef("main", "main-commit"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := repo.SnapshotPath("main-commit", "alias/weights.safetensors")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(snapshot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, []byte("BBBBBBBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(repo.FriendlyPath(), "alias")
	if err := os.MkdirAll(filepath.Dir(alias), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifact.PayloadRoot, alias); err != nil {
		t.Skipf("directory symlink setup is unavailable: %v", err)
	}

	result, err := cache.Sync(SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) == 0 || !strings.Contains(strings.Join(errorStrings(result.Errors), " "), "app-owned EXL branch variant service tree") {
		t.Fatalf("Sync did not report the physical service conflict: %+v", result)
	}
	if got, err := os.ReadFile(protected); err != nil || string(got) != "AAAAAAAA" {
		t.Fatalf("Sync changed protected payload through symlink: got=%q err=%v", got, err)
	}

	if _, err := cache.WriteRebuildScript(); err != nil {
		t.Fatal(err)
	}
	output, err := runGeneratedRebuildScript(t, cache.RebuildScriptPath(), "--verbose")
	if err == nil || !strings.Contains(string(output), "ordinary download path") || !strings.Contains(string(output), "app-owned EXL branch variant service tree") {
		t.Fatalf("rebuild alias conflict err=%v output=%s", err, output)
	}
	if got, err := os.ReadFile(protected); err != nil || string(got) != "AAAAAAAA" {
		t.Fatalf("rebuild changed protected payload through symlink: got=%q err=%v", got, err)
	}
}

func errorStrings(errs []error) []string {
	result := make([]string, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			result = append(result, err.Error())
		}
	}
	return result
}
