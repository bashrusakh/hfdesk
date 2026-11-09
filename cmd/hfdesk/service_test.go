// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/app"
	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/service"
)

// fakeManager is a service.Manager that answers with a fixed status.
//
// The command family is exercised without touching the system of the test
// run: the manager of the platform supervisor is replaced by this one.
type fakeManager struct {
	status service.Status
	err    error
	// opErr is the failure of every lifecycle operation. It is the
	// answer of a supervisor that refuses the operation, e.g. a service
	// that is not installed.
	opErr error
}

func (m fakeManager) Install(context.Context, service.InstallOptions) error { return m.opErr }

func (m fakeManager) Uninstall(context.Context) error { return m.opErr }

func (m fakeManager) Start(context.Context) error { return m.opErr }

func (m fakeManager) Stop(context.Context) error { return m.opErr }

func (m fakeManager) Restart(context.Context) error { return m.opErr }

func (m fakeManager) Status(context.Context) (service.Status, error) {
	return m.status, m.err
}

// useFakeManager replaces the manager of the platform supervisor for the
// duration of the test.
//
// The command family never touches the system of the test run: the manager
// of the platform is the fake one, so a lifecycle command can be driven
// through every answer of a supervisor.
func useFakeManager(t *testing.T, mgr service.Manager) {
	t.Helper()
	previous := newServiceManager
	newServiceManager = func(service.Scope) (service.Manager, error) { return mgr, nil }
	t.Cleanup(func() { newServiceManager = previous })
}

// platformFactsByName holds the facts of the platforms of the service
// layer.
//
// The facts are the ones the service layer defines per platform: the test
// injects them, so the behaviour of a Windows instance is covered by a test
// that runs on Linux as well. The guard that keeps them in step with the
// service layer is TestServicePerPlatformFacts.
var platformFactsByName = map[string]platformFacts{
	"linux": {
		defaultScope:                service.ScopeUser,
		supportsUserScope:           true,
		supportsNamedServiceAccount: true,
	},
	"darwin": {
		defaultScope:                service.ScopeUser,
		supportsUserScope:           true,
		supportsNamedServiceAccount: true,
	},
	"windows": {
		defaultScope:                service.ScopeSystem,
		supportsUserScope:           false,
		supportsNamedServiceAccount: false,
	},
}

// usePlatform makes the command line behave as on the given platform for
// the duration of the test.
//
// The platform is the seam of the tests: a decision of the command line
// about Windows is checked on every platform, so the behaviour of a
// Windows instance is covered by a test that runs on Linux as well.
func usePlatform(t *testing.T, goos string) {
	t.Helper()
	facts, ok := platformFactsByName[goos]
	if !ok {
		t.Fatalf("the test does not know the facts of the platform %q", goos)
	}
	previous := hostPlatform
	hostPlatform = facts
	t.Cleanup(func() { hostPlatform = previous })
}

