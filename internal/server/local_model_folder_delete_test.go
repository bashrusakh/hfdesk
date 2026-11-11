// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

// Whole-model folder deletion through the details-modal button: the optional
// source/path body on DELETE /api/cache/{owner}/{repo}. These tests pin the
// acceptance case (a local model with no GGUF files at all), every safety
// layer of the local and friendly branches, the busy/refusal families, and the
// friendly routing between the established Hub+friendly pair and the
// friendly-only tree. The legacy no-body flow is pinned by the unchanged
// TestAPI_CacheDelete_* and cache_delete_active_writer tests.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustBeGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed, lstat err=%v", path, err)
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("expected %s to exist, err=%v", path, err)
	}
}

func localFolderDeleteBody(folder string) string {
	return fmt.Sprintf(`{"source":"Local","path":%q}`, folder)
}

func friendlyFolderDeleteBody(folder string) string {
	return fmt.Sprintf(`{"source":"Friendly view","path":%q}`, folder)
}

func requireDeleteSuccess(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp["success"] != true {
		t.Fatalf("expected success response, got %s", w.Body.String())
	}
}

// The acceptance pin (issue (a)): a local model with NO GGUF files at all —
// safetensors shards plus config — is listed with source "Local", deletes
// completely through the button flow including nested directories, and never
// touches siblings or symlink targets outside the folder.
func TestLocalModelFolderDelete_RemovesWholeNonGGUFFolder(t *testing.T) {
	scanRoot := t.TempDir()
	folder := filepath.Join(scanRoot, "org", "model")
	mustWriteFile(t, filepath.Join(folder, "config.json"), "{}")
	mustWriteFile(t, filepath.Join(folder, "model-00001-of-00002.safetensors"), "shard-one")
	mustWriteFile(t, filepath.Join(folder, "model-00002-of-00002.safetensors"), "shard-two")
	mustWriteFile(t, filepath.Join(folder, "nested", "deep", "extra.txt"), "deep")
	mustWriteFile(t, filepath.Join(folder, "README.md"), "notes")
	sibling := filepath.Join(scanRoot, "org", "sibling")
	mustWriteFile(t, filepath.Join(sibling, "keep.txt"), "keep")

	outsideDir := t.TempDir()
	outsideTarget := filepath.Join(outsideDir, "target.txt")
	mustWriteFile(t, outsideTarget, "outside")
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outsideTarget, filepath.Join(folder, "link-to-outside")); err != nil {
			t.Fatal(err)
		}
	}

	s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir(), LocalScanDirs: []string{scanRoot}})

	// The routing key itself: details must report this folder as source Local.
	w := cacheRequest(t, s, http.MethodGet, "/api/cache/org/model?type=model", "")
	var details CachedRepoInfo
	if err := json.Unmarshal(w.Body.Bytes(), &details); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || details.Source != cacheSourceLocal || details.Path != folder {
		t.Fatalf("local details routing: status=%d source=%q path=%q body=%s", w.Code, details.Source, details.Path, w.Body.String())
	}

	w = cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", localFolderDeleteBody(folder))
	if w.Code != http.StatusOK {
		t.Fatalf("non-GGUF local folder delete: status=%d body=%s", w.Code, w.Body.String())
	}
	requireDeleteSuccess(t, w)
	mustBeGone(t, folder)
	mustExist(t, filepath.Join(sibling, "keep.txt"))
	if runtime.GOOS != "windows" {
		mustExist(t, outsideTarget)
	}
}

// Configured-boundary protection refuses fail-closed in both directions the
// established selected-delete semantics project: a scan-root nested inside the
// folder (whole-folder deletion would take that other registered root with
// it), and a separately configured root owning the folder from above.
func TestLocalModelFolderDelete_RefusesConfiguredBoundaries(t *testing.T) {
	scanRoot := t.TempDir()
	folder := filepath.Join(scanRoot, "org", "model")
	mustWriteFile(t, filepath.Join(folder, "weights.safetensors"), "w")
	innerRoot := filepath.Join(folder, "inner")
	mustWriteFile(t, filepath.Join(innerRoot, "x", "keep.gguf"), "keep")
	cases := []struct {
		name     string
		scanDirs []string
		want     string
	}{
		{"descendant root nested inside the folder", []string{scanRoot, innerRoot}, "nested inside this model folder"},
		{"configured root owning the folder", []string{scanRoot, filepath.Join(scanRoot, "org")}, "configured descendant boundary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir(), LocalScanDirs: tc.scanDirs})
			w := cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", localFolderDeleteBody(folder))
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("boundary must refuse with 409: status=%d body=%s, want %q", w.Code, w.Body.String(), tc.want)
			}
			mustExist(t, filepath.Join(folder, "weights.safetensors"))
			mustExist(t, filepath.Join(innerRoot, "x", "keep.gguf"))
		})
	}
}

