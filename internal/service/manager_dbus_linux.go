// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

// systemdLoadedConfig is the effective command and the launch properties
// systemd reports for a unit. Dynamic process state is intentionally omitted:
// it is verified separately from the loaded registration.
type systemdLoadedConfig struct {
	loaded                   bool
	execPath                 string
	execArgs                 []string
	execFlags                []string
	ignoreErrors             bool
	workingDirectory         string
	user                     string
	group                    string
	supplementaryGroups      []string
	supplementaryGroupsKnown bool
	environment              []string
	environmentFiles         []systemdEnvironmentFile
	fragmentPath             string
	fragmentPathKnown        bool
	dropInPaths              []string
	dropInPathsKnown         bool
}

type systemdEnvironmentFile struct {
	Path         string
	IgnoreErrors bool
}

// registrationRunning reports whether the supervisor runs the registered unit.
//
// A unit the supervisor does not know does not run either: the unit file is
// there, but a daemon-reload is what makes the supervisor pick it up. Any other
// failure is reported, because an installation that cannot tell whether the
// instance runs cannot promise to bring it back.
func (m *systemdManager) registrationRunning(ctx context.Context) (bool, error) {
	out, err := m.runSystemctl(ctx, "show", unitName,
		"--property=LoadState", "--property=ActiveState", "--property=SubState")
	if err != nil {
		if isUnitAbsent(err) {
			return false, nil
		}
		return false, fmt.Errorf("service: the state of the installed unit %s could not be determined, the registration was not changed: %w", unitName, err)
	}
	state := parseSystemdShow(out).State
	// A starting instance already runs: it is about to reach the running
	// state, and a reinstall has to switch it to the new program as well.
	return runningState(state), nil
}

// registrationUnitConfig reads systemd's typed D-Bus properties. systemctl's
// ExecStart display joins argv elements with spaces, so it cannot prove their
// boundaries when a path or argument itself contains whitespace. These are the
// manager's parsed settings before runtime environment expansion, not the
// process argv or an atomic snapshot with service state.
func (m *systemdManager) registrationUnitConfig(ctx context.Context) (systemdLoadedConfig, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := m.runSystemctl(ctx, "show", unitName,
		"--property=LoadState")
	if err != nil {
		return systemdLoadedConfig{}, err
	}
	show := parseSystemdShow(out)
	if show.LoadState == "not-found" {
		return systemdLoadedConfig{}, nil
	}
	if show.LoadState != "loaded" {
		return systemdLoadedConfig{}, fmt.Errorf("systemd reported LoadState=%q", show.LoadState)
	}
	properties, err := m.loadedServiceProperties(ctx, unitName)
	if err != nil {
		return systemdLoadedConfig{}, err
	}
	return loadedConfigFromProperties(properties)
}

// currentMainPID reads the MainPID the supervisor reports for the loaded
// service. A running unit without a usable MainPID exposes no process the
// restart proof could judge, so the caller refuses instead of guessing.
func (m *systemdManager) currentMainPID(ctx context.Context) (uint32, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	properties, err := m.loadedServiceProperties(ctx, unitName)
	if err != nil {
		return 0, fmt.Errorf("read the running service process from systemd: %w", err)
	}
	return systemdMainPIDFromProperties(properties)
}

// A fresh manager connection occasionally gets no reply and never recovers,
// so every attempt uses its own connection and deadline, and a read that
// still fails after the attempts fails closed with its original error: a
// persistently unreadable loaded state is never reported as read.
const (
	loadedPropertiesReadAttempts = 3
	loadedPropertiesReadTimeout  = 3 * time.Second
)

// transientLoadedPropertiesError reports whether err is the transport class
// of a lost or broken reply, which a fresh connection can answer again, as
// opposed to a semantic answer of the manager or a caller that gave up.
func transientLoadedPropertiesError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, dbus.ErrClosed) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return false
}

