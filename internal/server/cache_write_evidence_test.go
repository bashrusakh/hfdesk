package server

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSelectedDeleteReservationInventoriesOffLockAndRejectsChangedSnapshot(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	job := &Job{ID: "queued", Repo: "owner/repo", OutputDir: root, Status: JobStatusQueued, Filters: []string{"weights.gguf"}}
	m.jobs[job.ID] = job
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		once.Do(func() { close(entered) })
		<-resume
		return inventoryWritableNamespace(base)
	}
	target := filepath.Join(jobDestinationBase(job), "weights.gguf")
	done := make(chan bool, 1)
	go func() { _, ok := m.reserveSelectedGGUF([]string{target}); done <- ok }()
	<-entered
	if _, ok := m.GetJob(job.ID); !ok || len(m.ListJobs()) != 1 {
		t.Fatal("inventory held manager lock while readers were blocked")
	}
	// A newly admitted queued writer changes the decision snapshot; evidence
	// collected before it existed must not authorize a late delete commit.
	m.mu.Lock()
	m.jobs["late"] = &Job{ID: "late", Repo: "owner/other", OutputDir: root, Status: JobStatusQueued}
	m.mu.Unlock()
	close(resume)
	if <-done {
		t.Fatal("reservation committed using a stale manager snapshot")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedDeleteReservationCloseDuringInventoryDrainsAttempt(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	job := &Job{ID: "queued", Repo: "owner/repo", OutputDir: root, Status: JobStatusQueued, Filters: []string{"different.gguf"}, partialFilesMu: &sync.Mutex{}}
	m.fenced = true // Keep scheduler execution outside this reservation test.
	m.jobs[job.ID] = job
	target := filepath.Join(jobDestinationBase(job), "other.gguf")
	if jobMayWriteSelectedPath(job, target) {
		t.Fatal("pre-shutdown nonmatching writer unexpectedly conflicts with reservation")
	}
	preCloseRelease, ok := m.reserveSelectedGGUF([]string{target})
	if !ok {
		t.Fatal("pre-shutdown reservation rejected despite the nonmatching writer")
	}
	preCloseRelease()
	entered, resume := make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	t.Cleanup(func() { resumeOnce.Do(func() { close(resume) }) })
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-resume
		return inventoryWritableNamespace(base)
	}
	done := make(chan bool, 1)
	go func() {
		_, ok := m.reserveSelectedGGUF([]string{target})
		done <- ok
	}()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- m.Close(context.Background()) }()
	waitForJobManagerStopping(t, m)
	resumeOnce.Do(func() { close(resume) })
	if <-done {
		t.Fatal("reservation committed after Close began")
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
}

func TestSelectedDeleteReservationRejectsManagedMutationCrossingInventory(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	job := &Job{ID: "queued", Repo: "owner/repo", OutputDir: root, Status: JobStatusQueued}
	m.jobs[job.ID] = job
	entered, resume := make(chan struct{}), make(chan struct{})
	var calls int
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		calls++
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-resume
		return inventoryWritableNamespace(base)
	}
	done := make(chan bool, 1)
	target := filepath.Join(jobDestinationBase(job), "other.gguf")
	go func() { _, ok := m.reserveSelectedGGUF([]string{target}); done <- ok }()
	<-entered
	finish := m.beginObservedMutation([]string{filepath.Join(root, "unrelated-managed-path")})
	finish() // completed during collection must still invalidate this attempt
	close(resume)
	if <-done {
		t.Fatal("unrelated test reservation unexpectedly conflicted")
	}
	if calls < 4 {
		t.Fatalf("stale scan was not retried with fresh inventories: calls=%d", calls)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func testCacheWriteAttempt(m *JobManager) *cacheWriteAttempt {
	return &cacheWriteAttempt{manager: m, start: m.captureWriteDecision(), evidence: map[string]selectedWriteEvidence{}, failed: map[string]bool{}, descriptors: map[string]writeDecisionJob{}, probes: map[string]writeIdentityProbe{}, targets: map[string]writeIdentityProbe{}, inventory: inventoryWritableNamespace}
}

func TestWriteDecisionSnapshotCopiesOnlyDecisionState(t *testing.T) {
	m := newJobManagerWithStatePath(Config{MaxActive: 2}, nil, filepath.Join(t.TempDir(), "state"))
	j := &Job{ID: "j", Repo: "o/r", Status: JobStatusQueued, Filters: []string{"one"}, Excludes: []string{"skip"}}
	m.jobs[j.ID] = j
	m.runActivities[1] = &jobWriteActivity{job: j, planKnown: true, planned: map[string]struct{}{"/tmp/a": {}}}
	s := m.captureWriteDecision()
	j.Filters[0] = "changed"
	j.Status = JobStatusRunning
	m.runActivities[1].planned["/tmp/b"] = struct{}{}
	if s.jobs[0].filters[0] != "one" || s.activities[0].planned[0] != "/tmp/a" {
		t.Fatal("snapshot shares live slices or maps")
	}
	j.Filters = []string{"one"}
	j.Status = JobStatusQueued
	delete(m.runActivities[1].planned, "/tmp/b")
	s = m.captureWriteDecision()
	j.Progress.DownloadedBytes++
	if !m.writeDecisionUnchanged(s) {
		t.Fatal("progress-only changes invalidated decision")
	}
	m.runActivities[1].planKnown = false
	if m.writeDecisionUnchanged(s) {
		t.Fatal("planKnown change did not invalidate decision")
	}
}

func TestWriteDecisionEpochDetectsCompletedMutation(t *testing.T) {
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "state"))
	s := m.captureWriteDecision()
	end := m.beginObservedMutation([]string{"/tmp/managed"})
	end()
	if m.writeDecisionUnchanged(s) {
		t.Fatal("begin+finish mutation escaped epoch validation")
	}
}

