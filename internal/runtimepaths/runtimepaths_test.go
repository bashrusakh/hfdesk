// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package runtimepaths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentReturnsInstalledModelVerbatim(t *testing.T) {
	model := Paths{
		ConfigFile: filepath.Join("/etc", "hfdesk", "hfdesk.yaml"),
		StateDir:   filepath.Join("/var", "lib", "hfdesk"),
		LogDir:     filepath.Join("/var", "log", "hfdesk"),
	}
	Install(model)
	t.Cleanup(Reset)

	if got := Current(); got != model {
		t.Fatalf("Current() = %#v, want the installed model %#v", got, model)
	}
}

func TestInstalledModelIgnoresEnvironmentAndWorkingDirectory(t *testing.T) {
	model := Paths{
		ConfigFile: filepath.Join("/srv", "hfdesk", "hfdesk.json"),
		StateDir:   filepath.Join("/srv", "hfdesk", "state"),
		LogDir:     filepath.Join("/srv", "hfdesk", "logs"),
	}
	Install(model)
	t.Cleanup(Reset)

	setSandboxEnv(t, t.TempDir())
	t.Chdir(t.TempDir())

	if got := Current(); got != model {
		t.Fatalf("Current() = %#v, want the installed model %#v", got, model)
	}
}

func TestInteractiveCurrentFollowsEnvironment(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	setSandboxEnv(t, t.TempDir())

	got := Current()
	if want := referencePaths(t); got != want {
		t.Fatalf("interactive resolution mismatch:\n got: %#v\nwant: %#v", got, want)
	}

	// The interactive model is resolved on every call, so a later environment
	// change must be picked up.
	setSandboxEnv(t, t.TempDir())

	got = Current()
	if got2 := referencePaths(t); got != got2 {
		t.Fatalf("interactive resolution did not follow the environment change:\n got: %#v\nwant: %#v", got, got2)
	}
}

func TestStateDirReturnsTheInstalledStateDirectory(t *testing.T) {
	model := Paths{
		ConfigFile: filepath.Join("/etc", "hfdesk", "hfdesk.json"),
		StateDir:   filepath.Join("/var", "lib", "hfdesk"),
		LogDir:     filepath.Join("/var", "log", "hfdesk"),
	}
	Install(model)
	t.Cleanup(Reset)

	if got, want := StateDir(), model.StateDir; got != want {
		t.Fatalf("StateDir() = %q, want the installed state directory %q", got, want)
	}

	// The installed state directory does not depend on the environment.
	setSandboxEnv(t, t.TempDir())
	t.Chdir(t.TempDir())

	if got, want := StateDir(), model.StateDir; got != want {
		t.Fatalf("StateDir() = %q, want the installed state directory %q", got, want)
	}
}

func TestStateDirFollowsTheInteractiveEnvironment(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	root := t.TempDir()
	setSandboxEnv(t, root)

	got := StateDir()
	if want := referenceAppConfigDir(); got != want {
		t.Fatalf("StateDir() = %q, want %q", got, want)
	}
	if !strings.Contains(got, root) {
		t.Fatalf("StateDir() = %q, want a directory inside the sandbox %q", got, root)
	}

	// The state directory is resolved on every call, so a later environment
	// change must be picked up.
	next := t.TempDir()
	setSandboxEnv(t, next)

	got = StateDir()
	if want := referenceAppConfigDir(); got != want {
		t.Fatalf("StateDir() after the environment change = %q, want %q", got, want)
	}
	if !strings.Contains(got, next) {
		t.Fatalf("StateDir() = %q, want a directory inside the sandbox %q", got, next)
	}
}

