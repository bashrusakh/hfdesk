// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/datasetselection"
)

type blockingDecisionContext struct {
	context.Context
	reached chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (c *blockingDecisionContext) Err() error {
	c.once.Do(func() { close(c.reached); <-c.resume })
	return context.Canceled
}

func TestInterruptedDatasetDecisionCacheCanBeRetried(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "records.parquet"}}, []string{"default"}, nil)
	blocked := &blockingDecisionContext{Context: context.Background(), reached: make(chan struct{}), resume: make(chan struct{})}
	c.ctx = blocked
	result := make(chan datasetDecisionQueryResult, 1)
	go func() {
		got, _ := fixedDatasetDecisionQuery(c, 0, false)
		result <- got
	}()
	<-blocked.reached
	close(blocked.resume)
	if got := <-result; !errors.Is(got.Err, context.Canceled) {
		t.Fatalf("interrupted query error=%v, want context.Canceled", got.Err)
	}
	c.uniqueEvaluation.mu.Lock()
	cached := c.uniqueEvaluation.fixedDone
	c.uniqueEvaluation.mu.Unlock()
	if cached {
		t.Fatal("interrupted fixed-outcome proof was cached")
	}
	c.ctx = context.Background()
	got, fixed := fixedDatasetDecisionQuery(c, 0, false)
	if !fixed || !got.Possible || got.Err != nil {
		t.Fatalf("retry did not complete normally: fixed=%t result=%+v", fixed, got)
	}
}

func compileDecisionFixture(nodes []hfNode, filters, excludes []string) datasetDecisionContext {
	files := make([]datasetselection.File, len(nodes))
	for i := range nodes {
		files[i] = datasetselection.File{Path: nodes[i].Path}
	}
	return compileDatasetDecisionContext(nodes, datasetselection.Recognize(files), filters, excludes, true, true, false)
}

func TestDatasetKnownMetadataNamesAreCompanionOnlyInScope(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "train.csv"},
		{Path: "tokenizer.json"},
		{Path: "docs/dataset_info.json"},
		{Path: "nested/dataset_info.json"},
		{Path: "data/dataset_infos.json"},
		{Path: "test/config.json"},
		{Path: "nested/config.json"},
	}, []string{"train"}, nil)
	for i := 1; i <= 4; i++ {
		got := c.Inputs[i].Alternatives
		if len(got) != 1 || got[0].Role != datasetselection.EvidenceRoleCompanion {
			t.Fatalf("in-scope known metadata %q alternatives = %+v; want companion-only", c.Inputs[i].Path, got)
		}
	}
	for i := 5; i <= 6; i++ {
		got := c.Inputs[i].Alternatives
		if len(got) == 1 && got[0].Role == datasetselection.EvidenceRoleCompanion {
			t.Fatalf("out-of-scope known metadata %q was forced companion-only: %+v", c.Inputs[i].Path, got)
		}
	}
	if got := c.Inputs[6].Alternatives; len(got) < 2 || got[0].Role != datasetselection.EvidenceRoleData || got[1].Role != datasetselection.EvidenceRoleCompanion {
		t.Fatalf("arbitrary nested config alternatives = %+v; want data/companion ambiguity", got)
	}

	excluded := compileDecisionFixture([]hfNode{{Path: "train.csv"}, {Path: "docs/dataset_info.json"}}, []string{"train"}, []string{"dataset_info.json"})
	if !excluded.Inputs[1].Excluded {
		t.Fatal("explicit exclude did not dominate scoped dataset metadata evidence")
	}
}

func TestDatasetKnownConfigRoleScope(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "weights/model.gguf"},
		{Path: "config.json"},
		{Path: "test/config.json"},
	}, []string{"weights/model.gguf"}, nil)
	target := 1
	if got := c.Inputs[target].Alternatives; len(got) != 1 || got[0].Role != datasetselection.EvidenceRoleCompanion {
		t.Fatalf("root config alternatives = %+v; want companion-only", got)
	}
	companion := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleCompanion}
	world := mustDecisionWorld(t, c, map[int]datasetDecisionRoleLabel{target: companion}, nil)
	evaluation, err := evaluateDatasetDecisionWorld(c, world)
	if err != nil {
		t.Fatal(err)
	}
	if !evaluation.Records[target].Selected {
		t.Fatalf("root config companion was not selected: %+v", evaluation.Records)
	}
	nested := c.Inputs[2].Alternatives
	if len(nested) == 1 && nested[0].Role == datasetselection.EvidenceRoleCompanion {
		t.Fatalf("test/config.json was forced companion-only: %+v", nested)
	}
	for _, cached := range []bool{true, false} {
		q := c
		if !cached {
			q.uniqueEvaluation = nil
		}
		can, cannot := datasetDecisionRefusal(q, target)
		if !can.Possible || cannot.Possible || can.Err != nil || cannot.Err != nil {
			t.Fatalf("cached=%t config feasibility can/cannot=%t/%t errors=%v/%v; want possible/impossible", cached, can.Possible, cannot.Possible, can.Err, cannot.Err)
		}
	}
}

func TestCanEmitDatasetDecisionStopsAtProvenNonEmission(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "train/part.parquet"},
		{Path: "test/part.parquet"},
	}, []string{"train"}, nil)

	// The fixed proof says the second record can never emit. Its negative
	// answer is conclusive; the query must not fall through to the general
	// feasibility solver and reinterpret it as an unknown.
	if got := canEmitDatasetDecision(c, 1); got.Possible {
		t.Fatalf("non-emitting test record got an emission witness: %+v", got)
	}
	if got := canEmitDatasetDecision(c, 0); !got.Possible || got.Record.Subdir != "train" {
		t.Fatalf("selected train record = %+v, want train emission", got)
	}
}

