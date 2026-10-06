#!/usr/bin/env python3
"""Deterministic contract validation for the hfdesk agentic triage contract.

No LLM. Checks, in order:
  1. every agentic workflow source imports the shared invariant and its core,
     pinned to the exact shared-package SHA;
  2. each triage workflow source declares the metadata-only capability surface:
     checkout/status-comment/bash/cli-proxy pinned false, tools.edit pinned false
     (no filesystem write capability), the engine-level shell denial
     (engine.args containing '--deny-tool shell'), safe-outputs
     report-failure-as-issue and report-failed-jobs pinned false, no add-comment,
     a fail-closed safe-outputs allowlist permitting only the
     report-failure-as-issue/report-failed-jobs/add-labels/remove-labels key
     family (no comment/issue/discussion/PR-write class, no unknown key),
     safe-outputs.add-labels issue-intent false (no label suggestion routing),
     the direct-label prompt instruction, tools.github.toolsets exactly ['issues']
     (the 'pull_requests' toolset would expose patch-capable PR tools the declared
     'allowed' narrowing cannot suppress), tools.github.allowed-repos declared as
     the single-element array ['${{ github.repository }}'] (a bare scalar string is
     rejected by the runtime gateway), tools.github.min-integrity present, one of
     the gh-aw schema levels, and exactly the intended level, no source/diff tools
     in the github allowed list including the PR-search/list tools
     (pull_request_read, list_pull_requests, search_pull_requests), no 'confirmed'
     in allowed label lists, and the
     required type family in remove-labels;
  3. each generated lock preserves those invariants (no add_comment, no forbidden
     github tool grants, compiled GITHUB_TOOLSETS exactly 'issues', an allow-only
     guard policy whose 'repos' is the
     single-element array ['${{ github.repository }}'] and whose
     'min-integrity' is exactly the intended
     level, no residual agent write capability: no --allow-tool write and no
     --allow-all-paths in the agent job, the compiled '--deny-tool shell' flag in
     the agent invocation, add_labels issue_intent:false in both safe-output
     configs, the compiled safe-output tool set exactly the read-only label
     family (manifest tools and both config key sets), the github server env
     carrying GITHUB_READ_ONLY, the safeoutputs write-sink 'accept' list confined
     to the private repository form, no report-failed-jobs machinery, failure
     reports disabled, no
     agent-job checkout, not staged) and stays structurally in sync with its source
     (shared import pin + exact safe-output label lists);
  4. every contract file referenced by .github/triage-policy.md exists;
  5. every workflow-managed label named by the policy currently exists
     (declared in .github/labels.yml and/or present in the live repository);
  6. the pinned shared import is vendored under .github/aw/imports/** so a
     runtime/compile never needs to read the private sibling repository;
  7. locks without tool-call-limits are reported as a warning (never an error):
     gh-aw v0.89.21 drops max-calls at compile time, so declared per-tool call
     limits are not yet enforced.

A referenced *managed* label that exists in neither the declared label file nor the
live repository is an error (removed/renamed managed label). A managed label that is
live but missing from the declared reference file is a warning: the reference file is
a setup aid and may lag the authoritative live label set.

Lock checks are textual (no YAML library) and deliberately conservative. Forbidden
github tool names are matched on the real agent-visible grant surfaces
(github(<tool>) allow-tool flags and the gh-aw-manifest mcp_servers 'github' tools
list), not the whole file, because the inlined shared contract quotes some of those
names in prohibition prose; the add_comment scan is whole-file and fail-closed.

What this validator does NOT guarantee:

  - It cannot see the gh-aw compile step. It compares the shared import pin and the
    exact add-labels/remove-labels allowed lists as a textual proxy; prompt text,
    tool schemas, and general job wiring are not proven in sync. Run
    `gh aw compile --strict` for that.
  - It cannot prove what the runtime MCP gateway does. The guard policy is asserted
    as a declaration (repos exactly ['${{ github.repository }}'], min-integrity
    exactly the intended level); it does not prove the gateway confines reads to
    that scope at run time, and a runtime safety net may widen it.
  - The min-integrity assertion is a match against INTENDED_MIN_INTEGRITY, a constant
    in this file. It catches unreviewed weakening or re-strengthening of the declared
    level, but it is not itself evidence that the declared level is the right security
    posture; changing that is a deliberate decision that must update the constant.
  - The `--allow-tool write` / `--allow-all-paths` scan is a substring check over the
    agent job section. It catches those exact flags disappearing or reappearing; it
    does not prove the absence of every other capability the engine could grant.
  - The engine-level shell assertion is textual on both sides: the source must contain
    the argument pair '--deny-tool shell' in engine.args, and the lock must contain the
    compiled '--deny-tool shell' flag in the agent job. It proves the denial reaches the
    Copilot CLI invocation; it cannot prove how the CLI's approval gate behaves at run
    time, so a staged trial remains the only end-to-end evidence for that.
  - The issue-intent assertion is textual on the exposed config and on the source key. It
    proves the compiled config requests the intent-free schema; the pinned runtime script
    (gh-aw-actions/setup, pinned in the lock manifest) is what actually strips the fields,
    so a change to that pin must be re-checked against that script.
  - The safe-output surface assertion is textual on three compiled surfaces (the manifest
    'safeoutputs' tool list, both config key sets, and the write-sink 'accept' list) plus
    the manifest's GITHUB_READ_ONLY env flag. It proves which handlers the pinned compiler
    enabled and declared; it cannot prove the runtime handler module behaves as its name
    implies, so a pin change to gh-aw-actions/setup must be re-checked.
  - The source safe-output allowlist parses block-mapping keys at depth 2 under a
    'safe-outputs:' key. It rejects a comment/issue/discussion/PR-write class and any
    unknown key, but a nesting trick the textual parser cannot see (for example a
    different key shape that gh-aw itself understands but this parser does not) would not
    be caught; `gh aw compile --strict` remains the schema-level check.
  - The toolsets assertion matches the declared list and the compiled GITHUB_TOOLSETS
    value against the intended set. It proves the workflow only requests the issue
    surface; it cannot prove how the pinned github-mcp-server builds its advertised
    tool list from that value, so the toolset-level prohibition rests on the server
    version pinned in the lock manifest. A server upgrade must be re-checked against
    the toolset-to-tool mapping (a staged trial is the end-to-end evidence).

Set VALIDATE_CONTRACT_SKIP_LIVE_LABELS=1 to skip the live repository label lookup
(offline/test mode used by .github/aw/test_validate_contract.py); in that mode
.github/labels.yml must declare every managed label because the live set is not read.
The live lookup also degrades to a warning (rather than an error) when it is attempted
but cannot run at all (no `gh` binary, no auth/network, unreadable repository), so the
validator stays usable offline; when the live set IS readable, a managed label missing
from both the declared file and the live repository remains a hard error.
"""

