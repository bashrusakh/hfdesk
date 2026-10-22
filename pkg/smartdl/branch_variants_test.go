// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
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

func TestEXLBranchVariantsPreserveMainWeights(t *testing.T) {
	const repo = "owner/exl3-model"
	modelHeader := makeSafetensorsHeader(t, `{"layers.0.mlp.q_weight":{"dtype":"U8","shape":[16],"data_offsets":[0,16]}}`, 500)
	var branchRequests, headerRequests atomic.Int64
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
			if strings.HasSuffix(r.URL.Path, "/main/model.safetensors") {
				headerRequests.Add(1)
				serveSafetensorsHeaderRange(w, r, modelHeader, 500)
				return
			}
			if strings.Contains(r.URL.Path, "/raw/4.00bpw/") {
				branchRequests.Add(1)
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
	if headerRequests.Load() != 2 {
		t.Fatalf("main model header range requests = %d, want prefix and bounded header", headerRequests.Load())
	}
}

func TestEXLBranchVariantsIgnoreRenamedCalibrationSafetensors(t *testing.T) {
	const (
		repo     = "turboderp/Qwen3.8-27B-exl3"
		revision = "4.00bpw"
	)
	calibrationHeader := makeSafetensorsHeader(t, `{"input_ids":{"dtype":"I64","shape":[250,2048],"data_offsets":[0,4096000]}}`, 4096088)
	var headerRequests, branchTreeRequests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			branch := strings.SplitN(r.URL.Path, "/tree/", 2)[1]
			switch branch {
			case "main":
				io.WriteString(w, `[{"type":"file","path":".gitattributes","size":1519},{"type":"file","path":"README.md","size":3481},{"type":"file","path":"cal_trace.json","size":3410836},{"type":"file","path":"cal_trace.md","size":2274406},{"type":"file","path":"calibration-batch-001.safetensors","size":4096088,"lfs":{"size":4096088,"oid":"a034bee4c7920b4ac2e62b9d881a9b118488c414a81cf89aa64bd9274c47d685"}},{"type":"file","path":"kld_table.json","size":190470},{"type":"file","path":"measurement.json","size":60916},{"type":"file","path":"noise_attrib.json","size":257094},{"type":"file","path":"qbench_prompts_gen.json","size":344109},{"type":"file","path":"qbench_prompts_gen.md","size":228736},{"type":"file","path":"recipe_1.40bpw.yaml","size":21487},{"type":"file","path":"recipe_1.60bpw.yaml","size":21487},{"type":"file","path":"recipe_1.80bpw.yaml","size":21487},{"type":"file","path":"recipe_2.00bpw.yaml","size":21490},{"type":"file","path":"recipe_2.20bpw.yaml","size":21484},{"type":"file","path":"recipe_3.00bpw.yaml","size":21487},{"type":"file","path":"recipe_4.00bpw.yaml","size":21487},{"type":"file","path":"recipe_5.00bpw.yaml","size":21486},{"type":"file","path":"recipe_6.00bpw.yaml","size":21486},{"type":"file","path":"rfn_2.00bpw.json","size":85427}]`)
			case revision:
				branchTreeRequests.Add(1)
				io.WriteString(w, `[{"type":"file","path":"config.json","size":4621},{"type":"file","path":"model-00001-of-00002.safetensors","size":8471336006},{"type":"file","path":"model-00002-of-00002.safetensors","size":8389473789},{"type":"file","path":"model.safetensors.index.json","size":238664},{"type":"file","path":"quantization_config.json","size":629045}]`)
			default:
				t.Errorf("unexpected tree revision %q", branch)
				http.NotFound(w, r)
			}
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/resolve/main/calibration-batch-001.safetensors"):
			headerRequests.Add(1)
			serveSafetensorsHeaderRange(w, r, calibrationHeader, 4096088)
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
		t.Fatalf("calibration-only main hid EXL branch: %+v", info.SelectableItems)
	}
	if info.RecommendedDownload == nil || info.RecommendedDownload.Revision != revision || len(info.RecommendedDownload.Filters) != 0 {
		t.Fatalf("recommended EXL selection = %+v, want unfiltered revision %q", info.RecommendedDownload, revision)
	}
	if headerRequests.Load() != 2 {
		t.Fatalf("header range requests = %d, want prefix and bounded header only", headerRequests.Load())
	}
	if branchTreeRequests.Load() == 0 {
		t.Fatal("calibration-only main did not inspect the real branch for size enrichment")
	}
}

