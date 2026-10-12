// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"fmt"
	"path"
	"reflect"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/datasetselection"
)

func compileDecisionFixture(nodes []hfNode, filters, excludes []string) datasetDecisionContext {
	files := make([]datasetselection.File, len(nodes))
	for i := range nodes {
		files[i] = datasetselection.File{Path: nodes[i].Path}
	}
	return compileDatasetDecisionContext(nodes, datasetselection.Recognize(files), filters, excludes, true, true, false)
}

func TestCompileDatasetDecisionContextPreservesActualRolesAndAlternatives(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "train.csv"}, {Path: "test/train.csv"}, {Path: "notes.txt"},
		{Path: "records.parquet"}, {Path: "config.json"}, {Path: "README.md"},
	}, nil, nil)
	checks := []struct {
		index int
		want  []datasetDecisionRoleLabel
	}{
		{0, []datasetDecisionRoleLabel{{datasetselection.EvidenceRoleData, "train"}}},
		{1, []datasetDecisionRoleLabel{{datasetselection.EvidenceRoleData, "test"}, {datasetselection.EvidenceRoleData, "train"}}},
		{2, []datasetDecisionRoleLabel{{datasetselection.EvidenceRoleData, "default"}, {datasetselection.EvidenceRoleCompanion, ""}}},
		{3, []datasetDecisionRoleLabel{{datasetselection.EvidenceRoleData, "default"}}},
		{4, []datasetDecisionRoleLabel{{datasetselection.EvidenceRoleCompanion, ""}}},
		{5, []datasetDecisionRoleLabel{{datasetselection.EvidenceRoleCompanion, ""}}},
	}
	for _, check := range checks {
		if got := c.Inputs[check.index].Alternatives; !reflect.DeepEqual(got, check.want) {
			t.Errorf("input %d alternatives=%+v want %+v", check.index, got, check.want)
		}
	}
	for _, label := range c.RecognizedLabels {
		if label == "unused-advertised" {
			t.Fatal("invented a group absent from actual evidence")
		}
	}
}

func TestFeasibleDatasetDecisionWorldHonorsPinsSkipsAndPositiveSeed(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "train.csv"}, {Path: "train/meta.json"}, {Path: "other/meta.json"}}, []string{"train"}, nil)
	train := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleData, Label: "train"}
	companion := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleCompanion}
	// A direct data file cannot be required skipped while the recognized group
	// filter selects it; an unseeded nested companion can satisfy the skip.
	if _, ok := feasibleAnyCoreDecisionWorld(c, datasetDecisionPins{0: train}, []int{0}, nil, nil); ok {
		t.Fatal("pinned direct data unexpectedly skipped")
	}
	if _, ok := feasibleAnyCoreDecisionWorld(c, datasetDecisionPins{2: companion}, []int{2}, nil, nil); !ok {
		t.Fatal("pinned companion should permit required skip")
	}
	// A positive clause can force a selected data seed, while a contradictory
	// zero-direct requirement must be rejected rather than returning a bad world.
	positive := &datasetDecisionPositive{Input: 0, Alternative: train}
	if _, ok := feasibleAnyCoreDecisionWorld(c, datasetDecisionPins{0: train}, nil, nil, positive); !ok {
		t.Fatal("expected coherent positive seed world")
	}
	if _, ok := feasibleAnyCoreDecisionWorld(c, datasetDecisionPins{0: train}, nil, []int{0}, positive); ok {
		t.Fatal("positive and zero-direct contradiction accepted")
	}
}

func TestFeasibleDatasetDecisionWorldAgainstIndependentSmallOracle(t *testing.T) {
	cases := []struct {
		name    string
		nodes   []hfNode
		filters []string
	}{
		{"different-groups-and-path-collisions", []hfNode{{Path: "train.csv"}, {Path: "test/train.csv"}, {Path: "test/meta.json"}}, []string{"train", "test"}},
		{"custom-group-and-shared-ordinary-token", []hfNode{{Path: "data/customdata/a.parquet"}, {Path: "data/customdata/b.parquet"}, {Path: "root.json"}, {Path: "data/customdata/README.md"}}, []string{"customdata", "a.parquet"}},
		{"default-companion-and-repeat-filter", []hfNode{{Path: "records.parquet"}, {Path: "notes.json"}, {Path: "nested/info.md"}}, []string{"default", "DEFAULT", ".md"}},
		{"four-files-with-shared-custom-terms", []hfNode{{Path: "data/customdata/other/one.csv"}, {Path: "data/customdata/two.csv"}, {Path: "data/other/three.csv"}, {Path: "root.json"}}, []string{"customdata", "other", "CSV"}},
	}
	queries := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := compileDecisionFixture(tc.nodes, tc.filters, nil)
			for i, in := range c.Inputs {
				for _, pin := range in.Alternatives {
					for _, zeros := range [][]int{{i}, {i, (i + 1) % len(c.Inputs)}} {
						for _, positiveIndex := range []int{-1, (i + 1) % len(c.Inputs)} {
							positives := []*datasetDecisionPositive{nil}
							if positiveIndex >= 0 {
								positives = positives[:0]
								for _, alt := range c.Inputs[positiveIndex].Alternatives {
									positives = append(positives, &datasetDecisionPositive{Input: positiveIndex, Alternative: alt})
								}
							}
							for _, positive := range positives {
								for _, skipIndex := range []int{-1, (i + 2) % len(c.Inputs)} {
									var skips []int
									if skipIndex >= 0 {
										skips = []int{skipIndex}
									}
									pins := datasetDecisionPins{i: pin}
									for _, core := range oracleCoreAssignments(c) {
										want, oracleWorld := enumerateDecisionFeasibility(c, core, pins, skips, zeros, positive)
										gotWorld, got := feasibleDatasetDecisionWorld(c, core, pins, skips, zeros, positive)
										queries++
										if got != want {
											t.Errorf("solver/oracle mismatch filters=%q core=%v roles=%+v pins=%v skips=%v zeroDirect=%v positive=%+v: solver=%v oracle=%v witness=%+v", tc.filters, core, c.Inputs, pins, skips, zeros, positive, got, want, gotWorld)
											continue
										}
										if got && !decisionWitnessSatisfies(c, gotWorld, core, pins, skips, zeros, positive) {
											t.Errorf("invalid solver witness: filters=%q core=%v pins=%v skips=%v zeroDirect=%v positive=%+v witness=%+v", tc.filters, core, pins, skips, zeros, positive, gotWorld)
										}
										if want && !decisionWitnessSatisfies(c, oracleWorld, core, pins, skips, zeros, positive) {
											t.Errorf("oracle generated invalid witness: %s", fmt.Sprint(oracleWorld))
										}
									}
								}
							}
						}
					}
				}
			}
		})
	}
	if queries < 70 {
		t.Fatalf("oracle query count=%d, expected broad small-domain coverage", queries)
	}
	t.Logf("independent concrete oracle matched solver for %d generated queries", queries)
}

