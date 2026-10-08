// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hubtree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fileNode(p string, size int64) Node {
	return Node{Type: "file", Path: p, Size: size}
}

func dirNode(p string) Node {
	return Node{Type: "directory", Path: p}
}

// testWalker builds a Walker against a fake Hub rooted at the standard model
// tree path.
func testWalker(srv *httptest.Server, statusErr func(*http.Response) error) *Walker {
	return &Walker{
		TreeURL: func(prefix string) string {
			u := srv.URL + "/api/models/o/r/tree/main"
			if prefix != "" {
				u += "/" + prefix
			}
			return u
		},
		UserAgent: "hubtree-test/1",
		StatusErr: statusErr,
	}
}

// walkFiles runs a walk and returns the file paths passed to fn.
func walkFiles(t *testing.T, w *Walker) []string {
	t.Helper()
	var got []string
	err := w.Walk(context.Background(), "", func(n Node) error {
		if n.Type == "file" {
			got = append(got, n.Path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return got
}

func assertFiles(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d files %v, want %d %v", len(got), got, len(want), want)
	}
	set := make(map[string]bool, len(got))
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("missing file %q (got %v)", w, got)
		}
	}
}

// TestWalk_FollowsPagination is the issue #96 core case: a single directory
// whose listing spans more than two pages behind Link rel="next" must yield
// every file, with recursive=true on every request and the relative next
// target resolved against the request URL.
func TestWalk_FollowsPagination(t *testing.T) {
	const pageSize = 4
	var (
		treeReqs   atomic.Int64
		recursive  atomic.Int64
		cursorReqs atomic.Int64
		totalNodes int
	)
	// Three directories flattened into one recursive listing: 3 dir nodes +
	// 12 files = 15 nodes -> 4 pages at pageSize 4.
	var all []Node
	for d := 0; d < 3; d++ {
		all = append(all, dirNode(fmt.Sprintf("d%d", d)))
		for f := 0; f < 4; f++ {
			all = append(all, fileNode(fmt.Sprintf("d%d/f%d.bin", d, f), int64(d*10+f)))
		}
	}
	totalNodes = len(all)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/tree/") {
			http.NotFound(w, r)
			return
		}
		treeReqs.Add(1)
		if r.URL.Query().Get("recursive") == "true" {
			recursive.Add(1)
		}
		cursor := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			n, err := strconv.Atoi(c)
			if err != nil {
				t.Errorf("bad cursor %q: %v", c, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			cursor = n
			cursorReqs.Add(1)
		}
		end := cursor + pageSize
		if end > totalNodes {
			end = totalNodes
		}
		if end < totalNodes {
			// Deliberately a relative target: the walker must resolve it
			// against the request URL.
			w.Header().Set("Link", fmt.Sprintf("<%s?recursive=true&cursor=%d>; rel=\"next\"", r.URL.Path, end))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(all[cursor:end])
	}))
	defer srv.Close()

	got := walkFiles(t, testWalker(srv, nil))

	want := []string{
		"d0/f0.bin", "d0/f1.bin", "d0/f2.bin", "d0/f3.bin",
		"d1/f0.bin", "d1/f1.bin", "d1/f2.bin", "d1/f3.bin",
		"d2/f0.bin", "d2/f1.bin", "d2/f2.bin", "d2/f3.bin",
	}
	assertFiles(t, got, want...)

	if n := treeReqs.Load(); n != 4 {
		t.Errorf("tree requests = %d, want 4 paginated pages (no per-directory extras)", n)
	}
	if n := recursive.Load(); n != 4 {
		t.Errorf("recursive=true on %d requests, want 4", n)
	}
	if n := cursorReqs.Load(); n != 3 {
		t.Errorf("cursor (Link next) requests = %d, want 3", n)
	}
}

