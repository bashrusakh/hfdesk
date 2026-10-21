// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/service"
)

// interactiveFlagPattern matches the registration of a flag of the
// interactive command line, e.g. `flag.IntVar(&port, "port", 8080, ...)`.
var interactiveFlagPattern = regexp.MustCompile(`flag\.\w+Var\(&\w+, "([^"]+)"`)

// fakeManager is a service.Manager that answers with a fixed status.
//
// The command family is exercised without touching the system of the test
// run: the manager of the platform supervisor is replaced by this one.
type fakeManager struct {
	status service.Status
	err    error
}

func (m fakeManager) Install(context.Context, service.InstallOptions) error { return nil }

func (m fakeManager) Uninstall(context.Context) error { return nil }

func (m fakeManager) Start(context.Context) error { return nil }

func (m fakeManager) Stop(context.Context) error { return nil }

func (m fakeManager) Restart(context.Context) error { return nil }

func (m fakeManager) Status(context.Context) (service.Status, error) {
	return m.status, m.err
}

// personalCacheRoot returns the Hugging Face cache of the user that runs
// the test.
//
// It is the cache a user scope instance used to be reported with, even when
// the instance used another one, so it is the value the reports must never
// name.
func personalCacheRoot(t *testing.T) string {
	t.Helper()
	root, _ := service.DefaultCacheDirs(service.ScopeUser, runtimepaths.Paths{})
	return root
}

// writeConfigFile writes the config file of an instance with the given
// cache root.
func writeConfigFile(t *testing.T, path, cacheRoot string) {
	t.Helper()
	data, err := json.Marshal(map[string]string{"cache-dir": cacheRoot})
	if err != nil {
		t.Fatalf("encode the config file: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write the config file: %v", err)
	}
}

// summaryField returns the value of a "Key: value" line of a summary block.
//
// The value is read from the line of the key itself, so a value that is the
// prefix of another value of the same block cannot satisfy the check.
func summaryField(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, key+":") {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(trimmed, key+":"))
	}
	return ""
}

// usageOptionOwner returns the heading that introduces the option block the
// given option belongs to.
func usageOptionOwner(out, option string) string {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), option) {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			heading := strings.TrimSpace(lines[j])
			if heading == "" {
				continue
			}
			return heading
		}
	}
	return ""
}

func TestServiceUsageListsEverySubcommand(t *testing.T) {
	var buf bytes.Buffer
	writeServiceUsage(&buf)

	out := buf.String()
	for _, name := range serviceSubcommands {
		if !strings.Contains(out, name) {
			t.Errorf("the usage block does not mention the %q subcommand:\n%s", name, out)
		}
	}
}

// TestServiceUsageNamesTheOwnerOfTheListenOption checks that the usage
// block of the family does not offer a bind address as an option of the
// family: the option is listed with the subcommand that accepts it, because
// the other subcommands reject it.
func TestServiceUsageNamesTheOwnerOfTheListenOption(t *testing.T) {
	var buf bytes.Buffer
	writeServiceUsage(&buf)
	out := buf.String()

	owner := usageOptionOwner(out, "--listen")
	if owner == "" {
		// The family block does not offer the option at all: nothing it
		// advertises is rejected by a subcommand.
		return
	}
	if !strings.Contains(owner, "install") {
		t.Errorf("the usage block offers --listen without naming the subcommand that accepts it (%q):\n%s", owner, out)
	}
}

// TestServiceLifecycleSubcommandsRejectTheListenOption checks that the bind
// address is an option of the install subcommand only.
func TestServiceLifecycleSubcommandsRejectTheListenOption(t *testing.T) {
	for _, sub := range []string{"uninstall", "start", "stop", "restart", "status"} {
		var stdout, stderr bytes.Buffer
		code := runServiceCommand([]string{sub, "--listen", "127.0.0.1:9090"}, &stdout, &stderr)
		if code == 0 {
			t.Errorf("the %q subcommand must reject --listen, got exit code %d", sub, code)
		}
		if !strings.Contains(stderr.String(), "listen") {
			t.Errorf("the error of the %q subcommand does not name the rejected option:\n%s", sub, stderr.String())
		}
	}
}

// TestServiceLifecycleSubcommandsRejectTheInstallOnlyOptions checks that
// the options of the install subcommand are options of that subcommand
// only.
//
// The account of a machine-wide instance and the binary the registration
// points at belong to the installation: a lifecycle command has nothing to
// do with them and must not accept them.
func TestServiceLifecycleSubcommandsRejectTheInstallOnlyOptions(t *testing.T) {
	for _, sub := range []string{"uninstall", "start", "stop", "restart", "status", "run"} {
		for _, option := range [][]string{
			{"--service-user", "hfdesk"},
			{"--executable", "/usr/local/bin/hfdesk"},
		} {
			var stdout, stderr bytes.Buffer
			code := runServiceCommand(append([]string{sub}, option...), &stdout, &stderr)
			if code == 0 {
				t.Errorf("the %q subcommand must reject %s, got exit code %d", sub, option[0], code)
			}
			// The parser names the option without its leading dashes.
			name := strings.TrimPrefix(option[0], "--")
			if !strings.Contains(stderr.String(), name) {
				t.Errorf("the error of the %q subcommand does not name the rejected option %s:\n%s", sub, option[0], stderr.String())
			}
		}
	}
}

