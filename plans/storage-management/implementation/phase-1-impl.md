# Phase 1 implementation record

Status: **F3-F6 corrections implemented locally; independent/native evidence pending**.
Earlier clean candidate `7dc62163e9ac5dffc5b126fcd18ce8ec7c212fee` received
changes-required review; source baseline HEAD is
`9498fc344a308f26dd02b5c9bbcebb2380ccdf93` and the dirty worktree carries
F4-F6. Base and merge base are `b4644b7ab33d58bf44de159b119cf5ae5ff8ea9e`.
The parent-reported 40 independent
Linux C1-C14 scenarios, including C10, were on the earlier candidate and do not
cover bind namespaces. No native mount reproduction was possible on this host.

Historical source correction/integration:
The correction is committed as `957498e59c3580b38d1c02f1f6d7c052d5de1070` on
`feature/storage-root-ownership`. It has been integrated with freshly fetched
`origin/main` `b4644b7ab33d58bf44de159b119cf5ae5ff8ea9e` by ordinary merge
`fadcd1bde19a6751b3a72448e27ebe2518002774`; merge base was
`34b85134e11931efc2faff972bcb2d1110154153`. No publication occurred.

Historical task setup: the initial supplied baseline was
`29f7179fa54b7eea8b264bfa88cae4ec6053b37b`; this task branch was created there.
The package-boundary correction was recorded as `420b31c`, and main
`34b85134e11931efc2faff972bcb2d1110154153` was integrated before the current
review. The earlier candidate `4dd472075eed82f030efb0b39923cbf2416502cf` was not
rewritten. These history milestones are not completion/readiness evidence.
Old layout branches remain untouched and are not imported.

Implemented structure: `pkg/hfdownloader/root_domain.go` (1076 lines in this
candidate) contains the root domain; `internal/server/storage_roots.go` (261
lines) is the configuration/presentation adapter. Stable lexical identity,
role grouping, owned walking, copy/projection descriptors and primary-Hub
eligibility are present. Their presence does not establish complete physical
ancestry or protection of every destructive effect. #95 layout policy and frozen
job associations remain the foundation, not a new implementation phase.

Findings on immutable pre-correction HEAD (addressed in `957498e`; preserved
by parent-reported current C1-C14 independent Linux verification):

- **F1 high:** `containmentDistance` walks lexical ancestors of an externally
  configured alias, missing its real parent inside a destructive target.
  `WholeCopyAllowed` and `NestedProtectedRoots` therefore accept an insufficient
  negative observation as noncontainment.
- **F2 high:** legacy deletion consulted the domain only for the Hub target, then
  `safeDeleteFriendlyPath` recursively removed the secondary friendly subtree
  without configured-root protection. Independent production-runtime evidence
  confirms a configured friendly descendant and its sentinel were deleted.
- **Follow-up confirmed during correction:** the existing friendly helper checked
  the leaf and final resolved containment only. A real directory reached through
  a symlinked owner/intermediate projection component that resolved elsewhere
  inside the cache passed those checks; `RemoveAll` on the original spelling
  followed that component and could remove a foreign friendly repo after Hub
  removal. The corrected fixed-effect preflight uses one owner-level component
  validator, also reused by the existing cleanup call, before the first removal.
  Parsed Hub repository type now chooses the model/dataset projection role;
  repository-name substrings no longer select the role.

The F1/F2 shared correction is implemented in `pkg/hfdownloader/root_domain.go` and
`internal/server/storage_roots.go`, with the handler using one fixed legacy-HF
preflight before its first removal. The physical resolver preserves missing
suffixes beneath resolved aliases without changing configured IDs. Owner
discovery treats a symlinked leaf lexically to preserve friendly/local alias
ownership while still resolving aliased parent components. Projection-parent
protection is exempted only for the associated friendly effect; other protected
roots intersecting that effect block the whole operation.

Regression tests committed with the correction cover preservation cases,
including aliased parent with normal child, lexical nested-Hub refusal,
case-distinct IDs, captured base, source priority and frozen jobs. They also cover
inverse aliases/missing descendants, model and dataset friendly-root refusal
before either removal, symlinked friendly owner/projection components with
sentinels preserved, and a valid model ID containing `datasets--`. These are
implementation-local evidence. Parent reports corresponding current independent
production verification passed; this is not full phase-1 gate completion.

## Current blocker: F3

For T's child C exposed externally as protected bind-mounted S, EvalSymlinks
does not change S's namespace. `containmentDistance` (`root_domain.go:286-326`)
can return false/nil in both directions; `checkProtectedEffect:757-774` permits
the effect. `NestedProtectedRoots:408-442` also misses S, while owned walking at
C can match it by SameFile. The handler's existing complete preflight at
`api.go:1706` therefore still permits unsafe Hub/friendly recursion.

