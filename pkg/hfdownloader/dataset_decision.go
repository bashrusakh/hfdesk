// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/bashrusakh/hfdesk/internal/datasetselection"
)

// datasetCoreLabels is the finite conventional domain used by the later
// evaluator. Other evidence labels remain representable in residualLabels;
// this list is not a classifier or an advertised-group inventory.
var datasetCoreLabels = [...]string{"train", "test", "validation", "dev", "eval", "default"}

type datasetDecisionRoleLabel struct {
	Role  datasetselection.EvidenceRole
	Label string // empty only for companion alternatives
}

type datasetDecisionFilter struct {
	Index      int
	Original   string
	Normalized string
	Length     int
	Bit        uint
}

type datasetDecisionInput struct {
	Path string
	// Alternatives preserve the input's actual role/label possibilities.
	Alternatives                       []datasetDecisionRoleLabel
	PossibleCore, DefiniteCore         uint64
	PossibleResidual, DefiniteResidual []string
	Excluded                           bool
	// DirectFilterIndexes retain input filter order; a match uses the existing
	// complete-path matcher, not a second delimiter/token interpretation.
	DirectFilterIndexes              []int
	MembershipFilterIndexes          []int
	RecognizedTermFilterIndexes      []int
	RecognizedCompanionFilterIndexes []int
	CollisionFilterIndexes           []int
	DirectCoreConditions             uint64
	DirectResidualConditions         []string
	GroupCoreConditions              uint64
	GroupResidualConditions          []string
	Directory                        string
	// DirectSeedCandidate is an unexcluded inventory source that a direct
	// filter can select in at least one admissible membership interpretation.
	// A membership selection is also a directory seed even when its group
	// token is not an ordinary path match (for example, "train" selecting
	// train.csv).
	DirectSeedCandidate    bool
	ExactDuplicateOf       int
	CaseDuplicateOf        int
	ExactDuplicateEligible bool
	CaseDuplicateEligible  bool
	LFS                    bool
}

// datasetDecisionContext is an immutable, deterministic snapshot of inputs
// needed for finite dataset-selection reasoning.
type datasetDecisionContext struct {
	ctx                    context.Context
	Inputs                 []datasetDecisionInput
	Filters                []datasetDecisionFilter
	FilterBits             map[string]uint
	RecognizedLabels       []string
	CoreLabels             []string
	ResidualLabels         []string
	ExactMatch             bool
	IsDataset              bool
	GGUFMode               bool
	RetainMetadata         bool
	DirectSeedsByDirectory map[string][]int
	CaseIndexesByPath      map[string][]int
	uniqueEvaluation       *datasetDecisionUniqueEvaluation
}

// The compiled input is immutable. The mutex shares only fully completed proof
// and baseline caches across per-input queries.
type datasetDecisionUniqueEvaluation struct {
	mu        sync.Mutex
	proofDone bool
	unique    bool
	fixedDone bool
	fixedEmit []int8
	baseDone  bool
	world     datasetDecisionWorld
	records   []datasetDecisionRecord
	valid     bool
}

func compileDatasetDecisionContext(nodes []hfNode, membership datasetselection.Result, filters, excludes []string, exact, isDataset, ggufMode bool) datasetDecisionContext {
	return compileDatasetDecisionContextWithContext(context.Background(), nodes, membership, filters, excludes, exact, isDataset, ggufMode)
}

