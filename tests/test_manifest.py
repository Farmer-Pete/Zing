"""Tests for the manifest system (hashing, write/read, modification detection)."""

from __future__ import annotations

import hashlib
import json
import stat
from datetime import datetime
from pathlib import Path
from unittest.mock import patch

from zing_ai.manifest import (
    detect_modified_files,
    hash_file,
    hash_installed_files,
    load_manifest,
    read_manifest,
    write_manifest,
)

# ---------------------------------------------------------------------------
# hash_file
# ---------------------------------------------------------------------------


def test_hash_known_content(tmp_path: Path) -> None:
    path = tmp_path / "hello.txt"
    content = b"hello world\n"
    path.write_bytes(content)

    expected = hashlib.sha256(content).hexdigest()
    assert hash_file(path) == expected


def test_hash_empty_file(tmp_path: Path) -> None:
    path = tmp_path / "empty.txt"
    path.write_bytes(b"")

    expected = hashlib.sha256(b"").hexdigest()
    assert hash_file(path) == expected


# ---------------------------------------------------------------------------
# hash_installed_files
# ---------------------------------------------------------------------------


def test_hash_installed_returns_correct_mapping(tmp_path: Path) -> None:
    (tmp_path / "a.md").write_text("alpha", encoding="utf-8")
    (tmp_path / "b.md").write_text("bravo", encoding="utf-8")

    result = hash_installed_files(tmp_path, ["a.md", "b.md"])

    assert "a.md" in result
    assert "b.md" in result
    assert result["a.md"]["sha256"] == hashlib.sha256(b"alpha").hexdigest()
    assert result["b.md"]["sha256"] == hashlib.sha256(b"bravo").hexdigest()


def test_hash_installed_skips_missing_files(tmp_path: Path) -> None:
    (tmp_path / "a.md").write_text("alpha", encoding="utf-8")

    result = hash_installed_files(tmp_path, ["a.md", "nonexistent.md"])

    assert "a.md" in result
    assert "nonexistent.md" not in result


# ---------------------------------------------------------------------------
# write_manifest
# ---------------------------------------------------------------------------


def test_write_manifest_creates_valid_json(tmp_path: Path) -> None:
    (tmp_path / "zing.md").write_text("# Zing", encoding="utf-8")

    write_manifest(
        tmp_path,
        "claude-code",
        ["zing.md"],
        config_hash="abc",
        source_mtime_max=1234.5,
        package_version="0.1.0",
    )

    manifest_path = tmp_path / "zing-manifest.json"
    assert manifest_path.exists()

    data = json.loads(manifest_path.read_text(encoding="utf-8"))
    assert "version" in data
    assert "installed_at" in data
    assert "runtime" in data
    assert "files" in data
    assert data["runtime"] == "claude-code"
    assert "zing.md" in data["files"]
    assert "sha256" in data["files"]["zing.md"]


def test_write_manifest_version_matches_package(tmp_path: Path) -> None:
    from zing_ai import __version__

    (tmp_path / "a.md").write_text("content", encoding="utf-8")
    write_manifest(
        tmp_path,
        "opencode",
        ["a.md"],
        config_hash="abc",
        source_mtime_max=1234.5,
        package_version="0.1.0",
    )

    data = json.loads(
        (tmp_path / "zing-manifest.json").read_text(encoding="utf-8"),
    )
    assert data["version"] == __version__


def test_write_manifest_installed_at_is_iso_timestamp(tmp_path: Path) -> None:
    (tmp_path / "a.md").write_text("content", encoding="utf-8")
    write_manifest(
        tmp_path,
        "claude-code",
        ["a.md"],
        config_hash="abc",
        source_mtime_max=1234.5,
        package_version="0.1.0",
    )

    data = json.loads(
        (tmp_path / "zing-manifest.json").read_text(encoding="utf-8"),
    )
    dt = datetime.fromisoformat(data["installed_at"])
    assert dt is not None


def test_write_manifest_permission_error_prints_warning(tmp_path: Path) -> None:
    (tmp_path / "a.md").write_text("content", encoding="utf-8")

    tmp_path.chmod(stat.S_IRUSR | stat.S_IXUSR)

    try:
        with patch("sys.stderr") as mock_stderr:
            write_manifest(
                tmp_path,
                "claude-code",
                ["a.md"],
                config_hash="abc",
                source_mtime_max=1234.5,
                package_version="0.1.0",
            )
            mock_stderr.write.assert_called()
            written = "".join(
                call.args[0] for call in mock_stderr.write.call_args_list if call.args
            )
            assert "warning" in written
    finally:
        tmp_path.chmod(stat.S_IRWXU)


# ---------------------------------------------------------------------------
# read_manifest
# ---------------------------------------------------------------------------


