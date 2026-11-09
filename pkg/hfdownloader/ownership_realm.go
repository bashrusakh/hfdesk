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
	"sync/atomic"
	"time"
)

var (
	errOwnershipBusy        = errors.New("ownership scope is held")
	errOwnershipUnavailable = errors.New("OS ownership locking is unavailable")
)

const (
	// ownershipRetryDelay is the first retry pause after a refused
	// acquisition. Attempts back off exponentially (capped) with per-attempt
	// jitter so contended waiters decorrelate instead of re-contending in
	// fixed-quantum waves.
	ownershipRetryDelay    = 2 * time.Millisecond
	ownershipRetryDelayMax = 200 * time.Millisecond
)

// ownershipRetryNonce decorrelates the jitter of concurrent waiters.
var ownershipRetryNonce uint64

func ownershipRetryPause(attempt int) time.Duration {
	window := ownershipRetryDelay
	for i := 0; i < attempt && window < ownershipRetryDelayMax; i++ {
		window *= 2
	}
	if window > ownershipRetryDelayMax {
		window = ownershipRetryDelayMax
	}
	half := window / 2
	if half <= 0 {
		return window
	}
	jitter := time.Duration(atomic.AddUint64(&ownershipRetryNonce, 1) % uint64(half+1))
	return half + jitter
}

// ownershipConflictError identifies the exact lock a refused acquisition
// collided on so callers can classify the holder. It preserves the
// errOwnershipBusy identity for every existing busy check.
type ownershipConflictError struct {
	key  string
	path string
	err  error
}

func (e *ownershipConflictError) Error() string {
	return fmt.Sprintf("ownership lock %q is busy: %v", e.path, e.err)
}

func (e *ownershipConflictError) Unwrap() error { return e.err }

// ownershipHeldLocks records the lock paths THIS process holds exclusively so
// a refusal can be classified as same-process contention. It is process-local
// diagnostic state that never participates in exclusion: an entry is added
// before the OS lock attempt and dropped only after the OS lock is released,
// so it may over-claim holds (softening a classification) but can never miss
// one and falsely claim a foreign holder.
var ownershipHeldLocks = struct {
	sync.Mutex
	exclusive map[string]int
}{exclusive: map[string]int{}}

// ownershipHeldExclusivelyHere reports whether this process is a proven
// exclusive holder of path.
func ownershipHeldExclusivelyHere(path string) bool {
	clean := filepath.Clean(path)
	ownershipHeldLocks.Lock()
	defer ownershipHeldLocks.Unlock()
	return ownershipHeldLocks.exclusive[clean] > 0
}

type ownershipRegisteredLock struct {
	handle ownershipLockHandle
	path   string
	mode   ownershipLockMode
}

func (l *ownershipRegisteredLock) release() {
	l.handle.release()
	if l.mode != ownershipExclusive {
		return
	}
	unregisterExclusiveOwnershipLock(filepath.Clean(l.path))
}

func openRegisteredOwnershipLock(path string, mode ownershipLockMode) (ownershipLockHandle, error) {
	registered := mode == ownershipExclusive
	clean := filepath.Clean(path)
	if registered {
		ownershipHeldLocks.Lock()
		ownershipHeldLocks.exclusive[clean]++
		ownershipHeldLocks.Unlock()
	}
	handle, err := openOwnershipLock(path, mode)
	if err != nil {
		if registered {
			unregisterExclusiveOwnershipLock(clean)
		}
		return nil, err
	}
	return &ownershipRegisteredLock{handle: handle, path: path, mode: mode}, nil
}

func unregisterExclusiveOwnershipLock(clean string) {
	ownershipHeldLocks.Lock()
	if ownershipHeldLocks.exclusive[clean] <= 1 {
		delete(ownershipHeldLocks.exclusive, clean)
	} else {
		ownershipHeldLocks.exclusive[clean]--
	}
	ownershipHeldLocks.Unlock()
}

