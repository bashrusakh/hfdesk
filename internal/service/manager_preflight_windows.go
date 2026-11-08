// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// preflight rejects an installation with an unusable executable, a PE machine
// the host is known not to support, an invalid bind address or a directory the
// account of the instance is proven unable to use.
//
// The preflight runs before the registration is written, so a rejected
// installation never leaves a service behind that fails later with an
// opaque access error.
func (m *scmManager) preflight(opts InstallOptions, pathAccess ...scmPathAccess) error {
	if !filepath.IsAbs(opts.Executable) {
		return fmt.Errorf("service: executable %q must be an absolute path", opts.Executable)
	}
	info, err := os.Stat(opts.Executable)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("service: executable %q is not usable: %w", opts.Executable, err)
		}
		log.Printf("service: executable metadata for %q cannot be inspected (%v); the installer did not classify the executable as invalid", opts.Executable, err)
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("service: executable %q is not a regular file", opts.Executable)
	}
	if _, _, err := ParseListen(opts.Listen); err != nil {
		return err
	}
	return preflightDataPaths(opts, pathAccess...)
}

// scmExecutableAccess is what the access check of the registered program proved.
type scmExecutableAccess int

const (
	scmExecutableAccessInconclusive scmExecutableAccess = iota
	scmExecutableAccessAllowed
	scmExecutableAccessDenied
)

var scmCheckExecutableAccess = scmExecutableAccessOf

// preflightExecutableAccess refuses a program the account cannot execute, and
// a parent it cannot traverse. The program image is checked for execute only:
// Windows supports an image the account runs without reading it, so read data
// is not demanded of the program. An unreadable descriptor is not a denial:
// the installation reports that it could not check.
func preflightExecutableAccess(executable string, pathAccess ...scmPathAccess) error {
	var effective scmPathAccess
	if len(pathAccess) != 0 {
		effective = pathAccess[0]
	}
	access, reason := scmCheckExecutableAccess(executable, effective)
	switch access {
	case scmExecutableAccessDenied:
		return fmt.Errorf("service: executable %q is not usable by the account the instance runs as (%s): %s: grant the account execute access to the program, or choose a program the account can use", executable, scmServiceIdentityName, reason)
	case scmExecutableAccessInconclusive:
		log.Printf("service: effective access to executable %q for %s is inconclusive (%s)", executable, scmServiceIdentityName, reason)
	}
	return nil
}

func scmExecutableAccessOf(executable string, access scmPathAccess) (scmExecutableAccess, string) {
	// Only the execute right is required of the program image. GENERIC_READ
	// would demand the read data that an execute-only program legitimately
	// withholds, and GENERIC_EXECUTE would additionally demand read-control,
	// read-attributes and synchronize rights that a bare "Execute file"
	// access control entry does not carry. Every access control entry form
	// that grants execution grants FILE_EXECUTE, so a program the account
	// cannot execute is still refused by this mask.
	return scmCheckPathAccess(executable, windows.FILE_EXECUTE, access)
}

// scmParentDirs returns the directories above a path, the immediate parent first.
func scmParentDirs(path string) []string {
	var dirs []string
	current := filepath.Dir(filepath.Clean(path))
	for {
		dirs = append(dirs, current)
		parent := filepath.Dir(current)
		if parent == current {
			return dirs
		}
		current = parent
	}
}

const (
	scmServiceIdentitySID  = "S-1-5-19"
	scmServiceIdentityName = `NT AUTHORITY\LocalService`
)

