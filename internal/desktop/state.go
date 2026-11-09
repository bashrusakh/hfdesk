// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/internal/server"
)

// reachabilityTimeout bounds the probe of the published URL.
//
// The probe runs while a second launch waits for the user, so it must be
// short: a URL that does not answer right away is a URL of an instance that
// is not there.
const reachabilityTimeout = 2 * time.Second

// publishedState is the state the running instance publishes, so a second
// launch can open the web UI of the instance that is already running.
//
// The state is a convenience, never the authority on whether HFDesk is
// running: the single instance lock decides that.
type publishedState struct {
	// URL is the effective, browser-safe URL of the running server.
	URL string `json:"url"`
	// PID is the process id of the running instance.
	PID int `json:"pid"`
}

// publishedStatePath returns the path of the published state file.
//
// The file lives in the per user configuration directory, so two users of
// the same machine never share it.
func publishedStatePath() string {
	return filepath.Join(server.AppConfigDir(), "desktop-state.json")
}

// publishState writes the effective URL of the running instance.
//
// The file is written through a temporary file and renamed, so a reader
// never sees a half written state.
func publishState(path, url string) error {
	data, err := json.Marshal(publishedState{URL: url, PID: os.Getpid()})
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

// readPublishedState reads the published state.
//
// It reports false when the file is missing or corrupt. A state file that
// cannot be read is never a reason to fail a launch: the state is only a
// convenience.
func readPublishedState(path string) (publishedState, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return publishedState{}, false
	}
	var state publishedState
	if err := json.Unmarshal(data, &state); err != nil {
		return publishedState{}, false
	}
	if strings.TrimSpace(state.URL) == "" {
		return publishedState{}, false
	}
	return state, true
}

// removePublishedState removes the published state file.
func removePublishedState(path string) {
	_ = os.Remove(path)
}

// probeDesktopSingleton probes the per-session desktop singleton object of
// this session. The variable is the platform seam: Windows binds it to the
// service layer's probe in instance_windows.go, and a platform without the
// desktop host has no object, so nobody can hold one and the probe reports
// that the object did not pre-exist.
var probeDesktopSingleton = func(string) (func(), bool, error) { return nil, false, nil }

// ProbeDesktopSingleton reports whether the per-session desktop singleton
// object already existed when this process first touched it, through the
// probe of the platform (the service layer's probe on Windows), and returns
// a release that keeps the object alive until the caller ends it.
//
// A service mutation probes before it holds: together with the published
// state, the result tells whether a desktop instance of this session is
// alive (see LiveServingDesktop).
func ProbeDesktopSingleton(name string) (release func(), preExisting bool, err error) {
	return probeDesktopSingleton(name)
}

// publishedStatePresent reports whether a desktop instance published its
// state.
//
// The variable is the seam of the tests: a test drives both sides of
// LiveServingDesktop without writing into the configuration directory of
// the user that runs it.
var publishedStatePresent = func() bool {
	_, ok := readPublishedState(publishedStatePath())
	return ok
}

// LiveServingDesktop reports whether a desktop instance of this session is
// alive and serving: it published its state — only a started server
// publishes one, the shutdown removes it — and another process of this
// session held the singleton when the caller probed it around its hold.
//
// singletonPreExisting is the caller's ProbeDesktopSingleton result. The
// kernel object dies with the processes that hold it, so the two facts
// together decide both directions: a state file that outlived a crashed
// desktop meets a probe that found no holder and never turns into a
// refusal, while a launch that never started its server (the service gate,
// an unreadable registration) published no state and never turns into one
// either.
func LiveServingDesktop(singletonPreExisting bool) bool {
	return singletonPreExisting && publishedStatePresent()
}

// publishedURL returns the URL of the running instance, or an empty string
// when no instance published one.
func publishedURL() string {
	state, ok := readPublishedState(publishedStatePath())
	if !ok {
		return ""
	}
	return state.URL
}

// serverReachable reports whether the server behind the URL answers a health
// request.
//
// The second launch probes the published URL before it opens it, so a stale
// state file never sends the browser to a dead URL.
func serverReachable(url string) bool {
	target := strings.TrimRight(url, "/") + "/api/health"

	ctx, cancel := context.WithTimeout(context.Background(), reachabilityTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode == http.StatusOK
}
