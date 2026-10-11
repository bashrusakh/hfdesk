// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

type cacheSelectionResponse struct {
	Repo      string                   `json:"repo"`
	Type      string                   `json:"type"`
	Locations []cacheSelectionLocation `json:"locations"`
}

type testSelectedDeleteRequest struct {
	Repo       string                 `json:"repo"`
	Type       string                 `json:"type"`
	LocationID string                 `json:"locationId"`
	GroupID    string                 `json:"groupId"`
	Members    []cacheSelectionMember `json:"members"`
}

func getSelection(t *testing.T, s *Server, repo, repoType, location string) (int, cacheSelectionResponse) {
	t.Helper()
	path := "/api/cache-selection?repo=" + url.QueryEscape(repo) + "&type=" + url.QueryEscape(repoType)
	if location != "" {
		path += "&locationId=" + url.QueryEscape(location)
	}
	w := cacheRequest(t, s, "GET", path, "")
	var result cacheSelectionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode selection: %v: %s", err, w.Body.String())
	}
	return w.Code, result
}

func writeSelectionFile(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("gguf"), 0644); err != nil {
		t.Fatal(err)
	}
}

func requireReplacementFixtureOperation(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if runtime.GOOS == "windows" {
		t.Skipf("Windows prevented replacing an entry while its identity handle was held: %v", err)
	}
	t.Fatal(err)
}

func registerJobManagerCleanup(t *testing.T, s *Server) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.jobs.Close(ctx); err != nil {
			t.Errorf("close test job manager: %v", err)
		}
	})
}

func assertNoTemporaryPinContainer(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hfdesk-delete-pin-") {
			t.Fatalf("temporary pin container remains in %s: %s", dir, entry.Name())
		}
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" && os.IsPermission(err) {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatalf("create symlink: %v", err)
	}
}

func TestCacheSelectionLocalGroupsAndExplicitLocation(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	for _, q := range []string{"Q2_K", "Q3_K_M", "Q4_K_M", "Q5_K_M", "Q6_K", "Q8_0"} {
		writeSelectionFile(t, repoDir, "model-"+q+".gguf")
	}
	writeSelectionFile(t, repoDir, "alt-Q4_K_M.gguf")
	writeSelectionFile(t, repoDir, "model-MTP-Q4_K_M.gguf")
	writeSelectionFile(t, repoDir, "parts/model-Q4_K_M-00001-of-00002.gguf")
	writeSelectionFile(t, repoDir, "parts/model-Q4_K_M-00002-of-00002.gguf")
	writeSelectionFile(t, repoDir, "plain/model.gguf")
	for _, name := range []string{"model-Q4_K_M.gguf", "model_Q4_K_M.gguf", "model-F16.gguf", "model-F32.gguf", "model.gguf", "model.GGUF", "split-X-00001-of-00002.gguf", "split-X-00002-of-00003.gguf", "split-X_00001_of_00002.gguf"} {
		writeSelectionFile(t, repoDir, name)
	}
	writeSelectionFile(t, repoDir, "broken-Q4_K_M-00001-of-00002.gguf")
	writeSelectionFile(t, repoDir, "invalid-00000-of-00000.gguf")
	brokenLinkAdded := os.Symlink(filepath.Join(repoDir, "missing-target"), filepath.Join(repoDir, "missing-Q4_K_M.gguf")) == nil
	for i := 0; i < 25; i++ {
		writeSelectionFile(t, repoDir, fmt.Sprintf("many/extra-%02d.gguf", i))
	}
	otherRepo := filepath.Join(other, "owner", "model")
	writeSelectionFile(t, otherRepo, "model-Q4_K_M.gguf")
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root, other}})
	code, got := getSelection(t, s, "owner/model", "model", "")
	if code != 200 || len(got.Locations) != 2 {
		t.Fatalf("selection=%d %+v", code, got)
	}
	if got.Type != "model" {
		t.Fatalf("type=%q", got.Type)
	}
	loc := got.Locations[0]
	if !strings.Contains(loc.Path, root) {
		loc = got.Locations[1]
	}
	if len(loc.Groups) < 10 {
		t.Fatalf("groups omitted files: got %d", len(loc.Groups))
	}
	if loc.Warning == "" || loc.CanDelete {
		t.Fatalf("incompletely enumerated LocalScanDirs location must not advertise deletion: %+v", loc)
	}
	var completeLocation cacheSelectionLocation
	for _, candidate := range got.Locations {
		if strings.Contains(candidate.Path, other) {
			completeLocation = candidate
		}
	}
	if completeLocation.ID == "" || completeLocation.Warning != "" || !completeLocation.CanDelete {
		t.Fatalf("fully enumerated configured LocalScanDirs root did not advertise deletion: %+v", completeLocation)
	}
	q4Count, manyCount, splitWarning, invalidWarning := 0, 0, false, false
	paths := map[string]int{}
	groupForPath := map[string]string{}
	for _, g := range loc.Groups {
		if g.ID == "" || len(g.Members) == 0 {
			t.Errorf("invalid group: %+v", g)
		}
		if g.Quant == "Q4_K_M" {
			q4Count++
		}
		if strings.HasPrefix(g.Label, "many/") {
			manyCount++
		}
		if strings.Contains(g.Warning, "incomplete") {
			splitWarning = true
		}
		if strings.Contains(g.Label, "invalid") && g.Warning != "" {
			invalidWarning = true
		}
		for _, m := range g.Members {
			paths[m.Path]++
			groupForPath[m.Path] = g.ID
		}
	}
	for _, name := range []string{"model-Q4_K_M.gguf", "model_Q4_K_M.gguf", "model-F16.gguf", "model-F32.gguf", "model.gguf", "model.GGUF", "split-X-00001-of-00002.gguf", "split-X-00002-of-00003.gguf", "split-X_00001_of_00002.gguf"} {
		if paths[name] != 1 {
			t.Errorf("%s appears in %d groups", name, paths[name])
		}
	}
	for _, pair := range [][2]string{{"model-Q4_K_M.gguf", "model_Q4_K_M.gguf"}, {"model-F16.gguf", "model-F32.gguf"}, {"model.gguf", "model.GGUF"}, {"split-X-00001-of-00002.gguf", "split-X-00002-of-00003.gguf"}, {"split-X-00001-of-00002.gguf", "split-X_00001_of_00002.gguf"}} {
		if groupForPath[pair[0]] == groupForPath[pair[1]] {
			t.Errorf("distinct groups merged: %s and %s", pair[0], pair[1])
		}
	}
	if q4Count < 4 {
		t.Errorf("same-quant distinct families were merged: got %d Q4 groups", q4Count)
	}
	if manyCount != 25 {
		t.Errorf("full member inventory truncated: found %d/25 groups", manyCount)
	}
	if !splitWarning {
		t.Error("incomplete numbered shard set did not produce a warning")
	}
	if !invalidWarning {
		t.Error("invalid numbered shard indices/totals did not produce a warning")
	}
	if brokenLinkAdded && loc.Warning == "" {
		t.Error("unavailable local GGUF link did not produce a location warning")
	}
	code, selected := getSelection(t, s, "owner/model", "model", loc.ID)
	if code != 200 || len(selected.Locations) != 1 || selected.Locations[0].ID != loc.ID {
		t.Fatalf("explicit location retargeted: %d %+v", code, selected)
	}
	if code, _ := getSelection(t, s, "owner/model", "model", "local-not-current"); code != 400 {
		t.Fatalf("stale selector status=%d", code)
	}
	if code, _ := getSelection(t, s, "owner/model", "dataset", ""); code != 400 {
		t.Fatalf("dataset selector status=%d", code)
	}
}

func TestSelectedDeleteRefusesOwnerSymlinkEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	outsideRepo := filepath.Join(outside, "model")
	writeSelectionFile(t, outsideRepo, "model-Q4_K_M.gguf")
	if err := os.Symlink(outside, filepath.Join(root, "owner")); err != nil {
		if runtime.GOOS == "windows" && os.IsPermission(err) {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK || len(selection.Locations) != 1 {
		t.Fatalf("selection status=%d locations=%+v", code, selection.Locations)
	}
	loc := selection.Locations[0]
	if loc.CanDelete || len(loc.Groups) == 0 {
		t.Fatalf("escaped owner must be visible but not deletable: %+v", loc)
	}
	group := loc.Groups[0]
	body, err := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: loc.ID, GroupID: group.ID, Members: group.Members})
	if err != nil {
		t.Fatal(err)
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete through owner symlink status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(outsideRepo, "model-Q4_K_M.gguf")); err != nil {
		t.Fatalf("outside file was changed: %v", err)
	}
}

func TestSelectedDeleteAllowsConfiguredRootAlias(t *testing.T) {
	actual, alias := t.TempDir(), filepath.Join(t.TempDir(), "registered-root")
	writeSelectionFile(t, filepath.Join(actual, "owner", "model"), "model-Q4_K_M.gguf")
	if err := os.Symlink(actual, alias); err != nil {
		if runtime.GOOS == "windows" && os.IsPermission(err) {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{alias}})
	_, selection := getSelection(t, s, "owner/model", "model", "")
	if len(selection.Locations) != 1 || !selection.Locations[0].CanDelete {
		t.Fatalf("explicitly configured root alias was not admitted: %+v", selection.Locations)
	}
	loc, group := selection.Locations[0], selection.Locations[0].Groups[0]
	body, err := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: loc.ID, GroupID: group.ID, Members: group.Members})
	if err != nil {
		t.Fatal(err)
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("delete through configured root alias status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(actual, "owner", "model", "model-Q4_K_M.gguf")); !os.IsNotExist(err) {
		t.Fatalf("selected entry remains after authorized root-alias delete: %v", err)
	}
}

func TestSelectedDeleteProtectsAliasedConfiguredDescendant(t *testing.T) {
	outer := t.TempDir()
	child := filepath.Join(outer, "owner", "model", "library")
	writeSelectionFile(t, filepath.Join(outer, "owner", "model"), "outer-Q4_K_M.gguf")
	writeSelectionFile(t, filepath.Join(child, "owner", "model"), "vendor-Q4_K_M.gguf")
	alias := filepath.Join(t.TempDir(), "registered-library")
	symlinkOrSkip(t, child, alias)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{outer, alias}})
	_, selection := getSelection(t, s, "owner/model", "model", "")
	var outerLocation, childLocation *cacheSelectionLocation
	for i := range selection.Locations {
		loc := &selection.Locations[i]
		if pathIdentityKey(filepath.Clean(loc.Path)) == pathIdentityKey(filepath.Join(outer, "owner", "model")) {
			outerLocation = loc
		}
		if loc.Source == "Local" && strings.HasPrefix(loc.Path, alias) {
			childLocation = loc
		}
	}
	if outerLocation == nil {
		t.Fatalf("outer selection missing: %+v", selection.Locations)
	}
	for _, group := range outerLocation.Groups {
		for _, member := range group.Members {
			if strings.HasPrefix(member.Path, "library/") {
				t.Fatalf("outer location claimed separately registered descendant file: %+v", member)
			}
		}
	}
	if childLocation == nil || !childLocation.CanDelete {
		t.Fatalf("separately registered alias was not independently selectable: %+v", selection.Locations)
	}
	outerGroup := outerLocation.Groups[0]
	outerRequest, err := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: outerLocation.ID, GroupID: outerGroup.ID, Members: outerGroup.Members})
	if err != nil {
		t.Fatal(err)
	}
	if w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(outerRequest)); w.Code != http.StatusOK {
		t.Fatalf("independent outer group delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(child, "owner", "model", "vendor-Q4_K_M.gguf")); err != nil {
		t.Fatalf("outer deletion crossed the separately registered descendant boundary: %v", err)
	}
}

func TestSelectedDeleteProjectsDescendantBoundaryThroughOuterAlias(t *testing.T) {
	realOuter := t.TempDir()
	outerAlias := filepath.Join(t.TempDir(), "outer-alias")
	symlinkOrSkip(t, realOuter, outerAlias)
	realChild := filepath.Join(realOuter, "owner", "model", "library")
	writeSelectionFile(t, filepath.Join(realOuter, "owner", "model"), "outer-Q4_K_M.gguf")
	writeSelectionFile(t, filepath.Join(realChild, "owner", "model"), "child-Q4_K_M.gguf")
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{outerAlias, realChild}})
	_, selection := getSelection(t, s, "owner/model", "model", "")
	var outerLocation *cacheSelectionLocation
	for i := range selection.Locations {
		if pathIdentityKey(filepath.Clean(selection.Locations[i].Path)) == pathIdentityKey(filepath.Join(outerAlias, "owner", "model")) {
			outerLocation = &selection.Locations[i]
		}
	}
	if outerLocation == nil {
		t.Fatalf("outer-alias selection missing: %+v", selection.Locations)
	}
	for _, group := range outerLocation.Groups {
		for _, member := range group.Members {
			if strings.HasPrefix(member.Path, "library/") {
				t.Fatalf("outer alias exposed physical descendant file in walker namespace: %+v", member)
			}
		}
	}
	for _, group := range outerLocation.Groups {
		if group.Members[0].Path == "outer-Q4_K_M.gguf" {
			body, err := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: outerLocation.ID, GroupID: group.ID, Members: group.Members})
			if err != nil {
				t.Fatal(err)
			}
			if w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body)); w.Code != http.StatusOK {
				t.Fatalf("outer alias selected delete status=%d body=%s", w.Code, w.Body.String())
			}
			if _, err := os.Stat(filepath.Join(realChild, "owner", "model", "child-Q4_K_M.gguf")); err != nil {
				t.Fatalf("outer alias deletion crossed real descendant root: %v", err)
			}
			return
		}
	}
	t.Fatal("outer Q4 group missing")
}

