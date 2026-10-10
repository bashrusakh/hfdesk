// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import "strings"

// Quantization method descriptions.
var quantMethodDescriptions = map[string]string{
	"gptq":         "GPTQ - GPU-accelerated post-training quantization",
	"awq":          "AWQ - Activation-aware Weight Quantization",
	"exl2":         "EXL2 - ExLlamaV2 mixed-precision quantization",
	"exl3":         "EXL3 - ExLlamaV3 quantization",
	"bitsandbytes": "bitsandbytes INT8/INT4 quantization",
	"bnb":          "bitsandbytes INT8/INT4 quantization",
	"hqq":          "HQQ - Half-Quadratic Quantization",
	"eetq":         "EETQ - Easy and Efficient Quantization",
}

// analyzeQuantized analyzes GPTQ/AWQ/EXL2 quantized models.
func analyzeQuantized(metadata map[string]interface{}) *QuantizedInfo {
	info := &QuantizedInfo{}

	// Parse quantize_config.json
	config, ok := metadata["quantize_config.json"].(map[string]interface{})
	if !ok {
		config, ok = metadata["quantization_config.json"].(map[string]interface{})
	}
	if !ok {
		// Try config.json for some quantized models
		config, ok = metadata["config.json"].(map[string]interface{})
		if !ok {
			return nil
		}
	}

	// Detect quantization method
	if method, ok := config["quant_method"].(string); ok {
		info.Method = method
		if desc, exists := quantMethodDescriptions[method]; exists {
			info.MethodDescription = desc
		}
	}

	// Declared precision. A header "bits" value describes only the tensors it
	// applies to: it may be presented as model-wide precision only when the
	// config was read completely and every declared width is consistent with
	// uniform precision (issue #123). Per-expert width declarations, differing
	// group widths, effective/aggregate precision evidence, and a partially
	// recovered config all prevent that claim.
	headerBits, hasBits := config["bits"].(float64)
	if headBits, ok := config["head_bits"].(float64); ok {
		info.HeadBits = headBits
		if !hasBits || headBits != headerBits {
			info.MixedPrecision = true
		}
	}
	// Other tensor-group widths contradict a uniform claim when they differ.
	for _, key := range []string{"vision_bits", "mtp_bits"} {
		if groupBits, ok := config[key].(float64); ok && (!hasBits || groupBits != headerBits) {
			info.MixedPrecision = true
		}
	}
	// Per-expert width declarations are never model-wide precision. Completed
	// declarations are reflected as bounds; a cut declaration (nil) proves only
	// that per-expert widths exist, so no range is invented for it.
	for _, key := range []string{"expert_bits", "routed_expert_bits"} {
		expertBits, ok := config[key]
		if !ok {
			continue
		}
		info.MixedPrecision = true
		minBits, maxBits := expertBitRange(expertBits)
		if minBits != 0 && (info.ExpertBitsMin == 0 || minBits < info.ExpertBitsMin) {
			info.ExpertBitsMin = minBits
		}
		if maxBits > info.ExpertBitsMax {
			info.ExpertBitsMax = maxBits
		}
	}
	// Effective or aggregated precision evidence (EXL2 bits per weight, an
	// expert width average) is never proof of uniform model-wide precision.
	for _, key := range []string{"bits_per_weight", "routed_expert_bits_avg"} {
		if _, ok := config[key]; ok {
			info.MixedPrecision = true
		}
	}
	// A config recovered only partially at the metadata size bound may hold
	// more precision declarations past the cut, so uniformity is not
	// established even when the recovered fields look uniform.
	if _, ok := config[partialHeadMarker]; ok {
		info.ConfigPartial = true
	}
	if !info.MixedPrecision && !info.ConfigPartial {
		info.Bits = int(headerBits)
	}

	if groupSize, ok := config["group_size"].(float64); ok {
		info.GroupSize = int(groupSize)
	}

	if descAct, ok := config["desc_act"].(bool); ok {
		info.DescAct = descAct
	}

	if symm, ok := config["sym"].(bool); ok {
		info.Symmetric = symm
	}

	// AWQ specific fields
	if zeroPoint, ok := config["zero_point"].(bool); ok {
		info.ZeroPoint = zeroPoint
	}

	if version, ok := config["version"].(string); ok {
		info.Version = version
	}

	// EXL2 specific
	if bpw, ok := config["bits_per_weight"].(float64); ok {
		info.BitsPerWeight = bpw
	}

	// Module quantization info
	if modules, ok := config["modules_to_not_convert"].([]interface{}); ok {
		for _, m := range modules {
			if s, ok := m.(string); ok {
				info.ExcludedModules = append(info.ExcludedModules, s)
			}
		}
	}

	// Backend compatibility
	info.Backends = detectBackends(info)

	// Get model architecture from config.json if available
	if configJson, ok := metadata["config.json"].(map[string]interface{}); ok {
		if arch, ok := configJson["architectures"].([]interface{}); ok && len(arch) > 0 {
			if s, ok := arch[0].(string); ok {
				info.ModelArchitecture = s
			}
		}

		// Model size for VRAM estimation
		if hiddenSize, ok := configJson["hidden_size"].(float64); ok {
			if numLayers, ok := configJson["num_hidden_layers"].(float64); ok {
				info.EstimatedVRAM = estimateVRAM(int(hiddenSize), int(numLayers), info.Bits)
			}
		}
	}

	return info
}

