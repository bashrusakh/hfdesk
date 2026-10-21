// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/bashrusakh/hfdesk/internal/logging"
	"golang.org/x/sys/windows"
)

// standardLog is the logger that records the messages of the standard
// library logger.
var standardLog = logging.NewComponentLogger("stdlib")

// showMessage shows a native message box.
//
// The message box is the only error surface a windowless process has: a GUI
// subsystem process has no console, so a message that is only written to
// standard error would vanish.
func showMessage(caption, text string, icon uint32) {
	textPtr, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	captionPtr, err := windows.UTF16PtrFromString(caption)
	if err != nil {
		return
	}
	flags := icon | windows.MB_OK | windows.MB_SETFOREGROUND | windows.MB_TOPMOST
	_, _ = windows.MessageBox(0, textPtr, captionPtr, flags)
}

// reportStartupFailure shows a startup failure to the user.
//
// The desktop host has no console, so a failure that is only returned to the
// caller would leave the user with a process that vanished.
func reportStartupFailure(caption, detail, logPath string) {
	text := detail
	if logPath != "" {
		text += "\n\nThe log file is at " + logPath + "."
	}
	showMessage(caption, text, windows.MB_ICONERROR)
}

// standardLogWriter is the destination of the standard library logger.
//
// The third party libraries of the desktop host report their problems
// through the standard library logger, and one of them ends the process on
// its own. A GUI process has no console, so such a message would vanish and
// the process would disappear without an explanation. Every message is
// therefore recorded in the log file and shown in a native message box.
type standardLogWriter struct {
	// mu serializes the message boxes, so two messages never overlap.
	mu sync.Mutex
}

// Write records the message and shows it to the user.
func (w *standardLogWriter) Write(p []byte) (int, error) {
	message := strings.TrimRight(string(p), "\r\n")
	if strings.TrimSpace(message) == "" {
		return len(p), nil
	}

	// The record goes into the log file through the application pipeline.
	standardLog.Error("third party message", "message", message)

	// The message box is shown under the lock, so two messages never
	// overlap and the order of the messages is kept.
	w.mu.Lock()
	showMessage("HFDesk reported a problem",
		fmt.Sprintf("A component of HFDesk reported the following problem:\n\n%s", message),
		windows.MB_ICONERROR)
	w.mu.Unlock()
	return len(p), nil
}

// installStandardLogger routes the standard library logger to the native
// message box and to the log file.
//
// It returns the function that restores the previous destination.
func installStandardLogger() func() {
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&standardLogWriter{})
	log.SetFlags(0)
	return func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	}
}
