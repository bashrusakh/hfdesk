// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package service

// scmExecutableSupport is what the host has established about whether Windows
// can execute a PE machine type.
type scmExecutableSupport int

const (
	scmExecutableSupportUnknown scmExecutableSupport = iota
	scmExecutableSupported
	scmExecutableUnsupported
)

// scmExecutableSupportForHost classifies a candidate against the native host
// machine and the operating system's guest-machine query. The architecture of
// the installer process is deliberately not an input: Windows may run that
// process under emulation while supporting a different native or guest image.
func scmExecutableSupportForHost(candidate, native uint16, guestSupported func(uint16) (bool, error)) scmExecutableSupport {
	if candidate == 0 || native == 0 {
		return scmExecutableSupportUnknown
	}
	if candidate == native {
		return scmExecutableSupported
	}
	if guestSupported == nil {
		return scmExecutableSupportUnknown
	}
	supported, err := guestSupported(candidate)
	if err != nil {
		return scmExecutableSupportUnknown
	}
	if supported {
		return scmExecutableSupported
	}
	return scmExecutableUnsupported
}
