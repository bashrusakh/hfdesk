// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"syscall"

	"github.com/bashrusakh/hfdesk/internal/logging"
	"golang.org/x/sys/windows"
)

// standardLog is the logger that records the messages of the standard
// library logger.
var standardLog = logging.NewComponentLogger("stdlib")

// showMessage shows a native message box.
//
// It is a package level variable so the tests can replace it: the real
// message box blocks a process without an interactive window station, so a
// test that drives a path that reports a failure would never end.
var showMessage = showNativeMessage

// showNativeMessage shows a native message box.
//
// The message box is the only error surface a windowless process has: a GUI
// subsystem process has no console, so a message that is only written to
// standard error would vanish.
//
// A window whose text carries the standard log-path line of this launch is
// shown as a task dialog instead: the same words, split into instruction and
// content, with the log file reachable through one command link rather than
// printed as a line the user cannot act on. Every such window of the package
// reaches this one place, so the action is added to all of them together. The
// box below stays the fallback of that dialog — a machine whose common
// controls have no task dialog, or a call that fails for a reason of its own,
// gets exactly the window it got before, log line included.
func showNativeMessage(caption, text string, icon uint32) {
	if body, logPath := splitLogFileLine(text); logPath != "" {
		if taskDialogReport(caption, body, logPath, icon) == nil {
			return
		}
	}
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

// logFileButtonID is the command link that opens the log file of this launch.
// The failure report and the notice of the missing console program use this
// one id, so one callback rule answers the press in either dialog, and no id
// of one dialog can be read as the answer of the other by accident.
const logFileButtonID = 110

// user32SetWindowPos is how a failure report asks the window manager to put
// itself above the others; the shared system library carries no Go wrapper
// for the call.
var user32SetWindowPos = windows.NewLazySystemDLL("user32.dll").NewProc("SetWindowPos")

// The z-order a report takes, and the flags that change nothing but that
// order: hwndTopmost is (HWND)-1 — above every other top-level window, which
// is where the message box these reports replaced always landed — and the
// flags keep the position and the size the dialog chose for itself.
const (
	hwndTopmost     = ^uintptr(0)
	swpKeepGeometry = 0x0001 | 0x0002 // SWP_NOSIZE | SWP_NOMOVE
)

// The predefined icons of a task dialog, as MAKEINTRESOURCEW(-1..-3): the
// common controls name their stock icons by these integer resources when the
// dialog is asked for them without a module.
const (
	tdWarningIcon     = 0xFFFF
	tdErrorIcon       = 0xFFFE
	tdInformationIcon = 0xFFFD
)

// taskDialogIcon maps the icon of a message box onto the predefined icon of a
// task dialog. An icon of the box that has no counterpart — the question mark
// of a confirmation — shows no icon instead of a wrong one; the confirmations
// never name a log file, so they never reach the report anyway.
func taskDialogIcon(icon uint32) uintptr {
	switch icon {
	case windows.MB_ICONERROR:
		return tdErrorIcon
	case windows.MB_ICONWARNING:
		return tdWarningIcon
	case windows.MB_ICONINFORMATION:
		return tdInformationIcon
	default:
		return 0
	}
}

// splitReportText splits the text of a failure window into the bold
// instruction a task dialog leads with and the content under it: the first
// line is the headline every report of this package already starts with, and
// the rest is what it says beside it.
func splitReportText(text string) (instruction, content string) {
	instruction, content, _ = strings.Cut(text, "\n")
	return instruction, strings.TrimLeft(content, "\n")
}

// openFileWithDefaultProgram opens a file with the program Windows
// associates with it.
//
// It is a package level variable so the tests can replace it: the real call
// starts another program and blocks the dialog that asks for it.
var openFileWithDefaultProgram = shellExecuteOpenFile

// shellExecuteOpenFile opens path with the shell of Windows: the verb "open"
// hands the file to the program registered for its kind, which is how a user
// reads a log file — in their own editor, not in a window of HFDesk.
func shellExecuteOpenFile(path string) error {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	verbPtr, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verbPtr, pathPtr, nil, nil, windows.SW_SHOWNORMAL)
}

