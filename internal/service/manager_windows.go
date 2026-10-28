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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/bashrusakh/hfdesk/internal/pe"
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
// reinstall, or the request of a first installation.
func (m *scmManager) Install(ctx context.Context, opts InstallOptions) (InstallOptions, error) {
	// The registration is read before a directory is created: the merge of
	// the request needs the settings the instance runs with, and only an
	// instance that ran before is restarted.
	previous, err := scmPreviousRegistration(opts.Scope)
	if err != nil {
		return opts, err
	}
	if previous.present {
		opts = mergeInstallOptions(opts, previous.options)
	}

	if err := ValidateInstallOptions(opts); err != nil {
		return opts, err
	}
	// The elevation requirement is checked before a directory is created:
	// a permission error of the machine-wide directories is a much less
	// readable answer than the real reason.
	if err := requireElevated("an installation"); err != nil {
		return opts, err
	}
	// The paths the installation is about to create are remembered before
	// they are created, so the installation knows which of them it owns.
	// The list holds the directories of the instance only: a directory the
	// installation reuses belongs to whoever named it, and the installation
	// never creates one.
	snapshot := newSCMInstallSnapshot(opts)
	order := createdDirOrder(opts, snapshot.created)
	// A failed installation removes the directories it created. The
	// cleanup is best effort: it never replaces the error of the
	// installation, which is what the caller gets.
	fail := func(err error) error {
		removeCreatedDirs(order)
		return err
	}
	if err := PrepareDirs(opts); err != nil {
		// The preparation may have created some of the directories
		// before it failed: they are removed again, so a failed attempt
		// leaves the machine as it was.
		return opts, fail(err)
	}
	// The state of the instance is restricted before the reused
	// directories are checked, and a path that existed before the
	// installation is never rewritten: the check of every such path happens
	// before the first descriptor is changed.
	if err := restrictInstanceState(opts, snapshot); err != nil {
		return opts, fail(err)
	}
	if err := m.preflight(opts); err != nil {
		return opts, fail(err)
	}

	manager, err := mgr.Connect()
	if err != nil {
		return opts, fail(fmt.Errorf("service: connect to the service control manager: %w", err))
	}
	defer manager.Disconnect()

	// The registration is written and put into service. A failure restores
	// the previous registration, so the caller never keeps a registration
	// the installation failed to put into service.
	if err := m.register(ctx, manager, opts, previous); err != nil {
		return opts, fail(err)
	}

	// The event log source carries the diagnostics of the instance. A
	// source that cannot be registered is not a failed installation: the
	// administrator of the machine can register it separately, and the
	// instance falls back to the Application log.
	if err := registerEventSource(); err != nil {
		log.Printf("service: the event log source of %s was not registered: %v", ServiceName, err)
		logEvent(eventLogWarning, fmt.Sprintf("hfdesk: the event log source of %s was not registered: %v", ServiceName, err))
	}
	return opts, nil
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
		registration = scmRegistration{}
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
// identity. An update writes the start type and the delayed start the
// registration already has, so a service an administrator switched to a manual,
// a disabled or a delayed start stays that way; only a first installation is
// written with the automatic start.
//
// The directories of the instance are already created when the function is
// called. A failure after the registration was written restores the previous
// configuration; the caller removes the directories the installation created. A
// configuration write the platform refuses is read back and the previous
// configuration is restored, so a registration the platform mutated before the
// failure is not left half changed. A replaced registration that was running is
// restarted and verified, and a failure of that restart stops the instance the
// installation started, restores the previous configuration and brings the
// previous program back into service.
func (m *scmManager) register(ctx context.Context, manager *mgr.Mgr, opts InstallOptions, previous previousSCMRegistration) error {
	config := buildSCMConfig(opts)
	handle, err := manager.OpenService(ServiceName)
	created := false
	switch {
	case err == nil:
		// The service is registered already: the registration is updated
		// instead of a second one being created.
	case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		handle, err = manager.CreateService(ServiceName, opts.Executable, config, RunArgs(opts)...)
		if err != nil {
			return fmt.Errorf("service: register %s: %w", ServiceName, err)
		}
		created = true
		// A first installation replaces nothing: a failed one removes the
		// registration it created instead of restoring one.
		previous = previousSCMRegistration{}
	default:
		return fmt.Errorf("service: open the registration of %s: %w", ServiceName, err)
	}
	defer handle.Close()

	if !created {
		// The configuration of the registration is read before it is
		// changed, so a failure after the write can put it back as it was.
		previousConfig, err := handle.Config()
		if err != nil {
			return fmt.Errorf("service: read the registration of %s: %w", ServiceName, err)
		}
		if !previous.present {
			// The registration appeared between the read-only lookup and
			// this handle: it is the registration the installation
			// replaces, and its state decides whether it has to be
			// restarted.
			status, err := handle.Query()
			if err != nil {
				return scmRefused("an installation", fmt.Errorf("service: query the state of %s: %w", ServiceName, err))
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
			return err
		}

		// An administrator may have switched the service to a manual, a
		// disabled or a delayed start, and a repeat installation keeps the
		// state it finds.
		config = scmConfigWithAutostart(config, previousConfig)

		if err := m.updateRegistrationConfig(handle, config, previous); err != nil {
			return err
		}
		log.Printf("service: %s is installed already, the registration was updated", ServiceName)
		logEvent(eventLogInfo, fmt.Sprintf("hfdesk: the registration of %s was updated", ServiceName))
	}

	// The failure recovery is part of the registration: a service the machine
	// never restarts after a crash is not the instance that was asked for. A
	// policy that is registered already is kept, so a reinstall never resets
	// what the instance or an administrator decided.
	if err := configureSCMRecovery(handle); err != nil {
		return m.rollbackInstallation(ctx, handle, previous, scmRollbackRegistration,
			fmt.Errorf("service: configure the failure recovery of %s: %w", ServiceName, err))
	}

	if !previous.running {
		// A first installation and an instance that was not running stay
		// stopped: starting is an explicit, separate command.
		return nil
	}

	// The instance ran before the installation, so it has to run the new
	// program afterwards. The graceful stop completes before the new process
	// is started, so the two instances never compete for the bind address.
	if err := m.stopService(ctx, handle); err != nil {
		// The instance could not be stopped. The registration is put back
		// and the instance is left alone: an installation never fights a
		// running instance.
		return m.rollbackInstallation(ctx, handle, previous, scmRollbackRegistration,
			fmt.Errorf("service: stop the running instance of %s: %w", ServiceName, err))
	}
	check, err := m.startAndVerify(ctx, handle, opts.Executable, previous.processID)
	if err != nil {
		return m.rollbackInstallation(ctx, handle, previous, scmRollbackRestart,
			fmt.Errorf("service: the installation did not put the new executable into service: %w", err))
	}
	if check != scmImageVerified {
		// The handle of the running process was refused, so the image of the
		// process could not be read. The instance runs and the registration
		// carries the program; the installation reports exactly that and
		// never claims that the image was compared.
		message := fmt.Sprintf("hfdesk: %s runs the registered program %s, the image of the process was not readable from the shell of the installation", ServiceName, opts.Executable)
		log.Printf("service: %s", message)
		logEvent(eventLogWarning, message)
	}
	return nil
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
)

// rollbackInstallation restores the configuration of the registration a failed
// installation replaced.
//
// The failure is the error the caller gets, and the outcome of the restore is
// reported next to it. The restore is the configuration and not the whole
// registration: the failure recovery is left as the installation configured it.
//
// The running instance is only touched when the installation switched it. The
// restart rollback stops the instance the failed installation started before
// the previous registration is written back, so the registration and the
// process that runs never describe different programs. The restore runs on a
// context that the caller cannot cancel.
func (m *scmManager) rollbackInstallation(ctx context.Context, handle *mgr.Service, previous previousSCMRegistration, mode scmRollbackMode, failure error) error {
	if mode == scmRollbackRestart && previous.present && previous.running {
		// The installation switched the running instance, so the rollback
		// has to switch it back.
		return m.rollbackRestart(ctx, handle, previous, failure)
	}
	if err := restoreSCMRegistration(handle, previous); err != nil {
		return fmt.Errorf("%w (the registration of the failed installation could not be put back: %v)", failure, err)
	}
	if !previous.present {
		// The failed installation created the registration: there was no
		// previous one to restore, and the registration is removed again.
		return fmt.Errorf("%w (the registration of the failed installation was removed)", failure)
	}
	return fmt.Errorf("%w (the configuration of the previous registration was restored, the running instance was not restarted; the failure recovery of the registration is not part of the restore)", failure)
}

// scmRestartRollback is what the restart rollback of a failed installation
// proved about the machine.
//
// The facts are what was observed, not what was attempted, so no sentence
// composed from them claims more than the rollback can prove. They describe the
// configuration and the running instance, not the whole registration: the
// restore leaves the failure recovery in place.
type scmRestartRollback struct {
	// stopped reports whether the instance the failed installation started
	// was proven to be stopped.
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
	// verified reports whether the running process was proven to be the
	// previous program, in a process that is none of the processes of the
	// failed attempt.
	verified bool
}

// scmRestartRollbackSentence returns the sentence that reports the proven
// outcome of a restart rollback next to the failure of the installation.
//
// It is the only place the outcome is described, so a caller reads no sentence
// for a state the rollback did not reach: the claim that the previous instance
// was started again is made for a verified rollback only, and the sentence of a
// completed rollback says that the failure recovery stays in place.
func scmRestartRollbackSentence(facts scmRestartRollback, reason string) string {
	switch {
	case !facts.stopped:
		// The instance of the failed installation was not proven to be
		// stopped. Its registration is kept: it is the registration that
		// describes the program of the process that runs, and restoring the
		// previous one now would leave a registration that names another
		// program than the one that runs.
		return fmt.Sprintf("the registration of the failed installation was kept because the instance it started was not proven to be stopped: %s: the previous registration was not restored", reason)
	case !facts.restored:
		return fmt.Sprintf("the instance of the failed installation was stopped, but the previous registration could not be put back: %s", reason)
	case !facts.programKnown:
		// The restored registration carries a command line that cannot be
		// read, so there is no program to start: the rollback reports the
		// registration and the state of the instance and never claims a
		// start it did not make.
		return fmt.Sprintf("the instance of the failed installation was stopped and the configuration of the previous registration was restored, but the restored registration names no program, so no instance was started: the instance of %s is stopped", ServiceName)
	case !facts.started:
		// The start was refused, or it was accepted and the instance never
		// proved to run the previous program. Neither is a claim about the
		// program that runs: the reason names what was observed.
		return fmt.Sprintf("the instance of the failed installation was stopped and the configuration of the previous registration was restored, but the previous instance was not proven to be in service again: %s", reason)
	case !facts.verified:
		return "the instance of the failed installation was stopped and the configuration of the previous registration was restored and an instance was started, but the running process was not proven to be the previous program: the image of the process could not be read"
	default:
		return "the instance of the failed installation was stopped, the configuration of the previous registration was restored and the previous instance was started again; the failure recovery of the registration was not part of the restore and stays as the installation left it"
	}
}

// rollbackRestart stops the instance of the failed installation, restores the
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
func (m *scmManager) rollbackRestart(ctx context.Context, handle *mgr.Service, previous previousSCMRegistration, failure error) error {
	// The caller may have cancelled the context, the machine still has to be
	// put back the way it was.
	cleanup := context.WithoutCancel(ctx)
	facts := scmRestartRollback{}
	report := func(reason string) error {
		return fmt.Errorf("%w (%s)", failure, scmRestartRollbackSentence(facts, reason))
	}

	// The process of the failed attempt is read before it is stopped: the
	// verification below excludes it, so a process the rollback did not start
	// is never taken for the previous instance.
	failedProcessID := scmRunningProcessID(handle)

	if err := m.stopService(cleanup, handle); err != nil {
		return report(err.Error())
	}
	facts.stopped = true

	if err := restoreSCMRegistration(handle, previous); err != nil {
		return report(err.Error())
	}
	facts.restored = true

	executable := scmConfigExecutable(previous.config)
	if executable == "" {
		return report("")
	}
	facts.programKnown = true

	check, err := m.startAndVerify(cleanup, handle, executable, previous.processID, failedProcessID)
	if err != nil {
		return report(err.Error())
	}
	facts.started = true
	facts.verified = check == scmImageVerified
	return report("")
}

// scmRunningProcessID returns the process of the running instance, zero when the
// service control manager reports none.
//
// The process is read before a rollback stops the instance of the failed
// installation, so the verification of the restart can tell it apart from the
// instance the rollback starts. A state that cannot be read names no process
// either: the stop that follows reports the refusal.
func scmRunningProcessID(handle *mgr.Service) uint32 {
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
func (m *scmManager) updateRegistrationConfig(registration scmConfiguration, config mgr.Config, previous previousSCMRegistration) error {
	err := registration.UpdateConfig(config)
	if err == nil {
		return nil
	}
	failure := fmt.Errorf("service: update the registration of %s: %w", ServiceName, err)
	return failedUpdateRestore(registration, previous, failure)
}

// failedUpdateRestore reads the registration back after a refused update and
// restores the configuration the installation replaced.
//
// The outcome is what was observed, never what was attempted: a restore that
// was read back is reported as restored, and a read that fails, or a restore
// that fails, is reported as a registration that may be partially changed
// together with the reason. A registration that cannot be read back is not
// written back either: the failure path never claims a restore it could not
// prove, so the reason of the read is what the caller gets. The original failure
// stays the error the caller gets, and the running instance is never stopped or
// started here: the refused write did not touch it.
func failedUpdateRestore(registration scmConfiguration, previous previousSCMRegistration, failure error) error {
	if _, err := registration.Config(); err != nil {
		return fmt.Errorf("%w (the registration of %s may be partially changed: its configuration could not be read back: %v)", failure, ServiceName, err)
	}
	if err := restoreSCMConfig(registration, previous); err != nil {
		return fmt.Errorf("%w (the registration of %s may be partially changed: the previous configuration could not be restored: %v)", failure, ServiceName, err)
	}
	return fmt.Errorf("%w (the configuration of the previous registration of %s was restored; the running instance was not touched)", failure, ServiceName)
}

// restoreSCMRegistration puts the configuration of the registration back that a
// failed installation replaced.
//
// A registration of a first installation is removed. A replaced registration is
// written back, which proves the command line and thus the program of the
// instance. The write back leaves the failure recovery as the installation
// configured it.
func restoreSCMRegistration(handle *mgr.Service, previous previousSCMRegistration) error {
	if !previous.present {
		if err := handle.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return fmt.Errorf("service: remove the registration of %s: %w", ServiceName, err)
		}
		return nil
	}
	return restoreSCMConfig(handle, previous)
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
	if err := handle.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return scmImageUnverified, scmRefused("a start", fmt.Errorf("service: start %s: %w", ServiceName, err))
	}
	return m.verifyRunningExecutable(ctx, handle, executable, exclude...)
}

// verifyRunningExecutable waits until the instance runs the given program in a
// process that is not one of the excluded ones.
//
// The service control manager reports the process of the running instance, and
// its image is compared with the program: a registration that carries the
// program proves nothing while a process of the failed attempt still runs. The
// wait is bounded by the verification budget and the context.
func (m *scmManager) verifyRunningExecutable(ctx context.Context, handle *mgr.Service, executable string, exclude ...uint32) (scmImageCheck, error) {
	deadline := time.Now().Add(scmVerifyTimeout)
	detail := "the service control manager did not report the instance as running"
	for {
		status, err := handle.Query()
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
			settled, check, reason := scmReadRunningImage(handle, status.ProcessId, executable)
			if settled {
				return check, nil
			}
			detail = reason
		}
		if !time.Now().Before(deadline) {
			return scmImageUnverified, fmt.Errorf("the instance does not run %q within %s: %s", executable, scmVerifyTimeout, detail)
		}
		select {
		case <-ctx.Done():
			return scmImageUnverified, ctx.Err()
		case <-time.After(scmVerifyPollInterval):
		}
	}
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
	image, err := queryProcessImageName(processID)
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
	commandLine, err := queryServiceCommandLine(handle.Handle)
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
	// scmRecoveryConfigure writes the recovery policy of HFDesk: the
	// registration has none.
	scmRecoveryConfigure scmRecoveryDecision = iota
	// scmRecoveryKeep keeps the recovery policy the registration carries.
	scmRecoveryKeep
)

