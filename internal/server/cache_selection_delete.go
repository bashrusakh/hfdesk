// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

type jobWriteActivity struct {
	job       *Job
	planned   map[string]struct{}
	planKnown bool
}

func jobDestinationBase(job *Job) string {
	if job.Flat || job.LocalDir != "" {
		root := job.LocalDir
		if root == "" {
			root = job.OutputDir
		}
		repo := job.Repo
		if job.LocalRepo != "" {
			repo = job.LocalRepo
		}
		base, err := hfdownloader.DestinationBase(root, filepath.FromSlash(repo))
		if err != nil {
			return ""
		}
		return base
	}
	repoID := job.Repo
	if job.LocalRepo != "" {
		repoID = job.LocalRepo
	}
	repoType := hfdownloader.RepoTypeModel
	if job.IsDataset {
		repoType = hfdownloader.RepoTypeDataset
	}
	// Resolve through the HF cache owner so validation and the reservation path
	// exactly match the repository/type and friendly view used by the writer.
	// HFCache.Repo is a pure validation/path constructor; it performs no probes.
	repo, err := hfdownloader.NewHFCacheResolved(job.OutputDir, job.HubDir, 0).Repo(repoID, repoType)
	if err != nil {
		return ""
	}
	return repo.FriendlyPath()
}

// mutationEntryIdentity resolves symlinks in parent directories but preserves
// the final basename as an entry, so leaf symlink targets are never followed.
func mutationEntryIdentity(name string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(name))
	if err != nil {
		return "", err
	}
	parent, base := filepath.Dir(abs), filepath.Base(abs)
	if info, lstatErr := os.Lstat(abs); lstatErr == nil {
		isDirectory := info.IsDir()
		if info.Mode()&os.ModeSymlink == 0 && !info.IsDir() && !info.Mode().IsRegular() {
			return "", fmt.Errorf("cannot establish entry identity for %q", abs)
		}
		if isDirectory {
			resolved, resolveErr := filepath.EvalSymlinks(abs)
			if resolveErr != nil {
				return "", resolveErr
			}
			return pathIdentityKey(resolved), nil
		}
	} else if !os.IsNotExist(lstatErr) {
		return "", lstatErr
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		missing := []string{}
		ancestor := parent
		for {
			info, lstatErr := os.Lstat(ancestor)
			if lstatErr == nil {
				if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
					return "", fmt.Errorf("path component %q is not a directory", ancestor)
				}
				if info.Mode()&os.ModeSymlink != 0 {
					target, statErr := os.Stat(ancestor)
					if statErr != nil {
						return "", statErr
					}
					if !target.IsDir() {
						return "", fmt.Errorf("path component %q is not a directory", ancestor)
					}
				}
				resolved, err = filepath.EvalSymlinks(ancestor)
				if err != nil {
					return "", err
				}
				break
			}
			if !os.IsNotExist(lstatErr) {
				return "", lstatErr
			}
			next := filepath.Dir(ancestor)
			if next == ancestor {
				return "", lstatErr
			}
			missing = append(missing, filepath.Base(ancestor))
			ancestor = next
		}
		for i := len(missing) - 1; i >= 0; i-- {
			resolved = filepath.Join(resolved, missing[i])
		}
	}
	return pathIdentityKey(filepath.Join(resolved, base)), nil
}

// mutationDirectoryIdentity treats base as a directory scope, not a final
// entry. Existing directory aliases resolve to their destination; absent
// ordinary suffixes are reconstructed by mutationEntryIdentity.
func mutationDirectoryIdentity(name string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(name))
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			target, statErr := os.Stat(abs)
			if statErr != nil {
				return "", statErr
			}
			if !target.IsDir() {
				return "", fmt.Errorf("writer base %q is not a directory", abs)
			}
		} else if !info.IsDir() {
			return "", fmt.Errorf("writer base %q is not a directory", abs)
		}
		resolved, resolveErr := filepath.EvalSymlinks(abs)
		if resolveErr != nil {
			return "", resolveErr
		}
		return pathIdentityKey(resolved), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	return mutationEntryIdentity(abs)
}

// writableAlias is a directory symlink inside a writable base whose fully
// resolved destination stays inside that same base, so the alias participates
// in a finite set of repository-relative spellings.
type writableAlias struct {
	location string // absolute path of the symlink entry itself
	dest     string // absolute fully resolved destination directory
	name     string // name of the entry inside its parent directory
}

