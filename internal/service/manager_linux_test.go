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
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/server"
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

// unitDirective returns the value of one directive of a unit file, e.g. the
// account of a "User=" directive.
//
// An empty string is returned when the unit holds no such directive, so a
// caller can tell a directive that is absent from one that carries an empty
// value.
func unitDirective(unit, key string) string {
	for _, directive := range unitDirectives(unit) {
		name, value, ok := strings.Cut(directive, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(name) == key {
			return strings.TrimSpace(value)
		}
	}
	return ""
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

	unit, err := renderUnit(opts, nil)
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
	// The account of a machine-wide instance is an account of the machine:
	// the unit names it and the primary group of it.
	account := systemdSecondAccount(t)

	opts := systemdTestOptions(t, ScopeSystem)
	opts.ServiceUser = account.Username

	unit, err := renderUnit(opts, account)
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
	if want := "User=" + systemdQuote(account.Username) + "\n"; !strings.Contains(unit, want) {
		t.Errorf("the unit does not run as the service account %q:\n%s", account.Username, unit)
	}
	// The group of the unit is the primary group of the account, never the
	// name of the account.
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		t.Fatalf("LookupGroupId(%q) = %v, want nil", account.Gid, err)
	}
	if want := "Group=" + systemdQuote(group.Name) + "\n"; !strings.Contains(unit, want) {
		t.Errorf("the unit does not run in the primary group %q of the service account %q:\n%s", group.Name, account.Username, unit)
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

	unit, err := renderUnit(opts, nil)
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

	unit, err := renderUnit(opts, nil)
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

// TestSystemdRenderUnitEscapesTheDollarSign checks that a path with a dollar
// sign reaches the process as it was written.
//
// The supervisor expands the environment of the unit in the command line of
// the unit, so a dollar sign in a path would be rewritten before the process
// sees it. A doubled dollar sign is the documented escape of a literal dollar
// sign, so the unit carries the escaped form and the supervisor hands the
// original path to the process.
func TestSystemdRenderUnitEscapesTheDollarSign(t *testing.T) {
	opts := systemdTestOptions(t, ScopeUser)
	opts.Executable = "/opt/hf$desk/bin/hfdesk"
	opts.Paths.ConfigFile = "/etc/hf$desk/hfdesk.json"
	opts.Paths.StateDir = "/var/lib/hf$desk"
	opts.Paths.LogDir = "/var/log/hf$desk"
	opts.CacheDir = "/var/cache/hf$desk"
	opts.HubDir = "/var/cache/hf$desk/hub"
	opts.LocalDir = "/var/lib/models/hf$desk"

	unit, err := renderUnit(opts, nil)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}

	command := unitDirective(unit, "ExecStart")
	if command == "" {
		t.Fatalf("the unit has no ExecStart directive:\n%s", unit)
	}
	if !strings.Contains(command, "hf$$desk") {
		t.Errorf("the command of the unit does not escape the dollar sign of the paths: %q", command)
	}
	if strings.Contains(command, "hf$desk") {
		t.Errorf("the command of the unit holds an unescaped dollar sign: %q", command)
	}

	// The supervisor reconstructs the paths the process sees.
	parsed := parseUnit(unit)
	if parsed.ExecStart != opts.Executable {
		t.Errorf("ExecStart = %q, want %q", parsed.ExecStart, opts.Executable)
	}
	wantArgs := RunArgs(opts)
	if len(parsed.ExecArgs) != len(wantArgs) {
		t.Fatalf("the arguments of the unit = %v, want %v", parsed.ExecArgs, wantArgs)
	}
	for i := range wantArgs {
		if parsed.ExecArgs[i] != wantArgs[i] {
			t.Errorf("the argument %d of the unit = %q, want %q", i, parsed.ExecArgs[i], wantArgs[i])
		}
	}

	// The working directory is a setting of the unit and not a part of its
	// command line: the supervisor does not expand the environment of the
	// unit there, so the path stays as it was written.
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

	unit, err := renderUnit(opts, nil)
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

	unit, err := renderUnit(opts, nil)
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

// TestSystemdAnalyzeVerifySystemScope checks the machine-wide unit with the
// verifier of the supervisor itself: the machine-wide unit names an account,
// and the verifier resolves that name against the accounts of the machine.
//
// The unit of the test names the account of the test process, which is what
// a machine-wide installation names when the operator names their own
// account. The template names the primary group of that account, so the
// machine has to know that group as well.
//
// The executable of the unit is the test binary, so the verifier finds an
// executable that really exists, and the working directory of the unit is the
// temporary directory of the test, so the verifier finds a directory that
// really exists. Every other path of the unit stays the machine level path
// of the template.
//
// The test needs no privilege and installs nothing: it writes its unit into
// a temporary directory and hands it to the verifier.
func TestSystemdAnalyzeVerifySystemScope(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skipf("skipping: systemd-analyze is not available in this environment: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() = %v, want nil", err)
	}
	account, err := user.Current()
	if err != nil {
		t.Skipf("skipping: the account of the test process is unknown: %v", err)
	}
	// The unit names the account by its name, so the machine has to
	// resolve that name the way the supervisor resolves it.
	if _, err := user.Lookup(account.Username); err != nil {
		t.Skipf("skipping: the machine knows no account %q: %v", account.Username, err)
	}
	// The template names the primary group of the account, so the machine
	// has to resolve that group the way the supervisor resolves it.
	if _, err := user.LookupGroupId(account.Gid); err != nil {
		t.Skipf("skipping: the machine knows no primary group for the account %q: %v", account.Username, err)
	}

	root := t.TempDir()
	opts := systemdTestOptions(t, ScopeSystem)
	opts.Executable = executable
	opts.ServiceUser = account.Username
	// The working directory of the unit has to exist, so the state
	// directory of the test is the temporary directory. Every other path
	// stays the machine level path of the template.
	opts.Paths.StateDir = root

	unit, err := renderUnit(opts, account)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}
	// The unit that is verified is the machine-wide one: it names the
	// account and it is enabled for the boot of the machine.
	if want := "User=" + systemdQuote(account.Username) + "\n"; !strings.Contains(unit, want) {
		t.Fatalf("the unit does not run as the account %q:\n%s", account.Username, unit)
	}
	if !strings.Contains(unit, "WantedBy=multi-user.target") {
		t.Fatalf("the unit is not enabled for the boot of the machine:\n%s", unit)
	}
	// The machine-wide unit keeps the machine level home of the instance:
	// it never adopts the home of the user who runs the test.
	if want := "Environment=HOME=" + systemServiceHome; !strings.Contains(unit, want) {
		t.Fatalf("the unit does not carry the machine level home %q:\n%s", systemServiceHome, unit)
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
	t.Logf("running %s verify %s", analyze, path)
	if err := cmd.Run(); err != nil {
		t.Fatalf("systemd-analyze verify %s = %v\n%s\nthe unit:\n%s", path, err, stderr.String(), unit)
	}
	t.Logf("%s verify %s accepted the machine-wide unit", analyze, path)
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

	if _, err := m.Install(ctx, opts); err != nil {
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

	_, err = m.Install(context.Background(), opts)
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
	current := systemdCurrentAccount(t)
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

// systemdCurrentAccount returns the account of the test process.
func systemdCurrentAccount(t *testing.T) *user.User {
	t.Helper()
	account, err := user.Current()
	if err != nil {
		t.Skipf("skipping: the account of the test process is unknown: %v", err)
	}
	return account
}

// systemdAccountsOfTheMachine returns the accounts of the machine.
//
// The accounts are read from the password database of the machine, so the
// test works on every machine and needs no privilege.
func systemdAccountsOfTheMachine(t *testing.T) []*user.User {
	t.Helper()
	content, err := os.ReadFile("/etc/passwd")
	if err != nil {
		t.Skipf("skipping: the password database of the machine cannot be read: %v", err)
	}
	var accounts []*user.User
	for _, line := range strings.Split(string(content), "\n") {
		name, _, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || name == "" {
			continue
		}
		account, err := user.Lookup(name)
		if err != nil {
			continue
		}
		accounts = append(accounts, account)
	}
	if len(accounts) == 0 {
		t.Skip("skipping: the machine knows no account the test could use")
	}
	return accounts
}

// systemdAccountWithAnotherPrimaryGroup returns an account of the machine
// whose primary group carries another name than the account itself.
//
// Such an account is the case that shows the difference between the primary
// group of an account and the name of the account. The second result is
// false when the machine knows no such account.
func systemdAccountWithAnotherPrimaryGroup(t *testing.T) (*user.User, bool) {
	t.Helper()
	for _, account := range systemdAccountsOfTheMachine(t) {
		group, err := user.LookupGroupId(account.Gid)
		if err != nil || group.Name == "" {
			continue
		}
		if group.Name != account.Username {
			return account, true
		}
	}
	return nil, false
}

// systemdUnresolvableAccount returns an account whose primary group the
// machine does not know.
//
// The group of a unit is never guessed: an account whose group cannot be
// resolved leaves the group out of the unit, so the supervisor uses the
// default group of the account.
func systemdUnresolvableAccount(t *testing.T) *user.User {
	t.Helper()
	for _, gid := range []string{"4294967294", "999999999", "999999998"} {
		if _, err := user.LookupGroupId(gid); err == nil {
			continue
		}
		return &user.User{
			Username: "hfdesk-svc",
			Uid:      "999999999",
			Gid:      gid,
			Name:     "HFDesk service account",
			HomeDir:  systemServiceHome,
		}
	}
	t.Skip("skipping: the machine knows a group for every id the test could use")
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

// systemdTraversableTempDir returns a temporary directory the account of the
// test can pass through.
//
// The temporary directory of a test is owner-only, so an account that is not
// the owner of the test never reaches a directory below it. The access check
// of the service layer asks for the same: a directory below a parent
// without the execute permission is out of reach of the account.
func systemdTraversableTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// The mode of a create call is filtered by the umask of the process,
	// so the mode is set explicitly.
	for _, path := range []string{root, filepath.Dir(root)} {
		if err := os.Chmod(path, 0o711); err != nil {
			t.Fatalf("make %q traversable: %v", path, err)
		}
	}
	return root
}

// systemdSupplementaryGroup returns the id of a supplementary group of the
// account of the test process.
//
// The groups of an account are read from the group database of the machine,
// so the test needs a group whose member list holds the account of the
// test. The second result is false when the machine knows no such group.
func systemdSupplementaryGroup(t *testing.T) (string, bool) {
	t.Helper()
	current := systemdCurrentAccount(t)
	content, err := os.ReadFile("/etc/group")
	if err != nil {
		t.Skipf("skipping: the group database of the machine cannot be read: %v", err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) < 4 {
			continue
		}
		if fields[2] == current.Gid {
			// The primary group of the account is no supplementary
			// group of it.
			continue
		}
		for _, member := range strings.Split(fields[3], ",") {
			if member == current.Username {
				return fields[2], true
			}
		}
	}
	return "", false
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

	_, err = m.Install(context.Background(), opts)
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
// machine-wide instance names the service account, and that the group of the
// unit is the primary group of that account and never the name of the
// account. The unit of a user scope instance names no account at all.
func TestSystemdRenderUnitNamesTheServiceAccount(t *testing.T) {
	t.Run("the machine-wide scope names the account of the user", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)
		opts.ServiceUser = "hfdesk-svc"

		// The account is not on the machine, so no group of it can be
		// resolved: the unit names the account and no group at all.
		unit, err := renderUnit(opts, nil)
		if err != nil {
			t.Fatalf("renderUnit() = %v, want nil", err)
		}
		if got := unitDirective(unit, "User"); got != "hfdesk-svc" {
			t.Errorf("the account of the unit = %q, want %q", got, "hfdesk-svc")
		}
		if got := unitDirective(unit, "Group"); got != "" {
			t.Errorf("the unit names the group %q of an account the machine does not know", got)
		}
	})

	t.Run("the machine-wide scope falls back to the default of the platform", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)

		// The default account of the platform may or may not be on this
		// machine: the unit names it either way, and the group of the
		// unit is the primary group of the account when the machine
		// knows the account.
		account, err := user.Lookup(DefaultServiceUser())
		if err != nil {
			account = nil
		}

		unit, err := renderUnit(opts, account)
		if err != nil {
			t.Fatalf("renderUnit() = %v, want nil", err)
		}
		name := DefaultServiceUser()
		if got := unitDirective(unit, "User"); got != name {
			t.Errorf("the account of the unit = %q, want the default service account %q", got, name)
		}
		wantGroup := ""
		if account != nil {
			group, err := user.LookupGroupId(account.Gid)
			if err != nil {
				t.Fatalf("LookupGroupId(%q) = %v, want nil", account.Gid, err)
			}
			wantGroup = group.Name
		}
		if got := unitDirective(unit, "Group"); got != wantGroup {
			t.Errorf("the group of the unit = %q, want %q", got, wantGroup)
		}
	})

	t.Run("the user scope names no account", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeUser)
		opts.ServiceUser = "hfdesk-svc"

		unit, err := renderUnit(opts, nil)
		if err != nil {
			t.Fatalf("renderUnit() = %v, want nil", err)
		}
		for _, directive := range unitDirectives(unit) {
			if strings.HasPrefix(directive, "User=") || strings.HasPrefix(directive, "Group=") {
				t.Errorf("a user scope unit names an account: %q", directive)
			}
		}
		if got := unitDirective(unit, "User"); got != "" {
			t.Errorf("the account of a user scope unit = %q, want none", got)
		}
		if got := unitDirective(unit, "Group"); got != "" {
			t.Errorf("the group of a user scope unit = %q, want none", got)
		}
	})
}

