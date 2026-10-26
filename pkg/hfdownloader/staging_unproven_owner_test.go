// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the operator-visible classification of an UNPROVEN
// staging record (issue #140 / PR #151, correction package B).
//
// A blob-keyed staging record that was written without the lifetime
// ownership lock fence (LockFenced=false, which no released version ever
// wrote) or that cannot be read/parsed is refused by the claim. That refusal
// is the owner-decided rule and it does not change: the record is preserved
// and never taken over, because no age-based takeover and no takeover
// without proof of same-host death is allowed.
//
// What was wrong is the REPORTING: the refusal surfaced through the same
// sentinel and the same message as a PROVEN live owner, so the operator was
// told "blob <sha> is being downloaded by another process; try again later"
// for a state where no live writer was proven to exist. The two conditions
// are now distinguishable: the unproven record carries its own sentinel
// (ErrStagingOwnershipUnproven) and keeps its own message, which says what
// the state is and names the staging pair the operator has to remove.
//
// The cases drive the real Download path, so they exercise the product
// routing and the operator-visible error, and they are deterministic: the
// seeded record is refused before any network transfer.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestUnprovenStagingRecordIsNotReportedAsLiveOwner pins the operator-visible
// distinction between the two refusals of a staging record:
//
//   - a PROVEN live owner keeps "being downloaded by another process; try
//     again later" (the condition is transient, the owner may publish at any
//     moment);
//   - an UNPROVEN record (legacy/unfenced, or unreadable) says that its
//     owner can be proven neither alive nor dead, that the partial is
//     preserved, and that removing the staging pair is the recovery.
//
// It also pins that the unproven record's artifacts are preserved: the
// staged payload and its record are byte-identical afterwards, nothing is
// published, and no bytes are transferred.
func TestUnprovenStagingRecordIsNotReportedAsLiveOwner(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cases := []struct {
		name string
		// seed writes the staging record of the case.
		seed func(t *testing.T, metaPath string)
	}{
		{
			name: "unfenced record",
			seed: func(t *testing.T, metaPath string) {
				t.Helper()
				meta := IncompleteMeta{
					PID: os.Getpid(), Instance: "legacy-instance", Host: "another-host",
					StartedAt: time.Now().UTC(), Size: total, SHA256: sha,
					Version: stagingMetaVersion, Repo: "owner/model", Path: "model.bin",
					Commit: "commit123", LockFenced: false,
					Ranges: buildRangeLayout(total, 1),
				}
				if err := writeStagingMeta(metaPath, &meta); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unreadable record",
			seed: func(t *testing.T, metaPath string) {
				t.Helper()
				if err := os.WriteFile(metaPath, []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cacheDir := t.TempDir()
			repo, err := NewHFCache(cacheDir, time.Minute).Repo("owner/model", RepoTypeModel)
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			staging := repo.IncompletePath(sha)
			metaPath := repo.IncompleteMetaPath(sha)
			partial := []byte("a legacy partial payload")
			if err := os.WriteFile(staging, partial, 0o644); err != nil {
				t.Fatal(err)
			}
			tc.seed(t, metaPath)
			beforeMeta, err := os.ReadFile(metaPath)
			if err != nil {
				t.Fatal(err)
			}

			srv, gets := proofRecoveryHub(t, full)
			cfg := Settings{
				CacheDir: cacheDir, Endpoint: srv.URL, Concurrency: 1, MaxActiveDownloads: 1,
				MultipartThreshold: "1KB", Verify: "size", Retries: 0, StallTimeout: "0",
			}
			var skips []string
			dlErr := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ev ProgressEvent) {
				if ev.Event == "file_done" && strings.Contains(ev.Message, "skip") {
					skips = append(skips, ev.Message)
				}
			})
			if dlErr == nil {
				t.Fatalf("Download of an unproven staging record reported success; skips=%v", skips)
			}
			// The unproven refusal is distinguishable from the proven live
			// owner: it carries its own sentinel, and it does NOT claim a
			// proven live owner.
			if !errors.Is(dlErr, ErrStagingOwnershipUnproven) {
				t.Errorf("Download error = %v, want an unproven-ownership error", dlErr)
			}
			if errors.Is(dlErr, errBlobOwnedByLiveWriter) {
				t.Errorf("Download error = %v, want no proven-live-owner claim for an unproven record", dlErr)
			}
			msg := dlErr.Error()
			if !strings.Contains(msg, "cannot be proven to belong to a live or dead owner") {
				t.Errorf("Download error = %q, want the unproven-ownership wording", msg)
			}
			if strings.Contains(msg, "is being downloaded by another process") {
				t.Errorf("Download error = %q, want no live-owner wording for an unproven record", msg)
			}
			// The message names the staging pair the operator has to remove:
			// the documented recovery of an unproven record is manual.
			if !strings.Contains(msg, staging) || !strings.Contains(msg, metaPath) {
				t.Errorf("Download error = %q, want the staging pair %s / %s named", msg, staging, metaPath)
			}
			// Nothing else is exposed: the item's URL is not part of the
			// message.
			if strings.Contains(msg, srv.URL) {
				t.Errorf("Download error = %q, want no endpoint in the message", msg)
			}
			if len(skips) != 0 {
				t.Errorf("the job recorded a successful skip although the record's owner is unproven: %v", skips)
			}
			// The artifacts are preserved: the refusal never takes the
			// record over and never removes the partial.
			got, rerr := os.ReadFile(staging)
			if rerr != nil || string(got) != string(partial) {
				t.Errorf("the unproven record's payload did not survive: %q, %v", got, rerr)
			}
			afterMeta, err := os.ReadFile(metaPath)
			if err != nil {
				t.Fatalf("the unproven record's record did not survive: %v", err)
			}
			if string(afterMeta) != string(beforeMeta) {
				t.Errorf("the unproven record was rewritten:\nbefore %s\nafter  %s", beforeMeta, afterMeta)
			}
			// Nothing was published, and no bytes were transferred.
			if _, serr := os.Stat(repo.BlobPath(sha)); !os.IsNotExist(serr) {
				t.Errorf("a blob was published although the record's owner is unproven, stat err=%v", serr)
			}
			if n := gets.Load(); n != 0 {
				t.Errorf("the unproven record triggered %d payload GETs, want 0", n)
			}
		})
	}
}

