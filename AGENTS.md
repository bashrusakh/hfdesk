# HFDesk - AI Agent Reference

## Core purpose

HFDesk is a local web dashboard for searching, analyzing, downloading, and
managing Hugging Face models and datasets. The app runs as a small Go HTTP
server with embedded static assets and supports resumable downloads, HF cache
layout, LM Studio-style local folders, job tracking, cache browsing, and mirror
operations.

## Architecture overview

- The server entrypoint is `cmd/hfdesk/main.go`.
- HTTP API, job scheduling, settings, cache browser, search, README rendering,
  and WebSocket updates live in `internal/server/`.
- Static frontend files are embedded from `internal/assets/static/`.
- Download planning, HF Hub API interaction, resumable transfers, verification,
  cache layout, manifests, and mirror/sync logic live in `pkg/hfdownloader/`.
- Model/file analysis and recommended download selection live in `pkg/smartdl/`.
- Server-side state files are stored through helpers in `internal/server/` and
  normal per-user config locations unless overridden.

Keep the boundary clear: downloader/cache correctness belongs in
`pkg/hfdownloader`; job orchestration and API state belong in `internal/server`;
browser-only presentation belongs in `internal/assets/static`.

## Tech stack

- Language: Go 1.25 (`go.mod` is source of truth)
- Server: standard `net/http`
- WebSocket: `github.com/gorilla/websocket`
- Markdown README rendering: `goldmark` + `bluemonday`
- Config formats: JSON and YAML (`gopkg.in/yaml.v3`)
- Frontend: plain embedded HTML/CSS/JavaScript, no package manager build step

## Repository layout

| Path | What |
|---|---|
| `cmd/hfdesk/` | application entrypoint |
| `internal/server/` | HTTP API, jobs, settings, cache browser, websocket state |
| `internal/assets/` | embedded frontend assets |
| `internal/assets/static/` | browser UI files |
| `internal/service/` | service mode: scopes, lifecycle, and the systemd/launchd/SCM managers |
| `internal/runtimepaths/` | runtime path model for the config file, state root and log dir |
| `pkg/hfdownloader/` | Hugging Face scan/download/cache/mirror logic |
| `pkg/smartdl/` | model analysis and selectable download helpers |
| `docs/` | API docs and screenshots |
| `.github/workflows/` | CI, release, Docker workflows |

## Documentation map

Read relevant Markdown before changing behavior:

- General project behavior: `README.md`
- API contracts: `docs/API.md`
- Service mode reference: `docs/SERVICE.md`
- Contribution and commit conventions: `CONTRIBUTING.md`
- Release history and user-visible changes: `CHANGELOG.md`
- Current project notes: `TODO.md`
- PR expectations: `.github/PULL_REQUEST_TEMPLATE.md`

When changing API request/response shape, update `docs/API.md`. When changing
user-visible behavior for a release, update `CHANGELOG.md` if appropriate.

## Build and validation commands

```bash
# run all tests
go test ./...

# race check when touching concurrency, jobs, websockets, or shared state
go test ./... -race

# build binary
go build -o hfdesk ./cmd/hfdesk

# run locally
go run ./cmd/hfdesk
```

Format changed Go files with `gofmt -w <files>` before finalizing. For docs-only
changes, tests are usually not required; state that they were not run because the
change is documentation-only.

## Runtime entry points

- Main process: `cmd/hfdesk/main.go`
- Service command family: `cmd/hfdesk/service.go`
- Server routes: `internal/server/server.go`, `internal/server/api.go`
- Job manager: `internal/server/jobs.go`
- WebSocket hub: `internal/server/websocket.go`
- Config persistence: `internal/server/config.go`
- Service layer: `internal/service/service.go`, `internal/service/manager_linux.go`, `internal/service/manager_darwin.go`
- Runtime paths: `internal/runtimepaths/runtimepaths.go`
- Frontend app: `internal/assets/static/js/app.js`
- Frontend styles: `internal/assets/static/css/style.css`
- Downloader: `pkg/hfdownloader/downloader.go`
- Planning/API scan: `pkg/hfdownloader/plan.go`, `pkg/hfdownloader/api*.go`
- HF cache layout: `pkg/hfdownloader/hfcache.go`
- Smart analysis: `pkg/smartdl/analyzer.go`

## Agent constraints

- Prefer the smallest correct change.
- Do not run git/GitHub commands unless explicitly asked, except read-only
  inspection such as status/diff/log when preparing a requested commit or PR.
- Do not commit generated binaries, model files, cache contents, local config,
  tokens, or secrets.
- Do not add dependencies unless the user asks or there is no reasonable local
  implementation.
- Keep existing public API behavior stable unless the task explicitly changes it.
- Preserve user changes in a dirty working tree; never revert unrelated edits.
- Before PR creation, request/perform project review according to local policy
  (`ocr review`, `@reviewer`) when applicable.

## Code of conduct for agents

- Inspect nearby code before introducing new patterns.
- Keep diffs tight; avoid drive-by refactors.
- Prefer explicit state and validation over heuristics.
- Treat partial failure as normal for downloads, filesystem work, and network
  operations.
- Make cancellation and retry behavior explicit.
- Do not hide data loss, verification failure, or fallback behavior.
- Finish end-to-end: implementation, focused tests, and cleanup.

## Downloader correctness rules

Downloader bugs often produce silent corruption, so safety beats clever resume.

- Never assemble multipart downloads after context cancellation or known part
  errors.
- The publication is gated on a COMPLETE range layout, and the gate is
  CHECKED rather than implied: before the rename, the live and the persisted
  range layout must both cover the whole expected payload (every range `Done`,
  committed total equal to the expected size). An incomplete layout is refused
  with `errStagingLayoutIncomplete` and nothing is renamed, so the staged bytes
  stay on disk for a resume. The control flow that reaches `publish()` already
  implies completeness, but the invariant is asserted there so a later refactor
  cannot break it silently.
