// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// ErrUnsupportedServiceAction is returned by RunServiceAction on a platform
// that has no Windows service helper.
var ErrUnsupportedServiceAction = errors.New(
	"desktop: installing, updating and starting the HFDesk service is only supported on Windows")

// elevationDeclined is the reason of a refused request for administrator
// rights.
//
// The user does not see a raw Windows error code: the refusal is a decision of
// the user, and every notice that reports the launch of the helper says so.
const elevationDeclined = "the request for administrator rights was declined"

// serviceAction is one action of the elevated service helper.
//
// The helper is the same desktop executable, started with administrator rights
// and with the hidden --service-action flag; it drives the service control
// manager and reports the outcome in a native message box.
type serviceAction string

const (
	// serviceActionInstall registers the service, starts it and verifies
	// that it runs.
	serviceActionInstall serviceAction = "install"
	// serviceActionUpdate re-registers the service, keeps its settings and
	// its running state, and never starts a service that was stopped.
	serviceActionUpdate serviceAction = "update"
	// serviceActionStart starts the registered service and verifies that
	// it runs.
	serviceActionStart serviceAction = "start"
)

// serviceInstallAftermath describes what a failed installation left on the
// machine.
//
// A failed first installation must not leave a registration that serves
// nothing, because the service gate would end every later launch. Only the
// registration this action wrote itself is therefore removed again.
type serviceInstallAftermath int

const (
	// serviceInstallRemoved means that the registration the failed
	// installation wrote was removed again, so HFDesk is not installed as a
	// Windows service.
	serviceInstallRemoved serviceInstallAftermath = iota
	// serviceInstallKept means that the registration was left in place: it
	// existed before the action, or the state before the action could not be
	// read.
	serviceInstallKept
	// serviceInstallRemovalFailed means that the registration the failed
	// installation wrote could not be removed again.
	serviceInstallRemovalFailed
)

// String returns the aftermath for a log record.
func (a serviceInstallAftermath) String() string {
	switch a {
	case serviceInstallRemoved:
		return "removed"
	case serviceInstallKept:
		return "kept"
	case serviceInstallRemovalFailed:
		return "removal-failed"
	default:
		return "unknown"
	}
}

// ServiceActionOptions describes one invocation of the elevated helper.
//
// The flags are private to the desktop executable: they are not part of the
// shared options of the server host, and they are not documented in its usage.
type ServiceActionOptions struct {
	// Action is the action the helper performs: "install", "update" or
	// "start". Every other value is reported and ends the helper with a
	// failure.
	Action string

	// WaitPID is the process the helper waits for before it starts or
	// restarts the service: the desktop host passes its own process id, so
	// the service never starts while the desktop still holds the listen
	// address. Zero means there is nothing to wait for.
	WaitPID int

	// Version is the build version of the desktop host that launched the
	// helper. It is reported to the user, so the message names the release
	// the service was installed from.
	Version string
}

// parseServiceAction converts the value of the helper flag into an action.
//
// The comparison is case-insensitive and ignores surrounding space, so a
// hand-typed value is accepted as well. Every other value is rejected: the
// helper never guesses the action it was asked to perform.
func parseServiceAction(value string) (serviceAction, error) {
	action := serviceAction(strings.ToLower(strings.TrimSpace(value)))
	switch action {
	case serviceActionInstall, serviceActionUpdate, serviceActionStart:
		return action, nil
	default:
		return "", fmt.Errorf("desktop: unknown service action %q (want %q, %q or %q)",
			value, serviceActionInstall, serviceActionUpdate, serviceActionStart)
	}
}

// serviceChoice is the answer of the user to the service gate dialog.
type serviceChoice int

const (
	// serviceChoiceCancel changes nothing: the launch ends and starts no
	// server.
	serviceChoiceCancel serviceChoice = iota
	// serviceChoiceOpen opens the web UI of the running service.
	serviceChoiceOpen
	// serviceChoiceStart starts the stopped service.
	serviceChoiceStart
	// serviceChoiceUpdate re-registers the service from the folder of the
	// desktop executable.
	serviceChoiceUpdate
)