func TestRequiredSkipRejectsRootCompanionAndAcceptsNestedCompanion(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "train.csv"}, {Path: "config.json"}, {Path: "nested/notes.md"}}, []string{"train"}, nil)
	rootCompanion := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleCompanion}
	if _, ok := feasibleAnyCoreDecisionWorld(c, datasetDecisionPins{1: rootCompanion}, []int{1}, nil, nil); ok {
		t.Fatal("root non-LFS companion cannot satisfy required skip")
	}
	if _, ok := feasibleAnyCoreDecisionWorld(c, datasetDecisionPins{2: rootCompanion}, []int{2}, nil, nil); !ok {
		t.Fatal("unseeded nested companion should satisfy required skip")
	}
	if _, ok := feasibleAnyCoreDecisionWorld(c, datasetDecisionPins{99: rootCompanion}, nil, nil, nil); ok {
		t.Fatal("pin to unknown input accepted")
	}
	seeded := compileDecisionFixture([]hfNode{{Path: "train/data.csv"}, {Path: "train/meta.json"}}, []string{"train"}, nil)
	positiveSeed := &datasetDecisionPositive{Input: 0, Alternative: seeded.Inputs[0].Alternatives[0]}
	if _, ok := feasibleAnyCoreDecisionWorld(seeded, datasetDecisionPins{1: rootCompanion}, []int{1}, nil, positiveSeed); ok {
		t.Fatal("positive direct source cannot coexist with required skip of its companion descendant")
	}
	lfs := compileDecisionFixture([]hfNode{{Path: "train.csv"}, {Path: "other/weights.bin", LFS: &hfLfsInfo{Size: 3}}}, []string{"train"}, nil)
	if _, ok := feasibleAnyCoreDecisionWorld(lfs, datasetDecisionPins{1: rootCompanion}, []int{1}, nil, nil); !ok {
		t.Fatal("non-direct LFS companion should be skipped")
	}
	directLFS := compileDecisionFixture([]hfNode{{Path: "other/train/weights.bin", LFS: &hfLfsInfo{Size: 3}}}, []string{"train"}, nil)
	if _, ok := feasibleAnyCoreDecisionWorld(directLFS, datasetDecisionPins{0: rootCompanion}, []int{0}, nil, nil); ok {
		t.Fatal("directly matched LFS companion is selected, not skipped")
	}
	excludedSeed := compileDecisionFixture([]hfNode{{Path: "partition/train.csv"}, {Path: "partition/notes.md"}}, []string{"train"}, []string{"train.csv"})
	train := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleData, Label: "train"}
	if _, ok := feasibleAnyCoreDecisionWorld(excludedSeed, datasetDecisionPins{1: rootCompanion}, []int{1}, nil, nil); !ok {
		t.Fatal("excluded direct candidate must not seed companion scope")
	}
	if _, ok := feasibleAnyCoreDecisionWorld(excludedSeed, datasetDecisionPins{0: train}, []int{0}, nil, nil); !ok {
		t.Fatal("excluded file is skipped regardless of its role or direct match")
	}
}

func TestPositiveDirectAssignsResidualSharedCustomBit(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "data/customdata/other/a.parquet"}, {Path: "data/other/b.parquet"}}, []string{"other"}, nil)
	alt := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleData, Label: "other"}
	if !containsString(c.ResidualLabels, "other") {
		t.Fatalf("expected custom residual label: %+v", c.ResidualLabels)
	}
	bit := c.FilterBits["other"]
	if !oracleCollisionBits(c)[bit] {
		t.Fatalf("fixture did not create the residual shared-token collision: labels=%v inputs=%+v filters=%+v", c.RecognizedLabels, c.Inputs, c.Filters)
	}
	w, ok := feasibleAnyCoreDecisionWorld(c, datasetDecisionPins{1: alt}, nil, nil, &datasetDecisionPositive{Input: 1, Alternative: alt})
	if !ok || !w.SharedToken[bit] {
		t.Fatalf("positive custom membership must assign its shared residual unit true: ok=%v witness=%+v bit=%d", ok, w, bit)
	}
}

func TestZeroDirectUsesEveryIndependentGroupTerm(t *testing.T) {
	// Minimal counterexample to the first draft's aggregation: this file has
	// actual train/test/validation alternatives, but the pinned validation role
	// makes train and test direct only under their ordinary-path interpretations.
	// Both bits can be set to membership, making every direct term false.
	c := compileDecisionFixture([]hfNode{{Path: "train/test/validation/data.csv"}}, []string{"train", "test"}, nil)
	validation := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleData, Label: "validation"}
	pins := datasetDecisionPins{0: validation}
	zeros := []int{0}
	core := map[uint]bool{c.FilterBits["train"]: true, c.FilterBits["test"]: true}
	want, oracle := enumerateDecisionFeasibility(c, core, pins, nil, zeros, nil)
	gotWorld, got := feasibleDatasetDecisionWorld(c, core, pins, nil, zeros, nil)
	if !want || !got {
		t.Fatalf("multi-term zero-direct should be feasible: filters=%v roles=%+v pins=%v coreUnits=%v zeroDirect=%v expected=%v oracleWorld=%+v solver=%v witness=%+v", c.Filters, c.Inputs, pins, core, zeros, want, oracle, got, gotWorld)
	}
	if !decisionWitnessSatisfies(c, gotWorld, core, pins, nil, zeros, nil) {
		t.Fatalf("solver returned invalid witness: %+v", gotWorld)
	}
	if !gotWorld.SharedToken[c.FilterBits["train"]] || !gotWorld.SharedToken[c.FilterBits["test"]] {
		t.Fatalf("zero-direct must force both independent group bits to membership: %+v", gotWorld.SharedToken)
	}
	contradictoryCore := map[uint]bool{c.FilterBits["train"]: false, c.FilterBits["test"]: true}
	want, _ = enumerateDecisionFeasibility(c, contradictoryCore, pins, nil, zeros, nil)
	if want {
		t.Fatal("oracle found zero-direct under a fixed ordinary interpretation for train")
	}
	if _, ok := feasibleDatasetDecisionWorld(c, contradictoryCore, pins, nil, zeros, nil); ok {
		t.Fatal("primitive ignored its fixed core assignment")
	}
	if _, ok := feasibleDatasetDecisionWorld(c, map[uint]bool{c.FilterBits["train"]: true}, pins, nil, zeros, nil); ok {
		t.Fatal("primitive accepted an incomplete core assignment")
	}
	positive := &datasetDecisionPositive{Input: 0, Alternative: validation}
	want, _ = enumerateDecisionFeasibility(c, core, pins, nil, zeros, positive)
	if want {
		t.Fatal("oracle found a world satisfying both zero-direct and positive-direct on one pinned file")
	}
	if _, ok := feasibleDatasetDecisionWorld(c, core, pins, nil, zeros, positive); ok {
		t.Fatal("solver accepted conflicting shared-token requirements")
	}
	duplicate := compileDecisionFixture([]hfNode{{Path: "train/test/validation/data.csv"}}, []string{"train", "TRAIN", "test"}, nil)
	dupCore := map[uint]bool{duplicate.FilterBits["train"]: true, duplicate.FilterBits["test"]: true}
	want, _ = enumerateDecisionFeasibility(duplicate, dupCore, pins, nil, zeros, nil)
	gotWorld, got = feasibleDatasetDecisionWorld(duplicate, dupCore, pins, nil, zeros, nil)
	if !want || !got || duplicate.Filters[0].Bit != duplicate.Filters[1].Bit || !gotWorld.SharedToken[duplicate.Filters[0].Bit] {
		t.Fatalf("case-normalized duplicate terms must share the one forced unit: oracle=%v solver=%v world=%+v filters=%+v", want, got, gotWorld, duplicate.Filters)
	}
	explicit := compileDecisionFixture([]hfNode{{Path: "train/test/validation/data.csv"}}, []string{"train", "test", ".csv"}, nil)
	want, _ = enumerateAnyCoreDecisionFeasibility(explicit, pins, nil, zeros, nil)
	if want {
		t.Fatal("independent ordinary .csv term should make zero-direct impossible")
	}
	if _, ok := feasibleAnyCoreDecisionWorld(explicit, pins, nil, zeros, nil); ok {
		t.Fatal("solver ignored constant-true explicit ordinary term")
	}
}

