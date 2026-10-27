// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestFreshClaimInstallIsAtomicAndRecoversStagedPartial(t *testing.T) {
	oldLink := linkStagingClaimRecord
	defer func() { linkStagingClaimRecord = oldLink }()
	dir := t.TempDir()
	staging := filepath.Join(dir, "fresh.incomplete")
	metaPath := staging + ".meta"
	item := PlanItem{RelativePath: "fresh.bin", Size: 4096, AcceptRanges: true}
	job := Job{Repo: "o/r"}
	temp := metaPath + ".tmp"
	if err := os.Mkdir(temp, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := claimStaging(context.Background(), staging, filepath.Join(dir, "fresh.bin"), metaPath,
		item, job, "commit", true, Settings{Verify: "none"}, false)
	if err == nil {
		t.Fatal("claim unexpectedly succeeded with an unusable record temp")
	}
	if _, err := os.Stat(metaPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed initial install exposed an authoritative claim: %v", err)
	}
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed initial install created a payload: %v", err)
	}
	if err := os.Remove(temp); err != nil {
		t.Fatal(err)
	}
	linkErr := errors.New("injected atomic claim install interruption")
	stagedCompleteRecord := false
	linkStagingClaimRecord = func(src, dst string) error {
		if src == temp && dst == metaPath {
			raw, readErr := os.ReadFile(src)
			var staged IncompleteMeta
			stagedCompleteRecord = readErr == nil && json.Unmarshal(raw, &staged) == nil &&
				staged.Instance == processInstanceToken && staged.Version == stagingMetaVersion
			return linkErr
		}
		return os.Link(src, dst)
	}
	_, err = claimStaging(context.Background(), staging, filepath.Join(dir, "fresh.bin"), metaPath,
		item, job, "commit", true, Settings{Verify: "none"}, false)
	linkStagingClaimRecord = oldLink
	if !errors.Is(err, linkErr) || !stagedCompleteRecord {
		t.Fatalf("failed atomic install error=%v, staged full record=%v", err, stagedCompleteRecord)
	}
	if _, err := os.Stat(metaPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed final link exposed a claim: %v", err)
	}
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed final link created a payload: %v", err)
	}
	// Simulate a crash leaving an incomplete temporary record. The next claim
	// must replace that non-authoritative temp and install one complete record.
	if err := os.WriteFile(temp, []byte(`{"instance":"interrupted`), 0o644); err != nil {
		t.Fatal(err)
	}
	state, err := claimStaging(context.Background(), staging, filepath.Join(dir, "fresh.bin"), metaPath,
		item, job, "commit", true, Settings{Verify: "none"}, false)
	if err != nil {
		t.Fatalf("claim after interrupted temp: %v", err)
	}
	defer state.release()
	meta, err := readStagingMeta(metaPath)
	if err != nil || meta.Instance != processInstanceToken || !meta.LockFenced {
		t.Fatalf("installed claim is not complete/current: meta=%+v err=%v", meta, err)
	}
	var decoded IncompleteMeta
	raw, err := os.ReadFile(metaPath)
	if err != nil || json.Unmarshal(raw, &decoded) != nil {
		t.Fatalf("installed record is not valid JSON: err=%v", err)
	}
}

