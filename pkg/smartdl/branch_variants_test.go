// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseEXLBranchCandidate(t *testing.T) {
	tests := []struct {
		name      string
		ref       RepoRef
		wantOK    bool
		wantBPW   float64
		wantHeads float64
	}{
		{name: "simple bpw", ref: RepoRef{Name: "4.00bpw", Type: "branch"}, wantOK: true, wantBPW: 4},
		{name: "SC bpw and head", ref: RepoRef{Name: "SC_6.00bpw_H6_V6", Type: "branch"}, wantOK: true, wantBPW: 6, wantHeads: 6},
		{name: "nested bpw", ref: RepoRef{Name: "exl/4.5bpw", Type: "branch"}, wantOK: true, wantBPW: 4.5},
		{name: "N_M branch", ref: RepoRef{Name: "4_25", Type: "branch"}, wantOK: true, wantBPW: 4.25},
		{name: "N_M branch with short fraction", ref: RepoRef{Name: "6_5", Type: "branch"}, wantOK: true, wantBPW: 6.5},
		{name: "nested N_M branch", ref: RepoRef{Name: "exl/4_25", Type: "branch"}, wantOK: true, wantBPW: 4.25},
		{name: "prefixed N_M is not a bitrate", ref: RepoRef{Name: "fix_4_2", Type: "branch"}, wantOK: false},
		{name: "SC N_M is not a bitrate", ref: RepoRef{Name: "SC_6_00", Type: "branch"}, wantOK: false},
		{name: "date-like N_M is not a plausible bitrate", ref: RepoRef{Name: "2024_01", Type: "branch"}, wantOK: false},
		{name: "N_M fraction above bound", ref: RepoRef{Name: "4_250", Type: "branch"}, wantOK: false},
		{name: "N_M whole bits above bound", ref: RepoRef{Name: "65_0", Type: "branch"}, wantOK: false},
		{name: "N_M with trailing text", ref: RepoRef{Name: "4_25-beta", Type: "branch"}, wantOK: false},
		{name: "tag is not a variant", ref: RepoRef{Name: "4.00bpw", Type: "tag"}, wantOK: false},
		{name: "fp16 is not a bitrate", ref: RepoRef{Name: "fp16", Type: "branch"}, wantOK: false},
		{name: "unsupported revision control", ref: RepoRef{Name: "bad\x7f/4.00bpw", Type: "branch"}, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseEXLBranchCandidate(tt.ref)
			if ok != tt.wantOK {
				t.Fatalf("parseEXLBranchCandidate() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.bpw != tt.wantBPW || got.headBits != tt.wantHeads {
				t.Fatalf("candidate = %+v, want bpw=%v head_bits=%v", got, tt.wantBPW, tt.wantHeads)
			}
		})
	}
}

func TestAnalyzeEXLBranchVariantsAndRevisionSelection(t *testing.T) {
	const (
		repo       = "owner/example-EXL3-model"
		balanced   = "4.00bpw"
		scVariant  = "SC_6.00bpw_H6_V6"
		mainConfig = `{"model_type":"llama"}`
	)
	refs := map[string]interface{}{
		"branches": []map[string]string{
			{"name": "main", "targetCommit": "main-sha"},
			{"name": balanced, "targetCommit": "4-sha"},
			{"name": scVariant, "targetCommit": "6-sha"},
			{"name": "fp16", "targetCommit": "fp-sha"},
		},
		"tags": []map[string]string{{"name": "5.00bpw", "targetCommit": "tag-sha"}},
	}
	refsJSON, _ := json.Marshal(refs)
	var sizePages atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			revision := strings.SplitN(r.URL.Path, "/tree/", 2)[1]
			if cursor := r.URL.Query().Get("cursor"); cursor != "" {
				if revision != scVariant || cursor != "2" {
					t.Errorf("unexpected paged tree request: %s", r.URL)
					http.NotFound(w, r)
					return
				}
				sizePages.Add(1)
				io.WriteString(w, `[{"type":"file","path":"config.json","size":10},{"type":"file","path":"tokenizer.json","size":20},{"type":"file","path":"quantization_config.json","size":10}]`)
				return
			}
			switch revision {
			case "main":
				io.WriteString(w, `[{"type":"file","path":"README.md","size":1},{"type":"file","path":"config.json","size":10}]`)
			case balanced:
				io.WriteString(w, `[{"type":"file","path":"config.json","size":10},{"type":"file","path":"model-4.safetensors","size":400}]`)
			case scVariant:
				w.Header().Set("Link", fmt.Sprintf("<%s?cursor=2>; rel=next", r.URL.Path))
				sizePages.Add(1)
				io.WriteString(w, `[{"type":"file","path":"model-6.safetensors","size":600}]`)
			default:
				t.Errorf("unexpected tree revision %q", revision)
				http.NotFound(w, r)
			}
		case strings.HasSuffix(r.URL.Path, "/refs"):
			w.Write(refsJSON)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/raw/"):
			parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"+repo+"/raw/"), "/", 2)
			if len(parts) != 2 {
				http.NotFound(w, r)
				return
			}
			revision, file := parts[0], parts[1]
			switch {
			case revision == "main" && file == "config.json":
				io.WriteString(w, mainConfig)
			case revision == balanced && file == "quantization_config.json":
				io.WriteString(w, `{"quant_method":"exl3","bits":4,"bits_per_weight":4.0,"head_bits":5}`)
			case revision == scVariant && file == "quantization_config.json":
				io.WriteString(w, `{"quant_method":"exl3","bits":4,"bits_per_weight":6.0,"head_bits":6}`)
			default:
				http.NotFound(w, r)
			}
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()})
	mainInfo, err := analyzer.AnalyzeModelWithRevision(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("analyze main: %v", err)
	}
	if len(mainInfo.SelectableItems) != 2 {
		t.Fatalf("branch options = %d, want 2: %+v", len(mainInfo.SelectableItems), mainInfo.SelectableItems)
	}
	items := make(map[string]SelectableItem, len(mainInfo.SelectableItems))
	recommended := 0
	for _, item := range mainInfo.SelectableItems {
		items[item.Revision] = item
		if !item.BranchVariant {
			t.Fatalf("EXL branch item lacks storage intent marker: %+v", item)
		}
		if item.Recommended {
			recommended++
		}
		if item.FilterValue != "" || len(item.Files) != 0 {
			t.Fatalf("branch choice must use revision, not a path/filter selector: %+v", item)
		}
	}
	if recommended != 1 {
		t.Fatalf("recommended branch count = %d, want exactly 1", recommended)
	}
	if got := items[balanced]; got.Revision != balanced || got.Method != "exl3" || got.BitsPerWeight != 4 || got.HeadBits != 5 || got.Size != 410 {
		t.Fatalf("balanced branch item = %+v", got)
	}
	if got := items[scVariant]; got.Revision != scVariant || got.Method != "exl3" || got.BitsPerWeight != 6 || got.HeadBits != 6 || got.Size != 640 {
		t.Fatalf("SC branch item = %+v", got)
	}
	if items[balanced].Recommended == false || items[scVariant].Recommended {
		t.Fatalf("expected closest-to-4 BPW branch %q to be the sole recommendation: %+v", balanced, items)
	}
	if mainInfo.RecommendedDownload == nil || mainInfo.RecommendedDownload.Revision != balanced || !mainInfo.RecommendedDownload.BranchVariant || len(mainInfo.RecommendedDownload.Filters) != 0 || mainInfo.RecommendedDownload.ExactMatch {
		t.Fatalf("recommended download = %+v; want unfiltered branch revision %q", mainInfo.RecommendedDownload, balanced)
	}
	if sizePages.Load() != 2 {
		t.Fatalf("SC branch tree pages = %d, want both pages", sizePages.Load())
	}

	selected, err := analyzer.AnalyzeModelWithRevision(context.Background(), repo, scVariant)
	if err != nil {
		t.Fatalf("analyze selected branch: %v", err)
	}
	if selected.Branch != scVariant || selected.Quantized == nil || selected.Quantized.Method != "exl3" || selected.Quantized.Bits != 0 || !selected.Quantized.MixedPrecision || selected.Quantized.HeadBits != 6 || selected.Quantized.BitsPerWeight != 6 {
		t.Fatalf("selected branch quantization = %+v (branch %q)", selected.Quantized, selected.Branch)
	}
	branchConfig, ok := selected.Metadata["quantization_config.json"].(map[string]interface{})
	if !ok || branchConfig["bits"] != float64(4) {
		t.Fatalf("selected branch lost raw nominal bits metadata: %+v", selected.Metadata["quantization_config.json"])
	}
	paths := make([]string, 0, len(selected.Files))
	for _, file := range selected.Files {
		paths = append(paths, file.Path)
	}
	if !reflect.DeepEqual(paths, []string{"model-6.safetensors", "config.json", "tokenizer.json", "quantization_config.json"}) {
		t.Fatalf("selected branch files = %v, want branch-only files", paths)
	}
}