// TestServiceUserIsRejectedForTheUserScope checks that the account of a
// machine-wide instance cannot be named for a user scope instance.
//
// A user scope instance runs as the user itself, so an account name would
// be a promise the installation cannot keep.
func TestServiceUserIsRejectedForTheUserScope(t *testing.T) {
	if isWindows() {
		t.Skip("the user scope does not exist on windows")
	}

	var stdout, stderr bytes.Buffer
	code := runServiceCommand([]string{"install", "--user", "--service-user", "hfdesk"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("the user scope must reject --service-user, got exit code %d", code)
	}
	errText := stderr.String()
	if !strings.Contains(errText, "--service-user") {
		t.Errorf("the error does not name the rejected option:\n%s", errText)
	}
	if !strings.Contains(errText, "user") {
		t.Errorf("the error does not explain the scope of the option:\n%s", errText)
	}
}

// TestBuildInstallOptionsCarriesTheServiceUser checks that the account
// reaches the options of the installation.
//
// The account is empty when the command line does not name one: the default
// of the platform is resolved by the platform manager, which knows the
// accounts of the machine.
func TestBuildInstallOptionsCarriesTheServiceUser(t *testing.T) {
	const executable = "/usr/local/bin/hfdesk"

	opts, err := buildInstallOptions(service.ScopeSystem, installFlags{serviceUser: "hfdesk-svc"}, executable)
	if err != nil {
		t.Fatalf("build the install options: %v", err)
	}
	if opts.ServiceUser != "hfdesk-svc" {
		t.Errorf("the service account is %q, want %q", opts.ServiceUser, "hfdesk-svc")
	}

	opts, err = buildInstallOptions(service.ScopeSystem, installFlags{}, executable)
	if err != nil {
		t.Fatalf("build the install options: %v", err)
	}
	if opts.ServiceUser != "" {
		t.Errorf("the service account is %q, want it to be unset", opts.ServiceUser)
	}
}

// TestBuildInstallOptionsRejectsTheServiceUserForTheUserScope checks that
// a user scope instance never carries a service account.
func TestBuildInstallOptionsRejectsTheServiceUserForTheUserScope(t *testing.T) {
	if isWindows() {
		t.Skip("the user scope does not exist on windows")
	}
	if _, err := buildInstallOptions(service.ScopeUser, installFlags{serviceUser: "hfdesk"}, "/usr/local/bin/hfdesk"); err == nil {
		t.Error("the user scope must reject a service account")
	}
}

// TestInstallSummaryReportsTheAccountOfTheInstance checks that the summary
// names the account the instance runs as.
func TestInstallSummaryReportsTheAccountOfTheInstance(t *testing.T) {
	opts := service.InstallOptions{
		Scope:      service.ScopeSystem,
		Executable: "/usr/local/bin/hfdesk",
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: "/etc/hfdesk/hfdesk.json",
			StateDir:   "/var/lib/hfdesk",
			LogDir:     "/var/log/hfdesk",
		},
		ServiceUser: "hfdesk-svc",
	}

	var buf bytes.Buffer
	mgr := fakeManager{status: service.Status{State: service.StateRunning}}
	writeInstallSummary(&buf, context.Background(), mgr, service.ScopeSystem, opts)

	out := buf.String()
	if got := summaryField(out, "Account"); got != "hfdesk-svc" {
		t.Errorf("the account of the summary is %q, want %q:\n%s", got, "hfdesk-svc", out)
	}
}

// TestInstallSummaryReportsTheDefaultAccountOfThePlatform checks that the
// summary names the account of the platform when the command line does not
// name one.
func TestInstallSummaryReportsTheDefaultAccountOfThePlatform(t *testing.T) {
	opts := service.InstallOptions{
		Scope:      service.ScopeSystem,
		Executable: "/usr/local/bin/hfdesk",
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: "/etc/hfdesk/hfdesk.json",
			StateDir:   "/var/lib/hfdesk",
			LogDir:     "/var/log/hfdesk",
		},
	}

	var buf bytes.Buffer
	mgr := fakeManager{status: service.Status{State: service.StateRunning}}
	writeInstallSummary(&buf, context.Background(), mgr, service.ScopeSystem, opts)

	out := buf.String()
	got := summaryField(out, "Account")
	if got == "" {
		t.Fatalf("the summary does not name an account:\n%s", out)
	}
	if want := service.DefaultServiceUser(); want != "" && got != want {
		t.Errorf("the account of the summary is %q, want the default account of the platform %q:\n%s", got, want, out)
	}
}

func TestServiceHelpSucceeds(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runServiceCommand([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("the help of the service command must succeed, got exit code %d", code)
	}
	if !strings.Contains(stdout.String(), "install") {
		t.Errorf("the help does not list the subcommands:\n%s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("the help must not write to stderr:\n%s", stderr.String())
	}
}

func TestServiceUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runServiceCommand([]string{"frobnicate"}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("an unknown subcommand must not succeed, got exit code %d", code)
	}

	errText := stderr.String()
	if !strings.Contains(errText, "frobnicate") {
		t.Errorf("the error does not name the unknown subcommand:\n%s", errText)
	}
	for _, name := range serviceSubcommands {
		if !strings.Contains(errText, name) {
			t.Errorf("the error does not name the valid subcommand %q:\n%s", name, errText)
		}
	}
}

func TestServiceWithoutSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runServiceCommand(nil, &stdout, &stderr)
	if code == 0 {
		t.Errorf("a missing subcommand must not succeed, got exit code %d", code)
	}
	for _, name := range serviceSubcommands {
		if !strings.Contains(stderr.String(), name) {
			t.Errorf("the usage block does not name the valid subcommand %q:\n%s", name, stderr.String())
		}
	}
}

