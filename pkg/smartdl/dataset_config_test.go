// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// configPresetFixture is a multi-config dataset with root strays: cc100
// holds train+test, wiki holds only validation, stray_train.csv is a
// root-level train-group file with no config, and cc100/config.json is a
// metadata companion inside a config folder.
func configPresetFixture() []FileInfo {
	return []FileInfo{
		{Name: "part.parquet", Path: "cc100/train/part.parquet", Size: 1000},
		{Name: "more.parquet", Path: "cc100/train/more.parquet", Size: 2000},
		{Name: "part.parquet", Path: "cc100/test/part.parquet", Size: 500},
		{Name: "part.parquet", Path: "wiki/validation/part.parquet", Size: 300},
		{Name: "stray_train.csv", Path: "stray_train.csv", Size: 700},
		{Name: "config.json", Path: "cc100/config.json", Size: 10},
		{Name: "README.md", Path: "README.md", Size: 10},
		{Name: "config.json", Path: "config.json", Size: 10},
	}
}

// TestDatasetConfigPresetGroupingAndRecommendation covers the D5b config
// preset rows: one item per detected config, folder filter, split-composed
// label, rolled-up size, root strays staying split-only, preserved split
// rows, and exactly one Recommended badge on the unique train-bearing
// config while every split row stays unbadged.
func TestDatasetConfigPresetGroupingAndRecommendation(t *testing.T) {
	items := DatasetToSelectableItems(analyzeDataset(configPresetFixture()))
	// Config presets come first so the UI's category order puts Dataset
	// Configs above Dataset Splits; split rows follow unchanged.
	for i, category := range []string{"config", "config", "split", "split", "split"} {
		if len(items) != 5 || items[i].Category != category {
			t.Fatalf("items[%d] = %+v (want 5 rows, %q first): %+v", i, items, category, items)
		}
	}
	cc100, wiki := items[0], items[1]
	if cc100.ID != "cc100/" || cc100.FilterValue != "cc100/" || cc100.Label != "cc100 (train+test)" ||
		cc100.Size != 3500 || cc100.SizeHuman != humanSize(3500) || cc100.Description != "Dataset config (3 files)" ||
		!cc100.Recommended ||
		strings.Join(cc100.Files, ",") != "cc100/train/part.parquet,cc100/train/more.parquet,cc100/test/part.parquet" {
		t.Fatalf("cc100 preset = %+v, want folder filter, (train+test) label, 3500-byte rollup, badge", cc100)
	}
	if wiki.ID != "wiki/" || wiki.FilterValue != "wiki/" || wiki.Label != "wiki (validation)" ||
		wiki.Size != 300 || wiki.Recommended || strings.Join(wiki.Files, ",") != "wiki/validation/part.parquet" {
		t.Fatalf("wiki preset = %+v, want its own folder, label and single-member rollup", wiki)
	}
	badges := 0
	for _, item := range items {
		if item.Recommended {
			badges++
		}
	}
	if badges != 1 {
		t.Errorf("Recommended badges = %d, want exactly one (the cc100 config): %+v", badges, items)
	}
	labels := map[string]string{"train": "Train", "validation": "Validation", "test": "Test"}
	for i, id := range []string{"train", "validation", "test"} {
		item := items[2+i]
		if item.ID != id || item.FilterValue != id || item.Label != labels[id] || item.Category != "split" || item.Recommended {
			t.Errorf("split row %d = %+v, want preserved unbadged split %q", i, item, id)
		}
	}
	trainFiles := strings.Join(items[2].Files, ",")
	if !strings.Contains(trainFiles, "stray_train.csv") || !strings.Contains(trainFiles, "cc100/train/part.parquet") {
		t.Errorf("train split files = %v, want config and root-stray members preserved", items[2].Files)
	}
	for _, item := range items[:2] {
		for _, file := range item.Files {
			if file == "stray_train.csv" || file == "cc100/config.json" {
				t.Errorf("%q leaked into config preset %q", file, item.ID)
			}
		}
	}
}

// TestDatasetConfigPresetMixedLayoutFolder pins the fallback for a config
// whose members span both detectConfig layouts (root-level <config>/ and
// data/<config>/): the preset must fall back to the bare <config>/ selector
// so no member lies outside the folder filter.
func TestDatasetConfigPresetMixedLayoutFolder(t *testing.T) {
	items := DatasetToSelectableItems(analyzeDataset([]FileInfo{
		{Name: "a.parquet", Path: "cc100/train/a.parquet", Size: 10},
		{Name: "b.parquet", Path: "data/cc100/test/b.parquet", Size: 20},
	}))
	preset := SelectableItem{}
	if len(items) == 3 {
		preset = items[0]
	}
	if len(items) != 3 || preset.Category != "config" || preset.ID != "cc100/" ||
		preset.FilterValue != "cc100/" || preset.Label != "cc100 (train+test)" ||
		len(preset.Files) != 2 || preset.Size != 30 {
		t.Fatalf("mixed-layout rows = %+v, want one bare cc100/ preset rolling up both layouts", items)
	}
}

// TestDatasetConfigPresetAbsentPreservesSplitRows is the preserved-behavior
// row: a dataset without config folders emits only split items and keeps the
// existing train recommendation.
func TestDatasetConfigPresetAbsentPreservesSplitRows(t *testing.T) {
	items := DatasetToSelectableItems(analyzeDataset([]FileInfo{
		{Name: "train.parquet", Path: "data/train.parquet", Size: 100},
		{Name: "test.parquet", Path: "data/test.parquet", Size: 50},
	}))
	if len(items) != 2 || items[0].ID != "train" || items[1].ID != "test" {
		t.Fatalf("items = %+v, want only the train and test splits", items)
	}
	for _, item := range items {
		if item.Category == "config" || item.FilterValue != item.ID {
			t.Errorf("item %+v must keep its split-only identity", item)
		}
	}
	if !items[0].Recommended || items[1].Recommended {
		t.Errorf("existing split-level recommendation lost: %+v", items)
	}
}