// writableNamespaceInventory records the resolved identity of one writable
// base and every directory alias it contains. ok=false (reported by the
// constructor) marks a mapping that can no longer be proven complete: an
// unreadable entry, a dangling or looping symlink, a resolution failure, or a
// directory alias whose destination leaves the base all forbid a negative
// pre-plan writer decision.
type writableNamespaceInventory struct {
	base    string
	aliases []writableAlias
	// snapshotNamespace is the job-semantic namespace kind: it is set only
	// when this inventory was built from the job's configured snapshot base
	// (jobSnapshotBase) and is never derived from the resolved physical
	// basename. A snapshot base aliased to a directory with another name must
	// still strip the unknown commit prefix below, and a friendly base whose
	// resolved basename happens to be "snapshots" must never do so.
	snapshotNamespace bool
}

const (
	maxWritableAliasDepth    = 64
	maxWritableAliasSpelling = 32
)

var errWritableNamespaceUnknown = errors.New("writable namespace cannot be inventoried")

// inventoryWritableNamespace resolves the writable base and enumerates every
// directory alias below it. A base that does not exist yet is trivially
// alias-free; every other failure is reported as an unknown mapping.
func inventoryWritableNamespace(base string) (writableNamespaceInventory, bool) {
	baseIdentity, err := mutationDirectoryIdentity(base)
	if err != nil {
		return writableNamespaceInventory{}, false
	}
	inv := writableNamespaceInventory{base: baseIdentity}
	if _, err := os.Lstat(baseIdentity); err != nil {
		if os.IsNotExist(err) {
			return inv, true
		}
		return writableNamespaceInventory{}, false
	}
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if depth > maxWritableAliasDepth {
			return errWritableNamespaceUnknown
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.IsDir() {
				if err := walk(path, depth+1); err != nil {
					return err
				}
				continue
			}
			if info.Mode()&os.ModeSymlink == 0 {
				continue
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				// Dangling targets, loops, and permission failures make the
				// mapping unknown; never derive a negative from them.
				return err
			}
			target, err := os.Stat(resolved)
			if err != nil {
				return err
			}
			if !target.IsDir() {
				// Leaf entries (including friendly/snapshot file links into
				// blobs) never reorganize directory spellings.
				continue
			}
			rel, err := filepath.Rel(baseIdentity, resolved)
			if err != nil {
				return err
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				// An alias destination outside the writer base can redirect a
				// repository path to an arbitrary physical target, so the
				// mapping cannot be proven complete from inside this base.
				return errWritableNamespaceUnknown
			}
			inv.aliases = append(inv.aliases, writableAlias{
				location: path,
				dest:     resolved,
				name:     entry.Name(),
			})
		}
		return nil
	}
	if err := walk(baseIdentity, 0); err != nil {
		return writableNamespaceInventory{}, false
	}
	return inv, true
}

// relativeWithin returns the slash-normalized path of p relative to base when
// p is lexically contained in base.
func relativeWithin(base, p string) (string, bool) {
	rel, err := filepath.Rel(base, p)
	if err != nil || filepath.IsAbs(rel) {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return filepath.ToSlash(rel), true
}

// joinSlashPath joins non-empty slash-separated components.
func joinSlashPath(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, "/")
}

