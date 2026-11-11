// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestPipelineDescriptions(t *testing.T) {
	expected := map[string]string{
		"StableDiffusionPipeline":        "Stable Diffusion v1.x text-to-image",
		"StableDiffusionXLPipeline":      "Stable Diffusion XL text-to-image",
		"FluxPipeline":                   "Flux text-to-image",
		"StableVideoDiffusionPipeline":   "Stable Video Diffusion",
		"LatentConsistencyModelPipeline": "LCM fast inference",
	}

	for pipeline, desc := range expected {
		t.Run(pipeline, func(t *testing.T) {
			got := pipelineDescriptions[pipeline]
			if got != desc {
				t.Errorf("pipelineDescriptions[%q] = %q, want %q", pipeline, got, desc)
			}
		})
	}
}

func TestRequiredComponents(t *testing.T) {
	t.Run("StableDiffusionPipeline", func(t *testing.T) {
		required := requiredComponents["StableDiffusionPipeline"]
		expected := []string{"unet", "vae", "text_encoder", "tokenizer", "scheduler"}

		if len(required) != len(expected) {
			t.Errorf("got %d components, want %d", len(required), len(expected))
		}

		for _, exp := range expected {
			found := false
			for _, r := range required {
				if r == exp {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("missing required component %q", exp)
			}
		}
	})

	t.Run("StableDiffusionXLPipeline", func(t *testing.T) {
		required := requiredComponents["StableDiffusionXLPipeline"]
		// SDXL has dual encoders
		if len(required) < 6 {
			t.Errorf("SDXL should have at least 6 components, got %d", len(required))
		}
	})

	t.Run("FluxPipeline", func(t *testing.T) {
		required := requiredComponents["FluxPipeline"]
		// Flux uses transformer instead of unet
		hasTransformer := false
		for _, r := range required {
			if r == "transformer" {
				hasTransformer = true
				break
			}
		}
		if !hasTransformer {
			t.Error("FluxPipeline should have 'transformer' component")
		}
	})
}

func TestAnalyzeDiffusers(t *testing.T) {
	t.Run("with model_index.json", func(t *testing.T) {
		files := []FileInfo{
			{Name: "model_index.json", Path: "model_index.json", Size: 500},
			{Name: "diffusion_pytorch_model.safetensors", Path: "unet/diffusion_pytorch_model.safetensors", Size: 3400000000},
			{Name: "diffusion_pytorch_model.safetensors", Path: "vae/diffusion_pytorch_model.safetensors", Size: 330000000},
			{Name: "model.safetensors", Path: "text_encoder/model.safetensors", Size: 500000000},
		}

		metadata := map[string]interface{}{
			"model_index.json": map[string]interface{}{
				"_class_name":        "StableDiffusionPipeline",
				"_diffusers_version": "0.25.0",
				"unet":               []interface{}{"diffusers", "UNet2DConditionModel"},
				"vae":                []interface{}{"diffusers", "AutoencoderKL"},
				"text_encoder":       []interface{}{"transformers", "CLIPTextModel"},
				"tokenizer":          []interface{}{"transformers", "CLIPTokenizer"},
				"scheduler":          []interface{}{"diffusers", "PNDMScheduler"},
			},
		}

		info := analyzeDiffusers(files, metadata)
		if info == nil {
			t.Fatal("expected non-nil result")
		}

		if info.PipelineType != "StableDiffusionPipeline" {
			t.Errorf("PipelineType = %q", info.PipelineType)
		}

		if info.DiffusersVersion != "0.25.0" {
			t.Errorf("DiffusersVersion = %q", info.DiffusersVersion)
		}

		if len(info.Components) < 4 {
			t.Errorf("expected at least 4 components, got %d", len(info.Components))
		}

		// Check unet component
		var unet *DiffusersComponent
		for i := range info.Components {
			if info.Components[i].Name == "unet" {
				unet = &info.Components[i]
				break
			}
		}
		if unet == nil {
			t.Fatal("unet component not found")
		}
		if unet.Library != "diffusers" {
			t.Errorf("unet.Library = %q", unet.Library)
		}
		if unet.ClassName != "UNet2DConditionModel" {
			t.Errorf("unet.ClassName = %q", unet.ClassName)
		}
		if !unet.Required {
			t.Error("unet should be required")
		}
	})

	t.Run("empty metadata", func(t *testing.T) {
		files := []FileInfo{}
		metadata := map[string]interface{}{}

		info := analyzeDiffusers(files, metadata)
		if info == nil {
			t.Fatal("expected non-nil result")
		}
		if info.PipelineType != "" {
			t.Errorf("PipelineType should be empty, got %q", info.PipelineType)
		}
	})

	t.Run("unknown pipeline type", func(t *testing.T) {
		metadata := map[string]interface{}{
			"model_index.json": map[string]interface{}{
				"_class_name": "CustomPipeline",
			},
		}

		info := analyzeDiffusers(nil, metadata)
		if info.PipelineType != "CustomPipeline" {
			t.Errorf("PipelineType = %q", info.PipelineType)
		}
		if info.PipelineDescription != "Diffusers pipeline" {
			t.Errorf("PipelineDescription = %q", info.PipelineDescription)
		}
	})
}

func TestCalculateComponentSize(t *testing.T) {
	files := []FileInfo{
		{Path: "unet/diffusion_pytorch_model.safetensors", Size: 3400000000},
		{Path: "unet/config.json", Size: 1000},
		{Path: "vae/diffusion_pytorch_model.safetensors", Size: 330000000},
		{Path: "text_encoder/model.safetensors", Size: 500000000},
		{Path: "config.json", Size: 500}, // Root file, not in any component
	}

	tests := []struct {
		component string
		expected  int64
	}{
		{"unet", 3400001000},
		{"vae", 330000000},
		{"text_encoder", 500000000},
		{"nonexistent", 0},
	}

	for _, tt := range tests {
		t.Run(tt.component, func(t *testing.T) {
			size := calculateComponentSize(files, tt.component)
			if size != tt.expected {
				t.Errorf("calculateComponentSize(%q) = %d, want %d", tt.component, size, tt.expected)
			}
		})
	}
}

func TestDetectVariants(t *testing.T) {
	tests := []struct {
		name     string
		files    []FileInfo
		expected []string
	}{
		{
			name: "fp16 in filename",
			files: []FileInfo{
				{Name: "model.fp16.safetensors", Path: "unet/model.fp16.safetensors"},
			},
			expected: []string{"fp16"},
		},
		{
			name: "fp16 in directory",
			files: []FileInfo{
				{Name: "model.safetensors", Path: "unet/fp16/model.safetensors", Directory: "unet/fp16"},
			},
			expected: []string{"fp16"},
		},
		{
			name: "multiple variants",
			files: []FileInfo{
				{Name: "model.fp16.safetensors", Path: "model.fp16.safetensors"},
				{Name: "model.fp32.safetensors", Path: "model.fp32.safetensors"},
				{Name: "model.bf16.safetensors", Path: "model.bf16.safetensors"},
			},
			expected: []string{"fp16", "fp32", "bf16"},
		},
		{
			name:     "no variants",
			files:    []FileInfo{{Name: "model.safetensors", Path: "model.safetensors"}},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := detectVariants(tt.files)
			if len(tt.expected) == 0 && len(result) == 0 {
				return // Both empty, OK
			}

			for _, exp := range tt.expected {
				found := false
				for _, r := range result {
					if r == exp {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("missing variant %q in result %v", exp, result)
				}
			}
		})
	}
}

func TestDetectPrecisions(t *testing.T) {
	tests := []struct {
		name    string
		files   []FileInfo
		hasFP32 bool
	}{
		{
			name: "standard safetensors",
			files: []FileInfo{
				{Name: "model.safetensors"},
			},
			hasFP32: true,
		},
		{
			name: "fp16 safetensors",
			files: []FileInfo{
				{Name: "model_fp16.safetensors"},
			},
			hasFP32: false, // Has fp16 indicator
		},
		{
			name: "pytorch bin",
			files: []FileInfo{
				{Name: "pytorch_model.bin"},
			},
			hasFP32: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := detectPrecisions(tt.files)

			hasFP32 := false
			for _, p := range result {
				if p == "fp32" {
					hasFP32 = true
					break
				}
			}

			if hasFP32 != tt.hasFP32 {
				t.Errorf("hasFP32 = %v, want %v (result: %v)", hasFP32, tt.hasFP32, result)
			}
		})
	}
}

