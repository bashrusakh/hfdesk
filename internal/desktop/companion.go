// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/internal/assets"
	"github.com/bashrusakh/hfdesk/internal/service"
	"github.com/bashrusakh/hfdesk/internal/serviceview"
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
// that size renders. The document states both bounds in the meta of its log
// panel, so the view never presents a bounded tail as the whole file.
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

// companionVersion is the build version the fallback document of the Logs
// window states in its header. The desktop host assigns it once at startup
// from its Options; a build without a version leaves it empty and the header
// states the honest dash.
var companionVersion string

// logsFallbackState is the model of the fallback document of the Logs
// window — the document the window shows when the registered address does
// not answer (or names none), in the frame of the runtime page the live
// window would have loaded.
//
// The document is self-contained and reaches out of nowhere: it fetches
// nothing and listens to nothing. It inlines the very stylesheets and the
// very units of the live page, so the fallback wears the same frame instead
// of a second one, and its one script only renders the records Go read and
// forwards the fixed host-bound actions.
type logsFallbackState struct {
	// Version is the build version shown in the header. Empty states the
	// honest dash.
	Version string
	// Address is the registered web-UI address that did not answer,
	// stated as a fact (never linked — a link to a dead address would
	// be a dead path). Empty when the registration names none.
	Address string
	// Note is the honest one-line reason there is no address to state,
	// used when Address is empty. Empty when Address is set.
	Note string
	// Service is the ONE table of the service state: its line, its tone
	// and the actions it proves. The unknown class carries no control at
	// all — a read that proved nothing may not invent one.
	Service serviceview.Snapshot
	// LogPath is the path of the service log file — the very file the
	// open action opens, so the path and the action always name one
	// file. Empty when none could be resolved.
	LogPath string
	// Log is the service-log section: the bounded tail of that same log
	// file, or the honest state of a file that does not exist yet or
	// could not be read.
	Log serviceLogSection
}

// fallbackLogRecord is one record of the tail as the document's wiring
// renders it. A line the service wrote is the wire record of the runtime
// log; a line it did not write — a rotated banner, a panic dump — keeps its
// own text under Raw and is never parsed into fields it does not have.
type fallbackLogRecord struct {
	Seq       uint64         `json:"seq,omitempty"`
	Time      string         `json:"time,omitempty"`
	Level     string         `json:"level,omitempty"`
	Component string         `json:"component,omitempty"`
	Message   string         `json:"message,omitempty"`
	Attrs     map[string]any `json:"attrs,omitempty"`
	// Raw is the text of a line that is not a record. It is a pointer so
	// an empty line keeps its discriminator in the JSON: a plain string
	// with omitempty would drop the key and the view would mistake the
	// line for a parsed record with no fields.
	Raw *string `json:"raw,omitempty"`
}

// logsFallbackScriptConfig is the data the single inline script of the
// document consumes: the records of the tail and the names the window host
// bound. The names come from the same constants companionWindowActions
// binds, so the page and the host cannot drift apart.
type logsFallbackScriptConfig struct {
	Records       []fallbackLogRecord `json:"records"`
	Actions       map[string]string   `json:"actions"`
	OpenLogFile   string              `json:"openLogFile"`
	OpenLogFolder string              `json:"openLogFolder"`
	TryAgain      string              `json:"tryAgain"`
}

// parseServiceLogTail turns the bounded tail into the records of the
// document. Every line is the wire record of the runtime log when it parses
// as one; any other line — invalid JSON, or JSON that is not a record —
// keeps its text verbatim under Raw, so the view can never drop it or
// invent fields for it.
func parseServiceLogTail(tail string) []fallbackLogRecord {
	records := make([]fallbackLogRecord, 0)
	if tail == "" {
		return records
	}
	for _, line := range strings.Split(tail, "\n") {
		var record fallbackLogRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil || record.Seq == 0 {
			raw := line
			record = fallbackLogRecord{Raw: &raw}
		}
		records = append(records, record)
	}
	return records
}