// selectedPathCandidates returns every repository-relative spelling through
// which the target can be written from this writable namespace. The set starts
// from the canonical containment (when the target's parent lies inside the
// base) and is closed under the inventoried directory aliases, including
// nested alias destinations and alias chains. ok=false means the spelling set
// cannot be enumerated finitely, which also forbids a negative answer.
func (inv writableNamespaceInventory) selectedPathCandidates(targetIdentity string) ([]string, bool) {
	parent := filepath.Dir(targetIdentity)
	spellings := map[string]map[string]struct{}{}
	add := func(dir, spelling string) (bool, error) {
		set, ok := spellings[dir]
		if !ok {
			set = map[string]struct{}{}
			spellings[dir] = set
		}
		if _, exists := set[spelling]; exists {
			return false, nil
		}
		if len(set) >= maxWritableAliasSpelling {
			return false, errWritableNamespaceUnknown
		}
		set[spelling] = struct{}{}
		return true, nil
	}
	seed := func(dir string) error {
		spelling, ok := relativeWithin(inv.base, dir)
		if !ok {
			return nil
		}
		_, err := add(dir, spelling)
		return err
	}
	// Canonical spellings: the target's parent when it lies inside the base
	// (missing ordinary suffixes stay lexical), and each alias location.
	if err := seed(parent); err != nil {
		return nil, false
	}
	for _, alias := range inv.aliases {
		if err := seed(filepath.Dir(alias.location)); err != nil {
			return nil, false
		}
	}
	// Close the spelling sets under the aliases: any spelling of an alias
	// location can route through the alias into every reached directory, which
	// then unlocks further aliases (nested destinations and chains included).
	for {
		pending := make([][2]string, 0, len(inv.aliases))
		for _, alias := range inv.aliases {
			prefixDir := filepath.Dir(alias.location)
			for prefix := range spellings[prefixDir] {
				for reached := range spellings {
					relDest, ok := relativeWithin(alias.dest, reached)
					if !ok {
						continue
					}
					pending = append(pending, [2]string{reached, joinSlashPath(prefix, alias.name, relDest)})
				}
			}
		}
		changed := false
		for _, candidate := range pending {
			did, err := add(candidate[0], candidate[1])
			if err != nil {
				return nil, false
			}
			if did {
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	name := filepath.ToSlash(filepath.Base(targetIdentity))
	candidates := make([]string, 0, len(spellings[parent])*2)
	for spelling := range spellings[parent] {
		candidate := joinSlashPath(spelling, name)
		candidates = append(candidates, candidate)
		// A semantic snapshot namespace is physically rooted at
		// snapshots/<commit>, while the plan's RelativePath starts below that
		// commit directory. The commit is not known until planning completes,
		// and it may be multi-component (e.g. the revision-name fallback
		// "feature/v2" when the revision-info lookup fails), so compare every
		// suffix after the first k = 1..n-1 leading components of the full
		// spelling, not just the suffix after the first component. Any suffix
		// that does not correspond to the real commit prefix only adds an
		// over-conservative possible-writer answer, which is safe; deriving a
		// negative from a one-component strip would not be. The snapshot kind
		// comes from the job's configured snapshot base, never from a resolved
		// physical basename: whenever the semantic base and its
		// resolved/aliased identity diverge in a way that leaves the spelling
		// closure incomplete (unreadable entries, dangling/looping symlinks,
		// outward alias destinations, or spelling overflow), the inventory or
		// the enumeration above already failed closed with the conservative
		// possible-writer answer, so no negative is ever derived from a
		// basename-resolved namespace type.
		if inv.snapshotNamespace {
			rest := candidate
			for {
				idx := strings.Index(rest, "/")
				if idx < 0 {
					break
				}
				rest = rest[idx+1:]
				candidates = append(candidates, rest)
			}
		}
	}
	return candidates, true
}

// writableNamespaceBase pairs one writable base of the job with its
// job-semantic namespace kind. The kind is decided by which configured job
// base produced the path, never by inspecting a resolved physical basename.
type writableNamespaceBase struct {
	path     string
	snapshot bool
}

func jobMayWriteSelectedPath(job *Job, target string) bool {
	base := jobDestinationBase(job)
	if base == "" {
		return false
	}
	targetIdentity, targetErr := mutationEntryIdentity(target)
	if targetErr != nil {
		return true
	}
	bases := []writableNamespaceBase{{path: base}}
	if snapshotBase := jobSnapshotBase(job); snapshotBase != "" && snapshotBase != base {
		bases = append(bases, writableNamespaceBase{path: snapshotBase, snapshot: true})
	}
	// A negative pre-plan answer requires a complete physical-to-repo mapping
	// proof for every writable namespace of the job. Any unknown or outward
	// mapping short-circuits to the conservative possible-writer answer.
	inventories := make([]writableNamespaceInventory, 0, len(bases))
	aliasesFound := false
	for _, writable := range bases {
		inv, ok := inventoryWritableNamespace(writable.path)
		if !ok {
			return true
		}
		inv.snapshotNamespace = writable.snapshot
		if len(inv.aliases) > 0 {
			aliasesFound = true
		}
		inventories = append(inventories, inv)
	}
	for _, inv := range inventories {
		if jobMayWriteSelectedPathWithinBase(inv, targetIdentity, job) {
			return true
		}
	}
	// Exact path-shaped filters name one exact repository path. While the
	// writable namespaces contain directory aliases, a canonical containment
	// miss does not prove that this exact spelling resolves elsewhere, so the
	// reservation stays conservative instead of deriving a negative from the
	// filter shape alone.
	if aliasesFound && hasPathFilter(job.Filters) {
		return true
	}
	return false
}

// jobMayWriteSelectedPathWithinBase reports whether any proven repository
// spelling of the target inside one writable namespace survives the job's
// filters and excludes.
func jobMayWriteSelectedPathWithinBase(inv writableNamespaceInventory, targetIdentity string, job *Job) bool {
	candidates, ok := inv.selectedPathCandidates(targetIdentity)
	if !ok {
		return true
	}
	for _, candidate := range candidates {
		if hfdownloader.GGUFPathSelected(candidate, job.Filters, job.Excludes, job.ExactMatch) {
			return true
		}
	}
	return false
}

// hasPathFilter reports whether a filter names a path rather than a bare
// name. It is only used to keep reservations conservative; a slashless filter
// is never treated as proof of a negative, because bare filters also select
// directory components.
func hasPathFilter(filters []string) bool {
	for _, filter := range filters {
		if strings.ContainsAny(filepath.ToSlash(strings.TrimSpace(filter)), "/\\") {
			return true
		}
	}
	return false
}

func jobSnapshotBase(job *Job) string {
	repoPath := hfJobRepoPath(job)
	if repoPath == "" {
		return ""
	}
	return filepath.Join(repoPath, "snapshots")
}

func sameMutationPath(a, b string) bool {
	a, errA := mutationEntryIdentity(a)
	b, errB := mutationEntryIdentity(b)
	return errA == nil && errB == nil && a == b
}

func mutationPathsOverlap(a, b string) bool {
	if sameMutationPath(a, b) {
		return true
	}
	a, errA := mutationEntryIdentity(a)
	b, errB := mutationEntryIdentity(b)
	if errA != nil || errB != nil {
		return true
	}
	return withinLocalRoot(a, b) || withinLocalRoot(b, a)
}

func mutationDirectoryEntryOverlap(directory, entry string) bool {
	dirPath, dirErr := mutationDirectoryIdentity(directory)
	entryPath, entryErr := mutationEntryIdentity(entry)
	if dirErr != nil || entryErr != nil {
		return true
	}
	return withinLocalRoot(dirPath, entryPath) || withinLocalRoot(entryPath, dirPath)
}

func (m *JobManager) reserveSelectedGGUF(paths []string) (func(), bool) {
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, err := filepath.Abs(filepath.Clean(p))
		if err != nil {
			return nil, false
		}
		clean = append(clean, abs)
	}
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return nil, false
	}
	entryConflicts := func(scopes []string) bool {
		for _, a := range scopes {
			for _, b := range clean {
				if mutationPathsOverlap(a, b) {
					return true
				}
			}
		}
		return false
	}
	for _, scopes := range m.deleteReservations {
		if entryConflicts(scopes) {
			m.mu.Unlock()
			return nil, false
		}
	}
	for _, scopes := range m.hfRepoReservations {
		for _, scope := range scopes {
			for _, entry := range clean {
				if mutationDirectoryEntryOverlap(scope, entry) {
					m.mu.Unlock()
					return nil, false
				}
			}
		}
	}
	for _, scopes := range m.mutationScopes {
		for _, scope := range scopes {
			for _, entry := range clean {
				if mutationDirectoryEntryOverlap(scope, entry) {
					m.mu.Unlock()
					return nil, false
				}
			}
		}
	}
	for _, activity := range m.runActivities {
		if activity.planKnown {
			if entryConflicts(mapKeys(activity.planned)) {
				m.mu.Unlock()
				return nil, false
			}
			continue
		}
		for _, target := range clean {
			if jobMayWriteSelectedPath(activity.job, target) {
				m.mu.Unlock()
				return nil, false
			}
		}
	}
	for _, job := range m.jobs {
		if job.Status != JobStatusQueued && !job.starting {
			continue
		}
		for _, target := range clean {
			if jobMayWriteSelectedPath(job, target) {
				m.mu.Unlock()
				return nil, false
			}
		}
	}
	if m.deleteReservations == nil {
		m.deleteReservations = make(map[uint64][]string)
	}
	m.opWG.Add(1)
	m.nextWriteID++
	id := m.nextWriteID
	m.deleteReservations[id] = clean
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.deleteReservations, id)
		m.dispatchLocked()
		m.mu.Unlock()
		m.opWG.Done()
	}, true
}

