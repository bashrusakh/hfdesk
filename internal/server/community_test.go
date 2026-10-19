// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	xhtml "golang.org/x/net/html"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// callCommunity invokes the handler directly so validation tests can supply
// repo values that the mux route would not match.
func callCommunity(t *testing.T, s *Server, repo, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/community/" + repo
	if query != "" {
		target += "?" + query
	}
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.SetPathValue("repo", repo)
	w := httptest.NewRecorder()
	s.handleCommunity(w, r)
	return w
}

func TestCommunityListModel(t *testing.T) {
	var (
		gotPath  string
		gotQuery url.Values
		gotAuth  string
		gotUA    string
	)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"count":12,"start":4,"discussions":[
			{"num":7,"title":"Question","author":{"name":"alice"},"status":"closed","isPullRequest":false,"createdAt":"2026-08-17T20:53:39.000Z","numComments":3,"pinned":true},
			{"num":8,"title":"Fix it","status":"open","isPullRequest":true,"createdAt":"2026-08-18T10:00:00.000Z","numComments":0,"pinned":false}
		]}`)
	}))
	defer hub.Close()

	s := &Server{config: Config{Endpoint: hub.URL + "/", Token: "hf_secret_token"}}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/community/owner/model?page=2&type=discussion&status=closed", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if gotPath != "/api/models/owner/model/discussions" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if got := gotQuery.Get("p"); got != "2" {
		t.Errorf("p = %q, want 2", got)
	}
	if got := gotQuery.Get("type"); got != "discussion" {
		t.Errorf("type = %q, want discussion", got)
	}
	if got := gotQuery.Get("status"); got != "closed" {
		t.Errorf("status = %q, want closed", got)
	}
	if gotAuth != "Bearer hf_secret_token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotUA != "hfdesk/1" {
		t.Errorf("User-Agent = %q, want hfdesk/1", gotUA)
	}
	if strings.Contains(w.Body.String(), "hf_secret_token") {
		t.Fatal("HF token leaked into the API response")
	}

	var resp CommunityListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Repo != "owner/model" || resp.IsDataset || resp.Page != 2 {
		t.Errorf("unexpected response metadata: %+v", resp)
	}
	if resp.Count != 12 || resp.Start != 4 || !resp.HasMore {
		t.Errorf("pagination = count %d start %d has_more %v", resp.Count, resp.Start, resp.HasMore)
	}
	if len(resp.Discussions) != 2 {
		t.Fatalf("discussions = %d, want 2", len(resp.Discussions))
	}
	first := resp.Discussions[0]
	if first.Num != 7 || first.Title != "Question" || first.Author != "alice" ||
		first.Status != "closed" || first.IsPullRequest || first.NumComments != 3 || !first.Pinned {
		t.Errorf("first discussion not normalized: %+v", first)
	}
	if first.CreatedAt != "2026-08-17T20:53:39.000Z" {
		t.Errorf("created_at = %q", first.CreatedAt)
	}
	second := resp.Discussions[1]
	if second.Author != "deleted" {
		t.Errorf("missing upstream author = %q, want deleted", second.Author)
	}
	if !second.IsPullRequest {
		t.Errorf("is_pull_request = false, want true for a PR")
	}
}

func TestCommunityListDatasetDefaults(t *testing.T) {
	var (
		gotPath  string
		gotQuery url.Values
	)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		io.WriteString(w, `{"count":0,"start":0,"discussions":[]}`)
	}))
	defer hub.Close()

	s := &Server{config: Config{Endpoint: hub.URL}}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/community/owner/data?dataset=TRUE", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if gotPath != "/api/datasets/owner/data/discussions" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if got := gotQuery.Get("p"); got != "0" {
		t.Errorf("p = %q, want 0", got)
	}
	if got := gotQuery.Get("type"); got != "all" {
		t.Errorf("type = %q, want all", got)
	}
	if got := gotQuery.Get("status"); got != "all" {
		t.Errorf("status = %q, want all", got)
	}
	if got := w.Body.String(); !strings.Contains(got, `"discussions":[]`) {
		t.Errorf("empty list must encode as [], got %s", got)
	}

	var resp CommunityListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.IsDataset {
		t.Error("is_dataset = false, want true")
	}
	if resp.Discussions == nil {
		t.Error("discussions must be an empty slice, not null")
	}
	if resp.HasMore {
		t.Error("has_more = true for an empty page")
	}
}

func TestCommunityAuthHeaderOnlyWhenTokenConfigured(t *testing.T) {
	var auth string
	var ua string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		ua = r.Header.Get("User-Agent")
		io.WriteString(w, `{"count":0,"start":0,"discussions":[]}`)
	}))
	defer hub.Close()

	s := &Server{config: Config{Endpoint: hub.URL}}
	w := callCommunity(t, s, "owner/model", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if auth != "" {
		t.Errorf("Authorization = %q, want empty when no token is configured", auth)
	}
	if ua != "hfdesk/1" {
		t.Errorf("User-Agent = %q, want hfdesk/1", ua)
	}
}

func TestCommunityThread(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/owner/model/discussions/2" {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("thread request must not send list parameters: %q", r.URL.RawQuery)
		}
		io.WriteString(w, `{
			"num":2,
			"title":"Upload LICENSE",
			"author":{"name":"meg","fullname":"Meg"},
			"status":"merged",
			"isPullRequest":true,
			"createdAt":"2022-06-02T12:01:38.000Z",
			"events":[
				{"id":"e1","type":"comment","author":{"name":"meg"},"createdAt":"2022-06-02T12:01:38.000Z","data":{"edited":false,"hidden":false,"latest":{"raw":"Hello **world**\n\n<script>alert(1)</script>\n\n<img src=x onerror=alert(2)>\n\n[link](docs/a.md)","html":"<p>upstream html</p>"}}},
				{"id":"e2","type":"status-change","author":{"name":"meg"},"createdAt":"2022-06-02T12:02:00.000Z","data":{"status":"merged"}},
				{"id":"e3","type":"commit","author":{"name":"meg"},"createdAt":"2022-06-02T12:03:00.000Z","data":{"subject":"Upload LICENSE","oid":"8ea340b34481fd0a03879abcb2dc5b4faa8ff176"}},
				{"id":"e4","type":"title-change","author":{"name":"meg"},"createdAt":"2022-06-02T12:04:00.000Z","data":{"from":"Old title","to":"New title"}},
				{"id":"e5","type":"pinning-change","author":{"name":"meg"},"createdAt":"2022-06-02T12:05:00.000Z","data":{"pinned":true}},
				{"id":"e6","type":"locking-change","author":{"name":"meg"},"createdAt":"2022-06-02T12:06:00.000Z","data":{"locked":false}},
				{"id":"e7","type":"report-status-change","author":{"name":"meg"},"createdAt":"2022-06-02T12:07:00.000Z","data":{"reportStatus":"ignored"}},
				{"id":"e8","type":"ref-deleted","author":{"name":"meg"},"createdAt":"2022-06-02T12:08:00.000Z","data":{"ref":"refs/pr/2"}},
				{"id":"e9","type":"commits-truncated","author":{"name":"meg"},"createdAt":"2022-06-02T12:09:00.000Z","data":{}},
				{"id":"e10","type":"reaction","author":{"name":"meg"},"createdAt":"2022-06-02T12:10:00.000Z","data":{}},
				{"id":"e11","type":"comment","createdAt":"2022-06-02T12:11:00.000Z","data":{"edited":false,"hidden":false,"latest":{"html":"<p>fallback <script>alert(3)</script><b>ok</b></p>"}}}
			]
		}`)
	}))
	defer hub.Close()

	s := &Server{config: Config{Endpoint: hub.URL, Token: "hf_secret_token"}}
	w := callCommunity(t, s, "owner/model", "num=2")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "hf_secret_token") {
		t.Fatal("HF token leaked into the API response")
	}

	var resp CommunityThreadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Repo != "owner/model" || resp.Num != 2 || resp.Title != "Upload LICENSE" ||
		resp.Author != "meg" || resp.Status != "merged" || !resp.IsPullRequest {
		t.Errorf("unexpected thread metadata: %+v", resp)
	}
	if resp.CreatedAt != "2022-06-02T12:01:38.000Z" {
		t.Errorf("created_at = %q", resp.CreatedAt)
	}
	wantTypes := []string{"comment", "status-change", "commit", "title-change", "pinning-change", "locking-change", "report-status-change", "ref-deleted", "commits-truncated", "reaction", "comment"}
	if len(resp.Events) != len(wantTypes) {
		t.Fatalf("events = %d, want %d", len(resp.Events), len(wantTypes))
	}
	for i, want := range wantTypes {
		if resp.Events[i].Type != want {
			t.Errorf("event %d type = %q, want %q (order must be preserved)", i, resp.Events[i].Type, want)
		}
	}

	first := resp.Events[0]
	if first.BodyHTML == nil {
		t.Fatal("comment body_html is missing")
	}
	body := *first.BodyHTML
	if !strings.Contains(body, "<strong>world</strong>") {
		t.Errorf("comment markdown was not rendered: %q", body)
	}
	if strings.Contains(strings.ToLower(body), "<script") || strings.Contains(strings.ToLower(body), "onerror") {
		t.Errorf("comment HTML was not sanitized: %q", body)
	}
	if strings.Contains(body, "upstream html") {
		t.Errorf("raw markdown must be preferred over upstream HTML: %q", body)
	}
	if !strings.Contains(body, "/owner/model/blob/main/docs/a.md") {
		t.Errorf("relative link was not resolved against the repo: %q", body)
	}
	if first.Edited == nil || *first.Edited || first.Hidden == nil || *first.Hidden {
		t.Errorf("comment edited/hidden not reported: %+v", first)
	}
	if first.Summary != "" {
		t.Errorf("comment summary = %q, want empty", first.Summary)
	}

	wantSummaries := map[int]string{
		1: "changed status to merged",
		9: "reaction",
	}
	for i, want := range wantSummaries {
		if resp.Events[i].Summary != want {
			t.Errorf("event %d summary = %q, want %q", i, resp.Events[i].Summary, want)
		}
		if resp.Events[i].BodyHTML != nil {
			t.Errorf("event %d should not carry body_html", i)
		}
	}
	for i, fragment := range map[int]string{
		2: "Upload LICENSE",
		3: "Old title",
		4: "pinned",
		5: "unlocked",
		6: "ignored",
		7: "refs/pr/2",
		8: "additional commits",
	} {
		if !strings.Contains(resp.Events[i].Summary, fragment) {
			t.Errorf("event %d summary = %q, want it to contain %q", i, resp.Events[i].Summary, fragment)
		}
		if resp.Events[i].Edited != nil || resp.Events[i].Hidden != nil {
			t.Errorf("non-comment event %d must not carry edited/hidden", i)
		}
	}

	fallback := resp.Events[10]
	if fallback.Author != "deleted" {
		t.Errorf("missing comment author = %q, want deleted", fallback.Author)
	}
	if fallback.BodyHTML == nil || !strings.Contains(*fallback.BodyHTML, "<b>ok</b>") ||
		strings.Contains(strings.ToLower(*fallback.BodyHTML), "<script") {
		t.Errorf("fallback HTML was not sanitized: %v", fallback.BodyHTML)
	}

	var generic struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &generic); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"body_html", "edited", "hidden"} {
		if _, ok := generic.Events[0][key]; !ok {
			t.Errorf("comment event is missing %q", key)
		}
	}
	for _, key := range []string{"body_html", "edited", "hidden"} {
		if _, ok := generic.Events[1][key]; ok {
			t.Errorf("system event unexpectedly carries %q", key)
		}
	}
	if _, ok := generic.Events[1]["summary"]; !ok {
		t.Error("system event is missing summary")
	}
	if _, ok := generic.Events[0]["author"].(string); !ok {
		t.Error("event author must be a normalized string, not an upstream object")
	}
}

// communityCommentBody renders one comment event through the thread handler and
// returns the sanitized body_html the API exposes plus the fake endpoint URL.
func communityCommentBody(t *testing.T, raw, upstreamHTML string) (body, endpoint string) {
	t.Helper()
	latest, err := json.Marshal(struct {
		Raw  string `json:"raw"`
		HTML string `json:"html"`
	}{Raw: raw, HTML: upstreamHTML})
	if err != nil {
		t.Fatal(err)
	}

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/discussions/") {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"num":1,"title":"thread","status":"open","events":[
			{"id":"c1","type":"comment","data":{"latest":`+string(latest)+`}}
		]}`)
	}))
	defer hub.Close()

	s := &Server{config: Config{Endpoint: hub.URL}}
	w := callCommunity(t, s, "owner/model", "num=1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp CommunityThreadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Events) != 1 || resp.Events[0].BodyHTML == nil {
		t.Fatalf("missing comment body_html: %+v", resp.Events)
	}
	return *resp.Events[0].BodyHTML, hub.URL
}

