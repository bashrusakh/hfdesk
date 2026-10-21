// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

func TestServerPathsFollowInstalledRuntimeModel(t *testing.T) {
	state := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(state, "hfdesk.json"),
		StateDir:   state,
		LogDir:     filepath.Join(state, "logs"),
	}
	runtimepaths.Install(model)
	t.Cleanup(runtimepaths.Reset)

	if got, want := ConfigPath(), model.ConfigFile; got != want {
		t.Errorf("ConfigPath() = %q, want %q", got, want)
	}
	if got, want := JobsStatePath(), filepath.Join(state, "jobs_state.json"); got != want {
		t.Errorf("JobsStatePath() = %q, want %q", got, want)
	}
	if got, want := HistoryPath(), filepath.Join(state, "download_history.json"); got != want {
		t.Errorf("HistoryPath() = %q, want %q", got, want)
	}
	if got, want := TargetsFilePath(), filepath.Join(state, "targets.yaml"); got != want {
		t.Errorf("TargetsFilePath() = %q, want %q", got, want)
	}
}

func TestServerPathsFollowTheEnvironmentWithoutAnInstalledModel(t *testing.T) {
	runtimepaths.Reset()
	t.Cleanup(runtimepaths.Reset)

	root := t.TempDir()
	setServerSandboxEnv(t, root)

	// The per-user config directory of the sandboxed environment.
	stateDir := referenceAppConfigDir()
	if !strings.HasPrefix(stateDir, root) {
		t.Fatalf("config directory %q is not inside the sandbox %q", stateDir, root)
	}

	if got, want := JobsStatePath(), filepath.Join(stateDir, "jobs_state.json"); got != want {
		t.Errorf("JobsStatePath() = %q, want %q", got, want)
	}
	if got, want := HistoryPath(), filepath.Join(stateDir, "download_history.json"); got != want {
		t.Errorf("HistoryPath() = %q, want %q", got, want)
	}
	if got, want := TargetsFilePath(), filepath.Join(stateDir, "targets.yaml"); got != want {
		t.Errorf("TargetsFilePath() = %q, want %q", got, want)
	}
}

func TestServerPathsInteractiveWithoutInstalledModel(t *testing.T) {
	runtimepaths.Reset()
	t.Cleanup(runtimepaths.Reset)

	if got, want := ConfigPath(), referenceConfigPath(t); got != want {
		t.Errorf("ConfigPath() = %q, want %q", got, want)
	}
	if got, want := JobsStatePath(), filepath.Join(AppConfigDir(), "jobs_state.json"); got != want {
		t.Errorf("JobsStatePath() = %q, want %q", got, want)
	}
	if got, want := HistoryPath(), filepath.Join(AppConfigDir(), "download_history.json"); got != want {
		t.Errorf("HistoryPath() = %q, want %q", got, want)
	}
}

func TestMirrorTargetsUseInstalledRuntimeModel(t *testing.T) {
	state := t.TempDir()
	runtimepaths.Install(runtimepaths.Paths{
		ConfigFile: filepath.Join(state, "hfdesk.json"),
		StateDir:   state,
		LogDir:     filepath.Join(state, "logs"),
	})
	t.Cleanup(runtimepaths.Reset)

	s := newTestServer(t)
	req := httptest.NewRequest("POST", "/api/mirror/targets", strings.NewReader(`{"name":"backup","path":"/tmp/hfdesk-backup"}`))
	w := httptest.NewRecorder()
	s.handleMirrorTargetAdd(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	targetsFile := filepath.Join(state, "targets.yaml")
	data, err := os.ReadFile(targetsFile)
	if err != nil {
		t.Fatalf("read targets file %s: %v", targetsFile, err)
	}
	if !strings.Contains(string(data), "backup") {
		t.Errorf("targets file %s does not contain the new target:\n%s", targetsFile, data)
	}
}

func TestMirrorTargetsFollowTheEnvironmentWithoutAnInstalledModel(t *testing.T) {
	runtimepaths.Reset()
	t.Cleanup(runtimepaths.Reset)
	setServerSandboxEnv(t, t.TempDir())

	addMirrorTarget(t)

	targetsFile := filepath.Join(referenceAppConfigDir(), "targets.yaml")
	data, err := os.ReadFile(targetsFile)
	if err != nil {
		t.Fatalf("read targets file %s: %v", targetsFile, err)
	}
	if !strings.Contains(string(data), "backup") {
		t.Errorf("targets file %s does not contain the new target:\n%s", targetsFile, data)
	}
}

func TestSettingsReportInstalledRuntimeModel(t *testing.T) {
	state := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(state, "hfdesk.json"),
		StateDir:   state,
		LogDir:     filepath.Join(state, "logs"),
	}
	runtimepaths.Install(model)
	t.Cleanup(runtimepaths.Reset)

	s := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/settings", nil)
	w := httptest.NewRecorder()
	s.handleGetSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var resp SettingsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if want := model.ConfigFile; resp.ConfigFile != want {
		t.Errorf("configFile = %q, want %q", resp.ConfigFile, want)
	}
	if want := filepath.Join(state, "targets.yaml"); resp.TargetsFile != want {
		t.Errorf("targetsFile = %q, want %q", resp.TargetsFile, want)
	}
}

