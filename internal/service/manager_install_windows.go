// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

func (m *scmManager) install(ctx context.Context, opts InstallOptions, startFirst bool) (InstallOptions, Status, error) {
	// The registration is read before a directory is created: the merge of
	// the request needs the settings the instance runs with, and only an
	// instance that ran before is restarted.
	previous, err := scmPreviousRegistration(opts.Scope)
	if err != nil {
		return opts, Status{}, err
	}
	if startFirst && previous.present {
		return opts, Status{}, fmt.Errorf("service: %s is already registered; the first-install action will not change or start it%w", ServiceName, ErrAlreadyRegistered)
	}
	if previous.present {
		opts = mergeInstallOptions(opts, previous.options, previous.present)
	}

	if err := ValidateInstallOptions(opts); err != nil {
		return opts, Status{}, err
	}
	// Candidate structure and host execution support are read before any
	// directory preparation, registration write or stop of a running service.
	if err := preflightExecutableImage(opts.Executable); err != nil {
		return opts, Status{}, err
	}
	// The elevation requirement is checked before a directory is created:
	// a permission error of the machine-wide directories is a much less
	// readable answer than the real reason.
	if err := requireElevated("an installation"); err != nil {
		return opts, Status{}, err
	}
	var pathAccess scmPathAccess
	pathAccess, err = scmOpenPathAccess(previous.processID)
	if err != nil {
		log.Printf("service: the effective access token of the running %s could not be obtained (%v): access preflight is inconclusive", ServiceName, err)
		pathAccess = nil
	}
	if pathAccess != nil {
		defer pathAccess.Close()
	}
	// The program is checked before the first write, so a refused program leaves
	// the instance unchanged. Only AccessCheck with the running LocalService
	// token can establish a denial; an unavailable token remains inconclusive.
	if err := preflightExecutableAccess(opts.Executable, pathAccess); err != nil {
		return opts, Status{}, err
	}
	// The paths the installation is about to create are remembered before
	// they are created, so the installation knows which of them it owns.
	// The list holds the directories of the instance only: a directory the
	// installation reuses belongs to whoever named it, and the installation
	// never creates one.
	snapshot := newSCMInstallSnapshot(opts)
	order := createdDirOrder(opts, snapshot.created)
	// Failures before registration work are no-write outcomes, so removing
	// created empty directories restores the pre-install filesystem state.
	// The registration path carries its own rollback proof below.
	fail := func(err error) error {
		removeCreatedDirs(order)
		return err
	}
	if err := PrepareDirs(opts); err != nil {
		// The preparation may have created some of the directories
		// before it failed: they are removed again, so a failed attempt
		// leaves the machine as it was.
		return opts, Status{}, fail(err)
	}
	// The state of the instance is restricted before the reused
	// directories are checked, and a path that existed before the
	// installation is never rewritten: the check of every such path happens
	// before the first descriptor is changed.
	if err := restrictInstanceState(opts, snapshot, pathAccess); err != nil {
		return opts, Status{}, fail(err)
	}
	if err := m.preflight(opts, pathAccess); err != nil {
		return opts, Status{}, fail(err)
	}

	manager, err := mgr.Connect()
	if err != nil {
		return opts, Status{}, fail(fmt.Errorf("service: connect to the service control manager: %w", err))
	}
	defer manager.Disconnect()

	// The registration is written and put into service. A failure restores
	// the previous registration, so the caller never keeps a registration
	// the installation failed to put into service; only a registration this
	// call wrote may be removed after a failed start.
	registered, err := m.register(ctx, manager, opts, previous, startFirst)
	if err != nil {
		cleanupCreatedSCMInstallDirs(order, registered)
		return opts, Status{}, err
	}
	opts.Created = registered.created
	opts.VersionVerified = registered.versionVerified

	// The event log source carries the diagnostics of the instance. A
	// source that cannot be registered is not a failed installation: the
	// administrator of the machine can register it separately, and the
	// instance falls back to the Application log.
	if err := registerEventSource(); err != nil {
		log.Printf("service: the event log source of %s was not registered: %v", ServiceName, err)
		logEvent(eventLogWarning, fmt.Sprintf("hfdesk: the event log source of %s was not registered: %v", ServiceName, err))
	}
	return opts, registered.status, nil
}

// previousSCMRegistration is the registration an installation replaces. It is
// read before anything changes: the merge needs the settings the instance runs
// with, a restart needs the process it ran in, and a failure needs it back.
type previousSCMRegistration struct {
	// present reports whether the instance was registered at all.
	present bool
	// options are the installation options the registration carries. They
	// are the values a request that names nothing keeps.
	options InstallOptions
	// running reports whether the instance ran before the installation.
	running bool
	// processID is the process the service control manager reported for the
	// running instance, zero when it reported none.
	processID uint32
	// config is the full configuration of the registration, read before the
	// installation changes it. It is what a failed installation writes back.
	config mgr.Config
}

// scmPreviousRegistration reads the registration the installation replaces.
//
// An absent registration is not an error: it is the state a first installation
// replaces. A registration the caller may not look at is reported as the
// elevation requirement it is.
func scmPreviousRegistration(scope Scope) (previousSCMRegistration, error) {
	facts, err := scmLookupRegistration(ServiceName)
	if err != nil {
		// A registration the caller may not look at is reported as the
		// elevation requirement of the installation and not of a status:
		// the message has to name the command the user runs.
		if state, stateErr := scmRegistrationStateOf(); stateErr == nil && state == scmRegistrationUnreadable {
			return previousSCMRegistration{}, scmElevationRequired("an installation")
		}
		return previousSCMRegistration{}, err
	}
	if !facts.present {
		return previousSCMRegistration{}, nil
	}
	return previousSCMRegistration{
		present:   true,
		options:   scmRegistrationOptions(facts.commandLine, scope),
		running:   scmInstanceRan(facts.present, facts.state),
		processID: facts.processID,
	}, nil
}

