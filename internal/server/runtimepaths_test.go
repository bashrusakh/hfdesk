// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// --- reference implementation ---
//
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