func TestServiceScopeSelectorsAreExclusive(t *testing.T) {
	if _, err := resolveScope(true, true); err == nil {
		t.Error("--user together with --system must be rejected")
	}

	var stdout, stderr bytes.Buffer
	code := runServiceCommand([]string{"install", "--user", "--system"}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("an ambiguous scope must not succeed, got exit code %d", code)
	}
	if !strings.Contains(stderr.String(), "--user") || !strings.Contains(stderr.String(), "--system") {
		t.Errorf("the error does not name both selectors:\n%s", stderr.String())
	}
}

func TestServiceScopeDefaults(t *testing.T) {
	scope, err := resolveScope(false, false)
	if err != nil {
		t.Fatalf("the default scope must resolve: %v", err)
	}
	want := service.ScopeUser
	if isWindows() {
		want = service.ScopeSystem
	}
	if scope != want {
		t.Errorf("the default scope is %q, want %q", scope, want)
	}

	user, err := resolveScope(true, false)
	if err != nil {
		t.Fatalf("the user scope must resolve: %v", err)
	}
	if user != service.ScopeUser {
		t.Errorf("the user scope is %q, want %q", user, service.ScopeUser)
	}

	system, err := resolveScope(false, true)
	if err != nil {
		t.Fatalf("the system scope must resolve: %v", err)
	}
	if system != service.ScopeSystem {
		t.Errorf("the system scope is %q, want %q", system, service.ScopeSystem)
	}
}

func TestServiceListenValidation(t *testing.T) {
	if _, err := validateListen(":8080"); err == nil {
		t.Error("an empty host must be rejected, it binds every interface")
	}
	if _, err := validateListen("127.0.0.1:0"); err == nil {
		t.Error("port 0 must be rejected")
	}
	if _, err := validateListen("127.0.0.1:70000"); err == nil {
		t.Error("a port out of range must be rejected")
	}

	listen, err := validateListen("127.0.0.1:9090")
	if err != nil {
		t.Fatalf("a loopback address must be accepted: %v", err)
	}
	if listen != "127.0.0.1:9090" {
		t.Errorf("the listen address is %q, want %q", listen, "127.0.0.1:9090")
	}
}

func TestServiceRelativePathsAreRejected(t *testing.T) {
	for _, args := range [][]string{
		{"install", "--state-dir", "relative/state"},
		{"install", "--config", "relative/hfdesk.json"},
		{"install", "--log-dir", "relative/logs"},
		{"install", "--cache-dir", "relative/cache"},
		{"install", "--hub-dir", "relative/hub"},
		{"install", "--local-dir", "relative/models"},
	} {
		var stdout, stderr bytes.Buffer
		code := runServiceCommand(args, &stdout, &stderr)
		if code == 0 {
			t.Errorf("%v must be rejected, got exit code %d", args, code)
		}
		if !strings.Contains(stderr.String(), "absolute") {
			t.Errorf("%v is not reported as a relative path:\n%s", args, stderr.String())
		}
	}
}

func TestBuildInstallOptionsUsesTheDefaultsOfTheScope(t *testing.T) {
	const executable = "/usr/local/bin/hfdesk"

	defaults, err := service.DefaultPaths(service.ScopeUser)
	if err != nil {
		t.Fatalf("the default paths of the scope: %v", err)
	}
	cacheRoot, hubDir := service.DefaultCacheDirs(service.ScopeUser, defaults)

	opts, err := buildInstallOptions(service.ScopeUser, installFlags{}, executable)
	if err != nil {
		t.Fatalf("build the install options: %v", err)
	}

	if opts.Scope != service.ScopeUser {
		t.Errorf("the scope is %q, want %q", opts.Scope, service.ScopeUser)
	}
	if opts.Executable != executable {
		t.Errorf("the executable is %q, want %q", opts.Executable, executable)
	}
	if opts.Listen != service.DefaultListen {
		t.Errorf("the listen address is %q, want %q", opts.Listen, service.DefaultListen)
	}
	if opts.Paths != defaults {
		t.Errorf("the paths are %+v, want %+v", opts.Paths, defaults)
	}
	if opts.CacheDir != cacheRoot {
		t.Errorf("the cache root is %q, want %q", opts.CacheDir, cacheRoot)
	}
	if opts.HubDir != hubDir {
		t.Errorf("the hub dir is %q, want %q", opts.HubDir, hubDir)
	}
	if opts.LocalDir != "" {
		t.Errorf("the local dir is %q, want it to be unset", opts.LocalDir)
	}
}

