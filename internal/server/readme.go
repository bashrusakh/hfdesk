// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
	"github.com/microcosm-cc/bluemonday/css"
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

// readmePolicy is the HTML sanitizer policy applied to rendered README
// content after the repository-link rewriter has classified anchors.
// readmeFallbackPolicy is used only when the parse/rewrite path fails; it never
// trusts README-authored internal-link attributes.
var (
	readmePolicy         = buildReadmePolicy(true)
	readmeFallbackPolicy = buildReadmePolicy(false)
)

var readmeGapLength = regexp.MustCompile(`^(?:0(?:\.0+)?|[0-9]+(?:\.[0-9]+)?(?:px|em|rem|%|vh|vw|cm|mm|in|pt|pc|ex|ch|vmin|vmax))$`)
var readmeBackgroundValue = regexp.MustCompile(`^[a-zA-Z0-9 #()%,.\-_/]+$`)

// buildReadmePolicy returns the HTML sanitizer policy applied to rendered
// README content. It starts from bluemonday's user-generated-content policy and
// additionally permits tables, lazy-loaded images, collapsible <details>, the
// relative asset-proxy URLs we inject during rewriting, and a broad set of
// safe display-oriented CSS properties used by Hugging Face model cards.
// allowRepoLinks permits the data-hf-repo* attributes that rewriteReadmeNodes
// computes; the fallback policy leaves them disallowed.
func buildReadmePolicy(allowRepoLinks bool) *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("loading", "width", "height", "align", "title").OnElements("img")
	p.AllowAttrs("align").OnElements("div", "p", "span", "table", "h1", "h2", "h3", "h4", "h5", "h6")
	p.AllowElements("table", "thead", "tbody", "tfoot", "tr", "th", "td", "caption", "colgroup", "col", "details", "summary")
	p.AllowAttrs("colspan", "rowspan", "align", "valign", "scope").OnElements("td", "th")
	p.AllowAttrs("open").OnElements("details")
	if allowRepoLinks {
		// Internal repository links carry their classified HF identity so the
		// browser can route them through Model Details while href stays the
		// external fallback. These attributes are only trusted after the README
		// rewriter sets them; see rewriteReadmeNodes.
		p.AllowAttrs("data-hf-repo", "data-hf-repo-type", "data-hf-repo-revision").OnElements("a")
	}
	// Image src may be a relative "/api/readme-asset?url=..." after rewriting.
	p.AllowRelativeURLs(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)

	// Allow safe CSS properties commonly used in Hugging Face model cards
	// (border-box cards, flex/grid layouts, typography, etc.).
	// Each property uses bluemonday's built-in safe default handler unless
	// otherwise noted.
	p.AllowStyles(
		// Colors.
		"color", "background-color",

		// Typography
		"font-family", "font-size", "font-weight", "line-height",
		"letter-spacing", "text-align", "text-transform", "text-decoration",
		"white-space", "word-break", "word-spacing",

		// Sizing and spacing
		"width", "min-width", "max-width",
		"height", "min-height", "max-height",
		"padding", "padding-top", "padding-bottom", "padding-left", "padding-right",
		"margin", "margin-top", "margin-bottom", "margin-left", "margin-right",

		// Layout — safe display values (block, inline, flex, grid, etc.)
		"display", "flex-direction", "flex-wrap", "flex",
		"align-items", "align-self", "align-content",
		"justify-content",
		"grid-template-columns", "grid-template-rows", "grid-gap", "grid-column", "grid-row",
		"order",
		"float", "clear",
		"overflow", "overflow-x", "overflow-y",
		"vertical-align",

		// Borders and visual appearance
		"border", "border-collapse", "border-spacing",
		"border-top", "border-right", "border-bottom", "border-left",
		"border-color", "border-style", "border-width",
		"border-radius",
		"opacity",
		"cursor",
	).Globally()

	// The following properties need custom validation because bluemonday's
	// built-in handlers are missing or too restrictive for common HF model-card
	// values:
	//
	//   - gap:                no default handler in v1.0.27
	//   - box-shadow:          default handler splits by space and rejects rgba(...)
	//   - background/image:    allow gradients, reject url(...)
	cssDisplay := regexp.MustCompile(`^[a-zA-Z0-9 #()%,.\-]+$`)
	p.AllowStyles("gap").MatchingHandler(readmeGapHandler).Globally()
	p.AllowStyles("box-shadow").Matching(cssDisplay).Globally()
	p.AllowStyles("background", "background-image").MatchingHandler(readmeBackgroundHandler).Globally()

	return p
}

