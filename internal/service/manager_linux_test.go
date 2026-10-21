// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// systemdTestOptions returns an installation for the systemd tests. The
// paths are absolute and carry no access data, so the checks of the
// rendered unit stay exact.
func systemdTestOptions(t *testing.T, scope Scope) InstallOptions {
	t.Helper()
	root := "/opt/hfdesk"
	if scope == ScopeSystem {
		root = "/srv/hfdesk"
	}
	return InstallOptions{
		Scope:      scope,
		Executable: root + "/bin/hfdesk",
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: root + "/etc/hfdesk/hfdesk.json",
			StateDir:   root + "/var/lib/hfdesk",
			LogDir:     root + "/var/log/hfdesk",
		},
		CacheDir: root + "/cache",
		HubDir:   root + "/cache/hub",
		LocalDir: root + "/models",
	}
}

// unitDirectives returns the effective lines of a unit file: the lines the
// supervisor reads, without the comments and the empty lines.
func unitDirectives(unit string) []string {
	var directives []string
	for _, line := range strings.Split(unit, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		directives = append(directives, trimmed)
	}
	return directives
}

// systemdTestExecutable writes a program that can be registered and returns
// its path. An installation refuses a program that does not exist, so every
// test of an installation needs one.
func systemdTestExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hfdesk")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("write the program: %v", err)
	}
	return path
}

func TestSystemdRenderUnitUserScope(t *testing.T) {
	opts := systemdTestOptions(t, ScopeUser)

	unit, err := renderUnit(opts)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}

	for _, want := range []string{
		"Description=HFDesk",
		"Documentation=https://github.com/bashrusakh/hfdesk",
		"After=network-online.target",
		"Wants=network-online.target",
		"Type=simple",
		"Restart=on-failure",
		"RestartSec=5s",
		"NoNewPrivileges=true",
		"UMask=0077",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit does not contain %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "WantedBy=multi-user.target") {
		t.Errorf("a user scope unit must not be enabled for the machine:\n%s", unit)
	}
	// A user scope instance runs as the user of the session: the unit
	// names no account at all.
	for _, directive := range unitDirectives(unit) {
		if strings.HasPrefix(directive, "User=") || strings.HasPrefix(directive, "Group=") {
			t.Errorf("a user scope unit names an account: %q", directive)
		}
	}

	parsed := parseUnit(unit)
	if !filepath.IsAbs(parsed.ExecStart) {
		t.Errorf("ExecStart = %q, want an absolute path", parsed.ExecStart)
	}
	if parsed.ExecStart != opts.Executable {
		t.Errorf("ExecStart = %q, want %q", parsed.ExecStart, opts.Executable)
	}
	if parsed.WorkingDirectory != opts.Paths.StateDir {
		t.Errorf("WorkingDirectory = %q, want the state dir %q", parsed.WorkingDirectory, opts.Paths.StateDir)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() = %v, want nil", err)
	}
	if got := parsed.Environment["HOME"]; got != home {
		t.Errorf("Environment HOME = %q, want the home of the user %q", got, home)
	}

	// The registered command is the canonical argument vector of the
	// shared contract.
	wantArgs := RunArgs(opts)
	if len(parsed.ExecArgs) != len(wantArgs) {
		t.Fatalf("the arguments of the unit = %v, want %v", parsed.ExecArgs, wantArgs)
	}
	for i := range wantArgs {
		if parsed.ExecArgs[i] != wantArgs[i] {
			t.Fatalf("the arguments of the unit = %v, want %v", parsed.ExecArgs, wantArgs)
		}
	}
}

func TestSystemdRenderUnitSystemScope(t *testing.T) {
	opts := systemdTestOptions(t, ScopeSystem)

	unit, err := renderUnit(opts)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}

	if !strings.Contains(unit, "WantedBy=multi-user.target") {
		t.Errorf("the unit does not contain %q:\n%s", "WantedBy=multi-user.target", unit)
	}
	if strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("a system scope unit must not be enabled for a user session:\n%s", unit)
	}

	parsed := parseUnit(unit)
	if got := parsed.Environment["HOME"]; got != "/var/lib/hfdesk" {
		t.Errorf("Environment HOME = %q, want %q", got, "/var/lib/hfdesk")
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if got := parsed.Environment["HOME"]; got == home {
			t.Errorf("Environment HOME = %q, want the machine level home of a system scope instance", got)
		}
	}
	// A machine wide instance runs as the dedicated service account of
	// the platform, never as the user who runs the install command.
	account := DefaultServiceUser()
	if !strings.Contains(unit, "User="+account+"\n") {
		t.Errorf("the unit does not run as the service account %q:\n%s", account, unit)
	}
	if !strings.Contains(unit, "Group="+account+"\n") {
		t.Errorf("the unit does not run in the group of the service account %q:\n%s", account, unit)
	}
	if current, err := user.Current(); err == nil && current.Username != "" {
		for _, directive := range unitDirectives(unit) {
			if directive == "User="+current.Username {
				t.Errorf("the unit runs as the user who installed it: %q", directive)
			}
		}
	}
}

func TestSystemdRenderUnitCarriesNoCredential(t *testing.T) {
	const sentinel = "hf_zzSENTINELzz_9f3a"

	opts := systemdTestOptions(t, ScopeUser)
	// Every field that could plausibly carry access data is filled with
	// the sentinel, so the check keeps its meaning when such a field is
	// added.
	setCredentialFields(&opts, sentinel)

	unit, err := renderUnit(opts)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}
	if strings.Contains(unit, sentinel) {
		t.Errorf("the unit leaks the sentinel %q:\n%s", sentinel, unit)
	}
	for _, directive := range unitDirectives(unit) {
		if looksLikeCredential(directive) {
			t.Errorf("the unit carries a credential looking directive %q", directive)
		}
	}

	// The supervisor owns the lifecycle: no fork, no detached process, no
	// pid file, no background shell.
	for _, forbidden := range []string{"nohup", "Type=forking", "PIDFile=", "StandardOutput=file:", "StandardError=file:"} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("the unit contains %q:\n%s", forbidden, unit)
		}
	}
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), "&") {
			t.Errorf("the unit puts a command into the background: %q", line)
		}
	}
}

