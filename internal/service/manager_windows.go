// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
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

	// scmLocalServiceAccountSID is the well-known security identifier of
	// that account: the service control manager accepts it wherever it
	// accepts the account name, so a registration carrying it runs as the
	// supported identity too.
	scmLocalServiceAccountSID = "S-1-5-19"

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
