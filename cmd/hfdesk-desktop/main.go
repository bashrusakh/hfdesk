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
//	--service-action install --wait-pid N --handover-ready-event NAME
//	  --handover-success-event NAME --instance-mutex NAME
//	--service-action update|start
//
// and the helper performs the action, reports it in a native message box and
// ends without a server, a tray icon or a window. The flags are private to the
// desktop executable: they are not part of the options of the hfdesk command,
// and they are not documented in its usage.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

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
	return runWithArgs(os.Args[1:])
}

func runWithArgs(args []string) int {
	// The helper runs first and on a flag set of its own: the service flags
	// are private to the desktop executable, so they are neither part of the
	// shared options of the server host nor part of its usage text.
	//
	// A command that carries any helper flag is handled here, even when its
	// arguments are incomplete, so it cannot fall through and start a server.
	helper, helperInvocation, helperErr := parseServiceHelperCommand(args)
	if helperInvocation {
		if helperErr != nil {
			return desktop.ReportCommandLine(helperErr, serviceHelperUsage)
		}
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

	if err := fs.Parse(args); err != nil {
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

const serviceHelperUsage = "Usage: hfdesk-desktop --service-action install --wait-pid N " +
	"--handover-ready-event NAME --handover-success-event NAME --instance-mutex NAME\n" +
	"       hfdesk-desktop --service-action update|start"

func parseServiceHelperCommand(args []string) (desktop.ServiceActionOptions, bool, error) {
	var helper desktop.ServiceActionOptions
	var action string
	flags := flag.NewFlagSet("hfdesk-desktop-service", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&action, "service-action", "", "install, update or start the HFDesk service")
	flags.IntVar(&helper.WaitPID, "wait-pid", 0, "the original desktop process")
	flags.StringVar(&helper.ReadyEvent, "handover-ready-event", "", "the per-attempt helper-ready event")
	flags.StringVar(&helper.SuccessEvent, "handover-success-event", "", "the per-attempt graceful-completion event")
	flags.StringVar(&helper.InstanceMutex, "instance-mutex", "", "the existing per-session desktop singleton")
	err := flags.Parse(args)
	if !hasServiceHelperArgument(args) {
		return desktop.ServiceActionOptions{}, false, nil
	}
	if err != nil {
		return helper, true, err
	}
	// A value that is not a flag ends flag parsing, so every flag after it
	// stays unconsumed: a helper that ran on silently dropped arguments
	// could perform its service action without the evidence those flags
	// name — for example without waiting for the desktop exit — so the
	// invocation is rejected instead, naming what was left behind.
	if flags.NArg() != 0 {
		return helper, true, fmt.Errorf(
			"the service helper arguments are not all flags: %s was not consumed, and a value that is not a flag ends flag parsing, so the flags after it are ignored",
			strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(action) == "" {
		return helper, true, errors.New("the service helper arguments do not name an action")
	}
	helper.Action = action
	return helper, true, nil
}

func hasServiceHelperArgument(args []string) bool {
	for _, arg := range args {
		for _, name := range []string{
			"--service-action", "-service-action",
			"--wait-pid", "-wait-pid",
			"--handover-ready-event", "-handover-ready-event",
			"--handover-success-event", "-handover-success-event",
			"--instance-mutex", "-instance-mutex",
		} {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				return true
			}
		}
	}
	return false
}
