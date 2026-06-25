// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
	"gopkg.in/yaml.v3"
)

// RunDir returns the directory where state files (config, jobs, history)
// should be stored. Priority: current working directory → executable dir → ".".
func RunDir() string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

// JobsStatePath returns the path to jobs_state.json in the app config directory.
func JobsStatePath() string {
	return filepath.Join(AppConfigDir(), "jobs_state.json")
}

// HistoryPath returns the path to download_history.json in the app config directory.
func HistoryPath() string {
	return filepath.Join(AppConfigDir(), "download_history.json")
}

// ConfigFile represents the persistent configuration file format.
// This matches the CLI config file format for consistency.
type ConfigFile struct {
	CacheDir           string       `json:"cache-dir,omitempty" yaml:"cache-dir,omitempty"`
	LocalDir           string       `json:"local-dir,omitempty" yaml:"local-dir,omitempty"`
	LocalScanDirs      []string     `json:"local-scan-dirs,omitempty" yaml:"local-scan-dirs,omitempty"`
	Token              string       `json:"token,omitempty" yaml:"token,omitempty"`
	Connections        int          `json:"connections,omitempty" yaml:"connections,omitempty"`
	MaxActive          int          `json:"max-active,omitempty" yaml:"max-active,omitempty"`
	MultipartThreshold string       `json:"multipart-threshold,omitempty" yaml:"multipart-threshold,omitempty"`
	MaxSpeed           string       `json:"max-speed,omitempty" yaml:"max-speed,omitempty"`
	Verify             string       `json:"verify,omitempty" yaml:"verify,omitempty"`
	Retries            *int         `json:"retries,omitempty" yaml:"retries,omitempty"`
	Endpoint           string       `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	BackoffInitial     string       `json:"backoff-initial,omitempty" yaml:"backoff-initial,omitempty"`
	BackoffMax         string       `json:"backoff-max,omitempty" yaml:"backoff-max,omitempty"`
	Proxy              *ProxyConfig `json:"proxy,omitempty" yaml:"proxy,omitempty"`
}

// ProxyConfig holds proxy settings for the config file.
type ProxyConfig struct {
	URL                string `json:"url,omitempty" yaml:"url,omitempty"`
	Username           string `json:"username,omitempty" yaml:"username,omitempty"`
	Password           string `json:"password,omitempty" yaml:"password,omitempty"`
	NoProxy            string `json:"no_proxy,omitempty" yaml:"no_proxy,omitempty"`
	NoEnvProxy         bool   `json:"no_env_proxy,omitempty" yaml:"no_env_proxy,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty" yaml:"insecure_skip_verify,omitempty"`
}

var configMu sync.Mutex

// AppConfigDir returns the normal per-user HFDesk configuration directory.
func AppConfigDir() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "HFDesk")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "hfdesk")
	}
	return RunDir()
}

func isTempDir(path string) bool {
	path = filepath.Clean(path)
	temp := filepath.Clean(os.TempDir())
	lower := strings.ToLower(path)
	if strings.HasPrefix(strings.ToLower(path), strings.ToLower(temp)) {
		return true
	}
	return strings.Contains(lower, "rar$") || strings.Contains(lower, ".rartemp")
}

// ConfigPath returns the path to the config file.
// Search order: existing config next to the current/executable folder, then the
// normal per-user config directory. New saves go to the per-user directory so
// running an unpacked archive does not create config in a temp extraction path.
func ConfigPath() string {
	dirs := []string{RunDir()}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}

	for _, dir := range dirs {
		if isTempDir(dir) {
			continue
		}
		for _, name := range []string{"hfdesk.json", "hfdesk.yaml", "hfdesk.yml"} {
			p := filepath.Join(dir, name)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}

	configDir := AppConfigDir()
	for _, name := range []string{"hfdesk.json", "hfdesk.yaml", "hfdesk.yml"} {
		p := filepath.Join(configDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	return filepath.Join(configDir, "hfdesk.json")
}

// LoadConfigFile loads configuration from the config file.
// Returns empty config if file doesn't exist (not an error).
func LoadConfigFile() (*ConfigFile, error) {
	path := ConfigPath()
	if path == "" {
		return &ConfigFile{}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ConfigFile{}, nil
		}
		return nil, err
	}

	cfg := &ConfigFile{}
	ext := strings.ToLower(filepath.Ext(path))

	switch ext {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	default:
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	}

	return cfg, nil
}

// SaveConfigFile saves configuration to the config file.
func SaveConfigFile(cfg *ConfigFile) error {
	configMu.Lock()
	defer configMu.Unlock()

	path := ConfigPath()
	if path == "" {
		return nil
	}

	// Ensure config directory exists
	configDir := filepath.Dir(path)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}

	ext := strings.ToLower(filepath.Ext(path))
	var data []byte
	var err error

	switch ext {
	case ".yaml", ".yml":
		data, err = yaml.Marshal(cfg)
	default:
		data, err = json.MarshalIndent(cfg, "", "  ")
	}

	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o644)
}

// ApplyConfigToServer applies config file settings to server config.
// CLI flags take precedence (non-zero values are not overwritten).
func ApplyConfigToServer(serverCfg *Config) error {
	fileCfg, err := LoadConfigFile()
	if err != nil {
		return err
	}

	// Only apply values that are not already set via CLI
	if serverCfg.CacheDir == "" && fileCfg.CacheDir != "" {
		serverCfg.CacheDir = fileCfg.CacheDir
	}
	if serverCfg.LocalDir == "" && fileCfg.LocalDir != "" {
		serverCfg.LocalDir = fileCfg.LocalDir
	}
	if len(serverCfg.LocalScanDirs) == 0 && len(fileCfg.LocalScanDirs) > 0 {
		serverCfg.LocalScanDirs = fileCfg.LocalScanDirs
	}
	if serverCfg.Token == "" && fileCfg.Token != "" {
		serverCfg.Token = fileCfg.Token
	}
	if fileCfg.Connections > 0 {
		serverCfg.Concurrency = fileCfg.Connections
	}
	if fileCfg.MaxActive > 0 {
		serverCfg.MaxActive = fileCfg.MaxActive
	}
	if serverCfg.MultipartThreshold == "" && fileCfg.MultipartThreshold != "" {
		serverCfg.MultipartThreshold = fileCfg.MultipartThreshold
	}
	if serverCfg.MaxSpeed == "" && fileCfg.MaxSpeed != "" {
		serverCfg.MaxSpeed = fileCfg.MaxSpeed
	}
	if fileCfg.Verify != "" {
		serverCfg.Verify = fileCfg.Verify
	}
	if fileCfg.Retries != nil && *fileCfg.Retries >= 0 {
		serverCfg.Retries = *fileCfg.Retries
	}
	if serverCfg.Endpoint == "" && fileCfg.Endpoint != "" {
		serverCfg.Endpoint = fileCfg.Endpoint
	}

	// Apply proxy settings if not already set
	if serverCfg.Proxy == nil && fileCfg.Proxy != nil {
		serverCfg.Proxy = &hfdownloader.ProxyConfig{
			URL:                fileCfg.Proxy.URL,
			Username:           fileCfg.Proxy.Username,
			Password:           fileCfg.Proxy.Password,
			NoProxy:            fileCfg.Proxy.NoProxy,
			NoEnvProxy:         fileCfg.Proxy.NoEnvProxy,
			InsecureSkipVerify: fileCfg.Proxy.InsecureSkipVerify,
		}
	}

	return nil
}
