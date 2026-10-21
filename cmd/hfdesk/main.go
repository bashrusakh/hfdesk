// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bashrusakh/hfdesk/internal/app"
	"github.com/bashrusakh/hfdesk/internal/logging"
)

// Version is set at build time via ldflags (-X main.Version=...) from the
// VERSION file. Defaults to "dev" for un-flagged local builds.
var Version = "dev"

func main() {
	if err := run(); err != nil {
		reportFailure(err)
		os.Exit(1)
	}
}

// run performs the CLI startup: it parses the flags, installs the logging
// pipeline and hands the shared application layer over to the host.
//
// The configuration assembly, the version propagation, the server startup and
// the browser handling live in the shared application layer, so the desktop
// host runs the very same code in-process.
func run() error {
	var opts app.Options

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "HFDesk %s\n\nUsage: hfdesk [options]\n\nOptions:\n", Version)
		flag.PrintDefaults()
	}
	app.RegisterFlags(flag.CommandLine, &opts)
	flag.Parse()

	// The console sink writes to standard error, so redirecting stdout
	// keeps working for scripts. The file sink is only enabled when a log
	// file was requested, so the default run writes no files.
	pipeline, err := app.SetupLogging(app.LogOptions{
		Level:    opts.LogLevel,
		Console:  true,
		FilePath: opts.LogFile,
	})
	if err != nil {
		return err
	}
	defer pipeline.Close()

	application, err := app.New(opts, Version)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := application.Start(ctx); err != nil {
		return err
	}

	if opts.ShouldOpenUI() {
		if err := application.OpenBrowser(); err != nil {
			logging.Component("app").Warn("could not open the web UI", "error", err)
		}
	}

	return application.Wait()
}

// reportFailure writes a failure to standard error.
//
// The logging pipeline writes to standard error as well, so the message
// reaches the terminal in either case.
func reportFailure(err error) {
	if logging.Default() != nil {
		logging.Component("app").Error("hfdesk failed", "error", err)
		return
	}
	fmt.Fprintf(os.Stderr, "hfdesk: %v\n", err)
}
