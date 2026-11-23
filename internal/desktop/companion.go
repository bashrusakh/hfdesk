// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// The private launch flags of the tray companion.
//
// Both flags belong to the desktop executable alone: SplitCompanionArgs
// strips them from the argument list BEFORE the host flags are parsed, so
// they are not part of the host usage text — exactly like the hidden
// --service-action family of the elevated helper. They are the command
// line of the relaunch the installation spawns, and of a hand-typed
// companion launch.
const (
	// companionModeFlag starts the launch as a tray companion: tray,
	// service actions and window, WITHOUT a server.
	companionModeFlag = "--tray-companion"
	// companionWaitFlag names how long this companion launch may wait
	// for the single instance lock of the session. It is passed only by
	// the relaunch of an installation, whose child is started while the
	// exiting host and the elevated helper still hold that lock. A
	// companion launch without it never waits: it reports the running
	// instance exactly like every ordinary second launch.
	companionWaitFlag = "--companion-wait"
)

// companionLaunch is the companion part of a desktop command line.
type companionLaunch struct {
	// Enabled reports that the launch was asked to be a tray companion.
	Enabled bool
	// Wait is the bounded wait for the single instance lock, zero for
	// every launch but the relaunch of an installation.
	Wait time.Duration
}

// SplitCompanionArgs separates the private tray-companion flags from the
// host flags of the desktop command line.
//
// It returns the companion part and the remaining arguments (the ORIGINAL
// slice when no companion flag is present, so an ordinary launch passes
// its arguments on byte for byte). An argument list that carries
// --companion-wait without --tray-companion, or a wait value that is not
// a duration, is an error: the flags name one relaunch protocol, and a
// half of it is reported instead of guessed.
func SplitCompanionArgs(args []string) (companionLaunch, []string, error) {
	var launch companionLaunch
	waitValue := ""
	waitSeen := false
	rest := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == companionModeFlag || arg == "-tray-companion":
			launch.Enabled = true
		case arg == companionWaitFlag || arg == "-companion-wait":
			if index+1 >= len(args) {
				return companionLaunch{}, rest, fmt.Errorf(
					"%s needs a duration as its value (for example %s 2m30s)", companionWaitFlag, companionWaitFlag)
			}
			index++
			waitValue = args[index]
			waitSeen = true
		case strings.HasPrefix(arg, companionWaitFlag+"="):
			waitValue = strings.TrimPrefix(arg, companionWaitFlag+"=")
			waitSeen = true
		case strings.HasPrefix(arg, "-companion-wait="):
			waitValue = strings.TrimPrefix(arg, "-companion-wait=")
			waitSeen = true
		default:
			rest = append(rest, arg)
		}
	}
	if !launch.Enabled && !waitSeen {
		// An ordinary launch: the caller keeps its own slice, so
		// nothing about its command line changes.
		return companionLaunch{}, args, nil
	}
	if !launch.Enabled {
		return companionLaunch{}, rest, fmt.Errorf(
			"%s is only part of a tray companion launch: it needs %s as well", companionWaitFlag, companionModeFlag)
	}
	if waitSeen {
		wait, err := time.ParseDuration(waitValue)
		if err != nil || wait < 0 {
			return companionLaunch{}, rest, fmt.Errorf(
				"%s %q is not a non-negative duration (for example 2m30s)", companionWaitFlag, waitValue)
		}
		launch.Wait = wait
	}
	return launch, rest, nil
}

// companionRelaunchArgs returns the command line of the child the
// installation relaunches as a tray companion: the arguments of THIS
// launch — so the log settings of the host travel with it — plus the two
// flags that name the companion mode and its bounded lock wait.
func companionRelaunchArgs(argv []string, wait time.Duration) []string {
	relaunch := make([]string, 0, len(argv)+4)
	relaunch = append(relaunch, argv...)
	return append(relaunch, companionModeFlag, companionWaitFlag, wait.String())
}

// companionOpenLogAction is the name of the one action the document of the
// companion window can ask the window to perform: open the full service log
// file with the default handler of the operating system.
//
// The window binds this name on the host side (companionWindowActions) and
// the page's single button calls it. The page names a fixed action and
// carries no data: the path of the file comes from the host, never from the
// document, and nothing of the page reaches a server — the companion
// listens to nothing.
const companionOpenLogAction = "hfdeskOpenServiceLog"

// serviceLogFileName is the name of the active log file of a service
// instance, next to its rotated generations (documented in docs/SERVICE.md).
const serviceLogFileName = "hfdesk.log"