- THE PAYLOAD IS GATED SEPARATELY FROM THE LAYOUT. The layout is the
  completion contract for the BYTES; the payload's own length is the completion
  contract for the FILE. After the payload handle is closed - so every write is
  flushed - the publication requires the staging file to hold exactly the
  expected number of bytes. A record that describes a complete layout over a
  physically short file is refused with `errStagingPayloadSize`, nothing is
  renamed, and the staged bytes stay on disk for a resume.
- THE TRANSFER IS PINNED TO THE RESOLVED COMMIT. When the planner resolves the
  requested revision to a commit, the WHOLE transfer is built from that commit:
  the tree walk that discovers the files AND every per-file URL (`lfsURL`,
  `rawURL`). Pinning only one half re-opens the hole the resolved commit
  closes: a branch that moves between the info call and the transfer would
  change the file set or the bytes behind the name the plan recorded, and the
  identity the staging record stores would then describe content that was
  never transferred. When the endpoint resolves NO commit, nothing can be
  pinned and the requested revision is used exactly as before; the integrity
  hole that leaves open is closed by the content proof below.
- AN UNPINNED IDENTITY IS CONTENT-PROVED BEFORE PUBLICATION. An identity that
  names an unresolved revision is not bound to an immutable object, so the
  bytes behind the name may differ from the digest the plan recorded. When the
  identity is UNPINNED, the item carries a known digest and `Verify != "none"`,
  exactly ONE full-file content proof runs before the publication, on both
  routes, and a mismatch publishes nothing with the actionable error of the
  existing pre-publication proof path. The normal size-mode "no reread" rule
  is unchanged for a PINNED identity: there the transfer is bound to an
  immutable commit, so the size contract is enough.
- Only a CURRENT-format (version 4) staging record is resumable. A record of
  any other version, and every pre-#140 `.part-NN`/`.parts.json` family, is an
  old or unsupported format: discard it under lifetime ownership and restart
  the file from zero rather than guessing. For a current `.incomplete.meta`
  claim, discard identity-mismatched data only while holding lifetime
  ownership; preserve unreadable or unproven current claims (refuse them with
  `ErrStagingOwnershipUnproven`) unless same-host owner death is proven.
- `Settings.Verify` governs LFS verification too: `none` performs no
  post-download content hash; `size` (default) checks exact size plus complete
  validated transfer coverage on both the ranged and single-request paths and
  performs no full-file SHA-256 pass on the normal path (the pre-download skip
  check, the inherited single-path `.part` proof, the unpinned-identity content
  proof, and a single-request known digest with no known expected size may
  still hash); `sha256` performs at most one
  full-file pass, always before the rename/publication when a known digest
  exists, and never publishes a mismatch; `etag` proves a known digest with
  that same single pre-publication pass and keeps the historical non-LFS ETag
  behavior. A known digest is never hashed only after publication; HEAD-ETag-only
  verification (no known digest) remains a post-publication check and can
  report a mismatch then.
- The single-request path is not exempt from the trust model: every 206 must
  match the exact requested Content-Range, expected total and body length, its
  writer is bounded to the requested range, and the file must have exactly the
  expected size before the rename. `.part` bytes inherited from an earlier run
  have no validated provenance: content-prove them with one full-file SHA-256
  pass before publication when a known digest exists and `Verify != "none"`,
  and never publish on a mismatch. A fresh transfer in `sha256`/`etag` mode is
  likewise proven before the rename. Without a known expected size, discard the
  partial rather than guessing a resume layout.
- Written byte count alone does not prove a response ended cleanly. If a body
  errors after delivering the requested final byte, never mark the range Done or
  rename the single-path partial; preserve its validated prefix and reserve the
  final byte for a nonempty retry/restart range until a response completes clean
  framing. Keep progress unique across that revalidation.
- Durable proof gate: a multipart staging record carries `ProofPending`, set and
  flushed before any known-digest proof starts and cleared and flushed only
  after it succeeds. On resume it forces the proof in every mode except `none`
  (including `size`) whenever a known digest exists, so a crash mid-proof, a
  mismatch whose cleanup failed, or a later mode
  change cannot publish unproven all-Done ranges. A mismatch keeps the flag set,
  attempts poison (truncate to zero) only while the claim is still owned under
  lifetime ownership and local metadata serialization, and attempts the owned
  discard; if ownership cannot be verified, poison is skipped and the durable
  flag remains the guard, so the next run re-proves or discards. Publish refuses while a known digest's proof
  is pending; `none` is the explicit opt-out. The single-path `.part` is its own
  marker when a proof is required (sha256/etag, inherited bytes, or a known
  digest with unknown size): it is renamed only after proof, so every restart
  re-proves it. (The sidecar's `framing_complete` is response-framing evidence,
  not a content proof.) A fresh size transfer with a known size follows the size
  contract and may rename without a hash.
- OLD OR UNSUPPORTED PARTIALS ARE DISCARDED, NEVER MIGRATED. Only a version-4
  `.incomplete`/`.meta` record is resume state. A record of any other version
  (including a branch-only version-3 record whose `legacy_migrated` flag
  described imported bytes) and every pre-#140 `.part-NN`/`.parts.json` family
  is discarded under the claim/domain and the file is re-downloaded from zero;
  a warning is logged. The multipart route never imports a single-route
  `<dst>.part`: a sidecar-bearing `.part` is resumed only by the single-request
  route under its own identity rule, and a sidecarless `.part` is discarded by
  that route and never inherited. Old artifacts may be removed by the
  multipart route only under its claim/domain, or left for the existing
  success/cleanup path; a live single-route `.part`/sidecar is never removed by
  the multipart route. Already-published blobs, completed files, and the
  snapshot/friendly entries are untouched. An oversized single `.part` is
  invalid: discard it, never clamp it. An unreadable or unproven current-format
  record is refused with `ErrStagingOwnershipUnproven` and preserved, never
  auto-deleted.
- A ROUTE REMOVES ONLY THE ARTIFACTS IT CREATES. The multipart HEAD fail-fast
  creates no `.part`: a `<dst>.part`/`.part.meta` next to the destination
  belongs to the single-request route (in flat/output mode possibly to a LIVE
  writer of it) or to an earlier run, and in flat/output mode the multipart
  route reaches that HEAD holding no domain at all, so it leaves the pair
  untouched. Only the single-request fail-fast removes its own zero-byte partial
  (with its sidecar), because that route holds the destination domain and knows
  the partial is the one its attempt created.
