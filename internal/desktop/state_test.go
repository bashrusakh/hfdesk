// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// TestServerReachableAnswersForALiveServer proves that a running instance is
// recognized by its health endpoint.
func TestServerReachableAnswersForALiveServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if !serverReachable(server.URL) {
		t.Error("the running server was not recognized")
	}
}

// TestServerReachableRejectsADeadServer proves that a stale state file is
// not taken for a running instance: the URL of a server that is not there
// must never be opened in the browser.
func TestServerReachableRejectsADeadServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := server.URL
	server.Close()

	if serverReachable(url) {
		t.Error("the closed server was taken for a running one")
	}
}

// TestServerReachableRejectsAnUnhealthyServer proves that a server that
// does not answer with a success is not taken for a running instance.
func TestServerReachableRejectsAnUnhealthyServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	if serverReachable(server.URL) {
		t.Error("the failing server was taken for a running one")
	}
}

// TestPublishServingStateStopsTheHostWhenThePublishFails proves the
// fail-closed direction: a desktop host whose state cannot be published
// stops its server instead of serving.
//
// The mutation guard of the session decides from the published state
// whether a desktop serves, so a host that kept serving without one would
// let `hfdesk service install/start/restart` start the service beside it.
// The failure therefore stops the server and leaves no state behind.
func TestPublishServingStateStopsTheHostWhenThePublishFails(t *testing.T) {
	previous := publishState
	publishState = func(string, string) error {
		return errors.New("the state directory is read-only")
	}
	t.Cleanup(func() { publishState = previous })

	path := filepath.Join(t.TempDir(), "desktop-state.json")
	stopped := false
	err := publishServingState(path, "http://127.0.0.1:8080", func() { stopped = true })

	if err == nil {
		t.Fatal("publishServingState() = nil, want the failure of the publish")
	}
	if !strings.Contains(err.Error(), "publish the effective URL") {
		t.Errorf("the error = %q, want it to name the publish that failed", err)
	}
	if !stopped {
		t.Error("the server was not stopped although the state could not be published: " +
			"the host would serve a state the mutation guard cannot see")
	}
	if _, ok := service.ReadPublishedState(path); ok {
		t.Error("a published state exists after the failed publish")
	}
}

// TestPublishServingStatePublishesAndKeepsServing proves the other
// direction: a publish that succeeds lets the host serve exactly as before
// and leaves the state the mutation guard reads.
func TestPublishServingStatePublishesAndKeepsServing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop-state.json")
	stopped := false
	if err := publishServingState(path, "http://127.0.0.1:8080", func() { stopped = true }); err != nil {
		t.Fatalf("publishServingState() = %v, want nil", err)
	}
	if stopped {
		t.Error("the host was stopped although its state was published")
	}
	state, ok := service.ReadPublishedState(path)
	if !ok {
		t.Fatal("no state was published although the publish succeeded")
	}
	if state.URL != "http://127.0.0.1:8080" {
		t.Errorf("the published URL = %q, want %q", state.URL, "http://127.0.0.1:8080")
	}
}
