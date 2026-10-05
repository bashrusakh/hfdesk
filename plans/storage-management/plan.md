# HFDesk storage management

Canonical foundation: merged PR #95 on `main` (HF_HUB_CACHE wins over an explicit cacheDir). Issue #76 remains closed. PR #70 is superseded evidence only; its implementation and unpublished layout candidates are not a baseline.

This plan covers the authorized Issue #63 replacement in four bounded phases:

1. Root/path ownership and eligibility (Issue #63, this branch).
2. Repository coordination and managed HF state: native shared/exclusive leases, short metadata lock, RepoState outside hub, and shared extracted #80 atomic writer/confidence reconciliation.
3. Reconciled inventory and deletion engine: strict model GGUF quant identity across shards/revisions, immutable durable plans, forward recovery, conservative GC, and safe whole-copy behavior; no UI.
4. Re-enumerated opaque copy/artifact selectors, conditional requests, typed DTOs, safe UI, and docs; Fixes #63.

Phases 2–4 are future work and are not part of this implementation package. Do not broaden phase 1 into transactional deletion or UI/API contract changes.
