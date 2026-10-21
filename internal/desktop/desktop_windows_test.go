// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/app"
)

// shutdownTimeout bounds the wait for the graceful shutdown of the
// application.
const shutdownTimeout = 30 * time.Second

// portReleaseTimeout bounds the wait for the port of the stopped server to
// be released.
const portReleaseTimeout = 5 * time.Second

// TestInstanceLockDetectsASecondAcquisition proves that the single instance
// lock of the desktop host behaves the way the second desktop launch relies
// on: the first acquisition succeeds and reports that no instance is
// running, and a second acquisition in the same session is detected as
// "already running".
//
// The lock is a named kernel object, so the second acquisition finds the
// object the first one created. Nothing of the lock path opens a window or
// shows a message box, so the test runs on a CI agent.
func TestInstanceLockDetectsASecondAcquisition(t *testing.T) {
	first, err := acquireInstanceLock()
	if err != nil {
		t.Fatalf("acquire the single instance lock: %v", err)
	}
	defer first.Release()

	if first.Exists() {
		t.Fatal("the first acquisition reports an instance that is already running")
	}

	second, err := acquireInstanceLock()
	if err != nil {
		t.Fatalf("acquire the single instance lock a second time: %v", err)
	}
	defer second.Release()

	if !second.Exists() {
		t.Fatal("the second acquisition does not report the running instance")
	}
}

// TestDesktopServerLifecycle proves that the desktop host drives the shared
// application layer the way it drives it at runtime: the server starts on an
// OS-assigned port, the runtime endpoint reports the state of the running
// server, and the graceful shutdown of the cancelled context stops the
// server and releases the port.
//
// The test stays on the loopback interface and finishes in a moment.
//
// Not covered here: the notification area icon, the WebView2 window and the
// interaction with them. They need an interactive Windows session, which a
// CI agent does not provide, and the desktop host reports their problems in
// a native message box that would block the agent.
func TestDesktopServerLifecycle(t *testing.T) {
	useTemporaryStateDir(t)

	// The desktop host installs the same pipeline: the file sink is always
	// on, the console sink is off because a GUI process has no console.
	logFile := filepath.Join(t.TempDir(), "hfdesk.log")
	previous := slog.Default()
	pipeline, err := app.SetupLogging(app.LogOptions{
		Level:    "info",
		Console:  false,
		FilePath: logFile,
	})
	if err != nil {
		t.Fatalf("install the logging pipeline: %v", err)
	}
	defer func() {
		pipeline.Close()
		// The pipeline is the process wide default logger, so the logger
		// the other tests of the package expect is put back.
		slog.SetDefault(previous)
	}()

	// A record of its own makes the log cursor of the runtime status
	// deterministic: the cursor is at least the sequence number of this
	// record, whatever the server logged in the meantime.
	slog.Info("the desktop smoke test started")

	const version = "1.2.3-desktop-test"
	application, err := app.New(app.Options{Port: 0}, version)
	if err != nil {
		t.Fatalf("create the application: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("start the application: %v", err)
	}

	url := application.URL()
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("url = %q, want a loopback url", url)
	}
	port := portOfURL(t, url)

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}

	status := readRuntimeStatus(t, client, url)
	if status.Version != version {
		t.Errorf("version = %q, want %q", status.Version, version)
	}
	if status.Status != "ok" {
		t.Errorf("status = %q, want %q", status.Status, "ok")
	}
	if status.URL != url {
		t.Errorf("url = %q, want %q", status.URL, url)
	}
	if !strings.HasSuffix(status.Address, ":"+strconv.Itoa(port)) {
		t.Errorf("address = %q, want the address of the port %d", status.Address, port)
	}
	if status.ActiveJobs.Running != 0 || status.ActiveJobs.Queued != 0 {
		t.Errorf("active jobs = %+v, want no active job", status.ActiveJobs)
	}
	if status.LogCursor == 0 {
		t.Error("log cursor = 0, want the cursor of the emitted records")
	}
	if status.LogFile != logFile {
		t.Errorf("log file = %q, want %q", status.LogFile, logFile)
	}

	// The graceful shutdown: the cancelled context stops the server, and
	// the application waits for it.
	cancel()
	if err := waitForShutdown(t, application); err != nil {
		t.Fatalf("the graceful shutdown failed: %v", err)
	}

	// The server stopped serving.
	response, err := client.Get(url + "/api/health")
	if err == nil {
		response.Body.Close()
		t.Fatal("the server still answers after the graceful shutdown")
	}

	// The port is released, so another instance can take it.
	listener, err := listenAgain(port)
	if err != nil {
		t.Fatalf("the port %d was not released: %v", port, err)
	}
	listener.Close()
}

// runtimeStatus is the part of the runtime status the smoke test reads.
type runtimeStatus struct {
	Version    string `json:"version"`
	Status     string `json:"status"`
	URL        string `json:"url"`
	Address    string `json:"address"`
	ActiveJobs struct {
		Running int `json:"running"`
		Queued  int `json:"queued"`
	} `json:"activeJobs"`
	LogCursor uint64 `json:"logCursor"`
	LogFile   string `json:"logFile"`
}

// readRuntimeStatus reads the runtime status of the running server.
func readRuntimeStatus(t *testing.T, client *http.Client, url string) runtimeStatus {
	t.Helper()

	response, err := client.Get(url + "/api/runtime")
	if err != nil {
		t.Fatalf("read the runtime status: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("runtime status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	var status runtimeStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatalf("decode the runtime status: %v", err)
	}
	return status
}

// waitForShutdown waits for the application to stop and returns the error of
// the shutdown.
func waitForShutdown(t *testing.T, application *app.App) error {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- application.Wait() }()

	select {
	case err := <-done:
		return err
	case <-time.After(shutdownTimeout):
		t.Fatal("the application did not stop after the context was cancelled")
		return nil
	}
}

// listenAgain binds the port of the stopped server again, so the test can
// prove that the graceful shutdown released it.
//
// Windows reuses a recently used address by default, so the bind succeeds
// right after the shutdown. The small retry budget covers the moment
// between the close of the listener and the release of the port.
func listenAgain(port int) (net.Listener, error) {
	address := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))

	deadline := time.Now().Add(portReleaseTimeout)
	for {
		listener, err := net.Listen("tcp", address)
		if err == nil {
			return listener, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// portOfURL returns the port of a browser-safe URL of the server.
func portOfURL(t *testing.T, url string) int {
	t.Helper()

	_, portText, err := net.SplitHostPort(strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatalf("url %q carries no port: %v", url, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("url %q carries no port: %v", url, err)
	}
	if port <= 0 {
		t.Fatalf("url = %q, want a bound port", url)
	}
	return port
}

// useTemporaryStateDir points the per user configuration directory of the
// platform at a temporary directory and moves the working directory, so the
// test never reads or writes the state of the real installation.
func useTemporaryStateDir(t *testing.T) {
	t.Helper()

	root := t.TempDir()
	t.Setenv("AppData", root)
	t.Setenv("LOCALAPPDATA", root)
	t.Chdir(t.TempDir())
}