func (m *systemdManager) loadedServiceProperties(ctx context.Context, name string) (map[string]dbus.Variant, error) {
	var lastErr error
	for attempt := 1; attempt <= loadedPropertiesReadAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, loadedPropertiesReadTimeout)
		properties, err := m.loadedServicePropertiesOnce(attemptCtx, name)
		cancel()
		if err == nil {
			return properties, nil
		}
		lastErr = err
		if attempt == loadedPropertiesReadAttempts || ctx.Err() != nil || !transientLoadedPropertiesError(err) {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func (m *systemdManager) loadedServicePropertiesOnce(ctx context.Context, name string) (map[string]dbus.Variant, error) {
	if m.readServiceProperties != nil {
		return m.readServiceProperties(ctx, name)
	}
	conn, err := m.systemdDBusConnection(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: connect to the %s systemd manager: %w", m.scope, err)
	}
	defer conn.Close()

	manager := conn.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1")
	var objectPath dbus.ObjectPath
	// LoadUnit, not GetUnit: GetUnit only answers for a unit already in
	// the loaded set of the manager, and a first registration whose unit
	// file just came in through a reload is not reliably one — the
	// manager collects an inactive unreferenced unit again. LoadUnit
	// loads on demand and returns the object to read, which is what the
	// loaded-command verification of every installation needs.
	if err := manager.CallWithContext(ctx, "org.freedesktop.systemd1.Manager.LoadUnit", dbus.FlagNoAutoStart, name).Store(&objectPath); err != nil {
		return nil, fmt.Errorf("service: read the loaded object path of %s: %w", name, err)
	}
	if !objectPath.IsValid() {
		return nil, fmt.Errorf("service: systemd returned an invalid object path for %s", name)
	}
	unit := conn.Object("org.freedesktop.systemd1", objectPath)
	call := unit.CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", dbus.FlagNoAutoStart, "org.freedesktop.systemd1.Service")
	properties, err := dbusPropertiesFromCall(call)
	if err != nil {
		return nil, fmt.Errorf("service: read the loaded properties of %s: %w", name, err)
	}
	// FragmentPath and DropInPaths belong to the base Unit interface, not
	// Service. Missing values keep source/cleanup proof conservative without
	// blocking an otherwise usable registration.
	call = unit.CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", dbus.FlagNoAutoStart, "org.freedesktop.systemd1.Unit")
	if unitProperties, unitErr := dbusPropertiesFromCall(call); unitErr == nil {
		for _, name := range []string{"FragmentPath", "DropInPaths"} {
			if property, ok := unitProperties[name]; ok {
				properties[name] = property
			}
		}
	}
	return properties, nil
}

func dbusPropertiesFromCall(call *dbus.Call) (map[string]dbus.Variant, error) {
	if call == nil {
		return nil, errors.New("systemd returned no D-Bus property reply")
	}
	if call.Err != nil {
		return nil, call.Err
	}
	if len(call.Body) != 1 {
		return nil, fmt.Errorf("systemd returned %d D-Bus property values, want one dictionary", len(call.Body))
	}
	properties, ok := call.Body[0].(map[string]dbus.Variant)
	if !ok {
		return nil, fmt.Errorf("systemd returned D-Bus property body %T, want map[string]dbus.Variant", call.Body[0])
	}
	return properties, nil
}

func (m *systemdManager) systemdDBusConnection(ctx context.Context) (*dbus.Conn, error) {
	switch m.scope {
	case ScopeSystem:
		if os.Geteuid() == 0 {
			conn, privateErr := connectSystemdPrivate(ctx, "/run/systemd/private")
			if privateErr == nil {
				return conn, nil
			}
			conn, busErr := dbus.ConnectSystemBus(dbus.WithContext(ctx))
			if busErr == nil {
				return conn, nil
			}
			return nil, fmt.Errorf("manager-private socket: %v; system bus: %w", privateErr, busErr)
		}
		return dbus.ConnectSystemBus(dbus.WithContext(ctx))

	case ScopeUser:
		if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
			conn, privateErr := connectSystemdPrivate(ctx, filepath.Join(runtimeDir, "systemd", "private"))
			if privateErr == nil {
				return conn, nil
			}
			if address := sessionBusAddress(); address != "" {
				conn, busErr := dbus.Connect(address, dbus.WithContext(ctx))
				if busErr == nil {
					return conn, nil
				}
				return nil, fmt.Errorf("manager-private socket: %v; session bus: %w", privateErr, busErr)
			}
			return nil, privateErr
		}
		if address := sessionBusAddress(); address != "" {
			return dbus.Connect(address, dbus.WithContext(ctx))
		}
		return nil, errors.New("no user manager transport is available from XDG_RUNTIME_DIR or the session bus environment")

	default:
		return nil, fmt.Errorf("unknown systemd scope %q", m.scope)
	}
}

