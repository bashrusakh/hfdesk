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

	"github.com/bashrusakh/hfdesk/internal/logging"
	"github.com/bashrusakh/hfdesk/internal/serviceview"
)

// runtimeServiceStub records what the host surface of a test reported.
type runtimeServiceStub struct {
	// reads counts the reads of the state.
	reads int
	// snapshot is the view every read reports.
	snapshot serviceview.Snapshot
	// err is the failure every read reports.
	err error
	// installs counts the dispatched install actions.
	installs int
}

// stubRuntimeService registers a service surface for the duration of the
// test and restores the empty surface when the test ends, so a test never
// reaches a real registration or a real installation.
func stubRuntimeService(t *testing.T, read func() (serviceview.Snapshot, error), install func()) {
	t.Helper()
	SetRuntimeService(read, install)
	t.Cleanup(func() { SetRuntimeService(nil, nil) })
}

// TestRuntimeStatusReportsTheServiceState proves that the status endpoint
// carries the state word of the platform service registration, so the
// Runtime window can show the same status the tray menu shows.
func TestRuntimeStatusReportsTheServiceState(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 16)
	want := serviceview.From("running", true, false)
	stub := &runtimeServiceStub{snapshot: want}
	stubRuntimeService(t,
		func() (serviceview.Snapshot, error) {
			stub.reads++
			return stub.snapshot, stub.err
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
	wantWire := want
	wantWire.State = "" // the raw word is never serialized in the view
	if status.Service != wantWire {
		t.Errorf("service view = %+v, want %+v", status.Service, wantWire)
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
				stubRuntimeService(t, func() (serviceview.Snapshot, error) {
					return serviceview.Snapshot{}, errors.New("the service control manager did not answer")
				}, nil)
			},
		},
		{
			name: "the read reports no state",
			setup: func(t *testing.T) {
				stubRuntimeService(t, func() (serviceview.Snapshot, error) {
					return serviceview.From("   ", false, false), nil
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
			if want := serviceview.From("", false, true); status.Service != want {
				t.Errorf("service view = %+v, want the unknown view %+v", status.Service, want)
			}
		})
	}
}

// wantWithStateCleared is the wire shape of a snapshot: the raw state word
// is kept out of the serialized view (json:"-") and travels in serviceState.
func wantWithStateCleared(snapshot serviceview.Snapshot) serviceview.Snapshot {
	snapshot.State = ""
	return snapshot
}

// TestRuntimeStatusServiceView proves that the status carries the classified
// view and the compatible raw word of the same read, and that the view is
// exactly the shared table of the application — for a host that provides the
// installation, so the table's install action reaches the page unmasked.
func TestRuntimeStatusServiceView(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 16)

	for _, tc := range []struct {
		name     string
		state    string
		running  bool
		readErr  bool
		wantWord string
	}{
		{name: "absent", state: "not-installed", wantWord: "not-installed"},
		{name: "running", state: "running", running: true, wantWord: "running"},
		{name: "stopped", state: "stopped", wantWord: "stopped"},
		{name: "failed", state: "failed", wantWord: "failed"},
		{name: "broken", state: "broken", wantWord: "broken"},
		{name: "broken with a running instance", state: "broken", running: true, wantWord: "broken"},
		{name: "starting", state: "starting", wantWord: "starting"},
		{name: "stopping", state: "stopping", wantWord: "stopping"},
		{name: "the unknown word", state: "unknown", wantWord: "unknown"},
		{name: "no state", state: "   ", wantWord: "unknown"},
		{name: "failed read", state: "running", running: true, readErr: true, wantWord: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := serviceview.From(tc.state, tc.running, tc.readErr)
			stubRuntimeService(t, func() (serviceview.Snapshot, error) {
				if tc.readErr {
					return serviceview.Snapshot{}, errors.New("the service control manager did not answer")
				}
				return want, nil
			}, func() {})

			recorder := httptest.NewRecorder()
			newRuntimeTestServer(pipeline).handler().ServeHTTP(recorder,
				runtimeRequest(http.MethodGet, "/api/runtime", "127.0.0.1:5555"))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}

			var status RuntimeStatusResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
				t.Fatalf("decode status response: %v", err)
			}
			if status.Service != wantWithStateCleared(want) {
				t.Errorf("service view = %+v, want %+v", status.Service, wantWithStateCleared(want))
			}
			if status.ServiceState != tc.wantWord {
				t.Errorf("serviceState = %q, want %q", status.ServiceState, tc.wantWord)
			}

			// The wire shape of the view: the classified fields and the
			// six actions, and never the raw state word (which the
			// compatible field carries).
			var payload struct {
				Service map[string]json.RawMessage `json:"service"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode raw status response: %v", err)
			}
			for _, key := range []string{"class", "label", "tone", "actions"} {
				if _, ok := payload.Service[key]; !ok {
					t.Errorf("the service object %v does not carry %q", payload.Service, key)
				}
			}
			if _, ok := payload.Service["state"]; ok {
				t.Error("the service object carries a raw state word, want the classified view only")
			}
			var actions map[string]json.RawMessage
			if err := json.Unmarshal(payload.Service["actions"], &actions); err != nil {
				t.Fatalf("decode actions %q: %v", payload.Service["actions"], err)
			}
			for _, key := range []string{"install", "start", "stop", "update", "delete", "open"} {
				if _, ok := actions[key]; !ok {
					t.Errorf("the actions object %v does not carry %q", actions, key)
				}
			}
		})
	}
}

// runtimeStatusOf performs one GET /api/runtime of a test server and decodes
// the status it answers.
func runtimeStatusOf(t *testing.T, pipeline *logging.Pipeline) RuntimeStatusResponse {
	t.Helper()

	recorder := httptest.NewRecorder()
	newRuntimeTestServer(pipeline).handler().ServeHTTP(recorder,
		runtimeRequest(http.MethodGet, "/api/runtime", "127.0.0.1:5555"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var status RuntimeStatusResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	return status
}

// TestRuntimeStatusWithdrawsTheInstallActionWithoutAHostInstallation proves
// the rule of the status beside a host that registered no installation: the
// install action is withdrawn from the view the page reads — invisible and
// therefore inactive — because POST /api/runtime/install-service could only
// answer 404 there. The proven absence is the case the button is offered in,
// so it is pinned beside the two states the table hides it in anyway; every
// other action keeps the table's answer, the raw state word travels
// unchanged, and the snapshot the host returned is not touched: the
// withdrawal belongs to the copy this request answers with.
func TestRuntimeStatusWithdrawsTheInstallActionWithoutAHostInstallation(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 16)

	for _, tc := range []struct {
		name    string
		state   string
		running bool
	}{
		{name: "a proven absence offers no installation", state: "not-installed"},
		{name: "a stopped registration offers no installation", state: "stopped"},
		{name: "a running registration hides it as before", state: "running", running: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := serviceview.From(tc.state, tc.running, false)
			stub := &runtimeServiceStub{snapshot: read}
			// No installation is registered beside the read.
			stubRuntimeService(t,
				func() (serviceview.Snapshot, error) { return stub.snapshot, nil }, nil)

			status := runtimeStatusOf(t, pipeline)

			if status.Service.Actions.Install != (serviceview.Action{}) {
				t.Errorf("the install action = %+v, want it withdrawn: this host registers no installation",
					status.Service.Actions.Install)
			}
			want := read
			want.Actions.Install = serviceview.Action{}
			if status.Service != wantWithStateCleared(want) {
				t.Errorf("service view = %+v, want the table's answer without the install action %+v",
					status.Service, wantWithStateCleared(want))
			}
			if status.ServiceState != tc.state {
				t.Errorf("serviceState = %q, want the raw word of the read %q", status.ServiceState, tc.state)
			}
			if stub.snapshot != read {
				t.Errorf("the read's snapshot became %+v, want the value the host returned %+v: "+
					"the withdrawal belongs to the copy of the response", stub.snapshot, read)
			}
		})
	}
}

// TestRuntimeStatusKeepsTheInstallActionOfAHostThatProvidesIt proves the
// other direction: a host that registered the installation keeps the table's
// answer — visible and enabled beside a proven absence, hidden beside a
// running service — and a state the read could not settle still offers
// nothing at all.
func TestRuntimeStatusKeepsTheInstallActionOfAHostThatProvidesIt(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 16)

	for _, tc := range []struct {
		name    string
		state   string
		running bool
		want    serviceview.Action
	}{
		{
			name:  "a proven absence offers the installation",
			state: "not-installed",
			want:  serviceview.Action{Visible: true, Enabled: true},
		},
		{
			name:    "a running service hides it",
			state:   "running",
			running: true,
			want:    serviceview.Action{},
		},
		{
			name:  "an unsettled read offers nothing",
			state: "unknown",
			want:  serviceview.Action{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubRuntimeService(t, func() (serviceview.Snapshot, error) {
				return serviceview.From(tc.state, tc.running, false), nil
			}, func() {})

			status := runtimeStatusOf(t, pipeline)

			if status.Service.Actions.Install != tc.want {
				t.Errorf("the install action = %+v, want the table's answer %+v",
					status.Service.Actions.Install, tc.want)
			}
			want := serviceview.From(tc.state, tc.running, false)
			if status.Service != wantWithStateCleared(want) {
				t.Errorf("service view = %+v, want the table's answer %+v",
					status.Service, wantWithStateCleared(want))
			}
			if status.ServiceState != tc.state {
				t.Errorf("serviceState = %q, want the raw word of the read %q", status.ServiceState, tc.state)
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
	stub := &runtimeServiceStub{snapshot: serviceview.From("not-installed", false, false)}
	stubRuntimeService(t,
		func() (serviceview.Snapshot, error) { return stub.snapshot, nil },
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
	stub := &runtimeServiceStub{snapshot: serviceview.From("not-installed", false, false)}
	stubRuntimeService(t,
		func() (serviceview.Snapshot, error) { return stub.snapshot, nil },
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
	stubRuntimeService(t, func() (serviceview.Snapshot, error) {
		return serviceview.From("running", true, false), nil
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
