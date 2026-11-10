// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The flags and the value of SHELLEXECUTEINFO.
const (
	// seeMaskNoCloseProcess asks the shell for the handle of the started
	// process. The helper is a process of its own, so the handle is the
	// only way to name and to release the process the shell started.
	seeMaskNoCloseProcess = 0x00000040
	// swShowNormal shows the started program in a normal window. The helper
	// has no window, so the value only reaches the creation of the process.
	swShowNormal = 1
)

// elevatedLaunch describes one launch of the elevated service helper.
type elevatedLaunch struct {
	// Executable is the absolute path of the program to start: the desktop
	// executable itself.
	Executable string
	// Action is the service action of the helper.
	Action serviceAction
	// WaitPID is the original desktop process for an install handover.
	// Zero leaves start on its ordinary path; an update or a start may
	// still carry InstanceMutex alone.
	WaitPID int
	// ReadyEvent and SuccessEvent are the per-attempt handover event names.
	ReadyEvent   string
	SuccessEvent string
	// InstanceMutex names the existing desktop singleton object to hold.
	InstanceMutex string
	// DesktopState is the path of the published claim of the desktop that
	// launches the helper; it travels beside InstanceMutex so the guard of
	// the action reads the ORIGINATING desktop's claim even when another
	// administrator approves the elevation.
	DesktopState string
}

// launchElevated starts the service helper with administrator rights.
//
// It is a package level variable so the tests can drive the launch: a test
// cannot answer a prompt of the user account control, and a real elevated
// process would change the machine.
var launchElevated = shellExecuteElevated

var launchElevatedProcess = shellExecuteElevatedProcess

type elevatedProcess struct {
	handle windows.Handle
	pid    uint32
}

func (p *elevatedProcess) Close() {
	if p == nil || p.handle == 0 {
		return
	}
	_ = windows.CloseHandle(p.handle)
	p.handle = 0
}

// shellExecuteEx is the entry point of the shell of Windows.
var shellExecuteEx = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

// shellExecuteInfo is the SHELLEXECUTEINFO of the Windows API.
//
// The struct mirrors the C declaration field by field. The padding of the
// 64-bit ABI around the 32-bit fields is inserted by the Go compiler itself,
// because a field's alignment is architecture dependent: an explicitly
// spelled padding field would occupy a real slot on the 32-bit ABI, where the
// native structure has none, and shift every later field — the shell reads a
// pointer out of every field, so a field at the wrong offset would make the
// call read a string out of a flag. cbSize is the size of this struct at the
// call site, so it has to match the native size of the running architecture.
type shellExecuteInfo struct {
	// cbSize is the size of the struct.
	cbSize uint32
	// fMask are the SEE_MASK_* flags.
	fMask uint32
	// hwnd is the owner of the dialog the shell may show.
	hwnd uintptr
	// lpVerb is the action the shell performs.
	lpVerb *uint16
	// lpFile is the program the shell starts.
	lpFile *uint16
	// lpParameters is the command line of the program.
	lpParameters *uint16
	// lpDirectory is the working directory of the program.
	lpDirectory *uint16
	// nShow is the show state of the program.
	nShow int32
	// hInstApp is the instance of the application.
	hInstApp uintptr
	// lpIDList is the item identifier list of the shell.
	lpIDList uintptr
	// lpClass is the class of the file.
	lpClass *uint16
	// hkeyClass is the class key of the file.
	hkeyClass uintptr
	// dwHotKey is the hot key of the launched program.
	dwHotKey uint32
	// hIcon is the union of hIcon and hMonitor.
	hIcon uintptr
	// hProcess is the handle of the started process. It is only set when
	// SEE_MASK_NOCLOSEPROCESS was asked for.
	hProcess uintptr
}