func TestQueryDatasetGroupSelectionCancellationIsErrorNotNegativeProof(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "train_test.csv"}}, []string{"train"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.ctx = ctx
	got := queryDatasetGroupSelection(c)
	if !errors.Is(got.Err, context.Canceled) || got.Possible {
		t.Fatalf("cancelled group-selection proof = %+v, want cancellation error", got)
	}
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

func TestFeasibleDatasetDecisionWorldHonorsPinsIneligibilityAndPositiveSeed(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "train.csv"}, {Path: "train/meta.json"}, {Path: "other/meta.json"}}, []string{"train"}, nil)
	train := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleData, Label: "train"}
	companion := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleCompanion}
	// A direct data file cannot be required ineligible while the recognized
	// group filter selects it; an unseeded nested companion is ineligible.
	if _, ok := feasibleAnyCoreDecisionWorld(c, datasetDecisionPins{0: train}, []int{0}, nil, nil); ok {
		t.Fatal("pinned direct data unexpectedly ineligible")
	}
	if _, ok := feasibleAnyCoreDecisionWorld(c, datasetDecisionPins{2: companion}, []int{2}, nil, nil); !ok {
		t.Fatal("pinned companion should permit required ineligibility")
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
								for _, ineligibleIndex := range []int{-1, (i + 2) % len(c.Inputs)} {
									var ineligible []int
									if ineligibleIndex >= 0 {
										ineligible = []int{ineligibleIndex}
									}
									pins := datasetDecisionPins{i: pin}
									for _, core := range oracleCoreAssignments(c) {
										want, oracleWorld := enumerateDecisionFeasibility(c, core, pins, ineligible, zeros, positive)
										gotWorld, got := feasibleDatasetDecisionWorld(c, core, pins, ineligible, zeros, positive)
										queries++
										if got != want {
											t.Errorf("solver/oracle mismatch filters=%q core=%v roles=%+v pins=%v ineligible=%v zeroDirect=%v positive=%+v: solver=%v oracle=%v witness=%+v", tc.filters, core, c.Inputs, pins, ineligible, zeros, positive, got, want, gotWorld)
											continue
										}
										if got && !decisionWitnessSatisfies(c, gotWorld, core, pins, ineligible, zeros, positive) {
											t.Errorf("invalid solver witness: filters=%q core=%v pins=%v ineligible=%v zeroDirect=%v positive=%+v witness=%+v", tc.filters, core, pins, ineligible, zeros, positive, gotWorld)
										}
										if want && !decisionWitnessSatisfies(c, oracleWorld, core, pins, ineligible, zeros, positive) {
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

func TestRequiredIneligibilityUsesAllowlist(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "train.csv"}, {Path: "config.json"}, {Path: "nested/notes.txt"}}, []string{"train"}, nil)
	rootCompanion := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleCompanion}
	if _, ok := feasibleAnyCoreDecisionWorld(c, datasetDecisionPins{1: rootCompanion}, []int{1}, nil, nil); ok {
		t.Fatal("root non-LFS companion cannot satisfy required ineligibility")
	}
	if _, ok := feasibleAnyCoreDecisionWorld(c, datasetDecisionPins{2: rootCompanion}, []int{2}, nil, nil); !ok {
		t.Fatal("unseeded nested ambiguous companion should satisfy required ineligibility")
	}
	definite := compileDecisionFixture([]hfNode{{Path: "train.csv"}, {Path: "nested/notes.md"}}, []string{"train"}, nil)
	if _, ok := feasibleAnyCoreDecisionWorld(definite, datasetDecisionPins{1: rootCompanion}, []int{1}, nil, nil); !ok {
		t.Fatal("arbitrarily nested docs path must be skippable")
	}
	if _, ok := feasibleAnyCoreDecisionWorld(c, datasetDecisionPins{99: rootCompanion}, nil, nil, nil); ok {
		t.Fatal("pin to unknown input accepted")
	}
	seeded := compileDecisionFixture([]hfNode{{Path: "train/data.csv"}, {Path: "partition/meta.json"}}, []string{"train"}, nil)
	positiveSeed := &datasetDecisionPositive{Input: 0, Alternative: seeded.Inputs[0].Alternatives[0]}
	if _, ok := feasibleAnyCoreDecisionWorld(seeded, datasetDecisionPins{1: rootCompanion}, []int{1}, nil, positiveSeed); !ok {
		t.Fatal("unknown companion must remain skippable next to a direct data source")
	}
	lfs := compileDecisionFixture([]hfNode{{Path: "train.csv"}, {Path: "other/weights.bin", LFS: &hfLfsInfo{Size: 3}}}, []string{"train"}, nil)
	if _, ok := feasibleAnyCoreDecisionWorld(lfs, datasetDecisionPins{1: rootCompanion}, []int{1}, nil, nil); !ok {
		t.Fatal("non-direct LFS companion should be ineligible")
	}
	directLFS := compileDecisionFixture([]hfNode{{Path: "other/train/weights.bin", LFS: &hfLfsInfo{Size: 3}}}, []string{"train"}, nil)
	if _, ok := feasibleAnyCoreDecisionWorld(directLFS, datasetDecisionPins{0: rootCompanion}, []int{0}, nil, nil); ok {
		t.Fatal("directly matched LFS companion is eligible")
	}
	excludedSeed := compileDecisionFixture([]hfNode{{Path: "partition/train.csv"}, {Path: "partition/notes.txt"}}, []string{"train"}, []string{"train.csv"})
	train := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleData, Label: "train"}
	if _, ok := feasibleAnyCoreDecisionWorld(excludedSeed, datasetDecisionPins{1: rootCompanion}, []int{1}, nil, nil); !ok {
		t.Fatal("excluded direct candidate must not seed companion scope")
	}
	if _, ok := feasibleAnyCoreDecisionWorld(excludedSeed, datasetDecisionPins{0: train}, []int{0}, nil, nil); !ok {
		t.Fatal("excluded file is skipped regardless of its role or direct match")
	}
}

func TestPositiveDirectDoesNotNeedSharedCustomBitChoice(t *testing.T) {
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
	if !ok {
		t.Fatalf("positive custom membership must remain feasible: ok=%v witness=%+v bit=%d", ok, w, bit)
	}
}

