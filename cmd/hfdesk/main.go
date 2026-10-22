// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/bashrusakh/hfdesk/internal/server"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// Version is set at build time via ldflags (-X main.Version=...) from the
// VERSION file. Defaults to "dev" for un-flagged local builds.
var Version = "dev"

func main() {
	if handled, code := runRebuildCLI(os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	// Surface the build version to the API/UI so the displayed version tracks
	// the binary instead of a hardcoded string.
	server.Version = Version

	var (
		port         int
		cacheDir     string
		token        string
		localDir     string
		stallTimeout string
		openUI       bool
		noOpen       bool
	)

	flag.IntVar(&port, "port", 8080, "HTTP port")
	flag.StringVar(&cacheDir, "cache-dir", "", "Hugging Face cache directory")
	flag.StringVar(&token, "token", "", "Hugging Face token (also reads HF_TOKEN env)")
	flag.StringVar(&localDir, "local-dir", "", "write real files into this directory instead of the HF cache layout")
	flag.StringVar(&stallTimeout, "stall-timeout", "", "abort a stalled download read after this duration (e.g. 60s; 0 disables)")
	flag.BoolVar(&openUI, "open", true, "open the web UI in the default browser")
	flag.BoolVar(&noOpen, "no-open", false, "do not open the web UI automatically")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "HFDesk %s\n\nUsage: hfdesk [options]\n       hfdesk --rebuild [--cache-dir R] [--hub-dir H] [--clean] [--verbose]\n\nOptions:\n", Version)
		flag.PrintDefaults()
	}
	flag.Parse()

	cfg := server.DefaultConfig()
	cfg.Port = port
	cfg.CacheDir = cacheDir
	cfg.Token = token
	cfg.LocalDir = localDir
	if err := server.ApplyConfigToServer(&cfg); err != nil {
		log.Fatalf("load config: %v", err)
	}
	// CLI flag takes precedence over the persisted setting. Reject a negative
	// or unparseable value: it must never silently disable the stall watchdog
	// or be accepted as a bogus duration.
	if stallTimeout != "" {
		d, err := time.ParseDuration(stallTimeout)
		if err != nil || d < 0 {
			log.Fatalf("invalid --stall-timeout %q: expected a non-negative duration such as 60s or 0 to disable", stallTimeout)
		}
		cfg.StallTimeout = stallTimeout
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if openUI && !noOpen {
		go openBrowser(fmt.Sprintf("http://localhost:%d", cfg.Port))
	}

	if err := server.New(cfg).ListenAndServe(ctx); err != nil {
		log.Fatal(err)
	}
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
	result, err := cache.Sync(hfdownloader.SyncOptions{Clean: clean, Verbose: verbose, OnProgress: progress})
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

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("open browser: %v", err)
	}
}
