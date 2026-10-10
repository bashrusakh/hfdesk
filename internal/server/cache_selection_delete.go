// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

type jobWriteActivity struct {
	job          *Job
	planned      map[string]frozenMutationPath
	planKnown    bool
	planComplete bool
}

// frozenMutationPath retains the physical spelling for basename predicates
// and an immutable, comparison-only key. Neither field is an incoming lookup
// path; frozen values are never passed to filesystem identity resolvers.
type frozenMutationPath struct {
	physical string
	key      string
	// raw is the spelling used by the operation's filesystem IO. It is kept
	// separately from the immutable comparison snapshot for admission checks.
	raw string
}

func freezeMutationEntry(path string) (frozenMutationPath, error) {
	return freezeMutationEntryWithKey(path, pathIdentityKey)
}

func freezeMutationEntryWithKey(path string, key func(string) string) (frozenMutationPath, error) {
	physical, err := mutationEntryPhysicalIdentity(path)
	if err != nil {
		return frozenMutationPath{}, err
	}
	return frozenMutationPath{physical: physical, key: key(physical)}, nil
}

func freezeMutationDirectory(path string) (frozenMutationPath, error) {
	return freezeMutationDirectoryWithKey(path, pathIdentityKey)
}

func freezeMutationDirectoryWithKey(path string, key func(string) string) (frozenMutationPath, error) {
	physical, err := mutationDirectoryPhysicalIdentity(path)
	if err != nil {
		return frozenMutationPath{}, err
	}
	return frozenMutationPath{physical: physical, key: key(physical)}, nil
}

func frozenMutationOverlap(a, b frozenMutationPath) bool {
	return pathKeysOverlap(a.key, b.key)
}

// mutationScopesOverlap checks both immutable admission snapshots and the
// current mappings of raw spellings whose owners perform filesystem IO there.
func mutationScopesOverlap(a, b frozenMutationPath) bool {
	if frozenMutationOverlap(a, b) {
		return true
	}
	if a.raw != "" {
		if frozenMutationOverlapsRawDirectory(b, a.raw) {
			return true
		}
		current, err := freezeMutationDirectory(a.raw)
		if err != nil || frozenMutationOverlap(current, b) {
			return true
		}
	}
	if b.raw != "" {
		if frozenMutationOverlapsRawDirectory(a, b.raw) {
			return true
		}
		current, err := freezeMutationDirectory(b.raw)
		if err != nil || frozenMutationOverlap(a, current) {
			return true
		}
	}
	return false
}

func pathKeysOverlap(a, b string) bool {
	return withinLocalRoot(a, b) || withinLocalRoot(b, a)
}

func frozenMutationOverlapsRawDirectory(frozen frozenMutationPath, incoming string) bool {
	return frozenMutationOverlapsRawDirectoryWithKey(frozen, incoming, pathIdentityKey)
}

func frozenMutationOverlapsRawDirectoryWithKey(frozen frozenMutationPath, incoming string, key func(string) string) bool {
	dir, err := mutationDirectoryPhysicalIdentity(incoming)
	if err != nil {
		return true
	}
	return pathKeysOverlap(frozen.key, key(dir))
}

func frozenMutationOverlapsRawEntry(frozen frozenMutationPath, incoming string) bool {
	return frozenMutationOverlapsRawEntryWithKey(frozen, incoming, pathIdentityKey)
}

