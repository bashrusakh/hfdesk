// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/bashrusakh/hfdesk/internal/logsink"
	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/server"
)

// RunOptions configures the headless service host.
type RunOptions struct {
	// Paths is the runtime path model of the instance. It is installed
	// process-wide before any server state is read or written.
	Paths runtimepaths.Paths
	// Listen is the host:port the instance binds. Empty means DefaultListen.
	Listen string
	// CacheDir is the absolute Hugging Face cache root of the registration.
	// It is the fallback: the cache-dir of the config file of the instance
	// wins over it. Empty keeps the default resolution.
	CacheDir string
	// HubDir is the absolute Hugging Face hub cache directory of the
	// registration. It is the fallback: a config file of the instance that
	// defines a cache root owns the hub directory as well. Empty keeps the
	// default resolution.
	HubDir string
	// LocalDir is the absolute flat output directory of the registration.
	// It is the fallback: the local-dir of the config file of the instance
	// wins over it. Empty means the Hugging Face cache layout is used.
	LocalDir string
}

// Run starts HFDesk in service mode and blocks until ctx is cancelled or the
// server fails. It never opens a browser and never creates a tray icon: the
// instance is supervised, there is no desktop session to interact with.
func Run(ctx context.Context, opts RunOptions) error {
	// The runtime path model is installed first: the config file, the job
	// state, the download history and the mirror targets are all resolved
	// from it, so no server state may be read or written before it is in
	// place.
	runtimepaths.Install(opts.Paths)

	// The process log goes to the supervisor (systemd journal, launchd log
	// files) and into the bounded rotating log file of the instance.
	sink, err := logsink.Open(opts.Paths.LogDir)
	if err != nil {
		return fmt.Errorf("service: open log sink: %w", err)
	}
	defer sink.Close()
	previousLogOutput := log.Writer()
	log.SetOutput(io.MultiWriter(os.Stdout, sink))
	defer log.SetOutput(previousLogOutput)

	// Resolve the bind address of the instance.
	listen := opts.Listen
	if listen == "" {
		listen = DefaultListen
	}
	host, port, err := ParseListen(listen)
	if err != nil {
		return err
	}

	// The configuration of the instance resolves its destinations and pins
	// the cache environment, so it has to run before the server is created:
	// the server captures the cache identity of the process when it is
	// built.
	cfg, err := buildConfig(opts, host, port)
	if err != nil {
		return err
	}

	// The server owns the graceful shutdown path (quiesce, HTTP drain, job
	// manager close); the host only waits for it.
	return server.New(cfg).ListenAndServe(ctx)
}

// destinations are the storage locations of a service instance.
type destinations struct {
	// CacheDir is the Hugging Face cache root of the instance.
	CacheDir string
	// HubDir is the Hugging Face hub cache directory of the instance.
	HubDir string
	// LocalDir is the flat output directory of the instance.
	LocalDir string
}

