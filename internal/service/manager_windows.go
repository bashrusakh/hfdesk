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
	"strconv"
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
// instance, restricts the access to them, checks the directories the
// installation reuses, registers the service and registers the event log
// source of the instance.
//
// The state of the instance is restricted before the registration is
// written: every directory the instance owns carries the restrictive
// descriptor, so the directories the instance creates inside them inherit
// it and the account of the instance can work with them.
//
// The reused directories are checked before the registration is written as
// well: a directory the account of the instance cannot reach is a failed
// installation and not a service that fails later with an opaque access
// error.
//
// A service that is registered already is not registered a second time:
// its registration is updated instead, so the instance keeps its identity.
//
// An installation never starts the instance: starting is an explicit,
// separate command.
//
// Every failure of the installation happens before the registration is
// written, so a failed installation never leaves a half-configured service
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
	// The state of the instance is restricted before the reused
	// directories are checked: the directories the instance creates
	// inside its own directories inherit the restrictive descriptor, so
	// the account of the instance can work with them.
	if err := restrictInstanceState(opts); err != nil {
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
	applyRegistration(&status, registration)

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
// run: an unusable executable, an invalid bind address or a directory the
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
	if _, _, err := ParseListen(opts.Listen); err != nil {
		return err
	}
	return preflightDataPaths(opts)
}

// The service identity of the instance is the account the registration runs
// the program as: LocalService (S-1-5-19). The access control list of a
// directory the installation reuses has to grant that account the access
// the instance needs, otherwise the instance fails later with an opaque
// access error.
const (
	// scmServiceIdentitySID is the SID of the account the instance runs
	// as: LocalService (S-1-5-19).
	scmServiceIdentitySID = "S-1-5-19"

	// scmServiceIdentityName is the name of the account the instance runs
	// as. The name is part of the message of a failed installation, so
	// the user knows which account needs the access.
	scmServiceIdentityName = `NT AUTHORITY\LocalService`
)

// scmIdentityGroups are the groups the account of the instance belongs to.
//
// An access control list that grants access to one of them grants the
// access to the account of the instance as well.
var scmIdentityGroups = []string{
	"S-1-1-0",      // Everyone
	"S-1-5-32-545", // Users
	"S-1-5-11",     // Authenticated Users
	"S-1-5-4",      // Interactive Users
}

// preflightDataPaths checks the directories an installation reuses: the
// cache root, the hub directory and the local output directory.
//
// The directories do not belong to the instance: they can hold the models
// and the cache of an interactive run of the user. Their access control
// list is only read, never changed, and never changed recursively.
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
		if err := preflightDataPath(dir.name, dir.path); err != nil {
			return err
		}
	}
	return nil
}

// preflightDataPath checks that the account of the instance can work with
// the directory the installation reuses.
//
// A directory that does not exist yet is created by the installation, which
// runs elevated: the nearest existing ancestor has to grant the account the
// access to create it, and the created directory is checked again.
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

	// The directory does not exist yet. The account of the instance has to
	// be able to create it in the nearest existing ancestor.
	ancestor, err := nearestExistingDir(path)
	if err != nil {
		return fmt.Errorf("service: %s %q cannot be created: %w", name, path, err)
	}
	if err := checkServiceAccess("parent directory", ancestor); err != nil {
		return fmt.Errorf("service: %s %q cannot be created: %w", name, path, err)
	}
	if err := os.MkdirAll(path, dirMode); err != nil {
		return fmt.Errorf("service: create %s %q: %w", name, path, err)
	}
	return checkServiceAccess(name, path)
}

// nearestExistingDir returns the closest ancestor of a path that exists.
//
// The ancestor is the directory the installation has to create the missing
// path in.
func nearestExistingDir(path string) (string, error) {
	current := filepath.Clean(path)
	for {
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing directory was found above %q", path)
		}
		info, err := os.Stat(parent)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%q is not a directory", parent)
			}
			return parent, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("read %q: %w", parent, err)
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
	// The owner and the group are read next to the access control list:
	// the conversion of a descriptor into its SDDL form needs the whole
	// descriptor, and reading them needs no right beyond the one the
	// access control list needs.
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("service: the access rights of the %s %q cannot be read: %w: the installation cannot tell whether the account %s can use the path", label, path, err, scmServiceIdentityName)
	}
	sddl := sd.String()
	if sddl == "" {
		return fmt.Errorf("service: the access rights of the %s %q cannot be read: the installation cannot tell whether the account %s can use the path", label, path, scmServiceIdentityName)
	}
	if err := sddlGrantsIdentity(sddl); err != nil {
		return fmt.Errorf("service: the %s %q is not usable by the account the instance runs as (%s): %w: grant the account read and write access to the path, or install the instance without this path", label, path, scmServiceIdentityName, err)
	}
	return nil
}

