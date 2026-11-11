// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"testing"
)

func TestAdapterTypeDescriptions(t *testing.T) {
	tests := []struct {
		adapterType string
		contains    string
	}{
		{"lora", "Low-Rank"},
		{"qlora", "Quantized"},
		{"ia3", "Inhibiting"},
		{"adalora", "Adaptive"},
		{"prefix", "Prefix"},
		{"vera", "Vector"},
	}

	for _, tt := range tests {
		t.Run(tt.adapterType, func(t *testing.T) {
			desc, ok := adapterTypeDescriptions[tt.adapterType]
			if !ok {
				t.Errorf("missing description for %q", tt.adapterType)
				return
			}
			if desc == "" {
				t.Errorf("empty description for %q", tt.adapterType)
			}
		})
	}
}

func TestAnalyzeLoRA(t *testing.T) {
	t.Run("standard LoRA config", func(t *testing.T) {
		metadata := map[string]interface{}{
			"adapter_config.json": map[string]interface{}{
				"peft_type":               "lora",
				"base_model_name_or_path": "meta-llama/Llama-2-7b-hf",
				"r":                       float64(16),
				"lora_alpha":              float64(32),
				"lora_dropout":            float64(0.05),
				"target_modules":          []interface{}{"q_proj", "v_proj", "k_proj", "o_proj"},
				"bias":                    "none",
				"task_type":               "CAUSAL_LM",
			},
		}

		info := analyzeLoRA(metadata)
		if info == nil {
			t.Fatal("expected non-nil result")
		}

		if info.AdapterType != "lora" {
			t.Errorf("AdapterType = %q", info.AdapterType)
		}
		if info.BaseModel != "meta-llama/Llama-2-7b-hf" {
			t.Errorf("BaseModel = %q", info.BaseModel)
		}
		if info.Rank != 16 {
			t.Errorf("Rank = %d", info.Rank)
		}
		if info.Alpha != 32 {
			t.Errorf("Alpha = %f", info.Alpha)
		}
		if info.Dropout != 0.05 {
			t.Errorf("Dropout = %f", info.Dropout)
		}
		if len(info.TargetModules) != 4 {
			t.Errorf("TargetModules count = %d", len(info.TargetModules))
		}
		if info.Bias != "none" {
			t.Errorf("Bias = %q", info.Bias)
		}
		if info.TaskType != "CAUSAL_LM" {
			t.Errorf("TaskType = %q", info.TaskType)
		}
	})

	t.Run("QLoRA config", func(t *testing.T) {
		metadata := map[string]interface{}{
			"adapter_config.json": map[string]interface{}{
				"peft_type":  "qlora",
				"quant_type": "nf4",
				"r":          float64(64),
			},
		}

		info := analyzeLoRA(metadata)
		if info == nil {
			t.Fatal("expected non-nil result")
		}

		if info.AdapterType != "qlora" {
			t.Errorf("AdapterType = %q", info.AdapterType)
		}
		if info.QuantType != "nf4" {
			t.Errorf("QuantType = %q", info.QuantType)
		}
	})

	t.Run("target_modules as map", func(t *testing.T) {
		metadata := map[string]interface{}{
			"adapter_config.json": map[string]interface{}{
				"peft_type": "lora",
				"target_modules": map[string]interface{}{
					"q_proj": true,
					"v_proj": true,
				},
			},
		}

		info := analyzeLoRA(metadata)
		if info == nil {
			t.Fatal("expected non-nil result")
		}

		if len(info.TargetModules) != 2 {
			t.Errorf("TargetModules count = %d, want 2", len(info.TargetModules))
		}
	})

	t.Run("missing adapter_config.json", func(t *testing.T) {
		metadata := map[string]interface{}{}
		info := analyzeLoRA(metadata)
		if info != nil {
			t.Error("expected nil for missing config")
		}
	})

	t.Run("boolean fields", func(t *testing.T) {
		metadata := map[string]interface{}{
			"adapter_config.json": map[string]interface{}{
				"peft_type":         "lora",
				"fan_in_fan_out":    true,
				"init_lora_weights": true,
			},
		}

		info := analyzeLoRA(metadata)
		if !info.FanInFanOut {
			t.Error("FanInFanOut should be true")
		}
		if !info.InitLoraWeights {
			t.Error("InitLoraWeights should be true")
		}
	})

	t.Run("description for known type", func(t *testing.T) {
		metadata := map[string]interface{}{
			"adapter_config.json": map[string]interface{}{
				"peft_type": "lora",
			},
		}

		info := analyzeLoRA(metadata)
		if info.AdapterDescription == "" {
			t.Error("AdapterDescription should not be empty")
		}
	})

	t.Run("description for unknown type", func(t *testing.T) {
		metadata := map[string]interface{}{
			"adapter_config.json": map[string]interface{}{
				"peft_type": "unknown_type",
			},
		}

		info := analyzeLoRA(metadata)
		if info.AdapterDescription != "PEFT adapter" {
			t.Errorf("AdapterDescription = %q, want 'PEFT adapter'", info.AdapterDescription)
		}
	})
}

func TestLoRAToRelatedDownloads(t *testing.T) {
	t.Run("nil info", func(t *testing.T) {
		result := LoRAToRelatedDownloads(nil)
		if result != nil {
			t.Error("expected nil for nil input")
		}
	})

	t.Run("no base model", func(t *testing.T) {
		info := &LoRAInfo{AdapterType: "lora"}
		result := LoRAToRelatedDownloads(info)
		if result != nil {
			t.Error("expected nil when no base model")
		}
	})

	t.Run("with base model", func(t *testing.T) {
		info := &LoRAInfo{
			AdapterType: "lora",
			BaseModel:   "meta-llama/Llama-2-7b-hf",
		}

		result := LoRAToRelatedDownloads(info)
		if len(result) != 1 {
			t.Fatalf("expected 1 related download, got %d", len(result))
		}

		related := result[0]
		if related.Type != "base_model" {
			t.Errorf("Type = %q", related.Type)
		}
		if related.Repo != "meta-llama/Llama-2-7b-hf" {
			t.Errorf("Repo = %q", related.Repo)
		}
		if !related.Required {
			t.Error("should be required")
		}
	})
}