// assertNoHTMLIdentifiers fails if any element in fragment keeps an id or name
// attribute. Comments are injected into the application document, so such
// attributes can shadow elements resolved by document.querySelector.
func assertNoHTMLIdentifiers(t *testing.T, fragment string) {
	t.Helper()
	doc, err := xhtml.Parse(strings.NewReader("<html><body>" + fragment + "</body></html>"))
	if err != nil {
		t.Fatalf("parse comment HTML: %v", err)
	}
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			for _, a := range n.Attr {
				if a.Key == "id" || a.Key == "name" {
					t.Errorf("comment HTML retains %s=%q in %q", a.Key, a.Val, fragment)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
}

func TestCommunityCommentHTMLRemovesGlobalIdentifiers(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		upstreamHTML string
	}{
		{
			name: "raw markdown",
			raw: `<div id="page-jobs">clobber</div><map name="page-jobs"></map>` +
				`<span name="page-cache">named</span><a id="page-settings">anchor</a>` +
				`<div><span ID="page-cache">nested</span></div>`,
		},
		{
			name:         "upstream html fallback",
			upstreamHTML: `<div id="page-jobs">clobber</div><map name="page-jobs"></map>`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := communityCommentBody(t, tt.raw, tt.upstreamHTML)
			if !strings.Contains(body, "clobber") {
				t.Fatalf("comment content missing from body_html: %q", body)
			}
			assertNoHTMLIdentifiers(t, body)
		})
	}
}