func TestServerDirectoryModeFollowsTheInstalledRuntimeModel(t *testing.T) {
	runtimepaths.Reset()
	t.Cleanup(runtimepaths.Reset)

	// Without an installed model the interactive run keeps the historical
	// mode of its per-user directories.
	if got, want := serverDirMode(), os.FileMode(0o755); got != want {
		t.Errorf("serverDirMode() without an installed model = %o, want %o", got, want)
	}

	runtimepaths.Install(runtimepaths.Paths{
		ConfigFile: filepath.Join(t.TempDir(), "hfdesk.json"),
		StateDir:   filepath.Join(t.TempDir(), "state"),
		LogDir:     filepath.Join(t.TempDir(), "logs"),
	})

	// A supervised instance owns its directories.
	if got, want := serverDirMode(), os.FileMode(0o700); got != want {
		t.Errorf("serverDirMode() with an installed model = %o, want %o", got, want)
	}
}

func TestServerDirectoriesKeepTheHistoricalModeWithoutAnInstalledModel(t *testing.T) {
	runtimepaths.Reset()
	t.Cleanup(runtimepaths.Reset)
	setServerSandboxEnv(t, t.TempDir())

	configDir := filepath.Join(t.TempDir(), "config")
	if err := saveConfigFile(filepath.Join(configDir, "hfdesk.json"), &ConfigFile{}); err != nil {
		t.Fatalf("save config file: %v", err)
	}
	assertDirMode(t, configDir, 0o755)

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := saveJobsState(filepath.Join(stateDir, "jobs_state.json"), nil); err != nil {
		t.Fatalf("save jobs state: %v", err)
	}
	assertDirMode(t, stateDir, 0o755)

	// The history is written into the state directory of the effective
	// runtime path model, which is the per-user directory of an interactive
	// run.
	if err := AppendHistory(&Job{ID: "1", Repo: "owner/model"}); err != nil {
		t.Fatalf("append history: %v", err)
	}
	assertDirMode(t, AppConfigDir(), 0o755)
}

func TestServerDirectoriesAreOwnerOnlyWithAnInstalledModel(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "hfdesk.json"),
		StateDir:   filepath.Join(root, "state"),
		LogDir:     filepath.Join(root, "logs"),
	}
	runtimepaths.Install(model)
	t.Cleanup(runtimepaths.Reset)

	configDir := filepath.Join(t.TempDir(), "config")
	if err := saveConfigFile(filepath.Join(configDir, "hfdesk.json"), &ConfigFile{}); err != nil {
		t.Fatalf("save config file: %v", err)
	}
	assertDirMode(t, configDir, 0o700)

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := saveJobsState(filepath.Join(stateDir, "jobs_state.json"), nil); err != nil {
		t.Fatalf("save jobs state: %v", err)
	}
	assertDirMode(t, stateDir, 0o700)

	if err := AppendHistory(&Job{ID: "1", Repo: "owner/model"}); err != nil {
		t.Fatalf("append history: %v", err)
	}
	assertDirMode(t, model.StateDir, 0o700)
}

func TestServerDirectoriesKeepTheModeOfAnExistingDirectory(t *testing.T) {
	runtimepaths.Reset()
	t.Cleanup(runtimepaths.Reset)

	// MkdirAll never changes the mode of a directory that already exists,
	// so an existing directory keeps its mode.
	configDir := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveConfigFile(filepath.Join(configDir, "hfdesk.json"), &ConfigFile{}); err != nil {
		t.Fatalf("save config file: %v", err)
	}
	assertDirMode(t, configDir, 0o755)

	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveJobsState(filepath.Join(stateDir, "jobs_state.json"), nil); err != nil {
		t.Fatalf("save jobs state: %v", err)
	}
	assertDirMode(t, stateDir, 0o755)
}

