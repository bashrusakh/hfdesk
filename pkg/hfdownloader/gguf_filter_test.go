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
	trainingNamedRepo := []string{
		"model-q4_k_m.gguf",
		"training-model.safetensors",
	}
	legacySplitRepo := []string{
		"model-Q4_K_M-00001-of-00002.gguf-split",
		"model-Q4_K_M-00002-of-00002.gguf-split",
		"config.json",
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
		{"training-named payload keeps mixed mode", trainingNamedRepo, []string{"q4_k_m", "training-model"}, false, false},
		{"legacy .gguf-split quant selected", legacySplitRepo, []string{"Q4_K_M"}, false, true},
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

func TestIsModelPayloadPathUsesSupportedWeightFormatsAndExclusions(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"weights/model.safetensors", true},
		{"weights/model.onnx", true},
		{"weights/pytorch_model.bin", true},
		{"weights/model.pt", true},
		{"weights/model.pth", true},
		{"weights/model.gguf-split", true},
		{"weights/flax_model.msgpack", true},
		{"weights/flax_model-00001-of-00002.msgpack", true},
		{"weights/diffusion_flax_model.msgpack", true},
		{"weights/tf_model.h5", true},
		{"weights/tf_model-00001-of-00002.h5", true},
		{"weights/model.tflite", true},
		{"weights/rust_model.ot", true},
		{"weights/custom.msgpack", false},
		{"weights/checkpoint.h5", false},
		{"weights/arbitrary.tflite", false},
		{"weights/model.ot", false},
		{"weights/pytorch_model.bin.index.json", false},
		{"weights/tokenizer.bin", false},
		{"weights/training-model.safetensors", true},
		{"weights/model-training.pt", true},
		{"weights/optimizer.pt", false},
		{"weights/adam_optimizer.pt", false},
		{"weights/training_optimizer.pt", false},
		{"weights/my_optimizer.pt", false},
		{"weights/optimizer-state.pt", false},
		{"weights/training-state.pth", false},
		{"weights/training_args.bin", false},
		{"weights/optimizer.bin", false},
		{"config.json", false},
		{"notes.unknown", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := isModelPayloadPath(tc.path); got != tc.want {
				t.Errorf("isModelPayloadPath(%q) = %t, want %t", tc.path, got, tc.want)
			}
		})
	}
}