func TestEXLBranchVariantsRemainAvailableAfterSpecializedMetadataClassification(t *testing.T) {
	const (
		repo         = "owner/model-exl3"
		branch       = "4.00bpw"
		mainConfig   = `{"vision_config":{"model_type":"siglip","image_size":224},"text_config":{"model_type":"llama","max_position_embeddings":4096}}`
		branchConfig = `{"vision_config":{"model_type":"siglip","image_size":224},"text_config":{"model_type":"llama","max_position_embeddings":4096},"quantization_config":{"quant_method":"exl3","bits":4,"bits_per_weight":4,"head_bits":6}}`
	)
	var branchTreeRequests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			revision := strings.SplitN(r.URL.Path, "/tree/", 2)[1]
			switch revision {
			case "main":
				io.WriteString(w, `[{"type":"file","path":"config.json","size":100}]`)
			case branch:
				branchTreeRequests.Add(1)
				io.WriteString(w, `[{"type":"file","path":"config.json","size":100},{"type":"file","path":"model.safetensors","size":400}]`)
			default:
				t.Errorf("unexpected tree revision %q", revision)
				http.NotFound(w, r)
			}
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/raw/"):
			parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"+repo+"/raw/"), "/", 2)
			if len(parts) != 2 {
				http.NotFound(w, r)
				return
			}
			revision, file := parts[0], parts[1]
			switch {
			case revision == "main" && file == "config.json":
				io.WriteString(w, mainConfig)
			case revision == branch && file == "config.json":
				io.WriteString(w, branchConfig)
			default:
				http.NotFound(w, r)
			}
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()})
	mainInfo, err := analyzer.AnalyzeModelWithRevision(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("analyze specialized main: %v", err)
	}
	if mainInfo.Type != TypeMultimodal || mainInfo.Multimodal == nil || mainInfo.Multimodal.VisionEncoder != "siglip" || mainInfo.Multimodal.TextEncoder != "llama" {
		t.Fatalf("main modality projection = type %q, multimodal %+v", mainInfo.Type, mainInfo.Multimodal)
	}
	if len(mainInfo.SelectableItems) != 1 {
		t.Fatalf("specialized main branch choices = %+v, want the EXL branch while retaining multimodal classification", mainInfo.SelectableItems)
	}
	item := mainInfo.SelectableItems[0]
	if item.Revision != branch || item.Method != "exl3" || item.BitsPerWeight != 4 || item.HeadBits != 6 || !item.Recommended {
		t.Fatalf("specialized main branch choice = %+v", item)
	}
	if mainInfo.RecommendedDownload == nil || mainInfo.RecommendedDownload.Revision != branch || len(mainInfo.RecommendedDownload.Filters) != 0 {
		t.Fatalf("specialized main recommendation = %+v, want unfiltered branch revision %q", mainInfo.RecommendedDownload, branch)
	}
	if branchTreeRequests.Load() == 0 {
		t.Fatal("specialized main did not inspect the branch tree for optional size enrichment")
	}

	selected, err := analyzer.AnalyzeModelWithRevision(context.Background(), repo, branch)
	if err != nil {
		t.Fatalf("analyze selected specialized branch: %v", err)
	}
	if selected.Branch != branch || selected.Type != TypeMultimodal || selected.Multimodal == nil || selected.Multimodal.VisionEncoder != "siglip" || selected.Multimodal.TextEncoder != "llama" {
		t.Fatalf("selected branch lost revision or modality projection: branch=%q type=%q multimodal=%+v", selected.Branch, selected.Type, selected.Multimodal)
	}
	if selected.Quantized == nil || selected.Quantized.Method != "exl3" || selected.Quantized.HeadBits != 6 {
		t.Fatalf("selected specialized branch lost nested quantization projection: %+v", selected.Quantized)
	}
}

// TestEXLBranchVariantsOfferedWhenMainCarriesWeights pins the availability
// rule next to the preserved main analysis: a main revision that carries
// model weights keeps its type, quantization projection, and metadata, and
// the bitrate-shaped branch ref is still offered. Visibility no longer
// depends on guessing the main tree's file content, so no safetensors header
// probe runs against the weight file at all. Precedence is pinned beside it:
// because the main tree carries recognized payload by name (root
// model.safetensors), the offered row carries no Recommended badge and no
// branch-variant recommendation appears — this safetensors-only Transformers
// analysis produced no file recommendation at base (rows were hidden there),
// so it must not gain one from the now-visible branch row.
func TestEXLBranchVariantsOfferedWhenMainCarriesWeights(t *testing.T) {
	const repo = "owner/exl3-model"
	var headerRequests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			if strings.HasSuffix(r.URL.Path, "/tree/main") {
				io.WriteString(w, `[{"type":"file","path":"config.json","size":10},{"type":"file","path":"model.safetensors","size":500},{"type":"file","path":"quantization_config.json","size":20}]`)
				return
			}
			io.WriteString(w, `[{"type":"file","path":"model.safetensors","size":400}]`)
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/raw/main/model.safetensors"),
			strings.HasSuffix(r.URL.Path, "/resolve/main/model.safetensors"):
			headerRequests.Add(1)
			io.WriteString(w, "weight content must never be probed for branch visibility")
		case strings.HasSuffix(r.URL.Path, "/raw/main/config.json"):
			io.WriteString(w, `{"model_type":"llama"}`)
		case strings.HasSuffix(r.URL.Path, "/raw/main/quantization_config.json"):
			io.WriteString(w, `{"quant_method":"exl3","bits":4,"head_bits":6}`)
		case strings.Contains(r.URL.Path, "/raw/4.00bpw/"):
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).AnalyzeModelWithRevision(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("analyze main with weights: %v", err)
	}
	if info.Type != TypeTransformers || info.Quantized == nil || info.Quantized.Method != "exl3" {
		t.Fatalf("main analysis changed: type=%s quantized=%+v", info.Type, info.Quantized)
	}
	if len(info.AnalysisWarnings) != 0 {
		t.Fatalf("analysis with weights produced a warning: %+v", info.AnalysisWarnings)
	}
	items := exlBranchSelectableItems(info)
	if len(items) != 1 {
		t.Fatalf("branch options = %+v, want the shaped ref offered despite main weights", items)
	}
	item := items[0]
	if item.Revision != "4.00bpw" || !item.BranchVariant || item.Method != "exl3" {
		t.Fatalf("branch item = %+v, want the offered EXL3 branch", item)
	}
	if item.Recommended {
		t.Fatalf("branch item = %+v, want no Recommended badge while the format analysis owns main-tree weights", item)
	}
	if info.RecommendedDownload != nil {
		t.Fatalf("recommended download = %+v, want nil: no file recommendation existed at base and the branch row must not create one", info.RecommendedDownload)
	}
	if headerRequests.Load() != 0 {
		t.Fatalf("main weight header requests = %d, want none: availability must not probe file content", headerRequests.Load())
	}
}

