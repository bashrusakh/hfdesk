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
	return text
}