func TestMirrorTargetsSaveCreatesTheStateDirectoryWithTheServerMode(t *testing.T) {
	t.Run("interactive", func(t *testing.T) {
		runtimepaths.Reset()
		t.Cleanup(runtimepaths.Reset)
		setServerSandboxEnv(t, t.TempDir())

		addMirrorTarget(t)

		// The state directory of an interactive run keeps the historical
		// mode, so the targets save cannot introduce a different one.
		assertDirMode(t, AppConfigDir(), 0o755)
		if _, err := os.Stat(filepath.Join(AppConfigDir(), "targets.yaml")); err != nil {
			t.Fatalf("stat targets file: %v", err)
		}
	})

	t.Run("installed", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		runtimepaths.Install(runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "hfdesk.json"),
			StateDir:   stateDir,
			LogDir:     filepath.Join(root, "logs"),
		})
		t.Cleanup(runtimepaths.Reset)

		addMirrorTarget(t)

		assertDirMode(t, stateDir, 0o700)
		if _, err := os.Stat(filepath.Join(stateDir, "targets.yaml")); err != nil {
			t.Fatalf("stat targets file: %v", err)
		}
	})
}

// addMirrorTarget adds a mirror target through the API.
func addMirrorTarget(t *testing.T) {
	t.Helper()

	s := newTestServer(t)
	req := httptest.NewRequest("POST", "/api/mirror/targets", strings.NewReader(`{"name":"backup","path":"/tmp/hfdesk-backup"}`))
	w := httptest.NewRecorder()
	s.handleMirrorTargetAdd(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

// --- helpers ---

// setServerSandboxEnv points every platform specific config location at a
// temporary root, so an interactive run resolves its per-user directories
// inside the sandbox of the test.
func setServerSandboxEnv(t *testing.T, root string) {
	t.Helper()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
}

// assertDirMode checks the permission bits of a directory. The umask of the
// process masks the mode bits of a created directory, so the expected mode is
// masked the same way.
func assertDirMode(t *testing.T, dir string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support POSIX permission bits")
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if got, expected := info.Mode().Perm(), want&^processUmask(t); got != expected {
		t.Errorf("%s mode = %o, want %o", dir, got, expected)
	}
}

// processUmask returns the permission bits the umask of the process removes
// from a created file or directory.
func processUmask(t *testing.T) os.FileMode {
	t.Helper()

	probe := filepath.Join(t.TempDir(), "umask-probe")
	if err := os.Mkdir(probe, 0o777); err != nil {
		t.Fatalf("create umask probe: %v", err)
	}
	info, err := os.Stat(probe)
	if err != nil {
		t.Fatalf("stat umask probe: %v", err)
	}
	return os.FileMode(0o777) &^ info.Mode().Perm()
}

// --- reference implementation ---
// The helpers below re-implement the documented interactive resolution without
// using the runtime path model, so the tests compare behaviour instead of
// comparing a function with itself.

func referenceConfigPath(t *testing.T) string {
	t.Helper()
	dirs := []string{referenceRunDir()}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, dir := range dirs {
		if referenceIsTempDir(dir) {
			continue
		}
		for _, name := range []string{"hfdesk.json", "hfdesk.yaml", "hfdesk.yml"} {
			p := filepath.Join(dir, name)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}

	configDir := referenceAppConfigDir()
	for _, name := range []string{"hfdesk.json", "hfdesk.yaml", "hfdesk.yml"} {
		p := filepath.Join(configDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(configDir, "hfdesk.json")
}

func referenceAppConfigDir() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "HFDesk")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "hfdesk")
	}
	return referenceRunDir()
}

func referenceRunDir() string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

func referenceIsTempDir(path string) bool {
	path = filepath.Clean(path)
	temp := filepath.Clean(os.TempDir())
	lower := strings.ToLower(path)
	if strings.HasPrefix(strings.ToLower(path), strings.ToLower(temp)) {
		return true
	}
	return strings.Contains(lower, "rar$") || strings.Contains(lower, ".rartemp")
}