// logsFallbackScriptConfigFor builds the data of the inline script for one
// document.
func logsFallbackScriptConfigFor(state logsFallbackState) logsFallbackScriptConfig {
	return logsFallbackScriptConfig{
		Records: parseServiceLogTail(state.Log.Tail),
		Actions: map[string]string{
			"install": logsInstallServiceAction,
			"start":   logsStartServiceAction,
			"stop":    logsStopServiceAction,
			"update":  logsUpdateServiceAction,
			"delete":  logsDeleteServiceAction,
		},
		OpenLogFile:   openLogFileAction,
		OpenLogFolder: openLogFolderAction,
		TryAgain:      logsTryAgainAction,
	}
}

// scriptJSON encodes data for the one inline script of the document. The
// encoding escapes <, >, & and the JavaScript line separators, so a value
// from the machine — a log message that names </script>, an address with an
// ampersand — can never close the element or change the parse of the
// script.
func scriptJSON(data any) string {
	encoded, err := json.Marshal(data)
	if err != nil {
		return "null"
	}
	return strings.NewReplacer(
		"&", `\u0026`,
		"<", `\u003c`,
		">", `\u003e`,
		"\u2028", `\u2028`,
		"\u2029", `\u2029`,
	).Replace(string(encoded))
}

// fallbackAsset reads one shipped asset of the live page. The embedded tree
// is a build-time dependency of this document: a read that fails leaves the
// frame without that part instead of taking the whole fallback down.
func fallbackAsset(name string) string {
	data, err := fs.ReadFile(assets.StaticFS(), name)
	if err != nil {
		return ""
	}
	return string(data)
}

// logsFallbackWiring is the local wiring of the fallback document: it
// renders the records Go read through the shipped renderer, filters them,
// copies them, forwards the open actions and the service items to the
// functions the window host bound, and scrolls the view to the end of the
// tail. It fetches nothing, listens to nothing and carries no data out of
// the document; every value reaches the DOM through textContent.
const logsFallbackWiring = `(function () {
  'use strict';

  var config = window.HFDeskFallbackConfig;
  var logView = window.HFDeskLogView;
  var body = document.getElementById('logBody');
  var emptyState = document.getElementById('emptyState');
  var emptyTitle = document.getElementById('emptyTitle');
  var emptyText = document.getElementById('emptyText');
  var logPanel = document.getElementById('logView');
  var levelFilter = document.getElementById('levelFilter');
  var searchInput = document.getElementById('searchInput');
  var copyButton = document.getElementById('copyButton');
  var view = { level: 'all', searchLower: '' };
  var items = [];

  for (var i = 0; i < config.records.length; i++) {
    var record = config.records[i];
    if (typeof record.raw === 'string') {
      items.push({ raw: record.raw });
      continue;
    }
    var normalized = logView.normalizeRecord(record);
    if (normalized) items.push({ record: normalized });
  }

  function matches(item) {
    if (item.raw !== undefined) {
      if (!view.searchLower) return true;
      return item.raw.toLowerCase().indexOf(view.searchLower) !== -1;
    }
    return logView.matches(item.record, view);
  }

  function render() {
    var visible = [];
    for (var i = 0; i < items.length; i++) {
      if (matches(items[i])) visible.push(items[i]);
    }

    body.replaceChildren();
    for (var j = 0; j < visible.length; j++) {
      body.appendChild(visible[j].raw !== undefined
        ? logView.buildRawRow(visible[j].raw)
        : logView.buildRow(visible[j].record));
    }

    if (items.length === 0) {
      emptyState.hidden = false;
    } else if (visible.length === 0) {
      emptyState.hidden = false;
      emptyTitle.textContent = 'No log records match';
      emptyText.textContent = 'The level filter and the search hide every record of the tail.';
    } else {
      emptyState.hidden = true;
    }

    logPanel.scrollTop = logPanel.scrollHeight;
  }

  levelFilter.addEventListener('change', function () {
    view.level = levelFilter.value;
    render();
  });

  searchInput.addEventListener('input', function () {
    view.searchLower = searchInput.value.toLowerCase();
    render();
  });

  copyButton.addEventListener('click', function () {
    var lines = [];
    for (var i = 0; i < items.length; i++) {
      if (!matches(items[i])) continue;
      lines.push(items[i].raw !== undefined ? items[i].raw : logView.formatLine(items[i].record));
    }
    if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
      navigator.clipboard.writeText(lines.join('\n')).catch(function () {});
    }
  });

  function bindOpen(id, name) {
    var button = document.getElementById(id);
    if (!button) return;
    button.addEventListener('click', function () {
      var owner = window[name];
      if (typeof owner === 'function') owner();
    });
  }

  bindOpen('openLogsButton', config.openLogFolder);
  bindOpen('openLogFileButton', config.openLogFile);
  bindOpen('try-again', config.tryAgain);

  window.HFDeskServiceControl.wire({
    trigger: document.getElementById('serviceTrigger'),
    menu: document.getElementById('serviceMenu'),
    onAction: function (name) {
      var owner = window[config.actions[name]];
      if (typeof owner !== 'function') return;
      return owner();
    }
  });

  render();
})();
`