var ownershipLockOpen = openRegisteredOwnershipLock

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
	// onRelease runs once, after this lease's handles are closed:
	// whole-tree admission attaches the file-descriptor-limit hold here so
	// the raised soft limit covers every held lock handle through the
	// caller's removal and is restored only afterwards.
	onRelease func()
}

// ownershipStatIdentity, when non-nil, replaces the platform directory-identity
// primitive so a test can model two participants that resolve one shared
// directory but see divergent host-local stat identity (as two hosts mounting
// shared storage do). Production leaves it nil and behavior is unchanged.
var ownershipStatIdentity func(path string) (string, error)

// statDirectoryIdentity is the entry point flat-realm code uses to read
// host-local directory identity, routed through ownershipStatIdentity for
// tests. Flat keys that must converge across hosts derive from the persisted
// realm ID and realm-relative path tokens, not from this value.
func statDirectoryIdentity(path string) (string, error) {
	if ownershipStatIdentity != nil {
		return ownershipStatIdentity(path)
	}
	return ownershipDirectoryIdentity(path)
}

// ownershipPathSpelling, when non-nil, replaces the path spelling that flat
// realm-anchored key derivation reads (the two sides of the realm-relative
// comparison), so a test can model hosts that mount one shared realm+item
// topology under different prefix spellings. It must map a real path to a
// same-volume prefix spelling of it; filesystem operations keep using the real
// path. Production leaves it nil and behavior is unchanged.
var ownershipPathSpelling func(path string) string

// ownershipSpelledPath returns the spelling flat realm-anchored key derivation
// reads for path: the test seam when set, the real path otherwise.
func ownershipSpelledPath(path string) string {
	if ownershipPathSpelling != nil {
		return ownershipPathSpelling(path)
	}
	return path
}

