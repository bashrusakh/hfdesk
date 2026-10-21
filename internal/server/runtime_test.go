// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/logging"
)

// newRuntimeTestPipeline builds a logging pipeline with a small ring and
// returns it with a logger bound to it.
//
// The pipeline is not installed as the process wide default: the server
// reads it through its pipeline provider, so a test does not have to change
// the process wide state. The standard library logger is silenced for the
// duration of the test, so the request log of the server does not end up in
// the test output.
func newRuntimeTestPipeline(t *testing.T, capacity int) (*logging.Pipeline, *slog.Logger) {
	t.Helper()
	pipeline, err := logging.New(logging.Options{Level: slog.LevelDebug, RingCapacity: capacity})
	if err != nil {
		t.Fatalf("build logging pipeline: %v", err)
	}
	silenceRequestLog(t)
	return pipeline, pipeline.Logger().With(logging.ComponentKey, "test")
}

// silenceRequestLog silences the standard library logger for the duration of
// the test.
func silenceRequestLog(t *testing.T) {
	t.Helper()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
}

// newRuntimeTestServer builds a server that reads its log records from the
// given pipeline.
func newRuntimeTestServer(pipeline *logging.Pipeline) *Server {
	return &Server{
		pipelineProvider: func() *logging.Pipeline { return pipeline },
	}
}

// runtimeRequest builds a request for the runtime endpoints with an explicit
// peer address.
//
// httptest.NewRequest uses 192.0.2.1:1234, the documentation address of
// TEST-NET-1, as the peer, so every allowed case has to set its own.
func runtimeRequest(method, target, remoteAddr string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = remoteAddr
	return r
}

// runtimeLogPaths are the routes of the runtime window.
var runtimeLogPaths = []string{
	"/api/runtime",
	"/api/runtime/logs",
	"/api/runtime/logs/stream",
}

// TestRuntimeEndpointsRejectNonLoopbackPeer proves that the runtime data of
// the local machine is not handed to a client that is not on the loopback
// interface: the request is rejected before any status or log data is
// produced.
func TestRuntimeEndpointsRejectNonLoopbackPeer(t *testing.T) {
	pipeline, logger := newRuntimeTestPipeline(t, 16)
	logger.Info("private record", "token", "value")
	handler := newRuntimeTestServer(pipeline).handler()

	for _, path := range runtimeLogPaths {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, runtimeRequest(http.MethodGet, path, "192.0.2.10:5555"))

			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
			}
			body := recorder.Body.String()
			for _, secret := range []string{"private record", "logCursor", "\"version\"", "\"records\"", "\"url\""} {
				if strings.Contains(body, secret) {
					t.Errorf("rejection body %q carries runtime data (%q)", body, secret)
				}
			}
		})
	}
}

// TestRuntimeEndpointsAllowLoopbackPeer proves that a client on the loopback
// interface is served.
func TestRuntimeEndpointsAllowLoopbackPeer(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 16)
	handler := newRuntimeTestServer(pipeline).handler()

	for _, path := range []string{"/api/runtime", "/api/runtime/logs"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, runtimeRequest(http.MethodGet, path, "127.0.0.1:5555"))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
		})
	}
}

