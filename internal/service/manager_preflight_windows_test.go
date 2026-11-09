// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/bashrusakh/hfdesk/internal/pe"
	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

func TestSCMStateSDDLGrantsOnlyTheMachineAccounts(t *testing.T) {
	sddl := scmStateSDDL()

	// SYSTEM, the administrators of the machine and the account the
	// instance runs as.
	for _, want := range []string{";;;SY)", ";;;BA)", ";;;LS)"} {
		if !strings.Contains(sddl, want) {
			t.Errorf("the descriptor %q does not grant %q", sddl, want)
		}
	}
	// Nobody else.
	for _, forbidden := range []string{"WD", "Everyone", "Users", "AU;", ";;;AU)", "IU)", "BU)", "Authenticated"} {
		if strings.Contains(sddl, forbidden) {
			t.Errorf("the descriptor %q grants access to %q", sddl, forbidden)
		}
	}
	// The inheritance of the parent directory is blocked.
	if !strings.HasPrefix(sddl, "D:P") {
		t.Errorf("the descriptor %q does not block the inheritance", sddl)
	}
}

// scmTestNamesPath reports whether a message of the manager names a path.
//
// The messages of the manager quote the path they talk about, so the
// backslashes of a Windows path are escaped in them: the path of a message
// is the quoted form of the path, not the path itself. The check looks for
// both forms, so it does not depend on the way a message renders the path.
//
// The manager judges a path by its real location: the platform follows the
// links of a path before it uses it, and it reports the name a directory has
// on the disk, so the name of a directory that is reached through a short
// name is the long name of the same directory. A message can therefore name
// the real location of a path instead of the path as it was written, and the
// check looks for that form as well.
//
// The real location of a path that does not exist cannot be read: the path as
// it was written is then the only form a message can name.
func scmTestNamesPath(message, path string) bool {
	if strings.Contains(message, path) || strings.Contains(message, strconv.Quote(path)) {
		return true
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return strings.Contains(message, resolved) || strings.Contains(message, strconv.Quote(resolved))
}

// TestSCMTestNamesPathAcceptsTheRealLocationOfAPath checks that the
// comparison of a path with a message accepts the real location of the path.
//
// The manager judges a path by its real location, and it names that location
// in its messages: the platform reports the name a directory has on the disk,
// so a directory that is reached through a short name is named by its long
// name. A comparison that accepts only the path as it was written fails on a
// machine that names the directory differently.
//
// The comparison keeps its meaning: the path as it was written is still
// accepted, and a path the message does not name is not a path the message
// names.
func TestSCMTestNamesPathAcceptsTheRealLocationOfAPath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "cache"), 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

	// The path as it was written names the directory with another case: the
	// platform reports the name the directory has on the disk.
	spelled := filepath.Join(root, "CACHE")
	resolved, err := filepath.EvalSymlinks(spelled)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q) = %v, want nil", spelled, err)
	}
	if resolved == spelled {
		t.Skipf("the platform reports the path %q as it was written", spelled)
	}

	// The message names the real location of the path.
	message := fmt.Sprintf("service: the cache dir %q does not exist", resolved)
	if !scmTestNamesPath(message, spelled) {
		t.Errorf("scmTestNamesPath(%q, %q) = false, want true: the message names the real location %q of the path", message, spelled, resolved)
	}
	// The path as it was written is still accepted.
	if !scmTestNamesPath(fmt.Sprintf("service: the cache dir %q does not exist", spelled), spelled) {
		t.Errorf("scmTestNamesPath() = false, want true: the message names the path %q", spelled)
	}
	// A path the message does not name is not a path the message names.
	other := filepath.Join(root, "other")
	if scmTestNamesPath(message, other) {
		t.Errorf("scmTestNamesPath(%q, %q) = true, want false: the message does not name the path", message, other)
	}
}

// TestPreflightDataPathRefusesAFile checks that a file where the instance
// needs a directory is refused.
func TestPreflightDataPathRefusesAFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cache")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	err := preflightDataPath("cache dir", path)
	if err == nil {
		t.Fatalf("preflightDataPath(%q) = nil, want an error", path)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the reason", path, err)
	}
	if !scmTestNamesPath(err.Error(), path) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the path", path, err)
	}
}

// scmTestNamedDir is one directory an installation can name.
type scmTestNamedDir struct {
	// name names the directory in the name of the subtest.
	name string
	// path returns the named directory of the installation.
	path func(opts *InstallOptions, root string) string
}

// scmTestNamedDirs returns the directories an installation can name: the
// cache root, the hub directory and the local output directory.
func scmTestNamedDirs() []scmTestNamedDir {
	return []scmTestNamedDir{
		{"the cache dir", func(opts *InstallOptions, root string) string {
			opts.CacheDir = filepath.Join(root, "cache")
			return opts.CacheDir
		}},
		{"the hub dir", func(opts *InstallOptions, root string) string {
			opts.HubDir = filepath.Join(root, "hub")
			return opts.HubDir
		}},
		{"the local dir", func(opts *InstallOptions, root string) string {
			opts.LocalDir = filepath.Join(root, "models")
			return opts.LocalDir
		}},
	}
}

// scmTestOptionsWithNamedDir returns the options of an installation that
// names one directory only, and the directory it names.
//
// The other directories are not part of the installation: the preflight
// checks the named directories only.
func scmTestOptionsWithNamedDir(root string, named scmTestNamedDir) (InstallOptions, string) {
	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	opts.CacheDir = ""
	opts.HubDir = ""
	opts.LocalDir = ""
	return opts, named.path(&opts, root)
}

// TestPreflightDataPathsAcceptAMissingNamedDirectory checks that a named
// directory that does not exist yet is accepted when the account of the
// instance can create it in its nearest existing ancestor.
//
// The instance creates the directory when it runs, as the account the
// instance runs as, so the installation judges the directory through the
// ancestor it would be created in: the account has to be able to create
// entries there.
//
// The installation creates nothing: the directory belongs to whoever named
// it, and the instance creates it when it runs.
func TestPreflightDataPathsAcceptAMissingNamedDirectory(t *testing.T) {
	for _, named := range scmTestNamedDirs() {
		t.Run(named.name, func(t *testing.T) {
			root := t.TempDir()
			opts, path := scmTestOptionsWithNamedDir(root, named)
			access := scmTestPathAccessFor(t, path, true)
			if err := preflightDataPaths(opts, access); err != nil {
				t.Errorf("preflightDataPaths() = %v, want nil for a directory the account of the instance can create", err)
			}
			// The installation created nothing: the instance creates the
			// directory when it runs.
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the directory %q exists (stat error: %v), want none: the instance creates it when it runs", path, err)
			}
		})
	}
}

