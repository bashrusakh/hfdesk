// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
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

func jobMayWriteSelectedPath(job *Job, target string) bool {
	base := jobDestinationBase(job)
	if base == "" {
		return false
	}
	targetIdentity, targetErr := mutationEntryIdentity(target)
	if targetErr != nil {
		return true
	}
	if pathWithinWriterBase(base, targetIdentity) {
		// Canonical directory names prove physical overlap, not the raw remote path
		// spelling used by PlanRepo's path-based excludes. The basename is invariant
		// across parent aliases, so it is safe for a negative filter decision; a
		// directory-only exclude may conservatively classify this as a writer.
		return hfdownloader.GGUFPathSelected(filepath.Base(targetIdentity), job.Filters, job.Excludes, job.ExactMatch)
	}
	if snapshotBase := jobSnapshotBase(job); snapshotBase != "" && pathWithinWriterBase(snapshotBase, targetIdentity) {
		// Until the plan completes, its resolved commit and remote paths are
		// unknown. Keep the frozen repository snapshots subtree reserved, but use
		// only the basename for sound negative GGUF/filter decisions.
		return hfdownloader.GGUFPathSelected(filepath.Base(targetIdentity), job.Filters, job.Excludes, job.ExactMatch)
	}
	return false
}

func jobMayWriteFrozenSelectedPath(job *Job, target string) bool {
	base := jobDestinationBase(job)
	if base == "" {
		return false
	}
	target, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return true
	}
	if pathWithinWriterBase(base, target) {
		return hfdownloader.GGUFPathSelected(filepath.Base(target), job.Filters, job.Excludes, job.ExactMatch)
	}
	if snapshotBase := jobSnapshotBase(job); snapshotBase != "" && pathWithinWriterBase(snapshotBase, target) {
		return hfdownloader.GGUFPathSelected(filepath.Base(target), job.Filters, job.Excludes, job.ExactMatch)
	}
	return false
}

