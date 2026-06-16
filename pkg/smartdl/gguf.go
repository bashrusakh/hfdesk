// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// GGUF quantization quality ratings (1-5 stars).
//
// Entries prefixed/suffixed with UD-style modifiers (e.g. Q4_K_XL) are unsloth's
// "Unsloth Dynamic" quants — same bit-budget family as the corresponding K
// variant but with improved layer-by-layer quantization, rated at or above the
// closest standard K_M equivalent.
var quantQuality = map[string]int{
	// 1-bit (unsloth dynamic only)
	"IQ1_S": 1,
	"IQ1_M": 1,

	// 2-bit
	"Q2_K":    1,
	"Q2_K_S":  1,
	"Q2_K_L":  1,
	"Q2_K_XL": 2, // unsloth dynamic, better than plain Q2_K
	"IQ2_S":   1,
	"IQ2_M":   1,
	"IQ2_XS":  1,
	"IQ2_XXS": 1,

	// 3-bit
	"Q3_K_S":  2,
	"Q3_K_M":  2,
	"Q3_K_L":  2,
	"Q3_K_XL": 3, // unsloth dynamic
	"IQ3_S":   2,
	"IQ3_XS":  2,
	"IQ3_XXS": 2,
	"IQ3_M":   2,

	// 4-bit
	"Q4_0":    3,
	"Q4_1":    3,
	"Q4_K_S":  3,
	"Q4_K_L":  4,
	"Q4_K_M":  4,
	"Q4_K_XL": 4, // unsloth dynamic, comparable to Q4_K_M
	"IQ4_NL":  3,
	"IQ4_XS":  3,

	// 5-bit
	"Q5_0":    4,
	"Q5_1":    4,
	"Q5_K_S":  4,
	"Q5_K_L":  5,
	"Q5_K_M":  5,
	"Q5_K_XL": 5, // unsloth dynamic

	// 6-bit
	"Q6_K":    5,
	"Q6_K_L":  5,
	"Q6_K_XL": 5, // unsloth dynamic

	// 8-bit
	"Q8_0":    5,
	"Q8_K_XL": 5, // unsloth dynamic

	// Full / half precision
	"F16":  5,
	"F32":  5,
	"BF16": 5,

	// Other native GGUF formats
	"MXFP4_MOE": 4,

	// APEX MoE-aware mixed precision quantization profiles
	"APEX_QUALITY":    5,
	"APEX_I_QUALITY":  5,
	"APEX_BALANCED":   4,
	"APEX_I_BALANCED": 4,
	"APEX_COMPACT":    3,
	"APEX_I_COMPACT":  3,
	"APEX_MINI":       2,
	"APEX_I_MINI":     2,
}

