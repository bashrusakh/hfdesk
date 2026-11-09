// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/logging"
	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/server"
)

func TestRunInstallsRuntimeModelAndStopsOnCancel(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)

	opts := RunOptions{Paths: model}
	start := time.Now()
	if err := runService(t, opts); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("Run() returned after %s, want a prompt shutdown", elapsed)
	}

	if got := runtimepaths.Current(); got != model {
		t.Errorf("runtimepaths.Current() = %+v, want %+v", got, model)
	}

	// The process log reached the log file of the instance.
	logFile := filepath.Join(model.LogDir, "hfdesk.log")
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read %s: %v", logFile, err)
	}
	if len(data) == 0 {
		t.Errorf("%s is empty, want the process log", logFile)
	}
}

// TestRunWritesTheRecordsOfTheServerIntoTheLogFileOfTheInstance checks the
// property the service mode depends on: the records of the server reach the
// log file of the instance.
//
// The server logs through the shared logging pipeline, so the instance has to
// install that pipeline with a file sink that points at the log file of the
// instance. A host that redirects the standard logger instead leaves the log
// file of a running instance without the records of the server.
//
// The test is hermetic: the state directory and the log directory of the
// instance are temporary directories, and the instance is a real one.
func TestRunWritesTheRecordsOfTheServerIntoTheLogFileOfTheInstance(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)

	port := freePort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{Paths: model, Listen: addr})
	}()

	// The instance has to answer before the log is read: the record of the
	// server is written when it starts.
	if _, err := waitForSettings(t, addr); err != nil {
		cancel()
		<-done
		t.Fatalf("the instance never answered: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	// The pipeline is closed when the host returns, so the log file of the
	// instance is complete when the run is over.
	logFile := filepath.Join(model.LogDir, "hfdesk.log")
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read %s: %v", logFile, err)
	}
	if len(data) == 0 {
		t.Fatalf("%s is empty, want the records of the server", logFile)
	}
	// The record is a record of the shared logging pipeline: the file sink
	// of the pipeline writes the records as JSON lines, with the component
	// that produced them.
	if !strings.Contains(string(data), `"component":"server"`) {
		t.Errorf("%s = %s, want the record of the server of the shared pipeline", logFile, data)
	}
	if !strings.Contains(string(data), `"message":"server starting"`) {
		t.Errorf("%s = %s, want the record of the server of the shared pipeline", logFile, data)
	}
}

// TestRunKeepsRunningWhenTheLogFileCannotBeWritten checks that a log file
// that cannot be written never stops the instance.
//
// The failure of the log file is not the failure of the instance: the console
// sink still reaches the supervisor of the instance, and the instance keeps
// serving.
func TestRunKeepsRunningWhenTheLogFileCannotBeWritten(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)

	// The log file cannot be created: a directory sits at its path.
	if err := os.MkdirAll(filepath.Join(model.LogDir, "hfdesk.log"), 0o700); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{Paths: model, Listen: addr})
	}()

	if _, err := waitForSettings(t, addr); err != nil {
		cancel()
		<-done
		t.Fatalf("the instance never answered: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() = %v, want nil: the failure of the log file never stops the instance", err)
	}
}

// TestRunKeepsWritingTheLogFileWhenTheStandardErrorFails checks the log of
// a running instance against a broken standard error.
//
// The console sink of the shared pipeline writes to the standard error of the
// process, and a Windows instance is started by the service control manager
// that has no valid standard error there. The failure of the console sink
// must never suppress the file sink: the log file of the instance still has
// to receive every record.
func TestRunKeepsWritingTheLogFileWhenTheStandardErrorFails(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)

	// The standard error of the process is broken: every write to it fails.
	broken, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	if err := broken.Close(); err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = broken
	t.Cleanup(func() { os.Stderr = previous })

	port := freePort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{Paths: model, Listen: addr})
	}()

	if _, err := waitForSettings(t, addr); err != nil {
		cancel()
		<-done
		t.Fatalf("the instance never answered: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	logFile := filepath.Join(model.LogDir, "hfdesk.log")
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read %s: %v", logFile, err)
	}
	if !strings.Contains(string(data), `"message":"server starting"`) {
		t.Errorf("%s = %s, want the record of the server: the console sink fails, the file sink still receives every record", logFile, data)
	}

	// The broken standard error really broke the console sink: without this
	// the assertion above would prove nothing.
	pipeline := logging.Default()
	if pipeline == nil {
		t.Fatal("the logging pipeline of the instance is not installed")
	}
	if pipeline.Err() == nil {
		t.Error("the console sink did not fail: the broken standard error was not used")
	}
}

