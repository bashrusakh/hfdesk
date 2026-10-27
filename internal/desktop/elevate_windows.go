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
	// WaitPID is the process the helper waits for before it starts or
	// restarts the service. Zero means there is nothing to wait for.
	WaitPID int
}

// launchElevated starts the service helper with administrator rights.
//
// It is a package level variable so the tests can drive the launch: a test
// cannot answer a prompt of the user account control, and a real elevated
// process would change the machine.
var launchElevated = shellExecuteElevated

// shellExecuteEx is the entry point of the shell of Windows.
var shellExecuteEx = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

// shellExecuteInfo is the SHELLEXECUTEINFO of the Windows API.
//
// The struct mirrors the C declaration field by field, including the padding
// the 64-bit ABI inserts around the 32-bit fields: the shell reads a pointer
// out of every field, so a field at the wrong offset would make the call read
// a string out of a flag.
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
	// _ is the padding of the 64-bit ABI.
	_ uint32
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
	// _ is the padding of the 64-bit ABI.
	_ uint32
	// hIcon is the union of hIcon and hMonitor.
	hIcon uintptr
	// hProcess is the handle of the started process. It is only set when
	// SEE_MASK_NOCLOSEPROCESS was asked for.
	hProcess uintptr
}

// shellExecuteElevated starts the service helper with administrator rights.
//
// The verb "runas" asks the shell for an elevated process: the user account
// control shows its prompt, and the call only returns after the user answered
// it. The program is named by its absolute path and the arguments carry the
// action and the process the helper waits for, so the helper never depends on
// the working directory of the desktop host.
//
// A refusal of the user is reported as such: the desktop host is not allowed to
// change the machine, and it keeps running untouched. Every other failure
// carries the error of the shell.
func shellExecuteElevated(l elevatedLaunch) (uint32, error) {
	if !filepath.IsAbs(l.Executable) {
		return 0, fmt.Errorf("the desktop program %q is not an absolute path", l.Executable)
	}

	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return 0, err
	}
	program, err := windows.UTF16PtrFromString(l.Executable)
	if err != nil {
		return 0, err
	}
	parameters, err := windows.UTF16PtrFromString(serviceHelperArguments(l))
	if err != nil {
		return 0, err
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
				return 0, errors.New(elevationDeclined)
			}
			return 0, fmt.Errorf("the elevated service helper could not be started: %w", callErr)
		}
		return 0, errors.New("the elevated service helper could not be started")
	}

	// The handle is asked for with SEE_MASK_NOCLOSEPROCESS, so the process
	// is named after it. A launch that reports no handle is still a started
	// helper: it waits for this process itself, so the host quits either
	// way, and the missing handle is recorded instead of reported as a
	// failure of the launch.
	pid := uint32(0)
	if info.hProcess != 0 {
		handle := windows.Handle(info.hProcess)
		if named, err := windows.GetProcessId(handle); err == nil {
			pid = named
		}
		_ = windows.CloseHandle(handle)
	}
	return pid, nil
}

// serviceHelperArguments returns the command line of the elevated helper.
//
// The action is the value of the hidden flag of the desktop executable, and the
// process to wait for is the desktop host itself: the helper starts the service
// only after this process exited, so the service never binds an address the
// desktop still holds.
func serviceHelperArguments(l elevatedLaunch) string {
	args := []string{"--service-action", string(l.Action)}
	if l.WaitPID > 0 {
		args = append(args, "--wait-pid", strconv.Itoa(l.WaitPID))
	}
	return strings.Join(args, " ")
}
