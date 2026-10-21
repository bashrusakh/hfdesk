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

	root := t.TempDir()
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
// commands that grant the access, the administrator runs them.
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

	// The ownership of the cache tree is unchanged.
	if owner, group := darwinOwner(t, cache); owner != beforeUID || group != beforeGID {
		t.Errorf("the owner of the cache directory %q = %d:%d, want %d:%d", cache, owner, group, beforeUID, beforeGID)
	}
	// The refused installation creates no directory of the instance.
	if _, err := os.Stat(stateDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused installation created the state directory %q: %v", stateDir, err)
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
