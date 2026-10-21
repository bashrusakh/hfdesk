// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// fullInstallOptions returns a fully populated installation. The paths carry
// no credential-looking fragment so the argument vector checks stay exact.
func fullInstallOptions(t *testing.T) InstallOptions {
	t.Helper()
	const root = "/opt/hfdesk"
	return InstallOptions{
		Scope:      ScopeUser,
		Executable: root + "/bin/hfdesk",
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: root + "/etc/hfdesk.json",
			StateDir:   root + "/var",
			LogDir:     root + "/var/logs",
		},
		CacheDir: root + "/cache",
		HubDir:   root + "/cache/hub",
		LocalDir: root + "/models",
	}
}

func TestRunArgsMatchesCanonicalArgv(t *testing.T) {
	got := RunArgs(fullInstallOptions(t))
	want := []string{
		"service",
		"run",
		"--config", "/opt/hfdesk/etc/hfdesk.json",
		"--state-dir", "/opt/hfdesk/var",
		"--log-dir", "/opt/hfdesk/var/logs",
		"--listen", "127.0.0.1:8080",
		"--cache-dir", "/opt/hfdesk/cache",
		"--hub-dir", "/opt/hfdesk/cache/hub",
		"--local-dir", "/opt/hfdesk/models",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RunArgs() =\n%v\nwant\n%v", got, want)
	}
}

func TestRunArgsOmitsUnsetDirectories(t *testing.T) {
	opts := InstallOptions{
		Scope:  ScopeSystem,
		Listen: "127.0.0.1:9090",
		Paths: runtimepaths.Paths{
			ConfigFile: "/opt/hfdesk/etc/hfdesk.json",
			StateDir:   "/opt/hfdesk/var",
			LogDir:     "/opt/hfdesk/var/logs",
		},
	}
	got := RunArgs(opts)
	want := []string{
		"service",
		"run",
		"--config", "/opt/hfdesk/etc/hfdesk.json",
		"--state-dir", "/opt/hfdesk/var",
		"--log-dir", "/opt/hfdesk/var/logs",
		"--listen", "127.0.0.1:9090",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RunArgs() =\n%v\nwant\n%v", got, want)
	}
	for _, arg := range got {
		if strings.HasPrefix(arg, "--cache-dir") || strings.HasPrefix(arg, "--hub-dir") || strings.HasPrefix(arg, "--local-dir") {
			t.Errorf("RunArgs() = %v, want no directory flag for an unset directory", got)
		}
	}
}

func TestRunArgsUsesDefaultListenWhenUnset(t *testing.T) {
	opts := fullInstallOptions(t)
	opts.Listen = ""
	got := RunArgs(opts)
	if want := DefaultListen; !containsValue(got, want) {
		t.Errorf("RunArgs() = %v, want the default listen %q", got, want)
	}
}

func TestRunArgsCarriesNoCredential(t *testing.T) {
	const secret = "hf_SECRETTOKEN_zzz"

	opts := fullInstallOptions(t)
	// InstallOptions has no credential field today. Every field that looks
	// like one is filled with the sentinel, so the check keeps its meaning
	// when such a field is added.
	setCredentialFields(&opts, secret)

	args := RunArgs(opts)
	for _, arg := range args {
		if strings.Contains(arg, secret) {
			t.Errorf("RunArgs() leaks a credential: %v", args)
		}
	}
	for _, arg := range args {
		if looksLikeCredential(arg) {
			t.Errorf("RunArgs() contains the credential-looking argument %q: %v", arg, args)
		}
	}
}

func TestParseListenAcceptsValidAddresses(t *testing.T) {
	cases := []struct {
		addr string
		host string
		port int
	}{
		{"127.0.0.1:8080", "127.0.0.1", 8080},
		{"[::1]:8080", "::1", 8080},
		{"localhost:8080", "localhost", 8080},
		{"127.0.0.1:1", "127.0.0.1", 1},
		{"127.0.0.1:65535", "127.0.0.1", 65535},
	}
	for _, tc := range cases {
		host, port, err := ParseListen(tc.addr)
		if err != nil {
			t.Errorf("ParseListen(%q) = error %v, want (%q, %d)", tc.addr, err, tc.host, tc.port)
			continue
		}
		if host != tc.host || port != tc.port {
			t.Errorf("ParseListen(%q) = (%q, %d), want (%q, %d)", tc.addr, host, port, tc.host, tc.port)
		}
	}
}

