// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import "testing"

func TestIsGGUFFilterDownload(t *testing.T) {
	ggufRepo := []string{
		"readme.md",
		".gitattributes",
		"gemma4-opus48-q8_0.gguf",
		"fp16/config.json",
		"fp16/tokenizer.json",
		"fp16/tokenizer_config.json",
	}
	multimodalRepo := []string{
		"model-q4_k_m.gguf",
		"mmproj-f16.gguf",
		"config.json",
	}
	transformersRepo := []string{
		"model.safetensors",
		"config.json",
		"tokenizer.json",
	}

	cases := []struct {
		name    string
		files   []string
		filters []string
		exact   bool
		want    bool
	}{
		{"gguf quant selected", ggufRepo, []string{"q8_0"}, false, true},
		{"gguf quant uppercase filter", ggufRepo, []string{"Q8_0"}, false, true},
		{"no filters", ggufRepo, nil, false, false},
		{"filter matches no gguf in repo", ggufRepo, []string{"q4_k_m"}, false, false},
		{"mmproj filter targets a gguf", multimodalRepo, []string{"q4_k_m", "mmproj"}, false, true},
		{"safetensors filter is not gguf", transformersRepo, []string{"safetensors"}, false, false},
		{"non-gguf filter in gguf repo", ggufRepo, []string{"config.json"}, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGGUFFilterDownload(tc.files, tc.filters, tc.exact); got != tc.want {
				t.Errorf("isGGUFFilterDownload(%v, %v) = %v, want %v", tc.files, tc.filters, got, tc.want)
			}
		})
	}
}