// TestRecommendationPrecedenceAcrossBranchAvailability pins requirement 2
// against requirement 3: the branch Recommended badge follows
// mainTreeCarriesPayload — names/layout decide, file names never prove
// calibration — so offering EXL-shaped branch rows never changes the
// recommendation a known format already produced from main-tree payload,
// while a classic EXL branch repository (config-only main, payloads on
// branches) keeps the branch-variant recommendation established by PR #152 —
// base parity with git show d7ba4fd:pkg/smartdl/branch_variants_test.go,
// whose classic-EXL cases asserted an unfiltered branch revision.
func TestRecommendationPrecedenceAcrossBranchAvailability(t *testing.T) {
	serve := func(t *testing.T, mainTree string, raw map[string]string) *httptest.Server {
		t.Helper()
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/tree/main"):
				io.WriteString(w, mainTree)
			case strings.Contains(r.URL.Path, "/tree/"):
				io.WriteString(w, `[]`)
			case strings.HasSuffix(r.URL.Path, "/refs"):
				io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
			case strings.HasSuffix(r.URL.Path, "/auth-check"):
				http.NotFound(w, r)
			case strings.Contains(r.URL.Path, "/raw/main/"):
				rel := r.URL.Path[strings.LastIndex(r.URL.Path, "/raw/main/")+len("/raw/main/"):]
				if body, ok := raw[rel]; ok {
					io.WriteString(w, body)
					return
				}
				http.NotFound(w, r)
			default:
				// Optional branch metadata and size lookups degrade silently.
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(hub.Close)
		return hub
	}

	t.Run("known format with weights on main keeps its file recommendation", func(t *testing.T) {
		hub := serve(t,
			`[{"type":"file","path":"config.json","size":10},{"type":"file","path":"model.safetensors","size":400},{"type":"file","path":"pytorch_model.bin","size":300}]`,
			map[string]string{"config.json": `{"model_type":"llama"}`})
		info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).
			AnalyzeModelWithRevision(context.Background(), "owner/example-EXL3-model", "main")
		if err != nil {
			t.Fatalf("analyze weights-on-main: %v", err)
		}
		if info.Type != TypeTransformers || info.Transformers == nil {
			t.Fatalf("main analysis = type %s, want a concrete Transformers file selection", info.Type)
		}
		items := exlBranchSelectableItems(info)
		if len(items) != 1 {
			t.Fatalf("branch rows = %+v, want the shaped ref offered alongside the format selection", items)
		}
		if item := items[0]; item.Revision != "4.00bpw" || !item.BranchVariant {
			t.Fatalf("branch row = %+v, want the offered, downloadable branch variant", item)
		} else if item.Recommended {
			t.Fatalf("branch row = %+v, want no Recommended badge while the format owns a main-tree selection", item)
		}
		rd := info.RecommendedDownload
		if rd == nil || rd.BranchVariant || rd.Revision != "" {
			t.Fatalf("recommended download = %+v, want the format's file selection, not a branch variant", rd)
		}
		if want := []string{"safetensors"}; !reflect.DeepEqual(rd.Filters, want) {
			t.Fatalf("recommended filters = %v, want the format selection %v", rd.Filters, want)
		}
	})

	t.Run("GGUF weights on main keep the artifact recommendation", func(t *testing.T) {
		hub := serve(t,
			`[{"type":"file","path":"config.json","size":10},{"type":"file","path":"model-Q4_K_M.gguf","size":4000000000}]`,
			map[string]string{"config.json": `{"model_type":"llama"}`})
		info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).
			AnalyzeModelWithRevision(context.Background(), "owner/example-EXL3-model", "main")
		if err != nil {
			t.Fatalf("analyze GGUF weights-on-main: %v", err)
		}
		if info.Type != TypeGGUF || info.GGUF == nil {
			t.Fatalf("main analysis = type %s, want a concrete GGUF file selection", info.Type)
		}
		items := exlBranchSelectableItems(info)
		if len(items) != 1 {
			t.Fatalf("branch rows = %+v, want the shaped ref offered alongside the GGUF selection", items)
		}
		if item := items[0]; item.Revision != "4.00bpw" || !item.BranchVariant || item.Recommended {
			t.Fatalf("branch row = %+v, want an offered, unrecommended branch variant", item)
		}
		rd := info.RecommendedDownload
		if rd == nil || rd.BranchVariant || rd.Revision != "" || !rd.ExactMatch {
			t.Fatalf("recommended download = %+v, want the GGUF artifact request, not a branch variant", rd)
		}
		if want := []string{"./model-Q4_K_M.gguf"}; !reflect.DeepEqual(rd.Filters, want) {
			t.Fatalf("recommended filters = %v, want the GGUF artifact %v", rd.Filters, want)
		}
	})

	t.Run("classic EXL branch repository keeps the branch recommendation", func(t *testing.T) {
		hub := serve(t, `[{"type":"file","path":"config.json","size":10}]`,
			map[string]string{"config.json": `{"model_type":"llama"}`})
		info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).
			AnalyzeModelWithRevision(context.Background(), "owner/example-EXL3-model", "main")
		if err != nil {
			t.Fatalf("analyze config-only main: %v", err)
		}
		items := exlBranchSelectableItems(info)
		if len(items) != 1 || !items[0].Recommended {
			t.Fatalf("branch rows = %+v, want the sole row recommended without a competing file selection", items)
		}
		rd := info.RecommendedDownload
		if rd == nil || rd.Revision != "4.00bpw" || !rd.BranchVariant || len(rd.Filters) != 0 || rd.ExactMatch {
			t.Fatalf("recommended download = %+v; want unfiltered branch revision %q (base parity)", rd, "4.00bpw")
		}
	})

	analyze := func(t *testing.T, hub *httptest.Server) *RepoInfo {
		t.Helper()
		info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).
			AnalyzeModelWithRevision(context.Background(), "owner/example-EXL3-model", "main")
		if err != nil {
			t.Fatalf("analyze fixture main: %v", err)
		}
		return info
	}
	// offeredAndUnbadged asserts the availability half of the rule: the shaped
	// ref is listed and downloadable, and payload on main withholds the badge.
	offeredAndUnbadged := func(t *testing.T, info *RepoInfo) {
		t.Helper()
		items := exlBranchSelectableItems(info)
		if len(items) != 1 {
			t.Fatalf("branch rows = %+v, want the shaped ref offered", items)
		}
		if item := items[0]; item.Revision != "4.00bpw" || !item.BranchVariant || item.Recommended {
			t.Fatalf("branch row = %+v, want an offered, unrecommended branch variant while main carries payload", item)
		}
	}

	t.Run("specialized multimodal with root payload keeps nil recommendation", func(t *testing.T) {
		// F1a: reclassification hides info.Transformers, but the root payload
		// still decides precedence by name — base hid the rows behind this
		// payload (mainWeightEvidencePresent) and the reclassified type yields
		// no selectable items, so base recommendation was nil.
		hub := serve(t,
			`[{"type":"file","path":"config.json","size":10},{"type":"file","path":"model.safetensors","size":400}]`,
			map[string]string{"config.json": `{"vision_config":{"model_type":"siglip","image_size":224},"text_config":{"model_type":"llama","max_position_embeddings":4096}}`})
		info := analyze(t, hub)
		if info.Type != TypeMultimodal {
			t.Fatalf("main analysis = type %s (metadata %+v, warnings %+v), want specialized reclassification over root payload", info.Type, info.Metadata, info.AnalysisWarnings)
		}
		offeredAndUnbadged(t, info)
		if info.RecommendedDownload != nil {
			t.Fatalf("recommended download = %+v, want nil (base parity: payload hid the rows, the type yields no items)", info.RecommendedDownload)
		}
	})

	t.Run("LoRA with adapter payload keeps nil recommendation", func(t *testing.T) {
		hub := serve(t,
			`[{"type":"file","path":"adapter_config.json","size":300},{"type":"file","path":"adapter_model.safetensors","size":1200}]`,
			map[string]string{"adapter_config.json": `{"r":16,"lora_alpha":32,"target_modules":["q_proj"]}`})
		info := analyze(t, hub)
		if info.Type != TypeLoRA {
			t.Fatalf("main analysis = type %s, want LoRA classification", info.Type)
		}
		offeredAndUnbadged(t, info)
		if info.RecommendedDownload != nil {
			t.Fatalf("recommended download = %+v, want nil (base parity: payload hid the rows, LoRA yields no selectable items)", info.RecommendedDownload)
		}
	})

	t.Run("payload-less diffusers main recommends the branch", func(t *testing.T) {
		// Base parity: no weight-shaped file anywhere → mainWeightEvidenceAbsent
		// → base showed the rows and badged one, and the unfiltered branch won
		// the recommendation.
		hub := serve(t,
			`[{"type":"file","path":"model_index.json","size":500},{"type":"file","path":"README.md","size":100}]`,
			map[string]string{"model_index.json": `{"_class_name":"StableDiffusionPipeline","unet":{"_class_name":"UNet2DConditionModel"},"vae":{"_class_name":"AutoencoderKL"}}`})
		info := analyze(t, hub)
		if info.Type != TypeDiffusers {
			t.Fatalf("main analysis = type %s, want Diffusers classification", info.Type)
		}
		items := exlBranchSelectableItems(info)
		if len(items) != 1 || items[0].Revision != "4.00bpw" || !items[0].BranchVariant || !items[0].Recommended {
			t.Fatalf("branch rows = %+v, want the badged branch: payload-less main means the branch is the actual selection (base parity)", items)
		}
		rd := info.RecommendedDownload
		if rd == nil || rd.Revision != "4.00bpw" || !rd.BranchVariant || len(rd.Filters) != 0 {
			t.Fatalf("recommended download = %+v, want unfiltered branch revision %q (base parity)", rd, "4.00bpw")
		}
	})

	t.Run("subdirectory payload keeps the diffusers component recommendation", func(t *testing.T) {
		// Seam closure: payload recognition spans the whole main tree like
		// base's classifyMainWeights scan — unet/diffusion_pytorch_model.safetensors
		// counts, so no badge and the component selection stands (base hid the
		// rows and preserved it), while the branch row stays offered.
		hub := serve(t,
			`[{"type":"file","path":"model_index.json","size":500},{"type":"file","path":"README.md","size":100},{"type":"file","path":"unet/diffusion_pytorch_model.safetensors","size":400}]`,
			map[string]string{"model_index.json": `{"_class_name":"StableDiffusionPipeline","unet":["diffusers","UNet2DConditionModel"],"vae":["diffusers","AutoencoderKL"]}`})
		info := analyze(t, hub)
		if info.Type != TypeDiffusers {
			t.Fatalf("main analysis = type %s, want Diffusers classification", info.Type)
		}
		offeredAndUnbadged(t, info)
		rd := info.RecommendedDownload
		if rd == nil || rd.BranchVariant || rd.Revision != "" {
			t.Fatalf("recommended download = %+v, want the diffusers component selection, not a branch variant", rd)
		}
		filters := append([]string(nil), rd.Filters...)
		sort.Strings(filters)
		// Authorized B3 shape shift: the weighted unet now contributes its
		// chosen weight set as exact path selectors (the GGUF-artifact
		// mechanism), while the weightless vae keeps its legacy folder token.
		if want := []string{"unet/diffusion_pytorch_model.safetensors", "vae"}; !reflect.DeepEqual(filters, want) {
			t.Fatalf("recommended filters = %v, want the chosen weight set plus the weightless folder filter %v (base hid the rows via its whole-tree payload scan)", filters, want)
		}
	})

	t.Run("marker-bearing model.pt keeps base's hidden-rows outcome", func(t *testing.T) {
		// (f) Base's name table: ext .pt is weight-ish and "model" is a
		// weight-role marker → Present → base hid the rows; format/nil
		// recommendation stands. A bespoke extension list without .pt would
		// wrongly Absent-badge the branch here.
		hub := serve(t,
			`[{"type":"file","path":"config.json","size":10},{"type":"file","path":"model.pt","size":400}]`,
			map[string]string{"config.json": `{"model_type":"llama"}`})
		info := analyze(t, hub)
		offeredAndUnbadged(t, info)
		if info.RecommendedDownload != nil {
			t.Fatalf("recommended download = %+v, want nil (base hid the rows on name evidence; this Generic tree yields no items)", info.RecommendedDownload)
		}
	})

	t.Run("clean data-only main still recommends the branch", func(t *testing.T) {
		// (g) Base's name table: neither .md nor .json is weight-shaped →
		// Absent → base showed the rows and badged one (base parity with the
		// classic unfiltered-branch expectation).
		hub := serve(t,
			`[{"type":"file","path":"README.md","size":100},{"type":"file","path":"measurement.json","size":60916}]`,
			nil)
		info := analyze(t, hub)
		items := exlBranchSelectableItems(info)
		if len(items) != 1 || items[0].Revision != "4.00bpw" || !items[0].BranchVariant || !items[0].Recommended {
			t.Fatalf("branch rows = %+v, want the badged branch on a clean Absent main (base parity)", items)
		}
		rd := info.RecommendedDownload
		if rd == nil || rd.Revision != "4.00bpw" || !rd.BranchVariant || len(rd.Filters) != 0 || rd.ExactMatch {
			t.Fatalf("recommended download = %+v, want unfiltered branch revision %q (base parity)", rd, "4.00bpw")
		}
	})

	t.Run("unrecognized weight-ish name hides the badge like base's Unknown", func(t *testing.T) {
		// (h) Base's name table: .bin that is neither tokenizer/vocab nor
		// marker-bearing (training_args.bin — base's own comment example) is
		// Unknown, which also hid rows at base; the candidate never warns
		// (requirement 3) and this Transformers tree has no selectable items
		// (analyzeWeightFiles skips training artifacts), so the name-based
		// output is nil.
		hub := serve(t,
			`[{"type":"file","path":"config.json","size":10},{"type":"file","path":"training_args.bin","size":900}]`,
			map[string]string{"config.json": `{"model_type":"llama"}`})
		info := analyze(t, hub)
		if info.Type != TypeTransformers {
			t.Fatalf("main analysis = type %s, want Transformers (config + root .bin)", info.Type)
		}
		if len(info.AnalysisWarnings) != 0 {
			t.Fatalf("Unknown-class name produced an analysis warning: %+v (candidate availability never warns)", info.AnalysisWarnings)
		}
		offeredAndUnbadged(t, info)
		if info.RecommendedDownload != nil {
			t.Fatalf("recommended download = %+v, want nil (base hid the rows for Unknown too; no selectable items exist)", info.RecommendedDownload)
		}
	})

	t.Run("renamed weight-looking file counts as payload by name", func(t *testing.T) {
		// Names decide in both directions: a renamed weight-shaped file is
		// payload wherever it sits in the main tree, exactly like
		// model.safetensors, so no badge and the name-based output (nil for
		// this config-less Generic tree) stands.
		hub := serve(t,
			`[{"type":"file","path":"README.md","size":100},{"type":"file","path":"renamed-trace.safetensors","size":1024}]`,
			nil)
		info := analyze(t, hub)
		offeredAndUnbadged(t, info)
		if info.RecommendedDownload != nil {
			t.Fatalf("recommended download = %+v, want the name-based output (nil for this config-less Generic tree)", info.RecommendedDownload)
		}
	})
}

