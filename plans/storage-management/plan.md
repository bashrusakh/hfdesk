# HFDesk storage management

Canonical foundation: merged PR #95 on `main` (HF_HUB_CACHE wins over an explicit cacheDir). Issue #76 remains closed. PR #70 is superseded evidence only; its implementation and unpublished layout candidates are not a baseline.

Current phase: **1; corrections remain incomplete pending independent/native
evidence**. Historical source snapshot `9498fc344a308f26dd02b5c9bbcebb2380ccdf93`
and its dirty-worktree corrections are evidence only, not the current checkout.
The current clean candidate at the start of this corrective stage was
`8ee926f629a94f21b6e1127c7a9daaba846d8ec3` on `feature/storage-root-ownership`,
based on `b4644b7ab33d58bf44de159b119cf5ae5ff8ea9e` (also the merge base). It
received a full review verdict of pass with notes and a fresh tester checkpoint
covering 27 production HTTP scenarios; the portable model-fixture correspondence
gap described in the implementation record remains under correction. Those
earlier results do not cover the corrected tests/docs state or establish native
mount behavior. The current focused test and evidence-record correction is
uncommitted; its exact candidate and local test results must be recorded after
validation. Independent review/test of that resulting candidate and native CI
remain pending.

Earlier independent Linux production evidence covered 27 HTTP scenarios on
`8ee926f`; separate earlier parent-reported C1-C14/40-scenario evidence applies
to a prior candidate only. Neither substitutes for current portable model checks
or native mount execution. Go 1.26.7 local evidence is implementation-local only;
Go 1.24 native-mount CI and native Windows/macOS evidence remain pending.

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

Next safe action: run the integrated independent checkpoint and full stable-diff
review against the committed candidate. Execute the
required Go 1.24 private-namespace mount CI job before any Ready decision. Phase 2
cannot use phase 1 as a completed foundation until these gates pass.