func pathWithinWriterBase(base, targetIdentity string) bool {
	baseIdentity, err := mutationDirectoryIdentity(base)
	if err != nil {
		return true
	}
	rel, err := filepath.Rel(baseIdentity, targetIdentity)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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

// physicalEntryOverlap treats stored as an already-frozen physical entry key
// and resolves only the newly arriving path. It must not re-resolve stored
// through a lexical alias after admission.
func physicalEntryOverlap(stored, incoming string) bool {
	stored, err := filepath.Abs(filepath.Clean(stored))
	if err != nil {
		return true
	}
	incoming, err = mutationEntryIdentity(incoming)
	if err != nil {
		return true
	}
	return stored == incoming || withinLocalRoot(stored, incoming) || withinLocalRoot(incoming, stored)
}

func plannedEntriesConflict(frozenEntries, plannedPaths []string) bool {
	for _, frozen := range frozenEntries {
		for _, planned := range plannedPaths {
			if physicalEntryOverlap(frozen, planned) {
				return true
			}
		}
	}
	return false
}

func directoryOverlapsFrozenEntry(scope, frozenEntry string) bool {
	dir, err := mutationDirectoryIdentity(scope)
	if err != nil {
		return true
	}
	entry, err := filepath.Abs(filepath.Clean(frozenEntry))
	if err != nil {
		return true
	}
	return withinLocalRoot(dir, entry) || withinLocalRoot(entry, dir)
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
		// Freeze the selected entry's physical parent at admission. The final
		// basename is intentionally retained as an entry (not followed when it
		// is a symlink), so later root-alias changes cannot move this reservation.
		identity, err := mutationEntryIdentity(abs)
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
	entryConflicts := func(scopes []string) bool {
		for _, a := range scopes {
			for _, b := range clean {
				if physicalEntryOverlap(a, b) {
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
				if directoryOverlapsFrozenEntry(scope, entry) {
					m.mu.Unlock()
					return nil, false
				}
			}
		}
	}
	for _, scopes := range m.mutationScopes {
		for _, scope := range scopes {
			for _, entry := range clean {
				if directoryOverlapsFrozenEntry(scope, entry) {
					m.mu.Unlock()
					return nil, false
				}
			}
		}
	}
	for _, activity := range m.runActivities {
		if activity.planKnown {
			if plannedEntriesConflict(clean, mapKeys(activity.planned)) {
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
				if directoryOverlapsFrozenEntry(write, scope) {
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

// selectedDeleteHooks are per-server synchronization points for deterministic
// endpoint tests. Production servers leave them nil.
type selectedDeleteHooks struct {
	beforeRootOpen func()
	afterRootOpen  func()
	afterFinalScan func()
	beforeRemove   func(int)
	removeEntry    func(*os.Root, string) error
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
	if beforeOpen != nil {
		beforeOpen()
	}
	root, err := os.OpenRoot(base)
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
		next, openErr := root.OpenRoot(component)
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
func pinSelectedEntry(root *os.Root, physicalRepo, name string, observed os.FileInfo, linkOnly bool) (*selectedEntryIdentity, error) {
	var pin *os.File
	var err error
	if linkOnly {
		pin, err = openSelectedLinkEntry(physicalRepo, name)
	} else {
		pin, err = openSelectedRegularEntry(root, physicalRepo, name)
	}
	if err != nil {
		return nil, err
	}
	info, err := pin.Stat()
	if err != nil || !os.SameFile(observed, info) {
		_ = pin.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("selected entry changed while its identity was being pinned")
	}
	return &selectedEntryIdentity{info: info, pin: pin}, nil
}

func closeSelectedEntryIdentities(files []cacheSelectionFile) {
	for i := range files {
		files[i].identity.close()
	}
}

func localSelectionFilesRoot(root *os.Root, physicalRepo string, excluded []string, pinPaths map[string]bool) ([]cacheSelectionFile, string) {
	var files []cacheSelectionFile
	var warning string
	excludedSet := make(map[string]bool, len(excluded))
	for _, name := range excluded {
		excludedSet[filepath.ToSlash(filepath.Clean(name))] = true
	}
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if excludedSet[filepath.ToSlash(filepath.Clean(name))] {
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
		if pinPaths[relative] {
			identity, err = pinSelectedEntry(root, physicalRepo, relative, info, info.Mode()&os.ModeSymlink != 0)
			if err != nil {
				return err
			}
		}
		size, linkOnly := info.Size(), info.Mode()&os.ModeSymlink != 0
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
					identity.close()
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
				identity.close()
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
func selectedDeleteExclusions(cfg Config, selected *cacheSelectionLocation, physicalRepo string) ([]string, bool) {
	roots := localCacheRoots(cfg.cacheRoot(), cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes)
	selectedLexical, err := filepath.Abs(filepath.Clean(selected.Path))
	if err != nil {
		return nil, false
	}
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
		if pathIdentityKey(candidateLexical) == pathIdentityKey(selectedLexical) {
			continue
		}
		if withinLocalRoot(selectedLexical, candidateLexical) {
			rel, relErr := filepath.Rel(selectedLexical, candidateLexical)
			if relErr != nil || !add(rel) {
				return nil, false
			}
		}
		_, candidatePhysical, ok := configuredRootIdentity(candidate.Path)
		if !ok {
			return nil, false
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
	for _, member := range group.Members {
		if !safeSelectedRelative(member.Path) {
			writeError(w, http.StatusConflict, "Selection is unsafe", "A selected path is not a safe relative GGUF entry")
			return
		}
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
	// Recheck the configured binding at reservation admission. Subsequent
	// enumeration and unlink remain anchored to the opened physical repository.
	openedInfo, openedErr = root.Stat(".")
	boundInfo, boundErr = os.Stat(selected.Path)
	if openedErr != nil || boundErr != nil || !os.SameFile(openedInfo, boundInfo) {
		writeError(w, http.StatusConflict, "Selection is stale", "The configured location changed while deletion was being admitted")
		return
	}
	excluded, boundariesComplete := selectedDeleteExclusions(cfg, selected, physicalRepo)
	if !boundariesComplete {
		writeError(w, http.StatusConflict, "Location could not be verified", "A configured descendant boundary could not be safely resolved")
		return
	}
	pinPaths := make(map[string]bool, len(group.Members))
	for _, member := range group.Members {
		pinPaths[filepath.ToSlash(filepath.FromSlash(member.Path))] = true
	}
	files, scanWarning := localSelectionFilesRoot(root, physicalRepo, excluded, pinPaths)
	defer closeSelectedEntryIdentities(files)
	if scanWarning != "" {
		writeError(w, http.StatusConflict, "Location could not be verified", scanWarning)
		return
	}
	if s.selectedDeleteHooks.afterFinalScan != nil {
		s.selectedDeleteHooks.afterFinalScan()
	}
	freshGroups := makeSelectionGroups(selected.ID, files)
	var fresh *cacheSelectionGroup
	for i := range freshGroups {
		if freshGroups[i].ID == req.GroupID {
			fresh = &freshGroups[i]
			break
		}
	}
	if fresh == nil || !selectionCompositionEqual(group.Members, fresh.Members) {
		writeError(w, http.StatusConflict, "Selection is stale", "The selected GGUF group changed while deletion was being reserved; refresh before deleting")
		return
	}
	group = fresh
	rels := make([]string, 0, len(group.Members))
	for _, member := range group.Members {
		rel := filepath.FromSlash(member.Path)
		for dir := filepath.Dir(rel); dir != "."; dir = filepath.Dir(dir) {
			parentInfo, parentErr := root.Lstat(dir)
			if parentErr != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
				writeError(w, http.StatusConflict, "Selection is unsafe", "A selected file has an unsafe parent component")
				return
			}
		}
		info, statErr := root.Lstat(rel)
		if statErr != nil || member.identity == nil || !os.SameFile(member.identity.info, info) || info.Size() != member.identity.info.Size() || info.IsDir() || (member.LinkOnly && info.Mode()&os.ModeSymlink == 0) || (!member.LinkOnly && !info.Mode().IsRegular()) {
			writeError(w, http.StatusConflict, "Selection is stale", "A selected GGUF entry changed during preflight")
			return
		}
		rels = append(rels, rel)
	}

	result := selectedDeleteResponse{OK: true, Repo: req.Repo, GroupID: group.ID, Removed: []string{}, Remaining: []string{}}
	for i, member := range group.Members {
		if s.selectedDeleteHooks.beforeRemove != nil {
			s.selectedDeleteHooks.beforeRemove(i)
		}
		info, statErr := root.Lstat(rels[i])
		if statErr != nil || member.identity == nil || !os.SameFile(member.identity.info, info) || info.Size() != member.identity.info.Size() || (member.LinkOnly && info.Mode()&os.ModeSymlink == 0) || (!member.LinkOnly && !info.Mode().IsRegular()) {
			if len(result.Removed) == 0 {
				writeError(w, http.StatusConflict, "Selection is stale", "A selected GGUF entry changed immediately before deletion")
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
		remove := root.Remove
		if s.selectedDeleteHooks.removeEntry != nil {
			remove = func(name string) error { return s.selectedDeleteHooks.removeEntry(root, name) }
		}
		if err := remove(rels[i]); err != nil {
			result.OK = false
			result.Remaining = append(result.Remaining, member.Path)
			result.Errors = append(result.Errors, member.Path+": "+err.Error())
			for _, remaining := range group.Members[i+1:] {
				result.Remaining = append(result.Remaining, remaining.Path)
			}
			break
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
