// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers the durable high-water-mark ordering of every single-route sidecar
// persist: the payload's written bytes sync before the record, the record
// syncs before its rename, and a broken ordering at a checkpoint fails the
// item instead of publishing.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Every sidecar persist, whatever its claim state, must order the partial's
// written bytes durable before the record is persisted, and must sync its own
// record before the rename publishes it: the claim must be true under power
// loss, not only under a process crash. The test fails if a payload sync is
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
	var payloadSizes []int64
	oldPayloadSync := syncSinglePartPayload
	syncSinglePartPayload = func(f *os.File) error {
		fi, err := f.Stat()
		if err != nil {
			t.Errorf("stat partial at a sidecar checkpoint: %v", err)
			payloadSizes = append(payloadSizes, -1)
		} else {
			payloadSizes = append(payloadSizes, fi.Size())
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

	// Every persist syncs the payload before its record: the fresh startup
	// identity (claim false) over the empty partial, then the framing
	// checkpoint (claim true) over the full payload.
	want := []string{"sync-payload", "sync-sidecar", "sync-payload", "sync-sidecar"}
	if len(events) != len(want) {
		t.Fatalf("checkpoint events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("checkpoint events = %v, want %v: every sidecar persist must sync the payload first", events, want)
		}
	}
	if len(payloadSizes) != 2 || payloadSizes[0] != 0 || payloadSizes[1] != total {
		t.Fatalf("payload sync sizes = %v, want [0 %d] (startup persist, then the framing checkpoint)", payloadSizes, total)
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

// A resumed exact-size partial with a framing proof re-publishes its identity
// claim-true at the startup self-heal persist: that persist must order the
// full payload durable before its record syncs, so the claim it writes is
// already backed by durable bytes. The oracle fails if the payload sync is
// missing, ordered after the record sync, or sees other than the claimed
// payload size. (The destination already carries the claim-true record from
// the prior run, so a claim-bit readback cannot discriminate here; the event
// order and the payload size do.)
func TestSingleStartupSelfHealSyncsPayloadBeforeClaimTrueSidecar(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)

	dir := t.TempDir()
	dst := filepath.Join(dir, "f.bin")
	srv := serveWhole(t, full, nil, nil)
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "f.bin", URL: srv.URL, Size: total}
	cfg := Settings{Retries: 0, StallTimeout: "0"}

	seedSinglePartial(t, dst, full, job, it, completeSingleIdentity())

	var events []string
	var payloadSizes []int64
	oldPayloadSync := syncSinglePartPayload
	syncSinglePartPayload = func(f *os.File) error {
		fi, err := f.Stat()
		if err != nil {
			t.Errorf("stat partial at the startup checkpoint: %v", err)
			payloadSizes = append(payloadSizes, -1)
		} else {
			payloadSizes = append(payloadSizes, fi.Size())
		}
		events = append(events, "sync-payload")
		return f.Sync()
	}
	t.Cleanup(func() { syncSinglePartPayload = oldPayloadSync })

	oldRecordSync := syncSingleIdentityRecord
	syncSingleIdentityRecord = func(f *os.File) error {
		events = append(events, "sync-sidecar")
		return f.Sync()
	}
	t.Cleanup(func() { syncSingleIdentityRecord = oldRecordSync })

	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("resume of the framed exact-size partial: %v", err)
	}

	// The claim-true startup persist is the only checkpoint: the exact-size
	// shortcut publishes without a request, so there is exactly one pair.
	want := []string{"sync-payload", "sync-sidecar"}
	if len(events) != len(want) {
		t.Fatalf("checkpoint events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("checkpoint events = %v, want %v: the payload must be durable before the claim-true record syncs", events, want)
		}
	}
	if len(payloadSizes) != 1 || payloadSizes[0] != total {
		t.Fatalf("payload sync sizes = %v, want one sync of the claimed %d bytes", payloadSizes, total)
	}
	assertExactFile(t, dst, full)
}

