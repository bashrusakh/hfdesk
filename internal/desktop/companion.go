// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"fmt"
	"html"
	"strings"
	"time"
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
// not the service currently answers it), and the log file of this launch.
// The service log section is deliberately not part of the model: it waits
// for the user's decision on its source, and the page reserves its place
// without rendering it.
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
}

// companionPage renders the model into the page of the companion window.
//
// Everything the model carries is escaped, so a value from the machine
// can never become markup. The page has no script and fetches nothing:
// it is a document, not a client of a listener — the invariant of the
// companion is that nothing of it listens, and the page honors that by
// having no channel to a server of its own. The comment where the
// service log section will render keeps the interface ready for it.
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
	page.WriteString("<p class=\"label\">Service log</p>\n")
	page.WriteString("<!-- The service log section renders here once its source is decided: nothing of this page fetches from a listener of its own. -->\n")
	if state.LogPath != "" {
		page.WriteString("<p>The log file is at <code>" + html.EscapeString(state.LogPath) + "</code>.</p>\n")
	}
	page.WriteString("</main>\n</body>\n</html>\n")
	return page.String()
}