// quantDescriptions provides human-readable descriptions for quantization levels.
var quantDescriptions = map[string]string{
	// 1-bit
	"IQ1_S": "Unsloth dynamic 1-bit, small",
	"IQ1_M": "Unsloth dynamic 1-bit, medium",

	// 2-bit
	"Q2_K":    "Smallest, significant quality loss",
	"Q2_K_S":  "Smallest, significant quality loss",
	"Q2_K_L":  "Small 2-bit, noticeable quality loss",
	"Q2_K_XL": "Unsloth dynamic 2-bit, improved quality",
	"IQ2_S":   "Importance matrix 2-bit, small",
	"IQ2_M":   "Importance matrix 2-bit, medium",
	"IQ2_XS":  "Importance matrix 2-bit, extra small",
	"IQ2_XXS": "Importance matrix 2-bit, extra extra small",

	// 3-bit
	"Q3_K_S":  "Very small, noticeable quality loss",
	"Q3_K_M":  "Small, noticeable quality loss",
	"Q3_K_L":  "Small, noticeable quality loss",
	"Q3_K_XL": "Unsloth dynamic 3-bit, improved quality",
	"IQ3_S":   "Importance matrix 3-bit, small",
	"IQ3_XS":  "Importance matrix 3-bit, extra small",
	"IQ3_XXS": "Importance matrix 3-bit, extra extra small",
	"IQ3_M":   "Importance matrix 3-bit, medium",

	// 4-bit
	"Q4_0":    "Legacy 4-bit, good balance",
	"Q4_1":    "Legacy 4-bit with scales",
	"Q4_K_S":  "Small 4-bit, good quality",
	"Q4_K_L":  "Large 4-bit, recommended",
	"Q4_K_M":  "Medium 4-bit, recommended",
	"Q4_K_XL": "Unsloth dynamic 4-bit, recommended",
	"IQ4_NL":  "Importance matrix 4-bit, non-linear",
	"IQ4_XS":  "Importance matrix 4-bit, extra small",

	// 5-bit
	"Q5_0":    "Legacy 5-bit, very good quality",
	"Q5_1":    "Legacy 5-bit with scales",
	"Q5_K_S":  "Small 5-bit, excellent quality",
	"Q5_K_L":  "Large 5-bit, excellent quality",
	"Q5_K_M":  "Medium 5-bit, excellent quality",
	"Q5_K_XL": "Unsloth dynamic 5-bit, excellent quality",

	// 6-bit
	"Q6_K":    "6-bit, near-lossless",
	"Q6_K_L":  "Large 6-bit, near-lossless",
	"Q6_K_XL": "Unsloth dynamic 6-bit, near-lossless",

	// 8-bit
	"Q8_0":    "8-bit, minimal loss",
	"Q8_K_XL": "Unsloth dynamic 8-bit, minimal loss",

	// Full / half precision
	"F16":  "Half precision, full quality",
	"F32":  "Full precision, original quality",
	"BF16": "Brain float 16, full quality",

	// Other native GGUF formats
	"MXFP4_MOE": "MXFP4 MoE quantization",

	// APEX MoE-aware mixed precision quantization profiles
	"APEX_QUALITY":    "APEX Quality profile",
	"APEX_I_QUALITY":  "APEX importance-matrix Quality profile",
	"APEX_BALANCED":   "APEX Balanced profile",
	"APEX_I_BALANCED": "APEX importance-matrix Balanced profile",
	"APEX_COMPACT":    "APEX Compact profile",
	"APEX_I_COMPACT":  "APEX importance-matrix Compact profile",
	"APEX_MINI":       "APEX Mini profile",
	"APEX_I_MINI":     "APEX importance-matrix Mini profile",
}