func TestSystemdRenderUnitQuotesPathsWithSpaces(t *testing.T) {
	opts := systemdTestOptions(t, ScopeUser)
	opts.Executable = "/opt/HF Desk/bin/hfdesk"
	opts.Paths.StateDir = "/var/lib/HF Desk"

	unit, err := renderUnit(opts)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}
	if !strings.Contains(unit, `ExecStart="/opt/HF Desk/bin/hfdesk"`) {
		t.Errorf("the executable is not quoted:\n%s", unit)
	}
	if !strings.Contains(unit, `"/var/lib/HF Desk"`) {
		t.Errorf("an argument with a space is not quoted:\n%s", unit)
	}

	parsed := parseUnit(unit)
	if parsed.ExecStart != opts.Executable {
		t.Errorf("ExecStart = %q, want %q", parsed.ExecStart, opts.Executable)
	}
	if got := parsed.flagValue("--state-dir"); got != opts.Paths.StateDir {
		t.Errorf("--state-dir = %q, want %q", got, opts.Paths.StateDir)
	}
	if parsed.WorkingDirectory != opts.Paths.StateDir {
		t.Errorf("WorkingDirectory = %q, want %q", parsed.WorkingDirectory, opts.Paths.StateDir)
	}
}

func TestSystemdUnitPaths(t *testing.T) {
	t.Run("user scope with XDG_CONFIG_HOME", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "/tmp/hfdesk-xdg")
		m := &systemdManager{scope: ScopeUser}
		got, err := m.unitPath()
		if err != nil {
			t.Fatalf("unitPath() = %v, want nil", err)
		}
		want := "/tmp/hfdesk-xdg/systemd/user/hfdesk.service"
		if got != want {
			t.Errorf("unitPath() = %q, want %q", got, want)
		}
	})

	t.Run("user scope without XDG_CONFIG_HOME", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", "/home/hfdesk-user")
		m := &systemdManager{scope: ScopeUser}
		got, err := m.unitPath()
		if err != nil {
			t.Fatalf("unitPath() = %v, want nil", err)
		}
		want := "/home/hfdesk-user/.config/systemd/user/hfdesk.service"
		if got != want {
			t.Errorf("unitPath() = %q, want %q", got, want)
		}
	})

	t.Run("system scope", func(t *testing.T) {
		m := &systemdManager{scope: ScopeSystem}
		got, err := m.unitPath()
		if err != nil {
			t.Fatalf("unitPath() = %v, want nil", err)
		}
		want := "/etc/systemd/system/hfdesk.service"
		if got != want {
			t.Errorf("unitPath() = %q, want %q", got, want)
		}
	})
}

func TestSystemdParseShow(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   State
	}{
		{
			name:   "not found",
			output: "LoadState=not-found\nActiveState=inactive\nSubState=dead",
			want:   StateNotInstalled,
		},
		{
			name:   "active",
			output: "LoadState=loaded\nActiveState=active\nSubState=running",
			want:   StateRunning,
		},
		{
			name:   "inactive",
			output: "LoadState=loaded\nActiveState=inactive\nSubState=dead",
			want:   StateStopped,
		},
		{
			name:   "activating",
			output: "LoadState=loaded\nActiveState=activating\nSubState=start",
			want:   StateStarting,
		},
		{
			name:   "deactivating",
			output: "LoadState=loaded\nActiveState=deactivating\nSubState=stop",
			want:   StateStopping,
		},
		{
			name:   "failed",
			output: "LoadState=loaded\nActiveState=failed\nSubState=failed",
			want:   StateFailed,
		},
		{
			name:   "unknown active state",
			output: "LoadState=loaded\nActiveState=reloading\nSubState=reload",
			want:   StateUnknown,
		},
		{
			name:   "empty output",
			output: "",
			want:   StateUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSystemdShow(tc.output)
			if got.State != tc.want {
				t.Errorf("parseSystemdShow(%q).State = %q, want %q", tc.output, got.State, tc.want)
			}
		})
	}
}

func TestSystemdParseShowReadsTheProperties(t *testing.T) {
	output := "LoadState=loaded\nActiveState=active\nSubState=running\n"
	show := parseSystemdShow(output)
	if show.LoadState != "loaded" {
		t.Errorf("LoadState = %q, want %q", show.LoadState, "loaded")
	}
	if show.ActiveState != "active" {
		t.Errorf("ActiveState = %q, want %q", show.ActiveState, "active")
	}
	if show.SubState != "running" {
		t.Errorf("SubState = %q, want %q", show.SubState, "running")
	}
}

func TestSystemdParseUnitRecoversTheRegistration(t *testing.T) {
	opts := systemdTestOptions(t, ScopeUser)

	unit, err := renderUnit(opts)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}
	parsed := parseUnit(unit)

	if parsed.ExecStart != opts.Executable {
		t.Errorf("the registered executable = %q, want %q", parsed.ExecStart, opts.Executable)
	}
	if parsed.WorkingDirectory != opts.Paths.StateDir {
		t.Errorf("the registered working directory = %q, want %q", parsed.WorkingDirectory, opts.Paths.StateDir)
	}
	if got := parsed.listen(); got != opts.Listen {
		t.Errorf("the registered listen address = %q, want %q", got, opts.Listen)
	}
	paths := parsed.paths()
	if paths.ConfigFile != opts.Paths.ConfigFile {
		t.Errorf("the registered config file = %q, want %q", paths.ConfigFile, opts.Paths.ConfigFile)
	}
	if paths.StateDir != opts.Paths.StateDir {
		t.Errorf("the registered state dir = %q, want %q", paths.StateDir, opts.Paths.StateDir)
	}
	if paths.LogDir != opts.Paths.LogDir {
		t.Errorf("the registered log dir = %q, want %q", paths.LogDir, opts.Paths.LogDir)
	}
}

// TestSystemdAnalyzeVerify checks the rendered unit with the verifier of the
// supervisor itself: a unit that the supervisor cannot parse is not a unit.
//
// The unit of the test points at the test binary, so the verifier finds an
// executable that really exists.
func TestSystemdAnalyzeVerify(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skipf("skipping: systemd-analyze is not available in this environment: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() = %v, want nil", err)
	}

	root := t.TempDir()
	opts := systemdTestOptions(t, ScopeUser)
	opts.Executable = executable
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "hfdesk.json"),
		StateDir:   root,
		LogDir:     filepath.Join(root, "logs"),
	}
	if err := os.MkdirAll(opts.Paths.LogDir, 0o700); err != nil {
		t.Fatal(err)
	}

	unit, err := renderUnit(opts)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}
	path := filepath.Join(root, unitName)
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, analyze, "verify", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("systemd-analyze verify %s = %v\n%s\nthe unit:\n%s", path, err, stderr.String(), unit)
	}
}