func TestBuildInstallOptionsAppliesTheOverrides(t *testing.T) {
	const executable = "/usr/local/bin/hfdesk"

	flags := installFlags{
		listen:   "0.0.0.0:9090",
		config:   "/etc/hfdesk/custom.json",
		stateDir: "/var/lib/hfdesk-custom",
		logDir:   "/var/log/hfdesk-custom",
		cacheDir: "/srv/cache",
		hubDir:   "/srv/cache/hub",
		localDir: "/srv/models",
	}

	opts, err := buildInstallOptions(service.ScopeSystem, flags, executable)
	if err != nil {
		t.Fatalf("build the install options: %v", err)
	}

	if opts.Scope != service.ScopeSystem {
		t.Errorf("the scope is %q, want %q", opts.Scope, service.ScopeSystem)
	}
	if opts.Listen != "0.0.0.0:9090" {
		t.Errorf("the listen address is %q, want %q", opts.Listen, "0.0.0.0:9090")
	}
	if opts.Paths.ConfigFile != "/etc/hfdesk/custom.json" {
		t.Errorf("the config file is %q, want %q", opts.Paths.ConfigFile, "/etc/hfdesk/custom.json")
	}
	if opts.Paths.StateDir != "/var/lib/hfdesk-custom" {
		t.Errorf("the state dir is %q, want %q", opts.Paths.StateDir, "/var/lib/hfdesk-custom")
	}
	if opts.Paths.LogDir != "/var/log/hfdesk-custom" {
		t.Errorf("the log dir is %q, want %q", opts.Paths.LogDir, "/var/log/hfdesk-custom")
	}
	if opts.CacheDir != "/srv/cache" {
		t.Errorf("the cache root is %q, want %q", opts.CacheDir, "/srv/cache")
	}
	if opts.HubDir != "/srv/cache/hub" {
		t.Errorf("the hub dir is %q, want %q", opts.HubDir, "/srv/cache/hub")
	}
	if opts.LocalDir != "/srv/models" {
		t.Errorf("the local dir is %q, want %q", opts.LocalDir, "/srv/models")
	}
}

// TestBuildInstallOptionsKeepsTheCacheLayoutTogether checks that the
// registered cache root and the registered hub directory are one layout: an
// explicit cache root moves the hub directory with it, and an explicit hub
// directory is honoured as given.
//
// A registration that names a cache root and the hub of another tree would
// write the blobs of the instance into a directory its own cache root does
// not contain.
func TestBuildInstallOptionsKeepsTheCacheLayoutTogether(t *testing.T) {
	const executable = "/usr/local/bin/hfdesk"

	defaults, err := service.DefaultPaths(service.ScopeUser)
	if err != nil {
		t.Fatalf("the default paths of the scope: %v", err)
	}
	defaultRoot, defaultHub := service.DefaultCacheDirs(service.ScopeUser, defaults)

	for _, tc := range []struct {
		name     string
		flags    installFlags
		wantRoot string
		wantHub  string
	}{
		{
			name:     "a cache root alone moves the hub directory with it",
			flags:    installFlags{cacheDir: "/srv/cache"},
			wantRoot: "/srv/cache",
			wantHub:  "/srv/cache/hub",
		},
		{
			name:     "a hub directory alone is honoured as given",
			flags:    installFlags{hubDir: "/mnt/hub"},
			wantRoot: defaultRoot,
			wantHub:  "/mnt/hub",
		},
		{
			name:     "both directories are honoured as given",
			flags:    installFlags{cacheDir: "/srv/cache", hubDir: "/mnt/hub"},
			wantRoot: "/srv/cache",
			wantHub:  "/mnt/hub",
		},
		{
			name:     "without an override the pair of the scope is kept",
			flags:    installFlags{},
			wantRoot: defaultRoot,
			wantHub:  defaultHub,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := buildInstallOptions(service.ScopeUser, tc.flags, executable)
			if err != nil {
				t.Fatalf("build the install options: %v", err)
			}
			if opts.CacheDir != tc.wantRoot {
				t.Errorf("the cache root is %q, want %q", opts.CacheDir, tc.wantRoot)
			}
			if opts.HubDir != tc.wantHub {
				t.Errorf("the hub dir is %q, want %q", opts.HubDir, tc.wantHub)
			}
		})
	}
}

func TestRenderStatusReportsTheInstance(t *testing.T) {
	status := service.Status{
		State:      service.StateRunning,
		Scope:      service.ScopeSystem,
		Name:       service.ServiceName,
		UnitPath:   "/etc/systemd/system/hfdesk.service",
		Executable: "/usr/local/bin/hfdesk",
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: "/etc/hfdesk/hfdesk.json",
			StateDir:   "/var/lib/hfdesk",
			LogDir:     "/var/log/hfdesk",
		},
	}

	var buf bytes.Buffer
	renderStatus(&buf, service.ScopeSystem, status)

	out := buf.String()
	for _, want := range []string{
		service.ServiceName,
		"running",
		"127.0.0.1:8080",
		"/etc/systemd/system/hfdesk.service",
		"/usr/local/bin/hfdesk",
		"/etc/hfdesk/hfdesk.json",
		"/var/lib/hfdesk",
		"/var/log/hfdesk",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the status does not report %q:\n%s", want, out)
		}
	}
}

// TestRenderStatusReportsTheCacheRootOfTheInstance checks that the status
// reports the cache root the instance uses, and not the cache of the user
// that runs the command.
func TestRenderStatusReportsTheCacheRootOfTheInstance(t *testing.T) {
	status := service.Status{
		State:      service.StateRunning,
		Scope:      service.ScopeUser,
		Name:       service.ServiceName,
		UnitPath:   "/etc/systemd/user/hfdesk.service",
		Executable: "/usr/local/bin/hfdesk",
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: "/etc/hfdesk/hfdesk.json",
			StateDir:   "/var/lib/hfdesk",
			LogDir:     "/var/log/hfdesk",
		},
		CacheDir: "/srv/instance-cache",
	}

	var buf bytes.Buffer
	renderStatus(&buf, service.ScopeUser, status)

	out := buf.String()
	if !strings.Contains(out, "/srv/instance-cache") {
		t.Errorf("the status does not report the cache root of the instance:\n%s", out)
	}
	if personal := personalCacheRoot(t); personal != "" && strings.Contains(out, personal) {
		t.Errorf("the status reports the personal cache of the invoking user %q:\n%s", personal, out)
	}
}

