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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
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
