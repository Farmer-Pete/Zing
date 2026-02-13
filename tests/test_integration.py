"""End-to-end integration tests for the full install flow.

These tests exercise the high-level install functions (not individual helpers)
and verify the complete outcome: files installed, manifest written, backups
created on re-install, and content correctness for both runtimes.
"""

from __future__ import annotations

import argparse
import os
import re
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from zing_ai.installer import install_claude, install_opencode
from zing_ai.manifest import detect_modified_files, read_manifest


# ---------------------------------------------------------------------------
# Expected file lists (shared with unit tests; duplicated here so that
# integration tests are self-contained).
# ---------------------------------------------------------------------------

CLAUDE_EXPECTED_FILES = [
    "zing.md",
    "zing/new.md",
    "zing/build.md",
    "zing/plan.md",
    "zing/plan-audit.md",
    "zing/build-audit.md",
    "zing/pr-audit.md",
    "zing/plan-linear.md",
    "zing/_shared/review-core.md",
]

OPENCODE_EXPECTED_FILES = [
    "zing.md",
    "zing-new.md",
    "zing-build.md",
    "zing-plan.md",
    "zing-plan-audit.md",
    "zing-build-audit.md",
    "zing-pr-audit.md",
    "zing-plan-linear.md",
    "_shared/review-core.md",
]

# PascalCase Claude tool names that must NOT appear in OpenCode files.
CLAUDE_TOOL_NAMES = ["AskUserQuestion", "TaskCreate", "Bash", "Read", "Grep"]


# ===================================================================
# Claude Code E2E
# ===================================================================