// preflightDataPaths checks the directories an installation reuses: the
// cache root, the hub directory and the local output directory.
//
// The directories do not belong to the instance: they can hold the models
// and the cache of an interactive run of the user. Their access control
// list is only read, never changed, and never changed recursively.
//
// A directory that does not exist yet and that really lives inside the
// state directory of the instance belongs to the instance: the state
// directory belongs to the account the instance runs as, and the instance
// creates the cache tree it needs inside it. Such a directory is neither
// checked nor created.
//
// The exemption covers a directory that does not exist yet only. A directory
// that exists already carries the access control list of whoever owns it,
// and owning the state directory grants no right on a directory inside it:
// a child of the state root that the account of the instance cannot write
// in is a directory the instance cannot work with. Such a directory is
// checked like every other named directory, wherever it lives.
//
// The real location of the two paths decides that: the platform follows the
// links of a path before it uses it, so a directory that is reached through
// a link lives where its target lives and not where the link is. A link
// that leaves the state directory is a directory outside of it, and it is
// checked like every other named directory.
//
// A directory the user named is outside of the instance: the account of the
// instance has to be able to work with it. A directory that exists already is
// checked against the account of the instance, wherever it lives.
//
// A directory that does not exist yet is checked through its nearest
// existing ancestor: the account has to be able to create the directory
// there, because the instance creates it when it runs, as the account the
// instance runs as.
//
// The installation never creates a directory the user named: it belongs to
// whoever named it, and the installation only reads the access control list
// of the ancestor it judges.
func preflightDataPaths(opts InstallOptions, pathAccess ...scmPathAccess) error {
	var access scmPathAccess
	if len(pathAccess) != 0 {
		access = pathAccess[0]
	}
	for _, dir := range []struct {
		name string
		path string
	}{
		{"cache dir", opts.CacheDir},
		{"hub dir", opts.HubDir},
		{"local dir", opts.LocalDir},
	} {
		if dir.path == "" {
			continue
		}
		if pathMissing(dir.path) {
			// The directory does not exist yet. The instance creates
			// the cache tree it needs inside the state directory it
			// owns, so such a directory is neither checked nor
			// created.
			inside, err := scmPathInsideStateRoot(opts.Paths.StateDir, dir.path)
			if err != nil {
				// The real location of the path cannot be read: the
				// exemption is not granted, and the path is checked
				// like every other named directory. The check below is
				// the safe answer, and the reason is logged.
				log.Printf("service: the real location of the %s %q cannot be read (%v): the path is checked like every other named directory", dir.name, dir.path, err)
				inside = false
			}
			if inside {
				// The default of the scope lives inside the state
				// directory of the instance, and the state directory
				// belongs to the account of the instance.
				continue
			}
		}
		// The directory is checked against the account of the instance,
		// wherever it lives: a directory that exists already is checked
		// against its own access control list, and a directory that does
		// not exist yet is checked through its nearest existing ancestor.
		// The check reads the access control list of the real location of
		// the directory.
		if err := preflightDataPath(dir.name, dir.path, access); err != nil {
			return err
		}
	}
	return nil
}

// scmPathInsideStateRoot reports whether a named directory really lives
// inside the state root of the instance.
//
// The decision is made on the real location of the two paths: the platform
// follows the links of a path before it uses it, so a directory that is
// reached through a link lives where its target lives and not where the link
// is.
//
// The decision asks nothing about the account of the instance: the exemption
// is a fact about the two paths. A tree that does not exist yet is judged by
// the place it will live in, and the account of the instance is never asked
// about a tree that is not there.
func scmPathInsideStateRoot(stateRoot, path string) (bool, error) {
	if stateRoot == "" {
		return false, nil
	}
	root, err := scmResolvePath(stateRoot)
	if err != nil {
		return false, err
	}
	target, err := scmResolvePath(path)
	if err != nil {
		return false, err
	}
	return hfdownloader.PathInside(root, target), nil
}

// scmResolvePath returns the real location of a path.
//
// The platform follows the symbolic links and the reparse points of a path
// before it uses it, so the real location of a path is the one that decides
// whether the account of the instance can work with it.
//
// The resolution follows the links of the path as far as the path exists:
// the parts that are missing keep the location they were written with, so a
// directory that is created later is judged by the place it will live in.
func scmResolvePath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	// The path does not exist: the deepest ancestor of it that exists is
	// resolved, and the parts that are missing are appended to it.
	existing, missing, err := scmSplitExistingPath(path)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{resolved}, missing...)...), nil
}

