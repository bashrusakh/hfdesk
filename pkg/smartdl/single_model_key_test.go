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
		{"model.fp16.safetensors", "model", true, true},
		{"model-BF16.gguf", "model", true, true},
		{"modelA-v2.safetensors", "modelA-v2", true, true},
		{"pytorch_model-00001-of-00002.bin", "pytorch_model", true, true},
		// Recognized mmproj companion spellings are non-weights: every
		// explicit shape contributes no model key.
		{"model.mmproj-f16.gguf", "", false, false},
		{"mmproj-F32.gguf", "", false, false},
		{"mmproj.gguf", "", false, false},
		{"x-mmproj-y.gguf", "", false, false},
		{"Ternary-Bonsai-2-27B-mmproj-BF16.gguf", "", false, false},
		{"mmproj-Qwen2.5-VL-7B-Instruct-f16.gguf", "", false, false},
		{"MMPROJ-F16.GGUF", "", false, false},
		{"nested/mmproj-F32.gguf", "", false, false},
		// The deletion rule is narrower than IsMMProjFile: an incidental
		// interior "mmproj" substring is an ordinary weight name.
		{"mmprojector-f16.gguf", "mmprojector", true, true},
		{"my-mmprojdata-model-Q4_K_M.gguf", "my-mmprojdata-model", true, true},
		// Diffusers component directories drop from the key (derived from
		// requiredComponents, not a second list).
		{"unet/diffusion_pytorch_model.safetensors", "diffusion_pytorch_model", true, true},
		{"vae/diffusion_pytorch_model.safetensors", "diffusion_pytorch_model", true, true},
		// Any other parent directory stays in the key.
		{"a/pytorch_lora_weights.safetensors", "a/pytorch_lora_weights", true, true},
		{"b/pytorch_lora_weights.safetensors", "b/pytorch_lora_weights", true, true},
		// A component-NAMED directory at a non-first position is NOT a
		// pipeline component — every directory component stays verbatim in the
		// general key, so these pairs must never merge.
		{"a/unet/x.safetensors", "a/unet/x", true, true},
		{"a/x.safetensors", "a/x", true, true},
		{"a/b/unet/x.safetensors", "a/b/unet/x", true, true},
		{"a/b/x.safetensors", "a/b/x", true, true},
		// An interior span never cuts — the full name is the key.
		{"tiny-Q4_K_M-llama.gguf", "tiny-Q4_K_M-llama", true, true},
		{"model.fp16.ema.safetensors", "model.fp16.ema", true, true},
		// Companion matching is ASCII-byte-aligned; Unicode case-mapping
		// length changes in the model part do not affect the delimiter.
		{"AȺȺ.m.gguf", "AȺȺ.m", true, true},
		{"AȺȺ.mmproj-zẞẞ.gguf", "", false, false},
		{"AȺȺȺ.mm.gguf", "AȺȺȺ.mm", true, true},
		{"AȺȺȺ.mmproj-zẞẞẞ.gguf", "", false, false},
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
		{"a/pytorch_lora_weights.safetensors", "b/pytorch_lora_weights.safetensors"}, // nested adapters
		{"modelA.safetensors", "modelA-v2.safetensors"},
		// F8 pairs: dropping a component-NAMED interior directory merged these.
		{"a/unet/x.safetensors", "a/x.safetensors"},
		{"a/b/unet/x.safetensors", "a/b/x.safetensors"},
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