// Scope and path validation: outside-root, traversal, wrong-source (friendly
// namespace claimed as Local), missing folder, malformed body, dataset scope,
// unknown source and incomplete discriminators all refuse without deleting
// anything.
func TestLocalModelFolderDelete_PathAndScopeRefusals(t *testing.T) {
	scanRoot := t.TempDir()
	folder := filepath.Join(scanRoot, "org", "model")
	mustWriteFile(t, filepath.Join(folder, "weights.safetensors"), "w")
	cacheDir := t.TempDir()
	outside := t.TempDir()
	s := newTestServerWithConfig(t, Config{CacheDir: cacheDir, LocalScanDirs: []string{scanRoot}})

	cases := []struct {
		name       string
		repo       string
		query      string
		body       string
		wantCode   int
		wantDetail string
	}{
		{"unknown source", "", "?type=model", fmt.Sprintf(`{"source":"External","path":%q}`, folder), http.StatusBadRequest, "Invalid deletion scope"},
		{"missing path", "", "?type=model", `{"source":"Local"}`, http.StatusBadRequest, "source and path are required together"},
		{"stray path without source", "", "?type=model", fmt.Sprintf(`{"path":%q}`, folder), http.StatusBadRequest, "source and path are required together"},
		{"dataset scope", "", "?type=dataset", localFolderDeleteBody(folder), http.StatusBadRequest, "Whole-folder deletion is only available for models"},
		{"outside-root path", "", "?type=model", localFolderDeleteBody(filepath.Join(outside, "org", "model")), http.StatusBadRequest, "Path is not a configured local model folder"},
		{"traversal path", "", "?type=model", localFolderDeleteBody(filepath.Join(scanRoot, "org", "model", "..", "..", "..", "etc")), http.StatusBadRequest, "Path is not a configured local model folder"},
		{"friendly namespace claimed as Local", "", "?type=model", localFolderDeleteBody(filepath.Join(cacheDir, "models", "org", "model")), http.StatusBadRequest, "Path is not a configured local model folder"},
		{"malformed body", "", "?type=model", `{oops`, http.StatusBadRequest, "Invalid request body"},
		// The folder name comes from the repo id, never from the client path:
		// a matching template whose folder is absent reaches the anchored
		// opener and refuses through the established unsafe family.
		{"missing folder", "org/absent", "?type=model", localFolderDeleteBody(filepath.Join(scanRoot, "org", "absent")), http.StatusConflict, "Selection is unsafe"},
		// A body without the discriminator behaves exactly like no body: the
		// legacy Hub-only flow runs and the local folder is untouched.
		{"empty object body is legacy", "", "?type=model", "{}", http.StatusNotFound, "Repository not found in cache"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := tc.repo
			if repo == "" {
				repo = "org/model"
			}
			w := cacheRequest(t, s, http.MethodDelete, "/api/cache/"+repo+tc.query, tc.body)
			if w.Code != tc.wantCode || !strings.Contains(w.Body.String(), tc.wantDetail) {
				t.Fatalf("status=%d body=%s, want %d containing %q", w.Code, w.Body.String(), tc.wantCode, tc.wantDetail)
			}
			mustExist(t, filepath.Join(folder, "weights.safetensors"))
		})
	}
}

// Owner/model symlink components are refused before any removal, and neither
// the link nor its target is touched.
func TestLocalModelFolderDelete_RefusesSymlinkComponents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture")
	}
	t.Run("owner is a symlink", func(t *testing.T) {
		scanRoot := t.TempDir()
		elsewhere := t.TempDir()
		realModel := filepath.Join(elsewhere, "org", "model")
		mustWriteFile(t, filepath.Join(realModel, "weights.safetensors"), "w")
		if err := os.Symlink(filepath.Join(elsewhere, "org"), filepath.Join(scanRoot, "org")); err != nil {
			t.Fatal(err)
		}
		s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir(), LocalScanDirs: []string{scanRoot}})
		folder := filepath.Join(scanRoot, "org", "model")
		w := cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", localFolderDeleteBody(folder))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Selection is unsafe") {
			t.Fatalf("symlinked owner must refuse with 409 unsafe: status=%d body=%s", w.Code, w.Body.String())
		}
		mustExist(t, filepath.Join(realModel, "weights.safetensors"))
		mustExist(t, filepath.Join(scanRoot, "org"))
	})
	t.Run("model is a symlink", func(t *testing.T) {
		scanRoot := t.TempDir()
		elsewhere := t.TempDir()
		realModel := filepath.Join(elsewhere, "realm")
		mustWriteFile(t, filepath.Join(realModel, "weights.safetensors"), "w")
		mustMkdirAll(t, filepath.Join(scanRoot, "org"))
		if err := os.Symlink(realModel, filepath.Join(scanRoot, "org", "model")); err != nil {
			t.Fatal(err)
		}
		s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir(), LocalScanDirs: []string{scanRoot}})
		folder := filepath.Join(scanRoot, "org", "model")
		w := cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", localFolderDeleteBody(folder))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Selection is unsafe") {
			t.Fatalf("symlinked model folder must refuse with 409 unsafe: status=%d body=%s", w.Code, w.Body.String())
		}
		mustExist(t, filepath.Join(realModel, "weights.safetensors"))
		mustExist(t, filepath.Join(scanRoot, "org", "model"))
	})
}

