// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Uninstall removes the registration of the instance, and only that: the config
// file, the jobs, the history, the logs, the models and the cache are never
// touched, and an absent registration is a clean success.
//
// The existence of the registration is checked before the elevation
// requirement, so an absent registration is a clean success in every shell and
// the requirement applies only when there is something to remove.
func (m *scmManager) Uninstall(ctx context.Context) error {
	state, err := scmRegistrationStateOf()
	if err != nil {
		return err
	}
	return m.uninstall(ctx, state, isElevatedProcess())
}

// uninstall removes the registration of the instance after the existence
// of the registration was established.
//
// The state of the registration and the state of the token of the process
// are parameters, so the decision of the uninstallation can be driven from
// both sides without an elevated shell: the service control manager
// accepts a machine-wide registration only from an administrator, so a
// test cannot create one without an elevated shell.
func (m *scmManager) uninstall(ctx context.Context, state scmRegistrationState, elevated bool) error {
	switch scmUninstallActionFor(state) {
	case scmUninstallNothing:
		// There is no registration: the uninstallation is already done.
		return nil
	case scmUninstallElevate:
		// The registration could not be read: the shell of the caller
		// has no rights on it. The elevation requirement is the
		// truthful reason, so it is reported instead of the access
		// denied error of the platform.
		if err := requireElevatedFor("an uninstallation", elevated); err != nil {
			return err
		}
		return scmElevationRequired("an uninstallation")
	case scmUninstallRemove:
		// The registration has to be removed: the elevation requirement
		// is checked before the removal.
	}

	// The elevation requirement is checked before the registration is
	// removed: a permission error of the machine is a much less readable
	// answer than the real reason.
	if err := requireElevatedFor("an uninstallation", elevated); err != nil {
		return err
	}
	manager, handle, ok, err := m.openRegistrationPrivileged("an uninstallation")
	if err != nil {
		return err
	}
	if !ok {
		// The registration is gone between the check and the removal:
		// the uninstallation is already done.
		return nil
	}
	defer manager.Disconnect()
	defer handle.Close()

	// A running instance is stopped before the registration is removed:
	// the service control manager refuses to delete a service that is
	// still running.
	if _, err := m.stopService(ctx, handle); err != nil {
		return err
	}
	if err := handle.Delete(); err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			// The registration is marked for the deletion already.
			return nil
		}
		return fmt.Errorf("service: remove the registration of %s: %w", ServiceName, err)
	}
	return nil
}

// Start starts the registered instance.
//
// Starting an instance that runs already is a stable success: the state
// the caller asked for already holds.
//
// The existence of the registration is looked up with a read-only
// connection, so the answer does not depend on the rights of the shell: an
// instance that is not registered is reported as such in every shell.
func (m *scmManager) Start(ctx context.Context) error {
	manager, handle, ok, err := m.openRegistration("a start")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("service: %s is not installed", ServiceName)
	}
	defer manager.Disconnect()
	defer handle.Close()

	if err := handle.Start(); err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return nil
		}
		return scmRefused("a start", fmt.Errorf("service: start %s: %w", ServiceName, err))
	}
	return nil
}

// Stop stops the registered instance.
//
// Stopping an instance that is stopped already is a stable success, and so
// is stopping an instance that is not registered at all: it does not run
// either.
func (m *scmManager) Stop(ctx context.Context) error {
	manager, handle, ok, err := m.openRegistration("a stop")
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	defer manager.Disconnect()
	defer handle.Close()

	_, err = m.stopService(ctx, handle)
	return err
}

// Restart stops and starts the registered instance.
//
// The instance is stopped first: the graceful shutdown of the server
// completes before a new process is started, so the two instances never
// compete for the bind address.
func (m *scmManager) Restart(ctx context.Context) error {
	manager, handle, ok, err := m.openRegistration("a restart")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("service: %s is not installed", ServiceName)
	}
	defer manager.Disconnect()
	defer handle.Close()

	if _, err := m.stopService(ctx, handle); err != nil {
		return err
	}
	if err := handle.Start(); err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return nil
		}
		return scmRefused("a restart", fmt.Errorf("service: start %s: %w", ServiceName, err))
	}
	return nil
}

// Status reports the state of the registered instance.
//
// The status reads the registration with the rights a status needs only, which
// the default descriptor grants to the interactive users of the machine, so a
// normal shell reads it without elevation. The description comes from the
// registration, and a registered executable that is gone is reported as broken.
// The registration has no file of its own: the path is the registry key of the
// service.
func (m *scmManager) Status(ctx context.Context) (Status, error) {
	facts, err := scmLookupRegistration(ServiceName)
	if err != nil {
		return scmBaseStatus(m.scope), err
	}
	return scmStatusFrom(m.scope, facts)
}