// scmRegistrationOptions returns the installation options a registration
// carries: the program of the command line, the bind address, the runtime paths
// and the cache layout.
//
// A command line that cannot be read records no setting, so the merge falls
// back to the defaults of the request: an installation is the way a damaged
// registration is repaired, so it never fails over one. No field is marked
// explicit, and the account is not recorded: the instance runs as LocalService.
func scmRegistrationOptions(commandLine string, scope Scope) InstallOptions {
	registration, err := parseSCMCommandLine(commandLine)
	if err != nil {
		log.Printf("service: the command line of the registration of %s cannot be read (%v), the request applies", ServiceName, err)
		return InstallOptions{Scope: scope}
	}
	return InstallOptions{
		Scope:      scope,
		Executable: registration.executable,
		Listen:     registration.listen(),
		Paths:      registration.paths(),
		CacheDir:   registration.cacheDir(),
		HubDir:     registration.flagValue("--hub-dir"),
		LocalDir:   registration.flagValue("--local-dir"),
	}
}

// register writes the registration of the instance and puts it into service.
//
// The registration is created when the instance is not registered and updated
// otherwise: a second registration never exists, so the instance keeps its
// identity. An update changes the command line while retaining the readable SCM
// settings it does not own, including start type and delayed start; only a first
// installation is written with the automatic start.
//
// The directories of the instance are already created when the function is
// called. A failure after a registration write attempts to restore the previous
// configuration; the result carries whether that restore or deletion was
// verified so the caller removes created directories only when it is safe. A
// replaced registration that was running is restarted and verified, and a
// failure of that restart stops the instance the installation started, restores
// the previous configuration and brings the previous program back into service.
//
// The result reports who created the registration, whether the version matched
// and the state verified while the creation handle was still owned.
type scmRegisterResult struct {
	created         bool
	versionVerified bool
	status          Status
	// createdDirsSafeToRemove is true only when this attempt made no
	// registration write, or rollback proved the prior registration restored
	// or the new registration deleted.
	createdDirsSafeToRemove bool
}

