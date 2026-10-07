// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	StallTimeout       string       `json:"stall-timeout,omitempty" yaml:"stall-timeout,omitempty"`
	Endpoint           string       `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	BackoffInitial     string       `json:"backoff-initial,omitempty" yaml:"backoff-initial,omitempty"`
	BackoffMax         string       `json:"backoff-max,omitempty" yaml:"backoff-max,omitempty"`
	Proxy              *ProxyConfig `json:"proxy,omitempty" yaml:"proxy,omitempty"`
	// DownloadRoutes maps a closed route key (see routes.go) to a destination
	// directory for routed downloads. Opt-in: empty means use LocalDir/HF
	// cache as before.
	DownloadRoutes map[string]string `json:"download-routes,omitempty" yaml:"download-routes,omitempty"`
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

// This prefix is reserved for display values, including masks from older GETs.
const tokenRedactionPrefix = "********"

func isRedactedToken(token string) bool {
	return strings.HasPrefix(token, tokenRedactionPrefix)
}

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
	configMu.Lock()
	defer configMu.Unlock()
	return loadConfigFile(ConfigPath())
}

func loadConfigFile(path string) (*ConfigFile, error) {
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

	// Old settings clients could have persisted a display mask. It must never
	// be used as a credential or propagated to a subsequent settings save.
	if isRedactedToken(cfg.Token) {
		cfg.Token = ""
	}
	return cfg, nil
}

// SaveConfigFile saves configuration to the config file.
func SaveConfigFile(cfg *ConfigFile) error {
	configMu.Lock()
	defer configMu.Unlock()

	return saveConfigFile(ConfigPath(), cfg)
}

// saveSettingsConfig preserves the selected file's credential unless an
// explicit settings set/clear intent exists. Resolve/read/write under one lock
// so a concurrent save cannot change the credential between read and write.
func saveSettingsConfig(cfg *ConfigFile, tokenWrite *string) error {
	configMu.Lock()
	defer configMu.Unlock()
	path := ConfigPath()
	stored, err := loadConfigFile(path)
	if err != nil {
		return err
	}
	c := *cfg
	c.Token = stored.Token
	if tokenWrite != nil {
		c.Token = *tokenWrite
	}
	return saveConfigFile(path, &c)
}

func saveConfigFile(path string, cfg *ConfigFile) error {
	if path == "" {
		return nil
	}
	if isRedactedToken(cfg.Token) {
		return errors.New("cannot save a redacted token as a credential")
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

	// Keep following config symlinks as before, but replace the target only
	// after a complete write. A read/chmod/write failure must not truncate the
	// previous config. Tighten old files too: a create mode alone cannot do so.
	path, err = configWriteTarget(path)
	if err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("config target is not a regular file")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hfdesk-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Resolve final-component symlinks (including dangling links) without replacing
// the link itself. Directory symlinks continue to be followed by filesystem IO.
func configWriteTarget(path string) (string, error) {
	for range 255 {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return path, nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	return "", errors.New("too many config symlinks")
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
	// Token precedence: --token flag > HF_TOKEN env > config file.
	if serverCfg.Token == "" {
		if envToken := strings.TrimSpace(os.Getenv("HF_TOKEN")); envToken != "" {
			serverCfg.Token = envToken
		} else if fileCfg.Token != "" {
			serverCfg.Token = fileCfg.Token
		}
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
	// StallTimeout: the config file applies whenever it carries a value; the
	// CLI --stall-timeout flag is applied on top afterwards (see cmd/hfdesk),
	// so an explicit flag still wins. Unlike MultipartThreshold there is no
	// non-empty DefaultConfig sentinel to compare against safely.
	//
	// A NEGATIVE or unparseable value must never silently disable the watchdog,
	// so reject it here and keep the protective default rather than applying
	// garbage. "0"/"0s" is a deliberate disable and is applied as-is.
	if fileCfg.StallTimeout != "" {
		if d, err := time.ParseDuration(fileCfg.StallTimeout); err == nil && d >= 0 {
			serverCfg.StallTimeout = fileCfg.StallTimeout
		}
	}
	if serverCfg.Endpoint == "" && fileCfg.Endpoint != "" {
		serverCfg.Endpoint = fileCfg.Endpoint
	}
	if len(serverCfg.DownloadRoutes) == 0 && len(fileCfg.DownloadRoutes) > 0 {
		// Sanitize at the config boundary: drop keys outside the closed
		// route-key set (e.g. a hand-edited legacy key) and normalize values,
		// so cfg.DownloadRoutes never holds an invalid key and GET /api/settings
		// never advertises one. This keeps load and save agreeing on the key
		// set, so echoing the loaded map back cannot fail validation.
		serverCfg.DownloadRoutes = sanitizeDownloadRoutes(fileCfg.DownloadRoutes)
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