// TestEXLBranchVariantsIgnoreMainCalibrationFiles pins that calibration
// artifacts on main neither gate the branch row nor invite a header probe:
// the branch is listed from its ref shape, size enrichment still runs, and
// the analyzer never reads the main tree's safetensors content. Recommendation
// follows the names-decide rule (user decision, provenance: interactive page
// «Калибровочный шов: имена против пробы заголовка», answer «Имена решают
// (рекомендация)»): a root calibration-batch-001.safetensors looks like
// payload by name, so the row gets no Recommended badge and the
// recommendation stays the analysis's name-based output (nil for this
// config-less Generic tree) — file names never prove calibration, and the
// branch remains manually selectable through the always-offered row.
func TestEXLBranchVariantsIgnoreMainCalibrationFiles(t *testing.T) {
	const (
		repo     = "turboderp/Qwen3.8-27B-exl3"
		revision = "4.00bpw"
	)
	var headerRequests, branchTreeRequests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			branch := strings.SplitN(r.URL.Path, "/tree/", 2)[1]
			switch branch {
			case "main":
				io.WriteString(w, `[{"type":"file","path":".gitattributes","size":1519},{"type":"file","path":"README.md","size":3481},{"type":"file","path":"cal_trace.json","size":3410836},{"type":"file","path":"calibration-batch-001.safetensors","size":4096088,"lfs":{"size":4096088,"oid":"a034bee4c7920b4ac2e62b9d881a9b118488c414a81cf89aa64bd9274c47d685"}},{"type":"file","path":"recipe_4.00bpw.yaml","size":21487}]`)
			case revision, "4_25":
				branchTreeRequests.Add(1)
				io.WriteString(w, `[{"type":"file","path":"config.json","size":4621},{"type":"file","path":"model-00001-of-00002.safetensors","size":8471336006},{"type":"file","path":"model-00002-of-00002.safetensors","size":8389473789}]`)
			default:
				t.Errorf("unexpected tree revision %q", branch)
				http.NotFound(w, r)
			}
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/resolve/main/calibration-batch-001.safetensors"),
			strings.HasSuffix(r.URL.Path, "/raw/main/calibration-batch-001.safetensors"):
			headerRequests.Add(1)
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/raw/4.00bpw/quantization_config.json"):
			io.WriteString(w, `{"quant_method":"exl3","bits_per_weight":4}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).AnalyzeModelWithRevision(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("analyze calibration-only main: %v", err)
	}
	if len(info.SelectableItems) != 1 || info.SelectableItems[0].Revision != revision || info.SelectableItems[0].Method != "exl3" {
		t.Fatalf("calibration main tree hid EXL branch: %+v", info.SelectableItems)
	}
	if info.SelectableItems[0].Recommended {
		t.Fatalf("branch row = %+v, want no Recommended badge: names decide, a root calibration-shaped safetensors counts as payload", info.SelectableItems[0])
	}
	if len(info.AnalysisWarnings) != 0 {
		t.Fatalf("calibration main tree produced an analysis warning: %+v", info.AnalysisWarnings)
	}
	if info.RecommendedDownload != nil {
		t.Fatalf("recommended download = %+v, want the name-based output (nil for this config-less Generic tree), never the branch", info.RecommendedDownload)
	}
	if headerRequests.Load() != 0 {
		t.Fatalf("calibration header requests = %d, want none: availability must not probe file content", headerRequests.Load())
	}
	if branchTreeRequests.Load() == 0 {
		t.Fatal("branch row did not inspect the real branch for size enrichment")
	}
}

// TestEXLBranchVisibilityNeverDependsOnMainFileContent pins the availability
// rule across every main-tree shape that used to gate branch eligibility
// through safetensors header probes: whatever those headers would say (model
// weight, calibration-only, malformed, unavailable, hanging, over the probe
// budget, a Range-ignoring body, or no safetensors at all), the shaped branch
// ref is offered, no header request is made, and no analysis warning is
// emitted — an unknown file role is a normal state, not a blocker.
func TestEXLBranchVisibilityNeverDependsOnMainFileContent(t *testing.T) {
	overBudgetFiles := make([]exlHeaderFixtureFile, 0, 17)
	for i := 0; i < 17; i++ {
		overBudgetFiles = append(overBudgetFiles, exlHeaderFixtureFile{
			path: fmt.Sprintf("calibration-%02d.safetensors", i),
			size: 1024,
		})
	}
	for _, tc := range []struct {
		name  string
		files []exlHeaderFixtureFile
	}{
		{name: "model weight header", files: []exlHeaderFixtureFile{{path: "model.safetensors", size: 1024}}},
		{name: "calibration-only header", files: []exlHeaderFixtureFile{{path: "renamed-trace.safetensors", size: 1024}}},
		{name: "malformed header", files: []exlHeaderFixtureFile{{path: "malformed.safetensors", size: 1024}}},
		{name: "unavailable header endpoint", files: []exlHeaderFixtureFile{{path: "unavailable.safetensors", mode: "unavailable", size: 1024}}},
		{name: "hanging header endpoint", files: []exlHeaderFixtureFile{{path: "slow.safetensors", mode: "timeout", size: 1024}}},
		{name: "file count over the old probe budget", files: overBudgetFiles},
		{name: "Range-ignoring 200 body", files: []exlHeaderFixtureFile{{path: "opaque-export.safetensors", mode: "range", size: 1024}}},
		{name: "no safetensors at all", files: []exlHeaderFixtureFile{{path: "README.md", size: 10}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, headerRequests := analyzeEXLHeaderFixture(t, tc.files)
			branchRows := make([]string, 0, 1)
			for _, item := range info.SelectableItems {
				if item.Revision != "" {
					branchRows = append(branchRows, item.Revision)
				}
			}
			if !reflect.DeepEqual(branchRows, []string{"4.00bpw"}) {
				t.Fatalf("branch rows = %v, want the shaped ref offered for any main content", branchRows)
			}
			if headerRequests != 0 {
				t.Fatalf("header HTTP requests = %d, want none: availability must not probe file content", headerRequests)
			}
			if len(info.AnalysisWarnings) != 0 {
				t.Fatalf("analysis warnings = %+v, want none: nothing is hidden, so nothing is incomplete", info.AnalysisWarnings)
			}
		})
	}
}

type exlHeaderFixtureFile struct {
	path string
	mode string
	size int64
}

// analyzeEXLHeaderFixture exercises the production analyzer path with a fake
// Hub whose main tree varies only in the file content that used to gate
// branch visibility. The weight endpoints answer per mode and are counted:
// availability must never reach them, so the answers only document the old
// trigger shapes.
func analyzeEXLHeaderFixture(t *testing.T, files []exlHeaderFixtureFile) (*RepoInfo, int64) {
	t.Helper()
	const repo = "owner/exl3-model"
	byPath := make(map[string]exlHeaderFixtureFile, len(files))
	tree := make([]map[string]interface{}, 0, len(files))
	for _, file := range files {
		byPath[file.path] = file
		tree = append(tree, map[string]interface{}{"type": "file", "path": file.path, "size": file.size})
	}
	var headerRequests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/"+repo+"/tree/main":
			if err := json.NewEncoder(w).Encode(tree); err != nil {
				t.Errorf("encode main tree: %v", err)
			}
		case r.URL.Path == "/api/models/"+repo+"/refs":
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/tree/"):
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/"+repo+"/raw/main/"):
			path := strings.TrimPrefix(r.URL.Path, "/"+repo+"/raw/main/")
			if _, ok := byPath[path]; !ok {
				http.NotFound(w, r)
				return
			}
			headerRequests.Add(1)
			switch byPath[path].mode {
			case "unavailable":
				http.Error(w, "UPSTREAM_BODY_MUST_NOT_LEAK", http.StatusServiceUnavailable)
			case "timeout":
				<-r.Context().Done()
			case "range":
				w.Header().Set("Content-Length", "16")
				w.WriteHeader(http.StatusOK) // This endpoint ignores Range and serves a complete body.
				_, _ = w.Write([]byte("range unsupported"))
			default:
				io.WriteString(w, "unused header content")
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).AnalyzeModelWithRevision(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("analyze fixture main: %v", err)
	}
	return info, headerRequests.Load()
}

func TestAnalyzeEXLBranchNestedQuantizationConfigWithoutRepoNameHint(t *testing.T) {
	const (
		repo       = "owner/metadata-model"
		revision   = "4.25bpw"
		config     = `{"architectures":["Qwen2ForCausalLM"],"model_type":"qwen2","quantization_config":{"quant_method":"exl2","bits":4.25,"head_bits":6}}`
		staleIndex = `{"metadata":{"total_size":15231233024},"weight_map":{"model.embed_tokens.weight":"model-00001-of-00004.safetensors","lm_head.weight":"model-00004-of-00004.safetensors"}}`
	)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			branch := strings.SplitN(r.URL.Path, "/tree/", 2)[1]
			switch branch {
			case "main":
				io.WriteString(w, `[{"type":"file","path":"README.md","size":10},{"type":"file","path":"config.json","size":160}]`)
			case revision, "4_25":
				io.WriteString(w, `[{"type":"file","path":"config.json","size":160},{"type":"file","path":"output.safetensors","size":500},{"type":"file","path":"model.safetensors.index.json","size":40}]`)
			default:
				t.Errorf("unexpected tree revision %q", branch)
				http.NotFound(w, r)
			}
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.25bpw"},{"name":"4_25"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/raw/main/config.json"):
			io.WriteString(w, config)
		case strings.HasSuffix(r.URL.Path, "/raw/4.25bpw/config.json"), strings.HasSuffix(r.URL.Path, "/raw/4_25/config.json"):
			io.WriteString(w, config)
		case strings.HasSuffix(r.URL.Path, "/raw/4.25bpw/model.safetensors.index.json"):
			io.WriteString(w, staleIndex)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()})
	mainInfo, err := analyzer.AnalyzeModelWithRevision(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("analyze main: %v", err)
	}
	if len(mainInfo.SelectableItems) != 2 {
		t.Fatalf("nested branch options = %+v, want metadata-identified BPW and N_M EXL branches", mainInfo.SelectableItems)
	}
	item := mainInfo.SelectableItems[0]
	if item.Revision != revision || item.Method != "exl2" || item.BitsPerWeight != 4.25 || item.HeadBits != 6 || !item.Recommended {
		t.Fatalf("nested branch option = %+v", item)
	}
	if mainInfo.RecommendedDownload == nil || mainInfo.RecommendedDownload.Revision != revision || len(mainInfo.RecommendedDownload.Filters) != 0 {
		t.Fatalf("nested branch recommendation = %+v", mainInfo.RecommendedDownload)
	}

	selected, err := analyzer.AnalyzeModelWithRevision(context.Background(), repo, revision)
	if err != nil {
		t.Fatalf("analyze selected nested-config branch: %v", err)
	}
	if selected.Type != TypeTransformers || selected.Branch != revision || selected.Quantized == nil || selected.Quantized.Method != "exl2" || selected.Quantized.Bits != 0 || !selected.Quantized.MixedPrecision || selected.Quantized.HeadBits != 6 || selected.Quantized.BitsPerWeight != 0 {
		t.Fatalf("selected nested quantization projection/type = %+v / %s", selected.Quantized, selected.Type)
	}
	outer, ok := selected.Metadata["config.json"].(map[string]interface{})
	if !ok || outer["model_type"] != "qwen2" {
		t.Fatalf("selected raw config lost outer model metadata: %+v", selected.Metadata["config.json"])
	}
	nested, ok := outer["quantization_config"].(map[string]interface{})
	if !ok || nested["bits"] != float64(4.25) || nested["head_bits"] != float64(6) || nested["quant_method"] != "exl2" {
		t.Fatalf("selected raw nested config changed: %+v", outer["quantization_config"])
	}
	if selected.Transformers == nil || len(selected.Transformers.WeightFiles) != 1 || selected.Transformers.WeightFiles[0].Path != "output.safetensors" {
		t.Fatalf("selected branch lost its actual output.safetensors payload: %+v", selected.Transformers)
	}
}

// TestEXLBranchConfigEnrichmentOverridesDisplay pins that branch-config
// reads only adjust display fields and never decide whether a row exists:
// an explicit EXL branch method wins over the main/repository display, a
// failed lookup keeps the caller's display, an unknown method string keeps
// it too, and a recognized non-EXL branch method clears the name-based EXL
// guess instead of claiming a flavor the config denies.
func TestEXLBranchConfigEnrichmentOverridesDisplay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/raw/exl-branch/quantization_config.json"):
			io.WriteString(w, `{"quant_method":"exl2","bits_per_weight":4.5,"head_bits":8}`)
		case strings.HasSuffix(r.URL.Path, "/raw/non-exl-branch/quantization_config.json"):
			io.WriteString(w, `{"quant_method":"gptq","bits":4}`)
		case strings.HasSuffix(r.URL.Path, "/raw/unknown-branch/quantization_config.json"):
			io.WriteString(w, `{"quant_method":"mystery_quant","bits":4}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a := NewAnalyzer(AnalyzerOptions{Endpoint: server.URL, HTTPClient: server.Client()})
	base := exlBranchDisplay{method: "exl3", bpw: 4.25, headBits: 6}

	got := a.lookupEXLBranch(context.Background(), "owner/exl3-model", exlBranchCandidate{revision: "exl-branch"}, base)
	if got.method != "exl2" || got.bpw != 4.5 || got.headBits != 8 {
		t.Fatalf("branch-config display = %+v, want exl2 with the config's widths", got)
	}

	got = a.lookupEXLBranch(context.Background(), "owner/exl3-model", exlBranchCandidate{revision: "missing"}, base)
	if got != base {
		t.Fatalf("failed lookup display = %+v, want the unchanged base %+v", got, base)
	}

	got = a.lookupEXLBranch(context.Background(), "owner/exl3-model", exlBranchCandidate{revision: "unknown-branch"}, base)
	if got.method != "exl3" {
		t.Fatalf("unknown branch method display = %+v, want the base method kept", got)
	}

	got = a.lookupEXLBranch(context.Background(), "owner/exl3-model", exlBranchCandidate{revision: "non-exl-branch"}, base)
	if got.method != "" || got.bpw != 4.25 || got.headBits != 6 {
		t.Fatalf("known non-EXL branch config display = %+v, want no EXL method claim and the name widths", got)
	}
}

// TestAnalyzeEXLBranchListedDespiteKnownNonEXLMainMethod pins the new
// availability rule for the known-non-EXL case: the shaped branch is offered
// even though the main configuration declares gptq, while the known non-EXL
// method still outranks the repository-name guess for display (no method
// claim), the main quantization metadata is preserved, and the offered row
// keeps a downloadable branch-variant request shape.
func TestAnalyzeEXLBranchListedDespiteKnownNonEXLMainMethod(t *testing.T) {
	const repo = "owner/exl3-model"
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			if strings.HasSuffix(r.URL.Path, "/tree/main") {
				io.WriteString(w, `[{"type":"file","path":"config.json","size":20},{"type":"file","path":"quantization_config.json","size":40}]`)
				return
			}
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/raw/main/config.json"):
			io.WriteString(w, `{"model_type":"llama"}`)
		case strings.HasSuffix(r.URL.Path, "/raw/main/quantization_config.json"):
			io.WriteString(w, `{"quant_method":"gptq","bits":4}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).AnalyzeModelWithRevision(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("analyze non-EXL main: %v", err)
	}
	mainConfig, ok := info.Metadata["quantization_config.json"].(map[string]interface{})
	if !ok || mainConfig["quant_method"] != "gptq" {
		t.Fatalf("main quantization metadata = %+v, want explicit gptq", info.Metadata["quantization_config.json"])
	}
	items := exlBranchSelectableItems(info)
	if len(items) != 1 {
		t.Fatalf("branch items = %+v, want the shaped ref offered despite the known non-EXL main method", items)
	}
	item := items[0]
	if item.Revision != "4.00bpw" || !item.BranchVariant || item.Method != "" {
		t.Fatalf("branch item = %+v, want no EXL method claim over known non-EXL evidence", item)
	}
	if info.RecommendedDownload == nil || !info.RecommendedDownload.BranchVariant || !BranchVariantRevisionShape(info.RecommendedDownload.Revision) {
		t.Fatalf("recommended download = %+v, want a downloadable branch-variant request", info.RecommendedDownload)
	}
}

// TestAnalyzeEXLBranchUsesMainConfigBeforeRepoNameFallback pins the method
// evidence order: the main config establishes the method, so a branch whose
// own config is unavailable is still classified as an EXL variant, and the
// repository name is only a fallback.
func TestAnalyzeEXLBranchUsesMainConfigBeforeRepoNameFallback(t *testing.T) {
	const mainConfig = `{"model_type":"llama","quantization_config":{"quant_method":"exl2"}}`
	for _, tc := range []struct {
		name   string
		repo   string
		branch string
		// branchFile is the only file of the branch tree.
		branchFile string
		branchSize int
	}{
		{name: "bpw branch of an EXL-named repository", repo: "owner/model-exl3", branch: "4.25bpw", branchFile: "README.md", branchSize: 10},
		{name: "N_M branch without a repository hint", repo: "owner/metadata-model", branch: "4_25", branchFile: "pytorch_model.bin", branchSize: 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mainConfigRequests, unavailableBranchConfigRequests atomic.Int64
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/tree/"):
					switch {
					case strings.HasSuffix(r.URL.Path, "/tree/main"):
						io.WriteString(w, `[{"type":"file","path":"config.json","size":100}]`)
					case strings.HasSuffix(r.URL.Path, "/tree/"+tc.branch):
						fmt.Fprintf(w, `[{"type":"file","path":%q,"size":%d}]`, tc.branchFile, tc.branchSize)
					default:
						t.Errorf("unexpected tree request %q", r.URL.Path)
						http.NotFound(w, r)
					}
				case strings.HasSuffix(r.URL.Path, "/refs"):
					fmt.Fprintf(w, `{"branches":[{"name":"main"},{"name":%q}],"tags":[]}`, tc.branch)
				case strings.HasSuffix(r.URL.Path, "/auth-check"):
					http.NotFound(w, r)
				case strings.Contains(r.URL.Path, "/raw/"):
					switch {
					case strings.HasSuffix(r.URL.Path, "/raw/main/config.json"):
						mainConfigRequests.Add(1)
						io.WriteString(w, mainConfig)
					case strings.Contains(r.URL.Path, "/raw/"+tc.branch+"/"):
						// The branch has no method metadata, so its output must
						// use the main config rather than accidentally proving
						// branch priority.
						unavailableBranchConfigRequests.Add(1)
						http.NotFound(w, r)
					default:
						t.Errorf("unexpected raw request %q", r.URL.Path)
						http.NotFound(w, r)
					}
				default:
					t.Errorf("unexpected request %q", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer hub.Close()

			info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).AnalyzeModelWithRevision(context.Background(), tc.repo, "main")
			if err != nil {
				t.Fatalf("analyze main: %v", err)
			}
			if mainConfigRequests.Load() == 0 {
				t.Fatal("analysis did not fetch the main config that establishes the method")
			}
			mainMetadata, ok := info.Metadata["config.json"].(map[string]interface{})
			if !ok {
				t.Fatalf("main config metadata = %+v, want fetched config object", info.Metadata["config.json"])
			}
			mainQuantization, ok := mainMetadata["quantization_config"].(map[string]interface{})
			if !ok || mainQuantization["quant_method"] != "exl2" {
				t.Fatalf("main quantization config = %+v, want explicit exl2", mainMetadata["quantization_config"])
			}
			if unavailableBranchConfigRequests.Load() == 0 {
				t.Fatal("analysis did not attempt optional branch-config lookup")
			}
			if len(info.SelectableItems) != 1 {
				t.Fatalf("selectable items = %+v, want the EXL branch", info.SelectableItems)
			}
			item := info.SelectableItems[0]
			if item.Revision != tc.branch || !item.BranchVariant || item.Method != "exl2" || item.BitsPerWeight != 4.25 || !item.Recommended {
				t.Fatalf("branch item = %+v, want recommended EXL2 4.25 BPW from main config", item)
			}
		})
	}
}

// TestEXLBranchMainMethodSurvivesOptionalLookupTimeout pins requirement 3:
// when the shared optional-lookup budget expires before any branch config is
// read, every candidate row still exists, carrying the display method the
// main configuration established, and the timeout itself is not a warning.
func TestEXLBranchMainMethodSurvivesOptionalLookupTimeout(t *testing.T) {
	oldWait := exlBranchLookupWait
	exlBranchLookupWait = 150 * time.Millisecond
	defer func() { exlBranchLookupWait = oldWait }()

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/raw/") {
			http.NotFound(w, r)
			return
		}
		<-r.Context().Done()
	}))
	defer hub.Close()

	refs := make([]RepoRef, 0, 5)
	for i := 1; i <= 5; i++ {
		refs = append(refs, RepoRef{Name: fmt.Sprintf("%d.00bpw", i), Type: "branch"})
	}
	info := &RepoInfo{
		Repo:      "owner/model",
		Refs:      refs,
		Quantized: &QuantizedInfo{Method: "exl2"},
	}
	items, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).exlBranchVariantItems(context.Background(), info)
	if err != nil {
		t.Fatalf("branch discovery with a slow optional lookup: %v", err)
	}
	if len(items) != len(refs) {
		t.Fatalf("main EXL2 method established %d of %d branch rows after optional timeout: %+v", len(items), len(refs), items)
	}
	for _, item := range items {
		if item.Method != "exl2" || !item.BranchVariant {
			t.Fatalf("display-degraded branch item = %+v; want the EXL2 branch row kept", item)
		}
	}
}

// TestEXLBranchLookupTimeoutNeverTruncatesVisibility pins requirement 3: the
// budgeted per-branch lookup can leave queued candidates unvisited when the
// optional-lookup budget expires, but every shaped ref keeps its row — the
// budget can only degrade display enrichment, never truncate the offered
// list — and queued candidates still never start before the shared deadline.
func TestEXLBranchLookupTimeoutNeverTruncatesVisibility(t *testing.T) {
	const repo = "owner/model-exl3"
	oldWait := exlBranchLookupWait
	exlBranchLookupWait = 150 * time.Millisecond
	defer func() { exlBranchLookupWait = oldWait }()

	started := make(chan string, exlBranchLookupWorkers)
	canceled := make(chan string, exlBranchLookupWorkers)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/raw/") {
			http.NotFound(w, r)
			return
		}
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		revision := parts[len(parts)-2]
		started <- revision
		<-r.Context().Done()
		canceled <- revision
	}))
	defer hub.Close()

	refs := make([]RepoRef, 0, exlBranchLookupWorkers+2)
	for i := 0; i < exlBranchLookupWorkers+2; i++ {
		refs = append(refs, RepoRef{Name: fmt.Sprintf("variant-%d.00bpw", i+1), Type: "branch"})
	}
	info := &RepoInfo{
		Repo:      repo,
		Refs:      refs,
		Quantized: &QuantizedInfo{Method: "gptq"},
	}
	result := make(chan []SelectableItem, 1)
	resultErr := make(chan error, 1)
	go func() {
		items, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).exlBranchVariantItems(context.Background(), info)
		result <- items
		resultErr <- err
	}()

	seen := make(map[string]bool, exlBranchLookupWorkers)
	for i := 0; i < exlBranchLookupWorkers; i++ {
		select {
		case revision := <-started:
			seen[revision] = true
		case <-time.After(2 * time.Second):
			t.Fatal("all lookup workers did not enter blocked branch-config requests")
		}
	}
	for i := 0; i < exlBranchLookupWorkers; i++ {
		select {
		case <-canceled:
		case <-time.After(exlBranchLookupTimeout + time.Second):
			t.Fatal("shared branch metadata timeout did not cancel the occupied worker requests")
		}
	}

	var items []SelectableItem
	select {
	case items = <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("branch variant discovery did not return after optional metadata timeout")
	}
	if err := <-resultErr; err != nil {
		t.Fatalf("optional metadata timeout failed branch discovery: %v", err)
	}
	if len(items) != len(refs) {
		t.Fatalf("lookup timeout truncated the branch list: %d of %d shaped refs kept: %+v", len(items), len(refs), items)
	}
	for i, ref := range refs {
		if items[i].Revision != ref.Name || !items[i].BranchVariant {
			t.Fatalf("branch row %d = %+v, want the unvisited shaped ref %q still offered", i, items[i], ref.Name)
		}
	}
	for i := exlBranchLookupWorkers; i < len(refs); i++ {
		if seen[refs[i].Name] {
			t.Fatalf("queued candidate %q unexpectedly started a lookup before the shared deadline", refs[i].Name)
		}
	}
}

func TestAnalyzeEXLNMBranchUsesBranchConfigWithoutMainOrRepoHint(t *testing.T) {
	const repo = "owner/generic-model"
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			if strings.HasSuffix(r.URL.Path, "/tree/main") {
				io.WriteString(w, `[{"type":"file","path":"config.json","size":20}]`)
				return
			}
			io.WriteString(w, `[{"type":"file","path":"model.safetensors","size":100}]`)
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"exl/4_25"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/raw/exl/4_25/quantize_config.json"):
			io.WriteString(w, `{"quant_method":"exl2","bits_per_weight":4.25,"head_bits":6}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).AnalyzeModelWithRevision(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("analyze generic main: %v", err)
	}
	if len(info.AnalysisWarnings) != 0 {
		t.Fatalf("main without safetensors produced a header-discovery warning: %+v", info.AnalysisWarnings)
	}
	if len(info.SelectableItems) != 1 {
		t.Fatalf("branch options = %+v, want N_M branch recognized from branch config", info.SelectableItems)
	}
	item := info.SelectableItems[0]
	if item.Revision != "exl/4_25" || item.Method != "exl2" || item.BitsPerWeight != 4.25 || item.HeadBits != 6 || !item.Recommended {
		t.Fatalf("branch option = %+v, want EXL2 4.25 BPW from branch config", item)
	}
}

type exlNMBranchFixture struct {
	repo          string
	branches      []string
	mainFiles     []string
	mainFileBody  map[string]string
	branchConfigs map[string]string
}

// analyzeEXLNMBranchFixture exercises the production analyzer path with a fake
// Hub. Main carries no model weights; only the explicitly configured main files
// and branch quantization configs exist. Optional branch size lookups are empty
// so they never decide eligibility.
func analyzeEXLNMBranchFixture(t *testing.T, fixture exlNMBranchFixture) *RepoInfo {
	t.Helper()
	type branchRef struct {
		Name string `json:"name"`
	}
	refs := make([]branchRef, 0, len(fixture.branches))
	for _, name := range fixture.branches {
		refs = append(refs, branchRef{Name: name})
	}
	mainFiles := fixture.mainFiles
	if len(mainFiles) == 0 {
		mainFiles = []string{"README.md"}
	}
	tree := make([]map[string]interface{}, 0, len(mainFiles))
	for _, path := range mainFiles {
		tree = append(tree, map[string]interface{}{"type": "file", "path": path, "size": 64})
	}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/refs"):
			if err := json.NewEncoder(w).Encode(map[string]interface{}{"branches": refs, "tags": []interface{}{}}); err != nil {
				t.Errorf("encode refs: %v", err)
			}
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/tree/main"):
			if err := json.NewEncoder(w).Encode(tree); err != nil {
				t.Errorf("encode main tree: %v", err)
			}
		case strings.Contains(r.URL.Path, "/tree/"):
			io.WriteString(w, `[]`)
		case strings.Contains(r.URL.Path, "/raw/"):
			for revision, content := range fixture.branchConfigs {
				if strings.HasSuffix(r.URL.Path, "/raw/"+revision+"/quantization_config.json") {
					io.WriteString(w, content)
					return
				}
			}
			for path, content := range fixture.mainFileBody {
				if strings.HasSuffix(r.URL.Path, "/raw/main/"+path) {
					io.WriteString(w, content)
					return
				}
			}
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected request %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).AnalyzeModelWithRevision(context.Background(), fixture.repo, "main")
	if err != nil {
		t.Fatalf("analyze %q: %v", fixture.repo, err)
	}
	return info
}

