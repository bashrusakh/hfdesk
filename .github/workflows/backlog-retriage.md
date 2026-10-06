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
  status-comment: false
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
  args: ["--deny-tool", "shell"]
  concurrency:
    group: "gh-aw-triage-${{ github.repository }}"
    queue: max
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
  - bashrusakh/repo-docs-sync/packages/ghaw-triage/workflows/contract-invariant.md@e2de4a989077b7fdf558ef5656040dc2e547e674
  - bashrusakh/repo-docs-sync/packages/ghaw-triage/workflows/backlog-retriage-core.md@e2de4a989077b7fdf558ef5656040dc2e547e674
checkout: false
max-ai-credits: 10
max-turns: 20
timeout-minutes: 20
network:
  allowed: [defaults, github, ollama.com]
tools:
  bash: false
  cli-proxy: false
  edit: false
  github:
    mode: local
    toolsets: [issues]
    # Declared scope only, not a runtime guarantee (a gateway safety net may widen it):
    # 'repos' is the current repository — exactly the expression below. min-integrity: none
    # is deliberate: triage must read reports from any contributor, and gh-aw docs prescribe
    # 'none' for public-repo triage. Otherwise metadata-only (no shell/source/diff; safe
    # outputs are label adds/removes only), so the injection surface is metadata-only. If
    # abuse appears, add blocked-users / trusted-users / approval-labels.
    allowed-repos: ["${{ github.repository }}"]
    min-integrity: none
    # max-calls is declared intent; gh-aw v0.89.21 currently drops it at compile time (no tool-call-limits in locks). Revisit when the compiler emits limits.
    # No 'pull_requests' toolset: it exposes pull_request_read (with get_diff/get_files)
    # and list_pull_requests, which the CLI does not filter down to the declared allowed
    # list, so a PR-search capability would become agent-visible. Item metadata, including
    # changed filenames where a batch item is a PR, comes from the prepared
    # .policy/backlog/<POLICY_SHA>/batch.json; related/duplicate reasoning over PRs uses
    # that supplied metadata only.
    allowed:
      - name: issue_read
        max-calls: 12
      - name: search_issues
        max-calls: 3
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
  report-failure-as-issue: false
  report-failed-jobs: false
  add-labels:
    # Disable issue-intent metadata (rationale/confidence/suggest) for label adds: the
    # exposed tool schema drops those fields and the handler never routes a label through
    # pending-suggestion review. hfdesk triage applies labels directly; a suggestion would
    # be a silent no-op. (remove-labels has no suggestion path and rejects this key.)
    issue-intent: false
    allowed: ["bug", "enhancement", "documentation", "question", "refactor", "ci", "needs-info", "duplicate"]
    blocked: ["priority-*", "codex-*", "confirmed", "invalid", "wontfix", "good first issue", "help wanted", "~*", "*[bot]"]
    max: 5
  remove-labels:
    allowed: ["bug", "enhancement", "documentation", "question", "refactor", "ci", "needs-info", "duplicate"]
    blocked: ["priority-*", "codex-*", "confirmed", "invalid", "wontfix", "good first issue", "help wanted", "~*", "*[bot]"]
    max: 5
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
an all-repository sweep. If one item needs disproportionate investigation, preserve the
affected managed metadata and move on; use `noop` if a completion signal is required.

Related/duplicate/fixed/supersession reasoning over pull requests is bounded to the
supplied batch metadata: this deployment exposes no PR-search or PR-list capability, so do
not attempt or claim to look up pull requests outside the supplied batch by search, list,
or identifier. Judge relatedness from the supplied titles, bodies, labels, and state
only; when that evidence cannot establish equivalence, preserve the affected state.

## hfdesk managed label boundary

- Managed across the union of hfdesk triage families: `bug`, `enhancement`,
  `documentation`, `question`, `refactor`, `ci`, `needs-info`, `duplicate`.
- Type reconciliation: preserve a correct contributor-applied type, fill a type that is
  clearly missing, and replace a clearly incorrect managed type (remove the wrong type and
  add the correct one). Make no change when the type is ambiguous — preserve the affected
  state rather than churning it.
- `confirmed` is human/verification-owned and out of scope: this workflow does not own
  it and must never add or remove it.
- Human-reserved (never add or remove; never infer): `priority-*`, `codex-*`,
  `approved-for-fix`, `codex-fixing`, `ready-for-human-review`, `invalid`, `wontfix`,
  `good first issue`, `help wanted`, and anything not listed as managed.
- Priority is maintainer-owned. Do not create labels. Do not auto-close/reopen issues,
  close/merge PRs, modify code, assign users, or post routine comments. hfdesk
  authorizes no prerequisite/approval/decision-gate semantics for automation.

This workflow is metadata-only and silent: the agent produces no human-facing output — it
does not reproduce, validate, review code, read source/diff, or comment — and the only
repository writes it performs are the label safe outputs. When evidence is insufficient,
preserve the affected managed metadata and use `noop` if a completion signal is required —
the agent has no explanatory output channel. `needs-info` means metadata/routing
information is missing; it never means implementation proof is missing. The metadata
baseline needed to classify and route a report is the semantic type, what is broken or the
affected user-facing area, and the reported behavior; a report establishing neither the
affected area nor the reported behavior (for example "doesn't work") is missing routing
information, so `needs-info` applies even when the type is inferable. This scopes the
silence claim to the agent and its safe outputs; operator diagnostics are separate, and
gh-aw may still file run-failure or detection diagnostics as repository-level issues
outside this workflow's agent and safe outputs.

Prefer preserving an existing state over speculative churn. Use only this workflow's safe
outputs (`add-labels`, `remove-labels`). Request label adds and removes directly as plain
label names; never attach `suggest`, `rationale`, or `confidence` intent metadata — this
deployment does not use suggestion/intent review, and a suggested label is not applied.
