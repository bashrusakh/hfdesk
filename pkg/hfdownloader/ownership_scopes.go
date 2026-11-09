// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var errOwnershipPathChanged = errors.New("ownership path changed during admission")

// errOwnershipAdmissionUnestablished reports that destructive admission could
// establish neither a live-writer conflict nor a re-verified vanish. It never
// wraps os.ErrNotExist: "admission could not be established" must never be
// readable by a caller as "the target is gone".
var errOwnershipAdmissionUnestablished = errors.New("cache ownership admission could not be established")

// admissionUnestablishedError re-identifies a not-exist-wrapped failure from
// admission resolution so no failed admission satisfies errors.Is(err,
// os.ErrNotExist); any other error, including a busy refusal, keeps its identity.
func admissionUnestablishedError(target string, err error) error {
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fmt.Errorf("%w: removal target %q (%v)", errOwnershipAdmissionUnestablished, target, err)
}

type ownershipDirectoryKeys struct {
	path        string
	pathKey     string
	entryKey    string
	objectKey   string
	objectID    string
	nodeID      string
	nodeIDs     []string
	pathLock    string
	entryLock   string
	objectLock  string
	controlPath string
}

type ownershipResolvedPath struct {
	canonical    string
	directories  []ownershipDirectoryKeys
	itemKeys     []string
	itemRequests []ownershipLockRequest
	itemNodeID   string
	nodeRealm    *ownershipRealm
}

type ownershipTreeNode struct {
	path     string
	objectID string
}

func appendUniqueOwnershipPaths(paths []string, more ...string) []string {
	seen := make(map[string]bool, len(paths)+len(more))
	result := make([]string, 0, len(paths)+len(more))
	for _, path := range append(append([]string(nil), paths...), more...) {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		result = append(result, path)
	}
	return result
}

func canonicalOwnershipPath(name string, directory bool) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(name))
	if err != nil {
		return "", err
	}
	if directory {
		info, err := os.Lstat(abs)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("ownership tree target %q is not a real directory", abs)
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", err
		}
		return filepath.Clean(resolved), nil
	}

	parent := filepath.Dir(abs)
	base := filepath.Base(abs)
	missing := make([]string, 0, 4)
	for current := parent; ; current = filepath.Dir(current) {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			info, statErr := os.Stat(current)
			if statErr != nil {
				return "", statErr
			}
			if !info.IsDir() {
				return "", fmt.Errorf("ownership path parent %q is not a directory", current)
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(filepath.Join(resolved, base)), nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", resolveErr
		}
		next := filepath.Dir(current)
		if next == current {
			return "", resolveErr
		}
		missing = append(missing, filepath.Base(current))
	}
}

func canonicalOwnershipDirectoryPath(name string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(name))
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err == nil {
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", err
		}
		info, err = os.Stat(resolved)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("ownership path %q is not a directory", abs)
		}
		return filepath.Clean(resolved), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return canonicalOwnershipPath(abs, false)
}

func ownershipPathIsWithin(base, target string) (bool, error) {
	rootKey, baseParts, err := ownershipNormalizedPathParts(base)
	if err != nil {
		return false, err
	}
	targetRootKey, targetParts, err := ownershipNormalizedPathParts(target)
	if err != nil {
		return false, err
	}
	if rootKey != targetRootKey || len(baseParts) > len(targetParts) {
		return false, nil
	}
	for i := range baseParts {
		if baseParts[i] != targetParts[i] {
			return false, nil
		}
	}
	return true, nil
}

func validateOwnershipRealmOutside(realm *ownershipRealm, storageRoots ...string) error {
	if realm == nil {
		return fmt.Errorf("ownership realm is unavailable")
	}
	for _, root := range storageRoots {
		if root == "" {
			continue
		}
		insideRealm, err := ownershipPathIsWithin(realm.root, root)
		if err != nil {
			return fmt.Errorf("compare ownership realm with storage root %q: %w", root, err)
		}
		insideStorage, err := ownershipPathIsWithin(root, realm.root)
		if err != nil {
			return fmt.Errorf("compare storage root %q with ownership realm: %w", root, err)
		}
		if insideRealm || insideStorage {
			return fmt.Errorf("ownership realm %q must not overlap storage root %q", realm.root, root)
		}
	}
	return nil
}

func (r *ownershipRealm) ensureDirectories(ctx context.Context, dirs []string, wait time.Duration) error {
	for _, dir := range dirs {
		lease, err := r.acquireWriterWithWait(ctx, []string{filepath.Join(dir, ".hfdesk-directory-admission")}, wait, nil)
		if err != nil {
			return fmt.Errorf("admit directory %q: %w", dir, err)
		}
		lease.Release()
	}
	return nil
}

