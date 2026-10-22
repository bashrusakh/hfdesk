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
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/bashrusakh/hfdesk/internal/app"
	"github.com/bashrusakh/hfdesk/internal/desktop"
)

// Version is set at build time via ldflags (-X main.Version=...) from the
// VERSION file. Defaults to "dev" for un-flagged local builds.
var Version = "dev"

func main() {
	if code := run(); code != 0 {
		os.Exit(code)
	}
}

// run performs the desktop startup: it parses the shared flags and hands the
// startup over to the desktop host.
//
// The flags are the flags of the hfdesk command, so both hosts accept the
// same options and read the same configuration file.
//
// The flags are parsed with ContinueOnError: the default of the flag package
// prints the error and the usage to standard error and ends the process, and
// a GUI subsystem program has no console, so the user would see a process
// that vanished without an explanation. The problem is reported to the user
// in a native message box instead, and the process ends with a non-zero exit
// code.
//
// The exit code of the process is returned: zero for a host that started and
// that ended without a failure.
func run() int {
	var opts app.Options

	// The usage is collected on its own: the desktop host shows the
	// command line problem in a message box, and the box shows the error
	// and the usage. The output of the flag package is discarded, so the
	// error is not reported a second time.
	var usageText bytes.Buffer
	fs := flag.NewFlagSet("hfdesk-desktop", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {
		fs.SetOutput(&usageText)
		fmt.Fprintf(&usageText, "HFDesk %s\n\nUsage: hfdesk-desktop [options]\n\nOptions:\n", Version)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
	}
	app.RegisterFlags(fs, &opts)

	if err := fs.Parse(os.Args[1:]); err != nil {
		return desktop.ReportCommandLine(err, usageText.String())
	}

	if err := desktop.Run(desktop.Options{App: opts, Version: Version}); err != nil {
		// The desktop host reports its failures to the user itself, in a
		// native message box. On a platform without a desktop host there
		// is no message box, so the error goes to the console.
		fmt.Fprintf(os.Stderr, "hfdesk-desktop: %v\n", err)
		return 1
	}
	return 0
}
