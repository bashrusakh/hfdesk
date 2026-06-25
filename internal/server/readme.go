// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	xhtml "golang.org/x/net/html"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// readmeAssetPath is the local endpoint that proxies a remote (HF-hosted) image
// so gated/private repo assets load with the server's token.
const readmeAssetPath = "/api/readme-asset"

// readmeMaxAsset caps how many bytes the asset proxy will stream for one image.
const readmeMaxAsset = 32 << 20 // 32 MiB

// readmeMarkdown renders GitHub-Flavored Markdown. Unsafe (raw HTML) output is
// enabled on purpose — many Hugging Face READMEs rely on inline HTML for
// centered logos and layout — and the result is always run through the
// bluemonday sanitizer below before it reaches the browser.
var readmeMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe()),
)

var readmePolicy = buildReadmePolicy()

// buildReadmePolicy returns the HTML sanitizer policy applied to rendered
// README content. It starts from bluemonday's user-generated-content policy and
// additionally permits tables, lazy-loaded images, collapsible <details>, and
// the relative asset-proxy URLs we inject during rewriting.
func buildReadmePolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("loading", "width", "height", "align", "title").OnElements("img")
	p.AllowAttrs("align").OnElements("div", "p", "span", "table", "h1", "h2", "h3", "h4", "h5", "h6")
	p.AllowElements("table", "thead", "tbody", "tfoot", "tr", "th", "td", "caption", "colgroup", "col", "details", "summary")
	p.AllowAttrs("colspan", "rowspan", "align", "valign", "scope").OnElements("td", "th")
	p.AllowAttrs("open").OnElements("details")
	// Image src may be a relative "/api/readme-asset?url=..." after rewriting.
	p.AllowRelativeURLs(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p
}

// stripFrontmatter removes a leading YAML frontmatter block (--- ... ---) that
// Hugging Face model cards put at the top of README.md.
func stripFrontmatter(md string) string {
	s := strings.TrimLeft(md, " \t\r\n")
	if !strings.HasPrefix(s, "---") {
		return md
	}
	rest := s[3:]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return md
	}
	after := rest[idx+len("\n---"):]
	if nl := strings.IndexByte(after, '\n'); nl >= 0 {
		return after[nl+1:]
	}
	return ""
}

// renderReadmeHTML converts README markdown into sanitized HTML ready to drop
// into the analysis panel. Relative image and link URLs are resolved against
// the repo's raw/blob bases; images hosted on the HF endpoint are routed
// through the local asset proxy (so gated repos load with the server token),
// while external images (badges, etc.) keep their original URL.
func renderReadmeHTML(markdown, baseRawURL, baseBlobURL, endpointHost string) string {
	md := stripFrontmatter(markdown)
	var buf bytes.Buffer
	if err := readmeMarkdown.Convert([]byte(md), &buf); err != nil {
		return ""
	}

	doc, err := xhtml.Parse(strings.NewReader("<html><body>" + buf.String() + "</body></html>"))
	if err != nil {
		return readmePolicy.Sanitize(buf.String())
	}
	body := findReadmeBody(doc)
	if body == nil {
		return readmePolicy.Sanitize(buf.String())
	}
	rewriteReadmeNodes(body, baseRawURL, baseBlobURL, endpointHost)

	var out bytes.Buffer
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := xhtml.Render(&out, c); err != nil {
			return readmePolicy.Sanitize(buf.String())
		}
	}
	return readmePolicy.Sanitize(out.String())
}

func findReadmeBody(n *xhtml.Node) *xhtml.Node {
	if n.Type == xhtml.ElementNode && n.Data == "body" {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if b := findReadmeBody(c); b != nil {
			return b
		}
	}
	return nil
}

func rewriteReadmeNodes(n *xhtml.Node, baseRaw, baseBlob, endpointHost string) {
	if n.Type == xhtml.ElementNode {
		switch n.Data {
		case "img":
			rewriteAttr(n, "src", func(v string) string {
				abs := resolveReadmeURL(v, baseRaw)
				if isHostURL(abs, endpointHost) {
					return readmeAssetPath + "?url=" + url.QueryEscape(abs)
				}
				return abs
			})
			setNodeAttr(n, "loading", "lazy")
		case "a":
			rewriteAttr(n, "href", func(v string) string {
				return resolveReadmeURL(v, baseBlob)
			})
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		rewriteReadmeNodes(c, baseRaw, baseBlob, endpointHost)
	}
}

// resolveReadmeURL turns a possibly-relative URL into an absolute one against
// base. Absolute, data:, mailto: and in-page anchors are returned unchanged.
func resolveReadmeURL(ref, base string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ref
	}
	low := strings.ToLower(ref)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") ||
		strings.HasPrefix(low, "data:") || strings.HasPrefix(low, "mailto:") ||
		strings.HasPrefix(ref, "#") {
		return ref
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(strings.TrimPrefix(ref, "./"))
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

func isHostURL(raw, host string) bool {
	if host == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

func getNodeAttr(n *xhtml.Node, key string) (string, int) {
	for i, a := range n.Attr {
		if a.Key == key {
			return a.Val, i
		}
	}
	return "", -1
}

func setNodeAttr(n *xhtml.Node, key, val string) {
	if _, i := getNodeAttr(n, key); i >= 0 {
		n.Attr[i].Val = val
		return
	}
	n.Attr = append(n.Attr, xhtml.Attribute{Key: key, Val: val})
}

func rewriteAttr(n *xhtml.Node, key string, fn func(string) string) {
	if v, i := getNodeAttr(n, key); i >= 0 {
		n.Attr[i].Val = fn(v)
	}
}

// readmeEndpointHost returns the host of the configured HF endpoint, defaulting
// to huggingface.co. Only images on this host are proxied.
func readmeEndpointHost(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "huggingface.co"
	}
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return "huggingface.co"
}

// handleReadmeAsset proxies a single HF-hosted image, adding the server's auth
// token so gated/private repo assets load. It only fetches URLs on the
// configured HF endpoint host to avoid being an open proxy (SSRF).
func (s *Server) handleReadmeAsset(w http.ResponseWriter, r *http.Request) {
	cfg := s.snapshotConfig()
	raw := r.URL.Query().Get("url")
	if raw == "" {
		writeError(w, http.StatusBadRequest, "Missing url", "")
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		writeError(w, http.StatusBadRequest, "Invalid url", "")
		return
	}
	if !strings.EqualFold(u.Host, readmeEndpointHost(cfg.Endpoint)) {
		writeError(w, http.StatusForbidden, "Asset host not allowed", "")
		return
	}

	client, err := hfdownloader.BuildHTTPClient(cfg.Proxy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Invalid proxy configuration", err.Error())
		return
	}
	client.Timeout = 30 * time.Second

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Bad request", err.Error())
		return
	}
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	req.Header.Set("User-Agent", "hfdesk/1")

	resp, err := client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Failed to fetch asset", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeError(w, resp.StatusCode, "Asset fetch failed", "")
		return
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	io.Copy(w, io.LimitReader(resp.Body, readmeMaxAsset))
}
