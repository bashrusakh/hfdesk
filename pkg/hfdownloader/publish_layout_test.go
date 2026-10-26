// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the publication invariant of the ranged route
// (issue #140 / PR #151, correction package B):
//
//   - publish() asserts, explicitly, that the range layout of the download
//     covers the whole expected payload - every range Done and the committed
//     total equal to the expected size - before it renames the staging
//     payload onto the publish target;
//   - an incomplete layout is refused with errStagingLayoutIncomplete and
//     nothing is published: the staged bytes stay on disk for a resume;
//   - a COMPLETE layout still publishes, so the guard does not over-refuse.
//
// The cases construct the stagingDownload through the package's own test
// seams (the claim record, the ownership lock and the layout) and call
// publish() directly. That is deterministic: no network, no timing, no
// sleeps.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// completeRangeLayout returns the range layout of a fully transferred
// payload: every range is Done and committed in full.
func completeRangeLayout(size int64, n int) []RangeProgress {
	ranges := buildRangeLayout(size, n)
	for i := range ranges {
		ranges[i].Committed = ranges[i].End - ranges[i].Start + 1
		ranges[i].Done = true
	}
	return ranges
}

// publishLayoutCase describes one seeded staging download and the layout the
// publication must refuse.
type publishLayoutCase struct {
	name string
	// live is the in-memory layout of the download.
	live []RangeProgress
	// record is the persisted claim record's layout. A nil value reuses the
	// live layout.
	record []RangeProgress
	// recordSize overrides the record's size field. Zero means "the item's
	// size".
	recordSize int64
}

// TestStagingPublishRefusesIncompleteLayout pins the publication invariant:
// publish() must not rename a staging payload whose range layout does not
// cover the whole expected payload.
//
// The guarantee used to rest only on control flow (every runRange nil-return
// path is caught by the cancellation guard or by the errCh drain), which is
// correct but implicit. The guard added to publish() makes it a checked
// invariant, and this test is what proves the guard actually refuses: it
// seeds an incomplete layout into a download and calls publish().
func TestStagingPublishRefusesIncompleteLayout(t *testing.T) {
	const total = 4096
	payload := []byte("a partially transferred payload")

	cases := []publishLayoutCase{
		{
			name: "a range that is not done",
			live: []RangeProgress{
				{Start: 0, End: total/2 - 1, Committed: total / 2, Done: true},
				{Start: total / 2, End: total - 1, Committed: total/2 - 1},
			},
		},
		{
			name: "a committed total below the expected size",
			live: []RangeProgress{
				{Start: 0, End: total - 1, Committed: total / 2},
			},
		},
		{
			name: "a persisted record that is not done",
			live: completeRangeLayout(total, 1),
			record: []RangeProgress{
				{Start: 0, End: total/2 - 1, Committed: total / 2, Done: true},
				{Start: total / 2, End: total - 1, Committed: total/2 - 1},
			},
		},
		{
			name:       "a persisted record that describes another size",
			live:       completeRangeLayout(total, 1),
			recordSize: total + 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			staging := filepath.Join(dir, "x.incomplete")
			pub := filepath.Join(dir, "x.bin")
			if err := os.WriteFile(staging, payload, 0o644); err != nil {
				t.Fatal(err)
			}
			own, oerr := acquireStagingOwnershipLock(staging)
			if oerr != nil {
				t.Fatalf("acquire ownership lock: %v", oerr)
			}
			defer own.release()

			record := tc.record
			if record == nil {
				record = tc.live
			}
			recordSize := tc.recordSize
			if recordSize == 0 {
				recordSize = total
			}
			meta := IncompleteMeta{
				PID:       os.Getpid(),
				Instance:  processInstanceToken,
				Host:      processHostname,
				StartedAt: time.Now().UTC(),
				Size:      recordSize,
				Version:   stagingMetaVersion,
				Repo:      "o/r",
				Path:      "x.bin",
				Ranges:    record,
			}
			if err := writeStagingMeta(staging+".meta", &meta); err != nil {
				t.Fatal(err)
			}

			d := &stagingDownload{
				ctx:         context.Background(),
				dst:         pub,
				stg:         staging,
				pub:         pub,
				meta:        staging + ".meta",
				it:          PlanItem{RelativePath: "x.bin", Size: total},
				ranges:      tc.live,
				osExclusive: own != nil,
				own:         own,
			}

			err := d.publish()
			if !errors.Is(err, errStagingLayoutIncomplete) {
				t.Fatalf("publish with an incomplete layout = %v, want errStagingLayoutIncomplete", err)
			}
			// The target is NOT published: an incomplete payload must never
			// reach the destination.
			if _, serr := os.Stat(pub); !os.IsNotExist(serr) {
				t.Errorf("publish created the destination from an incomplete layout, stat err=%v", serr)
			}
			// The staged bytes are preserved for a resume.
			if _, serr := os.Stat(staging); serr != nil {
				t.Errorf("the refused publish removed the staged payload: %v", serr)
			}
		})
	}
}

// TestStagingPublishAcceptsCompleteLayout is the positive control of the
// publication invariant: a download whose live AND persisted layout is
// complete still publishes. The guard must not turn a finished transfer into
// a failure.
func TestStagingPublishAcceptsCompleteLayout(t *testing.T) {
	const total = 4096
	payload := []byte("a fully transferred payload")
	dir := t.TempDir()
	staging := filepath.Join(dir, "x.incomplete")
	pub := filepath.Join(dir, "x.bin")
	if err := os.WriteFile(staging, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	own, oerr := acquireStagingOwnershipLock(staging)
	if oerr != nil {
		t.Fatalf("acquire ownership lock: %v", oerr)
	}
	defer own.release()

	layout := completeRangeLayout(total, 1)
	meta := IncompleteMeta{
		PID:       os.Getpid(),
		Instance:  processInstanceToken,
		Host:      processHostname,
		StartedAt: time.Now().UTC(),
		Size:      total,
		Version:   stagingMetaVersion,
		Repo:      "o/r",
		Path:      "x.bin",
		Ranges:    layout,
	}
	if err := writeStagingMeta(staging+".meta", &meta); err != nil {
		t.Fatal(err)
	}

	d := &stagingDownload{
		ctx:         context.Background(),
		dst:         pub,
		stg:         staging,
		pub:         pub,
		meta:        staging + ".meta",
		it:          PlanItem{RelativePath: "x.bin", Size: total},
		ranges:      layout,
		osExclusive: own != nil,
		own:         own,
	}
	if err := d.publish(); err != nil {
		t.Fatalf("publish of a complete layout: %v", err)
	}
	if _, serr := os.Stat(staging); !os.IsNotExist(serr) {
		t.Errorf("the published staging file survived, stat err=%v", serr)
	}
	if got, rerr := os.ReadFile(pub); rerr != nil || string(got) != string(payload) {
		t.Errorf("published payload = %q (err=%v)", got, rerr)
	}
}
