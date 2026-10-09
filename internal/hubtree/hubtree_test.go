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
	"net/url"
	"sort"
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

func TestWalk_PaginationErrorScope(t *testing.T) {
	for _, tc := range []struct {
		name, link, diagnostic string
		structural             bool
	}{
		{"invalid next", `</401/%zz>; rel=next`, "hubtree: invalid next pagination target", true},
		{"malformed next", `</unauthorized; rel=next`, "hubtree: malformed next pagination link", true},
		{"empty next", `<>; rel=next`, "hubtree: empty next pagination target", true},
		{"cycle", `<?recursive=true>; rel=next`, "hubtree: pagination cycle at", true},
		{"origin", `<http://foreign.invalid/401>; rel=next`, "hubtree: cross-origin next pagination target", true},
		{"401", "", "unauthorized", false},
		{"403", "", "forbidden", false},
		{"404", "", "not found", false},
		{"decode", "", "unexpected EOF", false},
		{"initial URL", "", "hubtree: invalid tree URL", false},
		{"cancel", "", "context canceled", false},
		{"budget", "", "hubtree: tree walk exceeded", false},
		{"network", "", "tree request failed after 5 attempts", false},
		{"callback", "", "callback 401 unauthorized not found", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.name == "401" || tc.name == "403" || tc.name == "404" {
					status, _ := strconv.Atoi(tc.name)
					w.WriteHeader(status)
					return
				}
				if tc.name == "decode" {
					_, _ = w.Write([]byte(`[{`))
					return
				}
				w.Header().Set("Link", tc.link)
				_ = json.NewEncoder(w).Encode([]Node{fileNode("data.txt", 1)})
			}))
			defer srv.Close()
			walker := testWalker(srv, nil)
			walker.StatusErr = func(resp *http.Response) error {
				return errors.New(map[int]string{401: "unauthorized", 403: "forbidden", 404: "not found"}[resp.StatusCode])
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch tc.name {
			case "initial URL":
				walker.TreeURL = func(string) string { return srv.URL + "/401/%zz" }
			case "cancel":
				cancel()
			case "budget":
				old := maxWalkRequests
				maxWalkRequests = 0
				defer func() { maxWalkRequests = old }()
			case "network":
				srv.Close()
			}
			callbackErr := errors.New(tc.diagnostic)
			err := walker.Walk(ctx, "", func(Node) error {
				if tc.name == "callback" {
					return callbackErr
				}
				t.Error("failed listing delivered a file")
				return nil
			})
			if err == nil || !strings.HasPrefix(err.Error(), tc.diagnostic) {
				t.Fatalf("error = %v, want diagnostic prefix %q", err, tc.diagnostic)
			}
			if errors.Is(err, ErrPagination) != tc.structural || errors.Is(fmt.Errorf("outer: %w", err), ErrPagination) != tc.structural {
				t.Errorf("error = %v, structural = %v; want %v through wrapping", err, errors.Is(err, ErrPagination), tc.structural)
			}
			if tc.name == "invalid next" {
				var escape url.EscapeError
				if !errors.As(err, &escape) {
					t.Errorf("lost underlying URL error: %v", err)
				}
			}
			if tc.name == "callback" && err != callbackErr {
				t.Errorf("callback error identity changed: %v", err)
			}
		})
	}
}

