// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// mmprojSelectionGroup returns the group that owns memberPath.
func mmprojSelectionGroup(t *testing.T, location cacheSelectionLocation, memberPath string) cacheSelectionGroup {
	t.Helper()
	for _, group := range location.Groups {
		for _, member := range group.Members {
			if member.Path == memberPath {
				return group
			}
		}
	}
	t.Fatalf("no group owns %q in %s: %+v", memberPath, location.Path, location.Groups)
	return cacheSelectionGroup{}
}

// mmprojLocalLocation returns the local location that holds owner/model under
// the given root.
func mmprojLocalLocation(t *testing.T, selection cacheSelectionResponse, root string) cacheSelectionLocation {
	t.Helper()
	want := pathIdentityKey(filepath.Join(root, "owner", "model"))
	for _, location := range selection.Locations {
		if pathIdentityKey(filepath.Clean(location.Path)) == want {
			return location
		}
	}
	t.Fatalf("local location for %q missing: %+v", root, selection.Locations)
	return cacheSelectionLocation{}
}

// Acceptance case 1: a single downloaded mmproj companion is represented as
// its own deletable group instead of being dropped from the selectable model.
func TestCacheSelectionExposesMMProjCompanionGroup(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	writeSelectionFile(t, repoDir, "mmproj-F32.gguf")
	writeSelectionFile(t, repoDir, "model-Q4_K_M.gguf")
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK || len(selection.Locations) != 1 {
		t.Fatalf("selection=%d %+v", code, selection)
	}
	location := mmprojLocalLocation(t, selection, root)
	if !location.CanDelete {
		t.Fatalf("completely enumerated local location must advertise deletion: %+v", location)
	}
	companion := mmprojSelectionGroup(t, location, "mmproj-F32.gguf")
	if companion.Kind != cacheSelectionKindVisionEncoder {
		t.Fatalf("companion kind=%q, want %q", companion.Kind, cacheSelectionKindVisionEncoder)
	}
	if !strings.HasPrefix(companion.ID, "mmproj-") {
		t.Fatalf("companion group id=%q must use the mmproj- prefix", companion.ID)
	}
	if companion.Quant != "" {
		t.Fatalf("companion group reported a model quantisation label %q", companion.Quant)
	}
	if companion.Label != "mmproj-F32.gguf" {
		t.Fatalf("companion label=%q, want the family path", companion.Label)
	}
	if !companion.CanDelete {
		t.Fatalf("companion group is not deletable: %+v", companion)
	}
	if len(companion.Members) != 1 || companion.Members[0].Path != "mmproj-F32.gguf" {
		t.Fatalf("companion members=%+v", companion.Members)
	}
	llm := mmprojSelectionGroup(t, location, "model-Q4_K_M.gguf")
	if llm.Kind != cacheSelectionKindLLM {
		t.Fatalf("llm kind=%q, want %q", llm.Kind, cacheSelectionKindLLM)
	}
	if !strings.HasPrefix(llm.ID, "gguf-") {
		t.Fatalf("llm group id=%q must keep the gguf- prefix", llm.ID)
	}
	if llm.Quant != "Q4_K_M" {
		t.Fatalf("llm quant=%q", llm.Quant)
	}
	// The wire contract always carries "kind", and a companion never carries a
	// model quantisation label.
	raw := cacheRequest(t, s, "GET", "/api/cache-selection?repo=owner%2Fmodel&type=model", "")
	var payload struct {
		Locations []struct {
			Groups []struct {
				Kind  string  `json:"kind"`
				Quant *string `json:"quant"`
			} `json:"groups"`
		} `json:"locations"`
	}
	if err := json.Unmarshal(raw.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode raw selection: %v: %s", err, raw.Body.String())
	}
	kinds := map[string]int{}
	for _, location := range payload.Locations {
		for _, group := range location.Groups {
			if group.Kind == "" {
				t.Fatalf("group without a kind field: %s", raw.Body.String())
			}
			kinds[group.Kind]++
			if group.Kind == cacheSelectionKindVisionEncoder {
				if group.Quant != nil && *group.Quant != "" {
					t.Fatalf("companion group carries a quant field: %s", raw.Body.String())
				}
			}
		}
	}
	if kinds[cacheSelectionKindVisionEncoder] != 1 || kinds[cacheSelectionKindLLM] != 1 {
		t.Fatalf("advertised kinds=%v body=%s", kinds, raw.Body.String())
	}
}