// TestRuntimeOriginGuard proves the second half of the local-only rule: a
// browser page on another site cannot read the runtime data, even though the
// request it makes really does come from the loopback interface, and a
// forwarded header cannot be used to fake a loopback peer.
func TestRuntimeOriginGuard(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 16)
	handler := newRuntimeTestServer(pipeline).handler()

	cases := []struct {
		name       string
		path       string
		remoteAddr string
		origin     string
		forwarded  string
		want       int
	}{
		{
			name:       "loopback peer without origin",
			path:       "/api/runtime",
			remoteAddr: "127.0.0.1:5555",
			want:       http.StatusOK,
		},
		{
			name:       "loopback peer with loopback origin",
			path:       "/api/runtime",
			remoteAddr: "127.0.0.1:5555",
			origin:     "http://127.0.0.1:8080",
			want:       http.StatusOK,
		},
		{
			name:       "loopback peer with localhost origin",
			path:       "/api/runtime",
			remoteAddr: "127.0.0.1:5555",
			origin:     "http://localhost:8080",
			want:       http.StatusOK,
		},
		{
			name:       "loopback peer with cross origin",
			path:       "/api/runtime",
			remoteAddr: "127.0.0.1:5555",
			origin:     "https://evil.example",
			want:       http.StatusForbidden,
		},
		{
			name:       "loopback peer with opaque origin",
			path:       "/api/runtime",
			remoteAddr: "127.0.0.1:5555",
			origin:     "null",
			want:       http.StatusForbidden,
		},
		{
			name:       "public peer with spoofed forwarded header",
			path:       "/api/runtime",
			remoteAddr: "192.0.2.10:5555",
			forwarded:  "127.0.0.1",
			want:       http.StatusForbidden,
		},
		{
			name:       "public peer on the log page",
			path:       "/api/runtime/logs",
			remoteAddr: "192.0.2.10:5555",
			want:       http.StatusForbidden,
		},
		{
			name:       "cross origin on the log stream",
			path:       "/api/runtime/logs/stream",
			remoteAddr: "127.0.0.1:5555",
			origin:     "https://evil.example",
			want:       http.StatusForbidden,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runtimeRequest(http.MethodGet, c.path, c.remoteAddr)
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if c.forwarded != "" {
				r.Header.Set("X-Forwarded-For", c.forwarded)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, r)
			if recorder.Code != c.want {
				t.Fatalf("status = %d, want %d", recorder.Code, c.want)
			}
		})
	}
}

// TestRuntimeStatusResponse proves that the status endpoint reports the
// version, the status, the browser-safe URL, the effective bound address, the
// active job counts and the log cursor.
func TestRuntimeStatusResponse(t *testing.T) {
	pipeline, logger := newRuntimeTestPipeline(t, 16)
	logger.Info("hello")

	manager := newTestJobManager(t, Config{CacheDir: t.TempDir()}, nil)
	manager.mu.Lock()
	manager.jobs["running"] = &Job{ID: "running", Status: JobStatusRunning}
	manager.jobs["queued"] = &Job{ID: "queued", Status: JobStatusQueued}
	manager.mu.Unlock()

	s := newRuntimeTestServer(pipeline)
	s.jobs = manager
	s.setBoundAddr(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080})

	recorder := httptest.NewRecorder()
	s.handler().ServeHTTP(recorder, runtimeRequest(http.MethodGet, "/api/runtime", "127.0.0.1:5555"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode status payload %q: %v", recorder.Body.String(), err)
	}
	for _, key := range []string{"version", "status", "url", "address", "activeJobs", "logCursor"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("status payload %v does not carry %q", payload, key)
		}
	}

	var status RuntimeStatusResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if status.Version != Version {
		t.Errorf("version = %q, want %q", status.Version, Version)
	}
	if status.Status != "ok" {
		t.Errorf("status = %q, want %q", status.Status, "ok")
	}
	if want := "http://127.0.0.1:8080"; status.URL != want {
		t.Errorf("url = %q, want %q", status.URL, want)
	}
	if want := "127.0.0.1:8080"; status.Address != want {
		t.Errorf("address = %q, want %q", status.Address, want)
	}
	if status.ActiveJobs.Running != 1 || status.ActiveJobs.Queued != 1 {
		t.Errorf("active jobs = %+v, want 1 running and 1 queued", status.ActiveJobs)
	}
	if status.LogCursor != pipeline.Cursor() {
		t.Errorf("log cursor = %d, want %d", status.LogCursor, pipeline.Cursor())
	}
}

// TestRuntimeLogsCursor proves that a log page carries only the records
// emitted after the cursor of the client, and that it reports the cursor of
// the next page.
func TestRuntimeLogsCursor(t *testing.T) {
	pipeline, logger := newRuntimeTestPipeline(t, 16)
	logger.Info("first")
	logger.Info("second")
	logger.Info("third")

	handler := newRuntimeTestServer(pipeline).handler()

	page := runtimeLogsPage(t, handler, "127.0.0.1:5555", "after=0")
	if len(page.Records) != 3 {
		t.Fatalf("records = %d, want 3", len(page.Records))
	}
	for i, want := range []uint64{1, 2, 3} {
		if page.Records[i].Seq != want {
			t.Errorf("record %d has seq %d, want %d", i, page.Records[i].Seq, want)
		}
	}
	if page.Cursor != 3 {
		t.Errorf("cursor = %d, want 3", page.Cursor)
	}
	if page.Truncated {
		t.Error("truncated = true, want false for a window the ring still covers")
	}

	// The cursor of the previous page continues where it stopped.
	page = runtimeLogsPage(t, handler, "127.0.0.1:5555", "after=2")
	if len(page.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(page.Records))
	}
	if page.Records[0].Seq != 3 {
		t.Errorf("record has seq %d, want 3", page.Records[0].Seq)
	}
	if page.Cursor != 3 {
		t.Errorf("cursor = %d, want 3", page.Cursor)
	}

	// A cursor at the head returns an empty page and keeps the cursor.
	page = runtimeLogsPage(t, handler, "127.0.0.1:5555", "after=3")
	if len(page.Records) != 0 {
		t.Fatalf("records = %d, want 0", len(page.Records))
	}
	if page.Cursor != 3 {
		t.Errorf("cursor = %d, want 3", page.Cursor)
	}
}

