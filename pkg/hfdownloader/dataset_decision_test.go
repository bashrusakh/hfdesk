// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"reflect"
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
	if got := c.Inputs[2].AncestorDirectSeedIndexes; !reflect.DeepEqual(got, []int{1, 4}) {
		t.Fatalf("nested seed indexes=%v want both actual direct seed sources", got)
	}
	if len(c.Inputs[3].AncestorDirectSeedIndexes) != 0 {
		t.Fatalf("unrelated directory inherited seeds: %v", c.Inputs[3].AncestorDirectSeedIndexes)
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