func (m *scmManager) register(ctx context.Context, manager *mgr.Mgr, opts InstallOptions, previous previousSCMRegistration, startFirst bool) (scmRegisterResult, error) {
	result := scmRegisterResult{createdDirsSafeToRemove: true}
	config := buildSCMConfig(opts)
	handle, err := scmOpenService(manager, ServiceName)
	created := false
	switch {
	case err == nil:
		if startFirst && !previous.present {
			// A registration appeared after the read-only lookup. It is not
			// the service this first-install action created, so it is neither
			// updated, started, stopped nor removed here.
			_ = handle.Close()
			result.createdDirsSafeToRemove = false
			return result, fmt.Errorf("service: %s appeared during the first installation; no existing registration was changed%w", ServiceName, ErrAlreadyRegistered)
		}
		// The service is registered already: the registration is updated
		// instead of a second one being created.
	case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		// CreateService may partially register the service before returning an
		// error; only a successful rollback below proves directory cleanup safe.
		result.createdDirsSafeToRemove = false
		handle, err = manager.CreateService(ServiceName, opts.Executable, config, RunArgs(opts)...)
		if err != nil {
			return result, fmt.Errorf("service: register %s: %w", ServiceName, err)
		}
		created = true
		// A first installation replaces nothing: a failed one removes the
		// registration it created instead of restoring one.
		previous = previousSCMRegistration{}
	default:
		// The lookup did not establish whether a concurrent registration now
		// references these paths.
		result.createdDirsSafeToRemove = false
		return result, fmt.Errorf("service: open the registration of %s: %w", ServiceName, err)
	}
	defer handle.Close()
	target := scmServiceRecoveryTarget{manager: m, handle: handle}
	result.created = created

	if !created {
		// The configuration of the registration is read before it is
		// changed, so a failure after the write can put it back as it was.
		previousConfig, err := scmQueryServiceConfig(handle)
		if err != nil {
			if !previous.present {
				// A registration appeared after the read-only lookup. Its
				// command line is unknown, so these paths are not safe to
				// remove even though this attempt has not written yet.
				result.createdDirsSafeToRemove = false
			}
			return result, fmt.Errorf("service: read the registration of %s: %w", ServiceName, err)
		}
		if !previous.present {
			// Keep paths until this newly observed registration is either
			// restored by a verified rollback or left untouched on success.
			result.createdDirsSafeToRemove = false
			// The registration appeared between the read-only lookup and
			// this handle: it is the registration the installation
			// replaces, and its state decides whether it has to be
			// restarted.
			status, err := scmQueryStatus(handle)
			if err != nil {
				return result, scmRefused("an installation", fmt.Errorf("service: query the state of %s: %w", ServiceName, err))
			}
			previous.present = true
			previous.running = scmInstanceRan(true, status.State)
			previous.processID = status.ProcessId
		}
		previous.config = previousConfig

		// An administrator may have changed the account of the
		// registration away from the supported identity. An update keeps
		// that identity and refuses such a registration instead of
		// writing the supported account back over the decision: the
		// refusal happens before the registration is written, before the
		// recovery policy is configured and before the instance is
		// stopped, so nothing of the machine is touched.
		if err := scmServiceAccountRefusal(previousConfig.ServiceStartName); err != nil {
			// This refusal happens before any write of this attempt: no
			// registration was written, so the cleanup decision stays the
			// one the code above already made — the directories this
			// attempt created are safe to remove again for a registration
			// it knew, while a registration that appeared between the
			// read-only lookup and the open may reference them and keeps
			// the keep-paths decision of its case. The neighbouring
			// disabled-start refusal does not touch the decision either.
			return result, err
		}

		// A running instance with a disabled start type cannot be put into
		// service again after the installation stops it: the service control
		// manager refuses to start a disabled service. The installation
		// refuses before it writes the registration, so the instance keeps
		// running.
		if err := scmDisabledRunningRefusal(previous.present, previous.running, previousConfig.StartType); err != nil {
			return result, err
		}

		// An administrator may have changed native settings such as start
		// type, dependencies or display metadata. Keep those while replacing
		// only the command line and the fixed HFDesk identity.
		config = scmConfigForUpdate(config, previousConfig)

		createdDirsSafeToRemove, err := m.updateRegistrationConfig(handle, config, previous)
		if err != nil {
			result.createdDirsSafeToRemove = createdDirsSafeToRemove
			return result, err
		}
		result.createdDirsSafeToRemove = false
		log.Printf("service: %s is installed already, the registration was updated", ServiceName)
		logEvent(eventLogInfo, fmt.Sprintf("hfdesk: the registration of %s was updated", ServiceName))
	}

	// The failure recovery is part of the registration: a service the machine
	// never restarts after a crash is not the instance that was asked for. A
	// policy that is registered already is kept, so a reinstall never resets
	// what the instance or an administrator decided.
	if err := configureSCMRecovery(handle); err != nil {
		createdDirsSafeToRemove, rollbackErr := m.rollbackInstallation(ctx, target, previous, scmRollbackRegistration,
			fmt.Errorf("service: configure the failure recovery of %s: %w", ServiceName, err))
		result.createdDirsSafeToRemove = createdDirsSafeToRemove
		return result, rollbackErr
	}

	if !previous.running {
		if !startFirst {
			// The ordinary install command and an update of a stopped
			// instance preserve the stopped state.
			return result, nil
		}
		if !created {
			return result, errors.New("service: the first-install action did not create its registration")
		}
		ownedConfig, err := handle.Config()
		if err != nil {
			cause := fmt.Errorf("service: read the newly created registration before its first start: %w", err)
			return result, &InstallStartError{Cause: cause, CleanupError: errors.New("the registration configuration could not be proven unchanged; it was not started or removed")}
		}
		if !scmSameOwnedConfig(ownedConfig, config) {
			cause := errors.New("service: the newly created registration changed before its first start")
			return result, &InstallStartError{Cause: cause, CleanupError: errors.New("the changed registration was not started, stopped or removed")}
		}
		verified, status, err := m.startFirstRegistration(ctx, target, opts, ownedConfig)
		if err != nil {
			var startFailure *InstallStartError
			result.createdDirsSafeToRemove = errors.As(err, &startFailure) && startFailure.RegistrationRemoved
			return result, err
		}
		result.versionVerified = verified
		result.status = status
		return result, nil
	}

	// The instance ran before the installation, so it has to run the new
	// program afterwards. The graceful stop completes before the new process
	// is started, so the two instances never compete for the bind address.
	outcome, err := m.stopService(ctx, target)
	if err != nil {
		// Only an undelivered control proves the instance keeps running; an
		// accepted or unreadable stop may still put it down, so the rollback
		// settles it instead of leaving the machine without its instance.
		createdDirsSafeToRemove, rollbackErr := m.rollbackInstallation(ctx, target, previous, scmStopRollbackMode(outcome),
			fmt.Errorf("service: stop the running instance of %s: %w", ServiceName, err))
		result.createdDirsSafeToRemove = createdDirsSafeToRemove
		return result, rollbackErr
	}
	check, versionVerified, err := m.startAndVerifyVersion(ctx, handle, opts.Executable, opts.Listen, opts.ExpectedVersion, previous.processID)
	if err != nil {
		createdDirsSafeToRemove, rollbackErr := m.rollbackInstallation(ctx, target, previous, scmRollbackRestart,
			fmt.Errorf("service: the installation did not put the new executable into service: %w", err))
		result.createdDirsSafeToRemove = createdDirsSafeToRemove
		return result, rollbackErr
	}
	if check != scmImageVerified {
		// The handle of the running process was refused, so the image of the
		// process could not be read. The instance runs and the registration
		// carries the program; the installation reports exactly that. The
		// independent health-version result, when present, remains its own
		// fact and is not erased by the image-read result.
		message := fmt.Sprintf("hfdesk: %s runs the registered program %s, the image of the process was not readable from the shell of the installation", ServiceName, opts.Executable)
		log.Printf("service: %s", message)
		logEvent(eventLogWarning, message)
	}
	status, finalState, err := scmStatusFromTarget(target)
	if err != nil {
		createdDirsSafeToRemove, rollbackErr := m.rollbackInstallation(ctx, target, previous, scmRollbackRestart,
			fmt.Errorf("service: the state of the restarted instance cannot be confirmed: %w", err))
		result.createdDirsSafeToRemove = createdDirsSafeToRemove
		return result, rollbackErr
	}
	if finalState.State != svc.Running || finalState.ProcessId == previous.processID {
		createdDirsSafeToRemove, rollbackErr := m.rollbackInstallation(ctx, target, previous, scmRollbackRestart,
			fmt.Errorf("service: the restarted instance did not remain in a new running process (state %v, process %d)", finalState.State, finalState.ProcessId))
		result.createdDirsSafeToRemove = createdDirsSafeToRemove
		return result, rollbackErr
	}
	result.versionVerified = versionVerified
	result.status = status
	return result, nil
}

type scmFirstInstallTarget interface {
	scmRunningImageTarget
	scmServiceControl
	Start() error
	Config() (mgr.Config, error)
	Delete() error
}