// The bounds of the service log tail the companion window shows.
//
// The tail is the END of the file and it is read under both limits, so a
// large log never reaches the page as a whole: the byte bound caps the read
// itself — the file is seeked to its end and only the last
// serviceLogTailBytes are read — and the line bound caps what a tail of
// that size renders. The window states nothing of the limits; they are the
// honest volume bound of this view, pinned by test.
const (
	serviceLogTailBytes = 64 * 1024
	serviceLogTailLines = 200
)

// serviceLogPath returns the path of the active service log file.
//
// A log directory the registration names decides (an installation may have
// registered a non-default one), and a machine without a readable
// registration falls back to the default of the machine-wide scope — the
// ProgramData folder of the documented default.
func serviceLogPath(registeredLogDir string) string {
	if registeredLogDir != "" {
		return filepath.Join(registeredLogDir, serviceLogFileName)
	}
	paths, err := service.DefaultPaths(service.ScopeSystem)
	if err != nil {
		return ""
	}
	return filepath.Join(paths.LogDir, serviceLogFileName)
}

// readServiceLogTail returns the tail of the log file at path under the
// documented bounds: at most serviceLogTailBytes from the end of the file,
// cut to whole lines, at most serviceLogTailLines of them, in file order and
// ending with the newest line.
//
// A file that cannot be opened or read returns its error; a read that was
// bound reports no error — the bound is the view, not a failure.
func readServiceLogTail(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// Only the end of the file is read: everything before the window is
	// seeked over, never loaded.
	var start int64
	if info, statErr := file.Stat(); statErr == nil && info.Size() > serviceLogTailBytes {
		start = info.Size() - serviceLogTailBytes
	}
	if start > 0 {
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			return "", err
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, serviceLogTailBytes))
	if err != nil {
		return "", err
	}
	text := string(data)
	if start > 0 {
		// The bound cut a line in half at its start: that fragment is
		// dropped, so the tail begins with a whole line.
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			text = text[newline+1:]
		} else {
			text = ""
		}
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	if last := len(lines) - 1; last >= 0 && lines[last] == "" {
		lines = lines[:last]
	}
	if len(lines) > serviceLogTailLines {
		lines = lines[len(lines)-serviceLogTailLines:]
	}
	return strings.Join(lines, "\n"), nil
}

// serviceLogSection is the service-log part of the page of the companion
// window: the path of the file, and exactly one of the honest shapes the
// section shows — the bounded tail of a readable file, the fact that the
// file does not exist yet, or the one-line reason it could not be read. An
// empty section (no path) claims nothing and renders no content.
type serviceLogSection struct {
	// Path is the service log file this section speaks about — the very
	// file the open-full-file action opens. Empty when no path could be
	// resolved.
	Path string
	// Missing reports the file does not exist: the service has not
	// written a log yet.
	Missing bool
	// ReadError is the one-line reason the file could not be read
	// (permissions and the like), empty when it could.
	ReadError string
	// Tail is the bounded tail of the file, newest line last.
	Tail string
}

// serviceLogSectionAt reads the service-log section from the file at path.
//
// The states are the honest ones: a file that is not there is missing, a
// file that cannot be read carries its reason as a single line, and a
// readable file carries its bounded tail.
func serviceLogSectionAt(path string) serviceLogSection {
	section := serviceLogSection{Path: path}
	if path == "" {
		return section
	}
	tail, err := readServiceLogTail(path)
	switch {
	case err == nil:
		section.Tail = tail
	case os.IsNotExist(err):
		section.Missing = true
	default:
		section.ReadError = oneLine(err.Error())
	}
	return section
}

// oneLine collapses a multi-line machine message into the one line the
// window shows it on.
func oneLine(message string) string {
	return strings.Join(strings.Fields(message), " ")
}

// serviceLogMissingText returns the report of the open-full-file action when
// the service log does not exist yet — the honest state of a service that
// has not written a log, with no action to offer (there is nothing to open)
// and the standard log-path line of this launch.
func serviceLogMissingText(path string) string {
	text := "The service log file has not been created yet, so there is nothing to open."
	if path != "" {
		text += "\n\nThe service writes its log at " + path + "."
	}
	return failureWindowText(text)
}

