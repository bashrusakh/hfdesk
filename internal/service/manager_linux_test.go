// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// systemdTestOptions returns an installation for the systemd tests. The
// paths are absolute and carry no access data, so the checks of the
// rendered unit stay exact.
func systemdTestOptions(t *testing.T, scope Scope) InstallOptions {
	t.Helper()
	root := "/opt/hfdesk"
	if scope == ScopeSystem {
		root = "/srv/hfdesk"
	}
	return InstallOptions{
		Scope:      scope,
		Executable: root + "/bin/hfdesk",
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: root + "/etc/hfdesk/hfdesk.json",
			StateDir:   root + "/var/lib/hfdesk",
			LogDir:     root + "/var/log/hfdesk",
		},
		CacheDir: root + "/cache",
		HubDir:   root + "/cache/hub",
		LocalDir: root + "/models",
	}
}

// unitDirectives returns the effective lines of a unit file: the lines the
// supervisor reads, without the comments and the empty lines.
func unitDirectives(unit string) []string {
	var directives []string
	for _, line := range strings.Split(unit, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		directives = append(directives, trimmed)
	}
	return directives
}

// unitDirective returns the value of one directive of a unit file, e.g. the
// account of a "User=" directive.
//
// An empty string is returned when the unit holds no such directive, so a
// caller can tell a directive that is absent from one that carries an empty
// value.
func unitDirective(unit, key string) string {
	for _, directive := range unitDirectives(unit) {
		name, value, ok := strings.Cut(directive, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(name) == key {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// systemdTestExecutable writes a program that can be registered and returns
// its path. An installation refuses a program that does not exist, so every
// test of an installation needs one.
func systemdTestExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hfdesk")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("write the program: %v", err)
	}
	return path
}

// systemdUserSessionUsable reports whether the supervisor of the session of
// the user can be reached.
func systemdUserSessionUsable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=Version")
	out, err := cmd.CombinedOutput()
	return err == nil && strings.Contains(string(out), "Version=")
}

// waitForState waits until the supervisor reports the wanted state.
func waitForState(t *testing.T, ctx context.Context, m *systemdManager, want State) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last State
	for time.Now().Before(deadline) {
		status, err := m.Status(ctx)
		if err == nil {
			last = status.State
			if last == want {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the state of the unit = %q, want %q", last, want)
}

// systemdRecordingSupervisor replaces systemctl with a fake supervisor that
// records every call and keeps the unit file on disk separate from a
// supervisor-loaded copy. Typed D-Bus properties come from the Go test fake,
// not the lossy display string printed by systemctl.
//
// A successful daemon-reload copies the on-disk unit into the loaded copy; a
// configured reload failure leaves that copy unchanged. `show` reports the
// lifecycle state, while the loaded configuration is read from the typed Go
// fake and active state is controlled separately by HFDESK_TEST_SYSTEMCTL_ACTIVE.
// A restart fails once when HFDESK_TEST_SYSTEMCTL_RESTART_FAIL_ONCE is "1", so
// a test can drive a failed update and a successful restore.
//
// A restart reports the state it leaves behind: when
// HFDESK_TEST_SYSTEMCTL_RESTARTED names a marker file, the supervisor creates it
// on the first restart and reports HFDESK_TEST_SYSTEMCTL_ACTIVE_AFTER_RESTART
// from then on. A restart returns as soon as the unit was forked, so a test can
// drive a restart that is accepted while the unit is dead. When
// HFDESK_TEST_SYSTEMCTL_RESTART_COUNT names a file, the number of restarts is
// written to it for a test readiness probe to distinguish the failed update
// from a later recovery start.
//
// The readiness probe is replaced for the run of the test: the fake supervisor
// forks no instance, so no server of the instance answers on the registered
// address. A test that drives the probe installs one of its own after this
// helper returned.
//
// The enablement that "is-enabled" reports is HFDESK_TEST_SYSTEMCTL_ENABLED_STATE:
// "enabled" (the default) and "enabled-runtime" exit with a zero status, so the
// unit counts as enabled for boot, and every other state ("disabled", "masked",
// "masked-runtime") exits with a non-zero status, so a test can drive a repeat
// installation of a unit its administrator disabled or of a masked unit that the
// installation refuses. "static", "indirect", "alias" and "linked" exit with a
// zero status as well, the way the real supervisor answers them, so a test proves
// that the printed state and not the status decides. "masked" is a persistent
// mask, which the fake supervisor reports while a unit file may exist;
// "masked-runtime" is the state of a unit masked at runtime, which a test
// combines with an absent unit file, because the mask lives in /run and leaves
// nothing at the unit path. For both mask states "show" reports
// LoadState=masked with no active instance as well, whether or not a unit file
// exists, the way the real supervisor answers a masked unit. "not-found" is what
// a fresh machine reports for a unit it does not know. An "enable" that fails once
// HFDESK_TEST_SYSTEMCTL_ENABLE_FAIL is "1" drives the failure of an installation
// that cannot enable its registration; "enable --runtime" fails with it.
//
// The log path is returned. It is written even when the supervisor is asked
// for a property only, so a test can tell a restart from a state query.
//
// When HFDESK_TEST_SYSTEMCTL_TAMPER_UNIT names a file and
// HFDESK_TEST_SYSTEMCTL_TAMPER_MARKER a path that does not exist yet, the
// first call of the fake appends one line to that file and creates the
// marker, so the file changes exactly once between the snapshot of an
// installation and the re-read that guards the atomic write. Every other
// test leaves both variables unset and the fake never touches a unit file.
func systemdRecordingSupervisor(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
if [ -n "$HFDESK_TEST_SYSTEMCTL_LOG" ]; then
	printf '%s\n' "$*" >> "$HFDESK_TEST_SYSTEMCTL_LOG" || exit 1
fi
if [ -n "$HFDESK_TEST_SYSTEMCTL_TAMPER_UNIT" ] && [ ! -e "$HFDESK_TEST_SYSTEMCTL_TAMPER_MARKER" ] && [ -e "$HFDESK_TEST_SYSTEMCTL_TAMPER_UNIT" ]; then
	# A seam for the TOCTOU guard: the file this installation snapshotted
	# is rewritten exactly once by "another installer" before the first
	# supervisor command of the update reaches the manager.
	: > "$HFDESK_TEST_SYSTEMCTL_TAMPER_MARKER" || exit 1
	printf '# a concurrent installation wrote this file\n' >> "$HFDESK_TEST_SYSTEMCTL_TAMPER_UNIT" || exit 1
fi
if [ "$1" = "--user" ]; then
	shift
fi
case "$1" in
is-enabled)
	state="${HFDESK_TEST_SYSTEMCTL_ENABLED_STATE:-enabled}"
	echo "$state"
	case "$state" in
	enabled|enabled-runtime|static|indirect|alias|linked)
		# The supervisor exits zero for every one of these states, and only
		# "enabled" and "enabled-runtime" are enablements: the printed
		# state decides what an installation keeps.
		exit 0
		;;
	esac
	exit 1
	;;
daemon-reload)
	count=0
	if [ -n "$HFDESK_TEST_SYSTEMCTL_RELOAD_COUNT" ] && [ -e "$HFDESK_TEST_SYSTEMCTL_RELOAD_COUNT" ]; then
		count=$(cat "$HFDESK_TEST_SYSTEMCTL_RELOAD_COUNT")
	fi
	count=$((count + 1))
	if [ -n "$HFDESK_TEST_SYSTEMCTL_RELOAD_COUNT" ]; then
		printf '%s' "$count" > "$HFDESK_TEST_SYSTEMCTL_RELOAD_COUNT" || exit 1
	fi
	if [ "$HFDESK_TEST_SYSTEMCTL_DAEMON_RELOAD_FAIL_AT" = "$count" ]; then
		echo "the fake supervisor refuses daemon-reload $count" >&2
		exit 1
	fi
	unit="${XDG_CONFIG_HOME}/systemd/user/hfdesk.service"
	if [ -n "$HFDESK_TEST_SYSTEMCTL_LOADED_UNIT" ]; then
		if [ -e "$unit" ]; then
			cp "$unit" "$HFDESK_TEST_SYSTEMCTL_LOADED_UNIT" || exit 1
		else
			rm -f "$HFDESK_TEST_SYSTEMCTL_LOADED_UNIT" || exit 1
		fi
	fi
	exit 0
	;;
enable)
	if [ "$HFDESK_TEST_SYSTEMCTL_ENABLE_FAIL" = "1" ]; then
		echo "the fake supervisor refuses the enable" >&2
		exit 1
	fi
	exit 0
	;;
