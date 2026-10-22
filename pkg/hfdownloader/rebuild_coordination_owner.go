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
	// errCoordinationCarrierUnavailable marks the case where the per-account
	// private carrier cannot be established: the account is unknown, the
	// base directory cannot be created, or another account owns the path.
	//
	// The carrier is an advisory cross-process mechanism, so this condition
	// never fails the operation that asked for a lease. The caller keeps the
	// in-process half of the coordination and reports the loss of the
	// cross-process half.
	errCoordinationCarrierUnavailable = errors.New("HFDesk coordination carrier is unavailable")
)

// coordinationCarrierUnavailable marks err as the loss of the cross-process
// carrier rather than a refusal to adopt a directory this account owns.
func coordinationCarrierUnavailable(err error) error {
	return fmt.Errorf("%w: %w", errCoordinationCarrierUnavailable, err)
}

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
// the OS account and the account's private base directory.
//
// The identity is the account's platform-native id, so the app and the CLI of
// one account always resolve the same carrier and two accounts never share
// one. The anchor is the account's private base directory: the account record
// when the platform has one, and the environment home when it has none. A
// numeric container identity has no account record, so the environment home
// the container already sets is the anchor there. os.UserCacheDir and
// os.TempDir are intentionally not used because their documented platform
// behavior follows per-process environment overrides, which would let two
// processes of one account resolve two different carriers.
//
// The carrier is a direct child of that anchor, beside the account's own
// cache area instead of inside it. It is created by the account with mode
// 0700, so only that account reaches the lease objects, and its name is not
// a path an ordinary download can select.
//
// The anchor is never tightened by the carrier: an existing anchor keeps its
// own mode and its own owner, and a missing one is created with the ordinary
// directory mode. A container gives every account the same home - the shared
// data root - and a shared anchor that the first account tightened would lock
// the other accounts out of their own cache. The carrier therefore adds
// exactly one directory below the anchor and leaves every other directory of
// the anchor alone.
//
// A failure to name the account is reported as errCoordinationAccountUnavailable,
// which the caller turns into the loss of the cross-process carrier instead of
// a failed operation.
func defaultCoordinationBaseDir() (string, error) {
	account, err := resolveCoordinationAccount()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(account.ID))
	profile, err := filepath.Abs(filepath.Clean(account.Home))
	if err != nil {
		return "", fmt.Errorf("resolve current OS user profile path: %w", err)
	}
	return filepath.Join(profile, "hfdesk-coord-"+hex.EncodeToString(digest[:])), nil
}

// createPrivateCoordinationBase creates the carrier directory of one account.
//
// The carrier is created with mode 0700, so only that account reaches the
// lease objects inside it. Its parent is the account's private base
// directory, which the carrier never tightens: a missing parent is created
// with the ordinary directory mode, and an existing parent keeps its own mode
// and its own owner. That is what keeps a shared parent usable for the other
// accounts of the same machine, which is the normal shape of the container
// data root.
func createPrivateCoordinationBase(base string) error {
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return fmt.Errorf("create per-user coordination parent: %w", err)
	}
	if err := os.Mkdir(base, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create per-user coordination base: %w", err)
	}
	return nil
}

func privateCoordinationDirectoryPath() (string, error) {
	base, err := privateCoordinationBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, coordinationDirectoryName), nil
}

// ensurePrivateCoordinationDirectory bootstraps a per-OS-user coordinator
// below the account's private base directory. The owner record is published
// only after this process creates the directory; an existing unmarked
// directory is never adopted as the lock namespace.
//
// The carrier adds one private directory below the account's base directory
// and never tightens that base directory, so a shared base directory stays
// usable for the other accounts of the same machine.
//
// A carrier that cannot be established at all - an unknown account, an
// unusable base directory, or a path another account owns - is reported as
// errCoordinationCarrierUnavailable: the lease keeps its in-process half and
// the operation that asked for it continues. A directory this account owns
// but cannot prove is its own coordinator stays a refusal, so foreign state
// is never adopted.
func ensurePrivateCoordinationDirectory(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	base, err := privateCoordinationBaseDir()
	if err != nil {
		return "", coordinationCarrierUnavailable(fmt.Errorf("resolve per-user coordination base: %w", err))
	}
	if err := createPrivateCoordinationBase(base); err != nil {
		return "", coordinationCarrierUnavailable(err)
	}
	baseInfo, err := os.Lstat(base)
	if err != nil {
		return "", coordinationCarrierUnavailable(fmt.Errorf("inspect per-user coordination base: %w", err))
	}
	if !baseInfo.IsDir() || baseInfo.Mode()&os.ModeSymlink != 0 {
		return "", coordinationCarrierUnavailable(fmt.Errorf("refuse unsafe per-user coordination base %q", base))
	}
	if err := validatePrivateCoordinationDirectory(base, baseInfo); err != nil {
		return "", coordinationCarrierUnavailable(err)
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return "", coordinationCarrierUnavailable(fmt.Errorf("resolve per-user coordination base: %w", err))
	}
	dir := filepath.Join(base, coordinationDirectoryName)
	created := false
	if err := os.Mkdir(dir, 0o700); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return "", coordinationCarrierUnavailable(fmt.Errorf("create private coordination directory: %w", err))
	}
	if created {
		if err := writeCoordinationDirectoryOwner(dir); err != nil {
			return "", coordinationCarrierUnavailable(fmt.Errorf("establish private coordination ownership: %w", err))
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return "", coordinationCarrierUnavailable(fmt.Errorf("inspect private coordination directory: %w", err))
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", coordinationCarrierUnavailable(fmt.Errorf("refuse unsafe private coordination directory %q", dir))
		}
		if err := validatePrivateCoordinationDirectory(dir, info); err != nil {
			return "", coordinationCarrierUnavailable(err)
		}
		if _, err := readCoordinationDirectoryOwner(dir); err == nil {
			physical, err := filepath.EvalSymlinks(dir)
			if err != nil {
				return "", coordinationCarrierUnavailable(fmt.Errorf("resolve private coordination directory: %w", err))
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