// Advertised links are fresh requests, not redirects: reject the destination
// before any request, even without credentials and after traversal transitions.
func TestWalk_PaginationOriginBoundary(t *testing.T) {
	for _, token := range []string{"", "fake-token"} {
		for _, stage := range []string{"first page", "later page", "directory", "400 fallback", "422 fallback", "retry", "https downgrade"} {
			t.Run(stage+"/token="+token, func(t *testing.T) {
				var foreign, source atomic.Int64
				dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					foreign.Add(1)
					_ = json.NewEncoder(w).Encode([]Node{fileNode("foreign.bin", 1)})
				}))
				defer dst.Close()
				srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := source.Add(1)
					wantAuth := ""
					if token != "" {
						wantAuth = "Bearer " + token
					}
					if r.Header.Get("Authorization") != wantAuth {
						t.Error("source authorization changed")
					}
					if n == 1 {
						switch stage {
						case "later page":
							w.Header().Set("Link", `<?cursor=2>; rel=next`)
							_ = json.NewEncoder(w).Encode([]Node{fileNode("first.bin", 1)})
							return
						case "directory":
							_ = json.NewEncoder(w).Encode([]Node{dirNode("sub")})
							return
						case "400 fallback", "422 fallback":
							status := http.StatusBadRequest
							if stage == "422 fallback" {
								status = http.StatusUnprocessableEntity
							}
							w.WriteHeader(status)
							return
						case "retry":
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
					}
					w.Header().Set("Link", "<"+dst.URL+"/401/unauthorized/not%20found>; rel=next")
					_ = json.NewEncoder(w).Encode([]Node{fileNode("local.bin", 1)})
				}))
				if stage == "https downgrade" {
					srv.StartTLS()
				} else {
					srv.Start()
				}
				defer srv.Close()
				walker := testWalker(srv, nil)
				walker.Client, walker.Token = srv.Client(), token
				err := walker.Walk(context.Background(), "", func(Node) error {
					t.Error("failed listing delivered a partial page")
					return nil
				})
				if err == nil || err.Error() != "hubtree: cross-origin next pagination target" {
					t.Errorf("error = %v, want stable cross-origin error without target text", err)
				}
				if got := foreign.Load(); got != 0 {
					t.Errorf("foreign requests = %d, want zero", got)
				}
				wantSource := int64(2)
				if stage == "first page" || stage == "https downgrade" {
					wantSource = 1
				}
				if got := source.Load(); got != wantSource {
					t.Errorf("source requests = %d, want %d", got, wantSource)
				}
			})
		}
	}
}

func dirNode(p string) Node {
	return Node{Type: "directory", Path: p}
}

// Keep the original production predicate as the differential/benchmark oracle.
func scanSubtreeListed(nodes []Node, dir string) bool {
	p := dir + "/"
	for _, n := range nodes {
		if strings.HasPrefix(n.Path, p) {
			return true
		}
	}
	return false
}