// The group ID contract is pinned: existing LLM group IDs are unchanged and a
// companion group uses its own prefix over the same family/shape hash input.
func TestCacheSelectionGroupIDContractForCompanions(t *testing.T) {
	groups := makeSelectionGroups("fixed-location", []cacheSelectionFile{
		{path: "model-Q4_K_M.gguf", size: 5},
		{path: "mmproj-F32.gguf", size: 5},
	})
	if len(groups) != 2 {
		t.Fatalf("groups=%+v", groups)
	}
	byID := map[string]cacheSelectionGroup{}
	for _, group := range groups {
		byID[group.ID] = group
	}
	llm, ok := byID["gguf-e91b84f0cab3867ec986c100"]
	if !ok {
		t.Fatalf("existing llm group id changed: %+v", groups)
	}
	if llm.Kind != cacheSelectionKindLLM || llm.Quant != "Q4_K_M" {
		t.Fatalf("llm group=%+v", llm)
	}
	companion, ok := byID["mmproj-ae6e5eb759b4485fe13822bf"]
	if !ok {
		t.Fatalf("companion group id missing: %+v", groups)
	}
	if companion.Kind != cacheSelectionKindVisionEncoder || companion.Quant != "" {
		t.Fatalf("companion group=%+v", companion)
	}
}

// Acceptance case 2: coexisting F16/BF16/F32 companions stay three
// independent groups with their own actions.
func TestCacheSelectionMMProjVariantsAreIndependentGroups(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	for _, name := range []string{"mmproj-F16.gguf", "mmproj-BF16.gguf", "mmproj-F32.gguf", "model-Q4_K_M.gguf", "model-Q8_0.gguf"} {
		writeSelectionFile(t, repoDir, name)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK {
		t.Fatalf("selection status=%d", code)
	}
	location := mmprojLocalLocation(t, selection, root)
	companions, llms := 0, 0
	owner := map[string]string{}
	for _, group := range location.Groups {
		switch group.Kind {
		case cacheSelectionKindVisionEncoder:
			companions++
		case cacheSelectionKindLLM:
			llms++
		default:
			t.Fatalf("unknown group kind %q: %+v", group.Kind, group)
		}
		if len(group.Members) != 1 {
			t.Fatalf("group %q merged members: %+v", group.ID, group)
		}
		member := group.Members[0].Path
		if other, ok := owner[member]; ok {
			t.Fatalf("member %q appears in groups %q and %q", member, other, group.ID)
		}
		owner[member] = group.ID
	}
	if companions != 3 {
		t.Fatalf("companion groups=%d, want 3: %+v", companions, location.Groups)
	}
	if llms != 2 {
		t.Fatalf("llm groups=%d, want 2: %+v", llms, location.Groups)
	}
	for _, name := range []string{"mmproj-F16.gguf", "mmproj-BF16.gguf", "mmproj-F32.gguf"} {
		group := mmprojSelectionGroup(t, location, name)
		if !group.CanDelete || group.Kind != cacheSelectionKindVisionEncoder {
			t.Fatalf("companion %q is not independently deletable: %+v", name, group)
		}
	}
}

// Acceptance case 3: deleting one companion leaves the other companions and
// every LLM quantization intact.
func TestSelectedDeleteRemovesOnlySelectedMMProjCompanion(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	for _, name := range []string{"mmproj-F16.gguf", "mmproj-BF16.gguf", "mmproj-F32.gguf", "model-Q4_K_M.gguf", "model-Q8_0.gguf"} {
		writeSelectionFile(t, repoDir, name)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK {
		t.Fatalf("selection status=%d", code)
	}
	location := mmprojLocalLocation(t, selection, root)
	group := mmprojSelectionGroup(t, location, "mmproj-F32.gguf")
	body, err := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: location.ID, GroupID: group.ID, Members: group.Members})
	if err != nil {
		t.Fatal(err)
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("companion delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(repoDir, "mmproj-F32.gguf")); !os.IsNotExist(err) {
		t.Fatalf("selected companion remains: %v", err)
	}
	for _, name := range []string{"mmproj-F16.gguf", "mmproj-BF16.gguf", "model-Q4_K_M.gguf", "model-Q8_0.gguf"} {
		if _, err := os.Stat(filepath.Join(repoDir, name)); err != nil {
			t.Fatalf("unselected file %s changed: %v", name, err)
		}
	}
	code, selection = getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK {
		t.Fatalf("refreshed selection status=%d", code)
	}
	location = mmprojLocalLocation(t, selection, root)
	for _, name := range []string{"mmproj-F16.gguf", "mmproj-BF16.gguf"} {
		group := mmprojSelectionGroup(t, location, name)
		if !group.CanDelete || group.Kind != cacheSelectionKindVisionEncoder {
			t.Fatalf("remaining companion %q is no longer independently deletable: %+v", name, group)
		}
	}
}

// Acceptance case 4: a writer that would write the selected companion blocks
// the deletion under the existing reservation rules.
func TestSelectedDeleteBlockedByMMProjWriter(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	writeSelectionFile(t, repoDir, "mmproj-F32.gguf")
	writeSelectionFile(t, repoDir, "model-Q4_K_M.gguf")
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalDir: root})
	registerJobManagerCleanup(t, s)
	// A dispatched-but-not-yet-running writer models the planned writer the
	// reservation must refuse: it is already scheduled to write this exact
	// companion file.
	writer := &Job{ID: "mmproj-writer", Repo: "owner/model", OutputDir: root, LocalDir: root, Filters: []string{"mmproj-f32"}, Status: JobStatusQueued, starting: true}
	s.jobs.mu.Lock()
	s.jobs.jobs[writer.ID] = writer
	s.jobs.mu.Unlock()
	t.Cleanup(func() {
		s.jobs.mu.Lock()
		delete(s.jobs.jobs, writer.ID)
		s.jobs.mu.Unlock()
	})
	body := selectedGroupRequest(t, s, "owner/model", root, "mmproj-F32.gguf")
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete with a matching mmproj writer status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(repoDir, "mmproj-F32.gguf")); err != nil {
		t.Fatalf("busy companion delete removed the selected file: %v", err)
	}
}

