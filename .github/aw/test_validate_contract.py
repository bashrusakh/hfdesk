#!/usr/bin/env python3
"""Unit tests for .github/aw/validate-contract.py (stdlib only, no network).

The validator is loaded as a module from a temporary copy of the repository's
contract surface, so its REPO_ROOT resolves to the copy and every mutation in a
test is discarded. The copy contains:

  - the full .github/ tree (workflows, triage-policy.md, labels.yml, vendored
    imports, the validator itself);
  - AGENTS.md and CONTRIBUTING.md from the repository root.

.github/labels.yml in the fixture is augmented with the managed labels that
exist live but are not yet declared there (enhancement, documentation, question,
refactor, ci). Tests run with VALIDATE_CONTRACT_SKIP_LIVE_LABELS=1, so the live
`gh label list` lookup is skipped and the declared file must be self-sufficient;
without the augmentation the baseline copy could not pass offline. No `gh`
process is ever started.

Run:
  python3 .github/aw/test_validate_contract.py
  python3 -m unittest discover -s .github/aw -t .github/aw -p 'test_validate_contract.py' -v

The dotted-path form `python3 -m unittest .github/aw/test_validate_contract.py` does
not work: unittest parses that argument as an importable module name, and ".github"
is not a valid Python package path.
"""

from __future__ import annotations

import contextlib
import importlib.util
import io
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
REAL_ROOT = os.path.dirname(os.path.dirname(HERE))
VALIDATOR_REL = os.path.join(".github", "aw", "validate-contract.py")
SKIP_LIVE_LABELS = "VALIDATE_CONTRACT_SKIP_LIVE_LABELS"

ISSUE_LOCK = ".github/workflows/issue-triage.lock.yml"
PR_LOCK = ".github/workflows/pr-semantic-intake.lock.yml"
BACKLOG_LOCK = ".github/workflows/backlog-retriage.lock.yml"
ISSUE_MD = ".github/workflows/issue-triage.md"
PR_MD = ".github/workflows/pr-semantic-intake.md"
BACKLOG_MD = ".github/workflows/backlog-retriage.md"

PIN = "e2de4a989077b7fdf558ef5656040dc2e547e674"
ZERO_SHA = "0" * 40

# The runtime-accepted allowed-repos shapes: the scalar string form is rejected
# by the pinned gateway, so tests mutate from the array form and assert the
# scalar / widened / literal-owner-repo alternatives all fail.
SOURCE_REPO_SCOPE_ARRAY = '    allowed-repos: ["${{ github.repository }}"]\n'
SOURCE_REPO_SCOPE_SCALAR = '    allowed-repos: "${{ github.repository }}"\n'
LOCK_REPOS_ARRAY_BLOCK = (
    '"repos": [\n                      "${{ github.repository }}"\n                    ],'
)

# The engine-level shell denial: asserted as the adjacent pair in engine.args and as
# the compiled CLI flag in the agent invocation.
SOURCE_ENGINE_ARGS = '  args: ["--deny-tool", "shell"]\n'
LOCK_DENY_SHELL = "--deny-tool shell"

# The github toolset selection: the capability root. The 'pull_requests' toolset makes
# the pinned server advertise pull_request_read (get_diff/get_files) and
# list_pull_requests regardless of the declared per-tool allow-tool narrowing.
SOURCE_TOOLSETS_ISSUES = "    toolsets: [issues]\n"
SOURCE_TOOLSETS_WITH_PRS = "    toolsets: [issues, pull_requests]\n"
LOCK_TOOLSETS_ISSUES = '"GITHUB_TOOLSETS": "issues"'
LOCK_TOOLSETS_WITH_PRS = '"GITHUB_TOOLSETS": "issues,pull_requests"'

# The direct-label instruction in the deployment prompt bodies (a suggested label is
# routed to pending review instead of applied, so it must never be used here).
DIRECT_LABEL_INSTRUCTION = "never attach `suggest`, `rationale`, or `confidence`"

# The config-level disable of label intent metadata on the add-labels safe output.
# Lock configs embed JSON inside a YAML scalar, so the JSON quotes are backslash-escaped.
SOURCE_ISSUE_INTENT_FALSE = "    issue-intent: false\n"
LOCK_ISSUE_INTENT_FALSE = '\\"issue_intent\\":false'

