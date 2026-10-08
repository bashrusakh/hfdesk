// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

// Package hubtree walks Hugging Face repository trees completely.
//
// The Hub tree API returns at most 1,000 entries per response and puts the
// rest behind a Link rel="next" pagination header, and some custom Endpoint
// mirrors ignore the recursive query parameter entirely. A walk therefore:
//
//   - requests recursive=true so one call returns the whole subtree;
//   - follows Link rel="next" until exhausted, resolving each target against
//     the request URL and refusing to follow the same target twice;
//   - falls back to per-directory listing for mirrors that ignore
//     recursive=true, listing a directory explicitly only when no listed
//     descendant proves the server already covered it;
//   - skips malformed, empty, "." / ".." and outside-prefix directory nodes
//     so a hostile or broken listing cannot loop forever, with an overall
//     request budget as the hard bound;
//   - retries 429/5xx/network errors a bounded number of times, honoring
//     Retry-After (delta-seconds or HTTP date) and the Hub
//     RateLimit: "api";r=0;t=<secs> header (throttling responses only,
//     mirroring pkg/hfdownloader/retry.go), capped and interruptible through
//     context cancellation;
//
// and fails 401/403/404 (and any other terminal status) immediately with
// caller-supplied error text.
//
// This walk is shared by pkg/hfdownloader (plan/download) and pkg/smartdl
// (analyze) so the two tree walkers cannot drift apart again (#96).
package hubtree

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	// maxRetryableAttempts bounds how many times one tree request is issued
	// (the initial attempt plus retries) for 429/5xx/network failures.
	maxRetryableAttempts = 5

	// maxRetryWait caps any single retry wait, including server-requested
	// Retry-After / RateLimit hints, so a hostile or misconfigured mirror
	// cannot park a walk for an unbounded time.
	maxRetryWait = 5 * time.Minute

	// retryBackoffBase is the local backoff before the first retry; it
	// doubles per attempt and is only ever the floor (a larger
	// server-requested wait wins).
	retryBackoffBase = 250 * time.Millisecond
)

// maxWalkRequests caps the total HTTP requests one Walk may issue (pages,
// per-directory listings, and retries together). It is a package variable so
// tests can lower it deterministically, mirroring headForETagTimeout in
// pkg/hfdownloader. Hitting the budget is a loud error, never a silent
// truncation: a legitimate repo walks far below it, while a hostile or
// broken listing that keeps fabricating fresh directories stops here.
var maxWalkRequests = 10000

