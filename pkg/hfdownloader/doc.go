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

Exactly one live writer may own a staging file. Every record transition (claim,
take-over, checkpoint, heartbeat, removal) is serialized across processes by a
sibling O_EXCL lock file, and every mutation re-reads the record under that
lock: a heartbeat can never race a take-over or resurrect ownership. The claim
carries a per-process instance token plus PID/host diagnostics and heartbeat
freshness, so PID reuse and containers sharing PID 1 are not treated as the
same writer; a dead or stale owner remains recoverable. Cleanup removes a
staged pair only while no live claim remains.

Legacy migration: a valid pre-#140 `.part-NN`/`.parts.json` family, or a leftover
single `.part` sequential prefix, is copied into the staging file once and the
obsolete artifacts are removed only after the migrated range layout is durable.
An oversized single `.part` is invalid and is discarded rather than clamped.
Migrated bytes are tagged LegacyMigrated: they have layout/prefix structure but
no validated provenance, so when the item carries a known SHA-256 and Verify is
not "none" they receive one full-file SHA-256 proof before publication, even in
"size" mode (the documented one-time migration exception). A mismatch discards
the staging data and never publishes. With Verify "none" they inherit that
mode's no-hash semantics.

# Verification Options

The Settings.Verify field controls post-download verification, for LFS
files included:

  - "none": No post-download content hash (fastest)
  - "size": Verify expected size and complete transfer coverage (default);
    no full-file SHA-256 reread on the normal transfer path. The
    pre-download skip check over an existing file and the one-time legacy
    migration proof may still hash
  - "etag": Historical compatibility: LFS files with a known SHA-256 still
    receive the hash they always received (at most one pass); non-LFS
    files keep their historical no-post-check behavior
  - "sha256": Full SHA-256 hash verification (most secure, slower); at
    most one full-file read per file

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