// TestSystemdRenderUnitNamesThePrimaryGroupOfTheAccount checks that the
// group of a machine-wide unit is the primary group of its service account
// and never the name of the account.
//
// The account of the test is an account whose primary group carries another
// name than the account, whenever the machine knows one: that is the case
// that shows the difference. A machine whose accounts all carry the name of
// their primary group is checked against the resolved primary group instead.
func TestSystemdRenderUnitNamesThePrimaryGroupOfTheAccount(t *testing.T) {
	account, other := systemdAccountWithAnotherPrimaryGroup(t)
	if !other {
		account = systemdCurrentAccount(t)
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		t.Fatalf("LookupGroupId(%q) = %v, want nil", account.Gid, err)
	}

	opts := systemdTestOptions(t, ScopeSystem)
	opts.ServiceUser = account.Username

	unit, err := renderUnit(opts, account)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}
	t.Logf("the machine-wide unit of the account %q:\n%s", account.Username, unit)

	if got := unitDirective(unit, "User"); got != account.Username {
		t.Errorf("the account of the unit = %q, want %q", got, account.Username)
	}
	if got := unitDirective(unit, "Group"); got != group.Name {
		t.Errorf("the group of the unit = %q, want the primary group %q of the account %q", got, group.Name, account.Username)
	}
	// The name of the account is never the group of the unit: an account
	// whose primary group carries another name would name a group that
	// does not exist.
	if group.Name != account.Username {
		if got := unitDirective(unit, "Group"); got == account.Username {
			t.Errorf("the group of the unit = %q, want the primary group %q of the account %q", got, group.Name, account.Username)
		}
	}
}

// TestSystemdRenderUnitOmitsAnUnresolvableGroup checks that a machine-wide
// unit names no group at all when the primary group of its service account
// cannot be resolved: the supervisor then uses the default group of the
// account, and no group name is ever guessed.
func TestSystemdRenderUnitOmitsAnUnresolvableGroup(t *testing.T) {
	account := systemdUnresolvableAccount(t)

	opts := systemdTestOptions(t, ScopeSystem)
	opts.ServiceUser = account.Username

	unit, err := renderUnit(opts, account)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}
	t.Logf("the machine-wide unit of the account %q:\n%s", account.Username, unit)

	if got := unitDirective(unit, "User"); got != account.Username {
		t.Errorf("the account of the unit = %q, want %q", got, account.Username)
	}
	if got := unitDirective(unit, "Group"); got != "" {
		t.Errorf("the unit names the group %q of an account whose group cannot be resolved", got)
	}
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

	root := systemdTraversableTempDir(t)
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

	_, err = m.Install(context.Background(), opts)
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

// TestSystemdCheckServiceAccountAccessSkipsAMissingDirectoryInsideTheStateRoot
// checks that a directory inside the state directory of the instance that does
// not exist yet is not checked against the service account.
//
// The installation creates the state directory of the instance and grants it
// to the account, so the service creates the directory itself below the state
// directory, as the account. The check of the named directories must not
// refuse such a directory. A directory outside of the state directory is
// still checked, because the installation never grants it to the account.
func TestSystemdCheckServiceAccountAccessSkipsAMissingDirectoryInsideTheStateRoot(t *testing.T) {
	other := systemdSecondAccount(t)
	root := systemdTraversableTempDir(t)

	// The state directory belongs to the account of the test, so the
	// account of the check cannot write into it.
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatalf("close the state directory: %v", err)
	}

	m := &systemdManager{scope: ScopeSystem}
	opts := systemdTestOptions(t, ScopeSystem)
	opts.Paths.StateDir = state
	// The named directories lie inside the state directory of the instance
	// and do not exist yet: the service creates them below the state
	// directory, and the account owns the state directory after the
	// installation.
	opts.CacheDir = filepath.Join(state, "models")
	opts.HubDir = filepath.Join(opts.CacheDir, "hub")
	// The local directory lies inside the state directory of the instance
	// as well: the installation grants the state directory to the account,
	// so the account can use the local directory as well.
	opts.LocalDir = filepath.Join(state, "output")

	if err := m.checkServiceAccountAccess(opts, other); err != nil {
		t.Errorf("checkServiceAccountAccess() = %v, want nil for a directory inside the state directory of the instance", err)
	}

	// A directory outside of the state directory is still checked: the
	// installation never grants it to the account.
	foreign := filepath.Join(root, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatalf("create the foreign directory: %v", err)
	}
	if err := os.Chmod(foreign, 0o700); err != nil {
		t.Fatalf("close the foreign directory: %v", err)
	}
	opts.LocalDir = foreign
	if err := m.checkServiceAccountAccess(opts, other); err == nil {
		t.Error("checkServiceAccountAccess() = nil, want the refusal of a directory outside of the state directory of the instance")
	}
}

// TestSystemdCheckServiceAccountAccessChecksAnExistingDirectoryInsideTheStateRoot
// checks that a named directory inside the state directory of the instance is
// only exempt from the check while it does not exist yet.
//
// The installation grants the state directory to the account and creates
// nothing below it: a directory that exists already inside the state
// directory belongs to whoever owns it, and the account has to be able to
// write there. The installation refuses such a directory when the account
// cannot, so the failure happens at the installation and not at the first
// write of the service.
func TestSystemdCheckServiceAccountAccessChecksAnExistingDirectoryInsideTheStateRoot(t *testing.T) {
	other := systemdSecondAccount(t)
	root := systemdTraversableTempDir(t)

	// The state directory of the instance. The installation grants it to
	// the account, so the account passes through it: the mode leaves the
	// traversal to every account.
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o711); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	if err := os.Chmod(state, 0o711); err != nil {
		t.Fatalf("open the state directory to the traversal: %v", err)
	}

	// A named directory inside the state directory that does not exist
	// yet: the service creates it below the state directory, and the
	// account owns the state directory after the installation.
	missing := filepath.Join(state, "models")

	// A named directory inside the state directory that exists and that
	// the account cannot write into: it belongs to the user of the test
	// and to nobody else.
	closed := filepath.Join(state, "closed")
	if err := os.Mkdir(closed, 0o700); err != nil {
		t.Fatalf("create the closed directory: %v", err)
	}
	if err := os.Chmod(closed, 0o700); err != nil {
		t.Fatalf("close the closed directory: %v", err)
	}

	// A named directory inside the state directory that exists and that
	// the account can write into.
	open := filepath.Join(state, "open")
	if err := os.Mkdir(open, 0o777); err != nil {
		t.Fatalf("create the open directory: %v", err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatalf("open the open directory: %v", err)
	}

	// A named directory outside of the state directory that does not
	// exist yet: the account has to create it below the scratch
	// directory, and it cannot write there.
	outsideMissing := filepath.Join(root, "outside-missing")

	// A directory outside of the state directory that the account of the
	// check cannot write into.
	foreign := filepath.Join(root, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatalf("create the foreign directory: %v", err)
	}
	if err := os.Chmod(foreign, 0o700); err != nil {
		t.Fatalf("close the foreign directory: %v", err)
	}

	// A link inside the state directory that points out of it, to a
	// directory that exists.
	outside := filepath.Join(state, "outside")
	if err := os.Symlink(foreign, outside); err != nil {
		t.Fatalf("create the link out of the state directory: %v", err)
	}

	// A link inside the state directory that points out of it, to a
	// directory that does not exist yet.
	outsideMissingLink := filepath.Join(state, "outside-missing")
	if err := os.Symlink(outsideMissing, outsideMissingLink); err != nil {
		t.Fatalf("create the link to the missing directory: %v", err)
	}

	m := &systemdManager{scope: ScopeSystem}
	opts := systemdTestOptions(t, ScopeSystem)
	opts.Paths.StateDir = state
	opts.HubDir = ""
	opts.LocalDir = ""

	cases := []struct {
		name    string
		cache   string
		wantErr bool
	}{
		{name: "a missing directory inside the state directory", cache: missing},
		{name: "an existing directory inside the state directory that the account cannot write", cache: closed, wantErr: true},
		{name: "an existing directory inside the state directory that the account can write", cache: open},
		{name: "a missing directory outside of the state directory", cache: outsideMissing, wantErr: true},
		{name: "a link inside the state directory that points to an existing directory out of it", cache: outside, wantErr: true},
		{name: "a link inside the state directory that points to a missing directory out of it", cache: outsideMissingLink, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts.CacheDir = tc.cache
			err := m.checkServiceAccountAccess(opts, other)
			if !tc.wantErr {
				if err != nil {
					t.Errorf("checkServiceAccountAccess() = %v, want nil for the directory %q", err, tc.cache)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkServiceAccountAccess() = nil, want the refusal of the directory %q", tc.cache)
			}
			// The refusal names the directory, the account and the
			// access that is missing.
			message := err.Error()
			for _, want := range []string{
				"the service account " + strconv.Quote(other.Username),
				strconv.Quote(tc.cache),
				"cannot write",
			} {
				if !strings.Contains(message, want) {
					t.Errorf("checkServiceAccountAccess() = %q, want a message that mentions %q", message, want)
				}
			}
		})
	}
}