func frozenMutationOverlapsRawEntryWithKey(frozen frozenMutationPath, incoming string, key func(string) string) bool {
	entry, err := mutationEntryPhysicalIdentity(incoming)
	if err != nil {
		return true
	}
	return pathKeysOverlap(frozen.key, key(entry))
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

// mutationEntryPhysicalIdentity resolves symlinks in parent directories but
// preserves the final basename as an entry, so leaf symlink targets are never
// followed. The returned spelling is for filesystem identity only; callers
// must apply pathIdentityKey separately when comparing identities.
func mutationEntryPhysicalIdentity(name string) (string, error) {
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
			return filepath.Clean(resolved), nil
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
	return filepath.Clean(filepath.Join(resolved, base)), nil
}

// mutationEntryIdentity is a comparison identity, not a filesystem path.
func mutationEntryIdentity(name string) (string, error) {
	physical, err := mutationEntryPhysicalIdentity(name)
	if err != nil {
		return "", err
	}
	return pathIdentityKey(physical), nil
}

// mutationDirectoryIdentity treats base as a directory scope, not a final
// entry. Existing directory aliases resolve to their destination; absent
// ordinary suffixes are reconstructed by mutationEntryPhysicalIdentity before
// the comparison key is derived.
func mutationDirectoryIdentity(name string) (string, error) {
	physical, err := mutationDirectoryPhysicalIdentity(name)
	if err != nil {
		return "", err
	}
	return pathIdentityKey(physical), nil
}

func mutationDirectoryPhysicalIdentity(name string) (string, error) {
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
		return filepath.Clean(resolved), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	return mutationEntryPhysicalIdentity(abs)
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
	snapshotNamespace := filepath.Base(filepath.Clean(inv.base)) == "snapshots"
	candidates := make([]string, 0, len(spellings[parent])*2)
	for spelling := range spellings[parent] {
		candidate := joinSlashPath(spelling, name)
		candidates = append(candidates, candidate)
		// Snapshot paths are physically rooted at snapshots/<commit>, while
		// the plan's RelativePath starts below that commit directory. The
		// commit is not known until planning completes, so compare both
		// spellings.
		if snapshotNamespace {
			if idx := strings.Index(candidate, "/"); idx >= 0 {
				candidates = append(candidates, candidate[idx+1:])
			}
		}
	}
	return candidates, true
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
	bases := []string{base}
	if snapshotBase := jobSnapshotBase(job); snapshotBase != "" && snapshotBase != base {
		bases = append(bases, snapshotBase)
	}
	// A negative pre-plan answer requires a complete physical-to-repo mapping
	// proof for every writable namespace of the job. Any unknown or outward
	// mapping short-circuits to the conservative possible-writer answer.
	inventories := make([]writableNamespaceInventory, 0, len(bases))
	aliasesFound := false
	for _, writableBase := range bases {
		inv, ok := inventoryWritableNamespace(writableBase)
		if !ok {
			return true
		}
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

func jobMayWriteFrozenSelectedPath(job *Job, target frozenMutationPath) bool {
	base := jobDestinationBase(job)
	if base == "" {
		return false
	}
	if pathWithinWriterBase(base, target.key) {
		return hfdownloader.GGUFPathSelected(filepath.Base(target.physical), job.Filters, job.Excludes, job.ExactMatch)
	}
	if snapshotBase := jobSnapshotBase(job); snapshotBase != "" && pathWithinWriterBase(snapshotBase, target.key) {
		return hfdownloader.GGUFPathSelected(filepath.Base(target.physical), job.Filters, job.Excludes, job.ExactMatch)
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

func plannedEntriesConflict(frozenEntries []frozenMutationPath, plannedPaths map[string]frozenMutationPath) bool {
	for _, frozen := range frozenEntries {
		for _, planned := range plannedPaths {
			if frozenMutationOverlap(frozen, planned) {
				return true
			}
		}
	}
	return false
}

// plannedRawEntriesConflict re-resolves only incoming raw plan spellings. The
// frozen values remain authoritative for the mapping captured at plan time;
// raw keys are retained separately because their aliases may have moved.
func plannedRawEntriesConflict(frozenEntries []frozenMutationPath, plannedPaths map[string]frozenMutationPath) bool {
	if plannedEntriesConflict(frozenEntries, plannedPaths) {
		return true
	}
	for raw, captured := range plannedPaths {
		current, err := freezeMutationEntry(raw)
		if err != nil {
			// An unfreezable destination remains represented by its raw writer
			// spelling; the caller's incomplete-plan fallback conservatively
			// covers destinations whose current mapping is still unavailable.
			if captured.key == "" {
				continue
			}
			return true
		}
		for _, frozen := range frozenEntries {
			if frozenMutationOverlap(frozen, current) {
				return true
			}
		}
	}
	return false
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
	clean := make([]frozenMutationPath, 0, len(paths))
	for _, p := range paths {
		// Freeze the selected entry's physical parent at admission. The final
		// basename is intentionally retained as an entry (not followed when it
		// is a symlink), so later root-alias changes cannot move this reservation.
		identity, err := freezeMutationEntry(p)
		if err != nil {
			return nil, false
		}
		clean = append(clean, identity)
	}
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return nil, false
	}
	entryConflicts := func(scopes []frozenMutationPath) bool {
		for _, a := range scopes {
			for _, b := range clean {
				if frozenMutationOverlap(a, b) {
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
				if mutationScopesOverlap(scope, entry) {
					m.mu.Unlock()
					return nil, false
				}
			}
		}
	}
	for _, scopes := range m.mutationScopes {
		for _, scope := range scopes {
			for _, entry := range clean {
				if mutationScopesOverlap(scope, entry) {
					m.mu.Unlock()
					return nil, false
				}
			}
		}
	}
	for _, activity := range m.runActivities {
		if activity.planKnown {
			if plannedRawEntriesConflict(clean, activity.planned) {
				m.mu.Unlock()
				return nil, false
			}
			if activity.planComplete {
				continue
			}
		}
		for _, target := range clean {
			if jobMayWriteFrozenSelectedPath(activity.job, target) {
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
			if jobMayWriteFrozenSelectedPath(job, target) {
				m.mu.Unlock()
				return nil, false
			}
		}
	}
	if m.deleteReservations == nil {
		m.deleteReservations = make(map[uint64][]frozenMutationPath)
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
	complete := true
	for _, item := range plan.Items {
		friendly, err := hfdownloader.SafeJoin(base, item.RelativePath)
		if err != nil {
			complete = false
			continue
		}
		entries = append(entries, friendly)
		if repo != nil {
			snapshot, err := repo.SnapshotPath(plan.Commit, item.RelativePath)
			if err != nil {
				complete = false
				continue
			}
			entries = append(entries, snapshot)
		}
	}
	return entries, complete
}

func hfReservationConflictsJob(scopes []frozenMutationPath, job *Job) bool {
	base := jobDestinationBase(job)
	if base == "" {
		return false
	}
	paths := []string{base}
	if repo := hfJobRepoPath(job); repo != "" {
		paths = append(paths, repo)
	}
	for _, scope := range scopes {
		for _, path := range paths {
			// Compare both the reservation's captured identity and the current
			// mappings of actual writer IO spellings. Either side may have been
			// redirected since admission; neither mapping alone is sufficient.
			current, err := freezeMutationDirectory(path)
			if err != nil || mutationScopesOverlap(scope, current) {
				return true
			}
		}
	}
	return false
}

// reserveSelectedHF protects the complete selected HF repo-reference namespace
// and the associated friendly view for the full scan/unlink interval.
func (m *JobManager) reserveSelectedHF(repoDir, friendlyDir string) (func(), bool) {
	scopes := make([]frozenMutationPath, 0, 2)
	for _, scope := range []string{repoDir, friendlyDir} {
		frozen, err := freezeMutationDirectory(scope)
		if err != nil {
			return nil, false
		}
		frozen.raw = scope
		scopes = append(scopes, frozen)
	}
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return nil, false
	}
	conflictScopes := func(other []frozenMutationPath) bool {
		for _, a := range scopes {
			for _, b := range other {
				if mutationScopesOverlap(a, b) {
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
		if activity.planKnown && plannedRawEntriesConflict(scopes, activity.planned) {
			m.mu.Unlock()
			return nil, false
		}
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
		m.hfRepoReservations = make(map[uint64][]frozenMutationPath)
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

// plannedDestinationSet retains every raw absolute lookup spelling as its map
// key and freezes a separate physical/comparison identity as its value. Thus
// case-distinct raw paths are not collapsed, and comparisons never re-resolve
// a completed plan's stored path.
func plannedDestinationSet(paths []string) (map[string]frozenMutationPath, bool) {
	planned := make(map[string]frozenMutationPath, len(paths))
	complete := true
	for _, path := range paths {
		absolute, err := filepath.Abs(filepath.Clean(path))
		if err != nil {
			complete = false
			continue
		}
		frozen, err := freezeMutationEntry(absolute)
		if err != nil {
			planned[absolute] = frozenMutationPath{raw: absolute}
			complete = false
			continue
		}
		frozen.raw = absolute
		planned[absolute] = frozen
	}
	return planned, complete
}

func (m *JobManager) beginCacheMutation(paths ...string) (func(), bool) {
	clean := make([]frozenMutationPath, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		frozen, err := freezeMutationDirectory(p)
		if err != nil {
			return nil, false
		}
		frozen.raw = p
		clean = append(clean, frozen)
	}
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return nil, false
	}
	for _, scopes := range m.deleteReservations {
		for _, scope := range scopes {
			for _, write := range clean {
				if mutationScopesOverlap(write, scope) {
					m.mu.Unlock()
					return nil, false
				}
			}
		}
	}
	for _, scopes := range m.hfRepoReservations {
		for _, scope := range scopes {
			for _, write := range clean {
				if mutationScopesOverlap(write, scope) {
					m.mu.Unlock()
					return nil, false
				}
			}
		}
	}
	if m.mutationScopes == nil {
		m.mutationScopes = make(map[uint64][]frozenMutationPath)
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
			if jobMayWriteFrozenSelectedPath(job, target) {
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
	m.runActivities[id] = &jobWriteActivity{job: job, planned: make(map[string]frozenMutationPath)}
	return id
}

type selectedDeleteRequest struct {
	Repo       string                 `json:"repo"`
	Type       string                 `json:"type"`
	LocationID string                 `json:"locationId"`
	GroupID    string                 `json:"groupId"`
	Members    []cacheSelectionMember `json:"members"`
}

// selectedDeleteHooks are per-server synchronization points for deterministic
// endpoint tests. Production servers leave them nil.
type selectedDeleteHooks struct {
	beforeRootOpen       func()
	afterRootOpen        func()
	afterReservation     func()
	afterGroupValidation func()
	beforeIdentityPin    func(string)
	forceHardLinkPin     bool
	beforeHardLinkPin    func(source, destination string) error
	removeHardLinkPin    func(*os.Root, string) error
	closeHardLinkRoot    func(*os.Root) error
	afterFinalScan       func()
	beforeRemove         func(int)
	removeEntry          func(*os.Root, string) error
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

// openSelectedDirectoryRoot makes the last path component a directory
// traversal component (rather than opening it as a file first). In particular,
// a concurrent replacement with a FIFO must fail without waiting for a peer.
func openSelectedDirectoryRoot(root *os.Root, name string) (*os.Root, error) {
	return root.OpenRoot(name + string(filepath.Separator) + ".")
}

func openSelectedLocalBase(name string) (*os.Root, error) {
	return os.OpenRoot(name + string(filepath.Separator) + ".")
}

func openSelectedDirectoryFile(root *os.Root, name string) (*os.File, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("local selection path %q is no longer a directory", name)
	}
	child, err := openSelectedDirectoryRoot(root, name)
	if err != nil {
		return nil, err
	}
	f, err := child.Open(".")
	closeErr := child.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		_ = f.Close()
		return nil, closeErr
	}
	info, err := f.Stat()
	if err != nil || !info.IsDir() || !os.SameFile(before, info) {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("local selection path %q is no longer a directory", name)
	}
	return f, nil
}

// openSelectedLocalRepo admits a repository through its configured lexical
// location, then keeps an os.Root anchored to that exact repository directory.
// Comparing the opened directory with both observed bindings rejects an alias
// retarget that races admission.
func openSelectedLocalRepo(repoDir string, beforeOpen func()) (*os.Root, string, error) {
	base := filepath.Dir(filepath.Dir(repoDir))
	before, err := os.Stat(base)
	if err != nil || !before.IsDir() {
		return nil, "", fmt.Errorf("configured local root is unavailable")
	}
	physicalBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return nil, "", err
	}
	physicalBase, err = filepath.Abs(physicalBase)
	if err != nil {
		return nil, "", err
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	root, err := openSelectedLocalBase(base)
	if err != nil {
		return nil, "", err
	}
	openedBase, openedErr := root.Stat(".")
	afterBase, afterErr := os.Stat(base)
	physicalBaseInfo, physicalErr := os.Stat(physicalBase)
	if openedErr != nil || afterErr != nil || physicalErr != nil || !os.SameFile(before, openedBase) || !os.SameFile(openedBase, afterBase) || !os.SameFile(openedBase, physicalBaseInfo) {
		root.Close()
		return nil, "", fmt.Errorf("configured local root changed while opening its deletion scope")
	}
	for _, component := range []string{filepath.Base(filepath.Dir(repoDir)), filepath.Base(repoDir)} {
		before, statErr := root.Lstat(component)
		if statErr != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, "", fmt.Errorf("selected repository has an unsafe owner/model component")
		}
		next, openErr := openSelectedDirectoryRoot(root, component)
		if openErr != nil {
			root.Close()
			return nil, "", openErr
		}
		opened, openedErr := next.Stat(".")
		after, afterErr := root.Lstat(component)
		root.Close()
		root = next
		if openedErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
			root.Close()
			return nil, "", fmt.Errorf("selected repository changed while opening its deletion scope")
		}
	}
	physical := filepath.Join(physicalBase, filepath.Base(filepath.Dir(repoDir)), filepath.Base(repoDir))
	opened, openedErr := root.Stat(".")
	physicalInfo, statErr := os.Stat(physical)
	if openedErr != nil || statErr != nil || !os.SameFile(opened, physicalInfo) {
		root.Close()
		return nil, "", fmt.Errorf("selected repository changed while opening its deletion scope")
	}
	return root, physical, nil
}

// localSelectionFilesRoot is the delete path's authoritative enumeration. It
// deliberately does not follow directory symlinks and never leaves the opened
// repository root. The normal cache-selection scanner remains unchanged.
type selectedHardLinkPin struct {
	name    string
	path    string
	info    os.FileInfo
	created bool
}

type hardLinkParentComponent struct {
	name string
	info os.FileInfo
}

type selectedHardLinkContainer struct {
	parent           string
	parentRoot       *os.Root
	parentInfo       os.FileInfo
	parentComponents []hardLinkParentComponent
	name             string
	info             os.FileInfo
	root             *os.Root
	created          bool
	pins             []*selectedHardLinkPin
}

// selectedEntryPinSet owns every request-local identity resource from the
// moment it is created until cleanup is explicitly finalized before response.
type selectedEntryPinSet struct {
	root              *os.Root
	expected          map[string]cacheSelectionMember
	forceHardLink     bool
	beforeHardLink    func(source, destination string) error
	removeHardLink    func(*os.Root, string) error
	closeHardLinkRoot func(*os.Root) error
	handles           []*os.File
	containers        map[string]*selectedHardLinkContainer
	containerOrder    []*selectedHardLinkContainer
	extraRoots        []*os.Root
	cleaned           bool
}

type hardLinkPinLocationUnavailable struct {
	err error
}

func (e *hardLinkPinLocationUnavailable) Error() string { return e.err.Error() }

func (e *hardLinkPinLocationUnavailable) Unwrap() error { return e.err }

func newSelectedEntryPinSet(root *os.Root, hooks selectedDeleteHooks, expected map[string]cacheSelectionMember) *selectedEntryPinSet {
	return &selectedEntryPinSet{
		root:              root,
		expected:          expected,
		forceHardLink:     hooks.forceHardLinkPin,
		beforeHardLink:    hooks.beforeHardLinkPin,
		removeHardLink:    hooks.removeHardLinkPin,
		closeHardLinkRoot: hooks.closeHardLinkRoot,
		containers:        make(map[string]*selectedHardLinkContainer),
	}
}

func (p *selectedEntryPinSet) hardLinkParent(parent string) (*selectedHardLinkContainer, error) {
	parent = filepath.ToSlash(filepath.Clean(filepath.FromSlash(parent)))
	if existing := p.containers[parent]; existing != nil {
		return existing, nil
	}
	container := &selectedHardLinkContainer{parent: parent}
	if parent == "." {
		container.parentRoot = p.root
		info, err := p.root.Stat(".")
		if err != nil {
			return nil, fmt.Errorf("stat selected-entry parent: %w", err)
		}
		container.parentInfo = info
		p.containers[parent] = container
		p.containerOrder = append(p.containerOrder, container)
		return container, nil
	}
	clean := filepath.FromSlash(parent)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("selected-entry parent is outside the admitted repository")
	}
	current := p.root
	var prefix string
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		before, err := current.Lstat(component)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("selected-entry parent has an unsafe component")
		}
		next, err := openSelectedDirectoryRoot(current, component)
		if err != nil {
			return nil, fmt.Errorf("open selected-entry parent component %q: %w", component, err)
		}
		// Own each opened parent immediately, including a later component failure.
		p.extraRoots = append(p.extraRoots, next)
		opened, openedErr := next.Stat(".")
		after, afterErr := current.Lstat(component)
		if openedErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
			if openedErr != nil {
				return nil, fmt.Errorf("verify selected-entry parent component %q: %w", component, openedErr)
			}
			if afterErr != nil {
				return nil, fmt.Errorf("recheck selected-entry parent component %q: %w", component, afterErr)
			}
			return nil, fmt.Errorf("selected-entry parent component %q changed while opening", component)
		}
		prefix = filepath.Join(prefix, component)
		container.parentComponents = append(container.parentComponents, hardLinkParentComponent{name: filepath.ToSlash(prefix), info: after})
		current = next
	}
	container.parentRoot = current
	parentInfo, err := current.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("stat selected-entry parent %q: %w", parent, err)
	}
	container.parentInfo = parentInfo
	p.containers[parent] = container
	p.containerOrder = append(p.containerOrder, container)
	return container, nil
}

func (p *selectedEntryPinSet) verifyHardLinkParentBinding(container *selectedHardLinkContainer) error {
	if container.parentRoot == nil || container.parentInfo == nil {
		return fmt.Errorf("selected-entry parent %q has no verified binding", container.parent)
	}
	for _, component := range container.parentComponents {
		current, err := p.root.Lstat(filepath.FromSlash(component.name))
		if err != nil {
			return fmt.Errorf("verify selected-entry parent %q: %w", component.name, err)
		}
		if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(component.info, current) {
			return fmt.Errorf("selected-entry parent %q binding changed", component.name)
		}
	}
	opened, err := container.parentRoot.Stat(".")
	if err != nil {
		return fmt.Errorf("verify opened selected-entry parent %q: %w", container.parent, err)
	}
	if !os.SameFile(container.parentInfo, opened) {
		return fmt.Errorf("opened selected-entry parent %q binding changed", container.parent)
	}
	return nil
}

func (p *selectedEntryPinSet) ensureHardLinkContainer(parent string) (*selectedHardLinkContainer, error) {
	container, err := p.hardLinkParent(parent)
	if err != nil {
		return nil, err
	}
	if err := p.verifyHardLinkParentBinding(container); err != nil {
		return nil, err
	}
	if container.created {
		if container.root == nil {
			return nil, fmt.Errorf("temporary identity-pin container %q is unavailable", container.name)
		}
		return container, nil
	}
	for attempt := 0; attempt < 8; attempt++ {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return nil, fmt.Errorf("create temporary identity-pin name: %w", err)
		}
		name := ".hfdesk-delete-pin-" + hex.EncodeToString(random)
		if err := container.parentRoot.Mkdir(name, 0700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			if os.IsPermission(err) {
				return nil, &hardLinkPinLocationUnavailable{err: fmt.Errorf("create temporary identity-pin container %q under %q: %w", name, parent, err)}
			}
			return nil, fmt.Errorf("create temporary identity-pin container %q under %q: %w", name, parent, err)
		}
		// Record ownership immediately after the exclusive mkdir succeeds.
		container.name = name
		container.created = true
		info, err := container.parentRoot.Lstat(name)
		if err != nil {
			return nil, fmt.Errorf("capture temporary identity-pin container %q: %w", name, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("temporary identity-pin container %q changed during creation", name)
		}
		if info.Mode().Perm()&0077 != 0 {
			return nil, fmt.Errorf("temporary identity-pin container %q is not private", name)
		}
		container.info = info
		containerRoot, err := openSelectedDirectoryRoot(container.parentRoot, name)
		if err != nil {
			return nil, fmt.Errorf("open temporary identity-pin container %q: %w", name, err)
		}
		container.root = containerRoot
		opened, statErr := containerRoot.Stat(".")
		if statErr != nil || !os.SameFile(info, opened) {
			if statErr != nil {
				return nil, fmt.Errorf("verify temporary identity-pin container %q: %w", name, statErr)
			}
			return nil, fmt.Errorf("temporary identity-pin container %q changed while opening", name)
		}
		if err := verifyHardLinkContainerBinding(container); err != nil {
			return nil, err
		}
		if err := p.verifyHardLinkParentBinding(container); err != nil {
			return nil, err
		}
		return container, nil
	}
	return nil, fmt.Errorf("could not allocate an exclusive temporary identity-pin container under %q", parent)
}

func verifyHardLinkContainerBinding(container *selectedHardLinkContainer) error {
	if container.root == nil || container.info == nil {
		return fmt.Errorf("temporary identity-pin container %q has no verified binding", container.name)
	}
	current, err := container.parentRoot.Lstat(container.name)
	if err != nil {
		return fmt.Errorf("verify temporary identity-pin container %q: %w", container.name, err)
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(container.info, current) {
		return fmt.Errorf("temporary identity-pin container %q binding changed", container.name)
	}
	opened, err := container.root.Stat(".")
	if err != nil || !os.SameFile(container.info, opened) {
		if err != nil {
			return fmt.Errorf("verify opened temporary identity-pin container %q: %w", container.name, err)
		}
		return fmt.Errorf("opened temporary identity-pin container %q binding changed", container.name)
	}
	return nil
}

func (p *selectedEntryPinSet) pinByHardLink(name string) (*selectedEntryIdentity, error) {
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(name)))
	identity, err := p.pinByHardLinkAt(name, parent)
	if err == nil || parent == "." {
		return identity, err
	}
	var unavailable *hardLinkPinLocationUnavailable
	if !errors.As(err, &unavailable) {
		return nil, err
	}
	fallback, fallbackErr := p.pinByHardLinkAt(name, ".")
	if fallbackErr != nil {
		return nil, fmt.Errorf("source-parent identity pin failed: %v; repository-root fallback failed: %w", err, fallbackErr)
	}
	return fallback, nil
}

func (p *selectedEntryPinSet) pinByHardLinkAt(name, parent string) (*selectedEntryIdentity, error) {
	// Confirm the newest known source metadata before ensureHardLinkContainer
	// can create any directory or link artifact. The later identity check still
	// detects a source replaced in the unavoidable interval after this read.
	if expected, ok := p.expected[filepath.ToSlash(name)]; ok {
		current, err := p.root.Lstat(filepath.FromSlash(name))
		if err != nil {
			return nil, err
		}
		if expected.LinkOnly || !current.Mode().IsRegular() || current.Size() != expected.Size {
			return nil, fmt.Errorf("selected source changed before temporary identity pin creation")
		}
	}
	container, err := p.ensureHardLinkContainer(parent)
	if err != nil {
		return nil, err
	}
	if err := verifyHardLinkContainerBinding(container); err != nil {
		return nil, err
	}
	if err := p.verifyHardLinkParentBinding(container); err != nil {
		return nil, err
	}
	pinName := fmt.Sprintf("entry-%08d.pin", len(container.pins))
	destination := filepath.ToSlash(filepath.Join(parent, container.name, pinName))
	if p.beforeHardLink != nil {
		if err := p.beforeHardLink(name, destination); err != nil {
			if os.IsPermission(err) {
				return nil, &hardLinkPinLocationUnavailable{err: fmt.Errorf("create temporary identity pin %q: %w", destination, err)}
			}
			return nil, fmt.Errorf("create temporary identity pin %q: %w", destination, err)
		}
	}
	parentName := filepath.FromSlash(name)
	if parent != "." {
		parentName, err = filepath.Rel(filepath.FromSlash(parent), parentName)
		if err != nil || parentName == ".." || filepath.IsAbs(parentName) || strings.HasPrefix(parentName, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("selected entry is outside its hard-link pin parent")
		}
	}
	if err := verifyHardLinkContainerBinding(container); err != nil {
		return nil, err
	}
	if err := p.verifyHardLinkParentBinding(container); err != nil {
		return nil, err
	}
	containerDestination := filepath.Join(container.name, pinName)
	if err := container.parentRoot.Link(parentName, containerDestination); err != nil {
		if os.IsPermission(err) {
			return nil, &hardLinkPinLocationUnavailable{err: fmt.Errorf("create temporary identity pin %q: %w", destination, err)}
		}
		return nil, fmt.Errorf("create temporary identity pin %q: %w", destination, err)
	}
	record := &selectedHardLinkPin{name: pinName, path: destination, created: true}
	// Link success creates a resource immediately; register it before any stat.
	container.pins = append(container.pins, record)
	info, err := container.root.Lstat(pinName)
	if err != nil {
		return nil, fmt.Errorf("capture temporary identity pin %q: %w", destination, err)
	}
	record.info = info
	if err := verifyHardLinkContainerBinding(container); err != nil {
		return nil, err
	}
	if err := p.verifyHardLinkParentBinding(container); err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("temporary identity pin %q is not a regular file", destination)
	}
	sourceInfo, err := container.parentRoot.Lstat(parentName)
	if err != nil {
		return nil, fmt.Errorf("verify selected source for temporary identity pin %q: %w", destination, err)
	}
	expected, hasExpected := p.expected[filepath.ToSlash(name)]
	if !sourceInfo.Mode().IsRegular() || !os.SameFile(info, sourceInfo) || info.Size() != sourceInfo.Size() || (hasExpected && (expected.LinkOnly || sourceInfo.Size() != expected.Size)) {
		return nil, fmt.Errorf("selected source changed while temporary identity pin %q was created", destination)
	}
	return &selectedEntryIdentity{info: info}, nil
}

func (p *selectedEntryPinSet) pin(root *os.Root, physicalRepo, name string, linkOnly bool) (*selectedEntryIdentity, error) {
	if expected, ok := p.expected[filepath.ToSlash(name)]; ok {
		current, err := root.Lstat(filepath.FromSlash(name))
		if err != nil {
			return nil, err
		}
		if (current.Mode()&os.ModeSymlink != 0) != expected.LinkOnly || (!expected.LinkOnly && (!current.Mode().IsRegular() || current.Size() != expected.Size)) {
			return nil, fmt.Errorf("selected entry changed before identity pinning")
		}
	}
	if !linkOnly && p.forceHardLink {
		return p.pinByHardLink(name)
	}
	var pin *os.File
	var err error
	if linkOnly {
		pin, err = openSelectedLinkEntry(physicalRepo, name)
	} else {
		pin, err = openSelectedRegularEntry(root, physicalRepo, name)
	}
	if err != nil {
		if !linkOnly && shouldHardLinkSelectedEntry(err) {
			return p.pinByHardLink(name)
		}
		return nil, err
	}
	// Own the open handle before its metadata check so every error path closes it.
	p.handles = append(p.handles, pin)
	info, err := pin.Stat()
	if err != nil {
		return nil, err
	}
	sourceInfo, err := root.Lstat(filepath.FromSlash(name))
	if err != nil {
		return nil, err
	}
	expected, hasExpected := p.expected[filepath.ToSlash(name)]
	if !os.SameFile(info, sourceInfo) || (sourceInfo.Mode()&os.ModeSymlink != 0) != linkOnly || (info.Mode()&os.ModeSymlink != 0) != linkOnly || (!linkOnly && info.Size() != sourceInfo.Size()) || (hasExpected && (expected.LinkOnly != linkOnly || (!linkOnly && sourceInfo.Size() != expected.Size))) {
		return nil, fmt.Errorf("selected entry changed while its identity was being captured")
	}
	return &selectedEntryIdentity{info: info, pin: pin}, nil
}

func (p *selectedEntryPinSet) cleanup() []string {
	if p.cleaned {
		return nil
	}
	p.cleaned = true
	var failures []string
	for _, handle := range p.handles {
		if err := handle.Close(); err != nil {
			failures = append(failures, "close selected-entry identity handle: "+err.Error())
		}
	}
	for _, container := range p.containerOrder {
		parentBindingErr := p.verifyHardLinkParentBinding(container)
		if parentBindingErr != nil {
			failures = append(failures, fmt.Sprintf("selected-entry parent %q name binding changed during cleanup; retained parent handle remains anchored: %v", container.parent, parentBindingErr))
		}
		for _, pin := range container.pins {
			if !pin.created {
				continue
			}
			artifact := pin.path
			if container.root == nil || pin.info == nil {
				failures = append(failures, fmt.Sprintf("could not verify temporary identity pin %q; possible artifact remains", artifact))
				continue
			}
			current, err := container.root.Lstat(pin.name)
			if os.IsNotExist(err) {
				failures = append(failures, fmt.Sprintf("temporary identity pin %q binding disappeared; possible renamed artifact remains", artifact))
				continue
			}
			if err != nil {
				failures = append(failures, fmt.Sprintf("inspect temporary identity pin %q: %v", artifact, err))
				continue
			}
			if !os.SameFile(pin.info, current) {
				failures = append(failures, fmt.Sprintf("temporary identity pin %q binding changed; possible artifact remains", artifact))
				continue
			}
			remove := container.root.Remove
			if p.removeHardLink != nil {
				remove = func(name string) error { return p.removeHardLink(container.root, name) }
			}
			if err := remove(pin.name); err != nil {
				failures = append(failures, fmt.Sprintf("remove temporary identity pin %q (possible artifact remains): %v", artifact, err))
			}
		}
		if container.root != nil {
			closeRoot := container.root.Close
			if p.closeHardLinkRoot != nil {
				closeRoot = func() error { return p.closeHardLinkRoot(container.root) }
			}
			if err := closeRoot(); err != nil {
				failures = append(failures, fmt.Sprintf("close temporary identity-pin container %q: %v", container.name, err))
			}
			container.root = nil
		}
		if !container.created {
			continue
		}
		if container.info == nil {
			failures = append(failures, fmt.Sprintf("temporary identity-pin container %q could not be safely identified; possible artifact remains", filepath.ToSlash(filepath.Join(container.parent, container.name))))
			continue
		}
		current, err := container.parentRoot.Lstat(container.name)
		if os.IsNotExist(err) || err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(container.info, current) {
			failures = append(failures, fmt.Sprintf("temporary identity-pin container %q binding changed; possible artifact remains", filepath.ToSlash(filepath.Join(container.parent, container.name))))
			continue
		}
		if err := container.parentRoot.Remove(container.name); err != nil {
			failures = append(failures, fmt.Sprintf("remove temporary identity-pin container %q (possible artifact remains): %v", filepath.ToSlash(filepath.Join(container.parent, container.name)), err))
		}
		if parentBindingErr == nil {
			if err := p.verifyHardLinkParentBinding(container); err != nil {
				failures = append(failures, fmt.Sprintf("selected-entry parent %q name binding changed during cleanup; operations stayed on the retained parent handle: %v", container.parent, err))
			}
		}
	}
	for i := len(p.extraRoots) - 1; i >= 0; i-- {
		if err := p.extraRoots[i].Close(); err != nil {
			failures = append(failures, fmt.Sprintf("close selected-entry parent handle: %v", err))
		}
	}
	return failures
}

func withPinCleanupDetails(details string, failures []string) string {
	if len(failures) == 0 {
		return details
	}
	return details + "; temporary identity-pin cleanup: " + strings.Join(failures, "; ")
}

// localSelectionWalkFS keeps WalkDir's directory opens rooted and prevents a
// replaced directory entry from blocking indefinitely when it becomes a FIFO.
type localSelectionWalkFS struct{ root *os.Root }

func (f localSelectionWalkFS) Open(name string) (fs.File, error) {
	return openLocalSelectionDirectory(f.root, name)
}

func localSelectionFilesRoot(root *os.Root, physicalRepo string, excluded []string, pinPaths map[string]cacheSelectionMember, beforeIdentityPin func(string), pins *selectedEntryPinSet) ([]cacheSelectionFile, string) {
	var files []cacheSelectionFile
	var warning string
	excludedSet := make(map[string]bool, len(excluded))
	for _, name := range excluded {
		excludedSet[filepath.ToSlash(pathIdentityKey(filepath.Clean(name)))] = true
	}
	err := fs.WalkDir(localSelectionWalkFS{root: root}, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if excludedSet[filepath.ToSlash(pathIdentityKey(filepath.Clean(name)))] {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".gguf") {
			return nil
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		relative := filepath.ToSlash(name)
		var identity *selectedEntryIdentity
		if expected, selected := pinPaths[relative]; selected {
			if (info.Mode()&os.ModeSymlink != 0) != expected.LinkOnly || (!expected.LinkOnly && (!info.Mode().IsRegular() || info.Size() != expected.Size)) {
				return fmt.Errorf("selected entry changed before identity pinning")
			}
			rel := filepath.FromSlash(relative)
			for dir := filepath.Dir(rel); dir != "."; dir = filepath.Dir(dir) {
				parentInfo, parentErr := root.Lstat(dir)
				if parentErr != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("selected file has an unsafe parent component")
				}
			}
			if beforeIdentityPin != nil {
				beforeIdentityPin(relative)
			}
			identity, err = pins.pin(root, physicalRepo, relative, info.Mode()&os.ModeSymlink != 0)
			if err != nil {
				return err
			}
		}
		size, linkOnly := info.Size(), info.Mode()&os.ModeSymlink != 0
		if !linkOnly && identity != nil {
			size = identity.info.Size()
		}
		if linkOnly {
			// Root.Stat handles ordinary in-root links. For a relative link whose
			// target is outside this repository, resolve only read-only metadata
			// from the already admitted physical parent; unlink still acts on the
			// link entry alone.
			actual, statErr := root.Stat(name)
			if statErr != nil {
				parent := filepath.Join(physicalRepo, filepath.Dir(filepath.FromSlash(name)))
				target, linkErr := root.Readlink(name)
				if linkErr != nil {
					warning = "A GGUF link could not be resolved; unavailable links are omitted"
					return nil
				}
				if filepath.IsAbs(target) {
					actual, statErr = os.Stat(target)
				} else {
					actual, statErr = os.Stat(filepath.Join(parent, target))
				}
			}
			if statErr != nil || !actual.Mode().IsRegular() {
				warning = "A GGUF link could not be resolved; unavailable links are omitted"
				return nil
			}
			size = actual.Size()
		}
		files = append(files, cacheSelectionFile{path: relative, size: size, linkOnly: linkOnly, identity: identity})
		return nil
	})
	if err != nil {
		return files, "Could not fully read this location: " + err.Error()
	}
	return files, warning
}

// selectedDeleteExclusions projects configured descendant ownership into the
// admitted repository namespace. The selected root's physical identity is
// supplied by its open handle; the selected alias is never re-resolved here.
func selectedDeleteExclusions(cfg Config, selected *cacheSelectionLocation, physicalRepo, admittedSourceBase string) ([]string, bool) {
	roots := localCacheRoots(cfg.cacheRoot(), cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes)
	selectedLexical, err := filepath.Abs(filepath.Clean(selected.Path))
	if err != nil {
		return nil, false
	}
	selectedBase := filepath.Dir(filepath.Dir(selectedLexical))
	selectedBaseLexical, baseErr := filepath.Abs(filepath.Clean(selectedBase))
	if baseErr != nil || admittedSourceBase == "" {
		return nil, false
	}
	selectedBaseLexical = pathIdentityKey(selectedBaseLexical)
	selectedBasePhysical := pathIdentityKey(filepath.Clean(admittedSourceBase))
	selectedLexical = pathIdentityKey(selectedLexical)
	physicalRepo = pathIdentityKey(filepath.Clean(physicalRepo))
	var excluded []string
	seen := make(map[string]bool)
	add := func(rel string) bool {
		rel = filepath.Clean(rel)
		if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false
		}
		key := filepath.ToSlash(rel)
		if !seen[key] {
			seen[key] = true
			excluded = append(excluded, key)
		}
		return true
	}
	for _, candidate := range roots {
		candidateLexical, absErr := filepath.Abs(filepath.Clean(candidate.Path))
		if absErr != nil {
			return nil, false
		}
		candidateLexical = pathIdentityKey(candidateLexical)
		_, candidatePhysical, ok := configuredRootIdentity(candidate.Path)
		if !ok {
			return nil, false
		}
		// GET projects ownership only from descendants of the selected source
		// root. A separately configured root inside that source which contains
		// this repo (including an exact physical alias) takes ownership of the
		// repo from above; deleting through the broader source would otherwise
		// disagree with the locations GET presents.
		candidatePhysical = pathIdentityKey(filepath.Clean(candidatePhysical))
		if candidateLexical == selectedBaseLexical || candidatePhysical == selectedBasePhysical {
			continue
		}
		insideSource := withinLocalRoot(selectedBaseLexical, candidateLexical) || withinLocalRoot(selectedBasePhysical, candidatePhysical)
		containsRepo := withinLocalRoot(candidateLexical, selectedLexical) || withinLocalRoot(candidatePhysical, physicalRepo)
		if insideSource && containsRepo {
			return nil, false
		}
		if withinLocalRoot(selectedLexical, candidateLexical) {
			rel, relErr := filepath.Rel(selectedLexical, candidateLexical)
			if relErr != nil || !add(rel) {
				return nil, false
			}
		}
		if candidatePhysical != physicalRepo && withinLocalRoot(physicalRepo, candidatePhysical) {
			rel, relErr := filepath.Rel(physicalRepo, candidatePhysical)
			if relErr != nil || !add(rel) {
				return nil, false
			}
		}
	}
	return excluded, true
}

