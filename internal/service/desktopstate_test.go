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
			PublishedStateLive = func() bool { return tc.claimLive }
			if got := LiveServingDesktop(tc.preExisting); got != tc.wantServing {
				t.Errorf("LiveServingDesktop(preExisting=%t) = %t, want %t",
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