// TestPreflightDataPathsRefuseAMissingNamedDirectory checks that a named
// directory that does not exist yet is refused when the account of the
// instance cannot create it in its nearest existing ancestor.
//
// The instance creates the directory when it runs, as the account the
// instance runs as, so the account has to be able to create entries in the
// ancestor. A named data directory belongs to whoever named it: the
// installation only reads the access control list of the ancestor and never
// creates the directory.
//
// The refusal names the directory, the account and the access that is
// missing, so the user knows what to grant.
func TestPreflightDataPathsRefuseAMissingNamedDirectory(t *testing.T) {
	for _, named := range scmTestNamedDirs() {
		t.Run(named.name, func(t *testing.T) {
			root := t.TempDir()
			opts, path := scmTestOptionsWithNamedDir(root, named)
			access := scmTestPathAccessFor(t, path, false)

			err := preflightDataPaths(opts, access)
			if err == nil {
				t.Fatalf("preflightDataPaths() = nil, want the refusal of %s", named.name)
			}
			if !scmTestNamesPath(err.Error(), path) {
				t.Errorf("preflightDataPaths() = %v, want an error that names the path %q", err, path)
			}
			if !strings.Contains(err.Error(), scmServiceIdentityName) {
				t.Errorf("preflightDataPaths() = %v, want an error that names the account of the instance", err)
			}
			if !strings.Contains(err.Error(), "read and write access") {
				t.Errorf("preflightDataPaths() = %v, want an error that names the access that is missing", err)
			}
			if !strings.Contains(err.Error(), "never creates a directory the user named") {
				t.Errorf("preflightDataPaths() = %v, want an error that tells the user that the installation creates no directory of its own", err)
			}
			if !strings.Contains(err.Error(), "grant the account read and write access to the ancestor directory") {
				t.Errorf("preflightDataPaths() = %v, want an error that tells the user what to do", err)
			}
			// The refusal is one action path: grant the access, then
			// rerun the installation — never an alternative that skips
			// the named directory.
			if !strings.Contains(err.Error(), "hfdesk service install") {
				t.Errorf("preflightDataPaths() = %v, want an error that names the rerun command", err)
			}
			if strings.Contains(err.Error(), "or install the instance") {
				t.Errorf("preflightDataPaths() = %v, want no alternative to granting the access", err)
			}
			// The refused directory was not created behind the back of
			// the user.
			if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("the refused directory %q exists: %v", path, statErr)
			}
		})
	}
}

// TestPreflightDataPathJudgesTheNearestExistingAncestor checks that a named
// directory that does not exist yet is judged through the nearest existing
// ancestor of it and not through the directory above it.
//
// The instance creates the directory when it runs, as the account the
// instance runs as: the account has to be able to create it in the
// directory that holds it, and the directory that holds it is the nearest
// existing ancestor.
func TestPreflightDataPathJudgesTheNearestExistingAncestor(t *testing.T) {
	root := t.TempDir()
	// The directory that holds the named directory does not exist yet
	// either: the nearest existing ancestor is the one below the root.
	ancestor := filepath.Join(root, "a")
	if err := os.Mkdir(ancestor, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	path := filepath.Join(ancestor, "b", "cache")
	access := scmTestPathAccessFor(t, path, true)
	if err := preflightDataPath("cache dir", path, access); err != nil {
		t.Errorf("preflightDataPath(%q) = %v, want nil for a directory the account of the instance can create in its nearest existing ancestor", path, err)
	}
	// The installation created nothing.
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the directory %q exists (stat error: %v), want none: the instance creates it when it runs", path, err)
	}
}

// TestSCMPreflightDataPathsAcceptADirectoryInsideTheStateRoot checks that a
// named directory that does not exist yet and that really lives inside the
// state root of the instance is not checked against the account of the
// instance.
//
// The installation grants the state root to that account, and the instance
// creates the cache tree it needs inside it. A directory of the state root
// that does not exist yet is therefore neither checked nor created.
//
// A directory that exists already is checked, whatever it lives in: the
// access control list of the child decides what the account of the instance
// can do there, and owning the state root grants no right on it.
func TestSCMPreflightDataPathsAcceptADirectoryInsideTheStateRoot(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "var", "lib", "hfdesk")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   stateDir,
		LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
	}
	opts.CacheDir = filepath.Join(stateDir, "cache")
	opts.HubDir = filepath.Join(stateDir, "cache", "hub")
	opts.LocalDir = ""

	access := &scmTestPathAccess{}
	if err := preflightDataPaths(opts, access); err != nil {
		t.Errorf("preflightDataPaths() = %v, want nil for a directory inside the state root", err)
	}
	if len(access.calls) != 0 {
		t.Errorf("preflightDataPaths() checked %v, want the absent state-root default to remain exempt", access.calls)
	}
	// The directory was not created: the instance creates it when it runs.
	if _, err := os.Stat(opts.CacheDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the cache directory %q exists (stat error: %v), want none: the instance creates it when it runs", opts.CacheDir, err)
	}
}

// TestSCMPreflightDataPathsRefuseAnExistingDirectoryInsideTheStateRoot
// checks that a named directory that exists inside the state root of the
// instance is checked against the account of the instance.
//
// Owning a directory grants no right on a directory inside it: the access
// control list of the child decides what the account of the instance can do
// there. A cache that was moved by hand, a cache of an earlier installation
// and a directory that was named with --cache-dir and exists already
// therefore have to be checked, even when they live inside the state root
// the installation owns.
func TestSCMPreflightDataPathsRefuseAnExistingDirectoryInsideTheStateRoot(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "var", "lib", "hfdesk")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	// The cache exists already and the account of the instance cannot
	// work with it.
	cacheDir := filepath.Join(stateDir, "cache")
	if err := os.Mkdir(cacheDir, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   stateDir,
		LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
	}
	opts.CacheDir = cacheDir
	opts.HubDir = ""
	opts.LocalDir = ""

	err := preflightDataPaths(opts, scmTestPathAccessFor(t, cacheDir, false))
	if err == nil {
		t.Fatalf("preflightDataPaths() = nil, want the refusal of an existing directory inside the state root")
	}
	if !scmTestNamesPath(err.Error(), cacheDir) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the path", err)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the account of the instance", err)
	}
	// The refusal of an existing path is one action path: grant the
	// access, then rerun the installation — never an alternative that
	// drops the named path.
	if !strings.Contains(err.Error(), "hfdesk service install") {
		t.Errorf("preflightDataPaths() = %v, want an error that names the rerun command", err)
	}
	if strings.Contains(err.Error(), "or install the instance") {
		t.Errorf("preflightDataPaths() = %v, want no alternative to granting the access", err)
	}
}

// TestSCMPreflightDataPathsCheckAnExistingDirectoryOutsideTheStateRoot
// checks that a named directory that exists outside the state root of the
// instance is checked against the account of the instance, the way it has
// always been.
func TestSCMPreflightDataPathsCheckAnExistingDirectoryOutsideTheStateRoot(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "var", "lib", "hfdesk")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}

	// A directory outside of the state root the account of the instance
	// can work with.
	usable := filepath.Join(root, "models")
	if err := os.Mkdir(usable, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

	// A directory outside of the state root the account of the instance
	// cannot work with.
	unusable := filepath.Join(root, "other-models")
	if err := os.Mkdir(unusable, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   stateDir,
		LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
	}
	opts.HubDir = ""
	opts.LocalDir = ""

	// The directory the account of the instance can work with is
	// accepted.
	opts.CacheDir = usable
	if err := preflightDataPaths(opts, scmTestPathAccessFor(t, usable, true)); err != nil {
		t.Errorf("preflightDataPaths() = %v, want nil for a directory the account of the instance can work with", err)
	}

	// The directory it cannot work with is refused.
	opts.CacheDir = unusable
	err := preflightDataPaths(opts, scmTestPathAccessFor(t, unusable, false))
	if err == nil {
		t.Fatalf("preflightDataPaths() = nil, want the refusal of a directory the account of the instance cannot work with")
	}
	if !scmTestNamesPath(err.Error(), unusable) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the path", err)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the account of the instance", err)
	}
}

