// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

// Package runtimepaths resolves the runtime file locations used by HFDesk.
//
// Interactive runs keep their historical behaviour: the config file, the jobs
// state, the download history and the mirror targets are derived from the
// current working directory and the per-user config directory, and are
// resolved again on every call so a changed environment is picked up.
//
// A supervised run (systemd, launchd, an init script, ...) can install one
// explicit model so every path comes from a deterministic root instead of
// from the working directory or the user profile of the service account.
package runtimepaths

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Paths is the startup-resolved runtime path model.
type Paths struct {
	// ConfigFile is the config file read at startup and written by settings.
	ConfigFile string
	// StateDir holds the mutable runtime state: jobs, history and mirror
	// targets.
	StateDir string
	// LogDir holds the process log files.
	LogDir string
}

// configFileNames are the accepted config file names, in search order.
var configFileNames = []string{"hfdesk.json", "hfdesk.yaml", "hfdesk.yml"}

// JobsStateFile returns the path of the persisted job list.
func (p Paths) JobsStateFile() string {
	return filepath.Join(p.StateDir, "jobs_state.json")
}

// HistoryFile returns the path of the download history.
func (p Paths) HistoryFile() string {
	return filepath.Join(p.StateDir, "download_history.json")
}

// TargetsFile returns the path of the mirror targets config.
func (p Paths) TargetsFile() string {
	return filepath.Join(p.StateDir, "targets.yaml")
}

var (
	installedMu sync.RWMutex
	installed   *Paths
)

// Install installs an explicit path model process-wide. Afterwards Current
// returns exactly p and ignores the working directory, HOME and the user
// profile.
func Install(p Paths) {
	installedMu.Lock()
	defer installedMu.Unlock()
	installed = &p
}

// Reset restores the interactive default resolution.
func Reset() {
	installedMu.Lock()
	defer installedMu.Unlock()
	installed = nil
}

// IsInstalled reports whether an explicit runtime path model is installed
// for this process. A supervised instance installs one; an interactive run
// resolves its paths per call and installs nothing.
func IsInstalled() bool {
	installedMu.RLock()
	defer installedMu.RUnlock()
	return installed != nil
}

// Current returns the effective path model. Without an installed model the
// interactive defaults are resolved again on every call, so a changed HOME,
// XDG_CONFIG_HOME, APPDATA or working directory takes effect immediately.
func Current() Paths {
	installedMu.RLock()
	p := installed
	installedMu.RUnlock()
	if p != nil {
		return *p
	}
	return interactivePaths()
}

// interactivePaths resolves the interactive path model from the current
// process environment.
func interactivePaths() Paths {
	stateDir := AppConfigDir()
	return Paths{
		ConfigFile: interactiveConfigFile(stateDir),
		StateDir:   stateDir,
		LogDir:     filepath.Join(stateDir, "logs"),
	}
}

// interactiveConfigFile resolves the config file the way an interactive run
// always has: an existing file next to the current or the executable
// directory first, then the per-user config directory, defaulting to a JSON
// file in the per-user config directory.
func interactiveConfigFile(stateDir string) string {
	dirs := []string{RunDir()}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}

	for _, dir := range dirs {
		if isTempDir(dir) {
			continue
		}
		if p, ok := firstExistingConfig(dir); ok {
			return p
		}
	}

	if p, ok := firstExistingConfig(stateDir); ok {
		return p
	}

	return filepath.Join(stateDir, "hfdesk.json")
}

// firstExistingConfig returns the first existing config file in dir.
func firstExistingConfig(dir string) (string, bool) {
	for _, name := range configFileNames {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// RunDir returns the directory where state files (config, jobs, history)
// should be stored. Priority: current working directory → executable dir → ".".
func RunDir() string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

// AppConfigDir returns the normal per-user HFDesk configuration directory.
func AppConfigDir() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "HFDesk")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "hfdesk")
	}
	return RunDir()
}

// isTempDir reports whether path looks like a temporary extraction directory.
// Such directories are skipped when searching for a config file so an
// unpacked archive never becomes the source of truth.
func isTempDir(path string) bool {
	path = filepath.Clean(path)
	temp := filepath.Clean(os.TempDir())
	lower := strings.ToLower(path)
	if strings.HasPrefix(strings.ToLower(path), strings.ToLower(temp)) {
		return true
	}
	return strings.Contains(lower, "rar$") || strings.Contains(lower, ".rartemp")
}
