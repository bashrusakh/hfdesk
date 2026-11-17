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

const writePhaseTestTotal = int64(8 << 30)

// writePhaseHarness feeds synthetic file_progress intervals at the
// downloader's 200 ms cadence. Each tick advances wall time by one interval
// and the file's cumulative WriteWaitNanos by wait, exactly the series a
// network-bound (small wait) or disk-bound (wait approaches the interval)
// transfer produces.
type writePhaseHarness struct {
	t          *testing.T
	job        *Job
	now        time.Time
	wait       map[string]int64
	downloaded map[string]int64
}

func newWritePhaseHarness(t *testing.T, paths ...string) *writePhaseHarness {
	t.Helper()
	h := &writePhaseHarness{
		t:          t,
		job:        &Job{ID: "write-phase"},
		now:        time.Unix(1_700_000_000, 0),
		wait:       make(map[string]int64),
		downloaded: make(map[string]int64),
	}
	for _, path := range paths {
		h.apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: path, Total: writePhaseTestTotal})
	}
	for _, path := range paths {
		h.apply(hfdownloader.ProgressEvent{Event: "file_start", Path: path})
	}
	return h
}

func (h *writePhaseHarness) apply(evt hfdownloader.ProgressEvent) {
	applyJobProgress(h.job, evt, h.now)
}

// tick advances one 200 ms sampling interval for path and adds wait to its
// cumulative local write time.
func (h *writePhaseHarness) tick(path string, wait time.Duration) {
	h.now = h.now.Add(200 * time.Millisecond)
	h.wait[path] += int64(wait)
	h.downloaded[path] += 2 << 20
	h.apply(hfdownloader.ProgressEvent{
		Event:          "file_progress",
		Path:           path,
		Downloaded:     h.downloaded[path],
		Total:          writePhaseTestTotal,
		WriteWaitNanos: h.wait[path],
	})
}

func (h *writePhaseHarness) ticks(path string, n int, wait time.Duration) {
	for i := 0; i < n; i++ {
		h.tick(path, wait)
	}
}

func (h *writePhaseHarness) status(path string) string {
	h.t.Helper()
	for _, f := range h.job.Files {
		if f.Path == path {
			return f.Status
		}
	}
	h.t.Fatalf("file %q is not in the job", path)
	return ""
}

