# Protected-root / complete-effect reassessment

Date: 2026-10-06. Status: **changes required; model handoff only**.
Inspected source: `feature/storage-root-ownership` at
`9accc19d11b15e9896191e2c963b8f449d928116`; reviewed base
`34b85134e11931efc2faff972bcb2d1110154153`.
Fresh upstream `b4644b7ab33d58bf44de159b119cf5ae5ff8ea9e` is not the executing
candidate. No source/test/CI/branch change, dispatch, commit or publication was
performed by this reassessment. The previous interrupted invocation did not
complete or mutate the worktree.

## Goal and scope

Re-establish phase 1's configured-root protection across existing legacy
whole-HF removal effects, not invent a new deletion unit or phase-3 engine.
Authority is the user's current protected-root invariant, not implementation
comments or passing tests. #95 ENV precedence, read-only alias browsing,
external H, complete frozen jobs and Issue #76 closure remain unchanged.

## Source trace and counterexamples

**F1 — high, inverse alias ancestry.** `root_domain.go:286–316` first uses
configured lexical containment, then Stats the proposed parent and walks
`filepath.Dir` ancestors of the target spelling. For real effect
`T=/store/hub/models--owner--model` and protected
`S=/configured-alias -> T/application-data`, `Stat(S)` reaches the child but
`Dir(S)` visits `/`, not T. No match is found; the function returns false/nil.
`NestedProtectedRoots:357` misses S; `WholeCopyAllowed:609–622` misses both
ancestry and equality (S is a child, not T) and permits deletion. The existing
aliased-parent/normal-child fixtures exercise the opposite direction.

**F2 — high, incomplete effect-set validation.** `api.go:1707` validates only
Hub T. `1730` removes T; `1737` calls `safeDeleteFriendlyPath`; its prefix/leaf/
resolved-path checks (`1759–1785`) never consult configured-root protection;
`1787` removes the complete friendly subtree F. A root at `F/routed` is outside
T but inside the second destructive effect. HTTP 200 and deletion of both Hub
and friendly sentinel were confirmed by the independent disposable production
run, not merely a synthetic helper test. The recursive friendly mechanism is
unchanged from base: classify this as a missed task invariant, not automatically
a newly introduced regression.

Evidence: `/tmp/opencode/issue63-verify-Bqb8T7/verification-metadata.json`,
`reproduction.txt`, `repro-legacy-friendly-root.json`. Reviewer and tester
identities/evidence limits are recorded in `implementation/phase-1-impl.md`.

## Reframing: one shared boundary

Configured ancestry is a reservation/definition claim. Physical ancestry is a
claim about observed objects and their actual namespace parents. Eligibility is
a claim about **all intended effects**. Endpoint validation, matching repo name,
friendly role, `SameFile` equality, and a failed ancestor search are not mutually
equivalent to these outcomes. Keep the distinctions explicit in the existing
`ManagedRootSet`; no second owner or generic effect framework is needed.

### Options compared

| Option | Counterexample / tradeoff | Decision |
|---|---|---|
| Keep lexical-parent Stats; add inverse-alias and friendly handler guards | Misses other alias directions/missing suffixes; duplicates protection rules | Reject |
| Walk every destructive subtree comparing directory identities | Can find positive existing descendants, but absent/inaccessible entries and missing configured children still need a separate relation; costly for caches | Not the primary solution |
| Refuse every alias or absent configured root globally | Safe by overblocking but breaks safe external aliases, default absent friendly namespaces and normal deletion | Reject global policy; unknown-specific refusal remains required |
| Resolve physical observations, validate objects, compare resolved ancestors with SameFile; share complete legacy effect eligibility | Handles both alias directions while keeping lexical IDs and destructive trust unchanged; unresolved capabilities stay unknown | Chosen bounded approach, subject to semantic regression evidence |

### Physical-observation requirements

- Resolve both existing sides for observation, never rewrite configured IDs.
  Verify observed raw/resolved objects correspond with Stat/SameFile; compare
  actual resolved ancestor objects, not only canonical string prefixes.
- A positive lexical reservation can conservatively block an effect even when
  it is not physical proof. Negative permission requires sufficient observations.
- For `S=/alias/new-child` with `/alias -> T/application-data`, resolve the
  existing alias/prefix and retain the ordinary missing suffix as a reserved
  location. Reject T; do not invent a child inode or silently drop S.