// Acceptance case 5: an unrelated quant writer does not block a
// non-overlapping companion deletion.
func TestSelectedDeleteAllowsUnrelatedQuantWriterForMMProj(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	writeSelectionFile(t, repoDir, "mmproj-F32.gguf")
	writeSelectionFile(t, repoDir, "model-Q4_K_M.gguf")
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalDir: root})
	registerJobManagerCleanup(t, s)
	// The writer only claims the Q4 quantization, so it does not overlap the
	// selected companion.
	writer := &Job{ID: "quant-writer", Repo: "owner/model", OutputDir: root, LocalDir: root, Filters: []string{"q4_k_m"}, Status: JobStatusQueued, starting: true}
	s.jobs.mu.Lock()
	s.jobs.jobs[writer.ID] = writer
	s.jobs.mu.Unlock()
	t.Cleanup(func() {
		s.jobs.mu.Lock()
		delete(s.jobs.jobs, writer.ID)
		s.jobs.mu.Unlock()
	})
	body := selectedGroupRequest(t, s, "owner/model", root, "mmproj-F32.gguf")
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("delete with an unrelated quant writer status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(repoDir, "mmproj-F32.gguf")); !os.IsNotExist(err) {
		t.Fatalf("selected companion remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "model-Q4_K_M.gguf")); err != nil {
		t.Fatalf("the unrelated writer's own file was removed: %v", err)
	}
}

// Acceptance case 6: an unavailable companion link keeps the location
// fail-closed instead of exposing a destructive action.
func TestCacheSelectionMMProjUnavailableLinkStaysFailClosed(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	writeSelectionFile(t, repoDir, "mmproj-F32.gguf")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, filepath.Join(repoDir, "missing-target"), filepath.Join(repoDir, "mmproj-F16.gguf"))
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK {
		t.Fatalf("selection status=%d", code)
	}
	location := mmprojLocalLocation(t, selection, root)
	if location.Warning == "" || location.CanDelete {
		t.Fatalf("unavailable companion link did not keep the location fail-closed: %+v", location)
	}
	for _, group := range location.Groups {
		if group.CanDelete {
			t.Fatalf("incompletely enumerated location advertised a deletable group: %+v", group)
		}
	}
}