// LFS contains the LFS metadata a tree node may carry.
type LFS struct {
	Oid    string `json:"oid,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Sha256 string `json:"sha256,omitempty"`
}

// Node is one file or directory entry from the tree API. The JSON shape
// matches pkg/hfdownloader's hfNode and pkg/smartdl's hfTreeNode so both
// callers convert without losing fields.
type Node struct {
	Type   string `json:"type"` // "file"|"directory" (sometimes "blob"|"tree")
	Path   string `json:"path"`
	Size   int64  `json:"size,omitempty"`
	LFS    *LFS   `json:"lfs,omitempty"`
	Sha256 string `json:"sha256,omitempty"`
}

// Walker carries the per-caller configuration for a walk. It is read-only
// during Walk: all mutable walk state lives in a per-Walk private struct so
// concurrent walks never share mutable state.
type Walker struct {
	// TreeURL builds the tree API URL for a directory prefix, without any
	// query string. The walk appends recursive=true itself; pagination
	// targets come from the server's Link header.
	TreeURL func(prefix string) string

	// Token is the Hugging Face access token (sent as a Bearer header).
	Token string

	// UserAgent identifies the caller, preserving each caller's historical
	// User-Agent value.
	UserAgent string

	// Client performs the requests; http.DefaultClient when nil.
	Client *http.Client

	// StatusErr maps a terminal (non-200, non-retried) response to the
	// caller's error text (401/403/404 messages, generic status errors).
	// When nil a generic "tree API failed" error is built.
	StatusErr func(resp *http.Response) error

	// DecodeErr maps a JSON decode failure to the caller's error text.
	// When nil the raw decode error is returned.
	DecodeErr func(err error) error

	// OnResponse, when non-nil, receives every response before status
	// classification (used by pkg/smartdl to capture X-Repo-Commit from the
	// first response that carries it).
	OnResponse func(resp *http.Response)
}

// Walk lists the subtree under prefix, calling fn for every file node.
// Directory nodes are never passed to fn; they are either already covered by
// a recursive listing or explicitly listed by the walk itself. The first fn
// error aborts the walk and is returned unchanged.
func (w *Walker) Walk(ctx context.Context, prefix string, fn func(Node) error) error {
	if w.TreeURL == nil {
		return fmt.Errorf("hubtree: Walker.TreeURL is required")
	}
	st := &walkState{
		w:           w,
		client:      w.Client,
		fn:          fn,
		visited:     make(map[string]bool),
		seenPages:   make(map[string]bool),
		budget:      maxWalkRequests,
		maxRequests: maxWalkRequests,
		recursiveOK: true,
	}
	if st.client == nil {
		st.client = http.DefaultClient
	}
	return st.walk(ctx, prefix)
}

// walkState is the private, per-Walk mutable state.
type walkState struct {
	w           *Walker
	client      *http.Client
	fn          func(Node) error
	visited     map[string]bool // directory prefixes already listed
	seenPages   map[string]bool // pagination targets already followed
	budget      int             // remaining HTTP requests
	maxRequests int             // original budget, for the error message
	recursiveOK bool            // latched false once a server rejects ?recursive
}

// walk lists one prefix and recurses into directories in listing order,
// preserving the legacy walker's depth-first interleaving.
func (st *walkState) walk(ctx context.Context, prefix string) error {
	nodes, err := st.listAllPages(ctx, prefix)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		switch n.Type {
		case "directory", "tree":
			child, ok := cleanChildDir(prefix, n.Path)
			if !ok {
				// Malformed, empty, "."/"..", absolute, backslashed, or
				// outside-prefix directory: skip it so a hostile or broken
				// listing cannot steer the walk into a loop.
				continue
			}
			// Only list a directory when no listed descendant proves the
			// server already covered it (i.e. recursive=true worked).
			if subtreeListed(nodes, child) || st.visited[child] {
				continue
			}
			st.visited[child] = true
			if err := st.walk(ctx, child); err != nil {
				return err
			}
		default:
			if err := st.fn(n); err != nil {
				return err
			}
		}
	}
	return nil
}

// cleanChildDir validates a directory node from a listing and returns the
// cleaned path to list next. It reports false for anything that must not be
// requested: empty, absolute, backslashed, ".", "..", "../"-prefixed after
// cleaning, or not strictly under the prefix being listed.
func cleanChildDir(prefix, dir string) (string, bool) {
	if dir == "" || strings.HasPrefix(dir, "/") || strings.ContainsRune(dir, '\\') {
		return "", false
	}
	cleaned := path.Clean(dir)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	if prefix == "" {
		return cleaned, true
	}
	if !strings.HasPrefix(cleaned, prefix+"/") {
		return "", false
	}
	return cleaned, true
}

// subtreeListed reports whether any node in the listing lies strictly under
// dir, which proves the server returned the subtree (recursive listing
// honored) and an explicit listing of dir would be redundant.
func subtreeListed(nodes []Node, dir string) bool {
	p := dir + "/"
	for _, n := range nodes {
		if strings.HasPrefix(n.Path, p) {
			return true
		}
	}
	return false
}

// listAllPages fetches every page of one prefix's listing, following Link
// rel="next", and returns the concatenated nodes in server order.
func (st *walkState) listAllPages(ctx context.Context, prefix string) ([]Node, error) {
	useRecursive := st.recursiveOK
	u, err := st.pageURL(prefix, useRecursive)
	if err != nil {
		return nil, err
	}
	st.seenPages[u] = true

	var (
		nodes    []Node
		fromLink bool
	)
	for {
		resp, err := st.fetch(ctx, u)
		if err != nil {
			return nil, err
		}

		// A mirror that rejects the query parameter we added gets the
		// legacy no-query per-directory walk instead of a failure, so
		// custom Endpoint mirrors keep working. The downgrade latches for
		// the rest of the walk and only applies to URLs we built ourselves
		// (never to a server-provided pagination target).
		if !fromLink && useRecursive && st.recursiveOK &&
			(resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity) {
			resp.Body.Close()
			st.recursiveOK = false
			useRecursive = false
			if u, err = st.pageURL(prefix, false); err != nil {
				return nil, err
			}
			st.seenPages[u] = true
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, st.statusErr(resp)
		}

		var page []Node
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			return nil, st.decodeErr(err)
		}
		resp.Body.Close()
		nodes = append(nodes, page...)

		next := nextLink(resp.Header.Get("Link"))
		if next == "" {
			break
		}
		abs, err := resolveNext(u, next)
		if err != nil {
			// An unparseable pagination target ends the walk rather than
			// failing it: the header is optional metadata, and the old
			// walker never looked at it at all.
			break
		}
		if st.seenPages[abs] {
			// The server pointed back at a page we already consumed:
			// stop instead of following the cycle forever.
			break
		}
		st.seenPages[abs] = true
		u = abs
		fromLink = true
	}
	return nodes, nil
}

// pageURL builds the tree URL for prefix, appending recursive=true when the
// parameter is enabled.
func (st *walkState) pageURL(prefix string, recursive bool) (string, error) {
	raw := st.w.TreeURL(prefix)
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("hubtree: invalid tree URL %q: %w", raw, err)
	}
	if recursive {
		q := u.Query()
		q.Set("recursive", "true")
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// fetch performs one tree request, retrying 429/5xx statuses and network
// errors a bounded number of times with context-aware waits. It returns the
// terminal response (any status) for the caller to classify, or an error for
// transport failures, cancellation, or an exhausted request budget.
func (st *walkState) fetch(ctx context.Context, u string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		if st.budget <= 0 {
			return nil, fmt.Errorf("hubtree: tree walk exceeded %d requests (breaking a suspected runaway listing)", st.maxRequests)
		}
		st.budget--

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if st.w.Token != "" {
			req.Header.Set("Authorization", "Bearer "+st.w.Token)
		}
		req.Header.Set("User-Agent", st.w.UserAgent)

		resp, err := st.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt+1 >= maxRetryableAttempts {
				return nil, fmt.Errorf("tree request failed after %d attempts: %w", attempt+1, err)
			}
			if !sleepCtx(ctx, backoffFor(attempt)) {
				return nil, ctx.Err()
			}
			continue
		}

		if st.w.OnResponse != nil {
			st.w.OnResponse(resp)
		}
		if !retryableStatus(resp.StatusCode) {
			return resp, nil
		}
		// The last allowed attempt returns the throttled/failed response so
		// the caller surfaces its own terminal error text.
		if attempt+1 >= maxRetryableAttempts {
			return resp, nil
		}
		wait := retryWait(resp, backoffFor(attempt), time.Now())
		resp.Body.Close()
		if !sleepCtx(ctx, wait) {
			return nil, ctx.Err()
		}
	}
}

// statusErr builds the caller's error for a terminal response.
func (st *walkState) statusErr(resp *http.Response) error {
	if st.w.StatusErr != nil {
		return st.w.StatusErr(resp)
	}
	return fmt.Errorf("tree API failed: %s", resp.Status)
}

// decodeErr builds the caller's error for a JSON decode failure.
func (st *walkState) decodeErr(err error) error {
	if st.w.DecodeErr != nil {
		return st.w.DecodeErr(err)
	}
	return err
}

// retryableStatus reports whether a status is transient and worth retrying:
// rate limiting and server errors. It matches RetryableStatus in
// pkg/hfdownloader/retry.go.
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || (code >= 500 && code <= 599)
}

// backoffFor returns the local exponential backoff before the given retry
// attempt (0-based), capped at maxRetryWait.
func backoffFor(attempt int) time.Duration {
	d := retryBackoffBase
	for i := 0; i < attempt && d < maxRetryWait; i++ {
		d *= 2
	}
	if d > maxRetryWait {
		d = maxRetryWait
	}
	return d
}

// retryWait returns how long to wait before the next attempt: the larger of
// the local backoff and any server-requested wait, capped at maxRetryWait.
func retryWait(resp *http.Response, local time.Duration, now time.Time) time.Duration {
	wait := local
	if hint := serverHint(resp, now); hint > wait {
		wait = hint
	}
	if wait > maxRetryWait {
		wait = maxRetryWait
	}
	return wait
}

// serverHint returns the wait a throttling response (429/503) asks for,
// honoring both Retry-After (delta-seconds or HTTP date) and the Hugging Face
// Hub RateLimit header (for example `RateLimit: "api";r=0;t=30`). Other
// statuses may carry a stray Retry-After that must not change scheduling, so
// they return 0. When both headers are present the larger wait wins. This
// mirrors RetryAfterFromResponse in pkg/hfdownloader/retry.go (which cannot
// be imported here without an import cycle).
func serverHint(resp *http.Response, now time.Time) time.Duration {
	if resp == nil {
		return 0
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
	default:
		return 0
	}
	wait := parseRetryAfter(resp.Header.Get("Retry-After"), now)
	if rl := parseRateLimit(resp.Header.Get("RateLimit")); rl > wait {
		wait = rl
	}
	return wait
}

// parseRetryAfter parses a Retry-After header value: non-negative
// delta-seconds or an HTTP date. Unparseable, negative, or already-past
// values return 0.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return secondsToDuration(secs)
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// parseRateLimit parses the Hugging Face Hub RateLimit header, a
// semicolon-separated directive list such as `"api";r=0;t=30`. The `t` field
// is the wait in seconds; `t="0"` or a missing/unparseable `t` means no wait.
func parseRateLimit(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	for _, part := range strings.Split(v, ";") {
		part = strings.TrimSpace(part)
		key, val, found := strings.Cut(part, "=")
		if !found || strings.TrimSpace(key) != "t" {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"`)
		secs, err := strconv.Atoi(val)
		if err != nil {
			return 0
		}
		return secondsToDuration(secs)
	}
	return 0
}

