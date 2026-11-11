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

// TestParsedUnitFlagValueAcceptsBothArgumentForms pins that the unit reader
// recovers a run flag from both forms a registered ExecStart may carry: the
// paired words of the canonical write and the equals form a hand-edited or
// foreign unit may carry. A reader that only matched the paired form would
// drop the equals-form setting from the status and the reinstall merge.
func TestParsedUnitFlagValueAcceptsBothArgumentForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
	}{
		{"the paired form", `"/usr/bin/hfdesk" run --listen 127.0.0.1:9090 --cache-dir /srv/cache --local-dir /srv/models`},
		{"the equals form", `"/usr/bin/hfdesk" run --listen=127.0.0.1:9090 --cache-dir=/srv/cache --local-dir=/srv/models`},
		{"the mixed form", `"/usr/bin/hfdesk" run --listen=127.0.0.1:9090 --cache-dir /srv/cache --local-dir=/srv/models`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseUnit("[Service]\nExecStart=" + tc.args + "\n")
			if got := parsed.listen(); got != "127.0.0.1:9090" {
				t.Errorf("listen() = %q, want %q", got, "127.0.0.1:9090")
			}
			if got := parsed.flagValue("--cache-dir"); got != "/srv/cache" {
				t.Errorf("--cache-dir = %q, want %q", got, "/srv/cache")
			}
			if got := parsed.flagValue("--local-dir"); got != "/srv/models" {
				t.Errorf("--local-dir = %q, want %q", got, "/srv/models")
			}
		})
	}
}

func TestSystemdUnitPaths(t *testing.T) {
	t.Run("user scope with XDG_CONFIG_HOME", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "/tmp/hfdesk-xdg")
		m := &systemdManager{scope: ScopeUser}
		got, err := m.unitPath()
		assertNoError(t, err, "unitPath() = %v, want nil", err)
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
		assertNoError(t, err, "unitPath() = %v, want nil", err)
		want := "/home/hfdesk-user/.config/systemd/user/hfdesk.service"
		if got != want {
			t.Errorf("unitPath() = %q, want %q", got, want)
		}
	})

	t.Run("system scope", func(t *testing.T) {
		m := &systemdManager{scope: ScopeSystem}
		got, err := m.unitPath()
		assertNoError(t, err, "unitPath() = %v, want nil", err)
		want := "/etc/systemd/system/hfdesk.service"
		if got != want {
			t.Errorf("unitPath() = %q, want %q", got, want)
		}
	})
}