// readmeGapHandler allows the CSS gap shorthand with one or two length values
// (e.g. 12px or 12px 1rem) plus the common global keywords accepted by the
// other README style handlers.
func readmeGapHandler(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "normal", "initial", "inherit":
		return true
	}

	parts := strings.Fields(value)
	if len(parts) == 0 || len(parts) > 2 {
		return false
	}

	for _, part := range parts {
		if strings.EqualFold(part, "normal") {
			continue
		}
		if !readmeGapLength.MatchString(part) {
			return false
		}
	}
	return true
}

// readmeBackgroundHandler allows common HF card backgrounds such as solid
// colors and gradients, while rejecting URL-bearing values.
func readmeBackgroundHandler(value string) bool {
	v := strings.TrimSpace(value)
	if v == "" {
		return false
	}

	switch strings.ToLower(v) {
	case "normal", "initial", "inherit", "unset", "revert", "revert-layer", "none", "transparent", "currentcolor":
		return true
	}
	if strings.Contains(strings.ToLower(v), "url(") {
		return false
	}
	if css.ColorHandler(v) {
		return true
	}
	if strings.Contains(strings.ToLower(v), "gradient(") && readmeBackgroundValue.MatchString(v) {
		return true
	}
	return false
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
// the repo's asset/blob bases; images hosted on the HF endpoint are routed
// through the local asset proxy (so gated repos load with the server token),
// while external images (badges, etc.) keep their original URL. endpointBase is
// the configured endpoint URL (including any path prefix) so repository links
// can be recognized beneath that prefix.
func renderReadmeHTML(markdown, baseRawURL, baseBlobURL, endpointBase string) string {
	return renderMarkdownHTMLWithFallback(
		stripFrontmatter(markdown), baseRawURL, baseBlobURL, endpointBase,
		readmePolicy.Sanitize, readmeFallbackPolicy.Sanitize,
	)
}

// htmlFragmentSanitizer sanitizes a rendered HTML fragment. README rendering
// supplies readmePolicy.Sanitize directly; comment rendering supplies a
// stricter sanitizer that also removes DOM identifiers.
type htmlFragmentSanitizer func(string) string

// renderMarkdownHTML converts Markdown to sanitized HTML using the shared URL
// rewriting pipeline. The caller decides whether the Markdown gets README
// frontmatter stripping and which sanitizer runs last, so comment content can
// opt out of model-card frontmatter handling without duplicating rendering.
func renderMarkdownHTML(markdown, baseRawURL, baseBlobURL, endpointHost string, sanitize htmlFragmentSanitizer) string {
	return renderMarkdownHTMLWithFallback(markdown, baseRawURL, baseBlobURL, endpointHost, sanitize, sanitize)
}

// renderMarkdownHTMLWithFallback additionally selects the sanitizer used when
// parsing or rewriting fails. README rendering uses the stricter fallback
// policy there because the rewriter could not strip README-authored
// data-hf-repo* attributes.
func renderMarkdownHTMLWithFallback(markdown, baseRawURL, baseBlobURL, endpointBase string, sanitize, fallback htmlFragmentSanitizer) string {
	var buf bytes.Buffer
	if err := readmeMarkdown.Convert([]byte(markdown), &buf); err != nil {
		return ""
	}
	return sanitizeRewrittenHTMLWithFallback(buf.String(), baseRawURL, baseBlobURL, endpointBase, sanitize, fallback)
}

// sanitizeRewrittenHTML parses an HTML fragment, rewrites relative image and
// link URLs against the repo bases, and runs the result through sanitize. It is
// shared by README rendering and the community upstream-HTML fallback.
func sanitizeRewrittenHTML(fragment, baseRawURL, baseBlobURL, endpointHost string, sanitize htmlFragmentSanitizer) string {
	return sanitizeRewrittenHTMLWithFallback(fragment, baseRawURL, baseBlobURL, endpointHost, sanitize, sanitize)
}

func sanitizeRewrittenHTMLWithFallback(fragment, baseRawURL, baseBlobURL, endpointBase string, sanitize, fallback htmlFragmentSanitizer) string {
	doc, err := xhtml.Parse(strings.NewReader("<html><body>" + fragment + "</body></html>"))
	if err != nil {
		return fallback(fragment)
	}
	body := findReadmeBody(doc)
	if body == nil {
		return fallback(fragment)
	}
	endpoint, ok := parseReadmeEndpoint(endpointBase)
	if !ok {
		endpoint = readmeEndpointInfo{host: "huggingface.co"}
	}
	rewriteReadmeNodes(body, baseRawURL, baseBlobURL, endpoint)

	var out bytes.Buffer
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := xhtml.Render(&out, c); err != nil {
			return fallback(fragment)
		}
	}
	return sanitize(out.String())
}

