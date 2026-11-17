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

// activitySample is one file_progress event as the downloader reports it.
type activitySample struct {
	downloaded   int64
	readBytes    int64
	lastRead     time.Time
	lastWrite    time.Time
	inFlight     bool
	activityOnly bool
}

// activityHarness drives applyJobProgress with a fake clock and explicit event
// fields, the only inputs the activity classifier consumes.
type activityHarness struct {
	t     *testing.T
	job   *Job
	now   time.Time
	total int64
}

func newActivityHarness(t *testing.T, paths ...string) *activityHarness {
	return newActivityHarnessTotal(t, activityTestTotal, paths...)
}

func newActivityHarnessTotal(t *testing.T, total int64, paths ...string) *activityHarness {
	t.Helper()
	h := &activityHarness{
		t:     t,
		job:   &Job{ID: "write-activity"},
		now:   time.Unix(1_700_000_000, 0),
		total: total,
	}
	for _, path := range paths {
		h.apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: path, Total: total})
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

func (h *activityHarness) sample(path string, s activitySample) {
	h.apply(hfdownloader.ProgressEvent{
		Event: "file_progress", Path: path, Downloaded: s.downloaded, Total: h.total,
		LastReadAt: s.lastRead, LastLocalWriteAt: s.lastWrite, ReadBytes: s.readBytes,
		LocalWorkInFlight: s.inFlight, ActivityOnly: s.activityOnly,
	})
}

func (h *activityHarness) tick(path string, s activitySample) {
	h.now = h.now.Add(200 * time.Millisecond)
	h.sample(path, s)
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
// fresh read means the network is delivering; only local work that is blocked
// in flight means writing; a completed write alone and a pure stall are not.
func TestApplyJobProgressClassifiesActivity(t *testing.T) {
	t.Run("recent read keeps the file active", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf")
		h.tick("a.gguf", activitySample{
			downloaded: 1 << 20, readBytes: 1 << 20,
			lastRead: h.ago(100 * time.Millisecond), lastWrite: h.ago(time.Second),
		})

		if got := h.status("a.gguf"); got != "active" {
			t.Fatalf("file status = %q, want active", got)
		}
		if h.job.Phase != "" {
			t.Fatalf("phase = %q, want empty while the network delivers", h.job.Phase)
		}
	})

	t.Run("a completed write alone stays active", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf")
		h.tick("a.gguf", activitySample{
			downloaded: 1 << 20, readBytes: 1 << 20,
			lastRead: h.ago(10 * time.Second), lastWrite: h.ago(100 * time.Millisecond),
		})

		if got := h.status("a.gguf"); got != "active" {
			t.Fatalf("file status = %q, want active: a completed write alone is not writing", got)
		}
		if h.job.Phase != "" {
			t.Fatalf("phase = %q, want empty", h.job.Phase)
		}
	})

	t.Run("preparation with local work in flight is writing", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf")
		h.apply(hfdownloader.ProgressEvent{Event: "file_preparing", Path: "a.gguf"})
		h.tick("a.gguf", activitySample{lastWrite: h.ago(time.Minute), inFlight: true, activityOnly: true})

		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status = %q, want writing for in-flight preparation", got)
		}
		if h.job.Phase != "writing" {
			t.Fatalf("phase = %q, want writing", h.job.Phase)
		}
	})

	t.Run("preparation without local work stays preparing", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf")
		h.apply(hfdownloader.ProgressEvent{Event: "file_preparing", Path: "a.gguf"})
		h.tick("a.gguf", activitySample{lastWrite: h.ago(time.Minute), activityOnly: true})

		if got := h.status("a.gguf"); got != "preparing" {
			t.Fatalf("file status = %q, want preparing while the claim waits", got)
		}
		if h.job.Phase != "preparing" {
			t.Fatalf("phase = %q, want preparing", h.job.Phase)
		}
	})

	t.Run("blocked write with local work in flight stays writing", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf")
		lastRead := h.ago(10 * time.Second)
		lastWrite := h.ago(100 * time.Millisecond)
		h.tick("a.gguf", activitySample{
			downloaded: 1 << 20, readBytes: 1 << 20,
			lastRead: lastRead, lastWrite: lastWrite, inFlight: true,
		})
		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status = %q, want writing", got)
		}

		for i := 0; i < 50; i++ { // 10 s with no completed write and no read
			h.tick("a.gguf", activitySample{
				downloaded: 1 << 20, readBytes: 1 << 20,
				lastRead: lastRead, lastWrite: lastWrite, inFlight: true,
			})
			if got := h.status("a.gguf"); got != "writing" {
				t.Fatalf("tick %d: file status = %q, want writing while the write is in flight", i, got)
			}
			if h.job.Phase != "writing" {
				t.Fatalf("tick %d: phase = %q, want writing", i, h.job.Phase)
			}
		}
	})

	t.Run("a read after writing returns active", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf")
		h.tick("a.gguf", activitySample{
			downloaded: 1 << 20, readBytes: 1 << 20,
			lastRead: h.ago(10 * time.Second), lastWrite: h.ago(time.Second), inFlight: true,
		})
		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status = %q, want writing", got)
		}

		h.tick("a.gguf", activitySample{
			downloaded: 2 << 20, readBytes: 2 << 20,
			lastRead: h.ago(20 * time.Millisecond), lastWrite: h.ago(time.Second),
		})
		if got := h.status("a.gguf"); got != "active" {
			t.Fatalf("file status after the read = %q, want active", got)
		}
		if h.job.Phase != "" {
			t.Fatalf("phase = %q after the read, want empty", h.job.Phase)
		}
	})

	t.Run("mixed job keeps the phase empty", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf", "b.gguf")
		h.tick("a.gguf", activitySample{
			downloaded: 1 << 20, readBytes: 1 << 20,
			lastRead: h.ago(10 * time.Second), lastWrite: h.ago(100 * time.Millisecond), inFlight: true,
		})
		h.tick("b.gguf", activitySample{
			downloaded: 1 << 20, readBytes: 1 << 20,
			lastRead: h.ago(50 * time.Millisecond), lastWrite: h.ago(100 * time.Millisecond),
		})

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
		h.tick("a.gguf", activitySample{
			downloaded: 1 << 20, readBytes: 1 << 20,
			lastRead: h.ago(10 * time.Second), lastWrite: h.ago(time.Second), inFlight: true,
		})
		if h.job.Phase != "writing" {
			t.Fatalf("phase = %q, want writing before completion", h.job.Phase)
		}
		h.apply(hfdownloader.ProgressEvent{Event: "file_done", Path: "a.gguf"})
		if h.job.Phase != "" {
			t.Fatalf("phase = %q after the last file finished, want empty", h.job.Phase)
		}
	})
}

