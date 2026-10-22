package hfdownloader

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/runtimecoordination"
)

var (
	testHFDeskBuildOnce sync.Once
	testHFDeskBinary    string
	testHFDeskBuildErr  error
	testHFDeskTempRoot  string
)

// Default-layout tests must not inherit an external user Hub. Dedicated tests
// below exercise accepted ENV overrides explicitly in temporary roots.
func TestMain(m *testing.M) {
	goModuleCache, _ := exec.Command("go", "env", "GOMODCACHE").Output()
	goBuildCache, _ := exec.Command("go", "env", "GOCACHE").Output()
	root, err := os.MkdirTemp("", "hfdownloader-tests-")
	if err != nil {
		panic(err)
	}
	for key, value := range map[string]string{
		"HOME": root, "USERPROFILE": root,
		"TMPDIR": root, "TMP": root, "TEMP": root,
		"XDG_CACHE_HOME": filepath.Join(root, "cache"), "XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_DATA_HOME": filepath.Join(root, "data"), "XDG_STATE_HOME": filepath.Join(root, "state"),
		"XDG_RUNTIME_DIR": filepath.Join(root, "runtime"),
		"APPDATA":         filepath.Join(root, "appdata"), "LOCALAPPDATA": filepath.Join(root, "localappdata"),
		"HFDESK_TEST_COORDINATION_DIR": filepath.Join(root, "coordination-runtime"),
		"HF_HOME":                      "", "HF_HUB_CACHE": "", "HF_TOKEN": "", "HFDESK_BIN": "",
		"GOMODCACHE": strings.TrimSpace(string(goModuleCache)), "GOCACHE": strings.TrimSpace(string(goBuildCache)),
	} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	testHFDeskTempRoot = root
	coordinationBase := filepath.Join(root, "coordination-runtime")
	runtimecoordination.SetBaseDirProvider(func() (string, error) { return coordinationBase, nil })
	code := m.Run()
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func ensureTestHFDeskBinary(t *testing.T) string {
	t.Helper()
	testHFDeskBuildOnce.Do(func() {
		moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			testHFDeskBuildErr = err
			return
		}
		testHFDeskBinary = filepath.Join(testHFDeskTempRoot, "hfdesk-test")
		if runtime.GOOS == "windows" {
			testHFDeskBinary += ".exe"
		}
		build := exec.Command("go", "build", "-o", testHFDeskBinary, "./cmd/hfdesk")
		build.Dir = moduleRoot
		if output, err := build.CombinedOutput(); err != nil {
			testHFDeskBuildErr = fmt.Errorf("build production HFDesk binary for rebuild-wrapper tests: %w\n%s", err, output)
		}
	})
	if testHFDeskBuildErr != nil {
		t.Fatalf("build production HFDesk binary: %v", testHFDeskBuildErr)
	}
	if err := os.Setenv("HFDESK_BIN", testHFDeskBinary); err != nil {
		t.Fatalf("set HFDESK_BIN for wrapper tests: %v", err)
	}
	return testHFDeskBinary
}

func runGeneratedRebuildScript(t *testing.T, scriptPath string, args ...string) ([]byte, error) {
	t.Helper()
	registerProfileCoordinationCleanup(t, filepath.Dir(scriptPath))
	binary := ensureTestHFDeskBinary(t)
	command := exec.Command("bash", append([]string{scriptPath}, args...)...)
	command.Env = filteredRebuildTestEnv("HFDESK_BIN")
	command.Env = append(command.Env, "HFDESK_BIN="+binary)
	return command.CombinedOutput()
}

func filteredRebuildTestEnv(drop ...string) []string {
	dropped := make(map[string]struct{}, len(drop))
	for _, name := range drop {
		dropped[name] = struct{}{}
	}
	var env []string
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if _, ok := dropped[name]; !ok {
			env = append(env, value)
		}
	}
	return env
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
	ensureTestHFDeskBinary(t)
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
	if out, err := runGeneratedRebuildScript(t, filepath.Join(r, "rebuild.sh")); err != nil {
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
	ensureTestHFDeskBinary(t)
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
	if out, err := runGeneratedRebuildScript(t, filepath.Join(moved, "rebuild.sh")); err != nil {
		t.Fatalf("portable script: %v %s", err, out)
	}
	if got, err := os.ReadFile(filepath.Join(moved, "models", "owner", "model", "file")); err != nil || string(got) != "portable" {
		t.Fatalf("portable content=%q %v", got, err)
	}
}

