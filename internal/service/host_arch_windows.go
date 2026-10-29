// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"fmt"
	"log"
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
	machine, err := pe.ExecutableMachine(executable)
	if err != nil {
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
