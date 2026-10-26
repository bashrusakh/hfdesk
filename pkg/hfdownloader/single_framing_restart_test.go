// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Clean-framing completion across a single-path restart.
//
// The identity sidecar proves which OBJECT a `.part` belongs to; it does not
// prove that the HTTP response body that wrote it ended cleanly. A process can
// exit after the final WriteAt but before io.Copy observes EOF, so an
// exactly-sized partial may still hold a torn final byte. A restart publishes
// such a partial without a request only when the sidecar also records a
// cleanly-framed completion. Otherwise it reserves the final byte and
// revalidates it with the non-empty range [size-1, size); an error while that
// revalidation body is still unframed keeps the final byte uncommitted.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

// TestDownloadSingle_CompletePartialWithoutFramingProofRevalidatesLastByte is
// the restart regression: a matching, pinned, exactly-sized partial whose
// sidecar does NOT record clean framing must not be published as-is. Only the
// final byte is re-requested and the result is the exact served content.
func TestDownloadSingle_CompletePartialWithoutFramingProofRevalidatesLastByte(t *testing.T) {
	const total = 64
	full := rangedTestPayload(total)
	dir := t.TempDir()
	dst := dir + "/model.bin"
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "model.bin", Size: total}

	// The sidecar matches the object but records no clean-framing completion.
	seedSinglePartial(t, dst, full, job, it, singleTestIdentity)

	var (
		requests atomic.Int32
		gotRange atomic.Value
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		gotRange.Store(r.Header.Get("Range"))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", total-1, total-1, total))
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(full[total-1:])
	}))
	defer srv.Close()
	it.URL = srv.URL + "/model.bin"

	cfg := Settings{Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}

	if got := requests.Load(); got != 1 {
		t.Errorf("issued %d requests; want exactly one final-byte revalidation", got)
	}
	wantRange := fmt.Sprintf("bytes=%d-%d", total-1, total-1)
	if got, _ := gotRange.Load().(string); got != wantRange {
		t.Errorf("revalidation Range = %q, want the non-empty last-byte range %q", got, wantRange)
	}
	assertExactFile(t, dst, full)
	if _, err := os.Stat(singlePartIdentityPath(dst)); !os.IsNotExist(err) {
		t.Errorf("identity sidecar survived publication (stat err=%v)", err)
	}
}

// TestDownloadSingle_RevalidationBodyErrorDoesNotPublishFinalByte is the
// negative of the restart rule: the revalidation body delivers the final byte
// and then aborts before clean framing. The item fails, nothing is published,
// and the final byte stays uncommitted for a later retry/restart.
func TestDownloadSingle_RevalidationBodyErrorDoesNotPublishFinalByte(t *testing.T) {
	const total = 64
	full := rangedTestPayload(total)
	dir := t.TempDir()
	dst := dir + "/model.bin"
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "model.bin", Size: total}
	seedSinglePartial(t, dst, full, job, it, singleTestIdentity)

	var (
		requests atomic.Int32
		gotRange atomic.Value
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		gotRange.Store(r.Header.Get("Range"))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", total-1, total-1, total))
		w.WriteHeader(http.StatusPartialContent)
		// Deliver the final byte, then abort the chunked body before its
		// terminal chunk: the byte arrived, but its framing is unproven.
		_, _ = w.Write(full[total-1:])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()
	it.URL = srv.URL + "/model.bin"

	cfg := Settings{Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err == nil {
		t.Fatal("the unframed revalidation body unexpectedly succeeded")
	}

	if got := requests.Load(); got != 1 {
		t.Errorf("issued %d requests; want exactly one final-byte revalidation", got)
	}
	wantRange := fmt.Sprintf("bytes=%d-%d", total-1, total-1)
	if got, _ := gotRange.Load().(string); got != wantRange {
		t.Errorf("revalidation Range = %q, want %q", got, wantRange)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("an unframed final byte was published (stat err=%v)", err)
	}
	fi, err := os.Stat(dst + ".part")
	if err != nil {
		t.Fatalf("stat partial: %v", err)
	}
	if fi.Size() != total-1 {
		t.Errorf("partial size = %d, want %d (the final byte must stay uncommitted)", fi.Size(), total-1)
	}
	record, err := readSinglePartIdentity(singlePartIdentityPath(dst))
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if record == nil || record.FramingComplete {
		t.Errorf("identity record = %+v, want a surviving record with FramingComplete=false", record)
	}
}

// TestDownloadSingle_OneByteCompletePartialWithoutFramingProofRevalidates is
// the size-1 edge of the restart rule: the reserved final byte starts at
// offset zero, so the revalidation must still be an explicit non-empty range
// [0, 1) rather than an implicit full GET.
func TestDownloadSingle_OneByteCompletePartialWithoutFramingProofRevalidates(t *testing.T) {
	full := []byte{0x7A}
	dir := t.TempDir()
	dst := dir + "/tiny.bin"
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "tiny.bin", Size: 1}
	seedSinglePartial(t, dst, full, job, it, singleTestIdentity)

	var (
		requests atomic.Int32
		gotRange atomic.Value
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		gotRange.Store(r.Header.Get("Range"))
		w.Header().Set("Content-Range", "bytes 0-0/1")
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(full)
	}))
	defer srv.Close()
	it.URL = srv.URL + "/tiny.bin"

	cfg := Settings{Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("issued %d requests; want exactly one final-byte revalidation", got)
	}
	if got, _ := gotRange.Load().(string); got != "bytes=0-0" {
		t.Errorf("revalidation Range = %q, want %q", got, "bytes=0-0")
	}
	assertExactFile(t, dst, full)
}

// TestDownloadSingle_CompletePartialWithFramingProofNeedsNoRequest pins the
// positive side of the rule: a matching sidecar WITH the clean-framing proof
// still publishes an exactly-sized partial without any network request.
func TestDownloadSingle_CompletePartialWithFramingProofNeedsNoRequest(t *testing.T) {
	const total = 64
	full := rangedTestPayload(total)
	dir := t.TempDir()
	dst := dir + "/model.bin"
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "model.bin", Size: total}
	seedSinglePartial(t, dst, full, job, it, completeSingleIdentity())

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "no request expected for a framed complete partial", http.StatusInternalServerError)
	}))
	defer srv.Close()
	it.URL = srv.URL + "/model.bin"

	cfg := Settings{Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadSingle: %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("issued %d requests; want 0 for an exactly-sized partial with framing proof", got)
	}
	assertExactFile(t, dst, full)
}