// TestRunCreatesTheLogDirectoryOwnerOnly checks the permissions of the log
// directory and of the log file of an instance.
//
// The log directory holds the log of the instance, so a directory that is
// missing at the start of an instance must not be recreated world-readable.
// A directory that already exists keeps its mode: it belongs to whoever
// named it, and the instance may only be able to write there.
func TestRunCreatesTheLogDirectoryOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows mode bits are emulated and do not represent DACL permissions")
	}

	t.Run("the missing log directory is created owner-only", func(t *testing.T) {
		root := t.TempDir()
		model := runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var"),
			LogDir:     filepath.Join(root, "var", "logs"),
		}
		t.Cleanup(runtimepaths.Reset)

		if err := runService(t, RunOptions{Paths: model}); err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}

		info, err := os.Stat(model.LogDir)
		if err != nil {
			t.Fatalf("stat %s: %v", model.LogDir, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("the mode of the log directory = %04o, want 0700", got)
		}

		// The log file keeps the mode the shared file sink gives it.
		info, err = os.Stat(filepath.Join(model.LogDir, "hfdesk.log"))
		if err != nil {
			t.Fatalf("stat the log file: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("the mode of the log file = %04o, want 0600", got)
		}
	})

	t.Run("an existing log directory keeps its mode", func(t *testing.T) {
		root := t.TempDir()
		model := runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var"),
			LogDir:     filepath.Join(root, "var", "logs"),
		}
		t.Cleanup(runtimepaths.Reset)

		// The log directory exists and belongs to somebody else: the
		// instance writes there and leaves the mode alone.
		if err := os.MkdirAll(model.LogDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(model.LogDir, 0o755); err != nil {
			t.Fatal(err)
		}

		if err := runService(t, RunOptions{Paths: model}); err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}

		info, err := os.Stat(model.LogDir)
		if err != nil {
			t.Fatalf("stat %s: %v", model.LogDir, err)
		}
		if got := info.Mode().Perm(); got != 0o755 {
			t.Errorf("the mode of the log directory = %04o, want 0755: the instance never widens the permissions of a directory it did not create", got)
		}
	})
}

func TestRunServesUntilTheContextIsCancelled(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)

	port := freePort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{Paths: model, Listen: addr})
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://" + addr + "/api/health"
	var lastErr error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		body, status, err := get(client, url)
		switch {
		case err != nil:
			lastErr = err
		case status != http.StatusOK:
			lastErr = fmt.Errorf("status %d: %s", status, body)
		case !strings.Contains(body, `"status":"ok"`):
			lastErr = fmt.Errorf("unexpected health payload: %s", body)
		default:
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("Run() = %v, want nil", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	<-done
	t.Fatalf("the health endpoint never answered: %v", lastErr)
}

// TestRunShutdownPersistsTheJobListWithinTheBudget checks the graceful
// shutdown of a running instance.
//
// The instance is stopped while a request is in flight and while its job
// list is not empty. The host has to return within the shared budget of the
// service layer, and the job list has to end up in the state directory of
// the instance.
//
// The budget is the one every supervisor of the service layer waits for: a
// supervisor that waits less kills the instance while it is still saving
// its state.
func TestRunShutdownPersistsTheJobListWithinTheBudget(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)

	// The instance starts with a job in its list: the shutdown has to
	// persist it.
	runtimepaths.Install(model)
	if err := server.SaveJobsState([]*server.Job{{
		ID:     "job-of-the-test",
		Repo:   "owner/model",
		Status: server.JobStatusPaused,
	}}); err != nil {
		t.Fatalf("SaveJobsState(): %v", err)
	}
	stateFile := model.JobsStateFile()

	port := freePort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{Paths: model, Listen: addr})
	}()

	if _, err := waitForSettings(t, addr); err != nil {
		cancel()
		<-done
		t.Fatalf("the instance never answered: %v", err)
	}

	// The job list of the running instance is not empty.
	client := &http.Client{Timeout: 2 * time.Second}
	body, status, err := get(client, "http://"+addr+"/api/jobs")
	if err != nil {
		cancel()
		<-done
		t.Fatalf("the job list of the instance: %v", err)
	}
	if status != http.StatusOK {
		cancel()
		<-done
		t.Fatalf("the job list of the instance: status %d: %s", status, body)
	}
	if !strings.Contains(body, "job-of-the-test") {
		cancel()
		<-done
		t.Fatalf("the job list of the instance = %s, want the job of the test", body)
	}

	// A request is in flight while the instance stops. The client sends
	// the head of a request and the rest of it after the stop was asked
	// for, so the drain of the HTTP server has to wait for it.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("dial the instance: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /api/health HTTP/1.1\r\nHost: hfdesk\r\n")); err != nil {
		cancel()
		<-done
		t.Fatalf("write the head of the request: %v", err)
	}
	// The server reads the head of the request and waits for the rest of
	// it. The pause gives it the time to do so.
	time.Sleep(100 * time.Millisecond)

	// The state file of the job list is removed, so the file that is there
	// after the stop is the one the shutdown wrote.
	if err := os.Remove(stateFile); err != nil {
		cancel()
		<-done
		t.Fatalf("remove %q: %v", stateFile, err)
	}

	const hold = 200 * time.Millisecond
	start := time.Now()
	cancel()
	time.Sleep(hold)
	if _, err := conn.Write([]byte("\r\n")); err != nil {
		t.Logf("write the rest of the request: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(ShutdownBudget + shutdownMargin):
		t.Fatalf("Run() did not return within %s", ShutdownBudget+shutdownMargin)
	}
	elapsed := time.Since(start)

	// The shutdown waited for the request that was in flight.
	if elapsed < hold {
		t.Errorf("the shutdown returned after %s, want at least %s: the drain of the HTTP server has to wait for the request in flight", elapsed, hold)
	}

	// The job list is persisted in the state directory of the instance.
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read %q: %v", stateFile, err)
	}
	if !strings.Contains(string(data), "job-of-the-test") {
		t.Errorf("%q = %s, want the job of the instance", stateFile, data)
	}
}

