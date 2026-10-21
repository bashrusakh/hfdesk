// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
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
