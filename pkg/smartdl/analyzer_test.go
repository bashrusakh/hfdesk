// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewAnalyzer(t *testing.T) {
	t.Run("default options", func(t *testing.T) {
		analyzer := NewAnalyzer(AnalyzerOptions{})
		if analyzer.endpoint != defaultEndpoint {
			t.Errorf("endpoint = %q, want %q", analyzer.endpoint, defaultEndpoint)
		}
		if analyzer.client == nil {
			t.Error("client should not be nil")
		}
	})

	t.Run("custom endpoint", func(t *testing.T) {
		analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: "https://hf-mirror.com"})
		if analyzer.endpoint != "https://hf-mirror.com" {
			t.Errorf("endpoint = %q", analyzer.endpoint)
		}
	})

	t.Run("endpoint trailing slash removed", func(t *testing.T) {
		analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: "https://huggingface.co/"})
		if analyzer.endpoint != "https://huggingface.co" {
			t.Errorf("endpoint = %q, trailing slash not removed", analyzer.endpoint)
		}
	})

	t.Run("with token", func(t *testing.T) {
		analyzer := NewAnalyzer(AnalyzerOptions{Token: "hf_test_token"})
		if analyzer.token != "hf_test_token" {
			t.Errorf("token = %q", analyzer.token)
		}
	})

	t.Run("custom HTTP client", func(t *testing.T) {
		customClient := &http.Client{Timeout: 60 * time.Second}
		analyzer := NewAnalyzer(AnalyzerOptions{HTTPClient: customClient})
		if analyzer.client != customClient {
			t.Error("custom client not used")
		}
	})
}

func TestAnalyzeExplicitNamespace(t *testing.T) {
	const repo = "owner/both"
	newHub := func(t *testing.T) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
		t.Helper()
		var modelTrees, datasetTrees atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/models/owner/both/tree/main":
				modelTrees.Add(1)
				io.WriteString(w, `[{"type":"file","path":"model.safetensors","size":10}]`)
			case r.URL.Path == "/api/datasets/owner/both/tree/main":
				datasetTrees.Add(1)
				io.WriteString(w, `[{"type":"file","path":"data.parquet","size":20}]`)
			case strings.HasSuffix(r.URL.Path, "/refs"):
				io.WriteString(w, `{"branches":[]}`)
			case strings.HasSuffix(r.URL.Path, "/auth-check"):
				// The merged analyzer probes the optional gated-access endpoint;
				// this fake mirror does not implement it, so access stays
				// fail-open.
				http.NotFound(w, r)
			default:
				t.Errorf("unexpected request: %s", r.URL)
				http.NotFound(w, r)
			}
		}))
		return srv, &modelTrees, &datasetTrees
	}

	t.Run("explicit model", func(t *testing.T) {
		hub, modelTrees, datasetTrees := newHub(t)
		defer hub.Close()
		analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL})

		info, err := analyzer.AnalyzeExplicitWithRevision(context.Background(), repo, false, "main")
		if err != nil || info == nil {
			t.Fatalf("info = %v, error = %v; want model analysis", info, err)
		}
		if info.IsDataset || info.Type == TypeDataset {
			t.Fatalf("is_dataset/type = %v/%v; want model", info.IsDataset, info.Type)
		}
		if len(info.Files) != 1 || info.Files[0].Path != "model.safetensors" {
			t.Fatalf("files = %+v; want model namespace", info.Files)
		}
		if modelTrees.Load() != 1 || datasetTrees.Load() != 0 {
			t.Fatalf("model/dataset tree requests = %d/%d; explicit model must not probe the dataset", modelTrees.Load(), datasetTrees.Load())
		}
	})

	t.Run("explicit dataset", func(t *testing.T) {
		hub, modelTrees, datasetTrees := newHub(t)
		defer hub.Close()
		analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL})

		info, err := analyzer.AnalyzeExplicitWithRevision(context.Background(), repo, true, "main")
		if err != nil || info == nil {
			t.Fatalf("info = %v, error = %v; want dataset analysis", info, err)
		}
		if !info.IsDataset || info.Type != TypeDataset {
			t.Fatalf("is_dataset/type = %v/%v; want dataset", info.IsDataset, info.Type)
		}
		if len(info.Files) != 1 || info.Files[0].Path != "data.parquet" {
			t.Fatalf("files = %+v; want dataset namespace", info.Files)
		}
		if modelTrees.Load() != 0 || datasetTrees.Load() != 1 {
			t.Fatalf("model/dataset tree requests = %d/%d; explicit dataset must not probe the model", modelTrees.Load(), datasetTrees.Load())
		}
	})

	t.Run("auto detect still needs selection", func(t *testing.T) {
		hub, modelTrees, datasetTrees := newHub(t)
		defer hub.Close()
		analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL})

		info, err := analyzer.Analyze(context.Background(), repo, false)
		if info != nil || !errors.Is(err, ErrBothExist) {
			t.Fatalf("info = %v, error = %v; want ErrBothExist", info, err)
		}
		if modelTrees.Load() != 1 || datasetTrees.Load() != 1 {
			t.Fatalf("model/dataset tree requests = %d/%d; auto detection must probe both", modelTrees.Load(), datasetTrees.Load())
		}
	})
}

