// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// TestPublishStateRoundTrip proves that the running instance publishes its
// effective URL and that the state is read back unchanged.
func TestPublishStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop-state.json")

	err := PublishState(path, "http://127.0.0.1:8080")
	assertNoError(t, err, "publish the state: %v", err)

	state, ok := ReadPublishedState(path)
	if !ok {
		t.Fatal("the published state was not read back")
	}
	if state.URL != "http://127.0.0.1:8080" {
		t.Errorf("url = %q, want %q", state.URL, "http://127.0.0.1:8080")
	}
	if state.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d", state.PID, os.Getpid())
	}
}

// TestPublishStateCreatesTheDirectory proves that the state file is written
// even when its directory does not exist yet.
func TestPublishStateCreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "desktop-state.json")

	if err := PublishState(path, "http://127.0.0.1:8080"); err != nil {
		t.Fatalf("publish the state: %v", err)
	}
	_, err := os.Stat(path)
	assertNoError(t, err, "the published state is missing: %v", err)
}

// TestReadPublishedStateToleratesAMissingOrBrokenFile proves that a state
// file that cannot be read is never a reason to fail: the state is a
// convenience, the single instance lock is the authority.
//
// An empty url is deliberately no longer a case here (re-pinned with the
// claim model): the claim of a launch that passed the service gate but has
// not bound its address yet carries no url, and the presence of a claim is
// decided by the liveness of its process, not by the url it carries — see
// TestPublishStateCarriesTheClaimOfALaunchThatHasNotBoundItsAddress.
func TestReadPublishedStateToleratesAMissingOrBrokenFile(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		content string
		write   bool
	}{
		{name: "missing file"},
		{name: "broken content", content: "{not json", write: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".json")
			if c.write {
				err := os.WriteFile(path, []byte(c.content), 0o600)
				assertNoError(t, err, "write the state file: %v", err)
			}
			if _, ok := ReadPublishedState(path); ok {
				t.Error("the unusable state file was accepted")
			}
		})
	}
}

// TestRemovePublishedState proves that the state file is removed when the
// instance ends.
func TestRemovePublishedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop-state.json")
	err := PublishState(path, "http://127.0.0.1:8080")
	assertNoError(t, err, "publish the state: %v", err)

	RemovePublishedState(path)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the published state is still there: %v", err)
	}
}

// TestPublishStateCarriesTheClaimOfALaunchThatHasNotBoundItsAddress pins
// the claim of the starting window: a launch that passed the service gate
// publishes before it opens the window in which it binds its address, so
// the claim can carry no url yet — it must still read back as a claim, with
// the PID that identifies it. The empty url was rejected before the claim
// model; it is now the display data that fills in below (re-pinned with
// the model change).
func TestPublishStateCarriesTheClaimOfALaunchThatHasNotBoundItsAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop-state.json")

	err := PublishState(path, "")
	assertNoError(t, err, "publish the url-less claim: %v", err)

	state, ok := ReadPublishedState(path)
	if !ok {
		t.Fatal("the claim of the starting window was not read back")
	}
	if state.URL != "" {
		t.Errorf("url = %q, want the empty url of a launch that has not bound its address", state.URL)
	}
	if state.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d — the claim must identify its launch", state.PID, os.Getpid())
	}
}

// TestLiveServingDesktopNeedsBothFacts proves the discriminator of a
// service mutation beside a desktop instance: the mutation is refused only
// when the desktop published a claim whose process is live AND another
// process of the session held the singleton when the caller probed. A live
// claim beside a probe that found no holder must not refuse; a holder
// beside a claim that is not live — the stale file of a crashed desktop,
// or the gate's own launch, whose Start and Update answers end the launch
// before it ever publishes — must not refuse either, even though the very
// holder is the instance that launched the mutation.
func TestLiveServingDesktopNeedsBothFacts(t *testing.T) {
	previous := PublishedStateLive
	t.Cleanup(func() { PublishedStateLive = previous })

	for _, tc := range []struct {
		name        string
		preExisting bool
		claimLive   bool
		wantServing bool
	}{
		{name: "a live serving desktop", preExisting: true, claimLive: true, wantServing: true},
		{name: "a live claim beside a probe that found no holder", preExisting: false, claimLive: true, wantServing: false},
		{name: "a stale claim beside the holder of the gate's own launch", preExisting: true, claimLive: false, wantServing: false},
		{name: "a launch that never published a claim", preExisting: true, claimLive: false, wantServing: false},
		{name: "nothing at all", preExisting: false, claimLive: false, wantServing: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			PublishedStateLive = func() (bool, bool) { return tc.claimLive, false }
			if got := LiveServingDesktop(tc.preExisting); got != tc.wantServing {
				t.Errorf("LiveServingDesktop(preExisting=%t) = %t, want %t",
					tc.preExisting, got, tc.wantServing)
			}
		})
	}
}

