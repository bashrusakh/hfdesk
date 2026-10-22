// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"

	"github.com/bashrusakh/hfdesk/internal/logging"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// communityLog is the logger of the community proxy subsystem. It resolves
// the current default logger on every call, so records reach whichever
// pipeline the application installed.
var communityLog = logging.NewComponentLogger("community")

const (
	communityDefaultEndpoint = "https://huggingface.co"
	// communityMaxBody bounds how much upstream JSON HFDesk will read for one
	// community request. A list page or a long thread is far below this.
	communityMaxBody = 16 << 20 // 16 MiB
)

// CommunityDiscussion is one Discussion or Pull Request in a repository's
// community list. Author is a normalized username; a deleted or missing
// upstream author is reported as "deleted".
type CommunityDiscussion struct {
	Num           int    `json:"num"`
	Title         string `json:"title"`
	Author        string `json:"author"`
	Status        string `json:"status"`
	IsPullRequest bool   `json:"is_pull_request"`
	CreatedAt     string `json:"created_at"`
	NumComments   int    `json:"num_comments"`
	Pinned        bool   `json:"pinned"`
}

// CommunityListResponse is the response for a community list page.
type CommunityListResponse struct {
	Repo        string                `json:"repo"`
	IsDataset   bool                  `json:"is_dataset"`
	Page        int                   `json:"page"`
	Count       int                   `json:"count"`
	Start       int                   `json:"start"`
	HasMore     bool                  `json:"has_more"`
	Discussions []CommunityDiscussion `json:"discussions"`
}

// CommunityEvent is one event in a community thread. Comment events carry the
// sanitized BodyHTML plus Edited/Hidden; other event types carry a
// human-readable Summary instead.
type CommunityEvent struct {
	ID        string  `json:"id"`
	Type      string  `json:"type"`
	Author    string  `json:"author"`
	CreatedAt string  `json:"created_at"`
	BodyHTML  *string `json:"body_html,omitempty"`
	Edited    *bool   `json:"edited,omitempty"`
	Hidden    *bool   `json:"hidden,omitempty"`
	Summary   string  `json:"summary,omitempty"`
}

// CommunityThreadResponse is the response for a single community thread.
type CommunityThreadResponse struct {
	Repo          string           `json:"repo"`
	IsDataset     bool             `json:"is_dataset"`
	Num           int              `json:"num"`
	Title         string           `json:"title"`
	Author        string           `json:"author"`
	Status        string           `json:"status"`
	IsPullRequest bool             `json:"is_pull_request"`
	CreatedAt     string           `json:"created_at"`
	Events        []CommunityEvent `json:"events"`
}

// hfCommunityUser is the upstream author object. Only the display name is
// exposed; the rest of the upstream user object is intentionally dropped.
type hfCommunityUser struct {
	Name string `json:"name"`
}

type hfCommunityDiscussion struct {
	Num           int              `json:"num"`
	Title         string           `json:"title"`
	Author        *hfCommunityUser `json:"author"`
	Status        string           `json:"status"`
	IsPullRequest bool             `json:"isPullRequest"`
	CreatedAt     string           `json:"createdAt"`
	NumComments   int              `json:"numComments"`
	Pinned        bool             `json:"pinned"`
}

type hfCommunityList struct {
	Count       int                     `json:"count"`
	Start       int                     `json:"start"`
	Discussions []hfCommunityDiscussion `json:"discussions"`
}

type hfCommunityThread struct {
	Num           int                `json:"num"`
	Title         string             `json:"title"`
	Author        *hfCommunityUser   `json:"author"`
	Status        string             `json:"status"`
	IsPullRequest bool               `json:"isPullRequest"`
	CreatedAt     string             `json:"createdAt"`
	Events        []hfCommunityEvent `json:"events"`
}

type hfCommunityEvent struct {
	ID        string           `json:"id"`
	Type      string           `json:"type"`
	Author    *hfCommunityUser `json:"author"`
	CreatedAt string           `json:"createdAt"`
	Data      json.RawMessage  `json:"data"`
}

type hfCommunityCommentData struct {
	Edited bool `json:"edited"`
	Hidden bool `json:"hidden"`
	Latest struct {
		Raw  string `json:"raw"`
		HTML string `json:"html"`
	} `json:"latest"`
}

// hfCommunityEventData collects the data fields used to summarize non-comment
// events. Unknown fields are ignored.
type hfCommunityEventData struct {
	Status       string `json:"status"`
	ReportStatus string `json:"reportStatus"`
	Subject      string `json:"subject"`
	OID          string `json:"oid"`
	From         string `json:"from"`
	To           string `json:"to"`
	Pinned       *bool  `json:"pinned"`
	Locked       *bool  `json:"locked"`
	Ref          string `json:"ref"`
	RefName      string `json:"refName"`
}