func exlBranchSelectableItems(info *RepoInfo) []SelectableItem {
	items := make([]SelectableItem, 0, len(info.SelectableItems))
	for _, item := range info.SelectableItems {
		if item.Revision != "" {
			items = append(items, item)
		}
	}
	return items
}

func TestBranchVariantRevisionShape(t *testing.T) {
	for _, tc := range []struct {
		revision string
		valid    bool
	}{
		{revision: "4.00bpw", valid: true},
		{revision: "4_25", valid: true},
		{revision: "SC_6.00bpw_H6_V6", valid: true},
		{revision: "exl/4_25", valid: true},
		{revision: "4.00bpw-h6", valid: true},
		{revision: "main", valid: false},
		{revision: "master", valid: false},
		{revision: "v1.0", valid: false},
		{revision: "feature", valid: false},
		{revision: "", valid: false},
		{revision: "999_25", valid: false},
		{revision: "4_25x", valid: false},
		{revision: "4_100", valid: false},
		{revision: "4.00bpw/..", valid: false},
		{revision: "../4_25", valid: false},
		{revision: "exl/../4_25", valid: false},
		{revision: "4.00bpw/.", valid: false},
		// The digits must touch "bpw", and an N_M shape must be the whole
		// final component: a name that separates them is not a branch shape.
		{revision: "4_625-bpw-h6", valid: false},
		{revision: "6_0-bpw-h6", valid: false},
		{revision: "4_25-bpw-h6", valid: false},
	} {
		t.Run(tc.revision, func(t *testing.T) {
			if got := BranchVariantRevisionShape(tc.revision); got != tc.valid {
				t.Fatalf("BranchVariantRevisionShape(%q)=%v; want %v", tc.revision, got, tc.valid)
			}
		})
	}
}

