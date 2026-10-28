package hfdownloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const pinningFixtureCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type pinningFixture struct {
	server            *httptest.Server
	mu                sync.Mutex
	seen              []pinningRequest
	failPinnedPath    string
	failStatus        int
	missingPinned     string
	redirectPinned    string
	missingPinnedTree bool
	revisionStatus    int
}

type pinningRequest struct {
	method string
	path   string
	rangeH string
}

func digestText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func newPinningFixture(t *testing.T, commitFiles, branchFiles map[string]string, commitLFS, branchLFS map[string]bool, failPinnedPath string, failStatus int, missingPinned string) *pinningFixture {
	t.Helper()
	f := &pinningFixture{failPinnedPath: failPinnedPath, failStatus: failStatus, missingPinned: missingPinned}
	makeTree := func(files map[string]string, lfs map[string]bool) []hfNode {
		out := make([]hfNode, 0, len(files))
		for name, body := range files {
			n := hfNode{Type: "file", Path: name, Size: int64(len(body)), Sha256: digestText(body)}
			if lfs[name] {
				n.Sha256 = ""
				n.LFS = &hfLfsInfo{Oid: digestText(body), Size: int64(len(body))}
			}
			out = append(out, n)
		}
		return out
	}
	trees := map[string][]hfNode{
		pinningFixtureCommit: makeTree(commitFiles, commitLFS),
		"main":               makeTree(branchFiles, branchLFS),
	}
	files := map[string]map[string]string{pinningFixtureCommit: commitFiles, "main": branchFiles}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, pinningRequest{method: r.Method, path: r.URL.Path, rangeH: r.Header.Get("Range")})
		f.mu.Unlock()
		requestPath := r.URL.Path
		switch {
		case strings.Contains(requestPath, "/revision/"):
			if f.revisionStatus != 0 {
				w.WriteHeader(f.revisionStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: pinningFixtureCommit})
		case strings.Contains(requestPath, "/tree/"):
			revision := requestPath[strings.Index(requestPath, "/tree/")+len("/tree/"):]
			if revision == pinningFixtureCommit && f.missingPinnedTree {
				http.NotFound(w, r)
				return
			}
			nodes, ok := trees[revision]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(nodes)
		case strings.Contains(requestPath, "/raw/") || strings.Contains(requestPath, "/resolve/"):
			marker := "/raw/"
			if !strings.Contains(requestPath, marker) {
				marker = "/resolve/"
			}
			rest := requestPath[strings.Index(requestPath, marker)+len(marker):]
			parts := strings.SplitN(rest, "/", 2)
			if len(parts) != 2 {
				http.NotFound(w, r)
				return
			}
			revision, rel := parts[0], parts[1]
			if revision == pinningFixtureCommit && rel == f.failPinnedPath && f.failStatus != 0 {
				w.WriteHeader(f.failStatus)
				return
			}
			if revision == pinningFixtureCommit && rel == f.redirectPinned {
				w.Header().Set("Location", strings.Replace(r.URL.Path, "/"+pinningFixtureCommit+"/", "/main/", 1))
				w.WriteHeader(http.StatusFound)
				return
			}
			if revision == pinningFixtureCommit && rel == f.missingPinned {
				http.NotFound(w, r)
				return
			}
			body, ok := files[revision][rel]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func TestCommitURLRewriteToMutableBranchTriggersCoherentFallback(t *testing.T) {
	commitFiles := map[string]string{"config.json": "commit-generation"}
	branchFiles := map[string]string{"config.json": "branch-generation"}
	f := newPinningFixture(t, commitFiles, branchFiles, nil, nil, "", 0, "")
	f.redirectPinned = "config.json"
	cfg := Settings{OutputDir: t.TempDir(), Endpoint: f.server.URL, Verify: "size"}
	job := Job{Repo: "owner/model", Revision: "main"}
	var final Plan
	cfg.OnPlanComplete = func(plan Plan) { final = plan }
	if err := Download(context.Background(), job, cfg, nil); err != nil {
		t.Fatalf("download after observed mutable redirect: %v", err)
	}
	if final.CommitPinned || final.Commit != "main" || len(final.Items) != 1 || !strings.Contains(final.Items[0].URL, "/main/") {
		t.Fatalf("redirect fallback did not rebuild one unpinned plan: %+v", final)
	}
	got, err := os.ReadFile(filepath.Join(cfg.OutputDir, job.Repo, "config.json"))
	if err != nil || string(got) != branchFiles["config.json"] {
		t.Fatalf("published bytes = %q err=%v, want one branch response", got, err)
	}
}

func (f *pinningFixture) requests() []pinningRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pinningRequest(nil), f.seen...)
}