// A live in-process writer owning this repository's destination refuses the
// deletion with the established 409 busy family (the same writer-conflict the
// whole-repository endpoint's window is proven against); after the writer
// finishes, the same request succeeds.
func TestLocalModelFolderDelete_BusyLiveWriter(t *testing.T) {
	scanRoot := t.TempDir()
	folder := filepath.Join(scanRoot, "org", "model")
	mustWriteFile(t, filepath.Join(folder, "weights.safetensors"), "w")
	s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir(), LocalScanDirs: []string{scanRoot}})

	// An ordinary flat/local-mode download into this root writes exactly
	// root/owner/name — the job class that owns local model folders.
	writer := &Job{ID: "writer", Repo: "org/model", OutputDir: scanRoot, Flat: true, Status: JobStatusRunning}
	s.jobs.runActivities[1] = &jobWriteActivity{job: writer, planned: map[string]frozenMutationPath{}}
	w := cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", localFolderDeleteBody(folder))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Repository is busy") {
		t.Fatalf("live writer must refuse with 409 busy: status=%d body=%s", w.Code, w.Body.String())
	}
	mustExist(t, filepath.Join(folder, "weights.safetensors"))
	delete(s.jobs.runActivities, 1)

	w = cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", localFolderDeleteBody(folder))
	if w.Code != http.StatusOK {
		t.Fatalf("delete after writer finished: status=%d body=%s", w.Code, w.Body.String())
	}
	mustBeGone(t, folder)
}

// A cross-process flat-realm writer holding one of the folder's files refuses
// the deletion through the writeCacheDeleteAdmission 409 family; releasing the
// lease lets the same request succeed.
// Cross-process admission pins the whole-folder delete against flat writers in
// this or another process: a live lease on an existing file, and — the F1
// created-in-window race — a live lease on a file inside the folder that does
// not exist yet, which an entry-snapshot admission over enumerated files
// cannot exclude (that scenario fails against such a handler).
func TestLocalModelFolderDelete_BusyCrossProcessAdmission(t *testing.T) {
	scanRoot := t.TempDir()
	folder := filepath.Join(scanRoot, "org", "model")
	file := filepath.Join(folder, "weights.safetensors")
	mustWriteFile(t, file, "w")
	s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir(), LocalScanDirs: []string{scanRoot}})
	ctx := context.Background()
	admission, err := hfdownloader.OpenFlatAdmission(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	expectBusy := func(scenario string) {
		t.Helper()
		w := cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", localFolderDeleteBody(folder))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Repository is busy") {
			t.Fatalf("%s: live flat writer must refuse with 409 busy: status=%d body=%s", scenario, w.Code, w.Body.String())
		}
		mustExist(t, file)
		mustExist(t, folder)
	}
	lease, err := admission.AdmitFileWrites(ctx, []string{file})
	if err != nil {
		t.Fatal(err)
	}
	expectBusy("existing-file lease")
	lease()
	missing := filepath.Join(folder, "weights.safetensors.incomplete")
	lease, err = admission.AdmitFileWrites(ctx, []string{missing})
	if err != nil {
		t.Fatalf("writer admission for a not-yet-created file: %v", err)
	}
	expectBusy("not-yet-created-file lease")
	lease()
	w := cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", localFolderDeleteBody(folder))
	if w.Code != http.StatusOK {
		t.Fatalf("delete after lease release: status=%d body=%s", w.Code, w.Body.String())
	}
	mustBeGone(t, folder)
}

