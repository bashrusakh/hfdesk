// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/logging"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// capturedLog is a concurrency-safe buffer that collects what a logging
// pipeline writes to its console sink.
type capturedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capturedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capturedLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// installCapturingPipeline installs a logging pipeline whose console sink
// writes into an in-memory buffer and restores the previous default logger
// when the test ends.
//
// The pipeline is installed process wide because that is how the application
// installs it; the server package itself never installs one.
func installCapturingPipeline(t *testing.T) *capturedLog {
	t.Helper()
	captured := &capturedLog{}
	pipeline, err := logging.New(logging.Options{Console: true, ConsoleWriter: captured})
	if err != nil {
		t.Fatalf("build logging pipeline: %v", err)
	}
	previousLogger := slog.Default()
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	slog.SetDefault(pipeline.Logger())
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	return captured
}

// TestServeUsesCallerListener proves the Serve seam: the server answers
// requests on the listener the caller provided, reports the real bound
// address (including the OS-assigned port of a port 0 listener) and still
// quiesces and closes the job manager when the context is cancelled.
func TestServeUsesCallerListener(t *testing.T) {
	m := newJobManagerWithStatePath(Config{CacheDir: t.TempDir()}, nil, filepath.Join(t.TempDir(), "jobs.json"))
	registerTestJobManagerCleanup(t, m)
	s := &Server{jobs: m}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.Serve(ctx, listener) }()

	waitForServerBoundAddr(t, s, listener.Addr().String())

	resp, err := http.Get("http://" + listener.Addr().String() + "/api/health")
	if err != nil {
		t.Fatalf("health request failed: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read health response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var health map[string]any
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("decode health payload %q: %v", body, err)
	}
	if health["status"] != "ok" {
		t.Fatalf("health payload = %v", health)
	}

	// A listener bound to port 0 reports the OS-assigned port.
	_, port := splitAddrHostPort(s.BoundAddr())
	if port == 0 {
		t.Fatal("bound address carries no port")
	}
	if want := "http://127.0.0.1:" + strconv.Itoa(port); s.BrowserURL() != want {
		t.Errorf("BrowserURL() = %q, want %q", s.BrowserURL(), want)
	}

	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
	waitForManagerFence(t, m)
	select {
	case <-m.closeDone:
	default:
		t.Fatal("Serve returned before the job manager was closed")
	}
}

// TestServeBrowserURLUsesLoopbackForWildcardBind proves that a wildcard bind
// yields a browser-safe loopback URL with the effective port.
//
// The listener of the IPv4 wildcard is a dual-stack socket that reports the IPv6
// wildcard on some platforms. The server is configured with no address of its
// own, so the wildcard is the empty one and the URL names 127.0.0.1, which the
// dual-stack listener accepts.
func TestServeBrowserURLUsesLoopbackForWildcardBind(t *testing.T) {
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "jobs.json"))
	registerTestJobManagerCleanup(t, m)
	s := &Server{jobs: m}

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.Serve(ctx, listener) }()

	waitForServerBoundAddr(t, s, listener.Addr().String())

	_, port := splitAddrHostPort(s.BoundAddr())
	if port == 0 {
		t.Fatal("bound address carries no port")
	}
	want := "http://127.0.0.1:" + strconv.Itoa(port)
	if got := s.BrowserURL(); got != want {
		t.Errorf("BrowserURL() = %q, want %q for the bound address %s", got, want, s.BoundAddr())
	}

	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}

