// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

/*
Package hfdownloader provides a Go library for downloading models and datasets
from the HuggingFace Hub with resume support, multipart downloads, and verification.

This package documentation is the canonical, detailed contract for downloader
and cache correctness. AGENTS.md carries a short agent-level summary and points
here; docs/API.md describes the user/API-facing surface; CHANGELOG.md records
user-visible release notes. When a downloader rule changes, change this file
first and update the summaries only as far as their audience needs.

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

`file_finalizing` is emitted before the pre-publication proof (including for an
exact-size inherited partial), so the UI can show finalization.

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

`AppendFilterSubdir` is limited to single-component group/token filters because
the filter becomes a destination directory name. `PlanRepo` and `Download`
reject a path-shaped filter (one containing "/" or "\", including a "./" prefix
or trailing separator, or the "."/".." components) with
`ErrAppendFilterSubdirPathFilter` before any network or plan work. Such filters
remain valid when `AppendFilterSubdir` is disabled.

The public `PlanItem.DestinationSubdir` is the candidate relative prefix for a
planned item, separate from `PlanItem.Subdir` filter attribution. It is
populated even when `AppendFilterSubdir` is disabled. The downloader applies it
only when the job enables `AppendFilterSubdir`; consumers must check that flag
before combining it with `PlanItem.RelativePath` to determine the download
destination. `ScanPlan` preview event paths remain repository-relative.

# Ownership: one destination domain

Exactly one live writer may own a destination. The exclusion fence is the
destination ownership domain: an in-process slot keyed by the destination plus
an OS-level exclusive lock on its persistent domain lock file. The item acquires
the domain ONCE, after its pre-download skip decision and before the HEAD/claim,
and holds it for the whole ownership interval: claim read, create/resume,
transfer, hashing, publication/store and cancel cleanup. It is released when the
item returns.

The domain identity is name-based and differs by destination kind:

  - A content-addressed cache item (a supplied canonical SHA-256) uses the
    destination name `blobs/tmp-<sha>` (the single-request staging name and the
    destination key, even when a ranged LFS transfer writes directly to
    `blobs/<sha>.incomplete`). Its name is the digest, so its domain is exactly
    the blob key's domain: key `blobs/<sha>`, lock `blobs/tmp-<sha>.lock`. The
    publication's `holdsBlobKey` check applies to this key.
  - An item without a known digest (the planner permits an empty SHA-256) stages
    at the STRUCTURALLY NON-CANONICAL `blobs/tmp-path-<sanitized relative path>`.
    Its domain key is `blobs/path-<sanitized relative path>` and its lock is
    `blobs/tmp-path-<sanitized relative path>.lock`. "path-" is not a 64-hex
    digest, so neither identity can be reached by a blob key or lock, and the
    staging name can never equal a content-addressed `tmp-<sha>` name.
  - A legacy/output (flat) destination has no `blobs/`-style internal identity,
    so it borrows the multipart staging name: the domain key is the destination
    path and the lifetime lock is the persistent sibling
    `<destination>.incomplete.lock`. No new artifact name appears next to the
    user's model file. Because every route takes that domain, every flat
    TRANSFER leaves the persistent zero-byte
    `<destination>.incomplete.lock` next to the file, even when no multipart
    transfer ran: the shared domain/staging lock is created by the item-level
    acquisition, not by a multipart transfer. A destination skipped before that
    acquisition (an already complete file) leaves no lock. The unknown-size
    hand-off from the multipart route to
    the single-request path keeps the domain the item already holds instead of
    taking it a second time.

The `path-` prefix is REQUIRED, not cosmetic. Without it a digest-less item's
destination name was `blobs/tmp-<sanitized path>`, so file A at path `SHA256(Y)`
(content X) held the domain `blobs/SHA256(Y)` while file B at path `SHA256(X)`
(content Y) held `blobs/SHA256(X)`; A's store then needed the blob domain of its
computed digest `SHA256(X)` and B's store needed `SHA256(Y)`, and each waited for
the domain the other held. The in-process wait is unbounded (bounded only by the
job's context), so the job hung until cancelled; returning transfer slots does
not break an ownership cycle. The same prefix also prevents a digest-less item
from sharing a content-addressed `tmp-<sha>` staging file. Because the staging
name is unambiguous, the domain stays name-based, the writer and the cleanup
agree on ONE identity, and the two-item deadlock and the staging-name collision
cannot be constructed. The user-visible final/snapshot paths do not change;
only the internal staging name does.

Every writer of `blobs/<sha>` participates in that blob key's one domain,
including the exported `FinalizeBlob`, which publishes through the shared
no-replace primitive instead of renaming over the target. The two routes write
the same key — the ranged route stages at `blobs/<sha>.incomplete` and publishes
onto `blobs/<sha>`, the single-request route stages at `blobs/tmp-<sha>.part`
and publishes through the cache store onto `blobs/<sha>` — so one domain, not
one per staging path, is what prevents one route from adopting or deleting the
other's LIVE partial and what prevents the "the blob does not exist yet"
decision and the placement that follows it from being overtaken.

A busy lock means a live owner, and a claimant does not skip the file: the file
is not present while the owner holds it, so the claimant waits for the
destination's ownership domain (bounded, context-aware, never a takeover) and
re-checks the published content-addressed target afterwards. When the owner
released the domain without publishing, the writer downloads the file itself.
A flat or path-derived destination has no published content-addressed target,
so it always downloads the file itself after the wait. The lock is released by
the kernel when the process exits, so a crashed owner is recoverable
immediately; a suspended (but alive) process keeps ownership, and there is
deliberately no take-over while it is alive — terminate it to recover. Unix
uses `flock(LOCK_EX|LOCK_NB)`; Windows uses an exclusive-share `CreateFile`
handle (`dwShareMode=0`) held on the file handle.

The wait budget is bounded and derived from the existing ownership-lock
convention: the download budget is 60 times the 5s acquisition window, so the
effective budget is 5 MINUTES. It is a package-level knob, not a user setting,
and the wait runs inside the item's goroutine. When the budget expires with the
owner still present, the item fails with the actionable live-owner error ("blob
<sha> is being downloaded by another process; try again later", or the item's
path when it has no blob key) and the job does not report success while that
file is missing.

If the domain lock cannot be acquired (unsupported, denied, or filesystem
error), the operation fails before mutating claims or payloads and preserves
existing partial data. This is an operation-time safety error, not a permanent
platform-support policy. The domain lock file is persistent: it is never
unlinked (that would let two processes own different inodes) and is excluded
from cache statistics, listings, mirrors and cache deletion like every other
`.lock` artifact.

The multipart staging claim takes NO OS lock of its own and registers no second
in-process owner. It receives the held domain, verifies that the domain covers
its staging path (a nil, released or non-covering domain fails the claim
closed), performs its record transitions
under the per-path in-process metadata gate, and records whether its claim was
fenced by the domain (`LockFenced`). Every current claim and take-over is
fenced, so this code writes `LockFenced` true. That gate is intra-item serialization, not
cross-writer exclusion: while the destination domain is held no competing
writer exists. The old second `<staging>.lock` fence is gone and must not be
reintroduced: in flat/output mode it was literally the same file as the domain
lock, and a writer that took it from inside the transfer would contend with
itself. MIXED-VERSION WRITERS ARE INTENTIONALLY UNSUPPORTED: a branch-only
build that still held `<staging>.lock` instead of the destination domain is not
supported: a fenced (or provably dead/self) old/unsupported record is discarded,
while an unfenced record whose owner cannot be proven dead — of any version —
is refused as unproven and preserved, and no current code competes for its
obsolete lock. Do not reintroduce a compatibility path for mixed-version
writers.

The same distinction applies to claims and cleanup: a free domain lock is
authoritative only for a `LockFenced` record. An unfenced record belongs to a
writer that did not hold the domain, so a new process preserves it unless
same-host process evidence proves the owner dead; age, ambiguous host/PID
identity, or a free new lock is not sufficient. A free domain lock plus a
lock-fenced record is stale; a free domain lock plus an unfenced record is not
stale: the record is preserved and the claim refuses it with the
unproven-ownership error.

The ownership probe itself is non-owning (a shared lock on Unix, a shared-access
open on Windows), so concurrent probes are never mistaken for a writer. The
domain lock file therefore has two roles, and an acquisition must tell them
apart before it acts on a busy lock. On Unix a status probe's shared flock
refuses a claimant's exclusive flock on the same inode — even within one
process, because flock locks belong to the open file description, not the
process. Only the real lifetime writer may take the exclusive lock. Every
observation — the status probe and the contention check alike — stays
non-owning, because an observer that took the exclusive lock would itself look
like a live writer to every peer. The observation asks one question with one
atomic, non-owning operation: is an exclusive holder present? A shared probe is
granted unless an exclusive holder exists, so a refused shared probe proves a
live writer (the download path waits and re-checks the published target; the
cleanup leaves the artifacts alone), and a granted shared probe proves no
exclusive holder is present (the acquisition retries its own attempt within a
bounded, cancellable window). A granted shared probe does not distinguish "a
reader holds it" from "nobody holds it", and it does not need to: the real
acquisition is the only operation that resolves that case. A contention that
never resolves is reported as an actionable failure, never as a successful
skip, because no live writer is proven to exist.

CheckBlob reports three ownership outcomes and never guesses. It probes the
destination domain lock, `blobs/tmp-<sha>.lock`, the fence every writer of the
destination holds; the obsolete `<staging>.lock` of earlier branch-only builds
is deliberately not consulted, because no current writer takes it and treating
it as ownership would report a live writer that does not exist. A PROVEN live
owner (the domain lock is held exclusively) is `BlobDownloading`; the caller
must NOT skip that item, because the file is not present while the owner holds
it. A provably dead owner is `BlobStale` and may be resumed. A free lock plus
an unreadable or malformed record is also `BlobStale`, but conservatively: the
report does not prove the record is resumable, and the claim refuses it as
unproven and preserves it. An unknown
ownership state — the ownership probe failed, or the platform cannot represent
the lock — is `BlobOwnershipUnknown` together with an error that wraps
`ErrBlobOwnershipUnknown`. The caller surfaces that failure instead of skipping
the item, because a skip would report a successful download with the blob still
missing; the incomplete payload is preserved so a retry can resume it.

The claim record is diagnostics/resume state, not a freshness-based ownership
lease. Ownership is the held destination domain plus `LockFenced`. The record
is not rewritten on a timer and has no heartbeat; record age is never an
ownership signal. The former age-based configuration (`Settings.StaleTimeout`,
`HFCache.StaleTimeout` and `DefaultStaleTimeout`) has been removed; do not
reintroduce an age-based timeout, lease or takeover input. Every mutation
re-reads claim identity, so a checkpoint cannot resurrect a replaced claim. The
claim carries a per-process instance token plus PID/host diagnostics, so PID
reuse and containers sharing PID 1 are not treated as the same writer. Publish
checks the record and the still-held domain lock before rename, and checks
cancellation immediately before the rename; a missing or foreign record is
refused with `errStagingClaimLost` and is never renamed. Record transitions are
serialized by the per-staging metadata mutex;
the destination domain (held by the item) excludes foreign current-version
mutators and makes that gate intra-item state.

A claim record is always installed as a complete, valid document. The record is
staged in a sibling temp file and then installed by one atomic operation: a hard
link of the staged inode, which is atomic and never replaces a record that
already appeared, or — on a filesystem that cannot create hard links — an
atomic rename, used only while no record is present. Hard-link support is
therefore not a prerequisite for downloading. The staged copy is always a fresh
file, so a staged path left behind by an interrupted operation, which may share
the authoritative record's inode, is never written through and can never empty
the authoritative record before it is replaced.

The destination-domain waiter does not hold one of the job's
`MaxActiveDownloads` slots. The slot is a per-item lease that the ownership
acquisition itself returns for the duration of every wait (the in-process
domain slot and the cross-process OS-lock wait alike) and takes back before it
returns a domain the caller will transfer under. A failed acquisition keeps the
slot returned, so the item reports the error instead of transferring. The
multipart claim takes no ownership wait of its own and does not move the slot;
the lease belongs only to the domain acquisition. The acquisition's waits are
unchanged: an in-process contender waits for the current owner bounded by the
item's context, a cross-process PROVEN live owner is waited out until the
ownership budget expires and is then reported with the actionable ownership
error, and the local metadata gate is waited out within `stagingLockWait`. The
slot therefore bounds concurrent TRANSFERS, not waiters: any number of items may
wait, and the other
files of the job proceed while one item waits, even with a low limit. Waiting
items are still goroutines bounded by the job's file set, and cancellation stays
prompt. An item never blocks on a domain while holding a slot, and two
contenders for one destination cannot both hold its domain, so the wait cannot
deadlock. While the item waits it emits no progress events, so its byte counter
does not move; when a budget expires or a live owner is reported the item fails
with the actionable error and the job's status is failed, not completed.

# Resume identity

Resume is always enabled. Multipart downloads write directly into one staging
file (`blobs/<sha>.incomplete` in HF cache mode, `<destination>.incomplete`
otherwise) and persist their committed byte ranges in versioned metadata next
to it, so an interrupted transfer resumes from its durable high-water marks
without re-downloading valid bytes — even when the connection count changes
between runs. Range-record version 4 marks ranges Done only after the response
body reaches clean EOF. It is the ONLY resumable version. A FENCED record of
any other version — including a branch-only version-3 record whose
`legacy_migrated` flag described imported bytes — is discarded under the
claim's ownership and local metadata serialization, and the file is
re-downloaded from zero. So is an unfenced old-version record whose owner is
provably dead or is this process. An UNFENCED old-version record whose owner
cannot be proven dead is refused as unproven before the version-discard branch
and preserved. Partial downloads from old formats are never migrated; a warning
is logged when old artifacts are discarded.

A current version-4 record is resumable only when its identity matches the
current item exactly: same repository, resolved commit, relative path and
expected size; the identity is PINNED on both sides (the record's own
`commit_pinned` flag and the current plan's); the blob SHA is present on both
sides or neither; the range layout tiles the file with normalized completion
(Done only after clean framing); and the payload exists at the expected logical
size before any create/truncate. Otherwise the record is discarded and the
payload is downloaded again. An UNPINNED identity pins nothing — a moved branch
may change the content under that name — so no staged bytes are inherited from
it, and for an item without a known digest there is no content proof that could
catch a mixture either. A record written without the pinned flag is unpinned
and therefore not inheritable: the safe direction, and no released version ever
wrote these records. A pinned, matching record keeps its full resume: only the
missing bytes are re-fetched.

An unreadable or unproven CURRENT-format record is not discarded
automatically: it is refused with `ErrStagingOwnershipUnproven` and its record
and payload are preserved for the operator. Already-published blobs, completed
files and the snapshot/friendly entries are untouched.

The two staging refusals are told apart because they need different operator
actions. A PROVEN live owner keeps the transient live-owner message ("blob
<sha> is being downloaded by another process; try again later"); the owner may
publish the file at any moment, so the operator simply retries. An UNPROVEN
record reports that its staging state "cannot be proven to belong to a live or
dead owner (legacy or unreadable record)", that the partial is preserved, and
that removing the staging pair is the recovery; only this case carries
`ErrStagingOwnershipUnproven`, so a caller can tell the two apart with
`errors.Is`. OPERATOR PROCEDURE: remove the staging pair (`<staging>` and
`<staging>.meta`, for example `blobs/<sha>.incomplete` and
`blobs/<sha>.incomplete.meta`) and retry the download; the item then starts from
zero. Never remove the destination's persistent domain lock
(`blobs/tmp-<sha>.lock` for a cache destination, `<destination>.incomplete.lock`
for a flat one), and never remove a staging pair whose owner is a PROVEN live
owner — that owner is still writing to it.

Staging identity includes `plan.Commit`. If fetching repository info fails or
returns an empty SHA, the planner uses the requested revision string as the
fallback commit identity; that value is not necessarily an immutable commit.
THE TRANSFER IS PINNED TO THE RESOLVED COMMIT. When the planner resolves the
requested revision to a commit, the WHOLE transfer is built from that commit:
the tree walk that discovers the files AND every per-file URL (`lfsURL`,
`rawURL`). Both halves must name the same identity — walking the requested
revision while fetching the commit (or the other way round) would let a branch
that moves between the info call and the transfer change the file set or the
bytes behind the name the plan recorded, and the identity the staging record
stores would then describe content that was never transferred. When the
endpoint resolves no commit, nothing can be pinned and the requested revision
is used exactly as before; the integrity hole that leaves open is closed by the
content proof described under Publication.

The single-request path has its own resume IDENTITY, because its staging name
is not one. For an item without a known digest the staging name is derived from
the item's path (`tmp-path-<sanitized path>`), so two different objects — or two
revisions of one object — share one staging name, and the destination's
ownership domain excludes concurrent writers without establishing WHICH object
the staged bytes belong to. A size match is not an identity.

The sidecar `<dst>.part.meta` records that identity next to the partial, in the
same vocabulary the ranged record uses: repository, resolved commit, item
relative path and expected size, plus whether the identity is PINNED — that is,
whether the commit was actually resolved rather than the requested revision
standing in for it. The sidecar is version 1. Bytes already on disk are
inherited ONLY when the record exists, is readable and well-formed, matches the
current item exactly (repo, commit, path, size) and is PINNED on both sides.
Otherwise the inherited bytes are discarded (the partial is truncated) and the
transfer starts from zero: unproven bytes are never published. An unpinned
identity pins nothing — a moved branch may change the content under that name —
so nothing is inherited from it.

Object identity is not clean response framing. A process can exit after the
last `WriteAt` but before `io.Copy` observes EOF, so an exactly-sized partial can
still hold a torn final byte. The sidecar separately records whether the run
that produced the partial saw the body end cleanly at the exact expected byte
count (`framing_complete`). An exactly-sized partial is published without a
request only when the identity matches AND that proof is present. Otherwise the
final byte is reserved and re-requested as the non-empty range `[size-1, size)`;
only a cleanly-framed response commits it, and a body error during that
revalidation leaves the final byte uncommitted for a later retry/restart. A
fresh transfer records the flag false before any byte is trusted and sets it
only after `io.Copy` returns nil and every exact body-length check passes. When
the expected size is unknown the exact-size shortcut does not apply.

The record is written before any inherited byte is trusted, and after the
unproven bytes are gone, so it always describes the partial that is on disk. It
is removed together with the partial it describes (on publication, in the
cancel-cleanup path and after a failed content proof), so no identity record
outlives its partial, and it survives an interrupted run: that surviving record
is exactly what makes the next run's decision possible. The cancel-cleanup path
closes the single-path partial's writable handle before it removes anything: the
content proof could previously reach that cleanup while the handle was still
open, and Windows cannot unlink an open file, so an explicit cancel during the
proof does not retain the whole `.part` and its sidecar. Pause/internal aborts
still preserve both. A `.part` without a matching identity record is never
inherited. The known-digest content proof is unchanged and still applies to the
inherited bytes.

A PERMANENT error does not destroy that record. The single-request path's
fail-fast branch (401/403/404) removes the zero-byte partial the attempt
created, and the identity sidecar goes with it — but only with it. A non-empty
partial holds resumable bytes, and the sidecar is the only proof that those
bytes belong to this object; removing the record while the bytes stay would
make the next run discard them, so a permanent (but eventually fixable) error
would silently lose the progress. The sidecar therefore survives with a
non-empty partial, and the next run with the same identity resumes from the
stored offset. The same rule applies to every other branch that removes the
sidecar: it may only go when the partial it describes is actually gone, and only
a not-exist `stat` result proves that. Any other `stat` failure (permission
denied, a symlink loop, an I/O error) leaves the partial's existence unknown, so
the sidecar survives and the bytes keep their resume identity.

The two routes consult ONE shared identity rule: each persisted record exposes
its object identity as the same in-memory `resumeIdentity` value, and both
compare it with the single `matches` rule, so the routes cannot drift apart
while the version-1 `<dst>.part.meta` sidecar and the version-4 ranged record
keep their own formats, versions and lifecycle fields. Do not merge the two
records to share the rule.

The multipart route never imports a single-route `<dst>.part`: a sidecar-bearing
`.part` is resumed only by the single-request route under its own identity rule,
a sidecarless `.part` is discarded by that route and never inherited, and the
multipart route never removes the single-route `.part`/sidecar. An oversized
single `.part` is invalid: discard it, never clamp it.

Existing multipart resume state survives a threshold change. Before routing an
item to the single-request path, the downloader detects a valid current-format
(version 4) `<staging>.incomplete`/`.meta` pair, including the blob-keyed
staging pair for LFS cache items. A ranged-capable item keeps the multipart
route regardless of `MultipartThreshold`, so the state is resumed. A fresh item
with no state keeps the configured routing. A pre-#140 `.part-NN`/`.parts.json`
family is NOT resume state and does not force the multipart route; it is
discarded when the multipart route owns the staging (or left for the existing
success/cleanup path), and the file is re-downloaded from zero. If the server or
plan does not support ranges, the downloader emits an explicit warning,
downloads the full file through the single path, and removes the obsolete
versioned pair only after the final file is durable (claim-checked); the
pre-#140 family is removed by the single-path success cleanup. If an LFS item
remains unknown-size after HEAD, Download uses the single-path temp and the
normal cache-store path, so the blob and its snapshot/friendly entries are
created from the completed temp file.

A ROUTE REMOVES ONLY THE ARTIFACTS IT CREATES (route ownership, B1). The
multipart HEAD fail-fast creates no `.part`: a `<dst>.part`/`.part.meta` next to
the destination belongs to the single-request route, including a partial left
by an earlier run. The multipart route does not remove that pair even while it
holds the destination domain, because the domain excludes simultaneous writers
but does not make the pair the multipart route's artifact. Only the
single-request fail-fast removes its own zero-byte partial (with its sidecar),
because that route holds the destination domain and knows the partial is the
one its attempt created.

# Publication

Skip decisions are filesystem-based: LFS files are compared by SHA-256 hash
(when available in metadata), non-LFS files by size. That pre-download decision
is authoritative for an arbitrary destination. A staging claim treats an
existing publish target as completion proof only when the target is
content-addressed (`blobs/<known-sha>`); a legacy/output file or a `tmp-<sha>`
staging name that the skip check declined is downloaded again and atomically
renamed over the old bytes. A content-addressed target is not overwritten by a
writer of the same blob key: the publication places the payload with a
no-replace primitive, so a blob another writer completed while the transfer was
in flight stays untouched, and the loser discards its own staging pair and
reports the item as complete. The strength of that guarantee depends on the
filesystem; the two tiers are described below.

The single-request path follows the same validation model. A resumed `.part` is
a sequential prefix identified by the `<dst>.part.meta` sidecar described under
Resume identity, not by a per-range layout, so it is never assumed complete:
every ranged (206) response must match the
exact requested `Content-Range`, expected total and body length, writes are
bounded to the requested range, and the file must have exactly the expected
size before the rename. An ignored-Range `200 OK` response restarts the partial
from zero. A reader or copy error after the final requested byte was written is
not successful framing: publication is refused, and the last byte is left
uncommitted for a nonempty retry/restart range. When the expected size is
unknown, the partial is discarded rather than guessed at. Known-digest
verification happens before publication on this path too: in `sha256` and
`etag` modes a fresh or resumed transfer is proven with one full-file SHA-256
pass before the rename, and in `size` mode inherited `.part` bytes are proven
once (the inherited-bytes exception). A mismatch leaves the destination
untouched and removes the partial. Verify `none` inherits that mode's no-hash
semantics.

The ranged publication is gated on a COMPLETE range layout, and the gate is
checked rather than implied. Before the rename, the live layout of the
download and the persisted claim record must both cover the whole expected
payload: every range `Done` (its bytes arrived under a response whose framing
ended cleanly) and the committed total equal to the expected size. A layout
that fails the check is refused with `errStagingLayoutIncomplete` and nothing
is renamed, so the staged bytes stay on disk for a resume. The control flow
that reaches the publication already implies completeness — every range
worker's nil return is caught by the cancellation guard or by the error drain —
but the invariant is asserted at the publication so that a later refactor
cannot break it silently.

The PAYLOAD is gated separately. The layout is the completion contract for the
bytes; the payload's own length is the completion contract for the file. After
the payload handle is closed — so every write is flushed — the publication
requires the staging file to hold exactly the expected number of bytes. A
record that describes a complete layout over a physically short file is
refused with `errStagingPayloadSize`, nothing is renamed, and the staged bytes
stay on disk for a resume.

Do not assemble or publish multipart downloads after context cancellation or a
known part error. The transfer stages directly into the payload file; there is
no separate assembly step, and a cancelled or failed transfer must never reach
the rename.

The publication primitive has two tiers, and they guarantee different things.
The tier is chosen by the FILESYSTEM, not by the operating system.

The atomic no-replace tier is the normal one: the payload is placed with a hard
link, which creates the final name and fails when the name already exists. The
existence decision and the placement are then ONE filesystem operation, so a
concurrent publisher cannot be overtaken between them, and an existing
`blobs/<sha>` is never replaced — not by another writer of the same blob key,
and not by a writer outside the blob key's domain either. The normal path stays
rename-only in cost: a hard link is a directory operation, not a payload copy.

The domain-exclusive tier is the fallback for a filesystem that cannot create
hard links. The Go standard library exposes no atomic no-replace rename on any
supported platform — `os.Rename` replaces an existing target on Unix and on
Windows, and the `syscall` package exports neither
`renameat2(RENAME_NOREPLACE)` nor `renamex_np(RENAME_EXCL)` — so there is no
stronger primitive to fall back to. The placement is then a check-then-rename
performed while the blob key's ownership domain is held exclusively, so no
other participant of that domain can create the target between the check and
the rename. The property that holds there is "an existing `blobs/<sha>` is
never replaced by a domain participant", not "never replaced" unconditionally:
the mirror writer and the selected-GGUF delete path are outside the domain, so
they are not fenced by it. The placement refuses to run this tier without the
ownership domain of the blob key it publishes, so the weaker guarantee is never
reached by accident, and hard-link support is not a prerequisite for
downloading.

If the placement itself fails (a cross-device move, for example), the cache
store falls back to copying the payload to a staging file in the destination
directory and renaming the completed stage into place. The copy runs inside the
same ownership domain, so no domain participant can create the target in that
window, but that final rename is not a no-replace primitive, so a writer outside
the domain is not fenced by it. The fallback prevents a partial copy from
appearing at the final blob path, but it does not provide crash durability or
rollback guarantees.

AN UNPINNED IDENTITY IS CONTENT-PROVED BEFORE PUBLICATION. An identity that
names an unresolved revision is not bound to an immutable object: the bytes
behind the name may differ from the digest the plan recorded. When the identity
is unpinned, the item carries a known digest and `Verify` is not `"none"`,
exactly ONE full-file content proof runs before the publication, on both routes,
and a mismatch publishes nothing. The normal size-mode "no reread" rule is
unchanged for a PINNED identity: there the transfer is bound to an immutable
commit, so the size contract is enough.

# Verification

The Settings.Verify field controls post-download verification, for LFS files
included:

  - "none": No post-download content hash (fastest); HTTP and ranged-response
    validation still apply during the transfer. A ProofPending record is
    ignored and publication proceeds (explicit no-hash opt-out).
  - "size": Verify expected size and complete validated transfer coverage on
    both transfer paths (default); no full-file SHA-256 reread on the normal
    transfer path. The pre-download skip check over an existing file, the
    inherited single-path `.part` proof, an unpinned-identity proof, and a
    single-request known digest with no known expected size may still hash. A
    resumed ProofPending record forces the known-digest proof even in size mode
    after a crash, a failed mismatch cleanup, or a mode change.
  - "etag": Historical compatibility mode. A known SHA-256 digest is proven
    before publication with at most one pass (the same pre-publish engine proof
    used by "sha256"); non-LFS ETag-only files keep their historical
    no-post-check behavior. The HEAD-ETag fallback exists only in "sha256" mode.
  - "sha256": Full SHA-256 hash verification (most secure, slower); at most one
    full-file read per file, always before publication when the item carries a
    known digest. Only an eligible fresh sequential LFS ranged transfer with
    one task covering the whole file hashes while writing; every other transfer
    (including the single-request path, fresh or resumed) performs one
    sequential pass before the destination/blob is renamed into place, so a
    mismatch never publishes corrupt bytes. When no known digest exists, the
    HEAD-ETag fallback remains a post-publication check and can report a
    mismatch then.

A known digest is never hashed only after publication. HEAD-ETag-only
verification (no known digest) remains a post-publication check and can report
a mismatch then. Size checks are only a fallback for files without a trusted
hash.

Durable proof gate: a multipart staging record persists a `ProofPending` flag.
It is set and flushed before any known-digest proof starts (fresh sha256/etag,
an unpinned identity) and cleared and flushed only after the proof succeeds,
immediately before publication. On resume the flag forces the proof in every
mode except "none" — including "size" — whenever the item carries a known
digest, so a crash mid-proof, a mismatch whose cleanup fails, or a later
Verify-mode change cannot publish unproven all-Done ranges. On a mismatch the
flag stays set, poison (a zero-length payload) is attempted only while the claim
is still owned under the metadata gate and lifetime OS lock, and the owned
discard is attempted; if ownership cannot be verified the poison is skipped and
the durable flag remains the guard, so the next run re-proves or discards.
Publication itself is refused while a known digest's proof is still pending.
Verify "none" is the explicit no-hash opt-out. The single-request `.part` has no
separate proof-pending flag (its `framing_complete` field records response
framing, not a content proof): when a proof is required (sha256/etag, inherited
bytes, or a known digest with unknown size) it is renamed only after that proof,
so every restart re-proves it; a fresh size transfer with a known size follows
the size contract and may rename without a hash. An oversized ranged body is a
protocol violation: it never completes a range, invalidates the sequential
hash-on-write state so the retry falls back to exactly one final full-file pass,
and does not double-count progress.

# Cache artifacts, links and cleanup

A cache's `blobs/` directory holds published content and the downloader's own
staging artifacts. The two are distinguished by one exhaustive, name-based
classification, and every consumer of a cache uses it: the mirror scan, copy
and verification, the cache statistics, the cache listing, the cache selection
and the cache deletion.

The staging shapes are derived from the code that creates them:

  - `tmp-<name>` - the single-request staging destination (a content digest, or
    the structurally non-canonical `tmp-path-<name>` form for an item without a
    known digest) and everything derived from it: `tmp-<name>.part` (the
    single-request partial), `tmp-<name>.part.meta` (its identity sidecar) and
    `tmp-<name>.lock` (the destination domain's persistent lock)
  - `<name>.incomplete` - the ranged staging payload
  - `<name>.incomplete.meta` - its resume record
  - `<name>.incomplete.meta.tmp` / `<name>.meta.tmp` - the atomic record temp
  - `<name>.lock` - the persistent ownership lock beside a staging payload
  - `<name>.part` - the single-request partial (the flat-mode destination's
    partial)
  - `<name>.part.meta` - the single-request identity sidecar
  - `<name>.part-NN` - the obsolete pre-#140 multipart family
  - `<name>.parts.json` - the obsolete pre-#140 layout metadata
  - `<name>.tmp-<random>` - the cache store's atomic copy temp

A published blob is named by its content: a canonical 64-hex SHA-256 digest. A
file in `blobs/` that is neither a staging artifact nor a canonical digest is
an UNUSUAL published name — the public storage API accepts such names, so it is
content: the cache statistics count it, the listing shows it, the selection
sees it, the mirror copies and verifies it, and the deletion path may delete it.

The classification is by NAME SHAPE, so a `blobs/` name that CARRIES a staging
shape — `tmp-<name>`, `<name>.part`, `<name>.part.meta`, `<name>.lock`,
`<name>.incomplete`, `<name>.incomplete.meta`, `<name>.meta.tmp`,
`<name>.part-NN`, `<name>.parts.json` or `<name>.tmp-<random>` — is treated as a
staging artifact by EVERY surface, including the deletion path (the conservative
choice: a staging artifact is never deleted), even when such a name was
published content. A canonical 64-hex blob is unaffected: it is content, and the
deletion path may remove it.

The staging exclusion applies ONLY inside `blobs/`, the namespace the downloader
writes these artifacts into. A legitimate `*.meta` (or `*.part`, `*.lock`) file
outside that namespace — a snapshot entry, for example — is content, and the
mirror copies AND verifies it, so copy and verify always agree on the same set.

Because the classification is by NAME SHAPE, a blob published under a
staging-shaped name would be invisible: the statistics, the listing, the
selection, the mirror and the deletion path would all exclude it. The boundary
that accepts a caller-provided blob key therefore refuses such a name, before
anything is placed: `StoreDownloadedFile` (and its context-bounded
implementation) and `FinalizeBlob` reject it with an actionable error that names
the staging namespace. The refusal is narrow — only the NAME SHAPE is refused —
so a non-canonical but non-reserved name (an unusual published name) is still
accepted, stored, visible and mirrored.

A cached file is reachable through its snapshot entry and its friendly entry;
the payload in `blobs/` alone is not the requested path. The transfer path and
both skip paths — the pre-download "blob exists" check and the post-domain
"blob exists" skip — treat a failed link creation as fatal for the item: a
link-creation failure fails the item with an actionable error ("the file is in
the cache but its cache path is not reachable") and the item emits no
`file_done` success. Every path that creates the cache entries returns the
exported `ErrCacheLinkUnreachable` sentinel, so the failure is uniform and
detectable with `errors.Is`. The end-of-job reconciliation that re-creates the
links of skipped items surfaces its link failures as a JOB error instead of only
a warning, so a job cannot finish "successful" with a missing path; its other
duties are unchanged. The friendly entry is deliberately ABSENT when
`Settings.NoFriendlyView` is set, so that is NOT an error.

The job contract follows: a job that reports success means the requested files
are present AND reachable at their snapshot/friendly paths. A file owned by
another live writer is not present, so it is never a successful skip. When the
destination's ownership domain is held by a proven live owner — another
process, or another writer of this process — the item WAITS for the domain
(bounded and context-aware, never a takeover) and, once it holds the domain,
RE-CHECKS the published content-addressed target. If the owner published the
blob while the item waited, the item finishes as an ordinary "blob exists" skip
with its own snapshot and friendly entries created and without another transfer;
if the owner released the domain without publishing, this writer downloads the
file itself; and if the owner still holds the domain when the wait budget
expires, the item fails with the actionable live-owner error and the job does
not report success while that file is missing. Skipped same-SHA/different-path
items are reconciled after all files finish and receive their own
snapshot/friendly entries. A path-derived destination has
no published content-addressed target to re-check, so the item that waited for
its domain obtains the file itself: it re-downloads these small non-LFS files,
and the store then reuses the already published blob. A successful skip that
leaves the file missing is forbidden in every case. The re-check trusts the
EXISTENCE of the published content-addressed blob as completion proof, exactly
like the pre-existing "blob exists" skip does, and does not re-verify its
content: in `size` mode a torn blob written by a foreign writer is therefore
accepted as complete — the same content-addressed assumption the pre-existing
skip already makes. Do not claim content verification for the wait/re-check
path.

Cleanup participates in the same ownership protocol. The artifacts of one cache
destination — the `blobs/tmp-<name>` staging name, its single-request `.part`,
the obsolete pre-#140 `.part-NN` parts and the blob-keyed staging pair — are
removed only while that destination's ownership domain is held, so a cancelled
job never deletes another live writer's partial: that writer would keep writing
to an unlinked inode and its later publication would fail with ENOENT. The
cleanup uses the SAME name-based identity the writer holds: destination
`blobs/tmp-<name>` means key `blobs/<name>` and lock `blobs/tmp-<name>.lock`. For
a content-addressed `tmp-<sha>` that is the blob key's domain; for a path-derived
`tmp-path-<sanitized path>` it is `blobs/path-<sanitized path>` with lock
`blobs/tmp-path-<sanitized path>.lock`, which no blob key or lock can reach. The
staging name is unambiguous, so there is ONE name-based identity per
destination. A live owner of the destination leaves those artifacts alone, and
the contention is reported. A job can still clean up its OWN artifacts: its
writer releases the domain when it returns, and the downloader's own cancel
cleanup removes its staging while it still holds the domain. The residual
limitation is that the cleanup cannot tell "my own destination" from "another
live owner of the same destination" beyond the domain itself, so a destination
another writer owns is left alone even when the cancelling job also had bytes
for it.

A legacy/output-mode destination has no `blobs/`-style internal identity, so its
cleanup derives the SAME domain identity the writer holds: the destination's
domain lock is the persistent sibling `<destination>.incomplete.lock`. Its
artifacts are removed only while that domain is held, so a cancelled job cannot
delete another live job's `.part`, `.incomplete` or `.meta` for the same path.
The cleanup entry points are collapsed to match the one fence: ONE owner-checked
removal runs while the caller holds the domain and the metadata gate
(`removeStagingArtifactsLocked`, used by the claim transitions, the publication
and the cancel cleanup), and ONE record-checked removal takes the metadata gate
and deletes only a pair that no live owner can be using
(`removeStagingArtifactsIfUnowned`, used by the external cleanup paths). The
earlier `removeStagingArtifacts`/`...IfUnclaimed`/`...IfUnclaimedLocked`
variants and the in-process claim registry existed only to work around the
claim's second lock and its self-conflict, and are gone. A cache destination
with no derivable domain is skipped with a warning instead of removing unowned
artifacts. The versioned staging pair (`<destination>.incomplete` and its
`.meta`) is still record-checked, and the single `.part`, the obsolete pre-#140
`.part-NN` parts and `.parts.json` are removed as before. The persistent
`<destination>.incomplete.lock` is never unlinked.

Known limitation (not a downloader invariant): the mirror writer copies
destination blobs with a non-atomic truncate-and-write and a size-only check, so
it can tear or overwrite a blob a downloader is publishing. The selected-GGUF
delete path removes blob payloads of the selected entries, and it does that
outside the blob key's ownership domain as well. Neither path takes the domain
lock, so neither is fenced by the publication domain: they are the documented
exception to "an existing `blobs/<sha>` is never replaced" (and, on a filesystem
without hard links, to the domain-exclusive tier as well). Both are known
limitations of those paths, not of the downloader: the downloader's publication
invariant does not depend on their behavior.

# Retry, cancellation and lifecycle

Keep retry resume offsets based on bytes actually written to disk. Do not delete
partial data on a transient network failure unless it is known to be stale or
incompatible. Use context-aware sleeps and requests so pause, cancel, shutdown
and requeue stop promptly. Cancellation is checked after `file_finalizing` and
again immediately before the single-path rename; an explicit cancel applies the
claim-checked cleanup, while a pause or internal abort preserves resumable
partials. A cancel inside the ranged pre-placement window is the exception:
it leaves the already-proven, complete staging pair on disk for a resume,
because the explicit-cancel cleanup is not run there by design. Keep progress
unique across the final-byte revalidation so a re-requested byte is not
counted twice.

`Settings.Retries` bounds non-progress retries per request; a non-truncating
attempt that advances the on-disk resume offset strictly beyond the highest
offset reached refunds the budget and resets the backoff. An ignored-Range
`200` restart does not refund the budget, even when the attempt writes beyond
the previous high-water mark; the new high-water mark is still recorded.
Progress refunds can increase the retry count, but a separate lifetime
retry-count ceiling limits retries after individual attempts return, and that
ceiling does not bound the duration or bytes of a single request that keeps
delivering data. `BackoffInitial` and `BackoffMax` default to 400ms and 10s. A
`Retry-After` header or the Hub `RateLimit` header raises the wait to the larger
server-requested time, capped at 5 minutes. `Settings.StallTimeout` aborts a
body read that delivers no bytes for the configured duration and retries from
the current on-disk offset; `"0"` disables it. `429` and all `5xx` responses are
retryable; other `4xx` responses are terminal for that file, while `401`, `403`,
and `404` fail the whole job. Optional verification `HEAD` metadata failures are
ignored; the actual file request remains authoritative.

# Concurrency

Two levels of concurrency are configurable:

  - Concurrency: Number of parallel HTTP connections per file (for multipart downloads)
  - MaxActiveDownloads: Maximum number of files TRANSFERRING simultaneously
    (the limit bounds concurrent transfers, not waiters: a file waiting for
    another writer's destination ownership domain does not consume a slot. The
    domain acquisition returns the slot for a wait and takes it back before
    returning a domain the caller will transfer under; a failure keeps the slot
    returned)

# Error Handling

Download returns the first error encountered. The context can be used for
cancellation, and canceled downloads can be resumed on the next run.

A cached file whose snapshot or friendly entry could not be created fails the
affected item with `ErrCacheLinkUnreachable`: the payload may be in `blobs/`,
but the requested path is not reachable through it, so the job does not report
success. The friendly entry is deliberately absent when
`Settings.NoFriendlyView` is set, so that is not an error.

An item whose destination is owned by a proven live writer fails with the
actionable live-owner error when the bounded wait expires. An unproven staging
record is a different condition and carries `ErrStagingOwnershipUnproven`; its
partial is preserved and removing the staging pair is the manual recovery. An
unknown ownership state carries `ErrBlobOwnershipUnknown`; the payload is
preserved and the item must not be skipped.

# Authentication

For private or gated repositories, set the Token field in Settings:

	cfg := hfdownloader.Settings{
		Token: "hf_xxxxxxxxxxxxx", // Your HuggingFace access token
	}

Tokens can be generated at: https://huggingface.co/settings/tokens
*/
package hfdownloader