// TestCheckLiveServingDesktopNamesTheCauseFromTheSameRead pins the
// single-read contract behind the refusal messages: the decision and the
// named cause are both derived from ONE seam answer, and a caller whose
// probe found no holder receives neither — there is no refusal to name a
// cause for. The message layer must never re-read the file: with one read
// the named cause cannot contradict the state that triggered the refusal.
func TestCheckLiveServingDesktopNamesTheCauseFromTheSameRead(t *testing.T) {
	previous := PublishedStateLive
	t.Cleanup(func() { PublishedStateLive = previous })

	for _, tc := range []struct {
		name                string
		preExisting         bool
		live, unreadable    bool
		wantServing         bool
		wantUnreadableCause bool
	}{
		{"a live claim beside a holder", true, true, false, true, false},
		{"an unreadable claim beside a holder", true, false, true, true, true},
		{"a claim neither live nor readable beside a holder", true, false, false, false, false},
		{"an unreadable claim beside a probe that found no holder", false, false, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			PublishedStateLive = func() (bool, bool) { return tc.live, tc.unreadable }
			serving, unreadableClaim := CheckLiveServingDesktop(tc.preExisting)
			if serving != tc.wantServing || unreadableClaim != tc.wantUnreadableCause {
				t.Errorf("CheckLiveServingDesktop(preExisting=%t) = (%t, %t), want (%t, %t)",
					tc.preExisting, serving, unreadableClaim, tc.wantServing, tc.wantUnreadableCause)
			}
			if got := LiveServingDesktop(tc.preExisting); got != tc.wantServing {
				t.Errorf("LiveServingDesktop(preExisting=%t) = %t, want %t (the wrapper agrees with the tuple)",
					tc.preExisting, got, tc.wantServing)
			}
		})
	}
}

// TestLiveServingDesktopDecidesOnTheClaimOfTheOriginatingDesktop drives the
// real claim read (no seam) through the three states the predicate cannot
// distinguish when it only asks whether a file exists. Each substate states
// its perturbation: the only thing that changes inside a pair is the PID of
// the claim or the path the guard reads.
func TestLiveServingDesktopDecidesOnTheClaimOfTheOriginatingDesktop(t *testing.T) {
	claimFile := filepath.Join(t.TempDir(), "desktop-state.json")
	previous := claimStatePath
	claimStatePath = func() string { return claimFile }
	t.Cleanup(func() { claimStatePath = previous })

	t.Run("the starting window publishes a claim before it binds its address", func(t *testing.T) {
		// The claim of a launch that passed the service gate but has not
		// bound its address yet: no url, the PID of this very process.
		// Perturbation: the singleton fact — with a holder the launch that
		// is about to bind the address refuses the mutation; without one,
		// the same claim refuses nothing.
		err := PublishState(claimFile, "")
		assertNoError(t, err, "publish the claim of the starting window: %v", err)

		if !LiveServingDesktop(true) {
			t.Error("the guard was blind to a launch that is about to bind the address")
		}
		if LiveServingDesktop(false) {
			t.Error("the claim refused the mutation although no process of the session holds the singleton")
		}
	})

	t.Run("the stale claim of a crashed desktop counts as absent", func(t *testing.T) {
		// Perturbation: only the PID inside the claim changes — the
		// process the claim names is gone in the first leg (a desktop that
		// crashed and left its file behind) and runs in the second.
		dead := exitedProcessPID(t)
		stale := `{"url":"http://127.0.0.1:8080","pid":` + strconv.Itoa(dead) + `}`
		if err := os.WriteFile(claimFile, []byte(stale), 0o600); err != nil {
			t.Fatalf("write the stale claim: %v", err)
		}
		if LiveServingDesktop(true) {
			t.Error("a claim of a dead process refused the mutation: the guard named a tray app that is not there")
		}

		err := PublishState(claimFile, "http://127.0.0.1:8080")
		assertNoError(t, err, "publish the claim of a live desktop: %v", err)
		if !LiveServingDesktop(true) {
			t.Error("the live claim of the same file was not seen by the guard")
		}
	})

	t.Run("the guard reads the claim of the desktop it was launched for", func(t *testing.T) {
		// The approving account's own claim directory is empty: without
		// the path the elevation carries, the guard reads that directory
		// and is blind to the serving desktop of the originating user.
		if err := os.Remove(claimFile); err != nil && !os.IsNotExist(err) {
			t.Fatalf("empty the approving claim directory: %v", err)
		}
		if LiveServingDesktop(true) {
			t.Error("the guard saw a claim although the directory it reads holds none")
		}

		// Perturbation: the state path the elevation carries beside the
		// singleton name — it points at the claim of the ORIGINATING
		// desktop, whose launch (this process) still runs.
		originating := filepath.Join(t.TempDir(), "desktop-state.json")
		err := PublishState(originating, "")
		assertNoError(t, err, "publish the claim of the originating desktop: %v", err)
		UsePublishedStatePath(originating)
		t.Cleanup(func() { UsePublishedStatePath("") })

		if !LiveServingDesktop(true) {
			t.Error("the guard was blind: the claim of the originating desktop did not travel with the elevation")
		}
	})
}

