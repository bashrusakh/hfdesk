// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimecoordination"
)

const (
	coordinationDirectoryName = "coordination"
	coordinationOwnerFilename = ".hfdesk-coordination-owner.json"
	coordinationOwnerVersion  = 1
	coordinationLeaseVersion  = 1
)

var (
	errInvalidCoordinationOwnership = errors.New("invalid HFDesk coordination ownership")
	errCoordinationOwnerPending     = errors.New("HFDesk coordination owner is not published yet")
)

type coordinationDirectoryOwner struct {
	Version     int    `json:"version"`
	Purpose     string `json:"purpose"`
	UserID      string `json:"userId"`
	DirectoryID string `json:"directoryId"`
	FileID      string `json:"fileId"`
}

type coordinationLeaseRecord struct {
	Version int    `json:"version"`
	Purpose string `json:"purpose"`
	RootID  string `json:"rootId"`
}

func privateCoordinationBaseDir() (string, error) {
	return runtimecoordination.BaseDir(defaultCoordinationBaseDir)
}

// defaultCoordinationBaseDir derives a stable per-account storage root from
// the OS account record/token, not HOME, XDG, or temporary-directory
// environment variables. os.UserCacheDir and os.TempDir intentionally are not
// used here because their documented platform behavior follows those per-
// process environment overrides.
func defaultCoordinationBaseDir() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current OS user profile: %w", err)
	}
	if current.HomeDir == "" {
		return "", fmt.Errorf("current OS user has no profile directory")
	}
	userID, err := coordinationOSUserID()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(userID))
	profile, err := filepath.Abs(filepath.Clean(current.HomeDir))
	if err != nil {
		return "", fmt.Errorf("resolve current OS user profile path: %w", err)
	}
	var root string
	switch runtime.GOOS {
	case "darwin":
		root = filepath.Join(profile, "Library", "Caches")
	case "windows":
		root = filepath.Join(profile, "AppData", "Local")
	default:
		root = filepath.Join(profile, ".cache")
	}
	return filepath.Join(root, "hfdesk-coord-"+hex.EncodeToString(digest[:])), nil
}

func privateCoordinationDirectoryPath() (string, error) {
	base, err := privateCoordinationBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, coordinationDirectoryName), nil
}

// ensurePrivateCoordinationDirectory bootstraps a per-OS-user coordinator
// below the OS-reported profile's private cache area. The owner record is
// published only after this process creates the directory; an existing
// unmarked directory is never adopted as the lock namespace.
func ensurePrivateCoordinationDirectory(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	base, err := privateCoordinationBaseDir()
	if err != nil {
		return "", fmt.Errorf("resolve per-user coordination base: %w", err)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("create per-user coordination base: %w", err)
	}
	baseInfo, err := os.Lstat(base)
	if err != nil {
		return "", fmt.Errorf("inspect per-user coordination base: %w", err)
	}
	if !baseInfo.IsDir() || baseInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("refuse unsafe per-user coordination base %q", base)
	}
	if err := validatePrivateCoordinationDirectory(base, baseInfo); err != nil {
		return "", err
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve per-user coordination base: %w", err)
	}
	dir := filepath.Join(base, coordinationDirectoryName)
	created := false
	if err := os.Mkdir(dir, 0o700); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create private coordination directory: %w", err)
	}
	if created {
		if err := writeCoordinationDirectoryOwner(dir); err != nil {
			return "", fmt.Errorf("establish private coordination ownership: %w", err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return "", fmt.Errorf("inspect private coordination directory: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refuse unsafe private coordination directory %q", dir)
		}
		if err := validatePrivateCoordinationDirectory(dir, info); err != nil {
			return "", err
		}
		if _, err := readCoordinationDirectoryOwner(dir); err == nil {
			physical, err := filepath.EvalSymlinks(dir)
			if err != nil {
				return "", fmt.Errorf("resolve private coordination directory: %w", err)
			}
			return filepath.Clean(physical), nil
		} else if !errors.Is(err, errCoordinationOwnerPending) {
			return "", fmt.Errorf("refuse unowned private coordination directory %q: %w", dir, err)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("refuse unowned or incomplete private coordination directory %q", dir)
		}
		if err := waitForCoordinationRecord(ctx); err != nil {
			return "", err
		}
	}
}

