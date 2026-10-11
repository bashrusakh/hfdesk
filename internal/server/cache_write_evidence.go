// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
)

// writeDecisionJob is the manager-owned portion of a reservation decision.
// It intentionally excludes progress, listeners and other presentation data.
type writeDecisionJob struct {
	id, repo, revision, output, hub, local, localRepo string
	status                                            JobStatus
	starting, flat, dataset, exact                    bool
	filters, excludes                                 []string
	generation                                        int
	createdAt                                         int64
	activityID                                        uint64
}

type writeDecisionActivity struct {
	id        uint64
	job       writeDecisionJob
	planKnown bool
	planned   []string
}

type writeDecisionSnapshot struct {
	epoch                              uint64
	fenced, stopping                   bool
	maxActive                          int
	jobs                               []writeDecisionJob
	activities                         []writeDecisionActivity
	deletes, hfReservations, mutations map[uint64][]frozenMutationPath
	mutationKinds                      map[uint64]writeScopeKind
}

type writeIdentityProbe struct {
	path, identity, linkTarget string
	directory                  bool
	mode                       os.FileMode
}

type writeScopeKind uint8

const (
	writeScopeDirectory writeScopeKind = iota
	writeScopeEntry
)

func decisionJob(j *Job) writeDecisionJob {
	return writeDecisionJob{id: j.ID, repo: j.Repo, revision: j.Revision, output: j.OutputDir,
		hub: j.HubDir, local: j.LocalDir, localRepo: j.LocalRepo, status: j.Status,
		starting: j.starting, flat: j.Flat, dataset: j.IsDataset, exact: j.ExactMatch,
		filters: append([]string(nil), j.Filters...), excludes: append([]string(nil), j.Excludes...),
		generation: j.generation, createdAt: j.CreatedAt.UnixNano()}
}

func cloneScopes[T any](in map[uint64][]T) map[uint64][]T {
	out := make(map[uint64][]T, len(in))
	for id, paths := range in {
		out[id] = append([]T(nil), paths...)
	}
	return out
}

func cloneScopeKinds(in map[uint64]writeScopeKind) map[uint64]writeScopeKind {
	out := make(map[uint64]writeScopeKind, len(in))
	for id, kind := range in {
		out[id] = kind
	}
	return out
}

func (m *JobManager) captureWriteDecisionLocked() writeDecisionSnapshot {
	s := writeDecisionSnapshot{epoch: m.writeDecisionEpoch, fenced: m.fenced, stopping: m.stopping,
		maxActive: m.effectiveMaxActive(), deletes: cloneScopes(m.deleteReservations),
		hfReservations: cloneScopes(m.hfRepoReservations), mutations: cloneScopes(m.mutationScopes), mutationKinds: cloneScopeKinds(m.mutationScopeKinds)}
	for _, j := range m.jobs {
		s.jobs = append(s.jobs, decisionJob(j))
	}
	for id, a := range m.runActivities {
		descriptor := a.descriptor
		if descriptor.id == "" {
			descriptor = decisionJob(a.job)
		}
		item := writeDecisionActivity{id: id, job: descriptor, planKnown: a.planKnown}
		if a.planKnown {
			item.planned = mapKeys(a.planned)
			sort.Strings(item.planned)
		}
		s.activities = append(s.activities, item)
	}
	sort.Slice(s.jobs, func(i, j int) bool { return s.jobs[i].id < s.jobs[j].id })
	sort.Slice(s.activities, func(i, j int) bool { return s.activities[i].id < s.activities[j].id })
	return s
}

func (m *JobManager) captureWriteDecision() writeDecisionSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.captureWriteDecisionLocked()
}

func (m *JobManager) writeDecisionChangeChannel() <-chan struct{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.writeDecisionChanged
}

func (m *JobManager) writeDecisionUnchanged(s writeDecisionSnapshot) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return reflect.DeepEqual(s, m.captureWriteDecisionLocked())
}

