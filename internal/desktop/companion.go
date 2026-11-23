// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
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
// desktopWindowActions) and the pages call them: the live runtime page of
// the server calls the two open actions, the fallback document calls the
// open action and the retry. Every name has ONE source of truth — these
// constants — and the pages reference them through the substituted
// template or through the pinned literals of runtime.js, so the page and
// the host binding cannot drift apart into a dead button.
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
// The report is the state (another HFDesk process of this session still
// holds the session — usually the elevated helper that is finishing the
// service action this child was relaunched for) and the one action that
// resolves it (start the desktop app again once that process finished). It
// carries no log-path line: the
// lock is taken before the log file is opened, so this launch has no log
// file of its own yet — the established rule of the lock reports (a
// launch that ends here names no log, because there is none).
func companionRelaunchWaitTimeoutText() string {
	return "HFDesk could not take over the desktop session: another HFDesk process of this Windows session still holds the single-instance object.\n\n" +
		"That is usually the elevated helper finishing the service action. Start the desktop app again in a moment."
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
// fallback document otherwise.
//
// The probe runs on every open, so the window shows the actual state at
// the moment the user asks for it: a service that stopped since the last
// open meets the honest fallback instead of a hanging navigation, and a
// service that came back meets the live log instead of a stale state
// page. An empty address skips the probe — there is nothing to ask.
func resolveLogsWindow(resolve func() string, fallback func() string) (string, string) {
	if url := resolve(); url != "" && runtimePageAnswers(url) {
		return url, ""
	}
	return "", fallback()
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
// host-bound actions to the functions the window bound.
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
}

// The placeholders of the script template the page builder replaces with
// the bound action names.
//
// The action names have ONE source of truth: the openLogFileAction and
// logsTryAgainAction constants, which are also the keys the host binds
// (companionWindowActions). The template never names them — the names are
// substituted while the page is generated — so the page and the host
// binding cannot drift apart, and a literal name that reaches the page
// from anywhere else fails the test that pins the rendered document.
const (
	logsFallbackOpenPlaceholder = "{{open}}"
	logsFallbackTryPlaceholder  = "{{try}}"
)

// logsFallbackScriptTemplate is the only script of the fallback document,
// and it performs exactly one local operation with no channel of any kind:
// it forwards the two fixed action names to the functions the window
// bound on the host side — open the log file, and retry the live page.
//
// Nothing here fetches, listens or carries data out of the document: the
// paths never travel through the page (the host owns them), and the
// invariant of the companion — nothing of it listens — is unchanged.
// Both wirings guard their element, so a document without one of the
// buttons runs the script as a no-op.
const logsFallbackScriptTemplate = `<script>
(function () {
	var open = document.getElementById("open-log-file");
	if (open && window.` + logsFallbackOpenPlaceholder + `) {
		open.addEventListener("click", function () { window.` + logsFallbackOpenPlaceholder + `(); });
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
	return strings.ReplaceAll(script, logsFallbackTryPlaceholder, logsTryAgainAction)
}

// logsFallbackPage renders the model into the fallback document of the
// Logs window.
//
// The document wears the visual language of the runtime page — the same
// header with the status line, the same fact strip, the same design token
// values — inlined, because a fallback document has no server to load a
// stylesheet from; that is exactly why it is a document and not a page.
// It states the honest state of the service, the address that did not
// answer (or the honest reason there is none), the path of the service log
// file with the action that opens it, and the one action that retries the
// live page. Everything the model carries is escaped, so a value from the
// machine can never become markup.
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
	if state.LogPath != "" {
		page.WriteString("<button class=\"btn btn-primary\" id=\"open-log-file\" type=\"button\">Open the log file</button>")
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
	page.WriteString("</div>\n</section>\n</div>\n")
	page.WriteString(logsFallbackScript())
	page.WriteString("</body>\n</html>\n")
	return page.String()
}