# Managed labels the policy expects the declared label file or the live repo to
# provide; the fixture declares them all so the offline run is self-sufficient.
FIXTURE_MANAGED_LABELS = (
    "bug", "enhancement", "documentation", "question",
    "refactor", "ci", "needs-info", "duplicate",
)


def load_validator(path: str):
    """Load validate-contract.py as a module from an explicit path."""
    spec = importlib.util.spec_from_file_location("validate_contract_under_test", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class ContractValidatorTest(unittest.TestCase):
    """Base fixture: a throwaway copy of the contract surface per test."""

    @classmethod
    def setUpClass(cls) -> None:
        cls._baseline_dir = tempfile.mkdtemp(prefix="validate-contract-baseline-")
        root = os.path.join(cls._baseline_dir, "repo")
        shutil.copytree(os.path.join(REAL_ROOT, ".github"), os.path.join(root, ".github"))
        for name in ("AGENTS.md", "CONTRIBUTING.md"):
            shutil.copy2(os.path.join(REAL_ROOT, name), os.path.join(root, name))
        cls._declare_fixture_labels(os.path.join(root, ".github", "labels.yml"))

    @classmethod
    def tearDownClass(cls) -> None:
        shutil.rmtree(cls._baseline_dir, ignore_errors=True)

    @staticmethod
    def _declare_fixture_labels(labels_path: str) -> None:
        with open(labels_path, encoding="utf-8") as fh:
            text = fh.read()
        missing = [
            label for label in FIXTURE_MANAGED_LABELS if f"- name: {label}\n" not in text
        ]
        if missing:
            with open(labels_path, "a", encoding="utf-8") as fh:
                for label in missing:
                    fh.write(f"- name: {label}\n  color: cccccc\n  description: fixture label.\n")

    def setUp(self) -> None:
        tmp = tempfile.mkdtemp(prefix="validate-contract-test-")
        self.addCleanup(shutil.rmtree, tmp, ignore_errors=True)
        self.root = os.path.join(tmp, "repo")
        shutil.copytree(os.path.join(self._baseline_dir, "repo"), self.root)
        self.mod = load_validator(os.path.join(self.root, VALIDATOR_REL))

        previous = os.environ.get(SKIP_LIVE_LABELS)
        os.environ[SKIP_LIVE_LABELS] = "1"

        def restore() -> None:
            if previous is None:
                os.environ.pop(SKIP_LIVE_LABELS, None)
            else:
                os.environ[SKIP_LIVE_LABELS] = previous

        self.addCleanup(restore)

    # -- helpers -----------------------------------------------------------

    def path(self, rel: str) -> str:
        return os.path.join(self.root, rel)

    def read(self, rel: str) -> str:
        with open(self.path(rel), encoding="utf-8") as fh:
            return fh.read()

    def write(self, rel: str, text: str) -> None:
        with open(self.path(rel), "w", encoding="utf-8") as fh:
            fh.write(text)

    def mutate(self, rel: str, old: str, new: str, occurrences: int = 1) -> None:
        """Replace `old` in `rel`, asserting the expected occurrence count."""
        text = self.read(rel)
        self.assertEqual(
            text.count(old), occurrences,
            f"expected {occurrences} occurrence(s) of {old[:60]!r} in {rel}",
        )
        self.write(rel, text.replace(old, new))

    def run_main(self) -> tuple[int, str]:
        """Run validate-contract main() on the fixture; return (code, output)."""
        del self.mod.errors[:]
        del self.mod.warnings[:]
        buffer = io.StringIO()
        with contextlib.redirect_stdout(buffer):
            code = self.mod.main()
        return code, buffer.getvalue()

    def assert_fails(self, expected: str) -> str:
        code, output = self.run_main()
        self.assertEqual(code, 1, f"expected failure; output:\n{output}")
        self.assertIn(expected, output, f"expected {expected!r}; output:\n{output}")
        return output

    # -- positive baseline -------------------------------------------------

    def test_fixture_baseline_passes(self) -> None:
        code, output = self.run_main()
        self.assertEqual(code, 0, output)
        self.assertIn("contract validation passed", output)

    def test_cli_on_temp_copy_passes(self) -> None:
        env = dict(os.environ)
        env[SKIP_LIVE_LABELS] = "1"
        result = subprocess.run(
            [sys.executable, self.path(VALIDATOR_REL)],
            cwd=self.root, env=env, capture_output=True, text=True, timeout=60,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("contract validation passed", result.stdout)

    def test_missing_tool_call_limits_is_warning_only(self) -> None:
        code, output = self.run_main()
        self.assertEqual(code, 0, output)
        self.assertIn("tool-call-limits", output)
        self.assertIn("warning:", output)
        self.assertNotIn("error:", output)

    # -- source (.md) negative cases --------------------------------------

    def test_forbidden_tool_readded_in_source_fails(self) -> None:
        self.mutate(
            ISSUE_MD,
            "      - name: search_issues\n        max-calls: 3\n",
            "      - name: search_issues\n        max-calls: 3\n"
            "      - name: get_pull_request_files\n        max-calls: 1\n",
        )
        self.assert_fails("github allowed list exposes forbidden tool(s): get_pull_request_files")

    def test_confirmed_in_source_remove_labels_fails(self) -> None:
        self.mutate(
            BACKLOG_MD,
            '  remove-labels:\n    allowed: ["bug", "enhancement", "documentation", '
            '"question", "refactor", "ci", "needs-info", "duplicate"]',
            '  remove-labels:\n    allowed: ["bug", "enhancement", "documentation", '
            '"question", "refactor", "ci", "needs-info", "duplicate", "confirmed"]',
        )
        self.assert_fails("must not allow 'confirmed' in remove-labels")

    def test_checkout_false_removed_fails(self) -> None:
        self.mutate(ISSUE_MD, "checkout: false\n", "")
        self.assert_fails("must set 'checkout: false'")

    def test_add_comment_source_key_fails(self) -> None:
        self.mutate(
            BACKLOG_MD,
            "safe-outputs:\n  report-failure-as-issue: false",
            "safe-outputs:\n  add-comment: true\n  report-failure-as-issue: false",
        )
        self.assert_fails("must not declare an 'add-comment:' key")

    def test_bash_false_removed_fails(self) -> None:
        self.mutate(ISSUE_MD, "  bash: false\n", "")
        self.assert_fails("must set 'bash: false'")

    def test_cli_proxy_false_removed_fails(self) -> None:
        self.mutate(PR_MD, "  cli-proxy: false\n", "")
        self.assert_fails("must set 'cli-proxy: false'")

    def test_status_comment_false_removed_fails(self) -> None:
        self.mutate(BACKLOG_MD, "  status-comment: false\n", "")
        self.assert_fails("must set 'status-comment: false'")

    def test_report_failure_as_issue_false_removed_fails(self) -> None:
        self.mutate(PR_MD, "  report-failure-as-issue: false\n", "")
        self.assert_fails("must set 'report-failure-as-issue: false'")

    def test_allowed_repos_missing_fails(self) -> None:
        self.mutate(ISSUE_MD, SOURCE_REPO_SCOPE_ARRAY, "")
        self.assert_fails("must declare allowed-repos in the github tools block")

    def test_allowed_repos_scalar_expression_fails(self) -> None:
        self.mutate(ISSUE_MD, SOURCE_REPO_SCOPE_ARRAY, SOURCE_REPO_SCOPE_SCALAR)
        self.assert_fails("allowed-repos must be exactly the array")

    def test_allowed_repos_other_owner_repo_fails(self) -> None:
        self.mutate(
            ISSUE_MD,
            SOURCE_REPO_SCOPE_ARRAY,
            '    allowed-repos: ["attacker/other-repo"]\n',
        )
        self.assert_fails("allowed-repos must be exactly the array")

    def test_allowed_repos_widened_to_all_fails(self) -> None:
        self.mutate(BACKLOG_MD, SOURCE_REPO_SCOPE_ARRAY, "    allowed-repos: all\n")
        self.assert_fails("allowed-repos must be exactly the array")

    def test_allowed_repos_widened_to_public_fails(self) -> None:
        self.mutate(PR_MD, SOURCE_REPO_SCOPE_ARRAY, "    allowed-repos: public\n")
        self.assert_fails("allowed-repos must be exactly the array")

    def test_allowed_repos_widened_to_wildcard_fails(self) -> None:
        self.mutate(
            PR_MD, SOURCE_REPO_SCOPE_ARRAY, '    allowed-repos: "bashrusakh/*"\n'
        )
        self.assert_fails("allowed-repos must be exactly the array")

    def test_min_integrity_missing_fails(self) -> None:
        self.mutate(ISSUE_MD, "    min-integrity: none\n", "")
        self.assert_fails("must declare min-integrity in the github tools block")

    def test_min_integrity_restrengthened_fails(self) -> None:
        for rel in (ISSUE_MD, PR_MD, BACKLOG_MD):
            with self.subTest(rel=rel):
                self.mutate(rel, "    min-integrity: none\n", "    min-integrity: approved\n")
                self.assert_fails("but the intended level is 'none'")

    def test_min_integrity_unknown_level_fails(self) -> None:
        self.mutate(ISSUE_MD, "    min-integrity: none\n", "    min-integrity: trusted\n")
        self.assert_fails("min-integrity must be one of")

    def test_edit_false_removed_fails(self) -> None:
        self.mutate(ISSUE_MD, "  edit: false\n", "")
        self.assert_fails("must set 'edit: false'")

    def test_report_failed_jobs_flipped_true_fails(self) -> None:
        for rel in (ISSUE_MD, PR_MD, BACKLOG_MD):
            with self.subTest(rel=rel):
                self.mutate(
                    rel,
                    "  report-failed-jobs: false\n",
                    "  report-failed-jobs: true\n",
                )
                self.assert_fails("must set 'report-failed-jobs: false'")

    def test_remove_labels_type_family_incomplete_fails(self) -> None:
        self.mutate(
            PR_MD,
            '  remove-labels:\n    allowed: ["bug", "enhancement", "documentation", '
            '"question", "refactor", "ci", "duplicate"]',
            '  remove-labels:\n    allowed: ["bug", "enhancement", "documentation", '
            '"question", "refactor", "duplicate"]',
        )
        self.assert_fails("remove-labels allowed list must include the shared type family")

    def test_source_pin_drift_fails(self) -> None:
        self.mutate(ISSUE_MD, PIN, ZERO_SHA, occurrences=2)
        self.assert_fails("shared import pin drift")

    # -- engine-level shell denial ----------------------------------------

    def test_engine_deny_shell_removed_fails(self) -> None:
        for rel in (ISSUE_MD, PR_MD, BACKLOG_MD):
            with self.subTest(rel=rel):
                self.mutate(rel, SOURCE_ENGINE_ARGS, "")
                self.assert_fails("must declare the engine-level shell denial")

    def test_engine_args_other_denial_fails(self) -> None:
        self.mutate(
            ISSUE_MD,
            SOURCE_ENGINE_ARGS,
            '  args: ["--deny-tool", "write"]\n',
        )
        self.assert_fails("must declare the engine-level shell denial")

    def test_engine_args_scalar_form_fails(self) -> None:
        # A scalar 'args' value cannot express the flag/value pair, so it must not pass.
        self.mutate(ISSUE_MD, SOURCE_ENGINE_ARGS, '  args: "--deny-tool shell"\n')
        self.assert_fails("must declare the engine-level shell denial")

    def test_lock_deny_shell_removed_fails(self) -> None:
        for rel in (ISSUE_LOCK, PR_LOCK, BACKLOG_LOCK):
            with self.subTest(rel=rel):
                self.mutate(rel, LOCK_DENY_SHELL, "--deny-tool nosuchflag")
                self.assert_fails("agent invocation must carry the engine-level shell denial")

    # -- direct label output (no suggestion routing) ----------------------

    def test_add_labels_issue_intent_false_removed_fails(self) -> None:
        for rel in (ISSUE_MD, PR_MD, BACKLOG_MD):
            with self.subTest(rel=rel):
                self.mutate(rel, SOURCE_ISSUE_INTENT_FALSE, "")
                self.assert_fails("must set 'issue-intent: false' under")

    def test_add_labels_issue_intent_true_fails(self) -> None:
        self.mutate(
            ISSUE_MD,
            SOURCE_ISSUE_INTENT_FALSE,
            "    issue-intent: true\n",
        )
        self.assert_fails("must set 'issue-intent: false' under")

    def test_issue_intent_key_elsewhere_in_safe_outputs_fails(self) -> None:
        # The key must be inside add-labels, not merely present under safe-outputs.
        self.mutate(ISSUE_MD, SOURCE_ISSUE_INTENT_FALSE, "")
        self.mutate(
            ISSUE_MD,
            "safe-outputs:\n  report-failure-as-issue: false\n",
            "safe-outputs:\n  report-failure-as-issue: false\n  issue-intent: false\n",
        )
        self.assert_fails("must set 'issue-intent: false' under")

    def test_direct_label_instruction_removed_fails(self) -> None:
        for rel in (ISSUE_MD, PR_MD, BACKLOG_MD):
            with self.subTest(rel=rel):
                self.mutate(rel, DIRECT_LABEL_INSTRUCTION, "prefer intent metadata")
                self.assert_fails("must instruct the agent to request label changes directly")

    def test_lock_issue_intent_false_removed_fails(self) -> None:
        text = self.read(ISSUE_LOCK)
        self.assertEqual(text.count(LOCK_ISSUE_INTENT_FALSE), 2, "unexpected config shape")
        self.write(
            ISSUE_LOCK,
            text.replace(LOCK_ISSUE_INTENT_FALSE, '\\"issue_intent\\":true'),
        )
        self.assert_fails("must carry add_labels issue_intent:false in both safe-output configs")

    def test_lock_issue_intent_false_in_one_config_only_fails(self) -> None:
        text = self.read(ISSUE_LOCK)
        head, sep, tail = text.partition("GH_AW_SAFE_OUTPUTS_HANDLER_CONFIG")
        self.assertTrue(sep, "handler config line not found")
        self.write(
            ISSUE_LOCK,
            head + sep + tail.replace(LOCK_ISSUE_INTENT_FALSE, '\\"issue_intent\\":true'),
        )
        self.assert_fails("must carry add_labels issue_intent:false in both safe-output configs")

    # -- generated lock negative cases ------------------------------------

    def test_forbidden_tool_readded_in_lock_grant_fails(self) -> None:
        self.mutate(
            ISSUE_LOCK,
            "        # --allow-tool github(issue_read)\n",
            "        # --allow-tool github(issue_read)\n"
            "        # --allow-tool github(get_file_contents)\n",
        )
        self.assert_fails("exposes forbidden github tool(s): get_file_contents")

    def test_forbidden_tool_readded_in_lock_manifest_fails(self) -> None:
        self.mutate(
            PR_LOCK,
            '"name":"github","tools":["issue_read","search_issues"]',
            '"name":"github","tools":["issue_read","search_issues",'
            '"pull_request_read"]',
        )
        self.assert_fails("exposes forbidden github tool(s): pull_request_read")

    def test_list_pull_requests_readded_in_lock_manifest_fails(self) -> None:
        self.mutate(
            PR_LOCK,
            '"name":"github","tools":["issue_read","search_issues"]',
            '"name":"github","tools":["issue_read","search_issues",'
            '"list_pull_requests"]',
        )
        self.assert_fails("exposes forbidden github tool(s): list_pull_requests")

    def test_search_pull_requests_readded_in_lock_manifest_fails(self) -> None:
        self.mutate(
            BACKLOG_LOCK,
            '"name":"github","tools":["issue_read","search_issues"]',
            '"name":"github","tools":["issue_read","search_issues",'
            '"search_pull_requests"]',
        )
        self.assert_fails("exposes forbidden github tool(s): search_pull_requests")

    # -- PR toolset exposure (capability root) ----------------------------

    def test_pull_requests_toolset_readded_in_source_fails(self) -> None:
        for rel in (PR_MD, BACKLOG_MD):
            with self.subTest(rel=rel):
                self.mutate(rel, SOURCE_TOOLSETS_ISSUES, SOURCE_TOOLSETS_WITH_PRS)
                self.assert_fails("toolsets must be exactly [issues]")

    def test_pull_requests_toolset_only_in_source_fails(self) -> None:
        self.mutate(
            PR_MD, SOURCE_TOOLSETS_ISSUES, "    toolsets: [pull_requests]\n"
        )
        self.assert_fails("toolsets must be exactly [issues]")

    def test_toolsets_missing_in_source_fails(self) -> None:
        for rel in (PR_MD, BACKLOG_MD):
            with self.subTest(rel=rel):
                self.mutate(rel, SOURCE_TOOLSETS_ISSUES, "")
                self.assert_fails("must declare toolsets in the github tools block")

    def test_search_pull_requests_readded_in_source_allowed_fails(self) -> None:
        self.mutate(
            PR_MD,
            "      - name: search_issues\n        max-calls: 2\n",
            "      - name: search_issues\n        max-calls: 2\n"
            "      - name: search_pull_requests\n        max-calls: 2\n",
        )
        self.assert_fails(
            "github allowed list exposes forbidden tool(s): search_pull_requests"
        )

    def test_pull_requests_toolset_readded_in_lock_fails(self) -> None:
        for rel in (PR_LOCK, BACKLOG_LOCK):
            with self.subTest(rel=rel):
                self.mutate(
                    rel, LOCK_TOOLSETS_ISSUES, LOCK_TOOLSETS_WITH_PRS
                )
                self.assert_fails("GITHUB_TOOLSETS must be exactly 'issues'")

    def test_toolsets_declaration_removed_in_lock_fails(self) -> None:
        self.mutate(PR_LOCK, LOCK_TOOLSETS_ISSUES, "")
        self.assert_fails("must carry exactly one GITHUB_TOOLSETS declaration")

    def test_toolsets_declaration_duplicated_in_lock_fails(self) -> None:
        self.mutate(
            PR_LOCK,
            LOCK_TOOLSETS_ISSUES,
            LOCK_TOOLSETS_ISSUES + "\n                  " + LOCK_TOOLSETS_ISSUES,
        )
        self.assert_fails("must carry exactly one GITHUB_TOOLSETS declaration")

    def test_add_comment_in_lock_fails(self) -> None:
        self.mutate(
            ISSUE_LOCK,
            "      - name: Set runtime paths\n",
            "      - name: Set runtime paths\n        # injected add_comment marker\n",
        )
        self.assert_fails("must not contain add_comment")

    def test_pin_drift_in_lock_fails(self) -> None:
        self.mutate(BACKLOG_LOCK, PIN, ZERO_SHA, occurrences=2)
        self.assert_fails("shared import pin drift")

    def test_guard_repos_scalar_form_fails(self) -> None:
        self.mutate(
            ISSUE_LOCK,
            LOCK_REPOS_ARRAY_BLOCK,
            '"repos": "${{ github.repository }}",',
        )
        self.assert_fails("guard policy must scope 'repos' to the repository as a JSON array")

    def test_guard_repos_wildcard_fails(self) -> None:
        self.mutate(ISSUE_LOCK, LOCK_REPOS_ARRAY_BLOCK, '"repos": ["*"],')
        self.assert_fails("guard policy 'repos' must be exactly the array")

    def test_guard_repos_widened_to_all_fails(self) -> None:
        self.mutate(ISSUE_LOCK, LOCK_REPOS_ARRAY_BLOCK, '"repos": ["all"],')
        self.assert_fails("guard policy 'repos' must be exactly the array")

    def test_guard_repos_widened_to_public_fails(self) -> None:
        self.mutate(ISSUE_LOCK, LOCK_REPOS_ARRAY_BLOCK, '"repos": ["public"],')
        self.assert_fails("guard policy 'repos' must be exactly the array")

    def test_guard_repos_literal_owner_repo_fails(self) -> None:
        self.mutate(
            ISSUE_LOCK, LOCK_REPOS_ARRAY_BLOCK, '"repos": ["bashrusakh/hfdesk"],'
        )
        self.assert_fails("guard policy 'repos' must be exactly the array")

    def test_guard_min_integrity_empty_fails(self) -> None:
        self.mutate(PR_LOCK, '"min-integrity": "none",', '"min-integrity": "",')
        self.assert_fails("guard policy min-integrity must be one of")

    def test_guard_min_integrity_restrengthened_fails(self) -> None:
        self.mutate(PR_LOCK, '"min-integrity": "none",', '"min-integrity": "approved",')
        self.assert_fails("guard policy min-integrity is 'approved'")

    def test_guard_min_integrity_unknown_level_fails(self) -> None:
        self.mutate(
            PR_LOCK, '"min-integrity": "none",', '"min-integrity": "trusted",'
        )
        self.assert_fails("guard policy min-integrity must be one of")

    def test_agent_allow_tool_write_reintroduced_fails(self) -> None:
        self.mutate(
            ISSUE_LOCK,
            "        # --allow-tool github(issue_read)\n",
            "        # --allow-tool github(issue_read)\n        # --allow-tool write\n",
        )
        self.assert_fails("must not grant '--allow-tool write'")

    def test_agent_allow_all_paths_reintroduced_fails(self) -> None:
        self.mutate(
            ISSUE_LOCK,
            "        # --allow-tool github(issue_read)\n",
            "        # --allow-tool github(issue_read)\n        # --allow-all-paths\n",
        )
        self.assert_fails("must not grant '--allow-all-paths'")

    def test_lock_report_failed_jobs_reintroduced_fails(self) -> None:
        self.mutate(
            BACKLOG_LOCK,
            "      - name: Set runtime paths\n",
            "      - name: Set runtime paths\n"
            '        env:\n          GH_AW_REPORT_FAILED_JOBS: "true"\n',
        )
        self.assert_fails("must not contain the report-failed-jobs machinery")

    def test_lock_failure_flag_true_fails(self) -> None:
        self.mutate(BACKLOG_LOCK, 'GH_AW_FAILURE_REPORT_AS_ISSUE: "false"',
                    'GH_AW_FAILURE_REPORT_AS_ISSUE: "true"')
        self.assert_fails('must set GH_AW_FAILURE_REPORT_AS_ISSUE: "false"')

    def test_agent_job_checkout_fails(self) -> None:
        self.mutate(ISSUE_LOCK, "  agent:\n",
                    "  agent:\n    x-injected: actions/checkout@v4\n")
        self.assert_fails("agent job must not check out the repository")

    def test_staged_safe_outputs_fails(self) -> None:
        self.mutate(
            PR_LOCK,
            "      GH_AW_WORKFLOW_ID_SANITIZED: prsemanticintake\n",
            "      GH_AW_WORKFLOW_ID_SANITIZED: prsemanticintake\n"
            '      GH_AW_SAFE_OUTPUTS_STAGED: "true"\n',
        )
        self.assert_fails("must not contain GH_AW_SAFE_OUTPUTS_STAGED")

    def test_lock_label_list_drift_fails(self) -> None:
        text = self.read(BACKLOG_LOCK)
        marker = "GH_AW_SAFE_OUTPUTS_HANDLER_CONFIG"
        head, sep, tail = text.partition(marker)
        self.assertTrue(sep, "handler config line not found")
        self.assertEqual(tail.count('\\"needs-info\\"'), 2, "unexpected config shape")
        tail = tail.replace('\\"needs-info\\"', '\\"needsinfo\\"', 1)
        self.write(BACKLOG_LOCK, head + sep + tail)
        self.assert_fails("safe-output label drift in add-labels")

    def test_missing_lock_fails(self) -> None:
        os.remove(self.path(BACKLOG_LOCK))
        self.assert_fails("missing lock file .github/workflows/backlog-retriage.lock.yml")

    # -- pre-existing checks stay enforced ---------------------------------

    def test_missing_core_import_in_source_fails(self) -> None:
        self.mutate(
            ISSUE_MD,
            "  - bashrusakh/repo-docs-sync/packages/ghaw-triage/workflows/"
            "issue-triage-core.md@" + PIN + "\n",
            "",
        )
        self.assert_fails("does not import issue-triage-core.md")

    def test_missing_vendored_import_fails(self) -> None:
        os.remove(self.path(
            ".github/aw/imports/bashrusakh/repo-docs-sync/" + PIN
            + "/packages_ghaw-triage_workflows_pr-intake-core.md"
        ))
        self.assert_fails("pinned import for pr-intake-core.md is not vendored")

    def test_policy_referencing_missing_contract_file_fails(self) -> None:
        self.mutate(
            os.path.join(".github", "triage-policy.md"),
            "## Metadata-only scope",
            "See `docs/does-not-exist.md` for details.\n\n## Metadata-only scope",
        )
        self.assert_fails("policy references missing contract file: docs/does-not-exist.md")

    def test_unmanaged_label_referenced_by_policy_fails(self) -> None:
        self.mutate(
            os.path.join(".github", "triage-policy.md"),
            "- Issue triage: `bug`, `enhancement`, `documentation`, `question`, "
            "`refactor`, `ci`, `needs-info`, `duplicate`",
            "- Issue triage: `bug`, `enhancement`, `documentation`, `question`, "
            "`refactor`, `ci`, `needs-info`, `duplicate`, `ghost-label`",
        )
        self.assert_fails("managed label 'ghost-label'")


if __name__ == "__main__":
    unittest.main(verbosity=2)