func connectSystemdPrivate(ctx context.Context, socketPath string) (*dbus.Conn, error) {
	stream, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	closeStream := func() { _ = stream.Close() }
	if err := checkSystemdPrivatePeer(stream); err != nil {
		closeStream()
		return nil, err
	}
	conn, err := dbus.NewConn(stream, dbus.WithContext(ctx))
	if err != nil {
		closeStream()
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := stream.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	if err := conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Geteuid()))}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := stream.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func checkSystemdPrivatePeer(stream net.Conn) error {
	unixConn, ok := stream.(*net.UnixConn)
	if !ok {
		return errors.New("systemd manager-private transport is not a Unix socket")
	}
	rawConn, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var peer *unix.Ucred
	var peerErr error
	if err := rawConn.Control(func(fd uintptr) {
		peer, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if peerErr != nil {
		return peerErr
	}
	if peer.Uid != 0 && peer.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("systemd manager-private peer uid %d does not match the current user", peer.Uid)
	}
	return nil
}

func sessionBusAddress() string {
	if address := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); address != "" && address != "autolaunch:" {
		return address
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		currentUser, err := user.Current()
		if err != nil {
			return ""
		}
		runtimeDir = filepath.Join("/run/user", currentUser.Uid)
	}
	busPath := filepath.Join(runtimeDir, "bus")
	if _, err := os.Stat(busPath); err == nil {
		return "unix:path=" + dbus.EscapeBusAddressValue(busPath)
	}
	content, err := os.ReadFile(filepath.Join(runtimeDir, "dbus-session"))
	if err != nil {
		return ""
	}
	const prefix = "DBUS_SESSION_BUS_ADDRESS="
	address := strings.TrimRight(strings.TrimPrefix(string(content), prefix), "\r\n")
	if !strings.HasPrefix(string(content), prefix) {
		return ""
	}
	return address
}

// loadedMatchesCanonicalCommand holds the loaded command to the canonical
// command of the unit that was written. ExecStart is the strict-owned field
// of the ownership contract: the typed loaded vector must be exactly the
// vector of the canonical command, with no loaded flags and no
// ignore-errors entry, or the write did not become the effective
// configuration.
//
// The expectation is the source word vector of the command the writer
// emitted, with the escape forms resolved the way the supervisor stores
// them before it spawns the process: the doubled dollar sign of a literal
// dollar sign stays doubled, the doubled percent sign becomes the literal
// percent sign.
func loadedMatchesCanonicalCommand(loaded systemdLoadedConfig, unit canonicalUnit) error {
	expected := splitSystemdWords(systemdExecStart(unit.installOptions()))
	if !loaded.loaded || len(loaded.execArgs) != len(expected) || loaded.ignoreErrors || len(loaded.execFlags) != 0 {
		return errors.New("the effective ExecStart is not the canonical command of the unit")
	}
	if loaded.execPath != expected[0] {
		return errors.New("the effective executable is not the canonical program of the unit")
	}
	for i, arg := range expected {
		if loaded.execArgs[i] != arg {
			return errors.New("the effective ExecStart is not the canonical command of the unit")
		}
	}
	return nil
}

// detectExecStartOverride refuses an installation whose effective command a
// drop-in owns: ExecStart is the field HFDesk owns, so a typed loaded
// command that differs from the canonical command of the file that passed
// detector A is an override in force, and HFDesk refuses before any change
// instead of overwriting it. The loaded side is the typed D-Bus projection
// of registrationUnitConfig, and an unloaded unit has no effective command
// to defend.
func detectExecStartOverride(loaded systemdLoadedConfig, unit canonicalUnit, scope Scope) error {
	if !loaded.loaded {
		return nil
	}
	if err := loadedMatchesCanonicalCommand(loaded, unit); err != nil {
		return execStartOverrideRefusal(loaded, scope)
	}
	return nil
}