func hfJobRepoPath(job *Job) string {
	repo, err := hfJobRepoDir(job)
	if err != nil {
		return ""
	}
	return repo.Path()
}

func hfJobRepoDir(job *Job) (*hfdownloader.RepoDir, error) {
	if job == nil || job.Flat || job.LocalDir != "" {
		return nil, fmt.Errorf("job does not use HF cache storage")
	}
	hub := job.HubDir
	if hub == "" {
		hub = filepath.Join(job.OutputDir, "hub")
	}
	repoID := job.Repo
	if job.LocalRepo != "" {
		repoID = job.LocalRepo
	}
	repoType := hfdownloader.RepoTypeModel
	if job.IsDataset {
		repoType = hfdownloader.RepoTypeDataset
	}
	cache := hfdownloader.NewHFCacheResolved(job.OutputDir, hub, 0)
	repo, err := cache.Repo(repoID, repoType)
	if err != nil {
		return nil, err
	}
	return repo, nil
}

func jobPlannedWriteEntries(job *Job, plan hfdownloader.Plan) ([]string, bool) {
	base := jobDestinationBase(job)
	if base == "" {
		return nil, false
	}
	if len(plan.Items) == 0 {
		return nil, true
	}
	var repo *hfdownloader.RepoDir
	if !job.Flat && job.LocalDir == "" {
		var err error
		repo, err = hfJobRepoDir(job)
		if err != nil {
			return nil, false
		}
	}
	entries := make([]string, 0, len(plan.Items)*2)
	for _, item := range plan.Items {
		friendly, err := hfdownloader.SafeJoin(base, item.RelativePath)
		if err != nil {
			return nil, false
		}
		entries = append(entries, friendly)
		if repo != nil {
			snapshot, err := repo.SnapshotPath(plan.Commit, item.RelativePath)
			if err != nil {
				return nil, false
			}
			entries = append(entries, snapshot)
		}
	}
	return entries, true
}