func (m *scmManager) startFirstRegistration(ctx context.Context, target scmFirstInstallTarget, opts InstallOptions, ownedConfig mgr.Config) (bool, Status, error) {
	if !scmFirstInstallRegistrationStillOwned(target, ownedConfig) {
		cause := errors.New("service: the newly created registration changed before its first start")
		return false, Status{}, &InstallStartError{Cause: cause, CleanupError: errors.New("the changed registration was not started, stopped or removed")}
	}
	startErr := target.Start()
	startedHere := startErr == nil
	if errors.Is(startErr, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		// A concurrent start is not this action's process. It may still be
		// checked for success, but cleanup must not stop it on failure.
		startErr = nil
	}
	if startErr != nil {
		failure := scmRefused("a start", fmt.Errorf("service: start %s: %w", ServiceName, startErr))
		return false, Status{}, m.cleanupFailedFirstRegistration(ctx, target, ownedConfig, 0, failure)
	}

	verification, err := m.verifyRunningExecutableVersionUsing(ctx, target,
		opts.Executable, opts.Listen, opts.ExpectedVersion, true)
	if err != nil {
		processID := verification.processID
		if !startedHere {
			processID = 0
		}
		failure := fmt.Errorf("service: the first installation did not put the new executable into service: %w", err)
		return false, Status{}, m.cleanupFailedFirstRegistration(ctx, target, ownedConfig, processID, failure)
	}
	if verification.imageCheck != scmImageVerified {
		message := fmt.Sprintf("hfdesk: %s is running its registered program %s, but the process image could not be read from the installation shell", ServiceName, opts.Executable)
		log.Printf("service: %s", message)
		logEvent(eventLogWarning, message)
	}

	if !scmFirstInstallRegistrationStillOwned(target, ownedConfig) {
		failure := errors.New("service: the first-install registration changed after readiness verification")
		return false, Status{}, m.cleanupFailedFirstRegistration(ctx, target, ownedConfig, verification.processID, failure)
	}
	status, finalState, err := scmStatusFromTarget(target)
	if err != nil {
		failure := fmt.Errorf("service: the state of the first installed instance cannot be confirmed: %w", err)
		processID := verification.processID
		if !startedHere {
			processID = 0
		}
		return false, Status{}, m.cleanupFailedFirstRegistration(ctx, target, ownedConfig, processID, failure)
	}
	if !scmFirstInstallRegistrationStillOwned(target, ownedConfig) {
		failure := errors.New("service: the first-install registration changed while its final state was read")
		return false, Status{}, m.cleanupFailedFirstRegistration(ctx, target, ownedConfig, verification.processID, failure)
	}
	if !status.Running || finalState.State != svc.Running || finalState.ProcessId != verification.processID {
		failure := fmt.Errorf("service: the first installed process did not remain in service (state %v, process %d, running %t)", finalState.State, finalState.ProcessId, status.Running)
		processID := verification.processID
		if !startedHere {
			processID = 0
		}
		return false, Status{}, m.cleanupFailedFirstRegistration(ctx, target, ownedConfig, processID, failure)
	}
	return verification.versionVerified, status, nil
}

type scmOwnedFirstInstallControl struct {
	target    scmFirstInstallTarget
	config    mgr.Config
	processID uint32
}

func (c scmOwnedFirstInstallControl) Query() (svc.Status, error) { return c.target.Query() }

func (c scmOwnedFirstInstallControl) Control(cmd svc.Cmd) (svc.Status, error) {
	if !scmFirstInstallRegistrationStillOwned(c.target, c.config) {
		return svc.Status{}, errors.New("the registration changed during first-install cleanup")
	}
	status, err := c.target.Query()
	if err != nil {
		return status, err
	}
	if status.State != svc.Stopped && (c.processID == 0 || status.ProcessId != c.processID) {
		return status, errors.New("the running process is not the process started by this first installation")
	}
	return c.target.Control(cmd)
}

func scmFirstInstallRegistrationStillOwned(target scmFirstInstallTarget, expected mgr.Config) bool {
	current, err := target.Config()
	return err == nil && scmSameOwnedConfig(current, expected)
}

func scmSameOwnedConfig(current, expected mgr.Config) bool {
	if !scmSameRegistration(current.BinaryPathName, expected.BinaryPathName) {
		return false
	}
	// x/sys returns Dependencies as a non-nil empty slice when the SCM has
	// no entries, while a freshly built configuration leaves it nil. They are
	// the same registration state, not a concurrent configuration change.
	if !slices.Equal(current.Dependencies, expected.Dependencies) {
		return false
	}
	current.BinaryPathName = expected.BinaryPathName
	current.Dependencies = expected.Dependencies
	return reflect.DeepEqual(current, expected)
}

func (m *scmManager) cleanupFailedFirstRegistration(ctx context.Context, target scmFirstInstallTarget, ownedConfig mgr.Config, processID uint32, cause error) error {
	cleanup := context.WithoutCancel(ctx)
	keep := func(owned, stopped bool, err error) error {
		return &InstallStartError{
			Cause:             cause,
			RegistrationOwned: owned,
			ProcessStopped:    stopped,
			CleanupError:      err,
		}
	}
	if !scmFirstInstallRegistrationStillOwned(target, ownedConfig) {
		return keep(false, false, errors.New("the registration no longer matches the configuration created by this installation"))
	}
	status, err := target.Query()
	if err != nil {
		return keep(true, false, fmt.Errorf("the state of the created registration cannot be read: %w", err))
	}
	if status.State != svc.Stopped {
		if processID == 0 || status.ProcessId != processID {
			return keep(true, false, fmt.Errorf("the running process %d was not proven to be the process started by this installation", status.ProcessId))
		}
		if _, err := m.stopService(cleanup, scmOwnedFirstInstallControl{target: target, config: ownedConfig, processID: processID}); err != nil {
			return keep(true, false, fmt.Errorf("the process started by this installation was not proven stopped: %w", err))
		}
	}
	if !scmFirstInstallRegistrationStillOwned(target, ownedConfig) {
		return keep(false, true, errors.New("the registration changed before it could be removed"))
	}
	if err := target.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return keep(true, true, fmt.Errorf("the stopped registration created by this installation could not be removed: %w", err))
	}
	return &InstallStartError{Cause: cause, RegistrationRemoved: true, RegistrationOwned: true, ProcessStopped: true}
}