// systemdUserSessionUsable reports whether the supervisor of the session of
// the user can be reached.
func systemdUserSessionUsable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=Version")
	out, err := cmd.CombinedOutput()
	return err == nil && strings.Contains(string(out), "Version=")
}

// waitForState waits until the supervisor reports the wanted state.
func waitForState(t *testing.T, ctx context.Context, m *systemdManager, want State) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last State
	for time.Now().Before(deadline) {
		status, err := m.Status(ctx)
		if err == nil {
			last = status.State
			if last == want {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the state of the unit = %q, want %q", last, want)
}

// TestSystemdUserIntegrationCycle runs a full lifecycle of a user scope
// installation against the systemd of the machine.
//
// The test skips itself when the environment has no usable user systemd
// session. It never runs as root: a root process would install a unit of
// the machine instead of a unit of the session of a user.
func TestSystemdUserIntegrationCycle(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping: the integration cycle must not run as root against a real unit of the machine")
	}
	if !systemdUserSessionUsable() {
		t.Skip("skipping: this environment has no usable user systemd session")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() = %v, want nil", err)
	}

	m := &systemdManager{scope: ScopeUser}
	unitPath, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	// The user manager of the machine loads the units of the
	// configuration directory of the user. When the two do not agree, the
	// supervisor would never see the unit of the test.
	managerPath := filepath.Join(home, ".config", "systemd", "user", unitName)
	if unitPath != managerPath {
		t.Skipf("skipping: the user manager of this environment loads the units of %s, the manager resolves %s", managerPath, unitPath)
	}
	if _, err := os.Stat(unitPath); err == nil {
		t.Skipf("skipping: %s already exists, the test refuses to replace an existing registration", unitPath)
	}

	root := t.TempDir()
	executable := filepath.Join(root, "bin", "hfdesk")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	// The probe stays in the foreground and ignores the arguments of the
	// canonical vector.
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexec sleep 120\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	opts := InstallOptions{
		Scope:      ScopeUser,
		Executable: executable,
		Listen:     "127.0.0.1:18099",
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var"),
			LogDir:     filepath.Join(root, "var", "logs"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = m.Uninstall(cleanupCtx)
		_ = os.Remove(unitPath)
		_, _ = m.runSystemctl(cleanupCtx, "daemon-reload")
	})

	if err := m.Install(ctx, opts); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}

	info, err := os.Stat(unitPath)
	if err != nil {
		t.Fatalf("the unit file was not written: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("the mode of the unit file = %v, want 0644", info.Mode().Perm())
	}
	content, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Type=simple") {
		t.Errorf("the unit is not a simple service:\n%s", content)
	}

	status, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if status.State != StateStopped {
		t.Errorf("the state after the installation = %q, want %q", status.State, StateStopped)
	}
	if status.Executable != opts.Executable {
		t.Errorf("the registered executable = %q, want %q", status.Executable, opts.Executable)
	}
	if status.Listen != opts.Listen {
		t.Errorf("the registered listen address = %q, want %q", status.Listen, opts.Listen)
	}
	if status.UnitPath != unitPath {
		t.Errorf("the unit path = %q, want %q", status.UnitPath, unitPath)
	}
	if status.Paths.ConfigFile != opts.Paths.ConfigFile {
		t.Errorf("the registered config file = %q, want %q", status.Paths.ConfigFile, opts.Paths.ConfigFile)
	}

	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	waitForState(t, ctx, m, StateRunning)
	// Starting a running service is a stable success.
	if err := m.Start(ctx); err != nil {
		t.Errorf("the second Start() = %v, want nil", err)
	}

	if err := m.Stop(ctx); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	waitForState(t, ctx, m, StateStopped)
	// Stopping a stopped service is a stable success.
	if err := m.Stop(ctx); err != nil {
		t.Errorf("the second Stop() = %v, want nil", err)
	}

	if err := m.Uninstall(ctx); err != nil {
		t.Fatalf("Uninstall() = %v, want nil", err)
	}
	if _, err := os.Stat(unitPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the unit file still exists after the uninstallation: %v", err)
	}
	// The uninstallation removes the registration only.
	if _, err := os.Stat(opts.Paths.StateDir); err != nil {
		t.Errorf("the state directory was removed: %v", err)
	}
	if _, err := os.Stat(opts.Paths.LogDir); err != nil {
		t.Errorf("the log directory was removed: %v", err)
	}

	status, err = m.Status(ctx)
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if status.State != StateNotInstalled {
		t.Errorf("the state after the uninstallation = %q, want %q", status.State, StateNotInstalled)
	}

	// Uninstalling an absent service is a clean success.
	if err := m.Uninstall(ctx); err != nil {
		t.Errorf("the second Uninstall() = %v, want nil", err)
	}
}

// TestSystemdPathInsideHome pins the semantics of the containment helper the
// guard of the machine-wide scope relies on.
//
// The test is lexical, the home directory itself counts as inside it, and a
// sibling that shares the prefix of the home does not.
func TestSystemdPathInsideHome(t *testing.T) {
	const home = "/home/user"
	cases := []struct {
		name string
		path string
		want bool
	}{
		{name: "a directory inside the home", path: "/home/user/.config/HFDesk/service", want: true},
		{name: "the home itself", path: "/home/user", want: true},
		{name: "a sibling with a common prefix", path: "/home/user2", want: false},
		{name: "a directory outside the home", path: "/srv/hfdesk", want: false},
		{name: "a path that leaves the home again", path: "/home/user/../user2", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hfdownloader.PathInside(home, tc.path); got != tc.want {
				t.Errorf("PathInside(%q, %q) = %v, want %v", home, tc.path, got, tc.want)
			}
		})
	}
}

