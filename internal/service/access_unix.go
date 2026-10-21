// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package service

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// The access check of a directory the service account of a machine-wide
// instance has to use.
//
// The check is the effective one: it asks whether the account can really
// create entries in the directory and really reach it, not whether it owns
// the directory. A directory the account owns with the mode 0500 is not
// writable at all, because the owner has no write permission in it, and a
// directory below a parent without the execute permission cannot be reached
// at all, not even by its owner.
//
// The check lives in one file for the Unix platforms, so Linux and macOS
// cannot drift apart. The identity of the account stays with the platform
// managers: how the account of a machine-wide instance is resolved is a
// fact of the platform.

// accessAccount is the identity an access check runs for.
type accessAccount struct {
	// UID is the numeric user id of the account.
	UID int
	// GID is the numeric id of the primary group of the account.
	GID int
	// Groups holds the numeric ids of the groups the account belongs to.
	// The primary group of the account is part of it.
	Groups []int
}

// accountCanUseDir reports whether the account can use the directory path.
//
// The account has to create entries in the directory: it needs the write and
// the execute permission on it. A directory that does not exist yet is
// checked through its nearest existing ancestor: the account has to be able
// to create the directory there.
//
// Every directory above the target has to be traversable by the account: it
// needs the execute permission on it, otherwise the account never reaches
// the target.
//
// The answer is a plain yes or no. An error means that the directory could
// not be read at all, which is a refusal as well.
func accountCanUseDir(path string, account accessAccount) (bool, error) {
	target := path
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if !info.IsDir() {
			return false, fmt.Errorf("it exists and is not a directory")
		}
	case errors.Is(err, fs.ErrNotExist):
		ancestor, err := nearestExistingDir(path)
		if err != nil {
			return false, err
		}
		target = ancestor
		info, err = os.Stat(target)
		if err != nil {
			return false, err
		}
	default:
		return false, err
	}

	// The account has to create entries in the directory: it needs the
	// write and the execute permission on it.
	if effectivePermission(info, account)&0o300 != 0o300 {
		return false, nil
	}

	// Every directory above the target has to be traversable, otherwise
	// the account never reaches the target.
	return accountCanTraverse(filepath.Dir(target), account)
}

// accountCanTraverse reports whether the account can pass through the
// directory and through every directory above it.
//
// A directory without the execute permission cannot be entered, so a
// directory below it is out of reach of the account.
func accountCanTraverse(dir string, account accessAccount) (bool, error) {
	current := filepath.Clean(dir)
	for {
		info, err := os.Stat(current)
		if err != nil {
			return false, err
		}
		if !info.IsDir() {
			return false, fmt.Errorf("%q is not a directory", current)
		}
		if effectivePermission(info, account)&0o100 == 0 {
			return false, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return true, nil
		}
		current = parent
	}
}

// effectivePermission returns the permission bits that apply to the account
// for the file the information describes.
//
// The resolution is the one of POSIX: the owner bits apply to the owner of
// the file, the group bits to a member of the group of the file and the
// other bits to everybody else.
//
// The bits are shifted into the position of the owner, so a caller tests
// them like the mode of a file: the write permission is 0o200 and the
// execute permission is 0o100, whatever the class of the permission is.
func effectivePermission(info fs.FileInfo, account accessAccount) os.FileMode {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// The file system does not report an owner: nobody is known to
		// have a permission on the file.
		return 0
	}
	mode := info.Mode().Perm()
	switch {
	case int(stat.Uid) == account.UID:
		return mode & 0o700
	case accountInGroup(int(stat.Gid), account):
		return (mode & 0o070) << 3
	default:
		return (mode & 0o007) << 6
	}
}

// accountInGroup reports whether the account belongs to the group.
//
// The primary group of the account and the supplementary groups of the
// account count: a directory the account reaches through a supplementary
// group is as usable as one it reaches through its primary group.
func accountInGroup(gid int, account accessAccount) bool {
	if account.GID == gid {
		return true
	}
	for _, group := range account.Groups {
		if group == gid {
			return true
		}
	}
	return false
}

// nearestExistingDir returns the nearest existing ancestor of the path.
//
// The ancestor has to be a directory: a path below a file can never be a
// directory.
func nearestExistingDir(path string) (string, error) {
	current := filepath.Dir(filepath.Clean(path))
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%q is not a directory", current)
			}
			return current, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing parent directory of %q was found", path)
		}
		current = parent
	}
}
