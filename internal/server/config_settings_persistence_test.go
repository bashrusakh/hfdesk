// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeTokenFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireTokenFileMode(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return // Windows uses ACLs rather than Unix permission bits.
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestConfigSettingsFormatsAndPermissions(t *testing.T) {
	for _, format := range []string{"new-json", "json", "yaml", "yml"} {
		t.Run(format, func(t *testing.T) {
			path := isolateTokenConfig(t)
			disk := ""
			if format != "new-json" {
				path = filepath.Join(filepath.Dir(path), "hfdesk."+format)
				content := `{"token":"hf_file_credential","connections":7}`
				if format != "json" {
					content = "token: hf_file_credential\nconnections: 7\n"
				}
				writeTokenFixture(t, path, content)
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
				disk = "hf_file_credential"
			}
			if ConfigPath() != path {
				t.Fatal("format selection changed")
			}
			cfg := DefaultConfig()
			cfg.Token = "hf_runtime_only"
			s := New(cfg)
			if response := requireTokenSettingsOK(t, s, `{"connections":6}`); strings.Contains(response, "warning") {
				t.Fatal(response)
			}
			requireTokenState(t, s, cfg.Token, disk)
			requireTokenFileMode(t, path)
			requireTokenSettingsOK(t, s, `{"token":"hf_set_in_file"}`)
			requireTokenState(t, s, "hf_set_in_file", "hf_set_in_file")
			requireTokenSettingsOK(t, s, `{"token":""}`)
			requireTokenState(t, s, "", "")
			requireTokenFileMode(t, path)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte("hf_set_in_file")) || bytes.Contains(data, []byte("hf_runtime_only")) {
				t.Error("credential clear/preservation failed on disk")
			}
			if ConfigPath() != path {
				t.Error("save changed selected format/path")
			}
		})
	}
}

func TestConfigSettingsFailureAndRetry(t *testing.T) {
	for _, failure := range []string{"malformed-json", "malformed-yaml", "read-directory", "unreadable", "unwritable-directory"} {
		for _, intent := range []string{"hf_pending_set", ""} {
			t.Run(failure+"/"+intent, func(t *testing.T) {
				path := isolateTokenConfig(t)
				if failure == "malformed-yaml" {
					path = filepath.Join(filepath.Dir(path), "hfdesk.yaml")
				}
				writeTokenFixture(t, path, `{"token":"hf_previous_disk"}`)
				cfg := DefaultConfig()
				cfg.Token = "hf_runtime_only"
				s := New(cfg)
				switch failure {
				case "malformed-json", "malformed-yaml":
					writeTokenFixture(t, path, "token: [\n")
				case "read-directory":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0o755); err != nil {
						t.Fatal(err)
					}
				case "unreadable":
					if runtime.GOOS == "windows" {
						t.Skip("requires Unix permission bits")
					}
					if err := os.Chmod(path, 0); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { os.Chmod(path, 0o600) })
					if _, err := os.ReadFile(path); err == nil {
						t.Skip("process bypasses file permissions")
					}
				case "unwritable-directory":
					if runtime.GOOS == "windows" {
						t.Skip("requires Unix permission bits")
					}
					if err := os.Chmod(filepath.Dir(path), 0o500); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { os.Chmod(filepath.Dir(path), 0o755) })
					probe, err := os.CreateTemp(filepath.Dir(path), "probe-*")
					if err == nil {
						probe.Close()
						os.Remove(probe.Name())
						t.Skip("process bypasses directory permissions")
					}
				}
				before, readErr := os.ReadFile(path)
				if response := requireTokenSettingsOK(t, s, `{"connections":8}`); !strings.Contains(response, "warning") {
					t.Error("ordinary save suppressed config failure")
				}
				if s.snapshotConfig().Token != cfg.Token || s.jobs.snapshotConfig().Token != cfg.Token {
					t.Error("failed ordinary save changed runtime credential")
				}
				response := requireTokenSettingsOK(t, s, `{"token":"`+intent+`","connections":9}`)
				if !strings.Contains(response, "warning") || strings.Contains(response, "hf_pending_set") || strings.Contains(response, "hf_runtime_only") {
					t.Error("persistence failure missing safe warning")
				}
				if s.snapshotConfig().Token != intent || s.jobs.snapshotConfig().Token != intent || s.jobs.snapshotConfig().Concurrency != 9 {
					t.Error("failure did not retain committed live settings")
				}
				if readErr == nil {
					after, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(before, after) {
						t.Error("failed persistence destroyed original file")
					}
				}
				switch failure {
				case "read-directory":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "unreadable":
					if err := os.Chmod(path, 0o600); err != nil {
						t.Fatal(err)
					}
					data, err := os.ReadFile(path)
					if err != nil || string(data) != `{"token":"hf_previous_disk"}` {
						t.Error("read failure changed the original file")
					}
				case "unwritable-directory":
					if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				content := `{"token":"hf_previous_disk"}`
				if failure == "malformed-yaml" {
					content = "token: hf_previous_disk\n"
				}
				writeTokenFixture(t, path, content)
				if response := requireTokenSettingsOK(t, s, `{"connections":4}`); strings.Contains(response, "warning") {
					t.Fatal(response)
				}
				requireTokenState(t, s, intent, intent)
				entries, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".hfdesk-config-*"))
				if err != nil || len(entries) != 0 {
					t.Error("config temporary files left behind")
				}
			})
		}
	}
}