// This deliberately enumerates the tiny test domain and evaluates direct
// terms/skip outcomes with an independent concrete reference, never through
// the symbolic solver, literal compiler, or production world evaluator.
func enumerateDecisionFeasibility(c datasetDecisionContext, coreAssignment map[uint]bool, pins datasetDecisionPins, skips, zeros []int, positive *datasetDecisionPositive) (bool, datasetDecisionWorld) {
	collisions := oracleCollisionBits(c)
	var bits []uint
	for bit := range collisions {
		if !oracleCoreBit(c, bit) {
			bits = append(bits, bit)
		}
	}
	for mask := 0; mask < 1<<len(bits); mask++ {
		w := datasetDecisionWorld{DataLabels: map[int]string{}, Companion: map[int]bool{}, SharedToken: map[uint]bool{}}
		for bit, value := range coreAssignment {
			if !collisions[bit] || !oracleCoreBit(c, bit) {
				return false, datasetDecisionWorld{}
			}
			w.SharedToken[bit] = value
		}
		for bit := range collisions {
			if oracleCoreBit(c, bit) {
				if _, ok := coreAssignment[bit]; !ok {
					return false, datasetDecisionWorld{}
				}
			}
		}
		for j, bit := range bits {
			w.SharedToken[bit] = mask&(1<<j) != 0
		}
		var visit func(int) (bool, datasetDecisionWorld)
		visit = func(i int) (bool, datasetDecisionWorld) {
			if i == len(c.Inputs) {
				for index, pin := range pins {
					if index < 0 || index >= len(c.Inputs) || !hasDecisionAlternative([]datasetDecisionRoleLabel{worldRole(w, index)}, pin) {
						return false, datasetDecisionWorld{}
					}
				}
				for _, index := range zeros {
					if concreteOracleDirect(c, w, index, collisions) {
						return false, datasetDecisionWorld{}
					}
				}
				if positive != nil {
					if positive.Input < 0 || positive.Input >= len(c.Inputs) || worldRole(w, positive.Input) != positive.Alternative || !concreteOracleDirect(c, w, positive.Input, collisions) {
						return false, datasetDecisionWorld{}
					}
				}
				if len(skips) > 0 {
					skipped := concreteOracleSkipped(c, w, collisions)
					for _, index := range skips {
						if index < 0 || index >= len(skipped) || !skipped[index] {
							return false, datasetDecisionWorld{}
						}
					}
				}
				return true, cloneDecisionWorld(w)
			}
			for _, alt := range c.Inputs[i].Alternatives {
				if alt.Role == datasetselection.EvidenceRoleCompanion {
					w.Companion[i] = true
				} else {
					w.DataLabels[i] = alt.Label
				}
				if ok, found := visit(i + 1); ok {
					return true, found
				}
				delete(w.Companion, i)
				delete(w.DataLabels, i)
			}
			return false, datasetDecisionWorld{}
		}
		if ok, found := visit(0); ok {
			return true, found
		}
	}
	return false, datasetDecisionWorld{}
}

func oracleCoreBit(c datasetDecisionContext, bit uint) bool {
	for _, filter := range c.Filters {
		if filter.Bit == bit {
			for _, label := range datasetCoreLabels {
				if filter.Normalized == label {
					return true
				}
			}
			return false
		}
	}
	return false
}
func oracleCoreAssignments(c datasetDecisionContext) []map[uint]bool {
	var bits []uint
	for bit := range oracleCollisionBits(c) {
		if oracleCoreBit(c, bit) {
			bits = append(bits, bit)
		}
	}
	assignments := make([]map[uint]bool, 0, 1<<len(bits))
	for mask := 0; mask < 1<<len(bits); mask++ {
		assignment := map[uint]bool{}
		for i, bit := range bits {
			assignment[bit] = mask&(1<<i) != 0
		}
		assignments = append(assignments, assignment)
	}
	return assignments
}
func enumerateAnyCoreDecisionFeasibility(c datasetDecisionContext, pins datasetDecisionPins, skips, zeros []int, positive *datasetDecisionPositive) (bool, datasetDecisionWorld) {
	for _, core := range oracleCoreAssignments(c) {
		if ok, w := enumerateDecisionFeasibility(c, core, pins, skips, zeros, positive); ok {
			return true, w
		}
	}
	return false, datasetDecisionWorld{}
}
func feasibleAnyCoreDecisionWorld(c datasetDecisionContext, pins datasetDecisionPins, skips, zeros []int, positive *datasetDecisionPositive) (datasetDecisionWorld, bool) {
	for _, core := range oracleCoreAssignments(c) {
		if w, ok := feasibleDatasetDecisionWorld(c, core, pins, skips, zeros, positive); ok {
			return w, true
		}
	}
	return datasetDecisionWorld{}, false
}

func oracleCollisionBits(c datasetDecisionContext) map[uint]bool {
	bits := map[uint]bool{}
	// Re-derive ambiguity from actual role/label alternatives and concrete path
	// matching; never use the compiler's collision-index attribution as oracle
	// input.
	for _, filter := range c.Filters {
		recognized := false
		for _, input := range c.Inputs {
			for _, alt := range input.Alternatives {
				if alt.Role == datasetselection.EvidenceRoleData && strings.EqualFold(alt.Label, filter.Normalized) {
					recognized = true
				}
			}
		}
		if !recognized {
			continue
		}
		for _, input := range c.Inputs {
			if input.Excluded {
				continue
			}
			if !filterMatchesPath(input.Path, filter.Original, c.ExactMatch) {
				continue
			}
			for _, alt := range input.Alternatives {
				if alt.Role == datasetselection.EvidenceRoleData && !strings.EqualFold(alt.Label, filter.Normalized) {
					bits[filter.Bit] = true
					break
				}
			}
		}
	}
	return bits
}

