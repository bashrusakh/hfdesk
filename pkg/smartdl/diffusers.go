// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Known diffusers pipeline types and their descriptions.
var pipelineDescriptions = map[string]string{
	"StableDiffusionPipeline":          "Stable Diffusion v1.x text-to-image",
	"StableDiffusionImg2ImgPipeline":   "Stable Diffusion image-to-image",
	"StableDiffusionInpaintPipeline":   "Stable Diffusion inpainting",
	"StableDiffusionXLPipeline":        "Stable Diffusion XL text-to-image",
	"StableDiffusionXLImg2ImgPipeline": "SDXL image-to-image",
	"StableDiffusionXLInpaintPipeline": "SDXL inpainting",
	"FluxPipeline":                     "Flux text-to-image",
	"FluxImg2ImgPipeline":              "Flux image-to-image",
	"FluxControlNetPipeline":           "Flux with ControlNet",
	"KandinskyPipeline":                "Kandinsky v2 text-to-image",
	"KandinskyV22Pipeline":             "Kandinsky v2.2 text-to-image",
	"StableVideoDiffusionPipeline":     "Stable Video Diffusion",
	"PixArtAlphaPipeline":              "PixArt-α text-to-image",
	"HunyuanDiTPipeline":               "Hunyuan-DiT text-to-image",
	"WuerstchenPipeline":               "Würstchen text-to-image",
	"AnimateDiffPipeline":              "AnimateDiff animation",
	"LatentConsistencyModelPipeline":   "LCM fast inference",
}

// Component requirements for known pipelines.
var requiredComponents = map[string][]string{
	"StableDiffusionPipeline":   {"unet", "vae", "text_encoder", "tokenizer", "scheduler"},
	"StableDiffusionXLPipeline": {"unet", "vae", "text_encoder", "text_encoder_2", "tokenizer", "tokenizer_2", "scheduler"},
	"FluxPipeline":              {"transformer", "vae", "text_encoder", "text_encoder_2", "tokenizer", "tokenizer_2", "scheduler"},
}

// analyzeDiffusers analyzes a diffusers repository.
func analyzeDiffusers(files []FileInfo, metadata map[string]interface{}) *DiffusersInfo {
	info := &DiffusersInfo{}

	// Parse model_index.json
	if modelIndex, ok := metadata["model_index.json"].(map[string]interface{}); ok {
		// Get pipeline type
		if className, ok := modelIndex["_class_name"].(string); ok {
			info.PipelineType = className
			if desc, exists := pipelineDescriptions[className]; exists {
				info.PipelineDescription = desc
			} else {
				info.PipelineDescription = "Diffusers pipeline"
			}
		}

		// Get diffusers version
		if version, ok := modelIndex["_diffusers_version"].(string); ok {
			info.DiffusersVersion = version
		}

		// Parse components
		for key, value := range modelIndex {
			if strings.HasPrefix(key, "_") {
				continue // Skip metadata fields
			}

			comp := DiffusersComponent{
				Name: key,
			}

			// Parse component config
			if arr, ok := value.([]interface{}); ok && len(arr) >= 2 {
				if lib, ok := arr[0].(string); ok {
					comp.Library = lib
				}
				if cls, ok := arr[1].(string); ok {
					comp.ClassName = cls
				}
			}

			// Calculate component size from files. A component with weights
			// reports exactly its best weight set (files, size); a weightless
			// folder keeps the whole-folder size it has always had.
			comp.Size = calculateComponentSize(files, key)
			comp.SizeHuman = humanSize(comp.Size)
			if set, ok := bestWeightSet(files, key); ok {
				comp.Files = set.files
				comp.Size = set.size
				comp.SizeHuman = humanSize(set.size)
			}

			// Determine if required
			if required, ok := requiredComponents[info.PipelineType]; ok {
				for _, r := range required {
					if r == key {
						comp.Required = true
						break
					}
				}
			}

			info.Components = append(info.Components, comp)
		}
	}

	// Detect available variants (fp16, fp32, bf16)
	info.Variants = detectVariants(files)

	// Detect available precisions
	info.Precisions = detectPrecisions(files)

	return info
}