// Acceptance case 6: an owner symlink escape stays refused for a companion
// group and never reaches the outside target.
func TestSelectedDeleteRefusesMMProjOwnerSymlinkEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	outsideRepo := filepath.Join(outside, "model")
	writeSelectionFile(t, outsideRepo, "mmproj-F32.gguf")
	symlinkOrSkip(t, outside, filepath.Join(root, "owner"))
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK || len(selection.Locations) != 1 {
		t.Fatalf("selection status=%d locations=%+v", code, selection.Locations)
	}
	location := selection.Locations[0]
	if location.CanDelete || len(location.Groups) == 0 {
		t.Fatalf("escaped owner must be visible but not deletable: %+v", location)
	}
	for _, group := range location.Groups {
		if group.CanDelete {
			t.Fatalf("escaped owner advertised a deletable companion group: %+v", group)
		}
	}
	group := mmprojSelectionGroup(t, location, "mmproj-F32.gguf")
	body, err := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: location.ID, GroupID: group.ID, Members: group.Members})
	if err != nil {
		t.Fatal(err)
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete through owner symlink status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(outsideRepo, "mmproj-F32.gguf")); err != nil {
		t.Fatalf("outside companion was changed: %v", err)
	}
}

// Acceptance case 6: a separately registered descendant root keeps ownership
// of its own companion file.
func TestSelectedDeleteProtectsAliasedMMProjDescendant(t *testing.T) {
	outer := t.TempDir()
	child := filepath.Join(outer, "owner", "model", "library")
	writeSelectionFile(t, filepath.Join(outer, "owner", "model"), "outer-Q4_K_M.gguf")
	writeSelectionFile(t, filepath.Join(child, "owner", "model"), "vendor-mmproj-F32.gguf")
	alias := filepath.Join(t.TempDir(), "registered-library")
	symlinkOrSkip(t, child, alias)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{outer, alias}})
	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK {
		t.Fatalf("selection status=%d", code)
	}
	outerLocation := mmprojLocalLocation(t, selection, outer)
	for _, group := range outerLocation.Groups {
		for _, member := range group.Members {
			if strings.HasPrefix(member.Path, "library/") {
				t.Fatalf("outer location claimed a separately registered descendant companion: %+v", member)
			}
		}
	}
	var childLocation *cacheSelectionLocation
	for i := range selection.Locations {
		if selection.Locations[i].Source == "Local" && strings.HasPrefix(selection.Locations[i].Path, alias) {
			childLocation = &selection.Locations[i]
		}
	}
	if childLocation == nil || !childLocation.CanDelete {
		t.Fatalf("separately registered alias was not independently selectable: %+v", selection.Locations)
	}
	companion := mmprojSelectionGroup(t, *childLocation, "vendor-mmproj-F32.gguf")
	if companion.Kind != cacheSelectionKindVisionEncoder || !companion.CanDelete {
		t.Fatalf("descendant companion group=%+v", companion)
	}
	body, err := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: childLocation.ID, GroupID: companion.ID, Members: companion.Members})
	if err != nil {
		t.Fatal(err)
	}
	if w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body)); w.Code != http.StatusOK {
		t.Fatalf("descendant companion delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(child, "owner", "model", "vendor-mmproj-F32.gguf")); !os.IsNotExist(err) {
		t.Fatalf("selected descendant companion remains: %v", err)
	}
}

