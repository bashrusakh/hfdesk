// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// The Windows service mode of HFDesk is a registration in the service
// control manager.
//
// The service control manager registers machine-wide services only, so a
// user scope installation does not exist on this platform. The
// registration is driven through the API of the supervisor: sc.exe,
// PowerShell and the shell are never called, so a path with a space or a
// quote in it cannot change the command that runs.
//
// The manager never escalates privileges itself: an installation and an
// uninstallation need an elevated shell, and the manager fails with a
// clear message when the process does not have one.
const (
	// scmDisplayName is the name the service manager shows.
	scmDisplayName = "HFDesk"

	// scmDescription is the description of the registration.
	scmDescription = "HFDesk - local dashboard for searching, analyzing and managing Hugging Face models and datasets."

	// scmLocalServiceAccount is the account the instance runs as.
	//
	// LocalService is the least privileged of the built-in service
	// accounts of the operating system: it has no password, it is not a
	// member of the administrators and it holds only the rights a
	// service needs. The instance therefore never runs with the rights
	// of the machine and never with the rights of the user who
	// installed it.
	//
	// The account has a profile of its own, so the instance sees
	// neither the desktop nor the files of the user who installed it.
	// The paths of the instance are the machine-wide paths of the
	// platform (%ProgramData%\HFDesk), never the profile of a user.
	scmLocalServiceAccount = `NT AUTHORITY\LocalService`

	// scmRegistrationPath is the place the registration lives in. The
	// service control manager keeps its database in the registry, so the
	// registration has no file of its own.
	scmRegistrationPath = `HKLM\SYSTEM\CurrentControlSet\Services\` + ServiceName
)

const (
	// scmStopTimeout bounds the wait for the graceful stop of the
	// instance.
	scmStopTimeout = 30 * time.Second

	// scmStopPollInterval is the pause between two checks of the state of
	// the instance.
	scmStopPollInterval = 200 * time.Millisecond
)

// scmManager drives the service control manager of HFDesk.
//
// The manager registers the instance as a machine-wide service, it starts
// and stops it and it reports its state. The registration carries the
// canonical argument vector of the shared contract: paths and a bind
// address only. A token, a proxy password or a basic auth password is
// never part of a registration, because every user of the machine can read
// the registration.
type scmManager struct {
	// scope is the installation scope of the instance.
	scope Scope
}

// newPlatformManager returns the service control manager of the given
// scope.
//
// The service control manager registers machine-wide services only, so a
// user scope installation is rejected explicitly instead of silently
// falling back to a machine-wide registration.
func newPlatformManager(scope Scope) (Manager, error) {
	switch scope {
	case ScopeSystem:
		return &scmManager{scope: scope}, nil
	case ScopeUser:
		return nil, fmt.Errorf("service: scope %q is not supported on windows: the service control manager registers machine-wide services only", scope)
	default:
		return nil, fmt.Errorf("service: unknown scope %q (want %q)", scope, ScopeSystem)
	}
}

// Install registers the instance with the service control manager.
//
// The installation validates the options, prepares the directories of the
// instance, registers the service, registers the event log source of the
// instance and restricts the access to the state of the instance.
//
// A service that is registered already is not registered a second time:
// its registration is updated instead, so the instance keeps its identity.
//
// An installation never starts the instance: starting is an explicit,
// separate command.
//
// A failure after the registration was created removes the registration
// again, so a failed installation never leaves a half-configured service
// behind.
func (m *scmManager) Install(ctx context.Context, opts InstallOptions) error {
	if err := ValidateInstallOptions(opts); err != nil {
		return err
	}
	// The elevation requirement is checked before a directory is created:
	// a permission error of the machine-wide directories is a much less
	// readable answer than the real reason.
	if err := requireElevated("an installation"); err != nil {
		return err
	}
	if err := PrepareDirs(opts); err != nil {
		return err
	}
	if err := m.preflight(opts); err != nil {
		return err
	}

	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service: connect to the service control manager: %w", err)
	}
	defer manager.Disconnect()

	config := buildSCMConfig(opts)
	handle, err := manager.OpenService(ServiceName)
	created := false
	switch {
	case err == nil:
		// The service is registered already. The registration is
		// updated instead of a second one being created, so the
		// instance keeps its identity and the status keeps describing
		// one service.
		defer handle.Close()
		if err := handle.UpdateConfig(config); err != nil {
			return fmt.Errorf("service: update the registration of %s: %w", ServiceName, err)
		}
		log.Printf("service: %s is installed already, the registration was updated", ServiceName)
		logEvent(eventLogInfo, fmt.Sprintf("hfdesk: the registration of %s was updated", ServiceName))
	case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		handle, err = manager.CreateService(ServiceName, opts.Executable, config, RunArgs(opts)...)
		if err != nil {
			return fmt.Errorf("service: register %s: %w", ServiceName, err)
		}
		defer handle.Close()
		created = true
	default:
		return fmt.Errorf("service: open the registration of %s: %w", ServiceName, err)
	}

	// The event log source carries the diagnostics of the instance. A
	// source that cannot be registered is not a failed installation: the
	// administrator of the machine can register it separately, and the
	// instance falls back to the Application log.
	if err := registerEventSource(); err != nil {
		log.Printf("service: the event log source of %s was not registered: %v", ServiceName, err)
		logEvent(eventLogWarning, fmt.Sprintf("hfdesk: the event log source of %s was not registered: %v", ServiceName, err))
	}

	// The state of the instance belongs to the machine.
	if err := restrictInstanceState(opts); err != nil {
		// A registration that this call created is removed again: a
		// half-configured service must not survive a failed
		// installation. A registration that was already there stays:
		// it belongs to the machine and not to this call.
		if created {
			rollbackRegistration(handle)
		}
		return err
	}
	return nil
}

// Uninstall removes the registration of the instance.
//
// It removes the registration only: the config file, the jobs, the history,
// the logs, the models and the cache of the instance are never touched. An
// absent registration is a clean success.
func (m *scmManager) Uninstall(ctx context.Context) error {
	if err := requireElevated("an uninstallation"); err != nil {
		return err
	}
	manager, handle, ok, err := m.openRegistration()
	if err != nil {
		return err
	}
	if !ok {
		// There is no registration: the uninstallation is already done.
		return nil
	}
	defer manager.Disconnect()
	defer handle.Close()

	// A running instance is stopped before the registration is removed:
	// the service control manager refuses to delete a service that is
	// still running.
	if err := m.stopService(ctx, handle); err != nil {
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
func (m *scmManager) Start(ctx context.Context) error {
	manager, handle, ok, err := m.openRegistration()
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
		return fmt.Errorf("service: start %s: %w", ServiceName, err)
	}
	return nil
}

// Stop stops the registered instance.
//
// Stopping an instance that is stopped already is a stable success, and so
// is stopping an instance that is not registered at all: it does not run
// either.
func (m *scmManager) Stop(ctx context.Context) error {
	manager, handle, ok, err := m.openRegistration()
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	defer manager.Disconnect()
	defer handle.Close()

	return m.stopService(ctx, handle)
}

// Restart stops and starts the registered instance.
//
// The instance is stopped first: the graceful shutdown of the server
// completes before a new process is started, so the two instances never
// compete for the bind address.
func (m *scmManager) Restart(ctx context.Context) error {
	manager, handle, ok, err := m.openRegistration()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("service: %s is not installed", ServiceName)
	}
	defer manager.Disconnect()
	defer handle.Close()

	if err := m.stopService(ctx, handle); err != nil {
		return err
	}
	if err := handle.Start(); err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return nil
		}
		return fmt.Errorf("service: start %s: %w", ServiceName, err)
	}
	return nil
}

// Status reports the state of the registered instance.
//
// The description of the instance comes from the registration, so the status
// describes the registration and not the intent of the caller. A registered
// executable that is gone is reported as broken, so a moved or deleted
// binary is never reported as a running service.
//
// The registration has no file of its own: the service control manager
// keeps its database in the registry, so the registration path is the
// registry key of the service.
func (m *scmManager) Status(ctx context.Context) (Status, error) {
	status := Status{
		State:    StateNotInstalled,
		Scope:    m.scope,
		Name:     ServiceName,
		UnitPath: scmRegistrationPath,
	}

	manager, handle, ok, err := m.openRegistration()
	if err != nil {
		return status, err
	}
	if !ok {
		status.Detail = fmt.Sprintf("%s is not registered", ServiceName)
		return status, nil
	}
	defer manager.Disconnect()
	defer handle.Close()

	commandLine, err := queryServiceCommandLine(handle.Handle)
	if err != nil {
		return status, fmt.Errorf("service: read the registration of %s: %w", ServiceName, err)
	}
	registration, err := parseSCMCommandLine(commandLine)
	if err != nil {
		return status, err
	}
	status.Executable = registration.executable
	status.Listen = registration.listen()
	status.Paths = registration.paths()

	if status.Executable != "" {
		if _, err := os.Stat(status.Executable); err != nil {
			status.State = StateBroken
			status.Detail = fmt.Sprintf("the registered executable %s does not exist", status.Executable)
			return status, nil
		}
	}

	query, err := handle.Query()
	if err != nil {
		return status, fmt.Errorf("service: query the state of %s: %w", ServiceName, err)
	}
	status.State = scmState(query.State)
	if status.State == StateStopped && query.Win32ExitCode != 0 {
		status.Detail = fmt.Sprintf("the instance exited with the code %d", query.Win32ExitCode)
	}
	return status, nil
}

// openRegistration opens the registration of the instance.
//
// The second result reports whether the service is registered at all: an
// absent registration is not an error, it is the state the caller asked
// about.
func (m *scmManager) openRegistration() (*mgr.Mgr, *mgr.Service, bool, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return nil, nil, false, fmt.Errorf("service: connect to the service control manager: %w", err)
	}
	handle, err := manager.OpenService(ServiceName)
	if err != nil {
		manager.Disconnect()
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil, nil, false, nil
		}
		return nil, nil, false, fmt.Errorf("service: open the registration of %s: %w", ServiceName, err)
	}
	return manager, handle, true, nil
}

// stopService stops the instance and waits for the graceful shutdown to
// finish.
//
// Stopping an instance that does not run is a stable success: the state the
// caller asked for already holds.
func (m *scmManager) stopService(ctx context.Context, handle *mgr.Service) error {
	status, err := handle.Query()
	if err != nil {
		return fmt.Errorf("service: query the state of %s: %w", ServiceName, err)
	}
	if status.State == svc.Stopped {
		return nil
	}
	if _, err := handle.Control(svc.Stop); err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			// The instance is not running: the wanted state already
			// holds.
			return nil
		}
		if !errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL) {
			return fmt.Errorf("service: stop %s: %w", ServiceName, err)
		}
	}
	return m.waitForStopped(ctx, handle)
}

// waitForStopped waits until the service control manager reports the
// instance as stopped.
//
// The wait is bounded: an instance that ignores the stop request must not
// block the caller forever.
func (m *scmManager) waitForStopped(ctx context.Context, handle *mgr.Service) error {
	deadline := time.Now().Add(scmStopTimeout)
	for {
		status, err := handle.Query()
		if err != nil {
			return fmt.Errorf("service: query the state of %s: %w", ServiceName, err)
		}
		if status.State == svc.Stopped {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service: %s did not stop within %s", ServiceName, scmStopTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(scmStopPollInterval):
		}
	}
}

// preflight rejects an installation the service control manager could not
// run: an unusable executable or an invalid bind address.
func (m *scmManager) preflight(opts InstallOptions) error {
	if !filepath.IsAbs(opts.Executable) {
		return fmt.Errorf("service: executable %q must be an absolute path", opts.Executable)
	}
	info, err := os.Stat(opts.Executable)
	if err != nil {
		return fmt.Errorf("service: executable %q is not usable: %w", opts.Executable, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("service: executable %q is not a regular file", opts.Executable)
	}
	if _, _, err := ParseListen(opts.Listen); err != nil {
		return err
	}
	return nil
}

// requireElevated rejects a machine-wide operation that needs an elevated
// shell when the process does not have one.
//
// The manager never escalates privileges itself: it never starts a runas
// call, never starts PowerShell and never starts a shell.
func requireElevated(action string) error {
	if isElevatedProcess() {
		return nil
	}
	return fmt.Errorf("service: %s needs an elevated shell: run the command as an administrator, HFDesk never escalates privileges itself", action)
}

// isElevatedProcess reports whether the process holds an elevated token.
func isElevatedProcess() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// buildSCMConfig builds the configuration of the registration of the
// instance.
//
// The configuration carries no credential at all: the service runs as
// LocalService, an account without a password, and the instance reads its
// token from its own config file.
func buildSCMConfig(opts InstallOptions) mgr.Config {
	return mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		DisplayName:      scmDisplayName,
		Description:      scmDescription,
		ServiceStartName: scmLocalServiceAccount,
		BinaryPathName:   scmCommandLine(opts),
	}
}

// scmCommandLine renders the command line the registration carries: the
// escaped executable followed by the escaped canonical arguments of the
// instance.
//
// The vector holds paths and a bind address only.
func scmCommandLine(opts InstallOptions) string {
	parts := make([]string, 0, len(RunArgs(opts))+1)
	parts = append(parts, syscall.EscapeArg(opts.Executable))
	for _, arg := range RunArgs(opts) {
		parts = append(parts, syscall.EscapeArg(arg))
	}
	return strings.Join(parts, " ")
}

// scmRegistration describes the registration of the instance.
type scmRegistration struct {
	// executable is the program the registration points at.
	executable string
	// args are the arguments of the program.
	args []string
}

// listen returns the bind address of the registration.
func (r scmRegistration) listen() string {
	return r.flagValue("--listen")
}

// paths returns the runtime path model of the registration.
func (r scmRegistration) paths() runtimepaths.Paths {
	return runtimepaths.Paths{
		ConfigFile: r.flagValue("--config"),
		StateDir:   r.flagValue("--state-dir"),
		LogDir:     r.flagValue("--log-dir"),
	}
}

// flagValue returns the value of a flag of the argument vector.
func (r scmRegistration) flagValue(flag string) string {
	for i, arg := range r.args {
		if arg == flag && i+1 < len(r.args) {
			return r.args[i+1]
		}
	}
	return ""
}

// parseSCMCommandLine reads the executable and the arguments out of the
// command line of a registration.
//
// The command line is decomposed with the rules of the operating system, so
// a path with a space in it is read back as one argument.
func parseSCMCommandLine(commandLine string) (scmRegistration, error) {
	registration := scmRegistration{}
	parts, err := windows.DecomposeCommandLine(commandLine)
	if err != nil {
		return registration, fmt.Errorf("service: read the command line of the registration: %w", err)
	}
	if len(parts) == 0 {
		return registration, errors.New("service: the registration carries no command line")
	}
	registration.executable = parts[0]
	registration.args = parts[1:]
	return registration, nil
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

// queryServiceCommandLine reads the command line of a registration.
//
// The service control manager is asked directly, so the status describes
// the registration and not the intent of the caller.
func queryServiceCommandLine(handle windows.Handle) (string, error) {
	size := uint32(1024)
	for {
		buffer := make([]byte, size)
		config := (*windows.QUERY_SERVICE_CONFIG)(unsafe.Pointer(&buffer[0]))
		var needed uint32
		err := windows.QueryServiceConfig(handle, config, uint32(len(buffer)), &needed)
		if err == nil {
			return windows.UTF16PtrToString(config.BinaryPathName), nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return "", err
		}
		if needed <= uint32(len(buffer)) {
			return "", err
		}
		size = needed
	}
}

// registerEventSource registers the event log source of the instance.
//
// A source that is registered already is not an error. A source that cannot
// be registered is not a failed installation either: the administrator of
// the machine can register it separately, and the instance falls back to
// the Application log.
func registerEventSource() error {
	err := eventlog.InstallAsEventCreate(scmEventSource, eventlog.Info|eventlog.Warning|eventlog.Error)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

// restrictInstanceState restricts the access to the state of the instance.
//
// The state of a machine-wide instance belongs to the machine: SYSTEM, the
// administrators of the machine and the account the instance runs as may
// work with it, nobody else.
//
// Only the state directory and the config file are touched. The model and
// the cache trees of an installation are never re-owned and never re-ACLed:
// they can hold gigabytes of data that the user may reuse from an
// interactive run, and a recursive change of the access rights would take
// the data away from the user who owns it.
func restrictInstanceState(opts InstallOptions) error {
	if err := applyRestrictiveACL(opts.Paths.StateDir); err != nil {
		return err
	}
	// The config file only exists when it was written already. A file
	// that is created later inherits the descriptor of the directory.
	if info, err := os.Stat(opts.Paths.ConfigFile); err == nil && info.Mode().IsRegular() {
		if err := applyRestrictiveACL(opts.Paths.ConfigFile); err != nil {
			return err
		}
	}
	return nil
}

// applyRestrictiveACL applies the security descriptor of the instance to a
// path.
//
// The descriptor blocks the inheritance of the parent directory: the access
// of the directory of ProgramData must not leak into the state of the
// instance.
func applyRestrictiveACL(path string) error {
	sd, err := windows.SecurityDescriptorFromString(scmStateSDDL())
	if err != nil {
		return fmt.Errorf("service: build the security descriptor of %q: %w", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("service: read the access control list of the security descriptor: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return fmt.Errorf("service: restrict the access to %q: %w", path, err)
	}
	return nil
}

// scmStateSDDL returns the security descriptor of the state of a
// machine-wide instance.
//
// The descriptor grants the full control to SYSTEM (SY), to the
// administrators of the machine (BA) and to the account the instance runs
// as, LocalService (LS, the SID S-1-5-19). Nobody else is granted
// anything: the "Authenticated Users" group (AU) would hand the state of
// the machine to every logged-in user of the machine.
//
// The descriptor is protected ("D:P"), so the access of the parent
// directory is not inherited.
func scmStateSDDL() string {
	return "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;LS)"
}

// rollbackRegistration removes a registration that a failed installation
// left behind.
//
// The rollback is best effort: the original failure of the installation is
// the error the caller gets.
func rollbackRegistration(handle *mgr.Service) {
	if err := handle.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		log.Printf("service: the registration of %s could not be removed: %v", ServiceName, err)
	}
}
