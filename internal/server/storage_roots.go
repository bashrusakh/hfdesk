package server

import (
	"path/filepath"
	"strings"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// localCacheRoot is the presentation adapter for an immutable downloader root.
// It contains no independent ownership or physical-identity decisions.
type localCacheRoot struct {
	ID          string
	Path        string
	AbsPath     string
	Source      string
	SkipSpecial bool
	set         *storageRootSet
}

type storageRootSet struct {
	domain    *hfdownloader.ManagedRootSet
	base      string
	hubRootID string
	sources   map[string]string
	roots     []localCacheRoot
	err       error
}

func configuredPath(path, base string) string {
	return hfdownloader.ConfiguredPath(path, base)
}

func configuredPathID(path, base string) string {
	return hfdownloader.ManagedRootID(path, base)
}

func pathIdentityKeyAt(path, base string) string {
	return hfdownloader.ConfiguredPathIdentity(path, base)
}

func cleanPathList(paths []string) []string {
	return cleanPathListAt(paths, mustWorkingDirectory())
}

func cleanPathListAt(paths []string, base string) []string {
	return hfdownloader.CleanConfiguredPathList(paths, base)
}

func mustWorkingDirectory() string {
	base, err := filepath.Abs(".")
	if err != nil {
		return "."
	}
	return base
}

func (root localCacheRoot) skipsOwner(owner string) bool {
	if root.SkipSpecial {
		if root.set == nil {
			return true
		}
		allowed, err := root.set.domain.AllowsOwner(root.ID, owner)
		return err != nil || !allowed
	}
	return false
}

func newManagedRootSet(cacheDir, hubDir, localDir string, localScanDirs []string, routes map[string]string, base string) *storageRootSet {
	if base == "" {
		base = mustWorkingDirectory()
	}
	var specs []hfdownloader.ManagedRootSpec
	sources := make(map[string]string)
	add := func(path, source string, roles hfdownloader.ManagedRootRole, restrictions hfdownloader.ManagedRootRestriction) {
		if strings.TrimSpace(path) == "" {
			return
		}
		id := configuredPathID(path, base)
		if roles&hfdownloader.ManagedRootBrowse != 0 {
			if current, found := sources[id]; !found || managedRootSourcePriority(source) < managedRootSourcePriority(current) {
				sources[id] = source
			}
		}
		specs = append(specs, hfdownloader.ManagedRootSpec{Path: path, Roles: roles, Restrictions: restrictions})
	}
	browseProtected := hfdownloader.ManagedRootBrowse | hfdownloader.ManagedRootProtected
	localRoles := browseProtected | hfdownloader.ManagedRootLocal
	add(filepath.Join(cacheDir, "models"), "Friendly view", browseProtected|hfdownloader.ManagedRootModelProjection, 0)
	add(filepath.Join(cacheDir, "datasets"), "Friendly view", hfdownloader.ManagedRootProtected|hfdownloader.ManagedRootDatasetProjection, 0)
	localRestrictions := hfdownloader.ManagedRootRestriction(0)
	if localDir != "" && pathIdentityKeyAt(localDir, base) == pathIdentityKeyAt(cacheDir, base) {
		localRestrictions |= hfdownloader.ManagedRootSkipSpecial
	}
	add(localDir, "Local", localRoles, localRestrictions)
	for _, dir := range cleanPathListAt(localScanDirs, base) {
		add(dir, "Local", localRoles, 0)
	}
	for _, dir := range routeDirsAt(routes, base) {
		add(dir, "Local", localRoles, 0)
	}
	add(hubDir, "HF cache", hfdownloader.ManagedRootProtected|hfdownloader.ManagedRootHub, 0)
	add(cacheDir, "Local", localRoles, hfdownloader.ManagedRootSkipSpecial)

	domain := hfdownloader.NewManagedRootSet(base, specs)
	set := &storageRootSet{domain: domain, base: base, hubRootID: configuredPathID(hubDir, base), sources: sources}
	set.roots, set.err = browseRoots(set)
	return set
}

func newManagedRootSetForConfig(cfg Config) *storageRootSet {
	cfg = cfg.captureCacheEnvironment()
	cache := cfg.cache()
	return newManagedRootSet(cache.Root, cache.HubDir(), cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, cfg.cacheEnv.pathBase)
}

func (set *storageRootSet) Root(id string) (localCacheRoot, bool) {
	root, ok := set.domain.Root(id)
	if !ok {
		return localCacheRoot{}, false
	}
	return localCacheRootFromDomain(set, root), true
}

func (set *storageRootSet) Roots() []localCacheRoot {
	roots := set.domain.Roots()
	result := make([]localCacheRoot, 0, len(roots))
	for _, root := range roots {
		result = append(result, localCacheRootFromDomain(set, root))
	}
	return result
}

func (set *storageRootSet) OwnerForPath(path string) (localCacheRoot, error) {
	if set.err != nil {
		return localCacheRoot{}, set.err
	}
	root, err := set.domain.OwnerForPath(path)
	if err != nil {
		return localCacheRoot{}, err
	}
	return localCacheRootFromDomain(set, root), nil
}

func (set *storageRootSet) NestedProtectedRoots(rootID string) ([]string, error) {
	if set.err != nil {
		return nil, set.err
	}
	roots, err := set.domain.NestedProtectedRoots(rootID)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(roots))
	for _, root := range roots {
		paths = append(paths, root.AbsolutePath)
	}
	return paths, nil
}

