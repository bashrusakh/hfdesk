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

// init replaces the liveness check of the shared file with the check of
// Windows: signal 0 is not supported here, while os.FindProcess OPENS the
// process on Windows, so a successful open is the proof that the claim's
// process still runs. An access denial means the process exists but this
// token may not open it: an unverifiable claim stays live and fails closed,
// while a pid the operating system does not know is the stale claim of a
// desktop that is gone.
func init() {
	claimProcessAlive = claimProcessAliveByOpenProcess
}

// claimProcessAliveByOpenProcess is the liveness check of a published claim
// on Windows.
func claimProcessAliveByOpenProcess(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	_ = process.Release()
	return true
}

// resolveDesktopSingletonName returns the object name to operate on: the
// given name, or the name of the session of this process for an unnamed
// caller.
func resolveDesktopSingletonName(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		computed, err := DesktopSingletonName()
		if err != nil {
			return "", fmt.Errorf("the per-session desktop singleton cannot be named: %w", err)
		}
		return computed, nil
	}
	return name, nil
}

// desktopSingletonObjectPointer encodes the name of the singleton object.
func desktopSingletonObjectPointer(name string) (*uint16, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("the desktop singleton %q is not a valid Windows object name: %w", name, err)
	}
	return namePtr, nil
}

// openDesktopSingletonForMutation opens the per-session desktop singleton:
// it creates the object when it does not exist and opens the existing one
// otherwise, and reports whether it already existed — the fact that proves
// another process of this session held the object, because the kernel object
// dies with the last handle of its holders.
//
// The object is created with the descriptor of the desktop host's own
// instance lock — the user of the process with full mutex access, the
// administrators and the system with read control — and the descriptor of an
// existing object is never replaced.
//
// A create request that Windows denies did so because the object exists and
// belongs to a token outside the descriptor's full-access entry (for example
// a helper elevated under another administrator account): the existing
// object is then opened with the read-control access the descriptor grants
// to the administrators. Any open handle keeps the object alive, which is
// all a hold needs, and this path never waits on the object. Only when even
// that open fails is the access denial returned to the caller, and a denied
// request reports the object as pre-existing either way.
func openDesktopSingletonForMutation(name string, namePtr *uint16) (windows.Handle, bool, error) {
	attrs, descriptor, err := desktopSingletonObjectSecurity()
	if err != nil {
		return 0, false, fmt.Errorf("secure the desktop singleton %q: %w", name, err)
	}
	handle, err := windows.CreateMutex(attrs, false, namePtr)
	runtime.KeepAlive(descriptor)
	if handle == 0 {
		if err == nil {
			err = errors.New("the operating system returned no handle")
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			limited, openErr := windows.OpenMutex(windows.READ_CONTROL, false, namePtr)
			if openErr == nil && limited != 0 {
				return limited, true, nil
			}
			return 0, true, err
		}
		return 0, false, err
	}
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		return 0, false, err
	}
	return handle, errors.Is(err, windows.ERROR_ALREADY_EXISTS), nil
}

// holdDesktopSingleton holds the per-session desktop singleton object of
// HoldDesktopSingleton for the calling process.
//
// The object is the desktop host's instance lock, so a launch of the desktop
// app of the same identity finds the object and reports the running
// instance; ERROR_ALREADY_EXISTS still returns a live handle to the existing
// object, and that handle is exactly the hold this path needs: the operating
// system keeps the object until the last handle closes, which is how the
// hold outlives the mutation it protects. An object owned by another
// Windows identity is held through its read-control open (see
// openDesktopSingletonForMutation); an object this identity cannot open at
// all ends the hold with ErrSingletonAccessDenied, before the mutation.
func holdDesktopSingleton(name string) (func(), error) {
	resolved, err := resolveDesktopSingletonName(name)
	if err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}
	namePtr, err := desktopSingletonObjectPointer(resolved)
	if err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}
	handle, _, err := openDesktopSingletonForMutation(resolved, namePtr)
	if handle == 0 {
		if err == nil {
			err = errors.New("the operating system returned no handle")
		}
		// The fallback open of the mutation also failed, so this identity
		// cannot touch an object that exists and belongs to another
		// identity: the hold is refused before the mutation, with the
		// reason and the step that ends it.
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, fmt.Errorf(
				"service: the desktop singleton %q exists but this Windows identity cannot access it: another HFDesk process under a different Windows account created it; end that process and try again%w",
				resolved, ErrSingletonAccessDenied)
		}
		return nil, fmt.Errorf("service: hold the desktop singleton %q during the service mutation: %w", resolved, err)
	}
	return func() {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
			handle = 0
		}
	}, nil
}

// probeDesktopSingleton is the Windows implementation behind
// ProbeDesktopSingleton: it reports whether the per-session desktop
// singleton object already existed when this process first touched it —
// the evidence that another HFDesk process of this session held the object
// at that moment.
//
// The probe opens or creates the object and keeps it through the returned
// release, so the object never disappears between the probe and a hold taken
// right after it, and no desktop launch can slip in while the caller decides.
// Callers that run a service mutation release the probe together with their
// hold.
//
// The result fails closed: whenever the probe cannot prove that the object
// did not pre-exist — an access denial, an unnameable object, any failed
// open — it reports preExisting together with the error, so a caller never
// mistakes an unproven freshness for proof that no desktop of this session
// is alive.
func probeDesktopSingleton(name string) (release func(), preExisting bool, err error) {
	resolved, err := resolveDesktopSingletonName(name)
	if err != nil {
		return nil, true, fmt.Errorf("service: %w", err)
	}
	namePtr, err := desktopSingletonObjectPointer(resolved)
	if err != nil {
		return nil, true, fmt.Errorf("service: %w", err)
	}
	handle, preExisting, err := openDesktopSingletonForMutation(resolved, namePtr)
	if handle != 0 {
		release = func() {
			if handle != 0 {
				_ = windows.CloseHandle(handle)
				handle = 0
			}
		}
	}
	if err != nil {
		return release, true, err
	}
	return release, preExisting, nil
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
