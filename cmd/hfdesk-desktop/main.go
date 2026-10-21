// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

// Command hfdesk-desktop is the Windows desktop host of HFDesk.
//
// It runs the same HFDesk server as the hfdesk command, in the same
// process, and adds the desktop shell: a system tray icon, a small
// Runtime / Logs window and the single instance lock.
//
// The binary has to be linked with the Windows GUI subsystem, so it has no
// console: every problem is reported in a native message box and written to
// the log file. The subsystem is a build time property of the linker, so the
// binary is built with the flag -H=windowsgui:
//
//	go build -ldflags "-H=windowsgui" -o hfdesk-desktop.exe ./cmd/hfdesk-desktop
//
// A binary that was built without the flag is a console subsystem program: it
// opens a console window next to the notification area icon. The host reads
// the PE header of its own executable at startup and tells the user how to
// rebuild the binary when it was built wrong. It keeps running either way.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/bashrusakh/hfdesk/internal/app"
	"github.com/bashrusakh/hfdesk/internal/desktop"
)

// Version is set at build time via ldflags (-X main.Version=...) from the
// VERSION file. Defaults to "dev" for un-flagged local builds.
var Version = "dev"

func main() {
	if err := run(); err != nil {
		// The desktop host reports its failures to the user itself, in a
		// native message box. On a platform without a desktop host there
		// is no message box, so the error goes to the console.
		fmt.Fprintf(os.Stderr, "hfdesk-desktop: %v\n", err)
		os.Exit(1)
	}
}

// run performs the desktop startup: it parses the shared flags and hands the
// startup over to the desktop host.
//
// The flags are the flags of the hfdesk command, so both hosts accept the
// same options and read the same configuration file.
func run() error {
	var opts app.Options

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "HFDesk %s\n\nUsage: hfdesk-desktop [options]\n\nOptions:\n", Version)
		flag.PrintDefaults()
	}
	app.RegisterFlags(flag.CommandLine, &opts)
	flag.Parse()

	return desktop.Run(desktop.Options{App: opts, Version: Version})
}