func TestSelectedDeleteDeniedWhenConfiguredDescendantCannotResolve(t *testing.T) {
	const fixtureEnv = "HFDESK_UNRESOLVED_ROOT_FIXTURE"
	if fixtureRoot := os.Getenv(fixtureEnv); fixtureRoot != "" {
		if os.Geteuid() == 0 {
			t.Fatal("permission regression helper unexpectedly retained root privileges")
		}
		outer := filepath.Join(fixtureRoot, "outer")
		alias := filepath.Join(fixtureRoot, "private", "child-alias")
		cache := filepath.Join(fixtureRoot, "cache")
		selected := filepath.Join(outer, "owner", "model", "library", "owner", "model", "model-Q4_K_M.gguf")
		s := newTestServerWithConfig(t, Config{CacheDir: cache, LocalScanDirs: []string{outer, alias}})
		_, selection := getSelection(t, s, "owner/model", "model", "")
		var outerLocation *cacheSelectionLocation
		for i := range selection.Locations {
			loc := &selection.Locations[i]
			if pathIdentityKey(filepath.Clean(loc.Path)) == pathIdentityKey(filepath.Join(outer, "owner", "model")) {
				outerLocation = loc
			}
		}
		if outerLocation == nil || outerLocation.CanDelete || outerLocation.Warning == "" {
			t.Fatalf("unresolved registered descendant did not make outer deletion unavailable: %+v", outerLocation)
		}
		body := selectedGroupRequest(t, s, "owner/model", outer, "outer-Q4_K_M.gguf")
		if w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body)); w.Code == http.StatusOK {
			t.Fatalf("delete accepted despite unresolved configured descendant: %s", w.Body.String())
		}
		if _, err := os.Stat(selected); err != nil {
			t.Fatalf("protected child file changed despite unresolved boundary: %v", err)
		}
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("permission-denied symlink resolution regression requires Linux credentials")
	}
	fixtureRoot := t.TempDir()
	if err := os.Chmod(fixtureRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	outer := filepath.Join(fixtureRoot, "outer")
	child := filepath.Join(outer, "owner", "model", "library")
	writeSelectionFile(t, filepath.Join(outer, "owner", "model"), "outer-Q4_K_M.gguf")
	writeSelectionFile(t, filepath.Join(child, "owner", "model"), "model-Q4_K_M.gguf")
	private := filepath.Join(fixtureRoot, "private")
	if err := os.Mkdir(private, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(child, filepath.Join(private, "child-alias")); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(fixtureRoot, "cache")
	if err := os.Mkdir(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		if err := filepath.Walk(fixtureRoot, func(name string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil
			}
			return os.Chown(name, 65534, 65534)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(private, 0); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(private, 0o700); err != nil {
			t.Errorf("restore permission for fixture cleanup: %v", err)
		}
	}()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmdName := executable
	cmdArgs := []string{"-test.run=^TestSelectedDeleteDeniedWhenConfiguredDescendantCannotResolve$"}
	if os.Geteuid() == 0 {
		runuser, err := exec.LookPath("runuser")
		if err != nil {
			t.Fatal("cannot exercise permission-denied resolution as an unprivileged identity: runuser unavailable")
		}
		data, err := os.ReadFile(executable)
		if err != nil {
			t.Fatal(err)
		}
		helper := filepath.Join(fixtureRoot, "server.test")
		if err := os.WriteFile(helper, data, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(helper, 0, 0); err != nil {
			t.Fatal(err)
		}
		cmdName = runuser
		cmdArgs = []string{"-u", "nobody", "--", helper, "-test.run=^TestSelectedDeleteDeniedWhenConfiguredDescendantCannotResolve$"}
	}
	cmd := exec.Command(cmdName, cmdArgs...)
	cmd.Env = append(os.Environ(), fixtureEnv+"="+fixtureRoot)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unprivileged permission regression helper failed: %v\n%s", err, output)
	}
}

func TestSelectedHFDeleteRefusesUnresolvedConfiguredDescendant(t *testing.T) {
	const fixtureEnv = "HFDESK_UNRESOLVED_HF_ROOT_FIXTURE"
	if fixtureRoot := os.Getenv(fixtureEnv); fixtureRoot != "" {
		if os.Geteuid() == 0 {
			t.Fatal("permission regression helper unexpectedly retained root privileges")
		}
		storage := filepath.Join(fixtureRoot, "cache")
		alias := filepath.Join(fixtureRoot, "private", "snapshot-library")
		rd, err := hfdownloader.NewHFCache(storage, 0).Repo("owner/model", hfdownloader.RepoTypeModel)
		if err != nil {
			t.Fatal(err)
		}
		snapshotEntry, err := rd.SnapshotPath("saved-version", "library/vendor/child/vendor-Q4_K_M.gguf")
		if err != nil {
			t.Fatal(err)
		}
		blob := filepath.Join(rd.BlobsDir(), strings.Repeat("a", 64))
		s := newTestServerWithConfig(t, Config{CacheDir: storage, LocalScanDirs: []string{alias}})
		_, selection := getSelection(t, s, "owner/model", "model", "")
		var hfLocation *cacheSelectionLocation
		var group *cacheSelectionGroup
		for i := range selection.Locations {
			loc := &selection.Locations[i]
			if !strings.HasPrefix(loc.ID, "hf-") {
				continue
			}
			hfLocation = loc
			for j := range loc.Groups {
				for _, member := range loc.Groups[j].Members {
					if member.Path == "library/vendor/child/vendor-Q4_K_M.gguf" {
						group = &loc.Groups[j]
					}
				}
			}
		}
		if hfLocation == nil || group == nil || hfLocation.CanDelete || group.CanDelete || hfLocation.Warning == "" {
			t.Fatalf("unresolved registered HF descendant did not preserve best-effort visibility while refusing delete: location=%+v group=%+v", hfLocation, group)
		}
		body, err := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: hfLocation.ID, GroupID: group.ID, Members: group.Members})
		if err != nil {
			t.Fatal(err)
		}
		w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
		if w.Code != http.StatusConflict {
			t.Fatalf("HF delete status=%d body=%s; unresolved configured root must refuse before payload helper", w.Code, w.Body.String())
		}
		if _, err := os.Lstat(snapshotEntry); err != nil {
			t.Fatalf("protected snapshot entry changed: %v", err)
		}
		if data, err := os.ReadFile(blob); err != nil || string(data) != "protected-model-payload" {
			t.Fatalf("protected payload changed: data=%q err=%v", data, err)
		}
		if ref, err := rd.ReadRef("main"); err != nil || ref != "saved-version" {
			t.Fatalf("HF ref changed: ref=%q err=%v", ref, err)
		}
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("permission-denied symlink resolution regression requires Linux credentials")
	}
	fixtureRoot := t.TempDir()
	if err := os.Chmod(fixtureRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	storage := filepath.Join(fixtureRoot, "cache")
	rd, err := hfdownloader.NewHFCache(storage, 0).Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	snapshotEntry, err := rd.SnapshotPath("saved-version", "library/vendor/child/vendor-Q4_K_M.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(snapshotEntry), 0o755); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(rd.BlobsDir(), strings.Repeat("a", 64))
	if err := os.WriteFile(blob, []byte("protected-model-payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	relTarget, err := filepath.Rel(filepath.Dir(snapshotEntry), blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relTarget, snapshotEntry); err != nil {
		t.Fatal(err)
	}
	if err := rd.WriteRef("main", "saved-version"); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(fixtureRoot, "private")
	if err := os.Mkdir(private, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(private, "snapshot-library")
	if err := os.Symlink(filepath.Join(rd.SnapshotsDir(), "saved-version", "library"), alias); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		if err := filepath.Walk(fixtureRoot, func(name string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil
			}
			return os.Chown(name, 65534, 65534)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(private, 0); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(private, 0o700); err != nil {
			t.Errorf("restore permission for fixture cleanup: %v", err)
		}
	}()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmdName := executable
	cmdArgs := []string{"-test.run=^TestSelectedHFDeleteRefusesUnresolvedConfiguredDescendant$"}
	if os.Geteuid() == 0 {
		runuser, err := exec.LookPath("runuser")
		if err != nil {
			t.Fatal("cannot exercise HF permission-denied resolution as unprivileged identity: runuser unavailable")
		}
		data, err := os.ReadFile(executable)
		if err != nil {
			t.Fatal(err)
		}
		helper := filepath.Join(fixtureRoot, "server.test")
		if err := os.WriteFile(helper, data, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(helper, 0, 0); err != nil {
			t.Fatal(err)
		}
		cmdName = runuser
		cmdArgs = []string{"-u", "nobody", "--", helper, "-test.run=^TestSelectedHFDeleteRefusesUnresolvedConfiguredDescendant$"}
	}
	cmd := exec.Command(cmdName, cmdArgs...)
	cmd.Env = append(os.Environ(), fixtureEnv+"="+fixtureRoot)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unprivileged HF permission regression helper failed: %v\n%s", err, output)
	}
}

func TestConfiguredHFProtectedRootsProjectsAliasesAndAllowsMissingUnrelatedRoots(t *testing.T) {
	storage := t.TempDir()
	rd, err := hfdownloader.NewHFCache(storage, 0).Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(rd.SnapshotsDir(), "saved-version", "library")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "registered-child")
	symlinkOrSkip(t, child, alias)
	missing := filepath.Join(rd.SnapshotsDir(), "saved-version", "future", "local-root")
	protected, complete := configuredHFProtectedRoots(Config{LocalScanDirs: []string{alias, missing}}, rd)
	if !complete {
		t.Fatalf("resolvable alias and ordinary missing suffix marked incomplete: %v", protected)
	}
	want := map[string]bool{pathIdentityKey(child): true, pathIdentityKey(missing): true}
	for _, root := range protected {
		delete(want, pathIdentityKey(root))
	}
	if len(want) != 0 {
		t.Fatalf("protected roots omitted projected descendant/missing boundary: %v; got %v", want, protected)
	}

	unrelated := t.TempDir()
	unrelatedAlias := filepath.Join(t.TempDir(), "unrelated-alias")
	symlinkOrSkip(t, unrelated, unrelatedAlias)
	protected, complete = configuredHFProtectedRoots(Config{LocalScanDirs: []string{unrelatedAlias}}, rd)
	if !complete || len(protected) != 0 {
		t.Fatalf("ordinary unrelated configured alias should not disable HF deletion: roots=%v complete=%t", protected, complete)
	}
}

func TestHFSelectionKeepsKnownAliasedConfiguredChildIndependent(t *testing.T) {
	storage := t.TempDir()
	rd, err := hfdownloader.NewHFCache(storage, 0).Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	versionRoot := filepath.Join(rd.SnapshotsDir(), "saved-version")
	child := filepath.Join(versionRoot, "library")
	writeSelectionFile(t, filepath.Join(child, "owner", "model"), "child-Q4_K_M.gguf")
	writeSelectionFile(t, versionRoot, "outer-Q5_K_M.gguf")
	alias := filepath.Join(t.TempDir(), "registered-library")
	symlinkOrSkip(t, child, alias)
	s := newTestServerWithConfig(t, Config{CacheDir: storage, LocalScanDirs: []string{alias}})
	_, selection := getSelection(t, s, "owner/model", "model", "")
	var hfLocation, localLocation *cacheSelectionLocation
	for i := range selection.Locations {
		loc := &selection.Locations[i]
		switch {
		case strings.HasPrefix(loc.ID, "hf-"):
			hfLocation = loc
		case loc.Source == "Local" && strings.HasPrefix(loc.Path, alias):
			localLocation = loc
		}
	}
	if hfLocation == nil || !hfLocation.CanDelete {
		t.Fatalf("known protected descendant unnecessarily disabled unrelated HF entries: %+v", selection.Locations)
	}
	if localLocation == nil || !localLocation.CanDelete {
		t.Fatalf("known aliased child root was not independently selectable: %+v", selection.Locations)
	}
	for _, group := range hfLocation.Groups {
		for _, member := range group.Members {
			if strings.Contains(member.Path, "child-Q4_K_M.gguf") {
				t.Fatalf("HF listing claimed separately registered alias member: %+v", member)
			}
		}
	}
	childVisible := false
	for _, group := range localLocation.Groups {
		for _, member := range group.Members {
			childVisible = childVisible || member.Path == "child-Q4_K_M.gguf"
		}
	}
	if !childVisible {
		t.Fatalf("protected alias member missing from independent local selection: %+v", localLocation.Groups)
	}
}

func TestSelectedDeleteConflictsWithHFWriterSnapshotPrescan(t *testing.T) {
	storage := t.TempDir()
	cache := hfdownloader.NewHFCache(storage, 0)
	rd, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rd.SnapshotsDir(), "owner", "model"), 0o755); err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(rd.SnapshotsDir(), "owner", "model", "model-Q4_K_M.gguf")
	if err := os.WriteFile(selected, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	hf, started, release := blockingRevisionServer(t, "model-Q4_K_M.gguf")
	defer hf.Close()
	s := newTestServerWithConfig(t, Config{CacheDir: storage, LocalScanDirs: []string{rd.SnapshotsDir()}, Endpoint: hf.URL})
	releaseDelete, ok := s.jobs.reserveSelectedGGUF([]string{selected})
	if !ok {
		t.Fatal("could not reserve selected snapshot for admission check")
	}
	if _, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "upstream/source", LocalRepo: "owner/model", Filters: []string{"Q4_K_M"}}); !errors.Is(err, errSelectionWriterBusy) {
		releaseDelete()
		t.Fatalf("new matching HF writer crossed held snapshot deletion: %v", err)
	}
	releaseDelete()
	job, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "upstream/source", LocalRepo: "owner/model", Filters: []string{"Q4_K_M"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("production HF writer did not enter prescan")
	}
	body := selectedGroupRequest(t, s, "owner/model", rd.SnapshotsDir(), "model-Q4_K_M.gguf")
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("active HF writer did not block selected snapshot deletion: status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(selected); err != nil {
		t.Fatalf("selected snapshot was removed during active writer: %v", err)
	}
	release()
	if !s.jobs.CancelJob(job.ID) {
		t.Fatal("could not cancel prescan test job")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatalf("close test job manager: %v", err)
	}
}

func TestCompletePlanUsesActualSnapshotDestinations(t *testing.T) {
	for _, tc := range []struct {
		name       string
		selectedAt string
		planPath   string
		wantStatus int
	}{
		{name: "same commit and path conflicts", selectedAt: "deadbeef", planPath: "owner/model/model-Q4_K_M.gguf", wantStatus: http.StatusConflict},
		{name: "same path in another commit is disjoint", selectedAt: "saved-version", planPath: "owner/model/model-Q4_K_M.gguf", wantStatus: http.StatusOK},
		{name: "unfiltered Q5-only plan is disjoint from selected Q4", selectedAt: "saved-version", planPath: "owner/model/model-Q5_K_M.gguf", wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := t.TempDir()
			cache := hfdownloader.NewHFCache(storage, 0)
			rd, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
			if err != nil {
				t.Fatal(err)
			}
			scanRoot := filepath.Join(rd.SnapshotsDir(), tc.selectedAt)
			selected := filepath.Join(scanRoot, "owner", "model", "model-Q4_K_M.gguf")
			if err := os.MkdirAll(filepath.Dir(selected), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(selected, []byte("gguf"), 0o644); err != nil {
				t.Fatal(err)
			}
			hf, started, release := blockingPlanServer(t, tc.planPath)
			defer hf.Close()
			var releaseOnce sync.Once
			releaseGate := func() { releaseOnce.Do(release) }
			defer releaseGate()
			s := newTestServerWithConfig(t, Config{CacheDir: storage, LocalScanDirs: []string{scanRoot}, Endpoint: hf.URL})
			job, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "upstream/source", LocalRepo: "owner/model"})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("production HF writer did not reach the planned transfer")
			}
			body := selectedGroupRequest(t, s, "owner/model", scanRoot, "model-Q4_K_M.gguf")
			w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
			if w.Code != tc.wantStatus {
				t.Fatalf("selected delete status=%d want=%d body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantStatus == http.StatusConflict {
				if _, err := os.Stat(selected); err != nil {
					t.Fatalf("busy delete removed in-flight selected file: %v", err)
				}
			} else if _, err := os.Lstat(selected); !os.IsNotExist(err) {
				t.Fatalf("disjoint selected entry was not deleted: %v", err)
			}
			releaseGate()
			if !s.jobs.CancelJob(job.ID) {
				t.Fatal("could not cancel planned transfer")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.jobs.Close(ctx); err != nil {
				t.Fatalf("close test job manager: %v", err)
			}
		})
	}
}

// A paginated production download must keep its write scope unknown until all
// Hub tree pages have been scanned, then reserve the later-page target before
// the first file transfer begins. This joins the Hub tree walker to the job
// manager's complete-plan callback and selected-delete admission path.
func TestPaginatedPlanReservesLaterPageBeforeTransfer(t *testing.T) {
	storage := t.TempDir()
	cache := hfdownloader.NewHFCache(storage, 0)
	rd, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(rd.SnapshotsDir(), "deadbeef", "owner", "model", "model-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(snapshot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	scanRoot := filepath.Join(rd.SnapshotsDir(), "deadbeef")

	pageStarted := make(chan struct{}, 1)
	transferStarted := make(chan struct{}, 1)
	releasePage := make(chan struct{})
	releaseTransfer := make(chan struct{})
	var releasePageOnce, releaseTransferOnce sync.Once
	openPage := func() { releasePageOnce.Do(func() { close(releasePage) }) }
	openTransfer := func() { releaseTransferOnce.Do(func() { close(releaseTransfer) }) }
	hf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			_, _ = w.Write([]byte(`{"sha":"deadbeef"}`))
		case strings.Contains(r.URL.Path, "/tree/") && r.URL.Query().Get("cursor") == "later":
			pageStarted <- struct{}{}
			select {
			case <-releasePage:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte(`[{"type":"file","path":"owner/model/model-Q4_K_M.gguf","size":5}]`))
		case strings.Contains(r.URL.Path, "/tree/"):
			w.Header().Set("Link", "<"+"http://"+r.Host+r.URL.Path+"?cursor=later>; rel=next")
			_, _ = w.Write([]byte(`[{"type":"file","path":"model-Q5_K_M.gguf","size":5},{"type":"file","path":"config.json","size":5}]`))
		case strings.Contains(r.URL.Path, "/raw/") || strings.Contains(r.URL.Path, "/resolve/"):
			transferStarted <- struct{}{}
			select {
			case <-releaseTransfer:
				_, _ = w.Write([]byte("data!"))
			case <-r.Context().Done():
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer hf.Close()
	defer openPage()
	defer openTransfer()

	s := newTestServerWithConfig(t, Config{CacheDir: storage, LocalScanDirs: []string{scanRoot}, Endpoint: hf.URL})
	job, _, err := s.jobs.CreateJob(DownloadRequest{
		Repo: "upstream/source", LocalRepo: "owner/model", Filters: []string{"q4_k_m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-pageStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not request the later Hub tree page")
	}

	body := selectedGroupRequest(t, s, "owner/model", scanRoot, "model-Q4_K_M.gguf")
	if w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body)); w.Code != http.StatusConflict {
		t.Fatalf("delete crossed a download with an incomplete paginated plan: status=%d body=%s", w.Code, w.Body.String())
	}

	openPage()
	select {
	case <-transferStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not begin the later-page Q4 transfer")
	}
	if w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body)); w.Code != http.StatusConflict {
		t.Fatalf("later-page planned snapshot was not protected before transfer: status=%d body=%s", w.Code, w.Body.String())
	}

	if !s.jobs.CancelJob(job.ID) {
		t.Fatal("could not cancel paginated test job")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatalf("close test job manager: %v", err)
	}
}