// Acceptance case 4 (planned writer): a running writer whose completed plan
// covers the selected companion blocks the companion deletion.
//
// The writer writes through a directory alias ("live" -> "blocked"), so the
// plan's raw destination spelling differs from the selected entry's spelling
// while both resolve to the same physical file. While the transfer is held the
// run activity carries a known, complete plan, so the queued-job filter branch
// and the activity's own filter fallback are both skipped and the planned
// writer reservation branch is the only mechanism that can refuse the
// deletion.
func TestSelectedDeleteBlockedByPlannedMMProjWriter(t *testing.T) {
	storage := t.TempDir()
	repoDir := filepath.Join(storage, "models", "o", "m")
	blocked := filepath.Join(repoDir, "blocked")
	const companion = "mmproj-F32.gguf"
	selectedFile := filepath.Join(blocked, companion)
	writeSelectionFile(t, blocked, companion)
	if err := os.Symlink(blocked, filepath.Join(repoDir, "live")); err != nil {
		if runtime.GOOS == "windows" && os.IsPermission(err) {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	hf, started, release := blockingPlanServer(t, "live/"+companion)
	defer hf.Close()
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(release) }
	defer releaseGate()
	s := newTestServerWithConfig(t, Config{CacheDir: storage, Endpoint: hf.URL, MaxActive: 1})
	registerJobManagerCleanup(t, s)
	job, _, err := s.jobs.CreateJob(DownloadRequest{
		Repo: "upstream/source", LocalRepo: "o/m", Filters: []string{"mmproj-f32"}, Excludes: []string{"blocked"},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("real downloader did not reach the planned companion transfer")
	}
	// The writer is a running job with a registered, complete plan. Prove the
	// two filter-predicate branches cannot produce the refusal: the job is
	// neither queued nor starting, and the run activity's plan is complete, so
	// only the planned-writer branch of the reservation is still consulted.
	s.jobs.mu.Lock()
	planKnown, planComplete := false, false
	for _, activity := range s.jobs.runActivities {
		if activity.job.ID == job.ID && activity.planKnown {
			planKnown, planComplete = true, activity.planComplete
		}
	}
	running := false
	if live, ok := s.jobs.jobs[job.ID]; ok {
		running = live.Status == JobStatusRunning && !live.starting
	}
	s.jobs.mu.Unlock()
	if !planKnown {
		t.Fatal("transfer began before the production run registered its completed plan")
	}
	if !planComplete {
		t.Fatal("the planned companion writer did not register a complete plan")
	}
	if !running {
		t.Fatal("the planned companion writer is not a running job")
	}

	body := selectedGroupRequest(t, s, "o/m", repoDir, "blocked/"+companion)
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete while the planned companion transfer is held: status=%d body=%s, want %d", w.Code, w.Body.String(), http.StatusConflict)
	}
	if _, err := os.Stat(selectedFile); err != nil {
		t.Fatalf("the planned writer's selected companion was removed: %v", err)
	}

	releaseGate()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, ok := s.jobs.GetJob(job.ID)
		if ok && (current.Status == JobStatusCompleted || current.Status == JobStatusFailed) {
			if current.Status != JobStatusCompleted {
				t.Fatalf("planned companion writer did not complete: %+v", current)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("planned companion writer did not finish after transfer release")
		case <-ticker.C:
		}
	}
	// The writer's plan targets the selected entry itself, so the completed
	// transfer replaces the selected file's content.
	data, err := os.ReadFile(selectedFile)
	if err != nil || string(data) != "data!" {
		t.Fatalf("planned companion transfer output=%q err=%v, want the written data", data, err)
	}
}

// Acceptance case 1 (HF cache): a saved mmproj companion is its own group in
// the HF location and can be removed without touching the LLM quantization.
func TestCacheSelectionHFMMProjCompanionGroup(t *testing.T) {
	s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})
	rd, err := s.snapshotConfig().cache().Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	store := func(version, name string) *hfdownloader.StoreFileResult {
		t.Helper()
		temp := filepath.Join(t.TempDir(), "download")
		if err := os.WriteFile(temp, []byte("payload"), 0644); err != nil {
			t.Fatal(err)
		}
		got, err := rd.StoreDownloadedFile(temp, name, version, "", "", false)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	companion := store("version-a", "mmproj-F32.gguf")
	llm := store("version-a", "model-Q4_K_M.gguf")
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
	group := mmprojSelectionGroup(t, *location, "mmproj-F32.gguf")
	if group.Kind != cacheSelectionKindVisionEncoder || !strings.HasPrefix(group.ID, "mmproj-") || group.Quant != "" {
		t.Fatalf("HF companion group=%+v", group)
	}
	if !group.CanDelete || len(group.Members) != 1 || len(group.Members[0].Versions) != 1 || group.Members[0].Versions[0] != "version-a" {
		t.Fatalf("HF companion group is not deletable: %+v", group)
	}
	body, err := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: location.ID, GroupID: group.ID, Members: group.Members})
	if err != nil {
		t.Fatal(err)
	}
	// The friendly-view entries exist before the delete, so their removal
	// below is a real assertion and not a vacuous one.
	for _, entry := range []string{companion.FriendlyPath, llm.FriendlyPath} {
		if _, err := os.Lstat(entry); err != nil {
			t.Fatalf("friendly entry %q missing before the delete: %v", entry, err)
		}
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("HF companion delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(companion.SnapshotPath); !os.IsNotExist(err) {
		t.Fatalf("selected HF companion snapshot remains: %v", err)
	}
	if _, err := os.Lstat(companion.FriendlyPath); !os.IsNotExist(err) {
		t.Fatalf("selected HF companion friendly entry remains: %v", err)
	}
	if _, err := os.Lstat(llm.SnapshotPath); err != nil {
		t.Fatalf("other quant snapshot changed: %v", err)
	}
	if _, err := os.Lstat(llm.FriendlyPath); err != nil {
		t.Fatalf("other quant friendly entry changed: %v", err)
	}
}