func TestNextLink_MultipleRelations(t *testing.T) {
	for _, tc := range []struct {
		name, header, want string
	}{
		{"next first", `<?cursor=2>; rel="next", <?cursor=1>; rel="prev"`, "?cursor=2"},
		{"next last", `<?cursor=1>; rel="prev", <?cursor=2>; rel="next"`, "?cursor=2"},
		{"unquoted next first", `<?cursor=2>; rel=next, <?cursor=1>; rel=prev`, "?cursor=2"},
		{"single quoted next first", `<?cursor=2>; rel='next', <?cursor=1>; rel='prev'`, "?cursor=2"},
		{"relation list", `<?cursor=2>; rel="next alternate", <?cursor=1>; rel="prev"`, "?cursor=2"},
		{"commas inside target and title", `<?cursor=a,b>; title="page, two"; rel="next", <?cursor=1>; rel="prev"`, "?cursor=a,b"},
		{"quoted false relation and brackets", `<?cursor=1>; title="ignore;rel=next,<fake>"; rel=prev, <?cursor=2>; rel=next`, "?cursor=2"},
		{"escaped quoted delimiters", `<?cursor=1>; title="ignore\";rel=next,<fake>"; rel=prev, <?cursor=2>; rel=next`, "?cursor=2"},
		{"quoted false next only", `<?cursor=1>; title="ignore;rel=next,<fake>"; rel=prev`, ""},
		{"malformed prev before next", `<broken; rel=prev, <?cursor=2>; rel=next`, "?cursor=2"},
		{"parameter whitespace", `<?cursor=2>; rel = "next"`, "?cursor=2"},
		{"no next", `<?cursor=1>; rel="prev", <?cursor=3>; rel="last"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := nextLink([]string{tc.header}); err != nil || got != tc.want {
				t.Fatalf("nextLink(%q) = %q, %v, want %q", tc.header, got, err, tc.want)
			}
		})
	}
}

func TestNextLink_AdvertisedMalformedNext(t *testing.T) {
	for _, header := range []string{
		`<>; rel=next`,
		`<   >; rel="next"`,
		`<?cursor=2; rel=next`,
		`<?cursor=2; rel=next; title="a>b"`,
		`?cursor=2; rel=next`,
		`<?cursor=2>; rel="next`,
	} {
		t.Run(header, func(t *testing.T) {
			if next, err := nextLink([]string{header}); err == nil {
				t.Fatalf("nextLink = %q, nil; want advertised-next error", next)
			}
		})
	}
}

// Apostrophes are legal token characters, not context-free string delimiters.
// Only parameter value starts can establish quoted metadata/relations.
func TestNextLink_ValueBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []string
		want    string
		wantErr bool
	}{
		{name: "reported prev first", headers: []string{`<?cursor=0>; title=owner's; rel=prev, <?cursor=2>; rel=next`}, want: "?cursor=2"},
		{name: "reported next only", headers: []string{`<?cursor=2>; title=owner's; rel=next`}, want: "?cursor=2"},
		{name: "leading apostrophe token", headers: []string{`<?cursor=2>; title='owners; rel=next`}, want: "?cursor=2"},
		{name: "paired apostrophes in token", headers: []string{`<?cursor=2>; title='owners'; rel=next`}, want: "?cursor=2"},
		{name: "interior apostrophe key", headers: []string{`<?cursor=2>; owner's=metadata; rel=next`}, want: "?cursor=2"},
		{name: "leading apostrophe key", headers: []string{`<?cursor=2>; 'owners=metadata; rel=next`}, want: "?cursor=2"},
		{name: "quoted value after apostrophe key", headers: []string{`<?cursor=0>; owner's="ignore,;rel=next,<fake>"; rel=prev, <?cursor=2>; rel=next`}, want: "?cursor=2"},
		{name: "quoted value escaping", headers: []string{`<?cursor=0>; title="owner's \\path\"; rel=next,<fake>"; rel=prev, <?cursor=2>; rel=next`}, want: "?cursor=2"},
		{name: "URI delimiters and apostrophe", headers: []string{`<?cursor=2&note=owner's,a;b>; rel=next`}, want: "?cursor=2&note=owner's,a;b"},
		{name: "bare relation", headers: []string{`<?cursor=2>; rel=next`}, want: "?cursor=2"},
		{name: "double quoted relation list", headers: []string{`<?cursor=2>; rel="alternate next"`}, want: "?cursor=2"},
		{name: "single quoted relation list", headers: []string{`<?cursor=2>; rel='alternate next'`}, want: "?cursor=2"},
		{name: "single quoted relation whitespace", headers: []string{`<?cursor=2>; rel = 'alternate next'`}, want: "?cursor=2"},
		{name: "escaped relation", headers: []string{`<?cursor=2>; rel="ne\xt"`}, want: "?cursor=2"},
		{name: "unclosed unrelated quote resets at field", headers: []string{`<?cursor=0>; title="unfinished`, `<?cursor=2>; rel=next`}, want: "?cursor=2"},
		{name: "metadata with apostrophe no next", headers: []string{`<?cursor=0>; title=owner's; rel=prev`}},
		{name: "URI-looking quoted metadata no next", headers: []string{`<?cursor=0>; title="<fake>;rel=next,other"; rel=prev`}},
		{name: "malformed prev with apostrophe", headers: []string{`<broken; title=owner's; rel=prev, <?cursor=2>; rel=next`}, want: "?cursor=2"},
		{name: "malformed next with apostrophe", headers: []string{`<broken; title=owner's; rel=next`}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nextLink(tc.headers)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("nextLink = %q, %v; want %q, error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestWalk_TargetDelimitersRemainLiteral(t *testing.T) {
	var reqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := reqs.Add(1)
		if page == 1 {
			w.Header().Set("Link", `<page,owner's;metadata?cursor=2>; rel=next`)
		} else if page != 2 || r.URL.Path != "/api/models/o/r/tree/page,owner's;metadata" || r.URL.Query().Get("cursor") != "2" {
			t.Errorf("unexpected pagination request: %s", r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode([]Node{fileNode(fmt.Sprintf("f%d.bin", page), page)})
	}))
	defer srv.Close()
	assertFiles(t, walkFiles(t, testWalker(srv, nil)), "f1.bin", "f2.bin")
	if got := reqs.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

// Multi-link headers must deliver every page regardless of relation order,
// not merely produce a correctly parsed target in isolation.
func TestWalk_MultipleLinkRelationsReturnsEveryFile(t *testing.T) {
	for _, nextFirst := range []bool{true, false} {
		name := "next last"
		if nextFirst {
			name = "next first"
		}
		t.Run(name, func(t *testing.T) {
			var reqs atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reqs.Add(1)
				if r.URL.Query().Get("recursive") != "true" {
					t.Error("recursive=true missing from page request")
				}
				page := 1
				if cursor := r.URL.Query().Get("cursor"); cursor != "" {
					var err error
					page, err = strconv.Atoi(cursor)
					if err != nil || page < 1 || page > 3 {
						t.Errorf("unexpected cursor %q", cursor)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				}
				prev := fmt.Sprintf("<?recursive=true&cursor=%d>; rel=\"prev\"", page-1)
				if page < 3 {
					next := fmt.Sprintf("<?recursive=true&cursor=%d>; rel=\"next\"", page+1)
					if nextFirst {
						w.Header().Set("Link", next+", "+prev)
					} else {
						w.Header().Set("Link", prev+", "+next)
					}
				} else {
					w.Header().Set("Link", prev)
				}
				_ = json.NewEncoder(w).Encode([]Node{fileNode(fmt.Sprintf("f%d.bin", page), int64(page))})
			}))
			defer srv.Close()

			got := walkFiles(t, testWalker(srv, nil))
			assertFiles(t, got, "f1.bin", "f2.bin", "f3.bin")
			if n := reqs.Load(); n != 3 {
				t.Errorf("requests = %d, want exactly 3 pages (no prev requests)", n)
			}
		})
	}
}

