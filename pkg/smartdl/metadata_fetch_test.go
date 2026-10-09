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
	"strings"
	"testing"
)

// Run the complete analyzer so later specialization cannot undo initial detection.
func analyzeFixture(t *testing.T, paths []string, metadata map[string]string, dataset bool) (*RepoInfo, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			if strings.Contains(r.URL.Path, "/datasets/") && !dataset {
				http.NotFound(w, r)
				return
			}
			nodes := make([]hfTreeNode, 0, len(paths))
			for _, path := range paths {
				nodes = append(nodes, hfTreeNode{Type: "file", Path: path, Size: 100})
			}
			json.NewEncoder(w).Encode(nodes)
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main","targetCommit":"abc"}]}`)
		case strings.Contains(r.URL.Path, "/raw/"):
			body, ok := metadata[filepath.Base(r.URL.Path)]
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
	got, err := a.fetchFile(context.Background(), "owner/model", false, "main", "config.json")
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
				if info.Type != TypeTransformers || info.Quantized == nil || info.Quantized.Method != "exl3" || info.Quantized.Bits != 4 || len(info.Quantized.Backends) != 0 {
					t.Fatalf("incorrect EXL3 result: %+v", info)
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
	content, err := a.fetchFile(context.Background(), "owner/model", false, "main", "config.json")
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
		{"trailing array", `{"quant_method":"exl3","bits":4,"head_bits":6,"layers":[`, map[string]interface{}{"quant_method": "exl3", "bits": float64(4), "head_bits": float64(6)}, false},
		{"incomplete number", `{"quant_method":"exl3","bits":4`, map[string]interface{}{"quant_method": "exl3"}, false},
		{"incomplete method", `{"quant_method":"exl`, nil, true},
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
			if len(got) != len(tc.want) {
				t.Fatalf("head=%v, want %v", got, tc.want)
			}
			for key, value := range tc.want {
				if got[key] != value {
					t.Fatalf("head=%v, want %v", got, tc.want)
				}
			}
		})
	}
}