func ownershipNormalizedPathParts(path string) (string, []string, error) {
	canonical, err := canonicalOwnershipDirectoryPath(path)
	if err != nil {
		return "", nil, err
	}
	root, parts := ownershipPathParts(canonical)
	keys := make([]string, 0, len(parts))
	parent := root
	for _, part := range parts {
		key, err := ownershipComponentKey(parent, part)
		if err != nil {
			return "", nil, err
		}
		keys = append(keys, key)
		parent = filepath.Join(parent, part)
	}
	return ownershipRootKey(root), keys, nil
}

func ownershipPathParts(abs string) (string, []string) {
	volume := filepath.VolumeName(abs)
	root := volume + string(filepath.Separator)
	if volume == "" {
		root = string(filepath.Separator)
	}
	rest := strings.TrimPrefix(abs, volume)
	rest = strings.TrimLeft(rest, string(filepath.Separator))
	if rest == "" {
		return filepath.Clean(root), nil
	}
	return filepath.Clean(root), strings.Split(rest, string(filepath.Separator))
}

func ownershipRootKey(root string) string { return ownershipVolumeKey(root) }

func (r *ownershipRealm) resolveOwnershipPath(name string, directory bool) (ownershipResolvedPath, error) {
	if r.cacheOwned {
		return r.resolveCacheOwnershipPath(name, directory)
	}
	canonical, err := canonicalOwnershipPath(name, directory)
	if err != nil {
		return ownershipResolvedPath{}, err
	}
	root, parts := ownershipPathParts(canonical)
	rootID, err := statDirectoryIdentity(root)
	if err != nil {
		return ownershipResolvedPath{}, fmt.Errorf("identify ownership root %q: %w", root, err)
	}
	rootPathKey := ownershipRootKey(root)
	parentToken := "object:" + rootID
	parentPath := root
	pathToken := rootPathKey
	// Flat path-derived identity is anchored to the realm: when the target is
	// on the realm's volume, its path tokens derive from the path RELATIVE to
	// the realm root, so any common-prefix movement of the whole realm+item
	// topology cancels out and hosts mounting one shared tree at different
	// prefixes derive identical keys and lock filenames. A target on another
	// volume (Windows cross-drive) has no relative form and keeps the
	// absolute-spelling chain — unchanged same-host behavior for split
	// topologies. The alias dimension (entry and object keys) never uses this
	// and stays host-local on purpose.
	relBase := ownershipSpelledPath(r.root)
	targetRel, relErr := filepath.Rel(relBase, ownershipSpelledPath(canonical))
	anchored := relErr == nil
	resolved := ownershipResolvedPath{canonical: canonical}
	missing := false
	limit := len(parts)
	if !directory && limit > 0 {
		limit--
	}
	for i := 0; i < limit; i++ {
		nameKey, keyErr := ownershipComponentKey(parentPath, parts[i])
		if keyErr != nil {
			return ownershipResolvedPath{}, fmt.Errorf("canonicalize ownership component %q: %w", parts[i], keyErr)
		}
		entryID := parentToken + "\x00" + nameKey
		current := filepath.Join(parentPath, parts[i])
		if anchored {
			rel, relWalkErr := filepath.Rel(relBase, ownershipSpelledPath(current))
			if relWalkErr != nil {
				return ownershipResolvedPath{}, fmt.Errorf("derive realm-relative ownership path for %q: %w", current, relWalkErr)
			}
			pathToken, keyErr = r.ownershipRelPathToken(rel)
			if keyErr != nil {
				return ownershipResolvedPath{}, fmt.Errorf("key realm-relative ownership path for %q: %w", current, keyErr)
			}
		} else {
			pathToken += "\x00" + nameKey
		}
		keys := ownershipDirectoryKeys{
			path:     current,
			pathKey:  r.resourceKey("directory-path", pathToken),
			entryKey: r.resourceKey("directory-entry", entryID),
		}
		if !missing {
			info, statErr := os.Stat(current)
			switch {
			case statErr == nil:
				if !info.IsDir() {
					return ownershipResolvedPath{}, fmt.Errorf("ownership component %q is not a directory", current)
				}
				objectID, idErr := statDirectoryIdentity(current)
				if idErr != nil {
					return ownershipResolvedPath{}, fmt.Errorf("identify ownership directory %q: %w", current, idErr)
				}
				keys.objectID = objectID
				keys.objectKey = r.resourceKey("directory-object", objectID)
				parentToken = "object:" + objectID
			case errors.Is(statErr, os.ErrNotExist):
				missing = true
				parentToken = "virtual:" + entryID
			case statErr != nil:
				return ownershipResolvedPath{}, statErr
			}
		} else {
			parentToken = "virtual:" + entryID
		}
		resolved.directories = append(resolved.directories, keys)
		parentPath = current
	}

	if directory {
		if len(parts) == 0 {
			rootPathToken := rootPathKey
			if anchored {
				rootPathToken, err = r.ownershipRelPathToken(targetRel)
				if err != nil {
					return ownershipResolvedPath{}, fmt.Errorf("key realm-relative ownership path for %q: %w", root, err)
				}
			}
			resolved.directories = append(resolved.directories, ownershipDirectoryKeys{
				path:      root,
				pathKey:   r.resourceKey("directory-path", rootPathToken),
				entryKey:  r.resourceKey("directory-entry", "root\x00"+rootPathKey),
				objectID:  rootID,
				objectKey: r.resourceKey("directory-object", rootID),
			})
			return resolved, nil
		}
		if len(resolved.directories) == 0 || resolved.directories[len(resolved.directories)-1].objectKey == "" {
			return ownershipResolvedPath{}, fmt.Errorf("ownership tree target %q is missing", canonical)
		}
		return resolved, nil
	}

	if len(parts) == 0 {
		return ownershipResolvedPath{}, fmt.Errorf("ownership item path %q has no filename", canonical)
	}
	nameKey, err := ownershipComponentKey(parentPath, parts[len(parts)-1])
	if err != nil {
		return ownershipResolvedPath{}, fmt.Errorf("canonicalize ownership entry %q: %w", parts[len(parts)-1], err)
	}
	itemPathToken := pathToken + "\x00" + nameKey
	if anchored {
		itemPathToken, err = r.ownershipRelPathToken(targetRel)
		if err != nil {
			return ownershipResolvedPath{}, fmt.Errorf("key realm-relative ownership path for %q: %w", canonical, err)
		}
	}
	entryID := parentToken + "\x00" + nameKey
	resolved.itemKeys = []string{
		r.resourceKey("item-path", itemPathToken),
		r.resourceKey("item-entry", entryID),
	}
	return resolved, nil
}