// exitedProcessPID returns the process id of a process that ran and ended:
// the claim of a desktop that crashed names such a pid.
func exitedProcessPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run the helper process to its exit: %v", err)
	}
	return cmd.Process.Pid
}

// TestLiveServingDesktopFailsClosedOnAClaimItCannotRead pins the
// indeterminable state of the guard: a claim that exists but cannot be
// read or parsed is never the absent claim — with a singleton holder the
// conjunction refuses (the mutation must not proceed beside a desktop the
// guard cannot see), and without a holder the two-fact model is unchanged
// and proceeds. CheckLiveServingDesktop reports the refusal and the cause
// together, from one read, and reports the unreadable cause for nothing
// else: an absent, stale or readable claim carries no such cause.
func TestLiveServingDesktopFailsClosedOnAClaimItCannotRead(t *testing.T) {
	claimFile := filepath.Join(t.TempDir(), "desktop-state.json")
	previous := claimStatePath
	claimStatePath = func() string { return claimFile }
	t.Cleanup(func() { claimStatePath = previous })

	for _, tc := range []struct {
		name    string
		fixture func(t *testing.T)
	}{
		{"an unparsable claim", func(t *testing.T) {
			if err := os.WriteFile(claimFile, []byte("{not json"), 0o600); err != nil {
				t.Fatalf("write the unparsable claim: %v", err)
			}
		}},
		{"an unreadable claim", func(t *testing.T) {
			// A directory at the claim path fails the read
			// deterministically (EISDIR), even for a test run as
			// root, where chmod 0 would not deny the read.
			if err := os.Mkdir(claimFile, 0o755); err != nil {
				t.Fatalf("put a directory at the claim path: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.RemoveAll(claimFile); err != nil {
				t.Fatalf("empty the claim path: %v", err)
			}
			tc.fixture(t)
			t.Cleanup(func() { _ = os.RemoveAll(claimFile) })

			// The refusal and its named cause come from ONE read:
			// this tuple is the fact the message layer receives.
			serving, unreadable := CheckLiveServingDesktop(true)
			if !serving || !unreadable {
				t.Errorf("CheckLiveServingDesktop(true) = (%t, %t), want (true, true): a refusal whose cause is the unreadable claim", serving, unreadable)
			}
			if !LiveServingDesktop(true) {
				t.Error("a claim the guard could not read did not fail closed: the mutation would proceed beside a desktop it cannot see")
			}
			if serving, unreadable := CheckLiveServingDesktop(false); serving || unreadable {
				t.Errorf("CheckLiveServingDesktop(false) = (%t, %t), want (false, false): although no process of the session holds the singleton", serving, unreadable)
			}
			if LiveServingDesktop(false) {
				t.Error("the unreadable claim refused although no process of the session holds the singleton")
			}
		})
	}

	t.Run("only the unreadable presence reports unreadable", func(t *testing.T) {
		// Perturbation: only the file at the claim path changes —
		// absent, stale (its process is gone), readable and live —
		// and the unreadable fact stays false while the conjunction
		// keeps its documented states.
		if err := os.RemoveAll(claimFile); err != nil {
			t.Fatalf("empty the claim path: %v", err)
		}
		if _, unreadable := CheckLiveServingDesktop(true); unreadable {
			t.Error("an absent claim carries the unreadable cause")
		}
		if LiveServingDesktop(true) {
			t.Error("an absent claim refused the mutation: the directory the guard reads holds no claim")
		}

		dead := exitedProcessPID(t)
		stale := `{"url":"","pid":` + strconv.Itoa(dead) + `}`
		if err := os.WriteFile(claimFile, []byte(stale), 0o600); err != nil {
			t.Fatalf("write the stale claim: %v", err)
		}
		if _, unreadable := CheckLiveServingDesktop(true); unreadable {
			t.Error("a stale but readable claim carries the unreadable cause")
		}
		if LiveServingDesktop(true) {
			t.Error("a claim of a dead process refused the mutation: the guard named a tray app that is not there")
		}

		if err := PublishState(claimFile, "http://127.0.0.1:8080"); err != nil {
			t.Fatalf("publish the claim of a live desktop: %v", err)
		}
		// The live claim refuses with the SERVING cause: the tuple
		// keeps the unreadable cause out of a readable claim, so the
		// message layer names the right variant from the same read.
		if serving, unreadable := CheckLiveServingDesktop(true); !serving || unreadable {
			t.Errorf("CheckLiveServingDesktop(true) = (%t, %t), want (true, false): a readable live claim refuses with the serving cause", serving, unreadable)
		}
		if !LiveServingDesktop(true) {
			t.Error("the live claim of the same file was not seen by the guard")
		}
	})
}

// TestPublishedStateFileBelongsToOneSession pins the per-session claim file
// through its discriminating counterexample: session A publishes a live
// claim, session B ends and removes its own claim — with the shared
// per-user file B's cleanup deleted A's claim and the guard of session A
// went blind — and A's claim must survive B's cleanup and still refuse.
// The scope is driven through the seam: the real Windows session identity
// is not exercisable on this host.
func TestPublishedStateFileBelongsToOneSession(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("HOME", root)
	t.Setenv("AppData", root)
	t.Setenv("LOCALAPPDATA", root)

	previousScope := publishedStateScope
	t.Cleanup(func() { publishedStateScope = previousScope })
	previousClaim := claimStatePath
	claimStatePath = PublishedStatePath
	t.Cleanup(func() { claimStatePath = previousClaim })

	const sessionA = "S-1-5-21-test.1"
	const sessionB = "S-1-5-21-test.2"

	pathInSession := func(scope string) string {
		publishedStateScope = func() string { return scope }
		return PublishedStatePath()
	}
	if pathA, pathB := pathInSession(sessionA), pathInSession(sessionB); pathA == pathB {
		t.Fatalf("two sessions of one user share the claim file %q", pathA)
	}

	// Session A publishes a live claim (the process of this test run).
	publishedStateScope = func() string { return sessionA }
	if err := PublishState(PublishedStatePath(), "http://127.0.0.1:8080"); err != nil {
		t.Fatalf("session A publishes its claim: %v", err)
	}

	// Session B ends and removes ITS claim: with the shared file this was
	// the counterexample — A's live claim disappeared and the guard of
	// session A proceeded while A served.
	publishedStateScope = func() string { return sessionB }
	RemovePublishedState(PublishedStatePath())

	publishedStateScope = func() string { return sessionA }
	if _, err := os.Stat(PublishedStatePath()); err != nil {
		t.Fatalf("session B's cleanup left session A's claim unreadable: %v", err)
	}
	if !LiveServingDesktop(true) {
		t.Error("one session's cleanup removed the live claim of the other session: the guard of session A went blind")
	}

	// A stale cross-session file refuses nothing in its own session: only
	// the liveness of the claim's process decides, in both sessions.
	publishedStateScope = func() string { return sessionB }
	stale := `{"url":"","pid":` + strconv.Itoa(exitedProcessPID(t)) + `}`
	if err := os.WriteFile(PublishedStatePath(), []byte(stale), 0o600); err != nil {
		t.Fatalf("session B writes its stale claim: %v", err)
	}
	if LiveServingDesktop(true) {
		t.Error("a stale claim of another session refused the mutation in its own session")
	}
}