// TestRenderStatusFallsBackToTheConfigFileOfTheInstance checks that the
// status reports the cache root of the config file of the instance when the
// supervisor does not report one.
func TestRenderStatusFallsBackToTheConfigFileOfTheInstance(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "hfdesk.json")
	cacheRoot := filepath.Join(dir, "instance-cache")
	writeConfigFile(t, configFile, cacheRoot)

	status := service.Status{
		State: service.StateRunning,
		Scope: service.ScopeUser,
		Paths: runtimepaths.Paths{
			ConfigFile: configFile,
			StateDir:   filepath.Join(dir, "state"),
			LogDir:     filepath.Join(dir, "logs"),
		},
	}

	var buf bytes.Buffer
	renderStatus(&buf, service.ScopeUser, status)

	out := buf.String()
	if !strings.Contains(out, cacheRoot) {
		t.Errorf("the status does not report the cache root %q of the config file:\n%s", cacheRoot, out)
	}
	if personal := personalCacheRoot(t); personal != "" && strings.Contains(out, personal) {
		t.Errorf("the status reports the personal cache of the invoking user %q:\n%s", personal, out)
	}
}

// TestRenderStatusReportsAnUnrecordedCacheRoot checks that a cache root
// that no source names is reported as unknown instead of the default of the
// scope or the personal cache of the invoking user.
func TestRenderStatusReportsAnUnrecordedCacheRoot(t *testing.T) {
	dir := t.TempDir()
	status := service.Status{
		State: service.StateNotInstalled,
		Scope: service.ScopeUser,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(dir, "missing.json"),
			StateDir:   filepath.Join(dir, "state"),
			LogDir:     filepath.Join(dir, "logs"),
		},
	}

	var buf bytes.Buffer
	renderStatus(&buf, service.ScopeUser, status)

	out := buf.String()
	if !strings.Contains(out, "not recorded") {
		t.Errorf("the status does not report that the cache root is not recorded:\n%s", out)
	}
	if personal := personalCacheRoot(t); personal != "" && strings.Contains(out, personal) {
		t.Errorf("the status reports the personal cache of the invoking user %q:\n%s", personal, out)
	}
}

// TestInstallSummaryReportsTheStateOfTheInstance checks that the summary of
// the installation reports the state the instance reached and that the hint
// that starts the instance is printed for every state that is not an active
// one.
func TestInstallSummaryReportsTheStateOfTheInstance(t *testing.T) {
	opts := service.InstallOptions{
		Scope:      service.ScopeUser,
		Executable: "/usr/local/bin/hfdesk",
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: "/etc/hfdesk/hfdesk.json",
			StateDir:   "/var/lib/hfdesk",
			LogDir:     "/var/log/hfdesk",
		},
		CacheDir: "/srv/cache",
	}

	for _, tc := range []struct {
		state    service.State
		wantHint bool
	}{
		{service.StateRunning, false},
		{service.StateStarting, false},
		{service.StateStopped, true},
		{service.StateFailed, true},
	} {
		var buf bytes.Buffer
		mgr := fakeManager{status: service.Status{
			State:    tc.state,
			Scope:    service.ScopeUser,
			UnitPath: "/etc/systemd/user/hfdesk.service",
		}}
		writeInstallSummary(&buf, context.Background(), mgr, service.ScopeUser, opts)

		out := buf.String()
		if !strings.Contains(out, "State:") || !strings.Contains(out, string(tc.state)) {
			t.Errorf("the install summary does not report the state %q:\n%s", tc.state, out)
		}
		hint := strings.Contains(out, "Start it with: hfdesk service start")
		if hint != tc.wantHint {
			t.Errorf("the install summary prints the start hint for the state %q: %v, want %v:\n%s",
				tc.state, hint, tc.wantHint, out)
		}
		if strings.Contains(out, "not started") {
			t.Errorf("the install summary claims that the instance was not started:\n%s", out)
		}
	}
}

// TestInstallSummaryReportsAFailedStatusQuery checks that a state that could
// not be determined is reported as such, and that the successful
// registration is reported nevertheless.
func TestInstallSummaryReportsAFailedStatusQuery(t *testing.T) {
	opts := service.InstallOptions{
		Scope:      service.ScopeUser,
		Executable: "/usr/local/bin/hfdesk",
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: "/etc/hfdesk/hfdesk.json",
			StateDir:   "/var/lib/hfdesk",
			LogDir:     "/var/log/hfdesk",
		},
		CacheDir: "/srv/cache",
	}

	var buf bytes.Buffer
	mgr := fakeManager{err: errors.New("the supervisor is not reachable")}
	writeInstallSummary(&buf, context.Background(), mgr, service.ScopeUser, opts)

	out := buf.String()
	if !strings.Contains(out, "The service was registered.") {
		t.Errorf("the install summary does not report the successful registration:\n%s", out)
	}
	if !strings.Contains(out, "could not be determined") {
		t.Errorf("the install summary does not report the failed status query:\n%s", out)
	}
	for _, state := range []string{"running", "stopped", "starting"} {
		if strings.Contains(out, state) {
			t.Errorf("the install summary claims the state %q it could not determine:\n%s", state, out)
		}
	}
}