// get performs a GET request and returns the body and the status code.
func get(client *http.Client, url string) (string, int, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(body), resp.StatusCode, nil
}

func TestRunPinsTheCacheEnvironment(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)

	cacheDir := filepath.Join(root, "cache")
	hubDir := filepath.Join(root, "cache", "hub")
	t.Setenv("HF_HOME", filepath.Join(root, "stale-home"))
	t.Setenv("HF_HUB_CACHE", filepath.Join(root, "stale-hub"))

	opts := RunOptions{Paths: model, CacheDir: cacheDir, HubDir: hubDir}
	if err := runService(t, opts); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	if got := os.Getenv("HF_HOME"); got != cacheDir {
		t.Errorf("HF_HOME = %q, want the cache dir of the installation %q", got, cacheDir)
	}
	if got := os.Getenv("HF_HUB_CACHE"); got != hubDir {
		t.Errorf("HF_HUB_CACHE = %q, want the hub dir of the installation %q", got, hubDir)
	}
}

func TestBuildConfigUsesTheServiceOwnedPaths(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	runtimepaths.Install(model)
	t.Cleanup(runtimepaths.Reset)

	cacheDir := filepath.Join(root, "cache")
	localDir := filepath.Join(root, "models")
	cfg, err := buildConfig(RunOptions{
		Paths:    model,
		CacheDir: cacheDir,
		LocalDir: localDir,
	}, "127.0.0.1", 8080)
	if err != nil {
		t.Fatalf("buildConfig() = %v, want nil", err)
	}

	if cfg.Addr != "127.0.0.1" || cfg.Port != 8080 {
		t.Errorf("listen = %q:%d, want 127.0.0.1:8080", cfg.Addr, cfg.Port)
	}
	if want := filepath.Join(model.StateDir, "models"); cfg.ModelsDir != want {
		t.Errorf("ModelsDir = %q, want %q", cfg.ModelsDir, want)
	}
	if want := filepath.Join(model.StateDir, "datasets"); cfg.DatasetsDir != want {
		t.Errorf("DatasetsDir = %q, want %q", cfg.DatasetsDir, want)
	}
	if cfg.CacheDir != cacheDir {
		t.Errorf("CacheDir = %q, want %q", cfg.CacheDir, cacheDir)
	}
	if cfg.LocalDir != localDir {
		t.Errorf("LocalDir = %q, want %q", cfg.LocalDir, localDir)
	}
}

