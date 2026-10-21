// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

/*
Package hfdownloader provides a Go library for downloading models and datasets
from the HuggingFace Hub with resume support, multipart downloads, and verification.

# Features

  - Resumable downloads: Interrupted downloads automatically resume from where they left off
  - Multipart downloads: Large files are downloaded in parallel chunks for faster speeds
  - LFS support: Handles Git LFS files transparently
  - Filtering: Download only specific files matching patterns (e.g., "q4_0", "gguf")
  - Verification: SHA-256, ETag, or size-based integrity verification
  - Progress events: Real-time progress callbacks for UI integration
  - Context cancellation: Full support for graceful shutdown via context

# Quick Start

Download a model with default settings:

	package main

	import (
		"context"
		"fmt"
		"log"

		"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
	)

	func main() {
		job := hfdownloader.Job{
			Repo:     "TheBloke/Mistral-7B-Instruct-v0.2-GGUF",
			Revision: "main",
			Filters:  []string{"q4_0"}, // Only download q4_0 quantization
		}

		cfg := hfdownloader.Settings{
			OutputDir:   "./Models",
			Concurrency: 8,
			Token:       "", // Set for private repos
		}

		err := hfdownloader.Download(context.Background(), job, cfg, func(e hfdownloader.ProgressEvent) {
			fmt.Printf("[%s] %s: %s\n", e.Event, e.Path, e.Message)
		})
		if err != nil {
			log.Fatal(err)
		}
	}

# Downloading Datasets

Set IsDataset to true for dataset repositories:

	job := hfdownloader.Job{
		Repo:      "facebook/flores",
		IsDataset: true,
	}

# Dry-Run / Planning

Get the file list without downloading:

	plan, err := hfdownloader.PlanRepo(ctx, job, cfg)
	if err != nil {
		log.Fatal(err)
	}

	for _, item := range plan.Items {
		fmt.Printf("%s (%d bytes, LFS=%v)\n", item.RelativePath, item.Size, item.LFS)
	}

# Progress Events

The ProgressFunc callback receives events throughout the download:

  - scan_start: Repository scanning has begun
  - plan_item: A file has been added to the download plan
  - file_start: Download of a file has started
  - file_progress: Periodic progress update during download
  - file_finalizing: Transfer finished; local finalization (strict hash verification, publish rename, cache entries) is running
  - file_done: File download complete (or skipped)
  - retry: A retry attempt is being made
  - error: An error occurred
  - done: All downloads complete

# Filter Matching

Filters are matched case-insensitively against repository-relative paths of any
file type, not only LFS files. Multiple filters can be specified; a file
matches if its path contains any filter string. With ExactMatch, filters select
by repository-relative path form: a trailing "/" selects descendants of a
directory, a leading-dot extension such as ".safetensors" selects that
extension, a filter containing "/" is a complete relative path, "./name" selects
only the literal root-level basename, and any other filter matches a whole
delimiter-bounded path component (a directory name or the basename). Recognized
dataset group filters also select data by membership. A filtered request with
no surviving direct or membership selection returns `ErrNoFilterMatch`:

	job := hfdownloader.Job{
		Repo:    "TheBloke/Llama-2-7B-GGUF",
		Filters: []string{"q4_0", "q5_0", "q8_0"},
	}

With AppendFilterSubdir, matched files are organized into subdirectories:

	job := hfdownloader.Job{
		Repo:               "TheBloke/Llama-2-7B-GGUF",
		Filters:            []string{"q4_0", "q5_0"},
		AppendFilterSubdir: true,  // Creates q4_0/ and q5_0/ subdirectories
	}

The public `PlanItem.DestinationSubdir` is the candidate relative prefix for a
planned item, separate from `PlanItem.Subdir` filter attribution. It is
populated even when `AppendFilterSubdir` is disabled. The downloader applies it
only when the job enables `AppendFilterSubdir`; consumers must check that flag
before combining it with `PlanItem.RelativePath` to determine the download
destination. `ScanPlan` preview event paths remain repository-relative.

# Resume Behavior

Resume is always enabled. Multipart downloads write directly into one
staging file (blobs/<sha>.incomplete in HF cache mode) and persist their
committed byte ranges in versioned metadata next to it, so an interrupted
transfer resumes from its durable high-water marks without re-downloading
valid bytes — even when the connection count changes between runs. Skip
Range-record version 3 marks ranges Done only after the response body reaches clean EOF;
version-2 Done records reserve the final byte for revalidation because the
older writer could infer completion from bytes written alone. Skip
decisions are filesystem-based:

  - LFS files: Compared by SHA-256 hash (if available in metadata)
  - Non-LFS files: Compared by file size

That pre-download decision is authoritative for an arbitrary destination. A
staging claim treats an existing publish target as completion proof only when
the target is content-addressed (blobs/<known-sha>); a legacy/output file or a
tmp-<sha> staging name that the skip check declined is downloaded again and
atomically renamed over the old bytes. Publish re-stats a content-addressed
target under the ownership locks and never overwrites one that appeared while
the transfer was in flight: it discards its own staging pair and reports the
item as complete.

Staging identity includes `plan.Commit`. If fetching repository info fails or
returns an empty SHA, the planner uses the requested revision string as the
fallback commit identity; that value is not necessarily an immutable commit.

Existing multipart resume state survives a threshold change. Before routing an
item to the single-request path, the downloader detects a usable pre-#140
`.part-NN`/`.parts.json` layout and a valid versioned
`<staging>.incomplete`/`.meta` pair (including the blob-keyed staging pair for
LFS cache items). Version-2 records are migrated conservatively by re-fetching
the final byte of completed ranges to validate response framing. A ranged-
capable item keeps the multipart route regardless of `MultipartThreshold`, so
the state is consumed through the existing migration/resume and content-proof
rules; a fresh item with no state keeps the configured routing. If the server or
plan does not support ranges, the downloader emits an explicit warning,
downloads the full file through the single path, and removes the obsolete
pair only after the final file is durable (claim-checked); the legacy family is
removed by the single-path success cleanup.

Exactly one live writer may own a staging file. The exclusion fence is an
OS-level exclusive lock on the persistent sibling file `<staging>.lock`,
acquired before the claim transition and held for the whole ownership interval
— claim read, create/resume, transfer, hashing, publish/cleanup. A busy lock
means a live owner and a claimant skips the file. The lock is released by the
kernel when the process exits, so a crashed owner is recoverable immediately; a
suspended (but alive) process keeps ownership, and there is deliberately no
take-over while it is alive — terminate it to recover. Unix uses
flock(LOCK_EX|LOCK_NB); Windows uses an exclusive-share CreateFile handle
(dwShareMode=0) held on the file handle. The record persists whether its claim
was OS-lock fenced (`LockFenced`); on a supported platform a free lock is then
authoritative for CheckBlob and reports the record stale even when its stored
PID is still live, so a reused PID cannot hide a resumable crash state. The same
distinction applies to claims and cleanup: a free lock is authoritative only
for a LockFenced record. A legacy/unfenced record belongs to a writer that did
not hold the OS lock, so a new process preserves it unless same-host process
evidence proves the owner dead; age or ambiguous identity is not sufficient.
The ownership probe itself is non-owning (a shared lock on Unix, a
shared-access open on Windows), so concurrent probes are never mistaken for a
writer.

The lock file therefore has two roles, and a claimant must tell them apart
before it acts on a busy lock. On Unix a status probe's shared flock refuses a
claimant's exclusive flock on the same inode — even within one process, because
flock locks belong to the open file description, not the process.

Only the real lifetime writer may take the exclusive lock. Every observation —
the status probe and the contention check alike — stays non-owning, because an
observer that took the exclusive lock would itself look like a live writer to
every peer: a concurrent claimant or status probe would then skip files that
nobody is downloading. The observation therefore asks one question with one
atomic, non-owning operation: is an exclusive holder present? A shared probe
is granted unless an exclusive holder exists, so a refused shared probe proves
a live writer (the claimant skips, exactly as before), and a granted shared
probe proves no exclusive holder is present (the claimant retries its own
acquisition within a bounded, cancellable window). A granted shared probe does
not distinguish "a reader holds it" from "nobody holds it", and it does not
need to: the real acquisition is the only operation that resolves that case.
A contention that never resolves is reported as an actionable failure, never as
a successful skip, because no live writer is proven to exist. A status probe in
flight can therefore never turn a resumable download into a silently skipped
file.

The claim record remains the identity/resume state and is heartbeated through
transfers, retry waits, strict verification and finalization. Per-path in-process
metadata gates serialize record transitions while the lifetime OS lock excludes
current-version foreign mutators. Every mutation re-reads claim identity; a
heartbeat cannot resurrect a replaced claim. The claim carries a per-process
instance token plus PID/host diagnostics, so PID reuse and containers sharing
PID 1 are not treated as the same writer. Publish checks the record and held OS
lock before rename; a missing or foreign record is refused with
`errStagingClaimLost`. If the OS lock is unsupported or cannot be acquired,
mutations fail before changing a claim or payload and preserve existing partial
data; this runtime error does not declare permanent platform support policy.
Age never authorizes transition recovery. An unfenced legacy record is
protected unless its same-host owner is provably dead.

A claim record is always installed as a complete, valid document. The record
is staged in a sibling temp file and then installed by one atomic operation: a
hard link of the staged inode, which is atomic and never replaces a record
that already appeared, or — on a filesystem that cannot create hard links — an
atomic rename, used only while no record is present. Hard-link support is
therefore not a prerequisite for downloading. The staged copy is always a
fresh file, so a staged path left behind by an interrupted operation, which
may share the authoritative record's inode, is never written through and can
never empty the authoritative record before it is replaced.

The single-request path follows the same validation model. A resumed `.part` is
a sequential prefix with no record, so it is never assumed complete: every
ranged (206) response must match the exact requested Content-Range, expected
total and body length, writes are bounded to the requested range, and the file
must have exactly the expected size before the rename. A reader/copy error after
the final requested byte was written is not successful framing: publication is
refused, and the last byte is left uncommitted for a nonempty retry/restart
range. When the expected size is unknown, the partial is discarded rather than
guessed at. Known-digest
verification happens before publication on this path too: in Verify "sha256"
and "etag" modes a fresh or resumed transfer is proven with one full-file
SHA-256 pass before the rename, and in "size" mode inherited `.part` bytes are
proven once (the one-time migration exception). A mismatch leaves the
destination untouched and removes the partial. Verify "none" inherits that
mode's no-hash semantics. An ignored-Range 200 response still restarts the
partial from zero.

Legacy migration: a valid pre-#140 `.part-NN`/`.parts.json` family, or a leftover
single `.part` sequential prefix, is copied into the staging file once and the
obsolete artifacts are removed only after the migrated range layout is durable.
An oversized single `.part` is invalid and is discarded rather than clamped. A
layout whose every part is missing, empty or oversized contributes no usable
bytes and is not a migrated state: the unusable family is discarded and a valid
single `.part` is migrated instead. Migrated bytes are tagged LegacyMigrated:
they have layout/prefix structure but no validated provenance, so when the item
carries a known SHA-256 and Verify is not "none" they receive one full-file
SHA-256 proof before publication, even in "size" mode (the documented one-time
migration exception). A mismatch discards the staging data and never publishes.
With Verify "none" they inherit that mode's no-hash semantics.

Durable proof gate: a multipart staging record persists a `ProofPending` flag.
It is set and flushed before any known-digest proof starts (fresh sha256/etag,
migrated bytes) and cleared and flushed only after the proof succeeds,
immediately before publication. On resume the flag forces the proof in every
mode except "none" — including "size" — whenever the item carries a known
digest, so a crash mid-proof, a mismatch whose cleanup fails, or a later
Verify-mode change cannot publish unproven all-Done
ranges. On a mismatch the flag stays set, poison (a zero-length payload) is
attempted only while the claim is still owned under the metadata gate and
lifetime OS lock, and
the owned discard is attempted; if ownership cannot be verified the poison is
skipped and the durable flag remains the guard, so the next run re-proves or
discards. Publication itself is refused while a known digest's proof is still
pending. Verify "none" is the explicit no-hash opt-out. The single-request
`.part` has no separate flag: when a proof is required (sha256/etag, inherited
bytes, or a known digest with unknown size) it is renamed only after that proof,
so every restart re-proves it; a fresh size transfer with a known size follows
the size contract and may rename without a hash.

# Verification Options

The Settings.Verify field controls post-download verification, for LFS
files included:

  - "none": No post-download content hash (fastest); a ProofPending record is
    ignored and publication proceeds (explicit opt-out)
  - "size": Verify expected size and complete validated transfer coverage on
    both transfer paths (default); no full-file SHA-256 reread on the normal
    transfer path. The pre-download skip check over an existing file, the
    one-time legacy migration proof, the inherited single-path `.part` proof,
    and a known digest with no known expected size may still hash. A resumed
    ProofPending record forces the known-digest proof even in size mode after a
    crash, a failed mismatch cleanup, or a mode change
  - "etag": Historical compatibility: a known SHA-256 digest is proven before
    publication with at most one pass (the same pre-publish engine proof used
    by "sha256"); non-LFS ETag-only files keep their historical no-post-check
    behavior
  - "sha256": Full SHA-256 hash verification (most secure, slower); at most
    one full-file read per file, always before publication when the item
    carries a known digest. When no known digest exists, the HEAD-ETag
    fallback remains a post-publication check and can report a mismatch then

# Concurrency

Two levels of concurrency are configurable:

  - Concurrency: Number of parallel HTTP connections per file (for multipart downloads)
  - MaxActiveDownloads: Maximum number of files downloading simultaneously

# Error Handling

Download returns the first error encountered. The context can be used for
cancellation, and canceled downloads can be resumed on the next run.

# Authentication

For private or gated repositories, set the Token field in Settings:

	cfg := hfdownloader.Settings{
		Token: "hf_xxxxxxxxxxxxx", // Your HuggingFace access token
	}

Tokens can be generated at: https://huggingface.co/settings/tokens
*/
package hfdownloader