func hfReservationConflictsJob(scopes []string, job *Job) bool {
	base := jobDestinationBase(job)
	if base == "" {
		return false
	}
	if repo := hfJobRepoPath(job); repo != "" && mutationDirectoryEntryOverlap(scopes[0], repo) {
		return true
	}
	for _, scope := range scopes {
		if mutationDirectoryEntryOverlap(scope, base) {
			return true
		}
	}
	return false
}

// reserveSelectedHF protects the complete selected HF repo-reference namespace
// and the associated friendly view for the full scan/unlink interval.
func (m *JobManager) reserveSelectedHF(repoDir, friendlyDir string) (func(), bool) {
	scopes := []string{repoDir, friendlyDir}
	for i, scope := range scopes {
		abs, err := filepath.Abs(filepath.Clean(scope))
		if err != nil {
			return nil, false
		}
		scopes[i] = abs
	}
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return nil, false
	}
	conflictScopes := func(other []string) bool {
		for _, a := range scopes {
			for _, b := range other {
				if mutationDirectoryEntryOverlap(a, b) {
					return true
				}
			}
		}
		return false
	}
	for _, other := range m.hfRepoReservations {
		if conflictScopes(other) {
			m.mu.Unlock()
			return nil, false
		}
	}
	for _, other := range m.deleteReservations {
		if conflictScopes(other) {
			m.mu.Unlock()
			return nil, false
		}
	}
	for _, writes := range m.mutationScopes {
		if conflictScopes(writes) {
			m.mu.Unlock()
			return nil, false
		}
	}
	for _, activity := range m.runActivities {
		if hfReservationConflictsJob(scopes, activity.job) {
			m.mu.Unlock()
			return nil, false
		}
	}
	for _, job := range m.jobs {
		if (job.Status == JobStatusQueued || job.starting) && hfReservationConflictsJob(scopes, job) {
			m.mu.Unlock()
			return nil, false
		}
	}
	m.opWG.Add(1)
	m.nextWriteID++
	id := m.nextWriteID
	if m.hfRepoReservations == nil {
		m.hfRepoReservations = make(map[uint64][]string)
	}
	m.hfRepoReservations[id] = scopes
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.hfRepoReservations, id)
		m.dispatchLocked()
		m.mu.Unlock()
		m.opWG.Done()
	}, true
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func (m *JobManager) beginCacheMutation(paths ...string) (func(), bool) {
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(filepath.Clean(p))
		if err != nil {
			return nil, false
		}
		clean = append(clean, abs)
	}
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return nil, false
	}
	for _, scopes := range m.deleteReservations {
		for _, scope := range scopes {
			for _, write := range clean {
				if mutationDirectoryEntryOverlap(write, scope) {
					m.mu.Unlock()
					return nil, false
				}
			}
		}
	}
	for _, scopes := range m.hfRepoReservations {
		for _, scope := range scopes {
			for _, write := range clean {
				if mutationDirectoryEntryOverlap(write, scope) {
					m.mu.Unlock()
					return nil, false
				}
			}
		}
	}
	if m.mutationScopes == nil {
		m.mutationScopes = make(map[uint64][]string)
	}
	m.opWG.Add(1)
	m.nextWriteID++
	id := m.nextWriteID
	m.mutationScopes[id] = clean
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.mutationScopes, id)
		m.dispatchLocked()
		m.mu.Unlock()
		m.opWG.Done()
	}, true
}

