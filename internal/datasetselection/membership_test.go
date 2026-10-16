// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package datasetselection

import "testing"

func TestRecognize(t *testing.T) {
	files := []File{
		{Path: "data/train-part.parquet"},
		{Path: "nested/test_records.csv"},
		{Path: "data/customdata/items.parquet"},
		{Path: "data.json"},
		{Path: "notes.txt"},
		{Path: "config.json"},
		{Path: "dataset_info.json"},
		{Path: "train/test.parquet"},
		{Path: "weights/model.safetensors"},
		{Path: "other/data.parquet"},
	}
	r := Recognize(files)
	assertIndexes(t, r.Groups["train"], 0)
	assertIndexes(t, r.Groups["test"], 1)
	assertIndexes(t, r.Groups["customdata"], 2)
	assertIndexes(t, r.Groups["default"], 9)
	assertIndexes(t, r.Unresolved, 3, 4, 7)
	for _, indices := range r.Groups {
		for _, i := range indices {
			if i == 5 || i == 6 || i == 8 {
				t.Errorf("non-data/service metadata file index %d classified as dataset data", i)
			}
		}
	}
}

func TestRecognizeRetainsInputIndexAndCaseInsensitiveRoles(t *testing.T) {
	files := []File{
		{Path: "DATA/TRAIN_part.JSON"},
		{Path: "DATA/TRAIN_part.JSON"},
		{Path: "validation/records.parquet"},
	}
	r := Recognize(files)
	assertIndexes(t, r.Groups["train"], 0, 1)
	assertIndexes(t, r.Groups["validation"], 2)
}

func TestRecognizeSplitPrefixBoundaries(t *testing.T) {
	files := []File{
		{Path: "corpus_train_000.parquet"},
		{Path: "corpus_test_000.parquet"},
		{Path: "pretrain_000.parquet"},
		{Path: "train-data.parquet"},
		{Path: "train.parquet"},
	}
	r := Recognize(files)
	assertIndexes(t, r.Groups["train"], 0, 3, 4)
	assertIndexes(t, r.Groups["test"], 1)
	assertIndexes(t, r.Groups["default"], 2)
}

func TestRecognizeTreatsFlatDataLeavesAsDefaultAndDirectoriesAsGroups(t *testing.T) {
	files := []File{
		{Path: "data/part-0000.parquet"},
		{Path: "data/part-0001.parquet"},
		{Path: "data/custom/part.parquet"},
	}
	r := Recognize(files)
	assertIndexes(t, r.Groups["default"], 0, 1)
	assertIndexes(t, r.Groups["custom"], 2)
}

func TestEvidenceRetainsResolverAlternativesAndReasons(t *testing.T) {
	files := []File{
		{Path: "train/records.parquet"},
		{Path: "data/customdata/items.csv"},
		{Path: "train/test/items.parquet"},
		{Path: "records.parquet"},
		{Path: "records.jsonl"},
		{Path: "config.json"},
		{Path: "dataset_info.json"},
		{Path: "nested/config.json"},
		{Path: "weights/model.safetensors"},
		{Path: "data/ordinary-companion/records.csv"},
	}
	r := Recognize(files)
	if len(r.Evidence) != len(files) {
		t.Fatalf("evidence count = %d, want %d", len(r.Evidence), len(files))
	}
	assertEvidence(t, r.Evidence[0], 0, EvidenceConventional, []string{"train"}, []string{"train"}, []EvidenceRole{EvidenceRoleData}, EvidenceRoleData)
	assertEvidence(t, r.Evidence[1], 1, EvidenceCustom, []string{"customdata"}, []string{"customdata"}, []EvidenceRole{EvidenceRoleData}, EvidenceRoleData)
	assertEvidence(t, r.Evidence[2], 2, EvidenceConflicting, []string{"test", "train"}, nil, []EvidenceRole{EvidenceRoleData}, "")
	assertEvidence(t, r.Evidence[3], 3, EvidenceUnnamedDefault, []string{"default"}, []string{"default"}, []EvidenceRole{EvidenceRoleData}, EvidenceRoleData)
	assertEvidence(t, r.Evidence[4], 4, EvidenceUnnamedAmbiguous, []string{"default"}, nil, []EvidenceRole{EvidenceRoleData, EvidenceRoleCompanion}, "")
	assertEvidence(t, r.Evidence[5], 5, EvidenceRootMetadata, nil, nil, []EvidenceRole{EvidenceRoleCompanion}, "")
	assertEvidence(t, r.Evidence[6], 6, EvidenceRootMetadata, nil, nil, []EvidenceRole{EvidenceRoleCompanion}, "")
	assertEvidence(t, r.Evidence[7], 7, EvidenceUnnamedAmbiguous, []string{"default"}, nil, []EvidenceRole{EvidenceRoleData, EvidenceRoleCompanion}, "")
	assertEvidence(t, r.Evidence[8], 8, EvidenceOrdinaryCompanion, nil, nil, []EvidenceRole{EvidenceRoleCompanion}, "")
	assertEvidence(t, r.Evidence[9], 9, EvidenceCustom, []string{"ordinary-companion"}, []string{"ordinary-companion"}, []EvidenceRole{EvidenceRoleData}, EvidenceRoleData)

	// The preparatory evidence must not alter current planner-facing membership.
	assertIndexes(t, r.Groups["train"], 0)
	assertIndexes(t, r.Groups["customdata"], 1)
	assertIndexes(t, r.Unresolved, 2, 4, 7)
	assertIndexes(t, r.Groups["default"], 3)
	assertIndexes(t, r.Groups["ordinary-companion"], 9)

	// Conflicting alternatives are stable even though legacy recognition uses a map.
	for range 20 {
		got := Recognize(files).Evidence[2].PossibleLabels
		if !sameStrings(got, []string{"test", "train"}) || got[0] != "test" {
			t.Fatalf("conflicting alternatives are not deterministically sorted: %v", got)
		}
	}
}

func assertEvidence(t *testing.T, got Evidence, index int, reason EvidenceReason, possible, definite []string, roles []EvidenceRole, definiteRole EvidenceRole) {
	t.Helper()
	if got.InputIndex != index || got.Reason != reason || !sameStrings(got.PossibleLabels, possible) || !sameStrings(got.DefiniteLabels, definite) || !sameRoles(got.PossibleRoles, roles) || got.DefiniteRole != definiteRole {
		t.Fatalf("evidence[%d] = %+v, want reason=%q possible=%v definite=%v", index, got, reason, possible, definite)
	}
}

func sameRoles(a, b []EvidenceRole) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, value := range a {
		found := false
		for _, want := range b {
			if value == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func assertIndexes(t *testing.T, got []int, want ...int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("indexes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("indexes = %v, want %v", got, want)
		}
	}
}
