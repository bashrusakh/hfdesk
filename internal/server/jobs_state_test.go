// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestJobManagerSerializesSnapshotThroughCommit(t *testing.T) {
	m := newLifecycleTestManager(t, Config{})
	path := m.statePath
	firstStarted := make(chan struct{})
	allowFirst := make(chan struct{})
	secondAtLock := make(chan struct{})
	m.saveMu = &observedSaveLocker{secondLock: secondAtLock}
	m.persistStateFile = func(path string, jobs []*Job) error {
		if len(jobs) == 1 && jobs[0].Repo == "before" {
			close(firstStarted)
			<-allowFirst
		}
		return saveJobsState(path, jobs)
	}
	m.jobs["job"] = &Job{ID: "job", Repo: "before", Status: JobStatusPaused}

	firstDone := make(chan error, 1)
	go func() { firstDone <- m.saveState() }()
	<-firstStarted

	m.mu.Lock()
	m.jobs["job"].Repo = "after"
	m.mu.Unlock()
	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondDone <- m.saveState()
	}()
	<-secondStarted
	<-secondAtLock
	// This mutation is after the second lock attempt, but before its snapshot
	// only when snapshot capture remains inside the saveMu critical section.
	m.mu.Lock()
	m.jobs["job"].Repo = "after-snapshot-barrier"
	m.mu.Unlock()
	close(allowFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	loaded, err := loadJobsState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Repo != "after-snapshot-barrier" {
		t.Fatalf("final state contains stale snapshot: %#v", loaded)
	}
}

type observedSaveLocker struct {
	mu         sync.Mutex
	callMu     sync.Mutex
	calls      int
	secondLock chan struct{}
}

func (l *observedSaveLocker) Lock() {
	l.callMu.Lock()
	l.calls++
	if l.calls == 2 {
		close(l.secondLock)
	}
	l.callMu.Unlock()
	l.mu.Lock()
}

func (l *observedSaveLocker) Unlock() { l.mu.Unlock() }

func TestJobManagerUsesFixedStatePathForLoadAndSave(t *testing.T) {
	configA, configB := t.TempDir(), t.TempDir()
	setTestConfigHome(t, configA)
	m := NewJobManager(Config{}, nil)
	registerTestJobManagerCleanup(t, m)
	pathA := JobsStatePath()
	if err := os.MkdirAll(filepath.Dir(pathA), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathA, []byte(`{"jobs":[{"id":"legacy","repo":"owner/model","status":"paused"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m.LoadState()
	setTestConfigHome(t, configB)
	m.mu.Lock()
	m.jobs["legacy"].Repo = "updated/model"
	m.mu.Unlock()
	if err := m.saveState(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(JobsStatePath()); !os.IsNotExist(err) {
		t.Fatalf("manager wrote to environment's new path: %v", err)
	}
	loaded, err := loadJobsState(pathA)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Repo != "updated/model" || loaded[0].OutputDir != "" {
		t.Fatalf("unexpected fixed-path persisted state: %#v", loaded)
	}
	m.mu.Lock()
	m.jobs = make(map[string]*Job)
	m.mu.Unlock()
	m.LoadState()
	if job, ok := m.GetJob("legacy"); !ok || job.Repo != "updated/model" {
		t.Fatalf("manager reloaded from changed environment path: %#v, %v", job, ok)
	}

	// A separate manager created while the environment points at B can still
	// load A explicitly through its fixed constructor path.
	restored := newJobManagerWithStatePath(Config{}, nil, pathA)
	registerTestJobManagerCleanup(t, restored)
	restored.LoadState()
	if job, ok := restored.GetJob("legacy"); !ok || job.Repo != "updated/model" {
		t.Fatalf("LoadState did not use manager path: %#v, %v", job, ok)
	}
}

func setTestConfigHome(t *testing.T, dir string) {
	t.Helper()
	// Set every platform's standard user-config environment source so tests
	// cannot read or write the real per-user HFDesk directory.
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", dir)
	}
}
