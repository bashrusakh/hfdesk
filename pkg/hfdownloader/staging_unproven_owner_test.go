// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers the operator-visible distinction between a proven live owner and an
// unproven record. Unproven state is preserved and rejected before transfer.

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

func TestUnprovenStagingRecordIsNotReportedAsLiveOwner(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	cases := []struct {
		name string
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
			// The version-discard branch runs only AFTER the ownership gate:
			// an unfenced old-version record whose owner is neither provably
			// dead nor this process is unproven, not discardable resume state.
			name: "unfenced old-version record",
			seed: func(t *testing.T, metaPath string) {
				t.Helper()
				meta := IncompleteMeta{
					PID: os.Getpid(), Instance: "legacy-instance", Host: "another-host",
					StartedAt: time.Now().UTC(), Size: total, SHA256: sha,
					Version: 3, Repo: "owner/model", Path: "model.bin",
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
			repo, err := NewHFCache(cacheDir).Repo("owner/model", RepoTypeModel)
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
			// Unproven ownership has its own sentinel and must not claim a proven live writer.
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
			// The message must not disclose the item's endpoint URL.
			if strings.Contains(msg, srv.URL) {
				t.Errorf("Download error = %q, want no endpoint in the message", msg)
			}
			if len(skips) != 0 {
				t.Errorf("the job recorded a successful skip although the record's owner is unproven: %v", skips)
			}
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
			if _, serr := os.Stat(repo.BlobPath(sha)); !os.IsNotExist(serr) {
				t.Errorf("a blob was published although the record's owner is unproven, stat err=%v", serr)
			}
			if n := gets.Load(); n != 0 {
				t.Errorf("the unproven record triggered %d payload GETs, want 0", n)
			}
		})
	}
}

// Live-owner and unproven-state errors need distinct sentinels and recovery messages.
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
	// Unproven-state diagnostics name the staging pair for documented manual recovery.
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
	// The unproven error also preserves the shared errBlobIncompleteOwned classification.
	if !errors.Is(unproven, errBlobIncompleteOwned) {
		t.Errorf("the unproven error lost the shared staging-unavailable classification: %v", unproven)
	}
}

// An existing unreadable record is unproven, not absent: preserve its partial
// and recovery diagnostic while retaining the filesystem cause for errors.As.
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
		"commit123", true, false)
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
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		t.Errorf("error = %v, want the underlying filesystem error to stay inspectable", err)
	} else if pathErr.Path != metaPath {
		t.Errorf("underlying error path = %q, want %q", pathErr.Path, metaPath)
	}
}