// serviceLogOpenFailedText returns the report of the open-full-file action
// whose default handler could not open the file: the state, the cause as
// presented by the system, the path of the service log itself, and the
// standard log-path line of this launch like every failure window of a
// launch that has a log.
func serviceLogOpenFailedText(path string, openErr error) string {
	text := "The service log file could not be opened in the default program."
	if openErr != nil {
		text += "\n\n" + oneLine(openErr.Error())
	}
	text += "\n\nThe service log is at " + path + "."
	return failureWindowText(text)
}

// serviceLogOpenFailedCaption is the caption of both reports of the
// open-full-file action, so the user reads one title for one button.
const serviceLogOpenFailedCaption = "HFDesk - the service log file could not be opened"

// companionRelaunchWaitTimeoutText returns the report of a relaunch child
// that could not take the single instance lock of the session within its
// bounded wait.
//
// The report is the state (another HFDesk process of this session still
// holds the session — usually the elevated helper that is finishing the
// installation) and the one action that resolves it (start the desktop
// app again once that process finished). It carries no log-path line: the
// lock is taken before the log file is opened, so this launch has no log
// file of its own yet — the established rule of the lock reports (a
// launch that ends here names no log, because there is none).
func companionRelaunchWaitTimeoutText() string {
	return "HFDesk could not take over the desktop session: another HFDesk process of this Windows session still holds the single-instance object.\n\n" +
		"That is usually the elevated helper finishing the service installation. Start the desktop app again in a moment."
}

// companionWebView2MissingText returns the report of a tray companion
// whose Runtime / Logs window cannot be shown because the WebView2
// runtime is missing.
//
// The report is the state (the window cannot be shown while the
// companion itself keeps running) and the one action that ends it —
// installing the Evergreen runtime — with the standard log-path line of
// every failure window of a launch that has a log.
func companionWebView2MissingText() string {
	return failureWindowText(
		"The Microsoft Edge WebView2 runtime is not installed, so the Runtime / Logs window of the tray companion cannot be shown.\n\n" +
			"Install the Evergreen runtime from\nhttps://developer.microsoft.com/microsoft-edge/webview2/\n\n" +
			"The tray companion itself keeps running: its service actions stay available from the icon in the notification area.")
}

// companionPageState is the model of the page the Runtime / Logs window of
// a tray companion shows.
//
// The companion has no server, so its window cannot render the runtime
// page of an instance: the page is built here, from the facts the window
// read — the honest state of the service, the web-UI address the
// REGISTRATION names (offered by the fact of the registration, whether or
// not the service currently answers it), the log file of this launch, and
// the service-log section (the bounded tail of the service's own log, or
// the honest state of a file that is not there or could not be read).
type companionPageState struct {
	// State is the honest one-line state of the service.
	State string
	// WebURL is the browser URL of the registered listen address, empty
	// when the registration names none or one that cannot be read as a
	// URL.
	WebURL string
	// Address names the registered address when WebURL is empty: the raw
	// address beside an unreadable URL, or the honest sentence of a
	// registration without any address. Empty when there is no
	// registration to speak of.
	Address string
	// LogPath is the path of the log file of this launch, empty for a
	// launch that has none yet.
	LogPath string
	// ServiceLog is the service-log section: the bounded tail of the
	// service log file, or the honest state of a file that does not
	// exist yet or could not be read. The zero value claims nothing and
	// renders no section content.
	ServiceLog serviceLogSection
}

// serviceLogScriptActionPlaceholder is the token of the script template the
// page builder replaces with companionOpenLogAction.
//
// The action name has ONE source of truth: the companionOpenLogAction
// constant, which is also the key the host binds (companionWindowActions).
// The template never names it — the name is substituted while the page is
// generated — so the page and the host binding cannot drift apart, and a
// literal name that reaches the page from anywhere else fails the test that
// pins the rendered document.
const serviceLogScriptActionPlaceholder = "{{action}}"

// serviceLogScriptTemplate is the only script of the page, and it performs
// exactly two local operations with no channel of any kind: the tail box is
// scrolled to its end — the window shows the END of the service log — and
// the one button forwards the fixed open-file action name to the function
// the window bound on the host side.
//
// Nothing here fetches, listens or carries data out of the document: the
// path of the file never travels through the page (the host owns it), and
// the invariant of the companion — nothing of it listens — is unchanged.
// Both operations guard their element, so a section without a tail or
// without a button runs the script as a no-op.
const serviceLogScriptTemplate = `<script>
(function () {
	var tail = document.getElementById("service-log-tail");
	if (tail) { tail.scrollTop = tail.scrollHeight; }
	var button = document.getElementById("open-service-log");
	if (button && window.` + serviceLogScriptActionPlaceholder + `) {
		button.addEventListener("click", function () { window.` + serviceLogScriptActionPlaceholder + `(); });
	}
})();
</script>
`