func TestConfigSettingsSymlinks(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "dangling", true: "existing"}[existing], func(t *testing.T) {
			path := isolateTokenConfig(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(filepath.Dir(path), "linked-config.json")
			disk := ""
			if existing {
				writeTokenFixture(t, target, `{"token":"hf_linked_disk"}`)
				disk = "hf_linked_disk"
			}
			if err := os.Symlink(filepath.Base(target), path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			cfg := DefaultConfig()
			cfg.Token = "hf_runtime_only"
			s := New(cfg)
			if response := requireTokenSettingsOK(t, s, `{}`); strings.Contains(response, "warning") {
				t.Fatal(response)
			}
			requireTokenState(t, s, cfg.Token, disk)
			requireTokenFileMode(t, target)
			if targetName, err := os.Readlink(path); err != nil || targetName != filepath.Base(target) {
				t.Error("config symlink replaced")
			}
			requireTokenSettingsOK(t, s, `{"token":"hf_set_through_link"}`)
			requireTokenState(t, s, "hf_set_through_link", "hf_set_through_link")
		})
	}
}

// TestConfigSettingsStartupPrecedence exercises the full startup token matrix
// through the real config path: --token flag > trimmed non-empty HF_TOKEN env >
// config file token. Every case also asserts the non-token file settings still
// apply and that an ordinary save does not disturb the stored credential.
func TestConfigSettingsStartupPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flag     string
		env      string
		file     string
		wantLive string
		wantDisk string
	}{
		{"flag-over-env-over-file", "hf_flag", "hf_env", "hf_disk", "hf_flag", "hf_disk"},
		{"env-over-file", "", "hf_env", "hf_disk", "hf_env", "hf_disk"},
		{"env-only", "", "hf_env", "", "hf_env", ""},
		{"trimmed-env-wins-over-file", "", "  hf_env_trimmed  ", "hf_disk", "hf_env_trimmed", "hf_disk"},
		{"whitespace-env-falls-back-to-file", "", "   ", "hf_disk", "hf_disk", "hf_disk"},
		{"empty-env-falls-back-to-file", "", "", "hf_disk", "hf_disk", "hf_disk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateTokenConfig(t)
			// Set the env tier explicitly so the matrix is deterministic
			// regardless of any ambient credential in the test environment.
			t.Setenv("HF_TOKEN", tc.env)
			if err := SaveConfigFile(&ConfigFile{Token: tc.file, CacheDir: "file-cache", Connections: 5}); err != nil {
				t.Fatal(err)
			}
			cfg := Config{Token: tc.flag}
			if err := ApplyConfigToServer(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Token != tc.wantLive || cfg.CacheDir != "file-cache" || cfg.Concurrency != 5 {
				t.Error("valid startup precedence changed")
			}
			s := New(cfg)
			requireTokenSettingsOK(t, s, `{}`)
			requireTokenState(t, s, tc.wantLive, tc.wantDisk)
		})
	}
}

// TestConfigSettingsEnvTokenLifecycle proves that the HF_TOKEN environment
// credential is a startup-only source. An ordinary settings save must never
// copy it to disk, and an existing stored file credential must survive. An
// explicit set/clear request still authors the stored value even when startup
// came from the environment.
func TestConfigSettingsEnvTokenLifecycle(t *testing.T) {
	type step struct{ body, live, disk string }
	for _, tc := range []struct {
		name      string
		fileToken string
		steps     []step
	}{
		{
			name:      "env-only",
			fileToken: "",
			steps:     []step{{`{"connections":11}`, "hf_env", ""}},
		},
		{
			name:      "env-over-file",
			fileToken: "hf_file",
			steps: []step{
				{`{"connections":11}`, "hf_env", "hf_file"},
				{`{"token":"hf_set"}`, "hf_set", "hf_set"},
				{`{"token":""}`, "", ""},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := isolateTokenConfig(t)
			t.Setenv("HF_TOKEN", "hf_env")
			if err := SaveConfigFile(&ConfigFile{Token: tc.fileToken}); err != nil {
				t.Fatal(err)
			}
			cfg := DefaultConfig()
			if err := ApplyConfigToServer(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Token != "hf_env" {
				t.Fatalf("startup token = %q, want %q", cfg.Token, "hf_env")
			}
			s := New(cfg)
			for _, st := range tc.steps {
				requireTokenSettingsOK(t, s, st.body)
				requireTokenState(t, s, st.live, st.disk)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte("hf_env")) {
				t.Error("env credential leaked to disk")
			}
		})
	}
}

func TestConfigSettingsReservedMasks(t *testing.T) {
	path := isolateTokenConfig(t)
	// Neutralize any ambient credential so the mask-vs-credential assertions
	// are deterministic: ApplyConfigToServer gives HF_TOKEN precedence over the
	// config file, which would otherwise override the persisted mask under test.
	t.Setenv("HF_TOKEN", "")
	writeTokenFixture(t, path, `{"token":"********stale"}`)
	cfg := DefaultConfig()
	if err := ApplyConfigToServer(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "" {
		t.Error("persisted mask used as startup credential")
	}
	s := New(cfg)
	requireTokenSettingsOK(t, s, `{}`)
	requireTokenState(t, s, "", "")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("********")) {
		t.Error("old persisted mask propagated")
	}
	if err := SaveConfigFile(&ConfigFile{Token: "********stale"}); err == nil {
		t.Error("direct config save accepted reserved mask")
	}
	cfg.Token = "********stale"
	if New(cfg).snapshotConfig().Token != "" {
		t.Error("reserved runtime mask used as credential")
	}
}
