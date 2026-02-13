"""Tests for the Claude Code and OpenCode installers."""

from __future__ import annotations

import importlib.resources
import os
import stat
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

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


class TestInstallClaude(unittest.TestCase):
    """Verify that install_claude copies files to the correct structure."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.target = Path(self.tmp) / "commands"

    def tearDown(self) -> None:
        import shutil

        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_creates_directory_structure(self) -> None:
        """The installer creates the expected directory tree."""
        install_claude(target_dir=self.target)

        for d in EXPECTED_DIRS:
            dir_path = self.target / d
            self.assertTrue(dir_path.is_dir(), f"Expected directory missing: {d}")

    def test_all_expected_files_exist(self) -> None:
        """Every expected markdown file is present after install."""
        install_claude(target_dir=self.target)

        for f in EXPECTED_FILES:
            file_path = self.target / f
            self.assertTrue(file_path.is_file(), f"Expected file missing: {f}")

    def test_no_extra_files(self) -> None:
        """No unexpected files (like __init__.py) are installed."""
        install_claude(target_dir=self.target)

        installed: set[str] = set()
        for root, _dirs, files in os.walk(self.target):
            for name in files:
                rel = os.path.relpath(os.path.join(root, name), self.target)
                installed.add(rel)

        expected_set = {f.replace("/", os.sep) for f in EXPECTED_FILES}
        self.assertEqual(installed, expected_set)

    def test_file_contents_match_source(self) -> None:
        """Installed files have the same content as the bundled source."""
        install_claude(target_dir=self.target)

        commands_root = importlib.resources.files("zing_ai.commands")

        # Check top-level zing.md
        src_content = commands_root.joinpath("zing.md").read_text(encoding="utf-8")
        dst_content = (self.target / "zing.md").read_text(encoding="utf-8")
        self.assertEqual(src_content, dst_content, "zing.md content mismatch")

        # Check a sub-command file
        src_content = commands_root.joinpath("zing").joinpath("build.md").read_text(encoding="utf-8")
        dst_content = (self.target / "zing" / "build.md").read_text(encoding="utf-8")
        self.assertEqual(src_content, dst_content, "zing/build.md content mismatch")

        # Check shared file
        src_content = commands_root.joinpath("_shared").joinpath("review-core.md").read_text(encoding="utf-8")
        dst_content = (self.target / "zing" / "_shared" / "review-core.md").read_text(encoding="utf-8")
        self.assertEqual(src_content, dst_content, "zing/_shared/review-core.md content mismatch")

    def test_idempotent_install(self) -> None:
        """Running install twice succeeds and produces the same result."""
        install_claude(target_dir=self.target)
        install_claude(target_dir=self.target)

        for f in EXPECTED_FILES:
            file_path = self.target / f
            self.assertTrue(file_path.is_file(), f"Expected file missing after re-install: {f}")


class TestInstallClaudeErrors(unittest.TestCase):
    """Verify error handling and cleanup."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()

    def tearDown(self) -> None:
        import shutil

        # Restore permissions before cleanup.
        for root, dirs, files in os.walk(self.tmp):
            for d in dirs:
                p = os.path.join(root, d)
                try:
                    os.chmod(p, stat.S_IRWXU)
                except OSError:
                    pass
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_unwritable_directory_exits_with_code_1(self) -> None:
        """Installing to a directory where we cannot create files exits cleanly."""
        # Create a read-only directory that prevents child creation.
        unwritable = Path(self.tmp) / "locked"
        unwritable.mkdir()
        os.chmod(unwritable, stat.S_IRUSR | stat.S_IXUSR)

        target = unwritable / "commands"
        with self.assertRaises(SystemExit) as ctx:
            install_claude(target_dir=target)

        self.assertEqual(ctx.exception.code, 1)

    def test_unwritable_directory_leaves_no_partial_files(self) -> None:
        """After a failed install to an unwritable dir, no files are left behind."""
        unwritable = Path(self.tmp) / "locked"
        unwritable.mkdir()
        os.chmod(unwritable, stat.S_IRUSR | stat.S_IXUSR)

        target = unwritable / "commands"
        with self.assertRaises(SystemExit):
            install_claude(target_dir=target)

        # The target directory should not exist.
        self.assertFalse(target.exists(), "Partial directory left behind")

    def test_partial_install_cleanup(self) -> None:
        """If a file copy fails midway, previously written files are removed."""
        target = Path(self.tmp) / "commands"

        real_write_text = Path.write_text
        write_count = 0

        def patched_write_text(self: Path, data: str, encoding: str = "utf-8") -> int:
            nonlocal write_count
            # Only count .md file writes within our target dir
            if str(self).startswith(str(target)) and self.suffix == ".md":
                write_count += 1
                if write_count > 2:
                    raise OSError("Simulated disk full")
            return real_write_text(self, data, encoding=encoding)

        with patch.object(Path, "write_text", patched_write_text):
            with self.assertRaises(SystemExit) as ctx:
                install_claude(target_dir=target)
            self.assertEqual(ctx.exception.code, 1)

        # After cleanup, no .md files should remain in the target tree.
        md_files = list(target.rglob("*.md")) if target.exists() else []
        self.assertEqual(md_files, [], f"Partial files left behind: {md_files}")