func TestTakeoverReplacementFailurePreservesAndResumesValidatedRecord(t *testing.T) {
	const total, prefix = 8192, 4096
	full := rangedTestPayload(total)
	dir := t.TempDir()
	dst := filepath.Join(dir, "takeover.bin")
	staging := dst + ".incomplete"
	metaPath := staging + ".meta"
	job := Job{Repo: "o/r"}
	item := PlanItem{RelativePath: "takeover.bin", URL: "http://unused", Size: total, AcceptRanges: true}
	payload := make([]byte, total)
	copy(payload, full[:prefix])
	if err := os.WriteFile(staging, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	old := newClaimMeta(item, job, "commit", true)
	old.Instance = "dead-predecessor"
	old.PID = 99999999
	old.Host = "some-other-host"
	old.LockFenced = true
	old.Ranges = buildRangeLayout(total, 2)
	old.Ranges[0].Committed, old.Ranges[0].Done = prefix, true
	if err := writeStagingMeta(metaPath, &old); err != nil {
		t.Fatal(err)
	}
	oldBytes, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}

	temp := metaPath + ".tmp"
	if err := os.Mkdir(temp, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = claimStaging(context.Background(), staging, dst, metaPath, item, job, "commit", true, Settings{Verify: "none"}, false)
	if err == nil {
		t.Fatal("takeover unexpectedly succeeded when replacement temp creation failed")
	}
	gotMeta, err := os.ReadFile(metaPath)
	if err != nil || !bytes.Equal(gotMeta, oldBytes) {
		t.Fatalf("failed takeover lost/replaced predecessor metadata: read err=%v", err)
	}
	gotPayload, err := os.ReadFile(staging)
	if err != nil || !bytes.Equal(gotPayload, payload) {
		t.Fatalf("failed takeover changed predecessor payload: read err=%v", err)
	}
	if err := os.Remove(temp); err != nil {
		t.Fatal(err)
	}
	// Replace the injected failure with an interrupted replacement temp from a
	// crash-before-rename. Recovery must overwrite only the temp; the validated
	// predecessor remains authoritative until the complete successor is renamed.
	if err := os.WriteFile(temp, []byte(`{"instance":"interrupted-successor`), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, ranges := rangedTestServer(t, full)
	item.URL = srv.URL + "/takeover.bin"
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", job,
		Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "none"}, item, dst,
		multipartOpts{Commit: "commit", CommitPinned: true}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("resume after interrupted takeover replacement: %v", err)
	}
	assertExactFile(t, dst, full)
	if len(*ranges) != 1 || (*ranges)[0] != "bytes=4096-8191" {
		t.Fatalf("recovery ranges=%v, want only the validated missing suffix", *ranges)
	}
}

// TestRangedDownloadSucceedsWithoutHardLinkSupport covers a filesystem that
// cannot create hard links at all. The claim record must still install
// atomically, so the whole ranged path keeps working there: a hard link is
// not an authorized prerequisite of the download.
func TestRangedDownloadSucceedsWithoutHardLinkSupport(t *testing.T) {
	oldLink := linkStagingClaimRecord
	defer func() { linkStagingClaimRecord = oldLink }()
	linkStagingClaimRecord = func(src, dst string) error {
		return &os.LinkError{Op: "link", Old: src, New: dst, Err: syscall.EOPNOTSUPP}
	}

	const total = 8192
	full := rangedTestPayload(total)
	dir := t.TempDir()
	dst := filepath.Join(dir, "nolinks.bin")
	job := Job{Repo: "o/r"}
	item := PlanItem{RelativePath: "nolinks.bin", Size: total, AcceptRanges: true}
	srv, _ := rangedTestServer(t, full)
	item.URL = srv.URL + "/nolinks.bin"

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", job,
		Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "none"}, item, dst,
		multipartOpts{Commit: "commit", CommitPinned: true}, func(ProgressEvent) {}); err != nil {
		t.Fatalf("ranged download failed on a filesystem without hard-link support: %v", err)
	}
	assertExactFile(t, dst, full)

	// No staged residue may survive a successful install.
	metaPath := dst + ".incomplete.meta"
	if _, err := os.Stat(metaPath + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged claim record survived the install: %v", err)
	}
}

// TestFreshClaimInstallSucceedsWithoutHardLinkSupport covers the claim
// transition itself on a filesystem without hard-link support: the record must
// still be installed completely and be authoritative afterwards.
func TestFreshClaimInstallSucceedsWithoutHardLinkSupport(t *testing.T) {
	oldLink := linkStagingClaimRecord
	defer func() { linkStagingClaimRecord = oldLink }()
	linkStagingClaimRecord = func(src, dst string) error {
		return &os.LinkError{Op: "link", Old: src, New: dst, Err: syscall.EOPNOTSUPP}
	}

	dir := t.TempDir()
	staging := filepath.Join(dir, "nolinks.incomplete")
	metaPath := staging + ".meta"
	item := PlanItem{RelativePath: "nolinks.bin", Size: 4096, AcceptRanges: true}
	job := Job{Repo: "o/r"}

	state, err := claimStaging(context.Background(), staging, filepath.Join(dir, "nolinks.bin"), metaPath,
		item, job, "commit", true, Settings{Verify: "none"}, false)
	if err != nil {
		t.Fatalf("claim failed on a filesystem without hard-link support: %v", err)
	}
	defer state.release()

	meta, err := readStagingMeta(metaPath)
	if err != nil {
		t.Fatalf("installed claim record is unreadable: %v", err)
	}
	if meta.Instance != processInstanceToken || meta.Version != stagingMetaVersion {
		t.Fatalf("installed claim record is not complete/current: %+v", meta)
	}
	// The staged name must not survive the install: a hard-link install
	// leaves both names on one inode, so a surviving staged name would be an
	// alias of the authoritative record.
	if _, err := os.Stat(metaPath + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged claim record survived the install: %v", err)
	}
}

