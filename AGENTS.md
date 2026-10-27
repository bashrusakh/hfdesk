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
- Downloader/cache correctness contract: `pkg/hfdownloader/doc.go` (canonical
  detailed contract; the Downloader contract section below is a summary)
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

## Downloader contract

The canonical, detailed contract for downloader and cache correctness is
`pkg/hfdownloader/doc.go`. Read it before changing downloader, cache, mirror,
or job ownership behavior. This section is the agent-level summary; when it and
`doc.go` differ, `doc.go` is authoritative and this summary must be updated.

- ONE destination ownership domain per destination, acquired ONCE at item
  level for every route (cache single, cache multipart, flat single, flat
  multipart), after the skip decision and before the HEAD/claim, and held
  through the claim, transfer, publication/store and cancel cleanup. A
  content-addressed cache item uses key `blobs/<sha>` and lock
  `blobs/tmp-<sha>.lock`; a digest-less item stages at
  `blobs/tmp-path-<sanitized path>` with key `blobs/path-<sanitized path>` and
  lock `blobs/tmp-path-<sanitized path>.lock`; a flat destination uses its path
  as the key and `<destination>.incomplete.lock` as the lock. The lock file is
  persistent and never unlinked. `FinalizeBlob` and every writer of
  `blobs/<sha>` participate in that key's domain.
- The multipart claim takes NO second lock: it receives the held domain,
  verifies that the domain covers its staging path, fails closed otherwise,
  serializes its record transitions with the intra-item metadata gate, and
  records `LockFenced`. `CheckBlob` probes the destination domain lock, not the
  obsolete `<staging>.lock`. Ownership is the destination domain plus
  `LockFenced`; the record is diagnostics/resume state, not a freshness lease,
  so there is no heartbeat and no timer rewrite, and `StaleTimeout` is removed
  (do not reintroduce an age-based timeout, lease or takeover). A proven live
  owner is waited for (bounded, context-aware, effective 5 minutes), never
  skipped and never taken over. An unavailable domain fails the item before it
  writes a payload and preserves existing partial data. Unfenced or unreadable
  current-format records are preserved
  and refused with `ErrStagingOwnershipUnproven`; never auto-delete such a
  record or its persistent domain lock.
- Resume is v4-only: the version-4 `<staging>.incomplete`/`.meta` record and
  the version-1 `<dst>.part.meta` sidecar are the only resume state. Any other
  version is never migrated: a fenced old-version record (or one whose owner is
  provably dead or is this process) is discarded under the claim and
  re-downloaded from zero, while an unfenced old-version record whose owner
  cannot be proven dead is refused as unproven and preserved (the ownership
  gate runs before the version branch). Every pre-#140 `.part-NN`/`.parts.json`
  family is discarded, never migrated, and re-downloaded from zero. Inherit
  staged bytes only under a
  matching identity pinned on both sides (repo, commit, path, size, through the
  one shared `resumeIdentity.matches` rule); the single-path exact-size shortcut
  also requires `framing_complete`. The whole transfer (tree walk and every
  per-file URL) is pinned to the resolved commit; an unpinned identity inherits
  nothing and is content-proved before publication. An oversized `.part` is
  discarded, never clamped. The sidecar goes only when its partial is actually
  gone; only a not-exist `stat` proves that.
- Only a content-addressed `blobs/<known-sha>` target is completion proof by
  existence; an arbitrary destination follows the authoritative pre-download
  skip decision. Publication asserts two independent gates: a complete range layout
  (`errStagingLayoutIncomplete`) and the exact payload length after flush
  (`errStagingPayloadSize`). Never assemble or publish after cancellation or a
  known part error; publication checks cancellation immediately before the
  rename. Content-addressed publication has two tiers: atomic hard-link
  no-replace normally, and domain-exclusive check-then-rename on a filesystem
  without hard links. The cache store's cross-device copy fallback runs inside
  the domain, but its final rename is not no-replace. A loser of a publication
  race discards its own staging pair. The mirror writer and the selected-GGUF
  delete path are outside the domain (known limitation).
- JOB CONTRACT: success means every requested file is present AND reachable at
  its snapshot/friendly path. A file owned by another live writer is never a
  completed skip: wait bounded, re-check the content-addressed blob, otherwise
  download it or fail with the actionable live-owner error. A link-creation
  failure fails the item in both transfer and skip paths with
  `ErrCacheLinkUnreachable`; a disabled friendly view (`NoFriendlyView`) is not
  an error. Waiters do not hold a `MaxActiveDownloads` slot: the slot is a
  per-item lease returned for every domain wait and taken back before transfer.
- Cache surfaces use one exhaustive name-shape classifier for staging
  artifacts in `blobs/`; unusual published names are content. Staging-shaped
  blob keys are refused by `StoreDownloadedFile` and `FinalizeBlob` before
  placement. Cleanup runs only under the destination domain, through the
  collapsed entry points `removeStagingArtifactsLocked` and
  `removeStagingArtifactsIfUnowned`. A route removes only the artifacts it
  creates (B1): for example, a multipart HEAD failure leaves a single-route
  `.part`/sidecar alone.
- `Settings.Verify` controls LFS verification too: `none`, `size` (default, no
  normal-path full-file reread), `sha256` (at most one pre-publication pass),
  and `etag` (the same pre-publication pass for a known digest; historical
  non-LFS ETag behavior). A known digest is never hashed only after publication.
  `ProofPending` forces a re-proof on resume in every mode except `none`. Size
  checks are only a fallback for files without a trusted hash.
- Keep retry/resume offsets based on bytes actually written to disk; do not
  delete partial data on transient network failure unless it is known to be
  stale or incompatible; use context-aware sleeps/requests so pause, cancel,
  shutdown and requeue stop promptly.

## Job and server state rules

- `JobManager` owns live job state; callers should receive snapshots, not shared
  mutable pointers.
- A job that reports success must mean the requested files are present AND
  reachable at their snapshot/friendly paths; the wait/re-check, link-failure
  and transfer-slot rules are in the Downloader contract above and in
  `pkg/hfdownloader/doc.go`. A job that ends with such a failure is `failed`,
  not `completed`.
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
