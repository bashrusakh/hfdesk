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

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// The Windows service mode of HFDesk is a registration in the service control
// manager.
//
// The service control manager registers machine-wide services only, so a user
// scope installation does not exist here. The registration is driven through
// the API of the supervisor: sc.exe, PowerShell and the shell are never called,
// so a path with a space or a quote cannot change the command that runs.
//
// The manager never escalates privileges itself: an installation and a
// removal need an elevated shell, and the manager fails with a clear message
// when the process has none. The existence of the registration is looked up
// before that requirement: an ordinary shell can read it, so an absent
// registration is a clean success in every shell.
//
// The status reads the registration with the rights a status needs only, which
// the default descriptor grants to the interactive users of the machine.
const (
	// scmDisplayName is the name the service manager shows.
	scmDisplayName = "HFDesk"

	// scmDescription is the description of the registration.
	scmDescription = "HFDesk - local dashboard for searching, analyzing and managing Hugging Face models and datasets."

	// scmLocalServiceAccount is the account the instance runs as.
	//
	// LocalService is the least privileged built-in service account: no
	// password, no membership in the administrators, only the rights a
	// service needs. It has a profile of its own, so the instance sees
	// neither the desktop nor the files of the user who installed it; the
	// paths of the instance are the machine-wide paths (%ProgramData%\HFDesk).
	scmLocalServiceAccount = `NT AUTHORITY\LocalService`

	// scmRegistrationPath is the place the registration lives in. The
	// service control manager keeps its database in the registry, so the
	// registration has no file of its own.
	scmRegistrationPath = `HKLM\SYSTEM\CurrentControlSet\Services\` + ServiceName
)

const (
	// scmStopTimeout bounds the wait for the graceful stop of the instance.
	//
	// The wait is the shared budget of the service layer, so the manager
	// never reports a failed stop while the instance is still saving its
	// state, and a number of its own could drift below it.
	scmStopTimeout = ShutdownBudget

	// scmStopPollInterval is the pause between two checks of the state of
	// the instance.
	scmStopPollInterval = 200 * time.Millisecond

	// scmVerifyTimeout bounds the wait for a restarted instance to run the
	// registered program.
	//
	// The wait is sized above the readiness budget of the layer, because the
	// service control manager reports a started instance as running only after
	// the instance waited for its HTTP server; the margin stays below the
	// independent outer wait of the desktop helper (60 s).
	scmVerifyTimeout = scmReadinessTimeout + 15*time.Second

	// scmVerifyPollInterval is the pause between two checks of a restarted
	// instance.
	scmVerifyPollInterval = 200 * time.Millisecond

	// scmMaxImagePathLength bounds the buffer of the image path of a process.
	// The path of a program is far below it; the bound keeps a platform that
	// keeps asking for a larger buffer from looping forever.
	scmMaxImagePathLength = 32 * 1024
)

// scmStopBudget bounds the graceful stop; a test shortens it.
var scmStopBudget = scmStopTimeout

// scmVerifyBudget bounds the wait for a restarted instance to run; a test
// shortens it.
var scmVerifyBudget = scmVerifyTimeout

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
// The installation reads the registration, validates the effective options,
// prepares the directories, restricts the access to them, checks the
// directories the installation reuses, writes the registration and registers
// the event log source.
//
// The registration is read before anything is changed: an installation over an
// existing registration keeps every setting the request did not name, because
// the registration describes the instance that runs. The program always comes
// from the request, and so does the autostart of a first installation: a
// service an administrator switched to a manual or a delayed automatic start
// keeps that state.
//
// The state of the instance is restricted before the registration is written,
// so the directories the instance creates inherit the restrictive descriptor.
// A path that existed before the installation keeps its descriptor: it belongs
// to whoever named it, so only its descriptor is read, to check that the account
// of the instance can work with it, and a refusal happens before the first
// descriptor is changed. The reused directories are checked the same way.
//
// A service that is registered already is updated instead of registered a
// second time, so the instance keeps its identity.
//
// An instance that ran before the installation is restarted and verified: a
// registration alone does not switch the running process. The installation only
// succeeds when the instance runs the registered program in a process that is
// not the one it ran in before. A stopped, broken or absent instance and a
// first installation are never started.
//
// The registration carries the failure recovery of the platform: the service
// control manager restarts an instance that ends without reporting its stop,
// and, with the non-crash switch, one that ends with an error code. The switch
// is documented to take effect at the next start of the machine, so the
// installation reports the policy it wrote and never a recovery the running
// system did not perform yet. A policy that is registered already is kept.
//
// A failure after the registration was written restores the previous one; a
// failure after the restart of a running instance stops the instance the failed
// installation started, restores the previous registration and brings the
// previous instance back. A failed first installation removes the registration
// it created and the directories the installation created while they are still
// empty; a directory that existed before is never touched.
//
// The returned options are the registration as written: the merged options of a
// reinstall, or the request of a first installation. Created reports whether this
// call wrote the registration.
func (m *scmManager) Install(ctx context.Context, opts InstallOptions) (InstallOptions, error) {
	installed, _, err := m.install(ctx, opts, false)
	return installed, err
}

// InstallAndStart is the desktop helper's first-install path. It only creates
// an absent registration and keeps the creation handle through start,
// readiness/version verification and any failure cleanup.
func (m *scmManager) InstallAndStart(ctx context.Context, opts InstallOptions) (InstallStartResult, error) {
	installed, status, err := m.install(ctx, opts, true)
	if err != nil {
		return InstallStartResult{}, err
	}
	return InstallStartResult{Installation: installed, Status: status}, nil
}

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
	handle, err := manager.OpenService(ServiceName)
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
		previousConfig, err := handle.Config()
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
			status, err := handle.Query()
			if err != nil {
				return result, scmRefused("an installation", fmt.Errorf("service: query the state of %s: %w", ServiceName, err))
			}
			previous.present = true
			previous.running = scmInstanceRan(true, status.State)
			previous.processID = status.ProcessId
		}
		previous.config = previousConfig

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

func scmStatusFromTarget(target interface {
	Query() (svc.Status, error)
	Config() (mgr.Config, error)
}) (Status, svc.Status, error) {
	status, err := target.Query()
	if err != nil {
		return Status{}, svc.Status{}, err
	}
	config, err := target.Config()
	if err != nil {
		return Status{}, svc.Status{}, err
	}
	result, err := scmStatusFrom(ScopeSystem, scmRegistrationFacts{
		present:     true,
		state:       status.State,
		exitCode:    status.Win32ExitCode,
		processID:   status.ProcessId,
		commandLine: config.BinaryPathName,
	})
	return result, status, err
}

// scmRollbackMode tells a rollback how much of the previous installation to put
// back.
type scmRollbackMode int

const (
	// scmRollbackRegistration writes the previous registration back and nothing
	// else. It is the rollback of a failure that never touched the running
	// instance: the process that runs keeps the image it was started from.
	scmRollbackRegistration scmRollbackMode = iota
	// scmRollbackRestart stops the instance the failed installation started,
	// writes the previous registration back and brings the previous program of
	// a running instance back into service.
	scmRollbackRestart
	// scmRollbackStop settles a stop the installation accepted or that may
	// still put the instance down.
	scmRollbackStop
	// scmRollbackUndeliveredStop restores the registration after a stop that
	// was never delivered. No control reached the instance, so the restore
	// does not touch the running process.
	scmRollbackUndeliveredStop
)

// scmStopRollbackMode returns the rollback a failed stop needs: an accepted or
// unreadable stop may still put the instance down, while an undelivered control
// leaves it running behind a registration that has to be restored.
func scmStopRollbackMode(outcome scmStopOutcome) scmRollbackMode {
	switch outcome {
	case scmStopUnsettled:
		return scmRollbackStop
	case scmStopNotDelivered:
		return scmRollbackUndeliveredStop
	default:
		return scmRollbackRegistration
	}
}

// rollbackInstallation restores the configuration of the registration a failed
// installation replaced.
//
// The failure is the error the caller gets, and the outcome of the restore is
// reported next to it. The restore is the configuration and not the whole
// registration: the failure recovery is left as the installation configured it.
//
// The running instance is only touched when the installation switched it, or
// when its stop may still put it down; the stop is proven before the restore.
// A stop that was never delivered is not such a case: the restore does not
// touch the process, and the previous registration describes it again.
func (m *scmManager) rollbackInstallation(ctx context.Context, target scmRecoveryTarget, previous previousSCMRegistration, mode scmRollbackMode, failure error) (createdDirsSafeToRemove bool, err error) {
	if previous.present && previous.running {
		switch mode {
		case scmRollbackRestart:
			return m.rollbackRestart(ctx, target, previous, failure, scmRollbackStartedInstance)
		case scmRollbackStop:
			return m.rollbackRestart(ctx, target, previous, failure, scmRollbackPreviousInstance)
		case scmRollbackUndeliveredStop:
			return m.rollbackUndeliveredStop(ctx, target, previous, failure)
		}
	}
	if err := restoreSCMRegistration(target, previous); err != nil {
		return false, fmt.Errorf("%w (the registration of the failed installation could not be put back: %v)", failure, err)
	}
	if !previous.present {
		// The failed installation created the registration: there was no
		// previous one to restore, and the registration is removed again.
		return true, fmt.Errorf("%w (the registration of the failed installation was removed)", failure)
	}
	return true, fmt.Errorf("%w (the configuration of the previous registration was restored, the running instance was not restarted; the failure recovery of the registration is not part of the restore)", failure)
}

type scmRollbackAttempt int

const (
	scmRollbackStartedInstance scmRollbackAttempt = iota
	// scmRollbackPreviousInstance is an instance that may still run.
	scmRollbackPreviousInstance
	// scmRollbackUndeliveredInstance is the instance the failed installation
	// replaced, whose stop was never delivered.
	scmRollbackUndeliveredInstance
)

// scmRestartRollback is what the restart rollback of a failed installation
// proved about the machine.
//
// The facts are what was observed, not what was attempted, so no sentence
// composed from them claims more than the rollback can prove. They describe the
// configuration and the running instance, not the whole registration: the
// restore leaves the failure recovery in place.
type scmRestartRollback struct {
	// attempt names the instance the rollback had to settle.
	attempt scmRollbackAttempt
	stopped bool
	// restored reports whether the configuration of the previous
	// registration is in place again. The failure recovery is not part of
	// that configuration, and the rollback never restores it.
	restored bool
	// programKnown reports whether the restored registration names the
	// program the previous instance ran.
	programKnown bool
	// started reports whether the instance was started again and the start
	// was accepted.
	started bool
	// running reports that the state of the instance was read as running
	// after the restore. A stop that was never delivered leaves it running.
	running bool
	// verified reports whether the running process was proven to be the
	// previous program, in a process that is none of the processes of the
	// failed attempt.
	verified bool
}

// scmStopStatusHint is the next step after a stop that was not settled.
const scmStopStatusHint = `run "hfdesk service status" to read the state of the service before another start`

// scmRestartRollbackSentence returns the sentence that reports the proven
// outcome of a restart rollback next to the failure of the installation.
//
// It is the only place the outcome is described, so a caller reads no sentence
// for a state the rollback did not reach: the claim that the previous instance
// was started again is made for a verified rollback only, and the sentence of a
// completed rollback says that the failure recovery stays in place.
//
// A stop that was never delivered is reported as the restore it is: the instance
// was not asked to stop, so the restore needed no stop and the sentence names
// the running state instead.
func scmRestartRollbackSentence(facts scmRestartRollback, reason string) string {
	previousInstance := facts.attempt == scmRollbackPreviousInstance || facts.attempt == scmRollbackUndeliveredInstance
	if facts.attempt == scmRollbackUndeliveredInstance {
		switch {
		case !facts.restored:
			return fmt.Sprintf("no stop control was delivered to the instance the failed installation replaced, but the configuration of the previous registration could not be put back: %s", reason)
		case facts.running:
			return "no stop control was delivered to the instance the failed installation replaced and the configuration of the previous registration was restored; the instance still runs and no instance was started"
		case !facts.stopped:
			return fmt.Sprintf("no stop control was delivered to the instance the failed installation replaced and the configuration of the previous registration was restored, but the running state of the instance could not be confirmed, so no instance was started: %s; %s", reason, scmStopStatusHint)
		}
	}
	switch {
	case !facts.stopped:
		// No instance is started: a second one would compete with it.
		if previousInstance {
			return fmt.Sprintf("the registration of the failed installation was kept because the instance it replaced was not proven to be stopped: %s: the previous registration was not restored and no instance was started; %s", reason, scmStopStatusHint)
		}
		return fmt.Sprintf("the registration of the failed installation was kept because the instance it started was not proven to be stopped: %s: the previous registration was not restored and no instance was started; %s", reason, scmStopStatusHint)
	case !facts.restored:
		if previousInstance {
			return fmt.Sprintf("the previous instance was proven stopped, but the previous registration could not be put back: %s", reason)
		}
		return fmt.Sprintf("the instance of the failed installation was stopped, but the previous registration could not be put back: %s", reason)
	case !facts.programKnown:
		// The restored registration carries a command line that cannot be
		// read, so there is no program to start: the rollback reports the
		// registration and the state of the instance and never claims a
		// start it did not make.
		if previousInstance {
			return fmt.Sprintf("the previous instance was proven stopped and the configuration of the previous registration was restored, but the restored registration names no program, so no instance was started: the instance of %s is stopped", ServiceName)
		}
		return fmt.Sprintf("the instance of the failed installation was stopped and the configuration of the previous registration was restored, but the restored registration names no program, so no instance was started: the instance of %s is stopped", ServiceName)
	case !facts.started:
		// The start was refused, or it was accepted and the instance never
		// proved to run the previous program. Neither is a claim about the
		// program that runs: the reason names what was observed.
		if previousInstance {
			return fmt.Sprintf("the previous instance was proven stopped and the configuration of the previous registration was restored, but the previous instance was not proven to be in service again: %s", reason)
		}
		return fmt.Sprintf("the instance of the failed installation was stopped and the configuration of the previous registration was restored, but the previous instance was not proven to be in service again: %s", reason)
	case !facts.verified:
		if previousInstance {
			return "the previous instance was proven stopped and the configuration of the previous registration was restored and an instance was started, but the running process was not proven to be the previous program: the image of the process could not be read"
		}
		return "the instance of the failed installation was stopped and the configuration of the previous registration was restored and an instance was started, but the running process was not proven to be the previous program: the image of the process could not be read"
	default:
		if previousInstance {
			return "the previous instance was proven stopped, the configuration of the previous registration was restored and the previous instance was started again; the failure recovery of the registration was not part of the restore and stays as the installation left it"
		}
		return "the instance of the failed installation was stopped, the configuration of the previous registration was restored and the previous instance was started again; the failure recovery of the registration was not part of the restore and stays as the installation left it"
	}
}

// rollbackRestart stops the instance the rollback has to remove, restores the
// previous registration and brings the previous instance back into service.
//
// The instance is stopped before the previous registration is written back,
// because the restored registration would otherwise describe another program
// than the running one. The stop is a clean no-op for an instance that does not
// run, and it is bounded by the shared budget.
//
// A stop that does not reach its goal leaves the machine as the failed
// installation left it, and the sentence says exactly that. The previous
// program is started again only when the restored registration names it, and
// the restart is reported as a success only when it was verified.
func (m *scmManager) rollbackRestart(ctx context.Context, target scmRecoveryTarget, previous previousSCMRegistration, failure error, attempt scmRollbackAttempt) (createdDirsSafeToRemove bool, err error) {
	// The caller may have cancelled the context, the machine still has to be
	// put back the way it was.
	cleanup := context.WithoutCancel(ctx)
	facts := scmRestartRollback{attempt: attempt}
	report := func(reason string) error {
		return fmt.Errorf("%w (%s)", failure, scmRestartRollbackSentence(facts, reason))
	}

	// The process of the failed attempt is read before it is stopped: the
	// verification below excludes it, so a process the rollback did not start
	// is never taken for the previous instance.
	failedProcessID := scmRunningProcessID(target)

	if _, err := m.stopService(cleanup, target); err != nil {
		return false, report(err.Error())
	}
	facts.stopped = true

	if err := restoreSCMConfig(target, previous); err != nil {
		return false, report(err.Error())
	}
	facts.restored = true
	err = startPreviousProgram(cleanup, target, previous, &facts, report, previous.processID, failedProcessID)
	return facts.restored, err
}

// rollbackUndeliveredStop restores the registration after a stop that was never
// delivered and brings the previous instance back into service only when its
// state proved it stopped.
//
// No stop control reached the instance, so it was never asked to stop: writing
// the previous configuration back does not touch the process, and the restored
// registration names the program it was started from again. The state is read
// after the restore: a running instance needs nothing more, a settled stopped
// one is started and verified, and an unreadable or unsettled state is reported
// without a start.
func (m *scmManager) rollbackUndeliveredStop(ctx context.Context, target scmRecoveryTarget, previous previousSCMRegistration, failure error) (createdDirsSafeToRemove bool, err error) {
	// The machine still has to be put back after the caller cancelled.
	cleanup := context.WithoutCancel(ctx)
	facts := scmRestartRollback{attempt: scmRollbackUndeliveredInstance}
	report := func(reason string) error {
		return fmt.Errorf("%w (%s)", failure, scmRestartRollbackSentence(facts, reason))
	}

	if err := restoreSCMConfig(target, previous); err != nil {
		return false, report(err.Error())
	}
	facts.restored = true

	status, err := target.Query()
	if err != nil {
		return true, report(err.Error())
	}
	switch status.State {
	case svc.Running:
		facts.running = true
		return true, report("")
	case svc.Stopped:
		facts.stopped = true
	default:
		return true, report(fmt.Sprintf("the state of the instance was %v", status.State))
	}
	return true, startPreviousProgram(cleanup, target, previous, &facts, report, previous.processID)
}

// startPreviousProgram starts the program of the restored registration and
// records what the start proved. A registration that names no program has
// nothing to start; the caller's report says that.
func startPreviousProgram(ctx context.Context, target scmRecoveryTarget, previous previousSCMRegistration, facts *scmRestartRollback, report func(string) error, exclude ...uint32) error {
	executable := scmConfigExecutable(previous.config)
	if executable == "" {
		return report("")
	}
	facts.programKnown = true
	check, err := target.StartAndVerify(ctx, executable, exclude...)
	if err != nil {
		return report(err.Error())
	}
	facts.started = true
	facts.verified = check == scmImageVerified
	return report("")
}

// scmRecoveryTarget is the registration a failed installation restores. The
// interface lets the recovery run without a real registration.
type scmRecoveryTarget interface {
	scmServiceControl
	scmConfiguration
	Delete() error
	StartAndVerify(ctx context.Context, executable string, exclude ...uint32) (scmImageCheck, error)
}

// scmServiceRecoveryTarget is the recovery target of a real registration.
type scmServiceRecoveryTarget struct {
	manager *scmManager
	handle  *mgr.Service
}

func (t scmServiceRecoveryTarget) Query() (svc.Status, error) { return scmQueryStatus(t.handle) }

func (t scmServiceRecoveryTarget) ReadRunningImage(processID uint32, executable string) (bool, scmImageCheck, string) {
	return scmReadRunningImage(t.handle, processID, executable)
}

func (t scmServiceRecoveryTarget) Start() error { return scmStartService(t.handle) }

func (t scmServiceRecoveryTarget) Control(cmd svc.Cmd) (svc.Status, error) {
	return scmControlService(t.handle, cmd)
}

func (t scmServiceRecoveryTarget) UpdateConfig(config mgr.Config) error {
	return t.handle.UpdateConfig(config)
}

func (t scmServiceRecoveryTarget) Config() (mgr.Config, error) {
	return scmQueryServiceConfig(t.handle)
}

func (t scmServiceRecoveryTarget) Delete() error { return scmDeleteService(t.handle) }

func (t scmServiceRecoveryTarget) StartAndVerify(ctx context.Context, executable string, exclude ...uint32) (scmImageCheck, error) {
	return t.manager.startAndVerify(ctx, t.handle, executable, exclude...)
}

// scmRunningProcessID returns the process of the running instance, zero when the
// service control manager reports none.
//
// The process is read before a rollback stops the instance of the failed
// installation, so the verification of the restart can tell it apart from the
// instance the rollback starts. A state that cannot be read names no process
// either: the stop that follows reports the refusal.
func scmRunningProcessID(handle scmServiceControl) uint32 {
	status, err := handle.Query()
	if err != nil {
		return 0
	}
	return status.ProcessId
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
		return false, fmt.Errorf("%w (the registration of %s may be partially changed: the previous configuration could not be restored or verified: %v)", failure, ServiceName, err)
	}
	return true, fmt.Errorf("%w (the configuration of the previous registration of %s was restored; the running instance was not touched)", failure, ServiceName)
}

// restoreSCMRegistration puts the configuration of the registration back that a
// failed installation replaced.
//
// A registration of a first installation is removed. A replaced registration is
// written back, which proves the command line and thus the program of the
// instance. The write back leaves the failure recovery as the installation
// configured it.
func restoreSCMRegistration(target scmRecoveryTarget, previous previousSCMRegistration) error {
	if !previous.present {
		if err := target.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return fmt.Errorf("service: remove the registration of %s: %w", ServiceName, err)
		}
		return nil
	}
	return restoreSCMConfig(target, previous)
}

// restoreSCMConfig writes the configuration of the previous registration back
// and reads it back afterwards.
//
// It is the one place the configuration of a replaced registration is restored:
// the rollback of a failed installation and the failure path of a refused update
// both call it, so the two cannot drift apart. The read back proves the command
// line and thus the program of the instance; the failure recovery is not part of
// the configuration this restores.
func restoreSCMConfig(registration scmConfiguration, previous previousSCMRegistration) error {
	if !previous.present {
		return errors.New("service: the failed installation created the registration, so there is no configuration to restore")
	}
	if err := registration.UpdateConfig(previous.config); err != nil {
		return fmt.Errorf("service: write the previous registration of %s back: %w", ServiceName, err)
	}
	config, err := registration.Config()
	if err != nil {
		return fmt.Errorf("service: read the restored registration of %s back: %w", ServiceName, err)
	}
	if !scmSameRegistration(config.BinaryPathName, previous.config.BinaryPathName) {
		return fmt.Errorf("service: the registration of %s does not hold the previous command line again", ServiceName)
	}
	return nil
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

// scmImageCheck is how much of the running instance an installation could prove.
type scmImageCheck int

const (
	// scmImageUnverified means the service control manager reports the instance
	// as running and the registration carries the program, but the image of the
	// running process could not be read: the platform refused its handle.
	scmImageUnverified scmImageCheck = iota
	// scmImageVerified means the running process really is the registered
	// program.
	scmImageVerified
)

// startAndVerify starts the instance and verifies that it runs the given
// program in a process that is not one of the excluded ones.
func (m *scmManager) startAndVerify(ctx context.Context, handle *mgr.Service, executable string, exclude ...uint32) (scmImageCheck, error) {
	if err := scmStartForVerification(handle); err != nil {
		return scmImageUnverified, err
	}
	check, err := m.verifyRunningExecutable(ctx, handle, executable, exclude...)
	return check, err
}

func scmStartForVerification(handle *mgr.Service) error {
	if err := scmStartService(handle); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return scmRefused("a start", fmt.Errorf("service: start %s: %w", ServiceName, err))
	}
	return nil
}

// startAndVerifyVersion verifies readiness and, when supplied, the version the
// health endpoint reports. An update needs readiness even for dev or unknown
// expected versions; rollback uses startAndVerify's image-only proof instead.
func (m *scmManager) startAndVerifyVersion(ctx context.Context, handle *mgr.Service, executable, listen, expectedVersion string, exclude ...uint32) (scmImageCheck, bool, error) {
	if err := scmStartForVerification(handle); err != nil {
		return scmImageUnverified, false, err
	}
	return m.verifyRunningExecutableVersion(ctx, handle, executable, listen, expectedVersion, exclude...)
}

// verifyRunningExecutable waits until the instance runs the given program in a
// process that is not one of the excluded ones.
//
// The service control manager reports the process of the running instance, and
// its image is compared with the program: a registration that carries the
// program proves nothing while a process of the failed attempt still runs. The
// wait is bounded by the verification budget and the context.
func (m *scmManager) verifyRunningExecutable(ctx context.Context, handle *mgr.Service, executable string, exclude ...uint32) (scmImageCheck, error) {
	result, err := m.verifyRunningExecutableVersionUsing(ctx,
		scmServiceRecoveryTarget{manager: m, handle: handle}, executable, "", "", false, exclude...)
	return result.imageCheck, err
}

// scmQueryStatus reads the state of the instance during a verification. The
// variable is the seam of the test: a test verifies without a registration of
// the service control manager.
var scmQueryStatus = (*mgr.Service).Query
var scmQueryServiceConfig = (*mgr.Service).Config
var scmStartService = func(service *mgr.Service) error { return service.Start() }
var scmControlService = (*mgr.Service).Control
var scmDeleteService = (*mgr.Service).Delete
var scmQueryProcessImageName = queryProcessImageName
var scmQueryServiceCommandLine = queryServiceCommandLine

type scmRunningImageTarget interface {
	Query() (svc.Status, error)
	ReadRunningImage(processID uint32, executable string) (bool, scmImageCheck, string)
}

type scmStartVerification struct {
	imageCheck      scmImageCheck
	versionVerified bool
	processID       uint32
}

// verifyRunningExecutableVersion also waits for the expected version.
func (m *scmManager) verifyRunningExecutableVersion(ctx context.Context, handle *mgr.Service, executable, listen, expectedVersion string, exclude ...uint32) (scmImageCheck, bool, error) {
	result, err := m.verifyRunningExecutableVersionUsing(ctx,
		scmServiceRecoveryTarget{manager: m, handle: handle}, executable, listen, expectedVersion, true, exclude...)
	return result.imageCheck, result.versionVerified, err
}

func (m *scmManager) verifyRunningExecutableVersionUsing(ctx context.Context, target scmRunningImageTarget, executable, listen, expectedVersion string, requireReadiness bool, exclude ...uint32) (scmStartVerification, error) {
	deadline := time.Now().Add(scmVerifyBudget)
	detail := "the service control manager did not report the instance as running"
	var lastProcessID uint32
	lastImageCheck := scmImageUnverified
	for {
		status, err := target.Query()
		switch {
		case err != nil:
			detail = fmt.Sprintf("the state of %s cannot be read: %v", ServiceName, err)
		case status.State != svc.Running:
			detail = fmt.Sprintf("the service control manager reports the state %v", status.State)
		case status.ProcessId == 0:
			detail = "the service control manager reports no process of the running instance"
		case scmProcessExcluded(exclude, status.ProcessId):
			// A process of the attempt still runs: the program of the
			// registration is not in service yet.
			detail = fmt.Sprintf("the process %d that ran before the attempt still runs", status.ProcessId)
		default:
			lastProcessID = status.ProcessId
			settled, check, reason := target.ReadRunningImage(status.ProcessId, executable)
			if settled {
				lastImageCheck = check
				if expectedVersion == "" && !requireReadiness {
					return scmStartVerification{imageCheck: check, processID: status.ProcessId}, nil
				}
				verified, probeErr := serviceInstallProbe(listen, expectedVersion)
				if probeErr == nil {
					return scmStartVerification{imageCheck: check, versionVerified: verified, processID: status.ProcessId}, nil
				}
				if errors.Is(probeErr, errServiceVersionMismatch) {
					return scmStartVerification{imageCheck: check, processID: status.ProcessId}, fmt.Errorf("the instance does not run the expected version: %w", probeErr)
				}
				detail = fmt.Sprintf("the instance does not answer on %s: %v", probeAddressOrListen(listen), probeErr)
			} else {
				// The image read did not settle the question: its reason is
				// the only detail there is.
				detail = reason
			}
		}
		if !time.Now().Before(deadline) {
			return scmStartVerification{imageCheck: lastImageCheck, processID: lastProcessID}, fmt.Errorf("the instance does not run %q within %s: %s", executable, scmVerifyBudget, detail)
		}
		select {
		case <-ctx.Done():
			return scmStartVerification{imageCheck: lastImageCheck, processID: lastProcessID}, ctx.Err()
		case <-time.After(scmVerifyPollInterval):
		}
	}
}

func probeAddressOrListen(listen string) string {
	if listen == "" {
		return "the instance"
	}
	return probeServiceAddress(listen)
}

// scmProcessExcluded reports whether a process is one the verification must not
// accept as the running instance.
//
// A process identifier of zero names no process: the manager reports it for an
// instance that does not run, and it never excludes another process. The list of
// excluded processes can carry it, because the caller passes the process the
// service control manager reported even when it reported none.
func scmProcessExcluded(exclude []uint32, processID uint32) bool {
	if processID == 0 {
		return false
	}
	return slices.Contains(exclude, processID)
}

// scmReadRunningImage reads the image of the running process and compares it
// with the program the registration carries.
//
// The first result reports whether the observation settles the question, the
// second one is the proof the installation has, the third one the reason a look
// did not settle it. The image of a process of another account can be refused
// to the installation; the wait then accepts the evidence it has (the running
// state and the program of the registration) and the caller is told that the
// image was not compared.
func scmReadRunningImage(handle *mgr.Service, processID uint32, executable string) (bool, scmImageCheck, string) {
	if executable == "" {
		// The program is unknown: there is nothing the image of the process
		// could be compared with, so the installation reports the running
		// state alone.
		return true, scmImageUnverified, ""
	}
	image, err := scmQueryProcessImageName(processID)
	switch {
	case err == nil:
		if scmSameExecutablePath(image, executable) {
			return true, scmImageVerified, ""
		}
		return false, scmImageUnverified, fmt.Sprintf("the running process %d runs %q and not %q", processID, image, executable)
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		pointsAt, err := scmRegistrationPointsAt(handle, executable)
		if err != nil {
			return false, scmImageUnverified, fmt.Sprintf("the registration of %s cannot be read back: %v", ServiceName, err)
		}
		if pointsAt {
			return true, scmImageUnverified, ""
		}
		return false, scmImageUnverified, fmt.Sprintf("the registration of %s does not carry the program %q", ServiceName, executable)
	default:
		return false, scmImageUnverified, fmt.Sprintf("the image of the running process %d cannot be read: %v", processID, err)
	}
}

// scmRegistrationPointsAt reports whether the registration of the instance
// carries the given program.
//
// The command line is read from the service control manager and not from the
// configuration the installation wrote, so the answer describes the registration
// that is really there.
func scmRegistrationPointsAt(handle *mgr.Service, executable string) (bool, error) {
	commandLine, err := scmQueryServiceCommandLine(handle.Handle)
	if err != nil {
		return false, err
	}
	registration, err := parseSCMCommandLine(commandLine)
	if err != nil {
		return false, nil
	}
	return scmSameExecutablePath(registration.executable, executable), nil
}

// queryProcessImageName returns the image of a process.
//
// The handle of the process asks for the least right that reads its image: the
// right to query the limited information of the process. The manager never opens
// a process with the rights that would let it change one.
func queryProcessImageName(processID uint32) (string, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, processID)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)

	size := uint32(windows.MAX_PATH)
	for {
		buffer := make([]uint16, size)
		length := size
		err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &length)
		if err == nil {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return "", err
		}
		// The image path of a program can be longer than the buffer: the
		// buffer grows until the path fits.
		if length > size {
			size = length
		} else {
			size *= 2
		}
		if size > scmMaxImagePathLength {
			return "", err
		}
	}
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
func scmLookupRegistration(name string) (scmRegistrationFacts, error) {
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

// scmServiceControl is the part of a registration the stop of an instance
// needs.
//
// The interface is the seam of the test: the manager talks to the service
// control manager through it, so the retry of a refused stop can be driven
// from a test without a registration of its own.
type scmServiceControl interface {
	// Query returns the state of the registration.
	Query() (svc.Status, error)
	// Control sends a control request to the registration.
	Control(c svc.Cmd) (svc.Status, error)
}

// scmStopOutcome tells what a failed stop proved.
type scmStopOutcome int

const (
	scmStopSettled scmStopOutcome = iota
	// scmStopNotDelivered reports a stop no control reached: the instance was
	// never asked to stop.
	scmStopNotDelivered
	// scmStopUnsettled reports a stop that may still put the instance down.
	scmStopUnsettled
)

// stopService stops the instance and waits for the graceful shutdown to
// finish.
//
// Stopping an instance that does not run is a stable success: the state the
// caller asked for already holds.
//
// The stop control is delivered again while the wait lasts: the service
// control manager refuses the control of an instance that is still starting
// (ERROR_SERVICE_CANNOT_ACCEPT_CTRL), and the instance of the platform
// accepts the control only after it reported the running state. A refusal
// is therefore not the end of the stop: the control is delivered again on
// the next step of the wait, until the instance stops or the budget of the
// wait is over.
//
// The wait is bounded by the shared budget of the service layer, so the
// outcome is the same on every machine: the instance stopped, or the wait
// is over and the instance is still running. A failure distinguishes a control
// that was never delivered from one that may still stop the instance: a state
// that cannot be read before any control was accepted proves no delivery.
func (m *scmManager) stopService(ctx context.Context, handle scmServiceControl) (scmStopOutcome, error) {
	deadline := time.Now().Add(scmStopBudget)
	delivered := false
	stopping := false
	for {
		status, err := handle.Query()
		if err != nil {
			// An accepted control, or an instance that was seen stopping, may
			// still put the instance down. Before that the error proves no
			// stop was requested.
			if delivered || stopping {
				return scmStopUnsettled, scmRefused("a stop", fmt.Errorf("service: query the state of %s: %w", ServiceName, err))
			}
			return scmStopNotDelivered, scmRefused("a stop", fmt.Errorf("service: query the state of %s: %w", ServiceName, err))
		}
		if status.State == svc.StopPending {
			stopping = true
		}
		if status.State == svc.Stopped {
			return scmStopSettled, nil
		}
		if !delivered {
			if _, err := handle.Control(svc.Stop); err != nil {
				switch {
				case errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE):
					// The instance is not running: the wanted state
					// already holds.
					return scmStopSettled, nil
				case errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL):
					// The control is delivered again inside the wait.
					log.Printf("service: %s does not accept a stop control (the state of the instance is %v), the stop request is delivered again", ServiceName, status.State)
				default:
					if status.State == svc.StopPending {
						return scmStopUnsettled, scmRefused("a stop", fmt.Errorf("service: stop %s: %w", ServiceName, err))
					}
					return scmStopNotDelivered, scmRefused("a stop", fmt.Errorf("service: stop %s: %w", ServiceName, err))
				}
			} else {
				// The supervisor accepted the control: the instance
				// stops.
				delivered = true
			}
		}
		if time.Now().After(deadline) {
			if delivered || status.State == svc.StopPending {
				return scmStopUnsettled, fmt.Errorf("service: %s did not stop within %s (the state of the instance was %v)", ServiceName, scmStopBudget, status.State)
			}
			return scmStopNotDelivered, fmt.Errorf("service: %s did not stop within %s (the state of the instance was %v)", ServiceName, scmStopBudget, status.State)
		}
		select {
		case <-ctx.Done():
			if delivered || status.State == svc.StopPending {
				return scmStopUnsettled, ctx.Err()
			}
			return scmStopNotDelivered, ctx.Err()
		case <-time.After(scmStopPollInterval):
		}
	}
}

// preflight rejects an installation with an unusable executable, a PE machine
// the host is known not to support, an invalid bind address or a directory the
// account of the instance is proven unable to use.
//
// The preflight runs before the registration is written, so a rejected
// installation never leaves a service behind that fails later with an
// opaque access error.
func (m *scmManager) preflight(opts InstallOptions, pathAccess ...scmPathAccess) error {
	if !filepath.IsAbs(opts.Executable) {
		return fmt.Errorf("service: executable %q must be an absolute path", opts.Executable)
	}
	info, err := os.Stat(opts.Executable)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("service: executable %q is not usable: %w", opts.Executable, err)
		}
		log.Printf("service: executable metadata for %q cannot be inspected (%v); the installer did not classify the executable as invalid", opts.Executable, err)
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("service: executable %q is not a regular file", opts.Executable)
	}
	if _, _, err := ParseListen(opts.Listen); err != nil {
		return err
	}
	return preflightDataPaths(opts, pathAccess...)
}

// scmExecutableAccess is what the access check of the registered program proved.
type scmExecutableAccess int

const (
	scmExecutableAccessInconclusive scmExecutableAccess = iota
	scmExecutableAccessAllowed
	scmExecutableAccessDenied
)

var scmCheckExecutableAccess = scmExecutableAccessOf

// preflightExecutableAccess refuses a program the account cannot execute, and
// a parent it cannot traverse. The program image is checked for execute only:
// Windows supports an image the account runs without reading it, so read data
// is not demanded of the program. An unreadable descriptor is not a denial:
// the installation reports that it could not check.
func preflightExecutableAccess(executable string, pathAccess ...scmPathAccess) error {
	var effective scmPathAccess
	if len(pathAccess) != 0 {
		effective = pathAccess[0]
	}
	access, reason := scmCheckExecutableAccess(executable, effective)
	switch access {
	case scmExecutableAccessDenied:
		return fmt.Errorf("service: executable %q is not usable by the account the instance runs as (%s): %s: grant the account execute access to the program, or choose a program the account can use", executable, scmServiceIdentityName, reason)
	case scmExecutableAccessInconclusive:
		log.Printf("service: effective access to executable %q for %s is inconclusive (%s)", executable, scmServiceIdentityName, reason)
	}
	return nil
}

func scmExecutableAccessOf(executable string, access scmPathAccess) (scmExecutableAccess, string) {
	// Only the execute right is required of the program image. GENERIC_READ
	// would demand the read data that an execute-only program legitimately
	// withholds, and GENERIC_EXECUTE would additionally demand read-control,
	// read-attributes and synchronize rights that a bare "Execute file"
	// access control entry does not carry. Every access control entry form
	// that grants execution grants FILE_EXECUTE, so a program the account
	// cannot execute is still refused by this mask.
	return scmCheckPathAccess(executable, windows.FILE_EXECUTE, access)
}

// scmParentDirs returns the directories above a path, the immediate parent first.
func scmParentDirs(path string) []string {
	var dirs []string
	current := filepath.Dir(filepath.Clean(path))
	for {
		dirs = append(dirs, current)
		parent := filepath.Dir(current)
		if parent == current {
			return dirs
		}
		current = parent
	}
}

const (
	scmServiceIdentitySID  = "S-1-5-19"
	scmServiceIdentityName = `NT AUTHORITY\LocalService`
)

// preflightDataPaths checks the directories an installation reuses: the
// cache root, the hub directory and the local output directory.
//
// The directories do not belong to the instance: they can hold the models
// and the cache of an interactive run of the user. Their access control
// list is only read, never changed, and never changed recursively.
//
// A directory that does not exist yet and that really lives inside the
// state directory of the instance belongs to the instance: the state
// directory belongs to the account the instance runs as, and the instance
// creates the cache tree it needs inside it. Such a directory is neither
// checked nor created.
//
// The exemption covers a directory that does not exist yet only. A directory
// that exists already carries the access control list of whoever owns it,
// and owning the state directory grants no right on a directory inside it:
// a child of the state root that the account of the instance cannot write
// in is a directory the instance cannot work with. Such a directory is
// checked like every other named directory, wherever it lives.
//
// The real location of the two paths decides that: the platform follows the
// links of a path before it uses it, so a directory that is reached through
// a link lives where its target lives and not where the link is. A link
// that leaves the state directory is a directory outside of it, and it is
// checked like every other named directory.
//
// A directory the user named is outside of the instance: the account of the
// instance has to be able to work with it. A directory that exists already is
// checked against the account of the instance, wherever it lives.
//
// A directory that does not exist yet is checked through its nearest
// existing ancestor: the account has to be able to create the directory
// there, because the instance creates it when it runs, as the account the
// instance runs as.
//
// The installation never creates a directory the user named: it belongs to
// whoever named it, and the installation only reads the access control list
// of the ancestor it judges.
func preflightDataPaths(opts InstallOptions, pathAccess ...scmPathAccess) error {
	var access scmPathAccess
	if len(pathAccess) != 0 {
		access = pathAccess[0]
	}
	for _, dir := range []struct {
		name string
		path string
	}{
		{"cache dir", opts.CacheDir},
		{"hub dir", opts.HubDir},
		{"local dir", opts.LocalDir},
	} {
		if dir.path == "" {
			continue
		}
		if pathMissing(dir.path) {
			// The directory does not exist yet. The instance creates
			// the cache tree it needs inside the state directory it
			// owns, so such a directory is neither checked nor
			// created.
			inside, err := scmPathInsideStateRoot(opts.Paths.StateDir, dir.path)
			if err != nil {
				// The real location of the path cannot be read: the
				// exemption is not granted, and the path is checked
				// like every other named directory. The check below is
				// the safe answer, and the reason is logged.
				log.Printf("service: the real location of the %s %q cannot be read (%v): the path is checked like every other named directory", dir.name, dir.path, err)
				inside = false
			}
			if inside {
				// The default of the scope lives inside the state
				// directory of the instance, and the state directory
				// belongs to the account of the instance.
				continue
			}
		}
		// The directory is checked against the account of the instance,
		// wherever it lives: a directory that exists already is checked
		// against its own access control list, and a directory that does
		// not exist yet is checked through its nearest existing ancestor.
		// The check reads the access control list of the real location of
		// the directory.
		if err := preflightDataPath(dir.name, dir.path, access); err != nil {
			return err
		}
	}
	return nil
}

// scmPathInsideStateRoot reports whether a named directory really lives
// inside the state root of the instance.
//
// The decision is made on the real location of the two paths: the platform
// follows the links of a path before it uses it, so a directory that is
// reached through a link lives where its target lives and not where the link
// is.
//
// The decision asks nothing about the account of the instance: the exemption
// is a fact about the two paths. A tree that does not exist yet is judged by
// the place it will live in, and the account of the instance is never asked
// about a tree that is not there.
func scmPathInsideStateRoot(stateRoot, path string) (bool, error) {
	if stateRoot == "" {
		return false, nil
	}
	root, err := scmResolvePath(stateRoot)
	if err != nil {
		return false, err
	}
	target, err := scmResolvePath(path)
	if err != nil {
		return false, err
	}
	return hfdownloader.PathInside(root, target), nil
}

// scmResolvePath returns the real location of a path.
//
// The platform follows the symbolic links and the reparse points of a path
// before it uses it, so the real location of a path is the one that decides
// whether the account of the instance can work with it.
//
// The resolution follows the links of the path as far as the path exists:
// the parts that are missing keep the location they were written with, so a
// directory that is created later is judged by the place it will live in.
func scmResolvePath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	// The path does not exist: the deepest ancestor of it that exists is
	// resolved, and the parts that are missing are appended to it.
	existing, missing, err := scmSplitExistingPath(path)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{resolved}, missing...)...), nil
}

// scmSplitExistingPath splits a path into the deepest ancestor of it that
// exists and the parts that are missing below it.
//
// The root of a volume is the end of the walk: a path below a volume that
// does not exist cannot be resolved.
func scmSplitExistingPath(path string) (string, []string, error) {
	var missing []string
	current := filepath.Clean(path)
	for {
		if _, err := os.Lstat(current); err == nil {
			return current, missing, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", nil, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", nil, fmt.Errorf("the path %q has no existing ancestor", path)
		}
		missing = append([]string{filepath.Base(current)}, missing...)
		current = parent
	}
}

// preflightDataPath checks that the account of the instance can work with
// the directory the installation reuses.
//
// A directory that exists already is checked against the account of the
// instance: the access control list of the directory decides whether the
// account can read it and write in it.
//
// A directory that does not exist yet is checked through its nearest
// existing ancestor: the instance creates the directory when it runs, as the
// account the instance runs as, so the account has to be able to create the
// directory there.
//
// The installation never creates a directory the user named: it belongs to
// whoever named it, and the installation only reads the access control list
// of the path it checks.
func preflightDataPath(name, path string, pathAccess ...scmPathAccess) error {
	var access scmPathAccess
	if len(pathAccess) != 0 {
		access = pathAccess[0]
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("service: %s %q must be an absolute path", name, path)
	}

	info, err := os.Stat(path)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("service: %s %q is not a directory: the instance needs a directory there", name, path)
		}
		return checkServiceAccess(name, path, access)
	case !errors.Is(err, fs.ErrNotExist):
		log.Printf("service: metadata for %s %q cannot be inspected (%v); path usability is inconclusive", name, path, err)
		return nil
	}

	// The directory does not exist yet. The instance creates it when it
	// runs, as the account the instance runs as, so the installation
	// judges it through its nearest existing ancestor: the account has to
	// be able to create the directory there.
	//
	// The installation never creates the directory: the directory and the
	// access rights of it belong to whoever named it, and an installation
	// that created one would take a tree of models over without being
	// asked to.
	return checkServiceCreateAccess(name, path, access)
}

// checkServiceCreateAccess checks that the account of the instance can
// create the directory the installation reuses.
//
// The directory does not exist yet: the instance creates it when it runs, as
// the account the instance runs as. The check therefore judges the nearest
// existing ancestor of the directory: the account has to be able to create
// entries in it, which is the access the instance needs on every directory
// it works with.
//
// The access control list of the ancestor is only read: the installation
// never creates the directory the user named and never applies a descriptor
// to a path it reuses.
//
// The path and its parents are checked with the effective token of the running
// LocalService instance, including the original and every observed
// reparse-resolved segment. Windows' AccessCheck applies DACL order; traversal
// checks are skipped only when that token reports SeChangeNotifyPrivilege enabled.
func checkServiceCreateAccess(label, path string, pathAccess ...scmPathAccess) error {
	var access scmPathAccess
	if len(pathAccess) != 0 {
		access = pathAccess[0]
	}
	sourceAncestor, ancestor, err := scmNearestExistingDirRoute(path)
	if err != nil {
		if errors.Is(err, errSCMNotDirectory) {
			return fmt.Errorf("service: the %s %q cannot be created because its nearest existing ancestor is not a directory: %w", label, path, err)
		}
		log.Printf("service: access to the nearest existing ancestor of %q for %s is inconclusive (%v)", path, scmServiceIdentityName, err)
		return nil
	}
	decision, reason := scmCheckPathAccessFrom(sourceAncestor, ancestor, windows.GENERIC_READ|windows.GENERIC_WRITE, access)
	switch decision {
	case scmExecutableAccessDenied:
		// The message names the directory, the account and the access
		// that is missing: the user has to grant the access to the
		// ancestor, and the name of the ancestor is what makes the
		// guidance actionable.
		return fmt.Errorf(
			"service: the %s %q does not exist and the account %s cannot create it: %s: the nearest existing ancestor %q has to grant the account read and write access: the installation never creates a directory the user named, the instance creates it when it runs, as the account the instance runs as: grant the account read and write access to the ancestor directory, or install the instance without this path",
			label, path, scmServiceIdentityName, reason, ancestor,
		)
	}
	if decision == scmExecutableAccessInconclusive {
		log.Printf("service: effective access to the nearest existing ancestor %q for %s is inconclusive (%s)", ancestor, scmServiceIdentityName, reason)
	}
	return nil
}

// scmNearestExistingDir returns the nearest existing ancestor directory of
// the path.
//
// The ancestor is the directory the instance creates the directory in: it is
// the deepest directory that exists on the way to the path. A path below a
// file has no such ancestor: a file can never hold a directory.
//
// The path is judged by its real location: the platform follows the links of
// a path before it uses it, so a directory that is reached through a link is
// created in the directory its target lives in.
func scmNearestExistingDir(path string) (string, error) {
	resolved, err := scmResolvePath(path)
	if err != nil {
		// The real location of the path cannot be read: the path as it
		// was written is the one that is judged.
		resolved = path
	}
	current := filepath.Clean(resolved)
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%q is not a directory: %w", current, errSCMNotDirectory)
			}
			return current, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing parent directory of %q was found", path)
		}
		current = parent
	}
}

var errSCMNotDirectory = errors.New("not a directory")

func scmNearestExistingDirRoute(path string) (sourceAncestor, resolvedAncestor string, err error) {
	resolvedAncestor, err = scmNearestExistingDir(path)
	if err != nil {
		return "", "", err
	}
	sourceAncestor, _, err = scmSplitExistingPath(path)
	if err != nil {
		return "", "", err
	}
	return sourceAncestor, resolvedAncestor, nil
}

// checkServiceAccess uses Windows AccessCheck with the effective service token
// to decide whether the account can work with a path.
//
// The access control list is only read: the descriptor of the instance is
// never applied to a path the installation reuses.
func checkServiceAccess(label, path string, pathAccess ...scmPathAccess) error {
	var access scmPathAccess
	if len(pathAccess) != 0 {
		access = pathAccess[0]
	}
	decision, reason := scmCheckPathAccess(path, windows.GENERIC_READ|windows.GENERIC_WRITE, access)
	switch decision {
	case scmExecutableAccessDenied:
		return fmt.Errorf("service: the %s %q is not usable by the account the instance runs as (%s): %s: grant the account read and write access to the path, or install the instance without this path", label, path, scmServiceIdentityName, reason)
	case scmExecutableAccessInconclusive:
		log.Printf("service: effective access to %s %q for %s is inconclusive (%s)", label, path, scmServiceIdentityName, reason)
	}
	return nil
}

// requireElevated rejects a machine-wide operation that needs an elevated
// shell when the process does not have one.
//
// The manager never escalates privileges itself: it never starts a runas
// call, never starts PowerShell and never starts a shell.
func requireElevated(action string) error {
	return requireElevatedFor(action, isElevatedProcess())
}

// requireElevatedFor rejects a machine-wide operation when the process does
// not hold an elevated token.
//
// The state of the token is a parameter, so the decision can be driven from
// both sides without an elevated shell: the real check passes the state of
// the token of the process, and the test passes the state of the two cases.
func requireElevatedFor(action string, elevated bool) error {
	if elevated {
		return nil
	}
	return fmt.Errorf("service: %s needs an elevated shell: run the command as an administrator, HFDesk never escalates privileges itself", action)
}

// scmElevationRequired reports that the service control manager refused an
// operation because the shell of the caller has no rights on the
// registration.
//
// The elevation requirement is the truthful reason: the registration of a
// machine-wide instance belongs to the machine, so a shell that is not an
// administrator shell can neither read it nor change it. The manager never
// escalates privileges itself.
func scmElevationRequired(action string) error {
	return fmt.Errorf("service: %s needs an elevated shell: the service control manager refused the operation because the shell has no rights on the registration of %s, run the command as an administrator, HFDesk never escalates privileges itself", action, ServiceName)
}

// scmRefused reports the reason of a refused operation of the service
// control manager.
//
// A refusal because the shell of the caller has no rights on the
// registration is reported as the elevation requirement it is: the raw
// access denied error of the platform does not tell the user what to do.
func scmRefused(action string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return scmElevationRequired(action)
	}
	return err
}

// isElevatedProcess reports whether the process holds an elevated token.
func isElevatedProcess() bool {
	return windows.GetCurrentProcessToken().IsElevated()
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

// cacheDir returns the cache root the registration carries.
//
// The registration holds the cache root the installation was made with. The
// effective cache root of the instance is the one of its own config file
// when that file defines one.
func (r scmRegistration) cacheDir() string {
	return r.flagValue("--cache-dir")
}

// flagValue returns the value of a flag of the argument vector.
func (r scmRegistration) flagValue(flag string) string {
	return runArgValue(r.args, flag)
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
	if strings.TrimSpace(parts[0]) == "" {
		return registration, errors.New("service: the registration carries no executable")
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
// The restrictive descriptor is applied to the paths the installation
// created: the directory of the config file, the state directory, the log
// directory and the config file itself. The descriptor is applied to the
// path itself and never recursively.
//
// A path that existed before the installation belongs to whoever owns it:
// its descriptor is only read, to check that the account of the instance can
// work with it. The check of every such path happens before the first
// descriptor is changed, so a refused installation leaves every descriptor
// of the machine as it was.
//
// The model and the cache trees of an installation are never re-owned and
// never re-ACLed: they can hold gigabytes of data that the user may reuse
// from an interactive run, and a recursive change of the access rights
// would take the data away from the user who owns it.
func restrictInstanceState(opts InstallOptions, snapshot scmInstallSnapshot, pathAccess ...scmPathAccess) error {
	var access scmPathAccess
	if len(pathAccess) != 0 {
		access = pathAccess[0]
	}
	paths := instanceStatePaths(opts, snapshot)
	// The paths that existed before the installation are checked first:
	// the account of the instance has to be able to work with them
	// before a single descriptor is changed.
	for _, path := range paths {
		if path.created {
			continue
		}
		if err := checkServiceAccess(path.name, path.path, access); err != nil {
			return err
		}
	}
	for _, path := range paths {
		if !path.created {
			continue
		}
		if err := applyRestrictiveACL(path.path); err != nil {
			return err
		}
	}
	return nil
}

// scmInstancePath is one path of the instance whose descriptor the
// installation manages.
type scmInstancePath struct {
	// name names the path in an error message.
	name string
	// path is the absolute path of the path.
	path string
	// created reports whether the installation creates the path itself.
	created bool
}

// instanceStatePaths returns the paths of the instance whose descriptor the
// installation manages: the directory of the config file, the state
// directory, the log directory and the config file itself.
//
// A path the installation creates is marked as created: the snapshot holds
// the directories an installation creates for every directory of the
// installation, and it is read before the directories are created, so the
// installation knows which of them it owns. The missing parents of such a
// directory belong to the installation as well.
//
// A path that existed before the installation is not marked: it belongs to
// whoever owns it.
//
// The config file is a file the instance writes itself: a file that existed
// before the installation is not marked, a file the installation created is.
//
// A path that is named twice is returned once: the config file can live in
// the state directory of the instance.
func instanceStatePaths(opts InstallOptions, snapshot scmInstallSnapshot) []scmInstancePath {
	var paths []scmInstancePath
	seen := make(map[string]bool, 4)
	add := func(name, path string, created bool) {
		if path == "" || path == "." || seen[path] {
			return
		}
		seen[path] = true
		paths = append(paths, scmInstancePath{name: name, path: path, created: created})
	}

	for _, dir := range []struct {
		name string
		path string
	}{
		{"config dir", filepath.Dir(opts.Paths.ConfigFile)},
		{"state dir", opts.Paths.StateDir},
		{"log dir", opts.Paths.LogDir},
	} {
		if dir.path == "" || dir.path == "." {
			continue
		}
		own := snapshot.created[dir.path]
		if len(own) == 0 {
			// The directory existed before the installation: it
			// belongs to whoever owns it.
			add(dir.name, dir.path, false)
			continue
		}
		// The installation creates the directory and the missing
		// parents of it: they belong to the installation.
		for _, path := range own {
			add(dir.name, path, true)
		}
	}

	// The config file holds the credentials of the instance. It is a
	// file the instance writes itself, so it is only a path of the
	// installation when it exists: a file that is written later inherits
	// the descriptor of its directory.
	if info, err := os.Stat(opts.Paths.ConfigFile); err == nil && info.Mode().IsRegular() {
		add("config file", opts.Paths.ConfigFile, snapshot.configFileMissing)
	}
	return paths
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