// serviceLogScript renders the script template of the page with the one
// action name of this build substituted in — the same constant the host
// binds, so the two can never name different functions.
func serviceLogScript() string {
	return strings.ReplaceAll(serviceLogScriptTemplate, serviceLogScriptActionPlaceholder, companionOpenLogAction)
}

// companionPage renders the model into the page of the companion window.
//
// Everything the model carries is escaped, so a value from the machine
// can never become markup. The page fetches nothing: it is a document,
// not a client of a listener — the invariant of the companion is that
// nothing of it listens, and the page honors that by having no channel
// to a server of its own. Its single script (serviceLogScript) only
// scrolls the tail box to its end and forwards the one fixed open-file
// action to the host-bound function — the action name it calls is
// substituted from companionOpenLogAction while the page is generated.
//
// The service log section is the last content of the document: it shows
// the bounded tail ending with the newest line, or the one honest state
// of a file that is not there or could not be read, plus the single
// action that opens the whole file with the default handler.
func companionPage(state companionPageState) string {
	var page strings.Builder
	page.WriteString(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>HFDesk - Runtime / Logs</title>
<style>
body { font-family: "Segoe UI", system-ui, sans-serif; margin: 0; padding: 2rem; color: #1a1a1a; background: #f6f7f9; }
main { max-width: 44rem; margin: 0 auto; }
h1 { font-size: 1.15rem; font-weight: 600; margin: 0 0 1rem; }
p { margin: 0.6rem 0; line-height: 1.5; }
a { color: #0b57d0; }
code { font-family: Consolas, "Courier New", monospace; font-size: 0.9em; background: #eceff3; padding: 0.1em 0.35em; border-radius: 3px; word-break: break-all; }
.label { color: #5a6270; font-size: 0.85rem; text-transform: uppercase; letter-spacing: 0.04em; margin-top: 1.75rem; }
pre { font-family: Consolas, "Courier New", monospace; font-size: 0.85em; background: #eceff3; padding: 0.75rem; border-radius: 4px; margin: 0.6rem 0; max-height: 20rem; overflow: auto; white-space: pre-wrap; word-break: break-word; }
button { font: inherit; font-size: 0.9rem; padding: 0.35rem 0.9rem; border: 1px solid #c3c8d0; border-radius: 4px; background: #fff; color: #1a1a1a; cursor: pointer; }
button:hover { background: #f0f2f5; }
</style>
</head>
<body>
<main>
`)
	if state.State != "" {
		page.WriteString("<h1>" + html.EscapeString(state.State) + "</h1>\n")
	}
	if state.WebURL != "" {
		escaped := html.EscapeString(state.WebURL)
		page.WriteString("<p>The web UI of the service is at <a href=\"" + escaped + "\">" + escaped + "</a>.</p>\n")
	} else if state.Address != "" {
		page.WriteString("<p>" + html.EscapeString(state.Address) + "</p>\n")
	}
	// The log file of THIS launch: the standard line of every surface of
	// a launch that has a log, stated before the section that speaks
	// about the log of the service.
	if state.LogPath != "" {
		page.WriteString("<p>The log file is at <code>" + html.EscapeString(state.LogPath) + "</code>.</p>\n")
	}
	section := state.ServiceLog
	if section.Path == "" && !section.Missing && section.ReadError == "" && section.Tail == "" {
		// A model without a service-log source claims nothing: the
		// section stays out instead of showing a heading over
		// nothing.
		page.WriteString("</main>\n</body>\n</html>\n")
		return page.String()
	}
	page.WriteString("<p class=\"label\">Service log</p>\n")
	switch {
	case section.Missing:
		page.WriteString("<p>The service log file has not been created yet.</p>\n")
	case section.ReadError != "":
		reason := html.EscapeString(strings.TrimSuffix(section.ReadError, "."))
		page.WriteString("<p>The service log file could not be read: " + reason + ".</p>\n")
	case section.Tail != "":
		page.WriteString("<pre id=\"service-log-tail\">" + html.EscapeString(section.Tail) + "</pre>\n")
	}
	if section.Path != "" && !section.Missing {
		page.WriteString("<p><button id=\"open-service-log\" type=\"button\">Open the full log file</button></p>\n")
	}
	page.WriteString(serviceLogScript())
	page.WriteString("</main>\n</body>\n</html>\n")
	return page.String()
}