// scmConfiguration is the part of a registration the installation writes.
//
// The interface is the seam of the test: the update of a registration and the
// restore of a failed update are the two places that write the configuration,
// so a Windows test can drive a registration that was mutated and then refused,
// and a restore that fails as well, without a registration of its own.
// "golang.org/x/sys/windows/svc/mgr".Service implements it.
type scmConfiguration interface {
	// UpdateConfig writes the configuration of the registration.
	UpdateConfig(config mgr.Config) error
	// Config reads the configuration of the registration.
	Config() (mgr.Config, error)
}

// updateRegistrationConfig writes the configuration of an update and reports a
// truthful outcome when the platform refuses it.
//
// Service.UpdateConfig is not atomic: it writes the program, then the account,
// the delayed auto start and the description in separate calls, so a later call
// can fail after the command line of the registration was already replaced. The
// running instance is not touched by that write, and it is not touched here
// either: the failure path never stops or starts anything, it puts the previous
// configuration back and says what it proved.
func (m *scmManager) updateRegistrationConfig(registration scmConfiguration, config mgr.Config, previous previousSCMRegistration) (createdDirsSafeToRemove bool, err error) {
	err = registration.UpdateConfig(config)
	if err == nil {
		return false, nil
	}
	failure := fmt.Errorf("service: update the registration of %s: %w", ServiceName, err)
	return failedUpdateRestore(registration, previous, failure)
}

// failedUpdateRestore restores the configuration the installation replaced
// after an update the platform refused.
//
// The repair is always attempted: a failed observation never prevents it,
// because the previous configuration is in memory and the update may already
// have replaced the command line. The outcome is what was observed, never what
// was attempted: the previous configuration is reported as restored only when
// the restore's own write and read-back proved its command line, and a restore
// that fails, or whose read-back fails, is reported as a registration that may
// be partially changed and whose previous configuration could not be restored
// or verified, together with the reason. The original failure stays the error
// the caller gets, and the running instance is never stopped or started here:
// the refused write did not touch it.
func failedUpdateRestore(registration scmConfiguration, previous previousSCMRegistration, failure error) (createdDirsSafeToRemove bool, err error) {
	if err := restoreSCMConfig(registration, previous); err != nil {
		return false, AppendActionErrorIfUnnamed(fmt.Errorf("%w (the registration of %s may be partially changed: the previous configuration could not be restored or verified: %v)", failure, ServiceName, err), scmStopStatusHint)
	}
	return true, AppendActionErrorIfUnnamed(fmt.Errorf("%w (the previous registration of %s was restored, its command line verified; the running instance was not touched)", failure, ServiceName), scmStopStatusHint)
}

// scmConfigExecutable returns the program of a registration configuration.
//
// A command line that cannot be read names no program: the caller then has no
// program to compare and reports the running state alone.
func scmConfigExecutable(config mgr.Config) string {
	registration, err := parseSCMCommandLine(config.BinaryPathName)
	if err != nil {
		return ""
	}
	return registration.executable
}

// scmSameRegistration reports whether two command lines register the same
// program with the same arguments.
//
// The program of a command line is compared by scmSameExecutablePath, so a
// command line the platform stored in another case or with another spelling of
// the path is the same registration. The arguments are compared as they are: an
// argument is the value the instance runs with, and the platform never rewrites
// one.
func scmSameRegistration(got, want string) bool {
	gotRegistration, gotErr := parseSCMCommandLine(got)
	wantRegistration, wantErr := parseSCMCommandLine(want)
	if gotErr != nil || wantErr != nil {
		// A command line that cannot be read is compared as it is written:
		// nothing about it can be proven.
		return got == want
	}
	if !scmSameExecutablePath(gotRegistration.executable, wantRegistration.executable) {
		return false
	}
	return slices.Equal(gotRegistration.args, wantRegistration.args)
}

// scmSameExecutablePath reports whether two paths name the same program.
//
// The comparison is made on the cleaned paths and without regard to the case:
// the service control manager, the process and the request name the same program
// in the case and the spelling they store. A path that is empty names no program
// at all, so it is never the same as another path.
func scmSameExecutablePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// scmInstanceRan reports whether a supervisor state means the instance runs,
// or is about to: it is running or starting. A registration that is not there
// does not run, and neither does one that is stopping: it already reached its
// wanted state, so an installation leaves it alone.
//
// The two callers ask the same question of two reads: an installation asks it
// about the state before it changed the registration, and the status asks it
// about the state it just read, where it is the supervisor's running fact of a
// registration that may be broken.
func scmInstanceRan(present bool, state svc.State) bool {
	if !present {
		return false
	}
	switch state {
	case svc.Running, svc.StartPending:
		return true
	default:
		return false
	}
}

// scmDisabledRunningRefusal returns the refusal of an installation that would
// stop a running instance whose start type is disabled.
//
// A service can run while its start type is disabled: the start type is a
// setting of the registration and the instance keeps running when an
// administrator disables it. The installation stops the instance to put the new
// program into service, and the service control manager refuses to start a
// disabled service, so the instance would stay stopped and the rollback could
// not bring it back either. The installation refuses before the registration is
// written and before the instance is stopped. A stopped instance with a disabled
// start type is allowed: the installation does not start it.
func scmDisabledRunningRefusal(present, running bool, startType uint32) error {
	if !present || !running || startType != mgr.StartDisabled {
		return nil
	}
	return fmt.Errorf("service: %s is running, but its start type is disabled: Windows will not start it again after the installation stops it. Set the start type to manual or automatic, for example with \"sc config %s start= demand\", and run the installation again", ServiceName, ServiceName)
}

