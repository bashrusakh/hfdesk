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
