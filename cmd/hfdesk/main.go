// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/bashrusakh/hfdesk/internal/app"
	"github.com/bashrusakh/hfdesk/internal/logging"
	"github.com/bashrusakh/hfdesk/internal/server"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// Version is set at build time via ldflags (-X main.Version=...) from the
// VERSION file. Defaults to "dev" for un-flagged local builds.
var Version = "dev"

func main() {
	// The service command family is a command of its own. It is dispatched
	// before the interactive startup, so "hfdesk" without a "service" token
	// keeps its exact behaviour: same flags, same defaults, same browser
	// launch.
	if isServiceCommand(os.Args[1:]) {
		os.Exit(runServiceCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	// The rebuild-only flags are a separate command of the binary, so they
	// are handled before the normal startup: they must not start the server
	// and they must not open the web UI.
	if handled, code := runRebuildCLI(os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}

	if err := run(); err != nil {
		os.Exit(1)
	}
}

// run performs the CLI startup: it parses the flags, installs the logging
// pipeline and hands the shared application layer over to the host.
//
// The configuration assembly, the version propagation, the server startup and
// the browser handling live in the shared application layer, so the desktop
// host runs the very same code in-process.
//
// A failure is reported before it is returned, while the logging pipeline is
// still open: the file sink drops the records that are emitted after it was
// closed, so a failure that is reported after the close would be missing
// from the log file the user is asked to attach to a bug report.
func run() (err error) {
	var opts app.Options

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "HFDesk %s\n\nUsage: hfdesk [options]\n       hfdesk --rebuild [--cache-dir R] [--hub-dir H] [--clean] [--verbose]\n\nOptions:\n", Version)
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
		// The pipeline could not be installed: there is no log file to
		// write to and no console sink, so the failure goes to standard
		// error in a readable form.
		reportFailure(err)
		return err
	}

	// The failure of the persistent log is reported to the user: the file
	// sink can fail to rotate the log file, and a log file that stopped
	// working is a problem the user has to know about.
	//
	// The console host has a console, so the failure goes to standard
	// error. The subscription reports it while the application runs, and
	// the report at the shutdown covers the failure that was recorded
	// before the subscription started. The failure is reported once.
	logFailure := app.NewLogFailureReporter(pipeline, func(text string) {
		fmt.Fprintln(os.Stderr, text)
	})
	logFailure.Subscribe()
	defer func() {
		// The failure is recorded through the pipeline before the pipeline
		// is closed: a closed file sink drops the records that are emitted
		// afterwards, so the log file would not contain the failure.
		if err != nil {
			reportFailure(err)
		}
		// The failure of the log file is reported before the pipeline is
		// closed, so the error of the pipeline is still available.
		//
		// The failure of the log file never changes the exit code of the
		// host: a log file that could not be rotated is not a reason to
		// fail a download that otherwise succeeded.
		logFailure.Report()
		_ = pipeline.Close()
	}()

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
			// The failure of the browser is reported the same way for
			// every caller: the same message, the URL that could not be
			// opened and the reason of the failure.
			logging.Component("app").Warn("could not open the web UI",
				"url", application.URL(), "error", err)
		}
	}

	return application.Wait()
}

// reportFailure reports a failure of the startup.
//
// The failure is recorded through the logging pipeline, so it reaches the
// console sink and the file sink. The pipeline is still open when the
// failure is reported, so the record reaches the log file.
//
// When the pipeline could not be installed there is no pipeline to record
// the failure in, so the message goes to standard error in a readable form.
func reportFailure(err error) {
	if logging.Default() != nil {
		logging.Component("app").Error("hfdesk failed", "error", err)
		return
	}
	fmt.Fprintf(os.Stderr, "hfdesk: %v\n", err)
}

const rebuildProtocolVersion = "HFDesk rebuild protocol 1"

// runRebuildCLI handles rebuild-only flags before the normal server path. The
// hidden probe lets the generated wrapper reject old binaries whose flag
// parser would otherwise fail (or whose command protocol differs).
func runRebuildCLI(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 1 && args[0] == "--hfdesk-rebuild-probe" {
		_, _ = fmt.Fprintln(stdout, rebuildProtocolVersion)
		return true, 0
	}
	if len(args) == 0 || (args[0] != "--rebuild" && args[0] != "--hfdesk-rebuild") {
		return false, 0
	}

	flags := flag.NewFlagSet("hfdesk rebuild", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var cacheDir, hubDir string
	var clean, verbose bool
	flags.StringVar(&cacheDir, "cache-dir", "", "HF cache output root (R)")
	flags.StringVar(&hubDir, "hub-dir", "", "exact HF Hub source root (H)")
	flags.BoolVar(&clean, "clean", false, "remove orphaned symlinks in the friendly view")
	flags.BoolVar(&verbose, "verbose", false, "print detailed rebuild progress")
	flags.BoolVar(&verbose, "v", false, "print detailed rebuild progress")
	if err := flags.Parse(args[1:]); err != nil {
		return true, 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "unexpected rebuild arguments: %v\n", flags.Args())
		return true, 2
	}
	cache, err := server.ResolveRebuildCache(cacheDir, hubDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "resolve rebuild cache: %v\n", err)
		return true, 1
	}
	progress := func(message string) {
		if verbose {
			_, _ = fmt.Fprintln(stderr, message)
		}
	}
	// The rebuild waits for the cache mutation lease a running download holds
	// for its whole transfer. The wait observes the terminal session, so an
	// interrupt ends it instead of leaving the command blocked.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := cache.Sync(hfdownloader.SyncOptions{Clean: clean, Verbose: verbose, OnProgress: progress, Context: ctx})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "rebuild failed: %v\n", err)
		return true, 1
	}
	if result == nil {
		_, _ = fmt.Fprintln(stderr, "rebuild failed: Go rebuild returned no result")
		return true, 1
	}
	_, _ = fmt.Fprintln(stdout, "Rebuild complete:")
	_, _ = fmt.Fprintf(stdout, "  Repos scanned:     %d\n", result.ReposScanned)
	_, _ = fmt.Fprintf(stdout, "  Symlinks created:  %d\n", result.SymlinksCreated)
	_, _ = fmt.Fprintf(stdout, "  Symlinks updated:  %d\n", result.SymlinksUpdated)
	if clean {
		_, _ = fmt.Fprintf(stdout, "  Orphans removed:   %d\n", result.OrphansRemoved)
	}
	for _, resultErr := range result.Errors {
		if resultErr != nil {
			_, _ = fmt.Fprintf(stderr, "rebuild error: %v\n", resultErr)
		}
	}
	if len(result.Errors) > 0 {
		return true, 1
	}
	return true, 0
}