from __future__ import annotations

import glob
import json
import os
import re
import subprocess
import sys

PIN = "e2de4a989077b7fdf558ef5656040dc2e547e674"
REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
POLICY = os.path.join(REPO_ROOT, ".github", "triage-policy.md")
LABELS_YML = os.path.join(REPO_ROOT, ".github", "labels.yml")
IMPORTS_ROOT = os.path.join(REPO_ROOT, ".github", "aw", "imports")
WORKFLOW_DIR = os.path.join(REPO_ROOT, ".github", "workflows")

WORKFLOWS = {
    "issue-triage.md": "issue-triage-core.md",
    "pr-semantic-intake.md": "pr-intake-core.md",
    "backlog-retriage.md": "backlog-retriage-core.md",
}

# Agent-visible capabilities the metadata-only triage contract forbids. Matched
# against real grant surfaces (github(<tool>) allow-tool flags and the compiled
# gh-aw-manifest mcp_servers 'github' tools list), never against whole-file text,
# because the inlined shared contract quotes these names in prohibition prose.
FORBIDDEN_GITHUB_TOOLS = (
    "get_pull_request_files",
    "get_pull_request_diff",
    "get_file_contents",
    "search_code",
    "get_files",
    "pull_request_read",
    "list_pull_requests",
    "search_pull_requests",
)

# The only github MCP toolset the triage contract may request: the issue surface.
# The 'pull_requests' toolset is forbidden because the pinned server
# (github-mcp-server v1.12.2) advertises that toolset's tools in full - including
# pull_request_read (whose method enum contains get_diff/get_files) and
# list_pull_requests - and the compiled per-tool '--allow-tool github(<tool>)' grants
# are not enforced by the Copilot CLI against toolset-exposed tools. Asking for the
# toolset therefore creates a real agent-visible capability regardless of the
# declared 'allowed' narrowing. Asserted as an exact intended set so any widening
# or re-tightening is a deliberate change to this constant.
REQUIRED_GITHUB_TOOLSETS = ("issues",)

# The compiled toolsets value the docker-launched github MCP server reads.
LOCK_TOOLSETS = re.compile(r'"GITHUB_TOOLSETS"\s*:\s*"([^"]*)"')

# Universal semantic type family every triage workflow must be able to remove
# (reconciliation capability: replace a clearly wrong managed type).
REQUIRED_REMOVE_LABELS = ("bug", "enhancement", "documentation", "question", "refactor", "ci")

# The engine-level shell denial every triage workflow must pass to the Copilot CLI.
# tools.bash: false makes gh-aw emit no shell(...) grant, but the Copilot CLI's own
# approval gate can still decide to run read-only commands, so the workflow must also
# deny the shell tool at the engine level. Asserted as the adjacent argument pair in
# engine.args and as the compiled CLI flag in the agent job.
REQUIRED_ENGINE_ARGS = ("--deny-tool", "shell")
LOCK_DENY_SHELL = " ".join(REQUIRED_ENGINE_ARGS)

# The deployment prompt bodies must tell the agent to request label changes directly.
# The add-labels safe-output schema exposes optional rationale/confidence/suggest intent
# metadata, and a suggested label is routed to pending review instead of applied, which
# is a silent no-op for a workflow that must apply labels. The config-level lever
# (safe-outputs.add-labels.issue-intent: false) removes those fields from the exposed
# tool schema; this instruction keeps the agent from relying on intent review anyway.
DIRECT_LABEL_INSTRUCTION = "never attach `suggest`, `rationale`, or `confidence`"

# The exact repository-scope expression the guard policy must declare, in the
# only runtime-accepted shape: a single-element array ['${{ github.repository }}'].
# gh-aw emits a scalar string for a scalar declaration, and the pinned gateway
# (gh-aw-mcpg v0.4.25) accepts a string 'repos' only as 'all' or 'public', so the
# scalar owner/repo form compiles but fails at runtime. A literal owner/repo,
# "all", "public", or any wildcard pattern is a scope change (widening or
# retargeting) and fails too: a literal owner/repo cannot follow a repository
# rename or transfer, and the widened forms are not the declared scope.
REPO_SCOPE_EXPRESSION = "${{ github.repository }}"

# The gh-aw v0.89.21 schema enum for tools.github.min-integrity, ordered from
# most to least restrictive in the upstream documentation sense.
MIN_INTEGRITY_LEVELS = ("merged", "approved", "unapproved", "none")

# The intended min-integrity level. gh-aw rejects a guard policy that sets
# allowed-repos without min-integrity, so the field cannot simply be dropped;
# the level and this constant must be changed together, deliberately. 'none' is
# the deliberate choice here: triage must read reports from any contributor, and
# gh-aw docs prescribe 'none' for public-repo triage. A change that does not
# update this constant fails validation, so both weakening and re-strengthening
# are caught rather than silently accepted.
INTENDED_MIN_INTEGRITY = "none"

SKIP_LIVE_LABELS_ENV = "VALIDATE_CONTRACT_SKIP_LIVE_LABELS"
LOCK_SUFFIX = ".lock.yml"

GITHUB_GRANT = re.compile(r"github\(([^)]*)\)")
LOCK_MANIFEST_GITHUB = re.compile(r'"name"\s*:\s*"github"\s*,\s*"tools"\s*:\s*\[([^\]]*)\]')
SHARED_REF_SHA = re.compile(r"repo-docs-sync/[^\s@\"']+@([0-9a-f]{40})")
LOCK_CONFIG_KEYS = ("GH_AW_SAFE_OUTPUTS_CONFIG", "GH_AW_SAFE_OUTPUTS_HANDLER_CONFIG")

# The only safe-outputs keys a triage source may declare. An explicit allowlist,
# not a forbidden list: any unlisted key (including a future comment/issue/
# discussion/PR-write safe output, or a lock-side misconfiguration) fails closed
# rather than compiling and passing green. The metadata-only contract has exactly
# two failure-diagnostic switches and two label outputs.
ALLOWED_SOURCE_SAFE_OUTPUT_KEYS = (
    "report-failure-as-issue",
    "report-failed-jobs",
    "add-labels",
    "remove-labels",
)

# Write-capable safe-output classes the agent surface must never expose. Named
# separately from the allowlist only to report the precise capability family
# instead of a generic 'unknown key'; the allowlist is the enforcing check.
FORBIDDEN_SOURCE_SAFE_OUTPUT_KEYS = (
    "add-comment",
    "create-issue",
    "update-issue",
    "close-issue",
    "create-discussion",
    "update-discussion",
    "create-pull-request",
    "push-to-pull-request-branch",
    "create-pull-request-review-comment",
    "reply-to-pull-request-review-comment",
    "submit-pull-request-review",
    "create-agent-session",
    "assign-to-user",
    "create-code-scanning-alert",
    "upload-asset",
)