// calculateComponentSize calculates total size of files in a component directory.
func calculateComponentSize(files []FileInfo, componentName string) int64 {
	var total int64
	prefix := componentName + "/"

	for _, f := range files {
		if strings.HasPrefix(f.Path, prefix) {
			total += f.Size
		}
	}

	return total
}

// componentWeightSet is the best weight set chosen for one component folder.
type componentWeightSet struct {
	// files are the chosen members sorted by path: every shard of one family,
	// conversions and support files excluded.
	files []FileInfo
	size  int64
	// marker is the chosen set's precision marker ("fp16", "fp32", "bf16")
	// or "" for default naming; it feeds the row label.
	marker string
}

// splitShardSuffix parses a "<prefix>-NNNNN-of-MMMMM" file stem. ok reports
// shard-shaped spelling; declared is the family total MMMMM.
func splitShardSuffix(stem string) (prefix string, declared int, ok bool) {
	idx := strings.LastIndex(stem, "-of-")
	if idx <= 0 || idx+4 >= len(stem) {
		return stem, 0, false
	}
	digits := stem[idx+4:]
	if strings.TrimFunc(digits, func(r rune) bool { return r >= '0' && r <= '9' }) != "" {
		return stem, 0, false
	}
	left := stem[:idx]
	nd := 0
	for nd < len(left) {
		if c := left[len(left)-1-nd]; c < '0' || c > '9' {
			break
		}
		nd++
	}
	if nd == 0 || nd >= len(left) || left[len(left)-1-nd] != '-' {
		return stem, 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return stem, 0, false
	}
	return left[:len(left)-1-nd], n, true
}

// precisionMarker returns the precision marker recognized in a weight
// filename stem: detectVariants' name rules plus the bare end-of-stem form
// shard stripping leaves behind ("model.fp16").
func precisionMarker(stem string) string {
	lower := strings.ToLower(stem)
	for _, marker := range []string{"fp16", "fp32", "bf16"} {
		if strings.Contains(lower, "."+marker+".") || strings.Contains(lower, "_"+marker+".") ||
			strings.HasSuffix(lower, "."+marker) || strings.HasSuffix(lower, "_"+marker) ||
			lower == marker {
			return marker
		}
	}
	return ""
}

// pathPrecisionMarker returns the precision marker of a path's variant
// directory ("fp16/..." folders), mirroring detectVariants' dir rule.
func pathPrecisionMarker(rel string) string {
	dir := rel
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		dir = dir[:i]
	}
	for _, seg := range strings.Split(dir, "/") {
		switch strings.ToLower(seg) {
		case "fp16", "fp32", "bf16":
			return strings.ToLower(seg)
		}
	}
	return ""
}

// weightFileMarker is a weight file's precision marker: its stem's, else its
// variant directory's.
func weightFileMarker(path string) string {
	name := path[strings.LastIndex(path, "/")+1:]
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	if prefix, _, ok := splitShardSuffix(stem); ok {
		stem = prefix
	}
	if marker := precisionMarker(stem); marker != "" {
		return marker
	}
	return pathPrecisionMarker(path)
}