class TestClaudeCodeE2E(unittest.TestCase):
    """Full install flow for Claude Code."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.target = Path(self.tmp) / "commands"

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    # -- Test 1: Full install flow ------------------------------------------

    def test_full_install_flow(self) -> None:
        """install_claude produces all 9 files, a manifest, and content matches source."""
        install_claude(target_dir=self.target)

        # All 9 files exist.
        for relpath in CLAUDE_EXPECTED_FILES:
            self.assertTrue(
                (self.target / relpath).is_file(),
                f"Expected file missing: {relpath}",
            )

        # Manifest exists and is valid.
        manifest = read_manifest(self.target)
        self.assertIsNotNone(manifest, "Manifest not written")
        self.assertEqual(manifest["runtime"], "claude-code")
        self.assertEqual(len(manifest["files"]), len(CLAUDE_EXPECTED_FILES))

        # Content matches bundled source.
        import importlib.resources

        commands_root = importlib.resources.files("zing_ai.commands")
        src_content = commands_root.joinpath("zing.md").read_text(encoding="utf-8")
        dst_content = (self.target / "zing.md").read_text(encoding="utf-8")
        self.assertEqual(src_content, dst_content, "zing.md content mismatch")

    # -- Test 2: Re-install with no modifications ---------------------------

    def test_reinstall_no_modifications(self) -> None:
        """Re-installing when no files were modified creates no patches."""
        install_claude(target_dir=self.target)
        install_claude(target_dir=self.target)

        patches_dir = self.target / "zing-patches"
        if patches_dir.exists():
            patches = list(patches_dir.rglob("*"))
            patch_files = [p for p in patches if p.is_file()]
            self.assertEqual(
                patch_files, [], "Patches created despite no modifications",
            )

        # All files still present and identical.
        for relpath in CLAUDE_EXPECTED_FILES:
            self.assertTrue(
                (self.target / relpath).is_file(),
                f"File missing after re-install: {relpath}",
            )

    # -- Test 3: Re-install with modifications ------------------------------

    def test_reinstall_with_modifications(self) -> None:
        """Modified files are backed up on re-install, then overwritten."""
        install_claude(target_dir=self.target)

        # Modify a file.
        modified_file = self.target / "zing.md"
        original_content = modified_file.read_text(encoding="utf-8")
        modified_file.write_text("USER MODIFICATION", encoding="utf-8")

        # Verify modification is detected.
        modified = detect_modified_files(self.target)
        self.assertIn("zing.md", modified)

        # Re-install.
        install_claude(target_dir=self.target)

        # Backup should exist in zing-patches/.
        patches_dir = self.target / "zing-patches"
        self.assertTrue(patches_dir.is_dir(), "zing-patches/ not created")
        patch_files = list(patches_dir.rglob("zing.md.*"))
        self.assertGreaterEqual(
            len(patch_files), 1,
            "No backup found for modified zing.md",
        )

        # The backup should contain the user modification.
        backup_content = patch_files[0].read_text(encoding="utf-8")
        self.assertEqual(backup_content, "USER MODIFICATION")

        # The installed file should be back to the original.
        restored_content = modified_file.read_text(encoding="utf-8")
        self.assertEqual(restored_content, original_content)

        # Manifest should be updated.
        manifest = read_manifest(self.target)
        self.assertIsNotNone(manifest)


# ===================================================================
# OpenCode E2E
# ===================================================================


class TestOpenCodeE2E(unittest.TestCase):
    """Full install flow for OpenCode."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.target = Path(self.tmp) / "commands"

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    # -- Test 4: Full install flow ------------------------------------------

    def test_full_install_flow(self) -> None:
        """install_opencode produces all 9 files in flat naming, manifest exists, content converted."""
        install_opencode(target_dir=self.target)

        # All 9 files exist.
        for relpath in OPENCODE_EXPECTED_FILES:
            self.assertTrue(
                (self.target / relpath).is_file(),
                f"Expected file missing: {relpath}",
            )

        # Manifest exists and is valid.
        manifest = read_manifest(self.target)
        self.assertIsNotNone(manifest, "Manifest not written")
        self.assertEqual(manifest["runtime"], "opencode")
        self.assertEqual(len(manifest["files"]), len(OPENCODE_EXPECTED_FILES))

        # Content is converted (not raw source).
        from zing_ai.converter import convert_for_opencode

        import importlib.resources

        commands_root = importlib.resources.files("zing_ai.commands")
        src_content = commands_root.joinpath("zing.md").read_text(encoding="utf-8")
        expected = convert_for_opencode(src_content)
        actual = (self.target / "zing.md").read_text(encoding="utf-8")
        self.assertEqual(actual, expected, "zing.md not converted for OpenCode")

    # -- Test 5: Re-install with modifications ------------------------------

    def test_reinstall_with_modifications(self) -> None:
        """Modified OpenCode files are backed up on re-install, then overwritten."""
        install_opencode(target_dir=self.target)

        # Modify a file.
        modified_file = self.target / "zing-build.md"
        original_content = modified_file.read_text(encoding="utf-8")
        modified_file.write_text("OPENCODE USER MOD", encoding="utf-8")

        # Verify modification is detected.
        modified = detect_modified_files(self.target)
        self.assertIn("zing-build.md", modified)

        # Re-install.
        install_opencode(target_dir=self.target)

        # Backup should exist.
        patches_dir = self.target / "zing-patches"
        self.assertTrue(patches_dir.is_dir(), "zing-patches/ not created")
        patch_files = list(patches_dir.rglob("zing-build.md.*"))
        self.assertGreaterEqual(
            len(patch_files), 1,
            "No backup found for modified zing-build.md",
        )

        # Backup contains the user modification.
        backup_content = patch_files[0].read_text(encoding="utf-8")
        self.assertEqual(backup_content, "OPENCODE USER MOD")

        # Installed file is restored.
        restored_content = modified_file.read_text(encoding="utf-8")
        self.assertEqual(restored_content, original_content)


# ===================================================================
# CLI Dispatch E2E
# ===================================================================


