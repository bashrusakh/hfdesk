// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// lfsPointerBody is the pointer stub the Hub serves under /raw/ for
// Git-LFS-backed files (issue #123): it is text, never the file's JSON.
const lfsPointerBody = "version https://git-lfs.github.com/spec/v1\noid sha256:2eac9dd74828b4240701601b08df6d1c75d9445d441ea2669388b7e3c1a4cf8a\nsize 32272712\n"

// Run the complete analyzer so later specialization cannot undo initial detection.
// raw and resolved bodies are keyed by base name and served strictly at /raw/
// and /resolve/ respectively (anything else 404s), so a fetch-path regression
// fails instead of silently passing. lfs marks tree entries as Git-LFS-backed;
// their /raw/ bodies should be the pointer stub and their /resolve/ bodies the
// actual bytes.
func analyzeFixtureFiles(t *testing.T, paths []string, lfs map[string]bool, raw, resolved map[string]string, dataset bool) (*RepoInfo, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			if strings.Contains(r.URL.Path, "/datasets/") && !dataset {
				http.NotFound(w, r)
				return
			}
			nodes := make([]map[string]interface{}, 0, len(paths))
			for _, path := range paths {
				node := map[string]interface{}{"type": "file", "path": path, "size": 100}
				if lfs[path] {
					node["size"] = 130
					node["lfs"] = map[string]interface{}{"size": 100, "sha256": "sha256sum"}
				}
				nodes = append(nodes, node)
			}
			json.NewEncoder(w).Encode(nodes)
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main","targetCommit":"abc"}]}`)
		case strings.Contains(r.URL.Path, "/resolve/"):
			body, ok := resolved[filepath.Base(r.URL.Path)]
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, body)
		case strings.Contains(r.URL.Path, "/raw/"):
			body, ok := raw[filepath.Base(r.URL.Path)]
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	return NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()}).Analyze(context.Background(), "owner/model", dataset)
}

// analyzeFixture analyzes a repository with no LFS files; content is served
// only at /raw/, so the fetch path stays pinned.
func analyzeFixture(t *testing.T, paths []string, metadata map[string]string, dataset bool) (*RepoInfo, error) {
	t.Helper()
	return analyzeFixtureFiles(t, paths, nil, metadata, nil, dataset)
}

func TestAnalyzeRootTypePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		paths    []string
		metadata map[string]string
		dataset  bool
		want     RepoType
	}{
		{"mixed safetensors", []string{"config.json", "model.safetensors", "onnx/model.onnx"}, nil, false, TypeTransformers},
		{"mixed bin", []string{"config.json", "pytorch_model.bin", "onnx/model.onnx"}, nil, false, TypeTransformers},
		{"mixed formats", []string{"config.json", "model.safetensors", "pytorch_model.bin", "onnx/model.onnx"}, nil, false, TypeTransformers},
		{"nested config", []string{"subdir/config.json", "model.safetensors"}, nil, false, TypeGeneric},
		{"nested weights", []string{"config.json", "subdir/model.safetensors", "onnx/model.onnx"}, nil, false, TypeONNX},
		{"tokenizer bin is not weights", []string{"config.json", "tokenizer.bin", "onnx/model.onnx"}, nil, false, TypeONNX},
		{"nested quantize", []string{"subdir/quantize_config.json", "model.safetensors"}, nil, false, TypeGeneric},
		{"transformers js", []string{"config.json", "quantize_config.json", "onnx/model.onnx"}, map[string]string{"config.json": `{}`, "quantize_config.json": `{"quant_method":"gptq"}`}, false, TypeONNX},
		{"root GPTQ", []string{"quantize_config.json", "model.safetensors", "onnx/model.onnx"}, map[string]string{"quantize_config.json": `{"quant_method":"gptq","bits":4}`}, false, TypeGPTQ},
		{"root AWQ", []string{"quantize_config.json", "model.safetensors", "onnx/model.onnx"}, map[string]string{"quantize_config.json": `{"quant_method":"awq","bits":4}`}, false, TypeAWQ},
		{"onnx only", []string{"onnx/model.onnx"}, nil, false, TypeONNX},
		{"multimodal", []string{"config.json", "model.safetensors", "onnx/model.onnx"}, map[string]string{"config.json": `{"model_type":"llava"}`}, false, TypeMultimodal},
		{"audio", []string{"config.json", "model.safetensors", "onnx/model.onnx"}, map[string]string{"config.json": `{"model_type":"whisper"}`}, false, TypeAudio},
		{"vision", []string{"config.json", "model.safetensors", "onnx/model.onnx"}, map[string]string{"config.json": `{"model_type":"vit"}`}, false, TypeVision},
		{"GGUF priority", []string{"model.gguf", "model_index.json", "adapter_config.json", "onnx/model.onnx"}, nil, false, TypeGGUF},
		{"Diffusers priority", []string{"model_index.json", "adapter_config.json", "config.json", "model.safetensors", "onnx/model.onnx"}, nil, false, TypeDiffusers},
		{"LoRA priority", []string{"adapter_config.json", "config.json", "model.safetensors", "onnx/model.onnx"}, nil, false, TypeLoRA},
		{"dataset", []string{"config.json", "model.safetensors", "onnx/model.onnx"}, nil, true, TypeDataset},
		{"EXL2", []string{"quantize_config.json", "model.safetensors", "onnx/model.onnx"}, map[string]string{"quantize_config.json": `{"quant_method":"exl2","bits_per_weight":4.5}`}, false, TypeGPTQ},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := analyzeFixture(t, tc.paths, tc.metadata, tc.dataset)
			if err != nil {
				t.Fatal(err)
			}
			if info.Type != tc.want {
				t.Fatalf("type = %s, want %s", info.Type, tc.want)
			}
			if tc.want == TypeTransformers && (info.Transformers == nil || len(info.Transformers.WeightFiles) == 0) {
				t.Fatalf("missing transformers analysis/choices: %+v", info)
			}
			if tc.name == "mixed formats" && len(info.SelectableItems) != 2 {
				t.Fatalf("missing existing format choices: %+v", info.SelectableItems)
			}
			if tc.name == "EXL2" && (info.Quantized == nil || info.Quantized.BitsPerWeight != 4.5) {
				t.Fatalf("EXL2 lost: %+v", info.Quantized)
			}
		})
	}
}

func TestFetchFileReadsWholeChunkedBody(t *testing.T) {
	body := `{"model_type":"llama","padding":"` + strings.Repeat("x", 90000) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/datasets/"):
			http.NotFound(w, r)
			return
		case strings.Contains(r.URL.Path, "/tree/"):
			io.WriteString(w, `[{"type":"file","path":"config.json"},{"type":"file","path":"model.safetensors"}]`)
			return
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[]}`)
			return
		}
		for start := 0; start < len(body); start += 127 {
			end := min(start+127, len(body))
			io.WriteString(w, body[start:end])
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	a := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()})
	got, err := a.fetchFile(context.Background(), "owner/model", false, "main", "config.json", false)
	if err != nil || string(got) != body {
		t.Fatalf("read %d/%d bytes: %v", len(got), len(body), err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	info, err := a.Analyze(context.Background(), "owner/model", false)
	if err != nil {
		t.Fatal(err)
	}
	if info.Metadata["config.json"].(map[string]interface{})["padding"] != strings.Repeat("x", 90000) {
		t.Fatal("chunked metadata not parsed through Analyze")
	}
}

func TestAnalyzeMetadataLimits(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		wantError        bool
	}{
		{"exact cap", "config.json", `{}` + strings.Repeat(" ", maxMetadataSize-2), false},
		{"recover EXL3", "quantization_config.json", `{"quant_method":"exl3","bits":4,"head_bits":6,"layers":["` + strings.Repeat("x", maxMetadataSize) + `"]}`, false},
		{"no head", "quantization_config.json", `{"layers":["` + strings.Repeat("x", maxMetadataSize) + `"]}`, true},
		{"ordinary overflow", "config.json", `{}` + strings.Repeat(" ", maxMetadataSize), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := analyzeFixture(t, []string{"config.json", "model.safetensors", tc.path}, map[string]string{tc.path: tc.body}, false)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), tc.path) || !strings.Contains(err.Error(), "10 MiB") {
					t.Fatalf("expected filename-specific limit error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if info.Metadata[tc.path] == nil {
				t.Fatal("metadata missing")
			}
			if tc.name == "recover EXL3" {
				q := info.Quantized
				if info.Type != TypeTransformers || q == nil || q.Method != "exl3" || len(q.Backends) != 0 {
					t.Fatalf("incorrect EXL3 result: %+v", info)
				}
				// head_bits 6 != bits 4 is mixed precision: the header bits
				// must not be presented as model-wide precision.
				if q.Bits != 0 || !q.MixedPrecision || q.HeadBits != 6 {
					t.Fatalf("header bits presented as model-wide precision: %+v", q)
				}
				if info.Metadata[tc.path].(map[string]interface{})["head_bits"] != float64(6) {
					t.Fatal("head_bits missing")
				}
			}
		})
	}
}

func TestAnalyzeMetadataReadFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/datasets/"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/tree/"):
			io.WriteString(w, `[{"type":"file","path":"config.json"},{"type":"file","path":"model.safetensors"},{"type":"file","path":"quantization_config.json"}]`)
		case strings.HasSuffix(r.URL.Path, "quantization_config.json"):
			w.Header().Set("Content-Length", fmt.Sprint(maxMetadataSize+100))
			io.WriteString(w, `{"quant_method":"exl3","bits":4}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	_, err := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()}).Analyze(context.Background(), "owner/model", false)
	if err == nil || !strings.Contains(err.Error(), "quantization_config.json") || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("read failure hidden/recovered: %v", err)
	}
}