# ---------------------------------------------------------------------------
# OpenCode installer tests
# ---------------------------------------------------------------------------

# All markdown files that should be installed for OpenCode, relative to target.
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

# Directories that should be created for OpenCode.
OPENCODE_EXPECTED_DIRS = [
    "_shared",
]


class TestInstallOpencode(unittest.TestCase):
    """Verify that install_opencode copies converted files to the flat structure."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.target = Path(self.tmp) / "commands"

    def tearDown(self) -> None:
        import shutil

        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_creates_directory_structure(self) -> None:
        """The installer creates the expected directory tree."""
        install_opencode(target_dir=self.target)

        for d in OPENCODE_EXPECTED_DIRS:
            dir_path = self.target / d
            self.assertTrue(dir_path.is_dir(), f"Expected directory missing: {d}")

    def test_all_expected_files_exist(self) -> None:
        """Every expected markdown file is present after install."""
        install_opencode(target_dir=self.target)

        for f in OPENCODE_EXPECTED_FILES:
            file_path = self.target / f
            self.assertTrue(file_path.is_file(), f"Expected file missing: {f}")

    def test_no_extra_files(self) -> None:
        """No unexpected files leak into the target directory."""
        install_opencode(target_dir=self.target)

        installed: set[str] = set()
        for root, _dirs, files in os.walk(self.target):
            for name in files:
                rel = os.path.relpath(os.path.join(root, name), self.target)
                installed.add(rel)

        expected_set = {f.replace("/", os.sep) for f in OPENCODE_EXPECTED_FILES}
        self.assertEqual(installed, expected_set)

    def test_flat_naming_scheme(self) -> None:
        """Sub-commands use zing-{name}.md flat naming (no zing/ subdirectory)."""
        install_opencode(target_dir=self.target)

        # zing/ subdirectory should NOT exist — files are flattened.
        self.assertFalse(
            (self.target / "zing").exists(),
            "zing/ subdirectory should not exist in OpenCode layout",
        )

        # Spot-check that flattened files exist directly in the target.
        self.assertTrue((self.target / "zing-build.md").is_file())
        self.assertTrue((self.target / "zing-plan.md").is_file())

    def test_content_is_converted(self) -> None:
        """Installed files have content run through convert_for_opencode."""
        from zing_ai.converter import convert_for_opencode

        install_opencode(target_dir=self.target)

        commands_root = importlib.resources.files("zing_ai.commands")

        # Check top-level zing.md is converted.
        src_content = commands_root.joinpath("zing.md").read_text(encoding="utf-8")
        expected = convert_for_opencode(src_content)
        actual = (self.target / "zing.md").read_text(encoding="utf-8")
        self.assertEqual(actual, expected, "zing.md content not converted")

        # Check a sub-command file is converted.
        src_content = (
            commands_root.joinpath("zing").joinpath("build.md")
            .read_text(encoding="utf-8")
        )
        expected = convert_for_opencode(src_content)
        actual = (self.target / "zing-build.md").read_text(encoding="utf-8")
        self.assertEqual(actual, expected, "zing-build.md content not converted")

        # Check _shared/ file is converted.
        src_content = (
            commands_root.joinpath("_shared").joinpath("review-core.md")
            .read_text(encoding="utf-8")
        )
        expected = convert_for_opencode(src_content)
        actual = (self.target / "_shared" / "review-core.md").read_text(encoding="utf-8")
        self.assertEqual(
            actual, expected, "_shared/review-core.md content not converted",
        )

    def test_tool_names_are_lowercase(self) -> None:
        """Converted files use lowercase tool names (OpenCode convention)."""
        install_opencode(target_dir=self.target)

        # Read all installed files and check that no PascalCase Claude tool
        # names remain (spot-check a few prominent ones).
        claude_tool_names = ["TaskCreate", "TaskUpdate", "Bash", "Read", "Grep"]
        for f in OPENCODE_EXPECTED_FILES:
            content = (self.target / f).read_text(encoding="utf-8")
            for tool in claude_tool_names:
                # Use word boundary check: the tool name shouldn't appear as a
                # standalone word.  (It can appear as part of another word like
                # "Reading".)
                import re

                if re.search(rf"\b{tool}\b", content):
                    # Only flag if it's not part of a compound word.
                    self.fail(
                        f"Claude tool name '{tool}' found unconverted in {f}"
                    )

    def test_idempotent_install(self) -> None:
        """Running install twice succeeds and produces the same result."""
        install_opencode(target_dir=self.target)
        install_opencode(target_dir=self.target)

        for f in OPENCODE_EXPECTED_FILES:
            file_path = self.target / f
            self.assertTrue(
                file_path.is_file(),
                f"Expected file missing after re-install: {f}",
            )


class TestInstallOpencodeErrors(unittest.TestCase):
    """Verify error handling and cleanup for the OpenCode installer."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()

    def tearDown(self) -> None:
        import shutil

        # Restore permissions before cleanup.
        for root, dirs, files in os.walk(self.tmp):
            for d in dirs:
                p = os.path.join(root, d)
                try:
                    os.chmod(p, stat.S_IRWXU)
                except OSError:
                    pass
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_unwritable_directory_exits_with_code_1(self) -> None:
        """Installing to a directory where we cannot create files exits cleanly."""
        unwritable = Path(self.tmp) / "locked"
        unwritable.mkdir()
        os.chmod(unwritable, stat.S_IRUSR | stat.S_IXUSR)

        target = unwritable / "commands"
        with self.assertRaises(SystemExit) as ctx:
            install_opencode(target_dir=target)

        self.assertEqual(ctx.exception.code, 1)

    def test_unwritable_directory_leaves_no_partial_files(self) -> None:
        """After a failed install to an unwritable dir, no files are left behind."""
        unwritable = Path(self.tmp) / "locked"
        unwritable.mkdir()
        os.chmod(unwritable, stat.S_IRUSR | stat.S_IXUSR)

        target = unwritable / "commands"
        with self.assertRaises(SystemExit):
            install_opencode(target_dir=target)

        self.assertFalse(target.exists(), "Partial directory left behind")

    def test_partial_install_cleanup(self) -> None:
        """If a file copy fails midway, previously written files are removed."""
        target = Path(self.tmp) / "commands"

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
            with self.assertRaises(SystemExit) as ctx:
                install_opencode(target_dir=target)
            self.assertEqual(ctx.exception.code, 1)

        # After cleanup, no .md files should remain in the target tree.
        md_files = list(target.rglob("*.md")) if target.exists() else []
        self.assertEqual(md_files, [], f"Partial files left behind: {md_files}")


if __name__ == "__main__":
    unittest.main()