func TestSystemdParseUnitRecoversTheRegistration(t *testing.T) {
	opts := systemdTestOptions(t, ScopeUser)

	facts, err := canonicalUnitForInstall(opts, nil)
	assertNoError(t, err, "canonicalUnitForInstall() = %v, want nil", err)
	unit, err := renderCanonicalUnit(facts)
	assertNoError(t, err, "renderCanonicalUnit() = %v, want nil", err)
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
	assertNoError(t, err, "Executable() = %v, want nil", err)

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

	facts, err := canonicalUnitForInstall(opts, nil)
	assertNoError(t, err, "canonicalUnitForInstall() = %v, want nil", err)
	unit, err := renderCanonicalUnit(facts)
	assertNoError(t, err, "renderCanonicalUnit() = %v, want nil", err)
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
	trailingFacts, err := canonicalUnitForInstall(trailingOpts, nil)
	assertNoError(t, err, "canonicalUnitForInstall() for terminal backslash = %v", err)
	trailingUnit, err := renderCanonicalUnit(trailingFacts)
	assertNoError(t, err, "renderCanonicalUnit() for terminal backslash = %v", err)
	if got := verify("first install with terminal backslash", trailingUnit); got != trailingStateDir {
		t.Errorf("native terminal-backslash WorkingDirectory = %q, want %q", got, trailingStateDir)
	}
	pairedBackslashes := filepath.Join(root, "state with paired trailing backslashes\\\\")
	if err := os.MkdirAll(pairedBackslashes, 0o700); err != nil {
		t.Fatal(err)
	}
	pairedOpts := opts
	pairedOpts.Paths.StateDir = pairedBackslashes
	pairedFacts, err := canonicalUnitForInstall(pairedOpts, nil)
	assertNoError(t, err, "canonicalUnitForInstall() for paired terminal backslashes = %v", err)
	pairedUnit, err := renderCanonicalUnit(pairedFacts)
	assertNoError(t, err, "renderCanonicalUnit() for paired terminal backslashes = %v", err)
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
	err := validateUnitPaths(opts, true)
	assertNoError(t, err, "executable-only validation rejected the inherited app state path: %v", err)
	facts, err := canonicalUnitForInstall(opts, nil)
	assertNoError(t, err, "canonicalUnitForInstall() = %v, want nil", err)
	if _, err := renderCanonicalUnit(facts); err == nil || !strings.Contains(err.Error(), "trailing spaces are stripped") {
		t.Fatalf("renderCanonicalUnit() = %v, want refusal of an unrepresentable WorkingDirectory", err)
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
	assertNoError(t, err, "")
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
		err := os.MkdirAll(dir, 0o700)
		assertNoError(t, err, "")
	}
	dataPath := filepath.Join(state, "jobs_state.json")
	data := []byte(`{"jobs":[{"id":"preserved-state"}]}`)
	err := os.WriteFile(dataPath, data, 0o600)
	assertNoError(t, err, "")
	content := "[Service]\nExecStart=/bin/true service run --config \"" + config + "\" --state-dir \"" + state + "\" --log-dir \"" + logs + "\"\n" +
		"WorkingDirectory=" + workingDirectory + "\nRestart=on-failure\n[Install]\nWantedBy=default.target\n"

	manager := &systemdManager{scope: ScopeUser}
	unitPath, err := manager.unitPath()
	assertNoError(t, err, "")
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
	assertNoError(t, err, "Executable() = %v, want nil", err)
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

	facts, err := canonicalUnitForInstall(opts, &unitWriteIdentity{account: account})
	assertNoError(t, err, "canonicalUnitForInstall() = %v, want nil", err)
	unit, err := renderCanonicalUnit(facts)
	assertNoError(t, err, "renderCanonicalUnit() = %v, want nil", err)
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
	facts, err := canonicalUnitForInstall(opts, nil)
	assertNoError(t, err, "canonicalUnitForInstall() = %v, want nil", err)
	unit, err := renderCanonicalUnit(facts)
	assertNoError(t, err, "renderCanonicalUnit() = %v, want nil", err)

	value := unitDirective(unit, "TimeoutStopSec")
	if value == "" {
		t.Fatalf("the unit has no TimeoutStopSec directive:\n%s", unit)
	}
	timeout, err := time.ParseDuration(value)
	assertNoError(t, err, "the TimeoutStopSec of the unit = %q, want a duration: %v", value, err)
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
	err := os.MkdirAll(filepath.Dir(config), 0o700)
	assertNoError(t, err, "")
	err = os.WriteFile(config, []byte(`{"connections":3}`), 0o600)
	assertNoError(t, err, "")
	stateDir, logDir := filepath.Join(root, "state"), filepath.Join(root, "logs")
	content := fmt.Sprintf("[Service]\nExecStart=/bin/true service run -config=%s -state-dir=%s -log-dir=%s --cache-dir=\n[Install]\nWantedBy=default.target\n", strings.ReplaceAll(config, " ", `\x20`), stateDir, logDir)
	manager := &systemdManager{scope: ScopeUser}
	setSystemdTestPropertyReader(t, manager)
	path, err := manager.unitPath()
	assertNoError(t, err, "unitPath() = %v", err)
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
	assertNoError(t, readErr, "")
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
	assertNoError(t, err, "Install() = %v, want the escaped-dollar update", err)
	if effective.Executable != newExecutable || effective.Paths.ConfigFile != requested.Paths.ConfigFile {
		t.Errorf("effective options = %+v, want the literal dollar paths", effective)
	}
	content, err := os.ReadFile(unitPath)
	assertNoError(t, err, "")
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
				err := os.WriteFile(name, []byte(wrapped), 0o644)
				assertNoError(t, err, "write the wrapped unit %q: %v", name, err)
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
				err := os.WriteFile(name, []byte(edited), 0o644)
				assertNoError(t, err, "write the hand-edited unit %q: %v", name, err)
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
				err := os.WriteFile(name, []byte(edited), 0o644)
				assertNoError(t, err, "write the hand-edited unit %q: %v", name, err)
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
		err := os.WriteFile(name, []byte(wrapped), 0o644)
		assertNoError(t, err, "write the wrapped unit %q: %v", name, err)
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
		err := os.MkdirAll(dir, 0o700)
		assertNoError(t, err, "")
	}

	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	_, tail, _ := splitRenderedExecStart(t, previousUnit, oldExecutable)
	wrapped := systemdWrapExecStart(t, previousUnit, oldExecutable, "/usr/bin/env HF_HOME=/srv/hfdesk "+oldExecutable+tail)
	for _, name := range []string{unitPath, os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")} {
		err := os.WriteFile(name, []byte(wrapped), 0o644)
		assertNoError(t, err, "write the wrapped unit %q: %v", name, err)
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
			err := os.WriteFile(name, []byte(wrapped), 0o644)
			assertNoError(t, err, "write the wrapped unit %q: %v", name, err)
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
		err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755)
		assertNoError(t, err, "")
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
	assertNoError(t, err, "unitHomeDir() = %v", err)
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
	err := os.MkdirAll(filepath.Dir(actualConfig), 0o700)
	assertNoError(t, err, "")
	err = os.WriteFile(actualConfig, []byte(`{"connections":3}`), 0o600)
	assertNoError(t, err, "")
	actualCache := filepath.Join(root, "cache with spaces")
	logDir := filepath.Join(root, "%n", "logs")
	managerLogDir := filepath.Join(root, unitName, "logs")
	managerHome := filepath.Join(root, "manager home")
	managerWorkingDir, err := os.UserHomeDir()
	assertNoError(t, err, "read the user home for the %%h fixture: %v", err)
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
		err := os.WriteFile(path, []byte(sourceUnit), 0o644)
		assertNoError(t, err, "write the dynamic source fixture %q: %v", path, err)
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
				err := os.Remove(fixture.loadedPath)
				assertNoError(t, err, "mark the stored unit as not loaded: %v", err)
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
	err := os.WriteFile(loadedPath, []byte(loadedUnit), 0o644)
	assertNoError(t, err, "write the simulated loaded unit: %v", err)
	loadedArgv := append([]string{oldExecutable}, RunArgs(previous)...)
	for i, arg := range loadedArgv {
		if arg == previous.Paths.ConfigFile {
			loadedArgv[i] = overrideConfig
			break
		}
	}
	home, err := unitHomeDir(previous)
	assertNoError(t, err, "unitHomeDir() = %v", err)
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
	assertNoError(t, readErr, "read the unit file: %v", readErr)
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
	assertNoError(t, err, "decode the typed override: %v", err)
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

