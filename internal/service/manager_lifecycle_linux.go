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
	"strings"
)

// Uninstall removes the registration of the service.
//
// It removes the registration only: the config file, the job history, the
// logs, the models and the cache of the instance are never touched. An
// absent registration is a clean success and the supervisor is left alone.
func (m *systemdManager) Uninstall(ctx context.Context) error {
	path, err := m.unitPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := m.requireRoot("an uninstallation"); err != nil {
		return err
	}

	// Stop the unit and drop it from the boot of the scope. A unit the
	// supervisor does not know is not an error: the registration is
	// already gone.
	if _, err := m.runSystemctl(ctx, "disable", "--now", unitName); err != nil {
		if !isUnitAbsent(err) {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("service: remove the unit file %q: %w", path, err)
	}
	if _, err := m.runSystemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	return nil
}

// lifecycleKind tells the guard of a lifecycle command what the command
// needs from the registration of the service.
type lifecycleKind int

const (
	// lifecycleStart is a command that brings the service up, a start or a
	// restart: it needs a registration, so an absent one is a refusal.
	lifecycleStart lifecycleKind = iota
	// lifecycleStop is a command that stops the service: a service that is
	// not registered does not run either, so an absent registration is a
	// stable success.
	lifecycleStop
)

// lifecycleOutcome is the answer of the guard of a lifecycle command.
type lifecycleOutcome int

const (
	// lifecycleRun means the command may touch the supervisor.
	lifecycleRun lifecycleOutcome = iota
	// lifecycleAlreadyReached means the wanted state of the command holds
	// already: the service is not registered, so it does not run either,
	// and the command is a stable success that must not touch the
	// supervisor.
	lifecycleAlreadyReached
)

// Start starts the registered unit. Starting a unit that already runs is a
// stable success, not an error.
//
// A unit that is not registered cannot be started: the command reports that
// the service is not installed.
func (m *systemdManager) Start(ctx context.Context) error {
	installed, err := m.installed()
	if err != nil {
		return err
	}
	if _, err := m.requireLifecycle(lifecycleStart, installed, os.Geteuid(), "a start"); err != nil {
		return err
	}
	if _, err := m.runSystemctl(ctx, "start", unitName); err != nil {
		// The supervisor may refuse a start of a unit that is already
		// active. That is the state the caller asked for.
		if state, ok := m.currentState(ctx); ok && (state == StateRunning || state == StateStarting) {
			return nil
		}
		return err
	}
	return nil
}

// Stop stops the registered unit. Stopping a unit that is already stopped is
// a stable success, not an error, and so is stopping a unit that is not
// registered at all: it does not run either.
func (m *systemdManager) Stop(ctx context.Context) error {
	installed, err := m.installed()
	if err != nil {
		return err
	}
	outcome, err := m.requireLifecycle(lifecycleStop, installed, os.Geteuid(), "a stop")
	if err != nil {
		return err
	}
	if outcome == lifecycleAlreadyReached {
		// The unit is not registered, so it does not run either: the
		// state the caller asked for already holds.
		return nil
	}
	if _, err := m.runSystemctl(ctx, "stop", unitName); err != nil {
		// A unit the supervisor does not know is not running, so the
		// wanted state is already reached.
		if isUnitAbsent(err) {
			return nil
		}
		if state, ok := m.currentState(ctx); ok && state == StateStopped {
			return nil
		}
		return err
	}
	return nil
}

// Restart stops and starts the registered unit.
//
// A unit that is not registered cannot be restarted: the command reports
// that the service is not installed.
func (m *systemdManager) Restart(ctx context.Context) error {
	installed, err := m.installed()
	if err != nil {
		return err
	}
	if _, err := m.requireLifecycle(lifecycleStart, installed, os.Geteuid(), "a restart"); err != nil {
		return err
	}
	if _, err := m.runSystemctl(ctx, "restart", unitName); err != nil {
		return err
	}
	return nil
}

// requireLifecycle reports what a lifecycle command of the manager may do.
//
// The registration of the service comes first: it needs no privilege and it
// gives the more useful answer. A stop of a service that is not registered
// reaches its wanted state already, because such a service does not run
// either.
//
// The elevation requirement comes second: a machine wide unit belongs to
// the machine, so a lifecycle command of that scope needs root. A user scope
// instance belongs to the user of the session and needs no elevation.
//
// The state of the registration and the effective user id are parameters,
// so the decision can be driven from both sides without an elevated shell:
// the command passes the state of the file system and the id of the process,
// and the test passes the states of the cases.
func (m *systemdManager) requireLifecycle(kind lifecycleKind, installed bool, uid int, action string) (lifecycleOutcome, error) {
	if !installed {
		if kind == lifecycleStop {
			return lifecycleAlreadyReached, nil
		}
		return lifecycleRun, fmt.Errorf("service: %s is not installed", unitName)
	}
	if err := m.requireRootFor(action, uid); err != nil {
		return lifecycleRun, err
	}
	return lifecycleRun, nil
}

// installed reports whether the unit file of the manager scope is there.
//
// The registration is read from the file system, so the answer needs no
// privilege: a caller that is not elevated still learns whether the
// service is registered.
func (m *systemdManager) installed() (bool, error) {
	path, err := m.unitPath()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// currentState queries the supervisor for the state of the unit. The second
// result is false when the supervisor could not be reached.
func (m *systemdManager) currentState(ctx context.Context) (State, bool) {
	out, err := m.runSystemctl(ctx, "show", unitName,
		"--property=LoadState", "--property=ActiveState", "--property=SubState")
	if err != nil {
		return StateUnknown, false
	}
	return parseSystemdShow(out).State, true
}

// isUnitAbsent reports whether a supervisor error means that the unit is
// not known to the supervisor. Such an error is not a failure of the
// operation: the wanted state (no unit) is already reached.
func isUnitAbsent(err error) bool {
	var svcErr *systemctlError
	if !errors.As(err, &svcErr) {
		return false
	}
	message := strings.ToLower(svcErr.stderr)
	for _, marker := range []string{
		"not loaded",
		"does not exist",
		"no such file",
		"not found",
		"unknown unit",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
