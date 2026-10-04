// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import "testing"

// TestFilterMatches covers substring (default) vs exact segment matching.
// Exact mode is the fix for github issue #78, where selecting the Q6_K quant
// also pulled in the UD-Q6_K_XL variant because of substring matching.
func TestFilterMatches(t *testing.T) {
	tests := []struct {
		name   string // file name (already lowercased by caller in plan.go)
		filter string // filter (already lowercased)
		exact  bool
		want   bool
	}{
		// The reported bug: substring matches the XL variant, exact does not.
		{"gemma-3-27b-it-q6_k.gguf", "q6_k", false, true},
		{"gemma-3-27b-it-ud-q6_k_xl.gguf", "q6_k", false, true}, // surprising (old behavior)
		{"gemma-3-27b-it-q6_k.gguf", "q6_k", true, true},
		{"gemma-3-27b-it-ud-q6_k_xl.gguf", "q6_k", true, false}, // fixed

		// The XL filter still selects only the XL file in exact mode.
		{"gemma-3-27b-it-ud-q6_k_xl.gguf", "q6_k_xl", true, true},
		{"gemma-3-27b-it-q6_k.gguf", "q6_k_xl", true, false},

		// q4_k must not pull q4_k_m in exact mode.
		{"qwen3-30b-a3b-q4_k_m.gguf", "q4_k", true, false},
		{"qwen3-30b-a3b-q4_k.gguf", "q4_k", true, true},
		{"qwen3-30b-a3b-q4_k_m.gguf", "q4_k_m", true, true},

		// Documented coarse cases still work in exact mode because the token is
		// a whole segment (extension / format).
		{"model.q4_0.gguf", "gguf", true, true},
		{"model.safetensors", "safetensors", true, true},
		{"model.q4_0.gguf", "q4_0", true, true},

		// Split files: the quant is still a whole segment.
		{"qwen3-q6_k-00001-of-00002.gguf", "q6_k", true, true},

		// APEX profile names use hyphenated multi-segment labels in filenames.
		{"qwen3-coder-next-apex-i-quality.gguf", "apex-i-quality", true, true},
		{"qwen3-coder-next-apex-quality.gguf", "apex-i-quality", true, false},
		{"qwen3-coder-next-apex-quality.gguf", "apex-quality", true, true},
		{"qwen3-coder-next-apex-i-quality.gguf", "apex-quality", true, false},

		// The one coarse-prefix case exact mode intentionally drops.
		{"qwen3-30b-a3b-q4_k_m.gguf", "q4", true, false},
		{"qwen3-30b-a3b-q4_k_m.gguf", "q4", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/"+tt.filter, func(t *testing.T) {
			if got := filterMatches(tt.name, tt.filter, tt.exact); got != tt.want {
				t.Errorf("filterMatches(%q, %q, exact=%v) = %v, want %v", tt.name, tt.filter, tt.exact, got, tt.want)
			}
		})
	}
}

// TestMatchesFilter verifies the exported wrapper lowercases both arguments and
// delegates to filterMatches, so a caller may pass names/filters in any case
// (the cache-delete path matches on-disk names and detected quant labels).
func TestMatchesFilter(t *testing.T) {
	tests := []struct {
		name   string
		filter string
		exact  bool
		want   bool
	}{
		// Case-insensitive on both sides.
		{"Qwen3-30B-A3B-Q4_K_M.gguf", "q4_k_m", false, true},
		{"qwen3-30b-a3b-q4_k_m.gguf", "Q4_K_M", false, true},
		{"Gemma-3-27B-UD-Q6_K_XL.gguf", "Q6_K", true, false},
		{"Gemma-3-27B-Q6_K.gguf", "Q6_K", true, true},
		// Split shards keep the quant as a whole segment.
		{"model-Q8_0-00001-of-00002.gguf", "Q8_0", true, true},
		// Unknown tokens do not match.
		{"model-Q4_K_M.gguf", "IQ3_XS", false, false},
		// mmproj companions must not match a quant token.
		{"mmproj-F16.gguf", "Q4_K_M", false, false},
		{"", "Q4_K_M", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name+"|"+tt.filter, func(t *testing.T) {
			if got := MatchesFilter(tt.name, tt.filter, tt.exact); got != tt.want {
				t.Errorf("MatchesFilter(%q, %q, exact=%v) = %v, want %v", tt.name, tt.filter, tt.exact, got, tt.want)
			}
		})
	}
}