// scopeOfPlatform returns the scope an installation uses on the given
// platform.
//
// The scope is resolved by the command line itself, so a test states the
// scope of a platform the same way an installation does: the user scope on
// Linux and macOS, the machine-wide scope on Windows.
func scopeOfPlatform(t *testing.T, goos string) service.Scope {
	t.Helper()

	usePlatform(t, goos)
	scope, err := resolveScope(false, false)
	if err != nil {
		t.Fatalf("the default scope of %s must resolve: %v", goos, err)
	}
	return scope
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

// usageOptionText returns the description of an option in a usage block:
// the line that names the option and the lines below it that belong to it.
//
// The option is named with the dashes it is written with, so the option of
// the family ("--service-user") and the option of the flag set of a
// subcommand ("-service-user") are two different names.
func usageOptionText(out, option string) string {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), option) {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		text := strings.TrimSpace(line)
		for j := i + 1; j < len(lines); j++ {
			next := lines[j]
			if strings.TrimSpace(next) == "" {
				break
			}
			if len(next)-len(strings.TrimLeft(next, " ")) <= indent {
				break
			}
			text += " " + strings.TrimSpace(next)
		}
		return text
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
		if code != serviceUsageExitCode {
			t.Errorf("the %q subcommand must reject --listen with the exit code %d, got %d", sub, serviceUsageExitCode, code)
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
			if code != serviceUsageExitCode {
				t.Errorf("the %q subcommand must reject %s with the exit code %d, got %d", sub, option[0], serviceUsageExitCode, code)
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
//
// The scope itself does not exist on the platform that has no per-user
// installation: the service control manager of Windows registers
// machine-wide services only, so the scope is refused there before the
// account is looked at.
func TestServiceUserIsRejectedForTheUserScope(t *testing.T) {
	for _, tc := range []struct {
		goos      string
		userScope bool
	}{
		{goos: "linux", userScope: true},
		{goos: "darwin", userScope: true},
		{goos: "windows"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			usePlatform(t, tc.goos)

			var stdout, stderr bytes.Buffer
			code := runServiceCommand([]string{"install", "--user", "--service-user", "hfdesk"}, &stdout, &stderr)
			if code != serviceUsageExitCode {
				t.Fatalf("the user scope must reject --service-user with the exit code %d, got %d", serviceUsageExitCode, code)
			}

			errText := stderr.String()
			if !tc.userScope {
				if !strings.Contains(errText, "--user") {
					t.Errorf("the error does not name the rejected selector:\n%s", errText)
				}
				return
			}
			if !strings.Contains(errText, "--service-user") {
				t.Errorf("the error does not name the rejected option:\n%s", errText)
			}
			if !strings.Contains(errText, "user") {
				t.Errorf("the error does not explain the scope of the option:\n%s", errText)
			}
		})
	}
}

// TestServiceUserIsRejectedOnWindows checks that the account of a
// machine-wide instance cannot be chosen on Windows.
//
// The service control manager registers the instance as the built-in
// LocalService account, and a service under a user account would need a
// credential, which must never reach the command line of an installation:
// the account is not a choice of the command line on this platform, so a
// command line that names one is refused.
func TestServiceUserIsRejectedOnWindows(t *testing.T) {
	usePlatform(t, "windows")

	var stdout, stderr bytes.Buffer
	code := runServiceCommand([]string{"install", "--system", "--service-user", "hfdesk-svc"}, &stdout, &stderr)
	if code != serviceUsageExitCode {
		t.Fatalf("the machine-wide installation must refuse --service-user with the exit code %d, got %d:\n%s",
			serviceUsageExitCode, code, stderr.String())
	}

	errText := stderr.String()
	if !strings.Contains(errText, "--service-user") {
		t.Errorf("the error does not name the rejected option:\n%s", errText)
	}
	if !strings.Contains(errText, windowsServiceAccount) {
		t.Errorf("the error does not name the account the instance really runs as (%q):\n%s", windowsServiceAccount, errText)
	}
	if !strings.Contains(errText, "cannot be chosen") {
		t.Errorf("the error does not say that the account cannot be chosen:\n%s", errText)
	}
	if stdout.Len() != 0 {
		t.Errorf("a refused installation must not report a registration:\n%s", stdout.String())
	}
}

// TestServiceUserIsRejectedForTheUserScopeOnWindows checks that a user scope
// installation with a named account is refused on Windows.
//
// The user scope does not exist on Windows, so the scope itself is refused
// before the account is looked at: the command line is refused either way.
func TestServiceUserIsRejectedForTheUserScopeOnWindows(t *testing.T) {
	usePlatform(t, "windows")

	var stdout, stderr bytes.Buffer
	code := runServiceCommand([]string{"install", "--user", "--service-user", "hfdesk-svc"}, &stdout, &stderr)
	if code != serviceUsageExitCode {
		t.Fatalf("the user scope must be refused with the exit code %d, got %d:\n%s",
			serviceUsageExitCode, code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("a refused installation must not report a registration:\n%s", stdout.String())
	}
}

// TestBuildInstallOptionsCarriesTheServiceUser checks that the account
// reaches the options of the installation.
//
// The account is empty when the command line does not name one: the default
// of the platform is resolved by the platform manager, which knows the
// accounts of the machine.
//
// The account reaches the options only on the platforms that have a named
// service account: on Windows the machine-wide instance runs as the built-in
// LocalService account, so an account of the command line is refused there
// with the message that names the account the instance really runs as.
func TestBuildInstallOptionsCarriesTheServiceUser(t *testing.T) {
	const executable = "/usr/local/bin/hfdesk"

	for _, tc := range []struct {
		goos      string
		namedUser bool
	}{
		{goos: "linux", namedUser: true},
		{goos: "darwin", namedUser: true},
		{goos: "windows"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			usePlatform(t, tc.goos)

			opts, err := buildInstallOptions(service.ScopeSystem, installFlags{}, executable)
			if err != nil {
				t.Fatalf("build the install options: %v", err)
			}
			if opts.ServiceUser != "" {
				t.Errorf("the service account is %q, want it to be unset", opts.ServiceUser)
			}

			opts, err = buildInstallOptions(service.ScopeSystem, installFlags{serviceUser: "hfdesk-svc"}, executable)
			if !tc.namedUser {
				if err == nil {
					t.Fatalf("the machine-wide scope must refuse a named account on %s", tc.goos)
				}
				if !isServiceUsageError(err) {
					t.Errorf("the refusal must be a command line that could not be understood: %v", err)
				}
				if !strings.Contains(err.Error(), windowsServiceAccount) {
					t.Errorf("the refusal does not name the account the instance really runs as (%q): %v", windowsServiceAccount, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("build the install options: %v", err)
			}
			if opts.ServiceUser != "hfdesk-svc" {
				t.Errorf("the service account is %q, want %q", opts.ServiceUser, "hfdesk-svc")
			}
		})
	}
}

// TestBuildInstallOptionsRejectsTheServiceUserForTheUserScope checks that
// a user scope instance never carries a service account.
//
// The refusal does not depend on the platform: a user scope instance runs as
// the user itself, so an account name would be a promise the installation
// cannot keep.
func TestBuildInstallOptionsRejectsTheServiceUserForTheUserScope(t *testing.T) {
	if _, err := buildInstallOptions(service.ScopeUser, installFlags{serviceUser: "hfdesk"}, "/usr/local/bin/hfdesk"); err == nil {
		t.Error("the user scope must reject a service account")
	}
}

// TestBuildInstallOptionsRejectsTheServiceUserOnWindows checks that a
// machine-wide installation never carries a named account on Windows.
//
// The registration of a Windows instance runs as the built-in LocalService
// account, so an account in the options would be a promise the
// registration cannot keep.
func TestBuildInstallOptionsRejectsTheServiceUserOnWindows(t *testing.T) {
	usePlatform(t, "windows")

	_, err := buildInstallOptions(service.ScopeSystem, installFlags{serviceUser: "hfdesk-svc"}, "/usr/local/bin/hfdesk")
	if err == nil {
		t.Fatal("the machine-wide scope must reject a service account on windows")
	}
	if !isServiceUsageError(err) {
		t.Errorf("the refusal must be a command line that could not be understood: %v", err)
	}
	if !strings.Contains(err.Error(), windowsServiceAccount) {
		t.Errorf("the refusal does not name the account the instance really runs as (%q): %v", windowsServiceAccount, err)
	}
}

// TestBuildInstallOptionsKeepsTheServiceUserOnLinuxAndMacOS checks that the
// account of a machine-wide instance is honoured on the platforms that have
// a named service account.
func TestBuildInstallOptionsKeepsTheServiceUserOnLinuxAndMacOS(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			usePlatform(t, goos)

			opts, err := buildInstallOptions(service.ScopeSystem, installFlags{serviceUser: "hfdesk-svc"}, "/usr/local/bin/hfdesk")
			if err != nil {
				t.Fatalf("build the install options: %v", err)
			}
			if opts.ServiceUser != "hfdesk-svc" {
				t.Errorf("the service account is %q, want %q", opts.ServiceUser, "hfdesk-svc")
			}
		})
	}
}

// TestBuildInstallOptionsRejectsTheServiceUserForTheUserScopeEverywhere
// checks that a user scope instance never carries a service account, on
// every platform.
//
// A user scope instance runs as the user itself, so an account name would
// be a promise the installation cannot keep.
func TestBuildInstallOptionsRejectsTheServiceUserForTheUserScopeEverywhere(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			usePlatform(t, goos)

			if _, err := buildInstallOptions(service.ScopeUser, installFlags{serviceUser: "hfdesk"}, "/usr/local/bin/hfdesk"); err == nil {
				t.Error("the user scope must reject a service account")
			}
		})
	}
}