// secondsToDuration converts a seconds count to a time.Duration, saturating
// at the maximum representable duration instead of overflowing.
func secondsToDuration(secs int) time.Duration {
	if secs <= 0 {
		return 0
	}
	const maxSeconds = int64(1<<63-1) / int64(time.Second)
	if int64(secs) >= maxSeconds {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(secs) * time.Second
}

// sleepCtx waits for d and returns false if ctx is canceled first. It matches
// sleepCtx in pkg/hfdownloader/utils.go.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// nextLink extracts the rel="next" target from a Link header. Link entries
// are scanned as `<url>; params` groups (URLs only ever appear inside angle
// brackets, so parameters can be read up to the next "<"). Returns "" when
// there is no next link.
func nextLink(header string) string {
	for i := 0; i < len(header); {
		start := strings.IndexByte(header[i:], '<')
		if start < 0 {
			return ""
		}
		start += i
		end := strings.IndexByte(header[start:], '>')
		if end < 0 {
			return ""
		}
		end += start
		target := header[start+1 : end]

		params := header[end:]
		if nxt := strings.IndexByte(params, '<'); nxt >= 0 {
			params = params[:nxt]
		}
		// The comma between link entries is not part of the preceding
		// relation value. Leave commas inside targets/quoted params intact.
		params = strings.TrimSuffix(strings.TrimSpace(params), ",")
		if linkHasRelNext(params) {
			return target
		}
		i = end + 1
	}
	return ""
}

// linkHasRelNext reports whether a Link entry's parameter section declares
// rel="next", rel='next', or rel=next.
func linkHasRelNext(params string) bool {
	for _, part := range strings.Split(params, ";") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "rel=") {
			continue
		}
		v := strings.TrimPrefix(part, "rel=")
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		for _, rel := range strings.Fields(v) {
			if rel == "next" {
				return true
			}
		}
	}
	return false
}

// resolveNext resolves a pagination target against the URL of the request
// that carried the Link header.
func resolveNext(baseURL, ref string) (string, error) {
	b, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}
