// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import "fmt"

// The captions of the message boxes of a launch that found a running
// instance.
const (
	// alreadyRunningCaption is the caption of the notice that tells the
	// user that HFDesk is already running.
	alreadyRunningCaption = "HFDesk is already running"
)

// logFailureCaption is the caption of the message box that reports a failure
// of the persistent log.
//
// The desktop host has no console: a log file that stopped working is only
// visible to the user in a message box.
const logFailureCaption = "HFDesk - the log file failed"

// browserOpenFailedCaption is the caption of the message box that reports a
// failed browser launch.
//
// Every caller of the browser reports the failure the same way, so the user
// always reads the same notice, whatever asked for the browser.
const browserOpenFailedCaption = "HFDesk could not open the web UI"

// alreadyRunningText returns the text of the notice of a launch that found
// a running instance and opened the web UI of it.
//
// The notice names the URL of the running instance and tells the user that
// the web UI is opened in the default browser.
func alreadyRunningText(url string) string {
	return fmt.Sprintf("HFDesk is already running.\n\n"+
		"The running instance serves %s.\n\n"+
		"The web UI is opened in your default browser.", url)
}

// alreadyRunningUnreachableText returns the text of the notice of a launch
// that found a running instance whose web UI it could not reach.
//
// The running instance did not answer, so there is no URL to open. The
// notice names the way to reach the running instance instead.
func alreadyRunningUnreachableText() string {
	return "HFDesk is already running.\n\n" +
		"Use the icon in the notification area to open the Runtime / Logs window."
}

// alreadyRunningOpenFailedText returns the text of the notice of a launch
// that found a running instance and could not open the web UI of it.
//
// The notice never claims that the web UI was opened: the second launch has
// no log file of its own, because the log file belongs to the launch that
// runs the host, so a claim that is wrong cannot be corrected by a later
// record. The notice names what could not be opened, the reason of the
// failure and the way to reach the running instance anyway.
func alreadyRunningOpenFailedText(url string, openErr error) string {
	return "HFDesk is already running.\n\n" +
		browserOpenFailedText(url, openErr) +
		"\n\nUse the icon in the notification area to open the Runtime / Logs window."
}

// trayStartupFailureText returns the text of the report of a host whose
// notification-area icon could not be created within its budget: without
// the icon the host would be unreachable, so it stops. The record of this
// refusal is written by the caller beside the box, so the log the window
// names really carries it.
func trayStartupFailureText() string {
	return failureWindowText("HFDesk could not create its icon in the notification area.\n\n" +
		"The desktop host needs the icon in the notification area to be reachable, so it stops.")
}

// runtimeWindowShowFailedText returns the text of the report of a Runtime /
// Logs window that could not be shown. The failure is recorded in the log
// before this text is built (the host logs it beside the box), so the log
// path the window names carries the very failure.
func runtimeWindowShowFailedText(err error) string {
	return failureWindowText(fmt.Sprintf("The Runtime / Logs window could not be shown.\n\n%v", err))
}

// runtimeWindowUnavailableText returns the text of the report of a Runtime /
// Logs window that cannot be shown because the WebView2 runtime is missing.
//
// The host keeps running: the server and the tray keep working, and the
// action shows the same message again. The report is a failure window of
// this launch like every other — the state (the window cannot be shown) and
// the one action (install the runtime) beside the standard log-path line,
// in the words the tray companion's mirror of it already carries.
func runtimeWindowUnavailableText(url string) string {
	return failureWindowText("The Microsoft Edge WebView2 runtime is not installed, so the Runtime / Logs window cannot be shown.\n\n" +
		"Install the Evergreen runtime from\nhttps://developer.microsoft.com/microsoft-edge/webview2/\n\n" +
		"HFDesk keeps running in the notification area. The web UI is still available at " + url + ".")
}

// publishStateStartupText returns the report of a launch that could not
// publish its effective URL and therefore stopped without serving.
//
// The server is stopped before this text is shown, so it states the result
// first, then the cause with the error of the publish, and exactly one real
// next action.
func publishStateStartupText(err error) string {
	return fmt.Sprintf("HFDesk is not serving.\n\n"+
		"The desktop host could not publish the effective URL of its local server:\n\n%v\n\n"+
		"Make sure the configuration directory is writable and start HFDesk again.", err)
}

// publishClaimStartupText returns the report of a launch that could not
// publish the claim of its own startup and therefore ended before any
// server existed. The mutation guard of the session reads that claim beside
// the singleton probe, so a launch it cannot see must not serve; the report
// states the result, the cause and the same one next action as every other
// startup failure.
func publishClaimStartupText(err error) string {
	return fmt.Sprintf("HFDesk is not serving.\n\n"+
		"The desktop host could not publish the claim of this launch:\n\n%v\n\n"+
		"Make sure the configuration directory is writable and start HFDesk again.", err)
}

// browserOpenFailedText returns the text of the notice of a failed browser
// launch.
//
// The text names the URL and the reason of the failure, and it never claims
// that the web UI was opened. Every caller of the browser uses it, so the
// user reads the same notice, whatever asked for the browser.
func browserOpenFailedText(url string, openErr error) string {
	text := fmt.Sprintf("HFDesk could not open the web UI in your default browser.\n\n%s", url)
	if openErr != nil {
		text += "\n\n" + openErr.Error()
	}
	// A launch that has no log of its own (the second launch reaches this
	// text before any logging started) appends nothing: runLogPath names
	// the file only when this launch really writes it.
	return failureWindowText(text)
}