func TestIsCalibrationOnlySafetensorsHeader(t *testing.T) {
	for _, tc := range []struct {
		name     string
		header   string
		wantRole bool
		wantErr  bool
	}{
		{
			name:     "legacy input_ids only",
			header:   `{"input_ids":{"dtype":"I64","shape":[250,2048],"data_offsets":[0,4096000]}}`,
			wantRole: true,
		},
		{
			name:     "current trace input_ids and row lengths",
			header:   `{"input_ids":{"dtype":"I64","shape":[250,2048],"data_offsets":[0,4096000]},"lengths":{"dtype":"I64","shape":[250],"data_offsets":[4096000,4098000]}}`,
			wantRole: true,
		},
		{
			name:     "length rows mismatch",
			header:   `{"input_ids":{"dtype":"I64","shape":[250,2048],"data_offsets":[0,4096000]},"lengths":{"dtype":"I64","shape":[249],"data_offsets":[4096000,4097992]}}`,
			wantRole: false,
		},
		{
			name:     "arbitrary integer tensor is not calibration",
			header:   `{"opaque_tensor":{"dtype":"I64","shape":[250,2048],"data_offsets":[0,4096000]}}`,
			wantRole: false,
		},
		{
			name:     "mixed input and model parameter is a weight file",
			header:   `{"input_ids":{"dtype":"I64","shape":[250,2048],"data_offsets":[0,4096000]},"q_weight":{"dtype":"U8","shape":[16],"data_offsets":[4096000,4096016]}}`,
			wantRole: false,
		},
		{
			name:    "malformed header is inconclusive",
			header:  `{"input_ids":`,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := isCalibrationOnlySafetensorsHeader([]byte(tc.header))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr=%v", err, tc.wantErr)
			}
			if err == nil && got != tc.wantRole {
				t.Fatalf("calibration role = %v, want %v", got, tc.wantRole)
			}
		})
	}
}