// TestInstallSummaryReportsTheAccountOfTheInstance checks that the summary
// names the account the instance really runs as.
//
// A machine-wide instance runs as the account the command line named on the
// platforms that have a named service account, and as the built-in
// LocalService account on Windows, where the account of the command line
// cannot be chosen and is never reported.
//
// A user scope instance runs as the invoking user, on the platforms that
// have the scope at all.
func TestInstallSummaryReportsTheAccountOfTheInstance(t *testing.T) {
	for _, tc := range []struct {
		goos      string
		namedUser bool
		userScope bool
	}{
		{goos: "linux", namedUser: true, userScope: true},
		{goos: "darwin", namedUser: true, userScope: true},
		{goos: "windows"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			usePlatform(t, tc.goos)

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
			want := "hfdesk-svc"
			if !tc.namedUser {
				want = windowsServiceAccount
			}
			if got := summaryField(out, "Account"); got != want {
				t.Errorf("the account of the summary is %q, want %q:\n%s", got, want, out)
			}
			if !tc.namedUser && strings.Contains(out, "hfdesk-svc") {
				t.Errorf("the summary reports the account %q the instance does not run as:\n%s", "hfdesk-svc", out)
			}

			if !tc.userScope {
				return
			}
			buf.Reset()
			writeInstallSummary(&buf, context.Background(), mgr, service.ScopeUser, opts)

			out = buf.String()
			want = invokingUser()
			if want == "" {
				want = "unknown"
			}
			if got := summaryField(out, "Account"); got != want {
				t.Errorf("the account of the summary is %q, want the invoking user %q:\n%s", got, want, out)
			}
		})
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

// TestServiceUsageDescribesTheServiceUserOption checks that the usage
// block of the family says where the account of a machine-wide instance
// can be named.
//
// The option is not available on every platform: the account of a Windows
// instance cannot be chosen, so a usage block that offers it everywhere
// promises more than the command line can keep.
func TestServiceUsageDescribesTheServiceUserOption(t *testing.T) {
	var buf bytes.Buffer
	writeServiceUsage(&buf)

	out := buf.String()
	text := usageOptionText(out, "--service-user")
	if text == "" {
		t.Fatalf("the usage block does not describe --service-user:\n%s", out)
	}
	for _, want := range []string{"Linux", "macOS", "Windows"} {
		if !strings.Contains(text, want) {
			t.Errorf("the description of --service-user does not mention %q:\n%s", want, text)
		}
	}
}

// TestServiceInstallHelpDescribesTheServiceUserOption checks that the help
// of the install subcommand says where the account of a machine-wide
// instance can be named.
func TestServiceInstallHelpDescribesTheServiceUserOption(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runServiceCommand([]string{"install", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("the help of the install subcommand must succeed, got exit code %d:\n%s", code, stderr.String())
	}

	out := stdout.String()
	text := usageOptionText(out, "-service-user")
	if text == "" {
		t.Fatalf("the help of the install subcommand does not describe --service-user:\n%s", out)
	}
	for _, want := range []string{"Linux", "macOS", "Windows"} {
		if !strings.Contains(text, want) {
			t.Errorf("the description of --service-user does not mention %q:\n%s", want, text)
		}
	}
}

// TestWindowsServiceAccountIsTheNameOfTheRegistration checks that the
// account the command line names for a Windows instance is the account the
// registration uses.
//
// The service control manager registers the instance with the built-in
// LocalService account under its full name (the platform manager of the
// service layer carries the same name). A summary that names another
// account describes an identity the registration does not have.
func TestWindowsServiceAccountIsTheNameOfTheRegistration(t *testing.T) {
	const want = `NT AUTHORITY\LocalService`
	if windowsServiceAccount != want {
		t.Errorf("the account of a Windows instance is %q, want %q", windowsServiceAccount, want)
	}
}

// TestInstallSummaryReportsTheWindowsAccount checks that the summary of a
// machine-wide installation names the account the registration uses on
// Windows.
//
// The registration runs as the built-in LocalService account, so the
// summary names that account and never the one a command line named: a
// summary that reports an account the instance does not run as sends the
// reader to the wrong identity.
func TestInstallSummaryReportsTheWindowsAccount(t *testing.T) {
	usePlatform(t, "windows")

	for _, tc := range []struct {
		name        string
		serviceUser string
	}{
		{name: "without a named account"},
		{name: "with a named account", serviceUser: "hfdesk-svc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := service.InstallOptions{
				Scope:      service.ScopeSystem,
				Executable: "/usr/local/bin/hfdesk",
				Listen:     "127.0.0.1:8080",
				Paths: runtimepaths.Paths{
					ConfigFile: "/etc/hfdesk/hfdesk.json",
					StateDir:   "/var/lib/hfdesk",
					LogDir:     "/var/log/hfdesk",
				},
				ServiceUser: tc.serviceUser,
			}

			var buf bytes.Buffer
			mgr := fakeManager{status: service.Status{State: service.StateRunning}}
			writeInstallSummary(&buf, context.Background(), mgr, service.ScopeSystem, opts)

			out := buf.String()
			if got := summaryField(out, "Account"); got != windowsServiceAccount {
				t.Errorf("the account of the summary is %q, want %q:\n%s", got, windowsServiceAccount, out)
			}
			if tc.serviceUser != "" && strings.Contains(out, tc.serviceUser) {
				t.Errorf("the summary reports the account %q the registration does not use:\n%s", tc.serviceUser, out)
			}
		})
	}
}

// TestInstallSummaryReportsTheNamedAccountOnLinuxAndMacOS checks that the
// summary names the account of the command line on the platforms that
// honour it.
func TestInstallSummaryReportsTheNamedAccountOnLinuxAndMacOS(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			usePlatform(t, goos)

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
		})
	}
}

