// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"reflect"
	"testing"
)

// ramBudgetFixture is a three-quant repository ordered by descending size:
//
//	Q8_0  8 GB  EstimatedRAM 9,324,288,000
//	Q4_K_M 3 GB EstimatedRAM 3,824,288,000
//	Q3_K_S 2 GB EstimatedRAM 2,724,288,000
func ramBudgetFixture(t *testing.T) *GGUFInfo {
	t.Helper()
	info := analyzeGGUF([]FileInfo{
		ggufRecommendationTestFile("a/model-Q8_0.gguf", 8_000_000_000),
		ggufRecommendationTestFile("b/model-Q4_K_M.gguf", 3_000_000_000),
		ggufRecommendationTestFile("c/model-Q3_K_S.gguf", 2_000_000_000),
	})
	if info == nil || len(info.Quantizations) != 3 {
		t.Fatalf("fixture produced %+v, want three quantizations", info)
	}
	return info
}

func recommendedQuantFiles(t *testing.T, items []SelectableItem) []string {
	t.Helper()
	var picked [][]string
	for _, item := range items {
		if item.Category == "quantization" && item.Recommended {
			picked = append(picked, item.Files)
		}
	}
	if len(picked) != 1 {
		t.Fatalf("got %d recommended quantization rows, want exactly 1: %+v", len(picked), items)
	}
	return picked[0]
}

// TestRecommendedQuantIndexUnsetKeepsLegacyRules pins that budget <= 0 (the
// unset state, and any non-positive value) is exactly the pre-existing rule:
// first non-MTP Q4_K_M, else first non-MTP quality>=4 under 10 GiB, else none;
// MTP variants are never auto-recommended.
func TestRecommendedQuantIndexUnsetKeepsLegacyRules(t *testing.T) {
	info := ramBudgetFixture(t)
	for _, budget := range []int64{0, -1} {
		if got, want := recommendedQuantIndex(info.Quantizations, budget), 1; got != want {
			t.Fatalf("budget %d index = %d, want %d (Q4_K_M)", budget, got, want)
		}
	}

	noQ4 := analyzeGGUF([]FileInfo{
		ggufRecommendationTestFile("a/model-Q8_0.gguf", 8_000_000_000),
		ggufRecommendationTestFile("b/model-Q2_K.gguf", 1_000_000_000),
	})
	if noQ4 == nil {
		t.Fatal("fixture produced nil")
	}
	if got, want := recommendedQuantIndex(noQ4.Quantizations, 0), 0; got != want {
		t.Fatalf("unset fallback index = %d, want %d (quality>=4)", got, want)
	}

	onlyMTP := analyzeGGUF([]FileInfo{
		ggufRecommendationTestFile("a/model-MTP-Q4_K_M.gguf", 3_000_000_000),
	})
	if onlyMTP == nil {
		t.Fatal("fixture produced nil")
	}
	if got := recommendedQuantIndex(onlyMTP.Quantizations, 0); got != -1 {
		t.Fatalf("unset all-MTP index = %d, want -1", got)
	}
	if got := recommendedQuantIndex(onlyMTP.Quantizations, 1<<50); got != -1 {
		t.Fatalf("budget all-MTP index = %d, want -1 (MTP never auto-recommended)", got)
	}
}

// TestApplyGGUFRAMBudgetUnsetIsByteIdentical is the preserved-behavior row:
// with the setting unset/absent the recommendation must not change at all.
func TestApplyGGUFRAMBudgetUnsetIsByteIdentical(t *testing.T) {
	info := ramBudgetFixture(t)
	items := GGUFToSelectableItems(info)
	repo := &RepoInfo{Repo: "owner/repo", Type: TypeGGUF, GGUF: info, SelectableItems: items}
	before := append([]SelectableItem(nil), repo.SelectableItems...)

	ApplyGGUFRAMBudget(repo, 0)
	if !reflect.DeepEqual(before, repo.SelectableItems) {
		t.Fatalf("budget 0 mutated items:\nbefore %+v\nafter  %+v", before, repo.SelectableItems)
	}

	// Defensive no-ops: nil repos, non-positive budgets, and non-GGUF
	// repositories are never touched.
	ApplyGGUFRAMBudget(nil, 1<<50)
	other := &RepoInfo{Repo: "owner/repo", Type: TypeTransformers, GGUF: info, SelectableItems: items}
	otherBefore := append([]SelectableItem(nil), other.SelectableItems...)
	ApplyGGUFRAMBudget(other, 1<<50)
	if !reflect.DeepEqual(otherBefore, other.SelectableItems) {
		t.Fatal("non-GGUF repository was mutated")
	}
}

