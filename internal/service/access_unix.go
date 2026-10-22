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
	"strings"
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
//
// The check judges the real location of a path. The kernel follows the
// symbolic links of a path before it uses it, so a directory that is reached
// through a link is used by its real location and not by the path of the
// link: the check resolves the path and asks about the resolved directory
// and the resolved ancestors of it. A check that asked about the path as it
// was written would accept a directory whose real location the account never
// reaches.
//
// A directory the installation creates itself does not need the check: the
// directory inside the state directory of the instance that does not exist
// yet is one of them. The state directory belongs to the account of the
// instance, and the service creates the directory below it. A directory that
// exists already is a different case: it belongs to whoever owns it, so it
// is checked against the account like every other named directory.

// maxResolvedLinks is the number of symbolic links the resolution of a path
// follows before it gives up.
//
// The kernel refuses a path with a link loop after a few links, so the
// resolution refuses it as well instead of following it forever.
const maxResolvedLinks = 40

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
// The check judges the real location of the path: the kernel follows the
// symbolic links of a path before it uses it, so the permissions of the
// resolved directory and the traversal of the resolved ancestors of it are
// the ones that decide whether the account can use the directory.
//
// The account has to create entries in the directory: it needs the write and
// the execute permission on it. A directory that does not exist yet is
// checked through its nearest existing ancestor: the account has to be able
// to create the directory there.
//
// Every directory above the target has to be traversable by the account: it
// needs the execute permission on it, otherwise the account never reaches
// the target. The directories of the path as it was written have to be
// traversable as well: the kernel resolves the path part by part, so it
// passes through them to reach the symbolic links of it.
//
// The answer is a plain yes or no. An error means that the directory could
// not be read at all, which is a refusal as well.
func accountCanUseDir(path string, account accessAccount) (bool, error) {
	// The real location of the path is the one the kernel uses, so the
	// check judges it and not the path as it was written.
	resolved, err := resolvePath(path)
	if err != nil {
		return false, err
	}

	target := resolved
	info, err := os.Stat(target)
	switch {
	case err == nil:
		if !info.IsDir() {
			return false, fmt.Errorf("it exists and is not a directory")
		}
	case errors.Is(err, fs.ErrNotExist):
		ancestor, err := nearestExistingDir(target)
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

	// Every directory above the real location of the target has to be
	// traversable, otherwise the account never reaches the target.
	ok, err := accountCanTraverse(filepath.Dir(target), account)
	if err != nil || !ok {
		return ok, err
	}

	// The path as it was written has to be traversable as well: the
	// kernel resolves it part by part, so it passes through the
	// directories of the path to reach the symbolic links in it.
	written, err := nearestExistingDir(path)
	if err != nil {
		return false, err
	}
	return accountCanTraverse(written, account)
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

// resolvePath returns the real location of the path.
//
// The kernel follows the symbolic links of a path before it uses it, so the
// real location of the path is the one that decides whether the account can
// use it. The resolution follows the links of the path part by part: a link
// is replaced by its target and the parts that follow it stay behind it.
//
// The resolution stops at the first part of the path that does not exist:
// the parts that are missing are appended to the real location of the path,
// so the caller can tell a directory that is there from one that has to be
// created.
func resolvePath(path string) (string, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		// An installation only ever names absolute paths, but a relative
		// one is resolved against the working directory of the process, so
		// the answer is always about a real location.
		absolute, err := filepath.Abs(clean)
		if err != nil {
			return "", err
		}
		clean = absolute
	}

	resolved := string(filepath.Separator)
	pending := pathComponents(clean)
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		candidate := filepath.Join(resolved, part)
		info, err := os.Lstat(candidate)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
			// The path does not exist from here on: the parts that are
			// missing keep the location they were written with.
			return filepath.Join(append([]string{resolved}, pending...)...), nil
		}
		pending = pending[1:]
		if info.Mode()&fs.ModeSymlink == 0 {
			resolved = candidate
			continue
		}
		// The part is a symbolic link: the link is replaced by its
		// target, and the parts that follow the link stay behind it. A
		// relative target is resolved against the directory of the link.
		links++
		if links > maxResolvedLinks {
			return "", fmt.Errorf("%q holds a symbolic link loop", path)
		}
		target, err := os.Readlink(candidate)
		if err != nil {
			return "", err
		}
		target = filepath.Clean(target)
		if filepath.IsAbs(target) {
			resolved = string(filepath.Separator)
		}
		pending = append(pathComponents(target), pending...)
	}
	return resolved, nil
}

// pathComponents splits a path into the parts below its root.
//
// The empty parts and the "." part name the directory itself and not a part
// below it, so they are left out.
func pathComponents(path string) []string {
	parts := strings.Split(path, string(filepath.Separator))
	filtered := parts[:0]
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		filtered = append(filtered, part)
	}
	return filtered
}

// pathMissingInsideRealDir reports whether the real location of the path lies
// inside the real location of the directory and the path does not exist yet.
//
// The kernel follows the symbolic links of a path before it uses it, so the
// real location of a path is the one that decides whether the account can use
// it. A link inside the directory that points out of it leaves the directory,
// so a path that is written inside the directory can still live outside of
// it.
//
// A path that does not exist yet is resolved through its nearest existing
// ancestor: the parts that are missing keep the location they were written
// with, so a directory that is going to be created is compared with the
// directory that holds it.
//
// The answer is false when the location of a path cannot be resolved: an
// unknown location is never a location inside the other path, so the caller
// checks the path against the account in that case.
//
// The directory has to be missing as well. A directory that exists already
// belongs to whoever owns it: the installation never walks the tree of the
// state directory and never grants a directory of it to the account, so an
// existing directory is checked against the account like every other named
// directory.
func pathMissingInsideRealDir(dir, path string) bool {
	if dir == "" || path == "" {
		return false
	}
	realDir, err := resolvePath(dir)
	if err != nil {
		return false
	}
	realPath, err := resolvePath(path)
	if err != nil {
		return false
	}
	if !pathContains(realDir, realPath) {
		return false
	}
	// The real location decides whether the directory is there: the
	// kernel follows the symbolic links of a path before it uses it, so a
	// path that points at a directory that is not there names a directory
	// that does not exist yet.
	_, err = os.Stat(realPath)
	return errors.Is(err, fs.ErrNotExist)
}

// pathContains reports whether the path lies inside the directory.
//
// The directory lies inside itself, so a path that names the directory is
// contained by it. A sibling that shares the prefix of the directory
// ("/stateful" against "/state") is not inside it.
func pathContains(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