func TestCommunityCommentHTMLKeepsFrontmatterLikeContent(t *testing.T) {
	body, _ := communityCommentBody(t, "---\n**Note:** see below\n---\nDetails", "")

	if !strings.Contains(body, "Note:") {
		t.Errorf("leading comment content was stripped as frontmatter: %q", body)
	}
	if !strings.Contains(body, "Details") {
		t.Errorf("trailing comment content missing: %q", body)
	}
}

func TestCommunityCommentHTMLFallbackRewritesURLs(t *testing.T) {
	body, endpoint := communityCommentBody(t, "",
		`<p><img src="assets/logo.png"><a href="docs/guide.md">guide</a></p>`)

	wantImage := "/api/readme-asset?url=" + url.QueryEscape(endpoint+"/owner/model/resolve/main/assets/logo.png")
	if !strings.Contains(body, wantImage) {
		t.Errorf("fallback image was not proxied through %s: %q", wantImage, body)
	}
	wantLink := endpoint + "/owner/model/blob/main/docs/guide.md"
	if !strings.Contains(body, wantLink) {
		t.Errorf("fallback link was not resolved to %q: %q", wantLink, body)
	}
}

func TestCommunityCommentHTMLFallbackStaysSanitized(t *testing.T) {
	body, _ := communityCommentBody(t, "",
		`<script>alert(1)</script><img src=x onerror=alert(2)><b>ok</b>`)

	if !strings.Contains(body, "<b>ok</b>") {
		t.Errorf("safe fallback HTML was dropped: %q", body)
	}
	lower := strings.ToLower(body)
	if strings.Contains(lower, "<script") || strings.Contains(lower, "onerror") {
		t.Errorf("fallback HTML was not sanitized: %q", body)
	}
	assertNoHTMLIdentifiers(t, body)
}