// handleCommunity proxies a repository's Hugging Face community content
// (Discussions and Pull Requests) read-only.
//
// Query parameters:
//
//	dataset – "true" to use the dataset API (case-insensitive)
//	num     – when present, return that thread instead of the list page
//	page    – list page (default 0)
//	type    – all | discussion | pull_request (default all)
//	status  – all | open | closed (default all)
func (s *Server) handleCommunity(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	if repo == "" {
		writeError(w, http.StatusBadRequest, "Missing repository", "Format: /api/community/owner/name")
		return
	}
	if !hfdownloader.IsValidModelName(repo) {
		writeError(w, http.StatusBadRequest, "Invalid repo format", "Expected owner/name")
		return
	}

	q := r.URL.Query()
	isDataset := strings.EqualFold(q.Get("dataset"), "true")
	cfg := s.snapshotConfig()
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	if endpoint == "" {
		endpoint = communityDefaultEndpoint
	}

	if vals, ok := q["num"]; ok {
		num, err := strconv.Atoi(vals[0])
		if err != nil || num <= 0 {
			writeError(w, http.StatusBadRequest, "Invalid discussion number", "num must be a positive integer")
			return
		}
		s.handleCommunityThread(w, r, cfg, endpoint, repo, isDataset, num)
		return
	}

	page := 0
	if vals, ok := q["page"]; ok {
		n, err := strconv.Atoi(vals[0])
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "Invalid page", "page must be a non-negative integer")
			return
		}
		page = n
	}
	discussionType := q.Get("type")
	if discussionType == "" {
		discussionType = "all"
	}
	switch discussionType {
	case "all", "discussion", "pull_request":
	default:
		writeError(w, http.StatusBadRequest, "Invalid discussion type", "type must be all, discussion, or pull_request")
		return
	}
	status := q.Get("status")
	if status == "" {
		status = "all"
	}
	switch status {
	case "all", "open", "closed":
	default:
		writeError(w, http.StatusBadRequest, "Invalid discussion status", "status must be all, open, or closed")
		return
	}

	params := url.Values{}
	params.Set("p", strconv.Itoa(page))
	params.Set("type", discussionType)
	params.Set("status", status)
	hfURL := communityAPIBase(endpoint, repo, isDataset) + "?" + params.Encode()

	var raw hfCommunityList
	if !s.fetchCommunityJSON(w, r, cfg, hfURL, &raw) {
		return
	}

	discussions := make([]CommunityDiscussion, 0, len(raw.Discussions))
	for _, d := range raw.Discussions {
		discussions = append(discussions, CommunityDiscussion{
			Num:           d.Num,
			Title:         d.Title,
			Author:        communityAuthorName(d.Author),
			Status:        d.Status,
			IsPullRequest: d.IsPullRequest,
			CreatedAt:     d.CreatedAt,
			NumComments:   d.NumComments,
			Pinned:        d.Pinned,
		})
	}

	writeJSON(w, http.StatusOK, CommunityListResponse{
		Repo:        repo,
		IsDataset:   isDataset,
		Page:        page,
		Count:       raw.Count,
		Start:       raw.Start,
		HasMore:     raw.Start+len(discussions) < raw.Count,
		Discussions: discussions,
	})
}

func (s *Server) handleCommunityThread(w http.ResponseWriter, r *http.Request, cfg Config, endpoint, repo string, isDataset bool, num int) {
	hfURL := fmt.Sprintf("%s/%d", communityAPIBase(endpoint, repo, isDataset), num)

	var raw hfCommunityThread
	if !s.fetchCommunityJSON(w, r, cfg, hfURL, &raw) {
		return
	}

	baseRaw, baseBlob := communityThreadBases(endpoint, repo, isDataset)
	events := make([]CommunityEvent, 0, len(raw.Events))
	for _, e := range raw.Events {
		event := CommunityEvent{
			ID:        e.ID,
			Type:      e.Type,
			Author:    communityAuthorName(e.Author),
			CreatedAt: e.CreatedAt,
		}
		if e.Type == "comment" {
			var data hfCommunityCommentData
			if len(e.Data) > 0 {
				// A malformed event body must not fail the whole thread; the
				// comment degrades to an empty sanitized body.
				_ = json.Unmarshal(e.Data, &data)
			}
			body := renderCommunityCommentHTML(data.Latest.Raw, data.Latest.HTML, baseRaw, baseBlob, readmeEndpointBase(cfg.Endpoint))
			edited, hidden := data.Edited, data.Hidden
			event.BodyHTML = &body
			event.Edited = &edited
			event.Hidden = &hidden
		} else {
			event.Summary = communityEventSummary(e.Type, e.Data)
		}
		events = append(events, event)
	}

	writeJSON(w, http.StatusOK, CommunityThreadResponse{
		Repo:          repo,
		IsDataset:     isDataset,
		Num:           communityThreadNum(raw.Num, num),
		Title:         raw.Title,
		Author:        communityAuthorName(raw.Author),
		Status:        raw.Status,
		IsPullRequest: raw.IsPullRequest,
		CreatedAt:     raw.CreatedAt,
		Events:        events,
	})
}