// TestSystemdCheckSystemPaths checks the guard of the machine-wide scope: a
// path inside the home directory of the user who runs the install command is
// refused, the machine-level paths are accepted, and a user scope instance is
// not touched at all.
func TestSystemdCheckSystemPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	inside := filepath.Join(home, ".config", "HFDesk", "service")
	// A sibling of the home that shares its prefix is not inside the home.
	sibling := home + "2"

	refused := []struct {
		name  string
		field string
		apply func(*InstallOptions)
	}{
		{name: "config file", field: "config file", apply: func(o *InstallOptions) { o.Paths.ConfigFile = inside }},
		{name: "state dir", field: "state dir", apply: func(o *InstallOptions) { o.Paths.StateDir = inside }},
		{name: "log dir", field: "log dir", apply: func(o *InstallOptions) { o.Paths.LogDir = inside }},
		{name: "cache dir", field: "cache dir", apply: func(o *InstallOptions) { o.CacheDir = inside }},
		{name: "hub dir", field: "hub dir", apply: func(o *InstallOptions) { o.HubDir = inside }},
		{name: "local dir", field: "local dir", apply: func(o *InstallOptions) { o.LocalDir = inside }},
	}
	for _, tc := range refused {
		t.Run("the "+tc.name+" inside the home of the user is refused", func(t *testing.T) {
			opts := systemdTestOptions(t, ScopeSystem)
			tc.apply(&opts)

			m := &systemdManager{scope: ScopeSystem}
			err := m.checkSystemPaths(opts)
			if err == nil {
				t.Fatalf("checkSystemPaths() = nil, want the refusal of the %s", tc.name)
			}
			want := fmt.Sprintf("service: the %s %q of a machine-wide instance must not live inside the home directory %q of the invoking user", tc.field, inside, home)
			if err.Error() != want {
				t.Errorf("checkSystemPaths() = %q, want %q", err, want)
			}
		})
	}

	t.Run("the home itself is refused", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)
		opts.Paths.StateDir = home

		m := &systemdManager{scope: ScopeSystem}
		err := m.checkSystemPaths(opts)
		if err == nil {
			t.Fatal("checkSystemPaths() = nil, want the refusal of the home directory of the user")
		}
		want := fmt.Sprintf("service: the state dir %q of a machine-wide instance must not live inside the home directory %q of the invoking user", home, home)
		if err.Error() != want {
			t.Errorf("checkSystemPaths() = %q, want %q", err, want)
		}
	})

	t.Run("a sibling with a common prefix is accepted", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)
		opts.Paths.StateDir = sibling

		m := &systemdManager{scope: ScopeSystem}
		if err := m.checkSystemPaths(opts); err != nil {
			t.Errorf("checkSystemPaths() = %v, want nil for the sibling %q of the home %q", err, sibling, home)
		}
	})

	t.Run("the machine-level paths are accepted", func(t *testing.T) {
		m := &systemdManager{scope: ScopeSystem}
		if err := m.checkSystemPaths(systemdTestOptions(t, ScopeSystem)); err != nil {
			t.Errorf("checkSystemPaths() = %v, want nil for the machine-level paths", err)
		}
	})

	t.Run("the user scope is not affected", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeUser)
		opts.Paths.StateDir = inside
		opts.CacheDir = inside

		m := &systemdManager{scope: ScopeUser}
		if err := m.checkSystemPaths(opts); err != nil {
			t.Errorf("checkSystemPaths() = %v, want nil for a user scope instance", err)
		}
	})
}

// TestSystemdSystemInstallRefusesTheHomeOfTheUser checks that the guard of
// the machine-wide scope runs before the unit file is written: a refused
// installation leaves no unit file and no directory of the instance behind.
func TestSystemdSystemInstallRefusesTheHomeOfTheUser(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	m := &systemdManager{scope: ScopeSystem}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	// The unit file of the machine scope lives in a fixed place of the
	// machine. The test never writes it: the refusal happens before the
	// unit file is written. The state of the file is compared before and
	// after the installation, so a unit that is installed already is never
	// touched by the test.
	before, existedBefore, err := readUnitFile(path)
	if err != nil {
		t.Logf("the unit file %q cannot be read: %v", path, err)
	}

	root := filepath.Join(home, ".config", "HFDesk", "service")
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			// The config file of the instance stays on the machine, the
			// state root of the instance is the one of the user: that is
			// the case the guard refuses.
			ConfigFile: "/etc/hfdesk/hfdesk.json",
			StateDir:   root,
			LogDir:     filepath.Join(root, "logs"),
		},
		CacheDir: "/var/lib/hfdesk/cache",
		HubDir:   "/var/lib/hfdesk/cache/hub",
		LocalDir: "/var/lib/hfdesk/models",
	}

	err = m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the home directory of the user")
	}
	want := fmt.Sprintf("service: the state dir %q of a machine-wide instance must not live inside the home directory %q of the invoking user", root, home)
	if err.Error() != want {
		t.Errorf("Install() = %q, want %q", err, want)
	}

	// The refused installation writes no unit file.
	after, existedAfter, err := readUnitFile(path)
	if err != nil {
		t.Logf("the unit file %q cannot be read: %v", path, err)
	}
	if existedBefore != existedAfter || string(before) != string(after) {
		t.Errorf("the refused installation changed the unit file %q", path)
	}
	// The refused installation creates no directory of the instance
	// either.
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused installation created the state directory %q: %v", root, err)
	}
}