// A dot-separated companion is classified by the same rule the download side
// uses, so the selection model and the cache metadata agree that it is a
// vision encoder and never a model quantization.
func TestCacheSelectionClassifiesDotSeparatedMMProjCompanion(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	const companion = "Qwopus3.5-9B-v3.5.mmproj-f16.gguf"
	const quant = "Qwopus3.5-9B-v3.5.Q8_0.gguf"
	writeSelectionFile(t, repoDir, companion)
	writeSelectionFile(t, repoDir, quant)
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK {
		t.Fatalf("selection status=%d", code)
	}
	location := mmprojLocalLocation(t, selection, root)
	group := mmprojSelectionGroup(t, location, companion)
	if group.Kind != cacheSelectionKindVisionEncoder {
		t.Fatalf("dot-separated companion kind=%q, want %q", group.Kind, cacheSelectionKindVisionEncoder)
	}
	if !strings.HasPrefix(group.ID, "mmproj-") || group.Quant != "" {
		t.Fatalf("dot-separated companion group=%+v", group)
	}
	llm := mmprojSelectionGroup(t, location, quant)
	if llm.Kind != cacheSelectionKindLLM || llm.Quant != "Q8_0" {
		t.Fatalf("llm group=%+v", llm)
	}
	// The cache-browser endpoint reports the companion as a companion instead
	// of listing it under quantizations.
	w := cacheRequest(t, s, "GET", "/api/cache/owner/model?type=model", "")
	if w.Code != http.StatusOK {
		t.Fatalf("cache info status=%d body=%s", w.Code, w.Body.String())
	}
	var info CachedRepoInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode cache info: %v: %s", err, w.Body.String())
	}
	if !info.HasMMProj {
		t.Fatalf("cache info did not report the companion: %+v", info)
	}
	reported := false
	for _, file := range info.MMProjFiles {
		if filepath.Base(file) == companion {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("cache info mmprojFiles=%v, want the dot-separated companion", info.MMProjFiles)
	}
	for _, quantization := range info.Quantizations {
		if quantization == "F16" {
			t.Fatalf("cache info reported the companion as a quantization: %v", info.Quantizations)
		}
	}
}

// Deleting an LLM quantization must not implicitly delete a companion.
func TestSelectedDeleteKeepsMMProjCompanionsWhenDeletingLLMQuant(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	for _, name := range []string{"mmproj-F16.gguf", "mmproj-F32.gguf", "model-Q4_K_M.gguf"} {
		writeSelectionFile(t, repoDir, name)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK {
		t.Fatalf("selection status=%d", code)
	}
	location := mmprojLocalLocation(t, selection, root)
	group := mmprojSelectionGroup(t, location, "model-Q4_K_M.gguf")
	if group.Kind != cacheSelectionKindLLM {
		t.Fatalf("llm group=%+v", group)
	}
	body, err := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: location.ID, GroupID: group.ID, Members: group.Members})
	if err != nil {
		t.Fatal(err)
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("llm delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(repoDir, "model-Q4_K_M.gguf")); !os.IsNotExist(err) {
		t.Fatalf("selected llm quant remains: %v", err)
	}
	for _, name := range []string{"mmproj-F16.gguf", "mmproj-F32.gguf"} {
		if _, err := os.Stat(filepath.Join(repoDir, name)); err != nil {
			t.Fatalf("companion %s was removed with the llm quant: %v", name, err)
		}
	}
	code, selection = getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK {
		t.Fatalf("refreshed selection status=%d", code)
	}
	location = mmprojLocalLocation(t, selection, root)
	for _, name := range []string{"mmproj-F16.gguf", "mmproj-F32.gguf"} {
		group := mmprojSelectionGroup(t, location, name)
		if !group.CanDelete || group.Kind != cacheSelectionKindVisionEncoder {
			t.Fatalf("companion %s is no longer independently deletable: %+v", name, group)
		}
	}
}

