// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/logging"
)

// openBrowserURLStub records the URLs the open-ui action opened.
type openBrowserURLStub struct {
	urls []string
	err  error
}

// open records the URL and reports the configured failure.
func (s *openBrowserURLStub) open(url string) error {
	s.urls = append(s.urls, url)
	return s.err
}

// stubOpenBrowserURL replaces the opener of the web UI for the duration of
// the test, so a test never starts a browser.
func stubOpenBrowserURL(t *testing.T, err error) *openBrowserURLStub {
	t.Helper()

	stub := &openBrowserURLStub{err: err}
	previous := openBrowserURL
	openBrowserURL = stub.open
	t.Cleanup(func() { openBrowserURL = previous })
	return stub
}

// newRuntimeUIServer builds a server that is bound to the loopback
// interface, so the web UI of the server has a real URL.
func newRuntimeUIServer(t *testing.T) *Server {
	t.Helper()

	pipeline, _ := newRuntimeTestPipeline(t, 16)
	s := &Server{
		config:           Config{Addr: "127.0.0.1", Port: 8080},
		pipelineProvider: func() *logging.Pipeline { return pipeline },
	}
	s.setBoundAddr(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080})
	return s
}

// runtimeOpenUIRequest builds the request of the open-ui action with an
// explicit peer address, an optional origin and an optional body.
func runtimeOpenUIRequest(remoteAddr, origin, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/runtime/open-ui", strings.NewReader(body))
	r.RemoteAddr = remoteAddr
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

// TestRuntimeOpenUIRejectsNonLocalClients proves that the open-ui action is
// guarded by the same local-only rule as the other runtime endpoints: a
// client that is not on the loopback interface, and a page of another site
// that borrows the loopback interface, are both rejected, and neither of
// them opens a browser.
func TestRuntimeOpenUIRejectsNonLocalClients(t *testing.T) {
	stub := stubOpenBrowserURL(t, nil)
	handler := newRuntimeUIServer(t).handler()

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
			handler.ServeHTTP(recorder, runtimeOpenUIRequest(c.remoteAddr, c.origin, ""))
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
			}
		})
	}

	if len(stub.urls) != 0 {
		t.Errorf("the rejected requests opened %v", stub.urls)
	}
}

// TestRuntimeOpenUIOpensTheServerURL proves that the action opens the URL of
// the running server, and that it opens nothing else: a URL in the query or
// in the body is ignored.
func TestRuntimeOpenUIOpensTheServerURL(t *testing.T) {
	stub := stubOpenBrowserURL(t, nil)
	handler := newRuntimeUIServer(t).handler()

	request := runtimeOpenUIRequest("127.0.0.1:5555", "", `{"url":"https://evil.example"}`)
	request.URL.RawQuery = "url=https://evil.example"

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	want := "http://127.0.0.1:8080"
	if len(stub.urls) != 1 || stub.urls[0] != want {
		t.Errorf("opened = %v, want the web UI of the server %q", stub.urls, want)
	}

	var payload RuntimeOpenUIResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode open-ui response %q: %v", recorder.Body.String(), err)
	}
	if !payload.OK {
		t.Error("the response does not report the success")
	}
}

// TestRuntimeOpenUIReportsAnOpenerFailure proves that a failing opener is
// reported as a server error with a short explanation, and that the internal
// failure is not handed to the client.
func TestRuntimeOpenUIReportsAnOpenerFailure(t *testing.T) {
	stub := stubOpenBrowserURL(t, errors.New("the browser is missing"))
	handler := newRuntimeUIServer(t).handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, runtimeOpenUIRequest("127.0.0.1:5555", "", ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}

	body := recorder.Body.String()
	for _, secret := range []string{"the browser is missing", "goroutine", ".go:"} {
		if strings.Contains(body, secret) {
			t.Errorf("the failure body %q leaks the internal failure (%q)", body, secret)
		}
	}
	if !strings.Contains(body, "Could not open the web UI") {
		t.Errorf("the failure body %q does not explain the failure", body)
	}
	if len(stub.urls) != 1 {
		t.Errorf("the opener was called %d times, want once", len(stub.urls))
	}
}

// TestRuntimeOpenUIWithoutABoundServer proves that the action reports that
// there is no web UI instead of opening a broken URL.
func TestRuntimeOpenUIWithoutABoundServer(t *testing.T) {
	stub := stubOpenBrowserURL(t, nil)
	pipeline, _ := newRuntimeTestPipeline(t, 16)
	s := &Server{pipelineProvider: func() *logging.Pipeline { return pipeline }}
	handler := s.handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, runtimeOpenUIRequest("127.0.0.1:5555", "", ""))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if len(stub.urls) != 0 {
		t.Errorf("the request opened %v", stub.urls)
	}
}
