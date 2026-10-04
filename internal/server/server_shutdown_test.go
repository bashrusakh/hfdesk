// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerShutdownDrainsHandlerBeforeFinalJobSnapshot(t *testing.T) {
	addr := reserveServerTestAddr(t)
	statePath := filepath.Join(t.TempDir(), "jobs.json")
	m := newJobManagerWithStatePath(Config{CacheDir: t.TempDir()}, nil, statePath)
	registerTestJobManagerCleanup(t, m)
	s := &Server{jobs: m}
	handlerEntered := make(chan struct{})
	allowMutation := make(chan struct{})
	handlerDone := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerEntered)
		<-allowMutation
		_, _, err := m.CreateJob(DownloadRequest{Repo: "owner/accepted-during-drain"})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusAccepted)
		}
		close(handlerDone)
	})
	finalWriteStarted := make(chan struct{})
	allowFinalWrite := make(chan struct{})
	m.persistStateFile = func(path string, jobs []*Job) error {
		close(finalWriteStarted)
		<-allowFinalWrite
		return saveJobsState(path, jobs)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.serveHTTP(ctx, addr, handler, 5*time.Second) }()
	waitForServerListener(t, addr)
	clientDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/mutate")
		if err != nil {
			clientDone <- err
			return
		}
		_ = resp.Body.Close()
		clientDone <- nil
	}()
	select {
	case <-handlerEntered:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("HTTP handler did not enter")
	}
	cancel()
	waitForManagerFence(t, m)
	close(allowMutation)
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted handler did not perform its manager mutation")
	}
	if err := <-clientDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-finalWriteStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("manager final persistence did not start after HTTP drain")
	}
	select {
	case err := <-serveDone:
		t.Fatalf("server returned before final persistence completed: %v", err)
	default:
	}
	close(allowFinalWrite)
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not return after final persistence completed")
	}
	loaded, err := loadJobsState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Repo != "owner/accepted-during-drain" || loaded[0].Status != JobStatusPaused {
		t.Fatalf("final snapshot missed accepted handler mutation: %#v", loaded)
	}
}

func TestServerPreCancelledContextAndBindErrorCleanup(t *testing.T) {
	t.Run("pre-cancelled context", func(t *testing.T) {
		m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "jobs.json"))
		registerTestJobManagerCleanup(t, m)
		s := &Server{jobs: m}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := s.serveHTTP(ctx, reserveServerTestAddr(t), http.NotFoundHandler(), time.Second)
		if err != nil {
			t.Fatalf("pre-cancelled server cleanup: %v", err)
		}
		select {
		case <-m.closeDone:
		default:
			t.Fatal("pre-cancelled server returned before manager close")
		}
	})

	t.Run("bind failure is joined with final persistence error", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		want := errors.New("final persistence failure")
		m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "jobs.json"))
		registerTestJobManagerCleanup(t, m)
		m.persistStateFile = func(string, []*Job) error { return want }
		s := &Server{jobs: m}
		err = s.serveHTTP(context.Background(), listener.Addr().String(), http.NotFoundHandler(), time.Second)
		if !errors.Is(err, want) {
			t.Fatalf("bind error path lost final persistence error: %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "listen on") {
			t.Fatalf("bind failure was not retained: %v", err)
		}
		select {
		case <-m.closeDone:
		default:
			t.Fatal("bind failure returned before manager close coordinator completed")
		}
	})
}

func TestServerHTTPShutdownTimeoutReturnsErrorAndRejectsLateMutation(t *testing.T) {
	addr := reserveServerTestAddr(t)
	m := newJobManagerWithStatePath(Config{CacheDir: t.TempDir()}, nil, filepath.Join(t.TempDir(), "jobs.json"))
	registerTestJobManagerCleanup(t, m)
	s := &Server{jobs: m}
	entered := make(chan struct{})
	allowLateMutation := make(chan struct{})
	lateMutation := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-allowLateMutation
		_, _, err := m.CreateJob(DownloadRequest{Repo: "owner/too-late"})
		lateMutation <- err
	})
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.serveHTTP(ctx, addr, handler, 25*time.Millisecond) }()
	waitForServerListener(t, addr)
	clientDone := make(chan struct{})
	go func() {
		resp, _ := http.Get("http://" + addr + "/blocked")
		if resp != nil {
			_ = resp.Body.Close()
		}
		close(clientDone)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("blocked handler did not enter")
	}
	cancel()
	select {
	case err := <-serveDone:
		if err == nil {
			t.Fatal("HTTP shutdown timeout was reported as success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not return after HTTP shutdown timeout")
	}
	close(allowLateMutation)
	select {
	case err := <-lateMutation:
		if !errors.Is(err, errJobManagerStopping) {
			t.Fatalf("late handler mutation error = %v, want stopping", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late handler did not finish after release")
	}
	select {
	case <-clientDone:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP client did not unwind")
	}
}

func TestServerUnexpectedServeErrorIsReturned(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("accept failed")
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "jobs.json"))
	registerTestJobManagerCleanup(t, m)
	s := &Server{jobs: m, httpServer: &http.Server{Handler: http.NotFoundHandler()}}
	err = s.serveHTTPListener(context.Background(), failingAcceptListener{Listener: listener, err: want}, time.Second)
	if !errors.Is(err, want) {
		t.Fatalf("serve error not returned: %v", err)
	}
	select {
	case <-m.closeDone:
	default:
		t.Fatal("serve error returned before manager close")
	}
}

type failingAcceptListener struct {
	net.Listener
	err error
}

func (l failingAcceptListener) Accept() (net.Conn, error) { return nil, l.err }

func reserveServerTestAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitForManagerFence(t *testing.T, m *JobManager) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.RLock()
		fenced := m.fenced
		m.mu.RUnlock()
		if fenced {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("server did not quiesce manager")
		case <-ticker.C:
		}
	}
}

func waitForServerListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			if closeErr := conn.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("server did not listen on %s: %v", addr, err)
		case <-ticker.C:
		}
	}
}