func TestStateDirDoesNotResolveTheConfigFile(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	// The home directory has to be captured before the sandbox environment
	// replaces it: the run directory must not look like a temporary one.
	home := currentHomeDir(t)

	setSandboxEnv(t, t.TempDir())

	stateDir := StateDir()

	// A config file next to the run directory is the config file of the
	// model, but it does not change the state directory.
	runDir := nonTempRunDir(t, home)
	if err := os.WriteFile(filepath.Join(runDir, "hfdesk.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(runDir)

	if got, want := Current().ConfigFile, filepath.Join(runDir, "hfdesk.json"); got != want {
		t.Fatalf("config file = %q, want %q", got, want)
	}
	if got := StateDir(); got != stateDir {
		t.Fatalf("StateDir() = %q, want the unchanged state directory %q", got, stateDir)
	}
	if got, want := StateDir(), referenceAppConfigDir(); got != want {
		t.Fatalf("StateDir() = %q, want %q", got, want)
	}
}

func TestStateDirAgreesWithCurrent(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	model := Paths{
		ConfigFile: filepath.Join("/etc", "hfdesk", "hfdesk.json"),
		StateDir:   filepath.Join("/var", "lib", "hfdesk"),
		LogDir:     filepath.Join("/var", "log", "hfdesk"),
	}

	check := func(stage string) {
		t.Helper()
		if got, want := StateDir(), Current().StateDir; got != want {
			t.Fatalf("%s: StateDir() = %q, want Current().StateDir = %q", stage, got, want)
		}
	}

	setSandboxEnv(t, t.TempDir())
	check("interactive")

	Install(model)
	check("installed")

	// An installed model without a state directory keeps its empty state
	// directory: the accessor does not invent one.
	Install(Paths{})
	check("installed without a state directory")

	Reset()
	check("after reset")

	if got, want := StateDir(), referenceAppConfigDir(); got != want {
		t.Fatalf("StateDir() after Reset = %q, want %q", got, want)
	}
}

func TestStateDirAgreesWithCurrentWithoutAnEnvironment(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	// Without a home and a config directory the interactive model falls
	// back to the run directory. The accessor has to agree there too.
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "APPDATA", "LOCALAPPDATA"} {
		t.Setenv(key, "")
	}

	if got, want := StateDir(), Current().StateDir; got != want {
		t.Fatalf("StateDir() = %q, want Current().StateDir = %q", got, want)
	}
	if got := StateDir(); got == "" {
		t.Fatalf("StateDir() = %q, want a usable path", got)
	}
}

func TestDerivedFileNamesLiveInStateDir(t *testing.T) {
	stateDir := filepath.Join("var", "lib", "hfdesk")
	p := Paths{StateDir: stateDir}

	if got, want := p.JobsStateFile(), filepath.Join(stateDir, "jobs_state.json"); got != want {
		t.Errorf("JobsStateFile() = %q, want %q", got, want)
	}
	if got, want := p.HistoryFile(), filepath.Join(stateDir, "download_history.json"); got != want {
		t.Errorf("HistoryFile() = %q, want %q", got, want)
	}
	if got, want := p.TargetsFile(), filepath.Join(stateDir, "targets.yaml"); got != want {
		t.Errorf("TargetsFile() = %q, want %q", got, want)
	}
}

func TestIsInstalledTracksTheInstalledModel(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	if IsInstalled() {
		t.Fatalf("IsInstalled() = true before a model was installed")
	}

	Install(Paths{
		ConfigFile: filepath.Join("/etc", "hfdesk", "hfdesk.json"),
		StateDir:   filepath.Join("/var", "lib", "hfdesk"),
		LogDir:     filepath.Join("/var", "log", "hfdesk"),
	})
	if !IsInstalled() {
		t.Fatalf("IsInstalled() = false after Install")
	}

	Reset()
	if IsInstalled() {
		t.Fatalf("IsInstalled() = true after Reset")
	}
}

func TestResetRestoresInteractiveResolution(t *testing.T) {
	model := Paths{
		ConfigFile: filepath.Join("/etc", "hfdesk", "hfdesk.json"),
		StateDir:   filepath.Join("/var", "lib", "hfdesk"),
		LogDir:     filepath.Join("/var", "log", "hfdesk"),
	}
	Install(model)
	if got := Current(); got != model {
		t.Fatalf("Current() = %#v, want the installed model %#v", got, model)
	}

	Reset()

	got := Current()
	if got == model {
		t.Fatalf("Reset did not restore the interactive resolution: %#v", got)
	}
	if want := referencePaths(t); got != want {
		t.Fatalf("interactive resolution mismatch after Reset:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestInteractiveConfigFileSearchOrder(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	// The home directory has to be captured before the sandbox environment
	// replaces it: the run directory must not look like a temporary one.
	home := currentHomeDir(t)

	sandbox := t.TempDir()
	setSandboxEnv(t, sandbox)

	// The per-user config directory holds a YAML config.
	cfgDir := AppConfigDir()
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userConfig := filepath.Join(cfgDir, "hfdesk.yaml")
	if err := os.WriteFile(userConfig, []byte("cache-dir: /tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runDir := nonTempRunDir(t, home)
	runConfig := filepath.Join(runDir, "hfdesk.json")
	if err := os.WriteFile(runConfig, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(runDir)

	// A config next to the run directory wins over the per-user config.
	if got, want := Current().ConfigFile, runConfig; got != want {
		t.Fatalf("config file = %q, want the run directory config %q", got, want)
	}

	// Without it the per-user config is used.
	if err := os.Remove(runConfig); err != nil {
		t.Fatal(err)
	}
	if got, want := Current().ConfigFile, userConfig; got != want {
		t.Fatalf("config file = %q, want the per-user config %q", got, want)
	}

	// Without any config file the default JSON file in the per-user config
	// directory is used.
	if err := os.Remove(userConfig); err != nil {
		t.Fatal(err)
	}
	if got, want := Current().ConfigFile, filepath.Join(cfgDir, "hfdesk.json"); got != want {
		t.Fatalf("config file = %q, want the default %q", got, want)
	}
}

// --- helpers ---

// setSandboxEnv points every platform specific config location at root.
func setSandboxEnv(t *testing.T, root string) {
	t.Helper()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
}

// currentHomeDir returns the home directory of the current environment.
func currentHomeDir(t *testing.T) string {
	t.Helper()
	for _, key := range []string{"HOME", "USERPROFILE"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available: %v", err)
	}
	return home
}

// nonTempRunDir creates a run directory that the interactive search does not
// skip as a temporary extraction directory.
func nonTempRunDir(t *testing.T, home string) string {
	t.Helper()
	dir, err := os.MkdirTemp(home, "hfdesk-runtimepaths-run-")
	if err != nil {
		t.Skipf("cannot create a run directory under %s: %v", home, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if isTempDir(dir) {
		t.Skipf("home directory %s is inside the temporary directory", home)
	}
	return dir
}

// --- reference implementation ---
//
// The helpers below re-implement the documented interactive resolution without
// using the code under test, so the tests compare behaviour instead of
// comparing a function with itself.

// referencePaths resolves the documented interactive path model.
func referencePaths(t *testing.T) Paths {
	t.Helper()
	stateDir := referenceAppConfigDir()
	return Paths{
		ConfigFile: referenceConfigFile(t, stateDir),
		StateDir:   stateDir,
		LogDir:     filepath.Join(stateDir, "logs"),
	}
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

func referenceConfigFile(t *testing.T, stateDir string) string {
	t.Helper()
	dirs := []string{referenceRunDir()}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, dir := range dirs {
		if referenceIsTempDir(dir) {
			continue
		}
		if p, ok := referenceFirstExistingConfig(dir); ok {
			return p
		}
	}
	if p, ok := referenceFirstExistingConfig(stateDir); ok {
		return p
	}
	return filepath.Join(stateDir, "hfdesk.json")
}

func referenceFirstExistingConfig(dir string) (string, bool) {
	for _, name := range []string{"hfdesk.json", "hfdesk.yaml", "hfdesk.yml"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
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
