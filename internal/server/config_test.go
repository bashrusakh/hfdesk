// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isWithinTemp reports whether path resolves inside the current temp dir.
func isWithinTemp(path string) bool {
	path = filepath.Clean(path)
	temp := filepath.Clean(os.TempDir())
	rel, err := filepath.Rel(temp, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TestApplyConfigToServer_TokenPrecedence verifies that the token is resolved
// as: --token flag > HF_TOKEN environment variable > config file token.
func TestApplyConfigToServer_TokenPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		flagToken string
		envToken  string
		fileToken string
		wantToken string
	}{
		{
			name:      "flag beats env and file",
			flagToken: "flag-token",
			envToken:  "env-token",
			fileToken: "file-token",
			wantToken: "flag-token",
		},
		{
			name:      "env beats file when flag empty",
			flagToken: "",
			envToken:  "env-token",
			fileToken: "file-token",
			wantToken: "env-token",
		},
		{
			name:      "file used when flag and env empty",
			flagToken: "",
			envToken:  "",
			fileToken: "file-token",
			wantToken: "file-token",
		},
		{
			name:      "whitespace-only env does not override file",
			flagToken: "",
			envToken:  "   ",
			fileToken: "file-token",
			wantToken: "file-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Isolate from the caller's real config file on every platform.
			// XDG_CONFIG_HOME covers Unix; AppData/USERPROFILE cover Windows,
			// where os.UserConfigDir() ignores XDG_CONFIG_HOME.
			tmp := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", tmp)
			t.Setenv("AppData", tmp)
			t.Setenv("USERPROFILE", tmp)
			t.Setenv("HOME", tmp)
			t.Setenv("HF_TOKEN", tt.envToken)

			configPath := ConfigPath()
			if !isWithinTemp(configPath) {
				t.Skipf("ConfigPath() = %q is outside the temp dir; refusing to touch a real config", configPath)
			}
			_ = os.Remove(configPath)

			if tt.fileToken != "" {
				if err := SaveConfigFile(&ConfigFile{Token: tt.fileToken}); err != nil {
					t.Fatalf("SaveConfigFile: %v", err)
				}
			}

			cfg := DefaultConfig()
			cfg.Token = tt.flagToken

			if err := ApplyConfigToServer(&cfg); err != nil {
				t.Fatalf("ApplyConfigToServer: %v", err)
			}

			if cfg.Token != tt.wantToken {
				t.Errorf("Token = %q; want %q", cfg.Token, tt.wantToken)
			}
		})
	}
}
