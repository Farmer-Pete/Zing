"""Tests for zing_ai.backup — patch backup and restore."""

from __future__ import annotations

import io
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from zing_ai.backup import backup_modified_files, list_patches, reapply_patches
from zing_ai.installer import install_claude
from zing_ai.manifest import write_manifest


class TestBackupModifiedFiles(unittest.TestCase):
    """Verify backup_modified_files behaviour."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.target = Path(self.tmp)

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_backs_up_modified_files(self) -> None:
        """Modified files are copied to zing-patches/ with timestamp suffix."""
        # Set up installed file and manifest.
        (self.target / "zing.md").write_text("# Original", encoding="utf-8")
        write_manifest(self.target, "claude-code", ["zing.md"])

        # Modify the file.
        (self.target / "zing.md").write_text("# Modified!", encoding="utf-8")

        result = backup_modified_files(self.target)

        self.assertEqual(len(result), 1)
        relpath, backup_path = result[0]
        self.assertEqual(relpath, "zing.md")
        self.assertTrue(backup_path.exists())
        self.assertTrue(backup_path.is_file())
        # Backup should contain the modified content.
        self.assertEqual(backup_path.read_text(encoding="utf-8"), "# Modified!")
        # Backup lives in zing-patches/.
        self.assertTrue(str(backup_path).startswith(str(self.target / "zing-patches")))

    def test_returns_empty_on_fresh_install(self) -> None:
        """Returns empty list when no manifest exists (fresh install)."""
        (self.target / "zing.md").write_text("# Zing", encoding="utf-8")

        result = backup_modified_files(self.target)
        self.assertEqual(result, [])

    def test_returns_empty_when_no_files_modified(self) -> None:
        """Returns empty list when all files match the manifest."""
        (self.target / "zing.md").write_text("# Zing", encoding="utf-8")
        write_manifest(self.target, "claude-code", ["zing.md"])

        result = backup_modified_files(self.target)
        self.assertEqual(result, [])

    def test_skips_deleted_files(self) -> None:
        """Deleted files are detected as modified but not backed up (nothing to copy)."""
        (self.target / "zing.md").write_text("# Zing", encoding="utf-8")
        write_manifest(self.target, "claude-code", ["zing.md"])
        (self.target / "zing.md").unlink()

        result = backup_modified_files(self.target)
        # Deleted files cannot be backed up -- nothing on disk to copy.
        self.assertEqual(result, [])

    def test_patches_dir_creation_failure_prints_warning(self) -> None:
        """If zing-patches/ cannot be created, prints warning and returns empty."""
        (self.target / "zing.md").write_text("# Original", encoding="utf-8")
        write_manifest(self.target, "claude-code", ["zing.md"])
        (self.target / "zing.md").write_text("# Modified!", encoding="utf-8")

        original_mkdir = Path.mkdir

        def failing_mkdir(self_path, *args, **kwargs):
            if "zing-patches" in str(self_path):
                raise OSError("permission denied")
            return original_mkdir(self_path, *args, **kwargs)

        with mock.patch.object(Path, "mkdir", failing_mkdir):
            stderr = io.StringIO()
            with mock.patch("sys.stderr", stderr):
                result = backup_modified_files(self.target)
                self.assertEqual(result, [])
                self.assertIn("warning", stderr.getvalue())


class TestListPatches(unittest.TestCase):
    """Verify list_patches behaviour."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.target = Path(self.tmp)

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_returns_correct_tuples(self) -> None:
        """list_patches returns (original_relpath, backup_path) tuples."""
        patches_dir = self.target / "zing-patches"
        patches_dir.mkdir()
        # Create a fake backup file.
        backup = patches_dir / "zing.md.2026-02-13T120000"
        backup.write_text("# backed up", encoding="utf-8")

        result = list_patches(self.target)

        self.assertEqual(len(result), 1)
        relpath, path = result[0]
        self.assertEqual(relpath, "zing.md")
        self.assertEqual(path, backup)

    def test_returns_empty_when_no_patches(self) -> None:
        """list_patches returns empty list when zing-patches/ doesn't exist."""
        result = list_patches(self.target)
        self.assertEqual(result, [])

    def test_returns_empty_for_empty_patches_dir(self) -> None:
        """list_patches returns empty list when zing-patches/ exists but is empty."""
        (self.target / "zing-patches").mkdir()
        result = list_patches(self.target)
        self.assertEqual(result, [])

    def test_multiple_patches_sorted(self) -> None:
        """list_patches returns multiple patches sorted by filename."""
        patches_dir = self.target / "zing-patches"
        patches_dir.mkdir()
        (patches_dir / "b.md.2026-02-13T120000").write_text("b", encoding="utf-8")
        (patches_dir / "a.md.2026-02-13T120000").write_text("a", encoding="utf-8")

        result = list_patches(self.target)

        self.assertEqual(len(result), 2)
        self.assertEqual(result[0][0], "a.md")
        self.assertEqual(result[1][0], "b.md")


class TestReapplyPatches(unittest.TestCase):
    """Verify reapply_patches output."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.target = Path(self.tmp)

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_prints_patch_info(self) -> None:
        """reapply_patches prints original path and backup path."""
        patches_dir = self.target / "zing-patches"
        patches_dir.mkdir()
        backup = patches_dir / "zing.md.2026-02-13T120000"
        backup.write_text("# backed up", encoding="utf-8")

        stdout = io.StringIO()
        with mock.patch("sys.stdout", stdout):
            reapply_patches(self.target)

        output = stdout.getvalue()
        self.assertIn("Backed-up patches:", output)
        self.assertIn(str(self.target / "zing.md"), output)
        self.assertIn(str(backup), output)

    def test_prints_no_patches_message(self) -> None:
        """reapply_patches prints 'No backed-up patches found.' when empty."""
        stdout = io.StringIO()
        with mock.patch("sys.stdout", stdout):
            reapply_patches(self.target)

        self.assertIn("No backed-up patches found.", stdout.getvalue())


class TestReinstallFlow(unittest.TestCase):
    """End-to-end: install -> modify -> re-install -> verify backup."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.target = Path(self.tmp)

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_reinstall_backs_up_modified_file(self) -> None:
        """Full flow: install, modify a file, re-install, verify backup exists."""
        # First install.
        install_claude(target_dir=self.target)

        # Verify zing.md exists.
        zing_md = self.target / "zing.md"
        self.assertTrue(zing_md.exists())
        original_content = zing_md.read_text(encoding="utf-8")

        # Modify a file.
        zing_md.write_text("# User modified this file!", encoding="utf-8")

        # Re-install (this should back up the modified file).
        install_claude(target_dir=self.target)

        # Verify backup exists in zing-patches/.
        patches_dir = self.target / "zing-patches"
        self.assertTrue(patches_dir.is_dir())

        backups = list(patches_dir.rglob("zing.md.*"))
        self.assertEqual(len(backups), 1)

        # Backup contains the user-modified content.
        self.assertEqual(
            backups[0].read_text(encoding="utf-8"),
            "# User modified this file!",
        )

        # The installed file was overwritten with the original bundled content.
        self.assertEqual(
            zing_md.read_text(encoding="utf-8"),
            original_content,
        )
