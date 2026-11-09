// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// linuxSystemPaths are the machine-wide paths of a system scope instance on
// Linux and macOS.
var linuxSystemPaths = runtimepaths.Paths{
	ConfigFile: "/etc/hfdesk/hfdesk.json",
	StateDir:   "/var/lib/hfdesk",
	LogDir:     "/var/log/hfdesk",
}

func TestDefaultPathsUserScope(t *testing.T) {
	paths, err := DefaultPaths(ScopeUser)
	switch runtime.GOOS {
	case "linux", "darwin":
		if err != nil {
			t.Fatalf("DefaultPaths(ScopeUser): %v", err)
		}
	case "windows":
		// The Windows service control manager registers machine-wide
		// services only.
		if err == nil {
			t.Fatalf("DefaultPaths(ScopeUser) on windows = %+v, want an error", paths)
		}
		return
	default:
		if err == nil {
			t.Fatalf("DefaultPaths(ScopeUser) on %s = %+v, want an error", runtime.GOOS, paths)
		}
		return
	}

	// The user scope instance keeps its state under the per-user
	// configuration root, next to the settings of the interactive run.
	root := filepath.Join(runtimepaths.AppConfigDir(), "service")
	want := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "hfdesk.json"),
		StateDir:   root,
		LogDir:     filepath.Join(root, "logs"),
	}
	if paths != want {
		t.Errorf("DefaultPaths(ScopeUser) =\n%+v\nwant\n%+v", paths, want)
	}

	assertAbsolutePaths(t, "user scope", paths)

	// The log dir of the instance lives inside its state dir.
	if !isInsidePath(paths.LogDir, paths.StateDir) {
		t.Errorf("user scope log dir %q is not inside the state dir %q", paths.LogDir, paths.StateDir)
	}
	// The config file of the instance lives inside the parent of its state
	// root.
	if !isInsidePath(paths.ConfigFile, filepath.Dir(paths.StateDir)) {
		t.Errorf("user scope config file %q is not inside the state root parent %q", paths.ConfigFile, filepath.Dir(paths.StateDir))
	}
}

func TestDefaultPathsSystemScope(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin":
		paths, err := DefaultPaths(ScopeSystem)
		if err != nil {
			t.Fatalf("DefaultPaths(ScopeSystem): %v", err)
		}
		if paths != linuxSystemPaths {
			t.Errorf("DefaultPaths(ScopeSystem) =\n%+v\nwant\n%+v", paths, linuxSystemPaths)
		}
		assertAbsolutePaths(t, "system scope", paths)
	case "windows":
		programData := t.TempDir()
		t.Setenv("ProgramData", programData)

		paths, err := DefaultPaths(ScopeSystem)
		if err != nil {
			t.Fatalf("DefaultPaths(ScopeSystem): %v", err)
		}
		root := filepath.Join(programData, "HFDesk")
		want := runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "hfdesk.json"),
			StateDir:   root,
			LogDir:     filepath.Join(root, "logs"),
		}
		if paths != want {
			t.Errorf("DefaultPaths(ScopeSystem) =\n%+v\nwant\n%+v", paths, want)
		}
		assertAbsolutePaths(t, "system scope", paths)
		if !isInsidePath(paths.LogDir, paths.StateDir) {
			t.Errorf("system scope log dir %q is not inside the state dir %q", paths.LogDir, paths.StateDir)
		}

		// Without the environment variable the documented default is used.
		t.Setenv("ProgramData", "")
		paths, err = DefaultPaths(ScopeSystem)
		if err != nil {
			t.Fatalf("DefaultPaths(ScopeSystem): %v", err)
		}
		wantRoot := filepath.Join(`C:\ProgramData`, "HFDesk")
		if paths.StateDir != wantRoot {
			t.Errorf("DefaultPaths(ScopeSystem).StateDir = %q, want %q", paths.StateDir, wantRoot)
		}
	default:
		paths, err := DefaultPaths(ScopeSystem)
		if err == nil {
			t.Fatalf("DefaultPaths(ScopeSystem) on %s = %+v, want an error", runtime.GOOS, paths)
		}
	}
}

func TestDefaultPathsScopesDoNotOverlap(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("the scope roots are platform specific, running on %s", runtime.GOOS)
	}

	user, err := DefaultPaths(ScopeUser)
	if err != nil {
		t.Fatalf("DefaultPaths(ScopeUser): %v", err)
	}
	system, err := DefaultPaths(ScopeSystem)
	if err != nil {
		t.Fatalf("DefaultPaths(ScopeSystem): %v", err)
	}

	if user.StateDir == system.StateDir {
		t.Errorf("both scopes use the state dir %q", user.StateDir)
	}

	userPaths := []string{user.ConfigFile, user.StateDir, user.LogDir}
	systemPaths := []string{system.ConfigFile, system.StateDir, system.LogDir}

	for _, p := range userPaths {
		for _, root := range systemPaths {
			if isInsidePath(p, root) {
				t.Errorf("user scope path %q is inside the system scope path %q", p, root)
			}
		}
	}
	for _, p := range systemPaths {
		for _, root := range userPaths {
			if isInsidePath(p, root) {
				t.Errorf("system scope path %q is inside the user scope path %q", p, root)
			}
		}
	}

	// The machine-wide roots are the documented ones and never appear in a
	// user scope path.
	for _, p := range userPaths {
		for _, root := range []string{"/etc/hfdesk", "/var/lib/hfdesk", "/var/log/hfdesk"} {
			if isInsidePath(p, root) {
				t.Errorf("user scope path %q is inside the system scope root %q", p, root)
			}
		}
	}
}

