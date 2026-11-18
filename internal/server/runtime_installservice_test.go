// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runtimeServiceStub records what the host surface of a test reported.
type runtimeServiceStub struct {
	// reads counts the reads of the state.
	reads int
	// state is the state word every read reports.
	state string
	// err is the failure every read reports.
	err error
	// installs counts the dispatched install actions.
	installs int
}

// stubRuntimeService registers a service surface for the duration of the
// test and restores the empty surface when the test ends, so a test never
// reaches a real registration or a real installation.
func stubRuntimeService(t *testing.T, read func() (string, error), install func()) {
	t.Helper()
	SetRuntimeService(read, install)
	t.Cleanup(func() { SetRuntimeService(nil, nil) })
}

// TestRuntimeStatusReportsTheServiceState proves that the status endpoint
// carries the state word of the platform service registration, so the
// Runtime window can show the same status the tray menu shows.
func TestRuntimeStatusReportsTheServiceState(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 16)
	stub := &runtimeServiceStub{state: "running"}
	stubRuntimeService(t,
		func() (string, error) {
			stub.reads++
			return stub.state, stub.err
		}, nil)
	handler := newRuntimeTestServer(pipeline).handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, runtimeRequest(http.MethodGet, "/api/runtime", "127.0.0.1:5555"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var status RuntimeStatusResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if status.ServiceState != "running" {
		t.Errorf("service state = %q, want %q", status.ServiceState, "running")
	}
	if stub.reads != 1 {
		t.Errorf("the status read the registration %d times, want once per request", stub.reads)
	}
}

// TestRuntimeStatusReportsAnUnknownServiceState proves the honesty rule of
// the status: a read that failed, a read without a state and a host without
// the service surface all report the unknown state, and none of them ever
// reports "not-installed" — a client would enable an installation on that
// word.
func TestRuntimeStatusReportsAnUnknownServiceState(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 16)
	handler := newRuntimeTestServer(pipeline).handler()

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{
			name: "no host surface",
			setup: func(t *testing.T) {
				stubRuntimeService(t, nil, nil)
			},
		},
		{
			name: "the read fails",
			setup: func(t *testing.T) {
				stubRuntimeService(t, func() (string, error) {
					return "", errors.New("the service control manager did not answer")
				}, nil)
			},
		},
		{
			name: "the read reports no state",
			setup: func(t *testing.T) {
				stubRuntimeService(t, func() (string, error) {
					return "   ", nil
				}, nil)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, runtimeRequest(http.MethodGet, "/api/runtime", "127.0.0.1:5555"))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}

			var status RuntimeStatusResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
				t.Fatalf("decode status response: %v", err)
			}
			if status.ServiceState != runtimeServiceStateUnknown {
				t.Errorf("service state = %q, want %q", status.ServiceState, runtimeServiceStateUnknown)
			}
			if status.ServiceState == "not-installed" {
				t.Error("an unsettled read reported a machine without a service")
			}
		})
	}
}

// runtimeInstallServiceRequest builds the request of the install-service
// action with an explicit peer address, an optional origin and an optional
// body.
//
// The host names the local machine, because the local-only guard rejects a
// request that asks for another name.
func runtimeInstallServiceRequest(remoteAddr, origin, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/runtime/install-service", strings.NewReader(body))
	r.RemoteAddr = remoteAddr
	r.Host = runtimeLocalHost
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

// TestRuntimeInstallServiceRejectsNonLocalClients proves that the
// install-service action is guarded by the same local-only rule as the
// other runtime endpoints: a client that is not on the loopback interface,
// and a page of another site that borrows the loopback interface, are both
// rejected, and neither of them reaches the host.
func TestRuntimeInstallServiceRejectsNonLocalClients(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 16)
	stub := &runtimeServiceStub{state: "not-installed"}
	stubRuntimeService(t,
		func() (string, error) { return stub.state, nil },
		func() { stub.installs++ })
	handler := newRuntimeTestServer(pipeline).handler()

	cases := []struct {
		name       string
		remoteAddr string
		origin     string
	}{
		{name: "non-loopback peer", remoteAddr: "192.0.2.10:5555"},
		{name: "cross origin", remoteAddr: "127.0.0.1:5555", origin: "https://evil.example"},
		{name: "opaque origin", remoteAddr: "127.0.0.1:5555", origin: "null"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, runtimeInstallServiceRequest(c.remoteAddr, c.origin, ""))
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
			}
		})
	}

	if stub.installs != 0 {
		t.Errorf("the rejected requests dispatched %d installations, want none", stub.installs)
	}
}

// TestRuntimeInstallServiceDispatchesToTheHostOnce proves the contract of
// the endpoint: one click reaches the host exactly once, the response is
// the body the neighboring runtime actions answer with, and nothing in the
// request selects where the action goes — the endpoint hands the host its
// own action and nothing else.
func TestRuntimeInstallServiceDispatchesToTheHostOnce(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 16)
	stub := &runtimeServiceStub{state: "not-installed"}
	stubRuntimeService(t,
		func() (string, error) { return stub.state, nil },
		func() { stub.installs++ })
	handler := newRuntimeTestServer(pipeline).handler()

	request := runtimeInstallServiceRequest("127.0.0.1:5555", "", `{"action":"update"}`)
	request.URL.RawQuery = "action=update"

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if stub.installs != 1 {
		t.Errorf("one click dispatched %d installations, want exactly one", stub.installs)
	}

	var payload RuntimeInstallServiceResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode install-service response %q: %v", recorder.Body.String(), err)
	}
	if !payload.OK {
		t.Error("the response does not report that the host received the action")
	}
}

// TestRuntimeInstallServiceWithoutAHostAction proves that a build whose
// host provides no installation answers 404 with an explanation instead of
// dispatching nothing behind a success, so the window can disable its
// button instead of failing on every click.
func TestRuntimeInstallServiceWithoutAHostAction(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 16)
	stubRuntimeService(t, func() (string, error) {
		return "running", nil
	}, nil)
	handler := newRuntimeTestServer(pipeline).handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, runtimeInstallServiceRequest("127.0.0.1:5555", "", ""))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}

	var payload ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error payload %q: %v", recorder.Body.String(), err)
	}
	if payload.Error == "" {
		t.Errorf("error payload %v does not explain the refusal", payload)
	}
}