func TestCoveredSubtrees_EquivalentToOriginalScan(t *testing.T) {
	paths := []string{"", "a", "a/", "a//", "a//b/file", "a/./b/file", "a/../b/file", `win\dir/file`, "deep/p/q/file", "A/file", "a2/file", "/a/file"}
	atoms := []string{"", "a", "a2", "A", ".", "..", `win\dir`}
	for _, a := range atoms {
		for _, b := range atoms {
			for _, c := range atoms {
				paths = append(paths, a+"/"+b+"/"+c)
			}
		}
	}
	candidates := map[string]bool{"unlisted": true}
	for _, p := range paths {
		candidates[p] = true
		// Include the exact cleaned queries made by traversal, not just
		// the raw paths used to build the index.
		for _, prefix := range []string{"", "a", "a2", `win\dir`} {
			if child, ok := cleanChildDir(prefix, p); ok {
				candidates[child] = true
			}
		}
	}
	var dirs []string
	for dir := range candidates {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	checks := 0
	check := func(nodes []Node, queries []string) {
		t.Helper()
		covered := coveredSubtrees(nodes)
		for _, dir := range queries {
			checks++
			if got, want := covered[dir], scanSubtreeListed(nodes, dir); got != want {
				t.Fatalf("coverage[%q] = %v, want scan %v; nodes = %v", dir, got, want, nodes)
			}
		}
	}
	check(nil, dirs)
	types := []string{"file", "directory", "tree", "blob", "unknown", ""}
	var all []Node
	for _, p := range paths {
		for _, typ := range types {
			// Singleton cases cannot have another node accidentally supply
			// coverage; every type must prove the same raw prefix predicate.
			n := Node{Type: typ, Path: p}
			check([]Node{n}, dirs)
			all = append(all, n)
		}
	}
	check(all, dirs)
	for i := 0; i+1 < len(all); i++ {
		check(all[i:i+2], dirs)
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	check(all, dirs) // input order cannot change early-break completeness
	for _, f := range coverageFixtures() {
		check(f.nodes, f.dirs)
	}
	t.Logf("%d deterministic index/scan equivalence checks", checks)
}

func TestWalk_CompleteListingCoverage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		root     []Node
		page2    []Node
		children map[string][]Node
		want     []string
	}{
		{
			name: "descendant on later page",
			root: []Node{dirNode("a")}, page2: []Node{fileNode("a/file", 1)},
			want: []string{"request root", "request root page2", "file a/file"},
		},
		{
			name: "directory alone is not coverage",
			root: []Node{dirNode("a")}, children: map[string][]Node{"a": {fileNode("a/file", 1)}},
			want: []string{"request root", "request a", "file a/file"},
		},
		{
			name: "directory descendant covers parent",
			root: []Node{dirNode("a"), {Type: "tree", Path: "a/b"}}, children: map[string][]Node{"a/b": {}},
			want: []string{"request root", "request a/b"},
		},
		{
			name: "raw trailing slash covers parent",
			root: []Node{dirNode("a"), {Type: "unknown", Path: "a/"}},
			want: []string{"request root", "file a/"},
		},
		{
			name: "raw cleaned mismatch",
			root: []Node{dirNode("x/../a"), fileNode("x/../a/file", 1)}, children: map[string][]Node{"a": {}},
			want: []string{"request root", "request a", "file x/../a/file"},
		},
		{
			name: "prefix a is not a2",
			root: []Node{dirNode("a"), dirNode("a2"), fileNode("a2/file", 1)}, children: map[string][]Node{"a": {}},
			want: []string{"request root", "request a", "file a2/file"},
		},
		{
			name: "empty and duplicate directories",
			root: []Node{dirNode(""), dirNode("a"), dirNode("a"), dirNode("a/.")}, children: map[string][]Node{"a": {}},
			// a/. proves raw coverage of a, but its cleaned a still needs
			// listing only if no raw descendant exists. Here all are covered.
			want: []string{"request root"},
		},
		{
			name: "duplicate empty directory listed once",
			root: []Node{dirNode(""), dirNode("a"), dirNode("a")}, children: map[string][]Node{"a": {}},
			want: []string{"request root", "request a"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var trace []string
			record := func(s string) { mu.Lock(); trace = append(trace, s); mu.Unlock() }
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				prefix := strings.TrimPrefix(r.URL.Path, "/api/models/o/r/tree/main")
				prefix = strings.TrimPrefix(prefix, "/")
				label := prefix
				if label == "" {
					label = "root"
				}
				if r.URL.Query().Has("cursor") {
					label += " page2"
				}
				record("request " + label)
				nodes, ok := tc.children[prefix]
				if prefix == "" {
					ok, nodes = true, tc.root
					if r.URL.Query().Has("cursor") {
						nodes = tc.page2
					} else if tc.page2 != nil {
						w.Header().Set("Link", `<?cursor=2>; rel=next`)
					}
				}
				if !ok {
					t.Errorf("unexpected explicit directory listing %q", prefix)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(nodes)
			}))
			defer srv.Close()
			err := testWalker(srv, nil).Walk(context.Background(), "", func(n Node) error {
				record("file " + n.Path)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(trace, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("trace = %v, want %v", trace, tc.want)
			}
		})
	}
}

func TestWalk_CoverageOrderAndCallbackFailure(t *testing.T) {
	for _, failAt := range []string{"", "mirror/first", "mirror/deep/last"} {
		t.Run("failAt="+failAt, func(t *testing.T) {
			levels := map[string][]Node{
				"":            {fileNode("root", 1), dirNode("recursive"), fileNode("recursive/file", 1), dirNode("mirror"), fileNode("tail", 1), dirNode("mirror")},
				"mirror":      {fileNode("mirror/first", 1), dirNode("mirror/deep"), fileNode("mirror/end", 1), dirNode("mirror/deep")},
				"mirror/deep": {fileNode("mirror/deep/last", 1)},
			}
			var mu sync.Mutex
			var trace []string
			record := func(s string) { mu.Lock(); trace = append(trace, s); mu.Unlock() }
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				prefix := strings.TrimPrefix(r.URL.Path, "/api/models/o/r/tree/main")
				prefix = strings.TrimPrefix(prefix, "/")
				record("request " + prefix)
				nodes, ok := levels[prefix]
				if !ok {
					t.Errorf("unexpected listing %q", prefix)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(nodes)
			}))
			defer srv.Close()
			sentinel := errors.New("callback failed")
			err := testWalker(srv, nil).Walk(context.Background(), "", func(n Node) error {
				record("file " + n.Path)
				if n.Path == failAt {
					return sentinel
				}
				return nil
			})
			if (failAt == "" && err != nil) || (failAt != "" && err != sentinel) {
				t.Errorf("error = %v, want unchanged callback result", err)
			}
			want := []string{"request ", "file root", "file recursive/file", "request mirror", "file mirror/first", "request mirror/deep", "file mirror/deep/last", "file mirror/end", "file tail"}
			if failAt == "mirror/first" {
				want = want[:5]
			} else if failAt == "mirror/deep/last" {
				want = want[:7]
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(trace, "\n") != strings.Join(want, "\n") {
				t.Errorf("trace = %v, want %v (no callback rollback or further requests)", trace, want)
			}
		})
	}
}