// TestUnprovenAndLiveOwnerErrorsDiffer pins that the two refusals of a
// staging record are told apart by their sentinel AND by their message, so
// a caller can react to them differently.
func TestUnprovenAndLiveOwnerErrorsDiffer(t *testing.T) {
	sha := strings.Repeat("a", 64)
	staging := "/cache/hub/models--owner--model/blobs/" + sha + ".incomplete"
	metaPath := staging + ".meta"

	live := blobLiveOwnerError(sha, "model.bin")
	unproven := newStagingUnprovenOwnerError(
		PlanItem{RelativePath: "model.bin", SHA256: sha}, staging, metaPath)

	if live.Error() == unproven.Error() {
		t.Fatalf("the two refusals share one message: %q", live.Error())
	}
	// The live owner is transient: the operator is told to retry later.
	if !errors.Is(live, errBlobOwnedByLiveWriter) {
		t.Errorf("the live-owner error lost its sentinel: %v", live)
	}
	if !strings.Contains(live.Error(), "is being downloaded by another process") ||
		!strings.Contains(live.Error(), "try again later") {
		t.Errorf("the live-owner message changed: %q", live.Error())
	}
	if errors.Is(live, ErrStagingOwnershipUnproven) {
		t.Errorf("the live-owner error carries the unproven sentinel: %v", live)
	}
	// The unproven record is not transient: the operator has to remove the
	// staging pair.
	if !errors.Is(unproven, ErrStagingOwnershipUnproven) {
		t.Errorf("the unproven error lost its sentinel: %v", unproven)
	}
	if errors.Is(unproven, errBlobOwnedByLiveWriter) {
		t.Errorf("the unproven error claims a proven live owner: %v", unproven)
	}
	if !strings.Contains(unproven.Error(), "cannot be proven to belong to a live or dead owner") ||
		!strings.Contains(unproven.Error(), "legacy or unreadable record") {
		t.Errorf("the unproven message changed: %q", unproven.Error())
	}
	if !strings.Contains(unproven.Error(), "the partial is preserved") {
		t.Errorf("the unproven message does not state that the partial is preserved: %q", unproven.Error())
	}
	if !strings.Contains(unproven.Error(), staging) || !strings.Contains(unproven.Error(), metaPath) {
		t.Errorf("the unproven message does not name the staging pair: %q", unproven.Error())
	}
	// Both refusals keep the shared "the staging file is not available to
	// this writer" classification, so the callers that key on it keep
	// working; only the unproven case carries the new sentinel.
	if !errors.Is(unproven, errBlobIncompleteOwned) {
		t.Errorf("the unproven error lost the shared staging-unavailable classification: %v", unproven)
	}
}

// TestUnprovenStagingReadErrorKeepsSentinelAndCause covers a record that
// EXISTS but cannot be read for a reason other than not-exist, such as the
// record path being a directory.
//
// The state is unproven for the same reason a malformed record is: the read
// failure proves neither a live owner nor a dead one, so the partial is
// preserved, the operator-visible message names the staging pair, and the
// caller can still distinguish the state with ErrStagingOwnershipUnproven.
// The underlying filesystem error stays inspectable as well.
func TestUnprovenStagingReadErrorKeepsSentinelAndCause(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "blob.incomplete")
	metaPath := staging + ".meta"
	// A directory at the record path makes os.ReadFile fail with a non-
	// not-exist error on every supported platform.
	if err := os.Mkdir(metaPath, 0o755); err != nil {
		t.Fatal(err)
	}
	it := PlanItem{RelativePath: "model.bin", SHA256: strings.Repeat("a", 64)}

	_, _, err := claimStagingLocked(staging, metaPath, it, Job{Repo: "owner/model"},
		"commit123", true, true, false, false)
	if err == nil {
		t.Fatal("an unreadable staging record was accepted")
	}
	if !errors.Is(err, ErrStagingOwnershipUnproven) {
		t.Errorf("error = %v, want the ErrStagingOwnershipUnproven sentinel", err)
	}
	if !errors.Is(err, errBlobIncompleteOwned) {
		t.Errorf("error = %v, want the shared staging-unavailable classification", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "cannot be proven to belong to a live or dead owner") {
		t.Errorf("error = %q, want the unproven-ownership wording", msg)
	}
	if !strings.Contains(msg, staging) || !strings.Contains(msg, metaPath) {
		t.Errorf("error = %q, want the staging pair %s / %s named", msg, staging, metaPath)
	}
	// The read failure is preserved, not replaced by the classification.
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		t.Errorf("error = %v, want the underlying filesystem error to stay inspectable", err)
	} else if pathErr.Path != metaPath {
		t.Errorf("underlying error path = %q, want %q", pathErr.Path, metaPath)
	}
}