// scmSplitExistingPath splits a path into the deepest ancestor of it that
// exists and the parts that are missing below it.
//
// The root of a volume is the end of the walk: a path below a volume that
// does not exist cannot be resolved.
func scmSplitExistingPath(path string) (string, []string, error) {
	var missing []string
	current := filepath.Clean(path)
	for {
		if _, err := os.Lstat(current); err == nil {
			return current, missing, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", nil, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", nil, fmt.Errorf("the path %q has no existing ancestor", path)
		}
		missing = append([]string{filepath.Base(current)}, missing...)
		current = parent
	}
}

// preflightDataPath checks that the account of the instance can work with
// the directory the installation reuses.
//
// A directory that exists already is checked against the account of the
// instance: the access control list of the directory decides whether the
// account can read it and write in it.
//
// A directory that does not exist yet is checked through its nearest
// existing ancestor: the instance creates the directory when it runs, as the
// account the instance runs as, so the account has to be able to create the
// directory there.
//
// The installation never creates a directory the user named: it belongs to
// whoever named it, and the installation only reads the access control list
// of the path it checks.
func preflightDataPath(name, path string, pathAccess ...scmPathAccess) error {
	var access scmPathAccess
	if len(pathAccess) != 0 {
		access = pathAccess[0]
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("service: %s %q must be an absolute path", name, path)
	}

	info, err := os.Stat(path)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("service: %s %q is not a directory: the instance needs a directory there", name, path)
		}
		return checkServiceAccess(name, path, access)
	case !errors.Is(err, fs.ErrNotExist):
		log.Printf("service: metadata for %s %q cannot be inspected (%v); path usability is inconclusive", name, path, err)
		return nil
	}

	// The directory does not exist yet. The instance creates it when it
	// runs, as the account the instance runs as, so the installation
	// judges it through its nearest existing ancestor: the account has to
	// be able to create the directory there.
	//
	// The installation never creates the directory: the directory and the
	// access rights of it belong to whoever named it, and an installation
	// that created one would take a tree of models over without being
	// asked to.
	return checkServiceCreateAccess(name, path, access)
}

// checkServiceCreateAccess checks that the account of the instance can
// create the directory the installation reuses.
//
// The directory does not exist yet: the instance creates it when it runs, as
// the account the instance runs as. The check therefore judges the nearest
// existing ancestor of the directory: the account has to be able to create
// entries in it, which is the access the instance needs on every directory
// it works with.
//
// The access control list of the ancestor is only read: the installation
// never creates the directory the user named and never applies a descriptor
// to a path it reuses.
//
// The path and its parents are checked with the effective token of the running
// LocalService instance, including the original and every observed
// reparse-resolved segment. Windows' AccessCheck applies DACL order; traversal
// checks are skipped only when that token reports SeChangeNotifyPrivilege enabled.
func checkServiceCreateAccess(label, path string, pathAccess ...scmPathAccess) error {
	var access scmPathAccess
	if len(pathAccess) != 0 {
		access = pathAccess[0]
	}
	sourceAncestor, ancestor, err := scmNearestExistingDirRoute(path)
	if err != nil {
		if errors.Is(err, errSCMNotDirectory) {
			return fmt.Errorf("service: the %s %q cannot be created because its nearest existing ancestor is not a directory: %w", label, path, err)
		}
		log.Printf("service: access to the nearest existing ancestor of %q for %s is inconclusive (%v)", path, scmServiceIdentityName, err)
		return nil
	}
	decision, reason := scmCheckPathAccessFrom(sourceAncestor, ancestor, windows.GENERIC_READ|windows.GENERIC_WRITE, access)
	switch decision {
	case scmExecutableAccessDenied:
		// The message names the directory, the account and the access
		// that is missing: the user has to grant the access to the
		// ancestor, and the name of the ancestor is what makes the
		// guidance actionable.
		return fmt.Errorf(
			"service: the %s %q does not exist and the account %s cannot create it: %s: the nearest existing ancestor %q has to grant the account read and write access: the installation never creates a directory the user named, the instance creates it when it runs, as the account the instance runs as: grant the account read and write access to the ancestor directory, or install the instance without this path",
			label, path, scmServiceIdentityName, reason, ancestor,
		)
	}
	if decision == scmExecutableAccessInconclusive {
		log.Printf("service: effective access to the nearest existing ancestor %q for %s is inconclusive (%s)", ancestor, scmServiceIdentityName, reason)
	}
	return nil
}

