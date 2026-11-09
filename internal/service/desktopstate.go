// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/bashrusakh/hfdesk/internal/server"
)

// PublishedState is the state the running desktop instance publishes, so a
// second launch can open the web UI of the instance that is already running.
//
// The state is a convenience, never the authority on whether HFDesk is
// running: the single instance lock decides that. It is owned by the
// service layer beside the singleton hold and probe it is read with: the
// headless command family classifies a mutation with the same two facts,
// and one shared owner keeps the file format, path and predicate from
// drifting apart for the desktop host as well.
type PublishedState struct {
	// URL is the effective, browser-safe URL of the running server.
	URL string `json:"url"`
	// PID is the process id of the running instance.
	PID int `json:"pid"`
}

// PublishedStatePath returns the path of the published state file.
//
// The file lives in the per user configuration directory, so two users of
// the same machine never share it.
func PublishedStatePath() string {
	return filepath.Join(server.AppConfigDir(), "desktop-state.json")
}

// PublishState writes the effective URL of the running instance.
//
// The file is written through a temporary file and renamed, so a reader
// never sees a half written state.
func PublishState(path, url string) error {
	data, err := json.Marshal(PublishedState{URL: url, PID: os.Getpid()})
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

// ReadPublishedState reads the published state.
//
// It reports false when the file is missing or corrupt. A state file that
// cannot be read is never a reason to fail a launch: the state is only a
// convenience.
func ReadPublishedState(path string) (PublishedState, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PublishedState{}, false
	}
	var state PublishedState
	if err := json.Unmarshal(data, &state); err != nil {
		return PublishedState{}, false
	}
	if strings.TrimSpace(state.URL) == "" {
		return PublishedState{}, false
	}
	return state, true
}

// RemovePublishedState removes the published state file.
func RemovePublishedState(path string) {
	_ = os.Remove(path)
}

// PublishedStatePresent reports whether a desktop instance published its
// state.
//
// The variable is the seam of the tests: a test drives both sides of
// LiveServingDesktop without writing into the configuration directory of
// the user that runs it.
var PublishedStatePresent = func() bool {
	_, ok := ReadPublishedState(PublishedStatePath())
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
	return singletonPreExisting && PublishedStatePresent()
}
