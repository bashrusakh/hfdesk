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
