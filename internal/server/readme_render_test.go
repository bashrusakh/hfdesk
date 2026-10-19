// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRenderReadmeHTML(t *testing.T) {
	md := "---\nlicense: mit\n---\n\n# Title\n\n" +
		"![logo](assets/logo.png)\n\n" +
		"![abs](https://huggingface.co/owner/model/raw/main/assets/abs.png)\n\n" +
		"![badge](https://img.shields.io/badge/x-y.svg)\n\n" +
		"[docs](docs/guide.md)\n\n" +
		"| a | b |\n|---|---|\n| 1 | 2 |\n\n" +
		"<script>alert(1)</script>\n"
	html := renderReadmeHTML(md,
		"https://huggingface.co/owner/model/resolve/main/",
		"https://huggingface.co/owner/model/blob/main/",
		"https://huggingface.co")

	cases := []struct {
		name    string
		sub     string
		present bool
	}{
		{"frontmatter stripped", "license: mit", false},
		{"title rendered", "<h1", true},
		{"table rendered", "<table", true},
		{"hf image proxied", "/api/readme-asset?url=", true},
		{"resolve path encoded", "resolve%2Fmain%2Fassets%2Flogo.png", true},
		{"absolute hf raw image normalized", "resolve%2Fmain%2Fassets%2Fabs.png", true},
		{"external image kept", "img.shields.io", true},
		{"lazy loading added", "loading=\"lazy\"", true},
		{"link resolved to blob", "blob/main/docs/guide.md", true},
		{"script sanitized away", "<script", false},
	}
	for _, c := range cases {
		if got := strings.Contains(html, c.sub); got != c.present {
			t.Errorf("%s: contains(%q)=%v want %v\n--- HTML ---\n%s", c.name, c.sub, got, c.present, html)
		}
	}
}