// TestWalk_MirrorIgnoresRecursive covers mirrors that ignore recursive=true
// and serve one level per call: the walker must fall back to explicit
// per-directory listing and still return every file.
func TestWalk_MirrorIgnoresRecursive(t *testing.T) {
	levels := map[string][]Node{
		"":         {fileNode("a.bin", 1), dirNode("sub")},
		"sub":      {fileNode("sub/b.bin", 2), dirNode("sub/deep")},
		"sub/deep": {fileNode("sub/deep/c.bin", 3)},
	}
	var (
		treeReqs        atomic.Int64
		recursiveOnRoot atomic.Bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/tree/") {
			http.NotFound(w, r)
			return
		}
		treeReqs.Add(1)
		if r.URL.Query().Get("recursive") == "true" && !r.URL.Query().Has("cursor") &&
			strings.HasSuffix(r.URL.Path, "/tree/main") {
			recursiveOnRoot.Store(true)
		}
		prefix := strings.TrimPrefix(r.URL.Path, "/api/models/o/r/tree/main")
		prefix = strings.TrimPrefix(prefix, "/")
		nodes, ok := levels[prefix]
		if !ok {
			// The fallback must only ask for directories that exist.
			t.Errorf("unexpected listing prefix %q", prefix)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(nodes)
	}))
	defer srv.Close()

	got := walkFiles(t, testWalker(srv, nil))
	assertFiles(t, got, "a.bin", "sub/b.bin", "sub/deep/c.bin")

	if n := treeReqs.Load(); n != 3 {
		t.Errorf("tree requests = %d, want 3 (root + sub + sub/deep)", n)
	}
	if !recursiveOnRoot.Load() {
		t.Error("walker never sent recursive=true; fallback ran without trying the recursive listing first")
	}
}