// logFileCallback returns the dialog callback that answers the "Open the log
// file" command link of a window: it opens the file and keeps the window open
// (S_FALSE is the documented answer that prevents a task dialog from closing),
// so the report or the notice the user came to read is still there when the
// file opened beside it. The closure owns the path for as long as the dialog
// runs, and any other notification of the dialog keeps the ordinary answer.
//
// topmost asks for the z-order of the window the moment it is created: a
// failure report of the log pipeline arrives on a goroutine that is not the
// foreground thread, and a window that lands behind the others is a report
// nobody reads — the reason the message box this dialog replaces was shown
// with MB_TOPMOST. The notice of the missing console program passes false: it
// is asked from the foreground thread of the user's own click and keeps the
// z-order its task dialog always had.
//
// It is created per dialog, the way the progress dialog of the transfer
// already creates its own, so no state of one window can reach another.
func logFileCallback(logPath string, topmost bool) uintptr {
	return syscall.NewCallback(func(hwnd, msg, wParam, lParam, data uintptr) uintptr {
		switch msg {
		case tdnCreated:
			if topmost {
				// Only the z-order is asked for: the flags keep
				// the position and the size the dialog chose,
				// and a call the window manager refuses changes
				// nothing about the window.
				user32SetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpKeepGeometry)
			}
		case tdnButtonClicked:
			if int32(wParam) == logFileButtonID {
				if err := openFileWithDefaultProgram(logPath); err != nil {
					// The path only exists where this launch
					// really writes a log, so the pipeline is
					// installed and this record reaches that
					// very file.
					desktopLog.Warn("the log file could not be opened", "path", logPath, "error", err)
				}
				return 1 // S_FALSE: the window stays open
			}
		}
		return 0
	})
}

// taskDialogReport shows a failure window as a task dialog: its first line as
// the instruction, the rest as content, the log file it names as one command
// link, and OK as the default button — so Enter and Esc both simply close the
// window, and only a deliberate press of the link opens the file. The window
// keeps the z-order of the message box it replaces: above the others, so a
// report that arrives while the user works elsewhere is still read.
//
// text is the body of the window without its log-path line; logPath is the
// file that line named. A failure of the call is returned to the caller, which
// then shows the very message box this dialog would have replaced.
func taskDialogReport(caption, text, logPath string, icon uint32) error {
	title, err := windows.UTF16PtrFromString(caption)
	if err != nil {
		return err
	}
	first, rest := splitReportText(text)
	if strings.TrimSpace(first) == "" {
		// A window without a headline of its own is not a report this
		// dialog can lead with; the caller shows its text as it is.
		return fmt.Errorf("the failure window %q carries no text", caption)
	}
	instruction, err := windows.UTF16PtrFromString(first)
	if err != nil {
		return err
	}
	var content *uint16
	if rest != "" {
		if content, err = windows.UTF16PtrFromString(rest); err != nil {
			return err
		}
	}
	openLabel, err := windows.UTF16PtrFromString(logFileOpenLabel + "\n" + logPath)
	if err != nil {
		return err
	}

	config := taskDialogConfig{
		dwFlags:            tdfAllowDialogCancellation | tdfUseCommandLinks,
		pszWindowTitle:     title,
		pszMainInstruction: instruction,
		pszContent:         content,
		mainIcon:           taskDialogIcon(icon),
		dwCommonButtons:    tdcOKButton,
		nDefaultButton:     standardButtonOKID,
		pfCallback:         logFileCallback(logPath, true),
	}
	if _, err := runTaskDialog(config, []taskDialogButton{{id: logFileButtonID, text: openLabel}}); err != nil {
		return err
	}
	return nil
}

// reportStartupFailure shows a startup failure to the user.
//
// The desktop host has no console, so a failure that is only returned to the
// caller would leave the user with a process that vanished.
func reportStartupFailure(caption, detail, logPath string) {
	showMessage(caption, detail+logFileLine(logPath), windows.MB_ICONERROR)
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
	// overlap and the order of the messages is kept. The window names the
	// log this launch writes — the message above reached it — so the log
	// is reachable from outside the window.
	w.mu.Lock()
	showMessage("HFDesk reported a problem",
		failureWindowText(fmt.Sprintf("A component of HFDesk reported the following problem:\n\n%s", message)),
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
