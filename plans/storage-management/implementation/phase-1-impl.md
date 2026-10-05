# Phase 1 implementation record

Status: **source correction and upstream integration complete; independent verification and review pending**.
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

Implemented structure: `pkg/hfdownloader/root_domain.go` (926 lines in this
candidate) contains the root domain; `internal/server/storage_roots.go` (254
lines) is the configuration/presentation adapter. Stable lexical identity,
role grouping, owned walking, copy/projection descriptors and primary-Hub
eligibility are present. Their presence does not establish complete physical
ancestry or protection of every destructive effect. #95 layout policy and frozen
job associations remain the foundation, not a new implementation phase.

Findings on immutable pre-correction HEAD (addressed in `957498e`; independent
verification remains pending):

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

The shared correction is implemented in `pkg/hfdownloader/root_domain.go` and
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
implementation-local evidence, not independent gate completion.

Independent prior evidence remains historical and does not cover this candidate:

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

## Local implementation evidence

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
- No independent tester/reviewer stage was performed; their pending evidence
  must bind to the post-integration candidate.
