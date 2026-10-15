// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

// Package datasetselection recognizes repository files that belong to dataset
// groups. It intentionally does not infer membership from an ambiguous file
// extension alone.
package datasetselection

import (
	"path"
	"sort"
	"strings"
)

// File describes the repository-relative path needed for membership analysis.
type File struct {
	Path string
}

// Result contains indexes into the input files. Groups are keyed by the same
// labels used by smartdl; Unresolved files are known data-shaped files whose
// dataset role cannot be established from their repository path.
type Result struct {
	Groups     map[string][]int
	Unresolved []int
	// Evidence is an input-ordered snapshot; callers should treat its slices
	// as immutable and must not use them as mutable resolver state.
	Evidence []Evidence
}

// Evidence records finite membership alternatives without changing the
// legacy Groups/Unresolved projection. PossibleLabels contains only labels
// justified by the recognized path, not every known group.
type Evidence struct {
	InputIndex     int
	PossibleLabels []string
	DefiniteLabels []string
	PossibleRoles  []EvidenceRole
	DefiniteRole   EvidenceRole
	Reason         EvidenceReason
}

// EvidenceRole distinguishes dataset membership from a non-dataset companion.
// Labels are only dataset group names; roles can never collide with labels.
type EvidenceRole string

const (
	EvidenceRoleData      EvidenceRole = "data"
	EvidenceRoleCompanion EvidenceRole = "ordinary-companion"
)

// EvidenceReason explains why membership is definite or remains ambiguous.
type EvidenceReason string

const (
	EvidenceConventional      EvidenceReason = "conventional-group"
	EvidenceCustom            EvidenceReason = "custom-data-group"
	EvidenceConflicting       EvidenceReason = "conflicting-conventional-groups"
	EvidenceUnnamedDefault    EvidenceReason = "unnamed-default-data"
	EvidenceUnnamedAmbiguous  EvidenceReason = "unnamed-data-or-companion"
	EvidenceRootMetadata      EvidenceReason = "root-metadata-companion"
	EvidenceOrdinaryCompanion EvidenceReason = "ordinary-companion"
)

var standardGroups = []string{"train", "test", "validation", "dev", "eval"}

// Recognize classifies data files using conventional split names and
// data/<custom-group>/ directory membership. Order and duplicate paths are
// retained as input indexes; callers that deduplicate inventory should do so
// before calling this function.
func Recognize(files []File) Result {
	r := Result{Groups: make(map[string][]int)}
	for i, file := range files {
		p := strings.TrimPrefix(path.Clean(strings.ReplaceAll(file.Path, "\\", "/")), "./")
		lower := strings.ToLower(p)
		base := path.Base(lower)
		ext := strings.TrimPrefix(path.Ext(base), ".")
		if strings.HasSuffix(base, ".tar.gz") {
			ext = "tar.gz"
		}
		if !dataExtension[ext] || isServiceMetadata(p) {
			if isServiceMetadata(p) {
				r.Evidence = append(r.Evidence, Evidence{InputIndex: i, PossibleRoles: []EvidenceRole{EvidenceRoleCompanion}, Reason: EvidenceRootMetadata})
			} else if !dataExtension[ext] {
				r.Evidence = append(r.Evidence, Evidence{InputIndex: i, PossibleRoles: []EvidenceRole{EvidenceRoleCompanion}, Reason: EvidenceOrdinaryCompanion})
			}
			continue
		}

		groups := pathGroups(lower, base)
		if len(groups) > 1 {
			r.Unresolved = append(r.Unresolved, i)
			sort.Strings(groups)
			r.Evidence = append(r.Evidence, Evidence{InputIndex: i, PossibleLabels: groups, PossibleRoles: []EvidenceRole{EvidenceRoleData}, Reason: EvidenceConflicting})
			continue
		}
		if len(groups) == 1 {
			r.Groups[groups[0]] = append(r.Groups[groups[0]], i)
			reason := EvidenceConventional
			if !isStandardGroup(groups[0]) {
				reason = EvidenceCustom
			}
			r.Evidence = append(r.Evidence, Evidence{InputIndex: i, PossibleLabels: []string{groups[0]}, DefiniteLabels: []string{groups[0]}, PossibleRoles: []EvidenceRole{EvidenceRoleData}, DefiniteRole: EvidenceRoleData, Reason: reason})
			continue
		}
		if ext == "json" || ext == "jsonl" || ext == "txt" {
			r.Unresolved = append(r.Unresolved, i)
			r.Evidence = append(r.Evidence, Evidence{InputIndex: i, PossibleLabels: []string{"default"}, PossibleRoles: []EvidenceRole{EvidenceRoleData, EvidenceRoleCompanion}, Reason: EvidenceUnnamedAmbiguous})
			continue
		}
		r.Groups["default"] = append(r.Groups["default"], i)
		r.Evidence = append(r.Evidence, Evidence{InputIndex: i, PossibleLabels: []string{"default"}, DefiniteLabels: []string{"default"}, PossibleRoles: []EvidenceRole{EvidenceRoleData}, DefiniteRole: EvidenceRoleData, Reason: EvidenceUnnamedDefault})
	}
	return r
}

func isStandardGroup(group string) bool {
	for _, standard := range standardGroups {
		if group == standard {
			return true
		}
	}
	return false
}

var dataExtension = map[string]bool{
	"parquet": true, "arrow": true, "json": true, "jsonl": true,
	"csv": true, "tsv": true, "txt": true, "tar": true,
	"tar.gz": true, "zip": true, "gz": true, "zst": true,
}

func isServiceMetadata(p string) bool {
	if strings.Contains(p, "/") {
		return false
	}
	switch strings.ToLower(p) {
	case "config.json", "dataset_info.json":
		return true
	default:
		return false
	}
}

func pathGroups(p, base string) []string {
	parts := strings.Split(p, "/")
	found := make(map[string]bool)
	for _, split := range standardGroups {
		for _, part := range parts[:len(parts)-1] {
			if part == split {
				found[split] = true
			}
		}
		if strings.HasPrefix(base, split+"-") || strings.HasPrefix(base, split+"_") || strings.HasPrefix(base, split+".") {
			found[split] = true
		}
	}
	if len(found) > 0 {
		groups := make([]string, 0, len(found))
		for group := range found {
			groups = append(groups, group)
		}
		return groups
	}
	// data/<group>/ is a directory convention. A leaf directly under data/
	// (for example data/part-0000.parquet) is an unnamed/default file, not a
	// custom group named after its filename.
	if len(parts) > 2 && parts[0] == "data" && parts[1] != "" {
		return []string{parts[1]}
	}
	return nil
}
