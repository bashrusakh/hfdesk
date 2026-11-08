// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	// unitName is the supervisor visible name of the HFDesk unit.
	unitName = ServiceName + ".service"

	// unitFileMode is the permission mode of a written unit file. The
	// supervisor reads the unit as an unprivileged user, so the file must
	// be world readable.
	unitFileMode os.FileMode = 0o644

	// unitDirMode is the permission mode of the directory that holds the
	// unit file.
	unitDirMode os.FileMode = 0o755

	// systemUnitPath is the machine wide location of the unit file.
	systemUnitPath = "/etc/systemd/system/" + unitName

	// systemServiceHome is the HOME of a machine wide instance. It is a
	// machine level directory: a system scope instance never inherits the
	// home of the user who happens to run the install command.
	systemServiceHome = "/var/lib/hfdesk"
)

// systemdManager drives the systemd supervisor of HFDesk.
//
// A user scope installation is a systemd user unit: it belongs to the
// session of the user and it is the default, because a desktop or a
// background installation belongs to the user who uses it.
//
// A system scope installation is a machine wide system unit. It is an
// explicit choice and it needs root: the manager never escalates
// privileges, it fails with a clear error when the process is not root.
// The lifecycle commands of the machine wide scope need root as well: the
// unit of the machine belongs to the machine.
//
// The supervisor owns the lifecycle of the instance. HFDesk stays a normal
// foreground process: the unit is a simple service, the process is never
// forked, detached or put into the background by HFDesk itself.
//
// Booting without a login session is an explicit choice of the user or of
// the administrator. The manager therefore never enables linger on its own:
// `loginctl enable-linger` (or anything like it) is never called, so a
// user scope instance starts with the session of the user and stops with it.
//
// The manager drives the supervisor with systemctl. The supervisor is never
// called through a shell, and the manager never launches an interactive
// process: no browser and no tray icon are ever started from the service
// layer.
type systemdManager struct {
	scope Scope
	// readServiceProperties supplies typed manager data in tests. Production
	// reads the same properties through a private D-Bus connection.
	readServiceProperties func(context.Context, string) (map[string]dbus.Variant, error)
	// restartWait overrides the bounded wait for a restarted instance to
	// run the new executable. It is zero in production, where the wait is
	// the shutdown budget of the service layer; a test drives the failure
	// of a restart without waiting for the whole budget.
	restartWait time.Duration
}

// newPlatformManager returns the systemd manager of the given scope.
func newPlatformManager(scope Scope) (Manager, error) {
	switch scope {
	case ScopeUser, ScopeSystem:
		return &systemdManager{scope: scope}, nil
	default:
		return nil, fmt.Errorf("service: unknown scope %q (want %q or %q)", scope, ScopeUser, ScopeSystem)
	}
}

// unitPath returns the path of the unit file of the manager scope.
//
// A user scope unit lives in the "systemd/user" directory of the per user
// configuration directory: $XDG_CONFIG_HOME/systemd/user/hfdesk.service, or
// ~/.config/systemd/user/hfdesk.service when XDG_CONFIG_HOME is not set. A
// system scope unit lives in /etc/systemd/system.
//
// The path is resolved on every call, so the manager always describes the
// registration of the current environment.
func (m *systemdManager) unitPath() (string, error) {
	if m.scope == ScopeSystem {
		return systemUnitPath, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil || home == "" {
			return "", errors.New("service: the configuration directory of the user is unknown, the unit file path cannot be resolved")
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", unitName), nil
}

// systemctlArgs prefixes the supervisor arguments with the scope of the
// manager: a user scope installation is driven by "systemctl --user", a
// machine wide one by plain "systemctl".
func (m *systemdManager) systemctlArgs(args ...string) []string {
	if m.scope == ScopeUser {
		return append([]string{"--user"}, args...)
	}
	return args
}

// supervisorHint returns the command a user can run to inspect the unit of
// the manager scope.
func (m *systemdManager) supervisorHint() string {
	if m.scope == ScopeUser {
		return "systemctl --user"
	}
	return "systemctl"
}

// systemctlError describes a failed supervisor call. The message of the
// supervisor is kept, so the reason of a failure is never lost.
type systemctlError struct {
	args   []string
	err    error
	stderr string
}

func (e *systemctlError) Error() string {
	msg := fmt.Sprintf("service: systemctl %s failed: %v", strings.Join(e.args, " "), e.err)
	if e.stderr != "" {
		msg += ": " + e.stderr
	}
	return msg
}

func (e *systemctlError) Unwrap() error { return e.err }

// runSystemctl runs the supervisor with the given arguments and returns its
// standard output. The supervisor is never called through a shell.
func (m *systemdManager) runSystemctl(ctx context.Context, args ...string) (string, error) {
	full := m.systemctlArgs(args...)
	cmd := exec.CommandContext(ctx, "systemctl", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &systemctlError{
			args:   full,
			err:    err,
			stderr: strings.TrimSpace(stderr.String()),
		}
	}
	return stdout.String(), nil
}

// requireRoot rejects a machine wide operation that needs root when the
// process does not have it. The manager never escalates privileges itself:
// it never calls sudo, su or pkexec.
func (m *systemdManager) requireRoot(action string) error {
	return m.requireRootFor(action, os.Geteuid())
}

// requireRootFor rejects a machine wide operation when the given user id is
// not the one of root.
//
// The user id is a parameter, so the decision can be driven from both sides
// without an elevated shell: the real check passes the effective user id of
// the process, and the test passes the ids of the two cases.
//
// A user scope instance belongs to the user of the session: it is written
// into the configuration directory of the user and it is driven by the
// supervisor of that session, so it never needs root.
func (m *systemdManager) requireRootFor(action string, uid int) error {
	if m.scope != ScopeSystem {
		return nil
	}
	if uid == 0 {
		return nil
	}
	return fmt.Errorf("service: %s in the system scope requires root: run the command as root, HFDesk never escalates privileges itself", action)
}
