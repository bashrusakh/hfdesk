// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	errOwnershipBusy        = errors.New("ownership scope is held")
	errOwnershipUnavailable = errors.New("OS ownership locking is unavailable")
)

const ownershipRetryDelay = 20 * time.Millisecond

var ownershipLockOpen = openOwnershipLock

var ownershipWriterProbe = func(ctx context.Context, realm *ownershipRealm, paths []string) (*ownershipLease, error) {
	return realm.probeWriterLease(ctx, paths)
}

type ownershipLockMode uint8

const (
	ownershipShared ownershipLockMode = iota + 1
	ownershipExclusive
)

type ownershipLockRequest struct {
	key  string
	mode ownershipLockMode
	// path is set for persistent node-metadata locks; legacy/flat requests use
	// the hashed lock filename derived by ownershipRealm.lockPath.
	path string
}

type ownershipLockHandle interface {
	release()
}

type ownershipRealm struct {
	root            string
	lockDir         string
	id              string
	rootObjectID    string
	lockDirObjectID string
	dataRoot        string
	nodeRoot        string
	cacheOwned      bool
	rootProfile     *ownershipCaseProfile
}

type ownershipLease struct {
	handles  map[string]ownershipLockHandle
	order    []string
	modes    map[string]ownershipLockMode
	once     sync.Once
	mu       sync.Mutex
	released bool
	validate func() error
}

// newOwnershipRealm retains the established local/external realm identity used
// by flat-mode callers.
func newOwnershipRealm(root string) (*ownershipRealm, error) {
	realm, err := newOwnershipRealmBase(root)
	if err != nil {
		return nil, err
	}
	realm.id = "path:" + filepath.Clean(realm.root)
	if realm.rootObjectID != "" {
		realm.id = "object:" + realm.rootObjectID
	}
	return realm, nil
}

func newOwnershipRealmBase(root string) (*ownershipRealm, error) {
	if root == "" {
		return nil, fmt.Errorf("ownership realm path is empty")
	}
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, fmt.Errorf("resolve ownership realm: %w", err)
	}
	if err := os.MkdirAll(abs, 0o777); err != nil {
		return nil, fmt.Errorf("create ownership realm: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve ownership realm symlinks: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("stat ownership realm: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("ownership realm %q is not a directory", resolved)
	}

	rootObjectID, _ := ownershipDirectoryIdentity(resolved)
	lockDir := filepath.Join(resolved, "v1")
	if err := os.MkdirAll(lockDir, 0o777); err != nil {
		return nil, fmt.Errorf("create ownership lock directory: %w", err)
	}
	lockDirObjectID, _ := ownershipDirectoryIdentity(lockDir)
	return &ownershipRealm{root: resolved, lockDir: lockDir, rootObjectID: rootObjectID, lockDirObjectID: lockDirObjectID}, nil
}

// loadOrCreateOwnershipNodeID is called only while metadataDir/bootstrap.lock
// is exclusively held. A full ID is published by same-directory rename from a
// synced temp; a crash leaves no partial final file.
func loadOrCreateOwnershipNodeID(metadataDir, name string) (string, error) {
	final := filepath.Join(metadataDir, name)
	if raw, err := os.ReadFile(final); err == nil {
		id := strings.TrimSpace(string(raw))
		if len(id) != 32 {
			return "", fmt.Errorf("ownership ID %s is malformed", final)
		}
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	entries, err := os.ReadDir(metadataDir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), name+".tmp-") {
			if err := os.Remove(filepath.Join(metadataDir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
		}
	}
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	id := hex.EncodeToString(value)
	tmp, err := os.CreateTemp(metadataDir, name+".tmp-")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(id); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, final); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return id, nil
}

// DefaultOwnershipDir is the per-user lock realm for legacy flat OutputDir
// downloads. HF-cache coordination is rooted under its frozen HubDir/CacheDir.
func DefaultOwnershipDir() string {
	if base, err := os.UserConfigDir(); err == nil && base != "" {
		return filepath.Join(base, "HFDesk", "ownership")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "hfdesk", "ownership")
	}
	return ""
}

func resolveOwnershipDir(path string) (string, error) {
	if path == "" {
		path = DefaultOwnershipDir()
	}
	if path == "" {
		return "", fmt.Errorf("cannot resolve the default ownership realm directory")
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve ownership realm path: %w", err)
	}
	return abs, nil
}

func (r *ownershipRealm) resourceKey(kind, identity string) string {
	if r.cacheOwned {
		sum := sha256.Sum256([]byte("hfdesk-cache-resource-v2\x00" + r.id + "\x00" + kind + "\x00" + identity))
		return "v2:" + hex.EncodeToString(sum[:])
	}
	return kind + "\x00" + r.id + "\x00" + identity
}

func (r *ownershipRealm) lockPath(key string) string {
	sum := sha256.Sum256([]byte("hfdesk-ownership-v1\x00" + r.id + "\x00" + key))
	return filepath.Join(r.lockDir, hex.EncodeToString(sum[:])+".lock")
}