// TestRuntimeLogsLevelFilter proves that the level parameter keeps only the
// records of the requested severity and above.
func TestRuntimeLogsLevelFilter(t *testing.T) {
	pipeline, logger := newRuntimeTestPipeline(t, 16)
	logger.Debug("debug record")
	logger.Info("info record")
	logger.Warn("warn record")
	logger.Error("error record")

	handler := newRuntimeTestServer(pipeline).handler()

	cases := []struct {
		query string
		want  []string
	}{
		{query: "", want: []string{"debug record", "info record", "warn record", "error record"}},
		{query: "&level=all", want: []string{"debug record", "info record", "warn record", "error record"}},
		{query: "&level=debug", want: []string{"debug record", "info record", "warn record", "error record"}},
		{query: "&level=info", want: []string{"info record", "warn record", "error record"}},
		{query: "&level=warn", want: []string{"warn record", "error record"}},
		{query: "&level=error", want: []string{"error record"}},
	}
	for _, c := range cases {
		t.Run("level="+c.query, func(t *testing.T) {
			page := runtimeLogsPage(t, handler, "127.0.0.1:5555", "after=0"+c.query)
			if len(page.Records) != len(c.want) {
				t.Fatalf("records = %d, want %d", len(page.Records), len(c.want))
			}
			for i, want := range c.want {
				if page.Records[i].Message != want {
					t.Errorf("record %d = %q, want %q", i, page.Records[i].Message, want)
				}
			}
		})
	}
}

// TestRuntimeLogsLimit proves that the limit parameter caps the size of a
// page and that the reported cursor continues after the page.
func TestRuntimeLogsLimit(t *testing.T) {
	pipeline, logger := newRuntimeTestPipeline(t, 16)
	logger.Info("first")
	logger.Info("second")
	logger.Info("third")

	handler := newRuntimeTestServer(pipeline).handler()

	page := runtimeLogsPage(t, handler, "127.0.0.1:5555", "after=0&limit=2")
	if len(page.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(page.Records))
	}
	if page.Records[0].Message != "first" || page.Records[1].Message != "second" {
		t.Errorf("records = %q, %q, want first and second",
			page.Records[0].Message, page.Records[1].Message)
	}
	if page.Cursor != 2 {
		t.Errorf("cursor = %d, want 2", page.Cursor)
	}

	// The rest of the window follows on the next page.
	page = runtimeLogsPage(t, handler, "127.0.0.1:5555", "after=2")
	if len(page.Records) != 1 || page.Records[0].Message != "third" {
		t.Fatalf("records = %+v, want the third record", page.Records)
	}
}

// TestRuntimeLogsTruncation proves that the endpoint reports that the
// requested window fell out of the bounded ring, so the client does not
// assume that the page continues its history.
func TestRuntimeLogsTruncation(t *testing.T) {
	pipeline, logger := newRuntimeTestPipeline(t, 2)
	logger.Info("first")
	logger.Info("second")
	logger.Info("third")
	logger.Info("fourth")

	handler := newRuntimeTestServer(pipeline).handler()

	page := runtimeLogsPage(t, handler, "127.0.0.1:5555", "after=0")
	if !page.Truncated {
		t.Error("truncated = false, want true for a window the ring no longer covers")
	}
	if len(page.Records) != 2 {
		t.Fatalf("records = %d, want the 2 retained records", len(page.Records))
	}
	if page.Records[0].Message != "third" || page.Records[1].Message != "fourth" {
		t.Errorf("records = %q, %q, want third and fourth",
			page.Records[0].Message, page.Records[1].Message)
	}

	// A cursor the ring still covers is not truncated.
	page = runtimeLogsPage(t, handler, "127.0.0.1:5555", "after=3")
	if page.Truncated {
		t.Error("truncated = true, want false for a window the ring covers")
	}
}