show)
	case "${HFDESK_TEST_SYSTEMCTL_ENABLED_STATE:-enabled}" in
	masked|masked-runtime)
		# A masked unit reports LoadState=masked with no active instance
		# whether the mask is a link to /dev/null at the unit path or lives
		# in /run and leaves no file there, the way the real supervisor
		# answers both mask kinds.
		echo "LoadState=masked"
		echo "ActiveState=inactive"
		echo "SubState=dead"
		exit 0
		;;
	esac
	active="${HFDESK_TEST_SYSTEMCTL_ACTIVE:-inactive}"
	if [ -n "$HFDESK_TEST_SYSTEMCTL_RESTARTED" ] && [ -e "$HFDESK_TEST_SYSTEMCTL_RESTARTED" ] && [ -n "$HFDESK_TEST_SYSTEMCTL_ACTIVE_AFTER_RESTART" ]; then
		active="$HFDESK_TEST_SYSTEMCTL_ACTIVE_AFTER_RESTART"
	fi
	sub=dead
	if [ "$active" = "active" ]; then
		sub=running
	fi
	if [ "$active" = "activating" ]; then
		sub=start-pre
	fi
	unit="${XDG_CONFIG_HOME}/systemd/user/hfdesk.service"
	if [ -n "$HFDESK_TEST_SYSTEMCTL_LOADED_UNIT" ]; then
		if [ ! -e "$HFDESK_TEST_SYSTEMCTL_LOADED_UNIT" ] && [ -z "$HFDESK_TEST_SYSTEMCTL_EXECUTABLE" ]; then
			echo "LoadState=not-found"
			echo "ActiveState=inactive"
			echo "SubState=dead"
			exit 0
		fi
		if [ -e "$HFDESK_TEST_SYSTEMCTL_LOADED_UNIT" ]; then
			unit="$HFDESK_TEST_SYSTEMCTL_LOADED_UNIT"
		fi
	fi
	if [ ! -e "$unit" ] && [ -z "$HFDESK_TEST_SYSTEMCTL_EXECUTABLE" ]; then
		echo "LoadState=not-found"
		echo "ActiveState=inactive"
		echo "SubState=dead"
		exit 0
	fi
	echo "LoadState=loaded"
	echo "ActiveState=$active"
	echo "SubState=$sub"
	executable="$HFDESK_TEST_SYSTEMCTL_EXECUTABLE"
	if [ -n "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT" ] && [ -e "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT" ]; then
		restart_count=$(cat "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT")
		if [ "$restart_count" = "2" ] && [ -n "$HFDESK_TEST_SYSTEMCTL_EXECUTABLE_AFTER_SECOND_RESTART" ]; then
			executable="$HFDESK_TEST_SYSTEMCTL_EXECUTABLE_AFTER_SECOND_RESTART"
		fi
	fi
	if [ -n "$HFDESK_TEST_SYSTEMCTL_EXEC_START" ]; then
		# The registered command as systemd reports it: the first word is
		# the executed path=, the whole vector the argv[]= the program
		# slot is read from.
		command="$HFDESK_TEST_SYSTEMCTL_EXEC_START"
		path="${command%% *}"
		printf 'ExecStart={ path=%s ; argv[]=%s ; ignore_errors=no }\n' "$path" "$command"
	elif [ -n "$executable" ]; then
		printf 'ExecStart={ path=%s }\n' "$executable"
	fi
	exit 0
	;;