// TestSCMPreflightDataPathsRefuseALinkThatLeavesTheStateRoot checks that a
// named directory that is reached through a link inside the state root is
// checked like every other named directory.
//
// The exemption of the state root is decided on the real location of the two
// paths: a link that leaves the state root points at a directory outside of
// it, and the account of the instance has to be able to work with it. A
// comparison of the paths as they were written would grant the exemption to
// the link, and the installation would be accepted and fail later with an
// access error.
func TestSCMPreflightDataPathsRefuseALinkThatLeavesTheStateRoot(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "var", "lib", "hfdesk")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}

	// A directory outside of the state root that the account of the
	// instance cannot use.
	outside := filepath.Join(root, "models")
	if err := os.Mkdir(outside, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

	// A link inside the state root that points at it.
	link := filepath.Join(stateDir, "cache")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("the platform does not allow a symbolic link here: %v", err)
	}

	// The link is not inside the state root: the directory it points at is
	// somewhere else.
	inside, err := scmPathInsideStateRoot(stateDir, link)
	if err != nil {
		t.Fatalf("scmPathInsideStateRoot() = %v, want nil", err)
	}
	if inside {
		t.Errorf("scmPathInsideStateRoot(%q, %q) = true, want false: the link points outside of the state root", stateDir, link)
	}

	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   stateDir,
		LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
	}
	opts.CacheDir = link
	opts.HubDir = ""
	opts.LocalDir = ""

	err = preflightDataPaths(opts, scmTestPathAccessFor(t, link, false))
	if err == nil {
		t.Fatalf("preflightDataPaths() = nil, want the refusal of a link that leaves the state root")
	}
	if !scmTestNamesPath(err.Error(), link) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the path", err)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the account of the instance", err)
	}
}

// TestPreflightDataPathRefusesAPathTheIdentityCannotReach checks that a
// directory the account of the instance cannot reach is refused before the
// registration is written.
//
// The directory does not exist: the instance creates it when it runs, as the
// account the instance runs as, so the refusal names the missing directory,
// the nearest existing ancestor it would be created in and the access the
// account is missing there.
//
// The guidance names the account of the instance, because the access rights
// of the ancestor have to be granted to that account.
func TestPreflightDataPathRefusesAPathTheIdentityCannotReach(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	path := filepath.Join(parent, "cache")
	err := preflightDataPath("cache dir", path, scmTestPathAccessFor(t, path, false))
	if err == nil {
		t.Fatalf("preflightDataPath(%q) = nil, want an error", path)
	}
	if !scmTestNamesPath(err.Error(), path) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the path", path, err)
	}
	// The manager names the real location of the ancestor: the platform
	// reports the name the directory has on the disk, so a directory that is
	// reached through a short name is named by its long name. The name of the
	// ancestor is what makes the guidance actionable, and the check accepts
	// the path as it was written and the real location of it.
	if !scmTestNamesPath(err.Error(), parent) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the nearest existing ancestor %q", path, err, parent)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the account of the instance", path, err)
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the missing directory", path, err)
	}
	if !strings.Contains(err.Error(), "read and write access") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the access that is missing", path, err)
	}
	if !strings.Contains(err.Error(), "never creates a directory the user named") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that tells the user that the installation creates no directory of its own", path, err)
	}
	if !strings.Contains(err.Error(), "grant the account read and write access to the ancestor directory") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that tells the user what to do", path, err)
	}
	if !strings.Contains(err.Error(), "hfdesk service install") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the rerun command", path, err)
	}
	if strings.Contains(err.Error(), "or install the instance") {
		t.Errorf("preflightDataPath(%q) = %v, want no alternative to granting the access", path, err)
	}
	// The refused directory is not created behind the back of the user.
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the refused directory %q exists: %v", path, statErr)
	}
}

// TestPreflightDataPathDoesNotInferLocalServiceAccessWithoutItsToken keeps an
// incomplete identity model from becoming a denial oracle.
func TestPreflightDataPathDoesNotInferLocalServiceAccessWithoutItsToken(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cache")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	err := preflightDataPath("cache dir", path)
	if err != nil {
		t.Fatalf("preflightDataPath(%q) = %v, want inconclusive success without an actual LocalService token", path, err)
	}
}

func TestPreflightDataPathLeavesDescriptorReadFailureInconclusive(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cache")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	resolved, err := scmResolvePath(path)
	if err != nil {
		t.Fatalf("resolve directory %q: %v", path, err)
	}
	access := &scmTestPathAccess{errors: map[string]error{resolved: errors.New("descriptor unavailable")}}
	if err := preflightDataPath("cache dir", path, access); err != nil {
		t.Fatalf("preflightDataPath(%q) = %v, want unknown AccessCheck evidence to remain inconclusive", path, err)
	}
}

// TestSCMPreflightExecutableAccess checks that a definite denial refuses, an
// allowed descriptor passes and an unreadable one refuses nothing. The
// refusal tells the user to grant execute access; it never demands read
// access to a native program.
func TestSCMPreflightExecutableAccess(t *testing.T) {
	previous := scmCheckExecutableAccess
	t.Cleanup(func() { scmCheckExecutableAccess = previous })

	executable := `C:\ProgramData\HFDesk\hfdesk.exe`
	cases := []struct {
		name    string
		access  scmExecutableAccess
		reason  string
		wantErr bool
	}{
		{"allowed", scmExecutableAccessAllowed, "", false},
		{"denied", scmExecutableAccessDenied, "the test denies the access", true},
		{"inconclusive", scmExecutableAccessInconclusive, "the test cannot read the descriptor", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scmCheckExecutableAccess = func(string, scmPathAccess) (scmExecutableAccess, string) {
				return tc.access, tc.reason
			}
			err := preflightExecutableAccess(executable)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("preflightExecutableAccess() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("preflightExecutableAccess() = nil, want the refusal of the program")
			}
			if !scmTestNamesPath(err.Error(), executable) {
				t.Errorf("preflightExecutableAccess() = %v, want the refused executable path %q", err, executable)
			}
			for _, want := range []string{scmServiceIdentityName, tc.reason, "grant the account execute access", "hfdesk service install"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("preflightExecutableAccess() = %v, want %q in the refusal", err, want)
				}
			}
			for _, unwanted := range []string{"read and execute", "grant the account read", "or choose a program"} {
				if strings.Contains(err.Error(), unwanted) {
					t.Errorf("preflightExecutableAccess() = %v, want no demand to grant %q for a native program", err, unwanted)
				}
			}
		})
	}
}