// TestRuntimeLogsRecordShape proves the wire shape of a record: the level is
// a name and not a number, the attributes are an object and the timestamp is
// RFC3339.
func TestRuntimeLogsRecordShape(t *testing.T) {
	pipeline, logger := newRuntimeTestPipeline(t, 16)
	logger.Warn("shaped record", "count", 3, "nested", map[string]any{"key": "value"})

	handler := newRuntimeTestServer(pipeline).handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, runtimeRequest(http.MethodGet, "/api/runtime/logs?after=0", "127.0.0.1:5555"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var payload struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode log payload %q: %v", recorder.Body.String(), err)
	}
	if len(payload.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(payload.Records))
	}
	record := payload.Records[0]
	if record["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", record["level"])
	}
	if record["component"] != "test" {
		t.Errorf("component = %v, want test", record["component"])
	}
	if record["message"] != "shaped record" {
		t.Errorf("message = %v, want shaped record", record["message"])
	}
	if _, ok := record["seq"]; !ok {
		t.Errorf("record %v does not carry a sequence number", record)
	}
	attrs, ok := record["attrs"].(map[string]any)
	if !ok {
		t.Fatalf("attrs = %v, want an object", record["attrs"])
	}
	if attrs["count"] != float64(3) {
		t.Errorf("attrs.count = %v, want 3", attrs["count"])
	}
	timestamp, ok := record["time"].(string)
	if !ok {
		t.Fatalf("time = %v, want a string", record["time"])
	}
	if _, err := time.Parse(time.RFC3339, timestamp); err != nil {
		t.Errorf("time %q is not RFC3339: %v", timestamp, err)
	}
}

// TestRuntimeLogQueryParsing proves the parsing of the query parameters,
// including the upper bound of the page size.
func TestRuntimeLogQueryParsing(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantCursor uint64
		wantLimit  int
		wantLevel  string
		wantErr    bool
	}{
		{name: "defaults", query: "", wantCursor: 0, wantLimit: runtimeLogsDefaultLimit},
		{name: "cursor", query: "after=42", wantCursor: 42, wantLimit: runtimeLogsDefaultLimit},
		{name: "limit", query: "limit=7", wantLimit: 7},
		{name: "limit above the maximum is clamped", query: "limit=100000", wantLimit: runtimeLogsMaxLimit},
		{name: "level all", query: "level=all", wantLimit: runtimeLogsDefaultLimit},
		{name: "level warn", query: "level=warn", wantLevel: "WARN", wantLimit: runtimeLogsDefaultLimit},
		{name: "level warning", query: "level=warning", wantLevel: "WARN", wantLimit: runtimeLogsDefaultLimit},
		{name: "invalid cursor", query: "after=abc", wantErr: true},
		{name: "negative cursor", query: "after=-1", wantErr: true},
		{name: "invalid level", query: "level=loud", wantErr: true},
		{name: "invalid limit", query: "limit=abc", wantErr: true},
		{name: "zero limit", query: "limit=0", wantErr: true},
		{name: "negative limit", query: "limit=-5", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/runtime/logs?"+c.query, nil)
			query, err := parseRuntimeLogQuery(r)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseRuntimeLogQuery(%q) = %+v, want an error", c.query, query)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRuntimeLogQuery(%q): %v", c.query, err)
			}
			if query.after != c.wantCursor {
				t.Errorf("after = %d, want %d", query.after, c.wantCursor)
			}
			if query.limit != c.wantLimit {
				t.Errorf("limit = %d, want %d", query.limit, c.wantLimit)
			}
			if c.wantLevel == "" {
				if query.filtered {
					t.Errorf("filter = %v, want no filter", query.filtered)
				}
				return
			}
			if !query.filtered {
				t.Fatalf("filter = false, want a filter for level %q", c.wantLevel)
			}
			if got := runtimeLogLevelName(query.level); got != c.wantLevel {
				t.Errorf("level = %q, want %q", got, c.wantLevel)
			}
		})
	}
}

