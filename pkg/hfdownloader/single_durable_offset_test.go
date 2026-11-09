// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers the single-route sidecar's durable byte offset: resume inherits
// exactly that offset and never the stat-visible size, the offset advances at
// the attempt-boundary checkpoint cadence with the payload synced before the
// record, and records predating the offset field fail safe to a full
// re-fetch instead of falling back to stat-size trust.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// readSingleSidecarOffset parses the sidecar the way a pre-offset-field
// reader would see it: an absent field decodes as zero.
func readSingleSidecarOffset(t *testing.T, dst string) int64 {
	t.Helper()
	data, err := os.ReadFile(singlePartIdentityPath(dst))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var rec struct {
		Offset int64 `json:"offset"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	return rec.Offset
}

// A run interrupted after its last checkpoint leaves bytes on disk above the
// durable offset. The resumed run must inherit exactly the offset: its Range
// starts there (not at the stat size), the untrusted tail is dropped from the
// partial rather than kept as if proven, and the completed publication is
// exact.
func TestSingleResumeInheritsExactlyTheDurableOffset(t *testing.T) {
	const size = 4_096
	const checkpoint = 1_024
	full := rangedTestPayload(size)
	dir := t.TempDir()
	dst := filepath.Join(dir, "model.bin")
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "model.bin", Size: size}
	cfg := Settings{Retries: 0, StallTimeout: "0"}

	// Run 1 dies mid-body after the checkpoint bytes; the attempt boundary
	// must persist the durable offset before the run fails.
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(full[:checkpoint])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv1.Close)
	it.URL = srv1.URL + "/model.bin"
	if _, err := downloadSingle(context.Background(), srv1.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err == nil {
		t.Fatal("the aborted first transfer unexpectedly succeeded")
	}
	if got := readSingleSidecarOffset(t, dst); got != checkpoint {
		t.Fatalf("durable offset after the aborted attempt = %d, want the %d bytes the checkpoint made durable", got, checkpoint)
	}

	// Simulated crash: further bytes landed above the durable offset without
	// a following persist, so the stat size now exceeds it.
	out, err := os.OpenFile(dst+".part", os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write(bytes.Repeat([]byte{0xEE}, 512)); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	// Resumed run, phase 1: a short suffix response. The Range must start at
	// the durable offset — not the stat size — and after it the partial must
	// hold offset+served bytes only: the tail beyond the offset was dropped,
	// not kept under the re-fetched bytes.
	var mu sync.Mutex
	var ranges []string
	srvShort := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", checkpoint, size-1, size))
		w.WriteHeader(http.StatusPartialContent)
		// Flush the headers before writing so the server cannot sniffer-declare
		// a short Content-Length: the clean chunked EOF after 256 bytes is the
		// protocol-level short body this fixture wants.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = w.Write(full[checkpoint : checkpoint+256])
	}))
	t.Cleanup(srvShort.Close)
	it.URL = srvShort.URL + "/model.bin"
	if _, err := downloadSingle(context.Background(), srvShort.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err == nil {
		t.Fatal("the short suffix response unexpectedly succeeded")
	}
	mu.Lock()
	gotRanges := append([]string(nil), ranges...)
	mu.Unlock()
	want := fmt.Sprintf("bytes=%d-%d", checkpoint, size-1)
	if len(gotRanges) != 1 || gotRanges[0] != want {
		t.Fatalf("resume Range = %v, want exactly %q: resume inherits the durable offset, never the stat size", gotRanges, want)
	}
	fi, err := os.Stat(dst + ".part")
	if err != nil {
		t.Fatal(err)
	}
	if wantSize := int64(checkpoint + 256); fi.Size() != wantSize {
		t.Errorf("partial size after the refetch = %d, want %d: bytes above the durable offset must not survive as if proven", fi.Size(), wantSize)
	}

	// Resumed run, phase 2: the healthy server completes from the advanced
	// durable offset; the published file is exact.
	mu.Lock()
	ranges = nil
	mu.Unlock()
	srv := serveWhole(t, full, &ranges, &mu)
	it.URL = srv.URL + "/model.bin"
	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("resumed downloadSingle: %v", err)
	}
	mu.Lock()
	gotRanges = append([]string(nil), ranges...)
	mu.Unlock()
	want = fmt.Sprintf("bytes=%d-%d", checkpoint+256, size-1)
	if len(gotRanges) != 1 || gotRanges[0] != want {
		t.Errorf("second resume Range = %v, want exactly %q", gotRanges, want)
	}
	assertExactFile(t, dst, full)
}

// A partial whose stat size reaches or exceeds the durable offset is never
// trusted above that offset: the tail is re-fetched and nothing unproven is
// published, even when the stat size and framing proof would otherwise look
// complete, and even when Verify cannot detect the holes.
func TestSingleStatSizeBeyondDurableOffsetIsNeverTrusted(t *testing.T) {
	const size = 4_096
	full := rangedTestPayload(size)
	job := Job{Repo: "o/r"}

	// sidecarRecord builds a raw sidecar; a nil offset omits the field
	// entirely, exactly as a record predating it was written.
	sidecarRecord := func(offset *int64, framing bool) []byte {
		rec := fmt.Sprintf(`{"version":1,"repo":"o/r","commit":"%s","path":"model.bin","size":%d,"pinned":true`,
			singleTestIdentity.Commit, size)
		if framing {
			rec += `,"framing_complete":true`
		}
		if offset != nil {
			rec += fmt.Sprintf(`,"offset":%d`, *offset)
		}
		return []byte(rec + "}")
	}
	off := func(v int64) *int64 { return &v }

	cases := []struct {
		name    string
		partial []byte
		record  []byte
		want    string // expected Range header; "" means one full GET
	}{
		{
			// Size-preserving loss: the stat size still reads as complete and
			// the record claims clean framing, but half the bytes sit above
			// the durable offset. The exact-size shortcut must not publish.
			name:    "size-preserving hole below the stat size",
			partial: append(append([]byte(nil), full[:size/2]...), bytes.Repeat([]byte{0xEE}, size/2)...),
			record:  sidecarRecord(off(size/2), true),
			want:    fmt.Sprintf("bytes=%d-%d", size/2, size-1),
		},
		{
			// The record claims more durable bytes than the partial holds;
			// neither side can prove which survived, so nothing is inherited.
			name:    "record offset beyond the partial",
			partial: bytes.Repeat([]byte{0xEE}, 1_000),
			record:  sidecarRecord(off(3_000), false),
			want:    "",
		},
		{
			// A dev-era record predates the offset field: its absent offset
			// reads as zero, so its full-size framing claim inherits nothing
			// and the corrupt stat-visible bytes are re-downloaded whole.
			name:    "record predating the offset field",
			partial: bytes.Repeat([]byte{0xEE}, size),
			record:  sidecarRecord(nil, true),
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "model.bin")
			it := PlanItem{RelativePath: "model.bin", Size: size}
			if err := os.WriteFile(dst+".part", tc.partial, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(singlePartIdentityPath(dst), tc.record, 0o644); err != nil {
				t.Fatal(err)
			}

			var mu sync.Mutex
			var ranges []string
			srv := serveWhole(t, full, &ranges, &mu)
			it.URL = srv.URL + "/model.bin"

			cfg := Settings{Verify: "none", Retries: 0, StallTimeout: "0"}
			if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
				t.Fatalf("downloadSingle: %v", err)
			}
			mu.Lock()
			got := append([]string(nil), ranges...)
			mu.Unlock()
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("Range = %v, want exactly [%q]: bytes beyond the durable offset must be re-fetched, never trusted from stat size", got, tc.want)
			}
			assertExactFile(t, dst, full)
		})
	}
}

// The durable offset advances during the transfer at the ranged family's
// checkpoint cadence — startup, the attempt boundary that moved the offset,
// framing completion — and every checkpoint orders the payload durable before
// its record: the record staged for rename at each checkpoint claims exactly
// the payload size just synced, never more, and never per write chunk.
func TestSingleAttemptBoundaryCheckpointAdvancesTheDurableOffset(t *testing.T) {
	const size = 4_096
	const firstChunk = 1_536
	full := rangedTestPayload(size)
	dir := t.TempDir()
	dst := filepath.Join(dir, "f.bin")
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "f.bin", Size: size}
	cfg := Settings{Retries: 1, BackoffInitial: "1ms", BackoffMax: "1ms", StallTimeout: "0"}

	var reqMu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqMu.Lock()
		requests++
		n := requests
		reqMu.Unlock()
		if n == 1 {
			// The first response dies mid-body so the transfer reaches an
			// attempt boundary with bytes written but not yet claimed.
			w.Header().Set("Content-Length", strconv.Itoa(size))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(full[:firstChunk])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
	}))
	t.Cleanup(srv.Close)
	it.URL = srv.URL + "/f.bin"

	var events []string
	var payloadSizes []int64
	var claimedOffsets []int64
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
		// The record staged for the rename must claim exactly the payload
		// size just synced: a claim never exceeds its durable bytes.
		if data, rerr := os.ReadFile(f.Name()); rerr == nil {
			var rec struct {
				Offset int64 `json:"offset"`
			}
			if json.Unmarshal(data, &rec) == nil {
				claimedOffsets = append(claimedOffsets, rec.Offset)
			} else {
				t.Errorf("parse staged sidecar: %v", rerr)
			}
		} else {
			t.Errorf("read staged sidecar: %v", rerr)
		}
		events = append(events, "sync-sidecar")
		return f.Sync()
	}
	t.Cleanup(func() { syncSingleIdentityRecord = oldRecordSync })

	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle across one aborted attempt: %v", err)
	}

	want := []string{"sync-payload", "sync-sidecar", "sync-payload", "sync-sidecar", "sync-payload", "sync-sidecar"}
	if len(events) != len(want) {
		t.Fatalf("checkpoint events = %v, want %v (startup, attempt boundary, framing)", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("checkpoint events = %v, want %v: every checkpoint must sync the payload before its record", events, want)
		}
	}
	wantOffsets := []int64{0, firstChunk, size}
	if len(payloadSizes) != len(wantOffsets) {
		t.Fatalf("payload sync sizes = %v, want %v", payloadSizes, wantOffsets)
	}
	for i := range wantOffsets {
		if payloadSizes[i] != wantOffsets[i] {
			t.Errorf("payload sync size[%d] = %d, want %d", i, payloadSizes[i], wantOffsets[i])
		}
		if claimedOffsets[i] != wantOffsets[i] {
			t.Errorf("record claim[%d] = %d, want %d: the offset advances at the checkpoint cadence and never exceeds the payload just synced",
				i, claimedOffsets[i], wantOffsets[i])
		}
	}
	assertExactFile(t, dst, full)
}

// A Range-ignoring 200 restart rewrites the partial from byte zero. The
// persisted claim must be corrected to zero BEFORE the first rewritten byte
// lands, so a crash or power loss at any point of the rewrite leaves a record
// that authorizes nothing and the next resume re-fetches instead of trusting
// bytes written after the last checkpoint. The test samples the persisted
// record on every progress event the rewrite emits (any positive claim there
// would authorize rewritten bytes), checks the checkpoint sequence carries the
// correction between the startup claim and the attempt boundary, and resumes
// afterwards from exactly the corrected, re-proven offset.
func TestSingleIgnoredRangeRestartInterruptedMidRewriteDoesNotClaimRewrittenBytes(t *testing.T) {
	const size = 4_096
	const durable = 1_024
	const served = 512
	full := rangedTestPayload(size)
	dir := t.TempDir()
	dst := filepath.Join(dir, "f.bin")
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "f.bin", Size: size}
	cfg := Settings{Retries: 0, StallTimeout: "0"}

	// Seed a partial whose record claims only the durable prefix; the extra
	// tail is what the startup checkpoint truncates away.
	partial := append(append([]byte(nil), full[:durable]...), bytes.Repeat([]byte{0xEE}, 512)...)
	if err := os.WriteFile(dst+".part", partial, 0o644); err != nil {
		t.Fatal(err)
	}
	seed := singleIdentityFor(job, it, singleTestIdentity)
	seed.Offset = durable
	if err := writeSinglePartIdentity(singlePartIdentityPath(dst), seed); err != nil {
		t.Fatal(err)
	}

	// The server ignores Range, answers 200 with the full body's length, and
	// dies mid-body: the client has truncated the partial to zero and begun
	// rewriting it when the attempt is interrupted.
	var mu sync.Mutex
	var gotRanges []string
	var responding atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotRanges = append(gotRanges, r.Header.Get("Range"))
		mu.Unlock()
		// Set before any body byte is written, so every progress event the
		// rewrite emits observes it and the pre-request emit does not.
		responding.Store(true)
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(full[:served])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)
	it.URL = srv.URL + "/f.bin"

	var payloadSizes []int64
	var claims []int64
	oldPayloadSync := syncSinglePartPayload
	syncSinglePartPayload = func(f *os.File) error {
		fi, err := f.Stat()
		if err != nil {
			t.Errorf("stat partial at a sidecar checkpoint: %v", err)
			payloadSizes = append(payloadSizes, -1)
		} else {
			payloadSizes = append(payloadSizes, fi.Size())
		}
		return f.Sync()
	}
	t.Cleanup(func() { syncSinglePartPayload = oldPayloadSync })
	oldRecordSync := syncSingleIdentityRecord
	syncSingleIdentityRecord = func(f *os.File) error {
		if data, rerr := os.ReadFile(f.Name()); rerr == nil {
			var rec struct {
				Offset int64 `json:"offset"`
			}
			if json.Unmarshal(data, &rec) == nil {
				claims = append(claims, rec.Offset)
			} else {
				t.Errorf("parse staged sidecar: %v", rerr)
			}
		} else {
			t.Errorf("read staged sidecar: %v", rerr)
		}
		return f.Sync()
	}
	t.Cleanup(func() { syncSingleIdentityRecord = oldRecordSync })

	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ev ProgressEvent) {
		if ev.Event != "file_progress" || !responding.Load() {
			return
		}
		// The rewrite is on disk right now: a persisted claim of any positive
		// size would authorize rewritten bytes a crash could leave unproven.
		rec, rerr := readSinglePartIdentity(singlePartIdentityPath(dst))
		if rerr != nil || rec == nil {
			t.Errorf("persisted record unreadable during the rewrite: id=%v err=%v", rec, rerr)
			return
		}
		if rec.Offset != 0 {
			t.Errorf("persisted claim %d during the rewrite authorizes bytes re-written from zero; the claim must be corrected before the first rewritten byte", rec.Offset)
		}
	}); err == nil {
		t.Fatal("the interrupted ignored-Range restart unexpectedly succeeded")
	}

	mu.Lock()
	phase1 := append([]string(nil), gotRanges...)
	mu.Unlock()
	if len(phase1) != 1 || phase1[0] != fmt.Sprintf("bytes=%d-%d", durable, size-1) {
		t.Errorf("restart Range = %v, want exactly one %q", phase1, fmt.Sprintf("bytes=%d-%d", durable, size-1))
	}
	// Startup claim, correction to zero before the rewrite, attempt boundary
	// over the rewritten bytes: each claim is backed by the payload sync
	// immediately before it, so the final claim never outruns durable bytes.
	wantPayloads := []int64{durable, 0, served}
	wantClaims := []int64{durable, 0, served}
	if len(payloadSizes) != len(wantPayloads) || len(claims) != len(wantClaims) {
		t.Fatalf("checkpoint payloads = %v claims = %v, want %v / %v", payloadSizes, claims, wantPayloads, wantClaims)
	}
	for i := range wantPayloads {
		if payloadSizes[i] != wantPayloads[i] {
			t.Errorf("payload sync size[%d] = %d, want %d", i, payloadSizes[i], wantPayloads[i])
		}
		if claims[i] != wantClaims[i] {
			t.Errorf("record claim[%d] = %d, want %d", i, claims[i], wantClaims[i])
		}
	}

	// The next resume inherits exactly the corrected, attempt-boundary-proven
	// offset and completes from there.
	mu.Lock()
	gotRanges = nil
	mu.Unlock()
	srvOK := serveWhole(t, full, &gotRanges, &mu)
	it.URL = srvOK.URL + "/f.bin"
	if _, err := downloadSingle(context.Background(), srvOK.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("resumed downloadSingle: %v", err)
	}
	mu.Lock()
	phase2 := append([]string(nil), gotRanges...)
	mu.Unlock()
	if len(phase2) != 1 || phase2[0] != fmt.Sprintf("bytes=%d-%d", served, size-1) {
		t.Errorf("resume Range = %v, want exactly one %q", phase2, fmt.Sprintf("bytes=%d-%d", served, size-1))
	}
	assertExactFile(t, dst, full)
}
