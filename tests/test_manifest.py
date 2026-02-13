"""Tests for the manifest system (hashing, write/read, modification detection)."""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import stat
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from zing_ai.manifest import (
    detect_modified_files,
    hash_file,
    hash_installed_files,
    read_manifest,
    write_manifest,
)


class TestHashFile(unittest.TestCase):
    """Verify SHA256 hashing of individual files."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_known_content(self) -> None:
        """hash_file returns the correct SHA256 for known content."""
        path = Path(self.tmp) / "hello.txt"
        content = b"hello world\n"
        path.write_bytes(content)

        expected = hashlib.sha256(content).hexdigest()
        self.assertEqual(hash_file(path), expected)

    def test_empty_file(self) -> None:
        """hash_file works for an empty file."""
        path = Path(self.tmp) / "empty.txt"
        path.write_bytes(b"")

        expected = hashlib.sha256(b"").hexdigest()
        self.assertEqual(hash_file(path), expected)


class TestHashInstalledFiles(unittest.TestCase):
    """Verify hashing of multiple installed files."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.target = Path(self.tmp)

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_returns_correct_mapping(self) -> None:
        """hash_installed_files returns a dict mapping relpaths to sha256."""
        (self.target / "a.md").write_text("alpha", encoding="utf-8")
        (self.target / "b.md").write_text("bravo", encoding="utf-8")

        result = hash_installed_files(self.target, ["a.md", "b.md"])

        self.assertIn("a.md", result)
        self.assertIn("b.md", result)
        self.assertEqual(
            result["a.md"]["sha256"],
            hashlib.sha256(b"alpha").hexdigest(),
        )
        self.assertEqual(
            result["b.md"]["sha256"],
            hashlib.sha256(b"bravo").hexdigest(),
        )

    def test_skips_missing_files(self) -> None:
        """hash_installed_files skips files that don't exist on disk."""
        (self.target / "a.md").write_text("alpha", encoding="utf-8")

        result = hash_installed_files(self.target, ["a.md", "nonexistent.md"])

        self.assertIn("a.md", result)
        self.assertNotIn("nonexistent.md", result)


class TestWriteManifest(unittest.TestCase):
    """Verify manifest writing."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.target = Path(self.tmp)

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_creates_valid_json(self) -> None:
        """write_manifest creates a valid JSON file with correct schema."""
        (self.target / "zing.md").write_text("# Zing", encoding="utf-8")

        write_manifest(self.target, "claude-code", ["zing.md"])

        manifest_path = self.target / "zing-manifest.json"
        self.assertTrue(manifest_path.exists())

        data = json.loads(manifest_path.read_text(encoding="utf-8"))
        self.assertIn("version", data)
        self.assertIn("installed_at", data)
        self.assertIn("runtime", data)
        self.assertIn("files", data)
        self.assertEqual(data["runtime"], "claude-code")
        self.assertIn("zing.md", data["files"])
        self.assertIn("sha256", data["files"]["zing.md"])

    def test_version_matches_package(self) -> None:
        """The manifest version matches zing_ai.__version__."""
        from zing_ai import __version__

        (self.target / "a.md").write_text("content", encoding="utf-8")
        write_manifest(self.target, "opencode", ["a.md"])

        data = json.loads(
            (self.target / "zing-manifest.json").read_text(encoding="utf-8"),
        )
        self.assertEqual(data["version"], __version__)

    def test_installed_at_is_iso_timestamp(self) -> None:
        """The installed_at field is a valid ISO 8601 timestamp."""
        from datetime import datetime

        (self.target / "a.md").write_text("content", encoding="utf-8")
        write_manifest(self.target, "claude-code", ["a.md"])

        data = json.loads(
            (self.target / "zing-manifest.json").read_text(encoding="utf-8"),
        )
        # Should not raise if it's a valid ISO timestamp.
        dt = datetime.fromisoformat(data["installed_at"])
        self.assertIsNotNone(dt)

    def test_permission_error_prints_warning(self) -> None:
        """write_manifest prints a warning but does NOT raise on failure."""
        (self.target / "a.md").write_text("content", encoding="utf-8")

        # Make the directory read-only so writing the manifest fails.
        self.target.chmod(stat.S_IRUSR | stat.S_IXUSR)

        try:
            # This should NOT raise.
            with patch("sys.stderr") as mock_stderr:
                write_manifest(self.target, "claude-code", ["a.md"])
                # Verify a warning was printed.
                mock_stderr.write.assert_called()
                written = "".join(
                    call.args[0]
                    for call in mock_stderr.write.call_args_list
                    if call.args
                )
                self.assertIn("warning", written)
        finally:
            # Restore permissions for cleanup.
            self.target.chmod(stat.S_IRWXU)


class TestReadManifest(unittest.TestCase):
    """Verify manifest reading."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.target = Path(self.tmp)

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_returns_parsed_dict(self) -> None:
        """read_manifest returns the parsed manifest dict."""
        manifest = {
            "version": "0.1.0",
            "installed_at": "2026-02-13T12:00:00+00:00",
            "runtime": "claude-code",
            "files": {
                "zing.md": {"sha256": "abc123"},
            },
        }
        (self.target / "zing-manifest.json").write_text(
            json.dumps(manifest),
            encoding="utf-8",
        )

        result = read_manifest(self.target)
        self.assertEqual(result, manifest)

    def test_returns_none_for_missing_file(self) -> None:
        """read_manifest returns None when the manifest doesn't exist."""
        result = read_manifest(self.target)
        self.assertIsNone(result)

    def test_returns_none_for_corrupt_json(self) -> None:
        """read_manifest returns None for invalid JSON."""
        (self.target / "zing-manifest.json").write_text(
            "{{not valid json",
            encoding="utf-8",
        )

        result = read_manifest(self.target)
        self.assertIsNone(result)


