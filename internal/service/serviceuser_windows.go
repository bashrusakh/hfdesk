// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// desktopSingletonPrefix is the prefix of the name of the per-session
// desktop singleton object.
//
// The Local prefix keeps the object inside the namespace of the session, so
// a second session of the same user does not see it. The name moved here
// from the desktop host: the elevated helper and the service command hold
// this very object, and one computation keeps the three from drifting apart.
const desktopSingletonPrefix = `Local\HFDesk.Desktop.`

// DesktopSingletonName returns the name of the per-session desktop singleton
// object: the single-instance lock of the Windows desktop host, scoped to
// the user and to the session of this process.
//
// It exists on Windows only, where the desktop host exists; the callers of
// it are compiled for Windows as well.
func DesktopSingletonName() (string, error) {
	sid, session, err := desktopSessionIdentity()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%s.%d", desktopSingletonPrefix, sid, session), nil
}

// desktopSessionIdentity returns the user and the session of the process for
// the name of the singleton.
//
// It is a package level variable so the tests can drive the refusal of a
// hold whose object cannot be named: a token or session that cannot be read
// must fail the hold before the mutation, never produce a guessed name.
var desktopSessionIdentity = currentDesktopSessionIdentity

func currentDesktopSessionIdentity() (string, uint32, error) {
	sid, err := processUserSID()
	if err != nil {
		return "", 0, err
	}
	// The pseudo token of the current process needs no closing.
	var session uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &session); err != nil {
		return "", 0, fmt.Errorf("service: read the session of the process: %w", err)
	}
	return sid, session, nil
}

// processUserSID returns the security identifier of the user of the process.
func processUserSID() (string, error) {
	// The pseudo token of the current process needs no closing.
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("service: read the user of the process: %w", err)
	}
	return user.User.Sid.String(), nil
}

// holdDesktopSingleton holds the per-session desktop singleton object of
// HoldDesktopSingleton for the calling process.
//
// The object is created with the descriptor of the desktop host's own
// instance lock — the user of the process with full mutex access, the
// administrators and the system with read control — so the hold behaves
// exactly like that lock: a launch of the desktop app of the same identity
// finds the object and reports the running instance, and the descriptor of an
// existing object is never replaced. ERROR_ALREADY_EXISTS still returns a
// live handle to the existing object, and that handle is exactly the hold
// this path needs: the operating system keeps the object until the last
// handle closes, which is how the hold outlives the mutation it protects.
func holdDesktopSingleton(name string) (func(), error) {
	if strings.TrimSpace(name) == "" {
		computed, err := DesktopSingletonName()
		if err != nil {
			return nil, fmt.Errorf("service: the per-session desktop singleton cannot be named: %w", err)
		}
		name = computed
	}
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("service: the desktop singleton %q is not a valid Windows object name: %w", name, err)
	}
	attrs, descriptor, err := desktopSingletonObjectSecurity()
	if err != nil {
		return nil, fmt.Errorf("service: secure the desktop singleton %q: %w", name, err)
	}
	handle, err := windows.CreateMutex(attrs, false, namePtr)
	runtime.KeepAlive(descriptor)
	if handle == 0 {
		if err == nil {
			err = errors.New("the operating system returned no handle")
		}
		// The object is created with a descriptor this process owns, so a
		// refusal to create it is a refusal to open an object that exists
		// and belongs to another identity: the hold is refused before the
		// mutation, with the reason and the step that ends it.
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, fmt.Errorf(
				"service: the desktop singleton %q exists but this Windows identity cannot access it: another HFDesk process under a different Windows account created it; end that process and try again%w",
				name, ErrSingletonAccessDenied)
		}
		return nil, fmt.Errorf("service: hold the desktop singleton %q during the service mutation: %w", name, err)
	}
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("service: hold the desktop singleton %q during the service mutation: %w", name, err)
	}
	return func() {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
			handle = 0
		}
	}, nil
}

// desktopSingletonObjectSecurity returns the security attributes of the
// per-session desktop singleton object.
//
// The descriptor is the one the desktop host creates its own instance lock
// with: the user of the process with full mutex access, the administrators
// and the system with read control, protected against inherited entries. Any
// creator of this object has to build the same descriptor, or a hold of one
// identity would lock the desktop host of another one out of its own object.
func desktopSingletonObjectSecurity() (*windows.SecurityAttributes, *windows.SECURITY_DESCRIPTOR, error) {
	sid, err := processUserSID()
	if err != nil {
		return nil, nil, err
	}
	sddl := fmt.Sprintf("D:P(A;;0x%08X;;;%s)(A;;0x%08X;;;BA)(A;;0x%08X;;;SY)",
		uint32(windows.MUTEX_ALL_ACCESS), sid, uint32(windows.READ_CONTROL), uint32(windows.READ_CONTROL))
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, nil, fmt.Errorf("build the desktop singleton descriptor: %w", err)
	}
	attrs := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	return attrs, descriptor, nil
}

// defaultServiceUser returns the default account a machine-wide instance
// runs as on Windows.
//
// Windows has no account to name: the instance runs as LocalService, a
// built-in least-privilege service account of the service control manager.
// The value is therefore empty, and the platform manager uses the built-in
// account instead of a name of its own.
func defaultServiceUser() string {
	return ""
}

// defaultScope returns the default installation scope on Windows.
//
// The service control manager registers machine-wide services only, so the
// system scope is the default here.
func defaultScope() Scope {
	return ScopeSystem
}

// supportsUserScope reports that a per-user installation does not exist on
// Windows.
//
// The service control manager registers machine-wide services only, so the
// per-user scope is not available.
func supportsUserScope() bool {
	return false
}

// supportsNamedServiceAccount reports that the account of a machine-wide
// instance cannot be named on Windows.
//
// The instance runs as LocalService, a built-in least-privilege service
// account of the service control manager, which has no name of its own.
func supportsNamedServiceAccount() bool {
	return false
}