// communityThreadNum keeps the upstream thread number, falling back to the
// requested number if an upstream fixture/response omits it.
func communityThreadNum(upstreamNum, requested int) int {
	if upstreamNum == 0 {
		return requested
	}
	return upstreamNum
}

func (s *Server) fetchCommunityJSON(w http.ResponseWriter, r *http.Request, cfg Config, hfURL string, dst any) bool {
	client, err := hfdownloader.BuildHTTPClient(cfg.Proxy)
	if err != nil {
		communityLog.Info("invalid proxy configuration")
		writeError(w, http.StatusInternalServerError, "Invalid proxy configuration", "")
		return false
	}
	client.Timeout = 20 * time.Second

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, hfURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to build upstream request", "")
		return false
	}
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	req.Header.Set("User-Agent", "hfdesk/1")

	resp, err := client.Do(req)
	if err != nil {
		// The error can contain the request URL but never the Authorization
		// header; keep the token out of both the log and the API response.
		communityLog.Info("upstream request failed", "error", err)
		writeError(w, http.StatusBadGateway, "Community request failed", "")
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		writeError(w, communityUpstreamStatus(resp.StatusCode), communityUpstreamMessage(resp.StatusCode), "")
		return false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, communityMaxBody+1))
	if err != nil {
		writeError(w, http.StatusBadGateway, "Failed to read community response", "")
		return false
	}
	if len(body) > communityMaxBody {
		writeError(w, http.StatusBadGateway, "Community response too large", "")
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		// Do not relay the upstream body: it can contain unrelated details and
		// the UI only needs to isolate the Community failure.
		writeError(w, http.StatusBadGateway, "Failed to parse community response", "")
		return false
	}
	return true
}

// communityAPIBase builds the upstream discussions endpoint for a repo. Each
// repo path segment is escaped so a validated-but-unusual name cannot inject
// query parameters or extra path components.
func communityAPIBase(endpoint, repo string, isDataset bool) string {
	repoType := "models"
	if isDataset {
		repoType = "datasets"
	}
	return fmt.Sprintf("%s/api/%s/%s/discussions", endpoint, repoType, escapeCommunityRepo(repo))
}

func escapeCommunityRepo(repo string) string {
	segments := strings.Split(repo, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	return strings.Join(segments, "/")
}

func communityThreadBases(endpoint, repo string, isDataset bool) (baseRaw, baseBlob string) {
	prefix := ""
	if isDataset {
		prefix = "datasets/"
	}
	repoPath := escapeCommunityRepo(repo)
	baseRaw = fmt.Sprintf("%s/%s%s/resolve/main/", endpoint, prefix, repoPath)
	baseBlob = fmt.Sprintf("%s/%s%s/blob/main/", endpoint, prefix, repoPath)
	return baseRaw, baseBlob
}

func communityAuthorName(author *hfCommunityUser) string {
	if author == nil || author.Name == "" {
		return "deleted"
	}
	return author.Name
}

// renderCommunityCommentHTML renders third-party comment HTML. Comments are not
// model cards, so a leading "---" block is content rather than frontmatter and
// is rendered verbatim. Both the raw-Markdown path and the upstream-HTML
// fallback go through the shared README URL-rewriting pipeline: relative links
// resolve against the repo and endpoint-hosted images use the local asset
// proxy. The result is always sanitized, and unlike READMEs it must not keep
// DOM identifiers (see sanitizeCommunityCommentHTML).
func renderCommunityCommentHTML(raw, upstreamHTML, baseRaw, baseBlob, endpointHost string) string {
	if strings.TrimSpace(raw) != "" {
		if rendered := renderMarkdownHTML(raw, baseRaw, baseBlob, endpointHost, sanitizeCommunityCommentHTML); rendered != "" {
			return rendered
		}
	}
	return sanitizeRewrittenHTML(upstreamHTML, baseRaw, baseBlob, endpointHost, sanitizeCommunityCommentHTML)
}

// sanitizeCommunityCommentHTML applies the README sanitizer and then removes
// DOM identifiers and repository-navigation attributes. Comment HTML is
// injected into the application document, so a retained id such as "page-jobs"
// would make document.querySelector("#page-jobs") resolve to the comment
// instead of the real page and blank the page content. Comments are not model
// cards, so a retained data-hf-repo* attribute would turn an external comment
// link into internal Model Details navigation; those attributes are stripped
// while the external href fallback remains. README rendering keeps both its
// identifiers and its classified repository-link attributes. A fragment that
// cannot be re-parsed for stripping is discarded rather than risking an
// identifier or navigation-attribute escape.
func sanitizeCommunityCommentHTML(fragment string) string {
	stripped, ok := stripCommunityCommentAttributes(readmePolicy.Sanitize(fragment))
	if !ok {
		return ""
	}
	return stripped
}

// stripCommunityCommentAttributes removes id/name and data-hf-repo* attributes
// from every element in an HTML fragment, reporting false if the fragment
// cannot be parsed or rendered back. Keeping the attribute removal here (after
// sanitization) makes the guarantee independent of the sanitizer's attribute
// policy.
func stripCommunityCommentAttributes(fragment string) (string, bool) {
	doc, err := xhtml.Parse(strings.NewReader("<html><body>" + fragment + "</body></html>"))
	if err != nil || doc == nil {
		return "", false
	}
	body := findReadmeBody(doc)
	if body == nil {
		return "", false
	}
	stripCommunityCommentAttrs(body)

	var out bytes.Buffer
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := xhtml.Render(&out, c); err != nil {
			return "", false
		}
	}
	return out.String(), true
}