func TestCommunityThreadDataset(t *testing.T) {
	var gotPath string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, `{"num":5,"title":"Dataset thread","author":{"name":"data-user"},"status":"open","isPullRequest":false,"createdAt":"2026-01-01T00:00:00.000Z","events":[]}`)
	}))
	defer hub.Close()

	s := &Server{config: Config{Endpoint: hub.URL}}
	w := callCommunity(t, s, "owner/data", "dataset=true&num=5")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if gotPath != "/api/datasets/owner/data/discussions/5" {
		t.Errorf("upstream path = %q", gotPath)
	}
	var resp CommunityThreadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.IsDataset || resp.Num != 5 || resp.Events == nil {
		t.Errorf("unexpected thread response: %+v", resp)
	}
}

func TestCommunityUpstreamErrorMapping(t *testing.T) {
	tests := []struct {
		name     string
		upstream int
		want     int
	}{
		{"bad request", http.StatusBadRequest, http.StatusBadRequest},
		{"unauthorized", http.StatusUnauthorized, http.StatusUnauthorized},
		{"forbidden", http.StatusForbidden, http.StatusForbidden},
		{"not found", http.StatusNotFound, http.StatusNotFound},
		{"rate limited", http.StatusTooManyRequests, http.StatusTooManyRequests},
		{"server error", http.StatusInternalServerError, http.StatusBadGateway},
		{"upstream unavailable", http.StatusServiceUnavailable, http.StatusBadGateway},
		{"teapot", http.StatusTeapot, http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.upstream)
				io.WriteString(w, `{"error":"hf_secret_token rejected"}`)
			}))
			defer hub.Close()

			s := &Server{config: Config{Endpoint: hub.URL, Token: "hf_secret_token"}}
			w := callCommunity(t, s, "owner/model", "")
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.want, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "hf_secret_token") {
				t.Fatal("HF token leaked into the error response")
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Error == "" || resp.OK {
				t.Errorf("unexpected error response: %+v", resp)
			}
		})
	}
}