// TestStartupRecordUsesTheBrowserURL proves that the startup record spells
// the dashboard URL the way the application opens it and the runtime API
// reports it, so the product has exactly one spelling of the dashboard URL.
//
// The record used to spell the URL as http://localhost:<port>, which is not
// the address a user reaches when the server binds a routable address.
func TestStartupRecordUsesTheBrowserURL(t *testing.T) {
	t.Run("the address of the running server", func(t *testing.T) {
		captured := installCapturingPipeline(t)

		m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "jobs.json"))
		registerTestJobManagerCleanup(t, m)
		s := &Server{jobs: m}

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		serveDone := make(chan error, 1)
		go func() { serveDone <- s.Serve(ctx, listener) }()

		waitForServerBoundAddr(t, s, listener.Addr().String())

		_, port := splitAddrHostPort(s.BoundAddr())
		want := "http://127.0.0.1:" + strconv.Itoa(port)
		record := waitForLogRecord(t, captured, "server starting")

		if !strings.Contains(record, "dashboard="+want) {
			t.Errorf("startup record = %q, want the dashboard URL %q", record, want)
		}
		if !strings.Contains(record, "api="+want+"/api") {
			t.Errorf("startup record = %q, want the API URL %q", record, want+"/api")
		}
		if !strings.Contains(record, "addr=http://"+listener.Addr().String()) {
			t.Errorf("startup record = %q, want the listening address", record)
		}
		if strings.Contains(record, "localhost") {
			t.Errorf("startup record = %q, want no second spelling of the dashboard URL", record)
		}

		cancel()
		select {
		case err := <-serveDone:
			if err != nil {
				t.Fatalf("Serve returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Serve did not return after the context was cancelled")
		}
	})

	t.Run("a routable bound address", func(t *testing.T) {
		captured := installCapturingPipeline(t)

		// The server records the address it is bound to before it writes
		// the startup record, so the record and the browser URL are built
		// from the same address.
		s := &Server{config: Config{AuthUser: "tester"}}
		addr := &net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 8080}
		s.setBoundAddr(addr)

		s.logStartup(addr)

		want := "http://192.168.1.5:8080"
		record := waitForLogRecord(t, captured, "server starting")

		if !strings.Contains(record, "dashboard="+want) {
			t.Errorf("startup record = %q, want the dashboard URL %q", record, want)
		}
		if !strings.Contains(record, "api="+want+"/api") {
			t.Errorf("startup record = %q, want the API URL %q", record, want+"/api")
		}
		if !strings.Contains(record, "addr=http://"+addr.String()) {
			t.Errorf("startup record = %q, want the listening address", record)
		}
		if strings.Contains(record, "localhost") {
			t.Errorf("startup record = %q, want no second spelling of the dashboard URL", record)
		}

		auth := waitForLogRecord(t, captured, "auth enabled")
		if !strings.Contains(auth, "user=tester") {
			t.Errorf("auth record = %q, want the configured user", auth)
		}
	})
}

// waitForLogRecord waits until the captured log holds a record with the given
// message and returns the line of that record.
//
// The server writes its records from its own goroutines, so the record of the
// startup is not there when the caller asks for it.
func waitForLogRecord(t *testing.T, captured *capturedLog, message string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, line := range strings.Split(captured.String(), "\n") {
			if strings.Contains(line, message) {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q record was written to the log: %q", message, captured.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// TestBrowserURLDerivation covers the host-to-URL derivation of BrowserURL.
func TestBrowserURLDerivation(t *testing.T) {
	for _, tc := range []struct {
		name string
		host string
		port int
		want string
	}{
		{name: "wildcard ipv4", host: "0.0.0.0", port: 8080, want: "http://127.0.0.1:8080"},
		{name: "wildcard ipv6", host: "::", port: 8080, want: "http://[::1]:8080"},
		{name: "wildcard ipv6 in its long spelling", host: "0:0:0:0:0:0:0:0", port: 8080, want: "http://[::1]:8080"},
		{name: "empty host", host: "", port: 8080, want: "http://127.0.0.1:8080"},
		{name: "loopback", host: "127.0.0.1", port: 8080, want: "http://127.0.0.1:8080"},
		{name: "lan address", host: "192.168.1.5", port: 8080, want: "http://192.168.1.5:8080"},
		{name: "ipv6 loopback", host: "::1", port: 8080, want: "http://[::1]:8080"},
		{name: "assigned port", host: "0.0.0.0", port: 34567, want: "http://127.0.0.1:34567"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := browserURLFor(tc.host, tc.port); got != tc.want {
				t.Errorf("browserURLFor(%q, %d) = %q, want %q", tc.host, tc.port, got, tc.want)
			}
		})
	}
}

// TestBrowserURLFollowsTheConfiguredWildcardFamily proves that the loopback
// address of a wildcard bind follows the configured address and not the socket
// the platform chose.
//
// The listener of the IPv4 wildcard is a dual-stack socket that reports the IPv6
// wildcard on some platforms, so a URL derived from the bound spelling alone
// would name ::1 on a machine whose IPv6 loopback is disabled, where only
// 127.0.0.1 reaches the listener. The operator asked for the IPv4 wildcard, so
// the URL names 127.0.0.1; a configuration of the IPv6 wildcard names ::1, which
// an IPv6-only listener needs. A concrete bound host is never replaced, and a
// configuration without a wildcard leaves the bound spelling in place.
func TestBrowserURLFollowsTheConfiguredWildcardFamily(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		bound      string
		want       string
	}{
		{name: "the IPv4 wildcard that the platform reports as dual-stack", configured: "0.0.0.0", bound: "::", want: "http://127.0.0.1:8080"},
		{name: "the IPv6 wildcard of the configuration", configured: "::", bound: "::", want: "http://[::1]:8080"},
		{name: "no configured address", configured: "", bound: "::", want: "http://127.0.0.1:8080"},
		{name: "a concrete bound host", configured: "::", bound: "192.168.1.5", want: "http://192.168.1.5:8080"},
		{name: "a concrete configured host that is not wildcard", configured: "localhost", bound: "::", want: "http://[::1]:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{config: Config{Addr: tc.configured}}
			s.setBoundAddr(&net.TCPAddr{IP: net.ParseIP(tc.bound), Port: 8080})
			if got := s.BrowserURL(); got != tc.want {
				t.Errorf("BrowserURL() = %q, want %q for the bound host %q of the configured address %q",
					got, tc.want, tc.bound, tc.configured)
			}
		})
	}
}