func writeCoordinationDirectoryOwner(dir string) error {
	userID, err := coordinationOSUserID()
	if err != nil {
		return err
	}
	fileID, err := branchVariantFilesystemIdentity(dir)
	if err != nil {
		return err
	}
	directoryID, err := newBranchVariantStoreID()
	if err != nil {
		return err
	}
	owner := coordinationDirectoryOwner{
		Version: coordinationOwnerVersion, Purpose: "hfdesk-runtime-coordination",
		UserID: userID, DirectoryID: directoryID, FileID: fileID,
	}
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, coordinationOwnerFilename)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := file.Write(data)
	if err != nil {
		_ = file.Close()
		return err
	}
	if n != len(data) {
		_ = file.Close()
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func readCoordinationDirectoryOwner(dir string) (*coordinationDirectoryOwner, error) {
	path := filepath.Join(dir, coordinationOwnerFilename)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errCoordinationOwnerPending
		}
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: coordination owner entry is not a regular file", errInvalidCoordinationOwnership)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var owner coordinationDirectoryOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return nil, errCoordinationOwnerPending
	}
	userID, err := coordinationOSUserID()
	if err != nil {
		return nil, err
	}
	fileID, err := branchVariantFilesystemIdentity(dir)
	if err != nil {
		return nil, err
	}
	if owner.Version != coordinationOwnerVersion || owner.Purpose != "hfdesk-runtime-coordination" ||
		owner.UserID != userID || owner.DirectoryID == "" || owner.FileID != fileID {
		return nil, fmt.Errorf("%w: coordinator identity does not match its directory", errInvalidCoordinationOwnership)
	}
	return &owner, nil
}

func ownedCoordinationDirectory() (string, bool, error) {
	dir, err := privateCoordinationDirectoryPath()
	if err != nil {
		return "", false, err
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return dir, false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return dir, false, nil
	}
	if err := validatePrivateCoordinationDirectory(dir, info); err != nil {
		return dir, false, err
	}
	if _, err := readCoordinationDirectoryOwner(dir); err != nil {
		return dir, false, err
	}
	physical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false, err
	}
	return filepath.Clean(physical), true, nil
}

// rejectOwnedCoordinationMutationTargets protects exact file/directory effects
// inside the validated coordinator scope. A matching pathname elsewhere is
// ordinary data; the final symlink is resolved only for content-following
// effects.
func rejectOwnedCoordinationMutationTargets(followLeaf bool, targets ...string) error {
	scope, owned, ownerErr := ownedCoordinationDirectory()
	if !owned && ownerErr == nil {
		return nil
	}
	for _, target := range targets {
		if target == "" {
			continue
		}
		abs, err := filepath.Abs(filepath.Clean(target))
		if err != nil {
			return err
		}
		spellings := []string{abs}
		parent, parentErr := resolvedPathForCreate(filepath.Dir(abs))
		if parentErr == nil {
			spellings = append(spellings, filepath.Join(parent, filepath.Base(abs)))
		}
		if followLeaf {
			if physical, resolveErr := resolvedPathForCreate(abs); resolveErr == nil {
				spellings = append(spellings, physical)
			}
		}
		for _, spelling := range spellings {
			if ownerErr != nil {
				candidate, candidateErr := privateCoordinationDirectoryPath()
				if candidateErr == nil && coordinationPathContains(candidate, spelling) {
					return fmt.Errorf("cannot verify private coordination ownership for mutation target %q: %w", target, ownerErr)
				}
				continue
			}
			if coordinationPathContains(scope, spelling) {
				return fmt.Errorf("filesystem effect %q conflicts with the app-owned HFDesk coordination scope", target)
			}
		}
	}
	return nil
}