// TestSystemdCheckServiceAccountAccessUsesTheRealLocationOfTheDirectory
// checks that the exemption for a directory inside the state directory of the
// instance is decided on the real location of the paths.
//
// The kernel follows the symbolic links of a path before it uses it, so a
// link inside the state directory that points out of it leaves the state
// directory: the account has to reach the target of the link on its own, and
// the installation refuses the directory when it cannot.
func TestSystemdCheckServiceAccountAccessUsesTheRealLocationOfTheDirectory(t *testing.T) {
	other := systemdSecondAccount(t)
	root := systemdTraversableTempDir(t)

	// The state directory belongs to the account of the test: the
	// installation grants it to the service account, so a directory inside
	// it is not checked against the account. The account of the check may
	// pass through it, so a refusal of a case is about the directory of the
	// case and not about the state directory.
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0o711); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	if err := os.Chmod(state, 0o711); err != nil {
		t.Fatalf("open the state directory to the traversal: %v", err)
	}

	// A plain directory inside the state directory. It exists already, so
	// it belongs to whoever owns it: the account of the check cannot write
	// into it, and the installation refuses it.
	plain := filepath.Join(state, "models")
	if err := os.Mkdir(plain, 0o700); err != nil {
		t.Fatalf("create the plain directory: %v", err)
	}
	if err := os.Chmod(plain, 0o700); err != nil {
		t.Fatalf("close the plain directory: %v", err)
	}

	// A directory outside of the state directory that the account of the
	// check cannot write into.
	foreign := filepath.Join(root, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatalf("create the foreign directory: %v", err)
	}
	if err := os.Chmod(foreign, 0o700); err != nil {
		t.Fatalf("close the foreign directory: %v", err)
	}

	// A directory outside of the state directory that every account can
	// write into.
	reachable := filepath.Join(root, "reachable")
	if err := os.Mkdir(reachable, 0o777); err != nil {
		t.Fatalf("create the reachable directory: %v", err)
	}
	if err := os.Chmod(reachable, 0o777); err != nil {
		t.Fatalf("open the reachable directory: %v", err)
	}

	// A link inside the state directory that points out of it.
	outside := filepath.Join(state, "outside")
	if err := os.Symlink(foreign, outside); err != nil {
		t.Fatalf("create the link out of the state directory: %v", err)
	}

	// A link inside the state directory that points to a place the account
	// of the check can reach.
	inside := filepath.Join(state, "inside")
	if err := os.Symlink(reachable, inside); err != nil {
		t.Fatalf("create the link to the reachable directory: %v", err)
	}

	// A directory inside the state directory that does not exist yet: the
	// installation creates it below the state directory, and the account
	// owns the state directory after the installation.
	missing := filepath.Join(state, "missing")

	m := &systemdManager{scope: ScopeSystem}
	opts := systemdTestOptions(t, ScopeSystem)
	opts.Paths.StateDir = state
	opts.HubDir = ""
	opts.LocalDir = ""

	cases := []struct {
		name    string
		cache   string
		wantErr bool
	}{
		{name: "a plain directory inside the state directory", cache: plain, wantErr: true},
		{name: "a missing directory inside the state directory", cache: missing},
		{name: "a link inside the state directory that points out of it", cache: outside, wantErr: true},
		{name: "a link inside the state directory that points to a reachable place", cache: inside},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts.CacheDir = tc.cache
			err := m.checkServiceAccountAccess(opts, other)
			if !tc.wantErr {
				if err != nil {
					t.Errorf("checkServiceAccountAccess() = %v, want nil for the directory %q", err, tc.cache)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkServiceAccountAccess() = nil, want the refusal of the directory %q", tc.cache)
			}
			// The refusal names the directory, the account and the
			// access that is missing.
			message := err.Error()
			for _, want := range []string{
				"the service account " + strconv.Quote(other.Username),
				strconv.Quote(tc.cache),
				"cannot write",
			} {
				if !strings.Contains(message, want) {
					t.Errorf("checkServiceAccountAccess() = %q, want a message that mentions %q", message, want)
				}
			}
		})
	}

	t.Run("the state directory is a link to another place", func(t *testing.T) {
		// The state directory of the instance is a link: the installation
		// grants the directory the link points at to the account, so a
		// directory inside it that does not exist yet is not checked
		// against the account.
		real := filepath.Join(root, "real-state")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatalf("create the real state directory: %v", err)
		}
		if err := os.Chmod(real, 0o700); err != nil {
			t.Fatalf("close the real state directory: %v", err)
		}
		models := filepath.Join(real, "models")
		if err := os.Mkdir(models, 0o700); err != nil {
			t.Fatalf("create the models directory: %v", err)
		}
		if err := os.Chmod(models, 0o700); err != nil {
			t.Fatalf("close the models directory: %v", err)
		}
		link := filepath.Join(root, "state-link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatalf("create the link of the state directory: %v", err)
		}

		opts.Paths.StateDir = link

		// A directory that does not exist yet below the linked state
		// directory: the service creates it, and the account owns the
		// real state directory after the installation.
		opts.CacheDir = filepath.Join(link, "output")
		if err := m.checkServiceAccountAccess(opts, other); err != nil {
			t.Errorf("checkServiceAccountAccess() = %v, want nil for a directory inside the state directory of the instance", err)
		}

		// The same directory named by its real location is inside the
		// state directory as well.
		opts.CacheDir = filepath.Join(real, "output")
		if err := m.checkServiceAccountAccess(opts, other); err != nil {
			t.Errorf("checkServiceAccountAccess() = %v, want nil for a directory inside the state directory of the instance", err)
		}

		// The directory that exists already inside the linked state
		// directory belongs to whoever owns it: the account cannot write
		// into it, and the installation refuses it.
		opts.CacheDir = models
		if err := m.checkServiceAccountAccess(opts, other); err == nil {
			t.Error("checkServiceAccountAccess() = nil, want the refusal of an existing directory inside the state directory of the instance")
		}
	})
}

// TestSystemdAccessCheckUsesTheEffectivePermission checks that the access
// check of a directory the service account has to use is the effective one.
//
// The ownership of a directory is not enough: a directory the account owns
// with the mode 0500 is not writable, because the owner has no write
// permission in it, and a directory below a parent without the execute
// permission cannot be reached at all.
//
// The account of the checks owns nothing of the test and belongs to the
// group of its directories, so the group permissions are the ones that
// apply to it.
func TestSystemdAccessCheckUsesTheEffectivePermission(t *testing.T) {
	current := systemdCurrentAccount(t)
	root := systemdTraversableTempDir(t)

	// The group of the directories of the test is the group of the test
	// process, so the other identity reaches them through the group.
	probe := filepath.Join(root, "probe")
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatalf("create the probe directory: %v", err)
	}
	_, group := systemdOwner(t, probe)

	// The other identity owns nothing of the test and belongs to the
	// group of its directories.
	other := &user.User{
		Username: "hfdesk-access-test",
		Uid:      "65534",
		Gid:      group,
		Name:     "HFDesk access test account",
		HomeDir:  "/nonexistent",
	}

	cases := []struct {
		name    string
		dir     string
		account *user.User
		mode    os.FileMode
		wantErr bool
	}{
		{
			name:    "a directory the account owns",
			dir:     "owned",
			account: current,
			mode:    0o700,
		},
		{
			name:    "a directory the account owns without the write permission",
			dir:     "readonly",
			account: current,
			mode:    0o500,
			wantErr: true,
		},
		{
			name:    "a directory the account owns without the write and the execute permission",
			dir:     "closed",
			account: current,
			mode:    0o400,
			wantErr: true,
		},
		{
			name:    "a directory of the group of the account",
			dir:     "group",
			account: other,
			mode:    0o770,
		},
		{
			name:    "a directory of the group of the account without the execute permission",
			dir:     "group-without-execute",
			account: other,
			mode:    0o620,
			wantErr: true,
		},
		{
			name:    "a directory the account does not own and cannot write into",
			dir:     "foreign",
			account: other,
			mode:    0o700,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(root, tc.dir)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatalf("create the directory: %v", err)
			}
			// The mode of a create call is filtered by the umask of
			// the process, so the mode is set explicitly.
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatalf("set the mode of the directory: %v", err)
			}
			err := checkDirWritableByAccount(dir, tc.account)
			if tc.wantErr && err == nil {
				t.Errorf("checkDirWritableByAccount(%q) = nil, want a refusal", dir)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("checkDirWritableByAccount(%q) = %v, want nil", dir, err)
			}
		})
	}

	t.Run("a directory below a parent without the execute permission", func(t *testing.T) {
		parent := filepath.Join(root, "closed-parent")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatalf("create the parent directory: %v", err)
		}
		child := filepath.Join(parent, "child")
		if err := os.Mkdir(child, 0o700); err != nil {
			t.Fatalf("create the child directory: %v", err)
		}
		// The group of the child directory is the group of the
		// account, so the account could use it if it could reach it.
		if err := os.Chmod(child, 0o770); err != nil {
			t.Fatalf("make the child directory usable: %v", err)
		}
		// The parent is closed for everybody but its owner.
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Fatalf("close the parent directory: %v", err)
		}
		if err := checkDirWritableByAccount(child, other); err == nil {
			t.Error("checkDirWritableByAccount() = nil, want the refusal of a directory the account cannot reach")
		}
	})
}

