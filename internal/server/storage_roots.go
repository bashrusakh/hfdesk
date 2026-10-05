package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type rootRole uint8

const (
	rootBrowse rootRole = 1 << iota
	rootProtected
	rootHub
)

// localCacheRoot is the read-only server adapter for one immutable managed
// root definition. ID is lexical identity; physical observations are never
// stored here and are refreshed by ManagedRootSet operations.
type localCacheRoot struct {
	ID          string
	Path        string
	AbsPath     string
	Source      string
	SkipSpecial bool
	roles       rootRole
	set         *ManagedRootSet
}

func (root localCacheRoot) skipsOwner(owner string) bool {
	if root.SkipSpecial {
		switch strings.ToLower(owner) {
		case "hub", "models", "datasets", "blobs", "snapshots", "refs":
			return true
		}
	}
	return false
}

// ManagedRootSet is an immutable configuration-generation snapshot. Its root
// definitions and lexical IDs never change; filesystem facts are observed on
// each operation so creating/replacing a path cannot leave stale authority.
type ManagedRootSet struct {
	base  string
	roots []localCacheRoot
}

func configuredPath(path, base string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	path = filepath.Clean(path)
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Clean(filepath.Join(base, path))
}

func configuredPathID(path, base string) string {
	identity := configuredPath(path, base)
	sum := sha256.Sum256([]byte(identity))
	return "root:" + hex.EncodeToString(sum[:12])
}

func pathIdentityKeyAt(path, base string) string {
	if path == "" {
		return ""
	}
	return configuredPath(path, base)
}

func cleanPathList(paths []string) []string { return cleanPathListAt(paths, mustWorkingDirectory()) }

func cleanPathListAt(paths []string, base string) []string {
	var cleaned []string
	seen := make(map[string]bool)
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		key := pathIdentityKeyAt(path, base)
		if seen[key] {
			continue
		}
		seen[key] = true
		cleaned = append(cleaned, path)
	}
	return cleaned
}

func mustWorkingDirectory() string {
	base, err := filepath.Abs(".")
	if err != nil {
		return "."
	}
	return base
}

func newManagedRootSet(cacheDir, hubDir, localDir string, localScanDirs []string, routes map[string]string, base string) *ManagedRootSet {
	if base == "" {
		base = mustWorkingDirectory()
	}
	set := &ManagedRootSet{base: filepath.Clean(base)}
	byID := make(map[string]int)
	add := func(path, source string, roles rootRole, skipSpecial bool) {
		if strings.TrimSpace(path) == "" {
			return
		}
		path = filepath.Clean(path)
		abs := configuredPath(path, set.base)
		id := configuredPathID(path, set.base)
		if index, found := byID[id]; found {
			root := &set.roots[index]
			hadBrowseRole := root.roles&rootBrowse != 0
			if roles&rootBrowse != 0 && (!hadBrowseRole || managedRootSourcePriority(source) < managedRootSourcePriority(root.Source)) {
				root.Source = source
			}
			root.roles |= roles
			root.SkipSpecial = root.SkipSpecial || skipSpecial
			return
		}
		byID[id] = len(set.roots)
		set.roots = append(set.roots, localCacheRoot{ID: id, Path: path, AbsPath: abs, Source: source, SkipSpecial: skipSpecial, roles: roles})
	}
	add(filepath.Join(cacheDir, "models"), "Friendly view", rootBrowse|rootProtected, false)
	add(filepath.Join(cacheDir, "datasets"), "Friendly view", rootProtected, false)
	add(localDir, "Local", rootBrowse|rootProtected, localDir != "" && pathIdentityKeyAt(localDir, set.base) == pathIdentityKeyAt(cacheDir, set.base))
	for _, dir := range cleanPathListAt(localScanDirs, set.base) {
		add(dir, "Local", rootBrowse|rootProtected, false)
	}
	for _, dir := range routeDirsAt(routes, set.base) {
		add(dir, "Local", rootBrowse|rootProtected, false)
	}
	add(hubDir, "HF cache", rootProtected|rootHub, false)
	add(cacheDir, "Local", rootBrowse|rootProtected, true)
	for i := range set.roots {
		set.roots[i].set = set
	}
	return set
}

func newManagedRootSetForConfig(cfg Config) *ManagedRootSet {
	cfg = cfg.captureCacheEnvironment()
	cache := cfg.cache()
	return newManagedRootSet(cache.Root, cache.HubDir(), cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, cfg.cacheEnv.pathBase)
}

func (set *ManagedRootSet) Roots() []localCacheRoot {
	return append([]localCacheRoot(nil), set.roots...)
}

