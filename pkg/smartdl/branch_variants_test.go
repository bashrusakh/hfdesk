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

// TestEXLBranchVariantsPreserveMainWeights pins the preserved behavior of a
// main revision that carries model weights: it gets no EXL branch option, and
// the analyzer makes no optional branch request for it.
//
// The weight file's name is the data that differs: a model-weight file is
// recognized by its content, not by a name that happens to look like one.
func TestEXLBranchVariantsPreserveMainWeights(t *testing.T) {
	for _, tc := range []struct {
		name   string
		file   string
		header string
	}{
		{
			name:   "standard weight file name",
			file:   "model.safetensors",
			header: `{"layers.0.mlp.q_weight":{"dtype":"U8","shape":[16],"data_offsets":[0,16]}}`,
		},
		{
			name:   "arbitrary weight file name",
			file:   "opaque-export-part.safetensors",
			header: `{"layers.0.attn.q_weight":{"dtype":"U8","shape":[16],"data_offsets":[0,16]},"layers.0.attn.q_scale":{"dtype":"F16","shape":[1],"data_offsets":[16,18]}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const repo = "owner/exl3-model"
			modelHeader := makeSafetensorsHeader(t, tc.header, 500)
			var branchRequests, headerRequests atomic.Int64
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/tree/"):
					if strings.HasSuffix(r.URL.Path, "/tree/main") {
						fmt.Fprintf(w, `[{"type":"file","path":"config.json","size":10},{"type":"file","path":%q,"size":500},{"type":"file","path":"quantization_config.json","size":20}]`, tc.file)
						return
					}
					branchRequests.Add(1)
					http.NotFound(w, r)
				case strings.HasSuffix(r.URL.Path, "/refs"):
					io.WriteString(w, `{"branches":[{"name":"main"},{"name":"4.00bpw"}],"tags":[]}`)
				case strings.HasSuffix(r.URL.Path, "/auth-check"):
					http.NotFound(w, r)
				case strings.HasSuffix(r.URL.Path, "/raw/main/"+tc.file):
					headerRequests.Add(1)
					serveSafetensorsHeaderRange(w, r, modelHeader, 500)
				case strings.HasSuffix(r.URL.Path, "/raw/main/config.json"):
					io.WriteString(w, `{"model_type":"llama"}`)
				case strings.HasSuffix(r.URL.Path, "/raw/main/quantization_config.json"):
					io.WriteString(w, `{"quant_method":"exl3","bits":4,"head_bits":6}`)
				case strings.Contains(r.URL.Path, "/raw/4.00bpw/"):
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
			if len(info.AnalysisWarnings) != 0 {
				t.Fatalf("confirmed model-weight header produced an analysis warning: %+v", info.AnalysisWarnings)
			}
			for _, item := range info.SelectableItems {
				if item.Revision != "" {
					t.Fatalf("main-with-weights repo gained branch item: %+v", item)
				}
			}
			if headerRequests.Load() != 2 || branchRequests.Load() != 0 {
				t.Fatalf("header/branch requests = %d/%d, want 2/0", headerRequests.Load(), branchRequests.Load())
			}
		})
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
			case revision, "4_25":
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
	if len(info.AnalysisWarnings) != 0 {
		t.Fatalf("confirmed calibration header produced an analysis warning: %+v", info.AnalysisWarnings)
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
	encoded, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal partial analysis: %v", err)
	}
	var response struct {
		AnalysisWarnings []struct {
			Code      string   `json:"code"`
			Message   string   `json:"message"`
			Causes    []string `json:"causes"`
			Retryable bool     `json:"retryable"`
		} `json:"analysis_warnings"`
	}
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatalf("decode analysis warning: %v", err)
	}
	if len(response.AnalysisWarnings) != 1 {
		t.Fatalf("analysis warnings = %+v, want one incomplete-branch diagnostic; analysis=%s", response.AnalysisWarnings, encoded)
	}
	warning := response.AnalysisWarnings[0]
	if warning.Code != "exl_branch_discovery_incomplete" || warning.Message == "" || !warning.Retryable || !reflect.DeepEqual(warning.Causes, []string{"header_unavailable"}) {
		t.Fatalf("analysis warning = %+v, want safe retryable header-unavailable diagnostic", warning)
	}
	if strings.Contains(string(encoded), "range unsupported") {
		t.Fatalf("analysis warning leaked upstream response body: %s", encoded)
	}
	if rangeRequests.Load() != 1 {
		t.Fatalf("unsupported range requests = %d, want one bounded attempt", rangeRequests.Load())
	}
}

// TestEXLBranchEligibilityDiscoveryOutcomes pins the outcomes of the optional
// safetensors header probe: an unknown main file role never exposes a branch
// row on its own, a known weight suppresses the uncertainty, and every
// incomplete discovery is reported as one retryable diagnostic.
func TestEXLBranchEligibilityDiscoveryOutcomes(t *testing.T) {
	weightHeader := makeSafetensorsHeader(t, `{"layers.0.q_weight":{"dtype":"U8","shape":[8],"data_offsets":[0,8]}}`, 1024)
	calibrationHeader := makeSafetensorsHeader(t, `{"input_ids":{"dtype":"I64","shape":[1,1],"data_offsets":[0,8]}}`, 1024)
	malformedHeader := makeSafetensorsHeader(t, `{"input_ids":`, 1024)
	probeLimitFiles := make([]exlHeaderFixtureFile, 0, maxEXLSafeTensorHeaderProbes+1)
	for i := 0; i < maxEXLSafeTensorHeaderProbes+1; i++ {
		probeLimitFiles = append(probeLimitFiles, exlHeaderFixtureFile{
			path:   fmt.Sprintf("calibration-%02d.safetensors", i),
			mode:   "header",
			header: calibrationHeader,
			size:   1024,
		})
	}
	for _, tc := range []struct {
		name string
		// files are the main tree files the analyzer inspects.
		files []exlHeaderFixtureFile
		// wantCauses are the diagnostic causes of the incomplete discovery. An
		// empty list means the discovery was complete and reported no warning.
		wantCauses []string
		// wantRequests is the number of header HTTP requests the probe may
		// make.
		wantRequests int64
	}{
		{
			name: "unknown role does not override later calibration evidence",
			files: []exlHeaderFixtureFile{
				{path: "unavailable.safetensors", mode: "unavailable", size: 1024},
				{path: "malformed.safetensors", mode: "header", header: malformedHeader, size: 1024},
				{path: "calibration.safetensors", mode: "header", header: calibrationHeader, size: 1024},
			},
			wantCauses:   []string{"header_unavailable", "header_uninterpretable"},
			wantRequests: 5,
		},
		{
			name: "known safetensors weight wins over an earlier unknown",
			files: []exlHeaderFixtureFile{
				{path: "unavailable.safetensors", mode: "unavailable", size: 1024},
				{path: "model.safetensors", mode: "header", header: weightHeader, size: 1024},
			},
			wantRequests: 3,
		},
		{
			name: "another known weight suppresses the unknown header warning",
			files: []exlHeaderFixtureFile{
				{path: "unavailable.safetensors", mode: "unavailable", size: 1024},
				{path: "model.gguf", size: 1024},
			},
			wantRequests: 0,
		},
		{
			name: "malformed header reports incomplete discovery",
			files: []exlHeaderFixtureFile{
				{path: "malformed.safetensors", mode: "header", header: malformedHeader, size: 1024},
			},
			wantCauses:   []string{"header_uninterpretable"},
			wantRequests: 2,
		},
		{
			name:         "probe limit reports incomplete discovery",
			files:        probeLimitFiles,
			wantCauses:   []string{"probe_limit"},
			wantRequests: 2 * int64(maxEXLSafeTensorHeaderProbes),
		},
		{
			name: "probe timeout reports incomplete discovery",
			files: []exlHeaderFixtureFile{
				{path: "slow.safetensors", mode: "timeout", size: 1024},
			},
			wantCauses:   []string{"probe_timeout"},
			wantRequests: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, headerRequests := analyzeEXLHeaderFixture(t, tc.files)
			if got := exlBranchRevisionCount(info); got != 0 {
				t.Fatalf("unknown main file role exposed %d branch rows, want 0", got)
			}
			if len(tc.wantCauses) == 0 {
				if len(info.AnalysisWarnings) != 0 {
					t.Fatalf("analysis warnings = %+v, want none", info.AnalysisWarnings)
				}
			} else {
				if len(info.AnalysisWarnings) != 1 {
					t.Fatalf("analysis warnings = %+v, want one incomplete-branch diagnostic", info.AnalysisWarnings)
				}
				warning := info.AnalysisWarnings[0]
				if warning.Code != exlBranchDiscoveryWarningCode || !warning.Retryable {
					t.Fatalf("analysis warning = %+v, want the retryable incomplete-branch diagnostic", warning)
				}
				if !reflect.DeepEqual(warning.Causes, tc.wantCauses) {
					t.Fatalf("analysis warning causes = %v, want %v", warning.Causes, tc.wantCauses)
				}
			}
			if headerRequests != tc.wantRequests {
				t.Fatalf("header HTTP requests = %d, want %d", headerRequests, tc.wantRequests)
			}
		})
	}
}

type exlHeaderFixtureFile struct {
	path   string
	mode   string
	header []byte
	size   int64
}

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
			file, ok := byPath[path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			headerRequests.Add(1)
			switch file.mode {
			case "unavailable":
				http.Error(w, "UPSTREAM_BODY_MUST_NOT_LEAK", http.StatusServiceUnavailable)
			case "timeout":
				<-r.Context().Done()
			default:
				serveSafetensorsHeaderRange(w, r, file.header, file.size)
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

func exlBranchRevisionCount(info *RepoInfo) int {
	count := 0
	for _, item := range info.SelectableItems {
		if item.Revision != "" {
			count++
		}
	}
	return count
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
	if len(info.AnalysisWarnings) != 0 {
		t.Fatalf("bounded complete 200 calibration header produced a warning: %+v", info.AnalysisWarnings)
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
				if err == nil && len(info.AnalysisWarnings) != 0 {
					err = fmt.Errorf("bounded Range-ignoring 200 produced warnings: %+v", info.AnalysisWarnings)
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
	if len(info.AnalysisWarnings) != 1 || !reflect.DeepEqual(info.AnalysisWarnings[0].Causes, []string{"header_unavailable"}) {
		t.Fatalf("analysis warnings = %+v, want bounded-header diagnostic", info.AnalysisWarnings)
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

func TestEXLBranchMethodPriorityAndFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/raw/branch/quantization_config.json") {
			io.WriteString(w, `{"quant_method":"exl2"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	a := NewAnalyzer(AnalyzerOptions{Endpoint: server.URL, HTTPClient: server.Client()})
	candidate := exlBranchCandidate{revision: "branch", bpw: 4.25, headBits: 6}

	// Branch config outranks both main-config and repo-name evidence.
	got := a.lookupEXLBranch(context.Background(), "owner/exl3-model", candidate, "exl3", "exl3", true)
	if !got.include || got.item.Method != "exl2" {
		t.Fatalf("branch-config method = %+v, want exl2 variant", got)
	}

	// An unavailable additional lookup must retain the method evidence the
	// caller already has; head_bits alone, without config or repo evidence,
	// must not classify EXL.
	got = a.lookupEXLBranch(context.Background(), "owner/exl3-model", exlBranchCandidate{revision: "missing", bpw: 4.25, headBits: 6}, "exl3", "exl2", true)
	if !got.include || got.item.Method != "exl2" {
		t.Fatalf("failed branch-config lookup did not retain main-config priority: %+v", got)
	}
	got = a.lookupEXLBranch(context.Background(), "owner/metadata-model", exlBranchCandidate{revision: "missing", bpw: 4.25, headBits: 6}, "", "", false)
	if got.include {
		t.Fatalf("head_bits/repo without EXL hint classified a variant: %+v", got)
	}
}

func TestAnalyzeEXLBranchDoesNotOverrideKnownNonEXLMainMethod(t *testing.T) {
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
	for _, item := range info.SelectableItems {
		if item.BranchVariant {
			t.Fatalf("repo-name fallback overrode known non-EXL main method: %+v", item)
		}
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

func TestEXLBranchMainMethodSeedsAllCandidatesWhenOptionalLookupTimesOut(t *testing.T) {
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
	items, warning, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).exlBranchVariantItems(context.Background(), info)
	if err != nil {
		t.Fatalf("branch discovery with a slow optional lookup: %v", err)
	}
	if warning != nil {
		t.Fatalf("optional lookup timeout produced a warning: %+v", warning)
	}
	if len(items) != len(refs) {
		t.Fatalf("main EXL2 method seeded %d of %d branch candidates after optional timeout: %+v", len(items), len(refs), items)
	}
	for _, item := range items {
		if item.Method != "exl2" || !item.BranchVariant {
			t.Fatalf("seeded branch item = %+v; want EXL2 branch", item)
		}
	}
}

func TestEXLBranchNonWeightArtifactsDoNotProveMainWeights(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []FileInfo
	}{
		{name: "training args bin", files: []FileInfo{{Path: "training_args.bin"}}},
		{name: "optimizer pt", files: []FileInfo{{Path: "optimizer.pt"}}},
		{name: "optimizer and training", files: []FileInfo{{Path: "training_args.bin"}, {Path: "optimizer.pt"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &RepoInfo{
				Repo:      "owner/model",
				Refs:      []RepoRef{{Name: "4.00bpw", Type: "branch"}},
				Quantized: &QuantizedInfo{Method: "exl2"},
				Files:     tc.files,
			}
			items, warning, err := NewAnalyzer(AnalyzerOptions{}).exlBranchVariantItems(context.Background(), info)
			if err != nil {
				t.Fatalf("branch discovery with non-weight artifacts: %v", err)
			}
			if len(items) != 0 {
				t.Fatalf("non-weight artifacts exposed branch items: %+v", items)
			}
			if warning == nil || warning.Code != exlBranchDiscoveryWarningCode || !warning.Retryable {
				t.Fatalf("non-weight artifacts warning=%+v; want the unknown-role warning path", warning)
			}
			if !reflect.DeepEqual(warning.Causes, []string{weightRoleUnrecognizedCause}) {
				t.Fatalf("non-weight artifact causes=%v; want %q", warning.Causes, weightRoleUnrecognizedCause)
			}
		})
	}

	for _, name := range []string{"pytorch_model.bin", "model.gguf", "tf_model.h5", "saved_model.pb", "consolidated.00.pth"} {
		t.Run("real weight "+name, func(t *testing.T) {
			info := &RepoInfo{
				Repo:      "owner/model",
				Refs:      []RepoRef{{Name: "4.00bpw", Type: "branch"}},
				Quantized: &QuantizedInfo{Method: "exl2"},
				Files:     []FileInfo{{Path: name}},
			}
			items, warning, err := NewAnalyzer(AnalyzerOptions{}).exlBranchVariantItems(context.Background(), info)
			if err != nil {
				t.Fatalf("branch discovery with real weight %q: %v", name, err)
			}
			if warning != nil {
				t.Fatalf("real weight %q produced warning: %+v", name, warning)
			}
			if len(items) != 0 {
				t.Fatalf("real weight %q exposed branch items: %+v", name, items)
			}
		})
	}

	t.Run("tokenizer bin is known non-weight", func(t *testing.T) {
		info := &RepoInfo{
			Repo:      "owner/model",
			Refs:      []RepoRef{{Name: "4.00bpw", Type: "branch"}},
			Quantized: &QuantizedInfo{Method: "exl2"},
			Files:     []FileInfo{{Path: "tokenizer.bin"}},
		}
		items, warning, err := NewAnalyzer(AnalyzerOptions{}).exlBranchVariantItems(context.Background(), info)
		if err != nil || warning != nil || len(items) != 1 {
			t.Fatalf("tokenizer.bin result items=%+v warning=%+v err=%v; want branch discovery without an unknown-role warning", items, warning, err)
		}
	})
}

func TestEXLBranchTimeoutDoesNotEmitUnvisitedRepoFallbackSeeds(t *testing.T) {
	const repo = "owner/model-exl3"
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
		items, _, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).exlBranchVariantItems(context.Background(), info)
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
	if len(items) != 0 {
		t.Fatalf("known GPTQ main retained %d preseeded EXL repo-name variants after unvisited lookups: %+v", len(items), items)
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

func TestEXLNMBranchRejectGenericRepo(t *testing.T) {
	// This exercises the production analyzer path, not a parser zero value:
	// without branch config, main config, or an EXL repository name there is no
	// method evidence, so a bitrate-shaped N_M ref must not be listed.
	info := analyzeEXLNMBranchFixture(t, exlNMBranchFixture{
		repo:     "owner/generic-model",
		branches: []string{"main", "4_25"},
	})
	if items := exlBranchSelectableItems(info); len(items) != 0 {
		t.Fatalf("generic repository exposed N_M branch rows without EXL evidence: %+v", items)
	}
	if info.RecommendedDownload != nil && info.RecommendedDownload.BranchVariant {
		t.Fatalf("generic repository recommended a branch variant: %+v", info.RecommendedDownload)
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

func TestEXLNMBranchEveryRecognizedNonEXLMethodExcludesRows(t *testing.T) {
	methods := []string{"gptq", "awq", "bitsandbytes", "bnb", "hqq", "eetq", "GPTQ", "BnB", "HQQ"}
	for _, method := range methods {
		t.Run("branch-config-"+method, func(t *testing.T) {
			info := analyzeEXLNMBranchFixture(t, exlNMBranchFixture{
				repo:          "owner/example-exl3-model",
				branches:      []string{"main", "4_25"},
				branchConfigs: map[string]string{"4_25": fmt.Sprintf(`{"quant_method":%q,"bits":4}`, method)},
			})
			if items := exlBranchSelectableItems(info); len(items) != 0 {
				t.Fatalf("recognized non-EXL branch method %q exposed rows: %+v", method, items)
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
			if items := exlBranchSelectableItems(info); len(items) != 0 {
				t.Fatalf("recognized non-EXL main method %q was overridden by the repository name: %+v", method, items)
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