class TestDetectModifiedFiles(unittest.TestCase):
    """Verify modification detection."""

    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp()
        self.target = Path(self.tmp)

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_no_changes_returns_empty(self) -> None:
        """detect_modified_files returns empty list when nothing changed."""
        # Install a file and write a manifest.
        (self.target / "zing.md").write_text("# Zing", encoding="utf-8")
        write_manifest(self.target, "claude-code", ["zing.md"])

        modified = detect_modified_files(self.target)
        self.assertEqual(modified, [])

    def test_detects_modified_file(self) -> None:
        """detect_modified_files returns paths of changed files."""
        (self.target / "zing.md").write_text("# Zing", encoding="utf-8")
        (self.target / "other.md").write_text("# Other", encoding="utf-8")
        write_manifest(self.target, "claude-code", ["zing.md", "other.md"])

        # Modify one file.
        (self.target / "zing.md").write_text("# Modified!", encoding="utf-8")

        modified = detect_modified_files(self.target)
        self.assertEqual(modified, ["zing.md"])

    def test_detects_deleted_file(self) -> None:
        """detect_modified_files treats deleted files as modified."""
        (self.target / "zing.md").write_text("# Zing", encoding="utf-8")
        write_manifest(self.target, "claude-code", ["zing.md"])

        # Delete the file.
        (self.target / "zing.md").unlink()

        modified = detect_modified_files(self.target)
        self.assertEqual(modified, ["zing.md"])

    def test_no_manifest_returns_empty(self) -> None:
        """detect_modified_files returns empty list when no manifest exists."""
        (self.target / "zing.md").write_text("# Zing", encoding="utf-8")

        modified = detect_modified_files(self.target)
        self.assertEqual(modified, [])

    def test_multiple_modified_files(self) -> None:
        """detect_modified_files returns all modified file paths."""
        (self.target / "a.md").write_text("alpha", encoding="utf-8")
        (self.target / "b.md").write_text("bravo", encoding="utf-8")
        (self.target / "c.md").write_text("charlie", encoding="utf-8")
        write_manifest(
            self.target, "claude-code", ["a.md", "b.md", "c.md"],
        )

        # Modify two of three.
        (self.target / "a.md").write_text("ALPHA", encoding="utf-8")
        (self.target / "c.md").write_text("CHARLIE", encoding="utf-8")

        modified = detect_modified_files(self.target)
        self.assertCountEqual(modified, ["a.md", "c.md"])


if __name__ == "__main__":
    unittest.main()
