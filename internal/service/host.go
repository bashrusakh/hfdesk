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
	// CacheDir is the absolute Hugging Face cache root. Empty keeps the
	// default resolution.
	CacheDir string
	// HubDir is the absolute Hugging Face hub cache directory. Empty keeps
	// the default resolution.
	HubDir string
	// LocalDir is the absolute flat output directory. Empty means the
	// Hugging Face cache layout is used.
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

	// Pin the cache environment. A supervisor hands over whatever
	// environment it happens to have (a login shell, a system-wide
	// environment file, a stale export), so the effective cache identity
	// would otherwise depend on the machine instead of on the installation.
	// The values of the installation win; an option that is not set keeps the
	// environment/default resolution.
	if opts.CacheDir != "" {
		if err := os.Setenv("HF_HOME", opts.CacheDir); err != nil {
			return fmt.Errorf("service: pin cache root: %w", err)
		}
	}
	if opts.HubDir != "" {
		if err := os.Setenv("HF_HUB_CACHE", opts.HubDir); err != nil {
			return fmt.Errorf("service: pin hub cache: %w", err)
		}
	}

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

	// The credentials of an instance come from its own config file, never
	// from the environment the supervisor handed over.
	cfg, err := buildConfig(opts, host, port)
	if err != nil {
		return err
	}

	// The server owns the graceful shutdown path (quiesce, HTTP drain, job
	// manager close); the host only waits for it.
	return server.New(cfg).ListenAndServe(ctx)
}

// buildConfig assembles the server configuration of a service instance.
func buildConfig(opts RunOptions, host string, port int) (server.Config, error) {
	if opts.Paths.StateDir == "" || !filepath.IsAbs(opts.Paths.StateDir) {
		return server.Config{}, fmt.Errorf("service: state dir %q must be an absolute path", opts.Paths.StateDir)
	}

	// server.ApplyConfigToServer prefers HF_TOKEN over the config file, so
	// the environment must not keep a credential: it is removed before the
	// configuration is loaded and the service-owned config file is the only
	// source of the token.
	if err := os.Unsetenv("HF_TOKEN"); err != nil {
		return server.Config{}, fmt.Errorf("service: drop environment token: %w", err)
	}

	cfg := server.DefaultConfig()
	// The server composes the bind address as "<Addr>:<Port>", so an IPv6
	// host keeps its brackets.
	cfg.Addr = listenHost(host)
	cfg.Port = port
	cfg.CacheDir = opts.CacheDir
	cfg.LocalDir = opts.LocalDir
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
	return cfg, nil
}

// listenHost returns the host in the form the server composes with the port
// into a socket address.
func listenHost(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}
