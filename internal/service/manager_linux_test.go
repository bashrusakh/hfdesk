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
	"path/filepath"
	"strings"
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
	// A machine wide instance must not inherit the identity of the user
	// who runs the install command.
	for _, directive := range unitDirectives(unit) {
		if strings.HasPrefix(directive, "User=") {
			t.Errorf("the unit names a user: %q", directive)
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

// systemdStatus registers a unit with the given arguments in a temporary
// configuration directory and returns the status of the instance.
//
// The unit points at a program that exists, so the status describes a
// registration and not a broken one.
func systemdStatus(t *testing.T, args ...string) Status {
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
	words := append([]string{systemdTestExecutable(t)}, args...)
	unit := "[Service]\nExecStart=" + strings.Join(words, " ") + "\n"
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