func TestCommunityMalformedUpstreamJSON(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"count":1,"discussions":`)
	}))
	defer hub.Close()

	s := &Server{config: Config{Endpoint: hub.URL, Token: "hf_secret_token"}}
	w := callCommunity(t, s, "owner/model", "")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "hf_secret_token") {
		t.Fatal("HF token leaked into the parse error response")
	}
}

func TestCommunityNetworkError(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := hub.URL
	hub.Close()

	s := &Server{config: Config{Endpoint: endpoint, Token: "hf_secret_token"}}
	w := callCommunity(t, s, "owner/model", "")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "hf_secret_token") {
		t.Fatal("HF token leaked into the network error response")
	}
}

func TestCommunityProxyConfiguration(t *testing.T) {
	var calls atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"count":0,"start":0,"discussions":[]}`)
	}))
	defer hub.Close()

	t.Run("invalid proxy configuration", func(t *testing.T) {
		s := &Server{config: Config{
			Endpoint: hub.URL,
			Proxy:    &hfdownloader.ProxyConfig{URL: "://bad", NoEnvProxy: true},
		}}
		w := callCommunity(t, s, "owner/model", "")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
		}
		if calls.Load() != 0 {
			t.Error("invalid proxy configuration reached the endpoint directly")
		}
	})

	t.Run("unreachable proxy", func(t *testing.T) {
		// Port zero cannot host a listener, so this proxy is deterministically
		// unreachable.
		s := &Server{config: Config{
			Endpoint: hub.URL,
			Proxy:    &hfdownloader.ProxyConfig{URL: "http://127.0.0.1:0", NoEnvProxy: true},
		}}
		w := callCommunity(t, s, "owner/model", "")
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
		}
		if calls.Load() != 0 {
			t.Error("proxy failure bypassed to the origin")
		}
	})
}

func TestCommunityInvalidInputs(t *testing.T) {
	var calls atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.Contains(r.URL.Path, "/discussions/") {
			io.WriteString(w, `{"num":1,"title":"ok","status":"open","events":[]}`)
			return
		}
		io.WriteString(w, `{"count":0,"start":0,"discussions":[]}`)
	}))
	defer hub.Close()
	s := &Server{config: Config{Endpoint: hub.URL}}

	invalid := []struct {
		name  string
		repo  string
		query string
	}{
		{"missing repo", "", ""},
		{"repo without owner", "owner", ""},
		{"dot segment", "owner/..", ""},
		{"zero num", "owner/model", "num=0"},
		{"negative num", "owner/model", "num=-1"},
		{"non-integer num", "owner/model", "num=abc"},
		{"empty num", "owner/model", "num="},
		{"negative page", "owner/model", "page=-1"},
		{"non-integer page", "owner/model", "page=abc"},
		{"fractional page", "owner/model", "page=1.5"},
		{"empty page", "owner/model", "page="},
		{"invalid type", "owner/model", "type=issues"},
		{"invalid status", "owner/model", "status=pending"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			before := calls.Load()
			w := callCommunity(t, s, tt.repo, tt.query)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if calls.Load() != before {
				t.Error("invalid input reached the upstream endpoint")
			}
		})
	}

	valid := []struct {
		name  string
		query string
	}{
		{"thread", "num=1"},
		{"list", "page=0&type=all&status=all"},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			w := callCommunity(t, s, "owner/model", tt.query)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCommunityThreadMalformedEventDataDoesNotFail(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"title":"thread","status":"open","events":[
			{"id":"bad-comment","type":"comment","data":"not-an-object"},
			{"id":"bad-system","type":"status-change","data":[]}
		]}`)
	}))
	defer hub.Close()

	s := &Server{config: Config{Endpoint: hub.URL}}
	w := callCommunity(t, s, "owner/model", "num=1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp CommunityThreadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Num != 1 {
		t.Errorf("num = %d, want requested 1 when upstream omits it", resp.Num)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(resp.Events))
	}
	if resp.Events[0].BodyHTML == nil || *resp.Events[0].BodyHTML != "" {
		t.Errorf("malformed comment body = %v, want empty string", resp.Events[0].BodyHTML)
	}
	if resp.Events[1].Summary != "changed status" {
		t.Errorf("malformed system event summary = %q", resp.Events[1].Summary)
	}
}
