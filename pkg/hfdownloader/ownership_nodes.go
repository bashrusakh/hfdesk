// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	errOwnershipCaseProfileUnavailable = errors.New("native ownership case profile is unavailable")
	errOwnershipCaseModeUnsupported    = errors.New("filesystem does not expose per-directory case mode")
)

const ownershipNodeFormatVersion = 1

type ownershipCaseProfile struct {
	Version   int    `json:"version"`
	Provider  string `json:"provider"`
	Sensitive bool   `json:"sensitive"`
}

type ownershipNode struct {
	control string
	data    string
	id      string
	nodeIDs []string
	profile *ownershipCaseProfile
}

type cacheOwnershipPrepPlan struct {
	path         string
	parent       ownershipNode
	components   []string
	dirCount     int
	bootstrapKey string
}

func newCacheOwnershipRealmContext(ctx context.Context, dataRoot, controlRoot string, wait time.Duration) (*ownershipRealm, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dataRoot == "" || controlRoot == "" {
		return nil, fmt.Errorf("cache ownership root is empty")
	}
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create cache data root %q: %w", dataRoot, err)
	}
	backend, err := newOwnershipRealmBase(controlRoot)
	if err != nil {
		return nil, err
	}
	canonicalRoot, err := canonicalOwnershipDirectoryPath(dataRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve cache data root %q: %w", dataRoot, err)
	}
	backend.dataRoot = canonicalRoot
	backend.nodeRoot = filepath.Join(backend.lockDir, "root")
	backend.cacheOwned = true
	if err := backend.initializeCacheNodeRoot(ctx, wait); err != nil {
		return nil, err
	}
	return backend, nil
}

func (r *ownershipRealm) rootNode() ownershipNode {
	return ownershipNode{
		control: r.nodeRoot,
		data:    r.dataRoot,
		id:      r.id,
		nodeIDs: []string{r.id},
		profile: r.rootProfile,
	}
}