// TestInstallSummaryReportsTheEffectiveCacheRoot checks that the summary of
// the installation names the cache root of the config file of the instance
// when it defines one.
func TestInstallSummaryReportsTheEffectiveCacheRoot(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "hfdesk.json")
	cacheRoot := filepath.Join(dir, "instance-cache")
	writeConfigFile(t, configFile, cacheRoot)

	opts := service.InstallOptions{
		Scope:      service.ScopeUser,
		Executable: "/usr/local/bin/hfdesk",
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: configFile,
			StateDir:   filepath.Join(dir, "state"),
			LogDir:     filepath.Join(dir, "logs"),
		},
		CacheDir: "/srv/cache",
	}

	var buf bytes.Buffer
	mgr := fakeManager{status: service.Status{State: service.StateRunning}}
	writeInstallSummary(&buf, context.Background(), mgr, service.ScopeUser, opts)

	out := buf.String()
	if !strings.Contains(out, cacheRoot) {
		t.Errorf("the install summary does not report the cache root %q of the config file:\n%s", cacheRoot, out)
	}
}

// TestInstallSummaryReportsTheCachePairOfTheInstance checks that the
// summary of the installation names the cache root and the hub directory of
// the pair the instance uses.
//
// A summary that names a cache root without its hub directory hides a layout
// that sends the blobs of the instance somewhere else.
func TestInstallSummaryReportsTheCachePairOfTheInstance(t *testing.T) {
	opts := service.InstallOptions{
		Scope:      service.ScopeUser,
		Executable: "/usr/local/bin/hfdesk",
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(t.TempDir(), "hfdesk.json"),
			StateDir:   "/var/lib/hfdesk",
			LogDir:     "/var/log/hfdesk",
		},
		CacheDir: "/srv/cache",
		HubDir:   "/srv/cache/hub",
	}

	var buf bytes.Buffer
	mgr := fakeManager{status: service.Status{State: service.StateRunning}}
	writeInstallSummary(&buf, context.Background(), mgr, service.ScopeUser, opts)

	out := buf.String()
	if got := summaryField(out, "Cache root"); got != "/srv/cache" {
		t.Errorf("the cache root of the summary is %q, want %q:\n%s", got, "/srv/cache", out)
	}
	if got := summaryField(out, "Hub dir"); got != "/srv/cache/hub" {
		t.Errorf("the hub dir of the summary is %q, want %q:\n%s", got, "/srv/cache/hub", out)
	}
}

// TestInstallSummaryReportsTheHubDirOfTheConfigFile checks that the summary
// names the hub directory of the cache root the config file of the instance
// defines: the config file owns the whole cache layout of the instance.
func TestInstallSummaryReportsTheHubDirOfTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "hfdesk.json")
	cacheRoot := filepath.Join(dir, "instance-cache")
	writeConfigFile(t, configFile, cacheRoot)

	opts := service.InstallOptions{
		Scope:      service.ScopeUser,
		Executable: "/usr/local/bin/hfdesk",
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: configFile,
			StateDir:   filepath.Join(dir, "state"),
			LogDir:     filepath.Join(dir, "logs"),
		},
		CacheDir: "/srv/cache",
		HubDir:   "/srv/cache/hub",
	}

	var buf bytes.Buffer
	mgr := fakeManager{status: service.Status{State: service.StateRunning}}
	writeInstallSummary(&buf, context.Background(), mgr, service.ScopeUser, opts)

	out := buf.String()
	if got := summaryField(out, "Cache root"); got != cacheRoot {
		t.Errorf("the cache root of the summary is %q, want %q:\n%s", got, cacheRoot, out)
	}
	wantHub := filepath.Join(cacheRoot, "hub")
	if got := summaryField(out, "Hub dir"); got != wantHub {
		t.Errorf("the hub dir of the summary is %q, want %q:\n%s", got, wantHub, out)
	}
}

// TestUninstallSummaryNamesTheEffectiveCacheRoot checks that the summary of
// the uninstall names the cache root the instance uses.
func TestUninstallSummaryNamesTheEffectiveCacheRoot(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "hfdesk.json")
	cacheRoot := filepath.Join(dir, "instance-cache")
	writeConfigFile(t, configFile, cacheRoot)

	for _, tc := range []struct {
		name   string
		status service.Status
		want   string
	}{
		{
			name: "the cache root of the config file of the instance",
			status: service.Status{
				Paths: runtimepaths.Paths{
					ConfigFile: configFile,
					StateDir:   filepath.Join(dir, "state"),
					LogDir:     filepath.Join(dir, "logs"),
				},
			},
			want: cacheRoot,
		},
		{
			name: "the cache root of the registration",
			status: service.Status{
				Paths: runtimepaths.Paths{
					ConfigFile: filepath.Join(dir, "missing.json"),
					StateDir:   filepath.Join(dir, "state"),
					LogDir:     filepath.Join(dir, "logs"),
				},
				CacheDir: "/srv/registered-cache",
			},
			want: "/srv/registered-cache",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := fakeManager{status: tc.status}
			paths, reported := instanceDataPaths(context.Background(), mgr, service.ScopeUser)

			var buf bytes.Buffer
			writeUninstallSummary(&buf, service.ScopeUser, paths, reported)

			out := buf.String()
			if !strings.Contains(out, tc.want) {
				t.Errorf("the uninstall summary does not name the cache root %q:\n%s", tc.want, out)
			}
			if personal := personalCacheRoot(t); personal != "" && strings.Contains(out, personal) {
				t.Errorf("the uninstall summary reports the personal cache of the invoking user %q:\n%s", personal, out)
			}
		})
	}
}