// TestActiveJobCounts proves the running/queued split: a job that was
// dispatched but is not marked running yet occupies an active slot, exactly
// like in the dispatcher.
func TestActiveJobCounts(t *testing.T) {
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "jobs.json"))
	registerTestJobManagerCleanup(t, m)
	s := &Server{jobs: m}

	m.mu.Lock()
	m.jobs["running"] = &Job{ID: "running", Status: JobStatusRunning}
	m.jobs["starting"] = &Job{ID: "starting", Status: JobStatusQueued, starting: true}
	m.jobs["queued"] = &Job{ID: "queued", Status: JobStatusQueued}
	m.jobs["paused"] = &Job{ID: "paused", Status: JobStatusPaused}
	m.jobs["completed"] = &Job{ID: "completed", Status: JobStatusCompleted}
	m.mu.Unlock()

	running, queued := s.ActiveJobCounts()
	if running != 2 {
		t.Errorf("running = %d, want 2 (the running job and the starting job)", running)
	}
	if queued != 1 {
		t.Errorf("queued = %d, want 1", queued)
	}
}

// TestServerRequestLogUsesStructuredPipeline proves the migrated request log
// reaches the application pipeline with the server component and no longer
// writes through the standard library logger.
func TestServerRequestLogUsesStructuredPipeline(t *testing.T) {
	captured := installCapturingPipeline(t)

	// Capture what the standard library logger receives on its own.
	standardLog := &capturedLog{}
	previousWriter := log.Writer()
	log.SetOutput(standardLog)
	t.Cleanup(func() { log.SetOutput(previousWriter) })

	s := &Server{}
	handler := s.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/health", nil))

	out := captured.String()
	for _, want := range []string{"[server]", "method=GET", "path=/api/health"} {
		if !strings.Contains(out, want) {
			t.Errorf("captured pipeline output %q does not contain %q", out, want)
		}
	}
	if standardLog.String() != "" {
		t.Errorf("request log still wrote through the standard library logger: %q", standardLog.String())
	}
}

