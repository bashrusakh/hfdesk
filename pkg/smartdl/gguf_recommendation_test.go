// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"path"
	"reflect"
	"strings"
	"testing"
)

func ggufRecommendationTestFile(repoPath string, size int64) FileInfo {
	return FileInfo{Name: path.Base(repoPath), Path: repoPath, Size: size}
}

func selectableItemsByCategory(items []SelectableItem, category string) []SelectableItem {
	var out []SelectableItem
	for _, item := range items {
		if item.Category == category {
			out = append(out, item)
		}
	}
	return out
}

// TestGGUFToSelectableItems_RecommendsOneQ4KMArtifact verifies that two
// path-distinct Q4_K_M artifacts remain independent selectable items but only
// one of them is part of the default/recommended download. Equal sizes are
// ordered by the analyzer's path tie-breaker, so the choice is deterministic.
func TestGGUFToSelectableItems_RecommendsOneQ4KMArtifact(t *testing.T) {
	files := []FileInfo{
		ggufRecommendationTestFile("b/model-Q4_K_M.gguf", 4_000_000_000),
		ggufRecommendationTestFile("a/model-Q4_K_M.gguf", 4_000_000_000),
	}
	info := analyzeGGUF(files)
	if info == nil {
		t.Fatal("analyzeGGUF returned nil")
	}
	if got := len(info.Quantizations); got != 2 {
		t.Fatalf("got %d quantizations, want 2: %+v", got, info.Quantizations)
	}
	// Equal size and quant name: the path tie-breaker must make a/ come first.
	if got := info.Quantizations[0].File.Path; got != "a/model-Q4_K_M.gguf" {
		t.Fatalf("first quantization path = %q, want a/model-Q4_K_M.gguf", got)
	}

	items := GGUFToSelectableItems(info)
	quantItems := selectableItemsByCategory(items, "quantization")
	if len(quantItems) != 2 {
		t.Fatalf("got %d quantization items, want 2: %+v", len(quantItems), quantItems)
	}

	ids := map[string]bool{}
	selectedFiles := map[string]bool{}
	var recommended []SelectableItem
	for _, item := range quantItems {
		if ids[item.ID] {
			t.Errorf("duplicate quantization item ID %q", item.ID)
		}
		ids[item.ID] = true
		if len(item.Files) != 1 {
			t.Fatalf("item %q files = %v, want one exact artifact path", item.Label, item.Files)
		}
		if selectedFiles[item.Files[0]] {
			t.Errorf("artifact %q is selected by more than one item", item.Files[0])
		}
		selectedFiles[item.Files[0]] = true
		if item.Recommended {
			recommended = append(recommended, item)
		}
	}
	if len(recommended) != 1 {
		t.Fatalf("got %d recommended quantizations, want exactly 1: %+v", len(recommended), recommended)
	}
	if got, want := recommended[0].Files, []string{"a/model-Q4_K_M.gguf"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recommended files = %v, want %v", got, want)
	}
	if !selectedFiles["a/model-Q4_K_M.gguf"] || !selectedFiles["b/model-Q4_K_M.gguf"] {
		t.Fatalf("both path-distinct artifacts must remain independently selectable: %v", selectedFiles)
	}

	repo := &RepoInfo{Repo: "owner/repo", Type: TypeGGUF, SelectableItems: items}
	if got, want := repo.RecommendedFilterValues(), []string{"a/model-Q4_K_M.gguf"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RecommendedFilterValues() = %v, want only the chosen artifact %v", got, want)
	}
}

// TestGGUFToSelectableItems_RecommendedShardedArtifactKeepsAllFiles verifies
// that the single recommended artifact keeps every shard in Files, and that a
// second Q4_K_M artifact is neither merged into it nor recommended.
func TestGGUFToSelectableItems_RecommendedShardedArtifactKeepsAllFiles(t *testing.T) {
	files := []FileInfo{
		ggufRecommendationTestFile("a/model-Q4_K_M-00001-of-00002.gguf", 3_000_000_000),
		ggufRecommendationTestFile("a/model-Q4_K_M-00002-of-00002.gguf", 3_000_000_000),
		ggufRecommendationTestFile("b/model-Q4_K_M.gguf", 4_000_000_000),
	}
	info := analyzeGGUF(files)
	if info == nil {
		t.Fatal("analyzeGGUF returned nil")
	}
	if got := len(info.Quantizations); got != 2 {
		t.Fatalf("got %d independent artifacts, want 2: %+v", got, info.Quantizations)
	}

	items := GGUFToSelectableItems(info)
	quantItems := selectableItemsByCategory(items, "quantization")
	if len(quantItems) != 2 {
		t.Fatalf("got %d quantization items, want 2", len(quantItems))
	}
	var recommended []SelectableItem
	for _, item := range quantItems {
		if item.Recommended {
			recommended = append(recommended, item)
		}
	}
	if len(recommended) != 1 {
		t.Fatalf("got %d recommended quantizations, want exactly 1: %+v", len(recommended), recommended)
	}
	wantFiles := []string{
		"a/model-Q4_K_M-00001-of-00002.gguf",
		"a/model-Q4_K_M-00002-of-00002.gguf",
	}
	if got := recommended[0].Files; !reflect.DeepEqual(got, wantFiles) {
		t.Fatalf("recommended sharded Files = %v, want complete shard group %v", got, wantFiles)
	}

	repo := &RepoInfo{Repo: "owner/repo", Type: TypeGGUF, SelectableItems: items}
	if got, want := repo.RecommendedFilterValues(), wantFiles; !reflect.DeepEqual(got, want) {
		t.Fatalf("RecommendedFilterValues() = %v, want only the sharded artifact's files %v", got, want)
	}
}