// execStartOverrideRefusal states the result, the cause with the DropInPaths
// the supervisor reports, and the one next action of the ownership contract.
//
// The action must clear the cause and make the next attempt succeed: an
// ExecStart= override a drop-in carries is removed from the exact drop-in
// file that carries it — moving customizations into local.conf cannot clear
// an override that already lives there — and the unit files are reloaded
// after the edit, because the supervisor holds the loaded command in memory
// until that reload reads the file again; a loaded command no reported
// drop-in explains only a reload reads again. A reported drop-in this
// process cannot read may be the one that carries the override, so no
// command is named for that state: the refusal says it repeats until the
// override is removed instead of promising a reload that cannot clear it.
func execStartOverrideRefusal(loaded systemdLoadedConfig, scope Scope) error {
	cause := fmt.Sprintf("the command systemd loads for %s is not the command in its unit file", unitName)
	if loaded.dropInPathsKnown && len(loaded.dropInPaths) > 0 {
		quoted := make([]string, 0, len(loaded.dropInPaths))
		for _, dropIn := range loaded.dropInPaths {
			quoted = append(quoted, fmt.Sprintf("%q", dropIn))
		}
		cause += fmt.Sprintf(" (drop-ins: %s)", strings.Join(quoted, ", "))
	}
	install := "hfdesk service install"
	reload := scopeSupervisorHint(scope) + " daemon-reload"
	if owner, unreadable := execStartOverrideDropIn(loaded.dropInPaths); owner != "" {
		return fmt.Errorf("service: the update was refused and the registration was not changed: %s. Remove the %q override from %q, reload the unit files with %q, and run %q again",
			cause, "ExecStart=", owner, reload, install)
	} else if unreadable {
		return fmt.Errorf("service: the update was refused and the registration was not changed: %s. HFDesk cannot read every drop-in the supervisor reports, so it names no command: the refusal repeats until the ExecStart= override is removed from the drop-in that carries it", cause)
	}
	return fmt.Errorf("service: the update was refused and the registration was not changed: %s. Reload the unit files with %q and run %q again",
		cause, reload, install)
}

// execStartOverrideDropIn returns the drop-in that carries an ExecStart=
// override: the last path the supervisor reports whose file holds such a
// directive, because the last drop-in loaded wins, and whether a reported
// drop-in could not be read. A file that cannot be read proves nothing: it
// may be the drop-in that carries the override, and the caller must not
// promise an action that cannot clear that state.
func execStartOverrideDropIn(paths []string) (owner string, unreadable bool) {
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			unreadable = true
			continue
		}
		if dropInHoldsExecStart(content) {
			owner = path
		}
	}
	return owner, unreadable
}

// dropInHoldsExecStart reports whether a drop-in carries an ExecStart=
// directive, the one directive of a drop-in that changes the command the
// supervisor loads.
func dropInHoldsExecStart(content []byte) bool {
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ExecStart=") {
			return true
		}
	}
	return false
}

// detectForeignRegistration refuses a registration whose loaded unit does
// not come from the unit file HFDesk writes: a vendor or generated fragment
// under our unit name is not our registration and is never adopted, while a
// unit systemd did not load has nothing to refuse.
func detectForeignRegistration(unitPath string, loaded systemdLoadedConfig, scope Scope) error {
	if !loaded.loaded {
		return nil
	}
	if loaded.fragmentPathKnown && filepath.IsAbs(loaded.fragmentPath) &&
		filepath.Clean(loaded.fragmentPath) == filepath.Clean(unitPath) {
		return nil
	}
	return foreignRegistrationRefusal(unitPath, loaded, scope)
}

// foreignRegistrationRefusal states the result, the cause with the fragment
// systemd loaded, and the one next action of the ownership contract.
//
// The action must clear the loaded foreign registration: the supervisor
// keeps a loaded fragment in memory — even a removed one — until a reload
// reads the file system again, so a fragment file that is there is removed
// and the unit files reloaded before the retry, a fragment file that is
// already gone needs only that reload, and a registration with no readable
// fragment has no file to remove — that state names no command and says so
// instead of promising a retry that would refuse again.
func foreignRegistrationRefusal(unitPath string, loaded systemdLoadedConfig, scope Scope) error {
	cause := fmt.Sprintf("the loaded registration of %s has no readable fragment of its own", unitName)
	if !loaded.fragmentPathKnown || !filepath.IsAbs(loaded.fragmentPath) {
		return fmt.Errorf("service: the update was refused and the registration was not changed: %s. No file names where this registration comes from, so HFDesk names no command that removes it: whatever loaded it must remove it, and an installation refuses again until the supervisor no longer loads it", cause)
	}
	cause = fmt.Sprintf("systemd loads %s from %q, not from its unit file %q", unitName, loaded.fragmentPath, unitPath)
	reload := scopeSupervisorHint(scope) + " daemon-reload"
	if _, err := os.Stat(loaded.fragmentPath); err == nil || !os.IsNotExist(err) {
		return fmt.Errorf("service: the update was refused and the registration was not changed: %s. Move your customizations into %q, remove the unit fragment %q, reload the unit files with %q, and run %q again",
			cause, unitDropInPath(unitPath), loaded.fragmentPath, reload, "hfdesk service install")
	}
	return fmt.Errorf("service: the update was refused and the registration was not changed: %s. Reload the unit files with %q and run %q again",
		cause, reload, "hfdesk service install")
}