// TestSystemdStatusReportsTheCacheRoot checks that the status of an instance
// reports the cache root the instance uses: the cache dir of the config file
// of the instance when that file defines one, and the registered cache dir
// otherwise.
func TestSystemdStatusReportsTheCacheRoot(t *testing.T) {
	root := t.TempDir()
	configFile := filepath.Join(root, "hfdesk.json")
	registered := filepath.Join(root, "cache")

	t.Run("the registered cache dir", func(t *testing.T) {
		if err := os.WriteFile(configFile, []byte(`{"local-dir":"/srv/hfdesk/models"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		status := systemdStatus(t, "--config", configFile, "--cache-dir", registered)
		if status.CacheDir != registered {
			t.Errorf("the cache root = %q, want the registered %q", status.CacheDir, registered)
		}
	})

	t.Run("the cache dir of the config file of the instance", func(t *testing.T) {
		configured := filepath.Join(root, "configured")
		if err := os.WriteFile(configFile, []byte(`{"cache-dir":"`+configured+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		status := systemdStatus(t, "--config", configFile, "--cache-dir", registered)
		if status.CacheDir != configured {
			t.Errorf("the cache root = %q, want the cache dir %q of the config file of the instance", status.CacheDir, configured)
		}
	})

	t.Run("no cache dir at all", func(t *testing.T) {
		if err := os.WriteFile(configFile, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		status := systemdStatus(t, "--config", configFile)
		if status.CacheDir != "" {
			t.Errorf("the cache root = %q, want none", status.CacheDir)
		}
	})
}

// TestSystemdStatusReportsAMissingExecutable checks that a registration that
// points at a program that is gone is reported as broken: the state of the
// instance is broken and the detail names the path of the missing program.
//
// The unit file is written into the temporary unit directory of the test, so
// the registration of the user of the machine is never touched, and the
// registration facts of the unit are parsed as they are in a real
// installation.
//
// The test does not need a running supervisor: a broken registration is
// decided from the unit file alone, and the other two cases only assert that
// the registration is not broken, whatever the supervisor answers.
func TestSystemdStatusReportsAMissingExecutable(t *testing.T) {
	t.Run("the registered executable is gone", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "bin", "hfdesk")
		status := systemdStatusOf(t, missing)
		if status.State != StateBroken {
			t.Errorf("the state = %q, want %q", status.State, StateBroken)
		}
		if !strings.Contains(status.Detail, missing) {
			t.Errorf("the detail = %q, want the path of the missing program %q", status.Detail, missing)
		}
		if status.Executable != missing {
			t.Errorf("the executable = %q, want %q", status.Executable, missing)
		}
	})

	t.Run("the registered executable exists", func(t *testing.T) {
		executable := systemdTestExecutable(t)
		status := systemdStatusOf(t, executable)
		if status.State == StateBroken {
			t.Errorf("the state = %q, want a state that is not broken", status.State)
		}
		if status.Executable != executable {
			t.Errorf("the executable = %q, want %q", status.Executable, executable)
		}
	})

	t.Run("the registration holds no executable", func(t *testing.T) {
		status := systemdStatusOf(t)
		if status.State == StateBroken {
			t.Errorf("the state = %q, want a state that is not broken", status.State)
		}
		if status.Executable != "" {
			t.Errorf("the executable = %q, want none", status.Executable)
		}
		// The unit file was really read: the registration that the status
		// describes is the unit of the temporary unit directory, and it
		// holds no program at all.
		content, err := os.ReadFile(status.UnitPath)
		if err != nil {
			t.Fatalf("read the unit file %q: %v", status.UnitPath, err)
		}
		if !strings.Contains(string(content), "ExecStart=") {
			t.Errorf("the unit file holds no ExecStart directive:\n%s", content)
		}
	})
}

// systemdStatus registers a unit with the given arguments in a temporary
// configuration directory and returns the status of the instance.
//
// The unit points at a program that exists, so the status describes a
// registration and not a broken one.
func systemdStatus(t *testing.T, args ...string) Status {
	t.Helper()
	return systemdStatusOf(t, append([]string{systemdTestExecutable(t)}, args...)...)
}

// systemdStatusOf registers a unit that starts the given words in a
// temporary configuration directory and returns the status of the instance.
//
// The words are the argument vector of the registered program, so an empty
// vector registers a unit without a program.
//
// The unit file is written into the temporary unit directory of the test, so
// the registration of the user of the machine is never touched. The
// supervisor of the environment may not know the unit: the registration
// facts are filled before the supervisor is asked, and they are the subject
// of the tests.
func systemdStatusOf(t *testing.T, words ...string) Status {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := &systemdManager{scope: ScopeUser}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the unit directory: %v", err)
	}
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, systemdQuoteAlways(word))
	}
	unit := "[Service]\nExecStart=" + strings.Join(quoted, " ") + "\n"
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatalf("write the unit file: %v", err)
	}

	status, err := m.Status(context.Background())
	if err != nil {
		// The supervisor of the test environment may not know the unit:
		// the registration facts are filled before the supervisor is
		// asked, and they are the subject of the test.
		t.Logf("the supervisor did not answer: %v", err)
	}
	return status
}

// systemdSecondAccount returns an account of the machine that is not the
// account of the test process.
//
// The tests of the service account need an account that is not the owner of
// the directories of the test: only then a directory the account cannot
// write into is a directory it really cannot write into.
func systemdSecondAccount(t *testing.T) *user.User {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Skipf("skipping: the account of the test process is unknown: %v", err)
	}
	for _, name := range []string{"nobody", "daemon", "bin", "sys", "games"} {
		account, err := user.Lookup(name)
		if err != nil {
			continue
		}
		if account.Uid == current.Uid {
			continue
		}
		return account
	}
	t.Skip("skipping: the machine has no second account the test could use")
	return nil
}

// systemdOwner returns the owner and the group of a path.
func systemdOwner(t *testing.T, path string) (string, string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skipf("skipping: the file system of the test does not report an owner: %v", info.Sys())
	}
	return strconv.Itoa(int(stat.Uid)), strconv.Itoa(int(stat.Gid))
}

// TestSystemdServiceAccountName checks how the account of a machine-wide
// instance is resolved: the account the user named wins, and an empty value
// falls back to the default of the platform.
func TestSystemdServiceAccountName(t *testing.T) {
	t.Run("the account of the user wins", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)
		opts.ServiceUser = "hfdesk-svc"

		if got := serviceAccountName(opts); got != "hfdesk-svc" {
			t.Errorf("serviceAccountName() = %q, want %q", got, "hfdesk-svc")
		}
	})

	t.Run("an empty account falls back to the default of the platform", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)
		opts.ServiceUser = ""

		got := serviceAccountName(opts)
		if want := DefaultServiceUser(); got != want {
			t.Errorf("serviceAccountName() = %q, want %q", got, want)
		}
	})
}

// TestSystemdInstallRefusesAMissingServiceAccount checks that a
// machine-wide installation whose account does not exist is refused with the
// guidance that names the account and the command that creates it, and that
// the refused installation leaves nothing behind.
func TestSystemdInstallRefusesAMissingServiceAccount(t *testing.T) {
	const missing = "hfdesk-missing-service-account"
	if _, err := user.Lookup(missing); err == nil {
		t.Skipf("skipping: the account %q exists on this machine", missing)
	}

	// The home of the user is a temporary directory, so the guard of the
	// machine-wide scope does not refuse the paths of the test.
	home := t.TempDir()
	t.Setenv("HOME", home)

	m := &systemdManager{scope: ScopeSystem}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	// The unit file of the machine scope lives in a fixed place of the
	// machine. The test never writes it: the refusal happens before the
	// unit file is written. The state of the file is compared before and
	// after the installation, so a unit that is installed already is never
	// touched by the test.
	before, existedBefore, err := readUnitFile(path)
	if err != nil {
		t.Logf("the unit file %q cannot be read: %v", path, err)
	}

	root := t.TempDir()
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
		ServiceUser: missing,
	}

	err = m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the missing service account")
	}
	want := fmt.Sprintf("service: the machine-wide instance needs the service account %q: create it with %q and install again (HFDesk never creates accounts itself)",
		missing, serviceAccountCreateCommand(missing))
	if err.Error() != want {
		t.Errorf("Install() = %q, want %q", err, want)
	}
	if !strings.Contains(err.Error(), "useradd --system --no-create-home") {
		t.Errorf("Install() = %q, want the command that creates the account", err)
	}

	// The refused installation writes no unit file.
	after, existedAfter, err := readUnitFile(path)
	if err != nil {
		t.Logf("the unit file %q cannot be read: %v", path, err)
	}
	if existedBefore != existedAfter || string(before) != string(after) {
		t.Errorf("the refused installation changed the unit file %q", path)
	}
	// The refused installation creates no directory of the instance
	// either.
	for _, dir := range instanceDirs(opts) {
		if _, err := os.Stat(dir.path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused installation created the %s %q: %v", dir.name, dir.path, err)
		}
	}
}