# The exact compiled agent-visible safe-output tool set, parsed from the
# gh-aw-manifest mcp_servers 'safeoutputs' entry. This is the surface the Copilot
# CLI can call (the lock carries a single '--allow-tool safeoutputs' grant, so the
# manifest tool list is the whole agent-visible write surface).
REQUIRED_LOCK_SAFE_OUTPUT_TOOLS = (
    "add_labels",
    "missing_data",
    "missing_tool",
    "noop",
    "remove_labels",
)

# The exact key set each compiled safe-output config may carry. It is the agent
# tool set above plus two keys the pinned compiler always emits as operator
# diagnostics, not agent tools: 'report_incomplete' records an agent incomplete
# signal, and 'create_report_incomplete_issue' is handled in the conclusion job
# (GH_AW_REPORT_INCOMPLETE_CREATE_ISSUE). Both are pre-existing on the base branch;
# asserting an exact set (rather than 'no forbidden key') keeps a future
# comment/issue/discussion/PR-write handler from slipping into either config.
ALLOWED_LOCK_SAFE_OUTPUT_CONFIG_KEYS = tuple(
    sorted(REQUIRED_LOCK_SAFE_OUTPUT_TOOLS)
    + ["create_report_incomplete_issue", "report_incomplete"]
)
LOCK_MANIFEST_SAFEOUTPUTS = re.compile(
    r'"name"\s*:\s*"safeoutputs"\s*,\s*"tools"\s*:\s*\[([^\]]*)\]'
)

# The only write-sink accept entry the safeoutputs MCP server may carry: the
# private form of the current-repository expression. '*', 'all', or 'public'
# would admit writes to other repositories or to public sinks.
SAFE_OUTPUTS_SINK_ACCEPT = "private:" + REPO_SCOPE_EXPRESSION

BACKTICK = re.compile(r"`([^`]+)`")
LABEL_LIKE = re.compile(r"^[a-z0-9][a-z0-9 ._*-]*$")
PATH_LIKE = re.compile(r"(/|\.md$|\.yml$|\.yaml$)")

errors: list[str] = []
warnings: list[str] = []


def read(path: str) -> str:
    with open(path, encoding="utf-8") as fh:
        return fh.read()


def section(text: str, heading: str) -> str:
    """Return the body of a '## <heading>' section."""
    match = re.search(
        r"^##\s+" + re.escape(heading) + r"\s*$", text, re.MULTILINE
    )
    if not match:
        return ""
    rest = text[match.end():]
    end = re.search(r"^##\s+", rest, re.MULTILINE)
    return rest[: end.start()] if end else rest


def declared_labels() -> set[str]:
    if not os.path.exists(LABELS_YML):
        errors.append(f"missing declared label file {rel(LABELS_YML)}")
        return set()
    names = set()
    for line in read(LABELS_YML).splitlines():
        m = re.match(r"^\s*-\s*name:\s*(.+?)\s*$", line)
        if m:
            names.add(m.group(1).strip().strip("\"'"))
    return names


def live_labels() -> tuple[set[str], str]:
    """Return (names, state) for the live repository label set.

    `state` is one of:
      - "live": the label list was read; a managed label absent from both the
        declared file and this set is an error (fail-closed);
      - "skipped": VALIDATE_CONTRACT_SKIP_LIVE_LABELS=1 explicitly asks for a
        self-sufficient declared file, so a miss stays an error;
      - "unavailable": the lookup was attempted but could not run (no `gh`, no
        auth/network, unreadable repository). The caller degrades a declared-file
        miss to a warning so the validator remains usable offline; it cannot tell
        drift from a removed label without the live set.
    """
    if os.environ.get(SKIP_LIVE_LABELS_ENV) == "1":
        return set(), "skipped"
    repo = os.environ.get("GH_REPO", "")
    cmd = ["gh", "label", "list", "--limit", "200"]
    if repo:
        cmd += ["--repo", repo]
    try:
        out = subprocess.run(
            cmd, capture_output=True, text=True, timeout=60, check=True
        ).stdout
    except Exception as exc:  # noqa: BLE001 - report and degrade to declared-only
        warnings.append(
            f"could not read live labels ({exc}); checking declared file only "
            "(missing declared labels are warnings until the live set is readable)"
        )
        return set(), "unavailable"
    names = set()
    for line in out.splitlines():
        if line.strip():
            names.add(line.split("\t", 1)[0].strip())
    return names, "live"


def rel(path: str) -> str:
    return os.path.relpath(path, REPO_ROOT)


def check_workflow_imports() -> None:
    for md, core in WORKFLOWS.items():
        path = os.path.join(WORKFLOW_DIR, md)
        if not os.path.exists(path):
            errors.append(f"missing workflow source {rel(path)}")
            continue
        text = read(path)
        for component in ("contract-invariant.md", core):
            needle = f"packages/ghaw-triage/workflows/{component}@{PIN}"
            if needle not in text:
                errors.append(
                    f"{rel(path)} does not import {component} pinned to {PIN}"
                )
        if "inlined-imports: true" not in text:
            errors.append(f"{rel(path)} must set inlined-imports: true")


def _exists(token: str) -> bool:
    """True when a policy-referenced path resolves under the repo root or .github."""
    token = token.strip().lstrip("/")
    bases = (REPO_ROOT, os.path.join(REPO_ROOT, ".github"))
    for base in bases:
        candidate = os.path.join(base, token)
        if os.path.exists(candidate):
            return True
        if "*" in token and glob.glob(candidate):
            return True
    return False


def check_contract_files() -> None:
    text = read(POLICY) if os.path.exists(POLICY) else ""
    if not text:
        errors.append(f"missing policy file {rel(POLICY)}")
        return

    # Core contract files that must exist as concrete files.
    concrete = {
        ".github/triage-policy.md",
        "AGENTS.md",
        "CONTRIBUTING.md",
        ".github/PULL_REQUEST_TEMPLATE.md",
    }
    for token in BACKTICK.findall(text):
        token = token.strip()
        if PATH_LIKE.search(token) and not token.startswith("http") and " " not in token:
            concrete.add(token)

    for token in sorted(concrete):
        if not _exists(token):
            errors.append(f"policy references missing contract file: {token}")