func worldRole(w datasetDecisionWorld, i int) datasetDecisionRoleLabel {
	if w.Companion[i] {
		return datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleCompanion}
	}
	return datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleData, Label: w.DataLabels[i]}
}
func concreteOracleDirect(c datasetDecisionContext, w datasetDecisionWorld, i int, collisions map[uint]bool) bool {
	recognized := map[string]bool{}
	for _, label := range c.RecognizedLabels {
		recognized[label] = true
	}
	for _, filter := range c.Filters {
		pathMatch := filterMatchesPath(c.Inputs[i].Path, filter.Original, c.ExactMatch)
		matched := false
		if w.Companion[i] || !recognized[filter.Normalized] {
			matched = pathMatch
		} else if collisions[filter.Bit] {
			if w.SharedToken[filter.Bit] {
				matched = w.DataLabels[i] == filter.Normalized
			} else {
				matched = pathMatch
			}
		} else {
			matched = w.DataLabels[i] == filter.Normalized
		}
		if matched {
			return true
		}
	}
	return false
}

// concreteOracleSkipped is a deliberately small independent reference for
// the evaluator's direct-selection, directory-seed, and input-order retention
// rules. It does not call the production world evaluator or validate helper.
func concreteOracleSkipped(c datasetDecisionContext, w datasetDecisionWorld, collisions map[uint]bool) []bool {
	n := len(c.Inputs)
	direct := make([]bool, n)
	dirs := map[string]bool{}
	for i, in := range c.Inputs {
		if in.Excluded {
			continue
		}
		direct[i] = concreteOracleDirect(c, w, i, collisions)
		if direct[i] && !w.Companion[i] {
			for d := path.Dir(strings.ReplaceAll(in.Path, "\\", "/")); d != "." && d != "/"; d = path.Dir(d) {
				dirs[d] = true
			}
		}
	}
	skipped := make([]bool, n)
	seenExact := map[string]bool{}
	seenFold := map[string]bool{}
	for i, in := range c.Inputs {
		if seenExact[in.Path] {
			skipped[i] = true
			continue
		}
		seenExact[in.Path] = true
		if in.Excluded {
			skipped[i] = true
			continue
		}
		fold := strings.ToLower(in.Path)
		if seenFold[fold] {
			skipped[i] = true
			continue
		}
		if !direct[i] {
			if !w.Companion[i] || in.LFS {
				skipped[i] = true
				continue
			}
			d := path.Dir(strings.ReplaceAll(in.Path, "\\", "/"))
			if d != "." && !dirs[d] {
				skipped[i] = true
				continue
			}
		}
		seenFold[fold] = true
	}
	return skipped
}
func cloneDecisionWorld(w datasetDecisionWorld) datasetDecisionWorld {
	out := datasetDecisionWorld{DataLabels: map[int]string{}, Companion: map[int]bool{}, SharedToken: map[uint]bool{}}
	for k, v := range w.DataLabels {
		out.DataLabels[k] = v
	}
	for k, v := range w.Companion {
		out.Companion[k] = v
	}
	for k, v := range w.SharedToken {
		out.SharedToken[k] = v
	}
	return out
}
func decisionWitnessSatisfies(c datasetDecisionContext, w datasetDecisionWorld, core map[uint]bool, pins datasetDecisionPins, skips, zeros []int, positive *datasetDecisionPositive) bool {
	collisions := oracleCollisionBits(c)
	if !oracleWorldValid(c, w, collisions) {
		return false
	}
	for bit, value := range core {
		if w.SharedToken[bit] != value {
			return false
		}
	}
	for i, pin := range pins {
		if i < 0 || i >= len(c.Inputs) || worldRole(w, i) != pin {
			return false
		}
	}
	for _, i := range zeros {
		if concreteOracleDirect(c, w, i, collisions) {
			return false
		}
	}
	if positive != nil && (positive.Input < 0 || positive.Input >= len(c.Inputs) || worldRole(w, positive.Input) != positive.Alternative || !concreteOracleDirect(c, w, positive.Input, collisions)) {
		return false
	}
	if len(skips) > 0 {
		skipped := concreteOracleSkipped(c, w, collisions)
		for _, i := range skips {
			if i < 0 || i >= len(skipped) || !skipped[i] {
				return false
			}
		}
	}
	return true
}

func oracleWorldValid(c datasetDecisionContext, w datasetDecisionWorld, collisions map[uint]bool) bool {
	for i, in := range c.Inputs {
		if !hasDecisionAlternative(in.Alternatives, worldRole(w, i)) {
			return false
		}
	}
	for bit := range w.SharedToken {
		if !collisions[bit] {
			return false
		}
	}
	for bit := range collisions {
		if _, ok := w.SharedToken[bit]; !ok {
			return false
		}
	}
	return true
}

func TestCompileDatasetDecisionContextFilterBitsAndCollisionEvidence(t *testing.T) {
	nodes := []hfNode{{Path: "train.csv"}, {Path: "test/train.csv"}, {Path: "data/custom/chunk.arrow"}, {Path: "dir_name/item.csv"}}
	c := compileDecisionFixture(nodes, []string{"train", " TRAIN ", ".csv", "test"}, nil)
	if c.Filters[0].Bit != c.Filters[1].Bit || c.Filters[0].Index == c.Filters[1].Index {
		t.Fatalf("duplicate tokens should share a bit but retain positions: %+v", c.Filters)
	}
	if c.FilterBits["train"] == c.FilterBits[".csv"] {
		t.Fatal("different normalized tokens shared a bit")
	}
	if got := c.Inputs[1].CollisionFilterIndexes; !reflect.DeepEqual(got, []int{0, 1, 3}) {
		t.Fatalf("collision interpretations=%v want [0 1 3]", got)
	}
	if len(c.Inputs[1].RecognizedCompanionFilterIndexes) != 0 {
		t.Fatalf("definite dataset input was recorded as companion: %v", c.Inputs[1].RecognizedCompanionFilterIndexes)
	}
	if got := c.Inputs[3].DirectFilterIndexes; !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("ordinary extension matching=%v want [.csv index]", got)
	}
	// Underscore is not an exact-match delimiter: it must not manufacture a
	// collision merely because the token is a substring of a path component.
	underscore := compileDecisionFixture([]hfNode{{Path: "train.csv"}, {Path: "not_train.csv"}}, []string{"train"}, nil)
	if got := underscore.Inputs[1].RecognizedCompanionFilterIndexes; len(got) != 0 {
		t.Fatalf("underscore substring became collision evidence: %v", got)
	}
}

func TestCompileDatasetDecisionContextRecognizedPathMatchDoesNotReclassifyData(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "train/data.parquet"}, {Path: "train/README.md"}}, []string{"train"}, nil)
	if got := c.Inputs[1].RecognizedCompanionFilterIndexes; !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("root/path companion recognized match=%v want [0]", got)
	}
	if len(c.Inputs[0].Alternatives) != 1 || c.Inputs[0].Alternatives[0].Role != datasetselection.EvidenceRoleData {
		t.Fatalf("definite data was reclassified by a path match: %+v", c.Inputs[0].Alternatives)
	}
}