func (m *JobManager) signalWriteDecisionLocked() {
	if m.writeDecisionChanged == nil {
		m.writeDecisionChanged = make(chan struct{})
		return
	}
	close(m.writeDecisionChanged)
	m.writeDecisionChanged = make(chan struct{})
}

func (m *JobManager) bumpWriteDecisionLocked() {
	m.writeDecisionEpoch++
	m.signalWriteDecisionLocked()
}

func (m *JobManager) beginObservedMutation(paths []string) func() {
	clean := make([]frozenMutationPath, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		frozen, err := freezeMutationEntry(p)
		if err != nil {
			// The mutation still happens; the epoch bump below keeps decision
			// waiters from treating their pre-mutation snapshot as current.
			continue
		}
		frozen.raw = p
		clean = append(clean, frozen)
	}
	m.mu.Lock()
	if m.mutationScopes == nil {
		m.mutationScopes = make(map[uint64][]frozenMutationPath)
	}
	if m.mutationScopeKinds == nil {
		m.mutationScopeKinds = make(map[uint64]writeScopeKind)
	}
	m.nextWriteID++
	id := m.nextWriteID
	m.mutationScopes[id] = clean
	m.mutationScopeKinds[id] = writeScopeEntry
	m.bumpWriteDecisionLocked()
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.mutationScopes, id)
		delete(m.mutationScopeKinds, id)
		m.bumpWriteDecisionLocked()
		m.mu.Unlock()
	}
}

// cacheWriteAttempt owns an immutable inventory assembled outside manager
// locks. Begin under m.mu so Close cannot race WaitGroup.Add.
type cacheWriteAttempt struct {
	manager     *JobManager
	start       writeDecisionSnapshot
	evidence    map[string]selectedWriteEvidence
	failed      map[string]bool
	descriptors map[string]writeDecisionJob
	probes      map[string]writeIdentityProbe
	targets     map[string]writeIdentityProbe
	inventory   func(string) (writableNamespaceInventory, bool)
}

func (m *JobManager) beginCacheWriteAttempt() (*cacheWriteAttempt, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return nil, false
	}
	m.opWG.Add(1)
	return &cacheWriteAttempt{manager: m, start: m.captureWriteDecisionLocked(), evidence: make(map[string]selectedWriteEvidence), failed: make(map[string]bool), descriptors: make(map[string]writeDecisionJob), probes: make(map[string]writeIdentityProbe), targets: make(map[string]writeIdentityProbe), inventory: inventoryWritableNamespace}, true
}

func (a *cacheWriteAttempt) Close() {
	if a != nil {
		a.manager.opWG.Done()
	}
}

func decisionJobKey(j *Job) string {
	return decisionDescriptorKey(decisionJob(j))
}

func decisionDescriptorKey(d writeDecisionJob) string {
	b, _ := json.Marshal(struct {
		ID, Repo, Revision, Output, Hub, Local, LocalRepo string
		Filters, Excludes                                 []string
		Generation                                        int
		Exact, Flat, Dataset                              bool
		ActivityID                                        uint64
	}{d.id, d.repo, d.revision, d.output, d.hub, d.local, d.localRepo, d.filters, d.excludes, d.generation, d.exact, d.flat, d.dataset, d.activityID})
	return string(b)
}

func descriptorForStartingJob(job, activity writeDecisionJob) bool {
	job.generation = activity.generation
	job.activityID = activity.activityID
	return decisionDescriptorKey(job) == decisionDescriptorKey(activity)
}

func jobFromDecision(d writeDecisionJob) *Job {
	return &Job{ID: d.id, Repo: d.repo, Revision: d.revision, OutputDir: d.output, HubDir: d.hub,
		LocalDir: d.local, LocalRepo: d.localRepo, Status: d.status, starting: d.starting,
		Flat: d.flat, IsDataset: d.dataset, ExactMatch: d.exact, Filters: append([]string(nil), d.filters...),
		Excludes: append([]string(nil), d.excludes...), generation: d.generation}
}