- A publish target's existence proves completion only when it is
  content-addressed (`blobs/<known-sha>`). Arbitrary destinations follow the
  authoritative pre-download skip decision and are redownloaded and atomically
  replaced when it declined them.
- One blob key owns ONE publication domain, not one domain per staging path.
  The ranged route (`blobs/<sha>.incomplete` -> `blobs/<sha>`) and the
  single-request route (`blobs/tmp-<sha>.part` -> `blobs/tmp-<sha>` ->
  `blobs/<sha>`) write the same key, so they must share one domain: an
  in-process slot keyed by the blob key plus the lifetime OS lock on the
  persistent sibling `blobs/tmp-<sha>.lock`, held for the whole operation on
  that key. One domain means the other route can neither adopt nor delete this
  route's LIVE partial (a live partial is never mistaken for a
  pre-#140 leftover), and the "the blob does not exist yet" decision and the
  placement that follows it cannot be overtaken. EVERY writer of `blobs/<sha>`
  must take that domain and publish through the shared no-replace primitive —
  including the exported `FinalizeBlob`, which must never rename over an
  existing content-addressed blob. The domain lock file is
  persistent, is never unlinked, and is excluded from cache statistics,
  listings, mirrors and cache deletion like every other `.lock` artifact.
  The domain is acquired ONCE per item, after the pre-download skip decision
  and before the HEAD/claim, and held through the claim, transfer, publication,
  store and cancel cleanup; it is released when the item returns. This is the
  ONLY ownership fence of the destination: the multipart staging claim receives
  the held domain, verifies that it covers the claim's staging path, and fails
  closed when it does not. It takes no second OS lock and registers no second
  in-process owner.