// bestWeightSet picks the best weight set inside component/: fp16 safetensors
// > default safetensors > .bin. Conversions never join — by extension
// (.onnx/.msgpack/.tflite/.xml), because a .bin beside a same-basename .xml is
// an OpenVINO member, or because it lives under an onnx/openvino/flax/tflite
// subfolder. Within the winning class a complete set beats an incomplete
// shard family, plain naming beats a precision-marked twin, and the
// lexicographically smallest sorted member list wins; a shard family joins
// whole or not at all. ok is false for a component without weights.
func bestWeightSet(files []FileInfo, component string) (componentWeightSet, bool) {
	prefix := component + "/"
	openvinoStems := make(map[string]bool)
	for _, f := range files {
		if strings.HasPrefix(f.Path, prefix) && strings.EqualFold(filepath.Ext(f.Name), ".xml") {
			openvinoStems[strings.TrimSuffix(f.Path, filepath.Ext(f.Name))] = true
		}
	}

	type weightGroup struct {
		files    []FileInfo
		size     int64
		class    int
		marker   string
		declared int
	}
	var groups = make(map[string]*weightGroup)

	for _, f := range files {
		if !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f.Name))
		var class int
		switch ext {
		case ".safetensors":
			if weightFileMarker(f.Path) == "fp16" {
				class = 0
			} else {
				class = 1
			}
		case ".bin":
			if openvinoStems[strings.TrimSuffix(f.Path, filepath.Ext(f.Name))] {
				continue
			}
			if inConversionDir(strings.TrimPrefix(f.Path[:strings.LastIndex(f.Path, "/")], prefix)) {
				continue
			}
			class = 2
		default:
			continue
		}

		stem := strings.TrimSuffix(f.Name, filepath.Ext(f.Name))
		family, declared, isShard := splitShardSuffix(stem)
		key := f.Path
		if isShard {
			key = f.Path[:strings.LastIndex(f.Path, "/")] + "\x00" + ext + "\x00" + family
		}
		g := groups[key]
		if g == nil {
			g = &weightGroup{class: class, marker: weightFileMarker(f.Path), declared: declared}
			groups[key] = g
		}
		g.files = append(g.files, f)
		g.size += f.Size
	}

	var best *weightGroup
	var bestRank [3]int
	var bestKey string
	for _, g := range groups {
		complete := g.declared == 0 || len(g.files) == g.declared
		rank := [3]int{g.class, 0, 0}
		if !complete {
			rank[1] = 1
		}
		if g.marker != "" && g.class != 0 {
			rank[2] = 1
		}
		sort.Slice(g.files, func(i, j int) bool { return g.files[i].Path < g.files[j].Path })
		memberPaths := make([]string, 0, len(g.files))
		for _, f := range g.files {
			memberPaths = append(memberPaths, f.Path)
		}
		memberKey := strings.Join(memberPaths, "\n")
		if best == nil || rankLess(rank, memberKey, bestRank, bestKey) {
			best, bestRank, bestKey = g, rank, memberKey
		}
	}
	if best == nil {
		return componentWeightSet{}, false
	}
	var size int64
	for _, f := range best.files {
		size += f.Size
	}
	marker := best.marker
	if best.class == 0 {
		marker = "fp16"
	}
	return componentWeightSet{files: best.files, size: size, marker: marker}, true
}

// rankLess orders candidate sets by (class, completeness, plain naming,
// member list).
func rankLess(aRank [3]int, aKey string, bRank [3]int, bKey string) bool {
	for i := range aRank {
		if aRank[i] != bRank[i] {
			return aRank[i] < bRank[i]
		}
	}
	return aKey < bKey
}

// inConversionDir reports whether a component-relative directory is a known
// conversion folder, so its .bin payloads never read as pytorch weights.
func inConversionDir(relDir string) bool {
	for _, seg := range strings.Split(relDir, "/") {
		switch strings.ToLower(seg) {
		case "onnx", "openvino", "flax", "tflite":
			return true
		}
	}
	return false
}

// detectVariants finds available variants (fp16, fp32, bf16) in files.
func detectVariants(files []FileInfo) []string {
	variants := make(map[string]bool)

	for _, f := range files {
		name := strings.ToLower(f.Name)
		dir := strings.ToLower(f.Directory)

		// Check for variant in filename
		if strings.Contains(name, ".fp16.") || strings.Contains(name, "_fp16.") {
			variants["fp16"] = true
		}
		if strings.Contains(name, ".fp32.") || strings.Contains(name, "_fp32.") {
			variants["fp32"] = true
		}
		if strings.Contains(name, ".bf16.") || strings.Contains(name, "_bf16.") {
			variants["bf16"] = true
		}

		// Check for variant directories
		if strings.Contains(dir, "/fp16") || dir == "fp16" {
			variants["fp16"] = true
		}
		if strings.Contains(dir, "/fp32") || dir == "fp32" {
			variants["fp32"] = true
		}
		if strings.Contains(dir, "/bf16") || dir == "bf16" {
			variants["bf16"] = true
		}
	}

	var result []string
	for v := range variants {
		result = append(result, v)
	}
	return result
}