// observeContainment returns true only when lexical containment or fresh
// filesystem evidence proves ancestry. It reports uncertainty rather than
// treating an inaccessible path as either inside or outside.
func observeContainment(parent, target string) (bool, error) {
	inside, _, err := observeContainmentDistance(parent, target)
	return inside, err
}

func observeContainmentDistance(parent, target string) (bool, int, error) {
	if withinLocalRoot(parent, target) {
		rel, _ := filepath.Rel(parent, target)
		if rel == "." {
			return true, 0, nil
		}
		return true, strings.Count(filepath.Clean(rel), string(filepath.Separator)) + 1, nil
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("stat configured root %q: %w", parent, err)
	}
	target = filepath.Clean(target)
	distance := 0
	for current := target; ; current = filepath.Dir(current) {
		info, statErr := os.Stat(current)
		if statErr == nil && os.SameFile(parentInfo, info) {
			return true, distance, nil
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return false, 0, fmt.Errorf("stat path ancestor %q: %w", current, statErr)
		}
		parentPath := filepath.Dir(current)
		if parentPath == current {
			break
		}
		distance++
	}
	return false, 0, nil
}

func withinLocalRoot(root, path string) bool {
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

// OwnerForPath chooses the most-specific configured root that owns path.
// Ties retain deterministic config registration order. Filesystem facts are
// gathered afresh, including for roots created after this set was built.
func (set *ManagedRootSet) OwnerForPath(path string) (localCacheRoot, error) {
	target := configuredPath(path, set.base)
	owner := -1
	distance := int(^uint(0) >> 1)
	for i, root := range set.roots {
		inside, candidateDistance, err := observeContainmentDistance(root.AbsPath, target)
		if err != nil {
			return localCacheRoot{}, err
		}
		if !inside {
			continue
		}
		if candidateDistance < distance {
			owner, distance = i, candidateDistance
		}
	}
	if owner < 0 {
		return localCacheRoot{}, os.ErrNotExist
	}
	return set.roots[owner], nil
}

// NestedProtectedRoots returns roots beneath the selected scan root, excluding
// the root itself. Unknown ancestry is an error, never a guessed exclusion.
func (set *ManagedRootSet) NestedProtectedRoots(rootID string) ([]string, error) {
	root, ok := set.rootByID(rootID)
	if !ok {
		return nil, fmt.Errorf("unknown managed root %q", rootID)
	}
	var nested []string
	for _, candidate := range set.roots {
		if candidate.ID == root.ID || candidate.roles&rootProtected == 0 {
			continue
		}
		inside, err := observeContainment(root.AbsPath, candidate.AbsPath)
		if err != nil {
			return nil, err
		}
		if inside {
			// Equal physical roots are a shared object, not a nested subtree.
			rootInfo, rootErr := os.Stat(root.AbsPath)
			candidateInfo, candidateErr := os.Stat(candidate.AbsPath)
			if rootErr != nil && !os.IsNotExist(rootErr) {
				return nil, fmt.Errorf("inspect managed root %q: %w", root.AbsPath, rootErr)
			}
			if candidateErr != nil && !os.IsNotExist(candidateErr) {
				return nil, fmt.Errorf("inspect protected root %q: %w", candidate.AbsPath, candidateErr)
			}
			if rootErr == nil && candidateErr == nil && os.SameFile(rootInfo, candidateInfo) {
				continue
			}
			nested = append(nested, candidate.AbsPath)
		}
	}
	sort.Strings(nested)
	return nested, nil
}

func (set *ManagedRootSet) rootByID(id string) (localCacheRoot, bool) {
	for _, root := range set.roots {
		if root.ID == id {
			return root, true
		}
	}
	return localCacheRoot{}, false
}

// Resolve accepts only a direct owner/name repo shape under the configured
// root. It does not grant destructive authority by itself.
func (set *ManagedRootSet) Resolve(rootID, owner, name string) (string, error) {
	root, ok := set.rootByID(rootID)
	if !ok || root.roles&rootBrowse == 0 || !isValidRepoComponent(owner) || !isValidRepoComponent(name) || owner == "." || owner == ".." || name == "." || name == ".." {
		return "", errors.New("invalid managed root repository path")
	}
	return filepath.Join(root.AbsPath, owner, name), nil
}

func rejectManagedSymlinkComponents(rootPath, target string) error {
	rootPath = filepath.Clean(rootPath)
	target = filepath.Clean(target)
	inside, err := observeContainment(rootPath, target)
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
			return fmt.Errorf("symlinked managed path component %q (root %q, target %q)", current, rootPath, target)
		}
	}
	return nil
}