func TestClassifyReadmeRepoURL(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		endpoint    string
		wantOK      bool
		wantRepo    string
		wantDataset bool
		wantRev     string
	}{
		{name: "model root", in: "https://huggingface.co/owner/model", endpoint: "https://huggingface.co", wantOK: true, wantRepo: "owner/model"},
		{name: "model trailing slash", in: "https://huggingface.co/owner/model/", endpoint: "https://huggingface.co", wantOK: true, wantRepo: "owner/model"},
		{name: "dataset root", in: "https://huggingface.co/datasets/owner/data", endpoint: "https://huggingface.co", wantOK: true, wantRepo: "owner/data", wantDataset: true},
		{name: "tree revision", in: "https://huggingface.co/owner/model/tree/dev", endpoint: "https://huggingface.co", wantOK: true, wantRepo: "owner/model", wantRev: "dev"},
		{name: "raw tree subpath is external", in: "https://huggingface.co/owner/model/tree/main/folder/file.md", endpoint: "https://huggingface.co"},
		{name: "raw dataset tree subpath is external", in: "https://huggingface.co/datasets/owner/data/tree/v2/train/part.parquet", endpoint: "https://huggingface.co"},
		{name: "raw multi-segment revision is external", in: "https://huggingface.co/owner/model/tree/refs/pr/1", endpoint: "https://huggingface.co"},
		{name: "dataset owner matching reserved route", in: "https://huggingface.co/datasets/organizations/huggingface", endpoint: "https://huggingface.co", wantOK: true, wantRepo: "organizations/huggingface", wantDataset: true},
		{name: "encoded revision slash", in: "https://huggingface.co/owner/model/tree/refs%2Fpr%2F1", endpoint: "https://huggingface.co", wantOK: true, wantRepo: "owner/model", wantRev: "refs/pr/1"},
		{name: "query ignored", in: "https://huggingface.co/owner/model?library=true", endpoint: "https://huggingface.co", wantOK: true, wantRepo: "owner/model"},
		{name: "explicit https default port", in: "https://huggingface.co:443/owner/model", endpoint: "https://huggingface.co", wantOK: true, wantRepo: "owner/model"},
		{name: "explicit endpoint https default port", in: "https://huggingface.co/owner/model", endpoint: "https://huggingface.co:443", wantOK: true, wantRepo: "owner/model"},
		{name: "explicit http default port", in: "http://hf.example.test:80/owner/model", endpoint: "http://hf.example.test:80", wantOK: true, wantRepo: "owner/model"},
		{name: "custom endpoint", in: "https://hf.example.test/owner/model/tree/dev", endpoint: "https://hf.example.test", wantOK: true, wantRepo: "owner/model", wantRev: "dev"},
		{name: "endpoint path prefix model root", in: "https://mirror.example/hf/owner/model", endpoint: "https://mirror.example/hf", wantOK: true, wantRepo: "owner/model"},
		{name: "endpoint path prefix tree revision", in: "https://mirror.example/hf/owner/model/tree/dev", endpoint: "https://mirror.example/hf", wantOK: true, wantRepo: "owner/model", wantRev: "dev"},
		{name: "endpoint path prefix dataset tree revision", in: "https://mirror.example/hf/datasets/owner/data/tree/v1", endpoint: "https://mirror.example/hf", wantOK: true, wantRepo: "owner/data", wantDataset: true, wantRev: "v1"},
		{name: "same host without endpoint prefix", in: "https://mirror.example/owner/model", endpoint: "https://mirror.example/hf"},
		{name: "endpoint prefix boundary", in: "https://mirror.example/hfold/owner/model", endpoint: "https://mirror.example/hf"},
		{name: "other host", in: "https://huggingface.co/owner/model", endpoint: "https://hf.example.test"},
		{name: "blob file URL", in: "https://huggingface.co/owner/model/blob/main/config.json", endpoint: "https://huggingface.co"},
		{name: "resolve file URL", in: "https://huggingface.co/owner/model/resolve/main/model.gguf", endpoint: "https://huggingface.co"},
		{name: "raw file URL", in: "https://huggingface.co/owner/model/raw/main/config.json", endpoint: "https://huggingface.co"},
		{name: "dataset blob file URL", in: "https://huggingface.co/datasets/owner/data/blob/v2/train.csv", endpoint: "https://huggingface.co"},
		{name: "dataset resolve file URL", in: "https://huggingface.co/datasets/owner/data/resolve/v2/train.csv", endpoint: "https://huggingface.co"},
		{name: "papers page", in: "https://huggingface.co/papers/2401.04088", endpoint: "https://huggingface.co"},
		{name: "hardware page", in: "https://huggingface.co/hardware/a10", endpoint: "https://huggingface.co"},
		{name: "inference page", in: "https://huggingface.co/inference/models", endpoint: "https://huggingface.co"},
		{name: "solutions page", in: "https://huggingface.co/solutions/models", endpoint: "https://huggingface.co"},
		{name: "chat page", in: "https://huggingface.co/chat/assistants", endpoint: "https://huggingface.co"},
		{name: "dataset reserved-looking owner", in: "https://huggingface.co/datasets/hardware/a10", endpoint: "https://huggingface.co", wantOK: true, wantRepo: "hardware/a10", wantDataset: true},
		{name: "organizations page", in: "https://huggingface.co/organizations/huggingface", endpoint: "https://huggingface.co"},
		{name: "docs page", in: "https://huggingface.co/docs/transformers", endpoint: "https://huggingface.co"},
		{name: "tasks page", in: "https://huggingface.co/tasks/text-generation", endpoint: "https://huggingface.co"},
		{name: "blog page", in: "https://huggingface.co/blog/foo", endpoint: "https://huggingface.co"},
		{name: "settings page", in: "https://huggingface.co/settings/profile", endpoint: "https://huggingface.co"},
		{name: "search feature page", in: "https://huggingface.co/search/full-text", endpoint: "https://huggingface.co"},
		{name: "learn feature page", in: "https://huggingface.co/learn/course", endpoint: "https://huggingface.co"},
		{name: "api feature page", in: "https://huggingface.co/api/models", endpoint: "https://huggingface.co"},
		{name: "profile page", in: "https://huggingface.co/owner", endpoint: "https://huggingface.co"},
		{name: "discussions page", in: "https://huggingface.co/owner/model/discussions", endpoint: "https://huggingface.co"},
		{name: "missing revision", in: "https://huggingface.co/owner/model/tree", endpoint: "https://huggingface.co"},
		{name: "unknown route", in: "https://huggingface.co/owner/model/settings", endpoint: "https://huggingface.co"},
		{name: "non-http scheme", in: "mailto:owner@example.com", endpoint: "https://huggingface.co"},
		{name: "external host", in: "https://example.com/owner/model", endpoint: "https://huggingface.co"},
		{name: "non-default port URL", in: "https://huggingface.co:8443/owner/model", endpoint: "https://huggingface.co"},
		{name: "non-default endpoint port", in: "https://huggingface.co/owner/model", endpoint: "https://huggingface.co:8443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, isDataset, revision, ok := classifyReadmeRepoURL(tt.in, tt.endpoint)
			if ok != tt.wantOK || repo != tt.wantRepo || isDataset != tt.wantDataset || revision != tt.wantRev {
				t.Fatalf("classifyReadmeRepoURL(%q, %q) = (%q, %v, %q, %v), want (%q, %v, %q, %v)",
					tt.in, tt.endpoint, repo, isDataset, revision, ok, tt.wantRepo, tt.wantDataset, tt.wantRev, tt.wantOK)
			}
		})
	}
}

