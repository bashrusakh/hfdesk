// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestSecondLaunchReportsAFailedBrowserOpen proves that a second launch
// that cannot open the web UI of the running instance tells the user.
//
// The second launch has no log file of its own: the log file belongs to the
// launch that runs the host, so a record of the failure would reach the
// standard logger and vanish in a GUI process. The failure therefore has to
// reach the user through a message box, and the message must not claim that
// the web UI was opened.
//
// The test drives the production path: the message box is recorded instead
// of shown, because a message box blocks a process without an interactive
// window station, and the browser is replaced by one that fails.
func TestSecondLaunchReportsAFailedBrowserOpen(t *testing.T) {
	useTemporaryStateDir(t)

	// The running instance answers the health request, so the second
	// launch tries to open its web UI.
	running := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer running.Close()

	if err := publishState(publishedStatePath(), running.URL); err != nil {
		t.Fatalf("publish the state of the running instance: %v", err)
	}

	// The browser cannot be opened.
	previousBrowser := openBrowser
	openBrowser = func(string) error { return errors.New("the browser could not be started") }
	defer func() { openBrowser = previousBrowser }()

	// The message box is recorded instead of shown.
	var messages []shownMessage
	previousMessage := showMessage
	showMessage = func(caption, text string, icon uint32) {
		messages = append(messages, shownMessage{caption: caption, text: text, icon: icon})
	}
	defer func() { showMessage = previousMessage }()

	// The record of the failure is captured. The second launch writes no
	// log file of its own, but the record still has to be there for the
	// run that owns the log file.
	records := &recordedMessages{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(records))
	defer slog.SetDefault(previousLogger)

	reportAlreadyRunning()

	// The user is told about the failure.
	if len(messages) != 1 {
		t.Fatalf("the second launch showed %d message boxes, want 1: %v", len(messages), messages)
	}
	if !strings.Contains(messages[0].text, "could not open the web UI") {
		t.Errorf("the message of the second launch is %q, want the notice that the web UI could not be opened",
			messages[0].text)
	}
	if strings.Contains(messages[0].text, "is opened in your default browser") {
		t.Errorf("the message of the second launch is %q, want it to not claim that the web UI was opened",
			messages[0].text)
	}
	if !strings.Contains(messages[0].text, "notification area") {
		t.Errorf("the message of the second launch is %q, want it to name the notification area icon",
			messages[0].text)
	}
	if messages[0].icon != windows.MB_ICONERROR {
		t.Errorf("the icon of the message of the second launch is %d, want the error icon %d",
			messages[0].icon, windows.MB_ICONERROR)
	}

	// The failure is recorded as well: the record is not the only place
	// where the failure appears, but it stays.
	if !records.contains("could not open the web UI of the running instance") {
		t.Errorf("the records of the second launch are %v, want the record of the failed open",
			records.messages)
	}
}

// TestSecondLaunchOpensTheWebUIOfTheRunningInstance proves that a second
// launch opens the web UI of the running instance and tells the user that it
// did so.
//
// The test drives the production path with a browser that records the URL
// it was asked to open, so the URL of the running instance and the notice
// about it cannot drift apart.
func TestSecondLaunchOpensTheWebUIOfTheRunningInstance(t *testing.T) {
	useTemporaryStateDir(t)

	// The running instance answers the health request, so the second
	// launch opens its web UI.
	running := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer running.Close()

	if err := publishState(publishedStatePath(), running.URL); err != nil {
		t.Fatalf("publish the state of the running instance: %v", err)
	}

	// The browser records the URL it opens.
	var opened []string
	previousBrowser := openBrowser
	openBrowser = func(url string) error {
		opened = append(opened, url)
		return nil
	}
	defer func() { openBrowser = previousBrowser }()

	// The message box is recorded instead of shown.
	var messages []shownMessage
	previousMessage := showMessage
	showMessage = func(caption, text string, icon uint32) {
		messages = append(messages, shownMessage{caption: caption, text: text, icon: icon})
	}
	defer func() { showMessage = previousMessage }()

	reportAlreadyRunning()

	// The web UI of the running instance was opened.
	if len(opened) != 1 || opened[0] != running.URL {
		t.Fatalf("the browser opened %v, want the URL of the running instance %q", opened, running.URL)
	}

	// The user is told that the web UI is opened.
	if len(messages) != 1 {
		t.Fatalf("the second launch showed %d message boxes, want 1: %v", len(messages), messages)
	}
	if !strings.Contains(messages[0].text, "is opened in your default browser") {
		t.Errorf("the message of the second launch is %q, want it to tell the user that the web UI was opened",
			messages[0].text)
	}
	if !strings.Contains(messages[0].text, running.URL) {
		t.Errorf("the message of the second launch is %q, want it to name the URL of the running instance",
			messages[0].text)
	}
}

// shownMessage is a message box that a test recorded instead of showing.
type shownMessage struct {
	caption string
	text    string
	icon    uint32
}

// recordedMessages collects the messages of the log records.
//
// The desktop host writes its records through the default logger of the
// standard library, so a test that replaces the default logger sees the
// records of the host.
type recordedMessages struct {
	messages []string
}

// Enabled accepts every level.
func (h *recordedMessages) Enabled(context.Context, slog.Level) bool { return true }

// Handle records the message of the record.
func (h *recordedMessages) Handle(_ context.Context, record slog.Record) error {
	h.messages = append(h.messages, record.Message)
	return nil
}

// WithAttrs keeps the same recorder.
func (h *recordedMessages) WithAttrs([]slog.Attr) slog.Handler { return h }

// WithGroup keeps the same recorder.
func (h *recordedMessages) WithGroup(string) slog.Handler { return h }

// contains reports whether the recorder saw the given message.
func (h *recordedMessages) contains(message string) bool {
	for _, recorded := range h.messages {
		if recorded == message {
			return true
		}
	}
	return false
}
