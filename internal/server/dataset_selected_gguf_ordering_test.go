package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// This fixture deliberately holds the real dataset prescan before its tree is
// returned. The selected-entry reservation must make the same correct decision
// whether the public writer or reservation arrives first. The reserved leaf is
// absent; this checks admission precision, not deletion of an existing group.
func TestPlanUnknownDatasetGGUFSelectedDeleteOrdering(t *testing.T) {
	for _, ordering := range []string{"writer-first", "delete-first"} {
		t.Run(ordering, func(t *testing.T) {
			for _, tc := range []struct {
				name        string
				reservation string
				wantAdmit   bool
			}{
				{name: "unmatched selected GGUF", reservation: "model-test.gguf", wantAdmit: true},
				{name: "matched target control", reservation: "model-train.gguf", wantAdmit: false},
				{name: "unrelated target control", reservation: "owner/other/other.gguf", wantAdmit: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					storage := t.TempDir()
					treeStarted := make(chan struct{}, 1)
					releaseTree := make(chan struct{})
					rawStarted := make(chan struct{}, 1)
					releaseRaw := make(chan struct{})
					var releaseTreeOnce, releaseRawOnce sync.Once
					openTree := func() { releaseTreeOnce.Do(func() { close(releaseTree) }) }
					openRaw := func() { releaseRawOnce.Do(func() { close(releaseRaw) }) }
					t.Cleanup(func() { openTree(); openRaw() })
					hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch {
						case strings.Contains(r.URL.Path, "/revision/"):
							_, _ = w.Write([]byte(`{"sha":"deadbeef"}`))
						case strings.Contains(r.URL.Path, "/tree/"):
							select {
							case treeStarted <- struct{}{}:
							default:
							}
							select {
							case <-releaseTree:
								_, _ = fmt.Fprint(w, `[{"type":"file","path":"train.csv","size":5},{"type":"file","path":"model-test.gguf","size":5},{"type":"file","path":"model-train.gguf","size":5}]`)
							case <-r.Context().Done():
							}
						case strings.Contains(r.URL.Path, "/raw/") || strings.Contains(r.URL.Path, "/resolve/"):
							select {
							case rawStarted <- struct{}{}:
							default:
							}
							select {
							case <-releaseRaw:
							case <-r.Context().Done():
								return
							}
							_, _ = w.Write([]byte("data!"))
						default:
							http.NotFound(w, r)
						}
					}))
					t.Cleanup(hub.Close)

					srv := newTestServerWithConfig(t, Config{CacheDir: storage, Endpoint: hub.URL})
					mux := http.NewServeMux()
					srv.registerAPIRoutes(mux)
					// Derive the target from the same frozen cache destination owner
					// used by production admission; no assumed cache directory layout.
					writer := &Job{OutputDir: storage, Repo: "owner/data", LocalRepo: "owner/data", IsDataset: true}
					target := filepath.Join(jobDestinationBase(writer), tc.reservation)
					if tc.name == "unrelated target control" {
						other, err := hfdownloader.NewHFCache(storage).Repo("owner/other", hfdownloader.RepoTypeDataset)
						if err != nil {
							t.Fatal(err)
						}
						target = filepath.Join(other.FriendlyPath(), "other.gguf")
					}
					post := func() *httptest.ResponseRecorder {
						t.Helper()
						body, err := json.Marshal(DownloadRequest{Repo: "owner/data", Dataset: true, Filters: []string{"train", "model-train.gguf"}, ExactMatch: true, AppendFilterSubdir: false})
						if err != nil {
							t.Fatal(err)
						}
						response := httptest.NewRecorder()
						mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/download", bytes.NewReader(body)))
						return response
					}

					var jobID string
					var releaseDelete func()
					var admissionProblem string
					if ordering == "writer-first" {
						response := post()
						if response.Code != http.StatusAccepted {
							t.Fatalf("public request status=%d body=%s", response.Code, response.Body.String())
						}
						var job Job
						if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
							t.Fatal(err)
						}
						writer = &job
						assertFrozenDatasetDestination(t, &job)
						jobID = job.ID
						select {
						case <-treeStarted:
						case <-time.After(5 * time.Second):
							t.Fatal("real dataset writer did not reach held tree prescan")
						}
						srv.jobs.mu.Lock()
						activityFound := false
						for _, activity := range srv.jobs.runActivities {
							if activity.job.ID == jobID {
								activityFound = true
								if activity.planKnown {
									srv.jobs.mu.Unlock()
									t.Fatal("tree gate was reached after the write plan became known")
								}
							}
						}
						srv.jobs.mu.Unlock()
						if !activityFound {
							t.Fatal("held tree prescan had no registered production run activity")
						}
						var ok bool
						releaseDelete, ok = srv.jobs.reserveSelectedGGUF([]string{target})
						if ok {
							originalRelease := releaseDelete
							var once sync.Once
							releaseDelete = func() { once.Do(originalRelease) }
							t.Cleanup(releaseDelete)
						}
						if ok != tc.wantAdmit {
							if ok {
								releaseDelete()
							}
							admissionProblem = fmt.Sprintf("reservation admitted=%v, want %v while plan unknown", ok, tc.wantAdmit)
							t.Errorf("%s", admissionProblem)
						}
						openTree()
					} else {
						var ok bool
						releaseDelete, ok = srv.jobs.reserveSelectedGGUF([]string{target})
						if !ok {
							t.Fatal("could not establish delete-first selected-entry reservation")
						}
						originalRelease := releaseDelete
						var once sync.Once
						releaseDelete = func() { once.Do(originalRelease) }
						t.Cleanup(releaseDelete)
						response := post()
						admitted := response.Code == http.StatusAccepted
						if admitted != tc.wantAdmit {
							admissionProblem = fmt.Sprintf("public request admitted=%v (status=%d body=%s), want %v", admitted, response.Code, response.Body.String(), tc.wantAdmit)
							t.Errorf("%s", admissionProblem)
						}
						if admitted {
							var job Job
							if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
								releaseDelete()
								t.Fatal(err)
							}
							writer = &job
							assertFrozenDatasetDestination(t, &job)
							jobID = job.ID
						} else if tc.wantAdmit {
							releaseDelete()
							releaseDelete = nil
							retry := post()
							if retry.Code != http.StatusAccepted {
								t.Fatalf("public retry after reservation release status=%d body=%s", retry.Code, retry.Body.String())
							}
							var job Job
							if err := json.Unmarshal(retry.Body.Bytes(), &job); err != nil {
								t.Fatal(err)
							}
							writer = &job
							assertFrozenDatasetDestination(t, &job)
							jobID = job.ID
						}
						openTree()
					}
					if releaseDelete != nil {
						releaseDelete()
					}
					// Ensure every admitted runner is cancelled/unwound even if any
					// assertion below fails. Close is also registered by test setup.
					if jobID != "" {
						t.Cleanup(func() { srv.jobs.CancelJob(jobID) })
					}

					if jobID != "" {
						select {
						case <-rawStarted:
						case <-time.After(5 * time.Second):
							t.Fatal("dataset writer did not reach raw transfer after planning")
						}
						actual := map[string]bool{}
						srv.jobs.mu.Lock()
						for _, activity := range srv.jobs.runActivities {
							if activity.job.ID == jobID && activity.planKnown {
								for _, path := range activity.planned {
									actual[path.physical] = true
								}
							}
						}
						srv.jobs.mu.Unlock()
						if len(actual) == 0 {
							t.Fatal("could not observe actual frozen plan destinations")
						}
						t.Logf("actual frozen plan destinations: %v", actual)
						if tc.name == "unmatched selected GGUF" {
							base := jobDestinationBase(writer)
							selected := filepath.Join(base, "model-test.gguf")
							if actual[selected] {
								t.Errorf("plan emitted selected unmatched target %q; actual frozen destinations=%v", selected, actual)
							}
							if !actual[filepath.Join(base, "train.csv")] || !actual[filepath.Join(base, "model-train.gguf")] {
								t.Fatalf("actual frozen plan omitted expected dataset/train GGUF destinations: %v", actual)
							}
						}
						openRaw()
						deadline := time.After(8 * time.Second)
						for {
							job, ok := srv.jobs.GetJob(jobID)
							if ok && (job.Status == JobStatusCompleted || job.Status == JobStatusFailed || job.Status == JobStatusCancelled) {
								if job.Status != JobStatusCompleted {
									t.Fatalf("dataset run ended %s: %s", job.Status, job.Error)
								}
								break
							}
							select {
							case <-deadline:
								t.Fatal("dataset writer did not finish after releasing raw transfer")
							case <-time.After(10 * time.Millisecond):
							}
						}
					}
					ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
					defer cancel()
					if err := srv.jobs.Close(ctx); err != nil {
						t.Fatalf("close job manager: %v", err)
					}
				})
			}
		})
	}
}

func assertFrozenDatasetDestination(t *testing.T, job *Job) {
	t.Helper()
	repoID := job.Repo
	if job.LocalRepo != "" {
		repoID = job.LocalRepo
	}
	repo, err := hfdownloader.NewHFCacheResolved(job.OutputDir, job.HubDir).Repo(repoID, hfdownloader.RepoTypeDataset)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := jobDestinationBase(job), repo.FriendlyPath(); got != want {
		t.Fatalf("frozen job destination base=%q, cache dataset destination=%q", got, want)
	}
}
