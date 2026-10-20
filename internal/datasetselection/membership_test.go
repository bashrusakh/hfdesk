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

func TestIsMetadataRecognizesKnownNamesAtAnyDepth(t *testing.T) {
	for _, p := range []string{
		"README", "README.txt", "README.md", "train/README.txt", "data/README.md",
		"LICENSE", "LICENSE.txt", "LICENSE.md", "LICENSE-MIT", "nested/LICENSE.txt", "data/LICENSE-APACHE-2.0",
		"dataset_info.json", "dataset_infos.json", "train/dataset_info.json", "data/dataset_infos.json",
		"config.json", "./LICENSE.txt",
		"train/config.json", "nested/config.json", "data/custom/config.json", "train/CONFIG.JSON",
		"tokenizer.json", "train/tokenizer.json", "data/tokenizer_config.json",
		"special_tokens_map.json", "added_tokens.json", "vocab.json", "merges.txt",
		"vocab.txt", "train/vocab.txt", "tokenizer/tokenizer.model",
		"tokenizer/spiece.model", "tokenizer/sentencepiece.bpe.model", "tokenizer/tokenizer.bin",
		"generation_config.json", "train/quantization_config.json", "adapter_config.json",
		"requirements.txt", "requirements-dev.txt", "test/requirements.txt", "REQUIREMENTS-DEV.TXT",
		"pyproject.toml", "train/pyproject.toml", "setup.py", "test/setup.py", "setup.cfg", "data/setup.cfg",
		"environment.yml", "test/environment.yaml", "data/environment-prod.yml",
	} {
		if !IsMetadata(p) {
			t.Errorf("IsMetadata(%q) = false, want true", p)
		}
	}
	for _, p := range []string{
		"", "notes.txt", "train/notes.txt", "data.json", "license.json", "readme.json",
		"nested/unknown.json", "train/unknown.jsonl", "unknown.txt",
		"myconfig.json", "tokenizer_config_extra.json", "requirements.json",
		"environment.yml.bak", "setup.py.bak",
	} {
		if IsMetadata(p) {
			t.Errorf("IsMetadata(%q) = true, want false", p)
		}
	}
}

func TestIsRootMetadataRecognizesOnlyKnownRootNames(t *testing.T) {
	for _, p := range []string{
		"README", "README.txt", "README.md", "readme.rst",
		"LICENSE", "LICENSE.txt", "LICENSE.md", "LICENSE-MIT", "LICENSE-APACHE-2.0",
		"dataset_info.json", "dataset_infos.json", "config.json", "./LICENSE.txt",
		"tokenizer.json", "tokenizer_config.json", "special_tokens_map.json", "added_tokens.json",
		"vocab.json", "merges.txt", "vocab.txt", "requirements.txt", "requirements-dev.txt",
		"pyproject.toml", "setup.py", "setup.cfg", "environment.yml", "environment-prod.yaml",
	} {
		if !IsRootMetadata(p) {
			t.Errorf("IsRootMetadata(%q) = false, want true", p)
		}
	}
	for _, p := range []string{
		"", "notes.txt", "data.json", "license.json", "unknown.json",
		"nested/README.txt", "nested/LICENSE.txt", "nested/dataset_infos.json", "nested/config.json",
		"train/tokenizer.json",
	} {
		if IsRootMetadata(p) {
			t.Errorf("IsRootMetadata(%q) = true, want false", p)
		}
	}
}