// scmServiceAccountRefusal returns the refusal of an update over a
// registration whose account is not the supported identity.
//
// Windows registers the instance as the built-in LocalService account, and
// an HFDesk update keeps that identity. An administrator may have changed
// the account of the registration with the service control manager, and
// writing the supported account back over that change would cancel the
// decision without a word. The update refuses instead — before the
// registration is written, before the recovery policy is configured and
// before the instance is stopped — and names the command that restores the
// supported identity. A registration that runs as the supported identity is
// never refused, whatever spelling the operating system reports it under
// (see scmSupportedServiceAccount): the update keeps its account exactly as
// it does today, and a first installation is unaffected — it registers the
// supported identity itself and never reaches this check.
func scmServiceAccountRefusal(account string) error {
	if scmSupportedServiceAccount(account) {
		return nil
	}
	observed := strings.TrimSpace(account)
	if observed == "" {
		// An empty start name is how the service control manager reports
		// the built-in LocalSystem account.
		observed = "LocalSystem"
	}
	return fmt.Errorf(
		"service: the registration of %s runs as \"%s\", and an HFDesk update keeps the supported identity %s: nothing was changed and the service was not stopped. Restore the supported identity from an elevated shell by running: sc config %s obj= \"%s\" password= \"\" — then run the installation again%w",
		ServiceName, observed, scmLocalServiceAccount,
		ServiceName, scmLocalServiceAccount, ErrServiceAccountMismatch)
}

// scmSupportedServiceAccount reports whether the account of an existing
// registration is the supported identity.
//
// The service control manager stores the name the registration was created
// with and resolves it case-insensitively; it also accepts the well-known
// security identifier of the account. What a read reports back is the form
// the operating system produced — on a localized system the built-in account
// carries the localized name the account database spells it with — so a name
// literal is not the identity. The two known spellings are fast paths, and
// any other spelling is resolved to its security identifier and compared
// with the well-known identifier of the supported identity: every spelling
// of LocalService is accepted and no other account is. An account the
// machine cannot resolve is not confirmed as the supported identity and
// keeps the refusal.
func scmSupportedServiceAccount(account string) bool {
	account = strings.TrimSpace(account)
	if strings.EqualFold(account, scmLocalServiceAccount) ||
		strings.EqualFold(account, scmLocalServiceAccountSID) {
		return true
	}
	if account == "" {
		return false
	}
	sid, err := scmAccountSecurityIdentifier(account)
	if err != nil {
		return false
	}
	return strings.EqualFold(sid, scmLocalServiceAccountSID)
}

// scmAccountSecurityIdentifier resolves an account name of the local
// machine to the string form of its security identifier.
//
// The variable is the seam of the tests: the spelling and localization of
// account names belong to the operating system a test happens to run on,
// and a test drives the identity comparison without depending on the
// language of that machine.
var scmAccountSecurityIdentifier = func(account string) (string, error) {
	sid, _, _, err := windows.LookupSID("", account)
	if err != nil {
		return "", err
	}
	return sid.String(), nil
}

// scmRecoveryActions returns the failure recovery of the instance.
//
// The platform restarts an instance that ended without reporting its stop. The
// first failure is followed by a restart after five seconds, the second one after
// thirty seconds and every further one after sixty seconds: the delay grows, so a
// program that cannot come up is not restarted in a tight loop.
func scmRecoveryActions() []mgr.RecoveryAction {
	return []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}
}

// scmRecoveryResetPeriod is the time after which the platform forgets the
// failures of the instance, in seconds: one day.
const scmRecoveryResetPeriod uint32 = 24 * 60 * 60

// scmRecoveryDecision is what an installation does with the failure recovery of
// the registration.
type scmRecoveryDecision int

const (
	// scmRecoveryConfigure writes the HFDesk policy after a successful read
	// proved that the registration has no recovery actions.
	scmRecoveryConfigure scmRecoveryDecision = iota
	// scmRecoveryKeep keeps the recovery policy the registration carries.
	scmRecoveryKeep
	// scmRecoveryUnreadable refuses to infer absence from a failed read.
	scmRecoveryUnreadable
)

// scmRecoveryDecisionFor returns what an installation does with the failure
// recovery of a registration.
//
// A registration with at least one action keeps its complete policy, including
// the non-crash flag. Only a successful read of an empty list authorizes
// default-action setup; an unreadable list is not absence.
func scmRecoveryDecisionFor(existing []mgr.RecoveryAction, readErr error) scmRecoveryDecision {
	if readErr != nil {
		return scmRecoveryUnreadable
	}
	if len(existing) == 0 {
		return scmRecoveryConfigure
	}
	return scmRecoveryKeep
}

type scmRecoverySettings struct {
	actions       []mgr.RecoveryAction
	resetPeriod   uint32
	nonCrash      bool
	command       string
	rebootMessage string
}

type scmRecoveryConfig interface {
	RecoveryActions() ([]mgr.RecoveryAction, error)
	ResetPeriod() (uint32, error)
	RecoveryActionsOnNonCrashFailures() (bool, error)
	RecoveryCommand() (string, error)
	RebootMessage() (string, error)
	SetActions([]mgr.RecoveryAction, uint32, string, string) error
	SetNonCrashFailures(bool) error
}

type scmServiceRecoveryConfig struct{ handle *mgr.Service }

func (c scmServiceRecoveryConfig) RecoveryActions() ([]mgr.RecoveryAction, error) {
	return c.handle.RecoveryActions()
}

func (c scmServiceRecoveryConfig) ResetPeriod() (uint32, error) { return c.handle.ResetPeriod() }

func (c scmServiceRecoveryConfig) RecoveryActionsOnNonCrashFailures() (bool, error) {
	return c.handle.RecoveryActionsOnNonCrashFailures()
}

func (c scmServiceRecoveryConfig) RecoveryCommand() (string, error) {
	return c.handle.RecoveryCommand()
}

func (c scmServiceRecoveryConfig) RebootMessage() (string, error) {
	return c.handle.RebootMessage()
}

func (c scmServiceRecoveryConfig) SetActions(actions []mgr.RecoveryAction, reset uint32, command, reboot string) error {
	return scmSetRecoveryActions(c.handle, actions, reset, command, reboot)
}

func (c scmServiceRecoveryConfig) SetNonCrashFailures(enabled bool) error {
	return c.handle.SetRecoveryActionsOnNonCrashFailures(enabled)
}