func TestZeroDirectUsesEveryIndependentGroupTerm(t *testing.T) {
	// The literal path matches both filters even when the chosen data group is
	// validation, so a membership interpretation cannot make it ineligible.
	c := compileDecisionFixture([]hfNode{{Path: "train/test/validation/data.csv"}}, []string{"train", "test"}, nil)
	validation := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleData, Label: "validation"}
	pins := datasetDecisionPins{0: validation}
	zeros := []int{0}
	core := map[uint]bool{c.FilterBits["train"]: true, c.FilterBits["test"]: true}
	want, oracle := enumerateDecisionFeasibility(c, core, pins, nil, zeros, nil)
	gotWorld, got := feasibleDatasetDecisionWorld(c, core, pins, nil, zeros, nil)
	if want || got {
		t.Fatalf("a literal train/test path cannot satisfy zero-direct: filters=%v roles=%+v pins=%v coreUnits=%v zeroDirect=%v expected=%v oracleWorld=%+v solver=%v witness=%+v", c.Filters, c.Inputs, pins, core, zeros, want, oracle, got, gotWorld)
	}
	contradictoryCore := map[uint]bool{c.FilterBits["train"]: false, c.FilterBits["test"]: true}
	want, _ = enumerateDecisionFeasibility(c, contradictoryCore, pins, nil, zeros, nil)
	if want {
		t.Fatal("oracle found zero-direct despite the literal train path match")
	}
	if _, ok := feasibleDatasetDecisionWorld(c, contradictoryCore, pins, nil, zeros, nil); ok {
		t.Fatal("primitive ignored the literal path match")
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
	if want || got || duplicate.Filters[0].Bit != duplicate.Filters[1].Bit {
		t.Fatalf("case-normalized duplicate literal terms cannot be suppressed: oracle=%v solver=%v world=%+v filters=%+v", want, got, gotWorld, duplicate.Filters)
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
// terms/raw eligibility with an independent concrete reference, never through
// the symbolic solver, literal compiler, or production world evaluator.
func enumerateDecisionFeasibility(c datasetDecisionContext, coreAssignment map[uint]bool, pins datasetDecisionPins, ineligible, zeros []int, positive *datasetDecisionPositive) (bool, datasetDecisionWorld) {
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
				if len(ineligible) > 0 {
					eligible := concreteOracleEligible(c, w, collisions)
					for _, index := range ineligible {
						if index < 0 || index >= len(eligible) || eligible[index] {
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
func enumerateAnyCoreDecisionFeasibility(c datasetDecisionContext, pins datasetDecisionPins, ineligible, zeros []int, positive *datasetDecisionPositive) (bool, datasetDecisionWorld) {
	for _, core := range oracleCoreAssignments(c) {
		if ok, w := enumerateDecisionFeasibility(c, core, pins, ineligible, zeros, positive); ok {
			return true, w
		}
	}
	return false, datasetDecisionWorld{}
}
func feasibleAnyCoreDecisionWorld(c datasetDecisionContext, pins datasetDecisionPins, ineligible, zeros []int, positive *datasetDecisionPositive) (datasetDecisionWorld, bool) {
	for _, core := range oracleCoreAssignments(c) {
		if w, ok := feasibleDatasetDecisionWorld(c, core, pins, ineligible, zeros, positive); ok {
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
		} else {
			matched = pathMatch || w.DataLabels[i] == filter.Normalized
		}
		if matched {
			return true
		}
	}
	return false
}

// oracleDatasetPayloadPath is an independent reference for dataset companion
// payload exclusion. It reimplements name/extension recognition rather than
// calling isDatasetPayloadPath, so a production predicate bug cannot be
// mirrored by the oracle it is meant to check.
func oracleDatasetPayloadPath(filePath string) bool {
	base := strings.ToLower(path.Base(strings.ReplaceAll(filePath, "\\", "/")))
	stem := strings.TrimSuffix(base, path.Ext(base))
	tokens := strings.FieldsFunc(stem, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == ' '
	})
	for i, token := range tokens {
		if token == "optimizer" {
			return true
		}
		if i+1 < len(tokens) && (token == "training" || token == "trainer") {
			next := tokens[i+1]
			if next == "args" || next == "state" || next == "states" {
				return true
			}
		}
	}
	switch path.Ext(base) {
	case ".safetensors", ".onnx", ".pt", ".pth", ".msgpack", ".h5", ".tflite", ".ot", ".gguf", ".gguf-split", ".ggml", ".ckpt":
		return true
	case ".bin":
		// Tokenizer binaries are small dataset companions; other binaries are
		// model payload by the same established model-selection convention.
		return !strings.Contains(base, "tokenizer")
	}
	return false
}

// oracleKnownDatasetCompanionPath independently reimplements the positive
// dataset companion allowlist for the oracle. It must not call the production
// allowlist/predicate helpers.
func oracleKnownDatasetCompanionPath(filePath string) bool {
	p := strings.TrimPrefix(strings.ReplaceAll(filePath, "\\", "/"), "./")
	base := strings.ToLower(path.Base(p))
	if oracleReadmeOrLicense(base) || oracleDatasetInfo(base) {
		return true
	}
	if !oracleConventionalCompanionDirectory(p) {
		return false
	}
	switch base {
	case "config.json", "tokenizer.json", "tokenizer_config.json",
		"special_tokens_map.json", "added_tokens.json", "vocab.json",
		"merges.txt", "vocab.txt",
		"pyproject.toml", "setup.py", "setup.cfg", "environment.yml",
		"environment.yaml", "dockerfile", "makefile", ".gitattributes",
		".gitignore", "tokenizer.bin", "copying", "notice":
		return true
	}
	if strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt") {
		return true
	}
	if strings.HasPrefix(base, "environment") &&
		(strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml")) {
		return true
	}
	switch path.Ext(base) {
	case ".model", ".spm", ".tiktoken", ".vocab", ".merges", ".jinja", ".j2",
		".md", ".markdown", ".rst", ".yaml", ".yml", ".toml", ".ini",
		".cfg", ".conf", ".py", ".sh", ".bash", ".zsh", ".bat", ".ps1",
		".sql", ".html", ".css", ".js", ".ts", ".ipynb", ".cff":
		return true
	}
	return false
}

func oracleReadmeOrLicense(base string) bool {
	if strings.HasPrefix(base, "readme") {
		if base == "readme" {
			return true
		}
		switch path.Ext(base) {
		case ".md", ".markdown", ".rst", ".txt":
			return true
		}
	}
	if base == "license" {
		return true
	}
	rest, ok := strings.CutPrefix(base, "license")
	if !ok {
		return false
	}
	if strings.HasPrefix(rest, "-") {
		return len(rest) > 1
	}
	switch rest {
	case ".md", ".markdown", ".rst", ".txt":
		return true
	}
	return false
}

func oracleDatasetInfo(base string) bool {
	return base == "dataset_info.json" || base == "dataset_infos.json"
}

func oracleConventionalCompanionDirectory(filePath string) bool {
	p := strings.TrimPrefix(strings.ReplaceAll(filePath, "\\", "/"), "./")
	dir := path.Dir(p)
	if dir == "." || dir == "" {
		return true
	}
	first, _, _ := strings.Cut(dir, "/")
	switch strings.ToLower(first) {
	case "docs", "doc", "notebooks", "scripts", "tools", "utils", "examples", "configs", "config", "tokenizer":
		return true
	}
	return false
}

// concreteOracleSkipped is a deliberately small independent reference for
// the evaluator's direct-selection and input-order retention rules. It does
// not call the production world evaluator or validate helper.
func concreteOracleEligible(c datasetDecisionContext, w datasetDecisionWorld, collisions map[uint]bool) []bool {
	n := len(c.Inputs)
	eligible := make([]bool, n)
	for i, in := range c.Inputs {
		if in.Excluded {
			continue
		}
		if concreteOracleDirect(c, w, i, collisions) {
			eligible[i] = true
			continue
		}
		if !w.Companion[i] || in.LFS || oracleDatasetPayloadPath(in.Path) || !oracleKnownDatasetCompanionPath(in.Path) {
			continue
		}
		eligible[i] = true
	}
	return eligible
}

// concreteOracleSkipped adds exact/case-folded output suppression after the
// independent raw eligibility oracle; it never defines kernel eligibility.
func concreteOracleSkipped(c datasetDecisionContext, w datasetDecisionWorld, collisions map[uint]bool) []bool {
	eligible := concreteOracleEligible(c, w, collisions)
	skipped := make([]bool, len(c.Inputs))
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
		if !eligible[i] {
			skipped[i] = true
			continue
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
func decisionWitnessSatisfies(c datasetDecisionContext, w datasetDecisionWorld, core map[uint]bool, pins datasetDecisionPins, ineligible, zeros []int, positive *datasetDecisionPositive) bool {
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
	if len(ineligible) > 0 {
		eligible := concreteOracleEligible(c, w, collisions)
		for _, i := range ineligible {
			if i < 0 || i >= len(eligible) || eligible[i] {
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

func TestCompileDatasetDecisionContextPathPredicatesAndDuplicates(t *testing.T) {
	nodes := []hfNode{
		{Path: "root.csv"}, {Path: "folder/train.csv"}, {Path: "folder/sub/companion.json"},
		{Path: "other/companion.json"}, {Path: "folder/train.csv"}, {Path: "FOLDER/train.csv"},
	}
	c := compileDecisionFixture(nodes, []string{"train.csv"}, []string{"other/"})
	if !reflect.DeepEqual(c.Inputs[1].DirectFilterIndexes, []int{0}) {
		t.Fatalf("full-path direct match=%v", c.Inputs[1].DirectFilterIndexes)
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

func TestKnownCompanionRoleOverrideClearsDirectDataConditions(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "docs/train/config.json"},
		{Path: "test/train/config.json"},
	}, []string{"train"}, nil)
	inScope := c.Inputs[0]
	if len(inScope.Alternatives) != 1 || inScope.Alternatives[0].Role != datasetselection.EvidenceRoleCompanion {
		t.Fatalf("in-scope config alternatives = %+v; want companion-only", inScope.Alternatives)
	}
	if inScope.DirectCoreConditions != 0 || len(inScope.DirectResidualConditions) != 0 {
		t.Fatalf("in-scope companion retained stale direct data state: %+v", inScope)
	}
	if c.Inputs[1].DirectCoreConditions == 0 {
		t.Fatalf("out-of-scope config unexpectedly lost direct data state: %+v", c.Inputs[1])
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
	if got.Records[2].Selected || !got.Records[2].Skipped {
		t.Fatalf("unknown nested non-LFS companion must be skipped: %+v", got.Records[2])
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
	if !a.Records[0].Selected || !b.Records[0].Selected || !a.Records[1].Selected || !b.Records[1].Selected {
		t.Fatalf("literal matches must survive either membership interpretation: membership=%+v ordinary=%+v", a.Records, b.Records)
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

func TestConcreteWorldMembershipSelectionDoesNotRetainUnknownNestedCompanion(t *testing.T) {
	c := compileDecisionFixture([]hfNode{{Path: "partition/train_dataset.csv"}, {Path: "partition/metadata.json"}}, []string{"train"}, nil)
	w := mustDecisionWorld(t, c, map[int]datasetDecisionRoleLabel{1: {Role: datasetselection.EvidenceRoleCompanion}}, nil)
	got, err := evaluateDatasetDecisionWorld(c, w)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Records[0].Selected || got.Records[1].Selected || !got.Records[1].Skipped {
		t.Fatalf("unknown nested companion must not inherit membership scope: %+v", got.Records)
	}
}

func TestConcreteWorldKnownCompanionDoesNotBroadenToUnknownNested(t *testing.T) {
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
	if !got.Records[1].Selected || got.Records[2].Selected || !got.Records[2].Skipped {
		t.Fatalf("known companion must not retain an unknown sibling: %+v", got.Records)
	}
}

func TestStableEvaluationWithMultipleDirectRoleAlternatives(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "train.csv"},
		{Path: "folder/a.json"},
		{Path: "folder/b.json"},
		{Path: "folder/README.md"},
	}, []string{"train", ".json"}, nil)
	for i := 1; i <= 2; i++ {
		if len(c.Inputs[i].Alternatives) < 2 || len(c.Inputs[i].DirectFilterIndexes) == 0 {
			t.Fatalf("fixture lacks a directly matched multi-role input at %d: %+v", i, c.Inputs[i])
		}
	}
	if !stableDatasetDecisionEvaluation(c) {
		t.Fatal("role-invariant direct matches should admit a reusable evaluation")
	}
	for _, roles := range []map[int]datasetDecisionRoleLabel{
		{1: {Role: datasetselection.EvidenceRoleData, Label: "default"}, 2: {Role: datasetselection.EvidenceRoleData, Label: "default"}},
		{1: {Role: datasetselection.EvidenceRoleCompanion}, 2: {Role: datasetselection.EvidenceRoleCompanion}},
		{1: {Role: datasetselection.EvidenceRoleData, Label: "default"}, 2: {Role: datasetselection.EvidenceRoleCompanion}},
	} {
		w := mustDecisionWorld(t, c, roles, nil)
		actual, err := evaluateDatasetDecisionWorld(c, w)
		if err != nil {
			t.Fatal(err)
		}
		want := concreteOracleRecords(c, w)
		for i := range want {
			if actual.Records[i].Selected != want[i].Selected || actual.Records[i].Subdir != want[i].Subdir {
				t.Fatalf("roles=%v record %d got %+v want %+v", roles, i, actual.Records[i], want[i])
			}
		}
	}
	for i := range c.Inputs {
		gotCan, gotCannot := datasetDecisionRefusal(c, i)
		wantCan, wantCannot, worlds := oracleQueryOutcomes(c, i)
		if worlds == 0 || gotCan.Possible != wantCan || gotCannot.Possible != (wantCan && wantCannot) {
			t.Fatalf("input %d query=%t/%t oracle=%t/%t worlds=%d", i, gotCan.Possible, gotCannot.Possible, wantCan, wantCan && wantCannot, worlds)
		}
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
	if !got.Records[0].Selected || !got.Records[1].Selected || !got.Records[2].Selected || !got.Records[3].Selected {
		t.Fatalf("literal path matches must remain selected alongside custom memberships: %+v", got.Records)
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

func TestSuppressedDirectCaseDuplicateKeepsKnownCompanionIndependent(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "Fold/train.csv"},
		{Path: "fold/TRAIN.CSV"},
		{Path: "fold/README.md"},
	}, []string{"train"}, nil)
	train := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleData, Label: "train"}
	companion := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleCompanion}
	w := mustDecisionWorld(t, c, map[int]datasetDecisionRoleLabel{0: train, 1: train, 2: companion}, nil)
	got, err := evaluateDatasetDecisionWorld(c, w)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Records[1].Eligible || got.Records[1].Selected || !got.Records[2].Selected {
		t.Fatalf("known companion must remain selected independent of duplicate scope: %+v", got.Records)
	}
	want := concreteOracleRecords(c, w)
	if got.Records[1] != want[1] || got.Records[2] != want[2] {
		t.Fatalf("suppressed source / companion mismatch: got=%+v/%+v oracle=%+v/%+v", got.Records[1], got.Records[2], want[1], want[2])
	}
}

func TestFeasibilityQueriesRespectJointSkipAndEmissionConstraints(t *testing.T) {
	cases := []struct {
		name   string
		nodes  []hfNode
		filter []string
		target int
	}{
		{"exact-duplicate-suppression", []hfNode{{Path: "train.csv"}, {Path: "folder/train.csv"}, {Path: "folder/notes.bin", LFS: &hfLfsInfo{Size: 4}}, {Path: "folder/notes.bin"}, {Path: "folder/NOTES.BIN"}, {Path: "notes.json"}}, []string{"train"}, 3},
		{"required-skip-conflicts-with-unique-witness", []hfNode{{Path: "train.csv"}, {Path: "README.md"}}, []string{"train"}, 0},
		{"root-default-data-or-companion-final-emission", []hfNode{{Path: "records.parquet"}, {Path: "notes.json"}}, []string{"default"}, 1},
		{"case-winner-must-remain-eligible", []hfNode{{Path: "train.csv"}, {Path: "folder/notes.bin", LFS: &hfLfsInfo{Size: 4}}, {Path: "folder/NOTES.BIN"}}, []string{"train"}, 2},
		{"ambiguous-lfs-prefix-before-case-winner", []hfNode{{Path: "records.parquet"}, {Path: "notes.json", LFS: &hfLfsInfo{Size: 4}}, {Path: "NOTES.JSON", LFS: &hfLfsInfo{Size: 4}}, {Path: "Notes.Json"}}, []string{"default"}, 3},
		{"role-pinned-companion-seed", []hfNode{{Path: "folder/train.csv"}, {Path: "folder/notes.json"}}, []string{"train"}, 1},
		{"unmasked-role-ambiguity", []hfNode{{Path: "folder/notes.json"}}, []string{"default"}, 0},
		{"ordinary-direct-companion", []hfNode{{Path: "train.csv"}, {Path: "docs/README.md"}}, []string{"train", "docs/"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := compileDecisionFixture(tc.nodes, tc.filter, nil)
			for _, cached := range []bool{true, false} {
				q := c
				if !cached {
					q.uniqueEvaluation = nil
				}
				checkDatasetDecisionAgainstOracle(t, q)
				for i := range q.Inputs {
					can := canEmitDatasetDecision(q, i)
					wantCan, wantCannot, worlds := oracleQueryOutcomes(q, i)
					cannot := canNotEmitDatasetDecision(q, i)
					refusalCan, refusalCannot := datasetDecisionRefusal(q, i)
					if worlds == 0 || can.Possible != wantCan || cannot.Possible != wantCannot || refusalCan.Possible != wantCan || refusalCannot.Possible != (wantCan && wantCannot) {
						t.Fatalf("cached=%t input=%d can/cannot=%t/%t refusal=%t/%t oracle=%t/%t worlds=%d", cached, i, can.Possible, cannot.Possible, refusalCan.Possible, refusalCannot.Possible, wantCan, wantCannot, worlds)
					}
					if cannot.Possible {
						e, err := evaluateDatasetDecisionWorld(q, cannot.World)
						if err != nil || e.Records[i].Selected {
							t.Fatalf("invalid skip witness: %+v, %v", e, err)
						}
					}
				}
			}
		})
	}
	c := compileDecisionFixture([]hfNode{{Path: "train.csv"}, {Path: "README.md"}}, []string{"train"}, nil)
	for _, cached := range []bool{true, false} {
		q := c
		if !cached {
			q.uniqueEvaluation = nil
		}
		got := canSelectDatasetDecision(q, 1, []int{0})
		if got.Possible {
			t.Fatalf("cached=%t unique-world shortcut ignored required earlier skip", cached)
		}
	}
	// A caller-supplied skip of an actually selected record remains a real
	// contradiction even when fixed emission outcomes are cached.
	train := compileDecisionFixture([]hfNode{{Path: "train.csv"}, {Path: "notes.json"}}, []string{"train", ".json"}, nil)
	for _, cached := range []bool{true, false} {
		q := train
		if !cached {
			q.uniqueEvaluation = nil
		}
		got := canSelectDatasetDecision(q, 1, []int{0})
		if got.Possible {
			t.Fatalf("cached=%t accepted contradictory required skip", cached)
		}
	}
}

func TestCanEmitConstrainsAllCaseFoldedPredecessors(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "records.parquet"},
		{Path: "notes.json", LFS: &hfLfsInfo{Size: 4}},
		{Path: "NOTES.JSON", LFS: &hfLfsInfo{Size: 4}},
		{Path: "Notes.Json"},
	}, []string{"default"}, nil)
	for _, cached := range []bool{true, false} {
		q := c
		if !cached {
			q.uniqueEvaluation = nil
		}
		got := canEmitDatasetDecision(q, 3)
		if !got.Possible {
			t.Fatalf("cached=%t expected the final first-eligible case variant to emit", cached)
		}
		e, err := evaluateDatasetDecisionWorld(q, got.World)
		if err != nil {
			t.Fatal(err)
		}
		if e.Records[1].Selected || e.Records[2].Selected || !e.Records[3].Selected {
			t.Fatalf("cached=%t emission witness violates ordered prefix: %+v", cached, e.Records)
		}
		wantCan, _, worlds := oracleQueryOutcomes(q, 3)
		if worlds == 0 || !wantCan {
			t.Fatalf("cached=%t independent complete-world oracle disagrees: can=%t worlds=%d", cached, wantCan, worlds)
		}
	}
}

func TestRequiredSkippedDuplicateMayStillBeDirectlyEligible(t *testing.T) {
	for _, tc := range []struct {
		name  string
		later string
	}{
		{name: "exact", later: "train.csv"},
		{name: "case-folded", later: "TRAIN.CSV"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := compileDecisionFixture([]hfNode{{Path: "train.csv"}, {Path: tc.later}, {Path: "notes.json"}}, []string{"train"}, nil)
			for _, cached := range []bool{true, false} {
				q := c
				if !cached {
					q.uniqueEvaluation = nil
				}
				got := canSkipDatasetDecision(q, 1)
				if !got.Possible {
					t.Fatal("expected a skipped-output witness for the duplicate")
				}
				e, err := evaluateDatasetDecisionWorld(q, got.World)
				if err != nil {
					t.Fatal(err)
				}
				if !e.Records[0].Selected || !e.Records[1].Skipped {
					t.Fatalf("cached=%t witness did not preserve first-emission suppression: %+v", cached, e.Records)
				}
				if !decisionHasDirect(q, got.World, 1) {
					t.Fatal("skipped duplicate witness incorrectly forced the record to be ineligible")
				}
				train := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleData, Label: "train"}
				if _, ok := feasibleAnyCoreDecisionWorld(q, datasetDecisionPins{1: train}, []int{1}, nil, nil); ok {
					t.Fatal("lower eligibility kernel accepted a directly matched duplicate as ineligible")
				}
			}
		})
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
		{"direct-companion-directory-seed", []hfNode{{Path: "train.csv"}, {Path: "other/README.md"}, {Path: "other/LICENSE"}}, []string{"train", "README"}, nil, 2},
		{"direct-lfs-companion-directory-seed", []hfNode{{Path: "train.csv"}, {Path: "docs/README.md", LFS: &hfLfsInfo{Size: 3}}, {Path: "docs/LICENSE"}}, []string{"train", ".md"}, nil, 2},
		{"attribution-tie", []hfNode{{Path: "dir/test.csv"}}, []string{".csv", "test"}, nil, 0},
		{"excluded-target", []hfNode{{Path: "train.csv"}, {Path: "test/train.csv"}}, []string{"train", "test"}, []string{"test/train.csv"}, 1},
		{"case-winner", []hfNode{{Path: "Train/a.csv"}, {Path: "train/A.CSV"}}, []string{"train", ".csv"}, nil, 1},
		{"excluded-case-winner", []hfNode{{Path: "Train/a.csv"}, {Path: "train/A.CSV"}}, []string{"train", ".csv"}, []string{"Train/a.csv"}, 1},
		{"case-winner-first-eligible-not-first-member", []hfNode{{Path: "train.csv"}, {Path: "metadata.json", LFS: &hfLfsInfo{Size: 4}}, {Path: "METADATA.JSON"}}, []string{"train"}, nil, 2},
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

func TestStableDirectEvaluationWithRoleAndSharedTokenAlternatives(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "test/train.csv"},
		{Path: "notes.json"},
	}, []string{"test", "train", ".csv", ".json"}, nil)
	if len(c.Inputs[1].Alternatives) < 2 {
		t.Fatalf("fixture did not contain role ambiguity: %+v", c.Inputs[1].Alternatives)
	}
	if len(collisionTokenBits(c)) == 0 {
		t.Fatal("fixture did not contain a shared-token collision")
	}
	if !stableDirectEvaluation(c) {
		t.Fatalf("explicit accepted filter union was not certified stable: %+v", c.Inputs)
	}
	for i := range c.Inputs {
		gotCan, gotCannot := datasetDecisionRefusal(c, i)
		wantCan, wantCannot, _ := oracleQueryOutcomes(c, i)
		if gotCan.Possible != wantCan || gotCannot.Possible != (wantCan && wantCannot) {
			t.Fatalf("input %d refusal=%t/%t oracle=%t/%t", i, gotCan.Possible, gotCannot.Possible, wantCan, wantCannot)
		}
	}
	first, second := c.Inputs[0].Alternatives[0], c.Inputs[0].Alternatives[1]
	companion := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleCompanion}
	shared := map[uint]bool{c.FilterBits["test"]: true, c.FilterBits["train"]: true}
	one, err := datasetDecisionWorldFromAlternatives(c, map[int]datasetDecisionRoleLabel{0: first, 1: companion}, shared)
	if err != nil {
		t.Fatal(err)
	}
	two, err := datasetDecisionWorldFromAlternatives(c, map[int]datasetDecisionRoleLabel{0: second, 1: companion}, shared)
	if err != nil {
		t.Fatal(err)
	}
	firstEval, err := evaluateDatasetDecisionWorld(c, one)
	if err != nil {
		t.Fatal(err)
	}
	secondEval, err := evaluateDatasetDecisionWorld(c, two)
	if err != nil {
		t.Fatal(err)
	}
	if !firstEval.Records[0].Selected || !secondEval.Records[0].Selected {
		t.Fatalf("role alternatives must preserve literal selection: first=%+v second=%+v", firstEval.Records[0], secondEval.Records[0])
	}
}

func TestStableRoleAmbiguityDoesNotRequireEveryInputDirect(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "train/data.parquet"},
		{Path: "notes.json"},
		{Path: "README.md"},
		{Path: "unselected.bin"},
		{Path: "test/train.csv"},
	}, []string{"train", ".parquet", ".json"}, []string{"test/train.csv"})
	if len(c.Inputs[1].Alternatives) < 2 {
		t.Fatalf("fixture must preserve default data/companion ambiguity: %+v", c.Inputs[1].Alternatives)
	}
	if len(collisionTokenBits(c)) != 0 {
		t.Fatalf("excluded multi-alternative input must not create an active collision: %v", collisionTokenBits(c))
	}
	if !stableDatasetDecisionEvaluation(c) {
		t.Fatalf("role ambiguity masked by explicit path match should be certified: %+v", c.Inputs)
	}
	if stableDirectEvaluation(c) {
		t.Fatal("proof must not require every active input to be directly selected")
	}
	for i := range c.Inputs {
		gotCan, gotCannot := datasetDecisionRefusal(c, i)
		wantCan, wantCannot, _ := oracleQueryOutcomes(c, i)
		if gotCan.Possible != wantCan || gotCannot.Possible != (wantCan && wantCannot) {
			t.Fatalf("input %d refusal=%t/%t oracle=%t/%t", i, gotCan.Possible, gotCannot.Possible, wantCan, wantCannot)
		}
		if gotCan.Possible {
			actual, err := evaluateDatasetDecisionWorld(c, gotCan.World)
			if err != nil {
				t.Fatal(err)
			}
			want := concreteOracleRecords(c, gotCan.World)
			for j := range want {
				if actual.Records[j] != want[j] {
					t.Fatalf("input %d accepted witness record %d=%+v oracle=%+v", i, j, actual.Records[j], want[j])
				}
			}
		}
	}
}