// TestSystemdAccessCheckResolvesTheSymlinksOfThePath checks that the access
// check judges the real location of a path.
//
// The kernel follows the symbolic links of a path before it uses it, so a
// directory that is reached through a link is used by its real location. A
// check that asked about the path as it was written would accept a directory
// whose real location the account never reaches, and the instance would fail
// with a permission error of the file system.
//
// The account of the check owns nothing of the test and is not a member of
// the group of its directories, so the permissions of the other accounts
// apply to it.
func TestSystemdAccessCheckResolvesTheSymlinksOfThePath(t *testing.T) {
	other := systemdSecondAccount(t)
	root := systemdTraversableTempDir(t)

	// The public directory is world writable, so the account could create
	// a directory in it. The private directory is closed for everybody but
	// its owner, so the account never reaches what is below it.
	public := filepath.Join(root, "public")
	if err := os.Mkdir(public, 0o777); err != nil {
		t.Fatalf("create the public directory: %v", err)
	}
	if err := os.Chmod(public, 0o777); err != nil {
		t.Fatalf("make the public directory world writable: %v", err)
	}
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatalf("create the private directory: %v", err)
	}
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatalf("close the private directory: %v", err)
	}
	data := filepath.Join(private, "data")
	if err := os.Mkdir(data, 0o777); err != nil {
		t.Fatalf("create the data directory: %v", err)
	}
	if err := os.Chmod(data, 0o777); err != nil {
		t.Fatalf("make the data directory world writable: %v", err)
	}

	// The open directory is traversable by the account, so the real
	// location of the directory below it is usable.
	open := filepath.Join(root, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatalf("create the open directory: %v", err)
	}
	openData := filepath.Join(open, "data")
	if err := os.Mkdir(openData, 0o777); err != nil {
		t.Fatalf("create the data directory of the open directory: %v", err)
	}
	if err := os.Chmod(openData, 0o777); err != nil {
		t.Fatalf("make the data directory world writable: %v", err)
	}

	// The locked directory is closed for everybody but its owner, so the
	// account never reaches the link in it.
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatalf("create the locked directory: %v", err)
	}
	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatalf("close the locked directory: %v", err)
	}
	lockedLink := filepath.Join(locked, "cache")
	if err := os.Symlink(filepath.Join("..", "open", "data"), lockedLink); err != nil {
		t.Fatalf("create the link in the locked directory: %v", err)
	}

	// The links are the paths the user names: the cache points into the
	// private directory, the models into the open one.
	linked := filepath.Join(public, "cache")
	if err := os.Symlink(filepath.Join("..", "private", "data"), linked); err != nil {
		t.Fatalf("create the link into the private directory: %v", err)
	}
	linkedMissing := filepath.Join(public, "missing")
	if err := os.Symlink(filepath.Join("..", "private", "new"), linkedMissing); err != nil {
		t.Fatalf("create the link into the missing directory: %v", err)
	}
	openLink := filepath.Join(public, "models")
	if err := os.Symlink(filepath.Join("..", "open", "data"), openLink); err != nil {
		t.Fatalf("create the link into the open directory: %v", err)
	}

	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name:    "a link into a directory the account cannot reach",
			path:    linked,
			wantErr: true,
		},
		{
			name:    "a link into a directory that does not exist yet below a directory the account cannot reach",
			path:    linkedMissing,
			wantErr: true,
		},
		{
			name:    "a link the account cannot reach itself",
			path:    lockedLink,
			wantErr: true,
		},
		{
			name: "a link into a directory the account can reach",
			path: openLink,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDirWritableByAccount(tc.path, other)
			if tc.wantErr && err == nil {
				t.Errorf("checkDirWritableByAccount(%q) = nil, want the refusal of the real location of the path", tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("checkDirWritableByAccount(%q) = %v, want nil for the real location of the path", tc.path, err)
			}
		})
	}
}

// TestSystemdAccessCheckUsesTheSupplementaryGroups checks that the access
// check of a directory the service account has to use knows the
// supplementary groups of the account.
//
// The groups of an account are read from the group database of the machine,
// so the test needs a group the account of the test belongs to. The test
// skips itself when the machine knows no such group.
func TestSystemdAccessCheckUsesTheSupplementaryGroups(t *testing.T) {
	current := systemdCurrentAccount(t)
	group, ok := systemdSupplementaryGroup(t)
	if !ok {
		t.Skip("skipping: the machine knows no supplementary group of the account of the test")
	}
	root := systemdTraversableTempDir(t)

	dir := filepath.Join(root, "supplementary")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}
	gid, err := strconv.Atoi(group)
	if err != nil {
		t.Fatalf("the group id %q is not a number: %v", group, err)
	}
	// The directory belongs to the group of the account, not to the
	// account itself.
	if err := os.Chown(dir, os.Getuid(), gid); err != nil {
		t.Fatalf("give the directory to the group %q: %v", group, err)
	}

	// The account of the check carries the name of the account of the
	// test, so the group database of the machine gives it the
	// supplementary group of the account of the test. It owns nothing of
	// the test.
	account := &user.User{
		Username: current.Username,
		Uid:      "65534",
		Gid:      current.Gid,
		Name:     current.Name,
		HomeDir:  current.HomeDir,
	}

	// The group permissions apply to the account: it can create entries
	// in the directory and it can reach it.
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatalf("make the directory usable: %v", err)
	}
	if err := checkDirWritableByAccount(dir, account); err != nil {
		t.Errorf("checkDirWritableByAccount() = %v, want nil for a directory of the supplementary group of the account", err)
	}

	// The group has no execute permission: the account cannot use the
	// directory.
	if err := os.Chmod(dir, 0o620); err != nil {
		t.Fatalf("close the directory: %v", err)
	}
	if err := checkDirWritableByAccount(dir, account); err == nil {
		t.Error("checkDirWritableByAccount() = nil, want the refusal of a directory without the execute permission of the group")
	}
}

// TestSystemdStopTimeoutCoversTheShutdownOfTheServer checks that the
// deadline of the supervisor covers the graceful shutdown of the server.
//
// The server publishes the budget of its own shutdown: the drain of the HTTP
// server and the close of the job manager, each with its own budget. The
// unit has to wait at least that long, or systemd kills the instance while
// it is still saving its state.
func TestSystemdStopTimeoutCoversTheShutdownOfTheServer(t *testing.T) {
	if ShutdownBudget < server.GracefulShutdownTimeout {
		t.Errorf("the shared budget of the service layer = %s, want at least %s", ShutdownBudget, server.GracefulShutdownTimeout)
	}

	opts := systemdTestOptions(t, ScopeUser)
	unit, err := renderUnit(opts, nil)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}

	value := unitDirective(unit, "TimeoutStopSec")
	if value == "" {
		t.Fatalf("the unit has no TimeoutStopSec directive:\n%s", unit)
	}
	timeout, err := time.ParseDuration(value)
	if err != nil {
		t.Fatalf("the TimeoutStopSec of the unit = %q, want a duration: %v", value, err)
	}
	if timeout < server.GracefulShutdownTimeout {
		t.Errorf("the TimeoutStopSec of the unit = %s, want at least %s: the deadline of the supervisor has to cover the graceful shutdown of the server", timeout, server.GracefulShutdownTimeout)
	}
	if timeout != ShutdownBudget {
		t.Errorf("the TimeoutStopSec of the unit = %s, want the shared budget %s", timeout, ShutdownBudget)
	}
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

	_, err := m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the cache directory")
	}
	want := fmt.Sprintf("service: the service account %q cannot write to the cache directory %q: grant it access and install again (for example add the account to the group that owns the directory, or give the directory itself to the account with %q; for a directory that holds a lot of data, granting group access is preferable to changing the owner)",
		account.Username, cache, "chown "+account.Username+" "+cache)
	if err.Error() != want {
		t.Errorf("Install() = %q, want %q", err, want)
	}
	// The refusal names the directory, the account and the missing write
	// access.
	for _, part := range []string{account.Username, cache, "cannot write to"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("Install() = %q, want the %q in the refusal", err, part)
		}
	}
	// The refusal never names a recursive ownership change: the tree of a
	// model or a cache directory belongs to the user who named it, and the
	// installation never walks it.
	if strings.Contains(err.Error(), "-R") {
		t.Errorf("Install() = %q, want no recursive ownership change in the refusal", err)
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

// TestSystemdInstallRefusesAControlCharacterInAPath checks that an
// installation whose path carries a control character is refused before
// anything is written.
//
// A unit file is a line based file: a newline in a path would end the
// directive it is written in and start another one the installation never
// wrote.
func TestSystemdInstallRefusesAControlCharacterInAPath(t *testing.T) {
	root := t.TempDir()
	// The unit of the user scope lives in the configuration directory of
	// the user, so the test points it at a directory of its own.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	m := &systemdManager{scope: ScopeUser}
	opts := InstallOptions{
		Scope:      ScopeUser,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
		LocalDir: filepath.Join(root, "var", "lib", "hf\ndesk"),
	}

	_, err := m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of a path with a control character")
	}
	if !strings.Contains(err.Error(), "control character") {
		t.Errorf("Install() = %q, want the refusal of the control character", err)
	}
	if !strings.Contains(err.Error(), "local dir") {
		t.Errorf("Install() = %q, want the name of the path in the refusal", err)
	}

	// The refusal happens before anything is written: no directory of the
	// instance exists and no unit file was written.
	for _, dir := range instanceDirs(opts) {
		if _, err := os.Stat(dir.path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused installation created the %s %q: %v", dir.name, dir.path, err)
		}
	}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused installation wrote the unit file %q: %v", path, err)
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
	unit, err := renderUnit(opts, nil)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}
	for _, directive := range unitDirectives(unit) {
		if strings.HasPrefix(directive, "User=") || strings.HasPrefix(directive, "Group=") {
			t.Errorf("a user scope unit names an account: %q", directive)
		}
	}
}

// systemdFakeSupervisor replaces the supervisor of the machine with a fake
// one, so an installation can be driven through a known point.
//
// The fake fails every call, unless the environment variable
// HFDESK_TEST_SUPERVISOR is "ok". A failing installation therefore stops
// after the directories of the instance were created, which is the point the
// cleanup of a failed installation has to cover.
//
// The fake writes a marker file into the directory of the environment
// variable HFDESK_TEST_MARKER_DIR when that variable names a directory, so
// a test can make a directory of the instance hold data before the
// installation fails.
func systemdFakeSupervisor(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
if [ -n "$HFDESK_TEST_MARKER_DIR" ]; then
	mkdir -p "$HFDESK_TEST_MARKER_DIR" || exit 1
	: > "$HFDESK_TEST_MARKER_DIR/marker" || exit 1
fi
if [ "$HFDESK_TEST_SUPERVISOR" = "ok" ]; then
	exit 0
fi
echo "the fake supervisor refuses the command" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake supervisor: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// systemdTempInstallOptions returns the options of a user scope installation
// whose directories live below the given root.
//
// The scope is the user scope: it needs neither an elevated shell nor a
// service account, so the test runs everywhere.
func systemdTempInstallOptions(t *testing.T, root string) InstallOptions {
	t.Helper()
	return InstallOptions{
		Scope:      ScopeUser,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
	}
}

// systemdTempInstallEnvironment points the registration and the home
// directory of the instance at temporary directories, so a test never touches
// the registration of the user of the test.
func systemdTempInstallEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

// TestSystemdCreatedDirOrder checks the order of the directories a failed
// installation removes: the directories are listed in the order they are
// created, so a parent is always removed after its children.
func TestSystemdCreatedDirOrder(t *testing.T) {
	root := t.TempDir()
	opts := InstallOptions{
		Scope:      ScopeUser,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
	}

	created := make(map[string][]string, 3)
	for _, dir := range instanceDirs(opts) {
		created[dir.path] = createdDirs(dir.path)
	}
	order := createdDirOrder(opts, created)

	want := []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		filepath.Join(root, "var", "lib", "hfdesk"),
		filepath.Join(root, "var", "log"),
		filepath.Join(root, "var", "log", "hfdesk"),
	}
	if strings.Join(order, "\n") != strings.Join(want, "\n") {
		t.Errorf("createdDirOrder() =\n%s\nwant\n%s", strings.Join(order, "\n"), strings.Join(want, "\n"))
	}
}

