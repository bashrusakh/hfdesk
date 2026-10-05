// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// ManagedRootRole describes how a configured path participates in storage
// ownership. Roles are semantic; display labels and server configuration stay
// in the adapter that constructs a ManagedRootSet.
type ManagedRootRole uint32

const (
	ManagedRootBrowse            ManagedRootRole = 1 << iota // Read-only repository discovery.
	ManagedRootProtected                                     // Must be protected from enclosing whole-copy operations.
	ManagedRootLocal                                         // Root provides local owner/name repository candidates.
	ManagedRootHub                                           // Root is the selected HF models/datasets Hub directory.
	ManagedRootModelProjection                               // Root contains friendly model projections, not independent copies.
	ManagedRootDatasetProjection                             // Root contains friendly dataset projections, not independent copies.
)

// ManagedRootRestriction is an OR-merged restriction carried by roots that
// are proven to be the same physical directory for a read/discovery operation.
type ManagedRootRestriction uint32

const (
	ManagedRootSkipSpecial ManagedRootRestriction = 1 << iota // Exclude HF cache-internal owner names.
)

// ManagedRootSpec is one path/role contribution to an immutable root set.
type ManagedRootSpec struct {
	Path         string
	Roles        ManagedRootRole
	Restrictions ManagedRootRestriction
}

// ManagedRoot is a defensive-copy view of an immutable configured root.
// ID derives only from captured-base lexical identity; AbsolutePath is also
// lexical and is never EvalSymlinks-canonicalized.
type ManagedRoot struct {
	ID           string
	Path         string
	AbsolutePath string
	Roles        ManagedRootRole
	Restrictions ManagedRootRestriction
}

// ManagedRootGroup groups browse roots only when filesystem facts prove they
// are the same directory; Members preserves their distinct configured IDs.
type ManagedRootGroup struct {
	Root    ManagedRoot
	Members []ManagedRoot
}

// PhysicalCopyKind classifies an existing destructive-unit candidate.
type PhysicalCopyKind uint8

const (
	PhysicalCopyLocal PhysicalCopyKind = iota + 1 // A direct local owner/name directory.
	PhysicalCopyHub                               // An exact models--owner--name/datasets--owner--name directory.
)

// RepoPhysicalCopy describes an existing physical repository directory.
// RootIDs retains every configured lexical identity proven to refer to this
// same physical directory.
type RepoPhysicalCopy struct {
	Kind         PhysicalCopyKind
	Path         string
	OwnerRootID  string
	RootIDs      []string
	Restrictions ManagedRootRestriction
}

// RepoProjection describes a friendly-view directory. It is deliberately not
// a RepoPhysicalCopy and never grants delete-unit authority.
type RepoProjection struct {
	Path         string
	OwnerRootID  string
	RootIDs      []string
	Restrictions ManagedRootRestriction
}

// RepoPhysicalCopySet separates actual physical copies from friendly
// projections, which never grant deletion authority.
type RepoPhysicalCopySet struct {
	Copies      []RepoPhysicalCopy
	Projections []RepoProjection
}

// ManagedRootSet is an immutable snapshot of root definitions for one config
// generation. It stores no filesystem identity observations; every operation
// performs fresh Stat/SameFile checks and returns errors for unknown facts.
type ManagedRootSet struct {
	base  string
	roots []ManagedRoot
	err   error
	// observe is nil in production. Tests may provide coherent namespace
	// observations so bind-mount views can be modeled without mount privilege.
	observe effectObserver
}

// ConfiguredPath returns a cleaned lexical path resolved against an explicit
// captured base. It never resolves symlinks or changes case.
func ConfiguredPath(path, base string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	path = filepath.Clean(path)
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Clean(filepath.Join(base, path))
}

// ConfiguredPathIdentity compares cleaned lexical spellings against base. It
// does not infer case-folding rules from GOOS and does not resolve symlinks.
func ConfiguredPathIdentity(path, base string) string { return ConfiguredPath(path, base) }

// ManagedRootID returns a stable identifier derived only from lexical path
// identity and the captured base, independent of filesystem existence/order.
func ManagedRootID(path, base string) string {
	identity := ConfiguredPath(path, base)
	sum := sha256.Sum256([]byte(identity))
	return "root:" + hex.EncodeToString(sum[:12])
}

// CleanConfiguredPathList trims, cleans, and lexically de-duplicates paths
// against a captured base while preserving the first configured spelling.
func CleanConfiguredPathList(paths []string, base string) []string {
	var cleaned []string
	seen := make(map[string]bool)
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		key := ConfiguredPathIdentity(path, base)
		if seen[key] {
			continue
		}
		seen[key] = true
		cleaned = append(cleaned, path)
	}
	return cleaned
}