func TestRebuildWrapperUsesGoWithoutPythonAndForwardsCleanVerbose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("generated rebuild wrapper is a POSIX Bash script")
	}
	ensureTestHFDeskBinary(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
	root := filepath.Join(t.TempDir(), "friendly $cache; ' root")
	hubDir := filepath.Join(t.TempDir(), "exact $hub; ' source")
	// The wrapper runs the production CLI in a separate process, which derives
	// its coordinator from the OS account. Track the lease entries that child
	// creates for this root so the run leaves no state in the user profile.
	registerProfileCoordinationCleanup(t, root)
	cache := NewHFCacheResolved(root, hubDir, 0)
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := repo.SnapshotDir("commit-main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "weights.bin"), []byte("from-custom-H"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.WriteRef("main", "commit-main"); err != nil {
		t.Fatal(err)
	}
	friendly := filepath.Join(repo.FriendlyPath(), "weights.bin")
	orphan := filepath.Join(repo.FriendlyPath(), "orphan")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing-target"), orphan); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := cache.WriteRebuildScript(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(cache.RebuildScriptPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"python3 -c", "find ", "manifest", "flock", "clean_orphans"} {
		if strings.Contains(strings.ToLower(string(content)), forbidden) {
			t.Fatalf("wrapper contains Go-owned rebuild logic %q:\n%s", forbidden, content)
		}
	}

	binDir := t.TempDir()
	binary := filepath.Join(binDir, "hfdesk binary ' path")
	if err := os.Symlink(os.Getenv("HFDESK_BIN"), binary); err != nil {
		t.Skipf("binary symlink unavailable: %v", err)
	}
	command := exec.Command(bash, cache.RebuildScriptPath(), "--clean", "--verbose")
	command.Env = filteredRebuildTestEnv("PATH", "HFDESK_BIN")
	command.Env = append(command.Env, "PATH="+t.TempDir(), "HFDESK_BIN="+binary)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("python-free wrapper rebuild: %v: %s", err, output)
	}
	if got, err := os.ReadFile(friendly); err != nil || string(got) != "from-custom-H" {
		t.Fatalf("wrapper did not rebuild from exact custom H: %q %v; output=%s", got, err, output)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("--clean was not forwarded: %v", err)
	}
	for _, expected := range []string{"Repos scanned:", "Orphans removed:   1", "Processing: model/owner/model", "Created: weights.bin", "Removed orphan:"} {
		if !strings.Contains(string(output), expected) {
			t.Fatalf("--verbose output missing %q: %s", expected, output)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "hub")); !os.IsNotExist(err) {
		t.Fatalf("custom-H wrapper created an unintended R/hub: %v", err)
	}
}

func TestRebuildWrapperFailsClosedWithoutCompatibleBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("generated rebuild wrapper is a POSIX Bash script")
	}
	ensureTestHFDeskBinary(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
	root := filepath.Join(t.TempDir(), "cache root")
	hubDir := filepath.Join(t.TempDir(), "hub source")
	cache := NewHFCacheResolved(root, hubDir, 0)
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := repo.SnapshotDir("commit-main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "weights.bin"), []byte("must-not-project"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.WriteRef("main", "commit-main"); err != nil {
		t.Fatal(err)
	}
	serviceSentinel := filepath.Join(root, BranchVariantServiceDirName, "sentinel")
	if err := os.MkdirAll(filepath.Dir(serviceSentinel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serviceSentinel, []byte("service-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	controlSentinel := filepath.Join(root, "hfdesk.json")
	if err := os.WriteFile(controlSentinel, []byte("{\"cache-dir\":\"unchanged\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.WriteRebuildScript(); err != nil {
		t.Fatal(err)
	}

	portListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer portListener.Close()
	port := portListener.Addr().(*net.TCPAddr).Port
	legacyBinary := filepath.Join(t.TempDir(), "legacy-hfdesk")
	if runtime.GOOS == "windows" {
		legacyBinary += ".exe"
	}
	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", legacyBinary, "./pkg/hfdownloader/testdata/legacy_hfdesk.go")
	build.Dir = moduleRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build legacy parser fixture: %v: %s", err, output)
	}
	serverSentinel := filepath.Join(t.TempDir(), "server-started")
	browserSentinel := filepath.Join(t.TempDir(), "browser-opened")
	workDir := t.TempDir()
	configBefore := []byte("{\"cache-dir\":\"unchanged\"}")
	if err := os.WriteFile(filepath.Join(workDir, "hfdesk.json"), configBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	baseEnv := filteredRebuildTestEnv("PATH", "HFDESK_BIN")
	baseEnv = append(baseEnv, "PATH="+t.TempDir(), fmt.Sprintf("LEGACY_PORT=%d", port), "LEGACY_SERVER_SENTINEL="+serverSentinel, "LEGACY_BROWSER_SENTINEL="+browserSentinel)

	for _, test := range []struct {
		name      string
		binary    string
		wantError string
	}{
		{name: "missing", wantError: "compatible HFDesk binary is required"},
		{name: "legacy parser", binary: legacyBinary, wantError: "missing the compatible rebuild entry"},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := append([]string(nil), baseEnv...)
			if test.binary != "" {
				env = append(env, "HFDESK_BIN="+test.binary)
			}
			command := exec.Command(bash, cache.RebuildScriptPath())
			command.Dir = workDir
			command.Env = env
			output, runErr := command.CombinedOutput()
			if runErr == nil || !strings.Contains(string(output), test.wantError) {
				t.Fatalf("wrapper error=%v output=%s; want %q", runErr, output, test.wantError)
			}
			if _, err := os.Lstat(filepath.Join(repo.FriendlyPath(), "weights.bin")); !os.IsNotExist(err) {
				t.Fatalf("incompatible wrapper changed the friendly view: %v", err)
			}
			if got, err := os.ReadFile(serviceSentinel); err != nil || string(got) != "service-state" {
				t.Fatalf("incompatible wrapper changed service state: %q %v", got, err)
			}
			if got, err := os.ReadFile(controlSentinel); err != nil || string(got) != string(configBefore) {
				t.Fatalf("incompatible wrapper changed R config: %q %v", got, err)
			}
			if got, err := os.ReadFile(filepath.Join(workDir, "hfdesk.json")); err != nil || string(got) != string(configBefore) {
				t.Fatalf("incompatible wrapper changed working config: %q %v", got, err)
			}
			if _, err := os.Stat(serverSentinel); !os.IsNotExist(err) {
				t.Fatalf("incompatible binary entered server startup: %v", err)
			}
			if _, err := os.Stat(browserSentinel); !os.IsNotExist(err) {
				t.Fatalf("incompatible binary launched the browser: %v", err)
			}
		})
	}
}
