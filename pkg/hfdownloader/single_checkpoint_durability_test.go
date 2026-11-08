// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers the durable high-water-mark ordering of the single-route sidecar's
// framing claim.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A sidecar persist that claims framing_complete must order the partial's
// written bytes durable before the claim is persisted, and must sync its own
// record before the rename publishes it: the claim must be true under power
// loss, not only under a process crash. The test fails if the payload sync is
// missing or happens after the sidecar persist, if the sidecar temp is not
// synced, or if the rename publishes the claim before that sync.
func TestSingleCheckpointSyncsClaimedPayloadBeforePersistingSidecar(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)

	dir := t.TempDir()
	// Publication is deliberately blocked (dst is a directory) so the sidecar
	// and partial survive finalization and the persisted claim stays readable.
	dst := filepath.Join(dir, "f.bin")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	sidecar := singlePartIdentityPath(dst)
	srv := serveWhole(t, full, nil, nil)
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "f.bin", URL: srv.URL, Size: total}
	cfg := Settings{Retries: 0, StallTimeout: "0"}

	var events []string
	oldPayloadSync := syncSinglePartPayload
	syncSinglePartPayload = func(f *os.File) error {
		fi, err := f.Stat()
		if err != nil {
			t.Errorf("stat partial at the framing checkpoint: %v", err)
		} else if fi.Size() != total {
			t.Errorf("partial holds %d bytes at the framing checkpoint, want the claimed %d", fi.Size(), total)
		}
		events = append(events, "sync-payload")
		return f.Sync()
	}
	t.Cleanup(func() { syncSinglePartPayload = oldPayloadSync })

	oldRecordSync := syncSingleIdentityRecord
	syncSingleIdentityRecord = func(f *os.File) error {
		events = append(events, "sync-sidecar")
		// The rename publishing this record must not have run yet: the
		// destination cannot already carry a framing claim.
		if id, err := readSinglePartIdentity(sidecar); err == nil && id != nil && id.FramingComplete {
			t.Errorf("sidecar carries framing_complete before its temp record was synced")
		}
		return f.Sync()
	}
	t.Cleanup(func() { syncSingleIdentityRecord = oldRecordSync })

	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err == nil {
		t.Fatal("publication unexpectedly succeeded; the fixture must block the final rename")
	}

	// The fresh run persists the identity (claim false), then the framing
	// checkpoint: payload sync, then sidecar sync, then the sidecar rename.
	want := []string{"sync-sidecar", "sync-payload", "sync-sidecar"}
	if len(events) != len(want) {
		t.Fatalf("checkpoint events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("checkpoint events = %v, want %v: the payload must be durable before the framing claim persists", events, want)
		}
	}

	id, err := readSinglePartIdentity(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if id == nil || !id.FramingComplete {
		t.Fatalf("persisted sidecar = %+v, want the framing_complete claim", id)
	}
	data, err := os.ReadFile(dst + ".part")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != total {
		t.Fatalf("partial holds %d bytes, want %d", len(data), total)
	}
}
