// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/server"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
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

func TestSplitSystemdWordsDecodesDocumentedCEscapes(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{name: "bell", line: `\a`, want: []string{"\a"}},
		{name: "backspace", line: `\b`, want: []string{"\b"}},
		{name: "form-feed", line: `\f`, want: []string{"\f"}},
		{name: "newline", line: `\n`, want: []string{"\n"}},
		{name: "carriage-return", line: `\r`, want: []string{"\r"}},
		{name: "tab", line: `\t`, want: []string{"\t"}},
		{name: "vertical-tab", line: `\v`, want: []string{"\v"}},
		{name: "slash", line: `a\\b`, want: []string{`a\b`}},
		{name: "double-quote", line: `"a\"b"`, want: []string{`a"b`}},
		{name: "single-quote", line: `\'a\'`, want: []string{"'a'"}},
		{name: "systemd-space", line: `a\sb`, want: []string{"a b"}},
		{name: "hex-space", line: `a\x20b`, want: []string{"a b"}},
		{name: "octal-space", line: `a\040b`, want: []string{"a b"}},
		{name: "unicode16-space", line: `a\u0020b`, want: []string{"a b"}},
		{name: "unicode32-space", line: `a\U00000020b`, want: []string{"a b"}},
		{name: "unicode32-last-native-valid-codepoint", line: `a\U0010FFFDb`, want: []string{"a" + string(rune(0x10fffd)) + "b"}},
		{name: "unicode16-noncharacter-remains-valid", line: `a\uFFFEb`, want: []string{"a" + string(rune(0xfffe)) + "b"}},
		{name: "unicode32-noncharacter-remains-literal", line: `a\U0000FFFEb`, want: []string{`a\U0000FFFEb`}},
		{name: "unicode32-maximum-scalar-is-native-invalid", line: `a\U0010FFFFb`, want: []string{`a\U0010FFFFb`}},
		{name: "unicode32-above-scalar-range-remains-literal", line: `a\U00110000b`, want: []string{`a\U00110000b`}},
		{name: "unicode32-maximum-uint32-remains-literal", line: `a\UFFFFFFFFb`, want: []string{`a\UFFFFFFFFb`}},
		{name: "unicode16-non-scalar-remains-literal", line: `a\uD800b`, want: []string{`a\uD800b`}},
		{name: "literal-percent", line: `a%%b`, want: []string{"a%b"}},
		{name: "single-quoted", line: `'a b'`, want: []string{"a b"}},
		{name: "empty-words", line: `"" ''`, want: []string{"", ""}},
		{name: "unknown-escape-is-literal", line: `a\qb`, want: []string{`a\qb`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := splitSystemdWords(test.line); !reflect.DeepEqual(got, test.want) {
				t.Errorf("splitSystemdWords(%q) = %#v, want %#v", test.line, got, test.want)
			}
		})
	}
}