func TestSelectedDeleteAndRebuildReserveDatasetFriendlyTree(t *testing.T) {
	storage := t.TempDir()
	datasetRoot := filepath.Join(storage, "datasets")
	repoDir := filepath.Join(datasetRoot, "owner", "dataset")
	writeSelectionFile(t, repoDir, "dataset-Q4_K_M.gguf")
	s := newTestServerWithConfig(t, Config{CacheDir: storage, LocalScanDirs: []string{datasetRoot}})
	body := selectedGroupRequest(t, s, "owner/dataset", datasetRoot, "dataset-Q4_K_M.gguf")
	releaseRebuild, ok := s.jobs.beginCacheMutation(filepath.Join(storage, "datasets"))
	if !ok {
		t.Fatal("could not acquire the same dataset scope used by cache rebuild")
	}
	if w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body)); w.Code != http.StatusConflict {
		t.Fatalf("held dataset rebuild scope did not block selected deletion: status=%d body=%s", w.Code, w.Body.String())
	}
	releaseRebuild()
	releaseDelete, ok := s.jobs.reserveSelectedGGUF([]string{filepath.Join(repoDir, "dataset-Q4_K_M.gguf")})
	if !ok {
		t.Fatal("could not reserve selected dataset member")
	}
	w := cacheRequest(t, s, "POST", "/api/cache/rebuild", `{}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("held selected deletion did not block dataset rebuild: status=%d body=%s", w.Code, w.Body.String())
	}
	releaseDelete()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatalf("close test job manager: %v", err)
	}
}

func TestMutationEntryIdentityResolvesParentAliasesButNotLeafTargets(t *testing.T) {
	actual, alias := t.TempDir(), t.TempDir()
	if err := os.Symlink(actual, filepath.Join(alias, "parent")); err != nil {
		if runtime.GOOS == "windows" && os.IsPermission(err) {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if !mutationPathsOverlap(filepath.Join(actual, "owner", "model", "Q4.gguf"), filepath.Join(alias, "parent", "owner", "model", "Q4.gguf")) {
		t.Fatal("parent alias did not identify the same entry")
	}
	if !mutationDirectoryEntryOverlap(filepath.Join(alias, "parent"), filepath.Join(actual, "owner", "model", "Q4.gguf")) ||
		!mutationDirectoryEntryOverlap(actual, filepath.Join(alias, "parent", "owner", "model", "Q4.gguf")) {
		t.Fatal("directory/entry scope comparison missed an alias in one direction")
	}
	target := filepath.Join(actual, "payload.gguf")
	leaf := filepath.Join(actual, "link-Q4.gguf")
	if err := os.Symlink(target, leaf); err != nil {
		t.Fatal(err)
	}
	got, err := mutationEntryIdentity(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if got == pathIdentityKey(target) {
		t.Fatalf("leaf identity followed symlink target: %q", got)
	}
}

func TestMutationEntryIdentityFailsClosedForUnknownParentResolution(t *testing.T) {
	root := t.TempDir()
	loop := filepath.Join(root, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		if runtime.GOOS == "windows" && os.IsPermission(err) {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	for _, prefix := range []string{loop, filepath.Join(root, "missing-target")} {
		if prefix != loop {
			if err := os.Symlink(filepath.Join(root, "does-not-exist"), prefix); err != nil {
				t.Fatal(err)
			}
		}
		path := filepath.Join(prefix, "child", "Q4.gguf")
		if _, err := mutationEntryIdentity(path); err == nil {
			t.Errorf("mutationEntryIdentity(%q) accepted unresolved symlink prefix", path)
		}
		if !mutationPathsOverlap(path, filepath.Join(t.TempDir(), "other", "Q4.gguf")) {
			t.Errorf("unresolved path %q was treated as a proven non-overlap", path)
		}
	}
	regular := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(regular, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := mutationEntryIdentity(filepath.Join(regular, "child", "Q4.gguf")); err == nil {
		t.Fatal("non-directory existing prefix was accepted as an ordinary missing suffix")
	}
}

func TestJobDestinationBaseUsesFrozenLocalRepoAndDatasetNamespace(t *testing.T) {
	job := &Job{OutputDir: "/cache", Repo: "upstream/source", LocalRepo: "owner/model", IsDataset: false}
	if got, want := jobDestinationBase(job), filepath.Join("/cache", "models", "owner", "model"); got != want {
		t.Fatalf("model destination=%q want %q", got, want)
	}
	if !jobMayWriteSelectedPath(job, filepath.Join("/cache", "models", "owner", "model", "model-Q4_K_M.gguf")) {
		t.Fatal("redirected model destination was not recognized as a selected-path writer")
	}
	if jobMayWriteSelectedPath(job, filepath.Join("/cache", "models", "upstream", "source", "model-Q4_K_M.gguf")) {
		t.Fatal("API repository spelling incorrectly authorized a different physical destination")
	}
	job.IsDataset = true
	if got, want := jobDestinationBase(job), filepath.Join("/cache", "datasets", "owner", "model"); got != want {
		t.Fatalf("dataset destination=%q want %q", got, want)
	}
	if jobMayWriteSelectedPath(job, filepath.Join("/cache", "models", "owner", "model", "model-Q4_K_M.gguf")) {
		t.Fatal("dataset writer leaked into the separate model namespace")
	}
	job.Flat, job.LocalDir, job.LocalRepo = true, "/flat", "nested/../custom/path"
	if got, want := jobDestinationBase(job), filepath.Join("/flat", "custom", "path"); got != want {
		t.Fatalf("normalized in-root flat destination=%q want %q", got, want)
	}
}

func TestJobDestinationBaseUsesModeSpecificRepositoryRules(t *testing.T) {
	for _, repo := range []string{"owner/..", "custom/deep/path", "a/../b", "owner/na\x00me"} {
		job := &Job{OutputDir: "/cache", Repo: "owner/source", LocalRepo: repo}
		if got := jobDestinationBase(job); got != "" {
			t.Errorf("HF-cache destination accepted invalid repo %q as %q", repo, got)
		}
		if paths, complete := jobPlannedWriteEntries(job, hfdownloader.Plan{}); complete || len(paths) != 0 {
			t.Errorf("invalid HF destination %q produced a complete empty plan: paths=%v complete=%v", repo, paths, complete)
		}
	}
	for _, repo := range []string{"custom/deep/path", "a/../b"} {
		job := &Job{LocalDir: "/local", OutputDir: "/local", Repo: "owner/source", LocalRepo: repo}
		if got := jobDestinationBase(job); got == "" {
			t.Errorf("flat destination rejected valid folder %q", repo)
		}
	}
}

func TestInvalidRestoredHFRepoCannotResumeRetryOrDispatch(t *testing.T) {
	srv := newTestServer(t)
	m := srv.jobs
	base := Job{
		Repo: "owner/source", LocalRepo: "owner/..", OutputDir: srv.config.CacheDir,
		HubDir: filepath.Join(srv.config.CacheDir, "hub"), CreatedAt: time.Now(),
	}
	paused := base
	paused.ID, paused.Status = "invalid-paused", JobStatusPaused
	failed := base
	failed.ID, failed.Status = "invalid-failed", JobStatusFailed
	queued := base
	queued.ID, queued.Status = "invalid-queued", JobStatusQueued
	m.mu.Lock()
	m.jobs[paused.ID] = &paused
	m.jobs[failed.ID] = &failed
	m.jobs[queued.ID] = &queued
	m.mu.Unlock()

	if m.ResumeJob(paused.ID) {
		t.Fatal("invalid restored HF repo was resumed")
	}
	if m.RetryJob(failed.ID) {
		t.Fatal("invalid restored HF repo was retried")
	}
	m.mu.Lock()
	m.dispatchLocked()
	got := m.jobs[queued.ID]
	if got.Status != JobStatusQueued || got.starting {
		m.mu.Unlock()
		t.Fatalf("invalid restored HF repo was dispatched: status=%s starting=%v", got.Status, got.starting)
	}
	if len(m.runActivities) != 0 {
		m.mu.Unlock()
		t.Fatalf("invalid restored HF repo registered a writer: %#v", m.runActivities)
	}
	m.mu.Unlock()
}

func TestJobMayWriteSelectedSnapshotEntry(t *testing.T) {
	cache := hfdownloader.NewHFCache(t.TempDir(), 0)
	rd, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{OutputDir: cache.Root, HubDir: cache.HubDir(), Repo: "upstream/source", LocalRepo: "owner/model", Filters: []string{"Q4_K_M"}}
	selected := filepath.Join(rd.SnapshotsDir(), "saved-version", "model-Q4_K_M.gguf")
	if !jobMayWriteSelectedPath(job, selected) {
		t.Fatal("HF writer was not recognized as a possible writer beneath the frozen repository snapshots")
	}
	if jobMayWriteSelectedPath(job, filepath.Join(rd.SnapshotsDir(), "saved-version", "model-Q5_K_M.gguf")) {
		t.Fatal("nonmatching Q5 snapshot was treated as a Q4 writer")
	}
	dataset, err := cache.Repo("owner/model", hfdownloader.RepoTypeDataset)
	if err != nil {
		t.Fatal(err)
	}
	job.IsDataset = true
	job.LocalRepo = "owner/model"
	if !jobMayWriteSelectedPath(job, filepath.Join(dataset.SnapshotsDir(), "saved-version", "data-Q4_K_M.gguf")) {
		t.Fatal("dataset HF writer was not recognized beneath its frozen repository snapshots")
	}
}

func TestJobPlannedWriteEntriesTreatsEmptyPlanAsComplete(t *testing.T) {
	entries, complete := jobPlannedWriteEntries(&Job{OutputDir: t.TempDir(), Repo: "owner/model"}, hfdownloader.Plan{})
	if !complete || len(entries) != 0 {
		t.Fatalf("empty plan coverage entries=%v complete=%t, want empty complete coverage", entries, complete)
	}
}

func TestJobPlannedWriteEntriesUsesFrozenDatasetRepoAndHub(t *testing.T) {
	output, frozenHub := t.TempDir(), filepath.Join(t.TempDir(), "exact-hub")
	job := &Job{
		OutputDir: output, HubDir: frozenHub, Repo: "upstream/source", LocalRepo: "owner/dataset",
		IsDataset: true,
	}
	plan := hfdownloader.Plan{Commit: "deadbeef", Items: []hfdownloader.PlanItem{{RelativePath: "nested/data-Q4_K_M.gguf"}}}
	entries, complete := jobPlannedWriteEntries(job, plan)
	if !complete || len(entries) != 2 {
		t.Fatalf("planned entries=%v complete=%t", entries, complete)
	}
	want := map[string]bool{
		filepath.Join(output, "datasets", "owner", "dataset", "nested", "data-Q4_K_M.gguf"):                         true,
		filepath.Join(frozenHub, "datasets--owner--dataset", "snapshots", "deadbeef", "nested", "data-Q4_K_M.gguf"): true,
	}
	for _, entry := range entries {
		if !want[filepath.Clean(entry)] {
			t.Errorf("unexpected destination %q", entry)
		}
		delete(want, filepath.Clean(entry))
	}
	if len(want) != 0 {
		t.Errorf("missing frozen planned destinations: %v", want)
	}
}

func TestJobAdmissionPredicateResolvesSelectedRootAliases(t *testing.T) {
	actual, alias := t.TempDir(), filepath.Join(t.TempDir(), "scan")
	if err := os.Symlink(actual, alias); err != nil {
		if runtime.GOOS == "windows" && os.IsPermission(err) {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	job := &Job{OutputDir: actual, Repo: "upstream/source", LocalRepo: "owner/model", Filters: []string{"Q4_K_M"}}
	target := filepath.Join(alias, "models", "owner", "model", "model-Q4_K_M.gguf")
	if !jobMayWriteSelectedPath(job, target) {
		t.Fatal("shared writer predicate missed a selected entry beneath a configured-root alias")
	}
	if !jobMayWriteSelectedPath(job, filepath.Join(actual, "models", "owner", "model", "model-Q4_K_M.gguf")) {
		t.Fatal("shared writer predicate missed the canonical spelling of the selected entry")
	}
	if jobMayWriteSelectedPath(job, filepath.Join(alias, "models", "owner", "model", "model-Q5_K_M.gguf")) {
		t.Fatal("nonmatching Q5 was treated as a Q4 writer through the alias")
	}

	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalDir: actual})
	selectedLocalPath := filepath.Join(alias, "owner", "model", "model-Q4_K_M.gguf")
	queued := &Job{ID: "queued-alias-q4", Repo: "upstream/source", OutputDir: actual, LocalDir: actual, LocalRepo: "owner/model", Flat: true, Filters: []string{"Q4_K_M"}, Status: JobStatusQueued}
	s.jobs.mu.Lock()
	s.jobs.jobs[queued.ID] = queued
	s.jobs.mu.Unlock()
	if _, ok := s.jobs.reserveSelectedGGUF([]string{selectedLocalPath}); ok {
		t.Fatal("queued writer using canonical root bypassed alias-spelled selection")
	}
	s.jobs.mu.Lock()
	delete(s.jobs.jobs, queued.ID)
	paused := *queued
	paused.ID, paused.Status = "paused-alias-q4", JobStatusPaused
	cancelled := *queued
	cancelled.ID, cancelled.Status = "cancelled-alias-q4", JobStatusCancelled
	s.jobs.jobs[paused.ID] = &paused
	s.jobs.jobs[cancelled.ID] = &cancelled
	s.jobs.mu.Unlock()
	release, ok := s.jobs.reserveSelectedGGUF([]string{selectedLocalPath})
	if !ok {
		t.Fatal("could not reserve alias-spelled selected entry")
	}
	if s.jobs.ResumeJob(paused.ID) {
		t.Fatal("paused canonical writer resumed through alias-spelled reservation")
	}
	if s.jobs.RetryJob(cancelled.ID) {
		t.Fatal("cancelled canonical writer retried through alias-spelled reservation")
	}
	if _, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "upstream/source", LocalRepo: "owner/model", Filters: []string{"Q4_K_M"}}); !errors.Is(err, errSelectionWriterBusy) {
		t.Fatalf("new matching aliased writer crossed reservation: %v", err)
	}
	if jobMayWriteSelectedPath(&Job{OutputDir: actual, Repo: "upstream/source", LocalRepo: "owner/model", Filters: []string{"Q5_K_M"}}, target) {
		t.Fatal("nonmatching Q5 writer was blocked by Q4 reservation")
	}
	release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatalf("close test job manager: %v", err)
	}
}

func TestSelectedDeleteBlocksPrescanWriterWhenAliasChangesExcludedPath(t *testing.T) {
	storage := t.TempDir()
	repoDir := filepath.Join(storage, "models", "o", "m")
	blocked := filepath.Join(repoDir, "blocked")
	writeSelectionFile(t, blocked, "model-Q4_K_M.gguf")
	if err := os.Symlink(blocked, filepath.Join(repoDir, "live")); err != nil {
		if runtime.GOOS == "windows" && os.IsPermission(err) {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	hf, started, release := blockingRevisionServer(t, "live/model-Q4_K_M.gguf")
	defer hf.Close()
	s := newTestServerWithConfig(t, Config{CacheDir: storage, Endpoint: hf.URL})
	job, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "upstream/source", LocalRepo: "o/m", Filters: []string{"Q4_K_M"}, Excludes: []string{"blocked"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("production run did not enter repository prescan")
	}
	body := selectedGroupRequest(t, s, "o/m", repoDir, "blocked/model-Q4_K_M.gguf")
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("prescan raw live-path writer did not block DELETE: status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(blocked, "model-Q4_K_M.gguf")); err != nil {
		t.Fatalf("pending writer's selected file was deleted: %v", err)
	}
	release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !s.jobs.CancelJob(job.ID) {
		t.Fatal("could not cancel prescan test job")
	}
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatalf("close test job manager: %v", err)
	}
}

func TestSelectedDeleteExcludesPlannedWriterThroughAliasAfterPlan(t *testing.T) {
	for _, tc := range []struct {
		name        string
		writerQuant string
		writerFile  string
		wantStatus  int
	}{
		{name: "matching Q4 conflicts", writerQuant: "Q4_K_M", writerFile: "model-Q4_K_M.gguf", wantStatus: http.StatusConflict},
		{name: "nonmatching Q5 remains allowed", writerQuant: "Q5_K_M", writerFile: "model-Q5_K_M.gguf", wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := t.TempDir()
			repoDir := filepath.Join(storage, "models", "o", "m")
			blocked := filepath.Join(repoDir, "blocked")
			selectedFile := filepath.Join(blocked, "model-Q4_K_M.gguf")
			writeSelectionFile(t, blocked, filepath.Base(selectedFile))
			if err := os.Symlink(blocked, filepath.Join(repoDir, "live")); err != nil {
				if runtime.GOOS == "windows" && os.IsPermission(err) {
					t.Skipf("symlink privilege unavailable: %v", err)
				}
				t.Fatal(err)
			}
			hf, started, release := blockingPlanServer(t, "live/"+tc.writerFile)
			defer hf.Close()
			var releaseOnce sync.Once
			releaseGate := func() { releaseOnce.Do(release) }
			s := newTestServerWithConfig(t, Config{CacheDir: storage, Endpoint: hf.URL, MaxActive: 1})
			job, _, err := s.jobs.CreateJob(DownloadRequest{
				Repo: "upstream/source", LocalRepo: "o/m", Filters: []string{tc.writerQuant}, Excludes: []string{"blocked"},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				releaseGate()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := s.jobs.Close(ctx); err != nil {
					t.Errorf("close test job manager: %v", err)
				}
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("real downloader did not reach the planned transfer")
			}
			s.jobs.mu.Lock()
			planned := false
			for _, activity := range s.jobs.runActivities {
				if activity.job.ID == job.ID && activity.planKnown {
					planned = true
					break
				}
			}
			s.jobs.mu.Unlock()
			if !planned {
				t.Fatal("transfer began before the production run registered its completed plan")
			}

			body := selectedGroupRequest(t, s, "o/m", repoDir, "blocked/model-Q4_K_M.gguf")
			w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
			if w.Code != tc.wantStatus {
				t.Errorf("delete while planned %s transfer is held: status=%d body=%s, want %d", tc.writerQuant, w.Code, w.Body.String(), tc.wantStatus)
			}
			if tc.wantStatus == http.StatusConflict {
				if _, err := os.Stat(selectedFile); err != nil {
					t.Errorf("conflicting planned writer's selected entry was removed: %v", err)
				}
			} else if _, err := os.Stat(selectedFile); !os.IsNotExist(err) {
				t.Errorf("nonconflicting Q5 writer prevented selected Q4 deletion: %v", err)
			}

			releaseGate()
			deadline := time.After(5 * time.Second)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				current, ok := s.jobs.GetJob(job.ID)
				if ok && (current.Status == JobStatusCompleted || current.Status == JobStatusFailed) {
					if current.Status != JobStatusCompleted {
						t.Errorf("planned writer did not complete: %+v", current)
					}
					break
				}
				select {
				case <-deadline:
					t.Fatal("planned writer did not finish after transfer release")
				case <-ticker.C:
				}
			}
			written := filepath.Join(blocked, tc.writerFile)
			data, err := os.ReadFile(written)
			if err != nil || string(data) != "data!" {
				t.Errorf("planned transfer output=%q err=%v, want recreated/written data", data, err)
			}
		})
	}
}

func TestAliasedExcludedRemotePathStillBlocksSelectedEntry(t *testing.T) {
	storage := t.TempDir()
	repoDir := filepath.Join(storage, "models", "o", "m")
	blockedDir := filepath.Join(repoDir, "blocked")
	writeSelectionFile(t, blockedDir, "model-Q4_K_M.gguf")
	if err := os.Symlink(blockedDir, filepath.Join(repoDir, "live")); err != nil {
		if runtime.GOOS == "windows" && os.IsPermission(err) {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	selected := filepath.Join(blockedDir, "model-Q4_K_M.gguf")

	hf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			_, _ = w.Write([]byte(`{"sha":"deadbeef"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte(`[{"type":"file","path":"live/model-Q4_K_M.gguf","size":5}]`))
		case strings.Contains(r.URL.Path, "/raw/") || strings.Contains(r.URL.Path, "/resolve/"):
			_, _ = w.Write([]byte("data!"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer hf.Close()

	plan, err := hfdownloader.PlanRepo(context.Background(), hfdownloader.Job{
		Repo: "upstream/source", Revision: "main", Filters: []string{"Q4_K_M"}, Excludes: []string{"blocked"},
	}, hfdownloader.Settings{Endpoint: hf.URL})
	if err != nil {
		t.Fatalf("production PlanRepo: %v", err)
	}
	if len(plan.Items) != 1 || plan.Items[0].RelativePath != "live/model-Q4_K_M.gguf" {
		t.Fatalf("production plan paths=%+v, expected raw live path", plan.Items)
	}

	job := &Job{OutputDir: storage, Repo: "upstream/source", LocalRepo: "o/m", Flat: false, Filters: []string{"Q4_K_M"}, Excludes: []string{"blocked"}}
	plannedOutput := filepath.Join(jobDestinationBase(job), filepath.FromSlash(plan.Items[0].RelativePath))
	if got, err := mutationEntryIdentity(plannedOutput); err != nil || got != pathIdentityKey(selected) {
		t.Fatalf("planned output identity=%q err=%v, selected entry=%q", got, err, selected)
	}
	if !jobMayWriteSelectedPath(job, selected) {
		t.Fatal("physical alias component was incorrectly applied as the remote path exclusion")
	}

	runHF, started, releaseRun := blockingRevisionServer(t, "model-Q4_K_M.gguf")
	defer runHF.Close()
	s := newTestServerWithConfig(t, Config{CacheDir: storage, Endpoint: runHF.URL, MaxActive: 1})
	blocker, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "owner/blocker"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocker did not enter production run")
	}
	queued, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "upstream/source", LocalRepo: "o/m", Filters: []string{"Q4_K_M"}, Excludes: []string{"blocked"}})
	if err != nil {
		t.Fatalf("create queued matching writer: %v", err)
	}
	if queued.Status != JobStatusQueued {
		t.Fatalf("matching writer status=%s, want queued", queued.Status)
	}
	if _, ok := s.jobs.reserveSelectedGGUF([]string{selected}); ok {
		t.Fatal("queued production-path writer through live alias did not block selected deletion")
	}
	if !s.jobs.CancelJob(queued.ID) {
		t.Fatal("could not cancel queued test writer")
	}
	s.jobs.mu.Lock()
	paused := *queued
	paused.ID, paused.Status = "paused-remote-live", JobStatusPaused
	s.jobs.jobs[paused.ID] = &paused
	s.jobs.mu.Unlock()
	release, ok := s.jobs.reserveSelectedGGUF([]string{selected})
	if !ok {
		t.Fatal("could not reserve selected physical entry")
	}
	if s.jobs.ResumeJob(paused.ID) || s.jobs.RetryJob(queued.ID) {
		t.Fatal("paused/retry admission crossed selected reservation for alias-excluded writer")
	}
	created, _, createErr := s.jobs.CreateJob(DownloadRequest{
		Repo: "upstream/source", LocalRepo: "o/m", Filters: []string{"Q4_K_M"}, Excludes: []string{"blocked"},
	})
	if !errors.Is(createErr, errSelectionWriterBusy) {
		if created != nil {
			_ = s.jobs.CancelJob(created.ID)
		}
		release()
		t.Fatalf("matching aliased writer crossed selected reservation: job=%+v err=%v", created, createErr)
	}
	q5, _, err := s.jobs.CreateJob(DownloadRequest{
		Repo: "upstream/source", Revision: "other", LocalRepo: "o/m", Filters: []string{"Q5_K_M"}, Excludes: []string{"blocked"},
	})
	if err != nil {
		t.Fatalf("nonmatching Q5 was rejected by Q4 reservation: %v", err)
	}
	if q5.Status != JobStatusQueued {
		t.Fatalf("Q5 status=%s, want queued behind blocker", q5.Status)
	}
	if !s.jobs.CancelJob(q5.ID) {
		t.Fatal("could not cancel nonmatching Q5 test job")
	}
	release()
	if !s.jobs.CancelJob(blocker.ID) {
		t.Fatal("could not cancel blocking test job")
	}
	releaseRun()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatalf("close test job manager: %v", err)
	}
	if _, err := os.Stat(selected); err != nil {
		t.Fatalf("reserved selected file changed: %v", err)
	}
}

