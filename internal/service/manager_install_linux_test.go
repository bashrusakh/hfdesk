// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/godbus/dbus/v5"
)

// TestSystemdUserIntegrationCycle runs a full lifecycle of a user scope
// installation against the systemd of the machine.
//
// The test skips itself when the environment has no usable user systemd
// session. It never runs as root: a root process would install a unit of
// the machine instead of a unit of the session of a user.
//
// The registered program is a wrapper around this test binary, so the
// instance the supervisor really starts serves the HFDesk readiness probe
// and exposes the canonical "service run" argv. That is what lets the cycle
// cover an update of a RUNNING instance on a real supervisor: the restart
// proof (own-instance readiness, typed argv, a new MainPID) runs against the
// real systemd, and a plain foreground script could never pass it.
//
// The cycle covers, on the real supervisor:
//   - a first installation: written, enabled for the boot of the session,
//     never started;
//   - an update of a stopped instance: stays stopped, the new program is
//     registered, and the printed "is-enabled" reads "enabled" both before
//     and after the repeat installation — the preservation is
//     non-interference on the supervisor side, so the printed state before
//     and after is the observable fact;
//   - an update of a running instance: succeeds only when the restarted
//     instance serves as its own instance on the new program.
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
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() = %v, want nil", err)
	}
	// Three programs, one wrapper each: the update names a new executable
	// path while the process behind it stays the probe-answering test
	// binary.
	writeProgram := func(name string) string {
		t.Helper()
		path := filepath.Join(root, "bin", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\n" +
			"export HFDESK_TEST_SYSTEMD_RUNTIME_PROCESS_FIXTURE=1\n" +
			"exec \"" + testBinary + "\" '-test.run=^TestSystemdRuntimeListenProcessFixture$' \"$@\"\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	programA := writeProgram("hfdesk-a")
	programB := writeProgram("hfdesk-b")
	programC := writeProgram("hfdesk-c")

	opts := InstallOptions{
		Scope:      ScopeUser,
		Executable: programA,
		Listen:     "127.0.0.1:18099",
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var"),
			LogDir:     filepath.Join(root, "var", "logs"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		// Uninstall stops a still running instance ("disable --now"),
		// removes the registration only and leaves the data behind.
		_ = m.Uninstall(cleanupCtx)
		_ = os.Remove(unitPath)
		_, _ = m.runSystemctl(cleanupCtx, "daemon-reload")
	})

	// The printed enablement of the real supervisor: an installation
	// preserves the enablement by non-interference, so "enabled" before
	// and after a repeat installation is the observable fact — the repeat
	// never runs a second "enable" itself.
	assertEnabledForBoot := func(stage string) {
		t.Helper()
		out, err := m.runSystemctl(ctx, "is-enabled", unitName)
		if err != nil {
			t.Fatalf("%s: is-enabled failed: %v", stage, err)
		}
		if got := strings.TrimSpace(out); got != "enabled" {
			t.Errorf("%s: the printed is-enabled = %q, want %q", stage, got, "enabled")
		}
	}

	// Leg 1: the first installation registers programA, enables the unit
	// for the boot of the session and never starts the instance.
	if _, err := m.Install(ctx, opts); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}
	assertEnabledForBoot("after the first installation")

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

	// Leg 2: an update of the stopped instance registers the new program,
	// never starts the instance, and leaves the printed enablement exactly
	// as it was.
	assertEnabledForBoot("before the repeat installation")
	updatedWhileStopped := opts
	updatedWhileStopped.Executable = programB
	if _, err := m.Install(ctx, updatedWhileStopped); err != nil {
		t.Fatalf("Install() over the stopped registration = %v, want nil", err)
	}
	assertEnabledForBoot("after the repeat installation")

	status, err = m.Status(ctx)
	if err != nil {
		t.Fatalf("Status() after the repeat installation = %v, want nil", err)
	}
	if status.State != StateStopped {
		t.Errorf("the state after the update of the stopped instance = %q, want %q", status.State, StateStopped)
	}
	if status.Executable != programB {
		t.Errorf("the registered executable = %q, want the updated program %q", status.Executable, programB)
	}
	updatedContent, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updatedContent), programB) {
		t.Errorf("the unit file does not register the new program %q:\n%s", programB, updatedContent)
	}
	if string(updatedContent) == string(content) {
		t.Errorf("the unit file was not rewritten for the new program:\n%s", updatedContent)
	}

	// Leg 3: the registered instance really starts and serves.
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	waitForState(t, ctx, m, StateRunning)
	// Starting a running service is a stable success.
	if err := m.Start(ctx); err != nil {
		t.Errorf("the second Start() = %v, want nil", err)
	}

	// Leg 4: an update of the RUNNING instance succeeds only on the real
	// restart proof — the supervisor reloads the new command, the restart
	// is verified through the typed argv and the readiness probe of the
	// own instance, and the MainPID after the restart is a new process.
	// A failed proof would roll the update back and fail this test.
	updatedWhileRunning := opts
	updatedWhileRunning.Executable = programC
	if _, err := m.Install(ctx, updatedWhileRunning); err != nil {
		t.Fatalf("Install() over the running registration = %v, want the verified restart onto %q", err, programC)
	}
	status, err = m.Status(ctx)
	if err != nil {
		t.Fatalf("Status() after the update of the running instance = %v, want nil", err)
	}
	if status.State != StateRunning {
		t.Errorf("the state after the update of the running instance = %q, want %q", status.State, StateRunning)
	}
	if status.Executable != programC {
		t.Errorf("the running instance registers %q, want the new program %q", status.Executable, programC)
	}
	runningContent, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runningContent), programC) {
		t.Errorf("the unit file does not register the new program %q:\n%s", programC, runningContent)
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

// TestSystemdInstallUsesLoadedVendorFragmentAndRetainsItsPaths checks that a
// loaded fragment HFDesk did not write under the unit name is refused as a
// foreign registration instead of being adopted (detector C): the refusal
// happens before any mutation — no unit file, no directory of the instance,
// no supervisor change — and the message states the fragment systemd loads
// and the one next action of the ownership contract.
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
		properties := systemdTestPropertiesFromUnit(vendorUnit)
		properties["FragmentPath"] = dbus.MakeVariant(vendorPath)
		return properties, nil
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", previous.Executable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

	_, err = manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, systemdTestExecutable(t)))
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the foreign fragment")
	}
	if !strings.Contains(err.Error(), vendorPath) || !strings.Contains(err.Error(), unitPath) {
		t.Errorf("Install() = %q, want the refusal to name the loaded fragment and our unit path", err)
	}
	assertSingleDropInAction(t, err, unitPath)
	if _, err := os.Stat(unitPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused installation wrote an override at %q (stat error: %v)", unitPath, err)
	}
	// The refusal happens before any directory of the instance is created.
	for _, dir := range instanceDirs(previous) {
		if _, err := os.Stat(dir.path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused installation created the %s %q: %v", dir.name, dir.path, err)
		}
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

	if _, err := manager.Install(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "no readable fragment") {
		t.Fatalf("Install() = %v, want the refusal of a loaded registration without a readable fragment", err)
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

// systemdExistingInstallation writes the unit file of an existing user scope
// registration and returns the manager, the path and the content of the unit.
//
// The content is the canonical rendering: it is what an installation of the
// current version leaves on disk, so detector A accepts the base file of
// every update fixture built here. The configuration directory of the user
// is a temporary directory, so the test never touches the registration of
// the user who runs the test.
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
	facts, err := canonicalUnitForInstall(opts, nil)
	if err != nil {
		t.Fatalf("canonicalUnitForInstall() = %v, want nil", err)
	}
	unit, err := renderCanonicalUnit(facts)
	if err != nil {
		t.Fatalf("renderCanonicalUnit() = %v, want nil", err)
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
	// The supervisor reports a different MainPID after the restart than
	// before it: the restart proof (D-11) requires the post-restart process
	// to be a new one, so the snapshot taken before the update names another
	// process than the fixture that serves after the restart.
	systemdRuntimePropertyReader(t, m, os.Getpid(), process.Process.Pid, restartCount)
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
	if unit.Program != newExecutable {
		t.Errorf("the registered executable is %q, want %q", unit.Program, newExecutable)
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
	if got := parseUnit(string(content)).Program; got != newExecutable {
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

// TestSystemdReinstallPreservesUnrelatedUnitSettings checks the ownership
// split of the registration: settings that are not the canonical base file
// live in the administrator's drop-in beside the unit and survive a
// reinstall byte for byte, while the base file the installation rewrites is
// exactly the canonical rendering of the merged facts — only the executable
// of the request differs from the previous bytes.
//
// The effective values the drop-in provides — an independent working
// directory, extra environment, an environment file, a restart policy and
// an ExecStartPre hook — belong to the preserved fields: no request flag
// named them, so the typed configuration must read back unchanged across
// the write.
func TestSystemdReinstallPreservesUnrelatedUnitSettings(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)

	// The administrator's drop-in beside the unit: HFDesk never writes it.
	dropInPath := unitDropInPath(unitPath)
	dropIn := "[Service]\n" +
		"WorkingDirectory=/operator/launch-dir\n" +
		"Environment=HF_HUB_CACHE=/operator/hub\n" +
		"EnvironmentFile=-/etc/hfdesk/service.env\n" +
		"Restart=always\n" +
		"ExecStartPre=/usr/bin/test -d /operator/data\n"
	if err := os.MkdirAll(filepath.Dir(dropInPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dropInPath, []byte(dropIn), 0o644); err != nil {
		t.Fatal(err)
	}
	// The typed configuration is what the supervisor loads from the base
	// file plus the drop-in layer, and it is read the same way before and
	// after the write: the drop-in layer itself never changes.
	home, err := unitHomeDir(previous)
	if err != nil {
		t.Fatal(err)
	}
	loadedEnvironment := []string{"HF_HUB_CACHE=/operator/hub", "HOME=" + home}
	loadedEnvironmentFiles := []systemdEnvironmentFile{{Path: "/etc/hfdesk/service.env", IgnoreErrors: true}}
	m.readServiceProperties = func(_ context.Context, name string) (map[string]dbus.Variant, error) {
		if name != unitName {
			return nil, fmt.Errorf("unexpected unit %q", name)
		}
		content, err := os.ReadFile(os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT"))
		if err != nil {
			return nil, err
		}
		properties := systemdTestPropertiesFromUnit(string(content))
		properties["WorkingDirectory"] = dbus.MakeVariant("/operator/launch-dir")
		properties["Environment"] = dbus.MakeVariant(loadedEnvironment)
		properties["EnvironmentFiles"] = dbus.MakeVariant(loadedEnvironmentFiles)
		properties["DropInPaths"] = dbus.MakeVariant([]string{dropInPath})
		return properties, nil
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

	effective, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}
	want := previous
	want.Executable = newExecutable
	if !reflect.DeepEqual(effective, want) {
		t.Errorf("Install() returned\n%+v\nwant the preserved registration\n%+v", effective, want)
	}

	// The drop-in survived byte for byte.
	if got, readErr := os.ReadFile(dropInPath); readErr != nil || string(got) != dropIn {
		t.Errorf("the drop-in changed (error %v):\n%s\nwant\n%s", readErr, got, dropIn)
	}
	// The base file is exactly the canonical rendering of the merged
	// facts: the semantic diff of the write is the executable alone.
	content, readErr := os.ReadFile(unitPath)
	if readErr != nil {
		t.Fatalf("read the updated unit: %v", readErr)
	}
	facts, factErr := canonicalUnitForInstall(want, nil)
	if factErr != nil {
		t.Fatalf("canonicalUnitForInstall() = %v, want nil", factErr)
	}
	wantContent, renderErr := renderCanonicalUnit(facts)
	if renderErr != nil {
		t.Fatalf("renderCanonicalUnit() = %v, want nil", renderErr)
	}
	if string(content) != wantContent {
		t.Errorf("the updated base unit =\n%s\nwant the canonical rendering\n%s", content, wantContent)
	}
	if string(content) == previousUnit {
		t.Error("the base unit was not rewritten for the new executable")
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
	if got := parseUnit(string(content)).Program; got != newExecutable {
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
	if registered.Program != fixture.updated.Executable || registered.flagValue("--config") != fixture.previous.Paths.ConfigFile {
		t.Errorf("updated registration = %+v; config path with spaces/quotes was not preserved", registered)
	}
	// The base file is the canonical rendering of the merged facts: the
	// typed layer kept the extra environment and the environment file
	// across the write (V1 proves it), and the written bytes changed only
	// where the request named the executable.
	facts, factErr := canonicalUnitForInstall(fixture.updated, nil)
	if factErr != nil {
		t.Fatalf("canonicalUnitForInstall() = %v, want nil", factErr)
	}
	wantContent, renderErr := renderCanonicalUnit(facts)
	if renderErr != nil {
		t.Fatalf("renderCanonicalUnit() = %v, want nil", renderErr)
	}
	if string(content) != wantContent {
		t.Errorf("updated unit =\n%s\nwant the canonical rendering\n%s", content, wantContent)
	}
	if calls := systemdSupervisorCalls(t, fixture.logPath); systemdSupervisorCallsTo(calls, "restart", unitName) != 0 {
		t.Errorf("the stopped unit was restarted:\n%s", strings.Join(calls, "\n"))
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

// TestSystemdReinstallKeepsTheAutostartStateOfTheUnit checks that a repeat
// installation never touches the autostart state it finds: no `enable` and
// no `enable --runtime` runs, whatever textual state the supervisor reports.
//
// A first installation enables the registration it writes (see
// TestSystemdFreshInstallNeverStartsTheInstance). A repeat installation
// preserves the enablement by non-interference instead: a unit an
// administrator disabled stays disabled, a unit enabled for this boot only
// ("systemctl enable --runtime") never becomes permanent, and a state that
// exits with a zero status without being an enablement ("static",
// "indirect", "alias", "linked") is not turned into one — the printed state
// decides, not the status. A masked unit is refused instead of being
// replaced, see TestSystemdInstallRefusesAMaskedUnit. The supervisor is
// reloaded in every case, so it picks the new unit file up, and the
// enablement is read before the unit file is written, so the state the
// installation preserves is the state of the unit it replaces.
func TestSystemdReinstallKeepsTheAutostartStateOfTheUnit(t *testing.T) {
	cases := []struct {
		name string
		// state is the enablement the supervisor reports for the unit
		// that is installed already.
		state string
	}{
		{"a unit that is enabled", "enabled"},
		{"a unit that is enabled for this boot only", "enabled-runtime"},
		{"a unit that is disabled", "disabled"},
		{"a unit that is static", "static"},
		{"a unit that is indirect", "indirect"},
		{"a unit that is an alias", "alias"},
		{"a unit that is linked", "linked"},
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
			if got := systemdSupervisorCallsTo(calls, "enable", unitName); got != 0 {
				t.Errorf("the unit was enabled permanently %d times, want none: a repeat installation never runs enable for a unit that was %q:\n%s", got, tc.state, strings.Join(calls, "\n"))
			}
			if got := systemdSupervisorCallsTo(calls, "enable", "--runtime", unitName); got != 0 {
				t.Errorf("the unit was enabled at runtime %d times, want none: a runtime-only enablement of a unit that was %q must stay untouched:\n%s", got, tc.state, strings.Join(calls, "\n"))
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

// TestSystemdInstallHandlesTheHandEditedBaseFileOnBothSides pins both sides
// of detector A at the installation level: a base file carrying any line
// HFDesk did not render is refused with zero supervisor mutations, while an
// untouched canonical base file is updated with the named flags only — the
// written bytes are exactly the canonical rendering of the merged facts, so
// the semantic diff of the update is the executable and the named options.
func TestSystemdInstallHandlesTheHandEditedBaseFileOnBothSides(t *testing.T) {
	t.Run("a hand-edited base file is refused with zero mutations", func(t *testing.T) {
		logPath := systemdRecordingSupervisor(t)
		root := t.TempDir()
		oldExecutable := systemdTestExecutable(t)
		newExecutable := systemdTestExecutable(t)
		previous := systemdExistingOptions(t, oldExecutable, root)
		m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
		edited := strings.Replace(previousUnit, "Type=simple\n", "Type=simple\n# administrator note\n", 1)
		if edited == previousUnit {
			t.Fatal("the canonical fixture no longer holds Type=simple")
		}
		for _, name := range []string{unitPath, os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")} {
			if err := os.WriteFile(name, []byte(edited), 0o644); err != nil {
				t.Fatalf("write the hand-edited unit %q: %v", name, err)
			}
		}
		t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

		_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
		assertSingleDropInAction(t, err, unitPath)
		if !strings.Contains(err.Error(), "is not the unit HFDesk generates") {
			t.Errorf("Install() = %q, want the cause to name the non-generated unit file", err)
		}
		content, readErr := os.ReadFile(unitPath)
		if readErr != nil || string(content) != edited {
			t.Errorf("the refused update changed the unit (read error %v):\n%s", readErr, content)
		}
		calls := systemdSupervisorCalls(t, logPath)
		mutating := [][]string{
			{"daemon-reload"},
			{"enable", unitName},
			{"enable", "--runtime", unitName},
			{"restart", unitName},
			{"stop", unitName},
			{"start", unitName},
			{"disable", "--now", unitName},
		}
		for _, command := range mutating {
			if got := systemdSupervisorCallsTo(calls, command...); got != 0 {
				t.Errorf("the refused installation called %q %d times:\n%s", strings.Join(command, " "), got, strings.Join(calls, "\n"))
			}
		}
	})

	t.Run("a canonical update changes only the named fields", func(t *testing.T) {
		logPath := systemdRecordingSupervisor(t)
		root := t.TempDir()
		oldExecutable := systemdTestExecutable(t)
		newExecutable := systemdTestExecutable(t)
		previous := systemdExistingOptions(t, oldExecutable, root)
		m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
		t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

		requested := DefaultInstallOptions(ScopeUser, newExecutable)
		requested.Listen = "127.0.0.1:19233"
		requested.Explicit.Listen = true
		effective, err := m.Install(context.Background(), requested)
		if err != nil {
			t.Fatalf("Install() = %v, want the canonical update accepted", err)
		}
		want := previous
		want.Executable = newExecutable
		want.Listen = requested.Listen
		want.Explicit = requested.Explicit
		if !reflect.DeepEqual(effective, want) {
			t.Errorf("Install() returned\n%+v\nwant the merged registration\n%+v", effective, want)
		}
		facts, factErr := canonicalUnitForInstall(want, nil)
		if factErr != nil {
			t.Fatalf("canonicalUnitForInstall() = %v, want nil", factErr)
		}
		wantContent, renderErr := renderCanonicalUnit(facts)
		if renderErr != nil {
			t.Fatalf("renderCanonicalUnit() = %v, want nil", renderErr)
		}
		content, readErr := os.ReadFile(unitPath)
		if readErr != nil {
			t.Fatalf("read the updated unit: %v", readErr)
		}
		if string(content) != wantContent {
			t.Errorf("the updated unit =\n%s\nwant the canonical rendering of the merged facts\n%s", content, wantContent)
		}
		if string(content) == previousUnit {
			t.Error("the unit file was not rewritten for the new executable")
		}
		if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 0 {
			t.Errorf("a stopped instance was restarted %d times", got)
		}
	})
}

// TestSystemdUpdateThatKeepsTheMainPIDFailsVerification pins the own-PID
// fact of the restart proof (D-11): a restart after which the supervisor
// reports the same MainPID did not start a new instance, so the update
// fails verification even though state, program, argv and readiness all
// looked healthy — and the previous registration is restored, restarted and
// honestly reported as started again because that restore is confirmed.
func TestSystemdUpdateThatKeepsTheMainPIDFailsVerification(t *testing.T) {
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
	// The supervisor reports the same process before and after the restart:
	// the update therefore passes every other check and fails on D-11 alone.
	systemdRuntimePropertyReader(t, m, process.Process.Pid, process.Process.Pid, restartCount)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_COUNT", restartCount)
	previousProbe := serviceReadinessProbe
	serviceReadinessProbe = probeServiceReady
	t.Cleanup(func() { serviceReadinessProbe = previousProbe })
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE_AFTER_SECOND_RESTART", oldExecutable)

	_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err == nil {
		t.Fatal("Install() = nil, want the own-PID verification failure")
	}
	for _, want := range []string{
		"kept the previous MainPID",
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
	// The update restart and the restart that restores the previous
	// instance.
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 2 {
		t.Errorf("the unit was restarted %d times, want the update attempt and the restore:\n%s", got, strings.Join(calls, "\n"))
	}
}

// TestSystemdUpdateRefusesACommandTheReloadDoesNotLoad pins V1 at the
// installation level: after daemon-reload the typed ExecStart the supervisor
// reports is NOT the canonical command that was just written, so
// verifyUpdatedUnit fails, the previous unit is restored and the error says
// both the honest cause and what the rollback proved — never a restarted
// instance a stopped update never attempted.
//
// Every other loaded field of the drift is preserved, so the case fails if
// the loaded-vs-canonical command check were removed: the stopped update
// would then succeed end to end.
func TestSystemdUpdateRefusesACommandTheReloadDoesNotLoad(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)

	home, err := unitHomeDir(previous)
	if err != nil {
		t.Fatalf("unitHomeDir() = %v, want nil", err)
	}
	// The snapshot the update reads before it writes: the typed
	// configuration of the canonical file on disk.
	oldProperties := systemdTestPropertiesFromUnit(previousUnit)
	// The answer after daemon-reload: the same launch configuration with
	// the pre-update command still loaded — the write did not become the
	// effective ExecStart.
	staleArgv := append([]string{oldExecutable}, RunArgs(previous)...)
	driftedProperties := systemdTypedProperties(oldExecutable, staleArgv, []string{"HOME=" + home}, previous.Paths.StateDir, "", "", nil)
	// Read sequence by reload count: before the write, after the write,
	// and after the rollback reload, where the supervisor must read back
	// the snapshot for the rollback to be reported as restored.
	setSystemdTypedPropertySequence(t, m, oldProperties, driftedProperties, oldProperties)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

	requested := DefaultInstallOptions(ScopeUser, newExecutable)
	_, err = m.Install(context.Background(), requested)
	if err == nil {
		t.Fatal("Install() = nil, want the V1 failure of a loaded command that is not the written canonical command")
	}
	for _, want := range []string{
		"did not load the canonical command that was written",
		"the previous registration and captured loaded command/launch settings were restored",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install() = %q, want an error that mentions %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "started again") {
		t.Errorf("Install() = %q, must not claim a restarted instance of a stopped update", err)
	}

	content, readErr := os.ReadFile(unitPath)
	if readErr != nil {
		t.Fatalf("read the unit file: %v", readErr)
	}
	if string(content) != previousUnit {
		t.Errorf("the unit file was not restored:\n%s\nwant\n%s", content, previousUnit)
	}
	calls := systemdSupervisorCalls(t, logPath)
	for _, command := range [][]string{{"restart", unitName}, {"start", unitName}} {
		if got := systemdSupervisorCallsTo(calls, command...); got != 0 {
			t.Errorf("the failed update called %q %d times:\n%s", strings.Join(command, " "), got, strings.Join(calls, "\n"))
		}
	}
}

// systemdLoadedDriftFields is the one field a case of
// TestSystemdUpdateVerifiesThePreservedAndRequestedFields lets drift after
// the write; every other loaded field stays at its pre-update value.
type systemdLoadedDriftFields struct {
	workingDirectory    string
	user                string
	group               string
	supplementaryGroups []string
	environmentFiles    []systemdEnvironmentFile
}

// TestSystemdUpdateVerifiesThePreservedAndRequestedFields pins every
// preservation and request branch of verifyUpdatedUnit at the installation
// level: the typed configuration reported after daemon-reload drifts in
// exactly one field, so verification must fail with the honest cause of that
// branch and the rollback must restore the previous unit and report it.
//
// Each case discriminates: the drift is the only difference from the
// snapshot, so a verification that dropped the branch lets this stopped
// update succeed, and the test fails on the missing error.
func TestSystemdUpdateVerifiesThePreservedAndRequestedFields(t *testing.T) {
	cases := []struct {
		name    string
		cause   string
		request func(*InstallOptions)
		drift   func(*systemdLoadedDriftFields)
	}{
		{
			name:  "the loaded working directory must be preserved",
			cause: "systemd did not preserve the loaded working directory",
			drift: func(d *systemdLoadedDriftFields) {
				d.workingDirectory = "/operator/drifted-working-directory"
			},
		},
		{
			name:  "the loaded service identity must be preserved",
			cause: "systemd did not preserve the loaded service identity",
			drift: func(d *systemdLoadedDriftFields) {
				d.user, d.group = "operator", "operator"
			},
		},
		{
			name:  "the loaded supplementary groups must be preserved",
			cause: "systemd did not preserve the loaded supplementary groups",
			drift: func(d *systemdLoadedDriftFields) {
				d.supplementaryGroups = []string{"operator"}
			},
		},
		{
			name:  "the loaded environment files must be preserved",
			cause: "systemd did not preserve the loaded environment and environment files",
			drift: func(d *systemdLoadedDriftFields) {
				d.environmentFiles = []systemdEnvironmentFile{{Path: "/operator/drifted.env"}}
			},
		},
		{
			name:  "the requested working directory must be loaded",
			cause: "systemd did not load the requested working directory",
			request: func(opts *InstallOptions) {
				opts.Paths.StateDir = filepath.Join(filepath.Dir(opts.Paths.StateDir), "hfdesk-renamed-state")
				opts.Explicit.StateDir = true
			},
			drift: func(d *systemdLoadedDriftFields) {
				d.workingDirectory = "/operator/another-working-directory"
			},
		},
		{
			name:  "the requested service identity must be loaded",
			cause: "systemd did not load the requested service identity",
			request: func(opts *InstallOptions) {
				opts.ServiceUser = "hfdesk-svc"
				opts.Explicit.ServiceUser = true
			},
			drift: func(d *systemdLoadedDriftFields) {
				d.user, d.group = "operator", "operator"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logPath := systemdRecordingSupervisor(t)
			root := t.TempDir()
			oldExecutable := systemdTestExecutable(t)
			newExecutable := systemdTestExecutable(t)
			previous := systemdExistingOptions(t, oldExecutable, root)
			m, unitPath, previousUnit := systemdExistingInstallation(t, previous)

			requested := DefaultInstallOptions(ScopeUser, newExecutable)
			if tc.request != nil {
				tc.request(&requested)
			}
			// The write is the canonical rendering of the merged facts —
			// the same merge the installation performs over the canonical
			// registration on disk.
			written := mergeInstallOptions(requested, previous, true)
			writtenArgv := append([]string{written.Executable}, RunArgs(written)...)
			home, homeErr := unitHomeDir(previous)
			if homeErr != nil {
				t.Fatalf("unitHomeDir() = %v, want nil", homeErr)
			}

			oldProperties := systemdTestPropertiesFromUnit(previousUnit)
			drift := systemdLoadedDriftFields{workingDirectory: previous.Paths.StateDir}
			tc.drift(&drift)
			driftedProperties := systemdTypedProperties(written.Executable, writtenArgv,
				[]string{"HOME=" + home}, drift.workingDirectory, drift.user, drift.group, drift.environmentFiles)
			driftedProperties["SupplementaryGroups"] = dbus.MakeVariant(drift.supplementaryGroups)
			setSystemdTypedPropertySequence(t, m, oldProperties, driftedProperties, oldProperties)
			t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

			_, err := m.Install(context.Background(), requested)
			if err == nil {
				t.Fatalf("Install() = nil, want the verification failure %q", tc.cause)
			}
			for _, want := range []string{
				tc.cause,
				"the previous registration and captured loaded command/launch settings were restored",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Install() = %q, want an error that mentions %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "started again") {
				t.Errorf("Install() = %q, must not claim a restarted instance of a stopped update", err)
			}

			content, readErr := os.ReadFile(unitPath)
			if readErr != nil {
				t.Fatalf("read the unit file: %v", readErr)
			}
			if string(content) != previousUnit {
				t.Errorf("the unit file was not restored:\n%s\nwant\n%s", content, previousUnit)
			}
			if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 0 {
				t.Errorf("the failed update restarted the stopped unit %d times:\n%s", got, strings.Join(systemdSupervisorCalls(t, logPath), "\n"))
			}
		})
	}
}

// TestSystemdUpdateFailsClosedOnAnUnconfirmableSupplementaryGroupsSnapshot
// pins the fail-closed rule of the supplementary-groups preservation: the
// snapshot established the groups (a real list or the established empty
// one), the typed answer after the write omits the property, and the
// verification must fail with the established cause and the honest rollback
// report instead of reading the gap as "no drift to check". When the
// snapshot itself never carried the optional, no strictness is added: the
// absent optional stays absent and the update succeeds.
//
// Every case discriminates: with the pre-fix condition (both answers known)
// the incomplete answer skipped the check and rows 1 and 2 succeeded end to
// end, and a symmetric "both answers must be present" rule refused row 3.
func TestSystemdUpdateFailsClosedOnAnUnconfirmableSupplementaryGroupsSnapshot(t *testing.T) {
	confirmedGroups := []string{"users", "video"}
	cases := []struct {
		name          string
		snapshot      func(map[string]dbus.Variant)
		omitAnswer    bool
		confirmAnswer func(map[string]dbus.Variant)
		cause         string // empty: the update must succeed
	}{
		{
			name: "the snapshot expected groups the incomplete answer cannot confirm",
			snapshot: func(properties map[string]dbus.Variant) {
				properties["SupplementaryGroups"] = dbus.MakeVariant([]string{"operator"})
			},
			omitAnswer: true,
			cause:      "systemd did not confirm the loaded supplementary groups after the update",
		},
		{
			name:       "the snapshot established an empty list the incomplete answer cannot confirm",
			omitAnswer: true,
			cause:      "systemd did not confirm the loaded supplementary groups after the update",
		},
		{
			name: "an absent snapshot optional stays absent",
			snapshot: func(properties map[string]dbus.Variant) {
				delete(properties, "SupplementaryGroups")
			},
		},
		{
			name: "the answer confirms the populated groups the snapshot established",
			snapshot: func(properties map[string]dbus.Variant) {
				properties["SupplementaryGroups"] = dbus.MakeVariant(confirmedGroups)
			},
			confirmAnswer: func(properties map[string]dbus.Variant) {
				properties["SupplementaryGroups"] = dbus.MakeVariant(confirmedGroups)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logPath := systemdRecordingSupervisor(t)
			root := t.TempDir()
			oldExecutable := systemdTestExecutable(t)
			newExecutable := systemdTestExecutable(t)
			previous := systemdExistingOptions(t, oldExecutable, root)
			m, unitPath, previousUnit := systemdExistingInstallation(t, previous)

			requested := DefaultInstallOptions(ScopeUser, newExecutable)
			written := mergeInstallOptions(requested, previous, true)
			writtenArgv := append([]string{written.Executable}, RunArgs(written)...)
			home, homeErr := unitHomeDir(previous)
			if homeErr != nil {
				t.Fatalf("unitHomeDir() = %v, want nil", homeErr)
			}

			oldProperties := systemdTestPropertiesFromUnit(previousUnit)
			if tc.snapshot != nil {
				tc.snapshot(oldProperties)
			}
			answeredProperties := systemdTypedProperties(written.Executable, writtenArgv,
				[]string{"HOME=" + home}, previous.Paths.StateDir, "", "", nil)
			if tc.omitAnswer {
				delete(answeredProperties, "SupplementaryGroups")
			}
			if tc.confirmAnswer != nil {
				tc.confirmAnswer(answeredProperties)
			}
			setSystemdTypedPropertySequence(t, m, oldProperties, answeredProperties, oldProperties)
			t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

			_, err := m.Install(context.Background(), requested)
			if tc.cause == "" {
				if err != nil {
					t.Fatalf("Install() = %v, want the verification to accept the answer that shows no drift", err)
				}
				content, readErr := os.ReadFile(unitPath)
				if readErr != nil || string(content) == previousUnit || parseUnit(string(content)).Program != newExecutable {
					t.Errorf("the updated unit (error %v) =\n%s\nwant the canonical write of %s", readErr, content, newExecutable)
				}
				if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 0 {
					t.Errorf("the stopped unit was restarted %d times:\n%s", got, strings.Join(systemdSupervisorCalls(t, logPath), "\n"))
				}
				return
			}
			if err == nil {
				t.Fatalf("Install() = nil, want the verification failure %q", tc.cause)
			}
			for _, want := range []string{
				tc.cause,
				"the previous registration and captured loaded command/launch settings were restored",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Install() = %q, want an error that mentions %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "started again") {
				t.Errorf("Install() = %q, must not claim a restarted instance of a stopped update", err)
			}
			content, readErr := os.ReadFile(unitPath)
			if readErr != nil || string(content) != previousUnit {
				t.Errorf("the unit file was not restored (error %v):\n%s\nwant\n%s", readErr, content, previousUnit)
			}
			if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 0 {
				t.Errorf("the failed update restarted the stopped unit %d times:\n%s", got, strings.Join(systemdSupervisorCalls(t, logPath), "\n"))
			}
		})
	}
}

// TestSystemdInstallRefusesTheRegistrationWhoseOwnFragmentVanished pins the
// missing-file branch of detector C: systemd reports OUR unit path as the
// FragmentPath it loaded, but the file was gone when the installation read
// it — the registration changed underneath the update, so the update is
// refused with that exact cause and before any mutation. Falling through to
// a first installation would write the unit file this test asserts away.
func TestSystemdInstallRefusesTheRegistrationWhoseOwnFragmentVanished(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	requested := systemdTempInstallOptions(t, root)
	m := &systemdManager{scope: ScopeUser}
	unitPath, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	if _, err := os.Stat(unitPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the fixture starts with a unit file at %q: %v", unitPath, err)
	}
	// The supervisor still holds a loaded registration whose fragment is
	// our own unit path, while the file itself is absent.
	requestedArgv := append([]string{requested.Executable}, RunArgs(requested)...)
	m.readServiceProperties = func(context.Context, string) (map[string]dbus.Variant, error) {
		properties := systemdTypedProperties(requested.Executable, requestedArgv, nil, requested.Paths.StateDir, "", "", nil)
		properties["FragmentPath"] = dbus.MakeVariant(unitPath)
		return properties, nil
	}
	// A non-empty executable makes the fake report the unit as loaded.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", requested.Executable)

	_, err = m.Install(context.Background(), requested)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of a loaded registration whose own file vanished")
	}
	for _, want := range []string{
		unitPath,
		"but the file was absent when the update read it",
		`Run "hfdesk service install" again`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install() = %q, want an error that mentions %q", err, want)
		}
	}
	if strings.Contains(err.Error(), " or ") {
		t.Errorf("Install() = %q, offers an alternative instead of one action", err)
	}

	// The refusal happened before any mutation: no unit file, no directory
	// of the instance, no supervisor change beyond the reads.
	if _, statErr := os.Stat(unitPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the refused installation wrote the unit file %q: %v", unitPath, statErr)
	}
	for _, dir := range instanceDirs(requested) {
		if _, statErr := os.Stat(dir.path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("the refused installation created the %s %q: %v", dir.name, dir.path, statErr)
		}
	}
	calls := systemdSupervisorCalls(t, logPath)
	for _, command := range [][]string{
		{"daemon-reload"},
		{"enable", unitName},
		{"restart", unitName},
		{"start", unitName},
		{"stop", unitName},
	} {
		if got := systemdSupervisorCallsTo(calls, command...); got != 0 {
			t.Errorf("the refused installation called %q %d times:\n%s", strings.Join(command, " "), got, strings.Join(calls, "\n"))
		}
	}
}

// TestSystemdInstallRefusesAnUpdateWhoseUnitChangesDuringTheUpdate pins the
// TOCTOU guard at the installation level: the fake supervisor rewrites the
// unit file once between the snapshot of the update and the re-read that
// guards the atomic write, so register refuses with the "changed while the
// update was running" cause instead of overwriting a change it never saw —
// no canonical write, no reload, no enable, no restart.
//
// Removing the guard lets the write proceed: the unit file would hold the
// new canonical command and the supervisor would be reloaded, and this test
// fails on both counts.
func TestSystemdInstallRefusesAnUpdateWhoseUnitChangesDuringTheUpdate(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_TAMPER_UNIT", unitPath)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_TAMPER_MARKER", filepath.Join(root, "tampered"))

	_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of a unit file that changed during the update")
	}
	for _, want := range []string{
		unitPath,
		"changed while the update was running",
		`Run "hfdesk service install" again`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install() = %q, want an error that mentions %q", err, want)
		}
	}

	// The seam fired: the file carries exactly the concurrent line. The
	// installation never wrote its canonical rendering over it.
	tampered := previousUnit + "# a concurrent installation wrote this file\n"
	content, readErr := os.ReadFile(unitPath)
	if readErr != nil {
		t.Fatalf("read the unit file: %v", readErr)
	}
	if string(content) != tampered {
		t.Errorf("the unit file =\n%s\nwant the snapshot with exactly the concurrent line\n%s", content, tampered)
	}
	if strings.Contains(string(content), newExecutable) {
		t.Errorf("the refused update wrote its canonical command:\n%s", content)
	}

	calls := systemdSupervisorCalls(t, logPath)
	for _, command := range [][]string{
		{"daemon-reload"},
		{"enable", unitName},
		{"enable", "--runtime", unitName},
		{"restart", unitName},
		{"start", unitName},
		{"stop", unitName},
	} {
		if got := systemdSupervisorCallsTo(calls, command...); got != 0 {
			t.Errorf("the refused update called %q %d times:\n%s", strings.Join(command, " "), got, strings.Join(calls, "\n"))
		}
	}
}

// TestSystemdInstallRefusesARunningUnitWithoutAMainPID checks the fail-closed
// side of the restart-proof snapshot: a running registration whose typed
// MainPID cannot be read gives the update no process identity to prove
// against after the restart, so the installation refuses before any mutation
// instead of restarting onto a proof it could never complete.
func TestSystemdInstallRefusesARunningUnitWithoutAMainPID(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	// The default reader reports MainPID=0 for this unit: no process
	// identity, no snapshot, no update.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)

	_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of a running unit without a MainPID")
	}
	for _, want := range []string{"exposed no MainPID", "the registration was not changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install() = %q, want an error that mentions %q", err, want)
		}
	}
	content, readErr := os.ReadFile(unitPath)
	if readErr != nil || string(content) != previousUnit {
		t.Errorf("the refused update changed the unit (read error %v):\n%s", readErr, content)
	}
	calls := systemdSupervisorCalls(t, logPath)
	for _, command := range [][]string{{"daemon-reload"}, {"restart", unitName}, {"stop", unitName}, {"enable", unitName}} {
		if got := systemdSupervisorCallsTo(calls, command...); got != 0 {
			t.Errorf("the refused installation called %q %d times:\n%s", strings.Join(command, " "), got, strings.Join(calls, "\n"))
		}
	}
}
