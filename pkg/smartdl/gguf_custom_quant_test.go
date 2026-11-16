// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"path/filepath"
	"reflect"
	"testing"
)

// TestGGUFQuantTypeRecognizesPrismMLTernaryTypes pins the two custom PrismML
// packing tokens as exact literals across the filename, path, and split-shard
// spellings the shared classifier accepts. The Q2_0 control guards against
// reading PQ2_0's suffix as the standard Q2_0 token.
func TestGGUFQuantTypeRecognizesPrismMLTernaryTypes(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"Ternary-Bonsai-2-27B-PTQ1_0.gguf", "PTQ1_0"},
		{"Ternary-Bonsai-2-27B-PQ2_0.gguf", "PQ2_0"},
		{"Ternary-Bonsai-2-27B-Q2_0.gguf", "Q2_0"},
		{"ternary-bonsai-2-27b-ptq1_0.gguf", "PTQ1_0"},
		{"model.PTQ1_0.gguf", "PTQ1_0"},
		{"model_PQ2_0.gguf", "PQ2_0"},
		{"model PTQ1_0.gguf", "PTQ1_0"},
		{"model-PTQ1_0-00001-of-00002.gguf", "PTQ1_0"},
		{"model-PTQ1_0-00002-of-00002.gguf", "PTQ1_0"},
		{"model-PQ2_0-00001-of-00002.gguf", "PQ2_0"},
		{"model-PTQ1_0_00001-of-00002.gguf", "PTQ1_0"},
		{"model-PQ2_0_00001-of-00002.gguf-split", "PQ2_0"},
		{"PTQ1_0/shard-00001-of-00002.gguf", "PTQ1_0"},
		{"PQ2_0/shard-00001-of-00002.gguf", "PQ2_0"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := ggufQuantType(FileInfo{Name: filepath.Base(tc.path), Path: tc.path}); got != tc.want {
				t.Fatalf("ggufQuantType(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestGGUFQuantTypeRejectsExtendedPrismMLTokens keeps the classifier's
// whole-token rule: only the exact PTQ1_0/PQ2_0 literals are recognized, so
// extended spellings and prefix variants stay unclassified.
func TestGGUFQuantTypeRejectsExtendedPrismMLTokens(t *testing.T) {
	for _, path := range []string{
		"XPTQ1_0.gguf",
		"XPQ2_0.gguf",
		"PTQ1_0_EXTRA.gguf",
		"model-PTQ1_0_EXTRA.gguf",
		"PQ2_0_EXTRA.gguf",
		"PTQ1_0CUSTOM.gguf",
		"PQ2_0X.gguf",
		"PTQ1_1.gguf",
		"PQ9_9.gguf",
		"model-PTQ1_0_EXTRA_00001-of-00002.gguf",
		"model-PQ2_0_EXTRA_00001-of-00002.gguf-split",
	} {
		t.Run(path, func(t *testing.T) {
			if got := ggufQuantType(FileInfo{Name: filepath.Base(path), Path: path}); got != "" {
				t.Fatalf("ggufQuantType(%q) = %q, want unclassified", path, got)
			}
		})
	}
}

// TestPrismMLTernaryQuantQualityAndDescriptions pins the 2-bit quality class
// and the runtime-requirement description for both custom types.
func TestPrismMLTernaryQuantQualityAndDescriptions(t *testing.T) {
	for name, wantDesc := range map[string]string{
		"PTQ1_0": "PTQ1_0 — ternary g128, 1.75 bits/weight · requires the PrismML llama.cpp fork",
		"PQ2_0":  "PQ2_0 — ternary g128, 2.13 bits/weight · requires the PrismML llama.cpp fork",
	} {
		t.Run(name, func(t *testing.T) {
			if got := quantQuality[name]; got != 2 {
				t.Errorf("quantQuality[%q] = %d, want 2", name, got)
			}
			if got := quantDescriptions[name]; got != wantDesc {
				t.Errorf("quantDescriptions[%q] = %q, want %q", name, got, wantDesc)
			}
		})
	}
}

// prismMLRecommendationFixture is the Bonsai-shaped repository: the two custom
// packings plus the standard Q2_0 and a full-precision F16 large enough to stay
// outside the quality>=4/under-10-GiB rule. Without an eligible artifact the
// default recommendation must stay empty.
func prismMLRecommendationFixture(t *testing.T) *GGUFInfo {
	t.Helper()
	info := analyzeGGUF([]FileInfo{
		ggufRecommendationTestFile("model-F16.gguf", 54*1024*1024*1024),
		ggufRecommendationTestFile("model-PQ2_0.gguf", 7_210_000_000),
		ggufRecommendationTestFile("model-PTQ1_0.gguf", 5_950_000_000),
		ggufRecommendationTestFile("model-Q2_0.gguf", 5_950_000_000),
	})
	if info == nil || len(info.Quantizations) != 4 {
		t.Fatalf("fixture produced %+v, want four quantizations", info)
	}
	return info
}

// TestPrismMLTernaryTypesNeverAutoRecommended pins that the custom packings
// produce labelled rows with 2-bit quality but no Recommended badge, and that
// the unset-budget recommendation stays empty for the whole fixture.
func TestPrismMLTernaryTypesNeverAutoRecommended(t *testing.T) {
	info := prismMLRecommendationFixture(t)
	if got := recommendedQuantIndex(info.Quantizations, 0); got != -1 {
		t.Fatalf("unset-budget recommended index = %d, want -1", got)
	}
	for _, item := range GGUFToSelectableItems(info) {
		if item.Category == "quantization" && item.Recommended {
			t.Fatalf("unexpected recommended quant row %+v", item)
		}
	}

	byName := make(map[string]GGUFQuantization, len(info.Quantizations))
	for _, q := range info.Quantizations {
		byName[q.Name] = q
	}
	for _, name := range []string{"PTQ1_0", "PQ2_0"} {
		q, ok := byName[name]
		if !ok {
			t.Fatalf("%s row missing: %+v", name, info.Quantizations)
		}
		if q.Quality != 2 || q.QualityStars != "★★☆☆☆" {
			t.Errorf("%s quality = %d %q, want 2 stars", name, q.Quality, q.QualityStars)
		}
	}
}

// TestPrismMLTernaryTypesSkippedInRAMFallback covers the nothing-fits path: the
// smallest-RAM fallback must pass over the custom packings. A repository with
// only custom packings therefore keeps no recommendation at all.
func TestPrismMLTernaryTypesSkippedInRAMFallback(t *testing.T) {
	t.Run("ordinary quant wins the fallback", func(t *testing.T) {
		info := prismMLRecommendationFixture(t)
		repo := &RepoInfo{Repo: "owner/repo", Type: TypeGGUF, GGUF: info, SelectableItems: GGUFToSelectableItems(info)}
		ApplyGGUFRAMBudget(repo, 1) // below every EstimatedRAM
		if got, want := recommendedQuantFiles(t, repo.SelectableItems), []string{"model-Q2_0.gguf"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("fallback recommended files = %v, want %v", got, want)
		}
		for _, item := range repo.SelectableItems {
			if item.Category != "quantization" || !item.Recommended {
				continue
			}
			if len(item.Files) == 1 && (item.Files[0] == "model-PTQ1_0.gguf" || item.Files[0] == "model-PQ2_0.gguf") {
				t.Fatalf("custom-runtime packing %q was auto-recommended", item.Files[0])
			}
		}
	})

	t.Run("only custom packings leave no recommendation", func(t *testing.T) {
		info := analyzeGGUF([]FileInfo{
			ggufRecommendationTestFile("model-PTQ1_0.gguf", 5_950_000_000),
			ggufRecommendationTestFile("model-PQ2_0.gguf", 7_210_000_000),
			ggufRecommendationTestFile("mmproj-F16.gguf", 100_000_000),
		})
		if info == nil || len(info.Quantizations) != 2 || len(info.MMProjFiles) != 1 {
			t.Fatalf("fixture produced %+v, want two custom packings and one projector", info)
		}
		if got := recommendedQuantIndex(info.Quantizations, 1); got != -1 {
			t.Fatalf("fallback recommended index = %d, want -1", got)
		}
		repo := &RepoInfo{Repo: "owner/repo", Type: TypeGGUF, GGUF: info, SelectableItems: GGUFToSelectableItems(info)}
		ApplyGGUFRAMBudget(repo, 1)
		for _, item := range repo.SelectableItems {
			if item.Recommended {
				t.Fatalf("custom-only fallback recommended %+v", item)
			}
		}
	})
}