// TestApplyJobProgressInFlightSelectsWriting is the discriminating case: an
// active (or pending) file with a stale completed write becomes writing only
// because local work is in flight. Removing the in-flight branch fails it.
func TestApplyJobProgressInFlightSelectsWriting(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		h := newActivityHarness(t, "a.gguf")
		h.tick("a.gguf", activitySample{
			lastRead: h.ago(time.Minute), lastWrite: h.ago(time.Minute), inFlight: true,
		})
		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status = %q, want writing from in-flight local work", got)
		}
	})

	t.Run("pending", func(t *testing.T) {
		h := &activityHarness{
			t:     t,
			job:   &Job{ID: "pending"},
			now:   time.Unix(1_700_000_000, 0),
			total: activityTestTotal,
		}
		h.apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: "a.gguf", Total: activityTestTotal})
		h.tick("a.gguf", activitySample{
			lastRead: h.ago(time.Minute), lastWrite: h.ago(time.Minute), inFlight: true,
		})
		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("file status = %q, want writing from in-flight local work", got)
		}
	})
}

// TestApplyJobProgressPureNetworkStallStaysActive covers the report that a read
// pause with nothing in flight was being reported as a disk write and held.
func TestApplyJobProgressPureNetworkStallStaysActive(t *testing.T) {
	h := newActivityHarness(t, "a.gguf")
	readAt := h.ago(100 * time.Millisecond)
	lastWrite := readAt.Add(time.Millisecond) // every chunk writes after its read
	h.sample("a.gguf", activitySample{
		downloaded: 1 << 20, readBytes: 1 << 20, lastRead: readAt, lastWrite: lastWrite,
	})
	if got := h.status("a.gguf"); got != "active" {
		t.Fatalf("file status after the read = %q, want active", got)
	}

	start := h.now
	for _, at := range []time.Duration{1600 * time.Millisecond, 10 * time.Second, 60 * time.Second} {
		h.now = start.Add(at)
		h.sample("a.gguf", activitySample{
			downloaded: 1 << 20, readBytes: 1 << 20, lastRead: readAt, lastWrite: lastWrite,
		})
		if got := h.status("a.gguf"); got != "active" {
			t.Fatalf("after %v of silence: file status = %q, want active", at, got)
		}
		if h.job.Phase != "" {
			t.Fatalf("after %v of silence: phase = %q, want empty", at, h.job.Phase)
		}
	}
}