// ownershipRelPathToken converts a path relative to the realm root into the
// flat path-derived identity material. The token is anchored at the realm (the
// persisted realm ID joins it in resourceKey), so a common-prefix movement of
// the realm+item topology cancels out; ".." steps toward paths outside the
// realm stay raw markers, and real components keep the platform component rule
// against their real parents. The rel-to-target direction is well defined: a
// relative path determines exactly one target under one realm root. The
// reverse direction is NOT injective on Windows: filepath.Rel folds case
// aliases unconditionally (EqualFold element matching), so two distinct
// targets in a case-SENSITIVE directory can map to one token — spurious
// mutual exclusion between distinct items, in the safe direction only:
// over-locking, never data mixing; staging/destination paths, entry/object
// keys, fences and resume identities stay distinct.
func (r *ownershipRealm) ownershipRelPathToken(rel string) (string, error) {
	token := "realm-rel-v1"
	current := r.root
	for _, component := range splitOwnershipRelativePath(rel) {
		if component == ".." {
			token += "\x00.."
			current = filepath.Dir(current)
			continue
		}
		key, err := ownershipComponentKey(current, component)
		if err != nil {
			return "", err
		}
		token += "\x00" + key
		current = filepath.Join(current, component)
	}
	return token, nil
}

func ownershipDirectoryRequests(keys ownershipDirectoryKeys, mode ownershipLockMode) []ownershipLockRequest {
	requests := []ownershipLockRequest{
		{key: keys.pathKey, mode: mode, path: keys.pathLock},
		{key: keys.entryKey, mode: mode, path: keys.entryLock},
	}
	if keys.objectKey != "" {
		requests = append(requests, ownershipLockRequest{key: keys.objectKey, mode: mode, path: keys.objectLock})
	}
	return requests
}

func (r *ownershipRealm) writerRequests(paths []string) ([]ownershipLockRequest, error) {
	if r.cacheOwned {
		return r.cacheWriterRequests(paths)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no ownership writer paths")
	}
	requests := make([]ownershipLockRequest, 0, len(paths)*4)
	for _, path := range paths {
		if path == "" {
			return nil, fmt.Errorf("ownership writer path is empty")
		}
		resolved, err := r.resolveOwnershipPath(path, false)
		if err != nil {
			return nil, err
		}
		for _, dir := range resolved.directories {
			requests = append(requests, ownershipDirectoryRequests(dir, ownershipShared)...)
		}
		for _, key := range resolved.itemKeys {
			requests = append(requests, ownershipLockRequest{key: key, mode: ownershipExclusive})
		}
		requests = append(requests, resolved.itemRequests...)
	}
	return normalizeOwnershipRequests(requests)
}