// Regex patterns for parsing GGUF filenames.
//
// quantPattern captures the full quantization type from a filename segment.
// Supports:
//   - IQ1..IQ4 importance-matrix quants with XXS/XS/S/M/NL suffixes
//   - Q2..Q8 with legacy _0/_1 suffixes, plain _K, or _K with
//     S/M/L/XL/XXL suffixes (XL/XXL are unsloth "Unsloth Dynamic" quants)
//   - F16/F32/BF16 float precisions
//   - MXFP4_MOE and APEX profile-style GGUF quantizations
//
// Alternation order inside the _K suffix group puts longer literals first
// (XXL before XL before L) so the longest applicable suffix is always captured.
var (
	quantPattern = regexp.MustCompile(`(?i)(UD[-_])?(IQ[1-4]_(?:XXS|XS|S|M|NL)|Q[2-8]_(?:[01]|K(?:_(?:XXL|XL|L|M|S))?)|APEX[-_](?:I[-_])?(?:BALANCED|COMPACT|MINI|QUALITY)|MXFP4_MOE|BF16|F(?:16|32))`)

	// Match parameter count: 7B, 13B, 70B, 1.5B, etc.
	paramPattern = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)[Bb]`)

	// Match model name from filename (before quant type).
	modelNamePattern = regexp.MustCompile(`^(.+?)[-._](?:IQ|Q|BF|F)\d`)

	// Match the shard suffix of a split GGUF, e.g. "-00001-of-00003" optionally
	// followed by ".gguf". HuggingFace/llama.cpp use 5-digit indices; allow 4-5
	// digits to be safe. Used to group shards of the same quantization together.
	splitSuffixPattern = regexp.MustCompile(`(?i)-\d{4,5}-of-\d{4,5}(?:\.gguf)?$`)
)

// stripSplitSuffix removes the "-NNNNN-of-MMMMM[.gguf]" shard suffix from a
// filename, returning the base used to group shards of one quantization.
func stripSplitSuffix(name string) string {
	return splitSuffixPattern.ReplaceAllString(name, "")
}

// isSplitShard reports whether a filename carries a split-GGUF shard suffix.
func isSplitShard(name string) bool {
	return splitSuffixPattern.MatchString(name)
}

// ggufQuantType extracts a quantization token from either the filename or its
// repo-relative path. Some GGUF repos put split shards under a quantized
// directory such as "UD-Q4_K_XL/" while the individual shard names omit the
// quant token, so checking only FileInfo.Name is not enough.
func ggufQuantType(f FileInfo) string {
	candidates := []string{f.Name, f.Path}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if m := quantPattern.FindStringSubmatch(strings.ToUpper(candidate)); len(m) >= 3 {
			quant := strings.ReplaceAll(strings.ToUpper(m[2]), "-", "_")
			if m[1] != "" {
				return "UD_" + quant
			}
			return quant
		}
	}
	return ""
}

// isMMProjFile reports whether a GGUF filename is a multimodal projector
// (vision encoder) file. These files live alongside LLM quantizations in
// multimodal GGUF repos and must be downloaded as a companion to the chosen
// LLM quant — they are not user-selectable quantizations themselves.
func isMMProjFile(name string) bool {
	return strings.Contains(strings.ToLower(filepath.Base(name)), "mmproj")
}

func isGGUFImatrixFile(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	return base == "imatrix.gguf" || strings.HasSuffix(base, "-imatrix.gguf") || strings.HasSuffix(base, ".imatrix.gguf")
}

// isGGUFMTPFile reports whether a GGUF filename is a Multi-Token Prediction
// (MTP) head companion file. These files sit alongside LLM quantizations in
// GGUF repos (e.g. mtp-gemma-4-26B-A4B-it.gguf) but are not user-selectable
// quantizations themselves. MTP files that carry a recognized quantization
// token (e.g. gemma-4-26B-A4B-it-BF16-MTP.gguf) are not caught here — they
// have proper quant labels and will still appear as quantization options.
func isGGUFMTPFile(name string) bool {
	return strings.HasPrefix(strings.ToLower(filepath.Base(name)), "mtp-")
}

// analyzeGGUF analyzes GGUF files and extracts quantization information.
// Multimodal projector ("mmproj") files are detected and kept separate from
// LLM quantizations so the picker shows clean quant options while still
// knowing to bundle the vision encoder with any quant download. MTP
// (Multi-Token Prediction) companion files without a recognized quantization
// token are filtered out alongside imatrix files.
func analyzeGGUF(files []FileInfo) *GGUFInfo {
	info := &GGUFInfo{}

	// Partition .gguf files into LLM quants vs mmproj vision encoders.
	var llmFiles []FileInfo
	for _, f := range files {
		if !strings.HasSuffix(strings.ToLower(f.Name), ".gguf") {
			continue
		}
		if isMMProjFile(f.Name) {
			info.MMProjFiles = append(info.MMProjFiles, f)
			continue
		}
		if isGGUFImatrixFile(f.Name) {
			continue
		}
		if isGGUFMTPFile(f.Name) {
			continue
		}
		llmFiles = append(llmFiles, f)
	}

	if len(llmFiles) == 0 && len(info.MMProjFiles) == 0 {
		return nil
	}

	// Extract model name and parameter count from the first LLM file (or the
	// first mmproj file if the repo is mmproj-only, which is rare).
	var nameSource string
	if len(llmFiles) > 0 {
		nameSource = llmFiles[0].Name
	} else {
		nameSource = info.MMProjFiles[0].Name
	}
	if matches := modelNamePattern.FindStringSubmatch(nameSource); len(matches) > 1 {
		info.ModelName = strings.ReplaceAll(matches[1], "-", " ")
		info.ModelName = strings.ReplaceAll(info.ModelName, "_", " ")
	}
	if matches := paramPattern.FindStringSubmatch(nameSource); len(matches) > 1 {
		info.ParameterCount = matches[1] + "B"
	}

	// Group LLM GGUF files into quantizations. Sharded/split quants (files named
	// like model-Q4_K_M-00001-of-00003.gguf) must be merged into one entry whose
	// size is the sum of all shards — otherwise each shard shows up as its own
	// option with only a fraction of the real size and wrong RAM estimate.
	type quantGroup struct {
		key   string
		files []FileInfo
	}
	var order []string
	groups := make(map[string]*quantGroup)

	for _, f := range llmFiles {
		quantType := ggufQuantType(f)

		// Grouping key: shards of the same quant share a key. When the quant type
		// is recognized, group by it (all shards of Q4_K_M merge). When it is not,
		// group by the filename with its shard suffix removed, so multi-shard
		// "unknown" files still merge while genuinely different files stay apart.
		var key string
		if quantType != "" {
			key = quantType
		} else {
			key = "unknown:" + strings.ToLower(stripSplitSuffix(filepath.Base(f.Name)))
		}

		g := groups[key]
		if g == nil {
			g = &quantGroup{key: key}
			groups[key] = g
			order = append(order, key)
		}
		g.files = append(g.files, f)
	}

	for _, key := range order {
		if q := quantizationFromGroup(groups[key].files); q != nil {
			info.Quantizations = append(info.Quantizations, *q)
		}
	}

	// Sort with the least-compressed / largest variants first so the list reads
	// from full precision and heavier quants down toward smaller files.
	sort.Slice(info.Quantizations, func(i, j int) bool {
		if info.Quantizations[i].File.Size != info.Quantizations[j].File.Size {
			return info.Quantizations[i].File.Size > info.Quantizations[j].File.Size
		}
		return info.Quantizations[i].Name < info.Quantizations[j].Name
	})

	return info
}

// parseGGUFQuantization extracts quantization info from a single GGUF file.
// It is a thin wrapper over quantizationFromGroup for the single-file case.
func parseGGUFQuantization(f FileInfo) *GGUFQuantization {
	return quantizationFromGroup([]FileInfo{f})
}

// quantizationFromGroup builds a single GGUFQuantization from one or more files
// that belong to the same quantization. For split GGUFs the group holds every
// shard; sizes are summed and RAM is estimated from the combined total. The
// representative File keeps the (first shard's) metadata but carries the
// aggregated size so existing consumers see the real footprint.
func quantizationFromGroup(files []FileInfo) *GGUFQuantization {
	if len(files) == 0 {
		return nil
	}

	// Sort shards by name so part 00001 comes first and Files[] is ordered.
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	var totalSize int64
	paths := make([]string, 0, len(files))
	for _, f := range files {
		totalSize += f.Size
		paths = append(paths, f.Path)
	}

	// Determine quant type from any file's name/path. Split repos sometimes put
	// the token in the containing directory rather than every shard filename.
	quantType := ""
	for _, f := range files {
		quantType = ggufQuantType(f)
		if quantType != "" {
			break
		}
	}

	rep := files[0]
	rep.Size = totalSize
	rep.SizeHuman = humanSize(totalSize)

	ram := estimateRAM(totalSize)

	if quantType == "" {
		// No recognized quantization (split file or unknown format).
		return &GGUFQuantization{
			Name:              "Unknown",
			File:              rep,
			Quality:           3,
			QualityStars:      qualityToStars(3),
			EstimatedRAM:      ram,
			EstimatedRAMHuman: humanSize(ram),
			Description:       "Unknown quantization format",
			Parts:             len(files),
			Files:             paths,
		}
	}

	baseQuantType := strings.TrimPrefix(quantType, "UD_")
	quality := quantQuality[baseQuantType]
	if quality == 0 {
		quality = 3 // Default to medium if not found
	}

	desc := quantDescriptions[baseQuantType]
	if desc == "" {
		desc = "Quantized model"
	}

	return &GGUFQuantization{
		Name:              quantType,
		File:              rep,
		Quality:           quality,
		QualityStars:      qualityToStars(quality),
		EstimatedRAM:      ram,
		EstimatedRAMHuman: humanSize(ram),
		Description:       desc,
		Parts:             len(files),
		Files:             paths,
	}
}

// qualityToStars converts a 1-5 quality rating to star representation.
func qualityToStars(quality int) string {
	filled := quality
	empty := 5 - quality
	return strings.Repeat("★", filled) + strings.Repeat("☆", empty)
}

// estimateRAM estimates RAM usage for a GGUF file.
// Formula: file_size * 1.1 + 500MB overhead
func estimateRAM(fileSize int64) int64 {
	const overhead = 500 * 1024 * 1024 // 500 MiB
	return int64(float64(fileSize)*1.1) + overhead
}

// RecommendGGUF recommends quantizations based on available RAM.
func RecommendGGUF(info *GGUFInfo, availableRAM int64) []GGUFQuantization {
	var recommended []GGUFQuantization

	for _, q := range info.Quantizations {
		if q.EstimatedRAM <= availableRAM {
			q.Recommended = true
			recommended = append(recommended, q)
		}
	}

	// Sort by quality (best that fits in RAM first)
	sort.Slice(recommended, func(i, j int) bool {
		if recommended[i].Quality != recommended[j].Quality {
			return recommended[i].Quality > recommended[j].Quality
		}
		return recommended[i].File.Size < recommended[j].File.Size
	})

	return recommended
}

// preferredMMProj picks the preferred multimodal projector file from a set.
// Preference order by precision: F16 > BF16 > F32 > first file. Returns the
// chosen FileInfo and a narrow filter string (lowercased basename without
// .gguf extension) that matches only that file under the downloader's
// case-insensitive substring filter semantics.
func preferredMMProj(files []FileInfo) (FileInfo, string) {
	precedence := []string{"-f16", "-bf16", "-f32"}
	for _, p := range precedence {
		for i := range files {
			name := strings.ToLower(filepath.Base(files[i].Name))
			if strings.Contains(name, p) {
				return files[i], strings.TrimSuffix(name, ".gguf")
			}
		}
	}
	// Fallback: pick the first file and build a filter from its full basename
	// so the filter still matches exactly one file.
	name := strings.ToLower(filepath.Base(files[0].Name))
	return files[0], strings.TrimSuffix(name, ".gguf")
}

// GGUFToSelectableItems converts GGUF quantizations to SelectableItems.
// This provides a unified interface for the web UI.
//
// When the repo contains mmproj vision-encoder files (multimodal models),
// an additional SelectableItem with Category="vision_encoder" is emitted
// for the preferred mmproj file. It is Recommended=true by default so that
// the recommended download selection auto-bundles the vision encoder
// alongside any selected LLM quant.
func GGUFToSelectableItems(info *GGUFInfo) []SelectableItem {
	if info == nil {
		return nil
	}
	if len(info.Quantizations) == 0 && len(info.MMProjFiles) == 0 {
		return nil
	}

	items := make([]SelectableItem, 0, len(info.Quantizations)+1)

	// Track if we have a Q4_K_M (common recommended default)
	hasQ4KM := false
	for _, q := range info.Quantizations {
		if q.Name == "Q4_K_M" || q.Name == "UD_Q4_K_M" {
			hasQ4KM = true
			break
		}
	}

	for _, q := range info.Quantizations {
		// Determine if this should be recommended
		// Q4_K_M is a good default, otherwise highest quality in 4-bit range
		recommended := false
		if hasQ4KM && (q.Name == "Q4_K_M" || q.Name == "UD_Q4_K_M") {
			recommended = true
		} else if !hasQ4KM && q.Quality >= 4 && q.File.Size < 10*1024*1024*1024 { // < 10 GiB
			recommended = true
		}

		// Files to download for this quant: all shards when split, else the one
		// file. Fall back to the representative path if Files wasn't populated.
		files := q.Files
		if len(files) == 0 {
			files = []string{q.File.Path}
		}

		// Filter value used by the downloader's substring match. For recognized
		// quants the quant token appears in every shard name, so the lowercased
		// name matches the whole set. For "Unknown" groups the name won't match
		// any filename, so use the shared base of the files instead.
		filterValue := strings.ToLower(strings.TrimPrefix(q.Name, "UD_"))
		if strings.HasPrefix(q.Name, "APEX_") {
			filterValue = strings.ToLower(strings.ReplaceAll(q.Name, "_", "-"))
		}
		if q.Name == "Unknown" {
			filterValue = strings.ToLower(stripSplitSuffix(filepath.Base(q.File.Name)))
		}

		// Annotate multi-part quants so the user sees they are sharded.
		desc := q.Description
		if q.Parts > 1 {
			desc = fmt.Sprintf("%s · %d files", desc, q.Parts)
		}

		item := SelectableItem{
			ID:           strings.ToLower(q.Name),
			Label:        q.Name,
			Description:  desc,
			Size:         q.File.Size,
			SizeHuman:    q.File.SizeHuman,
			Quality:      q.Quality,
			QualityStars: q.QualityStars,
			Recommended:  recommended,
			Category:     "quantization",
			FilterValue:  filterValue,
			Files:        files,
			RAM:          q.EstimatedRAM,
			RAMHuman:     q.EstimatedRAMHuman,
		}
		items = append(items, item)
	}

	// Append a vision-encoder companion item for EVERY mmproj file present, so
	// the user can pick the precision they want (f16 / bf16 / f32 / …) instead
	// of being limited to one. The preferred file (preferredMMProj precedence)
	// is marked Recommended so the recommended selection still auto-bundles a
	// sensible mmproj alongside the chosen quant (github issue #76). Each item
	// gets a filter derived from its own filename so it matches exactly one
	// file via the comma-separated -F filter list.
	if len(info.MMProjFiles) > 0 {
		preferred, _ := preferredMMProj(info.MMProjFiles)
		// Emit the preferred file first (default/recommended pick), then the
		// rest, so the recommended mmproj appears at the top of the list.
		ordered := make([]FileInfo, 0, len(info.MMProjFiles))
		ordered = append(ordered, preferred)
		for _, f := range info.MMProjFiles {
			if f.Name != preferred.Name {
				ordered = append(ordered, f)
			}
		}

		// Build the description once; it is shared across all mmproj items.
		mmDesc := "Multimodal projector"
		if info.MMProjUpstreamRepo != "" {
			mmDesc += " · upstream: " + info.MMProjUpstreamRepo
		}

		for _, f := range ordered {
			base := filepath.Base(f.Name)
			filter := strings.ToLower(strings.TrimSuffix(base, ".gguf"))
			items = append(items, SelectableItem{
				ID:           "mmproj-" + filter,
				Label:        base,
				Description:  mmDesc,
				Size:         f.Size,
				SizeHuman:    f.SizeHuman,
				Recommended:  f.Name == preferred.Name,
				Category:     "vision_encoder",
				FilterValue:  filter,
				Files:        []string{f.Path},
				UpstreamRepo: info.MMProjUpstreamRepo,
			})
		}
	}

	return items
}
