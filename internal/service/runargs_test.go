// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// runArgsPaths returns an absolute path set for the parser tests. The paths
// are built from the temporary directory of the test, so they stay absolute
// on every platform.
func runArgsPaths(t *testing.T) (configFile, stateDir, logDir, cacheDir, hubDir, localDir string) {
	t.Helper()
	root := t.TempDir()
	return filepath.Join(root, "etc", "hfdesk.json"),
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "logs"),
		filepath.Join(root, "cache"),
		filepath.Join(root, "cache", "hub"),
		filepath.Join(root, "models")
}

// runArgsVector returns the canonical argument vector of an instance with
// every optional directory set.
func runArgsVector(t *testing.T) []string {
	t.Helper()
	configFile, stateDir, logDir, cacheDir, hubDir, localDir := runArgsPaths(t)
	return []string{
		"service", "run",
		"--config", configFile,
		"--state-dir", stateDir,
		"--log-dir", logDir,
		"--listen", "127.0.0.1:9090",
		"--cache-dir", cacheDir,
		"--hub-dir", hubDir,
		"--local-dir", localDir,
	}
}

func TestParseRunArgsParsesTheFullVector(t *testing.T) {
	configFile, stateDir, logDir, cacheDir, hubDir, localDir := runArgsPaths(t)

	got, err := ParseRunArgs([]string{
		"service", "run",
		"--config", configFile,
		"--state-dir", stateDir,
		"--log-dir", logDir,
		"--listen", "127.0.0.1:9090",
		"--cache-dir", cacheDir,
		"--hub-dir", hubDir,
		"--local-dir", localDir,
	})
	if err != nil {
		t.Fatalf("ParseRunArgs() = %v, want nil", err)
	}

	want := RunOptions{
		Paths: runtimepaths.Paths{
			ConfigFile: configFile,
			StateDir:   stateDir,
			LogDir:     logDir,
		},
		Listen:   "127.0.0.1:9090",
		CacheDir: cacheDir,
		HubDir:   hubDir,
		LocalDir: localDir,
	}
	if got != want {
		t.Errorf("ParseRunArgs() = %+v, want %+v", got, want)
	}
}

func TestParseRunArgsAcceptsTheEqualsForm(t *testing.T) {
	configFile, stateDir, logDir, cacheDir, hubDir, localDir := runArgsPaths(t)

	spaceForm := []string{
		"service", "run",
		"--config", configFile,
		"--state-dir", stateDir,
		"--log-dir", logDir,
		"--listen", "127.0.0.1:9090",
		"--cache-dir", cacheDir,
		"--hub-dir", hubDir,
		"--local-dir", localDir,
	}
	equalsForm := []string{
		"service", "run",
		"--config=" + configFile,
		"--state-dir=" + stateDir,
		"--log-dir=" + logDir,
		"--listen=127.0.0.1:9090",
		"--cache-dir=" + cacheDir,
		"--hub-dir=" + hubDir,
		"--local-dir=" + localDir,
	}

	fromSpace, err := ParseRunArgs(spaceForm)
	if err != nil {
		t.Fatalf("ParseRunArgs(%v) = %v, want nil", spaceForm, err)
	}
	fromEquals, err := ParseRunArgs(equalsForm)
	if err != nil {
		t.Fatalf("ParseRunArgs(%v) = %v, want nil", equalsForm, err)
	}
	if fromSpace != fromEquals {
		t.Errorf("ParseRunArgs() = %+v for the equals form, want %+v", fromEquals, fromSpace)
	}
}

func TestParseRunArgsAcceptsTheFlagsWithoutTheCommandTokens(t *testing.T) {
	configFile, stateDir, logDir, cacheDir, hubDir, localDir := runArgsPaths(t)

	flags := []string{
		"--config", configFile,
		"--state-dir", stateDir,
		"--log-dir", logDir,
		"--cache-dir", cacheDir,
		"--hub-dir", hubDir,
		"--local-dir", localDir,
	}

	got, err := ParseRunArgs(flags)
	if err != nil {
		t.Fatalf("ParseRunArgs(%v) = %v, want nil", flags, err)
	}
	withCommand, err := ParseRunArgs(append([]string{"service", "run"}, flags...))
	if err != nil {
		t.Fatalf("ParseRunArgs() = %v, want nil", err)
	}
	if got != withCommand {
		t.Errorf("ParseRunArgs() = %+v, want %+v for the vector with the command tokens", got, withCommand)
	}
}