// CollectSnapshot inventories all job and run-activity descriptors in the
// immutable decision snapshot. A descriptor shared by a queued job and its
// activity is visited once; a distinct generation remains independent.
func (a *cacheWriteAttempt) CollectSnapshot(s writeDecisionSnapshot) {
	seen := make(map[string]struct{}, len(s.jobs)+len(s.activities))
	startingActivity := make(map[string][]writeDecisionJob)
	for _, activity := range s.activities {
		if !activity.planKnown {
			startingActivity[activity.job.id] = append(startingActivity[activity.job.id], activity.job)
		}
	}
	for _, d := range s.jobs {
		if d.status != JobStatusQueued && !d.starting {
			continue
		}
		if d.starting {
			for _, activity := range startingActivity[d.id] {
				if descriptorForStartingJob(d, activity) {
					d = activity
					break
				}
			}
		}
		key := decisionDescriptorKey(d)
		seen[key] = struct{}{}
		a.collectDescriptor(d)
	}
	for _, activity := range s.activities {
		if activity.planKnown {
			continue
		}
		key := decisionDescriptorKey(activity.job)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		a.collectDescriptor(activity.job)
	}
}

// Collect builds each semantic job's writable namespace once, independent
// of target count. It is safe to call only on the attempt-local job copies.
func (a *cacheWriteAttempt) Collect(job *Job) {
	if a == nil || job == nil {
		return
	}
	a.collectDescriptor(decisionJob(job))
}

func (a *cacheWriteAttempt) collectDescriptor(descriptor writeDecisionJob) {
	job := jobFromDecision(descriptor)
	key := decisionDescriptorKey(descriptor)
	if a.failed[key] {
		return
	}
	if _, ok := a.evidence[key]; ok {
		return
	}
	a.descriptors[key] = descriptor
	inventory := a.inventory
	if inventory == nil {
		inventory = inventoryWritableNamespace
	}
	bases := jobWritableBases(job)
	baseProbes := make([]writeIdentityProbe, len(bases))
	for i, base := range bases {
		probe, err := captureWriteIdentityProbe(base, true)
		if err != nil {
			a.failed[key] = true
			return
		}
		baseProbes[i] = probe
	}
	ev, ok := buildSelectedWriteEvidence(job, inventory)
	if !ok {
		a.failed[key] = true
		return
	}
	a.evidence[key] = ev
	for i, base := range bases {
		probe, err := captureWriteIdentityProbe(base, true)
		if err != nil || !reflect.DeepEqual(probe, baseProbes[i]) || i >= len(ev.inventories) || probe.identity != ev.inventories[i].base {
			a.failed[key] = true
			delete(a.evidence, key)
			return
		}
		a.probes[probe.path] = probe
	}
	for _, inv := range ev.inventories {
		for _, alias := range inv.aliases {
			probe, err := captureWriteIdentityProbe(alias.location, true)
			if err != nil || probe.identity != pathIdentityKey(alias.dest) {
				a.failed[key] = true
				delete(a.evidence, key)
				return
			}
			a.probes[probe.path] = probe
		}
	}
}

func (a *cacheWriteAttempt) MayWrite(job *Job, targets []string) bool {
	if a == nil {
		return true
	}
	base := decisionJob(job)
	base.activityID = 0
	if base.starting {
		base.generation++
	}
	baseKey := decisionDescriptorKey(base)
	identities := make([]string, len(targets))
	for i, p := range targets {
		probe, err := captureWriteIdentityProbe(p, false)
		if err != nil {
			return true
		}
		identities[i] = probe.identity
		a.targets[filepath.Clean(p)] = probe
	}
	found := false
	for key, descriptor := range a.descriptors {
		candidate := descriptor
		candidate.activityID = 0
		if decisionDescriptorKey(candidate) != baseKey {
			continue
		}
		found = true
		if a.failed[key] {
			return true
		}
		ev, ok := a.evidence[key]
		if !ok || selectedWriteEvidenceMayWrite(ev, identities) {
			return true
		}
	}
	return !found
}