func TestNonLoopbackWarning(t *testing.T) {
	warning := nonLoopbackWarning("0.0.0.0:8080")
	if warning == "" {
		t.Error("a non-loopback address must produce a warning")
	}
	if !strings.Contains(warning, "0.0.0.0:8080") {
		t.Errorf("the warning does not name the address:\n%s", warning)
	}

	if got := nonLoopbackWarning("127.0.0.1:8080"); got != "" {
		t.Errorf("a loopback address must not produce a warning, got:\n%s", got)
	}
	if got := nonLoopbackWarning(""); got != "" {
		t.Errorf("an unknown address must not produce a warning, got:\n%s", got)
	}
}

func TestUpgradeWarning(t *testing.T) {
	if got := upgradeWarning("/usr/local/bin/hfdesk"); got != "" {
		t.Errorf("a stable path must not produce a warning, got:\n%s", got)
	}
	if got := upgradeWarning("/tmp/hfdesk-1.2.3/hfdesk"); got == "" {
		t.Error("a version-stamped path must produce a warning")
	}
	if got := upgradeWarning(filepath.Join(os.TempDir(), "hfdesk", "hfdesk")); got == "" {
		t.Error("a temporary path must produce a warning")
	}
}

// TestUpgradeWarningDescribesTheRegisteredPath checks that the warning is
// about the path the registration points at.
//
// The registration keeps the name the installation is known by, so a stable
// name stays quiet even when the file behind it is a version-stamped
// release artifact: the next release replaces the file, the registration
// does not change.
func TestUpgradeWarningDescribesTheRegisteredPath(t *testing.T) {
	if got := upgradeWarning("/usr/local/bin/hfdesk"); got != "" {
		t.Errorf("a stable registered path must not produce a warning, got:\n%s", got)
	}
	if got := upgradeWarning("/opt/hfdesk-1.2.3/hfdesk"); got == "" {
		t.Error("a version-stamped registered path must produce a warning")
	}
}

// TestServiceInstallRejectsAnUnusableExecutable checks that the
// installation is refused when the named binary cannot be registered.
//
// The registration points at that exact path, so a path that is not a
// program would break the instance at the first start.
func TestServiceInstallRejectsAnUnusableExecutable(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(plain, []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"a relative path", "bin/hfdesk", "absolute"},
		{"a missing file", filepath.Join(dir, "missing"), "not usable"},
		{"a directory", dir, "regular file"},
		{"a file that is not executable", plain, "not executable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runServiceCommand([]string{"install", "--system", "--executable", tc.path}, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("the installation must be refused, got exit code %d", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("the error does not report %q:\n%s", tc.want, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("a refused installation must not report a registration:\n%s", stdout.String())
			}
		})
	}
}