func TestEXLNMBranchRejectsNonBitrateNames(t *testing.T) {
	info := analyzeEXLNMBranchFixture(t, exlNMBranchFixture{
		repo:         "owner/example-exl3-model",
		branches:     []string{"main", "4_25", "fix_4_2", "2024_01", "SC_6_00"},
		mainFiles:    []string{"README.md", "config.json"},
		mainFileBody: map[string]string{"config.json": `{"model_type":"llama"}`},
	})
	items := exlBranchSelectableItems(info)
	if len(items) != 1 {
		t.Fatalf("branch items = %+v, want only the bitrate-shaped 4_25 ref", items)
	}
	item := items[0]
	if item.Revision != "4_25" || !item.BranchVariant || item.Method != "exl3" || item.BitsPerWeight != 4.25 || !item.Recommended {
		t.Fatalf("branch item = %+v, want recommended EXL3 4.25 BPW from the repository name", item)
	}
	if info.RecommendedDownload == nil || info.RecommendedDownload.Revision != "4_25" || !info.RecommendedDownload.BranchVariant {
		t.Fatalf("recommended download = %+v, want the 4_25 branch", info.RecommendedDownload)
	}
}

// TestEXLNMBranchListedWithoutMethodEvidence pins that an unknown method is
// a normal state, not a blocker: with no branch config, no main config, and
// no EXL repository or branch-name hint, the bitrate-shaped ref is still
// offered with no method claim, and its recommended request keeps the
// downloadable branch-variant shape.
func TestEXLNMBranchListedWithoutMethodEvidence(t *testing.T) {
	info := analyzeEXLNMBranchFixture(t, exlNMBranchFixture{
		repo:     "owner/generic-model",
		branches: []string{"main", "4_25"},
	})
	items := exlBranchSelectableItems(info)
	if len(items) != 1 {
		t.Fatalf("branch items = %+v, want the shaped ref offered without method evidence", items)
	}
	item := items[0]
	if item.Revision != "4_25" || !item.BranchVariant || item.Method != "" || item.BitsPerWeight != 4.25 || !item.Recommended {
		t.Fatalf("branch item = %+v, want an EXL-shape row with no method claim", item)
	}
	if info.RecommendedDownload == nil || !info.RecommendedDownload.BranchVariant || !BranchVariantRevisionShape(info.RecommendedDownload.Revision) {
		t.Fatalf("recommended download = %+v, want a downloadable branch-variant request", info.RecommendedDownload)
	}
}