// NewManagedRootSet builds an immutable root-definition snapshot. Invalid or
// uncaptured bases are retained as errors returned by the operation methods.
func NewManagedRootSet(base string, specs []ManagedRootSpec) *ManagedRootSet {
	set := &ManagedRootSet{base: filepath.Clean(base)}
	if base == "" || !filepath.IsAbs(base) {
		set.err = errors.New("managed-root base must be an absolute captured path")
		return set
	}
	byID := make(map[string]int)
	for _, spec := range specs {
		if strings.TrimSpace(spec.Path) == "" {
			continue
		}
		path := filepath.Clean(spec.Path)
		id := ManagedRootID(path, set.base)
		if index, found := byID[id]; found {
			set.roots[index].Roles |= spec.Roles
			set.roots[index].Restrictions |= spec.Restrictions
			continue
		}
		byID[id] = len(set.roots)
		set.roots = append(set.roots, ManagedRoot{
			ID:           id,
			Path:         path,
			AbsolutePath: ConfiguredPath(path, set.base),
			Roles:        spec.Roles,
			Restrictions: spec.Restrictions,
		})
	}
	return set
}

// BasePath returns the captured lexical base used for relative root definitions.
func (set *ManagedRootSet) BasePath() string { return set.base }

// Err reports a constructor/base error retained by the immutable set.
func (set *ManagedRootSet) Err() error { return set.err }

// Roots returns defensive copies of the immutable root definitions.
func (set *ManagedRootSet) Roots() []ManagedRoot {
	if set.err != nil {
		return nil
	}
	return append([]ManagedRoot(nil), set.roots...)
}

// Root returns one defensive copy of a configured root by stable lexical ID.
func (set *ManagedRootSet) Root(rootID string) (ManagedRoot, bool) {
	for _, root := range set.roots {
		if root.ID == rootID {
			return root, true
		}
	}
	return ManagedRoot{}, false
}

// AllowsOwner applies static and freshly observed same-object restrictions to
// one owner entry. Unreadable alias facts are errors, not assumed distinct.
func (set *ManagedRootSet) AllowsOwner(rootID, owner string) (bool, error) {
	if err := set.ready(); err != nil {
		return false, err
	}
	root, ok := set.Root(rootID)
	if !ok {
		return false, fmt.Errorf("unknown managed root %q", rootID)
	}
	restrictions := root.Restrictions
	rootInfo, rootErr := os.Stat(root.AbsolutePath)
	if rootErr != nil && !os.IsNotExist(rootErr) {
		return false, fmt.Errorf("inspect managed root %q: %w", root.AbsolutePath, rootErr)
	}
	if rootErr == nil {
		for _, candidate := range set.roots {
			if candidate.ID == root.ID {
				continue
			}
			candidateInfo, candidateErr := os.Stat(candidate.AbsolutePath)
			if candidateErr != nil && !os.IsNotExist(candidateErr) {
				return false, fmt.Errorf("inspect managed root %q: %w", candidate.AbsolutePath, candidateErr)
			}
			if candidateErr == nil && os.SameFile(rootInfo, candidateInfo) {
				restrictions |= candidate.Restrictions
			}
		}
	}
	if restrictions&ManagedRootSkipSpecial != 0 && isHubSpecialOwner(owner) {
		return false, nil
	}
	return true, nil
}

func (set *ManagedRootSet) ready() error {
	if set == nil {
		return errors.New("managed-root set is nil")
	}
	return set.err
}

// withinManagedRoot tests cleaned configured lexical identity with exact
// component spelling. It does not establish physical containment when a path
// component is a symlink; mutation callers must use the separate physical and
// symlink checks below.
func withinManagedRoot(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if filepath.VolumeName(root) != filepath.VolumeName(path) {
		return false
	}
	rootTail := strings.TrimPrefix(root, filepath.VolumeName(root))
	pathTail := strings.TrimPrefix(path, filepath.VolumeName(path))
	rootParts := strings.Split(strings.Trim(rootTail, string(filepath.Separator)), string(filepath.Separator))
	pathParts := strings.Split(strings.Trim(pathTail, string(filepath.Separator)), string(filepath.Separator))
	if len(rootParts) == 1 && rootParts[0] == "" {
		rootParts = nil
	}
	if len(pathParts) == 1 && pathParts[0] == "" {
		pathParts = nil
	}
	if len(pathParts) < len(rootParts) {
		return false
	}
	for i := range rootParts {
		if rootParts[i] != pathParts[i] {
			return false
		}
	}
	return true
}

