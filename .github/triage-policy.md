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

Contributor-first labeling: `CONTRIBUTING.md` is the source of truth for triage
labels. Contributors apply the type label first; the automation only verifies the
label and fills a missing or incorrect one. It must not relabel a correct human
label.

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