func TestWalk_CoverageIsListingLocal(t *testing.T) {
	levels := map[string][]Node{
		"":       {dirNode("x"), dirNode("y")},
		"x":      {fileNode("y/deep/outside", 1)},
		"y":      {dirNode("y/deep")},
		"y/deep": {fileNode("y/deep/file", 1)},
	}
	var mu sync.Mutex
	var trace []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := strings.TrimPrefix(r.URL.Path, "/api/models/o/r/tree/main")
		prefix = strings.TrimPrefix(prefix, "/")
		mu.Lock()
		trace = append(trace, prefix)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(levels[prefix])
	}))
	defer srv.Close()
	walker := testWalker(srv, nil)
	for i := 0; i < 2; i++ {
		assertFiles(t, walkFiles(t, walker), "y/deep/outside", "y/deep/file")
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(trace, ","); got != ",x,y,y/deep,,x,y,y/deep" {
		t.Errorf("requests = %q, want each listing and each Walk isolated", got)
	}
}

func TestWalk_CoverageWaitsForAllPages(t *testing.T) {
	var reqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		if r.URL.Query().Get("cursor") == "2" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Link", `<?cursor=2>; rel=next`)
		_ = json.NewEncoder(w).Encode([]Node{dirNode("a"), fileNode("root", 1)})
	}))
	defer srv.Close()
	err := testWalker(srv, nil).Walk(context.Background(), "", func(Node) error {
		t.Error("failed complete listing delivered a callback")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "404") || reqs.Load() != 2 {
		t.Errorf("error = %v, requests = %d; want second-page failure, no child listing", err, reqs.Load())
	}
}

func TestOriginOf(t *testing.T) {
	for _, tc := range []struct {
		name, base, target string
		want               bool
	}{
		{"hostname case", "https://Hub.Example/tree", "https://hUB.eXAMPLE/page", true},
		{"http default", "http://hub.example/tree", "http://hub.example:80/page", true},
		{"https default", "https://hub.example:443/tree", "https://hub.example/page", true},
		{"numeric port", "https://hub.example:0443/tree", "https://hub.example/page", true},
		{"nondefault port", "https://hub.example:8443/tree", "https://hub.example:8443/page", true},
		{"other port", "https://hub.example/tree", "https://hub.example:8443/page", false},
		{"other host", "https://hub.example/tree", "https://other.example/page", false},
		{"downgrade", "https://hub.example/tree", "http://hub.example/page", false},
		{"scheme with same port", "https://hub.example:80/tree", "http://hub.example/page", false},
		{"ipv6 default", "http://[::1]/tree", "http://[::1]:80/page", true},
		{"ipv6 case", "https://[2001:DB8::A]:443/tree", "https://[2001:db8::a]/page", true},
		{"ipv6 other port", "http://[::1]:8000/tree", "http://[::1]:8001/page", false},
		{"ipv6 other host", "http://[::1]/tree", "http://[::2]/page", false},
		{"userinfo is not origin", "https://hub.example/tree", "https://user:password@hub.example/page", true},
		{"userinfo cannot disguise host", "https://hub.example/tree", "https://hub.example@other.example/page", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, err := url.Parse(tc.base)
			if err != nil {
				t.Fatal(err)
			}
			target, err := url.Parse(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if got := originOf(base).matches(target); got != tc.want {
				t.Errorf("origin matches = %v, want %v", got, tc.want)
			}
		})
	}
}

