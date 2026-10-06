// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxNamespaceWork                = 100000
	maxNamespaceIdentityComparisons = 1000000
	namespaceBatch                  = 128
)

// ObserveNamespaceMemberships takes one bounded, ephemeral observation of all
// direct repository slots in configured managed namespaces. It enumerates
// before callers filter by repo ID and retains every logical root/role
// occurrence, even when directory identities compare equal.
//
// Local slots have unknown repository type. Their current owned entries are
// exposed for server-side weight qualification; no type is inferred from the
// requested ID or from a source label.
func (set *ManagedRootSet) ObserveNamespaceMemberships() ([]NamespaceMembership, error) {
	if err := set.ready(); err != nil {
		return nil, err
	}
	work := 0
	comparisons := 0
	charge := func(n int) error {
		work += n
		if work > maxNamespaceWork {
			return fmt.Errorf("managed namespace observation exceeds %d entries", maxNamespaceWork)
		}
		return nil
	}
	chargeComparisons := func(n int) error {
		comparisons += n
		if comparisons > maxNamespaceIdentityComparisons {
			return fmt.Errorf("managed namespace identity comparisons exceed %d", maxNamespaceIdentityComparisons)
		}
		return nil
	}
	var memberships []NamespaceMembership
	for _, root := range set.roots {
		if root.Roles&ManagedRootHub != 0 {
			entries, err := readNamespaceDir(root.AbsolutePath, &work)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read managed Hub namespace %q: %w", root.AbsolutePath, err)
			}
			for _, entry := range entries {
				owner, name, repoType, ok := parseHubRepoDirName(entry.name)
				if !ok {
					continue
				}
				isDir, dirErr := namespaceEntryIsDir(filepath.Join(root.AbsolutePath, entry.name), entry.info)
				if dirErr != nil {
					return nil, fmt.Errorf("inspect Hub repository entry %q: %w", entry.name, dirErr)
				}
				if !isDir {
					continue
				}
				membership, err := set.observeNamespaceSlot(root, PhysicalCopyHub, owner+"/"+name, repoType, true, filepath.Join(root.AbsolutePath, entry.name), entry.info, chargeComparisons, charge)
				if err != nil {
					return nil, err
				}
				memberships = append(memberships, membership)
			}
		}

		projectionRoles := root.Roles & (ManagedRootModelProjection | ManagedRootDatasetProjection)
		for _, typed := range []struct {
			role ManagedRootRole
			typ  RepoType
		}{{ManagedRootModelProjection, RepoTypeModel}, {ManagedRootDatasetProjection, RepoTypeDataset}} {
			if projectionRoles&typed.role == 0 {
				continue
			}
			projections, err := set.enumerateOwnerNameSlots(root, PhysicalCopyKind(0), typed.typ, true, &work, chargeComparisons, charge)
			if err != nil {
				return nil, err
			}
			memberships = append(memberships, projections...)
		}

		if root.Roles&ManagedRootLocal != 0 {
			locals, err := set.enumerateOwnerNameSlots(root, PhysicalCopyLocal, "", false, &work, chargeComparisons, charge)
			if err != nil {
				return nil, err
			}
			memberships = append(memberships, locals...)
		}
	}
	return memberships, nil
}

type namespaceEntry struct {
	name string
	info os.FileInfo
}

// readNamespaceDir reads in bounded batches and charges every encountered
// entry to the operation-wide budget. A partial read is never returned as a
// complete namespace.
func readNamespaceDir(path string, work *int) ([]namespaceEntry, error) {
	return readNamespaceDirExpected(path, nil, work)
}