// TestSystemdRenderUnitNamesTheServiceAccount checks that the unit of a
// machine-wide instance names the service account and its group, and that
// the unit of a user scope instance names no account at all.
func TestSystemdRenderUnitNamesTheServiceAccount(t *testing.T) {
	t.Run("the machine-wide scope names the account of the user", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)
		opts.ServiceUser = "hfdesk-svc"

		unit, err := renderUnit(opts)
		if err != nil {
			t.Fatalf("renderUnit() = %v, want nil", err)
		}
		if !strings.Contains(unit, "User=hfdesk-svc\n") {
			t.Errorf("the unit does not run as the service account:\n%s", unit)
		}
		if !strings.Contains(unit, "Group=hfdesk-svc\n") {
			t.Errorf("the unit does not run in the group of the service account:\n%s", unit)
		}
	})

	t.Run("the machine-wide scope falls back to the default of the platform", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)

		unit, err := renderUnit(opts)
		if err != nil {
			t.Fatalf("renderUnit() = %v, want nil", err)
		}
		account := DefaultServiceUser()
		if !strings.Contains(unit, "User="+account+"\n") {
			t.Errorf("the unit does not run as the default service account %q:\n%s", account, unit)
		}
		if !strings.Contains(unit, "Group="+account+"\n") {
			t.Errorf("the unit does not run in the group of the default service account %q:\n%s", account, unit)
		}
	})

	t.Run("the user scope names no account", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeUser)
		opts.ServiceUser = "hfdesk-svc"

		unit, err := renderUnit(opts)
		if err != nil {
			t.Fatalf("renderUnit() = %v, want nil", err)
		}
		for _, directive := range unitDirectives(unit) {
			if strings.HasPrefix(directive, "User=") || strings.HasPrefix(directive, "Group=") {
				t.Errorf("a user scope unit names an account: %q", directive)
			}
		}
	})
}

// TestSystemdGrantInstanceDirs checks that the directories an installation
// creates belong to the service account of the instance, and that the
// installation never changes the ownership of a tree it does not own.
//
// Only root can change the ownership of a directory, so the test skips
// itself when it does not run as root.
func TestSystemdGrantInstanceDirs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("skipping: only root can change the ownership of a directory")
	}
	account := systemdSecondAccount(t)

	root := t.TempDir()
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
	}

	// The directories of the instance do not exist yet: the installation
	// creates them and grants them to the account.
	created := make(map[string][]string, 3)
	for _, dir := range instanceDirs(opts) {
		created[dir.path] = createdDirs(dir.path)
	}
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}

	// A cache tree of the instance is not owned by the installation: its
	// ownership has to stay the one it had.
	cache := filepath.Join(root, "cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatalf("create the cache directory: %v", err)
	}
	cacheOwner, cacheGroup := systemdOwner(t, cache)

	m := &systemdManager{scope: ScopeSystem}
	if err := m.grantInstanceDirs(opts, account, created); err != nil {
		t.Fatalf("grantInstanceDirs() = %v, want nil", err)
	}

	// Every directory the installation created belongs to the account: the
	// parents it created for the directories of the instance belong to it
	// as well, because the instance has to reach its own directories
	// through them.
	for _, dir := range instanceDirs(opts) {
		for _, path := range created[dir.path] {
			uid, gid := systemdOwner(t, path)
			if uid != account.Uid {
				t.Errorf("the owner of the %s %q = %q, want the service account %q", dir.name, path, uid, account.Uid)
			}
			if gid != account.Gid {
				t.Errorf("the group of the %s %q = %q, want the group %q of the service account", dir.name, path, gid, account.Gid)
			}
		}
	}
	if uid, gid := systemdOwner(t, cache); uid != cacheOwner || gid != cacheGroup {
		t.Errorf("the ownership of the cache directory %q = %q:%q, want %q:%q", cache, uid, gid, cacheOwner, cacheGroup)
	}
}

// TestSystemdGrantInstanceDirsRefusesAPreExistingDirectory checks that a
// directory that existed before the installation is not chowned.
//
// The directory belongs to whoever owns it, so the installation leaves it
// alone. The service account still has to be able to write there, so an
// installation that cannot do that is refused with the guidance that names
// the directory and the account.
func TestSystemdGrantInstanceDirsRefusesAPreExistingDirectory(t *testing.T) {
	account := systemdSecondAccount(t)

	root := t.TempDir()
	// The state directory exists before the installation and belongs to
	// the user of the test: the service account cannot write into it.
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	stateOwner, stateGroup := systemdOwner(t, state)

	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   state,
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
	}

	// The state directory existed before the installation, the other two
	// directories are created by it.
	created := make(map[string][]string, 3)
	for _, dir := range instanceDirs(opts) {
		created[dir.path] = createdDirs(dir.path)
	}
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	config := filepath.Dir(opts.Paths.ConfigFile)
	configOwner, configGroup := systemdOwner(t, config)

	m := &systemdManager{scope: ScopeSystem}
	err := m.grantInstanceDirs(opts, account, created)
	if err == nil {
		t.Fatal("grantInstanceDirs() = nil, want the refusal of a pre-existing directory")
	}
	want := fmt.Sprintf("service: the state dir %q of the machine-wide instance is not writable by the service account %q: grant it to the account (for example %q) and install again",
		state, account.Username, "chown "+account.Username+" "+state)
	if err.Error() != want {
		t.Errorf("grantInstanceDirs() = %q, want %q", err, want)
	}

	// The directory that existed before the installation still belongs to
	// its owner: it was not chowned.
	if uid, gid := systemdOwner(t, state); uid != stateOwner || gid != stateGroup {
		t.Errorf("the ownership of the state directory %q = %q:%q, want %q:%q", state, uid, gid, stateOwner, stateGroup)
	}
	// The refusal happened before the first ownership was changed, so the
	// directory the installation would have granted is untouched as well.
	if uid, gid := systemdOwner(t, config); uid != configOwner || gid != configGroup {
		t.Errorf("the ownership of the config directory %q = %q:%q, want %q:%q", config, uid, gid, configOwner, configGroup)
	}
}

