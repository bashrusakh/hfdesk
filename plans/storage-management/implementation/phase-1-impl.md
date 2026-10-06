# Phase 1 implementation record

## Current candidate

The feature branch remains based on published `0c876f38` with fresh base
`b4644b7`. The current uncommitted R candidate is limited to namespace facts,
read-side integration, fact tests, and compact plan updates. It is not a safety
implementation and is not Ready.

Full WIP preservation A is committed locally at
`a9931836a6517259884908d451f2975b62d2dcc0` on
`work/storage-root-full-wip`. It contains all pre-split task WIP, including N,
native/Windows test fixes and prior plans. A is intentionally retained, never
published, and is not an ancestor of R.

## R changes

`pkg/hfdownloader` now models current declared namespace memberships, root roles,
physical directory observations, owned regions and entries separately. It keeps
logical slots across equal directory objects, leaves Local type unknown, bounds
enumeration and fails on incomplete reads. The old admission methods and
destructive validation helper are removed. `internal/server` consumes common
observations for local scan, details, Hub membership and friendly metadata;
the existing read DTO/source precedence and captured-config behavior are retained.
The legacy delete handler/helper match the exact `b4644b7` source block; the
pre-existing F7 gap remains for S.

Guard/refusal tests and the previous native permission assertions are excluded
from R (their source remains recoverable from A/base for S). The Linux native
fixture retains the required test name and isolation worker, but now asserts
observed bind-mounted namespace regions rather than removal permission. The
existing relative-route test fixture now stays on the working-directory volume
for Windows path semantics.

## Evidence and next stage

On Linux Go 1.26.7, at feature HEAD `0c876f38` plus the current R worktree, the
focused downloader/server tests, `go test ./...`, `go test ./... -race`,
`go vet ./...`, `go build ./cmd/hfdesk`, Windows test cross-builds for downloader
and server, and the existing Windows storage-selector tests passed. The selector
run is Linux execution, not native Windows evidence.

Required-native `TestNativeMount*` was attempted. The runtime worker-marker
negative case passed, but launching the positive private-mount subprocess failed
with `operation not permitted` before namespace setup; no mount fixture ran. The
available Go toolchain is 1.26.7, so Go 1.24 CI and native Windows/macOS behavior
remain unverified. The legacy handler/helper source slice was compared directly
with `b4644b7` and is identical. Independent whole-R review and publication remain
with the parent. S must follow reviewed R and recover only bounded safety pieces
from A; do not merge or cherry-pick A.
