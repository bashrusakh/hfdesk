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
//
// The binary is also the elevated helper of the Windows service: the desktop
// host starts the very same executable with the hidden flags
//
//	--service-action install|update|start
//	--wait-pid N
//
// and the helper performs the action, reports it in a native message box and
// ends without a server, a tray icon or a window. The flags are private to the
// desktop executable: they are not part of the options of the hfdesk command,
// and they are not documented in its usage.
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

// run performs the startup of the process.
//
// The command is first read as the elevated helper of the Windows service: a
// launch that carries the hidden service flags performs the action and ends
// before anything of the desktop host exists. Every other command starts the
// desktop host.
//
// The exit code of the process is returned: zero for a process that ended
// without a failure.
func run() int {
	// The helper runs first and on a flag set of its own: the service flags
	// are private to the desktop executable, so they are neither part of the
	// shared options of the server host nor part of its usage text.
	//
	// A command that the helper flag set does not accept is the command of
	// the desktop host, and the flag set below reports its problems. A helper
	// command is therefore recognized by its action flag and never by the
	// absence of a host flag.
	var helper desktop.ServiceActionOptions
	var helperAction string
	helperFlags := flag.NewFlagSet("hfdesk-desktop-service", flag.ContinueOnError)
	helperFlags.SetOutput(io.Discard)
	helperFlags.StringVar(&helperAction, "service-action", "",
		"install, update or start the HFDesk service")
	helperFlags.IntVar(&helper.WaitPID, "wait-pid", 0,
		"wait for this process to exit before the service starts")
	if err := helperFlags.Parse(os.Args[1:]); err == nil && helperAction != "" {
		helper.Action = helperAction
		helper.Version = Version
		if err := desktop.RunServiceAction(helper); err != nil {
			// The helper reports its outcome to the user itself, in a native
			// message box: the exit code only tells the launcher what
			// happened. A GUI process has no console, so the line is written
			// for a command line that started the helper by hand.
			fmt.Fprintf(os.Stderr, "hfdesk-desktop: %v\n", err)
			return 1
		}
		return 0
	}

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
