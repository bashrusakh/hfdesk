// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"reflect"
	"testing"
)

// TestAnalyzeGGUF_LegacyGGUFSplitShards verifies that the legacy
// ".gguf-split" shard extension participates in GGUF detection, grouping, and
// recommendation exactly like ".gguf" shards.
func TestAnalyzeGGUF_LegacyGGUFSplitShards(t *testing.T) {
	files := []FileInfo{
		{Name: "model-Q4_K_M-00001-of-00002.gguf-split", Path: "model-Q4_K_M-00001-of-00002.gguf-split", Size: 100},
		{Name: "model-Q4_K_M-00002-of-00002.gguf-split", Path: "model-Q4_K_M-00002-of-00002.gguf-split", Size: 150},
		{Name: "model-Q6_K-00001-of-00002.gguf-split", Path: "model-Q6_K-00001-of-00002.gguf-split", Size: 90},
		{Name: "model-Q6_K-00002-of-00002.gguf-split", Path: "model-Q6_K-00002-of-00002.gguf-split", Size: 95},
		{Name: "config.json", Path: "config.json", Size: 10},
	}
	info := analyzeGGUF(files)
	if info == nil {
		t.Fatal("analyzeGGUF returned nil for .gguf-split shards")
	}
	if got := len(info.Quantizations); got != 2 {
		t.Fatalf("got %d quantizations, want 2: %+v", got, info.Quantizations)
	}

	byName := make(map[string]GGUFQuantization, len(info.Quantizations))
	for _, q := range info.Quantizations {
		byName[q.Name] = q
	}
	q4, ok := byName["Q4_K_M"]
	if !ok {
		t.Fatalf("Q4_K_M group missing: %+v", info.Quantizations)
	}
	if got, want := q4.Files, []string{"model-Q4_K_M-00001-of-00002.gguf-split", "model-Q4_K_M-00002-of-00002.gguf-split"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Q4_K_M Files = %v, want %v", got, want)
	}
	if q4.Parts != 2 || q4.File.Size != 250 {
		t.Fatalf("Q4_K_M parts/size = %d/%d, want 2/250", q4.Parts, q4.File.Size)
	}
	q6, ok := byName["Q6_K"]
	if !ok || q6.Parts != 2 || q6.File.Size != 185 {
		t.Fatalf("Q6_K group = %+v, want two shards totaling 185 bytes", q6)
	}

	items := GGUFToSelectableItems(info)
	var recommended []SelectableItem
	for _, item := range items {
		if item.Category == "quantization" && item.Recommended {
			recommended = append(recommended, item)
		}
	}
	if len(recommended) != 1 {
		t.Fatalf("got %d recommended quantizations, want exactly 1: %+v", len(recommended), recommended)
	}
	if got, want := recommended[0].Files, q4.Files; !reflect.DeepEqual(got, want) {
		t.Fatalf("recommended Files = %v, want complete Q4 split group %v", got, want)
	}
}

// TestStripSplitSuffixLegacyGGUFSplit covers both the old ".gguf-split" shard
// spelling and space-delimited model/quant names.
func TestStripSplitSuffixLegacyGGUFSplit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   string
		isPart bool
	}{
		{"model-Q4_K_M-00001-of-00002.gguf-split", "model-Q4_K_M", true},
		{"model Q5_K_M 00002-of-00002.gguf-split", "model Q5_K_M", true},
		{"model-Q6_K-00001-of-00002.gguf", "model-Q6_K", true},
		{"plain-Q4_K_M.gguf", "plain-Q4_K_M.gguf", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripSplitSuffix(tc.name); got != tc.want {
				t.Errorf("stripSplitSuffix(%q) = %q, want %q", tc.name, got, tc.want)
			}
			if got := isSplitShard(tc.name); got != tc.isPart {
				t.Errorf("isSplitShard(%q) = %t, want %t", tc.name, got, tc.isPart)
			}
		})
	}
}