// An unpinned retry restarts the complete file from byte zero and re-publishes
// the identity claim-false; that persist must also order the payload durable
// first, like every other sidecar persist. The oracle fails if the claim-false
// restart persist skips the payload sync (the pre-strengthening behavior) or
// orders it after the record sync.
func TestSingleUnpinnedRestartSyncsPayloadBeforeClaimFalseSidecar(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)

	dir := t.TempDir()
	dst := filepath.Join(dir, "f.bin")
	job := Job{Repo: "o/r", Revision: "main"}
	it := PlanItem{RelativePath: "f.bin", Size: total}
	cfg := Settings{Retries: 1, StallTimeout: "0"}
	unpinned := singleOpts{Commit: "main", CommitPinned: false}

	var mu sync.Mutex
	var ranges []string
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		requests++
		first := requests == 1
		mu.Unlock()
		if first {
			// The first response dies mid-body so the unpinned transfer must
			// restart the complete file from byte zero on retry.
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			_, _ = w.Write(full[:total/2])
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	}))
	t.Cleanup(srv.Close)
	it.URL = srv.URL

	var events []string
	var payloadSizes []int64
	oldPayloadSync := syncSinglePartPayload
	syncSinglePartPayload = func(f *os.File) error {
		fi, err := f.Stat()
		if err != nil {
			t.Errorf("stat partial at a sidecar checkpoint: %v", err)
			payloadSizes = append(payloadSizes, -1)
		} else {
			payloadSizes = append(payloadSizes, fi.Size())
		}
		events = append(events, "sync-payload")
		return f.Sync()
	}
	t.Cleanup(func() { syncSinglePartPayload = oldPayloadSync })

	oldRecordSync := syncSingleIdentityRecord
	syncSingleIdentityRecord = func(f *os.File) error {
		events = append(events, "sync-sidecar")
		return f.Sync()
	}
	t.Cleanup(func() { syncSingleIdentityRecord = oldRecordSync })

	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, unpinned, func(ProgressEvent) {}); err != nil {
		t.Fatalf("unpinned retry: %v", err)
	}

	// Three persists: the startup identity (claim false), the claim-false
	// restart after the truncation, then the framing checkpoint. Each must
	// read payload-sync, sidecar-sync.
	want := []string{
		"sync-payload", "sync-sidecar",
		"sync-payload", "sync-sidecar",
		"sync-payload", "sync-sidecar",
	}
	if len(events) != len(want) {
		t.Fatalf("checkpoint events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("checkpoint events = %v, want %v: every sidecar persist must sync the payload first", events, want)
		}
	}
	if len(payloadSizes) != 3 || payloadSizes[0] != 0 || payloadSizes[1] != 0 || payloadSizes[2] != total {
		t.Fatalf("payload sync sizes = %v, want [0 0 %d] (startup, truncated restart, framing checkpoint)", payloadSizes, total)
	}
	mu.Lock()
	gotRanges := append([]string(nil), ranges...)
	mu.Unlock()
	if len(gotRanges) != 2 || gotRanges[0] != "" || gotRanges[1] != "" {
		t.Fatalf("unpinned response ranges = %v, want two zero-start full GETs", gotRanges)
	}
	assertExactFile(t, dst, full)
}

// A durability-ordering failure at the framing checkpoint — either the payload
// sync or the sidecar record's own sync — must fail the item with that cause
// and publish nothing: no rename, no finalization attempt, and no framing
// claim on disk (the failed persist still leaves only the startup claim-false
// record). The partial itself is preserved for resume.
func TestSingleCheckpointSyncFailureFailsItemWithoutPublication(t *testing.T) {
	cases := []struct {
		name        string
		failPayload bool
	}{
		{name: "payload sync failure", failPayload: true},
		{name: "sidecar record sync failure", failPayload: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const total = 4_096
			full := rangedTestPayload(total)

			dir := t.TempDir()
			dst := filepath.Join(dir, "f.bin")
			sidecar := singlePartIdentityPath(dst)
			srv := serveWhole(t, full, nil, nil)
			job := Job{Repo: "o/r"}
			it := PlanItem{RelativePath: "f.bin", URL: srv.URL, Size: total}
			cfg := Settings{Retries: 0, StallTimeout: "0"}

			// The run syncs twice per channel: the startup identity, then the
			// framing checkpoint; the checkpoint's sync is the injected failure.
			errBoom := errors.New("injected sidecar durability failure")
			payloadCalls, recordCalls := 0, 0
			oldPayloadSync := syncSinglePartPayload
			syncSinglePartPayload = func(f *os.File) error {
				payloadCalls++
				if tc.failPayload && payloadCalls == 2 {
					return errBoom
				}
				return f.Sync()
			}
			t.Cleanup(func() { syncSinglePartPayload = oldPayloadSync })
			oldRecordSync := syncSingleIdentityRecord
			syncSingleIdentityRecord = func(f *os.File) error {
				recordCalls++
				if !tc.failPayload && recordCalls == 2 {
					return errBoom
				}
				return f.Sync()
			}
			t.Cleanup(func() { syncSingleIdentityRecord = oldRecordSync })

			var events []string
			_, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ev ProgressEvent) {
				events = append(events, ev.Event)
			})
			if err == nil {
				t.Fatal("a failed checkpoint durability ordering must fail the item")
			}
			if !errors.Is(err, errBoom) {
				t.Errorf("error = %v, want it to wrap the injected durability failure", err)
			}
			if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
				t.Errorf("dst was published despite the failed durability ordering: stat err = %v", serr)
			}
			for _, ev := range events {
				if ev == "file_finalizing" {
					t.Errorf("publication began (%s) despite the failed durability ordering", ev)
				}
			}
			id, rerr := readSinglePartIdentity(sidecar)
			if rerr != nil || id == nil {
				t.Fatalf("sidecar after the failed checkpoint: id=%v err=%v, want the startup record preserved", id, rerr)
			}
			if id.FramingComplete {
				t.Errorf("the failed persist published a framing claim: %+v", id)
			}
			fi, perr := os.Stat(dst + ".part")
			if perr != nil {
				t.Errorf("partial after the failed checkpoint: %v, want the %d bytes preserved for resume", perr, total)
			} else if fi.Size() != total {
				t.Errorf("partial holds %d bytes after the failed checkpoint, want the %d bytes preserved for resume", fi.Size(), total)
			}
			// The payload sync must precede the record sync at each persist:
			// a payload failure aborts before the record syncs, and a record
			// failure happens only after its payload sync succeeded.
			if tc.failPayload && (payloadCalls != 2 || recordCalls != 1) {
				t.Errorf("sync calls payload=%d record=%d, want payload=2 record=1", payloadCalls, recordCalls)
			}
			if !tc.failPayload && (payloadCalls != 2 || recordCalls != 2) {
				t.Errorf("sync calls payload=%d record=%d, want 2 and 2", payloadCalls, recordCalls)
			}
		})
	}
}