// configureSCMRecovery configures the failure recovery of the registration.
//
// The recovery is the recovery of a failed instance and not its lifecycle: the
// platform acts for an instance that ended without reporting its stop, and,
// with the non-crash switch set, for one that reported a non-zero exit code. An
// explicit stop reports the stopped state with exit code 0, so it is never
// turned into a restart.
//
// The switch is part of the policy of HFDesk: the handler reports a failed
// start and a failed host with a non-zero exit code, and without the switch the
// platform would recover a crashed process only. The switch takes effect at the
// next start of the machine, so the installation reports the policy it wrote.
//
// A nonempty custom action list and its non-crash flag are preserved as one
// policy, including intentional NoAction and crash-only actions. A successful
// read that proves no actions causes the required default restart policy to be
// configured. A failed read is not treated as absence.
func configureSCMRecovery(handle *mgr.Service) error {
	return configureSCMRecoveryWith(scmServiceRecoveryConfig{handle: handle})
}

func configureSCMRecoveryWith(config scmRecoveryConfig) error {
	existing, err := config.RecoveryActions()
	decision := scmRecoveryDecisionFor(existing, err)
	switch decision {
	case scmRecoveryUnreadable:
		return fmt.Errorf("service: read the failure recovery actions of %s: %w", ServiceName, err)
	case scmRecoveryKeep:
		log.Printf("service: %s keeps its existing failure recovery actions and non-crash flag", ServiceName)
		return nil
	}

	previous := scmRecoverySettings{actions: existing}
	if previous.resetPeriod, err = config.ResetPeriod(); err != nil {
		return fmt.Errorf("service: read the failure recovery reset period of %s: %w", ServiceName, err)
	}
	if previous.nonCrash, err = config.RecoveryActionsOnNonCrashFailures(); err != nil {
		return fmt.Errorf("service: read the non-crash recovery flag of %s: %w", ServiceName, err)
	}
	if previous.command, err = config.RecoveryCommand(); err != nil {
		return fmt.Errorf("service: read the recovery command of %s: %w", ServiceName, err)
	}
	if previous.rebootMessage, err = config.RebootMessage(); err != nil {
		return fmt.Errorf("service: read the recovery reboot message of %s: %w", ServiceName, err)
	}

	if err := config.SetActions(scmRecoveryActions(), scmRecoveryResetPeriod, previous.command, previous.rebootMessage); err != nil {
		failure := fmt.Errorf("service: configure the failure recovery actions of %s: %w", ServiceName, err)
		return scmRestoreRecoveryAfterFailure(config, previous, failure, false)
	}
	if !previous.nonCrash {
		if err := config.SetNonCrashFailures(true); err != nil {
			failure := fmt.Errorf("service: enable recovery for non-crash failures of %s: %w", ServiceName, err)
			return scmRestoreRecoveryAfterFailure(config, previous, failure, true)
		}
	}
	log.Printf("service: the registration of %s had no recovery actions; the required HFDesk crash/error policy was configured", ServiceName)
	return nil
}

func scmRestoreRecoveryAfterFailure(config scmRecoveryConfig, previous scmRecoverySettings, failure error, restoreFlag bool) error {
	if err := scmRestoreRecoverySettings(config, previous, restoreFlag); err != nil {
		return fmt.Errorf("%w (the previous empty recovery policy could not be restored or verified: %v)", failure, err)
	}
	return fmt.Errorf("%w (the previous empty recovery policy was restored and verified)", failure)
}

func scmRestoreRecoverySettings(config scmRecoveryConfig, previous scmRecoverySettings, restoreFlag bool) error {
	var restoreErrs []error
	if err := config.SetActions(previous.actions, previous.resetPeriod, previous.command, previous.rebootMessage); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("write the previous recovery actions: %w", err))
	}
	if restoreFlag {
		if err := config.SetNonCrashFailures(previous.nonCrash); err != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("write the previous non-crash recovery flag: %w", err))
		}
	}
	if len(restoreErrs) > 0 {
		return errors.Join(restoreErrs...)
	}
	actions, err := config.RecoveryActions()
	if err != nil {
		return fmt.Errorf("read the restored recovery actions: %w", err)
	}
	resetPeriod, err := config.ResetPeriod()
	if err != nil {
		return fmt.Errorf("read the restored recovery reset period: %w", err)
	}
	nonCrash, err := config.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return fmt.Errorf("read the restored non-crash recovery flag: %w", err)
	}
	command, err := config.RecoveryCommand()
	if err != nil {
		return fmt.Errorf("read the restored recovery command: %w", err)
	}
	reboot, err := config.RebootMessage()
	if err != nil {
		return fmt.Errorf("read the restored recovery reboot message: %w", err)
	}
	if len(actions) != 0 || resetPeriod != previous.resetPeriod || nonCrash != previous.nonCrash || command != previous.command || reboot != previous.rebootMessage {
		return errors.New("read-back does not match the previous empty recovery policy")
	}
	return nil
}

func scmSetRecoveryActions(handle *mgr.Service, actions []mgr.RecoveryAction, resetPeriod uint32, command, reboot string) error {
	var commandPtr, rebootPtr *uint16
	var err error
	if command != "" {
		commandPtr, err = windows.UTF16PtrFromString(command)
		if err != nil {
			return err
		}
	}
	if reboot != "" {
		rebootPtr, err = windows.UTF16PtrFromString(reboot)
		if err != nil {
			return err
		}
	}
	native := make([]windows.SC_ACTION, len(actions))
	for i, action := range actions {
		native[i] = windows.SC_ACTION{
			Type:  uint32(action.Type),
			Delay: uint32(action.Delay / time.Millisecond),
		}
	}
	policy := windows.SERVICE_FAILURE_ACTIONS{
		ResetPeriod:  resetPeriod,
		RebootMsg:    rebootPtr,
		Command:      commandPtr,
		ActionsCount: uint32(len(native)),
	}
	if len(native) > 0 {
		policy.Actions = &native[0]
	} else {
		// The SCM API accepts a non-nil buffer with a zero count to clear
		// actions, matching mgr.ResetRecoveryActions while retaining the
		// previous reset period and ancillary fields.
		policy.Actions = &windows.SC_ACTION{}
	}
	return windows.ChangeServiceConfig2(handle.Handle, windows.SERVICE_CONFIG_FAILURE_ACTIONS, (*byte)(unsafe.Pointer(&policy)))
}

