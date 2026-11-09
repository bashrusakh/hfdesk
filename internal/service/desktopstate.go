// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bashrusakh/hfdesk/internal/server"
)

// PublishedState is the state the desktop instance publishes, so a second
// launch can open the web UI of the instance that is already running and
// the mutation guard of the session can see a desktop that serves — or is
// about to serve — the address.
//
// The state is a convenience, never the authority on whether HFDesk is
// running: the single instance lock decides that. It is owned by the
// service layer beside the singleton hold and probe it is read with: the
// headless command family classifies a mutation with the same two facts,
// and one shared owner keeps the file format, path and predicate from
// drifting apart for the desktop host as well.
type PublishedState struct {
	// URL is the effective, browser-safe URL of the running server. It is
	// display data of the second launch, never the liveness of the claim:
	// a claim published before the launch bound its address carries none.
	URL string `json:"url"`
	// PID is the process id of the instance that published the state. It
	// identifies the claim: a reader validates that the process still
	// runs, so the file a crashed desktop left behind is a stale claim
	// and counts as absent.
	PID int `json:"pid"`
}

// PublishedStatePath returns the path of the published state file.
//
// The file lives in the per user configuration directory, so two users of
// the same machine never share it.
func PublishedStatePath() string {
	return filepath.Join(server.AppConfigDir(), "desktop-state.json")
}

// PublishState writes the claim of this instance: the effective URL when
// the server has one, always the process id that identifies it. An empty
// url is the claim of a launch that passed the service gate but has not
// bound its address yet.
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
// It reports false when the file is missing or corrupt. The url is not a
// condition: it is display data that fills in once the server bound its
// address, and the presence of a claim is decided by the liveness of its
// process (PublishedStateLive), not by the url it carries. A state file
// that cannot be read is never a reason to fail a launch: the state is
// only a convenience.
func ReadPublishedState(path string) (PublishedState, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PublishedState{}, false
	}
	var state PublishedState
	if err := json.Unmarshal(data, &state); err != nil {
		return PublishedState{}, false
	}
	return state, true
}

// RemovePublishedState removes the published state file.
func RemovePublishedState(path string) {
	_ = os.Remove(path)
}

// claimStatePath is the state file the mutation guard reads: the claim
// directory of this session by default, the claim of the desktop that
// launched the current mutation once UsePublishedStatePath pointed it
// there.
var claimStatePath = PublishedStatePath

// UsePublishedStatePath points the mutation guard at the published claim of
// the desktop that launched this mutation; an empty path restores the claim
// directory of this session.
//
// The elevated helper calls it with the state path its launcher passed on
// the command line beside --instance-mutex. An elevation answered by another
// administrator runs in the approving account's configuration directory,
// which never holds the claim of the desktop that asked for the mutation:
// without this path the guard would read that empty directory and be blind
// to a serving desktop of the originating user — the dangerous direction of
// the guard. The mutex already travels with the elevation; the claim path
// now travels with it, and both name the same originating desktop.
func UsePublishedStatePath(path string) {
	if cleaned := strings.TrimSpace(path); cleaned != "" {
		claimStatePath = func() string { return cleaned }
		return
	}
	claimStatePath = PublishedStatePath
}

// claimProcessAlive reports whether the process a published claim names
// still runs.
//
// The check is a platform fact (the ownership rule of this package): the
// default below is the signal check of the platforms that have signals,
// and the Windows file replaces it at init with the process-open check of
// a platform where signal 0 is not supported.
var claimProcessAlive = claimProcessAliveBySignal

// claimProcessAliveBySignal is the liveness check of the platforms that
// have signals: signal 0 asks for existence and changes nothing.
func claimProcessAliveBySignal(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer func() { _ = process.Release() }()
	signalErr := process.Signal(syscall.Signal(0))
	if signalErr == nil {
		return true
	}
	// The process exists but this identity may not signal it (EPERM): an
	// unverifiable claim stays live, so the guard fails closed instead of
	// hiding behind a claim it could not read. Every other answer is a
	// process that is gone.
	return errors.Is(signalErr, syscall.EPERM)
}

// PublishedStateLive reports whether a desktop instance published a claim
// that is still live: the file exists, parses, and the process it names
// still runs.
//
// A claim whose process died is the file a crashed desktop left behind —
// stale, and counted as absent, so no mutation is refused for a tray app
// that is not there. A claim whose process runs is live in both phases of
// the desktop: the starting window before the server bound its address and
// the serving desktop after it, because the claim is published before the
// launch opens the window in which it binds the address.
//
// The variable is the seam of the tests: a test drives both sides of
// LiveServingDesktop without writing into the configuration directory of
// the user that runs it.
var PublishedStateLive = func() bool {
	state, ok := ReadPublishedState(claimStatePath())
	if !ok {
		return false
	}
	return claimProcessAlive(state.PID)
}

// LiveServingDesktop reports whether a desktop instance that can still
// object to a service mutation of this session is up: another process of
// this session held the singleton when the caller probed it around its
// hold, AND the published claim the caller reads is live. The two facts
// decide the three states of a claim:
//
//   - stale claim — a desktop crashed and its file survived: the PID the
//     file names is dead, the claim counts as absent, and the mutation is
//     not refused for a tray app that does not exist;
//   - starting window — the launch passed the service gate but has not
//     bound its address yet: it published its claim before that window
//     opened, so the claim is live with its process and a mutation that
//     would take the address first is refused;
//   - foreign-user elevation — another administrator approved the prompt:
//     the state path travelled with the singleton name, so the helper
//     reads the ORIGINATING desktop's claim and never the (empty) claim
//     directory of the approving account.
//
// singletonPreExisting is the caller's ProbeDesktopSingleton result. The
// kernel object dies with the processes that hold it, so the two facts
// together decide both directions: a claim that outlived a crashed desktop
// meets a probe that found no holder and never turns into a refusal, while
// a launch that never committed to serving — the service gate's own Start
// and Update answers end the launch without a claim, and a claim whose
// process ended counts as absent — never refuses the mutation it launched
// itself.
func LiveServingDesktop(singletonPreExisting bool) bool {
	return singletonPreExisting && PublishedStateLive()
}
