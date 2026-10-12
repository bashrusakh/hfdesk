// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"fmt"
	"path"
	"strings"

	"github.com/bashrusakh/hfdesk/internal/datasetselection"
)

// datasetDecisionWorld is one complete concrete interpretation. SharedToken
// keys are normalized filter bits; true means membership interpretation and
// false means ordinary path interpretation. Bits are supplied only for
// compiled collisions. Production code does not enumerate these worlds.
type datasetDecisionWorld struct {
	DataLabels  map[int]string
	Companion   map[int]bool
	SharedToken map[uint]bool
}

type datasetDecisionRecord struct {
	Selected bool
	Skipped  bool
	Subdir   string
}

type datasetDecisionEvaluation struct {
	Records []datasetDecisionRecord
}

// evaluateDatasetDecisionWorld applies the exact-dataset-group concrete
// selection contract to one admissible role/token interpretation. It remains
// dormant: PlanRepo does not call it. No-filter, inexact, non-dataset, and
// GGUF requests are outside this evaluator's established contract.
func evaluateDatasetDecisionWorld(c datasetDecisionContext, world datasetDecisionWorld) (datasetDecisionEvaluation, error) {
	if !c.IsDataset || !c.ExactMatch || c.GGUFMode || len(c.Filters) == 0 {
		return datasetDecisionEvaluation{}, fmt.Errorf("concrete dataset evaluator requires an exact filtered non-GGUF dataset request")
	}
	recognizedLabels := make(map[string]bool, len(c.RecognizedLabels))
	for _, label := range c.RecognizedLabels {
		recognizedLabels[label] = true
	}
	groupSelected := false
	for _, f := range c.Filters {
		if recognizedLabels[f.Normalized] {
			groupSelected = true
			break
		}
	}
	if !groupSelected {
		return datasetDecisionEvaluation{}, fmt.Errorf("concrete dataset evaluator requires at least one recognized group filter")
	}
	if err := validateDatasetDecisionWorld(c, world); err != nil {
		return datasetDecisionEvaluation{}, err
	}
	collidingTokens := collisionTokenBits(c)
	out := datasetDecisionEvaluation{Records: make([]datasetDecisionRecord, len(c.Inputs))}
	dirs := make(map[string]bool)
	direct := make([]bool, len(c.Inputs))
	labels := make([]string, len(c.Inputs))
	companions := make([]bool, len(c.Inputs))
	for i, in := range c.Inputs {
		if in.Excluded {
			continue
		}
		labels[i] = strings.ToLower(world.DataLabels[i])
		companions[i] = world.Companion[i]
		for _, f := range c.Filters {
			if concreteDatasetFilterMatches(in, f, c, world, labels[i], companions[i], collidingTokens, recognizedLabels) {
				direct[i] = true
			}
		}
		if direct[i] && !companions[i] {
			for d := path.Dir(strings.ReplaceAll(in.Path, "\\", "/")); d != "." && d != "/"; d = path.Dir(d) {
				dirs[d] = true
			}
		}
	}
	seenExact, seenFold := make(map[string]bool), make(map[string]bool)
	for i, in := range c.Inputs {
		r := &out.Records[i]
		if seenExact[in.Path] {
			r.Skipped = true
			continue
		}
		seenExact[in.Path] = true
		if in.Excluded {
			r.Skipped = true
			continue
		}
		if seenFold[strings.ToLower(in.Path)] {
			r.Skipped = true
			continue
		}
		if !direct[i] {
			// Data never falls back to companion retention. Only an actual
			// companion role can enter the root/directory fallback, and LFS
			// companions are intentionally not retained by that fallback.
			if !companions[i] || in.LFS {
				r.Skipped = true
				continue
			}
			dir := path.Dir(strings.ReplaceAll(in.Path, "\\", "/"))
			if dir != "." && !dirs[dir] {
				r.Skipped = true
				continue
			}
		}
		// Attribution considers every applicable direct term, preserves input
		// order for equal lengths, and never turns attribution ambiguity into a
		// selection error.
		bestLength := -1
		for _, f := range c.Filters {
			if concreteDatasetFilterMatches(in, f, c, world, labels[i], companions[i], collidingTokens, recognizedLabels) && len(f.Original) > bestLength {
				r.Subdir, bestLength = f.Original, len(f.Original)
			}
		}
		r.Selected = true
		seenFold[strings.ToLower(in.Path)] = true
	}
	return out, nil
}