func (r *ownershipRealm) writerRequestsPrepared(paths []string, preparation *ownershipLease) ([]ownershipLockRequest, error) {
	if !r.cacheOwned {
		return r.writerRequests(paths)
	}
	requests := make([]ownershipLockRequest, 0, len(paths)*8)
	for _, path := range paths {
		resolved, err := r.resolveCacheOwnershipPathWithLease(path, false, preparation)
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

type ownershipPreparation struct {
	realm      *ownershipRealm
	paths      []string
	canonical  []string
	lease      *ownershipLease
	keep       map[string]bool
	created    bool
	completed  bool
	cachePlans []cacheOwnershipPrepPlan
}

func (r *ownershipRealm) writerPreparationRequests(paths []string) ([]ownershipLockRequest, map[string]bool, []string, []cacheOwnershipPrepPlan, error) {
	if r.cacheOwned {
		return r.cacheWriterPreparationRequests(paths)
	}
	if len(paths) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("no ownership writer paths")
	}
	requests := make([]ownershipLockRequest, 0, len(paths)*8)
	keep := make(map[string]bool)
	temporary := make(map[string]bool)
	canonical := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			return nil, nil, nil, nil, fmt.Errorf("ownership writer path is empty")
		}
		resolved, err := r.resolveOwnershipPath(path, false)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		canonical = append(canonical, resolved.canonical)
		parentExists := len(resolved.directories) == 0 || resolved.directories[len(resolved.directories)-1].objectKey != ""
		for _, dir := range resolved.directories {
			mode := ownershipShared
			if dir.objectKey == "" {
				mode = ownershipExclusive
			}
			dirRequests := ownershipDirectoryRequests(dir, mode)
			requests = append(requests, dirRequests...)
			for _, request := range dirRequests {
				if mode == ownershipExclusive {
					temporary[request.key] = true
				} else {
					keep[request.key] = true
				}
			}
		}
		for i, key := range resolved.itemKeys {
			requests = append(requests, ownershipLockRequest{key: key, mode: ownershipExclusive})
			if i == 0 || parentExists {
				keep[key] = true
			} else {
				temporary[key] = true
			}
		}
	}
	for key := range temporary {
		delete(keep, key)
	}
	normalized, err := normalizeOwnershipRequests(requests)
	return normalized, keep, canonical, nil, err
}

// prepareWriter holds short exclusive admission gates for missing directory
// entries. The caller may create only the requested parent directories while
// holding this preparation, then must Promote before any payload or claim I/O.
func (r *ownershipRealm) prepareWriter(ctx context.Context, paths []string, wait time.Duration) (*ownershipPreparation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requests, keep, canonical, plans, err := r.writerPreparationRequests(paths)
	if err != nil {
		return nil, err
	}
	lease, err := r.acquire(ctx, requests, wait)
	if err != nil {
		return nil, err
	}
	return &ownershipPreparation{
		realm:      r,
		paths:      append([]string(nil), canonical...),
		canonical:  canonical,
		lease:      lease,
		keep:       keep,
		cachePlans: plans,
	}, nil
}