// containmentDistance first recognizes configured lexical ancestry, then
// compares freshly resolved physical namespaces. Configured spellings remain
// unchanged; resolved names are observations only.
func containmentDistance(parent, target string) (bool, int, error) {
	if withinManagedRoot(parent, target) {
		rel, _ := filepath.Rel(parent, target)
		if rel == "." {
			return true, 0, nil
		}
		return true, strings.Count(filepath.Clean(rel), string(filepath.Separator)) + 1, nil
	}
	resolvedParent, _, err := resolveObservedPath(parent)
	if err != nil {
		return false, 0, fmt.Errorf("resolve configured root %q: %w", parent, err)
	}
	resolvedTarget, _, err := resolveObservedPath(target)
	if err != nil {
		return false, 0, fmt.Errorf("resolve target path %q: %w", target, err)
	}
	if withinManagedRoot(resolvedParent, resolvedTarget) {
		rel, _ := filepath.Rel(resolvedParent, resolvedTarget)
		if rel == "." {
			return true, 0, nil
		}
		return true, strings.Count(filepath.Clean(rel), string(filepath.Separator)) + 1, nil
	}
	parentInfo, parentErr := os.Stat(resolvedParent)
	if parentErr == nil {
		for current, distance := filepath.Clean(resolvedTarget), 0; ; current, distance = filepath.Dir(current), distance+1 {
			info, statErr := os.Stat(current)
			if statErr == nil && os.SameFile(parentInfo, info) {
				return true, distance, nil
			}
			if statErr != nil && !os.IsNotExist(statErr) {
				return false, 0, fmt.Errorf("stat resolved path ancestor %q: %w", current, statErr)
			}
			if filepath.Dir(current) == current {
				break
			}
		}
	} else if !os.IsNotExist(parentErr) {
		return false, 0, fmt.Errorf("stat resolved configured root %q: %w", resolvedParent, parentErr)
	}
	return false, 0, nil
}

// resolveObservedPath resolves symlinked existing prefixes and retains a
// verified ordinary missing suffix. It never changes configured identity.
func resolveObservedPath(path string) (string, bool, error) {
	path = filepath.Clean(path)
	var suffix []string
	for current := path; ; current = filepath.Dir(current) {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			info, statErr := os.Stat(resolved)
			if statErr != nil {
				return "", false, statErr
			}
			if !info.IsDir() && len(suffix) != 0 {
				return "", false, fmt.Errorf("non-directory path prefix %q", current)
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), len(suffix) == 0, nil
		}
		if !os.IsNotExist(err) {
			return "", false, err
		}
		if info, lstatErr := os.Lstat(current); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", false, fmt.Errorf("unresolved symlink %q", current)
		} else if lstatErr != nil && !os.IsNotExist(lstatErr) {
			return "", false, lstatErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false, err
		}
		suffix = append(suffix, filepath.Base(current))
	}
}

// OwnerForPath selects the most-specific configured root using lexical
// containment or fresh filesystem ancestry evidence.
func (set *ManagedRootSet) OwnerForPath(path string) (ManagedRoot, error) {
	if err := set.ready(); err != nil {
		return ManagedRoot{}, err
	}
	target := ConfiguredPath(path, set.base)
	owner, distance := -1, int(^uint(0)>>1)
	for i, root := range set.roots {
		inside, candidateDistance, err := ownerContainmentDistance(root.AbsolutePath, target)
		if err != nil {
			return ManagedRoot{}, err
		}
		if inside && candidateDistance < distance {
			owner, distance = i, candidateDistance
		}
	}
	if owner < 0 {
		return ManagedRoot{}, os.ErrNotExist
	}
	return set.roots[owner], nil
}

// A symlinked leaf remains owned by its configured lexical parent for
// discovery. Resolving that leaf would incorrectly transfer a friendly/local
// alias of a Hub repository to the Hub root; parent-component aliases are
// still resolved so nested configured roots retain ownership.
func ownerContainmentDistance(parent, target string) (bool, int, error) {
	if withinManagedRoot(parent, target) {
		return containmentDistance(parent, target)
	}
	info, err := os.Lstat(target)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return false, 0, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return false, 0, fmt.Errorf("inspect ownership target %q: %w", target, err)
	}
	return containmentDistance(parent, target)
}

