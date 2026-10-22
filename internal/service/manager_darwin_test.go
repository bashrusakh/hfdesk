// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package service

import (
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
)

// darwinCredentialSentinel is the value the credential looking fields of an
// installation carry in the tests. It must never reach a registration.
const darwinCredentialSentinel = "hf_sentinel_TOKEN_9f2b7c"

// darwinUserOptions returns the options of a user scope installation. The
// paths are the real ones of the platform, so the test checks the paths the
// installation actually uses.
func darwinUserOptions(t *testing.T) InstallOptions {
	t.Helper()
	paths, err := DefaultPaths(ScopeUser)
	if err != nil {
		t.Fatalf("DefaultPaths(ScopeUser): %v", err)
	}
	cache, hub := DefaultCacheDirs(ScopeUser, paths)
	return InstallOptions{
		Scope:      ScopeUser,
		Executable: "/usr/local/bin/hfdesk",
		Listen:     DefaultListen,
		Paths:      paths,
		CacheDir:   cache,
		HubDir:     hub,
		LocalDir:   filepath.Join(paths.StateDir, "models"),
	}
}

// darwinSystemOptions returns the options of a machine-wide installation.
func darwinSystemOptions(t *testing.T) InstallOptions {
	t.Helper()
	paths, err := DefaultPaths(ScopeSystem)
	if err != nil {
		t.Fatalf("DefaultPaths(ScopeSystem): %v", err)
	}
	cache, hub := DefaultCacheDirs(ScopeSystem, paths)
	return InstallOptions{
		Scope:      ScopeSystem,
		Executable: "/usr/local/bin/hfdesk",
		Listen:     DefaultListen,
		Paths:      paths,
		CacheDir:   cache,
		HubDir:     hub,
		LocalDir:   filepath.Join(paths.StateDir, "models"),
	}
}

// TestRenderPlistForTheUserScope checks the property list of a LaunchAgent.
func TestRenderPlistForTheUserScope(t *testing.T) {
	opts := darwinUserOptions(t)
	rendered, err := renderPlist(opts, nil)
	if err != nil {
		t.Fatalf("renderPlist(): %v", err)
	}

	registration, err := parsePlist([]byte(rendered))
	if err != nil {
		t.Fatalf("parsePlist(): %v", err)
	}

	// The label is derived from the service name, and it is also the name
	// of the property list file: launchd refuses to load a property list
	// whose file name does not match its label.
	if registration.Label != "com.hfdesk.hfdesk" {
		t.Errorf("Label = %q, want %q", registration.Label, "com.hfdesk.hfdesk")
	}
	if registration.Label != launchdLabel() {
		t.Errorf("Label = %q, want the label of the service name %q", registration.Label, launchdLabel())
	}

	// The argument vector is the executable and the canonical arguments of
	// the service host, nothing else.
	want := append([]string{opts.Executable}, RunArgs(opts)...)
	if !reflect.DeepEqual(registration.ProgramArguments, want) {
		t.Errorf("ProgramArguments =\n%v\nwant\n%v", registration.ProgramArguments, want)
	}
	if len(registration.ProgramArguments) == 0 || registration.ProgramArguments[0] != opts.Executable {
		t.Errorf("ProgramArguments[0] = %q, want the absolute executable %q", registration.ProgramArguments[0], opts.Executable)
	}

	if registration.WorkingDirectory != opts.Paths.StateDir {
		t.Errorf("WorkingDirectory = %q, want the state dir %q", registration.WorkingDirectory, opts.Paths.StateDir)
	}
	if !registration.RunAtLoad {
		t.Error("RunAtLoad = false, want true")
	}
	// The job is restarted after a failure only, like the systemd unit of
	// the Linux installation.
	if registration.KeepAliveSuccessfulExit {
		t.Error("KeepAlive.SuccessfulExit = true, want false")
	}
	if registration.UserName != "" || registration.GroupName != "" {
		t.Errorf("UserName = %q, GroupName = %q; want none for a user scope instance", registration.UserName, registration.GroupName)
	}

	// The environment of the process carries the home directory of the user
	// and nothing else.
	if len(registration.Environment) != 1 {
		t.Errorf("EnvironmentVariables = %v, want the home directory only", registration.Environment)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir(): %v", err)
	}
	if registration.Environment["HOME"] != home {
		t.Errorf("EnvironmentVariables.HOME = %q, want %q", registration.Environment["HOME"], home)
	}
}

// TestRenderPlistForTheSystemScope checks the property list of a
// LaunchDaemon.
func TestRenderPlistForTheSystemScope(t *testing.T) {
	opts := darwinSystemOptions(t)
	account := darwinCurrentAccount(t)
	opts.ServiceUser = account.Name

	rendered, err := renderPlist(opts, account)
	if err != nil {
		t.Fatalf("renderPlist(): %v", err)
	}

	registration, err := parsePlist([]byte(rendered))
	if err != nil {
		t.Fatalf("parsePlist(): %v", err)
	}

	// The daemon runs as the service account of the machine, not as root:
	// the instance owns its directories and needs no privilege of root.
	if registration.UserName != account.Name {
		t.Errorf("UserName = %q, want the service account %q", registration.UserName, account.Name)
	}
	if registration.UserName == "root" {
		t.Error("the machine-wide instance runs as root, want the service account")
	}
	if registration.GroupName != account.Group {
		t.Errorf("GroupName = %q, want the primary group %q of the service account", registration.GroupName, account.Group)
	}

	// The machine-wide instance never reuses the home directory of the user
	// who runs the install command.
	if registration.Environment["HOME"] != "/var/lib/hfdesk" {
		t.Errorf("EnvironmentVariables.HOME = %q, want %q", registration.Environment["HOME"], "/var/lib/hfdesk")
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if registration.Environment["HOME"] == home {
			t.Errorf("EnvironmentVariables.HOME = %q, want the machine-level home and not the home of the user", home)
		}
		if strings.Contains(rendered, home) {
			t.Errorf("the property list of the machine-wide instance mentions the home directory %q of the user", home)
		}
	}

	want := append([]string{opts.Executable}, RunArgs(opts)...)
	if !reflect.DeepEqual(registration.ProgramArguments, want) {
		t.Errorf("ProgramArguments =\n%v\nwant\n%v", registration.ProgramArguments, want)
	}
}

// TestRenderPlistCarriesNoCredential checks that no credential reaches a
// registration.
//
// A property list is readable by every user of the machine, so the
// registration carries the paths and the bind address of the instance and
// nothing else. The credentials of an instance come from its own config
// file.
func TestRenderPlistCarriesNoCredential(t *testing.T) {
	current := darwinCurrentAccount(t)

	for _, scope := range []Scope{ScopeUser, ScopeSystem} {
		t.Run(string(scope), func(t *testing.T) {
			var opts InstallOptions
			var account *serviceAccount
			if scope == ScopeUser {
				opts = darwinUserOptions(t)
			} else {
				opts = darwinSystemOptions(t)
				opts.ServiceUser = current.Name
				account = current
			}
			// Every string field of the options that could carry a
			// credential carries the sentinel.
			setCredentialFields(&opts, darwinCredentialSentinel)

			rendered, err := renderPlist(opts, account)
			if err != nil {
				t.Fatalf("renderPlist(): %v", err)
			}
			if strings.Contains(rendered, darwinCredentialSentinel) {
				t.Errorf("the property list carries the credential sentinel:\n%s", rendered)
			}

			// The registration holds the known keys only: there is no
			// place a credential could hide in.
			want := []string{"Label", "ProgramArguments", "WorkingDirectory", "RunAtLoad", "KeepAlive", "ProcessType", "ExitTimeOut", "EnvironmentVariables"}
			if scope == ScopeSystem {
				want = append(want, "UserName", "GroupName")
			}
			registration, err := parsePlist([]byte(rendered))
			if err != nil {
				t.Fatalf("parsePlist(): %v", err)
			}
			if !reflect.DeepEqual(registration.Keys, want) {
				t.Errorf("the keys of the property list =\n%v\nwant\n%v", registration.Keys, want)
			}

			// The environment of the process holds the home directory and
			// nothing else, so a credential has no place in it either.
			if len(registration.Environment) != 1 {
				t.Errorf("the environment of the registration = %v, want the home directory only", registration.Environment)
			}
			if _, ok := registration.Environment["HOME"]; !ok {
				t.Errorf("the environment of the registration = %v, want the home directory", registration.Environment)
			}

			// Every string of the property list is one of the values the
			// installation is made of.
			allowed := map[string]bool{
				launchdLabel():      true,
				opts.Executable:     true,
				opts.Paths.StateDir: true,
				"Background":        true,
			}
			if scope == ScopeSystem {
				allowed[current.Name] = true
				allowed[current.Group] = true
			}
			for _, arg := range RunArgs(opts) {
				allowed[arg] = true
			}
			for _, value := range registration.Environment {
				allowed[value] = true
			}
			for _, value := range darwinPlistStrings(registration) {
				if !allowed[value] {
					t.Errorf("the property list holds the unexpected value %q", value)
				}
			}
		})
	}
}

// TestRenderPlistKeepsTheLogFilesOfTheSupervisorOut checks that the
// property list does not point launchd at a log file.
//
// launchd does not rotate the files of StandardOutPath and
// StandardErrorPath, so they would grow without bound. HFDesk writes its
// own bounded rotating log file into the log directory of the instance.
func TestRenderPlistKeepsTheLogFilesOfTheSupervisorOut(t *testing.T) {
	opts := darwinUserOptions(t)
	rendered, err := renderPlist(opts, nil)
	if err != nil {
		t.Fatalf("renderPlist(): %v", err)
	}

	for _, forbidden := range []string{"StandardOutPath", "StandardErrorPath", "nohup"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("the property list mentions %q:\n%s", forbidden, rendered)
		}
	}
	// The program is never run through a shell, so the property list must
	// not carry a shell operator. The XML entities are removed first: an
	// escaped ampersand of a path is not an operator.
	if strings.Contains(darwinStripXMLEntities(rendered), "&") {
		t.Errorf("the property list carries an ampersand:\n%s", rendered)
	}
}

// TestParsePlistRecoversTheRegistration checks that the registered program
// and the bind address are recovered from a property list.
func TestParsePlistRecoversTheRegistration(t *testing.T) {
	opts := darwinUserOptions(t)
	rendered, err := renderPlist(opts, nil)
	if err != nil {
		t.Fatalf("renderPlist(): %v", err)
	}
	registration, err := parsePlist([]byte(rendered))
	if err != nil {
		t.Fatalf("parsePlist(): %v", err)
	}

	if registration.executable() != opts.Executable {
		t.Errorf("the executable = %q, want %q", registration.executable(), opts.Executable)
	}
	if registration.listen() != opts.Listen {
		t.Errorf("the listen address = %q, want %q", registration.listen(), opts.Listen)
	}
	if registration.listen() != DefaultListen {
		t.Errorf("the listen address = %q, want the default %q", registration.listen(), DefaultListen)
	}

	paths := registration.paths()
	want := runtimepaths.Paths{
		ConfigFile: opts.Paths.ConfigFile,
		StateDir:   opts.Paths.StateDir,
		LogDir:     opts.Paths.LogDir,
	}
	if paths != want {
		t.Errorf("the paths of the registration =\n%+v\nwant\n%+v", paths, want)
	}
}

// TestParsePlistRecoversTheConfigFileAndTheCacheDir checks that the config
// file and the cache directory of the registration are recovered from a
// property list.
//
// Both values come from the argument vector of the registered process: the
// status describes the registration, so it needs the config file of the
// instance and the cache directory the registration recorded.
func TestParsePlistRecoversTheConfigFileAndTheCacheDir(t *testing.T) {
	cases := []struct {
		name      string
		cacheDir  string
		wantCache string
	}{
		{
			name:      "a registration with a cache directory",
			cacheDir:  "/registered/cache",
			wantCache: "/registered/cache",
		},
		{
			name:      "a registration without a cache directory",
			cacheDir:  "",
			wantCache: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := darwinUserOptions(t)
			opts.CacheDir = tc.cacheDir

			rendered, err := renderPlist(opts, nil)
			if err != nil {
				t.Fatalf("renderPlist(): %v", err)
			}
			registration, err := parsePlist([]byte(rendered))
			if err != nil {
				t.Fatalf("parsePlist(): %v", err)
			}

			if got := registration.paths().ConfigFile; got != opts.Paths.ConfigFile {
				t.Errorf("the config file = %q, want %q", got, opts.Paths.ConfigFile)
			}
			if got := registration.cacheDir(); got != tc.wantCache {
				t.Errorf("the cache dir = %q, want %q", got, tc.wantCache)
			}
		})
	}
}

// TestParsePlistRejectsBrokenContent checks that a broken property list is
// reported instead of read as an empty registration.
func TestParsePlistRejectsBrokenContent(t *testing.T) {
	if _, err := parsePlist([]byte("<plist><dict><key>Label</key>")); err == nil {
		t.Error("parsePlist() = nil, want an error for a broken property list")
	}
}

