// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/bashrusakh/hfdesk/internal/server"
)

// Version is set at build time via ldflags.
var Version = "1.0.1"

func main() {
	var (
		port     int
		cacheDir string
		token    string
		localDir string
		openUI   bool
		noOpen   bool
	)

	flag.IntVar(&port, "port", 8080, "HTTP port")
	flag.StringVar(&cacheDir, "cache-dir", "", "Hugging Face cache directory")
	flag.StringVar(&token, "token", "", "Hugging Face token")
	flag.StringVar(&localDir, "local-dir", "", "write real files into this directory instead of the HF cache layout")
	flag.BoolVar(&openUI, "open", true, "open the web UI in the default browser")
	flag.BoolVar(&noOpen, "no-open", false, "do not open the web UI automatically")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "HFDesk %s\n\nUsage: hfdesk [options]\n\nOptions:\n", Version)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if openUI && !noOpen {
		go openBrowser(fmt.Sprintf("http://localhost:%d", cfg.Port))
	}

	if err := server.New(cfg).ListenAndServe(ctx); err != nil {
		log.Fatal(err)
	}
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