func (set *storageRootSet) Resolve(rootID, owner, name string) (string, error) {
	if set.err != nil {
		return "", set.err
	}
	return set.domain.Resolve(rootID, owner, name)
}

func (set *storageRootSet) RepoPhysicalCopies(repoID string, repoType hfdownloader.RepoType) (hfdownloader.RepoPhysicalCopySet, error) {
	if set.err != nil {
		return hfdownloader.RepoPhysicalCopySet{}, set.err
	}
	memberships, err := set.ObserveNamespaceMemberships()
	if err != nil {
		return hfdownloader.RepoPhysicalCopySet{}, err
	}
	return set.RepoPhysicalCopiesFromNamespace(repoID, repoType, memberships)
}

func (set *storageRootSet) ObserveNamespaceMemberships() ([]hfdownloader.NamespaceMembership, error) {
	if set.err != nil {
		return nil, set.err
	}
	return set.domain.ObserveNamespaceMemberships()
}

func (set *storageRootSet) RepoPhysicalCopiesFromNamespace(repoID string, repoType hfdownloader.RepoType, memberships []hfdownloader.NamespaceMembership) (hfdownloader.RepoPhysicalCopySet, error) {
	if set.err != nil {
		return hfdownloader.RepoPhysicalCopySet{}, set.err
	}
	return set.domain.RepoPhysicalCopiesFromNamespace(repoID, repoType, memberships)
}

func (set *storageRootSet) HubMembershipObserved(repoID string, repoType hfdownloader.RepoType, path string, memberships []hfdownloader.NamespaceMembership) bool {
	for _, membership := range hfdownloader.NamespaceMembershipsFor(memberships, repoID, repoType) {
		if membership.Kind == hfdownloader.PhysicalCopyHub && membership.Root.ID == set.hubRootID &&
			hfdownloader.ConfiguredPathIdentity(membership.Path, set.base) == hfdownloader.ConfiguredPathIdentity(path, set.base) {
			return true
		}
	}
	return false
}

func localCacheRootFromDomain(set *storageRootSet, root hfdownloader.ManagedRoot) localCacheRoot {
	return localCacheRoot{
		ID:          root.ID,
		Path:        root.Path,
		AbsPath:     root.AbsolutePath,
		Source:      set.sources[root.ID],
		SkipSpecial: root.Restrictions&hfdownloader.ManagedRootSkipSpecial != 0,
		set:         set,
	}
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

func browseRoots(set *storageRootSet) ([]localCacheRoot, error) {
	groups, err := set.domain.BrowseRootGroups()
	if err != nil {
		return nil, err
	}
	roots := make([]localCacheRoot, 0, len(groups))
	for _, group := range groups {
		root := localCacheRootFromDomain(set, group.Root)
		for _, member := range group.Members {
			source := set.sources[member.ID]
			if managedRootSourcePriority(source) < managedRootSourcePriority(root.Source) {
				root.Source = source
			}
		}
		roots = append(roots, root)
	}
	return roots, nil
}

func localCacheRoots(cacheDir, localDir string, localScanDirs []string, routes map[string]string) []localCacheRoot {
	roots, _ := browseRoots(newManagedRootSet(cacheDir, filepath.Join(cacheDir, "hub"), localDir, localScanDirs, routes, mustWorkingDirectory()))
	return roots
}