- EVERY CACHE DESTINATION OWNS ONE OWNERSHIP DOMAIN, NOT ONLY THE
  CONTENT-ADDRESSED ONES. The domain is keyed on the DESTINATION name and is
  the SAME identity for the writer and the cleanup:
  - a content-addressed item (a supplied canonical SHA-256) stages at
    `blobs/tmp-<sha>`; its name is the digest, so its domain is exactly the
    blob key's domain: key `blobs/<sha>`, lock `blobs/tmp-<sha>.lock`.
    `holdsBlobKey`, the publication domain, `FinalizeBlob` and the existing
    cache cleanup are unchanged for it.
  - an item WITHOUT a known digest (the planner allows an empty SHA-256) stages
    at the STRUCTURALLY NON-CANONICAL
    `blobs/tmp-path-<sanitized relative path>`. Its domain key is
    `blobs/path-<sanitized relative path>` and its lock is
    `blobs/tmp-path-<sanitized relative path>.lock`. "path-" is not a 64-hex
    digest, so neither identity can be reached by a blob key or lock, and the
    staging name can never equal a content-addressed `tmp-<sha>` name.
  The `path-` prefix is REQUIRED, not cosmetic. Without it a digest-less
  item's destination name was `blobs/tmp-<sanitized path>`, so file A at path
  `SHA256(Y)` (content X) held the domain `blobs/SHA256(Y)` while file B at
  path `SHA256(X)` (content Y) held `blobs/SHA256(X)`; A's store then needed
  the blob domain of its computed digest `SHA256(X)` and B's store needed
  `SHA256(Y)`, and each waited for the domain the other held. The in-process
  wait is unbounded (bounded only by the job's context), so the job hung until
  cancelled; returning transfer slots does not break an ownership cycle. The
  same prefix also prevents a digest-less item from sharing a content-
  addressed `tmp-<sha>` staging file. Because the staging name is now
  unambiguous, the domain stays name-based, the writer and the cleanup agree
  on ONE identity, and the two-item deadlock and the staging-name collision
  cannot be constructed. The user-visible final/snapshot paths do not change;
  only the internal staging name does.
  A writer that finds the destination owned by a PROVEN
  live owner waits for it (bounded, context-aware, never a takeover) and then
  obtains the file itself: a path-derived destination has no published
  content-addressed target to re-check (its bytes are moved into a blob whose
  name is the digest computed at store time, which the waiting writer cannot
  know before it transfers the file), so the item re-downloads these small
  non-LFS files and the store then reuses the already published blob instead
  of replacing it. A writer of a cache destination that cannot take its
  domain fails the item BEFORE it writes any payload, exactly like the
  content-addressed case; the partial data of an earlier run is preserved.
  The legacy/output (flat) mode has no `blobs/`-style internal identity, so it
  borrows the MULTIPART staging name for the destination's ONE domain: the
  domain key is the destination path, and the domain's lifetime lock is the
  persistent sibling `<destination>.incomplete.lock`. That lock file is a
  zero-byte artifact next to the user's model file and is never unlinked, and
  NO new artifact name appears next to the user's model file. The SAME domain
  is acquired ONCE at item level for EVERY route (cache single, cache
  multipart, flat single, flat multipart), after the skip decision and before
  the HEAD/claim, and held through the claim, transfer, publication, store and
  cancel cleanup. ONE lock file therefore excludes every pair of writers of the
  same flat destination: single-vs-single, single-vs-multipart,
  multipart-vs-multipart and cleanup-vs-any-writer. The multipart staging claim
  does NOT take a second lock: it verifies that the held domain covers its
  staging path (a missing, released or non-covering domain fails the claim
  closed), performs its record transitions under the intra-item metadata gate,
  and records `LockFenced`. The earlier "deadlock rule" (skip the item-level
  acquisition for the multipart route so it would not contend with its own
  claim) existed only because the claim took the same lock file; with one
  acquisition there is no self-conflict and no route-specific exception, and
  the unknown-size hand-off to the single-request path keeps the domain the
  item already holds instead of taking it a second time.
  A concurrent second writer of the same flat destination WAITS for the
  domain — bounded by the ownership budget for a cross-process owner, by the
  item's context for an in-process holder — and never takes it over. When the
  owner still holds it at the budget's end, the item fails with the actionable
  live-owner error ("blob <sha> is being downloaded by another process; try
  again later"); when the owner releases it, the writer obtains the file itself
  (a flat destination has no published content-addressed target to re-check),
  instead of racing it, which is safer than the pre-#140 behavior, where two
  processes wrote the same `.part-NN` payload files and could publish a
  silently corrupt model. A flat destination whose ownership domain cannot be
  acquired (the OS lock is unsupported, denied, or the filesystem failed) fails
  the item BEFORE it writes any payload, exactly like cache mode: that is the
  fail-closed consequence of every route owning a domain. And because every
  route takes that domain, EVERY flat download leaves the persistent zero-byte
  `<destination>.incomplete.lock` next to the file, even when no multipart
  transfer ran: the shared domain/staging lock is created by the item-level
  acquisition, not by a multipart transfer.
- JOB CONTRACT: "job completed successfully" MUST mean the requested files are
  present AND reachable at their snapshot/friendly paths. A file owned by
  another LIVE writer is NOT present, so it is NEVER a successful skip. When
  the destination's ownership domain is held by a proven
  live owner (another process, or another writer of this process), the item
  WAITS for the domain — bounded, context-aware, never a takeover — and, once
  it holds the domain, RE-CHECKS the published content-addressed target:
  - the blob is present (the owner published it while the item waited): the
    item completes as an ordinary skip ("blob exists"), with its own snapshot
    and friendly entries created and NO additional network transfer;
  - the owner released the domain without publishing: this writer downloads
    the file itself and publishes it;
  - the owner still holds the domain when the wait budget expires: the item
    fails with an explicit, actionable error ("blob <sha> is being downloaded
    by another process; try again later"), and the job does not report
    success while that file is missing.
  The re-check trusts the EXISTENCE of the published content-addressed blob as
  completion proof, exactly like the pre-existing "blob exists" skip does, and
  does not re-verify its content: in `size` mode a torn blob written by a
  foreign writer is therefore accepted as complete — the same content-addressed
  assumption the pre-existing skip already makes. Do not claim content
  verification for the wait/re-check path.
  A PATH-DERIVED destination (an item without a known digest) has no published
  content-addressed target to re-check, so after it waited for the domain the
  item OBTAINS the file itself — it re-downloads these small non-LFS files —
  and the store then reuses the already published blob. A successful skip that
  leaves the file missing is forbidden in every case.
  The wait budget is bounded and derived from the existing ownership-lock
  convention (the download budget is 60 times the 5s acquisition window, so
  the effective budget is 5 MINUTES); it is a package-level knob, not a user
  setting, and the in-process slot wait is bounded by the job's context only
  (its holder is a goroutine of this process that finishes or is cancelled).
  Cancellation stays prompt. A DESTINATION-DOMAIN waiter does not hold a
  `MaxActiveDownloads` slot: the slot is a per-item lease that the
  ownership-domain acquisition (the in-process slot wait and the cross-process
  OS-lock wait alike) returns for the duration of every wait and takes back
  before it returns a domain the caller will transfer under. The multipart
  claim performs no ownership wait of its own any more (it runs under the held
  domain and only takes the intra-item metadata gate), so it does not move the
  slot. The slot therefore bounds concurrent TRANSFERS, not waiters, so the
  other files of the job proceed while one item waits — a low limit no longer
  keeps the transfer budget from starting the files that could transfer.
  Waiters do not consume transfer slots (any number of items may wait) while
  the configured limit still bounds concurrent transfers. A contended item
  that has acquired its domain may wait to take its slot back, and that wait
  cannot deadlock: an item never blocks on a domain while holding a slot, and
  two contenders for one destination cannot both hold its domain. A FAILED
  acquisition keeps the slot returned (no domain reaches the caller, so the
  item reports the error instead of transferring). Waiting items are still
  goroutines bounded by the job's file set, and the slot is reacquired before
  the transfer. The acquisition's waits and budgets are unchanged: an
  in-process contender waits for the current owner bounded by the item's
  context, a cross-process PROVEN live owner is waited out until the ownership
  budget expires and is then reported with the actionable ownership error, and
  the local metadata gate is waited out within `stagingLockWait`.
  What the operator sees:
  the item stays in progress (no progress events are emitted while it waits,
  so its byte counter does not move), and when a budget expires or a live
  owner is reported the item fails with the actionable error above and the
  job's status is failed, not completed. The existing "blob exists" skip (the
  file IS present) and the existing unknown-ownership error
  (`BlobOwnershipUnknown`) are unchanged.
- A LINK-CREATION FAILURE FAILS THE ITEM, IN EVERY PATH. A cached file is
  reachable through its snapshot entry (and its friendly entry); the payload in
  `blobs/` alone is not the requested path. The transfer path already treated
  a failed link creation as fatal for the item, so the two SKIP paths (the
  pre-download "blob exists" skip and the post-domain "blob exists" skip) must
  agree with it: a link-creation failure makes the item FAIL with an
  actionable error ("the file is in the cache but its cache path is not
  reachable"), and the item emits NO `file_done` success. EVERY path that
  creates the cache entries — both transfer paths and both skip paths —
  returns the exported `ErrCacheLinkUnreachable` sentinel, so the failure is
  uniform and detectable with `errors.Is`. The end-of-job
  reconciliation that re-creates the links of skipped items surfaces its link
  failures as a JOB error instead of only a warning, so a job cannot finish
  "successful" with a missing path; its other duties are unchanged. The
  friendly entry is deliberately ABSENT when `Settings.NoFriendlyView` is set,
  so that is NOT an error.
- CLEANUP PARTICIPATES IN THE SAME OWNERSHIP PROTOCOL. The artifacts of one
  cache destination — the `blobs/tmp-<name>` staging name, its single-request
  `.part`, the obsolete pre-#140 `.part-NN` parts and the blob-keyed staging
  pair — are removed only while that destination's ownership domain is held.
  The cleanup uses the SAME name-based identity the writer holds: destination
  `blobs/tmp-<name>` -> key `blobs/<name>`, lock `blobs/tmp-<name>.lock`. For a
  content-addressed `tmp-<sha>` that is the blob key's domain; for a
  path-derived `tmp-path-<sanitized path>` it is `blobs/path-<sanitized path>`
  with lock `blobs/tmp-path-<sanitized path>.lock`, which no blob key or lock
  can reach. The staging name is unambiguous, so there is ONE name-based
  identity per destination. A cleanup without
  the domain
  could delete another live writer's LIVE partial: that writer would keep
  writing to an unlinked inode and its later publication would fail with
  ENOENT. When a live owner holds the destination (in this process or in
  another one),
  the artifacts are left alone and the contention is reported. A job CAN still
  clean up its OWN artifacts: its writer releases the domain when it returns,
  and the downloader's own cancel cleanup removes its staging while it still
  holds the domain. The residual limitation: the cleanup cannot distinguish
  "my own destination" from "another live owner of the same destination" beyond
  the domain itself, so a destination that another writer owns is left alone
  (safe) even when the cancelling job also had bytes for it. A legacy/output-mode
  destination has no `blobs/`-style internal identity, so its cleanup derives
  the SAME domain identity the writer holds: the destination's domain lock is
  the persistent sibling `<destination>.incomplete.lock`. Its artifacts are
  removed only while that domain is held, so a cancelled job cannot delete
  another live job's `.part`/`.incomplete`/`.meta` for the same path.
  Because the domain is the only fence, the cleanup entry points are collapsed:
  ONE owner-checked removal runs while the caller holds the domain and the
  metadata gate (`removeStagingArtifactsLocked`, used by the claim transitions,
  the publication and the cancel cleanup), and ONE record-checked removal takes
  the metadata gate and deletes only a pair that no live owner can be using
  (`removeStagingArtifactsIfUnowned`, used by the external cleanup paths). The
  old `removeStagingArtifacts`/`...IfUnclaimed`/`...IfUnclaimedLocked` variants
  and the in-process claim registry existed only to work around the claim's
  second lock and its self-conflict, and are gone. A cache `dst` with no
  derivable domain is skipped with a warning instead of removing unowned
  artifacts. The versioned staging pair is still record-checked, and the single
  `.part`, the obsolete pre-#140 `.part-NN` parts and `.parts.json` are removed
  as before. The persistent `<destination>.incomplete.lock` is never unlinked.
- A content-addressed publication has TWO tiers, and they guarantee different
  things. The tier is chosen by the FILESYSTEM, not by the operating system.
  - ATOMIC NO-REPLACE (a filesystem that can create hard links): place the
    payload with a hard link, which fails when the name exists, so the
    existence decision and the placement are ONE filesystem operation. An
    existing `blobs/<sha>` is then never replaced, proven or not, by another
    writer of the same key or by a writer outside the domain. The normal path
    stays rename-only in cost (a hard link is a directory operation, not a
    payload copy).
  - DOMAIN-EXCLUSIVE (a filesystem that cannot create hard links): the Go
    standard library exposes NO atomic no-replace rename on any supported
    platform — `os.Rename` replaces an existing target on Unix and on
    Windows, and `syscall` exports neither `renameat2(RENAME_NOREPLACE)` nor
    `renamex_np(RENAME_EXCL)` — so the placement is a check-then-rename
    performed while the blob key's ownership domain is held exclusively. The
    property that holds there is "an existing `blobs/<sha>` is never replaced
    by a domain participant", NOT "never replaced" unconditionally: a writer
    outside the domain (the mirror writer) is not fenced by it. The placement
    must refuse to run this tier without the ownership domain of the blob key
    it publishes, so the weaker guarantee is never reached by accident.
  Hard-link support is not a prerequisite for downloading: the fallback tier
  publishes normally, and the two tiers differ in strength, not in
  availability.
- The ownership check reports three outcomes and never guesses: a PROVEN live
  owner (the DESTINATION DOMAIN's lock is held exclusively) is
  `BlobDownloading` and the caller
  MUST NOT skip the item — the file is not present while the owner holds it, so
  the item waits for the destination's ownership domain and re-checks the
  published target (see the job contract above); a PROVABLY dead owner is
  `BlobStale` and may be resumed; an UNKNOWN ownership state (the probe failed,
  or the platform cannot represent the lock) is `BlobOwnershipUnknown` plus an
  error wrapping `ErrBlobOwnershipUnknown`, and the caller surfaces the
  failure. An unknown state must never become a successful skip: the job would
  report completion with the blob still missing. The incomplete payload is
  preserved. `CheckBlob` probes `blobs/tmp-<sha>.lock`, the destination domain
  lock that every writer of the destination holds. It does not probe the
  obsolete `<staging>.lock` of earlier branch-only builds: no current writer
  takes it, and treating it as ownership would report a live writer that does
  not exist. A free domain lock plus a lock-fenced record is stale; a free
  domain lock plus an unfenced record is not stale either - the record is
  preserved, and the claim refuses it with the unproven-ownership error.
- AN UNPROVEN STAGING RECORD IS NOT "ANOTHER LIVE WRITER". A staging record
  that was written without the lock fence (`LockFenced == false`, which no
  released version ever wrote) or that cannot be read/parsed is refused by the
  claim, because its owner can be proven neither alive nor dead. The refusal
  is the owner-decided rule and it does not change: the record and its partial
  are PRESERVED and never taken over — no age-based takeover, no takeover
  without proof of same-host death. What changes is the operator-visible
  error: a PROVEN live owner keeps "blob <sha> is being downloaded by another
  process; try again later" (transient — the owner may publish at any moment),
  while the unproven record says that its state "cannot be proven to belong
  to a live or dead owner (legacy or unreadable record)", that the partial is
  preserved, and that removing the staging pair is the recovery. The two are
  distinguishable by the exported sentinel `ErrStagingOwnershipUnproven`,
  which only the unproven case carries. OPERATOR PROCEDURE: the automatic
  recovery is deliberately absent, so the operator removes the staging pair
  (`<staging>` and `<staging>.meta`, e.g. `blobs/<sha>.incomplete` and
  `blobs/<sha>.incomplete.meta`) and retries; the download then starts from
  zero. Never remove the destination's persistent domain lock
  (`blobs/tmp-<sha>.lock` for a cache destination, `<destination>.incomplete.lock`
  for a flat one), and never remove a
  staging pair whose owner is a PROVEN live owner — that owner is still
  writing to it.
- Ownership is an OS-level exclusive lock on the persistent DESTINATION
  DOMAIN lock (Unix `flock(LOCK_EX|LOCK_NB)`, Windows exclusive-share
  `CreateFile`), acquired ONCE at item level before the HEAD/claim and held
  through transfer, hashing, publish/store and cancel cleanup. A busy lock is a
  live owner (the item waits for it and, when the budget expires, fails with
  the actionable live-owner error; the cleanup skips; `CheckBlob` reports
  downloading); a crashed process releases it at death, and a suspended owner
  keeps it — there is deliberately no takeover while it is alive. A free lock is
  authoritative only for a record written under it (`LockFenced`). An unfenced
  record is protected unless
  its same-host owner is provably dead; age, ambiguous host/PID identity, or a
  free new lock does not prove that an older writer cannot resume. If the
  domain lock cannot be acquired (unsupported, denied, or filesystem error),
  fail the operation before mutating claims or payloads and preserve partials;
  this is an operation-time safety error, not a permanent platform-support
  policy. Keep the domain lock file: never unlink it (that would let two
  processes own different inodes).
  The claim itself takes NO OS lock. It trusts the held domain, which is why a
  second `<staging>.lock` fence is not only redundant but harmful: in flat mode
  it was literally the same file as the domain lock, and the writer would
  contend with itself. The record's `LockFenced` flag is therefore written from
  the held domain (`true` for every current claim and take-over).
  MIXED-VERSION WRITERS ARE INTENTIONALLY UNSUPPORTED. A branch-only build that
  still held `<staging>.lock` instead of the destination domain is not
  supported: such a writer's record is either an old/unsupported format (S0
  discards it) or an unfenced v4 record (refused as unproven and preserved),
  and no current code competes for its obsolete lock. Do not reintroduce a
  compatibility path for it.
- Before routing an item to the single-request path, detect usable CURRENT
  multipart resume state: a valid version-4 `<staging>.incomplete`/`.meta`
  pair (including the blob-keyed LFS pair). A ranged-capable item keeps the
  multipart route even below the configured threshold so the state is resumed;
  a fresh item with no state keeps the configured routing. A pre-#140
  `.part-NN`/`.parts.json` family is NOT resume state and does not force the
  multipart route; it is discarded when the multipart route owns the staging,
  or by the existing success/cleanup path, and the file is re-downloaded.
  Without range support, warn, download the full file through the single path,
  and remove the obsolete versioned pair only after the final file is durable
  and claim-checked; the pre-#140 family is removed by the single-path success
  cleanup.
- Serialize current-process record transitions with the per-staging metadata
  mutex; the destination ownership domain (held by the item) excludes foreign
  current-version mutators and makes the gate intra-item state.
  Never recover a transition by age or unlink a lock that a suspended holder may
  still own. Re-read claim identity before mutation; cleanup and publish require
  the held domain, and publish checks cancellation immediately before
  rename. A missing or foreign record is refused, never renamed.
- Install a claim record only as a complete, valid document, atomically and
  without replacing a record that already appeared. Stage it in a sibling temp
  and install it with a hard link (atomic, no-replace), falling back to an
  atomic rename — used only while no record is present — on a filesystem that
  cannot create hard links. Hard-link support is not a prerequisite for
  downloading. The staged copy must always be a fresh file: a staged path left
  behind by an interrupted operation may share the authoritative record's
  inode, and writing through it would empty the authoritative record before it
  was replaced.
- Size checks are only a fallback for files without a trusted hash.
- Known limitation (not a downloader invariant): the mirror writer copies
  destination blobs with a non-atomic truncate-and-write and a size-only check,
  so it can tear or overwrite a blob a downloader is publishing, and the
  selected-GGUF delete path removes blob payloads outside the blob key's
  ownership domain. Neither path takes the domain lock, so neither is fenced
  by the publication domain: they are the documented exception to "an existing
  `blobs/<sha>` is never replaced" (and, on a filesystem without hard links,
  to the domain-exclusive tier as well). The downloader's publication
  invariant must not depend on either path's behavior.
- THE SINGLE-REQUEST PATH INHERITS BYTES ONLY UNDER A PROVEN IDENTITY. The
  single-request path stages its bytes in `<dst>.part`; for an item without a
  known digest the staging name is derived from the item's path
  (`tmp-path-<sanitized path>`), so two different objects - or two revisions
  of one object - share one staging name, and the destination domain excludes
  concurrent writers without establishing WHICH object the staged bytes belong
  to. A size match is not an identity. Object identity is not clean response
  framing either: a process can exit after the final WriteAt and before
  io.Copy observes EOF, so an exactly-sized partial can still hold a torn
  final byte. The sidecar therefore also records, independently of the
  identity, whether the producing run observed a cleanly-framed body at the
  exact expected byte count (`framing_complete`). An exactly-sized partial is
  published without a request only when the identity matches AND that proof is
  present; otherwise the final byte is reserved and re-requested as the
  non-empty range `[size-1, size)`, and only a cleanly-framed response commits
  it. A body error during that revalidation leaves the final byte uncommitted
  for a later retry/restart, exactly like the original transfer. The `.part` is
  a sequential prefix with
  NO RANGE-LAYOUT record: what identifies it is the identity sidecar
  `<dst>.part.meta` (JSON, versioned), which records the identity in the same
  vocabulary the ranged record uses: repo, resolved commit, item relative
  path, expected size, whether the
  identity is PINNED (the commit was actually resolved), and the separate
  `framing_complete` completion proof. Bytes already on
  disk are inherited ONLY when that record exists, is readable and well-formed,
  matches the CURRENT item exactly (repo, commit, path, size) and is PINNED on
  both sides. Otherwise the inherited bytes are discarded (the partial is
  truncated) and the transfer starts from zero: unproven bytes are never
  published. An UNPINNED identity (the planner fell back to the requested
  revision because the endpoint did not resolve a commit) pins nothing - a
  moved branch may change the content under that name - so nothing is
  inherited from it. The record is written before any inherited byte is
  trusted and after the unproven bytes are gone, is removed together with the
  partial it describes (on publication, in the cancel-cleanup path and after a
  failed content proof), so no identity record outlives its partial, and it
  survives an interrupted run:
  that surviving record is what makes the next run's decision possible. The
  cancel-cleanup path closes the single-path partial's writable handle BEFORE
  it removes anything: the content proof could previously reach that cleanup
  while the handle was still open, and Windows cannot unlink an open file, so
  an explicit cancel during the proof must not retain the whole `.part` and
  its sidecar. Pause/internal aborts still preserve both.
  A `.part` without a matching identity record is not inherited. The
  known-digest content proof is unchanged and still applies to the inherited bytes.
- A PERMANENT ERROR MUST NOT DESTROY THE RESUME IDENTITY. The single-request
  path's fail-fast branch (a permanent 401/403/404) removes the zero-byte
  partial the attempt created, and the identity sidecar goes with it - but ONLY
  with it. A NON-EMPTY partial holds resumable bytes, and the sidecar is the
  only proof that those bytes belong to this object; removing the record while
  the bytes stay would make the next run discard them, so a permanent (but
  eventually fixable) error would silently lose the progress. The sidecar
  therefore SURVIVES with a non-empty partial, and the next run with the same
  identity resumes from the stored offset. The same rule applies to every other
  branch that removes the sidecar: it may only go when the partial it
  describes is actually gone.
- THE RANGED PATH INHERITS STAGED BYTES ONLY UNDER A PINNED IDENTITY - THE
  SAME RULE AS THE SINGLE-REQUEST PATH. The ranged claim record carries the
  repository, the resolved commit, the item's relative path, the expected size
  and whether that commit was actually resolved (`commit_pinned`); the record
  is resumable ONLY when the identity matches the current item exactly (repo,
  commit, path, size), the identity is PINNED on both sides (the record's own
  flag and the current plan's), the blob SHA is present on both sides or
  neither, the range layout tiles the file with normalized completion, and the
  payload exists at the expected logical size. Otherwise the record is
  discarded and the payload is downloaded again. An UNPINNED identity (the
  planner fell back to the requested revision because the endpoint did not
  resolve a commit) pins nothing - a moved branch may change the content under
  that name - so no staged bytes are inherited from it, and for an item
  without a known digest there is no content proof that could catch the
  mixture either. A record written WITHOUT the pinned flag is unpinned and
  therefore not inheritable: the safe direction, and no released version ever
  wrote these records. A pinned, matching record keeps its resume exactly as
  before: only the missing bytes are re-fetched. The two routes consult ONE
  shared rule, so they cannot drift apart.
- STAGING ARTIFACTS AND PUBLISHED CONTENT ARE SEPARATED BY ONE EXHAUSTIVE
  CLASSIFIER. The downloader creates: the single-request staging name
  `tmp-<name>` (including the path-derived `tmp-path-<sanitized path>` form)
  with its `.part`, its `.part.meta` identity sidecar and its `.lock` domain
  lock; the ranged staging pair `<name>.incomplete` +
  `<name>.incomplete.meta` with its atomic record temp `<name>.meta.tmp`; the
  persistent ownership lock `<name>.lock`; the obsolete pre-#140 multipart
  family `<name>.part-NN` and `<name>.parts.json`; the flat-mode partials
  `<destination>.part` and `<destination>.incomplete`; and the cache store's
  atomic copy temp `<name>.tmp-<random>`. A published blob is named by its
  content: a canonical 64-hex SHA-256 digest. A file in `blobs/` that is
  neither a staging artifact nor a canonical digest is an UNUSUAL published
  name (the public storage API accepts it): it is content - the cache
  statistics count it, the listing shows it, the selection sees it, the mirror
  copies and verifies it, and the deletion path may delete it. The
  classification is by NAME SHAPE, so a `blobs/` name that CARRIES a staging
  shape (`tmp-<name>`, `<name>.part`, `<name>.part.meta`, `<name>.lock`,
  `<name>.incomplete`, `<name>.incomplete.meta`, `<name>.meta.tmp`,
  `<name>.part-NN`, `<name>.parts.json` or `<name>.tmp-<random>`) is treated
  as a staging artifact by EVERY surface, including the deletion path (the
  conservative choice: a staging artifact is never deleted), even when such a
  name was published content; a canonical 64-hex blob is unaffected. The
  staging exclusion applies ONLY inside `blobs/`: a
  legitimate `*.meta` (or `*.part`, `*.lock`) file outside that namespace - a
  snapshot entry, for example - is content, and the mirror copies AND verifies
  it, so copy and verify always agree on the same set. Every consumer - the
  mirror scan, copy and verification, the cache statistics, the cache listing,
  the cache selection and the cache deletion - uses that one classification.
- A BLOB KEY WHOSE NAME IS A STAGING SHAPE IS REFUSED AT THE BOUNDARY. Because
  the classification is by name shape, a blob published under a staging-shaped
  name would be INVISIBLE: the statistics, the listing, the selection, the
  mirror and the deletion path would all exclude it, so a successful store
  would have no reachable result. Every public path that chooses a blob name
  from a caller-provided key therefore refuses such a name BEFORE anything is
  placed: `StoreDownloadedFile` (and its context-bounded implementation) and
  `FinalizeBlob` reject it with an actionable error that names the staging
  namespace. The refusal is narrow - only the NAME SHAPE is refused - so a
  non-canonical but NON-reserved name (an unusual published name) stays
  accepted, stored, visible and mirrored.
- Keep retry resume offsets based on bytes actually written to disk.
- Do not delete partial data on transient network failure unless it is known to
  be stale or incompatible.
- Use context-aware sleeps/requests so pause, cancel, shutdown, and requeue stop
  promptly.

## Job and server state rules

- `JobManager` owns live job state; callers should receive snapshots, not shared
  mutable pointers.
- A job that reports success must mean the requested files are present AND
  reachable at their snapshot/friendly paths. A file
  owned by another live writer is not present: it is waited for (bounded) and
  re-checked, never reported as a completed skip. When the wait is exhausted
  the item fails with an actionable error, and the job's status is failed, not
  completed — see the downloader correctness rules for the ownership protocol.
  The wait budget is 5 minutes, and the wait runs inside the item's goroutine.
  A DESTINATION-DOMAIN waiter does not hold one of the job's
  `MaxActiveDownloads` slots: the slot is a per-item lease returned for the
  duration of every ownership-domain wait (in-process and cross-process
  alike) and taken back before the acquisition returns, so the
  slot bounds concurrent TRANSFERS rather than waiters. The multipart claim
  takes no ownership wait of its own any more (it runs under the held domain
  and only takes the intra-item metadata gate), so it does not move the slot.
  Other files of the job
  therefore proceed while one item waits, even with a low limit; waiters do
  not consume transfer slots, while concurrent transfers still obey the
  configured limit. A failed acquisition keeps the slot returned. Waiting
  items are still goroutines bounded by the
  job's file set; the acquisition's waits and budgets are unchanged (an
  in-process contender waits for the current owner bounded by the item's
  context; a cross-process PROVEN live owner is waited out until the ownership
  budget expires and is then reported with the actionable ownership error; the
  local metadata gate within `stagingLockWait`). What the
  operator sees: the item stays in progress (no progress events are emitted
  while it waits, so its byte counter does not move), and when a budget
  expires or a live owner is reported the item fails with the actionable
  error and the job's status is failed.
  A failed link creation (the snapshot or friendly entry of a cached file
  could not be created) also fails the item, in the transfer path and in both
  skip paths alike: the payload may be in `blobs/`, but the requested path is
  not reachable, so the job must not report success. A job that ends with such
  a failure is `failed`, not `completed`.
- Do not hold the job manager lock while broadcasting to listeners or WebSocket
  clients.
- Scheduler changes must preserve `max-active` semantics: queued jobs start in
  order, lowered limits requeue excess running jobs, and finished jobs dispatch
  the next queued item.
- Generation checks protect against stale `runJob` goroutines; keep them when
  changing pause/resume/retry/requeue flows.
- Progress events should distinguish transfer, finalizing, completion, skipped,
  error, and cancellation states.
- Speed and ETA should use bytes transferred in the current run, not already
  resumed/skipped bytes.

## Cache and filesystem rules

- HF cache mode follows Hugging Face-style layout under `hub/` with blobs,
  snapshots, refs, and friendly `models/` or `datasets/` views.
- Local mode writes real files under the configured local directory.
- Do not follow untrusted paths outside configured cache/local roots for delete,
  mirror, or rebuild operations.
- Prefer atomic writes for metadata/state files where practical.
- Symlink behavior must remain safe on Windows; preserve existing fallbacks and
  warnings.
- Cache delete and mirror operations must tolerate partial failure and report it
  clearly.

## API and UI rules

- Keep `docs/API.md` in sync with API contract changes.
- Server validation is authoritative; UI validation is convenience only.
- Do not expose full Hugging Face tokens through API responses or logs.
- WebSocket updates should be coalesced/throttled where high-frequency progress
  events would cause render churn.
- Frontend changes should preserve the existing visual language and static-asset
  architecture; do not introduce a JS build system for small UI edits.
- For UI or download behavior changes, do a manual browser smoke test when
  feasible.

## Smart download rules

- `pkg/smartdl` should classify repositories and selectable files without
  starting downloads.
- Recommended download output should remain a structured request body, not a
  shell command string contract.
- Exact matching and filter behavior must remain predictable for GGUF quants,
  sharded files, mmproj companions, safetensors, and datasets.

## Service mode rules

- The supervisor owns the lifecycle: HFDesk stays a foreground process, with
  no self-daemonization, no PID file, no shell wrapper, and no startup
  shortcut.
- The service is headless: it never opens a browser and never shows a tray
  icon.
- Service mode binds loopback by default; a non-loopback address must be
  requested explicitly, and `install` and `status` report it.
- Credentials never enter a supervisor registration, a command line, or a unit
  environment; they live in the service-owned config file.
- `uninstall` removes the registration only and never deletes models, cache,
  config, jobs/history, or logs.
- Platform-specific supervisor code stays in `internal/service`; the shared
  layer must not branch on the operating system.

## Testing expectations

- Downloader changes: run `go test ./pkg/hfdownloader` at minimum.
- Server/job/API changes: run `go test ./internal/server` at minimum.
- Smart analysis changes: run `go test ./pkg/smartdl` at minimum.
- Cross-package behavior changes: run `go test ./...`.
- Concurrency, cancellation, WebSocket, or shared-state changes: consider
  `go test ./... -race`.
- Add regression tests for corruption, resume, cancellation, scheduler, and API
  contract bugs whenever practical.

## Commit and PR conventions

Follow `CONTRIBUTING.md`:

- Commit/PR title template: `<scope>: <imperative summary>`
- Common scopes: `server`, `downloader`, `smartdl`, `ui`, `cmd`, `workflow`,
  `docs`, `tests`, or `fix(<area>)`
- Keep commits focused on one logical change.
- PR body should include summary, why, validation, and risk.

## Regression-prevention checklist

- Could this change corrupt an existing partial download or cache entry?
- Does cancellation stop before local assembly/finalization starts?
- Does retry/resume preserve already-downloaded bytes only when they are known
  compatible?
- Does a settings change affect currently running jobs, future jobs, or both?
- Does API state survive page refresh, server restart, and paused/retried jobs?
- Are skipped/resumed bytes excluded from live speed calculations?
- Can a failed fetch/delete/mirror operation masquerade as an empty success?
- Are tokens, local paths, and cache deletes handled safely?

## Recent changes

- High-level release notes: `CHANGELOG.md`
- Current working notes: `TODO.md`
- Recent commits: `git log --oneline -10`