// serviceChoicePrompt describes the choice the service gate asks the user. The
// dialog, its text and its buttons are derived from one value, so the question,
// the answer and the action that follows it cannot drift apart.
type serviceChoicePrompt struct {
	// State is the state the service control manager reports.
	State service.State
	// Running is the supervisor's own running fact of the status the
	// prompt was built from. A registration whose program is gone is
	// reported as broken while its process still serves, so the running
	// fact decides the first button together with the state.
	Running bool
	// Listen is the registered listen address of the service.
	Listen string
	// URL is the browser URL derived from Listen. It is empty when the
	// address cannot be read as one.
	URL string
	// DataDir is the state directory of the service: the machine-wide
	// directory the service owns, which is never the data of the desktop.
	DataDir string
	// Detail is the human readable detail of the state, when the service
	// reports one.
	Detail string
}

// serviceStateRunning reports whether the state is a service that serves.
//
// The pending start state counts as running: the instance is on its way up,
// and the user wants to reach it instead of starting it a second time. The
// stopping state does not count: the instance is on its way down, so it is
// treated like a stopped one and is never reported as serving.
func serviceStateRunning(state service.State) bool {
	return state == service.StateRunning || state == service.StateStarting
}

// serviceStatusRunning reports whether a status describes a service that runs.
//
// The supervisor's own fact comes first: a registration whose program is gone
// is reported as broken while the instance it started still serves, and such a
// status has to be taken for a running service. The state is the fallback of a
// manager that does not report the fact, which keeps the classification as the
// only evidence for it.
func serviceStatusRunning(status service.Status) bool {
	if status.Running {
		return true
	}
	return serviceStateRunning(status.State)
}

// servicePromptRunning reports whether the service of a prompt runs.
//
// The prompt carries the running fact of the status it was built from, so the
// question of the gate, the label of its first button and the action behind it
// are three renderings of one decision.
func servicePromptRunning(prompt serviceChoicePrompt) bool {
	return prompt.Running || serviceStateRunning(prompt.State)
}

// serviceStateSettled reports whether the state is a state that lasts.
//
// A pending state is not a result: the caller waits for the instance to leave
// it before it reports the outcome of an action.
func serviceStateSettled(state service.State) bool {
	return state != service.StateStarting && state != service.StateStopping
}

// serviceURL returns the browser URL of a service that listens on addr.
//
// A wildcard address has no address a browser can open, so it is taken from
// service.LocalAddress: the loopback interface of the same family, which is the
// address the readiness probe verifies the service on. Every other host is
// kept, including "localhost".
//
// An address that cannot be read as a host and a port, and a registration
// without any address, are errors: the caller reports them instead of opening a
// URL it invented.
func serviceURL(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("the registration names no listen address")
	}
	local, err := service.LocalAddress(addr)
	if err != nil {
		return "", fmt.Errorf("the registered listen address %q cannot be read: %w", addr, err)
	}
	return "http://" + local, nil
}

// The file names of the two Windows programs of HFDesk.
const (
	// consoleExecutableName is the program the service runs.
	consoleExecutableName = "hfdesk"
	// desktopExecutableName is the program of the desktop host.
	desktopExecutableName = "hfdesk-desktop"
	// executableSuffix is the suffix of a Windows program.
	executableSuffix = ".exe"
)

// findSiblingExecutable locates the console program that sits next to a desktop
// binary. It is a package level variable so the tests can drive a folder that
// has no console program.
var findSiblingExecutable = findConsoleExecutable