The read-only source inspection of Go 1.24.0 `os/removeall_at.go` establishes
that Unix removal opens and recurses through directory entries without a
mount-boundary exclusion: mounted directories are not symlink leaves. It may
remove contents before an EBUSY directory-removal error. Negative resolved-name
ancestor searches are consequently not proof of the intended deletion footprint.
This is ordinary filesystem namespace behavior, not a hostile race/transaction
claim. The correction model, C15-C23 cases and bounded assignments are in
`reviews/mount-proof-reassessment.md`; the earlier resolved-ancestor hypothesis
is explicitly superseded. No F3 code change has been made here.

Independent earlier evidence remains historical and does not cover this candidate:

- Reviewer `ses_ef4002372ffeSM2Ql95Bv3WFue`: full 16-file coverage,
  **changes required**, no external OCR (parent-supplied report).
- Tester `ses_ef400236dffeU2OdwGF8QCQJt3`: candidate-bound domain and
  domain/server race suites passed; production runtime returned HTTP 200
  `success:true` for F2 and deleted both Hub and the protected friendly marker.
  The direct nested-Hub control returned 400 and preserved its marker.
- Retained evidence: `/tmp/opencode/issue63-verify-Bqb8T7/`, including
  `verification-metadata.json`, `reproduction.txt` and
  `repro-legacy-friendly-root.json`, inspected during reassessment. Runtime
  processes, binary and fixture directories were cleaned by the tester;
  evidence and a stopped-fixture browser tab were intentionally retained.
- Executing toolchain was Linux Go 1.26.7, not project Go 1.24. Earlier recorded
  full tests/vet/build/cross-build passes are historical implementation-local
  evidence, not independent proof of F1/F2 or native Windows/macOS behavior.

Windows CI selects existing tests spanning the C1-C14 matrix, including
captured-base, friendly-priority, frozen destinations, F1/F2, aliased-component
refusal, valid-name and read-only manifest cases, in addition to prior ownership
and state-replacement coverage. There is no existing focused C10 injected
secondary-cleanup-I/O-failure test; this selector does not claim that case.
Several alias/case/reparse tests may skip when Windows filesystem capabilities
are unavailable; skips remain visible and are not weakened. The selection and
local Linux run do not prove native Windows/Go 1.24 execution; GitHub CI results
remain pending.

Fresh main `b4644b7ab33d58bf44de159b119cf5ae5ff8ea9e` is integrated. Relative to
reviewed base, upstream changed only Docker/runtime/docs/CI paths; no application
source or root guidance changed. The merge retained those upstream changes and
the task-branch source correction. No test here claims native Windows or Go 1.24
execution against the integrated candidate.

## Earlier local implementation evidence (historical)

- Regressions first failed: inverse alias descendant was omitted by
  `NestedProtectedRoots`; real model and dataset DELETE requests returned 200
  and removed the protected friendly subtree.
- Green after correction: `go test ./pkg/hfdownloader -run
  'TestManagedRoot(NestedProtectedFindsInverseAliasAndMissingDescendant|OwnerWalkUsesFreshPhysicalNestedFacts)' -count=1`;
  `go test ./internal/server -run
  'TestAPI_CacheDelete_ProtectsNestedFriendlyRootsBeforeEitherRemoval' -count=1`.
- Preserved targeted checks passed for local/Hub/projection alias grouping,
  external-H deletion, aliased nested ownership, and both model/dataset inverse
  alias/friendly protected-root HTTP cases, symlinked friendly owner/projection
  refusal with Hub and foreign markers intact, and model deletion for
  `owner/datasets--model`.
- The friendly alias regression explicitly asserts refusal came from the shared
  component validator, not an earlier unrelated handler rejection.
- Full Linux Go 1.26.7 validation passed in isolated HOME/XDG/APPDATA/
  LOCALAPPDATA/HF/TMP: `go test ./... -count=1`, `go test ./... -race -count=1`,
  `go vet ./...`, and `go build -o <scratch>/hfdesk ./cmd/hfdesk`.
- On integrated merge `fadcd1b`, both native-Windows storage selector commands
  from `.github/workflows/ci.yml` passed locally on Linux Go 1.26.7 with
  `-count=1 -v`; `go test -list` confirmed the named preservation/F1/F2 tests
  are selected. This is selector/Linux evidence only, not Go 1.24 or native
  Windows evidence. No selected tests skipped on this Linux run.
- No Go 1.24 executable or installation was available; Go 1.24 compatibility
  remains unverified. Native Windows/macOS filesystem behavior is unverified.
- At the time of those implementation-local runs, independent stages were still
  pending. They have since run on `7dc62163` per the parent report above: C1-C14
  passed, but full review requires changes for F3. Do not retain the old pending
  statement as current status or treat those passes as mount-proof evidence.

## Reassessment evidence and next action

The earlier planning-only reassessment inspected the clean `7dc62163` worktree,
existing domain/preflight, canonical artifacts, CI selectors and Go 1.24.0
removal/Windows metadata source. It did not run tests or mounts. The parent later
added the native Linux CI selector/job; its execution and native Windows/macOS
results remain unestablished.

The source correction now exists in the dirty worktree described below. The
source branch, HEAD, #95 policy, Issue #76 and unpublished branches remain
unchanged. No phase-2/3 mechanism or new filesystem-support policy is authorized.

## F3 correction — implementation-local evidence