// shellExecuteElevated starts the service helper with administrator rights and
// closes the returned process handle after reading its pid.
//
// The verb "runas" asks the shell for an elevated process: the user account
// control shows its prompt, and the call only returns after the user answered
// it. The program is named by its absolute path and the arguments carry the
// action and the per-attempt handover names, so the helper never depends on the
// working directory or identity of the desktop host.
//
// A refusal of the user is reported as such: the desktop host is not allowed to
// change the machine, and it keeps running untouched. Every other failure
// carries the error of the shell.
func shellExecuteElevated(l elevatedLaunch) (uint32, error) {
	process, err := shellExecuteElevatedProcess(l)
	if err != nil {
		return 0, err
	}
	if process == nil {
		return 0, nil
	}
	defer process.Close()
	return process.pid, nil
}

func shellExecuteElevatedProcess(l elevatedLaunch) (*elevatedProcess, error) {
	if !filepath.IsAbs(l.Executable) {
		return nil, fmt.Errorf("the desktop program %q is not an absolute path", l.Executable)
	}

	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return nil, err
	}
	program, err := windows.UTF16PtrFromString(l.Executable)
	if err != nil {
		return nil, err
	}
	parameters, err := windows.UTF16PtrFromString(serviceHelperArguments(l))
	if err != nil {
		return nil, err
	}
	info := shellExecuteInfo{
		cbSize:       uint32(unsafe.Sizeof(shellExecuteInfo{})),
		fMask:        seeMaskNoCloseProcess,
		lpVerb:       verb,
		lpFile:       program,
		lpParameters: parameters,
		nShow:        swShowNormal,
	}

	started, _, callErr := shellExecuteEx.Call(uintptr(unsafe.Pointer(&info)))
	// The shell reads the struct and every string in it during the call.
	runtime.KeepAlive(info)
	runtime.KeepAlive(parameters)
	runtime.KeepAlive(program)
	runtime.KeepAlive(verb)

	if started == 0 {
		if callErr != nil {
			if errors.Is(callErr, windows.ERROR_CANCELLED) {
				return nil, errors.New(elevationDeclined)
			}
			return nil, fmt.Errorf("the elevated service helper could not be started: %w", callErr)
		}
		return nil, errors.New("the elevated service helper could not be started")
	}

	process := &elevatedProcess{}
	if info.hProcess != 0 {
		process.handle = windows.Handle(info.hProcess)
		process.pid, _ = windows.GetProcessId(process.handle)
	}
	return process, nil
}

// serviceHelperArguments returns the command line of the elevated helper.
//
// Install carries the process, event names, and existing singleton identity of
// its handover. An update or start from the service gate carries the singleton
// identity alone: its mutation moves the service after the launching desktop
// ended, so the helper holds the singleton across that window without waiting
// for an exit. Every other command carries no wait state. The state path of
// the launching desktop travels beside the singleton identity in every form,
// so the helper's guard reads the claim of the ORIGINATING desktop under any
// approving account.
func serviceHelperArguments(l elevatedLaunch) string {
	args := []string{"--service-action", string(l.Action)}
	switch {
	case l.WaitPID > 0:
		args = append(args,
			"--wait-pid", strconv.Itoa(l.WaitPID),
			"--handover-ready-event", l.ReadyEvent,
			"--handover-success-event", l.SuccessEvent,
			"--instance-mutex", l.InstanceMutex)
	case strings.TrimSpace(l.InstanceMutex) != "":
		args = append(args, "--instance-mutex", l.InstanceMutex)
	}
	if path := strings.TrimSpace(l.DesktopState); path != "" {
		args = append(args, "--desktop-state", quoteHelperArgument(path))
	}
	return strings.Join(args, " ")
}

// quoteHelperArgument wraps a helper argument value that may contain spaces
// in the double quotes of the Windows command line, so the helper receives
// it as ONE flag value. The state path of a user profile usually contains a
// space ("C:\Users\First Last\..."); an unquoted value would end at the
// first space, and the helper's fail-closed parse would reject the whole
// invocation as unconsumed arguments instead of reading the claim.
//
// A quoted value is only needed where a value can contain a space: the
// fixed names of the handover never do, so their established command line
// stays byte for byte what it was. A Windows file name cannot contain a
// double quote, so the quoting cannot collide with the value itself.
func quoteHelperArgument(value string) string {
	if !strings.ContainsAny(value, " \t") {
		return value
	}
	return `"` + value + `"`
}
