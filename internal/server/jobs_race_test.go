// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"sync"
	"testing"
	"time"
)

// TestJobManager_CloneJobPartialFilesSync exercises the lock discipline for
// Job.partialFilesPtr: cloneJobLocked's whole-struct copy reads the pointer
// while runJob/cleanupPausedJobPartFiles/OnPartialFile write it under the
// per-job partialFilesMu. Before github issue #77's fix, the read inside
// `clone := *j` happened outside partialFilesMu, so this test reports a
// DATA RACE under -race while GetJob/ListJobs run against the writer.
//
// The test is hermetic: the job is inserted directly into m.jobs (the way
// routes_test.go does) and no downloader/network path runs. The writer
// mirrors runJob's pointer swap plus OnPartialFile's map mutation under
// partialFilesMu; the readers mirror GET /api/jobs and GET /api/jobs/{id}.
func TestJobManager_CloneJobPartialFilesSync(t *testing.T) {
	mgr := NewJobManager(Config{}, nil)

	job := &Job{
		ID:             "race-partial-files",
		Repo:           "race/partial-files",
		Status:         JobStatusRunning,
		CreatedAt:      time.Now(),
		partialFilesMu: &sync.Mutex{},
	}
	mgr.mu.Lock()
	mgr.jobs[job.ID] = job
	mgr.mu.Unlock()

	// stop is closed only after every reader and the writer have finished
	// their loop bodies, so no goroutine keeps touching job state after the
	// test returns.
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer: reassign partialFilesPtr and mutate the pointed-to map under
	// partialFilesMu, exactly like runJob's reset followed by the
	// OnPartialFile callback.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			newMap := map[string]struct{}{"blob-1": {}}
			job.partialFilesMu.Lock()
			job.partialFilesPtr = &newMap
			(*job.partialFilesPtr)["blob-2"] = struct{}{}
			delete(*job.partialFilesPtr, "blob-2")
			job.partialFilesMu.Unlock()
		}
	}()

	// Readers: snapshot via ListJobs and GetJob, and read the clone's own
	// deep-copied tracker (the clone must not share the live map).
	readers := 4
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, snap := range mgr.ListJobs() {
					if snap.ID == job.ID && snap.partialFilesPtr != nil {
						if len(*snap.partialFilesPtr) == 0 {
							t.Error("clone tracker should not be empty")
						}
					}
				}
				if snap, ok := mgr.GetJob(job.ID); ok && snap.partialFilesPtr != nil {
					_ = len(*snap.partialFilesPtr)
				}
			}
		}()
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}
