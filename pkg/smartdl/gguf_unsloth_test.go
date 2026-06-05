// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import "testing"

// TestQuantPattern_UnslothDynamic verifies that unsloth's "Unsloth Dynamic"
// quantization filenames (the ones with a UD- prefix and _XL/_XXL suffixes, or
// 1-bit IQ1 variants) are captured by quantPattern with their full quant type,
// not a truncated prefix. Covers github issue #72 where Q4_K_XL files were
// labeled as "Q4_K" in the web UI and filter.
//
// The filenames below are taken verbatim from
// huggingface.co/unsloth/Qwen3-30B-A3B-GGUF.
func TestQuantPattern_UnslothDynamic(t *testing.T) {
	tests := []struct {
		file string
		want string
	}{
		// Unsloth Dynamic XL variants — the main bug
		{"Qwen3-30B-A3B-UD-Q2_K_XL.gguf", "UD_Q2_K_XL"},
		{"Qwen3-30B-A3B-UD-Q3_K_XL.gguf", "UD_Q3_K_XL"},
		{"Qwen3-30B-A3B-UD-Q4_K_XL.gguf", "UD_Q4_K_XL"},
		{"Qwen3-30B-A3B-UD-Q5_K_XL.gguf", "UD_Q5_K_XL"},
		{"Qwen3-30B-A3B-UD-Q6_K_XL.gguf", "UD_Q6_K_XL"},
		{"Qwen3-30B-A3B-UD-Q8_K_XL.gguf", "UD_Q8_K_XL"},

		// 1-bit IQ variants — current regex is IQ[234] only
		{"Qwen3-30B-A3B-UD-IQ1_S.gguf", "UD_IQ1_S"},
		{"Qwen3-30B-A3B-UD-IQ1_M.gguf", "UD_IQ1_M"},

		// IQ2/IQ3 XXS/M variants present in unsloth repos — regex already
		// matches these but quality/description maps may be missing entries.
		{"Qwen3-30B-A3B-UD-IQ2_M.gguf", "UD_IQ2_M"},
		{"Qwen3-30B-A3B-UD-IQ2_XXS.gguf", "UD_IQ2_XXS"},
		{"Qwen3-30B-A3B-UD-IQ3_XXS.gguf", "UD_IQ3_XXS"},

		// Q2_K_L (non-UD but exists in unsloth repos)
		{"Qwen3-30B-A3B-Q2_K_L.gguf", "Q2_K_L"},

		// Standard quants that should still work after the regex change
		{"Qwen3-30B-A3B-Q4_K_M.gguf", "Q4_K_M"},
		{"Qwen3-30B-A3B-Q4_K_S.gguf", "Q4_K_S"},
		{"Qwen3-30B-A3B-Q4_0.gguf", "Q4_0"},
		{"Qwen3-30B-A3B-Q8_0.gguf", "Q8_0"},
		{"Qwen3-30B-A3B-Q6_K.gguf", "Q6_K"},
		{"Qwen3-30B-A3B-IQ4_NL.gguf", "IQ4_NL"},
		{"Qwen3-30B-A3B-IQ4_XS.gguf", "IQ4_XS"},
		{"Qwen3-Coder-Next-MXFP4_MOE.gguf", "MXFP4_MOE"},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got := ggufQuantType(FileInfo{Name: tt.file, Path: tt.file})
			if got != tt.want {
				t.Errorf("ggufQuantType(%q) = %q, want %q", tt.file, got, tt.want)
			}
		})
	}
}