// TestApplyJobProgressCreationInFlightKeepsWriting covers an 80 s preallocation
// with no network reads: writing throughout, speed 0, ETA 0.
func TestApplyJobProgressCreationInFlightKeepsWriting(t *testing.T) {
	h := newActivityHarness(t, "a.gguf")
	h.apply(hfdownloader.ProgressEvent{Event: "file_preparing", Path: "a.gguf"})
	lastWrite := h.now.Add(-time.Second)

	for i := 0; i < 400; i++ { // 80 s at the 200 ms progress cadence
		h.tick("a.gguf", activitySample{lastWrite: lastWrite, inFlight: true, activityOnly: true})
		if i%40 != 0 && i != 399 {
			continue
		}
		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("tick %d: file status = %q, want writing for the whole creation window", i, got)
		}
		if h.job.Phase != "writing" {
			t.Fatalf("tick %d: phase = %q, want writing", i, h.job.Phase)
		}
		if h.job.Progress.BytesPerSecond != 0 {
			t.Fatalf("tick %d: bytesPerSecond = %d, want 0 from the read counter", i, h.job.Progress.BytesPerSecond)
		}
		if h.job.Progress.EtaSeconds != 0 {
			t.Fatalf("tick %d: etaSeconds = %d, want 0 while no reads arrive", i, h.job.Progress.EtaSeconds)
		}
	}
}

// TestApplyJobProgressEtaStartsAtFirstRead checks that creation time never
// starts the transfer clock or enters the ETA average.
func TestApplyJobProgressEtaStartsAtFirstRead(t *testing.T) {
	const total = int64(20 << 20)
	job := &Job{ID: "first-read"}
	now := time.Unix(1_700_000_000, 0)
	apply := func(evt hfdownloader.ProgressEvent) { applyJobProgress(job, evt, now) }

	apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: "a.gguf", Total: total})
	apply(hfdownloader.ProgressEvent{Event: "file_start", Path: "a.gguf"})
	apply(hfdownloader.ProgressEvent{Event: "file_preparing", Path: "a.gguf"})
	lastWrite := now.Add(-time.Second)

	// 80 s of local creation with no reads.
	for i := 0; i < 400; i++ {
		now = now.Add(200 * time.Millisecond)
		apply(hfdownloader.ProgressEvent{
			Event: "file_progress", Path: "a.gguf", Total: total,
			LastLocalWriteAt: lastWrite, LocalWorkInFlight: true, ActivityOnly: true,
		})
	}
	if len(job.speedSamples) != 0 || !job.speedRunStart.IsZero() {
		t.Fatalf("creation started the transfer clock: %d samples, start %v", len(job.speedSamples), job.speedRunStart)
	}

	// First read: active, and the speed clock starts here.
	now = now.Add(200 * time.Millisecond)
	apply(hfdownloader.ProgressEvent{
		Event: "file_progress", Path: "a.gguf", Total: total,
		Downloaded: 1000, ReadBytes: 1000,
		LastReadAt: now, LastLocalWriteAt: lastWrite,
	})
	if job.Files[0].Status != "active" {
		t.Fatalf("file status after the first read = %q, want active", job.Files[0].Status)
	}
	if len(job.speedSamples) != 1 || !job.speedRunStart.Equal(now) {
		t.Fatalf("speed clock = %d samples starting %v, want one sample at %v",
			len(job.speedSamples), job.speedRunStart, now)
	}
	if job.Progress.BytesPerSecond != 0 || job.Progress.EtaSeconds != 0 {
		t.Fatalf("speed/ETA after the first read = %d B/s, %d s, want 0", job.Progress.BytesPerSecond, job.Progress.EtaSeconds)
	}

	// 5 s of reads at 2 MB/s; committed bytes advance ten times slower.
	readBytes, downloaded := int64(1000), int64(1000)
	for i := 0; i < 25; i++ {
		now = now.Add(200 * time.Millisecond)
		readBytes += 400_000
		downloaded += 100_000
		apply(hfdownloader.ProgressEvent{
			Event: "file_progress", Path: "a.gguf", Total: total,
			Downloaded: downloaded, ReadBytes: readBytes,
			LastReadAt: now, LastLocalWriteAt: lastWrite,
		})
	}
	if speed := job.Progress.BytesPerSecond; speed < 1_500_000 || speed > 2_500_000 {
		t.Fatalf("bytesPerSecond = %d, want the ~2 MB/s read rate", speed)
	}
	// Remaining ~17.5 MB at 2 MB/s is about 9 s; folding in the 80 s creation
	// window would put this in the hundreds of seconds.
	if eta := job.Progress.EtaSeconds; eta <= 0 || eta > 30 {
		t.Fatalf("etaSeconds = %d, want a transfer-only estimate (<= 30 s)", eta)
	}
}