// NestedProtectedRoots returns protected roots below rootID. Unknown ancestry
// is returned as an error rather than guessed.
func (set *ManagedRootSet) NestedProtectedRoots(rootID string) ([]ManagedRoot, error) {
	if err := set.ready(); err != nil {
		return nil, err
	}
	root, ok := set.Root(rootID)
	if !ok {
		return nil, fmt.Errorf("unknown managed root %q", rootID)
	}
	rootInfo, rootErr := os.Stat(root.AbsolutePath)
	if rootErr == nil && rootInfo.IsDir() {
		reached, err := set.observeEffectDirectories(root.AbsolutePath)
		if err != nil {
			return nil, fmt.Errorf("cannot establish nested protected-root reachability: %w", err)
		}
		if len(reached) == 0 || !os.SameFile(rootInfo, reached[0].info) {
			return nil, fmt.Errorf("managed root changed during nested-root observation %q", root.AbsolutePath)
		}
		var nested []ManagedRoot
		comparisonBudget := maxEffectDirectories
		for _, candidate := range set.roots {
			if candidate.ID == root.ID || candidate.Roles&ManagedRootProtected == 0 {
				continue
			}
			fact, err := protectedDirectoryFact(candidate, set.effectObserver())
			if err != nil {
				return nil, err
			}
			if fact.existing && os.SameFile(reached[0].info, fact.info) {
				continue
			}
			intersects, err := effectIntersectsProtected(reached, fact, &comparisonBudget)
			if err != nil {
				return nil, err
			}
			if intersects {
				nested = append(nested, candidate)
			}
		}
		sort.Slice(nested, func(i, j int) bool { return nested[i].ID < nested[j].ID })
		return nested, nil
	}
	if rootErr != nil && !os.IsNotExist(rootErr) {
		return nil, fmt.Errorf("inspect managed root %q: %w", root.AbsolutePath, rootErr)
	}
	if rootErr == nil && !rootInfo.IsDir() {
		return nil, fmt.Errorf("managed root is not a directory %q", root.AbsolutePath)
	}
	if errors.Is(rootErr, syscall.ENOTDIR) {
		return nil, fmt.Errorf("managed root has a non-directory path component %q", root.AbsolutePath)
	}
	// A not-yet-created read-only root has no directory graph to enumerate.
	// Retain the existing lexical/missing-suffix behavior for this case.
	var nested []ManagedRoot
	for _, candidate := range set.roots {
		if candidate.ID == root.ID || candidate.Roles&ManagedRootProtected == 0 {
			continue
		}
		inside, _, err := containmentDistance(root.AbsolutePath, candidate.AbsolutePath)
		if err != nil {
			return nil, err
		}
		if !inside {
			continue
		}
		rootInfo, rootErr := os.Stat(root.AbsolutePath)
		candidateInfo, candidateErr := os.Stat(candidate.AbsolutePath)
		if rootErr != nil && !os.IsNotExist(rootErr) {
			return nil, fmt.Errorf("inspect managed root %q: %w", root.AbsolutePath, rootErr)
		}
		if candidateErr != nil && !os.IsNotExist(candidateErr) {
			return nil, fmt.Errorf("inspect protected root %q: %w", candidate.AbsolutePath, candidateErr)
		}
		if rootErr == nil && candidateErr == nil && os.SameFile(rootInfo, candidateInfo) {
			continue
		}
		nested = append(nested, candidate)
	}
	sort.Slice(nested, func(i, j int) bool { return nested[i].ID < nested[j].ID })
	return nested, nil
}

// Resolve returns an exact owner/name path beneath a browsable configured root.
// It does not grant destructive authority.
func (set *ManagedRootSet) Resolve(rootID, owner, name string) (string, error) {
	if err := set.ready(); err != nil {
		return "", err
	}
	root, ok := set.Root(rootID)
	if !ok || root.Roles&ManagedRootBrowse == 0 || !IsValidRepoComponent(owner) || !IsValidRepoComponent(name) {
		return "", errors.New("invalid managed-root repository path")
	}
	return filepath.Join(root.AbsolutePath, owner, name), nil
}

// WalkOwned walks only entries owned by rootID, skipping nested roots and
// returning errors when physical ownership cannot be established.
func (set *ManagedRootSet) WalkOwned(rootID, dir string, fn filepath.WalkFunc) error {
	if err := set.ready(); err != nil {
		return err
	}
	root, ok := set.Root(rootID)
	if !ok {
		return fmt.Errorf("unknown managed root %q", rootID)
	}
	return filepath.Walk(ConfiguredPath(dir, set.base), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		owner, ownerErr := set.OwnerForPath(path)
		if ownerErr != nil {
			return ownerErr
		}
		if owner.ID != root.ID {
			same, err := set.SameDirectoryObjectFacts(owner.AbsolutePath, root.AbsolutePath)
			if err != nil {
				return err
			}
			if !same {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		return fn(path, info, nil)
	})
}

// SameDirectoryObjectFacts distinguishes known inequality from missing or
// unreadable physical facts.
func (set *ManagedRootSet) SameDirectoryObjectFacts(left, right string) (bool, error) {
	return SameDirectoryPath(ConfiguredPath(left, set.base), ConfiguredPath(right, set.base))
}

// SameFilePaths compares existing objects using filesystem facts. Missing or
// unreadable paths return an error rather than being treated as distinct.
func SameFilePaths(left, right string) (bool, error) {
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false, fmt.Errorf("stat %q: %w", left, err)
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		return false, fmt.Errorf("stat %q: %w", right, err)
	}
	return os.SameFile(leftInfo, rightInfo), nil
}

// SameDirectoryPath reports equality only when current filesystem facts prove
// both existing paths are directories referring to the same object. Missing
// or unreadable paths are errors, not assumed distinct.
func SameDirectoryPath(left, right string) (bool, error) {
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false, fmt.Errorf("stat directory %q: %w", left, err)
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		return false, fmt.Errorf("stat directory %q: %w", right, err)
	}
	if !leftInfo.IsDir() || !rightInfo.IsDir() {
		return false, nil
	}
	return os.SameFile(leftInfo, rightInfo), nil
}