// Friendly-only presence: details report source "Friendly view" (Hub absent)
// and the button flow removes exactly that tree while its parent stays.
func TestFriendlyViewDelete_RemovesFriendlyOnlyTree(t *testing.T) {
	cacheDir := t.TempDir()
	friendly := filepath.Join(cacheDir, "models", "org", "model")
	mustWriteFile(t, filepath.Join(friendly, "weights.safetensors"), "w")
	mustWriteFile(t, filepath.Join(friendly, "config.json"), "{}")
	mustWriteFile(t, filepath.Join(friendly, "sub", "extra.txt"), "x")
	s := newTestServerWithConfig(t, Config{CacheDir: cacheDir})

	w := cacheRequest(t, s, http.MethodGet, "/api/cache/org/model?type=model", "")
	var details CachedRepoInfo
	if err := json.Unmarshal(w.Body.Bytes(), &details); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || details.Source != cacheSourceFriendlyView || details.Path != friendly {
		t.Fatalf("friendly details routing: status=%d source=%q path=%q body=%s", w.Code, details.Source, details.Path, w.Body.String())
	}

	w = cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", friendlyFolderDeleteBody(friendly))
	if w.Code != http.StatusOK {
		t.Fatalf("friendly-only delete: status=%d body=%s", w.Code, w.Body.String())
	}
	requireDeleteSuccess(t, w)
	mustBeGone(t, friendly)
	mustExist(t, filepath.Join(cacheDir, "models", "org"))
}

// When the repository resolves as an HF cache item, the friendly discriminator
// uses the established endpoint semantics: the Hub repository and the friendly
// pair are both removed.
func TestFriendlyViewDelete_RemovesHubAndFriendlyPair(t *testing.T) {
	cacheDir := t.TempDir()
	s := newTestServerWithConfig(t, Config{CacheDir: cacheDir})
	rd, err := s.snapshotConfig().cache().Repo("org/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(rd.Path(), "blobs-stand-in.bin"), "hub")
	friendly := rd.FriendlyPath()
	mustWriteFile(t, filepath.Join(friendly, "weights.safetensors"), "w")

	w := cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", friendlyFolderDeleteBody(friendly))
	if w.Code != http.StatusOK {
		t.Fatalf("friendly-with-hub delete: status=%d body=%s", w.Code, w.Body.String())
	}
	requireDeleteSuccess(t, w)
	mustBeGone(t, rd.Path())
	mustBeGone(t, friendly)
}

// The friendly branch's refusal families: in-process window busy, cache-owned
// admission busy, path mismatch, missing folder, and both symlink escapes all
// refuse without removing anything outside.
func TestFriendlyViewDelete_Refusals(t *testing.T) {
	t.Run("in-process writer busy", func(t *testing.T) {
		cacheDir := t.TempDir()
		friendly := filepath.Join(cacheDir, "models", "org", "model")
		mustWriteFile(t, filepath.Join(friendly, "weights.safetensors"), "w")
		s := newTestServerWithConfig(t, Config{CacheDir: cacheDir})
		writer := &Job{ID: "writer", Repo: "org/model", OutputDir: cacheDir, Status: JobStatusRunning}
		s.jobs.runActivities[1] = &jobWriteActivity{job: writer, planned: map[string]frozenMutationPath{}}
		w := cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", friendlyFolderDeleteBody(friendly))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Repository is busy") {
			t.Fatalf("live writer must refuse with 409 busy: status=%d body=%s", w.Code, w.Body.String())
		}
		mustExist(t, filepath.Join(friendly, "weights.safetensors"))
	})

	t.Run("cache-owned admission busy", func(t *testing.T) {
		cacheDir := t.TempDir()
		friendly := filepath.Join(cacheDir, "models", "org", "model")
		mustWriteFile(t, filepath.Join(friendly, "weights.safetensors"), "w")
		s := newTestServerWithConfig(t, Config{CacheDir: cacheDir})
		admission, err := hfdownloader.OpenCacheAdmission(context.Background(), cacheDir)
		if err != nil {
			t.Fatal(err)
		}
		releaseTree, err := admission.AdmitTreeRemoval(context.Background(), friendly)
		if err != nil {
			t.Fatal(err)
		}
		w := cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", friendlyFolderDeleteBody(friendly))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Repository is busy") {
			t.Fatalf("live cache writer must refuse with 409 busy: status=%d body=%s", w.Code, w.Body.String())
		}
		mustExist(t, filepath.Join(friendly, "weights.safetensors"))
		releaseTree()
	})

	t.Run("path mismatch", func(t *testing.T) {
		cacheDir := t.TempDir()
		friendly := filepath.Join(cacheDir, "models", "org", "model")
		mustWriteFile(t, filepath.Join(friendly, "weights.safetensors"), "w")
		s := newTestServerWithConfig(t, Config{CacheDir: cacheDir})
		w := cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", friendlyFolderDeleteBody(filepath.Join(cacheDir, "models", "org", "other")))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "does not match this model's friendly view folder") {
			t.Fatalf("mismatched path must refuse with 400: status=%d body=%s", w.Code, w.Body.String())
		}
		mustExist(t, friendly)
	})

	t.Run("missing folder", func(t *testing.T) {
		s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})
		w := cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", friendlyFolderDeleteBody(filepath.Join(s.snapshotConfig().cacheRoot(), "models", "org", "model")))
		if w.Code != http.StatusNotFound {
			t.Fatalf("missing friendly folder must 404: status=%d body=%s", w.Code, w.Body.String())
		}
	})

	if runtime.GOOS != "windows" {
		t.Run("owner symlink escape", func(t *testing.T) {
			cacheDir := t.TempDir()
			outside := t.TempDir()
			mustWriteFile(t, filepath.Join(outside, "model", "weights.safetensors"), "outside")
			mustMkdirAll(t, filepath.Join(cacheDir, "models"))
			if err := os.Symlink(outside, filepath.Join(cacheDir, "models", "org")); err != nil {
				t.Fatal(err)
			}
			s := newTestServerWithConfig(t, Config{CacheDir: cacheDir})
			w := cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", friendlyFolderDeleteBody(filepath.Join(cacheDir, "models", "org", "model")))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Resolved path outside cache directory") {
				t.Fatalf("escaping owner symlink must refuse with 400: status=%d body=%s", w.Code, w.Body.String())
			}
			mustExist(t, filepath.Join(outside, "model", "weights.safetensors"))
			mustExist(t, filepath.Join(cacheDir, "models", "org"))
		})

		t.Run("model folder is a symlink", func(t *testing.T) {
			cacheDir := t.TempDir()
			outside := t.TempDir()
			mustWriteFile(t, filepath.Join(outside, "weights.safetensors"), "outside")
			mustMkdirAll(t, filepath.Join(cacheDir, "models", "org"))
			if err := os.Symlink(outside, filepath.Join(cacheDir, "models", "org", "model")); err != nil {
				t.Fatal(err)
			}
			s := newTestServerWithConfig(t, Config{CacheDir: cacheDir})
			w := cacheRequest(t, s, http.MethodDelete, "/api/cache/org/model?type=model", friendlyFolderDeleteBody(filepath.Join(cacheDir, "models", "org", "model")))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Cannot delete symlinked directories") {
				t.Fatalf("symlinked friendly folder must refuse with 400: status=%d body=%s", w.Code, w.Body.String())
			}
			mustExist(t, filepath.Join(outside, "weights.safetensors"))
			mustExist(t, filepath.Join(cacheDir, "models", "org", "model"))
		})
	}
}