func TestGetComponentFiles(t *testing.T) {
	files := []FileInfo{
		{Path: "unet/diffusion_pytorch_model.safetensors", Name: "diffusion_pytorch_model.safetensors"},
		{Path: "unet/config.json", Name: "config.json"},
		{Path: "vae/diffusion_pytorch_model.safetensors", Name: "diffusion_pytorch_model.safetensors"},
		{Path: "config.json", Name: "config.json"},
	}

	t.Run("unet component", func(t *testing.T) {
		result := GetComponentFiles(files, "unet")
		if len(result) != 2 {
			t.Errorf("expected 2 files, got %d", len(result))
		}
	})

	t.Run("vae component", func(t *testing.T) {
		result := GetComponentFiles(files, "vae")
		if len(result) != 1 {
			t.Errorf("expected 1 file, got %d", len(result))
		}
	})

	t.Run("nonexistent component", func(t *testing.T) {
		result := GetComponentFiles(files, "nonexistent")
		if len(result) != 0 {
			t.Errorf("expected 0 files, got %d", len(result))
		}
	})
}

func TestDiffusersToSelectableItems(t *testing.T) {
	t.Run("nil info", func(t *testing.T) {
		items := DiffusersToSelectableItems(nil)
		if items != nil {
			t.Error("expected nil for nil input")
		}
	})

	t.Run("with variants and components", func(t *testing.T) {
		info := &DiffusersInfo{
			PipelineType: "StableDiffusionPipeline",
			Variants:     []string{"fp16", "fp32"},
			Components: []DiffusersComponent{
				{Name: "unet", ClassName: "UNet2DConditionModel", Size: 3400000000, SizeHuman: "3.2 GiB", Required: true},
				{Name: "vae", ClassName: "AutoencoderKL", Size: 330000000, SizeHuman: "315 MiB", Required: true},
			},
		}

		items := DiffusersToSelectableItems(info)
		if len(items) != 4 { // 2 variants + 2 components
			t.Errorf("expected 4 items, got %d", len(items))
		}

		// Check that fp16 is recommended
		var fp16Item *SelectableItem
		for i := range items {
			if items[i].ID == "fp16" {
				fp16Item = &items[i]
				break
			}
		}
		if fp16Item == nil {
			t.Fatal("fp16 item not found")
		}
		if !fp16Item.Recommended {
			t.Error("fp16 should be recommended")
		}
		if fp16Item.Category != "variant" {
			t.Errorf("fp16 category = %q, want 'variant'", fp16Item.Category)
		}

		// Check that required components are marked as recommended
		var unetItem *SelectableItem
		for i := range items {
			if items[i].ID == "unet" {
				unetItem = &items[i]
				break
			}
		}
		if unetItem == nil {
			t.Fatal("unet item not found")
		}
		if !unetItem.Recommended {
			t.Error("unet should be recommended (required)")
		}
		if unetItem.Category != "component" {
			t.Errorf("unet category = %q, want 'component'", unetItem.Category)
		}
	})
}