// BrowseRootGroups merges same-object browse aliases for discovery, retaining
// lexical member IDs and OR-merging restrictions only on proven equality.
func (set *ManagedRootSet) BrowseRootGroups() ([]ManagedRootGroup, error) {
	if err := set.ready(); err != nil {
		return nil, err
	}
	var groups []ManagedRootGroup
	for _, candidate := range set.roots {
		if candidate.Roles&ManagedRootBrowse == 0 {
			continue
		}
		group := -1
		for i, existing := range groups {
			if existing.Root.ID == candidate.ID {
				group = i
				break
			}
			same, err := SameDirectoryPath(existing.Root.AbsolutePath, candidate.AbsolutePath)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			if err == nil && same {
				group = i
				break
			}
		}
		if group < 0 {
			groups = append(groups, ManagedRootGroup{Root: candidate, Members: []ManagedRoot{candidate}})
			continue
		}
		groups[group].Root.Roles |= candidate.Roles
		groups[group].Root.Restrictions |= candidate.Restrictions
		groups[group].Members = append(groups[group].Members, candidate)
	}
	return groups, nil
}

// WholeCopyAllowed performs non-mutating exact-shape, owner, nested-root, and
// symlink-component checks for a whole physical copy.
func (set *ManagedRootSet) WholeCopyAllowed(rootID, target string) error {
	if err := set.ready(); err != nil {
		return err
	}
	root, ok := set.Root(rootID)
	if !ok {
		return fmt.Errorf("unknown managed root %q", rootID)
	}
	if root.Roles&ManagedRootHub == 0 && root.Roles&(ManagedRootModelProjection|ManagedRootDatasetProjection) != 0 {
		return errors.New("friendly projection is not an independent physical-copy unit")
	}
	target = ConfiguredPath(target, set.base)
	rel, err := filepath.Rel(root.AbsolutePath, target)
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("whole-copy target is not below its configured root")
	}
	parts := strings.Split(rel, string(filepath.Separator))
	repoOwner, repoName := "", ""
	if root.Roles&ManagedRootHub != 0 {
		var ok bool
		repoOwner, repoName, _, ok = parseHubRepoDirName(parts[0])
		if len(parts) != 1 || !ok {
			return errors.New("whole-copy target is not an exact Hub repository entry")
		}
	} else {
		if len(parts) != 2 || !IsValidRepoComponent(parts[0]) || !IsValidRepoComponent(parts[1]) {
			return errors.New("whole-copy target is not an exact owner/name repository entry")
		}
		repoOwner, repoName = parts[0], parts[1]
	}
	targetInfo, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("cannot inspect whole-copy target: %w", err)
	}
	if !targetInfo.IsDir() || targetInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("whole-copy target is not a real directory")
	}
	if root.Roles&ManagedRootHub == 0 {
		for _, projection := range set.roots {
			if projection.Roles&(ManagedRootModelProjection|ManagedRootDatasetProjection) == 0 {
				continue
			}
			projectionPath := filepath.Join(projection.AbsolutePath, parts[0], parts[1])
			projectionInfo, projectionErr := os.Stat(projectionPath)
			if projectionErr != nil && !os.IsNotExist(projectionErr) {
				return fmt.Errorf("cannot inspect friendly projection %q: %w", projectionPath, projectionErr)
			}
			if projectionErr == nil && os.SameFile(projectionInfo, targetInfo) {
				return errors.New("target is a friendly projection, not an independent physical copy")
			}
		}
		copies, err := set.RepoPhysicalCopies(repoOwner+"/"+repoName, RepoTypeModel)
		if err != nil {
			return fmt.Errorf("cannot establish physical-copy identity: %w", err)
		}
		for _, candidate := range copies.Copies {
			if candidate.Kind != PhysicalCopyHub || !hasRootID(candidate.RootIDs, rootID) {
				continue
			}
			if candidateInfo, statErr := os.Stat(candidate.Path); statErr == nil && os.SameFile(candidateInfo, targetInfo) {
				return errors.New("target is an alias of the selected Hub physical copy")
			} else if statErr != nil && !os.IsNotExist(statErr) {
				return fmt.Errorf("cannot inspect selected Hub copy %q: %w", candidate.Path, statErr)
			}
		}
	} else {
		projectionRole := ManagedRootModelProjection
		if strings.HasPrefix(parts[0], "datasets--") {
			projectionRole = ManagedRootDatasetProjection
		}
		for _, projection := range set.roots {
			if projection.Roles&projectionRole == 0 {
				continue
			}
			projectionPath := filepath.Join(projection.AbsolutePath, repoOwner, repoName)
			projectionInfo, projectionErr := os.Stat(projectionPath)
			if projectionErr != nil && !os.IsNotExist(projectionErr) {
				return fmt.Errorf("cannot inspect friendly projection %q: %w", projectionPath, projectionErr)
			}
			if projectionErr == nil && os.SameFile(projectionInfo, targetInfo) {
				return errors.New("Hub target aliases a friendly projection path")
			}
		}
	}
	owner, err := set.OwnerForPath(target)
	if err != nil {
		return fmt.Errorf("cannot establish managed-root owner: %w", err)
	}
	if owner.ID != root.ID {
		same, err := set.SameDirectoryObjectFacts(owner.AbsolutePath, root.AbsolutePath)
		if err != nil {
			return fmt.Errorf("cannot verify physical managed-root owner: %w", err)
		}
		if !same {
			return fmt.Errorf("target is owned by managed root %s", owner.ID)
		}
	}
	if err := rejectManagedSymlinkComponents(set.base, root.AbsolutePath, target); err != nil {
		return err
	}
	if err := set.checkProtectedEffect(target); err != nil {
		return err
	}
	return nil
}

