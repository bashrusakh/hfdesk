// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

// TestSystemdPathInsideHome pins the semantics of the containment helper the
// guard of the machine-wide scope relies on.
//
// The test is lexical, the home directory itself counts as inside it, and a
// sibling that shares the prefix of the home does not.
func TestSystemdPathInsideHome(t *testing.T) {
	const home = "/home/user"
	cases := []struct {
		name string
		path string
		want bool
	}{
		{name: "a directory inside the home", path: "/home/user/.config/HFDesk/service", want: true},
		{name: "the home itself", path: "/home/user", want: true},
		{name: "a sibling with a common prefix", path: "/home/user2", want: false},
		{name: "a directory outside the home", path: "/srv/hfdesk", want: false},
		{name: "a path that leaves the home again", path: "/home/user/../user2", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hfdownloader.PathInside(home, tc.path); got != tc.want {
				t.Errorf("PathInside(%q, %q) = %v, want %v", home, tc.path, got, tc.want)
			}
		})
	}
}

// TestSystemdCheckSystemPaths checks the guard of the machine-wide scope: a
// path inside the home directory of the user who runs the install command is
// refused, the machine-level paths are accepted, and a user scope instance is
// not touched at all.
func TestSystemdCheckSystemPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

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
			opts := systemdTestOptions(t, ScopeSystem)
			tc.apply(&opts)

			m := &systemdManager{scope: ScopeSystem}
			err := m.checkSystemPaths(opts)
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
		opts := systemdTestOptions(t, ScopeSystem)
		opts.Paths.StateDir = home

		m := &systemdManager{scope: ScopeSystem}
		err := m.checkSystemPaths(opts)
		if err == nil {
			t.Fatal("checkSystemPaths() = nil, want the refusal of the home directory of the user")
		}
		want := fmt.Sprintf("service: the state dir %q of a machine-wide instance must not live inside the home directory %q of the invoking user", home, home)
		if err.Error() != want {
			t.Errorf("checkSystemPaths() = %q, want %q", err, want)
		}
	})

	t.Run("a sibling with a common prefix is accepted", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeSystem)
		opts.Paths.StateDir = sibling

		m := &systemdManager{scope: ScopeSystem}
		if err := m.checkSystemPaths(opts); err != nil {
			t.Errorf("checkSystemPaths() = %v, want nil for the sibling %q of the home %q", err, sibling, home)
		}
	})

	t.Run("the machine-level paths are accepted", func(t *testing.T) {
		m := &systemdManager{scope: ScopeSystem}
		if err := m.checkSystemPaths(systemdTestOptions(t, ScopeSystem)); err != nil {
			t.Errorf("checkSystemPaths() = %v, want nil for the machine-level paths", err)
		}
	})

	t.Run("the user scope is not affected", func(t *testing.T) {
		opts := systemdTestOptions(t, ScopeUser)
		opts.Paths.StateDir = inside
		opts.CacheDir = inside

		m := &systemdManager{scope: ScopeUser}
		if err := m.checkSystemPaths(opts); err != nil {
			t.Errorf("checkSystemPaths() = %v, want nil for a user scope instance", err)
		}
	})
}

// TestSystemdSystemInstallRefusesTheHomeOfTheUser checks that the guard of
// the machine-wide scope runs before the unit file is written: a refused
// installation leaves no unit file and no directory of the instance behind.
func TestSystemdSystemInstallRefusesTheHomeOfTheUser(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	m := &systemdManager{scope: ScopeSystem}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	// The unit file of the machine scope lives in a fixed place of the
	// machine. The test never writes it: the refusal happens before the
	// unit file is written. The state of the file is compared before and
	// after the installation, so a unit that is installed already is never
	// touched by the test.
	before, existedBefore, err := readUnitFile(path)
	if err != nil {
		t.Logf("the unit file %q cannot be read: %v", path, err)
	}

	root := filepath.Join(home, ".config", "HFDesk", "service")
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: systemdTestExecutable(t),
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

	_, err = m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the home directory of the user")
	}
	want := fmt.Sprintf("service: the state dir %q of a machine-wide instance must not live inside the home directory %q of the invoking user", root, home)
	if err.Error() != want {
		t.Errorf("Install() = %q, want %q", err, want)
	}

	// The refused installation writes no unit file.
	after, existedAfter, err := readUnitFile(path)
	if err != nil {
		t.Logf("the unit file %q cannot be read: %v", path, err)
	}
	if existedBefore != existedAfter || string(before) != string(after) {
		t.Errorf("the refused installation changed the unit file %q", path)
	}
	// The refused installation creates no directory of the instance
	// either.
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused installation created the state directory %q: %v", root, err)
	}
}

// TestSystemdStatusReportsTheCacheRoot checks that the status of an instance
// reports the cache root the instance uses: the cache dir of the config file
// of the instance when that file defines one, and the registered cache dir
// otherwise.
func TestSystemdStatusReportsTheCacheRoot(t *testing.T) {
	root := t.TempDir()
	configFile := filepath.Join(root, "hfdesk.json")
	registered := filepath.Join(root, "cache")

	t.Run("the registered cache dir", func(t *testing.T) {
		if err := os.WriteFile(configFile, []byte(`{"local-dir":"/srv/hfdesk/models"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		status := systemdStatus(t, "--config", configFile, "--cache-dir", registered)
		if status.CacheDir != registered {
			t.Errorf("the cache root = %q, want the registered %q", status.CacheDir, registered)
		}
	})

	t.Run("the cache dir of the config file of the instance", func(t *testing.T) {
		configured := filepath.Join(root, "configured")
		if err := os.WriteFile(configFile, []byte(`{"cache-dir":"`+configured+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		status := systemdStatus(t, "--config", configFile, "--cache-dir", registered)
		if status.CacheDir != configured {
			t.Errorf("the cache root = %q, want the cache dir %q of the config file of the instance", status.CacheDir, configured)
		}
	})

	t.Run("no cache dir at all", func(t *testing.T) {
		if err := os.WriteFile(configFile, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		status := systemdStatus(t, "--config", configFile)
		if status.CacheDir != "" {
			t.Errorf("the cache root = %q, want none", status.CacheDir)
		}
	})
}

// TestSystemdStatusReportsAMissingExecutable checks that a registration that
// points at a program that is gone is reported as broken: the state of the
// instance is broken and the detail names the path of the missing program.
//
// The unit file is written into the temporary unit directory of the test, so
// the registration of the user of the machine is never touched, and the
// registration facts of the unit are parsed as they are in a real
// installation.
//
// The test does not need a running supervisor: a broken registration is
// decided from the unit file alone, and the other two cases only assert that
// the registration is not broken, whatever the supervisor answers.
func TestSystemdStatusReportsAMissingExecutable(t *testing.T) {
	t.Run("the registered executable is gone", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "bin", "hfdesk")
		status := systemdStatusOf(t, missing)
		if status.State != StateBroken {
			t.Errorf("the state = %q, want %q", status.State, StateBroken)
		}
		if !strings.Contains(status.Detail, missing) {
			t.Errorf("the detail = %q, want the path of the missing program %q", status.Detail, missing)
		}
		if status.Executable != missing {
			t.Errorf("the executable = %q, want %q", status.Executable, missing)
		}
	})

	t.Run("the registered executable exists", func(t *testing.T) {
		executable := systemdTestExecutable(t)
		status := systemdStatusOf(t, executable)
		if status.State == StateBroken {
			t.Errorf("the state = %q, want a state that is not broken", status.State)
		}
		if status.Executable != executable {
			t.Errorf("the executable = %q, want %q", status.Executable, executable)
		}
	})

	t.Run("the registration holds no executable", func(t *testing.T) {
		status := systemdStatusOf(t)
		if status.State == StateBroken {
			t.Errorf("the state = %q, want a state that is not broken", status.State)
		}
		if status.Executable != "" {
			t.Errorf("the executable = %q, want none", status.Executable)
		}
		// The unit file was really read: the registration that the status
		// describes is the unit of the temporary unit directory, and it
		// holds no program at all.
		content, err := os.ReadFile(status.UnitPath)
		if err != nil {
			t.Fatalf("read the unit file %q: %v", status.UnitPath, err)
		}
		if !strings.Contains(string(content), "ExecStart=") {
			t.Errorf("the unit file holds no ExecStart directive:\n%s", content)
		}
	})
}

// TestSystemdStatusKeepsTheRunningFactOfABrokenRegistration checks that the
// status of a registration whose program is gone keeps the running fact of the
// supervisor next to the broken diagnosis.
//
// systemd keeps the process it started, so deleting the program stops the next
// start and not the instance. The state therefore stays broken, and the detail
// says whether the instance still runs, so a caller that has to know whether
// anything serves does not read "broken" as "nothing runs".
func TestSystemdStatusKeepsTheRunningFactOfABrokenRegistration(t *testing.T) {
	cases := []struct {
		name string
		// active is the state the supervisor reports.
		active string
		// wantRunning is the running fact of that state.
		wantRunning bool
	}{
		{"the instance still runs", "active", true},
		{"the instance does not run", "inactive", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logPath := systemdRecordingSupervisor(t)
			missing := filepath.Join(t.TempDir(), "bin", "hfdesk")
			opts := systemdExistingOptions(t, missing, t.TempDir())
			m, _, _ := systemdExistingInstallation(t, opts)
			t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", tc.active)

			status, err := m.Status(context.Background())
			if err != nil {
				t.Fatalf("Status() = %v, want nil", err)
			}
			if status.State != StateBroken {
				t.Errorf("the state = %q, want the diagnosis %q of the registration", status.State, StateBroken)
			}
			if status.Running != tc.wantRunning {
				t.Errorf("the running fact = %v, want %v", status.Running, tc.wantRunning)
			}
			if !strings.Contains(status.Detail, missing) {
				t.Errorf("the detail = %q, want it to name the missing program %q", status.Detail, missing)
			}
			if got := strings.Contains(status.Detail, "the instance still runs"); got != tc.wantRunning {
				t.Errorf("the detail = %q, want the running fact %v to be described", status.Detail, tc.wantRunning)
			}

			// The running fact comes from the supervisor, so a broken
			// registration is asked about as well.
			shows := 0
			for _, call := range systemdSupervisorCalls(t, logPath) {
				if strings.HasPrefix(strings.TrimPrefix(call, "--user "), "show ") {
					shows++
				}
			}
			if shows != 1 {
				t.Errorf("the supervisor was asked for the state %d times, want once", shows)
			}
		})
	}
}

// systemdStatus registers a unit with the given arguments in a temporary
// configuration directory and returns the status of the instance.
//
// The unit points at a program that exists, so the status describes a
// registration and not a broken one.
func systemdStatus(t *testing.T, args ...string) Status {
	t.Helper()
	return systemdStatusOf(t, append([]string{systemdTestExecutable(t)}, args...)...)
}

// systemdStatusOf registers a unit that starts the given words in a
// temporary configuration directory and returns the status of the instance.
//
// The words are the argument vector of the registered program, so an empty
// vector registers a unit without a program.
//
// The unit file is written into the temporary unit directory of the test, so
// the registration of the user of the machine is never touched. The
// supervisor of the environment may not know the unit: the registration
// facts are filled before the supervisor is asked, and they are the subject
// of the tests.
func systemdStatusOf(t *testing.T, words ...string) Status {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := &systemdManager{scope: ScopeUser}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the unit directory: %v", err)
	}
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, systemdQuoteAlways(word))
	}
	unit := "[Service]\nExecStart=" + strings.Join(quoted, " ") + "\n"
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatalf("write the unit file: %v", err)
	}

	status, err := m.Status(context.Background())
	if err != nil {
		// The supervisor of the test environment may not know the unit:
		// the registration facts are filled before the supervisor is
		// asked, and they are the subject of the test.
		t.Logf("the supervisor did not answer: %v", err)
	}
	return status
}