// TestSystemdFailedInstallRemovesTheDirectoriesItCreated checks that a
// failed installation removes the directories it created, so a failed attempt
// leaves the machine as it was.
func TestSystemdFailedInstallRemovesTheDirectoriesItCreated(t *testing.T) {
	systemdFakeSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()

	opts := systemdTempInstallOptions(t, root)
	m := &systemdManager{scope: ScopeUser}
	if _, err := m.Install(context.Background(), opts); err == nil {
		t.Fatal("Install() = nil, want the failure of the supervisor")
	}

	for _, dir := range []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		filepath.Join(root, "var", "lib", "hfdesk"),
		filepath.Join(root, "var", "log"),
		filepath.Join(root, "var", "log", "hfdesk"),
	} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the directory %q is still there after the failed installation (stat error: %v)", dir, err)
		}
	}
}

// TestSystemdFailedInstallKeepsADirectoryThatExistedBefore checks that a
// failed installation never removes a directory that existed before it: such
// a directory belongs to whoever owns it, with the ownership and the
// permissions it had.
func TestSystemdFailedInstallKeepsADirectoryThatExistedBefore(t *testing.T) {
	systemdFakeSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()

	// The config directory existed before the installation.
	config := filepath.Join(root, "etc")
	if err := os.Mkdir(config, 0o750); err != nil {
		t.Fatalf("create the config directory: %v", err)
	}
	owner, group := systemdOwner(t, config)

	opts := systemdTempInstallOptions(t, root)
	m := &systemdManager{scope: ScopeUser}
	if _, err := m.Install(context.Background(), opts); err == nil {
		t.Fatal("Install() = nil, want the failure of the supervisor")
	}

	// The directory that existed before the installation is still there,
	// with the owner and the permissions it had.
	info, err := os.Stat(config)
	if err != nil {
		t.Fatalf("the config directory %q is gone after the failed installation: %v", config, err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Errorf("the mode of the config directory %q = %v, want %v", config, info.Mode().Perm(), os.FileMode(0o750))
	}
	if uid, gid := systemdOwner(t, config); uid != owner || gid != group {
		t.Errorf("the ownership of the config directory %q = %q:%q, want %q:%q", config, uid, gid, owner, group)
	}
	// The directory the installation created inside it is gone.
	created := filepath.Join(config, "hfdesk")
	if _, err := os.Stat(created); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the directory %q is still there after the failed installation (stat error: %v)", created, err)
	}
}

// TestSystemdFailedInstallKeepsACreatedDirectoryThatHoldsData checks that
// the cleanup of a failed installation never deletes data: a directory the
// installation created but that holds something is kept, and the caller
// still gets the original error of the installation.
func TestSystemdFailedInstallKeepsACreatedDirectoryThatHoldsData(t *testing.T) {
	systemdFakeSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()

	opts := systemdTempInstallOptions(t, root)
	state := opts.Paths.StateDir
	m := &systemdManager{scope: ScopeUser}

	// The error of the failed supervisor call is the error the caller
	// gets: the cleanup of the directories never replaces it.
	t.Setenv("HFDESK_TEST_MARKER_DIR", t.TempDir())
	wantErr := m.reloadAndEnable(context.Background())
	if wantErr == nil {
		t.Fatal("the fake supervisor did not fail")
	}

	// The fake supervisor writes a marker into the state directory, so the
	// directory holds data when the installation fails.
	t.Setenv("HFDESK_TEST_MARKER_DIR", state)
	_, err := m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the failure of the supervisor")
	}
	if err.Error() != wantErr.Error() {
		t.Errorf("Install() = %q, want %q", err, wantErr)
	}

	// The state directory holds the marker, so it is kept, and so are the
	// directories that hold it.
	for _, dir := range []string{
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		state,
	} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("the directory %q is gone after the failed installation: %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "marker")); err != nil {
		t.Errorf("the marker of the state directory is gone: %v", err)
	}
	// The other directories of the instance are empty, so they are gone.
	for _, dir := range []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var", "log"),
		filepath.Join(root, "var", "log", "hfdesk"),
	} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the directory %q is still there after the failed installation (stat error: %v)", dir, err)
		}
	}
}

// TestSystemdFailedInstallRemovesTheDirectoriesOfAFailedPreparation checks
// that an installation that fails while it prepares the directories of the
// instance leaves the machine as it was as well: the directories it created
// before the failure are removed too.
func TestSystemdFailedInstallRemovesTheDirectoriesOfAFailedPreparation(t *testing.T) {
	systemdFakeSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()

	opts := systemdTempInstallOptions(t, root)
	// The log directory is the root of the file system: the installation
	// refuses to tighten it, and it fails after the other two
	// directories were created.
	opts.Paths.LogDir = string(filepath.Separator)

	m := &systemdManager{scope: ScopeUser}
	_, err := m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the log directory")
	}
	want := fmt.Sprintf("service: log dir path %q is the filesystem root and must not be tightened", string(filepath.Separator))
	if err.Error() != want {
		t.Errorf("Install() = %q, want %q", err, want)
	}

	// The directories the installation created before it failed are gone.
	for _, dir := range []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		filepath.Join(root, "var", "lib", "hfdesk"),
	} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the directory %q is still there after the failed installation (stat error: %v)", dir, err)
		}
	}
}

// TestSystemdSuccessfulInstallKeepsItsDirectories checks that a successful
// installation keeps the directories it created: the cleanup of a failed
// installation never touches the directories of an instance that runs.
func TestSystemdSuccessfulInstallKeepsItsDirectories(t *testing.T) {
	systemdFakeSupervisor(t)
	t.Setenv("HFDESK_TEST_SUPERVISOR", "ok")
	systemdTempInstallEnvironment(t)
	root := t.TempDir()

	opts := systemdTempInstallOptions(t, root)
	m := &systemdManager{scope: ScopeUser}
	if _, err := m.Install(context.Background(), opts); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}

	for _, dir := range []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		filepath.Join(root, "var", "lib", "hfdesk"),
		filepath.Join(root, "var", "log"),
		filepath.Join(root, "var", "log", "hfdesk"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("the directory %q is gone after the successful installation: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("the path %q is not a directory after the successful installation", dir)
		}
	}
}

// TestSystemdLifecycleOfAnAbsentUnit checks the lifecycle commands against a
// service that is not registered: the stop is a stable success, and the
// start and the restart report that the service is not installed.
//
// The test never touches a real installation: the unit file of the user
// scope lives in a temporary configuration directory, and the test skips
// itself when the machine holds a unit of the machine scope.
func TestSystemdLifecycleOfAnAbsentUnit(t *testing.T) {
	t.Run("the user scope", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		m := &systemdManager{scope: ScopeUser}
		path, err := m.unitPath()
		if err != nil {
			t.Fatalf("unitPath() = %v, want nil", err)
		}
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("the unit file %q exists, the test needs a service that is not registered", path)
		}

		ctx := context.Background()
		// A service that is not registered does not run either: the
		// state the caller asked for already holds.
		if err := m.Stop(ctx); err != nil {
			t.Errorf("Stop() = %v, want nil for a service that is not registered", err)
		}
		// A service that is not registered cannot be started, and the
		// reason is reported in every shell.
		err = m.Start(ctx)
		if err == nil {
			t.Fatal("Start() = nil, want an error for a service that is not registered")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("Start() = %v, want an error that names the missing registration", err)
		}
		// A restart cannot start the service either.
		err = m.Restart(ctx)
		if err == nil {
			t.Fatal("Restart() = nil, want an error for a service that is not registered")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("Restart() = %v, want an error that names the missing registration", err)
		}
	})

	t.Run("the machine-wide scope", func(t *testing.T) {
		m := &systemdManager{scope: ScopeSystem}
		path, err := m.unitPath()
		if err != nil {
			t.Fatalf("unitPath() = %v, want nil", err)
		}
		if _, err := os.Stat(path); err == nil {
			t.Skipf("the machine has a unit at %q: the test does not touch it", path)
		}

		ctx := context.Background()
		if err := m.Stop(ctx); err != nil {
			t.Errorf("Stop() = %v, want nil for a service that is not registered", err)
		}
		err = m.Start(ctx)
		if err == nil {
			t.Fatal("Start() = nil, want an error for a service that is not registered")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("Start() = %v, want an error that names the missing registration", err)
		}
		err = m.Restart(ctx)
		if err == nil {
			t.Fatal("Restart() = nil, want an error for a service that is not registered")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("Restart() = %v, want an error that names the missing registration", err)
		}
	})
}

// TestSystemdMachineWideLifecycleNeedsRoot checks the elevation requirement
// of the machine-wide scope for the lifecycle commands.
//
// A unit of the machine belongs to the machine, so starting, stopping and
// restarting it needs root. The user scope belongs to the user of the
// session and never needs an elevation.
//
// The state of the registration and the user id are parameters of the
// guard, so the test drives every case without an elevated shell.
func TestSystemdMachineWideLifecycleNeedsRoot(t *testing.T) {
	// A user id that is not the one of root.
	const unprivileged = 501

	cases := []struct {
		name   string
		kind   lifecycleKind
		action string
	}{
		{name: "a start", kind: lifecycleStart, action: "a start"},
		{name: "a stop", kind: lifecycleStop, action: "a stop"},
		{name: "a restart", kind: lifecycleStart, action: "a restart"},
	}
	for _, tc := range cases {
		t.Run("the machine-wide scope refuses "+tc.name+" without root", func(t *testing.T) {
			m := &systemdManager{scope: ScopeSystem}

			_, err := m.requireLifecycle(tc.kind, true, unprivileged, tc.action)
			if err == nil {
				t.Fatal("requireLifecycle() = nil, want the refusal of a shell that is not elevated")
			}
			want := fmt.Sprintf("service: %s in the system scope requires root: run the command as root, HFDesk never escalates privileges itself", tc.action)
			if err.Error() != want {
				t.Errorf("requireLifecycle() = %q, want %q", err, want)
			}
		})

		t.Run("the machine-wide scope accepts "+tc.name+" with root", func(t *testing.T) {
			m := &systemdManager{scope: ScopeSystem}

			if _, err := m.requireLifecycle(tc.kind, true, 0, tc.action); err != nil {
				t.Errorf("requireLifecycle() = %v, want nil for an elevated shell", err)
			}
		})

		t.Run("the user scope is not refused for "+tc.name, func(t *testing.T) {
			m := &systemdManager{scope: ScopeUser}

			if _, err := m.requireLifecycle(tc.kind, true, unprivileged, tc.action); err != nil {
				t.Errorf("requireLifecycle() = %v, want nil for a user scope instance", err)
			}
		})
	}

	t.Run("the check of the process uses the effective user id of the process", func(t *testing.T) {
		m := &systemdManager{scope: ScopeSystem}

		err := m.requireRoot("a start")
		forced := m.requireRootFor("a start", os.Geteuid())
		switch {
		case err == nil && forced == nil:
		case err != nil && forced != nil && err.Error() == forced.Error():
		default:
			t.Errorf("requireRoot() = %v, want the decision of requireRootFor(%d) = %v", err, os.Geteuid(), forced)
		}
	})
}

