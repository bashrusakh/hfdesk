# HFDesk storage management

Canonical foundation: merged PR #95 on `main` (HF_HUB_CACHE wins over an explicit cacheDir). Issue #76 remains closed. PR #70 is superseded evidence only; its implementation and unpublished layout candidates are not a baseline.

Current phase: **1, F3 correction implemented locally; independent/native evidence pending**.
Implementation worktree is based on `7dc62163e9ac5dffc5b126fcd18ce8ec7c212fee`
on `feature/storage-root-ownership`; base and merge base are
`b4644b7ab33d58bf44de159b119cf5ae5ff8ea9e`. The worktree is dirty with the
authorized CI/planner changes and this source/test correction; no commit or
publication occurred.

The workflow owner reports 40 independent production scenarios passing the
Linux C1-C14 boundary, including the safe C10 secondary permission failure.
Preserve those corrections. The former clean candidate's full review required
changes because F3 showed ordinary bind-mount aliases invalidate resolved-name
ancestor searches as proof of physical noncontainment. The correction now matches
protected directory objects/verified missing anchors against the complete bounded
recursive effect graph. Local Go 1.26.7 evidence is implementation-local only;
native Go 1.24 mount CI, independent verification/review, Windows and macOS
evidence remain pending.

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
model is in [phase 1](phases/phase-1.md) and the current
[namespace-proof reassessment](reviews/mount-proof-reassessment.md).
The earlier [symlink/effect-set reassessment](reviews/protected-roots-reassessment.md)
is historical; its resolved-ancestor negative-proof hypothesis is superseded.

Next safe action: reconcile the final local source/test/CI/planner diff, then run
the integrated independent checkpoint and full stable-diff review. Execute the
required Go 1.24 private-namespace mount CI job before any Ready decision. Phase 2
cannot use phase 1 as a completed foundation until these gates pass.