// rejectOwnedCoordinationTreeEffects preflights recursive effects. Unlike a
// leaf write, RemoveAll/copy-tree traversal is conflicting when its target is
// an ancestor of the actual owned coordination directory as well as when it
// is inside that directory.
func rejectOwnedCoordinationTreeEffects(target string) error {
	if target == "" {
		return nil
	}
	scope, owned, ownerErr := ownedCoordinationDirectory()
	if !owned && ownerErr == nil {
		return nil
	}
	targetPath, err := resolvedPathForCreate(target)
	if err != nil {
		if candidate, candidateErr := privateCoordinationDirectoryPath(); candidateErr == nil &&
			coordinationPathsOverlap(candidate, target) {
			return fmt.Errorf("cannot resolve recursive coordination effect %q: %w", target, err)
		}
		return nil
	}
	if ownerErr != nil {
		candidate, candidateErr := privateCoordinationDirectoryPath()
		if candidateErr == nil && coordinationPathsOverlap(candidate, targetPath) {
			return fmt.Errorf("cannot verify private coordination ownership for recursive effect %q: %w", target, ownerErr)
		}
		return nil
	}
	if coordinationPathsOverlap(scope, targetPath) {
		return fmt.Errorf("recursive filesystem effect %q conflicts with the app-owned HFDesk coordination scope", target)
	}
	return nil
}

func coordinationPathsOverlap(a, b string) bool {
	return coordinationPathContains(a, b) || coordinationPathContains(b, a)
}

func coordinationPathContains(root, target string) bool {
	root, errRoot := filepath.Abs(filepath.Clean(root))
	target, errTarget := filepath.Abs(filepath.Clean(target))
	if errRoot != nil || errTarget != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		root, target = strings.ToLower(root), strings.ToLower(target)
	}
	return PathInside(root, target)
}

func openRebuildCoordinationFile(ctx context.Context, path, rootID string, admission bool) (*os.File, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	purpose := "data"
	if admission {
		purpose = "admission"
	}
	expected := coordinationLeaseRecord{Version: coordinationLeaseVersion, Purpose: purpose, RootID: rootID}
	encoded, err := json.Marshal(expected)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr == nil {
			n, err := file.Write(encoded)
			if err != nil {
				_ = file.Close()
				return nil, err
			}
			if n != len(encoded) {
				_ = file.Close()
				return nil, io.ErrShortWrite
			}
			if err := file.Sync(); err != nil {
				_ = file.Close()
				return nil, err
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				_ = file.Close()
				return nil, err
			}
			return file, nil
		}
		if !errors.Is(createErr, os.ErrExist) {
			return nil, createErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refuse non-regular rebuild coordination object %q", path)
		}
		file, err = os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		opened, statErr := file.Stat()
		current, lstatErr := os.Lstat(path)
		if statErr != nil || lstatErr != nil || !current.Mode().IsRegular() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
			_ = file.Close()
			if statErr != nil {
				return nil, statErr
			}
			if lstatErr != nil {
				if errors.Is(lstatErr, os.ErrNotExist) {
					continue
				}
				return nil, lstatErr
			}
			return nil, fmt.Errorf("rebuild coordination object %q changed while opening", path)
		}
		var record coordinationLeaseRecord
		if opened.Size() == 0 || opened.Size() > 4096 {
			_ = file.Close()
			if opened.Size() == 0 && time.Now().Before(deadline) {
				if err := waitForCoordinationRecord(ctx); err != nil {
					return nil, err
				}
				continue
			}
			return nil, fmt.Errorf("refuse unowned rebuild coordination object %q", path)
		}
		data := make([]byte, opened.Size())
		n, readErr := file.ReadAt(data, 0)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			_ = file.Close()
			return nil, readErr
		}
		if n != len(data) || json.Unmarshal(data, &record) != nil {
			_ = file.Close()
			if time.Now().Before(deadline) {
				if err := waitForCoordinationRecord(ctx); err != nil {
					return nil, err
				}
				continue
			}
			return nil, fmt.Errorf("refuse unowned rebuild coordination object %q", path)
		}
		if record != expected {
			_ = file.Close()
			return nil, fmt.Errorf("refuse foreign rebuild coordination object %q", path)
		}
		return file, nil
	}
}

func waitForCoordinationRecord(ctx context.Context) error {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