// TestRuntimeEndpointsWithoutPipeline proves that the server works before
// the logging pipeline is installed: the status endpoint still answers and
// the log endpoint returns an empty page instead of failing.
func TestRuntimeEndpointsWithoutPipeline(t *testing.T) {
	silenceRequestLog(t)

	if pipeline := logging.Default(); pipeline != nil {
		t.Fatalf("a logging pipeline is installed process wide (%p), the case is not covered", pipeline)
	}

	// The default provider reads the process wide default, which is nil
	// here, and an explicit nil provider covers the same path.
	for name, s := range map[string]*Server{
		"default provider": {},
		"explicit nil":     {pipelineProvider: func() *logging.Pipeline { return nil }},
	} {
		t.Run(name, func(t *testing.T) {
			handler := s.handler()

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, runtimeRequest(http.MethodGet, "/api/runtime", "127.0.0.1:5555"))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			var status RuntimeStatusResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
				t.Fatalf("decode status response: %v", err)
			}
			if status.LogCursor != 0 {
				t.Errorf("log cursor = %d, want 0", status.LogCursor)
			}

			recorder = httptest.NewRecorder()
			handler.ServeHTTP(recorder, runtimeRequest(http.MethodGet, "/api/runtime/logs", "127.0.0.1:5555"))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			var page RuntimeLogsResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
				t.Fatalf("decode log response: %v", err)
			}
			if len(page.Records) != 0 {
				t.Errorf("records = %d, want 0", len(page.Records))
			}
			if page.Cursor != 0 {
				t.Errorf("cursor = %d, want 0", page.Cursor)
			}
			if page.Truncated {
				t.Error("truncated = true, want false")
			}
		})
	}
}

// TestRuntimeLogStream proves the log stream: it serves Server-Sent Events,
// replays the retained records, delivers a record that is emitted after the
// stream started, and ends and releases its subscription when the client
// goes away.
func TestRuntimeLogStream(t *testing.T) {
	pipeline, logger := newRuntimeTestPipeline(t, 32)
	logger.Info("replayed record")

	s := newRuntimeTestServer(pipeline)
	server := httptest.NewServer(s.handler())
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/runtime/logs/stream?after=0", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open log stream: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("content type = %q, want %q", got, "text/event-stream")
	}
	if got := response.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("cache control = %q, want %q", got, "no-cache")
	}

	reader := bufio.NewReader(response.Body)

	// The retained records are replayed first.
	event := readRuntimeLogEvent(t, reader)
	if event.Name != "log" {
		t.Fatalf("event = %q, want a log event", event.Name)
	}
	var replayed RuntimeLogRecord
	if err := json.Unmarshal([]byte(event.Data), &replayed); err != nil {
		t.Fatalf("decode replayed record %q: %v", event.Data, err)
	}
	if replayed.Message != "replayed record" {
		t.Errorf("replayed message = %q, want %q", replayed.Message, "replayed record")
	}
	if replayed.Level != "INFO" {
		t.Errorf("replayed level = %q, want INFO", replayed.Level)
	}

	// A record emitted after the stream started is delivered. The
	// subscription exists before the response headers are written, so the
	// record cannot slip through.
	logger.Info("live record", "count", 1)
	event = readRuntimeLogEvent(t, reader)
	if event.Name != "log" {
		t.Fatalf("event = %q, want a log event", event.Name)
	}
	var live RuntimeLogRecord
	if err := json.Unmarshal([]byte(event.Data), &live); err != nil {
		t.Fatalf("decode live record %q: %v", event.Data, err)
	}
	if live.Message != "live record" {
		t.Errorf("live message = %q, want %q", live.Message, "live record")
	}
	if live.Seq <= replayed.Seq {
		t.Errorf("live seq = %d, want a sequence number after %d", live.Seq, replayed.Seq)
	}
	if live.Attrs["count"] != float64(1) {
		t.Errorf("live attrs = %v, want count 1", live.Attrs)
	}

	// The client goes away: the stream ends and the subscription is
	// released.
	cancel()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_, _ = io.Copy(io.Discard, response.Body)
	}()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream did not end after the client went away")
	}
	waitForRuntimeLogStreamIdle(t)
}