// TestWalk_MalformedDirectoriesTerminate pins the hostile-listing guard:
// empty, ".", "..", "../x", "/", and self-echoing directory nodes
// must not be requested or looped over, while valid directories still list.
func TestWalk_MalformedDirectoriesTerminate(t *testing.T) {
	// Backslash directories are not malformed for traversal: skipping them
	// would suppress the caller's existing unsafe-FILE validation. Their empty
	// and unsafe-child cases are covered through PlanRepo below the adapter.
	malformed := []string{"", ".", "..", "../x", "/", "a/.."}
	var treeReqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/tree/") {
			http.NotFound(w, r)
			return
		}
		treeReqs.Add(1)
		prefix := strings.TrimPrefix(r.URL.Path, "/api/models/o/r/tree/main")
		prefix = strings.TrimPrefix(prefix, "/")
		var nodes []Node
		switch prefix {
		case "":
			nodes = append(nodes, fileNode("ok1.bin", 1))
			for _, m := range malformed {
				nodes = append(nodes, dirNode(m))
			}
			nodes = append(nodes, dirNode("good"), dirNode("good"))
		case "good":
			// Self-echo: a listing that returns the directory being listed.
			nodes = []Node{dirNode("good"), dirNode("elsewhere"), dirNode("good/.."), fileNode("good/ok2.bin", 2)}
		default:
			t.Errorf("walker requested malformed/outside directory %q", prefix)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(nodes)
	}))
	defer srv.Close()

	got := walkFiles(t, testWalker(srv, nil))
	assertFiles(t, got, "ok1.bin", "good/ok2.bin")

	if n := treeReqs.Load(); n != 2 {
		t.Errorf("tree requests = %d, want 2 (root + good, no repeats or malformed/outside-prefix)", n)
	}
}

// Cancellation must reach the actual next-page/directory request, including
// restored backslash traversal; silently skipping that traversal is not success.
func TestWalk_CancelDuringFollowupRequest(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprintf("directory=%v", directory), func(t *testing.T) {
			var reqs atomic.Int64
			entered := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if reqs.Add(1) == 1 {
					if directory {
						_ = json.NewEncoder(w).Encode([]Node{dirNode(`win\dir`)})
					} else {
						w.Header().Set("Link", `<?cursor=2>; rel=next`)
						_ = json.NewEncoder(w).Encode([]Node{fileNode("a.bin", 1)})
					}
					return
				}
				if directory && !strings.Contains(r.URL.EscapedPath(), "win%5Cdir") {
					t.Errorf("unexpected directory URL: %s", r.URL)
				}
				close(entered)
				<-r.Context().Done()
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			canceled := make(chan struct{})
			go func() {
				defer close(canceled)
				select {
				case <-entered:
				case <-ctx.Done():
				}
				cancel()
			}()
			defer func() { cancel(); <-canceled }()
			err := testWalker(srv, nil).Walk(ctx, "", func(Node) error { return nil })
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			if got := reqs.Load(); got != 2 {
				t.Errorf("requests = %d, want 2", got)
			}
		})
	}
}