func stripCommunityCommentAttrs(n *xhtml.Node) {
	if n.Type == xhtml.ElementNode {
		attrs := n.Attr[:0]
		for _, a := range n.Attr {
			switch a.Key {
			case "id", "name", "data-hf-repo", "data-hf-repo-type", "data-hf-repo-revision":
				continue
			}
			attrs = append(attrs, a)
		}
		n.Attr = attrs
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		stripCommunityCommentAttrs(c)
	}
}

func communityEventSummary(eventType string, data []byte) string {
	var payload hfCommunityEventData
	if len(data) > 0 {
		_ = json.Unmarshal(data, &payload)
	}

	switch eventType {
	case "status-change":
		if payload.Status != "" {
			return "changed status to " + payload.Status
		}
		return "changed status"
	case "commit":
		if payload.Subject != "" {
			return "committed: " + payload.Subject
		}
		if payload.OID != "" {
			return "committed " + shortCommunityOID(payload.OID)
		}
		return "committed"
	case "title-change":
		switch {
		case payload.From != "" && payload.To != "":
			return fmt.Sprintf("changed title from %q to %q", payload.From, payload.To)
		case payload.To != "":
			return fmt.Sprintf("changed title to %q", payload.To)
		default:
			return "changed title"
		}
	case "pinning-change":
		if payload.Pinned != nil {
			if *payload.Pinned {
				return "pinned the discussion"
			}
			return "unpinned the discussion"
		}
		return "changed pinning"
	case "locking-change":
		if payload.Locked != nil {
			if *payload.Locked {
				return "locked the discussion"
			}
			return "unlocked the discussion"
		}
		return "changed locking"
	case "report-status-change":
		if payload.ReportStatus != "" {
			return "changed report status to " + payload.ReportStatus
		}
		if payload.Status != "" {
			return "changed report status to " + payload.Status
		}
		return "changed report status"
	case "ref-deleted":
		ref := payload.Ref
		if ref == "" {
			ref = payload.RefName
		}
		if ref != "" {
			return "deleted ref " + ref
		}
		return "deleted a ref"
	case "commits-truncated":
		return "additional commits are not shown"
	default:
		if eventType == "" {
			return "event"
		}
		return eventType
	}
}

func shortCommunityOID(oid string) string {
	const max = 12
	if len(oid) > max {
		return oid[:max]
	}
	return oid
}

// communityUpstreamStatus preserves the upstream client-error status for
// statuses the UI can act on and maps everything else to 502.
func communityUpstreamStatus(code int) int {
	switch code {
	case http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusTooManyRequests:
		return code
	default:
		return http.StatusBadGateway
	}
}

func communityUpstreamMessage(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "Community request rejected"
	case http.StatusUnauthorized:
		return "Hugging Face authentication required"
	case http.StatusForbidden:
		return "Access to community content is forbidden"
	case http.StatusNotFound:
		return "Repository or discussion not found"
	case http.StatusTooManyRequests:
		return "Hugging Face rate limit exceeded"
	default:
		return "Community request failed"
	}
}