// TestSystemdGrantInstanceDirsKeepsAWritablePreExistingDirectory checks that
// a directory that existed before the installation and that the service
// account can write into is accepted and left alone.
//
// The directory belongs to whoever owns it, so the installation grants it to
// nobody: it only makes sure that the account can work there.
func TestSystemdGrantInstanceDirsKeepsAWritablePreExistingDirectory(t *testing.T) {
	account := systemdSecondAccount(t)

	root := t.TempDir()
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "state"),
			LogDir:     filepath.Join(root, "logs"),
		},
	}
	// Every directory of the instance exists before the installation and
	// is writable by the account.
	owners := map[string][2]string{}
	for _, dir := range instanceDirs(opts) {
		if err := os.MkdirAll(dir.path, 0o700); err != nil {
			t.Fatalf("create the %s %q: %v", dir.name, dir.path, err)
		}
		// The mode of a create call is filtered by the umask of the
		// process, so the mode is set explicitly.
		if err := os.Chmod(dir.path, 0o777); err != nil {
			t.Fatalf("make the %s %q writable: %v", dir.name, dir.path, err)
		}
		uid, gid := systemdOwner(t, dir.path)
		owners[dir.path] = [2]string{uid, gid}
	}

	created := make(map[string][]string, 3)
	for _, dir := range instanceDirs(opts) {
		created[dir.path] = createdDirs(dir.path)
	}

	m := &systemdManager{scope: ScopeSystem}
	if err := m.grantInstanceDirs(opts, account, created); err != nil {
		t.Fatalf("grantInstanceDirs() = %v, want nil for a pre-existing directory the account can write into", err)
	}

	// The directories still belong to their owner: the installation grants
	// only the directories it created itself.
	for _, dir := range instanceDirs(opts) {
		uid, gid := systemdOwner(t, dir.path)
		want := owners[dir.path]
		if uid != want[0] || gid != want[1] {
			t.Errorf("the ownership of the %s %q = %q:%q, want %q:%q", dir.name, dir.path, uid, gid, want[0], want[1])
		}
	}
}

// TestSystemdInstallRefusesAPreExistingDirectory checks that a machine-wide
// installation whose state directory existed before the installation is
// refused with the guidance that names the directory and the account, and
// that the refused installation never changes the ownership of that
// directory.
//
// The installation needs an elevated shell, so the test skips itself when it
// does not run as root.
func TestSystemdInstallRefusesAPreExistingDirectory(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("skipping: a machine-wide installation needs root")
	}
	account := systemdSecondAccount(t)

	// The home of the user is a temporary directory, so the guard of the
	// machine-wide scope does not refuse the paths of the test.
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := t.TempDir()
	// The state directory exists before the installation and belongs to
	// the user of the test: the service account cannot write into it.
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	stateOwner, stateGroup := systemdOwner(t, state)

	m := &systemdManager{scope: ScopeSystem}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	// The unit file of the machine scope lives in a fixed place of the
	// machine. The test never writes it: the refusal happens before the
	// unit file is written. The state of the file is compared before and
	// after the installation, so a unit that is installed already is never
	// touched by the test.
	before, existedBefore, err := readUnitFile(path)
	if err != nil {
		t.Logf("the unit file %q cannot be read: %v", path, err)
	}

	opts := InstallOptions{
		Scope:       ScopeSystem,
		Executable:  systemdTestExecutable(t),
		Listen:      DefaultListen,
		ServiceUser: account.Username,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   state,
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
	}

	err = m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the pre-existing state directory")
	}
	want := fmt.Sprintf("service: the state dir %q of the machine-wide instance is not writable by the service account %q: grant it to the account (for example %q) and install again",
		state, account.Username, "chown "+account.Username+" "+state)
	if err.Error() != want {
		t.Errorf("Install() = %q, want %q", err, want)
	}

	// The refused installation never changes the ownership of the
	// directory that existed before it.
	if uid, gid := systemdOwner(t, state); uid != stateOwner || gid != stateGroup {
		t.Errorf("the ownership of the state directory %q = %q:%q, want %q:%q", state, uid, gid, stateOwner, stateGroup)
	}
	// The refused installation writes no unit file.
	after, existedAfter, err := readUnitFile(path)
	if err != nil {
		t.Logf("the unit file %q cannot be read: %v", path, err)
	}
	if existedBefore != existedAfter || string(before) != string(after) {
		t.Errorf("the refused installation changed the unit file %q", path)
	}
}

// TestSystemdGrantInstanceDirsRefusesADirectoryOfTheMachine checks that the
// installation never hands a directory of the machine to the account of the
// instance.
//
// The directory of the config file is derived from the path of a file, so a
// config file in /etc would hand /etc itself to the account. The refusal
// happens before the first directory is granted, so no ownership changes at
// all.
func TestSystemdGrantInstanceDirsRefusesADirectoryOfTheMachine(t *testing.T) {
	account := systemdSecondAccount(t)

	root := t.TempDir()
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			// The config file lies directly in a top level directory
			// of the machine: its directory is /etc.
			ConfigFile: "/etc/hfdesk.json",
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
	}
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	stateOwner, stateGroup := systemdOwner(t, opts.Paths.StateDir)

	m := &systemdManager{scope: ScopeSystem}
	err := m.grantInstanceDirs(opts, account, nil)
	if err == nil {
		t.Fatal("grantInstanceDirs() = nil, want the refusal of a directory of the machine")
	}
	want := `service: the config dir "/etc" is a directory of the machine: the instance needs a directory of its own, name one below a directory of the machine`
	if err.Error() != want {
		t.Errorf("grantInstanceDirs() = %q, want %q", err, want)
	}

	// The refusal granted nothing: the state directory still belongs to
	// the account it belonged to.
	if uid, gid := systemdOwner(t, opts.Paths.StateDir); uid != stateOwner || gid != stateGroup {
		t.Errorf("the ownership of the state directory %q = %q:%q, want %q:%q", opts.Paths.StateDir, uid, gid, stateOwner, stateGroup)
	}
}

