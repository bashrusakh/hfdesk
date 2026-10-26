// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"math"
	"strconv"
	"strings"
)

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

// recognizedNonEXLMethod reports whether method is a quantization method this
// analyzer recognizes and that is not EXL2/EXL3. quantMethodDescriptions is the
// single source of truth for recognized methods; an unfamiliar string is
// unknown evidence, not a known-negative.
func recognizedNonEXLMethod(method string) bool {
	normalized := strings.ToLower(strings.TrimSpace(method))
	if normalized == "" {
		return false
	}
	if _, recognized := quantMethodDescriptions[normalized]; !recognized {
		return false
	}
	switch normalized {
	case "exl2", "exl3":
		return false
	default:
		return true
	}
}

// analyzeQuantized analyzes GPTQ/AWQ/EXL2 quantized models.
func analyzeQuantized(metadata map[string]interface{}) *QuantizedInfo {
	info := &QuantizedInfo{}

	config := quantizationConfig(metadata)
	if config == nil {
		return nil
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
	// config was read completely and every observed declared width is consistent
	// with it (issue #123). Unknown declarations and a partial config prevent
	// that claim; metadata such as bits_per_weight is independent evidence.
	headerBits, hasBits, validBits := declaredWidth(config, "bits")
	knownWidths := make([]float64, 0, 8)
	if hasBits {
		if validBits {
			knownWidths = append(knownWidths, headerBits)
		}
	}
	unknownWidths := hasBits && !validBits
	headBits, hasHeadBits, validHeadBits := declaredWidth(config, "head_bits")
	if hasHeadBits {
		if validHeadBits {
			info.HeadBits = headBits
			knownWidths = append(knownWidths, headBits)
		} else {
			unknownWidths = true
		}
	}
	// Group declarations are local evidence: equal values without a model-wide
	// declaration do not prove either uniformity or variation.
	for _, key := range []string{"vision_bits", "mtp_bits"} {
		if groupBits, present, valid := declaredWidth(config, key); present {
			if valid {
				knownWidths = append(knownWidths, groupBits)
			} else {
				unknownWidths = true
			}
		}
	}
	// A known per-expert range contributes observed widths; an absent or cut
	// range is uncertainty, not evidence of variation.
	for _, key := range []string{"expert_bits", "routed_expert_bits"} {
		expertBits, ok := config[key]
		if !ok {
			continue
		}
		minBits, maxBits, complete := expertBitRange(expertBits)
		if !complete {
			unknownWidths = true
		}
		if minBits != 0 && (info.ExpertBitsMin == 0 || minBits < info.ExpertBitsMin) {
			info.ExpertBitsMin = minBits
		}
		if maxBits > info.ExpertBitsMax {
			info.ExpertBitsMax = maxBits
		}
		if minBits != 0 {
			knownWidths = append(knownWidths, minBits, maxBits)
		}
	}
	// Only demonstrated width differences establish mixed precision.
	for i := 1; i < len(knownWidths); i++ {
		if knownWidths[i] != knownWidths[0] {
			info.MixedPrecision = true
			break
		}
	}
	// A config recovered only partially at the metadata size bound may hold
	// more precision declarations past the cut, so uniformity is not
	// established even when the recovered fields look uniform.
	if _, ok := config[partialHeadMarker]; ok {
		info.ConfigPartial = true
	}
	if intBits, representable := exactIntegerBits(headerBits); hasBits && representable &&
		!info.MixedPrecision && !info.ConfigPartial && !unknownWidths {
		info.Bits = intBits
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

func quantizationConfig(metadata map[string]interface{}) map[string]interface{} {
	for _, path := range []string{"quantize_config.json", "quantization_config.json"} {
		if config, ok := metadata[path].(map[string]interface{}); ok {
			return config
		}
	}
	config, ok := metadata["config.json"].(map[string]interface{})
	if !ok {
		return nil
	}
	if nested, ok := config["quantization_config"].(map[string]interface{}); ok {
		return nested
	}
	return config
}

// hasQuantizationConfig reports whether the metadata contains a dedicated
// quantization file or a nested config.json quantization object. A legacy
// top-level config.json map alone is not enough to opt into type projection.
func hasQuantizationConfig(metadata map[string]interface{}) bool {
	for _, path := range []string{"quantize_config.json", "quantization_config.json"} {
		if _, ok := metadata[path].(map[string]interface{}); ok {
			return true
		}
	}
	config, ok := metadata["config.json"].(map[string]interface{})
	if !ok {
		return false
	}
	_, ok = config["quantization_config"].(map[string]interface{})
	return ok
}

// declaredWidth reports whether a field exists and whether its value is a
// valid positive finite bit width.
func declaredWidth(config map[string]interface{}, key string) (width float64, present, valid bool) {
	value, present := config[key]
	if !present {
		return 0, false, false
	}
	width, ok := value.(float64)
	return width, true, ok && width > 0 && !math.IsNaN(width) && !math.IsInf(width, 0)
}

// exactIntegerBits prevents the public integer field from rounding fractional
// declarations or overflowing on values outside the platform int range.
func exactIntegerBits(width float64) (int, bool) {
	if width <= 0 || math.IsNaN(width) || math.IsInf(width, 0) || math.Trunc(width) != width {
		return 0, false
	}
	maxIntExclusive := math.Ldexp(1, strconv.IntSize-1)
	if width >= maxIntExclusive {
		return 0, false
	}
	return int(width), true
}

// expertBitRange collects valid positive finite widths from the declared
// object/list/scalar while separately reporting whether every branch was
// structurally complete and valid. Unknown leaves block a uniform claim but
// do not erase genuine conflicts among the valid widths.
func expertBitRange(value interface{}) (minBits, maxBits float64, complete bool) {
	complete = true
	var walk func(interface{})
	walk = func(v interface{}) {
		switch t := v.(type) {
		case float64:
			if t <= 0 || math.IsNaN(t) || math.IsInf(t, 0) {
				complete = false
				return
			}
			if minBits == 0 || t < minBits {
				minBits = t
			}
			if t > maxBits {
				maxBits = t
			}
		case map[string]interface{}:
			if len(t) == 0 {
				complete = false
			}
			for _, item := range t {
				walk(item)
			}
		case []interface{}:
			if len(t) == 0 {
				complete = false
			}
			for _, item := range t {
				walk(item)
			}
		default:
			complete = false
		}
	}
	walk(value)
	return minBits, maxBits, complete
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