func TestWriteDecisionReservationAndMutationSignal(t *testing.T) {
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "state"))
	before := m.captureWriteDecision()
	ch := m.writeDecisionChangeChannel()
	end := m.beginObservedMutation([]string{t.TempDir()})
	select {
	case <-ch:
	default:
		t.Fatal("begin did not wake decision waiters")
	}
	if m.writeDecisionUnchanged(before) {
		t.Fatal("mutation scope did not invalidate snapshot")
	}
	end()
}

func TestCacheWriteAttemptDeduplicatesJobsAndFailsClosed(t *testing.T) {
	root := t.TempDir()
	j := &Job{ID: "j", Repo: "owner/repo", OutputDir: root, Filters: []string{"weights.gguf"}}
	a := testCacheWriteAttempt(newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state")))
	// Missing inventory is conservative, including when no evidence was collected.
	if !a.MayWrite(j, []string{filepath.Join(jobDestinationBase(j), "weights.gguf")}) {
		t.Fatal("unbuilt inventory did not fail closed")
	}
}

func TestCacheWriteAttemptLifetimeRejectsAfterClose(t *testing.T) {
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "state"))
	a, ok := m.beginCacheWriteAttempt()
	if !ok {
		t.Fatal("initial attempt rejected")
	}
	a.Close()
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.beginCacheWriteAttempt(); ok {
		t.Fatal("attempt admitted after Close")
	}
}

func TestCacheWriteAttemptProbeFailsClosedOnIdentityError(t *testing.T) {
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "state"))
	a, ok := m.beginCacheWriteAttempt()
	if !ok {
		t.Fatal("attempt rejected")
	}
	defer a.Close()
	root := t.TempDir()
	j := &Job{ID: "probe", Repo: "owner/repo", OutputDir: root, Status: JobStatusQueued}
	base := jobDestinationBase(j)
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	a.Collect(j)
	target := filepath.Join(base, "target")
	a.MayWrite(j, []string{target})
	if !a.Revalidate([]string{target}) {
		t.Fatal("stable bounded identity probe rejected")
	}
	if err := os.RemoveAll(base); err != nil {
		t.Fatal(err)
	}
	if a.Revalidate([]string{target}) {
		t.Fatal("probe resolution error was not fail-closed")
	}
}