// TestApplyJobProgressClassifiesLocalWritePhase pins the per-file classifier
// and its job-wide projection against synthetic cumulative write-wait series.
func TestApplyJobProgressClassifiesLocalWritePhase(t *testing.T) {
	t.Run("network bound stays active", func(t *testing.T) {
		h := newWritePhaseHarness(t, "a.gguf")
		h.ticks("a.gguf", 10, 10*time.Millisecond)

		if got := h.status("a.gguf"); got != "active" {
			t.Fatalf("file status = %q, want active", got)
		}
		if h.job.Phase != "" {
			t.Fatalf("phase = %q, want empty while the network is the bottleneck", h.job.Phase)
		}
	})

	t.Run("disk bound becomes writing and sets the phase", func(t *testing.T) {
		h := newWritePhaseHarness(t, "a.gguf")
		h.ticks("a.gguf", 8, 200*time.Millisecond)

		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status = %q, want writing", got)
		}
		if h.job.Phase != "writing" {
			t.Fatalf("phase = %q, want writing for a single disk-bound file", h.job.Phase)
		}
		// bytesPerSecond/etaSeconds keep their byte-rate meaning while writes
		// dominate; the UI hides them, the API does not falsify them.
		if h.job.Progress.BytesPerSecond <= 0 {
			t.Fatalf("bytesPerSecond = %d, want the real advancing rate", h.job.Progress.BytesPerSecond)
		}
		if h.job.Progress.EtaSeconds <= 0 {
			t.Fatalf("etaSeconds = %d, want the real remaining-time estimate", h.job.Progress.EtaSeconds)
		}
	})

	t.Run("hysteresis prevents flapping on an oscillating series", func(t *testing.T) {
		h := newWritePhaseHarness(t, "a.gguf")
		h.tick("a.gguf", 200*time.Millisecond)
		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status after the first saturated interval = %q, want writing", got)
		}

		transitions := 0
		prev := "writing"
		for i := 0; i < 22; i++ {
			wait := time.Duration(0)
			if i%2 == 0 {
				wait = 200 * time.Millisecond
			}
			h.tick("a.gguf", wait)
			got := h.status("a.gguf")
			if got != prev {
				transitions++
				prev = got
			}
			if h.job.Phase != "writing" {
				t.Fatalf("interval %d: phase = %q, want writing throughout the oscillation", i, h.job.Phase)
			}
		}
		if transitions != 0 {
			t.Fatalf("oscillating series changed the file status %d times, want no flap", transitions)
		}
	})

	t.Run("minimum dwell holds writing through a short lull", func(t *testing.T) {
		h := newWritePhaseHarness(t, "a.gguf")
		h.tick("a.gguf", 200*time.Millisecond)
		h.ticks("a.gguf", 4, 0)
		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status after 0.8s of lull = %q, want writing inside the dwell", got)
		}
		h.tick("a.gguf", 0)
		if got := h.status("a.gguf"); got != "active" {
			t.Fatalf("file status after the dwell = %q, want active", got)
		}
		if h.job.Phase != "" {
			t.Fatalf("phase = %q, want empty once writes stop blocking", h.job.Phase)
		}
	})

	t.Run("stalled then resumed returns to active without flapping", func(t *testing.T) {
		h := newWritePhaseHarness(t, "a.gguf")
		h.tick("a.gguf", 200*time.Millisecond)
		h.ticks("a.gguf", 5, 0)
		if got := h.status("a.gguf"); got != "active" {
			t.Fatalf("file status after the stall = %q, want active", got)
		}
		if h.job.Phase != "" {
			t.Fatalf("phase = %q after the stall, want empty", h.job.Phase)
		}

		// One saturated interval is not enough to re-enter after the smoothed
		// share has decayed; a sustained series is.
		h.tick("a.gguf", 200*time.Millisecond)
		if got := h.status("a.gguf"); got != "active" {
			t.Fatalf("file status after one resumed interval = %q, want active", got)
		}
		h.tick("a.gguf", 200*time.Millisecond)
		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status after sustained writes = %q, want writing", got)
		}
	})

	t.Run("mixed job keeps the phase empty and only the file writing", func(t *testing.T) {
		h := newWritePhaseHarness(t, "a.gguf", "b.gguf")
		for i := 0; i < 8; i++ {
			h.tick("a.gguf", 200*time.Millisecond)
			h.tick("b.gguf", 10*time.Millisecond)
		}

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
		h := newWritePhaseHarness(t, "a.gguf")
		h.ticks("a.gguf", 8, 200*time.Millisecond)
		if h.job.Phase != "writing" {
			t.Fatalf("phase = %q, want writing before completion", h.job.Phase)
		}
		h.apply(hfdownloader.ProgressEvent{Event: "file_done", Path: "a.gguf"})
		if h.job.Phase != "" {
			t.Fatalf("phase = %q after the last file finished, want empty", h.job.Phase)
		}
	})
}

// TestJobJSONExposesLocalWritePhaseWithoutTimingState checks that the writing
// status rides the normal job snapshot while the classifier's private timing
// state stays out of it.
func TestJobJSONExposesLocalWritePhaseWithoutTimingState(t *testing.T) {
	h := newWritePhaseHarness(t, "a.gguf")
	h.ticks("a.gguf", 8, 200*time.Millisecond)

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
	for _, private := range []string{"writeShare", "writeWaitTotal", "writeSampleAt", "writingSince"} {
		if strings.Contains(string(data), `"`+private+`"`) {
			t.Errorf("job JSON leaked private timing state %q: %s", private, data)
		}
	}
}
