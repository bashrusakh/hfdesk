// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import "testing"

// The single-model key reduction that whole-folder deletion classifies with:
// one key per weight file's folder-relative path, non-weights ignored,
// unclassifiable weight names failing closed, and — the F3 invariant — a
// strictly right-anchored reduction under which two distinct names never
// share a key unless the files plausibly belong to one model.
func TestWeightFileModelKey(t *testing.T) {
	cases := []struct {
		name       string
		wantKey    string
		wantWeight bool
		wantOK     bool
	}{
		// One model, several shapes → one key.
		{"model-Q4_K_M.gguf", "model", true, true},
		{"model-Q4_K_M_00002-of-00002.gguf", "model", true, true},
		{"model-00002-of-00002.safetensors", "model", true, true},
		{"model.mmproj-f16.gguf", "model", true, true},
		{"model.fp16.safetensors", "model", true, true},
		{"model-BF16.gguf", "model", true, true},
		{"modelA-v2.safetensors", "modelA-v2", true, true},
		{"pytorch_model-00001-of-00002.bin", "pytorch_model", true, true},
		// Diffusers component directories drop from the key (derived from
		// requiredComponents, not a second list).
		{"unet/diffusion_pytorch_model.safetensors", "diffusion_pytorch_model", true, true},
		{"vae/diffusion_pytorch_model.safetensors", "diffusion_pytorch_model", true, true},
		// Any other parent directory stays in the key.
		{"a/pytorch_lora_weights.safetensors", "a/pytorch_lora_weights", true, true},
		{"b/pytorch_lora_weights.safetensors", "b/pytorch_lora_weights", true, true},
		// F3: an interior span never cuts — the full name is the key.
		{"tiny-Q4_K_M-llama.gguf", "tiny-Q4_K_M-llama", true, true},
		{"x-mmproj-y.gguf", "x-mmproj-y", true, true},
		{"model.fp16.ema.safetensors", "model.fp16.ema", true, true},
		// Non-weights carry no identity.
		{"config.json", "", false, false},
		{"notes.txt", "", false, false},
		{"nested/deep/extra.txt", "", false, false},
		// Weight-shaped names that cannot be reduced fail closed.
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

	// The F3 invariant pins: distinct names must never share a key unless the
	// files plausibly belong to one model. Both reviewer counterexamples and
	// the adapter pile must stay distinct; the legitimate same-model pairs
	// must stay equal.
	distinct := [][2]string{
		{"tiny-Q4_K_M-llama.gguf", "tinyllama.gguf"},                                 // interior quant excision merged these
		{"x-mmproj-y.gguf", "x.gguf"},                                                // any-"mmproj" cut merged these
		{"a/pytorch_lora_weights.safetensors", "b/pytorch_lora_weights.safetensors"}, // nested adapters
		{"modelA.safetensors", "modelA-v2.safetensors"},
	}
	for _, pair := range distinct {
		ka, _, _ := WeightFileModelKey(pair[0])
		kb, _, _ := WeightFileModelKey(pair[1])
		if ka == kb {
			t.Errorf("distinct names %q and %q must not share a key, both %q", pair[0], pair[1], ka)
		}
	}
	same := [][2]string{
		{"model-Q4_K_M.gguf", "model-Q5_K_S.gguf"},
		{"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"},
		{"model.gguf", "model.mmproj-f16.gguf"},
		{"unet/diffusion_pytorch_model.safetensors", "vae/diffusion_pytorch_model.safetensors"},
	}
	for _, pair := range same {
		ka, _, _ := WeightFileModelKey(pair[0])
		kb, _, _ := WeightFileModelKey(pair[1])
		if ka != kb || ka == "" {
			t.Errorf("one-model pair %q and %q must share a non-empty key, got %q and %q", pair[0], pair[1], ka, kb)
		}
	}
}
