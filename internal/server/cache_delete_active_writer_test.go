// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gatedHubEndpoint serves one small LFS file and holds the first payload GET
// until opened, so a production download stays provably in flight while the
// test drives the registered cache routes.
type gatedHubEndpoint struct {
	srv        *httptest.Server
	body       []byte
	hash       string
	reached    chan struct{}
	release    chan struct{}
	reachedOne sync.Once
	releaseOne sync.Once
}

func newGatedHubEndpoint(t *testing.T) *gatedHubEndpoint {
	t.Helper()
	f := &gatedHubEndpoint{
		body:    []byte("gated cache weights payload"),
		reached: make(chan struct{}),
		release: make(chan struct{}),
	}
	f.hash = fmt.Sprintf("%x", sha256.Sum256(f.body))
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "commit123"})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"type": "file", "path": "weights.safetensors", "size": len(f.body), "lfs": map[string]any{"oid": f.hash, "size": len(f.body)}}})
		case strings.Contains(r.URL.Path, "/resolve/"):
			switch {
			case r.Header.Get("X-HFDesk-Addressing-Probe") == "1":
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(f.body)))
				w.Header().Set("Content-Length", "1")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(f.body[:1])
			case r.Method == http.MethodHead:
				w.Header().Set("Content-Length", fmt.Sprint(len(f.body)))
				w.Header().Set("ETag", f.hash)
			default:
				f.reachedOne.Do(func() { close(f.reached) })
				select {
				case <-f.release:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(f.body)))
				w.Header().Set("ETag", f.hash)
				_, _ = w.Write(f.body)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	t.Cleanup(f.open)
	return f
}

func (f *gatedHubEndpoint) open() {
	f.releaseOne.Do(func() { close(f.release) })
}

