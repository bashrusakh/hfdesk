// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

const (
	// unitName is the supervisor visible name of the HFDesk unit.
	unitName = ServiceName + ".service"

	// unitFileMode is the permission mode of a written unit file. The
	// supervisor reads the unit as an unprivileged user, so the file must
	// be world readable.
	unitFileMode os.FileMode = 0o644

	// unitDirMode is the permission mode of the directory that holds the
	// unit file.
	unitDirMode os.FileMode = 0o755

	// systemUnitPath is the machine wide location of the unit file.
	systemUnitPath = "/etc/systemd/system/" + unitName

	// systemServiceHome is the HOME of a machine wide instance. It is a
	// machine level directory: a system scope instance never inherits the
	// home of the user who happens to run the install command.
	systemServiceHome = "/var/lib/hfdesk"
)

// systemdManager drives the systemd supervisor of HFDesk.
//
// A user scope installation is a systemd user unit: it belongs to the
// session of the user and it is the default, because a desktop or a
// background installation belongs to the user who uses it.
//
// A system scope installation is a machine wide system unit. It is an
// explicit choice and it needs root: the manager never escalates
// privileges, it fails with a clear error when the process is not root.
//
// The supervisor owns the lifecycle of the instance. HFDesk stays a normal
// foreground process: the unit is a simple service, the process is never
// forked, detached or put into the background by HFDesk itself.
//
// Booting without a login session is an explicit choice of the user or of
// the administrator. The manager therefore never enables linger on its own:
// `loginctl enable-linger` (or anything like it) is never called, so a
// user scope instance starts with the session of the user and stops with it.
//
// The manager drives the supervisor with systemctl. The supervisor is never
// called through a shell, and the manager never launches an interactive
// process: no browser and no tray icon are ever started from the service
// layer.
type systemdManager struct {
	scope Scope
}

// newPlatformManager returns the systemd manager of the given scope.
func newPlatformManager(scope Scope) (Manager, error) {
	switch scope {
	case ScopeUser, ScopeSystem:
		return &systemdManager{scope: scope}, nil
	default:
		return nil, fmt.Errorf("service: unknown scope %q (want %q or %q)", scope, ScopeUser, ScopeSystem)
	}
}

