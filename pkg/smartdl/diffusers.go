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

			// Calculate component size from files. A component whose folder
			// holds an eligible weight set reports exactly that set (files,
			// size); a folder with no eligible set keeps the whole-folder
			// size it has always had.
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

// weightGroup gathers one candidate set: a plain file's own path, or every
// member of one shard family in one directory and extension.
type weightGroup struct {
	files    []FileInfo
	size     int64
	class    int
	marker   string
	declared int
	// indices holds each shard member's present index NNNNN; nil for a
	// plain-file group.
	indices []int
	// declaredMismatch records members spelling different MMMMM totals.
	declaredMismatch bool
}

// eligible reports whether the group is a complete, well-indexed family: a
// plain file is a trivially complete one-member set; a shard family needs one
// declared total M, M members, and present indices forming exactly the
// distinct set 1..M — no gaps, no duplicates, no out-of-range members.
// Ineligible groups never join selection, so a component whose groups all
// fail falls through to its legacy weightless row.
func (g *weightGroup) eligible() bool {
	if g.indices == nil {
		return true
	}
	if g.declaredMismatch || g.declared < 1 || len(g.files) != g.declared {
		return false
	}
	seen := make(map[int]bool, len(g.indices))
	for _, i := range g.indices {
		if i < 1 || i > g.declared || seen[i] {
			return false
		}
		seen[i] = true
	}
	return len(seen) == g.declared
}

// splitShardSuffix parses a "<prefix>-NNNNN-of-MMMMM" file stem. ok reports
// shard-shaped spelling; index is the present member NNNNN and declared is
// the family total MMMMM.
func splitShardSuffix(stem string) (prefix string, index, declared int, ok bool) {
	idx := strings.LastIndex(stem, "-of-")
	if idx <= 0 || idx+4 >= len(stem) {
		return stem, 0, 0, false
	}
	digits := stem[idx+4:]
	if strings.TrimFunc(digits, func(r rune) bool { return r >= '0' && r <= '9' }) != "" {
		return stem, 0, 0, false
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
		return stem, 0, 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return stem, 0, 0, false
	}
	m, err := strconv.Atoi(left[len(left)-nd:])
	if err != nil {
		return stem, 0, 0, false
	}
	return left[:len(left)-1-nd], m, n, true
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
	if prefix, _, _, ok := splitShardSuffix(stem); ok {
		stem = prefix
	}
	if marker := precisionMarker(stem); marker != "" {
		return marker
	}
	return pathPrecisionMarker(path)
}

// bestWeightSet picks the best weight set inside component/: fp16
// safetensors > default safetensors > .bin, absolute among eligible sets.
// Only a complete, well-indexed family is eligible: a plain file is a
// trivially complete one-member set, and a shard family needs its present
// indices to be exactly the distinct set 1..M with the declared M equal to
// the member count. Conversions never join — by extension
// (.onnx/.msgpack/.tflite/.xml), because a .bin beside a same-basename .xml is
// an OpenVINO member, or because it lives under an onnx/openvino/flax/tflite
// subfolder. Within the winning class plain naming beats a precision-marked
// twin and the lexicographically smallest sorted member list wins; a shard
// family joins whole or not at all. ok is false for a component without
// weights and for one whose every group fails eligibility — split-spelling
// families, declared-total mismatches, index holes and conversions-only
// folders all fall through to the legacy weightless folder row.
func bestWeightSet(files []FileInfo, component string) (componentWeightSet, bool) {
	prefix := component + "/"
	openvinoStems := make(map[string]bool)
	for _, f := range files {
		if strings.HasPrefix(f.Path, prefix) && strings.EqualFold(filepath.Ext(f.Name), ".xml") {
			openvinoStems[strings.TrimSuffix(f.Path, filepath.Ext(f.Name))] = true
		}
	}

	var groups = make(map[string]*weightGroup)

	for _, f := range files {
		if !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f.Name))
		if ext != ".safetensors" && ext != ".bin" {
			continue
		}
		// A conversion folder's payload never joins, whatever weight
		// format it was converted into.
		if inConversionDir(strings.TrimPrefix(f.Path[:strings.LastIndex(f.Path, "/")+1], prefix)) {
			continue
		}
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
			class = 2
		}

		stem := strings.TrimSuffix(f.Name, filepath.Ext(f.Name))
		family, idx, declared, isShard := splitShardSuffix(stem)
		key := f.Path
		if isShard {
			key = f.Path[:strings.LastIndex(f.Path, "/")] + "\x00" + ext + "\x00" + family
		}
		g := groups[key]
		if g == nil {
			g = &weightGroup{class: class, marker: weightFileMarker(f.Path), declared: declared}
			groups[key] = g
		} else if isShard && declared != g.declared {
			g.declaredMismatch = true
		}
		g.files = append(g.files, f)
		g.size += f.Size
		if isShard {
			g.indices = append(g.indices, idx)
		}
	}

	var best *weightGroup
	var bestRank [2]int
	var bestKey string
	for _, g := range groups {
		if !g.eligible() {
			continue
		}
		rank := [2]int{g.class, 0}
		if g.marker != "" && g.class != 0 {
			rank[1] = 1
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

// rankLess orders eligible candidate sets by (class, plain naming, member
// list); completeness is already an eligibility gate, never a tie-break.
func rankLess(aRank [2]int, aKey string, bRank [2]int, bKey string) bool {
	for i := range aRank {
		if aRank[i] != bRank[i] {
			return aRank[i] < bRank[i]
		}
	}
	return aKey < bKey
}

// inConversionDir reports whether a component-relative directory is a known
// conversion folder, so its weight payloads never join a set in any format.
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