// LegacyHFDeleteAllowed preflights the complete existing whole-HF deletion
// unit before either its Hub copy or optional friendly subtree is removed.
func (set *ManagedRootSet) LegacyHFDeleteAllowed(hubRootID, hubTarget, friendlyTarget, cacheRoot string) error {
	if err := set.WholeCopyAllowed(hubRootID, hubTarget); err != nil {
		return err
	}
	if friendlyTarget == "" {
		return nil
	}
	owner, name, repoType, ok := parseHubRepoDirName(filepath.Base(hubTarget))
	if !ok {
		return errors.New("cannot establish friendly repository identity")
	}
	role := ManagedRootModelProjection
	if repoType == RepoTypeDataset {
		role = ManagedRootDatasetProjection
	}
	exists, err := ValidateLegacyFriendlyEffect(cacheRoot, friendlyTarget)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	for _, projection := range set.roots {
		if projection.Roles&role == 0 {
			continue
		}
		rel, relErr := filepath.Rel(projection.AbsolutePath, friendlyTarget)
		if relErr != nil || rel != filepath.Join(owner, name) {
			continue
		}
		return set.checkProtectedEffect(friendlyTarget)
	}
	return errors.New("friendly deletion effect has no configured projection owner")
}

// ValidateLegacyFriendlyEffect verifies the existing legacy friendly path
// without following symlinked components. A missing ordinary component is a
// safely absent effect; errors and non-directory prefixes are unknown/refused.
func ValidateLegacyFriendlyEffect(cacheRoot, friendlyTarget string) (bool, error) {
	if !filepath.IsAbs(cacheRoot) || !filepath.IsAbs(friendlyTarget) {
		return false, errors.New("friendly deletion paths must be absolute")
	}
	root := filepath.Clean(cacheRoot)
	target := filepath.Clean(friendlyTarget)
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, errors.New("friendly deletion effect is outside its cache root")
	}
	volumeRoot := filepath.VolumeName(target) + string(filepath.Separator)
	components, err := filepath.Rel(volumeRoot, target)
	if err != nil {
		return false, fmt.Errorf("resolve friendly deletion components: %w", err)
	}
	current := volumeRoot
	parts := strings.Split(components, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			return false, nil
		}
		if statErr != nil {
			return false, fmt.Errorf("inspect friendly deletion component %q: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("friendly deletion path contains symlinked component %q", current)
		}
		if i != len(parts)-1 && !info.IsDir() {
			return false, fmt.Errorf("friendly deletion path has non-directory component %q", current)
		}
		if i == len(parts)-1 && !info.IsDir() {
			return false, errors.New("friendly deletion effect is not a real directory")
		}
	}
	return true, nil
}

func (set *ManagedRootSet) checkProtectedEffect(effect string) error {
	// Keep positive namespace-ancestor evidence as an inexpensive conservative
	// rejection, but never use a negative result as proof of disjointness.
	for _, protected := range set.roots {
		if protected.Roles&ManagedRootProtected == 0 {
			continue
		}
		contains, _, err := containmentDistance(effect, protected.AbsolutePath)
		if err != nil {
			return fmt.Errorf("cannot verify protected-root boundary: %w", err)
		}
		containedBy, distance, err := containmentDistance(protected.AbsolutePath, effect)
		if err != nil {
			return fmt.Errorf("cannot verify protected-root boundary: %w", err)
		}
		if contains || (containedBy && distance == 0) {
			return fmt.Errorf("deletion effect intersects protected managed root %s", protected.ID)
		}
	}
	reached, err := set.observeEffectDirectories(effect)
	if err != nil {
		return fmt.Errorf("cannot prove protected-root disjointness for deletion effect: %w", err)
	}
	comparisonBudget := maxEffectDirectories
	for _, protected := range set.roots {
		if protected.Roles&ManagedRootProtected == 0 {
			continue
		}
		fact, err := protectedDirectoryFact(protected, set.effectObserver())
		if err != nil {
			return fmt.Errorf("cannot establish protected-root identity %s: %w", protected.ID, err)
		}
		intersects, err := effectIntersectsProtected(reached, fact, &comparisonBudget)
		if err != nil {
			return fmt.Errorf("cannot prove protected-root disjointness for deletion effect: %w", err)
		}
		if intersects {
			return fmt.Errorf("deletion effect intersects protected managed root %s", protected.ID)
		}
	}
	return nil
}