func TestCompileDatasetDecisionContextPathPredicatesSeedsAndDuplicates(t *testing.T) {
	nodes := []hfNode{
		{Path: "root.csv"}, {Path: "folder/train.csv"}, {Path: "folder/sub/companion.json"},
		{Path: "other/companion.json"}, {Path: "folder/train.csv"}, {Path: "FOLDER/train.csv"},
	}
	c := compileDecisionFixture(nodes, []string{"train.csv"}, []string{"other/"})
	if !reflect.DeepEqual(c.Inputs[1].DirectFilterIndexes, []int{0}) {
		t.Fatalf("full-path direct match=%v", c.Inputs[1].DirectFilterIndexes)
	}
	if got := validDatasetDirectorySeeds(c, c.Inputs[2].Directory); len(got) != 0 {
		t.Fatalf("parent-directory source incorrectly seeded its child: %v", got)
	}
	if len(validDatasetDirectorySeeds(c, c.Inputs[3].Directory)) != 0 {
		t.Fatalf("unrelated directory inherited seeds: %v", validDatasetDirectorySeeds(c, c.Inputs[3].Directory))
	}
	if !c.Inputs[3].Excluded {
		t.Fatal("exclude predicate missing")
	}
	if c.Inputs[4].ExactDuplicateOf != 1 || c.Inputs[4].ExactDuplicateEligible {
		t.Fatalf("exact duplicate attribution=%+v", c.Inputs[4])
	}
	if c.Inputs[5].CaseDuplicateOf != 1 || c.Inputs[5].CaseDuplicateEligible {
		t.Fatalf("case duplicate attribution=%+v", c.Inputs[5])
	}
	if !c.Inputs[1].ExactDuplicateEligible || !c.Inputs[1].CaseDuplicateEligible {
		t.Fatal("first path should be eligible for both dedup emissions")
	}
}

func TestCompileDatasetDecisionContextMembershipSelectionsCanSeedDirectories(t *testing.T) {
	// The filter "train" selects train.csv by membership even though the
	// ordinary complete-path matcher does not match that filename. It must
	// still seed companion scope for siblings beneath the selected directory.
	c := compileDecisionFixture([]hfNode{{Path: "partition/train.csv"}, {Path: "partition/metadata.json"}}, []string{"train"}, nil)
	if !c.Inputs[0].DirectSeedCandidate {
		t.Fatal("membership-selected file was not indexed as a possible seed")
	}
	if got := validDatasetDirectorySeeds(c, c.Inputs[1].Directory); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("membership-selected directory seeds=%v want [0]", got)
	}
	// Exclusion is prior to selection and must prevent a seed.
	excluded := compileDecisionFixture([]hfNode{{Path: "partition/train.csv"}, {Path: "partition/metadata.json"}}, []string{"train"}, []string{"train.csv"})
	if excluded.Inputs[0].DirectSeedCandidate || len(validDatasetDirectorySeeds(excluded, excluded.Inputs[1].Directory)) != 0 {
		t.Fatalf("excluded membership source seeded directory: %+v", excluded.Inputs)
	}
	companionOnly := compileDecisionFixture([]hfNode{{Path: "train/README.md"}, {Path: "other/metadata.json"}}, []string{"train"}, nil)
	if companionOnly.Inputs[0].DirectSeedCandidate || len(validDatasetDirectorySeeds(companionOnly, companionOnly.Inputs[1].Directory)) != 0 {
		t.Fatalf("directly selected companion became a directory seed: %+v", companionOnly.Inputs)
	}
}

func TestCompileDatasetDecisionContextIsSnapshot(t *testing.T) {
	filters := []string{"train"}
	excludes := []string{"draft"}
	nodes := []hfNode{{Path: "train.csv"}, {Path: "draft.json"}}
	c := compileDecisionFixture(nodes, filters, excludes)
	filters[0] = "test"
	excludes[0] = ""
	nodes[0].Path = "mutated.csv"
	if c.Filters[0].Original != "train" || c.Inputs[0].Path != "train.csv" || !c.Inputs[1].Excluded {
		t.Fatalf("compiled snapshot aliases live inputs: %+v", c)
	}
	if c.Inputs[0].DirectCoreConditions == 0 || c.Inputs[0].PossibleCore == 0 {
		t.Fatal("expected core condition and actual possible-label bits")
	}
}

func TestEvaluateDatasetDecisionWorldUsesConditionalCompanionScope(t *testing.T) {
	nodes := []hfNode{{Path: "train.csv"}, {Path: "train/README.md"}, {Path: "other/meta.json"}, {Path: "README.md"}, {Path: "root.csv"}}
	c := compileDecisionFixture(nodes, []string{"train"}, nil)
	w := mustDecisionWorld(t, c, map[int]datasetDecisionRoleLabel{
		2: {Role: datasetselection.EvidenceRoleCompanion},
		3: {Role: datasetselection.EvidenceRoleCompanion},
	}, nil)
	got, err := evaluateDatasetDecisionWorld(c, w)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Records[0].Selected || !got.Records[1].Selected || got.Records[2].Selected || !got.Records[3].Selected {
		t.Fatalf("unexpected direct/companion disposition: %+v", got.Records)
	}
	if got.Records[4].Selected {
		t.Fatalf("unselected root data fell back as a companion: %+v", got.Records[4])
	}
}

func TestEvaluateDatasetDecisionWorldHonorsFirstLongestAttributionAndLFS(t *testing.T) {
	nodes := []hfNode{{Path: "dir/test.csv"}, {Path: "dir/data.bin", LFS: &hfLfsInfo{Size: 1}}, {Path: "dir/data.json"}}
	c := compileDecisionFixture(nodes, []string{".csv", "test"}, nil)
	w := mustDecisionWorld(t, c, map[int]datasetDecisionRoleLabel{
		0: {Role: datasetselection.EvidenceRoleData, Label: "test"},
		1: {Role: datasetselection.EvidenceRoleCompanion},
		2: {Role: datasetselection.EvidenceRoleCompanion},
	}, nil)
	got, err := evaluateDatasetDecisionWorld(c, w)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Records[0].Selected || got.Records[0].Subdir != ".csv" {
		t.Fatalf("equal-length filters must preserve first input attribution: %+v", got.Records[0])
	}
	if got.Records[1].Selected || !got.Records[1].Skipped {
		t.Fatalf("unmatched LFS payload must not inherit companion scope: %+v", got.Records[1])
	}
	if !got.Records[2].Selected {
		t.Fatalf("nested non-LFS companion in selected directory should remain: %+v", got.Records[2])
	}
}

