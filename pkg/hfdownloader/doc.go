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
decisions are filesystem-based:

  - LFS files: Compared by SHA-256 hash (if available in metadata)
  - Non-LFS files: Compared by file size

That pre-download decision is authoritative for an arbitrary destination. A
staging claim treats an existing publish target as completion proof only when
the target is content-addressed (blobs/<known-sha>); a legacy/output file or a
tmp-<sha> staging name that the skip check declined is downloaded again and
atomically renamed over the old bytes.

Exactly one live writer may own a staging file. The exclusion fence is an
OS-level exclusive lock on the persistent sibling file `<staging>.lock`,
acquired before the claim transition and held for the whole ownership interval
— claim read, create/resume, transfer, hashing, publish/cleanup. A busy lock
means a live owner and a claimant skips the file. The lock is released by the
kernel when the process exits, so a crashed owner is recoverable immediately; a
suspended (but alive) process keeps ownership, and there is deliberately no
take-over while it is alive — terminate it to recover. Unix uses
flock(LOCK_EX|LOCK_NB); Windows uses an exclusive-share CreateFile handle
(dwShareMode=0) held on the file handle.

The claim record remains the identity/resume state and is heartbeated through
transfers, retry waits, strict verification and finalization. Every record
transition (claim, take-over, checkpoint, heartbeat, removal) is additionally
serialized across processes by a sibling `O_CREATE|O_EXCL` lock file, and every
mutation re-reads the record under that lock: a heartbeat can never race a
take-over or resurrect ownership. The claim carries a per-process instance
token plus PID/host diagnostics, so PID reuse and containers sharing PID 1 are
not treated as the same writer. Publish re-checks the record under the
transition lock and re-checks the OS lock before the rename; a missing or
foreign record is refused with errStagingClaimLost and the staging file is left
alone for its actual owner. On platforms or filesystems without an OS lock
primitive (for example plan9, js/wasm, Solaris/AIX, or a share that cannot
represent the lock), the transition-lock/heartbeat lease remains the fallback:
identity and heartbeat freshness decide ownership, with the weaker guarantee
that an in-flight handle cannot be fenced from a claimant.

The single-request path follows the same validation model. A resumed `.part` is
a sequential prefix with no record, so it is never assumed complete: every
ranged (206) response must match the exact requested Content-Range, expected
total and body length, writes are bounded to the requested range, and the file
must have exactly the expected size before the rename. When the expected size
is unknown, the partial is discarded rather than guessed at. Known-digest
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
digest, so a crash mid-proof, a mismatch whose cleanup cannot take the
transition lock, or a later Verify-mode change cannot publish unproven all-Done
ranges. On a mismatch the flag stays set, the payload is truncated to zero
(poisoned), and the owned discard is attempted; if the discard cannot acquire
the transition lock, the next run re-proves or discards. Publication itself is
refused while a known digest's proof is still pending. Verify "none" is the
explicit no-hash opt-out. The single-request `.part` needs no separate flag: it
is renamed only after the proof, so any restart treats its bytes as inherited
and re-proves them in every non-"none" mode.

# Verification Options

The Settings.Verify field controls post-download verification, for LFS
files included:

  - "none": No post-download content hash (fastest); a ProofPending record is
    ignored and publication proceeds (explicit opt-out)
  - "size": Verify expected size and complete validated transfer coverage on
    both transfer paths (default); no full-file SHA-256 reread on the normal
    transfer path. The pre-download skip check over an existing file, the
    one-time legacy migration proof, and the inherited single-path `.part`
    proof may still hash. A resumed ProofPending record forces the known-digest
    proof even in size mode after a crash, a failed mismatch cleanup, or a mode
    change
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