// expertBitRange returns the smallest and largest numeric bit widths declared
// anywhere inside a per-expert width declaration (expert_bits /
// routed_expert_bits: a per-expert object, a list, or a bare width). Both are 0
// when the value declares no numeric widths, including a declaration whose
// value was cut at the metadata bound.
func expertBitRange(value interface{}) (minBits, maxBits float64) {
	var walk func(interface{})
	walk = func(v interface{}) {
		switch t := v.(type) {
		case float64:
			if minBits == 0 || t < minBits {
				minBits = t
			}
			if t > maxBits {
				maxBits = t
			}
		case map[string]interface{}:
			for _, item := range t {
				walk(item)
			}
		case []interface{}:
			for _, item := range t {
				walk(item)
			}
		}
	}
	walk(value)
	return minBits, maxBits
}

// detectBackends returns compatible inference backends.
func detectBackends(info *QuantizedInfo) []string {
	var backends []string

	switch info.Method {
	case "gptq":
		backends = append(backends, "auto-gptq", "exllamav2", "transformers")
		if info.GroupSize == 128 && !info.DescAct {
			backends = append(backends, "vllm")
		}
	case "awq":
		backends = append(backends, "autoawq", "vllm", "transformers")
	case "exl2":
		backends = append(backends, "exllamav2")
	case "bitsandbytes", "bnb":
		backends = append(backends, "transformers", "bitsandbytes")
	case "hqq":
		backends = append(backends, "hqq", "transformers")
	case "eetq":
		backends = append(backends, "eetq", "transformers")
	}

	return backends
}

// estimateVRAM estimates GPU VRAM requirements for quantized models.
// This is a rough estimate based on model dimensions and bit width.
func estimateVRAM(hiddenSize, numLayers, bits int) int64 {
	if bits == 0 {
		bits = 4 // Default assumption
	}

	// Rough parameter count estimation for transformer models
	// params ≈ 12 * hidden_size^2 * num_layers (simplified)
	paramsApprox := int64(12) * int64(hiddenSize) * int64(hiddenSize) * int64(numLayers)

	// Bytes per parameter based on bit width
	bytesPerParam := float64(bits) / 8.0

	// Add 20% overhead for KV cache and activations
	vram := int64(float64(paramsApprox) * bytesPerParam * 1.2)

	return vram
}

// IsGPTQ checks if the model uses GPTQ quantization.
func IsGPTQ(info *QuantizedInfo) bool {
	return info.Method == "gptq"
}

// IsAWQ checks if the model uses AWQ quantization.
func IsAWQ(info *QuantizedInfo) bool {
	return info.Method == "awq"
}

// IsEXL2 checks if the model uses EXL2 quantization.
func IsEXL2(info *QuantizedInfo) bool {
	return info.Method == "exl2"
}

// VRAMHuman returns VRAM estimate as human-readable string.
func VRAMHuman(info *QuantizedInfo) string {
	return humanSize(info.EstimatedVRAM)
}

// SupportsBackend checks if a specific backend is compatible.
func SupportsBackend(info *QuantizedInfo, backend string) bool {
	for _, b := range info.Backends {
		if b == backend {
			return true
		}
	}
	return false
}

// QuantizedToSelectableItems converts quantized model info to SelectableItems.
// For GPTQ/AWQ, there's typically only one version, so this shows informational items.
func QuantizedToSelectableItems(info *QuantizedInfo, files []FileInfo) []SelectableItem {
	if info == nil {
		return nil
	}

	var items []SelectableItem

	// Check for safetensors vs bin files
	hasSafetensors := false
	hasPytorchBin := false
	var safetensorsSize, pytorchBinSize int64

	for _, f := range files {
		lower := strings.ToLower(f.Name)
		if strings.HasSuffix(lower, ".safetensors") {
			hasSafetensors = true
			safetensorsSize += f.Size
		} else if strings.HasSuffix(lower, ".bin") && !strings.Contains(lower, "tokenizer") {
			hasPytorchBin = true
			pytorchBinSize += f.Size
		}
	}

	// Add format options if both are available
	if hasSafetensors && hasPytorchBin {
		items = append(items, SelectableItem{
			ID:           "safetensors",
			Label:        "SafeTensors",
			Description:  "Faster loading, recommended",
			Size:         safetensorsSize,
			SizeHuman:    humanSize(safetensorsSize),
			Quality:      5,
			QualityStars: "★★★★★",
			Recommended:  true,
			Category:     "format",
			FilterValue:  "safetensors",
		})

		items = append(items, SelectableItem{
			ID:           "pytorch",
			Label:        "PyTorch (.bin)",
			Description:  "Legacy format",
			Size:         pytorchBinSize,
			SizeHuman:    humanSize(pytorchBinSize),
			Quality:      3,
			QualityStars: "★★★☆☆",
			Recommended:  false,
			Category:     "format",
			FilterValue:  ".bin",
		})
	}

	return items
}