func (p *ownershipPreparation) CreateParents(ctx context.Context) error {
	if p == nil {
		return fmt.Errorf("ownership preparation is not open for directory creation")
	}
	if p.completed || p.created {
		if !p.completed {
			p.Release()
		}
		return fmt.Errorf("ownership preparation is not open for directory creation")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for _, path := range p.paths {
		if err := ctx.Err(); err != nil {
			p.Release()
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			p.Release()
			return fmt.Errorf("create writer parent directory: %w", err)
		}
	}
	for _, plan := range p.cachePlans {
		if err := p.realm.initializeCachePathSuffix(plan, p.lease); err != nil {
			p.Release()
			return fmt.Errorf("initialize cache ownership nodes for %q: %w", plan.path, err)
		}
	}
	p.created = true
	return nil
}

// Promote acquires the final identity set before releasing temporary creation
// gates. On contention it releases the preparation; the caller must prepare
// again rather than waiting while holding those short gates.
func (p *ownershipPreparation) Promote(ctx context.Context) (*ownershipLease, error) {
	if p == nil {
		return nil, fmt.Errorf("ownership preparation is nil")
	}
	if p.completed {
		return nil, fmt.Errorf("ownership preparation is complete")
	}
	if !p.created {
		p.Release()
		return nil, fmt.Errorf("ownership preparation must create parent directories before promotion")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for i, path := range p.paths {
		current, err := canonicalOwnershipPath(path, false)
		if err != nil || current != p.canonical[i] {
			p.Release()
			if err != nil {
				return nil, fmt.Errorf("revalidate writer path: %w", err)
			}
			return nil, errOwnershipPathChanged
		}
	}
	finalRequests, err := p.realm.writerRequestsPrepared(p.paths, p.lease)
	if err != nil {
		p.Release()
		return nil, err
	}
	missing := make([]ownershipLockRequest, 0, len(finalRequests))
	transfer := make(map[string]bool)
	for _, request := range finalRequests {
		if held, ok := p.lease.modes[request.key]; ok {
			if held < request.mode {
				p.Release()
				return nil, errOwnershipBusy
			}
			if p.keep[request.key] {
				transfer[request.key] = true
			}
			continue
		}
		missing = append(missing, request)
	}
	finalLease, err := p.realm.tryAcquire(ctx, missing)
	if err != nil {
		p.Release()
		return nil, err
	}
	latestRequests, err := p.realm.writerRequestsPrepared(p.paths, p.lease)
	if err != nil || !sameOwnershipRequests(finalRequests, latestRequests) {
		finalLease.Release()
		p.Release()
		if err != nil {
			return nil, err
		}
		return nil, errOwnershipPathChanged
	}
	p.lease.transferTo(finalLease, transfer)
	p.Release()
	return finalLease, nil
}

func (p *ownershipPreparation) Release() {
	if p == nil || p.completed {
		return
	}
	p.completed = true
	if p.lease != nil {
		p.lease.Release()
		p.lease = nil
	}
}

func sameOwnershipRequests(a, b []ownershipLockRequest) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (r *ownershipRealm) acquireWriter(ctx context.Context, paths []string, wait time.Duration) (*ownershipLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return retryOwnership(ctx, wait, func() (*ownershipLease, error) {
		preparation, err := r.prepareWriter(ctx, paths, 0)
		if err != nil {
			return nil, err
		}
		if err := preparation.CreateParents(ctx); err != nil {
			return nil, err
		}
		return preparation.Promote(ctx)
	})
}

type ownershipPathSet struct {
	realm *ownershipRealm
	paths []string
}

// acquireOwnershipPathSets admits a complete H/R path set. Preparation is
// acquired in a stable order without waiting while partial gates are held; the
// final regular-file locks are attempted as one sorted, rollback-capable set.
func acquireOwnershipPathSets(ctx context.Context, wait time.Duration, sets ...ownershipPathSet) (*ownershipLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sets, err := normalizeOwnershipPathSets(sets)
	if err != nil {
		return nil, err
	}
	notice := downloadSlotWaitNoticeFromContext(ctx)
	waiting := false
	onWait := func() {
		if !waiting && notice != nil {
			waiting = true
			notice.beginWait()
		}
	}
	lease, err := retryOwnershipNotify(ctx, wait, func() (*ownershipLease, error) {
		preparations := make([]*ownershipPreparation, 0, len(sets))
		releasePreparations := func() {
			for _, preparation := range preparations {
				preparation.Release()
			}
		}
		for _, set := range sets {
			preparation, err := set.realm.prepareWriter(ctx, set.paths, 0)
			if err != nil {
				releasePreparations()
				return nil, err
			}
			preparations = append(preparations, preparation)
		}
		for _, preparation := range preparations {
			if err := preparation.CreateParents(ctx); err != nil {
				releasePreparations()
				return nil, err
			}
		}

		requestsByPreparation := make([][]ownershipLockRequest, len(preparations))
		allRequests := make([]ownershipLockRequest, 0)
		transfer := make(map[string]bool)
		for i, preparation := range preparations {
			requests, err := preparation.realm.writerRequestsPrepared(preparation.paths, preparation.lease)
			if err != nil {
				releasePreparations()
				return nil, err
			}
			requestsByPreparation[i] = requests
			for _, request := range requests {
				held, ok := preparation.lease.modes[request.key]
				if ok && held >= request.mode && preparation.keep[request.key] {
					transfer[request.key] = true
					continue
				}
				allRequests = append(allRequests, request)
			}
		}
		missing, err := normalizeOwnershipRequests(allRequests)
		if err != nil {
			releasePreparations()
			return nil, err
		}
		finalLease, err := preparations[0].realm.tryAcquire(ctx, missing)
		if err != nil {
			releasePreparations()
			return nil, err
		}
		for i, preparation := range preparations {
			latest, err := preparation.realm.writerRequestsPrepared(preparation.paths, preparation.lease)
			if err != nil || !sameOwnershipRequests(requestsByPreparation[i], latest) {
				finalLease.Release()
				releasePreparations()
				if err != nil {
					return nil, err
				}
				return nil, errOwnershipPathChanged
			}
		}
		for _, preparation := range preparations {
			preparation.lease.transferTo(finalLease, transfer)
			preparation.Release()
		}
		return finalLease, nil
	}, onWait)
	if err != nil {
		return nil, err
	}
	if waiting && notice != nil && !notice.endWait() {
		lease.Release()
		return nil, ctx.Err()
	}
	return lease, nil
}

func normalizeOwnershipPathSets(sets []ownershipPathSet) ([]ownershipPathSet, error) {
	byRealm := make(map[string]int, len(sets))
	result := make([]ownershipPathSet, 0, len(sets))
	for _, set := range sets {
		if set.realm == nil || !set.realm.cacheOwned || len(set.paths) == 0 {
			return nil, fmt.Errorf("cross-root ownership requires initialized cache realms and paths")
		}
		key := set.realm.id + "\x00" + set.realm.lockDir
		if index, ok := byRealm[key]; ok {
			result[index].paths = appendUniqueOwnershipPaths(result[index].paths, set.paths...)
			continue
		}
		byRealm[key] = len(result)
		result = append(result, ownershipPathSet{realm: set.realm, paths: appendUniqueOwnershipPaths(nil, set.paths...)})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no ownership path sets")
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].realm.id != result[j].realm.id {
			return result[i].realm.id < result[j].realm.id
		}
		return result[i].realm.lockDir < result[j].realm.lockDir
	})
	return result, nil
}