// TestParseLaunchdPrint checks the mapping of the output of "launchctl
// print" to the state of the instance.
func TestParseLaunchdPrint(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   State
	}{
		{
			name:   "a running job",
			output: "com.hfdesk.hfdesk = {\n\tstate = running\n\tpid = 4711\n}",
			want:   StateRunning,
		},
		{
			name:   "a running job with a quoted state",
			output: "com.hfdesk.hfdesk = {\n\t\"state\" = \"running\";\n\t\"pid\" = 4711;\n}",
			want:   StateRunning,
		},
		{
			name:   "a running job with a state in a nested block",
			output: "com.hfdesk.hfdesk = {\n\tactive count = 1\n\tjob = {\n\t\tstate = running\n\t\tpid = 4711\n\t}\n}",
			want:   StateRunning,
		},
		{
			name:   "a running job with a state that ends with a semicolon",
			output: "com.hfdesk.hfdesk = {\n\tstate = running;\n}",
			want:   StateRunning,
		},
		{
			name:   "a running job with a capitalized state",
			output: "com.hfdesk.hfdesk = {\n\tState = running\n}",
			want:   StateRunning,
		},
		{
			name:   "the state of the job wins over a nested state",
			output: "com.hfdesk.hfdesk = {\n\tstate = running\n\tjob = {\n\t\tstate = not running\n\t}\n}",
			want:   StateRunning,
		},
		{
			name:   "a job that is not running",
			output: "com.hfdesk.hfdesk = {\n\tstate = not running\n\tlast exit code = 0\n}",
			want:   StateStopped,
		},
		{
			name:   "a job that exited",
			output: "com.hfdesk.hfdesk = {\n\tstate = exited\n\tlast exit code = 0\n}",
			want:   StateStopped,
		},
		{
			name:   "a job that failed",
			output: "com.hfdesk.hfdesk = {\n\tstate = exited\n\tlast exit code = 1\n}",
			want:   StateFailed,
		},
		{
			name:   "a job that failed without a running state",
			output: "com.hfdesk.hfdesk = {\n\tstate = not running\n\tlast exit code = 78\n}",
			want:   StateFailed,
		},
		{
			name:   "a job that waits",
			output: "com.hfdesk.hfdesk = {\n\tstate = waiting\n}",
			want:   StateStarting,
		},
		{
			name:   "an unknown state",
			output: "com.hfdesk.hfdesk = {\n\tstate = something else\n}",
			want:   StateUnknown,
		},
		{
			name:   "no state at all",
			output: "com.hfdesk.hfdesk = {\n\tpid = 4711\n}",
			want:   StateUnknown,
		},
		{
			name:   "empty output",
			output: "",
			want:   StateUnknown,
		},
		{
			name: "the default state is not the state",
			output: "com.hfdesk.hfdesk = {\n\tstate = running\n\tdefault state = stopped\n" +
				"\tlast exit code = 1\n}",
			want: StateRunning,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseLaunchdPrint(test.output); got != test.want {
				t.Errorf("parseLaunchdPrint() = %q, want %q", got, test.want)
			}
		})
	}
}

// TestLaunchdPrintFieldsReadsTheVariantsOfTheOutput checks the fields that
// are read from the output of "launchctl print".
//
// The output of launchd is not an API: the fields are read defensively, so
// the fields of every known spelling are found.
func TestLaunchdPrintFieldsReadsTheVariantsOfTheOutput(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		state    string
		exitCode int
		pid      int
	}{
		{
			name:   "the fields of a running job",
			output: "com.hfdesk.hfdesk = {\n\tstate = running\n\tpid = 4711\n}",
			state:  "running",
			pid:    4711,
		},
		{
			name:   "the fields of a quoted job",
			output: "com.hfdesk.hfdesk = {\n\t\"state\" = \"not running\";\n\t\"pid\" = 4711;\n}",
			state:  "not running",
			pid:    4711,
		},
		{
			name:     "the fields of a job in a nested block",
			output:   "com.hfdesk.hfdesk = {\n\tjob = {\n\t\tstate = exited\n\t\tlast exit code = 78\n\t}\n}",
			state:    "exited",
			exitCode: 78,
		},
		{
			name:     "the exit code of a property list",
			output:   "{\n\"Label\" = \"com.hfdesk.hfdesk\";\n\"LastExitStatus\" = 3;\n};",
			exitCode: 3,
		},
		{
			name:     "the exit status of the output",
			output:   "com.hfdesk.hfdesk = {\n\tstate = exited\n\tlast exit status = 4\n}",
			state:    "exited",
			exitCode: 4,
		},
		{
			name:   "the default state is not the state",
			output: "com.hfdesk.hfdesk = {\n\tstate = running\n\tdefault state = stopped\n}",
			state:  "running",
		},
		{
			name:   "no field at all",
			output: "com.hfdesk.hfdesk = {\n\tactive count = 0\n}",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, exitCode, pid := launchdPrintFields(test.output)
			if state != test.state {
				t.Errorf("the state = %q, want %q", state, test.state)
			}
			if exitCode != test.exitCode {
				t.Errorf("the exit code = %d, want %d", exitCode, test.exitCode)
			}
			if pid != test.pid {
				t.Errorf("the pid = %d, want %d", pid, test.pid)
			}
		})
	}
}

// TestLaunchdUnknownStateDetailCarriesTheOutputOfTheSupervisor checks the
// detail of a status whose state could not be read.
//
// The detail has to name what the supervisor reported, so an administrator
// and a failing test see the same thing. It must stay short, and it must not
// carry a credential.
func TestLaunchdUnknownStateDetailCarriesTheOutputOfTheSupervisor(t *testing.T) {
	output := "com.hfdesk.hfdesk = {\n\tactive count = 0\n\tpath = /Users/somebody/Library/LaunchAgents/com.hfdesk.hfdesk.plist\n" +
		"\tenvironment = {\n\t\tHOME => /Users/somebody\n\t\tHF_TOKEN => hf_secret_value\n\t}\n}"

	detail := launchdUnknownStateDetail("gui/501/com.hfdesk.hfdesk", output)
	if !strings.Contains(detail, "active count = 0") {
		t.Errorf("the detail = %q, want the output of the supervisor", detail)
	}
	if !strings.Contains(detail, "launchctl print gui/501/com.hfdesk.hfdesk") {
		t.Errorf("the detail = %q, want the target of the supervisor", detail)
	}
	if strings.Contains(detail, "hf_secret_value") {
		t.Errorf("the detail = %q, want no credential", detail)
	}
	if !strings.Contains(detail, "redacted") {
		t.Errorf("the detail = %q, want the removed value to be named", detail)
	}
	if len(detail) > launchdStateDetailLimit+200 {
		t.Errorf("the detail is %d characters long, want at most %d", len(detail), launchdStateDetailLimit+200)
	}

	// A long output is cut, and the cut is visible.
	long := strings.Repeat("a very long line of the output of the supervisor. ", 100)
	cut := launchdPrintExcerpt(long, launchdStateDetailLimit)
	if len([]rune(cut)) > launchdStateDetailLimit+3 {
		t.Errorf("the excerpt is %d characters long, want at most %d", len([]rune(cut)), launchdStateDetailLimit+3)
	}
	if !strings.HasSuffix(cut, "...") {
		t.Errorf("the excerpt = %q, want the cut to be visible", cut)
	}
}

// TestLaunchdProcessAlive checks the check of a live process.
func TestLaunchdProcessAlive(t *testing.T) {
	if launchdProcessAlive(0) {
		t.Error("launchdProcessAlive(0) = true, want false for a process that does not exist")
	}
	if launchdProcessAlive(-1) {
		t.Error("launchdProcessAlive(-1) = true, want false for a process that does not exist")
	}
	if !launchdProcessAlive(os.Getpid()) {
		t.Error("launchdProcessAlive() = false, want true for the process of the test")
	}
}

// TestLaunchdStatusStateMapsTheOutputOfTheSupervisor checks the state and
// the detail a status carries for the output of "launchctl print".
//
// A broken instance must not look healthy: an instance that exited with a
// non-zero status is failed, and an instance that is not loaded is stopped.
func TestLaunchdStatusStateMapsTheOutputOfTheSupervisor(t *testing.T) {
	live := os.Getpid()
	tests := []struct {
		name       string
		output     string
		want       State
		wantDetail string
	}{
		{
			name:   "a running job",
			output: "com.hfdesk.hfdesk = {\n\tstate = running\n\tpid = 4711\n}",
			want:   StateRunning,
		},
		{
			name:       "a job that exited with a non-zero status",
			output:     "com.hfdesk.hfdesk = {\n\tstate = exited\n\tlast exit code = 1\n}",
			want:       StateFailed,
			wantDetail: "the instance exited with the status 1",
		},
		{
			name:   "a job that is not running",
			output: "com.hfdesk.hfdesk = {\n\tstate = not running\n\tlast exit code = 0\n}",
			want:   StateStopped,
		},
		{
			name:       "a job with a live process and an unknown state",
			output:     "com.hfdesk.hfdesk = {\n\tpid = " + strconv.Itoa(live) + "\n}",
			want:       StateRunning,
			wantDetail: "launchctl print gui/501/com.hfdesk.hfdesk reported no state, but the process " + strconv.Itoa(live) + " of the instance is alive",
		},
		{
			name:       "a job without a state and with a non-zero exit status",
			output:     "com.hfdesk.hfdesk = {\n\tactive count = 0\n\tlast exit code = 3\n}",
			want:       StateFailed,
			wantDetail: "the instance exited with the status 3",
		},
		{
			name:   "a job without a state and without a process",
			output: "com.hfdesk.hfdesk = {\n\tactive count = 0\n}",
			want:   StateUnknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, detail := launchdStatusState("gui/501/com.hfdesk.hfdesk", test.output)
			if state != test.want {
				t.Errorf("the state = %q, want %q", state, test.want)
			}
			if test.wantDetail != "" && detail != test.wantDetail {
				t.Errorf("the detail = %q, want %q", detail, test.wantDetail)
			}
			if test.want == StateUnknown && !strings.Contains(detail, "launchctl print gui/501/com.hfdesk.hfdesk reported") {
				t.Errorf("the detail = %q, want the output of the supervisor", detail)
			}
		})
	}
}

// TestLaunchdJobRunningDetectsARunningJob checks the detection of a job that
// runs.
//
// The detection is the same for the status of an instance and for the test
// of the lifecycle: the state of the job is "running", or the job has a live
// process.
func TestLaunchdJobRunningDetectsARunningJob(t *testing.T) {
	live := os.Getpid()
	tests := []struct {
		name  string
		state string
		pid   int
		want  bool
	}{
		{name: "the state says the job runs", state: "running", want: true},
		{name: "the state says the job does not run", state: "not running", want: false},
		{name: "an unknown state and a live process", pid: live, want: true},
		{name: "an unknown state and no process", want: false},
		{name: "a state that does not run and a live process", state: "exited", pid: live, want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := launchdJobRunning(test.state, test.pid); got != test.want {
				t.Errorf("launchdJobRunning(%q, %d) = %v, want %v", test.state, test.pid, got, test.want)
			}
		})
	}
}

// TestPlutilAcceptsTheRenderedPlist checks the property list with the
// property list tool of macOS.
func TestPlutilAcceptsTheRenderedPlist(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skipf("plutil is not available: %v", err)
	}
	current := darwinCurrentAccount(t)

	for _, scope := range []Scope{ScopeUser, ScopeSystem} {
		t.Run(string(scope), func(t *testing.T) {
			var opts InstallOptions
			var account *serviceAccount
			if scope == ScopeUser {
				opts = darwinUserOptions(t)
			} else {
				opts = darwinSystemOptions(t)
				opts.ServiceUser = current.Name
				account = current
			}
			rendered, err := renderPlist(opts, account)
			if err != nil {
				t.Fatalf("renderPlist(): %v", err)
			}
			path := filepath.Join(t.TempDir(), launchdLabel()+".plist")
			if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
				t.Fatalf("write the property list: %v", err)
			}
			output, err := exec.Command(plutil, "-lint", path).CombinedOutput()
			if err != nil {
				t.Fatalf("plutil -lint: %v\n%s", err, output)
			}
		})
	}
}

// TestWritePlistIsAtomic checks that the property list is written with the
// expected mode and that a failed installation can restore it.
func TestWritePlistIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, launchdLabel()+".plist")

	if err := writePlist(path, []byte("<plist/>"), plistMode); err != nil {
		t.Fatalf("writePlist(): %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the property list: %v", err)
	}
	if info.Mode().Perm() != plistMode {
		t.Errorf("the mode of the property list = %v, want %v", info.Mode().Perm(), plistMode)
	}
	// The temporary file of the atomic write is gone.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("the directory holds %d entries, want the property list only", len(entries))
	}

	// A failed installation restores the previous property list.
	backup, err := backupPlist(path)
	if err != nil {
		t.Fatalf("backupPlist(): %v", err)
	}
	if err := writePlist(path, []byte("<plist>other</plist>"), plistMode); err != nil {
		t.Fatalf("writePlist(): %v", err)
	}
	if err := backup.restore(); err != nil {
		t.Fatalf("restore(): %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the property list: %v", err)
	}
	if string(content) != "<plist/>" {
		t.Errorf("the restored property list = %q, want %q", content, "<plist/>")
	}

	// A property list that did not exist is removed again.
	missing := &plistBackup{path: filepath.Join(dir, "missing.plist")}
	if err := missing.restore(); err != nil {
		t.Fatalf("restore(): %v", err)
	}
}

// TestStatusReportsAMissingExecutable checks that a registration that points
// at a program that is gone is reported as broken.
func TestStatusReportsAMissingExecutable(t *testing.T) {
	if _, err := exec.LookPath("launchctl"); err != nil {
		t.Skipf("launchctl is not available: %v", err)
	}

	// The manager reads the registration of the instance, so the property
	// list of the instance is written into a temporary home directory.
	home := t.TempDir()
	launchd := &launchdManager{scope: ScopeUser, uid: strconv.Itoa(os.Getuid()), home: home}

	stateDir := t.TempDir()
	opts := InstallOptions{
		Scope:      ScopeUser,
		Executable: filepath.Join(stateDir, "hfdesk"),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(stateDir, "hfdesk.json"),
			StateDir:   stateDir,
			LogDir:     filepath.Join(stateDir, "logs"),
		},
	}
	rendered, err := renderPlist(opts, nil)
	if err != nil {
		t.Fatalf("renderPlist(): %v", err)
	}
	path := launchd.plistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the registration directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
		t.Fatalf("write the property list: %v", err)
	}
	if _, err := os.Stat(opts.Executable); err == nil {
		t.Fatalf("the executable %q exists, want a program that is gone", opts.Executable)
	}

	status, err := launchd.Status(context.Background())
	if err != nil {
		t.Fatalf("Status(): %v", err)
	}
	if status.State != StateBroken {
		t.Errorf("the state = %q, want %q", status.State, StateBroken)
	}
	if !strings.Contains(status.Detail, opts.Executable) {
		t.Errorf("the detail = %q, want the path of the missing program", status.Detail)
	}
	if status.Executable != opts.Executable {
		t.Errorf("the executable = %q, want %q", status.Executable, opts.Executable)
	}
}