// TestSCMPreflightExecutableAccessAcceptsExecuteOnlyNativeProgram pins the
// Windows contract of the program check: the program image is checked for
// execute only. An NTFS "Execute file" entry that withholds read data passes
// preflight, the preflight asks AccessCheck for the execute right of the
// program and never for its data, and a program the account cannot execute is
// still refused with a message that asks for execute access only.
func TestSCMPreflightExecutableAccessAcceptsExecuteOnlyNativeProgram(t *testing.T) {
	token := scmTestImpersonationToken(t)
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser() = %v", err)
	}
	if user.User.Sid == nil {
		t.Fatal("GetTokenUser() returned no user SID")
	}
	sid := user.User.Sid.String()

	root := t.TempDir()
	executable := filepath.Join(root, "hfdesk.exe")
	if err := os.WriteFile(executable, []byte("test program image"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) = %v", executable, err)
	}
	resolved, err := scmResolvePath(executable)
	if err != nil {
		t.Fatalf("resolve %q: %v", executable, err)
	}

	// The execute-only descriptor carries execute but no read data for the
	// program, so the real AccessCheck accepts the right the fixed preflight
	// asks for and refuses the read+execute mask the preflight used to ask
	// for: the old mask refused exactly this fixture.
	executeOnly := scmTestAccessCheckDescriptor(t, sid, fmt.Sprintf("D:P(A;;FX;;;%s)", sid))
	for _, tc := range []struct {
		name    string
		desired uint32
		allowed bool
	}{
		{"execute right of the program", windows.FILE_EXECUTE, true},
		{"the former read and execute mask", windows.GENERIC_READ | windows.GENERIC_EXECUTE, false},
	} {
		allowed, err := scmAccessCheckDescriptor(token, executeOnly, tc.desired)
		if err != nil {
			t.Fatalf("AccessCheck(%s) = %v", tc.name, err)
		}
		if allowed != tc.allowed {
			t.Errorf("AccessCheck(%s) = %v, want %v", tc.name, allowed, tc.allowed)
		}
	}

	// The preflight passes the execute-only native program...
	access := &scmTestPathAccess{
		check: func(_ string, desired uint32) (bool, error) {
			return scmAccessCheckDescriptor(token, executeOnly, desired)
		},
		// The traverse-bypass privilege isolates this fixture on the
		// program object itself; the traversal route is covered by its
		// own tests.
		privilege: true,
	}
	if err := preflightExecutableAccess(executable, access); err != nil {
		t.Fatalf("preflightExecutableAccess() = %v, want the execute-only program to pass", err)
	}
	if len(access.calls) != 1 {
		t.Fatalf("AccessCheck calls = %+v, want only the program object checked", access.calls)
	}
	if call := access.calls[0]; call.path != resolved || call.desired != windows.FILE_EXECUTE {
		t.Errorf("AccessCheck(%q, 0x%X), want the program checked at %q with FILE_EXECUTE only", call.path, call.desired, resolved)
	}

	// ...and still refuses a program the account cannot execute.
	readOnly := scmTestAccessCheckDescriptor(t, sid, fmt.Sprintf("D:P(A;;FR;;;%s)", sid))
	denied := &scmTestPathAccess{
		check: func(_ string, desired uint32) (bool, error) {
			return scmAccessCheckDescriptor(token, readOnly, desired)
		},
		privilege: true,
	}
	err = preflightExecutableAccess(executable, denied)
	if err == nil {
		t.Fatal("preflightExecutableAccess() = nil, want the refusal of a program without execute access")
	}
	if !strings.Contains(err.Error(), "grant the account execute access") {
		t.Errorf("preflightExecutableAccess() = %v, want the refusal to ask for execute access", err)
	}
	for _, unwanted := range []string{"read and execute", "grant the account read"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("preflightExecutableAccess() = %v, want no demand to grant %q", err, unwanted)
		}
	}
}

type scmTestPathAccess struct {
	results          map[string]bool
	resultsByDesired map[string]map[uint32]bool
	errors           map[string]error
	check            func(string, uint32) (bool, error)
	defaultSet       bool
	defaultAllow     bool
	privilege        bool
	privilegeErr     error
	calls            []struct {
		path    string
		desired uint32
	}
}

func (a *scmTestPathAccess) Check(path string, desired uint32) (bool, error) {
	a.calls = append(a.calls, struct {
		path    string
		desired uint32
	}{path, desired})
	if err := a.errors[path]; err != nil {
		return false, err
	}
	if a.check != nil {
		return a.check(path, desired)
	}
	if byDesired, ok := a.resultsByDesired[path]; ok {
		if result, ok := byDesired[desired]; ok {
			return result, nil
		}
	}
	result, ok := a.results[path]
	if !ok {
		if a.defaultSet {
			return a.defaultAllow, nil
		}
		return false, fmt.Errorf("no test AccessCheck result for %q", path)
	}
	return result, nil
}

func (a *scmTestPathAccess) HasEnabledPrivilege(name string) (bool, error) {
	if name != scmChangeNotifyPrivilege {
		return false, fmt.Errorf("unexpected privilege query %q", name)
	}
	return a.privilege, a.privilegeErr
}

func (*scmTestPathAccess) Close() {}

func scmTestPathAccessFor(t *testing.T, path string, allowed bool) *scmTestPathAccess {
	t.Helper()
	checked := path
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		var nearestErr error
		checked, nearestErr = scmNearestExistingDir(path)
		if nearestErr != nil {
			t.Fatalf("find the AccessCheck target for %q: %v", path, nearestErr)
		}
	}
	resolved, err := scmResolvePath(checked)
	if err != nil {
		t.Fatalf("resolve the AccessCheck target for %q: %v", path, err)
	}
	return &scmTestPathAccess{
		results:      map[string]bool{resolved: allowed},
		defaultSet:   true,
		defaultAllow: true,
		privilege:    true,
	}
}

func scmTestImpersonationToken(t *testing.T) windows.Token {
	t.Helper()
	var primary windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &primary); err != nil {
		t.Fatalf("OpenProcessToken() = %v", err)
	}
	t.Cleanup(func() { _ = primary.Close() })
	var token windows.Token
	if err := windows.DuplicateTokenEx(primary, windows.TOKEN_QUERY,
		nil, windows.SecurityImpersonation, windows.TokenImpersonation, &token); err != nil {
		t.Fatalf("DuplicateTokenEx() = %v", err)
	}
	t.Cleanup(func() { _ = token.Close() })
	return token
}

// AccessCheck requires valid owner and group SIDs in the descriptor, not only
// the DACL that determines the result.
func scmTestAccessCheckDescriptor(t *testing.T, ownerSID, dacl string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	sddl := fmt.Sprintf("O:%sG:%s%s", ownerSID, ownerSID, dacl)
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("SecurityDescriptorFromString(%q) = %v", sddl, err)
	}
	if !sd.IsValid() {
		t.Fatalf("SecurityDescriptorFromString(%q) returned an invalid descriptor", sddl)
	}
	if owner, _, err := sd.Owner(); err != nil || owner == nil {
		t.Fatalf("security descriptor owner = %v, %v; want a valid SID", owner, err)
	}
	if group, _, err := sd.Group(); err != nil || group == nil {
		t.Fatalf("security descriptor group = %v, %v; want a valid SID", group, err)
	}
	return sd
}

func scmTestCreateDirectorySymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) || errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
			errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
			t.Skipf("directory reparse points are unavailable: %v", err)
		}
		t.Fatalf("Symlink(%q, %q): %v", target, link, err)
	}
}

func TestSCMAccessCheckMapsGenericRightsAndHonorsDACLOrder(t *testing.T) {
	token := scmTestImpersonationToken(t)
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser() = %v", err)
	}
	if user.User.Sid == nil {
		t.Fatal("GetTokenUser() returned no user SID")
	}
	sid := user.User.Sid.String()
	for _, tc := range []struct {
		name    string
		dacl    string
		allowed bool
	}{
		{"earlier complete explicit allow wins before inherited deny", fmt.Sprintf("D:P(A;;FR;;;%s)(A;;FX;;;%s)(D;ID;FA;;;WD)", sid, sid), true},
		{"earlier deny blocks later allow", "D:P(D;;FR;;;WD)(A;;FA;;;" + sid + ")", false},
		{"inherit-only allow does not apply to this object", "D:P(A;OICIIO;FA;;;" + sid + ")", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd := scmTestAccessCheckDescriptor(t, sid, tc.dacl)
			allowed, err := scmAccessCheckDescriptor(token, sd, windows.GENERIC_READ|windows.GENERIC_EXECUTE)
			if err != nil {
				t.Fatalf("AccessCheck = %v", err)
			}
			if allowed != tc.allowed {
				t.Errorf("AccessCheck(%q) = %v, want %v", tc.dacl, allowed, tc.allowed)
			}
		})
	}
}

