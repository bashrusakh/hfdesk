# Phase 1 implementation record

## Current candidate

Historical review of `4bcf7f976b1f8ad25e524de4a420a65bfeb6b2a0` requires changes;
corrected R at `caaa4e2cc5ed7ef817f5d453f3c802c81eebfae0` awaits fresh whole-R
review. The ordinary integration merge is `ae057f3d320629b8d327fe817f771337f029989e`,
with `origin/main` `8d3f9824959a5d2c4f089748f1f18fbf656233a5` as both base and
merge-base. The remote feature head remains `caaa4e2`; no publication is included.
The pre-amendment base diff is 4,946 additions plus deletions; keep the complete
candidate below 5,000. R is not a safety implementation and is not Ready.

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
available Go toolchain is 1.26.7, so native Go 1.25 CI and Windows/macOS behavior
remain unverified. The legacy handler/helper source slice was compared directly
with `b4644b7` and is identical. These checks are historical and do not verify
the corrected dirty candidate. The parent owns refreshed verification, new
whole-R review and publication; native Windows/Go 1.25 checks remain pending.
S must follow reviewed R and recover only bounded safety pieces
from A; do not merge or cherry-pick A.