// TestSystemdUnitIdentityPinsTheAccountRules checks the identity directives
// unitIdentity renders for the live rollback identity verification: the unit
// always names the account of the request, it names the primary group of a
// resolvable account by its name, and it never guesses a group when the
// account is unknown or the group cannot be resolved.
func TestSystemdUnitIdentityPinsTheAccountRules(t *testing.T) {
	t.Run("an unknown account names no group", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)
		opts.ServiceUser = "hfdesk-svc"

		identity, err := unitIdentity(opts, nil)
		assertNoError(t, err, "unitIdentity() = %v, want nil", err)
		want := "User=" + systemdQuote(opts.ServiceUser) + "\n"
		if identity != want {
			t.Errorf("unitIdentity(opts, nil) = %q, want exactly %q", identity, want)
		}
	})

	t.Run("the group of the account is its primary group by name", func(t *testing.T) {
		account, ok := systemdAccountWithAnotherPrimaryGroup(t)
		if !ok {
			t.Skip("skipping: the machine knows no account whose primary group carries another name")
		}
		group, err := user.LookupGroupId(account.Gid)
		assertNoError(t, err, "LookupGroupId(%q) = %v, want nil", account.Gid, err)
		opts := systemdTestOptions(t, ScopeSystem)
		opts.ServiceUser = account.Username

		identity, err := unitIdentity(opts, account)
		assertNoError(t, err, "unitIdentity() = %v, want nil", err)
		want := "User=" + systemdQuote(account.Username) + "\nGroup=" + systemdQuote(group.Name) + "\n"
		if identity != want {
			t.Errorf("unitIdentity() = %q, want exactly %q", identity, want)
		}
	})

	t.Run("an unresolvable primary group is never guessed", func(t *testing.T) {
		account := systemdUnresolvableAccount(t)
		opts := systemdTestOptions(t, ScopeSystem)
		opts.ServiceUser = account.Username

		identity, err := unitIdentity(opts, account)
		assertNoError(t, err, "unitIdentity() = %v, want nil", err)
		want := "User=" + systemdQuote(account.Username) + "\n"
		if identity != want {
			t.Errorf("unitIdentity() = %q, want exactly %q", identity, want)
		}
	})
}

