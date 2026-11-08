// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSystemdLifecycleOfAnAbsentUnit checks the lifecycle commands against a
// service that is not registered: the stop is a stable success, and the
// start and the restart report that the service is not installed.
//
// The test never touches a real installation: the unit file of the user
// scope lives in a temporary configuration directory, and the test skips
// itself when the machine holds a unit of the machine scope.
func TestSystemdLifecycleOfAnAbsentUnit(t *testing.T) {
	t.Run("the user scope", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		m := &systemdManager{scope: ScopeUser}
		path, err := m.unitPath()
		if err != nil {
			t.Fatalf("unitPath() = %v, want nil", err)
		}
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("the unit file %q exists, the test needs a service that is not registered", path)
		}

		ctx := context.Background()
		// A service that is not registered does not run either: the
		// state the caller asked for already holds.
		if err := m.Stop(ctx); err != nil {
			t.Errorf("Stop() = %v, want nil for a service that is not registered", err)
		}
		// A service that is not registered cannot be started, and the
		// reason is reported in every shell.
		err = m.Start(ctx)
		if err == nil {
			t.Fatal("Start() = nil, want an error for a service that is not registered")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("Start() = %v, want an error that names the missing registration", err)
		}
		// A restart cannot start the service either.
		err = m.Restart(ctx)
		if err == nil {
			t.Fatal("Restart() = nil, want an error for a service that is not registered")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("Restart() = %v, want an error that names the missing registration", err)
		}
	})

	t.Run("the machine-wide scope", func(t *testing.T) {
		m := &systemdManager{scope: ScopeSystem}
		path, err := m.unitPath()
		if err != nil {
			t.Fatalf("unitPath() = %v, want nil", err)
		}
		if _, err := os.Stat(path); err == nil {
			t.Skipf("the machine has a unit at %q: the test does not touch it", path)
		}

		ctx := context.Background()
		if err := m.Stop(ctx); err != nil {
			t.Errorf("Stop() = %v, want nil for a service that is not registered", err)
		}
		err = m.Start(ctx)
		if err == nil {
			t.Fatal("Start() = nil, want an error for a service that is not registered")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("Start() = %v, want an error that names the missing registration", err)
		}
		err = m.Restart(ctx)
		if err == nil {
			t.Fatal("Restart() = nil, want an error for a service that is not registered")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("Restart() = %v, want an error that names the missing registration", err)
		}
	})
}

// TestSystemdMachineWideLifecycleNeedsRoot checks the elevation requirement
// of the machine-wide scope for the lifecycle commands.
//
// A unit of the machine belongs to the machine, so starting, stopping and
// restarting it needs root. The user scope belongs to the user of the
// session and never needs an elevation.
//
// The state of the registration and the user id are parameters of the
// guard, so the test drives every case without an elevated shell.
func TestSystemdMachineWideLifecycleNeedsRoot(t *testing.T) {
	// A user id that is not the one of root.
	const unprivileged = 501

	cases := []struct {
		name   string
		kind   lifecycleKind
		action string
	}{
		{name: "a start", kind: lifecycleStart, action: "a start"},
		{name: "a stop", kind: lifecycleStop, action: "a stop"},
		{name: "a restart", kind: lifecycleStart, action: "a restart"},
	}
	for _, tc := range cases {
		t.Run("the machine-wide scope refuses "+tc.name+" without root", func(t *testing.T) {
			m := &systemdManager{scope: ScopeSystem}

			_, err := m.requireLifecycle(tc.kind, true, unprivileged, tc.action)
			if err == nil {
				t.Fatal("requireLifecycle() = nil, want the refusal of a shell that is not elevated")
			}
			want := fmt.Sprintf("service: %s in the system scope requires root: run the command as root, HFDesk never escalates privileges itself", tc.action)
			if err.Error() != want {
				t.Errorf("requireLifecycle() = %q, want %q", err, want)
			}
		})

		t.Run("the machine-wide scope accepts "+tc.name+" with root", func(t *testing.T) {
			m := &systemdManager{scope: ScopeSystem}

			if _, err := m.requireLifecycle(tc.kind, true, 0, tc.action); err != nil {
				t.Errorf("requireLifecycle() = %v, want nil for an elevated shell", err)
			}
		})

		t.Run("the user scope is not refused for "+tc.name, func(t *testing.T) {
			m := &systemdManager{scope: ScopeUser}

			if _, err := m.requireLifecycle(tc.kind, true, unprivileged, tc.action); err != nil {
				t.Errorf("requireLifecycle() = %v, want nil for a user scope instance", err)
			}
		})
	}

	t.Run("the check of the process uses the effective user id of the process", func(t *testing.T) {
		m := &systemdManager{scope: ScopeSystem}

		err := m.requireRoot("a start")
		forced := m.requireRootFor("a start", os.Geteuid())
		switch {
		case err == nil && forced == nil:
		case err != nil && forced != nil && err.Error() == forced.Error():
		default:
			t.Errorf("requireRoot() = %v, want the decision of requireRootFor(%d) = %v", err, os.Geteuid(), forced)
		}
	})
}

