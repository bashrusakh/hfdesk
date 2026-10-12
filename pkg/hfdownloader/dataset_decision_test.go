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

func TestCompileDatasetDecisionContextMembershipSelectionsCanSeedDirectories(t *testing.T) {
	// The filter "train" selects train.csv by membership even though the
	// ordinary complete-path matcher does not match that filename. It must
	// still seed companion scope for siblings beneath the selected directory.
	c := compileDecisionFixture([]hfNode{{Path: "partition/train.csv"}, {Path: "partition/metadata.json"}}, []string{"train"}, nil)
	if !c.Inputs[0].DirectSeedCandidate {
		t.Fatal("membership-selected file was not indexed as a possible seed")
	}
	if got := c.Inputs[1].AncestorDirectSeedIndexes; !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("membership-selected directory seeds=%v want [0]", got)
	}
	// Exclusion is prior to selection and must prevent a seed.
	excluded := compileDecisionFixture([]hfNode{{Path: "partition/train.csv"}, {Path: "partition/metadata.json"}}, []string{"train"}, []string{"train.csv"})
	if excluded.Inputs[0].DirectSeedCandidate || len(excluded.Inputs[1].AncestorDirectSeedIndexes) != 0 {
		t.Fatalf("excluded membership source seeded directory: %+v", excluded.Inputs)
	}
	companionOnly := compileDecisionFixture([]hfNode{{Path: "train/README.md"}, {Path: "other/metadata.json"}}, []string{"train"}, nil)
	if companionOnly.Inputs[0].DirectSeedCandidate || len(companionOnly.Inputs[1].AncestorDirectSeedIndexes) != 0 {
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