func TestSystemdPreflightProjectsNativeGroupAndSupplementaryGroupAccess(t *testing.T) {
	systemdRecordingSupervisor(t)
	target := systemdSecondAccount(t)
	targetIdentity, err := linuxExecutableAccessIdentity(target)
	if err != nil {
		t.Skipf("could not resolve target account groups: %v", err)
	}
	targetGroups := make(map[int]struct{}, len(targetIdentity.Groups))
	for _, group := range targetIdentity.Groups {
		targetGroups[group] = struct{}{}
	}
	var extraGroup string
	for _, group := range currentExecutableAccessIdentity().Groups {
		if _, exists := targetGroups[group]; !exists {
			extraGroup = strconv.Itoa(group)
			break
		}
	}
	if extraGroup == "" {
		t.Skip("the test process has no group outside the target account's group database")
	}
	groupID, err := strconv.Atoi(extraGroup)
	if err != nil {
		t.Fatal(err)
	}

	root := systemdTraversableTempDir(t)
	candidate := filepath.Join(root, "candidate")
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(running)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(candidate, 0o010); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(candidate)
	if err != nil {
		t.Fatal(err)
	}
	localDir := filepath.Join(root, "group-only-local")
	if err := os.Mkdir(localDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(localDir, 0o770); err != nil {
		t.Fatal(err)
	}
	opts := systemdTempInstallOptions(t, root)
	opts.Scope = ScopeSystem
	opts.Executable = candidate
	opts.LocalDir = localDir

	type accessCase struct {
		name       string
		group      string
		supplement []string
		allow      bool
	}
	cases := []accessCase{
		{"primary Group grants execution", extraGroup, nil, true},
		{"wrong primary Group denies execution", target.Gid, nil, false},
		{"SupplementaryGroups grants execution", target.Gid, []string{extraGroup}, true},
		{"missing SupplementaryGroups denies execution", target.Gid, nil, false},
	}
	if group, err := user.LookupGroupId(extraGroup); err == nil && group.Name != "" && group.Name != extraGroup {
		cases = append(cases,
			accessCase{"named primary Group grants execution", group.Name, nil, true},
			accessCase{"named SupplementaryGroups grants execution", target.Gid, []string{group.Name}, true},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			properties := systemdTypedProperties(candidate, []string{candidate}, nil, root, target.Username, tc.group, nil)
			properties["SupplementaryGroups"] = dbus.MakeVariant(tc.supplement)
			loaded, err := loadedConfigFromProperties(properties)
			if err != nil {
				t.Fatalf("loadedConfigFromProperties() = %v", err)
			}
			effective := systemdAccessIdentityForInstall(ScopeSystem, InstallOptions{Scope: ScopeSystem}, true, loaded, true, nil)
			identity := effective.access
			if identity == nil || effective.accountName != target.Username || effective.owner == nil {
				t.Fatalf("systemdAccessIdentityForInstall() = %+v, want the loaded target identity and owner", effective)
			}
			gotAccess := effectivePermission(info, *identity)&0o100 != 0
			if gotAccess != tc.allow {
				t.Errorf("loaded identity %+v grants execute permission = %t, want %t", identity, gotAccess, tc.allow)
			}
			dirErr := (&systemdManager{scope: ScopeSystem}).checkServiceAccountAccessWithIdentity(opts, effective.accountName, identity)
			if tc.allow && dirErr != nil {
				t.Errorf("loaded native groups rejected the existing group-writable LocalDir: %v", dirErr)
			}
			if !tc.allow && dirErr == nil {
				t.Error("existing LocalDir passed without its owning native group")
			}
			if tc.group == extraGroup && identity.GID != groupID {
				t.Errorf("loaded Group primary GID = %d, want %d", identity.GID, groupID)
			}
			if slices.Contains(tc.supplement, extraGroup) && !slices.Contains(identity.Groups, groupID) {
				t.Errorf("loaded supplementary groups %v omit configured GID %d", identity.Groups, groupID)
			}
			err = (&systemdManager{scope: ScopeSystem}).preflightWithContext(context.Background(), opts, effective.accountName, identity, root)
			if os.Geteuid() == 0 {
				if tc.allow && err != nil {
					t.Errorf("preflight rejected native group access: %v", err)
				}
				if !tc.allow && err == nil {
					t.Error("preflight accepted an executable outside the loaded native groups")
				}
			} else if err != nil {
				t.Errorf("preflight turned unavailable target-credential evidence into a denial: %v", err)
			}
		})
	}
	t.Run("explicit ServiceUser overrides loaded owner", func(t *testing.T) {
		properties := systemdTypedProperties(candidate, []string{candidate}, nil, root, target.Username, extraGroup, nil)
		properties["SupplementaryGroups"] = dbus.MakeVariant([]string{extraGroup})
		loaded, err := loadedConfigFromProperties(properties)
		if err != nil {
			t.Fatal(err)
		}
		requested := InstallOptions{Scope: ScopeSystem}
		requested.Explicit.ServiceUser = true
		requestedAccount := systemdCurrentAccount(t)
		effective := systemdAccessIdentityForInstall(ScopeSystem, requested, true, loaded, true, requestedAccount)
		wantUID, _ := strconv.Atoi(requestedAccount.Uid)
		wantGID, _ := strconv.Atoi(requestedAccount.Gid)
		if effective.owner == nil || effective.owner.UID != wantUID || effective.owner.GID != wantGID {
			t.Errorf("explicit ServiceUser owner = %+v, want requested UID/GID %d:%d", effective.owner, wantUID, wantGID)
		}
	})

	t.Run("unestablished identity stays inconclusive", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			loaded      systemdLoadedConfig
			loadedKnown bool
			ownerKnown  bool
		}{
			{"unreadable manager context", systemdLoadedConfig{}, false, false},
			{"unit exists on disk but is unloaded", systemdLoadedConfig{loaded: false}, true, false},
			{"loaded UID/GID without a complete group list", systemdLoadedConfig{loaded: true, user: target.Username, group: target.Gid}, true, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				effective := systemdAccessIdentityForInstall(ScopeSystem, InstallOptions{Scope: ScopeSystem}, true, tc.loaded, tc.loadedKnown, nil)
				if effective.access != nil || (effective.owner != nil) != tc.ownerKnown {
					t.Fatalf("unavailable loaded identity = %+v, want access unknown and owner known=%t", effective, tc.ownerKnown)
				}
				if cwd := systemdWorkingDirectoryForPreflight(opts, true, tc.loaded, tc.loadedKnown); cwd != "" {
					t.Fatalf("unavailable native WorkingDirectory = %q, want unknown", cwd)
				}
				if err := (&systemdManager{scope: ScopeSystem}).preflightWithContext(context.Background(), opts, effective.accountName, effective.access, ""); err != nil {
					t.Errorf("preflight() = %v, want unknown native context to remain inconclusive", err)
				}
				if effective.owner == nil {
					newDir := filepath.Join(t.TempDir(), "new-state")
					created := map[string][]string{newDir: {newDir}}
					err := (&systemdManager{scope: ScopeSystem}).grantInstanceDirsForDirsWithIdentity(opts, effective, created, []instanceDir{{name: "state dir", path: newDir}})
					if err == nil || !strings.Contains(err.Error(), "effective systemd UID/GID is not established") {
						t.Errorf("ownership preparation error = %v, want a concrete unknown-native-owner blocker", err)
					}
				}
			})
		}
	})
}