// TestSystemdStopOfAnAbsentUnitReachesItsState checks the answer of the
// guard for a service that is not registered: a stop reaches its wanted
// state already, so it is a stable success that must not touch the
// supervisor, and a start and a restart are refused.
func TestSystemdStopOfAnAbsentUnitReachesItsState(t *testing.T) {
	m := &systemdManager{scope: ScopeSystem}

	outcome, err := m.requireLifecycle(lifecycleStop, false, 501, "a stop")
	if err != nil {
		t.Fatalf("requireLifecycle() = %v, want nil for a stop of a service that is not registered", err)
	}
	if outcome != lifecycleAlreadyReached {
		t.Errorf("requireLifecycle() = %d, want %d", outcome, lifecycleAlreadyReached)
	}

	for _, action := range []string{"a start", "a restart"} {
		_, err := m.requireLifecycle(lifecycleStart, false, 501, action)
		if err == nil {
			t.Fatalf("requireLifecycle() = nil, want the refusal of %s for a service that is not registered", action)
		}
		want := fmt.Sprintf("service: %s is not installed", unitName)
		if err.Error() != want {
			t.Errorf("requireLifecycle() = %q, want %q", err, want)
		}
	}
}

// TestSystemdAbsentUnitComesBeforeTheElevationRequirement checks the order
// of the two checks of a lifecycle command.
//
// The registration of the service comes first: it needs no privilege and it
// gives the more useful answer. A stop of a service that is not registered
// therefore stays a stable success even when the shell is not elevated, and
// a start of it still names the missing registration instead of the
// elevation requirement.
func TestSystemdAbsentUnitComesBeforeTheElevationRequirement(t *testing.T) {
	m := &systemdManager{scope: ScopeSystem}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Skipf("the machine has a unit at %q: the test does not touch it", path)
	}

	// The elevation requirement refuses a shell that is not elevated.
	if err := m.requireRootFor("a stop", 501); err == nil {
		t.Fatal("requireRootFor() = nil, want the refusal of a shell that is not elevated")
	}

	// The stop of a service that is not registered is a stable success:
	// the registration is checked first, so the elevation requirement is
	// not even looked at.
	if err := m.Stop(context.Background()); err != nil {
		t.Errorf("Stop() = %v, want nil for a service that is not registered", err)
	}

	// The start of a service that is not registered names the missing
	// registration, not the elevation requirement.
	err = m.Start(context.Background())
	if err == nil {
		t.Fatal("Start() = nil, want an error for a service that is not registered")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("Start() = %v, want an error that names the missing registration", err)
	}
	if strings.Contains(err.Error(), "requires root") {
		t.Errorf("Start() = %v, want the missing registration and not the elevation requirement", err)
	}
}

// TestSystemdUserScopeLifecycleNeedsNoRoot checks that the lifecycle
// commands of a user scope instance are never refused for the elevation
// requirement: a user unit belongs to the user of the session.
//
// The supervisor of the test is a fake one, so the test never touches the
// supervisor of the machine, and the registration of the test lives in a
// temporary configuration directory.
func TestSystemdUserScopeLifecycleNeedsNoRoot(t *testing.T) {
	systemdFakeSupervisor(t)
	t.Setenv("HFDESK_TEST_SUPERVISOR", "ok")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := &systemdManager{scope: ScopeUser}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v, want nil", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the unit directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatalf("write the unit file: %v", err)
	}

	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Errorf("Start() = %v, want nil for a user scope instance", err)
	}
	if err := m.Stop(ctx); err != nil {
		t.Errorf("Stop() = %v, want nil for a user scope instance", err)
	}
	if err := m.Restart(ctx); err != nil {
		t.Errorf("Restart() = %v, want nil for a user scope instance", err)
	}
}
