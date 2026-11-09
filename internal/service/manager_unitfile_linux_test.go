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
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/server"
	"github.com/godbus/dbus/v5"
)

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
	if !filepath.IsAbs(parsed.Program) {
		t.Errorf("ExecStart = %q, want an absolute path", parsed.Program)
	}
	if parsed.Program != opts.Executable {
		t.Errorf("ExecStart = %q, want %q", parsed.Program, opts.Executable)
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
	if parsed.Program != opts.Executable {
		t.Errorf("ExecStart = %q, want %q", parsed.Program, opts.Executable)
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
	if parsed.Program != opts.Executable {
		t.Errorf("ExecStart = %q, want %q", parsed.Program, opts.Executable)
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

func TestSystemdParseUnitRecoversTheRegistration(t *testing.T) {
	opts := systemdTestOptions(t, ScopeUser)

	unit, err := renderUnit(opts, nil)
	if err != nil {
		t.Fatalf("renderUnit() = %v, want nil", err)
	}
	parsed := parseUnit(unit)

	if parsed.Program != opts.Executable {
		t.Errorf("the registered executable = %q, want %q", parsed.Program, opts.Executable)
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
}

// TestSystemdInstallInsertsExplicitOptionBeforeTerminator checks that a
// hand-written base file with a "--" terminator is refused as not the unit
// HFDesk generates, before anything changes: the flag editor that inserted
// an option into an arbitrary source is gone, and an update over a base
// file HFDesk did not render is a detector-A refusal with the single next
// action of the ownership contract.
func TestSystemdInstallInsertsExplicitOptionBeforeTerminator(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	config := filepath.Join(root, "config.json")
	state := filepath.Join(root, "state")
	logs := filepath.Join(root, "logs")
	content := "[Service]\nExecStart=/bin/true service run --config " + config + " --state-dir " + state + " --log-dir " + logs + " --\nWorkingDirectory=" + root + "\n[Install]\nWantedBy=default.target\n"

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
	_, err = manager.Install(context.Background(), requested)
	assertSingleDropInAction(t, err, path)
	if !strings.Contains(err.Error(), "is not the unit HFDesk generates") {
		t.Errorf("Install() = %q, want the cause to name the non-generated unit file", err)
	}

	stored, readErr := os.ReadFile(path)
	if readErr != nil || string(stored) != content {
		t.Errorf("the refused update changed the unit (read error %v):\n%s", readErr, stored)
	}
	calls := systemdSupervisorCalls(t, logPath)
	for _, command := range [][]string{{"daemon-reload"}, {"enable", unitName}, {"restart", unitName}} {
		if got := systemdSupervisorCallsTo(calls, command...); got != 0 {
			t.Errorf("the refused update issued %q %d times: %v", strings.Join(command, " "), got, calls)
		}
	}
}

// TestSystemdExecutableUpdatePreservesInheritedStatePathAndIndependentWorkingDirectory
// checks that a hand-written base file with a WorkingDirectory independent of
// the state path is refused as not the unit HFDesk generates: an independent
// working directory is an administrator layer that belongs in the drop-in,
// not in the base file, so no update rewrites such a base file and the state
// data behind the inherited path stays untouched.
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

	_, err = manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, "/bin/echo"))
	assertSingleDropInAction(t, err, unitPath)
	if !strings.Contains(err.Error(), "is not the unit HFDesk generates") {
		t.Errorf("Install() = %q, want the cause to name the non-generated unit file", err)
	}
	stored, readErr := os.ReadFile(unitPath)
	if readErr != nil || string(stored) != content {
		t.Errorf("the refused update changed the unit (read error %v):\n%s", readErr, stored)
	}
	if after, readErr := os.ReadFile(dataPath); readErr != nil || !bytes.Equal(after, data) {
		t.Errorf("state data changed: %q, %v", after, readErr)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 0 {
		t.Errorf("the refused update restarted the stopped unit %d times", got)
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
			name:     "an env wrapper whose separated option value reads the program",
			output:   "ExecStart={ path=/usr/bin/env ; argv[]=/usr/bin/env -u LD_PRELOAD /opt/hfdesk/hfdesk service run --listen 127.0.0.1:19090 ; ignore_errors=no }\n",
			program:  "/opt/hfdesk/hfdesk",
			readable: true,
		},
		{
			name:     "an env wrapper whose option value leaves no program slot",
			output:   "ExecStart={ path=/usr/bin/env ; argv[]=/usr/bin/env -u LD_PRELOAD service run --listen 127.0.0.1:19090 ; ignore_errors=no }\n",
			program:  "/usr/bin/env",
			readable: false,
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

	if _, err := manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, "/bin/true")); err == nil {
		t.Fatal("Install() = nil, want the refusal of the hex-escaped base file")
	} else {
		assertSingleDropInAction(t, err, path)
		if !strings.Contains(err.Error(), "is not the unit HFDesk generates") {
			t.Errorf("Install() = %q, want the cause to name the non-generated unit file", err)
		}
	}
	updated, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(updated) != content {
		t.Errorf("the refused update changed the unit:\n%s", updated)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 0 {
		t.Errorf("the refused update restarted the unit %d times", got)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "daemon-reload"); got != 0 {
		t.Errorf("the refused update reloaded the supervisor %d times", got)
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
	if parsed.Program != newExecutable || parsed.flagValue("--config") != requested.Paths.ConfigFile {
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
			// Detector A subsumes the old rewrite refusal: an ExecStart=
			// the renderer never wrote is a hand-edited base file, so the
			// refusal states the non-generated unit and the single next
			// action of the ownership contract.
			assertSingleDropInAction(t, err, unitPath)
			if !strings.Contains(err.Error(), "is not the unit HFDesk generates") {
				t.Errorf("Install() = %q, want the cause to name the non-generated unit file", err)
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

// TestSystemdInstallRefusesAnUnverifiableRunningExecStartBeforeChangingTheUnit
// checks that a hand-edited registration whose ExecStart= cannot show the
// live process — the program alone or a run vector without the required
// flags — is refused before anything changes, whether the instance runs or
// is stopped: the case is subsumed by detector A, so the refusal names the
// non-generated unit file and the single next action, and neither the
// registration nor the running service is touched.
func TestSystemdInstallRefusesAnUnverifiableRunningExecStartBeforeChangingTheUnit(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail string
	}{
		{name: "the program alone"},
		{name: "a run vector without the required flags", tail: " service run --listen 127.0.0.1:19090"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logPath := systemdRecordingSupervisor(t)
			root := t.TempDir()
			oldExecutable := systemdTestExecutable(t)
			newExecutable := systemdTestExecutable(t)
			previous := systemdExistingOptions(t, oldExecutable, root)
			m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
			// The verification wait only matters without the refusal
			// rule: a disabled rule must fail fast into the churn this
			// test guards against instead of burning the shutdown budget.
			m.restartWait = 250 * time.Millisecond
			value := oldExecutable + tc.tail
			edited := systemdWrapExecStart(t, previousUnit, oldExecutable, value)
			for _, name := range []string{unitPath, os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")} {
				if err := os.WriteFile(name, []byte(edited), 0o644); err != nil {
					t.Fatalf("write the hand-edited unit %q: %v", name, err)
				}
			}
			t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
			t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)

			_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
			if err == nil {
				t.Fatal("Install() = nil, want the refusal of the unverifiable running command")
			}
			// The unverifiable-command case is subsumed by detector A:
			// a command without the canonical run vector is not the unit
			// HFDesk generates, so the refusal fires before any stop or
			// write and carries the one next action of the contract.
			assertSingleDropInAction(t, err, unitPath)
			if !strings.Contains(err.Error(), "is not the unit HFDesk generates") {
				t.Errorf("Install() = %q, want the cause to name the non-generated unit file", err)
			}
			content, readErr := os.ReadFile(unitPath)
			if readErr != nil || string(content) != edited {
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

// TestSystemdInstallRefusesAnUnverifiableCommandOfAStoppedInstance checks
// that detector A does not spare the stopped instance: a hand-edited
// command the restart verification could never prove — the program alone or
// a run vector without the required flags — is refused before anything
// changes instead of being rewritten, and the refusal carries the single
// next action of the ownership contract.
func TestSystemdInstallRefusesAnUnverifiableCommandOfAStoppedInstance(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail string
	}{
		{name: "the program alone"},
		{
			name: "a run vector without the required flags",
			tail: " service run --listen 127.0.0.1:19090",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logPath := systemdRecordingSupervisor(t)
			root := t.TempDir()
			oldExecutable := systemdTestExecutable(t)
			newExecutable := systemdTestExecutable(t)
			previous := systemdExistingOptions(t, oldExecutable, root)
			m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
			edited := systemdWrapExecStart(t, previousUnit, oldExecutable, oldExecutable+tc.tail)
			for _, name := range []string{unitPath, os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")} {
				if err := os.WriteFile(name, []byte(edited), 0o644); err != nil {
					t.Fatalf("write the hand-edited unit %q: %v", name, err)
				}
			}
			t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")
			t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)

			_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
			if err == nil {
				t.Fatal("Install() = nil, want the refusal of the unverifiable stopped command")
			}
			assertSingleDropInAction(t, err, unitPath)
			if !strings.Contains(err.Error(), "is not the unit HFDesk generates") {
				t.Errorf("Install() = %q, want the cause to name the non-generated unit file", err)
			}
			content, readErr := os.ReadFile(unitPath)
			if readErr != nil || string(content) != edited {
				t.Errorf("the refused update changed the unit: read err=%v\n%s", readErr, content)
			}
			calls := systemdSupervisorCalls(t, logPath)
			if count := systemdSupervisorCallsTo(calls, "daemon-reload"); count != 0 {
				t.Errorf("the refused update reloaded the supervisor %d times:\n%s", count, strings.Join(calls, "\n"))
			}
			for _, command := range []string{"restart", "start"} {
				if count := systemdSupervisorCallsTo(calls, command, unitName); count != 0 {
					t.Errorf("a stopped instance was %sed %d times:\n%s", command, count, strings.Join(calls, "\n"))
				}
			}
		})
	}
}

// TestSystemdInstallRefusesAnEnvWrappedExecStart checks the D-6 rule: an
// "env"-wrapped ExecStart= in the base file is a hand edit under detector A
// and the update is refused before anything changes, instead of the old
// repair that rewrote the program slot behind the wrapper. The refusal
// states the non-generated unit and the one next action — move the
// customization into the administrator drop-in — and the stopped instance
// is never started.
func TestSystemdInstallRefusesAnEnvWrappedExecStart(t *testing.T) {
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

	_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the env-wrapped base file")
	}
	assertSingleDropInAction(t, err, unitPath)
	if !strings.Contains(err.Error(), "is not the unit HFDesk generates") {
		t.Errorf("Install() = %q, want the cause to name the non-generated unit file", err)
	}
	content, readErr := os.ReadFile(unitPath)
	if readErr != nil || string(content) != wrapped {
		t.Errorf("the refused update changed the unit: read err=%v\n%s", readErr, content)
	}
	calls := systemdSupervisorCalls(t, logPath)
	if count := systemdSupervisorCallsTo(calls, "daemon-reload"); count != 0 {
		t.Errorf("the refused update reloaded the supervisor %d times:\n%s", count, strings.Join(calls, "\n"))
	}
	for _, command := range []string{"restart", "start"} {
		if count := systemdSupervisorCallsTo(calls, command, unitName); count != 0 {
			t.Errorf("a stopped instance was %sed %d times:\n%s", command, count, strings.Join(calls, "\n"))
		}
	}
}

// TestSystemdUpdateOfARunningEnvWrappedExecStartRefusesBeforeTheRestart
// checks the D-6 rule on the side a stopped instance never reaches: a
// running registration whose base file carries an "env" wrapper is refused
// by detector A before the running instance is stopped — no probe of the
// instance, no restart, no write — and the wrapper stays exactly where the
// administrator put it.
func TestSystemdUpdateOfARunningEnvWrappedExecStartRefusesBeforeTheRestart(t *testing.T) {
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

	_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the env-wrapped base file")
	}
	assertSingleDropInAction(t, err, unitPath)
	if !strings.Contains(err.Error(), "is not the unit HFDesk generates") {
		t.Errorf("Install() = %q, want the cause to name the non-generated unit file", err)
	}
	if len(probedListen) != 0 {
		t.Errorf("the refused update probed the running instance at %v before it refused", probedListen)
	}
	content, readErr := os.ReadFile(unitPath)
	if readErr != nil || string(content) != wrapped {
		t.Errorf("the refused update changed the unit: read err=%v\n%s", readErr, content)
	}
	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 0 {
		t.Errorf("the refused update restarted the unit %d times:\n%s", got, strings.Join(calls, "\n"))
	}
	if got := systemdSupervisorCallsTo(calls, "stop", unitName); got != 0 {
		t.Errorf("the refused update stopped the unit %d times:\n%s", got, strings.Join(calls, "\n"))
	}
}

// TestSystemdWrapperRegistrationReportsPreservedRunFlags checks that the
// facts read back from a recognized wrapper form are the flags the instance
// really runs with: the Status of a wrapped registration reports the values
// behind the "env" prefix, the words of the prefix itself are never
// mistaken for run flags, and a dynamic run flag behind the wrapper is
// still reported as dynamic.
func TestSystemdWrapperRegistrationReportsPreservedRunFlags(t *testing.T) {
	t.Run("the status reads the run flags behind the wrapper", func(t *testing.T) {
		status := systemdStatusOf(t, "/usr/bin/env", "-i", "HF_ARGS=--listen", "HF_HOME=/srv/hfdesk", "/opt/hfdesk/hfdesk",
			"service", "run", "--config", "/srv/etc/hfdesk.json", "--state-dir", "/srv/state", "--log-dir", "/srv/log",
			"--listen", "127.0.0.1:19090", "--cache-dir", "/srv/cache", "--hub-dir", "/srv/cache/hub", "--local-dir", "/srv/models")
		if status.Listen != "127.0.0.1:19090" {
			t.Errorf("listen = %q, want the registered 127.0.0.1:19090; the --listen-shaped word of the env prefix must not be read as a run flag", status.Listen)
		}
		wantPaths := runtimepaths.Paths{
			ConfigFile: "/srv/etc/hfdesk.json",
			StateDir:   "/srv/state",
			LogDir:     "/srv/log",
		}
		if status.Paths != wantPaths {
			t.Errorf("paths = %+v, want the registered %+v", status.Paths, wantPaths)
		}
		if status.Executable != "/opt/hfdesk/hfdesk" {
			t.Errorf("executable = %q, want the program of the wrapped command, not the wrapper token", status.Executable)
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
		if got := parsed.listen(); got != "127.0.0.1:19090" {
			t.Errorf("listen = %q, want the static address registered next to the dynamic flag", got)
		}
	})

	t.Run("an update refuses the wrapped unit", func(t *testing.T) {
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

		_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
		if err == nil {
			t.Fatal("Install() = nil, want the detector-A refusal of the wrapped unit")
		}
		assertSingleDropInAction(t, err, unitPath)
		content, readErr := os.ReadFile(unitPath)
		if readErr != nil || string(content) != wrapped {
			t.Errorf("the refused update changed the unit: read err=%v\n%s", readErr, content)
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
	// The base file stays the canonical rendering: settings beyond it — an
	// extra environment line and an environment file — live in the
	// supervisor-loaded layer the typed properties below report, so the
	// fixture pins preservation across a canonical rewrite without a
	// hand-edited base file.
	home, err := unitHomeDir(previous)
	if err != nil {
		t.Fatalf("unitHomeDir() = %v", err)
	}
	environment := []string{`HFDSK_TEST_LABEL=value with spaces and "quotes"`, "HOME=" + home}
	environmentFiles := []systemdEnvironmentFile{{Path: "/etc/hfdesk/service.env", IgnoreErrors: true}}
	oldArgv := append([]string{oldExecutable}, RunArgs(previous)...)
	updated := previous
	updated.Executable = newExecutable
	newArgv := append([]string{newExecutable}, RunArgs(updated)...)
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

// TestSystemdUpdatePreservesDynamicNativeOperands checks the D-6 family on
// dynamic operands: a base file whose operands carry specifiers or
// environment references is not the unit HFDesk generates, so every update
// over it — loaded or stored-but-unloaded, executable-only or with a named
// flag — is refused before any change instead of preserving the source
// expressions, and the file stays byte for byte as the administrator wrote
// it.
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
			_, err := fixture.manager.Install(context.Background(), requested)
			if err == nil {
				t.Fatal("Install() = nil, want the refusal of the dynamic base file")
			}
			assertSingleDropInAction(t, err, fixture.unitPath)
			if !strings.Contains(err.Error(), "is not the unit HFDesk generates") {
				t.Errorf("Install() = %q, want the cause to name the non-generated unit file", err)
			}
			updated, readErr := os.ReadFile(fixture.unitPath)
			if readErr != nil || string(updated) != fixture.sourceUnit {
				t.Errorf("the refused update changed the unit (read error %v):\n%s", readErr, updated)
			}
			for _, unresolved := range []string{"${HF_CONFIG}", "${HF_CACHE}", "${HF_HUB}", "${HF_LOCAL}", "%h", "%n"} {
				if _, err := os.Stat(filepath.Join(fixture.root, unresolved)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("update created a directory for unresolved expression %q: %v", unresolved, err)
				}
			}
			if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, fixture.logPath), "restart", unitName); got != 0 {
				t.Errorf("the refused update restarted the registration %d times", got)
			}
		})
	}
}

// TestSystemdReinstallRefusesAShadowedLoadedCommand checks that a
// registration whose effective loaded command differs from its unit file —
// a drop-in owning the strict-owned ExecStart — is not updated by silently
// changing only the base file (detector B). The refusal happens before any
// mutation, names the DropInPaths the supervisor reports, and the base file
// and the loaded override both stay exactly as they were.
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
	// The override really lives in local.conf — the very file the install
	// summary recommends. The refusal has to read the reported drop-in and
	// name the removal of the ExecStart= override from that file: moving
	// customizations into it again would never clear the override.
	dropInPath := unitDropInPath(unitPath)
	if err := os.MkdirAll(filepath.Dir(dropInPath), 0o755); err != nil {
		t.Fatalf("create the drop-in directory: %v", err)
	}
	if err := os.WriteFile(dropInPath, []byte("[Service]\nExecStart="+strings.Join(loadedArgv, " ")+"\n"), 0o644); err != nil {
		t.Fatalf("write the drop-in: %v", err)
	}
	loadedProperties["DropInPaths"] = dbus.MakeVariant([]string{dropInPath})
	m.readServiceProperties = func(context.Context, string) (map[string]dbus.Variant, error) {
		return loadedProperties, nil
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")

	_, err = m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err == nil {
		t.Fatal("Install() = nil, want refusal of the shadowed loaded command")
	}
	// The pre-write scan names the drop-in that carries the override —
	// the same state the loaded command mismatch reported before the
	// scan became unconditional — with the established removal action.
	if !strings.Contains(err.Error(), "carries an ExecStart= override") {
		t.Errorf("Install() = %q, want an error that identifies the drop-in that owns the command", err)
	}
	if !strings.Contains(err.Error(), unitDropInPath(unitPath)) {
		t.Errorf("Install() = %q, want the refusal to name the DropInPaths the supervisor reported", err)
	}
	assertSingleRemoveOverrideAction(t, err, unitDropInPath(unitPath))

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
	if registered.Program != oldExecutable || registered.listen() != previous.Listen || registered.flagValue("--config") != overrideConfig || registered.Environment["HF_HUB_CACHE"] != "/operator/hub" {
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

// canonicalGoldenUserUnit is the fact model of canonicalGoldenUserUnitText:
// paths carry spaces, a percent sign, a dollar sign, a quote and a
// backslash, so the golden bytes pin the quoting subset of the writer.
func canonicalGoldenUserUnit() canonicalUnit {
	return canonicalUnit{
		scope:      ScopeUser,
		executable: "/opt/hf desk/bin/100%$hf",
		paths: runtimepaths.Paths{
			ConfigFile: "/opt/hf desk/etc/c\"fg.json",
			StateDir:   "/opt/hf desk/var/lib\\dir",
			LogDir:     "/opt/hf desk/var/log",
		},
		listen:   "127.0.0.1:18080",
		cacheDir: "/opt/hf desk/cache",
		hubDir:   "/opt/hf desk/cache/hub",
		localDir: "/opt/hf desk/models",
		home:     "/home/golden",
	}
}

func canonicalGoldenSystemUnit() canonicalUnit {
	return canonicalUnit{
		scope:      ScopeSystem,
		executable: "/srv/hfdesk/bin/hfdesk",
		paths: runtimepaths.Paths{
			ConfigFile: "/srv/hfdesk/etc/hfdesk.json",
			StateDir:   "/srv/hfdesk/var/lib/hfdesk",
			LogDir:     "/srv/hfdesk/var/log/hfdesk",
		},
		listen: "127.0.0.1:8080",
		user:   "hfdesk-svc",
		group:  "hfdesk",
		home:   "/var/lib/hfdesk",
	}
}

const canonicalGoldenUserUnitText = `# HFDesk service unit.
# format: 1
#
# Generated by the hfdesk service install command; a later installation
# updates its command while preserving other unit settings.
#
# The unit holds no access data: an instance reads its own settings from its
# config file, which only the owner of the instance can read. The unit
# therefore carries the executable and the canonical arguments of the
# process only.

[Unit]
Description=HFDesk
Documentation=https://github.com/bashrusakh/hfdesk
After=network-online.target
Wants=network-online.target

[Service]
# The supervisor owns the lifecycle of the instance: HFDesk stays a normal
# foreground process and the supervisor restarts it when it fails.
Type=simple
ExecStart="/opt/hf desk/bin/100%%$$hf" service run --config "/opt/hf desk/etc/c\"fg.json" --state-dir "/opt/hf desk/var/lib\\dir" --log-dir "/opt/hf desk/var/log" --listen 127.0.0.1:18080 --cache-dir "/opt/hf desk/cache" --hub-dir "/opt/hf desk/cache/hub" --local-dir "/opt/hf desk/models"
WorkingDirectory=/opt/hf desk/var/lib\dir
Restart=on-failure
RestartSec=5s
# The supervisor waits for the graceful shutdown of the instance before it
# kills it. The wait covers the whole orderly shutdown of the server, so a
# stop never interrupts an instance that is still saving its state.
TimeoutStopSec=25s
NoNewPrivileges=true
UMask=0077
Environment=HOME=/home/golden

[Install]
WantedBy=default.target
`

const canonicalGoldenSystemUnitText = `# HFDesk service unit.
# format: 1
#
# Generated by the hfdesk service install command; a later installation
# updates its command while preserving other unit settings.
#
# The unit holds no access data: an instance reads its own settings from its
# config file, which only the owner of the instance can read. The unit
# therefore carries the executable and the canonical arguments of the
# process only.

[Unit]
Description=HFDesk
Documentation=https://github.com/bashrusakh/hfdesk
After=network-online.target
Wants=network-online.target

[Service]
# The supervisor owns the lifecycle of the instance: HFDesk stays a normal
# foreground process and the supervisor restarts it when it fails.
Type=simple
User=hfdesk-svc
Group=hfdesk
ExecStart="/srv/hfdesk/bin/hfdesk" service run --config /srv/hfdesk/etc/hfdesk.json --state-dir /srv/hfdesk/var/lib/hfdesk --log-dir /srv/hfdesk/var/log/hfdesk --listen 127.0.0.1:8080
WorkingDirectory=/srv/hfdesk/var/lib/hfdesk
Restart=on-failure
RestartSec=5s
# The supervisor waits for the graceful shutdown of the instance before it
# kills it. The wait covers the whole orderly shutdown of the server, so a
# stop never interrupts an instance that is still saving its state.
TimeoutStopSec=25s
NoNewPrivileges=true
UMask=0077
Environment=HOME=/var/lib/hfdesk

[Install]
WantedBy=multi-user.target
`

// canonicalFixtureUnits returns the fact models every canonical invariant is
// proven against: special characters, absent optional directories, the
// backslash state dirs the WorkingDirectory writer represents with a
// separator space, and both scopes.
func canonicalFixtureUnits(t *testing.T) []canonicalUnit {
	t.Helper()
	return []canonicalUnit{
		canonicalGoldenUserUnit(),
		canonicalGoldenSystemUnit(),
		{
			scope:      ScopeUser,
			executable: "/usr/local/bin/hfdesk",
			paths: runtimepaths.Paths{
				ConfigFile: "/etc/hfdesk.json",
				StateDir:   "/var/lib/hfdesk",
				LogDir:     "/var/log/hfdesk",
			},
			listen: "0.0.0.0:9999",
			home:   "/home/golden",
		},
		{
			scope:      ScopeUser,
			executable: "/usr/local/bin/hfdesk",
			paths: runtimepaths.Paths{
				ConfigFile: "/opt/a'b/hfdesk.json",
				StateDir:   "/srv/st\\",
				LogDir:     "/var/log/hfdesk",
			},
			listen: "127.0.0.1:8080",
			home:   "/home/golden",
		},
		{
			scope:      ScopeUser,
			executable: "/usr/local/bin/hfdesk",
			paths: runtimepaths.Paths{
				ConfigFile: "/etc/hfdesk.json",
				StateDir:   "/srv/st\\\\",
				LogDir:     "/var/log/hfdesk",
			},
			listen: "127.0.0.1:8080",
			home:   "/home/golden",
		},
		{
			scope:      ScopeSystem,
			executable: "/srv/hfdesk/bin/hfdesk",
			paths: runtimepaths.Paths{
				ConfigFile: "/srv/hfdesk/etc/hfdesk.json",
				StateDir:   "/srv/hfdesk/var/lib/hfdesk",
				LogDir:     "/srv/hfdesk/var/log/hfdesk",
			},
			listen: "127.0.0.1:8080",
			user:   "hfdesk-svc",
			home:   "/var/lib/hfdesk",
		},
	}
}

func TestSystemdCanonicalUnitGoldenBytes(t *testing.T) {
	t.Setenv("HOME", "/home/golden")
	for _, tc := range []struct {
		name  string
		unit  canonicalUnit
		scope Scope
		want  string
	}{
		{"user scope", canonicalGoldenUserUnit(), ScopeUser, canonicalGoldenUserUnitText},
		{"system scope", canonicalGoldenSystemUnit(), ScopeSystem, canonicalGoldenSystemUnitText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := renderCanonicalUnit(tc.unit)
			if err != nil {
				t.Fatalf("renderCanonicalUnit() = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("renderCanonicalUnit() =\n%s\nwant\n%s", got, tc.want)
			}
			read, err := readCanonicalUnit([]byte(got), tc.scope)
			if err != nil {
				t.Fatalf("readCanonicalUnit() = %v, want nil", err)
			}
			if read != tc.unit {
				t.Errorf("readCanonicalUnit() = %+v, want %+v", read, tc.unit)
			}
		})
	}
}

func TestSystemdCanonicalUnitRoundTrip(t *testing.T) {
	t.Setenv("HOME", "/home/golden")
	for _, unit := range canonicalFixtureUnits(t) {
		content, err := renderCanonicalUnit(unit)
		if err != nil {
			t.Fatalf("renderCanonicalUnit(%+v) = %v, want nil", unit, err)
		}
		read, err := readCanonicalUnit([]byte(content), unit.scope)
		if err != nil {
			t.Fatalf("readCanonicalUnit() = %v, want the facts of\n%s", err, content)
		}
		if read != unit {
			t.Errorf("readCanonicalUnit() = %+v, want %+v", read, unit)
		}
		again, err := renderCanonicalUnit(read)
		if err != nil {
			t.Fatalf("renderCanonicalUnit(read) = %v, want nil", err)
		}
		if again != content {
			t.Errorf("the second rendering differs from the first:\n%s\nwant\n%s", again, content)
		}
	}
}

func TestSystemdCanonicalUnitRenderIsByteStable(t *testing.T) {
	unit := canonicalGoldenUserUnit()
	first, err := renderCanonicalUnit(unit)
	if err != nil {
		t.Fatalf("renderCanonicalUnit() = %v, want nil", err)
	}
	for i := 0; i < 5; i++ {
		again, err := renderCanonicalUnit(unit)
		if err != nil {
			t.Fatalf("renderCanonicalUnit() = %v, want nil", err)
		}
		if again != first {
			t.Fatalf("renderCanonicalUnit() is not deterministic at run %d", i)
		}
	}
	// The same bytes come out of facts the reader built from those bytes,
	// so no hidden state influences the rendering.
	t.Setenv("HOME", "/home/golden")
	read, err := readCanonicalUnit([]byte(first), ScopeUser)
	if err != nil {
		t.Fatalf("readCanonicalUnit() = %v, want nil", err)
	}
	fromRead, err := renderCanonicalUnit(read)
	if err != nil {
		t.Fatalf("renderCanonicalUnit() = %v, want nil", err)
	}
	if fromRead != first {
		t.Errorf("rendering the read facts =\n%s\nwant\n%s", fromRead, first)
	}
}

// TestSystemdCanonicalUnitNeverWritesDirectoryDirectives pins the ownership
// invariant behind the chown hazard: the canonical unit never carries a
// systemd *Directory= directive, because those directives can recursively
// hand the ownership of existing directory trees to the supervisor while the
// instance directories are created and granted explicitly.
func TestSystemdCanonicalUnitNeverWritesDirectoryDirectives(t *testing.T) {
	t.Setenv("HOME", "/home/golden")
	for _, unit := range canonicalFixtureUnits(t) {
		content, err := renderCanonicalUnit(unit)
		if err != nil {
			t.Fatalf("renderCanonicalUnit() = %v, want nil", err)
		}
		for _, line := range strings.Split(content, "\n") {
			key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok || !strings.HasSuffix(key, "Directory") {
				continue
			}
			if key != "WorkingDirectory" {
				t.Errorf("the canonical unit writes the directive %q:\n%s", key, content)
			}
		}
		for _, forbidden := range []string{"StateDirectory=", "LogsDirectory=", "RuntimeDirectory="} {
			if strings.Contains(content, forbidden) {
				t.Errorf("the canonical unit writes %s:\n%s", forbidden, content)
			}
		}
	}
}

func TestSystemdReadCanonicalUnitRejectsNonCanonicalShapes(t *testing.T) {
	t.Setenv("HOME", "/home/golden")
	base := canonicalGoldenUserUnitText
	mutate := func(old, replacement string) string {
		t.Helper()
		if !strings.Contains(base, old) {
			t.Fatalf("the golden fixture no longer holds %q", old)
		}
		return strings.Replace(base, old, replacement, 1)
	}
	for _, tc := range []struct {
		name    string
		content string
		scope   Scope
		wantErr string
	}{
		{"line continuation", mutate("Type=simple\n", "Type=simple\\\n"), ScopeUser, "continuation"},
		{"missing format marker", mutate("# format: 1\n", ""), ScopeUser, "marker"},
		{"unsupported format", mutate("# format: 1", "# format: 2"), ScopeUser, "format"},
		{"C-style escape in a word", mutate(`--log-dir "/opt/hf desk/var/log"`, `--log-dir "/opt/hf desk/var/l\og"`), ScopeUser, "escape"},
		{"hex escape in a word", mutate(`--log-dir "/opt/hf desk/var/log"`, `--log-dir "/opt/hf desk/var/\x41og"`), ScopeUser, "escape"},
		{"specifier operand in the command", mutate("--listen 127.0.0.1:18080", "--listen %h"), ScopeUser, "carries a character"},
		{"specifier operand in the directive", mutate(`WorkingDirectory=/opt/hf desk/var/lib\dir`, "WorkingDirectory=%h"), ScopeUser, "specifier"},
		{"environment operand in the command", mutate("--listen 127.0.0.1:18080", "--listen $HOME"), ScopeUser, "carries a character"},
		{"quoted environment operand", mutate("--listen 127.0.0.1:18080", `--listen "$HF_HOME"`), ScopeUser, "environment reference"},
		{"env wrapper command", mutate(`ExecStart="/opt/hf desk/bin/100%%$$hf" service run`, `ExecStart="/usr/bin/env" -i HF_OFFLINE=1 "/opt/hf desk/bin/100%%$$hf" service run`), ScopeUser, "not a program"},
		{"duplicate ExecStart", mutate("WorkingDirectory=", "ExecStart=x\nWorkingDirectory="), ScopeUser, "more than one ExecStart"},
		{"reordered run flags", mutate(`--config "/opt/hf desk/etc/c\"fg.json" --state-dir "/opt/hf desk/var/lib\\dir"`, `--state-dir "/opt/hf desk/var/lib\\dir" --config "/opt/hf desk/etc/c\"fg.json"`), ScopeUser, "--config"},
		{"missing required flag", mutate(`--log-dir "/opt/hf desk/var/log" --listen`, "--listen"), ScopeUser, "--log-dir"},
		{"trailing argument", mutate("--listen 127.0.0.1:18080", "--listen 127.0.0.1:18080 --extra x"), ScopeUser, "argument the renderer does not write"},
		{"single quoted word", mutate(`ExecStart="/opt/hf desk/bin/100%%$$hf"`, `ExecStart='/opt/hf desk/bin/100%%$$hf'`), ScopeUser, "carries a character"},
		{"unclosed quoted word", mutate("--listen 127.0.0.1:18080", `--listen "127.0.0.1:18080`), ScopeUser, "not closed"},
		{"no final newline", strings.TrimSuffix(base, "\n"), ScopeUser, "final newline"},
		{"no ExecStart", mutate("ExecStart=", "Ignored="), ScopeUser, "no ExecStart"},
		{"second environment line", mutate("Environment=HOME=/home/golden", "Environment=HOME=/home/golden\nEnvironment=HOME=/home/other"), ScopeUser, "more than one Environment"},
		{"environment without HOME", mutate("Environment=HOME=/home/golden", "Environment=HF_HUB_OFFLINE=1"), ScopeUser, "not the HOME"},
		{"HOME is not the home of the user", mutate("Environment=HOME=/home/golden", "Environment=HOME=/somewhere"), ScopeUser, "instance runs with"},
		{"WorkingDirectory differs from the command", mutate(`WorkingDirectory=/opt/hf desk/var/lib\dir`, "WorkingDirectory=/other"), ScopeUser, "WorkingDirectory= holds"},
		{"user unit read as machine-wide", base, ScopeSystem, "machine-wide unit names no User="},
		{"machine-wide unit read as user scope", canonicalGoldenSystemUnitText, ScopeUser, "user scope unit names an account"},
		{"machine-wide unit without User", strings.Replace(canonicalGoldenSystemUnitText, "User=hfdesk-svc\n", "", 1), ScopeSystem, "machine-wide unit names no User="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readCanonicalUnit([]byte(tc.content), tc.scope)
			if err == nil {
				t.Fatalf("readCanonicalUnit() = nil, want a %q refusal for\n%s", tc.wantErr, tc.content)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("readCanonicalUnit() = %q, want the refusal to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestPreWriteExecStartOverrideClassifiesTheDirectory pins both sides
// of the pre-write scan's directory read: an absent directory proves that
// no drop-in exists and refuses nothing, while any other read error — a
// permission-denied on an existing directory included — refuses before
// anything is written instead of being read as an empty directory.
func TestPreWriteExecStartOverrideClassifiesTheDirectory(t *testing.T) {
	t.Run("a missing directory proves nothing and refuses nothing", func(t *testing.T) {
		unitPath := filepath.Join(t.TempDir(), unitName)
		if err := preWriteExecStartOverride(unitPath, ScopeUser); err != nil {
			t.Errorf("preWriteExecStartOverride() = %v, want nil for a missing drop-in directory", err)
		}
	})

	t.Run("an unreadable directory refuses before anything is written", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads every directory; the unreadable state needs an unprivileged reader")
		}
		unitPath := filepath.Join(t.TempDir(), unitName)
		dir := filepath.Dir(unitDropInPath(unitPath))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create the drop-in directory: %v", err)
		}
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatalf("make the drop-in directory unreadable: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

		err := preWriteExecStartOverride(unitPath, ScopeUser)
		if err == nil {
			t.Fatal("preWriteExecStartOverride() = nil, want the refusal of the unreadable drop-in directory")
		}
		for _, want := range []string{
			"the update was refused and the registration was not changed",
			dir,
			"cannot be read",
			"Restore this user's read access to the directory",
			`run "hfdesk service install" again`,
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not mention %q", err, want)
			}
		}
		if strings.Contains(err.Error(), " or ") {
			t.Errorf("the refusal %q offers an alternative instead of one action", err)
		}
	})
}

// TestSystemdDropInSearchDirsCoversTheReportedUnitPaths pins the scan's
// directory set to the unit paths the systemd of this machine enumerates:
// `systemd-analyze unit-paths` reports for the system scope /etc, /run and
// the two lib directories, and `systemd-analyze --user unit-paths` reports
// for the user scope /etc, the session runtime directory, /run/systemd/user,
// the XDG data directories and the two lib directories. Every one of them
// can hold a drop-in that owns the command, so every one of them is scanned;
// the previous user scope listed only /etc and /usr/lib, which let an
// override in /run/systemd/user escape the refusal-before-write. The
// perturbation: drop any expected entry and the containment check fails —
// exactly the escape the blocking residue described.
func TestSystemdDropInSearchDirsCoversTheReportedUnitPaths(t *testing.T) {
	unitPath := filepath.Join("/tmp", "somewhere", unitName)
	name := unitName + ".d"
	containment := func(t *testing.T, got []string, want []string) {
		t.Helper()
		have := make(map[string]bool, len(got))
		for _, dir := range got {
			have[dir] = true
		}
		for _, dir := range want {
			if !have[dir] {
				t.Errorf("the drop-in search dirs =\n%v\ndo not cover %q", got, dir)
			}
		}
	}

	t.Run("the user scope covers every reported directory", func(t *testing.T) {
		got := systemdDropInSearchDirs(unitPath, ScopeUser)
		if len(got) == 0 || got[0] != filepath.Dir(unitDropInPath(unitPath)) {
			t.Errorf("the first drop-in search dir = %v, want the unit's own directory %q first", got[:1], filepath.Dir(unitDropInPath(unitPath)))
		}
		want := []string{
			"/etc/systemd/user/" + name,
			userScopeRuntimeSystemdDir() + "/" + name,
			"/run/systemd/user/" + name,
			"/usr/local/lib/systemd/user/" + name,
			"/usr/lib/systemd/user/" + name,
		}
		// The XDG data directories resolve by environment, the way
		// systemd resolves them: the home directory of the user or
		// $XDG_DATA_HOME, then $XDG_DATA_DIRS or its default pair.
		dataHome := os.Getenv("XDG_DATA_HOME")
		if dataHome == "" {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				dataHome = filepath.Join(home, ".local", "share")
			}
		}
		if dataHome != "" {
			want = append(want, dataHome+"/systemd/user/"+name)
		}
		dataList := os.Getenv("XDG_DATA_DIRS")
		if dataList == "" {
			dataList = "/usr/local/share:/usr/share"
		}
		for _, dir := range filepath.SplitList(dataList) {
			if dir != "" {
				want = append(want, dir+"/systemd/user/"+name)
			}
		}
		containment(t, got, want)
	})

	t.Run("the system scope covers every reported directory", func(t *testing.T) {
		got := systemdDropInSearchDirs(unitPath, ScopeSystem)
		containment(t, got, []string{
			"/etc/systemd/system/" + name,
			"/run/systemd/system/" + name,
			"/usr/local/lib/systemd/system/" + name,
			"/usr/lib/systemd/system/" + name,
		})
	})
}

// TestDropInHoldsExecStartParsesTheSystemdAssignmentGrammar pins the
// directive grammar against systemd's own parsing: `ExecStart =/bin/true`
// and `ExecStart  =  /bin/true` are the ExecStart setting (verified with
// `systemd-analyze verify`), so a drop-in that writes the directive with
// whitespace around the "=" still owns the command — while commented lines
// and other keys never do. The perturbation is the space: the previous
// literal-prefix match missed every spaced row.
func TestDropInHoldsExecStartParsesTheSystemdAssignmentGrammar(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    bool
	}{
		{name: "the plain form", content: "[Service]\nExecStart=/bin/true\n", want: true},
		{name: "space before the equals", content: "ExecStart =/bin/true\n", want: true},
		{name: "spaces on both sides", content: "ExecStart  =  /bin/true\n", want: true},
		{name: "indented with a trailing comment", content: "  ExecStart = /bin/true ; the operator's note\n", want: true},
		{name: "after another directive", content: "[Service]\nEnvironment=HF_HUB_CACHE=/x\n\tExecStart = \"/path with spaces\"\n", want: true},
		{name: "a commented directive", content: ";ExecStart=/bin/false\n", want: false},
		{name: "a hash-commented directive", content: "  #ExecStart=/bin/false\n", want: false},
		{name: "a longer key that starts with the name", content: "ExecStartX=/bin/false\n", want: false},
		{name: "the name inside another value", content: "Environment=ExecStart=/bin/false\n", want: false},
		{name: "a line without an assignment", content: "plain prose about ExecStart\n", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dropInHoldsExecStart([]byte(tc.content)); got != tc.want {
				t.Errorf("dropInHoldsExecStart(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// TestPreWriteScanCoversTheDropInSearchPath pins the scope-wide scan: an
// override in ANOTHER directory of the standard drop-in search path is
// refused before any write (the previous single-directory scan missed it —
// and its content carried the whitespace form of the directive as well),
// and files in several directories are all named in the one instruction.
func TestPreWriteScanCoversTheDropInSearchPath(t *testing.T) {
	previousDirs := systemdDropInSearchDirs
	t.Cleanup(func() { systemdDropInSearchDirs = previousDirs })

	unitPath := filepath.Join(t.TempDir(), unitName)
	ownDir := filepath.Dir(unitDropInPath(unitPath))
	otherDir := t.TempDir() // stand-in for a standard search-path directory
	systemdDropInSearchDirs = func(string, Scope) []string {
		return []string{ownDir, otherDir}
	}

	t.Run("an override in another search-path directory refuses", func(t *testing.T) {
		otherFile := filepath.Join(otherDir, "local.conf")
		if err := os.WriteFile(otherFile, []byte("ExecStart  =  /bin/false --other-dir\n"), 0o644); err != nil {
			t.Fatalf("write the drop-in: %v", err)
		}
		err := preWriteExecStartOverride(unitPath, ScopeUser)
		if err == nil {
			t.Fatal("preWriteExecStartOverride() = nil, want the refusal of the other directory's override")
		}
		for _, want := range []string{
			"the update was refused and the registration was not changed",
			otherFile,
			`Remove the "ExecStart=" override from "` + otherFile + `"`,
			`run "hfdesk service install" again`,
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not mention %q", err, want)
			}
		}
		if strings.Contains(err.Error(), " or ") {
			t.Errorf("the refusal %q offers an alternative instead of one action", err)
		}
	})

	t.Run("every directory that carries the directive is named", func(t *testing.T) {
		if err := os.MkdirAll(ownDir, 0o755); err != nil {
			t.Fatalf("create the own drop-in directory: %v", err)
		}
		ownFile := unitDropInPath(unitPath)
		if err := os.WriteFile(ownFile, []byte("ExecStart=/bin/true\n"), 0o644); err != nil {
			t.Fatalf("write the own drop-in: %v", err)
		}
		err := preWriteExecStartOverride(unitPath, ScopeUser)
		if err == nil {
			t.Fatal("preWriteExecStartOverride() = nil, want the refusal that names both files")
		}
		for _, want := range []string{
			"the drop-ins",
			ownFile,
			filepath.Join(otherDir, "local.conf"),
			`Remove the "ExecStart=" override from each of them`,
			`run "hfdesk service install" again`,
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not mention %q", err, want)
			}
		}
		if strings.Contains(err.Error(), " or ") {
			t.Errorf("the refusal %q offers an alternative instead of one action", err)
		}
	})
}

// TestHandEditedUnitRefusalNamesNoRouteForAnUnknownHome pins the honest
// terminal of the home-unknown cause: an unresolvable HOME of the invoking
// user cannot be cleared by moving, removing or reloading files, so the
// refusal names no editing route and promises no retry. The perturbation is
// the cause: every other reason of this refusal keeps the established
// move/remove/reload instruction (the table above).
func TestHandEditedUnitRefusalNamesNoRouteForAnUnknownHome(t *testing.T) {
	t.Setenv("HOME", "")
	unitPath := filepath.Join(t.TempDir(), unitName)
	_, err := detectHandEditedUnit(unitPath, []byte(canonicalGoldenUserUnitText), ScopeUser)
	if err == nil {
		t.Fatal("detectHandEditedUnit() = nil, want the refusal of the unresolvable home")
	}
	for _, want := range []string{
		"the update was refused and the registration was not changed",
		"the home directory of the user is unknown",
		"names no file-editing route",
		"nothing of the registration was changed",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q", err, want)
		}
	}
	for _, unwanted := range []string{
		"Move your customizations into",
		"remove the unit file",
		// The refusal prints the reload instruction lowercase ("reload
		// the unit files with …"): the capital spelling could never
		// match it, which made this negative assertion inert.
		"reload the unit files",
		`run "hfdesk service install"`,
		" or ",
	} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("the refusal %q names %q, want the honest terminal of this cause", err, unwanted)
		}
	}
}

// assertSingleDropInAction proves the refusal standard of the ownership
// contract on one error: the result (refused, nothing changed), the path of
// the refused state and exactly one real next action — the customizations
// move into the administrator drop-in, the removal of the refused unit file
// itself, the reload that clears the supervisor's copy of the removed file
// and the installation command — with no alternative offered. Every step is
// required: a move alone leaves the non-generated file in place, and a
// removal without the reload leaves the supervisor holding the removed file
// until a reload reads the file system again.
func assertSingleDropInAction(t *testing.T, err error, unitPath string) {
	t.Helper()
	if err == nil {
		t.Fatal("the refusal = nil, want a refusal")
	}
	message := err.Error()
	for _, want := range []string{
		"the update was refused and the registration was not changed",
		unitPath,
		`Move your customizations into "` + unitDropInPath(unitPath) + `"`,
		`remove the unit file "` + unitPath + `"`,
		"reload the unit files with \"",
		`daemon-reload"`,
		`run "hfdesk service install" again`,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, " or ") {
		t.Errorf("the refusal %q offers an alternative instead of one action", message)
	}
	if got := strings.Count(message, "Move your customizations into"); got != 1 {
		t.Errorf("the refusal %q states %d customization actions, want exactly one", message, got)
	}
	if got := strings.Count(message, "remove the unit file"); got != 1 {
		t.Errorf("the refusal %q states %d removals of the unit file, want exactly one", message, got)
	}
}

// assertSingleForeignFragmentAction proves the refusal standard of the
// ownership contract for a foreign fragment that still exists at the path
// systemd reports: the customizations worth keeping move into the drop-in,
// the fragment itself is removed there, the unit files are reloaded — the
// supervisor keeps a loaded fragment, even a removed one, until that reload —
// and only then the installation runs again, with no alternative offered.
func assertSingleForeignFragmentAction(t *testing.T, err error, unitPath, fragment string) {
	t.Helper()
	if err == nil {
		t.Fatal("the refusal = nil, want a refusal")
	}
	message := err.Error()
	for _, want := range []string{
		"the update was refused and the registration was not changed",
		fragment,
		unitPath,
		`Move your customizations into "` + unitDropInPath(unitPath) + `"`,
		`remove the unit fragment "` + fragment + `"`,
		"reload the unit files with \"",
		`daemon-reload"`,
		`run "hfdesk service install" again`,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, " or ") {
		t.Errorf("the refusal %q offers an alternative instead of one action", message)
	}
	if got := strings.Count(message, "remove the unit fragment"); got != 1 {
		t.Errorf("the refusal %q states %d fragment removals, want exactly one", message, got)
	}
}

// assertSingleRemoveOverrideAction proves the refusal standard of the
// ownership contract for an ExecStart= override a drop-in owns: the one
// named action must clear the cause, so it removes the override from the
// exact drop-in that carries it — the move into local.conf cannot clear an
// override that already lives there — and reloads the unit files after the
// edit, because the supervisor holds the overridden command in memory until
// a reload reads the file again; it offers no alternative.
func assertSingleRemoveOverrideAction(t *testing.T, err error, dropIn string) {
	t.Helper()
	if err == nil {
		t.Fatal("the refusal = nil, want a refusal")
	}
	message := err.Error()
	for _, want := range []string{
		"the update was refused and the registration was not changed",
		`Remove the "ExecStart=" override from "` + dropIn + `"`,
		"reload the unit files with \"",
		`daemon-reload"`,
		`run "hfdesk service install" again`,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, " or ") {
		t.Errorf("the refusal %q offers an alternative instead of one action", message)
	}
	if got := strings.Count(message, `Remove the "ExecStart=" override`); got != 1 {
		t.Errorf("the refusal %q states %d removal actions, want exactly one", message, got)
	}
	if strings.Contains(message, "Move your customizations into") {
		t.Errorf("the refusal %q names the customization move, which never clears an override", message)
	}
}

// assertSingleReloadAction proves the refusal standard of the ownership
// contract for a drifted loaded command that no reported drop-in carries:
// the one named action must clear the cause, so it reloads the configuration
// the supervisor holds — the move into local.conf clears nothing a reload
// would not — and it offers no alternative.
func assertSingleReloadAction(t *testing.T, err error, hint string) {
	t.Helper()
	if err == nil {
		t.Fatal("the refusal = nil, want a refusal")
	}
	message := err.Error()
	for _, want := range []string{
		"the update was refused and the registration was not changed",
		`Reload the unit files with "` + hint + ` daemon-reload"`,
		`run "hfdesk service install" again`,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, " or ") {
		t.Errorf("the refusal %q offers an alternative instead of one action", message)
	}
	if strings.Contains(message, "Move your customizations into") {
		t.Errorf("the refusal %q names the customization move, which clears no loaded command", message)
	}
	if strings.Contains(message, `Remove the "ExecStart=" override`) {
		t.Errorf("the refusal %q names a removal from no drop-in that carries an override", message)
	}
}

func TestSystemdDetectHandEditedUnit(t *testing.T) {
	t.Setenv("HOME", "/home/golden")
	unitPath := "/etc/systemd/system/hfdesk.service"

	unit, err := detectHandEditedUnit(unitPath, []byte(canonicalGoldenUserUnitText), ScopeUser)
	if err != nil {
		t.Fatalf("detectHandEditedUnit() = %v, want the facts of the canonical unit", err)
	}
	if unit != canonicalGoldenUserUnit() {
		t.Errorf("detectHandEditedUnit() = %+v, want %+v", unit, canonicalGoldenUserUnit())
	}
	if _, err := detectHandEditedUnit(unitPath, []byte(canonicalGoldenSystemUnitText), ScopeSystem); err != nil {
		t.Errorf("detectHandEditedUnit() over the canonical machine-wide unit = %v, want nil", err)
	}

	base := canonicalGoldenUserUnitText
	mutate := func(old, replacement string) string {
		t.Helper()
		if !strings.Contains(base, old) {
			t.Fatalf("the golden fixture no longer holds %q", old)
		}
		return strings.Replace(base, old, replacement, 1)
	}
	notRendered := errUnitNotRendered.Error()
	for _, tc := range []struct {
		name    string
		content string
		wantErr string
	}{
		{"added comment", mutate("Type=simple\n", "Type=simple\n# administrator note\n"), notRendered},
		{"added directive", mutate("Restart=on-failure\n", "Restart=on-failure\nRestartPreventExitStatus=42\n"), notRendered},
		{"edited fixed directive", mutate("RestartSec=5s", "RestartSec=30s"), notRendered},
		{"indented directive", mutate("ExecStart=", "  ExecStart="), notRendered},
		{"env wrapper command", mutate(`ExecStart="/opt/hf desk/bin/100%%$$hf" service run`, `ExecStart="/usr/bin/env" -i HF_OFFLINE=1 "/opt/hf desk/bin/100%%$$hf" service run`), "not a program"},
		{"specifier operand", mutate("--listen 127.0.0.1:18080", "--listen %h"), "carries a character"},
		{"line continuation", mutate("Type=simple\n", "Type=simple\\\n"), "continuation"},
		{"removed format marker", mutate("# format: 1\n", ""), "marker"},
		{"reordered run flags", mutate(`--config "/opt/hf desk/etc/c\"fg.json" --state-dir "/opt/hf desk/var/lib\\dir"`, `--state-dir "/opt/hf desk/var/lib\\dir" --config "/opt/hf desk/etc/c\"fg.json"`), "--config"},
		{"duplicate ExecStart", mutate("WorkingDirectory=", "ExecStart=x\nWorkingDirectory="), "more than one ExecStart"},
		{"edited WorkingDirectory", mutate(`WorkingDirectory=/opt/hf desk/var/lib\dir`, "WorkingDirectory=/other"), "WorkingDirectory= holds"},
		{"removed final newline", strings.TrimSuffix(base, "\n"), "final newline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := detectHandEditedUnit(unitPath, []byte(tc.content), ScopeUser)
			if err == nil {
				t.Fatalf("detectHandEditedUnit() = nil, want a %q refusal", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("detectHandEditedUnit() = %q, want the refusal to mention %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "is not the unit HFDesk generates") {
				t.Errorf("detectHandEditedUnit() = %q, want the cause to name the non-generated unit file", err)
			}
			assertSingleDropInAction(t, err, unitPath)
		})
	}
}

func TestUnitFileUnchanged(t *testing.T) {
	snapshot := []byte("unit content\n")
	for _, tc := range []struct {
		name           string
		snapshot       []byte
		snapshotExists bool
		current        []byte
		currentExists  bool
		want           bool
	}{
		{"absent and still absent", nil, false, nil, false, true},
		{"unchanged file", snapshot, true, []byte("unit content\n"), true, true},
		{"content changed", snapshot, true, []byte("unit changed\n"), true, false},
		{"content grew", snapshot, true, []byte("unit content\nmore\n"), true, false},
		{"file appeared", nil, false, []byte("unit content\n"), true, false},
		{"file vanished", snapshot, true, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := unitFileUnchanged(tc.snapshot, tc.snapshotExists, tc.current, tc.currentExists)
			if got != tc.want {
				t.Errorf("unitFileUnchanged() = %v, want %v", got, tc.want)
			}
		})
	}
}
