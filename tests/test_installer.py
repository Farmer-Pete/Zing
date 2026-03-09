"""Tests for the Claude Code and OpenCode installers."""

from __future__ import annotations

import importlib.resources
import os
import re
import stat
from pathlib import Path
from unittest.mock import patch

import pytest

from zing_ai.installer import install_claude, install_opencode

# All markdown files that should be installed, relative to the target dir.
EXPECTED_FILES = [
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

# Directories that should be created.
EXPECTED_DIRS = [
    "zing",
    "zing/_shared",
]


# ---------------------------------------------------------------------------
# Claude Code installer
# ---------------------------------------------------------------------------


def test_claude_creates_directory_structure(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_claude(target_dir=target)

    for d in EXPECTED_DIRS:
        assert (target / d).is_dir(), f"Expected directory missing: {d}"


def test_claude_all_expected_files_exist(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_claude(target_dir=target)

    for f in EXPECTED_FILES:
        assert (target / f).is_file(), f"Expected file missing: {f}"


def test_claude_no_extra_files(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_claude(target_dir=target)

    installed: set[str] = set()
    for root, _dirs, files in os.walk(target):
        for name in files:
            rel = os.path.relpath(os.path.join(root, name), target)
            installed.add(rel)

    expected_set = {f.replace("/", os.sep) for f in EXPECTED_FILES}
    expected_set.add("zing-manifest.json")
    assert installed == expected_set


def test_claude_file_contents_match_source(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_claude(target_dir=target)

    commands_root = importlib.resources.files("zing_ai.commands")

    src_content = commands_root.joinpath("zing.md").read_text(encoding="utf-8")
    dst_content = (target / "zing.md").read_text(encoding="utf-8")
    assert src_content == dst_content, "zing.md content mismatch"

    src_content = commands_root.joinpath("zing").joinpath("build.md").read_text(encoding="utf-8")
    dst_content = (target / "zing" / "build.md").read_text(encoding="utf-8")
    assert src_content == dst_content, "zing/build.md content mismatch"

    src_content = (
        commands_root.joinpath("_shared").joinpath("review-core.md").read_text(encoding="utf-8")
    )
    dst_content = (target / "zing" / "_shared" / "review-core.md").read_text(encoding="utf-8")
    assert src_content == dst_content, "zing/_shared/review-core.md content mismatch"


def test_claude_idempotent_install(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_claude(target_dir=target)
    install_claude(target_dir=target)

    for f in EXPECTED_FILES:
        assert (target / f).is_file(), f"Expected file missing after re-install: {f}"


# ---------------------------------------------------------------------------
# Claude Code installer errors
# ---------------------------------------------------------------------------


def test_claude_unwritable_directory_exits_with_code_1(tmp_path: Path) -> None:
    unwritable = tmp_path / "locked"
    unwritable.mkdir()
    os.chmod(unwritable, stat.S_IRUSR | stat.S_IXUSR)

    target = unwritable / "commands"
    try:
        with pytest.raises(SystemExit) as exc_info:
            install_claude(target_dir=target)
        assert exc_info.value.code == 1
    finally:
        os.chmod(unwritable, stat.S_IRWXU)


def test_claude_unwritable_directory_leaves_no_partial_files(tmp_path: Path) -> None:
    unwritable = tmp_path / "locked"
    unwritable.mkdir()
    os.chmod(unwritable, stat.S_IRUSR | stat.S_IXUSR)

    target = unwritable / "commands"
    try:
        with pytest.raises(SystemExit):
            install_claude(target_dir=target)
        assert not target.exists(), "Partial directory left behind"
    finally:
        os.chmod(unwritable, stat.S_IRWXU)


def test_claude_partial_install_cleanup(tmp_path: Path) -> None:
    target = tmp_path / "commands"

    real_write_text = Path.write_text
    write_count = 0

    def patched_write_text(self: Path, data: str, encoding: str = "utf-8") -> int:
        nonlocal write_count
        if str(self).startswith(str(target)) and self.suffix == ".md":
            write_count += 1
            if write_count > 2:
                raise OSError("Simulated disk full")
        return real_write_text(self, data, encoding=encoding)

    with patch.object(Path, "write_text", patched_write_text):
        with pytest.raises(SystemExit) as exc_info:
            install_claude(target_dir=target)
        assert exc_info.value.code == 1

    md_files = list(target.rglob("*.md")) if target.exists() else []
    assert md_files == [], f"Partial files left behind: {md_files}"


# ---------------------------------------------------------------------------
# OpenCode installer
# ---------------------------------------------------------------------------

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

OPENCODE_EXPECTED_DIRS = [
    "_shared",
]


def test_opencode_creates_directory_structure(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    for d in OPENCODE_EXPECTED_DIRS:
        assert (target / d).is_dir(), f"Expected directory missing: {d}"


def test_opencode_all_expected_files_exist(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    for f in OPENCODE_EXPECTED_FILES:
        assert (target / f).is_file(), f"Expected file missing: {f}"


def test_opencode_no_extra_files(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    installed: set[str] = set()
    for root, _dirs, files in os.walk(target):
        for name in files:
            rel = os.path.relpath(os.path.join(root, name), target)
            installed.add(rel)

    expected_set = {f.replace("/", os.sep) for f in OPENCODE_EXPECTED_FILES}
    expected_set.add("zing-manifest.json")
    assert installed == expected_set


def test_opencode_flat_naming_scheme(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    assert not (target / "zing").exists(), "zing/ subdirectory should not exist in OpenCode layout"
    assert (target / "zing-build.md").is_file()
    assert (target / "zing-plan.md").is_file()


def test_opencode_content_is_converted(tmp_path: Path) -> None:
    from zing_ai.converter import convert_for_opencode

    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    commands_root = importlib.resources.files("zing_ai.commands")

    src_content = commands_root.joinpath("zing.md").read_text(encoding="utf-8")
    expected = convert_for_opencode(src_content)
    actual = (target / "zing.md").read_text(encoding="utf-8")
    assert actual == expected, "zing.md content not converted"

    src_content = commands_root.joinpath("zing").joinpath("build.md").read_text(encoding="utf-8")
    expected = convert_for_opencode(src_content)
    actual = (target / "zing-build.md").read_text(encoding="utf-8")
    assert actual == expected, "zing-build.md content not converted"

    src_content = (
        commands_root.joinpath("_shared").joinpath("review-core.md").read_text(encoding="utf-8")
    )
    expected = convert_for_opencode(src_content)
    actual = (target / "_shared" / "review-core.md").read_text(encoding="utf-8")
    assert actual == expected, "_shared/review-core.md content not converted"


def test_opencode_tool_names_are_lowercase(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_opencode(target_dir=target)

    claude_tool_names = ["TaskCreate", "TaskUpdate", "Bash", "Read", "Grep"]
    for f in OPENCODE_EXPECTED_FILES:
        content = (target / f).read_text(encoding="utf-8")
        for tool in claude_tool_names:
            if re.search(rf"\b{tool}\b", content):
                pytest.fail(f"Claude tool name '{tool}' found unconverted in {f}")


def test_opencode_idempotent_install(tmp_path: Path) -> None:
    target = tmp_path / "commands"
    install_opencode(target_dir=target)
    install_opencode(target_dir=target)

    for f in OPENCODE_EXPECTED_FILES:
        assert (target / f).is_file(), f"Expected file missing after re-install: {f}"


# ---------------------------------------------------------------------------
# OpenCode installer errors
# ---------------------------------------------------------------------------


def test_opencode_unwritable_directory_exits_with_code_1(tmp_path: Path) -> None:
    unwritable = tmp_path / "locked"
    unwritable.mkdir()
    os.chmod(unwritable, stat.S_IRUSR | stat.S_IXUSR)

    target = unwritable / "commands"
    try:
        with pytest.raises(SystemExit) as exc_info:
            install_opencode(target_dir=target)
        assert exc_info.value.code == 1
    finally:
        os.chmod(unwritable, stat.S_IRWXU)


def test_opencode_unwritable_directory_leaves_no_partial_files(tmp_path: Path) -> None:
    unwritable = tmp_path / "locked"
    unwritable.mkdir()
    os.chmod(unwritable, stat.S_IRUSR | stat.S_IXUSR)

    target = unwritable / "commands"
    try:
        with pytest.raises(SystemExit):
            install_opencode(target_dir=target)
        assert not target.exists(), "Partial directory left behind"
    finally:
        os.chmod(unwritable, stat.S_IRWXU)


def test_opencode_partial_install_cleanup(tmp_path: Path) -> None:
    target = tmp_path / "commands"

    real_write_text = Path.write_text
    write_count = 0

    def patched_write_text(self: Path, data: str, encoding: str = "utf-8") -> int:
        nonlocal write_count
        if str(self).startswith(str(target)) and self.suffix == ".md":
            write_count += 1
            if write_count > 2:
                raise OSError("Simulated disk full")
        return real_write_text(self, data, encoding=encoding)

    with patch.object(Path, "write_text", patched_write_text):
        with pytest.raises(SystemExit) as exc_info:
            install_opencode(target_dir=target)
        assert exc_info.value.code == 1

    md_files = list(target.rglob("*.md")) if target.exists() else []
    assert md_files == [], f"Partial files left behind: {md_files}"


# ---------------------------------------------------------------------------
# MCP registration integration
# ---------------------------------------------------------------------------


def test_claude_install_calls_register_mcp_server(tmp_path: Path) -> None:
    """install_claude() calls register_mcp_server('claude') after writing manifest."""
    target = tmp_path / "commands"
    with patch("zing_ai.installer.register_mcp_server") as mock_reg:
        install_claude(target_dir=target)

    mock_reg.assert_called_once_with("claude")


def test_opencode_install_calls_register_mcp_server(tmp_path: Path) -> None:
    """install_opencode() calls register_mcp_server('opencode') after writing manifest."""
    target = tmp_path / "commands"
    with patch("zing_ai.installer.register_mcp_server") as mock_reg:
        install_opencode(target_dir=target)

    mock_reg.assert_called_once_with("opencode")