// TestStatusReportsTheCacheDirOfTheInstance checks the cache root the status
// reports: the cache directory of the instance's own config file when it
// defines one, the cache directory of the registration otherwise.
//
// The values come from the property list on disk, so the status describes the
// registration and not the intent of the caller.
func TestStatusReportsTheCacheDirOfTheInstance(t *testing.T) {
	launchctl, err := exec.LookPath("launchctl")
	if err != nil {
		t.Skipf("launchctl is not available: %v", err)
	}

	root := t.TempDir()
	withCacheDir := filepath.Join(root, "with-cache.json")
	if err := os.WriteFile(withCacheDir, []byte(`{"cache-dir":"/from/config/file"}`), 0o600); err != nil {
		t.Fatalf("write the config file: %v", err)
	}
	withoutCacheDir := filepath.Join(root, "without-cache.json")
	if err := os.WriteFile(withoutCacheDir, []byte(`{"local-dir":"/from/config/file/models"}`), 0o600); err != nil {
		t.Fatalf("write the config file: %v", err)
	}

	cases := []struct {
		name string
		// configFile is the config file the registration points at.
		configFile string
		// registered is the cache directory the registration records.
		registered string
		// want is the cache root the status has to report.
		want string
	}{
		{
			name:       "the config file of the instance wins",
			configFile: withCacheDir,
			registered: "/registered/cache",
			want:       "/from/config/file",
		},
		{
			name:       "the registration is the fallback",
			configFile: withoutCacheDir,
			registered: "/registered/cache",
			want:       "/registered/cache",
		},
		{
			name:       "a missing config file falls back to the registration",
			configFile: filepath.Join(root, "missing.json"),
			registered: "/registered/cache",
			want:       "/registered/cache",
		},
		{
			name:       "no cache dir at all",
			configFile: withoutCacheDir,
			registered: "",
			want:       "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			launchd := darwinStatusManager(t)
			if output, err := exec.Command(launchctl, "print", launchd.domain()).CombinedOutput(); err != nil {
				t.Skipf("the domain %s is not available: %v\n%s", launchd.domain(), err, output)
			}

			stateDir := t.TempDir()
			opts := InstallOptions{
				Scope:      ScopeUser,
				Executable: darwinWriteFakeExecutable(t),
				Listen:     DefaultListen,
				Paths: runtimepaths.Paths{
					ConfigFile: tc.configFile,
					StateDir:   stateDir,
					LogDir:     filepath.Join(stateDir, "logs"),
				},
				CacheDir: tc.registered,
			}
			darwinWriteRegistration(t, launchd, opts)

			status, err := launchd.Status(context.Background())
			if err != nil {
				t.Fatalf("Status(): %v", err)
			}
			if status.State == StateNotInstalled {
				t.Fatalf("the state = %q, want the state of a registered instance", status.State)
			}
			if status.CacheDir != tc.want {
				t.Errorf("the cache dir = %q, want %q", status.CacheDir, tc.want)
			}
		})
	}
}

// TestLaunchdLifecycle runs the whole lifecycle of an instance against
// launchd.
//
// The test uses the property list of the user of the test: a registration
// that was there before the test is restored at the end of it.
//
// A LaunchAgent needs a session that can run the jobs of the gui domain. An
// environment without such a session loads the registration but never starts
// the instance: the test skips then, and it names what the supervisor
// reported, so the next run of the test is conclusive. An environment that
// runs the instance asserts the whole lifecycle.
func TestLaunchdLifecycle(t *testing.T) {
	launchctl, err := exec.LookPath("launchctl")
	if err != nil {
		t.Skipf("launchctl is not available: %v", err)
	}

	manager, err := newPlatformManager(ScopeUser)
	if err != nil {
		t.Fatalf("newPlatformManager(): %v", err)
	}
	launchd, ok := manager.(*launchdManager)
	if !ok {
		t.Fatalf("newPlatformManager() = %T, want a launchd manager", manager)
	}

	// A LaunchAgent can only be loaded into the gui domain of a user with
	// a session.
	if output, err := exec.Command(launchctl, "print", launchd.domain()).CombinedOutput(); err != nil {
		t.Skipf("the domain %s is not available: %v\n%s", launchd.domain(), err, output)
	}

	// The state of the instance lives in a temporary directory, the
	// registration of the instance in the LaunchAgents directory of the
	// user.
	stateDir := t.TempDir()
	backup, err := backupPlist(launchd.plistPath())
	if err != nil {
		t.Fatalf("backupPlist(): %v", err)
	}
	t.Cleanup(func() {
		_ = launchd.Uninstall(context.Background())
		_ = backup.restore()
	})

	opts := InstallOptions{
		Scope:      ScopeUser,
		Executable: darwinWriteFakeExecutable(t),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(stateDir, "hfdesk.json"),
			StateDir:   stateDir,
			LogDir:     filepath.Join(stateDir, "logs"),
		},
	}
	ctx := context.Background()

	if err := launchd.Install(ctx, opts); err != nil {
		t.Fatalf("Install(): %v", err)
	}
	if _, err := os.Stat(launchd.plistPath()); err != nil {
		t.Fatalf("the property list is missing: %v", err)
	}

	// The description of the instance comes from the registration and not
	// from the state of the job, so it holds even in an environment that
	// cannot run a LaunchAgent.
	status, err := launchd.Status(ctx)
	if err != nil {
		t.Fatalf("Status(): %v", err)
	}
	if status.Executable != opts.Executable {
		t.Errorf("the executable = %q, want %q", status.Executable, opts.Executable)
	}
	if status.Listen != opts.Listen {
		t.Errorf("the listen address = %q, want %q", status.Listen, opts.Listen)
	}
	if status.UnitPath != launchd.plistPath() {
		t.Errorf("the unit path = %q, want %q", status.UnitPath, launchd.plistPath())
	}
	if status.Name != launchdLabel() {
		t.Errorf("the name = %q, want %q", status.Name, launchdLabel())
	}
	if status.Paths.ConfigFile != opts.Paths.ConfigFile {
		t.Errorf("the config file = %q, want %q", status.Paths.ConfigFile, opts.Paths.ConfigFile)
	}

	// The instance has to run for the rest of the lifecycle. An
	// environment that cannot run the jobs of the gui domain loads the
	// registration but never starts the instance: the test skips then,
	// and it names what the supervisor reported.
	status, running := darwinWaitForRunning(t, launchd, 15*time.Second)
	if !running {
		loaded, output, err := launchd.printState(ctx)
		if err != nil {
			t.Fatalf("printState(): %v", err)
		}
		if !loaded {
			// The installation registered the job with launchd, so a
			// job that is not loaded any more is a defect of the
			// installation and not a limitation of the environment.
			t.Fatalf("the job %s is not loaded into %s after the installation", launchdLabel(), launchd.domain())
		}
		t.Skipf("the environment cannot run a LaunchAgent: launchctl print %s reported: %s",
			launchd.target(), launchdPrintExcerpt(output, 4000))
	}
	if status.State != StateRunning {
		t.Errorf("the state = %q, want %q", status.State, StateRunning)
	}

	// Starting a running instance is stable.
	if err := launchd.Start(ctx); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	status, err = launchd.Status(ctx)
	if err != nil {
		t.Fatalf("Status(): %v", err)
	}
	if status.State != StateRunning {
		t.Errorf("the state after the start = %q, want %q", status.State, StateRunning)
	}

	// The installation of a second time does not fail and does not create a
	// second registration.
	if err := launchd.Install(ctx, opts); err != nil {
		t.Fatalf("Install() a second time: %v", err)
	}
	status, err = launchd.Status(ctx)
	if err != nil {
		t.Fatalf("Status(): %v", err)
	}
	if status.State != StateRunning {
		t.Errorf("the state after the second installation = %q, want %q", status.State, StateRunning)
	}

	if err := launchd.Stop(ctx); err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	status, err = launchd.Status(ctx)
	if err != nil {
		t.Fatalf("Status(): %v", err)
	}
	if status.State != StateStopped {
		t.Errorf("the state after the stop = %q, want %q (%s)", status.State, StateStopped, status.Detail)
	}

	// Stopping a stopped instance is stable.
	if err := launchd.Stop(ctx); err != nil {
		t.Fatalf("Stop() a second time: %v", err)
	}

	if err := launchd.Uninstall(ctx); err != nil {
		t.Fatalf("Uninstall(): %v", err)
	}
	status, err = launchd.Status(ctx)
	if err != nil {
		t.Fatalf("Status(): %v", err)
	}
	if status.State != StateNotInstalled {
		t.Errorf("the state after the uninstall = %q, want %q", status.State, StateNotInstalled)
	}
	// The uninstall removes the registration only: the state of the
	// instance stays.
	if _, err := os.Stat(stateDir); err != nil {
		t.Errorf("the state directory %q of the instance is gone: %v", stateDir, err)
	}

	// Uninstalling an instance that is not installed is a clean success.
	if err := launchd.Uninstall(ctx); err != nil {
		t.Fatalf("Uninstall() a second time: %v", err)
	}
}

// TestLaunchdMachineWideInstallNeedsAnElevatedShell checks the elevation
// requirement of the machine-wide scope.
//
// A LaunchDaemon is registered in /Library/LaunchDaemons and started by the
// system domain: both belong to the machine and need root. The user scope
// needs no elevation: a LaunchAgent belongs to the user who installs it.
//
// The decision is a separate function that takes the user id of the process,
// so the test drives both branches without an elevated shell.
func TestLaunchdMachineWideInstallNeedsAnElevatedShell(t *testing.T) {
	const action = "an installation"

	t.Run("the machine-wide scope refuses a shell that is not elevated", func(t *testing.T) {
		launchd := &launchdManager{scope: ScopeSystem, uid: "501", home: "/Users/somebody"}

		err := launchd.requireElevatedFor(action, 501)
		if err == nil {
			t.Fatal("requireElevatedFor() = nil, want the refusal of a shell that is not elevated")
		}
		want := fmt.Sprintf("service: %s in the system scope requires an elevated shell: the LaunchDaemon is registered in %s and started by the system domain, HFDesk never escalates privileges itself", action, launchDaemonsDir)
		if err.Error() != want {
			t.Errorf("requireElevatedFor() = %q, want %q", err, want)
		}
		if !strings.Contains(err.Error(), "elevated shell") {
			t.Errorf("requireElevatedFor() = %v, want an error that names the elevated shell", err)
		}
	})

	t.Run("the machine-wide scope accepts an elevated shell", func(t *testing.T) {
		launchd := &launchdManager{scope: ScopeSystem}

		if err := launchd.requireElevatedFor(action, 0); err != nil {
			t.Errorf("requireElevatedFor() = %v, want nil for an elevated shell", err)
		}
	})

	t.Run("the user scope is not refused", func(t *testing.T) {
		launchd := &launchdManager{scope: ScopeUser, uid: "501", home: "/Users/somebody"}

		if err := launchd.requireElevatedFor(action, 501); err != nil {
			t.Errorf("requireElevatedFor() = %v, want nil for a user scope instance", err)
		}
	})

	t.Run("the check of the process uses the effective user id of the process", func(t *testing.T) {
		launchd := &launchdManager{scope: ScopeSystem}

		err := launchd.requireElevated(action)
		forced := launchd.requireElevatedFor(action, os.Geteuid())
		switch {
		case err == nil && forced == nil:
		case err != nil && forced != nil && err.Error() == forced.Error():
		default:
			t.Errorf("requireElevated() = %v, want the decision of requireElevatedFor(%d) = %v", err, os.Geteuid(), forced)
		}
	})
}

// TestLaunchdMachineWideUninstallNeedsAnElevatedShell checks the elevation
// requirement of the machine-wide scope for an uninstallation.
//
// A LaunchDaemon is registered in /Library/LaunchDaemons and it is unloaded
// from the system domain: both belong to the machine and need root, so the
// removal of the registration needs an elevated shell as well. The user
// scope needs no elevation: a LaunchAgent belongs to the user who installed
// it.
//
// The decision is the same function that the installation uses, with the user
// id of the process as a parameter, so the test drives both branches without
// an elevated shell.
func TestLaunchdMachineWideUninstallNeedsAnElevatedShell(t *testing.T) {
	const action = "an uninstallation"

	t.Run("the machine-wide scope refuses a shell that is not elevated", func(t *testing.T) {
		launchd := &launchdManager{scope: ScopeSystem, uid: "501", home: "/Users/somebody"}

		err := launchd.requireElevatedFor(action, 501)
		if err == nil {
			t.Fatal("requireElevatedFor() = nil, want the refusal of a shell that is not elevated")
		}
		want := fmt.Sprintf("service: %s in the system scope requires an elevated shell: the LaunchDaemon is registered in %s and started by the system domain, HFDesk never escalates privileges itself", action, launchDaemonsDir)
		if err.Error() != want {
			t.Errorf("requireElevatedFor() = %q, want %q", err, want)
		}
		if !strings.Contains(err.Error(), "elevated shell") {
			t.Errorf("requireElevatedFor() = %v, want an error that names the elevated shell", err)
		}
	})

	t.Run("the machine-wide scope accepts an elevated shell", func(t *testing.T) {
		launchd := &launchdManager{scope: ScopeSystem}

		if err := launchd.requireElevatedFor(action, 0); err != nil {
			t.Errorf("requireElevatedFor() = %v, want nil for an elevated shell", err)
		}
	})

	t.Run("the user scope is not refused", func(t *testing.T) {
		launchd := &launchdManager{scope: ScopeUser, uid: "501", home: "/Users/somebody"}

		if err := launchd.requireElevatedFor(action, 501); err != nil {
			t.Errorf("requireElevatedFor() = %v, want nil for a user scope instance", err)
		}
	})

	t.Run("the check of the process uses the effective user id of the process", func(t *testing.T) {
		launchd := &launchdManager{scope: ScopeSystem}

		err := launchd.requireElevated(action)
		forced := launchd.requireElevatedFor(action, os.Geteuid())
		switch {
		case err == nil && forced == nil:
		case err != nil && forced != nil && err.Error() == forced.Error():
		default:
			t.Errorf("requireElevated() = %v, want the decision of requireElevatedFor(%d) = %v", err, os.Geteuid(), forced)
		}
	})
}

// TestLaunchdUninstallOfAnAbsentRegistrationIsACleanSuccess checks that the
// existence of the registration is checked before the elevation
// requirement.
//
// A registration that is not there is an uninstallation that is already
// done: it is a clean success, and it stays one when the shell is not
// elevated. The existence of the registration comes first, so the
// machine-wide scope reports the same clean success for an absent
// registration instead of the elevation requirement.
func TestLaunchdUninstallOfAnAbsentRegistrationIsACleanSuccess(t *testing.T) {
	t.Run("the machine-wide scope of an absent registration", func(t *testing.T) {
		launchd := &launchdManager{scope: ScopeSystem, uid: "501", home: "/Users/somebody"}
		path := launchd.plistPath()
		if _, err := os.Stat(path); err == nil {
			// The machine has a machine-wide registration: the test must
			// never remove it.
			t.Skipf("the machine has a registration at %q: the test does not touch it", path)
		}

		// The elevation requirement refuses a shell that is not elevated.
		if err := launchd.requireElevatedFor("an uninstallation", 501); err == nil {
			t.Fatal("requireElevatedFor() = nil, want the refusal of a shell that is not elevated")
		}

		// The uninstallation of an absent registration is a clean success.
		if err := launchd.Uninstall(context.Background()); err != nil {
			t.Errorf("Uninstall() = %v, want nil for an absent registration", err)
		}

		if os.Geteuid() != 0 {
			// The shell of the test is not elevated, so the elevation
			// requirement refuses it: the clean success above shows that
			// the existence of the registration is checked first.
			if err := launchd.requireElevated("an uninstallation"); err == nil {
				t.Errorf("requireElevated() = nil, want the refusal of the shell %d", os.Geteuid())
			}
		}
	})

	t.Run("the user scope of an absent registration", func(t *testing.T) {
		launchd := &launchdManager{scope: ScopeUser, uid: "501", home: t.TempDir()}

		if err := launchd.Uninstall(context.Background()); err != nil {
			t.Errorf("Uninstall() = %v, want nil for an absent registration", err)
		}
	})
}

