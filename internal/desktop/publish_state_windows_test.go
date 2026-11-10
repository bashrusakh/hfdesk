// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/app"
	"github.com/bashrusakh/hfdesk/internal/service"
)

// TestDesktopHostStopsServingWhenTheStateCannotBePublished drives the real
// startup of the desktop host with a publish that fails.
//
// The serving desktop must fail closed: the mutation guard of this session
// reads the published state next to the singleton probe, so a host that kept
// serving without its state would let `hfdesk service install/start/restart`
// start the service beside it. The launch therefore stops the server through
// the graceful path, reports the failure like every other startup failure and
// leaves no published state behind.
func TestDesktopHostStopsServingWhenTheStateCannotBePublished(t *testing.T) {
	useTemporaryStateDir(t)

	// The message box is recorded instead of shown: a message box blocks a
	// process without an interactive window station.
	var messages []string
	replaceSeam(t, &showMessage, func(caption, text string, icon uint32) {
		messages = append(messages, caption+"\n"+text)
	})

	// The service gate finds no registration on this machine, so the launch
	// continues to the server like a launch on a machine without a service.
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return service.Status{State: service.StateNotInstalled}, nil
	})

	// The publish of the state fails, and the URL the host wanted to
	// publish is recorded: the test probes that very URL afterwards.
	//
	// The launch publishes twice (re-pinned with the claim model): first
	// the url-less claim of the starting window, then the effective URL
	// once the server bound its address. The claim publishes fine here —
	// through the REAL PublishState, so a claim really exists on disk
	// while the launch runs and the cleanup assertion below discriminates
	// the removal of it — and this test drives the failure of the serving
	// publish, where the server it would describe is already running,
	// with the established fail-closed stop; the failure of the early
	// claim has its own test.
	publishedURL := ""
	replaceSeam(t, &publishState, func(path, url string) error {
		publishedURL = url
		if url == "" {
			return service.PublishState(path, url)
		}
		return errors.New("the state directory is read-only")
	})

	logFile := filepath.Join(t.TempDir(), "hfdesk.log")
	err := run(Options{}, logFile)
	if err == nil {
		t.Fatal("run() = nil, want the failure of the publish to end the launch")
	}
	if !strings.Contains(err.Error(), "publish the effective URL") {
		t.Errorf("run() = %q, want it to name the publish that failed", err)
	}
	if publishedURL == "" {
		t.Fatal("the host never reached the publish of its state: the test did not cover the guard")
	}

	// Nothing is served: the URL the host would have published does not
	// answer after the launch ended.
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	if response, requestErr := client.Get(publishedURL + "/api/health"); requestErr == nil {
		response.Body.Close()
		t.Fatalf("the server still answers on %s after the failed publish", publishedURL)
	}

	// No state is published: the mutation guard never sees a serving
	// desktop that is not there.
	if state, ok := service.ReadPublishedState(service.PublishedStatePath()); ok {
		t.Errorf("a published state %q exists after the failed publish", state.URL)
	}

	// The user is told the result, the cause with the error and one next
	// action, through the report of an ordinary startup failure.
	report := ""
	for _, message := range messages {
		if strings.HasPrefix(message, "HFDesk could not start\n") {
			report = message
		}
	}
	if report == "" {
		t.Fatalf("no startup failure was reported, want one over the unpublished state: %v", messages)
	}
	assertNames(t, report, "report", []string{
		"HFDesk is not serving",
		"could not publish the effective URL",
		"the state directory is read-only",
	})
	if got := strings.Count(report, "start HFDesk again"); got != 1 {
		t.Errorf("the number of next actions in the report = %d, want exactly 1: %q", got, report)
	}

	// The failure survives in the log file of the launch.
	logged, readErr := os.ReadFile(logFile)
	assertNoError(t, readErr, "read %s: %v", logFile, readErr)
	if !strings.Contains(string(logged), "publish the effective URL") {
		t.Errorf("the log file %s does not record the failure of the publish", logFile)
	}
}

// TestDesktopHostDoesNotStartWhenTheStartupClaimCannotBePublished drives the
// real startup with a claim that cannot be published. The claim is the fact
// the mutation guard of this session reads beside the singleton probe from
// the moment the launch passes the service gate, so a launch the guard
// cannot see must not serve: the failure ends the launch BEFORE the
// application is created, with the established startup-failure report, and
// leaves no claim behind.
//
// The seam records every attempt: exactly one, carrying the url-less claim
// of the starting window — proof that the launch never reached the server
// stage it would otherwise report.
func TestDesktopHostDoesNotStartWhenTheStartupClaimCannotBePublished(t *testing.T) {
	useTemporaryStateDir(t)

	var messages []string
	replaceSeam(t, &showMessage, func(caption, text string, icon uint32) {
		messages = append(messages, caption+"\n"+text)
	})

	// The service gate finds no registration, so the launch would continue
	// to the server if its claim could be published.
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return service.Status{State: service.StateNotInstalled}, nil
	})

	var attempts []string
	replaceSeam(t, &publishState, func(path, url string) error {
		attempts = append(attempts, url)
		return errors.New("the state directory is read-only")
	})

	// The application — the first step that can create a server — must be
	// created ONLY after the claim published. The creation is observed
	// through the seam, so a startup that builds the application before
	// attempting the url-less claim fails this test even though every
	// publish assertion below would still pass.
	created := 0
	previousNewApplication := newApplication
	replaceSeam(t, &newApplication, func(options app.Options, version string) (*app.App, error) {
		created++
		return previousNewApplication(options, version)
	})

	logFile := filepath.Join(t.TempDir(), "hfdesk.log")
	err := run(Options{}, logFile)
	if err == nil {
		t.Fatal("run() = nil, want the failed claim publish to end the launch")
	}
	if created != 0 {
		t.Errorf("the application was created %d time(s) before the startup claim published, want the claim to publish first", created)
	}
	if !strings.Contains(err.Error(), "publish the startup claim") {
		t.Errorf("run() = %q, want it to name the claim that could not be published", err)
	}
	if len(attempts) != 1 || attempts[0] != "" {
		t.Errorf("the publish attempts = %q, want exactly the one url-less claim of the starting window", attempts)
	}

	// The launch left no claim behind: the guard never sees a launch that
	// does not run, and never misses one that does.
	if _, ok := service.ReadPublishedState(service.PublishedStatePath()); ok {
		t.Error("a claim exists after the failed publish")
	}

	// The user is told the result, the cause with the error and exactly
	// one next action, through the report of an ordinary startup failure.
	report := ""
	for _, message := range messages {
		if strings.HasPrefix(message, "HFDesk could not start\n") {
			report = message
		}
	}
	if report == "" {
		t.Fatalf("no startup failure was reported, want one over the unpublished claim: %v", messages)
	}
	assertNames(t, report, "report", []string{
		"HFDesk is not serving",
		"could not publish the claim of this launch",
		"the state directory is read-only",
	})
	if got := strings.Count(report, "start HFDesk again"); got != 1 {
		t.Errorf("the number of next actions in the report = %d, want exactly 1: %q", got, report)
	}
}