func TestSCMPathRouteUsesNativeAccessCheckForSourceDenials(t *testing.T) {
	token := scmTestImpersonationToken(t)
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser() = %v", err)
	}
	if user.User.Sid == nil {
		t.Fatal("GetTokenUser() returned no user SID")
	}
	root := t.TempDir()
	restricted := filepath.Join(root, "restricted")
	intermediate := filepath.Join(root, "intermediate")
	target := filepath.Join(root, "target")
	for _, dir := range []string{restricted, intermediate, target} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("Mkdir(%q): %v", dir, err)
		}
	}
	firstLink := filepath.Join(restricted, "junction")
	scmTestCreateDirectorySymlink(t, intermediate, firstLink)
	secondLink := filepath.Join(intermediate, "nested")
	scmTestCreateDirectorySymlink(t, target, secondLink)
	sourcePath := filepath.Join(firstLink, "nested", "hfdesk.exe")
	targetFile := filepath.Join(target, "hfdesk.exe")
	if err := os.WriteFile(targetFile, []byte("test image"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", targetFile, err)
	}
	resolved, err := scmResolvePath(sourcePath)
	if err != nil {
		t.Fatalf("resolve nested route %q: %v", sourcePath, err)
	}
	resolvedTarget, err := scmResolvePath(targetFile)
	if err != nil {
		t.Fatalf("resolve nested target %q: %v", targetFile, err)
	}
	if !strings.EqualFold(filepath.Clean(resolved), filepath.Clean(resolvedTarget)) {
		t.Fatalf("resolved route = %q, resolved target = %q; want the nested route to name that target", resolved, resolvedTarget)
	}
	resolvedRestricted, err := scmResolvePath(restricted)
	if err != nil {
		t.Fatalf("resolve source ancestor %q: %v", restricted, err)
	}
	resolvedIntermediate, err := scmResolvePath(firstLink)
	if err != nil {
		t.Fatalf("resolve first reparse target %q: %v", firstLink, err)
	}

	sid := user.User.Sid.String()
	allowed := scmTestAccessCheckDescriptor(t, sid, fmt.Sprintf("D:P(A;;FA;;;%s)", sid))
	deniedTraverse := scmTestAccessCheckDescriptor(t, sid, fmt.Sprintf("D:P(D;;FX;;;%s)(A;;FA;;;WD)", sid))
	descriptors := map[string]*windows.SECURITY_DESCRIPTOR{
		strings.ToUpper(filepath.Clean(resolved)):           allowed,
		strings.ToUpper(filepath.Clean(resolvedRestricted)): deniedTraverse,
	}
	// The zero-value privilege flag forces the no-bypass route through AccessCheck.
	access := &scmTestPathAccess{
		check: func(path string, desired uint32) (bool, error) {
			descriptor := descriptors[strings.ToUpper(filepath.Clean(path))]
			if descriptor == nil {
				descriptor = allowed
			}
			return scmAccessCheckDescriptor(token, descriptor, desired)
		},
	}
	decision, reason := scmCheckPathAccess(sourcePath, windows.GENERIC_READ|windows.GENERIC_EXECUTE, access)
	if decision != scmExecutableAccessDenied || !scmTestNamesPath(reason, restricted) {
		t.Fatalf("native AccessCheck with an allowed target and denied source ancestor = %v, %q; want source traversal denied", decision, reason)
	}

	descriptors[strings.ToUpper(filepath.Clean(resolvedRestricted))] = allowed
	descriptors[strings.ToUpper(filepath.Clean(resolvedIntermediate))] = deniedTraverse
	decision, reason = scmCheckPathAccess(sourcePath, windows.GENERIC_READ|windows.GENERIC_EXECUTE, access)
	if decision != scmExecutableAccessDenied || !scmTestNamesPath(reason, intermediate) {
		t.Fatalf("native AccessCheck with a denied nested reparse target = %v, %q; want the intermediate route denial", decision, reason)
	}

	descriptors[strings.ToUpper(filepath.Clean(resolvedIntermediate))] = allowed
	decision, reason = scmCheckPathAccess(sourcePath, windows.GENERIC_READ|windows.GENERIC_EXECUTE, access)
	if decision != scmExecutableAccessAllowed {
		t.Fatalf("native AccessCheck over the complete nested route = %v, %q; want allowed", decision, reason)
	}

	access.errors = map[string]error{resolvedIntermediate: errors.New("nested route descriptor unavailable")}
	decision, _ = scmCheckPathAccess(sourcePath, windows.GENERIC_READ|windows.GENERIC_EXECUTE, access)
	if decision != scmExecutableAccessInconclusive {
		t.Errorf("native route with an unavailable nested descriptor = %v, want inconclusive", decision)
	}
}