func rejectManagedSymlinkComponents(base, rootPath, target string) error {
	inside, _, err := containmentDistance(rootPath, target)
	if err != nil {
		return err
	}
	if !inside {
		return errors.New("target is outside its configured root")
	}
	if _, err := filepath.Rel(rootPath, target); err != nil {
		return errors.New("target is not a direct managed entry")
	}
	volumeRoot := filepath.VolumeName(target) + string(filepath.Separator)
	rel, err := filepath.Rel(volumeRoot, target)
	if err != nil {
		return fmt.Errorf("resolve managed path components: %w", err)
	}
	current := volumeRoot
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return fmt.Errorf("inspect managed path %q: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinked managed path component %q (root %q, target %q, base %q)", current, rootPath, target, base)
		}
	}
	return nil
}

// IsValidRepoComponent accepts the server's safe ASCII owner/name component
// alphabet and rejects traversal/separator forms.
func IsValidRepoComponent(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func validHubRepoDirName(name string) bool {
	_, _, _, ok := parseHubRepoDirName(name)
	return ok
}

func parseHubRepoDirName(name string) (owner, repo string, repoType RepoType, ok bool) {
	for _, prefix := range []string{"models--", "datasets--"} {
		if strings.HasPrefix(name, prefix) {
			owner, repo, found := strings.Cut(strings.TrimPrefix(name, prefix), "--")
			repoType = RepoTypeModel
			if prefix == "datasets--" {
				repoType = RepoTypeDataset
			}
			ok = found && IsValidRepoComponent(owner) && IsValidRepoComponent(repo)
			return owner, repo, repoType, ok
		}
	}
	return "", "", "", false
}

func hasRootID(rootIDs []string, wanted string) bool {
	for _, rootID := range rootIDs {
		if rootID == wanted {
			return true
		}
	}
	return false
}

type physicalCandidate struct {
	copy RepoPhysicalCopy
	info os.FileInfo
}

type projectionCandidate struct {
	projection RepoProjection
	info       os.FileInfo
}

// RepoPhysicalCopies enumerates existing physical candidates for a repository.
// Proven local/Hub aliases are grouped in Copies with all root IDs and merged
// restrictions; friendly directories remain in the separate Projections list
// unless the filesystem proves they are the same directory object.
func (set *ManagedRootSet) RepoPhysicalCopies(repoID string, repoType RepoType) (RepoPhysicalCopySet, error) {
	if err := set.ready(); err != nil {
		return RepoPhysicalCopySet{}, err
	}
	parts := strings.Split(repoID, "/")
	if len(parts) != 2 || !safeRepoPathComponent(parts[0]) || !safeRepoPathComponent(parts[1]) {
		return RepoPhysicalCopySet{}, errors.New("invalid owner/name repository ID")
	}
	if repoType != RepoTypeModel && repoType != RepoTypeDataset {
		return RepoPhysicalCopySet{}, errors.New("invalid repository type")
	}
	var local, hubs []physicalCandidate
	var projections []projectionCandidate
	for _, root := range set.roots {
		var kind PhysicalCopyKind
		var path string
		isProjection := false
		switch {
		case root.Roles&ManagedRootHub != 0:
			prefix := "models--"
			if repoType == RepoTypeDataset {
				prefix = "datasets--"
			}
			kind = PhysicalCopyHub
			path = filepath.Join(root.AbsolutePath, prefix+parts[0]+"--"+parts[1])
		case repoType == RepoTypeModel && root.Roles&ManagedRootModelProjection != 0,
			repoType == RepoTypeDataset && root.Roles&ManagedRootDatasetProjection != 0:
			isProjection = true
			path = filepath.Join(root.AbsolutePath, parts[0], parts[1])
		case root.Roles&ManagedRootLocal != 0 && root.Roles&(ManagedRootModelProjection|ManagedRootDatasetProjection) == 0:
			allowed, ownerErr := set.AllowsOwner(root.ID, parts[0])
			if ownerErr != nil {
				return RepoPhysicalCopySet{}, ownerErr
			}
			if !allowed {
				continue
			}
			kind = PhysicalCopyLocal
			path = filepath.Join(root.AbsolutePath, parts[0], parts[1])
		default:
			continue
		}
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return RepoPhysicalCopySet{}, fmt.Errorf("inspect repository candidate %q: %w", path, err)
		}
		if !info.IsDir() {
			continue
		}
		owner, err := set.OwnerForPath(path)
		if err != nil {
			return RepoPhysicalCopySet{}, fmt.Errorf("resolve repository candidate owner %q: %w", path, err)
		}
		if kind != PhysicalCopyHub {
			ownedPath := filepath.Join(owner.AbsolutePath, parts[0], parts[1])
			ownedInfo, ownedErr := os.Stat(ownedPath)
			if os.IsNotExist(ownedErr) {
				continue
			}
			if ownedErr != nil {
				return RepoPhysicalCopySet{}, fmt.Errorf("inspect owned repository candidate %q: %w", ownedPath, ownedErr)
			}
			if !ownedInfo.IsDir() || !os.SameFile(info, ownedInfo) {
				continue
			}
			path, info = ownedPath, ownedInfo
		}
		rootIDs := []string{root.ID}
		if owner.ID != root.ID {
			rootIDs = appendUnique(rootIDs, owner.ID)
		}
		if isProjection {
			projections = append(projections, projectionCandidate{projection: RepoProjection{Path: path, OwnerRootID: owner.ID, RootIDs: rootIDs, Restrictions: root.Restrictions | owner.Restrictions}, info: info})
			continue
		}
		candidate := physicalCandidate{copy: RepoPhysicalCopy{Kind: kind, Path: path, OwnerRootID: owner.ID, RootIDs: rootIDs, Restrictions: root.Restrictions | owner.Restrictions}, info: info}
		switch kind {
		case PhysicalCopyHub:
			hubs = append(hubs, candidate)
		default:
			local = append(local, candidate)
		}
	}
	local = groupPhysicalCandidates(local)
	hubs = groupPhysicalCandidates(hubs)
	// Merge aliases across roles too: when an explicitly configured local root
	// and the selected Hub name the same existing repository directory, they
	// share one physical-copy record with combined root IDs/restrictions.
	combined := append(append([]physicalCandidate(nil), hubs...), local...)
	combined = groupPhysicalCandidates(combined)
	hubs = hubs[:0]
	local = local[:0]
	for _, candidate := range combined {
		if candidate.copy.Kind == PhysicalCopyHub {
			hubs = append(hubs, candidate)
		} else {
			local = append(local, candidate)
		}
	}
	result := RepoPhysicalCopySet{}
	if len(hubs) == 1 {
		hub := &hubs[0].copy
		var projectionList []RepoProjection
		for _, projection := range projections {
			if os.SameFile(hubs[0].info, projection.info) {
				// Only collapse a projection into the Hub physical unit when the
				// filesystem proves it is the same directory object.
				hub.RootIDs = appendUnique(hub.RootIDs, projection.projection.RootIDs...)
				hub.Restrictions |= projection.projection.Restrictions
				continue
			}
			projectionValue := projection.projection
			// A configured local alias to a friendly projection is still not
			// an independent physical copy or delete unit.
			filtered := local[:0]
			for _, candidate := range local {
				if os.SameFile(candidate.info, projection.info) {
					projectionValue.RootIDs = appendUnique(projectionValue.RootIDs, candidate.copy.RootIDs...)
					projectionValue.Restrictions |= candidate.copy.Restrictions
					continue
				}
				filtered = append(filtered, candidate)
			}
			local = filtered
			projectionList = append(projectionList, projectionValue)
		}
		result.Copies = append(result.Copies, *hub)
		result.Projections = append(result.Projections, projectionList...)
	} else {
		result.Copies = append(result.Copies, hubsToCopies(hubs)...)
		for _, projection := range projections {
			alias := -1
			for i, candidate := range local {
				if os.SameFile(candidate.info, projection.info) {
					alias = i
					break
				}
			}
			projectionValue := projection.projection
			if alias >= 0 {
				projectionValue.RootIDs = appendUnique(projectionValue.RootIDs, local[alias].copy.RootIDs...)
				projectionValue.Restrictions |= local[alias].copy.Restrictions
				local = append(local[:alias], local[alias+1:]...)
			}
			result.Projections = append(result.Projections, projectionValue)
		}
	}
	result.Copies = append(result.Copies, hubsToCopies(local)...)
	return result, nil
}