// TestLaunchdCheckSystemPaths checks the guard of the machine-wide scope: a
// path inside the home directory of the user who runs the install command is
// refused, the machine-level paths are accepted, and a user scope instance is
// not touched at all.
func TestLaunchdCheckSystemPaths(t *testing.T) {
	home := t.TempDir()
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
			opts := darwinSystemOptions(t)
			tc.apply(&opts)

			launchd := &launchdManager{scope: ScopeSystem, home: home}
			err := launchd.checkSystemPaths(opts)
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
		opts := darwinSystemOptions(t)
		opts.Paths.StateDir = home

		launchd := &launchdManager{scope: ScopeSystem, home: home}
		err := launchd.checkSystemPaths(opts)
		if err == nil {
			t.Fatal("checkSystemPaths() = nil, want the refusal of the home directory of the user")
		}
		want := fmt.Sprintf("service: the state dir %q of a machine-wide instance must not live inside the home directory %q of the invoking user", home, home)
		if err.Error() != want {
			t.Errorf("checkSystemPaths() = %q, want %q", err, want)
		}
	})

	t.Run("a sibling with a common prefix is accepted", func(t *testing.T) {
		opts := darwinSystemOptions(t)
		opts.Paths.StateDir = sibling

		launchd := &launchdManager{scope: ScopeSystem, home: home}
		if err := launchd.checkSystemPaths(opts); err != nil {
			t.Errorf("checkSystemPaths() = %v, want nil for the sibling %q of the home %q", err, sibling, home)
		}
	})

	t.Run("a path that leaves the home again is accepted", func(t *testing.T) {
		opts := darwinSystemOptions(t)
		opts.Paths.StateDir = filepath.Join(home, "..", "user2")

		launchd := &launchdManager{scope: ScopeSystem, home: home}
		if err := launchd.checkSystemPaths(opts); err != nil {
			t.Errorf("checkSystemPaths() = %v, want nil for the path %q that leaves the home %q", err, opts.Paths.StateDir, home)
		}
	})

	t.Run("the machine-level paths are accepted", func(t *testing.T) {
		launchd := &launchdManager{scope: ScopeSystem, home: home}
		if err := launchd.checkSystemPaths(darwinSystemOptions(t)); err != nil {
			t.Errorf("checkSystemPaths() = %v, want nil for the machine-level paths", err)
		}
	})

	t.Run("the user scope is not affected", func(t *testing.T) {
		opts := darwinUserOptions(t)
		opts.Paths.StateDir = inside
		opts.CacheDir = inside

		launchd := &launchdManager{scope: ScopeUser, home: home}
		if err := launchd.checkSystemPaths(opts); err != nil {
			t.Errorf("checkSystemPaths() = %v, want nil for a user scope instance", err)
		}
	})
}

// TestLaunchdSystemInstallRefusesTheHomeOfTheUser checks that the guard of
// the machine-wide scope runs before the property list is written and before
// any directory is created: a refused installation leaves nothing behind.
//
// The guard runs before the elevation requirement, so the refusal is the same
// answer whether the command runs from an elevated shell or not, and it is
// the same answer the Linux manager gives for the same command line.
func TestLaunchdSystemInstallRefusesTheHomeOfTheUser(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	manager, err := newPlatformManager(ScopeSystem)
	if err != nil {
		t.Fatalf("newPlatformManager(ScopeSystem): %v", err)
	}
	launchd, ok := manager.(*launchdManager)
	if !ok {
		t.Fatalf("newPlatformManager(ScopeSystem) = %T, want a launchd manager", manager)
	}

	// The property list of the machine-wide scope lives in a fixed place of
	// the machine. The test never writes it: the refusal happens before
	// the property list is written. The state of the file is compared
	// before and after the installation, so a registration that exists
	// already is never touched by the test.
	path := launchd.plistPath()
	before, err := backupPlist(path)
	if err != nil {
		t.Logf("the registration %q cannot be read: %v", path, err)
		before = &plistBackup{path: path}
	}

	root := filepath.Join(home, ".config", "HFDesk", "service")
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: darwinWriteFakeExecutable(t),
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

	err = launchd.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the home directory of the user")
	}
	want := fmt.Sprintf("service: the state dir %q of a machine-wide instance must not live inside the home directory %q of the invoking user", root, home)
	if err.Error() != want {
		t.Errorf("Install() = %q, want %q", err, want)
	}

	// The refused installation creates no directory of the instance.
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused installation created the state directory %q: %v", root, err)
	}
	// The refused installation writes no property list.
	after, err := backupPlist(path)
	if err != nil {
		t.Logf("the registration %q cannot be read: %v", path, err)
		after = &plistBackup{path: path}
	}
	if before.existed != after.existed || string(before.content) != string(after.content) {
		t.Errorf("the refused installation changed the registration %q", path)
	}
}

// TestLaunchdServiceAccountName checks the name of the account a
// machine-wide instance runs as.
//
// The name of the options wins: an installation may name another account.
// An empty name means the default account of the platform.
func TestLaunchdServiceAccountName(t *testing.T) {
	if got := DefaultServiceUser(); got != "_hfdesk" {
		t.Errorf("DefaultServiceUser() = %q, want %q", got, "_hfdesk")
	}

	opts := InstallOptions{Scope: ScopeSystem}
	if got := serviceAccountName(opts); got != DefaultServiceUser() {
		t.Errorf("serviceAccountName() = %q, want the default account %q", got, DefaultServiceUser())
	}

	opts.ServiceUser = "hfdesk_other_account"
	if got := serviceAccountName(opts); got != "hfdesk_other_account" {
		t.Errorf("serviceAccountName() = %q, want %q", got, "hfdesk_other_account")
	}
}

// TestLaunchdSystemInstallRefusesAMissingServiceAccount checks that a
// machine-wide installation whose service account does not exist is refused
// with an actionable message, and that the refusal leaves nothing behind.
//
// The account is never created by the installation: the message names the
// account and the commands that create it. The check runs before any
// directory is created and before the property list is written, so the
// refusal is the same in every shell.
func TestLaunchdSystemInstallRefusesAMissingServiceAccount(t *testing.T) {
	const missing = "hfdesk_missing_service_account_for_the_test"

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")

	manager, err := newPlatformManager(ScopeSystem)
	if err != nil {
		t.Fatalf("newPlatformManager(ScopeSystem): %v", err)
	}
	launchd, ok := manager.(*launchdManager)
	if !ok {
		t.Fatalf("newPlatformManager(ScopeSystem) = %T, want a launchd manager", manager)
	}

	// The property list of the machine-wide scope lives in a fixed place of
	// the machine. The test never writes it: the refusal happens before
	// the property list is written. The state of the file is compared
	// before and after the installation, so a registration that exists
	// already is never touched by the test.
	path := launchd.plistPath()
	before, err := backupPlist(path)
	if err != nil {
		t.Logf("the registration %q cannot be read: %v", path, err)
		before = &plistBackup{path: path}
	}

	opts := InstallOptions{
		Scope:       ScopeSystem,
		Executable:  darwinWriteFakeExecutable(t),
		Listen:      DefaultListen,
		ServiceUser: missing,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   stateDir,
			LogDir:     filepath.Join(root, "logs"),
		},
	}

	err = launchd.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of a missing service account")
	}
	message := err.Error()
	for _, want := range []string{
		missing,
		"dscl",
		"UserShell",
		"UserHome",
		"UniqueID",
		"a free numeric id",
		"HFDesk never creates accounts itself",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("Install() = %q, want a message that mentions %q", message, want)
		}
	}

	// The refused installation creates no directory of the instance.
	if _, err := os.Stat(stateDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused installation created the state directory %q: %v", stateDir, err)
	}
	// The refused installation writes no property list.
	after, err := backupPlist(path)
	if err != nil {
		t.Logf("the registration %q cannot be read: %v", path, err)
		after = &plistBackup{path: path}
	}
	if before.existed != after.existed || string(before.content) != string(after.content) {
		t.Errorf("the refused installation changed the registration %q", path)
	}
}

// TestLaunchdRenderPlistCarriesTheServiceAccount checks that the property
// list of a machine-wide instance names the account it runs as and the
// primary group of that account, and that the property list of a user scope
// instance names neither.
func TestLaunchdRenderPlistCarriesTheServiceAccount(t *testing.T) {
	account := darwinCurrentAccount(t)

	t.Run("the machine-wide instance runs as the service account", func(t *testing.T) {
		opts := darwinSystemOptions(t)
		opts.ServiceUser = account.Name

		rendered, err := renderPlist(opts, account)
		if err != nil {
			t.Fatalf("renderPlist(): %v", err)
		}
		registration, err := parsePlist([]byte(rendered))
		if err != nil {
			t.Fatalf("parsePlist(): %v", err)
		}
		if registration.UserName != account.Name {
			t.Errorf("UserName = %q, want the service account %q", registration.UserName, account.Name)
		}
		if registration.GroupName != account.Group {
			t.Errorf("GroupName = %q, want the primary group %q of the service account", registration.GroupName, account.Group)
		}
	})

	t.Run("the machine-wide instance needs an account", func(t *testing.T) {
		opts := darwinSystemOptions(t)

		if _, err := renderPlist(opts, nil); err == nil {
			t.Error("renderPlist() = nil, want the refusal of a machine-wide instance without an account")
		}
	})

	t.Run("the user scope instance runs as the user itself", func(t *testing.T) {
		opts := darwinUserOptions(t)

		rendered, err := renderPlist(opts, nil)
		if err != nil {
			t.Fatalf("renderPlist(): %v", err)
		}
		registration, err := parsePlist([]byte(rendered))
		if err != nil {
			t.Fatalf("parsePlist(): %v", err)
		}
		if registration.UserName != "" || registration.GroupName != "" {
			t.Errorf("UserName = %q, GroupName = %q; want none for a user scope instance", registration.UserName, registration.GroupName)
		}
	})
}

// TestLaunchdGrantInstanceDirs checks that the directories of the instance
// are granted to the service account, and that the model and the cache trees
// are never touched.
//
// The test needs an elevated shell: only root may change the ownership of a
// directory. The test skips itself when the shell of the test is not
// elevated.
func TestLaunchdGrantInstanceDirs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skipf("only root may change the ownership of a directory, the shell of the test is %d", os.Geteuid())
	}
	account := darwinOtherAccount(t)

	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	if err := os.Mkdir(cache, 0o755); err != nil {
		t.Fatalf("create the cache directory: %v", err)
	}
	model := filepath.Join(cache, "model.bin")
	if err := os.WriteFile(model, []byte("model"), 0o644); err != nil {
		t.Fatalf("write the model: %v", err)
	}
	cacheOwnerUID, cacheOwnerGID := darwinOwner(t, cache)
	modelOwnerUID, modelOwnerGID := darwinOwner(t, model)

	opts := InstallOptions{
		Scope:    ScopeSystem,
		CacheDir: cache,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "state"),
			LogDir:     filepath.Join(root, "logs"),
		},
	}

	// The directories of the instance do not exist yet: the installation
	// creates them and grants them to the account.
	created := make(map[string][]string, 3)
	for _, dir := range instanceDirs(opts) {
		created[dir.path] = createdDirs(dir.path)
	}
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs(): %v", err)
	}

	launchd := &launchdManager{scope: ScopeSystem}
	if err := launchd.grantInstanceDirs(opts, account, created); err != nil {
		t.Fatalf("grantInstanceDirs(): %v", err)
	}

	for _, dir := range instanceDirs(opts) {
		owner, group := darwinOwner(t, dir.path)
		if owner != account.UID {
			t.Errorf("the owner of the %s %q = %d, want the service account %d", dir.name, dir.path, owner, account.UID)
		}
		if group != account.GID {
			t.Errorf("the group of the %s %q = %d, want the primary group of the service account %d", dir.name, dir.path, group, account.GID)
		}
	}

	// The cache tree belongs to whoever put it there: the installation
	// never chowns it and never walks into it.
	if owner, group := darwinOwner(t, cache); owner != cacheOwnerUID || group != cacheOwnerGID {
		t.Errorf("the owner of the cache directory %q = %d:%d, want %d:%d", cache, owner, group, cacheOwnerUID, cacheOwnerGID)
	}
	if owner, group := darwinOwner(t, model); owner != modelOwnerUID || group != modelOwnerGID {
		t.Errorf("the owner of the model %q = %d:%d, want %d:%d", model, owner, group, modelOwnerUID, modelOwnerGID)
	}
}