// Revalidate probes only the explicit bounded identities supplied by the
// caller. It is not a guarantee against unmanaged external filesystem writes.
func (a *cacheWriteAttempt) Revalidate(paths []string) bool {
	if a == nil || !a.manager.writeDecisionUnchanged(a.start) {
		return false
	}
	for _, probe := range a.probes {
		current, err := captureWriteIdentityProbe(probe.path, probe.directory)
		if err != nil || !reflect.DeepEqual(probe, current) {
			return false
		}
	}
	for _, p := range paths {
		probe, ok := a.targets[filepath.Clean(p)]
		if !ok {
			return false
		}
		current, err := captureWriteIdentityProbe(p, probe.directory)
		if err != nil || !reflect.DeepEqual(probe, current) {
			return false
		}
	}
	return true
}

func (a *cacheWriteAttempt) HasActiveBroadMutation(targets []string) bool {
	if a == nil {
		return true
	}
	// beginCacheMutation and HF reservations name directory scopes; selected
	// GGUF deletes name leaf entries whose final symlink must remain unfollowed.
	// The frozen identity kind is encoded in each scope key, so entry and
	// directory scopes compare correctly without re-deriving it here.
	for id, scopes := range a.start.mutations {
		_ = id
		for _, scope := range scopes {
			for _, target := range targets {
				if frozenMutationOverlapsRawEntry(scope, target) {
					return true
				}
			}
			for _, ev := range a.evidence {
				for _, inv := range ev.inventories {
					if frozenMutationOverlapsRawDirectory(scope, inv.base) {
						return true
					}
					for _, alias := range inv.aliases {
						if frozenMutationOverlapsRawDirectory(scope, alias.location) || frozenMutationOverlapsRawDirectory(scope, alias.dest) {
							return true
						}
					}
				}
			}
		}
	}
	for _, scopes := range a.start.hfReservations {
		for _, scope := range scopes {
			for _, target := range targets {
				if frozenMutationOverlapsRawEntry(scope, target) {
					return true
				}
			}
			for _, ev := range a.evidence {
				for _, inv := range ev.inventories {
					if frozenMutationOverlapsRawDirectory(scope, inv.base) {
						return true
					}
					for _, alias := range inv.aliases {
						if frozenMutationOverlapsRawDirectory(scope, alias.location) || frozenMutationOverlapsRawDirectory(scope, alias.dest) {
							return true
						}
					}
				}
			}
		}
	}
	for _, scopes := range a.start.deletes {
		for _, entry := range scopes {
			for _, target := range targets {
				if frozenMutationOverlapsRawEntry(entry, target) {
					return true
				}
			}
			for _, ev := range a.evidence {
				for _, inv := range ev.inventories {
					if frozenMutationOverlapsRawDirectory(entry, inv.base) {
						return true
					}
					for _, alias := range inv.aliases {
						if frozenMutationOverlapsRawDirectory(entry, alias.location) || frozenMutationOverlapsRawDirectory(entry, alias.dest) {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

func captureWriteIdentityProbe(path string, directory bool) (writeIdentityProbe, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return writeIdentityProbe{}, err
	}
	var identity string
	if directory {
		identity, err = mutationDirectoryIdentity(abs)
	} else {
		identity, err = mutationEntryIdentity(abs)
	}
	if err != nil {
		return writeIdentityProbe{}, err
	}
	p := writeIdentityProbe{path: abs, identity: identity, directory: directory}
	info, lerr := os.Lstat(abs)
	if lerr == nil {
		p.mode = info.Mode()
		if info.Mode()&os.ModeSymlink != 0 {
			p.linkTarget, err = os.Readlink(abs)
			if err != nil {
				return writeIdentityProbe{}, err
			}
		}
	} else if !os.IsNotExist(lerr) {
		return writeIdentityProbe{}, lerr
	}
	return p, nil
}

func jobWritableBases(job *Job) []string {
	base := jobDestinationBase(job)
	if base == "" {
		return nil
	}
	bases := []string{base}
	if snapshot := jobSnapshotBase(job); snapshot != "" && snapshot != base {
		bases = append(bases, snapshot)
	}
	return bases
}