func TestSystemdStatusUsesAcceptedRegistrationFlagSyntax(t *testing.T) {
	systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	manager := &systemdManager{scope: ScopeUser}
	path, err := manager.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v", err)
	}
	root := t.TempDir()
	unit := fmt.Sprintf(`[Service]
ExecStart=/bin/true service run -config=%s --config=%s -state-dir=%s --state-dir=%s -log-dir=%s -listen=127.0.0.1:8181 --listen=127.0.0.1:9191 -cache-dir=%s --cache-dir=%s -hub-dir=%s --hub-dir=%s -local-dir=%s --local-dir=%s
`, filepath.Join(root, "old.json"), filepath.Join(root, "current.json"), filepath.Join(root, "old-state"), filepath.Join(root, "state"), filepath.Join(root, "logs"), filepath.Join(root, "old-cache"), filepath.Join(root, "cache"), filepath.Join(root, "old-hub"), filepath.Join(root, "hub"), filepath.Join(root, "old-models"), filepath.Join(root, "models"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the unit directory: %v", err)
	}
	for _, name := range []string{path, os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")} {
		if err := os.WriteFile(name, []byte(unit), 0o644); err != nil {
			t.Fatalf("write the unit file %q: %v", name, err)
		}
	}
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	wantPaths := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "current.json"),
		StateDir:   filepath.Join(root, "state"),
		LogDir:     filepath.Join(root, "logs"),
	}
	if status.Executable != "/bin/true" || status.Listen != "127.0.0.1:9191" || status.Paths != wantPaths || status.CacheDir != filepath.Join(root, "cache") {
		t.Errorf("Status() = %+v, want the last accepted values in the registered command", status)
	}
}

// TestSystemdRestartVerifyWaitIsTheShutdownBudget checks that the verification
// of a restarted instance is bounded by the shutdown budget of the service
// layer, so the wait covers the orderly shutdown of the old process.
func TestSystemdRestartVerifyWaitIsTheShutdownBudget(t *testing.T) {
	if got := (&systemdManager{}).restartVerifyWait(); got != ShutdownBudget {
		t.Errorf("restartVerifyWait() = %s, want the shutdown budget %s", got, ShutdownBudget)
	}
	if got := (&systemdManager{restartWait: time.Second}).restartVerifyWait(); got != time.Second {
		t.Errorf("restartVerifyWait() = %s, want the wait of the manager %s", got, time.Second)
	}
}

// TestSystemdCheckExecutableAccessUsesKernelPermissions checks the effective
// kernel access result, including execute-only native files and parent search.
func TestSystemdCheckExecutableAccessUsesKernelPermissions(t *testing.T) {
	if accessProbeHasCapabilities() {
		t.Skip("current-identity access is deliberately inconclusive with effective/permitted capabilities")
	}

	root := t.TempDir()
	executable := filepath.Join(root, "program")
	if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	identity := currentExecutableAccessIdentity()
	readOnlyDenial := executableAccessDenied
	if os.Geteuid() == 0 {
		readOnlyDenial = executableAccessAllowed
	}
	for _, tc := range []struct {
		name string
		mode os.FileMode
		want executableAccess
		bits uint32
	}{
		{"owner execute", 0o700, executableAccessAllowed, unix.X_OK},
		{"execute-only native access", 0o100, executableAccessAllowed, unix.X_OK},
		{"read and execute follow the current identity", 0o100, readOnlyDenial, unix.R_OK | unix.X_OK},
		{"no execute permission", 0o600, executableAccessDenied, unix.X_OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Chmod(executable, tc.mode); err != nil {
				t.Fatal(err)
			}
			got, err := checkExecutableAccess(context.Background(), executable, identity, tc.bits, "")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("checkExecutableAccess(%04o, %03o) = %v, want %v", tc.mode, tc.bits, got, tc.want)
			}
		})
	}

	t.Run("parent without search permission", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can bypass this DAC search denial")
		}
		parent := filepath.Join(root, "blocked")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		blocked := filepath.Join(parent, "program")
		if err := os.WriteFile(blocked, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := checkExecutableAccess(context.Background(), blocked, identity, unix.X_OK, "")
		if err != nil {
			t.Fatal(err)
		}
		if got != executableAccessDenied {
			t.Errorf("checkExecutableAccess through a non-searchable parent = %v, want denied", got)
		}
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Errorf("restore the temporary parent permissions for cleanup: %v", err)
		}
	})

	t.Run("symlink to executable", func(t *testing.T) {
		if err := os.Chmod(executable, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "program-link")
		if err := os.Symlink(executable, link); err != nil {
			t.Fatal(err)
		}
		got, err := checkExecutableAccess(context.Background(), link, identity, unix.X_OK, "")
		if err != nil {
			t.Fatal(err)
		}
		if got != executableAccessAllowed {
			t.Errorf("checkExecutableAccess through a symlink = %v, want allowed", got)
		}
	})
}