// TestGGUFToSelectableItems_RecommendedFallbackPicksOneArtifact covers the
// no-Q4_K_M fallback: exactly one non-MTP quality>=4 artifact under 10 GiB is
// recommended, even when several candidates (and an oversized/MTP decoy) exist.
func TestGGUFToSelectableItems_RecommendedFallbackPicksOneArtifact(t *testing.T) {
	files := []FileInfo{
		ggufRecommendationTestFile("a/model-Q8_0.gguf", 12_000_000_000),    // quality 5 but >= 10 GiB
		ggufRecommendationTestFile("b/model-MTP-Q6_K.gguf", 9_000_000_000), // MTP is never auto-recommended
		ggufRecommendationTestFile("c/model-Q6_K.gguf", 8_000_000_000),     // first eligible fallback
		ggufRecommendationTestFile("d/model-Q5_K_M.gguf", 6_000_000_000),
		ggufRecommendationTestFile("e/model-Q4_K_S.gguf", 3_000_000_000), // quality 3
	}
	info := analyzeGGUF(files)
	if info == nil {
		t.Fatal("analyzeGGUF returned nil")
	}
	if got := len(info.Quantizations); got != 5 {
		t.Fatalf("got %d quantizations, want 5: %+v", got, info.Quantizations)
	}

	items := GGUFToSelectableItems(info)
	quantItems := selectableItemsByCategory(items, "quantization")
	var recommended []SelectableItem
	for _, item := range quantItems {
		if item.Recommended {
			recommended = append(recommended, item)
		}
		if strings.Contains(item.Label, "MTP") && item.Recommended {
			t.Errorf("MTP item %q must not be auto-recommended", item.Label)
		}
	}
	if len(recommended) != 1 {
		t.Fatalf("got %d recommended quantizations, want exactly 1: %+v", len(recommended), recommended)
	}
	if got, want := recommended[0].Files, []string{"c/model-Q6_K.gguf"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recommended files = %v, want first eligible fallback %v", got, want)
	}
}

// TestGGUFToSelectableItems_PathDistinctMMProjArtifacts verifies that two
// mmproj files sharing a basename in different directories are both emitted
// with path-distinct, deterministic IDs and exact Files, while only the
// preferred one is Recommended.
func TestGGUFToSelectableItems_PathDistinctMMProjArtifacts(t *testing.T) {
	files := []FileInfo{
		ggufRecommendationTestFile("model-Q4_K_M.gguf", 4_000_000_000),
		ggufRecommendationTestFile("a/mmproj-F16.gguf", 100),
		ggufRecommendationTestFile("b/mmproj-F16.gguf", 200),
	}
	info := analyzeGGUF(files)
	if info == nil {
		t.Fatal("analyzeGGUF returned nil")
	}

	items := GGUFToSelectableItems(info)
	vision := selectableItemsByCategory(items, "vision_encoder")
	if len(vision) != 2 {
		t.Fatalf("got %d vision_encoder items, want both path-distinct mmproj files: %+v", len(vision), vision)
	}
	if !vision[0].Recommended {
		t.Errorf("preferred mmproj item %q must be Recommended", vision[0].Label)
	}
	if vision[1].Recommended {
		t.Errorf("second mmproj item %q must not be Recommended", vision[1].Label)
	}
	if got, want := vision[0].Files, []string{"a/mmproj-F16.gguf"}; !reflect.DeepEqual(got, want) {
		t.Errorf("preferred mmproj Files = %v, want %v", got, want)
	}
	if got, want := vision[1].Files, []string{"b/mmproj-F16.gguf"}; !reflect.DeepEqual(got, want) {
		t.Errorf("second mmproj Files = %v, want %v", got, want)
	}
	if vision[0].ID == vision[1].ID {
		t.Errorf("path-distinct mmproj items share ID %q", vision[0].ID)
	}

	// IDs must be deterministic across repeated conversions of the same analysis.
	again := selectableItemsByCategory(GGUFToSelectableItems(info), "vision_encoder")
	if len(again) != 2 {
		t.Fatalf("second conversion got %d vision_encoder items, want 2", len(again))
	}
	if again[0].ID != vision[0].ID || again[1].ID != vision[1].ID {
		t.Errorf("mmproj IDs are not deterministic: first=%v second=%v",
			[]string{vision[0].ID, vision[1].ID}, []string{again[0].ID, again[1].ID})
	}

	repo := &RepoInfo{Repo: "owner/repo", Type: TypeGGUF, SelectableItems: items}
	wantFilters := []string{"./model-Q4_K_M.gguf", "a/mmproj-F16.gguf"}
	if got := repo.RecommendedFilterValues(); !reflect.DeepEqual(got, wantFilters) {
		t.Fatalf("RecommendedFilterValues() = %v, want %v", got, wantFilters)
	}
}

