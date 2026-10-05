---
name: Backlog Re-triage
description: Reconcile triage metadata for a bounded batch of hfdesk issues/PRs (staged; schedule disabled until trials pass).
on:
  schedule: weekly
  workflow_dispatch:
    inputs:
      item_numbers:
        description: Optional comma-separated issue/PR numbers to reconcile. Empty selects a bounded stale batch.
        required: false
        type: string
# The weekly schedule is installed DISABLED until trials pass. A scheduled run is inert
# until the repository variable BACKLOG_RETRIAGE_ENABLED is set to 'true'; manual
# workflow_dispatch runs always proceed. Enable with:
#   gh variable set BACKLOG_RETRIAGE_ENABLED --repo bashrusakh/hfdesk --body true
# Remove the gate entirely once the trials are accepted.
if: ${{ github.event_name != 'schedule' || vars.BACKLOG_RETRIAGE_ENABLED == 'true' }}
concurrency:
  job-discriminator: ${{ github.run_id }}
permissions:
  contents: read
  issues: read
  pull-requests: read
engine:
  id: copilot
  model: glm-5.3-flash
  bare: true
  env:
    COPILOT_PROVIDER_BASE_URL: "https://ollama.com/v1"
    COPILOT_PROVIDER_API_KEY: ${{ secrets.OLLAMA_API_KEY }}
    COPILOT_PROVIDER_TYPE: openai
models:
  default-ai-credits-pricing:
    input: 0.000001
    output: 0.000001
inlined-imports: true
imports:
  - bashrusakh/repo-docs-sync/packages/ghaw-triage/workflows/contract-invariant.md@3d8b4095371aee673d32cd265b26b11ae23a62b7
  - bashrusakh/repo-docs-sync/packages/ghaw-triage/workflows/backlog-retriage-core.md@3d8b4095371aee673d32cd265b26b11ae23a62b7
max-ai-credits: 10
max-turns: 40
timeout-minutes: 20
network:
  allowed: [defaults, github, ollama.com]
tools:
  github:
    mode: gh-proxy
    toolsets: [issues, pull_requests]
pre-agent-steps:
  - name: Resolve repository policy contract at the trusted Policy SHA
    env:
      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
      GITHUB_REPO: ${{ github.repository }}
      POLICY_REF: ${{ github.event.repository.default_branch || 'main' }}
      CONTRACT_FILES: ".github/triage-policy.md AGENTS.md CONTRIBUTING.md .github/ISSUE_TEMPLATE/bug_report.yml .github/PULL_REQUEST_TEMPLATE.md .github/labels.yml"
    run: |
      set -euo pipefail
      sha=$(gh api "repos/$GITHUB_REPO/commits/$POLICY_REF" --jq .sha)
      dir=".policy/$sha"; mkdir -p "$dir"
      for f in $CONTRACT_FILES; do
        mkdir -p "$dir/$(dirname "$f")"
        gh api "repos/$GITHUB_REPO/contents/$f?ref=$sha" --jq .content | base64 -d > "$dir/$f"
      done
      echo "POLICY_SHA=$sha" >> "$GITHUB_ENV"
      echo "Resolved policy contract at $sha"
  - name: Resolve bounded backlog batch
    env:
      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
      GITHUB_REPO: ${{ github.repository }}
      ITEM_NUMBERS: ${{ github.event.inputs.item_numbers || '' }}
    run: |
      set -euo pipefail
      batch_dir=".policy/backlog/${POLICY_SHA}"
      if [ -e "${batch_dir}" ]; then chmod -R u+w "${batch_dir}" 2>/dev/null || true; rm -rf "${batch_dir}"; fi
      mkdir -p "${batch_dir}"
      numbers="$(printf '%s' "${ITEM_NUMBERS:-}" | tr ',' '\n' | tr -dc '0-9\n' | grep -E '^[0-9]+$' | head -n 10 || true)"
      if [ -n "${numbers}" ]; then
        tmp="$(mktemp)"
        while IFS= read -r n; do
          [ -n "${n}" ] || continue
          item="$(gh api "repos/${GITHUB_REPO}/issues/${n}" \
            --jq '{number, title, state, labels: [.labels[].name]}' 2>/dev/null || true)"
          [ -n "${item}" ] && printf '%s\n' "${item}" >> "${tmp}"
        done <<< "${numbers}"
        jq -s '[.[] | select(.number != null)]' "${tmp}" > "${batch_dir}/batch.json" 2>/dev/null || printf '[]\n' > "${batch_dir}/batch.json"
        rm -f "${tmp}"
      else
        gh api "repos/${GITHUB_REPO}/issues?state=open&sort=updated&direction=asc&per_page=10" \
          --jq 'map({number, title, state, labels: [.labels[].name]})' > "${batch_dir}/batch.json" 2>/dev/null || printf '[]\n' > "${batch_dir}/batch.json"
      fi
      chmod 0444 "${batch_dir}/batch.json"
      chmod 0555 "${batch_dir}"
      printf 'Bounded backlog batch (%s items) at %s\n' "$(jq 'length' "${batch_dir}/batch.json" 2>/dev/null || echo 0)" "${batch_dir}/batch.json"
