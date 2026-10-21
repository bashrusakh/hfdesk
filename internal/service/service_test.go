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
	want := canonicalRunArgs()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RunArgs() =\n%v\nwant\n%v", got, want)
	}
}

// canonicalRunArgs is the exact argument vector of the installation of
// fullInstallOptions: the paths of the instance and its bind address, and
// nothing else.
func canonicalRunArgs() []string {
	return []string{
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

// installOptionStringFields are the string fields of InstallOptions.
//
// Every one of them carries the scope, a path, the bind address or the
// account of an instance. The type carries no credential field at all, so a
// registration can never carry one.
var installOptionStringFields = map[string]bool{
	"Scope":       true,
	"Executable":  true,
	"Listen":      true,
	"CacheDir":    true,
	"HubDir":      true,
	"LocalDir":    true,
	"ServiceUser": true,
}

// TestInstallOptionsCarryNoCredentialField checks that the options of an
// installation carry no credential.
//
// The check reads the actual fields of the type instead of filling the
// fields that look like a credential: a type without a credential field has
// nothing to fill, so a fill would prove nothing. A credential field that
// is added later fails this test.
func TestInstallOptionsCarryNoCredentialField(t *testing.T) {
	typ := reflect.TypeOf(InstallOptions{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.String {
			continue
		}
		if !installOptionStringFields[field.Name] {
			t.Errorf("InstallOptions.%s is a string field that is neither the scope, a path nor the bind address: the type must carry no credential field", field.Name)
		}
	}
}

func TestRunArgsCarriesNoCredential(t *testing.T) {
	opts := fullInstallOptions(t)

	// The argument vector holds the paths of the instance and its bind
	// address and nothing else. The exact vector is asserted, so a value
	// that is not part of it cannot reach a registration.
	args := RunArgs(opts)
	want := canonicalRunArgs()
	if !reflect.DeepEqual(args, want) {
		t.Errorf("RunArgs() =\n%v\nwant\n%v", args, want)
	}

	// The options carry no credential field (see
	// TestInstallOptionsCarryNoCredentialField), so no value of the vector
	// can be a credential in the first place.
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

// TestPrepareDirsCreatesOwnedDirectories checks that the directories an
// installation owns are created owner-only.
//
// They hold the job list, the download history, the mirror targets and the
// credentials of the instance, so a directory that is missing at the start
// of an instance must not be created world-readable.
func TestPrepareDirsCreatesOwnedDirectories(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc")
	stateDir := filepath.Join(root, "var")
	logDir := filepath.Join(root, "var", "logs")

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

// TestPrepareDirsLeavesTheModeOfAnExistingDirectoryAlone checks that the
// installation never changes the permissions of a directory the user named.
//
// An explicit --state-dir or --config can point at a directory that is
// shared with something else. Tightening it to 0700 would silently take it
// away from its other users, so the installation only makes sure that the
// directory exists.
func TestPrepareDirsLeavesTheModeOfAnExistingDirectoryAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows mode bits are emulated and do not represent DACL permissions")
	}

	root := t.TempDir()
	stateDir := filepath.Join(root, "var")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	opts := InstallOptions{
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   stateDir,
			LogDir:     filepath.Join(root, "logs"),
		},
	}
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}

	info, err := os.Stat(stateDir)
	if err != nil {
		t.Fatalf("stat %s: %v", stateDir, err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("the mode of %s is %v, want the untouched 0755", stateDir, info.Mode().Perm())
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

// TestDefaultServiceUserIsTheAccountOfThePlatform checks the default
// account of a machine-wide instance.
//
// The account is a dedicated, stable and unprivileged service account: the
// files it owns have to stay writable across reboots, and the instance
// needs no privilege of root. Windows has no account to name: the instance
// runs as the built-in LocalService account.
func TestDefaultServiceUserIsTheAccountOfThePlatform(t *testing.T) {
	want := map[string]string{
		"linux":   "hfdesk",
		"darwin":  "_hfdesk",
		"windows": "",
	}
	expected, ok := want[runtime.GOOS]
	if !ok {
		expected = ""
	}
	if got := DefaultServiceUser(); got != expected {
		t.Errorf("DefaultServiceUser() = %q, want %q", got, expected)
	}
}

// TestDefaultServiceUserIsStable checks that the default account does not
// depend on the machine or on the user that runs the command.
//
// The account owns the files of the instance, and those files have to stay
// writable across reboots: an account that changed from run to run would
// break the instance at the next restart.
func TestDefaultServiceUserIsStable(t *testing.T) {
	first := DefaultServiceUser()
	for i := 0; i < 3; i++ {
		if got := DefaultServiceUser(); got != first {
			t.Fatalf("DefaultServiceUser() = %q, want the stable value %q", got, first)
		}
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

// TestEffectiveCacheDir checks the cache root of an instance: the cache-dir
// of the config file of the instance wins over the registered cache
// directory, and the registration is the fallback.
func TestEffectiveCacheDir(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	if err := os.MkdirAll(etc, 0o700); err != nil {
		t.Fatal(err)
	}

	withCacheDir := filepath.Join(etc, "with-cache.json")
	if err := os.WriteFile(withCacheDir, []byte(`{"cache-dir":"/from/config/file"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	withoutCacheDir := filepath.Join(etc, "without-cache.json")
	if err := os.WriteFile(withoutCacheDir, []byte(`{"local-dir":"/from/config/file/models"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// A path that cannot be read: a directory is not a config file.
	unreadable := filepath.Join(etc, "unreadable.json")
	if err := os.Mkdir(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		configFile string
		registered string
		want       string
	}{
		{
			name:       "the config file of the instance wins",
			configFile: withCacheDir,
			registered: "/registered/cache",
			want:       "/from/config/file",
		},
		{
			name:       "the registration is the fallback",
			configFile: withoutCacheDir,
			registered: "/registered/cache",
			want:       "/registered/cache",
		},
		{
			name:       "a missing config file falls back",
			configFile: filepath.Join(etc, "missing.json"),
			registered: "/registered/cache",
			want:       "/registered/cache",
		},
		{
			name:       "an unreadable config file falls back",
			configFile: unreadable,
			registered: "/registered/cache",
			want:       "/registered/cache",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveCacheDir(tc.configFile, tc.registered)
			if got != tc.want {
				t.Errorf("EffectiveCacheDir(%q, %q) = %q, want %q", tc.configFile, tc.registered, got, tc.want)
			}
		})
	}
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
//
// The options of an installation carry no credential field today, so the
// fill has nothing to fill. The registrations of the platform managers use
// the helper so their check keeps its meaning when such a field is added.
// The service level test asserts that the type carries none.
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
