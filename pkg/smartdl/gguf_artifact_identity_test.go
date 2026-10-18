package smartdl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

func TestGGUFQuantClassificationUsesWholeTokensAndPathSeparators(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"Ternary-Bonsai-PQ2_0.gguf", ""},
		{"model-Q4_0CUSTOM.gguf", ""},
		{"variants/Q4_K_M/shard-00001-of-00002.gguf", "Q4_K_M"},
		{"variants/UD-Q4_K_XL/shard-00001-of-00002.gguf", "UD_Q4_K_XL"},
		{"model-IQ3_XXS.gguf", "IQ3_XXS"},
		{"model-APEX-I-QUALITY.gguf", "APEX_I_QUALITY"},
		{"model-BF16.gguf", "BF16"},
		{"model-F16.gguf", "F16"},
		{"model-Q6_K_XL.gguf", "Q6_K_XL"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := ggufQuantType(FileInfo{Name: filepath.Base(tc.path), Path: tc.path}); got != tc.want {
				t.Fatalf("ggufQuantType(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestGGUFArtifactIdentityIsIndependentOfQuantLabel(t *testing.T) {
	files := []FileInfo{
		{Name: "Ternary-Bonsai-PQ2_0.gguf", Path: "Ternary-Bonsai-PQ2_0.gguf", Size: 1},
		{Name: "model-QAD-Q4_0.gguf", Path: "model-QAD-Q4_0.gguf", Size: 2},
		{Name: "model-Q4_0.gguf", Path: "model-Q4_0.gguf", Size: 3},
		{Name: "model-Q4_0.gguf", Path: "nested/model-Q4_0.gguf", Size: 9},
		{Name: "model-MTP-Q4_K_M.gguf", Path: "model-MTP-Q4_K_M.gguf", Size: 4},
		{Name: "other-MTP-Q4_K_M.gguf", Path: "other-MTP-Q4_K_M.gguf", Size: 10},
		{Name: "model-Q4_K_M.gguf", Path: "model-Q4_K_M.gguf", Size: 5},
		{Name: "model-Q4_0.gguf", Path: "a/b-c-Q4_0.gguf", Size: 13},
		{Name: "model-Q4_0.gguf", Path: "a-b/c-Q4_0.gguf", Size: 14},
		{Name: "model-Q5_K_M-00001-of-00002.gguf", Path: "model-Q5_K_M-00001-of-00002.gguf", Size: 6},
		{Name: "model-Q5_K_M-00002-of-00002.gguf", Path: "model-Q5_K_M-00002-of-00002.gguf", Size: 7},
		{Name: "future-layout.gguf", Path: "future-layout.gguf", Size: 8},
	}
	if got := ggufQuantType(files[0]); got != "" {
		t.Fatalf("PQ2_0 was misclassified as %q", got)
	}

	// Exercise the exact recommended body against the public planner using the
	// same LFS tree shape the Hub returns for GGUF payloads.
	var tree []map[string]any
	for _, file := range files {
		tree = append(tree, map[string]any{"type": "file", "path": file.Path, "size": file.Size, "lfs": map[string]any{"size": file.Size}})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/datasets/") {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/revision/main") {
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": strings.Repeat("a", 40)})
			return
		}
		if strings.Contains(r.URL.Path, "/tree/") {
			_ = json.NewEncoder(w).Encode(tree)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	info, err := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()}).Analyze(context.Background(), "owner/repo", false)
	if err != nil {
		t.Fatalf("public analyzer: %v", err)
	}
	if info.Type != TypeGGUF || info.GGUF == nil {
		t.Fatalf("public analyzer type = %q, gguf=%v", info.Type, info.GGUF)
	}
	items := info.SelectableItems
	byLabel := make(map[string]SelectableItem, len(items))
	for _, item := range items {
		byLabel[item.Label] = item
	}
	for label, want := range map[string][]string{
		"Q4_0 · model-QAD-Q4_0.gguf":           {"model-QAD-Q4_0.gguf"},
		"Q4_0 · model-Q4_0.gguf":               {"model-Q4_0.gguf"},
		"Q4_0 · nested/model-Q4_0.gguf":        {"nested/model-Q4_0.gguf"},
		"Q4_K_M (MTP) · model-MTP-Q4_K_M.gguf": {"model-MTP-Q4_K_M.gguf"},
		"Q4_K_M (MTP) · other-MTP-Q4_K_M.gguf": {"other-MTP-Q4_K_M.gguf"},
		"Q4_K_M":                               {"model-Q4_K_M.gguf"},
		"Q4_0 · a/b-c-Q4_0.gguf":               {"a/b-c-Q4_0.gguf"},
		"Q4_0 · a-b/c-Q4_0.gguf":               {"a-b/c-Q4_0.gguf"},
		"Q5_K_M":                               {"model-Q5_K_M-00001-of-00002.gguf", "model-Q5_K_M-00002-of-00002.gguf"},
		"Unknown · future-layout.gguf":         {"future-layout.gguf"},
		"Unknown · Ternary-Bonsai-PQ2_0.gguf":  {"Ternary-Bonsai-PQ2_0.gguf"},
	} {
		item, ok := byLabel[label]
		if !ok || !reflect.DeepEqual(item.Files, want) {
			t.Errorf("item %q files = %v, want %v (exists=%v)", label, item.Files, want, ok)
		}
	}
	if len(info.GGUF.Quantizations) != 11 {
		t.Fatalf("got %d independent artifact groups, want 11", len(info.GGUF.Quantizations))
	}
	ids, labels := map[string]bool{}, map[string]bool{}
	for _, item := range items {
		if ids[item.ID] || labels[item.Label] {
			t.Errorf("artifact identity collision: ID=%q label=%q", item.ID, item.Label)
		}
		ids[item.ID], labels[item.Label] = true, true
	}

	selection := info
	if selection.RecommendedDownload == nil || !selection.RecommendedDownload.ExactMatch {
		t.Fatalf("GGUF recommendation must be an exact request: %+v", selection.RecommendedDownload)
	}
	for _, filter := range selection.RecommendedDownload.Filters {
		if filter == "q4_0" || filter == "q4_k_m" {
			t.Fatalf("recommendation emitted ambiguous quant label %q instead of artifact paths: %v", filter, selection.RecommendedDownload.Filters)
		}
	}
	plan, err := hfdownloader.PlanRepo(context.Background(), hfdownloader.Job{
		Repo: selection.RecommendedDownload.Repo, Filters: selection.RecommendedDownload.Filters,
		ExactMatch: selection.RecommendedDownload.ExactMatch,
	}, hfdownloader.Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo recommended selection: %v", err)
	}
	var planned []string
	for _, item := range plan.Items {
		planned = append(planned, item.RelativePath)
	}
	sort.Strings(planned)
	var want []string
	for _, filter := range selection.RecommendedDownload.Filters {
		filter = strings.TrimPrefix(filter, "./")
		want = append(want, filter)
	}
	sort.Strings(want)
	if !reflect.DeepEqual(planned, want) {
		t.Fatalf("PlanRepo selected %v; want exact recommended artifact paths %v", planned, want)
	}
	for _, item := range items {
		if item.Category != "quantization" {
			continue
		}
		filters := make([]string, len(item.Files))
		for i, file := range item.Files {
			filters[i] = exactArtifactFilter(file)
		}
		plan, err := hfdownloader.PlanRepo(context.Background(), hfdownloader.Job{
			Repo: "owner/repo", Filters: filters, ExactMatch: true,
		}, hfdownloader.Settings{Endpoint: srv.URL})
		if err != nil {
			t.Errorf("PlanRepo selectable %q: %v", item.Label, err)
			continue
		}
		got := make([]string, 0, len(plan.Items))
		for _, entry := range plan.Items {
			got = append(got, entry.RelativePath)
		}
		sort.Strings(got)
		want := append([]string(nil), item.Files...)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("PlanRepo selectable %q chose %v, want %v", item.Label, got, want)
		}
	}
}