// The folder-level pipeline-key rule: one real Diffusers tree is one model
// (a shared pipeline key), while two models in one role directory or
// pipeline weights mixed with top-level weights yield multiple keys; the
// per-folder outcomes are deterministic (sorted) and fail closed.
func TestSingleModelFolderKeys(t *testing.T) {
	one := []struct {
		name  string
		paths []string
	}{
		{"real pipeline tree", []string{
			"text_encoder/pytorch_model.bin", "unet/diffusion_pytorch_model.safetensors",
			"vae/diffusion_pytorch_model.safetensors", "tokenizer/merges.txt",
			"scheduler/scheduler_config.json",
		}},
		{"one base in two component dirs", []string{
			"unet/diffusion_pytorch_model.safetensors", "vae/diffusion_pytorch_model.safetensors",
		}},
		{"three quants of one base", []string{"model-Q4_K_M.gguf", "model-Q5_K_M.gguf", "model-Q6_K_S.gguf"}},
		{"canonical companion", []string{"model.gguf", "model.mmproj-f16.gguf"}},
		{"shard family with config", []string{"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors", "config.json"}},
	}
	for _, tc := range one {
		keys, unclassified := SingleModelFolderKeys(tc.paths)
		if unclassified || len(keys) != 1 {
			t.Errorf("%s: keys=%v unclassified=%v, want exactly one key", tc.name, keys, unclassified)
		}
	}
	// The pipeline tree's shared key is the pipeline sentinel itself.
	if keys, _ := SingleModelFolderKeys(one[0].paths); len(keys) != 1 || keys[0] != pipelineModelKey {
		t.Errorf("real pipeline tree keys=%v, want [%q]", keys, pipelineModelKey)
	}
	many := []struct {
		name  string
		paths []string
	}{
		// F8 folder: a component-named interior directory must not collapse
		// distinct folder-relative names.
		{"component-named dir at non-first position", []string{
			"a/unet/x.safetensors", "a/x.safetensors",
		}},
		{"component-named dir deeper", []string{
			"a/b/unet/x.safetensors", "a/b/x.safetensors",
		}},
		// F9 folder: sub-paths below a role directory are part of the role's
		// recorded basis, so an adapter pile inside unet/ refuses just as it
		// does at top level.
		{"adapter pile inside a role dir", []string{
			"unet/a/pytorch_lora_weights.safetensors", "unet/b/pytorch_lora_weights.safetensors",
		}},
		{"two models in one component dir", []string{"unet/modelA.safetensors", "unet/modelB.safetensors"}},
		{"pipeline mixed with a top-level GGUF", []string{
			"unet/diffusion_pytorch_model.safetensors", "vae/diffusion_pytorch_model.safetensors",
			"x-Q4_K_M.gguf",
		}},
		{"adapters in sibling dirs", []string{
			"a/pytorch_lora_weights.safetensors", "b/pytorch_lora_weights.safetensors",
		}},
		{"pile of five models", []string{
			"modelA-Q4_K_M.gguf", "modelB-Q5_K_M.gguf", "modelC-Q6_K.gguf",
			"modelD-Q8_0.gguf", "modelE-Q4_K_S.gguf",
		}},
	}
	for _, tc := range many {
		keys, unclassified := SingleModelFolderKeys(tc.paths)
		if unclassified || len(keys) < 2 {
			t.Errorf("%s: keys=%v unclassified=%v, want at least two keys", tc.name, keys, unclassified)
		}
	}
	// Sorted, deterministic output for the mixed case: pipeline + top-level.
	if keys, _ := SingleModelFolderKeys([]string{
		"unet/diffusion_pytorch_model.safetensors", "vae/diffusion_pytorch_model.safetensors",
		"x-Q4_K_M.gguf",
	}); len(keys) != 2 || keys[0] != pipelineModelKey || keys[1] != "x" {
		t.Errorf("mixed keys=%v, want [%q \"x\"]", keys, pipelineModelKey)
	}
	// Fail-closed outcomes.
	if keys, unclassified := SingleModelFolderKeys([]string{"config.json", "notes.txt"}); unclassified || len(keys) != 0 {
		t.Errorf("weightless folder: keys=%v unclassified=%v, want none", keys, unclassified)
	}
	if _, unclassified := SingleModelFolderKeys([]string{"unet/Q4_K_M.safetensors"}); !unclassified {
		t.Error("unclassifiable weight name must fail closed")
	}
}

