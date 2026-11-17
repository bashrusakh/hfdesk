// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

const activityTestTotal = int64(8 << 30)

// activityHarness drives applyJobProgress with a fake clock and explicit
// last-read/last-local-write instants, the only inputs the activity classifier
// consumes.
type activityHarness struct {
	t          *testing.T
	job        *Job
	now        time.Time
	downloaded map[string]int64
}

func newActivityHarness(t *testing.T, paths ...string) *activityHarness {
	t.Helper()
	h := &activityHarness{
		t:          t,
		job:        &Job{ID: "write-activity"},
		now:        time.Unix(1_700_000_000, 0),
		downloaded: make(map[string]int64),
	}
	for _, path := range paths {
		h.apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: path, Total: activityTestTotal})
		h.apply(hfdownloader.ProgressEvent{Event: "file_start", Path: path})
	}
	return h
}

func (h *activityHarness) apply(evt hfdownloader.ProgressEvent) {
	applyJobProgress(h.job, evt, h.now)
}

func (h *activityHarness) ago(d time.Duration) time.Time {
	return h.now.Add(-d)
}

// progress emits one transfer-progress sample at the current fake time.
func (h *activityHarness) progress(path string, lastRead, lastWrite time.Time) {
	h.downloaded[path] += 2 << 20
	h.apply(hfdownloader.ProgressEvent{
		Event:            "file_progress",
		Path:             path,
		Downloaded:       h.downloaded[path],
		Total:            activityTestTotal,
		LastReadAt:       lastRead,
		LastLocalWriteAt: lastWrite,
	})
}

// tick advances the fake clock by the downloader's 200 ms progress cadence and
// emits one sample.
func (h *activityHarness) tick(path string, lastRead, lastWrite time.Time) {
	h.now = h.now.Add(200 * time.Millisecond)
	h.progress(path, lastRead, lastWrite)
}

func (h *activityHarness) status(path string) string {
	h.t.Helper()
	for _, f := range h.job.Files {
		if f.Path == path {
			return f.Status
		}
	}
	h.t.Fatalf("file %q is not in the job", path)
	return ""
}

// TestApplyJobProgressClassifiesActivity pins the stateless activity rule: a
// fresh read means the network is delivering; with reads stalled, newer local
// progress means writing; silence alone never changes the state.
func TestApplyJobProgressClassifiesActivity(t *testing.T) {
	t.Run("recent read keeps the file active", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf")
		h.tick("a.gguf", h.ago(100*time.Millisecond), h.ago(10*time.Millisecond))

		if got := h.status("a.gguf"); got != "active" {
			t.Fatalf("file status = %q, want active", got)
		}
		if h.job.Phase != "" {
			t.Fatalf("phase = %q, want empty while the network delivers", h.job.Phase)
		}
	})

	t.Run("stale read with recent local write becomes writing", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf")
		h.tick("a.gguf", h.ago(10*time.Second), h.ago(100*time.Millisecond))

		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status = %q, want writing", got)
		}
		if h.job.Phase != "writing" {
			t.Fatalf("phase = %q, want writing for a single disk-bound file", h.job.Phase)
		}
	})

	t.Run("preparation with no reads is writing", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf")
		h.apply(hfdownloader.ProgressEvent{Event: "file_preparing", Path: "a.gguf"})
		h.tick("a.gguf", time.Time{}, h.ago(50*time.Millisecond))

		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status = %q, want writing during local preparation", got)
		}
		if h.job.Phase != "writing" {
			t.Fatalf("phase = %q, want writing during local preparation", h.job.Phase)
		}
	})

	t.Run("silence leaves the status unchanged", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf")
		h.tick("a.gguf", h.ago(10*time.Second), h.ago(100*time.Millisecond))
		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status = %q, want writing before silence", got)
		}

		h.now = h.now.Add(10 * time.Second)
		h.progress("a.gguf", h.ago(20*time.Second), h.ago(20*time.Second))
		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status after total silence = %q, want the previous writing state", got)
		}
	})

	t.Run("a long blocked write does not produce an active gap", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf")
		lastRead := h.ago(10 * time.Second)
		lastWrite := h.ago(100 * time.Millisecond)
		h.tick("a.gguf", lastRead, lastWrite)
		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status = %q, want writing", got)
		}

		// No completed write for 6 s: the local-write age crosses the 5 s
		// hold, but no read arrives, so the state must be held.
		for i := 0; i < 30; i++ {
			h.tick("a.gguf", lastRead, lastWrite)
			if got := h.status("a.gguf"); got != "writing" {
				t.Fatalf("tick %d after the write stalled: file status = %q, want writing", i, got)
			}
			if h.job.Phase != "writing" {
				t.Fatalf("tick %d after the write stalled: phase = %q, want writing", i, h.job.Phase)
			}
		}
	})

	t.Run("a read after writing returns active", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf")
		h.tick("a.gguf", h.ago(10*time.Second), h.ago(100*time.Millisecond))
		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status = %q, want writing", got)
		}

		h.tick("a.gguf", h.ago(20*time.Millisecond), h.ago(10*time.Millisecond))
		if got := h.status("a.gguf"); got != "active" {
			t.Fatalf("file status after the read = %q, want active", got)
		}
		if h.job.Phase != "" {
			t.Fatalf("phase = %q after the read, want empty", h.job.Phase)
		}
	})

	t.Run("mixed job keeps the phase empty", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf", "b.gguf")
		h.tick("a.gguf", h.ago(10*time.Second), h.ago(100*time.Millisecond))
		h.tick("b.gguf", h.ago(50*time.Millisecond), h.ago(100*time.Millisecond))

		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("disk-bound file status = %q, want writing", got)
		}
		if got := h.status("b.gguf"); got != "active" {
			t.Fatalf("network file status = %q, want active", got)
		}
		if h.job.Phase != "" {
			t.Fatalf("phase = %q, want empty while a file still pulls from the network", h.job.Phase)
		}
	})

	t.Run("last writing file finishing clears the phase", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf")
		h.tick("a.gguf", h.ago(10*time.Second), h.ago(100*time.Millisecond))
		if h.job.Phase != "writing" {
			t.Fatalf("phase = %q, want writing before completion", h.job.Phase)
		}
		h.apply(hfdownloader.ProgressEvent{Event: "file_done", Path: "a.gguf"})
		if h.job.Phase != "" {
			t.Fatalf("phase = %q after the last file finished, want empty", h.job.Phase)
		}
	})
}

// TestJobJSONExposesWritingFileStatus checks that the activity-derived status
// rides the normal job snapshot and that the event-only activity timestamps do
// not leak into it.
func TestJobJSONExposesWritingFileStatus(t *testing.T) {
	h := newActivityHarness(t, "a.gguf")
	h.tick("a.gguf", h.ago(10*time.Second), h.ago(100*time.Millisecond))

	data, err := json.Marshal(h.job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	var decoded struct {
		Phase string `json:"phase"`
		Files []struct {
			Path   string `json:"path"`
			Status string `json:"status"`
		} `json:"files"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal job JSON: %v", err)
	}
	if decoded.Phase != "writing" {
		t.Errorf("response phase = %q, want writing", decoded.Phase)
	}
	if len(decoded.Files) != 1 || decoded.Files[0].Status != "writing" {
		t.Errorf("response files = %+v, want one writing file", decoded.Files)
	}
	for _, eventOnly := range []string{"lastReadAt", "lastLocalWriteAt", "activityOnly"} {
		if strings.Contains(string(data), `"`+eventOnly+`"`) {
			t.Errorf("job JSON leaked event-only field %q: %s", eventOnly, data)
		}
	}
}