def check_labels() -> None:
    text = read(POLICY) if os.path.exists(POLICY) else ""
    if not text:
        return
    managed_blob = section(text, "Managed label families")
    managed = {
        tok.strip()
        for tok in BACKTICK.findall(managed_blob)
        if LABEL_LIKE.match(tok.strip())
    }
    if not managed:
        errors.append("policy declares no managed labels under 'Managed label families'")
        return

    declared = declared_labels()
    live, live_state = live_labels()
    known = declared | live

    for label in sorted(managed):
        if label in known:
            if label not in declared:
                warnings.append(
                    f"managed label '{label}' exists live but is missing from "
                    f"{rel(LABELS_YML)} (reference-set drift)"
                )
        elif live_state == "unavailable":
            # The live label set could not be read (offline run or unavailable `gh`),
            # so a declared-file miss cannot be distinguished from a label that exists
            # live but is not declared here. Degrade to a warning so the validator is
            # usable offline; with a readable live set (or the explicit offline test
            # mode, where the declared file must be self-sufficient) this is an error.
            warnings.append(
                f"managed label '{label}' is not declared in {rel(LABELS_YML)} and "
                "the live label set could not be read; cannot confirm it exists "
                "(reference-set drift or removed/renamed label)"
            )
        else:
            errors.append(
                f"managed label '{label}' referenced by policy exists in neither "
                f"{rel(LABELS_YML)} nor the live repository (removed/renamed?)"
            )


def check_vendored_imports() -> None:
    for component in (
        "contract-invariant.md",
        "issue-triage-core.md",
        "pr-intake-core.md",
        "backlog-retriage-core.md",
    ):
        pattern = os.path.join(
            IMPORTS_ROOT, "bashrusakh", "repo-docs-sync", PIN,
            f"packages_ghaw-triage_workflows_{component}",
        )
        if not os.path.exists(pattern):
            errors.append(
                f"pinned import for {component} is not vendored at "
                f"{rel(pattern)} (runtime would need the private repo)"
            )


def frontmatter(text: str) -> str:
    """Return the YAML frontmatter between the leading --- fences, or ''."""
    match = re.match(r"^---\s*$(.*?)^---\s*$", text, re.MULTILINE | re.DOTALL)
    return match.group(1) if match else ""


def key_entry(text: str, key: str, indent: int) -> tuple[str, list[str]] | None:
    """Textual YAML mapping lookup for a key at an exact indent (no YAML lib).

    Returns (tail, body): `tail` is the value text on the key's own line and
    `body` holds the following lines indented deeper than the key (blank and
    comment lines continue the block). The search is bounded by the caller's
    text slice, so nested lookups pass the parent block's body as `text`.
    """
    pattern = re.compile(r"^" + " " * indent + re.escape(key) + r"\s*:(.*)$")
    lines = text.splitlines()
    for index, line in enumerate(lines):
        match = pattern.match(line)
        if not match:
            continue
        body: list[str] = []
        for following in lines[index + 1:]:
            if not following.strip() or following.lstrip().startswith("#"):
                body.append(following)
                continue
            if len(following) - len(following.lstrip(" ")) <= indent:
                break
            body.append(following)
        return match.group(1).strip(), body
    return None


def _unquote(value: str) -> str:
    """Strip one layer of matching single or double quotes from a scalar."""
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
        return value[1:-1]
    return value


def string_list(tail: str, body: list[str]) -> list[str]:
    """Extract a YAML string list from a flow value (`[a, b]`) or block items."""
    if tail.startswith("["):
        inner = tail[1:tail.rindex("]")] if "]" in tail else tail[1:]
        return [item.strip().strip("\"'") for item in inner.split(",") if item.strip()]
    if tail:
        return [tail.strip("\"'")]
    values: list[str] = []
    for line in body:
        match = re.match(r"^\s*-\s*(.+?)\s*$", line)
        if match:
            values.append(match.group(1).strip().strip("\"'"))
    return values


def repo_scope_entries(value: str) -> list[str] | None:
    """Parse an allowed-repos value, accepting only the YAML flow-array form.

    Returns the scope entries for `[a, b]`, or None when `value` is a bare
    scalar (quoted or not). The scalar form is the shape the runtime gateway
    rejects (`allow-only.repos string must be 'all' or 'public'`), so it must
    not be accepted here even when it names the same repository expression.
    """
    value = value.strip()
    if not (value.startswith("[") and value.endswith("]")):
        return None
    return string_list(value, [])


def triage_source_label_lists(fm: str) -> dict[str, list[str] | None]:
    """Allowed label lists declared under safe-outputs.{add,remove}-labels."""
    lists: dict[str, list[str] | None] = {}
    safe = key_entry(fm, "safe-outputs", 0)
    safe_body = "\n".join(safe[1]) if safe else ""
    for name in ("add-labels", "remove-labels"):
        labels = None
        entry = key_entry(safe_body, name, 2) if safe else None
        if entry is not None:
            allowed = key_entry("\n".join(entry[1]), "allowed", 4)
            if allowed is not None:
                labels = string_list(allowed[0], allowed[1])
        lists[name] = labels
    return lists


def engine_args(fm: str) -> list[str] | None:
    """Parse the engine.args list, or None when the key is absent.

    Returns [] for a present-but-empty value, so a removed denial is reported as a
    missing argument rather than as a missing key.
    """
    engine = key_entry(fm, "engine", 0)
    if engine is None:
        return None
    args = key_entry("\n".join(engine[1]), "args", 2)
    if args is None:
        return None
    return string_list(args[0], args[1])


def add_labels_issue_intent_false(fm: str) -> bool:
    """True when safe-outputs.add-labels declares exactly 'issue-intent: false'.

    Scoped to the add-labels block on purpose: gh-aw v0.89.21 only accepts the key
    for issue-intent-capable safe outputs, and an unrelated top-level occurrence
    (for example tools.comment-memory) must not satisfy this check.
    """
    safe = key_entry(fm, "safe-outputs", 0)
    if safe is None:
        return False
    entry = key_entry("\n".join(safe[1]), "add-labels", 2)
    if entry is None:
        return False
    return bool(
        re.search(
            r"^\s*issue-intent\s*:\s*false\s*$", "\n".join(entry[1]), re.MULTILINE
        )
    )


def source_safe_output_keys(fm: str) -> list[str] | None:
    """Return the direct safe-outputs keys declared in the source frontmatter.

    Returns None when there is no safe-outputs block, and the ordered key list
    otherwise. The direct-child indentation is taken as the minimum indentation of
    the block body, so the mapping is checked whatever consistent indent is used.
    Used as an explicit allowlist gate: the metadata-only contract has exactly the
    failure-diagnostic switches and the two label outputs, so any other key -
    including a comment/issue/discussion/PR-write safe output - fails closed
    instead of compiling green.
    """
    safe = key_entry(fm, "safe-outputs", 0)
    if safe is None:
        return None
    body = [
        line
        for line in safe[1]
        if line.strip() and not line.lstrip().startswith("#")
    ]
    if not body:
        return []
    child_indent = min(len(line) - len(line.lstrip(" ")) for line in body)
    keys: list[str] = []
    for line in body:
        if len(line) - len(line.lstrip(" ")) != child_indent:
            continue
        stripped = line.strip()
        if ":" not in stripped:
            continue
        key = stripped.split(":", 1)[0].strip()
        keys.append(_unquote(key))
    return keys


