// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"strings"
	"testing"
)

// gemma3MultimodalFiles is the file list from unsloth/gemma-3-4b-it-GGUF,
// a real multimodal GGUF repo where vision-encoder "mmproj" files live side
// by side with LLM quantizations. Covers github issue #76.
func gemma3MultimodalFiles() []FileInfo {
	names := []string{
		"gemma-3-4b-it-BF16.gguf",
		"gemma-3-4b-it-IQ4_NL.gguf",
		"gemma-3-4b-it-IQ4_XS.gguf",
		"gemma-3-4b-it-Q2_K.gguf",
		"gemma-3-4b-it-Q2_K_L.gguf",
		"gemma-3-4b-it-Q3_K_M.gguf",
		"gemma-3-4b-it-Q3_K_S.gguf",
		"gemma-3-4b-it-Q4_0.gguf",
		"gemma-3-4b-it-Q4_1.gguf",
		"gemma-3-4b-it-Q4_K_M.gguf",
		"gemma-3-4b-it-Q4_K_S.gguf",
		"gemma-3-4b-it-Q5_K_M.gguf",
		"gemma-3-4b-it-Q5_K_S.gguf",
		"gemma-3-4b-it-Q6_K.gguf",
		"gemma-3-4b-it-Q8_0.gguf",
		"gemma-3-4b-it-UD-IQ1_M.gguf",
		"gemma-3-4b-it-UD-IQ1_S.gguf",
		"gemma-3-4b-it-UD-IQ2_M.gguf",
		"gemma-3-4b-it-UD-IQ2_XXS.gguf",
		"gemma-3-4b-it-UD-IQ3_XXS.gguf",
		"gemma-3-4b-it-UD-Q2_K_XL.gguf",
		"gemma-3-4b-it-UD-Q3_K_XL.gguf",
		"gemma-3-4b-it-UD-Q4_K_XL.gguf",
		"gemma-3-4b-it-UD-Q5_K_XL.gguf",
		"gemma-3-4b-it-UD-Q6_K_XL.gguf",
		"gemma-3-4b-it-UD-Q8_K_XL.gguf",
		"mmproj-BF16.gguf",
		"mmproj-F16.gguf",
		"mmproj-F32.gguf",
	}
	files := make([]FileInfo, 0, len(names))
	for i, n := range names {
		files = append(files, FileInfo{
			Name: n, Path: n, Size: int64(1_000_000_000 + i*100_000_000),
		})
	}
	return files
}

// TestAnalyzeGGUF_MMProjExcludedFromQuantizations verifies that mmproj vision
// encoder files are NOT listed alongside LLM quantizations. Before the fix,
// mmproj-F16.gguf was parsed as an "F16" quant, mmproj-BF16.gguf as "BF16",
// etc., polluting the quant picker with vision-encoder files that a user
// cannot meaningfully pick as a standalone model.
func TestAnalyzeGGUF_MMProjExcludedFromQuantizations(t *testing.T) {
	info := analyzeGGUF(gemma3MultimodalFiles())
	if info == nil {
		t.Fatal("analyzeGGUF returned nil")
	}

	// 26 LLM files, 3 mmproj files. Quantizations should only include the 26.
	const wantLLMQuants = 26
	if got := len(info.Quantizations); got != wantLLMQuants {
		names := []string{}
		for _, q := range info.Quantizations {
			names = append(names, q.File.Name)
		}
		t.Errorf("got %d quantizations, want %d; quants=%v", got, wantLLMQuants, names)
	}

	// No quantization entry should point to an mmproj file.
	for _, q := range info.Quantizations {
		if strings.Contains(strings.ToLower(q.File.Name), "mmproj") {
			t.Errorf("mmproj file %q leaked into Quantizations list (label=%q)", q.File.Name, q.Name)
		}
	}

	// MMProjFiles should contain all three vision-encoder files.
	if got := len(info.MMProjFiles); got != 3 {
		names := []string{}
		for _, f := range info.MMProjFiles {
			names = append(names, f.Name)
		}
		t.Errorf("got %d MMProjFiles, want 3; files=%v", got, names)
	}
}

