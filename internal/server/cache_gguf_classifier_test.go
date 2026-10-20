// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// splitSelectionGroup returns the selection group that owns memberPath.
func splitSelectionGroup(t *testing.T, location cacheSelectionLocation, memberPath string) cacheSelectionGroup {
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

func splitSelectionPaths(group cacheSelectionGroup) []string {
	paths := make([]string, 0, len(group.Members))
	for _, member := range group.Members {
		paths = append(paths, member.Path)
	}
	return paths
}

// TestCacheQuantLabelRequiresCompleteToken pins the classifier boundary rules
// shared with the downloader: an extended token is not a shorter known quant,
// and legacy split shards are stripped before matching.
func TestCacheQuantLabelRequiresCompleteToken(t *testing.T) {
	for name, want := range map[string]string{
		"model-Q4_K_M.gguf":                            "Q4_K_M",
		"UD-Q4_K_XL.gguf":                              "UD_Q4_K_XL",
		"model-Q4_K_M-00001-of-00002.gguf-split":       "Q4_K_M",
		"model Q5_K_M 00001-of-00002.gguf-split":       "Q5_K_M",
		"model-Q6_K_00001-of-00002.gguf":               "Q6_K",
		"PQ2_0.gguf":                                   "",
		"model-Q4_K_M_EXTRA.gguf":                      "",
		"model-Q4_K_M_EXTRA_00001-of-00002.gguf":       "",
		"model-Q4_K_M_EXTRA_00001-of-00002.gguf-split": "",
	} {
		if got := cacheQuantLabel(name); got != want {
			t.Errorf("cacheQuantLabel(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestLocalCacheSplitGGUFDiscoveryAndMetadata covers a local-only repo whose
// only recognized weights are legacy ".gguf-split" shards, plus quant labels
// that must remain Unknown because they extend a known token.
func TestLocalCacheSplitGGUFDiscoveryAndMetadata(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	repoDir := filepath.Join(root, "owner", "split-only")
	for _, name := range []string{
		"model-Q4_K_M-00001-of-00002.gguf-split",
		"model-Q4_K_M-00002-of-00002.gguf-split",
		"model Q6_K 00001-of-00002.gguf-split",
		"model Q6_K 00002-of-00002.gguf-split",
		"PQ2_0.gguf",
		"model-Q4_K_M_EXTRA.gguf",
	} {
		writeSelectionFile(t, repoDir, name)
	}
	if !hasLocalWeightFile(repoDir, nil) {
		t.Fatal(".gguf-split-only repository was not recognized as a local weight repo")
	}
	meta := collectCacheGGUFMetadata(repoDir, true, nil)
	if want := []string{"Q4_K_M", "Q6_K"}; !reflect.DeepEqual(meta.Quantizations, want) {
		t.Fatalf("cache quantizations = %v, want %v (PQ2_0/extended tokens must stay Unknown)", meta.Quantizations, want)
	}
	if meta.HasMMProj {
		t.Fatal("split-only LLM repo reported an mmproj companion")
	}

	repos, err := scanLocalCachedRepos(cacheDir, root, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Repo != "owner/split-only" {
		t.Fatalf("local scan = %+v, want only owner/split-only", repos)
	}
	if want := []string{"Q4_K_M", "Q6_K"}; !reflect.DeepEqual(repos[0].Quantizations, want) {
		t.Fatalf("scanned quantizations = %v, want %v", repos[0].Quantizations, want)
	}
	repo, err := findLocalCachedRepo(cacheDir, root, nil, nil, "owner/split-only", true)
	if err != nil {
		t.Fatalf("findLocalCachedRepo(.gguf-split-only): %v", err)
	}
	if repo.FileCount != 6 || len(repo.Files) != 6 {
		t.Fatalf("split-only file inventory = %d/%d, want 6", repo.FileCount, len(repo.Files))
	}
}

// TestCacheSelectionGroupsSplitSuffixDelimiters covers hyphen, space, and
// underscore shard delimiters across .gguf and .gguf-split, complete shard
// totals, and unclassified extended quant tokens.
func TestCacheSelectionGroupsSplitSuffixDelimiters(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	for _, name := range []string{
		"hyphen/model-Q4_K_M-00001-of-00002.gguf",
		"hyphen/model-Q4_K_M-00002-of-00002.gguf",
		"space/model Q6_K 00001-of-00002.gguf-split",
		"space/model Q6_K 00002-of-00002.gguf-split",
		"underscore/model-Q8_0_00001-of-00002.gguf-split",
		"underscore/model-Q8_0_00002-of-00002.gguf-split",
		"underscore_of/model-Q5_K_M_00001_of_00002.gguf",
		"underscore_of/model-Q5_K_M_00002_of_00002.gguf",
		"PQ2_0.gguf",
		"model-Q4_K_M_EXTRA.gguf",
	} {
		writeSelectionFile(t, repoDir, name)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})
	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK || len(selection.Locations) != 1 {
		t.Fatalf("selection=%d %+v", code, selection)
	}
	location := selection.Locations[0]
	for _, tc := range []struct {
		members []string
		quant   string
	}{
		{[]string{"hyphen/model-Q4_K_M-00001-of-00002.gguf", "hyphen/model-Q4_K_M-00002-of-00002.gguf"}, "Q4_K_M"},
		{[]string{"space/model Q6_K 00001-of-00002.gguf-split", "space/model Q6_K 00002-of-00002.gguf-split"}, "Q6_K"},
		{[]string{"underscore/model-Q8_0_00001-of-00002.gguf-split", "underscore/model-Q8_0_00002-of-00002.gguf-split"}, "Q8_0"},
		{[]string{"underscore_of/model-Q5_K_M_00001_of_00002.gguf", "underscore_of/model-Q5_K_M_00002_of_00002.gguf"}, "Q5_K_M"},
	} {
		group := splitSelectionGroup(t, location, tc.members[0])
		if group.Quant != tc.quant {
			t.Errorf("group for %q quant = %q, want %q", tc.members[0], group.Quant, tc.quant)
		}
		if group.Warning != "" {
			t.Errorf("complete shard group %q warned: %q", tc.members[0], group.Warning)
		}
		if got := splitSelectionPaths(group); !reflect.DeepEqual(got, tc.members) {
			t.Errorf("group members = %v, want %v", got, tc.members)
		}
		if other := splitSelectionGroup(t, location, tc.members[1]); other.ID != group.ID {
			t.Errorf("shards %q and %q split into %q/%q", tc.members[0], tc.members[1], group.ID, other.ID)
		}
	}
	for _, name := range []string{"PQ2_0.gguf", "model-Q4_K_M_EXTRA.gguf"} {
		group := splitSelectionGroup(t, location, name)
		if group.Quant != "" {
			t.Errorf("%s quant = %q, want Unknown", name, group.Quant)
		}
	}
}

// TestSelectedDeleteRemovesSplitSuffixGroups proves local discovery and
// selected deletion operate on both ".gguf" and ".gguf-split" groupings with
// space and underscore shard delimiters.
func TestSelectedDeleteRemovesSplitSuffixGroups(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "owner", "model")
	hyphen := []string{
		"hyphen/model-Q4_K_M-00001-of-00002.gguf-split",
		"hyphen/model-Q4_K_M-00002-of-00002.gguf-split",
	}
	space := []string{
		"space/model Q6_K 00001-of-00002.gguf-split",
		"space/model Q6_K 00002-of-00002.gguf-split",
	}
	for _, name := range []string{
		hyphen[0], hyphen[1],
		space[0], space[1],
		"plain/model-Q8_0.gguf",
	} {
		writeSelectionFile(t, repoDir, name)
	}
	s := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalScanDirs: []string{root}})

	code, selection := getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK || len(selection.Locations) != 1 {
		t.Fatalf("selection=%d %+v", code, selection)
	}
	location := selection.Locations[0]
	group := splitSelectionGroup(t, location, hyphen[0])
	if got := len(group.Members); got != 2 {
		t.Fatalf("hyphen group members = %d, want 2: %+v", got, group)
	}
	body, err := json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: location.ID, GroupID: group.ID, Members: group.Members})
	if err != nil {
		t.Fatal(err)
	}
	w := cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("hyphen .gguf-split delete status=%d body=%s", w.Code, w.Body.String())
	}
	for _, name := range hyphen {
		if _, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Errorf("selected .gguf-split member %s remains, err=%v", name, err)
		}
	}
	for _, name := range append(space, "plain/model-Q8_0.gguf") {
		if _, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(name))); err != nil {
			t.Errorf("unselected file %s changed: %v", name, err)
		}
	}

	code, selection = getSelection(t, s, "owner/model", "model", "")
	if code != http.StatusOK || len(selection.Locations) != 1 {
		t.Fatalf("refreshed selection=%d %+v", code, selection)
	}
	location = selection.Locations[0]
	group = splitSelectionGroup(t, location, space[0])
	body, err = json.Marshal(testSelectedDeleteRequest{Repo: "owner/model", Type: "model", LocationID: location.ID, GroupID: group.ID, Members: group.Members})
	if err != nil {
		t.Fatal(err)
	}
	w = cacheRequest(t, s, "DELETE", "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("space .gguf-split delete status=%d body=%s", w.Code, w.Body.String())
	}
	for _, name := range space {
		if _, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Errorf("selected space .gguf-split member %s remains, err=%v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(repoDir, "plain", "model-Q8_0.gguf")); err != nil {
		t.Errorf("unselected .gguf file changed: %v", err)
	}
}