// TestRuntimeLogStreamReportsGap proves that a stream whose replay window
// fell out of the bounded ring tells the client that it has to re-sync
// instead of silently showing a history with a hole in it.
func TestRuntimeLogStreamReportsGap(t *testing.T) {
	pipeline, logger := newRuntimeTestPipeline(t, 2)
	logger.Info("first")
	logger.Info("second")
	logger.Info("third")
	logger.Info("fourth")

	s := newRuntimeTestServer(pipeline)
	server := httptest.NewServer(s.handler())
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/runtime/logs/stream?after=0", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open log stream: %v", err)
	}
	defer response.Body.Close()

	reader := bufio.NewReader(response.Body)
	for {
		event := readRuntimeLogEvent(t, reader)
		if event.Name != "gap" {
			continue
		}
		var resync RuntimeLogResync
		if err := json.Unmarshal([]byte(event.Data), &resync); err != nil {
			t.Fatalf("decode gap event %q: %v", event.Data, err)
		}
		if resync.Cursor != pipeline.Cursor() {
			t.Errorf("gap cursor = %d, want %d", resync.Cursor, pipeline.Cursor())
		}
		return
	}
}

// TestRuntimeLogStreamSurvivesServerWriteTimeout proves that the long lived
// stream is not cut off by the write deadline of the server: the handler
// clears the deadline, so a record that is emitted long after the response
// headers were written still reaches the client.
func TestRuntimeLogStreamSurvivesServerWriteTimeout(t *testing.T) {
	pipeline, logger := newRuntimeTestPipeline(t, 32)

	s := newRuntimeTestServer(pipeline)
	server := httptest.NewUnstartedServer(s.handler())
	// A write timeout that is shorter than the wait below.
	server.Config.WriteTimeout = 300 * time.Millisecond
	server.Start()
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/runtime/logs/stream?after=0", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open log stream: %v", err)
	}
	defer response.Body.Close()

	reader := bufio.NewReader(response.Body)

	// Wait past the write timeout of the server.
	time.Sleep(600 * time.Millisecond)
	logger.Info("late record")

	event := readRuntimeLogEvent(t, reader)
	if event.Name != "log" {
		t.Fatalf("event = %q, want a log event", event.Name)
	}
	var late RuntimeLogRecord
	if err := json.Unmarshal([]byte(event.Data), &late); err != nil {
		t.Fatalf("decode late record %q: %v", event.Data, err)
	}
	if late.Message != "late record" {
		t.Errorf("late message = %q, want %q", late.Message, "late record")
	}
}

// runtimeLogsPage reads a log page from the handler and decodes it.
func runtimeLogsPage(t *testing.T, handler http.Handler, remoteAddr, query string) RuntimeLogsResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, runtimeRequest(http.MethodGet, "/api/runtime/logs?"+query, remoteAddr))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var page RuntimeLogsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode log payload %q: %v", recorder.Body.String(), err)
	}
	return page
}

// runtimeLogEvent is a Server-Sent Event read from a log stream.
type runtimeLogEvent struct {
	// Name is the event name.
	Name string
	// Data is the payload of the event.
	Data string
}

// readRuntimeLogEvent reads the next event of a Server-Sent Events stream.
//
// The read happens in a helper goroutine, so a stream that stays silent
// fails the test instead of hanging it.
func readRuntimeLogEvent(t *testing.T, reader *bufio.Reader) runtimeLogEvent {
	t.Helper()
	type outcome struct {
		event runtimeLogEvent
		err   error
	}
	outcomes := make(chan outcome, 1)
	go func() {
		event, err := readRuntimeLogEventFrom(reader)
		outcomes <- outcome{event: event, err: err}
	}()
	select {
	case result := <-outcomes:
		if result.err != nil {
			t.Fatalf("read stream event: %v", result.err)
		}
		return result.event
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a stream event")
		return runtimeLogEvent{}
	}
}

// readRuntimeLogEventFrom reads the next event of a Server-Sent Events
// stream.
func readRuntimeLogEventFrom(reader *bufio.Reader) (runtimeLogEvent, error) {
	event := runtimeLogEvent{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return event, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if event.Name != "" || event.Data != "" {
				return event, nil
			}
		case strings.HasPrefix(line, ":"):
			// A keep-alive comment.
		case strings.HasPrefix(line, "event:"):
			event.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			event.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

// waitForRuntimeLogStreamIdle waits until no log stream is open.
//
// The counter is decremented after the subscription of the stream was
// released, so a counter of zero means that no subscription is left behind.
func waitForRuntimeLogStreamIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		active := runtimeLogStreamActive.Load()
		if active == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d log streams are still open", active)
		}
		time.Sleep(time.Millisecond)
	}
}