// An mmproj companion is a non-weight in the whole-folder deletion
// classification: every surveyed explicit shape adds no key, so one model's
// weights plus any number of companions still classify as exactly one model.
// A companion-only folder yields zero keys (the server refuses it through the
// keyCount==0 rule), and two models that each carry companions keep two keys.
func TestSingleModelFolderKeys_MMProjCompanions(t *testing.T) {
	const model = "Omnicoder-9B-Opus-Distill.q8_0.gguf"
	const modelKey = "Omnicoder-9B-Opus-Distill"
	shapes := []struct {
		name       string
		companions []string
	}{
		{"leading mmproj-", []string{"mmproj-F32.gguf"}},
		{"dot-separated .mmproj-", []string{"Omnicoder-9B-Opus-Distill.mmproj-Q8_0.gguf"}},
		{"hyphenated -mmproj-", []string{"Ternary-Bonsai-2-27B-mmproj-BF16.gguf"}},
		{"bare mmproj.gguf", []string{"mmproj.gguf"}},
		{"model-name token after prefix", []string{
			"mmproj-Qwen2.5-VL-7B-Instruct-f16.gguf", "mmproj-model-f16.gguf",
		}},
		{"case-insensitive", []string{"MMPROJ-F16.GGUF"}},
		{"nested companion", []string{"sub/mmproj-F16.gguf"}},
		{"all companions together", []string{
			"mmproj-BF16.gguf", "mmproj-F16.gguf", "mmproj-F32.gguf",
		}},
	}
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			paths := append([]string{model}, tc.companions...)
			keys, unclassified := SingleModelFolderKeys(paths)
			if unclassified || len(keys) != 1 || keys[0] != modelKey {
				t.Fatalf("keys=%v unclassified=%v, want exactly [%q]", keys, unclassified, modelKey)
			}
			for _, companion := range tc.companions {
				if key, weight, ok := WeightFileModelKey(companion); key != "" || weight || ok {
					t.Errorf("companion %q = (%q, %v, %v), want no key and not a weight", companion, key, weight, ok)
				}
			}
		})
	}

	// A folder holding only companions has no model weight at all.
	companionsOnly := []string{"mmproj-BF16.gguf", "mmproj-F16.gguf", "mmproj-F32.gguf", "mmproj.gguf"}
	if keys, unclassified := SingleModelFolderKeys(companionsOnly); unclassified || len(keys) != 0 {
		t.Fatalf("companion-only folder: keys=%v unclassified=%v, want zero keys", keys, unclassified)
	}

	// Two models keep their own keys even when each carries companions.
	twoModels := []string{
		"modelA-Q4_K_M.gguf", "mmproj-F16.gguf",
		"modelB-Q5_K_M.gguf", "mmproj-BF16.gguf",
	}
	if keys, unclassified := SingleModelFolderKeys(twoModels); unclassified || len(keys) != 2 {
		t.Fatalf("two models with companions: keys=%v unclassified=%v, want two keys", keys, unclassified)
	}

	// An unrelated interior "mmproj" substring is not a companion under the
	// deletion rule; it keeps its own key so a second model cannot hide.
	ambiguous := []string{"mmprojector-f16.gguf", "my-mmprojdata-model-Q4_K_M.gguf"}
	if keys, unclassified := SingleModelFolderKeys(ambiguous); unclassified || len(keys) != 2 {
		t.Fatalf("interior mmproj substrings: keys=%v unclassified=%v, want two keys", keys, unclassified)
	}

	// One base's quants still merge when companions are present.
	quants := []string{
		"model-Q4_K_M.gguf", "model-Q5_K_M.gguf", "mmproj-F32.gguf", "mmproj-F16.gguf",
	}
	if keys, unclassified := SingleModelFolderKeys(quants); unclassified || len(keys) != 1 || keys[0] != "model" {
		t.Fatalf("quants plus companions: keys=%v unclassified=%v, want exactly [\"model\"]", keys, unclassified)
	}
}
