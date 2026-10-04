// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"testing"
)

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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Isolate from the caller's real config file and any config in the
			// launch directory, then point the app config dir at a temp dir.
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HF_TOKEN", tt.envToken)
			_ = os.Remove(ConfigPath())

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