func TestParseListenRejectsInvalidAddresses(t *testing.T) {
	invalid := []string{
		"",
		"   ",
		":8080",
		"127.0.0.1",
		"127.0.0.1:0",
		"127.0.0.1:70000",
		"127.0.0.1:http",
		"[::1]",
		"::1:8080",
	}
	for _, addr := range invalid {
		host, port, err := ParseListen(addr)
		if err == nil {
			t.Errorf("ParseListen(%q) = (%q, %d), want an error", addr, host, port)
		}
	}
}

func TestIsLoopbackListen(t *testing.T) {
	loopback := []string{
		"127.0.0.1:8080",
		"127.5.5.5:1",
		"[::1]:8080",
		"localhost:8080",
	}
	for _, addr := range loopback {
		if !IsLoopbackListen(addr) {
			t.Errorf("IsLoopbackListen(%q) = false, want true", addr)
		}
	}

	notLoopback := []string{
		"0.0.0.0:8080",
		"192.168.1.10:8080",
		"[::]:8080",
		":8080",
		"",
		"example.com:8080",
	}
	for _, addr := range notLoopback {
		if IsLoopbackListen(addr) {
			t.Errorf("IsLoopbackListen(%q) = true, want false", addr)
		}
	}
}

func TestParseScope(t *testing.T) {
	accepted := map[string]Scope{
		"user":   ScopeUser,
		"system": ScopeSystem,
		"USER":   ScopeUser,
		"System": ScopeSystem,
	}
	for input, want := range accepted {
		got, err := ParseScope(input)
		if err != nil {
			t.Errorf("ParseScope(%q) = error %v, want %q", input, err, want)
			continue
		}
		if got != want {
			t.Errorf("ParseScope(%q) = %q, want %q", input, got, want)
		}
	}

	for _, input := range []string{"", "root", "machine"} {
		if got, err := ParseScope(input); err == nil {
			t.Errorf("ParseScope(%q) = %q, want an error", input, got)
		}
	}
}

func TestValidateInstallOptionsAcceptsValidInstallation(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "bin", "hfdesk")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: executable,
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var"),
			LogDir:     filepath.Join(root, "var", "logs"),
		},
		CacheDir: filepath.Join(root, "cache"),
		HubDir:   filepath.Join(root, "cache", "hub"),
		LocalDir: filepath.Join(root, "models"),
	}
	if err := ValidateInstallOptions(opts); err != nil {
		t.Fatalf("ValidateInstallOptions() = %v, want nil", err)
	}
}

func TestValidateInstallOptionsRejectsInvalidInstallation(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "bin", "hfdesk")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	valid := func() InstallOptions {
		return InstallOptions{
			Scope:      ScopeUser,
			Executable: executable,
			Listen:     "127.0.0.1:8080",
			Paths: runtimepaths.Paths{
				ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
				StateDir:   filepath.Join(root, "var"),
				LogDir:     filepath.Join(root, "var", "logs"),
			},
			CacheDir: filepath.Join(root, "cache"),
			HubDir:   filepath.Join(root, "cache", "hub"),
			LocalDir: filepath.Join(root, "models"),
		}
	}

	cases := []struct {
		name   string
		mutate func(*InstallOptions)
	}{
		{"empty executable", func(o *InstallOptions) { o.Executable = "" }},
		{"relative executable", func(o *InstallOptions) { o.Executable = "bin/hfdesk" }},
		{"missing executable", func(o *InstallOptions) { o.Executable = filepath.Join(root, "bin", "missing") }},
		{"executable is a directory", func(o *InstallOptions) { o.Executable = filepath.Dir(executable) }},
		{"empty config file", func(o *InstallOptions) { o.Paths.ConfigFile = "" }},
		{"relative config file", func(o *InstallOptions) { o.Paths.ConfigFile = "etc/hfdesk.json" }},
		{"empty state dir", func(o *InstallOptions) { o.Paths.StateDir = "" }},
		{"relative state dir", func(o *InstallOptions) { o.Paths.StateDir = "var" }},
		{"empty log dir", func(o *InstallOptions) { o.Paths.LogDir = "" }},
		{"relative log dir", func(o *InstallOptions) { o.Paths.LogDir = "var/logs" }},
		{"relative cache dir", func(o *InstallOptions) { o.CacheDir = "cache" }},
		{"relative hub dir", func(o *InstallOptions) { o.HubDir = "cache/hub" }},
		{"relative local dir", func(o *InstallOptions) { o.LocalDir = "models" }},
		{"empty listen", func(o *InstallOptions) { o.Listen = "" }},
		{"invalid listen", func(o *InstallOptions) { o.Listen = "127.0.0.1:0" }},
		{"wildcard listen", func(o *InstallOptions) { o.Listen = ":8080" }},
		{"unknown scope", func(o *InstallOptions) { o.Scope = Scope("root") }},
		{"empty scope", func(o *InstallOptions) { o.Scope = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := valid()
			tc.mutate(&opts)
			if err := ValidateInstallOptions(opts); err == nil {
				t.Errorf("ValidateInstallOptions() = nil, want an error")
			}
		})
	}
}