// WholeCopyAllowed applies ownership and symlink/containment checks without
// mutating. Callers retain responsibility for their existing operation/API.
func (set *ManagedRootSet) WholeCopyAllowed(rootID, target string) error {
	root, ok := set.rootByID(rootID)
	if !ok {
		return fmt.Errorf("unknown managed root %q", rootID)
	}
	target = configuredPath(target, set.base)
	rel, relErr := filepath.Rel(root.AbsPath, target)
	if relErr != nil || rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return errors.New("whole-copy target is not below its configured root")
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if root.roles&rootHub != 0 {
		if len(parts) != 1 || !validHubRepoDirName(parts[0]) {
			return errors.New("whole-copy target is not an exact Hub repository entry")
		}
	} else if len(parts) != 2 || !isValidRepoComponent(parts[0]) || !isValidRepoComponent(parts[1]) || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return errors.New("whole-copy target is not an exact owner/name repository entry")
	}
	targetInfo, targetErr := os.Lstat(target)
	if targetErr != nil {
		return fmt.Errorf("cannot inspect whole-copy target: %w", targetErr)
	}
	if !targetInfo.IsDir() || targetInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("whole-copy target is not a real directory")
	}
	owner, err := set.OwnerForPath(target)
	if err != nil {
		return fmt.Errorf("cannot establish managed-root owner: %w", err)
	}
	if owner.ID != root.ID && !sameDirectoryObject(owner.AbsPath, root.AbsPath) {
		return fmt.Errorf("target is owned by managed root %s", owner.ID)
	}
	if err := rejectManagedSymlinkComponents(root.AbsPath, target); err != nil {
		return err
	}
	for _, protected := range set.roots {
		if protected.ID == root.ID || protected.roles&rootProtected == 0 {
			continue
		}
		contained, err := observeContainment(target, protected.AbsPath)
		if err != nil {
			return fmt.Errorf("cannot verify protected-root boundary: %w", err)
		}
		if contained {
			return fmt.Errorf("target encloses protected managed root %s", protected.ID)
		}
		protectedInfo, protectedErr := os.Stat(protected.AbsPath)
		if protectedErr != nil && !os.IsNotExist(protectedErr) {
			return fmt.Errorf("cannot inspect protected managed root %s: %w", protected.ID, protectedErr)
		}
		if protectedErr == nil && os.SameFile(protectedInfo, targetInfo) {
			return fmt.Errorf("target is a protected managed root %s", protected.ID)
		}
	}
	return nil
}

func validHubRepoDirName(name string) bool {
	for _, prefix := range []string{"models--", "datasets--"} {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		owner, repo, found := strings.Cut(strings.TrimPrefix(name, prefix), "--")
		return found && isValidRepoComponent(owner) && isValidRepoComponent(repo) && owner != "." && owner != ".." && repo != "." && repo != ".."
	}
	return false
}

func (root localCacheRoot) walk(dir string, fn filepath.WalkFunc) error {
	if root.set == nil {
		return filepath.Walk(dir, fn)
	}
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		owner, ownerErr := root.set.OwnerForPath(path)
		if ownerErr != nil {
			return ownerErr
		}
		if owner.ID != root.ID && !sameDirectoryObject(owner.AbsPath, root.AbsPath) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		return fn(path, info, err)
	})
}

func sameDirectoryObject(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && leftInfo.IsDir() && rightInfo.IsDir() && os.SameFile(leftInfo, rightInfo)
}

func managedRootSourcePriority(source string) int {
	switch source {
	case "HF cache":
		return 0
	case "Friendly view":
		return 1
	default:
		return 2
	}
}

func browseRoots(set *ManagedRootSet) []localCacheRoot {
	var roots []localCacheRoot
	for _, candidate := range set.roots {
		if candidate.roles&rootBrowse == 0 {
			continue
		}
		index := -1
		for i, existing := range roots {
			if existing.ID == candidate.ID || sameDirectoryObject(existing.AbsPath, candidate.AbsPath) {
				index = i
				break
			}
		}
		if index < 0 {
			roots = append(roots, candidate)
			continue
		}
		roots[index].SkipSpecial = roots[index].SkipSpecial || candidate.SkipSpecial
		if managedRootSourcePriority(candidate.Source) < managedRootSourcePriority(roots[index].Source) {
			roots[index].Source = candidate.Source
		}
	}
	return roots
}

func localCacheRoots(cacheDir, localDir string, localScanDirs []string, routes map[string]string) []localCacheRoot {
	return browseRoots(newManagedRootSet(cacheDir, filepath.Join(cacheDir, "hub"), localDir, localScanDirs, routes, mustWorkingDirectory()))
}
