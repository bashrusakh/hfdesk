// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"flag"
)

// ReportCommandLine shows a command line problem to the user and returns the
// exit code of the process.
//
// The desktop host is a GUI subsystem program: it has no console, so a
// message that is only written to standard error vanishes and the process
// simply disappears. The flag package would print the error and the usage to
// standard error and end the process, so the desktop host parses its flags
// with ContinueOnError and reports the problem here instead.
//
// usage is the usage text of the host, the same text the console host
// prints.
//
// A help request is not a failure: the usage is shown and the process ends
// with a success code.
func ReportCommandLine(err error, usage string) int {
	code := commandLineExitCode(err)
	if code == 0 {
		showUsage(usage)
		return code
	}
	showCommandLineError(err, usage)
	return code
}

// commandLineExitCode returns the exit code of a command line problem.
//
// A help request is not a failure: it ends the host with a success code.
// Every other problem ends the host with a non-zero code, so a launcher can
// tell that the desktop host did not start.
func commandLineExitCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}