// scmBaseStatus returns the status of the instance without the facts of a
// registration.
//
// The status names the instance and its registration even when the
// registration could not be read: the reader sees which instance the
// answer is about.
func scmBaseStatus(scope Scope) Status {
	return Status{
		State:    StateNotInstalled,
		Scope:    scope,
		Name:     ServiceName,
		UnitPath: scmRegistrationPath,
	}
}

// scmRegistrationFacts are the facts of a registration the status needs.
//
// The facts are read with the rights a status needs only, so a normal
// shell can read them.
type scmRegistrationFacts struct {
	// present reports whether the instance is registered at all.
	present bool
	// state is the state the service control manager reports.
	state svc.State
	// exitCode is the exit code the instance ended with.
	exitCode uint32
	// processID is the process of the running instance, zero when the
	// service control manager reports none.
	processID uint32
	// commandLine is the command line the registration carries.
	commandLine string
}

// scmLookupRegistration reads the facts of the registration of the given
// name with the rights a status needs.
//
// The service control manager is opened with the right to connect only and
// the registration with the right to query its state and the right to
// query its configuration. The privileged connection of the platform asks
// for every right instead, which a normal shell does not have.
//
// An absent registration is not an error: it is the state the caller asked
// about, and it is reported as such. A registration the caller may not
// look at is reported as the elevation requirement it is: the raw access
// denied error of the platform does not tell the user what to do.
//
// The variable below is a zero-behavior indirection: the installation-level
// tests drive the merge wiring with a registration fixture instead of
// registering a damaged service in the control manager of the test machine.
var scmLookupRegistration = readSCMRegistration

func readSCMRegistration(name string) (scmRegistrationFacts, error) {
	facts := scmRegistrationFacts{}

	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return facts, scmLookupError("connect to the service control manager", err)
	}
	defer windows.CloseServiceHandle(manager)

	pointer, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return facts, fmt.Errorf("service: the name of the service cannot be read: %w", err)
	}
	handle, err := windows.OpenService(manager, pointer, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			// The instance is not registered: that is the state the
			// caller asked about, not a failure.
			return facts, nil
		}
		return facts, scmLookupError(fmt.Sprintf("open the registration of %s", name), err)
	}
	defer windows.CloseServiceHandle(handle)

	var query windows.SERVICE_STATUS_PROCESS
	var needed uint32
	if err := windows.QueryServiceStatusEx(handle, windows.SC_STATUS_PROCESS_INFO, (*byte)(unsafe.Pointer(&query)), uint32(unsafe.Sizeof(query)), &needed); err != nil {
		return facts, scmLookupError(fmt.Sprintf("query the state of %s", name), err)
	}
	commandLine, err := queryServiceCommandLine(handle)
	if err != nil {
		return facts, scmLookupError(fmt.Sprintf("read the registration of %s", name), err)
	}

	facts.present = true
	facts.state = svc.State(query.CurrentState)
	facts.exitCode = query.Win32ExitCode
	// The process of the running instance is part of the state: an
	// installation that restarts an instance has to know the process it
	// replaces before it changes the registration.
	facts.processID = query.ProcessId
	facts.commandLine = commandLine
	return facts, nil
}

// scmLookupError reports the reason of a failed read-only lookup.
//
// A refusal of the platform because the shell has no rights on the
// registration is reported as the elevation requirement it is: the raw
// access denied error of the platform does not tell the user what to do.
func scmLookupError(what string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return scmElevationRequired("a status")
	}
	return fmt.Errorf("service: %s: %w", what, err)
}