func TestUniqueDatasetDecisionWorldKeepsPartialInventoryFast(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "train.csv"},
		{Path: "README.md"},
		{Path: "raw.bin"},
	}, []string{"train"}, []string{"raw.bin"})
	if !uniqueDatasetDecisionWorld(c) {
		t.Fatalf("singleton roles without token collisions should have one world: %+v", c.Inputs)
	}
	if stableDirectEvaluation(c) {
		t.Fatal("fixture must include ordinary unmatched/excluded entries, not rely on all-direct proof")
	}
	for i := range c.Inputs {
		gotCan, gotCannot := datasetDecisionRefusal(c, i)
		wantCan, wantCannot, _ := oracleQueryOutcomes(c, i)
		if gotCan.Possible != wantCan || gotCannot.Possible != (wantCan && wantCannot) {
			t.Fatalf("input %d refusal=%t/%t oracle=%t/%t", i, gotCan.Possible, gotCannot.Possible, wantCan, wantCannot)
		}
	}
}

func TestStableRoleProofFallsBackForRealRoleAndTokenUncertainty(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nodes   []hfNode
		filters []string
		target  int
	}{
		{"unmasked-role-ambiguity", []hfNode{{Path: "nested/data.json"}}, []string{"default"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := compileDecisionFixture(tc.nodes, tc.filters, nil)
			if stableDatasetDecisionEvaluation(c) {
				t.Fatal("uncertain role disposition must not be certified")
			}
			if stableDirectEvaluation(c) {
				t.Fatal("unmasked role ambiguity must not satisfy all-input direct proof")
			}
			gotCan, gotCannot := datasetDecisionRefusal(c, tc.target)
			wantCan, wantCannot, _ := oracleQueryOutcomes(c, tc.target)
			if gotCan.Possible != wantCan || gotCannot.Possible != (wantCan && wantCannot) {
				t.Fatalf("refusal=%t/%t oracle=%t/%t", gotCan.Possible, gotCannot.Possible, wantCan, wantCan && wantCannot)
			}
		})
	}
}

