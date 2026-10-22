// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/server"
)

// TestNewPropagatesTheVersion proves that the shared layer owns the version
// propagation to the server package.
func TestNewPropagatesTheVersion(t *testing.T) {
	useTemporaryConfigDir(t)

	application, err := New(Options{Port: 8080}, "1.2.3-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if server.Version != "1.2.3-test" {
		t.Errorf("server version = %q, want %q", server.Version, "1.2.3-test")
	}
	if application.Config().Port != 8080 {
		t.Errorf("config port = %d, want 8080", application.Config().Port)
	}
}

// TestNewReportsConfigurationProblems proves that a configuration problem is
// reported as an error instead of terminating the process.
func TestNewReportsConfigurationProblems(t *testing.T) {
	useTemporaryConfigDir(t)

	if _, err := New(Options{Port: 8080, StallTimeout: "-5s"}, "test"); err == nil {
		t.Fatal("New accepted a negative stall timeout")
	}
}

// TestStartReportsBindFailure proves that a port that is already in use is
// reported as an error instead of a silent success.
func TestStartReportsBindFailure(t *testing.T) {
	useTemporaryConfigDir(t)

	occupied, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port

	application, err := New(Options{Port: port}, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = application.Start(context.Background())
	if err == nil {
		t.Fatal("Start returned no error for an occupied port")
	}
	if !strings.Contains(err.Error(), "listen on") {
		t.Errorf("error = %q, want it to report the failed listen", err.Error())
	}
}

// TestStartReportsTheEffectiveURL proves that the URL comes from the address
// the server is really bound to: an OS-assigned port is reported, and the
// wildcard bind is reported as the loopback address a browser can open.
func TestStartReportsTheEffectiveURL(t *testing.T) {
	useTemporaryConfigDir(t)

	application, err := New(Options{Port: 0}, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	url := application.URL()
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("url = %q, want a loopback url", url)
	}
	portText := strings.TrimPrefix(url, "http://127.0.0.1:")
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("url %q carries no port: %v", url, err)
	}
	if port <= 0 {
		t.Errorf("url = %q, want the OS-assigned port", url)
	}

	// The server answers on the reported URL.
	resp, err := http.Get(url + "/api/health")
	if err != nil {
		t.Fatalf("health request: %v", err)
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

	cancel()
	if err := waitForShutdown(t, application); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// TestStartUsesTheConfiguredPort proves that a non-default port is reported
// correctly.
func TestStartUsesTheConfiguredPort(t *testing.T) {
	useTemporaryConfigDir(t)

	probe, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	application, err := New(Options{Port: port}, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	want := "http://127.0.0.1:" + strconv.Itoa(port)
	if got := application.URL(); got != want {
		t.Errorf("url = %q, want %q", got, want)
	}

	cancel()
	if err := waitForShutdown(t, application); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// TestWaitReturnsAfterGracefulShutdown proves that a cancelled context makes
// Wait return once the graceful shutdown completed.
func TestWaitReturnsAfterGracefulShutdown(t *testing.T) {
	useTemporaryConfigDir(t)

	application, err := New(Options{Port: 0}, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cancel()
	if err := waitForShutdown(t, application); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// TestWaitReportsTheStartupFailure proves that Wait reports the failure of a
// start that never served.
func TestWaitReportsTheStartupFailure(t *testing.T) {
	useTemporaryConfigDir(t)

	occupied, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port

	application, err := New(Options{Port: port}, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := application.Start(context.Background()); err == nil {
		t.Fatal("Start returned no error for an occupied port")
	}

	if err := application.Wait(); err == nil {
		t.Error("Wait returned no error after a failed start")
	}
}

// TestStartBindFailureLeavesNoServerBehind proves that a failed bind reports
// the error of the failed listen, leaves no running server behind and closes
// the job manager: the application never starts serving, and the job manager
// commits its final state.
func TestStartBindFailureLeavesNoServerBehind(t *testing.T) {
	useTemporaryConfigDir(t)

	occupied, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port

	application, err := New(Options{Port: port}, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = application.Start(context.Background())
	if err == nil {
		t.Fatal("Start returned no error for an occupied port")
	}
	want := fmt.Sprintf("listen on 0.0.0.0:%d", port)
	if !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error = %q, want it to start with %q", err.Error(), want)
	}

	// The application reports the startup failure.
	if err := application.Wait(); err == nil {
		t.Error("Wait returned no error after a failed start")
	}

	// The server never bound an address, so it never served a request.
	if addr := application.server.BoundAddr(); addr != nil {
		t.Errorf("the server bound %s after a failed start", addr)
	}

	// The job manager was closed: it committed its final state.
	if _, err := os.Stat(server.JobsStatePath()); err != nil {
		t.Errorf("the job manager did not commit its final state: %v", err)
	}
}

// waitForShutdown waits for the application to stop and returns the error
// of the shutdown.
func waitForShutdown(t *testing.T, application *App) error {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- application.Wait() }()

	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("Wait did not return after the context was cancelled")
		return nil
	}
}