func TestRecognizeKnownMetadataIsCompanionOnly(t *testing.T) {
	files := []File{
		{Path: "README.txt"},
		{Path: "README.md"},
		{Path: "LICENSE.txt"},
		{Path: "LICENSE.md"},
		{Path: "LICENSE"},
		{Path: "dataset_infos.json"},
		{Path: "config.json"},
		{Path: "train/README.txt"},
		{Path: "train/LICENSE.txt"},
		{Path: "data/dataset_infos.json"},
		{Path: "nested/dataset_info.json"},
		{Path: "nested/README.md"},
		{Path: "notes.txt"},
		{Path: "data.json"},
		{Path: "nested/config.json"},
		{Path: "nested/notes.txt"},
		{Path: "train/config.json"},
		{Path: "train/tokenizer.json"},
		{Path: "data/custom/config.json"},
		{Path: "unknown.txt"},
	}
	r := Recognize(files)
	knownRoot := []int{0, 1, 2, 3, 4, 5, 6}
	for _, i := range knownRoot {
		if len(r.Evidence) <= i {
			t.Fatalf("missing evidence for input %d", i)
		}
		assertEvidence(t, r.Evidence[i], i, EvidenceRootMetadata, nil, nil, []EvidenceRole{EvidenceRoleCompanion}, "")
	}
	knownNested := []int{7, 8, 9, 10, 11, 14, 16, 17, 18}
	for _, i := range knownNested {
		if len(r.Evidence) <= i {
			t.Fatalf("missing evidence for input %d", i)
		}
		assertEvidence(t, r.Evidence[i], i, EvidenceMetadataCompanion, nil, nil, []EvidenceRole{EvidenceRoleCompanion}, "")
	}
	unknown := []int{12, 13, 15, 19}
	for _, i := range unknown {
		assertEvidence(t, r.Evidence[i], i, EvidenceUnnamedAmbiguous, []string{"default"}, nil, []EvidenceRole{EvidenceRoleData, EvidenceRoleCompanion}, "")
	}
	assertIndexes(t, r.Unresolved, unknown...)
	if len(r.Groups) != 0 {
		t.Fatalf("metadata or unknown files were classified as dataset groups: %v", r.Groups)
	}
}

func TestRecognizeSplitDirectoryConfigIsCompanionButUnknownTextStaysData(t *testing.T) {
	files := []File{
		{Path: "train/config.json"},
		{Path: "train/tokenizer.json"},
		{Path: "train/notes.txt"},
		{Path: "data/custom/config.json"},
		{Path: "data/custom/part.parquet"},
	}
	r := Recognize(files)
	assertIndexes(t, r.Groups["train"], 2)
	assertIndexes(t, r.Groups["custom"], 4)
	assertEvidence(t, r.Evidence[0], 0, EvidenceMetadataCompanion, nil, nil, []EvidenceRole{EvidenceRoleCompanion}, "")
	assertEvidence(t, r.Evidence[1], 1, EvidenceMetadataCompanion, nil, nil, []EvidenceRole{EvidenceRoleCompanion}, "")
	assertEvidence(t, r.Evidence[2], 2, EvidenceConventional, []string{"train"}, []string{"train"}, []EvidenceRole{EvidenceRoleData}, EvidenceRoleData)
	assertEvidence(t, r.Evidence[3], 3, EvidenceMetadataCompanion, nil, nil, []EvidenceRole{EvidenceRoleCompanion}, "")
	assertEvidence(t, r.Evidence[4], 4, EvidenceCustom, []string{"custom"}, []string{"custom"}, []EvidenceRole{EvidenceRoleData}, EvidenceRoleData)
	if len(r.Unresolved) != 0 {
		t.Fatalf("split-directory data or known metadata unexpectedly became ambiguous: %v", r.Unresolved)
	}
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
	assertEvidence(t, r.Evidence[7], 7, EvidenceMetadataCompanion, nil, nil, []EvidenceRole{EvidenceRoleCompanion}, "")
	assertEvidence(t, r.Evidence[8], 8, EvidenceOrdinaryCompanion, nil, nil, []EvidenceRole{EvidenceRoleCompanion}, "")
	assertEvidence(t, r.Evidence[9], 9, EvidenceCustom, []string{"ordinary-companion"}, []string{"ordinary-companion"}, []EvidenceRole{EvidenceRoleData}, EvidenceRoleData)

	// The preparatory evidence must not alter current planner-facing membership.
	assertIndexes(t, r.Groups["train"], 0)
	assertIndexes(t, r.Groups["customdata"], 1)
	assertIndexes(t, r.Unresolved, 2, 4)
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