// findReadmeBody walks an xhtml tree and returns the first <body>
// element it finds, or nil if none. The goldmark renderer wraps the
// document in <html><head>…</head><body>…</body></html> and we want
// the body to run rewriting on.
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

// rewriteReadmeNodes walks an xhtml subtree rewriting relative
// asset URLs to point through our /api/readme-asset proxy so the
// browser fetches them with the server's auth token. baseRaw and
// baseBlob are the upstream asset/blob URL prefixes for the repo;
// endpoint is the configured HF endpoint (used to leave absolute
// external URLs alone and to recognize links under its path prefix).
//
// Repository links on the configured endpoint get data-hf-repo*
// attributes for Model Details navigation, but keep their resolved
// absolute href as the safe external fallback. Every anchor is cleared
// first so README-authored attributes cannot forge an internal link.
func rewriteReadmeNodes(n *xhtml.Node, baseRaw, baseBlob string, endpoint readmeEndpointInfo) {
	if n.Type == xhtml.ElementNode {
		switch n.Data {
		case "img":
			rewriteAttr(n, "src", func(v string) string {
				abs := resolveReadmeURL(v, baseRaw)
				abs = normalizeReadmeImageURL(abs, endpoint.host)
				if isHostURL(abs, endpoint.host) {
					return readmeAssetPath + "?url=" + url.QueryEscape(abs)
				}
				return abs
			})
			setNodeAttr(n, "loading", "lazy")
		case "a":
			removeNodeAttr(n, "data-hf-repo")
			removeNodeAttr(n, "data-hf-repo-type")
			removeNodeAttr(n, "data-hf-repo-revision")
			if href, i := getNodeAttr(n, "href"); i >= 0 {
				abs := resolveReadmeURL(href, baseBlob)
				n.Attr[i].Val = abs
				repo, isDataset, revision, ok := classifyReadmeRepoURLForEndpoint(abs, endpoint)
				if ok {
					repoType := "model"
					if isDataset {
						repoType = "dataset"
					}
					setNodeAttr(n, "data-hf-repo", repo)
					setNodeAttr(n, "data-hf-repo-type", repoType)
					if revision != "" {
						setNodeAttr(n, "data-hf-repo-revision", revision)
					}
				}
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		rewriteReadmeNodes(c, baseRaw, baseBlob, endpoint)
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

// readmeReservedModelRoutes are Hugging Face top-level paths that are Hub
// features, not repository owners. A URL such as /papers/2401.04088 would
// otherwise look like owner/model; keeping it external avoids turning a plain
// link click into an analysis request. This check is models-only: /datasets/...
// remains a repository namespace even when the dataset owner matches one of
// these names.
var readmeReservedModelRoutes = map[string]struct{}{
	"about":         {},
	"api":           {},
	"assets":        {},
	"blog":          {},
	"brand":         {},
	"billing":       {},
	"careers":       {},
	"changelog":     {},
	"chat":          {},
	"collections":   {},
	"contact":       {},
	"course":        {},
	"courses":       {},
	"docs":          {},
	"duplicate":     {},
	"following":     {},
	"hardware":      {},
	"inference":     {},
	"join":          {},
	"learn":         {},
	"login":         {},
	"logout":        {},
	"models":        {},
	"new":           {},
	"notifications": {},
	"oauth":         {},
	"organizations": {},
	"papers":        {},
	"posts":         {},
	"pricing":       {},
	"privacy":       {},
	"search":        {},
	"security":      {},
	"settings":      {},
	"signup":        {},
	"solutions":     {},
	"spaces":        {},
	"static":        {},
	"support":       {},
	"tasks":         {},
	"terms":         {},
}

// classifyReadmeRepoURL recognizes canonical model/dataset repository views on
// the configured Hugging Face endpoint. Only repository roots and revision
// tree views are internal: blob/resolve/raw URLs address a file (or a
// README-relative asset), so they keep their external/new-tab behavior. A tree
// view is internal only when its revision is exactly one path segment after
// "tree"; a raw multi-segment path such as /tree/refs/pr/1 would otherwise be
// truncated into a wrong revision, so it stays external. An encoded slash
// (%2F) inside that segment still decodes to a slash within one revision.
// Reserved Hub pages (papers, docs, settings, ...) and links on any other host
// are deliberately not internal. When the configured endpoint includes a path
// prefix (for example https://mirror.example/hf), the prefix is stripped
// boundary-safely before the repository path is parsed.
func classifyReadmeRepoURL(rawURL, endpointBase string) (repo string, isDataset bool, revision string, ok bool) {
	endpoint, ok := parseReadmeEndpoint(endpointBase)
	if !ok {
		return "", false, "", false
	}
	return classifyReadmeRepoURLForEndpoint(rawURL, endpoint)
}

func classifyReadmeRepoURLForEndpoint(rawURL string, endpoint readmeEndpointInfo) (repo string, isDataset bool, revision string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false, "", false
	}
	// Compare normalized host:port values. Browsers drop an explicit default
	// port from URL.host, so the server must do the same or a link such as
	// https://huggingface.co:443/owner/model would classify differently in the
	// server-rendered HTML and the client fallback. Non-default ports still
	// have to match exactly.
	if normalizeReadmeHost(u.Host, u.Scheme) != normalizeReadmeHost(endpoint.host, u.Scheme) {
		return "", false, "", false
	}

	segments, ok := readmePathSegments(u.EscapedPath())
	if !ok {
		return "", false, "", false
	}
	segments, ok = stripReadmePathPrefix(segments, endpoint.pathPrefix)
	if !ok {
		return "", false, "", false
	}

	if len(segments) == 0 {
		return "", false, "", false
	}
	if segments[0] == "datasets" {
		isDataset = true
		segments = segments[1:]
	} else if _, reserved := readmeReservedModelRoutes[segments[0]]; reserved {
		return "", false, "", false
	}
	if len(segments) < 2 {
		return "", false, "", false
	}
	repo = segments[0] + "/" + segments[1]
	if !hfdownloader.IsValidModelName(repo) {
		return "", false, "", false
	}
	if len(segments) == 2 {
		return repo, isDataset, "", true
	}
	// Only a single-segment tree revision is repository navigation.
	// blob/resolve/raw address a file and must remain external, and a raw
	// multi-segment tree path cannot be represented by one revision.
	if segments[2] != "tree" || len(segments) != 4 {
		return "", false, "", false
	}
	revision = segments[3]
	if !validReadmeRevision(revision) {
		return "", false, "", false
	}
	return repo, isDataset, revision, true
}

// readmeEndpointInfo is the configured HF endpoint parsed into the pieces link
// classification needs: the host for origin matching and the decoded path
// prefix that must precede the repository path.
type readmeEndpointInfo struct {
	host       string
	pathPrefix []string
}

// parseReadmeEndpoint parses a configured endpoint URL (or a bare host) and
// returns its normalized host and decoded path segments.
func parseReadmeEndpoint(endpointBase string) (readmeEndpointInfo, bool) {
	endpointBase = strings.TrimSpace(endpointBase)
	if endpointBase == "" {
		endpointBase = "https://huggingface.co"
	}
	if !strings.Contains(endpointBase, "://") {
		endpointBase = "https://" + endpointBase
	}
	u, err := url.Parse(endpointBase)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return readmeEndpointInfo{}, false
	}
	pathPrefix, ok := readmePathSegments(u.EscapedPath())
	if !ok {
		return readmeEndpointInfo{}, false
	}
	return readmeEndpointInfo{host: u.Host, pathPrefix: pathPrefix}, true
}

// readmePathSegments splits an escaped URL path on literal slashes and decodes
// each segment. An encoded slash stays inside its segment instead of creating
// a new path component.
func readmePathSegments(escapedPath string) ([]string, bool) {
	parts := strings.Split(strings.Trim(escapedPath, "/"), "/")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		decoded, err := url.PathUnescape(part)
		if err != nil {
			return nil, false
		}
		segments = append(segments, decoded)
	}
	return segments, true
}

// stripReadmePathPrefix removes the configured endpoint path prefix from a
// decoded link path. Matching is per segment, so /hfold cannot satisfy /hf.
func stripReadmePathPrefix(segments, prefix []string) ([]string, bool) {
	if len(prefix) == 0 {
		return segments, true
	}
	if len(segments) < len(prefix) {
		return nil, false
	}
	for i := range prefix {
		if segments[i] != prefix[i] {
			return nil, false
		}
	}
	return segments[len(prefix):], true
}

// normalizeReadmeHost lowercases a host[:port] and drops the port when it is
// the scheme's default, matching browser URL.host normalization.
func normalizeReadmeHost(hostport, scheme string) string {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return ""
	}
	u, err := url.Parse("//" + hostport)
	if err != nil || u.Host == "" {
		return strings.ToLower(hostport)
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		return host
	}
	switch strings.ToLower(scheme) {
	case "https":
		if port == "443" {
			return host
		}
	case "http":
		if port == "80" {
			return host
		}
	}
	return net.JoinHostPort(host, port)
}