func TestRenderReadmeHTML_RepositoryLinks(t *testing.T) {
	tests := []struct {
		name         string
		markdown     string
		baseRaw      string
		baseBlob     string
		endpointBase string
		contains     []string
		absent       []string
	}{
		{
			name:         "model repository",
			markdown:     "[model](https://huggingface.co/owner/model)\n",
			baseRaw:      "https://huggingface.co/owner/model/resolve/main/",
			baseBlob:     "https://huggingface.co/owner/model/blob/main/",
			endpointBase: "https://huggingface.co",
			contains: []string{
				`href="https://huggingface.co/owner/model"`,
				`data-hf-repo="owner/model"`,
				`data-hf-repo-type="model"`,
				`target="_blank"`,
			},
			absent: []string{"data-hf-repo-revision"},
		},
		{
			name:         "dataset repository",
			markdown:     "[data](https://huggingface.co/datasets/owner/data)\n",
			baseRaw:      "https://huggingface.co/datasets/owner/data/resolve/main/",
			baseBlob:     "https://huggingface.co/datasets/owner/data/blob/main/",
			endpointBase: "https://huggingface.co",
			contains: []string{
				`href="https://huggingface.co/datasets/owner/data"`,
				`data-hf-repo="owner/data"`,
				`data-hf-repo-type="dataset"`,
			},
		},
		{
			name:         "revision-bearing tree link",
			markdown:     "[dev](https://huggingface.co/owner/model/tree/dev)\n",
			baseRaw:      "https://huggingface.co/owner/model/resolve/main/",
			baseBlob:     "https://huggingface.co/owner/model/blob/main/",
			endpointBase: "https://huggingface.co",
			contains: []string{
				`href="https://huggingface.co/owner/model/tree/dev"`,
				`data-hf-repo="owner/model"`,
				`data-hf-repo-type="model"`,
				`data-hf-repo-revision="dev"`,
			},
		},
		{
			name:         "dataset tree link preserves revision and type",
			markdown:     "[v1](https://huggingface.co/datasets/owner/data/tree/v1)\n",
			baseRaw:      "https://huggingface.co/datasets/owner/data/resolve/main/",
			baseBlob:     "https://huggingface.co/datasets/owner/data/blob/main/",
			endpointBase: "https://huggingface.co",
			contains: []string{
				`href="https://huggingface.co/datasets/owner/data/tree/v1"`,
				`data-hf-repo="owner/data"`,
				`data-hf-repo-type="dataset"`,
				`data-hf-repo-revision="v1"`,
			},
		},
		{
			name:         "tree subpath stays external",
			markdown:     "[nested](https://huggingface.co/owner/model/tree/main/folder/file.md)\n",
			baseRaw:      "https://huggingface.co/owner/model/resolve/main/",
			baseBlob:     "https://huggingface.co/owner/model/blob/main/",
			endpointBase: "https://huggingface.co",
			contains:     []string{`href="https://huggingface.co/owner/model/tree/main/folder/file.md"`},
			absent:       []string{"data-hf-repo"},
		},
		{
			name:         "dataset tree subpath stays external",
			markdown:     "[train](https://huggingface.co/datasets/owner/data/tree/v1/train)\n",
			baseRaw:      "https://huggingface.co/datasets/owner/data/resolve/main/",
			baseBlob:     "https://huggingface.co/datasets/owner/data/blob/main/",
			endpointBase: "https://huggingface.co",
			contains:     []string{`href="https://huggingface.co/datasets/owner/data/tree/v1/train"`},
			absent:       []string{"data-hf-repo"},
		},
		{
			name:         "raw multi-segment revision stays external",
			markdown:     "[pr](https://huggingface.co/owner/model/tree/refs/pr/1)\n",
			baseRaw:      "https://huggingface.co/owner/model/resolve/main/",
			baseBlob:     "https://huggingface.co/owner/model/blob/main/",
			endpointBase: "https://huggingface.co",
			contains:     []string{`href="https://huggingface.co/owner/model/tree/refs/pr/1"`},
			absent:       []string{"data-hf-repo"},
		},
		{
			name:         "encoded revision slash stays internal",
			markdown:     "[pr](https://huggingface.co/owner/model/tree/refs%2Fpr%2F1)\n",
			baseRaw:      "https://huggingface.co/owner/model/resolve/main/",
			baseBlob:     "https://huggingface.co/owner/model/blob/main/",
			endpointBase: "https://huggingface.co",
			contains: []string{
				`href="https://huggingface.co/owner/model/tree/refs%2Fpr%2F1"`,
				`data-hf-repo="owner/model"`,
				`data-hf-repo-type="model"`,
				`data-hf-repo-revision="refs/pr/1"`,
			},
		},
		{
			name:         "blob file URL stays external",
			markdown:     "[config](https://huggingface.co/owner/model/blob/main/config.json)\n",
			baseRaw:      "https://huggingface.co/owner/model/resolve/main/",
			baseBlob:     "https://huggingface.co/owner/model/blob/main/",
			endpointBase: "https://huggingface.co",
			contains:     []string{`href="https://huggingface.co/owner/model/blob/main/config.json"`},
			absent:       []string{"data-hf-repo"},
		},
		{
			name:         "resolve file URL stays external",
			markdown:     "[weights](https://huggingface.co/owner/model/resolve/main/model.gguf)\n",
			baseRaw:      "https://huggingface.co/owner/model/resolve/main/",
			baseBlob:     "https://huggingface.co/owner/model/blob/main/",
			endpointBase: "https://huggingface.co",
			contains:     []string{`href="https://huggingface.co/owner/model/resolve/main/model.gguf"`},
			absent:       []string{"data-hf-repo"},
		},
		{
			name:         "raw file URL stays external",
			markdown:     "[raw](https://huggingface.co/owner/model/raw/main/config.json)\n",
			baseRaw:      "https://huggingface.co/owner/model/resolve/main/",
			baseBlob:     "https://huggingface.co/owner/model/blob/main/",
			endpointBase: "https://huggingface.co",
			contains:     []string{`href="https://huggingface.co/owner/model/raw/main/config.json"`},
			absent:       []string{"data-hf-repo"},
		},
		{
			name:         "relative config link stays external",
			markdown:     "[file](config.json)\n",
			baseRaw:      "https://huggingface.co/owner/model/resolve/main/",
			baseBlob:     "https://huggingface.co/owner/model/blob/main/",
			endpointBase: "https://huggingface.co",
			contains:     []string{`href="https://huggingface.co/owner/model/blob/main/config.json"`},
			absent:       []string{"data-hf-repo"},
		},
		{
			name:         "relative parent link resolves to a file and stays external",
			markdown:     "[up](../LICENSE)\n",
			baseRaw:      "https://huggingface.co/owner/model/resolve/main/",
			baseBlob:     "https://huggingface.co/owner/model/blob/main/",
			endpointBase: "https://huggingface.co",
			contains:     []string{`href="https://huggingface.co/owner/model/blob/LICENSE"`},
			absent:       []string{"data-hf-repo"},
		},
		{
			name:         "relative link resolves on custom endpoint",
			markdown:     "[sibling](/owner/other)\n",
			baseRaw:      "https://hf.example.test/owner/model/resolve/main/",
			baseBlob:     "https://hf.example.test/owner/model/blob/main/",
			endpointBase: "https://hf.example.test",
			contains: []string{
				`href="https://hf.example.test/owner/other"`,
				`data-hf-repo="owner/other"`,
				`data-hf-repo-type="model"`,
			},
		},
		{
			name:         "huggingface link is external on custom endpoint",
			markdown:     "[hf](https://huggingface.co/owner/model)\n",
			baseRaw:      "https://hf.example.test/owner/model/resolve/main/",
			baseBlob:     "https://hf.example.test/owner/model/blob/main/",
			endpointBase: "https://hf.example.test",
			contains:     []string{`href="https://huggingface.co/owner/model"`},
			absent:       []string{"data-hf-repo"},
		},
		{
			name:         "path-prefixed endpoint repository is internal",
			markdown:     "[model](https://mirror.example/hf/owner/model)\n",
			baseRaw:      "https://mirror.example/hf/owner/model/resolve/main/",
			baseBlob:     "https://mirror.example/hf/owner/model/blob/main/",
			endpointBase: "https://mirror.example/hf",
			contains: []string{
				`href="https://mirror.example/hf/owner/model"`,
				`data-hf-repo="owner/model"`,
				`data-hf-repo-type="model"`,
			},
		},
		{
			name:         "path-prefixed endpoint dataset tree revision is internal",
			markdown:     "[v1](https://mirror.example/hf/datasets/owner/data/tree/v1)\n",
			baseRaw:      "https://mirror.example/hf/datasets/owner/data/resolve/main/",
			baseBlob:     "https://mirror.example/hf/datasets/owner/data/blob/main/",
			endpointBase: "https://mirror.example/hf",
			contains: []string{
				`href="https://mirror.example/hf/datasets/owner/data/tree/v1"`,
				`data-hf-repo="owner/data"`,
				`data-hf-repo-type="dataset"`,
				`data-hf-repo-revision="v1"`,
			},
		},
		{
			name:         "same host without endpoint prefix stays external",
			markdown:     "[model](https://mirror.example/owner/model)\n",
			baseRaw:      "https://mirror.example/hf/owner/model/resolve/main/",
			baseBlob:     "https://mirror.example/hf/owner/model/blob/main/",
			endpointBase: "https://mirror.example/hf",
			contains:     []string{`href="https://mirror.example/owner/model"`},
			absent:       []string{"data-hf-repo"},
		},
		{
			name:         "path prefix boundary stays external",
			markdown:     "[model](https://mirror.example/hfold/owner/model)\n",
			baseRaw:      "https://mirror.example/hf/owner/model/resolve/main/",
			baseBlob:     "https://mirror.example/hf/owner/model/blob/main/",
			endpointBase: "https://mirror.example/hf",
			contains:     []string{`href="https://mirror.example/hfold/owner/model"`},
			absent:       []string{"data-hf-repo"},
		},
		{
			name:         "reserved hub pages are external",
			markdown:     "[papers](https://huggingface.co/papers/2401.04088) [orgs](https://huggingface.co/organizations/huggingface) [docs](https://huggingface.co/docs/transformers) [tasks](https://huggingface.co/tasks/text-generation) [blog](https://huggingface.co/blog/foo) [settings](https://huggingface.co/settings/profile) [hardware](https://huggingface.co/hardware/a10) [inference](https://huggingface.co/inference/models) [solutions](https://huggingface.co/solutions/models) [chat](https://huggingface.co/chat/assistants)\n",
			baseRaw:      "https://huggingface.co/owner/model/resolve/main/",
			baseBlob:     "https://huggingface.co/owner/model/blob/main/",
			endpointBase: "https://huggingface.co",
			contains: []string{
				`href="https://huggingface.co/papers/2401.04088"`,
				`href="https://huggingface.co/organizations/huggingface"`,
				`href="https://huggingface.co/docs/transformers"`,
				`href="https://huggingface.co/tasks/text-generation"`,
				`href="https://huggingface.co/blog/foo"`,
				`href="https://huggingface.co/settings/profile"`,
				`href="https://huggingface.co/hardware/a10"`,
				`href="https://huggingface.co/inference/models"`,
				`href="https://huggingface.co/solutions/models"`,
				`href="https://huggingface.co/chat/assistants"`,
			},
			absent: []string{"data-hf-repo"},
		},
		{
			name:         "profile and discussions are external",
			markdown:     "[profile](https://huggingface.co/owner) [discussions](https://huggingface.co/owner/model/discussions)\n",
			baseRaw:      "https://huggingface.co/owner/model/resolve/main/",
			baseBlob:     "https://huggingface.co/owner/model/blob/main/",
			endpointBase: "https://huggingface.co",
			contains: []string{
				`href="https://huggingface.co/owner"`,
				`href="https://huggingface.co/owner/model/discussions"`,
			},
			absent: []string{"data-hf-repo"},
		},
		{
			name:         "explicit default port is internal",
			markdown:     "[model](https://huggingface.co:443/owner/model)\n",
			baseRaw:      "https://huggingface.co/owner/model/resolve/main/",
			baseBlob:     "https://huggingface.co/owner/model/blob/main/",
			endpointBase: "https://huggingface.co",
			contains: []string{
				`href="https://huggingface.co:443/owner/model"`,
				`data-hf-repo="owner/model"`,
				`data-hf-repo-type="model"`,
			},
		},
		{
			name:         "external host is unchanged",
			markdown:     "[external](https://example.com/owner/model)\n",
			baseRaw:      "https://huggingface.co/owner/model/resolve/main/",
			baseBlob:     "https://huggingface.co/owner/model/blob/main/",
			endpointBase: "https://huggingface.co",
			contains:     []string{`href="https://example.com/owner/model"`},
			absent:       []string{"data-hf-repo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			html := renderReadmeHTML(tt.markdown, tt.baseRaw, tt.baseBlob, tt.endpointBase)
			for _, sub := range tt.contains {
				if !strings.Contains(html, sub) {
					t.Errorf("missing %q in:\n%s", sub, html)
				}
			}
			for _, sub := range tt.absent {
				if strings.Contains(html, sub) {
					t.Errorf("unexpected %q in:\n%s", sub, html)
				}
			}
		})
	}
}

