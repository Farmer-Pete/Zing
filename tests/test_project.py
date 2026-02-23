"""Tests for zing_ai.orchestrator.project."""

from __future__ import annotations

import os
from pathlib import Path

import click
import pytest

from zing_ai.orchestrator.project import (
    ensure_zing_dir,
    find_project_root,
    list_zing_files,
    resolve_zing_file,
)


# ---------------------------------------------------------------------------
# find_project_root
# ---------------------------------------------------------------------------

class TestFindProjectRoot:
    """Tests for find_project_root()."""

    def test_finds_root_when_git_exists(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        (tmp_path / ".git").mkdir()
        monkeypatch.chdir(tmp_path)
        assert find_project_root() == tmp_path

    def test_finds_root_from_subdirectory(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        (tmp_path / ".git").mkdir()
        sub = tmp_path / "a" / "b" / "c"
        sub.mkdir(parents=True)
        monkeypatch.chdir(sub)
        assert find_project_root() == tmp_path

    def test_raises_when_no_git(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.chdir(tmp_path)
        with pytest.raises(click.UsageError, match="Not inside a git repository"):
            find_project_root()


# ---------------------------------------------------------------------------
# ensure_zing_dir
# ---------------------------------------------------------------------------

class TestEnsureZingDir:
    """Tests for ensure_zing_dir()."""

    def test_creates_zing_dir(self, tmp_path: Path) -> None:
        result = ensure_zing_dir(tmp_path)
        assert result == tmp_path / ".zing"
        assert result.is_dir()

    def test_idempotent(self, tmp_path: Path) -> None:
        (tmp_path / ".zing").mkdir()
        result = ensure_zing_dir(tmp_path)
        assert result == tmp_path / ".zing"
        assert result.is_dir()


# ---------------------------------------------------------------------------
# list_zing_files
# ---------------------------------------------------------------------------

class TestListZingFiles:
    """Tests for list_zing_files()."""

    def test_returns_empty_when_no_zing_dir(self, tmp_path: Path) -> None:
        assert list_zing_files(tmp_path) == []

    def test_returns_empty_when_no_xml(self, tmp_path: Path) -> None:
        (tmp_path / ".zing").mkdir()
        (tmp_path / ".zing" / "notes.txt").touch()
        assert list_zing_files(tmp_path) == []

    def test_finds_xml_files(self, tmp_path: Path) -> None:
        zing = tmp_path / ".zing"
        zing.mkdir()
        (zing / "alpha.xml").touch()
        (zing / "beta.xml").touch()
        (zing / "readme.md").touch()
        result = list_zing_files(tmp_path)
        names = [p.name for p in result]
        assert names == ["alpha.xml", "beta.xml"]

    def test_returns_sorted(self, tmp_path: Path) -> None:
        zing = tmp_path / ".zing"
        zing.mkdir()
        (zing / "zebra.xml").touch()
        (zing / "alpha.xml").touch()
        result = list_zing_files(tmp_path)
        assert [p.name for p in result] == ["alpha.xml", "zebra.xml"]


# ---------------------------------------------------------------------------
# resolve_zing_file
# ---------------------------------------------------------------------------

class TestResolveZingFile:
    """Tests for resolve_zing_file()."""

    def test_resolves_existing_file(self, tmp_path: Path) -> None:
        zing = tmp_path / ".zing"
        zing.mkdir()
        target = zing / "plan.xml"
        target.touch()
        assert resolve_zing_file("plan.xml", tmp_path) == target

    def test_raises_for_missing_file(self, tmp_path: Path) -> None:
        zing = tmp_path / ".zing"
        zing.mkdir()
        with pytest.raises(click.UsageError, match="Zing file not found"):
            resolve_zing_file("nonexistent.xml", tmp_path)

    def test_raises_when_no_files_and_no_arg(self, tmp_path: Path) -> None:
        zing = tmp_path / ".zing"
        zing.mkdir()
        with pytest.raises(click.UsageError, match="No .xml files found"):
            resolve_zing_file(None, tmp_path)

    def test_auto_selects_single_file(self, tmp_path: Path) -> None:
        zing = tmp_path / ".zing"
        zing.mkdir()
        target = zing / "only.xml"
        target.touch()
        assert resolve_zing_file(None, tmp_path) == target

    def test_prompts_for_multiple_files(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        zing = tmp_path / ".zing"
        zing.mkdir()
        (zing / "alpha.xml").touch()
        (zing / "beta.xml").touch()
        # Simulate user entering "2" at the prompt
        monkeypatch.setattr("click.prompt", lambda *a, **kw: 2)
        result = resolve_zing_file(None, tmp_path)
        assert result == zing / "beta.xml"