// TestRepoInfo_RecommendedDownloadExcludesUpstreamMMProj verifies that a
// recommended upstream companion is never folded into the single-repository
// recommended request. The item itself keeps its UpstreamRepo identity and
// exact Files so the UI can offer an individual download.
func TestRepoInfo_RecommendedDownloadExcludesUpstreamMMProj(t *testing.T) {
	info := &RepoInfo{Repo: "owner/local", Type: TypeGGUF}
	info.GGUF = &GGUFInfo{
		Quantizations: []GGUFQuantization{{
			Name:    "Q4_K_M",
			Quality: 4,
			File: FileInfo{
				Name: "model-Q4_K_M.gguf",
				Path: "model-Q4_K_M.gguf",
				Size: 4_000_000_000,
			},
			Files: []string{"model-Q4_K_M.gguf"},
		}},
		MMProjFiles: []FileInfo{{
			Name: "mmproj-F16.gguf",
			Path: "mmproj-F16.gguf",
			Size: 100,
		}},
		MMProjUpstreamRepo: "owner/upstream",
	}
	populateSelectableItems(info)
	info.PopulateDownloadSelection()

	var upstream *SelectableItem
	for i := range info.SelectableItems {
		if info.SelectableItems[i].Category == "vision_encoder" {
			upstream = &info.SelectableItems[i]
			break
		}
	}
	if upstream == nil {
		t.Fatal("expected an upstream vision_encoder item")
	}
	if upstream.UpstreamRepo != "owner/upstream" {
		t.Errorf("upstream item UpstreamRepo = %q, want owner/upstream", upstream.UpstreamRepo)
	}
	if got, want := upstream.Files, []string{"mmproj-F16.gguf"}; !reflect.DeepEqual(got, want) {
		t.Errorf("upstream item Files = %v, want exact upstream path %v", got, want)
	}
	if !upstream.Recommended {
		t.Error("upstream item should remain individually Recommended")
	}

	wantFilters := []string{"./model-Q4_K_M.gguf"}
	if got := info.RecommendedFilters; !reflect.DeepEqual(got, wantFilters) {
		t.Fatalf("RecommendedFilters = %v, want only analyzed-repo files %v", got, wantFilters)
	}
	if info.RecommendedDownload == nil {
		t.Fatal("RecommendedDownload is nil, want the local artifact request")
	}
	if info.RecommendedDownload.Repo != "owner/local" {
		t.Errorf("RecommendedDownload.Repo = %q, want owner/local", info.RecommendedDownload.Repo)
	}
	if got := info.RecommendedDownload.Filters; !reflect.DeepEqual(got, wantFilters) {
		t.Fatalf("RecommendedDownload.Filters = %v, want %v", got, wantFilters)
	}
	for _, filter := range info.RecommendedDownload.Filters {
		if strings.Contains(strings.ToLower(filter), "mmproj") {
			t.Errorf("recommended request silently includes upstream mmproj filter %q", filter)
		}
	}

	t.Run("all recommended items upstream", func(t *testing.T) {
		onlyUpstream := &RepoInfo{
			Repo: "owner/local",
			Type: TypeGGUF,
			SelectableItems: []SelectableItem{{
				ID:           "mmproj-upstream",
				Label:        "mmproj-F16.gguf",
				Category:     "vision_encoder",
				Recommended:  true,
				Files:        []string{"mmproj-F16.gguf"},
				UpstreamRepo: "owner/upstream",
			}},
		}
		onlyUpstream.PopulateDownloadSelection()
		if len(onlyUpstream.RecommendedFilters) != 0 {
			t.Errorf("RecommendedFilters = %v, want empty", onlyUpstream.RecommendedFilters)
		}
		if onlyUpstream.RecommendedDownload != nil {
			t.Errorf("RecommendedDownload = %+v, want nil when every recommended item is upstream", onlyUpstream.RecommendedDownload)
		}
	})
}