// TestLaunchdGrantInstanceDirsRefusesBeforeItGrants checks that a refused
// installation leaves every ownership exactly as it was.
//
// The first directory of the instance can be granted to the service
// account, the second one cannot be used: the installation has to refuse it
// before it grants the first one, so a refused installation never leaves a
// partially granted instance behind.
//
// Only root can change the ownership of a directory, so the test skips
// itself when the shell of the test is not elevated. Every path of the test
// lives in a temporary directory: the test never touches a directory of the
// machine.
func TestLaunchdGrantInstanceDirsRefusesBeforeItGrants(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skipf("only root may change the ownership of a directory, the shell of the test is %d", os.Geteuid())
	}
	account := darwinOtherAccount(t)

	// The state directory is the second directory of the instance: it is
	// the one that cannot be used. It either belongs to somebody else or
	// it is not a directory at all.
	cases := []struct {
		name string
		// prepare makes the state directory of the instance unusable.
		prepare func(t *testing.T, path string)
		// want is the refusal the installation has to report.
		want func(state string) string
	}{
		{
			name: "the account cannot write into the directory",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				// The directory belongs to the user of the test, so the
				// service account cannot write into it.
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("create the state directory: %v", err)
				}
			},
			want: func(state string) string {
				return fmt.Sprintf("service: the state directory %q of the machine-wide instance is not writable by the service account %q: grant it to the account (for example %q) and install again",
					state, account.Name, "chown "+account.Name+" "+state)
			},
		},
		{
			name: "the path is not a directory",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
					t.Fatalf("write the state directory: %v", err)
				}
			},
			want: func(state string) string {
				return fmt.Sprintf("service: the state directory %q of the machine-wide instance cannot be used: it exists and is not a directory", state)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			configDir := filepath.Join(root, "etc")
			state := filepath.Join(root, "state")
			logDir := filepath.Join(root, "logs")

			opts := InstallOptions{
				Scope:  ScopeSystem,
				Listen: DefaultListen,
				Paths: runtimepaths.Paths{
					ConfigFile: filepath.Join(configDir, "hfdesk.json"),
					StateDir:   state,
					LogDir:     logDir,
				},
			}

			// The state directory existed before the installation.
			tc.prepare(t, state)

			// The directories the installation is about to create are
			// remembered before they are created, exactly like the
			// installation does it.
			created := make(map[string][]string, 3)
			for _, dir := range instanceDirs(opts) {
				created[dir.path] = createdDirs(dir.path)
			}
			// The installation creates the directories it owns itself.
			for _, path := range created[configDir] {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatalf("create the config directory: %v", err)
				}
			}
			for _, path := range created[logDir] {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatalf("create the log directory: %v", err)
				}
			}

			configOwner, configGroup := darwinOwner(t, configDir)
			logOwner, logGroup := darwinOwner(t, logDir)
			stateOwner, stateGroup := darwinOwner(t, state)

			launchd := &launchdManager{scope: ScopeSystem}
			err := launchd.grantInstanceDirs(opts, account, created)
			if err == nil {
				t.Fatal("grantInstanceDirs() = nil, want the refusal of the state directory")
			}
			want := tc.want(state)
			if err.Error() != want {
				t.Errorf("grantInstanceDirs() = %q, want %q", err, want)
			}

			// The refusal happened before the first ownership was changed,
			// so the directory the installation would have granted still
			// belongs to its owner.
			if owner, group := darwinOwner(t, configDir); owner != configOwner || group != configGroup {
				t.Errorf("the ownership of the config directory %q = %d:%d, want %d:%d", configDir, owner, group, configOwner, configGroup)
			}
			if owner, group := darwinOwner(t, logDir); owner != logOwner || group != logGroup {
				t.Errorf("the ownership of the log directory %q = %d:%d, want %d:%d", logDir, owner, group, logOwner, logGroup)
			}
			// The directory that existed before the installation is left
			// alone as well.
			if owner, group := darwinOwner(t, state); owner != stateOwner || group != stateGroup {
				t.Errorf("the ownership of the state directory %q = %d:%d, want %d:%d", state, owner, group, stateOwner, stateGroup)
			}
		})
	}
}

// TestLaunchdGrantInstanceDirsKeepsAWritablePreExistingDirectory checks
// that a directory that existed before the installation and that the service
// account can write into is accepted and left alone.
//
// The directory belongs to whoever owns it, so the installation grants it to
// nobody: it only makes sure that the account can work there.
func TestLaunchdGrantInstanceDirsKeepsAWritablePreExistingDirectory(t *testing.T) {
	account := darwinOtherAccount(t)

	root := darwinScratchDir(t)
	// The precondition of the fixtures below: the account of the instance
	// reaches the scratch directory, so a refusal of the installation is
	// about the directories themselves.
	darwinAssertReachable(t, account, root)
	opts := InstallOptions{
		Scope:  ScopeSystem,
		Listen: DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "state"),
			LogDir:     filepath.Join(root, "logs"),
		},
	}
	// Every directory of the instance exists before the installation and
	// is writable by the account.
	owners := map[string][2]int{}
	for _, dir := range instanceDirs(opts) {
		if err := os.MkdirAll(dir.path, 0o700); err != nil {
			t.Fatalf("create the %s %q: %v", dir.name, dir.path, err)
		}
		// The mode of a create call is filtered by the umask of the
		// process, so the mode is set explicitly.
		if err := os.Chmod(dir.path, 0o777); err != nil {
			t.Fatalf("make the %s %q writable: %v", dir.name, dir.path, err)
		}
		owner, group := darwinOwner(t, dir.path)
		owners[dir.path] = [2]int{owner, group}
	}

	created := make(map[string][]string, 3)
	for _, dir := range instanceDirs(opts) {
		created[dir.path] = createdDirs(dir.path)
	}

	launchd := &launchdManager{scope: ScopeSystem}
	if err := launchd.grantInstanceDirs(opts, account, created); err != nil {
		t.Fatalf("grantInstanceDirs() = %v, want nil for a pre-existing directory the account can write into", err)
	}

	// The directories still belong to their owner: the installation grants
	// only the directories it created itself.
	for _, dir := range instanceDirs(opts) {
		owner, group := darwinOwner(t, dir.path)
		want := owners[dir.path]
		if owner != want[0] || group != want[1] {
			t.Errorf("the ownership of the %s %q = %d:%d, want %d:%d", dir.name, dir.path, owner, group, want[0], want[1])
		}
	}
}

// TestLaunchdMachineDir checks the pure test that tells a directory of the
// machine from a directory of the instance.
//
// A directory of the machine is a top level directory of the file system,
// like /etc or /var. The account of a machine-wide instance may never own
// it: the account would own everything below it.
//
// The test is lexical: it never touches the file system, so it can name a
// directory of the machine without creating it.
func TestLaunchdMachineDir(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{name: "the root of the file system", path: "/", want: true},
		{name: "a top level directory of the machine", path: "/etc", want: true},
		{name: "another top level directory of the machine", path: "/var", want: true},
		{name: "a world writable directory of the machine", path: "/tmp", want: true},
		{name: "a directory below a directory of the machine", path: "/etc/hfdesk", want: false},
		{name: "a directory of the instance", path: "/var/lib/hfdesk", want: false},
		{name: "a relative path", path: "etc/hfdesk", want: false},
		{name: "an empty path", path: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := machineDir(tc.path); got != tc.want {
				t.Errorf("machineDir(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestLaunchdGrantInstanceDirsRefusesADirectoryOfTheMachine checks that a
// machine-wide installation never hands a directory of the machine to the
// account of the instance.
//
// The rule is the rule of the Linux installation: a top level directory of
// the file system belongs to the machine, so the instance needs a directory
// of its own below it. The same command line is answered the same way on
// both platforms.
//
// The refusal happens in the check phase of the installation, before the
// first ownership is changed, so a refused installation leaves every
// ownership exactly as it was.
//
// The directory of the machine is only named by the test: every path the
// test writes to lives in a temporary directory.
func TestLaunchdGrantInstanceDirsRefusesADirectoryOfTheMachine(t *testing.T) {
	account := darwinOtherAccount(t)

	root := t.TempDir()
	configDir := filepath.Join(root, "etc")
	logDir := filepath.Join(root, "logs")
	for _, path := range []string{configDir, logDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("create the directory %q: %v", path, err)
		}
	}

	opts := InstallOptions{
		Scope:  ScopeSystem,
		Listen: DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(configDir, "hfdesk.json"),
			// The state directory is a top level directory of the
			// machine: the account would own everything below it.
			StateDir: "/etc",
			LogDir:   logDir,
		},
	}

	// The directories the installation would grant to the account are
	// remembered before they are created, exactly like the installation
	// does it. The directory of the machine is never created: it is only
	// named by the command line.
	created := map[string][]string{
		configDir: {configDir},
		"/etc":    nil,
		logDir:    {logDir},
	}

	owners := map[string][2]int{}
	for _, path := range []string{configDir, logDir} {
		owner, group := darwinOwner(t, path)
		owners[path] = [2]int{owner, group}
	}

	launchd := &launchdManager{scope: ScopeSystem}
	err := launchd.grantInstanceDirs(opts, account, created)
	if err == nil {
		t.Fatal("grantInstanceDirs() = nil, want the refusal of a directory of the machine")
	}
	want := `service: the state directory "/etc" is a directory of the machine: the instance needs a directory of its own, name one below a directory of the machine`
	if err.Error() != want {
		t.Errorf("grantInstanceDirs() = %q, want %q", err, want)
	}

	// The refusal happened before the first ownership was changed, so the
	// directories the installation would have granted still belong to
	// their owner.
	for _, path := range []string{configDir, logDir} {
		owner, group := darwinOwner(t, path)
		want := owners[path]
		if owner != want[0] || group != want[1] {
			t.Errorf("the ownership of the directory %q = %d:%d, want %d:%d", path, owner, group, want[0], want[1])
		}
	}
}

// TestLaunchdGrantInstanceDirsKeepsADirectoryBelowADirectoryOfTheMachine
// checks that the rule of the directories of the machine does not reach a
// directory below a directory of the machine.
//
// Only a top level directory of the file system belongs to the machine. A
// directory below it, like /etc/hfdesk, is a directory of the instance, so
// the rule of the machine directories does not refuse it.
//
// The path is only named by the test: the test never writes to it, it only
// reads the answer of the installation.
func TestLaunchdGrantInstanceDirsKeepsADirectoryBelowADirectoryOfTheMachine(t *testing.T) {
	account := darwinOtherAccount(t)

	root := t.TempDir()
	configDir := filepath.Join(root, "etc")
	logDir := filepath.Join(root, "logs")
	for _, path := range []string{configDir, logDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("create the directory %q: %v", path, err)
		}
	}

	opts := InstallOptions{
		Scope:  ScopeSystem,
		Listen: DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(configDir, "hfdesk.json"),
			// The state directory lies below a directory of the
			// machine: it is not a directory of the machine itself.
			StateDir: "/etc/hfdesk",
			LogDir:   logDir,
		},
	}
	created := map[string][]string{
		configDir:     {configDir},
		"/etc/hfdesk": nil,
		logDir:        {logDir},
	}

	owners := map[string][2]int{}
	for _, path := range []string{configDir, logDir} {
		owner, group := darwinOwner(t, path)
		owners[path] = [2]int{owner, group}
	}

	launchd := &launchdManager{scope: ScopeSystem}
	err := launchd.grantInstanceDirs(opts, account, created)
	// The installation answers the question of the writability of the
	// directory, but it never refuses the directory as a directory of the
	// machine.
	if err != nil && strings.Contains(err.Error(), "is a directory of the machine") {
		t.Fatalf("grantInstanceDirs() = %q, want no refusal of a directory below a directory of the machine", err)
	}

	// Nothing was granted either way.
	for _, path := range []string{configDir, logDir} {
		owner, group := darwinOwner(t, path)
		want := owners[path]
		if owner != want[0] || group != want[1] {
			t.Errorf("the ownership of the directory %q = %d:%d, want %d:%d", path, owner, group, want[0], want[1])
		}
	}
}

// TestLaunchdSystemInstallRefusesACacheDirTheAccountCannotWrite checks that
// a cache directory the service account cannot write to is refused with an
// actionable message, and that the installation leaves the tree alone.
//
// The installation never chowns a cache tree: the guidance names the
// commands that grant the access, the administrator runs them. The guidance
// never asks for a recursive ownership change of the tree.
func TestLaunchdSystemInstallRefusesACacheDirTheAccountCannotWrite(t *testing.T) {
	account := darwinOtherAccount(t)

	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	if err := os.Mkdir(cache, 0o755); err != nil {
		t.Fatalf("create the cache directory: %v", err)
	}
	beforeUID, beforeGID := darwinOwner(t, cache)

	manager, err := newPlatformManager(ScopeSystem)
	if err != nil {
		t.Fatalf("newPlatformManager(ScopeSystem): %v", err)
	}
	launchd, ok := manager.(*launchdManager)
	if !ok {
		t.Fatalf("newPlatformManager(ScopeSystem) = %T, want a launchd manager", manager)
	}

	stateDir := filepath.Join(root, "state")
	opts := InstallOptions{
		Scope:       ScopeSystem,
		Executable:  darwinWriteFakeExecutable(t),
		Listen:      DefaultListen,
		ServiceUser: account.Name,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   stateDir,
			LogDir:     filepath.Join(root, "logs"),
		},
		CacheDir: cache,
		HubDir:   filepath.Join(cache, "hub"),
		LocalDir: filepath.Join(root, "models"),
	}

	err = launchd.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of a cache directory the account cannot write to")
	}
	message := err.Error()
	for _, want := range []string{account.Name, cache, "chown"} {
		if !strings.Contains(message, want) {
			t.Errorf("Install() = %q, want a message that mentions %q", message, want)
		}
	}
	// The refusal never recommends a recursive ownership change of the
	// cache tree.
	if strings.Contains(message, "-R") {
		t.Errorf("Install() = %q, want a message without a recursive ownership command", message)
	}

	// The ownership of the cache tree is unchanged.
	if owner, group := darwinOwner(t, cache); owner != beforeUID || group != beforeGID {
		t.Errorf("the owner of the cache directory %q = %d:%d, want %d:%d", cache, owner, group, beforeUID, beforeGID)
	}
	// The refused installation creates no directory of the instance.
	if _, err := os.Stat(stateDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused installation created the state directory %q: %v", stateDir, err)
	}
}

// TestLaunchdRefusesANamedDirTheAccountCannotWrite checks the refusal of a
// named directory the service account cannot write to.
//
// The refusal names the directory, the account and the access that is
// missing. It never recommends a recursive ownership change of a model or a
// cache tree: the access is granted to the directory itself or through the
// group that owns it, and for a tree that holds a lot of data the group is
// preferable to a new owner.
//
// The test needs no elevated shell: it only reads the ownership of a
// directory of a temporary directory.
func TestLaunchdRefusesANamedDirTheAccountCannotWrite(t *testing.T) {
	account := darwinOtherAccount(t)

	root := t.TempDir()
	// The directory belongs to the user of the test and to nobody else, so
	// the service account has no write access to it.
	cache := filepath.Join(root, "cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatalf("create the cache directory: %v", err)
	}

	opts := InstallOptions{
		Scope:    ScopeSystem,
		CacheDir: cache,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "state"),
			LogDir:     filepath.Join(root, "logs"),
		},
	}

	launchd := &launchdManager{scope: ScopeSystem}
	err := launchd.checkNamedDirs(opts, account)
	if err == nil {
		t.Fatal("checkNamedDirs() = nil, want the refusal of a cache directory the account cannot write to")
	}
	message := err.Error()

	// The refusal names the directory, the account and the access that is
	// missing.
	for _, want := range []string{
		"the service account " + strconv.Quote(account.Name),
		strconv.Quote(cache),
		"write access",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("checkNamedDirs() = %q, want a message that mentions %q", message, want)
		}
	}
	// The refusal never recommends a recursive ownership change of the
	// tree.
	if strings.Contains(message, "-R") {
		t.Errorf("checkNamedDirs() = %q, want a message without a recursive ownership command", message)
	}
	// The guidance is the ownership of the directory itself and the group
	// that owns it.
	for _, want := range []string{
		"chown " + account.Name + " " + cache,
		"member of the group that owns the directory",
		"group access is preferable to changing the owner",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("checkNamedDirs() = %q, want a message that mentions %q", message, want)
		}
	}
}

