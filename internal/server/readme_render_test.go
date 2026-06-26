// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
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
		"huggingface.co")

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
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png-bytes"))
	}))
	defer upstream.Close()

	srv := New(Config{Endpoint: upstream.URL})
	assetURL := upstream.URL + "/owner/model/raw/main/assets/logo.png"
	req := httptest.NewRequest(http.MethodGet, readmeAssetPath+"?url="+url.QueryEscape(assetURL), nil)
	w := httptest.NewRecorder()

	srv.handleReadmeAsset(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if gotPath != "/owner/model/resolve/main/assets/logo.png" {
		t.Fatalf("upstream path = %q, want %q", gotPath, "/owner/model/resolve/main/assets/logo.png")
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

func TestStripFrontmatter(t *testing.T) {
	if got := strings.TrimSpace(stripFrontmatter("---\na: b\n---\nhello")); got != "hello" {
		t.Errorf("got %q", got)
	}
	if got := stripFrontmatter("no front"); got != "no front" {
		t.Errorf("changed non-frontmatter: %q", got)
	}
}