// Only advertised fresh requests are constrained here. A redirect can still
// run under the existing client policy, but cannot redefine the Walk's origin
// or the base used to resolve its next link.
func TestWalk_RedirectDoesNotRedefinePaginationOrigin(t *testing.T) {
	for _, foreignNext := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreignNext=%v", foreignNext), func(t *testing.T) {
			var destination, source atomic.Int64
			dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				destination.Add(1)
				next := "?cursor=2"
				if foreignNext {
					next = "http://" + r.Host + "/page"
				}
				if r.URL.Path == "/landing" {
					w.Header().Set("Link", "<"+next+">; rel=next")
				}
				_ = json.NewEncoder(w).Encode([]Node{fileNode("first.bin", 1)})
			}))
			defer dst.Close()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				source.Add(1)
				if r.URL.Query().Get("cursor") == "2" {
					_ = json.NewEncoder(w).Encode([]Node{fileNode("second.bin", 2)})
					return
				}
				http.Redirect(w, r, dst.URL+"/landing", http.StatusFound)
			}))
			defer srv.Close()
			var files []string
			err := testWalker(srv, nil).Walk(context.Background(), "", func(n Node) error {
				files = append(files, n.Path)
				return nil
			})
			wantSource := int64(2)
			if foreignNext {
				wantSource = 1
				if err == nil || err.Error() != "hubtree: cross-origin next pagination target" || len(files) != 0 {
					t.Errorf("error = %v, files = %v; want foreign-next failure", err, files)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertFiles(t, files, "first.bin", "second.bin")
			}
			if destination.Load() != 1 || source.Load() != wantSource {
				t.Errorf("destination/source requests = %d/%d, want 1/%d (only existing redirect)", destination.Load(), source.Load(), wantSource)
			}
		})
	}
}

// Later URL builds must not reset the first-built origin, even if a custom
// builder changes its own endpoint during recursive fallback or traversal.
func TestWalk_FirstBuiltOriginIsImmutable(t *testing.T) {
	for _, stage := range []string{"directory", "400 fallback", "422 fallback"} {
		t.Run(stage, func(t *testing.T) {
			var destination atomic.Int64
			dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				destination.Add(1)
				w.Header().Set("Link", "<http://"+r.Host+"/next>; rel=next")
				_ = json.NewEncoder(w).Encode([]Node{fileNode("sub/file.bin", 1)})
			}))
			defer dst.Close()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch stage {
				case "400 fallback":
					w.WriteHeader(http.StatusBadRequest)
				case "422 fallback":
					w.WriteHeader(http.StatusUnprocessableEntity)
				default:
					_ = json.NewEncoder(w).Encode([]Node{dirNode("sub")})
				}
			}))
			defer srv.Close()
			builds := 0
			walker := &Walker{TreeURL: func(prefix string) string {
				builds++
				if builds == 1 {
					return srv.URL + "/tree/main"
				}
				return dst.URL + "/tree/main/" + prefix
			}}
			err := walker.Walk(context.Background(), "", func(Node) error { return nil })
			if err == nil || err.Error() != "hubtree: cross-origin next pagination target" {
				t.Errorf("error = %v, want original-origin failure", err)
			}
			// The operator-provided builder is not restricted by this change;
			// only its subsequent advertised next must be rejected.
			if got := destination.Load(); got != 1 {
				t.Errorf("destination requests = %d, want one built URL, no next", got)
			}
		})
	}
}

func TestWalk_OriginIsPerWalk(t *testing.T) {
	serve := func(label string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cursor") == "" {
				w.Header().Set("Link", "<http://"+r.Host+"/page?cursor=2>; rel=next")
			}
			_ = json.NewEncoder(w).Encode([]Node{fileNode(label+r.URL.Query().Get("cursor"), 1)})
		}))
	}
	a, b := serve("a"), serve("b")
	defer a.Close()
	defer b.Close()
	walker := &Walker{TreeURL: func(prefix string) string {
		if prefix == "a" {
			return a.URL + "/tree/main"
		}
		return b.URL + "/tree/main"
	}}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		for _, prefix := range []string{"a", "b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var files []string
				err := walker.Walk(context.Background(), prefix, func(n Node) error {
					files = append(files, n.Path)
					return nil
				})
				if err != nil {
					t.Error(err)
					return
				}
				if len(files) != 2 || files[0] != prefix || files[1] != prefix+"2" {
					t.Errorf("files = %v, want %s/%s2", files, prefix, prefix)
				}
			}()
		}
	}
	wg.Wait()
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