// TestSystemdCanonicalUnitForInstallPinsTheWrittenGroupRules checks the two
// Group= rules of the live write path through writtenServiceGroup: without
// identity context the unit names no group at all, and a primary group the
// machine cannot resolve is left out instead of guessed.
func TestSystemdCanonicalUnitForInstallPinsTheWrittenGroupRules(t *testing.T) {
	t.Run("no identity context names no group", func(t *testing.T) {
		// The current account resolves on every machine, so a write
		// path that resolved a group without identity context would
		// fill facts.group and fail this check.
		account := systemdCurrentAccount(t)
		opts := systemdTestOptions(t, ScopeSystem)
		opts.ServiceUser = account.Username

		facts, err := canonicalUnitForInstall(opts, nil)
		assertNoError(t, err, "canonicalUnitForInstall() = %v, want nil", err)
		if facts.user != account.Username {
			t.Fatalf("the written account = %q, want %q: the group rules run behind the account of the unit", facts.user, account.Username)
		}
		if facts.group != "" {
			t.Errorf("the written group without identity context = %q, want none", facts.group)
		}
	})

	t.Run("an unresolvable primary group is left out instead of guessed", func(t *testing.T) {
		account := systemdUnresolvableAccount(t)
		opts := systemdTestOptions(t, ScopeSystem)
		opts.ServiceUser = account.Username

		facts, err := canonicalUnitForInstall(opts, &unitWriteIdentity{account: account})
		assertNoError(t, err, "canonicalUnitForInstall() = %v, want nil", err)
		if facts.user != account.Username {
			t.Fatalf("the written account = %q, want %q", facts.user, account.Username)
		}
		if facts.group != "" {
			t.Errorf("the written group of an unresolvable primary group = %q, want none", facts.group)
		}
	})
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
			assertNoError(t, err, "renderCanonicalUnit() = %v, want nil", err)
			if got != tc.want {
				t.Errorf("renderCanonicalUnit() =\n%s\nwant\n%s", got, tc.want)
			}
			read, err := readCanonicalUnit([]byte(got), tc.scope)
			assertNoError(t, err, "readCanonicalUnit() = %v, want nil", err)
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
		assertNoError(t, err, "renderCanonicalUnit(%+v) = %v, want nil", unit, err)
		read, err := readCanonicalUnit([]byte(content), unit.scope)
		assertNoError(t, err, "readCanonicalUnit() = %v, want the facts of\n%s", err, content)
		if read != unit {
			t.Errorf("readCanonicalUnit() = %+v, want %+v", read, unit)
		}
		again, err := renderCanonicalUnit(read)
		assertNoError(t, err, "renderCanonicalUnit(read) = %v, want nil", err)
		if again != content {
			t.Errorf("the second rendering differs from the first:\n%s\nwant\n%s", again, content)
		}
	}
}