// TestAnalyzeDatasetProbeDegradation pins the surfacing of a failed
// post-success dataset probe: the probe stays optional (the analysis succeeds
// on the model files exactly as before), but its failure now carries a
// degradation warning with the underlying cause instead of vanishing. A
// definitive namespace miss and the successful probe (ErrBothExist, asserted
// in TestAnalyzeExplicitNamespace) yield no warning, and caller cancellation
// never becomes a warning cause, while a gated dataset twin degrades with the
// non-secret gated text surfaced in the warning message.
func TestAnalyzeDatasetProbeDegradation(t *testing.T) {
	newProbeHub := func(t *testing.T, datasetTree http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
		t.Helper()
		var datasetTrees atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/datasets/owner/probe/tree/main":
				datasetTrees.Add(1)
				datasetTree(w, r)
			case r.URL.Path == "/api/models/owner/probe/tree/main":
				_, _ = io.WriteString(w, `[{"type":"file","path":"config.json","size":100},{"type":"file","path":"model.safetensors","size":100}]`)
			case strings.HasSuffix(r.URL.Path, "/refs"):
				_, _ = io.WriteString(w, `{"branches":[]}`)
			case strings.HasSuffix(r.URL.Path, "/auth-check"):
				// Fail-open auth-check fixture, as in TestAnalyzeExplicitNamespace.
				http.NotFound(w, r)
			case strings.Contains(r.URL.Path, "/raw/"):
				_, _ = io.WriteString(w, `{"model_type":"llama"}`)
			default:
				t.Errorf("unexpected request: %s", r.URL)
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		return srv, &datasetTrees
	}

	wantSingleProbeWarning := func(t *testing.T, info *RepoInfo, wantCause string) AnalysisWarning {
		t.Helper()
		if len(info.AnalysisWarnings) != 1 {
			t.Fatalf("analysis warnings = %+v, want exactly one both-namespace degradation warning", info.AnalysisWarnings)
		}
		warning := info.AnalysisWarnings[0]
		if warning.Code != "both_namespace_detection_degraded" || !warning.Retryable {
			t.Fatalf("analysis warning = %+v, want the retryable both-namespace degradation code", warning)
		}
		if len(warning.Causes) != 1 || warning.Causes[0] != wantCause {
			t.Fatalf("warning causes = %v, want [%s]", warning.Causes, wantCause)
		}
		if !strings.Contains(warning.Message, "ErrBothExist") || !strings.Contains(warning.Message, "degraded") {
			t.Fatalf("warning message = %q, want it to name the degraded ErrBothExist detection", warning.Message)
		}
		return warning
	}

	// The three synchronous outcomes — a genuinely failed probe, a definitive
	// namespace miss, and a gated dataset twin — differ only in the probe
	// server's behaviour and the expected warning cause and message; each row
	// keeps its former t.Run label so a failure still names the case.
	const gatedMessage = "Access to this resource is gated."
	for _, tc := range []struct {
		name            string
		probe           http.HandlerFunc
		infoWant        string
		filesWant       string
		treesWant       string
		wantCause       string   // empty: assert that no warning is emitted
		warningContains []string // substrings the single warning must carry
		warningWant     string   // failure text for a missing substring
	}{
		{
			name: "failed probe succeeds with the underlying cause",
			probe: func(w http.ResponseWriter, r *http.Request) {
				// A completed fetch whose body is not a tree listing: the
				// existence answer is genuinely unknown — neither present nor
				// an authoritative namespace miss.
				_, _ = io.WriteString(w, `<html>broken gateway</html>`)
			},
			infoWant:        "a failed dataset probe must not fail the analysis",
			filesWant:       "want the model result preserved",
			treesWant:       "the probe must actually run for this degradation to discriminate",
			wantCause:       "probe_unavailable",
			warningContains: []string{"decode response"},
			warningWant:     "want the underlying probe error surfaced",
		},
		{
			name:      "definitive namespace miss carries no warning",
			probe:     http.NotFound,
			infoWant:  "want model analysis",
			filesWant: "want the model result",
			treesWant: "auto-detection must still probe the dataset namespace",
		},
		{
			name: "gated dataset twin degrades without failing the analysis",
			probe: func(w http.ResponseWriter, r *http.Request) {
				// The dataset twin exists but requires an accepted agreement:
				// 401/403 with the explicit GatedRepo marker is neither an
				// authoritative namespace miss nor a completed listing, so the
				// probe degrades detection instead of answering it.
				w.Header().Set("X-Error-Code", "GatedRepo")
				w.Header().Set("X-Error-Message", gatedMessage)
				w.WriteHeader(http.StatusForbidden)
			},
			infoWant:        "a gated dataset twin must not fail the model analysis",
			filesWant:       "want the model result preserved",
			treesWant:       "the gated probe must actually run for this degradation to discriminate",
			wantCause:       "probe_unavailable",
			warningContains: []string{"gated", gatedMessage},
			warningWant:     "want the non-secret gated text surfaced",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, datasetTrees := newProbeHub(t, tc.probe)

			info, err := NewAnalyzer(AnalyzerOptions{Endpoint: hub.URL, HTTPClient: hub.Client()}).
				Analyze(context.Background(), "owner/probe", false)
			if err != nil || info == nil {
				t.Fatalf("info = %v, error = %v; %s", info, err, tc.infoWant)
			}
			if info.IsDataset || len(info.Files) != 2 {
				t.Fatalf("analysis = is_dataset %v files %+v; %s", info.IsDataset, info.Files, tc.filesWant)
			}
			if got := datasetTrees.Load(); got != 1 {
				t.Fatalf("dataset tree requests = %d, want 1: %s", got, tc.treesWant)
			}
			if tc.wantCause == "" {
				if len(info.AnalysisWarnings) != 0 {
					t.Fatalf("analysis warnings = %+v, want none: a definitive namespace miss is a completed probe, not a degradation", info.AnalysisWarnings)
				}
				return
			}
			warning := wantSingleProbeWarning(t, info, tc.wantCause)
			for _, want := range tc.warningContains {
				if !strings.Contains(warning.Message, want) {
					t.Fatalf("warning message = %q, %s", warning.Message, tc.warningWant)
				}
			}
		})
	}

	t.Run("stalled probe succeeds with budget-expiry warning", func(t *testing.T) {
		srv, stalledHits := newStalledBodyFixture(t, analyzeNormalEndpoints, func(path string) bool {
			return strings.Contains(path, "/api/datasets/")
		})

		const testBudget = 500 * time.Millisecond
		oldBudget := treePageFetchTimeout
		treePageFetchTimeout = testBudget
		defer func() { treePageFetchTimeout = oldBudget }()

		info, err := awaitBudgetedAnalysis(t, testBudget, metadataBudgetSlack, metadataBudgetWatchdogSlack, "",
			func(ctx context.Context) (*RepoInfo, error) {
				return NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()}).
					Analyze(ctx, "owner/model", false)
			})

		if err != nil || info == nil {
			t.Fatalf("info = %v, error = %v; a stalled dataset probe must not fail the analysis", info, err)
		}
		if got := stalledHits.Load(); got != 1 {
			t.Errorf("stalled dataset tree requests = %d, want exactly 1 (an expired page budget is terminal, with no retry)", got)
		}
		if info.IsDataset || len(info.Files) != 2 {
			t.Fatalf("analysis = is_dataset %v files %+v; want the model result preserved", info.IsDataset, info.Files)
		}
		warning := wantSingleProbeWarning(t, info, "probe_timeout")
		if !strings.Contains(warning.Message, "timed out after 500ms") {
			t.Errorf("warning message = %q, want the budget expiry text as-is", warning.Message)
		}
		if strings.Contains(warning.Message, "cancel") {
			t.Errorf("warning message = %q; a budget expiry must not masquerade as caller cancellation", warning.Message)
		}
	})

	t.Run("caller cancellation is not reported as a warning", func(t *testing.T) {
		srv, stalledHits := newStalledBodyFixture(t, analyzeNormalEndpoints, func(path string) bool {
			return strings.Contains(path, "/api/datasets/")
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type result struct {
			info *RepoInfo
			err  error
		}
		done := make(chan result, 1)
		go func() {
			info, err := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()}).
				Analyze(ctx, "owner/model", false)
			done <- result{info, err}
		}()

		// Cancel only once the probe request is actually in flight, so the
		// cancellation lands inside the dataset probe itself.
		deadline := time.Now().Add(5 * time.Second)
		for stalledHits.Load() == 0 {
			if time.Now().After(deadline) {
				cancel()
				t.Fatal("dataset probe never reached the stalled endpoint")
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()

		select {
		case res := <-done:
			// The established cancellation outcome stays untouched: the
			// model analysis completes exactly as it did before this
			// warning surface existed; only the warning must not appear.
			if res.err != nil || res.info == nil {
				t.Fatalf("info = %v, error = %v; cancellation handling must stay exactly as before this warning surface", res.info, res.err)
			}
			if len(res.info.AnalysisWarnings) != 0 {
				t.Fatalf("analysis warnings = %+v, want none: caller cancellation must never become a degradation warning", res.info.AnalysisWarnings)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("analysis did not finish after caller cancellation")
		}
	})
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		bytes    int64
		expected string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TiB"},
		{4 * 1024 * 1024 * 1024, "4.0 GiB"},
		{int64(4.5 * 1024 * 1024 * 1024), "4.5 GiB"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := humanSize(tt.bytes)
			if result != tt.expected {
				t.Errorf("humanSize(%d) = %q, want %q", tt.bytes, result, tt.expected)
			}
		})
	}
}

