// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestReplaceJobsStateFileKeepsPreviousFileUntilRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	old := []byte(`{"jobs":[{"id":"old"}]}`)
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	ops := failingRenameOps{defaultStateFileOps{}}
	err := replaceJobsStateFile(path, []byte(`{"jobs":[{"id":"new"}]}`), ops)
	if err == nil {
		t.Fatal("expected replacement error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(old) {
		t.Fatalf("target changed before commit: %s", got)
	}
	if runtime.GOOS != "windows" {
		if mode := fileMode(t, path); mode.Perm() != 0o600 {
			t.Fatalf("mode widened: %v", mode.Perm())
		}
	}
	assertNoStateTemps(t, filepath.Dir(path))
}

func TestReplaceJobsStateFileWriteFailurePreservesTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	old := []byte(`{"jobs":[{"id":"old"}]}`)
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	ops := failingWriteOps{defaultStateFileOps{}}
	if err := replaceJobsStateFile(path, []byte(`{"jobs":[{"id":"new"}]}`), ops); err == nil {
		t.Fatal("expected write error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(old) {
		t.Fatalf("target changed after failed write: %s", got)
	}
	assertNoStateTemps(t, filepath.Dir(path))
}

func TestReplaceJobsStateFileCloseFailurePreservesTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	old := []byte(`{"jobs":[{"id":"old"}]}`)
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	closeErr := errors.New("close failed")
	err := replaceJobsStateFile(path, []byte(`{"jobs":[{"id":"new"}]}`), failingCloseOps{defaultStateFileOps{}, closeErr})
	if !errors.Is(err, closeErr) {
		t.Fatalf("close error not reported: %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(old) {
		t.Fatalf("target changed after close failure: %s", got)
	}
	assertNoStateTemps(t, filepath.Dir(path))
}

func TestReplaceJobsStateFileShortWritePreservesTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	old := []byte(`{"jobs":[{"id":"old"}]}`)
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceJobsStateFile(path, []byte(`{"jobs":[{"id":"new"}]}`), shortWriteOps{defaultStateFileOps{}}); err == nil {
		t.Fatal("expected short write error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(old) {
		t.Fatalf("target changed after short write: %s", got)
	}
	assertNoStateTemps(t, filepath.Dir(path))
}

func TestReplaceJobsStateFilePreservesRestrictiveModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows mode bits are emulated and do not represent DACL permissions")
	}
	for _, mode := range []os.FileMode{0o000, 0o400, 0o600} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.json")
			if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if err := replaceJobsStateFile(path, []byte("new"), defaultStateFileOps{}); err != nil {
				t.Fatal(err)
			}
			if got := fileMode(t, path).Perm(); got != mode {
				t.Fatalf("replacement mode = %04o, want %04o", got, mode)
			}
		})
	}
}

func TestReplaceJobsStateFileReportsCleanupFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	cleanupErr := errors.New("cleanup failed")
	ops := failingRenameAndRemoveOps{defaultStateFileOps{}, cleanupErr}
	err := replaceJobsStateFile(path, []byte("{}"), ops)
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup error not reported: %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target unexpectedly exists: %v", statErr)
	}
}

func TestJobsStateLoadWaitsForReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	old := []byte(`{"jobs":[{"id":"old"}]}`)
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	ops := blockingWriteOps{defaultStateFileOps{}, make(chan struct{}), make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(ops.allowWrite) })
	done := make(chan error, 1)
	go func() {
		stateMu.Lock()
		defer stateMu.Unlock()
		done <- replaceJobsStateFile(path, []byte(`{"jobs":[{"id":"new"}]}`), ops)
	}()
	<-ops.writeStarted
	visible, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(visible) != string(old) {
		t.Fatalf("reader observed incomplete replacement: %q", visible)
	}
	loaded := make(chan []byte, 1)
	go func() {
		stateMu.Lock()
		defer stateMu.Unlock()
		data, err := os.ReadFile(path)
		if err != nil {
			loaded <- []byte("ERROR: " + err.Error())
			return
		}
		loaded <- data
	}()
	release.Do(func() { close(ops.allowWrite) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := <-loaded; string(got) != `{"jobs":[{"id":"new"}]}` {
		t.Fatalf("loader saw %q", got)
	}
}

func TestSaveLoadJobsStatePathHelpers(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("APPDATA", configHome)
	t.Setenv("USERPROFILE", configHome)
	t.Setenv("HOME", configHome)
	job := &Job{ID: "id", Status: JobStatusRunning}
	if err := SaveJobsState([]*Job{job}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadJobsState()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Status != JobStatusPaused {
		t.Fatalf("unexpected restored state: %#v", loaded)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func assertNoStateTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".jobs_state-") {
			t.Errorf("temporary file remains: %s", entry.Name())
		}
	}
}

type failingRenameOps struct{ stateFileOps }

func (failingRenameOps) rename(string, string) error { return errors.New("rename failed") }

type failingRenameAndRemoveOps struct {
	stateFileOps
	err error
}

func (f failingRenameAndRemoveOps) rename(string, string) error { return errors.New("rename failed") }
func (f failingRenameAndRemoveOps) remove(_ string) error {
	return f.err
}

type failingWriteOps struct{ stateFileOps }

func (f failingWriteOps) createTemp(dir string) (stateTempFile, error) {
	tmp, err := f.stateFileOps.createTemp(dir)
	if err != nil {
		return nil, err
	}
	return failingStateTemp{stateTempFile: tmp}, nil
}

type failingStateTemp struct{ stateTempFile }

func (f failingStateTemp) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestReplaceJobsStateFileSyncFailurePreservesTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	old := []byte(`{"jobs":[{"id":"old"}]}`)
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	syncErr := errors.New("sync failed")
	err := replaceJobsStateFile(path, []byte(`{"jobs":[{"id":"new"}]}`), failingSyncOps{defaultStateFileOps{}, syncErr})
	if !errors.Is(err, syncErr) {
		t.Fatalf("sync error not reported: %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(old) {
		t.Fatalf("target changed after sync failure: %s", got)
	}
	assertNoStateTemps(t, filepath.Dir(path))
}

type failingSyncOps struct {
	stateFileOps
	err error
}

func (f failingSyncOps) createTemp(dir string) (stateTempFile, error) {
	tmp, err := f.stateFileOps.createTemp(dir)
	if err != nil {
		return nil, err
	}
	return failingSyncTemp{stateTempFile: tmp, err: f.err}, nil
}

type failingSyncTemp struct {
	stateTempFile
	err error
}

func (f failingSyncTemp) Sync() error { return f.err }

type shortWriteOps struct{ stateFileOps }

func (f shortWriteOps) createTemp(dir string) (stateTempFile, error) {
	tmp, err := f.stateFileOps.createTemp(dir)
	if err != nil {
		return nil, err
	}
	return shortWriteTemp{stateTempFile: tmp}, nil
}

type shortWriteTemp struct{ stateTempFile }

func (f shortWriteTemp) Write(data []byte) (int, error) {
	n := len(data) / 2
	if n == 0 {
		n = 1
	}
	written, err := f.stateTempFile.Write(data[:n])
	if err != nil {
		return written, err
	}
	return written, nil
}

type failingCloseOps struct {
	stateFileOps
	err error
}

func (f failingCloseOps) createTemp(dir string) (stateTempFile, error) {
	tmp, err := f.stateFileOps.createTemp(dir)
	if err != nil {
		return nil, err
	}
	return failingCloseTemp{stateTempFile: tmp, err: f.err}, nil
}

type failingCloseTemp struct {
	stateTempFile
	err error
}

func (f failingCloseTemp) Close() error {
	if err := f.stateTempFile.Close(); err != nil {
		return err
	}
	return f.err
}

type blockingWriteOps struct {
	stateFileOps
	writeStarted chan struct{}
	allowWrite   chan struct{}
}

func (b blockingWriteOps) createTemp(dir string) (stateTempFile, error) {
	f, err := b.stateFileOps.createTemp(dir)
	if err != nil {
		return nil, err
	}
	return blockingStateTemp{stateTempFile: f, started: b.writeStarted, allow: b.allowWrite}, nil
}

type blockingStateTemp struct {
	stateTempFile
	started chan struct{}
	allow   chan struct{}
}

func (b blockingStateTemp) Write(data []byte) (int, error) {
	close(b.started)
	<-b.allow
	return b.stateTempFile.Write(data)
}
