// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Tests that publication refuses a foreign or missing claim, an incomplete
// range layout or a physically short payload, while still accepting a
// complete transfer. Cases seed state and call publish directly, without
// network or timing.

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

// publishClaim describes the persisted claim of a fixture download. A zero
// instance persists no claim record at all; a zero size and nil ranges reuse
// the item's size and the live layout.
type publishClaim struct {
	instance string
	size     int64
	ranges   []RangeProgress
}

// makePublishDownload seeds stagingBytes in a fresh destination directory,
// holds the destination domain, persists the described claim, and returns the
// download with live as its in-memory layout. All publication-gate cases build
// their fixture here instead of re-seeding the same download inline.
func makePublishDownload(t *testing.T, stagingBytes []byte, size int64, live []RangeProgress, claim publishClaim) (*stagingDownload, string, string) {
	t.Helper()
	dir := t.TempDir()
	staging := filepath.Join(dir, "x.incomplete")
	pub := filepath.Join(dir, "x.bin")
	if err := os.WriteFile(staging, stagingBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	d := &stagingDownload{
		ctx:    context.Background(),
		dst:    pub,
		stg:    staging,
		pub:    pub,
		meta:   staging + ".meta",
		it:     PlanItem{RelativePath: "x.bin", Size: size},
		ranges: live,
		domain: acquireTestDomain(t, pub),
	}
	if claim.instance != "" {
		recordSize := claim.size
		if recordSize == 0 {
			recordSize = size
		}
		recordRanges := claim.ranges
		if recordRanges == nil {
			recordRanges = live
		}
		meta := IncompleteMeta{
			PID:       os.Getpid(),
			Instance:  claim.instance,
			Host:      processHostname,
			StartedAt: time.Now().UTC(),
			Size:      recordSize,
			Version:   stagingMetaVersion,
			Repo:      "o/r",
			Path:      "x.bin",
			Ranges:    recordRanges,
		}
		if err := writeStagingMeta(d.meta, &meta); err != nil {
			t.Fatal(err)
		}
	}
	return d, staging, pub
}

type publishGateCase struct {
	name string
	// staging is the payload physically present at the staging path.
	staging []byte
	// size is the item's declared size.
	size int64
	// live is the download's in-memory range layout.
	live []RangeProgress
	// claim is the persisted claim; its zero value persists no record.
	claim publishClaim
	// want is the expected refusal error; nil expects a successful publication.
	want error
	// stagedLen, when non-zero, pins the refused payload's physical length.
	stagedLen int
}

// One table over every publication gate: the claim, layout and payload-size
// refusals plus the positive control. Layout refusals use a full-sized payload
// so the physical-size guard cannot mask the layout check; the short-payload
// row carries complete live and persisted layouts so only the physical size
// can refuse it.
func TestStagingPublishEnforcesEveryPublicationGate(t *testing.T) {
	const total = 4096
	fullPayload := make([]byte, total)
	for i := range fullPayload {
		fullPayload[i] = byte(i % 251)
	}
	shortPayload := make([]byte, total/8)
	for i := range shortPayload {
		shortPayload[i] = byte(i % 251)
	}
	claimPayload := []byte("someone else's payload")
	claimSize := int64(len(claimPayload))

	cases := []publishGateCase{
		{
			name:    "foreign record",
			staging: claimPayload,
			size:    claimSize,
			live:    completeRangeLayout(claimSize, 1),
			claim: publishClaim{
				instance: "another-process-instance",
				ranges:   buildRangeLayout(claimSize, 1),
			},
			want: errStagingClaimLost,
		},
		{
			name:    "missing record",
			staging: claimPayload,
			size:    claimSize,
			live:    completeRangeLayout(claimSize, 1),
			want:    errStagingClaimLost,
		},
		{
			name:    "owned record still publishes",
			staging: claimPayload,
			size:    claimSize,
			live:    completeRangeLayout(claimSize, 1),
			claim:   publishClaim{instance: processInstanceToken},
		},
		{
			name:    "a range that is not done",
			staging: fullPayload,
			size:    total,
			live: []RangeProgress{
				{Start: 0, End: total/2 - 1, Committed: total / 2, Done: true},
				{Start: total / 2, End: total - 1, Committed: total/2 - 1},
			},
			claim: publishClaim{instance: processInstanceToken},
			want:  errStagingLayoutIncomplete,
		},
		{
			name:    "a committed total below the expected size",
			staging: fullPayload,
			size:    total,
			live: []RangeProgress{
				{Start: 0, End: total - 1, Committed: total / 2},
			},
			claim: publishClaim{instance: processInstanceToken},
			want:  errStagingLayoutIncomplete,
		},
		{
			name:    "a persisted record that is not done",
			staging: fullPayload,
			size:    total,
			live:    completeRangeLayout(total, 1),
			claim: publishClaim{
				instance: processInstanceToken,
				ranges: []RangeProgress{
					{Start: 0, End: total/2 - 1, Committed: total / 2, Done: true},
					{Start: total / 2, End: total - 1, Committed: total/2 - 1},
				},
			},
			want: errStagingLayoutIncomplete,
		},
		{
			name:    "a persisted record that describes another size",
			staging: fullPayload,
			size:    total,
			live:    completeRangeLayout(total, 1),
			claim:   publishClaim{instance: processInstanceToken, size: total + 1},
			want:    errStagingLayoutIncomplete,
		},
		{
			name:      "a physically short payload",
			staging:   shortPayload,
			size:      total,
			live:      completeRangeLayout(total, 1),
			claim:     publishClaim{instance: processInstanceToken},
			want:      errStagingPayloadSize,
			stagedLen: total / 8,
		},
		{
			name:    "complete layout publishes",
			staging: fullPayload,
			size:    total,
			live:    completeRangeLayout(total, 1),
			claim:   publishClaim{instance: processInstanceToken},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, staging, pub := makePublishDownload(t, tc.staging, tc.size, tc.live, tc.claim)

			err := d.publish()
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("publish %s = %v, want %v", tc.name, err, tc.want)
				}
				if _, serr := os.Stat(pub); !os.IsNotExist(serr) {
					t.Errorf("publish created the destination for %s, stat err=%v", tc.name, serr)
				}
				if _, serr := os.Stat(staging); serr != nil {
					t.Errorf("the refused publish removed the staged payload (%s): %v", tc.name, serr)
				}
				if tc.stagedLen != 0 {
					got, rerr := os.ReadFile(staging)
					if rerr != nil {
						t.Fatalf("read the refused staging payload: %v", rerr)
					}
					if len(got) != tc.stagedLen {
						t.Errorf("staged payload = %d bytes, want %d", len(got), tc.stagedLen)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("publish %s: %v", tc.name, err)
			}
			if _, serr := os.Stat(staging); !os.IsNotExist(serr) {
				t.Errorf("the published staging file survived (%s), stat err=%v", tc.name, serr)
			}
			got, rerr := os.ReadFile(pub)
			if rerr != nil {
				t.Fatalf("read published payload: %v", rerr)
			}
			if len(got) != len(tc.staging) {
				t.Fatalf("published payload = %d bytes, want %d", len(got), len(tc.staging))
			}
			for i := range got {
				if got[i] != tc.staging[i] {
					t.Fatalf("published payload differs at byte %d", i)
				}
			}
		})
	}
}