func readNamespaceDirExpected(path string, expected os.FileInfo, work *int) ([]namespaceEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("identify namespace directory %q: %w", path, err)
	}
	if !openedInfo.IsDir() || !os.SameFile(openedInfo, openedInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("namespace path is not a verified directory: %q", path)
	}
	if expected != nil && !os.SameFile(openedInfo, expected) {
		_ = file.Close()
		return nil, fmt.Errorf("namespace directory changed during observation: %q", path)
	}
	var entries []namespaceEntry
	for {
		batch, readErr := file.ReadDir(namespaceBatch)
		for _, entry := range batch {
			*work++
			if *work > maxNamespaceWork {
				_ = file.Close()
				return nil, fmt.Errorf("managed namespace observation exceeds %d entries", maxNamespaceWork)
			}
			info, infoErr := entry.Info()
			if infoErr != nil {
				_ = file.Close()
				return nil, fmt.Errorf("identify namespace entry %q: %w", filepath.Join(path, entry.Name()), infoErr)
			}
			entries = append(entries, namespaceEntry{name: entry.Name(), info: info})
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("read namespace directory %q: %w", path, readErr)
		}
		if len(batch) == 0 {
			_ = file.Close()
			return nil, fmt.Errorf("read namespace directory %q made no progress without EOF", path)
		}
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close namespace directory %q: %w", path, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries, nil
}

func namespaceEntryIsDir(path string, info os.FileInfo) (bool, error) {
	if info.Mode()&os.ModeSymlink == 0 {
		return info.IsDir(), nil
	}
	target, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return target.IsDir(), nil
}

func (set *ManagedRootSet) enumerateOwnerNameSlots(root ManagedRoot, kind PhysicalCopyKind, repoType RepoType, typeKnown bool, work *int, chargeComparisons func(int) error, charge func(int) error) ([]NamespaceMembership, error) {
	owners, err := readNamespaceDir(root.AbsolutePath, work)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read managed namespace %q: %w", root.AbsolutePath, err)
	}
	var memberships []NamespaceMembership
	for _, ownerEntry := range owners {
		owner := ownerEntry.name
		if !IsValidModelName(owner + "/placeholder") {
			continue
		}
		ownerIsDir, dirErr := namespaceEntryIsDir(filepath.Join(root.AbsolutePath, owner), ownerEntry.info)
		if dirErr != nil {
			return nil, fmt.Errorf("inspect namespace owner %q: %w", owner, dirErr)
		}
		if !ownerIsDir {
			continue
		}
		if kind == PhysicalCopyLocal {
			// HF repository entries are typed Hub namespaces, not Local owner
			// directories, even when a configured root has both roles.
			if root.Roles&ManagedRootHub != 0 && isHubInternalLocalOwner(owner) {
				continue
			}
			if err := chargeComparisons(2 * len(set.roots)); err != nil {
				return nil, err
			}
			allowed, err := set.AllowsOwner(root.ID, owner)
			if err != nil {
				return nil, fmt.Errorf("qualify Local owner %q: %w", owner, err)
			}
			if !allowed {
				continue
			}
		}
		ownerPath := filepath.Join(root.AbsolutePath, owner)
		ownerLstat, err := os.Lstat(ownerPath)
		if err != nil || !os.SameFile(ownerLstat, ownerEntry.info) {
			if err == nil {
				err = fmt.Errorf("namespace owner changed during enumeration")
			}
			return nil, fmt.Errorf("reinspect managed namespace owner %q: %w", ownerPath, err)
		}
		ownerInfo, err := os.Stat(ownerPath)
		if err != nil {
			return nil, fmt.Errorf("identify managed namespace owner %q: %w", ownerPath, err)
		}
		repos, err := readNamespaceDirExpected(ownerPath, ownerInfo, work)
		if err != nil {
			return nil, fmt.Errorf("read managed namespace owner %q: %w", ownerPath, err)
		}
		for _, repoEntry := range repos {
			name := repoEntry.name
			if !IsValidModelName(owner + "/" + name) {
				continue
			}
			path := filepath.Join(ownerPath, name)
			repoIsDir, dirErr := namespaceEntryIsDir(path, repoEntry.info)
			if dirErr != nil {
				return nil, fmt.Errorf("inspect repository namespace entry %q: %w", path, dirErr)
			}
			if !repoIsDir {
				continue
			}
			membership, err := set.observeNamespaceSlot(root, kind, owner+"/"+name, repoType, typeKnown, path, repoEntry.info, chargeComparisons, charge)
			if err != nil {
				return nil, err
			}
			memberships = append(memberships, membership)
		}
	}
	return memberships, nil
}

