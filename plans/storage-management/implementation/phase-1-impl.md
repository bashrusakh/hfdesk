# Phase 1 implementation record

## Current candidate

The reviewed historical R source commit is `4bcf7f976b1f8ad25e524de4a420a65bfeb6b2a0`;
local finalization started from branch HEAD `1cc361afe65cbfb81de864e5c3e207d64e3d0751`
and committed the corrected source/test/CI bytes as `ff666365266d839c3719a390334ebe4de9aaa604`.
Both are on `feature/storage-root-ownership`, based on `0c876f38`; current
base/merge-base is `b4644b7ab33d58bf44de159b119cf5ae5ff8ea9e`. The worktree has
11 intended source/test/CI correction files in that commit. The 1cc review is
historical and requires changes: it identified three implementation/evidence
correspondence gaps. The corrected dirty worktree is a new candidate, not the
reviewed 1cc state; fresh whole-R review is pending. Base-to-HEAD is 4,751 lines;
with the intended corrections it is 4,899 before this status-only doc update
(<5,000; the committed status snapshot brings the full diff to 4,918 lines).
R is not a safety implementation and is not Ready.

Full WIP preservation A is committed locally at
`a9931836a6517259884908d451f2975b62d2dcc0` on
`work/storage-root-full-wip`. It contains all pre-split task WIP, including N,
native/Windows test fixes and prior plans. A is intentionally retained, never
published, and is not an ancestor of R.

## R changes in the current worktree

`pkg/hfdownloader` now models current declared namespace memberships, root roles,
physical directory observations, owned regions and entries separately. It keeps
logical slots across equal directory objects, leaves Local type unknown, bounds
enumeration and fails on incomplete reads. The old admission methods and
destructive validation helper are removed. `internal/server` consumes common
observations for local scan, details, Hub membership and friendly metadata;
the existing read DTO/source precedence and captured-config behavior are retained.
The legacy delete handler/helper match the exact `b4644b7` source block; the
pre-existing F7 gap remains for S.

Corrections add separately observed namespace-boundary identity, path, owner and
pruned facts before excluding those paths from owned regions; boundary
occurrences do not imply owned entries. Enumeration and Hub parsing share the
read-side model-name validator, preserving valid spaced and Unicode names
without granting deletion authority. Owner qualification errors propagate
through lookup to an HTTP error rather than becoming cache misses. Namespace
assertions cover mixed/phantom observations, and native CI selects Namespace
tests. The legacy delete handler and strict validator remain unchanged.

Guard/refusal tests and the previous native permission assertions are excluded
from R (their source remains recoverable from A/base for S). The Linux native
fixture retains the required test name and isolation worker, but now asserts
observed bind-mounted entry occurrence without claiming that the nested entry
belongs to the enclosing owned region. Namespace enumeration and Hub directory
parsing use the existing read-side repo-ID validator, preserving valid Unicode
and spaced names. Search observes the namespace once and returns an error for
incomplete observations rather than reporting an uncached miss. Regression
coverage also preserves case-distinct nested roots when the filesystem supports
them and verifies valid local repo IDs through list/details. The existing
relative-route test fixture now stays on the working-directory volume for
Windows path semantics.

## Evidence and next stage

On Linux Go 1.26.7, against the prior R source content, the focused downloader/server
tests, `go test ./...`, `go test ./... -race`, `go vet ./...`,
`go build ./cmd/hfdesk`, Windows test cross-builds for downloader and server, and
the existing Windows storage-selector tests passed. The selector run is Linux
execution, not native Windows evidence.

Required-native `TestNativeMount*` was attempted. The runtime worker-marker
negative case passed, but launching the positive private-mount subprocess failed
with `operation not permitted` before namespace setup; no mount fixture ran. The
available Go toolchain is 1.26.7, so Go 1.24 CI and native Windows/macOS behavior
remain unverified. The legacy handler/helper source slice was compared directly
with `b4644b7` and is identical. These checks are historical and do not verify
the corrected dirty candidate. The parent owns refreshed verification, new
whole-R review and publication; native Windows/Go 1.24 checks remain pending.
S must follow reviewed R and recover only bounded safety pieces
from A; do not merge or cherry-pick A.