// TestLaunchdCheckNamedDirsUsesTheRealLocationOfTheDirectory checks that the
// exemption for a directory inside the state directory of the instance is
// decided on the real location of the paths.
//
// The kernel follows the symbolic links of a path before it uses it, so a
// link inside the state directory that points out of it leaves the state
// directory: the account has to reach the target of the link on its own, and
// the installation refuses the directory when it cannot.
func TestLaunchdCheckNamedDirsUsesTheRealLocationOfTheDirectory(t *testing.T) {
	account := darwinOtherAccount(t)
	root := darwinScratchDir(t)

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

	// A plain directory inside the state directory. The account of the
	// check cannot write into it: the installation grants the state
	// directory to the account, and not the tree below it.
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

	launchd := &launchdManager{scope: ScopeSystem}
	opts := InstallOptions{
		Scope: ScopeSystem,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   state,
			LogDir:     filepath.Join(root, "logs"),
		},
	}

	cases := []struct {
		name    string
		cache   string
		wantErr bool
	}{
		{name: "a plain directory inside the state directory", cache: plain},
		{name: "a missing directory inside the state directory", cache: missing},
		{name: "a link inside the state directory that points out of it", cache: outside, wantErr: true},
		{name: "a link inside the state directory that points to a reachable place", cache: inside},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts.CacheDir = tc.cache
			err := launchd.checkNamedDirs(opts, account)
			if !tc.wantErr {
				if err != nil {
					t.Errorf("checkNamedDirs() = %v, want nil for the directory %q", err, tc.cache)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkNamedDirs() = nil, want the refusal of the directory %q", tc.cache)
			}
			// The refusal names the directory, the account and the
			// access that is missing.
			message := err.Error()
			for _, want := range []string{
				"the service account " + strconv.Quote(account.Name),
				strconv.Quote(tc.cache),
				"write access",
			} {
				if !strings.Contains(message, want) {
					t.Errorf("checkNamedDirs() = %q, want a message that mentions %q", message, want)
				}
			}
		})
	}

	t.Run("the state directory is a link to another place", func(t *testing.T) {
		// The state directory of the instance is a link: the installation
		// grants the directory the link points at to the account, so a
		// directory inside it is not checked against the account.
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
		opts.CacheDir = filepath.Join(link, "models")
		if err := launchd.checkNamedDirs(opts, account); err != nil {
			t.Errorf("checkNamedDirs() = %v, want nil for a directory inside the state directory of the instance", err)
		}

		// The same directory named by its real location is inside the
		// state directory as well.
		opts.CacheDir = models
		if err := launchd.checkNamedDirs(opts, account); err != nil {
			t.Errorf("checkNamedDirs() = %v, want nil for a directory inside the state directory of the instance", err)
		}
	})
}

// TestLaunchdAccessCheckUsesTheEffectivePermission checks that the access
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
func TestLaunchdAccessCheckUsesTheEffectivePermission(t *testing.T) {
	root := darwinScratchDir(t)

	// The group of the directories of the test is the group of the test
	// process, so the other identity reaches them through the group.
	probe := filepath.Join(root, "probe")
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatalf("create the probe directory: %v", err)
	}
	_, group := darwinOwner(t, probe)

	// The owner of the test is the account of the test process.
	owner := &serviceAccount{
		Name:   "hfdesk-owner-test",
		UID:    os.Getuid(),
		GID:    os.Getgid(),
		Group:  "hfdesk-owner-test",
		Groups: []int{os.Getgid()},
	}
	// The other identity owns nothing of the test and belongs to the
	// group of its directories.
	other := &serviceAccount{
		Name:   "hfdesk-access-test",
		UID:    65534,
		GID:    group,
		Group:  "hfdesk-access-test",
		Groups: []int{group},
	}

	// The precondition of the cases below: every account reaches the
	// scratch directory, so a refusal of the check is about the directory
	// of the case and not about the scratch directory.
	for _, account := range []*serviceAccount{owner, other} {
		darwinAssertReachable(t, account, root)
	}

	cases := []struct {
		name    string
		dir     string
		account *serviceAccount
		mode    os.FileMode
		wantErr bool
	}{
		{
			name:    "a directory the account owns",
			dir:     "owned",
			account: owner,
			mode:    0o700,
		},
		{
			name:    "a directory the account owns without the write permission",
			dir:     "readonly",
			account: owner,
			mode:    0o500,
			wantErr: true,
		},
		{
			name:    "a directory the account owns without the write and the execute permission",
			dir:     "closed",
			account: owner,
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
			ok, err := accountCanWrite(dir, tc.account)
			if tc.wantErr {
				if ok {
					t.Errorf("accountCanWrite(%q) = true, want a refusal", dir)
				}
				return
			}
			if !ok {
				t.Errorf("accountCanWrite(%q) = false (%v), want true", dir, err)
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
		ok, err := accountCanWrite(child, other)
		if ok {
			t.Error("accountCanWrite() = true, want the refusal of a directory the account cannot reach")
		}
		if err != nil {
			t.Logf("accountCanWrite() = %v", err)
		}
	})
}

// TestLaunchdAccessCheckUsesTheSupplementaryGroups checks that the access
// check of a directory the service account has to use knows the
// supplementary groups of the account.
//
// The primary group of the account of the check is another one, so the
// directory is reachable through the supplementary group alone.
func TestLaunchdAccessCheckUsesTheSupplementaryGroups(t *testing.T) {
	supplementary, ok := darwinSupplementaryGroup(t)
	if !ok {
		t.Skip("skipping: the account of the test belongs to no group besides its primary one")
	}
	root := darwinScratchDir(t)

	dir := filepath.Join(root, "supplementary")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}
	// The directory belongs to the supplementary group of the account, not
	// to the account itself.
	if err := os.Chown(dir, os.Getuid(), supplementary); err != nil {
		t.Fatalf("give the directory to the group %d: %v", supplementary, err)
	}

	account := &serviceAccount{
		Name:   "hfdesk-access-test",
		UID:    65534,
		GID:    os.Getgid(),
		Group:  "hfdesk-access-test",
		Groups: []int{os.Getgid(), supplementary},
	}

	// The precondition of the checks below: the account reaches the scratch
	// directory, so a refusal of the check is about the directory itself.
	darwinAssertReachable(t, account, root)

	// The group permissions apply to the account: it can create entries
	// in the directory and it can reach it.
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatalf("make the directory usable: %v", err)
	}
	allowed, err := accountCanWrite(dir, account)
	if !allowed {
		t.Errorf("accountCanWrite() = false (%v), want true for a directory of the supplementary group of the account", err)
	}

	// The group has no execute permission: the account cannot use the
	// directory.
	if err := os.Chmod(dir, 0o620); err != nil {
		t.Fatalf("close the directory: %v", err)
	}
	allowed, err = accountCanWrite(dir, account)
	if allowed {
		t.Error("accountCanWrite() = true, want the refusal of a directory without the execute permission of the group")
	}
}

// TestLaunchdStopWaitCoversTheShutdownOfTheServer checks that the deadline
// of the supervisor covers the graceful shutdown of the server.
//
// The server publishes the budget of its own shutdown: the drain of the HTTP
// server and the close of the job manager, each with its own budget. The
// job and the manager have to wait at least that long, or launchd kills the
// instance while it is still saving its state.
func TestLaunchdStopWaitCoversTheShutdownOfTheServer(t *testing.T) {
	if ShutdownBudget < server.GracefulShutdownTimeout {
		t.Errorf("the shared budget of the service layer = %s, want at least %s", ShutdownBudget, server.GracefulShutdownTimeout)
	}
	if stopWaitTimeout < server.GracefulShutdownTimeout {
		t.Errorf("the stop wait of the manager = %s, want at least %s", stopWaitTimeout, server.GracefulShutdownTimeout)
	}
	if stopWaitTimeout != ShutdownBudget {
		t.Errorf("the stop wait of the manager = %s, want the shared budget %s", stopWaitTimeout, ShutdownBudget)
	}
	// The wait of the manager itself uses the shared budget, so a stop
	// never ends before the shutdown it waits for.
	if budget := (&launchdManager{}).shutdownBudget(); budget != ShutdownBudget {
		t.Errorf("the shutdown budget of the manager = %s, want the shared budget %s", budget, ShutdownBudget)
	}

	opts := darwinUserOptions(t)
	rendered, err := renderPlist(opts, nil)
	if err != nil {
		t.Fatalf("renderPlist(): %v", err)
	}
	registration, err := parsePlist([]byte(rendered))
	if err != nil {
		t.Fatalf("parsePlist(): %v", err)
	}
	if registration.ExitTimeOut == 0 {
		t.Fatalf("the property list has no ExitTimeOut key:\n%s", rendered)
	}
	timeout := time.Duration(registration.ExitTimeOut) * time.Second
	if timeout < server.GracefulShutdownTimeout {
		t.Errorf("the ExitTimeOut of the property list = %s, want at least %s", timeout, server.GracefulShutdownTimeout)
	}
	if timeout != ShutdownBudget {
		t.Errorf("the ExitTimeOut of the property list = %s, want the shared budget %s", timeout, ShutdownBudget)
	}
}

// TestLaunchdUserScopeNeedsNoServiceAccount checks that the user scope is
// unaffected by the service account of the machine-wide scope.
//
// A user scope instance runs as the user who installs it: it needs no
// service account, it is granted no directory and it names no account in
// its registration.
func TestLaunchdUserScopeNeedsNoServiceAccount(t *testing.T) {
	launchd := &launchdManager{scope: ScopeUser, home: t.TempDir()}

	opts := darwinUserOptions(t)
	// The account of the options is ignored in the user scope: the
	// instance runs as the user itself.
	opts.ServiceUser = "hfdesk_missing_service_account_for_the_test"

	account, err := launchd.resolveAccount(opts)
	if err != nil {
		t.Fatalf("resolveAccount() = %v, want nil for a user scope instance", err)
	}
	if account != nil {
		t.Errorf("resolveAccount() = %+v, want nil for a user scope instance", account)
	}
	if err := launchd.checkNamedDirs(opts, account); err != nil {
		t.Errorf("checkNamedDirs() = %v, want nil for a user scope instance", err)
	}
	if err := launchd.grantInstanceDirs(opts, account, nil); err != nil {
		t.Errorf("grantInstanceDirs() = %v, want nil for a user scope instance", err)
	}
}

// darwinWaitForRunning waits until the instance runs and reports whether it
// runs.
//
// The instance runs when the supervisor reports the state "running" or when
// the supervisor reports the pid of a live process: a state line of a
// release that this build does not know must not hide a running instance.
// The decision is the decision of the status, so the test and the status
// never disagree about the state of the instance. The status of the last
// poll is returned as well, so a caller can report what the supervisor said.
func darwinWaitForRunning(t *testing.T, manager *launchdManager, timeout time.Duration) (Status, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var status Status
	for {
		var err error
		status, err = manager.Status(context.Background())
		if err != nil {
			t.Fatalf("Status(): %v", err)
		}
		if status.State == StateRunning {
			return status, true
		}
		loaded, output, err := manager.printState(context.Background())
		if err != nil {
			t.Fatalf("printState(): %v", err)
		}
		if loaded {
			state, _ := launchdStatusState(manager.target(), output)
			if state == StateRunning {
				return status, true
			}
		}
		if time.Now().After(deadline) {
			return status, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// darwinWriteFakeExecutable writes a program that stays alive until it is
// signalled and returns its path.
func darwinWriteFakeExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hfdesk")
	script := "#!/bin/sh\n# The registered program of the test: it waits for a signal.\nexec sleep 300\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the program: %v", err)
	}
	return path
}

// darwinStatusManager returns a manager that reads its registration from a
// temporary home directory, so a test never touches the registration of the
// user of the test.
func darwinStatusManager(t *testing.T) *launchdManager {
	t.Helper()
	return &launchdManager{
		scope: ScopeUser,
		uid:   strconv.Itoa(os.Getuid()),
		home:  t.TempDir(),
	}
}

// darwinCurrentAccount returns the service account of the user of the test.
//
// The account of the user of the test exists on every machine, so the tests
// use it as the identity of a machine-wide instance.
func darwinCurrentAccount(t *testing.T) *serviceAccount {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Skipf("the user of the test cannot be resolved: %v", err)
	}
	account, err := resolveServiceAccount(InstallOptions{Scope: ScopeSystem, ServiceUser: current.Username})
	if err != nil {
		t.Skipf("the account %q of the user of the test cannot be resolved: %v", current.Username, err)
	}
	return account
}

// darwinOtherAccount returns an account of the machine that is not the user
// of the test.
//
// The tests need an account that exists and that owns nothing of the test,
// so the ownership of the directories of the test is unchanged.
func darwinOtherAccount(t *testing.T) *serviceAccount {
	t.Helper()
	for _, name := range []string{"nobody", "daemon"} {
		account, err := resolveServiceAccount(InstallOptions{Scope: ScopeSystem, ServiceUser: name})
		if err != nil {
			continue
		}
		if account.UID == os.Getuid() {
			continue
		}
		return account
	}
	t.Skip("the machine has no service account besides the user of the test")
	return nil
}

// darwinOwner returns the owner and the group of a path.
func darwinOwner(t *testing.T, path string) (int, int) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("the ownership of %q cannot be read", path)
	}
	return int(stat.Uid), int(stat.Gid)
}