// sddlGrantsIdentity reports whether the access control list of an SDDL
// string grants the account of the instance the access it needs.
//
// The check is a permission preflight, not a simulation of the access check
// of the operating system: it reads the access control list of the path
// itself, it does not resolve the inheritance of a parent directory, the
// privileges of a token or the membership of a group that is not listed
// here.
//
// A denied entry wins over an allowed one, so a directory that grants the
// account the access and denies it as well is refused.
func sddlGrantsIdentity(sddl string) error {
	var (
		allowed bool
		read    bool
		write   bool
	)
	for _, ace := range parseSDDLAces(sddl) {
		if !ace.coversIdentity() {
			continue
		}
		switch ace.kind {
		case "D", "OD":
			return fmt.Errorf("the access control list denies the account %s", scmServiceIdentityName)
		case "A", "OA":
			allowed = true
			read = read || maskGrantsRead(ace.rights)
			write = write || maskGrantsWrite(ace.rights)
		}
	}
	switch {
	case !allowed:
		return fmt.Errorf("the access control list grants the account %s no access", scmServiceIdentityName)
	case !read:
		return fmt.Errorf("the access control list grants the account %s no read access", scmServiceIdentityName)
	case !write:
		return fmt.Errorf("the access control list grants the account %s no write access", scmServiceIdentityName)
	}
	return nil
}

// sddlACE is one access control entry of an SDDL string.
type sddlACE struct {
	// kind is the type of the entry: "A" for an allowed entry and "D"
	// for a denied one.
	kind string
	// rights is the access mask the entry grants or denies.
	rights uint32
	// trustee is the SID the entry applies to.
	trustee string
}

// coversIdentity reports whether the entry applies to the account of the
// instance or to one of the groups it belongs to.
//
// The trustees of an SDDL string are raw SIDs. The aliases of the platform
// are accepted as well, so a descriptor that was written by hand is read
// the same way.
func (ace sddlACE) coversIdentity() bool {
	trustee := strings.ToUpper(strings.TrimSpace(ace.trustee))
	if sid, ok := sddlTrusteeAliases[trustee]; ok {
		trustee = sid
	}
	if trustee == scmServiceIdentitySID {
		return true
	}
	for _, group := range scmIdentityGroups {
		if trustee == group {
			return true
		}
	}
	return false
}

// sddlTrusteeAliases maps the aliases of a trustee of an SDDL access
// control entry onto the SID they stand for.
var sddlTrusteeAliases = map[string]string{
	"WD": "S-1-1-0",      // Everyone
	"BU": "S-1-5-32-545", // Users
	"AU": "S-1-5-11",     // Authenticated Users
	"IU": "S-1-5-4",      // Interactive Users
	"LS": "S-1-5-19",     // LocalService
}

// maskGrantsRead reports whether an access mask grants the read access the
// instance needs.
//
// The generic rights and the file specific rights are two different bit
// sets: an entry that grants one of them grants the read access.
func maskGrantsRead(mask uint32) bool {
	if mask&windows.GENERIC_ALL != 0 || mask&windows.GENERIC_READ != 0 {
		return true
	}
	return mask&windows.FILE_GENERIC_READ == windows.FILE_GENERIC_READ
}

// maskGrantsWrite reports whether an access mask grants the write access
// the instance needs.
//
// The generic rights and the file specific rights are two different bit
// sets: an entry that grants one of them grants the write access.
func maskGrantsWrite(mask uint32) bool {
	if mask&windows.GENERIC_ALL != 0 || mask&windows.GENERIC_WRITE != 0 {
		return true
	}
	return mask&windows.FILE_GENERIC_WRITE == windows.FILE_GENERIC_WRITE
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
// The trustee is the last field of the entry and the rights the third one.
func parseSDDLACE(body string) (sddlACE, bool) {
	fields := strings.Split(body, ";")
	if len(fields) < 2 {
		return sddlACE{}, false
	}
	ace := sddlACE{
		kind:    strings.ToUpper(strings.TrimSpace(fields[0])),
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
// Every directory the instance owns carries the descriptor: the directory
// of the config file, the state directory and the log directory, and so
// does the config file itself. The descriptor is applied to the path
// itself and never recursively.
//
// The model and the cache trees of an installation are never re-owned and
// never re-ACLed: they can hold gigabytes of data that the user may reuse
// from an interactive run, and a recursive change of the access rights
// would take the data away from the user who owns it.
func restrictInstanceState(opts InstallOptions) error {
	for _, path := range instanceStatePaths(opts) {
		if err := applyRestrictiveACL(path); err != nil {
			return err
		}
	}
	return nil
}

// instanceStatePaths returns the paths of the instance that carry the
// restrictive descriptor: the directory of the config file, the state
// directory, the log directory and the config file itself.
//
// The config file is the only path that may not exist yet: a config file
// that is written later inherits the descriptor of its directory. A path
// that is named twice is returned once: the config file can live in the
// state directory of the instance.
func instanceStatePaths(opts InstallOptions) []string {
	paths := []string{
		filepath.Dir(opts.Paths.ConfigFile),
		opts.Paths.StateDir,
		opts.Paths.LogDir,
	}
	if info, err := os.Stat(opts.Paths.ConfigFile); err == nil && info.Mode().IsRegular() {
		paths = append(paths, opts.Paths.ConfigFile)
	}

	unique := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		unique = append(unique, path)
	}
	return unique
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
