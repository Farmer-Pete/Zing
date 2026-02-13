"""End-to-end integration tests for the full install flow.

These tests exercise the high-level install functions (not individual helpers)
and verify the complete outcome: files installed, manifest written, backups
created on re-install, and content correctness for both runtimes.
"""

from __future__ import annotations

import importlib.resources
import re
from pathlib import Path
from unittest.mock import patch

from click.testing import CliRunner

from zing_ai.cli import cli
from zing_ai.installer import install_claude, install_opencode
from zing_ai.manifest import detect_modified_files, read_manifest

# ---------------------------------------------------------------------------
# Expected file lists (duplicated here so integration tests are self-contained)
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


def test_claude_full_install_flow(tmp_path: Path) -> None:
    """install_claude produces all 9 files, a manifest, and content matches source."""
    target = tmp_path / "commands"
    install_claude(target_dir=target)

    for relpath in CLAUDE_EXPECTED_FILES:
        assert (target / relpath).is_file(), f"Expected file missing: {relpath}"

    manifest = read_manifest(target)
    assert manifest is not None, "Manifest not written"
    assert manifest["runtime"] == "claude-code"
    assert len(manifest["files"]) == len(CLAUDE_EXPECTED_FILES)

    commands_root = importlib.resources.files("zing_ai.commands")
    src_content = commands_root.joinpath("zing.md").read_text(encoding="utf-8")
    dst_content = (target / "zing.md").read_text(encoding="utf-8")
    assert src_content == dst_content, "zing.md content mismatch"


def test_claude_reinstall_no_modifications(tmp_path: Path) -> None:
    """Re-installing when no files were modified creates no patches."""
    target = tmp_path / "commands"
    install_claude(target_dir=target)
    install_claude(target_dir=target)

    patches_dir = target / "zing-patches"
    if patches_dir.exists():
        patch_files = [p for p in patches_dir.rglob("*") if p.is_file()]
        assert patch_files == [], "Patches created despite no modifications"

    for relpath in CLAUDE_EXPECTED_FILES:
        assert (target / relpath).is_file(), f"File missing after re-install: {relpath}"


def test_claude_reinstall_with_modifications(tmp_path: Path) -> None:
    """Modified files are backed up on re-install, then overwritten."""
    target = tmp_path / "commands"
    install_claude(target_dir=target)

    modified_file = target / "zing.md"
    original_content = modified_file.read_text(encoding="utf-8")
    modified_file.write_text("USER MODIFICATION", encoding="utf-8")

    modified = detect_modified_files(target)
    assert "zing.md" in modified

    install_claude(target_dir=target)

    patches_dir = target / "zing-patches"
    assert patches_dir.is_dir(), "zing-patches/ not created"
    patch_files = list(patches_dir.rglob("zing.md.*"))
    assert len(patch_files) >= 1, "No backup found for modified zing.md"

    backup_content = patch_files[0].read_text(encoding="utf-8")
    assert backup_content == "USER MODIFICATION"

    restored_content = modified_file.read_text(encoding="utf-8")
    assert restored_content == original_content

    manifest = read_manifest(target)
    assert manifest is not None


# ===================================================================
# OpenCode E2E
# ===================================================================


def test_opencode_full_install_flow(tmp_path: Path) -> None:
    """install_opencode produces all 9 files in flat naming, manifest exists, content converted."""
    from zing_ai.converter import convert_for_opencode

    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    for relpath in OPENCODE_EXPECTED_FILES:
        assert (target / relpath).is_file(), f"Expected file missing: {relpath}"

    manifest = read_manifest(target)
    assert manifest is not None, "Manifest not written"
    assert manifest["runtime"] == "opencode"
    assert len(manifest["files"]) == len(OPENCODE_EXPECTED_FILES)

    commands_root = importlib.resources.files("zing_ai.commands")
    src_content = commands_root.joinpath("zing.md").read_text(encoding="utf-8")
    expected = convert_for_opencode(src_content)
    actual = (target / "zing.md").read_text(encoding="utf-8")
    assert actual == expected, "zing.md not converted for OpenCode"