// TestAnalyzeGGUF_UnslothRepoCoverage simulates the full analyzeGGUF pipeline
// against the file set from a real unsloth Qwen3-30B-A3B-GGUF repo and asserts
// every distinct quant filename produces a distinct, correctly-labeled entry.
// This reproduces the user-visible symptom in issue #72: before the fix, the
// six UD-Q*_K_XL files collide into six entries labeled Q2_K..Q8_K, so the
// web UI appears to be "missing" the XL variants.
func TestAnalyzeGGUF_UnslothRepoCoverage(t *testing.T) {
	ggufNames := []string{
		"Qwen3-30B-A3B-IQ4_NL.gguf",
		"Qwen3-30B-A3B-IQ4_XS.gguf",
		"Qwen3-30B-A3B-Q2_K.gguf",
		"Qwen3-30B-A3B-Q2_K_L.gguf",
		"Qwen3-30B-A3B-Q3_K_M.gguf",
		"Qwen3-30B-A3B-Q3_K_S.gguf",
		"Qwen3-30B-A3B-Q4_0.gguf",
		"Qwen3-30B-A3B-Q4_1.gguf",
		"Qwen3-30B-A3B-Q4_K_M.gguf",
		"Qwen3-30B-A3B-Q4_K_S.gguf",
		"Qwen3-30B-A3B-Q5_K_M.gguf",
		"Qwen3-30B-A3B-Q5_K_S.gguf",
		"Qwen3-30B-A3B-Q6_K.gguf",
		"Qwen3-30B-A3B-Q8_0.gguf",
		"Qwen3-30B-A3B-UD-IQ1_M.gguf",
		"Qwen3-30B-A3B-UD-IQ1_S.gguf",
		"Qwen3-30B-A3B-UD-IQ2_M.gguf",
		"Qwen3-30B-A3B-UD-IQ2_XXS.gguf",
		"Qwen3-30B-A3B-UD-IQ3_XXS.gguf",
		"Qwen3-30B-A3B-UD-Q2_K_XL.gguf",
		"Qwen3-30B-A3B-UD-Q3_K_XL.gguf",
		"Qwen3-30B-A3B-UD-Q4_K_XL.gguf",
		"Qwen3-30B-A3B-UD-Q5_K_XL.gguf",
		"Qwen3-30B-A3B-UD-Q6_K_XL.gguf",
		"Qwen3-30B-A3B-UD-Q8_K_XL.gguf",
		"Qwen3-Coder-Next-MXFP4_MOE.gguf",
	}
	files := make([]FileInfo, 0, len(ggufNames))
	for i, n := range ggufNames {
		files = append(files, FileInfo{
			Name: n, Path: n, Size: int64(1_000_000_000 + i*100_000_000),
		})
	}

	info := analyzeGGUF(files)
	if info == nil {
		t.Fatal("analyzeGGUF returned nil")
	}
	if got, want := len(info.Quantizations), len(ggufNames); got != want {
		t.Errorf("got %d quantizations, want %d", got, want)
	}

	// Build a set of labels observed and assert no "Unknown" slipped through.
	labels := make(map[string]int)
	for _, q := range info.Quantizations {
		labels[q.Name]++
	}
	for _, q := range info.Quantizations {
		if q.Name == "Unknown" {
			t.Errorf("got Unknown label (regex did not match a .gguf file); quantizations=%v", labels)
			break
		}
	}

	// Explicitly assert the UD labels are present and distinct from their
	// non-UD counterparts. Before the fix the UD entries would be labeled
	// Q2_K..Q8_K and collide with the non-UD files.
	wantPresent := []string{
		"UD_Q2_K_XL", "UD_Q3_K_XL", "UD_Q4_K_XL", "UD_Q5_K_XL", "UD_Q6_K_XL", "UD_Q8_K_XL",
		"UD_IQ1_S", "UD_IQ1_M",
		"MXFP4_MOE",
	}
	for _, w := range wantPresent {
		if labels[w] == 0 {
			t.Errorf("expected quantization label %q in result; labels=%v", w, labels)
		}
	}

	// Non-UD labels should still appear exactly once (not duplicated by a UD
	// file masquerading as the same label).
	for _, plain := range []string{"Q2_K", "Q6_K"} {
		if labels[plain] != 1 {
			t.Errorf("expected exactly one %q entry, got %d; labels=%v", plain, labels[plain], labels)
		}
	}
}

func TestAnalyzeGGUF_SortsLeastCompressedFirst(t *testing.T) {
	files := []FileInfo{
		{Name: "model-Q2_K.gguf", Path: "model-Q2_K.gguf", Size: 20},
		{Name: "model-Q8_0.gguf", Path: "model-Q8_0.gguf", Size: 80},
		{Name: "model-Q4_K_M.gguf", Path: "model-Q4_K_M.gguf", Size: 40},
	}

	info := analyzeGGUF(files)
	if info == nil {
		t.Fatal("analyzeGGUF returned nil")
	}
	got := []string{
		info.Quantizations[0].Name,
		info.Quantizations[1].Name,
		info.Quantizations[2].Name,
	}
	want := []string{"Q8_0", "Q4_K_M", "Q2_K"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %#v, want %#v", got, want)
		}
	}
}

func TestAnalyzeGGUF_UnslothSplitKeepsUDLabel(t *testing.T) {
	files := []FileInfo{
		{
			Name: "Qwen3-Coder-Next-UD-Q3_K_XL-00001-of-00002.gguf",
			Path: "Qwen3-Coder-Next-UD-Q3_K_XL-00001-of-00002.gguf",
			Size: 17_600_000_000,
		},
		{
			Name: "Qwen3-Coder-Next-UD-Q3_K_XL-00002-of-00002.gguf",
			Path: "Qwen3-Coder-Next-UD-Q3_K_XL-00002-of-00002.gguf",
			Size: 16_200_000_000,
		},
	}

	info := analyzeGGUF(files)
	if info == nil {
		t.Fatal("analyzeGGUF returned nil")
	}
	if len(info.Quantizations) != 1 {
		t.Fatalf("got %d quantizations, want 1: %#v", len(info.Quantizations), info.Quantizations)
	}
	q := info.Quantizations[0]
	if q.Name != "UD_Q3_K_XL" {
		t.Fatalf("quantization name = %q, want UD_Q3_K_XL", q.Name)
	}
	if q.Parts != 2 {
		t.Fatalf("parts = %d, want 2", q.Parts)
	}
	if q.File.Size != 33_800_000_000 {
		t.Fatalf("size = %d, want sum of shards", q.File.Size)
	}

	items := GGUFToSelectableItems(info)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].Label != "UD_Q3_K_XL" {
		t.Fatalf("item label = %q, want UD_Q3_K_XL", items[0].Label)
	}
	if items[0].FilterValue != "q3_k_xl" {
		t.Fatalf("filter = %q, want q3_k_xl", items[0].FilterValue)
	}
}