func compileDatasetDecisionContextWithContext(ctx context.Context, nodes []hfNode, membership datasetselection.Result, filters, excludes []string, exact, isDataset, ggufMode bool) datasetDecisionContext {
	if ctx == nil {
		ctx = context.Background()
	}
	c := datasetDecisionContext{ctx: ctx, ExactMatch: exact, IsDataset: isDataset, GGUFMode: ggufMode, FilterBits: make(map[string]uint)}
	coreBits := make(map[string]uint, len(datasetCoreLabels))
	for i, label := range datasetCoreLabels {
		coreBits[label] = uint(i)
		c.CoreLabels = append(c.CoreLabels, label)
	}

	// Labels are drawn solely from actual alternatives in this inventory.
	labelSet := make(map[string]struct{})
	evidence := make(map[int]datasetselection.Evidence, len(membership.Evidence))
	for _, e := range membership.Evidence {
		if ctx.Err() != nil {
			return c
		}
		evidence[e.InputIndex] = e
		for _, label := range e.PossibleLabels {
			if ctx.Err() != nil {
				return c
			}
			labelSet[strings.ToLower(label)] = struct{}{}
		}
		for _, label := range e.DefiniteLabels {
			if ctx.Err() != nil {
				return c
			}
			labelSet[strings.ToLower(label)] = struct{}{}
		}
	}
	for label := range membership.Groups {
		if ctx.Err() != nil {
			return c
		}
		labelSet[strings.ToLower(label)] = struct{}{}
	}
	for _, label := range stableSortedKeys(labelSet) {
		c.RecognizedLabels = append(c.RecognizedLabels, label)
	}
	residualBit := make(map[string]uint)
	recognizedSet := make(map[string]struct{}, len(c.RecognizedLabels))
	for _, label := range c.RecognizedLabels {
		recognizedSet[label] = struct{}{}
		if _, core := coreBits[label]; !core {
			residualBit[label] = uint(len(residualBit))
			c.ResidualLabels = append(c.ResidualLabels, label)
		}
	}

	for i, original := range filters {
		if ctx.Err() != nil {
			return c
		}
		norm := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(original), "\\", "/"))
		bit, ok := c.FilterBits[norm]
		if !ok {
			bit = uint(len(c.FilterBits))
			c.FilterBits[norm] = bit
		}
		c.Filters = append(c.Filters, datasetDecisionFilter{Index: i, Original: original, Normalized: norm, Length: len(original), Bit: bit})
	}

	c.Inputs = make([]datasetDecisionInput, len(nodes))
	firstExact, firstCase := map[string]int{}, map[string]int{}
	for i, n := range nodes {
		if ctx.Err() != nil {
			return c
		}
		in := datasetDecisionInput{Path: n.Path, LFS: n.LFS != nil, ExactDuplicateOf: -1, CaseDuplicateOf: -1}
		e, hasEvidence := evidence[i]
		if hasEvidence {
			for _, role := range e.PossibleRoles {
				if role == datasetselection.EvidenceRoleCompanion {
					in.Alternatives = append(in.Alternatives, datasetDecisionRoleLabel{Role: role})
					continue
				}
				for _, label := range e.PossibleLabels {
					in.Alternatives = append(in.Alternatives, datasetDecisionRoleLabel{Role: role, Label: strings.ToLower(label)})
				}
			}
		} else {
			// Preserve usable legacy Result values if evidence is absent.
			groupNames := make([]string, 0, len(membership.Groups))
			for label := range membership.Groups {
				groupNames = append(groupNames, label)
			}
			sort.Strings(groupNames)
			for _, label := range groupNames {
				indexes := membership.Groups[label]
				for _, index := range indexes {
					if index == i {
						in.Alternatives = append(in.Alternatives, datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleData, Label: strings.ToLower(label)})
					}
				}
			}
			if len(in.Alternatives) == 0 {
				in.Alternatives = []datasetDecisionRoleLabel{{Role: datasetselection.EvidenceRoleCompanion}}
			}
		}
		for _, alt := range in.Alternatives {
			if bit, ok := coreBits[alt.Label]; ok && alt.Role == datasetselection.EvidenceRoleData {
				in.PossibleCore |= 1 << bit
			}
			if _, ok := residualBit[alt.Label]; ok && alt.Role == datasetselection.EvidenceRoleData {
				in.PossibleResidual = appendUnique(in.PossibleResidual, alt.Label)
			}
		}
		if hasEvidence && e.DefiniteRole == datasetselection.EvidenceRoleData {
			for _, label := range e.DefiniteLabels {
				label = strings.ToLower(label)
				if bit, ok := coreBits[label]; ok {
					in.DefiniteCore |= 1 << bit
				} else {
					in.DefiniteResidual = appendUnique(in.DefiniteResidual, label)
				}
			}
		}
		in.Excluded = isExcludedPath(n.Path, excludes)
		for _, f := range c.Filters {
			if ctx.Err() != nil {
				return c
			}
			if filterMatchesPath(n.Path, f.Original, exact) {
				in.DirectFilterIndexes = append(in.DirectFilterIndexes, f.Index)
				if bit, ok := coreBits[f.Normalized]; ok {
					in.DirectCoreConditions |= 1 << bit
				}
				if _, ok := residualBit[f.Normalized]; ok {
					in.DirectResidualConditions = appendUnique(in.DirectResidualConditions, f.Normalized)
				}
			}
			if _, recognized := recognizedSet[f.Normalized]; recognized {
				if bit, ok := coreBits[f.Normalized]; ok && in.PossibleCore&(1<<bit) != 0 {
					in.MembershipFilterIndexes = append(in.MembershipFilterIndexes, f.Index)
				}
				if _, ok := residualBit[f.Normalized]; ok && containsString(in.PossibleResidual, f.Normalized) {
					in.MembershipFilterIndexes = append(in.MembershipFilterIndexes, f.Index)
				}
				if bit, ok := coreBits[f.Normalized]; ok && in.PossibleCore&(1<<bit) != 0 {
					in.GroupCoreConditions |= 1 << bit
				}
				if _, ok := residualBit[f.Normalized]; ok && containsString(in.PossibleResidual, f.Normalized) {
					in.GroupResidualConditions = appendUnique(in.GroupResidualConditions, f.Normalized)
				}
				if filterMatchesPath(n.Path, f.Original, exact) {
					in.RecognizedTermFilterIndexes = append(in.RecognizedTermFilterIndexes, f.Index)
					companion, collision := false, false
					for _, alt := range in.Alternatives {
						if alt.Role == datasetselection.EvidenceRoleCompanion {
							companion = true
						}
						if alt.Role == datasetselection.EvidenceRoleData && alt.Label != f.Normalized {
							collision = true
						}
					}
					if companion {
						in.RecognizedCompanionFilterIndexes = append(in.RecognizedCompanionFilterIndexes, f.Index)
					}
					if collision {
						in.CollisionFilterIndexes = append(in.CollisionFilterIndexes, f.Index)
					}
				}
			}
		}
		key := n.Path
		if prior, ok := firstExact[key]; ok {
			in.ExactDuplicateOf = prior
		} else {
			firstExact[key] = i
			in.ExactDuplicateEligible = true
		}
		lower := strings.ToLower(key)
		if prior, ok := firstCase[lower]; ok {
			in.CaseDuplicateOf = prior
		} else {
			firstCase[lower] = i
			in.CaseDuplicateEligible = true
		}
		c.Inputs[i] = in
	}
	for i := range c.Inputs {
		if ctx.Err() != nil {
			return c
		}
		in := &c.Inputs[i]
		hasPossibleDataRole := in.PossibleCore != 0 || len(in.PossibleResidual) != 0
		// Any ordinary explicit path match is a direct selection under every
		// actual role, including companion interpretations. Membership-only
		// selection remains limited to actual data alternatives.
		in.DirectSeedCandidate = !in.Excluded && (len(in.DirectFilterIndexes) != 0 || hasPossibleDataRole && (in.GroupCoreConditions != 0 || len(in.GroupResidualConditions) != 0))
	}
	// Index actual, non-excluded direct matches by containing directory. Keep
	// one shared index rather than copying each seed list onto every file.
	c.DirectSeedsByDirectory = make(map[string][]int)
	c.CaseIndexesByPath = make(map[string][]int)
	for i, input := range c.Inputs {
		if ctx.Err() != nil {
			return c
		}
		c.Inputs[i].Directory = path.Dir(strings.ReplaceAll(input.Path, "\\", "/"))
		fold := strings.ToLower(input.Path)
		c.CaseIndexesByPath[fold] = append(c.CaseIndexesByPath[fold], i)
		if !input.DirectSeedCandidate {
			continue
		}
		c.DirectSeedsByDirectory[c.Inputs[i].Directory] = append(c.DirectSeedsByDirectory[c.Inputs[i].Directory], i)
	}
	c.uniqueEvaluation = &datasetDecisionUniqueEvaluation{}
	return c
}

func stableSortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sortStrings(keys)
	return keys
}
func sortStrings(values []string) { sort.Strings(values) }
func appendUnique(values []string, value string) []string {
	for _, v := range values {
		if v == value {
			return values
		}
	}
	return append(values, value)
}
func containsString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