// selectedLocalLocationCandidate resolves the registered lexical root using
// the same root order and location-ID derivation as cacheSelectionLocations,
// without scanning the candidate through a path that could be retargeted.
func selectedLocalLocationCandidate(cfg Config, repo, locationID string) *cacheSelectionLocation {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	for _, root := range localCacheRoots(cfg.cacheRoot(), cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes) {
		if root.skipsOwner(parts[0]) {
			continue
		}
		id := "local-" + selectionHash(filepath.Clean(root.Path), repo, "model")
		if id == locationID {
			return &cacheSelectionLocation{
				ID: id, Source: root.Source,
				Path:      filepath.Join(root.Path, parts[0], parts[1]),
				CanDelete: isRegisteredLocalSelectionRoot(cfg, root.Path), kind: "local",
			}
		}
	}
	return nil
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
	var localRoot *os.Root
	var localPhysicalRepo string
	var localRelease func()
	defer func() {
		if localRoot != nil {
			_ = localRoot.Close()
		}
		if localRelease != nil {
			localRelease()
		}
	}()
	if strings.HasPrefix(req.LocationID, "local-") {
		candidate := selectedLocalLocationCandidate(cfg, req.Repo, req.LocationID)
		if candidate == nil || !candidate.CanDelete {
			writeError(w, http.StatusConflict, "Location is not eligible for local deletion", "This location is not a currently configured local scan or download root")
			return
		}
		var err error
		localRoot, localPhysicalRepo, err = openSelectedLocalRepo(candidate.Path, s.selectedDeleteHooks.beforeRootOpen)
		if err != nil {
			writeError(w, http.StatusConflict, "Selection is unsafe", err.Error())
			return
		}
		if s.selectedDeleteHooks.afterRootOpen != nil {
			s.selectedDeleteHooks.afterRootOpen()
		}
		opened, openedErr := localRoot.Stat(".")
		bound, boundErr := os.Stat(candidate.Path)
		if openedErr != nil || boundErr != nil || !os.SameFile(opened, bound) {
			writeError(w, http.StatusConflict, "Selection is stale", "The configured location changed while deletion was being admitted")
			return
		}
	}
	var selected *cacheSelectionLocation
	if localRoot != nil {
		// The normal location scanner is intentionally unchanged for GET and HF
		// deletion, but its filepath.Walk opens directories by path. Re-enumerate
		// this local selection only through the already admitted rooted walks below.
		selected = selectedLocalLocationCandidate(cfg, req.Repo, req.LocationID)
	} else {
		for _, loc := range cacheSelectionLocations(cfg, req.Repo) {
			if loc.ID == req.LocationID {
				copy := loc
				selected = &copy
				break
			}
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
	if localRoot == nil {
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
	}
	if localRoot != nil {
		opened, openedErr := localRoot.Stat(".")
		bound, boundErr := os.Stat(selected.Path)
		if openedErr != nil || boundErr != nil || !os.SameFile(opened, bound) {
			writeError(w, http.StatusConflict, "Selection is stale", "The configured location changed while deletion was being admitted")
			return
		}
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
	if localRoot == nil {
		writeError(w, http.StatusConflict, "Selection is stale", "The configured local deletion scope could not be opened")
		return
	}
	root, physicalRepo := localRoot, localPhysicalRepo
	openedInfo, openedErr := root.Stat(".")
	boundInfo, boundErr := os.Stat(selected.Path)
	if openedErr != nil || boundErr != nil || !os.SameFile(openedInfo, boundInfo) {
		writeError(w, http.StatusConflict, "Selection is stale", "The configured location changed while deletion was being admitted")
		return
	}
	// Derive the source root from the admitted repository, not by resolving the
	// configured source alias again: it may have been retargeted after admission.
	admittedSourceBase := filepath.Dir(filepath.Dir(physicalRepo))
	excluded, boundariesComplete := selectedDeleteExclusions(cfg, selected, physicalRepo, admittedSourceBase)
	if !boundariesComplete {
		writeError(w, http.StatusConflict, "Location could not be verified", "A configured descendant boundary could not be safely resolved")
		return
	}
	// Establish the current group from the admitted rooted repository before
	// request paths can influence reservation or identity-pin side effects.
	files, scanWarning := localSelectionFilesRoot(root, physicalRepo, excluded, nil, nil, nil)
	if scanWarning != "" {
		writeError(w, http.StatusConflict, "Location could not be verified", scanWarning)
		return
	}
	freshGroups := makeSelectionGroups(selected.ID, files)
	var fresh *cacheSelectionGroup
	for i := range freshGroups {
		if freshGroups[i].ID == req.GroupID {
			fresh = &freshGroups[i]
			break
		}
	}
	if fresh == nil || !selectionCompositionEqual(req.Members, fresh.Members) {
		writeError(w, http.StatusConflict, "Selection is stale", "The selected GGUF group or its exact members changed; refresh before deleting")
		return
	}
	group = fresh
	for _, member := range group.Members {
		if !safeSelectedRelative(member.Path) {
			writeError(w, http.StatusConflict, "Selection is unsafe", "A selected path is not a safe relative GGUF entry")
			return
		}
	}
	physicalPaths := make([]string, 0, len(group.Members))
	for _, member := range group.Members {
		physicalPaths = append(physicalPaths, filepath.Join(physicalRepo, filepath.FromSlash(member.Path)))
	}
	var ok bool
	localRelease, ok = s.jobs.reserveSelectedGGUF(physicalPaths)
	if !ok {
		localRelease = nil
		writeError(w, http.StatusConflict, "Selected GGUF is busy", "A queued or active operation may write this exact GGUF group; cancel the conflicting job and retry")
		return
	}
	if s.selectedDeleteHooks.afterReservation != nil {
		s.selectedDeleteHooks.afterReservation()
	}
	// Recheck the configured binding at reservation admission. Subsequent
	// enumeration and unlink remain anchored to the opened physical repository.
	openedInfo, openedErr = root.Stat(".")
	boundInfo, boundErr = os.Stat(selected.Path)
	if openedErr != nil || boundErr != nil || !os.SameFile(openedInfo, boundInfo) {
		writeError(w, http.StatusConflict, "Selection is stale", "The configured location changed while deletion was being admitted")
		return
	}
	// Revalidate the complete current group after acquiring the reservation and
	// before pinning any selected entry. Pin creation changes filesystem metadata
	// even when the later identity check refuses deletion.
	files, scanWarning = localSelectionFilesRoot(root, physicalRepo, excluded, nil, nil, nil)
	if scanWarning != "" {
		writeError(w, http.StatusConflict, "Location could not be verified", scanWarning)
		return
	}
	freshGroups = makeSelectionGroups(selected.ID, files)
	fresh = nil
	for i := range freshGroups {
		if freshGroups[i].ID == req.GroupID {
			fresh = &freshGroups[i]
			break
		}
	}
	if fresh == nil || !selectionCompositionEqual(group.Members, fresh.Members) || !selectionCompositionEqual(req.Members, fresh.Members) {
		writeError(w, http.StatusConflict, "Selection is stale", "The selected GGUF group changed while deletion was being reserved; refresh before deleting")
		return
	}
	group = fresh
	if s.selectedDeleteHooks.afterGroupValidation != nil {
		s.selectedDeleteHooks.afterGroupValidation()
	}
	pinPaths := make(map[string]cacheSelectionMember, len(group.Members))
	for _, member := range group.Members {
		pinPaths[filepath.ToSlash(filepath.FromSlash(member.Path))] = member
	}
	pins := newSelectedEntryPinSet(root, s.selectedDeleteHooks, pinPaths)
	files, scanWarning = localSelectionFilesRoot(root, physicalRepo, excluded, pinPaths, s.selectedDeleteHooks.beforeIdentityPin, pins)
	if scanWarning != "" {
		writeError(w, http.StatusConflict, "Location could not be verified", withPinCleanupDetails(scanWarning, pins.cleanup()))
		return
	}
	if s.selectedDeleteHooks.afterFinalScan != nil {
		s.selectedDeleteHooks.afterFinalScan()
	}
	freshGroups = makeSelectionGroups(selected.ID, files)
	fresh = nil
	for i := range freshGroups {
		if freshGroups[i].ID == req.GroupID {
			fresh = &freshGroups[i]
			break
		}
	}
	if fresh == nil || !selectionCompositionEqual(group.Members, fresh.Members) {
		details := "The selected GGUF group changed while deletion was being reserved; refresh before deleting"
		writeError(w, http.StatusConflict, "Selection is stale", withPinCleanupDetails(details, pins.cleanup()))
		return
	}
	group = fresh
	rels := make([]string, 0, len(group.Members))
	for _, member := range group.Members {
		rel := filepath.FromSlash(member.Path)
		for dir := filepath.Dir(rel); dir != "."; dir = filepath.Dir(dir) {
			parentInfo, parentErr := root.Lstat(dir)
			if parentErr != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
				details := "A selected file has an unsafe parent component"
				writeError(w, http.StatusConflict, "Selection is unsafe", withPinCleanupDetails(details, pins.cleanup()))
				return
			}
		}
		info, statErr := root.Lstat(rel)
		if statErr != nil || member.identity == nil || !os.SameFile(member.identity.info, info) || info.Size() != member.identity.info.Size() || info.IsDir() || (member.LinkOnly && info.Mode()&os.ModeSymlink == 0) || (!member.LinkOnly && !info.Mode().IsRegular()) {
			details := "A selected GGUF entry changed during preflight"
			writeError(w, http.StatusConflict, "Selection is stale", withPinCleanupDetails(details, pins.cleanup()))
			return
		}
		rels = append(rels, rel)
	}

	result := selectedDeleteResponse{OK: true, Repo: req.Repo, GroupID: group.ID, Removed: []string{}, Remaining: []string{}}
	for i, member := range group.Members {
		if s.selectedDeleteHooks.beforeRemove != nil {
			s.selectedDeleteHooks.beforeRemove(i)
		}
		parent := filepath.ToSlash(filepath.Dir(rels[i]))
		parentHandle, parentErr := pins.hardLinkParent(parent)
		if parentErr == nil {
			parentErr = pins.verifyHardLinkParentBinding(parentHandle)
		}
		name := filepath.Base(rels[i])
		var info os.FileInfo
		var statErr error
		if parentErr != nil {
			statErr = parentErr
		} else {
			info, statErr = parentHandle.parentRoot.Lstat(name)
		}
		if statErr != nil || member.identity == nil || !os.SameFile(member.identity.info, info) || info.Size() != member.identity.info.Size() || (member.LinkOnly && info.Mode()&os.ModeSymlink == 0) || (!member.LinkOnly && !info.Mode().IsRegular()) {
			if len(result.Removed) == 0 {
				details := "A selected GGUF entry changed immediately before deletion"
				writeError(w, http.StatusConflict, "Selection is stale", withPinCleanupDetails(details, pins.cleanup()))
				return
			}
			result.OK = false
			result.Remaining = append(result.Remaining, member.Path)
			result.Errors = append(result.Errors, member.Path+": selected entry changed immediately before deletion")
			for _, remaining := range group.Members[i+1:] {
				result.Remaining = append(result.Remaining, remaining.Path)
			}
			break
		}
		// Unlink through the retained handle for the selected parent directory.
		// Rechecking a same-inode leaf through root plus a multi-component path
		// is insufficient: a swapped parent symlink can redirect Remove to a
		// different hardlink name. The directory handle preserves the admitted
		// parent namespace for the actual unlink.
		remove := parentHandle.parentRoot.Remove
		if s.selectedDeleteHooks.removeEntry != nil {
			remove = func(name string) error { return s.selectedDeleteHooks.removeEntry(parentHandle.parentRoot, name) }
		}
		if err := remove(name); err != nil {
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
	cleanupErrors := pins.cleanup()
	if len(cleanupErrors) > 0 {
		result.OK = false
		result.Errors = append(result.Errors, cleanupErrors...)
		if len(result.Removed) == 0 {
			writeError(w, http.StatusConflict, "Selected GGUF deletion was refused", strings.Join(result.Errors, "; "))
			return
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
	if len(cleanupErrors) > 0 {
		result.Message = "Selected deletion was partial; identity-pin cleanup issues and any remaining entries are listed in errors"
	} else {
		result.Message = "Some selected entries could not be removed; other files and directories were left unchanged"
	}
	writeJSON(w, http.StatusMultiStatus, result)
}

var errSelectionWriterBusy = errors.New("selected GGUF is reserved for deletion")