// TestSystemdStopOfAnAbsentUnitReachesItsState checks the answer of the
// guard for a service that is not registered: a stop reaches its wanted
// state already, so it is a stable success that must not touch the
// supervisor, and a start and a restart are refused.
func TestSystemdStopOfAnAbsentUnitReachesItsState(t *testing.T) {
	m := &systemdManager{scope: ScopeSystem}

	outcome, err := m.requireLifecycle(lifecycleStop, false, 501, "a stop")
	if err != nil {
		t.Fatalf("requireLifecycle() = %v, want nil for a stop of a service that is not registered", err)
	}
	if outcome != lifecycleAlreadyReached {
		t.Errorf("requireLifecycle() = %d, want %d", outcome, lifecycleAlreadyReached)
	}

	for _, action := range []string{"a start", "a restart"} {
		_, err := m.requireLifecycle(lifecycleStart, false, 501, action)
		if err == nil {
			t.Fatalf("requireLifecycle() = nil, want the refusal of %s for a service that is not registered", action)
		}
		want := fmt.Sprintf("service: %s is not installed", unitName)
		if err.Error() != want {
			t.Errorf("requireLifecycle() = %q, want %q", err, want)
		}
	}
}

// TestSystemdAbsentUnitComesBeforeTheElevationRequirement checks the order
// of the two checks of a lifecycle command.
//
// The registration of the service comes first: it needs no privilege and it
// gives the more useful answer. A stop of a service that is not registered
// therefore stays a stable success even when the shell is not elevated, and
// a start of it still names the missing registration instead of the
// elevation requirement.
func TestSystemdAbsentUnitComesBeforeTheElevationRequirement(t *testing.T) {
	m := &systemdManager{scope: ScopeSystem}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Skipf("the machine has a unit at %q: the test does not touch it", path)
	}

	// The elevation requirement refuses a shell that is not elevated.
	if err := m.requireRootFor("a stop", 501); err == nil {
		t.Fatal("requireRootFor() = nil, want the refusal of a shell that is not elevated")
	}

	// The stop of a service that is not registered is a stable success:
	// the registration is checked first, so the elevation requirement is
	// not even looked at.
	if err := m.Stop(context.Background()); err != nil {
		t.Errorf("Stop() = %v, want nil for a service that is not registered", err)
	}

	// The start of a service that is not registered names the missing
	// registration, not the elevation requirement.
	err = m.Start(context.Background())
	if err == nil {
		t.Fatal("Start() = nil, want an error for a service that is not registered")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("Start() = %v, want an error that names the missing registration", err)
	}
	if strings.Contains(err.Error(), "requires root") {
		t.Errorf("Start() = %v, want the missing registration and not the elevation requirement", err)
	}
}

// TestSystemdUserScopeLifecycleNeedsNoRoot checks that the lifecycle
// commands of a user scope instance are never refused for the elevation
// requirement: a user unit belongs to the user of the session.
//
// The supervisor of the test is a fake one, so the test never touches the
// supervisor of the machine, and the registration of the test lives in a
// temporary configuration directory.
func TestSystemdUserScopeLifecycleNeedsNoRoot(t *testing.T) {
	systemdFakeSupervisor(t)
	t.Setenv("HFDESK_TEST_SUPERVISOR", "ok")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := &systemdManager{scope: ScopeUser}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the unit directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatalf("write the unit file: %v", err)
	}

	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Errorf("Start() = %v, want nil for a user scope instance", err)
	}
	if err := m.Stop(ctx); err != nil {
		t.Errorf("Stop() = %v, want nil for a user scope instance", err)
	}
	if err := m.Restart(ctx); err != nil {
		t.Errorf("Restart() = %v, want nil for a user scope instance", err)
	}
}

// TestDefaultInstallOptionsNamesNothing checks the constructor of a request
// that names nothing: the defaults of the scope are filled and no field is
// marked explicit, so an installation over an existing registration keeps
// every setting of it.
func TestDefaultInstallOptionsNamesNothing(t *testing.T) {
	for _, scope := range []Scope{ScopeUser, ScopeSystem} {
		t.Run(string(scope), func(t *testing.T) {
			paths, err := DefaultPaths(scope)
			if err != nil {
				t.Fatalf("DefaultPaths(%q) = %v, want nil", scope, err)
			}
			cacheRoot, hubDir := DefaultCacheDirs(scope, paths)

			opts := DefaultInstallOptions(scope, "/usr/local/bin/hfdesk")
			if opts.Scope != scope {
				t.Errorf("the scope is %q, want %q", opts.Scope, scope)
			}
			if opts.Executable != "/usr/local/bin/hfdesk" {
				t.Errorf("the executable is %q, want the named one", opts.Executable)
			}
			if opts.Listen != DefaultListen {
				t.Errorf("the listen address is %q, want %q", opts.Listen, DefaultListen)
			}
			if opts.Paths != paths {
				t.Errorf("the paths are %+v, want %+v", opts.Paths, paths)
			}
			if opts.CacheDir != cacheRoot || opts.HubDir != hubDir {
				t.Errorf("the cache layout is %q/%q, want %q/%q", opts.CacheDir, opts.HubDir, cacheRoot, hubDir)
			}
			if opts.LocalDir != "" || opts.ServiceUser != "" {
				t.Errorf("the request names a local dir or an account: %+v", opts)
			}
			if opts.Explicit != (InstallFieldSet{}) {
				t.Errorf("the request marks fields explicit: %+v", opts.Explicit)
			}
		})
	}
}

// installOptionsForMerge returns a fully populated installation with the
// given prefix in every path.
func installOptionsForMerge(prefix, executable string) InstallOptions {
	return InstallOptions{
		Scope:      ScopeUser,
		Executable: executable,
		Listen:     prefix + "-listen",
		Paths: runtimepaths.Paths{
			ConfigFile: prefix + "/etc/hfdesk.json",
			StateDir:   prefix + "/var",
			LogDir:     prefix + "/var/log",
		},
		CacheDir:    prefix + "/cache",
		HubDir:      prefix + "/cache/hub",
		LocalDir:    prefix + "/models",
		ServiceUser: prefix + "-account",
	}
}

// TestMergeInstallOptionsKeepsTheRegistration checks the precedence of a
// reinstall: a field the request did not name keeps the value of the existing
// registration, even when the request carries a default of the scope.
func TestMergeInstallOptionsKeepsTheRegistration(t *testing.T) {
	requested := installOptionsForMerge("/default", "/new/bin/hfdesk")
	existing := installOptionsForMerge("/registered", "/old/bin/hfdesk")

	merged := mergeInstallOptions(requested, existing)

	if merged.Executable != requested.Executable {
		t.Errorf("the executable is %q, want the one of the request %q", merged.Executable, requested.Executable)
	}
	want := existing
	want.Executable = requested.Executable
	if !reflect.DeepEqual(merged, want) {
		t.Errorf("mergeInstallOptions() =\n%+v\nwant\n%+v", merged, want)
	}
}

// TestMergeInstallOptionsHonoursTheExplicitFields checks that a field the
// request named itself wins over the existing registration.
func TestMergeInstallOptionsHonoursTheExplicitFields(t *testing.T) {
	requested := installOptionsForMerge("/named", "/new/bin/hfdesk")
	requested.Explicit = InstallFieldSet{
		Listen:      true,
		Config:      true,
		StateDir:    true,
		LogDir:      true,
		CacheDir:    true,
		HubDir:      true,
		LocalDir:    true,
		ServiceUser: true,
	}
	existing := installOptionsForMerge("/registered", "/old/bin/hfdesk")

	merged := mergeInstallOptions(requested, existing)

	if !reflect.DeepEqual(merged, requested) {
		t.Errorf("mergeInstallOptions() =\n%+v\nwant the request\n%+v", merged, requested)
	}
}

// TestMergeInstallOptionsUsesTheDefaultsOnAFirstInstall checks that an
// installation without a registration installs exactly what the request
// says: every unmarked field falls back to the default of the request.
func TestMergeInstallOptionsUsesTheDefaultsOnAFirstInstall(t *testing.T) {
	requested := installOptionsForMerge("/default", "/new/bin/hfdesk")

	merged := mergeInstallOptions(requested, InstallOptions{})

	if !reflect.DeepEqual(merged, requested) {
		t.Errorf("mergeInstallOptions() =\n%+v\nwant the request\n%+v", merged, requested)
	}
}

// TestMergeInstallOptionsResolvesEachFieldOnItsOwn checks that every field is
// merged separately: naming one field never drags another one with it. The
// cache root and the hub directory are separate fields of the layer; the
// command line resolves their coupling before the request reaches it.
func TestMergeInstallOptionsResolvesEachFieldOnItsOwn(t *testing.T) {
	requested := installOptionsForMerge("/named", "/new/bin/hfdesk")
	requested.Explicit.CacheDir = true
	existing := installOptionsForMerge("/registered", "/old/bin/hfdesk")

	merged := mergeInstallOptions(requested, existing)

	if merged.CacheDir != requested.CacheDir {
		t.Errorf("the cache root is %q, want the named one %q", merged.CacheDir, requested.CacheDir)
	}
	if merged.HubDir != existing.HubDir {
		t.Errorf("the hub dir is %q, want the registered one %q", merged.HubDir, existing.HubDir)
	}
	if merged.Listen != existing.Listen || merged.Paths != existing.Paths || merged.LocalDir != existing.LocalDir || merged.ServiceUser != existing.ServiceUser {
		t.Errorf("mergeInstallOptions() named more than the cache root:\n%+v", merged)
	}
}

// TestSystemdRegistrationOptionsRecoversTheRegistration checks that the
// options of an existing registration are read back from the unit file the
// supervisor holds: the argument vector carries the paths and the bind
// address, the identity directive carries the account of a machine-wide
// instance.
func TestSystemdRegistrationOptionsRecoversTheRegistration(t *testing.T) {
	opts := systemdTestOptions(t, ScopeSystem)
	opts.ServiceUser = "hfdesk-svc"

	unit, err := renderUnit(opts, nil)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}

	got := registrationOptions(parseUnit(unit), ScopeSystem)
	want := opts
	want.Executable = opts.Executable
	if !reflect.DeepEqual(got, want) {
		t.Errorf("registrationOptions() =\n%+v\nwant\n%+v", got, want)
	}
}

// TestSystemdRegistrationOptionsNameNoAccountForAUserScope checks that a
// user scope registration never recovers an account: the instance runs as the
// user of the session, whatever an old command line named.
func TestSystemdRegistrationOptionsNameNoAccountForAUserScope(t *testing.T) {
	opts := systemdTestOptions(t, ScopeUser)
	opts.ServiceUser = "hfdesk-svc"

	unit, err := renderUnit(opts, nil)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}

	if got := registrationOptions(parseUnit(unit), ScopeUser).ServiceUser; got != "" {
		t.Errorf("the account of a user scope registration = %q, want none", got)
	}
}