- Distinguish verified ordinary absence from dangling links, loops, permissions,
  ENOTDIR, incomplete resolution and unsupported reparse/namespace semantics.
  Those are unknown and cannot authorize mutation. A safely observed unrelated
  missing reservation or absent optional friendly effect must not block every
  normal delete. Shared directory ancestors alone do not prove intersection.
- Case/drive/volume spelling and different device IDs do not prove physical
  noncontainment (mounted volumes and aliases are counterexamples). If available
  observations cannot justify the inference on a supported platform, return
  unknown rather than asserting outside. Native Windows evidence is required;
  do not add broad platform assumptions or a speculative filesystem framework.
- Captured absolute bases avoid cwd reinterpretation. IDs/definitions remain
  stable when paths appear or observations change. Reuse one relation operation
  in owner selection, nested protection, walking and mutation eligibility.

These are proof obligations, not a claim that EvalSymlinks alone solves all
platform ancestry. Stop/escalate if satisfying them needs new material domain
semantics or native capabilities beyond the bounded phase-1 correction.

### Fixed legacy-HF preflight

The common owner evaluates the existing primary Hub target and optional
secondary friendly target derived from the same captured #95 association.
Hub uses physical-copy shape/ownership rules. Friendly uses the associated
projection namespace, exact owner/name shape, destructive confinement and the
same protected-root boundary test; it never becomes an independent copy.
Do not route it through physical-copy eligibility that always rejects projection
roles, and do not leave recursive cleanup as a policy bypass.

If either intended effect intersects protected roots or has unknown relevant
evidence, refuse before **any** removal, preserving both trees. Safely absent
friendly paths contribute no removal effect. After allowed preflight, ordinary
secondary I/O failure retains #95's visible partial-failure behavior. Revalidate
using the existing owner where appropriate; do not claim atomicity against
config/filesystem changes after observation, rollback or hostile-writer safety.
No lease/journal/quarantine/transaction is authorized. Matching owner/name
selects the legacy path only; future phase 3 still needs proven-link inventory.

## Bounded correction assignments (parent dispatch only)

### Debugger: integrated ancestry and effects

Start from the parent-established task branch/base after any safe main
integration. Consume the current findings on `34b85134..9accc19`; do not restart
from unpublished layout branches or PR70. Allowed edits are the existing domain,
server adapter/legacy integration and focused tests, plus authorized canonical
implementation evidence. No new API/UI or later-phase mechanism.

1. Establish failing phase-1 C1/C2/C4 tests, including real handler requests and
   sentinel checks for **both** trees; helper success alone is insufficient.
2. Correct/consolidate physical relation evidence inside ManagedRootSet; preserve
   configured IDs and all materially affected owner/walker consumers.
3. Consolidate one fixed legacy-HF effect preflight under that owner and call it
   before Hub RemoveAll. Keep handlers as adapters and existing execution/error
   paths; do not add one local guard per review comment.
4. Verify the changed and preserved cases in the phase matrix, then focused/full
   tests, race, vet, build and project Go 1.24 compatibility. Report exact
   base/head/dirty state, case results, unsupported facts and platform gaps.

These are two coherent correction batches (relation; effect integration) under
one model, not separate tester ceremonies. Return any required material new
authority/protocol/engine work to parent. No independent publication.

### DevOps: separate bounded coverage adjustment

After regression names stabilize, reconcile `.github/workflows/ci.yml` with the
parent-integrated Docker CI changes. Extend the native Windows storage/server
selectors to cover C1–C14 where executable, explicitly including
`TestCacheAndRoutePathsUseCapturedConfigBase` and
`TestFindLocalCachedRepoPreservesFriendlySourcePriority` (currently omitted).
Preserve existing writer tests and Go 1.24 selection. Verify selectors against
discovered test names; report actual execution/skips and native capability
failures. Do not weaken Windows skips/assertions or redesign unrelated CI.
An unexecuted selector or cross-build is not native runtime evidence.

## Independent checkpoint and completion gate

One tester assignment covers the corrected shared relation plus complete legacy
effect boundary, with the full C1–C14 changed/preserved map. Require a disposable
production/API F2 reproduction and F1 inverse-alias case with both trees intact
on refusal, not another old-suite-only pass. Then require full independent
review of the stable whole diff. Changed base/head stales affected evidence;
pending native/remote checks remain pending. No readiness/publication claim is
made by these artifacts. Phase 2 remains blocked on this foundation.