// TestSystemdCheckServiceAccountAccess checks the rules that decide whether
// the service account can write into a directory the user named: the
// ownership of the account, the group of the account, the world write
// permission and the nearest existing ancestor of a directory that is not
// there yet.
//
// The account of the test is the account of the test process, so the
// directories of the test belong to it.
func TestSystemdCheckServiceAccountAccess(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatalf("Current() = %v, want nil", err)
	}
	other := systemdSecondAccount(t)

	root := t.TempDir()
	owned := filepath.Join(root, "owned")
	if err := os.Mkdir(owned, 0o700); err != nil {
		t.Fatal(err)
	}
	group := filepath.Join(root, "group")
	if err := os.Mkdir(group, 0o770); err != nil {
		t.Fatal(err)
	}
	world := filepath.Join(root, "world")
	if err := os.Mkdir(world, 0o777); err != nil {
		t.Fatal(err)
	}
	closed := filepath.Join(root, "closed")
	if err := os.Mkdir(closed, 0o500); err != nil {
		t.Fatal(err)
	}

	// A directory the account cannot write into: it belongs to the
	// account of the test, and the account of the check is another one.
	foreign := filepath.Join(root, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}

	m := &systemdManager{scope: ScopeSystem}
	cases := []struct {
		name    string
		account *user.User
		path    string
		wantErr bool
	}{
		{name: "a directory the account owns", account: account, path: owned},
		{name: "a directory of the group of the account", account: account, path: group},
		{name: "a world writable directory", account: account, path: world},
		{name: "a directory the account cannot write into", account: other, path: foreign, wantErr: true},
		{name: "a directory without a write permission", account: other, path: closed, wantErr: true},
		{name: "a missing directory below a writable ancestor", account: account, path: filepath.Join(owned, "cache", "hub")},
		{name: "a missing directory below a closed ancestor", account: other, path: filepath.Join(closed, "cache"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDirWritableByAccount(tc.path, tc.account)
			if tc.wantErr && err == nil {
				t.Errorf("checkDirWritableByAccount(%q) = nil, want a refusal", tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("checkDirWritableByAccount(%q) = %v, want nil", tc.path, err)
			}
		})
	}

	t.Run("the scope default of the cache is not checked", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)
		opts.Paths.StateDir = filepath.Join(root, "state")
		opts.CacheDir = filepath.Join(opts.Paths.StateDir, "cache")
		opts.HubDir = filepath.Join(opts.CacheDir, "hub")
		// The machine-wide scope has no local dir of its own, so no
		// directory is named and nothing is checked.
		opts.LocalDir = ""
		// The account cannot write into the cache of the scope, and the
		// installation does not check it: the cache lives inside the
		// state directory of the instance.
		if err := m.checkServiceAccountAccess(opts, other); err != nil {
			t.Errorf("checkServiceAccountAccess() = %v, want nil for the cache of the scope", err)
		}
	})

	t.Run("a named cache of the scope is checked", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)
		opts.Paths.StateDir = filepath.Join(root, "state")
		opts.CacheDir = foreign
		opts.HubDir = filepath.Join(foreign, "hub")
		if err := m.checkServiceAccountAccess(opts, other); err == nil {
			t.Error("checkServiceAccountAccess() = nil, want the refusal of the named cache")
		}
	})

	t.Run("the user scope is not checked", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeUser)
		opts.CacheDir = foreign

		user := &systemdManager{scope: ScopeUser}
		if err := user.checkServiceAccountAccess(opts, other); err != nil {
			t.Errorf("checkServiceAccountAccess() = %v, want nil for a user scope instance", err)
		}
	})
}

// TestSystemdInstallRefusesACacheDirTheAccountCannotWrite checks that a
// machine-wide installation whose cache directory the service account cannot
// write into is refused with the guidance that names the account and the
// directory, and that the refused installation never changes the ownership
// of that directory.
func TestSystemdInstallRefusesACacheDirTheAccountCannotWrite(t *testing.T) {
	account := systemdSecondAccount(t)

	// The home of the user is a temporary directory, so the guard of the
	// machine-wide scope does not refuse the paths of the test.
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatalf("create the cache directory: %v", err)
	}
	owner, group := systemdOwner(t, cache)

	m := &systemdManager{scope: ScopeSystem}
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
		CacheDir:    cache,
		ServiceUser: account.Username,
	}

	err := m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the cache directory")
	}
	want := fmt.Sprintf("service: the service account %q cannot write to the cache directory %q: grant it access (for example %q or by adding it to the group that owns the directory) and install again",
		account.Username, cache, "chown -R "+account.Username+" "+cache)
	if err.Error() != want {
		t.Errorf("Install() = %q, want %q", err, want)
	}

	// The refused installation never changes the ownership of the cache
	// tree.
	if uid, gid := systemdOwner(t, cache); uid != owner || gid != group {
		t.Errorf("the ownership of the cache directory %q = %q:%q, want %q:%q", cache, uid, gid, owner, group)
	}
	// The refused installation creates no directory of the instance.
	for _, dir := range instanceDirs(opts) {
		if _, err := os.Stat(dir.path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused installation created the %s %q: %v", dir.name, dir.path, err)
		}
	}
}

// TestSystemdUserScopeNeedsNoServiceAccount checks that a user scope
// instance is not affected by the service account of the machine-wide scope:
// it needs no account, no account is written into its unit and the access of
// a directory it does not own is not checked.
func TestSystemdUserScopeNeedsNoServiceAccount(t *testing.T) {
	const missing = "hfdesk-missing-service-account"

	opts := systemdTestOptions(t, ScopeUser)
	opts.ServiceUser = missing
	// A cache directory the account could not write into: the user scope
	// instance writes it as the user of the session.
	opts.CacheDir = filepath.Join(t.TempDir(), "cache")

	m := &systemdManager{scope: ScopeUser}

	account, err := m.serviceAccount(opts)
	if err != nil {
		t.Fatalf("serviceAccount() = %v, want nil for a user scope instance", err)
	}
	if account != nil {
		t.Errorf("a user scope instance resolved the account %q, want none", account.Username)
	}
	if err := m.checkServiceAccountAccess(opts, account); err != nil {
		t.Errorf("checkServiceAccountAccess() = %v, want nil for a user scope instance", err)
	}
	if err := m.grantInstanceDirs(opts, account, nil); err != nil {
		t.Errorf("grantInstanceDirs() = %v, want nil for a user scope instance", err)
	}

	// The unit of a user scope instance names no account, even when the
	// command line named one.
	unit, err := renderUnit(opts)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}
	for _, directive := range unitDirectives(unit) {
		if strings.HasPrefix(directive, "User=") || strings.HasPrefix(directive, "Group=") {
			t.Errorf("a user scope unit names an account: %q", directive)
		}
	}
}
