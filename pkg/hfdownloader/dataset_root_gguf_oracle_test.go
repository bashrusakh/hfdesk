// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/datasetselection"
)

// TestRejectedRootGGUFInNonuniqueWorlds checks the fixed GGUF path guard
// against independently enumerated role/token worlds. The small oracle uses
// the existing concrete direct-match rules, then applies the established
// fixed-GGUF path requirement before ordered duplicate suppression.
func TestRejectedRootGGUFInNonuniqueWorlds(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "model-test.gguf"},
		{Path: "train/test/ambiguous.json"},
		{Path: "model-test.GGUF"},
	}, []string{"train"}, nil)
	if !c.IsDataset || c.GGUFMode || len(c.Inputs[0].Alternatives) < 1 {
		t.Fatalf("unexpected compiled fixture: dataset=%t GGUF=%t inputs=%+v", c.IsDataset, c.GGUFMode, c.Inputs)
	}
	if len(c.Inputs[1].Alternatives) < 2 {
		t.Fatalf("fixture lacks genuine role alternatives: %+v", c.Inputs[1].Alternatives)
	}
	if !strings.HasSuffix(strings.ToLower(c.Inputs[0].Path), ".gguf") || oracleRootGGUFAllowed(c, c.Inputs[0].Path) {
		t.Fatalf("root GGUF is not an unmatched guard case: path=%q filters=%+v", c.Inputs[0].Path, c.Filters)
	}

	worlds := enumerateRootGGUFEmittedSets(c)
	if len(worlds) == 0 {
		t.Fatal("fixture produced no admissible concrete worlds")
	}
	can := make([]bool, len(c.Inputs))
	cannot := make([]bool, len(c.Inputs))
	for wi, emitted := range worlds {
		for i, yes := range emitted {
			can[i] = can[i] || yes
			cannot[i] = cannot[i] || !yes
		}
		if emitted[0] {
			t.Fatalf("root GGUF emitted in oracle world %d", wi)
		}
	}
	if can[0] {
		t.Fatal("oracle permits the rejected root GGUF to emit")
	}

	uncached := c
	uncached.uniqueEvaluation = nil
	for _, tc := range []struct {
		name string
		c    datasetDecisionContext
	}{
		{"cached", c},
		{"uncached", uncached},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.c.Inputs {
				wantEmit, wantCannot := can[i], cannot[i]
				emit := canEmitDatasetDecision(tc.c, i)
				if emit.Possible != wantEmit || emit.Err != nil {
					t.Errorf("index %d can-emit=%+v, oracle possible=%t", i, emit, wantEmit)
				} else if emit.Possible && !oracleRootGGUFWorldEmits(tc.c, emit.World, i) {
					t.Errorf("index %d can-emit witness does not emit under oracle: %+v", i, emit.World)
				}
				cannot := canNotEmitDatasetDecision(tc.c, i)
				if cannot.Possible != wantCannot || cannot.Err != nil {
					t.Errorf("index %d cannot-emit=%+v, oracle possible=%t", i, cannot, wantCannot)
				} else if cannot.Possible && oracleRootGGUFWorldEmits(tc.c, cannot.World, i) {
					t.Errorf("index %d cannot-emit witness still emits under oracle: %+v", i, cannot.World)
				}
			}
		})
	}
	t.Logf("independent emitted-set oracle checked %d concrete role/token worlds and %d query targets (cached and uncached)", len(worlds), len(c.Inputs))
}

func enumerateRootGGUFEmittedSets(c datasetDecisionContext) [][]bool {
	collisions := oracleCollisionBits(c)
	var bits []uint
	for bit := range collisions {
		bits = append(bits, bit)
	}
	var worlds [][]bool
	for mask := 0; mask < 1<<len(bits); mask++ {
		w := datasetDecisionWorld{DataLabels: map[int]string{}, Companion: map[int]bool{}, SharedToken: map[uint]bool{}}
		for j, bit := range bits {
			w.SharedToken[bit] = mask&(1<<j) != 0
		}
		var visit func(int)
		visit = func(i int) {
			if i == len(c.Inputs) {
				if !oracleWorldValid(c, w, collisions) {
					return
				}
				eligible := concreteOracleEligible(c, w, collisions)
				// The fixed guard constrains eligibility, not raw direct matching.
				for j, in := range c.Inputs {
					if strings.HasSuffix(strings.ToLower(in.Path), ".gguf") && !oracleRootGGUFAllowed(c, in.Path) {
						eligible[j] = false
					}
				}
				emitted := make([]bool, len(c.Inputs))
				seenExact, seenFold := map[string]bool{}, map[string]bool{}
				for j, in := range c.Inputs {
					fold := strings.ToLower(in.Path)
					if seenExact[in.Path] {
						continue
					}
					seenExact[in.Path] = true
					if in.Excluded || seenFold[fold] || !eligible[j] {
						continue
					}
					emitted[j] = true
					seenFold[fold] = true
				}
				worlds = append(worlds, emitted)
				return
			}
			for _, alt := range c.Inputs[i].Alternatives {
				if alt.Role == datasetselection.EvidenceRoleCompanion {
					w.Companion[i] = true
				} else {
					w.DataLabels[i] = alt.Label
				}
				visit(i + 1)
				delete(w.Companion, i)
				delete(w.DataLabels, i)
			}
		}
		visit(0)
	}
	return worlds
}

func oracleRootGGUFWorldEmits(c datasetDecisionContext, w datasetDecisionWorld, index int) bool {
	collisions := oracleCollisionBits(c)
	if !oracleWorldValid(c, w, collisions) {
		return false
	}
	emitted := false
	eligible := concreteOracleEligible(c, w, collisions)
	for i, in := range c.Inputs {
		if strings.HasSuffix(strings.ToLower(in.Path), ".gguf") && !oracleRootGGUFAllowed(c, in.Path) {
			eligible[i] = false
		}
	}
	seenExact, seenFold := map[string]bool{}, map[string]bool{}
	for i, in := range c.Inputs {
		fold := strings.ToLower(in.Path)
		if seenExact[in.Path] {
			continue
		}
		seenExact[in.Path] = true
		if in.Excluded || seenFold[fold] || !eligible[i] {
			continue
		}
		if i == index {
			emitted = true
		}
		seenFold[fold] = true
	}
	return emitted
}

func oracleRootGGUFAllowed(c datasetDecisionContext, filePath string) bool {
	for _, filter := range c.Filters {
		if filterMatchesPath(filePath, filter.Original, c.ExactMatch) {
			return true
		}
	}
	return false
}