// darwinMachineTempRoot is the temporary directory of the machine that every
// account can pass through.
//
// The temporary directory of a test is owner-only, so an account that is not
// the owner of the test never reaches a directory below it. The access check
// of the service layer asks for the same: a directory below a parent
// without the execute permission is out of reach of the account.
//
// The fixtures of the access checks therefore live in the temporary directory
// of the machine: /tmp is of the mode 1777 on Linux and on macOS, where it
// resolves to /private/tmp. A fixture is then out of the reach of the account
// only because of its own permissions, which is what the checks are about.
const darwinMachineTempRoot = "/tmp"

// darwinScratchDir returns a scratch directory in the temporary directory of
// the machine.
//
// The directory is traversable by every account, and it is removed when the
// test ends.
func darwinScratchDir(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp(darwinMachineTempRoot, "hfdesk-darwin-test-")
	if err != nil {
		t.Fatalf("create the scratch directory in %q: %v", darwinMachineTempRoot, err)
	}
	// The mode of a create call is filtered by the umask of the process,
	// so the mode is set explicitly.
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("make the scratch directory %q traversable: %v", root, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove the scratch directory %q: %v", root, err)
		}
	})
	return root
}

// darwinAssertReachable asserts that the account reaches the scratch
// directory.
//
// The precondition of every fixture of an access check: the account has to
// pass through the scratch directory, otherwise a refusal of the check tells
// more about the scratch directory than about the fixture itself. The
// assertion names the account and the directory, so a broken precondition is
// diagnosable instead of looking like a wrong verdict of the check.
func darwinAssertReachable(t *testing.T, account *serviceAccount, root string) {
	t.Helper()
	ok, err := accountCanTraverse(root, accessAccount{
		UID:    account.UID,
		GID:    account.GID,
		Groups: account.Groups,
	})
	if err != nil {
		t.Fatalf("the account %q cannot pass through the scratch directory %q: %v", account.Name, root, err)
	}
	if !ok {
		t.Fatalf("the account %q cannot pass through the scratch directory %q: the fixture below it would be out of the reach of the account for a reason that has nothing to do with the fixture itself", account.Name, root)
	}
}

// darwinSupplementaryGroup returns the id of a supplementary group of the
// account of the test process.
//
// The second result is false when the account of the test belongs to no
// group besides its primary one.
func darwinSupplementaryGroup(t *testing.T) (int, bool) {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Skipf("skipping: the account of the test process is unknown: %v", err)
	}
	ids, err := current.GroupIds()
	if err != nil {
		t.Skipf("skipping: the groups of the account of the test are unknown: %v", err)
	}
	for _, id := range ids {
		gid, err := strconv.Atoi(id)
		if err != nil || gid == os.Getgid() {
			continue
		}
		return gid, true
	}
	return 0, false
}

// darwinWriteRegistration renders the property list of an installation and
// writes it to the registration of the manager.
func darwinWriteRegistration(t *testing.T, launchd *launchdManager, opts InstallOptions) {
	t.Helper()
	rendered, err := renderPlist(opts, nil)
	if err != nil {
		t.Fatalf("renderPlist(): %v", err)
	}
	path := launchd.plistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the registration directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(rendered), plistMode); err != nil {
		t.Fatalf("write the property list: %v", err)
	}
}

// darwinPlistStrings returns every string of a registration, including the
// strings of the nested dictionaries and arrays.
func darwinPlistStrings(registration plistRegistration) []string {
	var values []string
	for _, key := range registration.Keys {
		switch key {
		case "ProgramArguments":
			values = append(values, registration.ProgramArguments...)
		case "EnvironmentVariables":
			for _, value := range registration.Environment {
				values = append(values, value)
			}
		case "Label":
			values = append(values, registration.Label)
		case "WorkingDirectory":
			values = append(values, registration.WorkingDirectory)
		case "UserName":
			values = append(values, registration.UserName)
		case "GroupName":
			values = append(values, registration.GroupName)
		}
	}
	return values
}

// darwinStripXMLEntities removes the XML entities of a value, so an escaped
// ampersand of a path is not mistaken for a shell operator.
func darwinStripXMLEntities(value string) string {
	for _, entity := range []string{"&amp;", "&lt;", "&gt;", "&quot;", "&apos;", "&#34;", "&#39;", "&#x9;", "&#xA;", "&#xD;"} {
		value = strings.ReplaceAll(value, entity, "")
	}
	return value
}

// darwinFakeSupervisor replaces the supervisor of the machine with a fake
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
func darwinFakeSupervisor(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake supervisor: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// darwinTempInstallOptions returns the options of a user scope installation
// whose directories live below the given root.
//
// The scope is the user scope: it needs neither an elevated shell nor a
// service account, so the test runs everywhere.
func darwinTempInstallOptions(t *testing.T, root string) InstallOptions {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "hfdesk")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("write the program: %v", err)
	}
	return InstallOptions{
		Scope:      ScopeUser,
		Executable: executable,
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
	}
}