func TestDefaultCacheDirsSystemScope(t *testing.T) {
	paths, err := DefaultPaths(ScopeSystem)
	if err != nil {
		t.Fatalf("DefaultPaths(ScopeSystem): %v", err)
	}

	root, hub := DefaultCacheDirs(ScopeSystem, paths)
	wantRoot := filepath.Join(paths.StateDir, "cache")
	if root != wantRoot {
		t.Errorf("cache root = %q, want %q", root, wantRoot)
	}
	if hub != filepath.Join(wantRoot, "hub") {
		t.Errorf("hub dir = %q, want %q", hub, filepath.Join(wantRoot, "hub"))
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(hub) {
		t.Errorf("cache paths are not absolute: root %q, hub %q", root, hub)
	}

	// The machine-wide instance never uses the cache of the invoking user.
	home, err := os.UserHomeDir()
	if err != nil || home == "" || isRootDir(home) {
		return
	}
	for _, p := range []string{root, hub} {
		if isInsidePath(p, home) {
			t.Errorf("system scope cache path %q is inside the home directory %q", p, home)
		}
	}
}

func TestDefaultCacheDirsUserScope(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has no user scope service")
	}
	paths, err := DefaultPaths(ScopeUser)
	if err != nil {
		t.Fatalf("DefaultPaths(ScopeUser): %v", err)
	}

	tmp := t.TempDir()
	cacheRoot := filepath.Join(tmp, "hf")
	t.Setenv("HF_HOME", cacheRoot)
	t.Setenv("HF_HUB_CACHE", "")

	root, hub := DefaultCacheDirs(ScopeUser, paths)
	if root != cacheRoot {
		t.Errorf("cache root = %q, want %q", root, cacheRoot)
	}
	if hub != filepath.Join(root, "hub") {
		t.Errorf("hub dir = %q, want %q", hub, filepath.Join(root, "hub"))
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(hub) {
		t.Errorf("cache paths are not absolute: root %q, hub %q", root, hub)
	}

	// An explicit hub cache wins over the default hub of the root.
	hubDir := filepath.Join(tmp, "hub")
	t.Setenv("HF_HUB_CACHE", hubDir)
	root, hub = DefaultCacheDirs(ScopeUser, paths)
	if hub != hubDir {
		t.Errorf("hub dir = %q, want %q", hub, hubDir)
	}
	if root != cacheRoot {
		t.Errorf("cache root = %q, want %q", root, cacheRoot)
	}
}

func TestDefaultCacheDirsUserScopeIgnoresTheSystemStateRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has no user scope service")
	}
	system, err := DefaultPaths(ScopeSystem)
	if err != nil {
		t.Fatalf("DefaultPaths(ScopeSystem): %v", err)
	}

	// A user scope instance keeps its own cache, even when the paths of the
	// machine-wide instance are handed to it.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HF_HOME", "")
	t.Setenv("HF_HUB_CACHE", "")

	root, hub := DefaultCacheDirs(ScopeUser, system)
	if isInsidePath(root, system.StateDir) {
		t.Errorf("user scope cache root %q is inside the system state dir %q", root, system.StateDir)
	}
	if isInsidePath(hub, system.StateDir) {
		t.Errorf("user scope hub dir %q is inside the system state dir %q", hub, system.StateDir)
	}
}

func TestDefaultPathsRejectsUnknownScope(t *testing.T) {
	paths, err := DefaultPaths(Scope("bogus"))
	if err == nil {
		t.Fatalf("DefaultPaths(bogus) = %+v, want an error", paths)
	}
}

func TestDefaultCacheDirsUserScopeRootIsAbsolute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the windows home directory comes from USERPROFILE")
	}
	// Without HF_HOME and without a resolvable home directory the default
	// cache dir is relative: the reported root must still be absolute.
	t.Setenv("HF_HOME", "")
	t.Setenv("HF_HUB_CACHE", "")
	t.Setenv("HOME", "")

	root, hub := DefaultCacheDirs(ScopeUser, runtimepaths.Paths{})
	if !filepath.IsAbs(root) {
		t.Errorf("cache root %q is not absolute", root)
	}
	if !filepath.IsAbs(hub) {
		t.Errorf("hub dir %q is not absolute", hub)
	}
}

// assertAbsolutePaths checks that every path of a path model is absolute.
func assertAbsolutePaths(t *testing.T, scope string, paths runtimepaths.Paths) {
	t.Helper()
	for name, p := range map[string]string{
		"config file": paths.ConfigFile,
		"state dir":   paths.StateDir,
		"log dir":     paths.LogDir,
	} {
		if !filepath.IsAbs(p) {
			t.Errorf("%s %s %q is not an absolute path", scope, name, p)
		}
	}
}

// isInsidePath reports whether path is inside dir.
func isInsidePath(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
