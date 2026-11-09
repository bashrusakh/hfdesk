package hfdownloader

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Default-layout tests must not inherit an external user Hub. Dedicated tests
// below exercise accepted ENV overrides explicitly in temporary roots.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "hfdownloader-tests-")
	if err != nil {
		panic(err)
	}
	for key, value := range map[string]string{"HOME": root, "USERPROFILE": root, "HF_HOME": "", "HF_HUB_CACHE": "", "HF_TOKEN": ""} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func TestHFCacheCapturesHubIdentity(t *testing.T) {
	r := filepath.Join(t.TempDir(), "hub")
	h := filepath.Join(t.TempDir(), "exact ' shared")
	t.Setenv("HF_HUB_CACHE", h)
	c := NewHFCache(r, 0)
	repo, err := c.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HF_HUB_CACHE", filepath.Join(t.TempDir(), "later"))
	if c.Root != r || c.HubDir() != h || repo.Path() != filepath.Join(h, "models--owner--model") {
		t.Fatalf("cache relocated after construction: R=%q H=%q repo=%q", c.Root, c.HubDir(), repo.Path())
	}
}

func TestHFCacheCapturesDefaultRoot(t *testing.T) {
	r := filepath.Join(t.TempDir(), "home")
	t.Setenv("HF_HOME", r)
	t.Setenv("HF_HUB_CACHE", "")
	c := NewHFCache("", 0)
	t.Setenv("HF_HOME", filepath.Join(t.TempDir(), "changed"))
	t.Setenv("HF_HUB_CACHE", filepath.Join(t.TempDir(), "other"))
	if c.Root != r || c.HubDir() != filepath.Join(r, "hub") {
		t.Fatalf("default instance moved: R=%q H=%q", c.Root, c.HubDir())
	}
	fresh := NewHFCache("", 0)
	if fresh.Root == c.Root || fresh.HubDir() == c.HubDir() {
		t.Fatal("fresh library construction did not resolve current ENV")
	}
}

func TestCacheIdentityCompletedDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink/script assertions are Unix-specific; existing Windows fallbacks remain")
	}
	r := filepath.Join(t.TempDir(), "friendly ' root")
	h := filepath.Join(t.TempDir(), "exact ' store")
	later := filepath.Join(t.TempDir(), "later")
	t.Setenv("HF_HUB_CACHE", h)
	body := []byte("controlled LFS payload")
	hash := fmt.Sprintf("%x", sha256.Sum256(body))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.Contains(req.URL.Path, "/revision/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "commit123"})
		case strings.Contains(req.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"type": "file", "path": "weights.bin", "size": len(body), "lfs": map[string]any{"oid": hash, "size": len(body)}}})
		case strings.Contains(req.URL.Path, "/resolve/"):
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Header().Set("ETag", hash)
			if req.Method != "HEAD" {
				_, _ = w.Write(body)
			}
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()
	cfg := Settings{CacheDir: r, Endpoint: srv.URL, Concurrency: 1, Verify: "sha256"}
	err := Download(context.Background(), Job{Repo: "owner/model"}, cfg, func(e ProgressEvent) {
		if e.Event == "scan_start" {
			t.Setenv("HF_HUB_CACHE", later)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(h, "models--owner--model")
	for _, path := range []string{filepath.Join(repo, "blobs", hash), filepath.Join(repo, "snapshots", "commit123", "weights.bin"), filepath.Join(r, "models", "owner", "model", "weights.bin")} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(body) {
			t.Fatalf("artifact %s: %q %v", path, got, err)
		}
	}
	ref, err := os.ReadFile(filepath.Join(repo, "refs", "main"))
	if err != nil || strings.TrimSpace(string(ref)) != "commit123" {
		t.Fatalf("ref=%q %v", ref, err)
	}
	m, err := ReadManifest(filepath.Join(r, "models", "owner", "model", ManifestFilename))
	if err != nil || m.TotalFiles != 1 || m.TotalSize != int64(len(body)) || m.Commit != "commit123" {
		t.Fatalf("manifest=%+v %v", m, err)
	}
	if filepath.Clean(filepath.Join(r, filepath.FromSlash(m.RepoPath))) != repo {
		t.Fatalf("manifest physical repository=%q", m.RepoPath)
	}
	for _, path := range []string{filepath.Join(r, "hub"), later} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unintended artifact root %s: %v", path, err)
		}
	}
	// Running a generated script must use its selected H, not later ENV.
	friendly := filepath.Join(r, "models", "owner", "model", "weights.bin")
	if err := os.Remove(friendly); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", filepath.Join(r, "rebuild.sh")).CombinedOutput(); err != nil {
		t.Fatalf("script: %v %s", err, out)
	}
	if got, err := os.ReadFile(friendly); err != nil || string(got) != string(body) {
		t.Fatalf("rebuilt content=%q %v", got, err)
	}
	oldScript, _ := os.ReadFile(filepath.Join(r, "rebuild.sh"))
	if _, err := NewHFCacheResolved(r, later, 0).WriteRebuildScript(); err != nil {
		t.Fatal(err)
	}
	newScript, _ := os.ReadFile(filepath.Join(r, "rebuild.sh"))
	if string(oldScript) == string(newScript) || !strings.Contains(string(newScript), later) {
		t.Fatal("script did not regenerate its Hub identity")
	}
	// Frozen library cleanup still targets H, despite changed ambient ENV.
	dst := filepath.Join(repo, "blobs", "tmp-test")
	if err := os.WriteFile(dst+".part", []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CleanupJobPartFiles(Settings{CacheDir: r, HubDir: h}, []string{dst}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Fatalf("frozen cleanup: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "blobs", hash)); err != nil || string(got) != string(body) {
		t.Fatal("cleanup changed completed blob")
	}
}

func TestRebuildScriptDefaultPortability(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell/symlinks")
	}
	t.Setenv("HF_HUB_CACHE", "")
	base := t.TempDir()
	r := filepath.Join(base, "old")
	c := NewHFCache(r, 0)
	rd, _ := c.Repo("owner/model", RepoTypeModel)
	if err := rd.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := rd.SnapshotDir("commit")
	if err := os.MkdirAll(snapshot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "file"), []byte("portable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := rd.WriteRef("main", "commit"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteRebuildScript(); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(base, "new ' location")
	if err := os.Rename(r, moved); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", filepath.Join(moved, "rebuild.sh")).CombinedOutput(); err != nil {
		t.Fatalf("portable script: %v %s", err, out)
	}
	if got, err := os.ReadFile(filepath.Join(moved, "models", "owner", "model", "file")); err != nil || string(got) != "portable" {
		t.Fatalf("portable content=%q %v", got, err)
	}
}
