// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

/*
Package hfdownloader provides a Go library for downloading models and datasets
from the HuggingFace Hub with resume support, multipart downloads, and verification.

This is the canonical downloader/cache correctness contract. AGENTS.md summarizes
it; docs/API.md covers the public API and CHANGELOG.md records release changes.
Update this contract when rules change, then update the relevant summaries.

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

Exactly one participating writer may own an item at a time. Each operation
combines an in-process keyed waiter with the applicable persistent OS lock set.
In cache mode, Hub data uses the cache-owned control root
`H/.hfdesk-control/v1`, derived only from the frozen Hub root H; friendly-view
data uses the independent root `R/.hfdesk-control/v1`, derived from the frozen
cache root R. Cache clients sharing H therefore find the same Hub anchor without
per-user configuration or an R-derived path. The item acquires its scope after
the authoritative skip decision and before resume inspection, HEAD or claim,
and holds it through transfer, proof, publication/store and cancel cleanup.
Cache-link creation scopes the actual snapshot path under H and the enabled
friendly path under R while the blob item domain remains held.

`Settings.OwnershipDir` selects the external realm only for flat `OutputDir`
mode; empty uses the per-user default `<UserConfigDir>/HFDesk/ownership` (or the
documented home fallback). A non-empty value in cache mode is rejected rather
than selecting a second cache namespace; the legacy `NewHFCache*WithOwnershipDir`
constructors report that configuration error when an ownership operation is
attempted. Hub and friendly control roots are independent when H and R are
independent. Cache control nodes persist opaque anchor/node IDs; each node stores
leases and profiles in a `metadata/` branch separate from user components in
`children/`. Profile changes and malformed IDs fail closed. Cache lock files are
persistent and never unlinked; flat-mode hashed lock files remain under
`<OwnershipDir>/v1/`. No current writer creates payload-side domain-lock files.
Existing payload-side `.lock` files from older versions are not current ownership
proof and are not automatically migrated or removed. Server deletion, mirror
destructive steps and the HF branch of selected-GGUF deletion participate in
these cache-root scopes through the cache-owned admission API
(`OpenCacheAdmission` with `AdmitTreeRemoval`/`AdmitFileWrite`); selected
deletion's local-root branch participates through the same admission API
against the flat `Settings.OwnershipDir` realm (`OpenFlatAdmission` with
`AdmitFileWrites`). None of them participates through this library's transfer
internals.

The logical domain identity remains name-based and differs by destination
kind; external locks themselves are hashes of realm-scoped physical item keys:

  - A content-addressed cache item uses the canonical `blobs/<sha>` key across
    the single and ranged routes. Its scope covers the blob publication target
    and route-owned staging artifacts; actual link paths get their own scoped
    lease while the blob key remains held.
  - An item without a known digest (the planner permits an empty SHA-256) stages
    at the STRUCTURALLY NON-CANONICAL `blobs/tmp-path-<sanitized relative path>`.
    Its logical domain key is `blobs/path-<sanitized relative path>` and remains
    distinct from canonical blob keys. After hashing, store acquires the
    computed blob key while retaining ownership of its temporary file.
  - A flat destination is keyed by its actual path. Its scope covers the
    destination and route-owned partial/staging paths. No
    `<destination>.incomplete.lock` payload sibling is created, so a requested
    file such as `foo.incomplete.lock` is an independent ordinary item.

Scopes lock shared ancestor-directory identities and exclusive item entries.
Missing parent components use short exclusive preparation gates: create parents
while held, promote to the final physical keys, then release preparation before
payload I/O. Supported path identity details depend on the platform/filesystem;
do not claim universal casefold, bind-view or cross-client alias detection.
Tree mutations must use exclusive tree admission. Server HTTP DELETE, mirror
`deleteExtra` removal and the HF branch of selected-GGUF deletion take it
through `AdmitTreeRemoval` before removing anything; the local-root branch of
selected-GGUF deletion takes `AdmitFileWrites` entry admission over exactly the
member paths it unlinks, against the flat `Settings.OwnershipDir` realm that
flat writers resolve the same way (`DefaultOwnershipDir()` when unset). Both
selected-delete branches therefore exclude writers in other processes; local
exclusion is exactly as strong as the flat realm's existing writer
coordination — two participants exclude each other only when both resolve the
same `OwnershipDir`. A tree-removal admission whose target is missing must
re-verify the vanish while holding the missing target's creator-side keys — a
bare or wrapped ENOENT from resolution is not proof that the target is gone —
and an admission that can establish neither a live-writer conflict nor a
re-verified vanish fails closed instead of admitting.

The `path-` prefix separates digest-less destination domains from content-addressed
blob domains and their staging names. Without it, digest-named files whose contents
hash to each other's names can each hold the domain the other's store needs.
Transfer-slot release cannot break that ownership cycle. Writers and cleanup must
use the same name-based identity; final and snapshot paths are unchanged.

Every writer of `blobs/<sha>`, including exported `FinalizeBlob`, must participate
in that key's domain and publish through the shared no-replace primitive. The
single and ranged routes must share the domain despite different staging paths,
so neither can adopt/delete the other's live partial or overtake its publication.

A busy destination domain is not completion proof: wait without takeover, then
re-check the content-addressed target and download if it was not published. Flat
and path-derived destinations have no such target and download after waiting.
The OS releases control-file lock handles on process exit; a suspended owner
retains them. Unix uses shared/exclusive `flock`; Windows uses `LockFileEx` over
cache-node metadata locks or flat-mode external lock files.

Ownership waits use an internal five-minute budget in the item's
goroutine. In-process waits are bounded by the item context. Both release the
transfer slot while waiting and reacquire it before transfer. An exclusive
scope holder is waited for; unresolved contention fails with a retry instruction,
never successful completion.

If a cache-owned or flat-mode lock cannot be acquired (unsupported, denied, or
filesystem error), the operation fails before mutating claims or payloads and
preserves existing partial data. This is an operation-time safety error, not a
permanent platform-support policy. Control lock files are persistent: never
unlink them (two processes could otherwise lock different inodes). Cache-mode
locks live only under the hidden H/R control roots, not beside model payloads;
flat-mode locks live in the configured external realm. None are payload items.

The multipart claim reuses the held cache or flat scope and takes no second item lock
or transfer slot. It rejects nil/released/non-covering scopes before mutation and
records both `LockFenced=true` and a versioned `OwnershipFence` proof for the
held realm/item key under the per-path metadata gate. That gate serializes
intra-item transitions, not competing writers. `LockFenced` by itself is legacy
diagnostic data, not proof for the current cache/flat protocol. A held current
scope makes only a record carrying its exact current proof stale. Missing,
unknown or mismatched proofs are preserved/refused unless self-identity or
same-host owner-death evidence independently permits reclaim; the ownership
check runs before the record-version branch. Mixed-version writers remain
unsupported, and age, ambiguous identity or a free cache/flat key are insufficient.

All ownership observations are non-owning shared leases. A refused shared probe
proves an exclusive scope holder; a granted probe cannot distinguish another
reader from no holder. Claimants retry real exclusive acquisition within the
bounded cancellable window, then fail if the scope is not proven free. An observer
never takes an exclusive lock or impersonates a writer.

`CheckBlob` trusts an existing published blob. With ranged staging it probes the
cache/flat scope for the blob key and staging record. An exclusive holder is
`BlobDownloading`; a free scope permits record checks: an exact current proof or
independent self/death evidence is `BlobStale`, while an unproven live owner
remains `BlobDownloading`. A malformed/unreadable record with a free scope is
reported conservatively as `BlobStale`, not as permission to resume or delete
it: claim/cleanup still preserve and refuse it as unproven. Probe failure or
unsupported locking returns `BlobOwnershipUnknown` with
`ErrBlobOwnershipUnknown`, never a successful skip. Without ranged staging it
returns `BlobMissing`, even if a single-request writer holds an item scope;
callers still acquire the scope.

The claim record is diagnostics/resume state, not a freshness-based ownership
lease. Ownership is the held destination scope; a matching `LockFenced` plus
`OwnershipFence` record proves only that the predecessor used the same
realm/key. The record is not rewritten on a timer and has no heartbeat; record
age is never an ownership signal. The former age-based configuration (`Settings.StaleTimeout`,
`HFCache.StaleTimeout` and `DefaultStaleTimeout`) has been removed; do not
reintroduce an age-based timeout, lease or takeover input. Every mutation
re-reads claim identity, so a checkpoint cannot resurrect a replaced claim. The
claim carries a per-process instance token plus PID/host diagnostics, so PID
reuse and containers sharing PID 1 are not treated as the same writer. Publish
checks the record and the still-held cache/flat scope before rename, and checks
cancellation immediately before the rename; a missing or foreign record is
refused with `errStagingClaimLost` and is never renamed. Record transitions are
serialized by the per-staging metadata mutex;
the cache/flat scope (held by the item) excludes foreign current-version
mutators and makes that gate intra-item state.

Fresh claims install a complete staged document with atomic hard-link
no-replace, or, when hard links are unsupported, rename under the ownership
gates only if no record exists. Takeover atomically replaces the validated
predecessor with the complete successor. Staging always uses a fresh inode:
leftover temp aliases must never be truncated through the authoritative record.
Hard-link support is not a download prerequisite.

Ownership-scope acquisition returns the item's transfer slot during both
in-process and OS-lock waits, then reacquires it before returning the held scope
for transfer; acquisition failure leaves the slot returned. The multipart
claim neither waits for ownership nor moves the lease. In-process waits end with
the item context; waits for a proven foreign owner use the ownership budget,
while the metadata gate uses `stagingLockWait`. Waiting items emit no transfer
progress, and an unresolved scope wait fails the job. Returning the slot prevents
a slot/domain wait cycle.

# Resume identity

Resume is always enabled. Multipart downloads write directly into one staging
file (`blobs/<sha>.incomplete` in HF cache mode, `<destination>.incomplete`
otherwise) and persist their committed byte ranges in versioned metadata next
to it, so an interrupted transfer resumes from its durable high-water marks
without re-downloading valid bytes — even when the connection count changes
between runs. Range-record version 4 marks ranges Done only after the response
body reaches clean EOF. It is the ONLY resumable version. After ownership is
established by an exact current scope/key proof or independent self/death
evidence, a record of any other version — including a branch-only version-3
record whose `legacy_migrated` flag described imported bytes — is discarded
under the claim's ownership and local metadata serialization, and the file is
re-downloaded from zero. A legacy `LockFenced` bit without current proof does
not establish ownership: if its owner cannot independently be proven dead or
self, the record is refused as unproven before the version-discard branch and
preserved. Partial downloads from old formats are never migrated; a warning is
logged when proven-reclaimable old artifacts are discarded.

A version-4 record resumes only when repo, resolved commit, path and expected
size match, both identities are pinned, and SHA values are either both absent
or both present and equal. Its normalized ranges must tile the file, with Done
requiring clean framing, and the payload must have the expected logical size
before create/truncate. Otherwise discard the proven-reclaimable pair and
restart. Unpinned or missing-pinned-flag records inherit nothing; matching
pinned state retains committed bytes and fetches only missing bytes.

An unreadable or unproven CURRENT-format record is not discarded
automatically: it is refused with `ErrStagingOwnershipUnproven` and its record
and payload are preserved for the operator. Already-published blobs, completed
files and the snapshot/friendly entries are untouched.

An exclusive scope holder, unresolved shared-reader contention, and an UNPROVEN
record have distinct outcomes. An exclusive holder is waited for, then reported
with an actionable retry error if it remains. A shared reader can prevent an
exclusive acquisition without proving a writer; unresolved contention fails
closed without claiming a live writer. An UNPROVEN record reports that its
staging state "cannot be proven to belong to a live or dead owner (legacy or
unreadable record)", that the partial is preserved, and that removing the
staging pair is the recovery; only this case carries
`ErrStagingOwnershipUnproven`. OPERATOR PROCEDURE: remove that staging pair
(`<staging>` and `<staging>.meta`, for example `blobs/<sha>.incomplete` and
`blobs/<sha>.incomplete.meta`) only after establishing that no client is using
it. Never remove a persistent lock file under either cache-owned
`<H-or-R>/.hfdesk-control/v1/` tree or flat-mode `<OwnershipDir>/v1/` while any
participant may hold it; old payload-side `.lock` artifacts are not current
ownership proof and are not automatically migrated or deleted.

Resolve one commit for the tree walk and every per-file URL (`lfsURL`, `rawURL`);
otherwise a moving branch can make the recorded identity describe different
files or bytes. Hugging Face and commit-capable endpoints stay pinned. Branch
variants are always pinned and fail before destination reservation if the
endpoint cannot resolve their revision.

An ordinary custom endpoint may use the established revision-name fallback
only when commit addressing is confirmed unavailable. A valid revision response
without a SHA or a direct revision-lookup 404, followed by a successful
validated tree for the requested branch, retains the existing unresolved
endpoint mode. When metadata resolves to commit A, a direct commit-addressed
tree/file 404 qualifies only when the matching requested-branch operation
succeeds and a fresh metadata lookup still resolves the branch to A. File
probes are bounded GETs with a one-byte range, use the same path/authentication,
and preserve ordinary CDN redirects. A redirect that explicitly rewrites A to
the mutable branch is not accepted as pinned. Transport, cancellation,
authentication, throttling, server, decode, redirect, or other inconclusive
errors never authorize fallback. Branch variants never fall back.

Mode selection finishes before `OnPlanComplete` or `plan_item` events. On a
qualifying incompatibility the complete candidate is rebuilt from the branch
tree and all final URLs use that same branch mode; transfer errors never switch
the mode mid-job. An unpinned plan records the requested revision as its
effective identity, does not inherit staged bytes, and uses the single-request
route with every retry starting from byte zero. The output is revision-addressed,
not an immutable commit snapshot: no commit is written into the manifest or
`refs/<revision>`. Branch movement can yield files from different moments and
cannot be made immutable by this compatibility exception. A known digest with
`Verify != "none"` still requires the Publication content proof; `Verify=none`
does not gain a hash requirement. Probe failures that do not prove branch-only
support are optional diagnostics and do not gate cached skips or otherwise
working pinned transfers.

The single-request path needs an identity sidecar because its staging name is not
an identity: path-derived items and revisions can share one name, and size alone
does not identify the staged object.

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
Like the ranged record's durable high-water marks, a persisted sidecar never
claims bytes that are not durable: before a persist may set
`framing_complete` the partial's written bytes are synced, and the sidecar
itself is synced before its rename publishes the claim, so both hold under
power loss, not only a process crash.

Write the sidecar after discarding unproven bytes and before trusting inherited
bytes, so it describes the on-disk partial. Remove it only with the partial on
publication, explicit cancel or failed content proof; interrupted runs retain
it for resume. Explicit-cancel cleanup closes the writable handle first so
Windows can remove the pair. Pause/internal abort preserves both. A partial
without matching identity is never inherited; known-digest proof still applies.

A permanent single-request error (401/403/404) removes only the attempt's
zero-byte partial; its sidecar goes only if that partial is actually gone.
Nonempty partials keep their identity so a later matching run can resume.
Every sidecar-removal path requires a not-exist stat result for the partial:
permission, symlink-loop and I/O failures leave existence unknown and preserve
the sidecar.

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

A route removes only artifacts it creates (B1). A multipart HEAD failure creates
no `.part` and must preserve the single-route `.part`/sidecar, including earlier
runs' state, despite holding the destination domain. Single-route fail-fast
may remove only its own zero-byte partial and its sidecar under that domain.

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

Before ranged publication, both live and persisted layouts must cover the
expected payload: every range Done after clean framing, committed total equal
to expected size. Otherwise return `errStagingLayoutIncomplete`, rename nothing
and preserve staged bytes for resume.

After closing the payload handle, require its logical length to equal the
expected size independently of the layout. A physically short or oversized
payload returns `errStagingPayloadSize`, is not renamed and remains for resume.

Cancellation or a known part error must stop publication.

The publication primitive has two tiers, and they guarantee different things.
The tier is chosen by the FILESYSTEM, not by the operating system.

The atomic no-replace tier is the normal one: the payload is placed with a hard
link, which creates the final name and fails when the name already exists. The
existence decision and the placement are then ONE filesystem operation, so a
concurrent publisher cannot be overtaken between them, and an existing
`blobs/<sha>` is never replaced — not by another writer of the same blob key,
and not by a writer outside the blob key's domain either. The normal path stays
rename-only in cost: a hard link is a directory operation, not a payload copy.

Without hard-link support, placement checks and renames under the blob key's
exclusive domain, refusing to run without that matching domain. No domain
participant can create the target between check and rename, so participants
do not replace an existing blob. This is not unconditional no-replace: mirror
and selected-GGUF operations do not publish through this primitive. The mirror
admits each copied destination file and each removed repository tree, and
selected-GGUF deletion admits the repository and its friendly view, so against
ownership participants their exclusion comes from that admission rather than
from placement; the no-replace property itself is not claimed for them. Hard
links are not a prerequisite for downloading.

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

Settings.Verify controls transfer-completion verification, including LFS files:

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
is still owned under the metadata gate and external item scope, and the owned
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

The classifier recognizes current staging shapes and retained legacy residue
shapes:

  - `tmp-<name>` - the single-request staging destination (a content digest, or
    the structurally non-canonical `tmp-path-<name>` form for an item without a
    known digest) and everything derived from it: `tmp-<name>.part` (the
    single-request partial), `tmp-<name>.part.meta` (its identity sidecar) and
    the legacy `tmp-<name>.lock` residue shape (new locks are external)
  - `<name>.incomplete` - the ranged staging payload
  - `<name>.incomplete.meta` - its resume record
  - `<name>.incomplete.meta.tmp` / `<name>.meta.tmp` - the atomic record temp
  - `<name>.lock` - a legacy payload-side lock residue shape; cache-mode locks
    live under node `metadata/` branches and flat-mode locks in the external realm
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

`StoreDownloadedFile` (including its context-bounded implementation) and
`FinalizeBlob` reject staging-shaped keys before placement with an actionable
staging-namespace error, because such content would be hidden by classification.
Non-canonical names without a reserved shape remain accepted and visible.

A blob alone is not a reachable requested path. Transfer and both blob-exists
skip paths must create snapshot and enabled friendly entries; link failure
returns `ErrCacheLinkUnreachable` (detectable with `errors.Is`) and emits no
`file_done` success. Skipped-item reconciliation must surface link failures as
a job error, not merely a warning; it must also re-confirm that every skipped
blob is still complete and reachable before the job may succeed, failing with
`ErrCacheBlobUnavailable` when a blob vanished or is incomplete and with
`ErrCacheBlobCheckFailed` when that re-confirmation itself fails.
`NoFriendlyView` omits only the friendly entry, not the required snapshot.

Job success means every requested file is present and reachable through its
snapshot/friendly paths. A proven live owner is waited for; after acquisition,
the item reuses a newly published content-addressed blob or downloads if none
exists. A path-derived destination has no target to re-check, so it downloads
and the store may reuse the resulting blob. Shared-SHA items are reconciled
after transfers and get their own links. The wait/re-check trusts blob existence
like the pre-download skip; it does not re-verify content, so do not claim a
content proof for this path.

Cleanup removes a cache destination's temp, single partial, obsolete multipart
family and blob-keyed staging only while holding the same external item scope as
the writer. `tmp-<name>` maps to the logical `blobs/<name>` key; the
`tmp-path-` namespace remains disjoint from canonical blob domains. Flat cleanup
uses the destination item path and acquires no sibling `.incomplete.lock` file.
With an exclusive holder or an unproven record, cleanup leaves artifacts
untouched. Own explicit-cancel cleanup runs before scope release. External cleanup
cannot distinguish a job's earlier bytes from another current owner of that
destination, so it leaves the destination alone even if the cancelling job once
wrote there.

Under the item scope, `removeStagingArtifactsLocked` removes while the metadata
gate is held; `removeStagingArtifactsIfUnowned` takes that gate and removes only
a pair whose current scope/key proof or independent self/death evidence proves
reclaimable. Versioned pairs stay record-checked; single `.part`, obsolete
`.part-NN` and `.parts.json` retain their cleanup. A cache destination with no
derivable scope is skipped with a warning, never removed without ownership.

Known limitations outside the downloader domain: a mirror file copy is still a
non-atomic truncate-and-write performed under its entry admission, and it checks
nothing itself beyond `io.Copy`'s error contract — the copied byte count is
discarded, the explicit size comparison happens only in the opt-in verify pass,
and that verification compares size rather than content hashes; and HF
selected-GGUF deletion removes blob payloads under repository and friendly tree
admission rather than each blob's domain. The mirror's symlink-leaf replacement
takes entry admission and its directory creation takes the shared admitted
directory-creation class (a directory that already exists needs no admission
because nothing is mutated); the sync/rebuild path creates, replaces and
removes snapshot/friendly entries, prunes empty directories and writes the
rebuild script through those same shared primitives, so a live holder of an
entry or tree scope excludes those mutations in every participant — after the
per-entry bounded wait for the link and script funnels, immediately on the
non-waiting admission surface the mirror already uses for file writes and tree
removals. These admissions coordinate through the shared control tree's file
locks like the established file and tree admissions: a foreign machine is
excluded only when it shares that control filesystem, while the host-identity
fence model that refuses unproven foreign-host owners governs staging records,
which entry replacement and directory creation neither read nor write — that
inter-machine case is not exercised by a single-host test. None of these
operations performs domain-exclusive placement, so do not extend downloader
publication guarantees to them.

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

`Concurrency` limits parallel HTTP connections per file. `MaxActiveDownloads`
limits active transfers, not items waiting for destination ownership (see the
ownership section above).

# Error Handling

`Download` returns the first error; canceled transfers may resume later.
Required cache-entry failures use `ErrCacheLinkUnreachable`; skipped-blob
reconciliation failures use `ErrCacheBlobUnavailable` (vanished or incomplete)
or `ErrCacheBlobCheckFailed` (the re-confirmation itself failed); `NoFriendlyView`
exempts only the omitted friendly entry, never the snapshot. Live-owner timeout
fails the item. Unproven records use `ErrStagingOwnershipUnproven` and remain
for manual recovery; unknown ownership uses `ErrBlobOwnershipUnknown`, preserves
the payload and never produces a successful skip.

# Authentication

For private or gated repositories, set the Token field in Settings:

	cfg := hfdownloader.Settings{
		Token: "hf_xxxxxxxxxxxxx", // Your HuggingFace access token
	}

Tokens can be generated at: https://huggingface.co/settings/tokens
*/
package hfdownloader
