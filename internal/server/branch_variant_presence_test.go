// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

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
	"sync"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

const presenceRepo = "owner/example-EXL3-model"
const presenceRevision = "4.00bpw"

func writePresenceRepo(t *testing.T, root, revision string) *hfdownloader.RepoDir {
	t.Helper()
	commit := "2222222222222222222222222222222222222222"
	files := map[string]string{
		"config.json":       `{"model_type":"exl3","bits":4}`,
		"model.safetensors": "branch-a-weights",
	}
	cache := hfdownloader.NewHFCacheResolved(root, "", 0)
	repoDir, err := cache.Repo(presenceRepo, hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repoDir.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	manifest := &hfdownloader.DownloadManifest{
		Version:       "1.0",
		Type:          "model",
		Repo:          presenceRepo,
		Branch:        revision,
		Commit:        commit,
		BranchVariant: true,
		CompletedAt:   time.Now().UTC(),
		TotalFiles:    len(files),
		Files:         []hfdownloader.ManifestFile{},
	}
	var snapshotFiles []hfdownloader.SnapshotFile
	for name, body := range files {
		sum := sha256.Sum256([]byte(body))
		blobName := hex.EncodeToString(sum[:])
		if err := os.WriteFile(repoDir.BlobPath(blobName), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		snapshotFiles = append(snapshotFiles, hfdownloader.SnapshotFile{RelativePath: name, SHA256: blobName})
		manifest.Files = append(manifest.Files, hfdownloader.ManifestFile{Name: name, Blob: "blobs/" + blobName, Size: int64(len(body))})
		manifest.TotalSize += int64(len(body))
	}
	if err := repoDir.CreateSnapshot(commit, snapshotFiles); err != nil {
		t.Fatal(err)
	}
	if err := repoDir.WriteRef(revision, commit); err != nil {
		t.Fatal(err)
	}
	manifestDir, err := hfdownloader.BranchVariantManifestDir(repoDir, revision, commit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Write(manifestDir); err != nil {
		t.Fatal(err)
	}
	return repoDir
}

func writeLocalPresenceVariant(t *testing.T, root string) string {
	t.Helper()
	const commit = "2222222222222222222222222222222222222222"
	repoBase := filepath.Join(root, presenceRepo)
	variantDir := filepath.Join(repoBase, presenceRevision)
	if err := os.MkdirAll(variantDir, 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"config.json":       `{"model_type":"exl3","bits":4}`,
		"model.safetensors": "local-branch",
	}
	manifest := &hfdownloader.DownloadManifest{
		Version:       "1.0",
		Type:          "model",
		Repo:          presenceRepo,
		Branch:        presenceRevision,
		Commit:        commit,
		BranchVariant: true,
		CompletedAt:   time.Now().UTC(),
		TotalFiles:    len(files),
		Files:         []hfdownloader.ManifestFile{},
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(variantDir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		manifest.Files = append(manifest.Files, hfdownloader.ManifestFile{Name: name, Size: int64(len(body))})
	}
	// The completion record lives outside the payload tree, keyed by revision
	// and commit.
	manifestDir, err := hfdownloader.BranchVariantLocalManifestDir(repoBase, presenceRevision, commit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Write(manifestDir); err != nil {
		t.Fatal(err)
	}
	return variantDir
}

func TestBranchVariantPresenceRequiresManifestAndEveryFile(t *testing.T) {
	root := t.TempDir()
	repoDir := writePresenceRepo(t, root, presenceRevision)
	s := newTestServerWithConfig(t, Config{CacheDir: root})

	request := func(revision string) branchVariantPresence {
		t.Helper()
		w := cacheRequest(t, s, "GET", "/api/cache/"+presenceRepo+"?branchVariant=true&revision="+revision, "")
		if w.Code != http.StatusOK {
			t.Fatalf("presence status = %d body=%s", w.Code, w.Body.String())
		}
		var got branchVariantPresence
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := request(presenceRevision)
	if !got.Downloaded || got.DownloadStatus != "complete" || got.StorageMode != "hf-cache" {
		t.Fatalf("complete presence = %+v", got)
	}
	if got.Commit != "2222222222222222222222222222222222222222" || got.ExpectedFiles != 2 || len(got.Files) != 2 {
		t.Fatalf("complete presence identity = %+v", got)
	}
	if got.ArtifactPath == "" || !strings.HasSuffix(got.ArtifactPath, hfdownloader.ManifestFilename) {
		t.Fatalf("complete presence artifact path = %q", got.ArtifactPath)
	}

	commit := got.Commit
	if err := os.Remove(filepath.Join(repoDir.SnapshotsDir(), commit, "model.safetensors")); err != nil {
		t.Fatal(err)
	}
	got = request(presenceRevision)
	if got.Downloaded || got.DownloadStatus != "missing" {
		t.Fatalf("presence after deleting a file = %+v", got)
	}

	got = request("6.00bpw")
	if got.Downloaded || got.DownloadStatus != "missing" || got.Commit != "" {
		t.Fatalf("presence without ref/manifest = %+v", got)
	}
}

func TestBranchVariantPresenceRejectsManifestCommitMismatch(t *testing.T) {
	root := t.TempDir()
	repoDir := writePresenceRepo(t, root, presenceRevision)
	commit, err := repoDir.ReadRef(presenceRevision)
	if err != nil {
		t.Fatal(err)
	}
	manifestDir, err := hfdownloader.BranchVariantManifestDir(repoDir, presenceRevision, commit)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := hfdownloader.ReadManifest(filepath.Join(manifestDir, hfdownloader.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Commit = "3333333333333333333333333333333333333333"
	if _, err := manifest.Write(manifestDir); err != nil {
		t.Fatal(err)
	}

	s := newTestServerWithConfig(t, Config{CacheDir: root})
	w := cacheRequest(t, s, "GET", "/api/cache/"+presenceRepo+"?branchVariant=true&revision="+presenceRevision, "")
	if w.Code != http.StatusOK {
		t.Fatalf("presence status = %d body=%s", w.Code, w.Body.String())
	}
	var got branchVariantPresence
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Downloaded || got.DownloadStatus != "missing" {
		t.Fatalf("mismatched manifest commit reported downloaded: %+v", got)
	}
}

func TestBranchVariantPresenceLocalMode(t *testing.T) {
	root := t.TempDir()
	variantDir := writeLocalPresenceVariant(t, root)

	s := newTestServerWithConfig(t, Config{LocalDir: root})
	request := func() branchVariantPresence {
		t.Helper()
		w := cacheRequest(t, s, "GET", "/api/cache/"+presenceRepo+"?branchVariant=true&revision="+presenceRevision, "")
		if w.Code != http.StatusOK {
			t.Fatalf("local presence status = %d body=%s", w.Code, w.Body.String())
		}
		var got branchVariantPresence
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := request()
	if !got.Downloaded || got.DownloadStatus != "complete" || got.StorageMode != "local" {
		t.Fatalf("local complete presence = %+v", got)
	}
	if err := os.Remove(filepath.Join(variantDir, "config.json")); err != nil {
		t.Fatal(err)
	}
	got = request()
	if got.Downloaded || got.DownloadStatus != "missing" {
		t.Fatalf("local presence after deleting a file = %+v", got)
	}
}

func TestBranchVariantPresenceFindsConfiguredRouteWithoutSelector(t *testing.T) {
	defaultRoot := t.TempDir()
	routeRoot := t.TempDir()
	wantDir := writeLocalPresenceVariant(t, routeRoot)

	s := newTestServerWithConfig(t, Config{
		LocalDir:       defaultRoot,
		DownloadRoutes: map[string]string{"llm": routeRoot},
	})
	request := func(query string) branchVariantPresence {
		t.Helper()
		w := cacheRequest(t, s, "GET", "/api/cache/"+presenceRepo+"?branchVariant=true&revision="+presenceRevision+query, "")
		if w.Code != http.StatusOK {
			t.Fatalf("route presence status = %d body=%s", w.Code, w.Body.String())
		}
		var got branchVariantPresence
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := request("")
	if !got.Downloaded || got.DownloadStatus != "complete" || got.StorageMode != "local" {
		t.Fatalf("unqualified route presence = %+v", got)
	}
	if got.DestinationRoot != wantDir {
		t.Fatalf("unqualified route destination = %q, want %q", got.DestinationRoot, wantDir)
	}

	explicit := request("&routeKey=llm")
	if !explicit.Downloaded || explicit.DownloadStatus != "complete" || explicit.StorageMode != "local" {
		t.Fatalf("explicit route presence = %+v", explicit)
	}
	if explicit.DestinationRoot != wantDir {
		t.Fatalf("explicit route destination = %q, want %q", explicit.DestinationRoot, wantDir)
	}
}

func TestValidateBranchVariantRequest(t *testing.T) {
	base := DownloadRequest{Repo: "owner/model", Revision: presenceRevision, BranchVariant: true}
	tests := []struct {
		name    string
		req     DownloadRequest
		wantErr bool
	}{
		{name: "valid branch variant", req: base},
		{name: "ordinary request unchanged", req: DownloadRequest{Repo: "owner/model", Filters: []string{"q4"}, Dataset: true, LocalRepo: "owner/target"}},
		{name: "dataset", req: DownloadRequest{Repo: "owner/model", Revision: presenceRevision, BranchVariant: true, Dataset: true}, wantErr: true},
		{name: "missing revision", req: DownloadRequest{Repo: "owner/model", BranchVariant: true}, wantErr: true},
		{name: "non branch revision", req: DownloadRequest{Repo: "owner/model", Revision: "main", BranchVariant: true}, wantErr: true},
		{name: "empty manifest component", req: DownloadRequest{Repo: "owner/model", Revision: "exl//4_25", BranchVariant: true}, wantErr: true},
		{name: "overlong manifest component", req: DownloadRequest{Repo: "owner/model", Revision: strings.Repeat("a", 256) + "/4_25", BranchVariant: true}, wantErr: true},
		{name: "filters", req: DownloadRequest{Repo: "owner/model", Revision: presenceRevision, BranchVariant: true, Filters: []string{"config.json"}}, wantErr: true},
		{name: "excludes", req: DownloadRequest{Repo: "owner/model", Revision: presenceRevision, BranchVariant: true, Excludes: []string{"config.json"}}, wantErr: true},
		{name: "append filter subdir", req: DownloadRequest{Repo: "owner/model", Revision: presenceRevision, BranchVariant: true, AppendFilterSubdir: true}, wantErr: true},
		{name: "local repo", req: DownloadRequest{Repo: "owner/model", Revision: presenceRevision, BranchVariant: true, LocalRepo: "owner/other"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBranchVariantRequest(tt.req)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateBranchVariantRequest() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBranchVariantEndpointsRejectInvalidRequests(t *testing.T) {
	s := newTestServer(t)
	bodies := map[string]string{
		"filters":   `{"repo":"owner/model","revision":"4.00bpw","branchVariant":true,"filters":["config.json"]}`,
		"dataset":   `{"repo":"owner/model","revision":"4.00bpw","branchVariant":true,"dataset":true}`,
		"localRepo": `{"repo":"owner/model","revision":"4.00bpw","branchVariant":true,"localRepo":"owner/other"}`,
		// These pass the analyzer's branch shape but cannot become a safe
		// completion-manifest path component sequence.
		"emptyRevisionComponent":    `{"repo":"owner/model","revision":"exl//4_25","branchVariant":true}`,
		"overlongRevisionComponent": `{"repo":"owner/model","revision":"` + strings.Repeat("a", 256) + `/4_25","branchVariant":true}`,
	}
	for name, body := range bodies {
		t.Run("download/"+name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handleStartDownload(w, httptest.NewRequest("POST", "/api/download", strings.NewReader(body)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
		})
		t.Run("plan/"+name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handlePlan(w, httptest.NewRequest("POST", "/api/plan", strings.NewReader(body)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestBranchVariantPresenceCoexistsForRevisionsSharingCommit discriminates
// defect 2 at the presence layer: both branches point at one commit, so their
// completion records must be keyed by revision as well as commit and both
// revisions must report complete.
func TestBranchVariantPresenceCoexistsForRevisionsSharingCommit(t *testing.T) {
	root := t.TempDir()
	repoDir := writePresenceRepo(t, root, "4.00bpw")
	writePresenceRepo(t, root, "5.00bpw")

	s := newTestServerWithConfig(t, Config{CacheDir: root})
	for _, revision := range []string{"4.00bpw", "5.00bpw"} {
		w := cacheRequest(t, s, "GET", "/api/cache/"+presenceRepo+"?branchVariant=true&revision="+revision, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s presence status = %d body=%s", revision, w.Code, w.Body.String())
		}
		var got branchVariantPresence
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !got.Downloaded || got.DownloadStatus != "complete" {
			t.Fatalf("%s presence = %+v", revision, got)
		}
		manifestDir, err := hfdownloader.BranchVariantManifestDir(repoDir, revision, got.Commit)
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := hfdownloader.ReadManifest(filepath.Join(manifestDir, hfdownloader.ManifestFilename))
		if err != nil {
			t.Fatalf("%s completion manifest: %v", revision, err)
		}
		if manifest.Branch != revision {
			t.Errorf("%s completion manifest branch = %q", revision, manifest.Branch)
		}
	}
}

// TestBranchVariantLocalDownloadPreservesRepositoryManifestAndReportsPresence
// discriminates defect 4 end to end: the branch ships a repository-owned
// hfd.yaml, the completion record must live outside the payload tree, and
// presence must still report the revision complete.
func TestBranchVariantLocalDownloadPreservesRepositoryManifestAndReportsPresence(t *testing.T) {
	const (
		revision     = "7.00bpw"
		commit       = "7777777777777777777777777777777777777777"
		repoManifest = "# repository-owned hfd.yaml\n"
	)
	files := map[string]string{
		hfdownloader.ManifestFilename: repoManifest,
		"model.safetensors":           "local-branch-weights",
	}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": commit})
		case strings.Contains(r.URL.Path, "/tree/"):
			nodes := make([]map[string]any, 0, len(files))
			for name, body := range files {
				nodes = append(nodes, map[string]any{"type": "file", "path": name, "size": len(body)})
			}
			_ = json.NewEncoder(w).Encode(nodes)
		case strings.Contains(r.URL.Path, "/raw/"):
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			body, ok := files[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	root := t.TempDir()
	if err := hfdownloader.Download(context.Background(), hfdownloader.Job{
		Repo:          presenceRepo,
		Revision:      revision,
		BranchVariant: true,
	}, hfdownloader.Settings{OutputDir: root, Endpoint: hub.URL, Verify: "size"}, nil); err != nil {
		t.Fatalf("local branch download: %v", err)
	}

	payloadDir := filepath.Join(root, presenceRepo, revision)
	payload, err := os.ReadFile(filepath.Join(payloadDir, hfdownloader.ManifestFilename))
	if err != nil || string(payload) != repoManifest {
		t.Fatalf("payload hfd.yaml = %q err=%v, want repository content", payload, err)
	}
	manifestDir, err := hfdownloader.BranchVariantLocalManifestDir(filepath.Join(root, presenceRepo), revision, commit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(manifestDir, hfdownloader.ManifestFilename)); err != nil {
		t.Fatalf("completion manifest outside payload tree: %v", err)
	}

	s := newTestServerWithConfig(t, Config{LocalDir: root})
	w := cacheRequest(t, s, "GET", "/api/cache/"+presenceRepo+"?branchVariant=true&revision="+revision, "")
	if w.Code != http.StatusOK {
		t.Fatalf("presence status = %d body=%s", w.Code, w.Body.String())
	}
	var got branchVariantPresence
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Downloaded || got.DownloadStatus != "complete" || got.Commit != commit {
		t.Fatalf("presence = %+v", got)
	}
}

// writeLocalPresenceRecord writes one local completion record under
// <root>/<repo>/.hfdesk-variants/<revision>/<commit> and optionally the payload
// files it should describe. listed is the manifest file set, so a test can list
// a file that the payload does not contain.
func writeLocalPresenceRecord(t *testing.T, root, revision, commit string, completedAt time.Time, listed, payload map[string]string) string {
	t.Helper()
	repoBase := filepath.Join(root, presenceRepo)
	variantDir := filepath.Join(repoBase, revision)
	if payload != nil {
		if err := os.MkdirAll(variantDir, 0755); err != nil {
			t.Fatal(err)
		}
		for name, body := range payload {
			path := filepath.Join(variantDir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	manifest := &hfdownloader.DownloadManifest{
		Version:       "1.0",
		Type:          "model",
		Repo:          presenceRepo,
		Branch:        revision,
		Commit:        commit,
		BranchVariant: true,
		CompletedAt:   completedAt,
		TotalFiles:    len(listed),
		Files:         []hfdownloader.ManifestFile{},
	}
	names := make([]string, 0, len(listed))
	for name := range listed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := listed[name]
		manifest.Files = append(manifest.Files, hfdownloader.ManifestFile{Name: name, Size: int64(len(body))})
		manifest.TotalSize += int64(len(body))
	}
	manifestDir, err := hfdownloader.BranchVariantLocalManifestDir(repoBase, revision, commit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Write(manifestDir); err != nil {
		t.Fatal(err)
	}
	return variantDir
}

// TestBranchVariantPresenceLocalUsesNewestRecordOnly pins the local presence
// rule: an older complete record must not validate a payload that a newer
// attempt has replaced. Local records carry no content hash, so size-only
// checks cannot distinguish the two commits' bytes.
func TestBranchVariantPresenceLocalUsesNewestRecordOnly(t *testing.T) {
	const (
		revision = presenceRevision
		older    = "1111111111111111111111111111111111111111"
		newer    = "2222222222222222222222222222222222222222"
	)
	root := t.TempDir()
	common := map[string]string{
		"config.json":       `{"model_type":"exl3","bits":4}`,
		"model.safetensors": "shared-common-bytes",
	}
	writeLocalPresenceRecord(t, root, revision, older, time.Now().Add(-time.Hour).UTC(), common, common)
	// The newer commit lists the same common files plus one file the payload
	// no longer has; it is therefore incomplete.
	newerFiles := map[string]string{
		"config.json":       common["config.json"],
		"model.safetensors": common["model.safetensors"],
		"new-only.bin":      "newer-only-bytes",
	}
	writeLocalPresenceRecord(t, root, revision, newer, time.Now().UTC(), newerFiles, nil)

	s := newTestServerWithConfig(t, Config{LocalDir: root})
	w := cacheRequest(t, s, "GET", "/api/cache/"+presenceRepo+"?branchVariant=true&revision="+revision, "")
	if w.Code != http.StatusOK {
		t.Fatalf("presence status = %d body=%s", w.Code, w.Body.String())
	}
	var got branchVariantPresence
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Downloaded || got.DownloadStatus != "missing" {
		t.Fatalf("presence = %+v, want missing without falling back to the older complete record", got)
	}
	if got.Commit != newer {
		t.Fatalf("presence commit = %q, want newest record commit %q", got.Commit, newer)
	}
}

// TestBranchVariantPresenceLocalRecordTieBreak pins the deterministic choice
// between records that completed at the same instant: the lexicographically
// first commit wins, and an incomplete winner is not replaced by a later
// complete record.
func TestBranchVariantPresenceLocalRecordTieBreak(t *testing.T) {
	const (
		revision     = presenceRevision
		firstCommit  = "1111111111111111111111111111111111111111"
		secondCommit = "2222222222222222222222222222222222222222"
	)
	root := t.TempDir()
	completedAt := time.Now().UTC()
	payload := map[string]string{"config.json": `{"model_type":"exl3","bits":4}`}
	// The lexicographically first record is incomplete; the second is
	// complete. Presence must choose the first and report missing.
	incomplete := map[string]string{
		"config.json": `{"model_type":"exl3","bits":4}`,
		"missing.bin": "absent",
	}
	writeLocalPresenceRecord(t, root, revision, firstCommit, completedAt, incomplete, payload)
	writeLocalPresenceRecord(t, root, revision, secondCommit, completedAt, payload, nil)

	s := newTestServerWithConfig(t, Config{LocalDir: root})
	w := cacheRequest(t, s, "GET", "/api/cache/"+presenceRepo+"?branchVariant=true&revision="+revision, "")
	if w.Code != http.StatusOK {
		t.Fatalf("presence status = %d body=%s", w.Code, w.Body.String())
	}
	var got branchVariantPresence
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Downloaded || got.DownloadStatus != "missing" || got.Commit != firstCommit {
		t.Fatalf("tie-break presence = %+v, want missing for lexicographically first commit %q", got, firstCommit)
	}
}

// TestBranchVariantLocalRedownloadInvalidatesOlderRecord is the end-to-end
// invalidation contract: a complete A record is removed when a re-download
// starts, a failed B download leaves presence missing rather than falling back
// to A, and a later successful B writes its own complete record.
func TestBranchVariantLocalRedownloadInvalidatesOlderRecord(t *testing.T) {
	const (
		revision = presenceRevision
		commitA  = "1111111111111111111111111111111111111111"
		commitB  = "2222222222222222222222222222222222222222"
	)
	filesA := map[string]string{
		"config.json":       `{"version":"A"}`,
		"model.safetensors": "a-body",
	}
	filesB := map[string]string{
		"config.json":       `{"version":"B","new":true}`,
		"model.safetensors": "b-body-longer",
		"new-only.bin":      "b-extra",
	}

	var mu sync.Mutex
	mode := "A"
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current := mode
		mu.Unlock()
		commit := commitA
		files := filesA
		if current != "A" {
			commit = commitB
			files = filesB
		}
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": commit})
		case strings.Contains(r.URL.Path, "/tree/"):
			nodes := make([]map[string]any, 0, len(files))
			for name, body := range files {
				nodes = append(nodes, map[string]any{"type": "file", "path": name, "size": len(body)})
			}
			_ = json.NewEncoder(w).Encode(nodes)
		case strings.Contains(r.URL.Path, "/raw/"):
			if current == "fail-B" {
				http.NotFound(w, r)
				return
			}
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			body, ok := files[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	root := t.TempDir()
	cfg := hfdownloader.Settings{
		OutputDir:          root,
		Endpoint:           hub.URL,
		Verify:             "size",
		Concurrency:        1,
		MaxActiveDownloads: 1,
		Retries:            1,
		BackoffInitial:     "1ms",
		BackoffMax:         "1ms",
	}
	job := hfdownloader.Job{Repo: presenceRepo, Revision: revision, BranchVariant: true}
	if err := hfdownloader.Download(context.Background(), job, cfg, nil); err != nil {
		t.Fatalf("download A: %v", err)
	}

	s := newTestServerWithConfig(t, Config{LocalDir: root})
	requestPresence := func() branchVariantPresence {
		t.Helper()
		w := cacheRequest(t, s, "GET", "/api/cache/"+presenceRepo+"?branchVariant=true&revision="+revision, "")
		if w.Code != http.StatusOK {
			t.Fatalf("presence status = %d body=%s", w.Code, w.Body.String())
		}
		var got branchVariantPresence
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := requestPresence(); !got.Downloaded || got.Commit != commitA {
		t.Fatalf("presence after A = %+v", got)
	}

	mu.Lock()
	mode = "fail-B"
	mu.Unlock()
	if err := hfdownloader.Download(context.Background(), job, cfg, nil); err == nil {
		t.Fatal("re-download with unavailable payload unexpectedly succeeded")
	}
	if got := requestPresence(); got.Downloaded || got.DownloadStatus != "missing" {
		t.Fatalf("presence after failed B = %+v, want missing without the invalidated A record", got)
	}

	mu.Lock()
	mode = "B"
	mu.Unlock()
	if err := hfdownloader.Download(context.Background(), job, cfg, nil); err != nil {
		t.Fatalf("download B: %v", err)
	}
	got := requestPresence()
	if !got.Downloaded || got.DownloadStatus != "complete" || got.Commit != commitB {
		t.Fatalf("presence after B = %+v", got)
	}
}

// TestBranchVariantLocalRedownloadRefetchesSameSizeHashlessFile pins the
// hashless re-download rule: a moved local branch can keep a non-LFS file's
// size while changing its bytes, so a size-only skip would publish commit A's
// stale payload under commit B's completion record.
func TestBranchVariantLocalRedownloadRefetchesSameSizeHashlessFile(t *testing.T) {
	const (
		revision = presenceRevision
		commitA  = "1111111111111111111111111111111111111111"
		commitB  = "2222222222222222222222222222222222222222"
	)
	configA := `{"v":"A"}`
	configB := `{"v":"B"}`
	if len(configA) != len(configB) {
		t.Fatalf("fixture config sizes differ: %d vs %d", len(configA), len(configB))
	}
	filesA := map[string]string{"config.json": configA, "model.safetensors": "a-weights"}
	filesB := map[string]string{"config.json": configB, "model.safetensors": "b-weights"}

	var mu sync.Mutex
	mode := "A"
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current := mode
		mu.Unlock()
		commit := commitA
		files := filesA
		if current == "B" {
			commit = commitB
			files = filesB
		}
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": commit})
		case strings.Contains(r.URL.Path, "/tree/"):
			nodes := make([]map[string]any, 0, len(files))
			for name, body := range files {
				nodes = append(nodes, map[string]any{"type": "file", "path": name, "size": len(body)})
			}
			_ = json.NewEncoder(w).Encode(nodes)
		case strings.Contains(r.URL.Path, "/raw/"):
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			body, ok := files[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	root := t.TempDir()
	cfg := hfdownloader.Settings{
		OutputDir:          root,
		Endpoint:           hub.URL,
		Verify:             "size",
		Concurrency:        1,
		MaxActiveDownloads: 1,
		Retries:            1,
		BackoffInitial:     "1ms",
		BackoffMax:         "1ms",
	}
	job := hfdownloader.Job{Repo: presenceRepo, Revision: revision, BranchVariant: true}

	plan, err := hfdownloader.PlanRepo(context.Background(), job, cfg)
	if err != nil {
		t.Fatalf("plan A: %v", err)
	}
	hashless := false
	for _, item := range plan.Items {
		if item.RelativePath == "config.json" {
			hashless = !item.LFS && item.SHA256 == ""
		}
	}
	if !hashless {
		t.Fatal("fixture config.json is not a hashless non-LFS item")
	}

	if err := hfdownloader.Download(context.Background(), job, cfg, nil); err != nil {
		t.Fatalf("download A: %v", err)
	}
	payload := filepath.Join(root, presenceRepo, revision, "config.json")
	if body, err := os.ReadFile(payload); err != nil || string(body) != configA {
		t.Fatalf("payload after A = %q err=%v, want %q", body, err, configA)
	}

	mu.Lock()
	mode = "B"
	mu.Unlock()
	if err := hfdownloader.Download(context.Background(), job, cfg, nil); err != nil {
		t.Fatalf("download B: %v", err)
	}

	body, err := os.ReadFile(payload)
	if err != nil || string(body) != configB {
		t.Fatalf("payload after B = %q err=%v, want commit B bytes %q", body, err, configB)
	}
	manifestDir, err := hfdownloader.BranchVariantLocalManifestDir(filepath.Join(root, presenceRepo), revision, commitB)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := hfdownloader.ReadManifest(filepath.Join(manifestDir, hfdownloader.ManifestFilename))
	if err != nil {
		t.Fatalf("commit B completion manifest: %v", err)
	}
	if manifest.Commit != commitB || manifest.Branch != revision {
		t.Fatalf("completion manifest = commit %q branch %q, want %q/%q", manifest.Commit, manifest.Branch, commitB, revision)
	}

	s := newTestServerWithConfig(t, Config{LocalDir: root})
	w := cacheRequest(t, s, "GET", "/api/cache/"+presenceRepo+"?branchVariant=true&revision="+revision, "")
	if w.Code != http.StatusOK {
		t.Fatalf("presence status = %d body=%s", w.Code, w.Body.String())
	}
	var got branchVariantPresence
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Downloaded || got.DownloadStatus != "complete" || got.Commit != commitB {
		t.Fatalf("presence after B = %+v", got)
	}
}