class TestCLIDispatchE2E(unittest.TestCase):
    """Test 6: CLI _handle_install dispatches to both runtimes."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.claude_target = Path(self.tmp) / "claude_commands"
        self.opencode_target = Path(self.tmp) / "opencode_commands"

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_cli_dispatch_both_runtimes(self) -> None:
        """_handle_install with --all creates files for both Claude Code and OpenCode."""
        from zing_ai.cli import _handle_install

        args = argparse.Namespace(claude=False, opencode=False, all=True)

        # Patch the install functions to use our temp directories.
        with (
            patch(
                "zing_ai.installer.install_claude",
                side_effect=lambda target_dir=None: install_claude(
                    target_dir=self.claude_target,
                ),
            ) as mock_claude,
            patch(
                "zing_ai.installer.install_opencode",
                side_effect=lambda target_dir=None: install_opencode(
                    target_dir=self.opencode_target,
                ),
            ) as mock_opencode,
        ):
            _handle_install(args)

        # Verify Claude Code files were created.
        for relpath in CLAUDE_EXPECTED_FILES:
            self.assertTrue(
                (self.claude_target / relpath).is_file(),
                f"Claude file missing: {relpath}",
            )

        # Verify OpenCode files were created.
        for relpath in OPENCODE_EXPECTED_FILES:
            self.assertTrue(
                (self.opencode_target / relpath).is_file(),
                f"OpenCode file missing: {relpath}",
            )


# ===================================================================
# Content Verification
# ===================================================================


class TestContentVerification(unittest.TestCase):
    """Cross-runtime content checks."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.claude_target = Path(self.tmp) / "claude_commands"
        self.opencode_target = Path(self.tmp) / "opencode_commands"
        install_claude(target_dir=self.claude_target)
        install_opencode(target_dir=self.opencode_target)

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    # -- Test 7: No YAML frontmatter ---------------------------------------

    def test_no_yaml_frontmatter_claude(self) -> None:
        """No Claude Code installed file starts with YAML frontmatter ('---')."""
        for relpath in CLAUDE_EXPECTED_FILES:
            content = (self.claude_target / relpath).read_text(encoding="utf-8")
            self.assertFalse(
                content.startswith("---"),
                f"Claude file {relpath} starts with YAML frontmatter",
            )

    def test_no_yaml_frontmatter_opencode(self) -> None:
        """No OpenCode installed file starts with YAML frontmatter ('---')."""
        for relpath in OPENCODE_EXPECTED_FILES:
            content = (self.opencode_target / relpath).read_text(encoding="utf-8")
            self.assertFalse(
                content.startswith("---"),
                f"OpenCode file {relpath} starts with YAML frontmatter",
            )

    # -- Test 8: OpenCode tool names are lowercase --------------------------

    def test_opencode_no_pascal_case_tool_names(self) -> None:
        """No PascalCase Claude tool names remain in OpenCode files."""
        for relpath in OPENCODE_EXPECTED_FILES:
            content = (self.opencode_target / relpath).read_text(encoding="utf-8")
            for tool_name in CLAUDE_TOOL_NAMES:
                if re.search(rf"\b{tool_name}\b", content):
                    self.fail(
                        f"Claude tool name '{tool_name}' found unconverted "
                        f"in OpenCode file {relpath}",
                    )

    # -- Test 9: OpenCode skill chaining syntax -----------------------------

    def test_opencode_skill_chaining_syntax(self) -> None:
        """OpenCode files use skill({ name: syntax, not Skill(skill: syntax."""
        for relpath in OPENCODE_EXPECTED_FILES:
            content = (self.opencode_target / relpath).read_text(encoding="utf-8")

            # Should NOT contain Claude-style Skill(skill: calls.
            self.assertNotRegex(
                content,
                r"Skill\(skill:",
                f"Claude-style Skill(skill: found in OpenCode file {relpath}",
            )

            # If the source had skill calls, the OpenCode version should
            # use skill({ name: syntax.  We check files known to contain
            # skill calls.
            if relpath == "zing.md":
                self.assertRegex(
                    content,
                    r'skill\(\{',
                    f"Expected skill({{ name: syntax in {relpath}",
                )


if __name__ == "__main__":
    unittest.main()
