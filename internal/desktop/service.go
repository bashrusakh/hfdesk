// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"fmt"
	"net"
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
// The helper is the same desktop executable, started with administrator
// rights and with the hidden --service-action flag. It never starts a server,
// a tray icon or a window: it drives the service control manager and reports
// the outcome in a native message box.
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
	// restarts the service. The desktop host passes its own process id, so
	// the service never starts while the desktop still holds the listen
	// address. Zero means that there is nothing to wait for, which is the
	// case for an action the service gate launches: the gate ends before it
	// ever creates a server.
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

// serviceChoicePrompt describes the choice the service gate asks the user.
//
// The prompt carries the facts of the registration, so the dialog, its text
// and its buttons are derived from one value: the question, the answer and
// the action that follows it cannot drift apart.
type serviceChoicePrompt struct {
	// State is the state the service control manager reports.
	State service.State
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

// serviceStateSettled reports whether the state is a state that lasts.
//
// A pending state is not a result: the caller waits for the instance to leave
// it before it reports the outcome of an action.
func serviceStateSettled(state service.State) bool {
	return state != service.StateStarting && state != service.StateStopping
}

// serviceLoopbackHost is the host a browser of the machine reaches the
// service through.
const serviceLoopbackHost = "127.0.0.1"

// serviceURL returns the browser URL of a service that listens on addr.
//
// The registration carries the address the service really binds. A wildcard
// address is mapped onto the loopback interface, because that is the address
// the browser of the user reaches the machine through: 0.0.0.0, :: and every
// other spelling of "every interface" are the same bind, and an empty host
// binds every interface as well. The literal host name "localhost" is kept:
// the browser resolves it to the loopback interface itself, and the operator
// of the machine may have a reason to name it.
//
// An address that cannot be read as a host and a port is an error: the caller
// reports it instead of opening a URL it invented.
func serviceURL(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("the registration names no listen address")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("the registered listen address %q cannot be read: %w", addr, err)
	}
	if port == "" {
		return "", fmt.Errorf("the registered listen address %q carries no port", addr)
	}
	if host == "" {
		host = serviceLoopbackHost
	} else if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = serviceLoopbackHost
	}
	return "http://" + net.JoinHostPort(host, port), nil
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

// findSiblingExecutable locates the console program that sits next to a
// desktop binary: the folder of the desktop program and the program itself,
// which names the pair the release ships.
//
// It is a package level variable so the tests can drive a folder that has no
// console program: the folder of the running test executable is the folder of
// the test binary, and it cannot hold the program of a release.
var findSiblingExecutable = findConsoleExecutable

// findConsoleExecutable returns the path of the console program of HFDesk in
// dir.
//
// HFDesk stays portable: the desktop never downloads, copies or replaces a
// binary. The release ships the console program next to the desktop program,
// so the installation registers the program of the very folder the desktop
// runs from, and an update re-registers the program of the folder the helper
// runs from.
//
// The program paired with the desktop program is preferred: a release ships
// hfdesk_windows_amd64_1.3.0.exe next to hfdesk-desktop_windows_amd64_1.3.0.exe,
// and unpacking a new release next to the old one leaves two version-stamped
// pairs in the same folder. The pair of the program that asks is the one the
// user meant, so its exact name is looked up first. The plain name hfdesk.exe
// is the next fallback, and a folder with exactly one hfdesk*.exe is accepted
// as well. Zero and several candidates without a pair are an error that names
// the folder: registering one of several programs would register a program the
// user did not choose.
//
// The desktop program is never a candidate, whatever its name carries: it is
// the program that asks for the console program, and registering it would
// start a second desktop host as the service.
func findConsoleExecutable(dir, desktop string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("the folder %s cannot be read: %w", dir, err)
	}

	paired := pairedConsoleExecutableName(desktop)
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
// pair.
//
// The two programs of a release differ only by the prefix of their names, so the
// pair is derived from the name of the desktop program: the leading
// hfdesk-desktop prefix is replaced with hfdesk, and the rest of the name (the
// version stamp and the suffix) is kept. A desktop program that does not carry
// the prefix names no pair, so nothing is preferred for it.
func pairedConsoleExecutableName(desktop string) string {
	base := filepath.Base(desktop)
	if !strings.HasPrefix(strings.ToLower(base), desktopExecutableName) {
		return ""
	}
	return consoleExecutableName + base[len(desktopExecutableName):]
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