func TestCustomBranchOnlyFallbackRebuildsWholePlanBeforeReservation(t *testing.T) {
	commitFiles := map[string]string{"config.json": "commit-config", "weights.safetensors": "commit-weights"}
	branchFiles := map[string]string{"config.json": "branch-config-content", "weights.safetensors": "branch-weights-content"}
	lfs := map[string]bool{"weights.safetensors": true}
	f := newPinningFixture(t, commitFiles, branchFiles, lfs, lfs, "", 0, "weights.safetensors")
	cfg := Settings{CacheDir: t.TempDir(), Endpoint: f.server.URL, Verify: "size"}
	job := Job{Repo: "owner/model", Revision: "main"}
	cache, err := cfg.BuildHFCache()
	if err != nil {
		t.Fatal(err)
	}
	repoDir, err := cache.Repo(job.Repo, RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	var callbackCount atomic.Int32
	var callbackBoundary int
	cfg.OnPlanComplete = func(plan Plan) {
		callbackCount.Add(1)
		if plan.CommitPinned || plan.CommitResolved || plan.Commit != "main" {
			t.Errorf("effective fallback identity = commit %q pinned=%v resolved=%v", plan.Commit, plan.CommitPinned, plan.CommitResolved)
		}
		for _, item := range plan.Items {
			if !strings.Contains(item.URL, "/main/") {
				t.Errorf("fallback plan retained mixed/pinned URL %q", item.URL)
			}
			if item.RelativePath != "config.json" {
				continue
			}
			dst := filepath.Join(repoDir.BlobsDir(), "tmp-"+item.SHA256)
			if err := os.WriteFile(dst+".part", []byte("old-prefix"), 0o600); err != nil {
				t.Errorf("seed unpinned partial: %v", err)
				return
			}
			identity := singlePartIdentity{Version: singleIdentityVersion, Repo: job.Repo, Path: item.RelativePath, Size: item.Size, Pinned: false, FramingComplete: true}
			if err := writeSinglePartIdentity(dst+".part.meta", identity); err != nil {
				t.Errorf("seed unpinned identity: %v", err)
			}
		}
		callbackBoundary = len(f.requests())
	}
	var mu sync.Mutex
	var events []ProgressEvent
	err = Download(context.Background(), job, cfg, func(event ProgressEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("branch-only fallback download: %v", err)
	}
	if got := callbackCount.Load(); got != 1 {
		t.Fatalf("OnPlanComplete calls = %d, want exactly one final plan", got)
	}
	if callbackBoundary == 0 {
		t.Fatal("final plan callback did not run after the pinned capability checks")
	}
	for _, request := range f.requests()[callbackBoundary:] {
		if (strings.Contains(request.path, "/raw/") || strings.Contains(request.path, "/resolve/")) && !strings.Contains(request.path, "/main/") {
			t.Errorf("transfer after plan reservation used a different addressing mode: %s", request.path)
		}
		if request.rangeH != "" {
			t.Errorf("unpinned transfer stitched a range request: %+v", request)
		}
	}
	mu.Lock()
	warningIndex, planItemIndex := -1, -1
	for i, event := range events {
		if event.Event == "warning" && strings.Contains(event.Message, "not pinned") {
			warningIndex = i
		}
		if event.Event == "plan_item" && planItemIndex < 0 {
			planItemIndex = i
		}
	}
	mu.Unlock()
	if warningIndex < 0 || planItemIndex < 0 || warningIndex >= planItemIndex {
		t.Fatalf("fallback warning index=%d plan_item index=%d, warning must precede plan exposure", warningIndex, planItemIndex)
	}
	for path, want := range branchFiles {
		got, err := os.ReadFile(filepath.Join(repoDir.SnapshotsDir(), "main", filepath.FromSlash(path)))
		if err != nil || string(got) != want {
			t.Errorf("revision-addressed snapshot %s = %q err=%v, want branch bytes %q", path, got, err, want)
		}
	}
	manifest := readBranchFixtureManifest(t, filepath.Join(repoDir.FriendlyPath(), ManifestFilename))
	if manifest.Commit != "" || manifest.Branch != "main" {
		t.Errorf("fallback manifest claims immutable identity: %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(repoDir.RefsDir(), "main")); !os.IsNotExist(err) {
		t.Errorf("fallback wrote an immutable refs/main alias (err=%v)", err)
	}
}

func TestCommitProbeErrorsDoNotDowngradeOrBlockCachedSkip(t *testing.T) {
	content := "verified-content"
	files := map[string]string{"weights.safetensors": content}
	lfs := map[string]bool{"weights.safetensors": true}
	f := newPinningFixture(t, files, files, lfs, lfs, "weights.safetensors", http.StatusInternalServerError, "")
	cfg := Settings{CacheDir: t.TempDir(), Endpoint: f.server.URL, Verify: "size"}
	job := Job{Repo: "owner/model", Revision: "main"}
	plan, err := PlanRepo(context.Background(), job, cfg)
	if err != nil {
		t.Fatalf("pinned plan with inconclusive optional probe: %v", err)
	}
	if !plan.CommitPinned || plan.Commit != pinningFixtureCommit || !strings.Contains(plan.Items[0].URL, "/"+pinningFixtureCommit+"/") {
		t.Fatalf("inconclusive 500 probe downgraded plan: %+v", plan)
	}
	cache, err := cfg.BuildHFCache()
	if err != nil {
		t.Fatal(err)
	}
	repoDir, err := cache.Repo(job.Repo, RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repoDir.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	sha := digestText(content)
	if err := os.WriteFile(repoDir.BlobPath(sha), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var boundary int
	cfg.OnPlanComplete = func(final Plan) {
		if !final.CommitPinned || final.Commit != pinningFixtureCommit {
			t.Errorf("cached skip plan changed mode: %+v", final)
		}
		boundary = len(f.requests())
	}
	if err := Download(context.Background(), job, cfg, nil); err != nil {
		t.Fatalf("cached skip was gated by an optional probe failure: %v", err)
	}
	if boundary == 0 {
		t.Fatal("cached skip plan callback was not reached")
	}
	for _, request := range f.requests()[boundary:] {
		if strings.Contains(request.path, "/raw/") || strings.Contains(request.path, "/resolve/") {
			t.Errorf("cached item performed a transfer after reservation: %+v", request)
		}
	}
}

func TestPinnedTree404UsesValidatedBranchTreeBeforeFallback(t *testing.T) {
	files := map[string]string{"config.json": "branch-only-tree-content"}
	f := newPinningFixture(t, files, files, nil, nil, "", 0, "")
	f.missingPinnedTree = true
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/model", Revision: "main"}, Settings{Endpoint: f.server.URL})
	if err != nil {
		t.Fatalf("branch tree fallback: %v", err)
	}
	if plan.Commit != "main" || plan.CommitPinned || len(plan.Items) != 1 || !strings.Contains(plan.Items[0].URL, "/main/") {
		t.Fatalf("tree fallback identity = %+v", plan)
	}
	var branchTree, commitTree int
	for _, request := range f.requests() {
		if !strings.Contains(request.path, "/tree/") {
			continue
		}
		if strings.HasSuffix(request.path, "/tree/"+pinningFixtureCommit) {
			commitTree++
		}
		if strings.HasSuffix(request.path, "/tree/main") {
			branchTree++
		}
	}
	if commitTree != 1 || branchTree != 1 {
		t.Fatalf("tree capability comparison = commit %d branch %d, want one each", commitTree, branchTree)
	}
}

func TestRevisionAuthOrServerFailureNeverEnablesBranchFallback(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			files := map[string]string{"config.json": "branch-content"}
			f := newPinningFixture(t, files, files, nil, nil, "", 0, "")
			f.revisionStatus = status
			_, err := PlanRepo(context.Background(), Job{Repo: "owner/model", Revision: "main"}, Settings{Endpoint: f.server.URL})
			if err == nil {
				t.Fatalf("revision status %d was treated as branch-only capability", status)
			}
			for _, request := range f.requests() {
				if strings.Contains(request.path, "/tree/") || strings.Contains(request.path, "/raw/") || strings.Contains(request.path, "/resolve/") {
					t.Errorf("revision status %d triggered downstream fallback request %s", status, request.path)
				}
			}
		})
	}
}

func TestBranchVariantNeverFallsBackToMutableBranchAddressing(t *testing.T) {
	commitFiles := map[string]string{"config.json": "commit-only-bytes"}
	branchFiles := map[string]string{"config.json": "mutable-branch-bytes"}
	f := newPinningFixture(t, commitFiles, branchFiles, nil, nil, "", 0, "config.json")
	cfg := Settings{OutputDir: t.TempDir(), Endpoint: f.server.URL, Verify: "size"}
	job := Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}
	var final Plan
	cfg.OnPlanComplete = func(plan Plan) { final = plan }
	err := Download(context.Background(), job, cfg, nil)
	if err == nil {
		t.Fatal("branch variant used mutable branch bytes after its pinned URL failed")
	}
	if !final.CommitPinned || final.Commit != pinningFixtureCommit || len(final.Items) != 1 || !strings.Contains(final.Items[0].URL, "/"+pinningFixtureCommit+"/") {
		t.Fatalf("branch variant did not remain pinned: %+v", final)
	}
	for _, request := range f.requests() {
		if strings.Contains(request.path, "/raw/main/") || strings.Contains(request.path, "/resolve/main/") {
			t.Errorf("branch variant fell back to a mutable URL: %s", request.path)
		}
	}
}

func TestUnpinnedSingleRetryRestartsAtZeroInsteadOfStitchingResponses(t *testing.T) {
	first := []byte("AAAA")
	second := []byte("BBBBBBBB")
	var requests int32
	var rangesMu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-HFDesk-Addressing-Probe") == "1" {
			t.Error("direct single transfer unexpectedly used a planning probe")
		}
		rangesMu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		rangesMu.Unlock()
		if atomic.AddInt32(&requests, 1) == 1 {
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			_, _ = w.Write(first)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		_, _ = w.Write(second)
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "model.bin")
	item := PlanItem{RelativePath: "model.bin", URL: srv.URL, Size: int64(len(second)), SHA256: digestText(string(second))}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "owner/model", Revision: "main"}, Settings{Retries: 1, Verify: "size", StallTimeout: "0"}, item, dst, singleOpts{Commit: "main", CommitPinned: false}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("unpinned retry: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != string(second) {
		t.Fatalf("published retry bytes = %q err=%v, want only second response %q", got, err, second)
	}
	rangesMu.Lock()
	gotRanges := append([]string(nil), ranges...)
	rangesMu.Unlock()
	if len(gotRanges) != 2 || gotRanges[0] != "" || gotRanges[1] != "" {
		t.Fatalf("unpinned response ranges = %v, want two zero-start full GETs", gotRanges)
	}
}