def check_triage_sources() -> None:
    """Enforce the metadata-only capability surface on every triage source."""
    for md in WORKFLOWS:
        path = os.path.join(WORKFLOW_DIR, md)
        if not os.path.exists(path):
            continue  # check_workflow_imports already reported the missing source
        text = read(path)
        rel_path = rel(path)
        fm = frontmatter(text)
        if not fm:
            errors.append(f"{rel_path} has no YAML frontmatter")
            continue

        if not re.search(r"^\s*checkout\s*:\s*false\s*$", fm, re.MULTILINE):
            errors.append(f"{rel_path} must set 'checkout: false' (metadata-only workflow)")
        for key in ("bash", "cli-proxy"):
            if not re.search(
                r"^\s*" + re.escape(key) + r"\s*:\s*false\s*$", fm, re.MULTILINE
            ):
                errors.append(f"{rel_path} must set '{key}: false'")
        if not re.search(r"^\s*status-comment\s*:\s*false\s*$", fm, re.MULTILINE):
            errors.append(f"{rel_path} must set 'status-comment: false'")
        if not re.search(
            r"^\s*report-failure-as-issue\s*:\s*false\s*$", fm, re.MULTILINE
        ):
            errors.append(f"{rel_path} must set 'report-failure-as-issue: false'")
        if not re.search(
            r"^\s*report-failed-jobs\s*:\s*false\s*$", fm, re.MULTILINE
        ):
            errors.append(
                f"{rel_path} must set 'report-failed-jobs: false' "
                "(no non-agent failure-issue diagnostics)"
            )
        if not re.search(r"^\s*edit\s*:\s*false\s*$", fm, re.MULTILINE):
            errors.append(
                f"{rel_path} must set 'edit: false' (no filesystem write capability)"
            )
        if re.search(r"^\s*add-comment\s*:", fm, re.MULTILINE):
            errors.append(
                f"{rel_path} must not declare an 'add-comment:' key (silent workflow)"
            )

        keys = source_safe_output_keys(fm)
        if keys is None:
            errors.append(
                f"{rel_path} must declare a safe-outputs block with only the "
                "metadata-only key family: "
                + ", ".join(ALLOWED_SOURCE_SAFE_OUTPUT_KEYS)
            )
        else:
            safe = key_entry(fm, "safe-outputs", 0)
            if safe is not None and safe[0].strip():
                errors.append(
                    f"{rel_path} must use a block mapping for safe-outputs (the "
                    "inline flow/scalar form cannot be checked against the "
                    "metadata-only key allowlist)"
                )
            for key in keys:
                if key in ALLOWED_SOURCE_SAFE_OUTPUT_KEYS:
                    continue
                if key in FORBIDDEN_SOURCE_SAFE_OUTPUT_KEYS:
                    errors.append(
                        f"{rel_path} must not declare the write-capable "
                        f"safe-output '{key}' (metadata-only contract allows only "
                        + ", ".join(ALLOWED_SOURCE_SAFE_OUTPUT_KEYS)
                        + ")"
                    )
                else:
                    errors.append(
                        f"{rel_path} declares unexpected safe-output key '{key}' "
                        "(the metadata-only allowlist is exactly: "
                        + ", ".join(ALLOWED_SOURCE_SAFE_OUTPUT_KEYS)
                        + ")"
                    )

        tools = key_entry(fm, "tools", 0)
        github = key_entry("\n".join(tools[1]), "github", 2) if tools else None
        github_body = "\n".join(github[1]) if github else ""
        if not github_body:
            errors.append(f"{rel_path} has no github tools block")
        else:
            toolsets_entry = key_entry(github_body, "toolsets", 4)
            if toolsets_entry is None:
                errors.append(
                    f"{rel_path} must declare toolsets in the github tools block"
                )
            else:
                toolsets = string_list(toolsets_entry[0], toolsets_entry[1])
                if toolsets != list(REQUIRED_GITHUB_TOOLSETS):
                    errors.append(
                        f"{rel_path} toolsets must be exactly "
                        f"[{', '.join(REQUIRED_GITHUB_TOOLSETS)}]; got "
                        f"[{', '.join(toolsets)}]. The 'pull_requests' toolset is "
                        "forbidden: the pinned github-mcp-server advertises its tools "
                        "in full (pull_request_read with get_diff/get_files, "
                        "list_pull_requests) and the CLI does not enforce the "
                        "declared 'allowed' narrowing against toolset-exposed tools, "
                        "so the capability would be real."
                    )
            declared = re.search(
                r"^\s*allowed-repos\s*:\s*(.+?)\s*$", github_body, re.MULTILINE
            )
            if not declared:
                errors.append(
                    f"{rel_path} must declare allowed-repos in the github tools block"
                )
            else:
                entries = repo_scope_entries(declared.group(1))
                if entries != [REPO_SCOPE_EXPRESSION]:
                    errors.append(
                        f"{rel_path} allowed-repos must be exactly the array "
                        f"['{REPO_SCOPE_EXPRESSION}'] (runtime-accepted declared "
                        "repository scope; the scalar owner/repo form is rejected "
                        f"by the gateway); got '{declared.group(1).strip()}'"
                    )
            level = re.search(
                r"^\s*min-integrity\s*:\s*(.+?)\s*$", github_body, re.MULTILINE
            )
            if not level:
                errors.append(
                    f"{rel_path} must declare min-integrity in the github tools block"
                )
            else:
                value = _unquote(level.group(1))
                if value not in MIN_INTEGRITY_LEVELS:
                    errors.append(
                        f"{rel_path} min-integrity must be one of "
                        + ", ".join(MIN_INTEGRITY_LEVELS)
                        + f"; got '{value}'"
                    )
                elif value != INTENDED_MIN_INTEGRITY:
                    errors.append(
                        f"{rel_path} min-integrity is '{value}' but the intended "
                        f"level is '{INTENDED_MIN_INTEGRITY}' (capability change "
                        "must be deliberate and reflected here)"
                    )
            allowed = key_entry(github_body, "allowed", 4)
            exposed = set()
            if allowed is not None:
                for item in string_list(allowed[0], allowed[1]):
                    for tool in FORBIDDEN_GITHUB_TOOLS:
                        if re.search(r"\b" + tool + r"\b", item):
                            exposed.add(tool)
            if exposed:
                errors.append(
                    f"{rel_path} github allowed list exposes forbidden tool(s): "
                    + ", ".join(sorted(exposed))
                )

        lists = triage_source_label_lists(fm)
        for name in ("add-labels", "remove-labels"):
            labels = lists[name]
            if labels is None:
                errors.append(f"{rel_path} must declare safe-outputs.{name}.allowed")
            elif "confirmed" in labels:
                errors.append(
                    f"{rel_path} must not allow 'confirmed' in {name} "
                    "(human/verification-owned)"
                )
        remove = lists["remove-labels"]
        if remove is not None:
            missing = [
                label for label in REQUIRED_REMOVE_LABELS if label not in remove
            ]
            if missing:
                errors.append(
                    f"{rel_path} remove-labels allowed list must include the shared "
                    "type family; missing: " + ", ".join(missing)
                )

        args = engine_args(fm)
        joined = " ".join(args or [])
        if args is None or not any(
            args[index] == REQUIRED_ENGINE_ARGS[0]
            and args[index + 1: index + 2] == [REQUIRED_ENGINE_ARGS[1]]
            for index in range(len(args) - 1)
        ):
            errors.append(
                f"{rel_path} must declare the engine-level shell denial "
                f"'{LOCK_DENY_SHELL}' in engine.args (tools.bash: false alone does "
                "not stop the Copilot CLI's own approval gate from running "
                f"read-only commands); got '{joined}'"
            )

        if not add_labels_issue_intent_false(fm):
            errors.append(
                f"{rel_path} must set 'issue-intent: false' under "
                "safe-outputs.add-labels (a suggested label is routed to pending "
                "review instead of being applied)"
            )

        if DIRECT_LABEL_INSTRUCTION not in text:
            errors.append(
                f"{rel_path} must instruct the agent to request label changes "
                f"directly ('{DIRECT_LABEL_INSTRUCTION}' ...); suggesting a label is "
                "a silent no-op for this deployment"
            )