// TestApplyJobProgressBlockedWriteInFlightKeepsWriting checks that a write that
// blocks for 10 s stays writing instead of ageing out of a staleness window.
func TestApplyJobProgressBlockedWriteInFlightKeepsWriting(t *testing.T) {
	h := newActivityHarness(t, "a.gguf")
	lastRead := h.ago(10 * time.Second)
	lastWrite := h.ago(100 * time.Millisecond)

	for i := 0; i < 50; i++ { // 10 s with no completed write and no read
		h.tick("a.gguf", activitySample{
			downloaded: 1 << 20, readBytes: 1 << 20,
			lastRead: lastRead, lastWrite: lastWrite, inFlight: true,
		})
		if got := h.status("a.gguf"); got != "writing" {
			t.Fatalf("tick %d: file status = %q, want writing while the write is in flight", i, got)
		}
		if h.job.Phase != "writing" {
			t.Fatalf("tick %d: phase = %q, want writing", i, h.job.Phase)
		}
	}
}

// TestApplyJobProgressSpeedUsesReadBytes checks that committed writes do not
// drive the transfer rate: only response reads do.
func TestApplyJobProgressSpeedUsesReadBytes(t *testing.T) {
	h := newActivityHarness(t, "a.gguf")
	lastWrite := h.ago(100 * time.Millisecond)
	readBytes, downloaded := int64(0), int64(0)

	for i := 0; i < 12; i++ {
		h.now = h.now.Add(400 * time.Millisecond)
		readBytes += 800_000 // 2 MB/s
		downloaded += 20_000 // 50 KB/s committed
		h.sample("a.gguf", activitySample{
			downloaded: downloaded, readBytes: readBytes,
			lastRead: h.now, lastWrite: lastWrite,
		})
	}
	if speed := h.job.Progress.BytesPerSecond; speed < 1_500_000 || speed > 2_500_000 {
		t.Fatalf("bytesPerSecond = %d, want the ~2 MB/s read rate, not 50 KB/s committed", speed)
	}

	// Reads stop; committed bytes keep advancing. The displayed rate must fall
	// away from the committed rate rather than follow it. The 10 s speed
	// window needs to age out the last read samples first.
	for i := 0; i < 30; i++ {
		h.now = h.now.Add(400 * time.Millisecond)
		downloaded += 80_000
		h.sample("a.gguf", activitySample{
			downloaded: downloaded, readBytes: readBytes,
			lastRead: h.now.Add(-10 * time.Second), lastWrite: lastWrite,
		})
	}
	if speed := h.job.Progress.BytesPerSecond; speed > 200_000 {
		t.Fatalf("bytesPerSecond = %d, want the rate to fall when reads stop", speed)
	}
}

// TestApplyJobProgressMixedJobKeepsSpeedAcrossTransition checks that one file's
// writing→active transition does not clear another file's aggregate speed/ETA.
func TestApplyJobProgressMixedJobKeepsSpeedAcrossTransition(t *testing.T) {
	const total = int64(100 << 20)
	h := newActivityHarnessTotal(t, total, "a.gguf", "b.gguf")

	// File b transfers at 2 MB/s for 5 s.
	var readB, downloadedB int64
	for i := 0; i < 25; i++ {
		h.now = h.now.Add(200 * time.Millisecond)
		readB += 400_000
		downloadedB += 400_000
		h.sample("b.gguf", activitySample{
			downloaded: downloadedB, readBytes: readB,
			lastRead: h.now, lastWrite: h.ago(time.Second),
		})
	}
	speedBefore := h.job.Progress.BytesPerSecond
	etaBefore := h.job.Progress.EtaSeconds
	if speedBefore < 1_500_000 {
		t.Fatalf("pre-transition speed = %d, want the ~2 MB/s aggregate", speedBefore)
	}
	if etaBefore <= 0 {
		t.Fatalf("pre-transition ETA = %d, want a positive estimate", etaBefore)
	}

	// File a leaves creation: in-flight first, then its first read.
	h.apply(hfdownloader.ProgressEvent{Event: "file_preparing", Path: "a.gguf"})
	h.now = h.now.Add(200 * time.Millisecond)
	h.sample("a.gguf", activitySample{lastWrite: h.ago(time.Minute), inFlight: true, activityOnly: true})
	if got := h.status("a.gguf"); got != "writing" {
		t.Fatalf("file a status during creation = %q, want writing", got)
	}
	h.now = h.now.Add(200 * time.Millisecond)
	h.sample("a.gguf", activitySample{
		readBytes: 1000, lastRead: h.now, lastWrite: h.ago(time.Minute),
	})
	if got := h.status("a.gguf"); got != "active" {
		t.Fatalf("file a status after its first read = %q, want active", got)
	}

	if speed := h.job.Progress.BytesPerSecond; speed < speedBefore/2 || speed == 0 {
		t.Fatalf("post-transition speed = %d, want it to stay near %d", speed, speedBefore)
	}
	if eta := h.job.Progress.EtaSeconds; eta <= 0 || eta < etaBefore/2 {
		t.Fatalf("post-transition ETA = %d, want it to stay near %d", eta, etaBefore)
	}
}