// TestEXLNMBranchUnknownMethodFallsBackToNameEvidence pins the fallback: an
// unknown method string is not known-negative evidence, so the
// repository-name fallback still classifies the branch.
func TestEXLNMBranchUnknownMethodFallsBackToNameEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture exlNMBranchFixture
	}{
		{
			name: "unknown branch method",
			fixture: exlNMBranchFixture{
				repo:          "owner/example-exl3-model",
				branches:      []string{"main", "4_25"},
				branchConfigs: map[string]string{"4_25": `{"quant_method":"mystery_quant","bits":4}`},
			},
		},
		{
			name: "unknown main method",
			fixture: exlNMBranchFixture{
				repo:     "owner/example-exl3-model",
				branches: []string{"main", "4_25"},
				mainFiles: []string{
					"config.json",
					"quantization_config.json",
				},
				mainFileBody: map[string]string{
					"config.json":              `{"architectures":["LlamaForCausalLM"],"model_type":"llama"}`,
					"quantization_config.json": `{"quant_method":"mystery_quant","bits":4}`,
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := analyzeEXLNMBranchFixture(t, tc.fixture)
			items := exlBranchSelectableItems(info)
			if len(items) != 1 || items[0].Revision != "4_25" || items[0].Method != "exl3" {
				t.Fatalf("unknown method must preserve the repository-name fallback: %+v", items)
			}
		})
	}
}

