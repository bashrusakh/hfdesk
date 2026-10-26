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
//   - publish() asserts, explicitly, that the staging PAYLOAD itself holds
//     exactly the expected number of bytes before the same rename, so a
//     record that claims a complete layout over a physically short file is
//     refused as well;
//   - an incomplete layout is refused with errStagingLayoutIncomplete and
//     nothing is published: the staged bytes stay on disk for a resume;
//   - a short payload is refused with errStagingPayloadSize and nothing is
//     published: the staged bytes stay on disk for a resume;
//   - a COMPLETE layout over a full-size payload still publishes, so neither
//     guard over-refuses.
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
//
// The payload holds the FULL expected size here, so the layout is the only
// thing that can refuse the publication: the physical size guard must not
// mask the layout guard.
func TestStagingPublishRefusesIncompleteLayout(t *testing.T) {
	const total = 4096
	payload := make([]byte, total)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

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

// TestStagingPublishRefusesShortPayload pins the physical size invariant:
// publish() must not rename a staging payload that holds fewer bytes than the
// expected size, even when the record and the live layout both claim a
// COMPLETE transfer.
//
// The layout is the completion contract for the BYTES; the payload's own
// length is the completion contract for the FILE. A record can describe a
// complete layout while the file behind it is short, and publishing that file
// would put a payload that disagrees with its own metadata on the
// destination.
func TestStagingPublishRefusesShortPayload(t *testing.T) {
	const total = 4096
	short := make([]byte, total/8)
	for i := range short {
		short[i] = byte(i % 251)
	}

	dir := t.TempDir()
	staging := filepath.Join(dir, "x.incomplete")
	pub := filepath.Join(dir, "x.bin")
	if err := os.WriteFile(staging, short, 0o644); err != nil {
		t.Fatal(err)
	}
	own, oerr := acquireStagingOwnershipLock(staging)
	if oerr != nil {
		t.Fatalf("acquire ownership lock: %v", oerr)
	}
	defer own.release()

	// The record and the live layout both claim a COMPLETE transfer.
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
	err := d.publish()
	if !errors.Is(err, errStagingPayloadSize) {
		t.Fatalf("publish of a physically short payload = %v, want errStagingPayloadSize", err)
	}
	// Nothing is published.
	if _, serr := os.Stat(pub); !os.IsNotExist(serr) {
		t.Errorf("publish created the destination from a short payload, stat err=%v", serr)
	}
	// The staged bytes are preserved for a resume.
	got, serr := os.ReadFile(staging)
	if serr != nil {
		t.Fatalf("the refused publish removed the staged payload: %v", serr)
	}
	if len(got) != len(short) {
		t.Errorf("staged payload = %d bytes, want %d", len(got), len(short))
	}
}

// TestStagingPublishAcceptsCompleteLayout is the positive control of the
// publication invariant: a download whose live AND persisted layout is
// complete, over a payload that really holds the expected bytes, still
// publishes. Neither guard may turn a finished transfer into a failure.
func TestStagingPublishAcceptsCompleteLayout(t *testing.T) {
	const total = 4096
	payload := make([]byte, total)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
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
	got, rerr := os.ReadFile(pub)
	if rerr != nil {
		t.Fatalf("read published payload: %v", rerr)
	}
	if len(got) != total {
		t.Fatalf("published payload = %d bytes, want %d", len(got), total)
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("published payload differs at byte %d", i)
		}
	}
}
