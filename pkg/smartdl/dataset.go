// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bashrusakh/hfdesk/internal/datasetselection"
)

// Known dataset file formats and their descriptions.
var formatDescriptions = map[string]string{
	"parquet": "Apache Parquet columnar format (recommended)",
	"arrow":   "Apache Arrow IPC format",
	"json":    "JSON Lines format",
	"jsonl":   "JSON Lines format",
	"csv":     "Comma-separated values",
	"txt":     "Plain text",
	"tar":     "WebDataset tar archives",
	"tar.gz":  "Compressed WebDataset archives",
	"zip":     "Compressed archive",
}

// Standard split names.
var standardSplits = []string{"train", "test", "validation", "dev", "eval"}

// analyzeDataset analyzes a dataset repository.
func analyzeDataset(files []FileInfo) *DatasetInfo {
	info := &DatasetInfo{}
	inputs := make([]datasetselection.File, len(files))
	for i, f := range files {
		inputs[i] = datasetselection.File{Path: f.Path}
	}
	membership := datasetselection.Recognize(inputs)
	fileGroups := make(map[int]string)
	for group, indexes := range membership.Groups {
		for _, i := range indexes {
			fileGroups[i] = group
		}
	}

	// Collect splits and formats
	splitMap := make(map[string]*DatasetSplit)
	formatSet := make(map[string]bool)
	configSet := make(map[string]bool)
	for _, i := range membership.Unresolved {
		ext := datasetFileExtension(files[i].Name)
		if ext != "" {
			formatSet[ext] = true
		}
	}

	for i, f := range files {
		split, member := fileGroups[i]
		if !member {
			continue
		}
		ext := datasetFileExtension(f.Name)
		if ext == "" {
			continue
		}

		// Check if this is a data file
		if !isDataFileExtension(ext) {
			continue
		}

		formatSet[ext] = true

		// Detect config/subset from path
		config := detectConfig(f.Path)
		if config != "" {
			configSet[config] = true
		}

		// Add to split
		if _, exists := splitMap[split]; !exists {
			splitMap[split] = &DatasetSplit{
				Name: split,
			}
		}
		splitMap[split].Files = append(splitMap[split].Files, f)
		splitMap[split].Size += f.Size
	}

	// Convert splits map to slice
	for _, split := range splitMap {
		split.SizeHuman = humanSize(split.Size)
		split.FileCount = len(split.Files)
		info.Splits = append(info.Splits, *split)
	}

	// Sort splits by standard order
	sort.Slice(info.Splits, func(i, j int) bool {
		return splitPriority(info.Splits[i].Name) < splitPriority(info.Splits[j].Name)
	})

	// Collect formats
	for format := range formatSet {
		info.Formats = append(info.Formats, format)
	}
	sort.Strings(info.Formats)

	// Collect configs
	for config := range configSet {
		info.Configs = append(info.Configs, config)
	}
	sort.Strings(info.Configs)

	// Set primary format (prefer parquet > arrow > json)
	info.PrimaryFormat = selectPrimaryFormat(info.Formats)

	return info
}

func datasetFileExtension(name string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	if strings.HasSuffix(strings.ToLower(name), ".tar.gz") {
		return "tar.gz"
	}
	return ext
}

// isDataFileExtension checks if the extension indicates a data file.
func isDataFileExtension(ext string) bool {
	dataExts := map[string]bool{
		"parquet": true,
		"arrow":   true,
		"json":    true,
		"jsonl":   true,
		"csv":     true,
		"tsv":     true,
		"txt":     true,
		"tar":     true,
		"tar.gz":  true,
		"zip":     true,
		"gz":      true,
		"zst":     true,
	}
	return dataExts[ext]
}

