// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers the durable high-water-mark ordering of checkpoint flushes.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A checkpoint that claims written ranges must order those payload bytes
// durable before the record is persisted: the claim must be true under power
// loss, not only under a process crash. The test fails if the payload sync is
// missing or happens after the record install.
func TestCheckpointSyncsClaimedPayloadBeforePersistingRecord(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "x.incomplete")
	metaPath := staging + ".meta"

	const total = 4_096
	seed := newClaimMeta(PlanItem{RelativePath: "x.bin", Size: total}, Job{Repo: "o/r"}, "c", true)
	if err := writeStagingMeta(metaPath, &seed); err != nil {
		t.Fatal(err)
	}

	out, err := os.OpenFile(staging, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	var events []string
	oldRename := renameStagingClaimRecord
	renameStagingClaimRecord = func(oldpath, newpath string) error {
		events = append(events, "persist-record")
		return oldRename(oldpath, newpath)
	}
	t.Cleanup(func() { renameStagingClaimRecord = oldRename })
	oldSync := syncStagingPayload
	syncStagingPayload = func(f *os.File) error {
		events = append(events, "sync-payload")
		return f.Sync()
	}
	t.Cleanup(func() { syncStagingPayload = oldSync })

	d := &stagingDownload{
		ctx:          context.Background(),
		it:           PlanItem{RelativePath: "x.bin", Size: total},
		job:          Job{Repo: "o/r"},
		dst:          filepath.Join(dir, "x.bin"),
		stg:          staging,
		pub:          filepath.Join(dir, "x.bin"),
		meta:         metaPath,
		out:          out,
		ranges:       buildRangeLayout(total, 1),
		commit:       "c",
		commitPinned: true,
		startedAt:    time.Now().UTC(),
	}
	// The payload bytes are on disk, then the checkpoint claims them.
	if _, err := out.WriteAt(make([]byte, total), 0); err != nil {
		t.Fatal(err)
	}
	events = append(events, "write-payload")
	d.ranges[0].Committed = total
	d.ranges[0].Done = true
	if err := d.flushMeta(); err != nil {
		t.Fatalf("checkpoint flush: %v", err)
	}

	// Exactly one payload sync per checkpoint, between the writes and the
	// record install: no per-chunk syncs, and never after the claim.
	want := []string{"write-payload", "sync-payload", "persist-record"}
	if len(events) != len(want) {
		t.Fatalf("checkpoint events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("checkpoint events = %v, want %v: the record must persist only after the payload is durable", events, want)
		}
	}

	meta, err := readStagingMeta(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Ranges) != 1 || meta.Ranges[0].Committed != total || !meta.Ranges[0].Done {
		t.Fatalf("persisted checkpoint = %+v, want the claimed %d-byte range", meta.Ranges, total)
	}
}
