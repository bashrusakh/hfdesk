// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bashrusakh/hfdesk/internal/app"
	"github.com/bashrusakh/hfdesk/internal/logging"
	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/server"
)

// logFileName is the name of the log file of an instance inside its log
// directory.
const logFileName = "hfdesk.log"

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

	// The shared logging pipeline is installed before the server starts:
	// the server logs through it, so the pipeline is what puts the records
	// of the server into the log file of the instance. The pipeline is
	// closed when the host returns, so the records of the shutdown are
	// written before the file sink goes away.
	pipeline, err := installLogging(opts.Paths.LogDir)
	if err != nil {
		return err
	}
	defer func() { _ = pipeline.Close() }()

	// The failure of the persistent log is reported to the supervisor of the
	// instance: the file sink can fail to rotate the log file, and a log
	// file that stopped working is a problem the operator has to know
	// about. The report happens on the standard error of the process, and
	// it never changes the exit code of the host.
	logFailure := app.NewLogFailureReporter(pipeline, func(text string) {
		fmt.Fprintln(os.Stderr, text)
	})
	logFailure.Subscribe()
	defer logFailure.Report()

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

// installLogging installs the shared logging pipeline of a service instance.
//
// The console sink is on: the supervisor of the instance captures the
// standard error of the process (the systemd journal, the launchd log), so
// the records of the instance reach the supervisor. The file sink writes
// into the log file of the instance, so the records of the server land in
// the log file of the instance as well.
//
// The service mode has no log level option, so the pipeline keeps its
// default level.
//
// A log file that cannot be written never stops the instance: the pipeline
// falls back to the console sink alone and the failure is reported in a
// readable form. The failure of the log file never changes the exit code of
// the host.
func installLogging(logDir string) (*logging.Pipeline, error) {
	if logDir == "" {
		return nil, errors.New("service: the log directory of the instance is required")
	}
	logPath := filepath.Join(logDir, logFileName)

	// The log directory is created owner-only before the pipeline is
	// installed: the shared file sink creates the directory of the log file
	// with a wider mode, and the log directory of an instance holds the log
	// of the instance. A directory that already exists keeps its mode, so
	// the instance never widens the permissions of a directory it did not
	// create.
	if err := ensureDir(logDir, dirMode); err != nil {
		return installConsoleLogging(
			fmt.Errorf("service: create the log directory %q: %w", logDir, err), "")
	}

	pipeline, err := app.SetupLogging(app.LogOptions{
		Console:  true,
		FilePath: logPath,
	})
	if err != nil {
		return installConsoleLogging(err, logPath)
	}
	return pipeline, nil
}

// installConsoleLogging installs a console-only pipeline and reports the
// failure of the log file.
//
// The failure of the log file is not the failure of the instance: the
// instance keeps running, and the supervisor of the instance still receives
// every record through the console sink.
func installConsoleLogging(failure error, logPath string) (*logging.Pipeline, error) {
	// The report goes to the standard error of the process, where the
	// supervisor of the instance reads it. There is no pipeline yet, so the
	// report cannot go through the console sink.
	fmt.Fprintln(os.Stderr, app.LogFailureText(failure, logPath))

	pipeline, err := app.SetupLogging(app.LogOptions{Console: true})
	if err != nil {
		return nil, err
	}
	return pipeline, nil
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
