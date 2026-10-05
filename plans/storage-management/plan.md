# HFDesk storage management

Canonical foundation: merged PR #95 on `main` (HF_HUB_CACHE wins over an explicit cacheDir). Issue #76 remains closed. PR #70 is superseded evidence only; its implementation and unpublished layout candidates are not a baseline.

Current phase: **1, implementation-local correction complete; independent verification/review pending**. Task
branch remains `feature/storage-root-ownership` at HEAD
`9accc19d11b15e9896191e2c963b8f449d928116` with local implementation changes on
the authorized worktree, based on the reviewed base
`34b85134e11931efc2faff972bcb2d1110154153`. Full independent review requires
changes; independent production-runtime verification confirmed configured-root
data loss. Local F1/F2 corrections and regressions now pass, but independent
verification/review and native platform evidence remain outstanding.

The workflow owner supplied fresh upstream
`b4644b7ab33d58bf44de159b119cf5ae5ff8ea9e`; its intervening changes are
Docker/runtime/docs/CI, not application cache/jobs/path/state or project guidance.
The task candidate has not incorporated that upstream. Integration and renewed
candidate evidence belong to the parent/implementation stage, not this plan edit.

This plan covers the authorized Issue #63 replacement in four bounded phases:

1. Root/path ownership and eligibility (Issue #63, this branch).
2. Repository coordination and managed HF state: native shared/exclusive leases, short metadata lock, RepoState outside hub, and shared extracted #80 atomic writer/confidence reconciliation.
3. Reconciled inventory and deletion engine: strict model GGUF quant identity across shards/revisions, immutable durable plans, forward recovery, conservative GC, and safe whole-copy behavior; no UI.
4. Re-enumerated opaque copy/artifact selectors, conditional requests, typed DTOs, safe UI, and docs; Fixes #63.

Phases 2–4 are future work and are not part of this implementation package. Do not broaden phase 1 into transactional deletion or UI/API contract changes.

Phase 1 must protect configured roots across the **complete intended legacy
whole-HF destructive effect set**, including secondary friendly cleanup, before
any removal. Configured-definition ancestry, observed physical ancestry and
effect eligibility are separate claims. Unestablished physical noncontainment
is unknown, not deletion permission. The bounded correction and verification
model is in [phase 1](phases/phase-1.md) and the
[reassessment](reviews/protected-roots-reassessment.md).

Next safe action: parent reconciles the local implementation and then performs
the planned integration/CI and independent verification/review stages. Phase 2
cannot use phase 1 as a completed foundation until those gates succeed. No
publication was performed by this implementation.