// logsFallbackScript renders the only script of the document: the shipped
// renderer, the shipped menu control and the local wiring, followed by the
// data of this document. The data is encoded with scriptJSON, so no value
// of the machine can close the element.
func logsFallbackScript(state logsFallbackState) string {
	var script strings.Builder
	script.WriteString("<script>\n")
	script.WriteString(fallbackAsset("js/logview.js"))
	script.WriteString("\n")
	script.WriteString(fallbackAsset("js/servicecontrol.js"))
	script.WriteString("\nwindow.HFDeskFallbackConfig = ")
	script.WriteString(scriptJSON(logsFallbackScriptConfigFor(state)))
	script.WriteString(";\n")
	script.WriteString(logsFallbackWiring)
	script.WriteString("</script>\n")
	return script.String()
}

// logsFallbackTone is the class of the status dot of the one table's tone.
func logsFallbackTone(tone string) string {
	if tone == "" {
		return "is-muted"
	}
	return "is-" + tone
}

// logsFallbackServiceControl writes the service control of the header —
// the very markup of runtime.html, with the items of this state. The
// control is the state line and the actions that state proves; a read that
// proved nothing renders no control at all.
func logsFallbackServiceControl(page *strings.Builder, snapshot serviceview.Snapshot) {
	actions := snapshot.Actions
	shown := snapshot.Class != serviceview.ClassUnknown &&
		(actions.Install.Visible || actions.Start.Visible || actions.Stop.Visible ||
			actions.Update.Visible || actions.Delete.Visible)

	label := snapshot.Label
	if label == "" {
		label = "Service &mdash;"
	} else {
		label = html.EscapeString(label)
	}

	hidden := ""
	if !shown {
		hidden = " hidden"
	}

	page.WriteString(`<div class="runtime-service" id="serviceControl"` + hidden + `>
<button class="btn btn-outline btn-sm runtime-service-trigger" id="serviceTrigger" type="button"
        aria-haspopup="menu" aria-expanded="false" aria-controls="serviceMenu"
        title="The service registration of this machine">
<span class="status-indicator runtime-service-dot ` + logsFallbackTone(snapshot.Tone) + `" id="serviceDot"></span>
<span class="runtime-service-label" id="serviceLabel">` + label + `</span>
<svg class="runtime-service-chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor"
     stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="12" height="12" aria-hidden="true">
  <path d="m6 9 6 6 6-6"/>
</svg>
</button>
<div class="runtime-service-menu" id="serviceMenu" role="menu" aria-labelledby="serviceTrigger" hidden>
`)
	page.WriteString(fallbackServiceItem("serviceInstallItem", "install", "Install service&hellip;", "runtime-service-item", actions.Install))
	page.WriteString(fallbackServiceItem("serviceStartItem", "start", "Start service", "runtime-service-item", actions.Start))
	page.WriteString(fallbackServiceItem("serviceStopItem", "stop", "Stop service", "runtime-service-item", actions.Stop))
	page.WriteString(fallbackServiceItem("serviceUpdateItem", "update", "Update service", "runtime-service-item", actions.Update))

	separator := true
	if !actions.Delete.Visible || !(actions.Install.Visible || actions.Start.Visible ||
		actions.Stop.Visible || actions.Update.Visible) {
		separator = false
	}
	if separator {
		page.WriteString(`<div class="runtime-service-separator" id="serviceSeparator" role="separator"></div>` + "\n")
	} else {
		page.WriteString(`<div class="runtime-service-separator" id="serviceSeparator" role="separator" hidden></div>` + "\n")
	}
	page.WriteString(fallbackServiceItem("serviceDeleteItem", "delete", "Delete service&hellip;", "runtime-service-item runtime-service-item-danger", actions.Delete))

	page.WriteString(`</div>
<span class="badge badge-secondary runtime-service-chip" id="serviceChip" hidden>
  <span class="status-indicator runtime-service-dot is-muted" id="serviceChipDot"></span>
  <span id="serviceChipLabel">` + label + `</span>
</span>
</div>
`)
}