// scmInstallDirs returns the directories an installation creates, in the order
// it creates them: the directory of the config file, the state directory and
// the log directory.
//
// A named data directory (the cache root, the hub directory, the local output
// directory) belongs to whoever named it, so the installation never creates
// one. Every path is listed once.
func scmInstallDirs(opts InstallOptions) []string {
	dirs := []string{
		filepath.Dir(opts.Paths.ConfigFile),
		opts.Paths.StateDir,
		opts.Paths.LogDir,
	}

	unique := make([]string, 0, len(dirs))
	seen := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		if dir == "" || dir == "." || seen[dir] {
			continue
		}
		seen[dir] = true
		unique = append(unique, dir)
	}
	return unique
}

// createdDirs returns the directories that do not exist yet on the way to
// the path, from the top down.
//
// The list holds the path itself when it is missing: it is the set of
// directories an installation creates for the path. The list is read before
// the directories are created, so the installation knows which of them it
// owns.
func createdDirs(path string) []string {
	var missing []string
	current := filepath.Clean(path)
	for {
		if _, err := os.Stat(current); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			break
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	// The list was collected from the bottom up, the directories are
	// created from the top down.
	for i, j := 0, len(missing)-1; i < j; i, j = i+1, j-1 {
		missing[i], missing[j] = missing[j], missing[i]
	}
	return missing
}

// createdDirOrder returns the directories an installation creates, in the
// order it creates them.
//
// The map holds the directories of every directory of the installation. The
// directories are created in the order of scmInstallDirs (the directories of
// the instance first, the directories the installation reuses afterwards)
// and from the top down inside each of them, so the list is the order of the
// creation.
//
// A directory that several directories need is listed once: the first
// directory that needed it is the one that created it.
func createdDirOrder(opts InstallOptions, created map[string][]string) []string {
	var order []string
	seen := make(map[string]bool, len(created))
	for _, dir := range scmInstallDirs(opts) {
		for _, path := range created[dir] {
			if seen[path] {
				continue
			}
			seen[path] = true
			order = append(order, path)
		}
	}
	return order
}

// scmInstallSnapshot is the state of the paths of an installation before it
// creates anything.
//
// The snapshot is read before the first directory is created, so the
// installation knows which of the paths of the instance it owns and which
// ones belong to whoever named them.
type scmInstallSnapshot struct {
	// created holds the directories the installation creates for every
	// directory of the installation, the missing parents included.
	created map[string][]string
	// configFileMissing reports whether the config file of the instance
	// did not exist before the installation.
	configFileMissing bool
}

// newSCMInstallSnapshot reads the state of the paths of an installation
// before it creates anything.
//
// The directories of the installation are read one by one: a directory that
// does not exist yet is a directory the installation creates, and so are the
// missing parents of it.
func newSCMInstallSnapshot(opts InstallOptions) scmInstallSnapshot {
	dirs := scmInstallDirs(opts)
	created := make(map[string][]string, len(dirs))
	for _, dir := range dirs {
		created[dir] = createdDirs(dir)
	}
	return scmInstallSnapshot{
		created:           created,
		configFileMissing: pathMissing(opts.Paths.ConfigFile),
	}
}

// pathMissing reports whether a path does not exist.
//
// A path that cannot be read is not missing: the installation leaves such a
// path alone instead of taking it over.
func pathMissing(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// removeCreatedDirs removes the directories an installation created.
//
// The directories are removed in the reverse order of their creation, so a
// parent is only removed after its children. A directory that is not empty is
// kept: the removal never deletes data and it never walks into a directory.
//
// The removal is best effort: a failure of the removal is ignored, so the
// original failure of the installation stays the error the caller gets.
func removeCreatedDirs(dirs []string) {
	for i := len(dirs) - 1; i >= 0; i-- {
		path := dirs[i]
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			// The directory is gone already, or it is not a directory
			// any more: nothing is removed, so no file is ever deleted.
			continue
		}
		// A directory that holds something is kept: the removal of such
		// a directory would delete what it holds.
		_ = os.Remove(path)
	}
}

// cleanupCreatedSCMInstallDirs removes created paths after a no-write result or
// a rollback that proved the prior registration restored or the new one deleted.
func cleanupCreatedSCMInstallDirs(dirs []string, registration scmRegisterResult) {
	if registration.createdDirsSafeToRemove {
		removeCreatedDirs(dirs)
	}
}

// buildSCMConfig builds the configuration of the registration of the
// instance.
//
// The configuration is the one a first installation writes: the registration
// starts the instance automatically at the boot of the machine. A repeat
// installation keeps the autostart state of the registration it replaces (see
// scmConfigWithAutostart): the start type of a registration is a setting of the
// machine, and an administrator may have switched the service to a manual or a
// disabled start, or to a delayed automatic start.
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
		// BinaryPathName is the command line of the update path: the
		// update of a registration sends the field to the service
		// control manager. The create path ignores the field: the
		// service control manager rebuilds the command line of a new
		// registration from the executable and the arguments, so the
		// field does not control the create path.
		BinaryPathName: scmCommandLine(opts),
	}
}

// scmConfigForUpdate changes the program while retaining the other readable
// SCM settings. The service identity is fixed by the project contract and the
// SCM API does not return Password, so neither field is copied from a prior
// registration.
func scmConfigForUpdate(config, previous mgr.Config) mgr.Config {
	previous.BinaryPathName = config.BinaryPathName
	previous.ServiceStartName = scmLocalServiceAccount
	previous.Password = ""
	return previous
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