func mustDecisionWorld(t *testing.T, c datasetDecisionContext, choices map[int]datasetDecisionRoleLabel, shared map[uint]bool) datasetDecisionWorld {
	t.Helper()
	w, err := datasetDecisionWorldFromAlternatives(c, choices, shared)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestConcreteWorldSharedCollisionAndIndependentFilter(t *testing.T) {
	nodes := []hfNode{{Path: "test/train.csv"}, {Path: "train_dataset.csv"}}
	c := compileDecisionFixture(nodes, []string{"train", " TRAIN "}, nil)
	bit := c.FilterBits["train"]
	choices := map[int]datasetDecisionRoleLabel{
		0: {Role: datasetselection.EvidenceRoleData, Label: "train"},
	}
	membershipWorld := mustDecisionWorld(t, c, choices, map[uint]bool{bit: true})
	ordinaryWorld := mustDecisionWorld(t, c, choices, map[uint]bool{bit: false})
	a, err := evaluateDatasetDecisionWorld(c, membershipWorld)
	if err != nil {
		t.Fatal(err)
	}
	b, err := evaluateDatasetDecisionWorld(c, ordinaryWorld)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Records[0].Selected || !b.Records[0].Selected || !a.Records[1].Selected || b.Records[1].Selected {
		t.Fatalf("shared token interpretations collapsed: membership=%+v ordinary=%+v", a.Records, b.Records)
	}

	// An unrelated ordinary extension term independently selects data in
	// either shared-token interpretation.
	extensionContext := compileDecisionFixture(nodes, []string{"train", ".csv"}, nil)
	extensionBit := extensionContext.FilterBits["train"]
	for _, membershipChoice := range []bool{true, false} {
		world := mustDecisionWorld(t, extensionContext, choices, map[uint]bool{extensionBit: membershipChoice})
		got, err := evaluateDatasetDecisionWorld(extensionContext, world)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Records[0].Selected || !got.Records[1].Selected {
			t.Fatalf("independent extension term failed to select both matched paths for membership=%t: %+v", membershipChoice, got.Records)
		}
	}
}

func TestConcreteWorldDefaultDataDoesNotSelfSeedCompanionAlternative(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "nested/data.json"}}, []string{"default"}, nil)
	data := mustDecisionWorld(t, c, map[int]datasetDecisionRoleLabel{0: {Role: datasetselection.EvidenceRoleData, Label: "default"}}, nil)
	companion := mustDecisionWorld(t, c, map[int]datasetDecisionRoleLabel{0: {Role: datasetselection.EvidenceRoleCompanion}}, nil)
	a, err := evaluateDatasetDecisionWorld(c, data)
	if err != nil {
		t.Fatal(err)
	}
	b, err := evaluateDatasetDecisionWorld(c, companion)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Records[0].Selected || b.Records[0].Selected {
		t.Fatalf("default data/companion role distinction lost: data=%+v companion=%+v", a.Records[0], b.Records[0])
	}
}

func TestConcreteWorldMembershipSelectionSeedsNestedCompanionDirectory(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "partition/train_dataset.csv"}, {Path: "partition/metadata.json"}}, []string{"train"}, nil)
	w := mustDecisionWorld(t, c, map[int]datasetDecisionRoleLabel{1: {Role: datasetselection.EvidenceRoleCompanion}}, nil)
	got, err := evaluateDatasetDecisionWorld(c, w)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Records[0].Selected || !got.Records[1].Selected {
		t.Fatalf("membership-selected underscore path did not seed companion scope: %+v", got.Records)
	}
}

func TestConcreteWorldDirectCompanionDoesNotSeedNestedScope(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "train.csv"},
		{Path: "other/README.md"},
		{Path: "other/metadata.json"},
	}, []string{"train", "README"}, []string{"train.csv"})
	w := mustDecisionWorld(t, c, map[int]datasetDecisionRoleLabel{
		1: {Role: datasetselection.EvidenceRoleCompanion},
		2: {Role: datasetselection.EvidenceRoleCompanion},
	}, nil)
	got, err := evaluateDatasetDecisionWorld(c, w)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Records[1].Selected || got.Records[2].Selected {
		t.Fatalf("directly selected companion incorrectly seeded nested scope: %+v", got.Records)
	}
}

func TestConcreteWorldAttributionTieFollowsFilterOrder(t *testing.T) {
	nodes := []hfNode{{Path: "dir/test.csv"}}
	for _, tc := range []struct {
		filters []string
		want    string
	}{{[]string{".csv", "test"}, ".csv"}, {[]string{"test", ".csv"}, "test"}} {
		c := compileDecisionFixture(nodes, tc.filters, nil)
		got, err := evaluateDatasetDecisionWorld(c, mustDecisionWorld(t, c, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if got.Records[0].Subdir != tc.want {
			t.Errorf("filters %v attributed %q, want %q", tc.filters, got.Records[0].Subdir, tc.want)
		}
	}
}

func TestConcreteWorldRequiresAdmissibleExplicitAmbiguousRole(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "meta.json"}}, []string{"default"}, nil)
	if _, err := datasetDecisionWorldFromAlternatives(c, nil, nil); err == nil {
		t.Fatal("ambiguous evidence silently defaulted to companion")
	}
	if _, err := datasetDecisionWorldFromAlternatives(c, map[int]datasetDecisionRoleLabel{0: {Role: datasetselection.EvidenceRoleData, Label: "train"}}, nil); err == nil {
		t.Fatal("inadmissible data label accepted")
	}
}

func TestExcludedCollisionDoesNotCreateSharedTokenVariable(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "test/train.csv"}, {Path: "train_dataset.csv"}}, []string{"train"}, []string{"test/train.csv"})
	w := mustDecisionWorld(t, c, map[int]datasetDecisionRoleLabel{0: {Role: datasetselection.EvidenceRoleData, Label: "test"}}, nil)
	got, err := evaluateDatasetDecisionWorld(c, w)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Records[0].Skipped || !got.Records[1].Selected {
		t.Fatalf("excluded collision affected active membership interpretation: %+v", got.Records)
	}
}

func TestConcreteWorldKeepsMultipleCustomCollisionBitsDistinct(t *testing.T) {
	nodes := []hfNode{
		{Path: "test/alpha_group.csv"},
		{Path: "validation/beta_group.csv"},
		{Path: "data/alpha_group/chunk.csv"},
		{Path: "data/beta_group/chunk.csv"},
	}
	c := compileDecisionFixture(nodes, []string{"alpha_group", " ALPHA_GROUP ", "beta_group"}, nil)
	alphaBit, betaBit := c.FilterBits["alpha_group"], c.FilterBits["beta_group"]
	if alphaBit == betaBit {
		t.Fatal("distinct custom collision tokens shared one bit")
	}
	w := mustDecisionWorld(t, c, map[int]datasetDecisionRoleLabel{
		0: {Role: datasetselection.EvidenceRoleData, Label: "test"},
		1: {Role: datasetselection.EvidenceRoleData, Label: "validation"},
	}, map[uint]bool{alphaBit: true, betaBit: false})
	got, err := evaluateDatasetDecisionWorld(c, w)
	if err != nil {
		t.Fatal(err)
	}
	if got.Records[0].Selected || !got.Records[1].Selected || !got.Records[2].Selected || !got.Records[3].Selected {
		t.Fatalf("distinct custom token interpretations were not applied globally: %+v", got.Records)
	}
}