func normalizeOwnershipRequests(requests []ownershipLockRequest) ([]ownershipLockRequest, error) {
	strongest := make(map[string]ownershipLockMode, len(requests))
	paths := make(map[string]string, len(requests))
	for _, request := range requests {
		if request.key == "" || (request.mode != ownershipShared && request.mode != ownershipExclusive) {
			return nil, fmt.Errorf("invalid ownership lock request")
		}
		if request.mode > strongest[request.key] {
			strongest[request.key] = request.mode
		}
		if paths[request.key] == "" && request.path != "" {
			paths[request.key] = request.path
		}
	}
	normalized := make([]ownershipLockRequest, 0, len(strongest))
	for key, mode := range strongest {
		normalized = append(normalized, ownershipLockRequest{key: key, mode: mode, path: paths[key]})
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].key < normalized[j].key })
	return normalized, nil
}

// tryAcquire rolls back the whole sorted key set on the first conflict; callers
// never sleep while holding only part of a multi-resource lease.
func (r *ownershipRealm) tryAcquire(ctx context.Context, requests []ownershipLockRequest) (*ownershipLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	normalized, err := normalizeOwnershipRequests(requests)
	if err != nil {
		return nil, err
	}
	lease := &ownershipLease{
		handles: make(map[string]ownershipLockHandle, len(normalized)),
		modes:   make(map[string]ownershipLockMode, len(normalized)),
	}
	for _, request := range normalized {
		if err := ctx.Err(); err != nil {
			lease.Release()
			return nil, err
		}
		lockPath := request.path
		if lockPath == "" {
			lockPath = r.lockPath(request.key)
		}
		handle, err := ownershipLockOpen(lockPath, request.mode)
		if err != nil {
			lease.Release()
			return nil, err
		}
		lease.handles[request.key] = handle
		lease.order = append(lease.order, request.key)
		lease.modes[request.key] = request.mode
	}
	if err := ctx.Err(); err != nil {
		lease.Release()
		return nil, err
	}
	return lease, nil
}

func (l *ownershipLease) tryAdd(ctx context.Context, realm *ownershipRealm, requests []ownershipLockRequest) error {
	normalized, err := normalizeOwnershipRequests(requests)
	if err != nil {
		return err
	}
	missing := make([]ownershipLockRequest, 0, len(normalized))
	for _, request := range normalized {
		if held, ok := l.modes[request.key]; ok {
			if held < request.mode {
				return errOwnershipBusy
			}
			continue
		}
		missing = append(missing, request)
	}
	added, err := realm.tryAcquire(ctx, missing)
	if err != nil {
		return err
	}
	for _, key := range added.order {
		l.handles[key] = added.handles[key]
		l.order = append(l.order, key)
	}
	for key, mode := range added.modes {
		l.modes[key] = mode
	}
	added.handles = nil
	added.order = nil
	added.modes = nil
	return nil
}

func (l *ownershipLease) transferTo(dst *ownershipLease, keys map[string]bool) {
	if dst.handles == nil {
		dst.handles = make(map[string]ownershipLockHandle)
	}
	if dst.modes == nil {
		dst.modes = make(map[string]ownershipLockMode)
	}
	remaining := make([]string, 0, len(l.order))
	for _, key := range l.order {
		if !keys[key] {
			remaining = append(remaining, key)
			continue
		}
		dst.handles[key] = l.handles[key]
		dst.modes[key] = l.modes[key]
		dst.order = append(dst.order, key)
		delete(l.handles, key)
		delete(l.modes, key)
	}
	l.order = remaining
}

func (l *ownershipLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.mu.Lock()
		l.released = true
		handles := l.handles
		order := l.order
		l.handles = nil
		l.order = nil
		l.modes = nil
		l.validate = nil
		l.mu.Unlock()
		for i := len(order) - 1; i >= 0; i-- {
			handles[order[i]].release()
		}
	})
}

func (l *ownershipLease) Validate() error {
	if l == nil {
		return fmt.Errorf("ownership lease is nil")
	}
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return fmt.Errorf("ownership lease is released")
	}
	validate := l.validate
	if validate == nil {
		l.mu.Unlock()
		return nil
	}
	err := validate()
	l.mu.Unlock()
	return err
}

func (r *ownershipRealm) acquire(ctx context.Context, requests []ownershipLockRequest, wait time.Duration) (*ownershipLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return retryOwnership(ctx, wait, func() (*ownershipLease, error) {
		return r.tryAcquire(ctx, requests)
	})
}

func (r *ownershipRealm) acquireWriterWithWait(ctx context.Context, paths []string, wait time.Duration, onWait func()) (*ownershipLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return retryOwnershipNotify(ctx, wait, func() (*ownershipLease, error) {
		preparation, err := r.prepareWriter(ctx, paths, 0)
		if err != nil {
			return nil, err
		}
		if err := preparation.CreateParents(ctx); err != nil {
			return nil, err
		}
		return preparation.Promote(ctx)
	}, onWait)
}

