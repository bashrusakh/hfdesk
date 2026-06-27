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
	gotPath := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
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
		"</div>\n"
	html := renderReadmeHTML(md,
		"https://huggingface.co/owner/model/resolve/main/",
		"https://huggingface.co/owner/model/blob/main/",
		"huggingface.co")

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
		"huggingface.co")

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
		"huggingface.co")

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