func TestHandleReadme_CustomEndpointRepositoryLinks(t *testing.T) {
	var endpoint string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/owner/model/raw/main/README.md" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "[sibling]("+endpoint+"/owner/other)\n\n"+
			"[external](https://huggingface.co/owner/model)\n\n"+
			"[weights]("+endpoint+"/owner/model/resolve/main/model.gguf)\n")
	}))
	defer upstream.Close()
	endpoint = upstream.URL

	srv := newTestServerWithConfig(t, Config{Endpoint: endpoint})
	req := httptest.NewRequest(http.MethodGet, "/api/readme/owner/model", nil)
	req.SetPathValue("repo", "owner/model")
	w := httptest.NewRecorder()
	srv.handleReadme(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var got struct {
		HTML        string `json:"html"`
		BaseBlobURL string `json:"baseBlobURL"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if want := endpoint + "/owner/model/blob/main/"; got.BaseBlobURL != want {
		t.Errorf("baseBlobURL = %q, want %q", got.BaseBlobURL, want)
	}
	for _, sub := range []string{
		`href="` + endpoint + `/owner/other"`,
		`data-hf-repo="owner/other"`,
		`data-hf-repo-type="model"`,
	} {
		if !strings.Contains(got.HTML, sub) {
			t.Errorf("missing %q in:\n%s", sub, got.HTML)
		}
	}
	if count := strings.Count(got.HTML, "data-hf-repo="); count != 1 {
		// The external huggingface.co link and the endpoint file URL must not
		// acquire internal attributes merely because the custom endpoint serves
		// the current README.
		t.Errorf("internal repo attribute count = %d, want only the endpoint-relative link:\n%s", count, got.HTML)
	}
	for _, sub := range []string{
		`href="https://huggingface.co/owner/model"`,
		`href="` + endpoint + `/owner/model/resolve/main/model.gguf"`,
	} {
		if !strings.Contains(got.HTML, sub) {
			t.Errorf("missing external fallback href %q:\n%s", sub, got.HTML)
		}
	}
}

func TestHandleReadme_PathPrefixedEndpointRepositoryLinks(t *testing.T) {
	var upstreamURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hf/owner/model/raw/main/README.md" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "[model]("+upstreamURL+"/hf/owner/model)\n\n"+
			"[dataset]("+upstreamURL+"/hf/datasets/owner/data/tree/v1)\n\n"+
			"[nested]("+upstreamURL+"/hf/owner/model/tree/dev/folder/file.md)\n\n"+
			"[same-host]("+upstreamURL+"/owner/model)\n")
	}))
	defer upstream.Close()
	upstreamURL = upstream.URL
	endpoint := upstreamURL + "/hf"

	srv := newTestServerWithConfig(t, Config{Endpoint: endpoint})
	req := httptest.NewRequest(http.MethodGet, "/api/readme/owner/model", nil)
	req.SetPathValue("repo", "owner/model")
	w := httptest.NewRecorder()
	srv.handleReadme(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var got struct {
		HTML        string `json:"html"`
		BaseBlobURL string `json:"baseBlobURL"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if want := endpoint + "/owner/model/blob/main/"; got.BaseBlobURL != want {
		t.Errorf("baseBlobURL = %q, want %q", got.BaseBlobURL, want)
	}
	for _, sub := range []string{
		`href="` + endpoint + `/owner/model"`,
		`data-hf-repo="owner/model"`,
		`data-hf-repo-type="model"`,
		`href="` + endpoint + `/datasets/owner/data/tree/v1"`,
		`data-hf-repo="owner/data"`,
		`data-hf-repo-type="dataset"`,
		`data-hf-repo-revision="v1"`,
	} {
		if !strings.Contains(got.HTML, sub) {
			t.Errorf("missing %q in:\n%s", sub, got.HTML)
		}
	}
	if count := strings.Count(got.HTML, "data-hf-repo="); count != 2 {
		// The tree subpath and the same-host link without the endpoint prefix
		// must not acquire internal attributes.
		t.Errorf("internal repo attribute count = %d, want 2:\n%s", count, got.HTML)
	}
	for _, sub := range []string{
		`href="` + endpoint + `/owner/model/tree/dev/folder/file.md"`,
		`href="` + upstreamURL + `/owner/model"`,
	} {
		if !strings.Contains(got.HTML, sub) {
			t.Errorf("missing external fallback href %q:\n%s", sub, got.HTML)
		}
	}
}

func TestHandleReadmeEscapesRevision(t *testing.T) {
	var gotRequestURI, gotEscapedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestURI = r.RequestURI
		gotEscapedPath = r.URL.EscapedPath()
		if gotEscapedPath != "/owner/model/raw/refs%2Fpr%2F1/README.md" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "# refs/pr/1\n")
	}))
	defer upstream.Close()

	srv := newTestServerWithConfig(t, Config{Endpoint: upstream.URL})
	req := httptest.NewRequest(http.MethodGet, "/api/readme/owner/model?revision=refs%2Fpr%2F1", nil)
	req.SetPathValue("repo", "owner/model")
	w := httptest.NewRecorder()
	srv.handleReadme(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var got struct {
		Revision    string `json:"revision"`
		BaseRawURL  string `json:"baseRawURL"`
		BaseBlobURL string `json:"baseBlobURL"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Revision != "refs/pr/1" {
		t.Errorf("revision = %q, want the decoded value refs/pr/1", got.Revision)
	}
	if want := upstream.URL + "/owner/model/resolve/refs%2Fpr%2F1/"; got.BaseRawURL != want {
		t.Errorf("baseRawURL = %q, want %q", got.BaseRawURL, want)
	}
	if want := upstream.URL + "/owner/model/blob/refs%2Fpr%2F1/"; got.BaseBlobURL != want {
		t.Errorf("baseBlobURL = %q, want %q", got.BaseBlobURL, want)
	}
	if want := "/owner/model/raw/refs%2Fpr%2F1/README.md"; gotRequestURI != want || gotEscapedPath != want {
		t.Errorf("upstream request = %q (escaped path %q), want %q", gotRequestURI, gotEscapedPath, want)
	}
}

func TestHandleReadme_EmptySanitizedHTMLPresent(t *testing.T) {
	// A script-only README renders and sanitizes to empty HTML. Successful
	// responses must still include a string html field: the bundled client
	// renders only that field and treats a missing field as a contract error,
	// while an empty string is valid and leaves the section hidden.
	const markdown = "<script>alert(1)</script>"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/owner/model/raw/main/README.md" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, markdown)
	}))
	defer upstream.Close()

	srv := newTestServerWithConfig(t, Config{Endpoint: upstream.URL})
	req := httptest.NewRequest(http.MethodGet, "/api/readme/owner/model", nil)
	req.SetPathValue("repo", "owner/model")
	w := httptest.NewRecorder()
	srv.handleReadme(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Pointers distinguish missing/null fields from present empty strings.
	var got struct {
		HTML        *string `json:"html"`
		Markdown    *string `json:"markdown"`
		BaseRawURL  *string `json:"baseRawURL"`
		BaseBlobURL *string `json:"baseBlobURL"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.HTML == nil {
		t.Fatalf("html field missing or null on success; the client requires a string html: %s", w.Body.String())
	}
	if *got.HTML != "" {
		t.Fatalf("html = %q, want the empty sanitized output that must remain accepted", *got.HTML)
	}
	if got.Markdown == nil || *got.Markdown != markdown {
		t.Errorf("markdown = %v, want the raw README %q", got.Markdown, markdown)
	}
	if got.BaseRawURL == nil || *got.BaseRawURL != upstream.URL+"/owner/model/resolve/main/" {
		t.Errorf("baseRawURL = %v, want %q", got.BaseRawURL, upstream.URL+"/owner/model/resolve/main/")
	}
	if got.BaseBlobURL == nil || *got.BaseBlobURL != upstream.URL+"/owner/model/blob/main/" {
		t.Errorf("baseBlobURL = %v, want %q", got.BaseBlobURL, upstream.URL+"/owner/model/blob/main/")
	}
}

func TestRenderReadmeHTML_InternalLinkAttrsAreRewriterOwned(t *testing.T) {
	md := "<a href=\"https://huggingface.co/owner/model/tree/dev\" data-hf-repo=\"forged/model\" data-hf-repo-type=\"dataset\" data-hf-repo-revision=\"forged\" onclick=\"alert(1)\">model</a>\n" +
		"<a href=\"https://example.com/evil\" data-hf-repo=\"forged/model\" data-hf-repo-type=\"dataset\">external</a>\n" +
		"<script>alert(1)</script>\n"
	html := renderReadmeHTML(md,
		"https://huggingface.co/owner/model/resolve/main/",
		"https://huggingface.co/owner/model/blob/main/",
		"https://huggingface.co")

	for _, sub := range []string{
		`href="https://huggingface.co/owner/model/tree/dev"`,
		`data-hf-repo="owner/model"`,
		`data-hf-repo-type="model"`,
		`data-hf-repo-revision="dev"`,
		`href="https://example.com/evil"`,
	} {
		if !strings.Contains(html, sub) {
			t.Errorf("missing %q in:\n%s", sub, html)
		}
	}
	for _, sub := range []string{"forged", "onclick", "<script"} {
		if strings.Contains(html, sub) {
			t.Errorf("unexpected %q in:\n%s", sub, html)
		}
	}
}

func TestReadmeFallbackPolicyStripsInternalLinkAttrs(t *testing.T) {
	html := readmeFallbackPolicy.Sanitize(`<a href="https://example.com/" data-hf-repo="forged/model" data-hf-repo-type="dataset" data-hf-repo-revision="forged">x</a>`)
	if strings.Contains(html, "data-hf-repo") {
		t.Errorf("fallback policy preserved untrusted internal-link attributes:\n%s", html)
	}
	if !strings.Contains(html, `href="https://example.com/"`) {
		t.Errorf("fallback policy removed the external href:\n%s", html)
	}
}

func TestNormalizeReadmeImageURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "model raw becomes resolve",
			in:   "https://huggingface.co/owner/model/raw/main/assets/logo.png",
			want: "https://huggingface.co/owner/model/resolve/main/assets/logo.png",
		},
		{
			name: "dataset raw becomes resolve",
			in:   "https://huggingface.co/datasets/owner/data/raw/main/assets/plot.png",
			want: "https://huggingface.co/datasets/owner/data/resolve/main/assets/plot.png",
		},
		{
			name: "external url unchanged",
			in:   "https://example.com/raw/main/logo.png",
			want: "https://example.com/raw/main/logo.png",
		},
		{
			name: "already resolve unchanged",
			in:   "https://huggingface.co/owner/model/resolve/main/assets/logo.png",
			want: "https://huggingface.co/owner/model/resolve/main/assets/logo.png",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeReadmeImageURL(tt.in, "huggingface.co"); got != tt.want {
				t.Fatalf("normalizeReadmeImageURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestHandleReadmeAsset_NormalizesSameHostRawToResolve(t *testing.T) {
	gotPath := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png-bytes"))
	}))
	defer upstream.Close()

	srv := newTestServerWithConfig(t, Config{Endpoint: upstream.URL})
	assetURL := upstream.URL + "/owner/model/raw/main/assets/logo.png"
	req := httptest.NewRequest(http.MethodGet, readmeAssetPath+"?url="+url.QueryEscape(assetURL), nil)
	w := httptest.NewRecorder()

	srv.handleReadmeAsset(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if path := <-gotPath; path != "/owner/model/resolve/main/assets/logo.png" {
		t.Fatalf("upstream path = %q, want %q", path, "/owner/model/resolve/main/assets/logo.png")
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q, want %q", ct, "image/png")
	}
	body, err := io.ReadAll(w.Result().Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "png-bytes" {
		t.Fatalf("body = %q, want %q", string(body), "png-bytes")
	}
}

func TestReadmeHTML_StylesPreserved(t *testing.T) {
	// This README fragment mimics the kind of inline-styled HTML cards common in
	// Hugging Face model cards. All style properties used here are in the
	// allowed AllowStyles set.
	md := "<div style=\"font-family: sans-serif; color: #333; padding: 24px; border: 1px solid #ccc; border-radius: 12px; background-color: #f9f9f9; background-image: linear-gradient(180deg, #f9f9f9 0%, #e5e7eb 100%); box-shadow: 0 2px 8px rgba(0,0,0,0.1);\">\n" +
		"  <span style=\"font-weight: 800; font-size: 20px; text-transform: uppercase; letter-spacing: 1px;\">Hello</span>\n" +
		"  <div style=\"display: flex; gap: 12px 1rem; align-items: center; background: linear-gradient(135deg, #16a34a 0%, #047857 100%);\">\n" +
		"    <span style=\"font-size: 14px; line-height: 1.6;\">text</span>\n" +
		"  </div>\n" +
		"  <script>alert('xss')</script>\n" +
		"</div>\n"
	html := renderReadmeHTML(md,
		"https://huggingface.co/owner/model/resolve/main/",
		"https://huggingface.co/owner/model/blob/main/",
		"https://huggingface.co")

	cases := []struct {
		name    string
		sub     string
		present bool
	}{
		{"font-family preserved", "font-family: sans-serif", true},
		{"color preserved", "color: #", true},
		{"padding preserved", "padding: 24px", true},
		{"border preserved", "border: 1px solid #ccc", true},
		{"border-radius preserved", "border-radius: 12px", true},
		{"background-color preserved", "background-color:", true},
		{"background-image gradient preserved", "background-image: linear-gradient(180deg, #f9f9f9 0%, #e5e7eb 100%)", true},
		{"background gradient preserved", "background: linear-gradient(135deg, #16a34a 0%, #047857 100%)", true},
		{"box-shadow preserved", "box-shadow:", true},
		{"font-weight preserved", "font-weight: 800", true},
		{"font-size preserved", "font-size: 20px", true},
		{"text-transform preserved", "text-transform: uppercase", true},
		{"letter-spacing preserved", "letter-spacing:", true},
		{"display flex preserved", "display: flex", true},
		{"gap shorthand preserved", "gap: 12px 1rem", true},
		{"align-items preserved", "align-items: center", true},
		{"line-height preserved", "line-height:", true},
		{"script still sanitized", "<script", false},
	}
	for _, c := range cases {
		if got := strings.Contains(html, c.sub); got != c.present {
			t.Errorf("%s: contains(%q)=%v want %v\n--- HTML ---\n%s", c.name, c.sub, got, c.present, html)
		}
	}
}

func TestReadmeHTML_BackgroundURLSafety(t *testing.T) {
	md := "<div style=\"background-image: linear-gradient(135deg, #16a34a 0%, #047857 100%); background: url(https://example.com/bg.png) no-repeat center; color: white;\">Banner</div>\n"
	html := renderReadmeHTML(md,
		"https://huggingface.co/owner/model/resolve/main/",
		"https://huggingface.co/owner/model/blob/main/",
		"https://huggingface.co")

	if !strings.Contains(html, "background-image: linear-gradient(135deg, #16a34a 0%, #047857 100%)") {
		t.Errorf("safe gradient background-image should be preserved, but missing from: %s", html)
	}
	if strings.Contains(html, "background: url(https://example.com/bg.png) no-repeat center") {
		t.Errorf("URL-based background shorthand should be stripped, but found in: %s", html)
	}
	if !strings.Contains(html, "color: white") {
		t.Errorf("color should be preserved alongside background values, but missing from: %s", html)
	}
}

func TestReadmeHTML_DisallowedStyleStripped(t *testing.T) {
	// position, z-index, and invalid gap shorthand are NOT in the allowed style
	// list — they should be stripped by the sanitizer.
	md := "<div style=\"gap: 1rem 2rem 3rem; position: absolute; z-index: 999; color: red;\">content</div>\n"
	html := renderReadmeHTML(md,
		"https://huggingface.co/owner/model/resolve/main/",
		"https://huggingface.co/owner/model/blob/main/",
		"https://huggingface.co")

	if strings.Contains(html, "gap:") {
		t.Errorf("invalid gap shorthand should be stripped, but found in: %s", html)
	}
	if strings.Contains(html, "position:") {
		t.Errorf("position should be stripped, but found in: %s", html)
	}
	if strings.Contains(html, "z-index:") {
		t.Errorf("z-index should be stripped, but found in: %s", html)
	}
	if !strings.Contains(html, "color: red") {
		t.Errorf("color should still be preserved, but missing from: %s", html)
	}
}

func TestReadmeGapHandler(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "single length", in: "12px", want: true},
		{name: "two lengths", in: "12px 1rem", want: true},
		{name: "normal keyword", in: "normal", want: true},
		{name: "mixed normal and length", in: "normal 1rem", want: true},
		{name: "initial keyword", in: "initial", want: true},
		{name: "unitless length rejected", in: "12", want: false},
		{name: "three values rejected", in: "12px 1rem 2rem", want: false},
		{name: "negative rejected", in: "-1px", want: false},
		{name: "unsafe function rejected", in: "calc(10px + 1rem)", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readmeGapHandler(tt.in); got != tt.want {
				t.Fatalf("readmeGapHandler(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestReadmeBackgroundHandler(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "solid color", in: "#f9f9f9", want: true},
		{name: "gradient", in: "linear-gradient(135deg, #16a34a 0%, #047857 100%)", want: true},
		{name: "repeating gradient", in: "repeating-linear-gradient(90deg, #fff 0, #fff 8px, #eee 8px, #eee 16px)", want: true},
		{name: "transparent", in: "transparent", want: true},
		{name: "none", in: "none", want: true},
		{name: "javascript url rejected", in: "url(javascript:alert(1))", want: false},
		{name: "https url rejected", in: "url(https://example.com/bg.png)", want: false},
		{name: "url in shorthand rejected", in: "url(https://example.com/bg.png) no-repeat center", want: false},
		{name: "non-gradient image rejected", in: "cover", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readmeBackgroundHandler(tt.in); got != tt.want {
				t.Fatalf("readmeBackgroundHandler(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestStripFrontmatter(t *testing.T) {
	if got := strings.TrimSpace(stripFrontmatter("---\na: b\n---\nhello")); got != "hello" {
		t.Errorf("got %q", got)
	}
	if got := stripFrontmatter("no front"); got != "no front" {
		t.Errorf("changed non-frontmatter: %q", got)
	}
}