// TestDatasetConfigPresetRecommendationFallback pins the no-clear-winner
// behavior: with two train-bearing configs or with configs that hold no
// train split, no config preset is badged and the badge falls back to the
// existing split-level rule (train) or disappears with it — always at most
// one badge.
func TestDatasetConfigPresetRecommendationFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		files  []FileInfo
		badges []string // recommended row ids, in item order
	}{
		{"two train-bearing configs keep the split badge", []FileInfo{
			{Name: "part.parquet", Path: "alpha/train/part.parquet", Size: 10},
			{Name: "part.parquet", Path: "beta/train/part.parquet", Size: 10},
		}, []string{"train"}},
		{"configs without any train split recommend nothing", []FileInfo{
			{Name: "part.parquet", Path: "alpha/dev/part.parquet", Size: 10},
			{Name: "part.parquet", Path: "beta/dev/part.parquet", Size: 10},
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := DatasetToSelectableItems(analyzeDataset(tc.files))
			var badges []string
			for _, item := range items {
				if item.Category == "config" && item.Recommended {
					t.Errorf("config preset %q must stay unbadged without a unique train config", item.ID)
				}
				if item.Recommended {
					badges = append(badges, item.ID)
				}
			}
			if len(items) != 3 || strings.Join(badges, ",") != strings.Join(tc.badges, ",") {
				t.Errorf("rows/badges = %d/%v, want 3 rows and badges %v: %+v", len(items), badges, tc.badges, items)
			}
		})
	}
}

// TestDatasetConfigFolderFilterPlannerInterplay verifies the smartdl↔planner
// boundary without touching pkg/hfdownloader: the config folder selector is
// credited as a matched filter (no filters_unmatched notice), selects the
// config's files, and never double-reports alongside the split-mention
// notice family.
func TestDatasetConfigFolderFilterPlannerInterplay(t *testing.T) {
	paths := []string{
		"cc100/train/part.parquet",
		"cc100/test/part.parquet",
		"wiki/validation/part.parquet",
		"wiki/validation/train-part.csv",
		"README.md",
		"config.json",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/owner/multi/revision/rev1"):
			_, _ = w.Write([]byte(`{"sha":"rev1"}`))
		case strings.HasSuffix(r.URL.Path, "/owner/multi/tree/rev1"):
			nodes := make([]map[string]any, len(paths))
			for i, p := range paths {
				nodes[i] = map[string]any{"type": "file", "path": p, "size": 10}
			}
			_ = json.NewEncoder(w).Encode(nodes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	info, err := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()}).AnalyzeWithRevision(context.Background(), "owner/multi", true, "rev1")
	if err != nil {
		t.Fatal(err)
	}
	info.PopulateDownloadSelection()

	// The unique train-bearing config owns the recommendation, so the ready
	// request selects the config folder rather than the bare train split.
	if request := info.RecommendedDownload; request == nil || !request.ExactMatch ||
		len(request.Filters) != 1 || request.Filters[0] != "cc100/" {
		t.Fatalf("recommended request = %+v, want the cc100/ config folder", request)
	}
	configFilter := info.RecommendedDownload.Filters[0]

	planFor := func(filters ...string) *hfdownloader.Plan {
		t.Helper()
		plan, err := hfdownloader.PlanRepo(context.Background(), hfdownloader.Job{
			Repo: "owner/multi", Revision: "rev1", IsDataset: true, Filters: filters, ExactMatch: true,
		}, hfdownloader.Settings{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("PlanRepo(%v): %v", filters, err)
		}
		return plan
	}
	pathsOf := func(plan *hfdownloader.Plan) map[string]bool {
		got := map[string]bool{}
		for _, item := range plan.Items {
			got[item.RelativePath] = true
		}
		return got
	}

	t.Run("config folder filter selects the config and is credited as matched", func(t *testing.T) {
		plan := planFor(configFilter)
		planned := pathsOf(plan)
		for path, want := range map[string]bool{
			"cc100/train/part.parquet": true, "cc100/test/part.parquet": true,
			"wiki/validation/part.parquet": false, "wiki/validation/train-part.csv": false,
		} {
			if planned[path] != want {
				t.Errorf("plan contains %s = %t, want %t: %v", path, planned[path], want, planned)
			}
		}
		if len(plan.Notices) != 0 {
			t.Errorf("credited config folder filter must produce no notices, got %v", plan.Notices)
		}
	})

	t.Run("config filter beside a split filter never double-reports", func(t *testing.T) {
		plan := planFor(configFilter, "train")
		planned := pathsOf(plan)
		if !planned["cc100/train/part.parquet"] || !planned["cc100/test/part.parquet"] {
			t.Errorf("combined plan lost config files: %v", planned)
		}
		mentions := 0
		for _, notice := range plan.Notices {
			if strings.Contains(notice, "matched nothing") || strings.Contains(notice, "cc100") {
				t.Errorf("a credited filter was reported unmatched or the config filter was reported: %q", notice)
			}
			if strings.Contains(notice, "train-part.csv") {
				mentions++
			}
		}
		if mentions != 1 {
			t.Errorf("split-mention reports for the train label = %d, want exactly one: %v", mentions, plan.Notices)
		}
	})
}