// TestInstallSummaryReportsTheInvokingUserForTheUserScope checks that the
// summary of a user scope installation names the user the instance runs as.
//
// A user scope instance runs as the user itself, so the summary names that
// user and never an account of a command line.
func TestInstallSummaryReportsTheInvokingUserForTheUserScope(t *testing.T) {
	usePlatform(t, "linux")

	opts := service.InstallOptions{
		Scope:      service.ScopeUser,
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
	writeInstallSummary(&buf, context.Background(), mgr, service.ScopeUser, opts)

	out := buf.String()
	got := summaryField(out, "Account")
	want := invokingUser()
	if want == "" {
		want = "unknown"
	}
	if got != want {
		t.Errorf("the account of the summary is %q, want the invoking user %q:\n%s", got, want, out)
	}
	if strings.Contains(out, "hfdesk-svc") {
		t.Errorf("the summary reports an account the instance does not run as:\n%s", out)
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
	if code != serviceUsageExitCode {
		t.Errorf("an unknown subcommand must exit with %d, got %d", serviceUsageExitCode, code)
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
	if code != serviceUsageExitCode {
		t.Errorf("a missing subcommand must exit with %d, got %d", serviceUsageExitCode, code)
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
	if code != serviceUsageExitCode {
		t.Errorf("an ambiguous scope must exit with %d, got %d", serviceUsageExitCode, code)
	}
	if !strings.Contains(stderr.String(), "--user") || !strings.Contains(stderr.String(), "--system") {
		t.Errorf("the error does not name both selectors:\n%s", stderr.String())
	}
}

// TestServiceScopeSelectorsAreExclusiveEverywhere checks that every
// subcommand that carries the scope selector rejects an ambiguous one.
//
// Two scopes are two installations, so an ambiguous command line is a
// command line that could not be understood, on every subcommand that
// carries the selector.
func TestServiceScopeSelectorsAreExclusiveEverywhere(t *testing.T) {
	for _, sub := range []string{"install", "uninstall", "start", "stop", "restart", "status"} {
		var stdout, stderr bytes.Buffer
		code := runServiceCommand([]string{sub, "--user", "--system"}, &stdout, &stderr)
		if code != serviceUsageExitCode {
			t.Errorf("the %q subcommand must reject two scopes with the exit code %d, got %d",
				sub, serviceUsageExitCode, code)
		}
		if !strings.Contains(stderr.String(), "--user") || !strings.Contains(stderr.String(), "--system") {
			t.Errorf("the error of the %q subcommand does not name both selectors:\n%s", sub, stderr.String())
		}
	}
}

// TestServiceRunRejectsAnInvalidVector checks that an argument vector the
// host mode cannot parse is a command line that could not be understood.
//
// The vector of the host mode is the one the supervisor invokes, so a
// broken vector is a mistake of the invocation and not a failed operation.
func TestServiceRunRejectsAnInvalidVector(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--config", "relative/hfdesk.json"},
		{"run", "--config", "/tmp/hfdesk.json"},
	} {
		var stdout, stderr bytes.Buffer
		code := runServiceCommand(args, &stdout, &stderr)
		if code != serviceUsageExitCode {
			t.Errorf("%v must exit with %d, got %d", args, serviceUsageExitCode, code)
		}
		if stderr.Len() == 0 {
			t.Errorf("%v is rejected without a reason", args)
		}
	}
}

// TestServiceScopeDefaults checks that the default scope of an installation
// is the scope of the platform.
//
// The user scope is the default on Linux and macOS, the system scope on
// Windows: the service control manager of Windows registers machine-wide
// services only. The user scope does not exist there, so asking for it is
// refused instead of answered.
func TestServiceScopeDefaults(t *testing.T) {
	for _, tc := range []struct {
		goos      string
		scope     service.Scope
		userScope bool
	}{
		{goos: "linux", scope: service.ScopeUser, userScope: true},
		{goos: "darwin", scope: service.ScopeUser, userScope: true},
		{goos: "windows", scope: service.ScopeSystem},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			usePlatform(t, tc.goos)

			scope, err := resolveScope(false, false)
			if err != nil {
				t.Fatalf("the default scope must resolve: %v", err)
			}
			if scope != tc.scope {
				t.Errorf("the default scope on %s is %q, want %q", tc.goos, scope, tc.scope)
			}

			system, err := resolveScope(false, true)
			if err != nil {
				t.Fatalf("the system scope must resolve: %v", err)
			}
			if system != service.ScopeSystem {
				t.Errorf("the system scope is %q, want %q", system, service.ScopeSystem)
			}

			user, err := resolveScope(true, false)
			if !tc.userScope {
				if err == nil {
					t.Fatalf("the user scope must be refused on %s", tc.goos)
				}
				if !isServiceUsageError(err) {
					t.Errorf("the refusal must be a command line that could not be understood: %v", err)
				}
				if !strings.Contains(err.Error(), "--user") {
					t.Errorf("the refusal does not name the rejected selector: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("the user scope must resolve: %v", err)
			}
			if user != service.ScopeUser {
				t.Errorf("the user scope is %q, want %q", user, service.ScopeUser)
			}
		})
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
		if code != serviceUsageExitCode {
			t.Errorf("%v must exit with %d, got %d", args, serviceUsageExitCode, code)
		}
		if !strings.Contains(stderr.String(), "absolute") {
			t.Errorf("%v is not reported as a relative path:\n%s", args, stderr.String())
		}
	}
}

// TestBuildInstallOptionsUsesTheDefaultsOfTheScope checks that the options
// of an installation carry the defaults of its scope.
//
// The scope is the scope of the platform: the user scope on Linux and macOS,
// the machine-wide scope on Windows. The defaults themselves belong to the
// service layer of the host, so the test states that the command line uses
// the defaults of the scope and not which paths these are.
//
// A scope the host does not have has no defaults to carry: the service layer
// refuses it, so the options of an instance of that scope cannot be built.
func TestBuildInstallOptionsUsesTheDefaultsOfTheScope(t *testing.T) {
	const executable = "/usr/local/bin/hfdesk"

	for _, tc := range []struct {
		goos  string
		scope service.Scope
	}{
		{goos: "linux", scope: service.ScopeUser},
		{goos: "darwin", scope: service.ScopeUser},
		{goos: "windows", scope: service.ScopeSystem},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			scope := scopeOfPlatform(t, tc.goos)
			if scope != tc.scope {
				t.Fatalf("the scope of %s is %q, want %q", tc.goos, scope, tc.scope)
			}

			defaults, err := service.DefaultPaths(scope)
			if err != nil {
				if _, err := buildInstallOptions(scope, installFlags{}, executable); err == nil {
					t.Fatalf("the scope %q does not exist on this host, so the options of an instance of it cannot be built", scope)
				}
				return
			}
			cacheRoot, hubDir := service.DefaultCacheDirs(scope, defaults)

			opts, err := buildInstallOptions(scope, installFlags{}, executable)
			if err != nil {
				t.Fatalf("build the install options: %v", err)
			}

			if opts.Scope != scope {
				t.Errorf("the scope is %q, want %q", opts.Scope, scope)
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
		})
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
//
// The layout is the one of the scope of the platform: the user scope on
// Linux and macOS, the machine-wide scope on Windows. A scope the host does
// not have has no layout to keep: the service layer refuses it, so the
// options of an instance of that scope cannot be built.
func TestBuildInstallOptionsKeepsTheCacheLayoutTogether(t *testing.T) {
	const executable = "/usr/local/bin/hfdesk"

	for _, tc := range []struct {
		goos  string
		scope service.Scope
	}{
		{goos: "linux", scope: service.ScopeUser},
		{goos: "darwin", scope: service.ScopeUser},
		{goos: "windows", scope: service.ScopeSystem},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			scope := scopeOfPlatform(t, tc.goos)
			if scope != tc.scope {
				t.Fatalf("the scope of %s is %q, want %q", tc.goos, scope, tc.scope)
			}

			defaults, err := service.DefaultPaths(scope)
			if err != nil {
				if _, err := buildInstallOptions(scope, installFlags{}, executable); err == nil {
					t.Fatalf("the scope %q does not exist on this host, so the options of an instance of it cannot be built", scope)
				}
				return
			}
			defaultRoot, defaultHub := service.DefaultCacheDirs(scope, defaults)

			for _, c := range []struct {
				name     string
				flags    installFlags
				wantRoot string
				wantHub  string
			}{
				{
					name:     "a cache root alone moves the hub directory with it",
					flags:    installFlags{cacheDir: "/srv/cache"},
					wantRoot: "/srv/cache",
					wantHub:  filepath.Join("/srv/cache", "hub"),
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
				t.Run(c.name, func(t *testing.T) {
					opts, err := buildInstallOptions(scope, c.flags, executable)
					if err != nil {
						t.Fatalf("build the install options: %v", err)
					}
					if opts.CacheDir != c.wantRoot {
						t.Errorf("the cache root is %q, want %q", opts.CacheDir, c.wantRoot)
					}
					if opts.HubDir != c.wantHub {
						t.Errorf("the hub dir is %q, want %q", opts.HubDir, c.wantHub)
					}
				})
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

// TestInstallSummaryNamesTheScopeInTheStartHint checks that the hint of
// the summary starts the instance that was registered.
//
// The default scope of the platform is the user scope on Linux and macOS
// and the machine-wide scope on Windows: a hint without the scope starts
// another instance than the one that was just registered.
func TestInstallSummaryNamesTheScopeInTheStartHint(t *testing.T) {
	for _, tc := range []struct {
		scope    service.Scope
		selector string
	}{
		{scope: service.ScopeUser, selector: "--user"},
		{scope: service.ScopeSystem, selector: "--system"},
	} {
		t.Run(string(tc.scope), func(t *testing.T) {
			opts := service.InstallOptions{
				Scope:      tc.scope,
				Executable: "/usr/local/bin/hfdesk",
				Listen:     "127.0.0.1:8080",
				Paths: runtimepaths.Paths{
					ConfigFile: "/etc/hfdesk/hfdesk.json",
					StateDir:   "/var/lib/hfdesk",
					LogDir:     "/var/log/hfdesk",
				},
			}

			var buf bytes.Buffer
			mgr := fakeManager{status: service.Status{State: service.StateStopped}}
			writeInstallSummary(&buf, context.Background(), mgr, tc.scope, opts)

			out := buf.String()
			hint := startHint(out)
			want := "hfdesk service start " + tc.selector
			if hint != want {
				t.Errorf("the hint of the summary is %q, want %q:\n%s", hint, want, out)
			}
		})
	}
}

// startHint returns the command of the hint that starts the instance.
//
// The hint is the only line of a summary that names the command, so the
// line is found by its prefix.
func startHint(out string) string {
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "Start it with:") {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(trimmed, "Start it with:"))
	}
	return ""
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
			paths, reported, registered := instanceDataPaths(context.Background(), mgr, service.ScopeUser)

			var buf bytes.Buffer
			writeUninstallSummary(&buf, service.ScopeUser, paths, reported, registered)

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
// program would break the instance at the first start. The checks of the
// command line are the ones that hold on every platform: the path is
// absolute, it exists and it is a regular file.
func TestServiceInstallRejectsAnUnusableExecutable(t *testing.T) {
	dir := t.TempDir()

	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"a relative path", "bin/hfdesk", "absolute"},
		{"a missing file", filepath.Join(dir, "missing"), "not usable"},
		{"a directory", dir, "regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runServiceCommand([]string{"install", "--system", "--executable", tc.path}, &stdout, &stderr)
			if code != serviceUsageExitCode {
				t.Fatalf("the installation must be refused with the exit code %d, got %d", serviceUsageExitCode, code)
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

// TestServiceInstallAcceptsAnExistingExecutable checks that a binary that
// exists is accepted by the command layer.
//
// The command layer checks what is meaningful on every platform: the path
// is absolute, it exists and it is a regular file. The execute bit is a
// fact of the platform and belongs to the platform manager: Windows
// reports a regular file without any execute bit, so a check of the
// permission bits would refuse every binary of a Windows installation.
func TestServiceInstallAcceptsAnExistingExecutable(t *testing.T) {
	useFakeManager(t, fakeManager{status: service.Status{State: service.StateStopped}})

	dir := t.TempDir()
	program := filepath.Join(dir, "hfdesk")
	// The file carries no execute bit: the command line does not look at
	// the permission bits.
	if err := os.WriteFile(program, []byte("not a program\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runServiceCommand([]string{"install", "--system", "--executable", program}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("the installation must be accepted, got the exit code %d:\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "The service was registered.") {
		t.Errorf("the summary does not report the registration:\n%s", stdout.String())
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
// explicit path has to be a file the registration can point at.
//
// The path must be absolute, it must exist and it must be a regular file.
// The execute bit is not checked here: it is a fact of the platform, and
// the platform manager owns it.
func TestResolveExecutableRejectsAnUnusableExplicitPath(t *testing.T) {
	root := t.TempDir()
	program := filepath.Join(root, "hfdesk")
	if err := os.WriteFile(program, []byte("#!/bin/sh\n"), 0o755); err != nil {
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

// TestResolveExecutableAcceptsAnExistingFile checks that an existing
// regular file is accepted, whatever its permission bits say.
//
// The execute bit is a fact of the platform: Windows reports a regular
// file without any execute bit, so a check of the permission bits would
// refuse every binary of a Windows installation. The managers of the
// platforms that require the bit enforce it themselves.
func TestResolveExecutableAcceptsAnExistingFile(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(plain, []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveExecutable(plain, "")
	if err != nil {
		t.Fatalf("resolveExecutable(%q) = %v, want nil", plain, err)
	}
	if got != plain {
		t.Errorf("the registered executable is %q, want %q", got, plain)
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

// TestInteractiveFlagsAreUnchanged proves that the service mode did not
// change the interactive command line.
//
// The interactive host registers its flags through the shared application
// layer, so the flag set of this test is built the way the interactive path
// builds it: the names and the defaults are read from the registered flag
// set and not from a source file. A source scan only proves where a
// registration is written down, while the flag set proves what the command
// line really offers.
func TestInteractiveFlagsAreUnchanged(t *testing.T) {
	var opts app.Options
	fs := flag.NewFlagSet("hfdesk", flag.ContinueOnError)
	app.RegisterFlags(fs, &opts)

	// The lookup reports a missing flag instead of dereferencing it, so a
	// flag that is gone does not turn the defaults into a panic. A flag
	// that is gone is reported once, not once per assertion about it.
	reported := map[string]bool{}
	lookup := func(name string) *flag.Flag {
		f := fs.Lookup(name)
		if f == nil && !reported[name] {
			reported[name] = true
			t.Errorf("the interactive command line does not register the %q flag any more", name)
		}
		return f
	}

	// The original flags of the interactive command line. Their names are
	// the contract with the scripts that call the host.
	for _, name := range []string{"port", "cache-dir", "token", "local-dir", "stall-timeout", "open", "no-open"} {
		lookup(name)
	}

	// The defaults are the part of the contract that matters most: a host
	// that is started without an argument has to behave exactly as it
	// behaved before the service mode was added.
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse the interactive flags: %v", err)
	}
	if opts.Port != 8080 {
		t.Errorf("the default port is %d, want 8080", opts.Port)
	}
	if !opts.OpenUI {
		t.Error("the default of the \"open\" flag is false, want true")
	}
	if opts.NoOpen {
		t.Error("the default of the \"no-open\" flag is true, want false")
	}
	for _, c := range []struct {
		name  string
		value string
	}{
		{"cache-dir", opts.CacheDir},
		{"token", opts.Token},
		{"local-dir", opts.LocalDir},
		{"stall-timeout", opts.StallTimeout},
	} {
		if c.value != "" {
			t.Errorf("the default of the %q flag is %q, want an empty value", c.name, c.value)
		}
	}

	// The defaults of the registered flags themselves must be the very
	// same values: the registration and the options are one contract.
	if f := lookup("port"); f != nil && f.DefValue != "8080" {
		t.Errorf("the registered default of the \"port\" flag is %q, want %q", f.DefValue, "8080")
	}
	if f := lookup("open"); f != nil && f.DefValue != "true" {
		t.Errorf("the registered default of the \"open\" flag is %q, want %q", f.DefValue, "true")
	}
	if f := lookup("no-open"); f != nil && f.DefValue != "false" {
		t.Errorf("the registered default of the \"no-open\" flag is %q, want %q", f.DefValue, "false")
	}
	for _, name := range []string{"cache-dir", "token", "local-dir", "stall-timeout"} {
		if f := lookup(name); f != nil && f.DefValue != "" {
			t.Errorf("the registered default of the %q flag is %q, want an empty value", name, f.DefValue)
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

// TestServiceUnknownFlagIsAUsageError checks that an unknown flag of every
// subcommand is a command line that could not be understood.
//
// The exit code 2 tells a script that the invocation was wrong. The exit
// code 1 would tell it that the operation failed, and the script would
// report a broken installation for a typo.
func TestServiceUnknownFlagIsAUsageError(t *testing.T) {
	for _, sub := range serviceSubcommands {
		t.Run(sub, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runServiceCommand([]string{sub, "--bogus"}, &stdout, &stderr)
			if code != serviceUsageExitCode {
				t.Errorf("the %q subcommand must reject an unknown flag with the exit code %d, got %d",
					sub, serviceUsageExitCode, code)
			}
			if !strings.Contains(stderr.String(), "bogus") {
				t.Errorf("the error of the %q subcommand does not name the rejected flag:\n%s", sub, stderr.String())
			}
		})
	}
}

// TestServiceInvalidListenIsAUsageError checks that a bind address the
// command line cannot be parsed with is a command line that could not be
// understood.
//
// An empty host binds every interface, a port out of range names no port
// and a missing port is no address at all.
func TestServiceInvalidListenIsAUsageError(t *testing.T) {
	for _, listen := range []string{
		":8080",
		"127.0.0.1:0",
		"127.0.0.1:70000",
		"127.0.0.1",
		"localhost",
		"",
	} {
		var stdout, stderr bytes.Buffer
		code := runServiceCommand([]string{"install", "--listen", listen}, &stdout, &stderr)
		if code != serviceUsageExitCode {
			t.Errorf("the bind address %q must exit with %d, got %d", listen, serviceUsageExitCode, code)
		}
		if stderr.Len() == 0 {
			t.Errorf("the bind address %q is rejected without a reason", listen)
		}
	}
}

// TestServiceOperationalFailureExitsWithOne checks that an operation that
// was understood but failed exits with 1.
//
// The failure of the supervisor is the case of the family: the service is
// not installed, the elevation is missing or the supervisor refuses. The
// exit code 1 keeps it apart from a command line that could not be
// understood.
func TestServiceOperationalFailureExitsWithOne(t *testing.T) {
	useFakeManager(t, fakeManager{opErr: errors.New("service: hfdesk.service is not installed")})

	var stdout, stderr bytes.Buffer
	code := runServiceCommand([]string{"start"}, &stdout, &stderr)
	if code != serviceFailureExitCode {
		t.Errorf("a failed operation must exit with %d, got %d", serviceFailureExitCode, code)
	}
	if !strings.Contains(stderr.String(), "not installed") {
		t.Errorf("the error does not name the reason of the failure:\n%s", stderr.String())
	}
}

// TestServiceStatusOfAnAbsentRegistrationExitsWithOne checks that the
// status of a service that is not installed is a failed operation.
//
// The state is not one a script can use, so the command reports it with the
// exit code 1 and not with the exit code 0 of a usable instance.
func TestServiceStatusOfAnAbsentRegistrationExitsWithOne(t *testing.T) {
	useFakeManager(t, fakeManager{status: service.Status{State: service.StateNotInstalled}})

	var stdout, stderr bytes.Buffer
	code := runServiceCommand([]string{"status"}, &stdout, &stderr)
	if code != serviceFailureExitCode {
		t.Errorf("the status of an absent registration must exit with %d, got %d", serviceFailureExitCode, code)
	}
}

// TestServiceSuccessfulCommandExitsWithZero checks that a command that did
// what it was asked to do exits with 0.
func TestServiceSuccessfulCommandExitsWithZero(t *testing.T) {
	useFakeManager(t, fakeManager{status: service.Status{State: service.StateRunning}})

	for _, sub := range []string{"start", "status"} {
		var stdout, stderr bytes.Buffer
		code := runServiceCommand([]string{sub}, &stdout, &stderr)
		if code != 0 {
			t.Errorf("the %q subcommand must exit with 0, got %d:\n%s", sub, code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("the %q subcommand must not write to stderr:\n%s", sub, stderr.String())
		}
	}
}

// TestUninstallSummaryStatesWhatTheRemovalDid checks that the summary of
// the uninstall states what actually happened.
//
// A summary that always claims a removal hides the difference between a
// scope that had a registration and a scope that had none.
func TestUninstallSummaryStatesWhatTheRemovalDid(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   service.State
		want    string
		notWant string
	}{
		{
			name:    "a scope with a registration",
			state:   service.StateStopped,
			want:    "The registration of hfdesk (user scope) was removed.",
			notWant: "There was no registration",
		},
		{
			name:    "a scope without a registration",
			state:   service.StateNotInstalled,
			want:    "There was no registration of hfdesk (user scope) to remove.",
			notWant: "was removed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			writeUninstallSummary(&buf, service.ScopeUser, runtimepaths.Paths{}, cacheRootNotRecorded,
				tc.state != service.StateNotInstalled)

			out := buf.String()
			if !strings.Contains(out, tc.want) {
				t.Errorf("the uninstall summary does not state %q:\n%s", tc.want, out)
			}
			if strings.Contains(out, tc.notWant) {
				t.Errorf("the uninstall summary states %q as well:\n%s", tc.notWant, out)
			}
			if !strings.Contains(out, "The user data was kept") {
				t.Errorf("the uninstall summary does not report the kept user data:\n%s", out)
			}
		})
	}
}

// TestServiceUninstallReportsWhatTheRemovalDid drives the uninstall
// command through the manager of the test and checks that the summary
// states what the removal did.
func TestServiceUninstallReportsWhatTheRemovalDid(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   service.State
		want    string
		notWant string
	}{
		{
			name:    "a registered service",
			state:   service.StateStopped,
			want:    fmt.Sprintf("The registration of %s (%s scope) was removed.", service.ServiceName, defaultScope()),
			notWant: "There was no registration",
		},
		{
			name:    "an unregistered service",
			state:   service.StateNotInstalled,
			want:    fmt.Sprintf("There was no registration of %s (%s scope) to remove.", service.ServiceName, defaultScope()),
			notWant: "was removed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useFakeManager(t, fakeManager{status: service.Status{State: tc.state}})

			var stdout, stderr bytes.Buffer
			code := runServiceCommand([]string{"uninstall"}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("the uninstall must exit with 0, got %d:\n%s", code, stderr.String())
			}
			out := stdout.String()
			if !strings.Contains(out, tc.want) {
				t.Errorf("the uninstall summary does not state %q:\n%s", tc.want, out)
			}
			if strings.Contains(out, tc.notWant) {
				t.Errorf("the uninstall summary states %q as well:\n%s", tc.notWant, out)
			}
			if !strings.Contains(out, "The user data was kept") {
				t.Errorf("the uninstall summary does not report the kept user data:\n%s", out)
			}
		})
	}
}

// TestServicePlatformFactsOfTheHost checks that the facts the service layer
// exposes are the facts of the platform the test runs on.
//
// The command line decides with these facts, so a fact that does not match
// the platform would make the command line decide about another platform.
func TestServicePlatformFactsOfTheHost(t *testing.T) {
	want, ok := platformFactsByName[runtime.GOOS]
	if !ok {
		t.Skipf("the platform %q is not one of the platforms of the service layer", runtime.GOOS)
	}

	if got := service.DefaultScope(); got != want.defaultScope {
		t.Errorf("the default scope of %s is %q, want %q", runtime.GOOS, got, want.defaultScope)
	}
	if got := service.SupportsUserScope(); got != want.supportsUserScope {
		t.Errorf("%s has a per-user scope: %v, want %v", runtime.GOOS, got, want.supportsUserScope)
	}
	if got := service.SupportsNamedServiceAccount(); got != want.supportsNamedServiceAccount {
		t.Errorf("%s has a named service account: %v, want %v", runtime.GOOS, got, want.supportsNamedServiceAccount)
	}
}

// TestServicePerPlatformFacts checks the facts every platform of the
// service layer defines.
//
// The default scope of an installation is the user scope on Linux and
// macOS and the system scope on Windows, and the account of a machine-wide
// instance can be named on Linux and macOS but not on Windows.
//
// The facts of a platform live in the file of that platform, so the guard
// reads them from the source: a test that runs on one platform cannot call
// the code of another one.
func TestServicePerPlatformFacts(t *testing.T) {
	for _, tc := range []struct {
		file      string
		scope     service.Scope
		userScope bool
		namedUser bool
	}{
		{file: "serviceuser_linux.go", scope: service.ScopeUser, userScope: true, namedUser: true},
		{file: "serviceuser_darwin.go", scope: service.ScopeUser, userScope: true, namedUser: true},
		{file: "serviceuser_windows.go", scope: service.ScopeSystem, userScope: false, namedUser: false},
		{file: "serviceuser_other.go", scope: service.ScopeUser, userScope: true, namedUser: true},
	} {
		t.Run(tc.file, func(t *testing.T) {
			if got := serviceScopeOf(platformFactOf(t, tc.file, "defaultScope")); got != tc.scope {
				t.Errorf("the default scope of %s is %q, want %q", tc.file, got, tc.scope)
			}
			if got := platformFactOf(t, tc.file, "supportsUserScope"); got != boolName(tc.userScope) {
				t.Errorf("the per-user scope of %s is %q, want %q", tc.file, got, boolName(tc.userScope))
			}
			if got := platformFactOf(t, tc.file, "supportsNamedServiceAccount"); got != boolName(tc.namedUser) {
				t.Errorf("the named service account of %s is %q, want %q", tc.file, got, boolName(tc.namedUser))
			}
		})
	}
}

// TestServiceDefaultScopeFollowsThePlatformFacts checks that the default
// scope of an installation is the scope of the platform.
//
// The user scope is the default on Linux and macOS, the system scope on
// Windows: the service control manager of Windows registers machine-wide
// services only.
func TestServiceDefaultScopeFollowsThePlatformFacts(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			usePlatform(t, goos)

			scope, err := resolveScope(false, false)
			if err != nil {
				t.Fatalf("the default scope must resolve: %v", err)
			}
			if want := platformFactsByName[goos].defaultScope; scope != want {
				t.Errorf("the default scope on %s is %q, want %q", goos, scope, want)
			}
		})
	}
}

// TestServiceUserScopeRefusalFollowsThePlatformFacts checks that the
// per-user scope is refused where the platform does not have it.
//
// The service control manager of Windows registers machine-wide services
// only, so a per-user installation does not exist there.
func TestServiceUserScopeRefusalFollowsThePlatformFacts(t *testing.T) {
	for _, tc := range []struct {
		goos      string
		wantError bool
	}{
		{goos: "linux"},
		{goos: "darwin"},
		{goos: "windows", wantError: true},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			usePlatform(t, tc.goos)

			scope, err := resolveScope(true, false)
			if !tc.wantError {
				if err != nil {
					t.Fatalf("the per-user scope must resolve on %s: %v", tc.goos, err)
				}
				if scope != service.ScopeUser {
					t.Errorf("the scope on %s is %q, want %q", tc.goos, scope, service.ScopeUser)
				}
				return
			}
			if err == nil {
				t.Fatal("the per-user scope must be refused where the platform does not have it")
			}
			if !isServiceUsageError(err) {
				t.Errorf("the refusal must be a command line that could not be understood: %v", err)
			}
		})
	}
}

// TestServiceUserRefusalKeepsItsMessage checks that the refusal of a named
// account on Windows keeps its message.
//
// The message names the account the instance really runs as and explains
// why the account cannot be chosen: a user of the platform has to know what
// to do instead of the option.
func TestServiceUserRefusalKeepsItsMessage(t *testing.T) {
	usePlatform(t, "windows")

	_, err := buildInstallOptions(service.ScopeSystem, installFlags{serviceUser: "hfdesk-svc"}, "/usr/local/bin/hfdesk")
	if err == nil {
		t.Fatal("the machine-wide scope must reject a service account on windows")
	}
	want := "--service-user is not available on windows: the machine-wide instance runs as the built-in " +
		windowsServiceAccount + " account and the account cannot be chosen on this platform " +
		"(a service under a user account needs a credential, and a credential must never reach a command line), drop the option"
	if err.Error() != want {
		t.Errorf("the refusal is %q, want %q", err.Error(), want)
	}
}

// TestServiceCommandHasNoOperatingSystemComparison checks that the command
// family does not compare the operating system.
//
// The scope and the account of an instance are facts of the platform, and
// the service layer owns them: a comparison in the command line would put
// the decision about the platform back into a shared layer.
//
// The guard names the comparison and not the platform: the messages of the
// command line name the platform they talk about.
func TestServiceCommandHasNoOperatingSystemComparison(t *testing.T) {
	source, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}

	text := string(source)
	for _, forbidden := range []string{"runtime.GOOS", "platformGOOS", "isWindows"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the command family compares the operating system (%q):\n%s", forbidden, sourceLinesWith(text, forbidden))
		}
	}
}

// platformFactOf returns the value a per-platform function of the service
// layer returns.
//
// The facts of a platform live in the file of that platform, and the code
// of another platform cannot be called from a test that runs on this one:
// the guard reads the facts from the source of the platform file.
func platformFactOf(t *testing.T, file, name string) string {
	t.Helper()

	path := filepath.Join("..", "..", "internal", "service", file)
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		for _, stmt := range fn.Body.List {
			ret, ok := stmt.(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				continue
			}
			if value, ok := ret.Results[0].(*ast.Ident); ok {
				return value.Name
			}
		}
	}

	t.Fatalf("%s does not return a value from %s", path, name)
	return ""
}

// serviceScopeOf turns the name of a scope of the service layer into the
// scope it names.
func serviceScopeOf(name string) service.Scope {
	switch name {
	case "ScopeUser":
		return service.ScopeUser
	case "ScopeSystem":
		return service.ScopeSystem
	}
	return service.Scope(name)
}

// boolName returns the name a boolean literal has in the source of the
// service layer.
func boolName(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// sourceLinesWith returns the lines of a source that hold the given text.
func sourceLinesWith(source, text string) string {
	var lines []string
	for _, line := range strings.Split(source, "\n") {
		if strings.Contains(line, text) {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return strings.Join(lines, "\n")
}