func TestEXLBranchVariantsPreserveArbitraryNamedModelSafetensors(t *testing.T) {
	const repo = "owner/exl3-model"
	modelHeader := makeSafetensorsHeader(t, `{"layers.0.attn.q_weight":{"dtype":"U8","shape":[16],"data_offsets":[0,16]},"layers.0.attn.q_scale":{"dtype":"F16","shape":[1],"data_offsets":[16,18]}}`, 500)
	var headerRequests, branchRequests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			if strings.HasSuffix(r.URL.Path, "/tree/main") {
				io.WriteString(w, `[{"type":"file","path":"config.json","size":10},{"type":"file","path":"opaque-export-part.safetensors","size":500},{"type":"file","path":"quantization_config.json","size":20}]`)
				return
			}
			branchRequests.Add(1)
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/raw/main/opaque-export-part.safetensors"):
			headerRequests.Add(1)
			serveSafetensorsHeaderRange(w, r, modelHeader, 500)
		case strings.Contains(r.URL.Path, "/raw/") && strings.HasSuffix(r.URL.Path, "/main/config.json"):
			io.WriteString(w, `{"model_type":"llama"}`)
		case strings.Contains(r.URL.Path, "/raw/") && strings.HasSuffix(r.URL.Path, "/main/quantization_config.json"):
			io.WriteString(w, `{"quant_method":"exl3","bits":4,"head_bits":6}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).AnalyzeModelWithRevision(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("analyze real-weight main: %v", err)
	}
	if info.Type != TypeTransformers || info.Quantized == nil || info.Quantized.Method != "exl3" {
		t.Fatalf("main analysis changed: type=%s quantized=%+v", info.Type, info.Quantized)
	}
	for _, item := range info.SelectableItems {
		if item.Revision != "" {
			t.Fatalf("arbitrarily named real model weights gained a branch option: %+v", item)
		}
	}
	if headerRequests.Load() != 2 || branchRequests.Load() != 0 {
		t.Fatalf("header/branch requests = %d/%d, want 2/0", headerRequests.Load(), branchRequests.Load())
	}
}

func TestEXLBranchEligibilityConservativelyHandlesUnsupportedSafetensorsRange(t *testing.T) {
	const repo = "owner/exl3-model"
	var rangeRequests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			if strings.HasSuffix(r.URL.Path, "/tree/main") {
				io.WriteString(w, `[{"type":"file","path":"opaque.safetensors","size":1000}]`)
				return
			}
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/raw/main/opaque.safetensors"):
			rangeRequests.Add(1)
			io.WriteString(w, "range unsupported") // 200 must not be read as a full model file.
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).AnalyzeModelWithRevision(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("unsupported optional header probe failed main analysis: %v", err)
	}
	for _, item := range info.SelectableItems {
		if item.Revision != "" {
			t.Fatalf("unknown file role exposed branch option without positive calibration evidence: %+v", item)
		}
	}
	if rangeRequests.Load() != 1 {
		t.Fatalf("unsupported range requests = %d, want one bounded attempt", rangeRequests.Load())
	}
}

func TestEXLBranchEligibilityReadsBoundedHeaderFromRangeIgnoring200(t *testing.T) {
	const repo = "owner/exl3-model"
	headerFile := makeSafetensorsHeader(t, `{"input_ids":{"dtype":"I64","shape":[1,1],"data_offsets":[0,8]},"lengths":{"dtype":"I64","shape":[1],"data_offsets":[8,16]}}`, 512)
	// The tail is synthetic fixture storage, not model or calibration data.
	fileBody := append(append([]byte(nil), headerFile...), make([]byte, 16)...)
	var bodyBytesRead, fullResponses, responseLength, contentHeaderLength atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			if strings.HasSuffix(r.URL.Path, "/tree/main") {
				fmt.Fprintf(w, `[{"type":"file","path":"renamed-trace.safetensors","size":%d}]`, len(fileBody))
				return
			}
			io.WriteString(w, `[{"type":"file","path":"model.safetensors","size":500}]`)
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/raw/main/renamed-trace.safetensors"):
			fullResponses.Add(1)
			w.Header().Set("Content-Length", strconv.Itoa(len(fileBody)))
			w.WriteHeader(http.StatusOK) // This endpoint ignores Range and serves the complete file.
			_, _ = w.Write(fileBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	client := hub.Client()
	client.Transport = &safetensorsBodyCountingTransport{
		base:          client.Transport,
		path:          "/raw/main/renamed-trace.safetensors",
		count:         &bodyBytesRead,
		contentLength: &responseLength,
		headerLength:  &contentHeaderLength,
	}
	info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: client}).AnalyzeModelWithRevision(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("analyze Range-ignoring main: %v", err)
	}
	if len(info.SelectableItems) != 1 || info.SelectableItems[0].Revision != "4.00bpw" {
		t.Fatalf("complete 200 calibration response hid EXL branch: %+v", info.SelectableItems)
	}
	wantRead := int64(8 + len(headerFile)) // first prefix, then the skipped prefix plus JSON header.
	if bodyBytesRead.Load() != wantRead || fullResponses.Load() != 2 {
		t.Fatalf("HTTP 200 reads/requests/content-length/header = %d/%d/%d/%d (tree=%d body=%d), want header-only %d bytes across two requests", bodyBytesRead.Load(), fullResponses.Load(), responseLength.Load(), contentHeaderLength.Load(), len(fileBody), len(fileBody), wantRead)
	}
}

func TestEXLBranchEligibilityCancelsRangeIgnoring200BeforeBodyDrain(t *testing.T) {
	for _, protocol := range []struct {
		name string
		h2   bool
	}{{name: "http1"}, {name: "http2", h2: true}} {
		t.Run(protocol.name, func(t *testing.T) {
			const repo = "owner/exl3-model"
			headerFile := makeSafetensorsHeader(t, `{"input_ids":{"dtype":"I64","shape":[1,1],"data_offsets":[0,8]},"lengths":{"dtype":"I64","shape":[1],"data_offsets":[8,16]}}`, 512)
			tail := []byte("synthetic")
			fileSize := int64(len(headerFile) + len(tail))
			payloadGate := make(chan struct{})
			canceled := make(chan struct{}, 2)
			protocolMajors := make(chan int, 2)
			var payloadWrites atomic.Int64
			hub := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/tree/"):
					if strings.HasSuffix(r.URL.Path, "/tree/main") {
						fmt.Fprintf(w, `[{"type":"file","path":"calibration-stream.safetensors","size":%d,"lfs":{"size":%d,"oid":"0000000000000000000000000000000000000000000000000000000000000000"}}]`, fileSize, fileSize)
						return
					}
					io.WriteString(w, `[{"type":"file","path":"model.safetensors","size":500}]`)
				case strings.HasSuffix(r.URL.Path, "/refs"):
					io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
				case strings.HasSuffix(r.URL.Path, "/auth-check"):
					http.NotFound(w, r)
				case strings.HasSuffix(r.URL.Path, "/resolve/main/calibration-stream.safetensors"):
					protocolMajors <- r.ProtoMajor
					w.Header().Set("Content-Length", strconv.FormatInt(fileSize, 10))
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(headerFile)
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						canceled <- struct{}{}
					case <-payloadGate:
						payloadWrites.Add(1)
						_, _ = w.Write(tail)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			hub.EnableHTTP2 = protocol.h2
			if protocol.h2 {
				hub.StartTLS()
			} else {
				hub.Start()
			}
			defer hub.Close()

			client := hub.Client()
			if protocol.h2 {
				transport, ok := client.Transport.(*http.Transport)
				if !ok {
					t.Fatalf("HTTP/2 test transport has type %T", client.Transport)
				}
				transport.ForceAttemptHTTP2 = true
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: client}).AnalyzeModelWithRevision(ctx, repo, "main")
				if err == nil && (len(info.SelectableItems) != 1 || info.SelectableItems[0].Revision != "4.00bpw") {
					err = fmt.Errorf("Range-ignoring 200 hid EXL branch: %+v", info.SelectableItems)
				}
				result <- err
			}()
			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("analyze streaming 200 calibration header: %v", err)
				}
			case <-time.After(2 * time.Second):
				close(payloadGate)
				select {
				case <-result:
				case <-time.After(2 * time.Second):
				}
				t.Fatal("analysis drained or blocked on the unread 200 tensor body")
			}
			for i := 0; i < 2; i++ {
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("Range response body was not canceled after bounded header read")
				}
			}
			for i := 0; i < 2; i++ {
				select {
				case got := <-protocolMajors:
					want := 1
					if protocol.h2 {
						want = 2
					}
					if got != want {
						t.Fatalf("Range response protocol = HTTP/%d, want HTTP/%d", got, want)
					}
				case <-time.After(time.Second):
					t.Fatal("expected Range response protocol was not observed")
				}
			}
			if payloadWrites.Load() != 0 {
				t.Fatalf("server wrote %d synthetic payload chunks after header classification", payloadWrites.Load())
			}
		})
	}
}

func TestEXLBranchEligibilityBoundsSafetensorsHeaderRead(t *testing.T) {
	const repo = "owner/exl3-model"
	const headerSize = maxSafeTensorHeaderSize + 1
	totalSize := int64(8 + headerSize + 1)
	prefix := make([]byte, 8)
	binary.LittleEndian.PutUint64(prefix, uint64(headerSize))
	var rangeRequests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			if strings.HasSuffix(r.URL.Path, "/tree/main") {
				fmt.Fprintf(w, `[{"type":"file","path":"oversized-header.safetensors","size":%d}]`, totalSize)
				return
			}
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/raw/main/oversized-header.safetensors"):
			rangeRequests.Add(1)
			if r.Header.Get("Range") != "bytes=0-7" {
				t.Errorf("oversized-header probe Range = %q, want bytes=0-7", r.Header.Get("Range"))
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-7/%d", totalSize))
			w.Header().Set("Content-Length", "8")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(prefix)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).AnalyzeModelWithRevision(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("oversized optional header failed main analysis: %v", err)
	}
	for _, item := range info.SelectableItems {
		if item.Revision != "" {
			t.Fatalf("oversized unknown header exposed branch option: %+v", item)
		}
	}
	if rangeRequests.Load() != 1 {
		t.Fatalf("oversized header range requests = %d, want only the 8-byte prefix", rangeRequests.Load())
	}
}

func TestEXLBranchEligibilityRangeProbePropagatesCancellation(t *testing.T) {
	const repo = "owner/exl3-model"
	started := make(chan struct{})
	var startOnce atomic.Bool
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			if strings.HasSuffix(r.URL.Path, "/tree/main") {
				io.WriteString(w, `[{"type":"file","path":"opaque.safetensors","size":1000}]`)
				return
			}
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/raw/main/opaque.safetensors"):
			if startOnce.CompareAndSwap(false, true) {
				close(started)
			}
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).AnalyzeModelWithRevision(ctx, repo, "main")
		result <- err
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("safetensors header probe did not start")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("analysis error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("analysis did not stop after safetensors header probe cancellation")
	}
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
				io.WriteString(w, `[{"type":"file","path":"README.md","size":10}]`)
			case revision:
				io.WriteString(w, `[{"type":"file","path":"config.json","size":160},{"type":"file","path":"output.safetensors","size":500},{"type":"file","path":"model.safetensors.index.json","size":40}]`)
			default:
				t.Errorf("unexpected tree revision %q", branch)
				http.NotFound(w, r)
			}
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.25bpw"}],"tags":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/raw/4.25bpw/config.json"):
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
	if len(mainInfo.SelectableItems) != 1 {
		t.Fatalf("nested branch options = %+v, want one metadata-identified EXL branch", mainInfo.SelectableItems)
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

func makeSafetensorsHeader(t *testing.T, headerJSON string, totalSize int64) []byte {
	t.Helper()
	header := []byte(headerJSON)
	for len(header)%8 != 0 {
		header = append(header, ' ')
	}
	if int64(8+len(header)) > totalSize {
		t.Fatalf("fixture header length %d exceeds declared file size %d", len(header), totalSize)
	}
	fileHeader := make([]byte, 8+len(header))
	binary.LittleEndian.PutUint64(fileHeader[:8], uint64(len(header)))
	copy(fileHeader[8:], header)
	return fileHeader
}

func serveSafetensorsHeaderRange(w http.ResponseWriter, r *http.Request, fileHeader []byte, totalSize int64) {
	rawRange := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
	parts := strings.Split(rawRange, "-")
	if len(parts) != 2 {
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	start, startErr := strconv.ParseInt(parts[0], 10, 64)
	end, endErr := strconv.ParseInt(parts[1], 10, 64)
	if startErr != nil || endErr != nil || start < 0 || end < start || end >= int64(len(fileHeader)) {
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, totalSize))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(fileHeader[start : end+1])
}

type safetensorsBodyCountingTransport struct {
	base          http.RoundTripper
	path          string
	count         *atomic.Int64
	contentLength *atomic.Int64
	headerLength  *atomic.Int64
}

func (t *safetensorsBodyCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp != nil && strings.HasSuffix(req.URL.Path, t.path) {
		t.contentLength.Store(resp.ContentLength)
		if length, parseErr := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); parseErr == nil {
			t.headerLength.Store(length)
		}
		resp.Body = &safetensorsBodyCounter{ReadCloser: resp.Body, count: t.count}
	}
	return resp, err
}

type safetensorsBodyCounter struct {
	io.ReadCloser
	count *atomic.Int64
}

func (r *safetensorsBodyCounter) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.count.Add(int64(n))
	return n, err
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
