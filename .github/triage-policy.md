# Triage Automation Policy

Scope: labels and semantic typing applied automatically to hfdesk issues and pull
requests by the repository's triage automation. This file governs automation only;
it does not replace `CONTRIBUTING.md`. See "Authoritative sources" below.

## Managed label families

The automation owns these label families and may add or remove only these labels:

- Issue triage: `bug`, `enhancement`, `documentation`, `question`, `refactor`, `ci`, `needs-info`, `duplicate`
- PR semantic intake: `bug`, `enhancement`, `documentation`, `question`, `duplicate`, `refactor`, `ci`

Allowed semantic issue/change types are limited to `bug`, `enhancement`,
`documentation`, `question`, `refactor`, and `ci`. No area, component, risk, or
priority taxonomy is authorized.

The semantic **type** family is add-and-remove owned for the workflows that manage it:
the automation preserves a correct contributor-applied type, fills a type that is clearly
missing, and replaces a clearly incorrect managed type by removing the wrong type and
adding the correct one. Ambiguity means preserve the affected state; there is no churn.

`confirmed` is **not** automation-owned: it is human/verification-owned and is applied by
a human only after reproduction or validation. No triage workflow may add or remove it.

`needs-info` means metadata/routing information is missing from the report; it never means
that implementation proof, reproduction steps, or a fix are missing.

Contributor-first labeling: `CONTRIBUTING.md` is the source of truth for triage
labels. Contributors apply the type label first; the automation verifies the label, fills
a clearly missing one, and corrects a clearly wrong managed type. It must not relabel a
correct human label.

## Reserved labels (human-owned)

The automation must not add or remove any of the following:

- `priority-*`
- `codex-*`
- `approved-for-fix`
- `codex-fixing`
- `ready-for-human-review`
- `invalid`
- `wontfix`
- `good first issue`
- `help wanted`
- anything not listed under "Managed label families"

Priority is maintainer-owned. The automation must never set, infer, or change
priority.

## No prerequisite gate

There is no prerequisite, approval, or decision-gate concept authorized for
automation in hfdesk. The automation must not infer or mark a blocked/prerequisite
state, so a PR-prerequisite workflow has no applicable job here.

## Templates and evidence

Templates (`PULL_REQUEST_TEMPLATE.md`, `ISSUE_TEMPLATE/*`) are input schemas and
evidence, not retroactive mandatory checklists. A missing or unfilled template
field is not by itself grounds for `needs-info`; label only on the substance of the
report.

A report is sufficiently clear for metadata triage when its title/body establish the
semantic type, what is broken or the affected user-facing area, and the reported
behavior. A report that establishes neither the affected area nor the reported behavior
— for example "doesn't work" or "something is broken" with no further substance — lacks
the information needed for metadata routing, so `needs-info` applies even when the
semantic type is inferable.

## Metadata-only scope

Triage is metadata-only — the automation does not reproduce, validate, review code, read
source/diff, or comment; `needs-info` means metadata/routing information is missing.

## Policy precedence and drift

The current Policy SHA outranks stale automated conclusions. If the contract
changes, earlier automation conclusions are invalidated only where the change
materially affects them; otherwise previously derived labels may stand.

## Authoritative sources

The authoritative triage sources are, in order of relevance to a given conclusion:
this file, `AGENTS.md`, `CONTRIBUTING.md`, the current issue/PR templates, and the
current label policy. Only labels that currently exist in the repository may be
referenced; the automation must not create labels.