type metadataRoundTripFunc func(*http.Request) (*http.Response, error)

func (f metadataRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type countedMetadataBody struct{ remaining, read int }

func (b *countedMetadataBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), b.remaining)
	for i := range p[:n] {
		p[i] = ' '
	}
	b.remaining -= n
	b.read += n
	return n, nil
}

func (b *countedMetadataBody) Close() error { return nil }

func TestFetchFileBoundsOverflowRead(t *testing.T) {
	body := &countedMetadataBody{remaining: 3 * maxMetadataSize}
	a := NewAnalyzer(AnalyzerOptions{HTTPClient: &http.Client{Transport: metadataRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
	})}})
	content, err := a.fetchFile(context.Background(), "owner/model", false, "main", "config.json", false)
	if !errors.Is(err, errMetadataTooLarge) || len(content) != maxMetadataSize || body.read != maxMetadataSize+1 {
		t.Fatalf("unbounded/incorrect read: returned=%d read=%d err=%v", len(content), body.read, err)
	}
}

func TestDecodeQuantizationHeadCompletedFields(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       map[string]interface{}
		wantError  bool
	}{
		{"trailing array", `{"quant_method":"exl3","bits":4,"head_bits":6,"layers":[`, map[string]interface{}{"quant_method": "exl3", "bits": float64(4), "head_bits": float64(6), partialHeadMarker: true}, false},
		{"incomplete number", `{"quant_method":"exl3","bits":4`, map[string]interface{}{"quant_method": "exl3", "bits": nil, partialHeadMarker: true}, false},
		{"incomplete method", `{"quant_method":"exl`, nil, true},
		{"completed expert_bits", `{"quant_method":"exl3","bits":4,"expert_bits":{"0":{"gu":4,"down":3}},"tail":[`, map[string]interface{}{"quant_method": "exl3", "bits": float64(4), "expert_bits": map[string]interface{}{"0": map[string]interface{}{"gu": float64(4), "down": float64(3)}}, partialHeadMarker: true}, false},
		{"expert_bits scalar", `{"quant_method":"exl3","expert_bits":3.51,"tail":[`, map[string]interface{}{"quant_method": "exl3", "expert_bits": float64(3.51), partialHeadMarker: true}, false},
		{"truncated expert_bits", `{"quant_method":"exl3","expert_bits":{"0":{"gu":4`, map[string]interface{}{"quant_method": "exl3", "expert_bits": nil, partialHeadMarker: true}, false},
		{"complete object is not partial", `{"quant_method":"exl3","bits":4}`, map[string]interface{}{"quant_method": "exl3", "bits": float64(4)}, false},
		{"completed bits_per_weight", `{"quant_method":"exl2","bits":4,"bits_per_weight":4.5,"tail":[`, map[string]interface{}{"quant_method": "exl2", "bits": float64(4), "bits_per_weight": float64(4.5), partialHeadMarker: true}, false},
		{"truncated bits_per_weight", `{"quant_method":"exl2","bits":4,"bits_per_weight":4`, map[string]interface{}{"quant_method": "exl2", "bits": float64(4), "bits_per_weight": nil, partialHeadMarker: true}, false},
		{"completed routed_expert_bits", `{"quant_method":"exl3","routed_expert_bits":{"0":{"gu":4,"down":3}},"tail":[`, map[string]interface{}{"quant_method": "exl3", "routed_expert_bits": map[string]interface{}{"0": map[string]interface{}{"gu": float64(4), "down": float64(3)}}, partialHeadMarker: true}, false},
		{"no completed fields", `{"expert_bits":{"0":{"gu":4`, nil, true},
		{"nested method", `{"layers":{"quant_method":"exl3"},"tail":[`, nil, true},
		{"syntax corruption", `{"quant_method":"exl3","layers":[!`, nil, true},
		{"trailing corruption", `{"quant_method":"exl3"}garbage`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeQuantizationHead([]byte(tc.body))
			if tc.wantError {
				if err == nil {
					t.Fatalf("accepted invalid/unusable head: %v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("head=%v, want %v", got, tc.want)
			}
		})
	}
}