// buildConfig assembles the server configuration of a service instance.
//
// The destinations of an instance follow the precedence of a service
// instance: the config file of the instance wins over the registration.
// The user changes the cache directory and the output directory in the
// settings of the instance, and the next start of the instance has to use
// the changed value. The registration is the fallback for a destination the
// config file does not define.
//
// The runtime path model of the instance is installed here, so the settings
// of the instance come from its own config file and never from an ambient
// global the caller did not set. The server applies the settings of the
// instance through the process wide path model, and the model of the
// instance is the only one that may be in place.
//
// The cache environment is pinned from the effective values after the
// configuration was loaded: the server reads HF_HOME and HF_HUB_CACHE when
// it captures the cache identity of the process, and neither variable is
// read by the config file.
func buildConfig(opts RunOptions, host string, port int) (server.Config, error) {
	if opts.Paths.StateDir == "" || !filepath.IsAbs(opts.Paths.StateDir) {
		return server.Config{}, fmt.Errorf("service: state dir %q must be an absolute path", opts.Paths.StateDir)
	}

	// The path model of the instance is installed before any server state
	// is read: the config file, the job state, the download history and
	// the mirror targets all resolve from it.
	runtimepaths.Install(opts.Paths)

	// server.ApplyConfigToServer prefers HF_TOKEN over the config file, so
	// the environment must not keep a credential: it is removed before the
	// configuration is loaded and the service-owned config file is the only
	// source of the token.
	if err := os.Unsetenv("HF_TOKEN"); err != nil {
		return server.Config{}, fmt.Errorf("service: drop environment token: %w", err)
	}

	dest, err := effectiveDestinations(opts)
	if err != nil {
		return server.Config{}, err
	}

	cfg := server.DefaultConfig()
	// The server composes the bind address as "<Addr>:<Port>", so an IPv6
	// host keeps its brackets.
	cfg.Addr = listenHost(host)
	cfg.Port = port
	cfg.CacheDir = dest.CacheDir
	cfg.LocalDir = dest.LocalDir
	// The interactive defaults are relative ("./Models"), which would resolve
	// against the working directory the supervisor happened to choose. A
	// service instance keeps its output inside its own state root.
	cfg.ModelsDir = filepath.Join(opts.Paths.StateDir, "models")
	cfg.DatasetsDir = filepath.Join(opts.Paths.StateDir, "datasets")

	// The service-owned config file supplies the token, the proxy and the
	// remaining settings of the instance.
	if err := server.ApplyConfigToServer(&cfg); err != nil {
		return server.Config{}, fmt.Errorf("service: load config %q: %w", opts.Paths.ConfigFile, err)
	}

	// The cache environment is pinned from the effective values: the
	// configuration of the instance is loaded, so these are the values the
	// instance really uses.
	if err := pinCacheEnvironment(dest); err != nil {
		return server.Config{}, err
	}
	return cfg, nil
}

// effectiveDestinations resolves the storage locations of a service instance.
//
// The config file of the instance wins over the registration: the user
// changes the cache directory and the output directory in the settings of
// the instance, and the instance has to use the changed value after the
// next start. The registration is the fallback for a destination the config
// file does not define.
//
// The hub directory follows the cache root. A config file that defines the
// cache root owns the whole cache layout of the instance, so the hub
// directory is the "hub" child of that root. Without a cache root from the
// config file the registration keeps its hub directory, and a registration
// without one falls back to the "hub" child of the effective cache root.
func effectiveDestinations(opts RunOptions) (destinations, error) {
	fileCfg, err := server.LoadConfigFileAt(opts.Paths.ConfigFile)
	if err != nil {
		return destinations{}, fmt.Errorf("service: load config %q: %w", opts.Paths.ConfigFile, err)
	}

	dest := destinations{
		CacheDir: opts.CacheDir,
		LocalDir: opts.LocalDir,
	}
	if fileCfg.CacheDir != "" {
		dest.CacheDir = fileCfg.CacheDir
	}
	if fileCfg.LocalDir != "" {
		dest.LocalDir = fileCfg.LocalDir
	}

	switch {
	case fileCfg.CacheDir != "":
		dest.HubDir = filepath.Join(dest.CacheDir, "hub")
	case opts.HubDir != "":
		dest.HubDir = opts.HubDir
	case dest.CacheDir != "":
		dest.HubDir = filepath.Join(dest.CacheDir, "hub")
	}
	// An instance without a cache root and without a registered hub
	// directory keeps the default resolution: there is no root to derive a
	// hub directory from.
	return dest, nil
}

// pinCacheEnvironment pins the cache identity of the process.
//
// A supervisor hands over whatever environment it happens to have (a login
// shell, a system-wide environment file, a stale export), so the effective
// cache identity would otherwise depend on the machine instead of on the
// installation. The values of the installation win; a destination that is
// not set keeps the environment/default resolution.
//
// Only the two cache variables are pinned. The token of the environment is
// dropped instead: the config file of the instance is the only source of
// its credential.
func pinCacheEnvironment(dest destinations) error {
	if dest.CacheDir != "" {
		if err := os.Setenv("HF_HOME", dest.CacheDir); err != nil {
			return fmt.Errorf("service: pin cache root: %w", err)
		}
	}
	if dest.HubDir != "" {
		if err := os.Setenv("HF_HUB_CACHE", dest.HubDir); err != nil {
			return fmt.Errorf("service: pin hub cache: %w", err)
		}
	}
	return nil
}

// listenHost returns the host in the form the server composes with the port
// into a socket address.
func listenHost(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}
