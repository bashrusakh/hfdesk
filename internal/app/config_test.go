// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"flag"
	"fmt"
	"strings"
	"testing"
)

// TestBuildConfigAppliesDocumentedPrecedence proves the configuration
// precedence of the shared layer: the options win over the persisted config
// file, the file still wins for the values no option covers, and the stall
// timeout option overrides the file value.
func TestBuildConfigAppliesDocumentedPrecedence(t *testing.T) {
	configDir := useTemporaryConfigDir(t)
	unsetEnv(t, "HF_TOKEN")
	writeConfigFile(t, configDir, `{
		"cache-dir": "/file/cache",
		"local-dir": "/file/local",
		"token": "file-token",
		"connections": 3,
		"max-active": 7,
		"verify": "sha256",
		"retries": 9,
		"stall-timeout": "15s"
	}`)

	t.Run("the options win over the file", func(t *testing.T) {
		cfg, err := BuildConfig(Options{
			Port:         9090,
			CacheDir:     "/flag/cache",
			LocalDir:     "/flag/local",
			Token:        "flag-token",
			StallTimeout: "45s",
		})
		if err != nil {
			t.Fatalf("BuildConfig: %v", err)
		}

		if cfg.Port != 9090 {
			t.Errorf("port = %d, want 9090", cfg.Port)
		}
		if cfg.CacheDir != "/flag/cache" {
			t.Errorf("cache dir = %q, want the option value", cfg.CacheDir)
		}
		if cfg.LocalDir != "/flag/local" {
			t.Errorf("local dir = %q, want the option value", cfg.LocalDir)
		}
		if cfg.Token != "flag-token" {
			t.Errorf("token = %q, want the option value", cfg.Token)
		}
		if cfg.StallTimeout != "45s" {
			t.Errorf("stall timeout = %q, want the option value", cfg.StallTimeout)
		}

		// The file still wins for the values no option covers.
		if cfg.Concurrency != 3 {
			t.Errorf("concurrency = %d, want the file value 3", cfg.Concurrency)
		}
		if cfg.MaxActive != 7 {
			t.Errorf("max active = %d, want the file value 7", cfg.MaxActive)
		}
		if cfg.Verify != "sha256" {
			t.Errorf("verify = %q, want the file value", cfg.Verify)
		}
		if cfg.Retries != 9 {
			t.Errorf("retries = %d, want the file value 9", cfg.Retries)
		}
	})

	t.Run("the file fills in what the options leave empty", func(t *testing.T) {
		cfg, err := BuildConfig(Options{Port: 8080})
		if err != nil {
			t.Fatalf("BuildConfig: %v", err)
		}

		if cfg.CacheDir != "/file/cache" {
			t.Errorf("cache dir = %q, want the file value", cfg.CacheDir)
		}
		if cfg.LocalDir != "/file/local" {
			t.Errorf("local dir = %q, want the file value", cfg.LocalDir)
		}
		if cfg.StallTimeout != "15s" {
			t.Errorf("stall timeout = %q, want the file value", cfg.StallTimeout)
		}
		if cfg.Token != "file-token" {
			t.Errorf("token = %q, want the file value", cfg.Token)
		}
	})
}

// TestBuildConfigTokenPrecedence proves the token order: the option wins
// over the HF_TOKEN environment variable, and the environment variable wins
// over the config file.
func TestBuildConfigTokenPrecedence(t *testing.T) {
	configDir := useTemporaryConfigDir(t)
	unsetEnv(t, "HF_TOKEN")
	writeConfigFile(t, configDir, `{"token": "file-token"}`)

	t.Run("the option wins over the environment", func(t *testing.T) {
		t.Setenv("HF_TOKEN", "env-token")

		cfg, err := BuildConfig(Options{Token: "flag-token"})
		if err != nil {
			t.Fatalf("BuildConfig: %v", err)
		}
		if cfg.Token != "flag-token" {
			t.Errorf("token = %q, want the option value", cfg.Token)
		}
	})

	t.Run("the environment wins over the file", func(t *testing.T) {
		t.Setenv("HF_TOKEN", "env-token")

		cfg, err := BuildConfig(Options{})
		if err != nil {
			t.Fatalf("BuildConfig: %v", err)
		}
		if cfg.Token != "env-token" {
			t.Errorf("token = %q, want the environment value", cfg.Token)
		}
	})

	t.Run("the file wins without an option and an environment", func(t *testing.T) {
		cfg, err := BuildConfig(Options{})
		if err != nil {
			t.Fatalf("BuildConfig: %v", err)
		}
		if cfg.Token != "file-token" {
			t.Errorf("token = %q, want the file value", cfg.Token)
		}
	})
}

