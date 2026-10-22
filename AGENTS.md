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
| `pkg/hfdownloader/` | Hugging Face scan/download/cache/mirror logic |
| `pkg/smartdl/` | model analysis and selectable download helpers |
| `docs/` | API docs and screenshots |
| `.github/workflows/` | CI, release, Docker workflows |

## Documentation map

Read relevant Markdown before changing behavior:

- General project behavior: `README.md`
- API contracts: `docs/API.md`
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
- Server routes: `internal/server/server.go`, `internal/server/api.go`
- Job manager: `internal/server/jobs.go`
- WebSocket hub: `internal/server/websocket.go`
- Config persistence: `internal/server/config.go`
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
- Multipart part files are only reusable when their byte-range layout is known
  to match the current file size and connection count.
- If legacy per-part resume metadata is invalid or mismatched, discard only the
  incompatible part files and restart rather than guessing. For a current
  `.incomplete.meta` claim, discard identity-mismatched data only while holding
  lifetime ownership; preserve unreadable or ambiguous legacy claims unless
  same-host owner death is proven.
- `Settings.Verify` governs LFS verification too: `none` performs no
  post-download content hash; `size` (default) checks exact size plus complete
  validated transfer coverage on both the ranged and single-request paths and
  performs no full-file SHA-256 pass on the normal path (the pre-download skip
  check, the one-time legacy-migration content proof, the inherited
  single-path `.part` proof, and a single-request known digest with no known
  expected size may still hash); `sha256` performs at most one
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
  re-proves it. A fresh size transfer with a known size follows the size
  contract and may rename without a hash.
- Bytes migrated from a pre-#140 partial are not trusted on layout alone: mark
  them LegacyMigrated, keep the marker durable across resume, and content-prove
  them with one full-file SHA-256 pass before publication whenever a known
  digest exists and `Verify != "none"`. An oversized single `.part` is invalid:
  discard it, never clamp it. A multipart layout whose parts are all
  missing/empty/oversized contributes no bytes and is not a migrated layout:
  fall back to a valid single `.part`; set LegacyMigrated only when bytes were
  actually copied.
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
  that key. One domain means the other route can neither adopt, migrate nor
  delete this route's LIVE partial (a live partial is never mistaken for a
  pre-#140 leftover), and the "the blob does not exist yet" decision and the
  placement that follows it cannot be overtaken. EVERY writer of `blobs/<sha>`
  must take that domain and publish through the shared no-replace primitive —
  including the exported `FinalizeBlob`, which must never rename over an
  existing content-addressed blob. The domain lock file is
  persistent, is never unlinked, and is excluded from cache statistics,
  listings, mirrors and cache deletion like every other `.lock` artifact.
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
  owner (the lock is held exclusively) is `BlobDownloading` and may be skipped;
  a PROVABLY dead owner is `BlobStale` and may be resumed; an UNKNOWN
  ownership state (the probe failed, or the platform cannot represent the
  lock) is `BlobOwnershipUnknown` plus an error wrapping
  `ErrBlobOwnershipUnknown`, and the caller surfaces the failure. An unknown
  state must never become a successful skip: the job would report completion
  with the blob still missing. The incomplete payload is preserved.
- Ownership is an OS-level exclusive lock on the persistent `<staging>.lock`
  (Unix `flock(LOCK_EX|LOCK_NB)`, Windows exclusive-share `CreateFile`), acquired
  before the claim transition and held through transfer, hashing and
  publish/cleanup. A busy lock is a live owner (claim skips, cleanup skips,
  `CheckBlob` reports downloading); a crashed process releases it at death, and
  a suspended owner keeps it — there is deliberately no takeover while it is
  alive. A free lock is authoritative only for a record written under it
  (`LockFenced`). A fallback/legacy record is protected unless its same-host
  owner is provably dead; age, ambiguous host/PID identity, or a free new lock
  does not prove that an older writer cannot resume. If the lifetime lock cannot
  be acquired (unsupported, denied, or filesystem error), fail the operation
  before mutating claims or payloads and preserve partials; this is an
  operation-time safety error, not a permanent platform-support policy. Keep
  the ownership lock file: never unlink it (that would let two processes own
  different inodes).
- Before routing an item to the single-request path, detect usable multipart
  resume state: a valid versioned `<staging>.incomplete`/`.meta` pair
  (including the blob-keyed LFS pair; v2 completed ranges revalidate the final
  byte) or a usable pre-#140 `.part-NN`/`.parts.json` layout. A
  ranged-capable item keeps the multipart route even below the configured
  threshold so migration/resume consumes the state; a fresh item with no state
  keeps the configured routing. Without range support, warn, download the full
  file through the single path, and remove the obsolete versioned pair only after the
  final file is durable and claim-checked; the legacy family is removed by the
  single-path success cleanup.
- Serialize current-process record transitions with the per-staging metadata
  mutex while the lifetime OS lock excludes foreign current-version mutators.
  Never recover a transition by age or unlink a lock that a suspended holder may
  still own. Re-read claim identity before mutation; cleanup and publish require
  the lifetime capability, and publish checks cancellation immediately before
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
- Keep retry resume offsets based on bytes actually written to disk.
- Do not delete partial data on transient network failure unless it is known to
  be stale or incompatible.
- Use context-aware sleeps/requests so pause, cancel, shutdown, and requeue stop
  promptly.

## Job and server state rules

- `JobManager` owns live job state; callers should receive snapshots, not shared
  mutable pointers.
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