// pinnedExl3Config mirrors the real EXL3 quantization_config.json schema (issue
// #123): per-expert widths vary and only part of the model is quantized, so the
// header "bits" is not model-wide precision.
const pinnedExl3Config = `{"quant_method":"exl3","version":"dsv41-routeB-3p5","bits":4,"head_bits":16,"codebook":"mul1","out_scales":"always","expert_bits":{"0":{"gu":4,"down":4},"1":{"gu":4,"down":3},"2":{"gu":3,"down":3}},"quantized_modules":"layers.N.ffn.experts.E.{w1,w3,w2} only; every other tensor is the original checkpoint's"}`

// quantOnlyPaths is the pinned quantization-only repository shape: root
// weights only plus a root quantization_config.json, no root config.json.
func quantOnlyPaths() []string {
	paths := []string{"quantization_config.json"}
	for i := 1; i <= 48; i++ {
		paths = append(paths, fmt.Sprintf("model-%05d-of-00048.safetensors", i))
	}
	return paths
}

// Issue #123: a quantization-only repository with an LFS-backed config must
// expose its quantization info. The Hub serves the pointer stub at /raw/ and
// the actual JSON at /resolve/.
func TestAnalyzeQuantOnlyLFSRepo(t *testing.T) {
	info, err := analyzeFixtureFiles(t, quantOnlyPaths(),
		map[string]bool{"quantization_config.json": true},
		map[string]string{"quantization_config.json": lfsPointerBody},
		map[string]string{"quantization_config.json": pinnedExl3Config}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Classification and selectable/download behavior stay unchanged.
	if info.Type != TypeGeneric {
		t.Fatalf("type = %s, want %s", info.Type, TypeGeneric)
	}
	if len(info.SelectableItems) != 0 {
		t.Fatalf("selectable items changed: %+v", info.SelectableItems)
	}
	q := info.Quantized
	if q == nil {
		t.Fatalf("quantization info lost: %+v", info)
	}
	if q.Method != "exl3" {
		t.Fatalf("method = %q, want exl3", q.Method)
	}
	if IsGPTQ(q) || info.Type == TypeGPTQ || info.Type == TypeAWQ {
		t.Fatalf("GPTQ misclassification: %s %+v", info.Type, q)
	}
	if len(q.Backends) != 0 {
		t.Fatalf("invented backends: %v", q.Backends)
	}
	// Pinned mixed/per-expert precision: the header bits 4 must not be
	// presented as model-wide precision, and the per-expert 3..4 bit widths
	// must be visible.
	if !q.MixedPrecision || q.Bits != 0 {
		t.Fatalf("header bits presented as model-wide precision: %+v", q)
	}
	if q.HeadBits != 16 || q.ExpertBitsMin != 3 || q.ExpertBitsMax != 4 {
		t.Fatalf("mixed/per-expert precision lost: %+v", q)
	}
	meta, ok := info.Metadata["quantization_config.json"].(map[string]interface{})
	if !ok || meta["quant_method"] != "exl3" {
		t.Fatalf("LFS pointer interpreted as JSON or metadata lost: %+v", info.Metadata)
	}
}

// The excluded-modules contract also holds for a quantization-only root
// repository (issue #123): the declared width is projected and the excluded
// modules are carried alongside it, without mixed-precision semantics.
func TestAnalyzeQuantOnlyExcludedModulesKeepDeclaredBits(t *testing.T) {
	body := `{"quant_method":"gptq","bits":4,"modules_to_not_convert":["lm_head","embed_tokens"]}`
	info, err := analyzeFixtureFiles(t, quantOnlyPaths(),
		map[string]bool{"quantization_config.json": true},
		map[string]string{"quantization_config.json": lfsPointerBody},
		map[string]string{"quantization_config.json": body}, false)
	if err != nil {
		t.Fatal(err)
	}
	if info.Type != TypeGeneric {
		t.Fatalf("type = %s, want %s", info.Type, TypeGeneric)
	}
	q := info.Quantized
	if q == nil || q.Bits != 4 {
		t.Fatalf("declared width dropped for excluded modules: %+v", info.Quantized)
	}
	if q.MixedPrecision {
		t.Fatalf("exclusions alone imply mixed precision: %+v", q)
	}
	if len(q.ExcludedModules) != 2 || q.ExcludedModules[0] != "lm_head" || q.ExcludedModules[1] != "embed_tokens" {
		t.Fatalf("excluded modules not projected alongside bits: %+v", q)
	}
}

// Oversized pinned config: the bounded head keeps the completed leading
// fields (including expert_bits) without reading the whole config.
func TestAnalyzeQuantOnlyOversizedConfig(t *testing.T) {
	body := `{"quant_method":"exl3","bits":4,"head_bits":16,"expert_bits":{"0":{"gu":4,"down":3},"1":{"gu":3,"down":3}},"tensor_storage":{"` + strings.Repeat("x", maxMetadataSize) + `"}}`
	info, err := analyzeFixtureFiles(t, quantOnlyPaths(),
		map[string]bool{"quantization_config.json": true},
		map[string]string{"quantization_config.json": lfsPointerBody},
		map[string]string{"quantization_config.json": body}, false)
	if err != nil {
		t.Fatal(err)
	}
	if info.Type != TypeGeneric {
		t.Fatalf("type = %s, want %s", info.Type, TypeGeneric)
	}
	q := info.Quantized
	if q == nil || q.Method != "exl3" || len(q.Backends) != 0 {
		t.Fatalf("incorrect oversized result: %+v", info.Quantized)
	}
	if !q.MixedPrecision || q.Bits != 0 || q.HeadBits != 16 {
		t.Fatalf("header bits presented as model-wide precision: %+v", q)
	}
	if q.ExpertBitsMin != 3 || q.ExpertBitsMax != 4 {
		t.Fatalf("completed expert_bits dropped: %+v", q)
	}
	if info.Metadata["quantization_config.json"].(map[string]interface{})["quant_method"] != "exl3" {
		t.Fatal("recovered head missing")
	}
}

// Real pinned order: expert_bits is huge and truncated at the bound. The
// completed head fields still reach the projection and stay explicit.
func TestAnalyzeQuantOnlyOversizedTruncatedExpertBits(t *testing.T) {
	body := `{"quant_method":"exl3","version":"dsv41-routeB-3p5","bits":4,"head_bits":16,"expert_bits":{"0":{"gu":4,` + strings.Repeat(" ", maxMetadataSize)
	info, err := analyzeFixtureFiles(t, quantOnlyPaths(),
		map[string]bool{"quantization_config.json": true},
		map[string]string{"quantization_config.json": lfsPointerBody},
		map[string]string{"quantization_config.json": body}, false)
	if err != nil {
		t.Fatal(err)
	}
	q := info.Quantized
	if q == nil || q.Method != "exl3" || len(q.Backends) != 0 {
		t.Fatalf("incorrect oversized result: %+v", info.Quantized)
	}
	// head_bits 16 != bits 4 is already mixed; the truncated expert_bits map
	// must not be invented.
	if !q.MixedPrecision || q.Bits != 0 || q.HeadBits != 16 {
		t.Fatalf("header bits presented as model-wide precision: %+v", q)
	}
	if q.ExpertBitsMin != 0 || q.ExpertBitsMax != 0 {
		t.Fatalf("invented expert widths: %+v", q)
	}
}

// The proven probe shape: bounded recovery stops with a precision-relevant
// declaration cut at the cap (expert_bits started but never completed) while
// the recovered fields look uniform (head_bits == bits, or head_bits absent).
// No model-wide uniform claim may survive, the partial recovery must be
// explicit, and the cut declaration must not be silently dropped or invented.
func TestAnalyzeQuantOnlyOversizedTruncatedWidthDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"equal head width", `{"quant_method":"exl3","bits":4,"head_bits":4,"expert_bits":{"0":{"gu":4,` + strings.Repeat(" ", maxMetadataSize)},
		{"head width absent", `{"quant_method":"exl3","bits":4,"expert_bits":{"0":{"gu":4,` + strings.Repeat(" ", maxMetadataSize)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := analyzeFixtureFiles(t, quantOnlyPaths(),
				map[string]bool{"quantization_config.json": true},
				map[string]string{"quantization_config.json": lfsPointerBody},
				map[string]string{"quantization_config.json": tc.body}, false)
			if err != nil {
				t.Fatal(err)
			}
			q := info.Quantized
			if q == nil || q.Method != "exl3" {
				t.Fatalf("incorrect oversized result: %+v", info.Quantized)
			}
			// The truncated per-expert declaration blocks any model-wide
			// uniform claim even though head_bits == bits looks uniform.
			if q.Bits != 0 {
				t.Fatalf("uniform model-wide precision claimed from a truncated config: %+v", q)
			}
			if !q.ConfigPartial {
				t.Fatalf("partial head recovery not explicit: %+v", q)
			}
			if !q.MixedPrecision {
				t.Fatalf("truncated per-expert width declaration dropped silently: %+v", q)
			}
			if q.ExpertBitsMin != 0 || q.ExpertBitsMax != 0 {
				t.Fatalf("invented expert widths from a cut declaration: %+v", q)
			}
			meta, ok := info.Metadata["quantization_config.json"].(map[string]interface{})
			if !ok {
				t.Fatalf("recovered head missing: %+v", info.Metadata)
			}
			if v, exists := meta["expert_bits"]; !exists || v != nil {
				t.Fatalf("cut expert_bits declaration dropped or invented: %+v", meta)
			}
			if meta[partialHeadMarker] != true {
				t.Fatalf("partial recovery not explicit in metadata: %+v", meta)
			}
		})
	}
}