func TestPrepareDirsCreatesOwnedDirectories(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc")
	stateDir := filepath.Join(root, "var")
	logDir := filepath.Join(root, "var", "logs")

	// An existing state directory with loose permissions must be tightened.
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	opts := InstallOptions{
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(configDir, "hfdesk.json"),
			StateDir:   stateDir,
			LogDir:     logDir,
		},
	}
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}

	for _, dir := range []string{configDir, stateDir, logDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("stat %s: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
			continue
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v, want 0700", dir, info.Mode().Perm())
		}
	}
}

func TestPrepareDirsLeavesUnrelatedPathsAlone(t *testing.T) {
	root := t.TempDir()
	unrelatedDir := filepath.Join(root, "other")
	if err := os.MkdirAll(unrelatedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unrelatedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unrelatedFile := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(unrelatedFile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := InstallOptions{
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var"),
			LogDir:     filepath.Join(root, "var", "logs"),
		},
	}
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(unrelatedDir)
		if err != nil {
			t.Fatalf("stat %s: %v", unrelatedDir, err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("%s mode = %v, want the untouched 0755", unrelatedDir, info.Mode().Perm())
		}
	}
	data, err := os.ReadFile(unrelatedFile)
	if err != nil {
		t.Fatalf("read %s: %v", unrelatedFile, err)
	}
	if string(data) != "keep" {
		t.Errorf("%s = %q, want the untouched content", unrelatedFile, data)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	want := []string{"etc", "keep.txt", "other", "var"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("entries of %s = %v, want %v", root, names, want)
	}
}

func TestPrepareDirsRejectsTheFilesystemRoot(t *testing.T) {
	root := t.TempDir()
	opts := InstallOptions{
		Paths: runtimepaths.Paths{
			// A config file directly in the root would tighten the root
			// itself to 0700.
			ConfigFile: string(filepath.Separator) + "hfdesk.json",
			StateDir:   filepath.Join(root, "var"),
			LogDir:     filepath.Join(root, "var", "logs"),
		},
	}
	if err := PrepareDirs(opts); err == nil {
		t.Error("PrepareDirs() = nil, want an error for the filesystem root")
	}
}

func TestNewRejectsUnknownScope(t *testing.T) {
	if m, err := New(Scope("root")); err == nil {
		t.Errorf("New(root) = %v, want an error", m)
	}
}

func TestNewReturnsThePlatformManager(t *testing.T) {
	m, err := New(ScopeUser)
	if err == nil {
		if m == nil {
			t.Error("New() = nil, nil; want a manager")
		}
		return
	}
	// The platform managers are stubs until the platform work lands: the
	// error must say so instead of returning a broken manager.
	if !errors.Is(err, ErrNotImplemented) && !strings.Contains(err.Error(), "not supported on") {
		t.Errorf("New() = %v, want ErrNotImplemented or an unsupported-platform error", err)
	}
	if m != nil {
		t.Errorf("New() = %v, want a nil manager next to the error", m)
	}
}

// containsValue reports whether the argument vector holds the value.
func containsValue(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}

// credentialMarkers are the fragments a credential carries.
var credentialMarkers = []string{"hf_", "token", "password", "passwd", "secret", "credential", "bearer"}

// looksLikeCredential reports whether a value carries a credential marker.
func looksLikeCredential(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range credentialMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// setCredentialFields fills every credential-looking string field of opts
// with the sentinel.
func setCredentialFields(opts *InstallOptions, sentinel string) {
	value := reflect.ValueOf(opts).Elem()
	typ := value.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.String {
			continue
		}
		if !looksLikeCredential(strings.ToLower(field.Name)) {
			continue
		}
		value.Field(i).SetString(sentinel)
	}
}
