// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPublishStateRoundTrip proves that the running instance publishes its
// effective URL and that the state is read back unchanged.
func TestPublishStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop-state.json")

	if err := PublishState(path, "http://127.0.0.1:8080"); err != nil {
		t.Fatalf("publish the state: %v", err)
	}

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
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the published state is missing: %v", err)
	}
}

// TestReadPublishedStateToleratesAMissingOrBrokenFile proves that a state
// file that cannot be read is never a reason to fail: the state is a
// convenience, the single instance lock is the authority.
func TestReadPublishedStateToleratesAMissingOrBrokenFile(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		content string
		write   bool
	}{
		{name: "missing file"},
		{name: "broken content", content: "{not json", write: true},
		{name: "empty url", content: `{"url":"","pid":1}`, write: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".json")
			if c.write {
				if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
					t.Fatalf("write the state file: %v", err)
				}
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
	if err := PublishState(path, "http://127.0.0.1:8080"); err != nil {
		t.Fatalf("publish the state: %v", err)
	}

	RemovePublishedState(path)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the published state is still there: %v", err)
	}
}

// TestLiveServingDesktopNeedsBothFacts proves the discriminator of a
// service mutation beside a desktop instance: the mutation is refused only
// when the desktop published its state AND another process of the session
// held the singleton when the caller probed. A state file that outlived a
// crashed desktop meets a probe that found no holder and must not refuse;
// a holder without a published state — the service gate's launch, which
// never publishes one — must not refuse either.
func TestLiveServingDesktopNeedsBothFacts(t *testing.T) {
	previous := PublishedStatePresent
	t.Cleanup(func() { PublishedStatePresent = previous })

	for _, tc := range []struct {
		name         string
		preExisting  bool
		statePresent bool
		wantServing  bool
	}{
		{name: "a live serving desktop", preExisting: true, statePresent: true, wantServing: true},
		{name: "a stale state file of a crashed desktop", preExisting: false, statePresent: true, wantServing: false},
		{name: "a launch that never published a state", preExisting: true, statePresent: false, wantServing: false},
		{name: "nothing at all", preExisting: false, statePresent: false, wantServing: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			PublishedStatePresent = func() bool { return tc.statePresent }
			if got := LiveServingDesktop(tc.preExisting); got != tc.wantServing {
				t.Errorf("LiveServingDesktop(preExisting=%t) = %t, want %t",
					tc.preExisting, got, tc.wantServing)
			}
		})
	}
}
