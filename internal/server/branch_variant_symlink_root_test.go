// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// symlinkedTestParent creates a directory that is reached through a linked
// parent. It is the spelling macOS uses for its temporary directory
// (/var -> /private/var), so the same root/service-tree identity mismatch can
// be exercised on any host. It returns the plain base and the linked path.
func symlinkedTestParent(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(base, "linked")
	if err := os.Symlink(real, linked); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	return base, linked
}

// TestSymlinkedCacheRootKeepsCopiedBranchPresence proves that a cache root
// reached through a linked parent keeps one path identity: the copied root is
// still recognized as a valid store, so the complete artifact is reported as
// present instead of absent.
func TestSymlinkedCacheRootKeepsCopiedBranchPresence(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	_, linked := symlinkedTestParent(t)
	endpoint, body, _, _ := cacheDownloadFixture(t, "")
	defer endpoint.Close()
	sourceRoot := filepath.Join(linked, "source-cache")
	copyRoot := filepath.Join(linked, "copied-cache")
	s := newTestServerWithConfig(t, Config{
		CacheDir: sourceRoot, Endpoint: endpoint.URL, MaxActive: 1, Concurrency: 1, Verify: "sha256",
	})
	started := cacheRequest(t, s, http.MethodPost, "/api/download", `{"repo":"owner/model","revision":"4.00bpw","branchVariant":true}`)
	if started.Code != http.StatusAccepted {
		t.Fatalf("branch download status=%d body=%s", started.Code, started.Body.String())
	}
	var job Job
	if err := json.Unmarshal(started.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if !s.jobs.WaitAll(10 * time.Second) {
		t.Fatal("branch download did not finish before the root copy")
	}
	if err := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: "main"}, hfdownloader.Settings{
		CacheDir: sourceRoot, Endpoint: endpoint.URL, Concurrency: 1, Verify: "sha256",
	}, nil); err != nil {
		t.Fatalf("ordinary download before the root copy: %v", err)
	}
	copyHFRootBytes(t, sourceRoot, copyRoot)
	if err := os.RemoveAll(sourceRoot); err != nil {
		t.Fatalf("remove the source root before the presence query: %v", err)
	}

	copiedServer := newTestServerWithConfig(t, Config{
		CacheDir: copyRoot, Endpoint: endpoint.URL, MaxActive: 1, Concurrency: 1, Verify: "sha256",
	})
	status, presence := branchPresenceRequest(t, copiedServer, "revision=4.00bpw")
	if status != http.StatusOK || !presence.Downloaded || presence.DownloadStatus != "complete" || presence.Commit != "commit123" {
		t.Fatalf("copied-root presence behind a linked parent status=%d response=%+v", status, presence)
	}
	if _, err := hfdownloader.NewHFCache(copyRoot, 0).Sync(hfdownloader.SyncOptions{}); err != nil {
		t.Fatalf("Sync copied HF root after source removal: %v", err)
	}
	status, after := branchPresenceRequest(t, copiedServer, "revision=4.00bpw")
	if status != http.StatusOK || !after.Downloaded || after.Commit != presence.Commit {
		t.Fatalf("copied-root presence after the rebuild status=%d response=%+v", status, after)
	}
	ordinary, err := os.ReadFile(filepath.Join(copyRoot, "models", "owner", "model", "weights.safetensors"))
	if err != nil || !bytes.Equal(ordinary, body) {
		t.Fatalf("ordinary Hub view after the copied-root rebuild=%q err=%v", ordinary, err)
	}
}