restart)
	if [ -n "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT" ]; then
		count=0
		if [ -e "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT" ]; then
			count=$(cat "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT")
		fi
		count=$((count + 1))
		printf '%s' "$count" > "$HFDESK_TEST_SYSTEMCTL_RESTART_COUNT" || exit 1
	fi
	if [ "$HFDESK_TEST_SYSTEMCTL_RESTART_FAIL_ONCE" = "1" ] && [ ! -e "$HFDESK_TEST_SYSTEMCTL_RESTART_MARKER" ]; then
		: > "$HFDESK_TEST_SYSTEMCTL_RESTART_MARKER"
		echo "the fake supervisor refuses the restart" >&2
		exit 1
	fi
	if [ -n "$HFDESK_TEST_SYSTEMCTL_RESTARTED" ]; then
		: > "$HFDESK_TEST_SYSTEMCTL_RESTARTED" || exit 1
	fi
	exit 0
	;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake supervisor: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HFDESK_TEST_SYSTEMCTL_LOG", logPath)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT", filepath.Join(dir, "loaded-unit"))
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RELOAD_COUNT", filepath.Join(dir, "reload-count"))
	// The fake supervisor forks no instance, so the readiness probe of the
	// service layer is replaced for the run of the test. A test that drives
	// the probe installs its own after this helper returned.
	previous := serviceReadinessProbe
	serviceReadinessProbe = func(string) error { return nil }
	t.Cleanup(func() { serviceReadinessProbe = previous })
	return logPath
}

// systemdSupervisorCalls returns the lines the fake supervisor recorded.
func systemdSupervisorCalls(t *testing.T, logPath string) []string {
	t.Helper()
	content, err := os.ReadFile(logPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the supervisor log: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
}

// systemdSupervisorCallsTo counts the calls of the fake supervisor that equal
// the given command. The scope prefix of the manager is ignored, so a test
// names the command the way the manager issues it.
func systemdSupervisorCallsTo(calls []string, command ...string) int {
	want := strings.Join(command, " ")
	count := 0
	for _, call := range calls {
		if strings.TrimPrefix(call, "--user ") == want {
			count++
		}
	}
	return count
}

// systemdSupervisorCallIndex returns the index of the first call of the fake
// supervisor that equals the given command, and -1 when it was never called.
// The scope prefix of the manager is ignored.
func systemdSupervisorCallIndex(calls []string, command ...string) int {
	want := strings.Join(command, " ")
	for i, call := range calls {
		if strings.TrimPrefix(call, "--user ") == want {
			return i
		}
	}
	return -1
}
