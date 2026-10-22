// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

// Package app provides the shared application layer of HFDesk.
//
// It owns the startup sequence every host performs: the configuration
// assembly with its documented precedence, the version propagation, the
// server construction, the browser URL derivation and the graceful
// lifecycle. The CLI host in cmd/hfdesk and the Windows desktop host both
// build on it, so neither of them repeats the startup rules.
//
// A host only adapts host specific behavior: how its flags are parsed, which
// logging sinks are installed, how failures are reported and which window or
// tray the user interacts with.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/bashrusakh/hfdesk/internal/server"
)

// boundAddrTimeout bounds the wait for the server to report its bound
// address. The server records the address as the first thing it does when
// it starts to serve, so the wait is short.
const boundAddrTimeout = 10 * time.Second

// listenCleanupTimeout bounds the wait for the job manager cleanup that
// follows a failed bind. The server closes the job manager with this budget,
// so the cleanup is never cut short and a stuck job manager cannot keep the
// startup from reporting the failure.
const listenCleanupTimeout = 15 * time.Second

// Options holds the host-agnostic startup options shared by the CLI host and
// the future Windows desktop host.
type Options struct {
	Port         int
	CacheDir     string
	Token        string
	LocalDir     string
	StallTimeout string
	OpenUI       bool
	NoOpen       bool
	LogLevel     string
	LogFile      string
}

// RegisterFlags registers the shared options on fs.
//
// It does not set fs.Usage: each host prints its own usage text. It does not
// parse either; the host decides when the flags are parsed.
func RegisterFlags(fs *flag.FlagSet, o *Options) {
	fs.IntVar(&o.Port, "port", 8080, "HTTP port")
	fs.StringVar(&o.CacheDir, "cache-dir", "", "Hugging Face cache directory")
	fs.StringVar(&o.Token, "token", "", "Hugging Face token (also reads HF_TOKEN env)")
	fs.StringVar(&o.LocalDir, "local-dir", "", "write real files into this directory instead of the HF cache layout")
	fs.StringVar(&o.StallTimeout, "stall-timeout", "", "abort a stalled download read after this duration (e.g. 60s; 0 disables)")
	fs.BoolVar(&o.OpenUI, "open", true, "open the web UI in the default browser")
	fs.BoolVar(&o.NoOpen, "no-open", false, "do not open the web UI automatically")
	fs.StringVar(&o.LogLevel, "log-level", "", "minimum log level: debug, info, warn or error (default info)")
	fs.StringVar(&o.LogFile, "log-file", "", "write the log records to this rotating file")
}

// ShouldOpenUI reports whether the host should open the browser UI.
func (o Options) ShouldOpenUI() bool {
	return o.OpenUI && !o.NoOpen
}

// BuildConfig assembles the effective server configuration from the options
// and the persisted config file, applying the documented precedence and
// validation. It performs no I/O beyond reading the config file and mutates
// no global state.
func BuildConfig(o Options) (server.Config, error) {
	cfg := server.DefaultConfig()
	cfg.Port = o.Port
	cfg.CacheDir = o.CacheDir
	cfg.Token = o.Token
	cfg.LocalDir = o.LocalDir

	// The persisted config file fills in the values the options did not
	// set. It owns the precedence rules, including the token order (flag,
	// HF_TOKEN environment variable, config file), so they are applied
	// there and not duplicated here.
	if err := server.ApplyConfigToServer(&cfg); err != nil {
		return server.Config{}, fmt.Errorf("load config: %w", err)
	}

	// The stall timeout option takes precedence over the persisted setting.
	// A negative or unparseable value is rejected: it must never silently
	// disable the stall watchdog or be accepted as a bogus duration.
	if o.StallTimeout != "" {
		d, err := time.ParseDuration(o.StallTimeout)
		if err != nil || d < 0 {
			return server.Config{}, fmt.Errorf("invalid --stall-timeout %q: expected a non-negative duration such as 60s or 0 to disable", o.StallTimeout)
		}
		cfg.StallTimeout = o.StallTimeout
	}

	return cfg, nil
}