func TestStableEvaluationAcrossRealEnglishTokenCollisionAndImplicitCompanions(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "data/english/items-0000.parquet"},
		{Path: "data/english/train.csv"},
		{Path: "README.md"},
		{Path: "config.json"},
		{Path: "data/README.md"},
	}, []string{"english", "train"}, nil)
	if !collisionTokenBits(c)[c.FilterBits["english"]] {
		t.Fatal("fixture must retain the real english-directory shared-token collision")
	}
	for _, i := range []int{2, 3, 4} {
		if len(c.Inputs[i].Alternatives) != 1 || c.Inputs[i].Alternatives[0].Role != datasetselection.EvidenceRoleCompanion {
			t.Fatalf("metadata input %d must remain an implicit companion: %+v", i, c.Inputs[i].Alternatives)
		}
	}
	if !stableDatasetDecisionEvaluation(c) {
		t.Fatalf("direct selections and companion fallback should be invariant: %+v", c.Inputs)
	}
	world := mustDecisionWorld(t, c, nil, map[uint]bool{c.FilterBits["english"]: false})
	evaluation, err := evaluateDatasetDecisionWorld(c, world)
	if err != nil {
		t.Fatal(err)
	}
	for i, record := range evaluation.Records {
		if !record.Selected {
			t.Fatalf("fixture input %d should be selected in the baseline world (including seeded data/README.md): %+v", i, record)
		}
	}
	checkDatasetDecisionAgainstOracle(t, c)
}

