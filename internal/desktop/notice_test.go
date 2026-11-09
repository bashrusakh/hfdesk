// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"strings"
	"testing"
)

// TestAlreadyRunningOpenFailedTextDoesNotClaimTheWebUIWasOpened proves that
// the notice of a second launch that could not open the web UI never claims
// that the web UI was opened.
//
// The second launch has no log file of its own: the log file belongs to the
// launch that runs the host. A record of the failure cannot correct a
// message the user already read, so the message itself has to be true.
func TestAlreadyRunningOpenFailedTextDoesNotClaimTheWebUIWasOpened(t *testing.T) {
	text := alreadyRunningOpenFailedText("http://127.0.0.1:8080",
		errors.New("the browser could not be started"))

	if strings.Contains(text, "is opened in your default browser") {
		t.Errorf("text = %q, want it to not claim that the web UI was opened", text)
	}
}

// TestAlreadyRunningOpenFailedTextNamesTheFailureAndTheWayOut proves that
// the notice names what could not be opened, the reason of the failure and
// the way to reach the running instance anyway.
func TestAlreadyRunningOpenFailedTextNamesTheFailureAndTheWayOut(t *testing.T) {
	text := alreadyRunningOpenFailedText("http://127.0.0.1:8080",
		errors.New("the browser could not be started"))

	for _, want := range []string{
		"could not open the web UI",
		"http://127.0.0.1:8080",
		"the browser could not be started",
		"notification area",
		"Runtime / Logs window",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to mention %q", text, want)
		}
	}
}

// TestAlreadyRunningTextClaimsTheWebUIWasOpened proves that the notice of a
// second launch that opened the web UI says so, so the notice stays true in
// both directions.
func TestAlreadyRunningTextClaimsTheWebUIWasOpened(t *testing.T) {
	text := alreadyRunningText("http://127.0.0.1:8080")

	if !strings.Contains(text, "is opened in your default browser") {
		t.Errorf("text = %q, want it to tell the user that the web UI was opened", text)
	}
	if !strings.Contains(text, "http://127.0.0.1:8080") {
		t.Errorf("text = %q, want it to name the URL of the running instance", text)
	}
}

// TestAlreadyRunningUnreachableTextClaimsNoOpenedWebUI proves that the
// notice of a running instance that did not answer never claims that a
// browser was opened, and that it names the way to reach the running
// instance instead.
func TestAlreadyRunningUnreachableTextClaimsNoOpenedWebUI(t *testing.T) {
	text := alreadyRunningUnreachableText()

	if strings.Contains(text, "opened in your default browser") {
		t.Errorf("text = %q, want it to not claim that the web UI was opened", text)
	}
	if !strings.Contains(text, "notification area") {
		t.Errorf("text = %q, want it to name the notification area icon", text)
	}
}

// TestBrowserOpenFailedTextNamesTheFailure proves that the notice of a
// failed browser launch names the URL and the reason of the failure.
//
// Every caller of the browser reports the failure the same way, so the user
// always learns that the web UI was not opened, and why it was not opened.
func TestBrowserOpenFailedTextNamesTheFailure(t *testing.T) {
	text := browserOpenFailedText("http://127.0.0.1:8080",
		errors.New("the browser could not be started"))

	for _, want := range []string{
		"could not open the web UI",
		"http://127.0.0.1:8080",
		"the browser could not be started",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to mention %q", text, want)
		}
	}
	if strings.Contains(text, "is opened in your default browser") {
		t.Errorf("text = %q, want it to not claim that the web UI was opened", text)
	}
}

// TestAlreadyRunningOpenFailedTextUsesTheBrowserNotice proves that the
// notice of a second launch that could not open the web UI carries the same
// browser failure as every other caller of the browser.
//
// The second launch adds the way to reach the running instance, but it
// reports the failure of the browser the same way.
func TestAlreadyRunningOpenFailedTextUsesTheBrowserNotice(t *testing.T) {
	text := alreadyRunningOpenFailedText("http://127.0.0.1:8080",
		errors.New("the browser could not be started"))

	if !strings.Contains(text, browserOpenFailedText("http://127.0.0.1:8080",
		errors.New("the browser could not be started"))) {
		t.Errorf("text = %q, want it to carry the notice of the browser failure", text)
	}
}