// TestStagedRecordResidueCannotEmptyPredecessor covers the crash window
// between installing a claim record and removing its staged copy: the staged
// path is then a hard link to the authoritative record, so both names share
// one inode. A later run that stages through the same path must not write
// through that alias: if the replacement then fails, the predecessor must
// still be intact rather than emptied.
func TestStagedRecordResidueCannotEmptyPredecessor(t *testing.T) {
	dir := t.TempDir()
	metaPath := filepath.Join(dir, "alias.incomplete.meta")
	predecessor := []byte(`{"instance":"predecessor","version":4}`)
	if err := os.WriteFile(metaPath, predecessor, 0o644); err != nil {
		t.Fatal(err)
	}
	// Crash residue: the staged path is a hard link to the authoritative
	// record, i.e. both names share one inode.
	staged := metaPath + ".tmp"
	if err := os.Link(metaPath, staged); err != nil {
		t.Fatal(err)
	}

	// The replacement fails after the record was staged. The predecessor must
	// survive byte-identical: writing through the staged alias would have
	// emptied it before the failed install.
	oldLink := linkStagingClaimRecord
	defer func() { linkStagingClaimRecord = oldLink }()
	linkErr := errors.New("injected claim install interruption")
	linkStagingClaimRecord = func(src, dst string) error {
		return linkErr
	}
	successor := IncompleteMeta{Instance: processInstanceToken, Version: stagingMetaVersion}
	if _, _, err := createStagingClaim(metaPath, successor); !errors.Is(err, linkErr) {
		t.Fatalf("claim install error=%v, want the injected failure", err)
	}
	got, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, predecessor) {
		t.Fatalf("failed replacement did not preserve the predecessor record: %q", got)
	}
}

// TestInterruptedInstallResidueDoesNotReplacePredecessor covers the same
// alias hazard combined with a filesystem that cannot create hard links: the
// install must not replace the record that is already there, and must not
// empty it through the staged alias.
func TestInterruptedInstallResidueDoesNotReplacePredecessor(t *testing.T) {
	dir := t.TempDir()
	metaPath := filepath.Join(dir, "install.incomplete.meta")
	predecessor := []byte(`{"instance":"predecessor","version":4}`)
	if err := os.WriteFile(metaPath, predecessor, 0o644); err != nil {
		t.Fatal(err)
	}
	// Crash residue: the staged path shares the authoritative record's inode.
	staged := metaPath + ".tmp"
	if err := os.Link(metaPath, staged); err != nil {
		t.Fatal(err)
	}

	oldLink := linkStagingClaimRecord
	defer func() { linkStagingClaimRecord = oldLink }()
	linkStagingClaimRecord = func(src, dst string) error {
		return &os.LinkError{Op: "link", Old: src, New: dst, Err: syscall.EOPNOTSUPP}
	}
	successor := IncompleteMeta{Instance: "other-instance", Version: stagingMetaVersion}
	if _, retry, err := createStagingClaim(metaPath, successor); err != nil {
		t.Fatalf("claim install failed: %v", err)
	} else if !retry {
		t.Fatal("an appeared record was replaced instead of being left authoritative")
	}
	got, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("authoritative record was emptied through the staged alias")
	}
	if !bytes.Equal(got, predecessor) {
		t.Fatalf("authoritative record was modified: %q", got)
	}
}