// TestApplyJobProgressBlockedWriteResumeKeepsEta checks that a blocked write
// followed by resumed reads does not reset the rate history and collapse the
// ETA onto a fresh clock.
func TestApplyJobProgressBlockedWriteResumeKeepsEta(t *testing.T) {
	const total = int64(100 << 20)
	h := newActivityHarnessTotal(t, total, "a.gguf")

	// 20 s of reads at 2 MB/s.
	var readBytes, downloaded int64
	for i := 0; i < 100; i++ {
		h.now = h.now.Add(200 * time.Millisecond)
		readBytes += 400_000
		downloaded += 400_000
		h.sample("a.gguf", activitySample{
			downloaded: downloaded, readBytes: readBytes,
			lastRead: h.now, lastWrite: h.ago(time.Second),
		})
	}
	etaBefore := h.job.Progress.EtaSeconds
	if etaBefore < 20 {
		t.Fatalf("pre-block ETA = %d, want a normal transfer estimate", etaBefore)
	}

	// 10 s blocked write: no reads, local work in flight. The 1.5 s read hold
	// keeps the file active for the first few samples, then in-flight local
	// work selects writing for the rest of the block.
	lastRead := h.now
	lastWrite := h.now.Add(-time.Second)
	for i := 0; i < 50; i++ {
		h.now = h.now.Add(200 * time.Millisecond)
		h.sample("a.gguf", activitySample{
			downloaded: downloaded, readBytes: readBytes,
			lastRead: lastRead, lastWrite: lastWrite, inFlight: true,
		})
		readAge := h.now.Sub(lastRead)
		if readAge > activityReadHold {
			if got := h.status("a.gguf"); got != "writing" {
				t.Fatalf("blocked tick %d (read age %v): file status = %q, want writing", i, readAge, got)
			}
		}
	}

	// Resume reads for 8 s at 2 MB/s; the 10 s speed window needs to refill.
	for i := 0; i < 40; i++ {
		h.now = h.now.Add(200 * time.Millisecond)
		readBytes += 400_000
		downloaded += 400_000
		h.sample("a.gguf", activitySample{
			downloaded: downloaded, readBytes: readBytes,
			lastRead: h.now, lastWrite: h.now.Add(-time.Second),
		})
	}
	if got := h.status("a.gguf"); got != "active" {
		t.Fatalf("file status after resume = %q, want active", got)
	}
	if speed := h.job.Progress.BytesPerSecond; speed < 1_000_000 {
		t.Fatalf("post-resume speed = %d, want a recovering transfer rate", speed)
	}
	// A job-wide baseline reset on the transition would divide the whole
	// cumulative read count by the resume window and collapse this far below
	// the full-run estimate.
	if eta := h.job.Progress.EtaSeconds; eta <= 10 {
		t.Fatalf("post-resume ETA = %d, want a full-run estimate (not a reset clock)", eta)
	}
}

// TestJobJSONExposesWritingFileStatus checks that the activity-derived status
// rides the normal job snapshot and that the event-only activity fields do not
// leak into it.
func TestJobJSONExposesWritingFileStatus(t *testing.T) {
	h := newActivityHarness(t, "a.gguf")
	h.tick("a.gguf", activitySample{
		downloaded: 1 << 20, readBytes: 1 << 20,
		lastRead: h.ago(10 * time.Second), lastWrite: h.ago(time.Minute), inFlight: true,
	})

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
	for _, eventOnly := range []string{"lastReadAt", "lastLocalWriteAt", "readBytes", "localWorkInFlight", "activityOnly"} {
		if strings.Contains(string(data), `"`+eventOnly+`"`) {
			t.Errorf("job JSON leaked event-only field %q: %s", eventOnly, data)
		}
	}
}