// fallbackServiceItem writes one item of the service menu. The action map of
// the one table decides both the presence and the availability: a hidden
// item is never clickable behind the scenes.
func fallbackServiceItem(id, action, label, class string, act serviceview.Action) string {
	attrs := ""
	if !act.Visible {
		attrs += " hidden"
	}
	if !act.Enabled {
		attrs += " disabled"
	}
	return fmt.Sprintf(
		`<button class="%s" id="%s" type="button" role="menuitem" data-service-action="%s"%s>%s</button>`+"\n",
		class, id, action, attrs, label)
}

// logsFallbackEmptyState answers the honest empty state of the panel: the
// missing file, the unreadable file, the log that names no path and the
// empty file. A readable tail with records shows no empty state.
func logsFallbackEmptyState(section serviceLogSection) (title, text string, show bool) {
	switch {
	case section.Path == "":
		return "The log file is not known.", "The registration names no log directory, so there is no service log to show.", true
	case section.Missing:
		return "The service log file has not been created yet.", "The service writes the file when it runs.", true
	case section.ReadError != "":
		reason := strings.TrimSuffix(section.ReadError, ".")
		return "The service log file could not be read: " + reason + ".", "The reason above is the answer of the file system.", true
	case section.Tail == "":
		return "The service log is empty.", "The service has not written a record yet.", true
	}
	return "", "", false
}

