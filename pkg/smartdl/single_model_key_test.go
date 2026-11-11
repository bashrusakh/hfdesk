// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import "testing"

// The single-model key reduction that whole-folder deletion classifies with:
// one base key per weight file, non-weights ignored, unclassifiable weight
// names failing closed.
func TestWeightFileModelKey(t *testing.T) {
	cases := []struct {
		name       string
		wantKey    string
		wantWeight bool
		wantOK     bool
	}{
		{"model-Q4_K_M.gguf", "model", true, true},
		{"model-Q4_K_M_00002-of-00002.gguf", "model", true, true},
		{"model-00002-of-00002.safetensors", "model", true, true},
		{"model.mmproj-f16.gguf", "model", true, true},
		{"model.fp16.safetensors", "model", true, true},
		{"model-BF16.gguf", "model", true, true},
		{"modelA-v2.safetensors", "modelA-v2", true, true},
		{"diffusion_pytorch_model.safetensors", "diffusion_pytorch_model", true, true},
		{"pytorch_model-00001-of-00002.bin", "pytorch_model", true, true},
		{"config.json", "", false, false},
		{"notes.txt", "", false, false},
		{"Q4_K_M.gguf", "", true, false},
		{"fp16.safetensors", "", true, false},
		{".mmproj-f16.gguf", "", true, false},
	}
	for _, tc := range cases {
		key, weight, ok := WeightFileModelKey(tc.name)
		if key != tc.wantKey || weight != tc.wantWeight || ok != tc.wantOK {
			t.Errorf("WeightFileModelKey(%q) = (%q, %v, %v), want (%q, %v, %v)",
				tc.name, key, weight, ok, tc.wantKey, tc.wantWeight, tc.wantOK)
		}
	}
	// The conservative edge the classification depends on: a same-base pair
	// and its -v2 twin are two distinct keys, never one.
	a, _, _ := WeightFileModelKey("modelA.safetensors")
	b, _, _ := WeightFileModelKey("modelA-v2.safetensors")
	if a == b {
		t.Errorf("modelA and modelA-v2 must stay distinct keys, both %q", a)
	}
}
