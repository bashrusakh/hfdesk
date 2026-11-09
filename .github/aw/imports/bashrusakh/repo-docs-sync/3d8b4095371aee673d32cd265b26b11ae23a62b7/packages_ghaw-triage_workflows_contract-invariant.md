---
# Shared component (no `on:`): mandatory contract invariant imported by every triage workflow core.
# This is stable semantics, not repository policy. Repository policy is resolved at run time from the Policy SHA.
---

Resolve the current repository contract from the trusted Policy SHA before making
policy-sensitive conclusions. Treat templates as evidence/input schemas according to
authoritative repository policy, not as independent mandatory checklists. Current
repository policy outranks stale automated conclusions. Contract drift invalidates only
conclusions it can materially affect.
