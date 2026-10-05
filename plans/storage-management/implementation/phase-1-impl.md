# Phase 1 implementation record

Status: **implementation-local correction complete; independent verification and review pending**. Base
candidate was `9accc19d11b15e9896191e2c963b8f449d928116` on
`feature/storage-root-ownership`, reviewed against
`34b85134e11931efc2faff972bcb2d1110154153`. The worktree was clean before this
plan-only reassessment. Source/test changes are now present as an uncommitted
local candidate; no publication occurred.

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

Blocking findings on immutable baseline HEAD before the local correction:

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

Baseline regressions cover useful preservation cases, including aliased parent
with normal child, lexical nested-Hub refusal, case-distinct IDs, captured base,
source priority and frozen jobs. New dirty-worktree tests cover inverse aliases,
the complete Hub/friendly effect preflight, symlinked friendly owner/projection
components, and a valid model ID containing `datasets--`.

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

Candidate Windows CI has ownership steps but execution is pending. The server
selector omits `TestCacheAndRoutePathsUseCapturedConfigBase` and
`TestFindLocalCachedRepoPreservesFriendlySourcePriority`; several existing tests
skip Windows/symlink capabilities. CI selection is not executed coverage.
The bounded DevOps assignment must include named preservation/new protection
tests and report what actually ran/skipped on native Windows/Go 1.24.

Fresh main is `b4644b7ab33d58bf44de159b119cf5ae5ff8ea9e` per parent fetch/host
identity, also present in local `origin/main`. Compared with reviewed base,
only Docker/runtime/docs/CI paths changed; no application source or root guidance
changed. This candidate was not merged with it during reassessment. Parent must
establish the next base before correction/CI reconciliation; no test here claims
execution against fresh upstream.

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
- No Go 1.24 executable or installation was available; Go 1.24 compatibility
  remains unverified. Native Windows/macOS filesystem behavior is unverified.
- No independent tester/reviewer stage was performed; their pending evidence
  must bind to the post-integration candidate.