func TestSystemdCanonicalUnitRenderIsByteStable(t *testing.T) {
	unit := canonicalGoldenUserUnit()
	first, err := renderCanonicalUnit(unit)
	assertNoError(t, err, "renderCanonicalUnit() = %v, want nil", err)
	for i := 0; i < 5; i++ {
		again, err := renderCanonicalUnit(unit)
		assertNoError(t, err, "renderCanonicalUnit() = %v, want nil", err)
		if again != first {
			t.Fatalf("renderCanonicalUnit() is not deterministic at run %d", i)
		}
	}
	// The same bytes come out of facts the reader built from those bytes,
	// so no hidden state influences the rendering.
	t.Setenv("HOME", "/home/golden")
	read, err := readCanonicalUnit([]byte(first), ScopeUser)
	assertNoError(t, err, "readCanonicalUnit() = %v, want nil", err)
	fromRead, err := renderCanonicalUnit(read)
	assertNoError(t, err, "renderCanonicalUnit() = %v, want nil", err)
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
		assertNoError(t, err, "renderCanonicalUnit() = %v, want nil", err)
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
		if err := preWriteExecStartOverride(unitPath, ScopeUser, operationNoun(false)); err != nil {
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

		err := preWriteExecStartOverride(unitPath, ScopeUser, operationNoun(false))
		if err == nil {
			t.Fatal("preWriteExecStartOverride() = nil, want the refusal of the unreadable drop-in directory")
		}
		assertNamesWithout(t, err.Error(), "the refusal", []string{
			"the update was refused and the registration was not changed",
			dir,
			"cannot be read",
			"Restore this user's read access to the directory",
			`run "hfdesk service install" again`,
		}, []string{
			" or ",
		})
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
			// The control/attached/transient/generator trees of the
			// --user unit-paths output: the control tree beside the
			// unit's own directory, the runtime trees beside the
			// session systemd/user directory.
			filepath.Dir(filepath.Dir(unitPath)) + "/user.control/" + name,
			filepath.Dir(userScopeRuntimeSystemdDir()) + "/user.control/" + name,
			filepath.Dir(userScopeRuntimeSystemdDir()) + "/transient/" + name,
			filepath.Dir(userScopeRuntimeSystemdDir()) + "/generator.early/" + name,
			filepath.Dir(userScopeRuntimeSystemdDir()) + "/generator/" + name,
			filepath.Dir(userScopeRuntimeSystemdDir()) + "/generator.late/" + name,
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
			// The control/attached/transient/generator trees of the
			// unit-paths output.
			"/etc/systemd/system.control/" + name,
			"/run/systemd/system.control/" + name,
			"/etc/systemd/system.attached/" + name,
			"/run/systemd/system.attached/" + name,
			"/run/systemd/transient/" + name,
			"/run/systemd/generator.early/" + name,
			"/run/systemd/generator/" + name,
			"/run/systemd/generator.late/" + name,
		})
	})
}