func (set *ManagedRootSet) observeNamespaceSlot(root ManagedRoot, kind PhysicalCopyKind, repoID string, repoType RepoType, typeKnown bool, path string, listedInfo os.FileInfo, chargeComparisons func(int) error, charge func(int) error) (NamespaceMembership, error) {
	lstatInfo, err := os.Lstat(path)
	if err != nil {
		return NamespaceMembership{}, fmt.Errorf("inspect managed namespace slot %q: %w", path, err)
	}
	if !os.SameFile(lstatInfo, listedInfo) {
		return NamespaceMembership{}, fmt.Errorf("managed namespace slot changed during enumeration: %q", path)
	}
	if lstatInfo.Mode()&os.ModeSymlink != 0 {
		targetInfo, statErr := os.Stat(path)
		if statErr != nil {
			return NamespaceMembership{}, fmt.Errorf("resolve managed namespace alias %q: %w", path, statErr)
		}
		if !targetInfo.IsDir() {
			return NamespaceMembership{}, nil
		}
		if err := chargeComparisons(len(set.roots)); err != nil {
			return NamespaceMembership{}, err
		}
		owner, ownerErr := set.OwnerForPath(path)
		if ownerErr != nil {
			return NamespaceMembership{}, fmt.Errorf("resolve managed namespace alias owner %q: %w", path, ownerErr)
		}
		ownedPath := path
		ownedPathMatchObserved := kind == PhysicalCopyHub
		if kind != PhysicalCopyHub {
			parts := strings.Split(repoID, "/")
			if len(parts) != 2 {
				return NamespaceMembership{}, fmt.Errorf("invalid repository slot identity %q", repoID)
			}
			ownedPath = filepath.Join(owner.AbsolutePath, parts[0], parts[1])
			ownedInfo, ownedErr := os.Stat(ownedPath)
			if ownedErr != nil && !os.IsNotExist(ownedErr) {
				return NamespaceMembership{}, fmt.Errorf("inspect repository alias owner %q: %w", ownedPath, ownedErr)
			}
			if ownedErr == nil {
				if err := chargeComparisons(1); err != nil {
					return NamespaceMembership{}, err
				}
			}
			ownedPathMatchObserved = ownedErr == nil && ownedInfo.IsDir() && os.SameFile(targetInfo, ownedInfo)
			if !ownedPathMatchObserved {
				ownedPath = path
			}
		}
		if err := charge(1); err != nil {
			return NamespaceMembership{}, err
		}
		return NamespaceMembership{
			Root: root, OwnerRootID: owner.ID, Kind: kind, RepoID: repoID, RepoType: repoType,
			TypeKnown: typeKnown, OwnedPathMatchObserved: ownedPathMatchObserved, Path: path, OwnedPath: ownedPath, Directory: targetInfo,
			Entries: []NamespaceEntry{{Path: path, Info: lstatInfo}},
		}, nil
	}
	opened, err := set.effectObserver().openDir(path)
	if err != nil {
		return NamespaceMembership{}, fmt.Errorf("open managed namespace slot %q: %w", path, err)
	}
	rootInfo, statErr := opened.stat()
	closeErr := opened.close()
	if statErr != nil {
		return NamespaceMembership{}, fmt.Errorf("identify managed namespace slot %q: %w", path, statErr)
	}
	if closeErr != nil {
		return NamespaceMembership{}, fmt.Errorf("close managed namespace slot %q: %w", path, closeErr)
	}
	if !rootInfo.IsDir() || !os.SameFile(listedInfo, rootInfo) || !os.SameFile(rootInfo, rootInfo) {
		return NamespaceMembership{}, fmt.Errorf("managed namespace slot changed or has unknown identity: %q", path)
	}
	if chargeErr := chargeComparisons(len(set.roots)); chargeErr != nil {
		return NamespaceMembership{}, chargeErr
	}
	owner, err := set.OwnerForPath(path)
	if err != nil {
		return NamespaceMembership{}, fmt.Errorf("resolve managed namespace slot owner %q: %w", path, err)
	}
	ownedPath := path
	ownedPathMatchObserved := kind == PhysicalCopyHub
	if kind != PhysicalCopyHub {
		parts := strings.Split(repoID, "/")
		if len(parts) != 2 {
			return NamespaceMembership{}, fmt.Errorf("invalid repository slot identity %q", repoID)
		}
		ownedPath = filepath.Join(owner.AbsolutePath, parts[0], parts[1])
		ownedInfo, statErr := os.Stat(ownedPath)
		if statErr != nil && !os.IsNotExist(statErr) {
			return NamespaceMembership{}, fmt.Errorf("inspect owned repository slot %q: %w", ownedPath, statErr)
		}
		if statErr == nil {
			if err := chargeComparisons(1); err != nil {
				return NamespaceMembership{}, err
			}
		}
		ownedPathMatchObserved = statErr == nil && ownedInfo.IsDir() && os.SameFile(rootInfo, ownedInfo)
		if !ownedPathMatchObserved {
			ownedPath = path
		}
	}
	if err := chargeComparisons(len(set.roots)); err != nil {
		return NamespaceMembership{}, err
	}
	regions, entries, err := set.observeOwnedNamespaceEntries(root.ID, path, rootInfo, chargeComparisons, charge)
	if err != nil {
		return NamespaceMembership{}, fmt.Errorf("observe owned namespace slot %q: %w", path, err)
	}
	return NamespaceMembership{
		Root: root, OwnerRootID: owner.ID, Kind: kind, RepoID: repoID, RepoType: repoType,
		TypeKnown: typeKnown, OwnedPathMatchObserved: ownedPathMatchObserved, Path: path, OwnedPath: ownedPath, Directory: rootInfo,
		Regions: regions, Entries: entries,
	}, nil
}

