package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSelectedDeleteReservationRechecksNewMatchingWriterAfterInventory(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	m.fenced = true // Keep scheduler execution outside this reservation test.
	// Keep this lifecycle test's initial nonmatch control in the narrow dataset
	// projection; ordinary-model pre-plan negatives require complete plan proof.
	queued := &Job{ID: "queued", Repo: "owner/repo", OutputDir: root, IsDataset: true, Status: JobStatusQueued, Filters: []string{"different.gguf"}, partialFilesMu: &sync.Mutex{}}
	m.jobs[queued.ID] = queued
	target := filepath.Join(jobDestinationBase(queued), "weights.gguf")
	// Establish that the target is genuinely reservable before introducing a
	// nonmatching writer; then introduce a writer whose filter selects it.
	initial, ok := m.reserveSelectedGGUF([]string{target})
	if !ok {
		t.Fatal("initial reservation rejected with no competing writer")
	}
	initial()
	entered, resume := make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	t.Cleanup(func() {
		resumeOnce.Do(func() { close(resume) })
		if err := m.Close(context.Background()); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	var once sync.Once
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		once.Do(func() { close(entered) })
		<-resume
		return inventoryWritableNamespace(base)
	}
	done := make(chan bool, 1)
	go func() { _, ok := m.reserveSelectedGGUF([]string{target}); done <- ok }()
	<-entered
	// Admit a new queued writer after its namespace was inventoried. Its exact
	// filter matches the selected path, so refreshed evidence must reject.
	m.mu.Lock()
	late := &Job{ID: "late", Repo: "owner/repo", OutputDir: root, IsDataset: true, Status: JobStatusQueued, Filters: []string{"weights.gguf"}, partialFilesMu: &sync.Mutex{}}
	m.jobs[late.ID] = late
	m.bumpWriteDecisionLocked()
	m.mu.Unlock()
	resumeOnce.Do(func() { close(resume) })
	if <-done {
		t.Fatal("reservation committed although the newly admitted writer selects the target")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedDeleteReservationCloseDuringInventoryDrainsAttempt(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	// This is a narrow dataset nonmatch control, not model-mode evidence.
	job := &Job{ID: "queued", Repo: "owner/repo", OutputDir: root, IsDataset: true, Status: JobStatusQueued, Filters: []string{"different.gguf"}, partialFilesMu: &sync.Mutex{}}
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
	go func() { _, ok := m.reserveSelectedGGUF([]string{target}); done <- ok }()
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

func TestSelectedDeleteReservationRejectsCompletedManagedAliasMutationDuringInventory(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	m.fenced = true // Keep scheduler execution outside this reservation test.
	base := filepath.Join(root, "managed")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	job := &Job{ID: "queued", Repo: "owner/repo", LocalDir: base, Flat: true, Status: JobStatusQueued, Filters: []string{"different.gguf"}, partialFilesMu: &sync.Mutex{}}
	m.jobs[job.ID] = job
	target := filepath.Join(base, "selected.gguf")
	alias := filepath.Join(jobDestinationBase(job), "outward-alias")
	if err := os.MkdirAll(filepath.Dir(alias), 0700); err != nil {
		t.Fatal(err)
	}
	// A valid control proves this nonmatching queued job does not itself block.
	initial, ok := m.reserveSelectedGGUF([]string{target})
	if !ok {
		t.Fatal("initial reservation rejected despite the nonmatching writer")
	}
	initial()
	entered, resume := make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	t.Cleanup(func() {
		resumeOnce.Do(func() { close(resume) })
		if err := m.Close(context.Background()); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	var once sync.Once
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		inventory, ok := inventoryWritableNamespace(base)
		once.Do(func() {
			close(entered)
			<-resume
		})
		return inventory, ok
	}
	done := make(chan bool, 1)
	reservationReturned := make(chan struct{})
	go func() {
		defer close(reservationReturned)
		_, ok := m.reserveSelectedGGUF([]string{target})
		done <- ok
	}()
	t.Cleanup(func() {
		resumeOnce.Do(func() { close(resume) })
		<-reservationReturned
	})
	<-entered
	// This admitted managed mutation changes the writer's real filesystem
	// namespace while inventory is off-lock, then completes before commit.
	finish, ok := m.beginCacheMutation(alias)
	if !ok {
		resumeOnce.Do(func() { close(resume) })
		<-done
		t.Fatal("managed alias mutation was not admitted")
	}
	mutationActive := true
	defer func() {
		if mutationActive {
			finish()
		}
	}()
	outside := t.TempDir()
	symlinkOrSkip(t, outside, alias)
	finish()
	mutationActive = false
	resumeOnce.Do(func() { close(resume) })
	if <-done {
		t.Fatal("reservation committed from evidence collected before the managed outward alias existed")
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
	m.runActivities[1] = &jobWriteActivity{job: j, planKnown: true, planned: map[string]frozenMutationPath{"/tmp/a": {physical: "/tmp/a", key: "/tmp/a", raw: "/tmp/a"}}}
	s := m.captureWriteDecision()
	j.Filters[0] = "changed"
	j.Status = JobStatusRunning
	m.runActivities[1].planned["/tmp/b"] = frozenMutationPath{physical: "/tmp/b", key: "/tmp/b", raw: "/tmp/b"}
	if s.jobs[0].filters[0] != "one" || s.activities[0].planned[0].physical != "/tmp/a" {
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

func TestWriteDecisionSnapshotStableForAliasedPlannedPaths(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	aliasDir := filepath.Join(root, "alias")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, realDir, aliasDir)
	firstRaw := filepath.Join(realDir, "model.gguf")
	secondRaw := filepath.Join(aliasDir, "model.gguf")
	if err := os.WriteFile(firstRaw, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := freezeMutationEntry(firstRaw)
	if err != nil {
		t.Fatal(err)
	}
	second, err := freezeMutationEntry(secondRaw)
	if err != nil {
		t.Fatal(err)
	}
	first.raw = firstRaw
	second.raw = secondRaw
	if first.key != second.key || first.raw == second.raw {
		t.Fatalf("fixture must have distinct raw paths sharing physical identity: first=%+v second=%+v", first, second)
	}

	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	m.runActivities[1] = &jobWriteActivity{
		job:       &Job{ID: "active", Repo: "owner/repo"},
		planKnown: true,
		planned:   map[string]frozenMutationPath{first.raw: first, second.raw: second},
	}
	const attempts = 128
	wantFirst := first.raw
	if second.raw < wantFirst {
		wantFirst = second.raw
	}
	for i := 0; i < attempts; i++ {
		snapshot := m.captureWriteDecision()
		planned := snapshot.activities[0].planned
		if len(planned) != 2 {
			t.Fatalf("snapshot collapsed logical paths: got %d entries", len(planned))
		}
		if planned[0].raw != wantFirst {
			t.Fatalf("snapshot %d is not deterministically ordered by raw identity: first=%q want=%q", i, planned[0].raw, wantFirst)
		}
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
	symlinkOrSkip(t, first, link)
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
	symlinkOrSkip(t, second, link)
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
	symlinkOrSkip(t, physicalHub, hubLink)
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

func TestCacheWriteEvidenceRevalidatesRawMutationScopeAfterAliasRetarget(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "actual-a")
	dirB := filepath.Join(root, "actual-b")
	for _, dir := range []string{dirA, dirB} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "managed-alias")
	symlinkOrSkip(t, dirA, alias)

	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	release, ok := m.beginCacheMutation(alias)
	if !ok {
		t.Fatal("cache mutation scope rejected")
	}
	defer func() {
		release()
		if err := m.Close(context.Background()); err != nil {
			t.Errorf("close manager: %v", err)
		}
	}()
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, dirB, alias)

	job := &Job{ID: "writer", Repo: "owner/repo", LocalDir: dirB, Flat: true, Status: JobStatusQueued}
	base := jobDestinationBase(job)
	if base == "" {
		t.Fatal("writer destination base unavailable")
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	a, ok := m.beginCacheWriteAttempt()
	if !ok {
		t.Fatal("evidence attempt rejected")
	}
	defer a.Close()
	a.Collect(job)
	if !a.HasActiveBroadMutationFor(decisionJob(job), nil) {
		t.Fatal("retargeted raw mutation scope did not block the writer's current namespace")
	}
}

func TestWriteEvidenceObservedMutationRetainsUnfreezableCleanupScope(t *testing.T) {
	root := t.TempDir()
	obstruction := filepath.Join(root, "obstruct")
	if err := os.WriteFile(obstruction, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanup := filepath.Join(obstruction, "partial")
	if _, err := freezeMutationEntry(cleanup); err == nil {
		t.Skip("platform resolved an entry below a regular file; lexical fallback is unreachable")
	}
	absolute, err := filepath.Abs(filepath.Clean(cleanup))
	if err != nil {
		t.Fatal(err)
	}
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	finish := m.beginObservedMutation([]string{cleanup})
	defer finish()
	a := testCacheWriteAttempt(m)
	found := false
	for _, scopes := range a.start.mutations {
		for _, scope := range scopes {
			if scope.raw != cleanup {
				continue
			}
			found = true
			if scope.key != pathIdentityKey(absolute) {
				t.Fatalf("unfreezable cleanup key=%q want lexical %q", scope.key, pathIdentityKey(absolute))
			}
		}
	}
	if !found {
		t.Fatal("unfreezable cleanup operation was dropped from the mutation snapshot")
	}
	if !a.HasActiveBroadMutation([]string{obstruction}) {
		t.Fatal("unfreezable cleanup scope did not block overlapping writer evidence")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
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
		if len(paths) > 0 && paths[0].raw == tempFile {
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
	symlinkOrSkip(t, physical, leaf)
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
			return m.reserveSelectedHF(filepath.Dir(p), filepath.Join(filepath.Dir(p), "friendly"))
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

func TestCollectSnapshotIgnoresTerminalAndCompletePlanButFailsRelevantUnknownClosed(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	badTerminal := &Job{ID: "terminal", Repo: "../escape", OutputDir: root, Status: JobStatusCompleted}
	badKnown := &Job{ID: "known", Repo: "../escape", OutputDir: root, Status: JobStatusRunning}
	relevant := &Job{ID: "active", Repo: "owner/repo", OutputDir: root, Status: JobStatusRunning}
	m.jobs[badTerminal.ID] = badTerminal
	m.jobs[relevant.ID] = relevant
	knownPath := filepath.Join(root, "exact")
	m.runActivities[1] = &jobWriteActivity{job: badKnown, descriptor: decisionJob(badKnown), planKnown: true, planComplete: true, planned: map[string]frozenMutationPath{knownPath: {physical: knownPath, key: pathIdentityKey(knownPath), raw: knownPath}}}
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
	symlinkOrSkip(t, outside, filepath.Join(base, "escape"))
	a = testCacheWriteAttempt(m)
	a.Collect(relevant)
	if !a.MayWrite(relevant, []string{filepath.Join(base, "weights.gguf")}) {
		t.Fatal("relevant outward alias did not fail closed")
	}
}

func TestSelectedWriterIgnoresInternalAliasCycleForUnreachableTarget(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "owner", "repo")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, base, filepath.Join(base, "loop"))
	job := &Job{ID: "cycle", Repo: "owner/repo", OutputDir: root, Flat: true, Filters: []string{"selected.gguf"}, ExactMatch: true}
	target := filepath.Join(root, "repoB", "selected.gguf")
	if jobMayWriteSelectedPath(job, target) {
		t.Fatal("irrelevant internal alias cycle blocked an unreachable target")
	}
	if !jobMayWriteSelectedPath(job, filepath.Join(base, "selected.gguf")) {
		t.Fatal("reachable selected target was not recognized through the inventoried namespace")
	}
	if err := os.MkdirAll(filepath.Join(root, "repoB"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, deleteFirst := range []bool{false, true} {
		m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "state"))
		m.fenced = true
		queued := *job
		queued.ID, queued.Status, queued.partialFilesMu = "cycle-writer", JobStatusQueued, &sync.Mutex{}
		m.jobs[queued.ID] = &queued
		if deleteFirst {
			release, ok := m.reserveSelectedGGUF([]string{target})
			if !ok {
				t.Fatal("unreachable repoB deletion conflicted with cyclic repoA writer")
			}
			m.mu.Lock()
			blocked := m.jobBlockedByDeleteLocked(&queued)
			m.mu.Unlock()
			release()
			if blocked {
				t.Fatal("cyclic repoA admission conflicted with unreachable repoB deletion")
			}
		} else if release, ok := m.reserveSelectedGGUF([]string{target}); !ok {
			t.Fatal("repoB deletion conflicted with cyclic repoA writer")
		} else {
			release()
		}
		if err := m.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	// Unknown and outward mappings still prevent a negative answer.
	if err := os.Remove(filepath.Join(base, "loop")); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, t.TempDir(), filepath.Join(base, "outward"))
	if !jobMayWriteSelectedPath(job, target) {
		t.Fatal("outward alias did not retain conservative possible-writer handling")
	}
}

func TestSelectedPathCandidateClosureStopsBeforeQuadraticExpansion(t *testing.T) {
	const aliases = maxWritableAliasExpansion + 16
	base := filepath.Join(t.TempDir(), "base")
	inv := writableNamespaceInventory{base: base}
	for i := 0; i < aliases; i++ {
		name := fmt.Sprintf("dir-%05d", i)
		location := filepath.Join(base, name, "back")
		inv.aliases = append(inv.aliases, writableAlias{location: location, dest: base, name: "back"})
	}
	if _, ok := inv.selectedPathCandidates(filepath.Join(base, "dir-00000", "selected.gguf")); ok {
		t.Fatal("path spelling closure exceeded its bounded expansion budget")
	}
}

func TestObservedMutationCleanupRestartsReservationDispatch(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{MaxActive: 1}, nil, filepath.Join(root, "state"))
	job := &Job{ID: "queued", Repo: "owner/repo", OutputDir: root, Status: JobStatusQueued, Filters: []string{"different.gguf"}, partialFilesMu: &sync.Mutex{}}
	m.jobs[job.ID] = job
	entered := make(chan struct{})
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		return inventoryWritableNamespace(base)
	}
	m.deleteReservations[1] = freezeTestEntries(t, filepath.Join(root, "weights.gguf"))
	finishCleanup := m.beginObservedMutation([]string{filepath.Join(root, "weights.gguf")})
	finishCleanup()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("observed-mutation cleanup did not request reservation dispatch")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedDeleteLeaseUsesWriterSelectedPathNotNamespace(t *testing.T) {
	root := t.TempDir()
	localA := filepath.Join(root, "local-a")
	baseA := filepath.Join(localA, "owner", "repoA")
	if err := os.MkdirAll(baseA, 0700); err != nil {
		t.Fatal(err)
	}
	q4 := filepath.Join(baseA, "model-Q4_K_M.gguf")
	q6 := filepath.Join(baseA, "model-Q6_K.gguf")
	for _, p := range []string{q4, q6} {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	localB := filepath.Join(root, "local-b")
	baseB := filepath.Join(localB, "owner", "repoB")
	if err := os.MkdirAll(baseB, 0700); err != nil {
		t.Fatal(err)
	}
	q6B := filepath.Join(baseB, "model-Q6_K.gguf")
	if err := os.WriteFile(q6B, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	m.fenced = true
	// Dataset's direct-match projection preserves the selected-path lease
	// precision this test isolates without claiming a model pre-plan negative.
	writer := &Job{ID: "writer-A", Repo: "owner/repoA", LocalDir: localA, Flat: true, IsDataset: true, Filters: []string{"Q6_K"}, ExactMatch: true, Status: JobStatusQueued, partialFilesMu: &sync.Mutex{}}
	m.jobs[writer.ID] = writer
	lease, ok := m.reserveSelectedGGUF([]string{q4})
	if !ok {
		t.Fatal("disjoint Q4_K entry lease was blocked by Q6_K writer")
	}
	otherRepo, ok := m.reserveSelectedGGUF([]string{q6B})
	if !ok {
		t.Fatal("repoA entry lease blocked disjoint repoB deletion")
	}
	otherRepo()
	if sameRepo, ok := m.reserveSelectedGGUF([]string{q6}); ok {
		sameRepo()
		t.Fatal("queued Q6_K writer did not conflict with its selected Q6_K target")
	}
	lease()
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
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

func TestBroadMutationFailsClosedForFailedDescriptor(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	a := testCacheWriteAttempt(m)
	job := &Job{ID: "unknown", Repo: "owner/repo", OutputDir: root}
	a.inventory = func(string) (writableNamespaceInventory, bool) { return writableNamespaceInventory{}, false }
	a.Collect(job)
	descriptor := decisionJob(job)
	if !a.failed[decisionDescriptorKey(descriptor)] {
		t.Fatal("fixture did not produce a failed descriptor")
	}
	if !a.HasActiveBroadMutationFor(descriptor, nil) {
		t.Fatal("failed descriptor was treated as scope-free by broad-mutation admission")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