// TestAnalyzeGGUF_SpaceDelimitedQuantNames verifies that quantization tokens
// delimited by spaces are still recognized and grouped.
func TestAnalyzeGGUF_SpaceDelimitedQuantNames(t *testing.T) {
	files := []FileInfo{
		{Name: "Mistral 7B Q4_K_M.gguf", Path: "Mistral 7B Q4_K_M.gguf", Size: 4_000_000_000},
		{Name: "model Q6_K.gguf", Path: "model Q6_K.gguf", Size: 5_000_000_000},
		{Name: "model Q5_K_M 00001-of-00002.gguf-split", Path: "model Q5_K_M 00001-of-00002.gguf-split", Size: 1_000_000_000},
		{Name: "model Q5_K_M 00002-of-00002.gguf-split", Path: "model Q5_K_M 00002-of-00002.gguf-split", Size: 1_100_000_000},
	}
	info := analyzeGGUF(files)
	if info == nil {
		t.Fatal("analyzeGGUF returned nil for space-delimited names")
	}
	if got := len(info.Quantizations); got != 3 {
		t.Fatalf("got %d quantizations, want 3: %+v", got, info.Quantizations)
	}
	byName := make(map[string]GGUFQuantization, len(info.Quantizations))
	for _, q := range info.Quantizations {
		byName[q.Name] = q
	}
	if _, ok := byName["Q4_K_M"]; !ok {
		t.Errorf("space-delimited Q4_K_M was not recognized: %+v", info.Quantizations)
	}
	if _, ok := byName["Q6_K"]; !ok {
		t.Errorf("space-delimited Q6_K was not recognized: %+v", info.Quantizations)
	}
	q5, ok := byName["Q5_K_M"]
	if !ok {
		t.Fatalf("space-delimited Q5_K_M shards were not grouped: %+v", info.Quantizations)
	}
	if q5.Parts != 2 || q5.File.Size != 2_100_000_000 {
		t.Fatalf("Q5_K_M parts/size = %d/%d, want 2/2100000000", q5.Parts, q5.File.Size)
	}
}

func TestDetectTypeRecognizesLegacyGGUFSplit(t *testing.T) {
	files := []FileInfo{{
		Name: "model-Q4_K_M-00001-of-00002.gguf-split",
		Path: "model-Q4_K_M-00001-of-00002.gguf-split",
	}}
	if got := new(Analyzer).detectType(files, false); got != TypeGGUF {
		t.Fatalf("detectType(.gguf-split) = %q, want %q", got, TypeGGUF)
	}
}

// TestParseGGUFSplitShardDelimitersAndExtensions pins the shared shard grammar
// used by both smartdl grouping and the server cache-selection model.
func TestParseGGUFSplitShardDelimitersAndExtensions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wantIndex  int
		wantTotal  int
		wantOK     bool
		wantFamily string
	}{
		{"model-Q4_K_M-00001-of-00002.gguf", 1, 2, true, "model-Q4_K_M-{part}-of-00002.gguf"},
		{"model Q5_K_M 00002-of-00002.gguf-split", 2, 2, true, "model Q5_K_M {part}-of-00002.gguf-split"},
		{"model-Q6_K_00001-of-00002.gguf", 1, 2, true, "model-Q6_K_{part}-of-00002.gguf"},
		{"model-Q8_0_00001_of_00002.gguf-split", 1, 2, true, "model-Q8_0_{part}_of_00002.gguf-split"},
		{"invalid-00000-of-00000.gguf", 0, 0, true, "invalid-{part}-of-00000.gguf"},
		{"plain-Q4_K_M.gguf", 0, 0, false, ""},
		{"model-Q4_K_M_EXTRA.gguf", 0, 0, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shard, ok := ParseGGUFSplitShard(tc.name)
			if ok != tc.wantOK || shard.Index != tc.wantIndex || shard.Total != tc.wantTotal {
				t.Fatalf("ParseGGUFSplitShard(%q) = %+v/%t, want index=%d total=%d ok=%t", tc.name, shard, ok, tc.wantIndex, tc.wantTotal, tc.wantOK)
			}
			if !ok {
				return
			}
			family := tc.name[:shard.Start] + "{part}" + tc.name[shard.End:]
			if family != tc.wantFamily {
				t.Fatalf("family for %q = %q, want %q", tc.name, family, tc.wantFamily)
			}
		})
	}
}

// TestExportedGGUFClassifier pins the classifier surface consumed by the
// server cache metadata/selection/delete paths.
func TestExportedGGUFClassifier(t *testing.T) {
	for _, tc := range []struct {
		name, path, wantQuant string
		wantGGUF              bool
	}{
		{"model-Q4_K_M.gguf", "model-Q4_K_M.gguf", "Q4_K_M", true},
		{"model-Q4_K_M-00001-of-00002.gguf-split", "model-Q4_K_M-00001-of-00002.gguf-split", "Q4_K_M", true},
		{"model Q5_K_M 00002-of-00002.gguf-split", "model Q5_K_M 00002-of-00002.gguf-split", "Q5_K_M", true},
		{"shard-00001-of-00002.gguf", "variants/UD-Q4_K_XL/shard-00001-of-00002.gguf", "UD_Q4_K_XL", true},
		{"PQ2_0.gguf", "PQ2_0.gguf", "", true},
		{"model-Q4_K_M_EXTRA.gguf", "model-Q4_K_M_EXTRA.gguf", "", true},
		{"config.json", "config.json", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsGGUFFileName(tc.name); got != tc.wantGGUF {
				t.Errorf("IsGGUFFileName(%q) = %t, want %t", tc.name, got, tc.wantGGUF)
			}
			if got := GGUFQuantType(tc.name, tc.path); got != tc.wantQuant {
				t.Errorf("GGUFQuantType(%q, %q) = %q, want %q", tc.name, tc.path, got, tc.wantQuant)
			}
		})
	}
}