// detectPrecisions finds available precisions from safetensors/bin files.
func detectPrecisions(files []FileInfo) []string {
	precisions := make(map[string]bool)

	for _, f := range files {
		name := strings.ToLower(f.Name)

		// Standard model files indicate fp32 by default
		if strings.HasSuffix(name, ".safetensors") || strings.HasSuffix(name, ".bin") {
			if !strings.Contains(name, "fp16") && !strings.Contains(name, "bf16") {
				precisions["fp32"] = true
			}
		}
	}

	var result []string
	for p := range precisions {
		result = append(result, p)
	}
	return result
}

// GetComponentFiles returns all files belonging to a component.
func GetComponentFiles(files []FileInfo, componentName string) []FileInfo {
	var result []FileInfo
	prefix := componentName + "/"

	for _, f := range files {
		// Files directly in component directory
		if strings.HasPrefix(f.Path, prefix) {
			result = append(result, f)
			continue
		}

		// Root-level files with component prefix
		if filepath.Dir(f.Path) == "." && strings.HasPrefix(f.Name, componentName) {
			result = append(result, f)
		}
	}

	return result
}

// CalculateDownloadSize calculates total size for selected components and variant.
func CalculateDownloadSize(info *DiffusersInfo, files []FileInfo, selectedComponents []string, variant string) int64 {
	var total int64
	selected := make(map[string]bool)
	for _, c := range selectedComponents {
		selected[c] = true
	}

	for _, f := range files {
		// Check if file belongs to a selected component
		dir := strings.Split(f.Path, "/")[0]
		if !selected[dir] && len(selected) > 0 {
			continue
		}

		// Check variant match
		name := strings.ToLower(f.Name)
		if variant != "" {
			// Skip files that are different variant
			for _, v := range []string{"fp16", "fp32", "bf16"} {
				if v != variant && (strings.Contains(name, "."+v+".") || strings.Contains(name, "_"+v+".")) {
					continue
				}
			}
		}

		total += f.Size
	}

	return total
}

// DiffusersToSelectableItems converts Diffusers components and variants to SelectableItems.
func DiffusersToSelectableItems(info *DiffusersInfo) []SelectableItem {
	if info == nil {
		return nil
	}

	var items []SelectableItem

	// Add variants if available (fp16, fp32, bf16)
	variantDescriptions := map[string]string{
		"fp16": "Half precision - Recommended, uses less VRAM",
		"fp32": "Full precision - Maximum quality, more VRAM",
		"bf16": "Brain float - Good quality, efficient on modern GPUs",
	}

	for _, variant := range info.Variants {
		desc := variantDescriptions[variant]
		if desc == "" {
			desc = "Model variant"
		}

		item := SelectableItem{
			ID:          variant,
			Label:       strings.ToUpper(variant),
			Description: desc,
			Recommended: variant == "fp16",
			Category:    "variant",
			FilterValue: variant,
		}
		items = append(items, item)
	}

	// Add components
	for _, comp := range info.Components {
		desc := comp.ClassName
		if desc == "" {
			desc = comp.Library + " component"
		}

		label := comp.Name
		var chosen []string
		if len(comp.Files) > 0 {
			chosen = make([]string, 0, len(comp.Files))
			for _, f := range comp.Files {
				chosen = append(chosen, f.Path)
			}
			label = componentLabel(comp.Name, comp.Files[0].Path)
		}

		item := SelectableItem{
			ID:          comp.Name,
			Label:       label,
			Description: desc,
			Size:        comp.Size,
			SizeHuman:   comp.SizeHuman,
			Recommended: comp.Required,
			Category:    "component",
			FilterValue: comp.Name,
			Files:       chosen,
		}
		items = append(items, item)
	}

	return items
}

// componentLabel renders a ready component row: "Unet (fp16)" — the component
// name carrying its chosen set's precision marker where the set has one.
// Weightless components keep their plain name (their legacy row is untouched).
func componentLabel(name, samplePath string) string {
	if name == "" {
		return name
	}
	runes := []rune(name)
	label := strings.ToUpper(string(runes[0])) + string(runes[1:])
	if marker := weightFileMarker(samplePath); marker != "" {
		label += " (" + marker + ")"
	}
	return label
}