func (m *JobManager) jobBlockedByDeleteLocked(job *Job) bool {
	for _, scopes := range m.hfRepoReservations {
		if hfReservationConflictsJob(scopes, job) {
			return true
		}
	}
	for _, targets := range m.deleteReservations {
		for _, target := range targets {
			if jobMayWriteSelectedPath(job, target) {
				return true
			}
		}
	}
	return false
}

func (m *JobManager) registerRunActivityLocked(job *Job) uint64 {
	if m.runActivities == nil {
		m.runActivities = make(map[uint64]*jobWriteActivity)
	}
	m.nextWriteID++
	id := m.nextWriteID
	m.runActivities[id] = &jobWriteActivity{job: job, planned: make(map[string]struct{})}
	return id
}

type selectedDeleteRequest struct {
	Repo       string                 `json:"repo"`
	Type       string                 `json:"type"`
	LocationID string                 `json:"locationId"`
	GroupID    string                 `json:"groupId"`
	Members    []cacheSelectionMember `json:"members"`
}

type selectedDeleteResponse struct {
	OK               bool     `json:"ok"`
	Repo             string   `json:"repo"`
	GroupID          string   `json:"groupId"`
	Removed          []string `json:"removed"`
	Remaining        []string `json:"remaining"`
	LinkOnlyEntries  []string `json:"linkOnlyEntries,omitempty"`
	RetainedPayloads []string `json:"retainedPayloads,omitempty"`
	Errors           []string `json:"errors,omitempty"`
	LinkOnly         bool     `json:"linkOnly,omitempty"`
	Message          string   `json:"message"`
}

func selectionMemberKey(m cacheSelectionMember) string {
	versions := append([]string(nil), m.Versions...)
	sort.Strings(versions)
	return m.Path + "\x00" + strings.Join(versions, "\x00") + fmt.Sprintf("\x00%t\x00%d\x00%s", m.LinkOnly, m.Size, m.Message)
}

func selectionCompositionEqual(requested, current []cacheSelectionMember) bool {
	if len(requested) == 0 || len(requested) != len(current) {
		return false
	}
	a, b := make([]string, len(requested)), make([]string, len(current))
	for i := range requested {
		a[i] = selectionMemberKey(requested[i])
	}
	for i := range current {
		b[i] = selectionMemberKey(current[i])
	}
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
		if i > 0 && a[i] == a[i-1] {
			return false
		}
	}
	return true
}

func safeSelectedRelative(name string) bool {
	if name == "" || strings.Contains(name, "\\") || filepath.IsAbs(name) {
		return false
	}
	clean := path.Clean(name)
	return clean == name && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func preflightSelectedLocalEntries(repoDir string, members []cacheSelectionMember) (*os.Root, []string, error) {
	rootPath := filepath.Dir(filepath.Dir(repoDir))
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, nil, err
	}
	relRepo, err := filepath.Rel(rootPath, repoDir)
	if err != nil || relRepo == ".." || strings.HasPrefix(relRepo, ".."+string(filepath.Separator)) || filepath.IsAbs(relRepo) {
		root.Close()
		return nil, nil, fmt.Errorf("selected repository is outside its configured root")
	}
	// Owner and model are part of the admitted root-relative namespace. Refuse
	// traversal through either, even when the target happens to be in-root.
	for _, component := range strings.Split(relRepo, string(filepath.Separator)) {
		info, err := root.Lstat(component)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			root.Close()
			return nil, nil, fmt.Errorf("selected repository has an unsafe owner/model component")
		}
		next, err := root.OpenRoot(component)
		if err != nil {
			root.Close()
			return nil, nil, err
		}
		root.Close()
		root = next
	}
	rels := make([]string, 0, len(members))
	for _, member := range members {
		if !safeSelectedRelative(member.Path) || !strings.EqualFold(filepath.Ext(member.Path), ".gguf") || isCacheMMProjFile(member.Path) {
			root.Close()
			return nil, nil, fmt.Errorf("selected member is not a supported GGUF file")
		}
		rel := filepath.FromSlash(member.Path)
		for dir := filepath.Dir(rel); dir != "."; dir = filepath.Dir(dir) {
			info, err := root.Lstat(dir)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				root.Close()
				return nil, nil, fmt.Errorf("selected file has an unsafe parent component")
			}
		}
		info, err := root.Lstat(rel)
		if err != nil || info.IsDir() || (member.LinkOnly && info.Mode()&os.ModeSymlink == 0) || (!member.LinkOnly && !info.Mode().IsRegular()) {
			root.Close()
			return nil, nil, fmt.Errorf("selected entry changed since confirmation or is unsafe")
		}
		rels = append(rels, rel)
	}
	return root, rels, nil
}