func TestCalculateDownloadSize(t *testing.T) {
	info := &DiffusersInfo{
		Components: []DiffusersComponent{
			{Name: "unet"},
			{Name: "vae"},
		},
	}

	files := []FileInfo{
		{Path: "unet/model.safetensors", Name: "model.safetensors", Size: 3000000000},
		{Path: "unet/model.fp16.safetensors", Name: "model.fp16.safetensors", Size: 1500000000},
		{Path: "vae/model.safetensors", Name: "model.safetensors", Size: 300000000},
		{Path: "text_encoder/model.safetensors", Name: "model.safetensors", Size: 500000000},
	}

	t.Run("all components no variant", func(t *testing.T) {
		size := CalculateDownloadSize(info, files, []string{"unet", "vae"}, "")
		// Should include unet + vae files
		if size < 3000000000 {
			t.Errorf("size = %d, expected > 3GB", size)
		}
	})

	t.Run("single component", func(t *testing.T) {
		size := CalculateDownloadSize(info, files, []string{"vae"}, "")
		if size != 300000000 {
			t.Errorf("size = %d, expected 300000000", size)
		}
	})
}

func wfile(path string, size int64) FileInfo {
	return FileInfo{
		Path: path,
		Name: path[strings.LastIndex(path, "/")+1:],
		Size: size,
	}
}

