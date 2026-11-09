// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package desktop

import (
	"fmt"
	"os"
)

// showUsage writes the usage of the desktop host to standard error.
//
// There is no message box on this platform: the desktop host is only
// supported on Windows, so the console is the only surface there is.
func showUsage(usage string) {
	fmt.Fprint(os.Stderr, usage)
}

// showCommandLineError writes a command line error and the usage to standard
// error.
//
// There is no message box on this platform: the desktop host is only
// supported on Windows, so the console is the only surface there is.
func showCommandLineError(err error, usage string) {
	fmt.Fprintf(os.Stderr, "hfdesk-desktop: %v\n\n%s", err, usage)
}