func (r *ownershipRealm) acquireTreeRemoval(ctx context.Context, target string, wait time.Duration) (*ownershipLease, error) {
	return r.acquireTreeRemovals(ctx, []string{target}, wait)
}

func (r *ownershipRealm) acquireTreeRemovals(ctx context.Context, targets []string, wait time.Duration) (*ownershipLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return retryOwnership(ctx, wait, func() (*ownershipLease, error) {
		return r.tryTreeRemovals(ctx, targets)
	})
}

func (r *ownershipRealm) tryTreeRemovals(ctx context.Context, targets []string) (lease *ownershipLease, err error) {
	// Whole-tree admission holds two open lock files per directory in the
	// cache realm (entry and children locks) and three per existing
	// directory in the flat realm (path, entry and object keys) from the
	// first exclusive acquisition until the caller's removal finishes (the
	// walk below), so the process soft file-descriptor limit is raised
	// toward the hard limit for exactly this window and restored with this
	// lease's release. An exhaustion that even the hard limit cannot cover
	// surfaces below as the actionable limit error, never a raw EMFILE.
	releaseFileLimit := holdOwnershipFileLimit()
	nodes := make([]ownershipTreeNode, 0)
	defer func() {
		if lease == nil {
			releaseFileLimit()
		}
		if err != nil && ownershipFileDescriptorPressure(err) {
			err = ownershipTreeFileLimitError(r.cacheOwned, len(nodes), err)
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no ownership tree targets")
	}
	ownerRoot, err := r.resolveOwnershipPath(r.root, true)
	if err != nil {
		return nil, fmt.Errorf("resolve ownership realm root: %w", err)
	}
	ownerLockDir, err := r.resolveOwnershipPath(r.lockDir, true)
	if err != nil {
		return nil, fmt.Errorf("resolve ownership lock directory: %w", err)
	}
	type root struct {
		path string
		keys []ownershipDirectoryKeys
	}
	roots := make([]root, 0, len(targets))
	requests := make([]ownershipLockRequest, 0, len(targets)*8)
	for _, target := range targets {
		resolved, err := r.resolveOwnershipPath(target, true)
		if err != nil {
			return nil, err
		}
		for i, dir := range resolved.directories {
			if dir.objectID == r.lockDirObjectID || (i == len(resolved.directories)-1 && dir.objectID == r.rootObjectID) {
				return nil, fmt.Errorf("refuse deletion scope inside the external ownership lock directory")
			}
		}
		if ownershipDirectoryPathPrefix(resolved.directories, ownerRoot.directories) ||
			ownershipDirectoryPathPrefix(resolved.directories, ownerLockDir.directories) ||
			ownershipDirectoryPathPrefix(ownerLockDir.directories, resolved.directories) {
			return nil, fmt.Errorf("refuse deletion scope overlapping the external ownership realm")
		}
		if len(resolved.directories) == 0 {
			return nil, fmt.Errorf("ownership tree target %q has no directory identity", target)
		}
		last := len(resolved.directories) - 1
		for i, dir := range resolved.directories {
			mode := ownershipShared
			if i == last {
				mode = ownershipExclusive
			}
			requests = append(requests, ownershipDirectoryRequests(dir, mode)...)
		}
		roots = append(roots, root{path: resolved.canonical, keys: resolved.directories})
	}
	lease, err = r.tryAcquire(ctx, requests)
	if err != nil {
		return nil, err
	}
	lease.onRelease = releaseFileLimit
	visited := make(map[string]bool)
	// Each parent is EX before it is read; every real child directory is EX
	// before descent, including objects reached through another path view.
	var walk func(string, ownershipDirectoryKeys) error
	walk = func(path string, keys ownershipDirectoryKeys) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("ownership traversal target %q is not a real directory", path)
		}
		if keys.objectID == r.rootObjectID || keys.objectID == r.lockDirObjectID {
			return fmt.Errorf("refuse deletion traversal into the external ownership realm")
		}
		nodes = append(nodes, ownershipTreeNode{path: path, objectID: keys.objectID})
		if keys.objectID != "" && visited[keys.objectID] {
			return nil
		}
		if keys.objectID != "" {
			visited[keys.objectID] = true
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			entryInfo, err := entry.Info()
			if err != nil {
				return err
			}
			if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.IsDir() {
				continue
			}
			childPath := filepath.Join(path, entry.Name())
			child, err := r.resolveOwnershipPath(childPath, true)
			if err != nil {
				return err
			}
			childKeys := child.directories[len(child.directories)-1]
			if err := lease.tryAdd(ctx, r, ownershipDirectoryRequests(childKeys, ownershipExclusive)); err != nil {
				return err
			}
			if err := walk(child.canonical, childKeys); err != nil {
				return err
			}
		}
		return nil
	}
	for _, root := range roots {
		if err := walk(root.path, root.keys[len(root.keys)-1]); err != nil {
			lease.Release()
			return nil, err
		}
	}
	if err := validateOwnershipTree(nodes); err != nil {
		lease.Release()
		return nil, err
	}
	lease.validate = func() error { return validateOwnershipTree(nodes) }
	return lease, nil
}