func TestSelectedCacheDeleteRemovesOnlyConfirmedLocalGroup(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	for _, name := range []string{"model-Q2_K.gguf", "model-Q3_K_M.gguf", "model-Q4_K_M.gguf", "model-Q5_K_M.gguf", "model-Q6_K.gguf", "model-Q8_0.gguf", "model-Q4_K_M-00001-of-00002.gguf", "model-Q4_K_M-00002-of-00002.gguf", "README.md"} {
		writeSelectionFile(t, repoDir, name)
	}
	otherFile := filepath.Join(other, "owner", "model", "model-Q4_K_M.gguf")
	writeSelectionFile(t, filepath.Dir(otherFile), filepath.Base(otherFile))
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root, other}})
	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != 200 || len(selection.Locations) != 2 {
		t.Fatalf("selection=%d %+v", code, selection)
	}
	var selected cacheSelectionLocation
	for _, loc := range selection.Locations {
		if strings.Contains(loc.Path, root) {
			selected = loc
		}
	}
	var group cacheSelectionGroup
	for _, g := range selected.Groups {
		if len(g.Members) == 1 && g.Members[0].Path == "model-Q4_K_M.gguf" {
			group = g
			break
		}
	}
	if group.ID == "" {
		t.Fatalf("Q4 group not found: %+v", selected.Groups)
	}
	body, err := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: selected.ID, GroupID: group.ID, Members: group.Members})
	if err != nil {
		t.Fatal(err)
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != 200 {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
	selectedPaths := map[string]bool{}
	for _, member := range group.Members {
		selectedPaths[member.Path] = true
		if _, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(member.Path))); !os.IsNotExist(err) {
			t.Errorf("selected file %s remains, err=%v", member.Path, err)
		}
	}
	for _, name := range []string{"model-Q2_K.gguf", "model-Q3_K_M.gguf", "model-Q4_K_M-00001-of-00002.gguf", "model-Q4_K_M-00002-of-00002.gguf", "model-Q5_K_M.gguf", "model-Q6_K.gguf", "model-Q8_0.gguf", "README.md"} {
		if selectedPaths[name] {
			continue
		}
		if _, err := os.Stat(filepath.Join(repoDir, name)); err != nil {
			t.Errorf("unselected file %s changed: %v", name, err)
		}
	}
	if _, err := os.Stat(otherFile); err != nil {
		t.Errorf("same file in other location changed: %v", err)
	}
}