func sameSystemdLoadedConfig(a, b systemdLoadedConfig) bool {
	return a.loaded == b.loaded && a.execPath == b.execPath && slices.Equal(a.execArgs, b.execArgs) &&
		slices.Equal(a.execFlags, b.execFlags) && a.ignoreErrors == b.ignoreErrors &&
		a.workingDirectory == b.workingDirectory && a.user == b.user && a.group == b.group &&
		a.supplementaryGroupsKnown == b.supplementaryGroupsKnown && slices.Equal(a.supplementaryGroups, b.supplementaryGroups) &&
		slices.Equal(a.environment, b.environment) && slices.Equal(a.environmentFiles, b.environmentFiles) &&
		a.fragmentPathKnown == b.fragmentPathKnown && a.fragmentPath == b.fragmentPath &&
		a.dropInPathsKnown == b.dropInPathsKnown && slices.Equal(a.dropInPaths, b.dropInPaths)
}

type systemdExecStartEntry struct {
	Path                    string
	Argv                    []string
	IgnoreErrors            bool
	StartTimestamp          uint64
	ExitTimestamp           uint64
	StartTimestampMonotonic uint64
	ExitTimestampMonotonic  uint64
	PID                     uint32
	Code                    int32
	Status                  int32
}

type systemdExecStartExEntry struct {
	Path                    string
	Argv                    []string
	Flags                   []string
	StartTimestamp          uint64
	ExitTimestamp           uint64
	StartTimestampMonotonic uint64
	ExitTimestampMonotonic  uint64
	PID                     uint32
	Code                    int32
	Status                  int32
}

func loadedConfigFromProperties(properties map[string]dbus.Variant) (systemdLoadedConfig, error) {
	loaded := systemdLoadedConfig{loaded: true}
	if err := storeDBusProperty(properties, "WorkingDirectory", "s", &loaded.workingDirectory); err != nil {
		return systemdLoadedConfig{}, err
	}
	if err := storeDBusProperty(properties, "User", "s", &loaded.user); err != nil {
		return systemdLoadedConfig{}, err
	}
	if err := storeDBusProperty(properties, "Group", "s", &loaded.group); err != nil {
		return systemdLoadedConfig{}, err
	}
	if property, ok := properties["SupplementaryGroups"]; ok {
		if signature := property.Signature().String(); signature != "as" {
			return systemdLoadedConfig{}, fmt.Errorf("systemd property SupplementaryGroups has signature %q, want %q", signature, "as")
		}
		if err := dbus.Store([]interface{}{property.Value()}, &loaded.supplementaryGroups); err != nil {
			return systemdLoadedConfig{}, fmt.Errorf("decode systemd property SupplementaryGroups: %w", err)
		}
		loaded.supplementaryGroupsKnown = true
	}
	if err := storeDBusProperty(properties, "Environment", "as", &loaded.environment); err != nil {
		return systemdLoadedConfig{}, err
	}
	if err := storeDBusProperty(properties, "EnvironmentFiles", "a(sb)", &loaded.environmentFiles); err != nil {
		return systemdLoadedConfig{}, err
	}
	if property, ok := properties["FragmentPath"]; ok {
		if signature := property.Signature().String(); signature != "s" {
			return systemdLoadedConfig{}, fmt.Errorf("systemd property FragmentPath has signature %q, want %q", signature, "s")
		}
		if err := dbus.Store([]interface{}{property.Value()}, &loaded.fragmentPath); err != nil {
			return systemdLoadedConfig{}, fmt.Errorf("decode systemd property FragmentPath: %w", err)
		}
		loaded.fragmentPathKnown = true
	}
	if property, ok := properties["DropInPaths"]; ok {
		if signature := property.Signature().String(); signature != "as" {
			return systemdLoadedConfig{}, fmt.Errorf("systemd property DropInPaths has signature %q, want %q", signature, "as")
		}
		if err := dbus.Store([]interface{}{property.Value()}, &loaded.dropInPaths); err != nil {
			return systemdLoadedConfig{}, fmt.Errorf("decode systemd property DropInPaths: %w", err)
		}
		loaded.dropInPathsKnown = true
	}

	if command, ok := properties["ExecStartEx"]; ok {
		if signature := command.Signature().String(); signature != "a(sasasttttuii)" {
			return systemdLoadedConfig{}, fmt.Errorf("systemd returned ExecStartEx with signature %q", signature)
		}
		var commands []systemdExecStartExEntry
		if err := dbus.Store([]interface{}{command.Value()}, &commands); err != nil {
			return systemdLoadedConfig{}, fmt.Errorf("decode ExecStartEx: %w", err)
		}
		if len(commands) > 1 {
			return systemdLoadedConfig{}, errors.New("systemd reports multiple ExecStart commands")
		}
		if len(commands) == 1 {
			loaded.execPath = commands[0].Path
			loaded.execArgs = commands[0].Argv
			loaded.execFlags = commands[0].Flags
		}
	} else if command, ok := properties["ExecStart"]; ok {
		if signature := command.Signature().String(); signature != "a(sasbttttuii)" {
			return systemdLoadedConfig{}, fmt.Errorf("systemd returned ExecStart with signature %q", signature)
		}
		var commands []systemdExecStartEntry
		if err := dbus.Store([]interface{}{command.Value()}, &commands); err != nil {
			return systemdLoadedConfig{}, fmt.Errorf("decode ExecStart: %w", err)
		}
		if len(commands) > 1 {
			return systemdLoadedConfig{}, errors.New("systemd reports multiple ExecStart commands")
		}
		if len(commands) == 1 {
			loaded.execPath = commands[0].Path
			loaded.execArgs = commands[0].Argv
			loaded.ignoreErrors = commands[0].IgnoreErrors
		}
	} else {
		return systemdLoadedConfig{}, errors.New("systemd did not return ExecStart or ExecStartEx")
	}
	return loaded, nil
}