// TestBuildConfigInstallsTheRuntimeModel checks that the settings of an
// instance come from its own config file, even when no runtime path model
// was installed before.
//
// The server applies the settings of the instance through the process wide
// path model. Without a model of its own, the instance would read the
// settings of an ambient model instead of its own config file.
func TestBuildConfigInstallsTheRuntimeModel(t *testing.T) {
	// No model is installed: the interactive resolution is in place.
	runtimepaths.Reset()
	t.Cleanup(runtimepaths.Reset)

	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	if err := os.MkdirAll(filepath.Dir(model.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	cacheRoot := filepath.Join(root, "from-config-file")
	data, err := json.Marshal(map[string]string{
		"cache-dir": cacheRoot,
		"token":     "hf_token_of_the_instance",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model.ConfigFile, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := buildConfig(RunOptions{
		Paths:    model,
		CacheDir: "/registered/cache",
	}, "127.0.0.1", 8080)
	if err != nil {
		t.Fatalf("buildConfig() = %v, want nil", err)
	}

	if cfg.CacheDir != cacheRoot {
		t.Errorf("CacheDir = %q, want the cache dir of the config file of the instance %q", cfg.CacheDir, cacheRoot)
	}
	// The token is applied through the path model of the instance, so it
	// is the token of the config file of the instance and never the one
	// of an ambient config file.
	if cfg.Token != "hf_token_of_the_instance" {
		t.Errorf("Token = %q, want the token of the config file of the instance", cfg.Token)
	}
	if got := runtimepaths.Current(); got != model {
		t.Errorf("runtimepaths.Current() = %+v, want the model of the instance %+v", got, model)
	}
}

// serviceInstance writes the config file of a service instance with the given
// fields and installs the runtime path model that owns it. The model is
// restored when the test ends.
func serviceInstance(t *testing.T, fields map[string]string) runtimepaths.Paths {
	t.Helper()
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	if err := os.MkdirAll(filepath.Dir(model.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model.ConfigFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	runtimepaths.Install(model)
	t.Cleanup(runtimepaths.Reset)
	return model
}

// TestBuildConfigPrefersTheConfigFileOfTheInstance checks the coherence
// rule of a service instance: the config file of the instance wins over the
// registration.
//
// The user changes the cache directory and the output directory in the
// settings of the instance, so the next start of the instance has to use the
// changed value. The registration is only the fallback.
func TestBuildConfigPrefersTheConfigFileOfTheInstance(t *testing.T) {
	model := serviceInstance(t, map[string]string{
		"cache-dir": "/from/config/file",
		"local-dir": "/from/config/file/models",
	})
	t.Setenv("HF_HOME", "/stale/home")
	t.Setenv("HF_HUB_CACHE", "/stale/hub")

	cfg, err := buildConfig(RunOptions{
		Paths:    model,
		CacheDir: "/registered/cache",
		HubDir:   "/registered/cache/hub",
		LocalDir: "/registered/models",
	}, "127.0.0.1", 8080)
	if err != nil {
		t.Fatalf("buildConfig() = %v, want nil", err)
	}

	if cfg.CacheDir != "/from/config/file" {
		t.Errorf("CacheDir = %q, want the cache dir of the config file %q", cfg.CacheDir, "/from/config/file")
	}
	if cfg.LocalDir != "/from/config/file/models" {
		t.Errorf("LocalDir = %q, want the local dir of the config file %q", cfg.LocalDir, "/from/config/file/models")
	}
	if got := os.Getenv("HF_HOME"); got != "/from/config/file" {
		t.Errorf("HF_HOME = %q, want the cache dir of the config file %q", got, "/from/config/file")
	}
	if got, want := os.Getenv("HF_HUB_CACHE"), filepath.Join("/from/config/file", "hub"); got != want {
		t.Errorf("HF_HUB_CACHE = %q, want %q", got, want)
	}
}

// TestBuildConfigKeepsTheRegistrationWithoutAConfigFileValue checks the
// fallback of the coherence rule: an instance whose config file does not
// define a directory uses the directory of its registration.
func TestBuildConfigKeepsTheRegistrationWithoutAConfigFileValue(t *testing.T) {
	model := serviceInstance(t, map[string]string{})
	t.Setenv("HF_HOME", "/stale/home")
	t.Setenv("HF_HUB_CACHE", "/stale/hub")

	cfg, err := buildConfig(RunOptions{
		Paths:    model,
		CacheDir: "/registered/cache",
		HubDir:   "/registered/cache/hub",
		LocalDir: "/registered/models",
	}, "127.0.0.1", 8080)
	if err != nil {
		t.Fatalf("buildConfig() = %v, want nil", err)
	}

	if cfg.CacheDir != "/registered/cache" {
		t.Errorf("CacheDir = %q, want the registered cache dir %q", cfg.CacheDir, "/registered/cache")
	}
	if cfg.LocalDir != "/registered/models" {
		t.Errorf("LocalDir = %q, want the registered local dir %q", cfg.LocalDir, "/registered/models")
	}
	if got := os.Getenv("HF_HOME"); got != "/registered/cache" {
		t.Errorf("HF_HOME = %q, want the registered cache dir %q", got, "/registered/cache")
	}
	if got := os.Getenv("HF_HUB_CACHE"); got != "/registered/cache/hub" {
		t.Errorf("HF_HUB_CACHE = %q, want the registered hub dir %q", got, "/registered/cache/hub")
	}
}

// TestBuildConfigDerivesTheHubDir checks the hub rule of an instance.
//
// A config file that defines the cache root owns the whole cache layout, so
// the hub directory is the "hub" child of that root. Without a cache root
// from the config file the registration keeps its hub directory, and a
// registration without one falls back to the "hub" child of the effective
// cache root.
func TestBuildConfigDerivesTheHubDir(t *testing.T) {
	t.Run("the config file owns the hub directory", func(t *testing.T) {
		model := serviceInstance(t, map[string]string{"cache-dir": "/from/config/file"})
		t.Setenv("HF_HOME", "/stale/home")
		t.Setenv("HF_HUB_CACHE", "/stale/hub")

		if _, err := buildConfig(RunOptions{
			Paths:    model,
			CacheDir: "/registered/cache",
			HubDir:   "/registered/cache/hub",
		}, "127.0.0.1", 8080); err != nil {
			t.Fatalf("buildConfig() = %v, want nil", err)
		}
		if got, want := os.Getenv("HF_HUB_CACHE"), filepath.Join("/from/config/file", "hub"); got != want {
			t.Errorf("HF_HUB_CACHE = %q, want %q", got, want)
		}
	})

	t.Run("the registration keeps its hub directory", func(t *testing.T) {
		model := serviceInstance(t, map[string]string{})
		t.Setenv("HF_HOME", "/stale/home")
		t.Setenv("HF_HUB_CACHE", "/stale/hub")

		if _, err := buildConfig(RunOptions{
			Paths:    model,
			CacheDir: "/registered/cache",
			HubDir:   "/registered/hub",
		}, "127.0.0.1", 8080); err != nil {
			t.Fatalf("buildConfig() = %v, want nil", err)
		}
		if got := os.Getenv("HF_HUB_CACHE"); got != "/registered/hub" {
			t.Errorf("HF_HUB_CACHE = %q, want the registered hub dir %q", got, "/registered/hub")
		}
	})

	t.Run("the cache root owns the hub directory", func(t *testing.T) {
		model := serviceInstance(t, map[string]string{})
		t.Setenv("HF_HOME", "/stale/home")
		t.Setenv("HF_HUB_CACHE", "/stale/hub")

		if _, err := buildConfig(RunOptions{
			Paths:    model,
			CacheDir: "/registered/cache",
		}, "127.0.0.1", 8080); err != nil {
			t.Fatalf("buildConfig() = %v, want nil", err)
		}
		if got, want := os.Getenv("HF_HUB_CACHE"), filepath.Join("/registered/cache", "hub"); got != want {
			t.Errorf("HF_HUB_CACHE = %q, want %q", got, want)
		}
	})
}

// TestBuildConfigAppliesTheLocalDirRule checks the output directory of an
// instance: the local-dir of the config file of the instance wins over the
// registered output directory.
func TestBuildConfigAppliesTheLocalDirRule(t *testing.T) {
	t.Run("the config file wins", func(t *testing.T) {
		model := serviceInstance(t, map[string]string{"local-dir": "/from/config/file/models"})
		t.Setenv("HF_HOME", "/stale/home")
		t.Setenv("HF_HUB_CACHE", "/stale/hub")

		cfg, err := buildConfig(RunOptions{
			Paths:    model,
			CacheDir: "/registered/cache",
			LocalDir: "/registered/models",
		}, "127.0.0.1", 8080)
		if err != nil {
			t.Fatalf("buildConfig() = %v, want nil", err)
		}
		if cfg.LocalDir != "/from/config/file/models" {
			t.Errorf("LocalDir = %q, want the local dir of the config file %q", cfg.LocalDir, "/from/config/file/models")
		}
	})

	t.Run("the registration is the fallback", func(t *testing.T) {
		model := serviceInstance(t, map[string]string{})
		t.Setenv("HF_HOME", "/stale/home")
		t.Setenv("HF_HUB_CACHE", "/stale/hub")

		cfg, err := buildConfig(RunOptions{
			Paths:    model,
			CacheDir: "/registered/cache",
			LocalDir: "/registered/models",
		}, "127.0.0.1", 8080)
		if err != nil {
			t.Fatalf("buildConfig() = %v, want nil", err)
		}
		if cfg.LocalDir != "/registered/models" {
			t.Errorf("LocalDir = %q, want the registered local dir %q", cfg.LocalDir, "/registered/models")
		}
	})
}

// TestBuildConfigPinsTheCacheEnvironment checks that the cache environment
// of the process carries the effective values of the instance.
//
// A supervisor hands over whatever environment it happens to have, so the
// pinning has to replace it with the values of the instance. The pinning
// happens after the configuration was loaded, so the values are the ones
// the instance really uses.
func TestBuildConfigPinsTheCacheEnvironment(t *testing.T) {
	model := serviceInstance(t, map[string]string{"cache-dir": "/from/config/file"})
	t.Setenv("HF_HOME", "/stale/home")
	t.Setenv("HF_HUB_CACHE", "/stale/hub")

	if _, err := buildConfig(RunOptions{
		Paths:    model,
		CacheDir: "/registered/cache",
		HubDir:   "/registered/cache/hub",
	}, "127.0.0.1", 8080); err != nil {
		t.Fatalf("buildConfig() = %v, want nil", err)
	}

	if got := os.Getenv("HF_HOME"); got != "/from/config/file" {
		t.Errorf("HF_HOME = %q, want the effective cache root %q", got, "/from/config/file")
	}
	if got, want := os.Getenv("HF_HUB_CACHE"), filepath.Join("/from/config/file", "hub"); got != want {
		t.Errorf("HF_HUB_CACHE = %q, want the effective hub dir %q", got, want)
	}
}

// TestRunUsesTheConfigFileOfTheInstance checks the coherence rule of a
// running instance.
//
// The config file of the instance wins over the registration, so the cache
// root, the hub directory and the output directory the instance reports are
// the ones the user changed in the settings of the instance.
func TestRunUsesTheConfigFileOfTheInstance(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	if err := os.MkdirAll(filepath.Dir(model.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	cacheRoot := filepath.Join(root, "from-config-file")
	localDir := filepath.Join(root, "from-config-file", "models")
	data, err := json.Marshal(map[string]string{
		"cache-dir": cacheRoot,
		"local-dir": localDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model.ConfigFile, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// The supervisor handed over a stale environment and a registration
	// with other directories: neither of them may win over the config file.
	t.Setenv("HF_HOME", filepath.Join(root, "stale-home"))
	t.Setenv("HF_HUB_CACHE", filepath.Join(root, "stale-hub"))
	t.Cleanup(runtimepaths.Reset)

	port := freePort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Paths:    model,
			Listen:   addr,
			CacheDir: filepath.Join(root, "registered", "cache"),
			HubDir:   filepath.Join(root, "registered", "cache", "hub"),
			LocalDir: filepath.Join(root, "registered", "models"),
		})
	}()

	settings, settingsErr := waitForSettings(t, addr)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if settingsErr != nil {
		t.Fatalf("the settings of the instance: %v", settingsErr)
	}

	if settings.CacheDir != cacheRoot {
		t.Errorf("the cache dir of the instance = %q, want the cache dir of the config file %q", settings.CacheDir, cacheRoot)
	}
	if settings.ConfiguredCacheDir != cacheRoot {
		t.Errorf("the configured cache dir of the instance = %q, want the cache dir of the config file %q", settings.ConfiguredCacheDir, cacheRoot)
	}
	if want := filepath.Join(cacheRoot, "hub"); settings.EffectiveHubDir != want {
		t.Errorf("the effective hub dir of the instance = %q, want %q", settings.EffectiveHubDir, want)
	}
	if settings.LocalDir != localDir {
		t.Errorf("the local dir of the instance = %q, want the local dir of the config file %q", settings.LocalDir, localDir)
	}
}

// waitForSettings reads the settings a running instance reports.
func waitForSettings(t *testing.T, addr string) (server.SettingsResponse, error) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://" + addr + "/api/settings"
	var lastErr error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		body, status, err := get(client, url)
		switch {
		case err != nil:
			lastErr = err
		case status != http.StatusOK:
			lastErr = fmt.Errorf("status %d: %s", status, body)
		default:
			var settings server.SettingsResponse
			if err := json.Unmarshal([]byte(body), &settings); err != nil {
				return settings, fmt.Errorf("decode the settings: %w", err)
			}
			return settings, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return server.SettingsResponse{}, fmt.Errorf("the settings endpoint never answered: %v", lastErr)
}

// TestServiceDirectoriesAreOwnerOnly checks that the state directory and the
// config directory of an instance are created owner-only.
//
// They hold the job list, the download history, the mirror targets and the
// credentials of the instance, so a directory that is missing at the start
// of an instance must not be recreated world-readable.
func TestServiceDirectoriesAreOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows mode bits are emulated and do not represent DACL permissions")
	}

	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	runtimepaths.Install(model)
	t.Cleanup(runtimepaths.Reset)

	if err := server.SaveConfigFile(&server.ConfigFile{CacheDir: "/from/config/file"}); err != nil {
		t.Fatalf("SaveConfigFile() = %v, want nil", err)
	}
	if err := server.SaveJobsState(nil); err != nil {
		t.Fatalf("SaveJobsState() = %v, want nil", err)
	}
	if err := server.AppendHistory(&server.Job{ID: "job", Status: server.JobStatusCompleted}); err != nil {
		t.Fatalf("AppendHistory() = %v, want nil", err)
	}

	for _, dir := range []string{filepath.Dir(model.ConfigFile), model.StateDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("the mode of %s = %04o, want 0700", dir, got)
		}
	}
}

func TestBuildConfigKeepsIPv6HostsBracketed(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	runtimepaths.Install(model)
	t.Cleanup(runtimepaths.Reset)

	// The server composes the bind address as "<Addr>:<Port>": an IPv6 host
	// must keep its brackets to stay a valid socket address.
	cfg, err := buildConfig(RunOptions{Paths: model}, "::1", 8080)
	if err != nil {
		t.Fatalf("buildConfig() = %v, want nil", err)
	}
	if cfg.Addr != "[::1]" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, "[::1]")
	}
}

func TestBuildConfigRejectsARelativeStateDir(t *testing.T) {
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(t.TempDir(), "etc", "hfdesk.json"),
		StateDir:   "var",
		LogDir:     filepath.Join(t.TempDir(), "logs"),
	}
	if _, err := buildConfig(RunOptions{Paths: model}, "127.0.0.1", 8080); err == nil {
		t.Error("buildConfig() = nil, want an error for a relative state dir")
	}
}

func TestBuildConfigTakesTheTokenFromTheConfigFile(t *testing.T) {
	root := t.TempDir()
	configFile := filepath.Join(root, "etc", "hfdesk.json")
	if err := os.MkdirAll(filepath.Dir(configFile), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]string{"token": "hf_file_token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, data, 0o600); err != nil {
		t.Fatal(err)
	}

	model := runtimepaths.Paths{
		ConfigFile: configFile,
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	runtimepaths.Install(model)
	t.Cleanup(runtimepaths.Reset)

	// The environment must not contribute a credential: the token of the
	// service-owned config file is the only source.
	t.Setenv("HF_TOKEN", "hf_environment_token")
	cfg, err := buildConfig(RunOptions{Paths: model}, "127.0.0.1", 8080)
	if err != nil {
		t.Fatalf("buildConfig() = %v, want nil", err)
	}
	if cfg.Token != "hf_file_token" {
		t.Errorf("Token = %q, want the token of the config file", cfg.Token)
	}
	if got := os.Getenv("HF_TOKEN"); got != "" {
		t.Errorf("HF_TOKEN = %q, want it dropped from the service environment", got)
	}
}

// TestServiceBindsLoopbackWhileTheInteractiveServerKeepsItsDefault checks
// the contrast the service mode is built on.
//
// The interactive server keeps its own default: it binds every interface, so
// the dashboard of an interactive run is reachable. A service instance binds
// loopback instead: a supervised instance has no user sitting in front of it
// and must not expose the dashboard to the network by accident.
func TestServiceBindsLoopbackWhileTheInteractiveServerKeepsItsDefault(t *testing.T) {
	// The interactive default is intentionally unchanged by the service
	// mode: an interactive run of the server still binds 0.0.0.0.
	if got := server.DefaultConfig().Addr; got != "0.0.0.0" {
		t.Errorf("server.DefaultConfig().Addr = %q, want %q", got, "0.0.0.0")
	}

	// The default listen address of a service instance is loopback.
	host, port, err := ParseListen(DefaultListen)
	if err != nil {
		t.Fatalf("ParseListen(%q) = %v, want nil", DefaultListen, err)
	}
	if host != "127.0.0.1" {
		t.Errorf("the host of the default listen address %q = %q, want %q", DefaultListen, host, "127.0.0.1")
	}
	if !IsLoopbackListen(DefaultListen) {
		t.Errorf("the default listen address %q is not a loopback address", DefaultListen)
	}

	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	runtimepaths.Install(model)
	t.Cleanup(runtimepaths.Reset)

	// The configuration a service instance builds binds loopback, even
	// though the interactive default of the server binds every interface.
	cfg, err := buildConfig(RunOptions{Paths: model}, host, port)
	if err != nil {
		t.Fatalf("buildConfig() = %v, want nil", err)
	}
	if cfg.Addr != "127.0.0.1" {
		t.Errorf("the address of the service configuration = %q, want %q", cfg.Addr, "127.0.0.1")
	}
	if cfg.Port != port {
		t.Errorf("the port of the service configuration = %d, want %d", cfg.Port, port)
	}

	// The registered argument vector carries the loopback address, so the
	// instance the supervisor starts binds loopback as well.
	if got := listenArg(""); got != DefaultListen {
		t.Errorf("listenArg(\"\") = %q, want the default listen address %q", got, DefaultListen)
	}
	args := RunArgs(InstallOptions{Paths: model})
	if got := argumentValue(args, "--listen"); got != DefaultListen {
		t.Errorf("the listen address of the argument vector = %q, want %q", got, DefaultListen)
	}
}

// argumentValue returns the value of a flag of an argument vector.
func argumentValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// guiLaunchMarkers are the markers of a browser or a tray launcher. No file
// of the service package may contain one of them: a supervised instance has
// no desktop session to interact with.
var guiLaunchMarkers = []string{"xdg-open", "rundll32", "systray", "url.dll,FileProtocolHandler"}

// processLaunchMarkers are the markers of a process launch.
var processLaunchMarkers = []string{`"os/exec"`, "exec.Command"}

// The package has two file classes and they are treated differently.
//
// The shared, platform-neutral files (host.go, service.go, runargs.go,
// process.go, paths_*.go) run on every operating system. They own the
// command semantics of the service and not the machine, so they must not
// launch a process at all.
//
// The platform files (manager_*.go, process_*.go) own the supervisor of the
// platform and have to run its command line tool ("systemctl", "launchctl"),
// so a process launch is legitimate there. It is still not a browser and not
// a tray icon.
//
// Neither class may launch a browser or a tray icon.
//
// The class comes from the file name, so a new platform file is covered
// without changing this test.
func TestServiceLayerNeverLaunchesAGUI(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		src := stripComments(string(data))
		if !isPlatformFile(name) {
			for _, marker := range processLaunchMarkers {
				if strings.Contains(src, marker) {
					t.Errorf("%s contains %q: the shared service layer must not launch a process", name, marker)
				}
			}
		}
		for _, marker := range guiLaunchMarkers {
			if strings.Contains(src, marker) {
				t.Errorf("%s contains %q: the service layer must not launch a browser or a tray icon", name, marker)
			}
		}
	}
}

// isPlatformFile reports whether the file is a platform-specific file of the
// package. A platform file owns the supervisor of the platform and may run
// its command line tool.
func isPlatformFile(name string) bool {
	return strings.HasPrefix(name, "manager_") || strings.HasPrefix(name, "process_")
}

// runService starts the headless host with an already cancelled context and
// returns its error. A cancelled context is the normal shutdown path of a
// supervised instance, so the host must return without an error.
func runService(t *testing.T, opts RunOptions) error {
	t.Helper()
	return runServiceWith(t, opts, Run)
}

// runServiceWith starts the given entry point with an already cancelled
// context and returns its error. The entry point is the function under test,
// so the same shutdown path is exercised for every entry point of the
// package.
func runServiceWith(t *testing.T, opts RunOptions, run func(context.Context, RunOptions) error) error {
	t.Helper()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		opts.Listen = "127.0.0.1:" + strconv.Itoa(freePort(t))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err = run(ctx, opts)
		if err == nil || !strings.Contains(err.Error(), "address already in use") {
			return err
		}
	}
	return err
}

// freePort returns a port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// stripComments removes the line comments of a Go source so a doc comment
// does not trip a source marker check.
func stripComments(src string) string {
	var out strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}