// A truncation that carries no width information is a judgment call: the
// recovered fields look uniform, but uniformity is not established, so no
// model-wide uniform claim is made and the recovered fields stay usable.
func TestAnalyzeQuantOnlyOversizedTruncationWithoutWidthInfo(t *testing.T) {
	body := `{"quant_method":"gptq","bits":4,"group_size":128,"layers":[` + strings.Repeat(" ", maxMetadataSize)
	info, err := analyzeFixtureFiles(t, quantOnlyPaths(),
		map[string]bool{"quantization_config.json": true},
		map[string]string{"quantization_config.json": lfsPointerBody},
		map[string]string{"quantization_config.json": body}, false)
	if err != nil {
		t.Fatal(err)
	}
	q := info.Quantized
	if q == nil || q.Method != "gptq" {
		t.Fatalf("incorrect oversized result: %+v", info.Quantized)
	}
	if q.Bits != 0 || !q.ConfigPartial {
		t.Fatalf("uniform model-wide precision claimed from an unreadable tail: %+v", q)
	}
	if q.MixedPrecision {
		t.Fatalf("mixed precision invented without declaration: %+v", q)
	}
	if meta, _ := info.Metadata["quantization_config.json"].(map[string]interface{}); meta["bits"] != float64(4) {
		t.Fatalf("completed leading fields lost: %+v", meta)
	}
}