func (r *ownershipRealm) initializeCacheNodeRoot(ctx context.Context, wait time.Duration) error {
	if !r.cacheOwned {
		return nil
	}
	metaDir := filepath.Join(r.nodeRoot, "metadata")
	childrenDir := filepath.Join(r.nodeRoot, "children")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(childrenDir, 0o755); err != nil {
		return err
	}
	bootstrap, err := acquireOwnershipFileLock(ctx, filepath.Join(metaDir, "bootstrap.lock"), ownershipExclusive, wait)
	if err != nil {
		return fmt.Errorf("acquire cache-root profile bootstrap: %w", err)
	}
	defer bootstrap.Release()
	if err := ctx.Err(); err != nil {
		return err
	}

	profile, err := ownershipDirectoryCaseProfile(r.dataRoot, metaDir)
	if err != nil {
		return fmt.Errorf("read cache-root case profile: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureOwnershipControlDirectoryProfile(childrenDir, filepath.Join(metaDir, "profile"), profile); err != nil {
		return fmt.Errorf("initialize cache-root control profile: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	anchorID, err := loadOrCreateOwnershipNodeID(metaDir, "anchor-id")
	if err != nil {
		return fmt.Errorf("initialize cache anchor ID: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.id = anchorID
	r.rootProfile = &profile
	return nil
}

func ensureOwnershipControlDirectoryProfile(controlDir, profilePath string, want ownershipCaseProfile) error {
	if raw, err := os.ReadFile(profilePath); err == nil {
		var have ownershipCaseProfile
		if err := json.Unmarshal(raw, &have); err != nil {
			return fmt.Errorf("decode case profile %s: %w", profilePath, err)
		}
		if have.Version != ownershipNodeFormatVersion || have.Sensitive != want.Sensitive {
			return fmt.Errorf("case profile mismatch for control directory %s", controlDir)
		}
		actual, err := ownershipControlDirectoryCaseProfile(controlDir, filepath.Dir(profilePath))
		if err != nil {
			return err
		}
		if actual.Sensitive != want.Sensitive {
			return fmt.Errorf("control directory %s does not match its stored case profile", controlDir)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(controlDir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("control directory %s has children but no case profile", controlDir)
	}
	if err := setOwnershipControlDirectoryCaseProfile(controlDir, want); err != nil {
		return err
	}
	actual, err := ownershipControlDirectoryCaseProfile(controlDir, filepath.Dir(profilePath))
	if err != nil {
		return err
	}
	if actual.Sensitive != want.Sensitive {
		return fmt.Errorf("control directory %s case profile did not converge", controlDir)
	}
	return writeOwnershipCaseProfile(profilePath, want)
}

func writeOwnershipCaseProfile(path string, profile ownershipCaseProfile) error {
	data, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".case-profile.tmp-")
	if err != nil {
		return err
	}
	tmp := file.Name()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func readOwnershipCaseProfile(path string) (ownershipCaseProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ownershipCaseProfile{}, err
	}
	var profile ownershipCaseProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return ownershipCaseProfile{}, err
	}
	if profile.Version != ownershipNodeFormatVersion || profile.Provider == "" {
		return ownershipCaseProfile{}, fmt.Errorf("unsupported case profile %s", path)
	}
	return profile, nil
}

func (r *ownershipRealm) resolveCacheOwnershipPath(name string, directory bool) (ownershipResolvedPath, error) {
	return r.resolveCacheOwnershipPathWithLease(name, directory, nil)
}

func (r *ownershipRealm) resolveCacheOwnershipPathWithLease(name string, directory bool, preparation *ownershipLease) (ownershipResolvedPath, error) {
	if !r.cacheOwned || r.dataRoot == "" {
		return ownershipResolvedPath{}, fmt.Errorf("cache node ownership is not initialized")
	}
	canonical, err := canonicalOwnershipPath(name, directory)
	if err != nil {
		return ownershipResolvedPath{}, err
	}
	rel, err := filepath.Rel(r.dataRoot, canonical)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return ownershipResolvedPath{}, fmt.Errorf("ownership path %q is outside cache root %q", canonical, r.dataRoot)
	}
	components := splitOwnershipRelativePath(rel)
	if len(components) == 0 && !directory {
		return ownershipResolvedPath{}, fmt.Errorf("ownership item path %q has no component", canonical)
	}
	resolved := ownershipResolvedPath{canonical: canonical}
	if len(components) == 0 {
		info, err := os.Stat(r.dataRoot)
		if err != nil {
			return ownershipResolvedPath{}, err
		}
		keys, err := r.nodeDirectoryKeys(r.dataRoot, []string{r.id}, r.nodeRoot, info)
		if err != nil {
			return ownershipResolvedPath{}, err
		}
		resolved.directories = append(resolved.directories, keys)
		return resolved, nil
	}
	rootInfo, err := os.Stat(r.dataRoot)
	if err != nil {
		return ownershipResolvedPath{}, err
	}
	rootKeys, err := r.nodeDirectoryKeys(r.dataRoot, []string{r.id}, r.nodeRoot, rootInfo)
	if err != nil {
		return ownershipResolvedPath{}, err
	}
	resolved.directories = append(resolved.directories, rootKeys)
	parent := r.rootNode()
	limit := len(components)
	if !directory {
		limit--
	}
	for i := 0; i < limit; i++ {
		component := components[i]
		if !validOwnershipComponent(component) {
			return ownershipResolvedPath{}, fmt.Errorf("invalid ownership path component %q", component)
		}
		dataPath := filepath.Join(parent.data, component)
		info, statErr := os.Stat(dataPath)
		if statErr != nil {
			return ownershipResolvedPath{}, fmt.Errorf("cache parent %q is unavailable: %w", dataPath, statErr)
		}
		if !info.IsDir() {
			return ownershipResolvedPath{}, fmt.Errorf("ownership component %q is not a directory", dataPath)
		}
		bootstrapKey := r.resourceKey("node-child-bootstrap", parent.id)
		var child ownershipNode
		if preparation != nil && preparation.modes[bootstrapKey] == ownershipExclusive {
			child, err = r.ensureOwnershipNodeLocked(parent, component, dataPath, true)
		} else {
			child, err = r.ensureOwnershipNode(parent, component, dataPath, true)
		}
		if err != nil {
			return ownershipResolvedPath{}, err
		}
		keys, err := r.nodeDirectoryKeys(dataPath, child.nodeIDs, child.control, info)
		if err != nil {
			return ownershipResolvedPath{}, err
		}
		resolved.directories = append(resolved.directories, keys)
		parent = child
	}

	if directory {
		if len(components) == 0 {
			return resolved, nil
		}
		if len(resolved.directories) == 0 {
			return ownershipResolvedPath{}, fmt.Errorf("ownership tree target %q has no node", canonical)
		}
		return resolved, nil
	}
	leaf := components[len(components)-1]
	if !validOwnershipComponent(leaf) {
		return ownershipResolvedPath{}, fmt.Errorf("invalid ownership path component %q", leaf)
	}
	leafPath := filepath.Join(parent.data, leaf)
	bootstrapKey := r.resourceKey("node-child-bootstrap", parent.id)
	var child ownershipNode
	if preparation != nil && preparation.modes[bootstrapKey] == ownershipExclusive {
		child, err = r.ensureOwnershipNodeLocked(parent, leaf, leafPath, false)
	} else {
		child, err = r.ensureOwnershipNode(parent, leaf, leafPath, false)
	}
	if err != nil {
		return ownershipResolvedPath{}, err
	}
	identity := strings.Join(child.nodeIDs, "\x00")
	entryKey := r.resourceKey("node-entry", identity)
	childrenKey := r.resourceKey("node-children", identity)
	resolved.itemKeys = []string{
		entryKey,
		childrenKey,
	}
	leafMeta := filepath.Join(child.control, "metadata")
	resolved.itemRequests = []ownershipLockRequest{
		{key: entryKey, mode: ownershipExclusive, path: filepath.Join(leafMeta, "entry.lock")},
		{key: childrenKey, mode: ownershipExclusive, path: filepath.Join(leafMeta, "children.lock")},
	}
	resolved.itemNodeID = identity
	resolved.nodeRealm = r
	return resolved, nil
}

// resolveCacheOwnershipPathReadOnly derives IDs from already-published nodes.
// Held-lease verification must not reopen child-bootstrap locks: another item
// may be initializing a sibling node while this item owns its independent key.
func (r *ownershipRealm) resolveCacheOwnershipPathReadOnly(name string, directory bool) (ownershipResolvedPath, error) {
	canonical, err := canonicalOwnershipPath(name, directory)
	if err != nil {
		return ownershipResolvedPath{}, err
	}
	if !ownershipPathWithinRoot(r.dataRoot, canonical) {
		return ownershipResolvedPath{}, fmt.Errorf("ownership path %q is outside cache root %q", canonical, r.dataRoot)
	}
	rel, err := filepath.Rel(r.dataRoot, canonical)
	if err != nil {
		return ownershipResolvedPath{}, err
	}
	components := splitOwnershipRelativePath(rel)
	resolved := ownershipResolvedPath{canonical: canonical}
	rootInfo, err := os.Stat(r.dataRoot)
	if err != nil {
		return ownershipResolvedPath{}, err
	}
	rootProfile, err := readOwnershipCaseProfile(filepath.Join(r.nodeRoot, "metadata", "profile"))
	if err != nil || r.rootProfile == nil || rootProfile.Sensitive != r.rootProfile.Sensitive {
		if err == nil {
			err = fmt.Errorf("cache-root profile changed")
		}
		return ownershipResolvedPath{}, err
	}
	rootKeys, err := r.nodeDirectoryKeys(r.dataRoot, []string{r.id}, r.nodeRoot, rootInfo)
	if err != nil {
		return ownershipResolvedPath{}, err
	}
	resolved.directories = append(resolved.directories, rootKeys)
	if len(components) == 0 {
		if !directory {
			return ownershipResolvedPath{}, fmt.Errorf("ownership item path %q has no component", canonical)
		}
		return resolved, nil
	}
	parent := r.rootNode()
	limit := len(components)
	if !directory {
		limit--
	}
	for i := 0; i < limit; i++ {
		component := components[i]
		if !validOwnershipComponent(component) {
			return ownershipResolvedPath{}, fmt.Errorf("invalid ownership path component %q", component)
		}
		dataPath := filepath.Join(parent.data, component)
		info, err := os.Stat(dataPath)
		if err != nil {
			return ownershipResolvedPath{}, err
		}
		if !info.IsDir() {
			return ownershipResolvedPath{}, fmt.Errorf("ownership component %q is not a directory", dataPath)
		}
		child, err := r.readOwnershipNode(parent, component, dataPath, true)
		if err != nil {
			return ownershipResolvedPath{}, err
		}
		keys, err := r.nodeDirectoryKeys(dataPath, child.nodeIDs, child.control, info)
		if err != nil {
			return ownershipResolvedPath{}, err
		}
		resolved.directories = append(resolved.directories, keys)
		parent = child
	}
	if directory {
		return resolved, nil
	}
	leaf := components[len(components)-1]
	if !validOwnershipComponent(leaf) {
		return ownershipResolvedPath{}, fmt.Errorf("invalid ownership path component %q", leaf)
	}
	child, err := r.readOwnershipNode(parent, leaf, filepath.Join(parent.data, leaf), false)
	if err != nil {
		return ownershipResolvedPath{}, err
	}
	identity := strings.Join(child.nodeIDs, "\x00")
	entryKey := r.resourceKey("node-entry", identity)
	childrenKey := r.resourceKey("node-children", identity)
	resolved.itemNodeID = identity
	resolved.itemKeys = []string{entryKey, childrenKey}
	metaDir := filepath.Join(child.control, "metadata")
	resolved.itemRequests = []ownershipLockRequest{
		{key: entryKey, mode: ownershipExclusive, path: filepath.Join(metaDir, "entry.lock")},
		{key: childrenKey, mode: ownershipExclusive, path: filepath.Join(metaDir, "children.lock")},
	}
	return resolved, nil
}

func (r *ownershipRealm) readOwnershipNode(parent ownershipNode, component, dataPath string, requireDirectory bool) (ownershipNode, error) {
	controlPath := filepath.Join(parent.control, "children", component)
	metaDir := filepath.Join(controlPath, "metadata")
	id, err := readOwnershipNodeID(filepath.Join(metaDir, "node-id"))
	if err != nil {
		return ownershipNode{}, err
	}
	child := ownershipNode{
		control: controlPath,
		data:    dataPath,
		id:      id,
		nodeIDs: append(append([]string(nil), parent.nodeIDs...), id),
	}
	if requireDirectory {
		stored, err := readOwnershipCaseProfile(filepath.Join(metaDir, "profile"))
		if err != nil {
			return ownershipNode{}, err
		}
		current, err := ownershipDirectoryCaseProfile(dataPath, filepath.Join(parent.control, "metadata"))
		if err != nil {
			return ownershipNode{}, err
		}
		if stored.Sensitive != current.Sensitive {
			return ownershipNode{}, fmt.Errorf("case profile changed for cache directory %q", dataPath)
		}
		children := filepath.Join(controlPath, "children")
		actual, err := ownershipControlDirectoryCaseProfile(children, filepath.Join(metaDir, "metadata"))
		if err != nil {
			return ownershipNode{}, err
		}
		if actual.Sensitive != current.Sensitive {
			return ownershipNode{}, fmt.Errorf("control case profile changed for cache directory %q", dataPath)
		}
		child.profile = &stored
	}
	return child, nil
}

func (r *ownershipRealm) ensureOwnershipNode(parent ownershipNode, component, dataPath string, requireDirectory bool) (ownershipNode, error) {
	if !validOwnershipComponent(component) {
		return ownershipNode{}, fmt.Errorf("invalid ownership path component %q", component)
	}
	bootstrapPath := filepath.Join(parent.control, "metadata", "child-bootstrap.lock")
	bootstrap, err := ownershipLockOpen(bootstrapPath, ownershipExclusive)
	if err != nil {
		return ownershipNode{}, fmt.Errorf("bootstrap cache node %q: %w", dataPath, err)
	}
	defer bootstrap.release()
	return r.ensureOwnershipNodeLocked(parent, component, dataPath, requireDirectory)
}

// ensureOwnershipNodeLocked requires the parent metadata/child-bootstrap lock.
func (r *ownershipRealm) ensureOwnershipNodeLocked(parent ownershipNode, component, dataPath string, requireDirectory bool) (ownershipNode, error) {
	controlPath := filepath.Join(parent.control, "children", component)
	metaDir := filepath.Join(controlPath, "metadata")
	childrenDir := filepath.Join(controlPath, "children")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return ownershipNode{}, err
	}
	if _, err := loadOrCreateOwnershipNodeID(metaDir, "node-id"); err != nil {
		return ownershipNode{}, err
	}
	id, err := readOwnershipNodeID(filepath.Join(metaDir, "node-id"))
	if err != nil {
		return ownershipNode{}, err
	}
	nodeIDs := append(append([]string(nil), parent.nodeIDs...), id)
	child := ownershipNode{control: controlPath, data: dataPath, id: id, nodeIDs: nodeIDs}
	info, statErr := os.Stat(dataPath)
	if statErr == nil && info.IsDir() && requireDirectory {
		profile, err := ownershipDirectoryCaseProfile(dataPath, filepath.Join(parent.control, "metadata"))
		if err != nil {
			return ownershipNode{}, fmt.Errorf("read cache-node case profile for %q: %w", dataPath, err)
		}
		profilePath := filepath.Join(metaDir, "profile")
		if err := os.MkdirAll(childrenDir, 0o755); err != nil {
			return ownershipNode{}, err
		}
		if err := ensureOwnershipControlDirectoryProfile(childrenDir, profilePath, profile); err != nil {
			return ownershipNode{}, err
		}
		child.profile = &profile
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return ownershipNode{}, statErr
	} else if requireDirectory {
		return ownershipNode{}, fmt.Errorf("cache parent %q is missing during node initialization", dataPath)
	}
	if requireDirectory && statErr == nil && !info.IsDir() {
		return ownershipNode{}, fmt.Errorf("ownership component %q is not a directory", dataPath)
	}
	return child, nil
}

func (r *ownershipRealm) nodeDirectoryKeys(dataPath string, nodeIDs []string, controlPath string, info os.FileInfo) (ownershipDirectoryKeys, error) {
	objectID, err := ownershipDirectoryIdentity(dataPath)
	if err != nil {
		return ownershipDirectoryKeys{}, fmt.Errorf("identify data directory %q: %w", dataPath, err)
	}
	identity := strings.Join(nodeIDs, "\x00")
	keys := ownershipDirectoryKeys{
		path:        dataPath,
		pathKey:     r.resourceKey("node-entry", identity),
		entryKey:    r.resourceKey("node-entry", identity),
		objectID:    objectID,
		objectKey:   r.resourceKey("node-children", identity),
		nodeID:      nodeIDs[len(nodeIDs)-1],
		nodeIDs:     append([]string(nil), nodeIDs...),
		controlPath: controlPath,
	}
	metaDir := filepath.Join(controlPath, "metadata")
	keys.pathLock = filepath.Join(metaDir, "entry.lock")
	keys.entryLock = filepath.Join(metaDir, "entry.lock")
	keys.objectLock = filepath.Join(metaDir, "children.lock")
	if info != nil && !info.IsDir() {
		return ownershipDirectoryKeys{}, fmt.Errorf("ownership node %q is not a directory", dataPath)
	}
	return keys, nil
}

func (r *ownershipRealm) cacheWriterRequests(paths []string) ([]ownershipLockRequest, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("no cache ownership writer paths")
	}
	requests := make([]ownershipLockRequest, 0, len(paths)*8)
	for _, path := range paths {
		resolved, err := r.resolveCacheOwnershipPath(path, false)
		if err != nil {
			return nil, err
		}
		for _, dir := range resolved.directories {
			requests = append(requests, ownershipDirectoryRequests(dir, ownershipShared)...)
		}
		requests = append(requests, resolved.itemRequests...)
	}
	return normalizeOwnershipRequests(requests)
}

func (r *ownershipRealm) cacheWriterPreparationRequests(paths []string) ([]ownershipLockRequest, map[string]bool, []string, []cacheOwnershipPrepPlan, error) {
	if len(paths) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("no cache ownership writer paths")
	}
	requests := make([]ownershipLockRequest, 0, len(paths)*8)
	keep := make(map[string]bool)
	temporary := make(map[string]bool)
	canonical := make([]string, 0, len(paths))
	plans := make([]cacheOwnershipPrepPlan, 0)
	for _, path := range paths {
		if path == "" {
			return nil, nil, nil, nil, fmt.Errorf("cache ownership writer path is empty")
		}
		canonicalPath, err := canonicalOwnershipPath(path, false)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if !ownershipPathWithinRoot(r.dataRoot, canonicalPath) {
			return nil, nil, nil, nil, fmt.Errorf("ownership path %q is outside cache root %q", canonicalPath, r.dataRoot)
		}
		canonical = append(canonical, canonicalPath)
		parentPath := filepath.Dir(canonicalPath)
		existingParent, missingDirs, err := r.nearestExistingCacheDirectory(parentPath)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		parentResolved, err := r.resolveCacheOwnershipPath(existingParent, true)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		parentNode := r.rootNode()
		for _, dir := range parentResolved.directories {
			dirRequests := ownershipDirectoryRequests(dir, ownershipShared)
			requests = append(requests, dirRequests...)
			for _, request := range dirRequests {
				keep[request.key] = true
			}
			if dir.nodeID != "" {
				parentNode = ownershipNode{control: dir.controlPath, data: dir.path, id: dir.nodeID, nodeIDs: append([]string(nil), dir.nodeIDs...)}
			}
		}
		if len(missingDirs) == 0 {
			resolved, err := r.resolveCacheOwnershipPath(canonicalPath, false)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			requests = append(requests, resolved.itemRequests...)
			for _, request := range resolved.itemRequests {
				keep[request.key] = true
			}
			continue
		}
		bootstrapKey := r.resourceKey("node-child-bootstrap", parentNode.id)
		bootstrapPath := filepath.Join(parentNode.control, "metadata", "child-bootstrap.lock")
		requests = append(requests, ownershipLockRequest{key: bootstrapKey, mode: ownershipExclusive, path: bootstrapPath})
		temporary[bootstrapKey] = true
		components := append([]string(nil), missingDirs...)
		components = append(components, filepath.Base(canonicalPath))
		plans = append(plans, cacheOwnershipPrepPlan{
			path: canonicalPath, parent: parentNode, components: components,
			dirCount: len(missingDirs), bootstrapKey: bootstrapKey,
		})
	}
	for key := range temporary {
		delete(keep, key)
	}
	normalized, err := normalizeOwnershipRequests(requests)
	return normalized, keep, canonical, plans, err
}

func (r *ownershipRealm) nearestExistingCacheDirectory(path string) (string, []string, error) {
	missing := make([]string, 0, 4)
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return "", nil, fmt.Errorf("ownership path parent %q is not a directory", current)
			}
			if !ownershipPathWithinRoot(r.dataRoot, current) {
				return "", nil, fmt.Errorf("ownership parent %q is outside cache root %q", current, r.dataRoot)
			}
			for i, j := 0, len(missing)-1; i < j; i, j = i+1, j-1 {
				missing[i], missing[j] = missing[j], missing[i]
			}
			return current, missing, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", nil, err
		}
		next := filepath.Dir(current)
		if next == current {
			return "", nil, err
		}
		missing = append(missing, filepath.Base(current))
	}
}

func (r *ownershipRealm) initializeCachePathSuffix(plan cacheOwnershipPrepPlan, lease *ownershipLease) error {
	if lease == nil || lease.modes[plan.bootstrapKey] != ownershipExclusive {
		return fmt.Errorf("cache path preparation lost its child-bootstrap gate")
	}
	parent := plan.parent
	for i, component := range plan.components {
		dataPath := filepath.Join(parent.data, component)
		child, err := r.ensureOwnershipNodeLocked(parent, component, dataPath, i < plan.dirCount)
		if err != nil {
			return err
		}
		parent = child
	}
	return nil
}

func ownershipPathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func validOwnershipComponent(component string) bool {
	return component != "" && component != "." && component != ".." &&
		!strings.ContainsAny(component, "/\\\x00")
}

func splitOwnershipRelativePath(rel string) []string {
	if rel == "." || rel == "" {
		return nil
	}
	return strings.Split(rel, string(filepath.Separator))
}

func readOwnershipNodeID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if len(id) != 32 {
		return "", fmt.Errorf("ownership node ID %s is malformed", path)
	}
	return id, nil
}
