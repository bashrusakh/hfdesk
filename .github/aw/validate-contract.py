#!/usr/bin/env python3
"""Deterministic contract validation for the hfdesk agentic triage contract.

No LLM. Checks, in order:
  1. every agentic workflow source imports the shared invariant and its core,
     pinned to the exact shared-package SHA;
  2. every contract file referenced by .github/triage-policy.md exists;
  3. every workflow-managed label named by the policy currently exists
     (declared in .github/labels.yml and/or present in the live repository);
  4. the pinned shared import is vendored under .github/aw/imports/** so a
     runtime/compile never needs to read the private sibling repository.

A referenced *managed* label that exists in neither the declared label file nor the
live repository is an error (removed/renamed managed label). A managed label that is
live but missing from the declared reference file is a warning: the reference file is
a setup aid and may lag the authoritative live label set.
"""

from __future__ import annotations

import glob
import json
import os
import re
import subprocess
import sys

PIN = "3d8b4095371aee673d32cd265b26b11ae23a62b7"
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
        ".github/aw/scripts/fetch-policy-contract.sh",
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


def main() -> int:
    check_workflow_imports()
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