// TestGGUFToSelectableItems_EmitsVisionEncoderItem verifies that when mmproj
// files are present, GGUFToSelectableItems emits a "vision_encoder" category
// item that is Recommended by default. This is what makes the picker
// auto-select the vision encoder alongside any chosen LLM quant.
func TestGGUFToSelectableItems_EmitsVisionEncoderItem(t *testing.T) {
	info := analyzeGGUF(gemma3MultimodalFiles())
	if info == nil {
		t.Fatal("analyzeGGUF returned nil")
	}

	items := GGUFToSelectableItems(info)

	var quantItems, visionItems []SelectableItem
	for _, it := range items {
		switch it.Category {
		case "quantization":
			quantItems = append(quantItems, it)
		case "vision_encoder":
			visionItems = append(visionItems, it)
		}
	}

	if len(quantItems) != 26 {
		t.Errorf("got %d quantization items, want 26", len(quantItems))
	}

	// No quantization item should reference an mmproj file.
	for _, it := range quantItems {
		for _, f := range it.Files {
			if strings.Contains(strings.ToLower(f), "mmproj") {
				t.Errorf("quantization item %q references mmproj file %q", it.Label, f)
			}
		}
	}

	if len(visionItems) == 0 {
		t.Fatal("expected at least one vision_encoder SelectableItem")
	}

	// The preferred vision item should be the F16 mmproj, recommended,
	// and its FilterValue should narrowly match only that file.
	pref := visionItems[0]
	if !strings.Contains(strings.ToLower(pref.Files[0]), "mmproj-f16") {
		t.Errorf("preferred vision encoder file = %q, want mmproj-F16", pref.Files[0])
	}
	if !strings.Contains(strings.ToLower(pref.Label), "mmproj") {
		t.Errorf("vision encoder Label = %q, want it to mention mmproj filename", pref.Label)
	}
	if !pref.Recommended {
		t.Error("preferred vision encoder should be Recommended=true")
	}
	if pref.FilterValue == "" {
		t.Error("preferred vision encoder must have a FilterValue")
	}
	// FilterValue should be narrow enough to exclude BF16/F32 mmproj files.
	// Using strings.Contains semantics from the downloader filter code.
	matches := []string{}
	for _, f := range []string{"mmproj-F16.gguf", "mmproj-BF16.gguf", "mmproj-F32.gguf"} {
		if strings.Contains(strings.ToLower(f), strings.ToLower(pref.FilterValue)) {
			matches = append(matches, f)
		}
	}
	if len(matches) != 1 || matches[0] != "mmproj-F16.gguf" {
		t.Errorf("FilterValue %q matched %v, want only [mmproj-F16.gguf]", pref.FilterValue, matches)
	}

	// It should also match the corresponding LLM quant filenames? No — the
	// mmproj filter should match ONLY the mmproj file, not leak into LLM
	// quants. Spot-check with a representative LLM filename.
	if strings.Contains(strings.ToLower("gemma-3-4b-it-Q4_K_M.gguf"), strings.ToLower(pref.FilterValue)) {
		t.Errorf("vision encoder FilterValue %q unexpectedly matches LLM quant file", pref.FilterValue)
	}
}

// TestRepoInfo_MultimodalRecommendedCommandBundlesMMProj exercises the full
// analysis → SelectableItems → recommended CLI command pipeline for a real
// multimodal GGUF repo and asserts the recommended download command includes
// BOTH the LLM quant filter and the mmproj filter in a single -F list. This
// is the end-to-end behavior users care about in issue #76: "when I select
// the recommended download, mmproj comes along automatically."
func TestRepoInfo_MultimodalRecommendedCommandBundlesMMProj(t *testing.T) {
	info := &RepoInfo{
		Repo:   "unsloth/gemma-3-4b-it-GGUF",
		Type:   TypeGGUF,
		Files:  gemma3MultimodalFiles(),
		Branch: "main",
	}
	info.GGUF = analyzeGGUF(info.Files)
	populateSelectableItems(info)
	info.PopulateCLICommands()

	cmd := info.GenerateRecommendedCommand()
	if cmd == "" {
		t.Fatal("expected a non-empty recommended command")
	}
	// The recommended LLM default is Q4_K_M.
	if !strings.Contains(cmd, "q4_k_m") {
		t.Errorf("recommended command missing q4_k_m filter: %s", cmd)
	}
	// The recommended mmproj must also be present and narrow (f16, not bf16/f32).
	if !strings.Contains(cmd, "mmproj-f16") {
		t.Errorf("recommended command missing mmproj-f16 filter: %s", cmd)
	}
	// Both should appear in a single -F ... list.
	if !strings.Contains(cmd, "-F ") {
		t.Errorf("expected -F flag in recommended command: %s", cmd)
	}
}

// TestGGUFToSelectableItems_NoMMProjStillWorks verifies the non-multimodal
// case is unchanged: repos with no mmproj files produce no vision_encoder
// items and all quantizations are preserved.
func TestGGUFToSelectableItems_NoMMProjStillWorks(t *testing.T) {
	files := []FileInfo{
		{Name: "model.Q4_K_M.gguf", Path: "model.Q4_K_M.gguf", Size: 4_000_000_000},
		{Name: "model.Q5_K_M.gguf", Path: "model.Q5_K_M.gguf", Size: 5_000_000_000},
		{Name: "model.Q8_0.gguf", Path: "model.Q8_0.gguf", Size: 8_000_000_000},
	}
	info := analyzeGGUF(files)
	if info == nil {
		t.Fatal("analyzeGGUF returned nil")
	}
	if len(info.MMProjFiles) != 0 {
		t.Errorf("got %d MMProjFiles, want 0", len(info.MMProjFiles))
	}

	items := GGUFToSelectableItems(info)
	for _, it := range items {
		if it.Category == "vision_encoder" {
			t.Errorf("unexpected vision_encoder item in non-multimodal repo: %+v", it)
		}
	}
	if len(items) != 3 {
		t.Errorf("got %d items, want 3", len(items))
	}
}