// findConsoleExecutable returns the path of the console program of HFDesk in
// dir.
//
// HFDesk stays portable: the desktop never downloads, copies or replaces a
// binary, so the installation registers the program of the folder the desktop
// runs from.
//
// A version-stamped desktop program requires its pair: a release ships
// hfdesk_windows_amd64_1.3.0.exe next to hfdesk-desktop_windows_amd64_1.3.0.exe,
// and unpacking a new release next to an old one leaves two pairs in the same
// folder. The pair of the program that asks is the one the user meant, so
// copying only the new desktop program next to an old console program would
// register the old program while the success text names the new version; a
// missing pair is therefore an error that names the expected file and folder.
//
// A desktop program without a version stamp names no release, so the existing
// fallbacks stay: hfdesk.exe next, then a folder with exactly one hfdesk*.exe.
// Zero and several candidates without a pair are an error. The desktop program
// is never a candidate: registering it would start a second desktop host as the
// service.
func findConsoleExecutable(dir, desktop string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("the folder %s cannot be read: %w", dir, err)
	}

	paired := pairedConsoleExecutableName(desktop)
	stamp, versioned := desktopVersionStamp(desktop)
	pairedPath := ""
	preferred := ""
	var candidates []string
	for _, entry := range entries {
		if entry.IsDir() || !isConsoleExecutableName(entry.Name()) {
			continue
		}
		// The name alone is not enough: the service runs the program, so
		// it has to be a regular file.
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		switch {
		case paired != "" && strings.EqualFold(entry.Name(), paired):
			pairedPath = path
		case strings.EqualFold(entry.Name(), consoleExecutableName+executableSuffix):
			preferred = path
		default:
			candidates = append(candidates, path)
		}
	}
	if pairedPath != "" {
		return pairedPath, nil
	}
	// A version-stamped desktop program is only installed with the console
	// program of its own release: another program in the folder belongs to
	// another release, so it is not a fallback but a different version.
	if versioned && paired != "" {
		return "", fmt.Errorf("there is no %s in %s: %s is the desktop program of the release %s, and the service only runs the console program of that release. Copy %s of the release next to the desktop app",
			paired, dir, filepath.Base(desktop), stamp, paired)
	}
	if preferred != "" {
		return preferred, nil
	}

	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return "", fmt.Errorf("there is no %s%s in %s", consoleExecutableName, executableSuffix, dir)
	default:
		return "", fmt.Errorf("there are %d programs that could be the console program of HFDesk in %s: %s",
			len(candidates), dir, strings.Join(baseNames(candidates), ", "))
	}
}

// pairedConsoleExecutableName returns the name of the console program the
// release ships next to desktop, or an empty string when the program carries no
// pair: the leading hfdesk-desktop prefix is replaced with hfdesk and the rest
// of the name (version stamp and suffix) is kept.
func pairedConsoleExecutableName(desktop string) string {
	base := filepath.Base(desktop)
	if !strings.HasPrefix(strings.ToLower(base), desktopExecutableName) {
		return ""
	}
	return consoleExecutableName + base[len(desktopExecutableName):]
}

// desktopVersionStamp returns the version segment of the name of a desktop
// program, and whether the name carries one.
//
// Both programs of a release carry the same stamp
// (hfdesk-desktop_windows_amd64_1.4.0.exe), while the local build
// hfdesk-desktop.exe carries none. The stamp is the first segment of the rest of
// the name that reads as a release version, so a name that only carries the
// platform is a name without a version.
func desktopVersionStamp(desktop string) (string, bool) {
	base := filepath.Base(desktop)
	if !strings.HasPrefix(strings.ToLower(base), desktopExecutableName) {
		return "", false
	}
	rest := strings.TrimSuffix(strings.ToLower(base[len(desktopExecutableName):]), executableSuffix)
	for _, segment := range strings.Split(rest, "_") {
		if isVersionSegment(segment) {
			return segment, true
		}
	}
	return "", false
}

// isVersionSegment reports whether a segment of a program name is a release
// version, e.g. "1.4.0", "v1.3.0" or "1.3.0-beta.1".
//
// A version is numeric parts joined by dots, at least two of them, with an
// optional pre-release or build suffix: the platform name amd64 and a build
// number such as 2024 are not read as one, so the fallbacks stay available.
func isVersionSegment(segment string) bool {
	segment = strings.TrimPrefix(strings.ToLower(segment), "v")
	if index := strings.IndexAny(segment, "-+"); index >= 0 {
		segment = segment[:index]
	}
	parts := strings.Split(segment, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return false
			}
		}
	}
	return true
}

// isConsoleExecutableName reports whether name is a candidate for the console
// program of HFDesk.
//
// The comparison is case-insensitive, because the file systems of Windows are.
func isConsoleExecutableName(name string) bool {
	lower := strings.ToLower(name)
	if !strings.HasPrefix(lower, consoleExecutableName) || !strings.HasSuffix(lower, executableSuffix) {
		return false
	}
	return !strings.HasPrefix(lower, desktopExecutableName)
}

// baseNames returns the file names of paths, for an error that names the
// candidates instead of their folder only.
func baseNames(paths []string) []string {
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		names = append(names, filepath.Base(path))
	}
	return names
}
