// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// showUsage shows the usage of the desktop host to the user.
//
// The desktop host has no console, so the usage is shown in a native message
// box: a user that asked for the help would otherwise see nothing at all.
func showUsage(usage string) {
	showMessage("HFDesk usage", usage, windows.MB_ICONINFORMATION)
}

// showCommandLineError shows a command line error and the usage to the user.
//
// The desktop host has no console: a command line error that is only printed
// to standard error would leave the user with a process that vanished and no
// explanation.
func showCommandLineError(err error, usage string) {
	showMessage("HFDesk could not start",
		fmt.Sprintf("HFDesk could not read its command line.\n\n%v\n\n%s", err, usage),
		windows.MB_ICONERROR)
}
