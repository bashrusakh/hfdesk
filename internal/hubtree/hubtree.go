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
//     the request URL, requiring the original tree-request origin, and
//     refusing to follow the same target twice;
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
	"errors"
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

// ErrPagination identifies malformed/empty advertised next links, unresolved
// next targets, pagination cycles and origin rejection. It does not classify
// HTTP, transport, decoding, cancellation, budget or initial tree URL failures.
var ErrPagination = errors.New("hubtree: structural pagination failure")

// paginationError marks the known structural failures without changing their
// diagnostic text or losing an underlying URL parse error.
type paginationError struct{ error }

func (e paginationError) Is(target error) bool { return target == ErrPagination }
func (e paginationError) Unwrap() error        { return e.error }

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
	origin      *treeOrigin     // captured once from the first built tree URL
}

// treeOrigin compares URL origins without DNS resolution or path/userinfo
// policy. It does not change the HTTP client's existing redirect behavior.
type treeOrigin struct {
	scheme, hostname, port string
}

func originOf(u *url.URL) treeOrigin {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	} else if n, err := strconv.Atoi(port); err == nil {
		port = strconv.Itoa(n)
	}
	return treeOrigin{scheme: u.Scheme, hostname: u.Hostname(), port: port}
}

func (o treeOrigin) matches(u *url.URL) bool {
	other := originOf(u)
	return o.scheme == other.scheme && strings.EqualFold(o.hostname, other.hostname) && o.port == other.port
}

