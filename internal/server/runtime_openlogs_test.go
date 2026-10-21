// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/logging"
)

// newRuntimeFilePipeline builds a logging pipeline with a file sink and
// returns it with the path of its log file.
func newRuntimeFilePipeline(t *testing.T) (*logging.Pipeline, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "hfdesk.log")
	pipeline, err := logging.New(logging.Options{File: true, FilePath: path})
	if err != nil {
		t.Fatalf("build logging pipeline: %v", err)
	}
	t.Cleanup(func() { _ = pipeline.Close() })
	silenceRequestLog(t)
	return pipeline, path
}

// openLogDirectoryStub records the folders the open-logs action opened.
type openLogDirectoryStub struct {
	paths []string
	err   error
}

// open records the folder and reports the configured failure.
func (s *openLogDirectoryStub) open(path string) error {
	s.paths = append(s.paths, path)
	return s.err
}

// stubOpenLogDirectory replaces the opener of the log folder for the
// duration of the test, so a test never starts a file manager.
func stubOpenLogDirectory(t *testing.T, err error) *openLogDirectoryStub {
	t.Helper()

	stub := &openLogDirectoryStub{err: err}
	previous := openLogDirectory
	openLogDirectory = stub.open
	t.Cleanup(func() { openLogDirectory = previous })
	return stub
}

// runtimeOpenLogsRequest builds the request of the open-logs action with an
// explicit peer address, an optional origin and an optional body.
func runtimeOpenLogsRequest(remoteAddr, origin, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/runtime/open-logs", strings.NewReader(body))
	r.RemoteAddr = remoteAddr
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

// TestRuntimeStatusReportsTheLogFile proves that the status endpoint reports
// the path of the active log file, so the Runtime window can show where the
// log is written.
func TestRuntimeStatusReportsTheLogFile(t *testing.T) {
	pipeline, path := newRuntimeFilePipeline(t)
	s := newRuntimeTestServer(pipeline)

	recorder := httptest.NewRecorder()
	s.handler().ServeHTTP(recorder, runtimeRequest(http.MethodGet, "/api/runtime", "127.0.0.1:5555"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode status payload %q: %v", recorder.Body.String(), err)
	}
	if _, ok := payload["logFile"]; !ok {
		t.Errorf("status payload %v does not carry %q", payload, "logFile")
	}

	var status RuntimeStatusResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if status.LogFile != path {
		t.Errorf("log file = %q, want %q", status.LogFile, path)
	}
}

// TestRuntimeStatusReportsNoLogFileWithoutAFileSink proves that an
// application without a log file reports an empty path instead of a
// fabricated one.
func TestRuntimeStatusReportsNoLogFileWithoutAFileSink(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 16)
	s := newRuntimeTestServer(pipeline)

	recorder := httptest.NewRecorder()
	s.handler().ServeHTTP(recorder, runtimeRequest(http.MethodGet, "/api/runtime", "127.0.0.1:5555"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var status RuntimeStatusResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if status.LogFile != "" {
		t.Errorf("log file = %q, want an empty path", status.LogFile)
	}
}

// TestRuntimeOpenLogsRejectsNonLocalClients proves that the open-logs action
// is guarded by the same local-only rule as the other runtime endpoints: a
// client that is not on the loopback interface, and a page of another site
// that borrows the loopback interface, are both rejected, and neither of them
// opens a folder.
func TestRuntimeOpenLogsRejectsNonLocalClients(t *testing.T) {
	pipeline, _ := newRuntimeFilePipeline(t)
	stub := stubOpenLogDirectory(t, nil)
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
			handler.ServeHTTP(recorder, runtimeOpenLogsRequest(c.remoteAddr, c.origin, ""))
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
			}
		})
	}

	if len(stub.paths) != 0 {
		t.Errorf("the rejected requests opened %v", stub.paths)
	}
}

// TestRuntimeOpenLogsWithoutALogFile proves that the action reports that
// there is no log file instead of opening anything.
func TestRuntimeOpenLogsWithoutALogFile(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 16)
	stub := stubOpenLogDirectory(t, nil)
	handler := newRuntimeTestServer(pipeline).handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, runtimeOpenLogsRequest("127.0.0.1:5555", "", ""))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if len(stub.paths) != 0 {
		t.Errorf("the request opened %v", stub.paths)
	}

	var payload ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error payload %q: %v", recorder.Body.String(), err)
	}
	if payload.Error == "" {
		t.Errorf("error payload %v does not explain the refusal", payload)
	}
}

// TestRuntimeOpenLogsOpensTheLogFolder proves that the action opens the
// folder of the active log file, and that it opens nothing else: a path in
// the query or in the body is ignored.
func TestRuntimeOpenLogsOpensTheLogFolder(t *testing.T) {
	pipeline, path := newRuntimeFilePipeline(t)
	stub := stubOpenLogDirectory(t, nil)
	handler := newRuntimeTestServer(pipeline).handler()

	request := runtimeOpenLogsRequest("127.0.0.1:5555", "", `{"path":"/etc"}`)
	request.URL.RawQuery = "path=/etc"

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	want := filepath.Dir(path)
	if len(stub.paths) != 1 || stub.paths[0] != want {
		t.Errorf("opened = %v, want the log folder %q", stub.paths, want)
	}

	var payload RuntimeOpenLogsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode open-logs response %q: %v", recorder.Body.String(), err)
	}
	if !payload.OK {
		t.Error("the response does not report the success")
	}
	if payload.Path != want {
		t.Errorf("path = %q, want %q", payload.Path, want)
	}
}

// TestRuntimeOpenLogsReportsAnOpenerFailure proves that a failing opener is
// reported as a server error with a short explanation, and that the internal
// failure is not handed to the client.
func TestRuntimeOpenLogsReportsAnOpenerFailure(t *testing.T) {
	pipeline, _ := newRuntimeFilePipeline(t)
	stub := stubOpenLogDirectory(t, errors.New("the file manager is missing"))
	handler := newRuntimeTestServer(pipeline).handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, runtimeOpenLogsRequest("127.0.0.1:5555", "", ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}

	body := recorder.Body.String()
	for _, secret := range []string{"the file manager is missing", "goroutine", ".go:"} {
		if strings.Contains(body, secret) {
			t.Errorf("the failure body %q leaks the internal failure (%q)", body, secret)
		}
	}
	if !strings.Contains(body, "Could not open the log folder") {
		t.Errorf("the failure body %q does not explain the failure", body)
	}
	if len(stub.paths) != 1 {
		t.Errorf("the opener was called %d times, want once", len(stub.paths))
	}
}