func TestSystemdParseAndReplaceContinuedExecStart(t *testing.T) {
	content := `[Unit]
Description=HFDesk

[Service]
ExecStart=/bin/true \
# ignored comment in continued value
  service run -config=/srv/config\x20one/hfdesk.json -state-dir=/srv/state -log-dir=/srv/log --cache-dir=
WorkingDirectory=/operator/cwd
Environment=HF_HOME=/operator/home
Restart=on-failure
`
	unit := parseUnit(content)
	wantArgs := []string{
		"/bin/true", "service", "run", "-config=/srv/config one/hfdesk.json",
		"-state-dir=/srv/state", "-log-dir=/srv/log", "--cache-dir=",
	}
	if unit.ExecStartRaw != wantArgs[0] || !slices.Equal(append([]string{unit.ExecStartRaw}, unit.ExecArgsRaw...), wantArgs) {
		t.Fatalf("continued ExecStart = %q %#v, want %#v", unit.ExecStartRaw, unit.ExecArgsRaw, wantArgs)
	}
	loaded := systemdLoadedConfig{loaded: true, execPath: wantArgs[0], execArgs: wantArgs}
	if err := loadedMatchesUnit(loaded, unit); err != nil {
		t.Fatalf("loadedMatchesUnit() rejected the native parsed vector: %v", err)
	}

	updated, err := updateSystemdServiceDirectives(content, map[string]systemdDirectiveChange{
		"ExecStart": {value: `"/bin/echo" service run --config=/new`},
	})
	if err != nil {
		t.Fatalf("updateSystemdServiceDirectives() = %v", err)
	}
	if strings.Contains(updated, "ignored comment in continued value") || strings.Contains(updated, "-state-dir=/srv/state") {
		t.Errorf("replacing ExecStart left part of its old continuation behind:\n%s", updated)
	}
	for _, kept := range []string{"WorkingDirectory=/operator/cwd", "Environment=HF_HOME=/operator/home", "Restart=on-failure"} {
		if !strings.Contains(updated, kept) {
			t.Errorf("continued command replacement lost unrelated directive %q:\n%s", kept, updated)
		}
	}
	if got := parseUnit(updated).ExecStart; got != "/bin/echo" {
		t.Errorf("updated executable = %q, want /bin/echo", got)
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
	stateDir := filepath.Join(root, `state with spaces\and%literal"quoted`)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "hfdesk.json"),
		StateDir:   stateDir,
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	t.Setenv("SYSTEMD_LOG_LEVEL", "debug")
	verify := func(stage, content string) string {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, analyze, "verify", "--man=no", "--generators=no", path)
		cmd.Env = os.Environ()
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("systemd-analyze verify (%s) %s = %v\n%s\n%s\nthe unit:\n%s", stage, path, err, stdout.String(), stderr.String(), content)
		}
		for _, line := range strings.Split(stdout.String(), "\n") {
			if i := strings.Index(line, "WorkingDirectory:"); i >= 0 {
				selected := strings.TrimSpace(line[i+len("WorkingDirectory:"):])
				if _, err := os.Stat(selected); err != nil {
					t.Errorf("native selected WorkingDirectory %q is not an existing requested directory: %v", selected, err)
				}
				return selected
			}
		}
		t.Fatalf("systemd-analyze verify (%s) reported no selected WorkingDirectory\n%s\n%s", stage, stdout.String(), stderr.String())
		return ""
	}
	if got := parseUnit(unit).WorkingDirectory; got != stateDir {
		t.Errorf("first-install WorkingDirectory = %q, want literal path %q", got, stateDir)
	}
	if got := verify("first install", unit); got != stateDir {
		t.Errorf("native first-install WorkingDirectory = %q, want requested path %q", got, stateDir)
	}
	control := "[Service]\nExecStart=/bin/true\nWorkingDirectory=" + strings.ReplaceAll(stateDir, "%", "%%") + "\n"
	if got := verify("raw native-value control", control); got != stateDir {
		t.Fatalf("native raw-value control selected %q, want %q", got, stateDir)
	}

	explicitStateDir := filepath.Join(root, `updated state with spaces\and%literal"quoted`)
	if err := os.MkdirAll(explicitStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	updated := opts
	updated.Paths.StateDir = explicitStateDir
	updated.Explicit.StateDir = true
	unit, err = updateUnit(unit, updated, nil)
	if err != nil {
		t.Fatalf("updateUnit() = %v, want nil", err)
	}
	if got := parseUnit(unit).WorkingDirectory; got != explicitStateDir {
		t.Errorf("explicit StateDir update WorkingDirectory = %q, want literal path %q", got, explicitStateDir)
	}
	if got := verify("explicit StateDir update", unit); got != explicitStateDir {
		t.Errorf("native explicit-update WorkingDirectory = %q, want requested path %q", got, explicitStateDir)
	}

	adminWorkingDirectory := "WorkingDirectory=%h"
	workingDirectoryValue, err := systemdWorkingDirectoryValue(explicitStateDir)
	if err != nil {
		t.Fatal(err)
	}
	previousWorkingDirectory := "WorkingDirectory=" + workingDirectoryValue
	if !strings.Contains(unit, previousWorkingDirectory) {
		t.Fatalf("explicit update lost its WorkingDirectory directive:\n%s", unit)
	}
	unit = strings.Replace(unit, previousWorkingDirectory, adminWorkingDirectory, 1)
	execOnly := updated
	execOnly.Executable = "/bin/true"
	execOnly.Explicit.StateDir = false
	unit, err = updateUnit(unit, execOnly, nil)
	if err != nil {
		t.Fatalf("executable-only updateUnit() = %v, want nil", err)
	}
	if !strings.Contains(unit, adminWorkingDirectory) {
		t.Fatalf("executable-only update changed the administrator's WorkingDirectory source:\n%s", unit)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got := verify("executable-only update preserving admin WorkingDirectory", unit); got != home {
		t.Errorf("native executable-only WorkingDirectory = %q, want the native %%h path %q", got, home)
	}

	trailingStateDir := filepath.Join(root, "state with trailing backslash\\")
	if err := os.MkdirAll(trailingStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	trailingOpts := opts
	trailingOpts.Paths.StateDir = trailingStateDir
	trailingUnit, err := renderUnit(trailingOpts, nil)
	if err != nil {
		t.Fatalf("renderUnit() for terminal backslash = %v", err)
	}
	if got := verify("first install with terminal backslash", trailingUnit); got != trailingStateDir {
		t.Errorf("native terminal-backslash WorkingDirectory = %q, want %q", got, trailingStateDir)
	}
	updatedTrailingStateDir := filepath.Join(root, "updated state with trailing backslash\\")
	if err := os.MkdirAll(updatedTrailingStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	trailingOpts.Paths.StateDir = updatedTrailingStateDir
	trailingOpts.Explicit.StateDir = true
	trailingUnit, err = updateUnit(trailingUnit, trailingOpts, nil)
	if err != nil {
		t.Fatalf("updateUnit() for terminal backslash = %v", err)
	}
	if got := verify("explicit StateDir update with terminal backslash", trailingUnit); got != updatedTrailingStateDir {
		t.Errorf("native updated terminal-backslash WorkingDirectory = %q, want %q", got, updatedTrailingStateDir)
	}

	pairedBackslashes := filepath.Join(root, "state with paired trailing backslashes\\\\")
	if err := os.MkdirAll(pairedBackslashes, 0o700); err != nil {
		t.Fatal(err)
	}
	pairedOpts := opts
	pairedOpts.Paths.StateDir = pairedBackslashes
	pairedUnit, err := renderUnit(pairedOpts, nil)
	if err != nil {
		t.Fatalf("renderUnit() for paired terminal backslashes = %v", err)
	}
	if got := verify("first install with paired terminal backslashes", pairedUnit); got != pairedBackslashes {
		t.Errorf("native paired-backslash WorkingDirectory = %q, want %q", got, pairedBackslashes)
	}
}

func TestSystemdWorkingDirectoryRejectsUnrepresentableTrailingSpace(t *testing.T) {
	opts := systemdTestOptions(t, ScopeUser)
	opts.Paths.StateDir = filepath.Join(t.TempDir(), "state ")
	if err := validateUnitPaths(opts, false); err == nil || !strings.Contains(err.Error(), "trailing spaces are stripped") {
		t.Fatalf("first-install validateUnitPaths() = %v, want refusal of an unrepresentable WorkingDirectory", err)
	}
	if err := validateUnitPaths(opts, true); err != nil {
		t.Fatalf("executable-only validation rejected the inherited app state path: %v", err)
	}
	if _, err := renderUnit(opts, nil); err == nil || !strings.Contains(err.Error(), "trailing spaces are stripped") {
		t.Fatalf("renderUnit() = %v, want refusal of an unrepresentable WorkingDirectory", err)
	}
	opts.Explicit.StateDir = true
	if err := validateUnitPaths(opts, true); err == nil || !strings.Contains(err.Error(), "trailing spaces are stripped") {
		t.Fatalf("explicit StateDir validation = %v, want refusal of an unrepresentable WorkingDirectory", err)
	}
	unit := "[Service]\nExecStart=/bin/true service run --config /srv/config.json --state-dir /srv/state --log-dir /srv/log\nWorkingDirectory=/srv/state\n"
	if _, err := updateUnit(unit, opts, nil); err == nil || !strings.Contains(err.Error(), "trailing spaces are stripped") {
		t.Fatalf("updateUnit() = %v, want refusal of an unrepresentable WorkingDirectory", err)
	}
}

func TestSystemdRunFlagEditorFollowsFlagSetSemantics(t *testing.T) {
	source := "/bin/true service run -config=/srv/config.json --state-dir /srv/state -log-dir=/srv/logs -listen=127.0.0.1:18080 --listen 127.0.0.1:18081 --cache-dir=/srv/cache --"
	words := splitSystemdWordTokens(source)
	updated := replaceSystemdRunFlag(words, "listen", "127.0.0.1:19222", true)
	updatedSource := make([]string, len(updated))
	for i, word := range updated {
		updatedSource[i] = word.source
	}
	gotSource := strings.Join(updatedSource, " ")
	wantSource := "/bin/true service run -config=/srv/config.json --state-dir /srv/state -log-dir=/srv/logs --listen 127.0.0.1:19222 --cache-dir=/srv/cache --"
	if gotSource != wantSource {
		t.Fatalf("source update = %q, want %q", gotSource, wantSource)
	}
	before, err := ParseRunArgs(parseUnit("[Service]\nExecStart=" + source).ExecArgs)
	if err != nil {
		t.Fatalf("baseline ParseRunArgs() = %v", err)
	}
	after, err := ParseRunArgs(parseUnit("[Service]\nExecStart=" + gotSource).ExecArgs)
	if err != nil {
		t.Fatalf("updated ParseRunArgs() = %v", err)
	}
	before.Listen = after.Listen
	if after.Listen != "127.0.0.1:19222" || !reflect.DeepEqual(after, before) {
		t.Errorf("updated parsed options = %+v, want only Listen changed from %+v", after, before)
	}
}

func TestSystemdRunFlagEditorTreatsOptionValueAsDistinctFromTerminator(t *testing.T) {
	source := "/bin/true service run --config -- --state-dir /srv/state --log-dir /srv/log --"
	words := splitSystemdWordTokens(source)
	layout := scanSystemdRunArgs(words, true)
	if layout.terminator != len(words)-1 {
		t.Fatalf("terminator index = %d, want final standalone -- at %d", layout.terminator, len(words)-1)
	}
	if len(layout.options) != 3 || layout.options[0].name != "config" || words[layout.options[0].end-1].value != "--" {
		t.Fatalf("option layout = %+v, want config to consume the first -- as its value", layout)
	}
	updated := replaceSystemdRunFlag(words, "listen", "127.0.0.1:19222", true)
	var sourceWords []string
	for _, word := range updated {
		sourceWords = append(sourceWords, word.source)
	}
	got := strings.Join(sourceWords, " ")
	want := "/bin/true service run --config -- --state-dir /srv/state --log-dir /srv/log --listen 127.0.0.1:19222 --"
	if got != want {
		t.Fatalf("updated source = %q, want %q", got, want)
	}
	_, err := ParseRunArgs(parseUnit("[Service]\nExecStart=" + got).ExecArgs)
	if err == nil || !strings.Contains(err.Error(), "absolute path") || strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("ParseRunArgs() error = %v, want the consumed -- value to fail path validation, not terminate parsing", err)
	}
}

func TestSystemdInstallInsertsExplicitOptionBeforeTerminator(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	config := filepath.Join(root, "config.json")
	state := filepath.Join(root, "state")
	logs := filepath.Join(root, "logs")
	content := "[Service]\nExecStart=/bin/true service run --config " + config + " --state-dir " + state + " --log-dir " + logs + " --\nWorkingDirectory=" + root + "\n[Install]\nWantedBy=default.target\n"
	baseline, err := ParseRunArgs(parseUnit(content).ExecArgs)
	if err != nil {
		t.Fatalf("baseline ParseRunArgs() = %v", err)
	}

	manager := &systemdManager{scope: ScopeUser}
	path, err := manager.unitPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	setSystemdTestPropertyReader(t, manager)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

	requested := DefaultInstallOptions(ScopeUser, "/bin/true")
	requested.Listen = "127.0.0.1:19222"
	requested.Explicit.Listen = true
	installed, err := manager.Install(context.Background(), requested)
	if err != nil {
		t.Fatalf("Install() = %v", err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updatedArgs := parseUnit(string(stored)).ExecArgs
	parsed, err := ParseRunArgs(updatedArgs)
	if err != nil {
		t.Fatalf("successful Install() wrote an invalid ParseRunArgs vector: %v\n%s", err, stored)
	}
	baseline.Listen = requested.Listen
	if !reflect.DeepEqual(parsed, baseline) || installed.Listen != requested.Listen {
		t.Errorf("installed options = %+v / returned listen %q, want %+v", parsed, installed.Listen, baseline)
	}
	if !strings.Contains(string(stored), "--log-dir "+logs+" --listen 127.0.0.1:19222 --\n") {
		t.Errorf("explicit flag was not inserted before the true terminator:\n%s", stored)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 0 {
		t.Errorf("stopped service was restarted %d times", got)
	}
}

func TestSystemdExecutableUpdatePreservesInheritedStatePathAndIndependentWorkingDirectory(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	state := filepath.Join(root, "state ")
	config := filepath.Join(root, "config.json")
	logs := filepath.Join(root, "logs")
	workingDirectory := filepath.Join(root, "native-working-directory")
	for _, dir := range []string{state, filepath.Dir(config), logs, workingDirectory} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dataPath := filepath.Join(state, "jobs_state.json")
	data := []byte(`{"jobs":[{"id":"preserved-state"}]}`)
	if err := os.WriteFile(dataPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	content := "[Service]\nExecStart=/bin/true service run --config \"" + config + "\" --state-dir \"" + state + "\" --log-dir \"" + logs + "\"\n" +
		"WorkingDirectory=" + workingDirectory + "\nRestart=on-failure\n[Install]\nWantedBy=default.target\n"
	baseline, err := ParseRunArgs(parseUnit(content).ExecArgs)
	if err != nil || baseline.Paths.StateDir != state {
		t.Fatalf("baseline app argv = %+v, %v; want trailing-space state %q", baseline, err, state)
	}
	if _, err := os.Stat(filepath.Join(root, "state")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("trimmed state-path decoy exists before update: %v", err)
	}

	manager := &systemdManager{scope: ScopeUser}
	unitPath, err := manager.unitPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	setSystemdTestPropertyReader(t, manager)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

	installed, err := manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, "/bin/echo"))
	if err != nil {
		t.Fatalf("executable-only Install() rejected the inherited state path: %v", err)
	}
	if installed.Paths.StateDir != state {
		t.Fatalf("Install() state dir = %q, want the unchanged app path %q", installed.Paths.StateDir, state)
	}
	stored, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := parseUnit(string(stored))
	parsed, err := ParseRunArgs(updated.ExecArgs)
	if err != nil || parsed.Paths.StateDir != state {
		t.Fatalf("updated app argv = %+v, %v; want unchanged state %q", parsed, err, state)
	}
	if updated.WorkingDirectory != workingDirectory || !strings.Contains(string(stored), "WorkingDirectory="+workingDirectory+"\n") {
		t.Fatalf("executable-only update changed independent WorkingDirectory %q:\n%s", workingDirectory, stored)
	}
	if after, err := os.ReadFile(dataPath); err != nil || !bytes.Equal(after, data) {
		t.Errorf("state data changed: %q, %v", after, err)
	}
	if _, err := os.Stat(filepath.Join(root, "state")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("update created the trimmed state-path decoy: %v", err)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 0 {
		t.Errorf("stopped unit was restarted %d times", got)
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

// TestSystemdStatusKeepsTheRunningFactOfABrokenRegistration checks that the
// status of a registration whose program is gone keeps the running fact of the
// supervisor next to the broken diagnosis.
//
// systemd keeps the process it started, so deleting the program stops the next
// start and not the instance. The state therefore stays broken, and the detail
// says whether the instance still runs, so a caller that has to know whether
// anything serves does not read "broken" as "nothing runs".
func TestSystemdStatusKeepsTheRunningFactOfABrokenRegistration(t *testing.T) {
	cases := []struct {
		name string
		// active is the state the supervisor reports.
		active string
		// wantRunning is the running fact of that state.
		wantRunning bool
	}{
		{"the instance still runs", "active", true},
		{"the instance does not run", "inactive", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logPath := systemdRecordingSupervisor(t)
			missing := filepath.Join(t.TempDir(), "bin", "hfdesk")
			opts := systemdExistingOptions(t, missing, t.TempDir())
			m, _, _ := systemdExistingInstallation(t, opts)
			t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", tc.active)

			status, err := m.Status(context.Background())
			if err != nil {
				t.Fatalf("Status() = %v, want nil", err)
			}
			if status.State != StateBroken {
				t.Errorf("the state = %q, want the diagnosis %q of the registration", status.State, StateBroken)
			}
			if status.Running != tc.wantRunning {
				t.Errorf("the running fact = %v, want %v", status.Running, tc.wantRunning)
			}
			if !strings.Contains(status.Detail, missing) {
				t.Errorf("the detail = %q, want it to name the missing program %q", status.Detail, missing)
			}
			if got := strings.Contains(status.Detail, "the instance still runs"); got != tc.wantRunning {
				t.Errorf("the detail = %q, want the running fact %v to be described", status.Detail, tc.wantRunning)
			}

			// The running fact comes from the supervisor, so a broken
			// registration is asked about as well.
			shows := 0
			for _, call := range systemdSupervisorCalls(t, logPath) {
				if strings.HasPrefix(strings.TrimPrefix(call, "--user "), "show ") {
					shows++
				}
			}
			if shows != 1 {
				t.Errorf("the supervisor was asked for the state %d times, want once", shows)
			}
		})
	}
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

func TestSystemdPreflightProjectsNativeGroupAndSupplementaryGroupAccess(t *testing.T) {
	systemdRecordingSupervisor(t)
	target := systemdSecondAccount(t)
	targetIdentity, err := linuxExecutableAccessIdentity(target)
	if err != nil {
		t.Skipf("could not resolve target account groups: %v", err)
	}
	targetGroups := make(map[int]struct{}, len(targetIdentity.Groups))
	for _, group := range targetIdentity.Groups {
		targetGroups[group] = struct{}{}
	}
	var extraGroup string
	for _, group := range currentExecutableAccessIdentity().Groups {
		if _, exists := targetGroups[group]; !exists {
			extraGroup = strconv.Itoa(group)
			break
		}
	}
	if extraGroup == "" {
		t.Skip("the test process has no group outside the target account's group database")
	}
	groupID, err := strconv.Atoi(extraGroup)
	if err != nil {
		t.Fatal(err)
	}

	root := systemdTraversableTempDir(t)
	candidate := filepath.Join(root, "candidate")
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(running)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(candidate, 0o010); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(candidate)
	if err != nil {
		t.Fatal(err)
	}
	localDir := filepath.Join(root, "group-only-local")
	if err := os.Mkdir(localDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(localDir, 0o770); err != nil {
		t.Fatal(err)
	}
	opts := systemdTempInstallOptions(t, root)
	opts.Scope = ScopeSystem
	opts.Executable = candidate
	opts.LocalDir = localDir

	type accessCase struct {
		name       string
		group      string
		supplement []string
		allow      bool
	}
	cases := []accessCase{
		{"primary Group grants execution", extraGroup, nil, true},
		{"wrong primary Group denies execution", target.Gid, nil, false},
		{"SupplementaryGroups grants execution", target.Gid, []string{extraGroup}, true},
		{"missing SupplementaryGroups denies execution", target.Gid, nil, false},
	}
	if group, err := user.LookupGroupId(extraGroup); err == nil && group.Name != "" && group.Name != extraGroup {
		cases = append(cases,
			accessCase{"named primary Group grants execution", group.Name, nil, true},
			accessCase{"named SupplementaryGroups grants execution", target.Gid, []string{group.Name}, true},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			properties := systemdTypedProperties(candidate, []string{candidate}, nil, root, target.Username, tc.group, nil)
			properties["SupplementaryGroups"] = dbus.MakeVariant(tc.supplement)
			loaded, err := loadedConfigFromProperties(properties)
			if err != nil {
				t.Fatalf("loadedConfigFromProperties() = %v", err)
			}
			effective := systemdAccessIdentityForInstall(ScopeSystem, InstallOptions{Scope: ScopeSystem}, true, loaded, true, nil)
			identity := effective.access
			if identity == nil || effective.accountName != target.Username || effective.owner == nil {
				t.Fatalf("systemdAccessIdentityForInstall() = %+v, want the loaded target identity and owner", effective)
			}
			gotAccess := effectivePermission(info, *identity)&0o100 != 0
			if gotAccess != tc.allow {
				t.Errorf("loaded identity %+v grants execute permission = %t, want %t", identity, gotAccess, tc.allow)
			}
			dirErr := (&systemdManager{scope: ScopeSystem}).checkServiceAccountAccessWithIdentity(opts, effective.accountName, identity)
			if tc.allow && dirErr != nil {
				t.Errorf("loaded native groups rejected the existing group-writable LocalDir: %v", dirErr)
			}
			if !tc.allow && dirErr == nil {
				t.Error("existing LocalDir passed without its owning native group")
			}
			if tc.group == extraGroup && identity.GID != groupID {
				t.Errorf("loaded Group primary GID = %d, want %d", identity.GID, groupID)
			}
			if slices.Contains(tc.supplement, extraGroup) && !slices.Contains(identity.Groups, groupID) {
				t.Errorf("loaded supplementary groups %v omit configured GID %d", identity.Groups, groupID)
			}
			err = (&systemdManager{scope: ScopeSystem}).preflightWithContext(context.Background(), opts, effective.accountName, identity, root)
			if os.Geteuid() == 0 {
				if tc.allow && err != nil {
					t.Errorf("preflight rejected native group access: %v", err)
				}
				if !tc.allow && err == nil {
					t.Error("preflight accepted an executable outside the loaded native groups")
				}
			} else if err != nil {
				t.Errorf("preflight turned unavailable target-credential evidence into a denial: %v", err)
			}
		})
	}
	t.Run("explicit ServiceUser overrides loaded owner", func(t *testing.T) {
		properties := systemdTypedProperties(candidate, []string{candidate}, nil, root, target.Username, extraGroup, nil)
		properties["SupplementaryGroups"] = dbus.MakeVariant([]string{extraGroup})
		loaded, err := loadedConfigFromProperties(properties)
		if err != nil {
			t.Fatal(err)
		}
		requested := InstallOptions{Scope: ScopeSystem}
		requested.Explicit.ServiceUser = true
		requestedAccount := systemdCurrentAccount(t)
		effective := systemdAccessIdentityForInstall(ScopeSystem, requested, true, loaded, true, requestedAccount)
		wantUID, _ := strconv.Atoi(requestedAccount.Uid)
		wantGID, _ := strconv.Atoi(requestedAccount.Gid)
		if effective.owner == nil || effective.owner.UID != wantUID || effective.owner.GID != wantGID {
			t.Errorf("explicit ServiceUser owner = %+v, want requested UID/GID %d:%d", effective.owner, wantUID, wantGID)
		}
	})

	t.Run("unestablished identity stays inconclusive", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			loaded      systemdLoadedConfig
			loadedKnown bool
			ownerKnown  bool
		}{
			{"unreadable manager context", systemdLoadedConfig{}, false, false},
			{"unit exists on disk but is unloaded", systemdLoadedConfig{loaded: false}, true, false},
			{"loaded UID/GID without a complete group list", systemdLoadedConfig{loaded: true, user: target.Username, group: target.Gid}, true, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				effective := systemdAccessIdentityForInstall(ScopeSystem, InstallOptions{Scope: ScopeSystem}, true, tc.loaded, tc.loadedKnown, nil)
				if effective.access != nil || (effective.owner != nil) != tc.ownerKnown {
					t.Fatalf("unavailable loaded identity = %+v, want access unknown and owner known=%t", effective, tc.ownerKnown)
				}
				if cwd := systemdWorkingDirectoryForPreflight(opts, true, tc.loaded, tc.loadedKnown); cwd != "" {
					t.Fatalf("unavailable native WorkingDirectory = %q, want unknown", cwd)
				}
				if err := (&systemdManager{scope: ScopeSystem}).preflightWithContext(context.Background(), opts, effective.accountName, effective.access, ""); err != nil {
					t.Errorf("preflight() = %v, want unknown native context to remain inconclusive", err)
				}
				if effective.owner == nil {
					newDir := filepath.Join(t.TempDir(), "new-state")
					created := map[string][]string{newDir: {newDir}}
					err := (&systemdManager{scope: ScopeSystem}).grantInstanceDirsForDirsWithIdentity(opts, effective, created, []instanceDir{{name: "state dir", path: newDir}})
					if err == nil || !strings.Contains(err.Error(), "effective systemd UID/GID is not established") {
						t.Errorf("ownership preparation error = %v, want a concrete unknown-native-owner blocker", err)
					}
				}
			})
		}
	})
}

func TestSystemdGrantInstanceDirsUsesLoadedNativeUserAndGroup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to chown temporary instance directories and verify the actual effective UID/GID; ownership is not simulated")
	}
	base, loadedUser := systemdCurrentAccount(t), systemdSecondAccount(t)
	loadedUID, err := strconv.Atoi(loadedUser.Uid)
	if err != nil {
		t.Fatal(err)
	}
	loadedGID, err := strconv.Atoi(loadedUser.Gid)
	if err != nil {
		t.Fatal(err)
	}
	root := systemdTraversableTempDir(t)
	configDir := filepath.Join(root, "config")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(configDir, loadedUID, loadedGID); err != nil {
		t.Fatal(err)
	}
	beforeUID, beforeGID := systemdOwner(t, configDir)
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(configDir, "hfdesk.json"),
			StateDir:   filepath.Join(root, "state"),
			LogDir:     filepath.Join(root, "logs"),
		},
	}
	properties := systemdTypedProperties(opts.Executable, []string{opts.Executable}, nil, root, loadedUser.Username, loadedUser.Gid, nil)
	loaded, err := loadedConfigFromProperties(properties)
	if err != nil {
		t.Fatal(err)
	}
	effective := systemdAccessIdentityForInstall(ScopeSystem, InstallOptions{Scope: ScopeSystem}, true, loaded, true, base)
	if effective.access == nil || effective.accountName != loadedUser.Username || effective.owner == nil {
		t.Fatalf("projected identity = %+v, want the loaded user rather than the base account", effective)
	}
	baseUID, _ := strconv.Atoi(base.Uid)
	if effective.owner.UID == baseUID || effective.owner.UID != loadedUID || effective.owner.GID != loadedGID {
		t.Fatalf("projected owner = %+v, want loaded User/Group %d:%d rather than request account %q", effective.owner, loadedUID, loadedGID, base.Username)
	}
	dirs := systemdInstanceDirs(opts, nil)
	created := make(map[string][]string, len(dirs))
	for _, dir := range dirs {
		created[dir.path] = createdDirs(dir.path)
	}
	if err := prepareSystemdInstanceDirs(opts, dirs, nil); err != nil {
		t.Fatal(err)
	}
	manager := &systemdManager{scope: ScopeSystem}
	if err := manager.grantInstanceDirsForDirsWithIdentity(opts, effective, created, dirs); err != nil {
		t.Fatalf("grantInstanceDirsForDirsWithIdentity() = %v", err)
	}
	for _, path := range []string{opts.Paths.StateDir, opts.Paths.LogDir} {
		uid, gid := systemdOwner(t, path)
		if uid != loadedUser.Uid || gid != loadedUser.Gid {
			t.Errorf("new instance directory %q owner = %s:%s, want loaded User/Group %s:%s", path, uid, gid, loadedUser.Uid, loadedUser.Gid)
		}
	}
	if uid, gid := systemdOwner(t, configDir); uid != beforeUID || gid != beforeGID {
		t.Errorf("pre-existing config directory owner changed to %s:%s; want unchanged %s:%s", uid, gid, beforeUID, beforeGID)
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
if [ "$1" = "--user" ]; then
	shift
fi
if [ -n "$HFDESK_TEST_MARKER_DIR" ]; then
	mkdir -p "$HFDESK_TEST_MARKER_DIR" || exit 1
	: > "$HFDESK_TEST_MARKER_DIR/marker" || exit 1
fi
if [ "$1" = "show" ]; then
	unit="${XDG_CONFIG_HOME}/systemd/user/hfdesk.service"
	if [ ! -e "$unit" ]; then
			echo "LoadState=not-found"
			exit 0
		fi
		echo "LoadState=loaded"
		exit 0
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

// TestSystemdFailedInstallRemovesTheDirectoriesItCreated checks that created
// paths are removed after rollback verifies the first registration was removed.
func TestSystemdFailedInstallRemovesTheDirectoriesItCreated(t *testing.T) {
	systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	// The first reload fails, but the rollback reload succeeds and proves the
	// failed first registration was removed from the supervisor.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_DAEMON_RELOAD_FAIL_AT", "1")

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
	// The rollback did not prove whether the still-loaded unit references this
	// directory, so the newly created path is retained.
	created := filepath.Join(config, "hfdesk")
	if info, err := os.Stat(created); err != nil || !info.IsDir() {
		t.Errorf("the directory %q was removed despite unconfirmed rollback: %v", created, err)
	}
}

// TestSystemdFailedInstallKeepsACreatedDirectoryThatHoldsData checks that an
// unverified rollback retains every created path, not only the nonempty one.
func TestSystemdFailedInstallKeepsACreatedDirectoryThatHoldsData(t *testing.T) {
	systemdFakeSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()

	opts := systemdTempInstallOptions(t, root)
	state := opts.Paths.StateDir
	m := &systemdManager{scope: ScopeUser}

	// The fake supervisor writes a marker into the state directory, so the
	// directory holds data when the installation fails.
	t.Setenv("HFDESK_TEST_MARKER_DIR", state)
	_, err := m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the failure of the supervisor")
	}
	if !strings.Contains(err.Error(), "the fake supervisor refuses the command") {
		t.Errorf("Install() = %q, want the original fake-supervisor failure", err)
	}
	if !strings.Contains(err.Error(), "the previous unit file or captured loaded settings could not be restored or verified") {
		t.Errorf("Install() = %q, want the unconfirmed rollback outcome", err)
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
	// Rollback could not establish whether the retained registration refers to
	// these directories. Empty paths are kept on that uncertainty too.
	for _, dir := range []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var", "log"),
		filepath.Join(root, "var", "log", "hfdesk"),
	} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("the directory %q was removed despite unconfirmed rollback: %v", dir, err)
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

func TestSystemdPreparationFailureRetainsPathsReferencedByPriorUnit(t *testing.T) {
	systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	manager, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	blockingFile := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockingFile, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	requested := DefaultInstallOptions(ScopeUser, newExecutable)
	requested.Paths.LogDir = filepath.Join(blockingFile, "logs")
	requested.Explicit.LogDir = true
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

	_, err := manager.Install(context.Background(), requested)
	if err == nil || !strings.Contains(err.Error(), "log dir") {
		t.Fatalf("Install() = %v, want directory-preparation failure", err)
	}
	stored, readErr := os.ReadFile(unitPath)
	if readErr != nil || string(stored) != previousUnit {
		t.Fatalf("the no-write failure changed the previous registration (read error %v)", readErr)
	}
	for _, dir := range []string{filepath.Join(root, "etc", "hfdesk"), previous.Paths.StateDir} {
		if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
			t.Errorf("created path %q was removed despite the retained unit referring to it: %v", dir, statErr)
		}
	}
}

func TestSystemdVerifiedRollbackRetainsPathsReferencedByPreviousNativeDirective(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	manager, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	candidateState := filepath.Join(root, "candidate-state")
	previousUnit = strings.Replace(previousUnit, "\n\n[Install]", "\nExecStartPre=/usr/bin/test -d "+candidateState+"\n\n[Install]", 1)
	for _, path := range []string{unitPath, os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")} {
		if err := os.WriteFile(path, []byte(previousUnit), 0o644); err != nil {
			t.Fatalf("write previous native unit %q: %v", path, err)
		}
	}
	for _, dir := range instanceDirs(previous) {
		if err := os.MkdirAll(dir.path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ENABLE_FAIL", "1")

	requested := DefaultInstallOptions(ScopeUser, newExecutable)
	requested.Paths.StateDir = candidateState
	requested.Explicit.StateDir = true
	if _, err := manager.Install(context.Background(), requested); err == nil {
		t.Fatal("Install() = nil, want the enable failure and verified rollback")
	}
	if info, err := os.Stat(candidateState); err != nil || !info.IsDir() {
		t.Errorf("the previous native ExecStartPre reference %q was removed after rollback: %v", candidateState, err)
	}
	for _, path := range []string{unitPath, os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")} {
		content, err := os.ReadFile(path)
		if err != nil || string(content) != previousUnit {
			t.Errorf("rollback path %q (read error %v) = %q, want the prior unit", path, err, content)
		}
	}
	if calls := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); calls != 0 {
		t.Errorf("stopped previous unit was restarted %d times", calls)
	}
}

func TestSystemdDirectoryCleanupRequiresKnownEmptyDropIns(t *testing.T) {
	candidate := filepath.Join(t.TempDir(), "candidate-state")
	previous := previousRegistration{
		existed:     true,
		loadedKnown: true,
		content: []byte("[Service]\nExecStart=/bin/true service run --config /tmp/config --state-dir /tmp/state --log-dir /tmp/log\n" +
			"WorkingDirectory=/tmp/state\n"),
	}
	created := map[string][]string{candidate: {candidate}}

	for _, tc := range []struct {
		name    string
		dropIns []string
		known   bool
	}{
		{name: "unavailable property"},
		{name: "loaded drop-in", dropIns: []string{"/etc/systemd/system/hfdesk.service.d/override.conf"}, known: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			properties := systemdTypedProperties("/bin/true", []string{"/bin/true", "service", "run"}, nil, "/tmp/state", "", "", nil)
			if tc.known {
				properties["DropInPaths"] = dbus.MakeVariant(tc.dropIns)
			} else {
				delete(properties, "DropInPaths")
			}
			loaded, err := loadedConfigFromProperties(properties)
			if err != nil {
				t.Fatal(err)
			}
			previous.loaded = loaded
			if systemdCreatedDirsSafeToRemove(previous, created) {
				t.Fatal("cleanup was allowed without proof that the previous loaded unit has no drop-ins")
			}
		})
	}
}

func TestSystemdInstallUsesLoadedVendorFragmentAndRetainsItsPaths(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	previous := systemdExistingOptions(t, systemdTestExecutable(t), root)
	vendorUnit, err := renderUnit(previous, nil)
	if err != nil {
		t.Fatal(err)
	}
	vendorPath := filepath.Join(root, "lib", "systemd", "user", unitName)
	if err := os.MkdirAll(filepath.Dir(vendorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vendorPath, []byte(vendorUnit), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &systemdManager{scope: ScopeUser}
	unitPath, err := manager.unitPath()
	if err != nil {
		t.Fatal(err)
	}
	loadedPath := os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")
	if err := os.WriteFile(loadedPath, []byte(vendorUnit), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.readServiceProperties = func(_ context.Context, name string) (map[string]dbus.Variant, error) {
		if name != unitName {
			return nil, fmt.Errorf("unexpected unit %q", name)
		}
		content := vendorUnit
		if loaded, err := os.ReadFile(loadedPath); err == nil {
			content = string(loaded)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		fragment := vendorPath
		if _, err := os.Stat(unitPath); err == nil {
			fragment = unitPath
		}
		properties := systemdTestPropertiesFromUnit(content)
		properties["FragmentPath"] = dbus.MakeVariant(fragment)
		return properties, nil
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", previous.Executable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_DAEMON_RELOAD_FAIL_AT", "1")

	if _, err := manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, systemdTestExecutable(t))); err == nil {
		t.Fatal("Install() = nil, want the first reload failure and vendor-fragment rollback")
	}
	if _, err := os.Stat(unitPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the failed override remains at %q (stat error: %v)", unitPath, err)
	}
	if info, err := os.Stat(previous.Paths.StateDir); err != nil || !info.IsDir() {
		t.Errorf("the restored vendor fragment's state path %q was removed: %v", previous.Paths.StateDir, err)
	}
	if calls := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); calls != 0 {
		t.Errorf("stopped vendor registration was restarted %d times", calls)
	}
}

func TestSystemdInstallDoesNotTreatUnknownLoadedFragmentAsFirstInstall(t *testing.T) {
	systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	opts := systemdTempInstallOptions(t, root)
	manager := &systemdManager{scope: ScopeUser}
	unitPath, err := manager.unitPath()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", opts.Executable)
	manager.readServiceProperties = func(context.Context, string) (map[string]dbus.Variant, error) {
		properties := systemdTypedProperties(opts.Executable, RunArgs(opts), nil, opts.Paths.StateDir, "", "", nil)
		delete(properties, "FragmentPath")
		return properties, nil
	}

	if _, err := manager.Install(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "no readable source fragment") {
		t.Fatalf("Install() = %v, want a concrete loaded-source blocker", err)
	}
	if _, err := os.Stat(unitPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unknown loaded source was replaced at %q: %v", unitPath, err)
	}
	for _, dir := range []string{filepath.Dir(opts.Paths.ConfigFile), opts.Paths.StateDir, opts.Paths.LogDir} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("directory %q was created before the loaded fragment could be established: %v", dir, err)
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

	merged := mergeInstallOptions(requested, existing, true)

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

	merged := mergeInstallOptions(requested, existing, true)

	if !reflect.DeepEqual(merged, requested) {
		t.Errorf("mergeInstallOptions() =\n%+v\nwant the request\n%+v", merged, requested)
	}
}

// TestMergeInstallOptionsUsesTheDefaultsOnAFirstInstall checks that an
// installation without a registration installs exactly what the request
// says: every unmarked field falls back to the default of the request.
func TestMergeInstallOptionsUsesTheDefaultsOnAFirstInstall(t *testing.T) {
	requested := installOptionsForMerge("/default", "/new/bin/hfdesk")

	merged := mergeInstallOptions(requested, InstallOptions{}, false)

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

	merged := mergeInstallOptions(requested, existing, true)

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

func TestSystemdRegistrationOptionsUsesAcceptedFlagSyntaxAndLastValue(t *testing.T) {
	unit := `[Service]
ExecStart=/opt/hfdesk/hfdesk service run -config=/old/hfdesk.json --config=/new/hfdesk.json -state-dir /old/state --state-dir=/new/state -log-dir=/new/logs -listen=127.0.0.1:8181 --listen 127.0.0.1:9191 -cache-dir=/old/cache --cache-dir=/new/cache -hub-dir=/old/hub --hub-dir=/new/hub -local-dir=/old/models --local-dir=/new/models
`
	got := registrationOptions(parseUnit(unit), ScopeUser)
	want := InstallOptions{
		Scope:      ScopeUser,
		Executable: "/opt/hfdesk/hfdesk",
		Listen:     "127.0.0.1:9191",
		Paths: runtimepaths.Paths{
			ConfigFile: "/new/hfdesk.json",
			StateDir:   "/new/state",
			LogDir:     "/new/logs",
		},
		CacheDir: "/new/cache",
		HubDir:   "/new/hub",
		LocalDir: "/new/models",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("registrationOptions() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestLoadedConfigDecodesTypedExecStartArguments(t *testing.T) {
	wantArgs := []string{
		"/srv/HF Desk/bin/hfdesk", "service", "run", "--config",
		"/srv/HF Desk/config with spaces.json", "--empty", "", "--literal", "quote\" and \\ slash $HOME",
	}
	properties := map[string]dbus.Variant{
		"ExecStartEx": dbus.MakeVariant([]systemdExecStartExEntry{{
			Path:  wantArgs[0],
			Argv:  wantArgs,
			Flags: []string{"full-privileges"},
		}}),
		"WorkingDirectory": dbus.MakeVariant("/srv/HF Desk/state"),
		"User":             dbus.MakeVariant("hfdesk"),
		"Group":            dbus.MakeVariant("hfdesk"),
		"Environment": dbus.MakeVariant([]string{
			`HF_HOME=/srv/HF Desk/cache`, `HF_HUB_CACHE=/srv/HF Desk/hub`, `HF_LABEL=quoted "value"`,
		}),
		"EnvironmentFiles": dbus.MakeVariant([]systemdEnvironmentFile{{Path: "/etc/hfdesk/cache file.env", IgnoreErrors: true}}),
	}
	got, err := loadedConfigFromProperties(properties)
	if err != nil {
		t.Fatalf("loadedConfigFromProperties() = %v", err)
	}
	if !got.loaded || got.execPath != wantArgs[0] || !slices.Equal(got.execArgs, wantArgs) || !slices.Equal(got.execFlags, []string{"full-privileges"}) {
		t.Errorf("loaded ExecStart = %+v, want the typed argv and flags", got)
	}
	if got.workingDirectory != "/srv/HF Desk/state" || got.user != "hfdesk" || got.group != "hfdesk" {
		t.Errorf("loaded launch identity = %+v, want the typed values", got)
	}
	if !slices.Equal(got.environment, []string{`HF_HOME=/srv/HF Desk/cache`, `HF_HUB_CACHE=/srv/HF Desk/hub`, `HF_LABEL=quoted "value"`}) {
		t.Errorf("loaded Environment = %q, want each typed variable intact", got.environment)
	}
	if !slices.Equal(got.environmentFiles, []systemdEnvironmentFile{{Path: "/etc/hfdesk/cache file.env", IgnoreErrors: true}}) {
		t.Errorf("loaded EnvironmentFiles = %+v, want the typed path and ignore flag", got.environmentFiles)
	}
}

func TestLoadedConfigDecodesLegacyTypedExecStart(t *testing.T) {
	want := []string{"/opt/HF Desk/hfdesk", "--config", "/etc/HF Desk/hfdesk.json"}
	properties := map[string]dbus.Variant{
		"ExecStart":        dbus.MakeVariant([]systemdExecStartEntry{{Path: want[0], Argv: want}}),
		"WorkingDirectory": dbus.MakeVariant(""),
		"User":             dbus.MakeVariant(""),
		"Group":            dbus.MakeVariant(""),
		"Environment":      dbus.MakeVariant([]string{}),
		"EnvironmentFiles": dbus.MakeVariant([]systemdEnvironmentFile{}),
	}
	got, err := loadedConfigFromProperties(properties)
	if err != nil {
		t.Fatalf("loadedConfigFromProperties() = %v", err)
	}
	if got.execPath != want[0] || !slices.Equal(got.execArgs, want) || got.ignoreErrors {
		t.Errorf("legacy loaded ExecStart = %+v, want the lossless typed argv", got)
	}
}

func TestLoadedConfigRejectsUnavailableTypedProperties(t *testing.T) {
	properties := map[string]dbus.Variant{
		"WorkingDirectory": dbus.MakeVariant(""),
		"User":             dbus.MakeVariant(""),
		"Group":            dbus.MakeVariant(""),
		"Environment":      dbus.MakeVariant([]string{}),
		"EnvironmentFiles": dbus.MakeVariant([]systemdEnvironmentFile{}),
	}
	if _, err := loadedConfigFromProperties(properties); err == nil || !strings.Contains(err.Error(), "ExecStart") {
		t.Fatalf("loadedConfigFromProperties() error = %v, want missing typed ExecStart error", err)
	}
}

func TestLoadedConfigRejectsWrongExecStartSignatureForEmptyArray(t *testing.T) {
	properties := systemdTypedProperties("", []string{}, []string{}, "", "", "", []systemdEnvironmentFile{})
	properties["ExecStartEx"] = dbus.MakeVariant([]systemdExecStartEntry{})
	if _, err := loadedConfigFromProperties(properties); err == nil || !strings.Contains(err.Error(), `ExecStartEx with signature "a(sasbttttuii)"`) {
		t.Fatalf("loadedConfigFromProperties() error = %v, want wrong ExecStartEx signature", err)
	}
}

func TestDBusPropertiesFromCallPreservesTypedValues(t *testing.T) {
	raw := map[string]dbus.Variant{
		"ExecStartEx": dbus.MakeVariant([]systemdExecStartExEntry{{
			Path: "/bin/true", Argv: []string{"/bin/true", ""}, Flags: []string{},
		}}),
		"EnvironmentFiles": dbus.MakeVariant([]systemdEnvironmentFile{{Path: "/etc/hfdesk.env", IgnoreErrors: true}}),
		"WorkingDirectory": dbus.MakeVariant("/srv/hfdesk"),
		"User":             dbus.MakeVariant(""),
		"Group":            dbus.MakeVariant(""),
		"Environment":      dbus.MakeVariant([]string{}),
	}
	call := &dbus.Call{Body: []interface{}{raw}}
	if raw["ExecStartEx"].Signature().String() != "a(sasasttttuii)" || raw["EnvironmentFiles"].Signature().String() != "a(sb)" {
		t.Fatalf("fixture signatures changed: ExecStartEx=%s EnvironmentFiles=%s", raw["ExecStartEx"].Signature(), raw["EnvironmentFiles"].Signature())
	}
	properties, err := dbusPropertiesFromCall(call)
	if err != nil {
		t.Fatalf("dbusPropertiesFromCall() = %v", err)
	}
	if properties["ExecStartEx"].Signature().String() != "a(sasasttttuii)" || properties["EnvironmentFiles"].Signature().String() != "a(sb)" {
		t.Errorf("production extraction changed signatures: ExecStartEx=%s EnvironmentFiles=%s", properties["ExecStartEx"].Signature(), properties["EnvironmentFiles"].Signature())
	}
	if _, err := loadedConfigFromProperties(properties); err != nil {
		t.Errorf("loadedConfigFromProperties(raw) = %v, want a valid typed projection", err)
	}
}

func TestSessionBusAddressUsesCurrentContextWithoutMutatingEnvironment(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	busPath := filepath.Join(runtimeDir, "bus")
	if err := os.WriteFile(busPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	want := "unix:path=" + dbus.EscapeBusAddressValue(busPath)
	if got := sessionBusAddress(); got != want {
		t.Errorf("sessionBusAddress() = %q, want discovered runtime bus %q", got, want)
	}
	if got := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); got != "" {
		t.Errorf("session discovery mutated DBUS_SESSION_BUS_ADDRESS to %q", got)
	}
	if err := os.Remove(busPath); err != nil {
		t.Fatal(err)
	}
	const discovered = "unix:abstract=/tmp/hfdesk-test-session"
	if err := os.WriteFile(filepath.Join(runtimeDir, "dbus-session"), []byte("DBUS_SESSION_BUS_ADDRESS="+discovered+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := sessionBusAddress(); got != discovered {
		t.Errorf("sessionBusAddress() = %q, want dbus-session-file address %q", got, discovered)
	}
	const explicit = "unix:abstract=/tmp/hfdesk-explicit-session"
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", explicit)
	if got := sessionBusAddress(); got != explicit {
		t.Errorf("sessionBusAddress() = %q, want explicit address %q", got, explicit)
	}
}

func TestSystemdStatusUsesAcceptedRegistrationFlagSyntax(t *testing.T) {
	systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	manager := &systemdManager{scope: ScopeUser}
	path, err := manager.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v", err)
	}
	root := t.TempDir()
	unit := fmt.Sprintf(`[Service]
ExecStart=/bin/true service run -config=%s --config=%s -state-dir=%s --state-dir=%s -log-dir=%s -listen=127.0.0.1:8181 --listen=127.0.0.1:9191 -cache-dir=%s --cache-dir=%s -hub-dir=%s --hub-dir=%s -local-dir=%s --local-dir=%s
`, filepath.Join(root, "old.json"), filepath.Join(root, "current.json"), filepath.Join(root, "old-state"), filepath.Join(root, "state"), filepath.Join(root, "logs"), filepath.Join(root, "old-cache"), filepath.Join(root, "cache"), filepath.Join(root, "old-hub"), filepath.Join(root, "hub"), filepath.Join(root, "old-models"), filepath.Join(root, "models"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the unit directory: %v", err)
	}
	for _, name := range []string{path, os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")} {
		if err := os.WriteFile(name, []byte(unit), 0o644); err != nil {
			t.Fatalf("write the unit file %q: %v", name, err)
		}
	}
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	wantPaths := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "current.json"),
		StateDir:   filepath.Join(root, "state"),
		LogDir:     filepath.Join(root, "logs"),
	}
	if status.Executable != "/bin/true" || status.Listen != "127.0.0.1:9191" || status.Paths != wantPaths || status.CacheDir != filepath.Join(root, "cache") {
		t.Errorf("Status() = %+v, want the last accepted values in the registered command", status)
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

// TestSystemdExecStartProgram checks the program of the registered command
// as the supervisor reports it: the slot model reads the program behind a
// recognized wrapper, keeps the executed binary of a plain command, and
// reports a command without a readable program slot as unreadable instead of
// guessing — including a vector whose word boundaries the display lost.
func TestSystemdExecStartProgram(t *testing.T) {
	for _, tc := range []struct {
		name     string
		output   string
		program  string
		readable bool
	}{
		{
			name:     "a plain command",
			output:   "ExecStart={ path=/opt/hfdesk/bin/hfdesk ; argv[]=/opt/hfdesk/bin/hfdesk service run --listen 127.0.0.1:19090 ; ignore_errors=no }\n",
			program:  "/opt/hfdesk/bin/hfdesk",
			readable: true,
		},
		{
			name:     "a recognized env wrapper",
			output:   "ExecStart={ path=/usr/bin/env ; argv[]=/usr/bin/env -i HF_HOME=/srv/hfdesk /opt/hfdesk/hfdesk service run --listen 127.0.0.1:19090 ; ignore_errors=no }\n",
			program:  "/opt/hfdesk/hfdesk",
			readable: true,
		},
		{
			name:     "a wrapper shape without a program slot",
			output:   "ExecStart={ path=/usr/bin/env ; argv[]=/usr/bin/env bare-word /opt/hfdesk/hfdesk service run ; ignore_errors=no }\n",
			program:  "/usr/bin/env",
			readable: false,
		},
		{
			name:     "a vector whose word boundaries the display lost",
			output:   "ExecStart={ path=\"/srv/HF Desk/bin/hfdesk\" ; argv[]=/srv/HF Desk/bin/hfdesk service run ; ignore_errors=no }\n",
			program:  "/srv/HF Desk/bin/hfdesk",
			readable: false,
		},
		{
			name:     "the executed binary alone",
			output:   "ExecStart={ path=/opt/hf desk/bin/hfdesk }\n",
			program:  "/opt/hf desk/bin/hfdesk",
			readable: true,
		},
		{
			name:     "a property without a vector",
			output:   "ExecStart={ path=/bin/true ; ignore_errors=no }\n",
			program:  "/bin/true",
			readable: true,
		},
		{
			name:     "an empty property",
			output:   "ExecStart={ path= ; argv[]= }\n",
			program:  "",
			readable: true,
		},
		{
			name:     "no property at all",
			output:   "LoadState=not-found\n",
			program:  "",
			readable: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			program, readable := execStartProgram(tc.output)
			if program != tc.program || readable != tc.readable {
				t.Errorf("execStartProgram(%q) = (%q, %t), want (%q, %t)", tc.output, program, readable, tc.program, tc.readable)
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

// TestSystemdCheckExecutableAccessUsesKernelPermissions checks the effective
// kernel access result, including execute-only native files and parent search.
func TestSystemdCheckExecutableAccessUsesKernelPermissions(t *testing.T) {
	if accessProbeHasCapabilities() {
		t.Skip("current-identity access is deliberately inconclusive with effective/permitted capabilities")
	}

	root := t.TempDir()
	executable := filepath.Join(root, "program")
	if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	identity := currentExecutableAccessIdentity()
	readOnlyDenial := executableAccessDenied
	if os.Geteuid() == 0 {
		readOnlyDenial = executableAccessAllowed
	}
	for _, tc := range []struct {
		name string
		mode os.FileMode
		want executableAccess
		bits uint32
	}{
		{"owner execute", 0o700, executableAccessAllowed, unix.X_OK},
		{"execute-only native access", 0o100, executableAccessAllowed, unix.X_OK},
		{"read and execute follow the current identity", 0o100, readOnlyDenial, unix.R_OK | unix.X_OK},
		{"no execute permission", 0o600, executableAccessDenied, unix.X_OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Chmod(executable, tc.mode); err != nil {
				t.Fatal(err)
			}
			got, err := checkExecutableAccess(context.Background(), executable, identity, tc.bits, "")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("checkExecutableAccess(%04o, %03o) = %v, want %v", tc.mode, tc.bits, got, tc.want)
			}
		})
	}

	t.Run("parent without search permission", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can bypass this DAC search denial")
		}
		parent := filepath.Join(root, "blocked")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		blocked := filepath.Join(parent, "program")
		if err := os.WriteFile(blocked, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := checkExecutableAccess(context.Background(), blocked, identity, unix.X_OK, "")
		if err != nil {
			t.Fatal(err)
		}
		if got != executableAccessDenied {
			t.Errorf("checkExecutableAccess through a non-searchable parent = %v, want denied", got)
		}
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Errorf("restore the temporary parent permissions for cleanup: %v", err)
		}
	})

	t.Run("symlink to executable", func(t *testing.T) {
		if err := os.Chmod(executable, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "program-link")
		if err := os.Symlink(executable, link); err != nil {
			t.Fatal(err)
		}
		got, err := checkExecutableAccess(context.Background(), link, identity, unix.X_OK, "")
		if err != nil {
			t.Fatal(err)
		}
		if got != executableAccessAllowed {
			t.Errorf("checkExecutableAccess through a symlink = %v, want allowed", got)
		}
	})
}

func TestLinuxExecutableAccessIdentityCarriesSupplementaryGroups(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := linuxExecutableAccessIdentity(account)
	if err != nil {
		t.Skipf("could not resolve account groups: %v", err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	if identity.UID != uid || identity.GID != gid {
		t.Fatalf("identity = %+v, want uid=%d gid=%d", identity, uid, gid)
	}
	accountGroups, err := account.GroupIds()
	if err != nil {
		t.Skipf("could not resolve supplementary groups: %v", err)
	}
	got := make(map[int]bool, len(identity.Groups))
	for _, group := range identity.Groups {
		got[group] = true
	}
	for _, groupID := range accountGroups {
		group, err := strconv.Atoi(groupID)
		if err != nil {
			t.Fatalf("parse account group %q: %v", groupID, err)
		}
		if !got[group] {
			t.Errorf("identity groups %v omit account group %d", identity.Groups, group)
		}
	}
	credential, err := accessCredential(identity)
	if err != nil {
		t.Fatal(err)
	}
	if int(credential.Uid) != uid || int(credential.Gid) != gid {
		t.Errorf("credential = %+v, want uid=%d gid=%d", credential, uid, gid)
	}
	for _, group := range identity.Groups {
		found := false
		for _, credentialGroup := range credential.Groups {
			if int(credentialGroup) == group {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("credential groups %v omit identity group %d", credential.Groups, group)
		}
	}
}

func TestLinuxIDToUint32ChecksFullNumericBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   int64
		want    uint32
		wantErr bool
	}{
		{"zero", 0, 0, false},
		{"largest ordinary 32-bit ID", int64(math.MaxUint32 - 1), uint32(math.MaxUint32 - 1), false},
		{"uint32 sentinel bit pattern", int64(math.MaxUint32), math.MaxUint32, false},
		{"negative", -1, 0, true},
		{"first value beyond uint32", int64(math.MaxUint32) + 1, 0, true},
	} {
		got, err := linuxIDToUint32(tc.value)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("%s: linuxIDToUint32(%d) = (%d, %v), want (%d, error=%t)", tc.name, tc.value, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestAccessCredentialPreservesAndRejectsNativeIntegerBoundaries(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("accessAccount stores IDs as int; this host cannot represent the full uint32 domain")
	}
	maxUint32 := uint64(math.MaxUint32)
	maxID := int(maxUint32 - 1)
	credential, err := accessCredential(accessAccount{UID: maxID, GID: maxID, Groups: []int{maxID}})
	if err != nil {
		t.Fatalf("accessCredential(max uint32 bit pattern) = %v", err)
	}
	maxLegalID := uint32(maxUint32 - 1)
	if credential.Uid != maxLegalID || credential.Gid != maxLegalID || !slices.Contains(credential.Groups, maxLegalID) {
		t.Errorf("credential did not preserve the largest mapped Linux ID: %+v", credential)
	}
	overflow := int(maxUint32 + 1)
	for _, identity := range []accessAccount{
		{UID: -1},
		{GID: -1},
		{Groups: []int{-1}},
		{UID: overflow},
	} {
		if _, err := accessCredential(identity); err == nil {
			t.Errorf("accessCredential(%+v) accepted an ID outside its representable Linux domain", identity)
		}
	}
}

func TestInternalAccessProbeReportsKernelDenialSeparatelyFromUnknown(t *testing.T) {
	if accessProbeHasCapabilities() {
		t.Skip("the internal helper refuses to claim access under effective/permitted capabilities")
	}
	if code, handled := RunInternalAccessProbe([]string{"status"}); handled || code != 0 {
		t.Fatalf("ordinary arguments were handled as an internal probe: code=%d handled=%t", code, handled)
	}

	file := filepath.Join(t.TempDir(), "execute-only")
	if err := os.WriteFile(file, []byte("content"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o100); err != nil {
		t.Fatal(err)
	}
	code, handled := RunInternalAccessProbe([]string{internalAccessProbeCommand, file, strconv.Itoa(unix.X_OK), ""})
	if !handled || code != accessProbeAllowedCode {
		t.Errorf("execute-only X_OK probe = (%d, %t), want allowed", code, handled)
	}
	code, handled = RunInternalAccessProbe([]string{internalAccessProbeCommand, file, strconv.Itoa(unix.R_OK | unix.X_OK), ""})
	if !handled || code != accessProbeDeniedCode {
		t.Errorf("execute-only R_OK|X_OK probe = (%d, %t), want definite denial", code, handled)
	}
	code, handled = RunInternalAccessProbe([]string{internalAccessProbeCommand, filepath.Join(t.TempDir(), "missing"), strconv.Itoa(unix.X_OK), ""})
	if !handled || code != accessProbeDeniedCode {
		t.Errorf("missing path probe = (%d, %t), want definite denial", code, handled)
	}
	code, handled = RunInternalAccessProbe([]string{internalAccessProbeCommand, file, "invalid", ""})
	if !handled || code != accessProbeUnknownCode {
		t.Errorf("malformed internal request = (%d, %t), want unknown", code, handled)
	}
}

func TestSystemdExecuteOnlyNativeCandidateRunsAndInstalls(t *testing.T) {
	current, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(t.TempDir(), "hfdesk")
	if err := os.WriteFile(native, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(native, 0o100); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(native, "-test.run=^$").CombinedOutput(); err != nil {
		t.Fatalf("execute-only native control did not run: %v: %s", err, out)
	}

	_ = systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	opts := systemdTempInstallOptions(t, t.TempDir())
	opts.Executable = native
	manager := &systemdManager{scope: ScopeUser}
	if _, err := manager.Install(context.Background(), opts); err != nil {
		t.Fatalf("Install() refused the execute-only native candidate: %v", err)
	}
	setSystemdTestPropertyReader(t, manager)

	link := filepath.Join(t.TempDir(), "hfdesk-link")
	if err := os.Symlink(native, link); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(link, "-test.run=^$").CombinedOutput(); err != nil {
		t.Fatalf("execute-only native symlink did not run: %v: %s", err, out)
	}
	opts.Executable = link
	effective, err := manager.Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("Install() refused the execute-only native symlink: %v", err)
	}
	if effective.Executable != link {
		t.Errorf("registered executable = %q, want preserved symlink path %q", effective.Executable, link)
	}
}

func TestSystemdPreflightAcceptsReadableShebang(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh is unavailable: %v", err)
	}
	systemdRecordingSupervisor(t)
	root := t.TempDir()
	opts := systemdTempInstallOptions(t, root)
	opts.Executable = filepath.Join(t.TempDir(), "hfdesk-script")
	if err := os.WriteFile(opts.Executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(opts.Executable).Run(); err != nil {
		t.Fatalf("readable shebang control did not run: %v", err)
	}
	if err := (&systemdManager{scope: ScopeUser}).preflight(context.Background(), opts, nil); err != nil {
		t.Fatalf("preflight refused a readable runnable script: %v", err)
	}
}

func TestSystemdPreflightUsesLoadedWorkingDirectoryForRelativeInterpreters(t *testing.T) {
	if binfmtMiscHandlers(linuxBinfmtMiscDirectory) != binfmtMiscEmpty {
		t.Skip("the host's binfmt_misc state does not establish which kernel interpreter handles these formats")
	}
	if accessProbeHasCapabilities() {
		t.Skip("the current identity access probe is inconclusive with effective or permitted capabilities")
	}
	systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	identity := currentExecutableAccessIdentity()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	workingDirectory := filepath.Join(root, "native-working-directory")
	stateDirectory := filepath.Join(root, "application-state")
	for _, directory := range []string{workingDirectory, stateDirectory} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name  string
		build func(*testing.T, string) (string, []string)
	}{
		{
			name: "relative shebang",
			build: func(t *testing.T, dir string) (string, []string) {
				interpreter := filepath.Join(dir, "runner")
				if err := os.WriteFile(interpreter, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				candidate := filepath.Join(root, "shebang-candidate")
				if err := os.WriteFile(candidate, []byte("#!./runner\nexit 0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				return candidate, nil
			},
		},
		{
			name: "relative ELF PT_INTERP",
			build: func(t *testing.T, dir string) (string, []string) {
				image, originalInterpreter, ok := elfWithInterpreterPath(t, "./elf-loader")
				if !ok {
					t.Skip("the running ELF has no PT_INTERP space for a relative loader path")
				}
				candidate := filepath.Join(root, "elf-candidate")
				if err := os.WriteFile(candidate, image, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(originalInterpreter, filepath.Join(dir, "elf-loader")); err != nil {
					t.Fatal(err)
				}
				return candidate, []string{"-test.run=^$"}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate, args := tc.build(t, workingDirectory)
			for _, dir := range []struct {
				name string
				path string
				want bool
			}{
				{"loaded cwd", workingDirectory, true},
				{"application StateDir control", stateDirectory, false},
			} {
				command := exec.Command(candidate, args...)
				command.Dir = dir.path
				err := command.Run()
				if dir.want && err != nil {
					t.Fatalf("kernel execution from %s failed: %v", dir.name, err)
				}
				if !dir.want && err == nil {
					t.Fatalf("kernel execution from %s unexpectedly succeeded", dir.name)
				}
			}

			opts := systemdTempInstallOptions(t, root)
			opts.Executable = candidate
			opts.Paths.StateDir = stateDirectory
			properties := systemdTypedProperties(candidate, []string{candidate}, nil, workingDirectory, account.Username, account.Gid, nil)
			loaded, err := loadedConfigFromProperties(properties)
			if err != nil {
				t.Fatalf("loadedConfigFromProperties() = %v", err)
			}
			projectedCWD := systemdWorkingDirectoryForPreflight(opts, true, loaded, true)
			if projectedCWD != workingDirectory || projectedCWD == opts.Paths.StateDir {
				t.Fatalf("preflight cwd = %q, want loaded native WorkingDirectory %q, distinct from StateDir %q", projectedCWD, workingDirectory, opts.Paths.StateDir)
			}
			manager := &systemdManager{scope: ScopeUser}
			if err := manager.preflightWithContext(context.Background(), opts, account.Username, &identity, projectedCWD); err != nil {
				t.Fatalf("preflight with loaded WorkingDirectory refused the runnable candidate: %v", err)
			}
			if err := manager.preflightWithContext(context.Background(), opts, account.Username, &identity, stateDirectory); err == nil {
				t.Fatal("preflight using application StateDir accepted an interpreter absent from that cwd")
			}
			unknownCWD := systemdWorkingDirectoryForPreflight(opts, true, systemdLoadedConfig{}, false)
			if unknownCWD != "" {
				t.Fatalf("unavailable native WorkingDirectory = %q, want unknown", unknownCWD)
			}
			if err := manager.preflightWithContext(context.Background(), opts, account.Username, &identity, unknownCWD); err != nil {
				t.Errorf("preflight denied an unknown relative-interpreter context: %v", err)
			}
		})
	}
}

// TestSystemdInstallRefusesAnExecutableTheAccountCannotRun checks that a refused
// program fails the installation before the supervisor is asked to act and before
// the unit file or a directory is written.
func TestSystemdInstallRefusesAnExecutableTheAccountCannotRun(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the user-scope access case needs a non-root identity")
	}
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()

	opts := systemdTempInstallOptions(t, root)
	if err := os.Chmod(opts.Executable, 0o001); err != nil {
		t.Fatal(err)
	}

	m := &systemdManager{scope: ScopeUser}
	_, err := m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of a program the account cannot run")
	}
	account := systemdCurrentAccount(t)
	for _, want := range []string{opts.Executable, account.Username, "not usable by the account"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install() = %v, want %q in the refusal", err, want)
		}
	}

	path, pathErr := m.unitPath()
	if pathErr != nil {
		t.Fatalf("unitPath() = %v, want nil", pathErr)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the refused installation wrote the unit file %q: %v", path, statErr)
	}
	for _, dir := range instanceDirs(opts) {
		if _, statErr := os.Stat(dir.path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("the refused installation created the %s %q: %v", dir.name, dir.path, statErr)
		}
	}

	calls := systemdSupervisorCalls(t, logPath)
	for _, command := range []string{"daemon-reload", "enable", "restart", "start"} {
		if got := systemdSupervisorCallsTo(calls, command, unitName); got != 0 {
			t.Errorf("the refused installation issued %q %d times:\n%s", command, got, strings.Join(calls, "\n"))
		}
	}
}

func writeBinfmtMiscState(t *testing.T, active bool) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte("enabled\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if active {
		if err := os.WriteFile(filepath.Join(dir, "custom"), []byte("enabled\ninterpreter /usr/bin/qemu\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func elfWithMissingInterpreter(t *testing.T) ([]byte, bool) {
	t.Helper()
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(running)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := elf.NewFile(bytes.NewReader(content))
	if err != nil {
		return nil, false
	}
	var interpreter *elf.Prog
	for _, program := range parsed.Progs {
		if program.Type == elf.PT_INTERP {
			interpreter = program
			break
		}
	}
	if interpreter == nil || interpreter.Filesz < 3 || interpreter.Filesz > 4096 {
		return nil, false
	}
	start := int(interpreter.Off)
	size := int(interpreter.Filesz)
	if start < 0 || start+size > len(content) {
		return nil, false
	}
	missing := "/" + strings.Repeat("z", size-2)
	if len(missing)+1 != size {
		return nil, false
	}
	if _, err := os.Stat(missing); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}
	segment := make([]byte, size)
	copy(segment, missing)
	copy(content[start:start+size], segment)
	return content, true
}

func elfWithInterpreterPath(t *testing.T, interpreterPath string) ([]byte, string, bool) {
	t.Helper()
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(running)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := elf.NewFile(bytes.NewReader(content))
	if err != nil {
		return nil, "", false
	}
	for _, program := range parsed.Progs {
		if program.Type != elf.PT_INTERP || program.Filesz < 2 || program.Filesz > 4096 {
			continue
		}
		if program.Off > uint64(len(content)) || program.Filesz > uint64(len(content))-program.Off {
			return nil, "", false
		}
		start, size := int(program.Off), int(program.Filesz)
		if len(interpreterPath)+1 > size {
			return nil, "", false
		}
		segment := content[start : start+size]
		terminator := bytes.IndexByte(segment, 0)
		if terminator <= 0 {
			return nil, "", false
		}
		original := string(segment[:terminator])
		replacement := make([]byte, size)
		copy(replacement, interpreterPath)
		copy(content[start:start+size], replacement)
		return content, original, true
	}
	return nil, "", false
}

func makeInterpreterSymlinkTree(t *testing.T, root, target string, atLexicalPath bool) {
	t.Helper()
	actualInside := filepath.Join(root, "actual", "inside")
	if err := os.MkdirAll(actualInside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("actual", "inside"), filepath.Join(root, "via")); err != nil {
		t.Fatal(err)
	}
	interpreter := filepath.Join(root, "actual", "runner")
	if atLexicalPath {
		interpreter = filepath.Join(root, "runner")
	}
	if err := os.Symlink(target, interpreter); err != nil {
		t.Fatal(err)
	}
}

func TestSystemdInterpreterPathsPreserveKernelSymlinkDotDotResolution(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the direct interpreter controls run only as an unprivileged user")
	}
	if binfmtMiscHandlers(linuxBinfmtMiscDirectory) != binfmtMiscEmpty {
		t.Skip("the host has active or unreadable binfmt_misc handlers; these interpreter formats are unclassified")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh is unavailable: %v", err)
	}
	systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	identity := currentExecutableAccessIdentity()
	manager := &systemdManager{scope: ScopeUser}

	validRoot := t.TempDir()
	makeInterpreterSymlinkTree(t, validRoot, "/bin/sh", false)
	absoluteInterpreter := validRoot + "/via/../runner"
	absoluteScript := filepath.Join(validRoot, "absolute-script")
	if err := os.WriteFile(absoluteScript, []byte("#!"+absoluteInterpreter+"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(absoluteScript).CombinedOutput(); err != nil {
		t.Fatalf("kernel rejected valid absolute shebang path %q: %v: %s", absoluteInterpreter, err, out)
	}
	inspection, err := inspectExecutable(absoluteScript, validRoot, linuxBinfmtMiscDirectory)
	if err != nil || inspection.interpreter != absoluteInterpreter {
		t.Fatalf("absolute shebang inspection = (%+v, %v), want raw path %q", inspection, err, absoluteInterpreter)
	}
	access, err := checkExecutableAccess(context.Background(), inspection.interpreter, identity, unix.X_OK, "")
	if err != nil || access != executableAccessAllowed {
		t.Errorf("kernel access for valid absolute interpreter = (%v, %v), want allowed", access, err)
	}
	opts := systemdTempInstallOptions(t, t.TempDir())
	opts.Executable = absoluteScript
	opts.Paths.StateDir = validRoot
	if err := manager.preflight(context.Background(), opts, nil); err != nil {
		t.Errorf("preflight refused the runnable absolute symlink/.. script: %v", err)
	}

	relativeScript := filepath.Join(validRoot, "relative-script")
	if err := os.WriteFile(relativeScript, []byte("#!via/../runner\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(relativeScript)
	command.Dir = validRoot
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("kernel rejected valid relative shebang path: %v: %s", err, out)
	}
	inspection, err = inspectExecutable(relativeScript, validRoot, linuxBinfmtMiscDirectory)
	if err != nil || inspection.interpreter != "via/../runner" {
		t.Fatalf("relative shebang inspection = (%+v, %v), want raw path", inspection, err)
	}
	access, err = checkExecutableAccess(context.Background(), inspection.interpreter, identity, unix.X_OK, validRoot)
	if err != nil || access != executableAccessAllowed {
		t.Errorf("kernel access for valid relative interpreter = (%v, %v), want allowed", access, err)
	}
	opts.Executable = relativeScript
	if err := manager.preflight(context.Background(), opts, nil); err != nil {
		t.Errorf("preflight refused the runnable relative symlink/.. script: %v", err)
	}

	missingRoot := t.TempDir()
	makeInterpreterSymlinkTree(t, missingRoot, "/bin/sh", true)
	missingAbsolute := missingRoot + "/via/../runner"
	missingScript := filepath.Join(missingRoot, "script")
	if err := os.WriteFile(missingScript, []byte("#!"+missingAbsolute+"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(missingScript).Run(); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("kernel control for missing absolute interpreter = %v, want ENOENT", err)
	}
	assertSystemdInstallRefusedBeforeRestart(t, missingScript, missingRoot)

	missingRelative := filepath.Join(missingRoot, "relative-script")
	if err := os.WriteFile(missingRelative, []byte("#!via/../runner\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command = exec.Command(missingRelative)
	command.Dir = missingRoot
	if err := command.Run(); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("kernel control for missing relative interpreter = %v, want ENOENT", err)
	}
	assertSystemdInstallRefusedBeforeRestart(t, missingRelative, missingRoot)

	elfPath := "via/../runner"
	validELFRoot := t.TempDir()
	validELF, originalInterpreter, ok := elfWithInterpreterPath(t, elfPath)
	if !ok {
		t.Log("the test binary has no room for a relative PT_INTERP fixture; ELF path variant not exercised")
		return
	}
	makeInterpreterSymlinkTree(t, validELFRoot, originalInterpreter, false)
	validELFCandidate := filepath.Join(validELFRoot, "candidate")
	if err := os.WriteFile(validELFCandidate, validELF, 0o700); err != nil {
		t.Fatal(err)
	}
	command = exec.Command(validELFCandidate, "-test.run=^$")
	command.Dir = validELFRoot
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("kernel rejected valid relative PT_INTERP %q: %v: %s", elfPath, err, out)
	}
	elfInspection, err := inspectExecutable(validELFCandidate, validELFRoot, linuxBinfmtMiscDirectory)
	if err != nil || elfInspection.interpreter != elfPath {
		t.Fatalf("PT_INTERP inspection = (%+v, %v), want raw relative path %q", elfInspection, err, elfPath)
	}
	access, err = checkExecutableAccess(context.Background(), elfInspection.interpreter, identity, unix.X_OK, validELFRoot)
	if err != nil || access != executableAccessAllowed {
		t.Errorf("kernel access for valid relative PT_INTERP = (%v, %v), want allowed", access, err)
	}
	opts = systemdTempInstallOptions(t, t.TempDir())
	opts.Executable = validELFCandidate
	opts.Paths.StateDir = validELFRoot
	if err := manager.preflight(context.Background(), opts, nil); err != nil {
		t.Errorf("preflight refused the runnable relative PT_INTERP: %v", err)
	}

	missingELFRoot := t.TempDir()
	makeInterpreterSymlinkTree(t, missingELFRoot, originalInterpreter, true)
	missingELF, _, ok := elfWithInterpreterPath(t, elfPath)
	if !ok {
		t.Fatal("relative PT_INTERP no longer fits the test binary")
	}
	missingELFCandidate := filepath.Join(missingELFRoot, "candidate")
	if err := os.WriteFile(missingELFCandidate, missingELF, 0o700); err != nil {
		t.Fatal(err)
	}
	command = exec.Command(missingELFCandidate, "-test.run=^$")
	command.Dir = missingELFRoot
	if err := command.Run(); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("kernel control for missing relative PT_INTERP = %v, want ENOENT", err)
	}
	assertSystemdInstallRefusedBeforeRestart(t, missingELFCandidate, missingELFRoot)
}

func TestSystemdInspectExecutableUsesHostEvidence(t *testing.T) {
	emptyHandlers := writeBinfmtMiscState(t, false)
	activeHandlers := writeBinfmtMiscState(t, true)
	write := func(t *testing.T, name string, content []byte, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, content, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}

	plainText := write(t, "hfdesk", []byte("not an executable\n"), 0o700)
	if _, err := inspectExecutable(plainText, t.TempDir(), emptyHandlers); err == nil {
		t.Fatal("plain text with no shebang and no registered binfmt handler was not refused")
	}
	inspection, err := inspectExecutable(plainText, t.TempDir(), activeHandlers)
	if err != nil || inspection.format != executableFormatUnknown {
		t.Fatalf("plain text with a host handler = (%+v, %v), want unclassified and allowed", inspection, err)
	}
	if _, err := inspectExecutable(plainText, t.TempDir(), filepath.Join(t.TempDir(), "unavailable")); err != nil {
		t.Fatalf("plain text with unavailable handler evidence was refused: %v", err)
	}

	truncated := make([]byte, 20)
	copy(truncated, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	truncatedPath := write(t, "truncated-elf", truncated, 0o700)
	if _, err := inspectExecutable(truncatedPath, t.TempDir(), emptyHandlers); err == nil {
		t.Fatal("truncated ELF header with no registered handler was not refused")
	}
	inspection, err = inspectExecutable(truncatedPath, t.TempDir(), activeHandlers)
	if err != nil || inspection.format != executableFormatUnknown {
		t.Fatalf("truncated ELF with a host handler = (%+v, %v), want unclassified and allowed", inspection, err)
	}

	missingInterpreter := filepath.Join(t.TempDir(), "missing-interpreter")
	script := write(t, "script", []byte("#!"+missingInterpreter+"\nexit 0\n"), 0o700)
	inspection, err = inspectExecutable(script, t.TempDir(), emptyHandlers)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.format != executableFormatScript || inspection.interpreter != missingInterpreter {
		t.Fatalf("script inspection = %+v, want interpreter %q", inspection, missingInterpreter)
	}
	access, err := checkExecutableAccess(context.Background(), inspection.interpreter, currentExecutableAccessIdentity(), unix.X_OK, "")
	if err != nil || access != executableAccessDenied {
		t.Errorf("missing interpreter access = (%v, %v), want definite denial", access, err)
	}

	if _, err := os.Stat("/bin/sh"); err == nil {
		readableScript := write(t, "readable-script", []byte("#!/bin/sh\nexit 0\n"), 0o700)
		inspection, err := inspectExecutable(readableScript, t.TempDir(), emptyHandlers)
		if err != nil || inspection.format != executableFormatScript || inspection.interpreter != "/bin/sh" {
			t.Fatalf("readable script inspection = (%+v, %v)", inspection, err)
		}
		if err := exec.Command(readableScript).Run(); err != nil {
			t.Fatalf("the readable script control did not run: %v", err)
		}
	}

	if dynamicELF, ok := elfWithMissingInterpreter(t); ok {
		candidate := write(t, "missing-elf-interpreter", dynamicELF, 0o700)
		inspection, err := inspectExecutable(candidate, t.TempDir(), emptyHandlers)
		if err != nil {
			t.Fatal(err)
		}
		if inspection.format != executableFormatELF || inspection.interpreter == "" {
			t.Fatalf("ELF PT_INTERP inspection = %+v, want a discovered interpreter", inspection)
		}
		access, err := checkExecutableAccess(context.Background(), inspection.interpreter, currentExecutableAccessIdentity(), unix.X_OK, "")
		if err != nil || access != executableAccessDenied {
			t.Errorf("missing PT_INTERP access = (%v, %v), want definite denial", access, err)
		}
	}

	// A structurally complete ELF with another machine identity is not
	// refused by a GOARCH table; the kernel/host remains the format owner.
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	elf, err := os.ReadFile(running)
	if err != nil || len(elf) < 64 || !bytes.HasPrefix(elf, []byte{0x7f, 'E', 'L', 'F'}) {
		t.Fatalf("read the running ELF fixture: %v", err)
	}
	var order binary.ByteOrder = binary.LittleEndian
	if elf[5] == 2 {
		order = binary.BigEndian
	}
	machine := order.Uint16(elf[18:20])
	if machine == ^uint16(0) {
		machine = 0
	} else {
		machine++
	}
	order.PutUint16(elf[18:20], machine)
	otherMachine := write(t, "other-machine-elf", elf, 0o700)
	inspection, err = inspectExecutable(otherMachine, t.TempDir(), emptyHandlers)
	if err != nil || inspection.format != executableFormatELF {
		t.Errorf("structurally complete ELF with another machine = (%+v, %v), want no parser refusal", inspection, err)
	}

	// Other signatures stay unclassified rather than being treated as proof
	// of an unsupported format.
	other := write(t, "other-format", []byte("MZ\x90\x00"), 0o700)
	inspection, err = inspectExecutable(other, t.TempDir(), emptyHandlers)
	if err != nil || inspection.format != executableFormatUnknown {
		t.Errorf("unrecognized format = (%+v, %v), want unclassified and allowed", inspection, err)
	}
}

func TestSystemdInstallRefusesKnownUnusableBeforeRestart(t *testing.T) {
	if binfmtMiscHandlers(linuxBinfmtMiscDirectory) != binfmtMiscEmpty {
		t.Skip("the host has active or unreadable binfmt_misc handlers; these inputs are intentionally unclassified")
	}

	truncated := make([]byte, 20)
	copy(truncated, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	cases := []struct {
		name string
		data []byte
	}{
		{"plain text", []byte("not an executable\n")},
		{"truncated ELF", truncated},
		{"missing interpreter", []byte("#!/definitely-missing-hfdesk-interpreter\nexit 0\n")},
	}
	if dynamicELF, ok := elfWithMissingInterpreter(t); ok {
		cases = append(cases, struct {
			name string
			data []byte
		}{"missing ELF interpreter", dynamicELF})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := filepath.Join(t.TempDir(), "hfdesk")
			if err := os.WriteFile(candidate, tc.data, 0o700); err != nil {
				t.Fatal(err)
			}
			assertSystemdInstallRefusedBeforeRestart(t, candidate, "")
		})
	}
}

func assertSystemdInstallRefusedBeforeRestart(t *testing.T, candidate, workingDirectory string) {
	t.Helper()
	logPath := systemdRecordingSupervisor(t)
	previous := systemdExistingOptions(t, systemdTestExecutable(t), t.TempDir())
	if workingDirectory != "" {
		previous.Paths.StateDir = workingDirectory
	}
	manager, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	if err := os.MkdirAll(previous.Paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateSentinel := filepath.Join(previous.Paths.StateDir, "jobs_state.json")
	if err := os.WriteFile(stateSentinel, []byte(`{"jobs":[{"id":"keep"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", candidate)
	if _, err := manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, candidate)); err == nil {
		t.Fatal("Install() succeeded for a host-demonstrably unusable candidate")
	}
	unit, err := os.ReadFile(unitPath)
	if err != nil || string(unit) != previousUnit {
		t.Fatalf("the previous unit changed: read err=%v", err)
	}
	if content, err := os.ReadFile(stateSentinel); err != nil || string(content) != `{"jobs":[{"id":"keep"}]}` {
		t.Errorf("the state sentinel changed: content=%q err=%v", content, err)
	}
	calls := systemdSupervisorCalls(t, logPath)
	for _, command := range []string{"daemon-reload", "enable", "restart", "start"} {
		if count := systemdSupervisorCallsTo(calls, command, unitName); count != 0 {
			t.Errorf("preflight refusal issued %q %d times: %v", command, count, calls)
		}
	}
}

func TestSystemdPreflightWarnsRatherThanRejectingInconclusiveAccess(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousWriter) })
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	accountCopy := *account
	accountCopy.Uid = strconv.Itoa(os.Geteuid() + 1)
	opts := systemdTempInstallOptions(t, t.TempDir())
	opts.Scope = ScopeSystem
	manager := &systemdManager{scope: ScopeSystem}

	err = manager.preflight(context.Background(), opts, &accountCopy)
	if err != nil {
		t.Fatalf("preflight() = %v, want unknown access to remain unclassified", err)
	}
	if !strings.Contains(logs.String(), "inconclusive") || strings.Contains(logs.String(), "not usable") {
		t.Errorf("access warning = %q, want an inconclusive warning without a denial claim", logs.String())
	}
	path, err := manager.unitPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("inconclusive preflight wrote the registration %q: %v", path, err)
	}
	if calls := systemdSupervisorCalls(t, logPath); len(calls) != 0 {
		t.Errorf("inconclusive preflight called the supervisor: %v", calls)
	}
}

func TestSystemdPreflightLeavesUnknownFormatUnclassified(t *testing.T) {
	systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	opts := systemdTempInstallOptions(t, root)
	opts.Executable = filepath.Join(t.TempDir(), "custom-format")
	if err := os.WriteFile(opts.Executable, []byte("MZ\x90\x00"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (&systemdManager{scope: ScopeUser}).preflight(context.Background(), opts, nil); err != nil {
		t.Fatalf("preflight refused an unclassified format: %v", err)
	}
}

// systemdRecordingSupervisor replaces systemctl with a fake supervisor that
// records every call and keeps the unit file on disk separate from a
// supervisor-loaded copy. Typed D-Bus properties come from the Go test fake,
// not the lossy display string printed by systemctl.
//
// A successful daemon-reload copies the on-disk unit into the loaded copy; a
// configured reload failure leaves that copy unchanged. `show` reports the
// lifecycle state, while the loaded configuration is read from the typed Go
// fake and active state is controlled separately by HFDESK_TEST_SYSTEMCTL_ACTIVE.
// A restart fails once when HFDESK_TEST_SYSTEMCTL_RESTART_FAIL_ONCE is "1", so
// a test can drive a failed update and a successful restore.
//
// A restart reports the state it leaves behind: when
// HFDESK_TEST_SYSTEMCTL_RESTARTED names a marker file, the supervisor creates it
// on the first restart and reports HFDESK_TEST_SYSTEMCTL_ACTIVE_AFTER_RESTART
// from then on. A restart returns as soon as the unit was forked, so a test can
// drive a restart that is accepted while the unit is dead. When
// HFDESK_TEST_SYSTEMCTL_RESTART_COUNT names a file, the number of restarts is
// written to it for a test readiness probe to distinguish the failed update
// from a later recovery start.
//
// The readiness probe is replaced for the run of the test: the fake supervisor
// forks no instance, so no server of the instance answers on the registered
// address. A test that drives the probe installs one of its own after this
// helper returned.
//
// The enablement that "is-enabled" reports is HFDESK_TEST_SYSTEMCTL_ENABLED_STATE:
// "enabled" (the default) and "enabled-runtime" exit with a zero status, so the
// unit counts as enabled for boot, and every other state ("disabled", "masked",
// "masked-runtime") exits with a non-zero status, so a test can drive a repeat
// installation of a unit its administrator disabled or of a masked unit that the
// installation refuses. "static", "indirect", "alias" and "linked" exit with a
// zero status as well, the way the real supervisor answers them, so a test proves
// that the printed state and not the status decides. "masked" is a persistent
// mask, which the fake supervisor reports while a unit file may exist;
// "masked-runtime" is the state of a unit masked at runtime, which a test
// combines with an absent unit file, because the mask lives in /run and leaves
// nothing at the unit path. "not-found" is what a fresh machine reports for a
// unit it does not know. An "enable" that fails once
// HFDESK_TEST_SYSTEMCTL_ENABLE_FAIL is "1" drives the failure of an installation
// that cannot enable its registration; "enable --runtime" fails with it.
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
is-enabled)
	state="${HFDESK_TEST_SYSTEMCTL_ENABLED_STATE:-enabled}"
	echo "$state"
	case "$state" in
	enabled|enabled-runtime|static|indirect|alias|linked)
		# The supervisor exits zero for every one of these states, and only
		# "enabled" and "enabled-runtime" are enablements: the printed
		# state decides what an installation keeps.
		exit 0
		;;
	esac
	exit 1
	;;
daemon-reload)
	count=0
	if [ -n "$HFDESK_TEST_SYSTEMCTL_RELOAD_COUNT" ] && [ -e "$HFDESK_TEST_SYSTEMCTL_RELOAD_COUNT" ]; then
		count=$(cat "$HFDESK_TEST_SYSTEMCTL_RELOAD_COUNT")
	fi
	count=$((count + 1))
	if [ -n "$HFDESK_TEST_SYSTEMCTL_RELOAD_COUNT" ]; then
		printf '%s' "$count" > "$HFDESK_TEST_SYSTEMCTL_RELOAD_COUNT" || exit 1
	fi
	if [ "$HFDESK_TEST_SYSTEMCTL_DAEMON_RELOAD_FAIL_AT" = "$count" ]; then
		echo "the fake supervisor refuses daemon-reload $count" >&2
		exit 1
	fi
	unit="${XDG_CONFIG_HOME}/systemd/user/hfdesk.service"
	if [ -n "$HFDESK_TEST_SYSTEMCTL_LOADED_UNIT" ]; then
		if [ -e "$unit" ]; then
			cp "$unit" "$HFDESK_TEST_SYSTEMCTL_LOADED_UNIT" || exit 1
		else
			rm -f "$HFDESK_TEST_SYSTEMCTL_LOADED_UNIT" || exit 1
		fi
	fi
	exit 0
	;;
enable)
	if [ "$HFDESK_TEST_SYSTEMCTL_ENABLE_FAIL" = "1" ]; then
		echo "the fake supervisor refuses the enable" >&2
		exit 1
	fi
	exit 0
	;;
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
	unit="${XDG_CONFIG_HOME}/systemd/user/hfdesk.service"
	if [ -n "$HFDESK_TEST_SYSTEMCTL_LOADED_UNIT" ]; then
		if [ ! -e "$HFDESK_TEST_SYSTEMCTL_LOADED_UNIT" ] && [ -z "$HFDESK_TEST_SYSTEMCTL_EXECUTABLE" ]; then
			echo "LoadState=not-found"
			echo "ActiveState=inactive"
			echo "SubState=dead"
			exit 0
		fi
		if [ -e "$HFDESK_TEST_SYSTEMCTL_LOADED_UNIT" ]; then
			unit="$HFDESK_TEST_SYSTEMCTL_LOADED_UNIT"
		fi
	fi
	if [ ! -e "$unit" ] && [ -z "$HFDESK_TEST_SYSTEMCTL_EXECUTABLE" ]; then
		echo "LoadState=not-found"
		echo "ActiveState=inactive"
		echo "SubState=dead"
		exit 0
	fi
	echo "LoadState=loaded"
	echo "ActiveState=$active"
	echo "SubState=$sub"
	executable="$HFDESK_TEST_SYSTEMCTL_EXECUTABLE"
	if [ -n "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT" ] && [ -e "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT" ]; then
		restart_count=$(cat "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT")
		if [ "$restart_count" = "2" ] && [ -n "$HFDESK_TEST_SYSTEMCTL_EXECUTABLE_AFTER_SECOND_RESTART" ]; then
			executable="$HFDESK_TEST_SYSTEMCTL_EXECUTABLE_AFTER_SECOND_RESTART"
		fi
	fi
	if [ -n "$HFDESK_TEST_SYSTEMCTL_EXEC_START" ]; then
		# The registered command as systemd reports it: the first word is
		# the executed path=, the whole vector the argv[]= the program
		# slot is read from.
		command="$HFDESK_TEST_SYSTEMCTL_EXEC_START"
		path="${command%% *}"
		printf 'ExecStart={ path=%s ; argv[]=%s ; ignore_errors=no }\n' "$path" "$command"
	elif [ -n "$executable" ]; then
		printf 'ExecStart={ path=%s }\n' "$executable"
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
	t.Setenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT", filepath.Join(dir, "loaded-unit"))
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RELOAD_COUNT", filepath.Join(dir, "reload-count"))
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
	loadedPath := os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")
	m := &systemdManager{scope: ScopeUser}
	setSystemdTestPropertyReader(t, m)
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
	if err := os.WriteFile(loadedPath, []byte(unit), 0o644); err != nil {
		t.Fatalf("write the loaded unit: %v", err)
	}
	return m, path, unit
}

func setSystemdTestPropertyReader(t *testing.T, manager *systemdManager) {
	t.Helper()
	loadedPath := os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")
	manager.readServiceProperties = func(_ context.Context, name string) (map[string]dbus.Variant, error) {
		if name != unitName {
			return nil, fmt.Errorf("unexpected unit %q", name)
		}
		content, err := os.ReadFile(loadedPath)
		if err != nil {
			return nil, err
		}
		return systemdTestPropertiesFromUnit(string(content)), nil
	}
}

func systemdTestPropertiesFromUnit(content string) map[string]dbus.Variant {
	unit := parseUnit(content)
	var commands []systemdExecStartExEntry
	if unit.ExecStartRaw != "" {
		argv := append([]string{unit.ExecStartRaw}, unit.ExecArgsRaw...)
		commands = append(commands, systemdExecStartExEntry{Path: unit.ExecStartRaw, Argv: argv})
	}
	var environment []string
	var environmentFiles []systemdEnvironmentFile
	var supplementaryGroups []string
	section := ""
	for _, logical := range systemdLogicalLines(content) {
		trimmed := strings.TrimSpace(logical.text)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.ToLower(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
			continue
		}
		if section != "service" {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Environment":
			environment = append(environment, splitSystemdWords(value)...)
		case "SupplementaryGroups":
			if strings.TrimSpace(value) == "" {
				supplementaryGroups = nil
			} else {
				supplementaryGroups = append(supplementaryGroups, splitSystemdWords(value)...)
			}
		case "EnvironmentFile":
			for _, entry := range splitSystemdWords(value) {
				file := systemdEnvironmentFile{}
				if strings.HasPrefix(entry, "-") {
					file.IgnoreErrors = true
					entry = strings.TrimPrefix(entry, "-")
				}
				file.Path = entry
				environmentFiles = append(environmentFiles, file)
			}
		}
	}
	return map[string]dbus.Variant{
		"ExecStartEx":         dbus.MakeVariant(commands),
		"WorkingDirectory":    dbus.MakeVariant(unit.WorkingDirectory),
		"User":                dbus.MakeVariant(unit.User),
		"Group":               dbus.MakeVariant(unit.Group),
		"SupplementaryGroups": dbus.MakeVariant(supplementaryGroups),
		"MainPID":             dbus.MakeVariant(uint32(0)),
		"Environment":         dbus.MakeVariant(environment),
		"EnvironmentFiles":    dbus.MakeVariant(environmentFiles),
		"FragmentPath":        dbus.MakeVariant("/tmp/hfdesk-test-fragment.service"),
		"DropInPaths":         dbus.MakeVariant([]string{}),
	}
}

func systemdTypedProperties(execPath string, argv, environment []string, workingDirectory, userName, groupName string, environmentFiles []systemdEnvironmentFile) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"ExecStartEx": dbus.MakeVariant([]systemdExecStartExEntry{{
			Path:  execPath,
			Argv:  slices.Clone(argv),
			Flags: []string{},
		}}),
		"WorkingDirectory":    dbus.MakeVariant(workingDirectory),
		"User":                dbus.MakeVariant(userName),
		"Group":               dbus.MakeVariant(groupName),
		"SupplementaryGroups": dbus.MakeVariant([]string{}),
		"MainPID":             dbus.MakeVariant(uint32(0)),
		"Environment":         dbus.MakeVariant(slices.Clone(environment)),
		"EnvironmentFiles":    dbus.MakeVariant(slices.Clone(environmentFiles)),
		"FragmentPath":        dbus.MakeVariant("/tmp/hfdesk-test-fragment.service"),
		"DropInPaths":         dbus.MakeVariant([]string{}),
	}
}

func setSystemdTypedPropertySequence(t *testing.T, manager *systemdManager, properties ...map[string]dbus.Variant) {
	t.Helper()
	if len(properties) == 0 {
		t.Fatal("a typed manager sequence must include at least one state")
	}
	reloadCountPath := os.Getenv("HFDESK_TEST_SYSTEMCTL_RELOAD_COUNT")
	manager.readServiceProperties = func(_ context.Context, name string) (map[string]dbus.Variant, error) {
		if name != unitName {
			return nil, fmt.Errorf("unexpected unit %q", name)
		}
		index := 0
		content, err := os.ReadFile(reloadCountPath)
		if err == nil {
			count, parseErr := strconv.Atoi(strings.TrimSpace(string(content)))
			if parseErr != nil {
				return nil, parseErr
			}
			index = count
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if index >= len(properties) {
			index = len(properties) - 1
		}
		return properties[index], nil
	}
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
	previous.Listen = "127.0.0.1:" + strconv.Itoa(freePort(t))
	for _, dir := range []string{filepath.Dir(previous.Paths.ConfigFile), previous.Paths.StateDir, previous.Paths.LogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	process := startSystemdRegisteredProcess(t, previous, "fixture-version")
	restartCount := filepath.Join(root, "restart-count")
	systemdRuntimePropertyReader(t, m, process.Process.Pid, process.Process.Pid, restartCount)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_COUNT", restartCount)
	previousProbe := serviceReadinessProbe
	serviceReadinessProbe = probeServiceReady
	t.Cleanup(func() { serviceReadinessProbe = previousProbe })
	previousInstallProbe := serviceInstallProbe
	var probedListen []string
	serviceInstallProbe = func(listen, expectedVersion string) (bool, error) {
		probedListen = append(probedListen, listen)
		return previousInstallProbe(listen, expectedVersion)
	}
	t.Cleanup(func() { serviceInstallProbe = previousInstallProbe })
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)

	requested := DefaultInstallOptions(ScopeUser, newExecutable)
	effective, err := m.Install(context.Background(), requested)
	if err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}
	if !slices.Equal(probedListen, []string{previous.Listen}) {
		t.Errorf("static readiness probes = %v, want the registered address %q", probedListen, previous.Listen)
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

func TestSystemdReinstallPreservesUnrelatedUnitSettings(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	previousUnit = strings.Replace(previousUnit,
		"WorkingDirectory="+previous.Paths.StateDir,
		"WorkingDirectory=/operator/launch-dir", 1)
	previousUnit = strings.Replace(previousUnit,
		"Environment=HOME=",
		"Environment=HF_HUB_CACHE=/operator/hub\nEnvironmentFile=-/etc/hfdesk/service.env\nEnvironment=HOME=", 1)
	previousUnit = strings.Replace(previousUnit,
		"Restart=on-failure",
		"Restart=always", 1)
	previousUnit = strings.Replace(previousUnit,
		"TimeoutStopSec=",
		"ExecStartPre=/usr/bin/test -d /operator/data\nTimeoutStopSec=", 1)
	for _, name := range []string{unitPath, os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")} {
		if err := os.WriteFile(name, []byte(previousUnit), 0o644); err != nil {
			t.Fatalf("write the customized unit %q: %v", name, err)
		}
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

	if _, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable)); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}
	content, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read the updated unit: %v", err)
	}
	for _, setting := range []string{
		"WorkingDirectory=/operator/launch-dir",
		"Environment=HF_HUB_CACHE=/operator/hub",
		"EnvironmentFile=-/etc/hfdesk/service.env",
		"Restart=always",
		"ExecStartPre=/usr/bin/test -d /operator/data",
	} {
		if !strings.Contains(string(content), setting) {
			t.Errorf("the updated unit lost %q:\n%s", setting, content)
		}
	}
	if got := parseUnit(string(content)).ExecStart; got != newExecutable {
		t.Errorf("the updated executable = %q, want %q", got, newExecutable)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 0 {
		t.Errorf("the stopped unit was restarted %d times", got)
	}
}

func TestSystemdUpdateOfStoredButUnloadedUnitStaysStopped(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	loadedPath := os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")
	if err := os.Remove(loadedPath); err != nil {
		t.Fatalf("mark the stored unit as not loaded: %v", err)
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

	effective, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err != nil {
		t.Fatalf("Install() = %v, want an update of the stored unit", err)
	}
	want := previous
	want.Executable = newExecutable
	if !reflect.DeepEqual(effective, want) {
		t.Errorf("Install() = %+v, want the previous stored options with the new executable %+v", effective, want)
	}
	content, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read the updated unit: %v", err)
	}
	if got := parseUnit(string(content)).ExecStart; got != newExecutable {
		t.Errorf("stored executable = %q, want %q", got, newExecutable)
	}
	if string(content) == previousUnit {
		t.Fatal("the stored unit did not change to the requested executable")
	}
	if _, err := os.Stat(loadedPath); err != nil {
		t.Errorf("daemon-reload did not load the updated stored unit: %v", err)
	}
	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "start", unitName) + systemdSupervisorCallsTo(calls, "restart", unitName); got != 0 {
		t.Errorf("the stored but stopped instance was started %d times:\n%s", got, strings.Join(calls, "\n"))
	}
}

func TestSystemdStoredUnloadedUpdateKeepsHexEscapedConfigPath(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	config := filepath.Join(root, "config one", "hfdesk.json")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(`{"connections":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir, logDir := filepath.Join(root, "state"), filepath.Join(root, "logs")
	content := fmt.Sprintf("[Service]\nExecStart=/bin/true service run -config=%s -state-dir=%s -log-dir=%s --cache-dir=\n[Install]\nWantedBy=default.target\n", strings.ReplaceAll(config, " ", `\x20`), stateDir, logDir)
	manager := &systemdManager{scope: ScopeUser}
	setSystemdTestPropertyReader(t, manager)
	path, err := manager.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	installed, err := manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, "/bin/true"))
	if err != nil {
		t.Fatalf("Install() = %v, want a stopped stored-unit update", err)
	}
	if installed.Paths.ConfigFile != config {
		t.Errorf("Install() changed the unnamed config path: got %q, want %q", installed.Paths.ConfigFile, config)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := parseUnit(string(updated)).paths().ConfigFile; got != config {
		t.Errorf("updated unit config path = %q, want %q", got, config)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 0 {
		t.Errorf("stored but unloaded unit was restarted %d times", got)
	}
}

func TestSystemdUpdatePreservesEscapedDollarArguments(t *testing.T) {
	systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	m, unitPath, _ := systemdExistingInstallation(t, previous)
	requested := DefaultInstallOptions(ScopeUser, newExecutable)
	requested.Paths.ConfigFile = filepath.Join(root, "new$desk", "hfdesk.json")
	requested.Explicit.Config = true
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

	effective, err := m.Install(context.Background(), requested)
	if err != nil {
		t.Fatalf("Install() = %v, want the escaped-dollar update", err)
	}
	if effective.Executable != newExecutable || effective.Paths.ConfigFile != requested.Paths.ConfigFile {
		t.Errorf("effective options = %+v, want the literal dollar paths", effective)
	}
	content, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "new$$desk") {
		t.Errorf("the updated unit did not escape literal dollar signs:\n%s", content)
	}
	parsed := parseUnit(string(content))
	if parsed.ExecStart != newExecutable || parsed.flagValue("--config") != requested.Paths.ConfigFile {
		t.Errorf("the parsed registration = %+v, want the literal dollar config path", parsed)
	}
}

// splitRenderedExecStart locates the ExecStart= command of a rendered unit:
// it returns the program part (including the "ExecStart=" key), the run
// arguments behind it and the offset of the directive.
func splitRenderedExecStart(t *testing.T, unit, executable string) (program, tail string, start int) {
	t.Helper()
	program = `ExecStart="` + executable + `"`
	start = strings.Index(unit, program)
	if start < 0 {
		t.Fatalf("the rendered unit does not hold the command %q:\n%s", program, unit)
	}
	rest := unit[start:]
	lineEnd := strings.Index(rest, "\n")
	if lineEnd < 0 {
		lineEnd = len(rest)
	}
	return program, rest[len(program):lineEnd], start
}

// systemdWrapExecStart replaces the ExecStart= command of a rendered unit
// with a hand-edited value and keeps the rest of the unit file.
func systemdWrapExecStart(t *testing.T, unit, executable, value string) string {
	t.Helper()
	program, tail, start := splitRenderedExecStart(t, unit, executable)
	return unit[:start] + "ExecStart=" + value + unit[start+len(program)+len(tail):]
}

// TestSystemdExecStartUpdateRewritesOnlyRewritableCommands checks the merge
// of the unit command: the program slot takes the new program while a
// recognized wrapper and its arguments stay, and a form the merge cannot
// rewrite is refused by name instead of being corrupted.
func TestSystemdExecStartUpdateRewritesOnlyRewritableCommands(t *testing.T) {
	const executable = "/opt/hfdesk-new/hfdesk"
	const tail = "service run --listen 127.0.0.1:19090"
	opts := InstallOptions{Scope: ScopeUser, Executable: executable}

	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{
			name:  "the program with its run vector",
			value: "/opt/hfdesk-old/hfdesk " + tail,
			want:  `"` + executable + `" ` + tail,
		},
		{
			name:  "the program alone",
			value: "/opt/hfdesk-old/hfdesk",
			want:  `"` + executable + `"`,
		},
		{
			name:  "an env wrapper",
			value: "/usr/bin/env hfdesk " + tail,
			want:  `/usr/bin/env "` + executable + `" ` + tail,
		},
		{
			name:  "an env wrapper with options and assignments",
			value: "/usr/bin/env -i HF_HOME=/srv/hfdesk hfdesk " + tail,
			want:  `/usr/bin/env -i HF_HOME=/srv/hfdesk "` + executable + `" ` + tail,
		},
		{
			name:  "a dynamic program source",
			value: "%h/bin/hfdesk " + tail,
			want:  `"` + executable + `" ` + tail,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "[Unit]\nDescription=HFDesk\n\n[Service]\nExecStart=" + tc.value + "\nWorkingDirectory=/srv/state\n"
			got, err := updateSystemdExecStartSource(content, opts)
			if err != nil {
				t.Fatalf("updateSystemdExecStartSource() = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("updateSystemdExecStartSource() = %q, want %q", got, tc.want)
			}
			if err := validateSystemdExecStartUpdate(content, executable); err != nil {
				t.Errorf("validateSystemdExecStartUpdate() = %v, want nil for a rewritable form", err)
			}
		})
	}

	for _, tc := range []struct {
		name  string
		value string
		form  string
	}{
		{
			name:  "a shell script string",
			value: `/bin/sh -c "exec hfdesk ` + tail + `"`,
			form:  "/bin/sh -c",
		},
		{
			name:  "an unknown wrapper",
			value: "/usr/bin/timeout 30 hfdesk " + tail,
			form:  "timeout 30",
		},
		{
			name:  "an env wrapper with an option value",
			value: "/usr/bin/env -u LD_PRELOAD hfdesk " + tail,
			form:  "LD_PRELOAD",
		},
		{
			name:  "a command without the run vector",
			value: "/opt/hfdesk-old/hfdesk --serve",
			form:  "--serve",
		},
		{
			name:  "a command whose vector is not the run vector",
			value: "/opt/hfdesk-old/hfdesk service status",
			form:  "service status",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "[Service]\nExecStart=" + tc.value + "\n"
			if _, err := updateSystemdExecStartSource(content, opts); err == nil {
				t.Fatal("updateSystemdExecStartSource() = nil, want the refusal of the unrewritable form")
			}
			err := validateSystemdExecStartUpdate(content, executable)
			if err == nil {
				t.Fatal("validateSystemdExecStartUpdate() = nil, want the refusal of the unrewritable form")
			}
			for _, want := range []string{"ExecStart=", tc.form, "not changed", executable} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("validateSystemdExecStartUpdate() = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestSystemdInstallRefusesUnrewritableExecStartBeforeChangingTheUnit checks
// that an update over a hand-edited unit whose ExecStart= feeds a shell
// script is refused before anything changes, whether the instance runs or is
// stopped: a stopped instance must never be left with a command that cannot
// start, and a running instance keeps the supervisor untouched instead of
// being restarted onto a broken command. The refusal names the form, states
// that nothing changed and gives the next action.
func TestSystemdInstallRefusesUnrewritableExecStartBeforeChangingTheUnit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active string
	}{
		{name: "a stopped instance", active: "inactive"},
		{name: "a running instance", active: "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logPath := systemdRecordingSupervisor(t)
			root := t.TempDir()
			oldExecutable := systemdTestExecutable(t)
			newExecutable := systemdTestExecutable(t)
			previous := systemdExistingOptions(t, oldExecutable, root)
			m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
			wrapped := systemdWrapExecStart(t, previousUnit, oldExecutable,
				`/bin/sh -c "exec hfdesk service run --listen 127.0.0.1:19090"`)
			for _, name := range []string{unitPath, os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")} {
				if err := os.WriteFile(name, []byte(wrapped), 0o644); err != nil {
					t.Fatalf("write the wrapped unit %q: %v", name, err)
				}
			}
			t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", tc.active)
			t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)

			_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
			if err == nil {
				t.Fatal("Install() = nil, want the refusal of the shell-wrapped ExecStart")
			}
			for _, want := range []string{"ExecStart=", "/bin/sh -c", "not changed", newExecutable} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Install() = %q, want it to contain %q", err, want)
				}
			}
			content, readErr := os.ReadFile(unitPath)
			if readErr != nil || string(content) != wrapped {
				t.Errorf("the refused update changed the unit: read err=%v\n%s", readErr, content)
			}
			calls := systemdSupervisorCalls(t, logPath)
			for _, command := range []string{"daemon-reload", "enable", "restart", "start"} {
				if count := systemdSupervisorCallsTo(calls, command, unitName); count != 0 {
					t.Errorf("the refused update issued %q %d times: %v", command, count, calls)
				}
			}
		})
	}
}

// TestSystemdInstallRepairsAnEnvWrappedExecStart checks the rewrite the
// update promises for a recognized wrapper: the "env" prefix and its
// arguments stay, the program slot takes the new program, the run arguments
// and the other directives keep the registration's settings, and the stopped
// instance is not started.
func TestSystemdInstallRepairsAnEnvWrappedExecStart(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	_, tail, _ := splitRenderedExecStart(t, previousUnit, oldExecutable)
	wrapped := systemdWrapExecStart(t, previousUnit, oldExecutable, "/usr/bin/env HF_HOME=/srv/hfdesk hfdesk"+tail)
	for _, name := range []string{unitPath, os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")} {
		if err := os.WriteFile(name, []byte(wrapped), 0o644); err != nil {
			t.Fatalf("write the wrapped unit %q: %v", name, err)
		}
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)

	if _, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable)); err != nil {
		t.Fatalf("Install() = %v, want the preserved env-wrapped update", err)
	}
	content, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read the updated unit: %v", err)
	}

	// The promised rewrite: wrapper prefix kept, program slot replaced,
	// every run argument behind it untouched.
	want := `ExecStart=/usr/bin/env HF_HOME=/srv/hfdesk "` + newExecutable + `"` + tail
	if !strings.Contains(string(content), want) {
		t.Errorf("the updated unit does not hold the promised command %q:\n%s", want, content)
	}
	if strings.Contains(string(content), oldExecutable) {
		t.Errorf("the updated unit still names the old program %q:\n%s", oldExecutable, content)
	}
	for _, kept := range []string{
		"WorkingDirectory=" + previous.Paths.StateDir,
		"Restart=on-failure",
		"Environment=HOME=",
		"--listen " + previous.Listen,
		"--cache-dir " + previous.CacheDir,
	} {
		if !strings.Contains(string(content), kept) {
			t.Errorf("the env-wrapped update lost %q:\n%s", kept, content)
		}
	}
	calls := systemdSupervisorCalls(t, logPath)
	for _, command := range []string{"restart", "start"} {
		if count := systemdSupervisorCallsTo(calls, command, unitName); count != 0 {
			t.Errorf("a stopped instance was %sed %d times:\n%s", command, count, strings.Join(calls, "\n"))
		}
	}
}

// TestSystemdUpdateOfARunningEnvWrappedExecStartPassesVerification checks
// the verification a stopped instance never reaches: a running registration
// whose command carries a recognized "env" wrapper is rewritten onto the new
// program, restarted, and accepted. The supervisor reports the wrapper as the
// path it executes, so the verification must compare the program slot of the
// registered command — comparing the wrapper path would burn the wait, report
// a failure and roll the update back although env really started the new
// program.
func TestSystemdUpdateOfARunningEnvWrappedExecStartPassesVerification(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	previous.Listen = "127.0.0.1:" + strconv.Itoa(freePort(t))
	for _, dir := range []string{filepath.Dir(previous.Paths.ConfigFile), previous.Paths.StateDir, previous.Paths.LogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	_, tail, _ := splitRenderedExecStart(t, previousUnit, oldExecutable)
	wrapped := systemdWrapExecStart(t, previousUnit, oldExecutable, "/usr/bin/env HF_HOME=/srv/hfdesk "+oldExecutable+tail)
	for _, name := range []string{unitPath, os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")} {
		if err := os.WriteFile(name, []byte(wrapped), 0o644); err != nil {
			t.Fatalf("write the wrapped unit %q: %v", name, err)
		}
	}
	process := startSystemdRegisteredProcess(t, previous, "fixture-version")
	restartCount := filepath.Join(root, "restart-count")
	systemdRuntimePropertyReader(t, m, process.Process.Pid, process.Process.Pid, restartCount)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_COUNT", restartCount)
	previousProbe := serviceReadinessProbe
	serviceReadinessProbe = probeServiceReady
	t.Cleanup(func() { serviceReadinessProbe = previousProbe })
	previousInstallProbe := serviceInstallProbe
	var probedListen []string
	serviceInstallProbe = func(listen, expectedVersion string) (bool, error) {
		probedListen = append(probedListen, listen)
		return previousInstallProbe(listen, expectedVersion)
	}
	t.Cleanup(func() { serviceInstallProbe = previousInstallProbe })
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)
	// The supervisor reports the loaded command of the rewrite: the
	// wrapper path with the new program behind it.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXEC_START", "/usr/bin/env HF_HOME=/srv/hfdesk "+newExecutable+tail)

	if _, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable)); err != nil {
		t.Fatalf("Install() = %v, want the running env-wrapped update accepted without rollback", err)
	}
	if !slices.Equal(probedListen, []string{previous.Listen}) {
		t.Errorf("readiness probes = %v, want the registered address %q", probedListen, previous.Listen)
	}

	content, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read the updated unit: %v", err)
	}
	want := `ExecStart=/usr/bin/env HF_HOME=/srv/hfdesk "` + newExecutable + `"` + tail
	if !strings.Contains(string(content), want) {
		t.Errorf("the updated unit does not hold the promised command %q:\n%s", want, content)
	}
	if strings.Contains(string(content), oldExecutable) {
		t.Errorf("the updated unit still names the old program %q:\n%s", oldExecutable, content)
	}
	// A failed verification would restore the previous registration and
	// restart the previous instance: exactly one restart is the update.
	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 1 {
		t.Errorf("the unit was restarted %d times, want exactly once for the accepted update:\n%s", got, strings.Join(calls, "\n"))
	}
}

// TestSystemdWrapperRegistrationReportsPreservedRunFlags checks that the
// options read back from a recognized wrapper form are the flags the
// instance really runs with: registrationOptions and the effective options
// of an update report the values behind the "env" prefix, the words of the
// prefix itself are never mistaken for run flags, and a dynamic run flag
// behind the wrapper is still reported as dynamic.
func TestSystemdWrapperRegistrationReportsPreservedRunFlags(t *testing.T) {
	t.Run("registrationOptions reads the run flags behind the wrapper", func(t *testing.T) {
		unit := `[Service]
ExecStart=/usr/bin/env -i HF_ARGS=--listen HF_HOME=/srv/hfdesk /opt/hfdesk/hfdesk service run --config /srv/etc/hfdesk.json --state-dir /srv/state --log-dir /srv/log --listen 127.0.0.1:19090 --cache-dir /srv/cache --hub-dir /srv/cache/hub --local-dir /srv/models
WorkingDirectory=/srv/state
`
		got := registrationOptions(parseUnit(unit), ScopeUser)
		if got.Listen != "127.0.0.1:19090" {
			t.Errorf("listen = %q, want the registered 127.0.0.1:19090; the --listen-shaped word of the env prefix must not be read as a run flag", got.Listen)
		}
		wantPaths := runtimepaths.Paths{
			ConfigFile: "/srv/etc/hfdesk.json",
			StateDir:   "/srv/state",
			LogDir:     "/srv/log",
		}
		if got.Paths != wantPaths {
			t.Errorf("paths = %+v, want the registered %+v", got.Paths, wantPaths)
		}
		for _, pair := range []struct {
			name string
			got  string
			want string
		}{
			{"cache root", got.CacheDir, "/srv/cache"},
			{"hub dir", got.HubDir, "/srv/cache/hub"},
			{"local dir", got.LocalDir, "/srv/models"},
		} {
			if pair.got != pair.want {
				t.Errorf("%s = %q, want the registered %q", pair.name, pair.got, pair.want)
			}
		}
		if got.Executable != "/opt/hfdesk/hfdesk" {
			t.Errorf("executable = %q, want the program of the wrapped command, not the wrapper token", got.Executable)
		}
	})

	t.Run("a dynamic run flag behind the wrapper stays dynamic", func(t *testing.T) {
		unit := "[Service]\nExecStart=/usr/bin/env HF_HOME=%h /opt/hfdesk/hfdesk service run --config /srv/etc/hfdesk.json --state-dir %h/state --log-dir /srv/log --listen 127.0.0.1:19090\n"
		parsed := parseUnit(unit)
		if !parsed.DynamicRunArgs["state-dir"] {
			t.Errorf("DynamicRunArgs = %v, want the dynamic --state-dir behind the wrapper", parsed.DynamicRunArgs)
		}
		if parsed.DynamicRunArgs["listen"] {
			t.Errorf("DynamicRunArgs = %v, want the static --listen not marked dynamic", parsed.DynamicRunArgs)
		}
		if got := registrationOptions(parsed, ScopeUser).Listen; got != "127.0.0.1:19090" {
			t.Errorf("listen = %q, want the static address registered next to the dynamic flag", got)
		}
	})

	t.Run("an update reports the preserved registration", func(t *testing.T) {
		logPath := systemdRecordingSupervisor(t)
		root := t.TempDir()
		oldExecutable := systemdTestExecutable(t)
		newExecutable := systemdTestExecutable(t)
		previous := systemdExistingOptions(t, oldExecutable, root)
		m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
		_, tail, _ := splitRenderedExecStart(t, previousUnit, oldExecutable)
		wrapped := systemdWrapExecStart(t, previousUnit, oldExecutable, "/usr/bin/env HF_HOME=/srv/hfdesk hfdesk"+tail)
		for _, name := range []string{unitPath, os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")} {
			if err := os.WriteFile(name, []byte(wrapped), 0o644); err != nil {
				t.Fatalf("write the wrapped unit %q: %v", name, err)
			}
		}
		t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")
		t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)

		effective, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
		if err != nil {
			t.Fatalf("Install() = %v, want the update of the wrapped unit", err)
		}
		want := previous
		want.Executable = newExecutable
		if !reflect.DeepEqual(effective, want) {
			t.Errorf("Install() returned\n%+v\nwant the preserved registration\n%+v", effective, want)
		}
		if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 0 {
			t.Errorf("the stopped instance was restarted %d times", got)
		}
	})

	t.Run("the status names the program of the wrapped command", func(t *testing.T) {
		status := systemdStatusOf(t, "/usr/bin/env", "HF_HOME=/srv/hfdesk", "/bin/true", "service", "run", "--listen", "127.0.0.1:19090")
		if status.Executable != "/bin/true" {
			t.Errorf("the executable = %q, want the program %q behind the wrapper, not the wrapper token", status.Executable, "/bin/true")
		}
	})
}

type systemdLosslessUpdateFixture struct {
	logPath       string
	manager       *systemdManager
	unitPath      string
	previousUnit  string
	previous      InstallOptions
	updated       InstallOptions
	oldProperties map[string]dbus.Variant
	newProperties map[string]dbus.Variant
}

func newSystemdLosslessUpdateFixture(t *testing.T) systemdLosslessUpdateFixture {
	t.Helper()
	logPath := systemdRecordingSupervisor(t)
	root := filepath.Join(t.TempDir(), `root with "spaces"`)
	makeExecutable := func(name string) string {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	oldExecutable := makeExecutable(filepath.Join(`bin old "quoted"`, "hfdesk"))
	newExecutable := makeExecutable(filepath.Join(`bin new "quoted"`, "hfdesk"))
	previous := systemdExistingOptions(t, oldExecutable, root)
	previous.Listen = "127.0.0.1:" + strconv.Itoa(freePort(t))
	previous.Paths.ConfigFile = filepath.Join(root, `config "quoted" with spaces`, "hfdesk.json")
	previous.Paths.StateDir = filepath.Join(root, `state "quoted" with spaces`)
	previous.Paths.LogDir = filepath.Join(root, `logs "quoted" with spaces`)
	previous.CacheDir = filepath.Join(root, `cache "quoted" with spaces`)
	previous.HubDir = filepath.Join(previous.CacheDir, `hub "quoted"`)
	previous.LocalDir = ""
	manager, unitPath, previousUnit := systemdExistingInstallation(t, previous)

	command := "ExecStart=" + systemdExecStart(previous)
	withEmptyArgument := command + ` --local-dir ""`
	previousUnit = strings.Replace(previousUnit, command, withEmptyArgument, 1)
	environmentLine := `Environment=HFDSK_TEST_LABEL="value with spaces and \"quotes\""`
	previousUnit = strings.Replace(previousUnit, "Environment=HOME=", environmentLine+"\nEnvironment=HOME=", 1)
	previousUnit = strings.Replace(previousUnit, "\n\n[Install]", "\nEnvironmentFile=-/etc/hfdesk/service.env\n\n[Install]", 1)
	loadedPath := os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")
	for _, path := range []string{unitPath, loadedPath} {
		if err := os.WriteFile(path, []byte(previousUnit), 0o644); err != nil {
			t.Fatalf("write the independently-authored unit %q: %v", path, err)
		}
	}

	home, err := unitHomeDir(previous)
	if err != nil {
		t.Fatalf("unitHomeDir() = %v", err)
	}
	environment := []string{`HFDSK_TEST_LABEL=value with spaces and "quotes"`, "HOME=" + home}
	environmentFiles := []systemdEnvironmentFile{{Path: "/etc/hfdesk/service.env", IgnoreErrors: true}}
	oldArgv := append([]string{oldExecutable}, RunArgs(previous)...)
	oldArgv = append(oldArgv, "--local-dir", "")
	updated := previous
	updated.Executable = newExecutable
	newArgv := append([]string{newExecutable}, RunArgs(updated)...)
	newArgv = append(newArgv, "--local-dir", "")
	return systemdLosslessUpdateFixture{
		logPath:       logPath,
		manager:       manager,
		unitPath:      unitPath,
		previousUnit:  previousUnit,
		previous:      previous,
		updated:       updated,
		oldProperties: systemdTypedProperties(oldExecutable, oldArgv, environment, previous.Paths.StateDir, "", "", environmentFiles),
		newProperties: systemdTypedProperties(newExecutable, newArgv, environment, previous.Paths.StateDir, "", "", environmentFiles),
	}
}

type systemdDynamicPathFixture struct {
	logPath            string
	manager            *systemdManager
	unitPath           string
	loadedPath         string
	root               string
	sourceUnit         string
	configSource       string
	stateSource        string
	cacheSource        string
	hubSource          string
	localSource        string
	logDir             string
	listen             string
	managerState       string
	managerWorkingDir  string
	newExecutable      string
	explicitConfig     string
	oldProperties      map[string]dbus.Variant
	newProperties      map[string]dbus.Variant
	explicitProperties map[string]dbus.Variant
}

func newSystemdDynamicPathFixture(t *testing.T) systemdDynamicPathFixture {
	t.Helper()
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	configSource := filepath.Join(root, "${HF_CONFIG}", "hfdesk.json")
	actualConfig := filepath.Join(root, "config with spaces", "hfdesk.json")
	if err := os.MkdirAll(filepath.Dir(actualConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(actualConfig, []byte(`{"connections":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	actualCache := filepath.Join(root, "cache with spaces")
	logDir := filepath.Join(root, "%n", "logs")
	managerLogDir := filepath.Join(root, unitName, "logs")
	managerHome := filepath.Join(root, "manager home")
	managerWorkingDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("read the user home for the %%h fixture: %v", err)
	}
	managerState := filepath.Join(managerWorkingDir, "state with spaces")
	listen := "127.0.0.1:19090"
	stateSource := `%h/state with spaces`
	cacheSource := `${HF_CACHE}`
	hubSource := `${HF_HUB}`
	localSource := `${HF_LOCAL}`
	previous := systemdExistingOptions(t, "/bin/true", root)
	manager, unitPath, _ := systemdExistingInstallation(t, previous)
	sourceUnit := "[Unit]\nDescription=HFDesk\n\n[Service]\n" +
		"ExecStart=/bin/true service run --config \"" + configSource + "\"" +
		" --state-dir=\"" + stateSource + "\" --log-dir=" + logDir +
		" --listen=" + listen + " --cache-dir=" + cacheSource +
		" --hub-dir=" + hubSource + " --local-dir=" + localSource + "\n" +
		"WorkingDirectory=%h\n" +
		"Environment=HF_CONFIG=\"config with spaces\"\n" +
		"Environment=HF_CACHE=\"" + actualCache + "\"\n" +
		"Environment=HF_HUB=\"" + filepath.Join(root, "hub with spaces") + "\"\n" +
		"Environment=HF_LOCAL=\"" + filepath.Join(root, "models with spaces") + "\"\n" +
		"Environment=HOME=\"" + managerHome + "\"\n" +
		"Restart=on-failure\n\n[Install]\nWantedBy=default.target\n"
	loadedPath := os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")
	for _, path := range []string{unitPath, loadedPath} {
		if err := os.WriteFile(path, []byte(sourceUnit), 0o644); err != nil {
			t.Fatalf("write the dynamic source fixture %q: %v", path, err)
		}
	}
	newExecutable := systemdTestExecutable(t)
	explicitConfig := filepath.Join(root, "explicit config", "hfdesk.json")
	environment := []string{
		"HF_CONFIG=config with spaces",
		"HF_CACHE=" + actualCache,
		"HF_HUB=" + filepath.Join(root, "hub with spaces"),
		"HF_LOCAL=" + filepath.Join(root, "models with spaces"),
		"HOME=" + managerHome,
	}
	environmentFiles := []systemdEnvironmentFile{}
	oldArgv := []string{
		"/bin/true", "service", "run", "--config", configSource,
		"--state-dir=" + managerState, "--log-dir=" + managerLogDir,
		"--listen=" + listen, "--cache-dir=" + cacheSource,
		"--hub-dir=" + hubSource, "--local-dir=" + localSource,
	}
	newArgv := slices.Clone(oldArgv)
	newArgv[0] = newExecutable
	explicitArgv := []string{
		newExecutable, "service", "run", "--config", explicitConfig,
		"--state-dir=" + managerState, "--log-dir=" + managerLogDir,
		"--listen=" + listen, "--cache-dir=" + cacheSource,
		"--hub-dir=" + hubSource, "--local-dir=" + localSource,
	}
	return systemdDynamicPathFixture{
		logPath:            logPath,
		manager:            manager,
		unitPath:           unitPath,
		loadedPath:         loadedPath,
		root:               root,
		sourceUnit:         sourceUnit,
		configSource:       configSource,
		stateSource:        stateSource,
		cacheSource:        cacheSource,
		hubSource:          hubSource,
		localSource:        localSource,
		logDir:             logDir,
		listen:             listen,
		managerState:       managerState,
		managerWorkingDir:  managerWorkingDir,
		newExecutable:      newExecutable,
		explicitConfig:     explicitConfig,
		oldProperties:      systemdTypedProperties("/bin/true", oldArgv, environment, managerWorkingDir, "", "", environmentFiles),
		newProperties:      systemdTypedProperties(newExecutable, newArgv, environment, managerWorkingDir, "", "", environmentFiles),
		explicitProperties: systemdTypedProperties(newExecutable, explicitArgv, environment, managerWorkingDir, "", "", environmentFiles),
	}
}

func TestSystemdUpdatePreservesDynamicNativeOperands(t *testing.T) {
	for _, test := range []struct {
		name     string
		unloaded bool
		explicit bool
	}{
		{name: "loaded executable-only"},
		{name: "stored but unloaded executable-only", unloaded: true},
		{name: "loaded explicit config only", explicit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSystemdDynamicPathFixture(t)
			properties := fixture.newProperties
			if test.explicit {
				properties = fixture.explicitProperties
			}
			setSystemdTypedPropertySequence(t, fixture.manager, fixture.oldProperties, properties)
			if test.unloaded {
				if err := os.Remove(fixture.loadedPath); err != nil {
					t.Fatalf("mark the stored unit as not loaded: %v", err)
				}
			}
			t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

			requested := DefaultInstallOptions(ScopeUser, fixture.newExecutable)
			if test.explicit {
				requested.Paths.ConfigFile = fixture.explicitConfig
				requested.Explicit.Config = true
			}
			installed, err := fixture.manager.Install(context.Background(), requested)
			if err != nil {
				t.Fatalf("Install() = %v, want the native dynamic operands to survive", err)
			}
			wantConfig := fixture.configSource
			if test.explicit {
				wantConfig = fixture.explicitConfig
			}
			if installed.Executable != fixture.newExecutable || installed.Paths.ConfigFile != wantConfig || installed.Paths.StateDir != fixture.stateSource || installed.Paths.LogDir != fixture.logDir || installed.CacheDir != fixture.cacheSource || installed.HubDir != fixture.hubSource || installed.LocalDir != fixture.localSource {
				t.Errorf("Install() = %+v, want executable %q and source-preserved config/state/cache operands", installed, fixture.newExecutable)
			}
			updated, err := os.ReadFile(fixture.unitPath)
			if err != nil {
				t.Fatal(err)
			}
			text := string(updated)
			for _, preserved := range []string{
				"--state-dir=\"" + fixture.stateSource + "\"",
				"--log-dir=" + fixture.logDir,
				"--cache-dir=" + fixture.cacheSource,
				"--hub-dir=" + fixture.hubSource,
				"--local-dir=" + fixture.localSource,
				"WorkingDirectory=%h",
				"Environment=HF_CONFIG=\"config with spaces\"",
				"Environment=HF_CACHE=\"" + filepath.Join(fixture.root, "cache with spaces") + "\"",
				"Environment=HF_HUB=\"" + filepath.Join(fixture.root, "hub with spaces") + "\"",
				"Environment=HF_LOCAL=\"" + filepath.Join(fixture.root, "models with spaces") + "\"",
			} {
				if !strings.Contains(text, preserved) {
					t.Errorf("updated source lost unresupplied native operand/setting %q:\n%s", preserved, text)
				}
			}
			if !test.explicit && !strings.Contains(text, "--config \""+fixture.configSource+"\"") {
				t.Errorf("unmarked config expression was re-encoded or replaced:\n%s", text)
			}
			if test.explicit {
				if strings.Contains(text, fixture.configSource) || !strings.Contains(text, systemdQuoteCommand(fixture.explicitConfig)) {
					t.Errorf("explicit config resupply did not affect only the config operand:\n%s", text)
				}
			}
			for _, unresolved := range []string{"${HF_CONFIG}", "${HF_CACHE}", "${HF_HUB}", "${HF_LOCAL}", "%h", "%n"} {
				if _, err := os.Stat(filepath.Join(fixture.root, unresolved)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("update created a directory for unresolved expression %q: %v", unresolved, err)
				}
			}
			if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, fixture.logPath), "restart", unitName); got != 0 {
				t.Errorf("stopped/unloaded registration was restarted %d times", got)
			}
		})
	}
}

func TestSystemdDynamicLoadedMismatchRollsBackSource(t *testing.T) {
	fixture := newSystemdDynamicPathFixture(t)
	changedState := filepath.Join(fixture.root, "different manager state")
	changedArgv := []string{
		fixture.newExecutable, "service", "run", "--config", fixture.configSource,
		"--state-dir=" + changedState,
		"--log-dir=" + filepath.Join(fixture.root, unitName, "logs"),
		"--listen=" + fixture.listen, "--cache-dir=" + fixture.cacheSource,
		"--hub-dir=" + fixture.hubSource, "--local-dir=" + fixture.localSource,
	}
	changed := systemdTypedProperties(fixture.newExecutable, changedArgv, []string{
		"HF_CONFIG=config with spaces",
		"HF_CACHE=" + filepath.Join(fixture.root, "cache with spaces"),
		"HF_HUB=" + filepath.Join(fixture.root, "hub with spaces"),
		"HF_LOCAL=" + filepath.Join(fixture.root, "models with spaces"),
		"HOME=" + filepath.Join(fixture.root, "manager home"),
	}, fixture.managerWorkingDir, "", "", []systemdEnvironmentFile{})
	setSystemdTypedPropertySequence(t, fixture.manager, fixture.oldProperties, changed, fixture.oldProperties)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

	_, err := fixture.manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, fixture.newExecutable))
	if err == nil || !strings.Contains(err.Error(), "typed --state-dir argument changed across the update") {
		t.Fatalf("Install() error = %v, want rejection of the changed manager-loaded dynamic path", err)
	}
	content, readErr := os.ReadFile(fixture.unitPath)
	if readErr != nil || string(content) != fixture.sourceUnit {
		t.Errorf("restored source (error %v) =\n%s\nwant original dynamic source\n%s", readErr, content, fixture.sourceUnit)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, fixture.logPath), "restart", unitName); got != 0 {
		t.Errorf("stopped unit was restarted %d times during rollback", got)
	}
}

func TestSystemdUpdateUsesTypedArgvForSpacesQuotesEmptyArgsAndEnvironment(t *testing.T) {
	fixture := newSystemdLosslessUpdateFixture(t)
	setSystemdTypedPropertySequence(t, fixture.manager, fixture.oldProperties, fixture.newProperties)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

	effective, err := fixture.manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, fixture.updated.Executable))
	if err != nil {
		t.Fatalf("Install() = %v, want a full update with typed argv", err)
	}
	if !reflect.DeepEqual(effective, fixture.updated) {
		t.Errorf("Install() = %+v, want %+v", effective, fixture.updated)
	}
	content, err := os.ReadFile(fixture.unitPath)
	if err != nil {
		t.Fatal(err)
	}
	registered := parseUnit(string(content))
	if registered.ExecStart != fixture.updated.Executable || registered.flagValue("--config") != fixture.previous.Paths.ConfigFile {
		t.Errorf("updated registration = %+v; config path with spaces/quotes was not preserved", registered)
	}
	for _, preserved := range []string{
		`Environment=HFDSK_TEST_LABEL="value with spaces and \"quotes\""`,
		"EnvironmentFile=-/etc/hfdesk/service.env",
	} {
		if !strings.Contains(string(content), preserved) {
			t.Errorf("updated unit lost preserved setting %q:\n%s", preserved, content)
		}
	}
	if calls := systemdSupervisorCalls(t, fixture.logPath); systemdSupervisorCallsTo(calls, "restart", unitName) != 0 {
		t.Errorf("the stopped unit was restarted:\n%s", strings.Join(calls, "\n"))
	}
}

func TestSystemdRollbackUsesTypedArgvForSpacesQuotesEmptyArgsAndEnvironment(t *testing.T) {
	fixture := newSystemdLosslessUpdateFixture(t)
	previousProbe := serviceReadinessProbe
	serviceReadinessProbe = probeServiceReady
	t.Cleanup(func() { serviceReadinessProbe = previousProbe })
	process := startSystemdRegisteredProcess(t, fixture.previous, "fixture-version")
	for _, properties := range []map[string]dbus.Variant{fixture.oldProperties, fixture.newProperties} {
		properties["MainPID"] = dbus.MakeVariant(uint32(process.Process.Pid))
	}
	setSystemdTypedPropertySequence(t, fixture.manager, fixture.oldProperties, fixture.newProperties, fixture.oldProperties)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", fixture.previous.Executable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_FAIL_ONCE", "1")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_MARKER", filepath.Join(t.TempDir(), "restart"))

	_, err := fixture.manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, fixture.updated.Executable))
	if err == nil || !strings.Contains(err.Error(), "the previous registration was restored") {
		t.Fatalf("Install() = %v, want the failed restart and verified typed rollback", err)
	}
	content, readErr := os.ReadFile(fixture.unitPath)
	if readErr != nil || string(content) != fixture.previousUnit {
		t.Errorf("restored unit (error %v) =\n%s\nwant\n%s", readErr, content, fixture.previousUnit)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, fixture.logPath), "restart", unitName); got != 2 {
		t.Errorf("restart count = %d, want the failed update restart and one verified restore", got)
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
	previous.Listen = "127.0.0.1:" + strconv.Itoa(freePort(t))
	for _, dir := range []string{filepath.Dir(previous.Paths.ConfigFile), previous.Paths.StateDir, previous.Paths.LogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	process := startSystemdRegisteredProcess(t, previous, "fixture-version")
	restartCount := filepath.Join(root, "restart-count")
	systemdRuntimePropertyReader(t, m, process.Process.Pid, process.Process.Pid, restartCount)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_COUNT", restartCount)
	previousProbe := serviceReadinessProbe
	serviceReadinessProbe = probeServiceReady
	t.Cleanup(func() { serviceReadinessProbe = previousProbe })
	// The supervisor reports the running state the restore comes back to:
	// the refused restart never reached the verification of the new
	// executable, so the program it reports is the one of the restored
	// registration.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", oldExecutable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_FAIL_ONCE", "1")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_MARKER", filepath.Join(t.TempDir(), "restart"))

	requested := DefaultInstallOptions(ScopeUser, newExecutable)
	requested.Paths.StateDir = filepath.Join(root, "candidate-state")
	requested.Explicit.StateDir = true
	_, err := m.Install(context.Background(), requested)
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
	if _, statErr := os.Stat(requested.Paths.StateDir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("candidate-only state directory %q survived a verified restore: %v", requested.Paths.StateDir, statErr)
	}

	calls := systemdSupervisorCalls(t, logPath)
	// The failed restart and the restart that brings the previous
	// instance back.
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 2 {
		t.Errorf("the unit was restarted %d times, want the failed restart and the restore:\n%s", got, strings.Join(calls, "\n"))
	}
}

func TestSystemdRollbackReloadFailureDoesNotRestartOrClaimRecovery(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	executable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, executable, root)
	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	previousUnit = strings.Replace(previousUnit,
		"Environment=HOME=",
		"Environment=HF_HUB_CACHE=/registered/hub\nEnvironmentFile=-/etc/hfdesk/service.env\nEnvironment=HOME=", 1)
	for _, name := range []string{unitPath, os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")} {
		if err := os.WriteFile(name, []byte(previousUnit), 0o644); err != nil {
			t.Fatalf("write the previous unit %q: %v", name, err)
		}
	}
	oldProperties := systemdTestPropertiesFromUnit(previousUnit)
	oldConfig, err := loadedConfigFromProperties(oldProperties)
	if err != nil {
		t.Fatalf("decode the previous typed manager properties: %v", err)
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_DAEMON_RELOAD_FAIL_AT", "2")

	requested := DefaultInstallOptions(ScopeUser, executable)
	requested.Paths.ConfigFile = filepath.Join(root, "new-config", "hfdesk.json")
	requested.Explicit.Config = true
	updated := previous
	updated.Paths.ConfigFile = requested.Paths.ConfigFile
	updatedArgv := append([]string{executable}, RunArgs(updated)...)
	updatedEnvironment := slices.Clone(oldConfig.environment)
	for i, value := range updatedEnvironment {
		if strings.HasPrefix(value, "HF_HUB_CACHE=") {
			updatedEnvironment[i] = "HF_HUB_CACHE=/operator/new-hub"
		}
	}
	updatedProperties := systemdTypedProperties(executable, updatedArgv, updatedEnvironment, oldConfig.workingDirectory, oldConfig.user, oldConfig.group, oldConfig.environmentFiles)
	setSystemdTypedPropertySequence(t, m, oldProperties, updatedProperties)
	_, err = m.Install(context.Background(), requested)
	if err == nil {
		t.Fatal("Install() = nil, want the typed environment mismatch and unconfirmed rollback")
	}
	for _, want := range []string{"did not preserve the loaded environment", "reload the restored unit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install() = %q, want the original/rollback failure %q", err, want)
		}
	}
	for _, unsupported := range []string{"captured loaded command/launch settings were restored", "previous instance was started again"} {
		if strings.Contains(err.Error(), unsupported) {
			t.Errorf("Install() = %q, must not claim %q", err, unsupported)
		}
	}

	disk, readErr := os.ReadFile(unitPath)
	if readErr != nil || string(disk) != previousUnit {
		t.Errorf("stored unit after rollback (error %v) =\n%s\nwant\n%s", readErr, disk, previousUnit)
	}
	loadedBytes, readErr := os.ReadFile(os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT"))
	if readErr != nil {
		t.Fatalf("read the supervisor-loaded unit: %v", readErr)
	}
	loaded := parseUnit(string(loadedBytes))
	if loaded.ExecStart != executable || loaded.listen() != previous.Listen || loaded.flagValue("--config") == previous.Paths.ConfigFile {
		t.Errorf("the supervisor-loaded command/environment do not distinguish the failed update: %+v", loaded)
	}
	loadedConfig, err := loadedConfigFromProperties(updatedProperties)
	if err != nil {
		t.Fatalf("decode the still-loaded typed manager properties: %v", err)
	}
	if loadedConfig.execPath != executable || runArgValue(loadedConfig.execArgs[1:], "--listen") != previous.Listen || runArgValue(loadedConfig.execArgs[1:], "--config") == previous.Paths.ConfigFile || slices.Equal(loadedConfig.environment, oldConfig.environment) {
		t.Errorf("typed loaded state = %+v, want same executable/listen but changed config and environment", loadedConfig)
	}
	if info, statErr := os.Stat(filepath.Dir(requested.Paths.ConfigFile)); statErr != nil || !info.IsDir() {
		t.Errorf("candidate config directory %q was removed while the supervisor still has its command loaded: %v", filepath.Dir(requested.Paths.ConfigFile), statErr)
	}

	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 0 {
		t.Errorf("the instance was restarted %d times, want no restart while the typed loaded state is unconfirmed:\n%s", got, strings.Join(calls, "\n"))
	}
}

func TestSystemdUpdateWithUnavailableTypedPropertiesRefusesBeforeMutation(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	m.readServiceProperties = func(context.Context, string) (map[string]dbus.Variant, error) {
		return nil, errors.New("fake typed property read failed")
	}

	_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err == nil || !strings.Contains(err.Error(), "fake typed property read failed") {
		t.Fatalf("Install() = %v, want the typed-read failure", err)
	}
	content, readErr := os.ReadFile(unitPath)
	if readErr != nil || string(content) != previousUnit {
		t.Errorf("unit file changed without typed loaded settings (error %v):\n%s", readErr, content)
	}
	if calls := systemdSupervisorCalls(t, logPath); systemdSupervisorCallsTo(calls, "daemon-reload") != 0 {
		t.Errorf("the refused update touched systemd after the typed read failed: %v", calls)
	}
}

// TestSystemdReinstallRefusesAShadowedLoadedCommand checks that a registration
// whose effective loaded command differs from its unit file is not updated by
// silently changing only the base file. The fake keeps the same executable and
// listen address but gives the loaded command a different config and environment.
func TestSystemdReinstallRefusesAShadowedLoadedCommand(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)

	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	overrideConfig := filepath.Join(root, "administrator", "hfdesk.json")
	loadedUnit := strings.Replace(previousUnit,
		"--config "+previous.Paths.ConfigFile,
		"--config "+overrideConfig, 1)
	if loadedUnit == previousUnit {
		t.Fatal("the generated command did not contain the config path to override")
	}
	loadedUnit = strings.Replace(loadedUnit, "\n\n[Install]", "\nEnvironment=HF_HUB_CACHE=/operator/hub\n\n[Install]", 1)
	loadedPath := os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")
	if err := os.WriteFile(loadedPath, []byte(loadedUnit), 0o644); err != nil {
		t.Fatalf("write the simulated loaded unit: %v", err)
	}
	loadedArgv := append([]string{oldExecutable}, RunArgs(previous)...)
	for i, arg := range loadedArgv {
		if arg == previous.Paths.ConfigFile {
			loadedArgv[i] = overrideConfig
			break
		}
	}
	home, err := unitHomeDir(previous)
	if err != nil {
		t.Fatalf("unitHomeDir() = %v", err)
	}
	loadedEnvironment := []string{"HF_HUB_CACHE=/operator/hub", "HOME=" + home}
	loadedProperties := systemdTypedProperties(oldExecutable, loadedArgv, loadedEnvironment, previous.Paths.StateDir, "", "", nil)
	m.readServiceProperties = func(context.Context, string) (map[string]dbus.Variant, error) {
		return loadedProperties, nil
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")

	_, err = m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err == nil {
		t.Fatal("Install() = nil, want refusal of the shadowed loaded command")
	}
	if !strings.Contains(err.Error(), "differs from its unit file") {
		t.Errorf("Install() = %q, want an error that identifies the loaded command mismatch", err)
	}

	content, readErr := os.ReadFile(unitPath)
	if readErr != nil {
		t.Fatalf("read the unit file: %v", readErr)
	}
	if string(content) != previousUnit {
		t.Errorf("the base unit changed despite the override refusal:\n%s\nwant\n%s", content, previousUnit)
	}
	loaded, readErr := os.ReadFile(loadedPath)
	if readErr != nil || string(loaded) != loadedUnit {
		t.Errorf("the simulated loaded unit changed after refusal (error %v):\n%s\nwant\n%s", readErr, loaded, loadedUnit)
	}
	registered := parseUnit(string(loaded))
	if registered.ExecStart != oldExecutable || registered.listen() != previous.Listen || registered.flagValue("--config") != overrideConfig || registered.Environment["HF_HUB_CACHE"] != "/operator/hub" {
		t.Errorf("the loaded override does not retain its distinct config/environment: %+v", registered)
	}
	typed, err := loadedConfigFromProperties(loadedProperties)
	if err != nil {
		t.Fatalf("decode the typed override: %v", err)
	}
	if typed.execPath != oldExecutable || runArgValue(typed.execArgs[1:], "--listen") != previous.Listen || runArgValue(typed.execArgs[1:], "--config") != overrideConfig || !slices.Equal(typed.environment, loadedEnvironment) {
		t.Errorf("typed loaded override = %+v, want same executable/listen with distinct config/environment", typed)
	}

	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 0 {
		t.Errorf("the refused update restarted the unit %d times:\n%s", got, strings.Join(calls, "\n"))
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
	previous.Listen = "127.0.0.1:" + strconv.Itoa(freePort(t))

	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	process := startSystemdRegisteredProcess(t, previous, "fixture-version")
	restartCount := filepath.Join(t.TempDir(), "restarts")
	systemdRuntimePropertyReader(t, m, process.Process.Pid, process.Process.Pid, restartCount)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_COUNT", restartCount)
	// The supervisor reports what a restarted simple unit reports: the unit
	// is active with the program it was started with, and the restart of the
	// restore brings the previous program back.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)
	m.restartWait = 250 * time.Millisecond

	// The process fixture provides a real MainPID and argv. The readiness seam
	// fails the first probe and accepts the restore to exercise rollback reporting.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE_AFTER_SECOND_RESTART", oldExecutable)
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

func TestSystemdDynamicListenUpdateUsesTheRunningProcessArgv(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	realProbe := serviceReadinessProbe
	serviceReadinessProbe = probeServiceReady
	t.Cleanup(func() { serviceReadinessProbe = realProbe })

	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	manager, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	listen := "127.0.0.1:" + strconv.Itoa(freePort(t))
	dynamicPreviousUnit := systemdDynamicListenUnit(t, unitPath, previousUnit, previous.Listen, "Environment=HF_LISTEN="+listen)
	if !strings.Contains(dynamicPreviousUnit, "${HF_LISTEN}") {
		t.Fatal("the previous unit did not retain the dynamic Listen source")
	}
	process := startSystemdRuntimeProcess(t, filepath.Join(root, "runtime"), listen, "fixture-version")
	systemdRuntimePropertyReader(t, manager, process.Process.Pid, process.Process.Pid, filepath.Join(root, "restart-count"))
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_COUNT", filepath.Join(root, "restart-count"))

	loaded, err := manager.registrationUnitConfig(context.Background())
	if err != nil {
		t.Fatalf("registrationUnitConfig() = %v", err)
	}
	if got := runArgValue(loaded.execArgs[1:], "--listen"); got != "${HF_LISTEN}" {
		t.Fatalf("loaded ExecStartEx Listen = %q, want unresolved source expression", got)
	}
	runtimeOptions, _, err := manager.runtimeRunOptions(context.Background())
	if err != nil {
		t.Fatalf("runtimeRunOptions() = %v", err)
	}
	if runtimeOptions.Listen != listen {
		t.Fatalf("runtime argv Listen = %q, want effective process address %q", runtimeOptions.Listen, listen)
	}

	if _, err := manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable)); err != nil {
		t.Fatalf("Install() = %v, want the actual HFDesk process readiness probe to pass", err)
	}
	installed, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installed), "${HF_LISTEN}") || !strings.Contains(string(installed), "Environment=HF_LISTEN="+listen) {
		t.Errorf("updated source did not preserve the dynamic Listen expression and Environment value:\n%s", installed)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 1 {
		t.Errorf("restart count = %d, want one update restart", got)
	}
}

func TestSystemdDynamicListenRollbackProbesTheRestoredProcessAddress(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	realProbe := serviceReadinessProbe
	serviceReadinessProbe = probeServiceReady
	t.Cleanup(func() { serviceReadinessProbe = realProbe })
	realInstallProbe := serviceInstallProbe
	var probed []string
	serviceInstallProbe = func(listen, expectedVersion string) (bool, error) {
		probed = append(probed, listen)
		return realInstallProbe(listen, expectedVersion)
	}
	t.Cleanup(func() { serviceInstallProbe = realInstallProbe })

	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	manager, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	oldListen := "127.0.0.1:" + strconv.Itoa(freePort(t))
	newListen := "127.0.0.1:" + strconv.Itoa(freePort(t))
	environmentFile := filepath.Join(root, "hfdesk.env")
	if err := os.WriteFile(environmentFile, []byte("HF_LISTEN="+oldListen+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dynamicPreviousUnit := systemdDynamicListenUnit(t, unitPath, previousUnit, previous.Listen, "EnvironmentFile="+environmentFile)
	oldProcess := startSystemdRuntimeProcess(t, filepath.Join(root, "old-runtime"), oldListen, "old-version")
	if err := os.WriteFile(environmentFile, []byte("HF_LISTEN="+newListen+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newProcess := startSystemdRuntimeProcess(t, filepath.Join(root, "new-runtime"), newListen, "candidate-version")
	restartCount := filepath.Join(root, "restart-count")
	systemdRuntimePropertyReader(t, manager, oldProcess.Process.Pid, newProcess.Process.Pid, restartCount)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_COUNT", restartCount)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE_AFTER_SECOND_RESTART", oldExecutable)

	requested := DefaultInstallOptions(ScopeUser, newExecutable)
	requested.ExpectedVersion = "old-version"
	if _, err := manager.Install(context.Background(), requested); err == nil {
		t.Fatal("Install() = nil, want the candidate version mismatch and rollback")
	} else if !strings.Contains(err.Error(), "does not run the expected version") {
		t.Errorf("Install() = %v, want the candidate version mismatch", err)
	}
	if !slices.Equal(probed, []string{newListen, oldListen}) {
		t.Errorf("readiness probes used addresses %v, want candidate %q followed by restored %q", probed, newListen, oldListen)
	}
	if versionMatches, err := probeServiceReadyForVersion(oldListen, "old-version"); err != nil || !versionMatches {
		t.Errorf("restored old-version process probe = (%t, %v), want a verified old process", versionMatches, err)
	}
	content, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != dynamicPreviousUnit || !strings.Contains(string(content), "${HF_LISTEN}") {
		t.Errorf("rollback did not restore the original dynamic source:\n%s\nwant:\n%s", content, dynamicPreviousUnit)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 2 {
		t.Errorf("restart count = %d, want update and restore restarts", got)
	}
}

func TestSystemdDynamicListenDoesNotProbeAnUnobservableSourceValue(t *testing.T) {
	_ = systemdRecordingSupervisor(t)
	executable := systemdTestExecutable(t)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", executable)
	previousProbe := serviceInstallProbe
	t.Cleanup(func() { serviceInstallProbe = previousProbe })
	probeCalls := 0
	serviceInstallProbe = func(string, string) (bool, error) {
		probeCalls++
		return true, nil
	}
	manager := &systemdManager{
		scope:       ScopeUser,
		restartWait: 20 * time.Millisecond,
		readServiceProperties: func(context.Context, string) (map[string]dbus.Variant, error) {
			return map[string]dbus.Variant{"MainPID": dbus.MakeVariant(uint32(0))}, nil
		},
	}
	_, err := manager.verifyRunningExecutableWithListenSource(context.Background(), executable, "${HF_LISTEN}", "", true)
	if err == nil || !strings.Contains(err.Error(), "runtime process/Listen was not observable and stable") {
		t.Fatalf("dynamic verification error = %v, want precise unobservable-runtime-address failure", err)
	}
	if probeCalls != 0 {
		t.Errorf("readiness probe called %d times without effective process argv; unresolved source must not be treated as ready", probeCalls)
	}
	for _, tc := range []struct {
		name       string
		properties map[string]dbus.Variant
	}{
		{"missing MainPID", map[string]dbus.Variant{}},
		{"zero MainPID", map[string]dbus.Variant{"MainPID": dbus.MakeVariant(uint32(0))}},
		{"wrong MainPID type", map[string]dbus.Variant{"MainPID": dbus.MakeVariant("123")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := systemdMainPIDFromProperties(tc.properties); err == nil {
				t.Fatal("systemdMainPIDFromProperties() succeeded without a usable running process PID")
			}
		})
	}
}

func TestSystemdStaticReadinessRequiresLiveStableMatchingMainPID(t *testing.T) {
	_ = systemdRecordingSupervisor(t)
	previousReadinessProbe := serviceReadinessProbe
	serviceReadinessProbe = probeServiceReady
	t.Cleanup(func() { serviceReadinessProbe = previousReadinessProbe })
	listen := "127.0.0.1:" + strconv.Itoa(freePort(t))
	process := startSystemdRuntimeProcess(t, filepath.Join(t.TempDir(), "runtime"), listen, "fixture-version")
	executable := systemdTestExecutable(t)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", executable)
	previousProbe := serviceInstallProbe
	probeCalls := 0
	serviceInstallProbe = func(listen, expectedVersion string) (bool, error) {
		probeCalls++
		return previousProbe(listen, expectedVersion)
	}
	t.Cleanup(func() { serviceInstallProbe = previousProbe })
	managerWithPID := func(pidAtRead func(int) uint32) *systemdManager {
		reads := 0
		return &systemdManager{
			scope:       ScopeUser,
			restartWait: 20 * time.Millisecond,
			readServiceProperties: func(context.Context, string) (map[string]dbus.Variant, error) {
				reads++
				return map[string]dbus.Variant{"MainPID": dbus.MakeVariant(pidAtRead(reads))}, nil
			},
		}
	}

	for _, tc := range []struct {
		name string
		pid  uint32
	}{
		{"RemainAfterExit unit has no MainPID", 0},
		{"dead MainPID", ^uint32(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probeCalls = 0
			manager := managerWithPID(func(int) uint32 { return tc.pid })
			if _, err := manager.verifyRunningExecutable(context.Background(), executable, listen, "fixture-version"); err == nil || !strings.Contains(err.Error(), "service process could not be verified") {
				t.Fatalf("verification error = %v, want a live-process evidence failure", err)
			}
			if probeCalls != 0 {
				t.Errorf("readiness probe ran %d times without a live MainPID; the healthy bystander must not substitute", probeCalls)
			}
		})
	}

	probeCalls = 0
	if _, err := managerWithPID(func(read int) uint32 {
		if read == 1 {
			return uint32(process.Process.Pid)
		}
		return 0
	}).verifyRunningExecutable(context.Background(), executable, listen, "fixture-version"); err == nil {
		t.Fatal("readiness succeeded after MainPID disappeared during the successful HTTP probe")
	}
	if probeCalls != 1 {
		t.Errorf("readiness probe calls after process disappearance = %d, want one", probeCalls)
	}

	probeCalls = 0
	otherListen := "127.0.0.1:" + strconv.Itoa(freePort(t))
	_, err := managerWithPID(func(int) uint32 { return uint32(process.Process.Pid) }).verifyRunningExecutable(context.Background(), executable, otherListen, "fixture-version")
	if err == nil || !strings.Contains(err.Error(), "does not match the static unit Listen") {
		t.Fatalf("verification error = %v, want the static/runtime Listen mismatch", err)
	}
	if probeCalls != 0 {
		t.Errorf("readiness probe ran %d times against static Listen different from MainPID argv", probeCalls)
	}
}

// TestSystemdVerifyRejectsAnUnintendedWrappedCommand checks the negative side
// of the program comparison behind a wrapper: a wrapper that launches another
// program fails with that program named as the detail, and a wrapper shape
// whose program slot cannot be read fails while naming only the executable it
// found — the wrapper path is evidence, never the program verdict.
func TestSystemdVerifyRejectsAnUnintendedWrappedCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    []string
		wantNot []string
	}{
		{
			name:    "another program behind the wrapper",
			command: "/usr/bin/env HF_HOME=/srv/hfdesk /opt/hfdesk-other/hfdesk service run --listen 127.0.0.1:19090",
			want:    []string{`with the program "/opt/hfdesk-other/hfdesk"`},
		},
		{
			name:    "a wrapper shape without a readable program slot",
			command: "/usr/bin/env bare-word /opt/hfdesk/hfdesk service run --listen 127.0.0.1:19090",
			want:    []string{`with the executable "/usr/bin/env"`, "program slot cannot be read"},
			wantNot: []string{"with the program"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = systemdRecordingSupervisor(t)
			executable := systemdTestExecutable(t)
			t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
			t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", executable)
			t.Setenv("HFDESK_TEST_SYSTEMCTL_EXEC_START", tc.command)
			manager := &systemdManager{scope: ScopeUser, restartWait: 20 * time.Millisecond}

			_, err := manager.verifyRunningExecutable(context.Background(), executable, "127.0.0.1:19090", "fixture-version")
			if err == nil {
				t.Fatal("verifyRunningExecutable() = nil, want the wrapped command rejected")
			}
			for _, want := range append([]string{"does not run", executable}, tc.want...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("verification error = %q, want it to contain %q", err, want)
				}
			}
			for _, not := range tc.wantNot {
				if strings.Contains(err.Error(), not) {
					t.Errorf("verification error = %q, want it not to contain %q", err, not)
				}
			}
		})
	}
}

func TestSystemdReadinessAcceptsLiveScriptWrapperWithHFDeskArgs(t *testing.T) {
	_ = systemdRecordingSupervisor(t)
	root := t.TempDir()
	listen := "127.0.0.1:" + strconv.Itoa(freePort(t))
	paths := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "config", "hfdesk.json"),
		StateDir:   filepath.Join(root, "state"),
		LogDir:     filepath.Join(root, "logs"),
	}
	for _, dir := range []string{filepath.Dir(paths.ConfigFile), paths.StateDir, paths.LogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	serviceOptions := InstallOptions{Scope: ScopeUser, Executable: os.Args[0], Listen: listen, Paths: paths}
	args := append([]string{"-test.run=^TestSystemdRuntimeListenProcessFixture$"}, RunArgs(serviceOptions)...)
	wrapper := filepath.Join(root, "hfdesk-wrapper")
	script := "#!/bin/sh\n\"$@\" &\nchild=$!\ntrap 'kill \"$child\" 2>/dev/null || :; wait \"$child\" 2>/dev/null || :' TERM INT\nwait \"$child\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(wrapper, append([]string{os.Args[0]}, args...)...)
	command.Env = append(os.Environ(),
		"HFDESK_TEST_SYSTEMD_RUNTIME_PROCESS_FIXTURE=1",
		"HFDESK_TEST_SYSTEMD_RUNTIME_VERSION=fixture-version",
	)
	command.Stdout = io.Discard
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start the standalone wrapper process: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			<-done
		}
	})
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", wrapper)
	manager := &systemdManager{
		scope:       ScopeUser,
		restartWait: time.Second,
		readServiceProperties: func(context.Context, string) (map[string]dbus.Variant, error) {
			return map[string]dbus.Variant{"MainPID": dbus.MakeVariant(uint32(command.Process.Pid))}, nil
		},
	}
	verified, err := manager.verifyRunningExecutable(context.Background(), wrapper, listen, "fixture-version")
	if err != nil || !verified {
		t.Fatalf("verifyRunningExecutable() = (%t, %v), want the script wrapper's live HFDesk process; stderr: %s", verified, err, stderr.String())
	}
}

// TestSystemdWrappedRegisteredCommandKeepsTheRuntimeChecks checks the second
// fact of the verification behind a recognized wrapper: when the program slot
// of the registered command matches, the live process still has to be the
// HFDesk instance the readiness probe accepts — a stale process that reports
// another version is rejected — and a caller that verifies the command by its
// raw first token, as the restore of a wrapped registration does, still
// matches the executed wrapper path.
func TestSystemdWrappedRegisteredCommandKeepsTheRuntimeChecks(t *testing.T) {
	setup := func(t *testing.T, processVersion string) (*systemdManager, string, string) {
		t.Helper()
		_ = systemdRecordingSupervisor(t)
		listen := "127.0.0.1:" + strconv.Itoa(freePort(t))
		process := startSystemdRuntimeProcess(t, filepath.Join(t.TempDir(), "runtime"), listen, processVersion)
		executable := systemdTestExecutable(t)
		t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
		t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", executable)
		t.Setenv("HFDESK_TEST_SYSTEMCTL_EXEC_START",
			"/usr/bin/env HF_HOME=/srv/hfdesk "+executable+" service run --listen "+listen)
		manager := &systemdManager{
			scope:       ScopeUser,
			restartWait: time.Second,
			readServiceProperties: func(context.Context, string) (map[string]dbus.Variant, error) {
				return map[string]dbus.Variant{"MainPID": dbus.MakeVariant(uint32(process.Process.Pid))}, nil
			},
		}
		return manager, listen, executable
	}

	t.Run("a stale process that reports another version is rejected", func(t *testing.T) {
		manager, listen, executable := setup(t, "old-version")
		_, err := manager.verifyRunningExecutable(context.Background(), executable, listen, "fixture-version")
		if err == nil || !strings.Contains(err.Error(), "does not run the expected version") {
			t.Fatalf("verification error = %v, want the version rejection of the stale process", err)
		}
	})

	t.Run("the raw first token of the command still matches", func(t *testing.T) {
		manager, listen, _ := setup(t, "fixture-version")
		verified, err := manager.verifyRunningExecutable(context.Background(), "/usr/bin/env", listen, "fixture-version")
		if err != nil || !verified {
			t.Fatalf("verifyRunningExecutable() = (%t, %v), want the raw wrapper-path expectation of the restore path accepted; stderr error: %v", verified, err, err)
		}
	})
}

func TestSystemdRuntimeListenProcessFixture(t *testing.T) {
	if os.Getenv("HFDESK_TEST_SYSTEMD_RUNTIME_PROCESS_FIXTURE") != "1" {
		return
	}
	args := os.Args[1:]
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "service" || args[i+1] != "run" {
			continue
		}
		runOptions, err := ParseRunArgs(args[i:])
		if err != nil {
			t.Fatalf("ParseRunArgs() = %v", err)
		}
		runOptions.Version = os.Getenv("HFDESK_TEST_SYSTEMD_RUNTIME_VERSION")
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		if err := Run(ctx, runOptions); err != nil {
			t.Fatalf("Run() = %v", err)
		}
		return
	}
	t.Fatal("process argv did not contain service run")
}

func startSystemdRuntimeProcess(t *testing.T, root, listen, version string) *exec.Cmd {
	t.Helper()
	paths := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "config", "hfdesk.json"),
		StateDir:   filepath.Join(root, "state"),
		LogDir:     filepath.Join(root, "logs"),
	}
	return startSystemdServiceProcess(t, RunOptions{Paths: paths, Listen: listen}, version)
}

func startSystemdRegisteredProcess(t *testing.T, opts InstallOptions, version string) *exec.Cmd {
	t.Helper()
	for _, dir := range []string{filepath.Dir(opts.Paths.ConfigFile), opts.Paths.StateDir, opts.Paths.LogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("prepare standalone HFDesk paths: %v", err)
		}
	}
	return startSystemdServiceProcess(t, RunOptions{
		Paths:    opts.Paths,
		Listen:   opts.Listen,
		CacheDir: opts.CacheDir,
		HubDir:   opts.HubDir,
		LocalDir: opts.LocalDir,
	}, version)
}

func startSystemdServiceProcess(t *testing.T, options RunOptions, version string) *exec.Cmd {
	t.Helper()
	args := RunArgs(InstallOptions{
		Scope:      ScopeUser,
		Executable: os.Args[0],
		Listen:     options.Listen,
		Paths:      options.Paths,
		CacheDir:   options.CacheDir,
		HubDir:     options.HubDir,
		LocalDir:   options.LocalDir,
	})
	command := append([]string{"-test.run=^TestSystemdRuntimeListenProcessFixture$"}, args...)
	cmd := exec.Command(os.Args[0], command...)
	cmd.Env = append(os.Environ(),
		"HFDESK_TEST_SYSTEMD_RUNTIME_PROCESS_FIXTURE=1",
		"HFDESK_TEST_SYSTEMD_RUNTIME_VERSION="+version,
	)
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start standalone HFDesk process: %v", err)
	}
	done := make(chan struct{})
	var processErr error
	go func() {
		processErr = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("standalone HFDesk process exited before readiness: %v: %s", processErr, stderr.String())
		default:
		}
		if err := probeServiceReady(options.Listen); err == nil {
			return cmd
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("standalone HFDesk process did not serve on %s", options.Listen)
	return nil
}

func systemdDynamicListenUnit(t *testing.T, unitPath, unit, staticListen, environmentDirective string) string {
	t.Helper()
	dynamic := strings.Replace(unit, staticListen, "${HF_LISTEN}", 1)
	if dynamic == unit {
		t.Fatal("existing unit did not contain the static Listen value")
	}
	dynamic = strings.Replace(dynamic, "[Service]\n", "[Service]\n"+environmentDirective+"\n", 1)
	if err := os.WriteFile(unitPath, []byte(dynamic), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT"), []byte(dynamic), 0o644); err != nil {
		t.Fatal(err)
	}
	return dynamic
}

func systemdRuntimePropertyReader(t *testing.T, manager *systemdManager, beforeRestart, afterRestart int, restartCountPath string) {
	t.Helper()
	loadedPath := os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")
	manager.readServiceProperties = func(_ context.Context, name string) (map[string]dbus.Variant, error) {
		if name != unitName {
			return nil, fmt.Errorf("unexpected unit %q", name)
		}
		content, err := os.ReadFile(loadedPath)
		if err != nil {
			return nil, err
		}
		properties := systemdTestPropertiesFromUnit(string(content))
		pid := beforeRestart
		if countData, readErr := os.ReadFile(restartCountPath); readErr == nil {
			count, _ := strconv.Atoi(strings.TrimSpace(string(countData)))
			if count == 1 {
				pid = afterRestart
			} else if count >= 2 {
				pid = beforeRestart
			}
		}
		properties["MainPID"] = dbus.MakeVariant(uint32(pid))
		return properties, nil
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

// TestSystemdRestoreOfAUnitWithoutAProgramIsNotReportedAsRunning checks that
// active metadata without a live HFDesk MainPID is not treated as a verified
// restored instance.
func TestSystemdRestoreOfAUnitWithoutAProgramIsNotReportedAsRunning(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	systemdTempInstallEnvironment(t)
	m := &systemdManager{scope: ScopeUser}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v", err)
	}
	// A registration whose command line cannot be read: the section holds
	// no ExecStart, so the restored registration names no program.
	content := []byte("[Unit]\nDescription=HFDesk\n\n[Service]\nWorkingDirectory=" + root + "\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the unit directory: %v", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write the unit file: %v", err)
	}
	if err := os.WriteFile(os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT"), content, 0o644); err != nil {
		t.Fatalf("write the loaded unit: %v", err)
	}
	m.readServiceProperties = func(context.Context, string) (map[string]dbus.Variant, error) {
		return systemdTypedProperties("", []string{}, []string{}, root, "", "", []systemdEnvironmentFile{}), nil
	}

	m.restartWait = 250 * time.Millisecond
	loaded, err := m.registrationUnitConfig(context.Background())
	if err != nil {
		t.Fatalf("registrationUnitConfig() = %v", err)
	}
	previous := previousRegistration{path: path, content: content, existed: true, running: true, loaded: loaded, loadedKnown: true}

	// The supervisor reports the unit active, but the typed service MainPID is
	// zero and the registration names no HFDesk process.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	err = m.restorePreviousRegistration(context.Background(), previous)
	if err == nil {
		t.Fatal("restorePreviousRegistration() succeeded with no live HFDesk MainPID")
	} else if !strings.Contains(err.Error(), "service process could not be verified") {
		t.Errorf("restorePreviousRegistration() = %v, want an explicit process-evidence failure", err)
	}
	if strings.Contains(err.Error(), "the previous instance was started again") {
		t.Errorf("restorePreviousRegistration() = %v, must not claim a started instance", err)
	}

	// The state is verified and not assumed: a unit the supervisor does not
	// report active is a restore that was not confirmed.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")
	err = m.restorePreviousRegistration(context.Background(), previous)
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
//
// A fresh machine knows no unit: "is-enabled" prints "not-found" and exits with
// a non-zero status. The installation reads the enablement anyway, because a
// runtime mask leaves no unit file either, and a "not-found" is not a mask: the
// first installation proceeds and enables the registration it writes.
func TestSystemdFreshInstallNeverStartsTheInstance(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ENABLED_STATE", "not-found")

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
	// A first installation has no registration whose enablement it could
	// keep, but it asks for the enablement before it writes the unit file:
	// that is how a runtime mask, which leaves no unit file, is found. A
	// "not-found" answer is not a mask and the installation proceeds.
	if got := systemdSupervisorCallsTo(calls, "is-enabled", unitName); got != 1 {
		t.Errorf("a first installation read the enablement %d times, want once:\n%s", got, strings.Join(calls, "\n"))
	}
	enabledAt := systemdSupervisorCallIndex(calls, "is-enabled", unitName)
	reloadAt := systemdSupervisorCallIndex(calls, "daemon-reload")
	if enabledAt < 0 || enabledAt > reloadAt {
		t.Errorf("the enablement was read at %d and the supervisor was reloaded at %d, want the read before the reload:\n%s", enabledAt, reloadAt, strings.Join(calls, "\n"))
	}
}

// systemdSupervisorCallIndex returns the index of the first call of the fake
// supervisor that equals the given command, and -1 when it was never called.
// The scope prefix of the manager is ignored.
func systemdSupervisorCallIndex(calls []string, command ...string) int {
	want := strings.Join(command, " ")
	for i, call := range calls {
		if strings.TrimPrefix(call, "--user ") == want {
			return i
		}
	}
	return -1
}

// TestSystemdReinstallKeepsTheAutostartStateOfTheUnit checks that a repeat
// installation keeps the autostart state it finds, including its mode.
//
// A first installation enables the registration for boot. A repeat installation
// must not undo the decision of an administrator: a unit that was enabled before
// is enabled again, a unit that was disabled is left alone, and a unit an
// administrator enabled for this boot only ("systemctl enable --runtime") is
// enabled at runtime again instead of being made permanent. A masked unit is
// refused instead of being replaced, see TestSystemdInstallRefusesAMaskedUnit.
// The "static", "indirect", "alias" and "linked" states exit with a zero status
// of the supervisor and are not enablements either: the printed state decides,
// not the status. The supervisor is reloaded in every case, so it picks the new
// unit file up, and the enablement is read before the unit file is written, so
// the state that is kept is the state of the unit the installation replaces.
func TestSystemdReinstallKeepsTheAutostartStateOfTheUnit(t *testing.T) {
	cases := []struct {
		name string
		// state is the enablement the supervisor reports for the unit
		// that is installed already.
		state string
		// wantEnabled is how often the installation may enable the unit
		// persistently.
		wantEnabled int
		// wantRuntime is how often the installation may enable the unit
		// for this boot only.
		wantRuntime int
	}{
		{"a unit that is enabled", "enabled", 1, 0},
		{"a unit that is enabled for this boot only", "enabled-runtime", 0, 1},
		{"a unit that is disabled", "disabled", 0, 0},
		{"a unit that is static", "static", 0, 0},
		{"a unit that is indirect", "indirect", 0, 0},
		{"a unit that is an alias", "alias", 0, 0},
		{"a unit that is linked", "linked", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logPath := systemdRecordingSupervisor(t)
			root := t.TempDir()
			oldExecutable := systemdTestExecutable(t)
			newExecutable := systemdTestExecutable(t)
			previous := systemdExistingOptions(t, oldExecutable, root)

			m, _, _ := systemdExistingInstallation(t, previous)
			t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")
			t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)
			t.Setenv("HFDESK_TEST_SYSTEMCTL_ENABLED_STATE", tc.state)

			if _, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable)); err != nil {
				t.Fatalf("Install() = %v, want nil for a unit that was %q before", err, tc.state)
			}

			calls := systemdSupervisorCalls(t, logPath)
			if got := systemdSupervisorCallsTo(calls, "enable", unitName); got != tc.wantEnabled {
				t.Errorf("the unit was enabled permanently %d times, want %d for a unit that was %q before:\n%s", got, tc.wantEnabled, tc.state, strings.Join(calls, "\n"))
			}
			if got := systemdSupervisorCallsTo(calls, "enable", "--runtime", unitName); got != tc.wantRuntime {
				t.Errorf("the unit was enabled at runtime %d times, want %d for a unit that was %q before:\n%s", got, tc.wantRuntime, tc.state, strings.Join(calls, "\n"))
			}
			if got := systemdSupervisorCallsTo(calls, "daemon-reload"); got == 0 {
				t.Errorf("the supervisor was never reloaded:\n%s", strings.Join(calls, "\n"))
			}
			enabledAt := systemdSupervisorCallIndex(calls, "is-enabled", unitName)
			reloadAt := systemdSupervisorCallIndex(calls, "daemon-reload")
			if enabledAt < 0 {
				t.Fatalf("the enablement of the registration was never read:\n%s", strings.Join(calls, "\n"))
			}
			if enabledAt > reloadAt {
				t.Errorf("the enablement was read after the supervisor was reloaded, the state of the replaced unit is not the state that was kept:\n%s", strings.Join(calls, "\n"))
			}
		})
	}
}

// TestSystemdEnablementOfThePrintedState checks the mapping of the printed
// state of "systemctl is-enabled" onto the enablement an installation keeps.
//
// The status of the supervisor is not the answer: "is-enabled" exits zero for
// "static", "indirect", "alias" and "linked" as well, and a plain "enable" of
// one of those would either fail the installation or enable a unit for a boot
// nobody asked for. Only "enabled" and "enabled-runtime" are kept, and the mask
// is a refusal of its own.
func TestSystemdEnablementOfThePrintedState(t *testing.T) {
	cases := []struct {
		state string
		want  enablementMode
	}{
		{"enabled", enablementPersistent},
		{"enabled-runtime", enablementRuntime},
		{"disabled", enablementNone},
		{"masked", enablementNone},
		{"masked-runtime", enablementNone},
		{"static", enablementNone},
		{"indirect", enablementNone},
		{"alias", enablementNone},
		{"linked", enablementNone},
		{"not-found", enablementNone},
		{"", enablementNone},
		{"a state of a later release", enablementNone},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			facts := enablementOf(tc.state)
			if facts.mode != tc.want {
				t.Errorf("enablementOf(%q).mode = %d, want %d", tc.state, facts.mode, tc.want)
			}
			wantMasked := tc.state == "masked" || tc.state == "masked-runtime"
			if facts.masked != wantMasked {
				t.Errorf("enablementOf(%q).masked = %v, want %v", tc.state, facts.masked, wantMasked)
			}
		})
	}
}

// TestSystemdInstallRefusesAMaskedUnit checks that an installation over a
// persistently masked unit is refused instead of replacing the mask.
//
// A masked unit is a symbolic link to /dev/null, and writing the unit file over
// it would unmask the unit silently: the administrator who masked it would lose
// that decision without noticing. The installation therefore refuses before it
// writes the unit file and before it touches the supervisor, the unit path keeps
// the mask, and the error names the command that unmask the unit in the scope of
// the manager. See TestSystemdInstallRefusesARuntimeMaskedUnit for a mask that
// leaves no unit file at all.
func TestSystemdInstallRefusesAMaskedUnit(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)

	m, unitPath, _ := systemdExistingInstallation(t, previous)
	// A masked unit is a link to /dev/null. Replace the unit file of the test
	// registration with exactly that shape, so a write over the path would be
	// visible as a replacement of the mask.
	if err := os.Remove(unitPath); err != nil {
		t.Fatalf("remove the unit file: %v", err)
	}
	if err := os.Symlink(os.DevNull, unitPath); err != nil {
		t.Fatalf("mask the unit: %v", err)
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", oldExecutable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ENABLED_STATE", "masked")

	request := systemdTempInstallOptions(t, root)
	request.Executable = newExecutable
	_, err := m.Install(context.Background(), request)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of an installation over a masked unit")
	}
	for _, want := range []string{
		unitName,
		"is masked",
		"does not replace the mask",
		"systemctl --user unmask " + unitName,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install() = %q, want an error that mentions %q", err, want)
		}
	}

	// The unit path is untouched: it still holds the mask.
	info, lstatErr := os.Lstat(unitPath)
	if lstatErr != nil {
		t.Fatalf("the unit path is gone after the refused installation: %v", lstatErr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the unit path is no mask any more (mode %s), want a symbolic link", info.Mode())
	}
	if target, readErr := os.Readlink(unitPath); readErr != nil || target != os.DevNull {
		t.Errorf("the unit path points at %q (error: %v), want the mask %q", target, readErr, os.DevNull)
	}

	// The refusal happened before the unit file was written and before the
	// installation touched the supervisor: no directory of the instance was
	// created either.
	if _, statErr := os.Stat(filepath.Join(root, "etc")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the refused installation created the directory %q (stat error: %v)", filepath.Join(root, "etc"), statErr)
	}
	calls := systemdSupervisorCalls(t, logPath)
	for _, command := range [][]string{
		{"daemon-reload"},
		{"enable", unitName},
		{"restart", unitName},
		{"stop", unitName},
	} {
		if got := systemdSupervisorCallsTo(calls, command...); got != 0 {
			t.Errorf("the refused installation called %q %d times:\n%s", strings.Join(command, " "), got, strings.Join(calls, "\n"))
		}
	}
}

// TestSystemdInstallRefusesARuntimeMaskedUnit checks that an installation over a
// unit that is masked at runtime is refused as well.
//
// A runtime mask ("systemctl mask --runtime") lives in /run and leaves no file at
// the unit path, so the installation reads the unit file of a fresh machine and
// would register the unit while the mask stays in place: nothing is clobbered,
// but the new registration cannot start. The mask is only visible through the
// supervisor, which reports "masked-runtime" with a non-zero status, so the
// enablement is read whether or not a unit file exists. The refusal happens
// before any directory is created, before the unit file is written and before the
// supervisor is reloaded, enabled, restarted or asked to stop anything, and the
// error names the command that removes the mask.
func TestSystemdInstallRefusesARuntimeMaskedUnit(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ENABLED_STATE", "masked-runtime")

	m := &systemdManager{scope: ScopeUser}
	_, err := m.Install(context.Background(), systemdTempInstallOptions(t, root))
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of an installation over a runtime masked unit")
	}
	for _, want := range []string{
		unitName,
		"is masked",
		"does not replace the mask",
		"systemctl --user unmask " + unitName,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install() = %q, want an error that mentions %q", err, want)
		}
	}

	// No unit file was created: the runtime mask left no file at the path,
	// and the refused installation writes none. The directory of the unit
	// file is not created either, so the refusal came before the write.
	unitPath, pathErr := m.unitPath()
	if pathErr != nil {
		t.Fatalf("unitPath() = %v, want nil", pathErr)
	}
	if _, statErr := os.Stat(unitPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the refused installation created the unit file %q (stat error: %v)", unitPath, statErr)
	}
	if _, statErr := os.Stat(filepath.Dir(unitPath)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the refused installation created the unit directory %q (stat error: %v)", filepath.Dir(unitPath), statErr)
	}

	// No directory of the instance was created either, so the refusal came
	// before the directories were prepared.
	if entries, readErr := os.ReadDir(root); readErr != nil {
		t.Fatalf("read the root of the installation: %v", readErr)
	} else if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("the refused installation created %v below %q, want no directory at all", names, root)
	}

	// The manager checks both loaded-source identity and enablement before
	// preparation, and does not reload or change a masked registration.
	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "is-enabled", unitName); got != 1 {
		t.Errorf("the enablement was read %d times, want once before the refusal:\n%s", got, strings.Join(calls, "\n"))
	}
	if got := systemdSupervisorCallsTo(calls, "show", unitName, "--property=LoadState"); got != 1 {
		t.Errorf("loaded registration was checked %d times, want once before the refusal:\n%s", got, strings.Join(calls, "\n"))
	}
	for _, call := range calls {
		got := strings.TrimPrefix(call, "--user ")
		if got != "show "+unitName+" --property=LoadState" && got != "is-enabled "+unitName {
			t.Errorf("the refused installation called the supervisor with unexpected command %q:\n%s", call, strings.Join(calls, "\n"))
		}
	}
	for _, command := range [][]string{
		{"daemon-reload"},
		{"enable", unitName},
		{"restart", unitName},
		{"stop", unitName},
	} {
		if got := systemdSupervisorCallsTo(calls, command...); got != 0 {
			t.Errorf("the refused installation called %q %d times:\n%s", strings.Join(command, " "), got, strings.Join(calls, "\n"))
		}
	}
}

// TestSystemdEnablementFailureIsAFailedInstallation checks that the enable of a
// first installation keeps its error handling: a registration the supervisor
// refuses to enable is a failed installation, and the unit file the
// installation wrote is removed again.
//
// The registration of a first installation is enabled for the boot of the
// scope, so an installation that cannot enable it is not the registration the
// user asked for. The failure is reported and the rollback leaves no unit file
// behind.
func TestSystemdEnablementFailureIsAFailedInstallation(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ENABLE_FAIL", "1")

	m := &systemdManager{scope: ScopeUser}
	_, err := m.Install(context.Background(), systemdTempInstallOptions(t, root))
	if err == nil {
		t.Fatal("Install() = nil, want the failure of the enable")
	}
	if !strings.Contains(err.Error(), "enable") {
		t.Errorf("Install() = %q, want an error that names the refused enable", err)
	}

	path, pathErr := m.unitPath()
	if pathErr != nil {
		t.Fatalf("unitPath() = %v, want nil", pathErr)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the unit file %q is still there after the failed installation (stat error: %v)", path, statErr)
	}

	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "enable", unitName); got != 1 {
		t.Errorf("the unit was enabled %d times, want the one enable the installation attempted:\n%s", got, strings.Join(calls, "\n"))
	}
}