`pkg/hfdownloader/root_effect.go` adds a private namespace-observation seam and
an ephemeral bounded directory-edge walk. Each actual opened directory's
`FileInfo` is identity-checked; entries are read in batches and only real
directories recurse. Symlink leaves do not become content edges. Each namespace
occurrence is traversed even when its directory identity appeared elsewhere;
active-stack cycles, unreadable/partial entries, invalid identity, excessive
depth/work, and close/open failures return unknown and refuse. The walk retains
at most 100,000 directory/entry observations, at most 256 open recursion levels,
at most 100,000 pairwise identity comparisons, and 128 entries per read call. It reads
metadata only, not model bytes. No context is available at the existing domain
preflight API; bounds and prompt descriptor closure are the applicable stop
controls. It provides no hostile-writer atomicity guarantee.

`checkProtectedEffect` preserves existing positive name/ancestor blockers but
does not use negative ancestor results as permission. It compares reached
directory identities against each configured protected existing directory or
the deepest verified existing anchor of an ordinary missing reservation. The
authorizing-root ID cannot suppress a graph hit. `NestedProtectedRoots` uses the
same graph facts for existing roots and retains prior missing-root behavior.
Both legacy effects still complete admission before the handler's first removal.

Static semantic correspondence was checked against Go 1.24.0 sources:
`removeall_at.go` first tries unlink; for directories it opens relative entries
with `O_DIRECTORY|O_NOFOLLOW`, reads names and recurses, without a mount/device
boundary check. Thus a bind-mounted directory is an actual recursive edge, while
a symlink leaf is unlinked rather than traversed; final `AT_REMOVEDIR`/EBUSY can
occur after content deletion. `removeall_noat.go` recurses after `Lstat` reports
a directory. Go 1.24 Windows `types_windows.go` treats name-surrogate reparse
points (including junctions) without `ModeDir`; `SameFile` identity loading can
return false on failure. The implementation uses opened-directory `Stat` facts
for comparison and refuses failed observations. This is static source reasoning,
not Go 1.24 execution or native Windows evidence.

Added portable model regressions establish that the fixture itself defeats both
directions of the previous ancestor search, then cover a protected object reached
under another parent, a missing reservation anchor, the authorizing root
re-exposed in the effect, repeated same-object namespace views with distinct
children, active cycles, partial reads, unreadable entry identity and exhausted
depth. Linux
`TestNativeMountProtectedRootReachability` uses only an isolated child mount
namespace and disposable source/target directories; it tests Hub/friendly model
and dataset cases, unmounts before cleanup, and honors
`HFDESK_REQUIRE_NATIVE_MOUNTS=1` by failing rather than skipping setup problems.
Local run skipped because `unshare(CLONE_NEWNS)` returned `operation not
permitted`; no fixture mount was created. The parent-added Go 1.24 Ubuntu CI job
is configured but has not executed. There is no claim of native mount pass.

Earlier implementation-local validation on Linux Go 1.26.7 passed focused F3/domain and
server tests, `go test ./... -count=1`, `go test ./... -race -count=1`,
`go vet ./...`, and `go build -o <scratch>/hfdesk ./cmd/hfdesk` with isolated
HOME/XDG/APPDATA/LOCALAPPDATA/HF/TMP; the downloader test binary also cross-built
for Windows/amd64. Temporary validation scratch was
cleaned after removing Go module-cache read-only permissions within that owned
directory. Independent tester/review, Go 1.24 native-mount CI, and native
Windows/macOS remain pending. This work does not complete Phase 1 or authorize
Phase 2/publication.

## Final local correction and evidence boundary

On source baseline HEAD `9498fc344a308f26dd02b5c9bbcebb2380ccdf93`, the dirty
worktree includes three narrow follow-ups to the bounded observer and native
fixture:

- **F4:** a successful short `ReadDir` batch is not exhaustion. Continue until
  explicit EOF; reject zero-entry success without EOF and any non-EOF error.
  Tests cover a short batch hiding a later protected entry, empty non-EOF, and
  short batch followed by EOF.
- **F5:** distinguish an actually existing protected root from the existing
  anchor of a missing reservation. Only the former can be skipped as the root
  itself during `NestedProtectedRoots`; a missing reservation beneath a root
  alias remains represented. A focused regression covers it.
- **F6:** start the native test subprocess with `CLONE_NEWNS` before Go runtime
  startup and compare `/proc/self/ns/mnt` against `/proc/<actual-parent>/ns/mnt`.
  The environment marker alone cannot bypass isolation; a negative regression
  verifies rejection before mount setup. The CI selector now requires the exact
  positive bind-mount test as well as running the selected native cases.

Final implementation-local evidence: changed Go files formatted; focused package
tests, package race tests, full `go test ./...`, full `go test ./... -race`, vet,
build, Windows cross-build, and diff checks passed on Linux Go 1.26.7. Ordinary
native tests skip because this host denies private mount namespaces; required
mode correctly fails on that capability denial and is not a mount pass.
Independent tester/review, actual Go 1.24 private-namespace CI, native Windows
and macOS evidence remain pending. Phase 1 is not complete.