func TestCacheWriteAttemptRevalidatesCapturedTargetsAndNamespaceAliases(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	link := filepath.Join(root, "root-link")
	for _, dir := range []string{filepath.Join(first, "owner", "repo"), filepath.Join(second, "owner", "repo")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	a, ok := m.beginCacheWriteAttempt()
	if !ok {
		t.Fatal("attempt rejected")
	}
	defer a.Close()
	j := &Job{ID: "alias", Repo: "owner/repo", OutputDir: link, Status: JobStatusQueued}
	a.Collect(j)
	target := filepath.Join(link, "owner", "repo", "weights.gguf")
	a.MayWrite(j, []string{target})
	if !a.Revalidate([]string{target}) {
		t.Fatal("stable captured identities rejected")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	if a.Revalidate([]string{target}) {
		t.Fatal("valid symlink retarget was not detected")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if a.Revalidate([]string{target}) {
		t.Fatal("probe resolution error was not fail-closed")
	}
}

func TestCacheWriteAttemptInventoriesOncePerLifetimeNotTarget(t *testing.T) {
	root := t.TempDir()
	j := &Job{ID: "j", Repo: "owner/repo", OutputDir: root}
	count := 0
	a := testCacheWriteAttempt(newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state")))
	a.inventory = func(base string) (writableNamespaceInventory, bool) {
		count++
		identity, err := mutationDirectoryIdentity(base)
		if err != nil {
			return writableNamespaceInventory{}, false
		}
		return writableNamespaceInventory{base: identity}, true
	}
	a.Collect(j)
	a.Collect(j)
	newDescriptor := *j
	newDescriptor.generation++
	a.Collect(&newDescriptor)
	differentSelection := *j
	differentSelection.Filters = []string{"other.gguf"}
	a.Collect(&differentSelection)
	if count != 6 {
		t.Fatalf("inventories=%d, want each semantic descriptor × two namespaces", count)
	}
	if len(a.evidence) != 3 {
		t.Fatalf("evidence=%d", len(a.evidence))
	}
	// Targets are identity probes against cached evidence, not fresh inventories.
	for _, target := range []string{filepath.Join(root, "owner", "repo", "a"), filepath.Join(root, "owner", "repo", "b")} {
		a.MayWrite(j, []string{target})
	}
	if count != 6 {
		t.Fatalf("target checks rebuilt inventory: %d", count)
	}
}

func TestCacheWriteAttemptCollectsSnapshotJobsAndActivitiesOnce(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	j := &Job{ID: "same", Repo: "owner/repo", OutputDir: root, Status: JobStatusQueued, starting: true}
	m.jobs[j.ID] = j
	m.mu.Lock()
	m.registerRunActivityLocked(j)
	m.mu.Unlock()
	s := m.captureWriteDecision()
	count := 0
	a := testCacheWriteAttempt(m)
	a.inventory = func(base string) (writableNamespaceInventory, bool) {
		count++
		identity, err := mutationDirectoryIdentity(base)
		if err != nil {
			return writableNamespaceInventory{}, false
		}
		return writableNamespaceInventory{base: identity}, true
	}
	a.CollectSnapshot(s)
	if len(a.evidence) != 1 || count != 2 {
		t.Fatalf("evidence=%d inventory calls=%d; wanted one imminent writer × two namespaces", len(a.evidence), count)
	}
	var target string
	for _, ev := range a.evidence {
		target = filepath.Join(ev.inventories[0].base, "weights.gguf")
	}
	if !a.MayWrite(j, []string{target}) {
		t.Fatal("registered starting activity evidence did not match the imminent job")
	}
	for _, target := range []string{filepath.Join(root, "owner", "repo", "a.gguf"), filepath.Join(root, "owner", "repo", "b.gguf")} {
		a.MayWrite(j, []string{target})
	}
	if count != 2 {
		t.Fatalf("multiple targets rebuilt inventory: %d", count)
	}
}

func TestCollectSnapshotKeepsHistoricalActivityDistinctFromQueuedRetry(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	old := &Job{ID: "retry", Repo: "owner/repo", OutputDir: root, Status: JobStatusRunning}
	m.mu.Lock()
	m.registerRunActivityLocked(old)
	m.mu.Unlock()
	old.generation = 1 // the registered run has now started and claimed generation 1.
	queued := *old
	queued.Status = JobStatusQueued
	queued.starting = false
	m.jobs[queued.ID] = &queued
	s := m.captureWriteDecision()
	count := 0
	a := testCacheWriteAttempt(m)
	a.inventory = func(base string) (writableNamespaceInventory, bool) {
		count++
		identity, err := mutationDirectoryIdentity(base)
		if err != nil {
			return writableNamespaceInventory{}, false
		}
		return writableNamespaceInventory{base: identity}, true
	}
	a.CollectSnapshot(s)
	if len(a.evidence) != 2 || count != 4 {
		t.Fatalf("historical + retry evidence=%d inventories=%d, want distinct descriptors", len(a.evidence), count)
	}
}

func TestCacheWriteAttemptMutationScopesAreTargetRelevant(t *testing.T) {
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "state"))
	root := t.TempDir()
	end := m.beginObservedMutation([]string{root})
	a := testCacheWriteAttempt(m)
	if a.HasActiveBroadMutation([]string{filepath.Join(t.TempDir(), "other")}) {
		t.Fatal("unrelated active mutation caused a conflict")
	}
	if !a.HasActiveBroadMutation([]string{filepath.Join(root, "inside")}) {
		t.Fatal("overlapping active mutation did not block negative evidence")
	}
	end()
}

func TestCacheWriteAttemptMutationScopesCoverWholeWriterNamespace(t *testing.T) {
	root := t.TempDir()
	j := &Job{ID: "writer", Repo: "owner/repo", OutputDir: root, Status: JobStatusQueued}
	base := jobDestinationBase(j)
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	a := testCacheWriteAttempt(m)
	a.inventory = func(path string) (writableNamespaceInventory, bool) {
		identity, err := mutationDirectoryIdentity(path)
		if err != nil {
			return writableNamespaceInventory{}, false
		}
		return writableNamespaceInventory{base: identity}, true
	}
	a.Collect(j)
	end := m.beginObservedMutation([]string{filepath.Join(base, "other-subtree")})
	a.start = m.captureWriteDecision()
	if !a.HasActiveBroadMutation(nil) {
		t.Fatal("sibling mutation inside writer namespace did not conflict")
	}
	end()
	end = m.beginObservedMutation([]string{filepath.Join(root, "unrelated")})
	a.start = m.captureWriteDecision()
	if a.HasActiveBroadMutation(nil) {
		t.Fatal("unrelated namespace mutation conflicted")
	}
	end()
}

func TestBeginCacheMutationSymlinkDirectoryScopeConflictsWithSnapshotNamespace(t *testing.T) {
	root := t.TempDir()
	physicalHub := filepath.Join(root, "physical-hub")
	if err := os.MkdirAll(physicalHub, 0700); err != nil {
		t.Fatal(err)
	}
	hubLink := filepath.Join(root, "hub-link")
	if err := os.Symlink(physicalHub, hubLink); err != nil {
		t.Fatal(err)
	}
	job := &Job{ID: "writer", Repo: "owner/model", OutputDir: filepath.Join(root, "friendly-root"), HubDir: hubLink, Status: JobStatusRunning}
	repo := hfJobRepoPath(job)
	if repo == "" {
		t.Fatal("HF repo path unavailable")
	}
	if err := os.MkdirAll(filepath.Join(repo, "snapshots"), 0700); err != nil {
		t.Fatal(err)
	}
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	release, ok := m.beginCacheMutation(hubLink)
	if !ok {
		t.Fatal("cache mutation scope rejected")
	}
	a, ok := m.beginCacheWriteAttempt()
	if !ok {
		t.Fatal("evidence attempt rejected")
	}
	a.Collect(job)
	if !m.writeDecisionUnchanged(a.start) {
		t.Fatal("manager epoch changed during collection")
	}
	if !a.HasActiveBroadMutation(nil) {
		t.Fatal("symlink directory scope failed to overlap resolved snapshot namespace")
	}
	a.Close()
	release()

	unrelated := filepath.Join(root, "other-tree")
	if err := os.MkdirAll(unrelated, 0700); err != nil {
		t.Fatal(err)
	}
	release, ok = m.beginCacheMutation(unrelated)
	if !ok {
		t.Fatal("unrelated mutation scope rejected")
	}
	defer release()
	a, ok = m.beginCacheWriteAttempt()
	if !ok {
		t.Fatal("second evidence attempt rejected")
	}
	defer a.Close()
	a.Collect(job)
	if a.HasActiveBroadMutation(nil) {
		t.Fatal("unrelated directory scope blocked writer evidence")
	}
}

func TestObservedCleanupEntryScopeIsTypedAndNamespaceSensitive(t *testing.T) {
	root := t.TempDir()
	writerRoot := filepath.Join(root, "writer")
	job := &Job{ID: "writer", Repo: "owner/repo", LocalDir: writerRoot, Flat: true}
	base := jobDestinationBase(job)
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	tempFile := filepath.Join(base, "tmp-blob")
	if err := os.WriteFile(tempFile, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	end := m.beginObservedMutation([]string{tempFile})
	defer end()
	a, ok := m.beginCacheWriteAttempt()
	if !ok {
		t.Fatal("attempt rejected")
	}
	defer a.Close()
	a.Collect(job)
	if !m.writeDecisionUnchanged(a.start) {
		t.Fatal("manager state changed during entry-scope collection")
	}
	var scopeID uint64
	for id, paths := range a.start.mutations {
		if len(paths) > 0 && paths[0] == tempFile {
			scopeID = id
			if a.start.mutationKinds[id] != writeScopeEntry {
				t.Fatal("cleanup file scope lost its explicit entry kind")
			}
		}
	}
	if scopeID == 0 {
		t.Fatal("observed cleanup entry missing from immutable scope snapshot")
	}
	if !a.HasActiveBroadMutation(nil) {
		t.Fatal("relevant cleanup file did not overlap its writer namespace")
	}
	m.mu.Lock()
	m.mutationScopeKinds[scopeID] = writeScopeDirectory
	m.mu.Unlock()
	if m.writeDecisionUnchanged(a.start) {
		t.Fatal("scope-kind-only change did not invalidate the captured decision")
	}
	m.mu.Lock()
	m.mutationScopeKinds[scopeID] = writeScopeEntry
	m.mu.Unlock()
	if _, ok := m.reserveSelectedGGUF([]string{tempFile}); ok {
		t.Fatal("selected delete overlapped active cleanup entry")
	}
	if _, ok := m.reserveSelectedHF(base, filepath.Join(base, "friendly")); ok {
		t.Fatal("HF namespace reservation overlapped active cleanup entry")
	}
	unrelatedEntry := filepath.Join(root, "unrelated", "tmp-blob")
	if err := os.MkdirAll(filepath.Dir(unrelatedEntry), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unrelatedEntry, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	deleteRelease, ok := m.reserveSelectedGGUF([]string{unrelatedEntry})
	if !ok {
		t.Fatal("unrelated local deletion was blocked by cleanup entry")
	}
	deleteRelease()
	hfRelease, ok := m.reserveSelectedHF(filepath.Join(root, "unrelated-hub"), filepath.Join(root, "unrelated-friendly"))
	if !ok {
		t.Fatal("unrelated HF reservation was blocked by cleanup entry")
	}
	hfRelease()

	unrelated := &Job{ID: "other", Repo: "owner/repo", LocalDir: filepath.Join(root, "other-writer"), Flat: true}
	b := testCacheWriteAttempt(m)
	b.Collect(unrelated)
	if b.HasActiveBroadMutation(nil) {
		t.Fatal("existing cleanup file falsely conflicted with an unrelated namespace")
	}
}

func TestSelectedLocalDeleteKeepsFinalSymlinkEntryIdentity(t *testing.T) {
	root := t.TempDir()
	writerRoot := filepath.Join(root, "writer")
	job := &Job{ID: "writer", Repo: "owner/repo", LocalDir: writerRoot, Flat: true}
	base := jobDestinationBase(job)
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(base, "weights.gguf")
	if err := os.WriteFile(physical, []byte("weight"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(outside, "selected.gguf")
	if err := os.Symlink(physical, leaf); err != nil {
		t.Fatal(err)
	}
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	release, ok := m.reserveSelectedGGUF([]string{leaf})
	if !ok {
		t.Fatal("leaf deletion reservation rejected")
	}
	defer release()
	a := testCacheWriteAttempt(m)
	a.Collect(job)
	if a.HasActiveBroadMutation(nil) {
		t.Fatal("entry-scoped symlink deletion followed its leaf target")
	}
}

func TestSelectedReservationBeginAndEndAdvanceDecisionEpoch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reserve func(*JobManager, string) (func(), bool)
	}{
		{"selected GGUF", func(m *JobManager, p string) (func(), bool) { return m.reserveSelectedGGUF([]string{p}) }},
		{"selected HF", func(m *JobManager, p string) (func(), bool) {
			return m.reserveSelectedHF(p, filepath.Join(filepath.Dir(p), "friendly"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "entry")
			if err := os.WriteFile(target, []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
			m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
			before := m.captureWriteDecision()
			release, ok := tc.reserve(m, target)
			if !ok {
				t.Fatal("reservation rejected")
			}
			if m.writeDecisionUnchanged(before) {
				t.Fatal("reservation begin did not invalidate snapshot")
			}
			during := m.captureWriteDecision()
			release()
			if m.writeDecisionUnchanged(during) {
				t.Fatal("reservation release did not invalidate snapshot")
			}
		})
	}
}

func TestRunActivityKeepsRegisteredDescriptorAndFullSemanticKey(t *testing.T) {
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "state"))
	j := &Job{ID: "reused", Repo: "owner/repo", Revision: "main", OutputDir: "/tmp/out", Filters: []string{"a"}, generation: 8}
	m.mu.Lock()
	id := m.registerRunActivityLocked(j)
	m.mu.Unlock()
	j.generation = 50
	j.Filters[0] = "changed"
	s := m.captureWriteDecision()
	var activity writeDecisionActivity
	for _, a := range s.activities {
		if a.id == id {
			activity = a
		}
	}
	if activity.job.generation != 9 || activity.job.filters[0] != "a" {
		t.Fatal("activity descriptor followed mutated live job")
	}
	base := jobFromDecision(activity.job)
	base.generation = activity.job.generation
	mutations := []func(*Job){func(x *Job) { x.Filters = []string{"b"} }, func(x *Job) { x.Excludes = []string{"x"} }, func(x *Job) { x.ExactMatch = true }, func(x *Job) { x.Flat = true }, func(x *Job) { x.IsDataset = true }, func(x *Job) { x.LocalDir = "/tmp/local" }, func(x *Job) { x.OutputDir = "/tmp/other" }, func(x *Job) { x.HubDir = "/tmp/hub" }, func(x *Job) { x.LocalRepo = "redirect" }, func(x *Job) { x.Revision = "dev" }, func(x *Job) { x.generation++ }}
	keys := map[string]bool{decisionJobKey(base): true}
	for _, mutate := range mutations {
		copy := *base
		mutate(&copy)
		key := decisionJobKey(&copy)
		if keys[key] {
			t.Fatal("semantic descriptor field omitted from identity")
		}
		keys[key] = true
	}
}

func TestCollectSnapshotIgnoresTerminalAndKnownPlanButFailsRelevantUnknownClosed(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	badTerminal := &Job{ID: "terminal", Repo: "../escape", OutputDir: root, Status: JobStatusCompleted}
	badKnown := &Job{ID: "known", Repo: "../escape", OutputDir: root, Status: JobStatusRunning}
	relevant := &Job{ID: "active", Repo: "owner/repo", OutputDir: root, Status: JobStatusRunning}
	m.jobs[badTerminal.ID] = badTerminal
	m.jobs[relevant.ID] = relevant
	m.runActivities[1] = &jobWriteActivity{job: badKnown, descriptor: decisionJob(badKnown), planKnown: true, planned: map[string]struct{}{filepath.Join(root, "exact"): {}}}
	m.runActivities[2] = &jobWriteActivity{job: relevant, descriptor: decisionJob(relevant)}
	a := testCacheWriteAttempt(m)
	count := 0
	a.inventory = func(base string) (writableNamespaceInventory, bool) {
		count++
		return writableNamespaceInventory{base: base}, true
	}
	s := m.captureWriteDecision()
	a.CollectSnapshot(s)
	if len(a.evidence) != 1 || count != 2 {
		t.Fatalf("collected evidence=%d inventories=%d, want only relevant unknown activity", len(a.evidence), count)
	}

	// A relevant outward directory alias remains unknown and fails closed.
	base := jobDestinationBase(relevant)
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "escape")); err != nil {
		t.Fatal(err)
	}
	a = testCacheWriteAttempt(m)
	a.Collect(relevant)
	if !a.MayWrite(relevant, []string{filepath.Join(base, "weights.gguf")}) {
		t.Fatal("relevant outward alias did not fail closed")
	}
}

func TestFailedInventoryIsMemoizedPerDescriptor(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	a := testCacheWriteAttempt(m)
	job := &Job{ID: "unknown", Repo: "owner/repo", OutputDir: root}
	calls := 0
	a.inventory = func(string) (writableNamespaceInventory, bool) { calls++; return writableNamespaceInventory{}, false }
	a.Collect(job)
	a.Collect(job)
	if calls != 1 || len(a.failed) != 1 {
		t.Fatalf("failed inventory rescanned: calls=%d failed=%d", calls, len(a.failed))
	}
	if !a.MayWrite(job, []string{filepath.Join(root, "target")}) {
		t.Fatal("failed descriptor did not remain fail-closed")
	}
}