// TestApplyGGUFRAMBudgetPicksWithinBudget: when the legacy default fits the
// budget it stays the pick; when it does not, the best fitting quant by the
// existing quality heuristics wins.
func TestApplyGGUFRAMBudgetPicksWithinBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget int64
		want   []string
	}{
		{"Q4_K_M fits and stays the conventional default", 4_000_000_000, []string{"b/model-Q4_K_M.gguf"}},
		{"Q4_K_M excluded, quality heuristic picks the best fitting", 3_000_000_000, []string{"c/model-Q3_K_S.gguf"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := ramBudgetFixture(t)
			repo := &RepoInfo{Repo: "owner/repo", Type: TypeGGUF, GGUF: info, SelectableItems: GGUFToSelectableItems(info)}
			ApplyGGUFRAMBudget(repo, tc.budget)
			if got := recommendedQuantFiles(t, repo.SelectableItems); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("recommended files = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestApplyGGUFRAMBudgetRule2WithinBudget verifies the second legacy rule
// still runs over the fitting set (quality >= 4 and size < 10 GiB).
func TestApplyGGUFRAMBudgetRule2WithinBudget(t *testing.T) {
	info := analyzeGGUF([]FileInfo{
		ggufRecommendationTestFile("a/model-Q8_0.gguf", 8_000_000_000), // RAM 9.32 GB: over budget
		ggufRecommendationTestFile("b/model-Q6_K.gguf", 5_000_000_000), // RAM 6.02 GB: fits, quality 5
		ggufRecommendationTestFile("c/model-Q2_K.gguf", 1_000_000_000), // RAM 1.62 GB: fits, quality 1
	})
	if info == nil || len(info.Quantizations) != 3 {
		t.Fatalf("fixture produced %+v", info)
	}
	repo := &RepoInfo{Repo: "owner/repo", Type: TypeGGUF, GGUF: info, SelectableItems: GGUFToSelectableItems(info)}
	ApplyGGUFRAMBudget(repo, 7_000_000_000)
	if got, want := recommendedQuantFiles(t, repo.SelectableItems), []string{"b/model-Q6_K.gguf"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recommended files = %v, want %v", got, want)
	}
}

// TestApplyGGUFRAMBudgetNothingFitsFallsBackToSmallestRAM documents the
// fallback: when no quant fits the budget, the smallest-RAM non-MTP quant is
// recommended instead of dropping the recommendation.
func TestApplyGGUFRAMBudgetNothingFitsFallsBackToSmallestRAM(t *testing.T) {
	info := ramBudgetFixture(t)
	repo := &RepoInfo{Repo: "owner/repo", Type: TypeGGUF, GGUF: info, SelectableItems: GGUFToSelectableItems(info)}
	ApplyGGUFRAMBudget(repo, 1_000_000_000) // under every quant's EstimatedRAM
	if got, want := recommendedQuantFiles(t, repo.SelectableItems), []string{"c/model-Q3_K_S.gguf"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recommended files = %v, want smallest-RAM %v", got, want)
	}
}

// TestApplyGGUFRAMBudgetSkipsMTPAndForeignRows: the budget never promotes an
// MTP variant, and rows that are not this repository's quantization artifacts
// (for example branch-variant rows) keep their own recommendation state.
func TestApplyGGUFRAMBudgetSkipsMTPAndForeignRows(t *testing.T) {
	info := analyzeGGUF([]FileInfo{
		ggufRecommendationTestFile("a/model-MTP-Q8_0.gguf", 8_000_000_000),
		ggufRecommendationTestFile("b/model-Q3_K_S.gguf", 2_000_000_000),
	})
	if info == nil || len(info.Quantizations) != 2 {
		t.Fatalf("fixture produced %+v", info)
	}
	items := GGUFToSelectableItems(info)
	// A foreign revision-backed row (as the EXL branch flow appends) must
	// survive the budget untouched.
	items = append(items, SelectableItem{
		ID:            "4_25",
		Category:      "quantization",
		Revision:      "4_25",
		BranchVariant: true,
		Recommended:   true,
	})
	repo := &RepoInfo{Repo: "owner/repo", Type: TypeGGUF, GGUF: info, SelectableItems: items}
	ApplyGGUFRAMBudget(repo, 1<<50)

	byID := func(id string) SelectableItem {
		for _, item := range repo.SelectableItems {
			if item.ID == id {
				return item
			}
		}
		t.Fatalf("missing row %q: %+v", id, repo.SelectableItems)
		return SelectableItem{}
	}
	if byID("q8_0-mtp").Recommended {
		t.Error("MTP row must never be recommended")
	}
	q3 := byID("q3_k_s")
	if !q3.Recommended {
		t.Errorf("non-MTP quant should be recommended within an unbounded budget: %v", q3.Files)
	}
	if got, want := q3.Files, []string{"b/model-Q3_K_S.gguf"}; !reflect.DeepEqual(got, want) {
		t.Errorf("recommended files = %v, want %v", got, want)
	}
	if !byID("4_25").Recommended {
		t.Fatal("foreign revision-backed row lost its recommendation state")
	}
}

// TestApplyGGUFRAMBudgetRecommendedDownloadFollowsPick mirrors the server
// flow: after the budget re-pick, PopulateDownloadSelection rebuilds the
// ready-to-send recommended download from the newly recommended row.
func TestApplyGGUFRAMBudgetRecommendedDownloadFollowsPick(t *testing.T) {
	info := ramBudgetFixture(t)
	repo := &RepoInfo{Repo: "owner/repo", Type: TypeGGUF, GGUF: info, SelectableItems: GGUFToSelectableItems(info)}
	ApplyGGUFRAMBudget(repo, 1_000_000_000)
	repo.PopulateDownloadSelection()

	if repo.RecommendedDownload == nil {
		t.Fatal("recommended_download missing")
	}
	if !repo.RecommendedDownload.ExactMatch {
		t.Error("GGUF recommended download must stay exact-match")
	}
	if got, want := repo.RecommendedDownload.Filters, []string{"c/model-Q3_K_S.gguf"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recommended filters = %v, want %v", got, want)
	}
}