func TestSelectedLocalDeleteStaysOnOpenedRootAfterAliasRetarget(t *testing.T) {
	actual, other := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "scan")
	if err := os.Symlink(actual, alias); err != nil {
		if runtime.GOOS == "windows" && os.IsPermission(err) {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	rel := "model-Q4_K_M.gguf"
	writeSelectionFile(t, filepath.Join(actual, "owner", "model"), rel)
	writeSelectionFile(t, filepath.Join(other, "owner", "model"), rel)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalDir: actual, LocalScanDirs: []string{alias}})
	s.selectedDeleteHooks.forceHardLinkPin = true
	body := selectedGroupRequest(t, s, "owner/model", filepath.Join(alias, "owner", "model"), rel)
	retarget := func() {
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, alias); err != nil {
			t.Fatal(err)
		}
	}
	var mutationBlocked bool
	var writerBlocked bool
	s.selectedDeleteHooks.afterFinalScan = func() {
		retarget()
		job, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "upstream/source", LocalRepo: "owner/model", Filters: []string{"Q4_K_M"}})
		if errors.Is(err, errSelectionWriterBusy) {
			writerBlocked = true
		} else if err == nil {
			s.jobs.CancelJob(job.ID)
		}
		if release, ok := s.jobs.beginCacheMutation(filepath.Join(actual, "owner", "model")); ok {
			release()
		} else {
			mutationBlocked = true
		}
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
	if !mutationBlocked {
		t.Fatal("canonical writer for admitted root was not blocked after alias retarget")
	}
	if !writerBlocked {
		t.Fatal("canonical queued writer for admitted root was not blocked after alias retarget")
	}
	if _, err := os.Stat(filepath.Join(actual, "owner", "model", rel)); !os.IsNotExist(err) {
		t.Fatalf("selected entry in admitted root remains or was replaced: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, "owner", "model", rel)); err != nil {
		t.Fatalf("retarget destination was changed: %v", err)
	}
	assertNoTemporaryPinContainer(t, filepath.Join(actual, "owner", "model"))
	assertNoTemporaryPinContainer(t, filepath.Join(other, "owner", "model"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedLocalDeletePreservesDifferentSizedRetargetDestination(t *testing.T) {
	actual, other := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "scan")
	if err := os.Symlink(actual, alias); err != nil {
		if runtime.GOOS == "windows" && os.IsPermission(err) {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	rel := "model-Q4_K_M.gguf"
	writeSelectionFile(t, filepath.Join(actual, "owner", "model"), rel)
	otherFile := filepath.Join(other, "owner", "model", rel)
	if err := os.MkdirAll(filepath.Dir(otherFile), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherFile, []byte("a different-sized destination"), 0644); err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{alias}})
	body := selectedGroupRequest(t, s, "owner/model", filepath.Join(alias, "owner", "model"), rel)
	s.selectedDeleteHooks.afterFinalScan = func() {
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, alias); err != nil {
			t.Fatal(err)
		}
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(actual, "owner", "model", rel)); !os.IsNotExist(err) {
		t.Fatalf("selected entry in admitted root remains: %v", err)
	}
	if data, err := os.ReadFile(otherFile); err != nil || string(data) != "a different-sized destination" {
		t.Fatalf("different-sized retarget destination changed: data=%q err=%v", data, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedLocalDeleteRejectsAliasRetargetDuringAdmission(t *testing.T) {
	actual, other := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "scan")
	if err := os.Symlink(actual, alias); err != nil {
		if runtime.GOOS == "windows" && os.IsPermission(err) {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	rel := "model-Q4_K_M.gguf"
	writeSelectionFile(t, filepath.Join(actual, "owner", "model"), rel)
	writeSelectionFile(t, filepath.Join(other, "owner", "model"), rel)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{alias}})
	body := selectedGroupRequest(t, s, "owner/model", filepath.Join(alias, "owner", "model"), rel)
	s.selectedDeleteHooks.afterRootOpen = func() {
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, alias); err != nil {
			t.Fatal(err)
		}
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("retargeted admission status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	for _, path := range []string{filepath.Join(actual, "owner", "model", rel), filepath.Join(other, "owner", "model", rel)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("admission retarget changed %s: %v", path, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedLocalDeleteRejectsAliasRetargetDuringOpen(t *testing.T) {
	actual, other := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "scan")
	if err := os.Symlink(actual, alias); err != nil {
		if runtime.GOOS == "windows" && os.IsPermission(err) {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	rel := "model-Q4_K_M.gguf"
	writeSelectionFile(t, filepath.Join(actual, "owner", "model"), rel)
	writeSelectionFile(t, filepath.Join(other, "owner", "model"), rel)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{alias}})
	body := selectedGroupRequest(t, s, "owner/model", filepath.Join(alias, "owner", "model"), rel)
	s.selectedDeleteHooks.beforeRootOpen = func() {
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, alias); err != nil {
			t.Fatal(err)
		}
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("retarget during open status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	for _, path := range []string{filepath.Join(actual, "owner", "model", rel), filepath.Join(other, "owner", "model", rel)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retarget during open changed %s: %v", path, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedLocalDeleteRechecksMemberIdentityBeforeUnlink(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	first, second := "model-Q4_K_M-00001-of-00002.gguf", "model-Q4_K_M-00002-of-00002.gguf"
	writeSelectionFile(t, repoDir, first)
	writeSelectionFile(t, repoDir, second)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	body := selectedGroupRequest(t, s, "owner/model", repoDir, first)
	originalInfo, err := os.Lstat(filepath.Join(repoDir, first))
	if err != nil {
		t.Fatal(err)
	}
	// A same-size replacement after complete preflight must be detected before
	// the first unlink, leaving the replacement untouched and returning 409.
	s.selectedDeleteHooks.beforeRemove = func(i int) {
		if i != 0 {
			return
		}
		path := filepath.Join(repoDir, first)
		requireReplacementFixtureOperation(t, os.Remove(path))
		requireReplacementFixtureOperation(t, os.WriteFile(path, []byte("new!"), 0644))
		replacementInfo, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(originalInfo, replacementInfo) {
			t.Fatal("replacement reused the selected inode despite the retained identity pin")
		}
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("same-size replacement status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if data, err := os.ReadFile(filepath.Join(repoDir, first)); err != nil || string(data) != "new!" {
		t.Fatalf("replacement was removed or altered: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, second)); err != nil {
		t.Fatalf("unrelated shard changed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedLocalDeleteRejectsSizeChangeDuringIdentityCapture(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	first, second := "model-Q4_K_M-00001-of-00002.gguf", "model-Q4_K_M-00002-of-00002.gguf"
	writeSelectionFile(t, repoDir, first)
	writeSelectionFile(t, repoDir, second)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	body := selectedGroupRequest(t, s, "owner/model", repoDir, first)
	s.selectedDeleteHooks.beforeIdentityPin = func(name string) {
		if name != first {
			return
		}
		if err := os.WriteFile(filepath.Join(repoDir, first), []byte("growth!"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("size change during identity capture status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if data, err := os.ReadFile(filepath.Join(repoDir, first)); err != nil || string(data) != "growth!" {
		t.Fatalf("changed selected entry was removed or altered: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, second)); err != nil {
		t.Fatalf("other shard changed: %v", err)
	}
}

func TestSelectedLocalDeletePinsUnreadableRegularFileWithHardLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode-bit read denial and Darwin-style hard-link pin are Unix-specific")
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	name := "model-Q4_K_M.gguf"
	writeSelectionFile(t, repoDir, name)
	selectedPath := filepath.Join(repoDir, name)
	if err := os.Chmod(selectedPath, 0000); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(selectedPath)
	if err == nil {
		_ = probe.Close()
		t.Skip("test process can still read mode-000 fixture")
	}
	if !os.IsPermission(err) {
		t.Fatalf("open mode-000 fixture error=%v, want permission denied", err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	s.selectedDeleteHooks.forceHardLinkPin = true
	var privateContainer bool
	s.selectedDeleteHooks.beforeHardLinkPin = func(_, destination string) error {
		container := filepath.Join(repoDir, filepath.Dir(filepath.FromSlash(destination)))
		info, err := os.Stat(container)
		if err != nil {
			return err
		}
		privateContainer = info.IsDir() && info.Mode().Perm()&0077 == 0
		return nil
	}
	body := selectedGroupRequest(t, s, "owner/model", repoDir, name)
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("hard-link pin delete status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("selected mode-000 file remains after deletion: %v", err)
	}
	if !privateContainer {
		t.Fatal("hard-link pin container was not private while the pin was active")
	}
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hfdesk-delete-pin-") {
			t.Fatalf("temporary hard-link pin artifact remains after success: %s", entry.Name())
		}
	}
	if release, ok := s.jobs.beginCacheMutation(repoDir); !ok {
		t.Fatal("writer reservation was not released after hard-link cleanup")
	} else {
		release()
	}
}

func TestSelectedLocalDeletePlacesHardLinkPinUnderWritableSourceParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode-bit hard-link pin fixture is Unix-specific")
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	sourceParent := filepath.Join(repoDir, "nested")
	name := "model-Q4_K_M.gguf"
	writeSelectionFile(t, sourceParent, name)
	selectedPath := filepath.Join(sourceParent, name)
	if err := os.Chmod(selectedPath, 0000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(repoDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(repoDir, 0755); err != nil {
			t.Errorf("restore repo permissions for temporary fixture cleanup: %v", err)
		}
	})
	if err := os.Chmod(sourceParent, 0755); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(selectedPath)
	if err == nil {
		_ = probe.Close()
		t.Skip("test process can still read mode-000 fixture")
	}
	if !os.IsPermission(err) {
		t.Fatalf("open mode-000 fixture error=%v, want permission denied", err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	s.selectedDeleteHooks.forceHardLinkPin = true
	body := selectedGroupRequest(t, s, "owner/model", repoDir, "nested/"+name)
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("source-parent hard-link pin delete status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("selected nested file remains: %v", err)
	}
	assertNoTemporaryPinContainer(t, sourceParent)
	assertNoTemporaryPinContainer(t, repoDir)
}

func TestSelectedLocalDeleteUsesKnownRepoRootOnlyAfterSourceParentPinDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hard-link pin fallback fixture is Unix-specific")
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	sourceParent := filepath.Join(repoDir, "nested")
	name := "model-Q4_K_M.gguf"
	writeSelectionFile(t, sourceParent, name)
	selectedPath := filepath.Join(sourceParent, name)
	if err := os.Chmod(selectedPath, 0000); err != nil {
		t.Fatal(err)
	}
	if probe, err := os.Open(selectedPath); err == nil {
		_ = probe.Close()
		t.Skip("test process can still read mode-000 fixture")
	} else if !os.IsPermission(err) {
		t.Fatalf("open mode-000 fixture error=%v, want permission denied", err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	s.selectedDeleteHooks.forceHardLinkPin = true
	var destinations []string
	s.selectedDeleteHooks.beforeHardLinkPin = func(_, destination string) error {
		destinations = append(destinations, destination)
		if len(destinations) == 1 {
			return os.ErrPermission
		}
		return nil
	}
	body := selectedGroupRequest(t, s, "owner/model", repoDir, "nested/"+name)
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("repo-root fallback status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if len(destinations) != 2 {
		t.Fatalf("pin attempts=%v, want source-parent attempt then one repo-root fallback", destinations)
	}
	sourcePinDir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(destinations[0])))
	rootPinDir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(destinations[1])))
	if !strings.HasPrefix(sourcePinDir, "nested/.hfdesk-delete-pin-") || !strings.HasPrefix(rootPinDir, ".hfdesk-delete-pin-") {
		t.Fatalf("pin did not prefer source parent before repo-root fallback: %v", destinations)
	}
	if _, err := os.Lstat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("selected entry remains after approved repo-root fallback: %v", err)
	}
	assertNoTemporaryPinContainer(t, repoDir)
	assertNoTemporaryPinContainer(t, sourceParent)
}

func TestSelectedLocalDeleteCleansSourceParentPinThroughRetainedHandle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode-bit hard-link pin binding fixture is Unix-specific")
	}
	root, foreign := t.TempDir(), t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	sourceParent := filepath.Join(repoDir, "nested")
	movedParent := sourceParent + ".moved"
	name := "model-Q4_K_M.gguf"
	writeSelectionFile(t, sourceParent, name)
	selectedPath := filepath.Join(sourceParent, name)
	if err := os.Chmod(selectedPath, 0000); err != nil {
		t.Fatal(err)
	}
	if probe, err := os.Open(selectedPath); err == nil {
		_ = probe.Close()
		t.Skip("test process can still read mode-000 fixture")
	} else if !os.IsPermission(err) {
		t.Fatalf("open mode-000 fixture error=%v, want permission denied", err)
	}
	foreignMarker := filepath.Join(foreign, "keep.txt")
	if err := os.WriteFile(foreignMarker, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	s.selectedDeleteHooks.forceHardLinkPin = true
	s.selectedDeleteHooks.beforeHardLinkPin = func(source, _ string) error {
		if source != filepath.ToSlash(filepath.Join("nested", name)) {
			return nil
		}
		if err := os.Rename(sourceParent, movedParent); err != nil {
			return err
		}
		return os.Symlink(foreign, sourceParent)
	}
	body := selectedGroupRequest(t, s, "owner/model", repoDir, filepath.ToSlash(filepath.Join("nested", name)))
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "parent") {
		t.Fatalf("source-parent retarget status=%d body=%s, want diagnostic 409", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(movedParent, name)); err != nil {
		t.Fatalf("retarget removed selected entry from the held parent: %v", err)
	}
	if data, err := os.ReadFile(foreignMarker); err != nil || string(data) != "keep" {
		t.Fatalf("foreign parent target changed during cleanup: data=%q err=%v", data, err)
	}
	assertNoTemporaryPinContainer(t, movedParent)
}

func TestSelectedLocalDeleteHardLinkPinDetectsSameSizeReplacementBeforeFirstEffect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode-bit hard-link pin fixture is Unix-specific")
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	first, second := "model-Q4_K_M-00001-of-00002.gguf", "model-Q4_K_M-00002-of-00002.gguf"
	writeSelectionFile(t, repoDir, first)
	writeSelectionFile(t, repoDir, second)
	firstPath := filepath.Join(repoDir, first)
	if err := os.Chmod(firstPath, 0000); err != nil {
		t.Fatal(err)
	}
	if probe, err := os.Open(firstPath); err == nil {
		_ = probe.Close()
		t.Skip("test process can still read mode-000 fixture")
	} else if !os.IsPermission(err) {
		t.Fatalf("open mode-000 fixture error=%v, want permission denied", err)
	}
	originalInfo, err := os.Lstat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	s.selectedDeleteHooks.forceHardLinkPin = true
	body := selectedGroupRequest(t, s, "owner/model", repoDir, first)
	s.selectedDeleteHooks.beforeRemove = func(i int) {
		if i != 0 {
			return
		}
		if err := os.Remove(firstPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(firstPath, []byte("new!"), 0644); err != nil {
			t.Fatal(err)
		}
		replacementInfo, err := os.Lstat(firstPath)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(originalInfo, replacementInfo) {
			t.Fatal("replacement reused the hard-link-pinned inode")
		}
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("hard-link-pinned replacement status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if data, err := os.ReadFile(firstPath); err != nil || string(data) != "new!" {
		t.Fatalf("hard-link-pinned replacement was removed or altered: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, second)); err != nil {
		t.Fatalf("other shard changed: %v", err)
	}
	assertNoTemporaryPinContainer(t, repoDir)
}

func TestSelectedLocalDeleteHardLinkPinReportsLaterReplacementTruthfully(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode-bit hard-link pin fixture is Unix-specific")
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	sourceDir := filepath.Join(repoDir, "nested")
	first, second := "model-Q4_K_M-00001-of-00002.gguf", "model-Q4_K_M-00002-of-00002.gguf"
	writeSelectionFile(t, sourceDir, first)
	writeSelectionFile(t, sourceDir, second)
	firstPath, secondPath := filepath.Join(sourceDir, first), filepath.Join(sourceDir, second)
	if err := os.Chmod(firstPath, 0000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secondPath, 0000); err != nil {
		t.Fatal(err)
	}
	if probe, err := os.Open(secondPath); err == nil {
		_ = probe.Close()
		t.Skip("test process can still read mode-000 fixture")
	} else if !os.IsPermission(err) {
		t.Fatalf("open mode-000 fixture error=%v, want permission denied", err)
	}
	if err := os.Chmod(repoDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(repoDir, 0755); err != nil {
			t.Errorf("restore repo permissions for temporary fixture cleanup: %v", err)
		}
	})
	if err := os.Chmod(sourceDir, 0755); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Lstat(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	s.selectedDeleteHooks.forceHardLinkPin = true
	body := selectedGroupRequest(t, s, "owner/model", repoDir, "nested/"+first)
	s.selectedDeleteHooks.beforeRemove = func(i int) {
		if i != 1 {
			return
		}
		if err := os.Remove(secondPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(secondPath, []byte("new!"), 0644); err != nil {
			t.Fatal(err)
		}
		replacementInfo, err := os.Lstat(secondPath)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(originalInfo, replacementInfo) {
			t.Fatal("later replacement reused the hard-link-pinned inode")
		}
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("later hard-link-pinned replacement status=%d body=%s, want 207", w.Code, w.Body.String())
	}
	var result selectedDeleteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "nested/"+first || len(result.Remaining) != 1 || result.Remaining[0] != "nested/"+second {
		t.Fatalf("partial response does not match actual effects: %+v", result)
	}
	if data, err := os.ReadFile(secondPath); err != nil || string(data) != "new!" {
		t.Fatalf("later replacement was removed or altered: data=%q err=%v", data, err)
	}
	assertNoTemporaryPinContainer(t, sourceDir)
}

func TestSelectedLocalDeleteHardLinkPinSetupFailureRefusesAndCleans(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode-bit hard-link pin setup fixture is Unix-specific")
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	name := "model-Q4_K_M.gguf"
	writeSelectionFile(t, repoDir, name)
	selectedPath := filepath.Join(repoDir, name)
	if err := os.Chmod(selectedPath, 0000); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(selectedPath)
	if err == nil {
		_ = probe.Close()
		t.Skip("test process can still read mode-000 fixture")
	}
	if !os.IsPermission(err) {
		t.Fatalf("open mode-000 fixture error=%v, want permission denied", err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	s.selectedDeleteHooks.forceHardLinkPin = true
	s.selectedDeleteHooks.beforeHardLinkPin = func(string, string) error {
		return errors.New("injected unsupported hard-link pin")
	}
	body := selectedGroupRequest(t, s, "owner/model", repoDir, name)
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "injected unsupported hard-link pin") {
		t.Fatalf("hard-link setup refusal status=%d body=%s, want diagnostic 409", w.Code, w.Body.String())
	}
	if _, err := os.Stat(selectedPath); err != nil {
		t.Fatalf("failed pin setup changed selected file: %v", err)
	}
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hfdesk-delete-pin-") {
			t.Fatalf("temporary pin container leaked after setup refusal: %s", entry.Name())
		}
	}
}

func TestSelectedLocalDeleteDoesNotFollowRetargetedPinContainer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode-bit hard-link pin binding fixture is Unix-specific")
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	name := "model-Q4_K_M.gguf"
	writeSelectionFile(t, repoDir, name)
	selectedPath := filepath.Join(repoDir, name)
	if err := os.Chmod(selectedPath, 0000); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(selectedPath)
	if err == nil {
		_ = probe.Close()
		t.Skip("test process can still read mode-000 fixture")
	}
	if !os.IsPermission(err) {
		t.Fatalf("open mode-000 fixture error=%v, want permission denied", err)
	}
	foreign := filepath.Join(repoDir, "foreign")
	if err := os.Mkdir(foreign, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(foreign, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	s.selectedDeleteHooks.forceHardLinkPin = true
	s.selectedDeleteHooks.beforeHardLinkPin = func(_, destination string) error {
		container := filepath.Join(repoDir, filepath.Dir(filepath.FromSlash(destination)))
		moved := container + ".moved"
		if err := os.Rename(container, moved); err != nil {
			return err
		}
		return os.Symlink(foreign, container)
	}
	body := selectedGroupRequest(t, s, "owner/model", repoDir, name)
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "binding changed") || !strings.Contains(w.Body.String(), "possible artifact remains") {
		t.Fatalf("retargeted pin container status=%d body=%s, want diagnostic 409", w.Code, w.Body.String())
	}
	if _, err := os.Stat(selectedPath); err != nil {
		t.Fatalf("pin-container retarget changed selected file: %v", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatalf("foreign replacement directory changed: data=%q err=%v", data, err)
	}
}

func TestSelectedLocalDeleteReportsHardLinkCleanupFailureAfterEffects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode-bit hard-link pin cleanup fixture is Unix-specific")
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	name := "model-Q4_K_M.gguf"
	writeSelectionFile(t, repoDir, name)
	selectedPath := filepath.Join(repoDir, name)
	if err := os.Chmod(selectedPath, 0000); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(selectedPath)
	if err == nil {
		_ = probe.Close()
		t.Skip("test process can still read mode-000 fixture")
	}
	if !os.IsPermission(err) {
		t.Fatalf("open mode-000 fixture error=%v, want permission denied", err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	s.selectedDeleteHooks.forceHardLinkPin = true
	s.selectedDeleteHooks.removeHardLinkPin = func(*os.Root, string) error {
		return errors.New("injected hard-link cleanup failure")
	}
	body := selectedGroupRequest(t, s, "owner/model", repoDir, name)
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("cleanup failure after selected deletion status=%d body=%s, want 207", w.Code, w.Body.String())
	}
	var result selectedDeleteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || len(result.Removed) != 1 || result.Removed[0] != name || len(result.Remaining) != 0 || !strings.Contains(strings.Join(result.Errors, ";"), "injected hard-link cleanup failure") {
		t.Fatalf("cleanup-failure accounting is not truthful: %+v", result)
	}
	if _, err := os.Lstat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("selected entry was not removed: %v", err)
	}
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	var pinContainer string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hfdesk-delete-pin-") {
			pinContainer = entry.Name()
		}
	}
	if pinContainer == "" || !strings.Contains(strings.Join(result.Errors, ";"), pinContainer) {
		t.Fatalf("possible retained pin artifact was not identified in errors: entries=%v errors=%v", entries, result.Errors)
	}
	if release, ok := s.jobs.beginCacheMutation(repoDir); !ok {
		t.Fatal("writer reservation remained held after pin cleanup failure response")
	} else {
		release()
	}
}

func TestSelectedLocalDeleteReportsPinRootCloseFailureAfterEffects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode-bit hard-link pin fixture is Unix-specific")
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	name := "model-Q4_K_M.gguf"
	writeSelectionFile(t, repoDir, name)
	selectedPath := filepath.Join(repoDir, name)
	if err := os.Chmod(selectedPath, 0000); err != nil {
		t.Fatal(err)
	}
	if probe, err := os.Open(selectedPath); err == nil {
		_ = probe.Close()
		t.Skip("test process can still read mode-000 fixture")
	} else if !os.IsPermission(err) {
		t.Fatalf("open mode-000 fixture error=%v, want permission denied", err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	s.selectedDeleteHooks.forceHardLinkPin = true
	s.selectedDeleteHooks.closeHardLinkRoot = func(root *os.Root) error {
		if err := root.Close(); err != nil {
			return err
		}
		return errors.New("injected container close failure")
	}
	body := selectedGroupRequest(t, s, "owner/model", repoDir, name)
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("container close failure status=%d body=%s, want 207", w.Code, w.Body.String())
	}
	var result selectedDeleteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || len(result.Removed) != 1 || result.Removed[0] != name || len(result.Remaining) != 0 || !strings.Contains(strings.Join(result.Errors, ";"), "injected container close failure") {
		t.Fatalf("container close failure accounting is not truthful: %+v", result)
	}
	if _, err := os.Lstat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("selected entry was not removed: %v", err)
	}
	assertNoTemporaryPinContainer(t, repoDir)
	if release, ok := s.jobs.beginCacheMutation(repoDir); !ok {
		t.Fatal("writer reservation remained held after container close failure response")
	} else {
		release()
	}
}

func TestSelectedLocalDeleteReportsRenamedHardLinkPinArtifact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode-bit hard-link pin fixture is Unix-specific")
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	name := "model-Q4_K_M.gguf"
	writeSelectionFile(t, repoDir, name)
	selectedPath := filepath.Join(repoDir, name)
	if err := os.Chmod(selectedPath, 0000); err != nil {
		t.Fatal(err)
	}
	if probe, err := os.Open(selectedPath); err == nil {
		_ = probe.Close()
		t.Skip("test process can still read mode-000 fixture")
	} else if !os.IsPermission(err) {
		t.Fatalf("open mode-000 fixture error=%v, want permission denied", err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	s.selectedDeleteHooks.forceHardLinkPin = true
	body := selectedGroupRequest(t, s, "owner/model", repoDir, name)
	s.selectedDeleteHooks.beforeRemove = func(i int) {
		if i != 0 {
			return
		}
		entries, err := os.ReadDir(repoDir)
		if err != nil {
			t.Fatal(err)
		}
		var container string
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".hfdesk-delete-pin-") {
				container = filepath.Join(repoDir, entry.Name())
				break
			}
		}
		if container == "" {
			t.Fatal("temporary hard-link pin container was not created")
		}
		pins, err := os.ReadDir(container)
		if err != nil || len(pins) != 1 {
			t.Fatalf("temporary hard-link pin contents=%v err=%v", pins, err)
		}
		if err := os.Rename(filepath.Join(container, pins[0].Name()), filepath.Join(container, "moved-pin.entry")); err != nil {
			t.Fatal(err)
		}
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("renamed pin artifact status=%d body=%s, want 207", w.Code, w.Body.String())
	}
	var result selectedDeleteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || len(result.Removed) != 1 || result.Removed[0] != name || len(result.Remaining) != 0 || !strings.Contains(strings.Join(result.Errors, ";"), "possible renamed artifact remains") {
		t.Fatalf("renamed pin artifact accounting is incorrect: %+v", result)
	}
	if _, err := os.Lstat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("selected entry was not removed: %v", err)
	}
	if release, ok := s.jobs.beginCacheMutation(repoDir); !ok {
		t.Fatal("writer reservation remained held after renamed-pin cleanup response")
	} else {
		release()
	}
}

func TestSelectedLocalDeleteReportsPinCleanupFailureBeforeEffects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode-bit hard-link pin cleanup fixture is Unix-specific")
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	name := "model-Q4_K_M.gguf"
	writeSelectionFile(t, repoDir, name)
	selectedPath := filepath.Join(repoDir, name)
	if err := os.Chmod(selectedPath, 0000); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(selectedPath)
	if err == nil {
		_ = probe.Close()
		t.Skip("test process can still read mode-000 fixture")
	}
	if !os.IsPermission(err) {
		t.Fatalf("open mode-000 fixture error=%v, want permission denied", err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	s.selectedDeleteHooks.forceHardLinkPin = true
	s.selectedDeleteHooks.afterFinalScan = func() {
		if err := os.Chmod(selectedPath, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(selectedPath, []byte("growth!"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	s.selectedDeleteHooks.removeHardLinkPin = func(*os.Root, string) error {
		return errors.New("injected refusal pin cleanup failure")
	}
	body := selectedGroupRequest(t, s, "owner/model", repoDir, name)
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "injected refusal pin cleanup failure") || !strings.Contains(w.Body.String(), "possible artifact remains") || !strings.Contains(w.Body.String(), ".hfdesk-delete-pin-") {
		t.Fatalf("pre-effect cleanup failure status=%d body=%s, want diagnostic 409", w.Code, w.Body.String())
	}
	data, err := os.ReadFile(selectedPath)
	if err != nil || string(data) != "growth!" {
		t.Fatalf("pre-effect refusal changed selected file: data=%q err=%v", data, err)
	}
}

func TestSelectedLocalDeleteDetectsRenamedSameSizeReplacementBeforeFirstUnlink(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	first, second := "model-Q4_K_M-00001-of-00002.gguf", "model-Q4_K_M-00002-of-00002.gguf"
	writeSelectionFile(t, repoDir, first)
	writeSelectionFile(t, repoDir, second)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	body := selectedGroupRequest(t, s, "owner/model", repoDir, first)
	s.selectedDeleteHooks.beforeRemove = func(i int) {
		if i != 0 {
			return
		}
		path := filepath.Join(repoDir, first)
		replacement := path + ".replacement"
		if err := os.WriteFile(replacement, []byte("new!"), 0644); err != nil {
			t.Fatal(err)
		}
		requireReplacementFixtureOperation(t, os.Remove(path))
		requireReplacementFixtureOperation(t, os.Rename(replacement, path))
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("renamed same-size replacement status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if data, err := os.ReadFile(filepath.Join(repoDir, first)); err != nil || string(data) != "new!" {
		t.Fatalf("renamed replacement was removed or altered: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, second)); err != nil {
		t.Fatalf("unrelated shard changed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedLocalDeleteReportsPartialAfterLaterMemberReplacement(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	first, second := "model-Q4_K_M-00001-of-00002.gguf", "model-Q4_K_M-00002-of-00002.gguf"
	writeSelectionFile(t, repoDir, first)
	writeSelectionFile(t, repoDir, second)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	body := selectedGroupRequest(t, s, "owner/model", repoDir, first)
	originalInfo, err := os.Lstat(filepath.Join(repoDir, second))
	if err != nil {
		t.Fatal(err)
	}
	s.selectedDeleteHooks.beforeRemove = func(i int) {
		if i != 1 {
			return
		}
		path := filepath.Join(repoDir, second)
		requireReplacementFixtureOperation(t, os.Remove(path))
		requireReplacementFixtureOperation(t, os.WriteFile(path, []byte("new!"), 0644))
		replacementInfo, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(originalInfo, replacementInfo) {
			t.Fatal("later replacement reused the selected inode despite the retained identity pin")
		}
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("later replacement status=%d body=%s, want 207", w.Code, w.Body.String())
	}
	var result selectedDeleteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != first || len(result.Remaining) != 1 || result.Remaining[0] != second {
		t.Fatalf("partial response does not match actual effects: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(repoDir, first)); !os.IsNotExist(err) {
		t.Fatalf("first shard was not removed before the later replacement: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(repoDir, second)); err != nil || string(data) != "new!" {
		t.Fatalf("replacement was removed or altered: data=%q err=%v", data, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedLocalDeleteReportsPartialAfterLaterRenameReplacement(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	first, second := "model-Q4_K_M-00001-of-00002.gguf", "model-Q4_K_M-00002-of-00002.gguf"
	writeSelectionFile(t, repoDir, first)
	writeSelectionFile(t, repoDir, second)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	body := selectedGroupRequest(t, s, "owner/model", repoDir, first)
	s.selectedDeleteHooks.beforeRemove = func(i int) {
		if i != 1 {
			return
		}
		path := filepath.Join(repoDir, second)
		replacement := path + ".replacement"
		if err := os.WriteFile(replacement, []byte("new!"), 0644); err != nil {
			t.Fatal(err)
		}
		requireReplacementFixtureOperation(t, os.Remove(path))
		requireReplacementFixtureOperation(t, os.Rename(replacement, path))
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("later rename replacement status=%d body=%s, want 207", w.Code, w.Body.String())
	}
	var result selectedDeleteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != first || len(result.Remaining) != 1 || result.Remaining[0] != second {
		t.Fatalf("partial response does not match actual effects: %+v", result)
	}
	if data, err := os.ReadFile(filepath.Join(repoDir, second)); err != nil || string(data) != "new!" {
		t.Fatalf("renamed replacement was removed or altered: data=%q err=%v", data, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedLocalDeleteReportsInjectedUnlinkFailureTruthfully(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	first, second := "model-Q4_K_M-00001-of-00002.gguf", "model-Q4_K_M-00002-of-00002.gguf"
	writeSelectionFile(t, repoDir, first)
	writeSelectionFile(t, repoDir, second)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	body := selectedGroupRequest(t, s, "owner/model", repoDir, first)
	s.selectedDeleteHooks.removeEntry = func(root *os.Root, name string) error {
		if name == filepath.FromSlash(second) {
			return errors.New("injected unlink failure")
		}
		return root.Remove(name)
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("unlink failure status=%d body=%s, want 207", w.Code, w.Body.String())
	}
	var result selectedDeleteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != first || len(result.Remaining) != 1 || result.Remaining[0] != second || len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "injected unlink failure") {
		t.Fatalf("unlink failure response does not match actual effects: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(repoDir, first)); !os.IsNotExist(err) {
		t.Fatalf("first shard was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, second)); err != nil {
		t.Fatalf("failed unlink unexpectedly removed second shard: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedCacheDeleteRejectsStaleMembersAndDeletesHFLocation(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	writeSelectionFile(t, repoDir, "model-Q4_K_M.gguf")
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	_, selection := getSelection(t, s, "owner/model", "model", "")
	loc := selection.Locations[0]
	g := loc.Groups[0]
	stale, _ := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: loc.ID, GroupID: g.ID, Members: nil})
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(stale))
	if w.Code == 200 {
		t.Fatal("stale selection unexpectedly deleted the file")
	}
	if _, err := os.Stat(filepath.Join(repoDir, "model-Q4_K_M.gguf")); err != nil {
		t.Fatalf("stale request changed file: %v", err)
	}
	rd, err := s.snapshotConfig().cache().Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	hfFile := filepath.Join(rd.SnapshotsDir(), "version-1", "model-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(hfFile), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hfFile, []byte("hf"), 0644); err != nil {
		t.Fatal(err)
	}
	_, withHF := getSelection(t, s, "owner/model", "model", "")
	var hfLocation cacheSelectionLocation
	for _, candidate := range withHF.Locations {
		if strings.HasPrefix(candidate.ID, "hf-") {
			hfLocation = candidate
		}
	}
	if hfLocation.ID == "" || len(hfLocation.Groups) == 0 {
		t.Fatalf("HF selection fixture missing: %+v", withHF.Locations)
	}
	hfGroup := hfLocation.Groups[0]
	hfRequest, _ := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: hfLocation.ID, GroupID: hfGroup.ID, Members: hfGroup.Members})
	w = cacheRequest(t, s, "DELETE", "/api/cache-selection", string(hfRequest))
	if w.Code != http.StatusOK {
		t.Fatalf("HF selection delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(hfFile); !os.IsNotExist(err) {
		t.Fatalf("selected HF entry remains, err=%v", err)
	}
}

func TestSelectedHFDeletePreservesSharedPayloadAndOtherQuant(t *testing.T) {
	cacheRoot := t.TempDir()
	s := newTestServerWithConfig(t, Config{CacheDir: cacheRoot})
	rd, err := s.snapshotConfig().cache().Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	store := func(version, name, contents string) *hfdownloader.StoreFileResult {
		t.Helper()
		temp := filepath.Join(t.TempDir(), "download")
		if err := os.WriteFile(temp, []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
		got, err := rd.StoreDownloadedFile(temp, name, version, "", "", false)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	q4a := store("version-a", "model-Q4_K_M.gguf", "shared payload")
	store("version-b", "model-Q4_K_M.gguf", "shared payload")
	q5 := store("version-b", "model-Q5_K_M.gguf", "shared payload")
	unique := store("version-b", "model-Q3_K.gguf", "unshared payload")
	unrelatedDownloadStage := rd.BlobPath("tmp-" + strings.Repeat("9", 64))
	if err := os.WriteFile(unrelatedDownloadStage, []byte("unrelated download staging"), 0644); err != nil {
		t.Fatal(err)
	}
	unrelatedCopyStage, err := os.CreateTemp(rd.BlobsDir(), strings.Repeat("8", 64)+".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	unrelatedCopyStagePath := unrelatedCopyStage.Name()
	if _, err := unrelatedCopyStage.Write([]byte("unrelated copy staging")); err != nil {
		t.Fatal(err)
	}
	if err := unrelatedCopyStage.Close(); err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(rd.FriendlyPath(), "README.md")
	if err := os.MkdirAll(filepath.Dir(readme), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readme, []byte("ordinary friendly file"), 0644); err != nil {
		t.Fatal(err)
	}
	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK {
		t.Fatalf("selection status=%d", code)
	}
	var location *cacheSelectionLocation
	for i := range selection.Locations {
		if strings.HasPrefix(selection.Locations[i].ID, "hf-") {
			location = &selection.Locations[i]
		}
	}
	if location == nil {
		t.Fatalf("HF location missing: %+v", selection.Locations)
	}
	var group *cacheSelectionGroup
	for i := range location.Groups {
		if location.Groups[i].Members[0].Path == "model-Q4_K_M.gguf" {
			group = &location.Groups[i]
		}
	}
	if group == nil || len(group.Members) != 2 || len(group.Members[0].Versions) != 1 || len(group.Members[1].Versions) != 1 {
		t.Fatalf("Q4 group should cover both snapshots: %+v", location.Groups)
	}
	body, _ := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: location.ID, GroupID: group.ID, Members: group.Members})
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("HF delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(q4a.BlobPath); err != nil {
		t.Fatalf("shared payload removed despite Q5 reference: %v", err)
	}
	if _, err := os.Stat(q5.SnapshotPath); err != nil {
		t.Fatalf("other quant snapshot changed: %v", err)
	}
	if _, err := os.Stat(q5.FriendlyPath); err != nil {
		t.Fatalf("other quant friendly link changed: %v", err)
	}
	for _, name := range []string{unrelatedDownloadStage, unrelatedCopyStagePath} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("unrelated producer staging was removed by completed-group deletion: %v", err)
		}
	}
	if _, err := os.Lstat(q4a.SnapshotPath); !os.IsNotExist(err) {
		t.Fatalf("selected Q4 snapshot remains: %v", err)
	}
	if _, err := os.Lstat(q4a.FriendlyPath); !os.IsNotExist(err) {
		t.Fatalf("selected Q4 friendly entry remains: %v", err)
	}
	if got, err := os.ReadFile(readme); err != nil || string(got) != "ordinary friendly file" {
		t.Fatalf("ordinary friendly file changed: content=%q err=%v", got, err)
	}
	_, selection = getSelection(t, s, "owner/model", "model", location.ID)
	location = nil
	for i := range selection.Locations {
		if strings.HasPrefix(selection.Locations[i].ID, "hf-") {
			location = &selection.Locations[i]
		}
	}
	if location == nil {
		t.Fatal("refreshed HF location missing")
	}
	var q3 *cacheSelectionGroup
	for i := range location.Groups {
		if location.Groups[i].Members[0].Path == "model-Q3_K.gguf" {
			q3 = &location.Groups[i]
		}
	}
	if q3 == nil {
		t.Fatal("unique payload group missing")
	}
	body, _ = json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: location.ID, GroupID: q3.ID, Members: q3.Members})
	w = cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("unique HF delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(unique.BlobPath); !os.IsNotExist(err) {
		t.Fatalf("unreferenced selected payload remains, err=%v", err)
	}
}

func TestSelectedHFDeleteRetainsPayloadForDirectFriendlyBlobLink(t *testing.T) {
	s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})
	rd, err := s.snapshotConfig().cache().Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(t.TempDir(), "download")
	if err := os.WriteFile(temp, []byte("shared payload"), 0644); err != nil {
		t.Fatal(err)
	}
	stored, err := rd.StoreDownloadedFile(temp, "model-Q4_K_M.gguf", "version-a", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	blobView := filepath.Join(rd.FriendlyPath(), "blob-view")
	symlinkOrSkip(t, rd.BlobsDir(), blobView)
	otherName := filepath.Join(rd.FriendlyPath(), "model-Q5_K_M.gguf")
	symlinkOrSkip(t, filepath.Join(blobView, filepath.Base(stored.BlobPath)), otherName)
	body := selectedHFGroupRequest(t, s, "model-Q4_K_M.gguf")
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"retainedPayloads"`) || !strings.Contains(w.Body.String(), "shared payloads needed") {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(stored.BlobPath); err != nil {
		t.Fatalf("payload needed by retained friendly blob link was removed: %v", err)
	}
	if _, err := os.Lstat(otherName); err != nil {
		t.Fatalf("unselected friendly blob link changed: %v", err)
	}
	if _, err := os.Stat(otherName); err != nil {
		t.Fatalf("unselected friendly blob alias no longer resolves: %v", err)
	}
	if _, err := os.Lstat(stored.SnapshotPath); !os.IsNotExist(err) {
		t.Fatalf("selected snapshot entry remains: %v", err)
	}
}

func TestSelectedHFDeleteRefusesRetainedFriendlySnapshotDependency(t *testing.T) {
	s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})
	rd, err := s.snapshotConfig().cache().Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(rd.SnapshotsDir(), "version-a", "model-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(snapshot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, []byte("selected regular snapshot"), 0644); err != nil {
		t.Fatal(err)
	}
	friendly := filepath.Join(rd.FriendlyPath(), "model-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(friendly), 0755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, snapshot, friendly)
	snapshotView := filepath.Join(rd.FriendlyPath(), "snapshot-view")
	symlinkOrSkip(t, filepath.Dir(snapshot), snapshotView)
	otherName := filepath.Join(rd.FriendlyPath(), "model-Q5_K_M.gguf")
	symlinkOrSkip(t, filepath.Join(snapshotView, filepath.Base(snapshot)), otherName)
	body := selectedHFGroupRequest(t, s, "model-Q4_K_M.gguf")
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "HF deletion was partial") || !strings.Contains(w.Body.String(), "retained friendly name depends on a selected snapshot entry") {
		t.Fatalf("dependent friendly name should refuse deletion, status=%d body=%s", w.Code, w.Body.String())
	}
	for _, name := range []string{snapshot, friendly, snapshotView, otherName} {
		if _, err := os.Lstat(name); err != nil {
			t.Fatalf("refused operation changed %s: %v", name, err)
		}
	}
}

func TestSelectedHFDeleteReportsPartialAfterUnlinkAttempt(t *testing.T) {
	s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})
	rd, err := s.snapshotConfig().cache().Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(t.TempDir(), "download")
	if err := os.WriteFile(temp, []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}
	stored, err := rd.StoreDownloadedFile(temp, "model-Q4_K_M.gguf", "version-a", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	snapshotDir := filepath.Dir(stored.SnapshotPath)
	if err := os.Chmod(snapshotDir, 0555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(snapshotDir, 0755)
	body := selectedHFGroupRequest(t, s, "model-Q4_K_M.gguf")
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusMultiStatus || !strings.Contains(w.Body.String(), "HF deletion was partial") || !strings.Contains(w.Body.String(), "remaining") {
		t.Fatalf("failed unlink should be reported as partial execution, status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(stored.SnapshotPath); err != nil {
		t.Fatalf("failed snapshot unlink unexpectedly removed entry: %v", err)
	}
	if _, err := os.Stat(stored.BlobPath); err != nil {
		t.Fatalf("payload needed by remaining snapshot was removed: %v", err)
	}
}

func TestSelectedHFDeleteRefusesIncompleteBlobTargetWithConflict(t *testing.T) {
	for _, target := range []string{"data", "metadata"} {
		t.Run(target, func(t *testing.T) {
			s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})
			rd, err := s.snapshotConfig().cache().Repo("owner/model", hfdownloader.RepoTypeModel)
			if err != nil {
				t.Fatal(err)
			}
			if err := rd.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			temp := filepath.Join(t.TempDir(), "download")
			if err := os.WriteFile(temp, []byte("completed payload"), 0644); err != nil {
				t.Fatal(err)
			}
			stored, err := rd.StoreDownloadedFile(temp, "model-Q4_K_M.gguf", "version-a", "", "", false)
			if err != nil {
				t.Fatal(err)
			}
			partial := rd.IncompletePath("staging")
			partialBytes := []byte("partial bytes")
			if err := os.WriteFile(partial, partialBytes, 0644); err != nil {
				t.Fatal(err)
			}
			meta := rd.IncompleteMetaPath("staging")
			metaBytes := []byte("resume metadata")
			if err := os.WriteFile(meta, metaBytes, 0644); err != nil {
				t.Fatal(err)
			}
			selectedTarget := partial
			if target == "metadata" {
				selectedTarget = meta
			}
			if err := os.Remove(stored.SnapshotPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("..", "..", "blobs", filepath.Base(selectedTarget)), stored.SnapshotPath); err != nil {
				t.Fatal(err)
			}
			// Build the selection from the exact staging-backed composition so its
			// member size matches the request-time validation.
			body := selectedHFGroupRequest(t, s, "model-Q4_K_M.gguf")
			w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "HF deletion refused") || !strings.Contains(w.Body.String(), "selected snapshot link targets an incomplete blob") || strings.Contains(w.Body.String(), "HF deletion was partial") {
				t.Fatalf("staging target should be refused by preflight, status=%d body=%s", w.Code, w.Body.String())
			}
			for _, name := range []string{stored.SnapshotPath, stored.BlobPath, partial, meta, stored.FriendlyPath} {
				if _, err := os.Lstat(name); err != nil {
					t.Fatalf("refused request changed %s: %v", name, err)
				}
			}
			for _, check := range []struct {
				name string
				want []byte
			}{{partial, partialBytes}, {meta, metaBytes}, {stored.BlobPath, []byte("completed payload")}} {
				got, err := os.ReadFile(check.name)
				if err != nil || string(got) != string(check.want) {
					t.Fatalf("refused request changed contents of %s: got=%q err=%v", check.name, got, err)
				}
			}
		})
	}
}

func TestSelectedHFDeleteRefusesProducerStagingWithFreshConfirmation(t *testing.T) {
	for _, stagingKind := range []string{"download-target", "publication-copy"} {
		t.Run(stagingKind, func(t *testing.T) {
			s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})
			rd, err := s.snapshotConfig().cache().Repo("owner/model", hfdownloader.RepoTypeModel)
			if err != nil {
				t.Fatal(err)
			}
			if err := rd.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			members := []string{"model-Q4_K_M-00001-of-00002.gguf", "model-Q4_K_M-00002-of-00002.gguf"}
			stored := make([]*hfdownloader.StoreFileResult, 0, len(members))
			for i, member := range members {
				temp := filepath.Join(t.TempDir(), "download")
				payload := fmt.Sprintf("completed shard %d", i+1)
				if err := os.WriteFile(temp, []byte(payload), 0644); err != nil {
					t.Fatal(err)
				}
				entry, err := rd.StoreDownloadedFile(temp, member, "version-a", strings.Repeat(fmt.Sprintf("%x", i+1), 64), "", false)
				if err != nil {
					t.Fatal(err)
				}
				stored = append(stored, entry)
			}
			if err := rd.WriteRef("main", "version-a"); err != nil {
				t.Fatal(err)
			}
			stagingBytes := []byte("still being produced")
			stagingName := "tmp-" + strings.Repeat("d", 64)
			if stagingKind == "publication-copy" {
				staged, err := os.CreateTemp(rd.BlobsDir(), strings.Repeat("e", 64)+".tmp-*")
				if err != nil {
					t.Fatal(err)
				}
				stagingName = filepath.Base(staged.Name())
				if _, err := staged.Write(stagingBytes); err != nil {
					t.Fatal(err)
				}
				if err := staged.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(rd.BlobPath(stagingName), stagingBytes, 0644); err != nil {
				t.Fatal(err)
			}
			stageIndex := 0
			if stagingKind == "publication-copy" {
				stageIndex = 1
			}
			stageSnapshot := stored[stageIndex].SnapshotPath
			if err := os.Remove(stageSnapshot); err != nil {
				t.Fatal(err)
			}
			stageLink := filepath.Join("..", "..", "blobs", stagingName)
			if err := os.Symlink(stageLink, stageSnapshot); err != nil {
				t.Fatal(err)
			}
			friendlyLinks := make([]string, len(stored))
			snapshotLinks := make([]string, len(stored))
			for i, entry := range stored {
				friendlyLinks[i], err = os.Readlink(entry.FriendlyPath)
				if err != nil {
					t.Fatal(err)
				}
				snapshotLinks[i], err = os.Readlink(entry.SnapshotPath)
				if err != nil {
					t.Fatal(err)
				}
			}

			// Confirmation is generated only after the mixed completed/staging
			// composition exists, so a stale-size conflict cannot satisfy this case.
			body := selectedHFGroupRequest(t, s, members[0])
			w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "HF deletion refused") || !strings.Contains(w.Body.String(), "selected snapshot link targets an incomplete blob") || strings.Contains(w.Body.String(), "Selection is stale") {
				t.Fatalf("freshly confirmed producer staging composition was not specifically refused: status=%d body=%s", w.Code, w.Body.String())
			}
			for i, entry := range stored {
				if _, err := os.Lstat(entry.SnapshotPath); err != nil {
					t.Fatalf("snapshot %s changed: %v", entry.SnapshotPath, err)
				}
				if _, err := os.Lstat(entry.FriendlyPath); err != nil {
					t.Fatalf("friendly link %s changed: %v", entry.FriendlyPath, err)
				}
				want := fmt.Sprintf("completed shard %d", i+1)
				target, err := os.Readlink(entry.SnapshotPath)
				if err != nil || target != snapshotLinks[i] {
					t.Fatalf("snapshot link changed: target=%q want=%q err=%v", target, snapshotLinks[i], err)
				}
				if data, err := os.ReadFile(entry.BlobPath); err != nil || string(data) != want {
					t.Fatalf("blob %s changed: data=%q err=%v", entry.BlobPath, data, err)
				}
				friendlyTarget, err := os.Readlink(entry.FriendlyPath)
				if err != nil || friendlyTarget != friendlyLinks[i] {
					t.Fatalf("friendly link text changed after refusal: got=%q want=%q err=%v", friendlyTarget, friendlyLinks[i], err)
				}
			}
			if data, err := os.ReadFile(rd.BlobPath(stagingName)); err != nil || string(data) != string(stagingBytes) {
				t.Fatalf("staging target changed: data=%q err=%v", data, err)
			}
			if ref, err := rd.ReadRef("main"); err != nil || ref != "version-a" {
				t.Fatalf("ref changed after refused deletion: ref=%q err=%v", ref, err)
			}
		})
	}
}

func selectedHFGroupRequest(t *testing.T, s *Server, memberPath string) []byte {
	t.Helper()
	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK {
		t.Fatalf("selection status=%d", code)
	}
	for _, loc := range selection.Locations {
		if !strings.HasPrefix(loc.ID, "hf-") {
			continue
		}
		for _, group := range loc.Groups {
			for _, member := range group.Members {
				if member.Path == memberPath {
					body, err := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: loc.ID, GroupID: group.ID, Members: group.Members})
					if err != nil {
						t.Fatal(err)
					}
					return body
				}
			}
		}
	}
	t.Fatalf("HF member %q missing", memberPath)
	return nil
}

func TestHFSelectionExcludesConfiguredNestedSnapshotRoot(t *testing.T) {
	cacheRoot := t.TempDir()
	cache := Config{CacheDir: cacheRoot}.cache()
	rd, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(rd.SnapshotsDir(), "version-a", "library")
	writeSelectionFile(t, nested, "vendor-Q4_K_M.gguf")
	writeSelectionFile(t, filepath.Join(rd.SnapshotsDir(), "version-a"), "model-Q5_K_M.gguf")
	alias := filepath.Join(t.TempDir(), "nested-root")
	if err := os.Symlink(nested, alias); err != nil {
		if runtime.GOOS == "windows" && os.IsPermission(err) {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: cacheRoot, LocalScanDirs: []string{alias}})
	_, got := getSelection(t, s, "owner/model", "model", "")
	for _, loc := range got.Locations {
		if !strings.HasPrefix(loc.ID, "hf-") {
			continue
		}
		for _, group := range loc.Groups {
			for _, member := range group.Members {
				if strings.Contains(member.Path, "library/") || strings.Contains(member.Path, "vendor-Q4") {
					t.Fatalf("configured local subtree was included in the outer HF selection: %+v", member)
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(nested, "vendor-Q4_K_M.gguf")); err != nil {
		t.Fatalf("configured descendant file changed during selection: %v", err)
	}
	body := selectedHFGroupRequest(t, s, "model-Q5_K_M.gguf")
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("outer HF group deletion status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(nested, "vendor-Q4_K_M.gguf")); err != nil {
		t.Fatalf("configured descendant file changed by outer deletion: %v", err)
	}
}

func TestHFSelectionReservationProtectsRepositoryAndFriendlyWriters(t *testing.T) {
	cfg := Config{CacheDir: t.TempDir()}
	cache := cfg.cache()
	rd, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithConfig(t, cfg)
	release, ok := s.jobs.reserveSelectedHF(rd.Path(), rd.FriendlyPath())
	if !ok {
		t.Fatal("HF reservation failed")
	}
	defer release()
	sameRepo := &Job{Repo: "owner/model", OutputDir: cache.Root, HubDir: cache.HubDir()}
	if !s.jobs.jobBlockedByDeleteLocked(sameRepo) {
		t.Fatal("same-repository HF writer was not blocked")
	}
	otherRepo := &Job{Repo: "owner/other", OutputDir: cache.Root, HubDir: cache.HubDir()}
	if s.jobs.jobBlockedByDeleteLocked(otherRepo) {
		t.Fatal("unrelated HF repository was blocked")
	}
	otherHubSharedFriendly := &Job{Repo: "owner/model", OutputDir: cache.Root, HubDir: filepath.Join(t.TempDir(), "hub")}
	if !s.jobs.jobBlockedByDeleteLocked(otherHubSharedFriendly) {
		t.Fatal("writer to selected friendly view was not blocked")
	}
	localSibling := &Job{Repo: "owner/model", OutputDir: filepath.Join(t.TempDir(), "local"), LocalDir: filepath.Join(t.TempDir(), "local"), Flat: true}
	if s.jobs.jobBlockedByDeleteLocked(localSibling) {
		t.Fatal("unrelated local writer was blocked")
	}
	queuedServer := newTestServerWithConfig(t, cfg)
	queuedServer.jobs.mu.Lock()
	queuedServer.jobs.jobs["queued-same-hub"] = &Job{ID: "queued-same-hub", Repo: "owner/model", OutputDir: cache.Root, HubDir: cache.HubDir(), Status: JobStatusQueued}
	queuedServer.jobs.mu.Unlock()
	if _, ok := queuedServer.jobs.reserveSelectedHF(rd.Path(), rd.FriendlyPath()); ok {
		t.Fatal("queued same-repository writer did not block HF deletion")
	}
	otherRepoServer := newTestServerWithConfig(t, cfg)
	otherRepoServer.jobs.mu.Lock()
	otherRepoServer.jobs.jobs["queued-other-repo"] = &Job{ID: "queued-other-repo", Repo: "owner/other", OutputDir: cache.Root, HubDir: cache.HubDir(), Status: JobStatusQueued}
	otherRepoServer.jobs.mu.Unlock()
	otherRelease, ok := otherRepoServer.jobs.reserveSelectedHF(rd.Path(), rd.FriendlyPath())
	if !ok {
		t.Fatal("queued unrelated HF repository blocked selected deletion")
	}
	// The synthetic queued record exists only to exercise reservation
	// admission. Remove it before releasing the reservation, which dispatches
	// queued jobs; a real dispatched job must be created through CreateJob so
	// its run-lifecycle synchronization fields are initialized.
	otherRepoServer.jobs.mu.Lock()
	delete(otherRepoServer.jobs.jobs, "queued-other-repo")
	otherRepoServer.jobs.mu.Unlock()
	otherRelease()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := otherRepoServer.jobs.Close(ctx); err != nil {
		t.Fatalf("close unrelated queued-job test manager: %v", err)
	}
}

func TestHFReservationUsesFrozenDestinationAndBothSelectedScopes(t *testing.T) {
	cfg := Config{CacheDir: t.TempDir()}
	cache := cfg.cache()
	rd, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	for name, job := range map[string]*Job{
		"localRepo in the selected Hub": {
			Repo: "upstream/source", LocalRepo: "owner/model", OutputDir: cache.Root, HubDir: cache.HubDir(),
		},
		"old friendly root but same Hub repository": {
			Repo: "owner/model", OutputDir: t.TempDir(), HubDir: cache.HubDir(),
		},
		"flat actual destination inside selected repository": {
			Repo: "different/source", LocalRepo: "inside", OutputDir: t.TempDir(), LocalDir: rd.Path(), Flat: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if !hfReservationConflictsJob([]string{rd.Path(), rd.FriendlyPath()}, job) {
				t.Fatal("writer to selected HF state was not blocked")
			}
		})
	}
	otherDataset, err := cache.Repo("owner/model", hfdownloader.RepoTypeDataset)
	if err != nil {
		t.Fatal(err)
	}
	dataset := &Job{Repo: "upstream/source", LocalRepo: "owner/model", IsDataset: true, OutputDir: cache.Root, HubDir: cache.HubDir()}
	if hfReservationConflictsJob([]string{rd.Path(), rd.FriendlyPath()}, dataset) {
		t.Fatal("dataset actual namespace was mistaken for the selected model namespace")
	}
	if !hfReservationConflictsJob([]string{otherDataset.Path(), filepath.Join(cache.DatasetsDir(), "owner", "model")}, dataset) {
		t.Fatal("dataset LocalRepo destination was not recognized")
	}
}

func TestSelectedDeleteUnlinksOnlyConfirmedFriendlyLeaf(t *testing.T) {
	root, payloadDir := t.TempDir(), t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(payloadDir, "weights.gguf")
	if err := os.WriteFile(payload, []byte("weight payload"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(repoDir, "model-Q4_K_M.gguf")
	symlinkOrSkip(t, payload, link)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	body := selectedGroupRequest(t, s, "owner/model", root, "model-Q4_K_M.gguf")
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "without removing their weight targets") {
		t.Fatalf("link-only delete response status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("selected link remains, err=%v", err)
	}
	if _, err := os.Stat(payload); err != nil {
		t.Fatalf("link target was changed: %v", err)
	}
}

func TestSelectedDeleteDetectsReplacedLinkEntryAndPreservesBothTargets(t *testing.T) {
	root, payloadDir := t.TempDir(), t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	firstTarget := filepath.Join(payloadDir, "first.gguf")
	secondTarget := filepath.Join(payloadDir, "second.gguf")
	if err := os.WriteFile(firstTarget, []byte("payload!"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondTarget, []byte("payload!"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(repoDir, "model-Q4_K_M.gguf")
	symlinkOrSkip(t, firstTarget, link)
	originalLinkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	registerJobManagerCleanup(t, s)
	body := selectedGroupRequest(t, s, "owner/model", repoDir, filepath.Base(link))
	s.selectedDeleteHooks.beforeRemove = func(i int) {
		if i != 0 {
			return
		}
		requireReplacementFixtureOperation(t, os.Remove(link))
		requireReplacementFixtureOperation(t, os.Symlink(secondTarget, link))
		replacementInfo, err := os.Lstat(link)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(originalLinkInfo, replacementInfo) {
			t.Fatal("replacement symlink reused the pinned link identity")
		}
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("replaced link status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if target, err := os.Readlink(link); err != nil || target != secondTarget {
		t.Fatalf("replacement link changed: target=%q err=%v", target, err)
	}
	for _, target := range []string{firstTarget, secondTarget} {
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("link deletion changed target %s: %v", target, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedDeleteRemovesOnlyExactSplitMembersAndKeepsDirectories(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	writeSelectionFile(t, repoDir, "split/model-Q4_K_M-00001-of-00002.gguf")
	writeSelectionFile(t, repoDir, "split/model-Q4_K_M-00002-of-00002.gguf")
	writeSelectionFile(t, repoDir, "model-Q4_K_M.gguf")
	writeSelectionFile(t, repoDir, "model-Q5_K_M.gguf")
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	body := selectedGroupRequest(t, s, "owner/model", root, "split/model-Q4_K_M-00001-of-00002.gguf")
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("split delete status=%d body=%s", w.Code, w.Body.String())
	}
	for _, name := range []string{"split/model-Q4_K_M-00001-of-00002.gguf", "split/model-Q4_K_M-00002-of-00002.gguf"} {
		if _, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Errorf("split member %s remains, err=%v", name, err)
		}
	}
	for _, name := range []string{"model-Q4_K_M.gguf", "model-Q5_K_M.gguf"} {
		if _, err := os.Stat(filepath.Join(repoDir, name)); err != nil {
			t.Errorf("unselected file %s changed: %v", name, err)
		}
	}
	if info, err := os.Stat(filepath.Join(repoDir, "split")); err != nil || !info.IsDir() {
		t.Errorf("selected file's directory was removed: info=%v err=%v", info, err)
	}
}

func TestSelectedDeleteRejectsChangedRootAndGroupComposition(t *testing.T) {
	root, newRoot := t.TempDir(), t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	writeSelectionFile(t, repoDir, "model-Q4_K_M.gguf")
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	body := selectedGroupRequest(t, s, "owner/model", root, "model-Q4_K_M.gguf")
	s.configMu.Lock()
	s.config.LocalScanDirs = []string{newRoot}
	s.configMu.Unlock()
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("changed root accepted old selection: status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(repoDir, "model-Q4_K_M.gguf")); err != nil {
		t.Fatalf("stale root request changed file: %v", err)
	}

	// Re-select against the new root, then mutate the same selected group's
	// composition after confirmation; the old request must not expand to it.
	writeSelectionFile(t, filepath.Join(newRoot, "owner", "model"), "model-Q4_K_M-00001-of-00002.gguf")
	body = selectedGroupRequest(t, s, "owner/model", newRoot, "model-Q4_K_M-00001-of-00002.gguf")
	writeSelectionFile(t, filepath.Join(newRoot, "owner", "model"), "model-Q4_K_M-00002-of-00002.gguf")
	w = cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("changed composition accepted old selection: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestSelectedDeleteReservationExcludesOwningRebuildAndLegacyDelete(t *testing.T) {
	cacheRoot, payloadRoot := t.TempDir(), t.TempDir()
	s := newTestServerWithConfig(t, Config{CacheDir: cacheRoot})
	cache := s.snapshotConfig().cache()
	rd, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rd.Path(), 0755); err != nil {
		t.Fatal(err)
	}
	friendlyRepo := rd.FriendlyPath()
	if err := os.MkdirAll(friendlyRepo, 0755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(payloadRoot, "weights.gguf")
	if err := os.WriteFile(payload, []byte("weights"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(friendlyRepo, "model-Q4_K_M.gguf")
	symlinkOrSkip(t, payload, link)
	selectedPath := filepath.Join(friendlyRepo, "model-Q4_K_M.gguf")
	release, ok := s.jobs.reserveSelectedGGUF([]string{selectedPath})
	if !ok {
		t.Fatal("could not reserve selected friendly link")
	}
	defer release()
	w := cacheRequest(t, s, "POST", "/api/cache/rebuild", `{}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("rebuild crossed selected deletion: status=%d body=%s", w.Code, w.Body.String())
	}
	w = cacheRequest(t, s, "DELETE", "/api/cache/owner/model?type=model", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("legacy whole-repo delete crossed selected deletion: status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("selected friendly entry changed during refused writers: %v", err)
	}
	if _, err := os.Stat(rd.Path()); err != nil {
		t.Fatalf("Hub repository changed during refused legacy delete: %v", err)
	}
}

func TestSelectedDeleteReservationExcludesMirrorDestinationsButNotDryRun(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	targetRoot := t.TempDir()
	targets := &hfdownloader.TargetsConfig{Targets: map[string]hfdownloader.Target{"remote": {Path: targetRoot}}}
	if err := targets.Save(""); err != nil {
		t.Fatal(err)
	}

	pushRoot := filepath.Join(targetRoot, "hub")
	writeSelectionFile(t, filepath.Join(pushRoot, "owner", "model"), "model-Q4_K_M.gguf")
	pushServer := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{pushRoot}})
	if err := os.MkdirAll(pushServer.snapshotConfig().cache().HubDir(), 0755); err != nil {
		t.Fatal(err)
	}
	pushPath := filepath.Join(pushRoot, "owner", "model", "model-Q4_K_M.gguf")
	releasePush, ok := pushServer.jobs.reserveSelectedGGUF([]string{pushPath})
	if !ok {
		t.Fatal("could not reserve mirror-push destination file")
	}
	w := cacheRequest(t, pushServer, "POST", "/api/mirror/push", `{"target":"remote"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("mirror push wrote through selected deletion: status=%d body=%s", w.Code, w.Body.String())
	}
	// Push reads the server's local Hub source, not the selected friendly/local
	// source, so a dry run remains non-mutating and must not veto the reservation.
	w = cacheRequest(t, pushServer, "POST", "/api/mirror/push", `{"target":"remote","dryRun":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("read-only mirror push dry-run was incorrectly blocked: status=%d body=%s", w.Code, w.Body.String())
	}
	releasePush()

	pullCache := filepath.Join(t.TempDir(), "cache")
	pullHub := filepath.Join(pullCache, "hub")
	writeSelectionFile(t, filepath.Join(pullHub, "owner", "model"), "model-Q4_K_M.gguf")
	pullServer := newTestServerWithConfig(t, Config{CacheDir: pullCache, LocalScanDirs: []string{pullHub}})
	pullPath := filepath.Join(pullHub, "owner", "model", "model-Q4_K_M.gguf")
	releasePull, ok := pullServer.jobs.reserveSelectedGGUF([]string{pullPath})
	if !ok {
		t.Fatal("could not reserve mirror-pull destination file")
	}
	w = cacheRequest(t, pullServer, "POST", "/api/mirror/pull", `{"target":"remote"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("mirror pull wrote through selected deletion: status=%d body=%s", w.Code, w.Body.String())
	}
	releasePull()
}

func TestLocalGGUFWriterPredicateMatchesProductionFilters(t *testing.T) {
	for _, tc := range []struct {
		path    string
		filters []string
		want    bool
	}{
		{"model-Q4_K_M.gguf", []string{"q5_k_m"}, false},
		{"model-Q5_K_M.gguf", []string{"q5_k_m"}, true},
		{"nested/model-Q4_K_M.gguf", []string{"q4_k_m"}, true},
		{"model-Q4_K_M.gguf", nil, true},
	} {
		if got := hfdownloader.GGUFPathSelected(tc.path, tc.filters, nil, false); got != tc.want {
			t.Errorf("GGUFPathSelected(%q,%v)=%v want %v", tc.path, tc.filters, got, tc.want)
		}
	}
	if hfdownloader.GGUFPathSelected("nested/model-Q4_K_M.gguf", []string{"q4_k"}, nil, true) {
		t.Error("exact filter incorrectly treated an underscore prefix as a whole segment")
	}
	if hfdownloader.GGUFPathSelected("nested/model-Q4_K_M.gguf", []string{"q4_k_m"}, []string{"nested"}, false) {
		t.Error("excluded relative path was considered writable")
	}
}

func blockingPlanServer(t *testing.T, file string) (*httptest.Server, <-chan struct{}, func()) {
	t.Helper()
	started := make(chan struct{}, 8)
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("selected-delete test HTTP %s %s", r.Method, r.URL.Path)
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sha":"deadbeef"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `[{"type":"file","path":%q,"size":5}]`, file)
		case strings.Contains(r.URL.Path, "/raw/") || strings.Contains(r.URL.Path, "/resolve/"):
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-gate:
				_, _ = w.Write([]byte("data!"))
			case <-r.Context().Done():
			}
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, started, func() { close(gate) }
}

func blockingRevisionServer(t *testing.T, file string) (*httptest.Server, <-chan struct{}, func()) {
	t.Helper()
	started := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			started <- struct{}{}
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte(`{"sha":"deadbeef"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = fmt.Fprintf(w, `[{"type":"file","path":%q,"size":5}]`, file)
		case strings.Contains(r.URL.Path, "/raw/") || strings.Contains(r.URL.Path, "/resolve/"):
			_, _ = w.Write([]byte("data!"))
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, started, func() { close(gate) }
}

func selectedGroupRequest(t *testing.T, s *Server, repo, locationRoot, memberPath string) []byte {
	t.Helper()
	code, selection := getSelection(t, s, repo, "model", "")
	if code != http.StatusOK {
		t.Fatalf("selection status=%d", code)
	}
	for _, loc := range selection.Locations {
		if !strings.Contains(loc.Path, locationRoot) {
			continue
		}
		for _, group := range loc.Groups {
			for _, member := range group.Members {
				if member.Path == memberPath {
					body, err := json.Marshal(testSelectedDeleteRequest{Repo: repo, Type: "model", LocationID: loc.ID, GroupID: group.ID, Members: group.Members})
					if err != nil {
						t.Fatal(err)
					}
					return body
				}
			}
		}
	}
	t.Fatalf("selected member %q not found in %q", memberPath, locationRoot)
	return nil
}

func TestSelectedDeleteWaitsForActualQ4RunUnwind(t *testing.T) {
	root := t.TempDir()
	writeSelectionFile(t, filepath.Join(root, "owner", "model"), "model-Q4_K_M.gguf")
	hf, started, release := blockingPlanServer(t, "model-Q4_K_M.gguf")
	defer hf.Close()
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(release) }
	defer releaseGate()
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalDir: root, Endpoint: hf.URL, MaxActive: 1})
	job, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "owner/model", Filters: []string{"q4_k_m"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		got, _ := s.jobs.GetJob(job.ID)
		t.Fatalf("real downloader did not reach the planned file: %+v", got)
	}
	body := selectedGroupRequest(t, s, "owner/model", root, "model-Q4_K_M.gguf")
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete while actual Q4 run is writing status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "owner", "model", "model-Q4_K_M.gguf")); err != nil {
		t.Fatalf("busy delete removed selected file: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if current, ok := s.jobs.GetJob(job.ID); !ok || current.Status != JobStatusRunning {
		t.Fatalf("expected download to remain active behind blocked response, got %#v", current)
	}
	if !s.jobs.CancelJob(job.ID) {
		t.Fatal("cancel active job failed")
	}
	releaseGate()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedDeleteWaitsAfterCancelAndDismissUntilRunUnwinds(t *testing.T) {
	root := t.TempDir()
	writeSelectionFile(t, filepath.Join(root, "owner", "model"), "model-Q4_K_M.gguf")
	hf, started, releaseDownload := blockingPlanServer(t, "model-Q4_K_M.gguf")
	defer hf.Close()
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalDir: root, Endpoint: hf.URL, MaxActive: 1})
	persistEntered, persistRelease := make(chan struct{}), make(chan struct{})
	var persistOnce sync.Once
	s.jobs.persistStateFile = func(string, []*Job) error {
		persistOnce.Do(func() {
			close(persistEntered)
			<-persistRelease
		})
		return nil
	}
	job, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "owner/model", Filters: []string{"q4_k_m"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("real downloader did not reach the planned Q4 file")
	}
	cancelResult := make(chan bool, 1)
	go func() { cancelResult <- s.jobs.CancelJob(job.ID) }()
	select {
	case <-persistEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation/unwind did not reach the controlled persistence boundary")
	}
	got, ok := s.jobs.GetJob(job.ID)
	if !ok || got.Status != JobStatusCancelled {
		t.Fatalf("job was not visibly cancelled before dismissal: %#v", got)
	}
	if result, _ := s.jobs.DismissJobResult(job.ID); result != DismissJobOK {
		t.Fatalf("terminal cancelled job could not be dismissed: %v", result)
	}
	body := selectedGroupRequest(t, s, "owner/model", root, "model-Q4_K_M.gguf")
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete crossed cancelled/dismissed but unwinding writer: status=%d body=%s", w.Code, w.Body.String())
	}
	close(persistRelease)
	releaseDownload()
	select {
	case ok := <-cancelResult:
		if !ok {
			t.Fatal("cancel request failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel request did not finish after persistence was released")
	}
	runDone := make(chan struct{})
	go func() { s.jobs.runWG.Wait(); close(runDone) }()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled downloader did not unwind")
	}
	w = cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("delete remained blocked after actual run unwind: status=%d body=%s", w.Code, w.Body.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

type selectedDeleteObservedSaveLocker struct {
	mu     sync.Mutex
	count  atomic.Int32
	second chan struct{}
	once   sync.Once
}

func (l *selectedDeleteObservedSaveLocker) Lock() {
	if l.count.Add(1) == 2 {
		l.once.Do(func() { close(l.second) })
	}
	l.mu.Lock()
}

func (l *selectedDeleteObservedSaveLocker) Unlock() { l.mu.Unlock() }

func TestSelectedDeleteTracksBothRealGenerationsDuringRetry(t *testing.T) {
	root := t.TempDir()
	writeSelectionFile(t, filepath.Join(root, "owner", "model"), "model-Q4_K_M.gguf")
	hf, started, releaseDownload := blockingPlanServer(t, "model-Q4_K_M.gguf")
	defer hf.Close()
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalDir: root, Endpoint: hf.URL, MaxActive: 1})
	locker := &selectedDeleteObservedSaveLocker{second: make(chan struct{})}
	s.jobs.saveMu = locker
	persistEntered, persistRelease := make(chan struct{}), make(chan struct{})
	var persistOnce sync.Once
	s.jobs.persistStateFile = func(string, []*Job) error {
		persistOnce.Do(func() {
			close(persistEntered)
			<-persistRelease
		})
		return nil
	}
	job, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "owner/model", Filters: []string{"q4_k_m"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first real downloader generation did not start")
	}
	cancelResult := make(chan bool, 1)
	go func() { cancelResult <- s.jobs.CancelJob(job.ID) }()
	select {
	case <-persistEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not reach persistence")
	}
	select {
	case <-locker.second:
		// The second state save is the other real operation (CancelJob and the
		// unwinding run). The first retains saveMu, so the first run cannot exit.
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled run did not reach its serialized persistence boundary")
	}
	if got, ok := s.jobs.GetJob(job.ID); !ok || got.Status != JobStatusCancelled {
		t.Fatalf("first generation is not terminal before Retry: %#v", got)
	}
	if !s.jobs.RetryJob(job.ID) {
		t.Fatal("explicit Retry was rejected while the old cancelled run unwound")
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("second real downloader generation did not start")
	}
	s.jobs.mu.RLock()
	activeGenerations := len(s.jobs.runActivities)
	s.jobs.mu.RUnlock()
	if activeGenerations != 2 {
		t.Fatalf("expected independent old/new run records, got %d", activeGenerations)
	}
	body := selectedGroupRequest(t, s, "owner/model", root, "model-Q4_K_M.gguf")
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("one generation cleared another live target writer: status=%d body=%s", w.Code, w.Body.String())
	}
	close(persistRelease)
	if !s.jobs.CancelJob(job.ID) {
		t.Fatal("cancel second generation failed")
	}
	releaseDownload()
	select {
	case <-cancelResult:
	case <-time.After(5 * time.Second):
		t.Fatal("first cancel request did not finish")
	}
	runDone := make(chan struct{})
	go func() { s.jobs.runWG.Wait(); close(runDone) }()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("both downloader generations did not unwind")
	}
	w = cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("delete stayed busy after both generations exited: status=%d body=%s", w.Code, w.Body.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedDeleteAllowsActualNonOverlappingQ5Plan(t *testing.T) {
	root := t.TempDir()
	writeSelectionFile(t, filepath.Join(root, "owner", "model"), "model-Q4_K_M.gguf")
	hf, started, release := blockingPlanServer(t, "model-Q5_K_M.gguf")
	defer hf.Close()
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalDir: root, Endpoint: hf.URL, MaxActive: 1})
	job, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "owner/model", Filters: []string{"q5_k_m"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		got, _ := s.jobs.GetJob(job.ID)
		t.Fatalf("real downloader did not reach the planned Q5 file: %+v", got)
	}
	body := selectedGroupRequest(t, s, "owner/model", root, "model-Q4_K_M.gguf")
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("non-overlapping Q5 plan blocked Q4 delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "owner", "model", "model-Q4_K_M.gguf")); !os.IsNotExist(err) {
		t.Fatalf("Q4 remains after selected delete, err=%v", err)
	}
	release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !s.jobs.CancelJob(job.ID) {
		t.Fatal("cancel active job failed")
	}
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedDeleteUsesProductionPredicateForQueuedJobs(t *testing.T) {
	root := t.TempDir()
	writeSelectionFile(t, filepath.Join(root, "owner", "model"), "model-Q4_K_M.gguf")
	hf, started, release := blockingPlanServer(t, "model-Q5_K_M.gguf")
	defer hf.Close()
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalDir: root, Endpoint: hf.URL, MaxActive: 1})
	running, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "owner/blocker"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("real blocker run did not start")
	}
	q4, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "owner/model", Filters: []string{"q4_k_m"}})
	if err != nil {
		t.Fatal(err)
	}
	q5, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "owner/model", Filters: []string{"q5_k_m"}})
	if err != nil {
		t.Fatal(err)
	}
	if q4.Status != JobStatusQueued || q5.Status != JobStatusQueued {
		t.Fatalf("jobs did not remain queued behind blocker: Q4=%s Q5=%s", q4.Status, q5.Status)
	}
	target := filepath.Join(root, "owner", "model", "model-Q4_K_M.gguf")
	if _, ok := s.jobs.reserveSelectedGGUF([]string{target}); ok {
		t.Fatal("queued Q4 job did not veto deletion")
	}
	if !s.jobs.CancelJob(q4.ID) {
		t.Fatal("could not manually cancel queued Q4 job")
	}
	releaseDelete, ok := s.jobs.reserveSelectedGGUF([]string{target})
	if !ok {
		t.Fatal("queued Q5 job in same folder incorrectly vetoed Q4 deletion")
	}
	if _, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "owner/model", Filters: []string{"q4_k_m"}}); !errors.Is(err, errSelectionWriterBusy) {
		t.Fatalf("new matching job crossed active deletion: %v", err)
	}
	if _, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "owner/model", Revision: "other", Filters: []string{"q5_k_m"}}); err != nil {
		t.Fatalf("non-overlapping new Q5 job was rejected: %v", err)
	}
	releaseDelete()
	release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !s.jobs.CancelJob(running.ID) {
		t.Fatal("cancel active blocker failed")
	}
	if err := s.jobs.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCacheSelectionLocalSymlinkIsVisibleAsLinkOnly(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	writeSelectionFile(t, root, "outside/payload.gguf")
	link := filepath.Join(repoDir, "alias-Q4_K_M.gguf")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, filepath.Join(root, "outside", "payload.gguf"), link)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	code, got := getSelection(t, s, "owner/model", "model", "")
	if code != 200 || len(got.Locations) != 1 {
		t.Fatalf("selection=%d %+v", code, got)
	}
	found := false
	for _, g := range got.Locations[0].Groups {
		for _, m := range g.Members {
			if m.Path == "alias-Q4_K_M.gguf" {
				found = true
				if !m.LinkOnly || m.Message == "" {
					t.Errorf("link lacks link-only explanation: %+v", m)
				}
			}
		}
	}
	if !found {
		t.Fatal("valid named GGUF link omitted")
	}
}