// Oversized EXL2: the effective precision must survive bounded recovery so
// nominal bits can never stand in for it (issue #123).
func TestAnalyzeQuantOnlyOversizedExl2BitsPerWeight(t *testing.T) {
	body := `{"quant_method":"exl2","bits":4,"bits_per_weight":4.5,"layers":[` + strings.Repeat(" ", maxMetadataSize)
	info, err := analyzeFixtureFiles(t, quantOnlyPaths(),
		map[string]bool{"quantization_config.json": true},
		map[string]string{"quantization_config.json": lfsPointerBody},
		map[string]string{"quantization_config.json": body}, false)
	if err != nil {
		t.Fatal(err)
	}
	q := info.Quantized
	if q == nil || q.Method != "exl2" {
		t.Fatalf("incorrect oversized result: %+v", info.Quantized)
	}
	if q.BitsPerWeight != 4.5 {
		t.Fatalf("EXL2 bits per weight lost to bounded recovery: %+v", q)
	}
	if q.Bits != 0 || !q.MixedPrecision {
		t.Fatalf("nominal bits presented as model-wide precision: %+v", q)
	}
	if !q.ConfigPartial {
		t.Fatalf("partial head recovery not explicit: %+v", q)
	}
}

// Unknown methods keep their literal name: no mislabeling, no description and
// no backend claims.
func TestAnalyzeQuantOnlyUnknownMethod(t *testing.T) {
	body := `{"quant_method":"quipsharp","bits":4,"head_bits":8,"expert_bits":{"0":{"gu":4,"down":4}}}`
	info, err := analyzeFixtureFiles(t, quantOnlyPaths(),
		map[string]bool{"quantization_config.json": true},
		map[string]string{"quantization_config.json": lfsPointerBody},
		map[string]string{"quantization_config.json": body}, false)
	if err != nil {
		t.Fatal(err)
	}
	q := info.Quantized
	if q == nil || q.Method != "quipsharp" {
		t.Fatalf("literal method lost: %+v", info.Quantized)
	}
	if q.MethodDescription != "" || len(q.Backends) != 0 {
		t.Fatalf("unknown method mislabeled: %+v", q)
	}
	if !q.MixedPrecision || q.Bits != 0 {
		t.Fatalf("header bits presented as model-wide precision: %+v", q)
	}
}