func TestStableEvaluationWithLiteralCollisionAndSeededCompanionSet(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "train.csv"},
		{Path: "test/train.csv"},
		{Path: "test/README.md"},
	}, []string{"train"}, nil)
	if len(collisionTokenBits(c)) == 0 {
		t.Fatal("fixture must have a shared train-token collision")
	}
	if !stableDatasetDecisionEvaluation(c) {
		t.Fatal("literal direct selection must provide a stable seed despite group collision")
	}
	can, cannot, _ := oracleQueryOutcomes(c, 2)
	if !can || cannot {
		t.Fatalf("literal direct seed must always retain the same-directory companion, oracle can/cannot=%t/%t", can, cannot)
	}
	gotCan, gotCannot := datasetDecisionRefusal(c, 2)
	if gotCan.Possible != can || gotCannot.Possible != cannot {
		t.Fatalf("solver refusal=%t/%t oracle=%t/%t", gotCan.Possible, gotCannot.Possible, can, cannot)
	}
}

func TestStableEvaluationRejectsAmbiguousDataCompanionRole(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "folder/data.json"},
	}, []string{"default"}, nil)
	if len(c.Inputs[0].Alternatives) < 2 {
		t.Fatalf("fixture needs data/companion alternatives: %+v", c.Inputs[0].Alternatives)
	}
	if stableDatasetDecisionEvaluation(c) {
		t.Fatal("data/companion role changes selection for an unmatched default record")
	}
	can, cannot, _ := oracleQueryOutcomes(c, 0)
	if !can || !cannot {
		t.Fatalf("fixture must show selected data vs skipped companion worlds, can/cannot=%t/%t", can, cannot)
	}
	gotCan, gotCannot := datasetDecisionRefusal(c, 0)
	if gotCan.Possible != can || gotCannot.Possible != cannot {
		t.Fatalf("refusal=%t/%t oracle=%t/%t", gotCan.Possible, gotCannot.Possible, can, cannot)
	}
}