// logsFallbackPage renders the model into the fallback document of the Logs
// window.
//
// The document wears the frame of the runtime page — the same header, the
// same fact banner, the same status strip and the same log panel — from the
// very stylesheets and the very units of that page, inlined because a
// fallback document has no server to load them from. It states the address
// that did not answer (or the honest reason there is none), the service
// control of the one table with the actions of its state, the path of the
// service log with the actions that open it, and the bounded tail of that
// same log as the table of the live page. Everything the model carries is
// escaped, so a value from the machine can never become markup.
func logsFallbackPage(state logsFallbackState) string {
	records := parseServiceLogTail(state.Log.Tail)
	emptyTitle, emptyText, emptyShown := logsFallbackEmptyState(state.Log)
	emptyHidden := " hidden"
	if emptyShown {
		emptyHidden = ""
	}

	logFile := "&mdash;"
	if state.LogPath != "" {
		logFile = html.EscapeString(state.LogPath)
	}
	openDisabled := " disabled"
	if state.LogPath != "" {
		openDisabled = ""
	}
	server := "&mdash;"
	if state.Address != "" {
		server = html.EscapeString(state.Address)
	}
	version := strings.TrimSpace(state.Version)
	if version == "" {
		version = "&mdash;"
	} else {
		version = html.EscapeString(version)
	}

	var page strings.Builder
	page.WriteString(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>HFDesk - Logs</title>
<style>
`)
	page.WriteString(fallbackAsset("css/style.css"))
	page.WriteString("\n")
	page.WriteString(fallbackAsset("css/runtime.css"))
	page.WriteString(`
</style>
</head>
<body>
<div class="runtime-shell">

<header class="runtime-header">
<div class="logo">
<svg class="logo-icon" viewBox="0 0 512 512" aria-hidden="true">
  <rect width="512" height="512" rx="108" fill="#12121a"/>
  <rect x="5" y="5" width="502" height="502" rx="103" fill="none" stroke="#2a2a3a" stroke-width="4" opacity="0.6"/>
  <rect x="112" y="96" width="88" height="320" rx="20" fill="#6366f1"/>
  <rect x="312" y="96" width="88" height="320" rx="20" fill="#6366f1"/>
  <rect x="112" y="216" width="288" height="80" rx="18" fill="#6366f1"/>
</svg>
<span class="logo-text">HFDesk</span>
</div>
<div class="runtime-header-status">
<span class="connection-status">
  <span class="status-indicator runtime-status-indicator is-error" id="statusDot"></span>
  <span id="statusLabel">Offline</span>
</span>
<span class="runtime-version" id="versionLabel">Version `)
	page.WriteString(version)
	page.WriteString(`</span>
`)
	logsFallbackServiceControl(&page, state.Service)
	page.WriteString(`</div>
</header>

<div class="runtime-error" id="errorBanner" role="status">
  <strong id="errorTitle">Cannot reach the HFDesk server</strong>
  <span id="errorText">`)
	if state.Address != "" {
		page.WriteString("The web UI at <code>" + html.EscapeString(state.Address) + "</code> does not answer.")
	} else {
		note := state.Note
		if note == "" {
			note = "The address of the web UI is not known."
		}
		page.WriteString(html.EscapeString(note))
	}
	page.WriteString(`</span>
  <button class="btn btn-outline btn-sm" id="try-again" type="button">Try again</button>
</div>

<section class="runtime-strip" aria-label="Runtime status">
  <div class="runtime-stat">
    <span class="runtime-stat-label">Server</span>
    <span class="runtime-stat-value"><code class="runtime-url" id="serverUrl">` + server + `</code></span>
  </div>
  <div class="runtime-stat">
    <span class="runtime-stat-label">Active jobs</span>
    <span class="runtime-stat-value"><span id="activeJobs">&mdash;</span></span>
  </div>
  <div class="runtime-stat">
    <span class="runtime-stat-label">Log file</span>
    <span class="runtime-stat-value"><span class="config-chip"><code id="logFile">` + logFile + `</code></span></span>
  </div>
</section>

<section class="runtime-log-panel">
  <div class="runtime-log-toolbar">
    <div class="runtime-toolbar-group">
      <label class="runtime-toolbar-label" for="levelFilter">Level</label>
      <select class="search-select" id="levelFilter">
        <option value="all">All levels</option>
        <option value="debug">Debug</option>
        <option value="info">Info</option>
        <option value="warn">Warn</option>
        <option value="error">Error</option>
      </select>
      <div class="search-input-wrap runtime-search">
        <svg class="search-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="18" height="18" aria-hidden="true">
          <circle cx="11" cy="11" r="8"/><path d="m21 21-4.35-4.35"/>
        </svg>
        <input class="search-input" id="searchInput" type="text" placeholder="Search logs&hellip;" autocomplete="off" spellcheck="false">
      </div>
    </div>

    <div class="runtime-toolbar-group">
      <button class="btn btn-secondary btn-sm" id="copyButton" type="button"
              title="Copy the rows that are currently visible to the clipboard">Copy</button>
      <button class="btn btn-secondary btn-sm" id="openLogsButton" type="button"
              title="Open the folder of the service log file"` + openDisabled + `>Open log folder</button>
      <button class="btn btn-secondary btn-sm" id="openLogFileButton" type="button"
              title="Open the service log file in the default program"` + openDisabled + `>Open log file</button>
    </div>
  </div>

  <div class="runtime-log-meta">
    <span class="connection-status">
      <span class="status-indicator runtime-status-indicator is-error"></span>
      <span id="streamLabel">Offline &mdash; last `)
	page.WriteString(strconv.Itoa(len(records)))
	page.WriteString(` lines</span>
    </span>
    <span id="recordCount">at most `)
	page.WriteString(strconv.Itoa(serviceLogTailLines))
	page.WriteString(` lines, `)
	page.WriteString(strconv.Itoa(serviceLogTailBytes / 1024))
	page.WriteString(` KiB</span>
  </div>

  <div class="runtime-log-view" id="logView" role="log" aria-live="off" tabindex="0">
    <div class="runtime-log-head" aria-hidden="true">
      <span class="runtime-time">Time</span>
      <span class="runtime-level">Level</span>
      <span class="runtime-component">Component</span>
      <span class="runtime-message">Message</span>
      <span class="runtime-attrs">Attributes</span>
    </div>
    <div class="runtime-log-body" id="logBody"></div>
    <div class="empty-state" id="emptyState"` + emptyHidden + `>
      <h3 id="emptyTitle">` + html.EscapeString(emptyTitle) + `</h3>
      <p id="emptyText">` + html.EscapeString(emptyText) + `</p>
    </div>
  </div>
</section>

</div>
`)
	page.WriteString(logsFallbackScript(state))
	page.WriteString(`</body>
</html>
`)
	return page.String()
}