// TestResolveExecutableKeepsTheStableNameOfTheInvocation checks that the
// registration points at the name the user invoked.
//
// os.Executable() resolves a symbolic link and returns its target, so a
// registration built from it points at the version-stamped release artifact
// instead of at the stable name of the installation. The invocation path
// keeps the name.
func TestResolveExecutableKeepsTheStableNameOfTheInvocation(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "hfdesk-1.2.3", "hfdesk")
	if err := os.MkdirAll(filepath.Dir(release), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(root, "bin", "hfdesk")
	if err := os.MkdirAll(filepath.Dir(stable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release, stable); err != nil {
		t.Fatal(err)
	}

	got, err := resolveExecutable("", stable)
	if err != nil {
		t.Fatalf("resolveExecutable() = %v, want nil", err)
	}
	if got != stable {
		t.Errorf("the registered executable is %q, want the invoked path %q", got, stable)
	}
}

// TestUpgradeWarningStaysQuietForAStableName checks the whole chain: a
// stable name that points at a version-stamped file does not produce a
// warning.
//
// The registration keeps the name the installation is known by, and the next
// release replaces the file behind that name.
func TestUpgradeWarningStaysQuietForAStableName(t *testing.T) {
	root := treeOutsideTempDir(t)
	release := filepath.Join(root, "hfdesk-1.2.3", "hfdesk")
	if err := os.MkdirAll(filepath.Dir(release), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(root, "bin", "hfdesk")
	if err := os.MkdirAll(filepath.Dir(stable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release, stable); err != nil {
		t.Fatal(err)
	}

	registered, err := resolveExecutable("", stable)
	if err != nil {
		t.Fatalf("resolveExecutable() = %v, want nil", err)
	}
	if registered != stable {
		t.Fatalf("the registered executable is %q, want the invoked path %q", registered, stable)
	}
	if got := upgradeWarning(registered); got != "" {
		t.Errorf("a stable registered path must not produce a warning, got:\n%s", got)
	}
	// The version-stamped path itself is a warning: it does not survive
	// the next upgrade.
	if got := upgradeWarning(release); got == "" {
		t.Error("a version-stamped registered path must produce a warning")
	}
}

// treeOutsideTempDir returns a directory that is not inside the temporary
// directory of the process.
//
// The upgrade warning treats a path inside the temporary directory as a path
// that does not survive an upgrade, so a test of the warning needs a tree
// outside of it.
func treeOutsideTempDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	// The temporary directory of the process moves next to the tree, so
	// the tree is not inside it.
	moved := filepath.Join(base, "tmp")
	t.Setenv("TMPDIR", moved)
	t.Setenv("TMP", moved)
	t.Setenv("TEMP", moved)
	return filepath.Join(base, "tree")
}

// TestResolveExecutablePrefersTheInvocationPath checks that the path the
// user typed is the one the registration points at.
func TestResolveExecutablePrefersTheInvocationPath(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "hfdesk")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveExecutable("", bin)
	if err != nil {
		t.Fatalf("resolveExecutable() = %v, want nil", err)
	}
	if got != bin {
		t.Errorf("the registered executable is %q, want the invoked path %q", got, bin)
	}
}

// TestResolveExecutableFallsBackToTheRunningBinary checks that a bare name
// from the search path of the shell is never resolved against the working
// directory.
//
// The name of a command of the search path says nothing about the file the
// user wants to register: it is resolved against the working directory of
// the install command, which is not where the binary lives.
func TestResolveExecutableFallsBackToTheRunningBinary(t *testing.T) {
	root := t.TempDir()
	// A file with the name of the command in the working directory.
	if err := os.WriteFile(filepath.Join(root, "hfdesk"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	got, err := resolveExecutable("", "hfdesk")
	if err != nil {
		t.Fatalf("resolveExecutable() = %v, want nil", err)
	}
	if got == filepath.Join(root, "hfdesk") {
		t.Errorf("the registered executable is %q, want the path of the running binary", got)
	}

	running, err := os.Executable()
	if err != nil {
		t.Fatalf("the path of the running binary: %v", err)
	}
	want, err := filepath.Abs(running)
	if err != nil {
		t.Fatalf("the absolute path of the running binary: %v", err)
	}
	if got != want {
		t.Errorf("the registered executable is %q, want the path of the running binary %q", got, want)
	}
}

// TestResolveExecutableRejectsAnUnusableExplicitPath checks that an
// explicit path has to be a program that can be started.
func TestResolveExecutableRejectsAnUnusableExplicitPath(t *testing.T) {
	root := t.TempDir()
	program := filepath.Join(root, "hfdesk")
	if err := os.WriteFile(program, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(plain, []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want string
	}{
		{"a relative path", "bin/hfdesk", "absolute"},
		{"a missing file", filepath.Join(root, "missing"), "not usable"},
		{"a directory", root, "regular file"},
		{"a file that is not executable", plain, "not executable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveExecutable(tc.path, "")
			if err == nil {
				t.Fatalf("resolveExecutable(%q) = %q, want an error", tc.path, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error of resolveExecutable(%q) = %v, want it to mention %q", tc.path, err, tc.want)
			}
		})
	}
}

// TestResolveExecutableUsesTheExplicitPath checks that an explicit path
// wins over the invoked binary.
func TestResolveExecutableUsesTheExplicitPath(t *testing.T) {
	root := t.TempDir()
	program := filepath.Join(root, "hfdesk")
	if err := os.WriteFile(program, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveExecutable(program, filepath.Join(root, "other"))
	if err != nil {
		t.Fatalf("resolveExecutable() = %v, want nil", err)
	}
	if got != program {
		t.Errorf("the registered executable is %q, want the explicit path %q", got, program)
	}
}

func TestRunPathReturnsAfterTheContextIsCancelled(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")

	// The host installs the path model process wide; the interactive
	// resolution is restored afterwards.
	t.Cleanup(runtimepaths.Reset)

	// The host drops the token of the environment; it is restored so the
	// rest of the test run keeps its environment.
	token, hadToken := os.LookupEnv("HF_TOKEN")
	t.Cleanup(func() {
		if hadToken {
			_ = os.Setenv("HF_TOKEN", token)
			return
		}
		_ = os.Unsetenv("HF_TOKEN")
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	args := []string{
		"--config", filepath.Join(stateDir, "hfdesk.json"),
		"--state-dir", stateDir,
		"--log-dir", filepath.Join(stateDir, "logs"),
		"--listen", net.JoinHostPort("127.0.0.1", strconv.Itoa(freeLoopbackPort(t))),
	}

	done := make(chan error, 1)
	go func() { done <- runServiceHost(ctx, args) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the host returned an error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the host did not return after the context was cancelled")
	}
}

// freeLoopbackPort returns a port of the loopback interface that is free at
// the moment.
//
// The service layer rejects port 0, so the host needs a real port. The port
// is reserved and released again, so the instance binds a port of its own
// instead of a well known one.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	defer func() { _ = listener.Close() }()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("the reserved address is %T, want a TCP address", listener.Addr())
	}
	return addr.Port
}

func TestRunPathRejectsAnUnknownFlag(t *testing.T) {
	if err := runServiceHost(context.Background(), []string{"--unknown", "value"}); err == nil {
		t.Error("an unknown flag must be rejected")
	}
}

func TestInteractiveFlagsAreUnchanged(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	registered := map[string]bool{}
	for _, match := range interactiveFlagPattern.FindAllStringSubmatch(string(source), -1) {
		registered[match[1]] = true
	}

	for _, name := range []string{"port", "cache-dir", "token", "local-dir", "stall-timeout", "open", "no-open"} {
		if !registered[name] {
			t.Errorf("the interactive command line does not register the %q flag any more", name)
		}
	}
}

func TestServiceDispatchIsExact(t *testing.T) {
	if !isServiceCommand([]string{"service", "status"}) {
		t.Error("the token \"service\" must select the service command family")
	}
	// The token alone selects the family as well: the family itself
	// reports the missing subcommand.
	if !isServiceCommand([]string{"service"}) {
		t.Error("the token \"service\" alone must select the service command family")
	}
	for _, args := range [][]string{
		nil,
		{},
		{"--port", "9090"},
		{"--help"},
		{"services"},
		{"Service"},
	} {
		if isServiceCommand(args) {
			t.Errorf("%v must not select the service command family", args)
		}
	}
}