func validateOwnershipTree(nodes []ownershipTreeNode) error {
	for _, node := range nodes {
		current, err := os.Lstat(node.path)
		if err != nil {
			return fmt.Errorf("revalidate ownership directory %q: %w", node.path, err)
		}
		if current.Mode()&os.ModeSymlink != 0 || !current.IsDir() {
			return fmt.Errorf("%w: %s", errOwnershipPathChanged, node.path)
		}
		objectID, err := statDirectoryIdentity(node.path)
		if err != nil {
			return fmt.Errorf("reidentify ownership directory %q: %w", node.path, err)
		}
		if objectID != node.objectID {
			return fmt.Errorf("%w: %s", errOwnershipPathChanged, node.path)
		}
	}
	return nil
}

func ownershipDirectoryPathPrefix(prefix, path []ownershipDirectoryKeys) bool {
	if len(prefix) > len(path) {
		return false
	}
	for i := range prefix {
		if prefix[i].pathKey != path[i].pathKey {
			return false
		}
	}
	return true
}

// ErrOwnershipBusy reports a refused cache admission: a conflicting scope is held by
// another participant — this process or any other sharing the cache.
var ErrOwnershipBusy = errOwnershipBusy

// CacheAdmission issues non-blocking destructive/mutating admissions against the
// cache-owned control tree of one data root: HubDir for hub trees and files, the
// cache root for friendly-view trees. All lock state lives in that control tree and
// AdmitTreeRemoval never creates payload/data-tree entries (path resolution may
// still create hidden control-node metadata); AdmitFileWrite, through the writer
// preparation, may create a missing payload parent directory (mkdir only) before it
// takes entry admission. Neither writes payload content.
type CacheAdmission struct {
	realm *ownershipRealm
}

// OpenCacheAdmission resolves dataRoot's cache-owned control realm with the identity every participant of that cache derives,
// creating the data root's hidden .hfdesk-control control root when it is absent.
func OpenCacheAdmission(ctx context.Context, dataRoot string) (*CacheAdmission, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if dataRoot == "" {
		return nil, fmt.Errorf("cache admission data root is empty")
	}
	realm, err := newCacheOwnershipRealmContext(ctx, dataRoot, filepath.Join(dataRoot, ".hfdesk-control"), blobOwnershipWaitBudget)
	if err != nil {
		return nil, err
	}
	return &CacheAdmission{realm: realm}, nil
}

// AdmitTreeRemoval takes exclusive admission for the tree at target — shared on ancestors,
// exclusive on the target before any enumeration or descent — and returns the release held
// until removal finishes. It never waits: a live writer under or above the tree refuses
// with ErrOwnershipBusy.
//
// Re-verified-vanish rule: a target that no longer exists cannot be admitted by
// enumeration, and a bare or wrapped ENOENT from resolution is not by itself proof
// that the target is gone — it can also come from a mid-resolution race or a control
// node that could not be bootstrapped while the tree still exists. On any ENOENT the
// call first establishes the creator-side admission for the target — the same keys
// cacheWriterPreparationRequests takes for a missing path (shared ancestor entries,
// the parent's child-bootstrap gate when the chain is missing, and the target's
// creation entry keys) — creating no payload entries while it does so (resolution
// may still create hidden control-node metadata), and re-verifies the target under
// it. Only a target still gone while that admission is held is returned as an
// admitted removal, with the creator-side release held through the caller's tolerant
// removal so a creator racing the removal is excluded. A target found to exist
// escalates back to full tree admission; when admission cannot be established at
// all, the call fails closed — never with an os.ErrNotExist identity — instead of
// returning a release.
func (a *CacheAdmission) AdmitTreeRemoval(ctx context.Context, target string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	const maxVanishReverifyAttempts = 3
	var lastNotFound error
	for attempt := 0; attempt < maxVanishReverifyAttempts; attempt++ {
		lease, treeErr := a.realm.acquireTreeRemoval(ctx, target, 0)
		if treeErr == nil {
			return lease.Release, nil
		}
		if !errors.Is(treeErr, os.ErrNotExist) {
			return nil, treeErr
		}
		lastNotFound = treeErr
		// The target reported gone. Establish the creator-side admission a
		// creator of this target would take (never waiting while holding it)
		// and re-verify the vanish before anything proceeds.
		prep, prepErr := a.realm.prepareWriter(ctx, []string{target}, 0)
		if prepErr != nil {
			// Busy: a creator is active — refusal, not vanish, and the busy
			// identity passes through untouched so callers keep their 409.
			// Any other failure means admission could not be established —
			// fail closed, re-identifying a not-exist-wrapped cause so no
			// failed admission is ever readable as a vanished target.
			return nil, admissionUnestablishedError(target, prepErr)
		}
		_, statErr := os.Lstat(prep.canonical[0])
		switch {
		case statErr == nil:
			// The target exists again: this was not a vanish. Release the
			// creator-side gate before escalating to full tree admission.
			prep.Release()
		case errors.Is(statErr, os.ErrNotExist):
			// Re-verified vanished: hold the creator-side admission as the
			// removal's release; nothing can create the target meanwhile.
			return prep.Release, nil
		default:
			prep.Release()
			return nil, fmt.Errorf("re-verify vanished removal target %q: %w", target, statErr)
		}
	}
	return nil, admissionUnestablishedError(target, lastNotFound)
}