// scmNearestExistingDir returns the nearest existing ancestor directory of
// the path.
//
// The ancestor is the directory the instance creates the directory in: it is
// the deepest directory that exists on the way to the path. A path below a
// file has no such ancestor: a file can never hold a directory.
//
// The path is judged by its real location: the platform follows the links of
// a path before it uses it, so a directory that is reached through a link is
// created in the directory its target lives in.
func scmNearestExistingDir(path string) (string, error) {
	resolved, err := scmResolvePath(path)
	if err != nil {
		// The real location of the path cannot be read: the path as it
		// was written is the one that is judged.
		resolved = path
	}
	current := filepath.Clean(resolved)
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%q is not a directory: %w", current, errSCMNotDirectory)
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

var errSCMNotDirectory = errors.New("not a directory")

func scmNearestExistingDirRoute(path string) (sourceAncestor, resolvedAncestor string, err error) {
	resolvedAncestor, err = scmNearestExistingDir(path)
	if err != nil {
		return "", "", err
	}
	sourceAncestor, _, err = scmSplitExistingPath(path)
	if err != nil {
		return "", "", err
	}
	return sourceAncestor, resolvedAncestor, nil
}

// checkServiceAccess uses Windows AccessCheck with the effective service token
// to decide whether the account can work with a path.
//
// The access control list is only read: the descriptor of the instance is
// never applied to a path the installation reuses.
func checkServiceAccess(label, path string, pathAccess ...scmPathAccess) error {
	var access scmPathAccess
	if len(pathAccess) != 0 {
		access = pathAccess[0]
	}
	decision, reason := scmCheckPathAccess(path, windows.GENERIC_READ|windows.GENERIC_WRITE, access)
	switch decision {
	case scmExecutableAccessDenied:
		return fmt.Errorf("service: the %s %q is not usable by the account the instance runs as (%s): %s: grant the account read and write access to the path, or install the instance without this path", label, path, scmServiceIdentityName, reason)
	case scmExecutableAccessInconclusive:
		log.Printf("service: effective access to %s %q for %s is inconclusive (%s)", label, path, scmServiceIdentityName, reason)
	}
	return nil
}

// restrictInstanceState restricts the access to the state of the instance.
//
// The state of a machine-wide instance belongs to the machine: SYSTEM, the
// administrators of the machine and the account the instance runs as may
// work with it, nobody else.
//
// The restrictive descriptor is applied to the paths the installation
// created: the directory of the config file, the state directory, the log
// directory and the config file itself. The descriptor is applied to the
// path itself and never recursively.
//
// A path that existed before the installation belongs to whoever owns it:
// its descriptor is only read, to check that the account of the instance can
// work with it. The check of every such path happens before the first
// descriptor is changed, so a refused installation leaves every descriptor
// of the machine as it was.
//
// The model and the cache trees of an installation are never re-owned and
// never re-ACLed: they can hold gigabytes of data that the user may reuse
// from an interactive run, and a recursive change of the access rights
// would take the data away from the user who owns it.
func restrictInstanceState(opts InstallOptions, snapshot scmInstallSnapshot, pathAccess ...scmPathAccess) error {
	var access scmPathAccess
	if len(pathAccess) != 0 {
		access = pathAccess[0]
	}
	paths := instanceStatePaths(opts, snapshot)
	// The paths that existed before the installation are checked first:
	// the account of the instance has to be able to work with them
	// before a single descriptor is changed.
	for _, path := range paths {
		if path.created {
			continue
		}
		if err := checkServiceAccess(path.name, path.path, access); err != nil {
			return err
		}
	}
	for _, path := range paths {
		if !path.created {
			continue
		}
		if err := applyRestrictiveACL(path.path); err != nil {
			return err
		}
	}
	return nil
}

// scmInstancePath is one path of the instance whose descriptor the
// installation manages.
type scmInstancePath struct {
	// name names the path in an error message.
	name string
	// path is the absolute path of the path.
	path string
	// created reports whether the installation creates the path itself.
	created bool
}

// instanceStatePaths returns the paths of the instance whose descriptor the
// installation manages: the directory of the config file, the state
// directory, the log directory and the config file itself.
//
// A path the installation creates is marked as created: the snapshot holds
// the directories an installation creates for every directory of the
// installation, and it is read before the directories are created, so the
// installation knows which of them it owns. The missing parents of such a
// directory belong to the installation as well.
//
// A path that existed before the installation is not marked: it belongs to
// whoever owns it.
//
// The config file is a file the instance writes itself: a file that existed
// before the installation is not marked, a file the installation created is.
//
// A path that is named twice is returned once: the config file can live in
// the state directory of the instance.
func instanceStatePaths(opts InstallOptions, snapshot scmInstallSnapshot) []scmInstancePath {
	var paths []scmInstancePath
	seen := make(map[string]bool, 4)
	add := func(name, path string, created bool) {
		if path == "" || path == "." || seen[path] {
			return
		}
		seen[path] = true
		paths = append(paths, scmInstancePath{name: name, path: path, created: created})
	}

	for _, dir := range []struct {
		name string
		path string
	}{
		{"config dir", filepath.Dir(opts.Paths.ConfigFile)},
		{"state dir", opts.Paths.StateDir},
		{"log dir", opts.Paths.LogDir},
	} {
		if dir.path == "" || dir.path == "." {
			continue
		}
		own := snapshot.created[dir.path]
		if len(own) == 0 {
			// The directory existed before the installation: it
			// belongs to whoever owns it.
			add(dir.name, dir.path, false)
			continue
		}
		// The installation creates the directory and the missing
		// parents of it: they belong to the installation.
		for _, path := range own {
			add(dir.name, path, true)
		}
	}

	// The config file holds the credentials of the instance. It is a
	// file the instance writes itself, so it is only a path of the
	// installation when it exists: a file that is written later inherits
	// the descriptor of its directory.
	if info, err := os.Stat(opts.Paths.ConfigFile); err == nil && info.Mode().IsRegular() {
		add("config file", opts.Paths.ConfigFile, snapshot.configFileMissing)
	}
	return paths
}

// applyRestrictiveACL applies the security descriptor of the instance to a
// path.
//
// The descriptor blocks the inheritance of the parent directory: the access
// of the directory of ProgramData must not leak into the state of the
// instance.
func applyRestrictiveACL(path string) error {
	sd, err := windows.SecurityDescriptorFromString(scmStateSDDL())
	if err != nil {
		return fmt.Errorf("service: build the security descriptor of %q: %w", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("service: read the access control list of the security descriptor: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return fmt.Errorf("service: restrict the access to %q: %w", path, err)
	}
	return nil
}

// scmStateSDDL returns the security descriptor of the state of a
// machine-wide instance.
//
// The descriptor grants the full control to SYSTEM (SY), to the
// administrators of the machine (BA) and to the account the instance runs
// as, LocalService (LS, the SID S-1-5-19). Nobody else is granted
// anything: the "Authenticated Users" group (AU) would hand the state of
// the machine to every logged-in user of the machine.
//
// The descriptor is protected ("D:P"), so the access of the parent
// directory is not inherited.
func scmStateSDDL() string {
	return "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;LS)"
}