def job_section(text: str, job: str) -> str | None:
    """Return the block of a workflow job, from its header to the next job."""
    lines = text.splitlines()
    header = re.compile(r"^  " + re.escape(job) + r"\s*:\s*$")
    start = None
    for index, line in enumerate(lines):
        if header.match(line):
            start = index
            break
    if start is None:
        return None
    for index in range(start + 1, len(lines)):
        if re.match(r"^  [A-Za-z0-9_-]+\s*:\s*$", lines[index]):
            return "\n".join(lines[start:index])
    return "\n".join(lines[start:])


def guard_allow_only(text: str) -> str | None:
    """Return the body of the 'guard-policies' block that declares 'allow-only'.

    The JSON blobs are pretty-printed at fixed indentation, so the block ends
    at the next non-blank line indented no deeper than the 'guard-policies' key.
    """
    lines = text.splitlines()
    pattern = re.compile(r'^(\s*)"guard-policies"\s*:\s*\{\s*$')
    for index, line in enumerate(lines):
        match = pattern.match(line)
        if not match:
            continue
        indent = len(match.group(1))
        body = []
        for following in lines[index + 1:]:
            if not following.strip():
                body.append(following)
                continue
            if len(following) - len(following.lstrip(" ")) <= indent:
                break
            body.append(following)
        blob = "\n".join(body)
        if '"allow-only"' in blob:
            return blob
    return None


def lock_grant_tokens(text: str) -> set[str]:
    """Agent-visible github tool names from the real lock grant surfaces.

    Surfaces: `github(<tool>)` allow-tool flags and the compiled gh-aw-manifest
    mcp_servers 'github' tools list. The whole-file text is deliberately not
    scanned because the inlined shared contract quotes forbidden tool names in
    prohibition prose.
    """
    tokens: set[str] = set()
    for match in GITHUB_GRANT.finditer(text):
        tokens.update(piece for piece in re.split(r"[,\s]+", match.group(1)) if piece)
    manifest = LOCK_MANIFEST_GITHUB.search(text)
    if manifest:
        tokens.update(
            piece for piece in re.split(r"[,\s\"]+", manifest.group(1)) if piece
        )
    return tokens


def lock_safe_output_lists(text: str) -> dict[str, dict[str, list[str] | None] | None]:
    """Parse the safe-output label lists out of the compiled lock config JSON.

    Each config value is a double-quoted YAML scalar holding JSON, so it is
    decoded twice. Keys absent from the lock are omitted; a key present with an
    unparsable value maps to None so the caller reports it.
    """
    parsed: dict[str, dict[str, list[str] | None] | None] = {}
    for key in LOCK_CONFIG_KEYS:
        match = re.search(
            r"^\s*" + re.escape(key) + r':\s*(".*")\s*$', text, re.MULTILINE
        )
        if not match:
            continue
        try:
            config = json.loads(json.loads(match.group(1)))
        except (TypeError, ValueError):
            parsed[key] = None
            continue
        entry: dict[str, list[str] | None] = {}
        for name in ("add-labels", "remove-labels"):
            section = config.get(name.replace("-", "_"))
            allowed = section.get("allowed") if isinstance(section, dict) else None
            entry[name] = (
                [str(item) for item in allowed] if isinstance(allowed, list) else None
            )
        parsed[key] = entry
    return parsed


def lock_issue_intent_disabled(text: str) -> bool:
    """True when the compiled safe-output config disables add_labels issue-intent.

    The compiler serializes `safe-outputs.add-labels.issue-intent: false` into the
    handler config as `"issue_intent":false`; the runtime generator then strips the
    rationale/confidence/suggest fields from the exposed add_labels schema. Both the
    safe-outputs config and the handler config must carry it.
    """
    found = 0
    for key in LOCK_CONFIG_KEYS:
        match = re.search(
            r"^\s*" + re.escape(key) + r':\s*(".*")\s*$', text, re.MULTILINE
        )
        if not match:
            continue
        try:
            config = json.loads(json.loads(match.group(1)))
        except (TypeError, ValueError):
            continue
        section = config.get("add_labels")
        if isinstance(section, dict) and section.get("issue_intent") is False:
            found += 1
    return found == len(LOCK_CONFIG_KEYS)


def lock_config_key_sets(text: str) -> dict[str, list[str] | None]:
    """Return the compiled safe-output config key set(s), or None when unparsable.

    Both GH_AW_SAFE_OUTPUTS_CONFIG and GH_AW_SAFE_OUTPUTS_HANDLER_CONFIG are parsed;
    a key present with an unparsable value maps to None so the caller reports it,
    and a key absent from the lock is simply omitted (the caller reports that too).
    """
    parsed: dict[str, list[str] | None] = {}
    for key in LOCK_CONFIG_KEYS:
        match = re.search(
            r"^\s*" + re.escape(key) + r':\s*(".*")\s*$', text, re.MULTILINE
        )
        if not match:
            continue
        try:
            config = json.loads(json.loads(match.group(1)))
        except (TypeError, ValueError):
            parsed[key] = None
            continue
        parsed[key] = sorted(config.keys()) if isinstance(config, dict) else None
    return parsed


