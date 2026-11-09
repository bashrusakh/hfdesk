// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/bashrusakh/hfdesk/internal/logging"
	"golang.org/x/sys/windows"
)

// panicGuard is the panic handler of the desktop host.
//
// The host has no console: a panic that is not caught ends the process
// without an explanation and without a log record. The guard records the
// panic in the log file, shows it to the user in a native message box and
// ends the host through the normal path, so the server is stopped and the
// job state is flushed before the process ends.
type panicGuard struct {
	// logPath is the path of the log file. It is shown to the user.
	logPath string
	// end stops the host: it removes the published state, stops the
	// server through the normal path and closes the log pipeline.
	end func()
}

// report records the panic in the log file and shows it to the user.
//
// It returns the text of the panic. The text is redacted, so a credential
// that is part of the panic value cannot reach the message box.
//
// The stack is recorded in the log file only: it is long and it names the
// build paths of the binary, while the message box is for the user.
func (g panicGuard) report(recovered any) string {
	detail := logging.Redact(fmt.Sprintf("%v", recovered))
	desktopLog.Error("the desktop host stopped after a panic",
		"panic", detail,
		"stack", string(debug.Stack()))

	text := "HFDesk stopped after an unexpected error.\n\n" + detail
	if g.logPath != "" {
		text += "\n\nThe log file is at " + g.logPath + "."
	}
	showMessage("HFDesk stopped unexpectedly", text, windows.MB_ICONERROR)
	return detail
}

// runBackground runs a background action of the host under the guard.
//
// A panic in a background action would end the process without a message box
// and without a log record, so the panic is reported and the host ends
// through the normal path: the server is stopped and the process exits with
// a failure code.
func (g panicGuard) runBackground(action func()) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		g.report(recovered)
		g.end()
		os.Exit(1)
	}()
	action()
}