// TestSymlinkedCacheRootKeepsDeleteOwnershipPreflight proves that a delete
// whose recursive scope contains another repository's owned service tree is
// still refused with the ownership conflict before any removal, even when the
// selected cache root is reached through a linked parent.
func TestSymlinkedCacheRootKeepsDeleteOwnershipPreflight(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	_, linked := symlinkedTestParent(t)
	cacheRoot := filepath.Join(linked, "cache")
	cache := hfdownloader.NewHFCache(cacheRoot, 0)
	ordinary, err := cache.Repo("ordinary/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := ordinary.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := ordinary.WriteRef("main", "ordinary-commit"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ordinary.FriendlyPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	ordinaryMarker := filepath.Join(ordinary.FriendlyPath(), "keep.txt")
	if err := os.WriteFile(ordinaryMarker, []byte("ordinary"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifact, err := hfdownloader.ClaimBranchVariantArtifact(ordinary.FriendlyPath(), hfdownloader.RepoTypeModel, "other/model", "4_25", "commit-other")
	if err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(artifact.PayloadRoot, "weights.safetensors")
	if err := os.WriteFile(protected, []byte("AAAAAAAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	controlPath := filepath.Join(artifact.ControlRoot, ".hfdesk-owner.json")
	controlBefore, err := os.ReadFile(controlPath)
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServerWithConfig(t, Config{CacheDir: cacheRoot})
	response := cacheRequest(t, s, http.MethodDelete, "/api/cache/ordinary/model?type=model", "")
	if response.Code != http.StatusConflict {
		t.Fatalf("DELETE status=%d body=%s; want the ownership preflight conflict", response.Code, response.Body.String())
	}
	if _, err := os.Stat(ordinary.Path()); err != nil {
		t.Fatalf("Hub repo was partially deleted: %v", err)
	}
	if got, err := os.ReadFile(ordinaryMarker); err != nil || string(got) != "ordinary" {
		t.Fatalf("friendly ordinary data changed: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(protected); err != nil || string(got) != "AAAAAAAA" {
		t.Fatalf("foreign nested branch payload changed: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(controlPath); err != nil || !bytes.Equal(got, controlBefore) {
		t.Fatalf("foreign nested branch control changed: %q err=%v", got, err)
	}
}

// TestSymlinkedCacheRootStillRefusesLinkedHubDeleteRoot proves that the
// escape guard survives the canonical comparison: a Hub directory that is
// itself a link must not redefine the trusted deletion root, not even when
// the cache root is reached through a linked parent.
func TestSymlinkedCacheRootStillRefusesLinkedHubDeleteRoot(t *testing.T) {
	base, linked := symlinkedTestParent(t)
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "models--owner--model"), 0o755); err != nil {
		t.Fatal(err)
	}
	hubLink := filepath.Join(linked, "linked-hub")
	if err := os.Symlink(outside, hubLink); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	t.Setenv("HF_HUB_CACHE", hubLink)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(linked, "cache")})
	response := cacheRequest(t, s, http.MethodDelete, "/api/cache/owner/model?type=model", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("linked Hub redefined the trusted delete root: status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(outside, "models--owner--model")); err != nil {
		t.Fatalf("delete followed the linked Hub: %v", err)
	}
}

// TestSymlinkedCacheRootDeletesFriendlyView proves that an ordinary delete
// behind a linked parent removes the friendly view as well. The friendly path
// is compared against the cache directory, and a resolved friendly path that
// only differs in spelling from the selected cache directory is inside it.
func TestSymlinkedCacheRootDeletesFriendlyView(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	_, linked := symlinkedTestParent(t)
	cacheRoot := filepath.Join(linked, "cache")
	cache := hfdownloader.NewHFCache(cacheRoot, 0)
	repo, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := repo.WriteRef("main", "owner-commit"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repo.FriendlyPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(repo.FriendlyPath(), "weights.safetensors")
	if err := os.WriteFile(marker, []byte("friendly"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestServerWithConfig(t, Config{CacheDir: cacheRoot})
	response := cacheRequest(t, s, http.MethodDelete, "/api/cache/owner/model?type=model", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"success":true`) {
		t.Fatalf("delete behind a linked parent status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Lstat(repo.Path()); !os.IsNotExist(err) {
		t.Fatalf("Hub repository remains after the delete: %v", err)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("friendly view remains after the delete: %v", err)
	}
}
