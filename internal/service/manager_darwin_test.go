// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
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
		User:       darwinCredentialSentinel,
		UID:        darwinCredentialSentinel,
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
		User:       darwinCredentialSentinel,
		UID:        darwinCredentialSentinel,
	}
}

// TestRenderPlistForTheUserScope checks the property list of a LaunchAgent.
func TestRenderPlistForTheUserScope(t *testing.T) {
	opts := darwinUserOptions(t)
	rendered, err := renderPlist(opts)
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
	rendered, err := renderPlist(opts)
	if err != nil {
		t.Fatalf("renderPlist(): %v", err)
	}

	registration, err := parsePlist([]byte(rendered))
	if err != nil {
		t.Fatalf("parsePlist(): %v", err)
	}

	// The daemon runs as root of the machine.
	if registration.UserName != "root" {
		t.Errorf("UserName = %q, want %q", registration.UserName, "root")
	}
	if registration.GroupName != "wheel" {
		t.Errorf("GroupName = %q, want %q", registration.GroupName, "wheel")
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
	for _, scope := range []Scope{ScopeUser, ScopeSystem} {
		t.Run(string(scope), func(t *testing.T) {
			var opts InstallOptions
			if scope == ScopeUser {
				opts = darwinUserOptions(t)
			} else {
				opts = darwinSystemOptions(t)
			}
			// Every string field of the options that could carry a
			// credential carries the sentinel.
			setCredentialFields(&opts, darwinCredentialSentinel)

			rendered, err := renderPlist(opts)
			if err != nil {
				t.Fatalf("renderPlist(): %v", err)
			}
			if strings.Contains(rendered, darwinCredentialSentinel) {
				t.Errorf("the property list carries the credential sentinel:\n%s", rendered)
			}

			// The registration holds the known keys only: there is no
			// place a credential could hide in.
			want := []string{"Label", "ProgramArguments", "WorkingDirectory", "RunAtLoad", "KeepAlive", "ProcessType", "EnvironmentVariables"}
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
				"root":              true,
				"wheel":             true,
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
	rendered, err := renderPlist(opts)
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
	rendered, err := renderPlist(opts)
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

// TestPlutilAcceptsTheRenderedPlist checks the property list with the
// property list tool of macOS.
func TestPlutilAcceptsTheRenderedPlist(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skipf("plutil is not available: %v", err)
	}

	for _, scope := range []Scope{ScopeUser, ScopeSystem} {
		t.Run(string(scope), func(t *testing.T) {
			var opts InstallOptions
			if scope == ScopeUser {
				opts = darwinUserOptions(t)
			} else {
				opts = darwinSystemOptions(t)
			}
			rendered, err := renderPlist(opts)
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
	rendered, err := renderPlist(opts)
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

// TestLaunchdLifecycle runs the whole lifecycle of an instance against
// launchd.
//
// The test uses the property list of the user of the test: a registration
// that was there before the test is restored at the end of it.
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

	status := darwinWaitForState(t, launchd, StateRunning)
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

// darwinWaitForState waits until the instance reaches a state.
func darwinWaitForState(t *testing.T, manager *launchdManager, want State) Status {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var status Status
	for {
		var err error
		status, err = manager.Status(context.Background())
		if err != nil {
			t.Fatalf("Status(): %v", err)
		}
		if status.State == want {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("the state = %q, want %q (%s)", status.State, want, status.Detail)
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