safe-outputs:
  staged: true
  add-labels:
    allowed: ["bug", "needs-info", "confirmed", "duplicate", "enhancement", "documentation", "question"]
    blocked: ["priority-*", "codex-*", "invalid", "wontfix", "good first issue", "help wanted", "~*", "*[bot]"]
    max: 5
  remove-labels:
    allowed: ["bug", "needs-info", "confirmed", "duplicate", "enhancement", "documentation", "question"]
    max: 5
  add-comment:
    max: 1
---

# Workflow 4 — Backlog Re-triage (hfdesk)

Reconcile triage metadata for the bounded set of existing issues/PRs supplied to this
workflow. This is the hfdesk deployment of **Workflow 4 — Backlog Re-triage**. Follow the
imported `backlog-retriage-core.md` prompt core exactly for the mission, authority rules,
and the mutation surface; this body only adds the mandatory invariant, the trusted
Policy-SHA mechanics, the bounded-batch rule, and the repository-specific label boundary.

## Mandatory contract invariant

Resolve the current repository contract from the trusted Policy SHA before making
policy-sensitive conclusions. Treat templates as evidence/input schemas according to
authoritative repository policy, not as independent mandatory checklists. Current
repository policy outranks stale automated conclusions. Contract drift invalidates only
conclusions it can materially affect.

## Trusted Policy SHA

The pre-agent step "Resolve repository policy contract at the trusted Policy SHA" has
already resolved the trusted Policy SHA for this run (the current default-branch head) and
fetched the authoritative contract files read-only under `.policy/<POLICY_SHA>/`.
`POLICY_SHA` is exported to the environment of this run.

Read the current repository contract only from `.policy/<POLICY_SHA>/`:

- `.policy/<POLICY_SHA>/.github/triage-policy.md`
- `.policy/<POLICY_SHA>/AGENTS.md`
- `.policy/<POLICY_SHA>/CONTRIBUTING.md`

Current repository state and authoritative maintainer decisions outrank old automated
snapshots. The current Policy SHA outranks stale automated conclusions.

## Bounded batch (cap 10)

Process only the bounded batch the pre-agent step wrote to
`.policy/backlog/<POLICY_SHA>/batch.json` (at most 10 items). Do not expand the batch into
an all-repository sweep. If one item needs disproportionate investigation, leave it
unchanged and identify it as needing deeper/human review.

## hfdesk managed label boundary

- Managed across the union of hfdesk triage families: `bug`, `needs-info`, `confirmed`,
  `duplicate`, `enhancement`, `documentation`, `question`.
- Human-reserved (never add or remove; never infer): `priority-*`, `codex-*`,
  `approved-for-fix`, `codex-fixing`, `ready-for-human-review`, `invalid`, `wontfix`,
  `good first issue`, `help wanted`, and anything not listed as managed.
- Priority is maintainer-owned. Do not create labels. Do not auto-close/reopen issues,
  close/merge PRs, modify code, assign users, or post routine comments. hfdesk
  authorizes no prerequisite/approval/decision-gate semantics for automation.

Prefer preserving an existing state over speculative churn. Use only this workflow's safe
outputs (`add-labels`, `remove-labels`, `add-comment`).