func (set *ManagedRootSet) observeOwnedNamespaceEntries(rootID, rootPath string, expected os.FileInfo, chargeComparisons func(int) error, charge func(int) error) ([]NamespaceDirectoryRegion, []NamespaceEntry, error) {
	var regions []NamespaceDirectoryRegion
	var entries []NamespaceEntry
	seenRoot := false
	err := set.walkOwnedEntries(rootID, rootPath, expected, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filepath.Clean(path) == filepath.Clean(rootPath) {
			if !os.SameFile(info, expected) {
				return fmt.Errorf("managed namespace slot changed during observation: %q", path)
			}
			seenRoot = true
		}
		entries = append(entries, NamespaceEntry{Path: path, Info: info})
		if info.IsDir() {
			regions = append(regions, NamespaceDirectoryRegion{Path: path, Info: info})
		}
		return nil
	}, chargeComparisons, charge)
	if err != nil {
		return nil, nil, err
	}
	if seenRoot {
		return regions, entries, nil
	}
	// The candidate path belongs to a nested configured root, so the enclosing
	// root observes a distinct membership but no entries in that nested region.
	return nil, nil, nil
}

// NamespaceMembershipsFor filters only after complete managed-namespace
// enumeration. Local repository type remains unknown in this observation.
func NamespaceMembershipsFor(all []NamespaceMembership, repoID string, repoType RepoType) []NamespaceMembership {
	var matches []NamespaceMembership
	for _, membership := range all {
		if membership.RepoID != repoID || membership.TypeKnown && membership.RepoType != repoType {
			continue
		}
		matches = append(matches, membership)
	}
	return matches
}

func isHubInternalLocalOwner(owner string) bool {
	if validHubRepoDirName(owner) {
		return true
	}
	switch strings.ToLower(owner) {
	case "hub", "models", "datasets", "blobs", "snapshots", "refs":
		return true
	default:
		return false
	}
}