func TestStableEvaluationRejectsRoleDependentDirectorySeed(t *testing.T) {
	c := compileDecisionFixture([]hfNode{
		{Path: "folder/data.json"},
		{Path: "folder/notes.txt"},
	}, []string{"default"}, nil)
	if len(c.Inputs[0].Alternatives) < 2 {
		t.Fatalf("fixture needs data/companion ambiguity: %+v", c.Inputs[0].Alternatives)
	}
	if stableDatasetDecisionEvaluation(c) {
		t.Fatal("role-dependent data seed changes nested companion selection")
	}
	can, cannot, _ := oracleQueryOutcomes(c, 1)
	if !can || !cannot {
		t.Fatalf("fixture must vary nested companion selection, can/cannot=%t/%t", can, cannot)
	}
	gotCan, gotCannot := datasetDecisionRefusal(c, 1)
	if gotCan.Possible != can || gotCannot.Possible != cannot {
		t.Fatalf("refusal=%t/%t oracle=%t/%t", gotCan.Possible, gotCannot.Possible, can, cannot)
	}
}

func TestNestedCompanionCanUseDirectCompanionSeedWithoutStableCache(t *testing.T) {
	cases := []struct {
		name  string
		nodes []hfNode
	}{
		{"ordinary-source", []hfNode{{Path: "train.csv"}, {Path: "other/README.md"}, {Path: "other/LICENSE"}, {Path: "notes.json"}}},
		{"lfs-source", []hfNode{{Path: "train.csv"}, {Path: "other/README.md", LFS: &hfLfsInfo{Size: 3}}, {Path: "other/LICENSE"}, {Path: "notes.json"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := compileDecisionFixture(tc.nodes, []string{"train", "README"}, nil)
			for _, cached := range []bool{false, true} {
				query := c
				if !cached {
					query.uniqueEvaluation = nil
				}
				checkDatasetDecisionAgainstOracle(t, query)
				for _, target := range []int{2, 3} {
					got := canEmitDatasetDecision(query, target)
					want, _, worlds := oracleQueryOutcomes(query, target)
					if worlds == 0 || got.Possible != want {
						t.Fatalf("cached=%t target %q CanEmit=%t oracle=%t worlds=%d", cached, query.Inputs[target].Path, got.Possible, want, worlds)
					}
					if got.Possible {
						e, err := evaluateDatasetDecisionWorld(query, got.World)
						if err != nil || !e.Records[target].Selected {
							t.Fatalf("target %q has invalid witness: %+v, %v", query.Inputs[target].Path, e, err)
						}
					}
				}
			}
		})
	}
}

func checkDatasetDecisionAgainstOracle(t *testing.T, c datasetDecisionContext) {
	t.Helper()
	for i := range c.Inputs {
		gotCan, gotCannot := datasetDecisionRefusal(c, i)
		wantCan, wantCannot, worlds := oracleQueryOutcomes(c, i)
		if worlds == 0 || gotCan.Possible != wantCan || gotCannot.Possible != (wantCan && wantCannot) {
			t.Fatalf("input %d refusal=%t/%t oracle=%t/%t worlds=%d", i, gotCan.Possible, gotCannot.Possible, wantCan, wantCan && wantCannot, worlds)
		}
		if gotCan.Possible {
			actual, err := evaluateDatasetDecisionWorld(c, gotCan.World)
			if err != nil {
				t.Fatal(err)
			}
			want := concreteOracleRecords(c, gotCan.World)
			for j := range want {
				if actual.Records[j] != want[j] {
					t.Fatalf("input %d witness record %d=%+v oracle=%+v", i, j, actual.Records[j], want[j])
				}
			}
		}
	}
}

func BenchmarkDatasetDecisionCompileAndAllQueriesStableAmbiguous(b *testing.B) {
	for _, size := range []int{100, 1000} {
		b.Run(fmt.Sprintf("files-%d", size), func(b *testing.B) {
			nodes := make([]hfNode, size)
			for i := range nodes {
				ext := ".parquet"
				if i%2 != 0 {
					ext = ".json"
				}
				nodes[i].Path = fmt.Sprintf("test/train/part-%04d%s", i, ext)
			}
			files := make([]datasetselection.File, len(nodes))
			for i := range nodes {
				files[i].Path = nodes[i].Path
			}
			membership := datasetselection.Recognize(files)
			filters := []string{"test", "train", ".parquet", ".json"}
			for _, optimized := range []bool{false, true} {
				name := "solver"
				if optimized {
					name = "stable"
				}
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					for n := 0; n < b.N; n++ {
						c := compileDatasetDecisionContext(nodes, membership, filters, nil, true, true, false)
						if !stableDirectEvaluation(c) {
							b.Fatal("benchmark inventory is not a stable direct-selection context")
						}
						if !optimized {
							c.uniqueEvaluation = nil
						}
						for i := range c.Inputs {
							_, _ = datasetDecisionRefusal(c, i)
						}
					}
				})
			}
		})
	}
}