func TestCacheSelectionLocalLocationIDsSurviveRootReordering(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	writeSelectionFile(t, rootA, "owner/model/a.gguf")
	writeSelectionFile(t, rootB, "owner/model/b.gguf")
	cacheDir := filepath.Join(t.TempDir(), "cache")
	first := newTestServerWithConfig(t, Config{CacheDir: cacheDir, LocalScanDirs: []string{rootA, rootB}})
	second := newTestServerWithConfig(t, Config{CacheDir: cacheDir, LocalScanDirs: []string{rootB, rootA}})
	_, one := getSelection(t, first, "owner/model", "model", "")
	_, two := getSelection(t, second, "owner/model", "model", "")
	idsByPath := func(locations []cacheSelectionLocation) map[string]string {
		result := map[string]string{}
		for _, location := range locations {
			result[location.Path] = location.ID
		}
		return result
	}
	a, b := idsByPath(one.Locations), idsByPath(two.Locations)
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("expected both locations: first=%+v second=%+v", one, two)
	}
	for path, id := range a {
		if b[path] != id {
			t.Errorf("ID changed for %s: %s -> %s", path, id, b[path])
		}
	}
}

func TestCacheSelectionHFReadFailureWarns(t *testing.T) {
	hub := t.TempDir()
	t.Setenv("HF_HUB_CACHE", hub)
	s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})
	rd, err := s.snapshotConfig().cache().Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rd.Path(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rd.BlobsDir(), []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}
	code, got := getSelection(t, s, "owner/model", "model", "")
	if code != 200 || len(got.Locations) != 1 || got.Locations[0].Warning == "" {
		t.Fatalf("read failure hidden: %d %+v", code, got)
	}
}

