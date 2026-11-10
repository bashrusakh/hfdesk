package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

const (
	modelCompanionSafetensors = "SAFE-LFS"
	modelCompanionGGUF        = "NEW-GGUF"
	modelCompanionOld         = "protected-old-target-bytes"
)

type modelCompanionHub struct {
	blockTree   atomic.Bool
	treeReady   chan struct{}
	allowTree   chan struct{}
	allowGGUF   chan struct{}
	ggufStarted chan struct{}
	treeOnce    sync.Once
	ggufOnce    sync.Once
	treeGate    sync.Once
	ggufGate    sync.Once
}

func newModelCompanionHub(t *testing.T) (*httptest.Server, *modelCompanionHub) {
	t.Helper()
	h := &modelCompanionHub{
		treeReady:   make(chan struct{}),
		allowTree:   make(chan struct{}),
		allowGGUF:   make(chan struct{}),
		ggufStarted: make(chan struct{}),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"deadbeef"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			if h.blockTree.Load() {
				h.treeOnce.Do(func() { close(h.treeReady) })
				select {
				case <-h.allowTree:
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"type": "file", "path": "weights/model.safetensors", "size": len(modelCompanionSafetensors), "lfs": map[string]any{"size": len(modelCompanionSafetensors)}},
				{"type": "file", "path": "model-Q4_K_M.gguf", "size": len(modelCompanionGGUF)},
			})
		case strings.HasSuffix(r.URL.Path, "/resolve/deadbeef/weights/model.safetensors"):
			_, _ = w.Write([]byte(modelCompanionSafetensors))
		case strings.HasSuffix(r.URL.Path, "/raw/deadbeef/model-Q4_K_M.gguf"):
			h.ggufOnce.Do(func() { close(h.ggufStarted) })
			select {
			case <-h.allowGGUF:
				_, _ = w.Write([]byte(modelCompanionGGUF))
			case <-r.Context().Done():
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, h
}

func (h *modelCompanionHub) releaseTree() { h.treeGate.Do(func() { close(h.allowTree) }) }
func (h *modelCompanionHub) releaseGGUF() { h.ggufGate.Do(func() { close(h.allowGGUF) }) }

func assertOrdinaryModelCompanionPlan(t *testing.T, endpoint, root string) string {
	t.Helper()
	job := hfdownloader.Job{Repo: "owner/model", Revision: "main", Filters: []string{"weights/model.safetensors"}, ExactMatch: true}
	plan, err := hfdownloader.PlanRepo(context.Background(), job, hfdownloader.Settings{Endpoint: endpoint, OutputDir: root})
	if err != nil {
		t.Fatalf("public PlanRepo: %v", err)
	}
	seenSafe, seenGGUF := false, false
	for _, item := range plan.Items {
		switch item.RelativePath {
		case "weights/model.safetensors":
			seenSafe = item.LFS && item.Size == int64(len(modelCompanionSafetensors))
		case "model-Q4_K_M.gguf":
			seenGGUF = !item.LFS && item.Size == int64(len(modelCompanionGGUF))
		}
	}
	if !seenSafe || !seenGGUF || len(plan.Items) != 2 {
		t.Fatalf("public PlanRepo did not produce the expected ordinary mixed plan: %+v", plan.Items)
	}
	return filepath.Join(root, "owner", "model", "model-Q4_K_M.gguf")
}

func newModelCompanionJobServer(t *testing.T, root, endpoint string) *Server {
	t.Helper()
	return newTestServerWithConfig(t, Config{CacheDir: filepath.Join(t.TempDir(), "cache"), LocalDir: root, Endpoint: endpoint, MaxActive: 1})
}

func createModelCompanionJob(s *Server) (*Job, error) {
	job, _, err := s.jobs.CreateJob(DownloadRequest{
		Repo: "owner/model", Revision: "main", Filters: []string{"weights/model.safetensors"}, ExactMatch: true,
	})
	if err == nil && (job == nil || job.IsDataset) {
		return job, errors.New("model request was not retained as a model job")
	}
	return job, err
}

func writeProtectedModelCompanionTarget(t *testing.T, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(modelCompanionOld), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForModelCompanionSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func assertProtectedModelCompanionBytes(t *testing.T, target, want string) {
	t.Helper()
	got, err := os.ReadFile(target)
	if err != nil || string(got) != want {
		t.Fatalf("target bytes = %q, err=%v; want %q", got, err, want)
	}
}

func assertModelCompanionJobCompleted(t *testing.T, jobs *JobManager, id string) {
	t.Helper()
	waitJobStatus(t, jobs, id, JobStatusCompleted, 10*time.Second)
	job, ok := jobs.GetJob(id)
	if !ok || job.Status != JobStatusCompleted {
		t.Fatalf("job completion status: %+v found=%t", job, ok)
	}
}

func assertModelCompanionTargetMapping(t *testing.T, jobs *JobManager, id, target string) {
	t.Helper()
	job, ok := jobs.GetJob(id)
	if !ok {
		t.Fatalf("could not snapshot model writer %q", id)
	}
	base := jobDestinationBase(job)
	if got := filepath.Join(base, "model-Q4_K_M.gguf"); filepath.Clean(got) != filepath.Clean(target) {
		t.Fatalf("actual model destination %q does not map to protected target %q", got, target)
	}
	if _, err := mutationEntryIdentity(target); err != nil {
		t.Fatalf("selected target identity is not complete: %v", err)
	}
	inventory, complete := inventoryWritableNamespace(base)
	if !complete || len(inventory.aliases) != 0 {
		t.Fatalf("test preconditions lack complete no-alias writer namespace evidence: complete=%t aliases=%+v", complete, inventory.aliases)
	}
}

func TestOrdinaryModelCompanionDownloadRespectsDeleteFirstLease(t *testing.T) {
	root := t.TempDir()
	hf, hub := newModelCompanionHub(t)
	target := assertOrdinaryModelCompanionPlan(t, hf.URL, root)
	writeProtectedModelCompanionTarget(t, target)
	s := newModelCompanionJobServer(t, root, hf.URL)
	t.Cleanup(func() {
		hub.releaseTree()
		hub.releaseGGUF()
	})
	deleteRelease, ok := s.jobs.reserveSelectedGGUF([]string{target})
	if !ok {
		t.Fatal("could not acquire selected-delete lease on the protected target")
	}
	var releaseOnce sync.Once
	releaseDelete := func() { releaseOnce.Do(deleteRelease) }
	t.Cleanup(releaseDelete)
	if job, err := createModelCompanionJob(s); !errors.Is(err, errSelectionWriterBusy) {
		if job != nil {
			_ = s.jobs.CancelJob(job.ID)
		}
		t.Fatalf("ordinary model job crossed held delete lease: job=%+v err=%v", job, err)
	}
	assertProtectedModelCompanionBytes(t, target, modelCompanionOld)
	releaseDelete()
	job, err := createModelCompanionJob(s)
	if err != nil {
		t.Fatalf("create model job after releasing delete lease: %v", err)
	}
	assertModelCompanionTargetMapping(t, s.jobs, job.ID, target)
	waitForModelCompanionSignal(t, hub.ggufStarted, "ordinary model GGUF write request")
	hub.releaseGGUF()
	assertModelCompanionJobCompleted(t, s.jobs, job.ID)
	assertProtectedModelCompanionBytes(t, target, modelCompanionGGUF)
}

func TestOrdinaryModelCompanionDownloadRespectsWriterFirstPrescan(t *testing.T) {
	root := t.TempDir()
	hf, hub := newModelCompanionHub(t)
	target := assertOrdinaryModelCompanionPlan(t, hf.URL, root)
	writeProtectedModelCompanionTarget(t, target)
	s := newModelCompanionJobServer(t, root, hf.URL)
	t.Cleanup(func() {
		hub.releaseTree()
		hub.releaseGGUF()
	})
	hub.blockTree.Store(true)
	job, err := createModelCompanionJob(s)
	if err != nil {
		t.Fatalf("create model job: %v", err)
	}
	waitForModelCompanionSignal(t, hub.treeReady, "production plan prescan")
	if got, ok := s.jobs.GetJob(job.ID); !ok || got.Status != JobStatusRunning {
		t.Fatalf("writer is not running during held prescan: %+v found=%t", got, ok)
	}
	s.jobs.mu.Lock()
	unknownPlan := false
	for _, activity := range s.jobs.runActivities {
		if activity.job.ID == job.ID && !activity.planKnown {
			unknownPlan = true
			break
		}
	}
	s.jobs.mu.Unlock()
	if !unknownPlan {
		t.Fatal("held prescan did not retain the required running/unknown-plan state")
	}
	assertModelCompanionTargetMapping(t, s.jobs, job.ID, target)
	if release, ok := s.jobs.reserveSelectedGGUF([]string{target}); ok {
		release()
		t.Fatal("selected deletion was admitted while the ordinary model writer had no complete plan")
	}
	assertProtectedModelCompanionBytes(t, target, modelCompanionOld)
	hub.releaseTree()
	waitForModelCompanionSignal(t, hub.ggufStarted, "ordinary model GGUF write request after prescan")
	hub.releaseGGUF()
	assertModelCompanionJobCompleted(t, s.jobs, job.ID)
	assertProtectedModelCompanionBytes(t, target, modelCompanionGGUF)
}
