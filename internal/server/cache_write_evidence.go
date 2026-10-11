// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
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
}

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

func (m *JobManager) captureWriteDecisionLocked() writeDecisionSnapshot {
	s := writeDecisionSnapshot{epoch: m.writeDecisionEpoch, fenced: m.fenced, stopping: m.stopping,
		maxActive: m.effectiveMaxActive(), deletes: cloneScopes(m.deleteReservations),
		hfReservations: cloneScopes(m.hfRepoReservations), mutations: cloneScopes(m.mutationScopes)}
	for _, j := range m.jobs {
		s.jobs = append(s.jobs, decisionJob(j))
	}
	for id, a := range m.runActivities {
		item := writeDecisionActivity{id: id, job: decisionJob(a.job), planKnown: a.planKnown}
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
		frozen, err := freezeMutationDirectory(p)
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
	m.nextWriteID++
	id := m.nextWriteID
	m.mutationScopes[id] = clean
	m.bumpWriteDecisionLocked()
	m.mu.Unlock()
	return func() { m.mu.Lock(); delete(m.mutationScopes, id); m.bumpWriteDecisionLocked(); m.mu.Unlock() }
}

// cacheWriteAttempt owns an immutable inventory assembled outside manager
// locks. Begin under m.mu so Close cannot race WaitGroup.Add.
type cacheWriteAttempt struct {
	manager   *JobManager
	start     writeDecisionSnapshot
	evidence  map[string]selectedWriteEvidence
	complete  bool
	inventory func(string) (writableNamespaceInventory, bool)
}

func (m *JobManager) beginCacheWriteAttempt() (*cacheWriteAttempt, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return nil, false
	}
	m.opWG.Add(1)
	return &cacheWriteAttempt{manager: m, start: m.captureWriteDecisionLocked(), evidence: make(map[string]selectedWriteEvidence), complete: true, inventory: inventoryWritableNamespace}, true
}

func (a *cacheWriteAttempt) Close() {
	if a != nil {
		a.manager.opWG.Done()
	}
}

func decisionJobKey(j *Job) string {
	return j.ID + "\x00" + j.Repo + "\x00" + j.Revision + "\x00" + j.OutputDir + "\x00" + j.HubDir + "\x00" + j.LocalDir + "\x00" + j.LocalRepo + "\x00" + strconv.Itoa(j.generation)
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
	for _, d := range s.jobs {
		j := jobFromDecision(d)
		key := decisionJobKey(j)
		seen[key] = struct{}{}
		a.Collect(j)
	}
	for _, activity := range s.activities {
		j := jobFromDecision(activity.job)
		key := decisionJobKey(j)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		a.Collect(j)
	}
}

// Collect builds each semantic job's writable namespace once, independent
// of target count. It is safe to call only on the attempt-local job copies.
func (a *cacheWriteAttempt) Collect(job *Job) {
	a.CollectAs(job, "job")
}

// CollectAs keeps evidence from separate writer lifetimes distinct even when
// they reuse a visible semantic job ID (for example, a stale run and retry).
func (a *cacheWriteAttempt) CollectAs(job *Job, lifetime string) {
	if a == nil || job == nil || !a.complete {
		return
	}
	key := lifetime + "\x00" + decisionJobKey(job)
	if _, ok := a.evidence[key]; ok {
		return
	}
	inventory := a.inventory
	if inventory == nil {
		inventory = inventoryWritableNamespace
	}
	ev, ok := buildSelectedWriteEvidence(job, inventory)
	if !ok {
		a.complete = false
		return
	}
	a.evidence[key] = ev
}

func (a *cacheWriteAttempt) MayWrite(job *Job, targets []string) bool {
	if a == nil || !a.complete {
		return true
	}
	ev, ok := a.evidence["job\x00"+decisionJobKey(job)]
	if !ok {
		return true
	}
	identities := make([]string, len(targets))
	for i, p := range targets {
		id, err := mutationEntryIdentity(p)
		if err != nil {
			return true
		}
		identities[i] = id
	}
	return selectedWriteEvidenceMayWrite(ev, identities)
}

// Revalidate probes only the explicit bounded identities supplied by the
// caller. It is not a guarantee against unmanaged external filesystem writes.
func (a *cacheWriteAttempt) Revalidate(paths []string) bool {
	if a == nil || !a.complete || !a.manager.writeDecisionUnchanged(a.start) {
		return false
	}
	for _, p := range paths {
		if _, err := mutationEntryIdentity(filepath.Clean(p)); err != nil {
			return false
		}
	}
	return true
}

func (a *cacheWriteAttempt) HasActiveBroadMutation(targets []string) bool {
	if a == nil {
		return true
	}
	for _, scopes := range a.start.mutations {
		for _, scope := range scopes {
			for _, target := range targets {
				if frozenMutationOverlapsRawEntry(scope, target) {
					return true
				}
			}
		}
	}
	return false
}
