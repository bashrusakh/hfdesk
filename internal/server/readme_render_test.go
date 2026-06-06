// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
)

func TestRenderReadmeHTML(t *testing.T) {
	md := "---\nlicense: mit\n---\n\n# Title\n\n" +
		"![logo](assets/logo.png)\n\n" +
		"![badge](https://img.shields.io/badge/x-y.svg)\n\n" +
		"[docs](docs/guide.md)\n\n" +
		"| a | b |\n|---|---|\n| 1 | 2 |\n\n" +
		"<script>alert(1)</script>\n"
	html := renderReadmeHTML(md,
		"https://huggingface.co/owner/model/raw/main/",
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
		{"raw path encoded", "raw%2Fmain%2Fassets%2Flogo.png", true},
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

func TestStripFrontmatter(t *testing.T) {
	if got := strings.TrimSpace(stripFrontmatter("---\na: b\n---\nhello")); got != "hello" {
		t.Errorf("got %q", got)
	}
	if got := stripFrontmatter("no front"); got != "no front" {
		t.Errorf("changed non-frontmatter: %q", got)
	}
}