func TestCacheSelectionHFNonDirectoryRepoWarns(t *testing.T) {
	hub := t.TempDir()
	t.Setenv("HF_HUB_CACHE", hub)
	s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})
	rd, err := s.snapshotConfig().cache().Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(rd.Path()), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rd.Path(), []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}
	code, got := getSelection(t, s, "owner/model", "model", "")
	if code != 200 || len(got.Locations) != 1 || got.Locations[0].Warning == "" {
		t.Fatalf("non-directory HF path hidden: %d %+v", code, got)
	}
}

func TestCacheSelectionHFIncludesNamedFilesAcrossSnapshots(t *testing.T) {
	hub := t.TempDir()
	t.Setenv("HF_HUB_CACHE", hub)
	s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})
	cache := s.snapshotConfig().cache()
	rd, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"commit-a", "commit-b"} {
		dir, err := rd.SnapshotDir(version)
		if err != nil {
			t.Fatal(err)
		}
		writeSelectionFile(t, dir, "weights-Q4_K_M.gguf")
		writeSelectionFile(t, dir, "model.gguf")
		symlinkOrSkip(t, "weights-Q4_K_M.gguf", filepath.Join(dir, "alias.gguf"))
	}
	code, got := getSelection(t, s, "owner/model", "model", "")
	if code != 200 || len(got.Locations) != 1 || got.Locations[0].Source != "HF cache" {
		t.Fatalf("selection=%d %+v", code, got)
	}
	if len(got.Locations[0].Groups) != 3 {
		t.Fatalf("groups=%+v", got.Locations[0].Groups)
	}
	for _, group := range got.Locations[0].Groups {
		if len(group.Members) != 2 {
			t.Errorf("group does not include both versions: %+v", group)
		}
		for _, member := range group.Members {
			if len(member.Versions) != 1 {
				t.Errorf("missing version ref: %+v", member)
			}
		}
	}
	for _, group := range got.Locations[0].Groups {
		if strings.Contains(group.Label, "alias.gguf") && len(group.Members) != 2 {
			t.Errorf("snapshot links were not included across versions: %+v", group)
		}
	}
	if code, _ := getSelection(t, s, "owner/model", "model", "hf-invalid"); code != 400 {
		t.Fatalf("stale HF selector status=%d", code)
	}
}