func storeDBusProperty(properties map[string]dbus.Variant, name, signature string, destination interface{}) error {
	property, ok := properties[name]
	if !ok {
		return fmt.Errorf("systemd did not return property %s", name)
	}
	if got := property.Signature().String(); got != signature {
		return fmt.Errorf("systemd property %s has signature %q, want %q", name, got, signature)
	}
	if err := dbus.Store([]interface{}{property.Value()}, destination); err != nil {
		return fmt.Errorf("decode systemd property %s: %w", name, err)
	}
	return nil
}

func systemdMainPIDFromProperties(properties map[string]dbus.Variant) (uint32, error) {
	property, ok := properties["MainPID"]
	if !ok {
		return 0, errors.New("systemd did not report the running service MainPID")
	}
	if signature := property.Signature().String(); signature != "u" {
		return 0, fmt.Errorf("systemd property MainPID has signature %q, want %q", signature, "u")
	}
	var pid uint32
	if err := dbus.Store([]interface{}{property.Value()}, &pid); err != nil {
		return 0, fmt.Errorf("decode systemd property MainPID: %w", err)
	}
	if pid == 0 {
		return 0, errors.New("systemd reported no running service MainPID")
	}
	return pid, nil
}

// systemdShow holds the properties of the unit the supervisor reported.
type systemdShow struct {
	// LoadState tells whether the supervisor knows the unit.
	LoadState string
	// ActiveState is the high level state of the unit.
	ActiveState string
	// SubState is the low level state of the unit.
	SubState string
	// State is the lifecycle state of the unit in the vocabulary of the
	// shared contract.
	State State
}

// parseSystemdShow parses the output of the supervisor and maps the
// reported properties onto the lifecycle states of the shared contract.
func parseSystemdShow(output string) systemdShow {
	show := systemdShow{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "LoadState":
			show.LoadState = strings.TrimSpace(value)
		case "ActiveState":
			show.ActiveState = strings.TrimSpace(value)
		case "SubState":
			show.SubState = strings.TrimSpace(value)
		}
	}
	show.State = show.state()
	return show
}

// state maps the reported properties of the unit onto the lifecycle states
// of the shared contract.
func (s systemdShow) state() State {
	if s.LoadState == "not-found" {
		return StateNotInstalled
	}
	switch s.ActiveState {
	case "active":
		return StateRunning
	case "inactive":
		return StateStopped
	case "activating":
		return StateStarting
	case "deactivating":
		return StateStopping
	case "failed":
		return StateFailed
	}
	return StateUnknown
}