// TestSCMPathRouteChecksEveryAncestorOfAResolvedReparseRoute proves the
// traversal route covers the ancestors of every resolved prefix, not only the
// final endpoint: a nested reparse route whose intermediate target lives
// under a directory outside either endpoint's parents passes through that
// directory, and without SeChangeNotifyPrivilege the token needs
// FILE_TRAVERSE there.
func TestSCMPathRouteChecksEveryAncestorOfAResolvedReparseRoute(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	deniedDir := filepath.Join(root, "denied")
	middleDir := filepath.Join(deniedDir, "middle")
	targetDir := filepath.Join(root, "target")
	for _, dir := range []string{sourceDir, deniedDir, middleDir, targetDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}
	// source/junction -> denied/middle, denied/middle/nested -> target: the
	// route to the file passes through denied, which is an ancestor of
	// neither the source path nor the resolved endpoint.
	firstLink := filepath.Join(sourceDir, "junction")
	scmTestCreateDirectorySymlink(t, middleDir, firstLink)
	nestedLink := filepath.Join(middleDir, "nested")
	scmTestCreateDirectorySymlink(t, targetDir, nestedLink)
	sourcePath := filepath.Join(firstLink, "nested", "hfdesk.exe")
	if err := os.WriteFile(filepath.Join(targetDir, "hfdesk.exe"), []byte("test image"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	resolvedDenied, err := scmResolvePath(deniedDir)
	if err != nil {
		t.Fatalf("resolve the intermediate ancestor %q: %v", deniedDir, err)
	}
	denyTraverse := func(path string, desired uint32) bool {
		return desired&windows.FILE_TRAVERSE != 0 &&
			strings.EqualFold(filepath.Clean(path), filepath.Clean(resolvedDenied))
	}

	// Without the traverse privilege the intermediate ancestor is checked
	// and its denial stops the route.
	access := &scmTestPathAccess{
		check: func(path string, desired uint32) (bool, error) {
			return !denyTraverse(path, desired), nil
		},
	}
	decision, reason := scmCheckPathAccess(sourcePath, windows.FILE_EXECUTE, access)
	if decision != scmExecutableAccessDenied || !scmTestNamesPath(reason, resolvedDenied) {
		t.Fatalf("route with a denied intermediate ancestor = %v, %q; want the traversal of %q denied",
			decision, reason, resolvedDenied)
	}

	// The other side: the same route passes when the ancestor grants
	// traversal.
	allowed := &scmTestPathAccess{
		check: func(string, uint32) (bool, error) { return true, nil },
	}
	if decision, reason := scmCheckPathAccess(sourcePath, windows.FILE_EXECUTE, allowed); decision != scmExecutableAccessAllowed {
		t.Fatalf("route with a granted intermediate ancestor = %v, %q; want allowed", decision, reason)
	}

	// The privilege keeps its semantics: with SeChangeNotifyPrivilege
	// enabled the traversal route is not required.
	bypass := &scmTestPathAccess{
		check: func(path string, desired uint32) (bool, error) {
			return !denyTraverse(path, desired), nil
		},
		privilege: true,
	}
	if decision, reason := scmCheckPathAccess(sourcePath, windows.FILE_EXECUTE, bypass); decision != scmExecutableAccessAllowed {
		t.Fatalf("route with the traverse-bypass privilege = %v, %q; want allowed without the traversal", decision, reason)
	}
}

func TestSCMPathAccessHonorsTraversePrivilegeAndUnknownEvidence(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("Mkdir(%q): %v", parent, err)
	}
	path := filepath.Join(parent, "hfdesk.exe")
	if err := os.WriteFile(path, []byte("test image"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	resolved, err := scmResolvePath(path)
	if err != nil {
		t.Fatalf("resolve executable %q: %v", path, err)
	}
	resolvedParent, err := scmResolvePath(parent)
	if err != nil {
		t.Fatalf("resolve parent %q: %v", parent, err)
	}
	withPrivilege := &scmTestPathAccess{
		results:   map[string]bool{resolved: true, resolvedParent: false},
		privilege: true,
	}
	decision, _ := scmCheckPathAccess(path, windows.GENERIC_READ|windows.GENERIC_EXECUTE, withPrivilege)
	if decision != scmExecutableAccessAllowed {
		t.Fatalf("a denied parent traverse ACE overrode enabled SeChangeNotifyPrivilege: decision %v", decision)
	}
	if len(withPrivilege.calls) != 1 || !strings.EqualFold(filepath.Clean(withPrivilege.calls[0].path), filepath.Clean(resolved)) {
		t.Errorf("AccessCheck calls = %+v, want only the executable object when traverse bypass is enabled", withPrivilege.calls)
	}

	withoutPrivilege := &scmTestPathAccess{
		results: map[string]bool{resolved: true, resolvedParent: false},
	}
	decision, _ = scmCheckPathAccess(path, windows.GENERIC_READ|windows.GENERIC_EXECUTE, withoutPrivilege)
	if decision != scmExecutableAccessDenied {
		t.Errorf("a real parent traverse denial without the bypass privilege = %v, want denied", decision)
	}

	decision, _ = scmCheckPathAccess(path, windows.GENERIC_READ|windows.GENERIC_EXECUTE, nil)
	if decision != scmExecutableAccessInconclusive {
		t.Errorf("missing token evidence = %v, want inconclusive", decision)
	}
	unknown := &scmTestPathAccess{errors: map[string]error{resolved: errors.New("descriptor unavailable")}}
	decision, _ = scmCheckPathAccess(path, windows.GENERIC_READ|windows.GENERIC_EXECUTE, unknown)
	if decision != scmExecutableAccessInconclusive {
		t.Errorf("unavailable descriptor evidence = %v, want inconclusive", decision)
	}
	privilegeUnknown := &scmTestPathAccess{
		results:      map[string]bool{resolved: true},
		privilegeErr: errors.New("token privileges unavailable"),
	}
	decision, _ = scmCheckPathAccess(path, windows.GENERIC_READ|windows.GENERIC_EXECUTE, privilegeUnknown)
	if decision != scmExecutableAccessInconclusive {
		t.Errorf("unavailable traverse-privilege evidence = %v, want inconclusive", decision)
	}
}

func TestSCMPathAccessChecksTraversalOfAWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	resolved, err := scmResolvePath(dir)
	if err != nil {
		t.Fatalf("resolve directory %q: %v", dir, err)
	}
	access := &scmTestPathAccess{
		defaultSet:   true,
		defaultAllow: true,
		resultsByDesired: map[string]map[uint32]bool{
			resolved: {
				windows.GENERIC_READ | windows.GENERIC_WRITE: true,
				windows.FILE_TRAVERSE:                        false,
			},
		},
	}
	decision, reason := scmCheckPathAccess(dir, windows.GENERIC_READ|windows.GENERIC_WRITE, access)
	if decision != scmExecutableAccessDenied || !scmTestNamesPath(reason, resolved) {
		t.Fatalf("a working directory without traverse access = %v, %q; want the actual directory denial", decision, reason)
	}
}

func TestSCMDataDirectoryPreflightUsesTheSameAccessCheckOwner(t *testing.T) {
	dir := t.TempDir()
	denied := scmTestPathAccessFor(t, dir, false)
	err := preflightDataPath("cache dir", dir, denied)
	if err == nil || !strings.Contains(err.Error(), "Windows AccessCheck denied") {
		t.Fatalf("preflightDataPath() = %v, want the actual AccessCheck denial", err)
	}
	unknown := preflightDataPath("cache dir", dir)
	if unknown != nil {
		t.Fatalf("preflightDataPath() without a service token = %v, want inconclusive success", unknown)
	}
}

// TestPreflightExecutableImageRefusesAFileThatIsNotAProgram proves that an
// installation refuses a file that is not a Windows program: a text file
// renamed to hfdesk.exe is not a program the service control manager could
// start.
func TestPreflightExecutableImageRefusesAFileThatIsNotAProgram(t *testing.T) {
	path := scmTestExecutable(t, []byte(strings.Repeat("this is not a Windows program\n", 8)))

	err := preflightExecutableImage(path)
	if err == nil {
		t.Fatal("preflightExecutableImage accepted a file that is not a Windows program")
	}
	if !scmTestNamesPath(err.Error(), path) {
		t.Errorf("preflightExecutableImage(%q) = %v, want an error that names the file", path, err)
	}
	if !strings.Contains(err.Error(), "not a Windows executable") {
		t.Errorf("preflightExecutableImage(%q) = %v, want an error that says the file is not a Windows executable", path, err)
	}
	// The refusal names the one recovery: the Windows build of the
	// program, then the rerun of the installation.
	if !strings.Contains(err.Error(), "hfdesk service install") {
		t.Errorf("preflightExecutableImage(%q) = %v, want an error that names the rerun command", path, err)
	}
}

// TestPreflightExecutableImageRefusesATruncatedProgram proves that a program
// whose header was cut off is refused: a download that ended in the middle of
// the header is not a program the service control manager could start.
func TestPreflightExecutableImageRefusesATruncatedProgram(t *testing.T) {
	image := scmTestPEImage(pe.MachineAMD64)
	// The image ends before the PE header its DOS header points at.
	path := scmTestExecutable(t, image[:scmTestDosHeaderSize])

	err := preflightExecutableImage(path)
	if err == nil {
		t.Fatal("preflightExecutableImage accepted a truncated program")
	}
	if !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("preflightExecutableImage(%q) = %v, want the truncated header to be reported", path, err)
	}
}

// TestPreflightExecutableImageUsesTheHostSupportResult proves that candidate
// acceptance follows Windows host support rather than the architecture of the
// installer process.
func TestPreflightExecutableImageUsesTheHostSupportResult(t *testing.T) {
	cases := []struct {
		name    string
		machine uint16
		support scmExecutableSupport
		wantErr bool
	}{
		{"host-supported native image", pe.MachineAMD64, scmExecutableSupported, false},
		{"host-supported emulated image", pe.MachineARM64, scmExecutableSupported, false},
		{"known unsupported image", pe.MachineI386, scmExecutableUnsupported, true},
		{"unknown host support is not incompatibility", pe.MachineARM64, scmExecutableSupportUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := scmTestExecutable(t, scmTestPEImage(tc.machine))

			err := preflightExecutableImageWithSupport(path, func(uint16) scmExecutableSupport {
				return tc.support
			})
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("preflightExecutableImageWithSupport(%q) = %v, want nil", path, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("preflightExecutableImageWithSupport(%q) = nil, want the refusal of machine 0x%04X", path, tc.machine)
			}
			if !scmTestNamesPath(err.Error(), path) {
				t.Errorf("preflightExecutableImageWithSupport(%q) = %v, want an error that names the file", path, err)
			}
			if !strings.Contains(err.Error(), "reports this host cannot execute") {
				t.Errorf("preflightExecutableImageWithSupport(%q) = %v, want a known host incompatibility", path, err)
			}
			// The refusal names the one recovery: the build for this
			// machine, then the rerun of the installation.
			if !strings.Contains(err.Error(), "hfdesk service install") {
				t.Errorf("preflightExecutableImageWithSupport(%q) = %v, want an error that names the rerun command", path, err)
			}
		})
	}
}