func TestPathEscapeAll(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{"path/to/file", "path/to/file"},
		{"path with spaces", "path%20with%20spaces"},
		{"特殊字符", "%E7%89%B9%E6%AE%8A%E5%AD%97%E7%AC%A6"},
		{"file[1].txt", "file%5B1%5D.txt"},
		{"path/file name.txt", "path/file%20name.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := pathEscapeAll(tt.input)
			if result != tt.expected {
				t.Errorf("pathEscapeAll(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestAnalyzer_DetectType(t *testing.T) {
	analyzer := NewAnalyzer(AnalyzerOptions{})

	tests := []struct {
		name      string
		files     []FileInfo
		isDataset bool
		expected  RepoType
	}{
		{
			name:      "dataset",
			files:     []FileInfo{{Name: "data.parquet"}},
			isDataset: true,
			expected:  TypeDataset,
		},
		{
			name: "GGUF model",
			files: []FileInfo{
				{Name: "model.Q4_K_M.gguf", Path: "model.Q4_K_M.gguf"},
				{Name: "config.json", Path: "config.json"},
			},
			expected: TypeGGUF,
		},
		{
			name: "Diffusers model",
			files: []FileInfo{
				{Name: "model_index.json", Path: "model_index.json"},
				{Name: "unet/diffusion_pytorch_model.safetensors", Path: "unet/diffusion_pytorch_model.safetensors"},
			},
			expected: TypeDiffusers,
		},
		{
			name: "LoRA adapter",
			files: []FileInfo{
				{Name: "adapter_config.json", Path: "adapter_config.json"},
				{Name: "adapter_model.safetensors", Path: "adapter_model.safetensors"},
			},
			expected: TypeLoRA,
		},
		{
			name: "GPTQ model",
			files: []FileInfo{
				{Name: "quantize_config.json", Path: "quantize_config.json"},
				{Name: "model.safetensors", Path: "model.safetensors"},
			},
			expected: TypeGPTQ,
		},
		{
			name: "ONNX model",
			files: []FileInfo{
				{Name: "model.onnx", Path: "model.onnx"},
				{Name: "config.json", Path: "config.json"},
			},
			expected: TypeONNX,
		},
		{
			name: "Transformers safetensors",
			files: []FileInfo{
				{Name: "config.json", Path: "config.json"},
				{Name: "model.safetensors", Path: "model.safetensors"},
			},
			expected: TypeTransformers,
		},
		{
			name: "Transformers bin",
			files: []FileInfo{
				{Name: "config.json", Path: "config.json"},
				{Name: "pytorch_model.bin", Path: "pytorch_model.bin"},
			},
			expected: TypeTransformers,
		},
		{
			name: "Generic",
			files: []FileInfo{
				{Name: "README.md", Path: "README.md"},
			},
			expected: TypeGeneric,
		},
		{
			name: "Config without weights",
			files: []FileInfo{
				{Name: "config.json", Path: "config.json"},
			},
			expected: TypeGeneric,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := analyzer.detectType(tt.files, tt.isDataset)
			if result != tt.expected {
				t.Errorf("detectType() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestAnalyzer_TreeURL(t *testing.T) {
	analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: "https://huggingface.co"})

	tests := []struct {
		name      string
		repo      string
		isDataset bool
		revision  string
		prefix    string
		expected  string
	}{
		{
			name:     "model main",
			repo:     "TheBloke/Mistral-7B",
			revision: "main",
			expected: "https://huggingface.co/api/models/TheBloke/Mistral-7B/tree/main",
		},
		{
			name:      "dataset main",
			repo:      "facebook/flores",
			isDataset: true,
			revision:  "main",
			expected:  "https://huggingface.co/api/datasets/facebook/flores/tree/main",
		},
		{
			name:     "with prefix",
			repo:     "owner/repo",
			revision: "main",
			prefix:   "subdir",
			expected: "https://huggingface.co/api/models/owner/repo/tree/main/subdir",
		},
		{
			name:     "special revision",
			repo:     "owner/repo",
			revision: "v1.0.0",
			expected: "https://huggingface.co/api/models/owner/repo/tree/v1.0.0",
		},
		{
			name:     "branch path revision is one escaped segment",
			repo:     "owner/repo",
			revision: "feature/4.00bpw",
			expected: "https://huggingface.co/api/models/owner/repo/tree/feature%2F4.00bpw",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := analyzer.treeURL(tt.repo, tt.isDataset, tt.revision, tt.prefix)
			if result != tt.expected {
				t.Errorf("treeURL() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestAnalyzer_RepoURL(t *testing.T) {
	analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: "https://huggingface.co"})

	tests := []struct {
		name      string
		repo      string
		isDataset bool
		expected  string
	}{
		{
			name:     "model",
			repo:     "TheBloke/Mistral-7B",
			expected: "https://huggingface.co/TheBloke/Mistral-7B",
		},
		{
			name:      "dataset",
			repo:      "facebook/flores",
			isDataset: true,
			expected:  "https://huggingface.co/datasets/facebook/flores",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := analyzer.repoURL(tt.repo, tt.isDataset)
			if result != tt.expected {
				t.Errorf("repoURL() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestAnalyzer_RawURL(t *testing.T) {
	analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: "https://huggingface.co"})

	tests := []struct {
		name      string
		repo      string
		isDataset bool
		revision  string
		path      string
		expected  string
	}{
		{
			name:     "model config",
			repo:     "owner/repo",
			revision: "main",
			path:     "config.json",
			expected: "https://huggingface.co/owner/repo/raw/main/config.json",
		},
		{
			name:      "dataset file",
			repo:      "owner/dataset",
			isDataset: true,
			revision:  "main",
			path:      "data/train.parquet",
			expected:  "https://huggingface.co/datasets/owner/dataset/raw/main/data/train.parquet",
		},
		{
			name:     "nested path",
			repo:     "owner/repo",
			revision: "v1.0",
			path:     "deep/nested/file.txt",
			expected: "https://huggingface.co/owner/repo/raw/v1.0/deep/nested/file.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := analyzer.rawURL(tt.repo, tt.isDataset, tt.revision, tt.path)
			if result != tt.expected {
				t.Errorf("rawURL() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestAnalyzer_AddAuth(t *testing.T) {
	t.Run("with token", func(t *testing.T) {
		analyzer := NewAnalyzer(AnalyzerOptions{Token: "hf_test_token"})
		req, _ := http.NewRequest("GET", "http://example.com", nil)
		analyzer.addAuth(req)

		if auth := req.Header.Get("Authorization"); auth != "Bearer hf_test_token" {
			t.Errorf("Authorization = %q", auth)
		}
		if ua := req.Header.Get("User-Agent"); ua != "hfdownloader/3" {
			t.Errorf("User-Agent = %q", ua)
		}
	})

	t.Run("without token", func(t *testing.T) {
		analyzer := NewAnalyzer(AnalyzerOptions{})
		req, _ := http.NewRequest("GET", "http://example.com", nil)
		analyzer.addAuth(req)

		if auth := req.Header.Get("Authorization"); auth != "" {
			t.Errorf("Authorization should be empty, got %q", auth)
		}
		if ua := req.Header.Get("User-Agent"); ua != "hfdownloader/3" {
			t.Errorf("User-Agent = %q", ua)
		}
	})
}

func TestAnalyzer_DetectType_Priority(t *testing.T) {
	analyzer := NewAnalyzer(AnalyzerOptions{})

	// GGUF should have priority over transformers
	t.Run("GGUF over transformers", func(t *testing.T) {
		files := []FileInfo{
			{Name: "config.json", Path: "config.json"},
			{Name: "model.safetensors", Path: "model.safetensors"},
			{Name: "model.Q4_K_M.gguf", Path: "model.Q4_K_M.gguf"},
		}
		result := analyzer.detectType(files, false)
		if result != TypeGGUF {
			t.Errorf("expected GGUF, got %q", result)
		}
	})

	// Diffusers should have priority over transformers
	t.Run("Diffusers over transformers", func(t *testing.T) {
		files := []FileInfo{
			{Name: "config.json", Path: "config.json"},
			{Name: "model.safetensors", Path: "model.safetensors"},
			{Name: "model_index.json", Path: "model_index.json"},
		}
		result := analyzer.detectType(files, false)
		if result != TypeDiffusers {
			t.Errorf("expected Diffusers, got %q", result)
		}
	})

	// LoRA should have priority over transformers
	t.Run("LoRA over transformers", func(t *testing.T) {
		files := []FileInfo{
			{Name: "config.json", Path: "config.json"},
			{Name: "adapter_config.json", Path: "adapter_config.json"},
			{Name: "adapter_model.safetensors", Path: "adapter_model.safetensors"},
		}
		result := analyzer.detectType(files, false)
		if result != TypeLoRA {
			t.Errorf("expected LoRA, got %q", result)
		}
	})
}
