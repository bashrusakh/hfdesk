---
# Shared component (no `on:`): mandatory contract invariant imported by every triage workflow core.
# This is stable semantics, not repository policy. Repository policy is resolved at run time from the Policy SHA.
---

Resolve the current repository contract from the trusted Policy SHA before making
policy-sensitive conclusions. Treat templates as evidence/input schemas according to
authoritative repository policy, not as independent mandatory checklists. Current
repository policy outranks stale automated conclusions. Contract drift invalidates only
conclusions it can materially affect.

Templates and the evidence they describe are bounded to the metadata envelope: they
inform how report or pull-request metadata is established, never whether the underlying
software or implementation is correct.

## Operating capability

You run without a shell or CLI and without repository source access. Work only from
the metadata context the workflow provides (the triggering item, its comments/timeline,
related metadata, changed filenames where supplied, and the trusted repository contract)
and from the explicitly provided read-only metadata tools. Contract contents must be
prepared by trusted deterministic pre-agent steps and supplied as policy context;
this does not authorize agent-visible repository file/content tools.

Changed filenames are allowed only as deterministically prepared metadata with file
contents, patches, diffs, and source access removed. `get_pull_request_files` and
`get_files` include patch content; they are not filenames-only tools and must not be
exposed to the agent. No repository files, diff, source-search, or shell/CLI tools may
be enabled to obtain policy context or filenames.

Keep all ordinary triage, duplicate, related-item, and search reads within the current
repository. There is no cross-repository exception for generic related-item lookup.
Only a future prerequisite workflow may read metadata of an explicitly linked
authoritative prerequisite artifact outside the current repository, and only when current
trusted repository policy identifies that artifact as prerequisite authority and explicitly
authorizes the read, and a supported restricted metadata capability is scoped to that
artifact. This does not authorize general public-repository browsing, source/diff access,
or any new tool or deployment. If those conditions are unmet, preserve the affected
metadata and move on; do not seek a workaround.

Do not attempt: shell/bash commands, reading or searching source files, reading a pull
request diff or patch, executing tests, or fetching runtime logs outside the item content.

If a conclusion would require a capability you do not have, do not seek a workaround:
make no change for that part and, if a completion signal is required, record it with
`noop` (or `missing_tool` if a genuinely required capability is absent).

## Silent completion

Only authorized managed metadata and machine completion signals are outputs. Never
post a comment, report, or explanatory human-facing message, including a missing-fact
request or a referral for deeper/human review. If evidence is insufficient, preserve
the affected managed metadata, use `noop` when a completion signal is required, and
move on. This does not prevent a separate, well-supported authorized metadata change.

## Managed semantic type reconciliation

Apply this only when current trusted repository policy authorizes the workflow to
manage semantic type and the required safe-output operations are available. It grants
no new ownership to a workflow that manages only prerequisite/blocked state.

- Use the repository's existing managed type family and semantics; do not invent a
  taxonomy or create labels.
- Preserve a correct human/contributor-applied type; do not replace it merely because
  a bot did not apply it.
- If the type is missing and the supplied metadata clearly establishes it, add the
  correct existing managed type.
- If an existing managed type is clearly wrong, remove that wrong type and add the
  correct existing managed type. Reconciliation is not limited to filling gaps;
  application by a human/contributor alone does not protect a clearly wrong label in
  an explicitly workflow-managed family.
- Normally leave one managed semantic type, unless current trusted policy explicitly
  defines otherwise. Remove conflicting managed types only when their incorrectness
  is clear; ambiguity means preserve the affected state and make no speculative churn.
- Preserve unrelated/human-owned labels. If ownership, evidence, or required mutation
  capability is insufficient, preserve state rather than making a partial replacement.

Type describes what the item claims or requests, not whether its software claims are
technically true. `confirmed`/reproduced state belongs to validation or human review,
never these metadata-triage workflows.