// A file the tree did not mark as LFS whose /raw/ body is a pointer stub is
// retried through /resolve/ once instead of losing the metadata.
func TestAnalyzeUnmarkedLFSFallback(t *testing.T) {
	paths := []string{"config.json", "model.safetensors", "quantization_config.json"}
	info, err := analyzeFixtureFiles(t, paths, nil,
		map[string]string{"config.json": `{"model_type":"llama"}`, "quantization_config.json": lfsPointerBody},
		map[string]string{"quantization_config.json": `{"quant_method":"exl3","bits":4,"head_bits":6}`}, false)
	if err != nil {
		t.Fatal(err)
	}
	if info.Type != TypeTransformers {
		t.Fatalf("type = %s, want %s", info.Type, TypeTransformers)
	}
	q := info.Quantized
	if q == nil || q.Method != "exl3" {
		t.Fatalf("metadata lost to LFS pointer: %+v", info.Quantized)
	}
	if meta := info.Metadata["quantization_config.json"].(map[string]interface{}); meta["quant_method"] != "exl3" {
		t.Fatalf("pointer interpreted as JSON: %+v", meta)
	}
}

// A pointer that persists at /resolve/ is an explicit failure, never silently
// dropped metadata.
func TestAnalyzeLFSPointerUnresolved(t *testing.T) {
	paths := []string{"config.json", "model.safetensors", "quantization_config.json"}
	_, err := analyzeFixtureFiles(t, paths, map[string]bool{"quantization_config.json": true},
		map[string]string{"config.json": `{}`},
		map[string]string{"quantization_config.json": lfsPointerBody}, false)
	if err == nil || !strings.Contains(err.Error(), "quantization_config.json") || !strings.Contains(err.Error(), "pointer") {
		t.Fatalf("unresolved lfs pointer hidden: %v", err)
	}
}

// When /raw/ served a pointer stub and the /resolve/ retry cannot replace it,
// the lost metadata is an explicit failure, not a silent skip.
func TestAnalyzeUnmarkedLFSResolveFailure(t *testing.T) {
	paths := []string{"config.json", "model.safetensors", "quantization_config.json"}
	_, err := analyzeFixtureFiles(t, paths, nil,
		map[string]string{"config.json": `{}`, "quantization_config.json": lfsPointerBody},
		nil, false)
	if err == nil || !strings.Contains(err.Error(), "quantization_config.json") || !strings.Contains(err.Error(), "pointer") {
		t.Fatalf("lost LFS metadata not explicit: %v", err)
	}
}
