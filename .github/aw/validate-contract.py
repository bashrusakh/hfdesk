#!/usr/bin/env python3
"""Deterministic contract validation for the hfdesk agentic triage contract.

No LLM. Checks, in order:
  1. every agentic workflow source imports the shared invariant and its core,
     pinned to the exact shared-package SHA;
  2. each triage workflow source declares the metadata-only capability surface:
     checkout/status-comment/bash/cli-proxy/report-failure-as-issue pinned false,
     no add-comment, an allowed-repos + min-integrity scoped github surface without
     source/diff tools, no 'confirmed' in allowed label lists, and the required
     type family in remove-labels;
  3. each generated lock preserves those invariants (no add_comment, no forbidden
     github tool grants, scoped allow-only guard policy, failure reports disabled,
     no agent-job checkout, not staged) and stays structurally in sync with its
     source (shared import pin + exact safe-output label lists);
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
github tool names are matched on the real agent-visible grant surfaces (github(<tool>)
allow-tool flags and the gh-aw-manifest mcp_servers 'github' tools list), not the whole
file, because the inlined shared contract quotes some of those names in prohibition
prose; the add_comment scan is whole-file and fail-closed. Lock currency compares the
shared import pin(s) and the exact add-labels/remove-labels allowed lists from the
GH_AW_SAFE_OUTPUTS_CONFIG / GH_AW_SAFE_OUTPUTS_HANDLER_CONFIG JSON blobs; it cannot
prove general compile currency (prompt text, tool schemas, job wiring) without running
`gh aw compile`.

Set VALIDATE_CONTRACT_SKIP_LIVE_LABELS=1 to skip the live repository label lookup
(offline/test mode used by .github/aw/test_validate_contract.py); in that mode
.github/labels.yml must declare every managed label because the live set is not read.
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
)

# Universal semantic type family every triage workflow must be able to remove
# (reconciliation capability: replace a clearly wrong managed type).
REQUIRED_REMOVE_LABELS = ("bug", "enhancement", "documentation", "question", "refactor", "ci")

SKIP_LIVE_LABELS_ENV = "VALIDATE_CONTRACT_SKIP_LIVE_LABELS"
LOCK_SUFFIX = ".lock.yml"

GITHUB_GRANT = re.compile(r"github\(([^)]*)\)")
LOCK_MANIFEST_GITHUB = re.compile(r'"name"\s*:\s*"github"\s*,\s*"tools"\s*:\s*\[([^\]]*)\]')
SHARED_REF_SHA = re.compile(r"repo-docs-sync/[^\s@\"']+@([0-9a-f]{40})")
LOCK_CONFIG_KEYS = ("GH_AW_SAFE_OUTPUTS_CONFIG", "GH_AW_SAFE_OUTPUTS_HANDLER_CONFIG")

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


def live_labels() -> set[str]:
    if os.environ.get(SKIP_LIVE_LABELS_ENV) == "1":
        return set()
    repo = os.environ.get("GH_REPO", "")
    cmd = ["gh", "label", "list", "--limit", "200"]
    if repo:
        cmd += ["--repo", repo]
    try:
        out = subprocess.run(
            cmd, capture_output=True, text=True, timeout=60, check=True
        ).stdout
    except Exception as exc:  # noqa: BLE001 - report and degrade to declared-only
        warnings.append(f"could not read live labels ({exc}); checking declared file only")
        return set()
    names = set()
    for line in out.splitlines():
        if line.strip():
            names.add(line.split("\t", 1)[0].strip())
    return names


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
    live = live_labels()
    known = declared | live

    for label in sorted(managed):
        if label in known:
            if label not in declared:
                warnings.append(
                    f"managed label '{label}' exists live but is missing from "
                    f"{rel(LABELS_YML)} (reference-set drift)"
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
        if re.search(r"^\s*add-comment\s*:", fm, re.MULTILINE):
            errors.append(
                f"{rel_path} must not declare an 'add-comment:' key (silent workflow)"
            )

        tools = key_entry(fm, "tools", 0)
        github = key_entry("\n".join(tools[1]), "github", 2) if tools else None
        github_body = "\n".join(github[1]) if github else ""
        if not github_body:
            errors.append(f"{rel_path} has no github tools block")
        else:
            if not re.search(r"^\s*allowed-repos\s*:\s*\S", github_body, re.MULTILINE):
                errors.append(
                    f"{rel_path} must declare allowed-repos in the github tools block"
                )
            if not re.search(r"^\s*min-integrity\s*:\s*\S", github_body, re.MULTILINE):
                errors.append(
                    f"{rel_path} must declare min-integrity in the github tools block"
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

        guard = guard_allow_only(text)
        if guard is None:
            errors.append(
                f"{lock_rel} has no github guard policy with an allow-only 'repos' scope"
            )
        else:
            if not re.search(r'"min-integrity"\s*:\s*"[^"]+"', guard):
                errors.append(
                    f"{lock_rel} guard policy must declare a non-empty min-integrity"
                )
            repos = re.search(r'"repos"\s*:\s*"([^"]+)"', guard)
            if not repos:
                errors.append(
                    f"{lock_rel} guard policy must scope 'repos' to the repository"
                )
            elif not (
                re.fullmatch(r"\$\{\{\s*github\.repository\s*\}\}", repos.group(1))
                or re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repos.group(1))
            ):
                errors.append(
                    f"{lock_rel} guard policy 'repos' must be the repository expression "
                    f"or owner/repo form; got '{repos.group(1)}'"
                )

        if not re.search(r'GH_AW_FAILURE_REPORT_AS_ISSUE\s*:\s*"false"', text):
            errors.append(
                f'{lock_rel} must set GH_AW_FAILURE_REPORT_AS_ISSUE: "false"'
            )

        agent = job_section(text, "agent")
        if agent is None:
            errors.append(f"{lock_rel} has no 'agent:' job")
        elif "actions/checkout" in agent:
            errors.append(
                f"{lock_rel} agent job must not check out the repository "
                "(actions/checkout found)"
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