// AdmitFileWrite takes entry-level admission for overwriting the single file at path —
// the item keys a writer of that file holds — and returns the release held until the
// write finishes. Missing payload parent directories are created (mkdir only) before
// entry admission. It never waits; a live writer refuses.
func (a *CacheAdmission) AdmitFileWrite(ctx context.Context, path string) (func(), error) {
	lease, err := a.realm.acquireWriter(ctx, []string{path}, 0)
	if err != nil {
		return nil, err
	}
	return lease.Release, nil
}

// AdmitEntryReplacement takes entry-level admission for replacing or removing the
// single entry — a regular file or a symlink leaf — at path: the same per-entry keys
// AdmitFileWrite takes, held by the caller across the unlink of the old entry and
// the creation of its replacement (or across a single-entry removal). The caller
// performs the entry mutation itself; this method performs no payload I/O. It never
// waits; a live writer, replacer or remover of that entry refuses with
// ErrOwnershipBusy, wrapped with the entry path.
func (a *CacheAdmission) AdmitEntryReplacement(ctx context.Context, path string) (func(), error) {
	lease, err := a.realm.acquireWriter(ctx, []string{path}, 0)
	if err != nil {
		return nil, fmt.Errorf("admit entry replacement %q: %w", path, err)
	}
	return lease.Release, nil
}

// AdmitDirectories admits and performs directory creation — mkdir of each listed
// directory together with its missing parents, and nothing else — under the shared
// directory-creation admission. A directory that already exists needs no admission
// because no mutation happens. It never waits; a live writer or tree removal that
// holds a listed directory refuses with ErrOwnershipBusy.
func (a *CacheAdmission) AdmitDirectories(ctx context.Context, dirs []string) error {
	return a.realm.admitDirectoryCreation(ctx, dirs, 0)
}

// admitDirectoryCreation is the mkdir-only entry into ensureDirectories: existing
// directories are skipped without taking admission, so a caller pays admission only
// for creation that actually happens.
func (r *ownershipRealm) admitDirectoryCreation(ctx context.Context, dirs []string, wait time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
			continue
		}
		if err := r.ensureDirectories(ctx, []string{dir}, wait); err != nil {
			return err
		}
	}
	return nil
}

// FlatAdmission issues non-blocking admissions against the flat OutputDir
// ownership realm — the hashed lock files under OwnershipDir — that flat-mode
// writers coordinate through. The flat realm has no control tree: lock state
// lives entirely in that realm directory, and its key space is independent of
// the cache-owned H/R control trees.
type FlatAdmission struct {
	realm *ownershipRealm
}

// OpenFlatAdmission resolves ownershipDir exactly the way flat writers resolve
// it — the per-user DefaultOwnershipDir when empty — and opens that realm with
// the same identity, so both sides hash their locks into the same key space.
// Participants genuinely exclude each other when they read the same realm id
// from one shared OwnershipDir AND their realm-relative path keys converge:
// under one common shared mount, common-prefix movement cancels; on a split
// topology the rel-token input must match — the same structure below the
// lowest common ancestor and the same descending component spellings; the
// alias (dev:ino) dimension stays host-local. Resolving the same OwnershipDir
// is the necessary boundary of that exclusion, not a complete sufficient
// condition on its own; the full conditions live in doc.go.
func OpenFlatAdmission(ctx context.Context, ownershipDir string) (*FlatAdmission, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolved, err := resolveOwnershipDir(ownershipDir)
	if err != nil {
		return nil, fmt.Errorf("resolve flat ownership realm: %w", err)
	}
	realm, err := newOwnershipRealm(resolved)
	if err != nil {
		return nil, fmt.Errorf("open flat ownership realm: %w", err)
	}
	return &FlatAdmission{realm: realm}, nil
}

// AdmitFileWrites takes entry-level admission for overwriting the files at
// paths as ONE lease — shared on ancestors, exclusive per entry — acquired
// without waiting and rolled back as a unit. Missing payload parent directories
// are created (mkdir only) before entry admission; no payload content is
// written. A live writer of any listed file refuses with ErrOwnershipBusy.
func (a *FlatAdmission) AdmitFileWrites(ctx context.Context, paths []string) (func(), error) {
	lease, err := a.realm.acquireWriter(ctx, paths, 0)
	if err != nil {
		return nil, err
	}
	return lease.Release, nil
}