func TestParseRunArgsRequiresTheMandatoryFlags(t *testing.T) {
	base := runArgsVector(t)

	cases := []struct {
		name string
		flag string
	}{
		{"missing config", "--config"},
		{"missing state dir", "--state-dir"},
		{"missing log dir", "--log-dir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := dropFlag(base, tc.flag)
			if _, err := ParseRunArgs(args); err == nil {
				t.Fatalf("ParseRunArgs(%v) = nil, want an error", args)
			} else if !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("ParseRunArgs() = %v, want the error to name %s", err, tc.flag)
			}
		})
	}
}

func TestParseRunArgsRejectsRelativePaths(t *testing.T) {
	base := runArgsVector(t)

	cases := []struct {
		name  string
		flag  string
		value string
	}{
		{"relative config", "--config", "etc/hfdesk.json"},
		{"relative state dir", "--state-dir", "var"},
		{"relative log dir", "--log-dir", "var/logs"},
		{"relative cache dir", "--cache-dir", "cache"},
		{"relative hub dir", "--hub-dir", "cache/hub"},
		{"relative local dir", "--local-dir", "models"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := replaceFlag(base, tc.flag, tc.value)
			if _, err := ParseRunArgs(args); err == nil {
				t.Fatalf("ParseRunArgs(%v) = nil, want an error", args)
			} else if !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("ParseRunArgs() = %v, want the error to name %s", err, tc.flag)
			}
		})
	}
}

func TestParseRunArgsValidatesTheListenAddress(t *testing.T) {
	base := runArgsVector(t)

	for _, addr := range []string{":8080", "127.0.0.1:0", "127.0.0.1:70000", ""} {
		args := replaceFlag(base, "--listen", addr)
		if _, err := ParseRunArgs(args); err == nil {
			t.Errorf("ParseRunArgs(%v) = nil, want an error for the listen address %q", args, addr)
		}
	}

	accepted := []struct {
		addr string
		want string
	}{
		{"127.0.0.1:9090", "127.0.0.1:9090"},
		{"[::1]:8080", "[::1]:8080"},
		{"localhost:8080", "localhost:8080"},
	}
	for _, tc := range accepted {
		args := replaceFlag(base, "--listen", tc.addr)
		got, err := ParseRunArgs(args)
		if err != nil {
			t.Errorf("ParseRunArgs(%v) = %v, want nil", args, err)
			continue
		}
		if got.Listen != tc.want {
			t.Errorf("Listen = %q, want %q", got.Listen, tc.want)
		}
	}
}

func TestParseRunArgsIgnoresTheOrderOfTheFlags(t *testing.T) {
	configFile, stateDir, logDir, _, _, _ := runArgsPaths(t)

	got, err := ParseRunArgs([]string{
		"--listen", "127.0.0.1:9090",
		"--log-dir", logDir,
		"--state-dir", stateDir,
		"--config", configFile,
	})
	if err != nil {
		t.Fatalf("ParseRunArgs() = %v, want nil", err)
	}
	want := RunOptions{
		Paths: runtimepaths.Paths{
			ConfigFile: configFile,
			StateDir:   stateDir,
			LogDir:     logDir,
		},
		Listen: "127.0.0.1:9090",
	}
	if got != want {
		t.Errorf("ParseRunArgs() = %+v, want %+v", got, want)
	}
}

func TestParseRunArgsUsesTheDefaultListen(t *testing.T) {
	args := dropFlag(runArgsVector(t), "--listen")

	got, err := ParseRunArgs(args)
	if err != nil {
		t.Fatalf("ParseRunArgs(%v) = %v, want nil", args, err)
	}
	if got.Listen != DefaultListen {
		t.Errorf("Listen = %q, want the default %q", got.Listen, DefaultListen)
	}
}