// A whole-repository cache DELETE must not remove the payload of an active
// writer: it is refused with 409 while that writer keeps running, unrelated
// repositories stay deletable, and it succeeds once the writer is finished.
func TestWholeCacheDeleteRefusesActiveWriter(t *testing.T) {
	isolateCacheState(t)
	fx := newGatedHubEndpoint(t)
	cacheDir := t.TempDir()
	hubDir := filepath.Join(cacheDir, "hub")
	repoDir := filepath.Join(hubDir, "models--owner--model")
	friendlyDir := filepath.Join(cacheDir, "models", "owner", "model")

	s := New(Config{CacheDir: cacheDir, MaxActive: 1, Concurrency: 1, Verify: "sha256", Endpoint: fx.srv.URL})
	t.Cleanup(func() {
		fx.open()
		for _, job := range s.jobs.ListJobs() {
			if job.Status != JobStatusCompleted && job.Status != JobStatusFailed && job.Status != JobStatusCancelled {
				s.jobs.CancelJob(job.ID)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.jobs.Close(ctx)
	})

	job, _, err := s.jobs.CreateJob(DownloadRequest{Repo: "owner/model"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-fx.reached:
	case <-time.After(15 * time.Second):
		t.Fatal("download never reached its gated payload transfer")
	}

	w := cacheRequest(t, s, http.MethodDelete, "/api/cache/owner/model", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("whole-repo DELETE during an active write must be refused: status=%d body=%s", w.Code, w.Body.String())
	}
	if info, err := os.Stat(repoDir); err != nil || !info.IsDir() {
		t.Fatalf("refused DELETE removed the active writer's repository: %v", err)
	}
	if info, err := os.Stat(filepath.Join(hubDir, ".hfdesk-control")); err != nil || !info.IsDir() {
		t.Fatalf("refused DELETE damaged the ownership control tree: %v", err)
	}
	active, ok := s.jobs.GetJob(job.ID)
	if !ok {
		t.Fatal("active writer disappeared after the refused DELETE")
	}
	if active.Status != JobStatusRunning {
		t.Fatalf("writer must keep running after the refused DELETE: status=%s err=%q", active.Status, active.Error)
	}

	// An unrelated repository is still deletable while that writer runs.
	other := filepath.Join(hubDir, "models--owner--other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	w = cacheRequest(t, s, http.MethodDelete, "/api/cache/owner/other", "")
	if w.Code != http.StatusOK {
		t.Fatalf("unrelated whole-repo DELETE was blocked: status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("unrelated repository survived its DELETE: %v", err)
	}
	if active, ok = s.jobs.GetJob(job.ID); !ok || active.Status != JobStatusRunning {
		t.Fatalf("unrelated DELETE disturbed the active writer: ok=%t snapshot=%+v", ok, active)
	}

	fx.open()
	waitJobStatus(t, s.jobs, job.ID, JobStatusCompleted, 30*time.Second)
	s.jobs.WaitAll(30 * time.Second)
	assertCacheArtifacts(t, cacheDir, hubDir, fx.body, fx.hash)

	w = cacheRequest(t, s, http.MethodDelete, "/api/cache/owner/model", "")
	if w.Code != http.StatusOK {
		t.Fatalf("whole-repo DELETE after the writer finished must succeed: status=%d body=%s", w.Code, w.Body.String())
	}
	for _, path := range []string{repoDir, friendlyDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("finished DELETE left %s behind: %v", path, err)
		}
	}
}

// beginCacheMutation must refuse a whole-directory scope an active writer
// still owns. The unknown-plan cases are discriminated through the
// destination/repository fallback; a second case isolates the frozen-plan
// rule so that only it can refuse.
func TestBeginCacheMutationConflictsWithActiveWriter(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	hubDir := filepath.Join(cacheDir, "hub")
	repoDir := filepath.Join(hubDir, "models--owner--model")
	friendlyDir := filepath.Join(cacheDir, "models", "owner", "model")
	weights := filepath.Join(friendlyDir, "weights.safetensors")
	unrelated := filepath.Join(cacheDir, "unrelated-tree")
	for _, dir := range []string{repoDir, friendlyDir, unrelated} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	m := newJobManagerWithStatePath(Config{}, nil, filepath.Join(root, "state"))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	writer := &Job{ID: "writer", Repo: "owner/model", OutputDir: cacheDir, HubDir: hubDir, Status: JobStatusRunning}
	m.runActivities[1] = &jobWriteActivity{job: writer, planned: map[string]frozenMutationPath{}}

	// Each destination of the whole-repo delete is refused on its own: the Hub
	// repository directory and the friendly output path.
	if _, ok := m.beginCacheMutation(repoDir); ok {
		t.Fatal("Hub mutation admitted while the plan is unknown and a writer owns the repository")
	}
	if _, ok := m.beginCacheMutation(friendlyDir); ok {
		t.Fatal("friendly mutation admitted while the plan is unknown and a writer owns the destination")
	}
	if _, ok := m.beginCacheMutation(repoDir, friendlyDir); ok {
		t.Fatal("directory mutation admitted while the plan is unknown and a writer owns the destination")
	}
	if release, ok := m.beginCacheMutation(unrelated); !ok {
		t.Fatal("unrelated directory mutation blocked by the active writer")
	} else {
		release()
	}

	// Branch isolation: this activity's own destination and repository resolve
	// elsewhere, so only the frozen-plan rule can refuse the destination below.
	frozen, err := freezeMutationEntry(weights)
	if err != nil {
		t.Fatal(err)
	}
	frozen.raw = weights
	elsewhere := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	delete(m.runActivities, 1)
	m.runActivities[2] = &jobWriteActivity{
		job:          &Job{ID: "planned", Repo: "other/repo", OutputDir: elsewhere, HubDir: filepath.Join(elsewhere, "hub"), Status: JobStatusRunning},
		planKnown:    true,
		planComplete: true,
		planned:      map[string]frozenMutationPath{weights: frozen},
	}
	if _, ok := m.beginCacheMutation(friendlyDir); ok {
		t.Fatal("directory mutation admitted over a frozen plan entry outside the job's own destination")
	}
}

// The destructive window must gate the writer side too: while a whole-directory
// mutation owns a repository, a queued job for it is not dispatched (its
// activity would otherwise register after admission and be destroyed by the
// in-flight RemoveAll), the gate stays scoped to conflicting repositories, and
// the hold ends with the window.
func TestDispatchHoldsQueuedWriterDuringCacheMutation(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	repoDir := filepath.Join(cacheDir, "hub", "models--owner--model")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := newJobManagerWithStatePath(Config{CacheDir: cacheDir, MaxActive: 1}, nil, filepath.Join(root, "state"))
	defer m.Close(context.Background())
	conflicting := &Job{ID: "conflicting", Repo: "owner/model", OutputDir: cacheDir, Status: JobStatusQueued, CreatedAt: time.Now(), partialFilesMu: &sync.Mutex{}}
	unrelated := &Job{ID: "unrelated", Repo: "owner/other", OutputDir: cacheDir, Status: JobStatusQueued, CreatedAt: time.Now().Add(time.Second), partialFilesMu: &sync.Mutex{}}
	m.jobs[conflicting.ID], m.jobs[unrelated.ID] = conflicting, unrelated

	release, ok := m.beginCacheMutation(repoDir)
	if !ok {
		t.Fatal("destructive window was not admitted")
	}
	if !m.jobBlockedByDeleteLocked(conflicting) {
		t.Fatal("dispatch gate does not hold a queued writer for the mutating repository")
	}
	if m.jobBlockedByDeleteLocked(unrelated) {
		t.Fatal("unrelated queued writer held by a scoped mutation")
	}
	m.mu.Lock()
	m.dispatchLocked()
	activities := len(m.runActivities)
	m.mu.Unlock()
	if activities != 0 || conflicting.starting || conflicting.Status != JobStatusQueued {
		t.Fatalf("writer started inside the destructive window: activities=%d status=%s starting=%v", activities, conflicting.Status, conflicting.starting)
	}
	if !m.CancelJob(conflicting.ID) || !m.CancelJob(unrelated.ID) {
		t.Fatal("queued fixture jobs could not be cancelled")
	}
	release()
	if m.jobBlockedByDeleteLocked(conflicting) {
		t.Fatal("queued writer still held after the destructive window closed")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
