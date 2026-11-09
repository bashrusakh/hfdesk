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
			name:  "an env wrapper whose separated option value is kept",
			value: "/usr/bin/env -u NAME /path/to/hfdesk " + tail,
			want:  `/usr/bin/env -u NAME "` + executable + `" ` + tail,
		},
		{
			name:  "an env wrapper with the value-less -i option",
			value: "/usr/bin/env -i /path/to/hfdesk " + tail,
			want:  `/usr/bin/env -i "` + executable + `" ` + tail,
		},
		{
			name:  "an env wrapper with an assignment before the program",
			value: "/usr/bin/env FOO=bar /path/to/hfdesk " + tail,
			want:  `/usr/bin/env FOO=bar "` + executable + `" ` + tail,
		},
		{
			name:  "an env wrapper with an attached option value",
			value: "/usr/bin/env --unset=NAME /path/to/hfdesk " + tail,
			want:  `/usr/bin/env --unset=NAME "` + executable + `" ` + tail,
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
			name:  "an env wrapper whose option value leaves no program",
			value: "/usr/bin/env -u NAME " + tail,
			form:  "env -u NAME",
		},
		{
			name:  "an env wrapper with an option word outside the subset",
			value: "/usr/bin/env --debug /path/to/hfdesk " + tail,
			form:  "--debug",
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

// TestSystemdInstallRefusesAnUnverifiableRunningExecStartBeforeChangingTheUnit
// checks the rule that an update which must verify a running start requires a
// verifiable existing command: a hand-edited registration whose ExecStart=
// cannot show the live process the canonical "service run" vector with its
// required flags is refused before anything changes — no stop, no unit write,
// no restart — while the stopped instance of the same command keeps updating.
// The refusal names the found command, the canonical vector as the next
// action, and states that neither the registration nor the running service
// was touched.
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
			for _, want := range []string{"was refused", "not changed", "not stopped", value, "service run", "--config", newExecutable} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Install() = %q, want it to contain %q", err, want)
				}
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

// TestSystemdInstallUpdatesAnUnverifiableCommandOfAStoppedInstance checks
// the preserved side of the rule: a stopped instance has no start to verify,
// so the same hand-edited commands keep updating exactly as before — the
// program slot takes the new program, the run vector stays as it is, and
// nothing is started.
func TestSystemdInstallUpdatesAnUnverifiableCommandOfAStoppedInstance(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tail      string
		wantExtra string
	}{
		{name: "the program alone"},
		{
			name:      "a run vector without the required flags",
			tail:      " service run --listen 127.0.0.1:19090",
			wantExtra: " service run --listen 127.0.0.1:19090",
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

			if _, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable)); err != nil {
				t.Fatalf("Install() = %v, want the stopped update of an unverifiable command accepted", err)
			}
			content, readErr := os.ReadFile(unitPath)
			if readErr != nil {
				t.Fatalf("read the updated unit: %v", readErr)
			}
			want := `ExecStart="` + newExecutable + `"` + tc.wantExtra
			if !strings.Contains(string(content), want) {
				t.Errorf("the updated unit does not hold %q:\n%s", want, content)
			}
			if strings.Contains(string(content), oldExecutable) {
				t.Errorf("the updated unit still names the old program %q:\n%s", oldExecutable, content)
			}
			calls := systemdSupervisorCalls(t, logPath)
			for _, command := range []string{"restart", "start"} {
				if count := systemdSupervisorCallsTo(calls, command, unitName); count != 0 {
					t.Errorf("a stopped instance was %sed %d times:\n%s", command, count, strings.Join(calls, "\n"))
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