// Deleting an LLM quantization in the HF cache must not implicitly delete a
// companion.
func TestSelectedHFDeleteKeepsMMProjCompanionWhenDeletingLLMQuant(t *testing.T) {
	s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})
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
	companion := store("version-a", "mmproj-F32.gguf", "companion payload")
	llm := store("version-a", "model-Q4_K_M.gguf", "llm payload")
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
	group := mmprojSelectionGroup(t, *location, "model-Q4_K_M.gguf")
	if group.Kind != cacheSelectionKindLLM {
		t.Fatalf("llm group=%+v", group)
	}
	body, err := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: location.ID, GroupID: group.ID, Members: group.Members})
	if err != nil {
		t.Fatal(err)
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("HF llm delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(llm.SnapshotPath); !os.IsNotExist(err) {
		t.Fatalf("selected llm snapshot remains: %v", err)
	}
	if _, err := os.Lstat(companion.SnapshotPath); err != nil {
		t.Fatalf("companion snapshot was removed with the llm quant: %v", err)
	}
	if _, err := os.Lstat(companion.FriendlyPath); err != nil {
		t.Fatalf("companion friendly entry was removed with the llm quant: %v", err)
	}
	if _, err := os.Stat(companion.BlobPath); err != nil {
		t.Fatalf("companion payload was removed with the llm quant: %v", err)
	}
	// The companion stays selectable in the HF location.
	code, selection = getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK {
		t.Fatalf("refreshed selection status=%d", code)
	}
	location = nil
	for i := range selection.Locations {
		if strings.HasPrefix(selection.Locations[i].ID, "hf-") {
			location = &selection.Locations[i]
		}
	}
	if location == nil {
		t.Fatal("refreshed HF location missing")
	}
	group = mmprojSelectionGroup(t, *location, "mmproj-F32.gguf")
	if !group.CanDelete || group.Kind != cacheSelectionKindVisionEncoder {
		t.Fatalf("HF companion group=%+v", group)
	}
}

// A numbered companion split groups as one artifact and keeps the existing
// incomplete-shard warning.
func TestCacheSelectionMMProjNumberedSplitGroupsAsOneArtifact(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	writeSelectionFile(t, repoDir, "mmproj-F32-00001-of-00002.gguf")
	writeSelectionFile(t, repoDir, "mmproj-F32-00002-of-00002.gguf")
	writeSelectionFile(t, repoDir, "mmproj-F16-00001-of-00002.gguf")
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK {
		t.Fatalf("selection status=%d", code)
	}
	location := mmprojLocalLocation(t, selection, root)
	complete := mmprojSelectionGroup(t, location, "mmproj-F32-00001-of-00002.gguf")
	if complete.Kind != cacheSelectionKindVisionEncoder {
		t.Fatalf("split companion kind=%q", complete.Kind)
	}
	if len(complete.Members) != 2 || complete.Members[1].Path != "mmproj-F32-00002-of-00002.gguf" {
		t.Fatalf("split companion members=%+v", complete.Members)
	}
	if complete.Warning != "" {
		t.Fatalf("complete split reported a warning: %q", complete.Warning)
	}
	incomplete := mmprojSelectionGroup(t, location, "mmproj-F16-00001-of-00002.gguf")
	if len(incomplete.Members) != 1 {
		t.Fatalf("incomplete split members=%+v", incomplete.Members)
	}
	if !strings.Contains(incomplete.Warning, "incomplete") {
		t.Fatalf("incomplete split warning=%q", incomplete.Warning)
	}
}

// The shipped cache-selection UI is exercised through the Node harness in
// testdata/cache-selection-ui.cjs, which covers the companion group rendering
// and its exact DELETE body.
func TestCacheSelectionUICompanionGroups(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: run testdata/cache-selection-ui.cjs with Node to verify cache-selection UI")
	}
	cmd := exec.Command(node, "testdata/cache-selection-ui.cjs")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cache-selection UI regression: %v\n%s", err, out)
	}
	t.Log(string(out))
}