func TestPreflightExecutableImageAcceptsTheActualWindowsTestExecutable(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() = %v", err)
	}
	if err := preflightExecutableImage(path); err != nil {
		t.Fatalf("preflightExecutableImage(%q) = %v, want the actual running test image to pass", path, err)
	}
}

func TestPreflightExecutableImageRequiresACompletePEExecutable(t *testing.T) {
	path := scmTestExecutable(t, []byte(strings.Repeat("this is not a Windows program\n", 8)))

	if err := preflightExecutableImageWithSupport(path, nil); err == nil {
		t.Fatal("preflightExecutableImageWithSupport accepted a file that is not a Windows program")
	}
}

func TestPreflightExecutableImageSeparatesInspectionAccessFromDamage(t *testing.T) {
	for _, readErr := range []error{windows.ERROR_ACCESS_DENIED, fmt.Errorf("read optional header: %w", fs.ErrPermission)} {
		supportQueried := false
		err := preflightExecutableImageUsing(`C:\Restricted\hfdesk.exe`,
			func(string) (uint16, error) { return 0, readErr },
			func(uint16) scmExecutableSupport {
				supportQueried = true
				return scmExecutableUnsupported
			})
		if err != nil {
			t.Errorf("preflightExecutableImageUsing(access error) = %v, want inconclusive success", err)
		}
		if supportQueried {
			t.Error("host support was queried without a machine field")
		}
	}

	structureErr := errors.New("truncated optional header")
	err := preflightExecutableImageUsing(`C:\Restricted\hfdesk.exe`,
		func(string) (uint16, error) { return 0, structureErr }, nil)
	if err == nil || !errors.Is(err, structureErr) || !strings.Contains(err.Error(), "not a Windows executable") {
		t.Errorf("preflightExecutableImageUsing(structural error) = %v, want the invalid-image refusal", err)
	}

	missing := filepath.Join(t.TempDir(), "missing.exe")
	if err := preflightExecutableImage(missing); err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("preflightExecutableImage(%q) = %v, want the demonstrably missing candidate refused", missing, err)
	}
}

// TestInstanceStatePathsTellTheCreatedPathsFromTheExistingOnes checks that
// the installation knows which paths of the instance it owns: the
// directories it creates carry the restrictive descriptor, the paths that
// existed before the installation do not.
//
// A directory that existed before the installation belongs to whoever owns
// it, so the installation reads its descriptor and changes nothing.
func TestInstanceStatePathsTellTheCreatedPathsFromTheExistingOnes(t *testing.T) {
	root := t.TempDir()
	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	opts.CacheDir = filepath.Join(root, "cache")
	opts.HubDir = filepath.Join(root, "cache", "hub")
	opts.LocalDir = filepath.Join(root, "models")

	// The config directory and the config file existed before the
	// installation.
	if err := os.MkdirAll(filepath.Dir(opts.Paths.ConfigFile), 0o700); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	if err := os.WriteFile(opts.Paths.ConfigFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	snapshot := newSCMInstallSnapshot(opts)
	paths := instanceStatePaths(opts, snapshot)

	created := make(map[string]bool, len(paths))
	existing := make(map[string]bool, len(paths))
	for _, path := range paths {
		if path.created {
			created[path.path] = true
			continue
		}
		existing[path.path] = true
	}

	// The directories the installation creates are the state directory
	// and the log directory: the config directory existed already.
	for _, want := range []string{opts.Paths.StateDir, opts.Paths.LogDir} {
		if !created[want] {
			t.Errorf("the state directory %q is not marked as created: %v", want, paths)
		}
	}
	for _, want := range []string{filepath.Dir(opts.Paths.ConfigFile), opts.Paths.ConfigFile} {
		if !existing[want] {
			t.Errorf("the path %q is not marked as existing: %v", want, paths)
		}
	}
	for path := range created {
		if existing[path] {
			t.Errorf("the path %q is marked as created and as existing: %v", path, paths)
		}
	}
}

// TestSCMInstanceStateRestrictsTheDirectoriesItCreated checks that the
// directories the installation creates carry the restrictive descriptor of
// the instance.
//
// The descriptor is the one of the instance: SYSTEM, the administrators of
// the machine and the account the instance runs as, nobody else, and the
// inheritance of the parent directory is cut off.
func TestSCMInstanceStateRestrictsTheDirectoriesItCreated(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	if err := restrictInstanceState(opts, snapshot); err != nil {
		t.Fatalf("restrictInstanceState() = %v, want nil", err)
	}

	want := []string{"S-1-5-18", "S-1-5-19", "S-1-5-32-544"}
	for _, dir := range []string{
		filepath.Dir(opts.Paths.ConfigFile),
		opts.Paths.StateDir,
		opts.Paths.LogDir,
	} {
		if !scmTestDACLProtected(t, dir) {
			t.Errorf("the access control list of %q is not protected, want a protected one", dir)
		}
		got := scmTestTrustees(t, dir)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("the trustees of %q = %v, want %v", dir, got, want)
		}
	}
}

// TestSCMInstanceStateKeepsTheDescriptorOfADirectoryThatExistedBefore
// checks that a directory the user named and that existed before the
// installation keeps its descriptor.
//
// The descriptor of the directory grants the access to everybody, so a
// rewrite of it by the restrictive descriptor of the instance would be
// visible in the comparison.
func TestSCMInstanceStateKeepsTheDescriptorOfADirectoryThatExistedBefore(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	// The state directory existed before the installation.
	stateDir := opts.Paths.StateDir
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", stateDir, err)
	}
	scmTestSetDACL(t, stateDir, fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;WD)"))
	before := scmTestDACL(t, stateDir)

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	if err := restrictInstanceState(opts, snapshot); err != nil {
		t.Fatalf("restrictInstanceState() = %v, want nil", err)
	}

	if after := scmTestDACL(t, stateDir); after != before {
		t.Errorf("the access control list of the state directory %q = %q, want %q", stateDir, after, before)
	}
	// The restrictive descriptor of the instance was not applied to it:
	// everybody still has the access the directory had.
	trustees := scmTestTrustees(t, stateDir)
	if !slices.Contains(trustees, "S-1-1-0") {
		t.Errorf("the trustees of the state directory %q = %v, want everybody", stateDir, trustees)
	}
}