// TestEXLNMBranchConfigReadsNeverGateVisibility pins that a recognized
// non-EXL quant_method read from a branch or main config never removes the
// shaped ref from the list; it only keeps the display from claiming an EXL
// flavor the config denies (the repository-name guess stays suppressed).
func TestEXLNMBranchConfigReadsNeverGateVisibility(t *testing.T) {
	methods := []string{"gptq", "awq", "bitsandbytes", "bnb", "hqq", "eetq", "GPTQ", "BnB", "HQQ"}
	for _, method := range methods {
		t.Run("branch-config-"+method, func(t *testing.T) {
			info := analyzeEXLNMBranchFixture(t, exlNMBranchFixture{
				repo:          "owner/example-exl3-model",
				branches:      []string{"main", "4_25"},
				branchConfigs: map[string]string{"4_25": fmt.Sprintf(`{"quant_method":%q,"bits":4}`, method)},
			})
			items := exlBranchSelectableItems(info)
			if len(items) != 1 || items[0].Revision != "4_25" || !items[0].BranchVariant || items[0].Method != "" {
				t.Fatalf("recognized non-EXL branch config %q = %+v, want the row listed without an EXL method claim", method, items)
			}
		})
		t.Run("main-config-"+method, func(t *testing.T) {
			info := analyzeEXLNMBranchFixture(t, exlNMBranchFixture{
				repo:     "owner/example-exl3-model",
				branches: []string{"main", "4_25"},
				mainFiles: []string{
					"config.json",
					"quantization_config.json",
				},
				mainFileBody: map[string]string{
					"config.json":              `{"architectures":["LlamaForCausalLM"],"model_type":"llama"}`,
					"quantization_config.json": fmt.Sprintf(`{"quant_method":%q,"bits":4}`, method),
				},
			})
			items := exlBranchSelectableItems(info)
			if len(items) != 1 || items[0].Revision != "4_25" || !items[0].BranchVariant || items[0].Method != "" {
				t.Fatalf("recognized non-EXL main config %q = %+v, want the row listed without an EXL method claim", method, items)
			}
		})
	}
}

func TestRecognizedNonEXLMethodUsesQuantizedMethodTable(t *testing.T) {
	for method := range quantMethodDescriptions {
		want := method != "exl2" && method != "exl3"
		if got := recognizedNonEXLMethod(method); got != want {
			t.Fatalf("recognizedNonEXLMethod(%q)=%v; want %v", method, got, want)
		}
	}
	for _, unknown := range []string{"", "mystery_quant", "exl4"} {
		if recognizedNonEXLMethod(unknown) {
			t.Fatalf("unknown method %q was treated as recognized non-EXL evidence", unknown)
		}
	}
}

func TestEXLBranchVariantsIgnoreDatasets(t *testing.T) {
	const repo = "owner/model-exl3"
	var branchRequests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			revision := strings.SplitN(r.URL.Path, "/tree/", 2)[1]
			if revision == "main" {
				io.WriteString(w, `[{"type":"file","path":"data.csv","size":100}]`)
				return
			}
			branchRequests.Add(1)
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/raw/"):
			branchRequests.Add(1)
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).AnalyzeExplicitWithRevision(context.Background(), repo, true, "main")
	if err != nil {
		t.Fatalf("analyze dataset: %v", err)
	}
	if !info.IsDataset || info.Type != TypeDataset {
		t.Fatalf("dataset classification changed: is_dataset=%v type=%q", info.IsDataset, info.Type)
	}
	for _, item := range info.SelectableItems {
		if item.Revision != "" {
			t.Fatalf("dataset gained EXL branch choice: %+v", item)
		}
	}
	if branchRequests.Load() != 0 {
		t.Fatalf("dataset triggered %d optional branch requests, want 0", branchRequests.Load())
	}
}

func TestEXLBranchVariantSizingLimitAndFailureAreNonfatal(t *testing.T) {
	const repo = "owner/exl2-model"
	branches := make([]map[string]string, 0, maxEXLBranchSizeLookups+3)
	branches = append(branches, map[string]string{"name": "non-exl-5.00bpw"})
	for i := 0; i < maxEXLBranchSizeLookups+2; i++ {
		branches = append(branches, map[string]string{"name": fmt.Sprintf("variant-%02d-%d.00bpw", i, i+1)})
	}
	refsJSON, _ := json.Marshal(map[string]interface{}{"branches": branches, "tags": []map[string]string{{"name": "9.00bpw"}}})
	var treeRequests, active, peak, nonEXLConfigRequests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			revision := strings.SplitN(r.URL.Path, "/tree/", 2)[1]
			if revision == "main" {
				io.WriteString(w, `[{"type":"file","path":"README.md","size":1}]`)
				return
			}
			treeRequests.Add(1)
			now := active.Add(1)
			for {
				old := peak.Load()
				if now <= old || peak.CompareAndSwap(old, now) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			active.Add(-1)
			if revision == "variant-00-1.00bpw" {
				http.NotFound(w, r) // A failed optional size lookup must not hide its row.
				return
			}
			io.WriteString(w, `[{"type":"file","path":"model.safetensors","size":100}]`)
		case strings.HasSuffix(r.URL.Path, "/refs"):
			w.Write(refsJSON)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/raw/"):
			if strings.HasSuffix(r.URL.Path, "/non-exl-5.00bpw/quantization_config.json") {
				nonEXLConfigRequests.Add(1)
				io.WriteString(w, `{"quant_method":"gptq","bits":4}`)
				return
			}
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).AnalyzeModelWithRevision(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("optional branch lookups failed main analysis: %v", err)
	}
	// Every shaped ref is listed — including the one whose own config
	// declares a recognized non-EXL method, which only suppresses the display
	// method claim instead of hiding the row.
	if got, want := len(info.SelectableItems), maxEXLBranchSizeLookups+3; got != want {
		t.Fatalf("branch item count = %d, want all %d recognized refs", got, want)
	}
	if got := treeRequests.Load(); got != maxEXLBranchSizeLookups {
		t.Fatalf("branch tree sizing requests = %d, want exactly %d", got, maxEXLBranchSizeLookups)
	}
	if peak.Load() > exlBranchLookupWorkers {
		t.Fatalf("concurrent branch tree requests peaked at %d, limit %d", peak.Load(), exlBranchLookupWorkers)
	}
	if nonEXLConfigRequests.Load() != 1 {
		t.Fatalf("valid non-EXL branch config requests = %d, want exactly one lookup", nonEXLConfigRequests.Load())
	}
	items := make(map[string]SelectableItem, len(info.SelectableItems))
	for _, item := range info.SelectableItems {
		items[item.Revision] = item
	}
	nonEXL, ok := items["non-exl-5.00bpw"]
	if !ok {
		t.Fatal("shaped branch hidden by its non-EXL config read")
	}
	if nonEXL.Method != "" {
		t.Fatalf("non-EXL branch config method claim = %q, want no EXL flavor over known non-EXL evidence", nonEXL.Method)
	}
	if _, ok := items["variant-00-1.00bpw"]; !ok {
		t.Fatal("failed optional sizing lookup removed a valid branch item")
	}
	if items["variant-00-1.00bpw"].Method != "exl2" {
		t.Fatalf("EXL2 repo method fallback = %q, want exl2", items["variant-00-1.00bpw"].Method)
	}
	if items["variant-00-1.00bpw"].SizeHuman != "" {
		t.Fatalf("failed optional sizing lookup synthesized size %q", items["variant-00-1.00bpw"].SizeHuman)
	}
	// The size budget covers the first maxEXLBranchSizeLookups rows in sort
	// order: non-exl-5.00bpw plus variant-00 through variant-14.
	if items["variant-14-15.00bpw"].Size != 100 {
		t.Fatalf("last in-budget branch size = %+v, want 100", items["variant-14-15.00bpw"])
	}
	for _, revision := range []string{"variant-15-16.00bpw", "variant-16-17.00bpw", "variant-17-18.00bpw"} {
		item, ok := items[revision]
		if !ok {
			t.Fatalf("size cap removed unsized branch %q", revision)
		}
		if item.SizeHuman != "" {
			t.Fatalf("unsized branch %q unexpectedly has size %q", revision, item.SizeHuman)
		}
	}
}

func TestEXLBranchVariantCancellationPropagates(t *testing.T) {
	const repo = "owner/exl3-model"
	branchStarted := make(chan struct{})
	var startOnce atomic.Bool
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			if strings.HasSuffix(r.URL.Path, "/tree/main") {
				io.WriteString(w, `[{"type":"file","path":"README.md","size":1}]`)
				return
			}
			if startOnce.CompareAndSwap(false, true) {
				close(branchStarted)
			}
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"4.00bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/raw/"):
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()})
	result := make(chan error, 1)
	go func() {
		_, err := analyzer.AnalyzeModelWithRevision(ctx, repo, "main")
		result <- err
	}()
	select {
	case <-branchStarted:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("branch size request did not start")
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "context canceled") {
			t.Fatalf("analyze error = %v, want context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("analysis did not stop promptly after cancellation")
	}
}