// TestWalk_RetriesRateLimit verifies a 429 is retried after actually waiting
// out the server-requested RateLimit hint (t=1 second), then succeeds.
func TestWalk_RetriesRateLimit(t *testing.T) {
	var reqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqs.Add(1) == 1 {
			w.Header().Set("RateLimit", `"api";r=0;t=1`)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Node{fileNode("f.bin", 7)})
	}))
	defer srv.Close()

	start := time.Now()
	got := walkFiles(t, testWalker(srv, nil))
	elapsed := time.Since(start)

	assertFiles(t, got, "f.bin")
	if n := reqs.Load(); n != 2 {
		t.Errorf("requests = %d, want 2 (one 429 + one success)", n)
	}
	// The RateLimit hint asked for 1s; a walk that ignored it would return
	// in milliseconds (local backoff floor is 250ms).
	if elapsed < 900*time.Millisecond {
		t.Errorf("walk returned in %v; RateLimit wait was not honored", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Errorf("walk took %v; retry wait not bounded", elapsed)
	}
}

// TestRetryWaitParsesServerHints pins the wait computation without sleeping:
// Retry-After in seconds and HTTP-date form, the Hub RateLimit header, the
// larger-wins rule, throttling-only parsing, and the cap.
func TestRetryWaitParsesServerHints(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mk := func(code int, hdr map[string]string) *http.Response {
		resp := &http.Response{StatusCode: code, Header: http.Header{}}
		for k, v := range hdr {
			resp.Header.Set(k, v)
		}
		return resp
	}
	cases := []struct {
		name  string
		resp  *http.Response
		local time.Duration
		want  time.Duration
	}{
		{"retry-after seconds", mk(429, map[string]string{"Retry-After": "2"}), 0, 2 * time.Second},
		{"retry-after http-date", mk(429, map[string]string{"Retry-After": now.Add(3 * time.Second).Format(http.TimeFormat)}), 0, 3 * time.Second},
		{"rate-limit header", mk(429, map[string]string{"RateLimit": `"api";r=0;t=7`}), 0, 7 * time.Second},
		{"larger hint wins", mk(429, map[string]string{"Retry-After": "2", "RateLimit": `"api";r=0;t=7`}), 0, 7 * time.Second},
		{"local backoff wins", mk(429, map[string]string{"Retry-After": "1"}), 10 * time.Second, 10 * time.Second},
		{"503 throttling honored", mk(503, map[string]string{"Retry-After": "4"}), 0, 4 * time.Second},
		{"500 stray retry-after ignored", mk(500, map[string]string{"Retry-After": "9"}), 500 * time.Millisecond, 500 * time.Millisecond},
		{"404 ignored", mk(404, map[string]string{"Retry-After": "9"}), 0, 0},
		{"past date ignored", mk(429, map[string]string{"Retry-After": now.Add(-time.Minute).Format(http.TimeFormat)}), 0, 0},
		{"zero hint", mk(429, map[string]string{"Retry-After": "0"}), 0, 0},
		{"unparseable", mk(429, map[string]string{"Retry-After": "soon"}), 0, 0},
		{"capped", mk(429, map[string]string{"Retry-After": "400000"}), 0, maxRetryWait},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := retryWait(c.resp, c.local, now); got != c.want {
				t.Errorf("retryWait = %v, want %v", got, c.want)
			}
		})
	}
}

// TestWalk_PermanentErrorNoRetry pins fail-fast behavior: a 401 is surfaced
// through the caller's StatusErr exactly once, with no retry loop.
func TestWalk_PermanentErrorNoRetry(t *testing.T) {
	sentinel := errors.New("401 unauthorized: repo requires token or you do not have access")

	t.Run("caller error", func(t *testing.T) {
		var reqs atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqs.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		err := testWalker(srv, func(*http.Response) error { return sentinel }).Walk(
			context.Background(), "", func(Node) error { return nil })
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want sentinel", err)
		}
		if n := reqs.Load(); n != 1 {
			t.Errorf("requests = %d, want 1 (no retry on 401)", n)
		}
	})

	t.Run("default message", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		err := testWalker(srv, nil).Walk(context.Background(), "", func(Node) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "tree API failed: 401") {
			t.Fatalf("err = %v, want default 401 status error", err)
		}
	})
}