func safeRepoPathComponent(value string) bool {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\:\x00") {
		return false
	}
	return filepath.VolumeName(value) == ""
}

func hubsToCopies(candidates []physicalCandidate) []RepoPhysicalCopy {
	copies := make([]RepoPhysicalCopy, 0, len(candidates))
	for _, candidate := range candidates {
		copies = append(copies, candidate.copy)
	}
	return copies
}

func groupPhysicalCandidates(candidates []physicalCandidate) []physicalCandidate {
	var groups []physicalCandidate
	for _, candidate := range candidates {
		index := -1
		for i, group := range groups {
			if os.SameFile(group.info, candidate.info) {
				index = i
				break
			}
		}
		if index < 0 {
			groups = append(groups, candidate)
			continue
		}
		groups[index].copy.RootIDs = appendUnique(groups[index].copy.RootIDs, candidate.copy.RootIDs...)
		groups[index].copy.Restrictions |= candidate.copy.Restrictions
	}
	return groups
}

func appendUnique(target []string, values ...string) []string {
	for _, value := range values {
		found := false
		for _, existing := range target {
			if existing == value {
				found = true
				break
			}
		}
		if !found {
			target = append(target, value)
		}
	}
	return target
}

func isHubSpecialOwner(owner string) bool {
	switch strings.ToLower(owner) {
	case "hub", "models", "datasets", "blobs", "snapshots", "refs":
		return true
	default:
		return false
	}
}