// A flat writer admitted for a path UNDER the folder that does not exist yet
// (the F1 created-in-window scenario, e.g. a transfer's staging pair) makes the
// whole-folder delete answer 409 and remove NOTHING — pinned as the second
// scenario of TestLocalModelFolderDelete_BusyCrossProcessAdmission above.
// Unit pin for FlatAdmission.AdmitTreeRemoval's two documented rules, placed
// in this file because the correction's file scope excludes new pkg test
// files: refuse on a live writer under the tree, and the re-verified-vanish
// release excludes a creator of the gone target until released.
func TestFlatAdmissionTreeRemoval_WriterAndVanishRules(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	tree := filepath.Join(base, "owner", "model")
	mustWriteFile(t, filepath.Join(tree, "w.bin"), "w")
	admission, err := hfdownloader.OpenFlatAdmission(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := admission.AdmitFileWrites(ctx, []string{filepath.Join(tree, "w.bin")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admission.AdmitTreeRemoval(ctx, tree); !errors.Is(err, hfdownloader.ErrOwnershipBusy) {
		t.Fatalf("tree removal over a live writer must refuse busy, got %v", err)
	}
	lease()
	gone := filepath.Join(base, "never-created")
	release, err := admission.AdmitTreeRemoval(ctx, gone)
	if err != nil {
		t.Fatalf("vanished target must be admitted by re-verified vanish: %v", err)
	}
	if _, err := admission.AdmitFileWrites(ctx, []string{gone}); !errors.Is(err, hfdownloader.ErrOwnershipBusy) {
		t.Fatalf("a creator racing the admitted vanish must be refused busy, got %v", err)
	}
	release()
	creatorLease, err := admission.AdmitFileWrites(ctx, []string{gone})
	if err != nil {
		t.Fatalf("creator admission after release: %v", err)
	}
	creatorLease()
}