func (s *Server) handleSelectedCacheDelete(w http.ResponseWriter, r *http.Request) {
	var req selectedDeleteRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	if !hfdownloader.IsValidModelName(req.Repo) || req.Type != "model" || req.LocationID == "" || req.GroupID == "" || len(req.Members) == 0 {
		writeError(w, http.StatusBadRequest, "Invalid selection", "Expected a model repo, current locationId, groupId, and confirmed members")
		return
	}

	// Keep the configured-root identity stable through fresh enumeration and
	// unlink. Settings updates take the write lock; this read lock is not held
	// while any client/listener callback is invoked.
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	cfg := s.config
	var selected *cacheSelectionLocation
	for _, loc := range cacheSelectionLocations(cfg, req.Repo) {
		if loc.ID == req.LocationID {
			copy := loc
			selected = &copy
			break
		}
	}
	if selected == nil {
		writeError(w, http.StatusConflict, "Selection is stale", "Refresh the current locations and choose a current location")
		return
	}
	if !selected.CanDelete {
		writeError(w, http.StatusConflict, "Location is not eligible for local deletion", selected.DeleteReason)
		return
	}
	var group *cacheSelectionGroup
	for i := range selected.Groups {
		if selected.Groups[i].ID == req.GroupID {
			copy := selected.Groups[i]
			group = &copy
			break
		}
	}
	if group == nil || !group.CanDelete || !selectionCompositionEqual(req.Members, group.Members) {
		writeError(w, http.StatusConflict, "Selection is stale", "The selected GGUF group or its exact members changed; refresh before deleting")
		return
	}
	if selected.Warning != "" {
		writeError(w, http.StatusConflict, "Location could not be verified", selected.Warning)
		return
	}

	if selected.kind == "hf" {
		cache := cfg.cache()
		rd, err := cache.Repo(req.Repo, hfdownloader.RepoTypeModel)
		if err != nil {
			writeError(w, http.StatusConflict, "HF location changed", err.Error())
			return
		}
		release, ok := s.jobs.reserveSelectedHF(rd.Path(), rd.FriendlyPath())
		if !ok {
			writeError(w, http.StatusConflict, "HF cache is busy", "An operation may change references in this HF repository or its friendly view; retry after it finishes")
			return
		}
		defer release()
		var fresh *cacheSelectionGroup
		for _, loc := range cacheSelectionLocations(cfg, req.Repo) {
			if loc.ID != req.LocationID || loc.kind != "hf" || !loc.CanDelete {
				continue
			}
			for i := range loc.Groups {
				if loc.Groups[i].ID == req.GroupID {
					fresh = &loc.Groups[i]
					break
				}
			}
		}
		if fresh == nil || !selectionCompositionEqual(group.Members, fresh.Members) {
			writeError(w, http.StatusConflict, "Selection is stale", "The selected HF GGUF group changed while deletion was being reserved; refresh before deleting")
			return
		}
		entries := make([]hfdownloader.SelectedGGUFEntry, 0, len(fresh.Members))
		for _, member := range fresh.Members {
			entries = append(entries, hfdownloader.SelectedGGUFEntry{Path: member.Path, Versions: member.Versions})
		}
		protectedRoots, protectionComplete := configuredHFProtectedRoots(cfg, rd)
		if !protectionComplete {
			writeError(w, http.StatusConflict, "Location could not be verified", "A configured local root could not be safely resolved against HF storage")
			return
		}
		result := rd.DeleteSelectedGGUF(entries, protectedRoots...)
		if len(result.Errors) > 0 && !result.Attempted {
			writeError(w, http.StatusConflict, "HF deletion refused", strings.Join(result.Errors, "; "))
			return
		}
		retained := make([]string, 0, len(result.RetainedBlobs))
		for _, blob := range result.RetainedBlobs {
			retained = append(retained, filepath.Base(blob))
		}
		displayPath := func(name string) string {
			if rel, e := filepath.Rel(rd.Path(), name); e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "cache/" + filepath.ToSlash(rel)
			}
			if rel, e := filepath.Rel(rd.FriendlyPath(), name); e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "friendly/" + filepath.ToSlash(rel)
			}
			return filepath.Base(name)
		}
		removed, remaining := make([]string, 0, len(result.Removed)), make([]string, 0, len(result.Remaining))
		for _, name := range result.Removed {
			removed = append(removed, displayPath(name))
		}
		for _, name := range result.Remaining {
			remaining = append(remaining, displayPath(name))
		}
		response := selectedDeleteResponse{OK: len(result.Errors) == 0, Repo: req.Repo, GroupID: fresh.ID, Removed: removed, Remaining: remaining, Errors: result.Errors, RetainedPayloads: retained}
		if response.Removed == nil {
			response.Removed = []string{}
		}
		if response.Remaining == nil {
			response.Remaining = []string{}
		}
		if !response.OK {
			response.Message = "HF deletion was partial; remaining entries and payloads were preserved where still referenced"
		} else if len(result.RetainedBlobs) > 0 {
			response.Message = "Removed selected HF snapshot entries; shared payloads needed by other saved entries were retained"
		} else {
			response.Message = "Removed the selected HF GGUF entries and unreferenced payloads"
		}
		status := http.StatusOK
		if !response.OK {
			status = http.StatusMultiStatus
		}
		writeJSON(w, status, response)
		return
	}
	paths := make([]string, 0, len(group.Members))
	for _, member := range group.Members {
		if !safeSelectedRelative(member.Path) {
			writeError(w, http.StatusConflict, "Selection is unsafe", "A selected path is not a safe relative GGUF entry")
			return
		}
		paths = append(paths, filepath.Join(selected.Path, filepath.FromSlash(member.Path)))
	}
	release, ok := s.jobs.reserveSelectedGGUF(paths)
	if !ok {
		writeError(w, http.StatusConflict, "Selected GGUF is busy", "A queued or active operation may write this exact GGUF group; cancel the conflicting job and retry")
		return
	}
	defer release()
	// Re-enumerate after the reservation barrier: a writer may have completed
	// after the original scan but before reserve acquired the manager lock.
	current := cacheSelectionLocations(cfg, req.Repo)
	var fresh *cacheSelectionGroup
	for _, loc := range current {
		if loc.ID != req.LocationID || loc.kind != "local" || !loc.CanDelete {
			continue
		}
		for i := range loc.Groups {
			if loc.Groups[i].ID == req.GroupID {
				fresh = &loc.Groups[i]
				break
			}
		}
	}
	if fresh == nil || !selectionCompositionEqual(group.Members, fresh.Members) {
		writeError(w, http.StatusConflict, "Selection is stale", "The selected GGUF group changed while deletion was being reserved; refresh before deleting")
		return
	}
	group = fresh
	root, rels, preflightErr := preflightSelectedLocalEntries(selected.Path, group.Members)
	if preflightErr != nil {
		writeError(w, http.StatusConflict, "Selection is unsafe", preflightErr.Error())
		return
	}
	defer root.Close()

	result := selectedDeleteResponse{OK: true, Repo: req.Repo, GroupID: group.ID, Removed: []string{}, Remaining: []string{}}
	for i, member := range group.Members {
		if err := root.Remove(rels[i]); err != nil {
			result.OK = false
			result.Remaining = append(result.Remaining, member.Path)
			result.Errors = append(result.Errors, member.Path+": "+err.Error())
			continue
		}
		result.Removed = append(result.Removed, member.Path)
		if member.LinkOnly {
			result.LinkOnly = true
			result.LinkOnlyEntries = append(result.LinkOnlyEntries, member.Path)
		}
	}
	if result.OK {
		if result.LinkOnly {
			result.Message = "Removed the selected GGUF entries; link-only entries were unlinked without removing their weight targets"
		} else {
			result.Message = "Removed the selected local GGUF files"
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	result.Message = "Some selected entries could not be removed; other files and directories were left unchanged"
	writeJSON(w, http.StatusMultiStatus, result)
}

var errSelectionWriterBusy = errors.New("selected GGUF is reserved for deletion")