// detectSplit extracts split name from file path or name.
func detectSplit(path, name string) string {
	pathLower := strings.ToLower(path)
	nameLower := strings.ToLower(name)

	// Check for standard splits in path
	for _, split := range standardSplits {
		if strings.Contains(pathLower, "/"+split+"/") ||
			strings.HasPrefix(pathLower, split+"/") ||
			strings.Contains(nameLower, split+"-") ||
			strings.Contains(nameLower, split+"_") ||
			strings.HasPrefix(nameLower, split+".") ||
			strings.HasPrefix(nameLower, split+"-") {
			return split
		}
	}

	// Check for data/ prefix (common pattern)
	if strings.HasPrefix(pathLower, "data/") {
		parts := strings.Split(path, "/")
		if len(parts) >= 2 {
			// Check if second part is a split name
			candidate := strings.ToLower(parts[1])
			for _, split := range standardSplits {
				if candidate == split {
					return split
				}
			}
			// Return as config/split hybrid
			return candidate
		}
	}

	return ""
}

// detectConfig extracts configuration/subset name from path.
func detectConfig(path string) string {
	// Common patterns: data/<config>/<split>/ or <config>/train/
	parts := strings.Split(path, "/")

	if len(parts) >= 2 {
		first := parts[0]

		// Skip common non-config prefixes
		if first == "data" && len(parts) >= 3 {
			candidate := parts[1]
			// Check if it's not a standard split
			for _, split := range standardSplits {
				if strings.ToLower(candidate) == split {
					return ""
				}
			}
			return candidate
		}

		// Check if first part is not a standard split and not "data"
		if first != "data" {
			for _, split := range standardSplits {
				if strings.ToLower(first) == split {
					return ""
				}
			}
			// Could be a config
			if len(parts) >= 2 {
				return first
			}
		}
	}

	return ""
}

// splitPriority returns ordering priority for splits.
func splitPriority(split string) int {
	priorities := map[string]int{
		"train":      0,
		"validation": 1,
		"dev":        2,
		"test":       3,
		"eval":       4,
		"default":    5,
	}
	if p, ok := priorities[split]; ok {
		return p
	}
	return 100 // Unknown splits go last
}

// selectPrimaryFormat selects the best format from available options.
func selectPrimaryFormat(formats []string) string {
	priorities := []string{"parquet", "arrow", "jsonl", "json", "csv", "txt"}

	for _, pref := range priorities {
		for _, fmt := range formats {
			if fmt == pref {
				return fmt
			}
		}
	}

	if len(formats) > 0 {
		return formats[0]
	}
	return ""
}

// GetSplitByName finds a split by name.
func GetSplitByName(info *DatasetInfo, name string) *DatasetSplit {
	for i := range info.Splits {
		if info.Splits[i].Name == name {
			return &info.Splits[i]
		}
	}
	return nil
}

// CalculateSelectedSize calculates total size for selected splits.
func CalculateSelectedSize(info *DatasetInfo, selectedSplits []string) int64 {
	if len(selectedSplits) == 0 {
		// No selection = all splits
		var total int64
		for _, split := range info.Splits {
			total += split.Size
		}
		return total
	}

	selected := make(map[string]bool)
	for _, s := range selectedSplits {
		selected[s] = true
	}

	var total int64
	for _, split := range info.Splits {
		if selected[split.Name] {
			total += split.Size
		}
	}
	return total
}

// GetFormatDescription returns description for a format.
func GetFormatDescription(format string) string {
	if desc, ok := formatDescriptions[format]; ok {
		return desc
	}
	return "Data format"
}

// HasMultipleConfigs checks if dataset has multiple configurations.
func HasMultipleConfigs(info *DatasetInfo) bool {
	return len(info.Configs) > 1
}

// HasMultipleFormats checks if dataset has multiple formats available.
func HasMultipleFormats(info *DatasetInfo) bool {
	return len(info.Formats) > 1
}

// datasetConfigGroup rolls a repository config's member files into one
// whole-config preset row: the config's folder selector, the splits its
// files belong to, and the summed size of those members.
type datasetConfigGroup struct {
	name   string
	folder string
	splits []string
	files  []string
	size   int64
	// train reports whether any member file belongs to the train split —
	// the split the established dataset recommendation logic marks Recommended.
	train bool
}

// configFolderSelector returns the directory selector for the config
// component detectConfig found in p: data/<config>/... selects through
// "data/<config>/", a root-level <config>/... through "<config>/". The
// trailing slash is the established exact-match directory-filter grammar,
// so the selector picks the config folder at a path-component boundary.
func configFolderSelector(p, config string) string {
	parts := strings.Split(p, "/")
	if len(parts) >= 3 && parts[0] == "data" && parts[1] == config {
		return "data/" + config + "/"
	}
	return config + "/"
}

