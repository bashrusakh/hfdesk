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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseEXLBranchCandidate(t *testing.T) {
	tests := []struct {
		name      string
		ref       RepoRef
		allowNM   bool
		wantOK    bool
		wantBPW   float64
		wantHeads float64
	}{
		{name: "simple bpw", ref: RepoRef{Name: "4.00bpw", Type: "branch"}, wantOK: true, wantBPW: 4},
		{name: "SC bpw and head", ref: RepoRef{Name: "SC_6.00bpw_H6_V6", Type: "branch"}, wantOK: true, wantBPW: 6, wantHeads: 6},
		{name: "nested bpw", ref: RepoRef{Name: "exl/4.5bpw", Type: "branch"}, wantOK: true, wantBPW: 4.5},
		{name: "N_M branch for EXL repo", ref: RepoRef{Name: "4_25", Type: "branch"}, allowNM: true, wantOK: true, wantBPW: 4.25},
		{name: "N_M rejects generic repo", ref: RepoRef{Name: "6_5", Type: "branch"}, wantOK: false},
		{name: "tag is not a variant", ref: RepoRef{Name: "4.00bpw", Type: "tag"}, wantOK: false},
		{name: "fp16 is not a bitrate", ref: RepoRef{Name: "fp16", Type: "branch"}, wantOK: false},
		{name: "unsupported revision control", ref: RepoRef{Name: "bad\x7f/4.00bpw", Type: "branch"}, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseEXLBranchCandidate(tt.ref, tt.allowNM)
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
	if mainInfo.RecommendedDownload == nil || mainInfo.RecommendedDownload.Revision != balanced || len(mainInfo.RecommendedDownload.Filters) != 0 || mainInfo.RecommendedDownload.ExactMatch {
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
		branchConfig = `{"vision_config":{"model_type":"siglip","image_size":224},"text_config":{"model_type":"llama","max_position_embeddings":4096}}`
		branchQuant  = `{"quant_method":"exl3","bits_per_weight":4,"head_bits":6}`
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
				io.WriteString(w, `[{"type":"file","path":"config.json","size":100},{"type":"file","path":"model.safetensors","size":400},{"type":"file","path":"quantization_config.json","size":10}]`)
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
			case revision == branch && file == "quantization_config.json":
				io.WriteString(w, branchQuant)
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
	if item.Revision != branch || item.Method != "exl3" || item.BitsPerWeight != 4 || !item.Recommended {
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
}

func TestEXLBranchVariantsPreserveMainWeights(t *testing.T) {
	const repo = "owner/exl3-model"
	var branchRequests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			if strings.HasSuffix(r.URL.Path, "/tree/main") {
				io.WriteString(w, `[{"type":"file","path":"config.json","size":10},{"type":"file","path":"model.safetensors","size":500},{"type":"file","path":"quantization_config.json","size":20}]`)
				return
			}
			branchRequests.Add(1)
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/raw/"):
			if strings.HasSuffix(r.URL.Path, "/main/config.json") {
				io.WriteString(w, `{"model_type":"llama"}`)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/main/quantization_config.json") {
				io.WriteString(w, `{"quant_method":"exl3","bits":4,"head_bits":6}`)
				return
			}
			branchRequests.Add(1)
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
	for _, item := range info.SelectableItems {
		if item.Revision != "" {
			t.Fatalf("main-with-weights repo gained branch item: %+v", item)
		}
	}
	if branchRequests.Load() != 0 {
		t.Fatalf("main-with-weights repo made %d optional branch requests, want 0", branchRequests.Load())
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
	if got, want := len(info.SelectableItems), maxEXLBranchSizeLookups+2; got != want {
		t.Fatalf("branch item count = %d, want all %d recognized refs", got, want)
	}
	for _, item := range info.SelectableItems {
		if item.Revision == "non-exl-5.00bpw" {
			t.Fatal("explicit non-EXL branch config was listed as an EXL variant")
		}
	}
	if nonEXLConfigRequests.Load() != 1 {
		t.Fatalf("valid non-EXL branch config requests = %d, want exactly one lookup", nonEXLConfigRequests.Load())
	}
	if got := treeRequests.Load(); got != maxEXLBranchSizeLookups {
		t.Fatalf("branch tree sizing requests = %d, want exactly %d", got, maxEXLBranchSizeLookups)
	}
	if peak.Load() > exlBranchLookupWorkers {
		t.Fatalf("concurrent branch tree requests peaked at %d, limit %d", peak.Load(), exlBranchLookupWorkers)
	}
	items := make(map[string]SelectableItem, len(info.SelectableItems))
	for _, item := range info.SelectableItems {
		items[item.Revision] = item
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
	if items["variant-15-16.00bpw"].Size != 100 {
		t.Fatalf("invalid branch consumed a sizing slot; last in-budget valid branch = %+v", items["variant-15-16.00bpw"])
	}
	for i := maxEXLBranchSizeLookups; i < maxEXLBranchSizeLookups+2; i++ {
		revision := fmt.Sprintf("variant-%02d-%d.00bpw", i, i+1)
		if _, ok := items[revision]; !ok {
			t.Fatalf("size cap removed unsized branch %q", revision)
		}
		if items[revision].SizeHuman != "" {
			t.Fatalf("unsized branch %q unexpectedly has size %q", revision, items[revision].SizeHuman)
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