// TestLaunchdCreatedDirOrder checks the order of the directories a failed
// installation removes: the directories are listed in the order they are
// created, so a parent is always removed after its children.
func TestLaunchdCreatedDirOrder(t *testing.T) {
	root := t.TempDir()
	opts := InstallOptions{
		Scope:      ScopeUser,
		Executable: filepath.Join(root, "hfdesk"),
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
	if !reflect.DeepEqual(order, want) {
		t.Errorf("createdDirOrder() = %v, want %v", order, want)
	}
}

// TestLaunchdFailedInstallRemovesTheDirectoriesItCreated checks that a
// failed installation removes the directories it created, so a failed attempt
// leaves the machine as it was.
func TestLaunchdFailedInstallRemovesTheDirectoriesItCreated(t *testing.T) {
	darwinFakeSupervisor(t)
	root := t.TempDir()

	opts := darwinTempInstallOptions(t, root)
	m := darwinStatusManager(t)
	if err := m.Install(context.Background(), opts); err == nil {
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

// TestLaunchdFailedInstallKeepsADirectoryThatExistedBefore checks that a
// failed installation never removes a directory that existed before it: such
// a directory belongs to whoever owns it, with the ownership and the
// permissions it had.
func TestLaunchdFailedInstallKeepsADirectoryThatExistedBefore(t *testing.T) {
	darwinFakeSupervisor(t)
	root := t.TempDir()

	// The config directory existed before the installation.
	config := filepath.Join(root, "etc")
	if err := os.Mkdir(config, 0o750); err != nil {
		t.Fatalf("create the config directory: %v", err)
	}
	owner, group := darwinOwner(t, config)

	opts := darwinTempInstallOptions(t, root)
	m := darwinStatusManager(t)
	if err := m.Install(context.Background(), opts); err == nil {
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
	if uid, gid := darwinOwner(t, config); uid != owner || gid != group {
		t.Errorf("the ownership of the config directory %q = %d:%d, want %d:%d", config, uid, gid, owner, group)
	}
	// The directory the installation created inside it is gone.
	created := filepath.Join(config, "hfdesk")
	if _, err := os.Stat(created); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the directory %q is still there after the failed installation (stat error: %v)", created, err)
	}
}

// TestLaunchdFailedInstallKeepsACreatedDirectoryThatHoldsData checks that
// the cleanup of a failed installation never deletes data: a directory the
// installation created but that holds something is kept, and the caller
// still gets the original error of the installation.
func TestLaunchdFailedInstallKeepsACreatedDirectoryThatHoldsData(t *testing.T) {
	darwinFakeSupervisor(t)
	root := t.TempDir()

	opts := darwinTempInstallOptions(t, root)
	state := opts.Paths.StateDir
	m := darwinStatusManager(t)

	// The error of the failed supervisor call is the error the caller
	// gets: the cleanup of the directories never replaces it.
	t.Setenv("HFDESK_TEST_MARKER_DIR", t.TempDir())
	_, _, wantErr := m.printState(context.Background())
	if wantErr == nil {
		t.Fatal("the fake supervisor did not fail")
	}

	// The fake supervisor writes a marker into the state directory, so the
	// directory holds data when the installation fails.
	t.Setenv("HFDESK_TEST_MARKER_DIR", state)
	err := m.Install(context.Background(), opts)
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

// TestLaunchdFailedInstallRemovesTheDirectoriesOfAFailedPreparation checks
// that an installation that fails while it prepares the directories of the
// instance leaves the machine as it was as well: the directories it created
// before the failure are removed too.
func TestLaunchdFailedInstallRemovesTheDirectoriesOfAFailedPreparation(t *testing.T) {
	darwinFakeSupervisor(t)
	root := t.TempDir()

	opts := darwinTempInstallOptions(t, root)
	// The log directory is the root of the file system: the installation
	// refuses to tighten it, and it fails after the other two
	// directories were created.
	opts.Paths.LogDir = string(filepath.Separator)

	m := darwinStatusManager(t)
	err := m.Install(context.Background(), opts)
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

// TestLaunchdSuccessfulInstallKeepsItsDirectories checks that a successful
// installation keeps the directories it created: the cleanup of a failed
// installation never touches the directories of an instance that runs.
func TestLaunchdSuccessfulInstallKeepsItsDirectories(t *testing.T) {
	darwinFakeSupervisor(t)
	t.Setenv("HFDESK_TEST_SUPERVISOR", "ok")
	root := t.TempDir()

	opts := darwinTempInstallOptions(t, root)
	m := darwinStatusManager(t)
	if err := m.Install(context.Background(), opts); err != nil {
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

// TestLaunchdLifecycleOfAnAbsentRegistration checks the lifecycle commands
// against an instance that is not registered: the stop is a stable success,
// and the start and the restart report that the instance is not installed.
//
// The test never touches a real installation: the registration of the user
// scope lives in a temporary home directory, and the test skips itself when
// the machine holds a LaunchDaemon.
func TestLaunchdLifecycleOfAnAbsentRegistration(t *testing.T) {
	t.Run("the user scope", func(t *testing.T) {
		launchd := &launchdManager{scope: ScopeUser, uid: strconv.Itoa(os.Getuid()), home: t.TempDir()}
		path := launchd.plistPath()
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("the registration %q exists, the test needs an instance that is not registered", path)
		}

		ctx := context.Background()
		// An instance that is not registered does not run either: the
		// state the caller asked for already holds.
		if err := launchd.Stop(ctx); err != nil {
			t.Errorf("Stop() = %v, want nil for an instance that is not registered", err)
		}
		// An instance that is not registered cannot be started, and the
		// reason is reported in every shell.
		err := launchd.Start(ctx)
		if err == nil {
			t.Fatal("Start() = nil, want an error for an instance that is not registered")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("Start() = %v, want an error that names the missing registration", err)
		}
		// A restart cannot start the instance either.
		err = launchd.Restart(ctx)
		if err == nil {
			t.Fatal("Restart() = nil, want an error for an instance that is not registered")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("Restart() = %v, want an error that names the missing registration", err)
		}
	})

	t.Run("the machine-wide scope", func(t *testing.T) {
		launchd := &launchdManager{scope: ScopeSystem, uid: "0", home: t.TempDir()}
		path := launchd.plistPath()
		if _, err := os.Stat(path); err == nil {
			t.Skipf("the machine has a registration at %q: the test does not touch it", path)
		}

		// The elevation requirement refuses a shell that is not
		// elevated.
		if err := launchd.requireElevatedFor("a stop", 501); err == nil {
			t.Fatal("requireElevatedFor() = nil, want the refusal of a shell that is not elevated")
		}

		ctx := context.Background()
		// The existence of the registration comes first, so the stop of
		// an instance that is not registered stays a clean success even
		// when the shell is not elevated.
		if err := launchd.Stop(ctx); err != nil {
			t.Errorf("Stop() = %v, want nil for an instance that is not registered", err)
		}
		err := launchd.Start(ctx)
		if err == nil {
			t.Fatal("Start() = nil, want an error for an instance that is not registered")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("Start() = %v, want an error that names the missing registration", err)
		}
		if strings.Contains(err.Error(), "elevated shell") {
			t.Errorf("Start() = %v, want the missing registration and not the elevation requirement", err)
		}
		err = launchd.Restart(ctx)
		if err == nil {
			t.Fatal("Restart() = nil, want an error for an instance that is not registered")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("Restart() = %v, want an error that names the missing registration", err)
		}
	})
}

// TestLaunchdMachineWideLifecycleNeedsAnElevatedShell checks the elevation
// requirement of the machine-wide scope for the lifecycle commands.
//
// A LaunchDaemon is registered in /Library/LaunchDaemons and it is started
// and stopped by the system domain: both belong to the machine and need
// root. The user scope needs no elevation: a LaunchAgent belongs to the
// user who installs it.
//
// The decision is the same function that the installation uses, with the
// user id of the process as a parameter, so the test drives both branches
// without an elevated shell.
func TestLaunchdMachineWideLifecycleNeedsAnElevatedShell(t *testing.T) {
	for _, action := range []string{"a start", "a stop", "a restart"} {
		t.Run("the machine-wide scope refuses "+action+" without an elevated shell", func(t *testing.T) {
			launchd := &launchdManager{scope: ScopeSystem, uid: "501", home: "/Users/somebody"}

			err := launchd.requireElevatedFor(action, 501)
			if err == nil {
				t.Fatal("requireElevatedFor() = nil, want the refusal of a shell that is not elevated")
			}
			want := fmt.Sprintf("service: %s in the system scope requires an elevated shell: the LaunchDaemon is registered in %s and started by the system domain, HFDesk never escalates privileges itself", action, launchDaemonsDir)
			if err.Error() != want {
				t.Errorf("requireElevatedFor() = %q, want %q", err, want)
			}
		})

		t.Run("the machine-wide scope accepts "+action+" with an elevated shell", func(t *testing.T) {
			launchd := &launchdManager{scope: ScopeSystem}

			if err := launchd.requireElevatedFor(action, 0); err != nil {
				t.Errorf("requireElevatedFor() = %v, want nil for an elevated shell", err)
			}
		})

		t.Run("the user scope is not refused for "+action, func(t *testing.T) {
			launchd := &launchdManager{scope: ScopeUser, uid: "501", home: "/Users/somebody"}

			if err := launchd.requireElevatedFor(action, 501); err != nil {
				t.Errorf("requireElevatedFor() = %v, want nil for a user scope instance", err)
			}
		})
	}

	t.Run("the check of the process uses the effective user id of the process", func(t *testing.T) {
		launchd := &launchdManager{scope: ScopeSystem}

		err := launchd.requireElevated("a start")
		forced := launchd.requireElevatedFor("a start", os.Geteuid())
		switch {
		case err == nil && forced == nil:
		case err != nil && forced != nil && err.Error() == forced.Error():
		default:
			t.Errorf("requireElevated() = %v, want the decision of requireElevatedFor(%d) = %v", err, os.Geteuid(), forced)
		}
	})
}

// darwinFakePID is the process id the fake supervisor reports for a job
// that runs.
//
// The id is above every id a process of a machine can have, so a test that
// reads the state of the fake job never depends on a process of the machine
// that happens to carry the same id.
const darwinFakePID = 999999999

// darwinFakeLaunchd is a fake supervisor that records the commands the
// manager issues.
//
// The fake models the state of a launchd job: whether it is loaded into its
// domain, whether it runs and whether it is disabled in the override
// database of launchd. Every command is recorded, so a test can assert the
// sequence the manager issued, and a rule can make a command fail, so a
// test can drive a failure of the supervisor.
//
// The fake never touches the machine: it answers the commands of launchd
// from its own state.
type darwinFakeLaunchd struct {
	// loaded reports whether the job is loaded into its domain.
	loaded bool
	// running reports whether the job runs.
	running bool
	// pid is the process id of the job while it runs.
	pid int
	// disabled reports whether the job is disabled in the override
	// database of launchd.
	disabled bool
	// keepRunning makes the fake ignore the signal of a stop: the
	// instance stays alive, so the wait of the manager runs into its
	// budget.
	keepRunning bool
	// commands holds every command the manager issued, in order.
	commands []string
	// failures maps the first word of a command to the error the fake
	// returns for it.
	failures map[string]error
}

// run answers a command of the supervisor.
func (f *darwinFakeLaunchd) run(ctx context.Context, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("service: launchctl: the fake supervisor got no command")
	}
	command := strings.Join(args, " ")
	f.commands = append(f.commands, command)
	if err, ok := f.failures[args[0]]; ok {
		return "", err
	}
	switch args[0] {
	case "print":
		if !f.loaded {
			return "", fmt.Errorf("service: launchctl %s: exit status 1: Could not find service", command)
		}
		return f.printOutput(), nil
	case "print-disabled":
		if f.disabled {
			return launchdLabel() + " => disabled\n", nil
		}
		return "", nil
	case "kill":
		if !f.keepRunning {
			f.running = false
			f.pid = 0
		}
		return "", nil
	case "bootout":
		f.loaded = false
		f.running = false
		f.pid = 0
		return "", nil
	case "bootstrap":
		f.loaded = true
		f.running = true
		f.pid = darwinFakePID
		return "", nil
	case "enable":
		f.disabled = false
		return "", nil
	case "disable":
		f.disabled = true
		return "", nil
	case "kickstart":
		f.loaded = true
		f.running = true
		f.pid = darwinFakePID
		return "", nil
	default:
		return "", fmt.Errorf("service: launchctl %s: the fake supervisor does not know the command", command)
	}
}

// printOutput renders the output of "launchctl print" for the fake job.
func (f *darwinFakeLaunchd) printOutput() string {
	state := "not running"
	pid := 0
	if f.running {
		state = "running"
		pid = f.pid
	}
	return fmt.Sprintf("%s = {\n\tstate = %s\n\tpid = %d\n}", launchdLabel(), state, pid)
}

// TestLaunchdRestartStopsTheInstanceBeforeItStartsTheReplacement checks
// that a restart is the graceful stop followed by the start.
//
// The instance has to drain its connections and save its state before the
// replacement starts: a restart that kills the running process and starts
// the replacement at the same time loses the state of the instance and lets
// the two instances compete for the bind address.
//
// The commands the manager issues are asserted, so the check needs no
// supervisor: the fake records them.
func TestLaunchdRestartStopsTheInstanceBeforeItStartsTheReplacement(t *testing.T) {
	launchd := darwinStatusManager(t)
	fake := &darwinFakeLaunchd{loaded: true, running: true, pid: darwinFakePID}
	launchd.run = fake.run
	darwinWriteRegistration(t, launchd, darwinTempInstallOptions(t, t.TempDir()))

	if err := launchd.Restart(context.Background()); err != nil {
		t.Fatalf("Restart(): %v", err)
	}

	want := []string{
		// The state of the job is read, the instance is asked to stop
		// and the manager waits for it.
		"print " + launchd.target(),
		"kill SIGTERM " + launchd.target(),
		"print " + launchd.target(),
		// The job is unloaded, so launchd does not restart the
		// instance that is gone.
		"bootout " + launchd.target(),
		// The replacement is registered again and started.
		"print " + launchd.target(),
		"bootstrap " + launchd.domain() + " " + launchd.plistPath(),
		"kickstart " + launchd.target(),
	}
	if !reflect.DeepEqual(fake.commands, want) {
		t.Errorf("the commands of the manager =\n%v\nwant\n%v", fake.commands, want)
	}
	// The running instance is never killed and replaced: "kickstart -k"
	// does exactly that.
	for _, command := range fake.commands {
		if strings.Contains(command, "kickstart -k") {
			t.Errorf("the manager issued %q, want the graceful stop of the instance", command)
		}
	}
}

// TestLaunchdRestartDoesNotStartTheReplacementWhenTheStopFailed checks that
// a restart whose stop failed does not start the replacement.
//
// An instance that did not stop is not covered by a new one: the two would
// compete for the bind address, and the state of the instance that is still
// there would be lost without a report.
func TestLaunchdRestartDoesNotStartTheReplacementWhenTheStopFailed(t *testing.T) {
	launchd := darwinStatusManager(t)
	fake := &darwinFakeLaunchd{
		loaded:  true,
		running: true,
		pid:     darwinFakePID,
		failures: map[string]error{
			"kill": errors.New("service: launchctl kill SIGTERM: exit status 1: the supervisor refuses the signal"),
		},
	}
	launchd.run = fake.run
	darwinWriteRegistration(t, launchd, darwinTempInstallOptions(t, t.TempDir()))

	err := launchd.Restart(context.Background())
	if err == nil {
		t.Fatal("Restart() = nil, want the failure of the stop")
	}
	if !strings.Contains(err.Error(), "the supervisor refuses the signal") {
		t.Errorf("Restart() = %v, want the reason of the failed stop", err)
	}
	// The replacement is not started.
	for _, command := range fake.commands {
		if strings.HasPrefix(command, "bootstrap") || strings.HasPrefix(command, "kickstart") {
			t.Errorf("the manager issued %q, want no start of the replacement", command)
		}
	}
}

// TestLaunchdStopReportsTheOverrunOfTheShutdownBudget checks that a stop
// whose instance did not finish its graceful shutdown is reported.
//
// The instance is unloaded even then, because a process that is still there
// would be restarted by launchd, but the caller never sees a plain success
// for a shutdown that did not complete.
func TestLaunchdStopReportsTheOverrunOfTheShutdownBudget(t *testing.T) {
	launchd := darwinStatusManager(t)
	// The instance ignores the signal: the wait of the manager runs
	// into its budget.
	fake := &darwinFakeLaunchd{loaded: true, running: true, pid: darwinFakePID, keepRunning: true}
	launchd.run = fake.run
	// The wait is shortened, so the test does not wait for the whole
	// budget of the service layer.
	launchd.stopBudget = 250 * time.Millisecond
	darwinWriteRegistration(t, launchd, darwinTempInstallOptions(t, t.TempDir()))

	err := launchd.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop() = nil, want the report of the overrun of the budget")
	}
	message := err.Error()
	// The message names the budget and says that the instance was
	// unloaded anyway.
	for _, want := range []string{
		launchd.shutdownBudget().String(),
		"graceful shutdown",
		"unloaded anyway",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("Stop() = %q, want a message that mentions %q", message, want)
		}
	}
	// The job is unloaded even though the instance did not shut down.
	unloaded := false
	for _, command := range fake.commands {
		if command == "bootout "+launchd.target() {
			unloaded = true
		}
	}
	if !unloaded {
		t.Errorf("the commands of the manager = %v, want the unload of the job", fake.commands)
	}
}

// TestLaunchdRollbackReloadsTheRestoredRegistration checks that a failed
// installation gives the supervisor the state of the registration back.
//
// The instance may have been running before the attempt: a rollback that
// only restores the property list would leave a registration that claims to
// be running while nothing runs.
func TestLaunchdRollbackReloadsTheRestoredRegistration(t *testing.T) {
	const previous = "<plist>the registration of the instance</plist>"
	const changed = "<plist>the registration of the failed installation</plist>"

	// writeRegistration writes the registration of the instance and
	// returns the backup of it.
	writeRegistration := func(t *testing.T, launchd *launchdManager) (*plistBackup, string) {
		t.Helper()
		path := launchd.plistPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create the registration directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(previous), plistMode); err != nil {
			t.Fatalf("write the registration: %v", err)
		}
		backup, err := backupPlist(path)
		if err != nil {
			t.Fatalf("backupPlist(): %v", err)
		}
		// The installation wrote another registration and failed.
		if err := os.WriteFile(path, []byte(changed), plistMode); err != nil {
			t.Fatalf("write the registration of the failed installation: %v", err)
		}
		return backup, path
	}

	t.Run("a registration that ran is loaded again", func(t *testing.T) {
		launchd := darwinStatusManager(t)
		fake := &darwinFakeLaunchd{loaded: true, running: true, pid: darwinFakePID}
		launchd.run = fake.run
		backup, path := writeRegistration(t, launchd)

		cause := errors.New("service: bootstrap " + launchd.target() + ": exit status 1")
		state := launchdLoadState{
			previous:     launchdSupervisorState{loaded: true},
			bootstrapped: true,
			replaced:     true,
		}
		err := launchd.rollback(context.Background(), backup, state, cause)
		if !errors.Is(err, cause) {
			t.Errorf("rollback() = %v, want the cause of the failed installation", err)
		}

		// The property list is restored.
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read the registration: %v", err)
		}
		if string(content) != previous {
			t.Errorf("the restored registration = %q, want %q", content, previous)
		}
		// The registration is loaded again, so the instance runs like it
		// did before the attempt.
		want := []string{
			"bootout " + launchd.target(),
			"bootstrap " + launchd.domain() + " " + path,
			"enable " + launchd.target(),
		}
		if !reflect.DeepEqual(fake.commands, want) {
			t.Errorf("the commands of the manager =\n%v\nwant\n%v", fake.commands, want)
		}
	})

	t.Run("a registration that was disabled stays disabled", func(t *testing.T) {
		launchd := darwinStatusManager(t)
		fake := &darwinFakeLaunchd{loaded: true, running: true, pid: darwinFakePID}
		launchd.run = fake.run
		backup, path := writeRegistration(t, launchd)

		cause := errors.New("service: bootstrap " + launchd.target() + ": exit status 1")
		state := launchdLoadState{
			previous:     launchdSupervisorState{loaded: true, disabled: true},
			bootstrapped: true,
			replaced:     true,
		}
		if err := launchd.rollback(context.Background(), backup, state, cause); !errors.Is(err, cause) {
			t.Errorf("rollback() = %v, want the cause of the failed installation", err)
		}

		want := []string{
			"bootout " + launchd.target(),
			"bootstrap " + launchd.domain() + " " + path,
			"disable " + launchd.target(),
		}
		if !reflect.DeepEqual(fake.commands, want) {
			t.Errorf("the commands of the manager =\n%v\nwant\n%v", fake.commands, want)
		}
	})

	t.Run("a registration that cannot be loaded again is reported", func(t *testing.T) {
		launchd := darwinStatusManager(t)
		fake := &darwinFakeLaunchd{
			loaded:  true,
			running: true,
			pid:     darwinFakePID,
			failures: map[string]error{
				"bootstrap": errors.New("service: launchctl bootstrap: exit status 1: the supervisor refuses the registration"),
			},
		}
		launchd.run = fake.run
		backup, path := writeRegistration(t, launchd)

		cause := errors.New("service: bootstrap " + launchd.target() + ": exit status 1")
		state := launchdLoadState{
			previous:     launchdSupervisorState{loaded: true},
			bootstrapped: true,
			replaced:     true,
		}
		err := launchd.rollback(context.Background(), backup, state, cause)
		if !errors.Is(err, cause) {
			t.Errorf("rollback() = %v, want the cause of the failed installation", err)
		}
		if !strings.Contains(err.Error(), "could not be loaded") {
			t.Errorf("rollback() = %v, want a message that names the failed restore of the registration", err)
		}
		// The property list is restored even then.
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read the registration: %v", err)
		}
		if string(content) != previous {
			t.Errorf("the restored registration = %q, want %q", content, previous)
		}
	})

	t.Run("a registration that did not run stays unloaded", func(t *testing.T) {
		launchd := darwinStatusManager(t)
		// The failed attempt loaded the job: the rollback unloads it
		// again, because it did not run before.
		fake := &darwinFakeLaunchd{loaded: true, running: true, pid: darwinFakePID}
		launchd.run = fake.run
		backup, _ := writeRegistration(t, launchd)

		cause := errors.New("service: bootstrap " + launchd.target() + ": exit status 1")
		// The failed attempt loaded the job: the rollback unloads it
		// again, because it did not run before.
		state := launchdLoadState{
			previous:     launchdSupervisorState{loaded: false},
			bootstrapped: true,
		}
		if err := launchd.rollback(context.Background(), backup, state, cause); !errors.Is(err, cause) {
			t.Errorf("rollback() = %v, want the cause of the failed installation", err)
		}

		want := []string{"bootout " + launchd.target()}
		if !reflect.DeepEqual(fake.commands, want) {
			t.Errorf("the commands of the manager =\n%v\nwant\n%v", fake.commands, want)
		}
	})
}

// TestLaunchdDisabledLabel checks the reading of the database of the
// disabled jobs of launchd.
//
// The output of launchd is not an API: a disabled job is listed as
// "<label> => disabled", and a job that carries further overrides is listed
// as a block that names the disabled flag. Both forms are read.
func TestLaunchdDisabledLabel(t *testing.T) {
	label := launchdLabel()
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{name: "an empty database", output: "", want: false},
		{name: "a disabled job", output: label + " => disabled\n", want: true},
		{name: "another disabled job", output: "com.example.other => disabled\n", want: false},
		{
			name:   "a job with a block of overrides",
			output: label + " => {\n\t\"Disabled\" => true\n\t\"ThrottleInterval\" => 10\n}\n",
			want:   true,
		},
		{
			name:   "a block without the disabled flag",
			output: label + " => {\n\t\"ThrottleInterval\" => 10\n}\n",
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := launchdDisabledLabel(tc.output, label); got != tc.want {
				t.Errorf("launchdDisabledLabel() = %v, want %v", got, tc.want)
			}
		})
	}
}