func setPaths(set componentWeightSet) []string {
	paths := make([]string, 0, len(set.files))
	for _, f := range set.files {
		paths = append(paths, f.Path)
	}
	return paths
}

// TestBestWeightSetPrecedence pins the approved best-set rule: fp16
// safetensors > default safetensors > .bin, the stated within-class
// tie-breaks (plain naming beats a precision-marked twin, then the
// lexicographically smallest member set), and the no-weights outcome.
func TestBestWeightSetPrecedence(t *testing.T) {
	tests := []struct {
		name  string
		files []FileInfo
		want  []string
	}{
		{"fp16 safetensors beat default safetensors and bin", []FileInfo{
			wfile("unet/model.safetensors", 300),
			wfile("unet/model.fp16.safetensors", 150),
			wfile("unet/model.bin", 300),
		}, []string{"unet/model.fp16.safetensors"}},
		{"default safetensors beat bin", []FileInfo{
			wfile("unet/model.safetensors", 300),
			wfile("unet/model.bin", 300),
		}, []string{"unet/model.safetensors"}},
		{"bin qualifies when nothing else exists", []FileInfo{
			wfile("unet/config.json", 10),
			wfile("unet/model.bin", 300),
		}, []string{"unet/model.bin"}},
		{"plain default naming beats a bf16 twin in the safetensors class", []FileInfo{
			wfile("unet/model.safetensors", 300),
			wfile("unet/model.bf16.safetensors", 150),
		}, []string{"unet/model.safetensors"}},
		{"the lexicographically smallest set wins a flat tie", []FileInfo{
			wfile("unet/b.safetensors", 200),
			wfile("unet/a.safetensors", 100),
		}, []string{"unet/a.safetensors"}},
		{"an fp16 variant folder outranks the default set", []FileInfo{
			wfile("unet/model.safetensors", 300),
			wfile("unet/fp16/model.safetensors", 150),
		}, []string{"unet/fp16/model.safetensors"}},
		{"a weightless folder has no set", []FileInfo{
			wfile("unet/config.json", 10),
			wfile("tokenizer/tokenizer.json", 20),
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, ok := bestWeightSet(tt.files, "unet")
			if len(tt.want) == 0 {
				if ok {
					t.Fatalf("set = %v, want no weight set for a weightless component", setPaths(set))
				}
				return
			}
			if !ok {
				t.Fatal("ok = false, want a chosen weight set")
			}
			if got := setPaths(set); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("chosen set = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBestWeightSetShardFamily pins the shard rule: one
// <prefix>-NNNNN-of-MMMMM family joins whole (all three shards, never two of
// three) and only when it is a complete, well-indexed family; class
// precedence is absolute among eligible sets, and a component with no
// eligible set falls through to its legacy weightless row.
func TestBestWeightSetShardFamily(t *testing.T) {
	t.Run("a complete fp16 family joins whole and outranks the default set", func(t *testing.T) {
		set, ok := bestWeightSet([]FileInfo{
			wfile("unet/diffusion_pytorch_model.fp16-00001-of-00003.safetensors", 100),
			wfile("unet/diffusion_pytorch_model.fp16-00002-of-00003.safetensors", 110),
			wfile("unet/diffusion_pytorch_model.fp16-00003-of-00003.safetensors", 120),
			wfile("unet/diffusion_pytorch_model.safetensors", 500),
			wfile("unet/diffusion_pytorch_model.bin", 500),
		}, "unet")
		if !ok {
			t.Fatal("ok = false, want the fp16 family chosen")
		}
		want := []string{
			"unet/diffusion_pytorch_model.fp16-00001-of-00003.safetensors",
			"unet/diffusion_pytorch_model.fp16-00002-of-00003.safetensors",
			"unet/diffusion_pytorch_model.fp16-00003-of-00003.safetensors",
		}
		if got := setPaths(set); !reflect.DeepEqual(got, want) {
			t.Fatalf("chosen set = %v, want all three fp16 shards %v, never two of three", got, want)
		}
		if set.size != 330 || set.marker != "fp16" {
			t.Fatalf("set = size %d marker %q, want 330/fp16", set.size, set.marker)
		}
	})

	t.Run("an incomplete fp16 family yields to the complete default set", func(t *testing.T) {
		// The fp16 family lacks its third shard, so it is not a complete,
		// well-indexed family and never joins selection: class precedence
		// rules only among eligible sets, and the complete default set wins.
		set, ok := bestWeightSet([]FileInfo{
			wfile("unet/model.fp16-00001-of-00003.safetensors", 100),
			wfile("unet/model.fp16-00002-of-00003.safetensors", 110),
			wfile("unet/other.safetensors", 500),
		}, "unet")
		if !ok {
			t.Fatal("ok = false, want the complete default set chosen")
		}
		want := []string{"unet/other.safetensors"}
		if got := setPaths(set); !reflect.DeepEqual(got, want) {
			t.Fatalf("chosen set = %v, want the complete default set %v: the half fp16 family must never be selectable", got, want)
		}
	})

	t.Run("a complete default set beats the incomplete family beside it", func(t *testing.T) {
		set, ok := bestWeightSet([]FileInfo{
			wfile("unet/model-00001-of-00003.safetensors", 100),
			wfile("unet/model-00002-of-00003.safetensors", 110),
			wfile("unet/other.safetensors", 500),
		}, "unet")
		if !ok {
			t.Fatal("ok = false, want a chosen weight set")
		}
		if got := setPaths(set); !reflect.DeepEqual(got, []string{"unet/other.safetensors"}) {
			t.Fatalf("chosen set = %v, want the complete default set, not the incomplete family", got)
		}
	})
}

// TestBestWeightSetFamilyEligibility pins the eligibility gate: a shard
// family joins selection only when its present indices are exactly the
// distinct set 1..M with the declared M equal to the member count. Every
// ineligible shape — a split-spelling family, a declared-total mismatch in
// either direction, an index hole — leaves no eligible set, so the component
// falls through to its legacy weightless row.
func TestBestWeightSetFamilyEligibility(t *testing.T) {
	// itemRow runs the analyze → selectable-row path for a unet folder
	// holding exactly the given files plus a model_index.json naming unet.
	itemRow := func(t *testing.T, files []FileInfo) *SelectableItem {
		t.Helper()
		all := append([]FileInfo{wfile("model_index.json", 500)}, files...)
		metadata := map[string]interface{}{
			"model_index.json": map[string]interface{}{
				"_class_name": "StableDiffusionPipeline",
				"unet":        []interface{}{"diffusers", "UNet2DConditionModel"},
			},
		}
		items := DiffusersToSelectableItems(analyzeDiffusers(all, metadata))
		for i := range items {
			if items[i].ID == "unet" {
				return &items[i]
			}
		}
		t.Fatal("unet row not found")
		return nil
	}

	t.Run("a split-spelling family leaves the component weightless", func(t *testing.T) {
		files := []FileInfo{
			wfile("unet/config.json", 40),
			wfile("unet/m.fp16-00001-of-00002.safetensors", 100),
			wfile("unet/m-00002-of-00002.safetensors", 110),
		}
		if set, ok := bestWeightSet(files, "unet"); ok {
			t.Fatalf("set = %v, want no eligible set: one family spelled two ways is two broken halves", setPaths(set))
		}
		row := itemRow(t, files)
		if len(row.Files) != 0 {
			t.Fatalf("row files = %v, want none: the component keeps its weightless folder row", row.Files)
		}
		if row.Label != "unet" || row.FilterValue != "unet" || row.Size != 250 || row.SizeHuman != humanSize(250) {
			t.Fatalf("row = %+v, want the untouched legacy folder row (label/filter \"unet\", whole-folder 250 bytes)", row)
		}
	})

	t.Run("two shards of a declared three are ineligible", func(t *testing.T) {
		if set, ok := bestWeightSet([]FileInfo{
			wfile("unet/model-00001-of-00003.safetensors", 100),
			wfile("unet/model-00002-of-00003.safetensors", 110),
		}, "unet"); ok {
			t.Fatalf("set = %v, want no eligible set: present indices {1,2} are not exactly 1..3", setPaths(set))
		}
	})

	t.Run("three shards of a declared two are ineligible", func(t *testing.T) {
		if set, ok := bestWeightSet([]FileInfo{
			wfile("unet/model-00001-of-00002.safetensors", 100),
			wfile("unet/model-00002-of-00002.safetensors", 110),
			wfile("unet/model-00003-of-00002.safetensors", 120),
		}, "unet"); ok {
			t.Fatalf("set = %v, want no eligible set: the out-of-range third member breaks the declared total", setPaths(set))
		}
	})

	t.Run("an index hole makes the family ineligible", func(t *testing.T) {
		if set, ok := bestWeightSet([]FileInfo{
			wfile("unet/model-00002-of-00002.safetensors", 110),
			wfile("unet/model-00003-of-00002.safetensors", 120),
		}, "unet"); ok {
			t.Fatalf("set = %v, want no eligible set: present indices {2,3} are not exactly 1..2", setPaths(set))
		}
	})
}

// TestBestWeightSetExcludesConversions pins that ONNX, OpenVINO (the .xml +
// same-basename .bin pair, and conversion subfolders), Flax/msgpack and
// tflite copies join nothing whatever their weight format — a conversion
// folder excludes .safetensors as surely as .bin — while an unpaired pytorch
// .bin still qualifies.
func TestBestWeightSetExcludesConversions(t *testing.T) {
	files := []FileInfo{
		wfile("unet/model.safetensors", 500),
		wfile("unet/model.onnx", 900),
		wfile("unet/model.msgpack", 800),
		wfile("unet/model.tflite", 700),
		wfile("unet/model.xml", 60),
		wfile("unet/model.bin", 600),
		wfile("unet/openvino/weights.bin", 650),
		wfile("unet/pytorch_model.bin", 550),
		wfile("unet/onnx/weights.bin", 640),
	}
	set, ok := bestWeightSet(files, "unet")
	if !ok {
		t.Fatal("ok = false, want the default safetensors chosen")
	}
	if got := setPaths(set); !reflect.DeepEqual(got, []string{"unet/model.safetensors"}) {
		t.Fatalf("chosen set = %v, want exactly the safetensors: every conversion copy must stay an alternative", got)
	}

	t.Run("bin fallback skips conversion members only", func(t *testing.T) {
		set, ok := bestWeightSet([]FileInfo{
			wfile("unet/model.onnx", 900),
			wfile("unet/model.xml", 60),
			wfile("unet/model.bin", 600),
			wfile("unet/pytorch_model.bin", 550),
		}, "unet")
		if !ok {
			t.Fatal("ok = false, want the unpaired pytorch bin chosen")
		}
		if got := setPaths(set); !reflect.DeepEqual(got, []string{"unet/pytorch_model.bin"}) {
			t.Fatalf("chosen set = %v, want only the unpaired pytorch bin", got)
		}
	})

	t.Run("a conversions-only component has no weight set", func(t *testing.T) {
		if set, ok := bestWeightSet([]FileInfo{
			wfile("unet/model.onnx", 900),
			wfile("unet/model.xml", 60),
			wfile("unet/model.bin", 600),
		}, "unet"); ok {
			t.Fatalf("set = %v, want none: conversions never form a set", setPaths(set))
		}
	})

	t.Run("a safetensors under a conversion dir joins nothing", func(t *testing.T) {
		if set, ok := bestWeightSet([]FileInfo{
			wfile("unet/config.json", 40),
			wfile("unet/onnx/model.safetensors", 700),
		}, "unet"); ok {
			t.Fatalf("set = %v, want none: a conversion folder's weights never join, whatever the format", setPaths(set))
		}
	})
}

// TestDiffusersBestSetComponentItem pins the row contract end to end: the
// weighted item carries exactly its chosen set (label, size), the weightless
// item keeps its current row, and the variant category row stays unchanged.
func TestDiffusersBestSetComponentItem(t *testing.T) {
	files := []FileInfo{
		wfile("unet/config.json", 1000),
		wfile("unet/diffusion_pytorch_model.safetensors", 300),
		wfile("unet/diffusion_pytorch_model.fp16.safetensors", 150),
		wfile("unet/model.onnx", 900),
		wfile("tokenizer/tokenizer.json", 500),
	}
	metadata := map[string]interface{}{
		"model_index.json": map[string]interface{}{
			"_class_name": "StableDiffusionPipeline",
			"unet":        []interface{}{"diffusers", "UNet2DConditionModel"},
			"tokenizer":   []interface{}{"transformers", "CLIPTokenizer"},
		},
	}
	items := DiffusersToSelectableItems(analyzeDiffusers(files, metadata))

	var unet, tokenizer, fp16 *SelectableItem
	for i := range items {
		switch items[i].ID {
		case "unet":
			unet = &items[i]
		case "tokenizer":
			tokenizer = &items[i]
		case "fp16":
			fp16 = &items[i]
		}
	}
	if unet == nil || tokenizer == nil {
		t.Fatalf("items = %+v, want unet and tokenizer rows", items)
	}
	if want := []string{"unet/diffusion_pytorch_model.fp16.safetensors"}; !reflect.DeepEqual(unet.Files, want) {
		t.Fatalf("unet files = %v, want exactly the fp16 set %v: default set, bin and ONNX stay alternatives", unet.Files, want)
	}
	if unet.Size != 150 || unet.SizeHuman != humanSize(150) {
		t.Fatalf("unet size = %d/%q, want the chosen set's 150 bytes only", unet.Size, unet.SizeHuman)
	}
	if unet.Label != "Unet (fp16)" || unet.FilterValue != "unet" || unet.Category != "component" || !unet.Recommended {
		t.Fatalf("unet row = %+v, want label \"Unet (fp16)\" with unchanged identity, category and required badge", unet)
	}
	if len(tokenizer.Files) != 0 || tokenizer.Label != "tokenizer" || tokenizer.Size != 500 || tokenizer.FilterValue != "tokenizer" {
		t.Fatalf("tokenizer row = %+v, want the unchanged weightless folder row of 500 bytes", tokenizer)
	}
	if fp16 == nil || fp16.Category != "variant" || len(fp16.Files) != 0 || fp16.FilterValue != "fp16" {
		t.Fatalf("fp16 variant row = %+v, want the unchanged thin variant row with no files", fp16)
	}
}

// TestRecommendedDownloadCarriesComponentFiles pins the recommendation
// mechanism: a recommended weighted component contributes its chosen files
// (every shard) as exact selectors with exactMatch like GGUF artifacts, the
// required set keeps its multi-badge semantics, conversions never reach the
// body, and a weightless-only recommendation stays the legacy folder-filter
// body without exactMatch.
func TestRecommendedDownloadCarriesComponentFiles(t *testing.T) {
	analyzeRepo := func(files []FileInfo) *RepoInfo {
		repo := &RepoInfo{Repo: "owner/pipeline", Type: TypeDiffusers, Files: files}
		repo.Diffusers = analyzeDiffusers(files, map[string]interface{}{
			"model_index.json": map[string]interface{}{
				"_class_name": "StableDiffusionPipeline",
				"unet":        []interface{}{"diffusers", "UNet2DConditionModel"},
				"vae":         []interface{}{"diffusers", "AutoencoderKL"},
				"tokenizer":   []interface{}{"transformers", "CLIPTokenizer"},
				"scheduler":   []interface{}{"diffusers", "PNDMScheduler"},
			},
		})
		populateSelectableItems(repo)
		repo.PopulateDownloadSelection()
		return repo
	}

	t.Run("chosen files flow in with every shard", func(t *testing.T) {
		repo := analyzeRepo([]FileInfo{
			wfile("model_index.json", 500),
			wfile("unet/diffusion_pytorch_model.fp16-00001-of-00003.safetensors", 100),
			wfile("unet/diffusion_pytorch_model.fp16-00002-of-00003.safetensors", 110),
			wfile("unet/diffusion_pytorch_model.fp16-00003-of-00003.safetensors", 120),
			wfile("unet/diffusion_pytorch_model.safetensors", 500),
			wfile("unet/model.onnx", 900),
			wfile("vae/diffusion_pytorch_model.safetensors", 330),
			wfile("tokenizer/tokenizer.json", 50),
			wfile("scheduler/scheduler_config.json", 60),
		})
		rd := repo.RecommendedDownload
		if rd == nil || rd.BranchVariant || rd.Revision != "" || !rd.ExactMatch {
			t.Fatalf("recommended download = %+v, want the component file selection with exactMatch", rd)
		}
		got := append([]string(nil), rd.Filters...)
		sort.Strings(got)
		want := []string{
			"scheduler",
			"tokenizer",
			"unet/diffusion_pytorch_model.fp16-00001-of-00003.safetensors",
			"unet/diffusion_pytorch_model.fp16-00002-of-00003.safetensors",
			"unet/diffusion_pytorch_model.fp16-00003-of-00003.safetensors",
			"vae/diffusion_pytorch_model.safetensors",
		}
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("recommended filters = %v, want the chosen weight sets plus weightless folder filters %v", got, want)
		}
		recommended := 0
		for _, item := range repo.SelectableItems {
			if item.Category == "component" && item.Recommended {
				recommended++
			}
		}
		if recommended != 4 {
			t.Fatalf("recommended component rows = %d, want the full required set of 4", recommended)
		}
	})

	t.Run("a weightless-only recommendation stays the legacy body", func(t *testing.T) {
		repo := analyzeRepo([]FileInfo{
			wfile("model_index.json", 500),
			wfile("tokenizer/tokenizer.json", 50),
			wfile("scheduler/scheduler_config.json", 60),
		})
		rd := repo.RecommendedDownload
		if rd == nil || rd.ExactMatch || rd.BranchVariant || rd.Revision != "" {
			t.Fatalf("recommended download = %+v, want the legacy folder-filter body without exactMatch", rd)
		}
		got := append([]string(nil), rd.Filters...)
		sort.Strings(got)
		// Every required component is weightless here, so the body is exactly
		// the legacy folder-filter selection: no files, no exactMatch.
		if want := []string{"scheduler", "tokenizer", "unet", "vae"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("recommended filters = %v, want the unchanged weightless folder filters %v", got, want)
		}
	})
}