// datasetConfigGroups groups every split member file by its detectConfig
// config and returns the groups in deterministic config-name order.
func datasetConfigGroups(info *DatasetInfo) []*datasetConfigGroup {
	byName := make(map[string]*datasetConfigGroup)
	var groups []*datasetConfigGroup
	for _, split := range info.Splits {
		for _, file := range split.Files {
			config := detectConfig(file.Path)
			if config == "" {
				continue
			}
			g := byName[config]
			if g == nil {
				g = &datasetConfigGroup{name: config, folder: configFolderSelector(file.Path, config)}
				byName[config] = g
				groups = append(groups, g)
			} else if folder := configFolderSelector(file.Path, config); folder != g.folder {
				// Members span both canonical config layouts; the bare
				// <config>/ selector still selects the folder at a component
				// boundary in either spelling.
				g.folder = config + "/"
			}
			if split.Name == "train" {
				g.train = true
			}
			present := false
			for _, existing := range g.splits {
				if existing == split.Name {
					present = true
					break
				}
			}
			if !present {
				g.splits = append(g.splits, split.Name)
			}
			g.files = append(g.files, file.Path)
			g.size += file.Size
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].name < groups[j].name })
	return groups
}

// label renders the preset row label, e.g. "cc100 (train+test)": the config
// name followed by its splits in the established split-priority order.
func (g *datasetConfigGroup) label() string {
	names := append([]string(nil), g.splits...)
	sort.Slice(names, func(i, j int) bool {
		pi, pj := splitPriority(names[i]), splitPriority(names[j])
		if pi != pj {
			return pi < pj
		}
		return names[i] < names[j]
	})
	return fmt.Sprintf("%s (%s)", g.name, strings.Join(names, "+"))
}

// DatasetToSelectableItems converts dataset configs and splits to
// SelectableItems. Config preset rows come first so the UI's
// "Dataset Configs" category renders above "Dataset Splits". Exactly one
// row can carry Recommended: when exactly one config holds the train
// split — the split the existing dataset recommendation logic marks
// Recommended — that config's preset takes the badge and every split row
// stays unbadged; with no unique winner (several train-bearing configs or
// none) the badge falls back to the existing split-level rule unchanged.
func DatasetToSelectableItems(info *DatasetInfo) []SelectableItem {
	if info == nil || len(info.Splits) == 0 {
		return nil
	}

	var items []SelectableItem

	configGroups := datasetConfigGroups(info)
	trainConfigs := 0
	for _, g := range configGroups {
		if g.train {
			trainConfigs++
		}
	}
	configRecommended := trainConfigs == 1
	for _, g := range configGroups {
		items = append(items, SelectableItem{
			ID:          g.folder,
			Label:       g.label(),
			Description: fmt.Sprintf("Dataset config (%d files)", len(g.files)),
			Size:        g.size,
			SizeHuman:   humanSize(g.size),
			Recommended: configRecommended && g.train,
			Category:    "config",
			FilterValue: g.folder,
			Files:       g.files,
		})
	}

	// Add splits
	splitDescriptions := map[string]string{
		"train":      "Primary training data",
		"validation": "Validation/evaluation set",
		"dev":        "Development set",
		"test":       "Held-out test set",
		"eval":       "Evaluation set",
		"default":    "Default dataset split",
	}

	for _, split := range info.Splits {
		desc := splitDescriptions[split.Name]
		if desc == "" {
			desc = "Dataset split"
		}

		// Add file count to description
		if split.FileCount > 0 {
			desc = fmt.Sprintf("%s (%d files)", desc, split.FileCount)
		}

		item := SelectableItem{
			ID:          split.Name,
			Label:       strings.Title(split.Name),
			Description: desc,
			Size:        split.Size,
			SizeHuman:   split.SizeHuman,
			Recommended: !configRecommended && split.Name == "train",
			Category:    "split",
			FilterValue: split.Name,
		}
		for _, file := range split.Files {
			item.Files = append(item.Files, file.Path)
		}
		items = append(items, item)
	}

	return items
}
