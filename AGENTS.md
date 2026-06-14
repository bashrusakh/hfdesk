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

- Language: Go 1.24 (`go.mod` is source of truth)
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
- If resume metadata is missing, invalid, or mismatched, discard stale part files
  and restart that file rather than guessing.
- LFS files with known SHA-256 must be verified after download.
- Size checks are only a fallback for files without a trusted hash.
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