func concreteDatasetFilterMatches(in datasetDecisionInput, f datasetDecisionFilter, c datasetDecisionContext, world datasetDecisionWorld, label string, companion bool, collidingTokens map[uint]bool, recognizedLabels map[string]bool) bool {
	if companion {
		// Ordinary path selection remains available to companions, including
		// recognized group-token matches in either shared interpretation.
		return filterMatchesPath(in.Path, f.Original, c.ExactMatch)
	}
	if recognizedLabels[f.Normalized] {
		if collidingTokens[f.Bit] && !world.SharedToken[f.Bit] {
			return filterMatchesPath(in.Path, f.Original, c.ExactMatch)
		}
		return label != "" && label == f.Normalized
	}
	return filterMatchesPath(in.Path, f.Original, c.ExactMatch)
}

func validateDatasetDecisionWorld(c datasetDecisionContext, world datasetDecisionWorld) error {
	for i := range world.DataLabels {
		if i < 0 || i >= len(c.Inputs) {
			return fmt.Errorf("data role refers to unknown input %d", i)
		}
	}
	for i := range world.Companion {
		if i < 0 || i >= len(c.Inputs) {
			return fmt.Errorf("companion role refers to unknown input %d", i)
		}
	}
	for i, in := range c.Inputs {
		label, data := world.DataLabels[i]
		companion := world.Companion[i]
		if data == companion { // both false or both true
			return fmt.Errorf("input %d must have exactly one concrete role", i)
		}
		valid := false
		for _, alt := range in.Alternatives {
			if data && alt.Role == datasetselection.EvidenceRoleData && strings.EqualFold(alt.Label, label) || companion && alt.Role == datasetselection.EvidenceRoleCompanion {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("input %d role/label is not an actual membership alternative", i)
		}
	}
	collidingTokens := collisionTokenBits(c)
	for bit := range world.SharedToken {
		if !collidingTokens[bit] {
			return fmt.Errorf("shared-token interpretation supplied for non-colliding bit %d", bit)
		}
	}
	for _, f := range c.Filters {
		if !collidingTokens[f.Bit] {
			continue
		}
		if _, ok := world.SharedToken[f.Bit]; !ok {
			return fmt.Errorf("missing shared-token interpretation for filter %q", f.Normalized)
		}
	}
	return nil
}

func collisionTokenBits(c datasetDecisionContext) map[uint]bool {
	bits := make(map[uint]bool)
	filterBits := make(map[int]uint, len(c.Filters))
	for _, f := range c.Filters {
		filterBits[f.Index] = f.Bit
	}
	for _, in := range c.Inputs {
		if in.Excluded {
			continue
		}
		for _, index := range in.CollisionFilterIndexes {
			if bit, ok := filterBits[index]; ok {
				bits[bit] = true
			}
		}
	}
	return bits
}

// datasetDecisionWorldFromAlternatives is a strict test/construction helper:
// singleton evidence is filled automatically, but ambiguous inputs require an
// explicit actual alternative and every shared collision bit is required.
func datasetDecisionWorldFromAlternatives(c datasetDecisionContext, choices map[int]datasetDecisionRoleLabel, shared map[uint]bool) (datasetDecisionWorld, error) {
	w := datasetDecisionWorld{DataLabels: make(map[int]string), Companion: make(map[int]bool), SharedToken: make(map[uint]bool)}
	for i := range choices {
		if i < 0 || i >= len(c.Inputs) {
			return datasetDecisionWorld{}, fmt.Errorf("role choice refers to unknown input %d", i)
		}
	}
	for bit, value := range shared {
		w.SharedToken[bit] = value
	}
	for i, in := range c.Inputs {
		choice, ok := choices[i]
		if !ok && len(in.Alternatives) == 1 {
			choice, ok = in.Alternatives[0], true
		}
		if !ok {
			return datasetDecisionWorld{}, fmt.Errorf("input %d has ambiguous evidence and no explicit choice", i)
		}
		if choice.Role == datasetselection.EvidenceRoleCompanion {
			w.Companion[i] = true
		} else if choice.Role == datasetselection.EvidenceRoleData {
			w.DataLabels[i] = strings.ToLower(choice.Label)
		} else {
			return datasetDecisionWorld{}, fmt.Errorf("input %d has unsupported role %q", i, choice.Role)
		}
	}
	return w, validateDatasetDecisionWorld(c, w)
}