// TestBuildConfigRejectsInvalidStallTimeout proves that a negative or
// unparseable stall timeout is rejected instead of silently disabling the
// watchdog.
func TestBuildConfigRejectsInvalidStallTimeout(t *testing.T) {
	useTemporaryConfigDir(t)

	for _, value := range []string{"-5s", "-1", "abc", "60"} {
		cfg, err := BuildConfig(Options{StallTimeout: value})
		if err == nil {
			t.Fatalf("BuildConfig with stall timeout %q returned no error", value)
		}
		want := fmt.Sprintf("invalid --stall-timeout %q: expected a non-negative duration such as 60s or 0 to disable", value)
		if err.Error() != want {
			t.Errorf("error = %q, want %q", err.Error(), want)
		}
		if cfg.StallTimeout != "" {
			t.Errorf("stall timeout = %q, want no configuration at all", cfg.StallTimeout)
		}
	}

	// Zero is a deliberate disable and stays valid.
	cfg, err := BuildConfig(Options{StallTimeout: "0"})
	if err != nil {
		t.Fatalf("BuildConfig with stall timeout 0: %v", err)
	}
	if cfg.StallTimeout != "0" {
		t.Errorf("stall timeout = %q, want %q", cfg.StallTimeout, "0")
	}
}

// TestRegisterFlagsKeepsTheSharedOptions proves the registered flag names,
// defaults and the browser semantics of the shared options.
func TestRegisterFlagsKeepsTheSharedOptions(t *testing.T) {
	var opts Options
	fs := flag.NewFlagSet("hfdesk", flag.ContinueOnError)
	RegisterFlags(fs, &opts)

	// The host prints its own usage text, so the shared layer must not
	// replace the default one.
	var usage bytes.Buffer
	fs.SetOutput(&usage)
	fs.Usage()
	if !strings.Contains(usage.String(), "Usage of hfdesk:") {
		t.Errorf("RegisterFlags replaced the usage text: %q", usage.String())
	}

	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if opts.Port != 8080 {
		t.Errorf("port = %d, want the default 8080", opts.Port)
	}
	if !opts.OpenUI {
		t.Error("open must default to true")
	}
	if opts.NoOpen {
		t.Error("no-open must default to false")
	}
	if opts.CacheDir != "" || opts.Token != "" || opts.LocalDir != "" || opts.StallTimeout != "" {
		t.Errorf("unexpected default options: %+v", opts)
	}
	if opts.LogLevel != "" || opts.LogFile != "" {
		t.Errorf("unexpected default log options: %+v", opts)
	}
	if !opts.ShouldOpenUI() {
		t.Error("the browser must open by default")
	}

	var parsed Options
	fs = flag.NewFlagSet("hfdesk", flag.ContinueOnError)
	RegisterFlags(fs, &parsed)
	if err := fs.Parse([]string{
		"--port", "9090",
		"--cache-dir", "/cache",
		"--token", "flag-token",
		"--local-dir", "/local",
		"--stall-timeout", "30s",
		"--no-open",
		"--log-level", "debug",
		"--log-file", "/tmp/hfdesk.log",
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Port != 9090 || parsed.CacheDir != "/cache" || parsed.Token != "flag-token" ||
		parsed.LocalDir != "/local" || parsed.StallTimeout != "30s" ||
		parsed.LogLevel != "debug" || parsed.LogFile != "/tmp/hfdesk.log" {
		t.Errorf("unexpected parsed options: %+v", parsed)
	}
	if !parsed.OpenUI {
		t.Error("open must stay true")
	}
	if parsed.ShouldOpenUI() {
		t.Error("no-open must disable the browser")
	}
}
