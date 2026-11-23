// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/logging"
)

// TestServerShutdownEndsWithALiveLogStream proves the stop signal of the
// streaming responses. The Logs window and the runtime page keep a live
// Server-Sent Events log stream open, and that stream never ends on its own:
// http.Server.Shutdown waits for every active response, and the request
// context of a streaming handler is not cancelled by Shutdown. Without a
// stop signal the stream holds the HTTP drain until the whole drain deadline
// passes and the shutdown fails with "shutdown HTTP server: context deadline
// exceeded" — which is exactly what aborted the service-installation
// handover of the desktop host. With the signal, the stream ends on the
// stop, the shutdown returns promptly and cleanly, and the subscription of
// the stream is released.
func TestServerShutdownEndsWithALiveLogStream(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 32)
	addr := reserveServerTestAddr(t)
	m := newJobManagerWithStatePath(Config{CacheDir: t.TempDir()}, nil, filepath.Join(t.TempDir(), "jobs.json"))
	registerTestJobManagerCleanup(t, m)
	s := &Server{
		jobs:             m,
		pipelineProvider: func() *logging.Pipeline { return pipeline },
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.serveHTTP(ctx, addr, s.handler(), 5*time.Second) }()
	waitForServerListener(t, addr)

	// The normal state of the user's machine: an open window with a live
	// log stream that never ends by itself.
	response, err := http.Get("http://" + addr + "/api/runtime/logs/stream?after=0")
	if err != nil {
		cancel()
		t.Fatalf("open the live log stream: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("stream status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		cancel()
		t.Fatalf("stream content type = %q, want %q", got, "text/event-stream")
	}

	// The stop of the host: the stream has to end on this signal, so the
	// HTTP drain completes promptly instead of exhausting its deadline.
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("shutdown with a live log stream: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the server did not stop promptly while a live log stream was open: the stream held the HTTP drain until its deadline")
	}
	waitForRuntimeLogStreamIdle(t)
}

// TestServerShutdownDrainsAnInFlightRequestWithALiveLogStream proves the
// graceful semantics of ordinary requests survives the stop signal of the
// streams: beside a live log stream that ends on the signal, an ordinary
// in-flight request is drained to its complete response — not killed — and
// the shutdown still returns promptly and cleanly.
func TestServerShutdownDrainsAnInFlightRequestWithALiveLogStream(t *testing.T) {
	pipeline, _ := newRuntimeTestPipeline(t, 32)
	addr := reserveServerTestAddr(t)
	m := newJobManagerWithStatePath(Config{CacheDir: t.TempDir()}, nil, filepath.Join(t.TempDir(), "jobs.json"))
	registerTestJobManagerCleanup(t, m)
	s := &Server{
		jobs:             m,
		pipelineProvider: func() *logging.Pipeline { return pipeline },
	}

	handlerEntered := make(chan struct{})
	mux := s.handler()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow-in-flight" {
			close(handlerEntered)
			time.Sleep(300 * time.Millisecond)
			_, _ = io.WriteString(w, "in-flight complete")
			return
		}
		mux.ServeHTTP(w, r)
	})

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.serveHTTP(ctx, addr, handler, 5*time.Second) }()
	waitForServerListener(t, addr)

	clientDone := make(chan string, 1)
	go func() {
		response, err := http.Get("http://" + addr + "/slow-in-flight")
		if err != nil {
			clientDone <- "request failed: " + err.Error()
			return
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			clientDone <- "read failed: " + readErr.Error()
			return
		}
		clientDone <- string(body)
	}()
	select {
	case <-handlerEntered:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the in-flight request did not enter its handler")
	}

	// The live log stream opens beside the in-flight request.
	response, err := http.Get("http://" + addr + "/api/runtime/logs/stream?after=0")
	if err != nil {
		cancel()
		t.Fatalf("open the live log stream: %v", err)
	}
	defer response.Body.Close()
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		cancel()
		t.Fatalf("stream content type = %q, want %q", got, "text/event-stream")
	}

	cancel()

	// The stop signal ends the stream, not the ordinary request: the
	// in-flight response still arrives complete.
	select {
	case body := <-clientDone:
		if body != "in-flight complete" {
			t.Errorf("the in-flight response = %q, want the complete response", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request was not drained to its complete response")
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("shutdown with a live log stream and a drained request: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the server did not stop promptly beside a live log stream")
	}
	waitForRuntimeLogStreamIdle(t)
}