func BenchmarkDatasetDecisionCompileAndAllQueriesMaskedRoleAmbiguity(b *testing.B) {
	for _, size := range []int{100, 1000} {
		b.Run(fmt.Sprintf("files-%d", size), func(b *testing.B) {
			nodes := make([]hfNode, 0, size)
			for i := 0; i < size-4; i++ {
				nodes = append(nodes, hfNode{Path: fmt.Sprintf("train/part-%04d.parquet", i)})
			}
			nodes = append(nodes, hfNode{Path: "notes.json"}, hfNode{Path: "README.md"}, hfNode{Path: "config.json"}, hfNode{Path: "test/train.csv"})
			files := make([]datasetselection.File, len(nodes))
			for i := range nodes {
				files[i].Path = nodes[i].Path
			}
			membership := datasetselection.Recognize(files)
			filters, excludes := []string{"train", ".parquet", ".json"}, []string{"test/train.csv"}
			for _, optimized := range []bool{false, true} {
				name := "solver"
				if optimized {
					name = "stable"
				}
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					for n := 0; n < b.N; n++ {
						c := compileDatasetDecisionContext(nodes, membership, filters, excludes, true, true, false)
						if !stableDatasetDecisionEvaluation(c) {
							b.Fatal("benchmark inventory does not satisfy masked-role proof")
						}
						if !optimized {
							c.uniqueEvaluation = nil
						}
						for i := range c.Inputs {
							_, _ = datasetDecisionRefusal(c, i)
						}
					}
				})
			}
		})
	}
}

func BenchmarkDatasetDecisionCompileAndAllQueriesRealTokenCollision(b *testing.B) {
	for _, size := range []int{100, 1000} {
		b.Run(fmt.Sprintf("files-%d", size), func(b *testing.B) {
			nodes := make([]hfNode, 0, size)
			for i := 0; i < size-4; i++ {
				nodes = append(nodes, hfNode{Path: fmt.Sprintf("data/english/items-%04d.parquet", i)})
			}
			nodes = append(nodes, hfNode{Path: "data/english/train.csv"}, hfNode{Path: "README.md"}, hfNode{Path: "config.json"}, hfNode{Path: "data/README.md"})
			files := make([]datasetselection.File, len(nodes))
			for i := range nodes {
				files[i].Path = nodes[i].Path
			}
			membership := datasetselection.Recognize(files)
			filters := []string{"english", "train"}
			for _, optimized := range []bool{false, true} {
				name := "solver"
				if optimized {
					name = "stable"
				}
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					for n := 0; n < b.N; n++ {
						c := compileDatasetDecisionContext(nodes, membership, filters, nil, true, true, false)
						if len(collisionTokenBits(c)) == 0 || !stableDatasetDecisionEvaluation(c) {
							b.Fatal("inventory does not have the real-token stable-selection proof")
						}
						if !optimized {
							c.uniqueEvaluation = nil
						}
						for i := range c.Inputs {
							_, _ = datasetDecisionRefusal(c, i)
						}
					}
				})
			}
		})
	}
}

func BenchmarkDatasetDecisionAllEmitAndNonEmitQueriesWithUnknownRoot(b *testing.B) {
	for _, size := range []int{100, 1000} {
		for _, notesAtEnd := range []bool{false, true} {
			name := fmt.Sprintf("files-%d/notes-at-beginning", size)
			if notesAtEnd {
				name = fmt.Sprintf("files-%d/notes-at-end", size)
			}
			b.Run(name, func(b *testing.B) {
				nodes := make([]hfNode, 0, size)
				addData := func(i int) {
					nodes = append(nodes, hfNode{Path: fmt.Sprintf("train/part-%04d.parquet", i)})
				}
				if !notesAtEnd {
					nodes = append(nodes, hfNode{Path: "notes.json"})
				}
				for i := 0; i < size-4; i++ {
					addData(i)
				}
				nodes = append(nodes, hfNode{Path: "test/part.csv"}, hfNode{Path: "README.md"}, hfNode{Path: "config.json"})
				if notesAtEnd {
					nodes = append(nodes, hfNode{Path: "notes.json"})
				}
				files := make([]datasetselection.File, len(nodes))
				for i := range nodes {
					files[i].Path = nodes[i].Path
				}
				membership := datasetselection.Recognize(files)
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					c := compileDatasetDecisionContext(nodes, membership, []string{"train", ".parquet", ".json"}, []string{"test/part.csv"}, true, true, false)
					for i := range c.Inputs {
						_ = canEmitDatasetDecision(c, i)
						_ = canNotEmitDatasetDecision(c, i)
					}
				}
			})
		}
	}
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
	for i, in := range c.Inputs {
		if !in.Excluded {
			direct[i] = concreteOracleDirect(c, w, i, collisions)
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
			if !w.Companion[i] || in.LFS || oracleDatasetPayloadPath(in.Path) || !oracleKnownDatasetCompanionPath(in.Path) {
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
	eligible := concreteOracleEligible(c, w, collisions)
	direct := make([]bool, len(c.Inputs))
	for i := range c.Inputs {
		direct[i] = !c.Inputs[i].Excluded && concreteOracleDirect(c, w, i, collisions)
	}
	out := make([]datasetDecisionRecord, len(c.Inputs))
	for i, in := range c.Inputs {
		out[i].Eligible = eligible[i]
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
	return pathMatch || w.DataLabels[i] == f.Normalized
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

func BenchmarkDatasetDecisionCompileAndAllQueriesUnmaskedMetadata(b *testing.B) {
	for _, size := range []int{100, 1000} {
		b.Run(fmt.Sprintf("files-%d", size), func(b *testing.B) {
			nodes := make([]hfNode, 0, size)
			for i := 0; i < size-4; i++ {
				nodes = append(nodes, hfNode{Path: fmt.Sprintf("train/part-%04d.parquet", i)})
			}
			nodes = append(nodes, hfNode{Path: "notes.json"}, hfNode{Path: "README.md"}, hfNode{Path: "config.json"}, hfNode{Path: "tail/notes.json"})
			files := make([]datasetselection.File, len(nodes))
			for i := range nodes {
				files[i].Path = nodes[i].Path
			}
			membership := datasetselection.Recognize(files)
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				c := compileDatasetDecisionContext(nodes, membership, []string{"train"}, nil, true, true, false)
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
