// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/bashrusakh/hfdesk/internal/pe"
)

var scmIsWow64GuestMachineSupported = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsWow64GuestMachineSupported")

func scmNativeWindowsMachine() (uint16, error) {
	var processMachine, nativeMachine uint16
	if err := windows.IsWow64Process2(windows.CurrentProcess(), &processMachine, &nativeMachine); err != nil {
		return 0, fmt.Errorf("query the native Windows machine: %w", err)
	}
	if nativeMachine == 0 {
		return 0, fmt.Errorf("Windows did not report the native machine")
	}
	return nativeMachine, nil
}

func scmWindowsGuestMachineSupported(machine uint16) (bool, error) {
	if err := scmIsWow64GuestMachineSupported.Find(); err != nil {
		return false, err
	}
	var supported uint32
	hresult, _, callErr := scmIsWow64GuestMachineSupported.Call(
		uintptr(machine), uintptr(unsafe.Pointer(&supported)),
	)
	if uint32(hresult) != 0 {
		if callErr != syscall.Errno(0) {
			return false, callErr
		}
		return false, fmt.Errorf("IsWow64GuestMachineSupported returned HRESULT 0x%08X", uint32(hresult))
	}
	return supported != 0, nil
}

func scmWindowsExecutableSupport(machine uint16) scmExecutableSupport {
	native, err := scmNativeWindowsMachine()
	if err != nil {
		return scmExecutableSupportUnknown
	}
	return scmExecutableSupportForHost(machine, native, scmWindowsGuestMachineSupported)
}

func preflightExecutableImage(executable string) error {
	return preflightExecutableImageWithSupport(executable, scmWindowsExecutableSupport)
}

func preflightExecutableImageWithSupport(executable string, support func(uint16) scmExecutableSupport) error {
	return preflightExecutableImageUsing(executable, pe.ExecutableMachine, support)
}

func preflightExecutableImageUsing(executable string, readMachine func(string) (uint16, error), support func(uint16) scmExecutableSupport) error {
	machine, err := readMachine(executable)
	if err != nil {
		if scmPEInspectionUnavailable(err) {
			log.Printf("service: PE structure inspection of %q is inconclusive (%v); the installer did not classify the executable as invalid", executable, err)
			return nil
		}
		return fmt.Errorf("service: executable %q is not a Windows executable: %w", executable, err)
	}
	if support == nil {
		return nil
	}
	switch support(machine) {
	case scmExecutableUnsupported:
		return fmt.Errorf("service: executable %q has PE machine 0x%04X, which Windows reports this host cannot execute", executable, machine)
	case scmExecutableSupportUnknown:
		log.Printf("service: Windows execution support for PE machine 0x%04X of %q cannot be determined; the installer did not infer incompatibility", machine, executable)
	}
	return nil
}

func scmPEInspectionUnavailable(err error) bool {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return false
	}
	var pathErr *os.PathError
	var linkErr *os.LinkError
	return os.IsPermission(err) || errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.As(err, &pathErr) || errors.As(err, &linkErr)
}