def lock_server_blocks(text: str, name: str) -> list[str]:
    """Return the bodies of every pretty-printed '<name>': { ... } JSON block.

    The lock's MCP config is emitted at fixed indentation, so a block ends at the
    next non-blank line indented no deeper than its key. Returning every match
    (not just the first) lets the caller fail closed on a duplicate server entry,
    which a JSON parser would otherwise silently resolve last-wins.
    """
    lines = text.splitlines()
    pattern = re.compile(r'^(\s*)"' + re.escape(name) + r'"\s*:\s*\{\s*$')
    blocks: list[str] = []
    for index, line in enumerate(lines):
        match = pattern.match(line)
        if not match:
            continue
        indent = len(match.group(1))
        body = []
        for following in lines[index + 1:]:
            if not following.strip():
                body.append(following)
                continue
            if len(following) - len(following.lstrip(" ")) <= indent:
                break
            body.append(following)
        blocks.append("\n".join(body))
    return blocks


def check_lock_safe_output_surface(lock_rel: str, text: str) -> None:
    """Assert the compiled safe-output surface is exactly the read-only label family.

    Three independent surfaces are checked, because any one of them can widen the
    agent-visible write capability:
      - the gh-aw-manifest mcp_servers 'safeoutputs' tools list (the compiled tool
        surface; the agent job carries a single '--allow-tool safeoutputs' grant, so
        the manifest list is the whole agent-visible safe-output surface);
      - the GH_AW_SAFE_OUTPUTS_CONFIG and GH_AW_SAFE_OUTPUTS_HANDLER_CONFIG key sets
        (the enabled handler set; the pinned compiler always adds the two built-in
        incomplete-run diagnostics, which are enabled by default with safe-outputs);
      - the github server env GITHUB_READ_ONLY flag (read-only server mode) and the
        safeoutputs write-sink 'accept' list (must stay confined to the private form
        of the repository expression).
    """
    manifests = LOCK_MANIFEST_SAFEOUTPUTS.findall(text)
    if len(manifests) != 1:
        errors.append(
            f"{lock_rel} must carry exactly one gh-aw-manifest 'safeoutputs' tools "
            f"entry; found {len(manifests)}"
        )
    else:
        tools = sorted(
            piece
            for piece in re.split(r"[,\s\"]+", manifests[0])
            if piece
        )
        if tools != sorted(REQUIRED_LOCK_SAFE_OUTPUT_TOOLS):
            errors.append(
                f"{lock_rel} compiled safe-output tool set must be exactly "
                f"[{', '.join(REQUIRED_LOCK_SAFE_OUTPUT_TOOLS)}]; got "
                f"[{', '.join(tools)}] (a comment/issue/discussion/PR-write "
                "handler or tool must not be agent-visible)"
            )

    parsed = lock_config_key_sets(text)
    for key in LOCK_CONFIG_KEYS:
        if key not in parsed:
            errors.append(f"{lock_rel} has no {key}")
            continue
        keys = parsed[key]
        if keys is None:
            errors.append(f"{lock_rel} {key} is not a valid JSON config")
            continue
        unexpected = sorted(set(keys) - set(ALLOWED_LOCK_SAFE_OUTPUT_CONFIG_KEYS))
        missing = sorted(set(ALLOWED_LOCK_SAFE_OUTPUT_CONFIG_KEYS) - set(keys))
        if unexpected:
            errors.append(
                f"{lock_rel} {key} carries unexpected safe-output handler(s): "
                + ", ".join(unexpected)
                + " (only the read-only label family plus the built-in "
                "incomplete-run diagnostics may be enabled)"
            )
        if missing:
            errors.append(
                f"{lock_rel} {key} is missing safe-output handler(s): "
                + ", ".join(missing)
            )

    github_blocks = lock_server_blocks(text, "github")
    if len(github_blocks) != 1:
        errors.append(
            f"{lock_rel} must carry exactly one 'github' MCP server block; found "
            f"{len(github_blocks)} (a duplicate entry could widen the surface)"
        )
    elif not re.search(r'"GITHUB_READ_ONLY"\s*:\s*"1"', github_blocks[0]):
        errors.append(
            f'{lock_rel} must carry GITHUB_READ_ONLY: "1" in the github server env '
            "(the pinned github MCP server would otherwise expose write tools)"
        )

    safeoutputs_blocks = lock_server_blocks(text, "safeoutputs")
    if len(safeoutputs_blocks) != 1:
        errors.append(
            f"{lock_rel} must carry exactly one 'safeoutputs' MCP server block; "
            f"found {len(safeoutputs_blocks)}"
        )
    else:
        sinks = re.findall(
            r'"accept"\s*:\s*\[(.*?)\]', safeoutputs_blocks[0], re.DOTALL
        )
        if len(sinks) != 1:
            errors.append(
                f"{lock_rel} must carry exactly one safeoutputs write-sink 'accept' "
                f"list; found {len(sinks)}"
            )
        else:
            entries = sorted(
                piece.strip().strip("\"'")
                for piece in sinks[0].split(",")
                if piece.strip()
            )
            if entries != [SAFE_OUTPUTS_SINK_ACCEPT]:
                errors.append(
                    f"{lock_rel} safeoutputs write-sink 'accept' must be exactly "
                    f"['{SAFE_OUTPUTS_SINK_ACCEPT}']; got {entries} (a wildcard, 'all', "
                    "or 'public' sink admits writes outside the current repository)"
                )


def check_lock_currency(md: str, lock_text: str, lock_rel: str) -> None:
    """Lock currency without `gh aw`: shared pin + safe-output label lists.

    The strongest textual proxy for compile currency is compared against the
    source: the shared import SHA(s) embedded in the lock header and the exact
    allowed add/remove label names in the compiled safe-output config. Label
    order is not compared (YAML arrays are unordered for this purpose); set
    equality still catches added, removed, or renamed labels. General compile
    currency (prompt text, tool schemas, job wiring) cannot be proven without
    running `gh aw compile`.
    """
    src_path = os.path.join(WORKFLOW_DIR, md)
    if not os.path.exists(src_path):
        return  # check_workflow_imports already reported the missing source
    src = read(src_path)

    src_pins = set(SHARED_REF_SHA.findall(src))
    lock_pins = set(SHARED_REF_SHA.findall(lock_text))
    if src_pins != lock_pins:
        details = []
        if src_pins - lock_pins:
            details.append("missing " + ", ".join(sorted(src_pins - lock_pins)))
        if lock_pins - src_pins:
            details.append("unexpected " + ", ".join(sorted(lock_pins - src_pins)))
        errors.append(
            f"{lock_rel} shared import pin drift vs {rel(src_path)}: "
            + ("; ".join(details) if details else "no shared pin found")
        )

    source_lists = triage_source_label_lists(frontmatter(src))
    parsed = lock_safe_output_lists(lock_text)
    if "GH_AW_SAFE_OUTPUTS_CONFIG" not in parsed:
        errors.append(f"{lock_rel} has no GH_AW_SAFE_OUTPUTS_CONFIG")
    for key, entry in sorted(parsed.items()):
        if entry is None:
            errors.append(f"{lock_rel} {key} is not a valid JSON config")
            continue
        for name in ("add-labels", "remove-labels"):
            lock_labels = entry.get(name)
            source_labels = source_lists.get(name)
            if source_labels is None:
                continue  # source-side gap is reported by check_triage_sources
            if lock_labels is None:
                errors.append(f"{lock_rel} {key} is missing the {name} allowed list")
                continue
            if set(lock_labels) != set(source_labels):
                missing = sorted(set(source_labels) - set(lock_labels))
                extra = sorted(set(lock_labels) - set(source_labels))
                details = []
                if missing:
                    details.append("missing " + ", ".join(missing))
                if extra:
                    details.append("unexpected " + ", ".join(extra))
                errors.append(
                    f"{lock_rel} safe-output label drift in {name} vs {rel(src_path)}: "
                    + "; ".join(details)
                )