func TestConcreteWorldCaseInsensitiveFirstEligibleEmission(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "Train/file.csv"}, {Path: "train/FILE.CSV"}}, []string{"train", ".csv"}, nil)
	w := mustDecisionWorld(t, c, nil, nil)
	got, err := evaluateDatasetDecisionWorld(c, w)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Records[0].Selected || got.Records[1].Selected || !got.Records[1].Skipped {
		t.Fatalf("case-equivalent records did not retain first eligible winner: %+v", got.Records)
	}
}

func TestDatasetDecisionQueriesRefuseOnlyWhenBothOutcomesHaveWorlds(t *testing.T) {
	cases := []struct {
		name              string
		nodes             []hfNode
		filters, excludes []string
		target            int
	}{
		{"train-test-collision", []hfNode{{Path: "train.csv"}, {Path: "test/train_data.parquet"}}, []string{"train", "test"}, nil, 1},
		{"explicit-filename-extension-directory", []hfNode{{Path: "data/train.csv"}, {Path: "data/test.csv"}, {Path: "metadata.json"}}, []string{"train", "data/train.csv", ".csv", "data/"}, nil, 0},
		{"both-groups", []hfNode{{Path: "train.csv"}, {Path: "test.csv"}}, []string{"train", "test"}, nil, 0},
		{"custom-and-train", []hfNode{{Path: "train.csv"}, {Path: "data/custom/chunk.parquet"}}, []string{"train", "custom"}, nil, 1},
		{"nested-companion-seed", []hfNode{{Path: "train.csv"}, {Path: "train/meta.json"}}, []string{"train"}, nil, 1},
		{"seed-in-child-directory", []hfNode{{Path: "data/train/test.csv"}, {Path: "data/README.md"}}, []string{"train"}, nil, 1},
		{"child-companion-is-not-seeded-by-parent", []hfNode{{Path: "folder/train.csv"}, {Path: "folder/sub/notes.json"}}, []string{"train", "default"}, nil, 1},
		{"direct-companion-filter", []hfNode{{Path: "train.csv"}, {Path: "docs/README.md"}}, []string{"train", "docs/"}, nil, 1},
		{"direct-lfs-companion-filter", []hfNode{{Path: "train.csv"}, {Path: "docs/README.md", LFS: &hfLfsInfo{Size: 3}}}, []string{"train", ".md"}, nil, 1},
		{"attribution-tie", []hfNode{{Path: "dir/test.csv"}}, []string{".csv", "test"}, nil, 0},
		{"excluded-target", []hfNode{{Path: "train.csv"}, {Path: "test/train.csv"}}, []string{"train", "test"}, []string{"test/train.csv"}, 1},
		{"case-winner", []hfNode{{Path: "Train/a.csv"}, {Path: "train/A.CSV"}}, []string{"train", ".csv"}, nil, 1},
		{"excluded-case-winner", []hfNode{{Path: "Train/a.csv"}, {Path: "train/A.CSV"}}, []string{"train", ".csv"}, []string{"Train/a.csv"}, 1},
	}
	checked := 0
	oracleWorlds := uint64(0)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := compileDecisionFixture(tc.nodes, tc.filters, tc.excludes)
			gotCan, gotCannot := canEmitDatasetDecision(c, tc.target), canNotEmitDatasetDecision(c, tc.target)
			wantCan, wantCannot, worlds := oracleQueryOutcomes(c, tc.target)
			oracleWorlds += worlds
			if gotCan.Possible != wantCan || gotCannot.Possible != wantCannot {
				t.Fatalf("query mismatch: can=%t/%t cannot=%t/%t alternatives=%+v filters=%+v inputs=%+v", gotCan.Possible, wantCan, gotCannot.Possible, wantCannot, c.Inputs[tc.target].Alternatives, c.Filters, c.Inputs)
			}
			can, cannot := datasetDecisionRefusal(c, tc.target)
			if can.Possible != wantCan || cannot.Possible != (wantCan && wantCannot) {
				t.Fatalf("refusal mismatch: can=%t cannot=%t want can=%t cannot=%t", can.Possible, cannot.Possible, wantCan, wantCan && wantCannot)
			}
			if gotCan.Possible && !queryWorldEmits(c, gotCan.World, tc.target) {
				t.Fatal("CanEmit witness does not emit target")
			}
			if gotCan.Possible {
				actual, err := evaluateDatasetDecisionWorld(c, gotCan.World)
				if err != nil {
					t.Fatal(err)
				}
				want := concreteOracleRecords(c, gotCan.World)
				for i := range want {
					if actual.Records[i].Selected != want[i].Selected || actual.Records[i].Subdir != want[i].Subdir {
						t.Fatalf("accepted world record %d differs from independent selection/attribution oracle: got %+v want %+v", i, actual.Records[i], want[i])
					}
				}
			}
			if gotCannot.Possible {
				e, err := evaluateDatasetDecisionWorld(c, gotCannot.World)
				if err != nil || !e.Records[tc.target].Skipped {
					t.Fatalf("CanNotEmit witness invalid: %+v, %v", e, err)
				}
			}
			checked++
		})
	}
	if checked != len(cases) {
		t.Fatalf("checked %d cases", checked)
	}
	if oracleWorlds == 0 {
		t.Fatal("independent oracle enumerated no admissible worlds")
	}
	t.Logf("independent concrete worlds checked: %d", oracleWorlds)
}

