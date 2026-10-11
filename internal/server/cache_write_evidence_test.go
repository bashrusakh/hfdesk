package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

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
	a := &cacheWriteAttempt{evidence: map[string]selectedWriteEvidence{}, complete: true}
	// Missing inventory is conservative, including when no evidence was collected.
	if !a.MayWrite(j, []string{filepath.Join(root, "owner", "repo", "weights.gguf")}) {
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
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	missingParent := filepath.Join(parent, "target")
	if a.Revalidate([]string{missingParent}) {
		t.Fatal("unresolvable probe accepted")
	}
	if !a.Revalidate([]string{filepath.Join(t.TempDir(), "new-target")}) {
		t.Fatal("stable bounded identity probe rejected")
	}
}

func TestCacheWriteAttemptInventoriesOncePerLifetimeNotTarget(t *testing.T) {
	root := t.TempDir()
	j := &Job{ID: "j", Repo: "owner/repo", OutputDir: root}
	count := 0
	a := &cacheWriteAttempt{evidence: map[string]selectedWriteEvidence{}, complete: true, inventory: func(base string) (writableNamespaceInventory, bool) {
		count++
		return writableNamespaceInventory{base: base}, true
	}}
	a.CollectAs(j, "activity-old")
	a.CollectAs(j, "activity-old")
	a.CollectAs(j, "retry-new")
	if count != 4 {
		t.Fatalf("inventories=%d, want two namespace bases per distinct writer lifetime", count)
	}
	if len(a.evidence) != 2 {
		t.Fatalf("evidence=%d", len(a.evidence))
	}
	// Targets are identity probes against cached evidence, not fresh inventories.
	for _, target := range []string{filepath.Join(root, "owner", "repo", "a"), filepath.Join(root, "owner", "repo", "b")} {
		a.MayWrite(j, []string{target})
	}
	if count != 4 {
		t.Fatalf("target checks rebuilt inventory: %d", count)
	}
}

func TestCacheWriteAttemptCollectsSnapshotJobsAndActivitiesOnce(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	j := &Job{ID: "same", Repo: "owner/repo", OutputDir: root, Status: JobStatusQueued, Filters: []string{"*.gguf"}}
	m.jobs[j.ID] = j
	m.runActivities[1] = &jobWriteActivity{job: j, planned: map[string]struct{}{}}
	// An old in-flight generation and a retry are distinct writers despite the
	// shared visible job ID and destination.
	old := *j
	old.generation = 1
	m.runActivities[2] = &jobWriteActivity{job: &old, planned: map[string]struct{}{}}
	s := m.captureWriteDecision()
	count := 0
	a := &cacheWriteAttempt{evidence: map[string]selectedWriteEvidence{}, complete: true, inventory: func(base string) (writableNamespaceInventory, bool) {
		count++
		return writableNamespaceInventory{base: base}, true
	}}
	a.CollectSnapshot(s)
	if len(a.evidence) != 2 || count != 4 {
		t.Fatalf("evidence=%d inventory calls=%d; want two lifetimes × two namespaces", len(a.evidence), count)
	}
	for _, target := range []string{filepath.Join(root, "owner", "repo", "a.gguf"), filepath.Join(root, "owner", "repo", "b.gguf")} {
		a.MayWrite(j, []string{target})
	}
	if count != 4 {
		t.Fatalf("multiple targets rebuilt inventory: %d", count)
	}
}

func TestCacheWriteAttemptMutationScopesAreTargetRelevant(t *testing.T) {
	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(t.TempDir(), "state"))
	root := t.TempDir()
	end := m.beginObservedMutation([]string{root})
	a := &cacheWriteAttempt{manager: m, start: m.captureWriteDecision(), complete: true}
	if a.HasActiveBroadMutation([]string{filepath.Join(t.TempDir(), "other")}) {
		t.Fatal("unrelated active mutation caused a conflict")
	}
	if !a.HasActiveBroadMutation([]string{filepath.Join(root, "inside")}) {
		t.Fatal("overlapping active mutation did not block negative evidence")
	}
	end()
}