def check_triage_locks() -> None:
    """Enforce the compiled triage invariants and lock currency."""
    for md in WORKFLOWS:
        lock_name = md[: -len(".md")] + LOCK_SUFFIX
        path = os.path.join(WORKFLOW_DIR, lock_name)
        lock_rel = rel(path)
        if not os.path.exists(path):
            errors.append(f"missing lock file {lock_rel}")
            continue
        text = read(path)

        if "add_comment" in text:
            errors.append(f"{lock_rel} must not contain add_comment (silent workflow)")
        exposed = lock_grant_tokens(text) & set(FORBIDDEN_GITHUB_TOOLS)
        if exposed:
            errors.append(
                f"{lock_rel} exposes forbidden github tool(s): "
                + ", ".join(sorted(exposed))
            )

        check_lock_safe_output_surface(lock_rel, text)

        # The toolset selection is the capability root: the pinned github-mcp-server
        # advertises every tool of a requested toolset regardless of the per-tool
        # allow-tool grants, so the compiled GITHUB_TOOLSETS value must be exactly the
        # intended set.
        toolsets = LOCK_TOOLSETS.findall(text)
        if len(toolsets) != 1:
            errors.append(
                f"{lock_rel} must carry exactly one GITHUB_TOOLSETS declaration; "
                f"found {len(toolsets)}"
            )
        else:
            requested = [piece.strip() for piece in toolsets[0].split(",") if piece.strip()]
            if requested != list(REQUIRED_GITHUB_TOOLSETS):
                errors.append(
                    f"{lock_rel} GITHUB_TOOLSETS must be exactly "
                    f"'{','.join(REQUIRED_GITHUB_TOOLSETS)}'; got '{toolsets[0]}' "
                    "(the 'pull_requests' toolset exposes pull_request_read/"
                    "list_pull_requests regardless of the declared allowed list)"
                )

        guard = guard_allow_only(text)
        if guard is None:
            errors.append(
                f"{lock_rel} has no github guard policy with an allow-only 'repos' scope"
            )
        else:
            level = re.search(r'"min-integrity"\s*:\s*"([^"]*)"', guard)
            if not level:
                errors.append(
                    f"{lock_rel} guard policy must declare a non-empty min-integrity"
                )
            elif level.group(1) not in MIN_INTEGRITY_LEVELS:
                errors.append(
                    f"{lock_rel} guard policy min-integrity must be one of "
                    + ", ".join(MIN_INTEGRITY_LEVELS)
                    + f"; got '{level.group(1)}'"
                )
            elif level.group(1) != INTENDED_MIN_INTEGRITY:
                errors.append(
                    f"{lock_rel} guard policy min-integrity is "
                    f"'{level.group(1)}' but the intended level is "
                    f"'{INTENDED_MIN_INTEGRITY}'"
                )
            repos = re.search(r'"repos"\s*:\s*(\[.*?\])', guard, re.DOTALL)
            if not repos:
                errors.append(
                    f"{lock_rel} guard policy must scope 'repos' to the repository "
                    "as a JSON array (['${{ github.repository }}']); the scalar "
                    "string form is rejected by the pinned gateway at runtime"
                )
            else:
                entries = repo_scope_entries(repos.group(1))
                if entries != [REPO_SCOPE_EXPRESSION]:
                    errors.append(
                        f"{lock_rel} guard policy 'repos' must be exactly the array "
                        f"['{REPO_SCOPE_EXPRESSION}']; got '{repos.group(1)}'"
                    )

        if not re.search(r'GH_AW_FAILURE_REPORT_AS_ISSUE\s*:\s*"false"', text):
            errors.append(
                f'{lock_rel} must set GH_AW_FAILURE_REPORT_AS_ISSUE: "false"'
            )

        if re.search(r'GH_AW_REPORT_FAILED_JOBS\s*:', text) or "report_failed_jobs" in text:
            errors.append(
                f"{lock_rel} must not contain the report-failed-jobs machinery "
                "(non-agent failure diagnostics disabled)"
            )

        agent = job_section(text, "agent")
        if agent is None:
            errors.append(f"{lock_rel} has no 'agent:' job")
        elif "actions/checkout" in agent:
            errors.append(
                f"{lock_rel} agent job must not check out the repository "
                "(actions/checkout found)"
            )
        else:
            for flag in ("--allow-tool write", "--allow-all-paths"):
                if flag in agent:
                    errors.append(
                        f"{lock_rel} agent job must not grant '{flag}' "
                        "(residual filesystem/write capability)"
                    )
            if f"--deny-tool {REQUIRED_ENGINE_ARGS[1]}" not in agent:
                errors.append(
                    f"{lock_rel} agent invocation must carry the engine-level shell "
                    f"denial '{LOCK_DENY_SHELL}' (tools.bash: false alone does not stop "
                    "the Copilot CLI's own approval gate)"
                )

        if "GH_AW_SAFE_OUTPUTS_STAGED" in text:
            errors.append(
                f"{lock_rel} must not contain GH_AW_SAFE_OUTPUTS_STAGED "
                "(live safe-output mode required)"
            )

        check_lock_currency(md, text, lock_rel)

        if "tool-call-limits" not in text:
            warnings.append(
                f"{lock_rel} has no tool-call-limits; gh-aw v0.89.21 drops max-calls "
                "at compile time, so declared per-tool call limits are not enforced"
            )

        if not lock_issue_intent_disabled(text):
            errors.append(
                f"{lock_rel} must carry add_labels issue_intent:false in both "
                "safe-output configs (label suggestion routing must be off)"
            )


def main() -> int:
    check_workflow_imports()
    check_triage_sources()
    check_triage_locks()
    if os.path.exists(POLICY):
        check_contract_files()
        check_labels()
    else:
        errors.append(f"missing policy file {rel(POLICY)}")
    check_vendored_imports()

    for warning in warnings:
        print(f"warning: {warning}")
    for error in errors:
        print(f"error: {error}")

    if errors:
        print(f"\ncontract validation FAILED with {len(errors)} error(s)")
        return 1
    print("\ncontract validation passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