// newOwnershipRealm resolves the flat external realm with the shared identity
// persisted inside it (see initializeFlatRealmID).
func newOwnershipRealm(root string) (*ownershipRealm, error) {
	realm, err := newOwnershipRealmBase(root)
	if err != nil {
		return nil, err
	}
	if err := realm.initializeFlatRealmID(); err != nil {
		return nil, err
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

	rootObjectID, _ := statDirectoryIdentity(resolved)
	lockDir := filepath.Join(resolved, "v1")
	if err := os.MkdirAll(lockDir, 0o777); err != nil {
		return nil, fmt.Errorf("create ownership lock directory: %w", err)
	}
	lockDirObjectID, _ := statDirectoryIdentity(lockDir)
	return &ownershipRealm{root: resolved, lockDir: lockDir, rootObjectID: rootObjectID, lockDirObjectID: lockDirObjectID}, nil
}

const (
	// Flat-realm identity file and bootstrap gate, siblings of the v1 lock dir
	// so the hashed item-lock namespace under v1 stays untouched.
	ownershipRealmIDFile        = "realm-id"
	ownershipRealmBootstrapFile = "realm-bootstrap.lock"
)

// ownershipRealmIDBootstrapWait bounds the exclusive first-creation gate before
// the operation fails closed.
const ownershipRealmIDBootstrapWait = 30 * time.Second

// initializeFlatRealmID reads the shared realm ID, creating it on first use
// under an exclusive bootstrap gate, so racing first users converge on one ID
// while that gate's advisory lock is effective across clients — the same
// condition that already bounds the cross-host locks themselves; where
// advisory locks are not effective across clients, racing first users can each
// mint their own ID on first use. A published ID is read without the gate (an
// atomic rename makes it complete or absent, never partial), and a crash
// creation temp is discarded, never migrated. Flat keys that must converge
// across hosts derive from this ID, not from host-local stat identity.
func (r *ownershipRealm) initializeFlatRealmID() error {
	idPath := filepath.Join(r.root, ownershipRealmIDFile)
	if id, err := readOwnershipNodeID(idPath); err == nil {
		r.id = id
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read flat ownership realm ID: %w", err)
	}
	bootstrap, err := acquireOwnershipFileLock(context.Background(),
		filepath.Join(r.root, ownershipRealmBootstrapFile),
		ownershipExclusive, ownershipRealmIDBootstrapWait)
	if err != nil {
		return fmt.Errorf("acquire flat ownership realm ID gate: %w", err)
	}
	defer bootstrap.Release()
	id, err := loadOrCreateOwnershipNodeID(r.root, ownershipRealmIDFile)
	if err != nil {
		return fmt.Errorf("initialize flat ownership realm ID: %w", err)
	}
	r.id = id
	return nil
}

// loadOrCreateOwnershipNodeID is called only while the caller holds the
// relevant exclusive bootstrap gate for the directory being initialized: the
// cache node's metadata bootstrap lock (metadata/bootstrap.lock for the root,
// or the parent's metadata/child-bootstrap.lock for a child), or the flat
// realm's own realm-bootstrap.lock (see initializeFlatRealmID). A full ID is
// published by same-directory rename from a synced temp; a crash leaves no
// partial final file.
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
			if errors.Is(err, errOwnershipBusy) {
				return nil, &ownershipConflictError{key: request.key, path: lockPath, err: err}
			}
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
		onRelease := l.onRelease
		l.onRelease = nil
		l.mu.Unlock()
		for i := len(order) - 1; i >= 0; i-- {
			handles[order[i]].release()
		}
		if onRelease != nil {
			onRelease()
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
	for try := 0; ; try++ {
		lease, err := attempt()
		if err == nil {
			return lease, nil
		}
		if !errors.Is(err, errOwnershipBusy) {
			return nil, err
		}
		if wait == 0 {
			return nil, err
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
		pause := ownershipRetryPause(try)
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
	requests, _, err := r.probeWriterRequests(paths)
	if err != nil {
		return nil, err
	}
	return r.tryAcquire(ctx, requests)
}

// probeWriterRequests builds the established non-owning shared probe set for
// paths and reports the item's own scope keys so a refusal can be classified.
func (r *ownershipRealm) probeWriterRequests(paths []string) ([]ownershipLockRequest, map[string]bool, error) {
	requests := make([]ownershipLockRequest, 0, len(paths)*4)
	ownScope := make(map[string]bool, len(paths)*2)
	for _, path := range paths {
		resolved, err := r.resolveOwnershipPath(path, false)
		if err != nil {
			return nil, nil, err
		}
		for _, dir := range resolved.directories {
			requests = append(requests, ownershipDirectoryRequests(dir, ownershipShared)...)
		}
		for _, key := range resolved.itemKeys {
			ownScope[key] = true
			requests = append(requests, ownershipLockRequest{key: key, mode: ownershipShared})
		}
		for _, request := range resolved.itemRequests {
			request.mode = ownershipShared
			requests = append(requests, request)
		}
	}
	return requests, ownScope, nil
}

// probeWriterClassified performs the shared probe and classifies a refusal:
// nil/nil means granted; a classified error names the holder class (the
// foreign class, errStagingOwnershipSameProcess for a shared key this
// process holds, or errStagingOwnershipUncertain when unidentifiable); any
// other probe failure is returned as the error.
func (r *ownershipRealm) probeWriterClassified(ctx context.Context, paths []string) (error, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requests, ownScope, err := r.probeWriterRequests(paths)
	if err != nil {
		if !errors.Is(err, errOwnershipBusy) {
			return nil, err
		}
		var conflict *ownershipConflictError
		if !errors.As(err, &conflict) || conflict == nil {
			return errStagingOwnershipUncertain, nil
		}
		return classifyOwnershipConflict(conflict, ownScope), nil
	}
	lease, err := r.tryAcquire(ctx, requests)
	if err == nil {
		lease.Release()
		return nil, nil
	}
	if !errors.Is(err, errOwnershipBusy) {
		return nil, err
	}
	var conflict *ownershipConflictError
	if !errors.As(err, &conflict) || conflict == nil {
		return errStagingOwnershipUncertain, nil
	}
	return classifyOwnershipConflict(conflict, ownScope), nil
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
