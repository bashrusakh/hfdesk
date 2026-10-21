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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/logging"
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
		t.Errorf("BrowserURL() = %q, want %q", got, want)
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

// TestBrowserURLDerivation covers the host-to-URL derivation of BrowserURL.
func TestBrowserURLDerivation(t *testing.T) {
	for _, tc := range []struct {
		name string
		host string
		port int
		want string
	}{
		{name: "wildcard ipv4", host: "0.0.0.0", port: 8080, want: "http://127.0.0.1:8080"},
		{name: "wildcard ipv6", host: "::", port: 8080, want: "http://127.0.0.1:8080"},
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

// TestProxyErrorReasonDropsCredentialBearingURL proves that a proxy
// configuration failure cannot put the proxy credentials into a log record:
// the parse failure carries the whole proxy URL, the logged reason does not.
func TestProxyErrorReasonDropsCredentialBearingURL(t *testing.T) {
	_, err := url.Parse("http://proxyuser:proxysecret@proxy.example:notaport")
	if err == nil {
		t.Fatal("expected the proxy URL to fail parsing")
	}
	reason := proxyErrorReason(fmt.Errorf("invalid proxy URL: %w", err))
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