// TestDropInHoldsExecStartParsesTheSystemdAssignmentGrammar pins the
// directive grammar against systemd's own parsing: `ExecStart =/bin/true`
// and `ExecStart  =  /bin/true` are the ExecStart setting inside
// [Service] (verified with `systemd-analyze verify`), so a drop-in that
// writes the directive with whitespace around the "=" still owns the
// command — while commented lines and other keys never do. The SECTION
// rule comes from the same tool: `ExecStart=` under [Unit] or [Install]
// reports `Unknown key 'ExecStart' in section […], ignoring.`, a bare line
// reports `Assignment outside of section. Ignoring.` and `[service]`
// reports `Unknown section 'service'. Ignoring.` — an assignment systemd
// ignores never owns the command, so only an exact [Service] counts. The
// perturbations are the space (the previous literal-prefix match missed
// every spaced row) and the section (the previous scanner counted every
// key regardless of the section around it).
func TestDropInHoldsExecStartParsesTheSystemdAssignmentGrammar(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    bool
	}{
		{name: "the plain form", content: "[Service]\nExecStart=/bin/true\n", want: true},
		{name: "space before the equals", content: "[Service]\nExecStart =/bin/true\n", want: true},
		{name: "spaces on both sides", content: "[Service]\nExecStart  =  /bin/true\n", want: true},
		{name: "indented with a trailing comment", content: "[Service]\n  ExecStart = /bin/true ; the operator's note\n", want: true},
		{name: "after another directive", content: "[Service]\nEnvironment=HF_HUB_CACHE=/x\n\tExecStart = \"/path with spaces\"\n", want: true},
		{name: "a commented directive", content: "[Service]\n;ExecStart=/bin/false\n", want: false},
		{name: "a hash-commented directive", content: "[Service]\n  #ExecStart=/bin/false\n", want: false},
		{name: "a longer key that starts with the name", content: "[Service]\nExecStartX=/bin/false\n", want: false},
		{name: "the name inside another value", content: "[Service]\nEnvironment=ExecStart=/bin/false\n", want: false},
		{name: "a line without an assignment", content: "[Service]\nplain prose about ExecStart\n", want: false},
		// The section rule: an assignment systemd ignores never owns
		// the command (each form verified with `systemd-analyze
		// verify`), while a later [Service] section counts again.
		{name: "an ExecStart only under [Unit]", content: "[Unit]\nDescription=x\nExecStart=/bin/false\n", want: false},
		{name: "an ExecStart only under [Install]", content: "[Install]\nWantedBy=default.target\nExecStart=/bin/false\n", want: false},
		{name: "an ExecStart outside of any section", content: "ExecStart=/bin/true\n", want: false},
		{name: "a lowercase section is not the service section", content: "[service]\nExecStart=/bin/true\n", want: false},
		{name: "the service section after another section", content: "[Unit]\nDescription=x\nExecStart=/bin/false\n[Service]\nExecStart=/bin/true\n", want: true},
		{name: "a service assignment before a trailing install section", content: "[Service]\nExecStart=/bin/true\n[Install]\nWantedBy=default.target\nExecStart=/bin/false\n", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dropInHoldsExecStart([]byte(tc.content)); got != tc.want {
				t.Errorf("dropInHoldsExecStart(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// TestSystemdDropInSearchDirsCoversTheUnitPathOverride pins the
// $SYSTEMD_UNIT_PATH entries: the variable overrides the unit load path
// (systemd.unit(5)), so its entries participate in the search like the
// static trees — and the static trees stay listed as the superset, because
// a trailing ":" appends the usual path and the running supervisor's
// environment is what decides the real path.
func TestSystemdDropInSearchDirsCoversTheUnitPathOverride(t *testing.T) {
	envRoot := t.TempDir()
	t.Setenv("SYSTEMD_UNIT_PATH", envRoot)

	unitPath := filepath.Join("/tmp", "somewhere", unitName)
	name := unitName + ".d"
	got := systemdDropInSearchDirs(unitPath, ScopeUser)
	have := make(map[string]bool, len(got))
	for _, dir := range got {
		have[dir] = true
	}
	if !have[envRoot+"/"+name] {
		t.Errorf("the drop-in search dirs =\n%v\ndo not cover the $SYSTEMD_UNIT_PATH entry %q", got, envRoot+"/"+name)
	}
	if !have["/etc/systemd/user/"+name] {
		t.Errorf("the drop-in search dirs =\n%v\nlost the static trees beside the $SYSTEMD_UNIT_PATH override", got)
	}
}

// TestPreWriteScanRefusesACarrierInTheUnitPathOverride drives the refusal
// through a REAL seam: a drop-in carrying ExecStart= in a
// $SYSTEMD_UNIT_PATH entry is refused before any write with the file
// named — the env entry is scanned like a static tree (the other real
// directories of the scan are read-only for this test and may contribute
// further carriers; the assertion only requires the env carrier to be
// named, so the test stays robust to their presence).
func TestPreWriteScanRefusesACarrierInTheUnitPathOverride(t *testing.T) {
	envRoot := t.TempDir()
	t.Setenv("SYSTEMD_UNIT_PATH", envRoot)

	unitPath := filepath.Join(t.TempDir(), unitName)
	envDir := filepath.Join(envRoot, unitName+".d")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatalf("create the env drop-in directory: %v", err)
	}
	carrier := filepath.Join(envDir, "override.conf")
	if err := os.WriteFile(carrier, []byte("[Service]\nExecStart  =  /bin/false --from-the-env-entry\n"), 0o644); err != nil {
		t.Fatalf("write the carrier: %v", err)
	}

	err := preWriteExecStartOverride(unitPath, ScopeUser, operationNoun(false))
	if err == nil {
		t.Fatal("preWriteExecStartOverride() = nil, want the refusal of the env entry's carrier")
	}
	assertNamesWithout(t, err.Error(), "the refusal", []string{
		"the update was refused and the registration was not changed",
		carrier,
		`Remove the "ExecStart=" override from "` + carrier + `"`,
		`run "hfdesk service install" again`,
	}, []string{
		" or ",
	})
}

// TestPreWriteScanRefusesACarrierInAGeneratorTree pins the transient and
// generator trees of the unit-paths output: a drop-in sitting in a
// generator directory takes part in the merge like any other, so a carrier
// there is refused before any write with the file named. The seam supplies
// the generator-shaped drop-in directory; the per-directory and message
// rules are the ones every other tree shares.
func TestPreWriteScanRefusesACarrierInAGeneratorTree(t *testing.T) {
	previousDirs := systemdDropInSearchDirs
	t.Cleanup(func() { systemdDropInSearchDirs = previousDirs })

	unitPath := filepath.Join(t.TempDir(), unitName)
	ownDir := filepath.Dir(unitDropInPath(unitPath))
	generatorDropIn := filepath.Join(t.TempDir(), "generator.late", unitName+".d")
	if err := os.MkdirAll(generatorDropIn, 0o755); err != nil {
		t.Fatalf("create the generator drop-in directory: %v", err)
	}
	carrier := filepath.Join(generatorDropIn, "local.conf")
	if err := os.WriteFile(carrier, []byte("[Service]\nExecStart=/bin/false --from-a-generator-tree\n"), 0o644); err != nil {
		t.Fatalf("write the carrier: %v", err)
	}
	systemdDropInSearchDirs = func(string, Scope) []string {
		return []string{ownDir, generatorDropIn}
	}

	err := preWriteExecStartOverride(unitPath, ScopeUser, operationNoun(false))
	if err == nil {
		t.Fatal("preWriteExecStartOverride() = nil, want the refusal of the generator tree's carrier")
	}
	assertNamesWithout(t, err.Error(), "the refusal", []string{
		"the update was refused and the registration was not changed",
		carrier,
		`Remove the "ExecStart=" override from "` + carrier + `"`,
		`run "hfdesk service install" again`,
	}, []string{
		" or ",
	})
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
		if err := os.WriteFile(otherFile, []byte("[Service]\nExecStart  =  /bin/false --other-dir\n"), 0o644); err != nil {
			t.Fatalf("write the drop-in: %v", err)
		}
		err := preWriteExecStartOverride(unitPath, ScopeUser, operationNoun(false))
		if err == nil {
			t.Fatal("preWriteExecStartOverride() = nil, want the refusal of the other directory's override")
		}
		assertNamesWithout(t, err.Error(), "the refusal", []string{
			"the update was refused and the registration was not changed",
			otherFile,
			`Remove the "ExecStart=" override from "` + otherFile + `"`,
			`run "hfdesk service install" again`,
		}, []string{
			" or ",
		})
	})

	t.Run("every directory that carries the directive is named", func(t *testing.T) {
		if err := os.MkdirAll(ownDir, 0o755); err != nil {
			t.Fatalf("create the own drop-in directory: %v", err)
		}
		ownFile := unitDropInPath(unitPath)
		if err := os.WriteFile(ownFile, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
			t.Fatalf("write the own drop-in: %v", err)
		}
		err := preWriteExecStartOverride(unitPath, ScopeUser, operationNoun(false))
		if err == nil {
			t.Fatal("preWriteExecStartOverride() = nil, want the refusal that names both files")
		}
		assertNamesWithout(t, err.Error(), "the refusal", []string{
			"the drop-ins",
			ownFile,
			filepath.Join(otherDir, "local.conf"),
			`Remove the "ExecStart=" override from each of them`,
			`run "hfdesk service install" again`,
		}, []string{
			" or ",
		})
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
	assertNames(t, err.Error(), "the refusal", []string{
		"the update was refused and the registration was not changed",
		"the home directory of the user is unknown",
		"names no file-editing route",
		"nothing of the registration was changed",
	})
	assertNamesWithout(t, err.Error(), "the refusal", nil, []string{
		"Move your customizations into",
		"remove the unit file",
		// The refusal prints the reload instruction lowercase ("reload
		// the unit files with …"): the capital spelling could never
		// match it, which made this negative assertion inert.
		"reload the unit files",
		`run "hfdesk service install"`,
		" or ",
	})
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
	assertNamesWithout(t, message, "the refusal", []string{
		"the update was refused and the registration was not changed",
		unitPath,
		`Move your customizations into "` + unitDropInPath(unitPath) + `"`,
		`remove the unit file "` + unitPath + `"`,
		"reload the unit files with \"",
		`daemon-reload"`,
		`run "hfdesk service install" again`,
	}, []string{
		" or ",
	})
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
	assertNamesWithout(t, message, "the refusal", []string{
		"the update was refused and the registration was not changed",
		fragment,
		unitPath,
		`Move your customizations into "` + unitDropInPath(unitPath) + `"`,
		`remove the unit fragment "` + fragment + `"`,
		"reload the unit files with \"",
		`daemon-reload"`,
		`run "hfdesk service install" again`,
	}, []string{
		" or ",
	})
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
	assertNamesWithout(t, message, "the refusal", []string{
		"the update was refused and the registration was not changed",
		`Remove the "ExecStart=" override from "` + dropIn + `"`,
		"reload the unit files with \"",
		`daemon-reload"`,
		`run "hfdesk service install" again`,
	}, []string{
		" or ",
	})
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
	assertNamesWithout(t, message, "the refusal", []string{
		"the update was refused and the registration was not changed",
		`Reload the unit files with "` + hint + ` daemon-reload"`,
		`run "hfdesk service install" again`,
	}, []string{
		" or ",
	})
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
	assertNoError(t, err, "detectHandEditedUnit() = %v, want the facts of the canonical unit", err)
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