func TestParseRunArgsRejectsAnUnknownFlagWithoutEchoingItsValue(t *testing.T) {
	const secret = "hf_secret"

	cases := [][]string{
		append(runArgsVector(t), "--token", secret),
		append(runArgsVector(t), "--token="+secret),
	}
	for _, args := range cases {
		_, err := ParseRunArgs(args)
		if err == nil {
			t.Fatalf("ParseRunArgs(%v) = nil, want an error", args)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("ParseRunArgs() error leaks the value of the unknown flag: %v", err)
		}
	}
}

func TestParseRunArgsRejectsAPositionalArgument(t *testing.T) {
	args := append(runArgsVector(t), "leftover")
	if _, err := ParseRunArgs(args); err == nil {
		t.Fatalf("ParseRunArgs(%v) = nil, want an error", args)
	}
}

func TestParseRunArgsRoundTripsTheRunArgsVector(t *testing.T) {
	configFile, stateDir, logDir, cacheDir, hubDir, localDir := runArgsPaths(t)

	opts := InstallOptions{
		Scope:  ScopeUser,
		Listen: "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: configFile,
			StateDir:   stateDir,
			LogDir:     logDir,
		},
		CacheDir: cacheDir,
		HubDir:   hubDir,
		LocalDir: localDir,
	}

	got, err := ParseRunArgs(RunArgs(opts))
	if err != nil {
		t.Fatalf("ParseRunArgs(RunArgs(opts)) = %v, want nil", err)
	}
	want := RunOptions{
		Paths:    opts.Paths,
		Listen:   opts.Listen,
		CacheDir: opts.CacheDir,
		HubDir:   opts.HubDir,
		LocalDir: opts.LocalDir,
	}
	if got != want {
		t.Errorf("ParseRunArgs(RunArgs(opts)) = %+v, want %+v", got, want)
	}
}

func TestRunArgsHelpListsEveryFlag(t *testing.T) {
	help := RunArgsHelp()
	want := []string{"config", "state-dir", "log-dir", "listen", "cache-dir", "hub-dir", "local-dir"}

	if len(help) != len(want) {
		t.Fatalf("RunArgsHelp() = %+v, want %d flags", help, len(want))
	}
	for i, entry := range help {
		if entry.Name != want[i] {
			t.Errorf("RunArgsHelp()[%d].Name = %q, want %q", i, entry.Name, want[i])
		}
		if strings.TrimSpace(entry.Usage) == "" {
			t.Errorf("RunArgsHelp()[%d] (%s) has no usage text", i, entry.Name)
		}
		if strings.Contains(entry.Usage, "\n") {
			t.Errorf("RunArgsHelp()[%d] (%s) usage is not a single line", i, entry.Name)
		}
	}
}

// dropFlag removes the flag and its value from the argument vector.
func dropFlag(args []string, flag string) []string {
	for i := 0; i < len(args); i++ {
		if args[i] != flag {
			continue
		}
		out := make([]string, 0, len(args)-2)
		out = append(out, args[:i]...)
		if i+1 < len(args) {
			out = append(out, args[i+2:]...)
		}
		return out
	}
	return args
}

// replaceFlag replaces the value of the flag in the argument vector.
func replaceFlag(args []string, flag string, value string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out); i++ {
		if out[i] == flag && i+1 < len(out) {
			out[i+1] = value
			return out
		}
	}
	return out
}

// TestRunArgsHelpMatchesTheParsedFlags keeps the help text and the parser in
// step: every flag the parser knows is listed with a usage.
func TestRunArgsHelpMatchesTheParsedFlags(t *testing.T) {
	help := RunArgsHelp()
	names := make([]string, 0, len(help))
	for _, entry := range help {
		names = append(names, entry.Name)
	}

	parsed := runArgsVector(t)
	for i := 0; i < len(parsed); i++ {
		arg := parsed[i]
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		name := strings.TrimPrefix(arg, "--")
		found := false
		for _, candidate := range names {
			if candidate == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the flag %s is parsed but missing from RunArgsHelp() = %v", arg, names)
		}
	}
	if !reflect.DeepEqual(names, []string{"config", "state-dir", "log-dir", "listen", "cache-dir", "hub-dir", "local-dir"}) {
		t.Errorf("RunArgsHelp() = %v, want the flags in the documented order", names)
	}
}