// unitPath returns the path of the unit file of the manager scope.
//
// A user scope unit lives in the "systemd/user" directory of the per user
// configuration directory: $XDG_CONFIG_HOME/systemd/user/hfdesk.service, or
// ~/.config/systemd/user/hfdesk.service when XDG_CONFIG_HOME is not set. A
// system scope unit lives in /etc/systemd/system.
//
// The path is resolved on every call, so the manager always describes the
// registration of the current environment.
func (m *systemdManager) unitPath() (string, error) {
	if m.scope == ScopeSystem {
		return systemUnitPath, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil || home == "" {
			return "", errors.New("service: the configuration directory of the user is unknown, the unit file path cannot be resolved")
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", unitName), nil
}

// systemctlArgs prefixes the supervisor arguments with the scope of the
// manager: a user scope installation is driven by "systemctl --user", a
// machine wide one by plain "systemctl".
func (m *systemdManager) systemctlArgs(args ...string) []string {
	if m.scope == ScopeUser {
		return append([]string{"--user"}, args...)
	}
	return args
}

// supervisorHint returns the command a user can run to inspect the unit of
// the manager scope.
func (m *systemdManager) supervisorHint() string {
	if m.scope == ScopeUser {
		return "systemctl --user"
	}
	return "systemctl"
}

// systemctlError describes a failed supervisor call. The message of the
// supervisor is kept, so the reason of a failure is never lost.
type systemctlError struct {
	args   []string
	err    error
	stderr string
}

func (e *systemctlError) Error() string {
	msg := fmt.Sprintf("service: systemctl %s failed: %v", strings.Join(e.args, " "), e.err)
	if e.stderr != "" {
		msg += ": " + e.stderr
	}
	return msg
}

func (e *systemctlError) Unwrap() error { return e.err }

// runSystemctl runs the supervisor with the given arguments and returns its
// standard output. The supervisor is never called through a shell.
func (m *systemdManager) runSystemctl(ctx context.Context, args ...string) (string, error) {
	full := m.systemctlArgs(args...)
	cmd := exec.CommandContext(ctx, "systemctl", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &systemctlError{
			args:   full,
			err:    err,
			stderr: strings.TrimSpace(stderr.String()),
		}
	}
	return stdout.String(), nil
}

// requireRoot rejects a machine wide operation that needs root when the
// process does not have it. The manager never escalates privileges itself:
// it never calls sudo, su or pkexec.
func (m *systemdManager) requireRoot(action string) error {
	if m.scope != ScopeSystem {
		return nil
	}
	if os.Geteuid() == 0 {
		return nil
	}
	return fmt.Errorf("service: %s in the system scope requires root: run the command as root, HFDesk never escalates privileges itself", action)
}

// Install registers the service with the supervisor.
//
// The installation validates first, writes the unit file atomically,
// reloads the supervisor and enables the unit for the boot of the scope. It
// never starts the instance: starting is an explicit, separate command.
//
// A failure after the unit file was written rolls the unit file back, so an
// installation never leaves a half configured unit behind.
func (m *systemdManager) Install(ctx context.Context, opts InstallOptions) error {
	if err := ValidateInstallOptions(opts); err != nil {
		return err
	}
	// The root requirement is checked before any directory is created: a
	// permission error of the machine wide directories is a much less
	// readable answer than the real reason.
	if err := m.requireRoot("an installation"); err != nil {
		return err
	}
	if err := PrepareDirs(opts); err != nil {
		return err
	}
	if err := m.preflight(opts); err != nil {
		return err
	}

	unit, err := renderUnit(opts)
	if err != nil {
		return err
	}
	path, err := m.unitPath()
	if err != nil {
		return err
	}

	previous, existed, err := readUnitFile(path)
	if err != nil {
		return err
	}
	if err := writeUnitFile(path, unit); err != nil {
		return err
	}
	if err := m.reloadAndEnable(ctx); err != nil {
		m.rollbackUnit(ctx, path, previous, existed)
		return err
	}
	return nil
}

// preflight rejects an installation the supervisor could not run: a missing
// supervisor, an unusable bind address or an executable that cannot be run.
func (m *systemdManager) preflight(opts InstallOptions) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("service: systemctl is not available: %w", err)
	}
	if _, _, err := ParseListen(opts.Listen); err != nil {
		return err
	}
	info, err := os.Stat(opts.Executable)
	if err != nil {
		return fmt.Errorf("service: executable %q is not usable: %w", opts.Executable, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("service: executable %q is not a regular file", opts.Executable)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("service: executable %q is not executable", opts.Executable)
	}
	return nil
}

// reloadAndEnable makes the supervisor aware of the unit file and enables
// the unit for the boot of the scope.
func (m *systemdManager) reloadAndEnable(ctx context.Context) error {
	if _, err := m.runSystemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if _, err := m.runSystemctl(ctx, "enable", unitName); err != nil {
		return err
	}
	return nil
}

// rollbackUnit restores the registration of a failed installation: the
// previous unit file is written back, or the unit file of the failed
// installation is removed when there was none. The supervisor is reloaded
// afterwards, so it never keeps a stale unit.
//
// The rollback is best effort: the original failure of the installation is
// the error the caller gets.
func (m *systemdManager) rollbackUnit(ctx context.Context, path string, previous []byte, existed bool) {
	// The caller may have cancelled the context, the rollback still has to
	// clean the registration up.
	cleanup := context.WithoutCancel(ctx)
	if existed {
		_ = writeUnitFile(path, string(previous))
	} else {
		_ = os.Remove(path)
	}
	_, _ = m.runSystemctl(cleanup, "daemon-reload")
}

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

// Start starts the registered unit. Starting a unit that already runs is a
// stable success, not an error.
func (m *systemdManager) Start(ctx context.Context) error {
	if err := m.requireInstalled(); err != nil {
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
// a stable success, not an error.
func (m *systemdManager) Stop(ctx context.Context) error {
	if err := m.requireInstalled(); err != nil {
		return err
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
func (m *systemdManager) Restart(ctx context.Context) error {
	if err := m.requireInstalled(); err != nil {
		return err
	}
	if _, err := m.runSystemctl(ctx, "restart", unitName); err != nil {
		return err
	}
	return nil
}

// requireInstalled rejects a lifecycle command for a service that is not
// registered, so the caller gets a readable reason instead of a cryptic
// supervisor error.
func (m *systemdManager) requireInstalled() error {
	path, err := m.unitPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("service: %s is not installed", unitName)
		}
		return err
	}
	return nil
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

// Status reports the state of the registered service.
//
// The registration is described by the unit file: the executable, the bind
// address and the runtime paths come from the registered unit, not from the
// current intent of the caller. A registered executable that is gone is
// reported as broken, so a moved or deleted binary is never reported as a
// running service.
//
// The name of the status is the label of the service ("hfdesk"), not the
// name of the unit file ("hfdesk.service"): the label is the identity the
// user knows the service by.
func (m *systemdManager) Status(ctx context.Context) (Status, error) {
	path, err := m.unitPath()
	if err != nil {
		return Status{}, err
	}
	status := Status{
		Scope:    m.scope,
		Name:     ServiceName,
		UnitPath: path,
	}

	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			status.State = StateNotInstalled
			status.Detail = fmt.Sprintf("the unit file does not exist: %s", path)
			return status, nil
		}
		return Status{}, err
	}

	unit := parseUnit(string(content))
	status.Executable = unit.ExecStart
	status.Listen = unit.listen()
	status.Paths = unit.paths()

	if status.Executable != "" {
		if _, err := os.Stat(status.Executable); err != nil {
			status.State = StateBroken
			status.Detail = fmt.Sprintf("registered executable not found: %s", status.Executable)
			return status, nil
		}
	}

	out, err := m.runSystemctl(ctx, "show", unitName,
		"--property=LoadState", "--property=ActiveState", "--property=SubState")
	if err != nil {
		status.State = StateUnknown
		status.Detail = err.Error()
		return status, err
	}
	show := parseSystemdShow(out)
	status.State = show.State
	switch status.State {
	case StateNotInstalled:
		status.Detail = "the supervisor does not know the unit, reload the supervisor to pick the unit file up"
	case StateFailed:
		status.Detail = fmt.Sprintf("the unit failed, see `%s status %s`", m.supervisorHint(), unitName)
	}
	return status, nil
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

// readUnitFile reads the current content of a unit file. The second result
// reports whether the file existed at all.
func readUnitFile(path string) ([]byte, bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return content, true, nil
}

// writeUnitFile writes a unit file atomically: the content goes into a
// temporary file of the same directory and is renamed over the target. A
// reader of the unit file therefore never sees a half written unit.
func writeUnitFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, unitDirMode); err != nil {
		return fmt.Errorf("service: create the unit directory %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".hfdesk-unit-*")
	if err != nil {
		return fmt.Errorf("service: create a temporary unit file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// The temporary file only survives when it was not renamed.
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("service: write the unit file %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("service: close the unit file %q: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, unitFileMode); err != nil {
		return fmt.Errorf("service: set the mode of the unit file %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("service: install the unit file %q: %w", path, err)
	}
	return nil
}

// unitTemplate is the shape of a HFDesk unit.
//
// The unit keeps the supervisor in charge: HFDesk stays a normal foreground
// process, the unit is a simple service and the process is never forked,
// detached or put into the background by HFDesk itself.
//
// The hardening set is deliberately small. HFDesk reads and writes the
// models, the cache and the state of the instance in directories that depend
// on the installation, so a sandbox that hides the file system or the home
// directory of the user would break the instance instead of protecting it.
// Every further restriction would need the explicit list of the paths the
// instance may write, and that list depends on the installation.
//
// The unit does not redirect the output: HFDesk writes its own bounded
// rotating log file into the log directory of the instance and the supervisor
// keeps the standard output and the standard error of the process in its
// journal.
const unitTemplate = `# HFDesk service unit.
#
# Generated by the hfdesk service install command; a later installation
# overwrites this file.
#
# The unit holds no access data: an instance reads its own settings from its
# config file, which only the owner of the instance can read. The unit
# therefore carries the executable and the canonical arguments of the
# process only.

[Unit]
Description=HFDesk
Documentation=https://github.com/bashrusakh/hfdesk
After=network-online.target
Wants=network-online.target

[Service]
# The supervisor owns the lifecycle of the instance: HFDesk stays a normal
# foreground process and the supervisor restarts it when it fails.
Type=simple
ExecStart=%s
WorkingDirectory=%s
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
UMask=0077
Environment=%s

[Install]
WantedBy=%s
`

// renderUnit renders the systemd unit of an installation.
//
// The function is pure: it renders the text of the unit and never touches
// the system. The command of the unit is the canonical argument vector of
// the shared contract, so every platform registers the same command.
func renderUnit(opts InstallOptions) (string, error) {
	home, err := unitHomeDir(opts)
	if err != nil {
		return "", err
	}

	words := make([]string, 0, len(RunArgs(opts))+1)
	words = append(words, systemdQuoteAlways(opts.Executable))
	for _, arg := range RunArgs(opts) {
		words = append(words, systemdQuote(arg))
	}

	target := "default.target"
	if opts.Scope == ScopeSystem {
		target = "multi-user.target"
	}

	return fmt.Sprintf(unitTemplate,
		strings.Join(words, " "),
		systemdQuote(opts.Paths.StateDir),
		systemdQuote("HOME="+home),
		target,
	), nil
}

// unitHomeDir returns the HOME of the registered process.
//
// A user scope instance runs as the user of the session, so it keeps the
// home of that user. A machine wide instance never inherits the home of the
// user who runs the install command: it uses the machine level state
// directory as its home.
func unitHomeDir(opts InstallOptions) (string, error) {
	if opts.Scope == ScopeSystem {
		return systemServiceHome, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("service: the home directory of the user is unknown: %w", err)
	}
	if home == "" {
		return "", errors.New("service: the home directory of the user is unknown")
	}
	return home, nil
}

// systemdQuote renders one value of a unit file. The value is quoted only
// when it needs it, so a plain path stays readable.
func systemdQuote(value string) string {
	if value == "" {
		return `""`
	}
	if !strings.ContainsAny(value, " \t\"'\\%") {
		return value
	}
	return systemdQuoteAlways(value)
}

// systemdQuoteAlways renders one value of a unit file in the C style
// quoting of systemd, no matter what the value contains.
func systemdQuoteAlways(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '%':
			// A percent sign starts a specifier of the supervisor, a
			// doubled one is a literal percent sign.
			b.WriteString("%%")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// parsedUnit holds the registration facts of a unit file.
type parsedUnit struct {
	// ExecStart is the executable of the unit.
	ExecStart string
	// ExecArgs are the arguments of the executable.
	ExecArgs []string
	// WorkingDirectory is the working directory of the unit.
	WorkingDirectory string
	// Environment holds the environment variables of the unit.
	Environment map[string]string
}

// listen returns the bind address of the registered instance.
func (u parsedUnit) listen() string {
	return u.flagValue("--listen")
}

// paths returns the runtime paths of the registered instance.
//
// The values come from the registered unit file, so the status describes
// the registration and not the current intent of the caller.
func (u parsedUnit) paths() runtimepaths.Paths {
	paths := runtimepaths.Paths{
		ConfigFile: u.flagValue("--config"),
		StateDir:   u.flagValue("--state-dir"),
		LogDir:     u.flagValue("--log-dir"),
	}
	if paths.StateDir == "" {
		paths.StateDir = u.WorkingDirectory
	}
	return paths
}

// flagValue returns the value of a flag of the argument vector of the unit.
func (u parsedUnit) flagValue(flag string) string {
	for i := 0; i+1 < len(u.ExecArgs); i++ {
		if u.ExecArgs[i] == flag {
			return u.ExecArgs[i+1]
		}
	}
	return ""
}

// parseUnit reads the registration facts of a unit file.
//
// The parser is tolerant: a unit file that was edited by hand is described
// as far as it can be read, so the status of the registration stays
// meaningful.
func parseUnit(content string) parsedUnit {
	unit := parsedUnit{Environment: map[string]string{}}
	section := ""
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.ToLower(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		if section != "service" {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ExecStart":
			words := splitSystemdWords(value)
			if len(words) == 0 {
				continue
			}
			unit.ExecStart = words[0]
			unit.ExecArgs = words[1:]
		case "WorkingDirectory":
			if words := splitSystemdWords(value); len(words) > 0 {
				unit.WorkingDirectory = words[0]
			}
		case "Environment":
			for _, word := range splitSystemdWords(value) {
				name, envValue, ok := strings.Cut(word, "=")
				if !ok {
					continue
				}
				unit.Environment[name] = envValue
			}
		}
	}
	return unit
}

// splitSystemdWords splits a command line of a unit file into its words.
// It understands the quoting of the supervisor: single quotes, double quotes
// and backslash escapes.
func splitSystemdWords(line string) []string {
	var (
		words []string
		word  strings.Builder
		has   bool
	)
	flush := func() {
		if !has {
			return
		}
		words = append(words, word.String())
		word.Reset()
		has = false
	}
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		case c == '\'':
			has = true
			end := strings.IndexByte(line[i+1:], '\'')
			if end < 0 {
				word.WriteString(line[i+1:])
				i = len(line)
				continue
			}
			word.WriteString(line[i+1 : i+1+end])
			i = i + 1 + end
		case c == '"':
			has = true
			for i++; i < len(line); i++ {
				if line[i] == '\\' && i+1 < len(line) {
					word.WriteString(unescapeSystemdChar(line[i+1]))
					i++
					continue
				}
				if line[i] == '%' && i+1 < len(line) && line[i+1] == '%' {
					word.WriteByte('%')
					i++
					continue
				}
				if line[i] == '"' {
					break
				}
				word.WriteByte(line[i])
			}
		case c == '\\':
			has = true
			if i+1 < len(line) {
				word.WriteString(unescapeSystemdChar(line[i+1]))
				i++
			}
		case c == '%':
			has = true
			if i+1 < len(line) && line[i+1] == '%' {
				word.WriteByte('%')
				i++
				continue
			}
			word.WriteByte('%')
		default:
			has = true
			word.WriteByte(c)
		}
	}
	flush()
	return words
}

// unescapeSystemdChar returns the character an escape sequence of a unit
// file stands for.
func unescapeSystemdChar(c byte) string {
	switch c {
	case 'n':
		return "\n"
	case 't':
		return "\t"
	case 'r':
		return "\r"
	case 's':
		return " "
	default:
		return string(c)
	}
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