// Independent exhaustive query oracle: enumerate actual roles and every
// collision interpretation, then use the separate test-only concrete matcher
// and emission model; it never calls the production feasibility primitive.
func oracleQueryOutcomes(c datasetDecisionContext, target int) (can, cannot bool, worlds uint64) {
	collisions := oracleCollisionBits(c)
	var residual []uint
	for bit := range collisions {
		if !oracleCoreBit(c, bit) {
			residual = append(residual, bit)
		}
	}
	if len(residual) > 12 {
		return false, false, 0
	}
	for _, core := range oracleCoreAssignments(c) {
		for mask := 0; mask < 1<<len(residual); mask++ {
			w := datasetDecisionWorld{DataLabels: map[int]string{}, Companion: map[int]bool{}, SharedToken: map[uint]bool{}}
			for bit, value := range core {
				w.SharedToken[bit] = value
			}
			for i, bit := range residual {
				w.SharedToken[bit] = mask&(1<<i) != 0
			}
			var visit func(int)
			visit = func(i int) {
				if i == len(c.Inputs) {
					if !oracleWorldValid(c, w, collisions) {
						return
					}
					worlds++
					selected := concreteOracleSelected(c, w, collisions)
					if selected[target] {
						can = true
					} else {
						cannot = true
					}
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
	}
	return
}

func concreteOracleSelected(c datasetDecisionContext, w datasetDecisionWorld, collisions map[uint]bool) []bool {
	direct := make([]bool, len(c.Inputs))
	dirs := map[string]bool{}
	for i, in := range c.Inputs {
		if !in.Excluded {
			direct[i] = concreteOracleDirect(c, w, i, collisions)
			if direct[i] && !w.Companion[i] {
				for d := path.Dir(strings.ReplaceAll(in.Path, "\\", "/")); d != "." && d != "/"; d = path.Dir(d) {
					dirs[d] = true
				}
			}
		}
	}
	selected := make([]bool, len(c.Inputs))
	seenExact, seenFold := map[string]bool{}, map[string]bool{}
	for i, in := range c.Inputs {
		if seenExact[in.Path] {
			continue
		}
		seenExact[in.Path] = true
		if in.Excluded {
			continue
		}
		fold := strings.ToLower(in.Path)
		if seenFold[fold] {
			continue
		}
		if !direct[i] {
			if !w.Companion[i] || in.LFS {
				continue
			}
			d := path.Dir(strings.ReplaceAll(in.Path, "\\", "/"))
			if d != "." && !dirs[d] {
				continue
			}
		}
		selected[i] = true
		seenFold[fold] = true
	}
	return selected
}

func concreteOracleRecords(c datasetDecisionContext, w datasetDecisionWorld) []datasetDecisionRecord {
	collisions := oracleCollisionBits(c)
	selected := concreteOracleSelected(c, w, collisions)
	direct := make([]bool, len(c.Inputs))
	for i := range c.Inputs {
		direct[i] = !c.Inputs[i].Excluded && concreteOracleDirect(c, w, i, collisions)
	}
	out := make([]datasetDecisionRecord, len(c.Inputs))
	for i, in := range c.Inputs {
		out[i].Selected = selected[i]
		out[i].Skipped = !selected[i]
		if !selected[i] {
			continue
		}
		best := -1
		for _, f := range c.Filters {
			if direct[i] && concreteOracleFilterMatch(i, in, f, c, w, collisions) && len(f.Original) > best {
				out[i].Subdir = f.Original
				best = len(f.Original)
			}
		}
	}
	return out
}

func concreteOracleFilterMatch(i int, in datasetDecisionInput, f datasetDecisionFilter, c datasetDecisionContext, w datasetDecisionWorld, collisions map[uint]bool) bool {
	pathMatch := filterMatchesPath(in.Path, f.Original, c.ExactMatch)
	if w.Companion[i] {
		return pathMatch
	}
	recognized := containsString(c.RecognizedLabels, f.Normalized)
	if !recognized {
		return pathMatch
	}
	if collisions[f.Bit] && !w.SharedToken[f.Bit] {
		return pathMatch
	}
	return w.DataLabels[i] == f.Normalized
}

func BenchmarkDatasetDecisionQueryThousandFileInventory(b *testing.B) {
	nodes := make([]hfNode, 1000)
	for i := range nodes {
		nodes[i].Path = fmt.Sprintf("train/part-%04d.parquet", i)
	}
	c := compileDecisionFixture(nodes, []string{"train", ".parquet"}, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = canEmitDatasetDecision(c, 500)
	}
}

func BenchmarkDatasetDecisionCompileAndAllQueries(b *testing.B) {
	for _, size := range []int{100, 1000} {
		b.Run(fmt.Sprintf("files-%d", size), func(b *testing.B) {
			nodes := make([]hfNode, size)
			for i := range nodes {
				nodes[i].Path = fmt.Sprintf("train/part-%04d.parquet", i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				c := compileDecisionFixture(nodes, []string{"train", ".parquet"}, nil)
				for i := range c.Inputs {
					_ = canEmitDatasetDecision(c, i)
					_ = canNotEmitDatasetDecision(c, i)
				}
			}
		})
	}
}

func BenchmarkDatasetDecisionAmbiguousFirstVariableQuery(b *testing.B) {
	nodes := make([]hfNode, 100)
	for i := range nodes {
		nodes[i].Path = fmt.Sprintf("metadata/file-%04d.txt", i)
	}
	c := compileDecisionFixture(nodes, []string{"default"}, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = datasetDecisionRefusal(c, 0)
	}
}

func TestCompileDatasetDecisionContextCopiesEvidence(t *testing.T) {
	evidence := datasetselection.Evidence{
		InputIndex:     0,
		PossibleLabels: []string{"train"},
		DefiniteLabels: []string{"train"},
		PossibleRoles:  []datasetselection.EvidenceRole{datasetselection.EvidenceRoleData},
		DefiniteRole:   datasetselection.EvidenceRoleData,
	}
	membership := datasetselection.Result{Evidence: []datasetselection.Evidence{evidence}}
	c := compileDatasetDecisionContext([]hfNode{{Path: "train.csv"}}, membership, []string{"train"}, nil, true, true, false)
	membership.Evidence[0].PossibleLabels[0] = "changed"
	membership.Evidence[0].PossibleRoles[0] = datasetselection.EvidenceRoleCompanion
	if got := c.Inputs[0].Alternatives; !reflect.DeepEqual(got, []datasetDecisionRoleLabel{{datasetselection.EvidenceRoleData, "train"}}) {
		t.Fatalf("compiled context aliases evidence input: %+v", got)
	}
}

func TestCompileDatasetDecisionContextCoreAndResidualDomain(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "train.csv"}, {Path: "data/custom/chunk.arrow"}, {Path: "default.parquet"}}, []string{"train", "custom", "default"}, nil)
	if len(c.CoreLabels) != 6 || !reflect.DeepEqual(c.ResidualLabels, []string{"custom"}) {
		t.Fatalf("domain core=%v residual=%v", c.CoreLabels, c.ResidualLabels)
	}
	if len(c.Inputs[1].PossibleResidual) != 1 || c.Inputs[1].PossibleResidual[0] != "custom" || len(c.Inputs[1].DefiniteResidual) != 1 {
		t.Fatalf("custom group evidence=%+v", c.Inputs[1])
	}
	if c.Inputs[2].DefiniteCore == 0 {
		t.Fatalf("default definite membership was lost: %+v", c.Inputs[2])
	}
	if !reflect.DeepEqual(c.Inputs[1].GroupResidualConditions, []string{"custom"}) {
		t.Fatalf("custom group token membership=%v", c.Inputs[1].GroupResidualConditions)
	}
}

func TestCompileDatasetDecisionContextIndependentPathForms(t *testing.T) {
	nodes := []hfNode{{Path: "nested/folder/file.csv"}, {Path: "train/data.parquet"}}
	c := compileDecisionFixture(nodes, []string{"nested/", ".csv", "train/data.parquet"}, nil)
	if got := c.Inputs[0].DirectFilterIndexes; !reflect.DeepEqual(got, []int{0, 1}) {
		t.Fatalf("directory and extension matches=%v want [0 1]", got)
	}
	if got := c.Inputs[1].DirectFilterIndexes; !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("full relative path match=%v want [2]", got)
	}
}
