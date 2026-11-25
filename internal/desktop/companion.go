// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
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
	// the relaunch child of a handover — the installation, and every
	// action whose end state leaves the host beside the service — whose
	// child is started while the exiting host and the elevated helper
	// still hold that lock. A companion launch without it never waits: it
	// reports the running instance exactly like every ordinary second
	// launch.
	companionWaitFlag = "--companion-wait"
	// relaunchWaitFlag names the same bounded wait for a relaunch child
	// that comes back as an ORDINARY launch instead of a companion: the
	// removal of the registration ends its handover in desktop mode, and
	// that child is started while the exiting host and the elevated helper
	// still hold the lock exactly like the companion child is. The wait is
	// deliberately separate from companionWaitFlag: the mode of the child
	// is chosen by --tray-companion alone, so a wait that does not carry
	// that flag is a desktop relaunch and never half of one protocol.
	relaunchWaitFlag = "--relaunch-wait"
)

// companionLaunch is the companion part of a desktop command line.
type companionLaunch struct {
	// Enabled reports that the launch asked to be a tray companion.
	Enabled bool
	// Wait is the bounded wait for the single instance lock, zero for
	// every launch but the relaunch child of a handover.
	Wait time.Duration
}

// SplitCompanionArgs separates the private tray-companion flags from the
// host flags of the desktop command line.
//
// It returns the companion part and the remaining arguments (the ORIGINAL
// slice when no companion flag is present, so an ordinary launch passes
// its arguments on byte for byte). An argument list that carries
// --companion-wait without --tray-companion, or a wait value that is not
// a duration, is an error: those two flags name one relaunch protocol,
// and a half of it is reported instead of guessed. The mode-neutral
// --relaunch-wait of the desktop-mode relaunch carries no mode of its own
// on purpose: it is the whole of that child's protocol.
func SplitCompanionArgs(args []string) (companionLaunch, []string, error) {
	var launch companionLaunch
	waitValue := ""
	waitSeen := ""
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
			waitSeen = companionWaitFlag
		case strings.HasPrefix(arg, companionWaitFlag+"="):
			waitValue = strings.TrimPrefix(arg, companionWaitFlag+"=")
			waitSeen = companionWaitFlag
		case strings.HasPrefix(arg, "-companion-wait="):
			waitValue = strings.TrimPrefix(arg, "-companion-wait=")
			waitSeen = companionWaitFlag
		case arg == relaunchWaitFlag || arg == "-relaunch-wait":
			if index+1 >= len(args) {
				return companionLaunch{}, rest, fmt.Errorf(
					"%s needs a duration as its value (for example %s 2m30s)", relaunchWaitFlag, relaunchWaitFlag)
			}
			index++
			waitValue = args[index]
			waitSeen = relaunchWaitFlag
		case strings.HasPrefix(arg, relaunchWaitFlag+"="):
			waitValue = strings.TrimPrefix(arg, relaunchWaitFlag+"=")
			waitSeen = relaunchWaitFlag
		case strings.HasPrefix(arg, "-relaunch-wait="):
			waitValue = strings.TrimPrefix(arg, "-relaunch-wait=")
			waitSeen = relaunchWaitFlag
		default:
			rest = append(rest, arg)
		}
	}
	if !launch.Enabled && waitSeen == "" {
		// An ordinary launch: the caller keeps its own slice, so
		// nothing about its command line changes.
		return companionLaunch{}, args, nil
	}
	if !launch.Enabled && waitSeen == companionWaitFlag {
		return companionLaunch{}, rest, fmt.Errorf(
			"%s is only part of a tray companion launch: it needs %s as well", companionWaitFlag, companionModeFlag)
	}
	if waitSeen != "" {
		wait, err := time.ParseDuration(waitValue)
		if err != nil || wait < 0 {
			return companionLaunch{}, rest, fmt.Errorf(
				"%s %q is not a non-negative duration (for example %s)", waitSeen, waitValue, waitSeen)
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

// desktopRelaunchArgs returns the command line of the child a handover
// relaunches as an ORDINARY launch: the arguments of THIS launch — so the
// log settings of the host travel with it — plus the mode-neutral bounded
// lock wait and no mode flag, so the child runs the ordinary startup and its
// own service gate decides how it continues. That is the end state of the
// removal of the registration: with the registration gone the gate reports
// the machine without one and the launch takes the desktop mode again.
func desktopRelaunchArgs(argv []string, wait time.Duration) []string {
	relaunch := make([]string, 0, len(argv)+3)
	relaunch = append(relaunch, argv...)
	return append(relaunch, relaunchWaitFlag, wait.String())
}

// The fixed action names the document of the Logs window can call on the
// window host.
//
// The window binds these names on the host side (companionWindowActions,
// desktopWindowActions), and a document calls a name only through the
// element it renders: the fallback document calls the open action, the
// retry and — in the state its own button shows — ONE service action: the
// start beside a registration the read found settled without a running
// instance, the update beside a broken registration whose program is gone,
// the installation beside a proven absence of one. The runtime page of the
// server calls the open actions beside the ones it performs over its own
// API. Every name has ONE source of truth — these constants — and the pages
// reference them through the substituted template or through the pinned
// literals of runtime.js, so the page and the host binding cannot drift
// apart into a dead button.
//
// The five service actions are the very flows of the tray menu (see
// windowServiceActions), and BOTH windows bind them: a desktop host has to
// be able to do what its own tray does, and a tray companion whose
// registered address answers shows the live page of the service. The
// fallback document itself exists only beside a tray companion — the
// desktop host always loads the page of its own server.
//
// The names carry no data: the host owns every path it acts on, and the
// open actions run in the process of the WINDOW — in the user's session —
// because the server of a service runs in session 0 and cannot open a
// file or a folder on the user's desktop.
const (
	// openLogFolderAction opens the folder of the active log file.
	openLogFolderAction = "hfdeskOpenLogFolder"
	// openLogFileAction opens the active log file itself with the
	// default handler: the service log for a tray companion, the log of
	// this launch for the desktop host.
	openLogFileAction = "hfdeskOpenLogFile"
	// openUiAction opens the web UI in the default browser: the
	// registered address beside a tray companion (the very address the
	// link of the old static window carried), the address of the own
	// server for the desktop host. The browser belongs to the user's
	// session, so the window host opens it — a server that runs as a
	// service could not reach the desktop from session 0.
	openUiAction = "hfdeskOpenUi"
	// logsTryAgainAction retries the live page: it re-reads the
	// registration, probes the address and shows the live runtime page
	// when it answers, or this fallback again when it does not.
	logsTryAgainAction = "hfdeskTryAgain"
	// logsStartServiceAction starts the registered service the state
	// read of the fallback document found settled without a running
	// instance — the button of that state, and the very flow the Start
	// service item of the menu runs.
	logsStartServiceAction = "hfdeskStartService"
	// logsUpdateServiceAction re-registers the service the state read of
	// the fallback document found BROKEN without a running instance: a
	// start of that state would fail on the missing program, so the
	// document offers the update that repairs the registration — the very
	// flow the Update service item of the menu runs.
	logsUpdateServiceAction = "hfdeskUpdateService"
	// logsInstallServiceAction registers the service on the machine
	// the state read proved WITHOUT a registration — the button of that
	// state, and the very flow the Install service item of the menu
	// runs.
	logsInstallServiceAction = "hfdeskInstallService"
	// logsStopServiceAction stops the running service: the very flow the
	// Stop service item of the menu runs, with its confirmation.
	logsStopServiceAction = "hfdeskStopService"
	// logsDeleteServiceAction removes the registration from the machine:
	// the very flow the Delete service item of the menu runs, with its
	// confirmation and — beside a desktop that serves the address — the
	// handover it commits.
	logsDeleteServiceAction = "hfdeskDeleteService"
)

// logsServiceAction is the primary action the fallback document of the
// Logs window offers for one read of the state — the step of the state,
// in the vocabulary of the menu item it mirrors. The zero value offers no
// step: a state that cannot prove one keeps the document to the two
// buttons it has always had.
type logsServiceAction int

const (
	// logsServiceActionNone offers no step: a running service, a
	// transition, and a state the read could not settle or report.
	logsServiceActionNone logsServiceAction = iota
	// logsServiceActionStart starts the registered service the read
	// found settled without a running instance.
	logsServiceActionStart
	// logsServiceActionInstall registers the service on a machine the
	// read proved without a registration.
	logsServiceActionInstall
	// logsServiceActionUpdate re-registers the service the read found
	// BROKEN without a running instance: the state whose start would fail
	// on the missing program, and whose one repairing step is the update.
	logsServiceActionUpdate
)

// serviceLogFileName is the name of the active log file of a service
// instance, next to its rotated generations (documented in docs/SERVICE.md).
const serviceLogFileName = "hfdesk.log"

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

// The bounds of the service log tail the fallback document shows.
//
// The tail is the END of the file and it is read under both limits, so a
// large log never reaches the document as a whole: the byte bound caps the
// read itself — the file is seeked to its end and only the last
// serviceLogTailBytes are read — and the line bound caps what a tail of
// that size renders. The document states nothing of the limits; they are the
// honest volume bound of this view, pinned by test.
const (
	serviceLogTailBytes = 64 * 1024
	serviceLogTailLines = 200
)

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

// serviceLogSection is the service-log part of the fallback document of the
// Logs window: the path of the file, and exactly one of the honest shapes the
// section shows — the bounded tail of a readable file, the fact that the
// file does not exist yet, or the one-line reason it could not be read. An
// empty section (no path) claims nothing and renders no content.
type serviceLogSection struct {
	// Path is the service log file this section speaks about — the very
	// file the open-the-log-file action opens. Empty when no path could
	// be resolved.
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

// serviceLogMissingText returns the report of the open-the-log-file action
// when the service log does not exist yet — the honest state of a service
// that has not written a log, with no action to offer (there is nothing to
// open) and the standard log-path line of this launch.
func serviceLogMissingText(path string) string {
	text := "The service log file has not been created yet, so there is nothing to open."
	if path != "" {
		text += "\n\nThe service writes its log at " + path + "."
	}
	return failureWindowText(text)
}

// serviceLogOpenFailedText returns the report of the open-the-log-file
// action whose default handler could not open the service log: the state,
// the cause as presented by the system, the path of the service log
// itself, and the standard log-path line of this launch like every failure
// window of a launch that has a log.
func serviceLogOpenFailedText(path string, openErr error) string {
	text := "The service log file could not be opened in the default program."
	if openErr != nil {
		text += "\n\n" + oneLine(openErr.Error())
	}
	text += "\n\nThe service log is at " + path + "."
	return failureWindowText(text)
}

// serviceLogOpenFailedCaption is the caption of both reports of the
// open-the-log-file action of a tray companion, so the user reads one
// title for one button.
const serviceLogOpenFailedCaption = "HFDesk - the service log file could not be opened"

// noLogFileText returns the report of an open action whose log file could
// not be resolved at all: state only, no action and no invented path, with
// the standard log-path line of this launch when this launch has one.
func noLogFileText() string {
	return failureWindowText("There is no log file to open.")
}

// logFileMissingText returns the report of the open-the-log-file action of
// the desktop host when the log file of this launch does not exist yet —
// state only, nothing to open, plus the standard log-path line that names
// exactly the file the action looked for.
func logFileMissingText() string {
	return failureWindowText("The log file has not been created yet, so there is nothing to open.")
}

// logFileOpenFailedText returns the report of the open-the-log-file action
// of the desktop host whose default handler refused the file: the state,
// the cause as presented by the system, and the standard log-path line of
// this launch — which is the very file the action tried to open.
func logFileOpenFailedText(openErr error) string {
	text := "The log file could not be opened in the default program."
	if openErr != nil {
		text += "\n\n" + oneLine(openErr.Error())
	}
	return failureWindowText(text)
}

// logFileOpenFailedCaption is the caption of both reports of the
// open-the-log-file action of the desktop host, so the user reads one
// title for one button.
const logFileOpenFailedCaption = "HFDesk - the log file could not be opened"

// logFolderMissingText returns the report of the open-the-log-folder action
// when the folder of the log file does not exist yet: state, the folder it
// would have opened, and the standard log-path line of this launch.
func logFolderMissingText(dir string) string {
	text := "The log folder has not been created yet, so there is nothing to open."
	if dir != "" {
		text += "\n\nThe log folder is at " + dir + "."
	}
	return failureWindowText(text)
}

// logFolderOpenFailedText returns the report of the open-the-log-folder
// action whose opener failed: the state, the cause as presented by the
// system, the folder itself and the standard log-path line of this launch.
func logFolderOpenFailedText(dir string, openErr error) string {
	text := "The log folder could not be opened."
	if openErr != nil {
		text += "\n\n" + oneLine(openErr.Error())
	}
	if dir != "" {
		text += "\n\nThe log folder is at " + dir + "."
	}
	return failureWindowText(text)
}

// logFolderOpenFailedCaption is the caption of both reports of the
// open-the-log-folder action, so the user reads one title for one button.
const logFolderOpenFailedCaption = "HFDesk - the log folder could not be opened"

// companionRelaunchWaitTimeoutText returns the report of a relaunch child
// that could not take the single instance lock of the session within its
// bounded wait.
//
// The report is the state (another HFDesk process of this session still runs)
// and the one action that resolves it (start the desktop app again once that
// process finished). It carries no log-path line: the
// lock is taken before the log file is opened, so this launch has no log
// file of its own yet — the established rule of the lock reports (a
// launch that ends here names no log, because there is none).
func companionRelaunchWaitTimeoutText() string {
	return "HFDesk could not take over the desktop session: another HFDesk process of this Windows session is still running.\n\n" +
		"Start the desktop app again in a moment."
}

// companionWebView2MissingText returns the report of a tray companion
// whose Logs window cannot be shown because the WebView2 runtime is
// missing.
//
// The report is the state (the window cannot be shown while the
// companion itself keeps running) and the one action that ends it —
// installing the Evergreen runtime — with the standard log-path line of
// every failure window of a launch that has a log.
func companionWebView2MissingText() string {
	return failureWindowText(
		"The Microsoft Edge WebView2 runtime is not installed, so the Logs window of the tray companion cannot be shown.\n\n" +
			"Install the Evergreen runtime from\nhttps://developer.microsoft.com/microsoft-edge/webview2/\n\n" +
			"The tray companion itself keeps running: its service actions stay available from the icon in the notification area.")
}

// runtimePageProbeTimeout bounds the probe of the registered address of
// the Logs window.
//
// The budget covers the round trip to a local server: a service that does
// not answer within it is treated as unreachable, so the window shows its
// fallback state instead of sitting on a navigation that never completes.
var runtimePageProbeTimeout = 3 * time.Second

// runtimePageAnswers reports whether the runtime page at url answers with
// a successful response right now.
//
// It is a package level variable so the tests can decide the answer
// without a real service on the machine they run on.
var runtimePageAnswers = probeRuntimePage

// probeRuntimePage performs the probe: one bounded GET of the runtime
// page, following the same navigation the window would take. Only a
// successful response counts — a refusal, a timeout and a non-200 answer
// all mean the address does not serve the page right now.
func probeRuntimePage(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), runtimePageProbeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	// A bounded drain keeps the connection reusable without ever loading
	// a body of an unknown size into memory.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return response.StatusCode == http.StatusOK
}

// resolveLogsWindow decides what the Logs window shows on an open: the
// runtime page of the registered address when that address answers, the
// fallback document otherwise, together with the key of the read it was
// rendered from.
//
// The probe runs on every open, so the window shows the actual state at
// the moment the user asks for it: a service that stopped since the last
// open meets the honest fallback instead of a hanging navigation, and a
// service that came back meets the live log instead of a stale state
// page. An empty address skips the probe — there is nothing to ask.
func resolveLogsWindow(resolve func() string, fallback func() (string, string)) (target, key, document string) {
	if url := resolve(); url != "" && runtimePageAnswers(url) {
		return url, "", ""
	}
	key, document = fallback()
	return "", key, document
}

// logsFallbackState is the model of the fallback document of the Logs
// window — the state the window shows when the registered address does
// not answer (or names none), in the visual language of the runtime page
// the live window would have loaded.
//
// The document is self-contained and reaches out of nowhere: it fetches
// nothing and listens to nothing — the invariant of the companion is that
// nothing of it listens, and a local document honors that by having no
// channel to any server. Its single script only forwards the fixed
// host-bound actions to the functions the window bound and scrolls the
// tail box to its end.
type logsFallbackState struct {
	// State is the honest one-line state of the service, the heading of
	// the document.
	State string
	// Address is the registered web-UI address that did not answer,
	// stated as a fact (never linked — a link to a dead address would
	// be a dead path). Empty when the registration names none.
	Address string
	// Note is the honest one-line reason there is no address to state,
	// used when Address is empty. Empty when Address is set.
	Note string
	// LogPath is the path of the service log file — the very file the
	// open action opens, so the path and the action always name one
	// file. Empty when none could be resolved: the document then states
	// that fact and offers no action with nothing to open.
	LogPath string
	// Log is the service-log section: the bounded tail of that same log
	// file, or the honest state of a file that does not exist yet or
	// could not be read. The zero value claims nothing and renders no
	// section content.
	Log serviceLogSection
	// ServiceAction is the primary action of the header — the step this
	// state proves: the start of the registration beside a settled state
	// that does not run, the update beside a broken registration whose
	// start would fail on its missing program, the installation beside a
	// proven absence, and no step beside a state that proves none. It leads
	// the header, the open of the log file follows it, and the retry comes
	// last; without it the header is the one it has always been.
	ServiceAction logsServiceAction
}

// The placeholders of the script template the page builder replaces with
// the bound action names.
//
// The action names have ONE source of truth: the openLogFileAction,
// logsTryAgainAction, logsStartServiceAction, logsUpdateServiceAction and
// logsInstallServiceAction constants, which are also the keys the host binds
// (companionWindowActions). The template never names them — the names are
// substituted while the page is generated — so the page and the host
// binding cannot drift apart, and a literal name that reaches the page
// from anywhere else fails the test that pins the rendered document.
const (
	logsFallbackOpenPlaceholder    = "{{open}}"
	logsFallbackTryPlaceholder     = "{{try}}"
	logsFallbackStartPlaceholder   = "{{start}}"
	logsFallbackUpdatePlaceholder  = "{{update}}"
	logsFallbackInstallPlaceholder = "{{install}}"
)

// logsFallbackScriptTemplate is the only script of the fallback document,
// and it performs exactly two local operations with no channel of any kind:
// it forwards the fixed action names to the functions the window bound
// on the host side — open the log file, the service action this document's
// state button names (start beside a settled registration that does not run,
// update beside a broken one, installation beside a proven absence), and
// retry the live page — and scrolls the service-log tail box to its end, so
// the document shows the END of that log.
//
// Nothing here fetches, listens or carries data out of the document: the
// paths never travel through the page (the host owns them), and the
// invariant of the companion — nothing of it listens — is unchanged.
// Every wiring guards its element, so a document without one of the
// elements — a state whose button is not on screen — runs that part as a
// no-op.
const logsFallbackScriptTemplate = `<script>
(function () {
	var tail = document.getElementById("service-log-tail");
	if (tail) { tail.scrollTop = tail.scrollHeight; }
	var open = document.getElementById("open-log-file");
	if (open && window.` + logsFallbackOpenPlaceholder + `) {
		open.addEventListener("click", function () { window.` + logsFallbackOpenPlaceholder + `(); });
	}
	var start = document.getElementById("start-service");
	if (start && window.` + logsFallbackStartPlaceholder + `) {
		start.addEventListener("click", function () { window.` + logsFallbackStartPlaceholder + `(); });
	}
	var update = document.getElementById("update-service");
	if (update && window.` + logsFallbackUpdatePlaceholder + `) {
		update.addEventListener("click", function () { window.` + logsFallbackUpdatePlaceholder + `(); });
	}
	var install = document.getElementById("install-service");
	if (install && window.` + logsFallbackInstallPlaceholder + `) {
		install.addEventListener("click", function () { window.` + logsFallbackInstallPlaceholder + `(); });
	}
	var again = document.getElementById("try-again");
	if (again && window.` + logsFallbackTryPlaceholder + `) {
		again.addEventListener("click", function () { window.` + logsFallbackTryPlaceholder + `(); });
	}
})();
</script>
`

// logsFallbackScript renders the script template of the document with the
// action names of this build substituted in — the same constants the host
// binds, so the two can never name different functions.
func logsFallbackScript() string {
	script := strings.ReplaceAll(logsFallbackScriptTemplate, logsFallbackOpenPlaceholder, openLogFileAction)
	script = strings.ReplaceAll(script, logsFallbackTryPlaceholder, logsTryAgainAction)
	script = strings.ReplaceAll(script, logsFallbackStartPlaceholder, logsStartServiceAction)
	script = strings.ReplaceAll(script, logsFallbackUpdatePlaceholder, logsUpdateServiceAction)
	return strings.ReplaceAll(script, logsFallbackInstallPlaceholder, logsInstallServiceAction)
}

// logsFallbackPage renders the model into the fallback document of the
// Logs window.
//
// The document wears the visual language of the runtime page — the same
// header with the status line, the same fact strip, the same design token
// values — inlined, because a fallback document has no server to load a
// stylesheet from; that is exactly why it is a document and not a page.
// It states the honest state of the service, the address that did not
// answer (or the honest reason there is none), the step that state proves
// as the primary action of the header — the start of the registration
// beside a settled state that does not run, the update beside a broken
// registration, the installation beside a proven absence, and no step
// beside a state that proves none, so the window-diagnosis always offers its
// treatment step when one exists — the
// path of the service log file with the action that opens it, the bounded
// tail of that same log under its own **Service log** section — or the
// honest state of a file that is not there or could not be read — and the
// one action that retries the live page. Everything the model carries is
// escaped, so a value from the machine can never become markup.
func logsFallbackPage(state logsFallbackState) string {
	var page strings.Builder
	page.WriteString(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>HFDesk - Logs</title>
<style>
:root {
  --color-bg: #0a0a0f;
  --color-bg-secondary: #12121a;
  --color-bg-tertiary: #1a1a24;
  --color-surface: #1e1e2a;
  --color-surface-hover: #252532;
  --color-border: #2a2a3a;
  --color-border-light: #3a3a4a;
  --color-text: #c8c8d0;
  --color-text-secondary: #a0a0b0;
  --color-text-muted: #6a6a7a;
  --color-primary: #6366f1;
  --color-primary-hover: #7c7ff7;
  --font-family: 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
  --font-mono: 'JetBrains Mono', 'Fira Code', 'Consolas', monospace;
  --radius-sm: 6px;
  --radius-full: 9999px;
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--color-bg); color: var(--color-text); font-family: var(--font-family); font-size: 14px; }
.shell { display: flex; flex-direction: column; min-height: 100vh; }
.header { display: flex; align-items: center; flex-wrap: wrap; gap: 10px 20px; padding: 14px 20px; background: var(--color-bg-secondary); border-bottom: 1px solid var(--color-border); }
.logo { font-size: 14px; font-weight: 600; }
.status { display: flex; align-items: center; gap: 8px; min-width: 0; }
.dot { width: 8px; height: 8px; border-radius: var(--radius-full); background: var(--color-text-muted); flex-shrink: 0; }
.status h1 { margin: 0; font-size: 13px; font-weight: 500; color: var(--color-text-secondary); }
.actions { display: flex; flex-wrap: wrap; gap: 10px; margin-left: auto; }
.banner { padding: 10px 20px; background: rgba(239, 68, 68, 0.08); border-bottom: 1px solid rgba(239, 68, 68, 0.3); color: var(--color-text-secondary); font-size: 13px; }
.strip { display: flex; flex-wrap: wrap; gap: 10px 32px; padding: 12px 20px; background: var(--color-bg-secondary); border-bottom: 1px solid var(--color-border); }
.stat { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.stat-label { font-size: 11px; color: var(--color-text-muted); text-transform: uppercase; letter-spacing: 0.05em; }
.stat-empty { font-size: 13px; color: var(--color-text-muted); }
code { font-family: var(--font-mono); font-size: 12px; background: var(--color-bg-tertiary); border: 1px solid var(--color-border); border-radius: var(--radius-sm); padding: 3px 8px; overflow-wrap: anywhere; color: var(--color-text-secondary); }
pre { font-family: var(--font-mono); font-size: 12px; background: var(--color-bg-tertiary); border: 1px solid var(--color-border); border-radius: var(--radius-sm); padding: 8px 10px; margin: 0; max-height: 340px; overflow: auto; white-space: pre-wrap; overflow-wrap: anywhere; color: var(--color-text-secondary); }
.btn { font: inherit; font-size: 13px; padding: 7px 14px; border: 1px solid var(--color-border-light); border-radius: var(--radius-sm); background: var(--color-surface); color: var(--color-text); cursor: pointer; }
.btn:hover { background: var(--color-surface-hover); }
.btn-primary { background: var(--color-primary); border-color: var(--color-primary); color: #fff; }
.btn-primary:hover { background: var(--color-primary-hover); }
@media (max-width: 720px) {
  .header, .banner, .strip { padding-left: 14px; padding-right: 14px; }
  .actions { margin-left: 0; }
}
</style>
</head>
<body>
<div class="shell">
<header class="header">
<div class="logo">HFDesk</div>
`)
	if state.State != "" {
		page.WriteString("<div class=\"status\"><span class=\"dot\" aria-hidden=\"true\"></span><h1>" +
			html.EscapeString(state.State) + "</h1></div>\n")
	}
	page.WriteString("<div class=\"actions\">")
	// The step of the state leads the header, and it is the ONE primary
	// button of that header: the open of the log file takes the
	// secondary place beside it, and keeps the primary place it has
	// always had when the state offers no step.
	openClass := "btn btn-primary"
	switch state.ServiceAction {
	case logsServiceActionStart:
		openClass = "btn"
		page.WriteString("<button class=\"btn btn-primary\" id=\"start-service\" type=\"button\">Start service</button>")
	case logsServiceActionUpdate:
		openClass = "btn"
		page.WriteString("<button class=\"btn btn-primary\" id=\"update-service\" type=\"button\">Update service</button>")
	case logsServiceActionInstall:
		openClass = "btn"
		page.WriteString("<button class=\"btn btn-primary\" id=\"install-service\" type=\"button\">Install service...</button>")
	}
	if state.LogPath != "" {
		page.WriteString("<button class=\"" + openClass + "\" id=\"open-log-file\" type=\"button\">Open the log file</button>")
	}
	page.WriteString("<button class=\"btn\" id=\"try-again\" type=\"button\">Try again</button>")
	page.WriteString("</div>\n</header>\n")

	switch {
	case state.Address != "":
		page.WriteString("<div class=\"banner\" role=\"status\">The web UI at <code>" +
			html.EscapeString(state.Address) + "</code> does not answer.</div>\n")
	case state.Note != "":
		page.WriteString("<div class=\"banner\" role=\"status\">" + html.EscapeString(state.Note) + "</div>\n")
	}

	page.WriteString("<section class=\"strip\">\n<div class=\"stat\"><div class=\"stat-label\">Log file</div>")
	if state.LogPath != "" {
		page.WriteString("<code>" + html.EscapeString(state.LogPath) + "</code>")
	} else {
		page.WriteString("<span class=\"stat-empty\">The log file is not known.</span>")
	}
	page.WriteString("</div>\n</section>\n")

	section := state.Log
	if section.Path != "" || section.Missing || section.ReadError != "" || section.Tail != "" {
		// The service-log section speaks only when the model carries
		// a source: one heading and exactly one of the honest shapes
		// of that file — the bounded tail ending with the newest
		// line, the fact that the file is not there yet, or the
		// one-line reason it could not be read.
		page.WriteString("<section class=\"strip\">\n<div class=\"stat\"><div class=\"stat-label\">Service log</div>")
		switch {
		case section.Missing:
			page.WriteString("<span class=\"stat-empty\">The service log file has not been created yet.</span>")
		case section.ReadError != "":
			reason := html.EscapeString(strings.TrimSuffix(section.ReadError, "."))
			page.WriteString("<span class=\"stat-empty\">The service log file could not be read: " + reason + ".</span>")
		case section.Tail != "":
			page.WriteString("<pre id=\"service-log-tail\">" + html.EscapeString(section.Tail) + "</pre>")
		}
		page.WriteString("</div>\n</section>\n")
	}
	page.WriteString("</div>\n")
	page.WriteString(logsFallbackScript())
	page.WriteString("</body>\n</html>\n")
	return page.String()
}
