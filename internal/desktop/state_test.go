// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
