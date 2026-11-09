// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Tests that publication rejects an incomplete range layout or wrong-sized
// payload, while still accepting a complete transfer. Cases seed state and
// call publish directly, without network or timing.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func completeRangeLayout(size int64, n int) []RangeProgress {
	ranges := buildRangeLayout(size, n)
	for i := range ranges {
		ranges[i].Committed = ranges[i].End - ranges[i].Start + 1
		ranges[i].Done = true
	}
	return ranges
}

// Publish refuses missing/foreign claims under the held destination domain
// and still accepts an owned claim with a complete layout.
func TestStagingPublishRefusesForeignClaim(t *testing.T) {
	payload := []byte("someone else's payload")
	makeDownload := func(t *testing.T) (*stagingDownload, string, string) {
		t.Helper()
		dir := t.TempDir()
		staging := filepath.Join(dir, "x.incomplete")
		pub := filepath.Join(dir, "x.bin")
		if err := os.WriteFile(staging, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		domain := acquireTestDomain(t, pub)
		d := &stagingDownload{
			ctx:    context.Background(),
			dst:    pub,
			stg:    staging,
			pub:    pub,
			meta:   staging + ".meta",
			it:     PlanItem{RelativePath: "x.bin", Size: int64(len(payload))},
			ranges: completeRangeLayout(int64(len(payload)), 1),
			domain: domain,
		}
		return d, staging, pub
	}

	t.Run("foreign record", func(t *testing.T) {
		d, staging, pub := makeDownload(t)
		foreign := IncompleteMeta{
			PID:       os.Getpid(),
			Instance:  "another-process-instance",
			Host:      processHostname,
			StartedAt: time.Now().UTC(),
			Size:      int64(len("someone else's payload")),
			Version:   stagingMetaVersion,
			Repo:      "o/r",
			Path:      "x.bin",
			Ranges:    buildRangeLayout(int64(len("someone else's payload")), 1),
		}
		if err := writeStagingMeta(d.meta, &foreign); err != nil {
			t.Fatal(err)
		}
		if err := d.publish(); !errors.Is(err, errStagingClaimLost) {
			t.Fatalf("publish with a foreign record = %v, want errStagingClaimLost", err)
		}
		if _, err := os.Stat(staging); err != nil {
			t.Errorf("publish moved a foreign staging file: %v", err)
		}
		if _, err := os.Stat(pub); !os.IsNotExist(err) {
			t.Errorf("publish created the destination under a foreign record, stat err=%v", err)
		}
	})

	t.Run("missing record", func(t *testing.T) {
		d, staging, pub := makeDownload(t)
		if err := d.publish(); !errors.Is(err, errStagingClaimLost) {
			t.Fatalf("publish with a missing record = %v, want errStagingClaimLost", err)
		}
		if _, err := os.Stat(staging); err != nil {
			t.Errorf("publish moved an unclaimed staging file: %v", err)
		}
		if _, err := os.Stat(pub); !os.IsNotExist(err) {
			t.Errorf("publish created the destination without a claim, stat err=%v", err)
		}
	})

	t.Run("owned record still publishes", func(t *testing.T) {
		d, staging, pub := makeDownload(t)
		ours := IncompleteMeta{
			PID:       os.Getpid(),
			Instance:  processInstanceToken,
			Host:      processHostname,
			StartedAt: time.Now().UTC(),
			Size:      int64(len("someone else's payload")),
			Version:   stagingMetaVersion,
			Repo:      "o/r",
			Path:      "x.bin",
			Ranges:    completeRangeLayout(int64(len("someone else's payload")), 1),
		}
		if err := writeStagingMeta(d.meta, &ours); err != nil {
			t.Fatal(err)
		}
		if err := d.publish(); err != nil {
			t.Fatalf("publish of an owned record: %v", err)
		}
		if _, err := os.Stat(staging); !os.IsNotExist(err) {
			t.Errorf("published staging file survived, stat err=%v", err)
		}
		if got, err := os.ReadFile(pub); err != nil || string(got) != "someone else's payload" {
			t.Errorf("published payload = %q (err=%v)", got, err)
		}
	})
}

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

// Publication must reject incomplete live or persisted layouts. The payload is
// full-sized so the physical-size guard cannot mask the layout check.
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
			domain := acquireTestDomain(t, pub)

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
				ctx:    context.Background(),
				dst:    pub,
				stg:    staging,
				pub:    pub,
				meta:   staging + ".meta",
				it:     PlanItem{RelativePath: "x.bin", Size: total},
				ranges: tc.live,
				domain: domain,
			}

			err := d.publish()
			if !errors.Is(err, errStagingLayoutIncomplete) {
				t.Fatalf("publish with an incomplete layout = %v, want errStagingLayoutIncomplete", err)
			}
			if _, serr := os.Stat(pub); !os.IsNotExist(serr) {
				t.Errorf("publish created the destination from an incomplete layout, stat err=%v", serr)
			}
			if _, serr := os.Stat(staging); serr != nil {
				t.Errorf("the refused publish removed the staged payload: %v", serr)
			}
		})
	}
}

// Even complete live and persisted layouts must not publish a physically short payload.
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
	domain := acquireTestDomain(t, pub)

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
		ctx:    context.Background(),
		dst:    pub,
		stg:    staging,
		pub:    pub,
		meta:   staging + ".meta",
		it:     PlanItem{RelativePath: "x.bin", Size: total},
		ranges: layout,
		domain: domain,
	}
	err := d.publish()
	if !errors.Is(err, errStagingPayloadSize) {
		t.Fatalf("publish of a physically short payload = %v, want errStagingPayloadSize", err)
	}
	if _, serr := os.Stat(pub); !os.IsNotExist(serr) {
		t.Errorf("publish created the destination from a short payload, stat err=%v", serr)
	}
	got, serr := os.ReadFile(staging)
	if serr != nil {
		t.Fatalf("the refused publish removed the staged payload: %v", serr)
	}
	if len(got) != len(short) {
		t.Errorf("staged payload = %d bytes, want %d", len(got), len(short))
	}
}

// Positive control: complete live and persisted layouts over a full-sized payload still publish.
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
	domain := acquireTestDomain(t, pub)

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
		ctx:    context.Background(),
		dst:    pub,
		stg:    staging,
		pub:    pub,
		meta:   staging + ".meta",
		it:     PlanItem{RelativePath: "x.bin", Size: total},
		ranges: layout,
		domain: domain,
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