// TestSCMInstanceStateRefusesADirectoryTheIdentityCannotUse checks that an
// established AccessCheck denial for an existing state directory is refused,
// and that no descriptor was changed.
//
// The refusal happens before the first descriptor is changed, so the
// directories the installation created keep the descriptor they inherited.
func TestSCMInstanceStateRefusesADirectoryTheIdentityCannotUse(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	// Access denial is injected through the native-query seam, without
	// changing its ACL.
	stateDir := opts.Paths.StateDir
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", stateDir, err)
	}
	// The path access result is injected below; no ACL is changed by this test.

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	before := make(map[string]string, 3)
	for _, path := range []string{
		stateDir,
		filepath.Dir(opts.Paths.ConfigFile),
		opts.Paths.LogDir,
	} {
		before[path] = scmTestDACL(t, path)
	}

	err := restrictInstanceState(opts, snapshot, scmTestPathAccessFor(t, stateDir, false))
	if err == nil {
		t.Fatal("restrictInstanceState() = nil, want the refusal of the state directory")
	}
	if !scmTestNamesPath(err.Error(), stateDir) {
		t.Errorf("restrictInstanceState() = %v, want an error that names the path", err)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("restrictInstanceState() = %v, want an error that names the account of the instance", err)
	}
	if !strings.Contains(err.Error(), "grant the account") {
		t.Errorf("restrictInstanceState() = %v, want an error that tells the user what to do", err)
	}

	// No descriptor was changed anywhere: not on the directory that
	// existed before and not on the directories the installation
	// created.
	for path, want := range before {
		if got := scmTestDACL(t, path); got != want {
			t.Errorf("the access control list of %q = %q, want %q", path, got, want)
		}
	}
}

// TestSCMInstanceStateKeepsTheDescriptorOfAConfigFileThatExistedBefore
// checks that the config file of the instance keeps its descriptor when it
// existed before the installation.
//
// The config file holds the credentials of the instance, but it belongs to
// whoever wrote it: the installation reads its descriptor and changes
// nothing.
func TestSCMInstanceStateKeepsTheDescriptorOfAConfigFileThatExistedBefore(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	configDir := filepath.Dir(opts.Paths.ConfigFile)
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", configDir, err)
	}
	// The directory has to be usable by the account of the instance,
	// otherwise the installation refuses it.
	scmTestSetDACL(t, configDir, scmTestAllowInstanceIdentityDACL(t))
	if err := os.WriteFile(opts.Paths.ConfigFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	scmTestSetDACL(t, opts.Paths.ConfigFile, fmt.Sprintf("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;WD)"))
	before := scmTestDACL(t, opts.Paths.ConfigFile)

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	if err := restrictInstanceState(opts, snapshot); err != nil {
		t.Fatalf("restrictInstanceState() = %v, want nil", err)
	}

	if after := scmTestDACL(t, opts.Paths.ConfigFile); after != before {
		t.Errorf("the access control list of the config file %q = %q, want %q", opts.Paths.ConfigFile, after, before)
	}
}

// TestSCMInstanceStateKeepsTheDescriptorOfTheReusedDirectories checks that
// the directories the installation reuses keep their descriptor.
//
// The cache root, the hub directory and the local output directory can hold
// the models of the user: the installation reads their descriptor and never
// changes it.
func TestSCMInstanceStateKeepsTheDescriptorOfTheReusedDirectories(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	// The reused directories existed before the installation. The
	// descriptor of every one of them is protected, so the inheritance
	// of the platform cannot change it behind the back of the test.
	reused := []string{opts.CacheDir, opts.HubDir, opts.LocalDir}
	before := make(map[string]string, len(reused))
	for _, dir := range reused {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("MkdirAll(%q) = %v, want nil", dir, err)
		}
		scmTestSetDACL(t, dir, scmTestAllowInstanceIdentityDACL(t))
		before[dir] = scmTestDACL(t, dir)
	}
	// The cache root lives in the state directory of the instance, so the
	// state directory existed before the installation as well: it has to
	// be usable by the account of the instance.
	scmTestSetDACL(t, opts.Paths.StateDir, scmTestAllowInstanceIdentityDACL(t))

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	if err := restrictInstanceState(opts, snapshot); err != nil {
		t.Fatalf("restrictInstanceState() = %v, want nil", err)
	}
	// The preflight reads the reused directories as well.
	if err := preflightDataPaths(opts); err != nil {
		t.Fatalf("preflightDataPaths() = %v, want nil", err)
	}

	for _, dir := range reused {
		if after := scmTestDACL(t, dir); after != before[dir] {
			t.Errorf("the access control list of the reused directory %q = %q, want %q", dir, after, before[dir])
		}
	}
}

// The layout of the structurally complete PE image the tests synthesize.
const (
	// scmTestDosHeaderSize is the size of the DOS header, the part of an
	// image the offset of the PE header is read from.
	scmTestDosHeaderSize = 64
	// scmTestPeHeaderOffset is the offset of the PE header in the
	// synthesized image.
	scmTestPeHeaderOffset       = 0x80
	scmTestOptionalHeaderSize   = 240
	scmTestOptionalHeader32Size = 224
	scmTestSectionHeaderSize    = 40
	scmTestSectionRawOffset     = 0x200
	scmTestSectionRawSize       = 0x200
)

// scmTestPEImage builds a structurally complete PE image with one section.
// It exercises header validation, not actual Windows loadability.
func scmTestPEImage(machine uint16) []byte {
	image := make([]byte, scmTestSectionRawOffset+scmTestSectionRawSize)
	optionalSize := scmTestOptionalHeaderSize
	magic := uint16(0x020B)
	if machine == pe.MachineI386 {
		optionalSize = scmTestOptionalHeader32Size
		magic = 0x010B
	}
	copy(image[0:], []byte("MZ"))
	binary.LittleEndian.PutUint32(image[0x3C:], scmTestPeHeaderOffset)
	copy(image[scmTestPeHeaderOffset:], []byte("PE\x00\x00"))
	binary.LittleEndian.PutUint16(image[scmTestPeHeaderOffset+4:], machine)
	binary.LittleEndian.PutUint16(image[scmTestPeHeaderOffset+4+2:], 1)
	binary.LittleEndian.PutUint16(image[scmTestPeHeaderOffset+4+16:], uint16(optionalSize))
	binary.LittleEndian.PutUint16(image[scmTestPeHeaderOffset+4+18:], 0x0002)
	optional := scmTestPeHeaderOffset + 24
	binary.LittleEndian.PutUint16(image[optional:], magic)
	binary.LittleEndian.PutUint32(image[optional+16:], 0x1000)
	section := optional + optionalSize
	copy(image[section:], []byte(".text"))
	binary.LittleEndian.PutUint32(image[section+8:], scmTestSectionRawSize)
	binary.LittleEndian.PutUint32(image[section+12:], 0x1000)
	binary.LittleEndian.PutUint32(image[section+16:], scmTestSectionRawSize)
	binary.LittleEndian.PutUint32(image[section+20:], scmTestSectionRawOffset)
	return image
}

// scmTestExecutable writes data as hfdesk.exe into a temporary directory and
// returns its path.
func scmTestExecutable(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hfdesk.exe")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	return path
}

// scmTestRefuseLocalDir injects an established AccessCheck denial without
// mutating the ACL of the local directory.
//
// The installation fails after creating its own directories, exercising that
// cleanup boundary before any live SCM operation.
func scmTestRefuseLocalDir(t *testing.T, opts InstallOptions) *scmTestPathAccess {
	t.Helper()
	if err := os.MkdirAll(opts.LocalDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", opts.LocalDir, err)
	}
	access := scmTestPathAccessFor(t, opts.LocalDir, false)
	previous := scmOpenPathAccess
	scmOpenPathAccess = func(uint32) (scmPathAccess, error) {
		return access, nil
	}
	t.Cleanup(func() { scmOpenPathAccess = previous })
	return access
}