// TestSystemdExecStartPath checks the program path of the ExecStart property
// the supervisor reports.
func TestSystemdExecStartPath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "a running invocation",
			output: "LoadState=loaded\nExecStart={ path=/opt/hfdesk/bin/hfdesk ; argv[]=/opt/hfdesk/bin/hfdesk service run ; ignore_errors=no ; pid=42 }\n",
			want:   "/opt/hfdesk/bin/hfdesk",
		},
		{
			name:   "a path with spaces",
			output: "ExecStart={ path=/opt/hf desk/bin/hfdesk ; argv[]=/opt/hf desk/bin/hfdesk ; ignore_errors=no }\n",
			want:   "/opt/hf desk/bin/hfdesk",
		},
		{
			name:   "an invocation without arguments",
			output: "ExecStart={ path=/bin/true ; ignore_errors=no }\n",
			want:   "/bin/true",
		},
		{
			name:   "an empty property",
			output: "ExecStart={ path= ; argv[]= }\n",
			want:   "",
		},
		{
			name:   "no property at all",
			output: "LoadState=not-found\n",
			want:   "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := execStartPath(tc.output); got != tc.want {
				t.Errorf("execStartPath(%q) = %q, want %q", tc.output, got, tc.want)
			}
		})
	}
}

// TestSystemdRestartVerifyWaitIsTheShutdownBudget checks that the verification
// of a restarted instance is bounded by the shutdown budget of the service
// layer, so the wait covers the orderly shutdown of the old process.
func TestSystemdRestartVerifyWaitIsTheShutdownBudget(t *testing.T) {
	if got := (&systemdManager{}).restartVerifyWait(); got != ShutdownBudget {
		t.Errorf("restartVerifyWait() = %s, want the shutdown budget %s", got, ShutdownBudget)
	}
	if got := (&systemdManager{restartWait: time.Second}).restartVerifyWait(); got != time.Second {
		t.Errorf("restartVerifyWait() = %s, want the wait of the manager %s", got, time.Second)
	}
}

// systemdRecordingSupervisor replaces systemctl with a fake supervisor that
// records every call and answers the state query of an installation.
//
// The reported state is HFDESK_TEST_SYSTEMCTL_ACTIVE, and the program of its
// ExecStart property is HFDESK_TEST_SYSTEMCTL_EXECUTABLE. A restart fails once
// when HFDESK_TEST_SYSTEMCTL_RESTART_FAIL_ONCE is "1", so a test can drive the
// failure of the first restart while the restore of the previous registration
// still succeeds.
//
// A restart reports the state it leaves behind: when
// HFDESK_TEST_SYSTEMCTL_RESTARTED names a marker file, the supervisor creates it
// on the first restart and reports HFDESK_TEST_SYSTEMCTL_ACTIVE_AFTER_RESTART
// from then on. A restart returns as soon as the unit was forked, so a test can
// drive a restart that is accepted while the unit is dead. When
// HFDESK_TEST_SYSTEMCTL_RESTART_COUNT names a file, the number of restarts is
// written to it; the program of the ExecStart property is then reported as
// HFDESK_TEST_SYSTEMCTL_EXECUTABLE_AFTER_RESTART from the second restart on, so a
// test can drive a restart of an update and a restart of a restore that bring
// different programs into service.
//
// The readiness probe is replaced for the run of the test: the fake supervisor
// forks no instance, so no server of the instance answers on the registered
// address. A test that drives the probe installs one of its own after this
// helper returned.
//
// The log path is returned. It is written even when the supervisor is asked
// for a property only, so a test can tell a restart from a state query.
func systemdRecordingSupervisor(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
if [ -n "$HFDESK_TEST_SYSTEMCTL_LOG" ]; then
	printf '%s\n' "$*" >> "$HFDESK_TEST_SYSTEMCTL_LOG" || exit 1
fi
if [ "$1" = "--user" ]; then
	shift
fi
case "$1" in
show)
	active="${HFDESK_TEST_SYSTEMCTL_ACTIVE:-inactive}"
	if [ -n "$HFDESK_TEST_SYSTEMCTL_RESTARTED" ] && [ -e "$HFDESK_TEST_SYSTEMCTL_RESTARTED" ] && [ -n "$HFDESK_TEST_SYSTEMCTL_ACTIVE_AFTER_RESTART" ]; then
		active="$HFDESK_TEST_SYSTEMCTL_ACTIVE_AFTER_RESTART"
	fi
	sub=dead
	if [ "$active" = "active" ]; then
		sub=running
	fi
	if [ "$active" = "activating" ]; then
		sub=start-pre
	fi
	echo "LoadState=loaded"
	echo "ActiveState=$active"
	echo "SubState=$sub"
	exe="$HFDESK_TEST_SYSTEMCTL_EXECUTABLE"
	if [ -n "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT" ] && [ -n "$HFDESK_TEST_SYSTEMCTL_EXECUTABLE_AFTER_RESTART" ] && [ -e "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT" ] && [ "$(cat "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT")" -ge 2 ]; then
		exe="$HFDESK_TEST_SYSTEMCTL_EXECUTABLE_AFTER_RESTART"
	fi
	if [ -n "$exe" ]; then
		echo "ExecStart={ path=$exe ; argv[]=$exe service run ; ignore_errors=no }"
	fi
	exit 0
	;;
restart)
	if [ -n "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT" ]; then
		count=0
		if [ -e "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT" ]; then
			count=$(cat "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT")
		fi
		count=$((count + 1))
		printf '%s' "$count" > "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT" || exit 1
	fi
	if [ "$HFDESK_TEST_SYSTEMCTL_RESTART_FAIL_ONCE" = "1" ] && [ ! -e "$HFDESK_TEST_SYSTEMCTL_RESTART_MARKER" ]; then
		: > "$HFDESK_TEST_SYSTEMCTL_RESTART_MARKER"
		echo "the fake supervisor refuses the restart" >&2
		exit 1
	fi
	if [ -n "$HFDESK_TEST_SYSTEMCTL_RESTARTED" ]; then
		: > "$HFDESK_TEST_SYSTEMCTL_RESTARTED" || exit 1
	fi
	exit 0
	;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake supervisor: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HFDESK_TEST_SYSTEMCTL_LOG", logPath)
	// The fake supervisor forks no instance, so the readiness probe of the
	// service layer is replaced for the run of the test. A test that drives
	// the probe installs its own after this helper returned.
	previous := serviceReadinessProbe
	serviceReadinessProbe = func(string) error { return nil }
	t.Cleanup(func() { serviceReadinessProbe = previous })
	return logPath
}

// systemdSupervisorCalls returns the lines the fake supervisor recorded.
func systemdSupervisorCalls(t *testing.T, logPath string) []string {
	t.Helper()
	content, err := os.ReadFile(logPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the supervisor log: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
}

// systemdSupervisorCallsTo counts the calls of the fake supervisor that equal
// the given command. The scope prefix of the manager is ignored, so a test
// names the command the way the manager issues it.
func systemdSupervisorCallsTo(calls []string, command ...string) int {
	want := strings.Join(command, " ")
	count := 0
	for _, call := range calls {
		if strings.TrimPrefix(call, "--user ") == want {
			count++
		}
	}
	return count
}

// systemdExistingInstallation writes the unit file of an existing user scope
// registration and returns the manager, the path and the content of the unit.
//
// The configuration directory of the user is a temporary directory, so the
// test never touches the registration of the user who runs the test.
func systemdExistingInstallation(t *testing.T, opts InstallOptions) (*systemdManager, string, string) {
	t.Helper()
	systemdTempInstallEnvironment(t)
	m := &systemdManager{scope: ScopeUser}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	unit, err := renderUnit(opts, nil)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the unit directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatalf("write the unit file: %v", err)
	}
	return m, path, unit
}

// systemdExistingOptions returns a user scope registration whose settings all
// differ from the defaults of the scope, so a reinstall over it proves which
// values were kept.
func systemdExistingOptions(t *testing.T, executable, root string) InstallOptions {
	t.Helper()
	return InstallOptions{
		Scope:      ScopeUser,
		Executable: executable,
		Listen:     "127.0.0.1:19090",
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
		CacheDir: filepath.Join(root, "cache"),
		HubDir:   filepath.Join(root, "cache", "hub"),
		LocalDir: filepath.Join(root, "models"),
	}
}

// TestSystemdReinstallOfARunningUnitSwitchesTheExecutable checks that an
// installation over a running registration keeps every setting the request
// did not name, points the registration at the new executable and restarts the
// instance, so the running process is the new program.
func TestSystemdReinstallOfARunningUnitSwitchesTheExecutable(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)

	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)

	requested := DefaultInstallOptions(ScopeUser, newExecutable)
	effective, err := m.Install(context.Background(), requested)
	if err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}
	// The manager reports the registration it wrote: every setting the
	// request did not name is the one of the previous registration, and
	// the executable is the new one.
	wantEffective := previous
	wantEffective.Executable = newExecutable
	if !reflect.DeepEqual(effective, wantEffective) {
		t.Errorf("Install() returned\n%+v\nwant the effective registration\n%+v", effective, wantEffective)
	}

	content, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read the unit file: %v", err)
	}
	unit := parseUnit(string(content))
	if unit.ExecStart != newExecutable {
		t.Errorf("the registered executable is %q, want %q", unit.ExecStart, newExecutable)
	}
	for _, kept := range []struct {
		name string
		got  string
		want string
	}{
		{"the listen address", unit.listen(), previous.Listen},
		{"the config file", unit.flagValue("--config"), previous.Paths.ConfigFile},
		{"the state dir", unit.paths().StateDir, previous.Paths.StateDir},
		{"the log dir", unit.flagValue("--log-dir"), previous.Paths.LogDir},
		{"the cache root", unit.flagValue("--cache-dir"), previous.CacheDir},
		{"the hub dir", unit.flagValue("--hub-dir"), previous.HubDir},
		{"the local dir", unit.flagValue("--local-dir"), previous.LocalDir},
	} {
		if kept.got != kept.want {
			t.Errorf("%s = %q, want the registered %q", kept.name, kept.got, kept.want)
		}
	}
	if string(content) == previousUnit {
		t.Error("the unit file was not rewritten for the new executable")
	}

	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 1 {
		t.Errorf("the unit was restarted %d times, want exactly once after the reload:\n%s", got, strings.Join(calls, "\n"))
	}
}