// App is the running application: one configuration, one server, one
// lifecycle, shared by every host.
type App struct {
	cfg    server.Config
	server *server.Server

	mu       sync.Mutex
	started  bool
	done     chan struct{}
	serveErr error
	startErr error
}

// New assembles the configuration, propagates the version to the server
// package and constructs the server. It does not bind or serve yet, so a
// configuration problem is reported before anything is bound.
func New(o Options, version string) (*App, error) {
	cfg, err := BuildConfig(o)
	if err != nil {
		return nil, err
	}
	// Surface the build version to the API/UI so the displayed version
	// tracks the binary instead of a hardcoded string.
	server.Version = version
	return &App{cfg: cfg, server: server.New(cfg)}, nil
}

// Start binds the listener and starts serving in the background. It returns
// an error when the address cannot be bound. After it returns nil, URL and
// Config are available.
func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return errors.New("app: the application was already started")
	}
	a.started = true
	a.mu.Unlock()

	cfg := a.cfg
	addr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		// The bind failed: release the resources the server allocated.
		// The server owns the cleanup of its job manager, so the
		// application asks it to close. The cleanup is bounded, so a stuck
		// job manager cannot keep the startup from reporting the failure.
		// The cleanup is not bound by the cancellation of the caller: the
		// job manager has to be closed even when the host is already
		// shutting down.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), listenCleanupTimeout)
		closeErr := a.server.Close(cleanupCtx)
		cancel()

		startErr := errors.Join(fmt.Errorf("listen on %s: %w", addr, err), closeErr)
		a.mu.Lock()
		a.startErr = startErr
		a.mu.Unlock()
		return startErr
	}

	done := make(chan struct{})
	a.mu.Lock()
	a.done = done
	a.mu.Unlock()

	go func() {
		serveErr := a.server.Serve(ctx, listener)
		a.mu.Lock()
		a.serveErr = serveErr
		a.mu.Unlock()
		close(done)
	}()

	// The server records the bound address when it starts to serve. Wait
	// for it, so URL reports the effective address, including an
	// OS-assigned port, as soon as Start returns.
	return a.waitForBoundAddr(ctx, done)
}

// waitForBoundAddr waits until the server reports the address it is bound
// to, so the effective URL is known when Start returns.
func (a *App) waitForBoundAddr(ctx context.Context, done <-chan struct{}) error {
	deadline := time.Now().Add(boundAddrTimeout)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		if a.server.BoundAddr() != nil {
			return nil
		}
		select {
		case <-done:
			if err := a.serveError(); err != nil {
				return err
			}
			return errors.New("app: the server stopped before it reported its bound address")
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			return errors.New("app: the server did not report its bound address")
		}
	}
}

// URL returns the effective, browser-safe URL of the running server.
//
// It is derived from the address the server is really bound to, so a
// non-default port and an OS-assigned port are both reported correctly.
func (a *App) URL() string {
	return a.server.BrowserURL()
}

// Config returns the effective server configuration.
//
// It is the configuration the application was started with. Later settings
// changes are applied to the server's own copy of the configuration.
func (a *App) Config() server.Config {
	return a.cfg
}

// Wait blocks until the server stops. When ctx is cancelled the server
// performs its graceful shutdown (quiesce jobs, drain HTTP, close the job
// manager) and Wait returns the server's error.
//
// A server that stopped without an error, for example after a graceful
// shutdown, returns nil.
func (a *App) Wait() error {
	a.mu.Lock()
	done := a.done
	startErr := a.startErr
	a.mu.Unlock()

	if done == nil {
		// The application never started serving: there is nothing to wait
		// for, so the startup failure is the result.
		return startErr
	}

	<-done
	return a.serveError()
}

// serveError returns the error the serving goroutine ended with.
func (a *App) serveError() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.serveErr
}