// scmStatusFrom builds the status of the instance out of the facts of a
// registration.
//
// The mapping is a pure function of the facts, so it can be checked
// without a registration of its own: reading the command line of a
// registration needs a real service in the service control manager, which
// a test cannot create without an elevated shell.
//
// An absent registration is reported as not installed: the registration is
// not there, so there is nothing to describe.
//
// An instance that crashed is reported as failed: the service control
// manager has no state of its own for a failure and reports a crashed
// instance as stopped, so the exit code is what tells a failed instance
// from a cleanly stopped one.
//
// An instance that never ran is reported as stopped: the manager reports
// the code of a service that was never started for it, and that code is no
// failure of the instance.
//
// The running fact of the state is carried as Status.Running next to the
// diagnosis: a registration whose program was deleted is reported as broken
// while the process the manager started keeps serving.
func scmStatusFrom(scope Scope, facts scmRegistrationFacts) (Status, error) {
	status := scmBaseStatus(scope)
	if !facts.present {
		status.Detail = fmt.Sprintf("%s is not registered", ServiceName)
		return status, nil
	}

	registration, err := parseSCMCommandLine(facts.commandLine)
	if err != nil {
		return status, err
	}
	applyRegistration(&status, registration)

	// The running fact is read from the same supervisor answer as the state,
	// before the registration is judged: the service control manager keeps a
	// process running after its program was deleted, so a broken
	// registration can still serve.
	status.Running = scmInstanceRan(true, facts.state)

	if broken, detail := scmBrokenRegistration(status.Executable); broken {
		status.State = StateBroken
		if status.Running {
			detail += brokenRunningDetail
		}
		status.Detail = detail
		return status, nil
	}

	status.State = scmState(facts.state)
	if status.State == StateStopped && scmFailedExit(facts.exitCode) {
		// The service control manager has no state of its own for a
		// failure: an instance that crashed is reported as stopped
		// there, like an instance that ended cleanly. The exit code is
		// what tells the two apart, so a stopped instance that ended
		// with the code of a failed run is reported as failed, the way
		// the managers of the other platforms report the same crash.
		status.State = StateFailed
		status.Detail = fmt.Sprintf("the instance exited with the code %d", facts.exitCode)
	}
	return status, nil
}

// scmFailedExit reports whether the exit code of the instance is the code of
// a failed run.
//
// The service control manager has no state of its own for a failure: it
// reports a crashed instance as stopped, like a cleanly stopped one, so the
// exit code is what tells the two apart.
//
// A service that never ran carries the code of a service that was never
// started (ERROR_SERVICE_NEVER_STARTED): the manager reports it for every
// service that was never started, and it is no failure of the instance. The
// instance is stopped, the way a unit that never ran is reported as stopped
// by the manager of the other platforms.
func scmFailedExit(exitCode uint32) bool {
	return exitCode != 0 && exitCode != uint32(windows.ERROR_SERVICE_NEVER_STARTED)
}

// scmBrokenRegistration reports whether the registration points at an
// executable that is gone and describes the damage.
//
// A registration that points at a program that does not exist anymore is
// broken: the instance can never run again until it is installed again. A
// registration that carries no executable at all is not broken: there is no
// program that could be missing.
//
// The decision is a pure function of the registered path, so it can be
// checked without a registration of its own: reading the command line of a
// registration needs a real service in the service control manager, which a
// test cannot create without an elevated shell.
func scmBrokenRegistration(executable string) (bool, string) {
	if executable == "" {
		return false, ""
	}
	if _, err := os.Stat(executable); err != nil {
		return true, fmt.Sprintf("the registered executable %s does not exist", executable)
	}
	return false, ""
}

// applyRegistration fills the status with the facts of the registration.
//
// The cache root of the status is the effective one: the cache directory of
// the config file of the instance wins over the registered one, because the
// user changes it in the settings of the instance. A registration that
// defines no cache root at all reports none.
func applyRegistration(status *Status, registration scmRegistration) {
	status.Executable = registration.executable
	status.Listen = registration.listen()
	status.Paths = registration.paths()
	status.CacheDir = EffectiveCacheDir(status.Paths.ConfigFile, registration.cacheDir())
}

// scmRegistrationState describes what the service control manager knows
// about the registration of the instance.
type scmRegistrationState int

const (
	// scmRegistrationAbsent means the instance is not registered.
	scmRegistrationAbsent scmRegistrationState = iota
	// scmRegistrationPresent means the instance is registered.
	scmRegistrationPresent
	// scmRegistrationUnreadable means the caller may not look at the
	// registration, so it cannot tell whether the instance is
	// registered.
	scmRegistrationUnreadable
)

// scmRegistrationStateOf reports what the service control manager knows
// about the registration of the instance.
func scmRegistrationStateOf() (scmRegistrationState, error) {
	return scmRegistrationStateOfName(ServiceName)
}

// scmRegistrationStateOfName reports what the service control manager
// knows about the registration of the given name.
//
// The check opens the service control manager with the right to connect
// only and the registration with the right to query its state only: an
// ordinary shell can read the registration of the instance, so it can tell
// whether the instance is installed at all. The manager of the platform
// opens the service control manager with every right instead, which an
// ordinary shell does not have.
//
// A registration the caller may not look at is not an absent registration:
// the caller cannot tell, and the truth is that the operation needs the
// rights of the machine either way.
func scmRegistrationStateOfName(name string) (scmRegistrationState, error) {
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		state, reason := scmClassifyRegistrationError(err)
		if reason != nil {
			return state, fmt.Errorf("service: connect to the service control manager: %w", reason)
		}
		return state, nil
	}
	defer windows.CloseServiceHandle(manager)

	pointer, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return scmRegistrationUnreadable, fmt.Errorf("service: the name of the service cannot be read: %w", err)
	}
	handle, err := windows.OpenService(manager, pointer, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		state, reason := scmClassifyRegistrationError(err)
		if reason != nil {
			return state, fmt.Errorf("service: open the registration of %s: %w", name, reason)
		}
		return state, nil
	}
	windows.CloseServiceHandle(handle)
	return scmRegistrationPresent, nil
}