def test_read_manifest_returns_parsed_dict(tmp_path: Path) -> None:
    manifest = {
        "version": "0.1.0",
        "installed_at": "2026-02-13T12:00:00+00:00",
        "runtime": "claude-code",
        "files": {
            "zing.md": {"sha256": "abc123"},
        },
    }
    (tmp_path / "zing-manifest.json").write_text(
        json.dumps(manifest),
        encoding="utf-8",
    )

    result = read_manifest(tmp_path)
    assert result == manifest


def test_read_manifest_returns_none_for_missing_file(tmp_path: Path) -> None:
    assert read_manifest(tmp_path) is None


def test_read_manifest_returns_none_for_corrupt_json(tmp_path: Path) -> None:
    (tmp_path / "zing-manifest.json").write_text(
        "{{not valid json",
        encoding="utf-8",
    )
    assert read_manifest(tmp_path) is None


# ---------------------------------------------------------------------------
# detect_modified_files
# ---------------------------------------------------------------------------


def test_detect_no_changes_returns_empty(tmp_path: Path) -> None:
    (tmp_path / "zing.md").write_text("# Zing", encoding="utf-8")
    write_manifest(
        tmp_path,
        "claude-code",
        ["zing.md"],
        config_hash="abc",
        source_mtime_max=1234.5,
        package_version="0.1.0",
    )

    assert detect_modified_files(tmp_path) == []


def test_detect_modified_file(tmp_path: Path) -> None:
    (tmp_path / "zing.md").write_text("# Zing", encoding="utf-8")
    (tmp_path / "other.md").write_text("# Other", encoding="utf-8")
    write_manifest(
        tmp_path,
        "claude-code",
        ["zing.md", "other.md"],
        config_hash="abc",
        source_mtime_max=1234.5,
        package_version="0.1.0",
    )

    (tmp_path / "zing.md").write_text("# Modified!", encoding="utf-8")

    assert detect_modified_files(tmp_path) == ["zing.md"]


def test_detect_deleted_file(tmp_path: Path) -> None:
    (tmp_path / "zing.md").write_text("# Zing", encoding="utf-8")
    write_manifest(
        tmp_path,
        "claude-code",
        ["zing.md"],
        config_hash="abc",
        source_mtime_max=1234.5,
        package_version="0.1.0",
    )

    (tmp_path / "zing.md").unlink()

    assert detect_modified_files(tmp_path) == ["zing.md"]


def test_detect_no_manifest_returns_empty(tmp_path: Path) -> None:
    (tmp_path / "zing.md").write_text("# Zing", encoding="utf-8")

    assert detect_modified_files(tmp_path) == []


def test_detect_multiple_modified_files(tmp_path: Path) -> None:
    (tmp_path / "a.md").write_text("alpha", encoding="utf-8")
    (tmp_path / "b.md").write_text("bravo", encoding="utf-8")
    (tmp_path / "c.md").write_text("charlie", encoding="utf-8")
    write_manifest(
        tmp_path,
        "claude-code",
        ["a.md", "b.md", "c.md"],
        config_hash="abc",
        source_mtime_max=1234.5,
        package_version="0.1.0",
    )

    (tmp_path / "a.md").write_text("ALPHA", encoding="utf-8")
    (tmp_path / "c.md").write_text("CHARLIE", encoding="utf-8")

    modified = detect_modified_files(tmp_path)
    assert sorted(modified) == ["a.md", "c.md"]


# ---------------------------------------------------------------------------
# write_manifest — new fields
# ---------------------------------------------------------------------------


def test_write_manifest_includes_new_fields(tmp_path: Path) -> None:
    (tmp_path / "zing.md").write_text("# Zing", encoding="utf-8")

    write_manifest(
        tmp_path,
        "claude-code",
        ["zing.md"],
        config_hash="abc",
        source_mtime_max=1234.5,
        package_version="0.1.0",
    )

    data = json.loads((tmp_path / "zing-manifest.json").read_text(encoding="utf-8"))
    assert data["config_hash"] == "abc"
    assert data["source_mtime_max"] == 1234.5
    assert data["package_version"] == "0.1.0"


# ---------------------------------------------------------------------------
# load_manifest
# ---------------------------------------------------------------------------


def test_load_manifest_round_trip(tmp_path: Path) -> None:
    (tmp_path / "zing.md").write_text("# Zing", encoding="utf-8")

    write_manifest(
        tmp_path,
        "claude-code",
        ["zing.md"],
        config_hash="abc",
        source_mtime_max=1234.5,
        package_version="0.1.0",
    )

    result = load_manifest(tmp_path)
    assert result is not None
    assert result["config_hash"] == "abc"
    assert result["source_mtime_max"] == 1234.5
    assert result["package_version"] == "0.1.0"
    assert result["runtime"] == "claude-code"
    assert "zing.md" in result["files"]


def test_load_manifest_returns_none_when_missing(tmp_path: Path) -> None:
    assert load_manifest(tmp_path) is None