func TestLinuxExecutableAccessIdentityCarriesSupplementaryGroups(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := linuxExecutableAccessIdentity(account)
	if err != nil {
		t.Skipf("could not resolve account groups: %v", err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	if identity.UID != uid || identity.GID != gid {
		t.Fatalf("identity = %+v, want uid=%d gid=%d", identity, uid, gid)
	}
	accountGroups, err := account.GroupIds()
	if err != nil {
		t.Skipf("could not resolve supplementary groups: %v", err)
	}
	got := make(map[int]bool, len(identity.Groups))
	for _, group := range identity.Groups {
		got[group] = true
	}
	for _, groupID := range accountGroups {
		group, err := strconv.Atoi(groupID)
		if err != nil {
			t.Fatalf("parse account group %q: %v", groupID, err)
		}
		if !got[group] {
			t.Errorf("identity groups %v omit account group %d", identity.Groups, group)
		}
	}
	credential, err := accessCredential(identity)
	if err != nil {
		t.Fatal(err)
	}
	if int(credential.Uid) != uid || int(credential.Gid) != gid {
		t.Errorf("credential = %+v, want uid=%d gid=%d", credential, uid, gid)
	}
	for _, group := range identity.Groups {
		found := false
		for _, credentialGroup := range credential.Groups {
			if int(credentialGroup) == group {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("credential groups %v omit identity group %d", credential.Groups, group)
		}
	}
}

func TestLinuxIDToUint32ChecksFullNumericBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   int64
		want    uint32
		wantErr bool
	}{
		{"zero", 0, 0, false},
		{"largest ordinary 32-bit ID", int64(math.MaxUint32 - 1), uint32(math.MaxUint32 - 1), false},
		{"uint32 sentinel bit pattern", int64(math.MaxUint32), math.MaxUint32, false},
		{"negative", -1, 0, true},
		{"first value beyond uint32", int64(math.MaxUint32) + 1, 0, true},
	} {
		got, err := linuxIDToUint32(tc.value)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("%s: linuxIDToUint32(%d) = (%d, %v), want (%d, error=%t)", tc.name, tc.value, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestAccessCredentialPreservesAndRejectsNativeIntegerBoundaries(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("accessAccount stores IDs as int; this host cannot represent the full uint32 domain")
	}
	maxUint32 := uint64(math.MaxUint32)
	maxID := int(maxUint32 - 1)
	credential, err := accessCredential(accessAccount{UID: maxID, GID: maxID, Groups: []int{maxID}})
	if err != nil {
		t.Fatalf("accessCredential(max uint32 bit pattern) = %v", err)
	}
	maxLegalID := uint32(maxUint32 - 1)
	if credential.Uid != maxLegalID || credential.Gid != maxLegalID || !slices.Contains(credential.Groups, maxLegalID) {
		t.Errorf("credential did not preserve the largest mapped Linux ID: %+v", credential)
	}
	overflow := int(maxUint32 + 1)
	for _, identity := range []accessAccount{
		{UID: -1},
		{GID: -1},
		{Groups: []int{-1}},
		{UID: overflow},
	} {
		if _, err := accessCredential(identity); err == nil {
			t.Errorf("accessCredential(%+v) accepted an ID outside its representable Linux domain", identity)
		}
	}
}

func TestInternalAccessProbeReportsKernelDenialSeparatelyFromUnknown(t *testing.T) {
	if accessProbeHasCapabilities() {
		t.Skip("the internal helper refuses to claim access under effective/permitted capabilities")
	}
	if code, handled := RunInternalAccessProbe([]string{"status"}); handled || code != 0 {
		t.Fatalf("ordinary arguments were handled as an internal probe: code=%d handled=%t", code, handled)
	}

	file := filepath.Join(t.TempDir(), "execute-only")
	if err := os.WriteFile(file, []byte("content"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o100); err != nil {
		t.Fatal(err)
	}
	code, handled := RunInternalAccessProbe([]string{internalAccessProbeCommand, file, strconv.Itoa(unix.X_OK), ""})
	if !handled || code != accessProbeAllowedCode {
		t.Errorf("execute-only X_OK probe = (%d, %t), want allowed", code, handled)
	}
	code, handled = RunInternalAccessProbe([]string{internalAccessProbeCommand, file, strconv.Itoa(unix.R_OK | unix.X_OK), ""})
	if !handled || code != accessProbeDeniedCode {
		t.Errorf("execute-only R_OK|X_OK probe = (%d, %t), want definite denial", code, handled)
	}
	code, handled = RunInternalAccessProbe([]string{internalAccessProbeCommand, filepath.Join(t.TempDir(), "missing"), strconv.Itoa(unix.X_OK), ""})
	if !handled || code != accessProbeDeniedCode {
		t.Errorf("missing path probe = (%d, %t), want definite denial", code, handled)
	}
	code, handled = RunInternalAccessProbe([]string{internalAccessProbeCommand, file, "invalid", ""})
	if !handled || code != accessProbeUnknownCode {
		t.Errorf("malformed internal request = (%d, %t), want unknown", code, handled)
	}
}

func TestSystemdExecuteOnlyNativeCandidateRunsAndInstalls(t *testing.T) {
	current, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(t.TempDir(), "hfdesk")
	if err := os.WriteFile(native, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(native, 0o100); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(native, "-test.run=^$").CombinedOutput(); err != nil {
		t.Fatalf("execute-only native control did not run: %v: %s", err, out)
	}

	_ = systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	opts := systemdTempInstallOptions(t, t.TempDir())
	opts.Executable = native
	manager := &systemdManager{scope: ScopeUser}
	systemdLoadedFromDiskSeam(t, manager)
	if _, err := manager.Install(context.Background(), opts); err != nil {
		t.Fatalf("Install() refused the execute-only native candidate: %v", err)
	}
	setSystemdTestPropertyReader(t, manager)

	link := filepath.Join(t.TempDir(), "hfdesk-link")
	if err := os.Symlink(native, link); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(link, "-test.run=^$").CombinedOutput(); err != nil {
		t.Fatalf("execute-only native symlink did not run: %v: %s", err, out)
	}
	opts.Executable = link
	effective, err := manager.Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("Install() refused the execute-only native symlink: %v", err)
	}
	if effective.Executable != link {
		t.Errorf("registered executable = %q, want preserved symlink path %q", effective.Executable, link)
	}
}

func TestSystemdPreflightAcceptsReadableShebang(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh is unavailable: %v", err)
	}
	systemdRecordingSupervisor(t)
	root := t.TempDir()
	opts := systemdTempInstallOptions(t, root)
	opts.Executable = filepath.Join(t.TempDir(), "hfdesk-script")
	if err := os.WriteFile(opts.Executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(opts.Executable).Run(); err != nil {
		t.Fatalf("readable shebang control did not run: %v", err)
	}
	if err := (&systemdManager{scope: ScopeUser}).preflight(context.Background(), opts, nil); err != nil {
		t.Fatalf("preflight refused a readable runnable script: %v", err)
	}
}

func TestSystemdPreflightUsesLoadedWorkingDirectoryForRelativeInterpreters(t *testing.T) {
	if binfmtMiscHandlers(linuxBinfmtMiscDirectory) != binfmtMiscEmpty {
		t.Skip("the host's binfmt_misc state does not establish which kernel interpreter handles these formats")
	}
	if accessProbeHasCapabilities() {
		t.Skip("the current identity access probe is inconclusive with effective or permitted capabilities")
	}
	systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	identity := currentExecutableAccessIdentity()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	workingDirectory := filepath.Join(root, "native-working-directory")
	stateDirectory := filepath.Join(root, "application-state")
	for _, directory := range []string{workingDirectory, stateDirectory} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name  string
		build func(*testing.T, string) (string, []string)
	}{
		{
			name: "relative shebang",
			build: func(t *testing.T, dir string) (string, []string) {
				interpreter := filepath.Join(dir, "runner")
				if err := os.WriteFile(interpreter, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				candidate := filepath.Join(root, "shebang-candidate")
				if err := os.WriteFile(candidate, []byte("#!./runner\nexit 0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				return candidate, nil
			},
		},
		{
			name: "relative ELF PT_INTERP",
			build: func(t *testing.T, dir string) (string, []string) {
				image, originalInterpreter, ok := elfWithInterpreterPath(t, "./elf-loader")
				if !ok {
					t.Skip("the running ELF has no PT_INTERP space for a relative loader path")
				}
				candidate := filepath.Join(root, "elf-candidate")
				if err := os.WriteFile(candidate, image, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(originalInterpreter, filepath.Join(dir, "elf-loader")); err != nil {
					t.Fatal(err)
				}
				return candidate, []string{"-test.run=^$"}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate, args := tc.build(t, workingDirectory)
			for _, dir := range []struct {
				name string
				path string
				want bool
			}{
				{"loaded cwd", workingDirectory, true},
				{"application StateDir control", stateDirectory, false},
			} {
				command := exec.Command(candidate, args...)
				command.Dir = dir.path
				err := command.Run()
				if dir.want && err != nil {
					t.Fatalf("kernel execution from %s failed: %v", dir.name, err)
				}
				if !dir.want && err == nil {
					t.Fatalf("kernel execution from %s unexpectedly succeeded", dir.name)
				}
			}

			opts := systemdTempInstallOptions(t, root)
			opts.Executable = candidate
			opts.Paths.StateDir = stateDirectory
			properties := systemdTypedProperties(candidate, []string{candidate}, nil, workingDirectory, account.Username, account.Gid, nil)
			loaded, err := loadedConfigFromProperties(properties)
			if err != nil {
				t.Fatalf("loadedConfigFromProperties() = %v", err)
			}
			projectedCWD := systemdWorkingDirectoryForPreflight(opts, true, loaded, true)
			if projectedCWD != workingDirectory || projectedCWD == opts.Paths.StateDir {
				t.Fatalf("preflight cwd = %q, want loaded native WorkingDirectory %q, distinct from StateDir %q", projectedCWD, workingDirectory, opts.Paths.StateDir)
			}
			manager := &systemdManager{scope: ScopeUser}
			if err := manager.preflightWithContext(context.Background(), opts, account.Username, &identity, projectedCWD); err != nil {
				t.Fatalf("preflight with loaded WorkingDirectory refused the runnable candidate: %v", err)
			}
			err = manager.preflightWithContext(context.Background(), opts, account.Username, &identity, stateDirectory)
			if err == nil {
				t.Fatal("preflight using application StateDir accepted an interpreter absent from that cwd")
			}
			// The refusal names the interpreter as its cause and the
			// one action path: grant the account access to it, then
			// rerun the installation in this manager's scope.
			for _, want := range []string{"names an interpreter the account", `run "hfdesk service install --user" again`} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the interpreter refusal %q does not mention %q", err, want)
				}
			}
			unknownCWD := systemdWorkingDirectoryForPreflight(opts, true, systemdLoadedConfig{}, false)
			if unknownCWD != "" {
				t.Fatalf("unavailable native WorkingDirectory = %q, want unknown", unknownCWD)
			}
			if err := manager.preflightWithContext(context.Background(), opts, account.Username, &identity, unknownCWD); err != nil {
				t.Errorf("preflight denied an unknown relative-interpreter context: %v", err)
			}
		})
	}
}

// TestSystemdInstallRefusesAnExecutableTheAccountCannotRun checks that a refused
// program fails the installation before the supervisor is asked to act and before
// the unit file or a directory is written.
func TestSystemdInstallRefusesAnExecutableTheAccountCannotRun(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the user-scope access case needs a non-root identity")
	}
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()

	opts := systemdTempInstallOptions(t, root)
	if err := os.Chmod(opts.Executable, 0o001); err != nil {
		t.Fatal(err)
	}

	m := &systemdManager{scope: ScopeUser}
	_, err := m.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of a program the account cannot run")
	}
	account := systemdCurrentAccount(t)
	// The refusal names the one action path: grant the account access,
	// then rerun the installation in the scope this manager runs — the
	// test installs the user scope, which the rerun command names.
	for _, want := range []string{opts.Executable, account.Username, "not usable by the account", `run "hfdesk service install --user" again`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install() = %v, want %q in the refusal", err, want)
		}
	}

	path, pathErr := m.unitPath()
	if pathErr != nil {
		t.Fatalf("unitPath() = %v, want nil", pathErr)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the refused installation wrote the unit file %q: %v", path, statErr)
	}
	for _, dir := range instanceDirs(opts) {
		if _, statErr := os.Stat(dir.path); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("the refused installation created the %s %q: %v", dir.name, dir.path, statErr)
		}
	}

	calls := systemdSupervisorCalls(t, logPath)
	for _, command := range []string{"daemon-reload", "enable", "restart", "start"} {
		if got := systemdSupervisorCallsTo(calls, command, unitName); got != 0 {
			t.Errorf("the refused installation issued %q %d times:\n%s", command, got, strings.Join(calls, "\n"))
		}
	}
}

func writeBinfmtMiscState(t *testing.T, active bool) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte("enabled\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if active {
		if err := os.WriteFile(filepath.Join(dir, "custom"), []byte("enabled\ninterpreter /usr/bin/qemu\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func elfWithMissingInterpreter(t *testing.T) ([]byte, bool) {
	t.Helper()
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(running)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := elf.NewFile(bytes.NewReader(content))
	if err != nil {
		return nil, false
	}
	var interpreter *elf.Prog
	for _, program := range parsed.Progs {
		if program.Type == elf.PT_INTERP {
			interpreter = program
			break
		}
	}
	if interpreter == nil || interpreter.Filesz < 3 || interpreter.Filesz > 4096 {
		return nil, false
	}
	start := int(interpreter.Off)
	size := int(interpreter.Filesz)
	if start < 0 || start+size > len(content) {
		return nil, false
	}
	missing := "/" + strings.Repeat("z", size-2)
	if len(missing)+1 != size {
		return nil, false
	}
	if _, err := os.Stat(missing); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}
	segment := make([]byte, size)
	copy(segment, missing)
	copy(content[start:start+size], segment)
	return content, true
}

func elfWithInterpreterPath(t *testing.T, interpreterPath string) ([]byte, string, bool) {
	t.Helper()
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(running)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := elf.NewFile(bytes.NewReader(content))
	if err != nil {
		return nil, "", false
	}
	for _, program := range parsed.Progs {
		if program.Type != elf.PT_INTERP || program.Filesz < 2 || program.Filesz > 4096 {
			continue
		}
		if program.Off > uint64(len(content)) || program.Filesz > uint64(len(content))-program.Off {
			return nil, "", false
		}
		start, size := int(program.Off), int(program.Filesz)
		if len(interpreterPath)+1 > size {
			return nil, "", false
		}
		segment := content[start : start+size]
		terminator := bytes.IndexByte(segment, 0)
		if terminator <= 0 {
			return nil, "", false
		}
		original := string(segment[:terminator])
		replacement := make([]byte, size)
		copy(replacement, interpreterPath)
		copy(content[start:start+size], replacement)
		return content, original, true
	}
	return nil, "", false
}

func makeInterpreterSymlinkTree(t *testing.T, root, target string, atLexicalPath bool) {
	t.Helper()
	actualInside := filepath.Join(root, "actual", "inside")
	if err := os.MkdirAll(actualInside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("actual", "inside"), filepath.Join(root, "via")); err != nil {
		t.Fatal(err)
	}
	interpreter := filepath.Join(root, "actual", "runner")
	if atLexicalPath {
		interpreter = filepath.Join(root, "runner")
	}
	if err := os.Symlink(target, interpreter); err != nil {
		t.Fatal(err)
	}
}

func TestSystemdInterpreterPathsPreserveKernelSymlinkDotDotResolution(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the direct interpreter controls run only as an unprivileged user")
	}
	if binfmtMiscHandlers(linuxBinfmtMiscDirectory) != binfmtMiscEmpty {
		t.Skip("the host has active or unreadable binfmt_misc handlers; these interpreter formats are unclassified")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh is unavailable: %v", err)
	}
	systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	identity := currentExecutableAccessIdentity()
	manager := &systemdManager{scope: ScopeUser}

	validRoot := t.TempDir()
	makeInterpreterSymlinkTree(t, validRoot, "/bin/sh", false)
	absoluteInterpreter := validRoot + "/via/../runner"
	absoluteScript := filepath.Join(validRoot, "absolute-script")
	if err := os.WriteFile(absoluteScript, []byte("#!"+absoluteInterpreter+"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(absoluteScript).CombinedOutput(); err != nil {
		t.Fatalf("kernel rejected valid absolute shebang path %q: %v: %s", absoluteInterpreter, err, out)
	}
	inspection, err := inspectExecutable(absoluteScript, validRoot, linuxBinfmtMiscDirectory)
	if err != nil || inspection.interpreter != absoluteInterpreter {
		t.Fatalf("absolute shebang inspection = (%+v, %v), want raw path %q", inspection, err, absoluteInterpreter)
	}
	access, err := checkExecutableAccess(context.Background(), inspection.interpreter, identity, unix.X_OK, "")
	if err != nil || access != executableAccessAllowed {
		t.Errorf("kernel access for valid absolute interpreter = (%v, %v), want allowed", access, err)
	}
	opts := systemdTempInstallOptions(t, t.TempDir())
	opts.Executable = absoluteScript
	opts.Paths.StateDir = validRoot
	if err := manager.preflight(context.Background(), opts, nil); err != nil {
		t.Errorf("preflight refused the runnable absolute symlink/.. script: %v", err)
	}

	relativeScript := filepath.Join(validRoot, "relative-script")
	if err := os.WriteFile(relativeScript, []byte("#!via/../runner\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(relativeScript)
	command.Dir = validRoot
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("kernel rejected valid relative shebang path: %v: %s", err, out)
	}
	inspection, err = inspectExecutable(relativeScript, validRoot, linuxBinfmtMiscDirectory)
	if err != nil || inspection.interpreter != "via/../runner" {
		t.Fatalf("relative shebang inspection = (%+v, %v), want raw path", inspection, err)
	}
	access, err = checkExecutableAccess(context.Background(), inspection.interpreter, identity, unix.X_OK, validRoot)
	if err != nil || access != executableAccessAllowed {
		t.Errorf("kernel access for valid relative interpreter = (%v, %v), want allowed", access, err)
	}
	opts.Executable = relativeScript
	if err := manager.preflight(context.Background(), opts, nil); err != nil {
		t.Errorf("preflight refused the runnable relative symlink/.. script: %v", err)
	}

	missingRoot := t.TempDir()
	makeInterpreterSymlinkTree(t, missingRoot, "/bin/sh", true)
	missingAbsolute := missingRoot + "/via/../runner"
	missingScript := filepath.Join(missingRoot, "script")
	if err := os.WriteFile(missingScript, []byte("#!"+missingAbsolute+"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(missingScript).Run(); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("kernel control for missing absolute interpreter = %v, want ENOENT", err)
	}
	assertSystemdInstallRefusedBeforeRestart(t, missingScript, missingRoot)

	missingRelative := filepath.Join(missingRoot, "relative-script")
	if err := os.WriteFile(missingRelative, []byte("#!via/../runner\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command = exec.Command(missingRelative)
	command.Dir = missingRoot
	if err := command.Run(); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("kernel control for missing relative interpreter = %v, want ENOENT", err)
	}
	assertSystemdInstallRefusedBeforeRestart(t, missingRelative, missingRoot)

	elfPath := "via/../runner"
	validELFRoot := t.TempDir()
	validELF, originalInterpreter, ok := elfWithInterpreterPath(t, elfPath)
	if !ok {
		t.Log("the test binary has no room for a relative PT_INTERP fixture; ELF path variant not exercised")
		return
	}
	makeInterpreterSymlinkTree(t, validELFRoot, originalInterpreter, false)
	validELFCandidate := filepath.Join(validELFRoot, "candidate")
	if err := os.WriteFile(validELFCandidate, validELF, 0o700); err != nil {
		t.Fatal(err)
	}
	command = exec.Command(validELFCandidate, "-test.run=^$")
	command.Dir = validELFRoot
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("kernel rejected valid relative PT_INTERP %q: %v: %s", elfPath, err, out)
	}
	elfInspection, err := inspectExecutable(validELFCandidate, validELFRoot, linuxBinfmtMiscDirectory)
	if err != nil || elfInspection.interpreter != elfPath {
		t.Fatalf("PT_INTERP inspection = (%+v, %v), want raw relative path %q", elfInspection, err, elfPath)
	}
	access, err = checkExecutableAccess(context.Background(), elfInspection.interpreter, identity, unix.X_OK, validELFRoot)
	if err != nil || access != executableAccessAllowed {
		t.Errorf("kernel access for valid relative PT_INTERP = (%v, %v), want allowed", access, err)
	}
	opts = systemdTempInstallOptions(t, t.TempDir())
	opts.Executable = validELFCandidate
	opts.Paths.StateDir = validELFRoot
	if err := manager.preflight(context.Background(), opts, nil); err != nil {
		t.Errorf("preflight refused the runnable relative PT_INTERP: %v", err)
	}

	missingELFRoot := t.TempDir()
	makeInterpreterSymlinkTree(t, missingELFRoot, originalInterpreter, true)
	missingELF, _, ok := elfWithInterpreterPath(t, elfPath)
	if !ok {
		t.Fatal("relative PT_INTERP no longer fits the test binary")
	}
	missingELFCandidate := filepath.Join(missingELFRoot, "candidate")
	if err := os.WriteFile(missingELFCandidate, missingELF, 0o700); err != nil {
		t.Fatal(err)
	}
	command = exec.Command(missingELFCandidate, "-test.run=^$")
	command.Dir = missingELFRoot
	if err := command.Run(); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("kernel control for missing relative PT_INTERP = %v, want ENOENT", err)
	}
	assertSystemdInstallRefusedBeforeRestart(t, missingELFCandidate, missingELFRoot)
}

func TestSystemdInspectExecutableUsesHostEvidence(t *testing.T) {
	emptyHandlers := writeBinfmtMiscState(t, false)
	activeHandlers := writeBinfmtMiscState(t, true)
	write := func(t *testing.T, name string, content []byte, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, content, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}

	plainText := write(t, "hfdesk", []byte("not an executable\n"), 0o700)
	if _, err := inspectExecutable(plainText, t.TempDir(), emptyHandlers); err == nil {
		t.Fatal("plain text with no shebang and no registered binfmt handler was not refused")
	}
	inspection, err := inspectExecutable(plainText, t.TempDir(), activeHandlers)
	if err != nil || inspection.format != executableFormatUnknown {
		t.Fatalf("plain text with a host handler = (%+v, %v), want unclassified and allowed", inspection, err)
	}
	if _, err := inspectExecutable(plainText, t.TempDir(), filepath.Join(t.TempDir(), "unavailable")); err != nil {
		t.Fatalf("plain text with unavailable handler evidence was refused: %v", err)
	}

	truncated := make([]byte, 20)
	copy(truncated, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	truncatedPath := write(t, "truncated-elf", truncated, 0o700)
	if _, err := inspectExecutable(truncatedPath, t.TempDir(), emptyHandlers); err == nil {
		t.Fatal("truncated ELF header with no registered handler was not refused")
	}
	inspection, err = inspectExecutable(truncatedPath, t.TempDir(), activeHandlers)
	if err != nil || inspection.format != executableFormatUnknown {
		t.Fatalf("truncated ELF with a host handler = (%+v, %v), want unclassified and allowed", inspection, err)
	}

	missingInterpreter := filepath.Join(t.TempDir(), "missing-interpreter")
	script := write(t, "script", []byte("#!"+missingInterpreter+"\nexit 0\n"), 0o700)
	inspection, err = inspectExecutable(script, t.TempDir(), emptyHandlers)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.format != executableFormatScript || inspection.interpreter != missingInterpreter {
		t.Fatalf("script inspection = %+v, want interpreter %q", inspection, missingInterpreter)
	}
	access, err := checkExecutableAccess(context.Background(), inspection.interpreter, currentExecutableAccessIdentity(), unix.X_OK, "")
	if err != nil || access != executableAccessDenied {
		t.Errorf("missing interpreter access = (%v, %v), want definite denial", access, err)
	}

	if _, err := os.Stat("/bin/sh"); err == nil {
		readableScript := write(t, "readable-script", []byte("#!/bin/sh\nexit 0\n"), 0o700)
		inspection, err := inspectExecutable(readableScript, t.TempDir(), emptyHandlers)
		if err != nil || inspection.format != executableFormatScript || inspection.interpreter != "/bin/sh" {
			t.Fatalf("readable script inspection = (%+v, %v)", inspection, err)
		}
		if err := exec.Command(readableScript).Run(); err != nil {
			t.Fatalf("the readable script control did not run: %v", err)
		}
	}

	if dynamicELF, ok := elfWithMissingInterpreter(t); ok {
		candidate := write(t, "missing-elf-interpreter", dynamicELF, 0o700)
		inspection, err := inspectExecutable(candidate, t.TempDir(), emptyHandlers)
		if err != nil {
			t.Fatal(err)
		}
		if inspection.format != executableFormatELF || inspection.interpreter == "" {
			t.Fatalf("ELF PT_INTERP inspection = %+v, want a discovered interpreter", inspection)
		}
		access, err := checkExecutableAccess(context.Background(), inspection.interpreter, currentExecutableAccessIdentity(), unix.X_OK, "")
		if err != nil || access != executableAccessDenied {
			t.Errorf("missing PT_INTERP access = (%v, %v), want definite denial", access, err)
		}
	}

	// A structurally complete ELF with another machine identity is not
	// refused by a GOARCH table; the kernel/host remains the format owner.
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	elf, err := os.ReadFile(running)
	if err != nil || len(elf) < 64 || !bytes.HasPrefix(elf, []byte{0x7f, 'E', 'L', 'F'}) {
		t.Fatalf("read the running ELF fixture: %v", err)
	}
	var order binary.ByteOrder = binary.LittleEndian
	if elf[5] == 2 {
		order = binary.BigEndian
	}
	machine := order.Uint16(elf[18:20])
	if machine == ^uint16(0) {
		machine = 0
	} else {
		machine++
	}
	order.PutUint16(elf[18:20], machine)
	otherMachine := write(t, "other-machine-elf", elf, 0o700)
	inspection, err = inspectExecutable(otherMachine, t.TempDir(), emptyHandlers)
	if err != nil || inspection.format != executableFormatELF {
		t.Errorf("structurally complete ELF with another machine = (%+v, %v), want no parser refusal", inspection, err)
	}

	// Other signatures stay unclassified rather than being treated as proof
	// of an unsupported format.
	other := write(t, "other-format", []byte("MZ\x90\x00"), 0o700)
	inspection, err = inspectExecutable(other, t.TempDir(), emptyHandlers)
	if err != nil || inspection.format != executableFormatUnknown {
		t.Errorf("unrecognized format = (%+v, %v), want unclassified and allowed", inspection, err)
	}
}

func TestSystemdInstallRefusesKnownUnusableBeforeRestart(t *testing.T) {
	if binfmtMiscHandlers(linuxBinfmtMiscDirectory) != binfmtMiscEmpty {
		t.Skip("the host has active or unreadable binfmt_misc handlers; these inputs are intentionally unclassified")
	}

	truncated := make([]byte, 20)
	copy(truncated, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	cases := []struct {
		name string
		data []byte
	}{
		{"plain text", []byte("not an executable\n")},
		{"truncated ELF", truncated},
		{"missing interpreter", []byte("#!/definitely-missing-hfdesk-interpreter\nexit 0\n")},
	}
	if dynamicELF, ok := elfWithMissingInterpreter(t); ok {
		cases = append(cases, struct {
			name string
			data []byte
		}{"missing ELF interpreter", dynamicELF})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := filepath.Join(t.TempDir(), "hfdesk")
			if err := os.WriteFile(candidate, tc.data, 0o700); err != nil {
				t.Fatal(err)
			}
			assertSystemdInstallRefusedBeforeRestart(t, candidate, "")
		})
	}
}

func assertSystemdInstallRefusedBeforeRestart(t *testing.T, candidate, workingDirectory string) {
	t.Helper()
	logPath := systemdRecordingSupervisor(t)
	previous := systemdExistingOptions(t, systemdTestExecutable(t), t.TempDir())
	if workingDirectory != "" {
		previous.Paths.StateDir = workingDirectory
	}
	manager, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	if err := os.MkdirAll(previous.Paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateSentinel := filepath.Join(previous.Paths.StateDir, "jobs_state.json")
	if err := os.WriteFile(stateSentinel, []byte(`{"jobs":[{"id":"keep"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", candidate)
	if _, err := manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, candidate)); err == nil {
		t.Fatal("Install() succeeded for a host-demonstrably unusable candidate")
	}
	unit, err := os.ReadFile(unitPath)
	if err != nil || string(unit) != previousUnit {
		t.Fatalf("the previous unit changed: read err=%v", err)
	}
	if content, err := os.ReadFile(stateSentinel); err != nil || string(content) != `{"jobs":[{"id":"keep"}]}` {
		t.Errorf("the state sentinel changed: content=%q err=%v", content, err)
	}
	calls := systemdSupervisorCalls(t, logPath)
	for _, command := range []string{"daemon-reload", "enable", "restart", "start"} {
		if count := systemdSupervisorCallsTo(calls, command, unitName); count != 0 {
			t.Errorf("preflight refusal issued %q %d times: %v", command, count, calls)
		}
	}
}

func TestSystemdPreflightWarnsRatherThanRejectingInconclusiveAccess(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousWriter) })
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	accountCopy := *account
	accountCopy.Uid = strconv.Itoa(os.Geteuid() + 1)
	opts := systemdTempInstallOptions(t, t.TempDir())
	opts.Scope = ScopeSystem
	manager := &systemdManager{scope: ScopeSystem}

	err = manager.preflight(context.Background(), opts, &accountCopy)
	if err != nil {
		t.Fatalf("preflight() = %v, want unknown access to remain unclassified", err)
	}
	if !strings.Contains(logs.String(), "inconclusive") || strings.Contains(logs.String(), "not usable") {
		t.Errorf("access warning = %q, want an inconclusive warning without a denial claim", logs.String())
	}
	path, err := manager.unitPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("inconclusive preflight wrote the registration %q: %v", path, err)
	}
	if calls := systemdSupervisorCalls(t, logPath); len(calls) != 0 {
		t.Errorf("inconclusive preflight called the supervisor: %v", calls)
	}
}

func TestSystemdPreflightLeavesUnknownFormatUnclassified(t *testing.T) {
	systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	opts := systemdTempInstallOptions(t, root)
	opts.Executable = filepath.Join(t.TempDir(), "custom-format")
	if err := os.WriteFile(opts.Executable, []byte("MZ\x90\x00"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (&systemdManager{scope: ScopeUser}).preflight(context.Background(), opts, nil); err != nil {
		t.Fatalf("preflight refused an unclassified format: %v", err)
	}
}

func TestSystemdDynamicListenUpdateUsesTheRunningProcessArgv(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	realProbe := serviceReadinessProbe
	serviceReadinessProbe = probeServiceReady
	t.Cleanup(func() { serviceReadinessProbe = realProbe })

	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	manager, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	listen := "127.0.0.1:" + strconv.Itoa(freePort(t))
	dynamicPreviousUnit := systemdDynamicListenUnit(t, unitPath, previousUnit, previous.Listen, "Environment=HF_LISTEN="+listen)
	if !strings.Contains(dynamicPreviousUnit, "${HF_LISTEN}") {
		t.Fatal("the previous unit did not retain the dynamic Listen source")
	}
	process := startSystemdRuntimeProcess(t, filepath.Join(root, "runtime"), listen, "fixture-version")
	systemdRuntimePropertyReader(t, manager, process.Process.Pid, process.Process.Pid, filepath.Join(root, "restart-count"))
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_COUNT", filepath.Join(root, "restart-count"))

	loaded, err := manager.registrationUnitConfig(context.Background())
	if err != nil {
		t.Fatalf("registrationUnitConfig() = %v", err)
	}
	if got := runArgValue(loaded.execArgs[1:], "--listen"); got != "${HF_LISTEN}" {
		t.Fatalf("loaded ExecStartEx Listen = %q, want unresolved source expression", got)
	}
	runtimeOptions, _, err := manager.runtimeRunOptions(context.Background())
	if err != nil {
		t.Fatalf("runtimeRunOptions() = %v", err)
	}
	if runtimeOptions.Listen != listen {
		t.Fatalf("runtime argv Listen = %q, want effective process address %q", runtimeOptions.Listen, listen)
	}

	if _, err := manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable)); err == nil {
		t.Fatal("Install() = nil, want the detector-A refusal of the dynamic Listen source")
	} else {
		assertSingleDropInAction(t, err, unitPath)
		if !strings.Contains(err.Error(), "is not the unit HFDesk generates") {
			t.Errorf("Install() = %q, want the cause to name the non-generated unit file", err)
		}
	}
	installed, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installed), "${HF_LISTEN}") || !strings.Contains(string(installed), "Environment=HF_LISTEN="+listen) {
		t.Errorf("the refused update changed the dynamic Listen source:\n%s", installed)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 0 {
		t.Errorf("restart count = %d, want no restart on a refusal", got)
	}
}

func TestSystemdDynamicListenDoesNotProbeAnUnobservableSourceValue(t *testing.T) {
	_ = systemdRecordingSupervisor(t)
	executable := systemdTestExecutable(t)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", executable)
	previousProbe := serviceInstallProbe
	t.Cleanup(func() { serviceInstallProbe = previousProbe })
	probeCalls := 0
	serviceInstallProbe = func(string, string) (bool, error) {
		probeCalls++
		return true, nil
	}
	manager := &systemdManager{
		scope:       ScopeUser,
		restartWait: 20 * time.Millisecond,
		readServiceProperties: func(context.Context, string) (map[string]dbus.Variant, error) {
			return map[string]dbus.Variant{"MainPID": dbus.MakeVariant(uint32(0))}, nil
		},
	}
	_, err := manager.verifyRunningExecutableWithListenSource(context.Background(), executable, "${HF_LISTEN}", "", true)
	if err == nil || !strings.Contains(err.Error(), "runtime process/Listen was not observable and stable") {
		t.Fatalf("dynamic verification error = %v, want precise unobservable-runtime-address failure", err)
	}
	if probeCalls != 0 {
		t.Errorf("readiness probe called %d times without effective process argv; unresolved source must not be treated as ready", probeCalls)
	}
	for _, tc := range []struct {
		name       string
		properties map[string]dbus.Variant
	}{
		{"missing MainPID", map[string]dbus.Variant{}},
		{"zero MainPID", map[string]dbus.Variant{"MainPID": dbus.MakeVariant(uint32(0))}},
		{"wrong MainPID type", map[string]dbus.Variant{"MainPID": dbus.MakeVariant("123")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := systemdMainPIDFromProperties(tc.properties); err == nil {
				t.Fatal("systemdMainPIDFromProperties() succeeded without a usable running process PID")
			}
		})
	}
}

func TestSystemdStaticReadinessRequiresLiveStableMatchingMainPID(t *testing.T) {
	_ = systemdRecordingSupervisor(t)
	previousReadinessProbe := serviceReadinessProbe
	serviceReadinessProbe = probeServiceReady
	t.Cleanup(func() { serviceReadinessProbe = previousReadinessProbe })
	listen := "127.0.0.1:" + strconv.Itoa(freePort(t))
	process := startSystemdRuntimeProcess(t, filepath.Join(t.TempDir(), "runtime"), listen, "fixture-version")
	executable := systemdTestExecutable(t)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", executable)
	previousProbe := serviceInstallProbe
	probeCalls := 0
	serviceInstallProbe = func(listen, expectedVersion string) (bool, error) {
		probeCalls++
		return previousProbe(listen, expectedVersion)
	}
	t.Cleanup(func() { serviceInstallProbe = previousProbe })
	managerWithPID := func(pidAtRead func(int) uint32) *systemdManager {
		reads := 0
		return &systemdManager{
			scope:       ScopeUser,
			restartWait: 20 * time.Millisecond,
			readServiceProperties: func(context.Context, string) (map[string]dbus.Variant, error) {
				reads++
				return map[string]dbus.Variant{"MainPID": dbus.MakeVariant(pidAtRead(reads))}, nil
			},
		}
	}

	for _, tc := range []struct {
		name string
		pid  uint32
	}{
		{"RemainAfterExit unit has no MainPID", 0},
		{"dead MainPID", ^uint32(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probeCalls = 0
			manager := managerWithPID(func(int) uint32 { return tc.pid })
			if _, err := manager.verifyRunningExecutable(context.Background(), executable, listen, "fixture-version"); err == nil || !strings.Contains(err.Error(), "service process could not be verified") {
				t.Fatalf("verification error = %v, want a live-process evidence failure", err)
			}
			if probeCalls != 0 {
				t.Errorf("readiness probe ran %d times without a live MainPID; the healthy bystander must not substitute", probeCalls)
			}
		})
	}

	probeCalls = 0
	if _, err := managerWithPID(func(read int) uint32 {
		if read == 1 {
			return uint32(process.Process.Pid)
		}
		return 0
	}).verifyRunningExecutable(context.Background(), executable, listen, "fixture-version"); err == nil {
		t.Fatal("readiness succeeded after MainPID disappeared during the successful HTTP probe")
	}
	if probeCalls != 1 {
		t.Errorf("readiness probe calls after process disappearance = %d, want one", probeCalls)
	}

	probeCalls = 0
	otherListen := "127.0.0.1:" + strconv.Itoa(freePort(t))
	_, err := managerWithPID(func(int) uint32 { return uint32(process.Process.Pid) }).verifyRunningExecutable(context.Background(), executable, otherListen, "fixture-version")
	if err == nil || !strings.Contains(err.Error(), "does not match the static unit Listen") {
		t.Fatalf("verification error = %v, want the static/runtime Listen mismatch", err)
	}
	if probeCalls != 0 {
		t.Errorf("readiness probe ran %d times against static Listen different from MainPID argv", probeCalls)
	}
}

// TestSystemdVerifyRejectsAnUnintendedWrappedCommand checks the negative side
// of the program comparison behind a wrapper: a wrapper that launches another
// program fails with that program named as the detail, and a wrapper shape
// whose program slot cannot be read fails while naming only the executable it
// found — the wrapper path is evidence, never the program verdict.
func TestSystemdVerifyRejectsAnUnintendedWrappedCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    []string
		wantNot []string
	}{
		{
			name:    "another program behind the wrapper",
			command: "/usr/bin/env HF_HOME=/srv/hfdesk /opt/hfdesk-other/hfdesk service run --listen 127.0.0.1:19090",
			want:    []string{`with the program "/opt/hfdesk-other/hfdesk"`},
		},
		{
			name:    "a wrapper shape without a readable program slot",
			command: "/usr/bin/env bare-word /opt/hfdesk/hfdesk service run --listen 127.0.0.1:19090",
			want:    []string{`with the executable "/usr/bin/env"`, "program slot cannot be read"},
			wantNot: []string{"with the program"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = systemdRecordingSupervisor(t)
			executable := systemdTestExecutable(t)
			t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
			t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", executable)
			t.Setenv("HFDESK_TEST_SYSTEMCTL_EXEC_START", tc.command)
			manager := &systemdManager{scope: ScopeUser, restartWait: 20 * time.Millisecond}

			_, err := manager.verifyRunningExecutable(context.Background(), executable, "127.0.0.1:19090", "fixture-version")
			if err == nil {
				t.Fatal("verifyRunningExecutable() = nil, want the wrapped command rejected")
			}
			for _, want := range append([]string{"does not run", executable}, tc.want...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("verification error = %q, want it to contain %q", err, want)
				}
			}
			for _, not := range tc.wantNot {
				if strings.Contains(err.Error(), not) {
					t.Errorf("verification error = %q, want it not to contain %q", err, not)
				}
			}
		})
	}
}

func TestSystemdReadinessAcceptsLiveScriptWrapperWithHFDeskArgs(t *testing.T) {
	_ = systemdRecordingSupervisor(t)
	root := t.TempDir()
	listen := "127.0.0.1:" + strconv.Itoa(freePort(t))
	paths := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "config", "hfdesk.json"),
		StateDir:   filepath.Join(root, "state"),
		LogDir:     filepath.Join(root, "logs"),
	}
	for _, dir := range []string{filepath.Dir(paths.ConfigFile), paths.StateDir, paths.LogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	serviceOptions := InstallOptions{Scope: ScopeUser, Executable: os.Args[0], Listen: listen, Paths: paths}
	args := append([]string{"-test.run=^TestSystemdRuntimeListenProcessFixture$"}, RunArgs(serviceOptions)...)
	wrapper := filepath.Join(root, "hfdesk-wrapper")
	script := "#!/bin/sh\n\"$@\" &\nchild=$!\ntrap 'kill \"$child\" 2>/dev/null || :; wait \"$child\" 2>/dev/null || :' TERM INT\nwait \"$child\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(wrapper, append([]string{os.Args[0]}, args...)...)
	command.Env = append(os.Environ(),
		"HFDESK_TEST_SYSTEMD_RUNTIME_PROCESS_FIXTURE=1",
		"HFDESK_TEST_SYSTEMD_RUNTIME_VERSION=fixture-version",
	)
	command.Stdout = io.Discard
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start the standalone wrapper process: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			<-done
		}
	})
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", wrapper)
	manager := &systemdManager{
		scope:       ScopeUser,
		restartWait: time.Second,
		readServiceProperties: func(context.Context, string) (map[string]dbus.Variant, error) {
			return map[string]dbus.Variant{"MainPID": dbus.MakeVariant(uint32(command.Process.Pid))}, nil
		},
	}
	verified, err := manager.verifyRunningExecutable(context.Background(), wrapper, listen, "fixture-version")
	if err != nil || !verified {
		t.Fatalf("verifyRunningExecutable() = (%t, %v), want the script wrapper's live HFDesk process; stderr: %s", verified, err, stderr.String())
	}
}

// TestSystemdWrappedRegisteredCommandKeepsTheRuntimeChecks checks the second
// fact of the verification behind a recognized wrapper: when the program slot
// of the registered command matches, the live process still has to be the
// HFDesk instance the readiness probe accepts — a stale process that reports
// another version is rejected — and a caller that verifies the command by its
// raw first token, as the restore of a wrapped registration does, still
// matches the executed wrapper path.
func TestSystemdWrappedRegisteredCommandKeepsTheRuntimeChecks(t *testing.T) {
	setup := func(t *testing.T, processVersion string) (*systemdManager, string, string) {
		t.Helper()
		_ = systemdRecordingSupervisor(t)
		listen := "127.0.0.1:" + strconv.Itoa(freePort(t))
		process := startSystemdRuntimeProcess(t, filepath.Join(t.TempDir(), "runtime"), listen, processVersion)
		executable := systemdTestExecutable(t)
		t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
		t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", executable)
		t.Setenv("HFDESK_TEST_SYSTEMCTL_EXEC_START",
			"/usr/bin/env HF_HOME=/srv/hfdesk "+executable+" service run --listen "+listen)
		manager := &systemdManager{
			scope:       ScopeUser,
			restartWait: time.Second,
			readServiceProperties: func(context.Context, string) (map[string]dbus.Variant, error) {
				return map[string]dbus.Variant{"MainPID": dbus.MakeVariant(uint32(process.Process.Pid))}, nil
			},
		}
		return manager, listen, executable
	}

	t.Run("a stale process that reports another version is rejected", func(t *testing.T) {
		manager, listen, executable := setup(t, "old-version")
		_, err := manager.verifyRunningExecutable(context.Background(), executable, listen, "fixture-version")
		if err == nil || !strings.Contains(err.Error(), "does not run the expected version") {
			t.Fatalf("verification error = %v, want the version rejection of the stale process", err)
		}
	})

	t.Run("the raw first token of the command still matches", func(t *testing.T) {
		manager, listen, _ := setup(t, "fixture-version")
		verified, err := manager.verifyRunningExecutable(context.Background(), "/usr/bin/env", listen, "fixture-version")
		if err != nil || !verified {
			t.Fatalf("verifyRunningExecutable() = (%t, %v), want the raw wrapper-path expectation of the restore path accepted; stderr error: %v", verified, err, err)
		}
	})
}

func TestSystemdRuntimeListenProcessFixture(t *testing.T) {
	if os.Getenv("HFDESK_TEST_SYSTEMD_RUNTIME_PROCESS_FIXTURE") != "1" {
		return
	}
	args := os.Args[1:]
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "service" || args[i+1] != "run" {
			continue
		}
		runOptions, err := ParseRunArgs(args[i:])
		if err != nil {
			t.Fatalf("ParseRunArgs() = %v", err)
		}
		runOptions.Version = os.Getenv("HFDESK_TEST_SYSTEMD_RUNTIME_VERSION")
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		if err := Run(ctx, runOptions); err != nil {
			t.Fatalf("Run() = %v", err)
		}
		return
	}
	t.Fatal("process argv did not contain service run")
}

func startSystemdRuntimeProcess(t *testing.T, root, listen, version string) *exec.Cmd {
	t.Helper()
	paths := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "config", "hfdesk.json"),
		StateDir:   filepath.Join(root, "state"),
		LogDir:     filepath.Join(root, "logs"),
	}
	return startSystemdServiceProcess(t, RunOptions{Paths: paths, Listen: listen}, version)
}

func startSystemdRegisteredProcess(t *testing.T, opts InstallOptions, version string) *exec.Cmd {
	t.Helper()
	for _, dir := range []string{filepath.Dir(opts.Paths.ConfigFile), opts.Paths.StateDir, opts.Paths.LogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("prepare standalone HFDesk paths: %v", err)
		}
	}
	return startSystemdServiceProcess(t, RunOptions{
		Paths:    opts.Paths,
		Listen:   opts.Listen,
		CacheDir: opts.CacheDir,
		HubDir:   opts.HubDir,
		LocalDir: opts.LocalDir,
	}, version)
}

func startSystemdServiceProcess(t *testing.T, options RunOptions, version string) *exec.Cmd {
	t.Helper()
	args := RunArgs(InstallOptions{
		Scope:      ScopeUser,
		Executable: os.Args[0],
		Listen:     options.Listen,
		Paths:      options.Paths,
		CacheDir:   options.CacheDir,
		HubDir:     options.HubDir,
		LocalDir:   options.LocalDir,
	})
	command := append([]string{"-test.run=^TestSystemdRuntimeListenProcessFixture$"}, args...)
	cmd := exec.Command(os.Args[0], command...)
	cmd.Env = append(os.Environ(),
		"HFDESK_TEST_SYSTEMD_RUNTIME_PROCESS_FIXTURE=1",
		"HFDESK_TEST_SYSTEMD_RUNTIME_VERSION="+version,
	)
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start standalone HFDesk process: %v", err)
	}
	done := make(chan struct{})
	var processErr error
	go func() {
		processErr = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("standalone HFDesk process exited before readiness: %v: %s", processErr, stderr.String())
		default:
		}
		if err := probeServiceReady(options.Listen); err == nil {
			return cmd
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("standalone HFDesk process did not serve on %s", options.Listen)
	return nil
}

func systemdDynamicListenUnit(t *testing.T, unitPath, unit, staticListen, environmentDirective string) string {
	t.Helper()
	dynamic := strings.Replace(unit, staticListen, "${HF_LISTEN}", 1)
	if dynamic == unit {
		t.Fatal("existing unit did not contain the static Listen value")
	}
	dynamic = strings.Replace(dynamic, "[Service]\n", "[Service]\n"+environmentDirective+"\n", 1)
	if err := os.WriteFile(unitPath, []byte(dynamic), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT"), []byte(dynamic), 0o644); err != nil {
		t.Fatal(err)
	}
	return dynamic
}

func systemdRuntimePropertyReader(t *testing.T, manager *systemdManager, beforeRestart, afterRestart int, restartCountPath string) {
	t.Helper()
	loadedPath := os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")
	manager.readServiceProperties = func(_ context.Context, name string) (map[string]dbus.Variant, error) {
		if name != unitName {
			return nil, fmt.Errorf("unexpected unit %q", name)
		}
		content, err := os.ReadFile(loadedPath)
		if err != nil {
			return nil, err
		}
		properties := systemdTestPropertiesFromUnit(string(content))
		pid := beforeRestart
		if countData, readErr := os.ReadFile(restartCountPath); readErr == nil {
			count, _ := strconv.Atoi(strings.TrimSpace(string(countData)))
			if count == 1 {
				pid = afterRestart
			} else if count >= 2 {
				pid = beforeRestart
			}
		}
		properties["MainPID"] = dbus.MakeVariant(uint32(pid))
		return properties, nil
	}
}
