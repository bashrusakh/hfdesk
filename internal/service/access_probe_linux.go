// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"errors"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

var errAccessWorkingDirectoryNotReady = errors.New("working directory is not available for an access check")

// internalAccessProbeCommand is handled only by the HFDesk CLI's private
// access-check path; it never executes the candidate program.
const internalAccessProbeCommand = "--hfdesk-internal-access-check"

const (
	accessProbeDeniedCode  = 10
	accessProbeUnknownCode = 11
	accessProbeAllowedCode = 0
)

// RunInternalAccessProbe handles the private access-check command. handled is
// false for every ordinary invocation. The exit code distinguishes a definite
// access denial from a probe the host could not perform.
func RunInternalAccessProbe(args []string) (exitCode int, handled bool) {
	if len(args) == 0 || args[0] != internalAccessProbeCommand {
		return 0, false
	}
	if unix.Getuid() != unix.Geteuid() || unix.Getgid() != unix.Getegid() || accessProbeHasCapabilities() {
		return accessProbeUnknownCode, true
	}
	if len(args) != 4 {
		return accessProbeUnknownCode, true
	}
	mode, err := strconv.ParseUint(args[2], 10, 32)
	if err != nil || mode == 0 || mode&^uint64(unix.R_OK|unix.X_OK) != 0 {
		return accessProbeUnknownCode, true
	}

	// The helper runs with real and effective IDs set to the service identity,
	// so the original faccessat check uses that identity and remains available
	// on kernels predating faccessat2.
	err = faccessatExecutablePath(args[1], uint32(mode), args[3])
	switch {
	case err == nil:
		return accessProbeAllowedCode, true
	case err == unix.EACCES || err == unix.ENOENT || err == unix.ENOTDIR || err == unix.ELOOP:
		return accessProbeDeniedCode, true
	default:
		return accessProbeUnknownCode, true
	}
}

// faccessatExecutablePath checks a path exactly as the kernel resolves it.
// A relative interpreter is anchored at systemd's WorkingDirectory by opening
// that directory and passing the original spelling to faccessat; filepath.Clean
// would incorrectly collapse symlink/.. components.
func faccessatExecutablePath(path string, mode uint32, workingDirectory string) error {
	dirfd := unix.AT_FDCWD
	if !filepath.IsAbs(path) {
		if workingDirectory == "" || !filepath.IsAbs(workingDirectory) {
			return errAccessWorkingDirectoryNotReady
		}
		if err := unix.Faccessat(unix.AT_FDCWD, workingDirectory, unix.X_OK, 0); err != nil {
			if errors.Is(err, unix.ENOENT) {
				return errAccessWorkingDirectoryNotReady
			}
			return err
		}
		opened, err := unix.Open(workingDirectory, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return errAccessWorkingDirectoryNotReady
			}
			return err
		}
		defer unix.Close(opened)
		if err := unix.Faccessat(opened, ".", unix.X_OK, 0); err != nil {
			return err
		}
		dirfd = opened
	}
	return unix.Faccessat(dirfd, path, mode, 0)
}

func accessProbeHasCapabilities() bool {
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capget(&header, &data[0]); err != nil {
		return true
	}
	for _, word := range data {
		if word.Effective != 0 || word.Permitted != 0 {
			return true
		}
	}
	return false
}