// scmClassifyRegistrationError classifies the error of an existence check.
//
// An absent registration is not an error: it is the state the caller asked
// about. A registration the caller may not look at is not an absent
// registration either: the caller cannot tell, and the truth is that the
// operation needs the rights of the machine either way.
//
// The second result is the error the caller reports. An access denied
// error is not reported as it is: the elevation requirement is the
// truthful reason, so the raw error of the platform is dropped.
func scmClassifyRegistrationError(err error) (scmRegistrationState, error) {
	switch {
	case err == nil:
		return scmRegistrationPresent, nil
	case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		return scmRegistrationAbsent, nil
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return scmRegistrationUnreadable, nil
	default:
		return scmRegistrationUnreadable, err
	}
}

// scmUninstallAction is what an uninstallation does with the registration
// of the instance.
type scmUninstallAction int

const (
	// scmUninstallNothing means there is no registration to remove.
	scmUninstallNothing scmUninstallAction = iota
	// scmUninstallRemove means the registration has to be removed.
	scmUninstallRemove
	// scmUninstallElevate means the caller has to run the command as an
	// administrator before the registration can be removed.
	scmUninstallElevate
)

// scmUninstallActionFor decides what an uninstallation does after the
// existence of the registration was established.
//
// The decision is a pure function of the outcome of the existence check,
// so it can be checked without a registration of its own: the service
// control manager accepts a machine-wide registration only from an
// administrator, so a test cannot create one without an elevated shell.
//
// An absent registration is a clean success: the uninstallation is already
// done and it needs no elevation at all. A registration the caller may not
// look at needs an elevated shell: the caller cannot tell whether the
// instance is installed, and the removal needs the rights of the machine
// either way.
func scmUninstallActionFor(state scmRegistrationState) scmUninstallAction {
	switch state {
	case scmRegistrationAbsent:
		return scmUninstallNothing
	case scmRegistrationPresent:
		return scmUninstallRemove
	default:
		return scmUninstallElevate
	}
}

// openRegistration opens the registration of the instance.
//
// The second result reports whether the service is registered at all: an
// absent registration is not an error, it is the state the caller asked
// about.
//
// The existence of the registration is looked up with a read-only
// connection first: an ordinary shell can read the registration of the
// instance, so it tells an absent registration from a registration it may
// not look at. The privileged connection is opened only when there is
// something to change.
func (m *scmManager) openRegistration(action string) (*mgr.Mgr, *mgr.Service, bool, error) {
	state, err := scmRegistrationStateOf()
	if err != nil {
		return nil, nil, false, err
	}
	switch state {
	case scmRegistrationAbsent:
		return nil, nil, false, nil
	case scmRegistrationUnreadable:
		return nil, nil, false, scmElevationRequired(action)
	}
	return m.openRegistrationPrivileged(action)
}

// openRegistrationPrivileged opens the registration of the instance with
// the rights the supervisor needs to change it.
//
// The connection of the platform asks for every right of the service
// control manager, so an ordinary shell cannot open it. A refusal of the
// platform because the shell has no rights is reported as the elevation
// requirement it is.
func (m *scmManager) openRegistrationPrivileged(action string) (*mgr.Mgr, *mgr.Service, bool, error) {
	manager, err := mgr.Connect()
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, nil, false, scmElevationRequired(action)
		}
		return nil, nil, false, fmt.Errorf("service: connect to the service control manager: %w", err)
	}
	handle, err := manager.OpenService(ServiceName)
	if err != nil {
		manager.Disconnect()
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil, nil, false, nil
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, nil, false, scmElevationRequired(action)
		}
		return nil, nil, false, fmt.Errorf("service: open the registration of %s: %w", ServiceName, err)
	}
	return manager, handle, true, nil
}

// scmState maps the state of the service control manager onto the
// lifecycle states of the shared contract.
func scmState(state svc.State) State {
	switch state {
	case svc.Running:
		return StateRunning
	case svc.Stopped:
		return StateStopped
	case svc.StartPending:
		return StateStarting
	case svc.StopPending:
		return StateStopping
	default:
		// The remaining states of the supervisor (paused, pause
		// pending and continue pending) have no counterpart in the
		// shared contract: the instance does not support them.
		return StateUnknown
	}
}