func retryOwnership(ctx context.Context, wait time.Duration, attempt func() (*ownershipLease, error)) (*ownershipLease, error) {
	return retryOwnershipNotify(ctx, wait, attempt, nil)
}

func ownershipWaitContext(ctx context.Context, wait time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if wait <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, wait)
}

func acquireOwnershipFileLock(ctx context.Context, path string, mode ownershipLockMode, wait time.Duration) (*ownershipLease, error) {
	return retryOwnership(ctx, wait, func() (*ownershipLease, error) {
		handle, err := ownershipLockOpen(path, mode)
		if err != nil {
			return nil, err
		}
		return &ownershipLease{
			handles: map[string]ownershipLockHandle{"bootstrap": handle},
			order:   []string{"bootstrap"},
			modes:   map[string]ownershipLockMode{"bootstrap": mode},
		}, nil
	})
}

func retryOwnershipNotify(ctx context.Context, wait time.Duration, attempt func() (*ownershipLease, error), onWait func()) (*ownershipLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if wait < 0 {
		wait = 0
	}
	deadline := time.Now().Add(wait)
	for {
		lease, err := attempt()
		if err == nil {
			return lease, nil
		}
		if !errors.Is(err, errOwnershipBusy) {
			return nil, err
		}
		if wait == 0 {
			return nil, errOwnershipBusy
		}
		if onWait != nil {
			onWait()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("ownership wait exceeded %s: %w", wait, errOwnershipBusy)
		}
		pause := ownershipRetryDelay
		if pause > remaining {
			pause = remaining
		}
		if !sleepCtx(ctx, pause) {
			return nil, ctx.Err()
		}
	}
}

func (r *ownershipRealm) probeWriter(ctx context.Context, paths []string) (bool, error) {
	lease, err := r.probeWriterLease(ctx, paths)
	if errors.Is(err, errOwnershipBusy) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	lease.Release()
	return false, nil
}

func (r *ownershipRealm) probeWriterLease(ctx context.Context, paths []string) (*ownershipLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requests := make([]ownershipLockRequest, 0, len(paths)*4)
	for _, path := range paths {
		resolved, err := r.resolveOwnershipPath(path, false)
		if err != nil {
			return nil, err
		}
		for _, dir := range resolved.directories {
			requests = append(requests, ownershipDirectoryRequests(dir, ownershipShared)...)
		}
		for _, key := range resolved.itemKeys {
			requests = append(requests, ownershipLockRequest{key: key, mode: ownershipShared})
		}
		for _, request := range resolved.itemRequests {
			request.mode = ownershipShared
			requests = append(requests, request)
		}
	}
	return r.tryAcquire(ctx, requests)
}

func (r *ownershipRealm) fenceProof(path string) (string, error) {
	resolved, err := r.resolveOwnershipPath(path, false)
	if err != nil {
		return "", err
	}
	return ownershipFenceForResolvedPath(r, path, resolved)
}

func (r *ownershipRealm) fenceProofForHeldPath(path string) (string, error) {
	var resolved ownershipResolvedPath
	var err error
	if r.cacheOwned {
		resolved, err = r.resolveCacheOwnershipPathReadOnly(path, false)
	} else {
		resolved, err = r.resolveOwnershipPath(path, false)
	}
	if err != nil {
		return "", err
	}
	return ownershipFenceForResolvedPath(r, path, resolved)
}

func ownershipFenceForResolvedPath(r *ownershipRealm, path string, resolved ownershipResolvedPath) (string, error) {
	if r.cacheOwned {
		if len(resolved.itemKeys) == 0 {
			return "", fmt.Errorf("cache ownership path %q has no resolved node ID", path)
		}
		identity := "hfdesk-cache-v4-fence-v2\x00" + resolved.itemKeys[0]
		sum := sha256.Sum256([]byte(identity))
		return "v2:" + hex.EncodeToString(sum[:]), nil
	}
	identity := "hfdesk-current-fence-v1\x00" + r.id
	for _, key := range resolved.itemKeys {
		identity += "\x00" + key
	}
	sum := sha256.Sum256([]byte(identity))
	return "v1:" + hex.EncodeToString(sum[:]), nil
}

func (l *ownershipLease) holdsCoverage(r *ownershipRealm, path string) bool {
	if l == nil || r == nil {
		return false
	}
	var resolved ownershipResolvedPath
	var err error
	if r.cacheOwned {
		resolved, err = r.resolveCacheOwnershipPathReadOnly(path, false)
	} else {
		resolved, err = r.resolveOwnershipPath(path, false)
	}
	if err != nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return false
	}
	for _, key := range resolved.itemKeys {
		if l.modes[key] != ownershipExclusive {
			return false
		}
	}
	return true
}
