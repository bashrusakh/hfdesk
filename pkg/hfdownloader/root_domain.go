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

// ManagedRootRole classifies a configured path for namespace observation and
// read-side behavior. Roles do not prove exclusive recursive ownership or
// grant operation-specific authority.
type ManagedRootRole uint32

const (
	ManagedRootBrowse            ManagedRootRole = 1 << iota // Read-only repository discovery.
	ManagedRootProtected                                     // Configured as protected for later operation-specific policy.
	ManagedRootLocal                                         // Root enumerates Local owner/name slots with unknown type.
	ManagedRootHub                                           // Root has a typed HF models/datasets namespace layout.
	ManagedRootModelProjection                               // Root has a friendly model projection layout.
	ManagedRootDatasetProjection                             // Root has a friendly dataset projection layout.
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

// ManagedRootGroup groups browse roots whose directory objects compare equal.
// Members preserves lexical IDs; grouping does not equate child namespace views.
type ManagedRootGroup struct {
	Root    ManagedRoot
	Members []ManagedRoot
}

// PhysicalCopyKind classifies the configured namespace where a slot was observed.
type PhysicalCopyKind uint8

const (
	PhysicalCopyLocal PhysicalCopyKind = iota + 1 // A direct local owner/name directory.
	PhysicalCopyHub                               // An exact models--owner--name/datasets--owner--name directory.
)

// RepoPhysicalCopy is a legacy read-side view of one repository directory slot.
// RootIDs records configured lexical identities observed at that directory;
// it does not establish exclusive recursive-view membership or authorization.
type RepoPhysicalCopy struct {
	Kind         PhysicalCopyKind
	Path         string
	OwnerRootID  string
	RootIDs      []string
	Restrictions ManagedRootRestriction
}

// RepoProjection describes a friendly-view directory slot. It carries no
// authority over the directory or any recursive effect.
type RepoProjection struct {
	Path         string
	OwnerRootID  string
	RootIDs      []string
	Restrictions ManagedRootRestriction
}

// RepoPhysicalCopySet preserves the legacy read DTO while exposing the
// underlying logical namespace memberships separately.
type RepoPhysicalCopySet struct {
	Copies      []RepoPhysicalCopy
	Projections []RepoProjection
	Memberships []NamespaceMembership
}

// NamespaceMembership records one current logical slot and its observed
// directory entries. It is descriptive evidence, not authorization.
type NamespaceMembership struct {
	Root                   ManagedRoot
	OwnerRootID            string
	Kind                   PhysicalCopyKind
	RepoID                 string
	RepoType               RepoType
	TypeKnown              bool
	OwnedPathMatchObserved bool
	Path                   string
	OwnedPath              string
	Directory              os.FileInfo
	Regions                []NamespaceDirectoryRegion
	Entries                []NamespaceEntry
}

// NamespaceDirectoryRegion records one reachable directory occurrence. The
// same object may appear at several paths with different child views.
type NamespaceDirectoryRegion struct {
	Path string
	Info os.FileInfo
}

// NamespaceEntry records one current entry with lstat-style identity; symlinks
// are leaves and are not traversed for their target contents.
type NamespaceEntry struct {
	Path string
	Info os.FileInfo
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

// AllowsOwner is a browse-enumeration filter for reserved HF cache names. It
// applies static and freshly observed same-object restrictions; it is not a
// destructive eligibility or exclusive-ownership decision.
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
// component spelling. It is not a filesystem ancestry observation.
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

// containmentDistance recognizes configured lexical ancestry and then attempts
// path-spelling filesystem observations. A negative result means only that
// those observations did not establish ancestry; it is not proof of recursive
// view disjointness. Configured spellings remain unchanged.
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

// OwnerForPath reports the most-specific configured namespace relation found
// by lexical containment or current path-spelling observations. It does not
// establish exclusive recursive ownership.
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

// NestedProtectedRoots observes protected roots intersecting the configured
// root's current directory graph. Unknown reachability is returned as an error.
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

// Resolve returns a lexical owner/name path beneath a browsable configured root.
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

// WalkOwned reports entries observed in rootID's namespace, excluding regions
// attributed to nested configured roots. It does not establish exclusive use.
func (set *ManagedRootSet) WalkOwned(rootID, dir string, fn filepath.WalkFunc) error {
	if err := set.ready(); err != nil {
		return err
	}
	return set.walkOwnedEntries(rootID, dir, nil, fn, nil, nil)
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

// BrowseRootGroups groups roots with equal directory-object observations while
// retaining each lexical root ID. Equal objects do not imply equal child views.
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

// RepoPhysicalCopiesFromNamespace projects one requested ID from a complete
// namespace observation. The memberships remain available without grouping so
// callers can distinguish logical slots even when objects compare equal.
func (set *ManagedRootSet) RepoPhysicalCopiesFromNamespace(repoID string, repoType RepoType, all []NamespaceMembership) (RepoPhysicalCopySet, error) {
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
	matched := NamespaceMembershipsFor(all, repoID, repoType)
	result := RepoPhysicalCopySet{Memberships: append([]NamespaceMembership(nil), matched...)}
	for _, membership := range matched {
		if !membership.OwnedPathMatchObserved {
			continue
		}
		root, ok := set.Root(membership.Root.ID)
		if !ok {
			return RepoPhysicalCopySet{}, fmt.Errorf("namespace membership has unknown root %q", membership.Root.ID)
		}
		owner, ok := set.Root(membership.OwnerRootID)
		if !ok {
			return RepoPhysicalCopySet{}, fmt.Errorf("namespace membership has unknown owner root %q", membership.OwnerRootID)
		}
		if membership.OwnedPath == "" {
			return RepoPhysicalCopySet{}, fmt.Errorf("namespace membership has no owned path for %q", membership.Path)
		}
		rootIDs := []string{root.ID}
		if owner.ID != root.ID {
			rootIDs = append(rootIDs, owner.ID)
		}
		if membership.Kind == 0 {
			result.Projections = append(result.Projections, RepoProjection{Path: membership.OwnedPath, OwnerRootID: owner.ID, RootIDs: rootIDs, Restrictions: root.Restrictions | owner.Restrictions})
			continue
		}
		result.Copies = append(result.Copies, RepoPhysicalCopy{Kind: membership.Kind, Path: membership.OwnedPath, OwnerRootID: owner.ID, RootIDs: rootIDs, Restrictions: root.Restrictions | owner.Restrictions})
	}
	return result, nil
}

// RepoPhysicalCopies returns the legacy display projection of current
// namespace facts. It does not establish that a slot is an exclusive copy.
func (set *ManagedRootSet) RepoPhysicalCopies(repoID string, repoType RepoType) (RepoPhysicalCopySet, error) {
	all, err := set.ObserveNamespaceMemberships()
	if err != nil {
		return RepoPhysicalCopySet{}, err
	}
	return set.RepoPhysicalCopiesFromNamespace(repoID, repoType, all)
}

func safeRepoPathComponent(value string) bool {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\:\x00") {
		return false
	}
	return filepath.VolumeName(value) == ""
}

func isHubSpecialOwner(owner string) bool {
	switch strings.ToLower(owner) {
	case "hub", "models", "datasets", "blobs", "snapshots", "refs":
		return true
	default:
		return false
	}
}