// TestRuntimeRequestsAreNotLoggedAtInfoLevel proves that the requests of the
// runtime surface do not fill the info level history that the Runtime window
// shows.
//
// The window polls the status every few seconds, and every poll was written
// into the same bounded ring as the records the window exists to show, so
// the history was gone after half an hour. The requests of the surface are
// logged at debug level instead.
//
// Every other request keeps its level, and a request that answers with an
// error is still an info level record: the failure of an endpoint is
// information the user needs.
func TestRuntimeRequestsAreNotLoggedAtInfoLevel(t *testing.T) {
	pipeline := installRequestLogPipeline(t, slog.LevelInfo)
	s := &Server{jobs: newTestJobManager(t, Config{CacheDir: t.TempDir()}, nil)}
	handler := s.handler()

	// A request of the runtime surface.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, runtimeRequest(http.MethodGet, "/api/runtime", "127.0.0.1:5555"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	// A request of another endpoint that answers with an error.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/jobs/missing", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}

	records, _ := pipeline.Records(0, logging.OldestFirst)
	info := requestLogPaths(records, slog.LevelInfo)
	for _, path := range info {
		if isRuntimeDiagnosticsPath(path) {
			t.Errorf("the request of the runtime surface (%q) is logged at info level", path)
		}
	}
	if !slices.Contains(info, "/api/jobs/missing") {
		t.Errorf("the info level records %v do not carry the failed request", info)
	}
}

// TestRuntimeRequestsAreLoggedAtDebugLevel proves that the information about
// the requests of the runtime surface is not dropped: a user who asks for
// debug level still sees them.
func TestRuntimeRequestsAreLoggedAtDebugLevel(t *testing.T) {
	pipeline := installRequestLogPipeline(t, slog.LevelDebug)
	s := &Server{jobs: newTestJobManager(t, Config{CacheDir: t.TempDir()}, nil)}
	handler := s.handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, runtimeRequest(http.MethodGet, "/api/runtime", "127.0.0.1:5555"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	records, _ := pipeline.Records(0, logging.OldestFirst)
	debug := requestLogPaths(records, slog.LevelDebug)
	if !slices.Contains(debug, "/api/runtime") {
		t.Errorf("the debug level records %v do not carry the request of the runtime surface", debug)
	}
	for _, path := range requestLogPaths(records, slog.LevelInfo) {
		if isRuntimeDiagnosticsPath(path) {
			t.Errorf("the request of the runtime surface (%q) is logged at info level", path)
		}
	}
}

// installRequestLogPipeline installs a logging pipeline as the process wide
// default logger and returns it, so a test can read the records of the
// request log with their level.
//
// The pipeline is installed process wide because that is how the application
// installs it: the request log resolves the current default logger on every
// call.
func installRequestLogPipeline(t *testing.T, level slog.Level) *logging.Pipeline {
	t.Helper()

	pipeline, err := logging.New(logging.Options{Level: level, RingCapacity: 64})
	if err != nil {
		t.Fatalf("build logging pipeline: %v", err)
	}
	previous := slog.Default()
	slog.SetDefault(pipeline.Logger())
	t.Cleanup(func() { slog.SetDefault(previous) })
	return pipeline
}

// requestLogPaths returns the paths of the request records that carry the
// given level.
func requestLogPaths(records []logging.Record, level slog.Level) []string {
	var paths []string
	for _, record := range records {
		if record.Message != "request" || record.Level != level {
			continue
		}
		path, _ := record.Attrs["path"].(string)
		paths = append(paths, path)
	}
	return paths
}

// TestProxyErrorReasonDropsCredentialBearingURL proves that a proxy
// configuration failure cannot put the proxy credentials into a log record:
// the parse failure carries the whole proxy URL, the logged reason does not.
//
// The rule itself lives in pkg/hfdownloader, so the reason is rendered by
// the shared helper and not by a second implementation.
func TestProxyErrorReasonDropsCredentialBearingURL(t *testing.T) {
	_, err := url.Parse("http://proxyuser:proxysecret@proxy.example:notaport")
	if err == nil {
		t.Fatal("expected the proxy URL to fail parsing")
	}
	reason := hfdownloader.ProxyErrorReason(fmt.Errorf("invalid proxy URL: %w", err))
	if reason == "" {
		t.Fatal("proxy error reason is empty")
	}
	for _, secret := range []string{"proxysecret", "proxyuser", "proxy.example"} {
		if strings.Contains(reason, secret) {
			t.Errorf("proxy error reason %q still carries %q", reason, secret)
		}
	}
}

// waitForServerBoundAddr waits until the server reports the given bound
// address.
func waitForServerBoundAddr(t *testing.T, s *Server, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		addr := s.BoundAddr()
		if addr != nil && addr.String() == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not report the bound address %s (got %v)", want, addr)
		}
		time.Sleep(time.Millisecond)
	}
}
