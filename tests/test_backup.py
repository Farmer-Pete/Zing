"""Tests for zing_ai.backup — patch backup and restore."""

from __future__ import annotations

import io
from pathlib import Path
from unittest import mock

from zing_ai.backup import backup_modified_files, list_patches, reapply_patches
from zing_ai.installer import install_claude
from zing_ai.manifest import write_manifest

# ---------------------------------------------------------------------------
# backup_modified_files
# ---------------------------------------------------------------------------


def test_backs_up_modified_files(tmp_path: Path) -> None:
    (tmp_path / "zing.md").write_text("# Original", encoding="utf-8")
    write_manifest(
        tmp_path,
        "claude-code",
        ["zing.md"],
        config_hash="abc",
        source_mtime_max=0.0,
        package_version="0.0.0",
    )

    (tmp_path / "zing.md").write_text("# Modified!", encoding="utf-8")

    result = backup_modified_files(tmp_path)

    assert len(result) == 1
    relpath, backup_path = result[0]
    assert relpath == "zing.md"
    assert backup_path.exists()
    assert backup_path.is_file()
    assert backup_path.read_text(encoding="utf-8") == "# Modified!"
    assert str(backup_path).startswith(str(tmp_path / "zing-patches"))


def test_backup_returns_empty_on_fresh_install(tmp_path: Path) -> None:
    (tmp_path / "zing.md").write_text("# Zing", encoding="utf-8")

    assert backup_modified_files(tmp_path) == []


def test_backup_returns_empty_when_no_files_modified(tmp_path: Path) -> None:
    (tmp_path / "zing.md").write_text("# Zing", encoding="utf-8")
    write_manifest(
        tmp_path,
        "claude-code",
        ["zing.md"],
        config_hash="abc",
        source_mtime_max=0.0,
        package_version="0.0.0",
    )

    assert backup_modified_files(tmp_path) == []


def test_backup_skips_deleted_files(tmp_path: Path) -> None:
    (tmp_path / "zing.md").write_text("# Zing", encoding="utf-8")
    write_manifest(
        tmp_path,
        "claude-code",
        ["zing.md"],
        config_hash="abc",
        source_mtime_max=0.0,
        package_version="0.0.0",
    )
    (tmp_path / "zing.md").unlink()

    assert backup_modified_files(tmp_path) == []


def test_patches_dir_creation_failure_prints_warning(tmp_path: Path) -> None:
    (tmp_path / "zing.md").write_text("# Original", encoding="utf-8")
    write_manifest(
        tmp_path,
        "claude-code",
        ["zing.md"],
        config_hash="abc",
        source_mtime_max=0.0,
        package_version="0.0.0",
    )
    (tmp_path / "zing.md").write_text("# Modified!", encoding="utf-8")

    original_mkdir = Path.mkdir

    def failing_mkdir(self_path, *args, **kwargs):
        if "zing-patches" in str(self_path):
            raise OSError("permission denied")
        return original_mkdir(self_path, *args, **kwargs)

    with mock.patch.object(Path, "mkdir", failing_mkdir):
        stderr = io.StringIO()
        with mock.patch("sys.stderr", stderr):
            result = backup_modified_files(tmp_path)
            assert result == []
            assert "warning" in stderr.getvalue()


# ---------------------------------------------------------------------------
# list_patches
# ---------------------------------------------------------------------------


def test_list_patches_returns_correct_tuples(tmp_path: Path) -> None:
    patches_dir = tmp_path / "zing-patches"
    patches_dir.mkdir()
    backup = patches_dir / "zing.md.2026-02-13T120000"
    backup.write_text("# backed up", encoding="utf-8")

    result = list_patches(tmp_path)

    assert len(result) == 1
    relpath, path = result[0]
    assert relpath == "zing.md"
    assert path == backup


def test_list_patches_returns_empty_when_no_patches(tmp_path: Path) -> None:
    assert list_patches(tmp_path) == []


def test_list_patches_returns_empty_for_empty_patches_dir(tmp_path: Path) -> None:
    (tmp_path / "zing-patches").mkdir()
    assert list_patches(tmp_path) == []


def test_list_patches_multiple_sorted(tmp_path: Path) -> None:
    patches_dir = tmp_path / "zing-patches"
    patches_dir.mkdir()
    (patches_dir / "b.md.2026-02-13T120000").write_text("b", encoding="utf-8")
    (patches_dir / "a.md.2026-02-13T120000").write_text("a", encoding="utf-8")

    result = list_patches(tmp_path)

    assert len(result) == 2
    assert result[0][0] == "a.md"
    assert result[1][0] == "b.md"


# ---------------------------------------------------------------------------
# reapply_patches
# ---------------------------------------------------------------------------


def test_reapply_prints_patch_info(tmp_path: Path) -> None:
    patches_dir = tmp_path / "zing-patches"
    patches_dir.mkdir()
    backup = patches_dir / "zing.md.2026-02-13T120000"
    backup.write_text("# backed up", encoding="utf-8")

    stdout = io.StringIO()
    with mock.patch("sys.stdout", stdout):
        reapply_patches(tmp_path)

    output = stdout.getvalue()
    assert "Backed-up patches:" in output
    assert str(tmp_path / "zing.md") in output
    assert str(backup) in output


def test_reapply_prints_no_patches_message(tmp_path: Path) -> None:
    stdout = io.StringIO()
    with mock.patch("sys.stdout", stdout):
        reapply_patches(tmp_path)

    assert "No backed-up patches found." in stdout.getvalue()


# ---------------------------------------------------------------------------
# Full reinstall flow
# ---------------------------------------------------------------------------


def test_reinstall_backs_up_modified_file(tmp_path: Path) -> None:
    install_claude(target_dir=tmp_path)

    zing_md = tmp_path / "zing.md"
    assert zing_md.exists()
    original_content = zing_md.read_text(encoding="utf-8")

    zing_md.write_text("# User modified this file!", encoding="utf-8")

    install_claude(target_dir=tmp_path)

    patches_dir = tmp_path / "zing-patches"
    assert patches_dir.is_dir()

    backups = list(patches_dir.rglob("zing.md.*"))
    assert len(backups) == 1

    assert backups[0].read_text(encoding="utf-8") == "# User modified this file!"
    assert zing_md.read_text(encoding="utf-8") == original_content