// TestSystemdReinstallOfAStoppedUnitDoesNotStartIt checks that a registration
// that was stopped stays stopped: an installation never starts an instance
// that was not running.
func TestSystemdReinstallOfAStoppedUnitDoesNotStartIt(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)

	m, unitPath, _ := systemdExistingInstallation(t, previous)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)

	if _, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable)); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}

	content, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read the unit file: %v", err)
	}
	if got := parseUnit(string(content)).ExecStart; got != newExecutable {
		t.Errorf("the registered executable is %q, want %q", got, newExecutable)
	}

	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 0 {
		t.Errorf("a stopped instance was restarted %d times:\n%s", got, strings.Join(calls, "\n"))
	}
	if got := systemdSupervisorCallsTo(calls, "start", unitName); got != 0 {
		t.Errorf("a stopped instance was started %d times:\n%s", got, strings.Join(calls, "\n"))
	}
}

// TestSystemdFailedRestartRestoresThePreviousRegistration checks that a
// restart the supervisor refuses neither leaves the new registration behind
// nor keeps the instance down: the previous unit file is written back, the
// previous instance is started again and the running state of the restore is
// verified before it is reported.
func TestSystemdFailedRestartRestoresThePreviousRegistration(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)

	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	// The supervisor reports the running state the restore comes back to:
	// the refused restart never reached the verification of the new
	// executable, so the program it reports is the one of the restored
	// registration.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", oldExecutable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_FAIL_ONCE", "1")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_MARKER", filepath.Join(t.TempDir(), "restart"))

	_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err == nil {
		t.Fatal("Install() = nil, want the failure of the restart")
	}
	if !strings.Contains(err.Error(), "the previous registration was restored") {
		t.Errorf("Install() = %q, want an error that reports the restored registration", err)
	}
	// The restart of the restore is verified, so the sentence that names
	// the started instance is allowed here.
	if !strings.Contains(err.Error(), "the previous instance was started again") {
		t.Errorf("Install() = %q, want the verified restart of the previous instance", err)
	}

	content, readErr := os.ReadFile(unitPath)
	if readErr != nil {
		t.Fatalf("read the unit file: %v", readErr)
	}
	if string(content) != previousUnit {
		t.Errorf("the unit file was not restored:\n%s\nwant\n%s", content, previousUnit)
	}

	calls := systemdSupervisorCalls(t, logPath)
	// The failed restart and the restart that brings the previous
	// instance back.
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 2 {
		t.Errorf("the unit was restarted %d times, want the failed restart and the restore:\n%s", got, strings.Join(calls, "\n"))
	}
}

// TestSystemdUnverifiedRestartRestoresThePreviousRegistration checks that a
// restart which does not put the new executable into service is a failed
// installation: a registration alone never reports success, the previous unit
// file is restored and the previous instance is started again, which is
// verified before it is reported.
func TestSystemdUnverifiedRestartRestoresThePreviousRegistration(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)

	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	// The supervisor keeps reporting the old program: the restart
	// succeeds, but the instance never runs the new executable.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", oldExecutable)
	m.restartWait = 250 * time.Millisecond

	_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err == nil {
		t.Fatal("Install() = nil, want the failed verification of the new executable")
	}
	if !strings.Contains(err.Error(), "does not run") {
		t.Errorf("Install() = %q, want an error that names the executable the instance does not run", err)
	}
	if !strings.Contains(err.Error(), "the previous registration was restored") {
		t.Errorf("Install() = %q, want an error that reports the restored registration", err)
	}
	if !strings.Contains(err.Error(), "the previous instance was started again") {
		t.Errorf("Install() = %q, want the verified restart of the previous instance", err)
	}

	content, readErr := os.ReadFile(unitPath)
	if readErr != nil {
		t.Fatalf("read the unit file: %v", readErr)
	}
	if string(content) != previousUnit {
		t.Errorf("the unit file was not restored:\n%s\nwant\n%s", content, previousUnit)
	}

	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 2 {
		t.Errorf("the unit was restarted %d times, want the failed restart and the restore:\n%s", got, strings.Join(calls, "\n"))
	}
}

// TestSystemdActiveUnitThatDoesNotServeIsAFailedUpdate checks that a restarted
// instance counts as verified only when it really serves.
//
// The supervisor reports the unit active with the registered program, which is
// all a Type=simple unit proves: the process was forked. The instance of the
// test does not answer, the way a process that died on a bind error does not, so
// the update is a failure: the previous registration is restored and the
// previous instance is started again, which is verified before it is reported.
func TestSystemdActiveUnitThatDoesNotServeIsAFailedUpdate(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)

	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	// The supervisor reports what a restarted simple unit reports: the unit
	// is active with the program it was started with, and the restart of the
	// restore brings the previous program back.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE_AFTER_RESTART", oldExecutable)
	m.restartWait = 250 * time.Millisecond

	// The instance of the new registration does not answer: the probe fails
	// until the restore restarted the previous instance, and the instance of
	// the restored registration answers.
	restartCount := filepath.Join(t.TempDir(), "restarts")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_COUNT", restartCount)
	serviceReadinessProbe = func(listen string) error {
		count := 0
		if content, err := os.ReadFile(restartCount); err == nil {
			count, _ = strconv.Atoi(strings.TrimSpace(string(content)))
		}
		if count < 2 {
			return errors.New("dial tcp " + probeServiceAddress(listen) + ": connect: connection refused")
		}
		return nil
	}

	_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err == nil {
		t.Fatal("Install() = nil, want the failure of the verification of the instance that does not serve")
	}
	for _, want := range []string{
		"does not answer on",
		previous.Listen,
		"the previous registration was restored",
		"the previous instance was started again",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install() = %q, want an error that mentions %q", err, want)
		}
	}

	content, readErr := os.ReadFile(unitPath)
	if readErr != nil {
		t.Fatalf("read the unit file: %v", readErr)
	}
	if string(content) != previousUnit {
		t.Errorf("the unit file was not restored:\n%s\nwant\n%s", content, previousUnit)
	}

	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 2 {
		t.Errorf("the unit was restarted %d times, want the failed update and the restore:\n%s", got, strings.Join(calls, "\n"))
	}
}

// TestSystemdVerifiedRestartProbesTheListenAddressOfTheUnit checks that the
// readiness probe asks the address the registration names.
//
// The address is read from the unit that was written, not from the request of
// the installation: a reinstall keeps the listen address of the registration,
// and the probe has to reach that instance.
func TestSystemdVerifiedRestartProbesTheListenAddressOfTheUnit(t *testing.T) {
	systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)

	m, _, _ := systemdExistingInstallation(t, previous)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)

	var probed []string
	serviceReadinessProbe = func(listen string) error {
		probed = append(probed, listen)
		return nil
	}

	if _, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable)); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}
	if len(probed) == 0 {
		t.Fatal("the readiness probe was never called for a restarted instance")
	}
	for _, listen := range probed {
		if listen != previous.Listen {
			t.Errorf("the readiness probe asked %q, want the listen address of the registration %q", listen, previous.Listen)
		}
	}
}

// TestSystemdRestoreThatIsNotConfirmedIsNotReportedAsStartedAgain checks that a
// restart of the restored registration which the supervisor accepted but which
// left the unit down is not reported as a started instance: the error says that
// the registration was restored and that the restart was issued, and it never
// claims that the previous instance runs again.
func TestSystemdRestoreThatIsNotConfirmedIsNotReportedAsStartedAgain(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)

	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	// The unit runs before the installation, and the restart of the failure
	// leaves it down: the supervisor accepts every restart, and
	// `systemctl restart` returns as soon as the unit was forked, so its
	// exit status is not a running instance.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", oldExecutable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTARTED", filepath.Join(t.TempDir(), "restarted"))
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE_AFTER_RESTART", "inactive")
	m.restartWait = 250 * time.Millisecond

	_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err == nil {
		t.Fatal("Install() = nil, want the unconfirmed state of the new executable")
	}
	for _, want := range []string{
		"the registration was restored",
		"the restart of the previous instance was issued",
		"but the restart was not confirmed",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install() = %q, want an error that mentions %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "the previous instance was started again") {
		t.Errorf("Install() = %q, want no claim that the previous instance was started again", err)
	}

	content, readErr := os.ReadFile(unitPath)
	if readErr != nil {
		t.Fatalf("read the unit file: %v", readErr)
	}
	if string(content) != previousUnit {
		t.Errorf("the unit file was not restored:\n%s\nwant\n%s", content, previousUnit)
	}

	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 2 {
		t.Errorf("the unit was restarted %d times, want the failed attempt and the restore:\n%s", got, strings.Join(calls, "\n"))
	}
}

// TestSystemdRestoreOfAUnitWithoutAProgramVerifiesTheState checks that a restore
// whose registration names no program verifies the running state alone: there is
// no program to compare, so the rollback reports what the supervisor observed
// and never a program it does not know.
func TestSystemdRestoreOfAUnitWithoutAProgramVerifiesTheState(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	path := filepath.Join(root, "etc", "systemd", "user", unitName)
	// A registration whose command line cannot be read: the section holds
	// no ExecStart, so the restored registration names no program.
	content := []byte("[Unit]\nDescription=HFDesk\n\n[Service]\nWorkingDirectory=" + root + "\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the unit directory: %v", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write the unit file: %v", err)
	}

	m := &systemdManager{scope: ScopeUser}
	m.restartWait = 250 * time.Millisecond
	previous := previousRegistration{path: path, content: content, existed: true, running: true}

	// A supervisor that reports the unit active is enough: the restore
	// reports a started instance without comparing a program.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	if err := m.restorePreviousRegistration(context.Background(), previous); err != nil {
		t.Fatalf("restorePreviousRegistration() = %v, want the verified running state", err)
	}

	// The state is verified and not assumed: a unit the supervisor does not
	// report active is a restore that was not confirmed.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")
	err := m.restorePreviousRegistration(context.Background(), previous)
	if err == nil {
		t.Fatal("restorePreviousRegistration() = nil, want the unconfirmed state of the restored unit")
	}
	if !strings.Contains(err.Error(), "is not active") {
		t.Errorf("restorePreviousRegistration() = %q, want an error that names the state it verified", err)
	}
	if !strings.Contains(err.Error(), "the restart of the previous instance was issued") {
		t.Errorf("restorePreviousRegistration() = %q, want an error that reports the issued restart", err)
	}

	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 2 {
		t.Errorf("the unit was restarted %d times, want the restore of each call:\n%s", got, strings.Join(calls, "\n"))
	}
}

// TestSystemdFreshInstallNeverStartsTheInstance checks the documented
// behaviour of a first installation: the registration is written and enabled,
// and starting stays the explicit, separate command.
func TestSystemdFreshInstallNeverStartsTheInstance(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()

	m := &systemdManager{scope: ScopeUser}
	if _, err := m.Install(context.Background(), systemdTempInstallOptions(t, root)); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}

	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "enable", unitName); got != 1 {
		t.Errorf("the unit was enabled %d times, want once:\n%s", got, strings.Join(calls, "\n"))
	}
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 0 {
		t.Errorf("a first installation restarted the unit %d times:\n%s", got, strings.Join(calls, "\n"))
	}
	if got := systemdSupervisorCallsTo(calls, "start", unitName); got != 0 {
		t.Errorf("a first installation started the unit %d times:\n%s", got, strings.Join(calls, "\n"))
	}
}