def test_opencode_reinstall_with_modifications(tmp_path: Path) -> None:
    """Modified OpenCode files are backed up on re-install, then overwritten."""
    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    modified_file = target / "zing-build.md"
    original_content = modified_file.read_text(encoding="utf-8")
    modified_file.write_text("OPENCODE USER MOD", encoding="utf-8")

    modified = detect_modified_files(target)
    assert "zing-build.md" in modified

    install_opencode(target_dir=target)

    patches_dir = target / "zing-patches"
    assert patches_dir.is_dir(), "zing-patches/ not created"
    patch_files = list(patches_dir.rglob("zing-build.md.*"))
    assert len(patch_files) >= 1, "No backup found for modified zing-build.md"

    backup_content = patch_files[0].read_text(encoding="utf-8")
    assert backup_content == "OPENCODE USER MOD"

    restored_content = modified_file.read_text(encoding="utf-8")
    assert restored_content == original_content


# ===================================================================
# CLI Dispatch E2E
# ===================================================================


def test_cli_dispatch_both_runtimes(tmp_path: Path) -> None:
    """CLI install --all dispatches to both Claude Code and OpenCode installers."""
    claude_target = tmp_path / "claude_commands"
    opencode_target = tmp_path / "opencode_commands"

    runner = CliRunner()

    with (
        patch(
            "zing_ai.installer.install_claude",
            side_effect=lambda: install_claude(target_dir=claude_target),
        ),
        patch(
            "zing_ai.installer.install_opencode",
            side_effect=lambda: install_opencode(target_dir=opencode_target),
        ),
    ):
        result = runner.invoke(cli, ["install", "--all"])

    assert result.exit_code == 0

    for relpath in CLAUDE_EXPECTED_FILES:
        assert (claude_target / relpath).is_file(), f"Claude file missing: {relpath}"

    for relpath in OPENCODE_EXPECTED_FILES:
        assert (opencode_target / relpath).is_file(), f"OpenCode file missing: {relpath}"


# ===================================================================
# Content Verification
# ===================================================================


def test_no_yaml_frontmatter_claude(tmp_path: Path) -> None:
    """No Claude Code installed file starts with YAML frontmatter ('---')."""
    target = tmp_path / "commands"
    install_claude(target_dir=target)

    for relpath in CLAUDE_EXPECTED_FILES:
        content = (target / relpath).read_text(encoding="utf-8")
        assert not content.startswith("---"), f"Claude file {relpath} starts with YAML frontmatter"


def test_no_yaml_frontmatter_opencode(tmp_path: Path) -> None:
    """No OpenCode installed file starts with YAML frontmatter ('---')."""
    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    for relpath in OPENCODE_EXPECTED_FILES:
        content = (target / relpath).read_text(encoding="utf-8")
        assert not content.startswith("---"), (
            f"OpenCode file {relpath} starts with YAML frontmatter"
        )


def test_opencode_no_pascal_case_tool_names(tmp_path: Path) -> None:
    """No PascalCase Claude tool names remain in OpenCode files."""
    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    for relpath in OPENCODE_EXPECTED_FILES:
        content = (target / relpath).read_text(encoding="utf-8")
        for tool_name in CLAUDE_TOOL_NAMES:
            assert not re.search(rf"\b{tool_name}\b", content), (
                f"Claude tool name '{tool_name}' found unconverted in OpenCode file {relpath}"
            )


def test_opencode_skill_chaining_syntax(tmp_path: Path) -> None:
    """OpenCode files use skill({ name: syntax, not Skill(skill: syntax."""
    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    for relpath in OPENCODE_EXPECTED_FILES:
        content = (target / relpath).read_text(encoding="utf-8")

        assert not re.search(r"Skill\(skill:", content), (
            f"Claude-style Skill(skill: found in OpenCode file {relpath}"
        )

        if relpath == "zing.md":
            assert re.search(r"skill\(\{", content), f"Expected skill({{ name: syntax in {relpath}"