// walk lists one prefix and recurses into directories in listing order,
// preserving the legacy walker's depth-first interleaving.
func (st *walkState) walk(ctx context.Context, prefix string) error {
	nodes, err := st.listAllPages(ctx, prefix)
	if err != nil {
		return err
	}
	var covered map[string]bool
	for _, n := range nodes {
		switch n.Type {
		case "directory", "tree":
			child, ok := cleanChildDir(prefix, n.Path)
			if !ok {
				// Malformed, empty, "."/"..", absolute, or
				// outside-prefix directory: skip it so a hostile or broken
				// listing cannot steer the walk into a loop.
				continue
			}
			// Only list a directory when no listed descendant proves the
			// server already covered it (i.e. recursive=true worked).
			if covered == nil {
				// Build once from all successful pages, only when queried.
				covered = coveredSubtrees(nodes)
			}
			if covered[child] || st.visited[child] {
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
// requested: empty, absolute, ".", "..", "../"-prefixed after
// cleaning, or not strictly under the prefix being listed.
// Backslashes remain literal URL path characters: traverse those directories
// as before, leaving file validation to the caller rather than hiding files.
func cleanChildDir(prefix, dir string) (string, bool) {
	if dir == "" || strings.HasPrefix(dir, "/") {
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

// coveredSubtrees indexes the raw prefixes before every slash in a complete
// listing. covered[dir] is exactly any(strings.HasPrefix(n.Path, dir+"/")),
// independent of node type. Do not clean paths here: traversal validates the
// directory separately, while coverage has always compared raw server paths.
func coveredSubtrees(nodes []Node) map[string]bool {
	covered := make(map[string]bool)
	for _, n := range nodes {
		for end := strings.LastIndexByte(n.Path, '/'); end >= 0; end = strings.LastIndexByte(n.Path[:end], '/') {
			ancestor := n.Path[:end]
			if covered[ancestor] {
				// Every existing key already has all of its raw ancestors
				// indexed. Reverse traversal avoids repeating shared chains.
				break
			}
			covered[ancestor] = true
		}
	}
	return covered
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

		next, err := nextLink(resp.Header.Values("Link"))
		if err != nil {
			return nil, err
		}
		if next == "" {
			break
		}
		target, err := resolveNext(u, next)
		if err != nil {
			return nil, paginationError{fmt.Errorf("hubtree: invalid next pagination target %q: %w", next, err)}
		}
		if !st.origin.matches(target) {
			// Do not echo attacker-controlled URL text: callers may classify
			// errors by strings such as "401", "unauthorized", or "not found".
			return nil, paginationError{fmt.Errorf("hubtree: cross-origin next pagination target")}
		}
		abs := target.String()
		if st.seenPages[abs] {
			return nil, paginationError{fmt.Errorf("hubtree: pagination cycle at %q", abs)}
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
	if st.origin == nil {
		origin := originOf(u)
		st.origin = &origin
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

// nextLink reads all physical Link fields. Absence of next is normal
// exhaustion; an identifiable but malformed/empty next is an error. Unrelated
// malformed metadata does not invalidate a listing. Single-quoted relations
// remain supported for compatibility with existing mirrors.
func nextLink(headers []string) (string, error) {
	for _, header := range headers {
		for _, entry := range splitLinkParts(header, ',') {
			entry = strings.TrimSpace(entry)
			target, params := "", entry
			validTarget := false
			if strings.HasPrefix(entry, "<") {
				if end := linkTargetEnd(entry[1:]); end >= 0 {
					end++ // account for the opening angle bracket
					target, params = entry[1:end], entry[end+1:]
					validTarget = true
				} else if start := strings.IndexByte(entry, ';'); start >= 0 {
					// Recover parameters only to detect an advertised next in
					// an unclosed target, never to follow that malformed entry.
					params = entry[start:]
				}
			}
			next, validRel := linkHasRelNext(params)
			if !next {
				continue
			}
			if !validTarget || !validRel {
				return "", paginationError{fmt.Errorf("hubtree: malformed next pagination link %q", entry)}
			}
			if strings.TrimSpace(target) == "" {
				return "", paginationError{fmt.Errorf("hubtree: empty next pagination target")}
			}
			return target, nil
		}
	}
	return "", nil
}

// linkTargetEnd finds a closing angle bracket within a URI reference, not in
// a later entry or quoted parameter of a malformed target.
func linkTargetEnd(value string) int {
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '>':
			return i
		case '<', '"':
			return -1
		}
	}
	return -1
}

// splitLinkParts separates entries/parameters using parameter-key and value
// boundaries. Only confirmed entry-start URI targets and quoted VALUES protect
// delimiters; token characters in keys/bare values do not open quotes. Unclosed
// angle targets remain inspectable for advertised-next error recovery.
func splitLinkParts(value string, sep byte) []string {
	var parts []string
	start, keyStart := 0, 0
	entryStart := sep == ','
	for i := 0; i < len(value); i++ {
		c := value[i]
		if entryStart {
			if c == ' ' || c == '\t' {
				continue
			}
			entryStart = false
			if c == '<' {
				if end := linkTargetEnd(value[i+1:]); end >= 0 {
					i += end + 1
					continue
				}
			}
		}
		if c == sep {
			parts = append(parts, value[start:i])
			start = i + 1
		}
		switch c {
		case ';', ',':
			keyStart = i + 1
			entryStart = c == ',' && sep == ','
		case '=':
			key := strings.TrimSpace(value[keyStart:i])
			consumed, _, _ := readLinkValue(value[i+1:], key)
			i += consumed
		}
	}
	return append(parts, value[start:])
}

// readLinkValue consumes a parameter value up to its next unprotected
// delimiter. Standard double-quoted values decode quoted pairs; single quotes
// are a compatibility form only for rel. Apostrophes elsewhere remain legal
// bare-token characters, even at value start. Splitting and relation evaluation
// share this lexical rule so harmless metadata cannot hide pagination.
func readLinkValue(value, key string) (consumed int, decoded string, valid bool) {
	i := 0
	for i < len(value) && (value[i] == ' ' || value[i] == '\t') {
		i++
	}
	var quote byte
	if i < len(value) && (value[i] == '"' || (value[i] == '\'' && strings.EqualFold(strings.TrimSpace(key), "rel"))) {
		quote = value[i]
	}
	if quote == 0 {
		for i < len(value) && value[i] != ';' && value[i] != ',' {
			i++
		}
		return i, strings.TrimSpace(value[:i]), true
	}
	i++
	var text strings.Builder
	closed := false
	for i < len(value) {
		c := value[i]
		i++
		if c == '\\' {
			if i == len(value) {
				break
			}
			text.WriteByte(value[i])
			i++
		} else if c == quote {
			closed = true
			break
		} else {
			text.WriteByte(c)
		}
	}
	endQuote := i
	for i < len(value) && value[i] != ';' && value[i] != ',' {
		i++
	}
	return i, text.String(), closed && strings.TrimSpace(value[endQuote:i]) == ""
}

// linkHasRelNext returns whether next is advertised and whether its relation
// value is well formed. Quoted delimiters in unrelated parameters are ignored.
func linkHasRelNext(params string) (next, valid bool) {
	for _, part := range splitLinkParts(params, ';') {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "rel") {
			continue
		}
		_, decoded, valid := readLinkValue(value, key)
		for _, rel := range strings.Fields(decoded) {
			if rel == "next" {
				return true, valid
			}
		}
	}
	return false, true
}

// resolveNext resolves a pagination target against the URL of the request
// that carried the Link header.
func resolveNext(baseURL, ref string) (*url.URL, error) {
	b, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return nil, err
	}
	return b.ResolveReference(r), nil
}