// validReadmeRevision rejects values that cannot be represented safely in the
// analyze request or in a DOM data attribute. Slashes are allowed because HF
// branch names may contain them; an encoded slash stays within this segment.
func validReadmeRevision(revision string) bool {
	if revision == "" || revision == "." || revision == ".." || strings.ContainsAny(revision, "\\\x00") {
		return false
	}
	for _, r := range revision {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// normalizeReadmeImageURL rewrites same-host Hugging Face image URLs from
// /raw/... to /resolve/... so Git LFS-backed assets fetch actual bytes instead
// of pointer files. Non-HF URLs and already-resolved paths are returned
// unchanged.
func normalizeReadmeImageURL(raw, endpointHost string) string {
	if !isHostURL(raw, endpointHost) {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) < 4 {
		return raw
	}
	rawIdx := 2
	if parts[0] == "datasets" {
		if len(parts) < 5 {
			return raw
		}
		rawIdx = 3
	}
	if parts[rawIdx] != "raw" {
		return raw
	}
	parts[rawIdx] = "resolve"
	u.Path = "/" + strings.Join(parts, "/")
	return u.String()
}

// isHostURL reports whether raw (an absolute or relative URL) parses
// to the same host as the given host string. Used to decide whether
// a <img src> in a rendered README is hosted on the configured HF
// endpoint (proxy it) or somewhere else (leave it alone).
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

// getNodeAttr returns the value of the named attribute on n and its
// index in n.Attr, or ("", -1) if not present.
func getNodeAttr(n *xhtml.Node, key string) (string, int) {
	for i, a := range n.Attr {
		if a.Key == key {
			return a.Val, i
		}
	}
	return "", -1
}

// setNodeAttr sets the named attribute on n to val, replacing it if
// it already exists or appending a new attribute otherwise.
func setNodeAttr(n *xhtml.Node, key, val string) {
	if _, i := getNodeAttr(n, key); i >= 0 {
		n.Attr[i].Val = val
		return
	}
	n.Attr = append(n.Attr, xhtml.Attribute{Key: key, Val: val})
}

// removeNodeAttr removes every occurrence of the named attribute from n.
func removeNodeAttr(n *xhtml.Node, key string) {
	kept := n.Attr[:0]
	for _, attr := range n.Attr {
		if attr.Key != key {
			kept = append(kept, attr)
		}
	}
	n.Attr = kept
}

// rewriteAttr applies fn to the value of the named attribute on n
// if the attribute is present, leaving it unchanged otherwise. Used
// to rewrite image src URLs in rendered READMEs.
func rewriteAttr(n *xhtml.Node, key string, fn func(string) string) {
	if v, i := getNodeAttr(n, key); i >= 0 {
		n.Attr[i].Val = fn(v)
	}
}

// readmeEndpointBase returns the configured HF endpoint URL, defaulting to
// huggingface.co. The full URL is preserved so repository-link classification
// can account for an endpoint path prefix as well as its host.
func readmeEndpointBase(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "https://huggingface.co"
	}
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return endpoint
	}
	return "https://huggingface.co"
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
	// Derive BOTH scheme and host (with port) from the configured endpoint, not
	// from the user-supplied URL. Forwarding the user's scheme could downgrade
	// to plain HTTP and leak the bearer token, and comparing only the hostname
	// breaks endpoints that use an explicit port. (SSRF guard.)
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = "https://huggingface.co"
	}
	ep, err := url.Parse(endpoint)
	if err != nil || ep.Host == "" {
		writeError(w, http.StatusInternalServerError, "Invalid endpoint configuration", "")
		return
	}
	if !strings.EqualFold(u.Host, ep.Host) {
		writeError(w, http.StatusForbidden, "Asset host not allowed", "")
		return
	}
	raw = normalizeReadmeImageURL(raw, ep.Host)
	u, err = url.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid url", "")
		return
	}
	safeURL := &url.URL{
		Scheme:   ep.Scheme,
		Host:     ep.Host,
		Path:     u.Path,
		RawQuery: u.RawQuery,
	}

	client, err := hfdownloader.BuildHTTPClient(cfg.Proxy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Invalid proxy configuration", err.Error())
		return
	}
	client.Timeout = 30 * time.Second

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, safeURL.String(), nil)
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