// TestWalk_CancelDuringBackoff verifies context cancellation interrupts a
// server-requested retry wait promptly instead of sleeping it out.
func TestWalk_CancelDuringBackoff(t *testing.T) {
	var reqs atomic.Int64
	var once sync.Once
	served := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		w.Header().Set("RateLimit", `"api";r=0;t=60`)
		w.WriteHeader(http.StatusTooManyRequests)
		once.Do(func() { close(served) })
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-served
		// Let the client receive the 429 and enter its 60s wait, then cancel.
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := testWalker(srv, nil).Walk(ctx, "", func(Node) error { return nil })
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("cancellation took %v; backoff wait was not interruptible", elapsed)
	}
	if n := reqs.Load(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

// TestWalk_RejectsRecursiveFallsBackToPlainListing pins the custom-Endpoint
// mirror contract: a server that rejects the recursive query parameter with
// 400 or 422 must not break the walk — the walker drops the parameter it added
// (latching for the whole walk) and keeps the URL path shape otherwise
// unchanged.
func TestWalk_RejectsRecursiveFallsBackToPlainListing(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			levels := map[string][]Node{
				"":    {fileNode("a.bin", 1), dirNode("sub")},
				"sub": {fileNode("sub/b.bin", 2)},
			}
			var (
				recursiveReqs atomic.Int64
				totalReqs     atomic.Int64
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "/tree/") {
					http.NotFound(w, r)
					return
				}
				totalReqs.Add(1)
				if r.URL.Query().Has("recursive") {
					recursiveReqs.Add(1)
					w.WriteHeader(status)
					return
				}
				prefix := strings.TrimPrefix(r.URL.Path, "/api/models/o/r/tree/main")
				prefix = strings.TrimPrefix(prefix, "/")
				nodes, ok := levels[prefix]
				if !ok {
					t.Errorf("unexpected listing prefix %q", prefix)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(nodes)
			}))
			defer srv.Close()

			got := walkFiles(t, testWalker(srv, nil))
			assertFiles(t, got, "a.bin", "sub/b.bin")

			if n := recursiveReqs.Load(); n != 1 {
				t.Errorf("requests carrying recursive param = %d, want 1 (downgrade latches)", n)
			}
			// The rejected recursive root request, the plain root retry, and sub.
			if n := totalReqs.Load(); n != 3 {
				t.Errorf("total requests = %d, want 3 (rejected recursive root + plain root + sub)", n)
			}
		})
	}
}

// TestWalk_RequestBudgetBoundsRunawayListing proves the hard bound: a hostile
// server that answers every listing with one fresh, perfectly valid child
// directory is stopped at the request budget with a loud error instead of
// walking forever.
func TestWalk_RequestBudgetBoundsRunawayListing(t *testing.T) {
	old := maxWalkRequests
	maxWalkRequests = 50
	defer func() { maxWalkRequests = old }()

	var reqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/tree/") {
			http.NotFound(w, r)
			return
		}
		n := reqs.Add(1)
		prefix := strings.TrimPrefix(r.URL.Path, "/api/models/o/r/tree/main")
		prefix = strings.TrimPrefix(prefix, "/")
		child := fmt.Sprintf("n%d", n)
		if prefix != "" {
			child = prefix + "/" + child
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Node{dirNode(child)})
	}))
	defer srv.Close()

	err := testWalker(srv, nil).Walk(context.Background(), "", func(Node) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("err = %v, want request-budget error", err)
	}
	if n := reqs.Load(); n != 50 {
		t.Errorf("requests = %d, want exactly the budget (50)", n)
	}
}

// TestWalk_NetworkErrorRetried verifies a transient transport failure is
// retried within the bounded attempt budget and then succeeds.
func TestWalk_NetworkErrorRetried(t *testing.T) {
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqs.Add(1) == 1 {
			// Drop the connection before responding: a network error.
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
			t.Error("hijack failed; cannot simulate a network error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Node{fileNode("f.bin", 7)})
	}))
	defer srv.Close()

	start := time.Now()
	got := walkFiles(t, testWalker(srv, nil))
	elapsed := time.Since(start)

	assertFiles(t, got, "f.bin")
	if n := reqs.Load(); n != 2 {
		t.Errorf("requests = %d, want 2 (failed attempt + success)", n)
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("walk returned in %v; local backoff before network retry not applied", elapsed)
	}
}