// scmRecoveryDecisionFor returns what an installation does with the failure
// recovery of a registration.
//
// A registration with at least one action keeps its policy, because a repeat
// installation keeps what the instance or an administrator set. The policy of
// HFDesk is configured only for a registration without one; a read that fails
// is not evidence of no recovery, so the policy is configured then as well.
func scmRecoveryDecisionFor(existing []mgr.RecoveryAction, readErr error) scmRecoveryDecision {
	if readErr != nil || len(existing) == 0 {
		return scmRecoveryConfigure
	}
	return scmRecoveryKeep
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
// A registration that carries a policy keeps it, switch included. A recovery
// that cannot be configured is a failed installation.
func configureSCMRecovery(handle *mgr.Service) error {
	existing, err := handle.RecoveryActions()
	if scmRecoveryDecisionFor(existing, err) == scmRecoveryKeep {
		log.Printf("service: %s keeps the failure recovery of its registration", ServiceName)
		return nil
	}
	if err != nil {
		log.Printf("service: the failure recovery of %s cannot be read (%v), the recovery policy of HFDesk is configured", ServiceName, err)
	} else {
		log.Printf("service: the registration of %s has no failure recovery, the recovery policy of HFDesk is configured", ServiceName)
	}
	if err := handle.SetRecoveryActions(scmRecoveryActions(), scmRecoveryResetPeriod); err != nil {
		return err
	}
	// The switch takes effect at the next start of the machine, which is
	// documented by the platform: an installation never reports more than
	// the policy it wrote.
	return handle.SetRecoveryActionsOnNonCrashFailures(true)
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

	return m.stopService(ctx, handle)
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

	if err := m.stopService(ctx, handle); err != nil {
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
// is over and the instance is still running.
func (m *scmManager) stopService(ctx context.Context, handle scmServiceControl) error {
	deadline := time.Now().Add(scmStopTimeout)
	delivered := false
	for {
		status, err := handle.Query()
		if err != nil {
			return scmRefused("a stop", fmt.Errorf("service: query the state of %s: %w", ServiceName, err))
		}
		if status.State == svc.Stopped {
			return nil
		}
		if !delivered {
			if _, err := handle.Control(svc.Stop); err != nil {
				switch {
				case errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE):
					// The instance is not running: the wanted state
					// already holds.
					return nil
				case errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL):
					// The instance is still starting and the
					// supervisor refuses the control. The
					// control is delivered again inside the
					// wait.
					log.Printf("service: %s is still starting (the state of the instance is %v), the stop request is delivered again", ServiceName, status.State)
				default:
					return scmRefused("a stop", fmt.Errorf("service: stop %s: %w", ServiceName, err))
				}
			} else {
				// The supervisor accepted the control: the instance
				// stops.
				delivered = true
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service: %s did not stop within %s (the state of the instance was %v)", ServiceName, scmStopTimeout, status.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(scmStopPollInterval):
		}
	}
}

// preflight rejects an installation the service control manager could not
// run: an unusable executable, an executable that is not a Windows program of
// the architecture of this process, an invalid bind address or a directory the
// account of the instance cannot reach.
//
// The preflight runs before the registration is written, so a rejected
// installation never leaves a service behind that fails later with an
// opaque access error.
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
	if err := preflightExecutableImage(opts.Executable); err != nil {
		return err
	}
	if _, _, err := ParseListen(opts.Listen); err != nil {
		return err
	}
	return preflightDataPaths(opts)
}

// preflightExecutableImage rejects an executable the service control manager
// of this machine could not run.
//
// The platform starts the registered image itself, so the header of the file is
// the only place the installation can learn what it is about to register: a
// truncated download, a file that is not a program at all and a binary of
// another architecture are refused here, before the registration is written
// and before a running instance is stopped.
//
// An architecture this installation does not compare is still required to name
// a PE image: only the architectures HFDesk ships have a comparison.
func preflightExecutableImage(executable string) error {
	return preflightExecutableImageFor(executable, runtime.GOARCH)
}

// preflightExecutableImageFor is preflightExecutableImage for the named
// architecture of the process.
//
// The architecture is a parameter so the comparison can be checked without a
// machine of every architecture of the platform.
func preflightExecutableImageFor(executable, arch string) error {
	machine, err := pe.Machine(executable)
	if err != nil {
		return fmt.Errorf("service: executable %q is not a Windows executable: %w", executable, err)
	}
	want, known := scmProcessMachine(arch)
	if !known {
		return nil
	}
	if machine != want {
		return fmt.Errorf("service: executable %q is built for another architecture: the image is machine 0x%04X and this process is %s (0x%04X)", executable, machine, arch, want)
	}
	return nil
}

// scmProcessMachine returns the machine type of an image that runs on the given
// architecture of the process.
//
// The machine types are the ones HFDesk ships a program for. An architecture
// without a value is not compared by the installation: the program is then
// only required to be a PE image.
func scmProcessMachine(arch string) (machine uint16, known bool) {
	switch arch {
	case "amd64":
		return pe.MachineAMD64, true
	case "arm64":
		return pe.MachineARM64, true
	default:
		return 0, false
	}
}

// The service identity of the instance is the account the registration runs
// the program as: LocalService (S-1-5-19). The access control list of a
// directory the installation reuses has to grant that account the access
// the instance needs, otherwise the instance fails later with an opaque
// access error.
//
// The membership of the account is the one the documentation of the
// platform grants to it, and it is the only source of it. The built-in
// service accounts of the platform are local authenticated users, so the
// token of a service logon of the account carries the account itself,
// everybody and the authenticated users.
//
// The Interactive Users group (S-1-5-4) is deliberately not part of it: it
// is the group of an interactive logon, and the instance never runs an
// interactive logon. A descriptor that grants the access to the interactive
// users only is a descriptor the instance can never use, and the
// installation has to refuse it.
//
// The membership is never read from a token of the account: the local
// security authority of a machine does not accept the logon of the account
// on every machine, and a check of a path has to answer the same way on
// every machine. The message of a refusal says that the check was made
// against the documented membership and not against the groups of a token,
// so the operator knows what the answer is based on.
const (
	// scmServiceIdentitySID is the SID of the account the instance runs
	// as: LocalService (S-1-5-19).
	scmServiceIdentitySID = "S-1-5-19"

	// scmServiceIdentityName is the name of the account the instance runs
	// as. The name is part of the message of a failed installation, so
	// the user knows which account needs the access.
	scmServiceIdentityName = `NT AUTHORITY\LocalService`

	// scmEveryoneSID is the SID of the Everyone group. Every token of
	// the platform carries it, so an access control list that grants the
	// access to everybody grants it to the account of the instance as
	// well.
	scmEveryoneSID = "S-1-1-0"

	// scmAuthenticatedUsersSID is the SID of the Authenticated Users
	// group. The documentation of the platform treats the built-in
	// service accounts as local authenticated users, so the token of a
	// service logon of the account carries this group as well.
	scmAuthenticatedUsersSID = "S-1-5-11"

	// scmInteractiveUsersSID is the SID of the Interactive Users group.
	// It is the group of an interactive logon: the instance runs a
	// service logon and is never an interactive user, so the group never
	// covers the account of the instance.
	scmInteractiveUsersSID = "S-1-5-4"
)

// scmIdentitySIDs is the set of security identifiers that cover the account
// the instance runs as.
//
// The set is the documented membership of the account: the account itself,
// everybody and the authenticated users. The Interactive Users group is
// deliberately not part of it.
//
// An access control list that grants the access to one of them grants it to
// the account of the instance as well.
type scmIdentitySIDs struct {
	// sids holds the SIDs of the set in their canonical form.
	sids map[string]bool
}

// covers reports whether a SID covers the account of the instance.
func (i scmIdentitySIDs) covers(sid string) bool {
	return i.sids[scmNormalizeSID(sid)]
}

// note returns the note that tells the caller that the check was made
// against the documented membership of the account and not against the
// groups of a token of it.
//
// The note is never empty: the membership of the account is the documented
// one, so the user has to know that the groups of the account were not read
// from a token of it.
func (i scmIdentitySIDs) note() string {
	return fmt.Sprintf(
		": the check was made against the membership the documentation of the platform grants to the account %s: the account itself, everybody and the authenticated users, and not against the groups of a token of the account",
		scmServiceIdentityName,
	)
}

// scmNormalizeSID returns the canonical form of a SID.
//
// The trustees of an access control entry and the SIDs of the membership of
// the account are both written in the canonical form, but a descriptor that
// was written by hand can carry another case and spaces around the SID.
func scmNormalizeSID(sid string) string {
	return strings.ToUpper(strings.TrimSpace(sid))
}

// scmDocumentedIdentitySIDs returns the membership the documentation of the
// platform grants to the account of the instance.
//
// The documentation of Microsoft treats the built-in service accounts as
// local authenticated users: LocalService and NetworkService belong to the
// same class as the Authenticated Users group, and the default security
// descriptors of the system grant that class its rights. The token of such a
// logon therefore carries the account itself, everybody and the authenticated
// users.
//
// The Interactive Users group is deliberately not part of the set: it is the
// group of an interactive logon, and the account of the instance runs a
// service logon.
//
// The documentation the membership is taken from:
//
//   - "Well-known security identifiers in Windows" (Microsoft Learn):
//     S-1-5-19 is LocalService, S-1-5-11 is Authenticated Users and
//     S-1-5-4 is Interactive Users.
//   - "Service Account Security and Access Rights" (Microsoft Learn):
//     LocalService and NetworkService are members of the Authenticated
//     Users group.
//
// The set is the only source of the membership: the local security authority
// of a machine does not accept the logon of the account on every machine,
// and a check of a path has to answer the same way on every machine. The
// note of the set tells the operator that the check was made against the
// documentation of the platform and not against the groups of a token of
// the account.
func scmDocumentedIdentitySIDs() scmIdentitySIDs {
	return scmIdentitySIDs{
		sids: map[string]bool{
			scmNormalizeSID(scmServiceIdentitySID):    true,
			scmNormalizeSID(scmEveryoneSID):           true,
			scmNormalizeSID(scmAuthenticatedUsersSID): true,
		},
	}
}

// scmServiceIdentity is the membership of the account the instance runs as.
//
// The membership is read once per process: it is a property of the account
// and not of the path that is checked.
var scmServiceIdentity = sync.OnceValue(scmDocumentedIdentitySIDs)

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
func preflightDataPaths(opts InstallOptions) error {
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
		if err := preflightDataPath(dir.name, dir.path); err != nil {
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
func preflightDataPath(name, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("service: %s %q must be an absolute path", name, path)
	}

	info, err := os.Stat(path)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("service: %s %q is not a directory: the instance needs a directory there", name, path)
		}
		return checkServiceAccess(name, path)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("service: %s %q is not usable: %w", name, path, err)
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
	return checkServiceCreateAccess(name, path)
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
// The directories above the ancestor are not judged: the account of the
// instance traverses them with the privilege that lets a service account
// bypass the traverse check, so a directory the account reaches is never
// refused here for a traverse right it does not need. The check of the
// manager is a preflight of the permissions of a path and not a simulation
// of the access check of the operating system.
func checkServiceCreateAccess(label, path string) error {
	ancestor, err := scmNearestExistingDir(path)
	if err != nil {
		return fmt.Errorf("service: the %s %q does not exist and the nearest existing ancestor of it cannot be read: %w: the installation cannot tell whether the account %s can create the directory", label, path, err, scmServiceIdentityName)
	}
	sddl, err := scmReadServiceAccess(ancestor)
	if err != nil {
		return fmt.Errorf("service: the %s %q does not exist and the access rights of its nearest existing ancestor %q cannot be read: %w: the installation cannot tell whether the account %s can create the directory", label, path, ancestor, err, scmServiceIdentityName)
	}
	if err := sddlGrantsIdentity(sddl, scmServiceIdentity()); err != nil {
		// The message names the directory, the account and the access
		// that is missing: the user has to grant the access to the
		// ancestor, and the name of the ancestor is what makes the
		// guidance actionable.
		return fmt.Errorf(
			"service: the %s %q does not exist and the account %s cannot create it: %w: the nearest existing ancestor %q has to grant the account read and write access: the installation never creates a directory the user named, the instance creates it when it runs, as the account the instance runs as: grant the account read and write access to the ancestor directory, or install the instance without this path",
			label, path, scmServiceIdentityName, err, ancestor,
		)
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
				return "", fmt.Errorf("%q is not a directory", current)
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

// checkServiceAccess reads the access control list of a path and checks
// that the account of the instance can work with it.
//
// The access control list is only read: the descriptor of the instance is
// never applied to a path the installation reuses.
func checkServiceAccess(label, path string) error {
	sddl, err := scmReadServiceAccess(path)
	if err != nil {
		return fmt.Errorf("service: the access rights of the %s %q cannot be read: %w: the installation cannot tell whether the account %s can use the path", label, path, err, scmServiceIdentityName)
	}
	if err := sddlGrantsIdentity(sddl, scmServiceIdentity()); err != nil {
		return fmt.Errorf("service: the %s %q is not usable by the account the instance runs as (%s): %w: grant the account read and write access to the path, or install the instance without this path", label, path, scmServiceIdentityName, err)
	}
	return nil
}

// scmReadServiceAccess reads the access control list of a path and returns
// it in its SDDL form.
//
// The list that decides the access is the one of the real location of the
// path: the platform follows the links of a path before it uses it, so a
// directory that is reached through a link is used by the rights of its
// target and not by the rights of the link itself. A path whose real
// location cannot be read is judged by the path as it was written.
func scmReadServiceAccess(path string) (string, error) {
	resolved, err := scmResolvePath(path)
	if err != nil {
		resolved = path
	}
	// The owner and the group are read next to the access control list:
	// the conversion of a descriptor into its SDDL form needs the whole
	// descriptor, and reading them needs no right beyond the one the
	// access control list needs.
	sd, err := windows.GetNamedSecurityInfo(
		resolved,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION,
	)
	if err != nil {
		return "", err
	}
	sddl := sd.String()
	if sddl == "" {
		return "", errors.New("the access control list is empty")
	}
	return sddl, nil
}

// sddlGrantsIdentity reports whether the access control list of an SDDL
// string grants the account of the instance the access it needs.
//
// The check is a permission preflight, not a simulation of the access check
// of the operating system: it reads the access control list of the path
// itself, it does not resolve the inheritance of a parent directory or the
// privileges of a token.
//
// The trustees that cover the account are the ones of the documented
// membership of the account. The Interactive Users group is never one of
// them: the instance runs a service logon and is never an interactive user,
// so a descriptor that grants the access to the interactive users only is
// refused.
//
// A denied entry wins over an allowed one, but only when it denies a right
// the instance needs: a directory that denies the change of its access
// control list, or the change of its owner, is a directory the instance can
// still read and write, and it is not refused because of it.
//
// An inherit only entry applies to the children of the object and never to
// the object itself: it grants the account nothing on the directory the
// check is made for, and it denies it nothing either.
func sddlGrantsIdentity(sddl string, identity scmIdentitySIDs) error {
	var (
		allowed bool
		read    bool
		write   bool
	)
	for _, ace := range parseSDDLAces(sddl) {
		if ace.inheritOnly() {
			// The entry is about the children of the directory, not
			// about the directory itself.
			continue
		}
		if !ace.coversIdentity(identity) {
			continue
		}
		switch ace.kind {
		case "D", "OD":
			// The entry takes the access away only when it denies a
			// right the instance needs.
			denied := scmMaskRights(ace.rights) & scmNeededRights
			if denied == 0 {
				continue
			}
			return fmt.Errorf("the access control list denies the account %s the %s%s", scmServiceIdentityName, scmRightName(denied), identity.note())
		case "A", "OA":
			allowed = true
			rights := scmMaskRights(ace.rights)
			read = read || rights&scmRightRead != 0
			write = write || rights&scmRightWrite != 0
		}
	}
	switch {
	case !allowed:
		return fmt.Errorf("the access control list grants the account %s no access%s", scmServiceIdentityName, identity.note())
	case !read:
		return fmt.Errorf("the access control list grants the account %s no read access%s", scmServiceIdentityName, identity.note())
	case !write:
		return fmt.Errorf("the access control list grants the account %s no write access%s", scmServiceIdentityName, identity.note())
	}
	return nil
}

// sddlACE is one access control entry of an SDDL string.
type sddlACE struct {
	// kind is the type of the entry: "A" for an allowed entry and "D"
	// for a denied one.
	kind string
	// flags holds the inheritance flags of the entry.
	flags string
	// rights is the access mask the entry grants or denies.
	rights uint32
	// trustee is the SID the entry applies to.
	trustee string
}

// inheritOnly reports whether the entry applies to the children of the
// object and never to the object itself.
//
// The inherit only flag ("IO") is one of the two letter codes of the flags
// field of an entry. The field is a concatenation of such codes, so the
// codes are read one after the other: a search for the text of the code
// would read the letters of two neighbouring codes as the inherit only
// flag.
func (ace sddlACE) inheritOnly() bool {
	flags := strings.ToUpper(ace.flags)
	for i := 0; i+2 <= len(flags); i += 2 {
		if flags[i:i+2] == "IO" {
			return true
		}
	}
	return false
}

// coversIdentity reports whether the entry applies to the account of the
// instance or to one of the groups it belongs to.
//
// The trustees of an SDDL string are raw SIDs. The aliases of the platform
// are accepted as well, so a descriptor that was written by hand is read
// the same way.
//
// The membership of the account decides: an alias is only another way to
// write a SID, and the SID is one of the ones of the effective membership of
// the account or it is not one of them.
func (ace sddlACE) coversIdentity(identity scmIdentitySIDs) bool {
	trustee := strings.ToUpper(strings.TrimSpace(ace.trustee))
	if sid, ok := sddlTrusteeAliases[trustee]; ok {
		trustee = sid
	}
	return identity.covers(trustee)
}

// sddlTrusteeAliases maps the aliases of a trustee of an SDDL access
// control entry onto the SID they stand for.
//
// The map resolves the aliases of the platform, it does not grant any
// access: a resolved trustee covers the account of the instance only when
// the effective membership of the account carries it. The alias of the
// Interactive Users (IU) therefore never covers the account of the
// instance, which runs a service logon.
var sddlTrusteeAliases = map[string]string{
	"WD": "S-1-1-0",      // Everyone
	"BU": "S-1-5-32-545", // Users
	"AU": "S-1-5-11",     // Authenticated Users
	"IU": "S-1-5-4",      // Interactive Users
	"LS": "S-1-5-19",     // LocalService
}

// The rights the instance needs on a directory it works with.
//
// The instance reads the directory, it creates entries in it, and it reads
// and writes the files in it: it needs the read access and the write access
// of the directory and of its files.
//
// A right the instance does not need is not one of them. The right to
// change the access control list of the directory, the right to change its
// owner and the rights of the configuration of the platform are none of
// them: a denied entry that carries one of them takes nothing the instance
// needs away.
const (
	// scmRightRead is the right to read the directory and the files in
	// it.
	scmRightRead uint32 = 1 << iota
	// scmRightWrite is the right to create entries in the directory and
	// to write the files in it.
	scmRightWrite
)

// scmNeededRights is the mask of the rights the instance needs.
//
// The mask is the intersection a denied entry is measured against: an entry
// that denies one of these rights takes a right the instance needs away.
const scmNeededRights = scmRightRead | scmRightWrite

// The individual rights of a file and of a directory the instance needs.
//
// The instance reads a directory, it creates entries in it and it reads and
// writes the files in it. The rights are the ones of a directory, and the
// rights of a file carry the same values: the right to list the entries of
// the directory is FILE_READ_DATA, the right to walk through it is
// FILE_EXECUTE, the right to create a file in it is FILE_WRITE_DATA and the
// right to create a directory in it is FILE_APPEND_DATA.
//
// The right to delete an entry of the directory is the one the platform does
// not export, so it is written down here.
const (
	// scmFileDeleteChild is the right to delete an entry of a directory
	// (FILE_DELETE_CHILD). The platform does not export the constant.
	scmFileDeleteChild uint32 = 0x00000040

	// scmFileReadRights is the mask of the individual rights the instance
	// needs to read a directory and the files in it: the right to list
	// the entries of the directory (FILE_LIST_DIRECTORY, the same value
	// as FILE_READ_DATA), the right to read the extended attributes of
	// the entries, the right to walk through the directory
	// (FILE_TRAVERSE, the same value as FILE_EXECUTE) and the right to
	// read their attributes.
	scmFileReadRights uint32 = windows.FILE_READ_DATA |
		windows.FILE_READ_EA |
		windows.FILE_EXECUTE |
		windows.FILE_READ_ATTRIBUTES

	// scmFileWriteRights is the mask of the individual rights the
	// instance needs to create entries in a directory and to write the
	// files in it: the right to create a file in it (FILE_ADD_FILE, the
	// same value as FILE_WRITE_DATA), the right to create a directory in
	// it (FILE_ADD_SUBDIRECTORY, the same value as FILE_APPEND_DATA),
	// the right to write the extended attributes of the entries, the
	// right to delete an entry of it and the right to write their
	// attributes.
	scmFileWriteRights uint32 = windows.FILE_WRITE_DATA |
		windows.FILE_APPEND_DATA |
		windows.FILE_WRITE_EA |
		scmFileDeleteChild |
		windows.FILE_WRITE_ATTRIBUTES
)

// scmMaskRights returns the rights of the check that an access mask
// carries.
//
// The mask is read by its individual bits: the rights of the check are the
// rights of a directory and of a file, and an entry that carries one of
// them carries the right it stands for. An entry that carries a single one
// of them denies that one, so a denial of the right to create a file in a
// directory is a denial of the write access the instance needs.
//
// The generic rights are the same rights in another form: the manager of the
// platform expands them into the file rights of the object before it checks
// the access, so they carry the rights they stand for. The full control
// carries every right of the check. The right to execute an object is none
// of the rights of the check.
//
// A right the instance does not need is not one of them: the right to delete
// the object, the right to read its descriptor, the right to change its
// access control list, the right to change its owner and the right to
// synchronize with it are none of them, so an entry that carries one of them
// takes nothing the instance needs away.
//
// The mapping is the same for an allowed and for a denied entry: a right
// that an entry does not carry is not a right it grants and not a right it
// denies.
func scmMaskRights(mask uint32) uint32 {
	if mask&windows.GENERIC_ALL != 0 {
		return scmRightRead | scmRightWrite
	}
	var rights uint32
	if mask&windows.GENERIC_READ != 0 {
		rights |= scmRightRead
	}
	if mask&windows.GENERIC_WRITE != 0 {
		rights |= scmRightWrite
	}
	if mask&scmFileReadRights != 0 {
		rights |= scmRightRead
	}
	if mask&scmFileWriteRights != 0 {
		rights |= scmRightWrite
	}
	return rights
}

// scmRightName names the rights of the check a mask carries.
//
// The name is the one the messages of the manager use: the read access and
// the write access are the access the instance needs on a path.
func scmRightName(rights uint32) string {
	switch {
	case rights&scmRightRead != 0 && rights&scmRightWrite != 0:
		return "read and write access"
	case rights&scmRightRead != 0:
		return "read access"
	default:
		return "write access"
	}
}

// parseSDDLAces returns the access control entries of an SDDL string.
//
// The entries are the parenthesized groups of the string. The parts around
// them (the header of the descriptor, the owner and the group) carry no
// entry.
func parseSDDLAces(sddl string) []sddlACE {
	var aces []sddlACE
	for i := 0; i < len(sddl); {
		open := strings.IndexByte(sddl[i:], '(')
		if open < 0 {
			break
		}
		i += open + 1
		end := strings.IndexByte(sddl[i:], ')')
		if end < 0 {
			break
		}
		body := sddl[i : i+end]
		i += end + 1
		if ace, ok := parseSDDLACE(body); ok {
			aces = append(aces, ace)
		}
	}
	return aces
}

// parseSDDLACE reads one access control entry of an SDDL string.
//
// The entry has the shape (type;flags;rights;object-type;inheritance;trustee).
// The trustee is the last field of the entry, the rights the third one and
// the flags the second one.
func parseSDDLACE(body string) (sddlACE, bool) {
	fields := strings.Split(body, ";")
	if len(fields) < 2 {
		return sddlACE{}, false
	}
	ace := sddlACE{
		kind:    strings.ToUpper(strings.TrimSpace(fields[0])),
		flags:   strings.ToUpper(strings.TrimSpace(fields[1])),
		trustee: strings.TrimSpace(fields[len(fields)-1]),
	}
	if len(fields) >= 3 {
		ace.rights = parseSDDLRights(fields[2])
	}
	return ace, true
}

// parseSDDLRights reads the rights field of an access control entry.
//
// The field holds an access mask as a hexadecimal number or as one of the
// aliases of the platform. A field that cannot be read grants nothing.
func parseSDDLRights(field string) uint32 {
	field = strings.TrimSpace(field)
	if field == "" {
		return 0
	}
	if mask, ok := sddlRights[strings.ToUpper(field)]; ok {
		return mask
	}
	text := strings.TrimPrefix(strings.TrimPrefix(field, "0x"), "0X")
	mask, err := strconv.ParseUint(text, 16, 32)
	if err != nil {
		return 0
	}
	return uint32(mask)
}

// scmFileAllAccess is the access mask of the alias "FA" of the rights field
// of an SDDL entry: the standard rights of a file plus every right on the
// data of the file. The platform does not export the constant.
const scmFileAllAccess uint32 = 0x001F01FF

// sddlRights maps the aliases of the rights field of an SDDL access control
// entry onto the access mask they stand for.
//
// The aliases are the ones the platform reads and writes: the generic
// rights (GA, GR, GW, GX), the standard rights (RC, SD, WD, WO) and the
// file rights (FA, FR, FW, FX).
var sddlRights = map[string]uint32{
	"GA": windows.GENERIC_ALL,
	"GR": windows.GENERIC_READ,
	"GW": windows.GENERIC_WRITE,
	"GX": windows.GENERIC_EXECUTE,
	"RC": windows.READ_CONTROL,
	"SD": windows.DELETE,
	"WD": windows.WRITE_DAC,
	"WO": windows.WRITE_OWNER,
	"FA": scmFileAllAccess,
	"FR": windows.FILE_GENERIC_READ,
	"FW": windows.FILE_GENERIC_WRITE,
	"FX": windows.FILE_GENERIC_EXECUTE,
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

// scmConfigWithAutostart returns the configuration an update of a registration
// writes: the configuration of the installation with the autostart state the
// registration already has.
//
// A repeat installation keeps the autostart state it finds. An administrator
// may have switched the service to a manual or a disabled start, so it does not
// start at boot, or to a delayed automatic start; a reinstall with a new binary
// is not the place to undo that decision. The two fields are the whole autostart
// state of a registration: the update of the configuration writes both of them,
// so they are kept together.
func scmConfigWithAutostart(config, previous mgr.Config) mgr.Config {
	config.StartType = previous.StartType
	config.DelayedAutoStart = previous.DelayedAutoStart
	return config
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
func restrictInstanceState(opts InstallOptions, snapshot scmInstallSnapshot) error {
	paths := instanceStatePaths(opts, snapshot)
	// The paths that existed before the installation are checked first:
	// the account of the instance has to be able to work with them
	// before a single descriptor is changed.
	for _, path := range paths {
		if path.created {
			continue
		}
		if err := checkServiceAccess(path.name, path.path); err != nil {
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
