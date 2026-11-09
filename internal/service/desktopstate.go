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

// PublishedStatePath returns the path of the published state file of this
// session.
//
// The file lives in the per user configuration directory and its name
// carries the session scope of the desktop that publishes it — the same
// user-and-session identity the per-session desktop singleton is named with —
// so two users never share it, and two sessions of one user never share it
// either: the exit of a desktop removes only its own claim and can neither
// hide nor delete the claim of another session. A platform without a desktop
// host has no session to name and keeps the plain per-user file name.
func PublishedStatePath() string {
	scope := publishedStateScope()
	if scope == "" {
		return filepath.Join(server.AppConfigDir(), "desktop-state.json")
	}
	return filepath.Join(server.AppConfigDir(), "desktop-state."+scope+".json")
}

// publishedStateScope is the session scope the claim file name carries.
//
// It is a variable so the tests can drive the cross-session behavior of the
// claim file on a host whose real Windows session identity they cannot
// exercise; the platform files own the real answer.
var publishedStateScope = currentPublishedStateScope

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

// ReadPublishedState reads the published state, tolerantly: it reports false
// when the file is missing, unreadable or corrupt.
//
// The url is not a condition: it is display data that fills in once the
// server bound its address, and the presence of a claim is decided by the
// liveness of its process (PublishedStateLive), not by the url it carries.
// A state file that cannot be read is never a reason to fail a launch: the
// state is only a convenience.
//
// The mutation guard does not decide from this tolerant read: a claim that
// exists but cannot be read or parsed is an indeterminable fact for it —
// the unreadableClaim answer of CheckLiveServingDesktop, never an absent
// one.
func ReadPublishedState(path string) (PublishedState, bool) {
	state, presence := readPublishedClaim(path)
	return state, presence == claimReadable
}

// claimPresence classifies the claim file of the guard: absent, present but
// unreadable or unparsable, or readable.
type claimPresence int

const (
	claimAbsent claimPresence = iota
	claimUnreadable
	claimReadable
)

// readPublishedClaim reads the claim file once and classifies the answer.
//
// Absence is reported only for a path the platform proves is not there: any
// other read failure (a permission this identity does not have, a directory
// where the file should be) and any parse failure reports the claim as
// present-but-unreadable, the state in which whether a desktop serves cannot
// be determined.
func readPublishedClaim(path string) (PublishedState, claimPresence) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return PublishedState{}, claimAbsent
		}
		return PublishedState{}, claimUnreadable
	}
	var state PublishedState
	if err := json.Unmarshal(data, &state); err != nil {
		return PublishedState{}, claimUnreadable
	}
	return state, claimReadable
}

// ClaimStatePath returns the claim file the mutation guard reads right now:
// the claim file of this session by default, the claim of the desktop that
// launched the current mutation once UsePublishedStatePath pointed it there.
// The refusal over a claim that cannot be read names this very file.
func ClaimStatePath() string {
	return claimStatePath()
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

// PublishedStateLive reports the two facts of the published claim in ONE
// read of the file: live (a readable claim whose process it names still
// runs) and unreadable (the file exists but cannot be read or parsed).
//
// A claim whose process died is the file a crashed desktop left behind —
// stale, counted as absent: neither fact answers true, so no mutation is
// refused for a tray app that is not there. A claim whose process runs is
// live in both phases of the desktop: the starting window before the server
// bound its address and the serving desktop after it, because the claim is
// published before the launch opens the window in which it binds the
// address.
//
// The unreadable answer fails closed at the conjunction that decides the
// guard (CheckLiveServingDesktop): absence is only absence when the file is
// provably not there, and a guard that could not inspect the claim never
// proceeds beside a desktop it cannot see. The two answers come from one
// read, so the cause a refusal names cannot contradict the state that
// triggered it — the message layer never re-reads the file.
//
// The variable is the seam of the tests: a test drives both sides of
// LiveServingDesktop without writing into the configuration directory of
// the user that runs it.
var PublishedStateLive = func() (live bool, unreadable bool) {
	state, presence := readPublishedClaim(claimStatePath())
	switch presence {
	case claimReadable:
		return claimProcessAlive(state.PID), false
	case claimUnreadable:
		return false, true
	default:
		return false, false
	}
}

// CheckLiveServingDesktop classifies the mutation guard from ONE read of
// the claim: serving is the two-fact conjunction — the caller's probe found
// a pre-existing holder AND the claim is live or unreadable — and
// unreadableClaim tells the caller which cause the refusal must name (the
// honest terminal of a claim the guard could not read, or nothing when the
// claim proved live). Both answers derive from the same read, so the named
// cause can never contradict the state that triggered the refusal.
//
// A caller whose probe found no holder gets (false, false): no desktop of
// this session held the singleton, so the claim cannot belong to a serving
// desktop of this session and there is no refusal to name a cause for.
func CheckLiveServingDesktop(singletonPreExisting bool) (serving bool, unreadableClaim bool) {
	live, unreadable := PublishedStateLive()
	if !singletonPreExisting {
		return false, false
	}
	return live || unreadable, unreadable
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
// A claim that exists but cannot be read or parsed meets the second fact
// fail-closed: the conjunction refuses — a guard that could not inspect the
// claim never proceeds beside a desktop it cannot see — and the caller
// reports that indeterminable cause (through CheckLiveServingDesktop, from
// the same read that decided the refusal) instead of the serving one.
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
	serving, _ := CheckLiveServingDesktop(singletonPreExisting)
	return serving
}
